package adapters

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/arhammahajan/model-router/internal/harness/convstore"
)

type deepseekAdapter struct{}

// NewDeepseekAdapter returns the convstore adapter for DeepSeek CLI (~/.dsh/sessions/*/*).
func NewDeepseekAdapter() convstore.Adapter {
	return &deepseekAdapter{}
}

func (a *deepseekAdapter) Agent() string { return "deepseek" }

func (a *deepseekAdapter) Discover(ctx context.Context, env *convstore.ScanEnv) ([]convstore.DiscoveredSource, error) {
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
			dir: filepath.Join(env.Paths.Home, ".dsh", "sessions"),
		})
	}
	for _, ph := range env.ProfileShadowHomes("deepseek") {
		roots = append(roots,
			rootSpec{dir: filepath.Join(ph.ShadowHome, "sessions"), profile: ph.Profile},
			rootSpec{dir: filepath.Join(ph.ShadowHome, ".dsh", "sessions"), profile: ph.Profile},
		)
	}

	var out []convstore.DiscoveredSource
	for _, r := range roots {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if fi, err := os.Stat(r.dir); err != nil || !fi.IsDir() {
			continue
		}
		matches, _ := filepath.Glob(filepath.Join(r.dir, "*", "*"))
		for _, p := range matches {
			if err := ctx.Err(); err != nil {
				return out, err
			}
			fi, err := os.Stat(p)
			if err != nil {
				continue
			}
			if fi.IsDir() {
				if !env.MarkRealPath(p) {
					continue
				}
				size, mtime := deepseekStatDir(p, fi)
				out = append(out, convstore.DiscoveredSource{
					SourceKey: convstore.MakeSourceKey("deepseek", r.profile, p),
					Agent:     "deepseek",
					Profile:   r.profile,
					Path:      p,
					Kind:      convstore.SourceDir,
					SizeBytes: size,
					MtimeNs:   mtime,
				})
			} else if strings.HasSuffix(p, ".json") || strings.HasSuffix(p, ".jsonl") {
				if !env.MarkRealPath(p) {
					continue
				}
				size, mtime, ok := convstore.StatFile(p)
				if !ok {
					continue
				}
				out = append(out, convstore.DiscoveredSource{
					SourceKey: convstore.MakeSourceKey("deepseek", r.profile, p),
					Agent:     "deepseek",
					Profile:   r.profile,
					Path:      p,
					Kind:      convstore.SourceFile,
					SizeBytes: size,
					MtimeNs:   mtime,
				})
			}
		}
	}
	return out, nil
}

func (a *deepseekAdapter) ParseMetadata(ctx context.Context, src convstore.DiscoveredSource, _ *convstore.SourceCursor) (*convstore.MetadataResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var (
		id            string
		cwd           string
		branch        string
		model         string
		explicitTitle string
		firstPrompt   string
		createdMs     int64
		updatedMs     int64
		msgCount      int
	)

	inspectRecord := func(rec map[string]any) {
		if rec == nil {
			return
		}
		if id == "" {
			for _, k := range []string{"session_id", "sessionId", "id"} {
				if v, _ := rec[k].(string); v != "" {
					id = v
					break
				}
			}
		}
		if cwd == "" {
			for _, k := range []string{"cwd", "working_directory", "workdir"} {
				if v, _ := rec[k].(string); v != "" {
					cwd = v
					break
				}
			}
		}
		if branch == "" {
			for _, k := range []string{"git_branch", "gitBranch", "branch"} {
				if v, _ := rec[k].(string); v != "" {
					branch = v
					break
				}
			}
		}
		if v, _ := rec["model"].(string); v != "" {
			model = v
		}
		for _, k := range []string{"title", "summary", "name"} {
			if v, _ := rec[k].(string); v != "" {
				if ct := convstore.CleanTitle(v); ct != "" && explicitTitle == "" {
					explicitTitle = ct
				}
			}
		}
		for _, k := range []string{"created_at", "createdAt", "started_at", "start_time", "timestamp", "updated_at", "updatedAt"} {
			if ts := convstore.ParseTimestampMs(rec[k]); ts > 0 {
				if createdMs == 0 || ts < createdMs {
					createdMs = ts
				}
				if ts > updatedMs {
					updatedMs = ts
				}
			}
		}
		role, text := deepseekExtractMessage(rec)
		if role == "user" || role == "assistant" {
			msgCount++
		}
		if role == "user" && firstPrompt == "" && text != "" {
			if ct := convstore.CleanTitle(text); ct != "" {
				firstPrompt = ct
			}
		}
	}

	inspectFile := func(path string) {
		if strings.HasSuffix(path, ".jsonl") {
			head, tail, err := convstore.ReadHeadTailLines(path, convstore.DefaultHeadBytes, convstore.DefaultTailBytes, 80, 30)
			if err != nil {
				return
			}
			for _, line := range head {
				var rec map[string]any
				if json.Unmarshal([]byte(line), &rec) == nil {
					inspectRecord(rec)
				}
			}
			for _, line := range tail {
				var rec map[string]any
				if json.Unmarshal([]byte(line), &rec) == nil {
					inspectRecord(rec)
				}
			}
			return
		}
		if strings.HasSuffix(path, ".json") {
			raw, err := os.ReadFile(path)
			if err != nil {
				return
			}
			var top any
			if json.Unmarshal(raw, &top) != nil {
				return
			}
			switch v := top.(type) {
			case map[string]any:
				inspectRecord(v)
				for _, arrKey := range []string{"messages", "turns", "history"} {
					if arr, ok := v[arrKey].([]any); ok {
						for _, item := range arr {
							if m, _ := item.(map[string]any); m != nil {
								inspectRecord(m)
							}
						}
					}
				}
			case []any:
				for _, item := range v {
					if m, _ := item.(map[string]any); m != nil {
						inspectRecord(m)
					}
				}
			}
		}
	}

	if src.Kind == convstore.SourceDir {
		if fi, err := os.Stat(src.Path); err != nil || !fi.IsDir() {
			return nil, err
		}
		// Inspect metadata files first, then remaining JSON/JSONL files in sorted order.
		priority := []string{"meta.json", "session.json", "summary.json"}
		seen := make(map[string]bool)
		for _, name := range priority {
			p := filepath.Join(src.Path, name)
			if _, _, ok := convstore.StatFile(p); ok {
				seen[p] = true
				inspectFile(p)
			}
		}
		for _, f := range deepseekListDataFiles(src.Path) {
			if seen[f] {
				continue
			}
			inspectFile(f)
		}
		if id == "" {
			id = filepath.Base(src.Path)
		}
	} else {
		if _, err := os.Stat(src.Path); err != nil {
			return nil, err
		}
		inspectFile(src.Path)
		if id == "" {
			base := filepath.Base(src.Path)
			id = strings.TrimSuffix(base, filepath.Ext(base))
		}
	}

	if cwd == "" {
		encodedParent := filepath.Base(filepath.Dir(src.Path))
		cwd = deepseekDecodeDirName(encodedParent)
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
				Agent:        "deepseek",
				ID:           id,
				App:          convstore.DisplayApp("deepseek"),
				Profile:      src.Profile,
				Title:        title,
				Preview:      preview,
				CWD:          cwd,
				Branch:       branch,
				Model:        model,
				CreatedAtMs:  createdMs,
				UpdatedAtMs:  updatedMs,
				MessageCount: msgCount,
				SourceKey:    src.SourceKey,
				SourcePath:   src.Path,
				ResumeTarget: id,
			},
		},
	}, nil
}

func (a *deepseekAdapter) ParseMessages(ctx context.Context, src convstore.DiscoveredSource, prev *convstore.SourceCursor) (*convstore.MessagesResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	convID := deepseekResolveSessionID(src)

	if src.Kind == convstore.SourceFile && strings.HasSuffix(src.Path, ".jsonl") {
		return deepseekParseIncrementalJSONL(ctx, src.Path, convID, prev)
	}

	var files []string
	if src.Kind == convstore.SourceDir {
		files = deepseekListDataFiles(src.Path)
	} else {
		files = []string{src.Path}
	}
	if len(files) == 0 {
		return nil, nil
	}

	// If there is a single JSONL file in the session dir (and no other message JSON),
	// use incremental JSONL streaming.
	var jsonlFiles, jsonFiles []string
	for _, f := range files {
		base := filepath.Base(f)
		if strings.HasSuffix(f, ".jsonl") {
			jsonlFiles = append(jsonlFiles, f)
		} else if strings.HasSuffix(f, ".json") && base != "meta.json" && base != "summary.json" {
			jsonFiles = append(jsonFiles, f)
		}
	}
	if len(jsonlFiles) == 1 && len(jsonFiles) == 0 {
		return deepseekParseIncrementalJSONL(ctx, jsonlFiles[0], convID, prev)
	}

	var msgs []convstore.Message
	appendMsg := func(rec map[string]any) {
		role, raw := deepseekExtractMessage(rec)
		if role != "user" && role != "assistant" {
			return
		}
		cleaned, truncated := convstore.CleanMessageText(raw)
		if cleaned == "" {
			return
		}
		msgs = append(msgs, convstore.Message{
			Seq:       len(msgs),
			Role:      role,
			Text:      cleaned,
			TsMs:      deepseekMessageTimestamp(rec),
			Truncated: truncated,
		})
	}

	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if strings.HasSuffix(f, ".jsonl") {
			_, _, _, _ = convstore.ReadIncrementalJSONL(f, 0, "", 0, func(line []byte) error {
				var rec map[string]any
				if json.Unmarshal(line, &rec) == nil && rec != nil {
					appendMsg(rec)
				}
				return nil
			})
			continue
		}
		if strings.HasSuffix(f, ".json") {
			raw, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			var top any
			if json.Unmarshal(raw, &top) != nil {
				continue
			}
			switch v := top.(type) {
			case []any:
				for _, item := range v {
					if m, _ := item.(map[string]any); m != nil {
						appendMsg(m)
					}
				}
			case map[string]any:
				foundNested := false
				for _, arrKey := range []string{"messages", "turns", "history"} {
					if arr, ok := v[arrKey].([]any); ok {
						foundNested = true
						for _, item := range arr {
							if m, _ := item.(map[string]any); m != nil {
								appendMsg(m)
							}
						}
					}
				}
				if !foundNested {
					appendMsg(v)
				}
			}
		}
	}

	if len(msgs) == 0 {
		return nil, nil
	}
	return &convstore.MessagesResult{
		Messages: map[string][]convstore.Message{convID: msgs},
		Append:   false,
	}, nil
}

func deepseekParseIncrementalJSONL(ctx context.Context, path, convID string, prev *convstore.SourceCursor) (*convstore.MessagesResult, error) {
	var prevOffset int64
	var prevHash string
	if prev != nil {
		prevOffset = prev.MsgByteOffset
		prevHash = prev.MsgPrefixHash
	}
	var msgs []convstore.Message
	newOffset, newHash, appended, err := convstore.ReadIncrementalJSONL(path, prevOffset, prevHash, 0, func(line []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var rec map[string]any
		if json.Unmarshal(line, &rec) != nil || rec == nil {
			return nil
		}
		role, raw := deepseekExtractMessage(rec)
		if role != "user" && role != "assistant" {
			return nil
		}
		cleaned, truncated := convstore.CleanMessageText(raw)
		if cleaned == "" {
			return nil
		}
		msgs = append(msgs, convstore.Message{
			Seq:       len(msgs),
			Role:      role,
			Text:      cleaned,
			TsMs:      deepseekMessageTimestamp(rec),
			Truncated: truncated,
		})
		return nil
	})
	if err != nil {
		return nil, err
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

func deepseekResolveSessionID(src convstore.DiscoveredSource) string {
	if src.Kind == convstore.SourceDir {
		for _, name := range []string{"meta.json", "session.json", "summary.json"} {
			if raw, err := os.ReadFile(filepath.Join(src.Path, name)); err == nil {
				var rec map[string]any
				if json.Unmarshal(raw, &rec) == nil && rec != nil {
					for _, k := range []string{"session_id", "sessionId", "id"} {
						if v, _ := rec[k].(string); v != "" {
							return v
						}
					}
				}
			}
		}
		return filepath.Base(src.Path)
	}
	if strings.HasSuffix(src.Path, ".jsonl") {
		if head, _, err := convstore.ReadHeadTailLines(src.Path, 8<<10, 0, 5, 0); err == nil {
			for _, line := range head {
				var rec map[string]any
				if json.Unmarshal([]byte(line), &rec) == nil && rec != nil {
					for _, k := range []string{"session_id", "sessionId", "id"} {
						if v, _ := rec[k].(string); v != "" {
							return v
						}
					}
				}
			}
		}
	}
	base := filepath.Base(src.Path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

func deepseekStatDir(dir string, dirInfo os.FileInfo) (int64, int64) {
	var totalSize int64
	maxMtime := dirInfo.ModTime().UnixNano()
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if fi, serr := d.Info(); serr == nil {
			totalSize += fi.Size()
			if mt := fi.ModTime().UnixNano(); mt > maxMtime {
				maxMtime = mt
			}
		}
		return nil
	})
	return totalSize, maxMtime
}

func deepseekListDataFiles(dir string) []string {
	var files []string
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(d.Name(), ".json") || strings.HasSuffix(d.Name(), ".jsonl") {
			files = append(files, path)
		}
		return nil
	})
	sort.Strings(files)
	return files
}

// deepseekDecodeDirName decodes DeepSeek's "--path-segments--" directory name
// back into a best-effort absolute path, probing the local filesystem to
// preserve hyphenated directory names when they exist on disk.
func deepseekDecodeDirName(name string) string {
	if !strings.HasPrefix(name, "--") || !strings.HasSuffix(name, "--") || len(name) <= 4 {
		return ""
	}
	inner := name[2 : len(name)-2]
	if inner == "" {
		return ""
	}
	tokens := strings.Split(inner, "-")
	cur := "/"
	i := 0
	for i < len(tokens) {
		matched := false
		// Greedily prefer the longest hyphen-joined segment that exists on disk.
		for end := len(tokens); end > i; end-- {
			seg := strings.Join(tokens[i:end], "-")
			cand := filepath.Join(cur, seg)
			if fi, err := os.Stat(cand); err == nil && fi.IsDir() {
				cur = cand
				i = end
				matched = true
				break
			}
		}
		if !matched {
			cur = filepath.Join(cur, tokens[i])
			i++
		}
	}
	return filepath.Clean(cur)
}

func deepseekExtractMessage(rec map[string]any) (string, string) {
	role := deepseekNormalizeRole(rec["role"])
	if role == "" {
		role = deepseekNormalizeRole(rec["type"])
	}
	if role == "" {
		if msg, ok := rec["message"].(map[string]any); ok && msg != nil {
			role = deepseekNormalizeRole(msg["role"])
		}
	}
	if role == "" {
		return "", ""
	}
	if msg, ok := rec["message"].(map[string]any); ok && msg != nil {
		for _, k := range []string{"content", "parts", "text"} {
			if raw, exists := msg[k]; exists {
				if s := convstore.ExtractPlainText(raw); s != "" {
					return role, s
				}
			}
		}
	}
	for _, k := range []string{"content", "parts", "text"} {
		if raw, exists := rec[k]; exists {
			if s := convstore.ExtractPlainText(raw); s != "" {
				return role, s
			}
		}
	}
	return role, ""
}

func deepseekNormalizeRole(v any) string {
	s, _ := v.(string)
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "user", "user_message":
		return "user"
	case "assistant", "assistant_message", "model":
		return "assistant"
	default:
		return ""
	}
}

func deepseekMessageTimestamp(rec map[string]any) int64 {
	for _, k := range []string{"timestamp", "created_at", "ts"} {
		if ms := convstore.ParseTimestampMs(rec[k]); ms > 0 {
			return ms
		}
	}
	return 0
}
