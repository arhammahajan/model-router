package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustDecodeAnthropic(t *testing.T, raw string) (*Generation, []Loss) {
	t.Helper()
	g, losses, rerr := DecodeAnthropicRequest([]byte(raw), DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode failed: %v", rerr)
	}
	return g, losses
}

func decodeAnthropicExpect(t *testing.T, raw string, wantStatus int) {
	t.Helper()
	_, _, rerr := DecodeAnthropicRequest([]byte(raw), DefaultLimits())
	if rerr == nil {
		t.Fatalf("expected status %d, got success for %s", wantStatus, raw)
	}
	if rerr.Status != wantStatus {
		t.Fatalf("expected status %d, got %d (%s) for %s", wantStatus, rerr.Status, rerr.Msg, raw)
	}
}

func TestAnthropicDecodeTextRequest(t *testing.T) {
	// Corrected: top_k has no portable Chat wire mapping and is a diagnosed
	// loss (previously asserted lossless ExtraTopK forwarding). Lossless
	// decode therefore omits top_k; top_k loss is covered in the reverse
	// regression suite.
	g, losses := mustDecodeAnthropic(t, `{"model":"m","max_tokens":64,
		"system":"You are helpful.",
		"messages":[{"role":"user","content":"Hi"}],
		"temperature":0.5,"top_p":0.9,
		"stop_sequences":["END"],"stream":false}`)
	if len(losses) != 0 {
		t.Fatalf("unexpected losses: %v", losses)
	}
	if g.Model != "m" || len(g.Messages) != 2 {
		t.Fatalf("bad decode: %+v", g)
	}
	if g.Messages[0].Role != RoleSystem || g.Messages[1].Role != RoleUser {
		t.Fatalf("roles wrong: %+v", g.Messages)
	}
	if g.Temperature == nil || *g.Temperature != 0.5 {
		t.Fatalf("bad temperature: %+v", g.Temperature)
	}
	if g.ExtraTopK != nil {
		t.Fatalf("top_k must not forward to the Chat bridge: %+v", g.ExtraTopK)
	}
	if g.MaxTokens == nil || *g.MaxTokens != 64 {
		t.Fatalf("bad max_tokens: %+v", g.MaxTokens)
	}
	if len(g.Stop) != 1 || g.Stop[0] != "END" {
		t.Fatalf("bad stop: %+v", g.Stop)
	}
}

func TestAnthropicDecodeRequiresMaxTokens(t *testing.T) {
	decodeAnthropicExpect(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, 400)
	decodeAnthropicExpect(t, `{"model":"m","max_tokens":null,"messages":[{"role":"user","content":"hi"}]}`, 400)
	decodeAnthropicExpect(t, `{"model":"m","max_tokens":0,"messages":[{"role":"user","content":"hi"}]}`, 400)
	decodeAnthropicExpect(t, `{"model":"m","max_tokens":-1,"messages":[{"role":"user","content":"hi"}]}`, 400)
	decodeAnthropicExpect(t, `{"model":"m","max_tokens":1.5,"messages":[{"role":"user","content":"hi"}]}`, 400)
	decodeAnthropicExpect(t, `{"model":"m","max_tokens":"64","messages":[{"role":"user","content":"hi"}]}`, 400)
}

func TestAnthropicDecodeRejectsUnknownField(t *testing.T) {
	decodeAnthropicExpect(t, `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"nonsense_field":1}`, 400)
}

func TestAnthropicDecodeStreamBool(t *testing.T) {
	decodeAnthropicExpect(t, `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"stream":null}`, 400)
	decodeAnthropicExpect(t, `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"stream":"yes"}`, 400)
}

func TestAnthropicDecodeToolsRoundTrip(t *testing.T) {
	g, losses := mustDecodeAnthropic(t, `{"model":"m","max_tokens":64,
		"messages":[
			{"role":"user","content":"What time?"},
			{"role":"assistant","content":[
				{"type":"text","text":"Let me check."},
				{"type":"tool_use","id":"call_1","name":"get_time","input":{"tz":"UTC"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"noon"}]}],
		"tools":[{"name":"get_time","description":"clock",
			"input_schema":{"type":"object","properties":{"tz":{"type":"string"}}}}],
		"tool_choice":{"type":"tool","name":"get_time","disable_parallel_tool_use":true}}`)
	if len(losses) != 0 {
		t.Fatalf("unexpected losses: %v", losses)
	}
	if !g.HasTools || len(g.Tools) != 1 || g.Tools[0].Name != "get_time" {
		t.Fatalf("tools wrong: %+v", g.Tools)
	}
	if g.Choice == nil || g.Choice.Mode != "specific" || g.Choice.Name != "get_time" {
		t.Fatalf("choice wrong: %+v", g.Choice)
	}
	if g.ParallelToolCalls == nil || *g.ParallelToolCalls {
		t.Fatalf("parallel mapping wrong: %+v", g.ParallelToolCalls)
	}
	if len(g.Messages) != 3 {
		t.Fatalf("messages wrong: %+v", g.Messages)
	}
	// Identity preserved across history.
	foundCall, foundResult := false, false
	for _, m := range g.Messages {
		for _, tc := range m.ToolCalls {
			if tc.ID == "call_1" && tc.Name == "get_time" && string(tc.Arguments) == `{"tz":"UTC"}` {
				foundCall = true
			}
		}
		for _, r := range m.Results {
			if r.CallID == "call_1" && r.Content == "noon" && !r.IsError {
				foundResult = true
			}
		}
	}
	if !foundCall || !foundResult {
		t.Fatalf("identities lost: %+v", g.Messages)
	}
}

func TestAnthropicDecodeChoiceModes(t *testing.T) {
	tools := `"tools":[{"name":"a","input_schema":{"type":"object"}}]`
	for tc, mode := range map[string]string{
		`{"type":"auto"}`: "auto", `{"type":"any"}`: "any", `{"type":"none"}`: "none",
	} {
		g, _ := mustDecodeAnthropic(t, `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],`+tools+`,"tool_choice":`+tc+`}`)
		if g.Choice == nil || g.Choice.Mode != mode {
			t.Fatalf("%s mapped to %+v", tc, g.Choice)
		}
	}
	// disable_parallel_tool_use=false is the default: no control emitted.
	g, losses := mustDecodeAnthropic(t, `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],`+tools+`,"tool_choice":{"type":"auto","disable_parallel_tool_use":false}}`)
	if len(losses) != 0 || g.ParallelToolCalls != nil {
		t.Fatalf("false disable must be exact default: %+v %v", g.ParallelToolCalls, losses)
	}
	// A dangling auto choice without tools is dropped with a diagnostic.
	g, losses = mustDecodeAnthropic(t, `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"auto"}}`)
	if len(losses) != 1 || g.Choice != nil {
		t.Fatalf("dangling choice must be dropped with a loss: %+v %v", g.Choice, losses)
	}
	if _, rerr := EnforcePolicy(losses, false); rerr == nil || rerr.Code != CodeTranslationLoss {
		t.Fatalf("strict must refuse, got %v", rerr)
	}
	decodeAnthropicExpect(t, `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],`+tools+`,"tool_choice":{"type":"tool","name":"ghost"}}`, 400)
	decodeAnthropicExpect(t, `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"tool","name":"ghost"}}`, 400)
}

func TestAnthropicDecodeRejectsMixedOrder(t *testing.T) {
	// Text after a tool_use cannot be preserved by the Chat destination.
	decodeAnthropicExpect(t, `{"model":"m","max_tokens":8,"messages":[
		{"role":"assistant","content":[
			{"type":"tool_use","id":"c1","name":"a","input":{}},
			{"type":"text","text":"late"}]}]}`, 400)
	// Text mixed with tool results cannot be split in a guessed order.
	decodeAnthropicExpect(t, `{"model":"m","max_tokens":8,"messages":[
		{"role":"user","content":[
			{"type":"text","text":"here"},
			{"type":"tool_result","tool_use_id":"c1","content":"x"}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"a","input":{}}]}]}`, 400)
	// ... even in lossy mode: mixed order is a validation error, not a loss.
	raw := `{"model":"m","max_tokens":8,"messages":[
		{"role":"assistant","content":[
			{"type":"text","text":"first"},
			{"type":"tool_use","id":"c1","name":"a","input":{}},
			{"type":"text","text":"second"}]}]}`
	_, losses, rerr := DecodeAnthropicRequest([]byte(raw), DefaultLimits())
	if rerr == nil || rerr.Status != 400 {
		t.Fatalf("mixed order must 400 in lossy-capable decode, got %v (%v)", rerr, losses)
	}
}

func TestAnthropicDecodeHistoryIdentity(t *testing.T) {
	call := `{"role":"assistant","content":[{"type":"tool_use","id":"call-1","name":"a","input":{}}]}`
	hist := func(body string) string {
		return `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"},` + call + `,` + body + `]}`
	}
	decodeAnthropicExpect(t, hist(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-9","content":"x"}]}`), 400)
	decodeAnthropicExpect(t, `{"model":"m","max_tokens":8,"messages":[{"role":"assistant","content":[
		{"type":"tool_use","id":"call-1","name":"a","input":{}},
		{"type":"tool_use","id":"call-1","name":"b","input":{}}]}]}`, 400)
	decodeAnthropicExpect(t, hist(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":"x"},{"type":"tool_result","tool_use_id":"call-1","content":"y"}]}`), 400)
	decodeAnthropicExpect(t, `{"model":"m","max_tokens":8,"messages":[{"role":"assistant","content":[
		{"type":"tool_use","name":"a","input":{}}]}]}`, 400)
}

func TestAnthropicDecodeLossyRejectsMalformed(t *testing.T) {
	base := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}]`
	decodeAnthropicExpect(t, base+`,"service_tier":7}`, 400)
	decodeAnthropicExpect(t, base+`,"service_tier":"priority"}`, 400)
	decodeAnthropicExpect(t, base+`,"metadata":{"user_id":7}}`, 400)
	decodeAnthropicExpect(t, base+`,"metadata":{"unknown":{}}}`, 400)
	decodeAnthropicExpect(t, base+`,"output_config":{"format":{"type":"json_object"}}}`, 400)
	decodeAnthropicExpect(t, base+`,"thinking":{"type":"mystery"}}`, 400)
	// Well-formed unbridged fields are diagnosed losses, not errors.
	// Disabled thinking is a typed Off intent; the other unbridged fields still lose.
	g, losses := mustDecodeAnthropic(t, base+`,"service_tier":"auto","metadata":{"user_id":"u1"},"thinking":{"type":"disabled"}}`)
	if len(losses) != 2 || !g.Reasoning.IsNone() || g.Reasoning.Origin() != "thinking" {
		t.Fatalf("expected two unrelated losses and a disabled intent, got %v %+v", losses, g.Reasoning)
	}
	if _, rerr := EnforcePolicy(losses, false); rerr == nil || rerr.Code != CodeTranslationLoss {
		t.Fatalf("strict must refuse, got %v", rerr)
	}
	kept, rerr := EnforcePolicy(losses, true)
	if rerr != nil {
		t.Fatalf("lossy must allow: %v", rerr)
	}
	hdr := EncodeLosses(kept)
	var decoded []Loss
	if err := json.Unmarshal([]byte(hdr), &decoded); err != nil || len(decoded) == 0 {
		t.Fatalf("bad loss header: %q", hdr)
	}
	if strings.Contains(hdr, "u1") {
		t.Fatalf("loss header must not carry values: %q", hdr)
	}
	_ = g
}

func TestAnthropicDecodeNativeToolsRejected(t *testing.T) {
	decodeAnthropicExpect(t, `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"bash_20250124","name":"bash"}]}`, 400)
	decodeAnthropicExpect(t, `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"web_search_20250305","name":"web_search"}]}`, 400)
	decodeAnthropicExpect(t, `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],
		"messages2":[]}`, 400)
	decodeAnthropicExpect(t, `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[
		{"type":"image","source":{"type":"base64","data":"eA==","media_type":"image/png"}}]}]}`, 400)
	decodeAnthropicExpect(t, `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[
		{"type":"thinking","thinking":"h","signature":"s"}]}]}`, 400)
	decodeAnthropicExpect(t, `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[
		{"type":"text","text":"x","citations":[{"type":"char_location","cited_text":"y"}]}]}]}`, 400)
}

func TestAnthropicDecodeNestedValidation(t *testing.T) {
	decodeAnthropicExpect(t, `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi","bogus":1}]}`, 400)
	decodeAnthropicExpect(t, `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[{"type":"text","text":"x","bogus":[]}]}]}`, 400)
	decodeAnthropicExpect(t, `{"model":"m","max_tokens":8,"messages":[{"role":"user"}]}`, 400)
	decodeAnthropicExpect(t, `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":null}]}`, 400)
	decodeAnthropicExpect(t, `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":""}]}`, 400)
	decodeAnthropicExpect(t, `{"model":"m","max_tokens":8,"messages":[{"role":"system","content":"hi"}]}`, 400)
	decodeAnthropicExpect(t, `{"model":"m","max_tokens":8,"messages":[{"role":"assistant","content":[
		{"type":"tool_use","id":"c1","name":"a","input":[1]}]}]}`, 400)
	decodeAnthropicExpect(t, `{"model":"m","max_tokens":8,"messages":[{"role":"assistant","content":[
		{"type":"tool_use","id":"c1","name":"a","input":"x"}]}]}`, 400)
	decodeAnthropicExpect(t, `{"model":"m","max_tokens":8,"messages":[{"role":"assistant","content":[
		{"type":"tool_use","id":"c1","name":"a","input":{},"zzz":1}]}]}`, 400)
	decodeAnthropicExpect(t, `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"x"}],
		"tools":[{"name":"a","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool"}}`, 400)
	decodeAnthropicExpect(t, `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"x"}],
		"tools":[{"name":"a","input_schema":{"type":"object"}}],"tool_choice":{"type":"auto","name":"a"}}`, 400)
	decodeAnthropicExpect(t, `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"x"}],
		"tools":[{"name":"has space","input_schema":{"type":"object"}}]}`, 400)
	decodeAnthropicExpect(t, `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"x"}],
		"tools":[{"name":"a","input_schema":{"type":"object"}}],"tool_choice":{"type":"auto","disable_parallel_tool_use":"yes"}}`, 400)
}

func TestAnthropicDecodeNumberPreservation(t *testing.T) {
	g, _ := mustDecodeAnthropic(t, `{"model":"m","max_tokens":8,"messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"calc","input":{"n":9007199254740993,"f":1.10}}]}]}`)
	args := string(g.Messages[0].ToolCalls[0].Arguments)
	if !strings.Contains(args, "9007199254740993") {
		t.Fatalf("large integer not preserved: %s", args)
	}
	if !strings.Contains(args, "1.10") {
		t.Fatalf("number literal not preserved: %s", args)
	}
}

func TestAnthropicDecodeResultLosses(t *testing.T) {
	// is_error has no Chat representation: strict refuses, lossy drops.
	raw := `{"model":"m","max_tokens":8,"messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"a","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"boom","is_error":true}]}]}`
	_, losses, rerr := DecodeAnthropicRequest([]byte(raw), DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	if len(losses) != 1 || !strings.Contains(losses[0].Field, "is_error") {
		t.Fatalf("is_error must be a single loss: %v", losses)
	}
	if _, rerr := EnforcePolicy(losses, false); rerr == nil {
		t.Fatalf("strict must refuse is_error")
	}
	g, kept, rerr := func() (*Generation, []Loss, *RouterError) {
		g, l, r := DecodeAnthropicRequest([]byte(raw), DefaultLimits())
		if r != nil {
			return nil, nil, r
		}
		k, r := EnforcePolicy(l, true)
		return g, k, r
	}()
	if rerr != nil {
		t.Fatalf("lossy must allow: %v", rerr)
	}
	_ = kept
	if g.Messages[1].Results[0].Content != "boom" || g.Messages[1].Results[0].IsError {
		t.Fatalf("lossy result wrong: %+v", g.Messages[1].Results[0])
	}
	// Malformed is_error is a 400 even in lossy mode.
	decodeAnthropicExpect(t, `{"model":"m","max_tokens":8,"messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"a","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"x","is_error":"yes"}]}]}`, 400)
}

func TestAnthropicDecodeToolStrictLoss(t *testing.T) {
	g, losses := mustDecodeAnthropic(t, `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"a","input_schema":{"type":"object"},"strict":true}]}`)
	if len(losses) != 1 || !strings.Contains(losses[0].Field, "strict") {
		t.Fatalf("strict:true must be a loss: %v", losses)
	}
	_ = g
	decodeAnthropicExpect(t, `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"a","input_schema":{"type":"object"},"strict":"yes"}]}`, 400)
}

func TestAnthropicDecodeToolHistoryCacheHintLoss(t *testing.T) {
	// Validated cache_control optimization hints on tool_use/tool_result
	// history are diagnosed dropped losses on translated routes (strict
	// refuses via EnforcePolicy, lossy drops); malformed hints are 400 in
	// both modes. IDs, args and results stay exact.
	raw := `{"model":"m","max_tokens":8,"messages":[
		{"role":"assistant","content":[
			{"type":"text","text":"Checking."},
			{"type":"tool_use","id":"call_1","name":"get_time","input":{"tz":"UTC"},"cache_control":{"type":"ephemeral"}}]},
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"call_1","content":"noon","cache_control":{"type":"ephemeral","ttl":"5m"}}]}]}`
	g, losses, rerr := DecodeAnthropicRequest([]byte(raw), DefaultLimits())
	if rerr != nil {
		t.Fatalf("well-formed history cache hints must decode: %v", rerr)
	}
	if len(losses) != 2 {
		t.Fatalf("expected two cache hint losses, got %v", losses)
	}
	fields := map[string]bool{}
	for _, l := range losses {
		fields[l.Field] = true
		if l.Action != "dropped" {
			t.Fatalf("cache hint loss must be dropped: %+v", l)
		}
	}
	if !fields["messages[0].content[1].cache_control"] || !fields["messages[1].content[0].cache_control"] {
		t.Fatalf("block-specific cache fields wrong: %v", losses)
	}
	if _, rerr := EnforcePolicy(losses, false); rerr == nil || rerr.Code != CodeTranslationLoss {
		t.Fatalf("strict must refuse cache hint losses, got %v", rerr)
	}
	if _, rerr := EnforcePolicy(losses, true); rerr != nil {
		t.Fatalf("lossy must allow cache hint losses: %v", rerr)
	}
	// IDs, names, args and results are exact through the loss.
	if len(g.Messages) != 2 || len(g.Messages[0].ToolCalls) != 1 || len(g.Messages[1].Results) != 1 {
		t.Fatalf("history shape lost: %+v", g.Messages)
	}
	tc := g.Messages[0].ToolCalls[0]
	if tc.ID != "call_1" || tc.Name != "get_time" || string(tc.Arguments) != `{"tz":"UTC"}` {
		t.Fatalf("tool_use identity/args lost: %+v", tc)
	}
	res := g.Messages[1].Results[0]
	if res.CallID != "call_1" || res.Content != "noon" {
		t.Fatalf("tool_result identity/content lost: %+v", res)
	}
	if len(g.Messages[0].Texts) != 1 || g.Messages[0].Texts[0].Text != "Checking." {
		t.Fatalf("assistant text lost: %+v", g.Messages[0].Texts)
	}
	// Optional TTL variant is well-formed.
	mustDecodeAnthropic(t, `{"model":"m","max_tokens":8,"messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"a","input":{},"cache_control":{"type":"ephemeral","ttl":"1h"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"x"}]}]}`)
	// Null means absent: no loss.
	if _, losses := mustDecodeAnthropic(t, `{"model":"m","max_tokens":8,"messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"a","input":{},"cache_control":null}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"x","cache_control":null}]}]}`); len(losses) != 0 {
		t.Fatalf("null cache hints must mean absent, got %v", losses)
	}
	// Malformed and unknown hints stay 400, never losses.
	for _, badCC := range []string{
		`"oops"`, `[]`, `{"type":"bad"}`, `{"type":7}`,
		`{"type":"ephemeral","ttl":"1d"}`, `{"type":"ephemeral","ttl":5}`,
		`{"type":"ephemeral","unknown":1}`,
	} {
		decodeAnthropicExpect(t, `{"model":"m","max_tokens":8,"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"a","input":{},"cache_control":`+badCC+`}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"x"}]}]}`, 400)
		decodeAnthropicExpect(t, `{"model":"m","max_tokens":8,"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"a","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"x","cache_control":`+badCC+`}]}]}`, 400)
	}
	// The shared validator also guards text-block, tool-definition and
	// top-level hints: malformed shapes are 400, well-formed are losses.
	decodeAnthropicExpect(t, `{"model":"m","max_tokens":8,"messages":[
		{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"bad"}}]}]}`, 400)
	decodeAnthropicExpect(t, `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"a","input_schema":{"type":"object"},"cache_control":{"ttl":"1d"}}]}`, 400)
	decodeAnthropicExpect(t, `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],
		"cache_control":{"type":"ephemeral","ttl":"2h"}}`, 400)
	g2, losses2 := mustDecodeAnthropic(t, `{"model":"m","max_tokens":8,
		"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}],
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"a","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral","ttl":"1h"}}]}`)
	if len(losses2) != 2 {
		t.Fatalf("text/tool cache hints must be losses, got %v", losses2)
	}
	_ = g2
}

func TestAnthropicEncodeChatWire(t *testing.T) {
	// Corrected: top_k has no portable Chat wire mapping (previously asserted
	// extra_body.top_k forwarding); the lossless wire omits top_k. Named
	// tool_choice remains an object; bare modes are strings (regression suite).
	g, _ := mustDecodeAnthropic(t, `{"model":"public","max_tokens":64,
		"system":[{"type":"text","text":"Be helpful."}],
		"messages":[
			{"role":"user","content":"What time?"},
			{"role":"assistant","content":[
				{"type":"text","text":"Checking."},
				{"type":"tool_use","id":"call_1","name":"get_time","input":{"tz":"UTC"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"noon"}]}],
		"temperature":0.5,"top_p":0.9,"stop_sequences":["END"],"stream":true,
		"tools":[{"name":"get_time","description":"clock",
			"input_schema":{"type":"object","properties":{"tz":{"type":"string"}}}}],
		"tool_choice":{"type":"tool","name":"get_time","disable_parallel_tool_use":true}}`)
	out, rerr := EncodeChatRequest(g, AbsentEffort(), "upstream-model")
	if rerr != nil {
		t.Fatalf("encode failed: %v", rerr)
	}
	var body struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Role      string `json:"role"`
			Content   any    `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
		MaxTokens   *int     `json:"max_tokens"`
		Temperature *float64 `json:"temperature"`
		TopP        *float64 `json:"top_p"`
		Stop        []string `json:"stop"`
		Tools       []struct {
			Type     string `json:"type"`
			Function struct {
				Name       string          `json:"name"`
				Parameters json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
		ToolChoice        map[string]any `json:"tool_choice"`
		ParallelToolCalls *bool          `json:"parallel_tool_calls"`
		StreamOptions     map[string]any `json:"stream_options"`
		ExtraBody         map[string]any `json:"extra_body"`
	}
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	if body.Model != "upstream-model" {
		t.Fatalf("model not replaced: %v", body.Model)
	}
	if !body.Stream || body.StreamOptions["include_usage"] != true {
		t.Fatalf("streaming must request upstream usage: %+v", body.StreamOptions)
	}
	if len(body.Messages) != 4 {
		t.Fatalf("messages wrong: %s", out)
	}
	if body.Messages[0].Role != "system" {
		t.Fatalf("system first: %s", out)
	}
	if body.Messages[2].Role != "assistant" || len(body.Messages[2].ToolCalls) != 1 {
		t.Fatalf("assistant calls wrong: %s", out)
	}
	tc := body.Messages[2].ToolCalls[0]
	if tc.ID != "call_1" || tc.Type != "function" || tc.Function.Name != "get_time" {
		t.Fatalf("call identity lost: %+v", tc)
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil || args["tz"] != "UTC" {
		t.Fatalf("args wrong: %q", tc.Function.Arguments)
	}
	if body.Messages[3].Role != "tool" || body.Messages[3].ToolCallID != "call_1" {
		t.Fatalf("tool result wrong: %s", out)
	}
	if body.ToolChoice["type"] != "function" || body.ToolChoice["function"].(map[string]any)["name"] != "get_time" {
		t.Fatalf("choice wrong: %v", body.ToolChoice)
	}
	if body.ParallelToolCalls == nil || *body.ParallelToolCalls {
		t.Fatalf("parallel mapping wrong: %v", body.ParallelToolCalls)
	}
	if _, ok := body.ExtraBody["top_k"]; ok {
		t.Fatalf("top_k must not forward to the Chat bridge: %v", body.ExtraBody)
	}
	if body.MaxTokens == nil || *body.MaxTokens != 64 {
		t.Fatalf("max_tokens wrong: %s", out)
	}
}

func TestAnthropicEncodeNonStreamOmitsUsage(t *testing.T) {
	g, _ := mustDecodeAnthropic(t, `{"model":"m","max_tokens":8,
		"messages":[{"role":"user","content":"hi"}]}`)
	out, rerr := EncodeChatRequest(g, AbsentEffort(), "up")
	if rerr != nil {
		t.Fatalf("encode: %v", rerr)
	}
	if strings.Contains(string(out), "stream_options") {
		t.Fatalf("non-streaming must not request usage: %s", out)
	}
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["tools"]; ok {
		t.Fatalf("no tools must mean no tools key: %s", out)
	}
}

func mustTranslateChatJSON(t *testing.T, raw string) []byte {
	t.Helper()
	out, rerr := TranslateChatJSONToAnthropic([]byte(raw), "public", DefaultLimits())
	if rerr != nil {
		t.Fatalf("translate failed: %v", rerr)
	}
	return out
}

func TestChatJSONToAnthropicText(t *testing.T) {
	raw := `{"id":"chatcmpl-1","object":"chat.completion","created":1700000000,"model":"up",
		"choices":[{"index":0,"message":{"role":"assistant","content":"Hi there"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
	out := mustTranslateChatJSON(t, raw)
	var body struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Role    string `json:"role"`
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			Input  int `json:"input_tokens"`
			Output int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	if body.Type != "message" || body.Role != "assistant" || body.Model != "public" {
		t.Fatalf("envelope wrong: %s", out)
	}
	if !strings.HasPrefix(body.ID, "msg_") {
		t.Fatalf("id wrong: %q", body.ID)
	}
	if len(body.Content) != 1 || body.Content[0].Text != "Hi there" {
		t.Fatalf("content wrong: %s", out)
	}
	if body.StopReason != "end_turn" {
		t.Fatalf("stop wrong: %q", body.StopReason)
	}
	if body.Usage.Input != 10 || body.Usage.Output != 5 {
		t.Fatalf("usage wrong: %+v", body.Usage)
	}
}

func TestChatJSONToAnthropicTools(t *testing.T) {
	raw := `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"up",
		"choices":[{"index":0,"message":{"role":"assistant","content":null,
			"tool_calls":[{"id":"call_1","type":"function",
				"function":{"name":"get_time","arguments":"{\"tz\":\"UTC\"}"}}]},
			"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":12,"completion_tokens":7,"total_tokens":19,
			"prompt_tokens_details":{"cached_tokens":4,"audio_tokens":0}}}`
	out := mustTranslateChatJSON(t, raw)
	var body struct {
		Content []struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			Input     int `json:"input_tokens"`
			Output    int `json:"output_tokens"`
			CacheRead int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	if body.StopReason != "tool_use" {
		t.Fatalf("stop wrong: %q", body.StopReason)
	}
	if len(body.Content) != 1 || body.Content[0].Type != "tool_use" {
		t.Fatalf("content wrong: %s", out)
	}
	tc := body.Content[0]
	if tc.ID != "call_1" || tc.Name != "get_time" || string(tc.Input) != `{"tz":"UTC"}` {
		t.Fatalf("identity lost: %+v", tc)
	}
	// Corrected: Chat prompt_tokens includes cached tokens, while Anthropic
	// input_tokens excludes them (previously asserted double-count input 12).
	// prompt 12 cached 4 => input 6 cache_read 4.
	if body.Usage.Input != 8 || body.Usage.Output != 7 || body.Usage.CacheRead != 4 {
		t.Fatalf("usage wrong: %+v", body.Usage)
	}
	if strings.Contains(string(out), "UTC\"}") && !strings.Contains(string(out), `"input":{"tz":"UTC"}`) {
		t.Fatalf("args not verbatim: %s", out)
	}
}

func TestChatJSONStopMapping(t *testing.T) {
	base := func(finish, message string) string {
		return `{"id":"c","object":"chat.completion","created":1,"model":"up",
			"choices":[{"index":0,"message":` + message + `,"finish_reason":"` + finish + `"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	}
	msg := `{"role":"assistant","content":"x"}`
	check := func(finish, want string) {
		t.Helper()
		out := mustTranslateChatJSON(t, base(finish, msg))
		if !strings.Contains(string(out), `"stop_reason":"`+want+`"`) {
			t.Fatalf("%s must map to %s: %s", finish, want, out)
		}
	}
	check("stop", "end_turn")
	check("length", "max_tokens")
	check("content_filter", "refusal")
	// A tool call coerces a bare stop into tool_use.
	toolMsg := `{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function",
		"function":{"name":"a","arguments":"{}"}}]}`
	out := mustTranslateChatJSON(t, base("stop", toolMsg))
	if !strings.Contains(string(out), `"stop_reason":"tool_use"`) {
		t.Fatalf("tool calls must coerce stop: %s", out)
	}
	// Refusal text is preserved with the refusal stop.
	refMsg := `{"role":"assistant","content":null,"refusal":"cannot help"}`
	out = mustTranslateChatJSON(t, base("content_filter", refMsg))
	if !strings.Contains(string(out), `"stop_reason":"refusal"`) || !strings.Contains(string(out), "cannot help") {
		t.Fatalf("refusal wrong: %s", out)
	}
}

func TestChatJSONRejectsUnrepresentable(t *testing.T) {
	base := func(choice, usage string) string {
		return `{"id":"c","object":"chat.completion","created":1,"model":"up",` +
			`"choices":[` + choice + `],"usage":` + usage + `}`
	}
	msg := `{"role":"assistant","content":"hi"}`
	usage := `{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}`
	must502 := func(raw string) {
		t.Helper()
		if _, rerr := TranslateChatJSONToAnthropic([]byte(raw), "public", DefaultLimits()); rerr == nil || rerr.Status != 502 {
			t.Fatalf("expected 502 for %s, got %v", raw, rerr)
		}
	}
	must502(base(`{"index":0,"message":`+msg+`,"finish_reason":"stop"},{"index":1,"message":`+msg+`,"finish_reason":"stop"}`, usage))
	must502(base(`{"index":0,"message":`+msg+`,"finish_reason":"function_call"}`, usage))
	must502(base(`{"index":0,"message":`+msg+`,"finish_reason":"bogus"}`, usage))
	must502(base(`{"index":0,"message":{"role":"user","content":"hi"},"finish_reason":"stop"}`, usage))
	must502(base(`{"index":0,"message":{"role":"assistant","content":"hi","logprobs":{}},"finish_reason":"stop"}`, usage))
	must502(base(`{"index":0,"message":{"role":"assistant","content":"hi","audio":{}},"finish_reason":"stop"}`, usage))
	must502(base(`{"index":0,"message":{"role":"assistant","content":"hi","function_call":{}},"finish_reason":"stop"}`, usage))
	must502(base(`{"index":0,"message":{"role":"assistant","content":"hi","annotations":[{"type":"x"}]},"finish_reason":"stop"}`, usage))
	must502(base(`{"index":0,"message":{"role":"assistant","content":"hi","name":"bob"},"finish_reason":"stop"}`, usage))
	must502(base(`{"index":0,"message":{"role":"assistant","content":"hi",
		"tool_calls":[{"id":"c1","type":"custom","function":{"name":"a","arguments":"{}"}}]},"finish_reason":"tool_calls"}`, usage))
	must502(base(`{"index":0,"message":{"role":"assistant","content":"hi",
		"tool_calls":[{"id":"c1","type":"function","function":{"name":"a","arguments":"[1]"}}]},"finish_reason":"tool_calls"}`, usage))
	must502(base(`{"index":0,"message":{"role":"assistant","content":"hi",
		"tool_calls":[{"id":"c1","type":"function","function":{"name":"a","arguments":"{}"}},
			{"id":"c1","type":"function","function":{"name":"b","arguments":"{}"}}]},"finish_reason":"tool_calls"}`, usage))
	must502(base(`{"index":0,"message":`+msg+`,"finish_reason":"stop"}`, `{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2,"mystery":1}`))
	must502(base(`{"index":0,"message":`+msg+`,"finish_reason":"stop"}`, `{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2,
		"completion_tokens_details":{"reasoning_tokens":3}}`))
	must502(base(`{"index":0,"message":`+msg+`,"finish_reason":"stop"}`, `{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2,
		"prompt_tokens_details":{"audio_tokens":2}}`))
	must502(base(`{"index":0,"message":`+msg+`,"finish_reason":"stop"}`, `{"prompt_tokens":1}`))
	must502(base(`{"index":0,"message":`+msg+`,"finish_reason":"stop"}`, `{"prompt_tokens":1.5,"completion_tokens":1,"total_tokens":2}`))
	// Zero-valued details are harmless and accepted.
	mustTranslateChatJSON(t, base(`{"index":0,"message":`+msg+`,"finish_reason":"stop"}`,
		`{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2,
			"prompt_tokens_details":null,"completion_tokens_details":{"reasoning_tokens":0}}`))
	// Large integers in tool arguments survive exactly.
	out := mustTranslateChatJSON(t, base(`{"index":0,"message":{"role":"assistant","content":null,
		"tool_calls":[{"id":"c1","type":"function","function":{"name":"n","arguments":"{\"n\":9007199254740993}"}}]},
		"finish_reason":"tool_calls"}`, usage))
	if !strings.Contains(string(out), "9007199254740993") {
		t.Fatalf("integers not preserved: %s", out)
	}
	// Unknown top-level members fail closed.
	must502(`{"id":"c","object":"chat.completion","created":1,"model":"up","choices":[],
		"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2},"mystery":1}`)
}

func runChatStream(t *testing.T, upstream string, lim Limits) (string, *RouterError) {
	t.Helper()
	var sb strings.Builder
	tr := &ChatToAnthropicSSETranslator{PublicModel: "public", Limits: lim}
	rerr := tr.Translate(strings.NewReader(upstream), &sb, nil)
	return sb.String(), rerr
}

func chatChunk(delta, finish, usage string) string {
	ch := `{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"up","choices":[{"index":0,"delta":` + delta
	if finish == "" {
		ch += `,"finish_reason":null`
	} else {
		ch += `,"finish_reason":"` + finish + `"`
	}
	ch += `}]}`
	if usage != "" {
		ch += `,"usage":` + usage
	}
	return "data: " + ch + "\n\n"
}

func chatUsageChunk(usage string) string {
	return "data: " + `{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"up","choices":[],"usage":` + usage + "}\n\n"
}

func TestChatSSETextLifecycle(t *testing.T) {
	usage := `{"prompt_tokens":25,"completion_tokens":15,"total_tokens":40}`
	upstream := chatChunk(`{"role":"assistant","content":""}`, "", "") +
		chatChunk(`{"content":"Hello"}`, "", "") +
		chatChunk(`{"content":"!"}`, "", "") +
		chatChunk(`{}`, "stop", "") +
		chatUsageChunk(usage) +
		"data: [DONE]\n\n"
	out, rerr := runChatStream(t, upstream, DefaultLimits())
	if rerr != nil {
		t.Fatalf("stream failed: %v", rerr)
	}
	for _, want := range []string{
		`event: message_start`, `"input_tokens":0`, `"output_tokens":0`,
		`event: content_block_start`, `"index":0`, `"type":"text"`,
		`"text_delta"`, `"text":"Hello"`, `"text":"!"`,
		`event: content_block_stop`, `event: message_delta`,
		`"stop_reason":"end_turn"`,
		`"input_tokens":25`, `"output_tokens":15`,
		`event: message_stop`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stream lacks %s:\n%s", want, out)
		}
	}
	// Event order: start < block_start < deltas < block_stop < delta < stop.
	order := []string{"message_start", "content_block_start", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop"}
	last := -1
	for _, ev := range order {
		i := strings.Index(out, "event: "+ev)
		if i < 0 || i < last {
			t.Fatalf("event order wrong at %s:\n%s", ev, out)
		}
		last = i
	}
}

func TestChatSSEFragmentedParallelTools(t *testing.T) {
	usage := `{"prompt_tokens":30,"completion_tokens":20,"total_tokens":50}`
	upstream := chatChunk(`{"role":"assistant"}`, "", "") +
		chatChunk(`{"tool_calls":[{"index":0,"id":"call_A","type":"function","function":{"name":"get_time","arguments":""}}]}`, "", "") +
		chatChunk(`{"tool_calls":[{"index":1,"id":"call_B","type":"function","function":{"name":"calc","arguments":""}}]}`, "", "") +
		chatChunk(`{"tool_calls":[{"index":0,"function":{"arguments":"{\"tz\":"}}]}`, "", "") +
		chatChunk(`{"tool_calls":[{"index":1,"function":{"arguments":"{\"n\":9007199254740993}"}},{"index":0,"function":{"arguments":"\"UTC\"}"}}]}`, "", "") +
		chatChunk(`{}`, "tool_calls", "") +
		chatUsageChunk(usage) +
		"data: [DONE]\n\n"
	out, rerr := runChatStream(t, upstream, DefaultLimits())
	if rerr != nil {
		t.Fatalf("stream failed: %v", rerr)
	}
	for _, want := range []string{
		`"id":"call_A"`, `"name":"get_time"`, `"id":"call_B"`, `"name":"calc"`,
		`9007199254740993`, `"stop_reason":"tool_use"`,
		`"input_tokens":30`, `"output_tokens":20`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stream lacks %s:\n%s", want, out)
		}
	}
	// Fragments of one call accumulate into ordered deltas.
	if strings.Index(out, `"call_A"`) > strings.Index(out, `"call_B"`) {
		t.Fatalf("tool order not preserved:\n%s", out)
	}
}

func TestChatSSEEmptyArgTool(t *testing.T) {
	usage := `{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}`
	upstream := chatChunk(`{"tool_calls":[{"index":0,"id":"to_1","type":"function","function":{"name":"ping","arguments":""}}]}`, "", "") +
		chatChunk(`{}`, "tool_calls", "") +
		chatUsageChunk(usage) +
		"data: [DONE]\n\n"
	out, rerr := runChatStream(t, upstream, DefaultLimits())
	if rerr != nil {
		t.Fatalf("empty-arg tool must succeed: %v", rerr)
	}
	if !strings.Contains(out, `"id":"to_1"`) || !strings.Contains(out, `"stop_reason":"tool_use"`) {
		t.Fatalf("tool identity missing:\n%s", out)
	}
}

func TestChatSSETruncationFails(t *testing.T) {
	// EOF before DONE.
	upstream := chatChunk(`{"content":"Hi"}`, "", "") +
		chatChunk(`{}`, "stop", "")
	if _, rerr := runChatStream(t, upstream, DefaultLimits()); rerr == nil {
		t.Fatalf("truncated stream must fail")
	}
	// DONE without finish.
	upstream = chatChunk(`{"content":"Hi"}`, "", "") + "data: [DONE]\n\n"
	out, rerr := runChatStream(t, upstream, DefaultLimits())
	if rerr == nil {
		t.Fatalf("DONE without finish must fail")
	}
	if strings.Contains(out, "message_stop") {
		t.Fatalf("must not emit success marker:\n%s", out)
	}
	// Finish without terminal usage: accounting never arrived, so the
	// translation fails instead of underreporting zeros.
	upstream = chatChunk(`{"content":"Hi"}`, "", "") +
		chatChunk(`{}`, "stop", "") + "data: [DONE]\n\n"
	if _, rerr := runChatStream(t, upstream, DefaultLimits()); rerr == nil {
		t.Fatalf("missing usage must fail")
	}
}

func TestChatSSEUnrepresentableFails(t *testing.T) {
	usage := `{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}`
	term := chatChunk(`{}`, "stop", "") + chatUsageChunk(usage) + "data: [DONE]\n\n"
	mustFail := func(name, first string) {
		t.Helper()
		out, rerr := runChatStream(t, first+term, DefaultLimits())
		if rerr == nil || rerr.Status != 502 {
			t.Fatalf("%s must fail with 502, got %v", name, rerr)
		}
		if strings.Contains(out, "message_stop") {
			t.Fatalf("%s must not succeed:\n%s", name, out)
		}
	}
	mustFail("multichoice", "data: "+`{"id":"c","object":"chat.completion.chunk","created":1,"model":"u",
		"choices":[{"index":0,"delta":{},"finish_reason":null},{"index":1,"delta":{},"finish_reason":null}]}`+"\n\n")
	mustFail("refusal", chatChunk(`{"refusal":"no"}`, "", ""))
	mustFail("function_call", chatChunk(`{"function_call":{"name":"f","arguments":"{}"}}`, "", ""))
	mustFail("audio", chatChunk(`{"audio":{"id":"a"}}`, "", ""))
	mustFail("bad-finish", chatChunk(`{}`, "function_call", ""))
	mustFail("dup-finish", chatChunk(`{}`, "stop", "")+chatChunk(`{}`, "stop", ""))
	mustFail("content-after-finish", chatChunk(`{}`, "stop", "")+chatChunk(`{"content":"late"}`, "", ""))
	mustFail("dup-id", chatChunk(`{"tool_calls":[{"index":0,"id":"c1","function":{"name":"a","arguments":""}}]}`, "", "")+
		chatChunk(`{"tool_calls":[{"index":1,"id":"c1","function":{"name":"b","arguments":""}}]}`, "", ""))
	mustFail("bad-args", chatChunk(`{"tool_calls":[{"index":0,"id":"c1","function":{"name":"a","arguments":"[1]"}}]}`, "", ""))
	mustFail("unknown-top", "data: "+`{"id":"c","mystery":1}`+"\n\n")
	mustFail("unknown-delta", chatChunk(`{"content":"x","bogus":1}`, "", ""))
	mustFail("bad-usage", chatChunk(`{}`, "stop", "")+
		chatUsageChunk(`{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2,"mystery":1}`)+
		"data: [DONE]\n\n")
	// Reasoning accounting has no representation.
	mustFail("reasoning-usage", chatChunk(`{}`, "stop", "")+
		chatUsageChunk(`{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2,
			"completion_tokens_details":{"reasoning_tokens":3}}`)+
		"data: [DONE]\n\n")
}

func TestChatSSEBounds(t *testing.T) {
	usage := `{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}`
	term := chatChunk(`{}`, "stop", "") + chatUsageChunk(usage) + "data: [DONE]\n\n"
	// Overlong frame.
	lim := DefaultLimits()
	lim.MaxFrameBytes = 64
	if _, rerr := runChatStream(t, chatChunk(`{"content":"hi"}`, "", "")+term, lim); rerr == nil {
		t.Fatalf("overlong frame must fail")
	}
	// Oversized tool arguments.
	lim = DefaultLimits()
	lim.MaxArgsChars = 8
	big := chatChunk(`{"tool_calls":[{"index":0,"id":"c1","function":{"name":"a","arguments":"{\"k\":\"0123456789\"}"}}]}`, "", "")
	if _, rerr := runChatStream(t, big+term, lim); rerr == nil {
		t.Fatalf("oversized args must fail")
	}
	// Too many tool calls.
	lim = DefaultLimits()
	lim.MaxToolCalls = 1
	two := chatChunk(`{"tool_calls":[{"index":0,"id":"c1","function":{"name":"a","arguments":""}}]}`, "", "") +
		chatChunk(`{"tool_calls":[{"index":1,"id":"c2","function":{"name":"b","arguments":""}}]}`, "", "")
	if _, rerr := runChatStream(t, two+term, lim); rerr == nil {
		t.Fatalf("too many calls must fail")
	}
}

type failingWriter struct{ n int }

func (f *failingWriter) Write(p []byte) (int, error) {
	f.n++
	if f.n > 1 {
		return 0, errTestDownstream
	}
	return len(p), nil
}

var errTestDownstream = errTestDownstreamNew()

func errTestDownstreamNew() error { return &testDownstreamError{} }

type testDownstreamError struct{}

func (e *testDownstreamError) Error() string { return "downstream unavailable" }

func TestChatSSEWriterFailure(t *testing.T) {
	usage := `{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}`
	upstream := chatChunk(`{"content":"Hello, this is a long enough first write to commit"}`, "", "") +
		chatChunk(`{"content":"more"}`, "", "") +
		chatChunk(`{}`, "stop", "") + chatUsageChunk(usage) + "data: [DONE]\n\n"
	tr := &ChatToAnthropicSSETranslator{PublicModel: "public", Limits: DefaultLimits()}
	fw := &failingWriter{}
	if rerr := tr.Translate(strings.NewReader(upstream), fw, nil); rerr == nil || rerr.Status != 502 {
		t.Fatalf("writer failure must 502, got %v", rerr)
	}
}

func TestChatSSEUpstreamErrorEvent(t *testing.T) {
	// Pre-commit error: nothing was written, so no Anthropic error event.
	out, rerr := runChatStream(t, "data: "+`{"error":{"code":500,"message":"boom"}}`+"\n\n", DefaultLimits())
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("upstream error must 502, got %v", rerr)
	}
	if out != "" {
		t.Fatalf("pre-commit error must emit nothing: %q", out)
	}
	// Post-commit error: an Anthropic error event, then failure.
	usage := `{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}`
	upstream := chatChunk(`{"content":"hi"}`, "", "") +
		"data: " + `{"error":{"code":500,"message":"boom"}}` + "\n\n" +
		chatChunk(`{}`, "stop", "") + chatUsageChunk(usage) + "data: [DONE]\n\n"
	out, rerr = runChatStream(t, upstream, DefaultLimits())
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("post-commit error must 502, got %v", rerr)
	}
	if !strings.Contains(out, "event: error") || strings.Contains(out, "message_stop") {
		t.Fatalf("post-commit error must emit error event only:\n%s", out)
	}
}
