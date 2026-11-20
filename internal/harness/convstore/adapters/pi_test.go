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

func TestPiAdapterDiscoverMetadataMessages(t *testing.T) {
	home := t.TempDir()
	env := convstore.NewScanEnv(harnessenv.Paths{Home: home})
	adapter := NewPiAdapter()
	if adapter.Agent() != "pi" {
		t.Fatalf("Agent() = %q, want pi", adapter.Agent())
	}

	// Default session nested in a subdirectory.
	sessDir := filepath.Join(home, ".pi", "agent", "sessions", "work-proj")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sessPath := filepath.Join(sessDir, "2026-11-20_pi-1.jsonl")
	initial := strings.Join([]string{
		`{"type":"session","id":"pi-1","cwd":"/work/repo","timestamp":"2026-11-20T10:00:00Z","branch":"main"}`,
		`{"type":"message","timestamp":"2026-11-20T10:01:00Z","message":{"role":"user","content":"Implement the Pi adapter"}}`,
		`{"type":"message","timestamp":"2026-11-20T10:02:00Z","message":{"role":"assistant","model":"claude-sonnet","content":[{"type":"thinking","text":"secret"},{"type":"text","text":"Sure, implementing now."}]}}`,
		`{"type":"title","title":"Pi Adapter Implementation","timestamp":"2026-11-20T10:03:00Z"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(sessPath, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}

	// Profile shadow home with its own isolated session + a symlinked duplicate of the default dir.
	profSessions := filepath.Join(home, ".model-router", "profiles", "pi", "work", "home", "sessions")
	if err := os.MkdirAll(profSessions, 0o755); err != nil {
		t.Fatal(err)
	}
	profPath := filepath.Join(profSessions, "pi-prof.jsonl")
	profContent := strings.Join([]string{
		`{"type":"session","id":"pi-prof","cwd":"/work/other","timestamp":"2026-11-20T11:00:00Z"}`,
		`{"type":"message","timestamp":"2026-11-20T11:01:00Z","message":{"role":"user","content":"Fallback prompt title"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(profPath, []byte(profContent), 0o644); err != nil {
		t.Fatal(err)
	}
	// Symlink to default session should be deduplicated by MarkRealPath.
	if err := os.Symlink(sessPath, filepath.Join(profSessions, "dup.jsonl")); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	sources, err := adapter.Discover(ctx, env)
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	if len(sources) != 2 {
		t.Fatalf("Discover returned %d sources, want 2: %+v", len(sources), sources)
	}

	var defaultSrc, profSrc convstore.DiscoveredSource
	for _, s := range sources {
		if s.Profile == "" {
			defaultSrc = s
		} else {
			profSrc = s
		}
	}
	if defaultSrc.Path != sessPath {
		t.Fatalf("defaultSrc.Path = %q, want %q", defaultSrc.Path, sessPath)
	}
	if profSrc.Profile != "work" || profSrc.Path != profPath {
		t.Fatalf("profSrc = %+v", profSrc)
	}

	// Metadata on default session (explicit title wins, ResumeTarget == src.Path).
	meta, err := adapter.ParseMetadata(ctx, defaultSrc, nil)
	if err != nil {
		t.Fatalf("ParseMetadata error: %v", err)
	}
	if len(meta.Conversations) != 1 {
		t.Fatalf("got %d conversations, want 1", len(meta.Conversations))
	}
	c := meta.Conversations[0]
	if c.ID != "pi-1" || c.CWD != "/work/repo" || c.Branch != "main" || c.Model != "claude-sonnet" {
		t.Fatalf("unexpected conv fields: %+v", c)
	}
	if c.Title != "Pi Adapter Implementation" {
		t.Fatalf("Title = %q, want Pi Adapter Implementation", c.Title)
	}
	if c.Preview != "Implement the Pi adapter" {
		t.Fatalf("Preview = %q, want Implement the Pi adapter", c.Preview)
	}
	if c.ResumeTarget != sessPath {
		t.Fatalf("ResumeTarget = %q, want %q", c.ResumeTarget, sessPath)
	}
	if c.CreatedAtMs == 0 || c.UpdatedAtMs <= c.CreatedAtMs {
		t.Fatalf("unexpected timestamps: created=%d updated=%d", c.CreatedAtMs, c.UpdatedAtMs)
	}

	// Metadata on profile session (fallback to first user prompt).
	profMeta, err := adapter.ParseMetadata(ctx, profSrc, nil)
	if err != nil {
		t.Fatalf("ParseMetadata(prof) error: %v", err)
	}
	if len(profMeta.Conversations) != 1 || profMeta.Conversations[0].Title != "Fallback prompt title" {
		t.Fatalf("profMeta = %+v", profMeta)
	}

	// Messages full parse.
	msgRes, err := adapter.ParseMessages(ctx, defaultSrc, nil)
	if err != nil {
		t.Fatalf("ParseMessages error: %v", err)
	}
	if msgRes.Append {
		t.Fatalf("expected Append=false on initial parse")
	}
	msgs := msgRes.Messages["pi-1"]
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "user" || msgs[0].Text != "Implement the Pi adapter" {
		t.Fatalf("msg[0] = %+v", msgs[0])
	}
	if msgs[1].Role != "assistant" || msgs[1].Text != "Sure, implementing now." {
		t.Fatalf("msg[1] = %+v", msgs[1])
	}

	// Append a new message and verify incremental streaming preserves convID.
	appendedLine := `{"type":"message","timestamp":"2026-11-20T10:05:00Z","message":{"role":"user","content":"Add unit tests too"}}` + "\n"
	f, err := os.OpenFile(sessPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(appendedLine); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	cursor := &convstore.SourceCursor{
		MsgByteOffset: msgRes.ByteOffset,
		MsgPrefixHash: msgRes.PrefixHash,
	}
	incRes, err := adapter.ParseMessages(ctx, defaultSrc, cursor)
	if err != nil {
		t.Fatalf("incremental ParseMessages error: %v", err)
	}
	if !incRes.Append {
		t.Fatalf("expected Append=true on incremental parse")
	}
	incMsgs := incRes.Messages["pi-1"]
	if len(incMsgs) != 1 || incMsgs[0].Text != "Add unit tests too" {
		t.Fatalf("incMsgs = %+v", incMsgs)
	}
}
