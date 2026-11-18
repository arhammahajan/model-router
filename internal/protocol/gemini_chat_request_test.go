package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustGeminiDecode(t *testing.T, body string, stream bool) *Generation {
	t.Helper()
	g, losses, rerr := DecodeGeminiRequest([]byte(body), "public-model", stream, DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode failed: %v", rerr.Msg)
	}
	if len(losses) != 0 {
		t.Fatalf("expected no losses, got %v", losses)
	}
	return g
}

func mustGeminiDecodeLossy(t *testing.T, body string) []Loss {
	t.Helper()
	_, losses, rerr := DecodeGeminiRequest([]byte(body), "public-model", false, DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode failed: %v", rerr.Msg)
	}
	return losses
}

func expectGemini400(t *testing.T, body string, stream bool) *RouterError {
	t.Helper()
	if !json.Valid([]byte(body)) {
		t.Fatal("invalid JSON fixture does not exercise source-field validation")
	}
	_, _, rerr := DecodeGeminiRequest([]byte(body), "public-model", stream, DefaultLimits())
	if rerr == nil || rerr.Status != 400 {
		t.Fatalf("expected 400, got %v", rerr)
	}
	return rerr
}

func TestGeminiDecodeTextAndControls(t *testing.T) {
	g := mustGeminiDecode(t, `{"contents":[
		{"role":"user","parts":[{"text":"hello"}]},
		{"role":"model","parts":[{"text":"hi"}]},
		{"role":"user","parts":[{"text":"again"}]}],
		"systemInstruction":{"parts":[{"text":"sys"}]},
		"generationConfig":{"temperature":1.5,"topP":0.5,"maxOutputTokens":100,"stopSequences":["halt"]}}`, false)
	if g.Model != "public-model" || g.Stream {
		t.Fatalf("model/stream not from URL/method: %+v", g)
	}
	if len(g.Messages) != 4 || g.Messages[0].Role != RoleSystem {
		t.Fatalf("system mapping wrong: %+v", g.Messages)
	}
	if g.Messages[0].Texts[0].Text != "sys" {
		t.Fatalf("system text wrong: %+v", g.Messages[0])
	}
	if *g.Temperature != 1.5 || *g.TopP != 0.5 || *g.MaxTokens != 100 {
		t.Fatalf("controls wrong: %+v", g)
	}
	if len(g.Stop) != 1 || g.Stop[0] != "halt" {
		t.Fatalf("stop wrong: %+v", g.Stop)
	}
	// The shared Chat encoder must render the IR with include_usage on
	// streams and the public model replaced by the upstream id.
	g.Stream = true
	wire, rerr := EncodeChatRequest(g, AbsentEffort(), "upstream-id")
	if rerr != nil {
		t.Fatalf("encode failed: %v", rerr.Msg)
	}
	var m map[string]any
	if err := json.Unmarshal(wire, &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "upstream-id" {
		t.Fatalf("upstream model not set: %v", m["model"])
	}
	so, _ := m["stream_options"].(map[string]any)
	if so["include_usage"] != true {
		t.Fatalf("stream_options.include_usage missing: %v", m)
	}
}

func TestGeminiDecodeSnakeAliasesAndCollisions(t *testing.T) {
	g := mustGeminiDecode(t, `{"contents":[{"parts":[{"text":"hi"}]}],
		"system_instruction":{"parts":[{"text":"s"}]},
		"generation_config":{"max_output_tokens":5,"stop_sequences":["x"]}}`, false)
	if g.Messages[0].Texts[0].Text != "s" || *g.MaxTokens != 5 || g.Stop[0] != "x" {
		t.Fatalf("snake aliases not honored: %+v", g)
	}
	rerr := expectGemini400(t, `{"contents":[{"parts":[{"text":"hi"}]}],
		"systemInstruction":{"parts":[{"text":"a"}]},
		"system_instruction":{"parts":[{"text":"b"}]}}`, false)
	if !strings.Contains(rerr.Msg, "more than once") {
		t.Fatalf("collision not diagnosed: %v", rerr.Msg)
	}
	expectGemini400(t, `{"contents":[{"parts":[{"text":"hi"}]}],"bogus":{}}`, false)
	expectGemini400(t, `{"contents":[{"role":"user","parts":[{"text":"hi","bogus":1}]}]}`, false)
}

func TestGeminiDecodeBodyModelStreamRejected(t *testing.T) {
	expectGemini400(t, `{"model":"x","contents":[{"parts":[{"text":"hi"}]}]}`, false)
	expectGemini400(t, `{"stream":true,"contents":[{"parts":[{"text":"hi"}]}]}`, true)
}

func TestGeminiDecodeRolesAndInterleaving(t *testing.T) {
	// Missing role defaults to user (documented Content behavior).
	g := mustGeminiDecode(t, `{"contents":[{"parts":[{"text":"hi"}]}]}`, false)
	if g.Messages[0].Role != RoleUser {
		t.Fatalf("default role wrong: %+v", g.Messages[0])
	}
	expectGemini400(t, `{"contents":[{"role":"assistant","parts":[{"text":"hi"}]}]}`, false)
	expectGemini400(t, `{"contents":[]}`, false)
	// Mixed order inside one content is refused, not reordered.
	expectGemini400(t, `{"contents":[
		{"role":"model","parts":[
			{"text":"a"},
			{"functionCall":{"name":"f","args":{}}},
			{"text":"b"}]},
		{"role":"user","parts":[{"functionResponse":{"name":"f","response":{}}}]}]}`, false)
	expectGemini400(t, `{"contents":[
		{"role":"model","parts":[{"functionCall":{"name":"f","args":{}}}]},
		{"role":"user","parts":[{"text":"t"},{"functionResponse":{"name":"f","response":{}}}]}]}`, false)
}

func TestGeminiDecodeFunctionRoundTrip(t *testing.T) {
	g := mustGeminiDecode(t, `{"contents":[
		{"role":"user","parts":[{"text":"go"}]},
		{"role":"model","parts":[
			{"text":"calling"},
			{"functionCall":{"name":"get_weather","args":{"city":"Oslo","n":9007199254740993},"id":"call_1"}}]},
		{"role":"user","parts":[
			{"functionResponse":{"name":"get_weather","response":{"temp":9007199254740993},"id":"call_1"}}]}],
		"tools":[{"functionDeclarations":[{"name":"get_weather",
			"description":"weather",
			"parameters":{"type":"OBJECT","properties":{"city":{"type":"STRING"},"n":{"type":"INTEGER"}}}}]}],
		"toolConfig":{"functionCallingConfig":{"mode":"AUTO"}}}`, false)
	if len(g.Tools) != 1 || g.Tools[0].Name != "get_weather" {
		t.Fatalf("tool missing: %+v", g.Tools)
	}
	var schema map[string]any
	if err := json.Unmarshal(g.Tools[0].Schema, &schema); err != nil {
		t.Fatal(err)
	}
	if schema["type"] != "object" {
		t.Fatalf("schema type not lowercased: %v", schema)
	}
	if g.Choice == nil || g.Choice.Mode != "auto" {
		t.Fatalf("choice wrong: %+v", g.Choice)
	}
	// Large integers survive exactly through the Chat wire encoding.
	wire, rerr := EncodeChatRequest(g, AbsentEffort(), "up")
	if rerr != nil {
		t.Fatalf("encode: %v", rerr.Msg)
	}
	if !strings.Contains(string(wire), "9007199254740993") {
		t.Fatalf("large integer lost: %s", wire)
	}
	if !strings.Contains(string(wire), `"tool_call_id":"call_1"`) {
		t.Fatalf("supplied id not preserved: %s", wire)
	}
}

func TestGeminiDecodeMissingIDsNameCorrelation(t *testing.T) {
	// Missing ids correlate by unambiguous name.
	g := mustGeminiDecode(t, `{"contents":[
		{"role":"model","parts":[{"functionCall":{"name":"f","args":{"a":1}}}]},
		{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"ok":true}}}]}],
		"tools":[{"functionDeclarations":[{"name":"f"}]}]}`, false)
	callID := g.Messages[0].ToolCalls[0].ID
	if !strings.HasPrefix(callID, "gdec-") {
		t.Fatalf("compat id not scoped: %q", callID)
	}
	var toolMsg *Message
	for i := range g.Messages {
		if g.Messages[i].Role == RoleTool {
			toolMsg = &g.Messages[i]
		}
	}
	if toolMsg == nil || toolMsg.Results[0].CallID != callID {
		t.Fatalf("name correlation broken: %+v", g.Messages)
	}
	// Parallel same-name calls without ids are ambiguous: refused.
	expectGemini400(t, `{"contents":[
		{"role":"model","parts":[
			{"functionCall":{"name":"f","args":{}}},
			{"functionCall":{"name":"f","args":{}}}]},
		{"role":"user","parts":[{"functionResponse":{"name":"f","response":{}}}]}],
		"tools":[{"functionDeclarations":[{"name":"f"}]}]}`, false)
	// A response id with no matching call is refused.
	expectGemini400(t, `{"contents":[
		{"role":"model","parts":[{"functionCall":{"name":"f","args":{}}}]},
		{"role":"user","parts":[{"functionResponse":{"name":"f","response":{},"id":"nope"}}]}],
		"tools":[{"functionDeclarations":[{"name":"f"}]}]}`, false)
	// Opaque supplied ids are preserved exactly with no loss: the Chat
	// function-call id is Required[str] with no function-name grammar, and
	// the Gemini FunctionCall id is an optional string.
	g2, losses, rerr := DecodeGeminiRequest([]byte(`{"contents":[
		{"role":"model","parts":[{"functionCall":{"name":"f","args":{},"id":"has:colon"}}]},
		{"role":"user","parts":[{"functionResponse":{"name":"f","response":{},"id":"has:colon"}}]}],
		"tools":[{"functionDeclarations":[{"name":"f"}]}]}`), "public-model", false, DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr.Msg)
	}
	if len(losses) != 0 {
		t.Fatalf("opaque id must be preserved without loss, got %v", losses)
	}
	if g2.Messages[0].ToolCalls[0].ID != "has:colon" || g2.Messages[1].Results[0].CallID != "has:colon" {
		t.Fatalf("opaque id not preserved: %+v", g2.Messages)
	}
}

func TestGeminiDecodeLosses(t *testing.T) {
	// Superseded by the Gemini-request reasoning contract: thinkingBudget
	// no longer emits a decode loss; a positive budget decodes to a
	// Budget intent (planned downstream), -1/UNSPECIFIED decode to
	// Absent, and 0 decodes to Off. The loss list below therefore omits
	// the thinking control; intent construction is covered by
	// gemini_reasoning_test.go.
	losses := mustGeminiDecodeLossy(t, `{"contents":[{"parts":[{"text":"hi"}]}],
		"generationConfig":{"topK":10,"seed":7,"candidateCount":2,"responseLogprobs":true,
			"thinkingConfig":{"thinkingBudget":100},"speechConfig":{}},
		"safetySettings":[{"category":"HARM_CATEGORY_HATE_SPEECH","threshold":"BLOCK_ONLY_HIGH"}],
		"labels":{"k":"v"},"serviceTier":"standard","store":true,
		"tools":[{"functionDeclarations":[{"name":"f","behavior":"BLOCKING","response":{"type":"OBJECT"}}]}]}`)
	fields := map[string]bool{}
	for _, l := range losses {
		fields[l.Field] = true
	}
	for _, want := range []string{"generationConfig.topK", "generationConfig.seed",
		"generationConfig.candidateCount", "generationConfig.responseLogprobs",
		"generationConfig.speechConfig",
		"safetySettings", "labels", "serviceTier", "store",
		"tools[0].functionDeclarations[0].behavior",
		"tools[0].functionDeclarations[0].response"} {
		if !fields[want] {
			t.Fatalf("missing loss %q in %v", want, losses)
		}
	}
	if fields["generationConfig.thinkingConfig.thinkingBudget"] {
		t.Fatalf("thinkingBudget must decode to an intent, not a loss: %v", losses)
	}
	// Malformed loss-carrying fields are 400 even in lossy intent.
	expectGemini400(t, `{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"topK":"x"}}`, false)
	expectGemini400(t, `{"contents":[{"parts":[{"text":"hi"}]}],"safetySettings":[{"category":"x","bogus":1}]}`, false)
}

func TestGeminiDecodeStateAndHostedToolsRefused(t *testing.T) {
	expectGemini400(t, `{"contents":[{"parts":[{"text":"hi"}]}],"cachedContent":"cachedContents/1"}`, false)
	expectGemini400(t, `{"contents":[{"parts":[{"text":"hi"}]}],"tools":[{"googleSearch":{}}]}`, false)
	expectGemini400(t, `{"contents":[{"parts":[{"text":"hi"}]}],"tools":[{"functionDeclarations":[{"name":"f","parameters":{},"parametersJsonSchema":{}}]}]}`, false)
}

func TestGeminiDecodeParametersJsonSchemaExact(t *testing.T) {
	// parametersJsonSchema passes through verbatim (no normalizer): the
	// large integer literal must survive the Chat wire encoding.
	g := mustGeminiDecode(t, `{"contents":[{"parts":[{"text":"hi"}]}],
		"tools":[{"functionDeclarations":[{"name":"f",
			"parametersJsonSchema":{"type":"object","properties":{"n":{"const":9007199254740993}}}}]}]}`, false)
	wire, rerr := EncodeChatRequest(g, AbsentEffort(), "up")
	if rerr != nil {
		t.Fatalf("encode: %v", rerr.Msg)
	}
	if !strings.Contains(string(wire), "9007199254740993") {
		t.Fatalf("schema number lost: %s", wire)
	}
}
