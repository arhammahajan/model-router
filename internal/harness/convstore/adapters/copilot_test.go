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

func TestCopilotAdapterDiscoverMetadataMessages(t *testing.T) {
	home := t.TempDir()
	env := convstore.NewScanEnv(harnessenv.Paths{Home: home})
	adapter := NewCopilotAdapter()
	if adapter.Agent() != "copilot" {
		t.Fatalf("Agent() = %q, want copilot", adapter.Agent())
	}

	// Session 1: workspace.yaml with summary + sibling events.jsonl.
	s1Dir := filepath.Join(home, ".copilot", "session-state", "cp-1")
	if err := os.MkdirAll(s1Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ws1 := filepath.Join(s1Dir, "workspace.yaml")
	yaml1 := strings.Join([]string{
		"id: cp-1",
		"cwd: /work/copilot-repo",
		"git_root: /work/copilot-repo",
		"branch: fix/auth",
		"summary: |-",
		"  Copilot Auth Fix",
		"created_at: 2026-11-20T10:00:00Z",
		"updated_at: 2026-11-20T10:15:00Z",
	}, "\n") + "\n"
	if err := os.WriteFile(ws1, []byte(yaml1), 0o644); err != nil {
		t.Fatal(err)
	}
	events1 := filepath.Join(s1Dir, "events.jsonl")
	evLines := strings.Join([]string{
		`{"type":"user.message","timestamp":"2026-11-20T10:01:00Z","data":{"content":"Fix OAuth refresh race"}}`,
		`{"type":"assistant.message","timestamp":"2026-11-20T10:02:00Z","data":{"transformedContent":"Added a mutex around token refresh."}}`,
		`{"type":"tool.execution","data":{"content":"ignored"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(events1, []byte(evLines), 0o644); err != nil {
		t.Fatal(err)
	}

	// Session 2 in shadow profile: no title in workspace.yaml, falls back to first user message in events.jsonl.
	s2Dir := filepath.Join(home, ".model-router", "profiles", "copilot", "corp", "home", "session-state", "cp-2")
	if err := os.MkdirAll(s2Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ws2 := filepath.Join(s2Dir, "workspace.yaml")
	if err := os.WriteFile(ws2, []byte("id: cp-2\ngit_root: /work/corp\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s2Dir, "events.jsonl"),
		[]byte(`{"role":"user","content":"Audit deployment manifests","timestamp":"2026-11-20T12:00:00Z"}`+"\n"), 0o644); err != nil {
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
		if s.Path == ws1 {
			src1 = s
		} else {
			src2 = s
		}
	}
	if src1.WALSize == 0 || src1.WALMtimeNs == 0 {
		t.Fatalf("expected WALSize and WALMtimeNs from events.jsonl, got %+v", src1)
	}

	meta1, err := adapter.ParseMetadata(ctx, src1, nil)
	if err != nil {
		t.Fatalf("ParseMetadata(1) error: %v", err)
	}
	c1 := meta1.Conversations[0]
	if c1.ID != "cp-1" || c1.CWD != "/work/copilot-repo" || c1.Branch != "fix/auth" || c1.Title != "Copilot Auth Fix" {
		t.Fatalf("c1 = %+v", c1)
	}
	if c1.Preview != "Fix OAuth refresh race" {
		t.Fatalf("c1.Preview = %q", c1.Preview)
	}

	meta2, err := adapter.ParseMetadata(ctx, src2, nil)
	if err != nil {
		t.Fatalf("ParseMetadata(2) error: %v", err)
	}
	c2 := meta2.Conversations[0]
	if c2.ID != "cp-2" || c2.CWD != "/work/corp" || c2.Title != "Audit deployment manifests" || c2.Profile != "corp" {
		t.Fatalf("c2 = %+v", c2)
	}

	msgRes, err := adapter.ParseMessages(ctx, src1, nil)
	if err != nil {
		t.Fatalf("ParseMessages error: %v", err)
	}
	msgs := msgRes.Messages["cp-1"]
	if len(msgs) != 2 {
		t.Fatalf("got %d msgs, want 2: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "user" || msgs[0].Text != "Fix OAuth refresh race" {
		t.Fatalf("msgs[0] = %+v", msgs[0])
	}
	if msgs[1].Role != "assistant" || msgs[1].Text != "Added a mutex around token refresh." {
		t.Fatalf("msgs[1] = %+v", msgs[1])
	}

	// Append new event and verify incremental streaming.
	f, err := os.OpenFile(events1, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"type":"user.message","timestamp":"2026-11-20T10:20:00Z","data":{"content":"Write regression test"}}` + "\n")
	_ = f.Close()

	incRes, err := adapter.ParseMessages(ctx, src1, &convstore.SourceCursor{
		MsgByteOffset: msgRes.ByteOffset,
		MsgPrefixHash: msgRes.PrefixHash,
	})
	if err != nil {
		t.Fatalf("incremental ParseMessages error: %v", err)
	}
	if !incRes.Append || len(incRes.Messages["cp-1"]) != 1 || incRes.Messages["cp-1"][0].Text != "Write regression test" {
		t.Fatalf("incRes = %+v", incRes)
	}

	// Session 3: t3-code-provider-test probe stub with no events.jsonl must be skipped.
	s3Dir := filepath.Join(home, ".copilot", "session-state", "cp-probe")
	if err := os.MkdirAll(s3Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ws3 := filepath.Join(s3Dir, "workspace.yaml")
	if err := os.WriteFile(ws3, []byte("id: cp-probe\ncwd: /home/u\nclient_name: t3-code-provider-test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	meta3, err := adapter.ParseMetadata(ctx, convstore.DiscoveredSource{
		SourceKey: convstore.MakeSourceKey("copilot", "", ws3),
		Agent:     "copilot",
		Path:      ws3,
		Kind:      convstore.SourceFile,
	}, nil)
	if err != nil {
		t.Fatalf("ParseMetadata(probe) error: %v", err)
	}
	if len(meta3.Conversations) != 0 {
		t.Fatalf("expected probe stub to be skipped, got %+v", meta3.Conversations)
	}
}
