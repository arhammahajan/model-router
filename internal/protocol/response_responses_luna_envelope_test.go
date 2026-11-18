package protocol

import (
	"strings"
	"testing"
)

// Live GPT-5.6 Luna Responses upstream echoes ordinary complete controls:
// `text:{"format":{"type":"text"},"verbosity":"medium"}` and zero-only
// `tool_usage:{"image_gen":{...0s...},"web_search":{"num_requests":0}}`
// (completed JSON). Reviewed verbosity ("low"|"medium"|"high") and
// structurally valid zero-only known tool_usage are exact absent controls
// with nothing to carry: validated and discarded. Absent/null pass.
// Nonzero known counters and unknown non-null members have no
// representation; malformed values are invalid. All failures are 502
// CodeUpstreamBad and value-free. Billing usage `cost`/`opencode` is
// intentionally untouched here (root shared cross-source policy owns it).
// Message-output and stream item phase fields are intentionally untouched
// here (root owns them).

func lunaFullRaw(extra string) string {
	okOut := `[{"id":"m","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi","annotations":[]}]}]`
	return `{"id":"resp_1","object":"response","created_at":1746989954.0,"model":"up",` +
		`"status":"completed","error":null,"incomplete_details":null,` +
		`"output":` + okOut + `,"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}` + extra + `}`
}

const lunaZeroToolUsage = `,"tool_usage":{"image_gen":{"input_tokens":0,"output_tokens":0,"total_tokens":0,"input_tokens_details":{"image_tokens":0,"text_tokens":0},"output_tokens_details":{"image_tokens":0,"text_tokens":0}},"web_search":{"num_requests":0}}`

const lunaMediumText = `,"text":{"format":{"type":"text"},"verbosity":"medium"}`

func TestResponsesLunaEnvelopeFullLiveBlockerPasses(t *testing.T) {
	// The exact live blocker: both echoes together on an ordinary complete.
	raw := lunaFullRaw(lunaMediumText + lunaZeroToolUsage)
	if _, rerr := DecodeResponsesResponse([]byte(raw), DefaultLimits()); rerr != nil {
		t.Fatalf("live Luna ordinary complete must pass, got %v", rerr)
	}
	for _, fn := range []func([]byte, string, Limits) ([]byte, *RouterError){
		TranslateResponsesJSONToChat, TranslateResponsesJSONToAnthropic, TranslateResponsesJSONToGemini,
	} {
		if _, rerr := fn([]byte(raw), "public", DefaultLimits()); rerr != nil {
			t.Fatalf("live Luna ordinary complete must pass all targets, got %v", rerr)
		}
	}
}

func TestResponsesLunaTextVerbosityFullAbsentControls(t *testing.T) {
	for _, extra := range []string{
		"",
		`,"text":null`,
		`,"text":{"format":{"type":"text"}}`,
		`,"text":{"format":{"type":"text"},"verbosity":"low"}`,
		`,"text":{"format":{"type":"text"},"verbosity":"medium"}`,
		`,"text":{"format":{"type":"text"},"verbosity":"high"}`,
		`,"text":{"verbosity":"low"}`,
		`,"text":{"format":{"type":"text"},"verbosity":null}`,
		`,"text":{"format":{"type":"text"},"verbosity":"medium","future_null":null}`,
	} {
		raw := lunaFullRaw(extra)
		if _, rerr := DecodeResponsesResponse([]byte(raw), DefaultLimits()); rerr != nil {
			t.Fatalf("full extra %q must pass, got %v", extra, rerr)
		}
		for _, fn := range []func([]byte, string, Limits) ([]byte, *RouterError){
			TranslateResponsesJSONToChat, TranslateResponsesJSONToAnthropic, TranslateResponsesJSONToGemini,
		} {
			if _, rerr := fn([]byte(raw), "public", DefaultLimits()); rerr != nil {
				t.Fatalf("full extra %q must pass all targets, got %v", extra, rerr)
			}
		}
	}
}

func TestResponsesLunaTextVerbosityFullUnknownUnrep(t *testing.T) {
	for _, extra := range []string{
		`,"text":{"format":{"type":"text"},"verbosity":"turbo"}`,
		`,"text":{"format":{"type":"text"},"verbosity":"VERBOSE"}`,
		`,"text":{"format":{"type":"text"},"verbosity":""}`,
		`,"text":{"format":{"type":"text"},"verbosity":"medium","future":"x"}`,
		`,"text":{"format":{"type":"text"},"verbosity":"medium","extra":"s"}`,
	} {
		_, rerr := DecodeResponsesResponse([]byte(lunaFullRaw(extra)), DefaultLimits())
		if rerr == nil || rerr.Status != 502 {
			t.Fatalf("full extra %q must fail 502, got %v", extra, rerr)
		}
		if !strings.Contains(rerr.Msg, "response.text.verbosity") && !strings.Contains(rerr.Msg, "response.text.") {
			t.Fatalf("unknown %q must name response.text path as unrepresentable, got %q", extra, rerr.Msg)
		}
		if !strings.Contains(rerr.Msg, "has no representation") {
			t.Fatalf("unknown %q must be unrepresentable, got %q", extra, rerr.Msg)
		}
		if strings.Contains(rerr.Msg, "turbo") || strings.Contains(rerr.Msg, "VERBOSE") {
			t.Fatalf("unknown error must be value-free, got %q", rerr.Msg)
		}
	}
}

func TestResponsesLunaTextVerbosityFullMalformedInvalid(t *testing.T) {
	for _, extra := range []string{
		`,"text":"medium"`,
		`,"text":[]`,
		`,"text":{"format":{"type":"text"},"verbosity":5}`,
		`,"text":{"format":{"type":"text"},"verbosity":true}`,
		`,"text":{"format":{"type":"text"},"verbosity":[]}`,
		`,"text":{"format":{"type":"text"},"verbosity":{}}`,
		`,"text":{"format":"text"}`,
	} {
		_, rerr := DecodeResponsesResponse([]byte(lunaFullRaw(extra)), DefaultLimits())
		if rerr == nil || rerr.Status != 502 {
			t.Fatalf("full extra %q must fail 502, got %v", extra, rerr)
		}
		if rerr.Msg != "Upstream returned an invalid response" {
			t.Fatalf("malformed %q must be invalid, got %q", extra, rerr.Msg)
		}
	}
}

func TestResponsesLunaToolUsageFullAbsentControls(t *testing.T) {
	for _, extra := range []string{
		"",
		`,"tool_usage":null`,
		`,"tool_usage":{}`,
		`,"tool_usage":{"image_gen":null,"web_search":null}`,
		`,"tool_usage":{"web_search":{"num_requests":0}}`,
		lunaZeroToolUsage,
		`,"tool_usage":{"image_gen":{"input_tokens":0,"output_tokens":0,"total_tokens":0,"input_tokens_details":{"image_tokens":0,"text_tokens":0},"output_tokens_details":{"image_tokens":0,"text_tokens":0}}}`,
		`,"tool_usage":{"image_gen":{"input_tokens":null,"input_tokens_details":null},"web_search":{"num_requests":null}}`,
		`,"tool_usage":{"image_gen":{"input_tokens":0},"web_search":null,"future_null":null}`,
	} {
		raw := lunaFullRaw(extra)
		if _, rerr := DecodeResponsesResponse([]byte(raw), DefaultLimits()); rerr != nil {
			t.Fatalf("full extra %q must pass, got %v", extra, rerr)
		}
		for _, fn := range []func([]byte, string, Limits) ([]byte, *RouterError){
			TranslateResponsesJSONToChat, TranslateResponsesJSONToAnthropic, TranslateResponsesJSONToGemini,
		} {
			if _, rerr := fn([]byte(raw), "public", DefaultLimits()); rerr != nil {
				t.Fatalf("full extra %q must pass all targets, got %v", extra, rerr)
			}
		}
	}
}

func TestResponsesLunaToolUsageFullNonzeroUnrep(t *testing.T) {
	for _, extra := range []string{
		`,"tool_usage":{"web_search":{"num_requests":1}}`,
		`,"tool_usage":{"image_gen":{"input_tokens":1,"output_tokens":0,"total_tokens":0,"input_tokens_details":{"image_tokens":0,"text_tokens":0},"output_tokens_details":{"image_tokens":0,"text_tokens":0}},"web_search":{"num_requests":0}}`,
		`,"tool_usage":{"image_gen":{"input_tokens":0,"output_tokens":0,"total_tokens":0,"input_tokens_details":{"image_tokens":1,"text_tokens":0},"output_tokens_details":{"image_tokens":0,"text_tokens":0}},"web_search":{"num_requests":0}}`,
		`,"tool_usage":{"image_gen":{"input_tokens":0,"output_tokens":0,"total_tokens":0,"input_tokens_details":{"image_tokens":0,"text_tokens":0},"output_tokens_details":{"image_tokens":0,"text_tokens":5}},"web_search":{"num_requests":0}}`,
	} {
		_, rerr := DecodeResponsesResponse([]byte(lunaFullRaw(extra)), DefaultLimits())
		if rerr == nil || rerr.Status != 502 {
			t.Fatalf("full extra %q must fail 502, got %v", extra, rerr)
		}
		if !strings.Contains(rerr.Msg, "response.tool_usage") || !strings.Contains(rerr.Msg, "has no representation") {
			t.Fatalf("nonzero %q must name response.tool_usage as unrepresentable, got %q", extra, rerr.Msg)
		}
	}
}

func TestResponsesLunaToolUsageFullUnknownUnrep(t *testing.T) {
	for _, extra := range []string{
		`,"tool_usage":{"bogus_tool":{"num_requests":0}}`,
		`,"tool_usage":{"image_gen":{"bogus_counter":0}}`,
		`,"tool_usage":{"web_search":{"bogus":0}}`,
		`,"tool_usage":{"image_gen":{"input_tokens_details":{"bogus":0}}}`,
		`,"tool_usage":{"web_search":{"num_requests":0},"future":"x"}`,
	} {
		_, rerr := DecodeResponsesResponse([]byte(lunaFullRaw(extra)), DefaultLimits())
		if rerr == nil || rerr.Status != 502 {
			t.Fatalf("full extra %q must fail 502, got %v", extra, rerr)
		}
		if !strings.Contains(rerr.Msg, "response.tool_usage") || !strings.Contains(rerr.Msg, "has no representation") {
			t.Fatalf("unknown %q must name response.tool_usage as unrepresentable, got %q", extra, rerr.Msg)
		}
		if strings.Contains(rerr.Msg, `"x"`) {
			t.Fatalf("unknown error must be value-free, got %q", rerr.Msg)
		}
	}
}

func TestResponsesLunaToolUsageFullMalformedInvalid(t *testing.T) {
	for _, extra := range []string{
		`,"tool_usage":"x"`,
		`,"tool_usage":[]`,
		`,"tool_usage":5`,
		`,"tool_usage":true`,
		`,"tool_usage":{"image_gen":"x"}`,
		`,"tool_usage":{"image_gen":[]}`,
		`,"tool_usage":{"web_search":[]}`,
		`,"tool_usage":{"image_gen":{"input_tokens":"0"}}`,
		`,"tool_usage":{"image_gen":{"input_tokens":1.5}}`,
		`,"tool_usage":{"image_gen":{"input_tokens":-1}}`,
		`,"tool_usage":{"image_gen":{"input_tokens":true}}`,
		`,"tool_usage":{"image_gen":{"input_tokens_details":"x"}}`,
		`,"tool_usage":{"image_gen":{"input_tokens_details":{"image_tokens":"0"}}}`,
		`,"tool_usage":{"web_search":{"num_requests":"0"}}`,
		`,"tool_usage":{"web_search":{"num_requests":1.5}}`,
	} {
		_, rerr := DecodeResponsesResponse([]byte(lunaFullRaw(extra)), DefaultLimits())
		if rerr == nil || rerr.Status != 502 {
			t.Fatalf("full extra %q must fail 502, got %v", extra, rerr)
		}
		if rerr.Msg != "Upstream returned an invalid response" {
			t.Fatalf("malformed %q must be invalid, got %q", extra, rerr.Msg)
		}
	}
}

func lunaSSEUpstream(createdExtra, terminalExtra string) string {
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

func TestResponsesLunaEnvelopeSSEEnvelopes(t *testing.T) {
	liveText := `,"text":{"format":{"type":"text"},"verbosity":"medium"}`
	// Live blocker echoes on created + terminal pass end-to-end.
	out, rerr := runR2CStream(t, lunaSSEUpstream(liveText+lunaZeroToolUsage, liveText+lunaZeroToolUsage), true, DefaultLimits())
	if rerr != nil {
		t.Fatalf("SSE live Luna echoes must pass, got %v", rerr)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Fatalf("SSE live Luna echoes missing terminator: %s", out)
	}
	// Absent on created + terminal passes.
	out, rerr = runR2CStream(t, lunaSSEUpstream("", ""), true, DefaultLimits())
	if rerr != nil {
		t.Fatalf("SSE absent echoes must pass, got %v", rerr)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Fatalf("SSE absent missing terminator: %s", out)
	}
	// Unknown verbosity on created fails as unrepresentable before success.
	_, rerr = runR2CStream(t, lunaSSEUpstream(`,"text":{"format":{"type":"text"},"verbosity":"turbo"}`, liveText+lunaZeroToolUsage), true, DefaultLimits())
	if rerr == nil || rerr.Status != 502 || !strings.Contains(rerr.Msg, "response.text.verbosity") {
		t.Fatalf("SSE unknown verbosity created must fail naming response.text.verbosity, got %v", rerr)
	}
	// Nonzero tool_usage on terminal fails as unrepresentable.
	_, rerr = runR2CStream(t, lunaSSEUpstream(liveText+lunaZeroToolUsage, liveText+`,"tool_usage":{"web_search":{"num_requests":2}}`), true, DefaultLimits())
	if rerr == nil || rerr.Status != 502 || !strings.Contains(rerr.Msg, "response.tool_usage") {
		t.Fatalf("SSE nonzero tool_usage terminal must fail naming response.tool_usage, got %v", rerr)
	}
	// Unknown tool_usage on created fails as unrepresentable.
	_, rerr = runR2CStream(t, lunaSSEUpstream(`,"tool_usage":{"bogus_tool":{"num_requests":0}}`, liveText+lunaZeroToolUsage), true, DefaultLimits())
	if rerr == nil || rerr.Status != 502 || !strings.Contains(rerr.Msg, "response.tool_usage") {
		t.Fatalf("SSE unknown tool_usage created must fail naming response.tool_usage, got %v", rerr)
	}
	// Malformed verbosity on created fails invalid.
	_, rerr = runR2CStream(t, lunaSSEUpstream(`,"text":{"format":{"type":"text"},"verbosity":5}`, liveText+lunaZeroToolUsage), true, DefaultLimits())
	if rerr == nil || rerr.Status != 502 || rerr.Msg != "Upstream returned an invalid response" {
		t.Fatalf("SSE malformed verbosity created must be invalid, got %v", rerr)
	}
	// Malformed tool_usage on created fails invalid.
	_, rerr = runR2CStream(t, lunaSSEUpstream(`,"tool_usage":{"image_gen":{"input_tokens":"0"}}`, liveText+lunaZeroToolUsage), true, DefaultLimits())
	if rerr == nil || rerr.Status != 502 || rerr.Msg != "Upstream returned an invalid response" {
		t.Fatalf("SSE malformed tool_usage created must be invalid, got %v", rerr)
	}
	// Identity/terminal strictness preserved even with the live echoes present:
	// response identity changing at the terminal still fails.
	badIdentity := strings.Replace(
		lunaSSEUpstream(liveText+lunaZeroToolUsage, liveText+lunaZeroToolUsage),
		`"id":"resp_1","model":"gpt-4.1"`, `"id":"resp_2","model":"gpt-4.1"`, 1)
	_, rerr = runR2CStream(t, badIdentity, true, DefaultLimits())
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("identity change with live echoes must still fail, got %v", rerr)
	}
	// Terminal status contradicting the event kind still fails.
	badTerminal := strings.Replace(
		lunaSSEUpstream(liveText+lunaZeroToolUsage, liveText+lunaZeroToolUsage),
		`"status":"completed","output"`, `"status":"incomplete","output"`, 1)
	_, rerr = runR2CStream(t, badTerminal, true, DefaultLimits())
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("status contradiction with live echoes must still fail, got %v", rerr)
	}
}
