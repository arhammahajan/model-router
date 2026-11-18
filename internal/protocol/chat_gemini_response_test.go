// Gemini -> Chat response codec tests: complete JSON translation.
package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func g2cTranslate(t *testing.T, body string) map[string]any {
	t.Helper()
	out, rerr := TranslateGeminiJSONToChat([]byte(body), "public-model", DefaultLimits())
	if rerr != nil {
		t.Fatalf("translate failed: %v", rerr)
	}
	var v map[string]any
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	if v["model"] != "public-model" {
		t.Fatalf("public model not restored: %v", v["model"])
	}
	return v
}

func g2cTranslateErr(t *testing.T, body string) *RouterError {
	t.Helper()
	_, rerr := TranslateGeminiJSONToChat([]byte(body), "public-model", DefaultLimits())
	if rerr == nil {
		t.Fatalf("expected error for %s", body)
	}
	if rerr.Status != 502 {
		t.Fatalf("response failures must be 502, got %v", rerr)
	}
	return rerr
}

func g2cUsage(prompt, cand int) string {
	total := prompt + cand
	return `{"promptTokenCount":` + itoa(prompt) + `,"candidatesTokenCount":` + itoa(cand) + `,"totalTokenCount":` + itoa(total) + `}`
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func TestG2CTextStop(t *testing.T) {
	v := g2cTranslate(t, `{"candidates":[{"content":{"role":"model",
		"parts":[{"text":"Hello"}]},"finishReason":"STOP"}],
		"usageMetadata":`+g2cUsage(11, 4)+`,"modelVersion":"gemini-2.5-flash","responseId":"r1"}`)
	choices := v["choices"].([]any)
	ch := choices[0].(map[string]any)
	if ch["finish_reason"] != "stop" {
		t.Fatalf("finish = %v", ch["finish_reason"])
	}
	msg := ch["message"].(map[string]any)
	if msg["content"] != "Hello" || msg["role"] != "assistant" {
		t.Fatalf("message = %v", msg)
	}
	usage := v["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(11) || usage["completion_tokens"] != float64(4) || usage["total_tokens"] != float64(15) {
		t.Fatalf("usage = %v", usage)
	}
}

func TestG2CToolCallsPreserveIDs(t *testing.T) {
	v := g2cTranslate(t, `{"candidates":[{"content":{"role":"model","parts":[
		{"functionCall":{"name":"get_weather","args":{"city":"SF"},"id":"call_1"}},
		{"functionCall":{"name":"get_time","args":{},"id":"call_2"}}]},"finishReason":"STOP"}],
		"usageMetadata":`+g2cUsage(20, 8)+`}`)
	choices := v["choices"].([]any)
	ch := choices[0].(map[string]any)
	if ch["finish_reason"] != "tool_calls" {
		t.Fatalf("calls must yield tool_calls, got %v", ch["finish_reason"])
	}
	msg := ch["message"].(map[string]any)
	calls := msg["tool_calls"].([]any)
	if len(calls) != 2 {
		t.Fatalf("calls = %v", calls)
	}
	ids := map[string]bool{}
	for _, c := range calls {
		cm := c.(map[string]any)
		ids[cm["id"].(string)] = true
		fn := cm["function"].(map[string]any)
		if fn["name"] == "get_weather" && fn["arguments"] != `{"city":"SF"}` {
			t.Fatalf("args must be exact JSON string: %v", fn["arguments"])
		}
	}
	if !ids["call_1"] || !ids["call_2"] {
		t.Fatalf("provider IDs must be preserved exactly: %v", ids)
	}
}

func TestG2CAbsentIDsMintScopedCompat(t *testing.T) {
	v := g2cTranslate(t, `{"candidates":[{"content":{"role":"model","parts":[
		{"functionCall":{"name":"f","args":{}}},
		{"functionCall":{"name":"f","args":{}}}]},"finishReason":"STOP"}],
		"usageMetadata":`+g2cUsage(5, 3)+`}`)
	choices := v["choices"].([]any)
	ch := choices[0].(map[string]any)
	msg := ch["message"].(map[string]any)
	calls := msg["tool_calls"].([]any)
	seen := map[string]bool{}
	for _, c := range calls {
		id := c.(map[string]any)["id"].(string)
		if !strings.HasPrefix(id, "gemini-compat-call-") {
			t.Fatalf("minted id must carry the gemini-compat-call- scope, got %q", id)
		}
		if seen[id] {
			t.Fatalf("minted ids must be unique: %v", calls)
		}
		seen[id] = true
	}
	// A supplied id colliding with the first minted suffix must not collide.
	v2 := g2cTranslate(t, `{"candidates":[{"content":{"role":"model","parts":[
		{"functionCall":{"name":"f","args":{},"id":"gemini-compat-call-0"}},
		{"functionCall":{"name":"f","args":{}}}]},"finishReason":"STOP"}],
		"usageMetadata":`+g2cUsage(5, 3)+`}`)
	ch2 := v2["choices"].([]any)[0].(map[string]any)
	calls2 := ch2["message"].(map[string]any)["tool_calls"].([]any)
	if len(calls2) != 2 || calls2[0].(map[string]any)["id"] == calls2[1].(map[string]any)["id"] {
		t.Fatalf("minted id must avoid supplied ids: %v", calls2)
	}
}

func TestG2CDuplicateIDsFail(t *testing.T) {
	g2cTranslateErr(t, `{"candidates":[{"content":{"role":"model","parts":[
		{"functionCall":{"name":"f","args":{},"id":"dup"}},
		{"functionCall":{"name":"f","args":{},"id":"dup"}}]},"finishReason":"STOP"}],
		"usageMetadata":`+g2cUsage(5, 3)+`}`)
}

func TestG2CFinishMapping(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"STOP", "stop"},
		{"MAX_TOKENS", "length"},
		{"SAFETY", "content_filter"},
		{"RECITATION", "content_filter"},
		{"BLOCKLIST", "content_filter"},
		{"PROHIBITED_CONTENT", "content_filter"},
		{"SPII", "content_filter"},
	} {
		v := g2cTranslate(t, `{"candidates":[{"content":{"role":"model",
			"parts":[{"text":"x"}]},"finishReason":"`+tc.in+`"}],
			"usageMetadata":`+g2cUsage(5, 2)+`}`)
		ch := v["choices"].([]any)[0].(map[string]any)
		if ch["finish_reason"] != tc.want {
			t.Fatalf("%s: got %v want %s", tc.in, ch["finish_reason"], tc.want)
		}
	}
	// Mutation guard: safety must never become an ordinary stop.
	v := g2cTranslate(t, `{"candidates":[{"content":{"role":"model",
		"parts":[{"text":"x"}]},"finishReason":"SAFETY"}],
		"usageMetadata":`+g2cUsage(5, 2)+`}`)
	if v["choices"].([]any)[0].(map[string]any)["finish_reason"] == "stop" {
		t.Fatalf("safety became ordinary stop")
	}
	// Unknown/stop-agnostic reasons fail closed.
	g2cTranslateErr(t, `{"candidates":[{"content":{"role":"model",
		"parts":[{"text":"x"}]},"finishReason":"MALFORMED_FUNCTION_CALL"}],
		"usageMetadata":`+g2cUsage(5, 2)+`}`)
	g2cTranslateErr(t, `{"candidates":[{"content":{"role":"model",
		"parts":[{"text":"x"}]},"finishReason":"OTHER"}],
		"usageMetadata":`+g2cUsage(5, 2)+`}`)
	g2cTranslateErr(t, `{"candidates":[{"content":{"role":"model",
		"parts":[{"text":"x"}]},"finishReason":"FINISH_REASON_UNSPECIFIED"}],
		"usageMetadata":`+g2cUsage(5, 2)+`}`)
}

func TestG2CBlockedPrompt(t *testing.T) {
	v := g2cTranslate(t, `{"promptFeedback":{"blockReason":"SAFETY","safetyRatings":[]},
		"usageMetadata":`+g2cUsage(7, 0)+`}`)
	ch := v["choices"].([]any)[0].(map[string]any)
	if ch["finish_reason"] != "content_filter" {
		t.Fatalf("blocked prompt must be content_filter, got %v", ch["finish_reason"])
	}
	// Blocked response contradicting candidates fails.
	g2cTranslateErr(t, `{"promptFeedback":{"blockReason":"SAFETY"},
		"candidates":[{"content":{"role":"model","parts":[{"text":"x"}]},"finishReason":"STOP"}],
		"usageMetadata":`+g2cUsage(7, 1)+`}`)
}

func TestG2CUnrepresentableParts(t *testing.T) {
	bodies := []string{
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"t","thoughtSignature":"opaque"}]},"finishReason":"STOP"}],"usageMetadata":` + g2cUsage(5, 2) + `}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"thought":true,"text":"t"}]},"finishReason":"STOP"}],"usageMetadata":` + g2cUsage(5, 2) + `}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"inlineData":{"mimeType":"image/png","data":"eA=="}}]},"finishReason":"STOP"}],"usageMetadata":` + g2cUsage(5, 2) + `}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"executableCode":{"language":"PYTHON","code":"1+1"}}]},"finishReason":"STOP"}],"usageMetadata":` + g2cUsage(5, 2) + `}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"f","thoughtSignature":"opaque"}}]},"finishReason":"STOP"}],"usageMetadata":` + g2cUsage(5, 2) + `}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"f","partialArgs":[{"jsonPath":"$.a"}]}}]},"finishReason":"STOP"}],"usageMetadata":` + g2cUsage(5, 2) + `}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"f","args":{},"willContinue":true}}]},"finishReason":"STOP"}],"usageMetadata":` + g2cUsage(5, 2) + `}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"x"}],"groundingMetadata":{"a":1}}],"finishReason":"STOP"}],"usageMetadata":` + g2cUsage(5, 2) + `}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"x"}]},"finishReason":"STOP","continuationToken":"opaque"}],"usageMetadata":` + g2cUsage(5, 2) + `}`,
	}
	for i, b := range bodies {
		g2cTranslateErr(t, b)
		_ = i
	}
}

func TestG2CUsageAccounting(t *testing.T) {
	// Cached share maps without double counting.
	v := g2cTranslate(t, `{"candidates":[{"content":{"role":"model",
		"parts":[{"text":"x"}]},"finishReason":"STOP"}],
		"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":4,"totalTokenCount":14,
		"cachedContentTokenCount":6}}`)
	usage := v["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(10) || usage["total_tokens"] != float64(14) {
		t.Fatalf("prompt must include cache verbatim: %v", usage)
	}
	det := usage["prompt_tokens_details"].(map[string]any)
	if det["cached_tokens"] != float64(6) {
		t.Fatalf("cached detail: %v", usage)
	}
	// Mismatched totals, cache overflow, nonzero reasoning/tool-use all fail.
	for _, u := range []string{
		`{"promptTokenCount":10,"candidatesTokenCount":4,"totalTokenCount":15}`,
		`{"promptTokenCount":10,"candidatesTokenCount":4,"totalTokenCount":14,"cachedContentTokenCount":11}`,
		`{"promptTokenCount":10,"candidatesTokenCount":4,"totalTokenCount":16,"thoughtsTokenCount":2}`,
		`{"promptTokenCount":10,"candidatesTokenCount":4,"totalTokenCount":16,"toolUsePromptTokenCount":2}`,
		`{"promptTokenCount":10,"candidatesTokenCount":4,"totalTokenCount":14,"promptTokensDetails":[{"modality":"TEXT","tokenCount":3}]}`,
	} {
		g2cTranslateErr(t, `{"candidates":[{"content":{"role":"model",
			"parts":[{"text":"x"}]},"finishReason":"STOP"}],"usageMetadata":`+u+`}`)
	}
	// Zero reasoning/tool-use and empty detail arrays are harmless defaults.
	g2cTranslate(t, `{"candidates":[{"content":{"role":"model",
		"parts":[{"text":"x"}]},"finishReason":"STOP"}],
		"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":4,"totalTokenCount":14,
		"thoughtsTokenCount":0,"toolUsePromptTokenCount":null,
		"promptTokensDetails":[],"trafficType":"ON_DEMAND"}}`)
	// Quoted and fractional counters fail.
	g2cTranslateErr(t, `{"candidates":[{"content":{"role":"model",
		"parts":[{"text":"x"}]},"finishReason":"STOP"}],
		"usageMetadata":{"promptTokenCount":"10","candidatesTokenCount":4,"totalTokenCount":14}}`)
}

func TestG2CMultipleCandidatesAndEnvelope(t *testing.T) {
	two := `{"candidates":[
		{"content":{"role":"model","parts":[{"text":"a"}]},"finishReason":"STOP"},
		{"content":{"role":"model","parts":[{"text":"b"}]},"finishReason":"STOP"}}],
		"usageMetadata":` + g2cUsage(5, 2) + `}`
	g2cTranslateErr(t, two)
	// Unknown non-null envelope members fail; nulls pass as newer defaults.
	g2cTranslateErr(t, `{"candidates":[{"content":{"role":"model",
		"parts":[{"text":"x"}]},"finishReason":"STOP"}],
		"usageMetadata":`+g2cUsage(5, 2)+`,"mysteryField":1}`)
	g2cTranslate(t, `{"candidates":[{"content":{"role":"model",
		"parts":[{"text":"x"}]},"finishReason":"STOP"}],
		"usageMetadata":`+g2cUsage(5, 1)+`,"mysteryField":null,"createTime":"2026-01-01T00:00:00Z"}`)
}
