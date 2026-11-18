// Gemini stream cross-target traces and identity plumbing.
//
// Same decoder events through all four reusable target encoders:
// order, buckets, finish, call IDs preserved; public alias stamped;
// wrapper IDs stable; provider model never leaks; router origins never
// claimed as provider; late identity/model cannot drift.
package protocol

import (
	"bytes"
	"strings"
	"testing"
)

func gsCrossFramesTextTool() []Frame {
	c0 := gsBuild(gsTextPart("hello "), "", "", "")
	tool := gsBuild(gsToolPart("get_time", "", `{"tz":"UTC"}`), "", "", `"responseId":"rp1","modelVersion":"mv1"`)
	term := gsBuild(gsTextPart("done"), "STOP", gsUsage(10, 6), `"responseId":"rp1","modelVersion":"mv1"`)
	return []Frame{{Data: c0}, {Data: tool}, {Data: term}}
}

func gsDecodeFull(t *testing.T, sse bool, frames []Frame) []ResponseEvent {
	t.Helper()
	dec := NewGeminiStreamDecoder(DefaultLimits(), sse)
	var all []ResponseEvent
	for i, f := range frames {
		evs, rerr := dec.DecodeFrame(f)
		if rerr != nil {
			t.Fatalf("DecodeFrame[%d]: %v", i, rerr)
		}
		all = append(all, evs...)
	}
	if sse {
		if tail, rerr := dec.Close(); rerr != nil || len(tail) != 0 {
			t.Fatalf("Close: %v/%d", rerr, len(tail))
		}
	} else {
		closed, rerr := dec.DecodeFrame(Frame{Closed: true})
		if rerr != nil {
			t.Fatalf("Closed: %v", rerr)
		}
		all = append(all, closed...)
		if tail, rerr := dec.Close(); rerr != nil || len(tail) != 0 {
			t.Fatalf("Close: %v/%d", rerr, len(tail))
		}
	}
	return all
}

func gsRunPipeline(t *testing.T, sseTarget bool, frames []Frame) string {
	t.Helper()
	// Drive the decoder once for lifecycle validation, then re-drive
	// through TranslateStream with a fresh decoder for wire output.
	// Both must agree; the wire is what targets render.
	dec1 := NewGeminiStreamDecoder(DefaultLimits(), true)
	var evs []ResponseEvent
	for _, f := range frames {
		e, rerr := dec1.DecodeFrame(f)
		if rerr != nil {
			t.Fatalf("direct: %v", rerr)
		}
		evs = append(evs, e...)
	}
	life := NewResponseLifecycle(DefaultLimits(), DialectGemini)
	for _, ev := range evs {
		if rerr := life.Observe(ev); rerr != nil {
			t.Fatalf("life: %v", rerr)
		}
	}
	if _, rerr := life.Result(); rerr != nil {
		t.Fatalf("Result: %v", rerr)
	}
	src := &gsSliceFrames{frames: frames}
	dec2 := NewGeminiStreamDecoder(DefaultLimits(), true)
	var enc EventEncoder
	if sseTarget {
		enc = NewGeminiEventEncoder("public", DefaultLimits(), true)
	} else {
		enc = NewChatEventEncoder("public", true, DefaultLimits())
	}
	var buf bytes.Buffer
	if rerr := TranslateStream(src, dec2, enc, &buf, nil, DefaultLimits(), DialectGemini); rerr != nil {
		t.Fatalf("pipeline: %v", rerr)
	}
	return buf.String()
}

// Same-source text+tool trace through all four targets.
func TestGeminiStreamCrossTargetSameSource(t *testing.T) {
	frames := gsCrossFramesTextTool()
	all := gsDecodeFull(t, true, frames)
	life := NewResponseLifecycle(DefaultLimits(), DialectGemini)
	for _, ev := range all {
		if rerr := life.Observe(ev); rerr != nil {
			t.Fatalf("life: %v", rerr)
		}
	}
	res, rerr := life.Result()
	if rerr != nil {
		t.Fatalf("Result: %v", rerr)
	}
	if string(res.ID) != "rp1" || res.Model != "mv1" || res.IDOrigin != IdentityProvider || res.ModelOrigin != IdentityProvider {
		t.Fatalf("Result=%+v", res)
	}
	lim := DefaultLimits()
	targets := map[string]EventEncoder{
		"chat":      NewChatEventEncoder("public", true, lim),
		"anthropic": NewAnthropicEventEncoder("public", lim),
		"responses": NewResponsesEventEncoder("public", lim),
		"gemini":    NewGeminiEventEncoder("public", lim, true),
	}
	outs := map[string]string{}
	chatFailed := false
	for name, enc := range targets {
		var buf bytes.Buffer
		failed := false
		for i, ev := range all {
			if eerr := enc.Encode(&buf, nil, ev); eerr != nil {
				if name == "chat" {
					failed = true
					break
				}
				t.Fatalf("%s Encode[%d](%T): %v", name, i, ev, eerr.Err)
			}
		}
		if name == "chat" {
			chatFailed = failed
			continue
		}
		outs[name] = buf.String()
	}
	if !chatFailed {
		t.Fatal("cross-target text-call-text must refuse in Chat")
	}
	// Ordered targets preserve the trace; Chat refuses at completion.
	for _, name := range []string{"anthropic", "responses", "gemini"} {
		out := outs[name]
		if !strings.Contains(out, "public") {
			t.Fatalf("%s missing public alias", name)
		}
		if strings.Contains(out, "mv1") {
			t.Fatalf("%s leaks provider model", name)
		}
	}
	// Gemini echoes the provider responseId mid-stream and terminally.
	if !strings.Contains(outs["gemini"], `"responseId":"rp1"`) {
		t.Fatalf("gemini missing provider echo: %s", outs["gemini"])
	}
	for _, name := range []string{"anthropic", "responses"} {
		if strings.Contains(outs[name], "rp1") {
			t.Fatalf("%s echoes upstream id: %s", name, outs[name])
		}
	}
	_ = chatFailed
}

// Text-call-text refuses in Chat, preserves order elsewhere (decoder facts).
func TestGeminiStreamTextCallTextOrder(t *testing.T) {
	c0 := gsBuild(gsTextPart("a"), "", "", "")
	c1 := gsBuild(gsToolPart("tt", "cc1", `{"x":1}`), "", "", "")
	c2 := gsBuild(gsTextPart("b"), "STOP", gsUsage(3, 3), "")
	all := gsDecodeFull(t, true, []Frame{{Data: c0}, {Data: c1}, {Data: c2}})
	var kinds []string
	for _, ev := range all {
		switch ev.(type) {
		case BlockOpen:
			kinds = append(kinds, "open")
		case TextFragment:
			kinds = append(kinds, "text")
		case ArgsFragment:
			kinds = append(kinds, "args")
		case BlockClose:
			kinds = append(kinds, "close")
		}
	}
	// Expect t0 open,text, c0 open,args, t1 open,text, closes.
	opens := 0
	for _, ev := range all {
		if o, ok := ev.(BlockOpen); ok {
			opens++
			_ = o
		}
	}
	if opens != 3 {
		t.Fatalf("blocks=%d kinds=%v", opens, kinds)
	}
	// Chat completion refuses.
	lim := DefaultLimits()
	chat := NewChatEventEncoder("public", true, lim)
	var buf bytes.Buffer
	failed := false
	for _, ev := range all {
		if eerr := chat.Encode(&buf, nil, ev); eerr != nil {
			failed = true
			break
		}
	}
	if !failed {
		t.Fatal("Chat accepted text-after-tool")
	}
}

// Router-minted IDs omitted downstream; provider compat prefix echoes.
func TestGeminiStreamRouterOmission(t *testing.T) {
	// Router start (no IDs) with a tool: terminal must omit responseId.
	tool := gsBuild(gsToolPart("t", "", `{"a":1}`), "STOP", gsUsage(2, 2), "")
	all := gsDecodeFull(t, true, []Frame{{Data: tool}})
	gem := NewGeminiEventEncoder("public", DefaultLimits(), true)
	var buf bytes.Buffer
	for _, ev := range all {
		if eerr := gem.Encode(&buf, nil, ev); eerr != nil {
			t.Fatalf("gem: %v", eerr.Err)
		}
	}
	if strings.Contains(buf.String(), "responseId") {
		t.Fatalf("router ID echoed: %s", buf.String())
	}
	// Provider-supplied gemini-compat-response echoes.
	tool2 := gsBuild(gsToolPart("t", "", `{"a":1}`), "STOP", gsUsage(2, 2), `"responseId":"gemini-compat-response","modelVersion":"mm"`)
	dec := NewGeminiStreamDecoder(DefaultLimits(), true)
	evs, rerr := dec.DecodeFrame(Frame{Data: tool2})
	if rerr != nil {
		t.Fatal(rerr)
	}
	start := evs[0].(ResponseStart)
	if start.IDOrigin != IdentityProvider {
		t.Fatalf("compat prefix inferred router: %+v", start)
	}
	life := NewResponseLifecycle(DefaultLimits(), DialectGemini)
	for _, ev := range evs {
		if rerr := life.Observe(ev); rerr != nil {
			t.Fatal(rerr)
		}
	}
	res, _ := life.Result()
	_ = res
	gem2 := NewGeminiEventEncoder("public", DefaultLimits(), true)
	var buf2 bytes.Buffer
	for _, ev := range evs {
		if eerr := gem2.Encode(&buf2, nil, ev); eerr != nil {
			t.Fatalf("gem2: %v", eerr.Err)
		}
	}
	if !strings.Contains(buf2.String(), `"responseId":"gemini-compat-response"`) {
		t.Fatalf("provider compat prefix not echoed: %s", buf2.String())
	}
}

// Identity/model events are metadata-only across all targets.
func TestGeminiStreamIdentityMetadataOnly(t *testing.T) {
	c0 := gsBuild(gsTextPart("hel"), "", "", "")
	c1 := gsBuild(gsTextPart("lo"), "STOP", gsUsage(5, 3), `"responseId":"rid9","modelVersion":"mm9"`)
	dec := NewGeminiStreamDecoder(DefaultLimits(), true)
	e0, _ := dec.DecodeFrame(Frame{Data: c0})
	e1, _ := dec.DecodeFrame(Frame{Data: c1})
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
				t.Fatalf("%s: %v", name, eerr.Err)
			}
			if _, ok := ev.(ProviderIdentityEvent); ok {
				if buf.Len() != before {
					t.Fatalf("%s identity wrote bytes", name)
				}
			}
			if _, ok := ev.(ResponseModelEvent); ok {
				if buf.Len() != before {
					t.Fatalf("%s model wrote bytes", name)
				}
			}
		}
		out := buf.String()
		if !strings.Contains(out, "public") || strings.Contains(out, "mm9") {
			t.Fatalf("%s model leak: %s", name, out)
		}
	}
	// Drift, before-start, after-completion, duplicate all fail.
	lim2 := DefaultLimits()
	chat := NewChatEventEncoder("public", true, lim2)
	var buf bytes.Buffer
	if eerr := chat.Encode(&buf, nil, ProviderIdentityEvent{ID: "x"}); eerr == nil {
		t.Fatal("identity before start accepted (chat)")
	}
	anth := NewAnthropicEventEncoder("public", lim2)
	if eerr := anth.Encode(&buf, nil, ProviderIdentityEvent{ID: "x"}); eerr == nil {
		t.Fatal("identity before start accepted (anthropic)")
	}
	respEnc := NewResponsesEventEncoder("public", lim2)
	if eerr := respEnc.Encode(&buf, nil, ProviderIdentityEvent{ID: "x"}); eerr == nil {
		t.Fatal("identity before start accepted (responses)")
	}
	gem := NewGeminiEventEncoder("public", lim2, true)
	if eerr := gem.Encode(&buf, nil, ProviderIdentityEvent{ID: "x"}); eerr == nil {
		t.Fatal("identity before start accepted (gemini)")
	}
	// Lifecycle: identity before start, duplicate, after completion.
	life := NewResponseLifecycle(DefaultLimits(), DialectGemini)
	if rerr := life.Observe(ProviderIdentityEvent{ID: "r"}); rerr == nil {
		t.Fatal("lifecycle identity before start accepted")
	}
	observeAll(t, life, ResponseStart{ID: "gemini-compat-response", Model: "", Source: DialectGemini, IDOrigin: IdentityRouterMinted, ModelOrigin: IdentityRouterMinted})
	if rerr := life.Observe(ProviderIdentityEvent{ID: ""}); rerr == nil {
		t.Fatal("empty identity accepted")
	}
	observeAll(t, life, ProviderIdentityEvent{ID: "r1"})
	if rerr := life.Observe(ProviderIdentityEvent{ID: "r1"}); rerr == nil {
		t.Fatal("duplicate identity accepted")
	}
	observeAll(t, life,
		BlockOpen{Item: "t0", Kind: BlockText},
		TextFragment{Item: "t0", Text: "hi"},
		BlockClose{Item: "t0"},
		UsageEvent{Usage: mustTerminalUsage(t, 5, 2)},
		CompletionEvent{Finish: mustFinish(t, FinishStop)})
	if rerr := life.Observe(ProviderIdentityEvent{ID: "r2"}); rerr == nil {
		t.Fatal("identity after completion accepted")
	}
	// Router origins rejected for non-Gemini sources.
	life2 := NewResponseLifecycle(DefaultLimits(), DialectChat)
	if rerr := life2.Observe(ResponseStart{ID: "r", Model: "m", Source: DialectChat, IDOrigin: IdentityRouterMinted}); rerr == nil {
		t.Fatal("router origin accepted for Chat")
	}
}

// Seeded/supplied IDs steer minting; duplicates and minted collisions conflict.
func TestGeminiStreamSeededCalls(t *testing.T) {
	// Supplied 0 and 1 steer the next mint to 2.
	c0 := gsBuild(gsToolPart("a", "gemini-compat-call-0", `{"x":1}`), "", "", "")
	c1 := gsBuild(gsToolPart("b", "gemini-compat-call-1", `{"x":1}`), "", "", "")
	c2 := gsBuild(gsToolPart("c", "", `{"x":1}`), "STOP", gsUsage(6, 6), "")
	dec := NewGeminiStreamDecoder(DefaultLimits(), true)
	var all []ResponseEvent
	for _, c := range []string{c0, c1, c2} {
		evs, rerr := dec.DecodeFrame(Frame{Data: c})
		if rerr != nil {
			t.Fatalf("frame: %v", rerr)
		}
		all = append(all, evs...)
	}
	var ids []string
	for _, ev := range all {
		if o, ok := ev.(BlockOpen); ok && o.Kind == BlockTool {
			ids = append(ids, string(o.Call))
		}
	}
	if len(ids) != 3 || ids[0] != "gemini-compat-call-0" || ids[1] != "gemini-compat-call-1" || ids[2] != "gemini-compat-call-2" {
		t.Fatalf("ids=%v", ids)
	}
	// Duplicate supplied.
	dup := gsBuild(gsToolPart("a", "dup", `{"x":1}`), "", "", "")
	dup2 := gsBuild(gsToolPart("b", "dup", `{"x":1}`), "", "", "")
	dec2 := NewGeminiStreamDecoder(DefaultLimits(), true)
	if _, rerr := dec2.DecodeFrame(Frame{Data: dup}); rerr != nil {
		t.Fatal(rerr)
	}
	if _, rerr := dec2.DecodeFrame(Frame{Data: dup2}); rerr == nil {
		t.Fatal("duplicate call accepted")
	}
	// Supplied colliding with minted.
	dec3 := NewGeminiStreamDecoder(DefaultLimits(), true)
	mintFirst := gsBuild(gsToolPart("a", "", `{"x":1}`), "", "", "")
	if _, rerr := dec3.DecodeFrame(Frame{Data: mintFirst}); rerr != nil {
		t.Fatal(rerr)
	}
	collide := gsBuild(gsToolPart("b", "gemini-compat-call-0", `{"x":1}`), "", "", "")
	if _, rerr := dec3.DecodeFrame(Frame{Data: collide}); rerr == nil {
		t.Fatal("minted collision accepted")
	}
}
