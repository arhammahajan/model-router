package adapters

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/arhammahajan/model-router/internal/harness/convstore"
	"github.com/arhammahajan/model-router/internal/harness/harnessenv"
)

func TestFxAdapterDiscoverMetadataMessages(t *testing.T) {
	home := t.TempDir()
	env := convstore.NewScanEnv(harnessenv.Paths{Home: home})
	ctx := context.Background()

	// 1. No binary on PATH and no libexec fallback -> empty Discover.
	missing := &FxAdapter{
		LookPathFn: func(string) (string, error) { return "", errors.New("not found") },
	}
	if missing.Agent() != "fx" {
		t.Fatalf("Agent() = %q, want fx", missing.Agent())
	}
	sources, err := missing.Discover(ctx, env)
	if err != nil || len(sources) != 0 {
		t.Fatalf("expected no sources when fx missing, got %+v, %v", sources, err)
	}

	// 2. Fallback to ~/.local/libexec/fx/fx-router when LookPath fails.
	libexec := filepath.Join(home, ".local", "libexec", "fx", "fx-router")
	if err := os.MkdirAll(filepath.Dir(libexec), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(libexec, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sources, err = missing.Discover(ctx, env)
	if err != nil {
		t.Fatalf("Discover fallback error: %v", err)
	}
	if len(sources) != 1 || sources[0].SourceKey != "fx::cli" || sources[0].Kind != convstore.SourceCLI || sources[0].Path != libexec {
		t.Fatalf("unexpected fallback sources: %+v", sources)
	}

	// 3. Injectable LookPathFn + RunFn (no real subprocess invoked).
	var gotBin string
	var gotArgs []string
	var hasDeadline bool
	adapter := &FxAdapter{
		LookPathFn: func(name string) (string, error) {
			if name != "fx" {
				t.Errorf("unexpected LookPath(%q)", name)
			}
			return "/fake/bin/fx", nil
		},
		RunFn: func(ctx context.Context, bin string, args ...string) ([]byte, error) {
			gotBin = bin
			gotArgs = append([]string(nil), args...)
			_, hasDeadline = ctx.Deadline()
			return []byte(`{
				"sessions": [
					{
						"id": "fx-1",
						"cwd": "/work/fx-proj",
						"title": "FX Routing Session",
						"model": "gpt-5",
						"branch": "main",
						"created_at": "2026-11-20T10:00:00Z",
						"updated_at": "2026-11-20T10:25:00Z",
						"message_count": 6
					},
					{
						"id": "fx-2",
						"cwd": "/work/fx-two",
						"summary": "Second FX Session",
						"created_at": 1763640000
					}
				]
			}`), nil
		},
	}

	sources, err = adapter.Discover(ctx, env)
	if err != nil || len(sources) != 1 || sources[0].SourceKey != "fx::cli" || sources[0].Path != "/fake/bin/fx" {
		t.Fatalf("Discover = %+v, %v", sources, err)
	}

	meta, err := adapter.ParseMetadata(ctx, sources[0], nil)
	if err != nil {
		t.Fatalf("ParseMetadata error: %v", err)
	}
	if gotBin != "/fake/bin/fx" || !reflect.DeepEqual(gotArgs, []string{"sessions", "--json"}) {
		t.Fatalf("command = %q %v", gotBin, gotArgs)
	}
	if !hasDeadline {
		t.Fatalf("expected 5s timeout deadline on RunFn context")
	}
	if len(meta.Conversations) != 2 {
		t.Fatalf("got %d conversations, want 2: %+v", len(meta.Conversations), meta.Conversations)
	}
	c1 := meta.Conversations[0]
	if c1.ID != "fx-1" || c1.CWD != "/work/fx-proj" || c1.Title != "FX Routing Session" || c1.Model != "gpt-5" || c1.Branch != "main" || c1.MessageCount != 6 || c1.ResumeTarget != "fx-1" {
		t.Fatalf("c1 = %+v", c1)
	}
	c2 := meta.Conversations[1]
	if c2.ID != "fx-2" || c2.Title != "Second FX Session" || c2.CreatedAtMs == 0 || c2.UpdatedAtMs != c2.CreatedAtMs {
		t.Fatalf("c2 = %+v", c2)
	}

	// 4. ParseMessages is metadata-only -> nil, nil.
	msgs, err := adapter.ParseMessages(ctx, sources[0], nil)
	if err != nil || msgs != nil {
		t.Fatalf("ParseMessages = %+v, %v; want nil, nil", msgs, err)
	}

	// 5. Constructor returns convstore.Adapter.
	if a := NewFxAdapter(); a.Agent() != "fx" {
		t.Fatalf("NewFxAdapter().Agent() = %q", a.Agent())
	}
}
