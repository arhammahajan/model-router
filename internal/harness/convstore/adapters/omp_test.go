package adapters

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arhammahajan/model-router/internal/harness/convstore"
	"github.com/arhammahajan/model-router/internal/harness/harnessenv"
)

func TestOmpAdapterDiscoverMetadataMessages(t *testing.T) {
	home := t.TempDir()
	env := convstore.NewScanEnv(harnessenv.Paths{Home: home})
	adapter := NewOmpAdapter()
	if adapter.Agent() != "omp" {
		t.Fatalf("Agent() = %q, want omp", adapter.Agent())
	}

	sessDir := filepath.Join(home, ".omp", "agent", "sessions", "proj")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sessPath := filepath.Join(sessDir, "omp-session.jsonl")
	content := strings.Join([]string{
		`{"type":"session","id":"omp-42","cwd":"/work/omp-repo","timestamp":"2026-11-20T09:00:00Z","title":"Omp Title"}`,
		`{"type":"message","timestamp":"2026-11-20T09:01:00Z","message":{"role":"user","content":"Explain the scheduler"}}`,
		`{"type":"message","timestamp":"2026-11-20T09:02:00Z","message":{"role":"assistant","content":"The scheduler runs in two stages."}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(sessPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	// Profile shadow home.
	profDir := filepath.Join(home, ".model-router", "profiles", "omp", "alt", "home", "sessions")
	if err := os.MkdirAll(profDir, 0o755); err != nil {
		t.Fatal(err)
	}
	profPath := filepath.Join(profDir, "omp-alt.jsonl")
	if err := os.WriteFile(profPath, []byte(`{"type":"session","id":"omp-alt","cwd":"/work/alt"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	sources, err := adapter.Discover(ctx, env)
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	if len(sources) != 2 {
		t.Fatalf("Discover returned %d sources, want 2", len(sources))
	}

	var mainSrc convstore.DiscoveredSource
	for _, s := range sources {
		if s.Path == sessPath {
			mainSrc = s
		}
	}
	meta, err := adapter.ParseMetadata(ctx, mainSrc, nil)
	if err != nil {
		t.Fatalf("ParseMetadata error: %v", err)
	}
	if len(meta.Conversations) != 1 {
		t.Fatalf("got %d convs, want 1", len(meta.Conversations))
	}
	c := meta.Conversations[0]
	if c.Agent != "omp" || c.ID != "omp-42" || c.CWD != "/work/omp-repo" {
		t.Fatalf("conv = %+v", c)
	}
	if c.Title != "Omp Title" {
		t.Fatalf("Title = %q, want Omp Title", c.Title)
	}
	if c.ResumeTarget != "omp-42" {
		t.Fatalf("ResumeTarget = %q, want omp-42", c.ResumeTarget)
	}

	msgRes, err := adapter.ParseMessages(ctx, mainSrc, nil)
	if err != nil {
		t.Fatalf("ParseMessages error: %v", err)
	}
	msgs := msgRes.Messages["omp-42"]
	if len(msgs) != 2 || msgs[0].Role != "user" || msgs[1].Role != "assistant" {
		t.Fatalf("msgs = %+v", msgs)
	}
}
