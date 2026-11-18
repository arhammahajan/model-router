package protocol

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// Independent wire validators (do not mirror the encoder): they assert the
// upstream Chat contract directly from JSON bytes.
func chatToolChoiceWireKind(t *testing.T, body []byte) (isString bool, strVal string, isObject bool) {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	raw, ok := m["tool_choice"]
	if !ok {
		t.Fatalf("missing tool_choice: %s", body)
	}
	s := strings.TrimSpace(string(raw))
	if strings.HasPrefix(s, `"`) {
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatalf("bad string tool_choice: %s", raw)
		}
		return true, v, false
	}
	if strings.HasPrefix(s, `{`) {
		return false, "", true
	}
	t.Fatalf("tool_choice must be string or object, got %s", raw)
	return false, "", false
}

func TestReverseToolChoiceWireUnion(t *testing.T) {
	tools := `"tools":[{"name":"a","input_schema":{"type":"object"}}]`
	cases := []struct {
		in       string
		wantStr  string
		wantObj  bool
		wantName string
	}{
		{`{"type":"auto"}`, "auto", false, ""},
		{`{"type":"any"}`, "required", false, ""},
		{`{"type":"none"}`, "none", false, ""},
		{`{"type":"tool","name":"a"}`, "", true, "a"},
	}
	for _, c := range cases {
		g, losses := mustDecodeAnthropic(t, `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],`+tools+`,"tool_choice":`+c.in+`}`)
		if len(losses) != 0 {
			t.Fatalf("%s unexpected losses %v", c.in, losses)
		}
		out, rerr := EncodeChatRequest(g, AbsentEffort(), "up")
		if rerr != nil {
			t.Fatalf("%s encode: %v", c.in, rerr)
		}
		isStr, s, isObj := chatToolChoiceWireKind(t, out)
		if c.wantObj {
			if !isObj {
				t.Fatalf("%s must be object: %s", c.in, out)
			}
			var obj map[string]json.RawMessage
			var body map[string]json.RawMessage
			_ = json.Unmarshal(out, &body)
			_ = json.Unmarshal(body["tool_choice"], &obj)
			var typ string
			_ = json.Unmarshal(obj["type"], &typ)
			if typ != "function" {
				t.Fatalf("%s named type wrong: %s", c.in, out)
			}
			var fn map[string]json.RawMessage
			_ = json.Unmarshal(obj["function"], &fn)
			var name string
			_ = json.Unmarshal(fn["name"], &name)
			if name != c.wantName {
				t.Fatalf("%s named name wrong: %s", c.in, out)
			}
			continue
		}
		if !isStr || s != c.wantStr {
			t.Fatalf("%s must be string %q, got %s", c.in, c.wantStr, out)
		}
		// Mutation guard: bare modes must NOT be {"type":"none"} objects.
		if strings.Contains(string(out), `"type":"none"`) || strings.Contains(string(out), `"type":"auto"`) || strings.Contains(string(out), `"type":"required"`) {
			t.Fatalf("%s emitted object form for bare mode: %s", c.in, out)
		}
	}
}

func TestReverseUsageSubtractionJSON(t *testing.T) {
	raw := `{"id":"c","object":"chat.completion","created":1,"model":"up",
		"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,
			"prompt_tokens_details":{"cached_tokens":4}}}`
	out := mustTranslateChatJSON(t, raw)
	// independent parse: unmarshal usage map directly
	var env map[string]json.RawMessage
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatal(err)
	}
	var usage map[string]json.RawMessage
	if err := json.Unmarshal(env["usage"], &usage); err != nil {
		t.Fatal(err)
	}
	var in, co, cr int
	_ = json.Unmarshal(usage["input_tokens"], &in)
	_ = json.Unmarshal(usage["output_tokens"], &co)
	_ = json.Unmarshal(usage["cache_read_input_tokens"], &cr)
	if in != 6 || co != 5 || cr != 4 {
		t.Fatalf("prompt10 cached4 must give input6 out5 read4, got %d %d %d: %s", in, co, cr, out)
	}
}

func TestReverseUsageFailuresJSON(t *testing.T) {
	msg := `{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}`
	mk := func(usage string) string {
		return `{"id":"c","object":"chat.completion","created":1,"model":"up","choices":[` + msg + `],"usage":` + usage + `}`
	}
	bad := []string{
		// cache > prompt
		`{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4,"prompt_tokens_details":{"cached_tokens":4}}`,
		// mismatched total
		`{"prompt_tokens":10,"completion_tokens":5,"total_tokens":14}`,
		`{"prompt_tokens":10,"completion_tokens":5,"total_tokens":16}`,
		// negative
		`{"prompt_tokens":-1,"completion_tokens":1,"total_tokens":0}`,
		// fraction
		`{"prompt_tokens":1.5,"completion_tokens":1,"total_tokens":2}`,
		// quoted
		`{"prompt_tokens":"10","completion_tokens":1,"total_tokens":11}`,
		// overflow int64
		`{"prompt_tokens":99999999999999999999,"completion_tokens":1,"total_tokens":2}`,
		// cached negative / fraction
		`{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":-1}}`,
		`{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":1.5}}`,
		// missing total
		`{"prompt_tokens":10,"completion_tokens":5}`,
	}
	for i, u := range bad {
		if _, rerr := TranslateChatJSONToAnthropic([]byte(mk(u)), "p", DefaultLimits()); rerr == nil || rerr.Status != 502 {
			t.Fatalf("case %d must 502 for %s, got %v", i, u, rerr)
		}
	}
	// zero-cache and null-cache are exact and accepted
	ok := []string{
		`{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}`,
		`{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":null}`,
		`{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":0}}`,
	}
	for i, u := range ok {
		out, rerr := TranslateChatJSONToAnthropic([]byte(mk(u)), "p", DefaultLimits())
		if rerr != nil {
			t.Fatalf("ok case %d must succeed: %v", i, rerr)
		}
		if strings.Contains(string(out), "cache_read_input_tokens") {
			t.Fatalf("ok case %d must omit cache_read when zero: %s", i, out)
		}
	}
}

func TestReverseUsageSSESubtractionAndPlacement(t *testing.T) {
	usage := `{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":4}}`
	upstream := chatChunk(`{"content":"hi"}`, "", "") +
		chatChunk(`{}`, "stop", "") + chatUsageChunk(usage) + "data: [DONE]\n\n"
	out, rerr := runChatStream(t, upstream, DefaultLimits())
	if rerr != nil {
		t.Fatalf("stream: %v", rerr)
	}
	if !strings.Contains(out, `"input_tokens":6`) || !strings.Contains(out, `"cache_read_input_tokens":4`) {
		t.Fatalf("SSE must carry input6 read4: %s", out)
	}
	// usage before finish must fail
	up2 := chatChunk(`{"content":"hi"}`, "", "") + chatUsageChunk(usage) +
		chatChunk(`{}`, "stop", "") + chatUsageChunk(usage) + "data: [DONE]\n\n"
	if _, rerr := runChatStream(t, up2, DefaultLimits()); rerr == nil {
		t.Fatalf("usage before finish must fail")
	}
	// duplicate usage must fail
	up3 := chatChunk(`{"content":"hi"}`, "", "") + chatChunk(`{}`, "stop", "") +
		chatUsageChunk(usage) + chatUsageChunk(usage) + "data: [DONE]\n\n"
	out3, rerr := runChatStream(t, up3, DefaultLimits())
	if rerr == nil {
		t.Fatalf("duplicate usage must fail")
	}
	if strings.Contains(out3, "message_stop") {
		t.Fatalf("duplicate usage must not succeed:\n%s", out3)
	}
	// duplicate finish must fail
	up4 := chatChunk(`{"content":"hi"}`, "", "") + chatChunk(`{}`, "stop", "") +
		chatChunk(`{}`, "stop", "") + chatUsageChunk(usage) + "data: [DONE]\n\n"
	if _, rerr := runChatStream(t, up4, DefaultLimits()); rerr == nil {
		t.Fatalf("duplicate finish must fail")
	}
}

func TestReverseTopKLossAndValidation(t *testing.T) {
	// well-formed top_k is a diagnosed loss, not forwarded
	_, losses, rerr := DecodeAnthropicRequest([]byte(`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"top_k":3}`), DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	if len(losses) != 1 || losses[0].Field != "top_k" {
		t.Fatalf("top_k must be single loss, got %v", losses)
	}
	if _, rerr := EnforcePolicy(losses, false); rerr == nil || rerr.Code != CodeTranslationLoss {
		t.Fatalf("strict must refuse top_k, got %v", rerr)
	}
	kept, rerr := EnforcePolicy(losses, true)
	if rerr != nil {
		t.Fatalf("lossy must allow: %v", rerr)
	}
	if len(kept) != 1 {
		t.Fatalf("lossy must report: %v", kept)
	}
	g, _, _ := DecodeAnthropicRequest([]byte(`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"top_k":3}`), DefaultLimits())
	out, rerr := EncodeChatRequest(g, AbsentEffort(), "up")
	if rerr != nil {
		t.Fatalf("encode: %v", rerr)
	}
	if strings.Contains(string(out), "extra_body") || strings.Contains(string(out), "top_k") {
		t.Fatalf("top_k must not forward: %s", out)
	}
	// malformed top_k is 400 even in lossy-capable decode
	for _, raw := range []string{
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"top_k":-1}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"top_k":1.5}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"top_k":"3"}`,
	} {
		decodeAnthropicExpect(t, raw, 400)
	}
	// null is absent
	g2, losses2 := mustDecodeAnthropic(t, `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"top_k":null}`)
	_ = g2
	if len(losses2) != 0 {
		t.Fatalf("null top_k must be absent: %v", losses2)
	}
	// temperature contract 0..1
	for _, good := range []string{"0", "0.5", "1"} {
		mustDecodeAnthropic(t, `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"temperature":`+good+`}`)
	}
	for _, badT := range []string{"-0.1", "1.5", "2", `"0.5"`, "nullx"} {
		raw := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"temperature":` + badT + `}`
		if badT == "nullx" {
			raw = `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"temperature":null}`
			// null is absent -> success; skip failure path
			mustDecodeAnthropic(t, raw)
			continue
		}
		decodeAnthropicExpect(t, raw, 400)
	}
}

func TestReverseThinkingDisabledIntent(t *testing.T) {
	g, losses, rerr := DecodeAnthropicRequest([]byte(`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`), DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	if len(losses) != 0 || !g.Reasoning.IsNone() || g.Reasoning.Origin() != "thinking" {
		t.Fatalf("disabled thinking must retain its intent without a decode loss: %v %+v", losses, g.Reasoning)
	}
	if _, rerr := EnforcePolicy(losses, false); rerr != nil {
		t.Fatalf("disabled intent alone must not refuse before planning: %v", rerr)
	}
	// disabled with extras is malformed, including wrong types
	for _, raw := range []string{
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled","budget_tokens":1024}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled","display":"summarized"}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled","budget_tokens":"lots"}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled","display":7}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled","mystery":1}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"adaptive"}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"between_tools"}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"mystery"}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"budget_tokens":1024}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":7}}`,
	} {
		decodeAnthropicExpect(t, raw, 400)
	}
	// Null extras mean absent and keep the same disabled intent.
	g2, losses2, rerr := DecodeAnthropicRequest([]byte(`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled","budget_tokens":null,"display":null}}`), DefaultLimits())
	if rerr != nil {
		t.Fatalf("null extras must be absent: %v", rerr)
	}
	if len(losses2) != 0 || !g2.Reasoning.IsNone() {
		t.Fatalf("null extras must retain disabled intent without a loss: %v %+v", losses2, g2.Reasoning)
	}
}

func TestReverseStreamingIdentityAndTrailing(t *testing.T) {
	usage := `{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}`
	term := func() string { return chatChunk(`{}`, "tool_calls", "") + chatUsageChunk(usage) + "data: [DONE]\n\n" }
	// fragmented name (different fragments) is explicitly rejected
	frag := chatChunk(`{"tool_calls":[{"index":0,"id":"c1","function":{"name":"get","arguments":""}}]}`, "", "") +
		chatChunk(`{"tool_calls":[{"index":0,"function":{"name":"_time","arguments":"{}"}}]}`, "", "") + term()
	if _, rerr := runChatStream(t, frag, DefaultLimits()); rerr == nil {
		t.Fatalf("fragmented name must fail")
	}
	// invalid name before emitting identity
	badName := chatChunk(`{"tool_calls":[{"index":0,"id":"c1","function":{"name":"has space","arguments":"{}"}}]}`, "", "") + term()
	out, rerr := runChatStream(t, badName, DefaultLimits())
	if rerr == nil {
		t.Fatalf("invalid name must fail")
	}
	if strings.Contains(out, "message_stop") {
		t.Fatalf("invalid name must not succeed:\n%s", out)
	}
	// Call IDs are opaque provider handles (bounded by MaxArgsChars, may
	// contain spaces/colons): a 65-char ID passes source decode and the
	// Anthropic target (the old stream translator wrongly applied the
	// 1..64 tool-name grammar to IDs). Tool names keep the grammar.
	long := strings.Repeat("a", 65)
	tooLong := chatChunk(`{"tool_calls":[{"index":0,"id":"`+long+`","function":{"name":"a","arguments":"{}"}}]}`, "", "") + term()
	outLong, rerr := runChatStream(t, tooLong, DefaultLimits())
	if rerr != nil {
		t.Fatalf("opaque 65-char call ID must succeed, got %v", rerr)
	}
	if !strings.Contains(outLong, `"`+long+`"`) || !strings.Contains(outLong, "message_stop") {
		t.Fatalf("opaque ID must be preserved:\n%s", outLong)
	}
	tooLongN := chatChunk(`{"tool_calls":[{"index":0,"id":"c1","function":{"name":"`+long+`","arguments":"{}"}}]}`, "", "") + term()
	if _, rerr := runChatStream(t, tooLongN, DefaultLimits()); rerr == nil {
		t.Fatalf("overlong name must fail")
	}
	// Bytes after the protocol terminal belong to the next generation and
	// are left unread: the translator returns promptly after [DONE] without
	// requiring EOF, so trailing frames and a second DONE marker are ignored
	// rather than invented as a second error after message_stop.
	trailing := chatChunk(`{"content":"hi"}`, "", "") + chatChunk(`{}`, "stop", "") +
		chatUsageChunk(usage) + "data: [DONE]\n\n" + chatChunk(`{"content":"late"}`, "", "")
	outT, rerr := runChatStream(t, trailing, DefaultLimits())
	if rerr != nil {
		t.Fatalf("trailing data after DONE must be left unread, got %v", rerr)
	}
	if strings.Count(outT, "event: message_stop") != 1 {
		t.Fatalf("terminal must emit exactly one message_stop:\n%s", outT)
	}
	// duplicate DONE is likewise beyond the terminal and left unread
	dupDone := chatChunk(`{"content":"hi"}`, "", "") + chatChunk(`{}`, "stop", "") +
		chatUsageChunk(usage) + "data: [DONE]\n\n" + "data: [DONE]\n\n"
	outD, rerr := runChatStream(t, dupDone, DefaultLimits())
	if rerr != nil {
		t.Fatalf("duplicate DONE beyond terminal must be left unread, got %v", rerr)
	}
	if strings.Count(outD, "event: message_stop") != 1 {
		t.Fatalf("terminal must emit exactly one message_stop:\n%s", outD)
	}
	// incomplete fragment (id without name) at finish is rejected
	incomplete := chatChunk(`{"tool_calls":[{"index":0,"id":"c1","function":{"arguments":"{}"}}]}`, "", "") + term()
	if _, rerr := runChatStream(t, incomplete, DefaultLimits()); rerr == nil {
		t.Fatalf("incomplete identity must fail")
	}
}

// strictTerminalReader supplies a complete valid terminal sequence and then
// fails any further Read: it proves Translate returns promptly after [DONE]
// without draining to EOF or probing for bytes beyond the terminal.
type strictTerminalReader struct {
	data       []byte
	off        int
	extraReads int
}

func (r *strictTerminalReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		r.extraReads++
		return 0, errors.New("read past terminal marker")
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	return n, nil
}

func TestReverseTerminalPromptReturn(t *testing.T) {
	usage := `{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}`
	upstream := chatChunk(`{"content":"hi"}`, "", "") +
		chatChunk(`{}`, "stop", "") + chatUsageChunk(usage) + "data: [DONE]\n\n"
	r := &strictTerminalReader{data: []byte(upstream)}
	var sb strings.Builder
	tr := &ChatToAnthropicSSETranslator{PublicModel: "public", Limits: DefaultLimits()}
	if rerr := tr.Translate(r, &sb, nil); rerr != nil {
		t.Fatalf("terminal must return nil promptly, got %v (extraReads=%d)", rerr, r.extraReads)
	}
	out := sb.String()
	if got := strings.Count(out, "event: message_stop"); got != 1 {
		t.Fatalf("must emit exactly one message_stop, got %d:\n%s", got, out)
	}
	if r.extraReads != 0 {
		t.Fatalf("must not read past [DONE], extraReads=%d", r.extraReads)
	}
}

func TestReversePreterminalMalformedNoSuccess(t *testing.T) {
	usage := `{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}`
	term := chatChunk(`{}`, "stop", "") + chatUsageChunk(usage) + "data: [DONE]\n\n"
	// Malformed JSON before any terminal marker must fail without success.
	bad := "data: not-json\n\n" + term
	out, rerr := runChatStream(t, bad, DefaultLimits())
	if rerr == nil {
		t.Fatalf("preterminal malformed chunk must fail")
	}
	if strings.Contains(out, "message_stop") {
		t.Fatalf("preterminal failure must not emit message_stop:\n%s", out)
	}
}
