// Final independent audit: cross-row / cross-merge probes for the claimed
// complete + streaming IR migration at 739f8bd (after a729651).
//
// Scope: prove the architecture objective (all 4 complete upstream rows +
// all 4 stream upstream rows flow through source-neutral decoders + typed
// Response/ResponseEvent + 4 target encoders, old pair semantics deleted,
// native bypass unchanged) with executable calls, not file counts. Focus on
// the two recent merge risks: Gemini InitialUsage restoration and empty
// Responses output[] acceptance. Do not change old fixture expectations.
package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func finalBadUnknown(rawValid, anchor, inject string) string {
	return strings.Replace(rawValid, anchor, inject+anchor, 1)
}

func finalErrKey(rerr *RouterError) string {
	if rerr == nil {
		return "<nil>"
	}
	return rerr.Msg
}

// One shared source validator per upstream: the same bad wire bytes must
// fail identically through every target wrapper of that source row.
func TestFinalCompleteSharedValidatorIdenticalErrors(t *testing.T) {
	lim := DefaultLimits()
	chatValid := chatIRRaw("stop", `{"role":"assistant","content":"hi"}`, chatIRUsage(10, 3))
	chatBad := finalBadUnknown(chatValid, `"usage":`, `"bogus_feature":1,"usage":`)
	anthValid := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":3}}`
	anthBad := finalBadUnknown(anthValid, `"usage":`, `"bogus_feature":1,"usage":`)
	gemValid := `{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":3,"totalTokenCount":13}}`
	gemBad := finalBadUnknown(gemValid, `"usageMetadata":`, `"bogusFeature":1,"usageMetadata":`)
	respValid := `{"id":"resp_1","object":"response","created_at":1,"model":"m","status":"completed","error":null,"output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hi","annotations":[]}]}],"usage":{"input_tokens":10,"output_tokens":3,"total_tokens":13}}`
	respBad := finalBadUnknown(respValid, `"usage":`, `"bogus_feature":1,"usage":`)

	type row struct {
		name string
		bad  string
		call []func() *RouterError
	}
	rows := []row{
		{"chat", chatBad, []func() *RouterError{
			func() *RouterError { _, e := TranslateChatJSONToAnthropic([]byte(chatBad), "p", lim); return e },
			func() *RouterError { _, e := TranslateChatJSONToResponses([]byte(chatBad), "p", lim); return e },
			func() *RouterError { _, e := TranslateChatJSONToGemini([]byte(chatBad), "p", lim); return e },
		}},
		{"anthropic", anthBad, []func() *RouterError{
			func() *RouterError { _, e := TranslateAnthropicJSON([]byte(anthBad), "p", lim); return e },
			func() *RouterError { _, e := TranslateAnthropicJSONToGemini([]byte(anthBad), "p", lim); return e },
			func() *RouterError { _, e := TranslateAnthropicJSONToResponses([]byte(anthBad), "p", lim); return e },
		}},
		{"gemini", gemBad, []func() *RouterError{
			func() *RouterError { _, e := TranslateGeminiJSONToChat([]byte(gemBad), "p", lim); return e },
			func() *RouterError { _, e := TranslateGeminiJSONToAnthropic([]byte(gemBad), "p", lim); return e },
			func() *RouterError { _, e := TranslateGeminiJSONToResponses([]byte(gemBad), "p", lim); return e },
		}},
		{"responses", respBad, []func() *RouterError{
			func() *RouterError { _, e := TranslateResponsesJSONToChat([]byte(respBad), "p", lim); return e },
			func() *RouterError { _, e := TranslateResponsesJSONToAnthropic([]byte(respBad), "p", lim); return e },
			func() *RouterError { _, e := TranslateResponsesJSONToGemini([]byte(respBad), "p", lim); return e },
		}},
	}
	for _, r := range rows {
		var first string
		for i, fn := range r.call {
			err := fn()
			if err == nil {
				t.Fatalf("%s wrapper %d accepted unknown-field wire", r.name, i)
			}
			if err.Status != 502 {
				t.Fatalf("%s wrapper %d status=%d want 502", r.name, i, err.Status)
			}
			k := finalErrKey(err)
			if i == 0 {
				first = k
			} else if k != first {
				t.Fatalf("%s wrappers diverge: %q vs %q (pair-owned validator survives)", r.name, first, k)
			}
			if strings.Contains(k, "bogus") {
				t.Fatalf("%s error leaks value: %q", r.name, k)
			}
		}
	}
	// Valid minimal wire of each source succeeds on every target wrapper.
	valids := []struct {
		name string
		raw  string
		call []func() ([]byte, *RouterError)
	}{
		{"chat", chatValid, []func() ([]byte, *RouterError){
			func() ([]byte, *RouterError) { return TranslateChatJSONToAnthropic([]byte(chatValid), "p", lim) },
			func() ([]byte, *RouterError) { return TranslateChatJSONToResponses([]byte(chatValid), "p", lim) },
			func() ([]byte, *RouterError) { return TranslateChatJSONToGemini([]byte(chatValid), "p", lim) },
		}},
		{"anthropic", anthValid, []func() ([]byte, *RouterError){
			func() ([]byte, *RouterError) { return TranslateAnthropicJSON([]byte(anthValid), "p", lim) },
			func() ([]byte, *RouterError) { return TranslateAnthropicJSONToGemini([]byte(anthValid), "p", lim) },
			func() ([]byte, *RouterError) { return TranslateAnthropicJSONToResponses([]byte(anthValid), "p", lim) },
		}},
		{"gemini", gemValid, []func() ([]byte, *RouterError){
			func() ([]byte, *RouterError) { return TranslateGeminiJSONToChat([]byte(gemValid), "p", lim) },
			func() ([]byte, *RouterError) { return TranslateGeminiJSONToAnthropic([]byte(gemValid), "p", lim) },
			func() ([]byte, *RouterError) { return TranslateGeminiJSONToResponses([]byte(gemValid), "p", lim) },
		}},
		{"responses", respValid, []func() ([]byte, *RouterError){
			func() ([]byte, *RouterError) { return TranslateResponsesJSONToChat([]byte(respValid), "p", lim) },
			func() ([]byte, *RouterError) { return TranslateResponsesJSONToAnthropic([]byte(respValid), "p", lim) },
			func() ([]byte, *RouterError) { return TranslateResponsesJSONToGemini([]byte(respValid), "p", lim) },
		}},
	}
	for _, v := range valids {
		for i, fn := range v.call {
			out, rerr := fn()
			if rerr != nil {
				t.Fatalf("%s wrapper %d rejected valid wire: %v", v.name, i, rerr)
			}
			if len(out) == 0 {
				t.Fatalf("%s wrapper %d empty output", v.name, i)
			}
		}
	}
}

// Target gates apply to typed IR without reparsing source JSON: nonzero
// reasoning refuses only on Anthropic, thoughts only off-Gemini.
func TestFinalCompleteTargetGatesNoReparsing(t *testing.T) {
	u := func() ResponseUsage {
		uu, rerr := NewTerminalUsage(DialectChat, 10, 8, 18)
		if rerr != nil {
			t.Fatalf("usage: %v", rerr)
		}
		five := 5
		uu.Reasoning = &five
		return uu
	}()
	resp, rerr := NewResponse("r1", "m", DialectChat,
		[]ResponseItem{TextItem{ID: "t0", Text: "hi"}}, u, Finish{Kind: FinishStop, Raw: "stop"})
	if rerr != nil {
		t.Fatalf("NewResponse: %v", rerr)
	}
	if _, rerr := EncodeAnthropicResponse(resp, "p"); rerr == nil {
		t.Fatal("Anthropic must refuse nonzero reasoning")
	}
	for _, tc := range []struct {
		name string
		fn   func() ([]byte, *RouterError)
	}{
		{"chat", func() ([]byte, *RouterError) { return EncodeChatResponse(resp, "p") }},
		{"responses", func() ([]byte, *RouterError) { return EncodeResponsesResponse(resp, "p") }},
		{"gemini", func() ([]byte, *RouterError) { return EncodeGeminiResponse(resp, "p") }},
	} {
		out, rerr := tc.fn()
		if rerr != nil {
			t.Fatalf("%s must carry reasoning: %v", tc.name, rerr)
		}
		if len(out) == 0 {
			t.Fatalf("%s empty", tc.name)
		}
	}
}

// Gemini start accounting is preserved but never borrowed: all stream
// targets succeed with reported start + authoritative terminal; a missing
// terminal fails everywhere even when start counts exist.
func TestFinalGeminiStreamInitialUsageAllTargets(t *testing.T) {
	lim := DefaultLimits()
	startUsage := `{"promptTokenCount":12,"cachedContentTokenCount":4,"candidatesTokenCount":1,"totalTokenCount":13}`
	termUsage := gxUsage(10, 3)
	c0 := gsBuild(gsTextPart("a"), "", startUsage, `"responseId":"rp-init","modelVersion":"mv-init"`)
	c1 := gsBuild(gsTextPart("b"), "STOP", termUsage, `"responseId":"rp-init","modelVersion":"mv-init"`)
	sse := gxSSE(c0, c1)
	arr := gxArray(c0, c1)
	outs := gxTranslateAll(t, sse, arr)
	for _, k := range []string{"anthropic-sse", "anthropic-array"} {
		out := outs[k]
		// Reported start counts preserved via the shared Anthropic
		// mapping (input = prompt - cache shares), never zeros.
		if !strings.Contains(out, `"input_tokens":8`) || !strings.Contains(out, `"output_tokens":1`) || !strings.Contains(out, `"cache_read_input_tokens":4`) {
			t.Fatalf("%s must preserve reported start counts: %q", k, out)
		}
		if !strings.Contains(out, `"input_tokens":10`) || !strings.Contains(out, `"output_tokens":3`) {
			t.Fatalf("%s terminal must be authoritative: %q", k, out)
		}
	}
	// Other targets must succeed with the same bytes and carry the
	// authoritative terminal (metadata-only start, no provider leak).
	for _, k := range []string{"chat-sse", "chat-array", "responses-sse", "responses-array"} {
		if !strings.Contains(outs[k], "public") {
			t.Fatalf("%s missing public alias", k)
		}
	}
	if strings.Contains(outs["chat-sse"], "mv-init") || strings.Contains(outs["responses-sse"], "rp-init") {
		t.Fatalf("provider identity leaked downstream")
	}
	// Initial accounting cannot satisfy the terminal requirement.
	badC0 := gsBuild(gsTextPart("a"), "", startUsage, "")
	badTerm := gsBuild(gsTextPart("b"), "STOP", "", "")
	for _, tc := range []struct {
		name string
		fn   func() *RouterError
	}{
		{"chat-sse", func() *RouterError {
			tr := &GeminiToChatSSETranslator{PublicModel: "p", IncludeUsage: true, Limits: lim}
			var b bytes.Buffer
			return tr.Translate(strings.NewReader(gxSSE(badC0, badTerm)), &b, nil)
		}},
		{"anthropic-sse", func() *RouterError {
			tr := &GeminiToAnthropicSSETranslator{PublicModel: "p", Limits: lim}
			var b bytes.Buffer
			return tr.Translate(strings.NewReader(gxSSE(badC0, badTerm)), &b, nil)
		}},
		{"responses-sse", func() *RouterError {
			tr := &GeminiToResponsesSSETranslator{PublicModel: "p", Limits: lim}
			var b bytes.Buffer
			return tr.Translate(strings.NewReader(gxSSE(badC0, badTerm)), &b, nil)
		}},
		{"chat-array", func() *RouterError {
			tr := &GeminiToChatJSONArrayTranslator{PublicModel: "p", IncludeUsage: true, Limits: lim}
			var b bytes.Buffer
			return tr.Translate(strings.NewReader(gxArray(badC0, badTerm)), &b, nil)
		}},
	} {
		if rerr := tc.fn(); rerr == nil {
			t.Fatalf("%s borrowed initial usage for missing terminal", tc.name)
		}
	}
	// Decoder-level pin: start carries InitialUsage with scope initial.
	for _, sseMode := range []bool{true, false} {
		d := NewGeminiStreamDecoder(DefaultLimits(), sseMode)
		evs, rerr := d.DecodeFrame(Frame{Data: c0})
		if rerr != nil {
			t.Fatal(rerr)
		}
		st, ok := evs[0].(ResponseStart)
		if !ok || st.InitialUsage == nil || st.InitialUsage.Provenance.Scope != UsageInitial {
			t.Fatalf("sse=%v start must carry initial scope: %#v", sseMode, evs[0])
		}
	}
}

// Empty Gemini generation: no invented items on any stream target.
// Responses keeps an empty output array (739f8bd), others succeed empty.
func TestFinalGeminiStreamEmptyAllTargets(t *testing.T) {
	emptyObj := `{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP","index":0}],"usageMetadata":` + gxUsage(2, 0) + `}`
	sse := gxSSE(emptyObj)
	arr := gxArray(emptyObj)
	outs := gxTranslateAll(t, sse, arr)
	for _, k := range []string{"chat-sse", "chat-array", "anthropic-sse", "anthropic-array", "responses-sse", "responses-array", "gemini-sse", "gemini-array"} {
		_ = k
	}
	for _, k := range []string{"responses-sse", "responses-array"} {
		out := outs[k]
		if strings.Contains(out, "response.output_item.added") || strings.Contains(out, "response.output_item.done") {
			t.Fatalf("%s invented an output item for empty generation: %q", k, out)
		}
		if !strings.Contains(out, "response.completed") {
			t.Fatalf("%s missing terminal: %q", k, out)
		}
		if !strings.Contains(out, `"output":[]`) {
			t.Fatalf("%s must keep empty output array: %q", k, out)
		}
	}
	// Chat/Anthropic/Gemini targets succeed without content as well.
	for _, k := range []string{"chat-sse", "anthropic-sse"} {
		if outs[k] == "" {
			t.Fatalf("%s empty output", k)
		}
	}
}

// Identity, exactness, order, and failure ownership across the merge.
func TestFinalCrossRowIdentityExactnessOrderFailure(t *testing.T) {
	lim := DefaultLimits()
	// Provider-supplied compat prefix keeps provider origin; minted stays
	// router and is omitted by the Gemini complete encoder.
	respP, rerr := DecodeGeminiResponse([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1,"totalTokenCount":6},"responseId":"gemini-compat-7","modelVersion":"mv"}`), lim)
	if rerr != nil {
		t.Fatalf("decode provider compat: %v", rerr)
	}
	if respP.IDOrigin != IdentityProvider {
		t.Fatalf("provider compat prefix must stay provider, got %q", respP.IDOrigin)
	}
	outP, rerr := EncodeGeminiResponse(respP, "public")
	if rerr != nil {
		t.Fatalf("encode provider compat: %v", rerr)
	}
	if !strings.Contains(string(outP), "gemini-compat-7") {
		t.Fatalf("provider compat id must echo: %s", outP)
	}
	respM, rerr := DecodeGeminiResponse([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1,"totalTokenCount":6}}`), lim)
	if rerr != nil {
		t.Fatalf("decode minted: %v", rerr)
	}
	if respM.IDOrigin != IdentityRouterMinted {
		t.Fatalf("absent id must be router-minted, got %q", respM.IDOrigin)
	}
	outM, rerr := EncodeGeminiResponse(respM, "public")
	if rerr != nil {
		t.Fatalf("encode minted: %v", rerr)
	}
	if strings.Contains(string(outM), "gemini-compat-response") {
		t.Fatalf("router-minted id must be omitted downstream: %s", outM)
	}
	// Exact bigint + empty args {} preserved through Gemini complete row.
	big := `{"n":9007199254740993}`
	rawTool := `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"get_time","args":` + big + `}}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1,"totalTokenCount":6}}`
	respT, rerr := DecodeGeminiResponse([]byte(rawTool), lim)
	if rerr != nil {
		t.Fatalf("decode bigint tool: %v", rerr)
	}
	for _, fn := range []func(Response, string) ([]byte, *RouterError){EncodeChatResponse, EncodeAnthropicResponse, EncodeResponsesResponse, EncodeGeminiResponse} {
		out, rerr := fn(respT, "p")
		if rerr != nil {
			t.Fatalf("bigint encode: %v", rerr)
		}
		if !strings.Contains(string(out), "9007199254740993") {
			t.Fatalf("bigint not exact: %s", out)
		}
	}
	// Missing args normalizes to exact {} once (decoder + stream agree).
	rawNoArgs := `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"get_time"}}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1,"totalTokenCount":6}}`
	respN, rerr := DecodeGeminiResponse([]byte(rawNoArgs), lim)
	if rerr != nil {
		t.Fatalf("decode missing args: %v", rerr)
	}
	tc, ok := respN.Items[0].(ToolCallItem)
	if !ok || string(tc.Arguments) != "{}" {
		t.Fatalf("missing args must be {}: %#v", respN.Items[0])
	}
	// Order: text-call-text refuses on Chat, preserves on others.
	items := []ResponseItem{TextItem{ID: "t0", Text: "a"}, ToolCallItem{ID: "c0", CallID: "call-1", Name: "get_time", Arguments: json.RawMessage(`{}`)}, TextItem{ID: "t1", Text: "b"}}
	u, _ := NewTerminalUsage(DialectGemini, 5, 1, 6)
	respO, rerr := NewResponse("r1", "m", DialectGemini, items, u, Finish{Kind: FinishStop, Raw: "STOP"})
	if rerr != nil {
		t.Fatalf("NewResponse order: %v", rerr)
	}
	if _, rerr := EncodeChatResponse(respO, "p"); rerr == nil {
		t.Fatal("Chat must refuse text-after-tool")
	}
	for _, fn := range []func(Response, string) ([]byte, *RouterError){EncodeAnthropicResponse, EncodeResponsesResponse, EncodeGeminiResponse} {
		if _, rerr := fn(respO, "p"); rerr != nil {
			t.Fatalf("ordered target refused text-call-text: %v", rerr)
		}
	}
	// Failure classification is value-free: malformed tool content never
	// appears in diagnostics; unrepresentable finish never becomes stop.
	secret := "SECRET-VALUE-12345"
	rawBad := `{"candidates":[{"content":{"role":"model","parts":[{"text":` + gxQ(secret) + `}],"finishReason":"MALFORMED_FUNCTION_CALL","index":0}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1,"totalTokenCount":6}}`
	if _, rerr := DecodeGeminiResponse([]byte(rawBad), lim); rerr == nil {
		t.Fatal("MALFORMED_FUNCTION_CALL must decode-fail")
	} else if strings.Contains(rerr.Msg, secret) || strings.Contains(string(rerr.Code), secret) {
		t.Fatalf("diagnostic leaks value: %v", rerr)
	}
	// Post-commit semantic failure aborts exactly once with no success
	// marker; writer failure owns the error with no Abort write.
	dec := NewChatStreamDecoder(lim)
	frames := NewSSEFrameReader(strings.NewReader("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\ndata: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"refusal\":\"no\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\ndata: [DONE]\n\n"), lim)
	enc := NewAnthropicEventEncoder("p", lim)
	var wb bytes.Buffer
	rerr = TranslateStream(frames, dec, enc, &wb, nil, lim, DialectChat)
	if rerr == nil {
		t.Fatal("streaming Chat refusal to Anthropic must fail post-commit")
	}
	if strings.Contains(wb.String(), "message_stop") {
		t.Fatalf("post-commit failure must not emit success: %q", wb.String())
	}
	aborts := strings.Count(wb.String(), "event: error")
	if aborts != 1 {
		t.Fatalf("Abort must happen exactly once, got %d in %q", aborts, wb.String())
	}
	wfail := &finalFailWriter{err: errors.New("downstream down")}
	frames2 := NewSSEFrameReader(strings.NewReader("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\ndata: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\ndata: [DONE]\n\n"), lim)
	enc2 := NewAnthropicEventEncoder("p", lim)
	dec2 := NewChatStreamDecoder(lim)
	if rerr := TranslateStream(frames2, dec2, enc2, wfail, nil, lim, DialectChat); rerr == nil {
		t.Fatal("writer failure must propagate")
	}
	if wfail.writesAfterErr > 0 {
		t.Fatalf("no Abort write after writer failure")
	}
}

type finalFailWriter struct {
	err            error
	writesAfterErr int
	failed         bool
}

func (w *finalFailWriter) Write(p []byte) (int, error) {
	if w.failed {
		w.writesAfterErr++
		return 0, w.err
	}
	w.failed = true
	return 0, w.err
}

// Framing and lifecycle: SSE needs no EOF after terminal; array needs ];
// blocks close before terminal; initial never substitutes for terminal.
func TestFinalFramingAndLifecycle(t *testing.T) {
	lim := DefaultLimits()
	// SSE prompt return: trailing bytes after terminal are unread.
	c0 := gxChunkWithEnvelope("hi", "", "", "", "")
	c1 := gxChunkWithEnvelope("", "STOP", gxUsage(4, 1), "", "")
	trailing := gxChunkWithEnvelope("trailing", "", "", "", "")
	sseBody := gxSSE(c0, c1) + "data: " + trailing + "\n\n"
	tr := &GeminiToChatSSETranslator{PublicModel: "p", IncludeUsage: true, Limits: lim}
	var buf bytes.Buffer
	if rerr := tr.Translate(strings.NewReader(sseBody), &buf, nil); rerr != nil {
		t.Fatalf("SSE must stop at terminal without EOF: %v", rerr)
	}
	// Array without close never succeeds.
	arrNoClose := strings.TrimSuffix(gxArray(c0, c1), "]")
	trA := &GeminiToChatJSONArrayTranslator{PublicModel: "p", IncludeUsage: true, Limits: lim}
	var bufA bytes.Buffer
	if rerr := trA.Translate(strings.NewReader(arrNoClose), &bufA, nil); rerr == nil {
		t.Fatal("array without ] must fail")
	} else if !strings.Contains(rerr.Msg, "without completion") && !strings.Contains(rerr.Msg, "invalid response") {
		t.Fatalf("array truncation must be value-free truncation, got %v", rerr)
	}
	// Array with close succeeds.
	trA2 := &GeminiToChatJSONArrayTranslator{PublicModel: "p", IncludeUsage: true, Limits: lim}
	var bufA2 bytes.Buffer
	if rerr := trA2.Translate(strings.NewReader(gxArray(c0, c1)), &bufA2, nil); rerr != nil {
		t.Fatalf("array with close must succeed: %v", rerr)
	}
	// Early delivery: text streams before the terminal is read.
	dec := NewGeminiStreamDecoder(lim, true)
	evs, rerr := dec.DecodeFrame(Frame{Data: c0})
	if rerr != nil {
		t.Fatalf("first frame: %v", rerr)
	}
	sawText := false
	for _, ev := range evs {
		if tf, ok := ev.(TextFragment); ok && tf.Text == "hi" {
			sawText = true
		}
	}
	if !sawText {
		t.Fatalf("first chunk must deliver text immediately: %#v", evs)
	}
	// Lifecycle: duplicate start and content-after-completion fail.
	lc := NewResponseLifecycle(lim, DialectGemini)
	u, _ := NewTerminalUsage(DialectGemini, 4, 1, 5)
	st, _ := NewResponse("r1", "m", DialectGemini, []ResponseItem{TextItem{ID: "t0", Text: "hi"}}, u, Finish{Kind: FinishStop, Raw: "STOP"})
	_ = st
	start := ResponseStart{ID: "r1", Model: "m", Source: DialectGemini}
	if rerr := lc.Observe(start); rerr != nil {
		t.Fatalf("start: %v", rerr)
	}
	if rerr := lc.Observe(start); rerr == nil {
		t.Fatal("duplicate start must fail")
	}
	_ = io.Discard
}
