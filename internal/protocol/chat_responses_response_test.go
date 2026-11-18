package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustTranslateR2C(t *testing.T, raw string) map[string]any {
	t.Helper()
	out, rerr := TranslateResponsesJSONToChat([]byte(raw), "public", DefaultLimits())
	if rerr != nil {
		t.Fatalf("translate failed: %v", rerr)
	}
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestR2CTextAndTools(t *testing.T) {
	raw := `{"id":"resp_1","object":"response","created_at":1710000000,"model":"up",
		"status":"completed","output":[
			{"id":"msg_1","type":"message","role":"assistant","status":"completed",
				"content":[{"type":"output_text","text":"Hi ","annotations":[]},{"type":"output_text","text":"there","annotations":[]}]},
			{"id":"fc_1","type":"function_call","status":"completed","call_id":"call_abc123","name":"get_time","arguments":"{\"tz\":\"UTC\"}"}],
		"usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":40},"output_tokens":12,"output_tokens_details":{},"total_tokens":112}}`
	body := mustTranslateR2C(t, raw)
	if body["model"] != "public" {
		t.Fatalf("public model not restored: %v", body["model"])
	}
	choices := body["choices"].([]any)
	choice := choices[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if msg["content"] != "Hi there" {
		t.Fatalf("text not concatenated: %v", msg["content"])
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish must be tool_calls: %v", choice["finish_reason"])
	}
	tcs := msg["tool_calls"].([]any)
	tc := tcs[0].(map[string]any)
	// Chat identity is the Responses call_id, not the item id.
	if tc["id"] != "call_abc123" {
		t.Fatalf("call identity must be call_id: %v", tc["id"])
	}
	fn := tc["function"].(map[string]any)
	if fn["name"] != "get_time" {
		t.Fatalf("name lost: %v", fn)
	}
	usage := body["usage"].(map[string]any)
	// Responses input_tokens already includes cached: no double count.
	if usage["prompt_tokens"] != float64(100) || usage["total_tokens"] != float64(112) {
		t.Fatalf("usage accounting wrong (double count?): %v", usage)
	}
	det := usage["prompt_tokens_details"].(map[string]any)
	if det["cached_tokens"] != float64(40) {
		t.Fatalf("cached share lost: %v", usage)
	}
}

func TestR2CRefusalAndIncomplete(t *testing.T) {
	// Refusal maps to content_filter with a refusal field.
	body := mustTranslateR2C(t, `{"id":"resp_2","object":"response","created_at":1710000000,"model":"up",
		"status":"completed","output":[{"id":"msg_2","type":"message","role":"assistant","status":"completed",
			"content":[{"type":"refusal","refusal":"I cannot help."}]}],
		"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`)
	choice := body["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "content_filter" {
		t.Fatalf("refusal must be content_filter: %v", choice["finish_reason"])
	}
	// Incomplete max_output_tokens maps to length, not a fake stop.
	body = mustTranslateR2C(t, `{"id":"resp_3","object":"response","created_at":1710000000,"model":"up",
		"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},
		"output":[{"id":"msg_3","type":"message","role":"assistant","status":"incomplete","content":[{"type":"output_text","text":"partial","annotations":[]}]}],
		"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`)
	choice = body["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "length" {
		t.Fatalf("incomplete must be length: %v", choice["finish_reason"])
	}
}

func TestR2CFailures(t *testing.T) {
	must502 := func(raw string) {
		t.Helper()
		if _, rerr := TranslateResponsesJSONToChat([]byte(raw), "public", DefaultLimits()); rerr == nil || rerr.Status != 502 {
			t.Fatalf("expected 502 for %s, got %v", raw, rerr)
		}
	}
	base := func(output, usage, extra string) string {
		return `{"id":"resp_x","object":"response","created_at":1710000000,"model":"up",` + extra + `"output":[` + output + `],"usage":` + usage + `}`
	}
	okOut := `{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi","annotations":[]}]}`
	okUsage := `{"input_tokens":5,"output_tokens":2,"total_tokens":7}`
	// Failed status never becomes a fake stop.
	must502(strings.Replace(base(okOut, okUsage, `"status":"failed",`), `"status":"failed"`, `"status":"failed"`, 1))
	// Reasoning items have no Chat representation.
	must502(base(`{"id":"rs_1","type":"reasoning","status":"completed"}`, okUsage, `"status":"completed",`))
	// Nonzero reasoning/audio usage must fail, not disappear.
	must502(base(okOut, `{"input_tokens":5,"output_tokens":2,"total_tokens":7,"output_tokens_details":{"reasoning_tokens":3}}`, `"status":"completed",`))
	must502(base(okOut, `{"input_tokens":5,"input_tokens_details":{"cached_tokens":99},"output_tokens":2,"total_tokens":7}`, `"status":"completed",`))
	// Conflicting call identities.
	must502(base(`{"id":"fc_a","type":"function_call","status":"completed","call_id":"dup","name":"a","arguments":"{}"},{"id":"fc_b","type":"function_call","status":"completed","call_id":"dup","name":"b","arguments":"{}"}`, okUsage, `"status":"completed",`))
	// Usage total mismatch.
	must502(base(okOut, `{"input_tokens":5,"output_tokens":2,"total_tokens":99}`, `"status":"completed",`))
	// Large integers in args survive exactly when valid.
	body := mustTranslateR2C(t, base(`{"id":"fc_n","type":"function_call","status":"completed","call_id":"call_big","name":"calc","arguments":"{\"n\": 9007199254740993}"}`, okUsage, `"status":"completed",`))
	s, _ := json.Marshal(body)
	if !strings.Contains(string(s), "9007199254740993") {
		t.Fatalf("large integer not preserved: %s", s)
	}
}

func TestR2CReasoningAccounting(t *testing.T) {
	// Responses reasoning_tokens is exact accounting and maps to Chat
	// completion_tokens_details (not exposed reasoning text). Example:
	// prompt 7 cached 4 output 3 reasoning 2 total 10.
	raw := `{"id":"resp_x","object":"response","created_at":1710000000,"model":"up","status":"completed",` +
		`"output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi","annotations":[]}]}],` +
		`"usage":{"input_tokens":7,"input_tokens_details":{"cached_tokens":4},"output_tokens":3,"output_tokens_details":{"reasoning_tokens":2},"total_tokens":10}}`
	body := mustTranslateR2C(t, raw)
	usage, ok := body["usage"].(map[string]any)
	if !ok {
		t.Fatalf("missing usage: %v", body)
	}
	if usage["prompt_tokens"] != float64(7) || usage["completion_tokens"] != float64(3) || usage["total_tokens"] != float64(10) {
		t.Fatalf("usage totals wrong: %v", usage)
	}
	if det, ok := usage["prompt_tokens_details"].(map[string]any); !ok || det["cached_tokens"] != float64(4) {
		t.Fatalf("cached accounting lost: %v", usage)
	}
	if det, ok := usage["completion_tokens_details"].(map[string]any); !ok || det["reasoning_tokens"] != float64(2) {
		t.Fatalf("reasoning accounting must map to Chat completion details: %v", usage)
	}
	// Nonzero reasoning output items still refuse (state, not count).
	badReasoningItem := `{"id":"resp_x","object":"response","created_at":1710000000,"model":"up","status":"completed",` +
		`"output":[{"id":"rs_1","type":"reasoning","status":"completed"}],` +
		`"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}`
	if _, rerr := TranslateResponsesJSONToChat([]byte(badReasoningItem), "public", DefaultLimits()); rerr == nil || rerr.Status != 502 {
		t.Fatalf("reasoning output items must fail, got %v", rerr)
	}
	// Unknown unmapped accounting still fails.
	badUnknown := `{"id":"resp_x","object":"response","created_at":1710000000,"model":"up","status":"completed",` +
		`"output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi","annotations":[]}]}],` +
		`"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10,"output_tokens_details":{"future_tokens":1}}}`
	if _, rerr := TranslateResponsesJSONToChat([]byte(badUnknown), "public", DefaultLimits()); rerr == nil || rerr.Status != 502 {
		t.Fatalf("unknown accounting must fail, got %v", rerr)
	}
	// Reasoning subset overflow still fails.
	badOverflow := `{"id":"resp_x","object":"response","created_at":1710000000,"model":"up","status":"completed",` +
		`"output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi","annotations":[]}]}],` +
		`"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7,"output_tokens_details":{"reasoning_tokens":3}}}`
	if _, rerr := TranslateResponsesJSONToChat([]byte(badOverflow), "public", DefaultLimits()); rerr == nil || rerr.Status != 502 {
		t.Fatalf("reasoning over completion must fail, got %v", rerr)
	}
}
