package adapters

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/arhammahajan/model-router/internal/harness/convstore"
)

type piAdapter struct{}

// NewPiAdapter returns the convstore adapter for Pi (~/.pi/agent/sessions/**/*.jsonl).
func NewPiAdapter() convstore.Adapter {
	return &piAdapter{}
}

func (a *piAdapter) Agent() string { return "pi" }

func (a *piAdapter) Discover(ctx context.Context, env *convstore.ScanEnv) ([]convstore.DiscoveredSource, error) {
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
			dir: filepath.Join(env.Paths.Home, ".pi", "agent", "sessions"),
		})
	}
	for _, ph := range env.ProfileShadowHomes("pi") {
		roots = append(roots,
			rootSpec{dir: filepath.Join(ph.ShadowHome, "sessions"), profile: ph.Profile},
			rootSpec{dir: filepath.Join(ph.ShadowHome, ".pi", "agent", "sessions"), profile: ph.Profile},
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
		_ = filepath.WalkDir(r.dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
				return nil
			}
			if !env.MarkRealPath(path) {
				return nil
			}
			size, mtime, ok := convstore.StatFile(path)
			if !ok {
				return nil
			}
			out = append(out, convstore.DiscoveredSource{
				SourceKey: convstore.MakeSourceKey("pi", r.profile, path),
				Agent:     "pi",
				Profile:   r.profile,
				Path:      path,
				Kind:      convstore.SourceFile,
				SizeBytes: size,
				MtimeNs:   mtime,
			})
			return nil
		})
	}
	return out, nil
}

func (a *piAdapter) ParseMetadata(ctx context.Context, src convstore.DiscoveredSource, _ *convstore.SourceCursor) (*convstore.MetadataResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	conv, ok, err := piParseSessionMetadata(src, "pi", true)
	if err != nil {
		return nil, err
	}
	if !ok {
		return &convstore.MetadataResult{}, nil
	}
	return &convstore.MetadataResult{Conversations: []convstore.Conversation{conv}}, nil
}

func (a *piAdapter) ParseMessages(ctx context.Context, src convstore.DiscoveredSource, prev *convstore.SourceCursor) (*convstore.MessagesResult, error) {
	return piParseSessionMessages(ctx, src, prev)
}

func piParseSessionMetadata(src convstore.DiscoveredSource, agent string, resumeByPath bool) (convstore.Conversation, bool, error) {
	head, tail, err := convstore.ReadHeadTailLines(src.Path, convstore.DefaultHeadBytes, convstore.DefaultTailBytes, 100, 50)
	if err != nil {
		return convstore.Conversation{}, false, err
	}
	if len(head) == 0 && len(tail) == 0 {
		return convstore.Conversation{}, false, nil
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

		if ts := piExtractRecordTimestamp(rec); ts > 0 {
			if createdMs == 0 || ts < createdMs {
				createdMs = ts
			}
			if ts > updatedMs {
				updatedMs = ts
			}
		}

		typ, _ := rec["type"].(string)
		switch typ {
		case "session":
			if v, _ := rec["id"].(string); v != "" && id == "" {
				id = v
			}
			if v, _ := rec["cwd"].(string); v != "" && cwd == "" {
				cwd = v
			}
			if v, _ := rec["gitBranch"].(string); v != "" && branch == "" {
				branch = v
			} else if v, _ := rec["branch"].(string); v != "" && branch == "" {
				branch = v
			}
			if v, _ := rec["model"].(string); v != "" && model == "" {
				model = v
			}
			if v, _ := rec["title"].(string); v != "" {
				if ct := convstore.CleanTitle(v); ct != "" {
					explicitTitle = ct
				}
			}
		case "title":
			if v, _ := rec["title"].(string); v != "" {
				if ct := convstore.CleanTitle(v); ct != "" {
					explicitTitle = ct
				}
			}
		case "session_info":
			if v, _ := rec["name"].(string); v != "" {
				if ct := convstore.CleanTitle(v); ct != "" {
					explicitTitle = ct
				}
			}
		case "model_change":
			if v, _ := rec["modelId"].(string); v != "" {
				model = v
			} else if v, _ := rec["model"].(string); v != "" {
				model = v
			}
		case "message":
			msg, _ := rec["message"].(map[string]any)
			if msg == nil {
				return
			}
			if v, _ := msg["model"].(string); v != "" {
				model = v
			}
			if ts := piExtractRecordTimestamp(msg); ts > 0 {
				if createdMs == 0 || ts < createdMs {
					createdMs = ts
				}
				if ts > updatedMs {
					updatedMs = ts
				}
			}
			role, _ := msg["role"].(string)
			role = strings.ToLower(strings.TrimSpace(role))
			if role == "user" || role == "assistant" {
				msgCount++
			}
			if role == "user" && firstPrompt == "" {
				raw := convstore.ExtractPlainText(msg["content"])
				if ct := convstore.CleanTitle(raw); ct != "" {
					firstPrompt = ct
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
		return convstore.Conversation{}, false, nil
	}
	if id == "" {
		id = piFileStem(src.Path)
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
	resumeTarget := id
	if resumeByPath {
		resumeTarget = src.Path
	}

	return convstore.Conversation{
		Agent:        agent,
		ID:           id,
		App:          convstore.DisplayApp(agent),
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
		ResumeTarget: resumeTarget,
	}, true, nil
}

func piParseSessionMessages(ctx context.Context, src convstore.DiscoveredSource, prev *convstore.SourceCursor) (*convstore.MessagesResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	convID := piResolveSessionID(src.Path)

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
		typ, _ := rec["type"].(string)
		if typ == "session" {
			if v, _ := rec["id"].(string); v != "" {
				convID = v
			}
			return nil
		}
		if typ != "message" {
			return nil
		}
		msg, _ := rec["message"].(map[string]any)
		if msg == nil {
			return nil
		}
		role, _ := msg["role"].(string)
		role = strings.ToLower(strings.TrimSpace(role))
		if role != "user" && role != "assistant" {
			return nil
		}
		raw := convstore.ExtractPlainText(msg["content"])
		cleaned, truncated := convstore.CleanMessageText(raw)
		if cleaned == "" {
			return nil
		}
		ts := piExtractRecordTimestamp(rec)
		if ts == 0 {
			ts = piExtractRecordTimestamp(msg)
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
		convID = piFileStem(src.Path)
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

func piResolveSessionID(path string) string {
	head, _, err := convstore.ReadHeadTailLines(path, 8<<10, 0, 10, 0)
	if err == nil {
		for _, line := range head {
			var rec map[string]any
			if json.Unmarshal([]byte(line), &rec) != nil || rec == nil {
				continue
			}
			if rec["type"] == "session" {
				if v, _ := rec["id"].(string); v != "" {
					return v
				}
			}
		}
	}
	return piFileStem(path)
}

func piExtractRecordTimestamp(rec map[string]any) int64 {
	for _, k := range []string{"timestamp", "updated_at", "created_at", "ts"} {
		if v, ok := rec[k]; ok {
			if ms := convstore.ParseTimestampMs(v); ms > 0 {
				return ms
			}
		}
	}
	return 0
}

func piFileStem(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}
