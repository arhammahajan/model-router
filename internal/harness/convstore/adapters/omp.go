package adapters

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/arhammahajan/model-router/internal/harness/convstore"
)

type ompAdapter struct{}

// NewOmpAdapter returns the convstore adapter for Omp (~/.omp/agent/sessions/**/*.jsonl).
func NewOmpAdapter() convstore.Adapter {
	return &ompAdapter{}
}

func (a *ompAdapter) Agent() string { return "omp" }

func (a *ompAdapter) Discover(ctx context.Context, env *convstore.ScanEnv) ([]convstore.DiscoveredSource, error) {
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
			dir: filepath.Join(env.Paths.Home, ".omp", "agent", "sessions"),
		})
	}
	for _, ph := range env.ProfileShadowHomes("omp") {
		roots = append(roots,
			rootSpec{dir: filepath.Join(ph.ShadowHome, "sessions"), profile: ph.Profile},
			rootSpec{dir: filepath.Join(ph.ShadowHome, ".omp", "agent", "sessions"), profile: ph.Profile},
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
				SourceKey: convstore.MakeSourceKey("omp", r.profile, path),
				Agent:     "omp",
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

func (a *ompAdapter) ParseMetadata(ctx context.Context, src convstore.DiscoveredSource, _ *convstore.SourceCursor) (*convstore.MetadataResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	conv, ok, err := piParseSessionMetadata(src, "omp", false)
	if err != nil {
		return nil, err
	}
	if !ok {
		return &convstore.MetadataResult{}, nil
	}
	return &convstore.MetadataResult{Conversations: []convstore.Conversation{conv}}, nil
}

func (a *ompAdapter) ParseMessages(ctx context.Context, src convstore.DiscoveredSource, prev *convstore.SourceCursor) (*convstore.MessagesResult, error) {
	return piParseSessionMessages(ctx, src, prev)
}
