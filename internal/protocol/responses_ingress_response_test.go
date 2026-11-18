package protocol

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func decodeResponsesJSON(t *testing.T, raw string) map[string]any {
	t.Helper()
	out, rerr := TranslateChatJSONToResponses([]byte(raw), "public-model", DefaultLimits())
	if rerr != nil {
		t.Fatalf("translate failed: %v", rerr)
	}
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func chatCompletion(text, finish string, usage string) string {
	return `{"id":"chatcmpl-1","object":"chat.completion","created":1700000000,
		"model":"upstream","choices":[{"index":0,
		"message":{"role":"assistant","content":` + text + `},
		"finish_reason":"` + finish + `"}],"usage":` + usage + `}`
}

func fullUsage() string {
	return `{"prompt_tokens":12,"completion_tokens":5,"total_tokens":17,
		"prompt_tokens_details":{"cached_tokens":3,"audio_tokens":0},
		"completion_tokens_details":{"reasoning_tokens":2,"audio_tokens":0,"text_tokens":5}}`
}

func TestChatJSONToResponsesText(t *testing.T) {
	body := decodeResponsesJSON(t, chatCompletion(`"Hello there"`, "stop", fullUsage()))
	if body["object"] != "response" || body["status"] != "completed" {
		t.Fatalf("bad envelope: %v", body)
	}
	if body["model"] != "public-model" {
		t.Fatalf("public model not rendered: %v", body["model"])
	}
	if body["error"] != nil || body["incomplete_details"] != nil {
		t.Fatalf("completed response must have null error/details: %v", body)
	}
	output := body["output"].([]any)
	msg := output[0].(map[string]any)
	if msg["type"] != "message" || msg["role"] != "assistant" || msg["status"] != "completed" {
		t.Fatalf("bad message item: %v", msg)
	}
	parts := msg["content"].([]any)
	part := parts[0].(map[string]any)
	if part["type"] != "output_text" || part["text"] != "Hello there" {
		t.Fatalf("bad text part: %v", part)
	}
	usage := body["usage"].(map[string]any)
	// Responses accounting: input keeps the full prompt total, cached
	// tokens break out separately (no Anthropic-style subtraction).
	if usage["input_tokens"] != float64(12) || usage["output_tokens"] != float64(5) ||
		usage["total_tokens"] != float64(17) {
		t.Fatalf("bad usage totals: %v", usage)
	}
	details := usage["input_tokens_details"].(map[string]any)
	if details["cached_tokens"] != float64(3) {
		t.Fatalf("cached accounting lost: %v", details)
	}
	outDet := usage["output_tokens_details"].(map[string]any)
	if outDet["reasoning_tokens"] != float64(2) {
		t.Fatalf("reasoning accounting lost: %v", outDet)
	}
}

func TestChatJSONToResponsesTools(t *testing.T) {
	raw := `{"id":"chatcmpl-1","object":"chat.completion","created":1700000000,
		"model":"upstream","choices":[{"index":0,"finish_reason":"tool_calls",
		"message":{"role":"assistant","content":"",
		"tool_calls":[
			{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}},
			{"id":"call_2","type":"function","function":{"name":"ping","arguments":""}}]}}],
		"usage":{"prompt_tokens":20,"completion_tokens":8,"total_tokens":28}}`
	body := decodeResponsesJSON(t, raw)
	output := body["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("expected 2 calls and no empty message item, got %v", output)
	}
	// Upstream Chat tool_choice objects must never leak into Responses:
	// the envelope carries no Chat-style choice.
	for k := range body {
		if k == "tool_choice" {
			t.Fatalf("Chat tool_choice must not leak: %v", body[k])
		}
	}
	seenIDs := map[string]bool{}
	seenCallIDs := map[string]bool{}
	for _, item := range output {
		fc := item.(map[string]any)
		if fc["type"] != "function_call" || fc["status"] != "completed" {
			t.Fatalf("bad call item: %v", fc)
		}
		id := fc["id"].(string)
		if !strings.HasPrefix(id, "fc_") || seenIDs[id] {
			t.Fatalf("item identity must be a distinct fc_ id: %v", fc)
		}
		seenIDs[id] = true
		callID := fc["call_id"].(string)
		if strings.HasPrefix(callID, "fc_") || seenCallIDs[callID] {
			t.Fatalf("call_id must be the upstream identity: %v", fc)
		}
		seenCallIDs[callID] = true
	}
	if !seenCallIDs["call_1"] || !seenCallIDs["call_2"] {
		t.Fatalf("call identities lost: %v", output)
	}
	empty := output[1].(map[string]any)
	if empty["arguments"] != "{}" {
		t.Fatalf("empty args must render as explicit {}: %v", empty)
	}
}

func TestChatJSONToResponsesRefusal(t *testing.T) {
	raw := `{"id":"chatcmpl-1","object":"chat.completion","created":1,
		"model":"u","choices":[{"index":0,"finish_reason":"content_filter",
		"message":{"role":"assistant","content":null,"refusal":"I cannot help"}}],
		"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}}`
	body := decodeResponsesJSON(t, raw)
	if body["status"] != "incomplete" {
		t.Fatalf("content filter must be incomplete, got %v", body["status"])
	}
	det := body["incomplete_details"].(map[string]any)
	if det["reason"] != "content_filter" {
		t.Fatalf("bad incomplete reason: %v", det)
	}
	// The explicit refusal text is preserved, not fabricated into completion.
	msg := body["output"].([]any)[0].(map[string]any)
	if msg["status"] != "incomplete" {
		t.Fatalf("item status must follow response: %v", msg)
	}
	part := msg["content"].([]any)[0].(map[string]any)
	if part["type"] != "refusal" || part["refusal"] != "I cannot help" {
		t.Fatalf("refusal not preserved: %v", part)
	}
}

func TestChatJSONToResponsesLength(t *testing.T) {
	body := decodeResponsesJSON(t, chatCompletion(`"partial"`, "length",
		`{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}`))
	if body["status"] != "incomplete" {
		t.Fatalf("length must be incomplete: %v", body)
	}
	if body["incomplete_details"].(map[string]any)["reason"] != "max_output_tokens" {
		t.Fatalf("bad reason: %v", body["incomplete_details"])
	}
}

func TestChatJSONToResponsesRejects(t *testing.T) {
	usage := `{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}`
	cases := map[string]string{
		"multiple choices": `{"id":"c","object":"chat.completion","created":1,"model":"u",
			"choices":[{"index":0,"message":{"role":"assistant","content":"a"},"finish_reason":"stop"},
			{"index":1,"message":{"role":"assistant","content":"b"},"finish_reason":"stop"}],
			"usage":` + usage + `}`,
		"unknown stop reason": `{"id":"c","object":"chat.completion","created":1,"model":"u",
			"choices":[{"index":0,"message":{"role":"assistant","content":"a"},"finish_reason":"never"}],
			"usage":` + usage + `}`,
		"deprecated function_call": `{"id":"c","object":"chat.completion","created":1,"model":"u",
			"choices":[{"index":0,"finish_reason":"function_call",
			"message":{"role":"assistant","content":null,"function_call":{"name":"f","arguments":"{}"}}}],
			"usage":` + usage + `}`,
		"logprobs": `{"id":"c","object":"chat.completion","created":1,"model":"u",
			"choices":[{"index":0,"message":{"role":"assistant","content":"a"},
			"finish_reason":"stop","logprobs":{}}],"usage":` + usage + `}`,
		"audio": `{"id":"c","object":"chat.completion","created":1,"model":"u",
			"choices":[{"index":0,"message":{"role":"assistant","content":"a","audio":{"id":"a"}},
			"finish_reason":"stop"}],"usage":` + usage + `}`,
		"annotations": `{"id":"c","object":"chat.completion","created":1,"model":"u",
			"choices":[{"index":0,"message":{"role":"assistant","content":"a",
			"annotations":[{"type":"url_citation"}]},"finish_reason":"stop"}],"usage":` + usage + `}`,
		"missing usage": `{"id":"c","object":"chat.completion","created":1,"model":"u",
			"choices":[{"index":0,"message":{"role":"assistant","content":"a"},"finish_reason":"stop"}]}`,
		"mismatched total": `{"id":"c","object":"chat.completion","created":1,"model":"u",
			"choices":[{"index":0,"message":{"role":"assistant","content":"a"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":8}}`,
		"cache over prompt": `{"id":"c","object":"chat.completion","created":1,"model":"u",
			"choices":[{"index":0,"message":{"role":"assistant","content":"a"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7,
			"prompt_tokens_details":{"cached_tokens":5}}}`,
		"audio accounting": `{"id":"c","object":"chat.completion","created":1,"model":"u",
			"choices":[{"index":0,"message":{"role":"assistant","content":"a"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7,
			"completion_tokens_details":{"audio_tokens":1}}}`,
		"unknown usage member": `{"id":"c","object":"chat.completion","created":1,"model":"u",
			"choices":[{"index":0,"message":{"role":"assistant","content":"a"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7,"future_tokens":1}}}`,
	}
	for name, raw := range cases {
		_, rerr := TranslateChatJSONToResponses([]byte(raw), "m", DefaultLimits())
		if rerr == nil || rerr.Status != 502 {
			t.Fatalf("%s should fail closed with 502, got %v", name, rerr)
		}
	}
}

func TestChatUsageCounterRelationships(t *testing.T) {
	// cache_write_tokens > prompt must fail.
	raw := `{"id":"c","object":"chat.completion","created":1,"model":"u",
		"choices":[{"index":0,"message":{"role":"assistant","content":"a"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7,
		"prompt_tokens_details":{"cache_write_tokens":5}}}`
	if _, rerr := TranslateChatJSONToResponses([]byte(raw), "m", DefaultLimits()); rerr == nil {
		t.Fatal("cache_write_tokens over prompt must fail")
	}
	// reasoning_tokens > completion must fail.
	raw = `{"id":"c","object":"chat.completion","created":1,"model":"u",
		"choices":[{"index":0,"message":{"role":"assistant","content":"a"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7,
		"completion_tokens_details":{"reasoning_tokens":4}}}`
	if _, rerr := TranslateChatJSONToResponses([]byte(raw), "m", DefaultLimits()); rerr == nil {
		t.Fatal("reasoning_tokens over completion must fail")
	}
	// Exact boundary values pass.
	raw = `{"id":"c","object":"chat.completion","created":1,"model":"u",
		"choices":[{"index":0,"message":{"role":"assistant","content":"a"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7,
		"prompt_tokens_details":{"cached_tokens":4,"cache_write_tokens":4},
		"completion_tokens_details":{"reasoning_tokens":3}}}`
	body := decodeResponsesJSON(t, raw)
	usage := body["usage"].(map[string]any)
	if usage["input_tokens_details"].(map[string]any)["cache_write_tokens"] != float64(4) {
		t.Fatalf("boundary counters lost: %v", usage)
	}
}

func TestChatUsageNoReadPastDone(t *testing.T) {
	// A stream that ends with duplicate usage after DONE must fail, and a
	// valid DONE must not be read past: Translate returns promptly with done.
	var in bytes.Buffer
	in.WriteString("data: " + respChatChunk(`{"content":"Hi"}`, "") + "\n\n")
	in.WriteString("data: " + respChatChunk(`{}`, "stop") + "\n\n")
	in.WriteString("data: " + respChatUsageChunk() + "\n\n")
	in.WriteString("data: [DONE]\n\n")
	in.WriteString("data: " + respChatUsageChunk() + "\n\n")
	var out bytes.Buffer
	tr := &ChatToResponsesSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
	// Extra bytes after the terminal marker belong to the next generation
	// and are left unread: the first generation succeeds.
	rerr := tr.Translate(&in, &out, nil)
	if rerr != nil {
		t.Fatalf("valid DONE should succeed without reading past: %v", rerr)
	}
}
