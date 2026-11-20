package adapters

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/arhammahajan/model-router/internal/harness/convstore"
)

type geminiAdapter struct{}

// NewGeminiAdapter returns the convstore adapter for Gemini CLI
// (~/.gemini/tmp/*/chats/session-*.jsonl and *.json).
func NewGeminiAdapter() convstore.Adapter {
	return &geminiAdapter{}
}

func (a *geminiAdapter) Agent() string { return "gemini" }

func (a *geminiAdapter) Discover(ctx context.Context, env *convstore.ScanEnv) ([]convstore.DiscoveredSource, error) {
	if env == nil {
		return nil, nil
	}
	type rootSpec struct {
		dir     string
		profile string
	}
	var roots []rootSpec
	if env.Paths.Home != "" {
		roots = append(roots, rootSpec{
			dir: filepath.Join(env.Paths.Home, ".gemini", "tmp"),
		})
	}
	for _, ph := range env.ProfileShadowHomes("gemini") {
		roots = append(roots,
			rootSpec{dir: filepath.Join(ph.ShadowHome, ".gemini", "tmp"), profile: ph.Profile},
			rootSpec{dir: filepath.Join(ph.ShadowHome, "tmp"), profile: ph.Profile},
		)
	}

	var out []convstore.DiscoveredSource
	patterns := []string{
		filepath.Join("*", "chats", "session-*.jsonl"),
		filepath.Join("*", "chats", "*.json"),
	}
	for _, r := range roots {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if fi, err := os.Stat(r.dir); err != nil || !fi.IsDir() {
			continue
		}
		for _, pat := range patterns {
			matches, _ := filepath.Glob(filepath.Join(r.dir, pat))
			for _, path := range matches {
				if err := ctx.Err(); err != nil {
					return out, err
				}
				if !env.MarkRealPath(path) {
					continue
				}
				size, mtime, ok := convstore.StatFile(path)
				if !ok {
					continue
				}
				out = append(out, convstore.DiscoveredSource{
					SourceKey: convstore.MakeSourceKey("gemini", r.profile, path),
					Agent:     "gemini",
					Profile:   r.profile,
					Path:      path,
					Kind:      convstore.SourceFile,
					SizeBytes: size,
					MtimeNs:   mtime,
				})
			}
		}
	}
	return out, nil
}

func (a *geminiAdapter) ParseMetadata(ctx context.Context, src convstore.DiscoveredSource, _ *convstore.SourceCursor) (*convstore.MetadataResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.HasSuffix(src.Path, ".json") {
		return geminiParseJSONMetadata(src)
	}
	return geminiParseJSONLMetadata(src)
}

func (a *geminiAdapter) ParseMessages(ctx context.Context, src convstore.DiscoveredSource, prev *convstore.SourceCursor) (*convstore.MessagesResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.HasSuffix(src.Path, ".json") {
		return geminiParseJSONMessages(src)
	}

	convID := geminiResolveSessionID(src.Path)
	var prevOffset int64
	var prevHash string
	if prev != nil {
		prevOffset = prev.MsgByteOffset
		prevHash = prev.MsgPrefixHash
	}

	var msgs []convstore.Message
	newOffset, newHash, appended, err := convstore.ReadIncrementalJSONL(src.Path, prevOffset, prevHash, 0, func(line []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil || rec == nil {
			return nil
		}
		if convID == "" {
			if v, _ := rec["sessionId"].(string); v != "" {
				convID = v
			}
		}
		role := geminiNormalizeRole(rec)
		if role == "" {
			return nil
		}
		raw := geminiExtractContent(rec)
		cleaned, truncated := convstore.CleanMessageText(raw)
		if cleaned == "" {
			return nil
		}
		msgs = append(msgs, convstore.Message{
			Seq:       len(msgs),
			Role:      role,
			Text:      cleaned,
			TsMs:      geminiExtractTimestamp(rec),
			Truncated: truncated,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if convID == "" {
		base := filepath.Base(src.Path)
		convID = strings.TrimSuffix(base, filepath.Ext(base))
	}
	out := make(map[string][]convstore.Message)
	if len(msgs) > 0 {
		out[convID] = msgs
	}
	return &convstore.MessagesResult{
		Messages:   out,
		Append:     appended,
		ByteOffset: newOffset,
		PrefixHash: newHash,
	}, nil
}

func geminiParseJSONLMetadata(src convstore.DiscoveredSource) (*convstore.MetadataResult, error) {
	head, tail, err := convstore.ReadHeadTailLines(src.Path, 64<<10, 64<<10, 120, 80)
	if err != nil {
		return nil, err
	}
	if len(head) == 0 && len(tail) == 0 {
		return &convstore.MetadataResult{}, nil
	}

	var (
		id            string
		projectHash   string
		cwd           string
		model         string
		explicitTitle string
		firstPrompt   string
		createdMs     int64
		updatedMs     int64
		msgCount      int
		sawAnyJSON    bool
	)

	processLine := func(line string) {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil || rec == nil {
			return
		}
		sawAnyJSON = true

		// Gemini CLI writes generated session titles and timestamps into `{"$set": {...}}` lines.
		if setMap, ok := rec["$set"].(map[string]any); ok && setMap != nil {
			for _, k := range []string{"title", "summary", "name"} {
				if v, _ := setMap[k].(string); v != "" {
					if ct := convstore.CleanTitle(v); ct != "" {
						explicitTitle = ct
					}
				}
			}
			for _, k := range []string{"lastUpdated", "updatedAt", "timestamp"} {
				if ts := convstore.ParseTimestampMs(setMap[k]); ts > 0 {
					if ts > updatedMs {
						updatedMs = ts
					}
				}
			}
		}

		if id == "" {
			for _, k := range []string{"sessionId", "session_id", "id"} {
				if v, _ := rec[k].(string); v != "" {
					id = v
					break
				}
			}
		}
		if projectHash == "" {
			if v, _ := rec["projectHash"].(string); v != "" {
				projectHash = v
			}
		}
		if cwd == "" {
			if v, _ := rec["cwd"].(string); v != "" {
				cwd = v
			}
		}
		if v, _ := rec["model"].(string); v != "" {
			model = v
		}
		for _, k := range []string{"title", "summary"} {
			if v, _ := rec[k].(string); v != "" {
				if ct := convstore.CleanTitle(v); ct != "" {
					explicitTitle = ct
				}
			}
		}
		for _, k := range []string{"startTime", "timestamp", "lastUpdated", "updatedAt", "createdAt"} {
			if ts := convstore.ParseTimestampMs(rec[k]); ts > 0 {
				if createdMs == 0 || ts < createdMs {
					createdMs = ts
				}
				if ts > updatedMs {
					updatedMs = ts
				}
			}
		}
		role := geminiNormalizeRole(rec)
		if role == "user" || role == "assistant" {
			raw := geminiExtractContent(rec)
			if cleaned, _ := convstore.CleanMessageText(raw); cleaned != "" {
				msgCount++
				if role == "user" && firstPrompt == "" {
					if ct := convstore.CleanTitle(raw); ct != "" {
						firstPrompt = ct
					}
				}
			}
		}
	}

	for _, line := range head {
		processLine(line)
	}
	for _, line := range tail {
		processLine(line)
	}

	if !sawAnyJSON {
		return &convstore.MetadataResult{}, nil
	}
	if id == "" {
		base := filepath.Base(src.Path)
		id = strings.TrimSuffix(base, filepath.Ext(base))
	}
	if cwd == "" {
		cwd = geminiResolveCWD(src.Path, projectHash)
	}
	if updatedMs == 0 {
		updatedMs = src.MtimeNs / 1e6
	}
	title := explicitTitle
	if title == "" {
		title = firstPrompt
	}
	preview := convstore.CleanPreview(firstPrompt)
	if preview == "" {
		preview = convstore.CleanPreview(title)
	}
	if title == "" && preview == "" && msgCount == 0 {
		return &convstore.MetadataResult{}, nil
	}

	return &convstore.MetadataResult{
		Conversations: []convstore.Conversation{
			{
				Agent:        "gemini",
				ID:           id,
				App:          convstore.DisplayApp("gemini"),
				Profile:      src.Profile,
				Title:        title,
				Preview:      preview,
				CWD:          cwd,
				Model:        model,
				CreatedAtMs:  createdMs,
				UpdatedAtMs:  updatedMs,
				MessageCount: msgCount,
				SourceKey:    src.SourceKey,
				SourcePath:   src.Path,
				ResumeTarget: src.Path,
			},
		},
	}, nil
}

func geminiParseJSONMetadata(src convstore.DiscoveredSource) (*convstore.MetadataResult, error) {
	raw, err := os.ReadFile(src.Path)
	if err != nil {
		return nil, err
	}
	var rec map[string]any
	if err := json.Unmarshal(raw, &rec); err != nil || rec == nil {
		return &convstore.MetadataResult{}, nil
	}
	id, _ := rec["sessionId"].(string)
	if id == "" {
		id, _ = rec["id"].(string)
	}
	if id == "" {
		base := filepath.Base(src.Path)
		id = strings.TrimSuffix(base, filepath.Ext(base))
	}
	projectHash, _ := rec["projectHash"].(string)
	cwd, _ := rec["cwd"].(string)
	if cwd == "" {
		cwd = geminiResolveCWD(src.Path, projectHash)
	}
	model, _ := rec["model"].(string)
	explicitTitle := convstore.CleanTitle(geminiAsString(rec["title"]))
	createdMs := convstore.ParseTimestampMs(rec["startTime"])
	updatedMs := convstore.ParseTimestampMs(rec["lastUpdated"])
	if ts := convstore.ParseTimestampMs(rec["timestamp"]); ts > 0 {
		if createdMs == 0 || ts < createdMs {
			createdMs = ts
		}
		if ts > updatedMs {
			updatedMs = ts
		}
	}

	var firstPrompt string
	var msgCount int
	if items, ok := rec["messages"].([]any); ok {
		for _, item := range items {
			m, _ := item.(map[string]any)
			if m == nil {
				continue
			}
			if ts := geminiExtractTimestamp(m); ts > 0 {
				if createdMs == 0 || ts < createdMs {
					createdMs = ts
				}
				if ts > updatedMs {
					updatedMs = ts
				}
			}
			if v, _ := m["model"].(string); v != "" {
				model = v
			}
			role := geminiNormalizeRole(m)
			if role == "user" || role == "assistant" {
				msgCount++
			}
			if role == "user" && firstPrompt == "" {
				if ct := convstore.CleanTitle(geminiExtractContent(m)); ct != "" {
					firstPrompt = ct
				}
			}
		}
	}

	if updatedMs == 0 {
		updatedMs = src.MtimeNs / 1e6
	}
	title := explicitTitle
	if title == "" {
		title = firstPrompt
	}
	preview := convstore.CleanPreview(firstPrompt)
	if preview == "" {
		preview = convstore.CleanPreview(title)
	}

	return &convstore.MetadataResult{
		Conversations: []convstore.Conversation{
			{
				Agent:        "gemini",
				ID:           id,
				App:          convstore.DisplayApp("gemini"),
				Profile:      src.Profile,
				Title:        title,
				Preview:      preview,
				CWD:          cwd,
				Model:        model,
				CreatedAtMs:  createdMs,
				UpdatedAtMs:  updatedMs,
				MessageCount: msgCount,
				SourceKey:    src.SourceKey,
				SourcePath:   src.Path,
				ResumeTarget: src.Path,
			},
		},
	}, nil
}

func geminiParseJSONMessages(src convstore.DiscoveredSource) (*convstore.MessagesResult, error) {
	raw, err := os.ReadFile(src.Path)
	if err != nil {
		return nil, err
	}
	var rec map[string]any
	if err := json.Unmarshal(raw, &rec); err != nil || rec == nil {
		return nil, nil
	}
	convID, _ := rec["sessionId"].(string)
	if convID == "" {
		convID, _ = rec["id"].(string)
	}
	if convID == "" {
		base := filepath.Base(src.Path)
		convID = strings.TrimSuffix(base, filepath.Ext(base))
	}
	items, _ := rec["messages"].([]any)
	var msgs []convstore.Message
	for _, item := range items {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		role := geminiNormalizeRole(m)
		if role == "" {
			continue
		}
		cleaned, truncated := convstore.CleanMessageText(geminiExtractContent(m))
		if cleaned == "" {
			continue
		}
		msgs = append(msgs, convstore.Message{
			Seq:       len(msgs),
			Role:      role,
			Text:      cleaned,
			TsMs:      geminiExtractTimestamp(m),
			Truncated: truncated,
		})
	}
	out := make(map[string][]convstore.Message)
	if len(msgs) > 0 {
		out[convID] = msgs
	}
	return &convstore.MessagesResult{
		Messages: out,
		Append:   false,
	}, nil
}

func geminiResolveSessionID(path string) string {
	head, _, err := convstore.ReadHeadTailLines(path, 8<<10, 0, 5, 0)
	if err == nil {
		for _, line := range head {
			var rec map[string]any
			if json.Unmarshal([]byte(line), &rec) != nil || rec == nil {
				continue
			}
			for _, k := range []string{"sessionId", "session_id", "id"} {
				if v, _ := rec[k].(string); v != "" {
					return v
				}
			}
		}
	}
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

func geminiResolveCWD(sessionPath, projectHash string) string {
	// Layout: <geminiRoot>/tmp/<projDir>/chats/session-*.jsonl
	chatsDir := filepath.Dir(sessionPath)
	projDir := filepath.Dir(chatsDir)
	projName := filepath.Base(projDir)

	if data, err := os.ReadFile(filepath.Join(projDir, ".project_root")); err == nil {
		if root := strings.TrimSpace(string(data)); root != "" {
			return root
		}
	}

	tmpDir := filepath.Dir(projDir)
	geminiRoot := filepath.Dir(tmpDir)
	if cwd := geminiLookupProjectsJSON(filepath.Join(geminiRoot, "projects.json"), projectHash, projName); cwd != "" {
		return cwd
	}
	return ""
}

func geminiLookupProjectsJSON(path, projectHash, projName string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var top any
	if err := json.Unmarshal(raw, &top); err != nil {
		return ""
	}

	matchMap := func(m map[string]any) string {
		// Direct hash or dir name -> path mapping.
		for _, key := range []string{projectHash, projName} {
			if key == "" {
				continue
			}
			if val, ok := m[key].(string); ok && filepath.IsAbs(val) {
				return val
			}
			if obj, ok := m[key].(map[string]any); ok {
				for _, pk := range []string{"path", "root", "cwd", "project_root"} {
					if val, _ := obj[pk].(string); val != "" {
						return val
					}
				}
			}
		}
		// Reverse mapping: path -> name or hash (Gemini CLI projects.json format).
		for k, v := range m {
			if !filepath.IsAbs(k) {
				continue
			}
			if projectHash != "" {
				sum := sha256.Sum256([]byte(k))
				if hex.EncodeToString(sum[:]) == projectHash {
					return k
				}
			}
			if s, ok := v.(string); ok && s != "" && (s == projName || s == projectHash) {
				return k
			}
		}
		return ""
	}

	switch t := top.(type) {
	case map[string]any:
		if sub, ok := t["projects"].(map[string]any); ok {
			if found := matchMap(sub); found != "" {
				return found
			}
		}
		if arr, ok := t["projects"].([]any); ok {
			if found := geminiMatchProjectsSlice(arr, projectHash, projName); found != "" {
				return found
			}
		}
		return matchMap(t)
	case []any:
		return geminiMatchProjectsSlice(t, projectHash, projName)
	}
	return ""
}

func geminiMatchProjectsSlice(items []any, projectHash, projName string) string {
	for _, item := range items {
		obj, _ := item.(map[string]any)
		if obj == nil {
			continue
		}
		var p string
		for _, pk := range []string{"path", "root", "cwd", "project_root"} {
			if v, _ := obj[pk].(string); v != "" {
				p = v
				break
			}
		}
		if p == "" {
			continue
		}
		if projectHash != "" {
			if h, _ := obj["hash"].(string); h == projectHash {
				return p
			}
			if h, _ := obj["projectHash"].(string); h == projectHash {
				return p
			}
			sum := sha256.Sum256([]byte(p))
			if hex.EncodeToString(sum[:]) == projectHash {
				return p
			}
		}
		if projName != "" {
			if n, _ := obj["name"].(string); n == projName {
				return p
			}
		}
	}
	return ""
}

func geminiNormalizeRole(rec map[string]any) string {
	for _, k := range []string{"role", "type"} {
		if s, _ := rec[k].(string); s != "" {
			switch strings.ToLower(strings.TrimSpace(s)) {
			case "user":
				return "user"
			case "model", "assistant", "gemini":
				return "assistant"
			}
		}
	}
	return ""
}

func geminiExtractContent(rec map[string]any) string {
	if raw, ok := rec["content"]; ok {
		if s := convstore.ExtractPlainText(raw); s != "" {
			return s
		}
	}
	if raw, ok := rec["parts"]; ok {
		if s := convstore.ExtractPlainText(raw); s != "" {
			return s
		}
	}
	if raw, ok := rec["text"]; ok {
		if s := convstore.ExtractPlainText(raw); s != "" {
			return s
		}
	}
	return ""
}

func geminiExtractTimestamp(rec map[string]any) int64 {
	for _, k := range []string{"timestamp", "createdAt", "ts", "startTime"} {
		if ms := convstore.ParseTimestampMs(rec[k]); ms > 0 {
			return ms
		}
	}
	return 0
}

func geminiAsString(v any) string {
	s, _ := v.(string)
	return s
}
