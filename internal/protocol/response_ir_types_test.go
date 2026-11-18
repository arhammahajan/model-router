// Semantic-invariant tests for the canonical Response IR types.
//
// These tests pin the type/validator contract only; they touch no
// server, transport, request-decoder, or pair-translator behavior.
// Comments cite the existing behavior each probe preserves.
package protocol

import (
	"strings"
	"testing"
)

func testRespLimits() Limits {
	return Limits{
		MaxArgsChars:       64,
		MaxStreamArgsChars: 128,
		MaxToolCalls:       4,
		MaxStreamBlocks:    4,
		MaxFrameBytes:      1024,
	}
}

func mustTerminalUsage(t *testing.T, prompt, completion int) ResponseUsage {
	t.Helper()
	u, rerr := NewTerminalUsage(DialectChat, prompt, completion, prompt+completion)
	if rerr != nil {
		t.Fatalf("NewTerminalUsage: %v", rerr)
	}
	return u
}

func observeAll(t *testing.T, l *ResponseLifecycle, evs ...ResponseEvent) {
	t.Helper()
	for _, ev := range evs {
		if rerr := l.Observe(ev); rerr != nil {
			t.Fatalf("Observe(%T): %v", ev, rerr)
		}
	}
}

// --- Opaque identities: bounded nonempty, never a name grammar ---------
//
// Correction vs the rejected proposal: response/item/call IDs are
// provider-minted opaque handles, NOT 1..64 [A-Za-z0-9_-] names.
// validateCallID (chat_decode.go) permits any bounded nonempty opaque
// string, and TestG2CRegrOpaqueCallIDsPreserved pins "has:colon and
// space" plus 100-char IDs. Only TOOL NAMES carry the 64-char grammar.
func TestResponseIRIdentityValidation(t *testing.T) {
	// Opaque IDs reject empty and whitespace-only; they accept spaces,
	// punctuation and long-but-bounded handles.
	for _, s := range []string{"", "  ", "\t\n "} {
		if _, rerr := NewCallID(s); rerr == nil {
			t.Errorf("NewCallID(%q) accepted", s)
		}
		if _, rerr := NewItemID(s); rerr == nil {
			t.Errorf("NewItemID(%q) accepted", s)
		}
		if _, rerr := NewResponseID(s); rerr == nil {
			t.Errorf("NewResponseID(%q) accepted", s)
		}
	}
	for _, s := range []string{"has space", "has:colon and space", "a\nb",
		"call_1", "toolu_abc", "ns__fn", "resp_1", "msg_x",
		strings.Repeat("x", MaxIdentityChars+1), strings.Repeat("y", 100)} {
		if _, rerr := NewCallID(s); rerr != nil {
			t.Errorf("NewCallID(%q): %v", s, rerr)
		}
		if _, rerr := NewItemID(s); rerr != nil {
			t.Errorf("NewItemID(%q): %v", s, rerr)
		}
		if _, rerr := NewResponseID(s); rerr != nil {
			t.Errorf("NewResponseID(%q): %v", s, rerr)
		}
	}
	// Tool names keep the 1..64 grammar: opaque handles are not names.
	for _, s := range []string{"", "  ", "has space", "a\nb", strings.Repeat("x", MaxIdentityChars+1)} {
		if _, rerr := NewToolName(s); rerr == nil {
			t.Errorf("NewToolName(%q) accepted", s)
		}
	}
	for _, s := range []string{"call_1", "toolu_abc", "ns__fn", "resp_1", "msg_x"} {
		if _, rerr := NewToolName(s); rerr != nil {
			t.Errorf("NewToolName(%q): %v", s, rerr)
		}
	}
}

// --- Exact raw args: objects only, numbers preserved -------------------------

func TestResponseIRCompactArgs(t *testing.T) {
	raw := `{"n": 9007199254740993, "b": true}`
	got, rerr := CompactArgs([]byte(raw))
	if rerr != nil {
		t.Fatalf("CompactArgs: %v", rerr)
	}
	if string(got) != `{"n":9007199254740993,"b":true}` {
		t.Fatalf("args not preserved exactly: %s", got)
	}
	for _, bad := range []string{`[1]`, `"s"`, `42`, `null`, `{oops`, ``} {
		if _, rerr := CompactArgs([]byte(bad)); rerr == nil {
			t.Errorf("CompactArgs(%q) accepted", bad)
		}
	}
	if _, rerr := NewToolCallItem("i1", "c1", "f", []byte("")); rerr == nil {
		t.Error("empty args string accepted; decoders must use EmptyArgs explicitly")
	}
	item, rerr := NewToolCallItem("i1", "c1", "f", EmptyArgs())
	if rerr != nil {
		t.Fatalf("NewToolCallItem with EmptyArgs: %v", rerr)
	}
	if string(item.Arguments) != "{}" {
		t.Fatalf("EmptyArgs = %s", item.Arguments)
	}
	// Malformed literals stay constructible but NewResponse rejects them.
	bad := ToolCallItem{ID: "i1", CallID: "c1", Name: "f", Arguments: []byte(`[1]}`)}
	if _, rerr := NewResponse("r1", "m", DialectChat, []ResponseItem{bad}, mustTerminalUsage(t, 5, 2), mustFinish(t, FinishStop)); rerr == nil {
		t.Error("NewResponse accepted non-object tool args literal")
	}
}

func mustFinish(t *testing.T, kind FinishKind) Finish {
	t.Helper()
	f, rerr := NewFinish(kind, string(kind))
	if rerr != nil {
		t.Fatalf("NewFinish(%q): %v", kind, rerr)
	}
	return f
}

// --- Closed finish: distinctions preserved, unknowns fail ---------------------

func TestResponseIRFinishClosed(t *testing.T) {
	if _, rerr := NewFinish("done", "done"); rerr == nil {
		t.Error("unknown finish kind accepted")
	}
	if _, rerr := NewFinish("", ""); rerr == nil {
		t.Error("empty finish kind accepted")
	}
	// Every preserved kind constructs; refusal and tool-calls stay
	// distinct from stop (audit D6: filter/refusal/tool-call finishes
	// must not coarsen into completed/truncated).
	seen := map[FinishKind]bool{}
	for _, k := range []FinishKind{FinishStop, FinishLength, FinishContentFilter, FinishRefusal, FinishToolCalls} {
		f, rerr := NewFinish(k, "raw-"+string(k))
		if rerr != nil {
			t.Fatalf("NewFinish(%q): %v", k, rerr)
		}
		if f.Raw != "raw-"+string(k) {
			t.Fatalf("finish raw provenance lost: %+v", f)
		}
		seen[f.Kind] = true
	}
	if len(seen) != 5 {
		t.Fatalf("finish kinds coarsened: %v", seen)
	}
	if _, rerr := NewCompletionEvent(Finish{Kind: "other"}); rerr == nil {
		t.Error("unknown finish accepted by NewCompletionEvent")
	}
	out, rerr := NewCompletionEvent(mustFinish(t, FinishRefusal))
	if rerr != nil {
		t.Fatalf("refused completion: %v", rerr)
	}
	if out.Finish.Kind != FinishRefusal {
		t.Fatalf("refusal not explicit: %+v", out.Finish)
	}
}

// --- Event constructors: coherence checked, emptiness source-owned -------------

func TestResponseIREventConstructors(t *testing.T) {
	if _, rerr := NewBlockOpen("", BlockTool, "c", "f"); rerr == nil {
		t.Error("tool block without item identity accepted")
	}
	if _, rerr := NewBlockOpen("i", BlockTool, "", "f"); rerr == nil {
		t.Error("tool block without call identity accepted")
	}
	if _, rerr := NewBlockOpen("i", BlockTool, "c", ""); rerr == nil {
		t.Error("tool block without name accepted")
	}
	if _, rerr := NewBlockOpen("i", BlockText, "c", ""); rerr == nil {
		t.Error("text block carrying a tool identity accepted")
	}
	if _, rerr := NewBlockOpen("i", BlockRefusal, "c", ""); rerr == nil {
		t.Error("refusal block carrying a tool identity accepted")
	}
	if _, rerr := NewBlockOpen("i", BlockKind("media"), "", ""); rerr == nil {
		t.Error("unknown block kind accepted")
	}
	// Empty fragments are ACCEPTED at the type boundary (correction vs
	// the rejected proposal): only the source contract may forbid
	// them. The lifecycle pins the same allowance.
	if _, rerr := NewTextFragment("i", ""); rerr != nil {
		t.Errorf("empty text fragment rejected at type boundary: %v", rerr)
	}
	if _, rerr := NewArgsFragment("i", ""); rerr != nil {
		t.Errorf("empty args fragment rejected at type boundary: %v", rerr)
	}
	if _, rerr := NewTextFragment("", "hi"); rerr == nil {
		t.Error("fragment without block identity accepted")
	}
}

// --- NewResponse: complete-decoder validation matches streaming -----------------

func TestResponseIRNewResponse(t *testing.T) {
	id, _ := NewResponseID("r1")
	usage := mustTerminalUsage(t, 5, 2)
	items := []ResponseItem{TextItem{ID: "t0", Text: "hi"}, RefusalItem{ID: "r0", Text: "no"}}
	if _, rerr := NewResponse(id, "m", DialectChat, items, usage, mustFinish(t, FinishStop)); rerr != nil {
		t.Fatalf("valid response: %v", rerr)
	}
	// Empty text spans are representable and preserved.
	empty := []ResponseItem{TextItem{ID: "t0", Text: ""}}
	resp, rerr := NewResponse(id, "m", DialectChat, empty, usage, mustFinish(t, FinishStop))
	if rerr != nil {
		t.Fatalf("empty text span: %v", rerr)
	}
	if tx, ok := resp.Items[0].(TextItem); !ok || tx.Text != "" {
		t.Fatalf("empty text span disappeared: %#v", resp.Items[0])
	}
	dup := []ResponseItem{TextItem{ID: "t0", Text: "a"}, TextItem{ID: "t0", Text: "b"}}
	if _, rerr := NewResponse(id, "m", DialectChat, dup, usage, mustFinish(t, FinishStop)); rerr == nil {
		t.Error("duplicate item IDs accepted")
	}
	c1, _ := NewToolCallItem("i1", "c", "f", EmptyArgs())
	c2, _ := NewToolCallItem("i2", "c", "g", EmptyArgs())
	if _, rerr := NewResponse(id, "m", DialectChat, []ResponseItem{c1, c2}, usage, mustFinish(t, FinishStop)); rerr == nil {
		t.Error("duplicate call IDs accepted")
	}
	if _, rerr := NewResponse("", "m", DialectChat, items, usage, mustFinish(t, FinishStop)); rerr == nil {
		t.Error("empty response ID accepted")
	}
	if _, rerr := NewResponse(id, "", DialectChat, items, usage, mustFinish(t, FinishStop)); rerr == nil {
		t.Error("empty model accepted")
	}
	if _, rerr := NewResponse(id, "m", DialectChat, items, ResponseUsage{}, mustFinish(t, FinishStop)); rerr == nil {
		t.Error("non-authoritative usage accepted")
	}
	if _, rerr := NewResponse(id, "m", DialectChat, items, usage, Finish{Kind: "bogus"}); rerr == nil {
		t.Error("unknown finish accepted")
	}
}

func TestResponseIRAbsentModelOrigin(t *testing.T) {
	usage, err := NewTerminalUsage(DialectGemini, 1, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := NewResponse("router-id", "", DialectGemini, nil, usage, mustFinish(t, FinishStop))
	if err != nil {
		t.Fatal(err)
	}
	if resp.ModelOrigin != IdentityRouterMinted {
		t.Fatalf("absent model origin = %q, want router", resp.ModelOrigin)
	}
}
