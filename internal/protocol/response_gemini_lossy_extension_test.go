// Gemini row lossy-extension policy tests.
//
// Gemini 4 responses observed through the opencode.ai gateway attach a
// thoughtSignature to ordinary text parts on every response, carry
// TEXT-modality usage breakdowns in promptTokensDetails/
// candidatesTokensDetails, and add usage.cost/usage.opencode gateway
// billing members. These tests pin the lossy policy for all three:
// strict keeps the 502 failures, lossy validates and drops them, and
// everything else keeps failing closed in both modes. The observed
// wire shapes below are captured verbatim from live upstream traffic.
package protocol

import (
	"strings"
	"testing"
)

// geminiLossyObservedUsage is the observed usageMetadata shape: TEXT
// modality details on both counters plus the gateway billing envelope.
func geminiLossyObservedUsage() string {
	return `{"candidatesTokensDetails":[{"modality":"TEXT","tokenCount":6}],` +
		`"promptTokensDetails":[{"modality":"TEXT","tokenCount":36}],` +
		`"candidatesTokenCount":6,"promptTokenCount":36,"totalTokenCount":42,` +
		`"cost":0,"opencode":{"billing_source":"go","list_cost":0.000132,"allowance":0.000264,` +
		`"tokens":{"input":36,"output":6,"reasoning":0,"cache_read":0,"cache_write_5m":0,"cache_write_1h":0}}}`
}

// geminiLossyObservedCandidate is the observed complete-response
// candidate: plain text part carrying an opaque thoughtSignature.
func geminiLossyObservedCandidate() string {
	return `[{"finishReason":"STOP","index":0,"content":{"role":"model","parts":[` +
		`{"text":"Hi!","thoughtSignature":"EowBCokBAWkUfRMZ18hkd6hRR6hsYsMtawu0lqI1f1dTa+kb3EQTcVH"}]}}]`
}

func geminiLossyObservedResponse() string {
	return `{"responseId":"vfH9aq3cF92suMYPtvTJ-Ao","modelVersion":"gemini-4-argon",` +
		`"candidates":` + geminiLossyObservedCandidate() + `,` +
		`"usageMetadata":` + geminiLossyObservedUsage() + `}`
}

func TestGeminiLossyExtensionsObservedEnvelope(t *testing.T) {
	// Strict: the signature fails closed first.
	_, rerr := DecodeGeminiResponse([]byte(geminiLossyObservedResponse()), DefaultLimits())
	if rerr == nil || rerr.Status != 502 ||
		!strings.Contains(rerr.Msg, `response.candidates[0].content.parts[0].thoughtSignature`) ||
		!strings.Contains(rerr.Msg, "has no representation") {
		t.Fatalf("strict must fail on the part signature, got %v", rerr)
	}
	// Lossy: the observed envelope decodes; text and accounting survive.
	resp, rerr := DecodeGeminiResponseWithPolicy([]byte(geminiLossyObservedResponse()), DefaultLimits(), true)
	if rerr != nil {
		t.Fatalf("lossy decode of observed envelope: %v", rerr)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("lossy must keep one item, got %d", len(resp.Items))
	}
	if tx, ok := resp.Items[0].(TextItem); !ok || tx.Text != "Hi!" {
		t.Fatalf("lossy must keep the text item, got %#v", resp.Items[0])
	}
	if resp.Usage.Prompt == nil || *resp.Usage.Prompt != 36 ||
		resp.Usage.Completion == nil || *resp.Usage.Completion != 6 ||
		resp.Usage.Total == nil || *resp.Usage.Total != 42 {
		t.Fatalf("lossy must keep authoritative scalars, got %#v", resp.Usage)
	}
	if resp.Finish.Kind != FinishStop {
		t.Fatalf("lossy must keep the finish, got %#v", resp.Finish)
	}
}

func TestGeminiLossySignatureOnFunctionCall(t *testing.T) {
	cand := `[{"content":{"role":"model","parts":[` +
		`{"functionCall":{"name":"f","args":{"a":1},"thoughtSignature":"sig"}}]},` +
		`"finishReason":"STOP"}]`
	raw := `{"candidates":` + cand + `,"usageMetadata":` + geminiIRUsage(5, 2) + `}`
	_, rerr := DecodeGeminiResponse([]byte(raw), DefaultLimits())
	if rerr == nil || !strings.Contains(rerr.Msg, "parts[0].functionCall.thoughtSignature") {
		t.Fatalf("strict must fail on the call signature, got %v", rerr)
	}
	resp, rerr := DecodeGeminiResponseWithPolicy([]byte(raw), DefaultLimits(), true)
	if rerr != nil {
		t.Fatalf("lossy call signature: %v", rerr)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("lossy must keep the call, got %d items", len(resp.Items))
	}
	if _, ok := resp.Items[0].(ToolCallItem); !ok {
		t.Fatalf("lossy must keep the tool call item, got %#v", resp.Items[0])
	}
}

func TestGeminiGatewayBillingOnGeminiRow(t *testing.T) {
	usage := strings.Replace(geminiIRUsage(7, 3), `"totalTokenCount":10`, `"totalTokenCount":10,"cost":0,"opencode":{"billing_source":"go","list_cost":0.5,"allowance":1,"tokens":{"input":7,"output":3,"reasoning":0,"cache_read":0,"cache_write_5m":0,"cache_write_1h":0}}`, 1)
	raw := `{"candidates":` + geminiIRTextCandidate("hello", "STOP") + `,"usageMetadata":` + usage + `}`
	// Strict: gateway members fail closed on the Gemini row too.
	_, rerr := DecodeGeminiResponse([]byte(raw), DefaultLimits())
	if rerr == nil || rerr.Status != 502 || !strings.Contains(rerr.Msg, "response.usageMetadata.cost") {
		t.Fatalf("strict must fail on usage cost, got %v", rerr)
	}
	// Lossy: validated and dropped; accounting stays provider-authoritative.
	resp, rerr := DecodeGeminiResponseWithPolicy([]byte(raw), DefaultLimits(), true)
	if rerr != nil {
		t.Fatalf("lossy gateway billing: %v", rerr)
	}
	if resp.Usage.Prompt == nil || *resp.Usage.Prompt != 7 || resp.Usage.Completion == nil || *resp.Usage.Completion != 3 {
		t.Fatalf("lossy must keep provider counters, got %#v", resp.Usage)
	}
}

func TestGeminiLossyStillFailClosed(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "thought part",
			raw:  `{"candidates":[{"content":{"role":"model","parts":[{"text":"t","thought":true,"thoughtSignature":"s"}]},"finishReason":"STOP"}],"usageMetadata":` + geminiIRUsage(5, 1) + `}`,
			want: "parts[0].thought",
		},
		{
			name: "image modality share",
			raw:  `{"candidates":` + geminiIRTextCandidate("t", "STOP") + `,"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1,"totalTokenCount":6,"promptTokensDetails":[{"modality":"IMAGE","tokenCount":5}]}}`,
			want: "promptTokensDetails[0].modality",
		},
		{
			name: "malformed signature type",
			raw:  `{"candidates":[{"content":{"role":"model","parts":[{"text":"t","thoughtSignature":5}]},"finishReason":"STOP"}],"usageMetadata":` + geminiIRUsage(5, 1) + `}`,
			want: "",
		},
		{
			name: "malformed detail entry member",
			raw:  `{"candidates":` + geminiIRTextCandidate("t", "STOP") + `,"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1,"totalTokenCount":6,"promptTokensDetails":[{"modality":"TEXT","tokenCount":5,"extra":1}]}}`,
			want: "promptTokensDetails[0].extra",
		},
		{
			name: "invalid gateway cost",
			raw:  `{"candidates":` + geminiIRTextCandidate("t", "STOP") + `,"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1,"totalTokenCount":6,"cost":"free"}}`,
			want: "",
		},
		{
			name: "unknown usage member",
			raw:  `{"candidates":` + geminiIRTextCandidate("t", "STOP") + `,"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1,"totalTokenCount":6,"future":1}}`,
			want: "response.usageMetadata.future",
		},
		{
			name: "nonzero tool-use prompt share",
			raw:  `{"candidates":` + geminiIRTextCandidate("t", "STOP") + `,"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1,"totalTokenCount":6,"toolUsePromptTokenCount":4}}`,
			want: "response.usageMetadata.toolUsePromptTokenCount",
		},
	}
	for _, tc := range cases {
		for _, allowDrop := range []bool{false, true} {
			_, rerr := DecodeGeminiResponseWithPolicy([]byte(tc.raw), DefaultLimits(), allowDrop)
			if rerr == nil || rerr.Status != 502 {
				t.Fatalf("%s (allowDrop=%v): expected 502, got %v", tc.name, allowDrop, rerr)
			}
			if tc.want != "" && !strings.Contains(rerr.Msg, tc.want) {
				t.Fatalf("%s (allowDrop=%v): expected path %q, got %q", tc.name, allowDrop, tc.want, rerr.Msg)
			}
		}
	}
}

func TestGeminiLossyThoughtsFold(t *testing.T) {
	// Live default-thinking shape: candidates exclude thoughts, the
	// wire total is prompt + candidates + thoughts (46+7+37=90).
	raw := `{"candidates":` + geminiIRTextCandidate("391", "STOP") + `,"usageMetadata":{"promptTokenCount":46,"candidatesTokenCount":7,"thoughtsTokenCount":37,"totalTokenCount":90}}`
	// Strict keeps the refusal.
	_, rerr := DecodeGeminiResponse([]byte(raw), DefaultLimits())
	if rerr == nil || rerr.Status != 502 || !strings.Contains(rerr.Msg, "response.usageMetadata.thoughtsTokenCount") {
		t.Fatalf("strict must refuse nonzero thoughts, got %v", rerr)
	}
	// Lossy folds the share into completion and carries it in the
	// Reasoning bucket.
	resp, rerr := DecodeGeminiResponseWithPolicy([]byte(raw), DefaultLimits(), true)
	if rerr != nil {
		t.Fatalf("lossy thoughts fold: %v", rerr)
	}
	if resp.Usage.Prompt == nil || *resp.Usage.Prompt != 46 ||
		resp.Usage.Completion == nil || *resp.Usage.Completion != 44 ||
		resp.Usage.Total == nil || *resp.Usage.Total != 90 {
		t.Fatalf("lossy must fold thoughts into completion, got %#v", resp.Usage)
	}
	if resp.Usage.Reasoning == nil || *resp.Usage.Reasoning != 37 {
		t.Fatalf("lossy must carry the reasoning share, got %#v", resp.Usage)
	}
	// A wire total that contradicts the fold fails closed in lossy.
	badTotal := strings.Replace(raw, `"totalTokenCount":90`, `"totalTokenCount":53`, 1)
	if _, rerr := DecodeGeminiResponseWithPolicy([]byte(badTotal), DefaultLimits(), true); rerr == nil || rerr.Status != 502 {
		t.Fatalf("fold equation must stay authoritative in lossy, got %v", rerr)
	}
}

func TestGeminiLossyFinishMessage(t *testing.T) {
	cand := `[{"finishReason":"STOP","finishMessage":"Model generated function call(s).","content":{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{"city":"Tokyo"}}}]}}]`
	raw := `{"candidates":` + cand + `,"usageMetadata":` + geminiIRUsage(94, 22) + `}`
	// Strict: the informational finish message fails closed.
	_, rerr := DecodeGeminiResponse([]byte(raw), DefaultLimits())
	if rerr == nil || rerr.Status != 502 || !strings.Contains(rerr.Msg, "response.candidates[0].finishMessage") {
		t.Fatalf("strict must refuse a nonempty finishMessage, got %v", rerr)
	}
	// Lossy: dropped; the tool call survives.
	resp, rerr := DecodeGeminiResponseWithPolicy([]byte(raw), DefaultLimits(), true)
	if rerr != nil {
		t.Fatalf("lossy finishMessage: %v", rerr)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("lossy must keep the call, got %d items", len(resp.Items))
	}
	if _, ok := resp.Items[0].(ToolCallItem); !ok {
		t.Fatalf("lossy must keep the tool call item, got %#v", resp.Items[0])
	}
	// Malformed finishMessage is invalid upstream data in both modes.
	badType := `[{"finishReason":"STOP","finishMessage":5,"content":{"role":"model","parts":[{"text":"t"}]}}]`
	for _, allowDrop := range []bool{false, true} {
		if _, rerr := DecodeGeminiResponseWithPolicy([]byte(`{"candidates":`+badType+`,"usageMetadata":`+geminiIRUsage(5, 1)+`}`), DefaultLimits(), allowDrop); rerr == nil || rerr.Status != 502 {
			t.Fatalf("malformed finishMessage (allowDrop=%v) must fail, got %v", allowDrop, rerr)
		}
	}
}

func TestTranslateGeminiLossyThoughtsAcrossTargets(t *testing.T) {
	// Live default-thinking envelope (plus the routine extensions).
	raw := `{"responseId":"Y_f9auKEF4vg0_gP9IXQoAY","modelVersion":"gemini-4-argon",` +
		`"candidates":[{"finishReason":"STOP","index":0,"content":{"role":"model","parts":[` +
		`{"text":"391","thoughtSignature":"Eo0BCooBAWkUfRNh5cPOFLmiMOMMMnsKanmUqivWTwumkgg12Xi"}]}}],` +
		`"usageMetadata":{"candidatesTokensDetails":[{"modality":"TEXT","tokenCount":7}],` +
		`"promptTokensDetails":[{"modality":"TEXT","tokenCount":46}],` +
		`"candidatesTokenCount":7,"promptTokenCount":46,"thoughtsTokenCount":37,"totalTokenCount":53}}`
	// Wrong wire total for the fold (prompt+candidates without thoughts):
	// this envelope must fail, proving the fold follows the wire, not the
	// gateway's own accounting.
	if _, rerr := DecodeGeminiResponseWithPolicy([]byte(raw), DefaultLimits(), true); rerr == nil {
		t.Fatalf("total without the thoughts share must fail even in lossy")
	}
	raw = strings.Replace(raw, `"totalTokenCount":53`, `"totalTokenCount":90`, 1)
	// Chat: completion includes reasoning; the share renders natively.
	out, rerr := TranslateGeminiJSONToChatWithPlanAndPolicy([]byte(raw), "gemini-4-argon", DefaultLimits(), nil, true)
	if rerr != nil {
		t.Fatalf("chat lossy thoughts translate: %v", rerr)
	}
	s := string(out)
	for _, want := range []string{`"completion_tokens":44`, `"reasoning_tokens":37`, `"content":"391"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("chat lossy output must carry %s: %s", want, s)
		}
	}
	// Anthropic: lossy projection drops the breakdown, keeps the
	// aggregate output_tokens (thinking tokens are generated tokens).
	out, rerr = TranslateGeminiJSONToAnthropicWithPolicy([]byte(raw), "gemini-4-argon", DefaultLimits(), true)
	if rerr != nil {
		t.Fatalf("anthropic lossy thoughts translate: %v", rerr)
	}
	s = string(out)
	if !strings.Contains(s, `"output_tokens":44`) || strings.Contains(s, "reasoning") {
		t.Fatalf("anthropic lossy projection wrong: %s", s)
	}
	// Anthropic strict keeps the refusal.
	if _, rerr := TranslateGeminiJSONToAnthropicWithPolicy([]byte(raw), "m", DefaultLimits(), false); rerr == nil || rerr.Status != 502 {
		t.Fatalf("anthropic strict must refuse the reasoning breakdown, got %v", rerr)
	}
	// Responses: the share renders natively.
	out, rerr = TranslateGeminiJSONToResponsesWithPlanAndPolicy([]byte(raw), "gemini-4-argon", DefaultLimits(), nil, true)
	if rerr != nil {
		t.Fatalf("responses lossy thoughts translate: %v", rerr)
	}
	s = string(out)
	if !strings.Contains(s, `"reasoning_tokens":37`) || !strings.Contains(s, `"output_tokens":44`) {
		t.Fatalf("responses lossy output wrong: %s", s)
	}
}

func TestGeminiLossyNeverOverridesAccounting(t *testing.T) {
	// Extensions never relax the authoritative equations: a wrong
	// total fails closed in lossy exactly as in strict.
	raw := `{"candidates":` + geminiIRTextCandidate("t", "STOP") + `,"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1,"totalTokenCount":7,"promptTokensDetails":[{"modality":"TEXT","tokenCount":5}]}}`
	for _, allowDrop := range []bool{false, true} {
		_, rerr := DecodeGeminiResponseWithPolicy([]byte(raw), DefaultLimits(), allowDrop)
		if rerr == nil || rerr.Status != 502 {
			t.Fatalf("bad total (allowDrop=%v): expected 502, got %v", allowDrop, rerr)
		}
	}
}

func TestTranslateGeminiLossyDropsNothingDownstream(t *testing.T) {
	raw := []byte(geminiLossyObservedResponse())
	// Chat target.
	out, rerr := TranslateGeminiJSONToChatWithPlanAndPolicy(raw, "gemini-4-argon", DefaultLimits(), nil, true)
	if rerr != nil {
		t.Fatalf("chat lossy translate: %v", rerr)
	}
	s := string(out)
	for _, leak := range []string{"thoughtSignature", "\"cost\"", "opencode", "promptTokensDetails"} {
		if strings.Contains(s, leak) {
			t.Fatalf("chat lossy output must not carry %s: %s", leak, s)
		}
	}
	if !strings.Contains(s, `"content":"Hi!"`) {
		t.Fatalf("chat lossy output must carry the text: %s", s)
	}
	// Anthropic target.
	out, rerr = TranslateGeminiJSONToAnthropicWithPolicy(raw, "gemini-4-argon", DefaultLimits(), true)
	if rerr != nil {
		t.Fatalf("anthropic lossy translate: %v", rerr)
	}
	if strings.Contains(string(out), "thoughtSignature") || !strings.Contains(string(out), "Hi!") {
		t.Fatalf("anthropic lossy output wrong: %s", out)
	}
	// Responses target.
	out, rerr = TranslateGeminiJSONToResponsesWithPlanAndPolicy(raw, "gemini-4-argon", DefaultLimits(), nil, true)
	if rerr != nil {
		t.Fatalf("responses lossy translate: %v", rerr)
	}
	if strings.Contains(string(out), "thoughtSignature") || !strings.Contains(string(out), "Hi!") {
		t.Fatalf("responses lossy output wrong: %s", out)
	}
	// All three targets keep the strict failure.
	for _, strict := range []struct {
		name string
		call func() ([]byte, *RouterError)
	}{
		{"chat", func() ([]byte, *RouterError) {
			return TranslateGeminiJSONToChatWithPlanAndPolicy(raw, "m", DefaultLimits(), nil, false)
		}},
		{"anthropic", func() ([]byte, *RouterError) {
			return TranslateGeminiJSONToAnthropicWithPolicy(raw, "m", DefaultLimits(), false)
		}},
		{"responses", func() ([]byte, *RouterError) {
			return TranslateGeminiJSONToResponsesWithPlanAndPolicy(raw, "m", DefaultLimits(), nil, false)
		}},
	} {
		if _, rerr := strict.call(); rerr == nil || rerr.Status != 502 {
			t.Fatalf("%s strict must fail, got %v", strict.name, rerr)
		}
	}
}
