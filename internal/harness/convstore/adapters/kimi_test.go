package adapters

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arhammahajan/model-router/internal/harness/convstore"
	"github.com/arhammahajan/model-router/internal/harness/harnessenv"
)

func TestKimiAdapterDiscoverMetadataMessages(t *testing.T) {
	home := t.TempDir()
	env := convstore.NewScanEnv(harnessenv.Paths{Home: home})
	adapter := NewKimiAdapter()
	if adapter.Agent() != "kimi" {
		t.Fatalf("Agent() = %q, want kimi", adapter.Agent())
	}

	cwd1 := "/work/kimi-alpha"
	sum1 := md5.Sum([]byte(cwd1))
	hash1 := hex.EncodeToString(sum1[:])

	// ~/.kimi/kimi.json with object work_dirs + session with state.json and wire.jsonl.
	kimiRoot := filepath.Join(home, ".kimi")
	sess1Dir := filepath.Join(kimiRoot, "sessions", hash1, "km-1")
	if err := os.MkdirAll(sess1Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kimiRoot, "kimi.json"),
		[]byte(`{"work_dirs":[{"path":"`+cwd1+`","kaos":"local"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	state1 := filepath.Join(sess1Dir, "state.json")
	if err := os.WriteFile(state1, []byte(`{"custom_title":"Kimi Custom Title","updated_at":"2026-11-20T14:00:00Z"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	wire1 := filepath.Join(sess1Dir, "wire.jsonl")
	wireLines := strings.Join([]string{
		`{"timestamp":1763640000,"message":{"type":"TurnBegin","payload":{"user_input":"Refactor the wire parser"}}}`,
		`{"timestamp":1763640001,"message":{"type":"ContentPart","payload":{"type":"think","text":"internal thoughts"}}}`,
		`{"timestamp":1763640002,"message":{"type":"ContentPart","payload":{"type":"text","text":"First chunk. "}}}`,
		`{"timestamp":1763640003,"message":{"type":"ContentPart","payload":{"type":"text","text":"Second chunk."}}}`,
		`{"timestamp":1763640004,"message":{"type":"TurnEnd","payload":{}}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(wire1, []byte(wireLines), 0o644); err != nil {
		t.Fatal(err)
	}

	// ~/.kimi-code/sessions/*/* with wire.jsonl only (no state.json) and string work_dirs.
	cwd2 := "/work/kimi-beta"
	sum2 := md5.Sum([]byte(cwd2))
	hash2 := hex.EncodeToString(sum2[:])
	kimiCodeRoot := filepath.Join(home, ".kimi-code")
	sess2Dir := filepath.Join(kimiCodeRoot, "sessions", hash2, "km-2")
	if err := os.MkdirAll(sess2Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kimiCodeRoot, "kimi.json"),
		[]byte(`{"work_dirs":["`+cwd2+`"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	wire2 := filepath.Join(sess2Dir, "wire.jsonl")
	if err := os.WriteFile(wire2, []byte(`{"timestamp":1763641000,"message":{"type":"TurnBegin","payload":{"user_input":"Wire-only Kimi session"}}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Profile shadow home under kimi-cli.
	profSess := filepath.Join(home, ".model-router", "profiles", "kimi-cli", "team", "home", "sessions", hash1, "km-prof")
	if err := os.MkdirAll(profSess, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profSess, "state.json"), []byte(`{"title":"Team Kimi Session"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	sources, err := adapter.Discover(ctx, env)
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	if len(sources) != 3 {
		t.Fatalf("Discover returned %d sources, want 3: %+v", len(sources), sources)
	}

	var src1, src2, srcProf convstore.DiscoveredSource
	for _, s := range sources {
		switch s.Path {
		case state1:
			src1 = s
		case wire2:
			src2 = s
		default:
			srcProf = s
		}
	}
	if src1.WALSize == 0 || src1.WALMtimeNs == 0 {
		t.Fatalf("expected WAL stats for wire.jsonl on src1, got %+v", src1)
	}
	if srcProf.Profile != "team" {
		t.Fatalf("expected profile=team, got %+v", srcProf)
	}

	meta1, err := adapter.ParseMetadata(ctx, src1, nil)
	if err != nil {
		t.Fatalf("ParseMetadata(1) error: %v", err)
	}
	c1 := meta1.Conversations[0]
	if c1.ID != "km-1" || c1.CWD != cwd1 || c1.Title != "Kimi Custom Title" || c1.Preview != "Refactor the wire parser" {
		t.Fatalf("c1 = %+v", c1)
	}

	meta2, err := adapter.ParseMetadata(ctx, src2, nil)
	if err != nil {
		t.Fatalf("ParseMetadata(2) error: %v", err)
	}
	c2 := meta2.Conversations[0]
	if c2.ID != "km-2" || c2.CWD != cwd2 || c2.Title != "Wire-only Kimi session" {
		t.Fatalf("c2 = %+v", c2)
	}

	// Messages parsing with coalesced ContentPart chunks and skipped think blocks.
	msgRes, err := adapter.ParseMessages(ctx, src1, nil)
	if err != nil {
		t.Fatalf("ParseMessages error: %v", err)
	}
	msgs := msgRes.Messages["km-1"]
	if len(msgs) != 2 {
		t.Fatalf("got %d msgs, want 2: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "user" || msgs[0].Text != "Refactor the wire parser" {
		t.Fatalf("msgs[0] = %+v", msgs[0])
	}
	if msgs[1].Role != "assistant" || msgs[1].Text != "First chunk. Second chunk." {
		t.Fatalf("msgs[1] = %+v", msgs[1])
	}

	// Append a new wire turn and verify incremental streaming.
	f, err := os.OpenFile(wire1, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"timestamp":1763640010,"message":{"type":"TurnBegin","payload":{"user_input":"Next wire turn"}}}` + "\n")
	_, _ = f.WriteString(`{"timestamp":1763640011,"message":{"type":"AssistantMessage","payload":{"content":"Done with next turn."}}}` + "\n")
	_ = f.Close()

	incRes, err := adapter.ParseMessages(ctx, src1, &convstore.SourceCursor{
		MsgByteOffset: msgRes.ByteOffset,
		MsgPrefixHash: msgRes.PrefixHash,
	})
	if err != nil {
		t.Fatalf("incremental ParseMessages error: %v", err)
	}
	if !incRes.Append || len(incRes.Messages["km-1"]) != 2 {
		t.Fatalf("incRes = %+v", incRes)
	}
	if incRes.Messages["km-1"][0].Text != "Next wire turn" || incRes.Messages["km-1"][1].Text != "Done with next turn." {
		t.Fatalf("incMsgs = %+v", incRes.Messages["km-1"])
	}
}
