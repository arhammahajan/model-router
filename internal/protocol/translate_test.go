package protocol

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

func mustDecode(t *testing.T, raw string) (*Generation, []Loss) {
	t.Helper()
	g, losses, rerr := DecodeChatRequest([]byte(raw), DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode failed: %v", rerr)
	}
	return g, losses
}

func TestDecodeTextRequest(t *testing.T) {
	g, losses := mustDecode(t, `{"model":"m","messages":[
		{"role":"system","content":"You are helpful."},
		{"role":"user","content":"Hi"}],
		"temperature":0.5,"max_tokens":64,"stop":["END"],"stream":false}`)
	if len(losses) != 0 {
		t.Fatalf("unexpected losses: %v", losses)
	}
	if g.Model != "m" || len(g.Messages) != 2 {
		t.Fatalf("bad decode: %+v", g)
	}
	if g.Temperature == nil || *g.Temperature != 0.5 {
		t.Fatalf("bad temperature: %+v", g.Temperature)
	}
	out, rerr := EncodeAnthropicRequest(g, "upstream-model")
	if rerr != nil {
		t.Fatalf("encode failed: %v", rerr)
	}
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "upstream-model" {
		t.Fatalf("model not replaced: %v", body["model"])
	}
	sys, ok := body["system"].([]any)
	if !ok || len(sys) != 1 {
		t.Fatalf("system not hoisted: %v", body["system"])
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["role"] != "user" {
		t.Fatalf("messages wrong: %v", body["messages"])
	}
	if body["max_tokens"] != float64(64) {
		t.Fatalf("max_tokens wrong: %v", body["max_tokens"])
	}
	if _, ok := body["stream"]; !ok {
		t.Fatalf("stream must be explicit: %v", body)
	}
}

func TestDecodeRejectsUnknownField(t *testing.T) {
	_, _, rerr := DecodeChatRequest([]byte(`{"model":"m","messages":[],"nonsense_field":1}`), DefaultLimits())
	if rerr == nil || rerr.Status != 400 {
		t.Fatalf("expected 400, got %v", rerr)
	}
}

func TestStrictRejectsUnmappedKnownField(t *testing.T) {
	_, losses := mustDecode(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"seed":7}`)
	if len(losses) == 0 {
		t.Fatalf("expected losses, got none")
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
	if strings.Contains(hdr, "hi") {
		t.Fatalf("loss header must not carry values: %q", hdr)
	}
}

func TestMetadataAlwaysRejected(t *testing.T) {
	_, _, rerr := DecodeChatRequest([]byte(`{"model":"m","messages":[],"metadata":{"user_id":"x"}}`), DefaultLimits())
	if rerr == nil || rerr.Status != 400 {
		t.Fatalf("expected 400, got %v", rerr)
	}
}

func TestToolsRoundTrip(t *testing.T) {
	g, losses := mustDecode(t, `{"model":"m","messages":[
		{"role":"user","content":"What time?"},
		{"role":"assistant","content":"","tool_calls":[
			{"id":"call_1","type":"function","function":{"name":"get_time","arguments":"{\"tz\":\"UTC\"}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"noon"}],
		"tools":[{"type":"function","function":{"name":"get_time","description":"clock",
			"parameters":{"type":"object","properties":{"tz":{"type":"string"}}}}}],
		"tool_choice":{"type":"function","function":{"name":"get_time"}},
		"parallel_tool_calls":false}`)
	if len(losses) != 0 {
		t.Fatalf("unexpected losses: %v", losses)
	}
	out, rerr := EncodeAnthropicRequest(g, "up")
	if rerr != nil {
		t.Fatalf("encode failed: %v", rerr)
	}
	var body struct {
		Tools []struct {
			Name        string         `json:"name"`
			InputSchema map[string]any `json:"input_schema"`
		} `json:"tools"`
		ToolChoice map[string]any `json:"tool_choice"`
		Messages   []struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string `json:"type"`
				ID        string `json:"id"`
				Name      string `json:"name"`
				ToolUseID string `json:"tool_use_id"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Tools) != 1 || body.Tools[0].Name != "get_time" {
		t.Fatalf("tools wrong: %v", body.Tools)
	}
	if body.ToolChoice["type"] != "tool" || body.ToolChoice["name"] != "get_time" {
		t.Fatalf("choice wrong: %v", body.ToolChoice)
	}
	if body.ToolChoice["disable_parallel_tool_use"] != true {
		t.Fatalf("parallel mapping wrong: %v", body.ToolChoice)
	}
	// Identity preserved across history.
	found := false
	for _, m := range body.Messages {
		for _, c := range m.Content {
			if c.Type == "tool_use" && c.ID == "call_1" && c.Name == "get_time" {
				found = true
			}
			if c.Type == "tool_result" && c.ToolUseID == "call_1" {
				found = found && true
			}
		}
	}
	if !found {
		t.Fatalf("identities lost: %s", out)
	}
}

func TestResponseJSON(t *testing.T) {
	raw := `{"id":"msg_1","type":"message","role":"assistant","model":"up",
		"content":[{"type":"text","text":"Hi "},{"type":"text","text":"there"},
			{"type":"tool_use","id":"to_1","name":"get_time","input":{"tz":"UTC"}}],
		"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5}}`
	out, rerr := TranslateAnthropicJSON([]byte(raw), "public", DefaultLimits())
	if rerr != nil {
		t.Fatalf("translate failed: %v", rerr)
	}
	var body struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
			Completion   int `json:"completion_tokens"`
			Total        int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	if body.Model != "public" {
		t.Fatalf("public model not restored: %q", body.Model)
	}
	if body.Choices[0].Message.Content != "Hi there" {
		t.Fatalf("content wrong: %q", body.Choices[0].Message.Content)
	}
	if body.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish wrong: %q", body.Choices[0].FinishReason)
	}
	tc := body.Choices[0].Message.ToolCalls[0]
	if tc.ID != "to_1" || tc.Function.Name != "get_time" {
		t.Fatalf("identity lost: %+v", tc)
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil || args["tz"] != "UTC" {
		t.Fatalf("args wrong: %q", tc.Function.Arguments)
	}
	if body.Usage.Total != 15 {
		t.Fatalf("usage wrong: %+v", body.Usage)
	}
}

func TestResponseJSONRejectsThinking(t *testing.T) {
	raw := `{"id":"m","type":"message","role":"assistant","model":"up",
		"content":[{"type":"thinking","thinking":"hmm"}],
		"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	if _, rerr := TranslateAnthropicJSON([]byte(raw), "public", DefaultLimits()); rerr == nil || rerr.Status != 502 {
		t.Fatalf("expected 502, got %v", rerr)
	}
	// Same in lossy mode: response losses are failures, not header diagnostics.
}

func TestSSETextStream(t *testing.T) {
	upstream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":8}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	var sb strings.Builder
	tr := &SSETranslator{PublicModel: "public", IncludeUsage: true, Limits: DefaultLimits()}
	if rerr := tr.Translate(strings.NewReader(upstream), &sb, nil); rerr != nil {
		t.Fatalf("stream failed: %v", rerr)
	}
	out := sb.String()
	if !strings.Contains(out, `"content":"Hello"`) {
		t.Fatalf("missing text delta: %s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("missing finish: %s", out)
	}
	if !strings.Contains(out, `"prompt_tokens":8`) {
		t.Fatalf("missing usage: %s", out)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Fatalf("missing terminator: %s", out)
	}
}

func TestSSETruncationFails(t *testing.T) {
	upstream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n"
	var sb strings.Builder
	tr := &SSETranslator{PublicModel: "public", Limits: DefaultLimits()}
	if rerr := tr.Translate(strings.NewReader(upstream), &sb, nil); rerr == nil {
		t.Fatalf("truncated stream must fail")
	}
	if strings.Contains(sb.String(), "[DONE]") {
		t.Fatalf("truncated stream must not emit success marker")
	}
}

func TestSSEUnknownEventFails(t *testing.T) {
	upstream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\"}}\n\n" +
		"event: citational_dance\ndata: {}\n\n"
	var sb strings.Builder
	tr := &SSETranslator{PublicModel: "public", Limits: DefaultLimits()}
	if rerr := tr.Translate(strings.NewReader(upstream), &sb, nil); rerr == nil {
		t.Fatalf("unknown event must fail")
	}
}

// --- review regressions: request validation is strict even in lossy mode ---

func decodeExpect(t *testing.T, raw string, wantStatus int) {
	t.Helper()
	_, _, rerr := DecodeChatRequest([]byte(raw), DefaultLimits())
	if rerr == nil {
		t.Fatalf("expected status %d, got success for %s", wantStatus, raw)
	}
	if rerr.Status != wantStatus {
		t.Fatalf("expected status %d, got %d (%s) for %s", wantStatus, rerr.Status, rerr.Msg, raw)
	}
}

func TestLossyRejectsMalformedSeed(t *testing.T) {
	// The parent's case: seed={invalid:'shape'} must 400, never reach a
	// generation with a loss record.
	raw := `{"model":"m","messages":[{"role":"user","content":"hi"}],"seed":{"invalid":"shape"}}`
	_, _, rerr := DecodeChatRequest([]byte(raw), DefaultLimits())
	if rerr == nil || rerr.Status != 400 {
		t.Fatalf("malformed seed must 400 in lossy-capable decode, got %v", rerr)
	}
	decodeExpect(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"seed":"7"}`, 400)
	decodeExpect(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"seed":7.5}`, 400)
	// Well-formed seed is a diagnosed loss, not an error.
	g, losses := mustDecode(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"seed":7}`)
	if len(losses) != 1 || losses[0].Field != "seed" {
		t.Fatalf("seed must be a single loss: %+v %v", g, losses)
	}
}

func TestBoolNullHandling(t *testing.T) {
	base := `{"model":"m","messages":[{"role":"user","content":"hi"}]`
	decodeExpect(t, base+`,"stream":null}`, 400)
	decodeExpect(t, base+`,"stream":"yes"}`, 400)
	decodeExpect(t, base+`,"stream_options":{"include_usage":null}}`, 400)
	decodeExpect(t, base+`,"stream_options":{"mystery":true}}`, 400)
	decodeExpect(t, base+`,"stream_options":[1]}`, 400)
	// Null on nullable bridged controls means absent.
	mustDecode(t, base+`,"temperature":null,"max_tokens":null,"stream_options":null,"extra_body":null,"tools":null,"tool_choice":null,"parallel_tool_calls":null}`)
	// include_obfuscation is a reviewed bool loss, not a silent drop.
	g, losses := mustDecode(t, base+`,"stream_options":{"include_usage":true,"include_obfuscation":false}}`)
	if !g.StreamIncludeUsage || len(losses) != 1 {
		t.Fatalf("stream_options mishandled: %+v %v", g, losses)
	}
}

func TestExtraBodyValidation(t *testing.T) {
	base := `{"model":"m","messages":[{"role":"user","content":"hi"}],"extra_body":`
	decodeExpect(t, base+`{"unknown_key":1}}`, 400)
	decodeExpect(t, base+`{"min_p":"high"}}`, 400)
	decodeExpect(t, base+`{"repetition_penalty":[1]}}`, 400)
	decodeExpect(t, base+`{"thinking":{"budget_tokens":"many"}}}`, 400)
	decodeExpect(t, base+`{"thinking":{"unknown":1}}}`, 400)
	decodeExpect(t, base+`{"top_k":-1}}`, 400)
	decodeExpect(t, base+`"[1]"}`, 400)
	// Well-formed members: top_k bridges, the rest are losses.
	g, losses := mustDecode(t, base+`{"top_k":5,"min_p":0.1,"thinking":{"type":"enabled","budget_tokens":100}}}`)
	if g.ExtraTopK == nil || *g.ExtraTopK != 5 {
		t.Fatalf("top_k not bridged: %+v", g.ExtraTopK)
	}
	if len(losses) != 2 {
		t.Fatalf("expected 2 losses, got %v", losses)
	}
}

func TestUnknownNestedMembersRejected(t *testing.T) {
	decodeExpect(t, `{"model":"m","messages":[{"role":"user","content":"hi","bogus":1}]}`, 400)
	decodeExpect(t, `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"x","annotations":[]}]}]}`, 400)
	decodeExpect(t, `{"model":"m","messages":[{"role":"user","content":[{"type":"text"}]}]}`, 400)
	decodeExpect(t, `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":["a"]}]}]}`, 400)
	decodeExpect(t, `{"model":"m","messages":[{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"t","arguments":"{}"},"extra":1}]}]}`, 400)
	decodeExpect(t, `{"model":"m","messages":[{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"t","arguments":"{}","zzz":1}}]}]}`, 400)
	decodeExpect(t, `{"model":"m","messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"t"}}],"tool_choice":{"type":"function","function":{"name":"t","extra":1}}}`, 400)
	decodeExpect(t, `{"model":"m","messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"t","parameters":{},"strict":"yes"}}]}`, 400)
	decodeExpect(t, `{"model":"m","messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"t"}}],"tool_choice":{"type":"function","function":{"name":""}}}}`, 400)
	decodeExpect(t, `{"model":"m","messages":[{"role":"user","content":"x"}],"stop":[5]}`, 400)
	decodeExpect(t, `{"model":"m","messages":[{"role":"user","content":"x"}],"response_format":{"type":"bogus"}}`, 400)
	decodeExpect(t, `{"model":"m","messages":[{"role":"user","content":"x"}],"function_call":"sometimes"}`, 400)
	decodeExpect(t, `{"model":"m","messages":[{"role":"user","content":"x"}],"web_search_options":{"search_context_size":"huge"}}`, 400)
	// refusal text is assistant text with an exact representation.
	g, losses := mustDecode(t, `{"model":"m","messages":[{"role":"assistant","content":[{"type":"refusal","refusal":"no"}]}]}`)
	if len(losses) != 0 || len(g.Messages[0].Texts) != 1 || g.Messages[0].Texts[0].Text != "no" {
		t.Fatalf("refusal must map to text: %+v %v", g.Messages, losses)
	}
	// file blocks have no reviewed schema: refused, never silently dropped.
	decodeExpect(t, `{"model":"m","messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"f"}}]}]}`, 400)
}

func TestToolArgumentsMustBeObjects(t *testing.T) {
	call := func(args string) string {
		return `{"model":"m","messages":[{"role":"assistant","content":"","tool_calls":[{"id":"call-1","type":"function","function":{"name":"get_time","arguments":` + args + `}}]}]}`
	}
	for _, scalar := range []string{`"5"`, `"\"x\""`, `"[1]"`, `"null"`, `"\"\"x"}`, `"{oops}"`} {
		_ = scalar
	}
	decodeExpect(t, call(`"5"`), 400)
	decodeExpect(t, call(`"\"x\""`), 400)
	decodeExpect(t, call(`"[1]"`), 400)
	decodeExpect(t, call(`"{oops}"`), 400)
	decodeExpect(t, call(`"null"`), 400)
	// Empty string is the ubiquitous no-arg convention: accepted as {}.
	g, _ := mustDecode(t, call(`""`))
	if string(g.Messages[0].ToolCalls[0].Arguments) != "{}" {
		t.Fatalf("empty args must mean {}: %s", g.Messages[0].ToolCalls[0].Arguments)
	}
}

func TestToolIdentityMatching(t *testing.T) {
	hist := func(body string) string {
		return `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"","tool_calls":[{"id":"call-1","type":"function","function":{"name":"get_time","arguments":"{}"}}]},` + body + `]}`
	}
	// Unmatched result identity.
	decodeExpect(t, hist(`{"role":"tool","tool_call_id":"call-9","content":"x"}`), 400)
	// Duplicate call IDs conflict.
	decodeExpect(t, `{"model":"m","messages":[{"role":"assistant","content":"","tool_calls":[{"id":"call-1","type":"function","function":{"name":"a","arguments":"{}"}},{"id":"call-1","type":"function","function":{"name":"b","arguments":"{}"}}]}]}`, 400)
	// Duplicate result IDs conflict.
	decodeExpect(t, hist(`{"role":"tool","tool_call_id":"call-1","content":"x"},{"role":"tool","tool_call_id":"call-1","content":"y"}`), 400)
	// Missing history ID cannot be carried over.
	decodeExpect(t, `{"model":"m","messages":[{"role":"assistant","content":"","tool_calls":[{"type":"function","function":{"name":"a","arguments":"{}"}}]}]}`, 400)
	// Conflicting (non-string) IDs.
	decodeExpect(t, `{"model":"m","messages":[{"role":"assistant","content":"","tool_calls":[{"id":5,"type":"function","function":{"name":"a","arguments":"{}"}}]}]}`, 400)
}

func TestOrdinaryToolsOnly(t *testing.T) {
	def := func(typ string) string {
		return `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"` + typ + `","function":{"name":"t","description":"d","parameters":{"type":"object"}}}]}`
	}
	decodeExpect(t, def("custom"), 400)
	decodeExpect(t, def("computer"), 400)
	decodeExpect(t, def("mystery"), 400)
	decodeExpect(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"a","description":"d","parameters":{"type":"object"}}},{"type":"function","function":{"name":"a","description":"e","parameters":{"type":"object"}}}]}`, 400)
	decodeExpect(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"has space","parameters":{"type":"object"}}}]}`, 400)
	// A forced choice must name an ordinary tool being sent.
	decodeExpect(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"a","parameters":{"type":"object"}}}],"tool_choice":{"type":"function","function":{"name":"ghost"}}}`, 400)
	decodeExpect(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"function","function":{"name":"ghost"}}}`, 400)
}

func TestNumberPreservationToUpstream(t *testing.T) {
	g, _ := mustDecode(t, `{"model":"m","messages":[{"role":"assistant","content":"","tool_calls":[{"id":"call-1","type":"function","function":{"name":"calc","arguments":"{\"n\": 9007199254740993, \"f\": 1.10}"}}]}]}`)
	out, rerr := EncodeAnthropicRequest(g, "up")
	if rerr != nil {
		t.Fatalf("encode: %v", rerr)
	}
	if !strings.Contains(string(out), "9007199254740993") {
		t.Fatalf("large integer not preserved: %s", out)
	}
	if !strings.Contains(string(out), "1.10") {
		t.Fatalf("number literal not preserved: %s", out)
	}
}

func TestLossesDeterministicAndBounded(t *testing.T) {
	var msgs []string
	for i := 0; i < 100; i++ {
		msgs = append(msgs, `{"role":"user","content":"hi","name":"n"}`)
	}
	raw := `{"model":"m","messages":[` + strings.Join(msgs, ",") + `]}`
	_, losses, rerr := DecodeChatRequest([]byte(raw), DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	if len(losses) != maxLossRecords {
		t.Fatalf("losses must be capped at %d, got %d", maxLossRecords, len(losses))
	}
	// Deterministic: sorted records plus a terminal truncation marker.
	body := losses[:len(losses)-1]
	for i := 1; i < len(body); i++ {
		if body[i].Field < body[i-1].Field {
			t.Fatalf("losses not sorted: %v", losses)
		}
	}
	if losses[len(losses)-1].Field != "losses" {
		t.Fatalf("last record must be the truncation marker: %+v", losses[len(losses)-1])
	}
	if _, rerr := EnforcePolicy(losses, true); rerr != nil {
		t.Fatalf("lossy must allow: %v", rerr)
	}
	hdr := EncodeLosses(losses)
	if strings.Contains(hdr, "hi") {
		t.Fatalf("loss header must not carry values")
	}
}

func TestNullMessagesRejected(t *testing.T) {
	decodeExpect(t, `{"model":"m","messages":null}`, 400)
	decodeExpect(t, `{"model":"m","messages":"hi"}`, 400)
}

// --- review regressions: response fidelity ---

func TestResponseRejectsCitations(t *testing.T) {
	// The parent's case: dropped citations returned 200; now 502.
	raw := `{"id":"m","type":"message","role":"assistant","model":"up",
		"content":[{"type":"text","text":"ok","citations":[{"type":"char","text":"x"}]}],
		"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	if _, rerr := TranslateAnthropicJSON([]byte(raw), "public", DefaultLimits()); rerr == nil || rerr.Status != 502 {
		t.Fatalf("citations must fail with 502, got %v", rerr)
	}
	// Empty citations are harmless: accepted.
	raw = `{"id":"m","type":"message","role":"assistant","model":"up",
		"content":[{"type":"text","text":"ok","citations":[]}],
		"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	if _, rerr := TranslateAnthropicJSON([]byte(raw), "public", DefaultLimits()); rerr != nil {
		t.Fatalf("empty citations must be accepted: %v", rerr)
	}
}

func TestResponseStrictness(t *testing.T) {
	base := func(content, usage, extra string) string {
		return `{"id":"m","type":"message","role":"assistant","model":"up",` + extra + `"content":[` + content + `],"stop_reason":"end_turn","usage":` + usage + `}`
	}
	okContent := `{"type":"text","text":"hi"}`
	okUsage := `{"input_tokens":1,"output_tokens":2}`
	must502 := func(raw string) {
		t.Helper()
		if _, rerr := TranslateAnthropicJSON([]byte(raw), "public", DefaultLimits()); rerr == nil || rerr.Status != 502 {
			t.Fatalf("expected 502 for %s, got %v", raw, rerr)
		}
	}
	must502(base(okContent, `{"input_tokens":1,"output_tokens":2,"cache_read_input_tokens":1.5}`, ""))
	must502(base(okContent, `{"input_tokens":1,"output_tokens":2,"cache_read_input_tokens":"5"}`, ""))
	must502(base(okContent, `{"input_tokens":1,"output_tokens":2,"cache_creation_input_tokens":-1}`, ""))
	must502(base(okContent, `{"input_tokens":1,"output_tokens":2,"server_tool_use":{"web_search_requests":1}}`, ""))
	must502(base(okContent, `{"input_tokens":1,"output_tokens":2,"cache_creation":{"ephemeral_5m_input_tokens":2}}`, ""))
	must502(base(okContent, `{"input_tokens":1,"output_tokens":2,"service_tier":"standard"}`, ""))
	must502(base(okContent, `{"input_tokens":1,"output_tokens":2,"mystery":null}`, ""))
	must502(base(okContent, `{"input_tokens":-1,"output_tokens":2}`, ""))
	must502(base(okContent, `{"input_tokens":1.5,"output_tokens":2}`, ""))
	must502(base(okContent, `{"input_tokens":"1","output_tokens":2}`, ""))
	must502(base(okContent, `{"input_tokens":1}`, ""))
	must502(base(`{"type":"text","text":"hi"}`, okUsage, `"container":{"id":"c"},`))
	must502(base(`{"type":"text"}`, okUsage, ""))
	must502(base(`{"type":"web_search_tool_result"}`, okUsage, ""))
	must502(base(`{"type":"tool_use","id":"t","name":"n","input":[1]}`, okUsage, ""))
	must502(base(`{"type":"tool_use","id":"t","name":"n","input":"x"}`, okUsage, ""))
	must502(base(`{"type":"tool_use","id":"t","name":"n","input":{},"zzz":1}`, okUsage, ""))
	must502(base(okContent, okUsage, `"role":"user",`))
	must502(strings.Replace(base(okContent, okUsage, ""), `"stop_reason":"end_turn"`, `"stop_reason":"pause_turn"`, 1))
	must502(strings.Replace(base(okContent, okUsage, ""), `"stop_reason":"end_turn"`, `"stop_reason":null`, 1))
	// stop_sequence is documented harmless metadata: accepted.
	mustDecodeResponse(t, strings.Replace(base(okContent, okUsage, ""), `"stop_reason":"end_turn"`, `"stop_reason":"stop_sequence","stop_sequence":"END"`, 1))
	// Complete tool_use requires an input object; null must not invent {}.
	must502(base(`{"type":"tool_use","id":"t","name":"n","input":null}`, okUsage, ""))
	mustDecodeResponse(t, base(`{"type":"tool_use","id":"t","name":"n","input":{}}`, okUsage, ""))
	// Large integers in tool arguments survive exactly.
	out := mustDecodeResponse(t, base(`{"type":"tool_use","id":"t","name":"n","input":{"n":9007199254740993}}`, okUsage, ""))
	if !strings.Contains(string(out), "9007199254740993") {
		t.Fatalf("response integers not preserved: %s", out)
	}
}

func mustDecodeResponse(t *testing.T, raw string) []byte {
	t.Helper()
	out, rerr := TranslateAnthropicJSON([]byte(raw), "public", DefaultLimits())
	if rerr != nil {
		t.Fatalf("response must translate: %v", rerr)
	}
	return out
}

// --- review regressions: streaming state machine ---

func runStream(t *testing.T, upstream string, lim Limits) (string, *RouterError) {
	t.Helper()
	var sb strings.Builder
	tr := &SSETranslator{PublicModel: "public", IncludeUsage: true, Limits: lim}
	rerr := tr.Translate(strings.NewReader(upstream), &sb, nil)
	return sb.String(), rerr
}

func streamStart(inputTokens string) string {
	return "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":" + inputTokens + "}}}\n\n"
}

func streamDelta(stop string) string {
	return "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"" + stop + "\"},\"usage\":{\"output_tokens\":3}}\n\n"
}

func streamStop() string {
	return "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
}

func TestStreamContentBeforeStart(t *testing.T) {
	upstream := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n"
	out, rerr := runStream(t, upstream, DefaultLimits())
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("content before start must fail: %v", rerr)
	}
	if strings.Contains(out, "Hi") {
		t.Fatalf("no content may leak before failure: %s", out)
	}
}

func TestStreamDuplicateStart(t *testing.T) {
	upstream := streamStart("8") + streamStart("8")
	if _, rerr := runStream(t, upstream, DefaultLimits()); rerr == nil {
		t.Fatalf("duplicate message_start must fail")
	}
}

func TestStreamDuplicateBlockIndex(t *testing.T) {
	upstream := streamStart("8") +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"
	if _, rerr := runStream(t, upstream, DefaultLimits()); rerr == nil {
		t.Fatalf("reused block index must fail")
	}
}

func TestStreamNegativeIndex(t *testing.T) {
	upstream := streamStart("8") +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":-1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"
	if _, rerr := runStream(t, upstream, DefaultLimits()); rerr == nil {
		t.Fatalf("negative index must fail")
	}
}

func TestStreamMessageStopOpenBlocks(t *testing.T) {
	upstream := streamStart("8") +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		streamDelta("end_turn") + streamStop()
	out, rerr := runStream(t, upstream, DefaultLimits())
	if rerr == nil {
		t.Fatalf("message_stop with open blocks must fail")
	}
	if strings.Contains(out, "[DONE]") {
		t.Fatalf("must not emit success marker: %s", out)
	}
}

func TestStreamEmptyArgToolEmitsIdentity(t *testing.T) {
	upstream := streamStart("8") +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"to_1\",\"name\":\"get_time\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		streamDelta("tool_use") + streamStop()
	out, rerr := runStream(t, upstream, DefaultLimits())
	if rerr != nil {
		t.Fatalf("empty-arg tool stream must succeed: %v", rerr)
	}
	if !strings.Contains(out, `"id":"to_1"`) || !strings.Contains(out, `"name":"get_time"`) {
		t.Fatalf("tool identity missing: %s", out)
	}
	if !strings.Contains(out, `"finish_reason":"tool_calls"`) || !strings.Contains(out, "[DONE]") {
		t.Fatalf("terminal framing wrong: %s", out)
	}
}

func TestStreamInitialBlockDataPreserved(t *testing.T) {
	upstream := streamStart("8") +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"Hello\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		streamDelta("end_turn") + streamStop()
	out, rerr := runStream(t, upstream, DefaultLimits())
	if rerr != nil {
		t.Fatalf("stream must succeed: %v", rerr)
	}
	if !strings.Contains(out, `"content":"Hello"`) {
		t.Fatalf("initial block text dropped: %s", out)
	}
}

func TestStreamInitialToolInputSeeded(t *testing.T) {
	upstream := streamStart("8") +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"to_9\",\"name\":\"calc\",\"input\":{\"a\":1}}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n" +
		streamDelta("tool_use") + streamStop()
	out, rerr := runStream(t, upstream, DefaultLimits())
	if rerr != nil {
		t.Fatalf("stream must succeed: %v", rerr)
	}
	if !strings.Contains(out, `"id":"to_9"`) || !strings.Contains(out, `{\"a\":1}`) {
		t.Fatalf("seeded tool input lost: %s", out)
	}
}

func TestStreamFrameBounds(t *testing.T) {
	lim := DefaultLimits()
	lim.MaxFrameBytes = 64
	// One overlong line.
	big := "event: content_block_delta\ndata: " + strings.Repeat("x", 100) + "\n\n"
	if _, rerr := runStream(t, streamStart("8")+big, lim); rerr == nil {
		t.Fatalf("overlong frame must fail")
	}
	// Many small lines in one frame exceeding the cumulative bound.
	var sb strings.Builder
	sb.WriteString("event: content_block_delta\n")
	for i := 0; i < 20; i++ {
		sb.WriteString("data: abcdef\n")
	}
	sb.WriteString("\n")
	if _, rerr := runStream(t, streamStart("8")+sb.String(), lim); rerr == nil {
		t.Fatalf("cumulative frame bytes must be bounded")
	}
}

func TestStreamUnknownDeltasRejected(t *testing.T) {
	text := func(delta string) string {
		return streamStart("8") +
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":" + delta + "}\n\n"
	}
	if _, rerr := runStream(t, text(`{"type":"citations_delta","citation":{}}`), DefaultLimits()); rerr == nil {
		t.Fatalf("citation deltas must fail")
	}
	if _, rerr := runStream(t, text(`{"type":"thinking_delta","thinking":"h"}`), DefaultLimits()); rerr == nil {
		t.Fatalf("reasoning deltas must fail")
	}
	if _, rerr := runStream(t, streamStart("8")+
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"h\"}}\n\n",
		DefaultLimits()); rerr == nil {
		t.Fatalf("reasoning blocks must fail")
	}
}

func TestStreamMissingUsageAndDelta(t *testing.T) {
	// Start without usage.
	noUsage := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\"}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		streamDelta("end_turn") + streamStop()
	if _, rerr := runStream(t, noUsage, DefaultLimits()); rerr == nil {
		t.Fatalf("missing start usage must fail")
	}
	// Delta without usage.
	noDeltaUsage := streamStart("8") +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n" +
		streamStop()
	if _, rerr := runStream(t, noDeltaUsage, DefaultLimits()); rerr == nil {
		t.Fatalf("missing delta usage must fail")
	}
	// Negative usage.
	negUsage := streamStart("8") +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":-1}}\n\n" +
		streamStop()
	if _, rerr := runStream(t, negUsage, DefaultLimits()); rerr == nil {
		t.Fatalf("negative usage must fail")
	}
}

func TestStreamReturnsAtValidStopWithoutReadingTrailingEvents(t *testing.T) {
	prefix := streamStart("8") +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		streamDelta("end_turn") + streamStop()
	trailing := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"late\"}}\n\n"
	reader := &cgmByteReader{data: []byte(prefix + trailing)}
	var out strings.Builder
	tr := SSETranslator{PublicModel: "public", Limits: DefaultLimits()}
	if err := tr.Translate(reader, &out, nil); err != nil {
		t.Fatalf("valid terminal must return promptly: %v", err)
	}
	if reader.served != len(prefix) {
		t.Fatalf("read past terminal: served %d, want %d", reader.served, len(prefix))
	}
	if strings.Contains(out.String(), "late") || strings.Count(out.String(), "data: [DONE]\n\n") != 1 {
		t.Fatalf("trailing event changed completed output: %s", out.String())
	}
}

// --- review regressions: standard Anthropic usage shape ---

// standardStart builds a realistic message_start with the full documented
// Message envelope: id/type/role/model, empty content, null stop fields and
// a usage object carrying both input and output counts.
func standardStart(usage string) string {
	return "event: message_start\ndata: {\"type\":\"message_start\",\"message\":" +
		"{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-haiku-4-5\"," +
		"\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":" + usage + "}}\n\n"
}

func textBlockFlow(text string) string {
	return "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"" + text + "\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"
}

func TestSSEStandardUsageStart(t *testing.T) {
	// The documented basic shape: start carries input and output counts and
	// no cache members at all.
	upstream := standardStart(`{"input_tokens":25,"output_tokens":1}`) +
		textBlockFlow("Hello!") +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":15}}\n\n" +
		streamStop()
	out, rerr := runStream(t, upstream, DefaultLimits())
	if rerr != nil {
		t.Fatalf("standard start must translate: %v", rerr)
	}
	for _, want := range []string{`"content":"Hello!"`, `"finish_reason":"stop"`,
		`"prompt_tokens":25`, `"completion_tokens":15`, `"total_tokens":40`} {
		if !strings.Contains(out, want) {
			t.Fatalf("stream lacks %s: %s", want, out)
		}
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Fatalf("missing terminator: %s", out)
	}
	if strings.Contains(out, "prompt_tokens_details") {
		t.Fatalf("no cached share, so no details are expected: %s", out)
	}
}

func TestSSEStandardUsageCachedZeros(t *testing.T) {
	// Plain traffic commonly carries the cache scalars as zeros even with
	// no caching; they must be accepted and change nothing.
	upstream := standardStart(`{"input_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0}`) +
		textBlockFlow("hey") +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":2}}\n\n" +
		streamStop()
	out, rerr := runStream(t, upstream, DefaultLimits())
	if rerr != nil {
		t.Fatalf("zero cache scalars must translate: %v", rerr)
	}
	for _, want := range []string{`"content":"hey"`, `"prompt_tokens":5`, `"completion_tokens":2`, "data: [DONE]"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stream lacks %s: %s", want, out)
		}
	}
}

func TestSSEStandardUsageCachedNonzero(t *testing.T) {
	// Nonzero cache counts are honestly accounted: the Chat prompt total is
	// the summation of all input components, with the cached share detailed.
	upstream := standardStart(`{"input_tokens":6,"cache_creation_input_tokens":2,"cache_read_input_tokens":4,"output_tokens":0}`) +
		textBlockFlow("hey") +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":5}}\n\n" +
		streamStop()
	out, rerr := runStream(t, upstream, DefaultLimits())
	if rerr != nil {
		t.Fatalf("cached start must translate: %v", rerr)
	}
	for _, want := range []string{`"prompt_tokens":12`, `"completion_tokens":5`, `"total_tokens":17`, `"cached_tokens":4`} {
		if !strings.Contains(out, want) {
			t.Fatalf("stream lacks honest accounting %s: %s", want, out)
		}
	}
}

func TestSSECumulativeOutputAuthoritative(t *testing.T) {
	// Delta counts are cumulative totals, not increments: repeated deltas
	// overwrite, and the final delta wins. Summing would report 1+5+15=21.
	upstream := standardStart(`{"input_tokens":25,"output_tokens":1}`) +
		textBlockFlow("Hi") +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":null,\"stop_sequence\":null},\"usage\":{\"output_tokens\":5}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":15}}\n\n" +
		streamStop()
	out, rerr := runStream(t, upstream, DefaultLimits())
	if rerr != nil {
		t.Fatalf("repeated deltas must translate: %v", rerr)
	}
	if !strings.Contains(out, `"completion_tokens":15`) {
		t.Fatalf("final delta must be authoritative: %s", out)
	}
	if !strings.Contains(out, `"prompt_tokens":25`) {
		t.Fatalf("prompt must not absorb output counts: %s", out)
	}
}

func TestSSEStartEnvelopeValidation(t *testing.T) {
	goodText := textBlockFlow("hey") +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":2}}\n\n" +
		streamStop()
	mustFail := func(name, start string) {
		t.Helper()
		out, rerr := runStream(t, start+goodText, DefaultLimits())
		if rerr == nil || rerr.Status != 502 {
			t.Fatalf("%s must fail with 502, got %v", name, rerr)
		}
		if strings.Contains(out, "[DONE]") || strings.Contains(out, "hey") {
			t.Fatalf("%s must not emit content or success: %s", name, out)
		}
	}
	msg := func(inner string) string {
		return "event: message_start\ndata: {\"type\":\"message_start\",\"message\":" + inner + "}\n\n"
	}
	usage := `"usage":{"input_tokens":5,"output_tokens":0}`
	mustFail("wrong role", msg(`{"id":"m","type":"message","role":"user","content":[],"usage":`+usage+`}`))
	mustFail("wrong type", msg(`{"id":"m","type":"text","role":"assistant","content":[],"usage":`+usage+`}`))
	mustFail("non-empty content", msg(`{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"x"}],"usage":`+usage+`}`))
	mustFail("start stop reason", msg(`{"id":"m","type":"message","role":"assistant","content":[],"stop_reason":"end_turn","usage":`+usage+`}`))
	mustFail("missing identity", msg(`{"type":"message","role":"assistant","content":[],"usage":`+usage+`}`))
	mustFail("missing usage", msg(`{"id":"m","type":"message","role":"assistant","content":[]}`))
	mustFail("malformed cache counter", msg(`{"id":"m","usage":{"input_tokens":5,"output_tokens":0,"cache_read_input_tokens":-1}}`))
	mustFail("fractional output counter", msg(`{"id":"m","usage":{"input_tokens":5,"output_tokens":1.5}}`))
	mustFail("unmapped billing member", msg(`{"id":"m","usage":{"input_tokens":5,"output_tokens":0,"server_tool_use":{"web_search_requests":1}}}`))
	// The legacy minimal envelope (identity plus input-only usage) stays
	// supported so existing scripted upstreams keep working.
	out, rerr := runStream(t, msg(`{"id":"m","usage":{"input_tokens":5}}`)+goodText, DefaultLimits())
	if rerr != nil {
		t.Fatalf("minimal start must stay supported: %v", rerr)
	}
	if !strings.Contains(out, `"prompt_tokens":5`) {
		t.Fatalf("minimal start usage wrong: %s", out)
	}
}

func TestStreamKeepaliveFrameReset(t *testing.T) {
	// Comment-only keepalive frames carry no event and must not accumulate
	// toward the frame bound across an otherwise idle stream. The bound
	// stays above the largest single documented frame so ordinary traffic
	// is unaffected.
	lim := DefaultLimits()
	lim.MaxFrameBytes = 512
	var sb strings.Builder
	sb.WriteString(streamStart("8"))
	for i := 0; i < 50; i++ {
		sb.WriteString(": keepalive\n\n")
	}
	sb.WriteString(textBlockFlow("hey"))
	sb.WriteString(streamDelta("end_turn"))
	sb.WriteString(streamStop())
	out, rerr := runStream(t, sb.String(), lim)
	if rerr != nil {
		t.Fatalf("keepalives must not trip the frame bound: %v", rerr)
	}
	if !strings.Contains(out, `"content":"hey"`) || !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("stream incomplete: %s", out)
	}
}

func TestStreamTotalBlockBound(t *testing.T) {
	// Total distinct block indices are bounded even when every block is a
	// cheap text block carrying no tool identity.
	lim := DefaultLimits()
	lim.MaxStreamBlocks = 3
	var sb strings.Builder
	sb.WriteString(streamStart("8"))
	for i := 0; i < 4; i++ {
		sb.WriteString("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":" +
			strconv.Itoa(i) + ",\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
	}
	out, rerr := runStream(t, sb.String(), lim)
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("block flood must fail with 502, got %v", rerr)
	}
	if strings.Contains(out, "[DONE]") {
		t.Fatalf("bounded stream must not succeed: %s", out)
	}
}

func TestResponseJSONStandardUsage(t *testing.T) {
	base := func(usage string) string {
		return `{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-4-5",` +
			`"content":[{"type":"text","text":"Hi"}],"stop_reason":"end_turn","stop_sequence":null,"usage":` + usage + `}`
	}
	check := func(raw, prompt, completion, total string, cached bool) {
		t.Helper()
		out, rerr := TranslateAnthropicJSON([]byte(raw), "public", DefaultLimits())
		if rerr != nil {
			t.Fatalf("standard usage must translate: %v", rerr)
		}
		s := string(out)
		for _, want := range []string{`"prompt_tokens":` + prompt, `"completion_tokens":` + completion, `"total_tokens":` + total} {
			if !strings.Contains(s, want) {
				t.Fatalf("usage lacks %s: %s", want, s)
			}
		}
		if cached != strings.Contains(s, `"cached_tokens"`) {
			t.Fatalf("cached details presence wrong (want %v): %s", cached, s)
		}
	}
	// Documented plain shape, with and without zero cache scalars.
	check(base(`{"input_tokens":25,"output_tokens":1}`), "25", "1", "26", false)
	check(base(`{"input_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0}`), "5", "0", "5", false)
	// Nonzero cache is honestly summed, never dropped.
	check(base(`{"input_tokens":6,"cache_creation_input_tokens":2,"cache_read_input_tokens":4,"output_tokens":5}`), "12", "5", "17", true)
	// Explicit nulls for unmapped billing members mean unused: tolerated.
	mustDecodeResponse(t, base(`{"input_tokens":1,"output_tokens":2,"service_tier":null,"server_tool_use":null}`))
	mustFail := func(raw string) {
		t.Helper()
		if _, rerr := TranslateAnthropicJSON([]byte(raw), "public", DefaultLimits()); rerr == nil || rerr.Status != 502 {
			t.Fatalf("expected 502 for %s, got %v", raw, rerr)
		}
	}
	mustFail(base(`{"input_tokens":1,"output_tokens":2,"cache_read_input_tokens":-1}`))
	mustFail(base(`{"input_tokens":1,"output_tokens":2,"cache_creation_input_tokens":1.5}`))
	mustFail(base(`{"input_tokens":1,"output_tokens":"2"}`))
	mustFail(base(`{"input_tokens":1}`))
	mustFail(base(`{"input_tokens":1,"output_tokens":2,"output_tokens_details":{"thinking_tokens":1}}`))
}
