package adapters

import (
	"context"

	"github.com/arhammahajan/model-router/internal/harness/convstore"
)

type kiloAdapter struct{}

// NewKiloAdapter returns the native conversation store adapter for Kilo.
func NewKiloAdapter() convstore.Adapter {
	return &kiloAdapter{}
}

func (a *kiloAdapter) Agent() string { return "kilo" }

func (a *kiloAdapter) Discover(ctx context.Context, env *convstore.ScanEnv) ([]convstore.DiscoveredSource, error) {
	return opencodeKiloDiscover(ctx, env, "kilo", "kilo.db")
}

func (a *kiloAdapter) ParseMetadata(ctx context.Context, src convstore.DiscoveredSource, _ *convstore.SourceCursor) (*convstore.MetadataResult, error) {
	return opencodeKiloParseMetadata(ctx, src, "kilo", []string{"session", "session_v2"})
}

func (a *kiloAdapter) ParseMessages(ctx context.Context, src convstore.DiscoveredSource, prev *convstore.SourceCursor) (*convstore.MessagesResult, error) {
	return opencodeKiloParseMessages(ctx, src, prev, []string{"session", "session_v2"})
}
