package protocol

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// Chat ingress custom emulation to Anthropic/Gemini via the shared
// ToolPlan, single {"content":string} codec and typed restore stage.

const chatEmuTextReq = `{"model":"m","messages":[{"role":"user","content":"patch this"}],
	"tools":[{"type":"custom","custom":{"name":"patch","description":"Apply a patch"}}],
	"tool_choice":{"type":"custom","custom":{"name":"patch"}}}`

const chatEmuGrammarReq = `{"model":"m","messages":[{"role":"user","content":"patch this"}],
	"tools":[{"type":"custom","custom":{"name":"patch","description":"Apply","format":{"type":"grammar","grammar":{"syntax":"regex","definition":"[a-z]+"}}}}],
	"tool_choice":{"type":"custom","custom":{"name":"patch"}}}`

func mustDecodeChatEmu(t *testing.T, raw string, dest chatDest) (*Generation, []Loss) {
	t.Helper()
	var gen *Generation
	var losses []Loss
	var rerr *RouterError
	switch dest {
	case chatDestAnthropic:
		gen, losses, rerr = DecodeChatRequest([]byte(raw), DefaultLimits())
	case chatDestGemini:
		gen, losses, rerr = DecodeChatForGemini([]byte(raw), DefaultLimits())
	default:
		t.Fatalf("bad dest")
	}
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	return gen, losses
}

func TestChatEmuDefinitionsBothDests(t *testing.T) {
	for _, dest := range []chatDest{chatDestAnthropic, chatDestGemini} {
		gen, losses := mustDecodeChatEmu(t, chatEmuTextReq, dest)
		if len(gen.Tools) != 1 || !gen.Tools[0].IsCustom() {
			t.Fatalf("dest %d custom not decoded: %+v", dest, gen.Tools)
		}
		if gen.Choice == nil || !gen.Choice.IsCustomChoice() || gen.Choice.Name != "patch" {
			t.Fatalf("dest %d custom choice not decoded: %+v", dest, gen.Choice)
		}
		if gen.ToolPlan == nil || !gen.ToolPlan.IsEmulatedCustom("patch") {
			t.Fatalf("dest %d emu binding missing", dest)
		}
		found := false
		for _, l := range losses {
			if l.Action == "approximated" {
				found = true
			}
			if strings.Contains(l.Reason, "grammar") && strings.Contains(strings.ToLower(l.Reason), "enforce") {
				t.Fatalf("grammar loss must not claim enforcement: %v", l)
			}
		}
		if !found {
			t.Fatalf("dest %d custom must approximate: %v", dest, losses)
		}
		if _, rerr := EnforcePolicy(losses, false); rerr == nil || rerr.Code != CodeTranslationLoss {
			t.Fatalf("dest %d strict must refuse pre-I/O: %v", dest, rerr)
		}
		kept, rerr := EnforcePolicy(losses, true)
		if rerr != nil {
			t.Fatalf("dest %d lossy must allow: %v", dest, rerr)
		}
		if !strings.Contains(EncodeLosses(kept), "approximated") {
			t.Fatalf("dest %d lossy header must carry approximation", dest)
		}
		var out []byte
		if dest == chatDestAnthropic {
			out, rerr = EncodeAnthropicRequest(gen, "up")
		} else {
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

func TestChatEmuGrammarAsInstructions(t *testing.T) {
	gen, losses := mustDecodeChatEmu(t, chatEmuGrammarReq, chatDestAnthropic)
	if len(losses) == 0 {
		t.Fatalf("grammar must still approximate: %v", losses)
	}
	if gen.Tools[0].Format == nil || gen.Tools[0].Format.Type != "grammar" {
		t.Fatalf("grammar not preserved: %+v", gen.Tools[0].Format)
	}
	out, rerr := EncodeAnthropicRequest(gen, "up")
	if rerr != nil {
		t.Fatalf("encode: %v", rerr)
	}
	s := string(out)
	if !strings.Contains(s, "Return content following this regex grammar:") || !strings.Contains(s, "[a-z]+") {
		t.Fatalf("grammar must travel as instructions text: %s", s)
	}
	genG, _ := mustDecodeChatEmu(t, chatEmuGrammarReq, chatDestGemini)
	outG, rerr := EncodeGeminiRequest(genG)
	if rerr != nil {
		t.Fatalf("gemini encode: %v", rerr)
	}
	if !strings.Contains(string(outG), "[a-z]+") {
		t.Fatalf("gemini grammar must travel as instructions: %s", outG)
	}
}

func TestChatEmuChoiceFamily(t *testing.T) {
	// Function choice naming a custom tool must refuse.
	bad := `{"model":"m","messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"custom","custom":{"name":"patch"}}],
		"tool_choice":{"type":"function","function":{"name":"patch"}}}`
	for _, dest := range []chatDest{chatDestAnthropic, chatDestGemini} {
		var rerr *RouterError
		if dest == chatDestAnthropic {
			_, _, rerr = DecodeChatRequest([]byte(bad), DefaultLimits())
		} else {
			_, _, rerr = DecodeChatForGemini([]byte(bad), DefaultLimits())
		}
		if rerr == nil || rerr.Status != 400 {
			t.Fatalf("dest %d choice family mismatch must 400, got %v", dest, rerr)
		}
	}
	// Custom choice naming a function tool must refuse.
	bad2 := `{"model":"m","messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],
		"tool_choice":{"type":"custom","custom":{"name":"lookup"}}}`
	for _, dest := range []chatDest{chatDestAnthropic, chatDestGemini} {
		var rerr *RouterError
		if dest == chatDestAnthropic {
			_, _, rerr = DecodeChatRequest([]byte(bad2), DefaultLimits())
		} else {
			_, _, rerr = DecodeChatForGemini([]byte(bad2), DefaultLimits())
		}
		if rerr == nil || rerr.Status != 400 {
			t.Fatalf("dest %d choice family mismatch must 400, got %v", dest, rerr)
		}
	}
	// Unknown choice must refuse.
	unknown := `{"model":"m","messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"custom","custom":{"name":"patch"}}],
		"tool_choice":{"type":"custom","custom":{"name":"ghost"}}}`
	if _, _, rerr := DecodeChatRequest([]byte(unknown), DefaultLimits()); rerr == nil || rerr.Status != 400 {
		t.Fatalf("unknown custom choice must 400, got %v", rerr)
	}
	if _, _, rerr := DecodeChatForGemini([]byte(unknown), DefaultLimits()); rerr == nil || rerr.Status != 400 {
		t.Fatalf("unknown custom choice must 400 (gemini), got %v", rerr)
	}
}

func TestChatEmuHistoryExactAndOrphan(t *testing.T) {
	input := "héllo\nworld ✓ \U0001F600 \"quoted\""
	rawInput, _ := json.Marshal(input)
	raw := `{"model":"m","messages":[
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"custom","custom":{"name":"patch","input":` + string(rawInput) + `}}]},
		{"role":"tool","tool_call_id":"call_1","content":"applied"}],
		"tools":[{"type":"custom","custom":{"name":"patch"}}]}`
	for _, dest := range []chatDest{chatDestAnthropic, chatDestGemini} {
		gen, _ := mustDecodeChatEmu(t, raw, dest)
		if len(gen.Messages) != 2 {
			t.Fatalf("dest %d history order lost", dest)
		}
		tc := gen.Messages[0].ToolCalls[0]
		if !tc.IsCustom || tc.CustomInput != input || tc.ID != "call_1" {
			t.Fatalf("dest %d exact input not preserved: %+v", dest, tc)
		}
	}
	// Orphan history without a definition is admitted with its own binding + loss.
	orphan := `{"model":"m","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","tool_calls":[{"id":"c1","type":"custom","custom":{"name":"ghost","input":"x"}}]},
		{"role":"tool","tool_call_id":"c1","content":"ok"}]}`
	for _, dest := range []chatDest{chatDestAnthropic, chatDestGemini} {
		gen, losses := mustDecodeChatEmu(t, orphan, dest)
		if gen.ToolPlan == nil || !gen.ToolPlan.IsEmulatedCustom("ghost") {
			t.Fatalf("dest %d orphan must mint emu binding", dest)
		}
		found := false
		for _, l := range losses {
			if l.Action == "approximated" {
				found = true
			}
		}
		if !found {
			t.Fatalf("dest %d orphan must approximate: %v", dest, losses)
		}
	}
	// Family conflicts fail closed.
	conflicts := []string{
		// Function history with an emulated custom name.
		`{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"patch","arguments":"{}"}}]}],"tools":[{"type":"custom","custom":{"name":"patch"}}]}`,
		// Custom history with a function definition name.
		`{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"c1","type":"custom","custom":{"name":"lookup","input":"x"}}]}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`,
		// Duplicate tool definitions across families.
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"custom","custom":{"name":"dup"}},{"type":"function","function":{"name":"dup","parameters":{"type":"object"}}}]}`,
	}
	for _, raw := range conflicts {
		if _, _, rerr := DecodeChatRequest([]byte(raw), DefaultLimits()); rerr == nil || rerr.Status != 400 {
			t.Fatalf("anthropic conflict must 400: %s got %v", raw, rerr)
		}
		if _, _, rerr := DecodeChatForGemini([]byte(raw), DefaultLimits()); rerr == nil || rerr.Status != 400 {
			t.Fatalf("gemini conflict must 400: %s got %v", raw, rerr)
		}
	}
}

func chatEmuUpstream(alias, call string, args string) (string, string) {
	anth := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x",` +
		`"content":[{"type":"tool_use","id":"` + call + `","name":"` + alias + `","input":` + args + `}],` +
		`"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":3}}`
	gem := `{"candidates":[{"content":{"role":"model","parts":[` +
		`{"functionCall":{"name":"` + alias + `","args":` + args + `,"id":"` + call + `"}}]},` +
		`"finishReason":"STOP","index":0}],"modelVersion":"gemini-x",` +
		`"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3,"totalTokenCount":8}}`
	return anth, gem
}

func TestChatEmuJSONRestored(t *testing.T) {
	args, _ := json.Marshal(map[string]string{"content": "hello ✓\nworld"})
	for _, dest := range []chatDest{chatDestAnthropic, chatDestGemini} {
		gen, _ := mustDecodeChatEmu(t, chatEmuTextReq, dest)
		anth, gem := chatEmuUpstream("patch", "call_7", string(args))
		raw := anth
		if dest == chatDestGemini {
			raw = gem
		}
		var out []byte
		var rerr *RouterError
		if dest == chatDestAnthropic {
			out, rerr = TranslateAnthropicJSONToChatWithPlan([]byte(raw), "public", DefaultLimits(), gen.ToolPlan)
		} else {
			out, rerr = TranslateGeminiJSONToChatWithPlan([]byte(raw), "public", DefaultLimits(), gen.ToolPlan)
		}
		if rerr != nil {
			t.Fatalf("dest %d restore: %v", dest, rerr)
		}
		var body map[string]any
		if err := json.Unmarshal(out, &body); err != nil {
			t.Fatal(err)
		}
		msg := body["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
		calls := msg["tool_calls"].([]any)
		if len(calls) != 1 {
			t.Fatalf("dest %d want one restored call: %v", dest, calls)
		}
		call := calls[0].(map[string]any)
		if call["type"] != "custom" || call["id"] != "call_7" {
			t.Fatalf("dest %d identity not restored: %v", dest, call)
		}
		custom := call["custom"].(map[string]any)
		if custom["name"] != "patch" || custom["input"] != "hello ✓\nworld" {
			t.Fatalf("dest %d exact input not restored: %v", dest, custom)
		}
	}
	// Nil plan stays raw (function passthrough).
	rawAnth, _ := chatEmuUpstream("patch", "call_7", `{"content":"hi"}`)
	out, rerr := TranslateAnthropicJSON([]byte(rawAnth), "public", DefaultLimits())
	if rerr != nil {
		t.Fatalf("nil plan: %v", rerr)
	}
	if !strings.Contains(string(out), `"type":"function"`) {
		t.Fatalf("nil plan must stay raw: %s", out)
	}
}

func TestChatEmuMalformedWrappersFailClosed(t *testing.T) {
	gen, _ := mustDecodeChatEmu(t, chatEmuTextReq, chatDestAnthropic)
	quote := func(s string) string {
		q, _ := json.Marshal(s)
		return string(q)
	}
	for name, args := range map[string]string{
		"extra key":     `{"content":"hi","extra":1}`,
		"duplicate key": `{"content":"first","\u0063ontent":"second"}`,
		"non-string":    `{"content":17}`,
		"empty object":  `{}`,
		"array":         `[1]`,
		"scalar":        `"hi"`,
		"missing key":   `{"text":"hi"}`,
		"null content":  `{"content":null}`,
		"invalid json":  `{oops}`,
		"empty":         ``,
	} {
		raw := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x",` +
			`"content":[{"type":"tool_use","id":"c1","name":"patch","input":` + args + `}],` +
			`"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`
		_ = quote
		if _, rerr := TranslateAnthropicJSONToChatWithPlan([]byte(raw), "public", DefaultLimits(), gen.ToolPlan); rerr == nil || rerr.Status != 502 {
			t.Fatalf("%s must refuse 502, got %v", name, rerr)
		}
	}
	genG, _ := mustDecodeChatEmu(t, chatEmuTextReq, chatDestGemini)
	for name, args := range map[string]string{
		"extra key":  `{"content":"hi","extra":1}`,
		"non-string": `{"content":17}`,
		"empty":      `{}`,
	} {
		raw := `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"patch","args":` + args + `,"id":"c1"}}]},"finishReason":"STOP","index":0}],` +
			`"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2},"modelVersion":"gemini-x"}`
		if _, rerr := TranslateGeminiJSONToChatWithPlan([]byte(raw), "public", DefaultLimits(), genG.ToolPlan); rerr == nil || rerr.Status != 502 {
			t.Fatalf("gemini %s must refuse 502, got %v", name, rerr)
		}
	}
	// Mismatched native custom in the IR fails closed.
	resp, rerr := DecodeResponsesResponse([]byte(`{"id":"resp_1","object":"response","created_at":1,"model":"up","status":"completed",`+
		`"output":[{"type":"custom_tool_call","id":"ctc_1","call_id":"c1","name":"patch","input":"hi","status":"completed"}],`+
		`"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`), DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode native custom: %v", rerr)
	}
	if _, rerr := gen.ToolPlan.RestoreResponse(resp, DefaultLimits()); rerr == nil || rerr.Status != 502 {
		t.Fatalf("mismatched native custom must refuse, got %v", rerr)
	}
	// Oversize wrapper fails.
	lim := DefaultLimits()
	lim.MaxArgsChars = 5
	big, _ := json.Marshal(map[string]string{"content": "toolonginput"})
	rawBig := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x",` +
		`"content":[{"type":"tool_use","id":"c1","name":"patch","input":` + string(big) + `}],` +
		`"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`
	if _, rerr := TranslateAnthropicJSONToChatWithPlan([]byte(rawBig), "public", lim, gen.ToolPlan); rerr == nil || rerr.Status != 502 {
		t.Fatalf("oversize must 502, got %v", rerr)
	}
}

func runAnthropicToChatStream(t *testing.T, sse string, plan *ToolPlan, lim Limits) (string, *RouterError) {
	t.Helper()
	tr := &SSETranslator{PublicModel: "public", IncludeUsage: false, Limits: lim, Plan: plan}
	var out bytes.Buffer
	rerr := tr.Translate(strings.NewReader(sse), &out, nil)
	return out.String(), rerr
}

func runGeminiToChatStream(t *testing.T, sse string, plan *ToolPlan) (string, *RouterError) {
	t.Helper()
	tr := &GeminiToChatSSETranslator{PublicModel: "public", IncludeUsage: false, Limits: DefaultLimits(), Plan: plan}
	var out bytes.Buffer
	rerr := tr.Translate(strings.NewReader(sse), &out, nil)
	return out.String(), rerr
}

func TestChatEmuStreamAnthropicOrderIncremental(t *testing.T) {
	gen, _ := mustDecodeChatEmu(t, chatEmuTextReq, chatDestAnthropic)
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
	tr := &SSETranslator{PublicModel: "public", Limits: DefaultLimits(), Plan: gen.ToolPlan}
	if rerr := tr.Translate(strings.NewReader(upstream), &out, func() { flushed = append(flushed, "flush") }); rerr != nil {
		t.Fatalf("anthropic stream: %v", rerr)
	}
	s := out.String()
	if !strings.Contains(s, "hello") || !strings.Contains(s, `"type":"custom"`) {
		t.Fatalf("anthropic custom not restored: %s", s)
	}
	if !strings.Contains(s, "hi ") {
		t.Fatalf("text must stay incremental: %s", s)
	}
	if len(flushed) == 0 {
		t.Fatal("deltas must flush incrementally")
	}
	// Truncation fails closed.
	trunc := nsAFrame("message_start", `{"type":"message_start","message":{"id":"msg_9","type":"message","role":"assistant","model":"claude-x","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":1}}}`) +
		nsAFrame("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_7","name":"patch","input":{}}}`)
	var truncOut bytes.Buffer
	tr2 := &SSETranslator{PublicModel: "public", Limits: DefaultLimits(), Plan: gen.ToolPlan}
	if rerr := tr2.Translate(strings.NewReader(trunc), &truncOut, nil); rerr == nil || rerr.Status != 502 {
		t.Fatalf("truncation must 502, got %v", rerr)
	}
	if strings.Contains(truncOut.String(), "[DONE]") || strings.Contains(truncOut.String(), `"finish_reason":"tool_calls"`) {
		t.Fatalf("truncated stream must not fabricate completion: %s", truncOut.String())
	}
}

func TestChatEmuStreamGeminiSSEAndArray(t *testing.T) {
	gen, _ := mustDecodeChatEmu(t, chatEmuTextReq, chatDestGemini)
	args, _ := json.Marshal(map[string]string{"content": "array-input ✓"})
	sseBody := `{"candidates":[{"content":{"role":"model","parts":[{"text":"hi "},{"functionCall":{"name":"patch","args":` + string(args) + `,"id":"call_9"}}]},"finishReason":"STOP","index":0}],` +
		`"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3,"totalTokenCount":8},"modelVersion":"gemini-x"}`
	var out bytes.Buffer
	tr := &GeminiToChatSSETranslator{PublicModel: "public", Limits: DefaultLimits(), Plan: gen.ToolPlan}
	if rerr := tr.Translate(strings.NewReader("data: "+sseBody+"\n\n"), &out, nil); rerr != nil {
		t.Fatalf("gemini sse: %v", rerr)
	}
	if !strings.Contains(out.String(), `"type":"custom"`) || !strings.Contains(out.String(), "array-input ✓") {
		t.Fatalf("gemini sse custom not restored: %s", out.String())
	}
	var arrOut bytes.Buffer
	arrTr := &GeminiToChatJSONArrayTranslator{PublicModel: "public", Limits: DefaultLimits(), Plan: gen.ToolPlan}
	if rerr := arrTr.Translate(strings.NewReader("["+sseBody+"]"), &arrOut, nil); rerr != nil {
		t.Fatalf("gemini array: %v", rerr)
	}
	if !strings.Contains(arrOut.String(), `"type":"custom"`) || !strings.Contains(arrOut.String(), "array-input ✓") {
		t.Fatalf("gemini array custom not restored: %s", arrOut.String())
	}
	// Oversize array fails closed without emitting completion.
	lim := DefaultLimits()
	lim.MaxArgsChars = 5
	var out2 bytes.Buffer
	tr2 := &GeminiToChatJSONArrayTranslator{PublicModel: "public", Limits: lim, Plan: gen.ToolPlan}
	if rerr := tr2.Translate(strings.NewReader("["+sseBody+"]"), &out2, nil); rerr == nil || rerr.Status != 502 {
		t.Fatalf("oversize must 502, got %v", rerr)
	}
	if strings.Contains(out2.String(), `"type":"custom"`) && strings.Contains(out2.String(), "[DONE]") {
		t.Fatalf("oversize must not emit completion: %s", out2.String())
	}
	// Write failure surfaces an error.
	failOut := &failAfterWriter{failAfter: 10}
	tr3 := &SSETranslator{PublicModel: "public", Limits: DefaultLimits(), Plan: gen.ToolPlan}
	upstream := nsAFrame("message_start", `{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"c","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}}`) +
		nsAFrame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_1","name":"patch","input":{}}}`) +
		nsAFrame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":`+nsAPartial(`{"content":"hi"}`)+`}}`) +
		nsAFrame("content_block_stop", `{"type":"content_block_stop","index":0}`) +
		nsAFrame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":1}}`) +
		nsAFrame("message_stop", `{"type":"message_stop"}`)
	if rerr := tr3.Translate(strings.NewReader(upstream), failOut, nil); rerr == nil {
		t.Fatal("write failure must surface an error")
	}
}
