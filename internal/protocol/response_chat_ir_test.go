// Chat row runtime tests: one shared decoder, three target encoders.
//
// These tests pin the migration contract: DecodeChatResponse validates
// the Chat source ONCE (same failure for every target), and each
// Encode* applies its own target gate via CheckRepresentable. A
// per-target reparse or a Chat-JSON intermediate would turn the
// shared-validation and cross-product tests below red.
package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func chatIRRaw(finish, msg, usage string) string {
	return `{"id":"chatcmpl-9","object":"chat.completion","created":1,"model":"up",` +
		`"choices":[{"index":0,"message":` + msg + `,"finish_reason":"` + finish + `"}],` +
		`"usage":` + usage + `}`
}

func chatIRUsage(prompt, completion int) string {
	return `{"prompt_tokens":` + g2cItoa(prompt) + `,"completion_tokens":` + g2cItoa(completion) +
		`,"total_tokens":` + g2cItoa(prompt+completion) + `}`
}

func TestChatDeepSeekCacheAccounting(t *testing.T) {
	const base = `{"prompt_tokens":36,"completion_tokens":18,"total_tokens":54,`
	for _, fields := range []string{
		`"prompt_cache_hit_tokens":6,"prompt_cache_miss_tokens":30`,
		`"prompt_cache_miss_tokens":30`,
		`"prompt_tokens_details":{"cached_tokens":6},"prompt_cache_hit_tokens":6,"prompt_cache_miss_tokens":30`,
	} {
		buckets, rerr := parseChatUsageBuckets(json.RawMessage(base + fields + "}"))
		if rerr != nil || buckets.cached == nil || *buckets.cached != 6 || buckets.prompt != 36 || buckets.total != 54 {
			t.Fatalf("cache partition changed authoritative accounting: %+v, %v", buckets, rerr)
		}
	}
	for _, fields := range []string{
		`"prompt_cache_hit_tokens":6,"prompt_cache_miss_tokens":29`,
		`"prompt_tokens_details":{"cached_tokens":5},"prompt_cache_hit_tokens":6`,
		`"prompt_cache_hit_tokens":37`,
		`"prompt_cache_miss_tokens":"30"`,
	} {
		if _, rerr := parseChatUsageBuckets(json.RawMessage(base + fields + "}")); rerr == nil || rerr.Status != 502 {
			t.Fatalf("contradictory or malformed accounting accepted: %v", rerr)
		}
	}
}

func chatIRDecode(t *testing.T, raw string) Response {
	t.Helper()
	resp, rerr := DecodeChatResponse([]byte(raw), DefaultLimits())
	if rerr != nil {
		t.Fatalf("DecodeChatResponse: %v", rerr)
	}
	return resp
}

func chatIREncodeAll(t *testing.T, resp Response) (a, r, g []byte, ae, re, ge *RouterError) {
	t.Helper()
	a, ae = EncodeAnthropicResponse(resp, "public")
	r, re = EncodeResponsesResponse(resp, "public")
	g, ge = EncodeGeminiResponse(resp, "public")
	return a, r, g, ae, re, ge
}

func mustJSON(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	return v
}

// --- Decoder pins source facts once ------------------------------------------

func TestChatIRDecoderFacts(t *testing.T) {
	raw := chatIRRaw("tool_calls",
		`{"role":"assistant","content":"hi","tool_calls":[`+
			`{"id":"call 1:x","type":"function","function":{"name":"get_time","arguments":"{\"n\":9007199254740993}"}},`+
			`{"id":"c2","type":"function","function":{"name":"ping","arguments":""}}]}`,
		`{"prompt_tokens":12,"completion_tokens":7,"total_tokens":19,`+
			`"prompt_tokens_details":{"cached_tokens":4,"cache_write_tokens":1,"text_tokens":0},`+
			`"completion_tokens_details":{"reasoning_tokens":3,"text_tokens":2}}`)
	resp := chatIRDecode(t, raw)
	if resp.Source != DialectChat || string(resp.ID) != "chatcmpl-9" || resp.Model != "up" {
		t.Fatalf("provenance lost: %+v", resp)
	}
	if len(resp.Items) != 3 {
		t.Fatalf("items wrong: %#v", resp.Items)
	}
	if tx, ok := resp.Items[0].(TextItem); !ok || tx.Text != "hi" {
		t.Fatalf("text item wrong: %#v", resp.Items[0])
	}
	for i, want := range []struct{ id, call, name string }{
		{"c0", "call 1:x", "get_time"}, {"c1", "c2", "ping"},
	} {
		tc, ok := resp.Items[i+1].(ToolCallItem)
		if !ok || string(tc.ID) != want.id || string(tc.CallID) != want.call || tc.Name != want.name {
			t.Fatalf("call %d wrong: %#v", i, resp.Items[i+1])
		}
	}
	if string(resp.Items[1].(ToolCallItem).Arguments) != `{"n":9007199254740993}` {
		t.Fatalf("bigint args not exact: %s", resp.Items[1].(ToolCallItem).Arguments)
	}
	if string(resp.Items[2].(ToolCallItem).Arguments) != `{}` {
		t.Fatalf("empty args must be explicit {}: %s", resp.Items[2].(ToolCallItem).Arguments)
	}
	u := resp.Usage
	if *u.Prompt != 12 || *u.Completion != 7 || *u.Total != 19 {
		t.Fatalf("totals wrong: %+v", u)
	}
	if *u.CacheRead != 4 || *u.CacheCreation != 1 || *u.Reasoning != 3 {
		t.Fatalf("buckets wrong: %+v", u)
	}
	if u.TextTokens == nil || *u.TextTokens != 2 {
		t.Fatalf("text bucket wrong: %+v", u)
	}
	if resp.Finish.Kind != FinishToolCalls || resp.Finish.Raw != "tool_calls" {
		t.Fatalf("finish wrong: %+v", resp.Finish)
	}
	if resp.MalformedTools != 0 {
		t.Fatalf("malformed count fabricated: %d", resp.MalformedTools)
	}
	if rerr := u.ValidateAuthoritative(); rerr != nil {
		t.Fatalf("decoded usage not authoritative: %v", rerr)
	}
}

func TestChatIRDecoderMalformedCarried(t *testing.T) {
	raw := chatIRRaw("stop",
		`{"role":"assistant","content":"hi","tool_calls":[`+
			`{"id":"ok","type":"function","function":{"name":"f","arguments":"{}"}},`+
			`{"id":"badname","type":"function","function":{"name":"","arguments":"{}"}},`+
			`{"id":"badargs","type":"function","function":{"name":"g","arguments":"[1]"}}]}`,
		chatIRUsage(4, 3))
	resp := chatIRDecode(t, raw)
	if resp.MalformedTools != 2 {
		t.Fatalf("malformed count = %d, want 2", resp.MalformedTools)
	}
	// Only the valid call survives, with its exact identity and args.
	var calls []ToolCallItem
	for _, it := range resp.Items {
		if c, ok := it.(ToolCallItem); ok {
			calls = append(calls, c)
		}
	}
	if len(calls) != 1 || string(calls[0].CallID) != "ok" {
		t.Fatalf("valid call lost: %#v", resp.Items)
	}
}

// --- Shared validation fails identically for every target ---------------------

func TestChatIRSharedValidation(t *testing.T) {
	msg := `{"role":"assistant","content":"hi"}`
	usage := chatIRUsage(4, 3)
	bad := []string{
		// Unknown members fail closed everywhere (never target-dependent).
		`{"id":"c","object":"chat.completion","created":1,"model":"u",` +
			`"choices":[{"index":0,"message":` + msg + `,"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7,"future":1}}`,
		chatIRRaw("stop", msg, `{"prompt_tokens":4,"completion_tokens":3,"total_tokens":8}`),
		chatIRRaw("stop", msg, `{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7,"prompt_tokens_details":{"cached_tokens":5}}`),
		chatIRRaw("stop", msg, `{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7,"completion_tokens_details":{"reasoning_tokens":4}}`),
		chatIRRaw("stop", msg, `{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7,"completion_tokens_details":{"audio_tokens":1}}`),
		chatIRRaw("quantum", msg, usage),
		chatIRRaw("function_call", msg, usage),
		chatIRRaw("stop", `{"role":"user","content":"hi"}`, usage),
		chatIRRaw("stop", `{"role":"assistant","content":"hi","audio":{}}`, usage),
		chatIRRaw("stop", `{"role":"assistant","content":"hi","function_call":{"name":"f"}}`, usage),
		chatIRRaw("stop", `{"role":"assistant","content":"hi","refusal":null,"logprobs":{}}`, usage),
		chatIRRaw("stop", `{"role":"assistant","content":null,"refusal":"x","tool_calls":[{"id":"d","type":"function","function":{"name":"a","arguments":"{}"}},{"id":"d","type":"function","function":{"name":"b","arguments":"{}"}}]}`, usage),
	}
	for _, raw := range bad {
		var msgs []string
		for _, fn := range []func([]byte, string, Limits) ([]byte, *RouterError){
			TranslateChatJSONToAnthropic, TranslateChatJSONToResponses, TranslateChatJSONToGemini,
		} {
			_, rerr := fn([]byte(raw), "p", DefaultLimits())
			if rerr == nil || rerr.Status != 502 {
				t.Fatalf("expected shared 502 for %.80s, got %v", raw, rerr)
			}
			msgs = append(msgs, rerr.Msg)
		}
		if msgs[0] != msgs[1] || msgs[1] != msgs[2] {
			t.Fatalf("validation not shared for %.80s: %q", raw, msgs)
		}
	}
}

// --- Same source x three targets -----------------------------------------------

func TestChatIRCrossProductZero(t *testing.T) {
	raw := chatIRRaw("stop", `{"role":"assistant","content":"hi"}`, chatIRUsage(2, 1))
	resp := chatIRDecode(t, raw)
	a, r, g, ae, re, ge := chatIREncodeAll(t, resp)
	if ae != nil || re != nil || ge != nil {
		t.Fatalf("zero buckets must validate everywhere: %v %v %v", ae, re, ge)
	}
	ab, rb, gb := mustJSON(t, a), mustJSON(t, r), mustJSON(t, g)
	if ab["model"] != "public" || rb["model"] != "public" || gb["modelVersion"] != "public" {
		t.Fatalf("public model not stamped: %v %v %v", ab["model"], rb["model"], gb["modelVersion"])
	}
	if rb["status"] != "completed" {
		t.Fatalf("responses status: %v", rb)
	}
	au := ab["usage"].(map[string]any)
	if au["input_tokens"] != float64(2) || au["output_tokens"] != float64(1) {
		t.Fatalf("anthropic usage: %v", au)
	}
	if _, ok := au["cache_read_input_tokens"]; ok {
		t.Fatalf("zero cache_read must be omitted: %v", au)
	}
	gu := gb["usageMetadata"].(map[string]any)
	if gu["promptTokenCount"] != float64(2) || gu["candidatesTokenCount"] != float64(1) || gu["totalTokenCount"] != float64(3) {
		t.Fatalf("gemini usage: %v", gu)
	}
	if _, ok := gu["thoughtsTokenCount"]; ok {
		t.Fatalf("thoughts fabricated: %v", gu)
	}
}

func TestChatIRCrossProductAccounting(t *testing.T) {
	mk := func(details string) Response {
		return chatIRDecode(t, chatIRRaw("stop", `{"role":"assistant","content":"hi"}`,
			`{"prompt_tokens":10,"completion_tokens":8,"total_tokens":18,`+details+`}`))
	}
	// Cached share: Anthropic subtracts, others keep verbatim.
	resp := mk(`"prompt_tokens_details":{"cached_tokens":4}`)
	a, r, g, ae, re, ge := chatIREncodeAll(t, resp)
	if ae != nil || re != nil || ge != nil {
		t.Fatalf("cached must validate: %v %v %v", ae, re, ge)
	}
	if got := mustJSON(t, a)["usage"].(map[string]any); got["input_tokens"] != float64(6) || got["cache_read_input_tokens"] != float64(4) {
		t.Fatalf("anthropic cached subtraction: %v", got)
	}
	if got := mustJSON(t, g)["usageMetadata"].(map[string]any); got["cachedContentTokenCount"] != float64(4) {
		t.Fatalf("gemini cached detail: %v", got)
	}
	// Cache-write: Responses carries, Anthropic/Gemini refuse.
	resp = mk(`"prompt_tokens_details":{"cache_write_tokens":2}`)
	_, r, _, ae, re, ge = chatIREncodeAll(t, resp)
	if ae == nil || ge == nil {
		t.Fatalf("cache_write must refuse in anthropic/gemini: %v %v", ae, ge)
	}
	if re != nil {
		t.Fatalf("cache_write must carry in responses: %v", re)
	}
	if det := mustJSON(t, r)["usage"].(map[string]any)["input_tokens_details"].(map[string]any); det["cache_write_tokens"] != float64(2) {
		t.Fatalf("cache_write detail lost: %v", det)
	}
	// Reasoning: Gemini thoughts, Responses native, Anthropic refuses.
	resp = mk(`"completion_tokens_details":{"reasoning_tokens":3}`)
	a, r, g, ae, re, ge = chatIREncodeAll(t, resp)
	if ae == nil {
		t.Fatal("reasoning must refuse in anthropic")
	}
	if re != nil || ge != nil {
		t.Fatalf("reasoning must carry in responses/gemini: %v %v", re, ge)
	}
	if got := mustJSON(t, g)["usageMetadata"].(map[string]any); got["thoughtsTokenCount"] != float64(3) || got["candidatesTokenCount"] != float64(5) {
		t.Fatalf("gemini thoughts mapping: %v", got)
	}
	if got := mustJSON(t, r)["usage"].(map[string]any)["output_tokens_details"].(map[string]any); got["reasoning_tokens"] != float64(3) {
		t.Fatalf("responses reasoning detail: %v", got)
	}
	// Text: harmless in Responses, refused in Anthropic/Gemini.
	resp = mk(`"prompt_tokens_details":{"text_tokens":5},"completion_tokens_details":{"text_tokens":0}`)
	_, r, _, ae, re, ge = chatIREncodeAll(t, resp)
	if ae == nil || ge == nil {
		t.Fatalf("text_tokens must refuse in anthropic/gemini: %v %v", ae, ge)
	}
	if re != nil {
		t.Fatalf("text_tokens must validate in responses: %v", re)
	}
}

func TestChatIRCrossProductContent(t *testing.T) {
	usage := chatIRUsage(4, 3)
	// Refusal: Anthropic folds into text, Responses renders a refusal
	// item, Gemini has no form.
	resp := chatIRDecode(t, chatIRRaw("content_filter",
		`{"role":"assistant","content":null,"refusal":"I cannot help"}`, usage))
	a, r, _, ae, re, ge := chatIREncodeAll(t, resp)
	if ae != nil || re != nil {
		t.Fatalf("refusal must validate in anthropic/responses: %v %v", ae, re)
	}
	if ge == nil {
		t.Fatal("refusal must refuse in gemini")
	}
	ab := mustJSON(t, a)
	if ab["stop_reason"] != "refusal" {
		t.Fatalf("anthropic stop: %v", ab)
	}
	if ab["content"].([]any)[0].(map[string]any)["text"] != "I cannot help" {
		t.Fatalf("anthropic refusal fold: %v", ab["content"])
	}
	rb := mustJSON(t, r)
	if rb["status"] != "incomplete" {
		t.Fatalf("responses status: %v", rb)
	}
	part := rb["output"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if part["type"] != "refusal" || part["refusal"] != "I cannot help" {
		t.Fatalf("responses refusal item: %v", part)
	}
	// Tool finish with no calls: Responses refuses, others render stops.
	resp = chatIRDecode(t, chatIRRaw("tool_calls", `{"role":"assistant","content":null}`, usage))
	a, r, g, ae, re, ge := chatIREncodeAll(t, resp)
	if ae != nil || ge != nil {
		t.Fatalf("call-less tool finish must render in anthropic/gemini: %v %v", ae, ge)
	}
	if re == nil {
		t.Fatal("call-less tool finish must refuse in responses")
	}
	if mustJSON(t, a)["stop_reason"] != "tool_use" {
		t.Fatalf("anthropic stop: %s", a)
	}
	if mustJSON(t, g)["candidates"].([]any)[0].(map[string]any)["finishReason"] != "STOP" {
		t.Fatalf("gemini finish: %s", g)
	}
	// Bare stop with calls coerces to tool_use in Anthropic.
	resp = chatIRDecode(t, chatIRRaw("stop",
		`{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"a","arguments":"{}"}}]}`, usage))
	a, _, _, ae, _, _ = chatIREncodeAll(t, resp)
	if ae != nil {
		t.Fatalf("stop+calls: %v", ae)
	}
	if mustJSON(t, a)["stop_reason"] != "tool_use" {
		t.Fatalf("stop must coerce with calls: %s", a)
	}
	// Malformed calls: Gemini renders MALFORMED_FUNCTION_CALL, others refuse.
	resp = chatIRDecode(t, chatIRRaw("stop",
		`{"role":"assistant","content":null,"tool_calls":[{"id":"x","type":"function","function":{"name":"f","arguments":"[1]"}}]}`,
		usage))
	if resp.MalformedTools != 1 {
		t.Fatalf("malformed count: %d", resp.MalformedTools)
	}
	a, r, g, ae, re, ge = chatIREncodeAll(t, resp)
	if ae == nil || re == nil {
		t.Fatalf("malformed must refuse in anthropic/responses: %v %v", ae, re)
	}
	if ge != nil {
		t.Fatalf("malformed must render in gemini: %v", ge)
	}
	if got := mustJSON(t, g)["candidates"].([]any)[0].(map[string]any)["finishReason"]; got != "MALFORMED_FUNCTION_CALL" {
		t.Fatalf("gemini malformed finish: %v", got)
	}
	// Grammar-invalid names pass Gemini through but refuse elsewhere.
	resp = chatIRDecode(t, chatIRRaw("stop",
		`{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"has space","arguments":"{}"}}]}`,
		usage))
	a, r, g, ae, re, ge = chatIREncodeAll(t, resp)
	if ae == nil || re == nil {
		t.Fatalf("grammar-invalid name must refuse in anthropic/responses: %v %v", ae, ge)
	}
	if ge != nil {
		t.Fatalf("grammar-invalid name must pass gemini: %v", ge)
	}
	fc := mustJSON(t, g)["candidates"].([]any)[0].(map[string]any)["content"].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionCall"].(map[string]any)
	if fc["name"] != "has space" {
		t.Fatalf("gemini name passthrough: %v", fc)
	}
}

func TestChatIRCrossProductIdentities(t *testing.T) {
	odd := "has:colon and space"
	long := strings.Repeat("z", 100)
	msg := `{"role":"assistant","content":null,"tool_calls":[` +
		`{"id":"` + odd + `","type":"function","function":{"name":"f","arguments":"{\"n\":9007199254740993}"}},` +
		`{"id":"` + long + `","type":"function","function":{"name":"g","arguments":""}}]}`
	raw := `{"id":"` + long + `","object":"chat.completion","created":1,"model":"up",` +
		`"choices":[{"index":0,"message":` + msg + `,"finish_reason":"tool_calls"}],` +
		`"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`
	resp := chatIRDecode(t, raw)
	a, r, g, ae, re, ge := chatIREncodeAll(t, resp)
	if ae != nil || re != nil || ge != nil {
		t.Fatalf("opaque IDs must validate everywhere: %v %v %v", ae, re, ge)
	}
	// Anthropic tool_use ids echo the upstream identities verbatim.
	var gotIDs []string
	for _, c := range mustJSON(t, a)["content"].([]any) {
		gotIDs = append(gotIDs, c.(map[string]any)["id"].(string))
	}
	if len(gotIDs) != 2 || gotIDs[0] != odd || gotIDs[1] != long {
		t.Fatalf("anthropic call ids: %v", gotIDs)
	}
	// Gemini responseId echoes the upstream id; functionCall ids echo too.
	gb := mustJSON(t, g)
	if gb["responseId"] != long {
		t.Fatalf("gemini responseId: %v", gb["responseId"])
	}
	parts := gb["candidates"].([]any)[0].(map[string]any)["content"].(map[string]any)["parts"].([]any)
	if parts[0].(map[string]any)["functionCall"].(map[string]any)["id"] != odd {
		t.Fatalf("gemini call id: %v", parts[0])
	}
	if !strings.Contains(string(g), "9007199254740993") {
		t.Fatalf("bigint lost in gemini: %s", g)
	}
	if !strings.Contains(string(a), "9007199254740993") {
		t.Fatalf("bigint lost in anthropic: %s", a)
	}
	// Responses mints distinct fc_/msg_ item ids while call_id restores
	// the upstream identity; seeded ids never collide.
	rb := mustJSON(t, r)
	seen := map[string]bool{}
	for _, item := range rb["output"].([]any) {
		fc := item.(map[string]any)
		id := fc["id"].(string)
		if !strings.HasPrefix(id, "fc_") || seen[id] {
			t.Fatalf("responses item id: %v", fc)
		}
		seen[id] = true
		if callID := fc["call_id"].(string); callID != odd && callID != long {
			t.Fatalf("responses call_id must restore upstream: %v", fc)
		}
		if fc["call_id"] == odd && !strings.Contains(fc["arguments"].(string), "9007199254740993") {
			t.Fatalf("bigint lost in responses: %v", fc)
		}
	}
	if rb["created_at"] == nil {
		t.Fatalf("responses created_at missing: %v", rb)
	}
}

// --- Encoders consult the gate, not just the decoder --------------------------

func TestChatIREncoderGate(t *testing.T) {
	// A validated reasoning Response refuses only where the mapping
	// proves it: the gate fires inside Encode*, independent of decode.
	three := 3
	resp := chatIRDecode(t, chatIRRaw("stop", `{"role":"assistant","content":"x"}`,
		`{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12,`+
			`"completion_tokens_details":{"reasoning_tokens":3}}`))
	if resp.Usage.Reasoning == nil || *resp.Usage.Reasoning != three {
		t.Fatalf("reasoning bucket: %+v", resp.Usage)
	}
	if _, rerr := EncodeAnthropicResponse(resp, "p"); rerr == nil {
		t.Fatal("reasoning must refuse in anthropic encode")
	}
	if _, rerr := EncodeGeminiResponse(resp, "p"); rerr != nil {
		t.Fatalf("reasoning must encode in gemini: %v", rerr)
	}
	// Clearing the bucket clears the refusal: the guard is about the
	// accounting, not the type.
	resp.Usage.Reasoning = nil
	if _, rerr := EncodeAnthropicResponse(resp, "p"); rerr != nil {
		t.Fatalf("cleared reasoning must encode in anthropic: %v", rerr)
	}
}
