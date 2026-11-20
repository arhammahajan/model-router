package adapters

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/arhammahajan/model-router/internal/harness/convstore"
)

type kimiAdapter struct{}

// NewKimiAdapter returns the convstore adapter for Kimi / Kimi Code
// (~/.kimi/sessions/*/* and ~/.kimi-code/sessions/*/*).
func NewKimiAdapter() convstore.Adapter {
	return &kimiAdapter{}
}

func (a *kimiAdapter) Agent() string { return "kimi" }

func (a *kimiAdapter) Discover(ctx context.Context, env *convstore.ScanEnv) ([]convstore.DiscoveredSource, error) {
	if env == nil {
		return nil, nil
	}
	type rootSpec struct {
		dir     string
		profile string
	}
	var roots []rootSpec
	if env.Paths.Home != "" {
		roots = append(roots,
			rootSpec{dir: filepath.Join(env.Paths.Home, ".kimi", "sessions")},
			rootSpec{dir: filepath.Join(env.Paths.Home, ".kimi-code", "sessions")},
		)
	}
	for _, agentKey := range []string{"kimi", "kimi-cli"} {
		for _, ph := range env.ProfileShadowHomes(agentKey) {
			roots = append(roots,
				rootSpec{dir: filepath.Join(ph.ShadowHome, "sessions"), profile: ph.Profile},
				rootSpec{dir: filepath.Join(ph.ShadowHome, ".kimi-code", "sessions"), profile: ph.Profile},
				rootSpec{dir: filepath.Join(ph.ShadowHome, ".kimi", "sessions"), profile: ph.Profile},
			)
		}
	}

	var out []convstore.DiscoveredSource
	for _, r := range roots {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if fi, err := os.Stat(r.dir); err != nil || !fi.IsDir() {
			continue
		}
		sessDirs, _ := filepath.Glob(filepath.Join(r.dir, "*", "*"))
		for _, sessDir := range sessDirs {
			if err := ctx.Err(); err != nil {
				return out, err
			}
			fi, err := os.Stat(sessDir)
			if err != nil || !fi.IsDir() {
				continue
			}
			statePath := filepath.Join(sessDir, "state.json")
			wirePath := filepath.Join(sessDir, "wire.jsonl")

			stateSize, stateMtime, hasState := convstore.StatFile(statePath)
			wireSize, wireMtime, hasWire := convstore.StatFile(wirePath)
			if !hasState && !hasWire {
				continue
			}

			var primaryPath string
			var size, mtime, walSize, walMtime int64
			if hasState {
				primaryPath = statePath
				size = stateSize
				mtime = stateMtime
				if hasWire {
					walSize = wireSize
					walMtime = wireMtime
				}
			} else {
				primaryPath = wirePath
				size = wireSize
				mtime = wireMtime
			}

			if !env.MarkRealPath(primaryPath) {
				continue
			}
			out = append(out, convstore.DiscoveredSource{
				SourceKey:  convstore.MakeSourceKey("kimi", r.profile, primaryPath),
				Agent:      "kimi",
				Profile:    r.profile,
				Path:       primaryPath,
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

func (a *kimiAdapter) ParseMetadata(ctx context.Context, src convstore.DiscoveredSource, _ *convstore.SourceCursor) (*convstore.MetadataResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sessDir := filepath.Dir(src.Path)
	id := filepath.Base(sessDir)
	dirHash := filepath.Base(filepath.Dir(sessDir))

	var (
		cwd           string
		model         string
		explicitTitle string
		firstPrompt   string
		createdMs     int64
		updatedMs     int64
	)

	statePath := filepath.Join(sessDir, "state.json")
	if raw, err := os.ReadFile(statePath); err == nil {
		var st map[string]any
		if json.Unmarshal(raw, &st) == nil && st != nil {
			if v, _ := st["id"].(string); v != "" {
				id = v
			} else if v, _ := st["session_id"].(string); v != "" {
				id = v
			}
			if v, _ := st["cwd"].(string); v != "" {
				cwd = v
			}
			if v, _ := st["model"].(string); v != "" {
				model = v
			}
			for _, k := range []string{"custom_title", "title"} {
				if v, _ := st[k].(string); v != "" {
					if ct := convstore.CleanTitle(v); ct != "" {
						explicitTitle = ct
						break
					}
				}
			}
			if ts := convstore.ParseTimestampMs(st["created_at"]); ts > 0 {
				createdMs = ts
			}
			if ts := convstore.ParseTimestampMs(st["updated_at"]); ts > 0 {
				updatedMs = ts
			}
		}
	}

	if cwd == "" {
		cwd = kimiResolveWorkDirHash(sessDir, dirHash)
	}

	wirePath := filepath.Join(sessDir, "wire.jsonl")
	if head, tail, err := convstore.ReadHeadTailLines(wirePath, convstore.DefaultHeadBytes, convstore.DefaultTailBytes, 60, 30); err == nil {
		scanWire := func(line string) {
			var rec map[string]any
			if json.Unmarshal([]byte(line), &rec) != nil || rec == nil {
				return
			}
			if ts := kimiWireTimestamp(rec); ts > 0 {
				if createdMs == 0 || ts < createdMs {
					createdMs = ts
				}
				if ts > updatedMs {
					updatedMs = ts
				}
			}
			if firstPrompt == "" {
				if msg, ok := rec["message"].(map[string]any); ok && msg != nil {
					if msg["type"] == "TurnBegin" {
						if p, _ := msg["payload"].(map[string]any); p != nil {
							raw := convstore.ExtractPlainText(p["user_input"])
							if ct := convstore.CleanTitle(raw); ct != "" {
								firstPrompt = ct
							}
						}
					}
				}
			}
		}
		for _, line := range head {
			scanWire(line)
		}
		for _, line := range tail {
			scanWire(line)
		}
	}

	if src.WALMtimeNs/1e6 > updatedMs {
		updatedMs = src.WALMtimeNs / 1e6
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
				Agent:        "kimi",
				ID:           id,
				App:          convstore.DisplayApp("kimi"),
				Profile:      src.Profile,
				Title:        title,
				Preview:      preview,
				CWD:          cwd,
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

func (a *kimiAdapter) ParseMessages(ctx context.Context, src convstore.DiscoveredSource, prev *convstore.SourceCursor) (*convstore.MessagesResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sessDir := filepath.Dir(src.Path)
	wirePath := filepath.Join(sessDir, "wire.jsonl")
	if _, _, ok := convstore.StatFile(wirePath); !ok {
		return nil, nil
	}
	convID := kimiResolveSessionID(sessDir)

	var prevOffset int64
	var prevHash string
	if prev != nil {
		prevOffset = prev.MsgByteOffset
		prevHash = prev.MsgPrefixHash
	}

	var msgs []convstore.Message
	var assistantBuf strings.Builder
	var assistantTs int64

	flushAssistant := func() {
		if assistantBuf.Len() == 0 {
			return
		}
		cleaned, truncated := convstore.CleanMessageText(assistantBuf.String())
		assistantBuf.Reset()
		ts := assistantTs
		assistantTs = 0
		if cleaned == "" {
			return
		}
		msgs = append(msgs, convstore.Message{
			Seq:       len(msgs),
			Role:      "assistant",
			Text:      cleaned,
			TsMs:      ts,
			Truncated: truncated,
		})
	}

	newOffset, newHash, appended, err := convstore.ReadIncrementalJSONL(wirePath, prevOffset, prevHash, 0, func(line []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil || rec == nil {
			return nil
		}
		ts := kimiWireTimestamp(rec)
		if msg, ok := rec["message"].(map[string]any); ok && msg != nil {
			mtype, _ := msg["type"].(string)
			payload, _ := msg["payload"].(map[string]any)
			switch mtype {
			case "TurnBegin":
				flushAssistant()
				if payload != nil {
					raw := convstore.ExtractPlainText(payload["user_input"])
					cleaned, truncated := convstore.CleanMessageText(raw)
					if cleaned != "" {
						msgs = append(msgs, convstore.Message{
							Seq:       len(msgs),
							Role:      "user",
							Text:      cleaned,
							TsMs:      ts,
							Truncated: truncated,
						})
					}
				}
				return nil
			case "ContentPart":
				if payload != nil {
					if pt, _ := payload["type"].(string); pt == "think" || pt == "thinking" || pt == "reasoning" {
						return nil
					}
					part := ""
					if s, ok := payload["text"].(string); ok {
						part = s
					} else if s, ok := payload["content"].(string); ok {
						part = s
					} else {
						part = convstore.ExtractPlainText(payload)
					}
					if part != "" {
						if assistantTs == 0 {
							assistantTs = ts
						}
						assistantBuf.WriteString(part)
					}
				}
				return nil
			case "AssistantMessage":
				flushAssistant()
				if payload != nil {
					raw := convstore.ExtractPlainText(payload)
					cleaned, truncated := convstore.CleanMessageText(raw)
					if cleaned != "" {
						msgs = append(msgs, convstore.Message{
							Seq:       len(msgs),
							Role:      "assistant",
							Text:      cleaned,
							TsMs:      ts,
							Truncated: truncated,
						})
					}
				}
				return nil
			case "TurnEnd":
				hadBuf := assistantBuf.Len() > 0
				flushAssistant()
				if !hadBuf && payload != nil {
					for _, k := range []string{"text", "content", "output", "assistant_output"} {
						if raw, ok := payload[k]; ok {
							cleaned, truncated := convstore.CleanMessageText(convstore.ExtractPlainText(raw))
							if cleaned != "" {
								msgs = append(msgs, convstore.Message{
									Seq:       len(msgs),
									Role:      "assistant",
									Text:      cleaned,
									TsMs:      ts,
									Truncated: truncated,
								})
								break
							}
						}
					}
				}
				return nil
			}
		}

		// Generic role fallback.
		role, _ := rec["role"].(string)
		role = strings.ToLower(strings.TrimSpace(role))
		if role == "user" || role == "assistant" {
			flushAssistant()
			raw := convstore.ExtractPlainText(rec["content"])
			if raw == "" {
				raw = convstore.ExtractPlainText(rec["text"])
			}
			cleaned, truncated := convstore.CleanMessageText(raw)
			if cleaned != "" {
				msgs = append(msgs, convstore.Message{
					Seq:       len(msgs),
					Role:      role,
					Text:      cleaned,
					TsMs:      ts,
					Truncated: truncated,
				})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	flushAssistant()

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

func kimiResolveSessionID(sessDir string) string {
	if raw, err := os.ReadFile(filepath.Join(sessDir, "state.json")); err == nil {
		var st map[string]any
		if json.Unmarshal(raw, &st) == nil && st != nil {
			if v, _ := st["id"].(string); v != "" {
				return v
			}
			if v, _ := st["session_id"].(string); v != "" {
				return v
			}
		}
	}
	return filepath.Base(sessDir)
}

func kimiResolveWorkDirHash(sessDir, dirHash string) string {
	if dirHash == "" {
		return ""
	}
	// sessDir = <base>/sessions/<dirHash>/<sessionID>
	sessionsDir := filepath.Dir(filepath.Dir(sessDir))
	baseDir := filepath.Dir(sessionsDir)
	parentDir := filepath.Dir(baseDir)

	candidates := []string{
		filepath.Join(baseDir, "kimi.json"),
		filepath.Join(parentDir, ".kimi", "kimi.json"),
		filepath.Join(parentDir, ".kimi-code", "kimi.json"),
	}
	seen := make(map[string]bool)
	for _, cfgPath := range candidates {
		clean := filepath.Clean(cfgPath)
		if seen[clean] {
			continue
		}
		seen[clean] = true
		if cwd := kimiMatchWorkDirInConfig(clean, dirHash); cwd != "" {
			return cwd
		}
	}
	return ""
}

func kimiMatchWorkDirInConfig(cfgPath, dirHash string) string {
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return ""
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil || cfg == nil {
		return ""
	}
	workDirs, _ := cfg["work_dirs"].([]any)
	for _, item := range workDirs {
		var p string
		switch v := item.(type) {
		case string:
			p = v
		case map[string]any:
			for _, k := range []string{"path", "dir", "cwd"} {
				if s, _ := v[k].(string); s != "" {
					p = s
					break
				}
			}
		}
		if p == "" {
			continue
		}
		if kimiMD5Hex(p) == dirHash {
			return p
		}
		trimmed := strings.TrimRight(p, "/")
		if trimmed != "" && trimmed != p && kimiMD5Hex(trimmed) == dirHash {
			return p
		}
	}
	return ""
}

func kimiMD5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func kimiWireTimestamp(rec map[string]any) int64 {
	for _, k := range []string{"timestamp", "ts", "created_at"} {
		if ms := convstore.ParseTimestampMs(rec[k]); ms > 0 {
			return ms
		}
	}
	return 0
}
