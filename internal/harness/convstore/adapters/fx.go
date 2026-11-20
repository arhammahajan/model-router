package adapters

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/arhammahajan/model-router/internal/harness/convstore"
)

const fxCommandTimeout = 5 * time.Second

// FxAdapter implements convstore.Adapter for the fx CLI (`fx sessions --json`).
// LookPathFn and RunFn are injectable so tests never shell out to a real binary.
type FxAdapter struct {
	LookPathFn func(string) (string, error)
	RunFn      func(ctx context.Context, bin string, args ...string) ([]byte, error)
}

// NewFxAdapter returns the convstore adapter for fx.
func NewFxAdapter() convstore.Adapter {
	return &FxAdapter{}
}

func (a *FxAdapter) Agent() string { return "fx" }

func (a *FxAdapter) resolveBinary(home string) string {
	lookPath := a.LookPathFn
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	if bin, err := lookPath("fx"); err == nil && bin != "" {
		return bin
	}
	if home != "" {
		candidate := filepath.Join(home, ".local", "libexec", "fx", "fx-router")
		if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
			return candidate
		}
	}
	return ""
}

func (a *FxAdapter) runCommand(ctx context.Context, bin string, args ...string) ([]byte, error) {
	if a.RunFn != nil {
		return a.RunFn(ctx, bin, args...)
	}
	return exec.CommandContext(ctx, bin, args...).Output()
}

func (a *FxAdapter) Discover(ctx context.Context, env *convstore.ScanEnv) ([]convstore.DiscoveredSource, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	home := ""
	if env != nil {
		home = env.Paths.Home
	}
	bin := a.resolveBinary(home)
	if bin == "" {
		return nil, nil
	}
	return []convstore.DiscoveredSource{
		{
			SourceKey: "fx::cli",
			Agent:     "fx",
			Path:      bin,
			Kind:      convstore.SourceCLI,
		},
	}, nil
}

func (a *FxAdapter) ParseMetadata(ctx context.Context, src convstore.DiscoveredSource, _ *convstore.SourceCursor) (*convstore.MetadataResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bin := src.Path
	if bin == "" {
		bin = a.resolveBinary("")
	}
	if bin == "" {
		return &convstore.MetadataResult{}, nil
	}

	runCtx, cancel := context.WithTimeout(ctx, fxCommandTimeout)
	defer cancel()

	out, err := a.runCommand(runCtx, bin, "sessions", "--json")
	if err != nil {
		return nil, err
	}

	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return &convstore.MetadataResult{}, nil
	}

	var top any
	if err := json.Unmarshal([]byte(trimmed), &top); err != nil {
		return nil, err
	}

	var rawSessions []any
	switch v := top.(type) {
	case map[string]any:
		rawSessions, _ = v["sessions"].([]any)
	case []any:
		rawSessions = v
	}

	convs := make([]convstore.Conversation, 0, len(rawSessions))
	for _, item := range rawSessions {
		rec, _ := item.(map[string]any)
		if rec == nil {
			continue
		}
		id, _ := rec["id"].(string)
		if id == "" {
			id, _ = rec["session_id"].(string)
		}
		if id == "" {
			continue
		}
		cwd, _ := rec["cwd"].(string)
		if cwd == "" {
			cwd, _ = rec["workdir"].(string)
		}
		branch, _ := rec["branch"].(string)
		if branch == "" {
			branch, _ = rec["git_branch"].(string)
		}
		model, _ := rec["model"].(string)

		var title string
		for _, k := range []string{"title", "name", "summary"} {
			if s, _ := rec[k].(string); s != "" {
				if ct := convstore.CleanTitle(s); ct != "" {
					title = ct
					break
				}
			}
		}
		var preview string
		if s, _ := rec["preview"].(string); s != "" {
			preview = convstore.CleanPreview(s)
		}
		if preview == "" {
			preview = convstore.CleanPreview(title)
		}

		createdMs := convstore.ParseTimestampMs(rec["created_at"])
		if createdMs == 0 {
			createdMs = convstore.ParseTimestampMs(rec["createdAt"])
		}
		updatedMs := convstore.ParseTimestampMs(rec["updated_at"])
		if updatedMs == 0 {
			updatedMs = convstore.ParseTimestampMs(rec["updatedAt"])
		}
		if updatedMs == 0 {
			updatedMs = createdMs
		}

		var msgCount int
		if f, ok := rec["message_count"].(float64); ok && f > 0 {
			msgCount = int(f)
		}

		convs = append(convs, convstore.Conversation{
			Agent:        "fx",
			ID:           id,
			App:          convstore.DisplayApp("fx"),
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
		})
	}

	return &convstore.MetadataResult{Conversations: convs}, nil
}

func (a *FxAdapter) ParseMessages(_ context.Context, _ convstore.DiscoveredSource, _ *convstore.SourceCursor) (*convstore.MessagesResult, error) {
	return nil, nil
}
