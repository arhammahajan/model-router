package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func chatEnvelope(choices, usage string) string {
	return `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"up",
		"choices":[` + choices + `],"usage":` + usage + `}`
}

func chatChoice(finish, msg string) string {
	return `{"index":0,"message":` + msg + `,"finish_reason":` + finish + `}`
}

func usageTriple(prompt, completion int) string {
	total := prompt + completion
	return `{"prompt_tokens":` + g2cItoa(prompt) + `,"completion_tokens":` + g2cItoa(completion) + `,"total_tokens":` + g2cItoa(total) + `}`
}

func g2cItoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func mustGeminiJSON(t *testing.T, raw string) map[string]any {
	t.Helper()
	out, rerr := TranslateChatJSONToGemini([]byte(raw), "public-model", DefaultLimits())
	if rerr != nil {
		t.Fatalf("translate failed: %v", rerr.Msg)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func expectGemini502(t *testing.T, raw string) {
	t.Helper()
	if _, rerr := TranslateChatJSONToGemini([]byte(raw), "p", DefaultLimits()); rerr == nil || rerr.Status != 502 {
		t.Fatalf("expected 502, got %v", rerr)
	}
}

func TestChatToGeminiText(t *testing.T) {
	m := mustGeminiJSON(t, chatEnvelope(
		chatChoice(`"stop"`, `{"role":"assistant","content":"hello"}`),
		usageTriple(10, 3)))
	if m["modelVersion"] != "public-model" {
		t.Fatalf("public model not restored: %v", m)
	}
	if m["responseId"] != "chatcmpl-1" {
		t.Fatalf("responseId not stable: %v", m)
	}
	cands, _ := m["candidates"].([]any)
	c0, _ := cands[0].(map[string]any)
	if c0["finishReason"] != "STOP" {
		t.Fatalf("finish wrong: %v", c0)
	}
	if c0["index"] != float64(0) {
		t.Fatalf("candidate index wrong: %v", c0)
	}
	content, _ := c0["content"].(map[string]any)
	parts, _ := content["parts"].([]any)
	if len(parts) != 1 || parts[0].(map[string]any)["text"] != "hello" {
		t.Fatalf("parts wrong: %v", parts)
	}
	u, _ := m["usageMetadata"].(map[string]any)
	if u["promptTokenCount"] != float64(10) || u["candidatesTokenCount"] != float64(3) || u["totalTokenCount"] != float64(13) {
		t.Fatalf("usage wrong: %v", u)
	}
	if _, ok := u["thoughtsTokenCount"]; ok {
		t.Fatalf("thoughts fabricated: %v", u)
	}
}

func TestChatToGeminiToolCallExact(t *testing.T) {
	raw := chatEnvelope(
		chatChoice(`"tool_calls"`, `{"role":"assistant","content":null,
			"tool_calls":[{"id":"call_9","type":"function",
				"function":{"name":"get_weather","arguments":"{\"city\":\"Oslo\",\"n\":9007199254740993}"}}]}`),
		`{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12,
			"prompt_tokens_details":{"cached_tokens":2},
			"completion_tokens_details":{"reasoning_tokens":3}}`)
	out, rerr := TranslateChatJSONToGemini([]byte(raw), "public-model", DefaultLimits())
	if rerr != nil {
		t.Fatalf("translate: %v", rerr.Msg)
	}
	if !strings.Contains(string(out), "9007199254740993") {
		t.Fatalf("large integer lost: %s", out)
	}
	m := map[string]any{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	cands, _ := m["candidates"].([]any)
	c0, _ := cands[0].(map[string]any)
	parts, _ := c0["content"].(map[string]any)["parts"].([]any)
	if len(parts) != 1 {
		t.Fatalf("parts wrong: %v", parts)
	}
	fc, _ := parts[0].(map[string]any)["functionCall"].(map[string]any)
	if fc["name"] != "get_weather" || fc["id"] != "call_9" {
		t.Fatalf("call identity wrong: %v", fc)
	}
	u, _ := m["usageMetadata"].(map[string]any)
	// completion 7 minus 3 thoughts = 4 candidates; total stays 12.
	if u["candidatesTokenCount"] != float64(4) || u["totalTokenCount"] != float64(12) ||
		u["thoughtsTokenCount"] != float64(3) || u["cachedContentTokenCount"] != float64(2) {
		t.Fatalf("usage subsets wrong: %v", u)
	}
}

func TestChatToGeminiMalformedCall(t *testing.T) {
	m := mustGeminiJSON(t, chatEnvelope(
		chatChoice(`"stop"`, `{"role":"assistant","content":null,
			"tool_calls":[{"id":"a","type":"function",
				"function":{"name":"f","arguments":"[1,2]"}}]}`),
		usageTriple(1, 1)))
	cands, _ := m["candidates"].([]any)
	c0, _ := cands[0].(map[string]any)
	if c0["finishReason"] != "MALFORMED_FUNCTION_CALL" {
		t.Fatalf("malformed not mapped: %v", c0)
	}
}

func TestChatToGeminiUsageOverflowAndTotals(t *testing.T) {
	// total mismatch, cache overflow and thoughts overflow all fail closed.
	expectGemini502(t, chatEnvelope(
		chatChoice(`"stop"`, `{"role":"assistant","content":"x"}`),
		`{"prompt_tokens":5,"completion_tokens":7,"total_tokens":13}`))
	expectGemini502(t, chatEnvelope(
		chatChoice(`"stop"`, `{"role":"assistant","content":"x"}`),
		`{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12,
			"prompt_tokens_details":{"cached_tokens":6}}`))
	expectGemini502(t, chatEnvelope(
		chatChoice(`"stop"`, `{"role":"assistant","content":"x"}`),
		`{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12,
			"completion_tokens_details":{"reasoning_tokens":8}}`))
	// Nonzero unmapped members are refused; known zero buckets are covered
	// by TestChatZeroUsageDetailsAcrossTargets.
	expectGemini502(t, chatEnvelope(
		chatChoice(`"stop"`, `{"role":"assistant","content":"x"}`),
		`{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12,
			"prompt_tokens_details":{"cached_tokens":null,"audio_tokens":1}}`))
	expectGemini502(t, chatEnvelope(
		chatChoice(`"stop"`, `{"role":"assistant","content":"x"}`),
		`{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12,
			"completion_tokens_details":{"reasoning_tokens":null,"audio_tokens":1}}`))
	expectGemini502(t, chatEnvelope(
		chatChoice(`"stop"`, `{"role":"assistant","content":"x"}`),
		`{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12,"extra":1}`))
	// Null details mean absent and are accepted.
	mustGeminiJSON(t, chatEnvelope(
		chatChoice(`"stop"`, `{"role":"assistant","content":"x"}`),
		`{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12,
			"prompt_tokens_details":null,"completion_tokens_details":null}`))
}

func TestChatToGeminiRejectsUnrepresentable(t *testing.T) {
	base := func(msg string) string {
		return chatEnvelope(chatChoice(`"stop"`, msg), usageTriple(1, 1))
	}
	for _, raw := range []string{
		base(`{"role":"assistant","content":"x","audio":{}}`),
		base(`{"role":"assistant","content":"x","function_call":{"name":"f"}}`),
		base(`{"role":"assistant","content":"x","refusal":"no"}`),
		base(`{"role":"assistant","content":"x","annotations":[{"type":"url_citation"}]}`),
		chatEnvelope(`{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"stop"},
			{"index":1,"message":{"role":"assistant","content":"y"},"finish_reason":"stop"}`,
			usageTriple(1, 1)),
		chatEnvelope(chatChoice(`"quantum"`, `{"role":"assistant","content":"x"}`), usageTriple(1, 1)),
		chatEnvelope(chatChoice(`"stop"`, `{"role":"user","content":"x"}`), usageTriple(1, 1)),
		chatEnvelope(chatChoice(`"stop"`, `{"role":"assistant","content":"x","bogus":1}`), usageTriple(1, 1)),
		`{"id":1}`,
	} {
		expectGemini502(t, raw)
	}
	// Conflicting call identities fail closed.
	expectGemini502(t, chatEnvelope(
		chatChoice(`"tool_calls"`, `{"role":"assistant","content":null,"tool_calls":[
			{"id":"dup","type":"function","function":{"name":"f","arguments":"{}"}},
			{"id":"dup","type":"function","function":{"name":"g","arguments":"{}"}}]}`),
		usageTriple(1, 1)))
	// Tool count bound enforced.
	many := `{"role":"assistant","content":null,"tool_calls":[`
	for i := 0; i < 1025; i++ {
		if i > 0 {
			many += ","
		}
		many += `{"id":"c` + g2cItoa(i) + `","type":"function","function":{"name":"f","arguments":"{}"}}`
	}
	expectGemini502(t, chatEnvelope(chatChoice(`"tool_calls"`, many+`]`), usageTriple(1, 1)))
}
