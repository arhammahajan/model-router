package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustDecodeForResponses(t *testing.T, raw string) (*Generation, []Loss) {
	t.Helper()
	g, losses, rerr := DecodeChatForResponses([]byte(raw), DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode failed: %v", rerr)
	}
	return g, losses
}

func TestCFRTextRequest(t *testing.T) {
	g, losses := mustDecodeForResponses(t, `{"model":"m","messages":[
		{"role":"system","content":"You are helpful."},
		{"role":"user","content":"Hi"}],
		"temperature":0.5,"max_tokens":64,"stream":false}`)
	if len(losses) != 0 {
		t.Fatalf("unexpected losses: %v", losses)
	}
	if g.Model != "m" || len(g.Messages) != 2 {
		t.Fatalf("bad decode: %+v", g)
	}
	if g.Temperature == nil || *g.Temperature != 0.5 {
		t.Fatalf("bad temperature: %+v", g.Temperature)
	}
	if g.MaxTokens == nil || *g.MaxTokens != 64 {
		t.Fatalf("bad max tokens: %+v", g.MaxTokens)
	}
}

func TestCFRStorePolicy(t *testing.T) {
	// Explicit false matches the forced stateless wire exactly.
	g, losses := mustDecodeForResponses(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"store":false}`)
	if len(losses) != 0 || g == nil {
		t.Fatalf("store=false must be exact: %+v", losses)
	}
	// True would be changed by forcing store=false: diagnosed loss.
	_, losses = mustDecodeForResponses(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"store":true}`)
	if len(losses) != 1 || losses[0].Field != "store" {
		t.Fatalf("store=true must be a loss: %+v", losses)
	}
	if _, rerr := EnforcePolicy(losses, false); rerr == nil || rerr.Code != CodeTranslationLoss {
		t.Fatalf("strict must refuse store change, got %v", rerr)
	}
	// Malformed store is 400 even lossy.
	_, _, rerr := DecodeChatForResponses([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"store":"yes"}`), DefaultLimits())
	if rerr == nil || rerr.Status != 400 {
		t.Fatalf("malformed store must 400, got %v", rerr)
	}
}

func TestCFRStrictToolChoice(t *testing.T) {
	g, losses := mustDecodeForResponses(t, `{"model":"m","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_time","arguments":"{\"tz\":\"UTC\"}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"noon"}],
		"tools":[{"type":"function","function":{"name":"get_time","parameters":{"type":"object"}}}],
		"tool_choice":{"type":"function","function":{"name":"get_time"}},"parallel_tool_calls":false}`)
	if len(losses) != 0 {
		t.Fatalf("unexpected losses: %v", losses)
	}
	if g.Choice == nil || g.Choice.Mode != "specific" || g.Choice.Name != "get_time" {
		t.Fatalf("bad choice: %+v", g.Choice)
	}
	// Explicit strict has no IR slot: diagnosed, never silent.
	_, losses = mustDecodeForResponses(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"t","parameters":{"type":"object"},"strict":true}}]}`)
	if len(losses) != 1 || !strings.Contains(losses[0].Field, "strict") {
		t.Fatalf("strict must be a loss: %+v", losses)
	}
}

func TestCFRRejectsUnknownAndMalformed(t *testing.T) {
	must400 := func(raw string) {
		t.Helper()
		if _, _, rerr := DecodeChatForResponses([]byte(raw), DefaultLimits()); rerr == nil || rerr.Status != 400 {
			t.Fatalf("expected 400 for %s, got %v", raw, rerr)
		}
	}
	must400(`{"model":"m","messages":[],"nonsense_field":1}`)
	must400(`{"model":"m","messages":[],"metadata":{"a":"b"}}`)
	must400(`{"model":"m","messages":[{"role":"user","content":"hi"}],"seed":"7"}`)
	must400(`{"model":"m","messages":[{"role":"developer","content":42}]}`)
	must400(`{"model":"m","messages":[{"role":"user","content":[{"type":"file","file":{"a":1}}]}]}`)
	// Well-formed unmapped controls are losses, not errors.
	_, losses := mustDecodeForResponses(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"seed":7,"stop":"END"}`)
	if len(losses) != 2 {
		t.Fatalf("expected seed+stop losses, got %v", losses)
	}
	var out []byte
	_ = out
	_ = json.Marshal
}
