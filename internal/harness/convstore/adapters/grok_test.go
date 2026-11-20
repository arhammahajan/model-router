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

func TestGrokAdapterDiscoverMetadataMessages(t *testing.T) {
	home := t.TempDir()
	env := convstore.NewScanEnv(harnessenv.Paths{Home: home})
	adapter := NewGrokAdapter()
	if adapter.Agent() != "grok" {
		t.Fatalf("Agent() = %q, want grok", adapter.Agent())
	}

	// Session 1: summary.json + sibling transcript.jsonl.
	s1Dir := filepath.Join(home, ".grok", "sessions", "proj1", "sess1")
	if err := os.MkdirAll(s1Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sum1 := filepath.Join(s1Dir, "summary.json")
	if err := os.WriteFile(sum1, []byte(`{
		"info": {
			"id": "grok-1",
			"cwd": "/work/grok-repo",
			"git_branch": "feat/grok",
			"model": "grok-4"
		},
		"session_summary": "Optimize HTTP router",
		"created_at": "2026-11-20T08:00:00Z",
		"last_active_at": "2026-11-20T08:45:00Z"
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t1Path := filepath.Join(s1Dir, "transcript.jsonl")
	t1Lines := strings.Join([]string{
		`{"role":"user","content":"Profile the HTTP router hot path","timestamp":"2026-11-20T08:01:00Z"}`,
		`{"role":"assistant","content":"Found allocation in header parsing.","timestamp":"2026-11-20T08:02:00Z"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(t1Path, []byte(t1Lines), 0o644); err != nil {
		t.Fatal(err)
	}

	// Session 2 in shadow profile: summary.json + messages.json.
	s2Dir := filepath.Join(home, ".model-router", "profiles", "grok", "pro", "home", "sessions", "proj2", "sess2")
	if err := os.MkdirAll(s2Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sum2 := filepath.Join(s2Dir, "summary.json")
	if err := os.WriteFile(sum2, []byte(`{"info":{"id":"grok-2","cwd":"/work/pro"},"title":"Grok Pro Chat","updated_at":"2026-11-20T09:00:00Z"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s2Dir, "messages.json"),
		[]byte(`[{"role":"user","content":"Hello Grok"},{"role":"assistant","content":"Hi!"}]`), 0o644); err != nil {
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

	var src1, src2 convstore.DiscoveredSource
	for _, s := range sources {
		if s.Path == sum1 {
			src1 = s
		} else {
			src2 = s
		}
	}
	if src1.WALSize == 0 || src1.WALMtimeNs == 0 {
		t.Fatalf("expected WAL stats for transcript.jsonl, got %+v", src1)
	}
	if src2.WALSize == 0 || src2.Profile != "pro" {
		t.Fatalf("src2 = %+v", src2)
	}

	m1, err := adapter.ParseMetadata(ctx, src1, nil)
	if err != nil {
		t.Fatalf("ParseMetadata(1) error: %v", err)
	}
	c1 := m1.Conversations[0]
	if c1.ID != "grok-1" || c1.CWD != "/work/grok-repo" || c1.Branch != "feat/grok" || c1.Model != "grok-4" || c1.Title != "Optimize HTTP router" {
		t.Fatalf("c1 = %+v", c1)
	}
	if c1.Preview != "Profile the HTTP router hot path" {
		t.Fatalf("c1.Preview = %q", c1.Preview)
	}

	msgRes1, err := adapter.ParseMessages(ctx, src1, nil)
	if err != nil {
		t.Fatalf("ParseMessages(1) error: %v", err)
	}
	if len(msgRes1.Messages["grok-1"]) != 2 {
		t.Fatalf("msgs1 = %+v", msgRes1.Messages)
	}

	// Append line and test incremental read.
	f, err := os.OpenFile(t1Path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"role":"user","content":"Benchmark after fix","timestamp":"2026-11-20T08:10:00Z"}` + "\n")
	_ = f.Close()

	incRes, err := adapter.ParseMessages(ctx, src1, &convstore.SourceCursor{
		MsgByteOffset: msgRes1.ByteOffset,
		MsgPrefixHash: msgRes1.PrefixHash,
	})
	if err != nil {
		t.Fatalf("incremental ParseMessages error: %v", err)
	}
	if !incRes.Append || len(incRes.Messages["grok-1"]) != 1 || incRes.Messages["grok-1"][0].Text != "Benchmark after fix" {
		t.Fatalf("incRes = %+v", incRes)
	}

	// JSON transcript on session 2.
	msgRes2, err := adapter.ParseMessages(ctx, src2, nil)
	if err != nil {
		t.Fatalf("ParseMessages(2) error: %v", err)
	}
	if msgRes2.Append || len(msgRes2.Messages["grok-2"]) != 2 {
		t.Fatalf("msgRes2 = %+v", msgRes2)
	}
}
