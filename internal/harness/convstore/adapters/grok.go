package adapters

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/arhammahajan/model-router/internal/harness/convstore"
)

type grokAdapter struct{}

// NewGrokAdapter returns the convstore adapter for Grok
// (~/.grok/sessions/*/*/summary.json + optional sibling transcript).
func NewGrokAdapter() convstore.Adapter {
	return &grokAdapter{}
}

func (a *grokAdapter) Agent() string { return "grok" }

var grokTranscriptNames = []string{
	"messages.jsonl",
	"events.jsonl",
	"session.jsonl",
	"transcript.jsonl",
	"messages.json",
}

func (a *grokAdapter) Discover(ctx context.Context, env *convstore.ScanEnv) ([]convstore.DiscoveredSource, error) {
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
			dir: filepath.Join(env.Paths.Home, ".grok", "sessions"),
		})
	}
	for _, ph := range env.ProfileShadowHomes("grok") {
		roots = append(roots,
			rootSpec{dir: filepath.Join(ph.ShadowHome, "sessions"), profile: ph.Profile},
			rootSpec{dir: filepath.Join(ph.ShadowHome, ".grok", "sessions"), profile: ph.Profile},
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
		matches, _ := filepath.Glob(filepath.Join(r.dir, "*", "*", "summary.json"))
		for _, summaryPath := range matches {
			if err := ctx.Err(); err != nil {
				return out, err
			}
			if !env.MarkRealPath(summaryPath) {
				continue
			}
			size, mtime, ok := convstore.StatFile(summaryPath)
			if !ok {
				continue
			}
			var walSize, walMtime int64
			if tPath := grokFindTranscript(filepath.Dir(summaryPath)); tPath != "" {
				walSize, walMtime, _ = convstore.StatFile(tPath)
			}
			out = append(out, convstore.DiscoveredSource{
				SourceKey:  convstore.MakeSourceKey("grok", r.profile, summaryPath),
				Agent:      "grok",
				Profile:    r.profile,
				Path:       summaryPath,
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

func (a *grokAdapter) ParseMetadata(ctx context.Context, src convstore.DiscoveredSource, _ *convstore.SourceCursor) (*convstore.MetadataResult, error) {
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

	info, _ := rec["info"].(map[string]any)
	var id, cwd, branch, model string
	if info != nil {
		id, _ = info["id"].(string)
		cwd, _ = info["cwd"].(string)
		branch, _ = info["git_branch"].(string)
		if branch == "" {
			branch, _ = info["branch"].(string)
		}
		model, _ = info["model"].(string)
	}
	if id == "" {
		if v, _ := rec["id"].(string); v != "" {
			id = v
		} else if v, _ := rec["session_id"].(string); v != "" {
			id = v
		} else {
			id = filepath.Base(filepath.Dir(src.Path))
		}
	}
	if cwd == "" {
		cwd, _ = rec["cwd"].(string)
	}
	if model == "" {
		model, _ = rec["model"].(string)
	}

	var title string
	for _, k := range []string{"session_summary", "title", "summary"} {
		if s, _ := rec[k].(string); s != "" {
			if ct := convstore.CleanTitle(s); ct != "" {
				title = ct
				break
			}
		}
	}

	createdMs := convstore.ParseTimestampMs(rec["created_at"])
	if createdMs == 0 && info != nil {
		createdMs = convstore.ParseTimestampMs(info["created_at"])
	}
	updatedMs := convstore.ParseTimestampMs(rec["last_active_at"])
	if updatedMs == 0 {
		updatedMs = convstore.ParseTimestampMs(rec["updated_at"])
	}
	if updatedMs == 0 && info != nil {
		updatedMs = convstore.ParseTimestampMs(info["updated_at"])
	}
	if updatedMs == 0 {
		updatedMs = createdMs
	}

	var firstPrompt string
	if tPath := grokFindTranscript(filepath.Dir(src.Path)); tPath != "" && strings.HasSuffix(tPath, ".jsonl") {
		if head, tail, rerr := convstore.ReadHeadTailLines(tPath, convstore.DefaultHeadBytes, convstore.DefaultTailBytes, 60, 20); rerr == nil {
			scan := func(line string) {
				var m map[string]any
				if json.Unmarshal([]byte(line), &m) != nil || m == nil {
					return
				}
				if ts := grokMessageTimestamp(m); ts > 0 && ts > updatedMs {
					updatedMs = ts
				}
				if firstPrompt == "" {
					role, text := grokExtractMessage(m)
					if role == "user" && text != "" {
						if ct := convstore.CleanTitle(text); ct != "" {
							firstPrompt = ct
						}
					}
				}
			}
			for _, line := range head {
				scan(line)
			}
			for _, line := range tail {
				scan(line)
			}
		}
	}

	if title == "" {
		title = firstPrompt
	}
	preview := convstore.CleanPreview(firstPrompt)
	if preview == "" {
		preview = convstore.CleanPreview(title)
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
				Agent:        "grok",
				ID:           id,
				App:          convstore.DisplayApp("grok"),
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

func (a *grokAdapter) ParseMessages(ctx context.Context, src convstore.DiscoveredSource, prev *convstore.SourceCursor) (*convstore.MessagesResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tPath := grokFindTranscript(filepath.Dir(src.Path))
	if tPath == "" {
		return nil, nil
	}
	convID := grokResolveSessionID(src.Path)

	if strings.HasSuffix(tPath, ".json") {
		return grokParseJSONTranscript(ctx, tPath, convID)
	}

	var prevOffset int64
	var prevHash string
	if prev != nil {
		prevOffset = prev.MsgByteOffset
		prevHash = prev.MsgPrefixHash
	}

	var msgs []convstore.Message
	newOffset, newHash, appended, err := convstore.ReadIncrementalJSONL(tPath, prevOffset, prevHash, 0, func(line []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil || rec == nil {
			return nil
		}
		role, raw := grokExtractMessage(rec)
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
			TsMs:      grokMessageTimestamp(rec),
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

func grokParseJSONTranscript(ctx context.Context, path, convID string) (*convstore.MessagesResult, error) {
	raw, err := os.ReadFile(path)
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
		} else if arr, ok := v["turns"].([]any); ok {
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
		role, rawText := grokExtractMessage(m)
		if role != "user" && role != "assistant" {
			continue
		}
		cleaned, truncated := convstore.CleanMessageText(rawText)
		if cleaned == "" {
			continue
		}
		msgs = append(msgs, convstore.Message{
			Seq:       len(msgs),
			Role:      role,
			Text:      cleaned,
			TsMs:      grokMessageTimestamp(m),
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

func grokFindTranscript(dir string) string {
	for _, name := range grokTranscriptNames {
		p := filepath.Join(dir, name)
		if _, _, ok := convstore.StatFile(p); ok {
			return p
		}
	}
	return ""
}

func grokResolveSessionID(summaryPath string) string {
	if raw, err := os.ReadFile(summaryPath); err == nil {
		var rec map[string]any
		if json.Unmarshal(raw, &rec) == nil && rec != nil {
			if info, _ := rec["info"].(map[string]any); info != nil {
				if id, _ := info["id"].(string); id != "" {
					return id
				}
			}
			if id, _ := rec["id"].(string); id != "" {
				return id
			}
		}
	}
	return filepath.Base(filepath.Dir(summaryPath))
}

func grokExtractMessage(rec map[string]any) (string, string) {
	role := grokNormalizeRole(rec["role"])
	if role == "" {
		role = grokNormalizeRole(rec["type"])
	}
	if role == "" {
		if msg, ok := rec["message"].(map[string]any); ok && msg != nil {
			role = grokNormalizeRole(msg["role"])
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

func grokNormalizeRole(v any) string {
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

func grokMessageTimestamp(rec map[string]any) int64 {
	for _, k := range []string{"timestamp", "created_at", "ts"} {
		if ms := convstore.ParseTimestampMs(rec[k]); ms > 0 {
			return ms
		}
	}
	return 0
}
