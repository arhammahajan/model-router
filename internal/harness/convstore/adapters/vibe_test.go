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

func TestVibeAdapterDiscoverMetadataMessages(t *testing.T) {
	home := t.TempDir()
	env := convstore.NewScanEnv(harnessenv.Paths{Home: home})
	adapter := NewVibeAdapter()
	if adapter.Agent() != "vibe" {
		t.Fatalf("Agent() = %q, want vibe", adapter.Agent())
	}

	// Session 1: meta.json + sibling messages.jsonl.
	s1Dir := filepath.Join(home, ".vibe", "logs", "session", "unified", "sess-1")
	if err := os.MkdirAll(s1Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta1Path := filepath.Join(s1Dir, "meta.json")
	meta1JSON := `{
		"session_id": "vibe-1",
		"title": "Vibe Migration Plan",
		"model": "devstral-2",
		"environment": {"working_directory": "/work/vibe-repo", "git_branch": "main"},
		"start_time": "2026-11-20T10:00:00Z",
		"end_time": "2026-11-20T10:30:00Z"
	}`
	if err := os.WriteFile(meta1Path, []byte(meta1JSON), 0o644); err != nil {
		t.Fatal(err)
	}
	msgs1Path := filepath.Join(s1Dir, "messages.jsonl")
	msgLines := strings.Join([]string{
		`{"role":"user","content":"Plan the DB schema migration","timestamp":"2026-11-20T10:01:00Z"}`,
		`{"role":"assistant","content":"Step 1: add columns with defaults.","timestamp":"2026-11-20T10:02:00Z"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(msgs1Path, []byte(msgLines), 0o644); err != nil {
		t.Fatal(err)
	}

	// Session 2 in shadow profile: meta.json only (origin_directory fallback, no transcript).
	s2Dir := filepath.Join(home, ".model-router", "profiles", "vibe", "alt", "home", "logs", "session", "unified", "sess-2")
	if err := os.MkdirAll(s2Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta2Path := filepath.Join(s2Dir, "meta.json")
	if err := os.WriteFile(meta2Path, []byte(`{"session_id":"vibe-2","origin_directory":"/work/vibe-alt","title":"Origin Dir Session","start_time":"2026-11-20T11:00:00Z"}`), 0o644); err != nil {
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
		if s.Path == meta1Path {
			src1 = s
		} else {
			src2 = s
		}
	}
	if src1.WALSize == 0 || src1.WALMtimeNs == 0 {
		t.Fatalf("expected WAL stats from sibling messages.jsonl, got %+v", src1)
	}
	if src2.WALSize != 0 || src2.Profile != "alt" {
		t.Fatalf("src2 = %+v", src2)
	}

	m1, err := adapter.ParseMetadata(ctx, src1, nil)
	if err != nil {
		t.Fatalf("ParseMetadata(1) error: %v", err)
	}
	c1 := m1.Conversations[0]
	if c1.ID != "vibe-1" || c1.CWD != "/work/vibe-repo" || c1.Branch != "main" || c1.Model != "devstral-2" || c1.Title != "Vibe Migration Plan" {
		t.Fatalf("c1 = %+v", c1)
	}

	m2, err := adapter.ParseMetadata(ctx, src2, nil)
	if err != nil {
		t.Fatalf("ParseMetadata(2) error: %v", err)
	}
	c2 := m2.Conversations[0]
	if c2.ID != "vibe-2" || c2.CWD != "/work/vibe-alt" || c2.Profile != "alt" {
		t.Fatalf("c2 = %+v", c2)
	}

	// Messages on session 1 + incremental append.
	msgRes, err := adapter.ParseMessages(ctx, src1, nil)
	if err != nil {
		t.Fatalf("ParseMessages(1) error: %v", err)
	}
	if len(msgRes.Messages["vibe-1"]) != 2 {
		t.Fatalf("msgs = %+v", msgRes.Messages)
	}

	f, err := os.OpenFile(msgs1Path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"role":"user","content":"Also add rollback steps","timestamp":"2026-11-20T10:10:00Z"}` + "\n")
	_ = f.Close()

	incRes, err := adapter.ParseMessages(ctx, src1, &convstore.SourceCursor{
		MsgByteOffset: msgRes.ByteOffset,
		MsgPrefixHash: msgRes.PrefixHash,
	})
	if err != nil {
		t.Fatalf("incremental ParseMessages error: %v", err)
	}
	if !incRes.Append || len(incRes.Messages["vibe-1"]) != 1 || incRes.Messages["vibe-1"][0].Text != "Also add rollback steps" {
		t.Fatalf("incRes = %+v", incRes)
	}

	// Session 2 without transcript returns nil, nil.
	msgRes2, err := adapter.ParseMessages(ctx, src2, nil)
	if err != nil || msgRes2 != nil {
		t.Fatalf("expected nil, nil when no transcript exists, got %+v, %v", msgRes2, err)
	}
}
