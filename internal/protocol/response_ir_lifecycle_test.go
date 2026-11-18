// Lifecycle validator tests: order, terminal contract, bounds, retention.
//
// The lifecycle retains NO text and NO argument bytes. These tests pin
// that correction: multi-megabyte text validates without retention,
// while the opt-in target-owned TerminalBuffer enforces the inherited
// bounds (per-args-field MaxArgsChars, shared text+args aggregate
// MaxStreamArgsChars, NO per-text-block cap).
package protocol

import (
	"strings"
	"testing"
)

func lifecycleStart(t *testing.T, source Dialect) (*ResponseLifecycle, ResponseStart) {
	t.Helper()
	l := NewResponseLifecycle(testRespLimits(), source)
	start := ResponseStart{ID: "r1", Model: "m", Source: source}
	observeAll(t, l, start)
	return l, start
}

// --- Order: start/block/delta/end/usage/completion -------------------------------

func TestResponseIRLifecycleOrder(t *testing.T) {
	lim := testRespLimits()
	start := ResponseStart{ID: "r1", Model: "m", Source: DialectChat}
	textOpen, _ := NewBlockOpen("t0", BlockText, "", "")
	_ = textOpen
	usage := UsageEvent{Usage: mustTerminalUsage(t, 5, 2)}
	complete, _ := NewCompletionEvent(mustFinish(t, FinishStop))

	// Content before start fails, not disappears.
	l := NewResponseLifecycle(lim, DialectChat)
	if rerr := l.Observe(textOpen); rerr == nil {
		t.Error("block before start accepted")
	}
	// Duplicate start fails.
	l2 := NewResponseLifecycle(lim, DialectChat)
	observeAll(t, l2, start)
	if rerr := l2.Observe(start); rerr == nil {
		t.Error("duplicate start accepted")
	}
	// Delta for an unopened block fails (no seeded blocks).
	l3 := NewResponseLifecycle(lim, DialectChat)
	observeAll(t, l3, start)
	if rerr := l3.Observe(TextFragment{Item: "t0", Text: "hi"}); rerr == nil {
		t.Error("delta for unopened block accepted")
	}
	// No dense-index requirement: string ItemIDs open in arrival order.
	observeAll(t, l3, BlockOpen{Item: "t0", Kind: BlockText})
	observeAll(t, l3, BlockOpen{Item: "z-9", Kind: BlockTool, Call: "c1", Name: "f"})
	// Kind mismatch: args on a text block, text on a tool block.
	if rerr := l3.Observe(ArgsFragment{Item: "t0", Fragment: `{"a":1}`}); rerr == nil {
		t.Error("args fragment on text block accepted")
	}
	observeAll(t, l3, BlockClose{Item: "t0"})
	// Delta for a closed block fails.
	if rerr := l3.Observe(TextFragment{Item: "t0", Text: "x"}); rerr == nil {
		t.Error("delta for closed block accepted")
	}
	if rerr := l3.Observe(TextFragment{Item: "z-9", Text: "hi"}); rerr == nil {
		t.Error("text fragment on tool block accepted")
	}
	// Duplicate block ID fails.
	if rerr := l3.Observe(BlockOpen{Item: "t0", Kind: BlockText}); rerr == nil {
		t.Error("duplicate block ID accepted")
	}
	// Completion with an unclosed block fails.
	if rerr := l3.Observe(complete); rerr == nil {
		t.Error("completion with unclosed block accepted")
	}
	observeAll(t, l3,
		ArgsFragment{Item: "z-9", Fragment: `{"a":`},
		ArgsFragment{Item: "z-9", Fragment: `1}`},
		BlockClose{Item: "z-9"},
		usage, complete)
	// Events after completion fail.
	if rerr := l3.Observe(usage); rerr == nil {
		t.Error("events after completion accepted")
	}

	// Refusal blocks accept text; tool blocks reject it.
	l4 := NewResponseLifecycle(lim, DialectChat)
	observeAll(t, l4, start)
	observeAll(t, l4, BlockOpen{Item: "r0", Kind: BlockRefusal})
	observeAll(t, l4, TextFragment{Item: "r0", Text: "I cannot"})
	observeAll(t, l4, BlockClose{Item: "r0"}, usage, complete)
	res, rerr := l4.Result()
	if rerr != nil {
		t.Fatalf("Result with refusal block: %v", rerr)
	}
	if len(res.Blocks) != 1 || res.Blocks[0].Kind != BlockRefusal {
		t.Fatalf("refusal block kind lost: %+v", res.Blocks)
	}

	// Happy path: hashes prove content flowed through validation
	// without retention (TextBytes counted, bytes not stored).
	if res.Blocks[0].TextBytes != int64(len("I cannot")) {
		t.Fatalf("text bytes not counted: %+v", res.Blocks[0])
	}
	if res.Blocks[0].TextHash == ([32]byte{}) {
		t.Error("text hash not folded")
	}
}

// --- Empty fragments: allowed; only source decoders may forbid -------------------

func TestResponseIRLifecycleEmptyFragments(t *testing.T) {
	l, _ := lifecycleStart(t, DialectChat)
	observeAll(t, l, BlockOpen{Item: "t0", Kind: BlockText})
	// Correction vs the rejected proposal: empty deltas are not a
	// universal order violation.
	observeAll(t, l, TextFragment{Item: "t0", Text: ""}, TextFragment{Item: "t0", Text: "hi"})
	observeAll(t, l, BlockClose{Item: "t0"})
	observeAll(t, l, BlockOpen{Item: "i1", Kind: BlockTool, Call: "c1", Name: "f"})
	observeAll(t, l, ArgsFragment{Item: "i1", Fragment: ""})
	observeAll(t, l, BlockClose{Item: "i1"},
		UsageEvent{Usage: mustTerminalUsage(t, 5, 2)},
		CompletionEvent{Finish: mustFinish(t, FinishStop)})
	if _, rerr := l.Result(); rerr != nil {
		t.Fatalf("Result with empty fragments: %v", rerr)
	}
}

// --- Terminal contract: completion + authoritative usage --------------------------

func TestResponseIRLifecycleTerminal(t *testing.T) {
	complete, _ := NewCompletionEvent(mustFinish(t, FinishLength))

	// Truncated: no completion, no usage.
	l := NewResponseLifecycle(testRespLimits(), DialectChat)
	observeAll(t, l, ResponseStart{ID: "r1", Model: "m", Source: DialectChat},
		BlockOpen{Item: "t0", Kind: BlockText}, BlockClose{Item: "t0"})
	if _, rerr := l.Result(); rerr == nil {
		t.Error("truncated stream assembled a success")
	}
	// Completion without terminal usage: zeros would be fabricated, so fail.
	l2 := NewResponseLifecycle(testRespLimits(), DialectChat)
	observeAll(t, l2, ResponseStart{ID: "r1", Model: "m", Source: DialectChat},
		BlockOpen{Item: "t0", Kind: BlockText}, BlockClose{Item: "t0"}, complete)
	if _, rerr := l2.Result(); rerr == nil {
		t.Error("completion without usage assembled a success")
	}
	// Duplicate terminal usage fails.
	l3 := NewResponseLifecycle(testRespLimits(), DialectChat)
	usage := UsageEvent{Usage: mustTerminalUsage(t, 5, 2)}
	observeAll(t, l3, ResponseStart{ID: "r1", Model: "m", Source: DialectChat},
		BlockOpen{Item: "t0", Kind: BlockText}, BlockClose{Item: "t0"}, usage)
	if rerr := l3.Observe(usage); rerr == nil {
		t.Error("duplicate terminal usage accepted")
	}
	// Multiple cumulative updates are valid (audit: no universal
	// usage-once restriction).
	cum := mustTerminalUsage(t, 5, 2)
	cum.Provenance.Scope = UsageCumulative
	observeAll(t, l3, UsageEvent{Usage: cum}, UsageEvent{Usage: cum})
	observeAll(t, l3, complete)
	if _, rerr := l3.Result(); rerr != nil {
		t.Fatalf("Result after cumulative updates: %v", rerr)
	}
	// FailureEvent completes the lifecycle without success.
	l4 := NewResponseLifecycle(testRespLimits(), DialectChat)
	observeAll(t, l4, ResponseStart{ID: "r1", Model: "m", Source: DialectChat})
	observeAll(t, l4, FailureEvent{Code: CodeUpstreamBad, Field: "response.output"})
	if !l4.Completed() {
		t.Error("FailureEvent did not complete the lifecycle")
	}
	if _, rerr := l4.Result(); rerr == nil {
		t.Error("failed stream assembled a success")
	}
}

// --- Tool identity conflicts ------------------------------------------------------

func TestResponseIRLifecycleToolConflicts(t *testing.T) {
	l, _ := lifecycleStart(t, DialectChat)
	observeAll(t, l, BlockOpen{Item: "i1", Kind: BlockTool, Call: "dup", Name: "f"})
	if rerr := l.Observe(BlockOpen{Item: "i2", Kind: BlockTool, Call: "dup", Name: "g"}); rerr == nil {
		t.Error("duplicate call ID accepted")
	}
	// Block count bound.
	lim := testRespLimits()
	l2 := NewResponseLifecycle(lim, DialectChat)
	observeAll(t, l2, ResponseStart{ID: "r1", Model: "m", Source: DialectChat})
	for i := 0; i < lim.MaxStreamBlocks; i++ {
		observeAll(t, l2, BlockOpen{Item: ItemID("t" + string(rune('0'+i))), Kind: BlockText}, BlockClose{Item: ItemID("t" + string(rune('0'+i)))})
	}
	if rerr := l2.Observe(BlockOpen{Item: "extra", Kind: BlockText}); rerr == nil {
		t.Error("block over MaxStreamBlocks accepted")
	}
	// Tool count bound at open time.
	l3 := NewResponseLifecycle(lim, DialectChat)
	observeAll(t, l3, ResponseStart{ID: "r1", Model: "m", Source: DialectChat})
	for i := 0; i < lim.MaxToolCalls; i++ {
		id := ItemID("c" + string(rune('a'+i)))
		observeAll(t, l3, BlockOpen{Item: id, Kind: BlockTool, Call: CallID("call" + string(rune('a'+i))), Name: "f"})
		observeAll(t, l3, BlockClose{Item: id})
	}
	if rerr := l3.Observe(BlockOpen{Item: "overflow", Kind: BlockTool, Call: "callz", Name: "f"}); rerr == nil {
		t.Error("tool call over MaxToolCalls accepted")
	}
}

// --- No whole-generation retention in the validator ----------------------------------

func TestResponseIRLifecycleNoTextRetention(t *testing.T) {
	lim := testRespLimits()
	l := NewResponseLifecycle(Limits{
		MaxArgsChars: lim.MaxArgsChars, MaxStreamArgsChars: 1 << 30,
		MaxToolCalls: lim.MaxToolCalls, MaxStreamBlocks: lim.MaxStreamBlocks,
		MaxFrameBytes: lim.MaxFrameBytes,
	}, DialectChat)
	observeAll(t, l, ResponseStart{ID: "r1", Model: "m", Source: DialectChat},
		BlockOpen{Item: "t0", Kind: BlockText})
	// 2MB of text in one block: the rejected proposal capped each text
	// block at MaxArgsChars (1MB). The lifecycle retains nothing, so no
	// text content bound applies here at all.
	big := strings.Repeat("z", 2*1024*1024)
	for i := 0; i < len(big); i += 32 * 1024 {
		end := i + 32*1024
		if end > len(big) {
			end = len(big)
		}
		observeAll(t, l, TextFragment{Item: "t0", Text: big[i:end]})
	}
	observeAll(t, l, BlockClose{Item: "t0"},
		UsageEvent{Usage: mustTerminalUsage(t, 5, 2)},
		CompletionEvent{Finish: mustFinish(t, FinishStop)})
	res, rerr := l.Result()
	if rerr != nil {
		t.Fatalf("large text lifecycle: %v", rerr)
	}
	if res.Blocks[0].TextBytes != int64(len(big)) {
		t.Fatalf("text bytes miscounted: %d", res.Blocks[0].TextBytes)
	}
}

// --- Target-owned TerminalBuffer: inherited bounds, no new text cap ------------------

func TestResponseIRTerminalBufferBounds(t *testing.T) {
	lim := testRespLimits()
	// Per-args-field cap inherited (MaxArgsChars).
	b := NewTerminalBuffer(lim)
	if rerr := b.ObserveArgs("i", strings.Repeat("x", lim.MaxArgsChars+1)); rerr == nil {
		t.Error("args over MaxArgsChars accepted")
	}
	// Shared text+args aggregate inherited (MaxStreamArgsChars):
	// retained text counts against the same aggregate (existing
	// translators share it), with NO per-text-block cap.
	b2 := NewTerminalBuffer(lim)
	half := strings.Repeat("y", lim.MaxStreamArgsChars/2)
	if rerr := b2.ObserveText("t0", half); rerr != nil {
		t.Fatalf("text under aggregate: %v", rerr)
	}
	if rerr := b2.ObserveText("t0", half); rerr != nil {
		t.Fatalf("text at aggregate: %v", rerr)
	}
	if rerr := b2.ObserveText("t0", "overflow"); rerr == nil {
		t.Error("text over the shared aggregate accepted")
	}
	// A single text block larger than MaxArgsChars is FINE when the
	// aggregate allows it: no new 1MB text cap (correction vs the
	// rejected proposal, which capped each text block at MaxArgsChars).
	big := Limits{MaxArgsChars: 64, MaxStreamArgsChars: 1 << 20, MaxToolCalls: 4, MaxStreamBlocks: 4, MaxFrameBytes: 1024}
	b3 := NewTerminalBuffer(big)
	if rerr := b3.ObserveText("t0", strings.Repeat("z", 1024)); rerr != nil {
		t.Fatalf("text over MaxArgsChars wrongly capped: %v", rerr)
	}
	// Args accumulation validates as complete JSON at block end.
	b4 := NewTerminalBuffer(lim)
	if rerr := b4.ObserveArgs("i1", `{"a":`); rerr != nil {
		t.Fatalf("args fragment: %v", rerr)
	}
	if rerr := b4.ObserveArgs("i1", `1}`); rerr != nil {
		t.Fatalf("args fragment: %v", rerr)
	}
	got, rerr := b4.ArgsResult("i1")
	if rerr != nil {
		t.Fatalf("ArgsResult: %v", rerr)
	}
	if string(got) != `{"a":1}` {
		t.Fatalf("args = %s", got)
	}
	if _, rerr := b4.ArgsResult("missing"); rerr != nil {
		t.Fatalf("absent args must mean {}: %v", rerr)
	}
	b5 := NewTerminalBuffer(lim)
	if rerr := b5.ObserveArgs("i1", `[1,2`); rerr != nil {
		t.Fatalf("args fragment: %v", rerr)
	}
	if _, rerr := b5.ArgsResult("i1"); rerr == nil {
		t.Error("non-object args accepted at block end")
	}
}

// --- Source provenance: start must agree with configured source -----------------------

func TestResponseIRLifecycleSourceProvenance(t *testing.T) {
	l := NewResponseLifecycle(testRespLimits(), DialectChat)
	if rerr := l.Observe(ResponseStart{ID: "r1", Model: "m", Source: DialectGemini}); rerr == nil {
		t.Error("provenance mismatch accepted")
	}
	if l.Completed() {
		t.Error("provenance mismatch marked completed")
	}
	l2 := NewResponseLifecycle(testRespLimits(), DialectChat)
	observeAll(t, l2, ResponseStart{ID: "r1", Model: "m", Source: DialectChat})
	if rerr := l2.Observe(ResponseStart{ID: "r2", Model: "m", Source: DialectChat}); rerr == nil {
		t.Error("duplicate start accepted")
	}
}

// --- FailureEvent: validated terminal, never nil success ------------------------------

func TestResponseIRLifecycleFailureValidation(t *testing.T) {
	// Empty Code/Field never becomes a terminal.
	l := NewResponseLifecycle(testRespLimits(), DialectChat)
	observeAll(t, l, ResponseStart{ID: "r1", Model: "m", Source: DialectChat})
	if rerr := l.Observe(FailureEvent{}); rerr == nil {
		t.Error("empty FailureEvent accepted as terminal")
	}
	if l.Completed() || l.Failed() {
		t.Error("empty FailureEvent marked completed/failed")
	}
	// Pre-start FailureEvent rejected.
	l2 := NewResponseLifecycle(testRespLimits(), DialectChat)
	if rerr := l2.Observe(FailureEvent{Code: CodeUpstreamBad, Field: "response.output"}); rerr == nil {
		t.Error("pre-start FailureEvent accepted")
	}
	if l2.Completed() {
		t.Error("pre-start FailureEvent marked completed")
	}
	// Valid FailureEvent completes as failure, never success.
	l3 := NewResponseLifecycle(testRespLimits(), DialectChat)
	observeAll(t, l3, ResponseStart{ID: "r1", Model: "m", Source: DialectChat})
	observeAll(t, l3, FailureEvent{Code: CodeUpstreamBad, Field: "response.output"})
	if !l3.Completed() || !l3.Failed() {
		t.Fatal("valid FailureEvent did not mark failed completion")
	}
	if l3.Failure() == nil {
		t.Fatal("Failure() nil after validated failure")
	}
	if _, rerr := l3.Result(); rerr == nil {
		t.Error("failed stream assembled a success")
	}
	// Sparse/string IDs, empty deltas, cumulative usage still preserved.
	l4 := NewResponseLifecycle(testRespLimits(), DialectChat)
	observeAll(t, l4, ResponseStart{ID: "r1", Model: "m", Source: DialectChat})
	observeAll(t, l4, BlockOpen{Item: "z-9", Kind: BlockText}, TextFragment{Item: "z-9", Text: ""})
	observeAll(t, l4, BlockClose{Item: "z-9"})
	cum := mustTerminalUsage(t, 5, 2)
	cum.Provenance.Scope = UsageCumulative
	observeAll(t, l4, UsageEvent{Usage: cum}, UsageEvent{Usage: cum})
	observeAll(t, l4, UsageEvent{Usage: mustTerminalUsage(t, 5, 2)},
		CompletionEvent{Finish: mustFinish(t, FinishStop)})
	if _, rerr := l4.Result(); rerr != nil {
		t.Fatalf("sparse IDs/empty deltas/cumulative updates: %v", rerr)
	}
}

// --- SHA256 integrity: distinct content, distinct hashes -------------------------------

func TestResponseIRLifecycleSHA256(t *testing.T) {
	finish := func() CompletionEvent {
		c, _ := NewCompletionEvent(mustFinish(t, FinishStop))
		return c
	}
	build := func(text string) [32]byte {
		l := NewResponseLifecycle(testRespLimits(), DialectChat)
		observeAll(t, l, ResponseStart{ID: "r1", Model: "m", Source: DialectChat},
			BlockOpen{Item: "t0", Kind: BlockText}, TextFragment{Item: "t0", Text: text},
			BlockClose{Item: "t0"}, UsageEvent{Usage: mustTerminalUsage(t, 5, 2)}, finish())
		res, rerr := l.Result()
		if rerr != nil {
			t.Fatalf("Result: %v", rerr)
		}
		return res.Blocks[0].TextHash
	}
	if build("hello") == ([32]byte{}) {
		t.Error("SHA256 not folded")
	}
	if build("hello") == build("world") {
		t.Error("distinct text shares integrity hash")
	}
}

// --- TerminalBuffer independent bounds: overflow-safe, id/kind/count -------------------

func TestResponseIRTerminalBufferIndependentBounds(t *testing.T) {
	lim := testRespLimits()
	// Invalid item syntax rejected independently.
	b := NewTerminalBuffer(lim)
	if rerr := b.ObserveText("", "hi"); rerr == nil {
		t.Error("empty item accepted by TerminalBuffer")
	}
	// Kind coherence: same item cannot be both text and tool.
	b2 := NewTerminalBuffer(lim)
	observeBufferText(t, b2, "i1", "hi")
	if rerr := b2.ObserveArgs("i1", `{"a":1}`); rerr == nil {
		t.Error("text/tool kind confusion accepted")
	}
	// Distinct-block bound enforced independently.
	b3 := NewTerminalBuffer(Limits{MaxArgsChars: 64, MaxStreamArgsChars: 1 << 20, MaxToolCalls: 100, MaxStreamBlocks: 1, MaxFrameBytes: 1024})
	observeBufferText(t, b3, "t0", "hi")
	if rerr := b3.ObserveText("t1", "hi"); rerr == nil {
		t.Error("TerminalBuffer ignored MaxStreamBlocks")
	}
	// Tool-call bound enforced independently.
	b4 := NewTerminalBuffer(Limits{MaxArgsChars: 64, MaxStreamArgsChars: 1 << 20, MaxToolCalls: 1, MaxStreamBlocks: 100, MaxFrameBytes: 1024})
	if rerr := b4.ObserveArgs("c1", `{"a":1}`); rerr != nil {
		t.Fatalf("args1: %v", rerr)
	}
	if rerr := b4.ObserveArgs("c2", `{"a":1}`); rerr == nil {
		t.Error("TerminalBuffer ignored MaxToolCalls")
	}
}

func observeBufferText(t *testing.T, b *TerminalBuffer, item ItemID, text string) {
	t.Helper()
	if rerr := b.ObserveText(item, text); rerr != nil {
		t.Fatalf("ObserveText: %v", rerr)
	}
}
