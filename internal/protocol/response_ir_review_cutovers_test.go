package protocol

// Independent review probes for the four COMPLETE source-row cutovers.
// Additive only: no production/shared/test/doc edits. Each probe asserts
// a contract-audit property through the public Translate* wrappers.

import (
	"encoding/json"
	"strings"
	"testing"
)

func reviewRawResponses(status, output, usage, extra string) string {
	return `{"id":"resp_review","object":"response","created_at":1746989954.0,"model":"up",` +
		`"status":"` + status + `","error":null,"incomplete_details":null,` +
		`"output":` + output + `,"usage":` + usage + extra + `}`
}

func reviewUsage(prompt, completion int, details string) string {
	total := prompt + completion
	u := `{"input_tokens":` + respItoa(prompt) +
		`,"output_tokens":` + respItoa(completion) +
		`,"total_tokens":` + respItoa(total)
	if details != "" {
		u += `,` + details
	}
	return u + `}`
}

func reviewMustOK(t *testing.T, raw string, fn func([]byte, string, Limits) ([]byte, *RouterError)) []byte {
	t.Helper()
	out, rerr := fn([]byte(raw), "public", DefaultLimits())
	if rerr != nil {
		t.Fatalf("expected success, got %v", rerr)
	}
	return out
}

func reviewMust502(t *testing.T, raw string, fn func([]byte, string, Limits) ([]byte, *RouterError)) *RouterError {
	t.Helper()
	_, rerr := fn([]byte(raw), "public", DefaultLimits())
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("expected 502, got %v", rerr)
	}
	if rerr.Code != CodeUpstreamBad {
		t.Fatalf("must be upstream failure, got %+v", rerr)
	}
	return rerr
}

// Deliberate change 1: Responses incomplete+content_filter renders the
// Anthropic refusal terminal (Chat-row precedent), not a 502.
// Source validation accepts the incomplete status; target
// representability maps canonical ContentFilter to refusal.
func TestReviewResponsesIncompleteFilterToAnthropicRefusal(t *testing.T) {
	textOut := `[{"id":"m","type":"message","role":"assistant","status":"incomplete",` +
		`"content":[{"type":"output_text","text":"partial","annotations":[]}]}]`
	raw := strings.Replace(reviewRawResponses("incomplete", textOut, reviewUsage(3, 2, ""), ""),
		`"incomplete_details":null`, `"incomplete_details":{"reason":"content_filter"}`, 1)
	out := reviewMustOK(t, raw, TranslateResponsesJSONToAnthropic)
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	if body["stop_reason"] != "refusal" {
		t.Fatalf("incomplete content_filter must render refusal, got %s", out)
	}
	// Chat and Gemini render their own filter terminals from the same source.
	chatOut := reviewMustOK(t, raw, TranslateResponsesJSONToChat)
	if !strings.Contains(string(chatOut), `"finish_reason":"content_filter"`) {
		t.Fatalf("chat filter terminal: %s", chatOut)
	}
	gemOut := reviewMustOK(t, raw, TranslateResponsesJSONToGemini)
	if !strings.Contains(string(gemOut), "SAFETY") {
		t.Fatalf("gemini filter terminal: %s", gemOut)
	}
	// Unknown incomplete reasons still fail value-free on all three.
	badReason := strings.Replace(reviewRawResponses("incomplete", textOut, reviewUsage(3, 2, ""), ""),
		`"incomplete_details":null`, `"incomplete_details":{"reason":"mystery_bogus"}`, 1)
	for _, fn := range []func([]byte, string, Limits) ([]byte, *RouterError){
		TranslateResponsesJSONToChat, TranslateResponsesJSONToAnthropic, TranslateResponsesJSONToGemini,
	} {
		rerr := reviewMust502(t, badReason, fn)
		if strings.Contains(rerr.Msg, "mystery_bogus") {
			t.Fatalf("diagnostic must be value-free, got %q", rerr.Msg)
		}
	}
}

// Deliberate change 2: valid empty output renders end_turn with one empty
// text block. Source validation: missing/null output still fails;
// present-but-empty output is a valid zero-item Stop that the Anthropic
// target represents as end_turn.
func TestReviewResponsesEmptyOutputPolicy(t *testing.T) {
	emptyMsg := `[{"id":"m1","type":"message","role":"assistant","status":"completed","content":[]}]`
	raw := reviewRawResponses("completed", emptyMsg, reviewUsage(2, 1, ""), "")
	out := reviewMustOK(t, raw, TranslateResponsesJSONToAnthropic)
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	if body["stop_reason"] != "end_turn" {
		t.Fatalf("empty output must render end_turn, got %s", out)
	}
	content, _ := body["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["type"] != "text" ||
		content[0].(map[string]any)["text"] != "" {
		t.Fatalf("empty completion must render one empty text block: %s", out)
	}
	// Missing/null output still fails at source decode on all three.
	for _, bad := range []string{
		strings.Replace(reviewRawResponses("completed", emptyMsg, reviewUsage(2, 1, ""), ""), `"output":`, `"output_missing":`, 1),
		strings.Replace(reviewRawResponses("completed", emptyMsg, reviewUsage(2, 1, ""), ""), `"output":`+emptyMsg, `"output":null`, 1),
	} {
		for _, fn := range []func([]byte, string, Limits) ([]byte, *RouterError){
			TranslateResponsesJSONToChat, TranslateResponsesJSONToAnthropic, TranslateResponsesJSONToGemini,
		} {
			reviewMust502(t, bad, fn)
		}
	}
}

// Ordered blocks: tool-first preserves order on Anthropic/Gemini and
// refuses on Chat with an explicit order gate (never silent reorder).
func TestReviewResponsesOrderGate(t *testing.T) {
	toolFirst := `[{"id":"f1","type":"function_call","status":"completed","call_id":"c1","name":"a","arguments":"{}"},` +
		`{"id":"m","type":"message","role":"assistant","status":"completed",` +
		`"content":[{"type":"output_text","text":"after","annotations":[]}]}]`
	raw := reviewRawResponses("completed", toolFirst, reviewUsage(1, 1, ""), "")
	rerr := reviewMust502(t, raw, TranslateResponsesJSONToChat)
	if !strings.Contains(rerr.Msg, "response.items order") {
		t.Fatalf("chat order gate must name path, got %q", rerr.Msg)
	}
	for _, fn := range []func([]byte, string, Limits) ([]byte, *RouterError){
		TranslateResponsesJSONToAnthropic, TranslateResponsesJSONToGemini,
	} {
		out, rerr := fn([]byte(raw), "public", DefaultLimits())
		if rerr != nil {
			t.Fatalf("ordered target must preserve tool-first: %v", rerr)
		}
		if strings.Index(string(out), "c1") > strings.Index(string(out), "after") {
			t.Fatalf("tool-first order lost: %s", out)
		}
	}
}

// Opaque call IDs and exact numerics survive all three Responses targets.
func TestReviewResponsesOpaqueIDAndBigint(t *testing.T) {
	output := `[{"id":"m","type":"message","role":"assistant","status":"completed",` +
		`"content":[{"type":"output_text","text":"hi","annotations":[]}]},` +
		`{"id":"fc_9","type":"function_call","status":"completed","call_id":"has:colon and space","name":"get_time","arguments":"{\"n\":9007199254740993}"}]`
	raw := reviewRawResponses("completed", output, reviewUsage(10, 5, ""), "")
	for _, fn := range []func([]byte, string, Limits) ([]byte, *RouterError){
		TranslateResponsesJSONToChat, TranslateResponsesJSONToAnthropic, TranslateResponsesJSONToGemini,
	} {
		out := reviewMustOK(t, raw, fn)
		if !strings.Contains(string(out), "has:colon and space") || !strings.Contains(string(out), "9007199254740993") {
			t.Fatalf("opaque/bigint lost: %s", out)
		}
	}
	// Duplicate call IDs fail on all three.
	dup := `[{"id":"f1","type":"function_call","status":"completed","call_id":"d","name":"a","arguments":"{}"},` +
		`{"id":"f2","type":"function_call","status":"completed","call_id":"d","name":"b","arguments":"{}"}]`
	for _, fn := range []func([]byte, string, Limits) ([]byte, *RouterError){
		TranslateResponsesJSONToChat, TranslateResponsesJSONToAnthropic, TranslateResponsesJSONToGemini,
	} {
		reviewMust502(t, reviewRawResponses("completed", dup, reviewUsage(1, 1, ""), ""), fn)
	}
}

// Prompt/cache/reasoning math by source: Responses prompt is verbatim,
// Anthropic target subtracts both shares, reasoning refuses on Anthropic.
func TestReviewResponsesUsageMath(t *testing.T) {
	textOut := `[{"id":"m","type":"message","role":"assistant","status":"completed",` +
		`"content":[{"type":"output_text","text":"hi","annotations":[]}]}]`
	raw := reviewRawResponses("completed", textOut,
		reviewUsage(12, 7, `"input_tokens_details":{"cached_tokens":4}`), "")
	aOut := reviewMustOK(t, raw, TranslateResponsesJSONToAnthropic)
	var aBody map[string]any
	if err := json.Unmarshal(aOut, &aBody); err != nil {
		t.Fatal(err)
	}
	aUsage := aBody["usage"].(map[string]any)
	if aUsage["input_tokens"] != float64(8) || aUsage["cache_read_input_tokens"] != float64(4) {
		t.Fatalf("anthropic usage math wrong: %v", aUsage)
	}
	// Nonzero reasoning travels to Chat/Gemini, refuses on Anthropic.
	reasonRaw := reviewRawResponses("completed", textOut,
		reviewUsage(7, 3, `"output_tokens_details":{"reasoning_tokens":2}`), "")
	reviewMustOK(t, reasonRaw, TranslateResponsesJSONToChat)
	reviewMustOK(t, reasonRaw, TranslateResponsesJSONToGemini)
	rerr := reviewMust502(t, reasonRaw, TranslateResponsesJSONToAnthropic)
	if !strings.Contains(rerr.Msg, "reasoning_tokens") {
		t.Fatalf("reasoning gate must name field, got %q", rerr.Msg)
	}
	// Nonzero cache creation travels only to Anthropic.
	writeRaw := reviewRawResponses("completed", textOut,
		reviewUsage(7, 3, `"input_tokens_details":{"cached_tokens":2,"cache_write_tokens":1}`), "")
	reviewMustOK(t, writeRaw, TranslateResponsesJSONToAnthropic)
	reviewMust502(t, writeRaw, TranslateResponsesJSONToChat)
	reviewMust502(t, writeRaw, TranslateResponsesJSONToGemini)
	// Totals overflow fails on all three.
	badTotals := reviewRawResponses("completed", textOut,
		`{"input_tokens":9223372036854775807,"output_tokens":1,"total_tokens":7}`, "")
	for _, fn := range []func([]byte, string, Limits) ([]byte, *RouterError){
		TranslateResponsesJSONToChat, TranslateResponsesJSONToAnthropic, TranslateResponsesJSONToGemini,
	} {
		reviewMust502(t, badTotals, fn)
	}
}

// Unknown nulls are newer defaults; unknown non-nulls fail closed.
func TestReviewResponsesUnknownNullPolicy(t *testing.T) {
	textOut := `[{"id":"m","type":"message","role":"assistant","status":"completed",` +
		`"content":[{"type":"output_text","text":"hi","annotations":[]}]}]`
	withNulls := reviewRawResponses("completed", textOut,
		`{"input_tokens":5,"output_tokens":2,"total_tokens":7,"future_usage":null}`, "")
	for _, fn := range []func([]byte, string, Limits) ([]byte, *RouterError){
		TranslateResponsesJSONToChat, TranslateResponsesJSONToAnthropic, TranslateResponsesJSONToGemini,
	} {
		reviewMustOK(t, withNulls, fn)
	}
	withFuture := reviewRawResponses("completed", textOut, reviewUsage(1, 1, ""), `,"future_feature":{"x":1}`)
	for _, fn := range []func([]byte, string, Limits) ([]byte, *RouterError){
		TranslateResponsesJSONToChat, TranslateResponsesJSONToAnthropic, TranslateResponsesJSONToGemini,
	} {
		rerr := reviewMust502(t, withFuture, fn)
		if !strings.Contains(rerr.Msg, "response.future_feature") {
			t.Fatalf("unknown gate must name path, got %q", rerr.Msg)
		}
	}
}

// Caps: oversize per-args and tool count fail on all three Responses targets.
func TestReviewResponsesCaps(t *testing.T) {
	textOut := `[{"id":"m","type":"message","role":"assistant","status":"completed",` +
		`"content":[{"type":"output_text","text":"hi","annotations":[]}]}]`
	big := `{"a":"` + strings.Repeat("x", DefaultLimits().MaxArgsChars) + `"}`
	bigOut := `[{"id":"f1","type":"function_call","status":"completed","call_id":"c1","name":"a","arguments":` +
		`"` + strings.Replace(big, `"`, `\"`, -1) + `"}]`
	_ = textOut
	for _, fn := range []func([]byte, string, Limits) ([]byte, *RouterError){
		TranslateResponsesJSONToChat, TranslateResponsesJSONToAnthropic, TranslateResponsesJSONToGemini,
	} {
		reviewMust502(t, reviewRawResponses("completed", bigOut, reviewUsage(1, 1, ""), ""), fn)
	}
	small := DefaultLimits()
	small.MaxToolCalls = 1
	twoCalls := `[{"id":"f1","type":"function_call","status":"completed","call_id":"c1","name":"a","arguments":"{}"},` +
		`{"id":"f2","type":"function_call","status":"completed","call_id":"c2","name":"b","arguments":"{}"}]`
	for _, fn := range []func([]byte, string, Limits) ([]byte, *RouterError){
		TranslateResponsesJSONToChat, TranslateResponsesJSONToAnthropic, TranslateResponsesJSONToGemini,
	} {
		if _, rerr := fn([]byte(reviewRawResponses("completed", twoCalls, reviewUsage(1, 1, ""), "")), "public", small); rerr == nil || rerr.Status != 502 {
			t.Fatalf("tool count must fail: %v", rerr)
		}
	}
}

// Gemini identity provenance: provider IDs echo, router-minted IDs never
// echo; absent model stays empty with router origin; public model stamped.
func TestReviewGeminiIdentityProvenance(t *testing.T) {
	mkGem := func(modelFrag, idFrag string) string {
		return `{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},` +
			`"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":2,` +
			`"candidatesTokenCount":1,"totalTokenCount":3}` + modelFrag + idFrag + `}`
	}
	// Provider-supplied compat-prefix ID keeps provider origin and echoes.
	raw := mkGem(`,"modelVersion":"m"`, `,"responseId":"gemini-compat-9"`)
	resp, rerr := DecodeGeminiResponse([]byte(raw), DefaultLimits())
	if rerr != nil || resp.IDOrigin != IdentityProvider {
		t.Fatalf("provider compat prefix must keep provider origin: %+v %v", resp, rerr)
	}
	for _, fn := range []func([]byte, string, Limits) ([]byte, *RouterError){
		TranslateGeminiJSONToChat, TranslateGeminiJSONToAnthropic, TranslateGeminiJSONToResponses,
	} {
		_ = reviewMustOK(t, raw, fn)
	}
	gOut := reviewMustOK(t, raw, TranslateGeminiJSONToChat)
	_ = gOut
	gemOut, rerr := TranslateGeminiJSONToChat([]byte(raw), "public", DefaultLimits())
	if rerr != nil {
		t.Fatal(rerr)
	}
	_ = gemOut
	direct, rerr := EncodeGeminiResponse(resp, "public")
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !strings.Contains(string(direct), "gemini-compat-9") {
		t.Fatalf("provider id must echo in Gemini target: %s", direct)
	}
	// Absent responseId is router-minted and omitted by the Gemini encoder.
	rawAbsent := mkGem(`,"modelVersion":"m"`, ``)
	resp2, rerr := DecodeGeminiResponse([]byte(rawAbsent), DefaultLimits())
	if rerr != nil || resp2.IDOrigin != IdentityRouterMinted {
		t.Fatalf("absent id must be router origin: %+v %v", resp2, rerr)
	}
	direct2, rerr := EncodeGeminiResponse(resp2, "public")
	if rerr != nil {
		t.Fatal(rerr)
	}
	if strings.Contains(string(direct2), "responseId") {
		t.Fatalf("router-minted id must never echo: %s", direct2)
	}
	// Absent modelVersion stays empty with router origin (no fake string).
	rawNoModel := mkGem(``, `,"responseId":"r1"`)
	resp3, rerr := DecodeGeminiResponse([]byte(rawNoModel), DefaultLimits())
	if rerr != nil || resp3.Model != "" || resp3.ModelOrigin != IdentityRouterMinted {
		t.Fatalf("absent model must stay empty with router origin: %+v %v", resp3, rerr)
	}
}

// Anthropic strictness retained: complete tool_use input required,
// stop_sequence refused only in Responses, unknown stops value-free.
func TestReviewAnthropicStrictnessRetained(t *testing.T) {
	mk := func(content, usage string) string {
		return `{"id":"msg_1","type":"message","role":"assistant","model":"c",` +
			`"content":` + content + `,"stop_reason":"tool_use","usage":` + usage + `}`
	}
	usage := `{"input_tokens":2,"output_tokens":1}`
	for _, bad := range []string{
		mk(`[{"type":"tool_use","id":"c1","name":"f"}]`, usage),
		mk(`[{"type":"tool_use","id":"c1","name":"f","input":null}]`, usage),
	} {
		for _, fn := range []func([]byte, string, Limits) ([]byte, *RouterError){
			TranslateAnthropicJSON, TranslateAnthropicJSONToGemini, TranslateAnthropicJSONToResponses,
		} {
			reviewMust502(t, bad, fn)
		}
	}
	ss := `{"id":"msg_1","type":"message","role":"assistant","model":"c",` +
		`"content":[{"type":"text","text":"hi"}],"stop_reason":"stop_sequence",` +
		`"stop_sequence":"x","usage":` + usage + `}`
	reviewMustOK(t, ss, TranslateAnthropicJSON)
	reviewMustOK(t, ss, TranslateAnthropicJSONToGemini)
	rerr := reviewMust502(t, ss, TranslateAnthropicJSONToResponses)
	if !strings.Contains(rerr.Msg, "stop_reason") {
		t.Fatalf("stop_sequence gate must name path, got %q", rerr.Msg)
	}
	bogus := `{"id":"msg_1","type":"message","role":"assistant","model":"c",` +
		`"content":[{"type":"text","text":"hi"}],"stop_reason":"bogus","usage":` + usage + `}`
	for _, fn := range []func([]byte, string, Limits) ([]byte, *RouterError){
		TranslateAnthropicJSON, TranslateAnthropicJSONToGemini, TranslateAnthropicJSONToResponses,
	} {
		rerr := reviewMust502(t, bogus, fn)
		if strings.Contains(rerr.Msg, "bogus") {
			t.Fatalf("stop diagnostic must be value-free, got %q", rerr.Msg)
		}
	}
}

func TestReviewChatUnknownFinishValueFree(t *testing.T) {
	const secret = "private-upstream-finish-value"
	raw := `{"id":"chat-review","object":"chat.completion","created":1700000000,"model":"up","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"` + secret + `"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	for _, fn := range []func([]byte, string, Limits) ([]byte, *RouterError){TranslateChatJSONToAnthropic, TranslateChatJSONToResponses, TranslateChatJSONToGemini} {
		err := reviewMust502(t, raw, fn)
		if strings.Contains(err.Msg, secret) {
			t.Fatalf("finish value leaked: %s", err.Msg)
		}
	}
}
