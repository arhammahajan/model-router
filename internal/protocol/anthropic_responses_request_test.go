package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustDecodeAnthropicForResponses(t *testing.T, raw string) (*Generation, []Loss) {
	t.Helper()
	g, losses, rerr := DecodeAnthropicForResponses([]byte(raw), DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode failed: %+v", rerr)
	}
	return g, losses
}

func mustFailAnthropicForResponses(t *testing.T, raw string, wantStatus int, wantCode string) *RouterError {
	t.Helper()
	_, _, rerr := DecodeAnthropicForResponses([]byte(raw), DefaultLimits())
	if rerr == nil {
		t.Fatalf("expected failure for %s", raw)
	}
	if rerr.Status != wantStatus || rerr.Code != wantCode {
		t.Fatalf("bad error: %+v (want %d/%s)", rerr, wantStatus, wantCode)
	}
	return rerr
}

func lossFields(losses []Loss) map[string]bool {
	m := map[string]bool{}
	for _, l := range losses {
		m[l.Field] = true
	}
	return m
}

func TestAFRBasicText(t *testing.T) {
	g, losses := mustDecodeAnthropicForResponses(t, `{"model":"claude-x","max_tokens":64,
		"system":"Be brief.","messages":[{"role":"user","content":"What time is it?"}],
		"temperature":0.5,"top_p":0.9}`)
	if len(losses) != 0 {
		t.Fatalf("unexpected losses: %v", losses)
	}
	if g.Model != "claude-x" {
		t.Fatalf("bad model: %+v", g)
	}
	if len(g.Messages) != 2 || g.Messages[0].Role != RoleSystem || g.Messages[1].Role != RoleUser {
		t.Fatalf("bad messages: %+v", g.Messages)
	}
	if g.MaxTokens == nil || *g.MaxTokens != 64 {
		t.Fatalf("bad max tokens: %+v", g.MaxTokens)
	}
	if g.Temperature == nil || *g.Temperature != 0.5 {
		t.Fatalf("bad temperature: %+v", g.Temperature)
	}
	// Encode to the Responses wire and assert the stable contract.
	out, rerr := EncodeResponsesRequest(g, AbsentEffort(), "upstream-model")
	if rerr != nil {
		t.Fatalf("encode: %v", rerr)
	}
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "upstream-model" || body["store"] != false {
		t.Fatalf("bad envelope: %v", body)
	}
	if _, ok := body["stop_sequences"]; ok {
		t.Fatalf("stop must never reach Responses: %v", body)
	}
	if body["max_output_tokens"] != float64(64) || body["temperature"] != 0.5 {
		t.Fatalf("bad controls: %v", body)
	}
	inp, ok := body["input"].([]any)
	if !ok || len(inp) != 2 {
		t.Fatalf("bad input: %v", body)
	}
}

func TestAFRRequestKindAssertions(t *testing.T) {
	base := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}]%s}`
	mustFailAnthropicForResponses(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, 400, CodeInvalidRequest)
	mustFailAnthropicForResponses(t, `{"model":"m","max_tokens":8,"messages":[]}`, 400, CodeInvalidRequest)
	mustFailAnthropicForResponses(t, strings.Replace(base, "%s", `,"temperature":"0.5"`, 1), 400, CodeInvalidRequest)
	mustFailAnthropicForResponses(t, strings.Replace(base, "%s", `,"temperature":1.5`, 1), 400, CodeInvalidRequest)
	mustFailAnthropicForResponses(t, strings.Replace(base, "%s", `,"max_tokens":0`, 1), 400, CodeInvalidRequest)
	mustFailAnthropicForResponses(t, strings.Replace(base, "%s", `,"top_k":"5"`, 1), 400, CodeInvalidRequest)
	mustFailAnthropicForResponses(t, strings.Replace(base, "%s", `,"stop_sequences":"x"`, 1), 400, CodeInvalidRequest)
	mustFailAnthropicForResponses(t, strings.Replace(base, "%s", `,"stop_sequences":[1]`, 1), 400, CodeInvalidRequest)
	mustFailAnthropicForResponses(t, strings.Replace(base, "%s", `,"mystery":1`, 1), 400, CodeInvalidGeneration)
	mustFailAnthropicForResponses(t, strings.Replace(base, "%s", `,"stream":null`, 1), 400, CodeInvalidRequest)
	mustFailAnthropicForResponses(t, strings.Replace(base, "%s", `,"thinking":{"type":"enabled","budget_tokens":100}`, 1), 400, CodeInvalidGeneration)
	mustFailAnthropicForResponses(t, strings.Replace(base, "%s", `,"thinking":{"type":"disabled","budget_tokens":5}`, 1), 400, CodeInvalidRequest)
	mustFailAnthropicForResponses(t, strings.Replace(base, "%s", `,"service_tier":"platinum"`, 1), 400, CodeInvalidRequest)
	// Unknown nested member inside a text block is a validation error.
	mustFailAnthropicForResponses(t, `{"model":"m","max_tokens":8,"messages":[{"role":"user",
		"content":[{"type":"text","text":"hi","bogus":1}]}]}`, 400, CodeInvalidGeneration)
	// Duplicate tool identities and dangling results are validation errors.
	mustFailAnthropicForResponses(t, `{"model":"m","max_tokens":8,"messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"t","input":{}}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"t","input":{}}]}],
		"tools":[{"name":"t","input_schema":{"type":"object"},"strict":false}]}`,
		400, CodeInvalidRequest)
	mustFailAnthropicForResponses(t, `{"model":"m","max_tokens":8,"messages":[
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"nope","content":"x"}]}]}`,
		400, CodeInvalidRequest)
}

func TestAFRLossesDestinationSpecific(t *testing.T) {
	g, losses := mustDecodeAnthropicForResponses(t, `{"model":"m","max_tokens":8,
		"messages":[{"role":"user","content":"hi"}],
		"top_k":5,"stop_sequences":["halt"],"thinking":{"type":"disabled"},
		"metadata":{"user_id":"u1"},"service_tier":"auto"}`)
	fields := lossFields(losses)
	for _, f := range []string{"top_k", "stop_sequences", "metadata", "service_tier"} {
		if !fields[f] {
			t.Fatalf("missing loss %s: %v", f, losses)
		}
	}
	if fields["thinking"] || !g.Reasoning.IsNone() {
		t.Fatalf("disabled thinking is retained for the shared planner: %v %+v", losses, g.Reasoning)
	}
	if len(g.Stop) != 0 {
		t.Fatalf("stop must be dropped for Responses, got %v", g.Stop)
	}
	if _, rerr := EnforcePolicy(losses, false); rerr == nil {
		t.Fatalf("strict must refuse planned losses")
	}
	kept, rerr := EnforcePolicy(losses, true)
	if rerr != nil || len(kept) != len(losses) {
		t.Fatalf("lossy must keep plan: %v %v", kept, rerr)
	}
	out, rerr := EncodeResponsesRequest(g, AbsentEffort(), "up")
	if rerr != nil {
		t.Fatalf("encode: %v", rerr)
	}
	if strings.Contains(string(out), "halt") || strings.Contains(string(out), "top_k") {
		t.Fatalf("lossy values leaked to wire: %s", out)
	}
}

func TestAFRStrictRetention(t *testing.T) {
	// Explicit true/false map exactly onto the Responses contract.
	g, losses := mustDecodeAnthropicForResponses(t, `{"model":"m","max_tokens":8,
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"t1","input_schema":{"type":"object"},"strict":true},
		         {"name":"t2","input_schema":{"type":"object"},"strict":false}]}`)
	if len(losses) != 0 {
		t.Fatalf("explicit strict must be exact, no loss: %v", losses)
	}
	if g.Tools[0].Strict == nil || !*g.Tools[0].Strict {
		t.Fatalf("strict true not retained: %+v", g.Tools[0])
	}
	if g.Tools[1].Strict == nil || *g.Tools[1].Strict {
		t.Fatalf("strict false not retained: %+v", g.Tools[1])
	}
	out, rerr := EncodeResponsesRequest(g, AbsentEffort(), "up")
	if rerr != nil {
		t.Fatalf("encode: %v", rerr)
	}
	var body struct {
		Tools []struct {
			Name   string `json:"name"`
			Strict bool   `json:"strict"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Tools) != 2 || !body.Tools[0].Strict || body.Tools[1].Strict {
		t.Fatalf("strict not honored on wire: %s", out)
	}
	// Omitted strict is the known Anthropic non-strict default: exact
	// mapping to explicit false on the wire, no diagnosed loss. Strict
	// mode must accept ordinary tools without a strict member.
	g2, losses2 := mustDecodeAnthropicForResponses(t, `{"model":"m","max_tokens":8,
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"t","input_schema":{"type":"object"}}]}`)
	if len(losses2) != 0 {
		t.Fatalf("omitted strict must be exact, no loss: %v", losses2)
	}
	if g2.Tools[0].Strict == nil || *g2.Tools[0].Strict {
		t.Fatalf("omitted strict must default to explicit false: %+v", g2.Tools[0])
	}
	if _, rerr := EnforcePolicy(losses2, false); rerr != nil {
		t.Fatalf("strict must accept omitted-strict: %v", rerr)
	}
	out2, rerr := EncodeResponsesRequest(g2, AbsentEffort(), "up")
	if rerr != nil {
		t.Fatalf("encode: %v", rerr)
	}
	if !strings.Contains(string(out2), `"strict":false`) {
		t.Fatalf("omitted strict must emit explicit false: %s", out2)
	}
	// Explicit null is also the non-strict default: exact, no loss.
	g3, losses3 := mustDecodeAnthropicForResponses(t, `{"model":"m","max_tokens":8,
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"t","input_schema":{"type":"object"},"strict":null}]}`)
	if len(losses3) != 0 {
		t.Fatalf("null strict must be exact, no loss: %v", losses3)
	}
	if g3.Tools[0].Strict == nil || *g3.Tools[0].Strict {
		t.Fatalf("null strict must default to explicit false: %+v", g3.Tools[0])
	}
	mustFailAnthropicForResponses(t, `{"model":"m","max_tokens":8,
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"t","input_schema":{"type":"object"},"strict":"yes"}]}`,
		400, CodeInvalidRequest)
}

func TestAFRToolChoiceAndParallel(t *testing.T) {
	for _, tc := range []struct{ in, mode, name string }{
		{`{"type":"auto"}`, "auto", ""},
		{`{"type":"any"}`, "any", ""},
		{`{"type":"none"}`, "none", ""},
		{`{"type":"tool","name":"t"}`, "specific", "t"},
	} {
		g, losses := mustDecodeAnthropicForResponses(t, `{"model":"m","max_tokens":8,
			"messages":[{"role":"user","content":"hi"}],
			"tools":[{"name":"t","input_schema":{"type":"object"},"strict":false}],
			"tool_choice":`+tc.in+`}`)
		if len(losses) != 0 {
			t.Fatalf("%s: unexpected losses %v", tc.in, losses)
		}
		if g.Choice == nil || g.Choice.Mode != tc.mode || g.Choice.Name != tc.name {
			t.Fatalf("%s: bad choice %+v", tc.in, g.Choice)
		}
	}
	g, losses := mustDecodeAnthropicForResponses(t, `{"model":"m","max_tokens":8,
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"t","input_schema":{"type":"object"},"strict":false}],
		"tool_choice":{"type":"auto","disable_parallel_tool_use":true}}`)
	if len(losses) != 0 {
		t.Fatalf("unexpected losses: %v", losses)
	}
	if g.ParallelToolCalls == nil || *g.ParallelToolCalls {
		t.Fatalf("parallel disable not carried: %+v", g.ParallelToolCalls)
	}
	out, rerr := EncodeResponsesRequest(g, AbsentEffort(), "up")
	if rerr != nil {
		t.Fatalf("encode: %v", rerr)
	}
	if !strings.Contains(string(out), `"parallel_tool_calls":false`) || !strings.Contains(string(out), `"auto"`) {
		t.Fatalf("choice/parallel not on wire: %s", out)
	}
	// Named choice for an unknown tool, forced choice without tools.
	mustFailAnthropicForResponses(t, `{"model":"m","max_tokens":8,
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"t","input_schema":{"type":"object"},"strict":false}],
		"tool_choice":{"type":"tool","name":"nope"}}`, 400, CodeInvalidRequest)
	mustFailAnthropicForResponses(t, `{"model":"m","max_tokens":8,
		"messages":[{"role":"user","content":"hi"}],
		"tool_choice":{"type":"any"}}`, 400, CodeInvalidRequest)
	// Dangling auto without tools is a diagnosed drop, not a 400.
	g2, losses2 := mustDecodeAnthropicForResponses(t, `{"model":"m","max_tokens":8,
		"messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"auto"}}`)
	if !lossFields(losses2)["tool_choice"] || g2.Choice != nil {
		t.Fatalf("dangling auto must drop with loss: %+v %v", g2.Choice, losses2)
	}
}

func TestAFRHistoryAndFidelity(t *testing.T) {
	g, losses := mustDecodeAnthropicForResponses(t, `{"model":"m","max_tokens":8,
		"messages":[
			{"role":"user","content":"calc 1+1"},
			{"role":"assistant","content":[
				{"type":"text","text":"thinking"},
				{"type":"tool_use","id":"call_big","name":"calc","input":{"n":9007199254740993,"f":1.50}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_big","content":"2"}]},
			{"role":"assistant","content":[{"type":"tool_use","id":"call_empty","name":"noop"}]}
		],
		"tools":[{"name":"calc","input_schema":{"type":"object"},"strict":false},
		         {"name":"noop","input_schema":{"type":"object"},"strict":false}]}`)
	if len(losses) != 0 {
		t.Fatalf("unexpected losses: %v", losses)
	}
	args := string(g.Messages[1].ToolCalls[0].Arguments)
	if !strings.Contains(args, "9007199254740993") {
		t.Fatalf("large integer not preserved: %s", args)
	}
	if !strings.Contains(args, "1.50") {
		t.Fatalf("numeric literal not preserved: %s", args)
	}
	if string(g.Messages[3].ToolCalls[0].Arguments) != "{}" {
		t.Fatalf("empty input must be {}: %s", g.Messages[3].ToolCalls[0].Arguments)
	}
	if g.Messages[2].Results[0].Content != "2" || g.Messages[2].Results[0].CallID != "call_big" {
		t.Fatalf("bad result: %+v", g.Messages[2].Results)
	}
	// Array-of-blocks tool content concatenates exactly once (approximated).
	g2, losses2 := mustDecodeAnthropicForResponses(t, `{"model":"m","max_tokens":8,
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"t","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1",
				"content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}]}],
		"tools":[{"name":"t","input_schema":{"type":"object"},"strict":false}]}`)
	if !lossFields(losses2)["messages[1].content[0].content"] {
		t.Fatalf("block concat must be diagnosed: %v", losses2)
	}
	if g2.Messages[1].Results[0].Content != "ab" {
		t.Fatalf("concat content wrong: %+v", g2.Messages[1].Results)
	}
	// Mixed order is refused in both modes.
	mustFailAnthropicForResponses(t, `{"model":"m","max_tokens":8,"messages":[
		{"role":"assistant","content":[
			{"type":"text","text":"a"},
			{"type":"tool_use","id":"c1","name":"t","input":{}},
			{"type":"text","text":"b"}]}],
		"tools":[{"name":"t","input_schema":{"type":"object"},"strict":false}]}`,
		400, CodeInvalidGeneration)
	mustFailAnthropicForResponses(t, `{"model":"m","max_tokens":8,"messages":[
		{"role":"user","content":[
			{"type":"text","text":"a"},
			{"type":"tool_result","tool_use_id":"c1","content":"x"}]}]}`,
		400, CodeInvalidGeneration)
}

func TestAFRNativeToolsRefused(t *testing.T) {
	// Versioned/server tools have no bridge mapping in either mode.
	mustFailAnthropicForResponses(t, `{"model":"m","max_tokens":8,
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"web_search_20260210","name":"s"}]}`, 400, CodeInvalidGeneration)
	mustFailAnthropicForResponses(t, `{"model":"m","max_tokens":8,"messages":[
		{"role":"assistant","content":[
			{"type":"tool_use","id":"c1","name":"bash","input":{},"caller":{"type":"direct"}}]}]}`,
		400, CodeInvalidGeneration)
}
