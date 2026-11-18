// Chat source stream IR provenance tests: malformed-args classification and
// logical source-index carriage.
//
// These tests pin the two typed source facts the shared lifecycle retains
// for later target gates (Gemini suppress-and-remap vs Anthropic/Responses
// refuse; reviewed logical ordering without reparsing opaque ItemIDs).
// They drive NewChatStreamDecoder directly, independent of any target.
package protocol

import (
	"testing"
)

// --- Ordinary malformed tool args are typed provenance, not 502 --------

func TestChatStreamIRMalformedArgsClassified(t *testing.T) {
	mk := func(argsFrag string) []string {
		return []string{
			csChunk(`{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"a","arguments":"`+argsFrag+`"}}]}`, ""),
			csChunk(`{}`, "tool_calls"),
			csUsageChunk(csUsage(3, 2)),
			"[DONE]",
		}
	}
	for name, argsFrag := range map[string]string{
		"truncated": `{\"a\":`,
		"array":     `[1]`,
		"scalar":    `\"s\"`,
	} {
		evs := csFullSuccess(t, mk(argsFrag))
		var malformed []MalformedToolEvent
		for _, ev := range evs {
			if e, ok := ev.(MalformedToolEvent); ok {
				malformed = append(malformed, e)
			}
		}
		if len(malformed) != 1 || malformed[0].Item != "c0" {
			t.Fatalf("%s: MalformedToolEvent = %+v, want exactly one for c0", name, malformed)
		}
		if _, ok := evs[len(evs)-1].(CompletionEvent); !ok {
			t.Fatalf("%s: terminal = %#v, want completion (malformed is provenance, not failure)", name, evs[len(evs)-1])
		}
		// The fact survives validation coherently: success summary with
		// the malformed count and per-block flag, exact arg bytes kept.
		life := NewResponseLifecycle(DefaultLimits(), DialectChat)
		for _, ev := range evs {
			if rerr := life.Observe(ev); rerr != nil {
				t.Fatalf("%s: lifecycle Observe(%T): %v", name, ev, rerr)
			}
		}
		res, rerr := life.Result()
		if rerr != nil {
			t.Fatalf("%s: lifecycle Result: %v", name, rerr)
		}
		if res.MalformedTools != 1 {
			t.Fatalf("%s: MalformedTools = %d, want 1", name, res.MalformedTools)
		}
		if len(res.Blocks) != 1 || !res.Blocks[0].Malformed {
			t.Fatalf("%s: block malformed flag = %+v, want one malformed block", name, res.Blocks)
		}
		// No valid object exists to reconstruct: the terminal buffer
		// still refuses the bytes (the later target gate decides).
		buf := NewTerminalBuffer(DefaultLimits())
		for _, ev := range evs {
			if e, ok := ev.(ArgsFragment); ok {
				if rerr := buf.ObserveArgs(e.Item, e.Fragment); rerr != nil {
					t.Fatalf("%s: buffer ObserveArgs: %v", name, rerr)
				}
			}
		}
		if _, rerr := buf.ArgsResult("c0"); rerr == nil {
			t.Fatalf("%s: malformed args reconstructed by terminal buffer", name)
		}
	}
	// Identity conflicts are still syntax-class 502s, never malformed
	// provenance: the conflicting second identity fails at its frame.
	csMustFailAt(t, []string{
		csChunk(`{"tool_calls":[{"index":0,"id":"c1","function":{"name":"a","arguments":"[1]"}}]}`, ""),
		csChunk(`{"tool_calls":[{"index":0,"id":"c2","function":{"name":"a","arguments":""}}]}`, ""),
	}, 1)
}

// --- Logical tool indices are typed provenance, not ID parsing ---------

func TestChatStreamIRSourceIndexProvenance(t *testing.T) {
	// Sparse indices arrive out of order; arrival order is preserved
	// (never sorted) while each block carries its logical position.
	evs := csFullSuccess(t, []string{
		csChunk(`{"tool_calls":[{"index":5,"id":"idx-2","type":"function","function":{"name":"five","arguments":""}}]}`, ""),
		csChunk(`{"tool_calls":[{"index":2,"id":"idx-5","type":"function","function":{"name":"two","arguments":""}}]}`, ""),
		csChunk(`{"tool_calls":[{"index":5,"function":{"arguments":"{}"}}]}`, ""),
		csChunk(`{"tool_calls":[{"index":2,"function":{"arguments":"{}"}}]}`, ""),
		csChunk(`{}`, "tool_calls"),
		csUsageChunk(csUsage(9, 4)),
		"[DONE]",
	})
	var opens []BlockOpen
	for _, ev := range evs {
		if e, ok := ev.(BlockOpen); ok && e.Kind == BlockTool {
			opens = append(opens, e)
		}
	}
	if len(opens) != 2 {
		t.Fatalf("tool opens = %+v, want 2", opens)
	}
	// Arrival order (5 before 2), opaque identities untouched, logical
	// indices typed: the misleading call-id text ("idx-2" on index 5)
	// proves no inference from id text.
	if opens[0].Item != "c5" || opens[0].SourceIndex == nil || *opens[0].SourceIndex != 5 || string(opens[0].Call) != "idx-2" {
		t.Fatalf("first open = %+v, want Item c5 index 5 call idx-2", opens[0])
	}
	if opens[1].Item != "c2" || opens[1].SourceIndex == nil || *opens[1].SourceIndex != 2 || string(opens[1].Call) != "idx-5" {
		t.Fatalf("second open = %+v, want Item c2 index 2 call idx-5", opens[1])
	}
	// The summary retains the same indices in the same arrival order.
	life := NewResponseLifecycle(DefaultLimits(), DialectChat)
	for _, ev := range evs {
		if rerr := life.Observe(ev); rerr != nil {
			t.Fatalf("lifecycle Observe(%T): %v", ev, rerr)
		}
	}
	res, rerr := life.Result()
	if rerr != nil {
		t.Fatalf("lifecycle Result: %v", rerr)
	}
	if len(res.Blocks) != 2 ||
		res.Blocks[0].SourceIndex == nil || *res.Blocks[0].SourceIndex != 5 ||
		res.Blocks[1].SourceIndex == nil || *res.Blocks[1].SourceIndex != 2 {
		t.Fatalf("summary indices = %+v, want [5 2]", res.Blocks)
	}
}

func TestChatStreamIRSourceIndexValidation(t *testing.T) {
	usage, rerr := NewTerminalUsage(DialectChat, 1, 1, 2)
	if rerr != nil {
		t.Fatalf("usage: %v", rerr)
	}
	complete, rerr := NewCompletionEvent(Finish{Kind: FinishStop})
	if rerr != nil {
		t.Fatalf("completion: %v", rerr)
	}
	start := func() *ResponseLifecycle {
		life := NewResponseLifecycle(DefaultLimits(), DialectChat)
		if rerr := life.Observe(ResponseStart{ID: "r1", Model: "m", Source: DialectChat}); rerr != nil {
			t.Fatalf("start: %v", rerr)
		}
		return life
	}
	openTool := func(item string, call string, idx int) BlockOpen {
		open, rerr := NewToolBlockOpen(ItemID(item), CallID(call), "f", idx)
		if rerr != nil {
			t.Fatalf("NewToolBlockOpen: %v", rerr)
		}
		return open
	}
	// Duplicate logical indices on distinct items conflict.
	life := start()
	if rerr := life.Observe(openTool("c0", "call-a", 0)); rerr != nil {
		t.Fatalf("first index: %v", rerr)
	}
	if rerr := life.Observe(openTool("c1", "call-b", 0)); rerr == nil {
		t.Fatal("duplicate source index accepted")
	}
	// Negative indices never construct.
	if _, rerr := NewToolBlockOpen("c9", "call-z", "f", -1); rerr == nil {
		t.Fatal("negative source index constructed")
	} else if rerr.Status != 502 {
		t.Fatalf("negative index: status=%d, want 502", rerr.Status)
	}
	// Text blocks from indexed sources carry their logical position;
	// Chat text stays unindexed by its decoder, but the lifecycle
	// accepts indexed text for Anthropic/Responses provenance.
	life2 := start()
	indexedText, rerr := NewIndexedBlockOpen("t0", BlockText, "", "", 0)
	if rerr != nil {
		t.Fatalf("indexed text construct: %v", rerr)
	}
	if rerr := life2.Observe(indexedText); rerr != nil {
		t.Fatalf("indexed text block refused: %v", rerr)
	}
	// Cross-kind duplicate logical indices conflict.
	lifeX := start()
	toolX, rerr := NewToolBlockOpen("c0", "call-a", "f", 0)
	if rerr != nil {
		t.Fatalf("tool open: %v", rerr)
	}
	if rerr := lifeX.Observe(toolX); rerr != nil {
		t.Fatalf("tool open observe: %v", rerr)
	}
	dupText, rerr := NewIndexedBlockOpen("t0", BlockText, "", "", 0)
	if rerr != nil {
		t.Fatalf("dup text construct: %v", rerr)
	}
	if rerr := lifeX.Observe(dupText); rerr == nil {
		t.Fatal("cross-kind duplicate index accepted")
	}
	// Wrong Call/Name on text still refuses at construction.
	if _, rerr := NewBlockOpen("t9", BlockText, "c", ""); rerr == nil {
		t.Fatal("call-on-text accepted")
	}
	// Malformed classification needs an open tool block, exactly once.
	life3 := start()
	if rerr := life3.Observe(openTool("c0", "call-a", 3)); rerr != nil {
		t.Fatalf("open: %v", rerr)
	}
	if rerr := life3.Observe(MalformedToolEvent{Item: "missing"}); rerr == nil {
		t.Fatal("malformed for unopened block accepted")
	}
	if rerr := life3.Observe(MalformedToolEvent{Item: "c0"}); rerr != nil {
		t.Fatalf("malformed: %v", rerr)
	}
	if rerr := life3.Observe(MalformedToolEvent{Item: "c0"}); rerr == nil {
		t.Fatal("duplicate malformed accepted")
	}
	if rerr := life3.Observe(BlockClose{Item: "c0"}); rerr != nil {
		t.Fatalf("close: %v", rerr)
	}
	if rerr := life3.Observe(UsageEvent{Usage: usage}); rerr != nil {
		t.Fatalf("usage: %v", rerr)
	}
	if rerr := life3.Observe(complete); rerr != nil {
		t.Fatalf("completion: %v", rerr)
	}
	res, rerr := life3.Result()
	if rerr != nil {
		t.Fatalf("Result: %v", rerr)
	}
	if res.MalformedTools != 1 || len(res.Blocks) != 1 || !res.Blocks[0].Malformed || res.Blocks[0].SourceIndex == nil || *res.Blocks[0].SourceIndex != 3 {
		t.Fatalf("malformed summary = %+v", res)
	}
}

func TestChatStreamIRIndexCopiesAndMalformedFinality(t *testing.T) {
	life := NewResponseLifecycle(DefaultLimits(), DialectChat)
	observeAll(t, life, ResponseStart{ID: "r", Model: "m", Source: DialectChat})
	open, err := NewToolBlockOpen("opaque-item", "opaque-call", "f", 0)
	if err != nil {
		t.Fatal(err)
	}
	observeAll(t, life, open)
	*open.SourceIndex = 9
	observeAll(t, life, MalformedToolEvent{Item: open.Item})
	if err := life.Observe(ArgsFragment{Item: open.Item, Fragment: "{}"}); err == nil {
		t.Fatal("arguments accepted after malformed classification")
	}
	observeAll(t, life, BlockClose{Item: open.Item}, UsageEvent{Usage: mustTerminalUsage(t, 1, 1)}, CompletionEvent{Finish: Finish{Kind: FinishToolCalls}})
	res, err := life.Result()
	if err != nil {
		t.Fatal(err)
	}
	if res.Blocks[0].SourceIndex == nil || *res.Blocks[0].SourceIndex != 0 {
		t.Fatal("source index aliased to event")
	}
	*res.Blocks[0].SourceIndex = 8
	again, err := life.Result()
	if err != nil {
		t.Fatal(err)
	}
	if *again.Blocks[0].SourceIndex != 0 {
		t.Fatal("source index aliased to result")
	}
}
