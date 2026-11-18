package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func mustTranslateR2A(t *testing.T, raw string) map[string]any {
	t.Helper()
	out, rerr := TranslateResponsesJSONToAnthropic([]byte(raw), "public-model", DefaultLimits())
	if rerr != nil {
		t.Fatalf("translate failed: %+v", rerr)
	}
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func mustFailR2A(t *testing.T, raw string) *RouterError {
	t.Helper()
	_, rerr := TranslateResponsesJSONToAnthropic([]byte(raw), "public-model", DefaultLimits())
	if rerr == nil {
		t.Fatalf("expected failure for %s", raw)
	}
	if rerr.Status != 502 || rerr.Code != CodeUpstreamBad {
		t.Fatalf("bad error: %+v", rerr)
	}
	return rerr
}

func r2aResp(usage, output, extra string) string {
	return `{"id":"resp_1","object":"response","created_at":1746989954.0,"model":"up-model",
		"status":"completed","error":null,"incomplete_details":null,
		"output":[` + output + `],"usage":{` + usage + `}` + extra + `}`
}

const r2aUsageFull = `"input_tokens":100,"output_tokens":20,"total_tokens":120,
	"input_tokens_details":{"cached_tokens":30},"output_tokens_details":{"reasoning_tokens":0}`

func TestR2ACacheBucketsCannotOverflowSubsetCheck(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	msg := `{"type":"message","id":"m1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"x"}]}`
	usage := fmt.Sprintf(`"input_tokens":%d,"output_tokens":0,"total_tokens":%d,"input_tokens_details":{"cached_tokens":%d,"cache_write_tokens":%d}`, maxInt, maxInt, maxInt, maxInt)
	mustFailR2A(t, r2aResp(usage, msg, ""))

	usage = fmt.Sprintf(`"input_tokens":%d,"output_tokens":0,"total_tokens":%d,"input_tokens_details":{"cached_tokens":%d,"cache_write_tokens":1}`, maxInt, maxInt, maxInt-1)
	body := mustTranslateR2A(t, r2aResp(usage, msg, ""))
	if body["usage"].(map[string]any)["input_tokens"] != float64(0) {
		t.Fatal("valid boundary cache buckets must leave zero uncached tokens")
	}
}

func TestR2ATextEnvelopeAndUsage(t *testing.T) {
	body := mustTranslateR2A(t, r2aResp(r2aUsageFull,
		`{"type":"message","id":"msg_1","status":"completed","role":"assistant",
			"content":[{"type":"output_text","text":"Hello","annotations":[],"logprobs":[]}]}`, ""))
	if body["type"] != "message" || body["role"] != "assistant" || body["model"] != "public-model" {
		t.Fatalf("bad envelope: %v", body)
	}
	if body["stop_reason"] != "end_turn" {
		t.Fatalf("bad stop: %v", body["stop_reason"])
	}
	content, ok := body["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("bad content: %v", body["content"])
	}
	block := content[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "Hello" {
		t.Fatalf("bad text block: %v", block)
	}
	// Responses input_tokens includes cache; Anthropic excludes the cached
	// buckets. 100 - 30 = 70, with no double counting.
	usage := body["usage"].(map[string]any)
	if usage["input_tokens"] != float64(70) || usage["output_tokens"] != float64(20) {
		t.Fatalf("bad usage totals: %v", usage)
	}
	if usage["cache_read_input_tokens"] != float64(30) {
		t.Fatalf("bad cache_read: %v", usage)
	}
	if id, ok := body["id"].(string); !ok || !strings.HasPrefix(id, "msg_") {
		t.Fatalf("bad id: %v", body["id"])
	}
}

func TestR2AUsageExactness(t *testing.T) {
	msg := `{"type":"message","id":"m1","status":"completed","role":"assistant",
		"content":[{"type":"output_text","text":"x","annotations":[],"logprobs":[]}]}`
	mk := func(u string) string { return r2aResp(u, msg, "") }
	// Mismatched total, cache overrun, fractional, quoted, unknown member.
	mustFailR2A(t, mk(`"input_tokens":100,"output_tokens":20,"total_tokens":999`))
	mustFailR2A(t, mk(`"input_tokens":10,"output_tokens":20,"total_tokens":30,
		"input_tokens_details":{"cached_tokens":11}`))
	mustFailR2A(t, mk(`"input_tokens":10.5,"output_tokens":20,"total_tokens":30`))
	mustFailR2A(t, mk(`"input_tokens":"10","output_tokens":20,"total_tokens":30`))
	mustFailR2A(t, mk(`"input_tokens":10,"output_tokens":20,"total_tokens":30,"bogus":1`))
	mustFailR2A(t, mk(`"input_tokens":10,"output_tokens":20,"total_tokens":30,
		"input_tokens_details":{"cached_tokens":2,"audio_tokens":1}`))
	// Nonzero reasoning has no Anthropic accounting field: refuse.
	rerr := mustFailR2A(t, mk(`"input_tokens":10,"output_tokens":20,"total_tokens":30,
		"output_tokens_details":{"reasoning_tokens":4}`))
	if !strings.Contains(rerr.Msg, "reasoning_tokens") {
		t.Fatalf("reasoning refusal must name the field: %v", rerr)
	}
	// Zero reasoning is exact accounting, not a loss.
	body := mustTranslateR2A(t, mk(`"input_tokens":10,"output_tokens":20,"total_tokens":30,
		"output_tokens_details":{"reasoning_tokens":0}`))
	if body["usage"].(map[string]any)["output_tokens"] != float64(20) {
		t.Fatalf("bad usage: %v", body["usage"])
	}
	// Both cached buckets are excluded from the Anthropic input total.
	body = mustTranslateR2A(t, mk(`"input_tokens":100,"output_tokens":20,"total_tokens":120,
		"input_tokens_details":{"cached_tokens":30,"cache_write_tokens":10}`))
	usage := body["usage"].(map[string]any)
	if usage["input_tokens"] != float64(60) || usage["cache_read_input_tokens"] != float64(30) ||
		usage["cache_creation_input_tokens"] != float64(10) {
		t.Fatalf("bad cache math: %v", usage)
	}
	// Cached buckets cannot exceed the input total together.
	mustFailR2A(t, mk(`"input_tokens":10,"output_tokens":20,"total_tokens":30,
		"input_tokens_details":{"cached_tokens":8,"cache_write_tokens":8}`))
	// Unknown null members are newer defaults, accepted.
	mustTranslateR2A(t, r2aResp(`"input_tokens":10,"output_tokens":20,"total_tokens":30,"future_field":null`, msg, ""))
}

func TestR2ATerminalMapping(t *testing.T) {
	text := `{"type":"message","id":"m1","status":"completed","role":"assistant",
		"content":[{"type":"output_text","text":"done","annotations":[],"logprobs":[]}]}`
	toolOut := `{"type":"function_call","id":"fc_9","call_id":"call_9","name":"get_time",
		"arguments":"{\"tz\":\"UTC\"}","status":"completed"}`
	// Completed text -> end_turn; completed calls -> tool_use.
	if b := mustTranslateR2A(t, r2aResp(r2aUsageFull, text, "")); b["stop_reason"] != "end_turn" {
		t.Fatalf("bad stop: %v", b["stop_reason"])
	}
	body := mustTranslateR2A(t, r2aResp(r2aUsageFull, text+","+toolOut, ""))
	if body["stop_reason"] != "tool_use" {
		t.Fatalf("bad stop: %v", body["stop_reason"])
	}
	content := body["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("bad content order: %v", body["content"])
	}
	tool := content[1].(map[string]any)
	// call_id is the stable Anthropic identity; the fc_ item id is not.
	if tool["type"] != "tool_use" || tool["id"] != "call_9" || tool["name"] != "get_time" {
		t.Fatalf("bad tool_use: %v", tool)
	}
	var input map[string]any
	rawInput, _ := json.Marshal(tool["input"])
	if err := json.Unmarshal(rawInput, &input); err != nil || input["tz"] != "UTC" {
		t.Fatalf("bad input object: %v", tool["input"])
	}
	// Tool-first order is preserved exactly.
	body2 := mustTranslateR2A(t, r2aResp(r2aUsageFull, toolOut+","+text, ""))
	c2 := body2["content"].([]any)
	if c2[0].(map[string]any)["type"] != "tool_use" || c2[1].(map[string]any)["type"] != "text" {
		t.Fatalf("order not preserved: %v", body2["content"])
	}
	// Incomplete max_output_tokens -> max_tokens.
	inc := `{"id":"resp_1","object":"response","created_at":1746989954.0,"model":"up-model",
		"status":"incomplete","error":null,"incomplete_details":{"reason":"max_output_tokens"},
		"output":[` + text + `],"usage":{` + r2aUsageFull + `}}`
	if b := mustTranslateR2A(t, inc); b["stop_reason"] != "max_tokens" {
		t.Fatalf("bad stop: %v", b["stop_reason"])
	}
	// Failed never fakes completion; unknown incomplete reasons have no
	// mapping.
	failed := `{"id":"resp_1","object":"response","created_at":1746989954.0,"model":"up-model",
		"status":"failed","error":{"code":"x","message":"y"},"incomplete_details":null,
		"output":[` + text + `],"usage":{` + r2aUsageFull + `}}`
	mustFailR2A(t, failed)
	// Incomplete content_filter renders the Anthropic refusal terminal.
	// The legacy pair row failed this; the unified row maps it exactly
	// like the Chat row does (chat_to_anthropic_response.go maps
	// content_filter to refusal, and EncodeAnthropicResponse maps the
	// canonical ContentFilter finish to refusal). Deliberate cutover
	// change: refusal, not a 502.
	filtInc := `{"id":"resp_1","object":"response","created_at":1746989954.0,"model":"up-model",
		"status":"incomplete","error":null,"incomplete_details":{"reason":"content_filter"},
		"output":[` + text + `],"usage":{` + r2aUsageFull + `}}`
	if b := mustTranslateR2A(t, filtInc); b["stop_reason"] != "refusal" {
		t.Fatalf("bad stop: %v", b["stop_reason"])
	}
	// Empty completion (a message with no parts) renders end_turn with one
	// empty text block. The legacy pair row failed text-less call-less
	// output; the unified row allows zero-item output as Stop (the
	// Chat and Gemini rows already rendered stop there). Deliberate
	// cutover change: end_turn, not a 502. A missing/null output array
	// still fails at source decode.
	empty := mustTranslateR2A(t, r2aResp(r2aUsageFull, text[:0]+`{"type":"message","id":"m1","status":"completed",
		"role":"assistant","content":[]}`, ""))
	if empty["stop_reason"] != "end_turn" {
		t.Fatalf("bad stop: %v", empty["stop_reason"])
	}
	emptyContent := empty["content"].([]any)
	if len(emptyContent) != 1 || emptyContent[0].(map[string]any)["type"] != "text" ||
		emptyContent[0].(map[string]any)["text"] != "" {
		t.Fatalf("empty completion must render one empty text block: %v", empty["content"])
	}
}

func TestR2AEmptyArgsAndIdentities(t *testing.T) {
	body := mustTranslateR2A(t, r2aResp(r2aUsageFull,
		`{"type":"function_call","id":"fc_1","call_id":"c1","name":"noop","arguments":"","status":"completed"}`, ""))
	tool := body["content"].([]any)[0].(map[string]any)
	if tool["id"] != "c1" {
		t.Fatalf("empty-args tool lost identity: %v", tool)
	}
	rawInput, _ := json.Marshal(tool["input"])
	if string(rawInput) != "{}" {
		t.Fatalf("empty args must be {}: %s", rawInput)
	}
	dup := r2aResp(r2aUsageFull,
		`{"type":"function_call","id":"fc_1","call_id":"c1","name":"a","arguments":"{}","status":"completed"},
		 {"type":"function_call","id":"fc_2","call_id":"c1","name":"a","arguments":"{}","status":"completed"}`, "")
	mustFailR2A(t, dup)
}

func TestR2AUnrepresentableOutput(t *testing.T) {
	mk := func(item string) string { return r2aResp(r2aUsageFull, item, "") }
	text := func(extra string) string {
		return `{"type":"message","id":"m1","status":"completed","role":"assistant","content":[` + extra + `]}`
	}
	// Refusal has no Anthropic block: fail, never drop.
	mustFailR2A(t, mk(text(`{"type":"refusal","refusal":"no"}`)))
	// Reasoning, media and service items fail closed.
	for _, item := range []string{
		`{"type":"reasoning","id":"r1","summary":[]}`,
		`{"type":"web_search_call","id":"w1","status":"completed"}`,
		`{"type":"file_search_call","id":"f1","status":"completed"}`,
		`{"type":"computer_call","id":"c1","status":"completed"}`,
		`{"type":"mcp_call","id":"mc1","name":"s","arguments":"{}"}`,
		`{"type":"code_interpreter_call","id":"ci1","status":"completed"}`,
		`{"type":"image_generation_call","id":"i1","status":"completed"}`,
		`{"type":"custom_tool_call","id":"ct1","name":"x","input":""}`,
		`{"type":"unknown_future_thing","id":"u1"}`,
	} {
		mustFailR2A(t, mk(item))
	}
	// Populated annotations/logprobs fail; empty arrays are allowed.
	mustFailR2A(t, mk(text(`{"type":"output_text","text":"x","annotations":[{"type":"c"}],"logprobs":[]}`)))
	mustFailR2A(t, mk(text(`{"type":"output_text","text":"x","annotations":[],"logprobs":[1]}`)))
	// Non-assistant roles and bad content fail.
	mustFailR2A(t, mk(text(`{"type":"output_text","text":"x","annotations":[],"logprobs":[]}`)[:0]+
		`{"type":"message","id":"m1","status":"completed","role":"user","content":[]}`))
	mustFailR2A(t, mk(`{"type":"function_call","id":"fc_1","call_id":"c1","name":"a",
		"arguments":"[1]","status":"completed"}`))
	mustFailR2A(t, mk(`{"type":"function_call","id":"fc_1","call_id":"c1","name":"a",
		"arguments":"{}","status":"completed","namespace":"ns"}`))
}

func TestR2AEnvelopeEchoes(t *testing.T) {
	msg := `{"type":"message","id":"m1","status":"completed","role":"assistant",
		"content":[{"type":"output_text","text":"x","annotations":[],"logprobs":[]}]}`
	echo := `,"store":false,"tools":[],"tool_choice":"auto","temperature":0.5,"top_p":1,
		"text":{"format":{"type":"text"}},"truncation":"auto","parallel_tool_calls":true,
		"instructions":"s","max_output_tokens":8,"metadata":{},"service_tier":"auto",
		"previous_response_id":"resp_old","reasoning":{"effort":"medium"},"completed_at":1746989955.0,
		"user":"u1","future_null":null`
	mustTranslateR2A(t, r2aResp(r2aUsageFull, msg, echo))
	// Non-default echoes fail as unrepresentable.
	for _, e := range []string{
		`,"background":true`,
		`,"text":{"format":{"type":"json_schema"}}`,
		`,"truncation":"weird"`,
		`,"future_feature":{"x":1}`,
		`,"temperature":"high"`,
		`,"max_output_tokens":"lots"`,
	} {
		mustFailR2A(t, r2aResp(r2aUsageFull, msg, e))
	}
}
