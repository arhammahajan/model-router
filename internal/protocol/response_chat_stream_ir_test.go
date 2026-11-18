// Chat source stream IR tests: direct frame -> semantic traces.
//
// These tests drive NewChatStreamDecoder directly, independent of any target:
// every assertion pins source-wire validation (envelopes, allowlists, finish
// and usage gates, identity rules) without calling the three legacy pair
// translators. The crosscheck test replays the legacy fixtures' wire shapes
// through the new decoder to prove source-fact compatibility.
package protocol

import (
	"strings"
	"testing"
)

// --- Independent wire builders (not shared with pair tests) -------------------

func csChunk(delta, finish string) string {
	ch := `{"id":"chatcmpl-7","object":"chat.completion.chunk","created":1700000000,"model":"gpt-4o","choices":[{"index":0,"delta":` + delta
	if finish == "" {
		ch += `,"finish_reason":null`
	} else {
		ch += `,"finish_reason":"` + finish + `"`
	}
	return ch + `}]}`
}

func csUsageChunk(usage string) string {
	return `{"id":"chatcmpl-7","object":"chat.completion.chunk","created":1700000000,"model":"gpt-4o","choices":[],"usage":` + usage + `}`
}

func csUsage(prompt, completion int) string {
	return `{"prompt_tokens":` + itoa(prompt) + `,"completion_tokens":` + itoa(completion) + `,"total_tokens":` + itoa(prompt+completion) + `}`
}

func csTerm(finish string, prompt, completion int) []string {
	return []string{csChunk(`{}`, finish), csUsageChunk(csUsage(prompt, completion)), "[DONE]"}
}

// csFullSuccess decodes a complete stream and validates Close-before-success:
// Close must read nothing and return no events.
func csFullSuccess(t *testing.T, datas []string) []ResponseEvent {
	t.Helper()
	dec := NewChatStreamDecoder(DefaultLimits())
	var all []ResponseEvent
	for _, d := range datas {
		evs, rerr := dec.DecodeFrame(Frame{Data: d})
		if rerr != nil {
			t.Fatalf("DecodeFrame(%q): %v", d, rerr)
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
	return all
}

// csMustFailAt asserts DecodeFrame itself fails at the exact frame index
// (not merely at Close via truncation): it proves the targeted guard fired
// rather than a downstream truncation masking a removed check.
func csMustFailAt(t *testing.T, datas []string, at int) *RouterError {
	t.Helper()
	dec := NewChatStreamDecoder(DefaultLimits())
	for i, d := range datas {
		_, rerr := dec.DecodeFrame(Frame{Data: d})
		if rerr != nil {
			if i != at {
				t.Fatalf("failure at frame %d, want guard at frame %d (%v)", i, at, rerr)
			}
			if rerr.Status != 502 {
				t.Fatalf("frame %d: status=%d, want 502 (%v)", i, rerr.Status, rerr)
			}
			return rerr
		}
	}
	t.Fatalf("expected 502 at frame %d, all frames accepted: %q", at, datas)
	return nil
}

// csMustFail asserts a 502 somewhere in the frame sequence, or at Close for
// truncated streams that decode without error but never terminate.
func csMustFail(t *testing.T, datas []string) *RouterError {
	t.Helper()
	dec := NewChatStreamDecoder(DefaultLimits())
	for i, d := range datas {
		if _, rerr := dec.DecodeFrame(Frame{Data: d}); rerr != nil {
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
	t.Fatalf("expected 502 failure, stream accepted: %q", datas)
	return nil
}

func csStart(t *testing.T, ev ResponseEvent) ResponseStart {
	t.Helper()
	start, ok := ev.(ResponseStart)
	if !ok {
		t.Fatalf("first event = %T, want ResponseStart", ev)
	}
	if start.Source != DialectChat || string(start.ID) != "chatcmpl-7" || start.Model != "gpt-4o" {
		t.Fatalf("start provenance wrong: %+v", start)
	}
	return start
}

// --- Text trace: start first, immediate deltas, terminal order ----------------

func TestChatStreamIRTextTrace(t *testing.T) {
	datas := []string{
		csChunk(`{"role":"assistant","content":"hel"}`, ""),
		csChunk(`{"content":"lo"}`, ""),
		csChunk(`{}`, "stop"),
		csUsageChunk(csUsage(25, 15)),
		"[DONE]",
	}
	evs := csFullSuccess(t, datas)
	if len(evs) != 7 {
		t.Fatalf("events = %d, want 7: %#v", len(evs), evs)
	}
	csStart(t, evs[0])
	open, ok := evs[1].(BlockOpen)
	if !ok || open.Item != "t0" || open.Kind != BlockText {
		t.Fatalf("ev[1] = %#v, want text BlockOpen t0", evs[1])
	}
	if frag, ok := evs[2].(TextFragment); !ok || frag.Item != "t0" || frag.Text != "hel" {
		t.Fatalf("ev[2] = %#v, want TextFragment hel", evs[2])
	}
	if frag, ok := evs[3].(TextFragment); !ok || frag.Text != "lo" {
		t.Fatalf("ev[3] = %#v, want TextFragment lo", evs[3])
	}
	if close, ok := evs[4].(BlockClose); !ok || close.Item != "t0" {
		t.Fatalf("ev[4] = %#v, want BlockClose t0", evs[4])
	}
	usage, ok := evs[5].(UsageEvent)
	if !ok || !usage.Terminal() {
		t.Fatalf("ev[5] = %#v, want terminal UsageEvent", evs[5])
	}
	if *usage.Usage.Prompt != 25 || *usage.Usage.Completion != 15 || *usage.Usage.Total != 40 {
		t.Fatalf("usage totals wrong: %+v", usage.Usage)
	}
	if usage.Usage.Provenance.Source != DialectChat {
		t.Fatalf("usage provenance wrong: %+v", usage.Usage.Provenance)
	}
	complete, ok := evs[6].(CompletionEvent)
	if !ok || complete.Finish.Kind != FinishStop || complete.Finish.Raw != "stop" {
		t.Fatalf("ev[6] = %#v, want stop CompletionEvent", evs[6])
	}
}

func TestChatStreamIRStartFirst(t *testing.T) {
	// No events exist before the first chunk: content cannot precede start
	// because the decoder emits ResponseStart before any content event.
	dec := NewChatStreamDecoder(DefaultLimits())
	if _, rerr := dec.Close(); rerr == nil {
		t.Fatal("Close without any frame accepted (truncation must fail)")
	}
	// [DONE] with no prior terminal state fails.
	dec2 := NewChatStreamDecoder(DefaultLimits())
	if _, rerr := dec2.DecodeFrame(Frame{Data: "[DONE]"}); rerr == nil {
		t.Fatal("[DONE] without finish/usage accepted")
	}
	// Empty data-only frame is malformed (legacy JSON-parse failure).
	dec3 := NewChatStreamDecoder(DefaultLimits())
	if _, rerr := dec3.DecodeFrame(Frame{Data: ""}); rerr == nil {
		t.Fatal("empty data-only frame accepted")
	}
	// Event-only frames (ping) emit nothing and fail nothing.
	dec4 := NewChatStreamDecoder(DefaultLimits())
	evs, rerr := dec4.DecodeFrame(Frame{Event: "ping", HasEvent: true, Data: ""})
	if rerr != nil || len(evs) != 0 {
		t.Fatalf("ping frame = %v %v, want no events no error", evs, rerr)
	}
	// Proxy-added event names do not change Chat source meaning.
	evs, rerr = dec4.DecodeFrame(Frame{Event: "message", HasEvent: true, Data: csChunk(`{"content":"hi"}`, "")})
	if rerr != nil {
		t.Fatalf("event-carrying Chat frame: %v", rerr)
	}
	if _, ok := evs[0].(ResponseStart); !ok {
		t.Fatalf("first content event = %T, want ResponseStart", evs[0])
	}
}

// --- Unknown members fail closed with value-free source paths ----------------

func TestChatStreamIRUnknownFields(t *testing.T) {
	term := csTerm("stop", 1, 1)
	badFirst := []string{
		`{"id":"chatcmpl-7","mystery":1}`,
		`{"id":"chatcmpl-7","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"bogus":1,"delta":{},"finish_reason":null}]}`,
		csChunk(`{"content":"x","bogus":1}`, ""),
		csChunk(`{"tool_calls":[{"index":0,"bogus":1}]}`, ""),
		csChunk(`{"tool_calls":[{"index":0,"function":{"name":"a","bogus":1}}]}`, ""),
		csChunk(`{}`, "stop") + "",
	}
	for _, first := range badFirst {
		if first == csChunk(`{}`, "stop") {
			continue
		}
		rerr := csMustFail(t, append([]string{first}, term...))
		if !strings.Contains(rerr.Msg, "has no representation") {
			t.Fatalf("unknown field must name the feature path: %v", rerr)
		}
	}
	// Unknown usage member fails at the terminal usage chunk.
	rerr := csMustFail(t, []string{
		csChunk(`{}`, "stop"),
		csUsageChunk(`{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2,"mystery":1}`),
		"[DONE]",
	})
	if !strings.Contains(rerr.Msg, "has no representation") {
		t.Fatalf("unknown usage member must name the feature path: %v", rerr)
	}
}

// --- Finish mapping: four valid stops, everything else refused ---------------

func TestChatStreamIRFinishMapping(t *testing.T) {
	for _, c := range []struct {
		raw  string
		kind FinishKind
	}{
		{"stop", FinishStop}, {"length", FinishLength},
		{"content_filter", FinishContentFilter}, {"tool_calls", FinishToolCalls},
	} {
		evs := csFullSuccess(t, csTerm(c.raw, 2, 1))
		complete, ok := evs[len(evs)-1].(CompletionEvent)
		if !ok || complete.Finish.Kind != c.kind || complete.Finish.Raw != c.raw {
			t.Fatalf("finish %q = %#v", c.raw, evs[len(evs)-1])
		}
	}
	for _, raw := range []string{"function_call", "quantum"} {
		rerr := csMustFail(t, []string{csChunk(`{}`, raw), csUsageChunk(csUsage(1, 1)), "[DONE]"})
		if !strings.Contains(rerr.Msg, "has no representation") {
			t.Fatalf("finish %q must be unrepresentable: %v", raw, rerr)
		}
	}
	// Deprecated function_call finish has no representation.
	if rerr := csMustFail(t, []string{csChunk(`{}`, "function_call"), csUsageChunk(csUsage(1, 1)), "[DONE]"}); !strings.Contains(rerr.Msg, "has no representation") {
		t.Fatalf("function_call finish must be unrepresentable: %v", rerr)
	}
}

// --- Terminal gate: finish + usage + [DONE], exactly once, in order ----------

func TestChatStreamIRTerminalGate(t *testing.T) {
	usage := csUsage(4, 3)
	// [DONE] without finish.
	csMustFailAt(t, []string{csChunk(`{"content":"hi"}`, ""), "[DONE]"}, 1)
	// [DONE] without usage.
	csMustFailAt(t, []string{csChunk(`{"content":"hi"}`, ""), csChunk(`{}`, "stop"), "[DONE]"}, 2)
	// Usage before finish.
	csMustFailAt(t, []string{csChunk(`{"content":"hi"}`, ""), csUsageChunk(usage), csChunk(`{}`, "stop"), csUsageChunk(usage), "[DONE]"}, 1)
	// Duplicate usage.
	csMustFailAt(t, []string{csChunk(`{"content":"hi"}`, ""), csChunk(`{}`, "stop"), csUsageChunk(usage), csUsageChunk(usage), "[DONE]"}, 3)
	// Duplicate finish.
	csMustFailAt(t, []string{csChunk(`{}`, "stop"), csChunk(`{}`, "stop"), csUsageChunk(usage), "[DONE]"}, 1)
	// Content after finish (non-empty) contradicts the terminal.
	csMustFailAt(t, []string{csChunk(`{}`, "stop"), csChunk(`{"content":"late"}`, ""), csUsageChunk(usage), "[DONE]"}, 1)
	// Tool delta after finish contradicts the terminal.
	csMustFailAt(t, []string{
		csChunk(`{"tool_calls":[{"index":0,"id":"c1","function":{"name":"a","arguments":""}}]}`, ""),
		csChunk(`{}`, "tool_calls"),
		csChunk(`{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}`, ""),
		csUsageChunk(usage), "[DONE]",
	}, 2)
	// Empty deltas after finish are tolerated (legacy skip policy).
	evs := csFullSuccess(t, []string{
		csChunk(`{"content":"hi"}`, ""),
		csChunk(`{}`, "stop"),
		csChunk(`{"content":""}`, ""),
		csUsageChunk(usage),
		"[DONE]",
	})
	if _, ok := evs[len(evs)-1].(CompletionEvent); !ok {
		t.Fatalf("empty post-finish delta must still terminate: %#v", evs[len(evs)-1])
	}
	// Events after [DONE] fail.
	dec := NewChatStreamDecoder(DefaultLimits())
	for _, d := range append([]string{csChunk(`{"content":"hi"}`, "")}, csTerm("stop", 4, 3)...) {
		if _, rerr := dec.DecodeFrame(Frame{Data: d}); rerr != nil {
			t.Fatalf("setup frame: %v", rerr)
		}
	}
	if _, rerr := dec.DecodeFrame(Frame{Data: csChunk(`{"content":"late"}`, "")}); rerr == nil {
		t.Fatal("frame after [DONE] accepted")
	}
}

func TestChatStreamRepeatedFinishUsage(t *testing.T) {
	usage := csUsage(9, 2)
	finish := strings.TrimSuffix(csChunk(`{}`, "stop"), "}") + `,"usage":` + usage + "}"
	echo := csUsageChunk(usage)
	events := csFullSuccess(t, []string{csChunk(`{"content":"ok"}`, ""), finish, echo, "[DONE]"})
	count := 0
	for _, event := range events {
		if u, ok := event.(UsageEvent); ok && u.Terminal() {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("want one terminal usage event, got %d", count)
	}
	csMustFailAt(t, []string{finish, csUsageChunk(csUsage(9, 3)), "[DONE]"}, 1)
	csMustFailAt(t, []string{finish, echo, echo, "[DONE]"}, 2)
}

// --- Unmapped content fails: refusal / function_call / audio / logprobs ------

func TestChatStreamIRUnmappedContentFails(t *testing.T) {
	term := csTerm("stop", 1, 1)
	mustUnrep := func(name, first string) {
		t.Helper()
		rerr := csMustFail(t, append([]string{first}, term...))
		if !strings.Contains(rerr.Msg, "has no representation") {
			t.Fatalf("%s must be unrepresentable: %v", name, rerr)
		}
	}
	mustUnrep("refusal", csChunk(`{"refusal":"no"}`, ""))
	mustUnrep("function_call", csChunk(`{"function_call":{"name":"f","arguments":"{}"}}`, ""))
	mustUnrep("audio", csChunk(`{"audio":{"id":"a"}}`, ""))
	// logprobs is a choice-level member.
	mustUnrep("logprobs", `{"id":"chatcmpl-7","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null,"logprobs":{}}]}`)
	// Non-empty refusal with a finish marker fails the same way.
	mustUnrep("refusal-with-finish", csChunk(`{"refusal":"no"}`, "stop"))
}

// --- Tool args: truncation and non-object payloads classify, not 502 ----

func TestChatStreamIRTruncatedArgs(t *testing.T) {
	mk := func(argsFrag string) []string {
		return []string{
			csChunk(`{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"a","arguments":"`+argsFrag+`"}}]}`, ""),
			csChunk(`{}`, "tool_calls"),
			csUsageChunk(csUsage(3, 2)),
			"[DONE]",
		}
	}
	// Truncated object: never becomes valid JSON, so the source marks
	// the block malformed for the target gate (Gemini suppress-and-remap
	// vs Anthropic/Responses refuse) instead of failing the stream.
	// Complete but non-object: arrays and scalars are not tool arguments.
	for _, argsFrag := range []string{`{\"a\":`, `[1]`, `\"s\"`} {
		evs := csFullSuccess(t, mk(argsFrag))
		var malformed []MalformedToolEvent
		for _, ev := range evs {
			if e, ok := ev.(MalformedToolEvent); ok {
				malformed = append(malformed, e)
			}
		}
		if len(malformed) != 1 || malformed[0].Item != "c0" {
			t.Fatalf("args %q: MalformedToolEvent = %+v, want one for c0", argsFrag, malformed)
		}
	}
}

// --- Parallel sparse tools: exact bytes, bigint, empty seed -------------------

func TestChatStreamIRArgsParallelBigintEmpty(t *testing.T) {
	datas := []string{
		csChunk(`{"role":"assistant"}`, ""),
		csChunk(`{"tool_calls":[{"index":0,"id":"call_A","type":"function","function":{"name":"get_time","arguments":""}}]}`, ""),
		csChunk(`{"tool_calls":[{"index":1,"id":"call_B","type":"function","function":{"name":"calc","arguments":""}}]}`, ""),
		csChunk(`{"tool_calls":[{"index":0,"function":{"arguments":"{\"tz\":"}}]}`, ""),
		csChunk(`{"tool_calls":[{"index":1,"function":{"arguments":"{\"n\":9007199254740993}"}},{"index":0,"function":{"arguments":"\"UTC\"}"}}]}`, ""),
		csChunk(`{}`, "tool_calls"),
		csUsageChunk(csUsage(30, 20)),
		"[DONE]",
	}
	evs := csFullSuccess(t, datas)
	csStart(t, evs[0])
	// Arrival order: block for index 0 opens before index 1.
	var kinds []string
	for _, ev := range evs {
		switch e := ev.(type) {
		case BlockOpen:
			kinds = append(kinds, "open:"+string(e.Item))
		case ArgsFragment:
			kinds = append(kinds, "args:"+string(e.Item)+":"+e.Fragment)
		case BlockClose:
			kinds = append(kinds, "close:"+string(e.Item))
		}
	}
	want := []string{
		"open:c0", "open:c1",
		`args:c0:{"tz":`, `args:c1:{"n":9007199254740993}`, `args:c0:"UTC"}`,
		"close:c0", "close:c1",
	}
	if len(kinds) != len(want) {
		t.Fatalf("tool event trace = %q, want %q", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("tool event trace = %q, want %q", kinds, want)
		}
	}
	complete, ok := evs[len(evs)-1].(CompletionEvent)
	if !ok || complete.Finish.Kind != FinishToolCalls {
		t.Fatalf("terminal = %#v, want tool_calls completion", evs[len(evs)-1])
	}
	// Empty-argument tool: opens, carries the canonical '{}' fallback
	// fragment exactly once, still closes.
	evs2 := csFullSuccess(t, []string{
		csChunk(`{"tool_calls":[{"index":0,"id":"to_1","type":"function","function":{"name":"ping","arguments":""}}]}`, ""),
		csChunk(`{}`, "tool_calls"),
		csUsageChunk(csUsage(5, 2)),
		"[DONE]",
	})
	var opens, args, closes int
	var payload string
	for _, ev := range evs2 {
		switch e := ev.(type) {
		case BlockOpen:
			opens++
		case ArgsFragment:
			args++
			payload = e.Fragment
		case BlockClose:
			closes++
		}
	}
	if opens != 1 || closes != 1 || args != 1 || payload != "{}" {
		t.Fatalf("empty-arg tool trace: opens=%d args=%d (%q) closes=%d", opens, args, payload, closes)
	}
}

// --- Identities: opaque call IDs, conflicts, stability, incompleteness --------

func TestChatStreamIRIdentityRules(t *testing.T) {
	// Same index, differing id: conflict at the second frame.
	csMustFailAt(t, []string{
		csChunk(`{"tool_calls":[{"index":0,"id":"c1","function":{"name":"a","arguments":""}}]}`, ""),
		csChunk(`{"tool_calls":[{"index":0,"id":"c2","function":{"name":"a","arguments":""}}]}`, ""),
	}, 1)
	// Same index, differing name: fragmented names have no mapping.
	csMustFailAt(t, []string{
		csChunk(`{"tool_calls":[{"index":0,"id":"c1","function":{"name":"aa","arguments":""}}]}`, ""),
		csChunk(`{"tool_calls":[{"index":0,"function":{"name":"ab"}}}]}`, ""),
	}, 1)
	// Cross-index duplicate call id: conflict when the second identity
	// completes (emit-time guard, before any terminal).
	csMustFailAt(t, []string{
		csChunk(`{"tool_calls":[{"index":0,"id":"dup","function":{"name":"a","arguments":""}}]}`, ""),
		csChunk(`{"tool_calls":[{"index":1,"id":"dup","function":{"name":"b","arguments":""}}]}`, ""),
	}, 1)
	// Incomplete identity at finish: refused, never guessed.
	csMustFailAt(t, []string{
		csChunk(`{"tool_calls":[{"index":0,"function":{"name":"a","arguments":"{}"}}]}`, ""),
		csChunk(`{}`, "tool_calls"),
		csUsageChunk(csUsage(2, 1)),
		"[DONE]",
	}, 1)
	csMustFailAt(t, []string{
		csChunk(`{"tool_calls":[{"index":0,"id":"c1","function":{"arguments":"{}"}}]}`, ""),
		csChunk(`{}`, "tool_calls"),
		csUsageChunk(csUsage(2, 1)),
		"[DONE]",
	}, 1)
	// Response id stability: differing non-empty ids conflict.
	csMustFailAt(t, []string{
		csChunk(`{"content":"a"}`, ""),
		`{"id":"other","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{"content":"b"},"finish_reason":null}]}`,
	}, 1)
	// Model stability.
	csMustFailAt(t, []string{
		csChunk(`{"content":"a"}`, ""),
		`{"id":"chatcmpl-7","object":"chat.completion.chunk","created":1,"model":"other","choices":[{"index":0,"delta":{"content":"b"},"finish_reason":null}]}`,
	}, 1)
	// First chunk without identity carries no provenance.
	csMustFailAt(t, []string{
		`{"object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{"content":"a"},"finish_reason":null}]}`,
	}, 0)
	// Whitespace-only call id is not an identity.
	csMustFailAt(t, []string{
		csChunk(`{"tool_calls":[{"index":0,"id":"  ","function":{"name":"a","arguments":""}}]}`, ""),
	}, 0)
}

// --- Name/id bounds: opaque handles pass the source; grammar is target-side --

func TestChatStreamIRIdentityBounds(t *testing.T) {
	// Long opaque call IDs (spaces, colons, 100 chars) pass source decode:
	// handles are opaque, not tool-name grammar.
	long := strings.Repeat("z", 100)
	evs := csFullSuccess(t, []string{
		csChunk(`{"tool_calls":[{"index":0,"id":"has:colon and space","type":"function","function":{"name":"f","arguments":"{}"}}]}`, ""),
		csChunk(`{"tool_calls":[{"index":1,"id":"`+long+`","type":"function","function":{"name":"g","arguments":""}}]}`, ""),
		csChunk(`{}`, "tool_calls"),
		csUsageChunk(csUsage(5, 2)),
		"[DONE]",
	})
	var calls []string
	for _, ev := range evs {
		if open, ok := ev.(BlockOpen); ok && open.Kind == BlockTool {
			calls = append(calls, string(open.Call))
		}
	}
	if len(calls) != 2 || calls[0] != "has:colon and space" || calls[1] != long {
		t.Fatalf("opaque call IDs not preserved: %q", calls)
	}
	// Grammar-invalid tool names pass the SOURCE (the 1..64 grammar is a
	// target gate via CheckRepresentable, matching the complete row).
	evs = csFullSuccess(t, []string{
		csChunk(`{"tool_calls":[{"index":0,"id":"c1","function":{"name":"has space","arguments":"{}"}}]}`, ""),
		csChunk(`{}`, "tool_calls"),
		csUsageChunk(csUsage(5, 2)),
		"[DONE]",
	})
	found := false
	for _, ev := range evs {
		if open, ok := ev.(BlockOpen); ok && open.Kind == BlockTool && open.Name == "has space" {
			found = true
		}
	}
	if !found {
		t.Fatalf("grammar-invalid name must pass source decode: %#v", evs)
	}
	// Whitespace-only tool name is not an identity.
	csMustFail(t, []string{
		csChunk(`{"tool_calls":[{"index":0,"id":"c1","function":{"name":"  ","arguments":"{}"}}]}`, ""),
	})
}

// --- Unknown finish values are value-free: static field-only 502 ---------

func TestChatStreamIRUnknownFinishValueFree(t *testing.T) {
	secret := "private-upstream-finish-value"
	rerr := csMustFailAt(t, []string{csChunk(`{}`, secret), csUsageChunk(csUsage(1, 1)), "[DONE]"}, 0)
	if rerr.Status != 502 {
		t.Fatalf("status = %d, want 502", rerr.Status)
	}
	if strings.Contains(rerr.Msg, secret) {
		t.Fatalf("finish error echoes upstream value: %q", rerr.Msg)
	}
	if rerr.Msg != "Upstream stop reason has no representation" {
		t.Fatalf("finish error must be static field-only, got %q", rerr.Msg)
	}
}
