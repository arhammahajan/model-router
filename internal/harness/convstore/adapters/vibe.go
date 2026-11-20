package adapters

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/arhammahajan/model-router/internal/harness/convstore"
)

type vibeAdapter struct{}

// NewVibeAdapter returns the convstore adapter for Vibe
// (~/.vibe/logs/session/unified/*/meta.json + optional sibling JSONL transcript).
func NewVibeAdapter() convstore.Adapter {
	return &vibeAdapter{}
}

func (a *vibeAdapter) Agent() string { return "vibe" }

var vibeTranscriptNames = []string{
	"messages.jsonl",
	"events.jsonl",
	"session.jsonl",
	"turns.jsonl",
}

func (a *vibeAdapter) Discover(ctx context.Context, env *convstore.ScanEnv) ([]convstore.DiscoveredSource, error) {
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
			dir: filepath.Join(env.Paths.Home, ".vibe", "logs", "session", "unified"),
		})
	}
	for _, ph := range env.ProfileShadowHomes("vibe") {
		roots = append(roots,
			rootSpec{dir: filepath.Join(ph.ShadowHome, "logs", "session", "unified"), profile: ph.Profile},
			rootSpec{dir: filepath.Join(ph.ShadowHome, ".vibe", "logs", "session", "unified"), profile: ph.Profile},
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
		matches, _ := filepath.Glob(filepath.Join(r.dir, "*", "meta.json"))
		for _, metaPath := range matches {
			if err := ctx.Err(); err != nil {
				return out, err
			}
			if !env.MarkRealPath(metaPath) {
				continue
			}
			size, mtime, ok := convstore.StatFile(metaPath)
			if !ok {
				continue
			}
			var walSize, walMtime int64
			if tPath := vibeFindTranscript(filepath.Dir(metaPath)); tPath != "" {
				walSize, walMtime, _ = convstore.StatFile(tPath)
			}
			out = append(out, convstore.DiscoveredSource{
				SourceKey:  convstore.MakeSourceKey("vibe", r.profile, metaPath),
				Agent:      "vibe",
				Profile:    r.profile,
				Path:       metaPath,
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

func (a *vibeAdapter) ParseMetadata(ctx context.Context, src convstore.DiscoveredSource, _ *convstore.SourceCursor) (*convstore.MetadataResult, error) {
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
		id = filepath.Base(filepath.Dir(src.Path))
	}

	var cwd, branch string
	if envMap, _ := rec["environment"].(map[string]any); envMap != nil {
		cwd, _ = envMap["working_directory"].(string)
		branch, _ = envMap["git_branch"].(string)
	}
	if cwd == "" {
		cwd, _ = rec["origin_directory"].(string)
	}
	if cwd == "" {
		cwd, _ = rec["cwd"].(string)
	}
	if branch == "" {
		if b, _ := rec["git_branch"].(string); b != "" {
			branch = b
		} else if b, _ := rec["branch"].(string); b != "" {
			branch = b
		}
	}

	model, _ := rec["model"].(string)
	if model == "" {
		if cfg, _ := rec["config"].(map[string]any); cfg != nil {
			if m, _ := cfg["active_model"].(string); m != "" {
				model = m
			} else if m, _ := cfg["model"].(string); m != "" {
				model = m
			}
		}
	}

	title := ""
	if s, _ := rec["title"].(string); s != "" {
		title = convstore.CleanTitle(s)
	}

	createdMs := convstore.ParseTimestampMs(rec["start_time"])
	if createdMs == 0 {
		createdMs = convstore.ParseTimestampMs(rec["created_at"])
	}
	updatedMs := convstore.ParseTimestampMs(rec["end_time"])
	if updatedMs == 0 {
		updatedMs = convstore.ParseTimestampMs(rec["updated_at"])
	}
	if updatedMs == 0 {
		updatedMs = createdMs
	}

	var firstPrompt string
	if tPath := vibeFindTranscript(filepath.Dir(src.Path)); tPath != "" {
		if head, tail, rerr := convstore.ReadHeadTailLines(tPath, convstore.DefaultHeadBytes, convstore.DefaultTailBytes, 60, 20); rerr == nil {
			scan := func(line string) {
				var m map[string]any
				if json.Unmarshal([]byte(line), &m) != nil || m == nil {
					return
				}
				if ts := vibeMessageTimestamp(m); ts > 0 && ts > updatedMs {
					updatedMs = ts
				}
				if firstPrompt == "" {
					role, text := vibeExtractMessage(m)
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
				Agent:        "vibe",
				ID:           id,
				App:          convstore.DisplayApp("vibe"),
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

func (a *vibeAdapter) ParseMessages(ctx context.Context, src convstore.DiscoveredSource, prev *convstore.SourceCursor) (*convstore.MessagesResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tPath := vibeFindTranscript(filepath.Dir(src.Path))
	if tPath == "" {
		return nil, nil
	}
	convID := vibeResolveSessionID(src.Path)

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
		role, raw := vibeExtractMessage(rec)
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
			TsMs:      vibeMessageTimestamp(rec),
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

func vibeFindTranscript(dir string) string {
	for _, name := range vibeTranscriptNames {
		p := filepath.Join(dir, name)
		if _, _, ok := convstore.StatFile(p); ok {
			return p
		}
	}
	return ""
}

func vibeResolveSessionID(metaPath string) string {
	if raw, err := os.ReadFile(metaPath); err == nil {
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
	return filepath.Base(filepath.Dir(metaPath))
}

func vibeExtractMessage(rec map[string]any) (string, string) {
	role := vibeNormalizeRole(rec["role"])
	if role == "" {
		role = vibeNormalizeRole(rec["type"])
	}
	if role == "" {
		if msg, ok := rec["message"].(map[string]any); ok && msg != nil {
			role = vibeNormalizeRole(msg["role"])
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
	for _, k := range []string{"content", "parts", "text", "message"} {
		if raw, exists := rec[k]; exists {
			if s := convstore.ExtractPlainText(raw); s != "" {
				return role, s
			}
		}
	}
	return role, ""
}

func vibeNormalizeRole(v any) string {
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

func vibeMessageTimestamp(rec map[string]any) int64 {
	for _, k := range []string{"timestamp", "created_at", "ts"} {
		if ms := convstore.ParseTimestampMs(rec[k]); ms > 0 {
			return ms
		}
	}
	return 0
}
