package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// Finding 1: full official-shaped JSON with documented echoes must succeed.
func TestReviewFullEnvelope(t *testing.T) {
	raw := `{"id":"resp_1","object":"response","created_at":1746989954,"model":"up","status":"completed",
		"background":false,"instructions":null,"max_output_tokens":null,"parallel_tool_calls":true,
		"previous_response_id":null,"reasoning":{"effort":null,"summary":null},"store":false,
		"temperature":1.0,"text":{"format":{"type":"text"}},"tool_choice":"auto","tools":[],
		"top_p":1.0,"truncation":"disabled","user":null,"metadata":{},"service_tier":"default",
		"error":null,"incomplete_details":null,
		"output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi","annotations":[],"logprobs":[]}]}],
		"usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":0},"output_tokens":5,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":15}}`
	out, rerr := TranslateResponsesJSONToChat([]byte(raw), "public", DefaultLimits())
	if rerr != nil {
		t.Fatalf("full envelope must succeed, got %v", rerr)
	}
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	// Usage with zero reasoning/cached details must validate.
	usage := body["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(10) {
		t.Fatalf("usage wrong: %v", usage)
	}
}

func TestReviewUsageZeroVsNonzero(t *testing.T) {
	base := func(usage string) string {
		return `{"id":"r","object":"response","created_at":1710000000,"model":"up","status":"completed","error":null,"incomplete_details":null,
		"output":[{"id":"m","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi","annotations":[]}]}],
		"usage":` + usage + `}`
	}
	// cache_write_tokens 0 must validate.
	ok := base(`{"input_tokens":5,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0},"output_tokens":2,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":7}`)
	if _, rerr := TranslateResponsesJSONToChat([]byte(ok), "public", DefaultLimits()); rerr != nil {
		t.Fatalf("zero accounting must validate, got %v", rerr)
	}
	// nonzero reasoning must fail, not disappear.
	bad := base(`{"input_tokens":5,"output_tokens":2,"output_tokens_details":{"reasoning_tokens":3},"total_tokens":7}`)
	if _, rerr := TranslateResponsesJSONToChat([]byte(bad), "public", DefaultLimits()); rerr == nil {
		t.Fatalf("nonzero reasoning must fail")
	}
	// nonzero cache_write must fail.
	bad2 := base(`{"input_tokens":5,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":5},"output_tokens":2,"output_tokens_details":{},"total_tokens":7}`)
	if _, rerr := TranslateResponsesJSONToChat([]byte(bad2), "public", DefaultLimits()); rerr == nil {
		t.Fatalf("nonzero cache_write must fail")
	}
	// background true is non-default feature: must fail.
	bg := strings.Replace(ok, `"status":"completed"`, `"status":"completed","background":true`, 1)
	if _, rerr := TranslateResponsesJSONToChat([]byte(bg), "public", DefaultLimits()); rerr == nil {
		t.Fatalf("background=true must fail")
	}
}

// Finding 2: encoder must emit strict:false for ordinary tools.
func TestReviewEncoderStrictFalse(t *testing.T) {
	g, _, rerr := DecodeChatForResponses([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"t","parameters":{"type":"object"}}}]}`), DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	out, rerr := EncodeResponsesRequest(g, AbsentEffort(), "up")
	if rerr != nil {
		t.Fatalf("encode: %v", rerr)
	}
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	tools := body["tools"].([]any)
	tool := tools[0].(map[string]any)
	if strict, ok := tool["strict"]; !ok || strict != false {
		t.Fatalf("ordinary tool must emit strict:false, got %v", tool)
	}
}

// Finding 3: quoted numerics rejected, audio opaque rejected.
func TestReviewQuotedNumericRejected(t *testing.T) {
	for _, raw := range []string{
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"temperature":"0.5"}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"top_p":"1"}`,
	} {
		if _, _, rerr := DecodeChatForResponses([]byte(raw), DefaultLimits()); rerr == nil {
			t.Fatalf("quoted numeric must 400 for %s", raw)
		}
	}
	// audio with arbitrary nested must not be broad accepted as loss.
	if _, _, rerr := DecodeChatForResponses([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"audio":{"foo":{"bar":[1,2,"x"]}}}`), DefaultLimits()); rerr == nil {
		// If it returns losses, that is broad acceptance -> fail.
		t.Fatalf("opaque audio must be rejected, not accepted as loss")
	}
	if _, _, rerr := DecodeChatForResponses([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"prediction":{"unknown":123}}`), DefaultLimits()); rerr == nil {
		t.Fatalf("opaque prediction must be rejected")
	}
}

// Finding 4: streaming lifecycle.
func TestReviewStreamSequenceEnforced(t *testing.T) {
	// Full valid stream with explicit increasing sequence_numbers, but with
	// a repeated sequence_number must fail.
	upstream := "event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_1\",\"status\":\"in_progress\"}}\n\n" +
		"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"sequence_number\":1,\"output_index\":0,\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"in_progress\"}}\n\n" +
		"event: response.content_part.added\ndata: {\"type\":\"response.content_part.added\",\"sequence_number\":2,\"output_index\":0,\"content_index\":0,\"item_id\":\"msg_1\",\"part\":{\"type\":\"output_text\"}}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":3,\"output_index\":0,\"content_index\":0,\"item_id\":\"msg_1\",\"delta\":\"Hello\",\"logprobs\":[]}\n\n" +
		"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"sequence_number\":3,\"output_index\":1,\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"in_progress\",\"call_id\":\"call_abc\",\"name\":\"get_time\",\"arguments\":\"\"}}\n\n" +
		"event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"sequence_number\":4,\"output_index\":1,\"item_id\":\"fc_1\",\"delta\":\"{}\"}}\n\n"
	// Note: malformed tail to force parse; the repeated seq 3 must already fail.
	// Use a valid terminal after to ensure only seq causes failure.
	upstreamValid := "event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_1\",\"status\":\"in_progress\"}}\n\n" +
		"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"sequence_number\":1,\"output_index\":0,\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"in_progress\"}}\n\n" +
		"event: response.content_part.added\ndata: {\"type\":\"response.content_part.added\",\"sequence_number\":2,\"output_index\":0,\"content_index\":0,\"item_id\":\"msg_1\",\"part\":{\"type\":\"output_text\"}}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":3,\"output_index\":0,\"content_index\":0,\"item_id\":\"msg_1\",\"delta\":\"Hello\",\"logprobs\":[]}\n\n" +
		"event: response.output_text.done\ndata: {\"type\":\"response.output_text.done\",\"sequence_number\":4,\"output_index\":0,\"content_index\":0,\"item_id\":\"msg_1\",\"text\":\"Hello\",\"logprobs\":[]}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":4,\"output_index\":0,\"content_index\":0,\"item_id\":\"msg_1\",\"delta\":\"!\",\"logprobs\":[]}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":5,\"response\":{\"id\":\"resp_1\",\"status\":\"completed\"," +
		"\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"Hello!\",\"annotations\":[]}]}]," +
		"\"usage\":{\"input_tokens\":5,\"output_tokens\":2,\"total_tokens\":7}}}\n\n"
	if _, rerr := runR2CStream(t, upstream, false, DefaultLimits()); rerr == nil {
		t.Fatalf("repeated sequence must fail (truncated variant)")
	}
	if _, rerr := runR2CStream(t, upstreamValid, false, DefaultLimits()); rerr == nil {
		t.Fatalf("repeated sequence must fail")
	}
	// Reordered sequence must fail.
	reordered := "event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":10,\"response\":{\"id\":\"resp_1\",\"status\":\"in_progress\"}}\n\n" +
		"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"sequence_number\":5,\"output_index\":0,\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"in_progress\"}}\n\n"
	if _, rerr := runR2CStream(t, reordered, false, DefaultLimits()); rerr == nil {
		t.Fatalf("reordered sequence must fail")
	}
}

func TestReviewStreamTextDoneConsistency(t *testing.T) {
	// Full stream where output_text.done text contradicts streamed deltas.
	upstream := "event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_1\",\"status\":\"in_progress\"}}\n\n" +
		"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"sequence_number\":1,\"output_index\":0,\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"in_progress\"}}\n\n" +
		"event: response.content_part.added\ndata: {\"type\":\"response.content_part.added\",\"sequence_number\":2,\"output_index\":0,\"content_index\":0,\"item_id\":\"msg_1\",\"part\":{\"type\":\"output_text\"}}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":3,\"output_index\":0,\"content_index\":0,\"item_id\":\"msg_1\",\"delta\":\"Hello\",\"logprobs\":[]}\n\n" +
		"event: response.output_text.done\ndata: {\"type\":\"response.output_text.done\",\"sequence_number\":4,\"output_index\":0,\"content_index\":0,\"item_id\":\"msg_1\",\"text\":\"DIFFERENT\",\"logprobs\":[]}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":5,\"response\":{\"id\":\"resp_1\",\"status\":\"completed\"," +
		"\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"Hello\",\"annotations\":[]}]}]," +
		"\"usage\":{\"input_tokens\":5,\"output_tokens\":2,\"total_tokens\":7}}}\n\n"
	if _, rerr := runR2CStream(t, upstream, false, DefaultLimits()); rerr == nil {
		t.Fatalf("inconsistent text done must fail")
	}
	_ = strings.Contains
}

func TestReviewStreamResponseIDMatch(t *testing.T) {
	// terminal response id mismatch must fail.
	upstream := r2cCreated() + r2cItemAddedMsg() + r2cPartAdded() + r2cTextDelta("Hello") +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_OTHER\",\"status\":\"completed\"," +
		"\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"Hello\",\"annotations\":[]}]}]," +
		"\"usage\":{\"input_tokens\":5,\"output_tokens\":2,\"total_tokens\":7}}}\n\n"
	if _, rerr := runR2CStream(t, upstream, false, DefaultLimits()); rerr == nil {
		t.Fatalf("response id mismatch must fail")
	}
}
