package adapters

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/arhammahajan/model-router/internal/harness/convstore"
	"github.com/arhammahajan/model-router/internal/harness/harnessenv"
)

func TestClineAdapterDiscoverMetadataMessages(t *testing.T) {
	home := t.TempDir()
	env := convstore.NewScanEnv(harnessenv.Paths{Home: home})
	adapter := NewClineAdapter()
	if adapter.Agent() != "cline" {
		t.Fatalf("Agent() = %q, want cline", adapter.Agent())
	}

	// Session 1: default home, JSON array messages.
	w1Dir := filepath.Join(home, ".cline", "data", "sessions", "ws1")
	if err := os.MkdirAll(w1Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	s1Path := filepath.Join(w1Dir, "s1.json")
	if err := os.WriteFile(s1Path, []byte(`{
		"session_id": "cl-1",
		"cwd": "/work/cline-repo",
		"title": "Cline Refactor",
		"prompt": "Refactor the auth middleware",
		"model": "claude-3.7-sonnet",
		"started_at": "2026-11-20T09:00:00Z",
		"updated_at": "2026-11-20T09:20:00Z"
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s1Msgs := filepath.Join(w1Dir, "s1.messages.json")
	if err := os.WriteFile(s1Msgs, []byte(`[
		{"role":"user","content":"Refactor the auth middleware","timestamp":"2026-11-20T09:00:01Z"},
		{"role":"assistant","content":[{"type":"text","text":"Middleware split into verifier and session store."}],"timestamp":"2026-11-20T09:00:30Z"}
	]`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Session 2: shadow profile, object wrapper {"messages": [...]}, title falls back to prompt.
	w2Dir := filepath.Join(home, ".model-router", "profiles", "cline", "personal", "home", "sessions", "ws2")
	if err := os.MkdirAll(w2Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	s2Path := filepath.Join(w2Dir, "s2.json")
	if err := os.WriteFile(s2Path, []byte(`{"session_id":"cl-2","cwd":"/work/personal","prompt":"Add dark mode toggle","started_at":"2026-11-20T10:00:00Z"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s2Msgs := filepath.Join(w2Dir, "s2.messages.json")
	if err := os.WriteFile(s2Msgs, []byte(`{"messages":[{"role":"user","content":"Add dark mode toggle"},{"role":"assistant","content":"Done."}]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	sources, err := adapter.Discover(ctx, env)
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	// *.messages.json must be skipped as primary sources.
	if len(sources) != 2 {
		t.Fatalf("Discover returned %d sources, want 2: %+v", len(sources), sources)
	}

	var src1, src2 convstore.DiscoveredSource
	for _, s := range sources {
		if s.Path == s1Path {
			src1 = s
		} else {
			src2 = s
		}
	}
	if src1.WALSize == 0 || src1.WALMtimeNs == 0 {
		t.Fatalf("expected WAL stats for s1.messages.json, got %+v", src1)
	}

	m1, err := adapter.ParseMetadata(ctx, src1, nil)
	if err != nil {
		t.Fatalf("ParseMetadata(1) error: %v", err)
	}
	c1 := m1.Conversations[0]
	if c1.ID != "cl-1" || c1.CWD != "/work/cline-repo" || c1.Title != "Cline Refactor" || c1.Preview != "Refactor the auth middleware" || c1.Model != "claude-3.7-sonnet" {
		t.Fatalf("c1 = %+v", c1)
	}

	m2, err := adapter.ParseMetadata(ctx, src2, nil)
	if err != nil {
		t.Fatalf("ParseMetadata(2) error: %v", err)
	}
	c2 := m2.Conversations[0]
	if c2.ID != "cl-2" || c2.Title != "Add dark mode toggle" || c2.Profile != "personal" {
		t.Fatalf("c2 = %+v", c2)
	}

	msgRes1, err := adapter.ParseMessages(ctx, src1, nil)
	if err != nil {
		t.Fatalf("ParseMessages(1) error: %v", err)
	}
	msgs1 := msgRes1.Messages["cl-1"]
	if len(msgs1) != 2 || msgs1[0].Role != "user" || msgs1[1].Text != "Middleware split into verifier and session store." {
		t.Fatalf("msgs1 = %+v", msgs1)
	}

	msgRes2, err := adapter.ParseMessages(ctx, src2, nil)
	if err != nil {
		t.Fatalf("ParseMessages(2) error: %v", err)
	}
	if len(msgRes2.Messages["cl-2"]) != 2 {
		t.Fatalf("msgs2 = %+v", msgRes2.Messages)
	}
}
