package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func r2gResp(status, output, usage string) []byte {
	return []byte(`{"id":"resp_1","object":"response","created_at":1746989954.0,"model":"gpt-x",
		"status":` + status + `,"error":null,"incomplete_details":null,
		"output":` + output + `,"usage":` + usage + `}`)
}

func r2gUsage() string {
	return `{"input_tokens":10,"input_tokens_details":{"cached_tokens":3},"output_tokens":7,"output_tokens_details":{"reasoning_tokens":2},"total_tokens":17}`
}

// Text completes to STOP with exact usage buckets.
func TestResponsesToGeminiText(t *testing.T) {
	raw := r2gResp(`"completed"`,
		`[{"type":"message","id":"msg_1","status":"completed","role":"assistant",
			"content":[{"type":"output_text","text":"hello","annotations":[],"logprobs":[]}]}]`,
		r2gUsage())
	out, rerr := TranslateResponsesJSONToGemini(raw, "gemini-2.0-flash", DefaultLimits())
	if rerr != nil {
		t.Fatalf("translate: %v", rerr)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatalf("out: %v", err)
	}
	var mv, rid string
	_ = json.Unmarshal(top["modelVersion"], &mv)
	_ = json.Unmarshal(top["responseId"], &rid)
	if mv != "gemini-2.0-flash" || rid != "resp_1" {
		t.Fatalf("identity: %s", out)
	}
	var cands []map[string]json.RawMessage
	if err := json.Unmarshal(top["candidates"], &cands); err != nil || len(cands) != 1 {
		t.Fatalf("candidates: %s", out)
	}
	var finish string
	_ = json.Unmarshal(cands[0]["finishReason"], &finish)
	if finish != "STOP" {
		t.Fatalf("finish: %s", out)
	}
	var usage map[string]json.RawMessage
	if err := json.Unmarshal(top["usageMetadata"], &usage); err != nil {
		t.Fatalf("usage: %v", err)
	}
	got := map[string]int{}
	for _, k := range []string{"promptTokenCount", "candidatesTokenCount", "totalTokenCount", "cachedContentTokenCount", "thoughtsTokenCount"} {
		var n int
		if err := json.Unmarshal(usage[k], &n); err != nil {
			t.Fatalf("usage %s: %s", k, out)
		}
		got[k] = n
	}
	// input 10 (cached 3 included), output 7 (reasoning 2) -> candidates 5.
	if got["promptTokenCount"] != 10 || got["candidatesTokenCount"] != 5 ||
		got["totalTokenCount"] != 17 || got["cachedContentTokenCount"] != 3 || got["thoughtsTokenCount"] != 2 {
		t.Fatalf("usage buckets: %v", got)
	}
}

// Tool calls keep exact raw schemas/args/numbers with opaque IDs preserved.
func TestResponsesToGeminiToolsExact(t *testing.T) {
	raw := r2gResp(`"completed"`,
		`[{"type":"function_call","id":"fc_9","call_id":"call-opaque-1","name":"calc",
			"arguments":"{\"n\":9007199254740993}","status":"completed"}]`,
		r2gUsage())
	out, rerr := TranslateResponsesJSONToGemini(raw, "m", DefaultLimits())
	if rerr != nil {
		t.Fatalf("translate: %v", rerr)
	}
	s := string(out)
	if !strings.Contains(s, `"id":"call-opaque-1"`) || !strings.Contains(s, "9007199254740993") {
		t.Fatalf("opaque IDs and large integers must survive exactly: %s", s)
	}
	if strings.Contains(s, "fc_9") {
		t.Fatalf("item id must not leak into the call identity: %s", s)
	}
}

// Incomplete reasons map; failed never becomes a stop.
func TestResponsesToGeminiTerminal(t *testing.T) {
	usage := r2gUsage()
	for _, tc := range []struct {
		status, det, want string
		ok                bool
	}{
		{`"incomplete"`, `{"reason":"max_output_tokens"}`, "MAX_TOKENS", true},
		{`"incomplete"`, `{"reason":"content_filter"}`, "SAFETY", true},
		{`"incomplete"`, `{"reason":"other"}`, "", false},
		{`"failed"`, `null`, "", false},
		{`"bogus"`, `null`, "", false},
	} {
		raw := []byte(`{"id":"r","object":"response","created_at":1,"model":"m","status":` + tc.status +
			`,"error":null,"incomplete_details":` + tc.det + `,"output":[],"usage":` + usage + `}`)
		out, rerr := TranslateResponsesJSONToGemini(raw, "m", DefaultLimits())
		if !tc.ok {
			if rerr == nil {
				t.Fatalf("status %s must fail", tc.status)
			}
			continue
		}
		if rerr != nil {
			t.Fatalf("status %s: %v", tc.status, rerr)
		}
		if !strings.Contains(string(out), `"`+tc.want+`"`) {
			t.Fatalf("want %s: %s", tc.want, out)
		}
	}
}

// Unknown non-null response features fail closed; unknown nulls pass.
func TestResponsesToGeminiEnvelopeClosed(t *testing.T) {
	base := func(extra string) []byte {
		return []byte(`{"id":"r","object":"response","created_at":1,"model":"m","status":"completed",
			"error":null,"incomplete_details":null,"output":[],"usage":` + r2gUsage() + extra + `}`)
	}
	if _, rerr := TranslateResponsesJSONToGemini(base(``), "m", DefaultLimits()); rerr != nil {
		t.Fatalf("base must pass: %v", rerr)
	}
	if _, rerr := TranslateResponsesJSONToGemini(base(`,"future_feature":null`), "m", DefaultLimits()); rerr != nil {
		t.Fatalf("unknown null must pass: %v", rerr)
	}
	if _, rerr := TranslateResponsesJSONToGemini(base(`,"future_feature":"x"`), "m", DefaultLimits()); rerr == nil {
		t.Fatalf("unknown non-null must fail")
	}
	// Refusal, annotations and unknown item types have no Gemini form.
	for _, output := range []string{
		`[{"type":"message","status":"completed","role":"assistant","content":[{"type":"refusal","refusal":"no"}]}]`,
		`[{"type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"a","annotations":[{"x":1}],"logprobs":[]}]}]`,
		`[{"type":"reasoning","id":"rs_1"}]`,
	} {
		raw := r2gResp(`"completed"`, output, r2gUsage())
		if _, rerr := TranslateResponsesJSONToGemini(raw, "m", DefaultLimits()); rerr == nil {
			t.Fatalf("unrepresentable must fail: %s", output)
		}
	}
}

// Malformed args become MALFORMED_FUNCTION_CALL, not a silent drop.
func TestResponsesToGeminiMalformedArgs(t *testing.T) {
	raw := r2gResp(`"completed"`,
		`[{"type":"function_call","call_id":"c1","name":"f","arguments":"oops","status":"completed"}]`,
		r2gUsage())
	out, rerr := TranslateResponsesJSONToGemini(raw, "m", DefaultLimits())
	if rerr != nil {
		t.Fatalf("translate: %v", rerr)
	}
	if !strings.Contains(string(out), "MALFORMED_FUNCTION_CALL") {
		t.Fatalf("want MALFORMED_FUNCTION_CALL: %s", out)
	}
}

// Usage accounting is exact: cached subset, reasoning subset, exact total.
func TestResponsesToGeminiUsageExact(t *testing.T) {
	for _, usage := range []string{
		`{"input_tokens":10,"input_tokens_details":{"cached_tokens":11},"output_tokens":7,"output_tokens_details":{},"total_tokens":17}`,
		`{"input_tokens":10,"input_tokens_details":{},"output_tokens":7,"output_tokens_details":{"reasoning_tokens":8},"total_tokens":17}`,
		`{"input_tokens":10,"input_tokens_details":{},"output_tokens":7,"output_tokens_details":{},"total_tokens":18}`,
		`{"input_tokens":10,"input_tokens_details":{},"output_tokens":7,"output_tokens_details":{"audio_tokens":1},"total_tokens":17}`,
		`{"input_tokens":10,"input_tokens_details":{"image_tokens":2},"output_tokens":7,"output_tokens_details":{},"total_tokens":17}`,
	} {
		raw := r2gResp(`"completed"`, `[]`, usage)
		if _, rerr := TranslateResponsesJSONToGemini(raw, "m", DefaultLimits()); rerr == nil {
			t.Fatalf("bad usage must fail: %s", usage)
		}
	}
	// Zero harmless details are exact, not failures.
	raw := r2gResp(`"completed"`, `[]`,
		`{"input_tokens":10,"input_tokens_details":{"text_tokens":0},"output_tokens":7,
		  "output_tokens_details":{"text_tokens":0},"total_tokens":17}`)
	if _, rerr := TranslateResponsesJSONToGemini(raw, "m", DefaultLimits()); rerr != nil {
		t.Fatalf("zero details must pass: %v", rerr)
	}
	// Conflicting call identities fail.
	raw = r2gResp(`"completed"`,
		`[{"type":"function_call","call_id":"c1","name":"f","arguments":"{}","status":"completed"},
		  {"type":"function_call","call_id":"c1","name":"f","arguments":"{}","status":"completed"}]`,
		r2gUsage())
	if _, rerr := TranslateResponsesJSONToGemini(raw, "m", DefaultLimits()); rerr == nil {
		t.Fatalf("conflicting identities must fail")
	}
}

// Top-level reasoning echo uses request-control types, not output-item types.
func TestResponsesToGeminiReasoningEcho(t *testing.T) {
	raw := []byte(`{"id":"resp_1","object":"response","model":"m","status":"completed",` +
		`"output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2},` +
		`"reasoning":{"effort":"medium","summary":"auto"}}`)
	if _, rerr := TranslateResponsesJSONToGemini(raw, "m", DefaultLimits()); rerr != nil {
		t.Fatalf("valid summary echo rejected: %v", rerr)
	}
	bad := []byte(`{"id":"r","object":"response","model":"m","status":"completed",` +
		`"output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2},` +
		`"reasoning":{"effort":"medium","summary":["x"]}}`)
	if _, rerr := TranslateResponsesJSONToGemini(bad, "m", DefaultLimits()); rerr == nil {
		t.Fatal("array summary must fail (output-item shape, not echo)")
	}
	badEffort := []byte(`{"id":"r","object":"response","model":"m","status":"completed",` +
		`"output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2},` +
		`"reasoning":{"effort":"turbo"}}`)
	if _, rerr := TranslateResponsesJSONToGemini(badEffort, "m", DefaultLimits()); rerr == nil {
		t.Fatal("unknown effort must fail")
	}
	reasoningItem := r2gResp(`"completed"`, `[{"type":"reasoning","id":"rs_1"}]`, r2gUsage())
	if _, rerr := TranslateResponsesJSONToGemini(reasoningItem, "m", DefaultLimits()); rerr == nil {
		t.Fatal("reasoning output items have no Gemini form")
	}
}
