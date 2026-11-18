package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustDecodeRFA(t *testing.T, raw string) (*Generation, []Loss) {
	t.Helper()
	g, losses, rerr := DecodeResponsesForAnthropic([]byte(raw), DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode failed: %v", rerr)
	}
	return g, losses
}

func decodeRFAExpect(t *testing.T, raw string, wantStatus int, wantCode string) *RouterError {
	t.Helper()
	_, _, rerr := DecodeResponsesForAnthropic([]byte(raw), DefaultLimits())
	if rerr == nil {
		t.Fatalf("expected status %d, got success for %s", wantStatus, raw)
	}
	if rerr.Status != wantStatus {
		t.Fatalf("expected status %d, got %d (%s) for %s", wantStatus, rerr.Status, rerr.Msg, raw)
	}
	if wantCode != "" && rerr.Code != wantCode {
		t.Fatalf("expected code %s, got %s (%s) for %s", wantCode, rerr.Code, rerr.Msg, raw)
	}
	return rerr
}

func TestRFATextWire(t *testing.T) {
	g, losses := mustDecodeRFA(t, `{"model":"m","input":"Hello",
		"instructions":"Be helpful.","temperature":0.5,"top_p":0.9,
		"max_output_tokens":64,"stream":false}`)
	if len(losses) != 0 {
		t.Fatalf("unexpected losses: %v", losses)
	}
	out, rerr := EncodeAnthropicRequest(g, "upstream-model")
	if rerr != nil {
		t.Fatalf("encode failed: %v", rerr)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	var model string
	if err := json.Unmarshal(body["model"], &model); err != nil || model != "upstream-model" {
		t.Fatalf("public model not replaced: %s", out)
	}
	var maxTok int
	if err := json.Unmarshal(body["max_tokens"], &maxTok); err != nil || maxTok != 64 {
		t.Fatalf("max_tokens wrong: %s", out)
	}
	var msgs []map[string]json.RawMessage
	if err := json.Unmarshal(body["messages"], &msgs); err != nil || len(msgs) != 1 {
		t.Fatalf("messages wrong: %s", out)
	}
	// Anthropic hoists system prompts out of messages.
	var system []map[string]any
	if err := json.Unmarshal(body["system"], &system); err != nil || len(system) != 1 {
		t.Fatalf("system not hoisted: %s", out)
	}
	if !strings.Contains(string(out), "Be helpful.") || !strings.Contains(string(out), "Hello") {
		t.Fatalf("text lost: %s", out)
	}
	if !strings.Contains(string(out), `"temperature":0.5`) || !strings.Contains(string(out), `"top_p":0.9`) {
		t.Fatalf("controls lost: %s", out)
	}
}

func TestRFADefaultMaxTokens(t *testing.T) {
	// Anthropic requires max_tokens; an absent Responses cap uses the
	// documented bridge default, not a model-detected value, and no loss.
	g, losses := mustDecodeRFA(t, `{"model":"m","input":"Hi"}`)
	if len(losses) != 0 {
		t.Fatalf("absent cap must not be a loss: %v", losses)
	}
	out, rerr := EncodeAnthropicRequest(g, "up")
	if rerr != nil {
		t.Fatalf("encode: %v", rerr)
	}
	if !strings.Contains(string(out), `"max_tokens":4096`) {
		t.Fatalf("bridge default missing: %s", out)
	}
}

func TestRFATemperatureBounds(t *testing.T) {
	// Exact within the Anthropic 0..1 range.
	g, losses := mustDecodeRFA(t, `{"model":"m","input":"Hi","temperature":1}`)
	if len(losses) != 0 || g.Temperature == nil || *g.Temperature != 1 {
		t.Fatalf("boundary temp must be exact: %+v %v", g.Temperature, losses)
	}
	// Valid source above 1: diagnosed loss, dropped (never clamped).
	g, losses, rerr := DecodeResponsesForAnthropic([]byte(`{"model":"m","input":"Hi","temperature":1.5}`), DefaultLimits())
	if rerr != nil {
		t.Fatalf("valid source temp must decode: %v", rerr)
	}
	if len(losses) != 1 || !strings.Contains(losses[0].Field, "temperature") {
		t.Fatalf("temp>1 must be a single loss: %v", losses)
	}
	if g.Temperature != nil {
		t.Fatalf("temp>1 must be dropped, got %v", *g.Temperature)
	}
	if _, rerr := EnforcePolicy(losses, false); rerr == nil || rerr.Code != CodeTranslationLoss {
		t.Fatalf("strict must refuse temp>1, got %v", rerr)
	}
	kept, rerr := EnforcePolicy(losses, true)
	if rerr != nil {
		t.Fatalf("lossy must allow: %v", rerr)
	}
	_ = kept
	out, rerr := EncodeAnthropicRequest(g, "up")
	if rerr != nil {
		t.Fatalf("encode: %v", rerr)
	}
	if strings.Contains(string(out), "temperature") {
		t.Fatalf("dropped temp must not reach the wire: %s", out)
	}
	// Loss header is value-free.
	hdr := EncodeLosses(losses)
	if strings.Contains(hdr, "1.5") {
		t.Fatalf("loss header must not carry values: %q", hdr)
	}
	// Malformed/out-of-range are 400 in both modes.
	decodeRFAExpect(t, `{"model":"m","input":"Hi","temperature":2.5}`, 400, CodeInvalidRequest)
	decodeRFAExpect(t, `{"model":"m","input":"Hi","temperature":"hot"}`, 400, CodeInvalidRequest)
	// Explicit null means absent, not a number.
	gNull, _, rerrNull := DecodeResponsesForAnthropic([]byte(`{"model":"m","input":"Hi","temperature":null}`), DefaultLimits())
	if rerrNull != nil || gNull.Temperature != nil {
		t.Fatalf("null temperature must be absent: %v %+v", rerrNull, gNull)
	}
}

func TestRFAExplicitImplicitStrict(t *testing.T) {
	// Implicit (omitted/null) strict: diagnosed loss, never silently equated.
	_, losses, rerr := DecodeResponsesForAnthropic([]byte(`{"model":"m","input":"Hi",
		"tools":[{"type":"function","name":"a","parameters":{"type":"object"}}]}`), DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	if len(losses) != 1 || !strings.Contains(losses[0].Field, "strict") {
		t.Fatalf("implicit strict must be a loss: %v", losses)
	}
	if _, rerr := EnforcePolicy(losses, false); rerr == nil || rerr.Code != CodeTranslationLoss {
		t.Fatalf("strict must refuse implicit strict: %v", rerr)
	}
	// Explicit false is exact: no loss, encodes cleanly.
	g, losses := mustDecodeRFA(t, `{"model":"m","input":"Hi",
		"tools":[{"type":"function","name":"a","strict":false,"parameters":{"type":"object"}}]}`)
	if len(losses) != 0 {
		t.Fatalf("explicit false must be exact: %v", losses)
	}
	if g.Tools[0].Strict == nil || *g.Tools[0].Strict {
		t.Fatalf("explicit false not retained: %+v", g.Tools[0].Strict)
	}
	out, rerr := EncodeAnthropicRequest(g, "up")
	if rerr != nil {
		t.Fatalf("encode: %v", rerr)
	}
	if strings.Contains(string(out), "strict") {
		t.Fatalf("Anthropic wire must not carry strict: %s", out)
	}
	// Explicit true is refused: Anthropic strict guarantees are not reviewed.
	decodeRFAExpect(t, `{"model":"m","input":"Hi",
		"tools":[{"type":"function","name":"a","strict":true}]}`, 400, CodeInvalidGeneration)
	// Malformed strict is a 400.
	decodeRFAExpect(t, `{"model":"m","input":"Hi",
		"tools":[{"type":"function","name":"a","strict":"yes"}]}`, 400, CodeInvalidRequest)
}

func TestRFAStatefulRefused(t *testing.T) {
	// Well-formed state references refuse with unsupported_responses_state.
	decodeRFAExpect(t, `{"model":"m","input":"Hi","previous_response_id":"resp_1"}`, 400, "unsupported_responses_state")
	decodeRFAExpect(t, `{"model":"m","input":"Hi","conversation":"conv_1"}`, 400, "unsupported_responses_state")
	decodeRFAExpect(t, `{"model":"m","input":"Hi","conversation":{"id":"conv_1"}}`, 400, "unsupported_responses_state")
	decodeRFAExpect(t, `{"model":"m","input":"Hi","prompt":{"id":"pmpt_1","version":"2"}}`, 400, "unsupported_responses_state")
	decodeRFAExpect(t, `{"model":"m","input":"Hi","background":true}`, 400, "unsupported_responses_state")
	// Malformed state is invalid_request, not a state refusal.
	decodeRFAExpect(t, `{"model":"m","input":"Hi","previous_response_id":7}`, 400, CodeInvalidRequest)
	decodeRFAExpect(t, `{"model":"m","input":"Hi","background":"yes"}`, 400, CodeInvalidRequest)
	decodeRFAExpect(t, `{"model":"m","input":"Hi","previous_response_id":"a","conversation":"b"}`, 400, CodeInvalidRequest)
	// Reviewed identity: false/null are absent.
	g, losses := mustDecodeRFA(t, `{"model":"m","input":"Hi","background":false}`)
	if len(losses) != 0 {
		t.Fatalf("background:false must be identity: %v", losses)
	}
	g, losses = mustDecodeRFA(t, `{"model":"m","input":"Hi","background":null,"previous_response_id":null}`)
	if len(losses) != 0 {
		t.Fatalf("null state must be absent: %v", losses)
	}
	_ = g
}

func TestRFAUnknownFieldsRejected(t *testing.T) {
	decodeRFAExpect(t, `{"model":"m","input":"Hi","mystery":1}`, 400, CodeInvalidGeneration)
	decodeRFAExpect(t, `{"model":"m","input":[{"type":"message","role":"user","content":"x","bogus":1}]}`, 400, CodeInvalidGeneration)
	decodeRFAExpect(t, `{"model":"m","input":"Hi","tools":[{"type":"function","name":"a","bogus":1}]}`, 400, CodeInvalidGeneration)
	// Malformed known-but-unbridged fields are 400 even in lossy mode.
	decodeRFAExpect(t, `{"model":"m","input":"Hi","service_tier":"platinum"}`, 400, CodeInvalidRequest)
	decodeRFAExpect(t, `{"model":"m","input":"Hi","truncation":"sometimes"}`, 400, CodeInvalidRequest)
	// Well-formed ones are diagnosed losses, never silently dropped.
	_, losses, rerr := DecodeResponsesForAnthropic([]byte(`{"model":"m","input":"Hi","service_tier":"auto","truncation":"disabled"}`), DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	if len(losses) != 2 {
		t.Fatalf("expected 2 losses, got %v", losses)
	}
	if _, rerr := EnforcePolicy(losses, false); rerr == nil {
		t.Fatalf("strict must refuse losses")
	}
}

func TestRFAToolsHistoryWire(t *testing.T) {
	// Tool-first input order is preserved one-to-one in the IR.
	g, losses := mustDecodeRFA(t, `{"model":"m","input":[
		{"type":"function_call","id":"fc_9","call_id":"call_9","name":"calc","arguments":"{\"x\":9007199254740993}"},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"Thanks ✓"}]},
		{"type":"function_call_output","call_id":"call_9","output":""},
		{"type":"message","role":"user","content":"done"}],
		"tools":[{"type":"function","name":"calc","strict":false,"description":"d",
			"parameters":{"type":"object","properties":{"x":{"type":"number"}}}}],
		"tool_choice":{"type":"function","name":"calc"},"parallel_tool_calls":false}`)
	if len(losses) != 0 {
		t.Fatalf("unexpected losses: %v", losses)
	}
	if len(g.Messages) != 4 || len(g.Messages[0].ToolCalls) != 1 {
		t.Fatalf("order not preserved: %+v", g.Messages)
	}
	if g.Messages[0].ToolCalls[0].ID != "call_9" {
		t.Fatalf("call_id identity lost: %+v", g.Messages[0].ToolCalls[0])
	}
	if !strings.Contains(string(g.Messages[0].ToolCalls[0].Arguments), "9007199254740993") {
		t.Fatalf("number literal not exact: %s", g.Messages[0].ToolCalls[0].Arguments)
	}
	// Empty tool result content is preserved exactly once (not dropped).
	if g.Messages[2].Results[0].Content != "" {
		t.Fatalf("empty result must survive: %+v", g.Messages[2].Results[0])
	}
	if g.Messages[1].Texts[0].Text != "Thanks ✓" {
		t.Fatalf("unicode result lost: %+v", g.Messages[1])
	}
	out, rerr := EncodeAnthropicRequest(g, "upstream")
	if rerr != nil {
		t.Fatalf("encode: %v", rerr)
	}
	// Call identity and numeric args survive the Anthropic wire exactly.
	if !strings.Contains(string(out), `"id":"call_9"`) || !strings.Contains(string(out), "9007199254740993") {
		t.Fatalf("identity/numbers lost: %s", out)
	}
	// Named forced choice reaches Anthropic as the tool union.
	if !strings.Contains(string(out), `"type":"tool"`) || !strings.Contains(string(out), `"name":"calc"`) {
		t.Fatalf("forced choice wrong: %s", out)
	}
	// Parallel false reaches Anthropic as disable_parallel_tool_use.
	if !strings.Contains(string(out), `"disable_parallel_tool_use":true`) {
		t.Fatalf("parallel mapping lost: %s", out)
	}
	// Upstream item id (fc_9) never becomes the call identity.
	if strings.Contains(string(out), "fc_9") {
		t.Fatalf("item id leaked into wire: %s", out)
	}
}

func TestRFAChoiceModes(t *testing.T) {
	for tc, want := range map[string]string{
		`"none"`: `"type":"none"`, `"auto"`: `"type":"auto"`, `"required"`: `"type":"any"`,
	} {
		g, _ := mustDecodeRFA(t, `{"model":"m","input":"Hi",
			"tools":[{"type":"function","name":"a","strict":false}],"tool_choice":`+tc+`}`)
		out, rerr := EncodeAnthropicRequest(g, "up")
		if rerr != nil {
			t.Fatalf("encode: %v", rerr)
		}
		if !strings.Contains(string(out), want) {
			t.Fatalf("%s must map to %s: %s", tc, want, out)
		}
	}
	// allowed_tools is validated then dropped with a loss.
	_, losses, rerr := DecodeResponsesForAnthropic([]byte(`{"model":"m","input":"Hi",
		"tools":[{"type":"function","name":"a","strict":false}],
		"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"a"}]}}`), DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	if len(losses) != 1 || !strings.Contains(losses[0].Field, "tool_choice") {
		t.Fatalf("allowed_tools must be a single loss: %v", losses)
	}
}

func TestRFAHistoryIdentity(t *testing.T) {
	decodeRFAExpect(t, `{"model":"m","input":[
		{"type":"function_call","call_id":"c1","name":"a","arguments":"{}"},
		{"type":"function_call_output","call_id":"c9","output":"x"}]}`, 400, CodeInvalidRequest)
	decodeRFAExpect(t, `{"model":"m","input":[
		{"type":"function_call","call_id":"c1","name":"a","arguments":"{}"},
		{"type":"function_call","call_id":"c1","name":"b","arguments":"{}"}]}`, 400, CodeInvalidRequest)
	decodeRFAExpect(t, `{"model":"m","input":"Hi","tool_choice":{"type":"function","name":"ghost"},
		"tools":[{"type":"function","name":"a","strict":false}]}`, 400, CodeInvalidRequest)
	decodeRFAExpect(t, `{"model":"m","input":"Hi","tool_choice":{"type":"function","name":"a"}}`, 400, CodeInvalidRequest)
	// Historical namespaces bind independently of this turn's declarations.
	g, losses, rerr := DecodeResponsesForAnthropic([]byte(`{"model":"m","input":[
		{"type":"function_call","call_id":"c1","name":"a","namespace":"ns","arguments":"{}"}]}`), DefaultLimits())
	if rerr != nil || len(losses) != 0 || g.ToolPlan == nil || g.Messages[0].ToolCalls[0].Name != NamespaceAlias("ns", "a") {
		t.Fatalf("namespaced history must retain its explicit binding: %v %v", rerr, losses)
	}
	// Hosted types still have no bridge mapping; custom tools are now
	// emulated as string functions with an approximated loss.
	decodeRFAExpect(t, `{"model":"m","input":"Hi","tools":[{"type":"web_search_preview"}]}`, 400, CodeInvalidGeneration)
	_, losses, rerr = DecodeResponsesForAnthropic([]byte(`{"model":"m","input":"Hi","tools":[{"type":"custom","name":"c"}]}`), DefaultLimits())
	if rerr != nil {
		t.Fatalf("custom must decode as emulated: %v", rerr)
	}
	if len(losses) == 0 {
		t.Fatalf("custom must report approximation: %v", losses)
	}
	for _, l := range losses {
		if l.Action != "approximated" {
			t.Fatalf("custom loss must be approximated: %+v", losses)
		}
	}
	if _, rerr := EnforcePolicy(losses, false); rerr == nil || rerr.Code != CodeTranslationLoss {
		t.Fatalf("strict must refuse custom emulation: %v", rerr)
	}
}

func TestRFAMediaReasoningRefused(t *testing.T) {
	decodeRFAExpect(t, `{"model":"m","input":"Hi","reasoning":{"effort":"medium"}}`, 400, CodeInvalidGeneration)
	decodeRFAExpect(t, `{"model":"m","input":[{"type":"message","role":"user",
		"content":[{"type":"input_file","file_id":"f"}]}]}`, 400, CodeInvalidGeneration)
	// Image parts are validated then diagnosed as losses (never forwarded).
	_, losses, rerr := DecodeResponsesForAnthropic([]byte(`{"model":"m","input":[
		{"type":"message","role":"user","content":[{"type":"input_image","image_url":"https://x/y.png"}]}]}`), DefaultLimits())
	if rerr != nil {
		t.Fatalf("image shape must decode: %v", rerr)
	}
	if len(losses) == 0 {
		t.Fatalf("image must be a loss: %v", losses)
	}
}

func TestRFALossesBoundedValueFree(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`{"model":"m","input":"Hi"`)
	for i := 0; i < 200; i++ {
		sb.WriteString(`,"extra` + strings.Repeat("x", 4) + `"`)
		_ = i
		break
	}
	// Many distinct unbridged-but-known fields via include-style arrays stay bounded.
	raw := `{"model":"m","input":"Hi","service_tier":"auto","truncation":"auto",
		"max_tool_calls":3,"top_logprobs":2,"safety_identifier":"s","prompt_cache_key":"k","user":"u",
		"prompt_cache_retention":"in_memory","store":true}`
	_, losses, rerr := DecodeResponsesForAnthropic([]byte(raw), DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	hdr := EncodeLosses(losses)
	if len(losses) > 64 {
		t.Fatalf("losses unbounded: %d", len(losses))
	}
	for _, l := range losses {
		if l.Action != "dropped" && l.Action != "approximated" {
			t.Fatalf("bad action: %+v", l)
		}
	}
	if strings.Contains(hdr, "Hi") {
		t.Fatalf("loss header must be value-free: %q", hdr)
	}
	prev := ""
	for _, l := range losses {
		if l.Field < prev {
			t.Fatalf("losses not sorted: %v", losses)
		}
		prev = l.Field
	}
}
