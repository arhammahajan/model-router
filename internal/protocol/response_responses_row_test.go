// Responses complete-row cutover: one source decode x three target
// encoders, exercised through the TranslateResponsesJSONTo* compatibility
// wrappers (the server call names). These tests pin that the complete JSON
// row is actually wired DecodeResponsesResponse -> EncodeChatResponse /
// EncodeAnthropicResponse / EncodeGeminiResponse with no pair reparse, no
// Chat JSON intermediate and no map semantic IR. Streams are untouched by
// this row and stay green via their own suites.
package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func r2rowUsage(prompt, completion int, details ...string) string {
	total := prompt + completion
	usage := `{"input_tokens":` + respItoa(prompt) +
		`,"output_tokens":` + respItoa(completion) +
		`,"total_tokens":` + respItoa(total)
	if len(details) > 0 && details[0] != "" {
		usage += `,` + details[0]
	}
	return usage + `}`
}

func r2rowResp(status, output, usage, extra string) string {
	return `{"id":"resp_row","object":"response","created_at":1746989954.0,"model":"up",` +
		`"status":"` + status + `","error":null,"incomplete_details":null,` +
		`"output":` + output + `,"usage":` + usage + extra + `}`
}

func r2rowMustFail(t *testing.T, raw string, fns ...func([]byte, string, Limits) ([]byte, *RouterError)) []*RouterError {
	t.Helper()
	var errs []*RouterError
	for _, fn := range fns {
		_, rerr := fn([]byte(raw), "public", DefaultLimits())
		if rerr == nil || rerr.Status != 502 {
			t.Fatalf("expected 502, got %v", rerr)
		}
		errs = append(errs, rerr)
	}
	return errs
}

// One full-featured source body renders coherently on all three targets:
// order preserved, item id distinct from call id, opaque call ids and
// exact bigint args intact, canonical buckets mapped per destination.
func TestResponsesRowCrossProductFull(t *testing.T) {
	output := `[{"id":"m1","type":"message","role":"assistant","status":"completed",` +
		`"content":[{"type":"output_text","text":"Hi ","annotations":[]},` +
		`{"type":"output_text","text":"there","annotations":[]}]},` +
		`{"id":"fc_1","type":"function_call","status":"completed","call_id":"call 1:x","name":"get_time","arguments":"{\"n\":9007199254740993}"},` +
		`{"id":"fc_2","type":"function_call","status":"completed","call_id":"c2","name":"ping","arguments":""}]`
	// (Reasoning and cache creation travel only to reviewed
	// destinations; they are covered in the gates test, not here.)
	usage := r2rowUsage(12, 7, `"input_tokens_details":{"cached_tokens":4}`)
	raw := r2rowResp("completed", output, usage, "")

	chatOut, rerr := TranslateResponsesJSONToChat([]byte(raw), "public", DefaultLimits())
	if rerr != nil {
		t.Fatalf("chat: %v", rerr)
	}
	var chat map[string]any
	if err := json.Unmarshal(chatOut, &chat); err != nil {
		t.Fatal(err)
	}
	msg := chat["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "Hi there" {
		t.Fatalf("chat text not concatenated in order: %v", msg["content"])
	}
	tcs := msg["tool_calls"].([]any)
	if tcs[0].(map[string]any)["id"] != "call 1:x" || tcs[1].(map[string]any)["id"] != "c2" {
		t.Fatalf("chat must carry call_id identity, not item id: %v", tcs)
	}
	if !strings.Contains(string(chatOut), "9007199254740993") {
		t.Fatalf("chat bigint args not exact: %s", chatOut)
	}
	if ch := chat["choices"].([]any)[0].(map[string]any); ch["finish_reason"] != "tool_calls" {
		t.Fatalf("chat finish: %v", ch["finish_reason"])
	}

	aOut, rerr := TranslateResponsesJSONToAnthropic([]byte(raw), "public", DefaultLimits())
	if rerr != nil {
		t.Fatalf("anthropic: %v", rerr)
	}
	var aMsg map[string]any
	if err := json.Unmarshal(aOut, &aMsg); err != nil {
		t.Fatal(err)
	}
	blocks := aMsg["content"].([]any)
	wantTypes := []string{"text", "text", "tool_use", "tool_use"}
	if len(blocks) != len(wantTypes) {
		t.Fatalf("anthropic order lost: %v", aMsg["content"])
	}
	for i, w := range wantTypes {
		if blocks[i].(map[string]any)["type"] != w {
			t.Fatalf("anthropic block %d: %v", i, blocks[i])
		}
	}
	if blocks[2].(map[string]any)["id"] != "call 1:x" {
		t.Fatalf("anthropic must carry call_id: %v", blocks[2])
	}
	aUsage := aMsg["usage"].(map[string]any)
	// Prompt excludes the cached share: 12-4=8. (Cache creation is
	// covered in the gates test: Chat and Gemini refuse it.)
	if aUsage["input_tokens"] != float64(8) || aUsage["cache_read_input_tokens"] != float64(4) ||
		aUsage["output_tokens"] != float64(7) {
		t.Fatalf("anthropic usage math wrong: %v", aUsage)
	}
	if _, ok := aUsage["cache_creation_input_tokens"]; ok {
		t.Fatalf("no creation share expected: %v", aUsage)
	}

	gOut, rerr := TranslateResponsesJSONToGemini([]byte(raw), "public", DefaultLimits())
	if rerr != nil {
		t.Fatalf("gemini: %v", rerr)
	}
	var gTop map[string]any
	if err := json.Unmarshal(gOut, &gTop); err != nil {
		t.Fatal(err)
	}
	if gTop["responseId"] != "resp_row" || gTop["modelVersion"] != "public" {
		t.Fatalf("gemini identity wrong: %s", gOut)
	}
	parts := gTop["candidates"].([]any)[0].(map[string]any)["content"].(map[string]any)["parts"].([]any)
	if len(parts) != 4 {
		t.Fatalf("gemini order lost: %s", gOut)
	}
	fc := parts[2].(map[string]any)["functionCall"].(map[string]any)
	if fc["id"] != "call 1:x" || fc["name"] != "get_time" {
		t.Fatalf("gemini must carry call_id: %v", fc)
	}
	if !strings.Contains(string(gOut), "9007199254740993") {
		t.Fatalf("gemini bigint args not exact: %s", gOut)
	}
}

// Completed refusal: Chat renders content_filter with the refusal field;
// Anthropic and Gemini refuse (reviewed gate, root 2861323).
func TestResponsesRowCrossProductRefusal(t *testing.T) {
	output := `[{"id":"m","type":"message","role":"assistant","status":"completed",` +
		`"content":[{"type":"refusal","refusal":"no"}]}]`
	raw := r2rowResp("completed", output, r2rowUsage(1, 1), "")
	chatOut, rerr := TranslateResponsesJSONToChat([]byte(raw), "public", DefaultLimits())
	if rerr != nil {
		t.Fatalf("chat refusal: %v", rerr)
	}
	if !strings.Contains(string(chatOut), `"finish_reason":"content_filter"`) ||
		!strings.Contains(string(chatOut), `"refusal":"no"`) {
		t.Fatalf("chat must render filter + refusal: %s", chatOut)
	}
	r2rowMustFail(t, raw, TranslateResponsesJSONToAnthropic, TranslateResponsesJSONToGemini)
	// Refusal + tools refuses on Chat (legacy r2c contract preserved by the
	// shared gate) and on the other two via the refusal gate.
	withTools := `[{"id":"m","type":"message","role":"assistant","status":"completed",` +
		`"content":[{"type":"refusal","refusal":"no"}]},` +
		`{"id":"f1","type":"function_call","status":"completed","call_id":"c1","name":"a","arguments":"{}"}]`
	r2rowMustFail(t, r2rowResp("completed", withTools, r2rowUsage(1, 1), ""),
		TranslateResponsesJSONToChat, TranslateResponsesJSONToAnthropic, TranslateResponsesJSONToGemini)
}

// Incomplete terminals map per destination from one shared Finish:
// length stays length with calls on Chat; Anthropic max_tokens; Gemini
// MAX_TOKENS. Incomplete content_filter renders the filter/refusal
// terminal, never a fake stop.
func TestResponsesRowCrossProductIncomplete(t *testing.T) {
	textOut := `[{"id":"m","type":"message","role":"assistant","status":"incomplete",` +
		`"content":[{"type":"output_text","text":"partial","annotations":[]}]}]`
	usage := r2rowUsage(3, 2)
	for _, tc := range []struct {
		reason             string
		chat, anth, gemini string
	}{
		{"max_output_tokens", "length", "max_tokens", "MAX_TOKENS"},
		{"content_filter", "content_filter", "refusal", "SAFETY"},
	} {
		raw := strings.Replace(r2rowResp("incomplete", textOut, usage, ""),
			`"incomplete_details":null`, `"incomplete_details":{"reason":"`+tc.reason+`"}`, 1)
		chatOut, rerr := TranslateResponsesJSONToChat([]byte(raw), "public", DefaultLimits())
		if rerr != nil {
			t.Fatalf("%s chat: %v", tc.reason, rerr)
		}
		if !strings.Contains(string(chatOut), `"finish_reason":"`+tc.chat+`"`) {
			t.Fatalf("%s chat finish: %s", tc.reason, chatOut)
		}
		aOut, rerr := TranslateResponsesJSONToAnthropic([]byte(raw), "public", DefaultLimits())
		if rerr != nil {
			t.Fatalf("%s anthropic: %v", tc.reason, rerr)
		}
		if !strings.Contains(string(aOut), `"stop_reason":"`+tc.anth+`"`) {
			t.Fatalf("%s anthropic stop: %s", tc.reason, aOut)
		}
		gOut, rerr := TranslateResponsesJSONToGemini([]byte(raw), "public", DefaultLimits())
		if rerr != nil {
			t.Fatalf("%s gemini: %v", tc.reason, rerr)
		}
		if !strings.Contains(string(gOut), `"`+tc.gemini+`"`) {
			t.Fatalf("%s gemini finish: %s", tc.reason, gOut)
		}
	}
}

// Provenance gates: nonzero reasoning and cache creation travel only
// where reviewed; malformed calls gate per destination; text-after-tool
// refuses on Chat while ordered targets preserve it.
func TestResponsesRowCrossProductGates(t *testing.T) {
	textOut := `[{"id":"m","type":"message","role":"assistant","status":"completed",` +
		`"content":[{"type":"output_text","text":"hi","annotations":[]}]}]`
	reasonUsage := r2rowUsage(7, 3, `"output_tokens_details":{"reasoning_tokens":2}`)
	rawReason := r2rowResp("completed", textOut, reasonUsage, "")
	if _, rerr := TranslateResponsesJSONToChat([]byte(rawReason), "public", DefaultLimits()); rerr != nil {
		t.Fatalf("chat reasoning accounting: %v", rerr)
	}
	if _, rerr := TranslateResponsesJSONToGemini([]byte(rawReason), "public", DefaultLimits()); rerr != nil {
		t.Fatalf("gemini reasoning accounting: %v", rerr)
	}
	errs := r2rowMustFail(t, rawReason, TranslateResponsesJSONToAnthropic)
	if !strings.Contains(errs[0].Msg, "reasoning_tokens") {
		t.Fatalf("reasoning gate must name the field: %q", errs[0].Msg)
	}

	writeUsage := r2rowUsage(7, 3, `"input_tokens_details":{"cached_tokens":2,"cache_write_tokens":1}`)
	rawWrite := r2rowResp("completed", textOut, writeUsage, "")
	if _, rerr := TranslateResponsesJSONToAnthropic([]byte(rawWrite), "public", DefaultLimits()); rerr != nil {
		t.Fatalf("anthropic cache creation: %v", rerr)
	}
	r2rowMustFail(t, rawWrite, TranslateResponsesJSONToChat, TranslateResponsesJSONToGemini)

	malOut := `[{"id":"f1","type":"function_call","status":"completed","call_id":"c1","name":"has space","arguments":"{}"}]`
	rawMal := r2rowResp("completed", malOut, r2rowUsage(1, 1), "")
	gOut, rerr := TranslateResponsesJSONToGemini([]byte(rawMal), "public", DefaultLimits())
	if rerr != nil {
		t.Fatalf("gemini malformed: %v", rerr)
	}
	if !strings.Contains(string(gOut), "MALFORMED_FUNCTION_CALL") {
		t.Fatalf("gemini must surface malformed: %s", gOut)
	}
	r2rowMustFail(t, rawMal, TranslateResponsesJSONToChat, TranslateResponsesJSONToAnthropic)

	toolFirst := `[{"id":"f1","type":"function_call","status":"completed","call_id":"c1","name":"a","arguments":"{}"},` +
		`{"id":"m","type":"message","role":"assistant","status":"completed",` +
		`"content":[{"type":"output_text","text":"after","annotations":[]}]}]`
	rawOrder := r2rowResp("completed", toolFirst, r2rowUsage(1, 1), "")
	errs = r2rowMustFail(t, rawOrder, TranslateResponsesJSONToChat)
	if !strings.Contains(errs[0].Msg, "response.items order") {
		t.Fatalf("order gate must name the path: %q", errs[0].Msg)
	}
	for _, fn := range []func([]byte, string, Limits) ([]byte, *RouterError){
		TranslateResponsesJSONToAnthropic, TranslateResponsesJSONToGemini,
	} {
		if _, rerr := fn([]byte(rawOrder), "public", DefaultLimits()); rerr != nil {
			t.Fatalf("ordered target must preserve tool-first: %v", rerr)
		}
	}
}

// Envelope and failure hygiene, identical on all three wrappers: known
// echoes and unknown nulls pass; unknown non-nulls, private state,
// oversize args and unknown statuses fail value-free with 502 and no I/O
// errors (CodeUpstreamBad, never 500).
func TestResponsesRowCrossProductHygiene(t *testing.T) {
	textOut := `[{"id":"m","type":"message","role":"assistant","status":"completed",` +
		`"content":[{"type":"output_text","text":"hi","annotations":[]}]}]`
	echo := `,"background":false,"instructions":"s","max_output_tokens":8,` +
		`"parallel_tool_calls":true,"reasoning":{"effort":"medium","summary":"auto"},` +
		`"store":false,"temperature":0.5,"text":{"format":{"type":"text"}},` +
		`"tool_choice":"auto","tools":[],"top_p":1.0,"truncation":"auto",` +
		`"metadata":{},"service_tier":"auto","completed_at":1746989955.0,"future_null":null`
	all := []func([]byte, string, Limits) ([]byte, *RouterError){
		TranslateResponsesJSONToChat, TranslateResponsesJSONToAnthropic, TranslateResponsesJSONToGemini,
	}
	for _, fn := range all {
		if _, rerr := fn([]byte(r2rowResp("completed", textOut, r2rowUsage(1, 1), echo)), "public", DefaultLimits()); rerr != nil {
			t.Fatalf("documented echoes + unknown null must pass: %v", rerr)
		}
	}
	// Unknown nulls inside usage sub-objects are newer defaults too.
	nullUsage := `{"input_tokens":5,"output_tokens":2,"total_tokens":7,` +
		`"input_tokens_details":{"cached_tokens":null},"output_tokens_details":null,"future_null":null}`
	for _, fn := range all {
		if _, rerr := fn([]byte(r2rowResp("completed", textOut, nullUsage, "")), "public", DefaultLimits()); rerr != nil {
			t.Fatalf("usage-level unknown nulls must pass: %v", rerr)
		}
	}
	privOut := `[{"id":"rs","type":"reasoning","status":"completed"}]`
	nsOut := `[{"id":"f1","type":"function_call","status":"completed","call_id":"c1","name":"a","arguments":"{}","namespace":"x"}]`
	bigArgs := `[{"id":"f1","type":"function_call","status":"completed","call_id":"c1","name":"a",` +
		`"arguments":"{\"k\":\"` + strings.Repeat("0123456789", 200000) + `\"}"}]`
	for _, bad := range []string{
		r2rowResp("completed", textOut, r2rowUsage(1, 1), `,"future_feature":{"x":1}`),
		r2rowResp("completed", textOut, r2rowUsage(1, 1), `,"background":true`),
		r2rowResp("completed", privOut, r2rowUsage(1, 1), ""),
		r2rowResp("completed", nsOut, r2rowUsage(1, 1), ""),
		r2rowResp("completed", bigArgs, r2rowUsage(1, 1), ""),
		r2rowResp("bogus", textOut, r2rowUsage(1, 1), ""),
	} {
		errs := r2rowMustFail(t, bad, all...)
		for _, e := range errs {
			if e.Code != CodeUpstreamBad {
				t.Fatalf("must be upstream failure, got %+v", e)
			}
		}
	}
	// Unknown status/reason diagnostics are value-free: no raw enum leaks.
	errs := r2rowMustFail(t, r2rowResp("bogus", textOut, r2rowUsage(1, 1), ""), all...)
	for _, e := range errs {
		if strings.Contains(e.Msg, "bogus") {
			t.Fatalf("status diagnostic must be value-free: %q", e.Msg)
		}
	}
}
