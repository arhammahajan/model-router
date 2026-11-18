// Lifecycle/TerminalBuffer identity integration: opaque IDs with byte budgets.
//
// Response/item/call IDs are opaque provider handles (may contain spaces
// or colons, proven by TestG2CRegrOpaqueCallIDsPreserved and the Chat row
// long-ID test), each bounded by lim.MaxArgsChars; tool names are
// nonempty here with the 1..64 grammar enforced target-side. Retained
// identity bytes are budgeted cumulatively under lim.MaxStreamArgsChars
// so 1MB IDs x 1024 blocks cannot build GB retention. The lifecycle
// counts and hashes content only; TerminalBuffer stays target-owned.
package protocol

import (
	"strings"
	"testing"
)

func TestLifecycleOpaqueIdentities(t *testing.T) {
	lim := DefaultLimits()
	// Long colon/space IDs accepted where proven (bounded, opaque).
	odd := "has:colon and space"
	long := strings.Repeat("z", 100)
	l := NewResponseLifecycle(lim, DialectChat)
	observeAll(t, l, ResponseStart{ID: ResponseID(long), Model: "m", Source: DialectChat})
	observeAll(t, l, BlockOpen{Item: ItemID("t0"), Kind: BlockText})
	observeAll(t, l, TextFragment{Item: "t0", Text: "hi"}, BlockClose{Item: "t0"})
	observeAll(t, l, BlockOpen{Item: ItemID("i1"), Kind: BlockTool, Call: CallID(odd), Name: "f"})
	observeAll(t, l, ArgsFragment{Item: "i1", Fragment: `{"a":1}`}, BlockClose{Item: "i1"})
	observeAll(t, l, UsageEvent{Usage: mustTerminalUsage(t, 5, 2)}, CompletionEvent{Finish: mustFinish(t, FinishStop)})
	if _, rerr := l.Result(); rerr != nil {
		t.Fatalf("opaque IDs: %v", rerr)
	}
	// Oversize single ID rejected (over lim.MaxArgsChars).
	l2 := NewResponseLifecycle(lim, DialectChat)
	if rerr := l2.Observe(ResponseStart{ID: ResponseID(strings.Repeat("x", lim.MaxArgsChars+1)), Model: "m", Source: DialectChat}); rerr == nil {
		t.Error("oversize response ID accepted")
	}
	l3 := NewResponseLifecycle(lim, DialectChat)
	observeAll(t, l3, ResponseStart{ID: "r1", Model: "m", Source: DialectChat})
	if rerr := l3.Observe(BlockOpen{Item: "t0", Kind: BlockTool, Call: CallID(strings.Repeat("y", lim.MaxArgsChars+1)), Name: "f"}); rerr == nil {
		t.Error("oversize call ID accepted")
	}
	// Grammar-invalid tool NAMES pass the lifecycle (target-side gate
	// owns the grammar) as long as they are nonempty and bounded.
	l4 := NewResponseLifecycle(lim, DialectChat)
	observeAll(t, l4, ResponseStart{ID: "r1", Model: "m", Source: DialectChat})
	observeAll(t, l4, BlockOpen{Item: "i1", Kind: BlockTool, Call: "c1", Name: "has space"})
	observeAll(t, l4, ArgsFragment{Item: "i1", Fragment: `{"a":1}`}, BlockClose{Item: "i1"})
	observeAll(t, l4, UsageEvent{Usage: mustTerminalUsage(t, 5, 2)}, CompletionEvent{Finish: mustFinish(t, FinishStop)})
	if _, rerr := l4.Result(); rerr != nil {
		t.Fatalf("grammar-invalid name rejected at lifecycle; must pass to target gate: %v", rerr)
	}
	// Source provenance and terminal contract remain enforced.
	l5 := NewResponseLifecycle(lim, DialectChat)
	if rerr := l5.Observe(ResponseStart{ID: "r1", Model: "m", Source: DialectGemini}); rerr == nil {
		t.Error("provenance mismatch accepted")
	}
}

func TestLifecycleIdentityBudget(t *testing.T) {
	// 1MB ID x 1024 blocks must not build GB retention: cumulative
	// identity bytes are capped by MaxStreamArgsChars.
	lim := Limits{MaxArgsChars: 1_000_000, MaxStreamArgsChars: 8_000_000, MaxToolCalls: 2048, MaxStreamBlocks: 2048, MaxFrameBytes: 8_000_000}
	l := NewResponseLifecycle(lim, DialectChat)
	observeAll(t, l, ResponseStart{ID: "r1", Model: "m", Source: DialectChat})
	big := strings.Repeat("q", 1_000_000)
	accepted := 0
	rejected := false
	for i := 0; i < 10; i++ {
		item := ItemID(string(rune('a'+i)) + big[:100])
		_ = item
		id := ItemID(strings.Repeat("i", 10) + string(rune('0'+i)) + strings.Repeat("j", 900_000))
		if rerr := l.Observe(BlockOpen{Item: id, Kind: BlockText}); rerr != nil {
			rejected = true
			break
		}
		accepted++
		_ = l.Observe(BlockClose{Item: id})
		_ = big
	}
	_ = accepted
	if !rejected {
		// 10 x ~900KB = ~9MB > 8MB budget: the 9th or 10th must fail.
		t.Error("cumulative identity budget never tripped over ~9MB retained IDs")
	}
	// TerminalBuffer applies the same opaque + budget policy standalone.
	b := NewTerminalBuffer(DefaultLimits())
	if rerr := b.ObserveText("has:colon and space", "hi"); rerr != nil {
		t.Fatalf("opaque item in buffer: %v", rerr)
	}
	if rerr := b.ObserveText("", "hi"); rerr == nil {
		t.Error("empty item accepted by buffer")
	}
	if rerr := b.ObserveText(ItemID(strings.Repeat("x", DefaultLimits().MaxArgsChars+1)), "hi"); rerr == nil {
		t.Error("oversize item accepted by buffer")
	}
}
