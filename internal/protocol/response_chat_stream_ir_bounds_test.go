// Chat source stream IR bounds, provenance, pipeline and crosscheck tests.
//
// Bounds inherit the documented translator caps (per-call MaxArgsChars,
// aggregate MaxStreamArgsChars, MaxToolCalls, MaxStreamBlocks); text is
// unretained by design. The pipeline test proves prompt [DONE] return with
// no EOF read and no extra DecodeFrame calls. The crosscheck replays legacy
// pair-fixture wire shapes through the new decoder to prove source-fact
// compatibility without calling the old translators.
package protocol

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// --- Limits: per-call args, aggregate args, tool count, block count ---------

func TestChatStreamIRLimits(t *testing.T) {
	mkTool := func(idx int, id, args string) string {
		return csChunk(`{"tool_calls":[{"index":`+itoa(idx)+`,"id":"`+id+`","type":"function","function":{"name":"f","arguments":"`+args+`"}}]}`, "")
	}
	// Per-call args cap.
	lim := DefaultLimits()
	lim.MaxArgsChars = 8
	dec := NewChatStreamDecoder(lim)
	if _, rerr := dec.DecodeFrame(Frame{Data: mkTool(0, "c1", `{\"k\":\"0123456789\"}`)}); rerr == nil {
		t.Fatal("oversized per-call args accepted")
	}
	// Aggregate args cap across parallel calls.
	lim2 := DefaultLimits()
	lim2.MaxStreamArgsChars = 16
	dec2 := NewChatStreamDecoder(lim2)
	if _, rerr := dec2.DecodeFrame(Frame{Data: mkTool(0, "c1", `{\"a\":\"12345678\"}`)}); rerr != nil {
		t.Fatalf("first args under aggregate: %v", rerr)
	}
	if _, rerr := dec2.DecodeFrame(Frame{Data: mkTool(1, "c2", `{\"b\":\"1\"}`)}); rerr == nil {
		t.Fatal("args over the shared aggregate accepted")
	}
	// Tool count cap.
	lim3 := DefaultLimits()
	lim3.MaxToolCalls = 1
	dec3 := NewChatStreamDecoder(lim3)
	if _, rerr := dec3.DecodeFrame(Frame{Data: mkTool(0, "c1", ``)}); rerr != nil {
		t.Fatalf("first tool: %v", rerr)
	}
	if _, rerr := dec3.DecodeFrame(Frame{Data: mkTool(1, "c2", ``)}); rerr == nil {
		t.Fatal("tool call over MaxToolCalls accepted")
	}
	// Block cap covers text plus tools (shared, like the legacy emit path).
	lim4 := DefaultLimits()
	lim4.MaxStreamBlocks = 1
	dec4 := NewChatStreamDecoder(lim4)
	if _, rerr := dec4.DecodeFrame(Frame{Data: csChunk(`{"content":"hi"}`, "")}); rerr != nil {
		t.Fatalf("first block: %v", rerr)
	}
	if _, rerr := dec4.DecodeFrame(Frame{Data: mkTool(0, "c1", ``)}); rerr == nil {
		t.Fatal("block over MaxStreamBlocks accepted")
	}
	// Overlong opaque identity refused by the frame budget, not grammar.
	lim5 := DefaultLimits()
	lim5.MaxArgsChars = 8
	dec5 := NewChatStreamDecoder(lim5)
	if _, rerr := dec5.DecodeFrame(Frame{Data: mkTool(0, "123456789", ``)}); rerr == nil {
		t.Fatal("identity over the frame budget accepted")
	}
	// Malformed literal counters fail: fraction, quoted, overflow index.
	for _, raw := range []string{
		`{"id":"chatcmpl-7","object":"chat.completion.chunk","created":1.5,"model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":null}]}`,
		csChunk(`{"tool_calls":[{"index":"0","id":"c1","function":{"name":"f","arguments":""}}]}`, ""),
		csChunk(`{"tool_calls":[{"index":99999999999999999999,"id":"c1","function":{"name":"f","arguments":""}}]}`, ""),
		csChunk(`{"tool_calls":[{"id":"c1","function":{"name":"f","arguments":""}}]}`, ""),
	} {
		decN := NewChatStreamDecoder(DefaultLimits())
		if _, rerr := decN.DecodeFrame(Frame{Data: raw}); rerr == nil {
			t.Fatalf("malformed literal accepted: %.60s", raw)
		}
	}
	// Choice index must be zero.
	dec6 := NewChatStreamDecoder(DefaultLimits())
	if _, rerr := dec6.DecodeFrame(Frame{Data: `{"id":"chatcmpl-7","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":1,"delta":{},"finish_reason":null}]}`}); rerr == nil {
		t.Fatal("nonzero choice index accepted")
	}
}

// --- Usage provenance: carried buckets, failed unmapped shares --------------

func TestChatStreamIRUsageProvenance(t *testing.T) {
	full := func(usage string) []ResponseEvent {
		t.Helper()
		return csFullSuccess(t, []string{csChunk(`{"content":"hi"}`, ""), csChunk(`{}`, "stop"), csUsageChunk(usage), "[DONE]"})
	}
	// Scalar shares travel with Chat provenance for the target gate.
	evs := full(`{"prompt_tokens":12,"completion_tokens":7,"total_tokens":19,` +
		`"prompt_tokens_details":{"cached_tokens":4,"cache_write_tokens":1,"text_tokens":5},` +
		`"completion_tokens_details":{"reasoning_tokens":3,"text_tokens":2}}`)
	usage := evs[len(evs)-2].(UsageEvent).Usage
	if *usage.Prompt != 12 || *usage.Completion != 7 || *usage.Total != 19 {
		t.Fatalf("totals wrong: %+v", usage)
	}
	if *usage.CacheRead != 4 || *usage.CacheCreation != 1 || *usage.Reasoning != 3 {
		t.Fatalf("buckets not carried: %+v", usage)
	}
	if usage.TextTokens == nil || *usage.TextTokens != 7 {
		t.Fatalf("text shares not summed: %+v", usage)
	}
	if usage.Provenance.Source != DialectChat || usage.Provenance.Scope != UsageTerminal {
		t.Fatalf("provenance wrong: %+v", usage.Provenance)
	}
	if rerr := usage.ValidateAuthoritative(); rerr != nil {
		t.Fatalf("carried usage not authoritative: %v", rerr)
	}
	// Universally unmapped nonzero shares fail at the source for every target.
	for _, usage := range []string{
		`{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7,"prompt_tokens_details":{"audio_tokens":1}}`,
		`{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7,"prompt_tokens_details":{"image_tokens":2}}`,
		`{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7,"completion_tokens_details":{"audio_tokens":1}}`,
		`{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7,"completion_tokens_details":{"accepted_prediction_tokens":1}}`,
		`{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7,"completion_tokens_details":{"rejected_prediction_tokens":1}}`,
		`{"prompt_tokens":4,"completion_tokens":3,"total_tokens":8}`,
		`{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7,"prompt_tokens_details":{"cached_tokens":5}}`,
		`{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7,"completion_tokens_details":{"reasoning_tokens":4}}`,
		`{"prompt_tokens":1.5,"completion_tokens":1,"total_tokens":2}`,
		`{"prompt_tokens":"4","completion_tokens":3,"total_tokens":7}`,
		`{"prompt_tokens":99999999999999999999,"completion_tokens":1,"total_tokens":2}`,
	} {
		rerr := csMustFail(t, []string{csChunk(`{}`, "stop"), csUsageChunk(usage), "[DONE]"})
		_ = rerr
	}
	// Zero known buckets stay harmless at the source.
	evs = full(`{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3,` +
		`"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":0,"text_tokens":0}}`)
	if u := evs[len(evs)-2].(UsageEvent).Usage; u.Reasoning == nil || *u.Reasoning != 0 {
		t.Fatalf("explicit zero reasoning lost: %+v", u)
	}
}

// --- Upstream error chunks fail without downstream writes --------------------

func TestChatStreamIRErrorChunk(t *testing.T) {
	// Pre-commit error: nothing decoded, pipeline owns output.
	dec := NewChatStreamDecoder(DefaultLimits())
	if _, rerr := dec.DecodeFrame(Frame{Data: `{"error":{"code":500,"message":"boom"}}`}); rerr == nil {
		t.Fatal("upstream error chunk accepted")
	}
	// Post-text error: same failure, no invented events.
	dec2 := NewChatStreamDecoder(DefaultLimits())
	if _, rerr := dec2.DecodeFrame(Frame{Data: csChunk(`{"content":"hi"}`, "")}); rerr != nil {
		t.Fatalf("text frame: %v", rerr)
	}
	if _, rerr := dec2.DecodeFrame(Frame{Data: `{"error":{"code":500,"message":"boom"}}`}); rerr == nil {
		t.Fatal("post-text upstream error accepted")
	}
}

// --- Pipeline: prompt [DONE] return, no EOF read, no extra decodes -----------

type csCountingSource struct {
	inner FrameSource
	calls int
}

func (s *csCountingSource) Next() (Frame, *RouterError) {
	s.calls++
	return s.inner.Next()
}

type csRecordEncoder struct {
	events    []ResponseEvent
	committed bool
	aborts    int
}

func (e *csRecordEncoder) Encode(w io.Writer, flush func(), ev ResponseEvent) *EncodeError {
	e.events = append(e.events, ev)
	e.committed = true
	return nil
}

func (e *csRecordEncoder) Committed() bool { return e.committed }

func (e *csRecordEncoder) Abort(w io.Writer, flush func()) { e.aborts++ }

func TestChatStreamIRPipelinePromptDone(t *testing.T) {
	lim := DefaultLimits()
	raw := "data: " + csChunk(`{"content":"Hello"}`, "") + "\n\n" +
		"data: " + csChunk(`{}`, "stop") + "\n\n" +
		"data: " + csUsageChunk(csUsage(9, 3)) + "\n\n" +
		"data: [DONE]\n\n" +
		"data: next-generation-remainder-without-terminator"
	src := &csCountingSource{inner: NewSSEFrameReader(strings.NewReader(raw), lim)}
	dec := NewChatStreamDecoder(lim)
	enc := &csRecordEncoder{}
	var out bytes.Buffer
	if rerr := TranslateStream(src, dec, enc, &out, nil, lim, DialectChat); rerr != nil {
		t.Fatalf("TranslateStream: %v", rerr)
	}
	// Four framed units consumed (three chunks + [DONE]); the trailing
	// remainder stays unread: no EOF read past the terminal.
	if src.calls != 4 {
		t.Fatalf("DecodeFrame calls = %d, want 4 (no read past [DONE])", src.calls)
	}
	if enc.aborts != 0 {
		t.Fatalf("success path aborted: %d", enc.aborts)
	}
	// Text is delivered before completion, usage is terminal-authoritative.
	var sawText, sawUsage, sawComplete int
	for i, ev := range enc.events {
		switch ev.(type) {
		case TextFragment:
			sawText = i + 1
		case UsageEvent:
			sawUsage = i + 1
		case CompletionEvent:
			sawComplete = i + 1
		}
	}
	if sawText == 0 || sawUsage == 0 || sawComplete == 0 {
		t.Fatalf("missing pipeline events: text=%d usage=%d complete=%d", sawText, sawUsage, sawComplete)
	}
	if !(sawText < sawUsage && sawUsage < sawComplete) {
		t.Fatalf("pipeline order wrong: text=%d usage=%d complete=%d", sawText, sawUsage, sawComplete)
	}
	if got := enc.events[sawUsage-1].(UsageEvent).Usage; *got.Prompt != 9 || *got.Completion != 3 {
		t.Fatalf("pipeline usage wrong: %+v", got)
	}
}

// --- Crosscheck: legacy pair-fixture wire shapes, new decoder only -----------

func TestChatStreamIRCrosscheckLegacyFixtures(t *testing.T) {
	// Mirrors TestChatSSEFragmentedParallelTools wire (anthropic ingress
	// suite) through the shared decoder: same accept, same event facts.
	evs := csFullSuccess(t, []string{
		csChunk(`{"role":"assistant"}`, ""),
		csChunk(`{"tool_calls":[{"index":0,"id":"call_A","type":"function","function":{"name":"get_time","arguments":""}}]}`, ""),
		csChunk(`{"tool_calls":[{"index":1,"id":"call_B","type":"function","function":{"name":"calc","arguments":""}}]}`, ""),
		csChunk(`{"tool_calls":[{"index":0,"function":{"arguments":"{\"tz\":"}}]}`, ""),
		csChunk(`{"tool_calls":[{"index":1,"function":{"arguments":"{\"n\":9007199254740993}"}},{"index":0,"function":{"arguments":"\"UTC\"}"}}]}`, ""),
		csChunk(`{}`, "tool_calls"),
		csUsageChunk(csUsage(30, 20)),
		"[DONE]",
	})
	var names, callIDs []string
	for _, ev := range evs {
		if open, ok := ev.(BlockOpen); ok && open.Kind == BlockTool {
			names = append(names, open.Name)
			callIDs = append(callIDs, string(open.Call))
		}
	}
	if len(names) != 2 || names[0] != "get_time" || names[1] != "calc" {
		t.Fatalf("legacy tool order/identity lost: %q", names)
	}
	if callIDs[0] != "call_A" || callIDs[1] != "call_B" {
		t.Fatalf("legacy call IDs lost: %q", callIDs)
	}
	// Mirrors TestChatSSETextLifecycle: empty content delta emits nothing but
	// stays valid; terminal usage + stop close the stream.
	evs = csFullSuccess(t, []string{
		csChunk(`{"role":"assistant","content":""}`, ""),
		csChunk(`{"content":"Hello"}`, ""),
		csChunk(`{"content":"!"}`, ""),
		csChunk(`{}`, "stop"),
		csUsageChunk(csUsage(25, 15)),
		"[DONE]",
	})
	var texts []string
	for _, ev := range evs {
		if frag, ok := ev.(TextFragment); ok {
			texts = append(texts, frag.Text)
		}
	}
	if len(texts) != 2 || texts[0] != "Hello" || texts[1] != "!" {
		t.Fatalf("legacy text deltas lost: %q", texts)
	}
	// Mirrors TestChatSSEUnrepresentableFails mustFail set: same rejects.
	term := csTerm("stop", 1, 1)
	for name, datas := range map[string][]string{
		"multichoice": {`{"id":"chatcmpl-7","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":null},{"index":1,"delta":{},"finish_reason":null}]}`},
		"refusal":     append([]string{csChunk(`{"refusal":"no"}`, "")}, term...),
		"dup-finish":  {csChunk(`{}`, "stop"), csChunk(`{}`, "stop"), csUsageChunk(csUsage(1, 1)), "[DONE]"},
		"dup-id": {
			csChunk(`{"tool_calls":[{"index":0,"id":"c1","function":{"name":"a","arguments":""}}]}`, ""),
			csChunk(`{"tool_calls":[{"index":1,"id":"c1","function":{"name":"b","arguments":""}}]}`, ""),
		},
		"truncated": {csChunk(`{"content":"Hi"}`, ""), csChunk(`{}`, "stop")},
		"no-finish": {csChunk(`{"content":"Hi"}`, ""), "[DONE]"},
		"no-usage":  {csChunk(`{"content":"Hi"}`, ""), csChunk(`{}`, "stop"), "[DONE]"},
	} {
		if rerr := csMustFail(t, datas); rerr == nil {
			t.Fatalf("legacy fixture %s accepted", name)
		}
	}
}

// --- Empty-object fallback is canonical: emitted once, counted twice ------

func TestChatStreamIREmptyArgsBoundCountsFallback(t *testing.T) {
	lim := DefaultLimits()
	lim.MaxStreamArgsChars = 1
	dec := NewChatStreamDecoder(lim)
	datas := []string{
		csChunk(`{"tool_calls":[{"index":0,"id":"c","function":{"name":"f","arguments":""}}]}`, ""),
		csChunk(`{}`, "tool_calls"),
		csUsageChunk(csUsage(1, 1)),
		"[DONE]",
	}
	for i, d := range datas[:3] {
		if _, rerr := dec.DecodeFrame(Frame{Data: d}); rerr != nil {
			t.Fatalf("frame %d: %v", i, rerr)
		}
	}
	// The canonical '{}' fallback carries 2 bytes, which exceed the
	// aggregate bound of 1: [DONE] must fail closed with an exact 502.
	if _, rerr := dec.DecodeFrame(Frame{Data: "[DONE]"}); rerr == nil {
		t.Fatal("[DONE] with over-budget empty-args fallback accepted")
	} else if rerr.Status != 502 {
		t.Fatalf("[DONE]: status=%d, want 502 (%v)", rerr.Status, rerr)
	}
	// The same 2 bytes count per call: a per-call bound of 1 refuses
	// the fallback even with a wide-open aggregate. Tiny opaque IDs
	// isolate the fallback bytes from the shared per-field budget.
	tinyChunk := func(delta, finish string) string {
		ch := `{"id":"i","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":` + delta
		if finish == "" {
			ch += `,"finish_reason":null`
		} else {
			ch += `,"finish_reason":"` + finish + `"`
		}
		return ch + `}]}`
	}
	tinyUsage := `{"id":"i","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	lim2 := DefaultLimits()
	lim2.MaxArgsChars = 1
	dec2 := NewChatStreamDecoder(lim2)
	for _, d := range []string{
		tinyChunk(`{"tool_calls":[{"index":0,"id":"c","function":{"name":"f","arguments":""}}]}`, ""),
		tinyChunk(`{}`, "tool_calls"),
		tinyUsage,
	} {
		if _, rerr := dec2.DecodeFrame(Frame{Data: d}); rerr != nil {
			t.Fatalf("per-call setup frame: %v", rerr)
		}
	}
	if _, rerr := dec2.DecodeFrame(Frame{Data: "[DONE]"}); rerr == nil {
		t.Fatal("[DONE] with over-budget per-call fallback accepted")
	} else if rerr.Status != 502 {
		t.Fatalf("[DONE] per-call: status=%d, want 502 (%v)", rerr.Status, rerr)
	}
}

func TestChatStreamIREmptyArgsFallbackOnce(t *testing.T) {
	evs := csFullSuccess(t, []string{
		csChunk(`{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"f","arguments":""}}]}`, ""),
		csChunk(`{}`, "tool_calls"),
		csUsageChunk(csUsage(5, 2)),
		"[DONE]",
	})
	var frags []ArgsFragment
	var closes int
	for _, ev := range evs {
		switch e := ev.(type) {
		case ArgsFragment:
			if e.Item == "c0" {
				frags = append(frags, e)
			}
		case BlockClose:
			if e.Item == "c0" {
				closes++
			}
		}
	}
	if len(frags) != 1 || frags[0].Fragment != "{}" {
		t.Fatalf("empty-args fallback = %+v, want exactly one '{}' fragment", frags)
	}
	if closes != 1 {
		t.Fatalf("empty-args closes = %d, want 1", closes)
	}
	// The fallback flows through validation coherently: the lifecycle
	// counts exactly its 2 bytes and the terminal buffer reconstructs
	// the same canonical object.
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
	if len(res.Blocks) != 1 || res.Blocks[0].ArgsBytes != 2 {
		t.Fatalf("lifecycle args bytes = %+v, want one block with 2", res.Blocks)
	}
	buf := NewTerminalBuffer(DefaultLimits())
	for _, ev := range evs {
		switch e := ev.(type) {
		case ArgsFragment:
			if rerr := buf.ObserveArgs(e.Item, e.Fragment); rerr != nil {
				t.Fatalf("buffer ObserveArgs: %v", rerr)
			}
		}
	}
	got, rerr := buf.ArgsResult("c0")
	if rerr != nil {
		t.Fatalf("buffer ArgsResult: %v", rerr)
	}
	if string(got) != "{}" {
		t.Fatalf("buffer ArgsResult = %s, want {}", got)
	}
}

func TestChatStreamIRExplicitEmptyObjectCountedOnce(t *testing.T) {
	evs := csFullSuccess(t, []string{
		csChunk(`{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}`, ""),
		csChunk(`{}`, "tool_calls"),
		csUsageChunk(csUsage(5, 2)),
		"[DONE]",
	})
	var frags int
	for _, ev := range evs {
		if e, ok := ev.(ArgsFragment); ok && e.Item == "c0" {
			frags++
			if e.Fragment != "{}" {
				t.Fatalf("explicit empty-object fragment = %q, want {}", e.Fragment)
			}
		}
	}
	if frags != 1 {
		t.Fatalf("explicit '{}' fragments = %d, want exactly 1 (no fallback duplication)", frags)
	}
}
