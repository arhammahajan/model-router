// Independent review probes: Gemini stream decoder + pipeline +
// shared framing + four target encoders + lifecycle Result only.
package protocol

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// helpers (review-local, no legacy reuse)
func rvGSSE(s string) Frame { return Frame{Data: s} }

func rvDecodeAll(t *testing.T, sse bool, datas []string) ([]ResponseEvent, *GeminiStreamDecoder) {
	t.Helper()
	dec := NewGeminiStreamDecoder(DefaultLimits(), sse)
	var all []ResponseEvent
	for i, d := range datas {
		evs, rerr := dec.DecodeFrame(Frame{Data: d})
		if rerr != nil {
			t.Fatalf("DecodeFrame[%d]: %v", i, rerr)
		}
		all = append(all, evs...)
	}
	return all, dec
}

func rvObserve(t *testing.T, evs []ResponseEvent) LifecycleResult {
	t.Helper()
	life := NewResponseLifecycle(DefaultLimits(), DialectGemini)
	for i, ev := range evs {
		if rerr := life.Observe(ev); rerr != nil {
			t.Fatalf("Observe[%d](%T): %v", i, ev, rerr)
		}
	}
	res, rerr := life.Result()
	if rerr != nil {
		t.Fatalf("Result: %v", rerr)
	}
	return res
}

type rvCountFrames struct {
	frames []Frame
	next   int
}

func (s *rvCountFrames) Next() (Frame, *RouterError) {
	if s.next >= len(s.frames) {
		return Frame{}, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream ended without completion"}
	}
	f := s.frames[s.next]
	s.next++
	return f, nil
}

type rvFailWriter struct {
	n         int
	failAfter int
	writes    int
}

func (w *rvFailWriter) Write(p []byte) (int, error) {
	w.n++
	if w.n > w.failAfter {
		return 0, io.ErrClosedPipe
	}
	w.writes += len(p)
	return len(p), nil
}

func TestReviewGeminiStreamAbsentStartHonest(t *testing.T) {
	term := gsBuild(gsTextPart("hi"), "STOP", gsUsage(2, 1), "")
	all, dec := rvDecodeAll(t, true, []string{term})
	start, ok := all[0].(ResponseStart)
	if !ok {
		t.Fatalf("ev0 = %#v, want ResponseStart", all[0])
	}
	if start.Model != "" {
		t.Fatalf("absent model = %q, want empty (no fake provider string)", start.Model)
	}
	if start.IDOrigin != IdentityRouterMinted || start.ModelOrigin != IdentityRouterMinted {
		t.Fatalf("origins = %q/%q, want router/router", start.IDOrigin, start.ModelOrigin)
	}
	if string(start.ID) != geminiCompatResponseID {
		t.Fatalf("minted id = %q", string(start.ID))
	}
	if tail, rerr := dec.Close(); rerr != nil || len(tail) != 0 {
		t.Fatalf("Close = %v/%d", rerr, len(tail))
	}
	res := rvObserve(t, all)
	if res.Model != "" || res.IDOrigin != IdentityRouterMinted || res.ModelOrigin != IdentityRouterMinted {
		t.Fatalf("Result = %+v, want empty model + router origins", res)
	}
	// Public alias stable downstream: provider strings never leak.
	var buf bytes.Buffer
	enc := NewChatEventEncoder("public-alias", true, DefaultLimits())
	for _, ev := range all {
		if eerr := enc.Encode(&buf, nil, ev); eerr != nil {
			t.Fatalf("chat encode: %v", eerr.Err)
		}
	}
	if !strings.Contains(buf.String(), "public-alias") {
		t.Fatalf("public alias missing: %s", buf.String())
	}
	if strings.Contains(buf.String(), geminiCompatResponseID) {
		t.Fatalf("minted id leaked downstream: %s", buf.String())
	}
}

func TestReviewGeminiStreamLateEqualMintedFlipsProvider(t *testing.T) {
	c0 := gsBuild(gsTextPart("a"), "", "", "")
	c1 := gsBuild(gsTextPart("b"), "STOP", gsUsage(3, 1),
		`"responseId":"`+geminiCompatResponseID+`"`)
	dec := NewGeminiStreamDecoder(DefaultLimits(), true)
	e0, rerr := dec.DecodeFrame(rvGSSE(c0))
	if rerr != nil {
		t.Fatal(rerr)
	}
	e1, rerr := dec.DecodeFrame(rvGSSE(c1))
	if rerr != nil {
		t.Fatal(rerr)
	}
	all := append(e0, e1...)
	found := false
	for _, ev := range all {
		if pi, ok := ev.(ProviderIdentityEvent); ok {
			found = true
			if string(pi.ID) != geminiCompatResponseID {
				t.Fatalf("identity = %q", string(pi.ID))
			}
		}
	}
	if !found {
		t.Fatalf("missing late identity for equal-minted value, kinds=%v", eventKinds(all))
	}
	res := rvObserve(t, all)
	if res.IDOrigin != IdentityProvider {
		t.Fatalf("origin = %q, want provider after late equal flip", res.IDOrigin)
	}
	if len(res.RouterMintedCalls) != 0 {
		t.Fatalf("equal provider value entered minted set: %v", res.RouterMintedCalls)
	}
	// Gemini terminal echoes provider-supplied compat string.
	gem := NewGeminiEventEncoder("public", DefaultLimits(), true)
	var buf bytes.Buffer
	for _, ev := range all {
		if eerr := gem.Encode(&buf, nil, ev); eerr != nil {
			t.Fatalf("gem encode: %v", eerr.Err)
		}
	}
	if !strings.Contains(buf.String(), `"responseId":"`+geminiCompatResponseID+`"`) {
		t.Fatalf("provider compat id not echoed: %s", buf.String())
	}
}

func TestReviewGeminiStreamCallCollisionBothDirections(t *testing.T) {
	// Two calls in ONE chunk with the same supplied id.
	dupPart := `{"candidates":[{"content":{"role":"model","parts":[` +
		gsToolPart("a", "dup1", `{"x":1}`) + `,` + gsToolPart("b", "dup1", `{"x":1}`) +
		`],"index":0}],"usageMetadata":` + gsUsage(1, 1) + `}`
	if _, rerr := NewGeminiStreamDecoder(DefaultLimits(), true).DecodeFrame(rvGSSE(dupPart)); rerr == nil {
		t.Fatal("same-chunk duplicate call id accepted")
	}
	// Minted first, supplied collides later.
	dec := NewGeminiStreamDecoder(DefaultLimits(), true)
	mintFirst := gsBuild(gsToolPart("a", "", `{"x":1}`), "", "", "")
	if _, rerr := dec.DecodeFrame(rvGSSE(mintFirst)); rerr != nil {
		t.Fatal(rerr)
	}
	collide := gsBuild(gsToolPart("b", "gemini-compat-call-0", `{"x":1}`), "", "", "")
	if _, rerr := dec.DecodeFrame(rvGSSE(collide)); rerr == nil {
		t.Fatal("supplied id colliding with minted accepted")
	}
	// Supplied first, minted skips it (steers, never collides).
	dec2 := NewGeminiStreamDecoder(DefaultLimits(), true)
	s0 := gsBuild(gsToolPart("a", "gemini-compat-call-0", `{"x":1}`), "", "", "")
	m1 := gsBuild(gsToolPart("b", "", `{"x":1}`), "STOP", gsUsage(2, 2), "")
	var ids []string
	for _, d := range []string{s0, m1} {
		evs, rerr := dec2.DecodeFrame(rvGSSE(d))
		if rerr != nil {
			t.Fatal(rerr)
		}
		for _, ev := range evs {
			if o, ok := ev.(BlockOpen); ok && o.Kind == BlockTool {
				ids = append(ids, string(o.Call))
				if o.Call == "gemini-compat-call-0" && o.CallOrigin != IdentityProvider {
					t.Fatalf("supplied compat prefix origin = %q, want provider", o.CallOrigin)
				}
			}
		}
	}
	if len(ids) != 2 || ids[0] != "gemini-compat-call-0" || ids[1] == "gemini-compat-call-0" {
		t.Fatalf("mint did not steer past supplied: %v", ids)
	}
}

func TestReviewGeminiStreamArgsExactAndEmptyCounted(t *testing.T) {
	big := `{"n":9007199254740993,"s":"x"}`
	elem := gsBuild(gsToolPart("calc", "c-big", big), "STOP", gsUsage(3, 3), "")
	evs, _ := rvDecodeAll(t, true, []string{elem})
	found := false
	for _, ev := range evs {
		if f, ok := ev.(ArgsFragment); ok {
			found = true
			if f.Fragment != big {
				t.Fatalf("args = %q, want exact %q", f.Fragment, big)
			}
		}
	}
	if !found {
		t.Fatal("no ArgsFragment")
	}
	// Missing args emit exactly {} and count toward the per-call bound.
	lim := Limits{MaxArgsChars: 1, MaxStreamArgsChars: 8000, MaxToolCalls: 4, MaxStreamBlocks: 4, MaxFrameBytes: 8000}
	missing := gsBuild(gsToolPart("t", "c", ""), "", "", "")
	if _, rerr := NewGeminiStreamDecoder(lim, true).DecodeFrame(rvGSSE(missing)); rerr == nil {
		t.Fatal("missing {} over 1-byte per-call bound accepted")
	} else if !strings.Contains(rerr.Msg, "size bound") {
		t.Fatalf("wrong error for {} over per-call bound: %v", rerr)
	}
	// Aggregate: two {} (2 bytes each) over a 3-byte aggregate fails on the second.
	lim2 := Limits{MaxArgsChars: 8000, MaxStreamArgsChars: 3, MaxToolCalls: 4, MaxStreamBlocks: 8, MaxFrameBytes: 8000}
	dec := NewGeminiStreamDecoder(lim2, true)
	first := gsBuild(gsToolPart("a", "c1", ""), "", "", "")
	second := gsBuild(gsToolPart("b", "c2", ""), "", "", "")
	if _, rerr := dec.DecodeFrame(rvGSSE(first)); rerr != nil {
		t.Fatalf("first {}: %v", rerr)
	}
	if _, rerr := dec.DecodeFrame(rvGSSE(second)); rerr == nil {
		t.Fatal("aggregate {} overflow accepted")
	}
}

func TestReviewGeminiStreamBounds(t *testing.T) {
	// MaxToolCalls.
	lim := Limits{MaxArgsChars: 8000, MaxStreamArgsChars: 80000, MaxToolCalls: 1, MaxStreamBlocks: 8, MaxFrameBytes: 8000}
	dec := NewGeminiStreamDecoder(lim, true)
	if _, rerr := dec.DecodeFrame(rvGSSE(gsBuild(gsToolPart("a", "c1", `{"x":1}`), "", "", ""))); rerr != nil {
		t.Fatal(rerr)
	}
	if _, rerr := dec.DecodeFrame(rvGSSE(gsBuild(gsToolPart("b", "c2", `{"x":1}`), "", "", ""))); rerr == nil {
		t.Fatal("second tool over MaxToolCalls accepted")
	}
	// MaxStreamBlocks: two text runs + one tool = 3 blocks over cap 2.
	limB := Limits{MaxArgsChars: 8000, MaxStreamArgsChars: 80000, MaxToolCalls: 8, MaxStreamBlocks: 2, MaxFrameBytes: 8000}
	decB := NewGeminiStreamDecoder(limB, true)
	if _, rerr := decB.DecodeFrame(rvGSSE(gsBuild(gsTextPart("a"), "", "", ""))); rerr != nil {
		t.Fatal(rerr)
	}
	if _, rerr := decB.DecodeFrame(rvGSSE(gsBuild(gsToolPart("t", "c1", `{"x":1}`), "", "", ""))); rerr != nil {
		t.Fatal(rerr)
	}
	if _, rerr := decB.DecodeFrame(rvGSSE(gsBuild(gsTextPart("b"), "", "", ""))); rerr == nil {
		t.Fatal("third block over MaxStreamBlocks accepted")
	}
	// Opaque responseId over MaxArgsChars.
	limI := Limits{MaxArgsChars: 4, MaxStreamArgsChars: 80000, MaxToolCalls: 4, MaxStreamBlocks: 8, MaxFrameBytes: 8000}
	bigID := gsBuild(gsTextPart("hi"), "", "", `"responseId":"12345"`)
	if _, rerr := NewGeminiStreamDecoder(limI, true).DecodeFrame(rvGSSE(bigID)); rerr == nil {
		t.Fatal("oversize responseId accepted")
	}
	// Pending pre-start identity over the aggregate.
	limP := Limits{MaxArgsChars: 100, MaxStreamArgsChars: 5, MaxToolCalls: 4, MaxStreamBlocks: 4, MaxFrameBytes: 8000}
	pend := gsBuild(gsTextPart("hi"), "", "", `"responseId":"12345678901234567890"`)
	if _, rerr := NewGeminiStreamDecoder(limP, true).DecodeFrame(rvGSSE(pend)); rerr == nil {
		t.Fatal("pending identity overflow accepted")
	}
}

func TestReviewGeminiStreamTerminalAndMetadataGates(t *testing.T) {
	term := gsBuild(gsTextPart("hi"), "STOP", gsUsage(1, 1), "")
	dec := NewGeminiStreamDecoder(DefaultLimits(), true)
	if _, rerr := dec.DecodeFrame(rvGSSE(term)); rerr != nil {
		t.Fatal(rerr)
	}
	if _, rerr := dec.DecodeFrame(rvGSSE(term)); rerr == nil {
		t.Fatal("post-terminal frame accepted")
	} else if !strings.Contains(rerr.Msg, "after completion") {
		t.Fatalf("post-terminal msg = %q", rerr.Msg)
	}
	if _, rerr := dec.DecodeFrame(Frame{Closed: true}); rerr == nil {
		t.Fatal("Closed frame accepted in SSE mode")
	}
	// Double identity: same ID twice in later chunks fails at lifecycle (decoder emits once).
	c0 := gsBuild(gsTextPart("a"), "", "", "")
	c1 := gsBuild(gsTextPart("b"), "", "", `"responseId":"r1"`)
	dec2 := NewGeminiStreamDecoder(DefaultLimits(), true)
	e0, _ := dec2.DecodeFrame(rvGSSE(c0))
	e1, _ := dec2.DecodeFrame(rvGSSE(c1))
	all := append(e0, e1...)
	count := 0
	for _, ev := range all {
		if _, ok := ev.(ProviderIdentityEvent); ok {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("identity events = %d, want exactly 1", count)
	}
	life := NewResponseLifecycle(DefaultLimits(), DialectGemini)
	for _, ev := range all {
		if rerr := life.Observe(ev); rerr != nil {
			t.Fatalf("observe: %v", rerr)
		}
	}
	if rerr := life.Observe(ProviderIdentityEvent{ID: "r1"}); rerr == nil {
		t.Fatal("duplicate ProviderIdentityEvent accepted by lifecycle")
	}
	// Model drift is value-free.
	dec3 := NewGeminiStreamDecoder(DefaultLimits(), true)
	if _, rerr := dec3.DecodeFrame(rvGSSE(gsBuild(gsTextPart("a"), "", "", `"modelVersion":"m1"`))); rerr != nil {
		t.Fatal(rerr)
	}
	if _, rerr := dec3.DecodeFrame(rvGSSE(gsBuild(gsTextPart("b"), "", "", `"modelVersion":"m2"`))); rerr == nil {
		t.Fatal("model drift accepted")
	} else if strings.Contains(rerr.Msg, "m2") || strings.Contains(rerr.Msg, "m1") {
		t.Fatalf("drift leaks value: %q", rerr.Msg)
	}
	// Array: content after armed finish fails; second finish never succeeds.
	decA := NewGeminiStreamDecoder(DefaultLimits(), false)
	elem := gsBuild(gsTextPart("hi"), "STOP", gsUsage(1, 1), "")
	if _, rerr := decA.DecodeFrame(Frame{Data: elem}); rerr != nil {
		t.Fatal(rerr)
	}
	if _, rerr := decA.DecodeFrame(Frame{Data: gsBuild(gsTextPart("x"), "", "", "")}); rerr == nil {
		t.Fatal("content-after-finish accepted")
	}
}

func TestReviewGeminiStreamMetadataOnlyNeverSucceeds(t *testing.T) {
	// Identity-only object without candidates matches legacy: invalid, never a start.
	if _, rerr := NewGeminiStreamDecoder(DefaultLimits(), true).DecodeFrame(rvGSSE(`{"responseId":"r1"}`)); rerr == nil {
		t.Fatal("identity-only object without candidates accepted")
	}
	// Empty array close without any terminal element fails.
	dec := NewGeminiStreamDecoder(DefaultLimits(), false)
	if _, rerr := dec.DecodeFrame(Frame{Closed: true}); rerr == nil {
		t.Fatal("bare close without terminal accepted")
	}
	// Empty-data keepalive emits nothing and does not start.
	dec2 := NewGeminiStreamDecoder(DefaultLimits(), true)
	if evs, rerr := dec2.DecodeFrame(Frame{Data: ""}); rerr != nil || len(evs) != 0 {
		t.Fatalf("empty data = %v/%d, want nil/0", rerr, len(evs))
	}
	if _, rerr := dec2.Close(); rerr == nil {
		t.Fatal("Close without terminal accepted")
	}
}

func TestReviewGeminiStreamProvenanceRestricted(t *testing.T) {
	life := NewResponseLifecycle(DefaultLimits(), DialectChat)
	if rerr := life.Observe(ResponseStart{ID: "r", Model: "m", Source: DialectChat, IDOrigin: IdentityRouterMinted}); rerr == nil {
		t.Fatal("router origin accepted for Chat")
	}
	lifeG := NewResponseLifecycle(DefaultLimits(), DialectGemini)
	if rerr := lifeG.Observe(ResponseStart{ID: "r", Model: "", Source: DialectChat, IDOrigin: IdentityRouterMinted}); rerr == nil {
		t.Fatal("mismatched source accepted")
	}
	// Provider identity for a non-Gemini generation fails.
	lifeC := NewResponseLifecycle(DefaultLimits(), DialectChat)
	observeAll(t, lifeC, ResponseStart{ID: "r", Model: "m", Source: DialectChat})
	if rerr := lifeC.Observe(ProviderIdentityEvent{ID: "late"}); rerr == nil {
		t.Fatal("provider identity accepted for Chat source")
	}
	// Router-minted call for a non-Gemini source fails.
	lifeC2 := NewResponseLifecycle(DefaultLimits(), DialectChat)
	observeAll(t, lifeC2, ResponseStart{ID: "r", Model: "m", Source: DialectChat})
	if rerr := lifeC2.Observe(BlockOpen{Item: "c0", Kind: BlockTool, Call: "x", Name: "f", CallOrigin: IdentityRouterMinted}); rerr == nil {
		t.Fatal("router CallOrigin accepted for Chat source")
	}
}

func TestReviewGeminiStreamTextRunOrdering(t *testing.T) {
	c0 := gsBuild(gsTextPart("a"), "", "", "")
	c1 := gsBuild(gsToolPart("tt", "cc1", `{"x":1}`), "", "", "")
	c2 := gsBuild(gsTextPart("b"), "STOP", gsUsage(3, 3), "")
	all, _ := rvDecodeAll(t, true, []string{c0, c1, c2})
	var items []ItemID
	for _, ev := range all {
		if o, ok := ev.(BlockOpen); ok {
			items = append(items, o.Item)
		}
	}
	if len(items) != 3 || items[0] != "t0" || items[1] != "c0" || items[2] != "t1" {
		t.Fatalf("block order = %v, want [t0 c0 t1]", items)
	}
	// Repeated text chunks share the same text run.
	r0 := gsBuild(gsTextPart("hel"), "", "", "")
	r1 := gsBuild(gsTextPart("lo"), "STOP", gsUsage(2, 2), "")
	allR, _ := rvDecodeAll(t, true, []string{r0, r1})
	opens := 0
	texts := 0
	for _, ev := range allR {
		if _, ok := ev.(BlockOpen); ok {
			opens++
		}
		if _, ok := ev.(TextFragment); ok {
			texts++
		}
	}
	if opens != 1 || texts != 2 {
		t.Fatalf("repeated text opens=%d texts=%d, want 1/2", opens, texts)
	}
	// Empty text creates no block and does not split the run.
	e0 := gsBuild(gsTextPart(""), "", "", "")
	e1 := gsBuild(gsTextPart("hi"), "STOP", gsUsage(1, 1), "")
	allE, _ := rvDecodeAll(t, true, []string{e0, e1})
	opensE := 0
	for _, ev := range allE {
		if _, ok := ev.(BlockOpen); ok {
			opensE++
		}
	}
	if opensE != 1 {
		t.Fatalf("empty-text run opens=%d, want 1", opensE)
	}
	// Chat target refuses text-after-tool; ordered targets keep order.
	chat := NewChatEventEncoder("public", true, DefaultLimits())
	failed := false
	for _, ev := range all {
		if eerr := chat.Encode(&bytes.Buffer{}, nil, ev); eerr != nil {
			failed = true
			break
		}
	}
	if !failed {
		t.Fatal("Chat accepted text-after-tool")
	}
}

func TestReviewGeminiStreamBlockedContradictions(t *testing.T) {
	blockedNoUsage := `{"promptFeedback":{"blockReason":"SAFETY"}}`
	if _, rerr := NewGeminiStreamDecoder(DefaultLimits(), true).DecodeFrame(rvGSSE(blockedNoUsage)); rerr == nil {
		t.Fatal("blocked terminal without usage accepted")
	}
	// Blocked signal after real content contradicts.
	dec := NewGeminiStreamDecoder(DefaultLimits(), true)
	if _, rerr := dec.DecodeFrame(rvGSSE(gsBuild(gsTextPart("hi"), "", "", ""))); rerr != nil {
		t.Fatal(rerr)
	}
	if _, rerr := dec.DecodeFrame(rvGSSE(blockedNoUsage)); rerr == nil {
		t.Fatal("blocked after content accepted")
	} else if !strings.Contains(rerr.Msg, "contradictory") {
		t.Fatalf("wrong class: %v", rerr)
	}
	// Blocked + tool call in the same terminal chunk contradicts.
	withCall := gsBuild(gsToolPart("t", "c1", `{"a":1}`), "SAFETY", gsUsage(1, 1), "")
	if _, rerr := NewGeminiStreamDecoder(DefaultLimits(), true).DecodeFrame(rvGSSE(withCall)); rerr == nil {
		t.Fatal("filter finish with calls accepted")
	}
	// Blocked prompt but non-filter finish contradicts.
	nonFilter := `{"promptFeedback":{"blockReason":"SAFETY"},"candidates":[{"content":{"role":"model","parts":[` + gsTextPart("hi") + `]},"finishReason":"STOP","index":0}],"usageMetadata":` + gsUsage(1, 1) + `}`
	if _, rerr := NewGeminiStreamDecoder(DefaultLimits(), true).DecodeFrame(rvGSSE(nonFilter)); rerr == nil {
		t.Fatal("blocked prompt with STOP finish accepted")
	}
	// Clean blocked terminal succeeds as ContentFilter.
	clean := `{"promptFeedback":{"blockReason":"SAFETY"},"usageMetadata":` + gsUsage(7, 0) + `}`
	evs, dec2 := rvDecodeAll(t, true, []string{clean})
	if tail, rerr := dec2.Close(); rerr != nil || len(tail) != 0 {
		t.Fatalf("clean blocked Close: %v", rerr)
	}
	sawComplete := false
	for _, ev := range evs {
		if c, ok := ev.(CompletionEvent); ok {
			sawComplete = true
			if c.Finish.Kind != FinishContentFilter {
				t.Fatalf("blocked finish = %q", c.Finish.Kind)
			}
		}
	}
	if !sawComplete {
		t.Fatal("clean blocked terminal missing CompletionEvent")
	}
}

func TestReviewGeminiStreamTerminalUsageStrict(t *testing.T) {
	// Terminal without usage fails; intermediate never substitutes.
	mid := gsBuild(gsTextPart("a"), "", gsUsage(100, 100), "")
	termNo := gsBuild(gsTextPart("b"), "STOP", "", "")
	dec := NewGeminiStreamDecoder(DefaultLimits(), true)
	if _, rerr := dec.DecodeFrame(rvGSSE(mid)); rerr != nil {
		t.Fatal(rerr)
	}
	if _, rerr := dec.DecodeFrame(rvGSSE(termNo)); rerr == nil {
		t.Fatal("terminal without usage accepted")
	}
	// Cache share larger than prompt fails.
	badCache := gsBuild(gsTextPart("hi"), "STOP",
		`{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3,"cachedContentTokenCount":9}`, "")
	if _, rerr := NewGeminiStreamDecoder(DefaultLimits(), true).DecodeFrame(rvGSSE(badCache)); rerr == nil {
		t.Fatal("cache>prompt accepted")
	}
	// Inexact total fails.
	badTotal := gsBuild(gsTextPart("hi"), "STOP",
		`{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":99}`, "")
	if _, rerr := NewGeminiStreamDecoder(DefaultLimits(), true).DecodeFrame(rvGSSE(badTotal)); rerr == nil {
		t.Fatal("inexact total accepted")
	}
	// Nonzero thoughts/toolUse fail as unrepresentable, never borrowed.
	for _, u := range []string{
		`{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3,"thoughtsTokenCount":1}`,
		`{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3,"toolUsePromptTokenCount":1}`,
		`{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3,"promptTokensDetails":[{"x":1}]}`,
	} {
		elem := gsBuild(gsTextPart("hi"), "STOP", u, "")
		if _, rerr := NewGeminiStreamDecoder(DefaultLimits(), true).DecodeFrame(rvGSSE(elem)); rerr == nil {
			t.Fatalf("unsupported usage accepted: %s", u)
		}
	}
	// Valid intermediate usage is dropped (no UsageEvent) and the terminal wins.
	dec2 := NewGeminiStreamDecoder(DefaultLimits(), true)
	e0, rerr := dec2.DecodeFrame(rvGSSE(mid))
	if rerr != nil {
		t.Fatal(rerr)
	}
	for _, ev := range e0 {
		if _, ok := ev.(UsageEvent); ok {
			t.Fatal("intermediate usage emitted")
		}
	}
	e1, rerr := dec2.DecodeFrame(rvGSSE(gsBuild(gsTextPart("b"), "STOP", gsUsage(5, 7), "")))
	if rerr != nil {
		t.Fatal(rerr)
	}
	for _, ev := range e1 {
		if u, ok := ev.(UsageEvent); ok {
			if *u.Usage.Prompt != 5 || *u.Usage.Completion != 7 {
				t.Fatalf("terminal borrowed intermediate: %+v", u.Usage)
			}
		}
	}
}

func TestReviewGeminiStreamSSENoReadPast(t *testing.T) {
	term := gsBuild(gsTextPart("hi"), "STOP", gsUsage(1, 1), "")
	src := &rvCountFrames{frames: []Frame{{Data: gsBuild(gsTextPart("a"), "", "", "")}, {Data: term}}}
	dec := NewGeminiStreamDecoder(DefaultLimits(), true)
	enc := NewChatEventEncoder("public", true, DefaultLimits())
	var buf bytes.Buffer
	flushes := 0
	flush := func() { flushes++ }
	if rerr := TranslateStream(src, dec, enc, &buf, flush, DefaultLimits(), DialectGemini); rerr != nil {
		t.Fatalf("pipeline: %v", rerr)
	}
	if src.next != 2 {
		t.Fatalf("reads = %d, want exactly 2 (no EOF read past terminal)", src.next)
	}
	if flushes == 0 {
		t.Fatal("no flushes: text was not delivered immediately")
	}
	if !strings.Contains(buf.String(), "hi") || !strings.Contains(buf.String(), "data: [DONE]") {
		t.Fatalf("wire missing text/terminator: %s", buf.String())
	}
}

func TestReviewGeminiStreamArrayFramingContract(t *testing.T) {
	elem := gsBuild(gsTextPart("hi"), "STOP", gsUsage(1, 1), "")
	// Immediate object delivery: first Next returns before the separator.
	r := strings.NewReader(`[` + elem + `,{"candidates":[{"content":{"role":"model","parts":[{"text":"x"}]},"index":0}]}` + `]`)
	fr := NewJSONArrayFrameReader(r, DefaultLimits())
	f0, rerr := fr.Next()
	if rerr != nil || f0.Closed || f0.Data == "" {
		t.Fatalf("first element not immediate: %v/%+v", rerr, f0)
	}
	// Truncation (EOF before ]) never succeeds through the new decoder.
	rT := strings.NewReader(`[` + elem)
	framesT := NewJSONArrayFrameReader(rT, DefaultLimits())
	decT := NewGeminiStreamDecoder(DefaultLimits(), false)
	encT := NewChatEventEncoder("public", true, DefaultLimits())
	var bufT bytes.Buffer
	if rerr := TranslateStream(framesT, decT, encT, &bufT, nil, DefaultLimits(), DialectGemini); rerr == nil {
		t.Fatal("array truncation succeeded")
	}
	// Trailing comma never succeeds.
	rC := strings.NewReader(`[` + elem + `,]`)
	framesC := NewJSONArrayFrameReader(rC, DefaultLimits())
	decC := NewGeminiStreamDecoder(DefaultLimits(), false)
	encC := NewChatEventEncoder("public", true, DefaultLimits())
	var bufC bytes.Buffer
	if rerr := TranslateStream(framesC, decC, encC, &bufC, nil, DefaultLimits(), DialectGemini); rerr == nil {
		t.Fatal("trailing comma succeeded")
	}
	// Bare close after an armed finish succeeds exactly once.
	dec := NewGeminiStreamDecoder(DefaultLimits(), false)
	evs, rerr := dec.DecodeFrame(Frame{Data: elem})
	if rerr != nil {
		t.Fatal(rerr)
	}
	for _, ev := range evs {
		if _, ok := ev.(CompletionEvent); ok {
			t.Fatal("array finish element emitted Completion before close")
		}
	}
	closed, rerr := dec.DecodeFrame(Frame{Closed: true})
	if rerr != nil {
		t.Fatal(rerr)
	}
	sawU, sawC := false, false
	for _, ev := range closed {
		if _, ok := ev.(UsageEvent); ok {
			sawU = true
		}
		if _, ok := ev.(CompletionEvent); ok {
			sawC = true
		}
	}
	if !sawU || !sawC {
		t.Fatalf("close kinds=%v, want usage+completion", eventKinds(closed))
	}
	if _, rerr := dec.DecodeFrame(Frame{Data: elem}); rerr == nil {
		t.Fatal("frame after array completion accepted")
	}
}

func TestReviewGeminiStreamWriteFailureStopsReads(t *testing.T) {
	term := gsBuild(gsTextPart("hi"), "STOP", gsUsage(1, 1), "")
	src := &rvCountFrames{frames: []Frame{{Data: gsBuild(gsTextPart("a"), "", "", "")}, {Data: term}}}
	dec := NewGeminiStreamDecoder(DefaultLimits(), true)
	enc := NewChatEventEncoder("public", true, DefaultLimits())
	w := &rvFailWriter{failAfter: 1}
	if rerr := TranslateStream(src, dec, enc, w, nil, DefaultLimits(), DialectGemini); rerr == nil {
		t.Fatal("write failure accepted")
	}
	if src.next > 2 {
		t.Fatalf("read past write failure: %d", src.next)
	}
	if !enc.Committed() {
		t.Fatal("encoder should be committed after first successful write")
	}
}

func TestReviewGeminiStreamIdentityMetadataOnlyAllTargets(t *testing.T) {
	c0 := gsBuild(gsTextPart("hel"), "", "", "")
	c1 := gsBuild(gsTextPart("lo"), "STOP", gsUsage(5, 3), `"responseId":"rid9","modelVersion":"mm9"`)
	dec := NewGeminiStreamDecoder(DefaultLimits(), true)
	e0, _ := dec.DecodeFrame(rvGSSE(c0))
	e1, _ := dec.DecodeFrame(rvGSSE(c1))
	all := append(e0, e1...)
	lim := DefaultLimits()
	encs := map[string]EventEncoder{
		"chat":      NewChatEventEncoder("public", true, lim),
		"anthropic": NewAnthropicEventEncoder("public", lim),
		"responses": NewResponsesEventEncoder("public", lim),
		"gemini":    NewGeminiEventEncoder("public", lim, true),
	}
	for name, enc := range encs {
		var buf bytes.Buffer
		for _, ev := range all {
			before := buf.Len()
			if eerr := enc.Encode(&buf, nil, ev); eerr != nil {
				t.Fatalf("%s encode(%T): %v", name, ev, eerr.Err)
			}
			switch ev.(type) {
			case ProviderIdentityEvent, ResponseModelEvent:
				if buf.Len() != before {
					t.Fatalf("%s metadata event wrote bytes", name)
				}
			}
		}
		out := buf.String()
		if !strings.Contains(out, "public") {
			t.Fatalf("%s missing public alias: %s", name, out)
		}
		if strings.Contains(out, "mm9") {
			t.Fatalf("%s leaks provider model: %s", name, out)
		}
		if name == "gemini" {
			if !strings.Contains(out, `"responseId":"rid9"`) {
				t.Fatalf("gemini must echo provider responseId terminally: %s", out)
			}
		} else if strings.Contains(out, "rid9") {
			t.Fatalf("%s must not echo upstream id: %s", name, out)
		}
	}
}
