// Anthropic stop_details source tests: complete + SSE.
//
// These pin the optional Messages `stop_details` member as documented in the
// official schema (Messages API reference; anthropic-sdk-python
// types/message.py + types/refusal_stop_details.py; handling-stop-reasons;
// handle-streaming-refusals):
//   - absent/null accepted for every stop (never a refusal);
//   - non-null validated for shape but never carried into the IR;
//   - non-null meaningful only on a `refusal` stop, never downgraded.
package protocol

import (
	"strings"
	"testing"
)

func anthStopDetailsRaw(stop, content, usage, detailsFrag string) string {
	base := anthIRRaw(stop, content, usage)
	if detailsFrag == "" {
		return base
	}
	return strings.Replace(base, `"usage":`, `"stop_details":`+detailsFrag+`,"usage":`, 1)
}

func TestAnthropicStopDetailsAbsentNull(t *testing.T) {
	usage := anthIRUsage(4, 3)
	content := `[{"type":"text","text":"hi"}]`
	for _, stop := range []string{"end_turn", "stop_sequence", "max_tokens", "tool_use", "refusal"} {
		// Absent.
		resp := anthIRDecode(t, anthIRRaw(stop, content, usage))
		if resp.Finish.Raw != stop {
			t.Fatalf("absent stop_details broke stop %q: %+v", stop, resp.Finish)
		}
		// Explicit null.
		raw := anthStopDetailsRaw(stop, content, usage, `null`)
		resp = anthIRDecode(t, raw)
		if resp.Finish.Raw != stop {
			t.Fatalf("null stop_details broke stop %q: %+v", stop, resp.Finish)
		}
	}
}

func TestAnthropicStopDetailsRefusalValid(t *testing.T) {
	usage := anthIRUsage(4, 3)
	content := `[{"type":"text","text":"hi"}]`
	valid := []string{
		`{"type":"refusal","category":"cyber","explanation":"declined"}`,
		`{"type":"refusal","category":"bio","explanation":null}`,
		`{"type":"refusal","category":null,"explanation":null}`,
		`{"type":"refusal"}`,
		`{"type":"refusal","category":"frontier_llm"}`,
		`{"type":"refusal","category":"reasoning_extraction","explanation":"x"}`,
		`{"type":"refusal","category":"general_harms","explanation":"y"}`,
	}
	for _, details := range valid {
		resp := anthIRDecode(t, anthStopDetailsRaw("refusal", content, usage, details))
		if resp.Finish.Kind != FinishRefusal || resp.Finish.Raw != "refusal" {
			t.Fatalf("refusal details %q broke finish: %+v", details, resp.Finish)
		}
		if len(resp.Items) != 1 {
			t.Fatalf("refusal details %q broke items: %#v", details, resp.Items)
		}
	}
}

func TestAnthropicStopDetailsNonRefusalNonNullFails(t *testing.T) {
	usage := anthIRUsage(4, 3)
	content := `[{"type":"text","text":"hi"}]`
	details := `{"type":"refusal","category":"cyber","explanation":"declined"}`
	for _, stop := range []string{"end_turn", "stop_sequence", "max_tokens", "tool_use"} {
		rerr := anthIRFail(t, anthStopDetailsRaw(stop, content, usage, details))
		if !strings.Contains(rerr.Msg, "has no representation") {
			t.Fatalf("stop %q nonnull details wrong diagnostic: %v", stop, rerr)
		}
		if !strings.Contains(rerr.Msg, "response.stop_details") {
			t.Fatalf("stop %q diagnostic missing path: %v", stop, rerr)
		}
		if strings.Contains(rerr.Msg, "cyber") || strings.Contains(rerr.Msg, "declined") {
			t.Fatalf("upstream safety metadata leaked: %v", rerr)
		}
	}
}

func TestAnthropicStopDetailsMalformed(t *testing.T) {
	usage := anthIRUsage(4, 3)
	content := `[{"type":"text","text":"hi"}]`
	bad := []string{
		`{"type":"refusal","category":"unknown_cat","explanation":null}`,
		`{"type":"other","category":null,"explanation":null}`,
		`{"type":"refusal","category":"cyber","explanation":null,"future":1}`,
		`{"type":"refusal","category":7,"explanation":null}`,
		`{"type":"refusal","category":null,"explanation":7}`,
		`{"category":"cyber"}`,
		`{"type":null,"category":null}`,
		`[1]`,
		`"refusal"`,
		`7`,
	}
	for _, details := range bad {
		rerr := anthIRFail(t, anthStopDetailsRaw("refusal", content, usage, details))
		if rerr.Status != 502 {
			t.Fatalf("details %q status=%d, want 502", details, rerr.Status)
		}
	}
}

func TestAnthropicStopDetailsValueFree(t *testing.T) {
	secret := "super-secret-explanation-value"
	usage := anthIRUsage(4, 3)
	content := `[{"type":"text","text":"hi"}]`
	// Non-refusal with secret details must not echo the secret.
	rerr := anthIRFail(t, anthStopDetailsRaw("end_turn", content, usage,
		`{"type":"refusal","category":"cyber","explanation":"`+secret+`"}`))
	if strings.Contains(rerr.Msg, secret) {
		t.Fatalf("secret explanation leaked: %v", rerr)
	}
	// Unknown category value must not echo either.
	unknownCat := "secret-category-value"
	rerr = anthIRFail(t, anthStopDetailsRaw("refusal", content, usage,
		`{"type":"refusal","category":"`+unknownCat+`"}`))
	if strings.Contains(rerr.Msg, unknownCat) {
		t.Fatalf("unknown category leaked: %v", rerr)
	}
}

// --- SSE ---------------------------------------------------------------

func TestAnthropicStreamStopDetailsStart(t *testing.T) {
	// Absent/null stop_details at start passes (legacy minimal + explicit null).
	frames := []Frame{
		asFr("message_start", asStartData(asMinMsg(`"m1"`, `{"input_tokens":9}`))),
		asFr("content_block_start", asBlockStart(0, `{"type":"text","text":""}`)),
		asFr("content_block_stop", asBlockStop(0)),
		asFr("message_delta", asMsgDelta(`{"stop_reason":"end_turn","stop_details":null}`, `{"output_tokens":1}`)),
		asFr("message_stop", `{"type":"message_stop"}`),
	}
	if _, dec := asFullSuccess(t, frames); dec == nil {
		t.Fatal("null stop_details stream must succeed")
	}
	// Non-null details at start are premature (no terminal yet).
	asFail(t, []Frame{
		asFr("message_start", asStartData(`{"id":"m1","type":"message","role":"assistant","model":"c","content":[],"stop_reason":null,"stop_sequence":null,"stop_details":{"type":"refusal"},"usage":{"input_tokens":9}}`)),
	})
}

func TestAnthropicStreamContainerEcho(t *testing.T) {
	frames := []Frame{
		asFr("message_start", asStartData(asMinMsg(`"m1"`, `{"input_tokens":9}`))),
		asFr("message_delta", asMsgDelta(`{"stop_reason":"end_turn","container":null}`, `{"output_tokens":1}`)),
		asFr("message_stop", `{"type":"message_stop"}`),
	}
	asFullSuccess(t, frames)
	frames[1] = asFr("message_delta", asMsgDelta(`{"stop_reason":"end_turn","container":{"id":"container_1"}}`, `{"output_tokens":1}`))
	if rerr := asFail(t, frames); !strings.Contains(rerr.Msg, "message_delta.delta.container") {
		t.Fatalf("active container must remain unrepresentable: %v", rerr)
	}
}

func TestAnthropicStreamStopDetailsDeltaRefusal(t *testing.T) {
	frames := []Frame{
		asFr("message_start", asStartData(asMinMsg(`"m1"`, `{"input_tokens":9}`))),
		asFr("content_block_start", asBlockStart(0, `{"type":"text","text":""}`)),
		asFr("content_block_stop", asBlockStop(0)),
		asFr("message_delta", asMsgDelta(`{"stop_reason":"refusal","stop_details":{"type":"refusal","category":"cyber","explanation":"declined"}}`, `{"output_tokens":1}`)),
		asFr("message_stop", `{"type":"message_stop"}`),
	}
	evs, _ := asFullSuccess(t, frames)
	ce, ok := evs[len(evs)-1].(CompletionEvent)
	if !ok || ce.Finish.Kind != FinishRefusal || ce.Finish.Raw != "refusal" {
		t.Fatalf("refusal stream finish wrong: %#v", evs[len(evs)-1])
	}
}

func TestAnthropicStreamStopDetailsMismatch(t *testing.T) {
	mk := func(delta string) []Frame {
		return []Frame{
			asFr("message_start", asStartData(asMinMsg(`"m1"`, `{"input_tokens":9}`))),
			asFr("content_block_start", asBlockStart(0, `{"type":"text","text":""}`)),
			asFr("content_block_stop", asBlockStop(0)),
			asFr("message_delta", asMsgDelta(delta, `{"output_tokens":1}`)),
			asFr("message_stop", `{"type":"message_stop"}`),
		}
	}
	// Same-delta mismatch: refusal details with a normal stop.
	rerr := asFail(t, mk(`{"stop_reason":"end_turn","stop_details":{"type":"refusal","category":"cyber"}}`))
	if !strings.Contains(rerr.Msg, "stop_details") {
		t.Fatalf("mismatch diagnostic missing path: %v", rerr)
	}
	// Malformed details at delta fail closed.
	asFail(t, mk(`{"stop_reason":"refusal","stop_details":{"type":"refusal","category":"unknown"}}`))
	asFail(t, mk(`{"stop_reason":"refusal","stop_details":{"type":"other"}}`))
}

func TestAnthropicObservedZeroUsageBreakdowns(t *testing.T) {
	const cache = `{"ephemeral_1h_input_tokens":0,"ephemeral_5m_input_tokens":0}`
	const thinking = `{"thinking_tokens":0}`
	raw := anthStopDetailsRaw("end_turn", `[{"type":"text","text":"OK"}]`,
		`{"input_tokens":14,"output_tokens":4,"cache_creation_input_tokens":0,"cache_creation":`+cache+`,"output_tokens_details":`+thinking+`}`, "")
	resp, rerr := DecodeAnthropicResponse([]byte(raw), DefaultLimits())
	if rerr != nil || *resp.Usage.Prompt != 14 || *resp.Usage.Completion != 4 {
		t.Fatalf("zero breakdowns must keep authoritative counters: %v %v", resp.Usage, rerr)
	}
	for _, usage := range []string{
		`{"input_tokens":14,"output_tokens":4,"cache_creation_input_tokens":1,"cache_creation":` + cache + `}`,
		`{"input_tokens":14,"output_tokens":4,"output_tokens_details":{"thinking_tokens":1}}`,
		`{"input_tokens":14,"output_tokens":4,"output_tokens_details":{"thinking_tokens":null}}`,
		`{"input_tokens":14,"output_tokens":4,"output_tokens_details":{"future":0}}`,
	} {
		raw := anthStopDetailsRaw("end_turn", `[{"type":"text","text":"OK"}]`, usage, "")
		if _, rerr := DecodeAnthropicResponse([]byte(raw), DefaultLimits()); rerr == nil {
			t.Fatalf("unsupported breakdown accepted: %s", usage)
		}
	}
	streamUsage := `{"input_tokens":14,"output_tokens":4,"cache_creation_input_tokens":0,"cache_creation":` + cache + `,"output_tokens_details":` + thinking + `}`
	parsed, rerr := parseAnthropicStreamUsage([]byte(streamUsage), "message_delta.usage", true, true)
	if rerr != nil || parsed.input != 14 || parsed.output != 4 {
		t.Fatalf("stream zero breakdowns must keep authoritative counters: %v %v", parsed, rerr)
	}
}
