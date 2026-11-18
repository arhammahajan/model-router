package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustDecodeResponses(t *testing.T, raw string) (*Generation, []Loss) {
	t.Helper()
	g, losses, rerr := DecodeResponsesRequest([]byte(raw), DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode failed: %v", rerr)
	}
	return g, losses
}

func TestResponsesDecodeText(t *testing.T) {
	g, losses := mustDecodeResponses(t, `{"model":"m","input":"Hello",
		"instructions":"Be helpful.","temperature":0.5,"top_p":0.9,
		"max_output_tokens":64,"stream":false}`)
	if len(losses) != 0 {
		t.Fatalf("unexpected losses: %v", losses)
	}
	if g.Model != "m" || len(g.Messages) != 2 {
		t.Fatalf("bad decode: %+v", g)
	}
	if g.Messages[0].Role != RoleSystem || g.Messages[1].Role != RoleUser {
		t.Fatalf("bad roles: %+v", g.Messages)
	}
	if g.Temperature == nil || *g.Temperature != 0.5 {
		t.Fatalf("bad temperature: %+v", g.Temperature)
	}
	if g.MaxTokens == nil || *g.MaxTokens != 64 {
		t.Fatalf("bad max tokens: %+v", g.MaxTokens)
	}
}

func TestResponsesDecodeArrayInputWithHistory(t *testing.T) {
	g, losses := mustDecodeResponses(t, `{"model":"m","input":[
		{"type":"message","role":"user","content":"What is 2+2?"},
		{"type":"function_call","id":"fc_1","call_id":"call_1","name":"calc","arguments":"{\"x\":2}"},
		{"type":"function_call_output","call_id":"call_1","output":"4"},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"Thanks"}]}
		],"tools":[{"type":"function","name":"calc","strict":false,"parameters":{"type":"object"}}]}`)
	if len(losses) != 0 {
		t.Fatalf("unexpected losses: %v", losses)
	}
	if len(g.Messages) != 4 {
		t.Fatalf("bad message count: %+v", g.Messages)
	}
	if g.Messages[1].ToolCalls[0].ID != "call_1" {
		t.Fatalf("bad call id: %+v", g.Messages[1].ToolCalls)
	}
	if string(g.Messages[1].ToolCalls[0].Arguments) != `{"x":2}` {
		t.Fatalf("arguments not preserved: %s", g.Messages[1].ToolCalls[0].Arguments)
	}
	if g.Messages[2].Results[0].Content != "4" {
		t.Fatalf("bad result: %+v", g.Messages[2].Results)
	}
	out, rerr := EncodeChatRequest(g, AbsentEffort(), "upstream-model")
	if rerr != nil {
		t.Fatalf("encode failed: %v", rerr)
	}
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("bad upstream messages: %v", body["messages"])
	}
	// History call identity must survive the Chat wire exactly.
	assistant := msgs[1].(map[string]any)
	call := assistant["tool_calls"].([]any)[0].(map[string]any)
	if call["id"] != "call_1" {
		t.Fatalf("call identity lost: %v", call)
	}
	toolMsg := msgs[2].(map[string]any)
	if toolMsg["tool_call_id"] != "call_1" || toolMsg["content"] != "4" {
		t.Fatalf("result identity lost: %v", toolMsg)
	}
}

func TestResponsesDecodeForcedChoiceWire(t *testing.T) {
	g, _ := mustDecodeResponses(t, `{"model":"m","input":"Hi",
		"tools":[{"type":"function","name":"get_weather","description":"d","strict":false,
		"parameters":{"type":"object","properties":{"city":{"type":"string"}}}}],
		"tool_choice":{"type":"function","name":"get_weather"},
		"parallel_tool_calls":false}`)
	out, rerr := EncodeChatRequest(g, AbsentEffort(), "upstream")
	if rerr != nil {
		t.Fatalf("encode failed: %v", rerr)
	}
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	tc, ok := body["tool_choice"].(map[string]any)
	if !ok || tc["type"] != "function" {
		t.Fatalf("forced choice is not the Chat object union: %v", body["tool_choice"])
	}
	fn := tc["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Fatalf("bad forced name: %v", fn)
	}
	if body["parallel_tool_calls"] != false {
		t.Fatalf("parallel flag lost: %v", body)
	}
	// Tool schema numbers must survive exactly (no float64 rewrite).
	tools := body["tools"].([]any)
	params := tools[0].(map[string]any)["function"].(map[string]any)["parameters"]
	if !strings.Contains(jsonString(params), `"city"`) {
		t.Fatalf("schema lost: %v", params)
	}
}

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestResponsesDecodeEmptyArgsPreserved(t *testing.T) {
	g, _ := mustDecodeResponses(t, `{"model":"m","input":[
		{"type":"function_call","call_id":"call_1","name":"ping"},
		{"type":"function_call_output","call_id":"call_1","output":"ok"}
		],"tools":[{"type":"function","name":"ping","strict":false}]}`)
	if string(g.Messages[0].ToolCalls[0].Arguments) != "{}" {
		t.Fatalf("empty args vanished: %s", g.Messages[0].ToolCalls[0].Arguments)
	}
	out, rerr := EncodeChatRequest(g, AbsentEffort(), "upstream")
	if rerr != nil {
		t.Fatalf("encode failed: %v", rerr)
	}
	if !strings.Contains(string(out), `"arguments":"{}"`) {
		t.Fatalf("empty args lost on wire: %s", out)
	}
}

func TestResponsesDecodeLosses(t *testing.T) {
	g, losses := mustDecodeResponses(t, `{"model":"m","input":"Hi",
		"store":true,"truncation":"auto","max_tool_calls":2,"client_metadata":{"client":"codex"},
		"input":"Hi","stream_options":{"include_obfuscation":true}}`)
	_ = g
	fields := map[string]bool{}
	for _, l := range losses {
		fields[l.Field] = true
		if l.Action != "dropped" {
			t.Fatalf("bad action: %+v", l)
		}
	}
	for _, want := range []string{"store", "truncation", "max_tool_calls", "client_metadata", "stream_options.include_obfuscation"} {
		if !fields[want] {
			t.Fatalf("missing loss %q in %v", want, losses)
		}
	}
	if _, rerr := EnforcePolicy(losses, false); rerr == nil {
		t.Fatal("strict policy should refuse losses")
	}
	kept, rerr := EnforcePolicy(losses, true)
	if rerr != nil || len(kept) != len(losses) {
		t.Fatalf("lossy policy should keep losses: %v %v", kept, rerr)
	}
}

func TestResponsesOptionalCachedWebSearch(t *testing.T) {
	const cachedSearch = `{"type":"web_search","external_web_access":false}`
	const function = `{"type":"function","name":"lookup","strict":false}`
	for _, tools := range []string{cachedSearch, cachedSearch + "," + function} {
		raw := `{"model":"m","input":"Hi","tool_choice":"auto","tools":[` + tools + `]}`
		gen, losses := mustDecodeResponses(t, raw)
		if len(losses) != 1 || losses[0].Field != "tools[0]" || losses[0].Action != "dropped" {
			t.Fatalf("unexpected search losses: %v", losses)
		}
		if _, rerr := EnforcePolicy(losses, false); rerr == nil || rerr.Code != CodeTranslationLoss {
			t.Fatalf("strict route must reject the search loss: %v", rerr)
		}
		body, rerr := EncodeChatRequest(gen, AbsentEffort(), "upstream")
		if rerr != nil || strings.Contains(string(body), "web_search") ||
			strings.Contains(string(body), "lookup") != (tools != cachedSearch) {
			t.Fatalf("wrong set of upstream tools: %s, %v", body, rerr)
		}
	}
	for _, raw := range []string{
		`{"model":"m","input":"Hi","tool_choice":"required","tools":[` + cachedSearch + `,` + function + `]}`,
		`{"model":"m","input":"Hi","tools":[{"type":"web_search","external_web_access":true}]}`,
		`{"model":"m","input":"Hi","tools":[{"type":"web_search","external_web_access":"false"}]}`,
		`{"model":"m","input":"Hi","tools":[{"type":"web_search","external_web_access":false,"filters":{}}]}`,
	} {
		if _, _, rerr := DecodeResponsesRequest([]byte(raw), DefaultLimits()); rerr == nil || rerr.Status != 400 {
			t.Fatalf("unexpected admission: %v", rerr)
		}
	}
}

func TestResponsesDecodeImageLossAndFileRefusal(t *testing.T) {
	_, losses, rerr := DecodeResponsesRequest([]byte(`{"model":"m","input":[
		{"type":"message","role":"user","content":[
			{"type":"input_text","text":"see "},
			{"type":"input_image","image_url":"https://example.com/i.png"}]}]}`), DefaultLimits())
	if rerr != nil {
		t.Fatalf("image should be a loss, not an error: %v", rerr)
	}
	if len(losses) != 1 || !strings.Contains(losses[0].Field, "input_image") {
		t.Fatalf("bad losses: %v", losses)
	}
	_, _, rerr = DecodeResponsesRequest([]byte(`{"model":"m","input":[
		{"type":"message","role":"user","content":[{"type":"input_file","file_id":"f_1"}]}]}`), DefaultLimits())
	if rerr == nil {
		t.Fatal("file input should require a same-protocol route")
	}
}

func TestResponsesStatelessRefusals(t *testing.T) {
	// Note (reverse named-effort vertical): reasoning.effort for the Chat
	// destination is no longer a stateless refusal; it decodes into the
	// closed Responses-origin intent and is planned like the forward unit.
	// Source: reasoning.py emit_effort + effort branch (Chat<->Responses same
	// enum exact) and protocol.py closed reasoning{effort,summary,
	// generate_summary}. Summaries and other-destination effort still refuse
	// (see TestRCDecodeSummariesRetainRefusalBothModes).
	cases := []string{
		`{"model":"m","input":"Hi","previous_response_id":"resp_1"}`,
		`{"model":"m","input":"Hi","conversation":"conv_1"}`,
		`{"model":"m","input":"Hi","conversation":{"id":"conv_1"}}`,
		`{"model":"m","input":"Hi","prompt":{"id":"pmpt_1"}}`,
		`{"model":"m","input":"Hi","background":true}`,
		`{"model":"m","input":"Hi","metadata":{"k":"v"}}`,
	}
	for _, raw := range cases {
		_, _, rerr := DecodeResponsesRequest([]byte(raw), DefaultLimits())
		if rerr == nil {
			t.Fatalf("expected refusal for %s", raw)
		}
		// Even lossy mode must not accept upstream-owned state.
		_, losses, rerr2 := DecodeResponsesRequest([]byte(raw), DefaultLimits())
		_ = losses
		if rerr2 == nil {
			t.Fatalf("lossy accepted stateful field: %s", raw)
		}
		if _, perr := EnforcePolicy([]Loss{{Field: "x", Reason: "y", Action: "dropped"}}, true); perr != nil {
			t.Fatal("lossy sanity check failed")
		}
	}
	// Explicit null/false state markers mean absent.
	g, _ := mustDecodeResponses(t, `{"model":"m","input":"Hi",
		"previous_response_id":null,"conversation":null,"prompt":null,
		"background":false,"reasoning":null,"metadata":null}`)
	if len(g.Messages) != 1 {
		t.Fatalf("null state markers should mean absent: %+v", g)
	}
}

func TestResponsesStatefulCode(t *testing.T) {
	// Well-formed non-null state refs and background:true must report the
	// stable code unsupported_responses_state (400, no I/O, both modes).
	stateCases := []string{
		`{"model":"m","input":"Hi","previous_response_id":"resp_1"}`,
		`{"model":"m","input":"Hi","conversation":"conv_1"}`,
		`{"model":"m","input":"Hi","conversation":{"id":"conv_1"}}`,
		`{"model":"m","input":"Hi","prompt":{"id":"pmpt_1"}}`,
		`{"model":"m","input":"Hi","background":true}`,
	}
	for _, raw := range stateCases {
		_, _, rerr := DecodeResponsesRequest([]byte(raw), DefaultLimits())
		if rerr == nil {
			t.Fatalf("expected refusal for %s", raw)
		}
		if rerr.Status != 400 || rerr.Code != "unsupported_responses_state" {
			t.Fatalf("state %s must be 400 unsupported_responses_state, got %v", raw, rerr)
		}
	}
	// Malformed types remain invalid_request validation.
	malformed := []string{
		`{"model":"m","input":"Hi","previous_response_id":123}`,
		`{"model":"m","input":"Hi","previous_response_id":""}`,
		`{"model":"m","input":"Hi","conversation":123}`,
		`{"model":"m","input":"Hi","conversation":""}`,
		`{"model":"m","input":"Hi","prompt":"pmpt_1"}`,
		`{"model":"m","input":"Hi","prompt":123}`,
		`{"model":"m","input":"Hi","background":"yes"}`,
		`{"model":"m","input":"Hi","background":123}`,
	}
	for _, raw := range malformed {
		_, _, rerr := DecodeResponsesRequest([]byte(raw), DefaultLimits())
		if rerr == nil || rerr.Status != 400 || rerr.Code != CodeInvalidRequest {
			t.Fatalf("malformed %s must be 400 invalid_request, got %v", raw, rerr)
		}
	}
	// Combination of previous_response_id + conversation is invalid_request.
	_, _, rerr := DecodeResponsesRequest([]byte(`{"model":"m","input":"Hi","previous_response_id":"resp_1","conversation":"conv_1"}`), DefaultLimits())
	if rerr == nil || rerr.Status != 400 || rerr.Code != CodeInvalidRequest {
		t.Fatalf("combined state must be 400 invalid_request, got %v", rerr)
	}
	// background:false/null and null state refs are absent, not state.
	for _, raw := range []string{
		`{"model":"m","input":"Hi","background":false}`,
		`{"model":"m","input":"Hi","background":null}`,
		`{"model":"m","input":"Hi","previous_response_id":null}`,
		`{"model":"m","input":"Hi","conversation":null}`,
		`{"model":"m","input":"Hi","prompt":null}`,
	} {
		if _, _, rerr := DecodeResponsesRequest([]byte(raw), DefaultLimits()); rerr != nil {
			t.Fatalf("absent marker %s must decode, got %v", raw, rerr)
		}
	}
}

func TestResponsesDeveloperRoleRequiresLossyBridge(t *testing.T) {
	gen, losses, err := DecodeResponsesRequest([]byte(`{"model":"m","input":[
		{"type":"message","role":"developer","content":"Be terse"}]}`), DefaultLimits())
	if err != nil || len(gen.Messages) != 1 || gen.Messages[0].Role != RoleSystem ||
		len(losses) != 1 || losses[0].Field != "input[0].role" || losses[0].Action != "approximated" {
		t.Fatalf("developer instruction and role loss must be preserved: %v %v %v", gen, losses, err)
	}
	if _, err := EnforcePolicy(losses, false); err == nil || err.Code != CodeTranslationLoss {
		t.Fatalf("developer role requires lossy policy, got %v", err)
	}
}

func TestResponsesDecodeUnknownFieldRejected(t *testing.T) {
	_, _, rerr := DecodeResponsesRequest([]byte(`{"model":"m","input":"Hi","future_field":1}`), DefaultLimits())
	if rerr == nil || rerr.Status != 400 {
		t.Fatalf("unknown top-level field should be a 400, got %v", rerr)
	}
	_, _, rerr = DecodeResponsesRequest([]byte(`{"model":"m","input":[
		{"type":"message","role":"user","content":"Hi","parity":"yes"}]}`), DefaultLimits())
	if rerr == nil || rerr.Status != 400 {
		t.Fatalf("unknown nested member should be a 400 even in lossy decode, got %v", rerr)
	}
}

func TestResponsesDecodeMalformedValuesRejected(t *testing.T) {
	cases := []string{
		`{"model":"m","input":"Hi","store":"yes"}`,
		`{"model":"m","input":"Hi","truncation":"sometimes"}`,
		`{"model":"m","input":"Hi","stream":"yes"}`,
		`{"model":"m","input":"Hi","stream_options":{"include_usage":true}}`,
		`{"model":"m","input":"Hi","tool_choice":{"type":"function"}}`,
		`{"model":"m","input":[]}`,
		`{"model":"m","input":[{"type":"message","role":"user"}]}`,
		`{"model":"m","input":[{"type":"web_search_call","id":"w_1"}]}`,
		`{"model":"m","input":"Hi","tools":[{"type":"web_search"}]}`,
		`{"model":"m","input":"Hi","tool_choice":{"type":"mcp","server_label":"s"}}`,
		`{"model":"m","input":"Hi","tools":[{"type":"function","name":"a"},{"type":"function","name":"a"}]}`,
	}
	for _, raw := range cases {
		_, _, rerr := DecodeResponsesRequest([]byte(raw), DefaultLimits())
		if rerr == nil {
			t.Fatalf("expected 400 for %s", raw)
		}
		if rerr.Status != 400 {
			t.Fatalf("expected 400 for %s, got %v", raw, rerr)
		}
	}
}

func TestResponsesDecodeHistoryIdentityMismatch(t *testing.T) {
	_, _, rerr := DecodeResponsesRequest([]byte(`{"model":"m","input":[
		{"type":"function_call","call_id":"call_1","name":"a","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_2","output":"x"}],
		"tools":[{"type":"function","name":"a"}]}`), DefaultLimits())
	if rerr == nil || !strings.Contains(rerr.Msg, "unmatched") {
		t.Fatalf("expected unmatched result error, got %v", rerr)
	}
}

func TestResponsesDecodeAllowedToolsLossy(t *testing.T) {
	raw := `{"model":"m","input":"Hi",
		"tools":[{"type":"function","name":"a","strict":false}],
		"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"a"}]}}`
	_, losses, rerr := DecodeResponsesRequest([]byte(raw), DefaultLimits())
	if rerr != nil {
		t.Fatalf("allowed_tools should decode to a loss: %v", rerr)
	}
	if len(losses) != 1 || losses[0].Field != "tool_choice" {
		t.Fatalf("bad losses: %v", losses)
	}
	if _, rerr := EnforcePolicy(losses, false); rerr == nil {
		t.Fatal("strict should refuse allowed_tools")
	}
	g, kept, rerr := DecodeResponsesRequest([]byte(raw), DefaultLimits())
	_ = g
	kept, rerr = EnforcePolicy(kept, true)
	if rerr != nil {
		t.Fatalf("lossy should reconcile allowed_tools: %v", rerr)
	}
	_ = kept
	g2, losses2, rerr := DecodeResponsesRequest([]byte(raw), DefaultLimits())
	if rerr != nil {
		t.Fatal(rerr)
	}
	kept2, _ := EnforcePolicy(losses2, true)
	_ = kept2
	if g2.Choice != nil {
		t.Fatalf("lossy allowed_tools should reconcile to no forced choice: %+v", g2.Choice)
	}
}

func TestResponsesDecodeInstructionsOrder(t *testing.T) {
	// Per the Responses create reference, request `instructions` is a
	// string (or null). Array forms appear only on echoed response
	// objects and must be rejected on ingress.
	g, _ := mustDecodeResponses(t, `{"model":"m",
		"instructions":"Be helpful.",
		"input":"Hi"}`)
	if len(g.Messages) != 2 || g.Messages[0].Role != RoleSystem {
		t.Fatalf("instructions should lead as system: %+v", g.Messages)
	}
	if g.Messages[0].Texts[0].Text != "Be helpful." {
		t.Fatalf("instruction text broken: %q", g.Messages[0].Texts[0].Text)
	}
	_, _, rerr := DecodeResponsesRequest([]byte(`{"model":"m",
		"instructions":[{"type":"message","role":"system","content":"Hi"}],
		"input":"Hi"}`), DefaultLimits())
	if rerr == nil {
		t.Fatal("array instructions should be rejected on request ingress")
	}
}

func TestResponsesDecodeOutputConcatApproximated(t *testing.T) {
	_, losses, rerr := DecodeResponsesRequest([]byte(`{"model":"m","input":[
		{"type":"function_call","call_id":"c1","name":"a","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":[
			{"type":"input_text","text":"a"},{"type":"input_text","text":"b"}]}],
		"tools":[{"type":"function","name":"a","strict":false}]}`), DefaultLimits())
	if rerr != nil {
		t.Fatalf("array output should concatenate: %v", rerr)
	}
	found := false
	for _, l := range losses {
		if l.Field == "input[1].output" && l.Action == "approximated" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing approximated loss: %v", losses)
	}
}

func TestResponsesDecodeStrictImplicitIsLoss(t *testing.T) {
	for _, raw := range []string{
		`{"model":"m","input":"Hi","tools":[{"type":"function","name":"a"}]}`,
		`{"model":"m","input":"Hi","tools":[{"type":"function","name":"a","strict":null}]}`,
	} {
		_, losses, rerr := DecodeResponsesRequest([]byte(raw), DefaultLimits())
		if rerr != nil {
			t.Fatalf("decode failed for %s: %v", raw, rerr)
		}
		found := false
		for _, l := range losses {
			if l.Field == "tools[0].strict" {
				found = true
			}
		}
		if !found {
			t.Fatalf("implicit strict default must be diagnosed as loss for %s, got %v", raw, losses)
		}
		if _, rerr := EnforcePolicy(losses, false); rerr == nil {
			t.Fatalf("strict policy must refuse implicit strict for %s", raw)
		}
	}
}

func TestResponsesDecodeStrictExplicitRetained(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{`{"model":"m","input":"Hi","tools":[{"type":"function","name":"a","strict":false}]}`, false},
		{`{"model":"m","input":"Hi","tools":[{"type":"function","name":"a","strict":true}]}`, true},
	} {
		g, losses, rerr := DecodeResponsesRequest([]byte(tc.raw), DefaultLimits())
		if rerr != nil {
			t.Fatalf("decode failed: %v", rerr)
		}
		for _, l := range losses {
			if l.Field == "tools[0].strict" {
				t.Fatalf("explicit strict must not be a loss for %s, got %v", tc.raw, losses)
			}
		}
		if g.Tools[0].Strict == nil || *g.Tools[0].Strict != tc.want {
			t.Fatalf("explicit strict not retained for %s: %+v", tc.raw, g.Tools[0])
		}
		out, rerr := EncodeChatRequest(g, AbsentEffort(), "upstream")
		if rerr != nil {
			t.Fatalf("encode failed: %v", rerr)
		}
		var body map[string]any
		if err := json.Unmarshal(out, &body); err != nil {
			t.Fatal(err)
		}
		fn := body["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
		if fn["strict"] != tc.want {
			t.Fatalf("Chat wire must carry explicit strict %v, got %v", tc.want, fn)
		}
	}
	_, _, rerr := DecodeResponsesRequest([]byte(`{"model":"m","input":"Hi","tools":[{"type":"function","name":"a","strict":"yes"}]}`), DefaultLimits())
	if rerr == nil || rerr.Status != 400 {
		t.Fatalf("malformed strict must be 400, got %v", rerr)
	}
}

func TestResponsesDecodeStoreExact(t *testing.T) {
	g, losses, rerr := DecodeResponsesRequest([]byte(`{"model":"m","input":"Hi","store":false}`), DefaultLimits())
	if rerr != nil {
		t.Fatalf("store:false should decode: %v", rerr)
	}
	for _, l := range losses {
		if l.Field == "store" {
			t.Fatalf("store:false must not be a loss, got %v", losses)
		}
	}
	_ = g
	if _, rerr := EnforcePolicy(losses, false); rerr != nil {
		t.Fatalf("strict policy must accept store:false: %v", rerr)
	}
	_, losses, rerr = DecodeResponsesRequest([]byte(`{"model":"m","input":"Hi","store":true}`), DefaultLimits())
	if rerr != nil {
		t.Fatalf("store:true should decode to a loss: %v", rerr)
	}
	found := false
	for _, l := range losses {
		if l.Field == "store" {
			found = true
		}
	}
	if !found {
		t.Fatalf("store:true must be diagnosed as loss, got %v", losses)
	}
	_, _, rerr = DecodeResponsesRequest([]byte(`{"model":"m","input":"Hi","store":"yes"}`), DefaultLimits())
	if rerr == nil || rerr.Status != 400 {
		t.Fatalf("malformed store must be 400, got %v", rerr)
	}
}

func TestResponsesDecodeNumericStringRejected(t *testing.T) {
	cases := []string{
		`{"model":"m","input":"Hi","temperature":"0.5"}`,
		`{"model":"m","input":"Hi","top_p":"0.9"}`,
		`{"model":"m","input":"Hi","max_output_tokens":"64"}`,
		`{"model":"m","input":"Hi","top_logprobs":"5"}`,
		`{"model":"m","input":"Hi","max_tool_calls":"2"}`,
	}
	for _, raw := range cases {
		_, _, rerr := DecodeResponsesRequest([]byte(raw), DefaultLimits())
		if rerr == nil || rerr.Status != 400 {
			t.Fatalf("numeric string must be 400 for %s, got %v", raw, rerr)
		}
	}
}

func TestResponsesDecodeUnknownNestedRejected(t *testing.T) {
	cases := []string{
		`{"model":"m","input":"Hi","text":{"format":{"type":"text","bogus":1}}}`,
		`{"model":"m","input":"Hi","text":{"format":{"type":"text","strict":"yes"}}}`,
		`{"model":"m","input":"Hi","text":{"format":{"type":"text","name":123}}}`,
		`{"model":"m","input":"Hi","prompt_cache_options":{"mode":"implicit","bogus":true}}`,
		`{"model":"m","input":"Hi","client_metadata":{"client":null}}`,
		`{"model":"m","input":"Hi","client_metadata":{"client":{"nested":true}}}`,
		`{"model":"m","input":"Hi","client_metadata":[]}`,
		`{"model":"m","input":"Hi","context_management":[{"type":"compaction","bogus":1}]}`,
		`{"model":"m","input":"Hi","moderation":{"model":"m","bogus":{}}}`,
	}
	for _, raw := range cases {
		_, _, rerr := DecodeResponsesRequest([]byte(raw), DefaultLimits())
		if rerr == nil || rerr.Status != 400 {
			t.Fatalf("unknown/malformed nested member must be 400 for %s, got %v", raw, rerr)
		}
	}
}
