package protocol

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// Responses ingress custom emulation: definitions, choices, history,
// response/stream restoration through the shared typed ToolPlan.
const respCustomTextReq = `{"model":"m-resp","input":"Fix hello",
	"tools":[{"type":"custom","name":"patch","description":"Apply a patch"}],
	"tool_choice":{"type":"custom","name":"patch"}}`

const respCustomGrammarReq = `{"model":"m-resp","input":"Fix hello",
	"tools":[{"type":"custom","name":"patch","description":"Apply","format":{"type":"grammar","syntax":"regex","definition":"[a-z]+"}}],
	"tool_choice":{"type":"custom","name":"patch"}}`

func mustDecodeResponsesCustom(t *testing.T, raw string, dest responsesDestination) (*Generation, []Loss) {
	t.Helper()
	gen, losses, rerr := decodeResponsesSource([]byte(raw), DefaultLimits(), dest)
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	return gen, losses
}

func TestResponsesCustomDefinitionsEmulated(t *testing.T) {
	for _, dest := range []responsesDestination{responsesDestChat, responsesDestAnthropic, responsesDestGemini} {
		gen, losses := mustDecodeResponsesCustom(t, respCustomTextReq, dest)
		if len(gen.Tools) != 1 || !gen.Tools[0].IsCustom() {
			t.Fatalf("dest %d custom not decoded: %+v", dest, gen.Tools)
		}
		if gen.Choice == nil || !gen.Choice.IsCustomChoice() || gen.Choice.Name != "patch" {
			t.Fatalf("dest %d custom choice not decoded: %+v", dest, gen.Choice)
		}
		if gen.ToolPlan == nil || !gen.ToolPlan.IsEmulatedCustom("patch") {
			t.Fatalf("dest %d emulation binding missing", dest)
		}
		found := false
		for _, l := range losses {
			if l.Action == "approximated" {
				found = true
			}
			if strings.Contains(l.Reason, "grammar") && strings.Contains(strings.ToLower(l.Reason), "enforce") && strings.Contains(l.Reason, "guarantee") {
				t.Fatalf("grammar loss must not claim enforcement: %v", l)
			}
		}
		if !found {
			t.Fatalf("dest %d custom must report approximation even for text: %v", dest, losses)
		}
		if _, rerr := EnforcePolicy(losses, false); rerr == nil || rerr.Code != CodeTranslationLoss {
			t.Fatalf("dest %d strict must refuse pre-I/O: %v", dest, rerr)
		}
		kept, rerr := EnforcePolicy(losses, true)
		if rerr != nil {
			t.Fatalf("dest %d lossy must allow: %v", dest, rerr)
		}
		hdr := EncodeLosses(kept)
		if !strings.Contains(hdr, "approximated") {
			t.Fatalf("lossy header must carry approximation: %q", hdr)
		}
		// Upstream bodies carry the single bounded emulation shape.
		var out []byte
		switch dest {
		case responsesDestChat:
			out, rerr = EncodeChatRequest(gen, AbsentEffort(), "up")
		case responsesDestAnthropic:
			out, rerr = EncodeAnthropicRequest(gen, "up")
		default:
			out, rerr = EncodeGeminiRequest(gen)
		}
		if rerr != nil {
			t.Fatalf("dest %d encode: %v", dest, rerr)
		}
		s := string(out)
		if !strings.Contains(s, `"content"`) || !strings.Contains(s, `"patch"`) {
			t.Fatalf("dest %d upstream must carry emulated function: %s", dest, s)
		}
		if strings.Contains(s, `"custom_tool_call"`) || strings.Contains(s, `"type":"custom"`) {
			t.Fatalf("dest %d upstream must not carry native custom: %s", dest, s)
		}
	}
}

func TestResponsesCustomGrammarDescription(t *testing.T) {
	gen, losses := mustDecodeResponsesCustom(t, respCustomGrammarReq, responsesDestChat)
	if len(losses) == 0 {
		t.Fatalf("grammar must still approximate: %v", losses)
	}
	if gen.Tools[0].Format == nil || gen.Tools[0].Format.Type != "grammar" {
		t.Fatalf("grammar not preserved: %+v", gen.Tools[0].Format)
	}
	out, rerr := EncodeChatRequest(gen, AbsentEffort(), "up")
	if rerr != nil {
		t.Fatalf("encode: %v", rerr)
	}
	s := string(out)
	if !strings.Contains(s, "Return content following this regex grammar:") || !strings.Contains(s, "[a-z]+") {
		t.Fatalf("grammar must travel as instructions text: %s", s)
	}
	for _, l := range losses {
		if strings.Contains(strings.ToLower(l.Reason), "enforce") && strings.Contains(l.Reason, "grammar") {
			t.Fatalf("loss must never claim native enforcement: %v", l)
		}
	}
}

func TestResponsesCustomExactInputUnicode(t *testing.T) {
	input := "héllo\nworld ✓ \U0001F600 \"quoted\""
	rawInput, _ := json.Marshal(input)
	raw := `{"model":"m","input":[
		{"type":"custom_tool_call","call_id":"call_1","name":"patch","input":` + string(rawInput) + `},
		{"type":"custom_tool_call_output","call_id":"call_1","output":"done"}],
		"tools":[{"type":"custom","name":"patch"}]}`
	for _, dest := range []responsesDestination{responsesDestChat, responsesDestAnthropic, responsesDestGemini} {
		gen, losses := mustDecodeResponsesCustom(t, raw, dest)
		if len(gen.Messages) != 2 {
			t.Fatalf("dest %d history order lost", dest)
		}
		tc := gen.Messages[0].ToolCalls[0]
		if !tc.IsCustom || tc.CustomInput != input || tc.ID != "call_1" {
			t.Fatalf("dest %d exact input not preserved: %+v", dest, tc)
		}
		if len(losses) == 0 {
			t.Fatalf("dest %d history orphan/custom must approximate", dest)
		}
		var out []byte
		var rerr *RouterError
		switch dest {
		case responsesDestChat:
			out, rerr = EncodeChatRequest(gen, AbsentEffort(), "up")
		case responsesDestAnthropic:
			out, rerr = EncodeAnthropicRequest(gen, "up")
		default:
			out, rerr = EncodeGeminiRequest(gen)
		}
		if rerr != nil {
			t.Fatalf("dest %d encode: %v", dest, rerr)
		}
		// The emulated upstream object must decode back to the exact string.
		var body map[string]any
		_ = json.Unmarshal(out, &body)
		found := false
		for _, needle := range []string{`héllo`, `world`, `✓`} {
			if strings.Contains(string(out), needle) {
				found = true
			}
		}
		if !found {
			t.Fatalf("dest %d unicode lost: %s", dest, out)
		}
		_ = body
	}
}

func respCustomUpstreamResponses(alias, args, call string) (string, string, string) {
	chat := `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"up",` +
		`"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[` +
		`{"id":"` + call + `","type":"function","function":{"name":"` + alias + `","arguments":` + respCustomQuote(args) + `}}]},` +
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`
	anth := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x",` +
		`"content":[{"type":"tool_use","id":"` + call + `","name":"` + alias + `","input":` + args + `}],` +
		`"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":3}}`
	gem := `{"candidates":[{"content":{"role":"model","parts":[` +
		`{"functionCall":{"name":"` + alias + `","args":` + args + `,"id":"` + call + `"}}]},` +
		`"finishReason":"STOP","index":0}],"modelVersion":"gemini-x",` +
		`"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3,"totalTokenCount":8}}`
	return chat, anth, gem
}

func respCustomQuote(s string) string {
	q, _ := json.Marshal(s)
	return string(q)
}

func TestResponsesCustomJSONRestoredAllBackends(t *testing.T) {
	alias := "patch"
	args, _ := json.Marshal(map[string]string{"content": "hello ✓\nworld"})
	chat, anth, gem := respCustomUpstreamResponses(alias, string(args), "call_7")
	for _, dest := range []responsesDestination{responsesDestChat, responsesDestAnthropic, responsesDestGemini} {
		gen, _ := mustDecodeResponsesCustom(t, respCustomTextReq, dest)
		var out []byte
		var rerr *RouterError
		switch dest {
		case responsesDestChat:
			out, rerr = TranslateChatJSONToResponsesWithPlan([]byte(chat), "public", DefaultLimits(), gen.ToolPlan)
		case responsesDestAnthropic:
			out, rerr = TranslateAnthropicJSONToResponsesWithPlan([]byte(anth), "public", DefaultLimits(), gen.ToolPlan)
		default:
			out, rerr = TranslateGeminiJSONToResponsesWithPlan([]byte(gem), "public", DefaultLimits(), gen.ToolPlan)
		}
		if rerr != nil {
			t.Fatalf("dest %d restore: %v", dest, rerr)
		}
		var body map[string]any
		if err := json.Unmarshal(out, &body); err != nil {
			t.Fatal(err)
		}
		output := body["output"].([]any)
		if len(output) != 1 {
			t.Fatalf("dest %d want one restored call: %v", dest, output)
		}
		call := output[0].(map[string]any)
		if call["type"] != "custom_tool_call" || call["name"] != "patch" || call["call_id"] != "call_7" {
			t.Fatalf("dest %d identity not restored: %v", dest, call)
		}
		if call["input"] != "hello ✓\nworld" {
			t.Fatalf("dest %d exact input not restored: %v", dest, call)
		}
		if _, hasNS := call["namespace"]; hasNS {
			t.Fatalf("dest %d plain custom must not mint namespace: %v", dest, call)
		}
	}
	// Nil plan stays raw.
	rawOut, rerr := TranslateChatJSONToResponsesWithPlan([]byte(chat), "public", DefaultLimits(), nil)
	if rerr != nil {
		t.Fatalf("nil plan: %v", rerr)
	}
	if !strings.Contains(string(rawOut), `"function_call"`) {
		t.Fatalf("nil plan must stay raw: %s", rawOut)
	}
}

func TestResponsesCustomMalformedWrapper(t *testing.T) {
	gen, _ := mustDecodeResponsesCustom(t, respCustomTextReq, responsesDestChat)
	for name, args := range map[string]string{
		"extra key":      `{"content":"hi","extra":1}`,
		"duplicate key":  `{"content":"first","\u0063ontent":"second"}`,
		"non-string":     `{"content":17}`,
		"empty object":   `{}`,
		"array":          `[1]`,
		"scalar":         `"hi"`,
		"missing key":    `{"text":"hi"}`,
		"null content":   `{"content":null}`,
		"invalid json":   `{oops}`,
		"empty envelope": ``,
	} {
		raw := `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"up",` +
			`"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[` +
			`{"id":"c1","type":"function","function":{"name":"patch","arguments":` + respCustomQuote(args) + `}}]},` +
			`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
		if _, rerr := TranslateChatJSONToResponsesWithPlan([]byte(raw), "public", DefaultLimits(), gen.ToolPlan); rerr == nil || rerr.Status != 502 {
			t.Fatalf("%s must refuse 502, got %v", name, rerr)
		}
	}
	// Malformed definitions are 400 pre-I/O in both modes.
	for _, raw := range []string{
		`{"model":"m","input":"hi","tools":[{"type":"custom"}]}`,
		`{"model":"m","input":"hi","tools":[{"type":"custom","name":""}]}`,
		`{"model":"m","input":"hi","tools":[{"type":"custom","name":"patch","format":{"type":"bogus"}}]}`,
		`{"model":"m","input":"hi","tools":[{"type":"custom","name":"patch","format":{"type":"grammar","syntax":"lark"}}]}`,
		`{"model":"m","input":"hi","tools":[{"type":"custom","name":"patch","parameters":{"type":"object"}}]}`,
		`{"model":"m","input":"hi","tools":[{"type":"custom","name":"patch"}],"tool_choice":{"type":"custom","name":"ghost"}}}`,
		`{"model":"m","input":"hi","tools":[{"type":"custom","name":"patch"}],"tool_choice":{"type":"function","name":"patch"}}}`,
		`{"model":"m","input":[{"type":"custom_tool_call","call_id":"c1","name":"a","input":17}],"tools":[{"type":"custom","name":"a"}]}`,
		`{"model":"m","input":[{"type":"custom_tool_call","call_id":"c1","name":"a","input":"x"},{"type":"function_call_output","call_id":"c1","output":"y"}],"tools":[{"type":"custom","name":"a"}]}`,
	} {
		if _, _, rerr := decodeResponsesSource([]byte(raw), DefaultLimits(), responsesDestChat); rerr == nil || rerr.Status != 400 {
			t.Fatalf("malformed must 400: %s got %v", raw, rerr)
		}
	}
	// Mismatched upstream family: native custom returned for emulated plan.
	nativeCustom := `{"id":"resp_1","object":"response","created_at":1710000000,"model":"up","status":"completed",` +
		`"output":[{"type":"custom_tool_call","id":"ctc_1","call_id":"c1","name":"patch","input":"hi","status":"completed"}],` +
		`"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
	resp, rerr := DecodeResponsesResponse([]byte(nativeCustom), DefaultLimits())
	if rerr != nil {
		t.Fatalf("native custom must decode standalone: %v", rerr)
	}
	if _, rerr := gen.ToolPlan.RestoreResponse(resp, DefaultLimits()); rerr == nil || rerr.Status != 502 {
		t.Fatalf("mismatched native custom must refuse, got %v", rerr)
	}
}

func TestResponsesCustomStreamRestoreOrderTruncation(t *testing.T) {
	gen, _ := mustDecodeResponsesCustom(t, respCustomTextReq, responsesDestChat)
	args0, _ := json.Marshal(map[string]string{"content": "first"})
	args1 := `{"q":"second"}`
	chunks := []string{
		nsChatChunk(`{"tool_calls":[{"index":0,"id":"call_c","type":"function","function":{"name":"patch","arguments":`+respCustomQuote(string(args0[:5]))+`}}]}`, ""),
		nsChatChunk(`{"tool_calls":[{"index":1,"id":"call_f","type":"function","function":{"name":"ghost","arguments":"{}"}}]}`, ""),
		nsChatChunk(`{"tool_calls":[{"index":0,"function":{"arguments":`+respCustomQuote(string(args0[5:]))+`}}]}`, ""),
		nsChatChunk(`{}`, "tool_calls"),
		nsChatUsage(),
		"[DONE]",
	}
	_ = args1
	// ghost is unknown upstream: passes through as function; patch restores as custom.
	// For order checking use two known tools: patch custom + lookup function.
	mixedReq := `{"model":"m","input":"hi","tools":[
		{"type":"custom","name":"patch"},
		{"type":"function","name":"lookup","strict":false,"parameters":{"type":"object"}}]}`
	gen2, _ := mustDecodeResponsesCustom(t, mixedReq, responsesDestChat)
	lookupArgs := `{"q":"hi"}`
	patchArgs, _ := json.Marshal(map[string]string{"content": "xyz"})
	mixed := []string{
		nsChatChunk(`{"tool_calls":[{"index":0,"id":"call_f","type":"function","function":{"name":"lookup","arguments":"`+strings.ReplaceAll(lookupArgs, `"`, `\"`)+`"}}]}`, ""),
		nsChatChunk(`{"tool_calls":[{"index":1,"id":"call_c","type":"function","function":{"name":"patch","arguments":`+respCustomQuote(string(patchArgs[:8]))+`}}]}`, ""),
		nsChatChunk(`{"tool_calls":[{"index":1,"function":{"arguments":`+respCustomQuote(string(patchArgs[8:]))+`}}]}`, ""),
		nsChatChunk(`{}`, "tool_calls"),
		nsChatUsage(),
		"[DONE]",
	}
	events := nsRunChatStream(t, mixed, gen2.ToolPlan)
	var kinds []string
	var names []string
	for _, ev := range events {
		if ty, ok := ev["type"].(string); ok {
			kinds = append(kinds, ty)
		}
		if item, ok := ev["item"].(map[string]any); ok {
			if n, ok := item["name"].(string); ok {
				names = append(names, n)
			}
			if ty, ok := item["type"].(string); ok {
				names = append(names, ty)
			}
		}
	}
	joined := strings.Join(names, ",")
	fi, ci := strings.Index(joined, "lookup"), strings.Index(joined, "patch")
	if fi < 0 || ci < 0 || fi > ci {
		t.Fatalf("source order not preserved: %v", names)
	}
	var done map[string]any
	for _, ev := range events {
		if ev["type"] == "response.output_item.done" {
			if item, ok := ev["item"].(map[string]any); ok && item["type"] == "custom_tool_call" {
				done = item
			}
		}
	}
	if done == nil || done["input"] != "xyz" {
		t.Fatalf("custom stream input not restored: %v", done)
	}
	// Truncation fails closed with no fabricated completion.
	var trunc bytes.Buffer
	trunc.WriteString("data: " + nsChatChunk(`{"tool_calls":[{"index":0,"id":"call_c","type":"function","function":{"name":"patch","arguments":"{}"}}]}`, "") + "\n\n")
	var truncOut bytes.Buffer
	tr2 := &ChatToResponsesSSETranslator{PublicModel: "public", Limits: DefaultLimits(), Plan: gen.ToolPlan}
	if rerr := tr2.Translate(&trunc, &truncOut, nil); rerr == nil || rerr.Status != 502 {
		t.Fatalf("truncation must 502, got %v", rerr)
	}
	if strings.Contains(truncOut.String(), "response.completed") {
		t.Fatalf("truncated stream must not fabricate completion")
	}
	// Single-error and write-failure bounds.
	if rerr := tr2.Translate(strings.NewReader("data: [DONE]\n\n"), &bytes.Buffer{}, nil); rerr == nil {
		t.Fatal("empty stream must fail")
	}
	failOut := &failAfterWriter{failAfter: 10}
	var failIn bytes.Buffer
	for _, c := range chunks {
		failIn.WriteString("data: " + c + "\n\n")
	}
	_ = failIn
	var failIn2 bytes.Buffer
	for _, c := range mixed {
		failIn2.WriteString("data: " + c + "\n\n")
	}
	tr3 := &ChatToResponsesSSETranslator{PublicModel: "public", Limits: DefaultLimits(), Plan: gen2.ToolPlan}
	if rerr := tr3.Translate(&failIn2, failOut, nil); rerr == nil {
		t.Fatal("write failure must surface an error")
	}
}

func TestResponsesCustomNamespaces(t *testing.T) {
	nsReq := `{"model":"m","input":"hi","tools":[
		{"type":"namespace","name":"muse","tools":[
			{"type":"custom","name":"patch","description":"Apply"},
			{"type":"function","name":"lookup","strict":false,"parameters":{"type":"object"}}]}],
		"tool_choice":{"type":"custom","namespace":"muse","name":"patch"}}`
	for _, dest := range []responsesDestination{responsesDestChat, responsesDestAnthropic, responsesDestGemini} {
		gen, losses := mustDecodeResponsesCustom(t, nsReq, dest)
		if len(gen.Tools) != 2 {
			t.Fatalf("dest %d tools not flattened: %+v", dest, gen.Tools)
		}
		var alias string
		for _, tl := range gen.Tools {
			if tl.IsCustom() {
				alias = tl.Name
			}
		}
		if alias == "" || !strings.HasPrefix(alias, "ns_") {
			t.Fatalf("dest %d namespaced custom must alias: %+v", dest, gen.Tools)
		}
		if !gen.ToolPlan.IsEmulatedCustom(alias) {
			t.Fatalf("dest %d namespaced custom must emulate", dest)
		}
		if gen.Choice == nil || !gen.Choice.IsCustomChoice() || gen.Choice.Name != alias {
			t.Fatalf("dest %d namespaced choice not flattened: %+v", dest, gen.Choice)
		}
		src, ok := gen.ToolPlan.SourceFor(alias)
		if !ok || src.Namespace != "muse" || src.Name != "patch" {
			t.Fatalf("dest %d provenance wrong: %+v", dest, src)
		}
		found := false
		for _, l := range losses {
			if l.Action == "approximated" {
				found = true
			}
		}
		if !found {
			t.Fatalf("dest %d namespaced custom must approximate: %v", dest, losses)
		}
		var out []byte
		var rerr *RouterError
		switch dest {
		case responsesDestChat:
			out, rerr = EncodeChatRequest(gen, AbsentEffort(), "up")
		case responsesDestAnthropic:
			out, rerr = EncodeAnthropicRequest(gen, "up")
		default:
			out, rerr = EncodeGeminiRequest(gen)
		}
		if rerr != nil {
			t.Fatalf("dest %d encode: %v", dest, rerr)
		}
		if !strings.Contains(string(out), alias) || strings.Contains(string(out), `"namespace"`) {
			t.Fatalf("dest %d upstream must carry flat alias only: %s", dest, out)
		}
		patchArgs, _ := json.Marshal(map[string]string{"content": "ns-input"})
		chat, anth, gem := respCustomUpstreamResponses(alias, string(patchArgs), "call_n")
		switch dest {
		case responsesDestChat:
			out, rerr = TranslateChatJSONToResponsesWithPlan([]byte(chat), "public", DefaultLimits(), gen.ToolPlan)
		case responsesDestAnthropic:
			out, rerr = TranslateAnthropicJSONToResponsesWithPlan([]byte(anth), "public", DefaultLimits(), gen.ToolPlan)
		default:
			out, rerr = TranslateGeminiJSONToResponsesWithPlan([]byte(gem), "public", DefaultLimits(), gen.ToolPlan)
		}
		if rerr != nil {
			t.Fatalf("dest %d restore: %v", dest, rerr)
		}
		var body map[string]any
		if err := json.Unmarshal(out, &body); err != nil {
			t.Fatal(err)
		}
		call := body["output"].([]any)[0].(map[string]any)
		if call["type"] != "custom_tool_call" || call["namespace"] != "muse" || call["name"] != "patch" || call["input"] != "ns-input" {
			t.Fatalf("dest %d namespaced restore wrong: %v", dest, call)
		}
	}
}

func TestResponsesCustomGeminiArrayStream(t *testing.T) {
	gen, _ := mustDecodeResponsesCustom(t, respCustomTextReq, responsesDestGemini)
	args, _ := json.Marshal(map[string]string{"content": "array-input ✓"})
	body := `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"patch","args":` + string(args) + `,"id":"call_9"}}]},"finishReason":"STOP","index":0}],` +
		`"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3,"totalTokenCount":8},"modelVersion":"gemini-x"}`
	var out bytes.Buffer
	tr := &GeminiToResponsesJSONArrayTranslator{PublicModel: "public", Limits: DefaultLimits(), Plan: gen.ToolPlan}
	if rerr := tr.Translate(strings.NewReader("["+body+"]"), &out, nil); rerr != nil {
		t.Fatalf("gemini array: %v", rerr)
	}
	events := parseResponsesEvents(t, out.String())
	found := false
	for _, ev := range events {
		if item, ok := ev["item"].(map[string]any); ok && item["type"] == "custom_tool_call" && item["input"] == "array-input ✓" {
			found = true
		}
	}
	if !found {
		t.Fatalf("gemini array custom not restored: %s", out.String())
	}
	lim := DefaultLimits()
	lim.MaxArgsChars = 5
	var out2 bytes.Buffer
	tr2 := &GeminiToResponsesJSONArrayTranslator{PublicModel: "public", Limits: lim, Plan: gen.ToolPlan}
	if rerr := tr2.Translate(strings.NewReader("["+body+"]"), &out2, nil); rerr == nil || rerr.Status != 502 {
		t.Fatalf("oversize must 502, got %v", rerr)
	}
	if strings.Contains(out2.String(), `"type":"custom_tool_call"`) && strings.Contains(out2.String(), "response.completed") {
		t.Fatalf("oversize must not emit completion: %s", out2.String())
	}
}

func TestResponsesCustomAnthropicStreamAndTextIncremental(t *testing.T) {
	gen, _ := mustDecodeResponsesCustom(t, respCustomTextReq, responsesDestAnthropic)
	upstream :=
		nsAFrame("message_start", `{"type":"message_start","message":{"id":"msg_9","type":"message","role":"assistant","model":"claude-x","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":1}}}`) +
			nsAFrame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`) +
			nsAFrame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi "}}`) +
			nsAFrame("content_block_stop", `{"type":"content_block_stop","index":0}`) +
			nsAFrame("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_7","name":"patch","input":{}}}`) +
			nsAFrame("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":`+nsAPartial(`{"content":"hel`)+`}}`) +
			nsAFrame("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":`+nsAPartial(`lo"}`)+`}}`) +
			nsAFrame("content_block_stop", `{"type":"content_block_stop","index":1}`) +
			nsAFrame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":3}}`) +
			nsAFrame("message_stop", `{"type":"message_stop"}`)
	var out bytes.Buffer
	var flushed []string
	tr := &AnthropicToResponsesSSETranslator{PublicModel: "public", Limits: DefaultLimits(), Plan: gen.ToolPlan}
	if rerr := tr.Translate(strings.NewReader(upstream), &out, func() { flushed = append(flushed, "flush") }); rerr != nil {
		t.Fatalf("anthropic stream: %v", rerr)
	}
	s := out.String()
	if !strings.Contains(s, "hello") || !strings.Contains(s, `"type":"custom_tool_call"`) {
		t.Fatalf("anthropic custom not restored: %s", s)
	}
	if !strings.Contains(s, "hi ") {
		t.Fatalf("text must stay incremental: %s", s)
	}
	if len(flushed) == 0 {
		t.Fatal("deltas must flush incrementally")
	}
	// Truncated Anthropic stream fails closed.
	trunc := nsAFrame("message_start", `{"type":"message_start","message":{"id":"msg_9","type":"message","role":"assistant","model":"claude-x","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":1}}}`) +
		nsAFrame("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_7","name":"patch","input":{}}}`)
	var truncOut bytes.Buffer
	tr2 := &AnthropicToResponsesSSETranslator{PublicModel: "public", Limits: DefaultLimits(), Plan: gen.ToolPlan}
	if rerr := tr2.Translate(strings.NewReader(trunc), &truncOut, nil); rerr == nil || rerr.Status != 502 {
		t.Fatalf("truncation must 502, got %v", rerr)
	}
	if strings.Contains(truncOut.String(), "response.completed") {
		t.Fatalf("truncated stream must not fabricate completion")
	}
}

func TestResponsesCustomNativePathUnchanged(t *testing.T) {
	// Same-proto Responses stays raw: nil plan restores nothing.
	raw := `{"id":"resp_1","object":"response","created_at":1710000000,"model":"up","status":"completed",` +
		`"output":[{"type":"custom_tool_call","id":"ctc_1","call_id":"c1","name":"patch","input":"hi","status":"completed"}],` +
		`"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
	resp, rerr := DecodeResponsesResponse([]byte(raw), DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	restored, rerr := (*ToolPlan)(nil).RestoreResponse(resp, DefaultLimits())
	if rerr != nil {
		t.Fatalf("nil restore: %v", rerr)
	}
	if len(restored.Items) != 1 {
		t.Fatalf("nil plan must pass through: %+v", restored.Items)
	}
	// Chat -> Responses native custom remains exact with no loss.
	chatRaw := `{"model":"m","messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"custom","custom":{"name":"patch"}}]}`
	gen, losses, rerr := DecodeChatForResponses([]byte(chatRaw), DefaultLimits())
	if rerr != nil {
		t.Fatalf("chat native decode: %v", rerr)
	}
	if len(losses) != 0 {
		t.Fatalf("native custom must be exact: %v", losses)
	}
	if gen.ToolPlan == nil || !gen.ToolPlan.IsNativeCustom("patch") {
		t.Fatalf("native plan missing")
	}
	if gen.ToolPlan.HasEmulatedCustom() {
		t.Fatalf("native plan must not emulate")
	}
}

func TestResponsesCustomCompleteKeepsMixedChatCallOrder(t *testing.T) {
	gen, _ := mustDecodeResponsesCustom(t, respCustomTextReq, responsesDestChat)
	raw := `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"up","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"custom_first","type":"function","function":{"name":"patch","arguments":"{\"content\":\"one\"}"}},{"id":"function_second","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	out, err := TranslateChatJSONToResponsesWithPlan([]byte(raw), "public", DefaultLimits(), gen.ToolPlan)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Output []struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
		} `json:"output"`
	}
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Output) != 2 || body.Output[0].Type != "custom_tool_call" || body.Output[0].CallID != "custom_first" || body.Output[1].CallID != "function_second" {
		t.Fatalf("output must preserve the source call order: %s", out)
	}
}
