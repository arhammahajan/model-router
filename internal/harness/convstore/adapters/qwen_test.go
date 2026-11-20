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

func TestQwenAdapterDiscoverMetadataMessages(t *testing.T) {
	home := t.TempDir()
	env := convstore.NewScanEnv(harnessenv.Paths{Home: home})
	adapter := NewQwenAdapter()
	if adapter.Agent() != "qwen" {
		t.Fatalf("Agent() = %q, want qwen", adapter.Agent())
	}

	chatsDir := filepath.Join(home, ".qwen", "projects", "-work-qwen", "chats")
	if err := os.MkdirAll(chatsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Runtime sidecar must be skipped.
	if err := os.WriteFile(filepath.Join(chatsDir, "sess-1.jsonl.runtime.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	sessPath := filepath.Join(chatsDir, "sess-1.jsonl")
	lines := strings.Join([]string{
		`{"type":"user","sessionId":"qwen-1","cwd":"/work/qwen","gitBranch":"feat/search","timestamp":"2026-11-20T08:00:00Z","message":{"parts":[{"text":"Build full-text search index"}]}}`,
		`{"type":"model","sessionId":"qwen-1","model":"qwen3-coder","timestamp":"2026-11-20T08:01:00Z","message":{"parts":[{"text":"Using SQLite FTS5 with BM25."}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(sessPath, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}

	// Shadow profile session with top-level content and explicit title.
	profChats := filepath.Join(home, ".model-router", "profiles", "qwen", "dev", "home", "projects", "-work-dev", "chats")
	if err := os.MkdirAll(profChats, 0o755); err != nil {
		t.Fatal(err)
	}
	profPath := filepath.Join(profChats, "stem-only.jsonl")
	profLines := strings.Join([]string{
		`{"type":"user","cwd":"/work/dev","title":"Custom Qwen Title","content":"Hello qwen","timestamp":"2026-11-20T09:00:00Z"}`,
		`{"type":"assistant","content":"Hi there!","timestamp":"2026-11-20T09:00:30Z"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(profPath, []byte(profLines), 0o644); err != nil {
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

	var mainSrc, profSrc convstore.DiscoveredSource
	for _, s := range sources {
		if s.Profile == "" {
			mainSrc = s
		} else {
			profSrc = s
		}
	}

	meta, err := adapter.ParseMetadata(ctx, mainSrc, nil)
	if err != nil {
		t.Fatalf("ParseMetadata error: %v", err)
	}
	c := meta.Conversations[0]
	if c.ID != "qwen-1" || c.CWD != "/work/qwen" || c.Branch != "feat/search" || c.Model != "qwen3-coder" {
		t.Fatalf("conv = %+v", c)
	}
	if c.Title != "Build full-text search index" {
		t.Fatalf("Title = %q", c.Title)
	}

	profMeta, err := adapter.ParseMetadata(ctx, profSrc, nil)
	if err != nil {
		t.Fatalf("ParseMetadata(prof) error: %v", err)
	}
	pc := profMeta.Conversations[0]
	if pc.ID != "stem-only" || pc.Title != "Custom Qwen Title" || pc.Profile != "dev" {
		t.Fatalf("profConv = %+v", pc)
	}

	msgRes, err := adapter.ParseMessages(ctx, mainSrc, nil)
	if err != nil {
		t.Fatalf("ParseMessages error: %v", err)
	}
	msgs := msgRes.Messages["qwen-1"]
	if len(msgs) != 2 {
		t.Fatalf("got %d msgs, want 2: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "user" || msgs[0].Text != "Build full-text search index" {
		t.Fatalf("msgs[0] = %+v", msgs[0])
	}
	if msgs[1].Role != "assistant" || msgs[1].Text != "Using SQLite FTS5 with BM25." {
		t.Fatalf("msgs[1] = %+v", msgs[1])
	}

	// Incremental append test.
	f, err := os.OpenFile(sessPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"type":"user","timestamp":"2026-11-20T08:05:00Z","message":{"content":"Follow-up question"}}` + "\n")
	_ = f.Close()

	incRes, err := adapter.ParseMessages(ctx, mainSrc, &convstore.SourceCursor{
		MsgByteOffset: msgRes.ByteOffset,
		MsgPrefixHash: msgRes.PrefixHash,
	})
	if err != nil {
		t.Fatalf("incremental ParseMessages error: %v", err)
	}
	if !incRes.Append || len(incRes.Messages["qwen-1"]) != 1 || incRes.Messages["qwen-1"][0].Text != "Follow-up question" {
		t.Fatalf("incRes = %+v", incRes)
	}
}
