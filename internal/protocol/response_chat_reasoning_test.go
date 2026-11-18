// Chat source private-reasoning compatibility tests: substantive
// reasoning deltas before visible output, nullable final field, and
// strict/foreign/malformed controls.
//
// The gateway may return provider-private `reasoning_content` on Chat
// assistant messages (complete) and deltas (SSE). It must never be
// echoed, projected to public text, or invented as continuation state.
// Absent/null/"" is exact in both modes; substantive is strict-502 and
// lossy-dropped; malformed is 502 in both modes. Terminal finish plus
// authoritative usage plus [DONE] remain required; truncation never
// becomes a synthetic success.
package protocol

import (
	"strings"
	"testing"
)

func chatReasonUsage() string { return `{"prompt_tokens":9,"completion_tokens":4,"total_tokens":13}` }

func chatReasonMsg(content, rc string) string {
	return `{"role":"assistant","content":` + content + `,"reasoning_content":` + rc + `}`
}

func chatReasonRaw(content, rc, finish string) string {
	msg := chatReasonMsg(content, rc)
	return `{"id":"chatcmpl-9","object":"chat.completion","created":1,"model":"up",` +
		`"choices":[{"index":0,"message":` + msg + `,"finish_reason":"` + finish + `"}],` +
		`"usage":` + chatReasonUsage() + `}`
}

func TestChatReasoningContentCompleteExact(t *testing.T) {
	for _, rc := range []string{`null`, `""`} {
		raw := chatReasonRaw(`"hi"`, rc, "stop")
		resp, rerr := DecodeChatResponse([]byte(raw), DefaultLimits())
		if rerr != nil {
			t.Fatalf("exact rc %s must decode: %v", rc, rerr)
		}
		if len(resp.Items) != 1 {
			t.Fatalf("rc %s items = %#v, want one text", rc, resp.Items)
		}
		if tx, ok := resp.Items[0].(TextItem); !ok || tx.Text != "hi" {
			t.Fatalf("rc %s text wrong: %#v", rc, resp.Items[0])
		}
		// Omitted entirely is identical.
		rawOmit := `{"id":"chatcmpl-9","object":"chat.completion","created":1,"model":"up",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
			`"usage":` + chatReasonUsage() + `}`
		resp2, rerr := DecodeChatResponse([]byte(rawOmit), DefaultLimits())
		if rerr != nil {
			t.Fatalf("omitted rc must decode: %v", rerr)
		}
		if len(resp2.Items) != 1 {
			t.Fatalf("omitted items = %#v", resp2.Items)
		}
	}
}

func TestChatReasoningContentCompleteStrictVsLossy(t *testing.T) {
	secret := "private-chain-of-thought-xyz"
	raw := chatReasonRaw(`"visible"`, `"`+secret+`"`, "stop")
	// Strict: 502 naming the field path only.
	_, rerr := DecodeChatResponse([]byte(raw), DefaultLimits())
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("strict substantive must 502, got %v", rerr)
	}
	if !strings.Contains(rerr.Msg, "response.choices[0].message.reasoning_content") || !strings.Contains(rerr.Msg, "has no representation") {
		t.Fatalf("strict error must name the field path: %q", rerr.Msg)
	}
	if strings.Contains(rerr.Msg, secret) {
		t.Fatalf("strict error echoes private content: %q", rerr.Msg)
	}
	// Lossy: drops, keeps visible text, usage, finish; never projects.
	resp, rerr := DecodeChatResponseWithPolicy([]byte(raw), DefaultLimits(), true)
	if rerr != nil {
		t.Fatalf("lossy substantive must decode: %v", rerr)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("lossy items = %#v, want one text", resp.Items)
	}
	if tx, ok := resp.Items[0].(TextItem); !ok || tx.Text != "visible" {
		t.Fatalf("lossy text wrong: %#v", resp.Items[0])
	}
	if rerr := resp.Usage.ValidateAuthoritative(); rerr != nil {
		t.Fatalf("lossy usage not authoritative: %v", rerr)
	}
	if resp.Finish.Kind != FinishStop {
		t.Fatalf("lossy finish wrong: %+v", resp.Finish)
	}
	// Encoded downstream must not carry the private text anywhere.
	for _, fn := range []func(Response, string) ([]byte, *RouterError){
		func(r Response, p string) ([]byte, *RouterError) { return EncodeAnthropicResponse(r, p) },
		func(r Response, p string) ([]byte, *RouterError) { return EncodeResponsesResponse(r, p) },
		func(r Response, p string) ([]byte, *RouterError) { return EncodeGeminiResponse(r, p) },
		func(r Response, p string) ([]byte, *RouterError) { return EncodeChatResponse(r, p) },
	} {
		out, rerr := fn(resp, "public")
		if rerr != nil {
			continue // some targets may refuse other provenance; only check leakage
		}
		if strings.Contains(string(out), secret) {
			t.Fatalf("encoded output leaks private reasoning: %s", out)
		}
	}
}

func TestChatReasoningContentCompleteMalformedForeign(t *testing.T) {
	// Malformed types are 502 invalid in both modes.
	for _, rc := range []string{`5`, `true`, `[]`, `{}`, `{"a":1}`} {
		raw := chatReasonRaw(`"hi"`, rc, "stop")
		if _, rerr := DecodeChatResponse([]byte(raw), DefaultLimits()); rerr == nil || rerr.Status != 502 {
			t.Fatalf("strict malformed %s must 502, got %v", rc, rerr)
		}
		if _, rerr := DecodeChatResponseWithPolicy([]byte(raw), DefaultLimits(), true); rerr == nil || rerr.Status != 502 {
			t.Fatalf("lossy malformed %s must 502, got %v", rc, rerr)
		}
	}
	// Foreign private fields stay closed in both modes.
	rawForeign := `{"id":"chatcmpl-9","object":"chat.completion","created":1,"model":"up",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"hi","reasoning":{"a":1}},"finish_reason":"stop"}],` +
		`"usage":` + chatReasonUsage() + `}`
	if _, rerr := DecodeChatResponse([]byte(rawForeign), DefaultLimits()); rerr == nil || rerr.Status != 502 {
		t.Fatalf("foreign must 502 strict, got %v", rerr)
	}
	if _, rerr := DecodeChatResponseWithPolicy([]byte(rawForeign), DefaultLimits(), true); rerr == nil || rerr.Status != 502 {
		t.Fatalf("foreign must 502 lossy, got %v", rerr)
	}
}

// --- Streaming ---

func csReasonChunk(delta string, finish string) string {
	ch := `{"id":"chatcmpl-7","object":"chat.completion.chunk","created":1700000000,"model":"gpt-4o","choices":[{"index":0,"delta":` + delta
	if finish == "" {
		ch += `,"finish_reason":null`
	} else {
		ch += `,"finish_reason":"` + finish + `"`
	}
	return ch + `}]}`
}

func TestChatReasoningContentStreamSubstantiveBeforeVisible(t *testing.T) {
	secret := "private-stream-thought-abc"
	usage := csUsage(9, 4)
	mk := func(policy bool) *ChatStreamDecoder {
		return NewChatStreamDecoderWithPolicy(DefaultLimits(), policy)
	}
	// Substantive reasoning deltas arrive before any visible output.
	datas := []string{
		csReasonChunk(`{"reasoning_content":"`+secret+`"}`, ""),
		csReasonChunk(`{"reasoning_content":" more"}`, ""),
		csReasonChunk(`{"content":"hi"}`, ""),
		csReasonChunk(`{}`, "stop"),
		csUsageChunk(usage),
		"[DONE]",
	}
	// Strict: first reasoning delta is 502 with the value-free path.
	dec := mk(false)
	if _, rerr := dec.DecodeFrame(Frame{Data: datas[0]}); rerr == nil || rerr.Status != 502 {
		t.Fatalf("strict reasoning delta must 502, got %v", rerr)
	} else {
		if !strings.Contains(rerr.Msg, "chunk.choices[0].delta.reasoning_content") {
			t.Fatalf("strict stream error must name path: %q", rerr.Msg)
		}
		if strings.Contains(rerr.Msg, secret) {
			t.Fatalf("strict stream error echoes private content: %q", rerr.Msg)
		}
	}
	// Lossy: drops reasoning, emits only visible text, terminates.
	dec = mk(true)
	var all []ResponseEvent
	for _, d := range datas {
		evs, rerr := dec.DecodeFrame(Frame{Data: d})
		if rerr != nil {
			t.Fatalf("lossy DecodeFrame(%q): %v", d, rerr)
		}
		all = append(all, evs...)
	}
	if tail, rerr := dec.Close(); rerr != nil || len(tail) != 0 {
		t.Fatalf("Close = %v %v, want no events", tail, rerr)
	}
	var texts []string
	for _, ev := range all {
		if f, ok := ev.(TextFragment); ok {
			texts = append(texts, f.Text)
		}
	}
	if len(texts) != 1 || texts[0] != "hi" {
		t.Fatalf("lossy text fragments = %q, want [hi]", texts)
	}
	var sawUsage, sawComplete bool
	for _, ev := range all {
		if u, ok := ev.(UsageEvent); ok && u.Terminal() {
			sawUsage = true
		}
		if c, ok := ev.(CompletionEvent); ok && c.Finish.Kind == FinishStop {
			sawComplete = true
		}
	}
	if !sawUsage || !sawComplete {
		t.Fatalf("lossy terminal missing usage=%v complete=%v: %#v", sawUsage, sawComplete, all)
	}
	// No fabricated text: joined output never contains the secret.
	var joined string
	for _, s := range texts {
		joined += s
	}
	if strings.Contains(joined, secret) || strings.Contains(joined, "more") {
		t.Fatalf("stream projected private reasoning to text: %q", joined)
	}
}

func TestChatReasoningContentStreamNullableFinal(t *testing.T) {
	usage := csUsage(9, 4)
	// Nullable final delta field is exact in both modes.
	for _, policy := range []bool{false, true} {
		dec := NewChatStreamDecoderWithPolicy(DefaultLimits(), policy)
		datas := []string{
			csReasonChunk(`{"content":"hi"}`, ""),
			csReasonChunk(`{"content":""}`, ""),
			csReasonChunk(`{"reasoning_content":null}`, "stop"),
			csUsageChunk(usage),
			"[DONE]",
		}
		var all []ResponseEvent
		for _, d := range datas {
			evs, rerr := dec.DecodeFrame(Frame{Data: d})
			if rerr != nil {
				t.Fatalf("policy=%v DecodeFrame(%q): %v", policy, d, rerr)
			}
			all = append(all, evs...)
		}
		if _, rerr := dec.Close(); rerr != nil {
			t.Fatalf("policy=%v Close: %v", policy, rerr)
		}
		if _, ok := all[len(all)-1].(CompletionEvent); !ok {
			t.Fatalf("policy=%v terminal = %T, want CompletionEvent", policy, all[len(all)-1])
		}
	}
	// Empty-string reasoning alongside the finish marker is also exact.
	dec := NewChatStreamDecoder(DefaultLimits())
	datas := []string{
		csReasonChunk(`{"content":"hi"}`, ""),
		csReasonChunk(`{"reasoning_content":""}`, "stop"),
		csUsageChunk(usage),
		"[DONE]",
	}
	for _, d := range datas {
		if _, rerr := dec.DecodeFrame(Frame{Data: d}); rerr != nil {
			t.Fatalf("empty final rc: %v", rerr)
		}
	}
	if _, rerr := dec.Close(); rerr != nil {
		t.Fatalf("empty final rc Close: %v", rerr)
	}
}

func TestChatReasoningContentStreamStrictForeignMalformed(t *testing.T) {
	usage := csUsage(5, 2)
	term := []string{csReasonChunk(`{}`, "stop"), csUsageChunk(usage), "[DONE]"}
	// Malformed reasoning types fail in both modes at the first frame.
	for _, frag := range []string{`{"reasoning_content":5}`, `{"reasoning_content":true}`, `{"reasoning_content":[]}`, `{"reasoning_content":{}}`} {
		for _, policy := range []bool{false, true} {
			dec := NewChatStreamDecoderWithPolicy(DefaultLimits(), policy)
			if _, rerr := dec.DecodeFrame(Frame{Data: csReasonChunk(frag, "")}); rerr == nil || rerr.Status != 502 {
				t.Fatalf("policy=%v malformed %s must 502, got %v", policy, frag, rerr)
			}
		}
	}
	// Foreign private field stays unrepresentable in both modes.
	for _, policy := range []bool{false, true} {
		dec := NewChatStreamDecoderWithPolicy(DefaultLimits(), policy)
		first := csReasonChunk(`{"reasoning_extra":"x"}`, "")
		if _, rerr := dec.DecodeFrame(Frame{Data: first}); rerr == nil || rerr.Status != 502 {
			t.Fatalf("policy=%v foreign must 502, got %v", policy, rerr)
		} else if !strings.Contains(rerr.Msg, "has no representation") {
			t.Fatalf("foreign must name path: %v", rerr)
		}
		_ = term
	}
	// No fabricated success: reasoning drop does not excuse truncation.
	dec := NewChatStreamDecoderWithPolicy(DefaultLimits(), true)
	truncated := []string{
		csReasonChunk(`{"reasoning_content":"think"}`, ""),
		csReasonChunk(`{"content":"hi"}`, ""),
		csReasonChunk(`{}`, "stop"),
		"[DONE]",
	}
	failed := false
	for _, d := range truncated {
		if _, rerr := dec.DecodeFrame(Frame{Data: d}); rerr != nil {
			failed = true
			break
		}
	}
	if !failed {
		if _, rerr := dec.Close(); rerr == nil {
			t.Fatal("truncated stream (missing usage) must fail even with reasoning drop")
		}
	}
}

func TestChatReasoningContentTranslatorsDropNeverProject(t *testing.T) {
	secret := "translator-private-999"
	usage := csUsage(9, 4)
	upstream := "data: " + csReasonChunk(`{"reasoning_content":"`+secret+`"}`, "") + "\n\n" +
		"data: " + csReasonChunk(`{"content":"hello"}`, "") + "\n\n" +
		"data: " + csReasonChunk(`{}`, "stop") + "\n\n" +
		"data: " + csUsageChunk(usage) + "\n\n" +
		"data: [DONE]\n\n"
	// Strict translators refuse pre-commit (no 200 body).
	{
		var out strings.Builder
		tr := &ChatToAnthropicSSETranslator{PublicModel: "p", Limits: DefaultLimits()}
		if rerr := tr.Translate(strings.NewReader(upstream), &out, nil); rerr == nil || rerr.Status != 502 {
			t.Fatalf("strict anthropic translator must 502, got %v", rerr)
		}
	}
	{
		var out strings.Builder
		tr := &ChatToResponsesSSETranslator{PublicModel: "p", Limits: DefaultLimits()}
		if rerr := tr.Translate(strings.NewReader(upstream), &out, nil); rerr == nil || rerr.Status != 502 {
			t.Fatalf("strict responses translator must 502, got %v", rerr)
		}
	}
	// Lossy translators succeed with visible text only and a terminal.
	trA := &ChatToAnthropicSSETranslator{PublicModel: "p", Limits: DefaultLimits(), AllowPrivateReasoningDrop: true}
	var outA strings.Builder
	if rerr := trA.Translate(strings.NewReader(upstream), &outA, nil); rerr != nil {
		t.Fatalf("lossy anthropic: %v", rerr)
	}
	if strings.Contains(outA.String(), secret) {
		t.Fatalf("anthropic output leaks reasoning: %s", outA.String())
	}
	if !strings.Contains(outA.String(), "message_stop") {
		t.Fatalf("anthropic output missing message_stop: %s", outA.String())
	}
	trR := &ChatToResponsesSSETranslator{PublicModel: "p", Limits: DefaultLimits(), AllowPrivateReasoningDrop: true}
	var outR strings.Builder
	if rerr := trR.Translate(strings.NewReader(upstream), &outR, nil); rerr != nil {
		t.Fatalf("lossy responses: %v", rerr)
	}
	if strings.Contains(outR.String(), secret) {
		t.Fatalf("responses output leaks reasoning: %s", outR.String())
	}
	if !strings.Contains(outR.String(), "response.completed") {
		t.Fatalf("responses output missing completed: %s", outR.String())
	}
}

// --- Native kimi-class complete reasoning/reasoning_details ---
//
// Measured live kimi Chat complete message:
// {"role":"assistant","content":string,"refusal":null,
//  "reasoning":string,
//  "reasoning_details":[{"type":"reasoning.text","text":string,
//  "format":string,"index":int}]}
// with detail text equal to reasoning. Same policy as reasoning_content:
// absent/null/empty is exact, substantive is strict-502 and lossy-dropped,
// malformed or unknown variants refuse in both modes. Dropped bytes never
// enter the IR, the logs, or continuation state.

func chatKimiReasonRaw(content, reasoning, details string) string {
	return `{"id":"chatcmpl-9","object":"chat.completion","created":1,"model":"up",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":` + content +
		`,"refusal":null,"reasoning":` + reasoning + `,"reasoning_details":` + details + `},"finish_reason":"stop"}],` +
		`"usage":` + chatReasonUsage() + `}`
}

func TestChatKimiReasoningCompleteExact(t *testing.T) {
	// Null/empty reasoning with null/empty details is exact in both modes.
	for _, tc := range [][2]string{
		{`null`, `null`},
		{`""`, `null`},
		{`null`, `[]`},
		{`""`, `[]`},
	} {
		raw := chatKimiReasonRaw(`"hi"`, tc[0], tc[1])
		for _, policy := range []bool{false, true} {
			resp, rerr := DecodeChatResponseWithPolicy([]byte(raw), DefaultLimits(), policy)
			if rerr != nil {
				t.Fatalf("reasoning=%s details=%s policy=%v must decode: %v", tc[0], tc[1], policy, rerr)
			}
			if len(resp.Items) != 1 {
				t.Fatalf("reasoning=%s details=%s items = %#v", tc[0], tc[1], resp.Items)
			}
			if tx, ok := resp.Items[0].(TextItem); !ok || tx.Text != "hi" {
				t.Fatalf("reasoning=%s text wrong: %#v", tc[0], resp.Items[0])
			}
		}
	}
}

func TestChatKimiReasoningCompleteStrictVsLossy(t *testing.T) {
	secret := "kimi-private-thought-xyz"
	detail := `[{"type":"reasoning.text","text":"` + secret + `","format":"general","index":0}]`
	// Live shape: substantive reasoning alongside matching details.
	raw := chatKimiReasonRaw(`"visible"`, `"`+secret+`"`, detail)
	_, rerr := DecodeChatResponse([]byte(raw), DefaultLimits())
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("strict live shape must 502, got %v", rerr)
	}
	if !strings.Contains(rerr.Msg, "response.choices[0].message.reasoning") || !strings.Contains(rerr.Msg, "has no representation") {
		t.Fatalf("strict error must name the field path: %q", rerr.Msg)
	}
	if strings.Contains(rerr.Msg, secret) {
		t.Fatalf("strict error echoes private content: %q", rerr.Msg)
	}
	// Details-only substantive names the details path instead.
	rawDetails := chatKimiReasonRaw(`"visible"`, `""`, detail)
	_, rerr = DecodeChatResponse([]byte(rawDetails), DefaultLimits())
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("strict details-only must 502, got %v", rerr)
	}
	if !strings.Contains(rerr.Msg, "response.choices[0].message.reasoning_details") {
		t.Fatalf("strict details error must name the details path: %q", rerr.Msg)
	}
	if strings.Contains(rerr.Msg, secret) {
		t.Fatalf("strict details error echoes private content: %q", rerr.Msg)
	}
	// Lossy: drops both, keeps visible text, usage, finish; never projects.
	for _, rawLossy := range []string{raw, rawDetails} {
		resp, rerr := DecodeChatResponseWithPolicy([]byte(rawLossy), DefaultLimits(), true)
		if rerr != nil {
			t.Fatalf("lossy live shape must decode: %v", rerr)
		}
		if len(resp.Items) != 1 {
			t.Fatalf("lossy items = %#v, want one text", resp.Items)
		}
		if tx, ok := resp.Items[0].(TextItem); !ok || tx.Text != "visible" {
			t.Fatalf("lossy text wrong: %#v", resp.Items[0])
		}
		if rerr := resp.Usage.ValidateAuthoritative(); rerr != nil {
			t.Fatalf("lossy usage not authoritative: %v", rerr)
		}
		if resp.Finish.Kind != FinishStop {
			t.Fatalf("lossy finish wrong: %+v", resp.Finish)
		}
		for _, fn := range []func(Response, string) ([]byte, *RouterError){
			func(r Response, p string) ([]byte, *RouterError) { return EncodeAnthropicResponse(r, p) },
			func(r Response, p string) ([]byte, *RouterError) { return EncodeResponsesResponse(r, p) },
			func(r Response, p string) ([]byte, *RouterError) { return EncodeGeminiResponse(r, p) },
			func(r Response, p string) ([]byte, *RouterError) { return EncodeChatResponse(r, p) },
		} {
			out, rerr := fn(resp, "public")
			if rerr != nil {
				continue // some targets may refuse other provenance; only check leakage
			}
			if strings.Contains(string(out), secret) {
				t.Fatalf("encoded output leaks private reasoning: %s", out)
			}
		}
	}
}

func TestChatKimiReasoningCompleteMalformedForeign(t *testing.T) {
	// Malformed types are 502 invalid in both modes.
	for _, tc := range [][2]string{
		{`5`, `null`},
		{`true`, `null`},
		{`[]`, `null`},
		{`{"a":1}`, `null`},
		{`null`, `5`},
		{`null`, `true`},
		{`null`, `{}`},
		{`null`, `"x"`},
		{`null`, `[5]`},
		{`null`, `[{}]`},
		{`null`, `[{"text":"x","format":"general","index":0}]`},               // missing type
		{`null`, `[{"type":"reasoning.text","format":"general","index":0}]`},  // missing text
		{`null`, `[{"type":"reasoning.text","text":"x","index":0}]`},          // missing format
		{`null`, `[{"type":"reasoning.text","text":"x","format":"general"}]`}, // missing index
		{`null`, `[{"type":"reasoning.text","text":5,"format":"general","index":0}]`},
		{`null`, `[{"type":"reasoning.text","text":"x","format":"general","index":"0"}]`},
		{`null`, `[{"type":"reasoning.text","text":"x","format":"general","index":-1}]`},
		{`null`, `[{"type":"reasoning.text","text":"x","format":"general","index":0},{"type":"reasoning.text","text":"y","format":"general","index":0}]`}, // duplicate index
	} {
		raw := chatKimiReasonRaw(`"hi"`, tc[0], tc[1])
		if _, rerr := DecodeChatResponse([]byte(raw), DefaultLimits()); rerr == nil || rerr.Status != 502 {
			t.Fatalf("strict reasoning=%s details=%s must 502, got %v", tc[0], tc[1], rerr)
		}
		if _, rerr := DecodeChatResponseWithPolicy([]byte(raw), DefaultLimits(), true); rerr == nil || rerr.Status != 502 {
			t.Fatalf("lossy reasoning=%s details=%s must 502, got %v", tc[0], tc[1], rerr)
		}
	}
	// Unknown variants stay closed in both modes, even lossy.
	for _, details := range []string{
		`[{"type":"reasoning.other","text":"x","format":"general","index":0}]`,
		`[{"type":"reasoning.text","text":"x","format":"general","index":0,"extra":1}]`,
	} {
		raw := chatKimiReasonRaw(`"hi"`, `null`, details)
		if _, rerr := DecodeChatResponse([]byte(raw), DefaultLimits()); rerr == nil || rerr.Status != 502 {
			t.Fatalf("strict unknown %s must 502, got %v", details, rerr)
		} else if !strings.Contains(rerr.Msg, "has no representation") {
			t.Fatalf("unknown must name path: %q", rerr.Msg)
		}
		if _, rerr := DecodeChatResponseWithPolicy([]byte(raw), DefaultLimits(), true); rerr == nil || rerr.Status != 502 {
			t.Fatalf("lossy unknown %s must 502, got %v", details, rerr)
		}
	}
}

func TestChatKimiReasoningCompleteBounds(t *testing.T) {
	// Oversized private strings refuse in both modes (size bound, value-free).
	big := strings.Repeat("r", DefaultLimits().MaxArgsChars+1)
	raw := chatKimiReasonRaw(`"hi"`, `"`+big+`"`, `null`)
	for _, policy := range []bool{false, true} {
		if _, rerr := DecodeChatResponseWithPolicy([]byte(raw), DefaultLimits(), policy); rerr == nil || rerr.Status != 502 {
			t.Fatalf("policy=%v oversized reasoning must 502, got %v", policy, rerr)
		}
	}
	bigDetail := `[{"type":"reasoning.text","text":"` + big + `","format":"general","index":0}]`
	raw = chatKimiReasonRaw(`"hi"`, `null`, bigDetail)
	for _, policy := range []bool{false, true} {
		if _, rerr := DecodeChatResponseWithPolicy([]byte(raw), DefaultLimits(), policy); rerr == nil || rerr.Status != 502 {
			t.Fatalf("policy=%v oversized detail text must 502, got %v", policy, rerr)
		}
	}
	// Entry-count bound is enforced through Limits in both modes.
	small := DefaultLimits()
	small.MaxToolCalls = 1
	raw = chatKimiReasonRaw(`"hi"`, `null`,
		`[{"type":"reasoning.text","text":"a","format":"general","index":0},{"type":"reasoning.text","text":"b","format":"general","index":1}]`)
	for _, policy := range []bool{false, true} {
		if _, rerr := DecodeChatResponseWithPolicy([]byte(raw), small, policy); rerr == nil || rerr.Status != 502 {
			t.Fatalf("policy=%v over-count details must 502, got %v", policy, rerr)
		}
	}
}
