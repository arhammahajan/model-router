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

func TestDeepseekAdapterDiscoverMetadataMessages(t *testing.T) {
	home := t.TempDir()
	env := convstore.NewScanEnv(harnessenv.Paths{Home: home})
	adapter := NewDeepseekAdapter()
	if adapter.Agent() != "deepseek" {
		t.Fatalf("Agent() = %q, want deepseek", adapter.Agent())
	}

	// Create a real hyphenated workspace dir so greedy path decoding preserves hyphens.
	realRepo := filepath.Join(t.TempDir(), "my-repo")
	if err := os.MkdirAll(realRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	encodedReal := "--" + strings.ReplaceAll(strings.Trim(realRepo, "/"), "/", "-") + "--"

	// Session 1: directory with meta.json (no explicit cwd -> decoded from dir name) + turns.jsonl.
	s1Dir := filepath.Join(home, ".dsh", "sessions", encodedReal, "dsh-1")
	if err := os.MkdirAll(s1Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s1Dir, "meta.json"),
		[]byte(`{"session_id":"dsh-1","title":"DeepSeek Cache Analysis","model":"deepseek-r1","created_at":"2026-11-20T07:00:00Z","updated_at":"2026-11-20T07:15:00Z"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	turnsPath := filepath.Join(s1Dir, "turns.jsonl")
	turns := strings.Join([]string{
		`{"role":"user","content":"Analyze cache eviction policy","timestamp":"2026-11-20T07:01:00Z"}`,
		`{"role":"assistant","content":"LRU with size cap works well here.","timestamp":"2026-11-20T07:02:00Z"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(turnsPath, []byte(turns), 0o644); err != nil {
		t.Fatal(err)
	}

	// Session 2 in shadow profile: standalone JSONL file with explicit cwd.
	s2Parent := filepath.Join(home, ".model-router", "profiles", "deepseek", "lab", "home", "sessions", "--work-ds--")
	if err := os.MkdirAll(s2Parent, 0o755); err != nil {
		t.Fatal(err)
	}
	s2File := filepath.Join(s2Parent, "dsh-2.jsonl")
	if err := os.WriteFile(s2File, []byte(`{"id":"dsh-2","cwd":"/work/explicit","role":"user","content":"Standalone JSONL prompt","timestamp":"2026-11-20T08:00:00Z"}`+"\n"), 0o644); err != nil {
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

	var src1, src2 convstore.DiscoveredSource
	for _, s := range sources {
		if s.Path == s1Dir {
			src1 = s
		} else {
			src2 = s
		}
	}
	if src1.Kind != convstore.SourceDir || src1.SizeBytes == 0 {
		t.Fatalf("src1 = %+v", src1)
	}
	if src2.Kind != convstore.SourceFile || src2.Profile != "lab" {
		t.Fatalf("src2 = %+v", src2)
	}

	m1, err := adapter.ParseMetadata(ctx, src1, nil)
	if err != nil {
		t.Fatalf("ParseMetadata(1) error: %v", err)
	}
	c1 := m1.Conversations[0]
	if c1.ID != "dsh-1" || c1.CWD != realRepo || c1.Title != "DeepSeek Cache Analysis" || c1.Model != "deepseek-r1" {
		t.Fatalf("c1 = %+v (want CWD=%q)", c1, realRepo)
	}

	m2, err := adapter.ParseMetadata(ctx, src2, nil)
	if err != nil {
		t.Fatalf("ParseMetadata(2) error: %v", err)
	}
	c2 := m2.Conversations[0]
	if c2.ID != "dsh-2" || c2.CWD != "/work/explicit" || c2.Title != "Standalone JSONL prompt" || c2.Profile != "lab" {
		t.Fatalf("c2 = %+v", c2)
	}

	// Best-effort decode for non-existent path.
	if got := deepseekDecodeDirName("--work-ds--"); got != "/work/ds" {
		t.Fatalf("deepseekDecodeDirName(--work-ds--) = %q, want /work/ds", got)
	}

	// Messages + incremental append for dir with single JSONL.
	msgRes1, err := adapter.ParseMessages(ctx, src1, nil)
	if err != nil {
		t.Fatalf("ParseMessages(1) error: %v", err)
	}
	msgs1 := msgRes1.Messages["dsh-1"]
	if len(msgs1) != 2 || msgs1[0].Role != "user" || msgs1[1].Role != "assistant" {
		t.Fatalf("msgs1 = %+v", msgs1)
	}

	f, err := os.OpenFile(turnsPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"role":"user","content":"What about LFU?","timestamp":"2026-11-20T07:05:00Z"}` + "\n")
	_ = f.Close()

	incRes, err := adapter.ParseMessages(ctx, src1, &convstore.SourceCursor{
		MsgByteOffset: msgRes1.ByteOffset,
		MsgPrefixHash: msgRes1.PrefixHash,
	})
	if err != nil {
		t.Fatalf("incremental ParseMessages error: %v", err)
	}
	if !incRes.Append || len(incRes.Messages["dsh-1"]) != 1 || incRes.Messages["dsh-1"][0].Text != "What about LFU?" {
		t.Fatalf("incRes = %+v", incRes)
	}
}
