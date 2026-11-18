package protocol

import (
	"strings"
	"testing"
)

// Minimal fully-valid Responses envelopes for echo tests. Output is one
// text message; usage totals balance exactly.
func rechoChatEnvelope(reasoningFrag string) string {
	base := `{"id":"resp_1","object":"response","created_at":1746989954.0,"model":"up",` +
		`"status":"completed","error":null,"incomplete_details":null,` +
		`"output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant",` +
		`"content":[{"type":"output_text","text":"hi","annotations":[],"logprobs":[]}]}],` +
		`"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}`
	if reasoningFrag == "" {
		return base + `}`
	}
	return base + `,"reasoning":` + reasoningFrag + `}`
}

func rechoAnthropicEnvelope(reasoningFrag string) string {
	// Same shape is valid for the Anthropic translator (zero reasoning
	// accounting, no cache buckets).
	return rechoChatEnvelope(reasoningFrag)
}

func rechoMustSucceedChat(t *testing.T, reasoningFrag string) {
	t.Helper()
	if _, rerr := TranslateResponsesJSONToChat([]byte(rechoChatEnvelope(reasoningFrag)), "public", DefaultLimits()); rerr != nil {
		t.Fatalf("chat JSON reasoning %s must succeed, got %v", reasoningFrag, rerr)
	}
}

func rechoMustFailChat(t *testing.T, reasoningFrag string) *RouterError {
	t.Helper()
	_, rerr := TranslateResponsesJSONToChat([]byte(rechoChatEnvelope(reasoningFrag)), "public", DefaultLimits())
	if rerr == nil || rerr.Status != 502 || rerr.Code != CodeUpstreamBad {
		t.Fatalf("chat JSON reasoning %s must fail with 502 upstream invalid, got %v", reasoningFrag, rerr)
	}
	return rerr
}

func rechoMustSucceedAnthropic(t *testing.T, reasoningFrag string) {
	t.Helper()
	if _, rerr := TranslateResponsesJSONToAnthropic([]byte(rechoAnthropicEnvelope(reasoningFrag)), "public", DefaultLimits()); rerr != nil {
		t.Fatalf("anthropic JSON reasoning %s must succeed, got %v", reasoningFrag, rerr)
	}
}

func rechoMustFailAnthropic(t *testing.T, reasoningFrag string) *RouterError {
	t.Helper()
	_, rerr := TranslateResponsesJSONToAnthropic([]byte(rechoAnthropicEnvelope(reasoningFrag)), "public", DefaultLimits())
	if rerr == nil || rerr.Status != 502 || rerr.Code != CodeUpstreamBad {
		t.Fatalf("anthropic JSON reasoning %s must fail with 502 upstream invalid, got %v", reasoningFrag, rerr)
	}
	return rerr
}

func TestResponsesReasoningEchoHelperDirect(t *testing.T) {
	valid := []string{
		`null`,
		`{}`,
		`{"effort":null,"summary":null,"generate_summary":null}`,
		`{"effort":"medium"}`,
		`{"effort":"high"}`,
		`{"summary":"auto"}`,
		`{"summary":"concise"}`,
		`{"summary":"detailed"}`,
		`{"generate_summary":"auto"}`,
		`{"generate_summary":"concise"}`,
		`{"generate_summary":"detailed"}`,
		`{"summary":"auto","generate_summary":"detailed","effort":"medium"}`,
		`{"effort":"medium","future_null":null}`,
		`{"effort":"max","context":"all_turns","mode":"pro"}`,
		`{"context":"auto","mode":"provider-mode"}`,
	}
	for _, raw := range valid {
		if rerr := validateResponsesReasoningEcho([]byte(raw)); rerr != nil {
			t.Fatalf("helper must accept %s, got %v", raw, rerr)
		}
	}
	invalid := []string{
		`"auto"`, `[]`, `5`, `true`,
		`{"summary":[]}`, `{"summary":{}}`, `{"summary":5}`, `{"summary":true}`,
		`{"summary":"verbose"}`, `{"summary":"AUTO"}`, `{"summary":""}`,
		`{"generate_summary":[]}`, `{"generate_summary":{}}`, `{"generate_summary":"verbose"}`,
		`{"effort":5}`, `{"effort":[]}`, `{"effort":{}}`, `{"effort":true}`,
		`{"summary":"auto","bogus":1}`,
		`{"summary":"auto","bogus":"x"}`,
		`{"context":"unknown"}`, `{"context":17}`, `{"mode":[]}`, `{"effort":"turbo"}`,
	}
	for _, raw := range invalid {
		rerr := validateResponsesReasoningEcho([]byte(raw))
		if rerr == nil || rerr.Status != 502 || rerr.Code != CodeUpstreamBad {
			t.Fatalf("helper must reject %s with 502 upstream invalid, got %v", raw, rerr)
		}
		if !strings.Contains(rerr.Msg, "response.reasoning") {
			t.Fatalf("helper error must name field path for %s, got %q", raw, rerr.Msg)
		}
	}
}

func TestResponsesReasoningEchoJSON(t *testing.T) {
	valid := []string{
		"", // omitted entirely
		`null`,
		`{}`,
		`{"effort":null,"summary":null}`,
		`{"effort":"medium"}`,
		`{"summary":"auto"}`,
		`{"summary":"concise"}`,
		`{"summary":"detailed"}`,
		`{"generate_summary":"auto"}`,
		`{"generate_summary":"concise"}`,
		`{"generate_summary":"detailed"}`,
		`{"effort":"medium","summary":"auto","generate_summary":"concise"}`,
		`{"effort":"medium","future_null":null}`,
		`{"effort":"max","context":"all_turns","mode":"pro"}`,
	}
	for _, frag := range valid {
		rechoMustSucceedChat(t, frag)
		rechoMustSucceedAnthropic(t, frag)
		if _, rerr := TranslateResponsesJSONToGemini([]byte(rechoChatEnvelope(frag)), "public", DefaultLimits()); rerr != nil {
			t.Fatalf("Gemini JSON reasoning %s must succeed: %v", frag, rerr)
		}
	}
	invalid := []string{
		`{"summary":[]}`,
		`{"summary":{}}`,
		`{"summary":5}`,
		`{"summary":true}`,
		`{"summary":"verbose"}`,
		`{"summary":"AUTO"}`,
		`{"summary":""}`,
		`{"generate_summary":[]}`,
		`{"generate_summary":"verbose"}`,
		`{"effort":5}`,
		`{"effort":[]}`,
		`{"effort":{}}`,
		`{"summary":"auto","bogus":1}`,
		`{"context":"unknown"}`, `{"context":17}`, `{"mode":[]}`, `{"effort":"turbo"}`,
		`"auto"`,
		`[]`,
	}
	for _, frag := range invalid {
		rerr := rechoMustFailChat(t, frag)
		if strings.Contains(rerr.Msg, "verbose") {
			t.Fatalf("chat error must be value-free, got %q", rerr.Msg)
		}
		rerr = rechoMustFailAnthropic(t, frag)
		if strings.Contains(rerr.Msg, "verbose") {
			t.Fatalf("anthropic error must be value-free, got %q", rerr.Msg)
		}
		if _, rerr := TranslateResponsesJSONToGemini([]byte(rechoChatEnvelope(frag)), "public", DefaultLimits()); rerr == nil || rerr.Status != 502 {
			t.Fatalf("Gemini JSON reasoning %s must fail with 502: %v", frag, rerr)
		}
	}
}

// Mutation guard: the recurring bug parsed top-level summary as an ARRAY
// (the OUTPUT reasoning item shape). Before the fix, "auto" was red
// (rejected) and [] was wrongly accepted. Both must now behave per the
// Responses create contract (enum string accepted, array rejected).
func TestResponsesReasoningEchoMutationGuard(t *testing.T) {
	rechoMustSucceedChat(t, `{"summary":"auto"}`)
	rechoMustSucceedAnthropic(t, `{"summary":"auto"}`)
	rechoMustFailChat(t, `{"summary":[]}`)
	rechoMustFailAnthropic(t, `{"summary":[]}`)
}

func TestResponsesReasoningEchoGeminiSSE(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		valid bool
	}{
		{`{"summary":"auto"}`, true},
		{`{"summary":"detailed","context":"all_turns","mode":"pro","effort":"max"}`, true},
		{`{"summary":[]}`, false},
		{`{"context":"unknown"}`, false},
		{`{"effort":"turbo"}`, false},
	} {
		stream := r2gCreated(0) + r2gMsgAdded(1, 0, "msg_1") + r2gPartAdded(2, 0, "msg_1") +
			r2gDelta(3, 0, "msg_1", "hi") + r2gTextDone(4, 0, "msg_1", "hi") +
			r2gPartDone(5, 0, "msg_1", "hi") + r2gItemDoneMsg(6, 0, "msg_1", "hi") +
			r2gTerminal(7, "completed", `"completed"`, `null`,
				`[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hi"}]}]`)
		stream = strings.Replace(stream, `"model":"gpt-x"`, `"model":"gpt-x","reasoning":`+tc.raw, 1)
		var out strings.Builder
		tr := ResponsesToGeminiSSETranslator{PublicModel: "public", Limits: DefaultLimits()}
		rerr := tr.Translate(strings.NewReader(stream), &out, nil)
		if tc.valid {
			if rerr != nil || !strings.Contains(out.String(), `"finishReason":"STOP"`) {
				t.Fatalf("valid Gemini SSE echo %s rejected: %v", tc.raw, rerr)
			}
		} else if rerr == nil || rerr.Status != 502 || strings.Contains(out.String(), `"finishReason"`) {
			t.Fatalf("invalid Gemini SSE echo %s succeeded: %v", tc.raw, rerr)
		}
	}
}

func TestResponsesReasoningEchoOutputItemsStillRejected(t *testing.T) {
	reasoningItem := `{"type":"reasoning","id":"r1","summary":[]}`
	chatRaw := `{"id":"resp_1","object":"response","created_at":1746989954.0,"model":"up",` +
		`"status":"completed","error":null,"incomplete_details":null,` +
		`"output":[` + reasoningItem + `],` +
		`"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7},"reasoning":{"summary":"auto"}}`
	if _, rerr := TranslateResponsesJSONToChat([]byte(chatRaw), "public", DefaultLimits()); rerr == nil || rerr.Status != 502 {
		t.Fatalf("chat must still reject output reasoning items, got %v", rerr)
	}
	if _, rerr := TranslateResponsesJSONToAnthropic([]byte(chatRaw), "public", DefaultLimits()); rerr == nil || rerr.Status != 502 {
		t.Fatalf("anthropic must still reject output reasoning items, got %v", rerr)
	}
}

// SSE: terminal response.completed reuses the full-envelope validator, so the
// same echo contract must hold on the streaming path.
func rechoChatSSEWithReasoning(t *testing.T, reasoningFrag string) (string, *RouterError) {
	t.Helper()
	terminal := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-4.1\",\"status\":\"completed\"," +
		"\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"hi\",\"annotations\":[]}]}]," +
		"\"usage\":{\"input_tokens\":7,\"output_tokens\":3,\"total_tokens\":10}"
	if reasoningFrag != "" {
		terminal += ",\"reasoning\":" + reasoningFrag
	}
	terminal += "}}\n\n"
	upstream := r2cCreated() + r2cItemAddedMsg() + r2cPartAdded() + r2cTextDelta("hi") +
		"event: response.output_text.done\ndata: {\"type\":\"response.output_text.done\",\"output_index\":0,\"content_index\":0,\"item_id\":\"msg_1\",\"text\":\"hi\"}\n\n" +
		r2cPartDone("hi") + r2cItemDoneMsg("hi") +
		terminal
	return runR2CStream(t, upstream, true, DefaultLimits())
}

func TestResponsesReasoningEchoChatSSE(t *testing.T) {
	for _, frag := range []string{`{"summary":"auto"}`, `{"summary":"concise"}`, `{"summary":"detailed"}`, `{"summary":null}`, `{}`} {
		out, rerr := rechoChatSSEWithReasoning(t, frag)
		if rerr != nil {
			t.Fatalf("chat SSE reasoning %s must succeed, got %v", frag, rerr)
		}
		if !strings.HasSuffix(out, "data: [DONE]\n\n") {
			t.Fatalf("chat SSE missing terminator for %s: %s", frag, out)
		}
	}
	for _, frag := range []string{`{"summary":[]}`, `{"summary":"verbose"}`, `{"summary":5}`, `{"effort":[]}`} {
		out, rerr := rechoChatSSEWithReasoning(t, frag)
		if rerr == nil || rerr.Status != 502 {
			t.Fatalf("chat SSE reasoning %s must fail with 502, got %v (%s)", frag, rerr, out)
		}
		if strings.Contains(out, "[DONE]") {
			t.Fatalf("chat SSE failure must not emit DONE for %s", frag)
		}
	}
}

func rechoAnthropicSSEWithReasoning(t *testing.T, reasoningFrag string) (string, *RouterError) {
	t.Helper()
	textOut := r2aTextOutput("hi")
	usage := r2aUsage()
	term := r2aTerminal(7, false, textOut, usage)
	if reasoningFrag != "" {
		term = strings.Replace(term, `"output":`, `"reasoning":`+reasoningFrag+`,"output":`, 1)
	}
	upstream := r2aCreated(0) + r2aTextItemAdded(1, 0) + r2aPartAdded(2, 0) +
		r2aTextDelta(3, 0, "hi") + r2aTextDone(4, 0, "hi") + r2aPartDone(5, 0, "hi") +
		r2aTextItemDone(6, 0, "hi") + term
	var sb strings.Builder
	tr := &ResponsesToAnthropicSSETranslator{PublicModel: "public", Limits: DefaultLimits()}
	rerr := tr.Translate(strings.NewReader(upstream), &sb, nil)
	return sb.String(), rerr
}

func TestResponsesReasoningEchoAnthropicSSE(t *testing.T) {
	for _, frag := range []string{`{"summary":"auto"}`, `{"summary":"concise"}`, `{"summary":"detailed"}`, `{"summary":null}`, `{}`} {
		out, rerr := rechoAnthropicSSEWithReasoning(t, frag)
		if rerr != nil {
			t.Fatalf("anthropic SSE reasoning %s must succeed, got %v", frag, rerr)
		}
		if !strings.Contains(out, "message_stop") {
			t.Fatalf("anthropic SSE missing terminal for %s: %s", frag, out)
		}
	}
	for _, frag := range []string{`{"summary":[]}`, `{"summary":"verbose"}`, `{"summary":5}`, `{"effort":[]}`} {
		out, rerr := rechoAnthropicSSEWithReasoning(t, frag)
		if rerr == nil || rerr.Status != 502 {
			t.Fatalf("anthropic SSE reasoning %s must fail with 502, got %v (%s)", frag, rerr, out)
		}
	}
}
