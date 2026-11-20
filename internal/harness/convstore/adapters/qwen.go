package adapters

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/arhammahajan/model-router/internal/harness/convstore"
)

type qwenAdapter struct{}

// NewQwenAdapter returns the convstore adapter for Qwen (~/.qwen/projects/*/chats/*.jsonl).
func NewQwenAdapter() convstore.Adapter {
	return &qwenAdapter{}
}

func (a *qwenAdapter) Agent() string { return "qwen" }

func (a *qwenAdapter) Discover(ctx context.Context, env *convstore.ScanEnv) ([]convstore.DiscoveredSource, error) {
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
			dir: filepath.Join(env.Paths.Home, ".qwen", "projects"),
		})
	}
	for _, ph := range env.ProfileShadowHomes("qwen") {
		roots = append(roots,
			rootSpec{dir: filepath.Join(ph.ShadowHome, "projects"), profile: ph.Profile},
			rootSpec{dir: filepath.Join(ph.ShadowHome, ".qwen", "projects"), profile: ph.Profile},
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
		matches, _ := filepath.Glob(filepath.Join(r.dir, "*", "chats", "*.jsonl"))
		for _, path := range matches {
			if err := ctx.Err(); err != nil {
				return out, err
			}
			if strings.HasSuffix(path, ".runtime.json") {
				continue
			}
			if !env.MarkRealPath(path) {
				continue
			}
			size, mtime, ok := convstore.StatFile(path)
			if !ok {
				continue
			}
			out = append(out, convstore.DiscoveredSource{
				SourceKey: convstore.MakeSourceKey("qwen", r.profile, path),
				Agent:     "qwen",
				Profile:   r.profile,
				Path:      path,
				Kind:      convstore.SourceFile,
				SizeBytes: size,
				MtimeNs:   mtime,
			})
		}
	}
	return out, nil
}

func (a *qwenAdapter) ParseMetadata(ctx context.Context, src convstore.DiscoveredSource, _ *convstore.SourceCursor) (*convstore.MetadataResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	head, tail, err := convstore.ReadHeadTailLines(src.Path, convstore.DefaultHeadBytes, convstore.DefaultTailBytes, 100, 50)
	if err != nil {
		return nil, err
	}
	if len(head) == 0 && len(tail) == 0 {
		return &convstore.MetadataResult{}, nil
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
		sawAnyJSON    bool
	)

	processLine := func(line string) {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil || rec == nil {
			return
		}
		sawAnyJSON = true

		if id == "" {
			for _, k := range []string{"sessionId", "session_id", "id"} {
				if v, _ := rec[k].(string); v != "" {
					id = v
					break
				}
			}
		}
		if cwd == "" {
			if v, _ := rec["cwd"].(string); v != "" {
				cwd = v
			}
		}
		if branch == "" {
			for _, k := range []string{"gitBranch", "git_branch", "branch"} {
				if v, _ := rec[k].(string); v != "" {
					branch = v
					break
				}
			}
		}
		if v, _ := rec["model"].(string); v != "" {
			model = v
		}
		if msg, ok := rec["message"].(map[string]any); ok && msg != nil {
			if v, _ := msg["model"].(string); v != "" {
				model = v
			}
		}
		if v, _ := rec["title"].(string); v != "" {
			if ct := convstore.CleanTitle(v); ct != "" {
				explicitTitle = ct
			}
		}

		for _, k := range []string{"timestamp", "createdAt", "created_at", "updatedAt", "updated_at"} {
			if ts := convstore.ParseTimestampMs(rec[k]); ts > 0 {
				if createdMs == 0 || ts < createdMs {
					createdMs = ts
				}
				if ts > updatedMs {
					updatedMs = ts
				}
			}
		}

		role := qwenRecordRole(rec)
		if role == "user" || role == "assistant" {
			msgCount++
		}
		if role == "user" && firstPrompt == "" {
			raw := qwenExtractContent(rec)
			if ct := convstore.CleanTitle(raw); ct != "" {
				firstPrompt = ct
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
				Agent:        "qwen",
				ID:           id,
				App:          convstore.DisplayApp("qwen"),
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

func (a *qwenAdapter) ParseMessages(ctx context.Context, src convstore.DiscoveredSource, prev *convstore.SourceCursor) (*convstore.MessagesResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	convID := qwenResolveSessionID(src.Path)

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
			for _, k := range []string{"sessionId", "session_id", "id"} {
				if v, _ := rec[k].(string); v != "" {
					convID = v
					break
				}
			}
		}
		role := qwenRecordRole(rec)
		if role != "user" && role != "assistant" {
			return nil
		}
		raw := qwenExtractContent(rec)
		cleaned, truncated := convstore.CleanMessageText(raw)
		if cleaned == "" {
			return nil
		}
		var ts int64
		for _, k := range []string{"timestamp", "createdAt", "created_at", "ts"} {
			if ms := convstore.ParseTimestampMs(rec[k]); ms > 0 {
				ts = ms
				break
			}
		}
		msgs = append(msgs, convstore.Message{
			Seq:       len(msgs),
			Role:      role,
			Text:      cleaned,
			TsMs:      ts,
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

func qwenResolveSessionID(path string) string {
	head, _, err := convstore.ReadHeadTailLines(path, 8<<10, 0, 10, 0)
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

func qwenRecordRole(rec map[string]any) string {
	for _, cand := range []any{rec["type"], rec["role"]} {
		if r := qwenNormalizeRole(cand); r != "" {
			return r
		}
	}
	if msg, ok := rec["message"].(map[string]any); ok && msg != nil {
		if r := qwenNormalizeRole(msg["role"]); r != "" {
			return r
		}
	}
	return ""
}

func qwenNormalizeRole(v any) string {
	s, _ := v.(string)
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "user":
		return "user"
	case "assistant", "model":
		return "assistant"
	default:
		return ""
	}
}

func qwenExtractContent(rec map[string]any) string {
	if msg, ok := rec["message"].(map[string]any); ok && msg != nil {
		if raw, ok := msg["parts"]; ok {
			if s := convstore.ExtractPlainText(raw); s != "" {
				return s
			}
		}
		if raw, ok := msg["content"]; ok {
			if s := convstore.ExtractPlainText(raw); s != "" {
				return s
			}
		}
	}
	if raw, ok := rec["parts"]; ok {
		if s := convstore.ExtractPlainText(raw); s != "" {
			return s
		}
	}
	if raw, ok := rec["content"]; ok {
		if s := convstore.ExtractPlainText(raw); s != "" {
			return s
		}
	}
	return ""
}
