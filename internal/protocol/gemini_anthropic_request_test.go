package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// RED: Gemini ingress decoded for the Anthropic destination.

// Plain text decodes with no losses; temperature/top_p map exactly.
func TestGeminiForAnthropicText(t *testing.T) {
	raw := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"temperature":0.5,"topP":0.9}}`)
	g, losses, rerr := DecodeGeminiForAnthropic(raw, "gemini-2.0-flash", false, DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	if g.Model != "gemini-2.0-flash" || len(g.Messages) != 1 {
		t.Fatalf("unexpected IR: %+v", g)
	}
	if g.Temperature == nil || *g.Temperature != 0.5 {
		t.Fatalf("temperature must map exactly: %+v", g.Temperature)
	}
	if g.TopP == nil || *g.TopP != 0.9 {
		t.Fatalf("top_p must map exactly: %+v", g.TopP)
	}
	if len(losses) != 0 {
		t.Fatalf("unexpected losses: %+v", losses)
	}
}

// Temperature above 1 is valid Gemini source but has no Anthropic
// representation: diagnosed as a dropped loss, never clamped.
func TestGeminiForAnthropicTemperatureAboveOne(t *testing.T) {
	for _, tc := range []struct {
		temp string
		want bool // true when the value must be dropped
	}{
		{"1", false},
		{"1.0", false},
		{"1.5", true},
		{"2", true},
	} {
		raw := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"temperature":` + tc.temp + `}}`)
		g, losses, rerr := DecodeGeminiForAnthropic(raw, "m", false, DefaultLimits())
		if rerr != nil {
			t.Fatalf("temp %s decode: %v", tc.temp, rerr)
		}
		if tc.want {
			if g.Temperature != nil {
				t.Fatalf("temp %s must be dropped, got %v", tc.temp, *g.Temperature)
			}
			if len(losses) != 1 || losses[0].Field != "generationConfig.temperature" || losses[0].Action != "dropped" {
				t.Fatalf("temp %s needs one dropped loss, got %+v", tc.temp, losses)
			}
		} else if g.Temperature == nil {
			t.Fatalf("temp %s must be kept", tc.temp)
		} else if len(losses) != 0 {
			t.Fatalf("temp %s must be lossless, got %+v", tc.temp, losses)
		}
	}
}

// topK maps exactly onto the existing IR ExtraTopK/encoder path.
func TestGeminiForAnthropicTopK(t *testing.T) {
	raw := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"topK":40}}`)
	g, losses, rerr := DecodeGeminiForAnthropic(raw, "m", false, DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	if len(losses) != 0 {
		t.Fatalf("topK must be exact, got %+v", losses)
	}
	if g.ExtraTopK == nil || *g.ExtraTopK != 40 {
		t.Fatalf("topK must reach ExtraTopK: %+v", g.ExtraTopK)
	}
	wire, rerr := EncodeAnthropicRequest(g, "upstream-model")
	if rerr != nil {
		t.Fatalf("encode: %v", rerr)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(wire, &body); err != nil {
		t.Fatalf("wire: %v", err)
	}
	var topK int
	if err := json.Unmarshal(body["top_k"], &topK); err != nil || topK != 40 {
		t.Fatalf("top_k must be 40 on the wire: %s", wire)
	}
}

// Malformed topK is a 400 before any loss accounting.
func TestGeminiForAnthropicTopKMalformed(t *testing.T) {
	for _, bad := range []string{
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"topK":-1}}`,
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"topK":"40"}}`,
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"topK":1.5}}`,
	} {
		if _, _, rerr := DecodeGeminiForAnthropic([]byte(bad), "m", false, DefaultLimits()); rerr == nil || rerr.Status != 400 {
			t.Fatalf("malformed topK must be 400, got %v", rerr)
		}
	}
}

// stopSequences within the official cap cross to Anthropic exactly.
func TestGeminiForAnthropicStopSequences(t *testing.T) {
	raw := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"stopSequences":["a","b","c","d","e"]}}`)
	g, losses, rerr := DecodeGeminiForAnthropic(raw, "m", false, DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	if len(g.Stop) != 5 || len(losses) != 0 {
		t.Fatalf("5 stop sequences must be exact: stop=%v losses=%v", g.Stop, losses)
	}
	wire, rerr := EncodeAnthropicRequest(g, "up")
	if rerr != nil {
		t.Fatalf("encode: %v", rerr)
	}
	if !strings.Contains(string(wire), `"stop_sequences":["a","b","c","d","e"]`) {
		t.Fatalf("stop_sequences must cross exactly: %s", wire)
	}
	// Empty list is an exact no-op.
	raw = []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"stopSequences":[]}}`)
	if g, losses, rerr := DecodeGeminiForAnthropic(raw, "m", false, DefaultLimits()); rerr != nil || len(g.Stop) != 0 || len(losses) != 0 {
		t.Fatalf("empty stopSequences must be exact: stop=%v losses=%v rerr=%v", g.Stop, losses, rerr)
	}
}

// More than 5 stopSequences is invalid_request in both policy modes and in
// every destination: the shared cap applies before any loss plan.
func TestGeminiStopSequencesCapAllDestinations(t *testing.T) {
	six := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"stopSequences":["1","2","3","4","5","6"]}}`
	for name, fn := range map[string]func([]byte, string, bool, Limits) (*Generation, []Loss, *RouterError){
		"chat":      DecodeGeminiRequest,
		"responses": DecodeGeminiForResponses,
		"anthropic": DecodeGeminiForAnthropic,
	} {
		if _, _, rerr := fn([]byte(six), "m", false, DefaultLimits()); rerr == nil || rerr.Status != 400 || rerr.Code != CodeInvalidRequest {
			t.Fatalf("%s: 6 stopSequences must be 400 invalid_request, got %v", name, rerr)
		}
		if _, _, rerr := fn([]byte(six), "m", true, DefaultLimits()); rerr == nil || rerr.Status != 400 {
			t.Fatalf("%s: 6 stopSequences must be 400 in stream mode, got %v", name, rerr)
		}
	}
	// Existing behavior for 0..5 is unchanged in every destination.
	five := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"stopSequences":["1","2","3","4","5"]}}`
	if g, _, rerr := DecodeGeminiRequest([]byte(five), "m", false, DefaultLimits()); rerr != nil || len(g.Stop) != 5 {
		t.Fatalf("chat 5 must stay exact: %+v %v", g, rerr)
	}
	if _, losses, rerr := DecodeGeminiForResponses([]byte(five), "m", false, DefaultLimits()); rerr != nil || len(losses) != 1 {
		t.Fatalf("responses 5 must stay one drop loss: %+v %v", losses, rerr)
	}
	nullCap := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"stopSequences":null}}`
	for name, fn := range map[string]func([]byte, string, bool, Limits) (*Generation, []Loss, *RouterError){
		"chat":      DecodeGeminiRequest,
		"responses": DecodeGeminiForResponses,
		"anthropic": DecodeGeminiForAnthropic,
	} {
		if g, losses, rerr := fn([]byte(nullCap), "m", false, DefaultLimits()); rerr != nil || len(g.Stop) != 0 || len(losses) != 0 {
			t.Fatalf("%s: null stopSequences must be absent: %+v %+v %v", name, g.Stop, losses, rerr)
		}
	}
	for _, bad := range []string{
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"stopSequences":"a"}}`,
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"stopSequences":[""]}}`,
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"stopSequences":[1]}}`,
	} {
		for name, fn := range map[string]func([]byte, string, bool, Limits) (*Generation, []Loss, *RouterError){
			"chat":      DecodeGeminiRequest,
			"responses": DecodeGeminiForResponses,
			"anthropic": DecodeGeminiForAnthropic,
		} {
			if _, _, rerr := fn([]byte(bad), "m", false, DefaultLimits()); rerr == nil || rerr.Status != 400 {
				t.Fatalf("%s: malformed stopSequences must be 400: %s -> %v", name, bad, rerr)
			}
		}
	}
}

// Absent max_tokens leaves the documented Anthropic bridge default (4096)
// to the encoder; a present cap maps exactly.
func TestGeminiForAnthropicMaxTokens(t *testing.T) {
	raw := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	g, losses, rerr := DecodeGeminiForAnthropic(raw, "m", false, DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	if g.MaxTokens != nil || len(losses) != 0 {
		t.Fatalf("absent cap must stay absent: %+v %+v", g.MaxTokens, losses)
	}
	wire, rerr := EncodeAnthropicRequest(g, "up")
	if rerr != nil {
		t.Fatalf("encode: %v", rerr)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(wire, &body); err != nil {
		t.Fatalf("wire: %v", err)
	}
	var cap int
	if err := json.Unmarshal(body["max_tokens"], &cap); err != nil || cap != DefaultAnthropicMaxTokens {
		t.Fatalf("absent cap must default to %d: %s", DefaultAnthropicMaxTokens, wire)
	}
	raw = []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":256}}`)
	if g, _, rerr := DecodeGeminiForAnthropic(raw, "m", false, DefaultLimits()); rerr != nil || g.MaxTokens == nil || *g.MaxTokens != 256 {
		t.Fatalf("present cap must map exactly: %+v %v", g, rerr)
	}
}

// History identities and exact argument literals cross the bridge.
func TestGeminiForAnthropicHistoryExact(t *testing.T) {
	raw := []byte(`{"contents":[
		{"role":"user","parts":[{"text":"q"}]},
		{"role":"model","parts":[{"functionCall":{"name":"f","args":{"n":9007199254740993},"id":"call_7"}}]},
		{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"output":"ok"},"id":"call_7"}}]}
	]}`)
	g, losses, rerr := DecodeGeminiForAnthropic(raw, "m", true, DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	if len(losses) != 0 {
		t.Fatalf("unexpected losses: %+v", losses)
	}
	if !g.Stream {
		t.Fatalf("stream mode must come from the method")
	}
	wire, rerr := EncodeAnthropicRequest(g, "up")
	if rerr != nil {
		t.Fatalf("encode: %v", rerr)
	}
	s := string(wire)
	if !strings.Contains(s, `"id":"call_7"`) || !strings.Contains(s, "9007199254740993") {
		t.Fatalf("identities and large integers must survive exactly: %s", s)
	}
}

// Tools and forced choice cross; penalties stay diagnosed losses while
// malformed source is a 400 before any I/O.
func TestGeminiForAnthropicToolsAndLosses(t *testing.T) {
	raw := []byte(`{
		"contents":[{"role":"user","parts":[{"text":"hi"}]}],
		"tools":[{"functionDeclarations":[{"name":"get_weather","parametersJsonSchema":{"type":"object"}}]}],
		"toolConfig":{"functionCallingConfig":{"mode":"ANY"}},
		"generationConfig":{"presencePenalty":0.5}
	}`)
	g, losses, rerr := DecodeGeminiForAnthropic(raw, "m", false, DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	if len(losses) != 1 || losses[0].Field != "generationConfig.presencePenalty" {
		t.Fatalf("penalty must be one diagnosed loss: %+v", losses)
	}
	if g.Choice == nil || g.Choice.Mode != "any" {
		t.Fatalf("ANY must map to any: %+v", g.Choice)
	}
	wire, rerr := EncodeAnthropicRequest(g, "up")
	if rerr != nil {
		t.Fatalf("encode: %v", rerr)
	}
	if !strings.Contains(string(wire), `"type":"any"`) {
		t.Fatalf("choice must encode: %s", wire)
	}
	for _, bad := range []string{
		`{"contents":[]}`,
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"model":"m"}`,
		`{"contents":[{"role":"user","parts":[{"functionCall":{"name":"f","args":[1]}}]}]}`,
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"temperature":99}}`,
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"bogus":1}`,
	} {
		if _, _, rerr := DecodeGeminiForAnthropic([]byte(bad), "m", false, DefaultLimits()); rerr == nil || rerr.Status != 400 {
			t.Fatalf("malformed source must be 400: %s -> %v", bad, rerr)
		}
		if _, _, rerr := DecodeGeminiForAnthropic([]byte(bad), "m", true, DefaultLimits()); rerr == nil || rerr.Status != 400 {
			t.Fatalf("malformed source must be 400 in stream mode: %s -> %v", bad, rerr)
		}
	}
}
