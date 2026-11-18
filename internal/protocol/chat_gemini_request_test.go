// Chat -> Gemini request codec tests: decode policy plus wire encoding.
package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestChatGeminiDecodedToolResultPreserved(t *testing.T) {
	for _, content := range []string{"noon", "", "α\n🙂"} {
		quoted, _ := json.Marshal(content)
		raw := []byte(`{"model":"m","messages":[{"role":"user","content":"Hi"},{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":` + string(quoted) + `}]}`)
		gen, _, rerr := DecodeChatForGemini(raw, DefaultLimits())
		if rerr != nil {
			t.Fatal(rerr)
		}
		wire, rerr := EncodeGeminiRequest(gen)
		if rerr != nil {
			t.Fatal(rerr)
		}
		var body struct {
			Contents []struct {
				Parts []struct {
					Response struct {
						Body struct{ Output string } `json:"response"`
					} `json:"functionResponse"`
				}
			}
		}
		if err := json.Unmarshal(wire, &body); err != nil {
			t.Fatal(err)
		}
		if got := body.Contents[2].Parts[0].Response.Body.Output; got != content {
			t.Fatalf("tool result = %q, want %q", got, content)
		}
	}
}

func c2gDecode(t *testing.T, body string) (*Generation, []Loss) {
	t.Helper()
	g, losses, rerr := DecodeChatForGemini([]byte(body), DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode failed: %v", rerr)
	}
	return g, losses
}

func c2gDecodeErr(t *testing.T, body string) *RouterError {
	t.Helper()
	_, _, rerr := DecodeChatForGemini([]byte(body), DefaultLimits())
	if rerr == nil {
		t.Fatalf("expected error for %s", body)
	}
	return rerr
}

func TestC2GTextRequestEncodes(t *testing.T) {
	g, losses := c2gDecode(t, `{"model":"m","messages":[
		{"role":"system","content":"Be brief."},
		{"role":"user","content":"Hi"},
		{"role":"assistant","content":"Hello"},
		{"role":"user","content":"Again"}],
		"temperature":0.5,"top_p":0.9,"max_tokens":32,"stop":["END"]}`)
	if len(losses) != 0 {
		t.Fatalf("losses = %v", losses)
	}
	out, rerr := EncodeGeminiRequest(g)
	if rerr != nil {
		t.Fatalf("encode: %v", rerr)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatalf("wire not JSON: %v", err)
	}
	for _, k := range []string{"model", "stream"} {
		if _, ok := body[k]; ok {
			t.Fatalf("wire must not carry %q: %s", k, out)
		}
	}
	var sys struct {
		Parts []struct {
			Text string `json:"text"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(body["systemInstruction"], &sys); err != nil || len(sys.Parts) != 1 || sys.Parts[0].Text != "Be brief." {
		t.Fatalf("systemInstruction = %s", body["systemInstruction"])
	}
	var contents []struct {
		Role  string `json:"role"`
		Parts []struct {
			Text string `json:"text"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(body["contents"], &contents); err != nil || len(contents) != 3 {
		t.Fatalf("contents = %s", body["contents"])
	}
	if contents[0].Role != "user" || contents[1].Role != "model" || contents[2].Role != "user" {
		t.Fatalf("roles = %v", contents)
	}
	var genCfg map[string]json.RawMessage
	if err := json.Unmarshal(body["generationConfig"], &genCfg); err != nil {
		t.Fatalf("generationConfig: %v", err)
	}
	for _, k := range []string{"temperature", "topP", "maxOutputTokens", "stopSequences"} {
		if _, ok := genCfg[k]; !ok {
			t.Fatalf("generationConfig missing %q: %s", k, body["generationConfig"])
		}
	}
	// snake_case must never appear on the Gemini wire.
	if strings.Contains(string(out), "max_output_tokens") || strings.Contains(string(out), "top_p") {
		t.Fatalf("snake_case leaked: %s", out)
	}
}

func TestC2GToolsEncodeWithParametersJsonSchema(t *testing.T) {
	g, losses := c2gDecode(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"get_weather",
		"description":"Get weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]},
		"strict":false}}],
		"tool_choice":{"type":"function","function":{"name":"get_weather"}},
		"parallel_tool_calls":true}`)
	if len(losses) != 0 {
		t.Fatalf("losses = %v", losses)
	}
	out, rerr := EncodeGeminiRequest(g)
	if rerr != nil {
		t.Fatalf("encode: %v", rerr)
	}
	s := string(out)
	if !strings.Contains(s, "parametersJsonSchema") {
		t.Fatalf("must use parametersJsonSchema, got %s", s)
	}
	if strings.Contains(s, `"parameters":`) {
		t.Fatalf("must not use OpenAPI parameters subset, got %s", s)
	}
	if strings.Contains(s, "strict") {
		t.Fatalf("strict must not appear on Gemini wire, got %s", s)
	}
	if !strings.Contains(s, `"mode":"ANY"`) || !strings.Contains(s, "get_weather") {
		t.Fatalf("toolConfig ANY+allowed names missing: %s", s)
	}
	// Exact numeric literal preservation through schema.
	g2, _ := c2gDecode(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{"n":{"const":9007199254740993}}}}}]}`)
	out2, _ := EncodeGeminiRequest(g2)
	if !strings.Contains(string(out2), "9007199254740993") {
		t.Fatalf("large integer not preserved: %s", out2)
	}
}

func TestC2GToolChoiceModes(t *testing.T) {
	modes := map[string]string{
		`"none"`:     `"mode":"NONE"`,
		`"auto"`:     `"mode":"AUTO"`,
		`"required"`: `"mode":"ANY"`,
	}
	for in, want := range modes {
		g, _ := c2gDecode(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],
			"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":`+in+`}`)
		out, rerr := EncodeGeminiRequest(g)
		if rerr != nil {
			t.Fatalf("%s: %v", in, rerr)
		}
		if !strings.Contains(string(out), want) {
			t.Fatalf("%s: want %s in %s", in, want, out)
		}
	}
}

func TestC2GLossPlan(t *testing.T) {
	g, losses := c2gDecode(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],
		"n":2,"store":true,"parallel_tool_calls":false,
		"tools":[{"type":"function","function":{"name":"f","strict":true}}],
		"extra_body":{"top_k":40},"stream_options":{"include_obfuscation":true},
		"frequency_penalty":0.5,"response_format":{"type":"json_object"}}`)
	_ = g
	fields := map[string]bool{}
	for _, l := range losses {
		fields[l.Field] = true
	}
	for _, want := range []string{"n", "store", "parallel_tool_calls", "tools[0].function.strict",
		"extra_body.top_k", "stream_options.include_obfuscation", "frequency_penalty", "response_format"} {
		if !fields[want] {
			t.Fatalf("missing loss %q in %v", want, losses)
		}
	}
	if _, _, rerr := DecodeChatForGemini([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"n":2}`), DefaultLimits()); rerr != nil {
		t.Fatalf("losses must not fail decode: %v", rerr)
	}
	// Strict policy refuses the plan before any I/O.
	if _, rerr := EnforcePolicy(losses, false); rerr == nil || rerr.Status != 400 {
		t.Fatalf("strict must refuse losses, got %v", rerr)
	}
	if _, rerr := EnforcePolicy(losses, true); rerr != nil {
		t.Fatalf("lossy must keep plan: %v", rerr)
	}
}

func TestC2GStopBounds(t *testing.T) {
	g, _ := c2gDecode(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"stop":["a","b"]}`)
	out, _ := EncodeGeminiRequest(g)
	if !strings.Contains(string(out), `"stopSequences":["a","b"]`) {
		t.Fatalf("stopSequences missing: %s", out)
	}
	_, losses := c2gDecode(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],
		"stop":["1","2","3","4","5","6"]}`)
	found := false
	for _, l := range losses {
		if l.Field == "stop" {
			found = true
		}
	}
	if !found {
		t.Fatalf(">5 stops must be a diagnosed loss, got %v", losses)
	}
}

func TestC2GHistoryIdentity(t *testing.T) {
	g, _ := c2gDecode(t, `{"model":"m","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"a\":9007199254740993}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"ok"}]}`)
	out, rerr := EncodeGeminiRequest(g)
	if rerr != nil {
		t.Fatalf("encode: %v", rerr)
	}
	s := string(out)
	if !strings.Contains(s, `"id":"call_1"`) {
		t.Fatalf("call/result IDs must be preserved exactly: %s", s)
	}
	if !strings.Contains(s, "9007199254740993") {
		t.Fatalf("args integer must survive: %s", s)
	}
	if strings.Count(s, `"functionResponse"`) != 1 {
		t.Fatalf("tool result must be one functionResponse: %s", s)
	}
	// Unmatched result fails.
	if rerr := c2gDecodeErr(t, `{"model":"m","messages":[
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_zzz","content":"ok"}]}`); rerr.Status != 400 {
		t.Fatalf("unmatched result must be 400, got %v", rerr)
	}
	// Duplicate call IDs fail.
	if rerr := c2gDecodeErr(t, `{"model":"m","messages":[{"role":"user","content":"hi"},
		{"role":"assistant","content":null,"tool_calls":[
		{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}},
		{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`); rerr.Status != 400 {
		t.Fatalf("duplicate IDs must be 400, got %v", rerr)
	}
}

func TestC2GMalformedAndUnknown(t *testing.T) {
	if rerr := c2gDecodeErr(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"temperature":"hot"}`); rerr.Status != 400 {
		t.Fatalf("quoted temperature must be 400, got %v", rerr)
	}
	if rerr := c2gDecodeErr(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"capabilities":"x"}`); rerr.Status != 400 {
		t.Fatalf("unknown field must be 400, got %v", rerr)
	}
	if rerr := c2gDecodeErr(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"metadata":{"a":1}}`); rerr.Status != 400 {
		t.Fatalf("metadata must require same-protocol route, got %v", rerr)
	}
	if rerr := c2gDecodeErr(t, `{"model":"m","messages":[{"role":"developer","content":42}]}`); rerr.Status != 400 {
		t.Fatalf("malformed developer content must be 400, got %v", rerr)
	}
	// Null store/metadata mean absent.
	if _, losses := c2gDecode(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"store":null,"metadata":null}`); len(losses) != 0 {
		t.Fatalf("nulls must be absent, got %v", losses)
	}
	// Explicit false store is exact (no storage either way).
	if _, losses := c2gDecode(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"store":false}`); len(losses) != 0 {
		t.Fatalf("store:false must be exact, got %v", losses)
	}
}
