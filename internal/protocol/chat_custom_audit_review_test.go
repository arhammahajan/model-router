package protocol

// Independent audit of the Chat -> Responses native custom vertical at
// base 15101f7. Focus: HTTP bridge JSON/SSE, semantic tool-family matching
// against the request plan (currently not passed into response translation),
// real Responses event shapes, mixed/parallel ordering, count/bytes,
// unicode/opaque IDs, and old function-only routes.
//
// Source facts vs intended policy: the response path is currently
// direct-wire (upstream family determines downstream family) with no
// ToolPlan parameter on TranslateResponsesJSONToChat or
// ResponsesToChatSSETranslator. The Python ground truth
// (model_router/tools.py ToolPlan.response for Chat ingress) is
// plan-aware: a function call whose name is in native_custom is restored
// to a Chat custom call. The Go Generation.ToolPlan is built but Empty()
// stays true for pure native-custom plans, so WrapDecoder is a no-op and
// the plan is never consulted downstream. Happy-path (upstream returns
// the declared family) works; wrong-family/hallucinated upstream
// diverges from Python.
//
// Primary spec grounding (OpenAI Responses): CustomToolCall id is optional,
// input is a string, format is optional (default unconstrained text).
// Chat custom wire is {"type":"custom","custom":{"name","input"}}.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// F1: mixed function/custom order is normalized in Chat JSON.
// Source order [custom, function] must stay ordered downstream.
func TestAuditMixedOrderJSONToChat(t *testing.T) {
	raw := `{"id":"resp_1","object":"response","created_at":1710000000,"model":"up","status":"completed","output":[` +
		`{"type":"custom_tool_call","id":"ctc_1","call_id":"call_c","name":"patch","input":"xyz","status":"completed"},` +
		`{"type":"function_call","id":"fc_1","call_id":"call_f","name":"lookup","arguments":"{\"q\":\"hi\"}","status":"completed"}],` +
		`"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`
	out, rerr := TranslateResponsesJSONToChat([]byte(raw), "public", DefaultLimits())
	if rerr != nil {
		t.Fatalf("translate: %v", rerr)
	}
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	msg := body["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	calls := msg["tool_calls"].([]any)
	if len(calls) != 2 {
		t.Fatalf("want 2 calls, got %s", out)
	}
	first := calls[0].(map[string]any)
	if first["type"] != "custom" {
		t.Fatalf("order not preserved: custom-first source rendered function-first: %s", out)
	}
}

// F2: mixed order in Chat SSE is reordered (function incremental vs
// custom buffered). Source custom-first must not render function-first.
func TestAuditMixedOrderStreamToChat(t *testing.T) {
	final := `{"id":"resp_1","object":"response","created_at":1710000000,"model":"up","status":"completed",` +
		`"output":[{"type":"custom_tool_call","id":"ctc_2","call_id":"call_c","name":"patch","input":"xyz","status":"completed"},` +
		`{"type":"function_call","id":"fc_1","call_id":"call_f","name":"lookup","arguments":"{\"q\":\"hi\"}","status":"completed"}],` +
		`"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`
	sse := "data: " + `{"type":"response.created","sequence_number":0,"response":{"id":"resp_1","model":"up","status":"in_progress","output":[]}}` + "\n\n" +
		"data: " + `{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"custom_tool_call","id":"ctc_2","call_id":"call_c","name":"patch","input":"","status":"in_progress"}}` + "\n\n" +
		"data: " + `{"type":"response.output_item.added","sequence_number":2,"output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_f","name":"lookup","arguments":"","status":"in_progress"}}` + "\n\n" +
		"data: " + `{"type":"response.custom_tool_call_input.delta","sequence_number":3,"output_index":0,"item_id":"ctc_2","delta":"xyz"}` + "\n\n" +
		"data: " + `{"type":"response.function_call_arguments.delta","sequence_number":4,"output_index":1,"item_id":"fc_1","delta":"{\"q\":\"hi\"}"}` + "\n\n" +
		"data: " + `{"type":"response.custom_tool_call_input.done","sequence_number":5,"output_index":0,"item_id":"ctc_2","input":"xyz"}` + "\n\n" +
		"data: " + `{"type":"response.function_call_arguments.done","sequence_number":6,"output_index":1,"item_id":"fc_1","arguments":"{\"q\":\"hi\"}"}` + "\n\n" +
		"data: " + `{"type":"response.output_item.done","sequence_number":7,"output_index":0,"item":{"type":"custom_tool_call","id":"ctc_2","call_id":"call_c","name":"patch","input":"xyz","status":"completed"}}` + "\n\n" +
		"data: " + `{"type":"response.output_item.done","sequence_number":8,"output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_f","name":"lookup","arguments":"{\"q\":\"hi\"}","status":"completed"}}` + "\n\n" +
		"data: " + `{"type":"response.completed","sequence_number":9,"response":` + final + `}` + "\n\n"
	tr := &ResponsesToChatSSETranslator{PublicModel: "public", Limits: DefaultLimits()}
	var out bytes.Buffer
	if rerr := tr.Translate(strings.NewReader(sse), &out, nil); rerr != nil {
		t.Fatalf("stream: %v", rerr)
	}
	s := out.String()
	fi, ci := strings.Index(s, "call_f"), strings.Index(s, "call_c")
	if fi < 0 || ci < 0 || fi < ci {
		t.Fatalf("stream order not preserved: custom-first source rendered function first (fi=%d ci=%d): %s", fi, ci, s)
	}
}

// A grammar-invalid custom name follows the same malformed classification
// as a function name in complete and streamed responses.
func TestAuditBadNameCompleteVsStream(t *testing.T) {
	raw := `{"id":"resp_1","object":"response","created_at":1710000000,"model":"up","status":"completed","output":[` +
		`{"type":"custom_tool_call","id":"ctc_1","call_id":"c1","name":"has space","input":"x","status":"completed"}],` +
		`"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`
	resp, rerr := DecodeResponsesResponse([]byte(raw), DefaultLimits())
	if rerr != nil {
		t.Fatalf("complete decode: %v", rerr)
	}
	if resp.MalformedTools != 1 {
		t.Fatalf("complete must classify a grammar-invalid custom name as one malformed tool, got %d", resp.MalformedTools)
	}
	final := `{"id":"resp_1","object":"response","created_at":1710000000,"model":"up","status":"completed",` +
		`"output":[{"type":"custom_tool_call","id":"ctc_1","call_id":"c1","name":"has space","input":"x","status":"completed"}],` +
		`"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`
	sse := "data: " + `{"type":"response.created","sequence_number":0,"response":{"id":"resp_1","model":"up","status":"in_progress","output":[]}}` + "\n\n" +
		"data: " + `{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"c1","name":"has space","input":"","status":"in_progress"}}` + "\n\n" +
		"data: " + `{"type":"response.custom_tool_call_input.delta","sequence_number":2,"output_index":0,"item_id":"ctc_1","delta":"x"}` + "\n\n" +
		"data: " + `{"type":"response.custom_tool_call_input.done","sequence_number":3,"output_index":0,"item_id":"ctc_1","input":"x"}` + "\n\n" +
		"data: " + `{"type":"response.output_item.done","sequence_number":4,"output_index":0,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"c1","name":"has space","input":"x","status":"completed"}}` + "\n\n" +
		"data: " + `{"type":"response.completed","sequence_number":5,"response":` + final + `}` + "\n\n"
	tr := &ResponsesToChatSSETranslator{PublicModel: "public", Limits: DefaultLimits()}
	var out strings.Builder
	rerr2 := tr.Translate(strings.NewReader(sse), &out, nil)
	_, rerr3 := TranslateResponsesJSONToChat([]byte(raw), "public", DefaultLimits())
	if rerr2 == nil || rerr2.Status != 502 || rerr3 == nil || rerr3.Status != 502 {
		t.Fatalf("both paths must refuse a malformed tool: complete err=%v stream err=%v", rerr3, rerr2)
	}
	if strings.Contains(out.String(), "[DONE]") || strings.Contains(out.String(), `"type":"custom"`) {
		t.Fatalf("malformed tool must not be emitted or terminate successfully: %s", out.String())
	}
}

// F4: complete custom requires provider id while function allows missing.
// Spec says id is optional for CustomToolCall (and FunctionCall). The code
// comment claims a surrogate is synthesized otherwise, but the code 502s.
func TestAuditCustomIDOptionalParity(t *testing.T) {
	rawCustom := `{"id":"resp_1","object":"response","created_at":1710000000,"model":"up","status":"completed","output":[` +
		`{"type":"custom_tool_call","call_id":"c1","name":"patch","input":"x","status":"completed"}],` +
		`"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`
	rawFunc := `{"id":"resp_1","object":"response","created_at":1710000000,"model":"up","status":"completed","output":[` +
		`{"type":"function_call","call_id":"c1","name":"lookup","arguments":"{}","status":"completed"}],` +
		`"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`
	_, rerrC := DecodeResponsesResponse([]byte(rawCustom), DefaultLimits())
	_, rerrF := DecodeResponsesResponse([]byte(rawFunc), DefaultLimits())
	if (rerrC == nil) != (rerrF == nil) {
		t.Fatalf("id parity: custom err=%v function err=%v (spec: both optional)", rerrC, rerrF)
	}
	if rerrC != nil {
		t.Fatalf("custom missing id should be allowed like function (spec optional): %v", rerrC)
	}
}

// F5: response translation enforces the request plan with a strict
// family-mismatch refusal (not Python facade repair). A function_call
// whose name was declared custom must refuse for Chat, and a
// custom_tool_call whose name was never declared must refuse; ordinary
// unknown functions still pass through.
func TestAuditPlanAwareFamilyRestoration(t *testing.T) {
	genRaw := `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"custom","custom":{"name":"patch"}}]}`
	gen, _, rerr := DecodeChatForResponses([]byte(genRaw), DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode req: %v", rerr)
	}
	if gen.ToolPlan == nil || !gen.ToolPlan.IsNativeCustom("patch") {
		t.Fatalf("native custom plan missing")
	}
	// Upstream returns the wrong family for a declared custom name:
	// strict refusal, never a facade repair to custom.
	up := `{"id":"resp_1","object":"response","created_at":1710000000,"model":"up","status":"completed","output":[` +
		`{"type":"function_call","id":"fc_1","call_id":"c1","name":"patch","arguments":"{}","status":"completed"}],` +
		`"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`
	if _, rerr := TranslateResponsesJSONToChatWithPlan([]byte(up), "public", DefaultLimits(), gen.ToolPlan); rerr == nil {
		t.Fatalf("declared custom returned as function must refuse")
	} else if rerr.Status != 502 {
		t.Fatalf("family mismatch must be 502, got %v", rerr)
	}
	// Hallucinated custom (name never declared) must refuse.
	upCustom := `{"id":"resp_1","object":"response","created_at":1710000000,"model":"up","status":"completed","output":[` +
		`{"type":"custom_tool_call","id":"ctc_1","call_id":"c1","name":"ghost","input":"x","status":"completed"}],` +
		`"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`
	if _, rerr := TranslateResponsesJSONToChatWithPlan([]byte(upCustom), "public", DefaultLimits(), gen.ToolPlan); rerr == nil {
		t.Fatalf("undeclared custom must refuse")
	}
	// Ordinary unknown function still passes through.
	upUnknown := `{"id":"resp_1","object":"response","created_at":1710000000,"model":"up","status":"completed","output":[` +
		`{"type":"function_call","id":"fc_1","call_id":"c1","name":"lookup","arguments":"{}","status":"completed"}],` +
		`"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`
	if _, rerr := TranslateResponsesJSONToChatWithPlan([]byte(upUnknown), "public", DefaultLimits(), gen.ToolPlan); rerr != nil {
		t.Fatalf("ordinary unknown function must pass: %v", rerr)
	}
}

// Old function-only routes now emulate Chat customs (positive checks).
func TestAuditOldRoutesRefuseCustom(t *testing.T) {
	raw := `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"custom","custom":{"name":"patch"}}]}`
	genA, lossesA, rerr := DecodeChatRequest([]byte(raw), DefaultLimits())
	if rerr != nil {
		t.Fatalf("anthropic dest must emulate custom: %v", rerr)
	}
	if genA.ToolPlan == nil || !genA.ToolPlan.IsEmulatedCustom("patch") {
		t.Fatalf("anthropic emu binding missing")
	}
	found := false
	for _, l := range lossesA {
		if l.Action == "approximated" {
			found = true
		}
	}
	if !found {
		t.Fatalf("anthropic custom must approximate: %v", lossesA)
	}
	genG, _, rerr := DecodeChatForGemini([]byte(raw), DefaultLimits())
	if rerr != nil {
		t.Fatalf("gemini dest must emulate custom: %v", rerr)
	}
	if genG.ToolPlan == nil || !genG.ToolPlan.IsEmulatedCustom("patch") {
		t.Fatalf("gemini emu binding missing")
	}
	up := `{"id":"resp_1","object":"response","created_at":1710000000,"model":"up","status":"completed","output":[` +
		`{"type":"custom_tool_call","id":"ctc_1","call_id":"c1","name":"patch","input":"x","status":"completed"}],` +
		`"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`
	resp, rerr := DecodeResponsesResponse([]byte(up), DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	if rerr := CheckRepresentable(resp, DialectAnthropic); rerr == nil {
		t.Fatalf("anthropic dest must refuse custom")
	}
	if rerr := CheckRepresentable(resp, DialectGemini); rerr == nil {
		t.Fatalf("gemini dest must refuse custom")
	}
	if rerr := CheckRepresentable(resp, DialectChat); rerr != nil {
		t.Fatalf("chat must accept custom: %v", rerr)
	}
	if rerr := CheckRepresentable(resp, DialectResponses); rerr != nil {
		t.Fatalf("responses must accept custom: %v", rerr)
	}
}

// Opaque/unicode IDs and bounds are enforced (positive checks).
func TestAuditOpaqueUnicodeBounds(t *testing.T) {
	raw := `{"id":"resp_1","object":"response","created_at":1710000000,"model":"up","status":"completed","output":[` +
		`{"type":"custom_tool_call","id":"ctc 1:has colon","call_id":"call:has colon and space","name":"patch","input":"héllo 🌍","status":"completed"}],` +
		`"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`
	out, rerr := TranslateResponsesJSONToChat([]byte(raw), "public", DefaultLimits())
	if rerr != nil {
		t.Fatalf("opaque/unicode: %v", rerr)
	}
	if !strings.Contains(string(out), "call:has colon and space") || !strings.Contains(string(out), "héllo 🌍") {
		t.Fatalf("opaque/unicode not preserved: %s", out)
	}
	lim := DefaultLimits()
	lim.MaxToolCalls = 1
	raw2 := `{"id":"resp_1","object":"response","created_at":1710000000,"model":"up","status":"completed","output":[` +
		`{"type":"custom_tool_call","id":"ctc_1","call_id":"c1","name":"a","input":"x","status":"completed"},` +
		`{"type":"custom_tool_call","id":"ctc_2","call_id":"c2","name":"b","input":"y","status":"completed"}],` +
		`"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`
	if _, rerr := DecodeResponsesResponse([]byte(raw2), lim); rerr == nil {
		t.Fatalf("MaxToolCalls must bound custom")
	}
	lim2 := DefaultLimits()
	lim2.MaxArgsChars = 5
	raw3 := `{"id":"resp_1","object":"response","created_at":1710000000,"model":"up","status":"completed","output":[` +
		`{"type":"custom_tool_call","id":"ctc_1","call_id":"c1","name":"patch","input":"toolonginput","status":"completed"}],` +
		`"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`
	if _, rerr := DecodeResponsesResponse([]byte(raw3), lim2); rerr == nil {
		t.Fatalf("MaxArgsChars must bound custom input")
	}
}

// Text/custom interleaving gate is consistent (positive check).
func TestAuditTextCustomOrderGate(t *testing.T) {
	bad := `{"id":"resp_1","object":"response","created_at":1710000000,"model":"up","status":"completed",` +
		`"output":[{"type":"custom_tool_call","id":"ctc_1","call_id":"c1","name":"patch","input":"x","status":"completed"},` +
		`{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi"}]}],` +
		`"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`
	if _, rerr := TranslateResponsesJSONToChat([]byte(bad), "public", DefaultLimits()); rerr == nil {
		t.Fatalf("text-after-tool must refuse for Chat")
	}
}
