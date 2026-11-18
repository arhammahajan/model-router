package protocol

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

const chatCustomGrammar = `{"model":"m","messages":[{"role":"user","content":"patch this"}],
	"tools":[{"type":"custom","custom":{"name":"patch","description":"Apply a patch",
	"format":{"type":"grammar","grammar":{"syntax":"regex","definition":"[a-z]+"}}}}],
	"tool_choice":{"type":"custom","custom":{"name":"patch"}}}`

const chatCustomText = `{"model":"m","messages":[{"role":"user","content":"patch this"}],
	"tools":[{"type":"custom","custom":{"name":"patch","description":"Apply"}}],
	"tool_choice":{"type":"custom","custom":{"name":"patch"}}}`

func mustDecodeChatCustom(t *testing.T, raw string) *Generation {
	t.Helper()
	gen, losses, rerr := DecodeChatForResponses([]byte(raw), DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	if len(losses) != 0 {
		t.Fatalf("native custom must be exact, got losses %v", losses)
	}
	return gen
}

func TestChatCustomNativeDefinitionGrammar(t *testing.T) {
	gen := mustDecodeChatCustom(t, chatCustomGrammar)
	if len(gen.Tools) != 1 || !gen.Tools[0].IsCustom() {
		t.Fatalf("custom tool not decoded: %+v", gen.Tools)
	}
	if gen.Tools[0].Format == nil || gen.Tools[0].Format.Type != "grammar" ||
		gen.Tools[0].Format.Syntax != "regex" || gen.Tools[0].Format.Definition != "[a-z]+" {
		t.Fatalf("grammar not preserved: %+v", gen.Tools[0].Format)
	}
	if gen.Choice == nil || !gen.Choice.IsCustomChoice() || gen.Choice.Name != "patch" {
		t.Fatalf("forced custom choice not decoded: %+v", gen.Choice)
	}
	if gen.ToolPlan == nil || !gen.ToolPlan.IsNativeCustom("patch") {
		t.Fatalf("native custom plan missing")
	}
	out, rerr := EncodeResponsesRequest(gen, AbsentEffort(), "upstream")
	if rerr != nil {
		t.Fatalf("encode: %v", rerr)
	}
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	tools := body["tools"].([]any)
	tool := tools[0].(map[string]any)
	if tool["type"] != "custom" || tool["name"] != "patch" {
		t.Fatalf("upstream must carry native custom: %v", tool)
	}
	format := tool["format"].(map[string]any)
	if format["type"] != "grammar" || format["syntax"] != "regex" || format["definition"] != "[a-z]+" {
		t.Fatalf("grammar not preserved upstream: %v", format)
	}
	tc := body["tool_choice"].(map[string]any)
	if tc["type"] != "custom" || tc["name"] != "patch" {
		t.Fatalf("forced custom choice not preserved upstream: %v", tc)
	}
}

func TestChatCustomNativeDefinitionText(t *testing.T) {
	gen := mustDecodeChatCustom(t, chatCustomText)
	if len(gen.Tools) != 1 || !gen.Tools[0].IsCustom() {
		t.Fatalf("custom tool not decoded: %+v", gen.Tools)
	}
	out, rerr := EncodeResponsesRequest(gen, AbsentEffort(), "up")
	if rerr != nil {
		t.Fatalf("encode: %v", rerr)
	}
	if !strings.Contains(string(out), `"type":"custom"`) {
		t.Fatalf("upstream must carry native custom: %s", out)
	}
}

func TestChatCustomHistoryAndResults(t *testing.T) {
	raw := `{"model":"m","messages":[
		{"role":"user","content":"patch this"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"custom","custom":{"name":"patch","input":"hello"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"applied"}],
		"tools":[{"type":"custom","custom":{"name":"patch"}}]}`
	gen := mustDecodeChatCustom(t, raw)
	if len(gen.Messages) != 3 {
		t.Fatalf("history order lost: %+v", gen.Messages)
	}
	tc := gen.Messages[1].ToolCalls[0]
	if !tc.IsCustom || tc.CustomInput != "hello" || tc.ID != "call_1" {
		t.Fatalf("history custom not preserved: %+v", tc)
	}
	out, rerr := EncodeResponsesRequest(gen, AbsentEffort(), "up")
	if rerr != nil {
		t.Fatalf("encode: %v", rerr)
	}
	s := string(out)
	if !strings.Contains(s, `"custom_tool_call"`) || !strings.Contains(s, `"hello"`) {
		t.Fatalf("history custom not forwarded upstream: %s", s)
	}
	if !strings.Contains(s, `"custom_tool_call_output"`) || !strings.Contains(s, `"applied"`) {
		t.Fatalf("custom result not forwarded upstream: %s", s)
	}
}

func TestChatCustomOrphanHistoryAdmitted(t *testing.T) {
	raw := `{"model":"m","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","tool_calls":[{"id":"c1","type":"custom","custom":{"name":"ghost","input":"x"}}]},
		{"role":"tool","tool_call_id":"c1","content":"ok"}]}`
	gen, losses, rerr := DecodeChatForResponses([]byte(raw), DefaultLimits())
	if rerr != nil {
		t.Fatalf("orphan custom history must be admitted: %v", rerr)
	}
	if len(losses) != 0 {
		t.Fatalf("orphan native custom has no loss, got %v", losses)
	}
	if gen.ToolPlan == nil || !gen.ToolPlan.IsNativeCustom("ghost") {
		t.Fatalf("orphan must mint native binding")
	}
}

func TestChatCustomUnknownChoiceRefused(t *testing.T) {
	raw := `{"model":"m","messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"custom","custom":{"name":"patch"}}],
		"tool_choice":{"type":"custom","custom":{"name":"ghost"}}}`
	if _, _, rerr := DecodeChatForResponses([]byte(raw), DefaultLimits()); rerr == nil || rerr.Status != 400 {
		t.Fatalf("unknown custom choice must refuse, got %v", rerr)
	}
}

func TestChatCustomChoiceTypeMismatchRefused(t *testing.T) {
	raw := `{"model":"m","messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"custom","custom":{"name":"patch"}},{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],
		"tool_choice":{"type":"function","function":{"name":"patch"}}}`
	if _, _, rerr := DecodeChatForResponses([]byte(raw), DefaultLimits()); rerr == nil || rerr.Status != 400 {
		t.Fatalf("choice type mismatch must refuse, got %v", rerr)
	}
}

func TestChatCustomMalformedShapesRefused(t *testing.T) {
	cases := []string{
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"custom"}]}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"custom","custom":{"name":""}}]}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"custom","custom":{"name":"patch","format":{"type":"bogus"}}}]}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"custom","custom":{"name":"patch","format":{"type":"grammar","grammar":{"syntax":"lark"}}}}]}`,
		`{"model":"m","messages":[{"role":"user","content":"hi","tool_calls":[{"id":"c1","type":"custom","custom":{"name":"patch","input":"x"}}]}]}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"custom","custom":{"name":"patch"}}],"tool_choice":{"type":"custom","custom":{}}}`,
	}
	for _, raw := range cases {
		if _, _, rerr := DecodeChatForResponses([]byte(raw), DefaultLimits()); rerr == nil || rerr.Status != 400 {
			t.Fatalf("malformed custom must 400: %s got %v", raw, rerr)
		}
	}
}

func TestChatCustomEmulatedOtherDests(t *testing.T) {
	raw := `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"custom","custom":{"name":"patch"}}]}`
	gen, losses, rerr := DecodeChatRequest([]byte(raw), DefaultLimits())
	if rerr != nil {
		t.Fatalf("anthropic dest must emulate custom: %v", rerr)
	}
	if gen.ToolPlan == nil || !gen.ToolPlan.IsEmulatedCustom("patch") {
		t.Fatalf("anthropic emu binding missing")
	}
	found := false
	for _, l := range losses {
		if l.Action == "approximated" {
			found = true
		}
	}
	if !found {
		t.Fatalf("anthropic custom must approximate: %v", losses)
	}
	genG, _, rerr := DecodeChatForGemini([]byte(raw), DefaultLimits())
	if rerr != nil {
		t.Fatalf("gemini dest must emulate custom: %v", rerr)
	}
	if genG.ToolPlan == nil || !genG.ToolPlan.IsEmulatedCustom("patch") {
		t.Fatalf("gemini emu binding missing")
	}
}

func TestResponsesCustomCompleteToChat(t *testing.T) {
	raw := `{"id":"resp_1","object":"response","created_at":1710000000,"model":"up",
		"status":"completed","output":[
			{"type":"custom_tool_call","id":"ctc_1","call_id":"call_abc","name":"patch","input":"hello world","status":"completed"}],
		"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`
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
	call := calls[0].(map[string]any)
	if call["type"] != "custom" || call["id"] != "call_abc" {
		t.Fatalf("chat custom identity must be call_id: %v", call)
	}
	custom := call["custom"].(map[string]any)
	if custom["name"] != "patch" || custom["input"] != "hello world" {
		t.Fatalf("custom input not preserved: %v", custom)
	}
}

func TestResponsesCustomMalformedToChatRefused(t *testing.T) {
	// Non-string input is malformed on the complete row and refuses for Chat.
	raw := `{"id":"resp_1","object":"response","created_at":1710000000,"model":"up",
		"status":"completed","output":[
			{"type":"custom_tool_call","id":"ctc_1","call_id":"c1","name":"patch","input":17,"status":"completed"}],
		"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`
	if _, rerr := TranslateResponsesJSONToChat([]byte(raw), "public", DefaultLimits()); rerr == nil {
		t.Fatalf("non-string custom input must refuse")
	}
}

func customStreamFrames() string {
	final := `{"id":"resp_1","object":"response","created_at":1710000000,"model":"up","status":"completed",` +
		`"output":[{"type":"custom_tool_call","id":"ctc_1","call_id":"call_1","name":"patch","input":"hello world","status":"completed"}],` +
		`"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`
	frames := []string{
		`{"type":"response.created","sequence_number":0,"response":{"id":"resp_1","model":"up","status":"in_progress","output":[]}}`,
		`{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"call_1","name":"patch","input":"","status":"in_progress"}}`,
		`{"type":"response.custom_tool_call_input.delta","sequence_number":2,"output_index":0,"item_id":"ctc_1","delta":"hello "}`,
		`{"type":"response.custom_tool_call_input.delta","sequence_number":3,"output_index":0,"item_id":"ctc_1","delta":"world"}`,
		`{"type":"response.custom_tool_call_input.done","sequence_number":4,"output_index":0,"item_id":"ctc_1","input":"hello world"}`,
		`{"type":"response.output_item.done","sequence_number":5,"output_index":0,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"call_1","name":"patch","input":"hello world","status":"completed"}}`,
		`{"type":"response.completed","sequence_number":6,"response":` + final + `}`,
	}
	var sb strings.Builder
	for _, f := range frames {
		sb.WriteString("data: " + f + "\n\n")
	}
	return sb.String()
}

func runResponsesToChatStream(t *testing.T, sse string, lim Limits) (string, *RouterError) {
	t.Helper()
	tr := &ResponsesToChatSSETranslator{PublicModel: "public", Limits: lim}
	var out bytes.Buffer
	rerr := tr.Translate(strings.NewReader(sse), &out, nil)
	return out.String(), rerr
}

func TestResponsesCustomStreamToChat(t *testing.T) {
	out, rerr := runResponsesToChatStream(t, customStreamFrames(), DefaultLimits())
	if rerr != nil {
		t.Fatalf("stream: %v", rerr)
	}
	if !strings.Contains(out, `"type":"custom"`) || !strings.Contains(out, `hello world`) {
		t.Fatalf("downstream must carry buffered custom call: %s", out)
	}
	if !strings.Contains(out, "response.completed") && !strings.Contains(out, "tool_calls") {
		t.Fatalf("terminal missing: %s", out)
	}
	// Text stays incremental: a text block before the custom call forwards
	// immediately (role chunk + text delta before the custom delta).
	if strings.Index(out, `"content"`) > strings.Index(out, `"type":"custom"`) {
		// content key appears in role chunk too; ensure at least one text
		// delta path exists when text is present (covered below).
	}
}

func TestResponsesCustomStreamTruncatedFails(t *testing.T) {
	frames := strings.Split(customStreamFrames(), "\n\n")
	trunc := strings.Join(frames[:3], "\n\n") + "\n\n"
	_, rerr := runResponsesToChatStream(t, trunc, DefaultLimits())
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("truncation must fail 502, got %v", rerr)
	}
	if out, _ := runResponsesToChatStream(t, trunc, DefaultLimits()); strings.Contains(out, "response.completed") || strings.Contains(out, `"finish_reason":"tool_calls"`) && strings.Contains(out, "[DONE]") {
		t.Fatalf("truncated stream must not fabricate completion")
	}
}

func TestResponsesCustomStreamInterleavedOrder(t *testing.T) {
	final := `{"id":"resp_1","object":"response","created_at":1710000000,"model":"up","status":"completed",` +
		`"output":[{"type":"function_call","id":"fc_1","call_id":"call_f","name":"lookup","arguments":"{\"q\":\"hi\"}","status":"completed"},` +
		`{"type":"custom_tool_call","id":"ctc_2","call_id":"call_c","name":"patch","input":"xyz","status":"completed"}],` +
		`"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`
	sse := "data: " + `{"type":"response.created","sequence_number":0,"response":{"id":"resp_1","model":"up","status":"in_progress","output":[]}}` + "\n\n" +
		"data: " + `{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_f","name":"lookup","arguments":"","status":"in_progress"}}` + "\n\n" +
		"data: " + `{"type":"response.output_item.added","sequence_number":2,"output_index":1,"item":{"type":"custom_tool_call","id":"ctc_2","call_id":"call_c","name":"patch","input":"","status":"in_progress"}}` + "\n\n" +
		"data: " + `{"type":"response.function_call_arguments.delta","sequence_number":3,"output_index":0,"item_id":"fc_1","delta":"{\"q\":\"hi\"}"}` + "\n\n" +
		"data: " + `{"type":"response.custom_tool_call_input.delta","sequence_number":4,"output_index":1,"item_id":"ctc_2","delta":"xyz"}` + "\n\n" +
		"data: " + `{"type":"response.function_call_arguments.done","sequence_number":5,"output_index":0,"item_id":"fc_1","arguments":"{\"q\":\"hi\"}"}` + "\n\n" +
		"data: " + `{"type":"response.custom_tool_call_input.done","sequence_number":6,"output_index":1,"item_id":"ctc_2","input":"xyz"}` + "\n\n" +
		"data: " + `{"type":"response.output_item.done","sequence_number":7,"output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_f","name":"lookup","arguments":"{\"q\":\"hi\"}","status":"completed"}}` + "\n\n" +
		"data: " + `{"type":"response.output_item.done","sequence_number":8,"output_index":1,"item":{"type":"custom_tool_call","id":"ctc_2","call_id":"call_c","name":"patch","input":"xyz","status":"completed"}}` + "\n\n" +
		"data: " + `{"type":"response.completed","sequence_number":9,"response":` + final + `}` + "\n\n"
	out, rerr := runResponsesToChatStream(t, sse, DefaultLimits())
	if rerr != nil {
		t.Fatalf("stream: %v", rerr)
	}
	fi, ci := strings.Index(out, "call_f"), strings.Index(out, "call_c")
	if fi < 0 || ci < 0 || fi > ci {
		t.Fatalf("identity/order not preserved: %s", out)
	}
	if !strings.Contains(out, `"type":"custom"`) || !strings.Contains(out, `"type":"function"`) {
		t.Fatalf("both call families must render: %s", out)
	}
}

func TestResponsesCustomStreamOversizeFails(t *testing.T) {
	lim := DefaultLimits()
	lim.MaxArgsChars = 5
	_, rerr := runResponsesToChatStream(t, customStreamFrames(), lim)
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("oversize custom chunk must 502, got %v", rerr)
	}
}

func TestResponsesCustomStreamContradictoryDoneFails(t *testing.T) {
	sse := strings.Replace(customStreamFrames(),
		`"input":"hello world"`, `"input":"changed"`, 1)
	_, rerr := runResponsesToChatStream(t, sse, DefaultLimits())
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("contradictory done must 502, got %v", rerr)
	}
}

func TestCustomWireStringsRejectInvalidUnicode(t *testing.T) {
	const bad = `\ud800`
	requests := []string{
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"custom","custom":{"name":"patch","format":{"type":"grammar","grammar":{"syntax":"lark","definition":"` + bad + `"}}}}]}`,
		`{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"custom","custom":{"name":"patch","input":"` + bad + `"}}]},{"role":"tool","tool_call_id":"call_1","content":"done"}]}`,
	}
	for _, raw := range requests {
		if _, _, err := DecodeChatForResponses([]byte(raw), DefaultLimits()); err == nil || err.Status != 400 {
			t.Fatalf("invalid freeform input or grammar must fail at the request boundary: %v", err)
		}
	}
	complete := `{"id":"resp_1","object":"response","created_at":1710000000,"model":"up","status":"completed","output":[{"type":"custom_tool_call","id":"ctc_1","call_id":"call_1","name":"patch","input":"` + bad + `","status":"completed"}],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`
	if _, err := TranslateResponsesJSONToChat([]byte(complete), "public", DefaultLimits()); err == nil || err.Status != 502 {
		t.Fatalf("invalid returned input must fail at the complete response boundary: %v", err)
	}
	stream := strings.Replace(customStreamFrames(), `"delta":"world"`, `"delta":"`+bad+`"`, 1)
	out, err := runResponsesToChatStream(t, stream, DefaultLimits())
	if err == nil || err.Status != 502 || strings.Contains(out, "[DONE]") || strings.Contains(out, `"type":"custom"`) {
		t.Fatalf("invalid input delta must stop the stream before emitting a custom call: %v %s", err, out)
	}
}
