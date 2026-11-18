// Anthropic source stream IR gates: duplicates, coherence, unsupported
// state, cumulative usage, terminal gates, malformed classification,
// bounds, truncation, and pipeline traces.
//
// Builders and success/failure drivers (asFr, asFullSuccess, asFailAt,
// asFail) live in response_anthropic_stream_ir_test.go; this file only
// adds gate coverage for the same decoder.
package protocol

import (
	"strings"
	"testing"
)

func TestAnthropicStreamDuplicateAndUnknownBlocks(t *testing.T) {
	start := asFr("message_start", asStartData(asMinMsg(`"m1"`, `{"input_tokens":9}`)))
	tool := func(idx int, id string) Frame {
		return asFr("content_block_start", asBlockStart(idx, `{"type":"tool_use","id":"`+id+`","name":"f","input":{}}`))
	}
	open := func(idx int) Frame {
		return asFr("content_block_start", asBlockStart(idx, `{"type":"text","text":""}`))
	}
	// Duplicate source index.
	asFailAt(t, []Frame{start, open(0), open(0)}, 2)
	// Duplicate global call ID across distinct indices.
	asFailAt(t, []Frame{start, tool(0, "dup"), tool(1, "dup")}, 2)
	// Delta and stop for unknown (never opened, already closed) blocks.
	asFailAt(t, []Frame{start, asFr("content_block_delta", asDelta(7, `{"type":"text_delta","text":"x"}`))}, 1)
	asFailAt(t, []Frame{start, open(0), asFr("content_block_stop", asBlockStop(0)), asFr("content_block_stop", asBlockStop(0))}, 3)
}

func TestAnthropicStreamStartFirstOnce(t *testing.T) {
	start := asFr("message_start", asStartData(asMinMsg(`"m1"`, `{"input_tokens":9}`)))
	// Content before start.
	asFailAt(t, []Frame{asFr("content_block_delta", asDelta(0, `{"type":"text_delta","text":"x"}`))}, 0)
	asFailAt(t, []Frame{asFr("message_stop", `{"type":"message_stop"}`)}, 0)
	// Duplicate start.
	asFailAt(t, []Frame{start, start}, 1)
	// Stop before any delta (no stop reason, no usage): truncation.
	asFail(t, []Frame{start, asFr("message_stop", `{"type":"message_stop"}`)})
}

// --- Event/type coherence, unknown fields, ping, error ---------------------

func TestAnthropicStreamEnvelopeCoherence(t *testing.T) {
	start := asStartData(asMinMsg(`"m1"`, `{"input_tokens":9}`))
	good := asFr("message_start", start)
	// Mismatched payload type.
	asFailAt(t, []Frame{asFr("message_start", `{"type":"message_stop"}`)}, 0)
	// Missing payload type.
	asFailAt(t, []Frame{asFr("message_start", `{"message":{"id":"m1"}}`)}, 0)
	// Unknown SSE event.
	asFailAt(t, []Frame{good, asFr("message_foo", `{"type":"message_foo"}`)}, 1)
	// Unknown members fail with a value-free field path.
	asFailAt(t, []Frame{asFr("message_start", `{"type":"message_start","message":{"id":"m1"},"future":1}`)}, 0)
	asFailAt(t, []Frame{good, asFr("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text"},"future":1}`)}, 1)
	// Data-only frames carry no event identity.
	dec := NewAnthropicStreamDecoder(DefaultLimits())
	if _, rerr := dec.DecodeFrame(Frame{Data: start}); rerr == nil {
		t.Fatal("data-only frame must fail")
	}
	// Event-only non-ping frames are malformed.
	asFailAt(t, []Frame{good, Frame{Event: "message_delta", HasEvent: true}}, 1)
	// Cross-kind members fail closed.
	asFailAt(t, []Frame{good, asFr("content_block_start", asBlockStart(0, `{"type":"text","text":"x","id":"c1"}`))}, 1)
	asFailAt(t, []Frame{good, asFr("content_block_start", asBlockStart(0, `{"type":"tool_use","id":"c1","name":"f","input":{},"text":"x"}`))}, 1)
}

func TestAnthropicStreamPingAndError(t *testing.T) {
	dec := NewAnthropicStreamDecoder(DefaultLimits())
	// Pings are keepalives at any point, including before start.
	for _, data := range []string{"", "   ", `{"type":"ping"}`} {
		if evs, rerr := dec.DecodeFrame(asFr("ping", data)); rerr != nil || len(evs) != 0 {
			t.Fatalf("ping %q: evs=%v err=%v", data, evs, rerr)
		}
	}
	if _, rerr := dec.DecodeFrame(asFr("ping", `{"type":"ping","future":1}`)); rerr == nil {
		t.Fatal("ping with unknown member must fail")
	}
	if _, rerr := dec.DecodeFrame(asFr("error", `{"type":"error"}`)); rerr == nil {
		t.Fatal("error event must fail")
	} else if !strings.Contains(rerr.Msg, "reported an error") {
		t.Fatalf("error diagnostic wrong: %v", rerr)
	}
}

// --- Unsupported source-private state fails, never drops ------------------

func TestAnthropicStreamUnsupportedKinds(t *testing.T) {
	start := asFr("message_start", asStartData(asMinMsg(`"m1"`, `{"input_tokens":9}`)))
	bads := []string{
		`{"type":"thinking","thinking":"hmm"}`,
		`{"type":"redacted_thinking","data":"x"}`,
		`{"type":"server_tool_use","id":"s","name":"search","input":{}}`,
		`{"type":"web_search_tool_result","tool_use_id":"s","content":[]}`,
		`{"type":"image","source":{}}`,
		`{"type":"document","source":{}}`,
		`{"type":"mystery","x":1}`,
	}
	for _, cb := range bads {
		asFailAt(t, []Frame{start, asFr("content_block_start", asBlockStart(0, cb))}, 1)
	}
	// Non-empty citations have no representation.
	asFailAt(t, []Frame{start, asFr("content_block_start", asBlockStart(0, `{"type":"text","text":"hi","citations":[{"type":"x"}]}`))}, 1)
	// Unrepresentable delta kinds fail value-free.
	for _, dtype := range []string{"thinking_delta", "signature_delta", "citations_delta"} {
		frames := []Frame{
			start,
			asFr("content_block_start", asBlockStart(0, `{"type":"text","text":""}`)),
			asFr("content_block_delta", asDelta(0, `{"type":"`+dtype+`"}`)),
		}
		if rerr := asFailAt(t, frames, 2); strings.Contains(rerr.Msg, dtype) {
			t.Fatalf("delta kind leaked into diagnostic: %v", rerr)
		}
	}
	// Kind-crossed deltas fail as malformed.
	frames := []Frame{
		start,
		asFr("content_block_start", asBlockStart(0, `{"type":"tool_use","id":"t1","name":"f","input":{}}`)),
		asFr("content_block_delta", asDelta(0, `{"type":"text_delta","text":"x"}`)),
	}
	asFailAt(t, frames, 2)
}

// --- Cumulative usage: authoritative last, refresh semantics ----------------

func TestAnthropicStreamCumulativeUsage(t *testing.T) {
	frames := []Frame{
		asFr("message_start", asStartData(asMinMsg(`"m1"`, `{"input_tokens":10,"cache_creation_input_tokens":1,"cache_read_input_tokens":2}`))),
		asFr("message_delta", asMsgDelta(`{}`, `{"output_tokens":2}`)),
		asFr("message_delta", asMsgDelta(`{"stop_reason":"end_turn"}`, `{"input_tokens":20,"output_tokens":4}`)),
		asFr("message_stop", `{"type":"message_stop"}`),
	}
	evs, _ := asFullSuccess(t, frames)
	// No events flow from intermediate deltas; only the stop emits.
	if len(evs) != 3 {
		t.Fatalf("events = %d, want start+usage+completion: %#v", len(evs), evs)
	}
	ue := evs[1].(UsageEvent)
	u := ue.Usage
	// Refresh: explicit input overwrites, preserved cache shares persist,
	// inclusive prompt counted once: 20+1+2=23, total 23+4=27.
	if *u.Prompt != 23 || *u.Completion != 4 || *u.Total != 27 {
		t.Fatalf("cumulative usage wrong: %+v", u)
	}
	if *u.CacheCreation != 1 || *u.CacheRead != 2 {
		t.Fatalf("cache shares lost: %+v", u)
	}
}

func TestAnthropicStreamTerminalGates(t *testing.T) {
	start := asFr("message_start", asStartData(asMinMsg(`"m1"`, `{"input_tokens":9}`)))
	usage := func(out string) Frame {
		return asFr("message_delta", asMsgDelta(`{"stop_reason":"end_turn"}`, out))
	}
	// Missing usage and message_delta with open blocks fail immediately.
	asFailAt(t, []Frame{start, asFr("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`)}, 1)
	asFailAt(t, []Frame{
		start,
		asFr("content_block_start", asBlockStart(0, `{"type":"text","text":""}`)),
		usage(`{"output_tokens":1}`),
		asFr("message_stop", `{"type":"message_stop"}`),
	}, 2)
	if rerr := asFailAt(t, []Frame{start, usage(`{"output_tokens":1,"future":1}`), asFr("message_stop", `{"type":"message_stop"}`)}, 1); rerr == nil {
		t.Fatal("unknown usage member must fail")
	}
	// Unknown stops fail with a static diagnostic at message_stop.
	for _, stop := range []string{"pause_turn", "model_context_window_exceeded", "stop", "STOP", ""} {
		delta := `{"stop_reason":"` + stop + `"}`
		if stop == "" {
			delta = `{}`
		}
		rerr := asFail(t, []Frame{start, asFr("message_delta", asMsgDelta(delta, `{"output_tokens":1}`)), asFr("message_stop", `{"type":"message_stop"}`)})
		if stop != "" && strings.Contains(rerr.Msg, stop) && stop != "stop" {
			t.Fatalf("unknown stop leaked: %v", rerr)
		}
	}
	// Reviewed stops map through the shared finish with provenance.
	for _, tc := range []struct {
		stop string
		kind FinishKind
	}{
		{"end_turn", FinishStop}, {"stop_sequence", FinishStop}, {"max_tokens", FinishLength},
		{"tool_use", FinishToolCalls}, {"refusal", FinishRefusal},
	} {
		frames := []Frame{start, asFr("message_delta", asMsgDelta(`{"stop_reason":"`+tc.stop+`"}`, `{"output_tokens":1}`)), asFr("message_stop", `{"type":"message_stop"}`)}
		evs, _ := asFullSuccess(t, frames)
		ce := evs[len(evs)-1].(CompletionEvent)
		if ce.Finish.Kind != tc.kind || ce.Finish.Raw != tc.stop {
			t.Fatalf("stop %q -> %+v, want kind %q", tc.stop, ce.Finish, tc.kind)
		}
	}
}

// --- Malformed classification vs 502 identity/syntax/size ------------------

func TestAnthropicStreamMalformedClassification(t *testing.T) {
	frames := []Frame{
		asFr("message_start", asStartData(asMinMsg(`"m1"`, `{"input_tokens":9}`))),
		asFr("content_block_start", asBlockStart(0, `{"type":"tool_use","id":"t1","name":"f","input":{}}`)),
		asFr("content_block_delta", asDelta(0, `{"type":"input_json_delta","partial_json":"{\"a\":"}`)),
		asFr("content_block_delta", asDelta(0, `{"type":"input_json_delta","partial_json":"}"}`)),
		asFr("content_block_stop", asBlockStop(0)),
		asFr("message_delta", asMsgDelta(`{"stop_reason":"end_turn"}`, `{"output_tokens":1}`)),
		asFr("message_stop", `{"type":"message_stop"}`),
	}
	evs, _ := asFullSuccess(t, frames)
	// Ordinary malformed fragment bytes classify the block; the stream
	// stays structurally valid through usage plus completion.
	var sawMalformed, sawClose, sawFallback bool
	for _, ev := range evs {
		switch e := ev.(type) {
		case MalformedToolEvent:
			sawMalformed = string(e.Item) == "b0"
		case BlockClose:
			sawClose = true
		case ArgsFragment:
			if e.Fragment == "{}" {
				sawFallback = true
			}
		}
	}
	if !sawMalformed || !sawClose || sawFallback {
		t.Fatalf("malformed=%v close=%v fallback=%v: %#v", sawMalformed, sawClose, sawFallback, evs)
	}
	lc := NewResponseLifecycle(DefaultLimits(), DialectAnthropic)
	for _, ev := range evs {
		if rerr := lc.Observe(ev); rerr != nil {
			t.Fatalf("Observe(%T): %v", ev, rerr)
		}
	}
	if res, rerr := lc.Result(); rerr != nil || res.MalformedTools != 1 {
		t.Fatalf("malformed lifecycle: %+v %v", res, rerr)
	}
}

func TestAnthropicStreamSyntaxStays502(t *testing.T) {
	start := asFr("message_start", asStartData(asMinMsg(`"m1"`, `{"input_tokens":9}`)))
	// Malformed seeded input (non-object, invalid JSON) is syntax: 502.
	for _, input := range []string{`[1]`, `7`, `"{}"`, `{"a":}`} {
		asFailAt(t, []Frame{start, asFr("content_block_start", asBlockStart(0, `{"type":"tool_use","id":"t1","name":"f","input":`+input+`}`))}, 1)
	}
	// Missing/empty/whitespace tool identity is 502, never classified.
	for _, cb := range []string{
		`{"type":"tool_use","name":"f","input":{}}`,
		`{"type":"tool_use","id":"","name":"f","input":{}}`,
		`{"type":"tool_use","id":"   ","name":"f","input":{}}`,
		`{"type":"tool_use","id":"t1","name":"","input":{}}`,
	} {
		asFailAt(t, []Frame{start, asFr("content_block_start", asBlockStart(0, cb))}, 1)
	}
}

// --- All bounds -------------------------------------------------------------

func TestAnthropicStreamBounds(t *testing.T) {
	start := func() Frame {
		return asFr("message_start", asStartData(asMinMsg(`"m1"`, `{"input_tokens":9}`)))
	}
	tool := func(idx int, id, input string) Frame {
		return asFr("content_block_start", asBlockStart(idx, `{"type":"tool_use","id":"`+id+`","name":"f","input":`+input+`}`))
	}
	frag := func(idx int, n int) Frame {
		return asFr("content_block_delta", asDelta(idx, `{"type":"input_json_delta","partial_json":"`+strings.Repeat("x", n)+`"}`))
	}
	run := func(lim Limits, frames []Frame) *RouterError {
		dec := NewAnthropicStreamDecoder(lim)
		for _, f := range frames {
			if _, rerr := dec.DecodeFrame(f); rerr != nil {
				return rerr
			}
		}
		_, rerr := dec.Close()
		return rerr
	}
	// Per-call: one field over MaxArgsChars fails.
	small := DefaultLimits()
	small.MaxArgsChars = 10
	if rerr := run(small, []Frame{start(), tool(0, "t1", `{"a":"123456789012345"}`)}); rerr == nil {
		t.Fatal("oversized seed must fail")
	}
	if rerr := run(small, []Frame{start(), tool(0, "t1", `{}`), frag(0, 11)}); rerr == nil {
		t.Fatal("oversized fragment must fail")
	}
	// Aggregate: two small fields over the shared budget fail.
	small = DefaultLimits()
	small.MaxArgsChars = 100
	small.MaxStreamArgsChars = 20
	if rerr := run(small, []Frame{start(), tool(0, "t1", `{"a":"123456"}`), tool(1, "t2", `{"b":"123456"}`)}); rerr == nil {
		t.Fatal("aggregate args must fail")
	}
	// Distinct blocks and tool-call counts.
	small = DefaultLimits()
	small.MaxStreamBlocks = 1
	if rerr := run(small, []Frame{
		start(),
		asFr("content_block_start", asBlockStart(0, `{"type":"text","text":""}`)),
		asFr("content_block_start", asBlockStart(1, `{"type":"text","text":""}`)),
	}); rerr == nil {
		t.Fatal("block bound must fail")
	}
	small = DefaultLimits()
	small.MaxToolCalls = 1
	if rerr := run(small, []Frame{start(), tool(0, "t1", `{}`), tool(1, "t2", `{}`)}); rerr == nil {
		t.Fatal("tool count must fail")
	}
	// String IDs bounded, never by the 64-char name grammar.
	small = DefaultLimits()
	small.MaxArgsChars = 8
	if rerr := run(small, []Frame{asFr("message_start", asStartData(asMinMsg(`"123456789"`, `{"input_tokens":9}`)))}); rerr == nil {
		t.Fatal("oversized start id must fail")
	}
	if rerr := run(small, []Frame{start(), tool(0, "123456789", `{}`)}); rerr == nil {
		t.Fatal("oversized call id must fail")
	}
}

// --- Truncation: no invented terminal ---------------------------------------

func TestAnthropicStreamTruncation(t *testing.T) {
	start := asFr("message_start", asStartData(asMinMsg(`"m1"`, `{"input_tokens":9}`)))
	// EOF after content, after a delta, or after usage-but-no-stop all
	// truncate: Close fails and no CompletionEvent was ever emitted.
	for _, frames := range [][]Frame{
		{start},
		{start, asFr("content_block_start", asBlockStart(0, `{"type":"text","text":"x"}`))},
		{start, asFr("message_delta", asMsgDelta(`{"stop_reason":"end_turn"}`, `{"output_tokens":1}`))},
	} {
		dec := NewAnthropicStreamDecoder(DefaultLimits())
		for _, f := range frames {
			if _, rerr := dec.DecodeFrame(f); rerr != nil {
				t.Fatalf("setup frame failed: %v", rerr)
			}
		}
		if _, rerr := dec.Close(); rerr == nil {
			t.Fatalf("truncation must fail: %v", frames)
		}
	}
}

// --- Valid terminal: Close reads nothing, pipeline stops promptly -----------

func TestAnthropicStreamTerminalNoEOF(t *testing.T) {
	dec := NewAnthropicStreamDecoder(DefaultLimits())
	for _, f := range asOfficialFrames() {
		if _, rerr := dec.DecodeFrame(f); rerr != nil {
			t.Fatalf("DecodeFrame: %v", rerr)
		}
	}
	// A keepalive ping raced with completion stays harmless; anything
	// else after completion fails.
	if evs, rerr := dec.DecodeFrame(asFr("ping", "")); rerr != nil || len(evs) != 0 {
		t.Fatalf("post-terminal ping: %v %v", evs, rerr)
	}
	if _, rerr := dec.DecodeFrame(asFr("ping", "")); rerr != nil {
		t.Fatalf("second ping: %v", rerr)
	}
	if _, rerr := dec.DecodeFrame(asFr("message_delta", asMsgDelta(`{"stop_reason":"end_turn"}`, `{"output_tokens":1}`))); rerr == nil {
		t.Fatal("post-terminal delta must fail")
	}
	if tail, rerr := dec.Close(); rerr != nil || len(tail) != 0 {
		t.Fatalf("Close after valid stop: %v %v", tail, rerr)
	}
}

func TestAnthropicStreamPipelineTrace(t *testing.T) {
	var sb strings.Builder
	write := func(event, data string) {
		sb.WriteString("event: " + event + "\ndata: " + data + "\n\n")
	}
	write("message_start", asStartData(asMinMsg(`"msg_7"`, `{"input_tokens":3}`)))
	write("content_block_start", asBlockStart(0, `{"type":"text","text":""}`))
	write("content_block_delta", asDelta(0, `{"type":"text_delta","text":"yo"}`))
	write("content_block_stop", asBlockStop(0))
	write("message_delta", asMsgDelta(`{"stop_reason":"end_turn"}`, `{"output_tokens":2}`))
	write("message_stop", `{"type":"message_stop"}`)
	lim := DefaultLimits()
	enc := &bufferEncoder{}
	var out strings.Builder
	if rerr := TranslateStream(sseSource(sb.String(), lim), NewAnthropicStreamDecoder(lim), enc, &out, nil, lim, DialectAnthropic); rerr != nil {
		t.Fatalf("TranslateStream: %v", rerr)
	}
	if enc.aborts != 0 {
		t.Fatalf("aborts = %d, want none on success", enc.aborts)
	}
	// The terminal pair is the last two encoded events: authoritative
	// usage, then completion. No EOF read was needed past them.
	if len(enc.events) != 6 {
		t.Fatalf("events = %d, want 6: %#v", len(enc.events), enc.events)
	}
	ue, ok := enc.events[4].(UsageEvent)
	if !ok || !ue.Terminal() || *ue.Usage.Prompt != 3 || *ue.Usage.Completion != 2 {
		t.Fatalf("pipeline usage wrong: %#v", enc.events[4])
	}
	ce, ok := enc.events[5].(CompletionEvent)
	if !ok || ce.Finish.Kind != FinishStop || ce.Finish.Raw != "end_turn" {
		t.Fatalf("pipeline completion wrong: %#v", enc.events[5])
	}
	start := enc.events[0].(ResponseStart)
	if start.Model != "" || string(start.ID) != "msg_7" {
		t.Fatalf("pipeline start wrong: %+v", start)
	}
}
