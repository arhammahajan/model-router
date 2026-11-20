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

func TestClaudeAdapterDiscoverMetadataAndIncrementalMessages(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	projDir := filepath.Join(home, ".claude", "projects", "-work-app")
	subDir := filepath.Join(projDir, "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}

	mainFile := filepath.Join(projDir, "sess-main.jsonl")
	mainLines := strings.Join([]string{
		`{"type":"user","isMeta":true,"sessionId":"sess-main","cwd":"/work/app","message":{"content":"<system-reminder>meta</system-reminder>"}}`,
		`{"type":"user","sessionId":"sess-main","cwd":"/work/app","gitBranch":"feat/auth","timestamp":"2026-11-20T10:00:00Z","message":{"content":"Fix login token refresh"}}`,
		`{"type":"assistant","sessionId":"sess-main","timestamp":"2026-11-20T10:01:00Z","message":{"model":"claude-sonnet-4-5","content":[{"type":"thinking","text":"internal"},{"type":"text","text":"I will inspect auth.go."}]}}`,
		`{"type":"ai-title","aiTitle":"AI Login Title"}`,
		`{"type":"custom-title","customTitle":"Renamed Auth Fix"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(mainFile, []byte(mainLines), 0o644); err != nil {
		t.Fatal(err)
	}

	// Sidechain file inside project dir (should be skipped in ParseMetadata/ParseMessages).
	sideFile := filepath.Join(projDir, "sess-side.jsonl")
	sideLines := `{"type":"user","isSidechain":true,"sessionId":"sess-side","cwd":"/work/app","message":{"content":"subagent prompt"}}` + "\n"
	if err := os.WriteFile(sideFile, []byte(sideLines), 0o644); err != nil {
		t.Fatal(err)
	}

	// File inside subagents/ dir (should be skipped in Discover).
	if err := os.WriteFile(filepath.Join(subDir, "nested.jsonl"), []byte(`{"type":"user","sessionId":"nested","message":{"content":"nested"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Isolated profile shadow home with a distinct file and a symlink to mainFile (deduplicated).
	shadowHome := filepath.Join(home, ".model-router", "profiles", "claude", "work", "home")
	shadowProj := filepath.Join(shadowHome, "projects", "-work-other")
	if err := os.MkdirAll(shadowProj, 0o755); err != nil {
		t.Fatal(err)
	}
	shadowFile := filepath.Join(shadowProj, "sess-work.jsonl")
	if err := os.WriteFile(shadowFile, []byte(`{"type":"user","sessionId":"sess-work","cwd":"/work/other","timestamp":"2026-11-20T11:00:00Z","message":{"content":"Work profile task"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(mainFile, filepath.Join(shadowProj, "dup-main.jsonl")); err != nil {
		t.Fatal(err)
	}

	env := convstore.NewScanEnv(harnessenv.Paths{Home: home, DataHome: filepath.Join(home, ".local", "share")})
	adapter := NewClaudeAdapter()
	if adapter.Agent() != "claude" {
		t.Fatalf("Agent() = %q", adapter.Agent())
	}

	ctx := context.Background()
	sources, err := adapter.Discover(ctx, env)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(sources) != 3 {
		t.Fatalf("Discover len = %d, want 3 (main, side, shadow; dup & subagents skipped): %+v", len(sources), sources)
	}

	byPath := map[string]convstore.DiscoveredSource{}
	for _, s := range sources {
		byPath[s.Path] = s
	}
	if _, ok := byPath[filepath.Clean(mainFile)]; !ok {
		t.Fatalf("missing mainFile in discovered sources")
	}
	if got := byPath[filepath.Clean(shadowFile)].Profile; got != "work" {
		t.Fatalf("shadow profile = %q, want work", got)
	}

	// ParseMetadata on mainFile.
	meta, err := adapter.ParseMetadata(ctx, byPath[filepath.Clean(mainFile)], nil)
	if err != nil {
		t.Fatalf("ParseMetadata(main): %v", err)
	}
	if len(meta.Conversations) != 1 {
		t.Fatalf("main convs = %d, want 1", len(meta.Conversations))
	}
	c := meta.Conversations[0]
	if c.ID != "sess-main" || c.App != "claude-code" || c.Title != "Renamed Auth Fix" || c.CWD != "/work/app" || c.Branch != "feat/auth" || c.Model != "claude-sonnet-4-5" || c.MessageCount != 2 {
		t.Fatalf("unexpected conv metadata: %+v", c)
	}
	if c.CreatedAtMs == 0 || c.UpdatedAtMs <= c.CreatedAtMs {
		t.Fatalf("unexpected timestamps: created=%d updated=%d", c.CreatedAtMs, c.UpdatedAtMs)
	}

	// ParseMetadata on sidechain file returns empty.
	sideMeta, err := adapter.ParseMetadata(ctx, byPath[filepath.Clean(sideFile)], nil)
	if err != nil {
		t.Fatalf("ParseMetadata(side): %v", err)
	}
	if len(sideMeta.Conversations) != 0 {
		t.Fatalf("expected sidechain to be skipped, got %+v", sideMeta.Conversations)
	}

	// ParseMessages initial pass.
	msgRes, err := adapter.ParseMessages(ctx, byPath[filepath.Clean(mainFile)], nil)
	if err != nil {
		t.Fatalf("ParseMessages(initial): %v", err)
	}
	if msgRes.Append {
		t.Fatalf("expected Append=false on initial pass")
	}
	msgs := msgRes.Messages["sess-main"]
	if len(msgs) != 2 || msgs[0].Role != "user" || msgs[0].Text != "Fix login token refresh" || msgs[1].Role != "assistant" || msgs[1].Text != "I will inspect auth.go." {
		t.Fatalf("unexpected initial messages: %+v", msgs)
	}

	// Append a new turn to mainFile and verify incremental append.
	f, err := os.OpenFile(mainFile, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"type":"user","sessionId":"sess-main","timestamp":"2026-11-20T10:02:00Z","message":{"content":"Also add unit tests"}}` + "\n")
	f.Close()

	prev := &convstore.SourceCursor{
		MsgByteOffset: msgRes.ByteOffset,
		MsgPrefixHash: msgRes.PrefixHash,
	}
	incRes, err := adapter.ParseMessages(ctx, byPath[filepath.Clean(mainFile)], prev)
	if err != nil {
		t.Fatalf("ParseMessages(incremental): %v", err)
	}
	if !incRes.Append {
		t.Fatalf("expected Append=true on incremental pass")
	}
	incMsgs := incRes.Messages["sess-main"]
	if len(incMsgs) != 1 || incMsgs[0].Role != "user" || incMsgs[0].Text != "Also add unit tests" {
		t.Fatalf("unexpected incremental messages: %+v", incMsgs)
	}
	if incRes.ByteOffset <= msgRes.ByteOffset {
		t.Fatalf("expected ByteOffset to advance: %d -> %d", msgRes.ByteOffset, incRes.ByteOffset)
	}
}

func TestClaudeAdapterConfigDirHonoredWhenHomeMatchesOS(t *testing.T) {
	home := t.TempDir()
	customCfg := filepath.Join(t.TempDir(), "custom-claude")
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", customCfg)

	proj := filepath.Join(customCfg, "projects", "-repo")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(proj, "abc.jsonl")
	if err := os.WriteFile(f, []byte(`{"type":"user","message":{"content":"hello from custom dir"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	env := convstore.NewScanEnv(harnessenv.Paths{Home: home})
	sources, err := NewClaudeAdapter().Discover(context.Background(), env)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(sources) != 1 || sources[0].Path != filepath.Clean(f) {
		t.Fatalf("got sources %+v, want %s", sources, f)
	}
}
