package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// RED: Gemini ingress decoded for the Responses destination.
func TestGeminiForResponsesText(t *testing.T) {
	raw := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"temperature":0.5}}`)
	g, losses, rerr := DecodeGeminiForResponses(raw, "gemini-2.0-flash", false, DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	if g.Model != "gemini-2.0-flash" || len(g.Messages) != 1 {
		t.Fatalf("unexpected IR: %+v", g)
	}
	if len(losses) != 0 {
		t.Fatalf("unexpected losses: %+v", losses)
	}
}

// stopSequences is validated for shape, then dropped as a diagnosed loss:
// Responses has no stop-sequence contract and nothing is forwarded.
func TestGeminiForResponsesStopSequencesDropped(t *testing.T) {
	raw := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"stopSequences":["a","b"]}}`)
	g, losses, rerr := DecodeGeminiForResponses(raw, "m", false, DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	if len(g.Stop) != 0 {
		t.Fatalf("Stop must not be set for Responses, got %v", g.Stop)
	}
	if len(losses) != 1 || losses[0].Field != "generationConfig.stopSequences" || losses[0].Action != "dropped" {
		t.Fatalf("expected one stopSequences drop loss, got %+v", losses)
	}
	// Empty list is an exact no-op: no loss, nothing forwarded.
	raw = []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"stopSequences":[]}}`)
	if _, losses, rerr := DecodeGeminiForResponses(raw, "m", false, DefaultLimits()); rerr != nil || len(losses) != 0 {
		t.Fatalf("empty stopSequences must be exact, losses=%v rerr=%v", losses, rerr)
	}
	// Malformed stopSequences is a 400 in both modes, never a loss.
	for _, bad := range []string{
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"stopSequences":"a"}}`,
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"stopSequences":[""]}}`,
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"stopSequences":[1]}}`,
	} {
		if _, _, rerr := DecodeGeminiForResponses([]byte(bad), "m", false, DefaultLimits()); rerr == nil || rerr.Status != 400 {
			t.Fatalf("malformed stopSequences must be 400, got %v", rerr)
		}
	}
}

// Omitted Gemini function strict is the known non-strict default: the
// Responses encoder emits explicit strict:false with no invented loss.
func TestGeminiForResponsesToolsEncodeExplicitFalse(t *testing.T) {
	raw := []byte(`{
		"contents":[{"role":"user","parts":[{"text":"hi"}]}],
		"tools":[{"functionDeclarations":[{"name":"get_weather","parametersJsonSchema":{"type":"object"}}]}],
		"toolConfig":{"functionCallingConfig":{"mode":"ANY"}}
	}`)
	g, losses, rerr := DecodeGeminiForResponses(raw, "m", false, DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	if len(losses) != 0 {
		t.Fatalf("unexpected losses: %+v", losses)
	}
	if len(g.Tools) != 1 || g.Tools[0].Strict != nil {
		t.Fatalf("Gemini tools carry no strict slot, got %+v", g.Tools)
	}
	if g.Choice == nil || g.Choice.Mode != "any" {
		t.Fatalf("ANY must map to any, got %+v", g.Choice)
	}
	wire, rerr := EncodeResponsesRequest(g, AbsentEffort(), "upstream-model")
	if rerr != nil {
		t.Fatalf("encode: %v", rerr)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(wire, &body); err != nil {
		t.Fatalf("wire: %v", err)
	}
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(body["tools"], &tools); err != nil || len(tools) != 1 {
		t.Fatalf("wire tools: %s", wire)
	}
	var strictVal *bool
	if err := json.Unmarshal(tools[0]["strict"], &strictVal); err != nil || strictVal == nil || *strictVal {
		t.Fatalf("wire strict must be explicit false: %s", wire)
	}
	var choice string
	if err := json.Unmarshal(body["tool_choice"], &choice); err != nil || choice != "required" {
		t.Fatalf("any must encode as required: %s", wire)
	}
}

// History identities and exact argument literals cross the bridge.
func TestGeminiForResponsesHistoryExact(t *testing.T) {
	raw := []byte(`{"contents":[
		{"role":"user","parts":[{"text":"q"}]},
		{"role":"model","parts":[{"functionCall":{"name":"f","args":{"n":9007199254740993},"id":"call_7"}}]},
		{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"output":"ok"},"id":"call_7"}}]}
	]}`)
	g, losses, rerr := DecodeGeminiForResponses(raw, "m", true, DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	if len(losses) != 0 {
		t.Fatalf("unexpected losses: %+v", losses)
	}
	if !g.Stream {
		t.Fatalf("stream mode must come from the method")
	}
	wire, rerr := EncodeResponsesRequest(g, AbsentEffort(), "up")
	if rerr != nil {
		t.Fatalf("encode: %v", rerr)
	}
	s := string(wire)
	if !strings.Contains(s, `"call_id":"call_7"`) || !strings.Contains(s, "9007199254740993") {
		t.Fatalf("identities and large integers must survive exactly: %s", s)
	}
}

// Source-malformed bodies are 400 in both modes.
func TestGeminiForResponsesMalformed(t *testing.T) {
	for _, bad := range []string{
		`{"contents":[]}`,
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"model":"m"}`,
		`{"contents":[{"role":"user","parts":[{"functionCall":{"name":"f","args":[1]}}]}]}`,
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"temperature":99}}`,
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"bogus":1}`,
	} {
		if _, _, rerr := DecodeGeminiForResponses([]byte(bad), "m", false, DefaultLimits()); rerr == nil || rerr.Status != 400 {
			t.Fatalf("malformed must be 400: %s -> %v", bad, rerr)
		}
	}
}

// Chat-worded losses are reworded for Responses; Chat behavior is exact.
func TestGeminiForResponsesLossWording(t *testing.T) {
	raw := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],
		"generationConfig":{"seed":1,"thinkingConfig":{"thinkingBudget":100}}}`)
	_, rlosses, rerr := DecodeGeminiForResponses(raw, "m", false, DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	for _, l := range rlosses {
		if strings.Contains(l.Reason, "Chat") {
			t.Fatalf("Responses losses must not name Chat: %+v", l)
		}
	}
	_, closses, rerr := DecodeGeminiRequest(raw, "m", false, DefaultLimits())
	if rerr != nil {
		t.Fatalf("chat decode: %v", rerr)
	}
	if len(rlosses) != len(closses) {
		t.Fatalf("loss counts must agree: %v vs %v", rlosses, closses)
	}
}
