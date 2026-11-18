// Gemini source stream decoder tests: wire facts, framing, and bounds.
//
// These source-level cases drive
// NewGeminiStreamDecoder directly (SSE true / array false) or through
// TranslateStream with shared framing/pipeline, never a pair translator
// or Chat/Responses JSON intermediate.
package protocol

import (
	"bytes"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"testing"
)

func gsItoa(n int) string { return strconv.Itoa(n) }

func gsUsage(prompt, cand int) string {
	return `{"promptTokenCount":` + gsItoa(prompt) + `,"candidatesTokenCount":` + gsItoa(cand) + `,"totalTokenCount":` + gsItoa(prompt+cand) + `}`
}

func gsBuild(partObj, finish, usage, envelope string) string {
	cand := `{`
	has := false
	if partObj != "" {
		cand += `"content":{"role":"model","parts":[` + partObj + `]}`
		has = true
	}
	if finish != "" {
		if has {
			cand += `,`
		}
		cand += `"finishReason":` + gsQuote(finish)
		has = true
	}
	if !has {
		cand += `"content":{"role":"model","parts":[]}`
	}
	cand += `,"index":0}`
	obj := `{"candidates":[` + cand + `]`
	if usage != "" {
		obj += `,"usageMetadata":` + usage
	}
	if envelope != "" {
		obj += `,` + envelope
	}
	obj += `}`
	return obj
}

func gsQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func gsTextPart(s string) string { return `{"text":` + gsQuote(s) + `}` }

func gsToolPart(name, id, args string) string {
	fc := `"name":` + gsQuote(name)
	if id != "" {
		fc += `,"id":` + gsQuote(id)
	}
	if args != "" {
		fc += `,"args":` + args
	}
	return `{"functionCall":{` + fc + `}}`
}

func gsSSEFrame(data string) Frame { return Frame{Data: data} }

func gsDecodeSSE(t *testing.T, datas []string) ([]ResponseEvent, *GeminiStreamDecoder) {
	t.Helper()
	dec := NewGeminiStreamDecoder(DefaultLimits(), true)
	var all []ResponseEvent
	for i, d := range datas {
		evs, rerr := dec.DecodeFrame(gsSSEFrame(d))
		if rerr != nil {
			t.Fatalf("DecodeFrame[%d]: %v", i, rerr)
		}
		all = append(all, evs...)
	}
	return all, dec
}

func gsObserve(t *testing.T, evs []ResponseEvent) *ResponseLifecycle {
	t.Helper()
	life := NewResponseLifecycle(DefaultLimits(), DialectGemini)
	for i, ev := range evs {
		if rerr := life.Observe(ev); rerr != nil {
			t.Fatalf("lifecycle Observe[%d](%T): %v", i, ev, rerr)
		}
	}
	return life
}

// 1. Absent early IDs stream text immediately with router origins.
func TestGeminiStreamAbsentIDsImmediateText(t *testing.T) {
	term := gsBuild(gsTextPart("hi"), "STOP", gsUsage(5, 2), "")
	frames := []string{gsBuild(gsTextPart("hel"), "", "", ""), term}
	dec := NewGeminiStreamDecoder(DefaultLimits(), true)
	evs0, rerr := dec.DecodeFrame(gsSSEFrame(frames[0]))
	if rerr != nil {
		t.Fatalf("frame0: %v", rerr)
	}
	if len(evs0) != 3 {
		t.Fatalf("frame0 events = %d, want start+open+text", len(evs0))
	}
	start, ok := evs0[0].(ResponseStart)
	if !ok || string(start.ID) != geminiCompatResponseID || start.Model != "" {
		t.Fatalf("ev0 = %#v, want minted start", evs0[0])
	}
	if start.IDOrigin != IdentityRouterMinted || start.ModelOrigin != IdentityRouterMinted {
		t.Fatalf("origins = %q/%q, want router/router", start.IDOrigin, start.ModelOrigin)
	}
	if _, ok := evs0[1].(BlockOpen); !ok {
		t.Fatalf("ev1 = %#v, want BlockOpen", evs0[1])
	}
	if tf, ok := evs0[2].(TextFragment); !ok || tf.Text != "hel" {
		t.Fatalf("ev2 = %#v, want TextFragment hel", evs0[2])
	}
	evs1, rerr := dec.DecodeFrame(gsSSEFrame(frames[1]))
	if rerr != nil {
		t.Fatalf("frame1: %v", rerr)
	}
	kinds := eventKinds(evs1)
	// terminal chunk carries text + close + usage + completion (5 events:
	// text, close t0, close? only t0 open, usage, completion).
	foundUsage, foundCompletion := false, false
	for _, ev := range evs1 {
		if _, ok := ev.(UsageEvent); ok {
			foundUsage = true
		}
		if _, ok := ev.(CompletionEvent); ok {
			foundCompletion = true
		}
	}
	if !foundUsage || !foundCompletion {
		t.Fatalf("terminal kinds = %v, want usage+completion", kinds)
	}
	if tail, rerr := dec.Close(); rerr != nil || len(tail) != 0 {
		t.Fatalf("Close = %v/%d, want nil/0", rerr, len(tail))
	}
	all := append(evs0, evs1...)
	life := gsObserve(t, all)
	res, rerr := life.Result()
	if rerr != nil {
		t.Fatalf("Result: %v", rerr)
	}
	if res.IDOrigin != IdentityRouterMinted || res.ModelOrigin != IdentityRouterMinted {
		t.Fatalf("Result origins = %q/%q, want router/router", res.IDOrigin, res.ModelOrigin)
	}
	if len(res.RouterMintedCalls) != 0 {
		t.Fatalf("minted = %v, want empty", res.RouterMintedCalls)
	}
}

// 2. Late IDs upgrade via typed events; drift conflicts.
func TestGeminiStreamLateIDsUpgrade(t *testing.T) {
	c0 := gsBuild(gsTextPart("hel"), "", "", "")
	c1 := gsBuild(gsTextPart("lo"), "STOP", gsUsage(5, 3), `"responseId":"r1","modelVersion":"m1"`)
	dec := NewGeminiStreamDecoder(DefaultLimits(), true)
	e0, rerr := dec.DecodeFrame(gsSSEFrame(c0))
	if rerr != nil {
		t.Fatalf("c0: %v", rerr)
	}
	e1, rerr := dec.DecodeFrame(gsSSEFrame(c1))
	if rerr != nil {
		t.Fatalf("c1: %v", rerr)
	}
	all := append(e0, e1...)
	var sawID, sawModel bool
	for _, ev := range all {
		if pi, ok := ev.(ProviderIdentityEvent); ok {
			sawID = true
			if string(pi.ID) != "r1" {
				t.Fatalf("identity = %#v", ev)
			}
		}
		if pm, ok := ev.(ResponseModelEvent); ok {
			sawModel = true
			if pm.Model != "m1" {
				t.Fatalf("model = %#v", ev)
			}
		}
	}
	if !sawID || !sawModel {
		t.Fatalf("missing late events kinds=%v", eventKinds(all))
	}
	life := gsObserve(t, all)
	res, rerr := life.Result()
	if rerr != nil {
		t.Fatalf("Result: %v", rerr)
	}
	if string(res.ID) != "r1" || res.Model != "m1" {
		t.Fatalf("Result = %+v", res)
	}
	if res.IDOrigin != IdentityProvider || res.ModelOrigin != IdentityProvider {
		t.Fatalf("origins = %q/%q", res.IDOrigin, res.ModelOrigin)
	}
	// Drift after known conflicts.
	c2 := gsBuild(gsTextPart("x"), "", "", `"responseId":"r2"`)
	dec2 := NewGeminiStreamDecoder(DefaultLimits(), true)
	if _, rerr := dec2.DecodeFrame(gsSSEFrame(c0)); rerr != nil {
		t.Fatal(rerr)
	}
	if _, rerr := dec2.DecodeFrame(gsSSEFrame(c1)); rerr != nil {
		t.Fatal(rerr)
	}
	if _, rerr := dec2.DecodeFrame(gsSSEFrame(c2)); rerr == nil {
		t.Fatal("id drift accepted")
	} else if strings.Contains(rerr.Msg, "r2") {
		t.Fatalf("drift leaks value: %q", rerr.Msg)
	}
}

// 3. Compat-prefix provider IDs stay provider, steer minting.
func TestGeminiStreamCompatPrefixProvider(t *testing.T) {
	toolSupplied := gsBuild(gsToolPart("get_time", "gemini-compat-call-3", `{"tz":"UTC"}`), "", "", "")
	toolMinted := gsBuild(gsToolPart("other", "", ""), "STOP", gsUsage(4, 4), "")
	dec := NewGeminiStreamDecoder(DefaultLimits(), true)
	e0, rerr := dec.DecodeFrame(gsSSEFrame(toolSupplied))
	if rerr != nil {
		t.Fatalf("supplied: %v", rerr)
	}
	e1, rerr := dec.DecodeFrame(gsSSEFrame(toolMinted))
	if rerr != nil {
		t.Fatalf("minted: %v", rerr)
	}
	all := append(e0, e1...)
	var opens []BlockOpen
	for _, ev := range all {
		if o, ok := ev.(BlockOpen); ok && o.Kind == BlockTool {
			opens = append(opens, o)
		}
	}
	if len(opens) != 2 {
		t.Fatalf("opens = %v", eventKinds(all))
	}
	if opens[0].Call != "gemini-compat-call-3" || opens[0].CallOrigin != IdentityProvider {
		t.Fatalf("open0 = %+v, want provider compat id", opens[0])
	}
	if opens[1].CallOrigin != IdentityRouterMinted {
		t.Fatalf("open1 = %+v, want router", opens[1])
	}
	if string(opens[1].Call) == "gemini-compat-call-3" {
		t.Fatal("mint collided with supplied")
	}
	life := gsObserve(t, all)
	res, rerr := life.Result()
	if rerr != nil {
		t.Fatalf("Result: %v", rerr)
	}
	if _, ok := res.RouterMintedCalls["gemini-compat-call-3"]; ok {
		t.Fatal("provider compat id in minted set")
	}
	if _, ok := res.RouterMintedCalls[opens[1].Call]; !ok {
		t.Fatalf("minted set = %v, missing %q", res.RouterMintedCalls, opens[1].Call)
	}
	// Exact args: supplied object preserved, missing -> {}.
	var frags []ArgsFragment
	for _, ev := range all {
		if f, ok := ev.(ArgsFragment); ok {
			frags = append(frags, f)
		}
	}
	if len(frags) != 2 || frags[0].Fragment != `{"tz":"UTC"}` || frags[1].Fragment != `{}` {
		t.Fatalf("frags = %+v", frags)
	}
}

// 4. SSE prompt terminal; further frames fail; Close without terminal fails.
func TestGeminiStreamSSEPromptTerminal(t *testing.T) {
	term := gsBuild(gsTextPart("hi"), "STOP", gsUsage(1, 1), "")
	dec := NewGeminiStreamDecoder(DefaultLimits(), true)
	evs, rerr := dec.DecodeFrame(gsSSEFrame(term))
	if rerr != nil {
		t.Fatalf("term: %v", rerr)
	}
	if _, rerr := dec.Close(); rerr != nil {
		t.Fatalf("Close: %v", rerr)
	}
	_ = evs
	if _, rerr := dec.DecodeFrame(gsSSEFrame(term)); rerr == nil {
		t.Fatal("post-terminal accepted")
	}
	dec2 := NewGeminiStreamDecoder(DefaultLimits(), true)
	if _, rerr := dec2.Close(); rerr == nil {
		t.Fatal("Close without terminal accepted")
	}
}

// 5. Array defers terminal until Closed.
func TestGeminiStreamArrayNeedsClose(t *testing.T) {
	lim := DefaultLimits()
	elem := gsBuild(gsTextPart("hi"), "STOP", gsUsage(2, 2), `"responseId":"r9","modelVersion":"mm"`)
	dec := NewGeminiStreamDecoder(lim, false)
	evs, rerr := dec.DecodeFrame(Frame{Data: elem})
	if rerr != nil {
		t.Fatalf("elem: %v", rerr)
	}
	for _, ev := range evs {
		if _, ok := ev.(UsageEvent); ok {
			t.Fatal("array finish element emitted Usage before close")
		}
		if _, ok := ev.(CompletionEvent); ok {
			t.Fatal("array finish element emitted Completion before close")
		}
	}
	if _, rerr := dec.Close(); rerr == nil {
		t.Fatal("Close before array close accepted")
	}
	term, rerr := dec.DecodeFrame(Frame{Closed: true})
	if rerr != nil {
		t.Fatalf("close: %v", rerr)
	}
	sawU, sawC := false, false
	for _, ev := range term {
		if _, ok := ev.(UsageEvent); ok {
			sawU = true
		}
		if _, ok := ev.(CompletionEvent); ok {
			sawC = true
		}
	}
	if !sawU || !sawC {
		t.Fatalf("closed kinds=%v", eventKinds(term))
	}
	if _, rerr := dec.Close(); rerr != nil {
		t.Fatalf("Close: %v", rerr)
	}
	all := append(evs, term...)
	life := NewResponseLifecycle(lim, DialectGemini)
	for i, ev := range all {
		if rerr := life.Observe(ev); rerr != nil {
			t.Fatalf("Observe[%d](%T): %v", i, ev, rerr)
		}
	}
	if _, rerr := life.Result(); rerr != nil {
		t.Fatalf("Result: %v", rerr)
	}
	// Post-finish element is content-after-finish.
	dec2 := NewGeminiStreamDecoder(lim, false)
	if _, rerr := dec2.DecodeFrame(Frame{Data: elem}); rerr != nil {
		t.Fatal(rerr)
	}
	if _, rerr := dec2.DecodeFrame(Frame{Data: gsBuild(gsTextPart("x"), "", "", "")}); rerr == nil {
		t.Fatal("post-finish element accepted")
	}
}

// 6. Terminal-own-usage: intermediate ignored, terminal without fails.
func TestGeminiStreamTerminalOwnUsage(t *testing.T) {
	mid := gsBuild(gsTextPart("a"), "", gsUsage(100, 100), "")
	termNoUsage := gsBuild(gsTextPart("b"), "STOP", "", "")
	dec := NewGeminiStreamDecoder(DefaultLimits(), true)
	e0, rerr := dec.DecodeFrame(gsSSEFrame(mid))
	if rerr != nil {
		t.Fatalf("mid: %v", rerr)
	}
	for _, ev := range e0 {
		if _, ok := ev.(UsageEvent); ok {
			t.Fatal("intermediate usage emitted")
		}
	}
	if _, rerr := dec.DecodeFrame(gsSSEFrame(termNoUsage)); rerr == nil {
		t.Fatal("terminal without usage accepted")
	}
	// Valid: intermediate ignored, terminal wins.
	dec2 := NewGeminiStreamDecoder(DefaultLimits(), true)
	if _, rerr := dec2.DecodeFrame(gsSSEFrame(mid)); rerr != nil {
		t.Fatal(rerr)
	}
	term := gsBuild(gsTextPart("b"), "STOP", gsUsage(5, 7), "")
	e1, rerr := dec2.DecodeFrame(gsSSEFrame(term))
	if rerr != nil {
		t.Fatalf("term: %v", rerr)
	}
	for _, ev := range e1 {
		if u, ok := ev.(UsageEvent); ok {
			if *u.Usage.Prompt != 5 || *u.Usage.Completion != 7 {
				t.Fatalf("terminal usage borrowed intermediate: %+v", u.Usage)
			}
		}
	}
}

// 7. Finish mapping and contradictions.
func TestGeminiStreamFinishMapping(t *testing.T) {
	cases := []struct {
		name       string
		finish     string
		hasCalls   bool
		blocked    bool
		wantKind   FinishKind
		wantFail   bool
		contradict bool
	}{
		{"stop", "STOP", false, false, FinishStop, false, false},
		{"toolcalls", "STOP", true, false, FinishToolCalls, false, false},
		{"length", "MAX_TOKENS", false, false, FinishLength, false, false},
		{"safety", "SAFETY", false, false, FinishContentFilter, false, false},
		{"recitation", "RECITATION", false, false, FinishContentFilter, false, false},
		{"filter+calls", "SAFETY", true, false, "", true, true},
		{"unknown", "MALFORMED_FUNCTION_CALL", false, false, "", true, false},
		{"continuation", "CONTINUATION", false, false, "", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var part string
			if tc.hasCalls {
				part = gsToolPart("t", "c1", `{"a":1}`)
			} else {
				part = gsTextPart("hi")
			}
			elem := gsBuild(part, tc.finish, gsUsage(1, 1), "")
			dec := NewGeminiStreamDecoder(DefaultLimits(), true)
			evs, rerr := dec.DecodeFrame(gsSSEFrame(elem))
			if tc.wantFail {
				if rerr == nil {
					t.Fatalf("finish %q accepted", tc.finish)
				}
				if strings.Contains(rerr.Msg, tc.finish) && tc.finish != "" {
					// Unknown finishes must not echo the value; filter
					// contradictions are also value-free.
					t.Fatalf("diagnostic leaks finish: %q", rerr.Msg)
				}
				return
			}
			if rerr != nil {
				t.Fatalf("finish %q: %v", tc.finish, rerr)
			}
			var got *CompletionEvent
			for _, ev := range evs {
				if c, ok := ev.(CompletionEvent); ok {
					cp := c
					got = &cp
				}
			}
			if got == nil || got.Finish.Kind != tc.wantKind {
				t.Fatalf("got %+v, want %q", got, tc.wantKind)
			}
		})
	}
}

// 8. Exact bigint args; missing -> {}.
func TestGeminiStreamExactBigint(t *testing.T) {
	big := `{"n":9007199254740993}`
	elem := gsBuild(gsToolPart("calc", "c-big", big), "STOP", gsUsage(3, 3), "")
	dec := NewGeminiStreamDecoder(DefaultLimits(), true)
	evs, rerr := dec.DecodeFrame(gsSSEFrame(elem))
	if rerr != nil {
		t.Fatalf("big: %v", rerr)
	}
	for _, ev := range evs {
		if f, ok := ev.(ArgsFragment); ok {
			if f.Fragment != big {
				t.Fatalf("args = %q, want exact %q", f.Fragment, big)
			}
		}
	}
}

// 9. role assistant rejected; unknown members fail.
func TestGeminiStreamRoleAndEnvelope(t *testing.T) {
	badRole := `{"candidates":[{"content":{"role":"assistant","parts":[{"text":"hi"}]},"index":0}],"usageMetadata":` + gsUsage(1, 1) + `}`
	dec := NewGeminiStreamDecoder(DefaultLimits(), true)
	if _, rerr := dec.DecodeFrame(gsSSEFrame(badRole)); rerr == nil {
		t.Fatal("assistant role accepted")
	}
	badTop := `{"candidates":[],"weird":1}`
	if _, rerr := NewGeminiStreamDecoder(DefaultLimits(), true).DecodeFrame(gsSSEFrame(badTop)); rerr == nil {
		t.Fatal("unknown member accepted")
	}
	nullTop := `{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP","index":0}],"usageMetadata":` + gsUsage(1, 1) + `,"weird":null}`
	if _, rerr := NewGeminiStreamDecoder(DefaultLimits(), true).DecodeFrame(gsSSEFrame(nullTop)); rerr != nil {
		t.Fatalf("unknown null rejected: %v", rerr)
	}
}

// 10. Missing vs supplied-empty identity metadata.
func TestGeminiStreamMissingVsSuppliedEmpty(t *testing.T) {
	// Missing/null absent -> minted/router.
	for _, env := range []string{"", `"responseId":null`, `"modelVersion":null`} {
		elem := gsBuild(gsTextPart("hi"), "STOP", gsUsage(1, 1), env)
		dec := NewGeminiStreamDecoder(DefaultLimits(), true)
		evs, rerr := dec.DecodeFrame(gsSSEFrame(elem))
		if rerr != nil {
			t.Fatalf("env %q: %v", env, rerr)
		}
		start := evs[0].(ResponseStart)
		if start.IDOrigin != IdentityRouterMinted {
			t.Fatalf("env %q origin %q", env, start.IDOrigin)
		}
	}
	// Supplied-empty fails.
	for _, env := range []string{`"responseId":""`, `"responseId":"  "`, `"modelVersion":""`, `"modelVersion":"  "`} {
		elem := gsBuild(gsTextPart("hi"), "STOP", gsUsage(1, 1), env)
		if _, rerr := NewGeminiStreamDecoder(DefaultLimits(), true).DecodeFrame(gsSSEFrame(elem)); rerr == nil {
			t.Fatalf("env %q accepted", env)
		}
	}
}

// 11. Malformed vs bounds.
func TestGeminiStreamMalformedVsBounds(t *testing.T) {
	// Non-object args malformed (invalid response, not size bound).
	for _, args := range []string{`[1]`, `"s"`, `1`} {
		elem := gsBuild(gsToolPart("t", "c1", args), "", "", "")
		_, rerr := NewGeminiStreamDecoder(DefaultLimits(), true).DecodeFrame(gsSSEFrame(elem))
		if rerr == nil {
			t.Fatalf("args %s accepted", args)
		} else if strings.Contains(rerr.Msg, "size bound") {
			t.Fatalf("args %s misclassified as bounds: %q", args, rerr.Msg)
		}
	}
	// Oversize args -> size bound.
	lim := Limits{MaxArgsChars: 8, MaxStreamArgsChars: 8000, MaxToolCalls: 4, MaxStreamBlocks: 4, MaxFrameBytes: 8000}
	elem := gsBuild(gsToolPart("t", "c1", `{"a":"123456789"}`), "", "", "")
	_, rerr := NewGeminiStreamDecoder(lim, true).DecodeFrame(gsSSEFrame(elem))
	if rerr == nil || !strings.Contains(rerr.Msg, "size bound") {
		t.Fatalf("oversize got %v", rerr)
	}
}

// 12. Pending identity bound.
func TestGeminiStreamPendingIdentityBound(t *testing.T) {
	lim := Limits{MaxArgsChars: 100, MaxStreamArgsChars: 5, MaxToolCalls: 4, MaxStreamBlocks: 4, MaxFrameBytes: 8000}
	elem := gsBuild(gsTextPart("hi"), "", "", `"responseId":"12345678901234567890"`)
	if _, rerr := NewGeminiStreamDecoder(lim, true).DecodeFrame(gsSSEFrame(elem)); rerr == nil {
		t.Fatal("pending identity overflow accepted")
	} else if !strings.Contains(rerr.Msg, "size bound") {
		t.Fatalf("wrong error: %v", rerr)
	}
}

// 13. Writer failure via pipeline stops reads.
func TestGeminiStreamWriterFailure(t *testing.T) {
	term := gsBuild(gsTextPart("hi"), "STOP", gsUsage(1, 1), "")
	frames := &gsSliceFrames{frames: []Frame{{Data: gsBuild(gsTextPart("a"), "", "", "")}, {Data: term}}, count: 0}
	dec := NewGeminiStreamDecoder(DefaultLimits(), true)
	enc := NewChatEventEncoder("public", false, DefaultLimits())
	w := &gsFailWriter{failAfter: 1}
	rerr := TranslateStream(frames, dec, enc, w, nil, DefaultLimits(), DialectGemini)
	if rerr == nil {
		t.Fatal("writer failure accepted")
	}
	if frames.count > 2 {
		t.Fatalf("read past failure: %d", frames.count)
	}
}

type gsSliceFrames struct {
	frames []Frame
	count  int
}

func (s *gsSliceFrames) Next() (Frame, *RouterError) {
	if s.count >= len(s.frames) {
		return Frame{}, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream ended without completion"}
	}
	f := s.frames[s.count]
	s.count++
	return f, nil
}

type gsFailWriter struct {
	n         int
	failAfter int
}

func (w *gsFailWriter) Write(p []byte) (int, error) {
	w.n++
	if w.n > w.failAfter {
		return 0, io.ErrClosedPipe
	}
	return len(p), nil
}

// 14. Array framing integration: truncation and trailing comma never succeed.
func TestGeminiStreamArrayFramingErrors(t *testing.T) {
	// Truncation: finish element but EOF before ].
	elem := gsBuild(gsTextPart("hi"), "STOP", gsUsage(1, 1), "")
	r := io.MultiReader(strings.NewReader(`[`+elem), strings.NewReader(``))
	frames := NewJSONArrayFrameReader(r, DefaultLimits())
	dec := NewGeminiStreamDecoder(DefaultLimits(), false)
	enc := NewChatEventEncoder("public", false, DefaultLimits())
	var buf bytes.Buffer
	if rerr := TranslateStream(frames, dec, enc, &buf, nil, DefaultLimits(), DialectGemini); rerr == nil {
		t.Fatal("truncation succeeded")
	}
	// Trailing comma.
	r2 := strings.NewReader(`[` + elem + `,]`)
	frames2 := NewJSONArrayFrameReader(r2, DefaultLimits())
	dec2 := NewGeminiStreamDecoder(DefaultLimits(), false)
	enc2 := NewChatEventEncoder("public", false, DefaultLimits())
	var buf2 bytes.Buffer
	if rerr := TranslateStream(frames2, dec2, enc2, &buf2, nil, DefaultLimits(), DialectGemini); rerr == nil {
		t.Fatal("trailing comma succeeded")
	}
}
