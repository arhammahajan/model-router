package adapters

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/arhammahajan/model-router/internal/harness/convstore"
)

type claudeAdapter struct{}

// NewClaudeAdapter returns the native conversation store adapter for Claude Code.
func NewClaudeAdapter() convstore.Adapter {
	return &claudeAdapter{}
}

func (a *claudeAdapter) Agent() string { return "claude" }

func (a *claudeAdapter) Discover(ctx context.Context, env *convstore.ScanEnv) ([]convstore.DiscoveredSource, error) {
	if env == nil {
		return nil, nil
	}

	type rootEntry struct {
		projectsDir string
		profile     string
	}
	var roots []rootEntry

	defaultConfigDir := filepath.Join(env.Paths.Home, ".claude")
	if cfgDir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); cfgDir != "" && sqliteAdapterIsOSHome(env.Paths.Home) {
		defaultConfigDir = cfgDir
	}
	roots = append(roots, rootEntry{
		projectsDir: filepath.Join(defaultConfigDir, "projects"),
		profile:     "",
	})

	for _, ph := range env.ProfileShadowHomes("claude") {
		roots = append(roots,
			rootEntry{projectsDir: filepath.Join(ph.ShadowHome, "projects"), profile: ph.Profile},
			rootEntry{projectsDir: filepath.Join(ph.ShadowHome, ".claude", "projects"), profile: ph.Profile},
		)
	}

	var sources []convstore.DiscoveredSource
	for _, root := range roots {
		if ctx.Err() != nil {
			return sources, ctx.Err()
		}
		projEntries, err := os.ReadDir(root.projectsDir)
		if err != nil {
			continue
		}
		for _, proj := range projEntries {
			if ctx.Err() != nil {
				return sources, ctx.Err()
			}
			if !proj.IsDir() || proj.Name() == "subagents" {
				continue
			}
			projDir := filepath.Join(root.projectsDir, proj.Name())
			files, err := os.ReadDir(projDir)
			if err != nil {
				continue
			}
			for _, f := range files {
				if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
					continue
				}
				fullPath := filepath.Join(projDir, f.Name())
				if !env.MarkRealPath(fullPath) {
					continue
				}
				size, mtime, ok := convstore.StatFile(fullPath)
				if !ok {
					continue
				}
				sources = append(sources, convstore.DiscoveredSource{
					SourceKey: convstore.MakeSourceKey("claude", root.profile, fullPath),
					Agent:     "claude",
					Profile:   root.profile,
					Path:      filepath.Clean(fullPath),
					Kind:      convstore.SourceFile,
					SizeBytes: size,
					MtimeNs:   mtime,
				})
			}
		}
	}
	return sources, nil
}

func (a *claudeAdapter) ParseMetadata(ctx context.Context, src convstore.DiscoveredSource, _ *convstore.SourceCursor) (*convstore.MetadataResult, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	head, tail, err := convstore.ReadHeadTailLines(src.Path, convstore.DefaultHeadBytes, convstore.DefaultTailBytes, 60, 60)
	if err != nil {
		return nil, err
	}
	if len(head) == 0 && len(tail) == 0 {
		return &convstore.MetadataResult{}, nil
	}

	var (
		sessionID       string
		cwd             string
		branch          string
		model           string
		customTitle     string
		aiTitle         string
		summary         string
		firstPrompt     string
		lastPrompt      string
		createdAtMs     int64
		updatedAtMs     int64
		msgCount        int
		sawSidechain    bool
		sawNonSidechain bool
	)

	processLine := func(line string) {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil || rec == nil {
			return
		}
		if sidechain, ok := rec["isSidechain"].(bool); ok {
			if sidechain {
				sawSidechain = true
				return
			}
			sawNonSidechain = true
		}

		if sessionID == "" {
			if s, _ := rec["sessionId"].(string); s != "" {
				sessionID = s
			} else if s, _ := rec["session_id"].(string); s != "" {
				sessionID = s
			}
		}
		if cwd == "" {
			if s, _ := rec["cwd"].(string); s != "" {
				cwd = s
			}
		}
		if s, _ := rec["gitBranch"].(string); s != "" {
			branch = s
		}

		for _, tk := range []string{"timestamp", "createdAt", "ts"} {
			if ts := sqliteAdapterTimestampMs(rec[tk]); ts > 0 {
				if createdAtMs == 0 || ts < createdAtMs {
					createdAtMs = ts
				}
				if ts > updatedAtMs {
					updatedAtMs = ts
				}
				break
			}
		}

		typ, _ := rec["type"].(string)
		switch typ {
		case "custom-title":
			if s, _ := rec["customTitle"].(string); convstore.CleanTitle(s) != "" {
				customTitle = convstore.CleanTitle(s)
			} else if s, _ := rec["title"].(string); convstore.CleanTitle(s) != "" {
				customTitle = convstore.CleanTitle(s)
			}
		case "ai-title":
			if s, _ := rec["aiTitle"].(string); convstore.CleanTitle(s) != "" {
				aiTitle = convstore.CleanTitle(s)
			} else if s, _ := rec["title"].(string); convstore.CleanTitle(s) != "" {
				aiTitle = convstore.CleanTitle(s)
			}
		case "summary":
			if s, _ := rec["summary"].(string); convstore.CleanTitle(s) != "" {
				summary = convstore.CleanTitle(s)
			}
		case "last-prompt":
			if s, _ := rec["lastPrompt"].(string); convstore.CleanTitle(s) != "" {
				lastPrompt = convstore.CleanTitle(s)
			}
		case "user":
			if isMeta, _ := rec["isMeta"].(bool); isMeta {
				return
			}
			sawNonSidechain = true
			msgCount++
			if firstPrompt == "" {
				raw := claudeExtractRecordText(rec)
				if t := convstore.CleanTitle(raw); t != "" {
					firstPrompt = t
				}
			}
		case "assistant":
			if isMeta, _ := rec["isMeta"].(bool); isMeta {
				return
			}
			sawNonSidechain = true
			msgCount++
			if msg, ok := rec["message"].(map[string]any); ok {
				if m, _ := msg["model"].(string); m != "" {
					model = m
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

	if sawSidechain && !sawNonSidechain {
		return &convstore.MetadataResult{}, nil
	}

	if sessionID == "" {
		sessionID = strings.TrimSuffix(filepath.Base(src.Path), filepath.Ext(src.Path))
	}
	if sessionID == "" {
		return &convstore.MetadataResult{}, nil
	}

	title := sqliteAdapterFirstNonEmpty(customTitle, aiTitle, summary, firstPrompt, lastPrompt)
	preview := convstore.CleanPreview(sqliteAdapterFirstNonEmpty(firstPrompt, summary, lastPrompt, title))

	if updatedAtMs == 0 {
		updatedAtMs = createdAtMs
	}
	if updatedAtMs == 0 && src.MtimeNs > 0 {
		updatedAtMs = src.MtimeNs / 1e6
	}
	if updatedAtMs == 0 {
		if _, mtime, ok := convstore.StatFile(src.Path); ok {
			updatedAtMs = mtime / 1e6
		}
	}

	conv := convstore.Conversation{
		Agent:        "claude",
		ID:           sessionID,
		App:          convstore.DisplayApp("claude"),
		Profile:      src.Profile,
		Title:        title,
		Preview:      preview,
		CWD:          cwd,
		Branch:       branch,
		Model:        model,
		CreatedAtMs:  createdAtMs,
		UpdatedAtMs:  updatedAtMs,
		MessageCount: msgCount,
		SourceKey:    src.SourceKey,
		SourcePath:   src.Path,
	}
	return &convstore.MetadataResult{Conversations: []convstore.Conversation{conv}}, nil
}

func (a *claudeAdapter) ParseMessages(ctx context.Context, src convstore.DiscoveredSource, prev *convstore.SourceCursor) (*convstore.MessagesResult, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	var prevOff int64
	var prevHash string
	if prev != nil {
		prevOff = prev.MsgByteOffset
		prevHash = prev.MsgPrefixHash
	}

	var (
		sessionID       string
		msgs            []convstore.Message
		sawSidechain    bool
		sawNonSidechain bool
	)

	newOff, newHash, appended, err := convstore.ReadIncrementalJSONL(
		src.Path,
		prevOff,
		prevHash,
		convstore.DefaultMaxJSONLLineBytes,
		func(line []byte) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var rec map[string]any
			if err := json.Unmarshal(line, &rec); err != nil || rec == nil {
				return nil
			}
			if sidechain, ok := rec["isSidechain"].(bool); ok {
				if sidechain {
					sawSidechain = true
					return nil
				}
				sawNonSidechain = true
			}
			if isMeta, _ := rec["isMeta"].(bool); isMeta {
				return nil
			}
			if sessionID == "" {
				if s, _ := rec["sessionId"].(string); s != "" {
					sessionID = s
				} else if s, _ := rec["session_id"].(string); s != "" {
					sessionID = s
				}
			}

			typ, _ := rec["type"].(string)
			if typ != "user" && typ != "assistant" {
				return nil
			}
			sawNonSidechain = true

			raw := claudeExtractRecordText(rec)
			cleaned, truncated := convstore.CleanMessageText(raw)
			if cleaned == "" {
				return nil
			}
			tsMs := sqliteAdapterTimestampMs(rec["timestamp"])
			msgs = append(msgs, convstore.Message{
				Seq:       len(msgs),
				Role:      typ,
				Text:      cleaned,
				TsMs:      tsMs,
				Truncated: truncated,
			})
			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	out := &convstore.MessagesResult{
		Messages:   make(map[string][]convstore.Message),
		Append:     appended,
		ByteOffset: newOff,
		PrefixHash: newHash,
	}
	if sawSidechain && !sawNonSidechain {
		return out, nil
	}
	if sessionID == "" && prev != nil && len(prev.ExistingUpdatedMs) == 1 {
		for id := range prev.ExistingUpdatedMs {
			sessionID = id
		}
	}
	if sessionID == "" {
		sessionID = strings.TrimSuffix(filepath.Base(src.Path), filepath.Ext(src.Path))
	}
	if sessionID != "" && len(msgs) > 0 {
		out.Messages[sessionID] = msgs
	}
	return out, nil
}

func claudeExtractRecordText(rec map[string]any) string {
	if msg, ok := rec["message"].(map[string]any); ok {
		if raw, exists := msg["content"]; exists {
			return convstore.ExtractPlainText(raw)
		}
	}
	if raw, exists := rec["content"]; exists {
		return convstore.ExtractPlainText(raw)
	}
	return ""
}
