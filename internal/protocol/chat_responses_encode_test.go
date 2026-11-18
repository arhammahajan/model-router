package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestREncRequestWire(t *testing.T) {
	g, _, rerr := DecodeChatForResponses([]byte(`{"model":"public","messages":[
		{"role":"system","content":"Be brief."},
		{"role":"user","content":"What time is it?"},
		{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_time","arguments":"{\"tz\":\"UTC\"}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"noon"}],
		"tools":[{"type":"function","function":{"name":"get_time","description":"clock","parameters":{"type":"object","properties":{"tz":{"type":"string"}}}}}],
		"tool_choice":{"type":"function","function":{"name":"get_time"}},
		"parallel_tool_calls":false,"temperature":0.5,"max_tokens":64}`), DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	out, rerr := EncodeResponsesRequest(g, AbsentEffort(), "upstream-model")
	if rerr != nil {
		t.Fatalf("encode: %v", rerr)
	}
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "upstream-model" {
		t.Fatalf("model not replaced: %v", body["model"])
	}
	if body["store"] != false {
		t.Fatalf("stateless store=false must be explicit: %v", body["store"])
	}
	if _, ok := body["extra_body"]; ok {
		t.Fatalf("no SDK extra_body wrapper on wire: %v", body)
	}
	if _, ok := body["conversation"]; ok {
		t.Fatalf("no conversation store references: %v", body)
	}
	if body["tool_choice"] == nil {
		t.Fatalf("tool_choice missing: %s", out)
	}
	tc := body["tool_choice"].(map[string]any)
	if tc["type"] != "function" || tc["name"] != "get_time" {
		t.Fatalf("named tool_choice must be flat Responses form: %v", tc)
	}
	if body["max_output_tokens"] != float64(64) {
		t.Fatalf("max_tokens must map to max_output_tokens: %v", body["max_output_tokens"])
	}
	s := string(out)
	if !strings.Contains(s, `"call_id":"call_1"`) || !strings.Contains(s, `"function_call_output"`) {
		t.Fatalf("history call_id/order not preserved: %s", s)
	}
	if !strings.Contains(s, `"input_text"`) {
		t.Fatalf("message content must use input_text: %s", s)
	}
	// Large integers survive exactly.
	g2, _, _ := DecodeChatForResponses([]byte(`{"model":"m","messages":[{"role":"assistant","content":"","tool_calls":[{"id":"call-9","type":"function","function":{"name":"calc","arguments":"{\"n\": 9007199254740993}"}}]}]}`), DefaultLimits())
	out2, rerr := EncodeResponsesRequest(g2, AbsentEffort(), "up")
	if rerr != nil {
		t.Fatalf("encode2: %v", rerr)
	}
	if !strings.Contains(string(out2), "9007199254740993") {
		t.Fatalf("large integer not preserved: %s", out2)
	}
}

func TestREncToolChoiceStrings(t *testing.T) {
	for chat, want := range map[string]string{"none": "none", "auto": "auto", "required": "required"} {
		g, _, rerr := DecodeChatForResponses([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],
			"tools":[{"type":"function","function":{"name":"t","parameters":{"type":"object"}}}],"tool_choice":"`+chat+`"}`), DefaultLimits())
		if rerr != nil {
			t.Fatalf("decode %s: %v", chat, rerr)
		}
		out, rerr := EncodeResponsesRequest(g, AbsentEffort(), "up")
		if rerr != nil {
			t.Fatalf("encode %s: %v", chat, rerr)
		}
		if !strings.Contains(string(out), `"`+want+`"`) {
			t.Fatalf("choice %s must map to %s: %s", chat, want, out)
		}
	}
}
