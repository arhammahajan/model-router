package adapters

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/arhammahajan/model-router/internal/harness/convstore"
)

type copilotAdapter struct{}

// NewCopilotAdapter returns the convstore adapter for GitHub Copilot CLI
// (~/.copilot/session-state/*/workspace.yaml + sibling events.jsonl).
func NewCopilotAdapter() convstore.Adapter {
	return &copilotAdapter{}
}

func (a *copilotAdapter) Agent() string { return "copilot" }

func (a *copilotAdapter) Discover(ctx context.Context, env *convstore.ScanEnv) ([]convstore.DiscoveredSource, error) {
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
			dir: filepath.Join(env.Paths.Home, ".copilot", "session-state"),
		})
	}
	for _, ph := range env.ProfileShadowHomes("copilot") {
		roots = append(roots,
			rootSpec{dir: filepath.Join(ph.ShadowHome, "session-state"), profile: ph.Profile},
			rootSpec{dir: filepath.Join(ph.ShadowHome, ".copilot", "session-state"), profile: ph.Profile},
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
		matches, _ := filepath.Glob(filepath.Join(r.dir, "*", "workspace.yaml"))
		for _, ws := range matches {
			if err := ctx.Err(); err != nil {
				return out, err
			}
			if !env.MarkRealPath(ws) {
				continue
			}
			size, mtime, ok := convstore.StatFile(ws)
			if !ok {
				continue
			}
			eventsPath := filepath.Join(filepath.Dir(ws), "events.jsonl")
			walSize, walMtime, _ := convstore.StatFile(eventsPath)
			out = append(out, convstore.DiscoveredSource{
				SourceKey:  convstore.MakeSourceKey("copilot", r.profile, ws),
				Agent:      "copilot",
				Profile:    r.profile,
				Path:       ws,
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

func (a *copilotAdapter) ParseMetadata(ctx context.Context, src convstore.DiscoveredSource, _ *convstore.SourceCursor) (*convstore.MetadataResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(src.Path)
	if err != nil {
		return nil, err
	}
	fields := copilotParseWorkspaceYAML(string(raw))

	// Skip automated provider health-check probe sessions (e.g. T3 Code probes).
	if strings.EqualFold(fields["client_name"], "t3-code-provider-test") ||
		strings.EqualFold(fields["last_client_name"], "t3-code-provider-test") {
		return &convstore.MetadataResult{}, nil
	}

	id := fields["id"]
	if id == "" {
		id = filepath.Base(filepath.Dir(src.Path))
	}
	cwd := fields["cwd"]
	if cwd == "" {
		cwd = fields["git_root"]
	}
	branch := fields["branch"]
	if branch == "" {
		branch = fields["git_branch"]
	}
	model := fields["model"]

	var title string
	for _, k := range []string{"name", "title", "summary"} {
		if ct := convstore.CleanTitle(fields[k]); ct != "" {
			title = ct
			break
		}
	}

	createdMs := convstore.ParseTimestampMs(fields["created_at"])
	updatedMs := convstore.ParseTimestampMs(fields["updated_at"])

	var firstPrompt string
	var msgCount int
	eventsPath := filepath.Join(filepath.Dir(src.Path), "events.jsonl")
	if head, tail, rerr := convstore.ReadHeadTailLines(eventsPath, convstore.DefaultHeadBytes, convstore.DefaultTailBytes, 80, 30); rerr == nil {
		scanEvent := func(line string) {
			var rec map[string]any
			if json.Unmarshal([]byte(line), &rec) != nil || rec == nil {
				return
			}
			if ts := copilotEventTimestamp(rec); ts > 0 {
				if createdMs == 0 || ts < createdMs {
					createdMs = ts
				}
				if ts > updatedMs {
					updatedMs = ts
				}
			}
			role, text := copilotExtractEventMessage(rec)
			if role == "user" || role == "assistant" {
				if text != "" {
					msgCount++
				}
				if firstPrompt == "" && role == "user" && text != "" {
					if ct := convstore.CleanTitle(text); ct != "" {
						firstPrompt = ct
					}
				}
			}
		}
		for _, line := range head {
			scanEvent(line)
		}
		for _, line := range tail {
			scanEvent(line)
		}
	}

	if title == "" {
		title = firstPrompt
	}
	preview := convstore.CleanPreview(firstPrompt)
	if preview == "" {
		preview = convstore.CleanPreview(title)
	}
	// Skip empty unstarted Copilot sessions (no title/summary and no user/assistant messages).
	if title == "" && preview == "" && msgCount == 0 {
		return &convstore.MetadataResult{}, nil
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
				Agent:        "copilot",
				ID:           id,
				App:          convstore.DisplayApp("copilot"),
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

func (a *copilotAdapter) ParseMessages(ctx context.Context, src convstore.DiscoveredSource, prev *convstore.SourceCursor) (*convstore.MessagesResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	eventsPath := filepath.Join(filepath.Dir(src.Path), "events.jsonl")
	if _, _, ok := convstore.StatFile(eventsPath); !ok {
		return nil, nil
	}

	convID := copilotResolveSessionID(src.Path)
	var prevOffset int64
	var prevHash string
	if prev != nil {
		prevOffset = prev.MsgByteOffset
		prevHash = prev.MsgPrefixHash
	}

	var msgs []convstore.Message
	newOffset, newHash, appended, err := convstore.ReadIncrementalJSONL(eventsPath, prevOffset, prevHash, 0, func(line []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil || rec == nil {
			return nil
		}
		role, raw := copilotExtractEventMessage(rec)
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
			TsMs:      copilotEventTimestamp(rec),
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

func copilotResolveSessionID(wsPath string) string {
	if raw, err := os.ReadFile(wsPath); err == nil {
		if id := copilotParseWorkspaceYAML(string(raw))["id"]; id != "" {
			return id
		}
	}
	return filepath.Base(filepath.Dir(wsPath))
}

func copilotParseWorkspaceYAML(content string) map[string]string {
	out := make(map[string]string)
	lines := strings.Split(content, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		// Skip indented lines unless captured as part of a block scalar below.
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key := strings.TrimSpace(k)
		val := strings.TrimSpace(v)
		if val == "|" || val == "|-" || val == ">" || val == ">-" {
			var block []string
			for j := i + 1; j < len(lines); j++ {
				if lines[j] == "" {
					block = append(block, "")
					i = j
					continue
				}
				if strings.HasPrefix(lines[j], " ") || strings.HasPrefix(lines[j], "\t") {
					block = append(block, strings.TrimSpace(lines[j]))
					i = j
					continue
				}
				break
			}
			val = strings.TrimSpace(strings.Join(block, " "))
		} else {
			val = copilotUnquoteYAML(val)
		}
		if key != "" && val != "" {
			out[key] = val
		}
	}
	return out
}

func copilotUnquoteYAML(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

func copilotEventTimestamp(rec map[string]any) int64 {
	for _, k := range []string{"timestamp", "created_at", "updated_at", "ts"} {
		if ms := convstore.ParseTimestampMs(rec[k]); ms > 0 {
			return ms
		}
	}
	if data, ok := rec["data"].(map[string]any); ok && data != nil {
		for _, k := range []string{"timestamp", "created_at", "ts"} {
			if ms := convstore.ParseTimestampMs(data[k]); ms > 0 {
				return ms
			}
		}
	}
	return 0
}

func copilotExtractEventMessage(rec map[string]any) (string, string) {
	typ, _ := rec["type"].(string)
	var role string
	switch strings.ToLower(strings.TrimSpace(typ)) {
	case "user.message", "user_message", "user":
		role = "user"
	case "assistant.message", "assistant_message", "assistant":
		role = "assistant"
	}
	if role == "" {
		for _, cand := range []any{rec["role"]} {
			if s, _ := cand.(string); s != "" {
				switch strings.ToLower(strings.TrimSpace(s)) {
				case "user":
					role = "user"
				case "assistant", "model":
					role = "assistant"
				}
			}
		}
	}
	if role == "" {
		if data, ok := rec["data"].(map[string]any); ok && data != nil {
			if s, _ := data["role"].(string); s != "" {
				switch strings.ToLower(strings.TrimSpace(s)) {
				case "user":
					role = "user"
				case "assistant", "model":
					role = "assistant"
				}
			}
		}
	}
	if role == "" {
		if msg, ok := rec["message"].(map[string]any); ok && msg != nil {
			if s, _ := msg["role"].(string); s != "" {
				switch strings.ToLower(strings.TrimSpace(s)) {
				case "user":
					role = "user"
				case "assistant", "model":
					role = "assistant"
				}
			}
		}
	}
	if role == "" {
		return "", ""
	}

	if data, ok := rec["data"].(map[string]any); ok && data != nil {
		for _, k := range []string{"content", "transformedContent", "text", "message"} {
			if raw, exists := data[k]; exists {
				if s := convstore.ExtractPlainText(raw); s != "" {
					return role, s
				}
			}
		}
	}
	if msg, ok := rec["message"].(map[string]any); ok && msg != nil {
		if s := convstore.ExtractPlainText(msg["content"]); s != "" {
			return role, s
		}
	}
	for _, k := range []string{"content", "transformedContent", "text"} {
		if raw, exists := rec[k]; exists {
			if s := convstore.ExtractPlainText(raw); s != "" {
				return role, s
			}
		}
	}
	return role, ""
}
