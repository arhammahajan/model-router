package protocol

import (
	"strings"
	"testing"
)

// Live Muse OpenCode Responses upstream echoes `top_logprobs:0` on ordinary
// generations (completed JSON and created SSE). Zero/absent/null are exact
// absent controls; nonzero has no representation; malformed is invalid.
// Billing usage `cost`/`opencode` is intentionally untouched here (root
// shared cross-source policy owns it).

func topLogprobsFullRaw(extra string) string {
	okOut := `[{"id":"m","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi","annotations":[]}]}]`
	return `{"id":"resp_1","object":"response","created_at":1746989954.0,"model":"up",` +
		`"status":"completed","error":null,"incomplete_details":null,` +
		`"output":` + okOut + `,"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}` + extra + `}`
}

func TestResponsesTopLogprobsFullAbsentControls(t *testing.T) {
	for _, extra := range []string{"", `,"top_logprobs":null`, `,"top_logprobs":0`} {
		raw := topLogprobsFullRaw(extra)
		if _, rerr := DecodeResponsesResponse([]byte(raw), DefaultLimits()); rerr != nil {
			t.Fatalf("full extra %q must pass, got %v", extra, rerr)
		}
		// All three target encoders accept the same source (no logprobs to carry).
		for _, fn := range []func([]byte, string, Limits) ([]byte, *RouterError){
			TranslateResponsesJSONToChat, TranslateResponsesJSONToAnthropic, TranslateResponsesJSONToGemini,
		} {
			if _, rerr := fn([]byte(raw), "public", DefaultLimits()); rerr != nil {
				t.Fatalf("full extra %q must pass all targets, got %v", extra, rerr)
			}
		}
	}
}

func TestResponsesTopLogprobsFullNonzeroUnrep(t *testing.T) {
	for _, extra := range []string{`,"top_logprobs":1`, `,"top_logprobs":2`, `,"top_logprobs":20`} {
		_, rerr := DecodeResponsesResponse([]byte(topLogprobsFullRaw(extra)), DefaultLimits())
		if rerr == nil || rerr.Status != 502 {
			t.Fatalf("full extra %q must fail 502, got %v", extra, rerr)
		}
		if !strings.Contains(rerr.Msg, "response.top_logprobs") || !strings.Contains(rerr.Msg, "has no representation") {
			t.Fatalf("nonzero must name response.top_logprobs as unrepresentable, got %q", rerr.Msg)
		}
	}
}

func TestResponsesTopLogprobsFullMalformedInvalid(t *testing.T) {
	for _, extra := range []string{
		`,"top_logprobs":"5"`, `,"top_logprobs":1.5`, `,"top_logprobs":-1`,
		`,"top_logprobs":true`, `,"top_logprobs":[]`, `,"top_logprobs":{}`,
	} {
		_, rerr := DecodeResponsesResponse([]byte(topLogprobsFullRaw(extra)), DefaultLimits())
		if rerr == nil || rerr.Status != 502 {
			t.Fatalf("full extra %q must fail 502, got %v", extra, rerr)
		}
		if rerr.Msg != "Upstream returned an invalid response" {
			t.Fatalf("malformed %q must be invalid, got %q", extra, rerr.Msg)
		}
	}
}

func topLogprobsSSEUpstream(createdExtra, terminalExtra string) string {
	createdResp := `{"id":"resp_1","status":"in_progress"` + createdExtra + `}`
	terminalResp := `{"id":"resp_1","model":"gpt-4.1","status":"completed",` +
		`"output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi","annotations":[]}]}],` +
		`"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}` + terminalExtra + `}`
	return "event: response.created\ndata: {\"type\":\"response.created\",\"response\":" + createdResp + "}\n\n" +
		r2cItemAddedMsg() + r2cPartAdded() + r2cTextDelta("hi") +
		"event: response.output_text.done\ndata: {\"type\":\"response.output_text.done\",\"output_index\":0,\"content_index\":0,\"item_id\":\"msg_1\",\"text\":\"hi\"}\n\n" +
		r2cPartDone("hi") + r2cItemDoneMsg("hi") +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + terminalResp + "}\n\n"
}

func TestResponsesTopLogprobsSSEEnvelopes(t *testing.T) {
	// Zero on created + terminal passes end-to-end.
	out, rerr := runR2CStream(t, topLogprobsSSEUpstream(`,"top_logprobs":0`, `,"top_logprobs":0`), true, DefaultLimits())
	if rerr != nil {
		t.Fatalf("SSE zero echoes must pass, got %v", rerr)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Fatalf("SSE zero echoes missing terminator: %s", out)
	}
	// Absent/null on created + terminal passes.
	out, rerr = runR2CStream(t, topLogprobsSSEUpstream("", ""), true, DefaultLimits())
	if rerr != nil {
		t.Fatalf("SSE absent echoes must pass, got %v", rerr)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Fatalf("SSE absent missing terminator: %s", out)
	}
	// Nonzero on created fails as unrepresentable before success.
	_, rerr = runR2CStream(t, topLogprobsSSEUpstream(`,"top_logprobs":1`, `,"top_logprobs":0`), true, DefaultLimits())
	if rerr == nil || rerr.Status != 502 || !strings.Contains(rerr.Msg, "response.top_logprobs") {
		t.Fatalf("SSE nonzero created must fail naming response.top_logprobs, got %v", rerr)
	}
	// Nonzero on terminal fails as unrepresentable.
	_, rerr = runR2CStream(t, topLogprobsSSEUpstream(`,"top_logprobs":0`, `,"top_logprobs":2`), true, DefaultLimits())
	if rerr == nil || rerr.Status != 502 || !strings.Contains(rerr.Msg, "response.top_logprobs") {
		t.Fatalf("SSE nonzero terminal must fail naming response.top_logprobs, got %v", rerr)
	}
	// Malformed on created fails invalid.
	_, rerr = runR2CStream(t, topLogprobsSSEUpstream(`,"top_logprobs":"5"`, `,"top_logprobs":0`), true, DefaultLimits())
	if rerr == nil || rerr.Status != 502 || rerr.Msg != "Upstream returned an invalid response" {
		t.Fatalf("SSE malformed created must be invalid, got %v", rerr)
	}
	// Status/lineage strictness preserved even with the zero echo present:
	// terminal status contradicting the event kind still fails.
	badTerminal := strings.Replace(
		topLogprobsSSEUpstream(`,"top_logprobs":0`, `,"top_logprobs":0`),
		`"type":"response.completed"`, `"type":"response.completed"`, 1)
	// Flip the embedded status to incomplete while the event stays completed.
	badTerminal = strings.Replace(badTerminal, `"status":"completed","output"`, `"status":"incomplete","output"`, 1)
	_, rerr = runR2CStream(t, badTerminal, true, DefaultLimits())
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("status contradiction with zero echo must still fail, got %v", rerr)
	}
}
