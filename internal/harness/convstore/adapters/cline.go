package adapters

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/arhammahajan/model-router/internal/harness/convstore"
)

type clineAdapter struct{}

// NewClineAdapter returns the convstore adapter for Cline
// (~/.cline/data/sessions/*/*.json + sibling <stem>.messages.json).
func NewClineAdapter() convstore.Adapter {
	return &clineAdapter{}
}

func (a *clineAdapter) Agent() string { return "cline" }

func (a *clineAdapter) Discover(ctx context.Context, env *convstore.ScanEnv) ([]convstore.DiscoveredSource, error) {
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
			dir: filepath.Join(env.Paths.Home, ".cline", "data", "sessions"),
		})
	}
	for _, ph := range env.ProfileShadowHomes("cline") {
		roots = append(roots,
			rootSpec{dir: filepath.Join(ph.ShadowHome, "sessions"), profile: ph.Profile},
			rootSpec{dir: filepath.Join(ph.ShadowHome, ".cline", "data", "sessions"), profile: ph.Profile},
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
		matches, _ := filepath.Glob(filepath.Join(r.dir, "*", "*.json"))
		for _, path := range matches {
			if err := ctx.Err(); err != nil {
				return out, err
			}
			if strings.HasSuffix(path, ".messages.json") {
				continue
			}
			if !env.MarkRealPath(path) {
				continue
			}
			size, mtime, ok := convstore.StatFile(path)
			if !ok {
				continue
			}
			msgPath := clineMessagesPath(path)
			walSize, walMtime, _ := convstore.StatFile(msgPath)
			out = append(out, convstore.DiscoveredSource{
				SourceKey:  convstore.MakeSourceKey("cline", r.profile, path),
				Agent:      "cline",
				Profile:    r.profile,
				Path:       path,
				Kind:       convstore.SourceFile,
				SizeBytes:  size,
				MtimeNs:    mtime,
				WALSize:    walSize,
				WALMtimeNs: walMtime,
			})
		}
	}
	return out, nil
}

func (a *clineAdapter) ParseMetadata(ctx context.Context, src convstore.DiscoveredSource, _ *convstore.SourceCursor) (*convstore.MetadataResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(src.Path)
	if err != nil {
		return nil, err
	}
	var rec map[string]any
	if err := json.Unmarshal(raw, &rec); err != nil || rec == nil {
		return &convstore.MetadataResult{}, nil
	}

	id, _ := rec["session_id"].(string)
	if id == "" {
		id, _ = rec["id"].(string)
	}
	if id == "" {
		base := filepath.Base(src.Path)
		id = strings.TrimSuffix(base, filepath.Ext(base))
	}

	cwd, _ := rec["cwd"].(string)
	branch, _ := rec["branch"].(string)
	if branch == "" {
		branch, _ = rec["git_branch"].(string)
	}
	model, _ := rec["model"].(string)
	if model == "" {
		model, _ = rec["model_id"].(string)
	}

	rawTitle, _ := rec["title"].(string)
	rawPrompt, _ := rec["prompt"].(string)
	title := convstore.CleanTitle(rawTitle)
	promptClean := convstore.CleanTitle(rawPrompt)
	if title == "" {
		title = promptClean
	}
	preview := convstore.CleanPreview(rawPrompt)
	if preview == "" {
		preview = convstore.CleanPreview(title)
	}

	createdMs := convstore.ParseTimestampMs(rec["started_at"])
	if createdMs == 0 {
		createdMs = convstore.ParseTimestampMs(rec["created_at"])
	}
	updatedMs := convstore.ParseTimestampMs(rec["updated_at"])
	if updatedMs == 0 {
		updatedMs = convstore.ParseTimestampMs(rec["ended_at"])
	}
	if updatedMs == 0 {
		updatedMs = createdMs
	}
	if src.WALMtimeNs/1e6 > updatedMs {
		updatedMs = src.WALMtimeNs / 1e6
	}
	if updatedMs == 0 {
		updatedMs = src.MtimeNs / 1e6
	}

	return &convstore.MetadataResult{
		Conversations: []convstore.Conversation{
			{
				Agent:        "cline",
				ID:           id,
				App:          convstore.DisplayApp("cline"),
				Profile:      src.Profile,
				Title:        title,
				Preview:      preview,
				CWD:          cwd,
				Branch:       branch,
				Model:        model,
				CreatedAtMs:  createdMs,
				UpdatedAtMs:  updatedMs,
				SourceKey:    src.SourceKey,
				SourcePath:   src.Path,
				ResumeTarget: id,
			},
		},
	}, nil
}

func (a *clineAdapter) ParseMessages(ctx context.Context, src convstore.DiscoveredSource, _ *convstore.SourceCursor) (*convstore.MessagesResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	msgPath := clineMessagesPath(src.Path)
	if _, _, ok := convstore.StatFile(msgPath); !ok {
		return nil, nil
	}
	convID := clineResolveSessionID(src.Path)

	raw, err := os.ReadFile(msgPath)
	if err != nil {
		return nil, err
	}
	var top any
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, nil
	}

	var items []any
	switch v := top.(type) {
	case []any:
		items = v
	case map[string]any:
		if arr, ok := v["messages"].([]any); ok {
			items = arr
		} else if arr, ok := v["history"].([]any); ok {
			items = arr
		}
	}

	var msgs []convstore.Message
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		role := clineNormalizeRole(m["role"])
		if role == "" {
			role = clineNormalizeRole(m["type"])
		}
		if role == "" {
			continue
		}
		rawText := convstore.ExtractPlainText(m["content"])
		if rawText == "" {
			rawText = convstore.ExtractPlainText(m["text"])
		}
		cleaned, truncated := convstore.CleanMessageText(rawText)
		if cleaned == "" {
			continue
		}
		var ts int64
		for _, k := range []string{"timestamp", "ts", "created_at"} {
			if ms := convstore.ParseTimestampMs(m[k]); ms > 0 {
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

func clineMessagesPath(sessionPath string) string {
	return strings.TrimSuffix(sessionPath, ".json") + ".messages.json"
}

func clineResolveSessionID(sessionPath string) string {
	if raw, err := os.ReadFile(sessionPath); err == nil {
		var rec map[string]any
		if json.Unmarshal(raw, &rec) == nil && rec != nil {
			if id, _ := rec["session_id"].(string); id != "" {
				return id
			}
			if id, _ := rec["id"].(string); id != "" {
				return id
			}
		}
	}
	base := filepath.Base(sessionPath)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

func clineNormalizeRole(v any) string {
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
