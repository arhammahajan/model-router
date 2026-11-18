// Anthropic source stream IR tests: direct frame -> semantic traces.
//
// These tests drive NewAnthropicStreamDecoder directly, independent of any
// target: every assertion pins source-wire validation (envelopes,
// allowlists, finish and usage gates, identity rules, malformed
// classification) without calling any legacy pair translator. The full
// stream replays the official legacy-compatibility wire shape
// (chat_sse_text.json: model/type/role omitted from message_start) plus
// tool and cumulative-usage coverage through the same decoder.
package protocol

import (
	"strings"
	"testing"
)

// --- Independent wire builders (not shared with pair tests) ---------------

func asFr(event, data string) Frame { return Frame{Event: event, HasEvent: true, Data: data} }

func asStartData(msg string) string { return `{"type":"message_start","message":` + msg + `}` }

func asMinMsg(id, usage string) string { return `{"id":` + id + `,"usage":` + usage + `}` }

func asStdMsg(model, usage string) string {
	return `{"id":"msg_9","type":"message","role":"assistant","model":` + model + `,"content":[],"stop_reason":null,"stop_sequence":null,"usage":` + usage + `}`
}

func asBlockStart(idx int, cb string) string {
	return `{"type":"content_block_start","index":` + itoa(idx) + `,"content_block":` + cb + `}`
}

func asDelta(idx int, delta string) string {
	return `{"type":"content_block_delta","index":` + itoa(idx) + `,"delta":` + delta + `}`
}

func asBlockStop(idx int) string { return `{"type":"content_block_stop","index":` + itoa(idx) + `}` }

func asMsgDelta(delta, usage string) string {
	return `{"type":"message_delta","delta":` + delta + `,"usage":` + usage + `}`
}

// asFullSuccess decodes a complete stream and validates Close-after-success:
// Close reads nothing and returns no events.
func asFullSuccess(t *testing.T, frames []Frame) ([]ResponseEvent, *AnthropicStreamDecoder) {
	t.Helper()
	dec := NewAnthropicStreamDecoder(DefaultLimits())
	var all []ResponseEvent
	for _, f := range frames {
		evs, rerr := dec.DecodeFrame(f)
		if rerr != nil {
			t.Fatalf("DecodeFrame(%s %q): %v", f.Event, f.Data, rerr)
		}
		all = append(all, evs...)
	}
	tail, rerr := dec.Close()
	if rerr != nil {
		t.Fatalf("Close: %v", rerr)
	}
	if len(tail) != 0 {
		t.Fatalf("Close returned %d events, want none", len(tail))
	}
	return all, dec
}

// asFailAt asserts DecodeFrame itself fails at the exact frame index,
// proving the targeted guard fired rather than a later truncation masking
// a removed check.
func asFailAt(t *testing.T, frames []Frame, at int) *RouterError {
	t.Helper()
	dec := NewAnthropicStreamDecoder(DefaultLimits())
	for i, f := range frames {
		if _, rerr := dec.DecodeFrame(f); rerr != nil {
			if i != at {
				t.Fatalf("failure at frame %d, want guard at frame %d (%v)", i, at, rerr)
			}
			if rerr.Status != 502 {
				t.Fatalf("frame %d: status=%d, want 502 (%v)", i, rerr.Status, rerr)
			}
			return rerr
		}
	}
	t.Fatalf("expected 502 at frame %d, all frames accepted", at)
	return nil
}

// asFail asserts a 502 somewhere in the frame sequence, or at Close for
// truncated streams that decode without error but never terminate.
func asFail(t *testing.T, frames []Frame) *RouterError {
	t.Helper()
	dec := NewAnthropicStreamDecoder(DefaultLimits())
	for i, f := range frames {
		if _, rerr := dec.DecodeFrame(f); rerr != nil {
			if rerr.Status != 502 {
				t.Fatalf("frame %d: status=%d, want 502 (%v)", i, rerr.Status, rerr)
			}
			return rerr
		}
	}
	if _, rerr := dec.Close(); rerr != nil {
		if rerr.Status != 502 {
			t.Fatalf("Close: status=%d, want 502 (%v)", rerr.Status, rerr)
		}
		return rerr
	}
	t.Fatalf("expected 502 failure, stream accepted")
	return nil
}

func asStart(t *testing.T, ev ResponseEvent) ResponseStart {
	t.Helper()
	start, ok := ev.(ResponseStart)
	if !ok {
		t.Fatalf("first event = %T, want ResponseStart", ev)
	}
	if start.Source != DialectAnthropic {
		t.Fatalf("start source = %q, want anthropic", start.Source)
	}
	return start
}

// --- Official full stream: legacy-minimal start, text, tool, usage, stop --

func asOfficialFrames() []Frame {
	return []Frame{
		asFr("message_start", asStartData(asMinMsg(`"msg_1"`, `{"input_tokens":5}`))),
		asFr("content_block_start", asBlockStart(0, `{"type":"text","text":""}`)),
		asFr("content_block_delta", asDelta(0, `{"type":"text_delta","text":"hey"}`)),
		asFr("content_block_stop", asBlockStop(0)),
		asFr("content_block_start", asBlockStart(1, `{"type":"tool_use","id":"toolu_1","name":"get_time","input":{}}`)),
		asFr("content_block_delta", asDelta(1, `{"type":"input_json_delta","partial_json":"{\"ti"}`)),
		asFr("content_block_delta", asDelta(1, `{"type":"input_json_delta","partial_json":"mezone\":\"UTC\"}"}`)),
		asFr("content_block_stop", asBlockStop(1)),
		asFr("message_delta", asMsgDelta(`{"stop_reason":"tool_use"}`, `{"output_tokens":2}`)),
		asFr("message_stop", `{"type":"message_stop"}`),
	}
}

func TestAnthropicStreamOfficialFullTrace(t *testing.T) {
	evs, _ := asFullSuccess(t, asOfficialFrames())
	if len(evs) != 10 {
		t.Fatalf("events = %d, want 10: %#v", len(evs), evs)
	}
	// Legacy-minimal start carries NO model/type/role: the absence is
	// preserved truthfully, never faked, and InitialUsage keeps the
	// reported input while output stays absent (not zero-filled).
	start := asStart(t, evs[0])
	if string(start.ID) != "msg_1" || start.Model != "" {
		t.Fatalf("minimal start not preserved: %+v", start)
	}
	iu := start.InitialUsage
	if iu == nil {
		t.Fatal("InitialUsage missing")
	}
	if iu.Provenance.Source != DialectAnthropic || iu.Provenance.Scope != UsageInitial {
		t.Fatalf("initial provenance wrong: %+v", iu.Provenance)
	}
	if iu.Prompt == nil || *iu.Prompt != 5 || iu.Completion != nil || iu.Total != nil {
		t.Fatalf("input-only start output must stay absent: %+v", iu)
	}
	if iu.CacheCreation != nil || iu.CacheRead != nil {
		t.Fatalf("absent shares fabricated: %+v", iu)
	}
	bo, ok := evs[1].(BlockOpen)
	if !ok || bo.Kind != BlockText || string(bo.Item) != "b0" {
		t.Fatalf("text open wrong: %#v", evs[1])
	}
	if bo.SourceIndex == nil || *bo.SourceIndex != 0 {
		t.Fatalf("text SourceIndex wrong (want content index 0): %#v", evs[1])
	}
	tf, ok := evs[2].(TextFragment)
	if !ok || string(tf.Item) != "b0" || tf.Text != "hey" {
		t.Fatalf("text fragment wrong: %#v", evs[2])
	}
	if _, ok := evs[3].(BlockClose); !ok {
		t.Fatalf("text close wrong: %#v", evs[3])
	}
	to, ok := evs[4].(BlockOpen)
	if !ok || to.Kind != BlockTool || string(to.Item) != "b1" || string(to.Call) != "toolu_1" || to.Name != "get_time" {
		t.Fatalf("tool open wrong: %#v", evs[4])
	}
	if to.SourceIndex == nil || *to.SourceIndex != 1 {
		t.Fatalf("tool SourceIndex wrong: %#v", evs[4])
	}
	// Seeded "{}" is the placeholder: only the two partials emit.
	a1, ok := evs[5].(ArgsFragment)
	if !ok || a1.Fragment != `{"ti` {
		t.Fatalf("first args fragment wrong: %#v", evs[5])
	}
	a2, ok := evs[6].(ArgsFragment)
	if !ok || a2.Fragment != `mezone":"UTC"}` {
		t.Fatalf("second args fragment wrong: %#v", evs[6])
	}
	if _, ok := evs[7].(BlockClose); !ok {
		t.Fatalf("tool close wrong: %#v", evs[7])
	}
	ue, ok := evs[8].(UsageEvent)
	if !ok || !ue.Terminal() {
		t.Fatalf("terminal usage wrong: %#v", evs[8])
	}
	u := ue.Usage
	if u.Prompt == nil || *u.Prompt != 5 || u.Completion == nil || *u.Completion != 2 || u.Total == nil || *u.Total != 7 {
		t.Fatalf("terminal usage wrong: %+v", u)
	}
	ce, ok := evs[9].(CompletionEvent)
	if !ok || ce.Finish.Kind != FinishToolCalls || ce.Finish.Raw != "tool_use" {
		t.Fatalf("completion wrong: %#v", evs[9])
	}
}

func TestAnthropicStreamOfficialLifecycle(t *testing.T) {
	evs, _ := asFullSuccess(t, asOfficialFrames())
	lc := NewResponseLifecycle(DefaultLimits(), DialectAnthropic)
	for _, ev := range evs {
		if rerr := lc.Observe(ev); rerr != nil {
			t.Fatalf("Observe(%T): %v", ev, rerr)
		}
	}
	res, rerr := lc.Result()
	if rerr != nil {
		t.Fatalf("Result: %v", rerr)
	}
	// The empty-model start is lifecycle-valid for the Anthropic source.
	if string(res.ID) != "msg_1" || res.Model != "" || res.Source != DialectAnthropic {
		t.Fatalf("result provenance wrong: %+v", res)
	}
	if len(res.Blocks) != 2 || res.Blocks[0].Kind != BlockText || res.Blocks[1].Kind != BlockTool {
		t.Fatalf("blocks wrong: %+v", res.Blocks)
	}
	if res.Blocks[0].SourceIndex == nil || *res.Blocks[0].SourceIndex != 0 {
		t.Fatalf("text index lost: %+v", res.Blocks[0])
	}
	if res.Blocks[1].SourceIndex == nil || *res.Blocks[1].SourceIndex != 1 {
		t.Fatalf("tool index lost: %+v", res.Blocks[1])
	}
	if res.Usage.Provenance.Scope != UsageTerminal || res.Finish.Kind != FinishToolCalls {
		t.Fatalf("terminal wrong: %+v %+v", res.Usage, res.Finish)
	}
	if res.MalformedTools != 0 {
		t.Fatalf("malformed count wrong: %d", res.MalformedTools)
	}
}

// --- Standard full start: descriptive members validated, zeros kept --------

func TestAnthropicStreamStandardStart(t *testing.T) {
	usage := `{"input_tokens":10,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}`
	frames := []Frame{asFr("message_start", asStartData(asStdMsg(`"claude-x"`, usage)))}
	dec := NewAnthropicStreamDecoder(DefaultLimits())
	evs, rerr := dec.DecodeFrame(frames[0])
	if rerr != nil {
		t.Fatalf("DecodeFrame: %v", rerr)
	}
	start := asStart(t, evs[0])
	if start.Model != "claude-x" {
		t.Fatalf("model lost: %+v", start)
	}
	iu := start.InitialUsage
	if iu.CacheCreation == nil || *iu.CacheCreation != 0 || iu.CacheRead == nil || *iu.CacheRead != 0 {
		t.Fatalf("explicit zero shares lost: %+v", iu)
	}
	if iu.Completion == nil || *iu.Completion != 0 || iu.Total == nil || *iu.Total != 10 {
		t.Fatalf("explicit zero output lost: %+v", iu)
	}
	// A supplied-empty model string is rejected at source: absence is
	// representable only when the member is omitted or null.
	for _, model := range []string{`""`, `"  "`} {
		asFail(t, []Frame{asFr("message_start", asStartData(asStdMsg(model, `{"input_tokens":5}`)))})
	}
}

// --- Compatibility seam: absence allowed here, never at COMPLETE ----------

func TestAnthropicStreamSeamVsCompleteBoundary(t *testing.T) {
	usage := `{"input_tokens":5,"output_tokens":1}`
	content := `[{"type":"text","text":"hi"}]`
	// The COMPLETE Anthropic decoder still rejects a missing, null, or
	// empty model at its wire boundary: the stream absence allowance
	// must not widen it.
	for _, raw := range []string{
		`{"id":"m","type":"message","role":"assistant","content":` + content + `,"stop_reason":"end_turn","usage":` + usage + `}`,
		`{"id":"m","type":"message","role":"assistant","model":null,"content":` + content + `,"stop_reason":"end_turn","usage":` + usage + `}`,
		`{"id":"m","type":"message","role":"assistant","model":"","content":` + content + `,"stop_reason":"end_turn","usage":` + usage + `}`,
	} {
		if _, rerr := DecodeAnthropicResponse([]byte(raw), DefaultLimits()); rerr == nil {
			t.Fatalf("COMPLETE boundary widened for %.80s", raw)
		}
	}
	// NewResponse carries the seam: Anthropic (stream) and Gemini allow
	// absence with router origin; Chat and Responses still require one.
	mkUsage := func() ResponseUsage {
		u, rerr := NewTerminalUsage(DialectAnthropic, 5, 1, 6)
		if rerr != nil {
			t.Fatalf("usage: %v", rerr)
		}
		return u
	}
	for _, source := range []Dialect{DialectAnthropic, DialectGemini} {
		resp, rerr := NewResponse("r1", "", source, nil, mkUsage(), Finish{Kind: FinishStop, Raw: "x"})
		if rerr != nil {
			t.Fatalf("NewResponse(%s empty model): %v", source, rerr)
		}
		if resp.ModelOrigin != IdentityRouterMinted {
			t.Fatalf("NewResponse(%s) origin = %q, want router", source, resp.ModelOrigin)
		}
	}
	for _, source := range []Dialect{DialectChat, DialectResponses} {
		if _, rerr := NewResponse("r1", "", source, nil, mkUsage(), Finish{Kind: FinishStop, Raw: "x"}); rerr == nil {
			t.Fatalf("NewResponse(%s empty model) must fail", source)
		}
	}
}

func TestAnthropicStreamLifecycleModelSeam(t *testing.T) {
	for _, source := range []Dialect{DialectAnthropic, DialectGemini, DialectResponses} {
		lc := NewResponseLifecycle(DefaultLimits(), source)
		if rerr := lc.Observe(ResponseStart{ID: "r1", Model: "", Source: source}); rerr != nil {
			t.Fatalf("Observe empty model (%s): %v", source, rerr)
		}
	}
	// Chat still requires a model at start; Responses streams start
	// empty and promote via ResponseModelEvent (terminal still
	// requires the actual model through DecodeResponsesResponse).
	for _, source := range []Dialect{DialectChat} {
		lc := NewResponseLifecycle(DefaultLimits(), source)
		if rerr := lc.Observe(ResponseStart{ID: "r1", Model: "", Source: source}); rerr == nil {
			t.Fatalf("Observe empty model (%s) must fail", source)
		}
	}
}

// --- Seeded content, placeholder policy, empty fallback --------------------

func TestAnthropicStreamSeededContent(t *testing.T) {
	big := `{"n":9007199254740993}`
	frames := []Frame{
		asFr("message_start", asStartData(asMinMsg(`"m1"`, `{"input_tokens":9}`))),
		asFr("content_block_start", asBlockStart(2, `{"type":"text","text":"seeded"}`)),
		asFr("content_block_delta", asDelta(2, `{"type":"text_delta","text":" live"}`)),
		asFr("content_block_stop", asBlockStop(2)),
		asFr("content_block_start", asBlockStart(5, `{"type":"tool_use","id":"t1","name":"f","input":`+big+`}`)),
		asFr("content_block_stop", asBlockStop(5)),
		asFr("message_delta", asMsgDelta(`{"stop_reason":"end_turn"}`, `{"output_tokens":1}`)),
		asFr("message_stop", `{"type":"message_stop"}`),
	}
	evs, _ := asFullSuccess(t, frames)
	// Seeded text emits immediately alongside its open.
	if _, ok := evs[1].(BlockOpen); !ok {
		t.Fatalf("text open wrong: %#v", evs[1])
	}
	if tf, ok := evs[2].(TextFragment); !ok || tf.Text != "seeded" {
		t.Fatalf("seeded text must emit immediately: %#v", evs[2])
	}
	// Seeded tool input emits immediately with exact bigint digits.
	bo2, ok := evs[5].(BlockOpen)
	if !ok || bo2.Kind != BlockTool {
		t.Fatalf("tool open wrong: %#v", evs[5])
	}
	af, ok := evs[6].(ArgsFragment)
	if !ok || af.Fragment != big {
		t.Fatalf("seeded args must emit exactly: %#v", evs[6])
	}
	if _, ok := evs[7].(BlockClose); !ok {
		t.Fatalf("tool close wrong: %#v", evs[7])
	}
}

func TestAnthropicStreamPlaceholderNeverDuplicates(t *testing.T) {
	frames := []Frame{
		asFr("message_start", asStartData(asMinMsg(`"m1"`, `{"input_tokens":9}`))),
		asFr("content_block_start", asBlockStart(0, `{"type":"tool_use","id":"t1","name":"f","input":{}}`)),
		asFr("content_block_delta", asDelta(0, `{"type":"input_json_delta","partial_json":"{\"a\":1}"}`)),
		asFr("content_block_stop", asBlockStop(0)),
		asFr("message_delta", asMsgDelta(`{"stop_reason":"end_turn"}`, `{"output_tokens":1}`)),
		asFr("message_stop", `{"type":"message_stop"}`),
	}
	evs, _ := asFullSuccess(t, frames)
	var frags []string
	for _, ev := range evs {
		if af, ok := ev.(ArgsFragment); ok {
			frags = append(frags, af.Fragment)
		}
		if _, ok := ev.(MalformedToolEvent); ok {
			t.Fatalf("valid args classified malformed: %#v", ev)
		}
	}
	// The "{}" placeholder must not duplicate the subsequent partial:
	// exactly the delta bytes flow, forming one valid object.
	if len(frags) != 1 || frags[0] != `{"a":1}` {
		t.Fatalf("placeholder duplicated partial: %q", frags)
	}
}

func TestAnthropicStreamEmptyArgsFallbackOnce(t *testing.T) {
	frames := []Frame{
		asFr("message_start", asStartData(asMinMsg(`"m1"`, `{"input_tokens":9}`))),
		asFr("content_block_start", asBlockStart(0, `{"type":"tool_use","id":"t1","name":"f","input":{}}`)),
		asFr("content_block_stop", asBlockStop(0)),
		asFr("message_delta", asMsgDelta(`{"stop_reason":"end_turn"}`, `{"output_tokens":1}`)),
		asFr("message_stop", `{"type":"message_stop"}`),
	}
	evs, _ := asFullSuccess(t, frames)
	count := 0
	for _, ev := range evs {
		if af, ok := ev.(ArgsFragment); ok {
			count++
			if af.Fragment != "{}" {
				t.Fatalf("fallback fragment = %q, want {}", af.Fragment)
			}
		}
	}
	if count != 1 {
		t.Fatalf("canonical {} emitted %d times, want exactly once", count)
	}
	// The 2 fallback bytes count: a one-byte per-call budget fails here.
	// (Single-char IDs keep the tiny budget focused on the fallback.)
	lim := DefaultLimits()
	lim.MaxArgsChars = 1
	tiny := []Frame{
		asFr("message_start", asStartData(asMinMsg(`"m"`, `{"input_tokens":9}`))),
		asFr("content_block_start", asBlockStart(0, `{"type":"tool_use","id":"t","name":"f","input":{}}`)),
		asFr("content_block_stop", asBlockStop(0)),
	}
	dec := NewAnthropicStreamDecoder(lim)
	for i, f := range tiny {
		if _, rerr := dec.DecodeFrame(f); rerr != nil {
			if i != 2 {
				t.Fatalf("fallback bound fired at frame %d: %v", i, rerr)
			}
			return
		}
	}
	t.Fatal("2-byte fallback must exceed a 1-byte per-call budget")
}

// --- Opaque IDs, parallel order, typed indices ------------------------------

func TestAnthropicStreamIdentitiesAndOrder(t *testing.T) {
	odd, long := "has:colon and space", strings.Repeat("z", 100)
	frames := []Frame{
		asFr("message_start", asStartData(asMinMsg(`"m1"`, `{"input_tokens":9}`))),
		asFr("content_block_start", asBlockStart(3, `{"type":"tool_use","id":"`+odd+`","name":"has space","input":{}}`)),
		asFr("content_block_start", asBlockStart(1, `{"type":"tool_use","id":"`+long+`","name":"`+strings.Repeat("n", 100)+`","input":{}}`)),
		asFr("content_block_stop", asBlockStop(3)),
		asFr("content_block_stop", asBlockStop(1)),
		asFr("message_delta", asMsgDelta(`{"stop_reason":"tool_use"}`, `{"output_tokens":1}`)),
		asFr("message_stop", `{"type":"message_stop"}`),
	}
	evs, _ := asFullSuccess(t, frames)
	// Arrival order is preserved (sparse index 3 before 1), never sorted;
	// logical positions travel in SourceIndex, never parsed from ItemIDs.
	o1, ok := evs[1].(BlockOpen)
	if !ok || string(o1.Call) != odd || o1.SourceIndex == nil || *o1.SourceIndex != 3 {
		t.Fatalf("first open wrong: %#v", evs[1])
	}
	o2, ok := evs[2].(BlockOpen)
	if !ok || string(o2.Call) != long || o2.SourceIndex == nil || *o2.SourceIndex != 1 {
		t.Fatalf("second open wrong: %#v", evs[2])
	}
	if string(o1.Item) == string(o2.Item) {
		t.Fatalf("item IDs collide: %q", o1.Item)
	}
}
