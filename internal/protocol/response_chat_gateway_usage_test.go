// Chat gateway usage extensions and Anthropic reasoning projection.
//
// Native live DeepSeek terminal frame carries gateway accounting alongside
// authoritative totals:
//
//	usage {prompt_tokens:36, completion_tokens:18, total_tokens:54,
//	  prompt_tokens_details:{cached_tokens:0},
//	  completion_tokens_details:{reasoning_tokens:15},
//	  cost:0,
//	  opencode:{billing_source, list_cost, allowance,
//	    tokens:{input,output,reasoning,cache_read,cache_write_5m,cache_write_1h}}}
//
// Strict decoders reject cost/opencode (unknown members). Lossy decoders
// (WithPolicy, allowPrivateDrop true) validate and drop them: bounded safe
// shape/number types, authoritative top scalar equations preserved, no
// reliance on optional gateway accounting, nonzero media/unknown still
// closed, data never echoed.
//
// Lossy Anthropic-target projection drops the Chat-source reasoning
// breakdown while preserving aggregate output_tokens. Responses/Gemini
// accounting and strict Anthropic unchanged.
//
// Both complete and stream rows are proven; truncation never becomes a
// synthetic success and [DONE]/finish are never fabricated.
package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// --- DeepSeek frame builders (independent wire shapes) ---------------------

func deepSeekGatewayUsage() string {
	return `{"prompt_tokens":36,"completion_tokens":18,"total_tokens":54,` +
		`"prompt_tokens_details":{"cached_tokens":0},` +
		`"completion_tokens_details":{"reasoning_tokens":15},` +
		`"cost":0,` +
		`"opencode":{"billing_source":"api","list_cost":0.001,"allowance":10,` +
		`"tokens":{"input":36,"output":18,"reasoning":15,"cache_read":0,"cache_write_5m":0,"cache_write_1h":0}}}`
}

func deepSeekCompleteRaw(usage string) string {
	return `{"id":"chatcmpl-ds1","object":"chat.completion","created":1,"model":"deepseek",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
		`"usage":` + usage + `}`
}

func mustNotContain(t *testing.T, out, secret string) {
	t.Helper()
	if strings.Contains(out, secret) {
		t.Fatalf("output echoes private gateway data %q:\n%s", secret, out)
	}
}

func mustNotContainErr(t *testing.T, rerr *RouterError, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if s != "" && strings.Contains(rerr.Msg, s) {
			t.Fatalf("error echoes upstream value %q: %q", s, rerr.Msg)
		}
	}
}

// --- Complete: strict rejects, lossy drops ---------------------------------

func TestChatGatewayUsageCompleteStrictVsLossy(t *testing.T) {
	raw := deepSeekCompleteRaw(deepSeekGatewayUsage())
	// Strict: unknown gateway members fail closed with value-free paths.
	_, rerr := DecodeChatResponse([]byte(raw), DefaultLimits())
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("strict gateway must 502, got %v", rerr)
	}
	if !strings.Contains(rerr.Msg, "has no representation") {
		t.Fatalf("strict gateway must name the feature path: %q", rerr.Msg)
	}
	mustNotContainErr(t, rerr, "api", "0.001")

	// Lossy: drops cost/opencode, keeps authoritative totals and reasoning.
	resp, rerr := DecodeChatResponseWithPolicy([]byte(raw), DefaultLimits(), true)
	if rerr != nil {
		t.Fatalf("lossy gateway must decode: %v", rerr)
	}
	u := resp.Usage
	if *u.Prompt != 36 || *u.Completion != 18 || *u.Total != 54 {
		t.Fatalf("lossy totals wrong: %+v", u)
	}
	if u.Reasoning == nil || *u.Reasoning != 15 {
		t.Fatalf("lossy reasoning lost: %+v", u)
	}
	if u.CacheRead == nil || *u.CacheRead != 0 {
		// cached_tokens:0 is exact; explicit zero is carried as nil-or-zero?
		// parseChatUsageBuckets carries cached even when zero (cached=0).
		// Accept either nil or 0, but never nonzero.
		if u.CacheRead != nil && *u.CacheRead != 0 {
			t.Fatalf("cache_read wrong: %+v", u)
		}
	}
	if rerr := u.ValidateAuthoritative(); rerr != nil {
		t.Fatalf("lossy usage not authoritative: %v", rerr)
	}
	if resp.Finish.Kind != FinishStop {
		t.Fatalf("finish wrong: %+v", resp.Finish)
	}
	// Encoded downstream must not carry gateway accounting anywhere.
	for _, fn := range []func(Response, string) ([]byte, *RouterError){
		func(r Response, p string) ([]byte, *RouterError) { return EncodeChatResponse(r, p) },
		func(r Response, p string) ([]byte, *RouterError) { return EncodeResponsesResponse(r, p) },
		func(r Response, p string) ([]byte, *RouterError) { return EncodeGeminiResponse(r, p) },
	} {
		out, rerr := fn(resp, "public")
		if rerr != nil {
			t.Fatalf("downstream encode failed (gateway must be dropped, not refused): %v", rerr)
		}
		s := string(out)
		for _, secret := range []string{"opencode", "billing_source", "list_cost", "allowance", "cache_write_5m", "cache_write_1h"} {
			if strings.Contains(s, secret) {
				t.Fatalf("encoded output leaks gateway metadata %q: %s", secret, s)
			}
		}
		mustNotContain(t, s, "api")
	}
}

func TestChatGatewayUsageCompleteValidation(t *testing.T) {
	mk := func(usage string) string { return deepSeekCompleteRaw(usage) }

	// Lossy still distinguishes nonzero media: audio/image/prediction fail.
	for _, usage := range []string{
		`{"prompt_tokens":36,"completion_tokens":18,"total_tokens":54,"prompt_tokens_details":{"audio_tokens":1},"completion_tokens_details":{"reasoning_tokens":15},"cost":0,"opencode":{"billing_source":"a","list_cost":0,"allowance":0,"tokens":{"input":36,"output":18,"reasoning":15,"cache_read":0,"cache_write_5m":0,"cache_write_1h":0}}}`,
		`{"prompt_tokens":36,"completion_tokens":18,"total_tokens":54,"prompt_tokens_details":{"image_tokens":2},"completion_tokens_details":{"reasoning_tokens":15},"cost":0}`,
		`{"prompt_tokens":36,"completion_tokens":18,"total_tokens":54,"completion_tokens_details":{"accepted_prediction_tokens":1},"cost":0}`,
		`{"prompt_tokens":36,"completion_tokens":18,"total_tokens":54,"completion_tokens_details":{"reasoning_tokens":15},"cost":0,"future":1}`,
	} {
		if _, rerr := DecodeChatResponseWithPolicy([]byte(mk(usage)), DefaultLimits(), true); rerr == nil || rerr.Status != 502 {
			t.Fatalf("lossy must still refuse nonzero media/unknown %q, got %v", usage, rerr)
		} else if !strings.Contains(rerr.Msg, "has no representation") {
			t.Fatalf("media/unknown must name path: %v", rerr)
		}
	}

	// Malformed gateway types fail as invalid (not silent drops), value-free.
	secretBilling := "secret-billing-999"
	for _, usage := range []string{
		`{"prompt_tokens":36,"completion_tokens":18,"total_tokens":54,"cost":"free"}`,
		`{"prompt_tokens":36,"completion_tokens":18,"total_tokens":54,"cost":true}`,
		`{"prompt_tokens":36,"completion_tokens":18,"total_tokens":54,"cost":-1}`,
		`{"prompt_tokens":36,"completion_tokens":18,"total_tokens":54,"opencode":{"billing_source":5,"list_cost":0,"allowance":0}}`,
		`{"prompt_tokens":36,"completion_tokens":18,"total_tokens":54,"opencode":{"billing_source":"` + secretBilling + `","list_cost":"x","allowance":0}}`,
		`{"prompt_tokens":36,"completion_tokens":18,"total_tokens":54,"opencode":{"billing_source":"a","list_cost":0,"allowance":0,"tokens":{"input":"36"}}}`,
		`{"prompt_tokens":36,"completion_tokens":18,"total_tokens":54,"opencode":{"billing_source":"a","list_cost":0,"allowance":0,"tokens":{"input":-1}}}`,
		`{"prompt_tokens":36,"completion_tokens":18,"total_tokens":54,"opencode":{"future":1}}`,
		`{"prompt_tokens":36,"completion_tokens":18,"total_tokens":54,"opencode":{"billing_source":"a","list_cost":0,"allowance":0,"tokens":{"future":1}}}`,
		`{"prompt_tokens":36,"completion_tokens":18,"total_tokens":54,"opencode":[]}`,
	} {
		_, rerr := DecodeChatResponseWithPolicy([]byte(mk(usage)), DefaultLimits(), true)
		if rerr == nil || rerr.Status != 502 {
			t.Fatalf("malformed gateway %q must 502, got %v", usage, rerr)
		}
		mustNotContainErr(t, rerr, secretBilling, "free")
	}

	// Unknown opencode member fails even in lossy with value-free path.
	_, rerr := DecodeChatResponseWithPolicy([]byte(mk(`{"prompt_tokens":36,"completion_tokens":18,"total_tokens":54,"opencode":{"billing_source":"a","mystery":1}}`)), DefaultLimits(), true)
	if rerr == nil || !strings.Contains(rerr.Msg, "response.usage.opencode") {
		t.Fatalf("unknown opencode member must name path, got %v", rerr)
	}

	// Authoritative equations still govern; gateway numbers never satisfy them.
	for _, usage := range []string{
		// total != prompt+completion, even though gateway tokens agree.
		`{"prompt_tokens":36,"completion_tokens":18,"total_tokens":55,"cost":0,"opencode":{"billing_source":"a","list_cost":0,"allowance":0,"tokens":{"input":36,"output":18,"reasoning":15}}}`,
		// reasoning > completion.
		`{"prompt_tokens":36,"completion_tokens":18,"total_tokens":54,"completion_tokens_details":{"reasoning_tokens":19},"cost":0}`,
		// cached > prompt.
		`{"prompt_tokens":36,"completion_tokens":18,"total_tokens":54,"prompt_tokens_details":{"cached_tokens":37},"cost":0}`,
	} {
		if _, rerr := DecodeChatResponseWithPolicy([]byte(mk(usage)), DefaultLimits(), true); rerr == nil {
			t.Fatalf("bad equation %q must fail, got success", usage)
		}
	}

	// No reliance on gateway accounting: mismatched opencode.tokens still
	// decodes as long as top scalars are exact.
	mismatched := `{"prompt_tokens":36,"completion_tokens":18,"total_tokens":54,` +
		`"completion_tokens_details":{"reasoning_tokens":15},"cost":1,` +
		`"opencode":{"billing_source":"other","list_cost":99.5,"allowance":0,` +
		`"tokens":{"input":1,"output":2,"reasoning":3,"cache_read":4,"cache_write_5m":5,"cache_write_1h":6}}}`
	resp, rerr := DecodeChatResponseWithPolicy([]byte(mk(mismatched)), DefaultLimits(), true)
	if rerr != nil {
		t.Fatalf("mismatched gateway accounting must not affect totals: %v", rerr)
	}
	if *resp.Usage.Prompt != 36 || *resp.Usage.Completion != 18 || *resp.Usage.Total != 54 {
		t.Fatalf("totals must come from top scalars, got %+v", resp.Usage)
	}
	if *resp.Usage.Reasoning != 15 {
		t.Fatalf("reasoning must come from details, got %+v", resp.Usage)
	}

	// Null gateway members read as absent (exact in both modes).
	nullGW := `{"prompt_tokens":36,"completion_tokens":18,"total_tokens":54,"cost":null,"opencode":null}`
	if _, rerr := DecodeChatResponse([]byte(mk(nullGW)), DefaultLimits()); rerr != nil {
		t.Fatalf("strict null gateway must decode: %v", rerr)
	}
	if _, rerr := DecodeChatResponseWithPolicy([]byte(mk(nullGW)), DefaultLimits(), true); rerr != nil {
		t.Fatalf("lossy null gateway must decode: %v", rerr)
	}
}

// --- Streaming: strict rejects, lossy drops, terminal gating holds --------

func TestChatGatewayUsageStreamStrictVsLossy(t *testing.T) {
	usage := deepSeekGatewayUsage()
	datas := []string{
		csChunk(`{"content":"hi"}`, ""),
		csChunk(`{}`, "stop"),
		csUsageChunk(usage),
		"[DONE]",
	}
	// Strict: terminal usage chunk with gateway fails at the usage frame.
	dec := NewChatStreamDecoder(DefaultLimits())
	failedAt := -1
	for i, d := range datas {
		if _, rerr := dec.DecodeFrame(Frame{Data: d}); rerr != nil {
			failedAt = i
			if !strings.Contains(rerr.Msg, "has no representation") {
				t.Fatalf("strict stream gateway must name path: %v", rerr)
			}
			mustNotContainErr(t, rerr, "api", "billing_source")
			break
		}
	}
	if failedAt != 2 {
		t.Fatalf("strict must fail at usage frame (2), failed at %d", failedAt)
	}

	// Lossy: drops gateway, terminates with authoritative usage.
	dec = NewChatStreamDecoderWithPolicy(DefaultLimits(), true)
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
	var sawUsage, sawComplete bool
	for _, ev := range all {
		if u, ok := ev.(UsageEvent); ok && u.Terminal() {
			sawUsage = true
			if *u.Usage.Prompt != 36 || *u.Usage.Completion != 18 || *u.Usage.Total != 54 {
				t.Fatalf("stream totals wrong: %+v", u.Usage)
			}
			if u.Usage.Reasoning == nil || *u.Usage.Reasoning != 15 {
				t.Fatalf("stream reasoning lost: %+v", u.Usage)
			}
		}
		if c, ok := ev.(CompletionEvent); ok && c.Finish.Kind == FinishStop {
			sawComplete = true
		}
	}
	if !sawUsage || !sawComplete {
		t.Fatalf("lossy stream missing terminal: usage=%v complete=%v: %#v", sawUsage, sawComplete, all)
	}
}

func TestChatGatewayUsageStreamTerminalGating(t *testing.T) {
	usage := deepSeekGatewayUsage()
	// Truncation never becomes success, even with lossy gateway drop.
	truncatedSets := [][]string{
		// Missing usage.
		{csChunk(`{"content":"hi"}`, ""), csChunk(`{}`, "stop"), "[DONE]"},
		// Missing [DONE].
		{csChunk(`{"content":"hi"}`, ""), csChunk(`{}`, "stop"), csUsageChunk(usage)},
		// Usage before finish.
		{csChunk(`{"content":"hi"}`, ""), csUsageChunk(usage), csChunk(`{}`, "stop"), csUsageChunk(usage), "[DONE]"},
		// Bad equation with gateway present.
		{csChunk(`{}`, "stop"), csUsageChunk(`{"prompt_tokens":36,"completion_tokens":18,"total_tokens":55,"cost":0}`), "[DONE]"},
		// Nonzero media with gateway present.
		{csChunk(`{}`, "stop"), csUsageChunk(`{"prompt_tokens":36,"completion_tokens":18,"total_tokens":54,"prompt_tokens_details":{"audio_tokens":1},"cost":0}`), "[DONE]"},
	}
	for i, datas := range truncatedSets {
		dec := NewChatStreamDecoderWithPolicy(DefaultLimits(), true)
		failed := false
		for _, d := range datas {
			if _, rerr := dec.DecodeFrame(Frame{Data: d}); rerr != nil {
				failed = true
				break
			}
		}
		if !failed {
			if _, rerr := dec.Close(); rerr == nil {
				t.Fatalf("truncated set %d must fail (never fabricate [DONE]/finish): %q", i, datas)
			} else {
				failed = true
			}
		}
		if !failed {
			t.Fatalf("truncated set %d accepted: %q", i, datas)
		}
	}
	// Events after [DONE] still fail in lossy mode.
	dec := NewChatStreamDecoderWithPolicy(DefaultLimits(), true)
	for _, d := range []string{csChunk(`{"content":"hi"}`, ""), csChunk(`{}`, "stop"), csUsageChunk(usage), "[DONE]"} {
		if _, rerr := dec.DecodeFrame(Frame{Data: d}); rerr != nil {
			t.Fatalf("setup frame %q: %v", d, rerr)
		}
	}
	if _, rerr := dec.DecodeFrame(Frame{Data: csChunk(`{"content":"late"}`, "")}); rerr == nil {
		t.Fatal("frame after [DONE] accepted in lossy mode")
	}
}

// --- Anthropic-target lossy projection of Chat reasoning -------------------

func TestChatAnthropicReasoningProjectionComplete(t *testing.T) {
	mk := func(reasoning int) string {
		return `{"id":"chatcmpl-9","object":"chat.completion","created":1,"model":"up",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":10,"completion_tokens":8,"total_tokens":18,` +
			`"completion_tokens_details":{"reasoning_tokens":` + itoa(reasoning) + `}}}`
	}
	// Strict: Chat reasoning refuses in Anthropic.
	respStrict, rerr := DecodeChatResponse([]byte(mk(3)), DefaultLimits())
	if rerr != nil {
		t.Fatalf("strict decode: %v", rerr)
	}
	if _, rerr := EncodeAnthropicResponse(respStrict, "public"); rerr == nil {
		t.Fatal("strict Chat reasoning must refuse in Anthropic")
	}

	// Lossy: projects (drops breakdown), preserves aggregate output_tokens.
	resp, rerr := DecodeChatResponseWithPolicy([]byte(mk(3)), DefaultLimits(), true)
	if rerr != nil {
		t.Fatalf("lossy decode: %v", rerr)
	}
	out, rerr := EncodeAnthropicResponseWithPolicy(resp, "public", true)
	if rerr != nil {
		t.Fatalf("lossy Anthropic projection must succeed: %v", rerr)
	}
	var v map[string]any
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	usage := v["usage"].(map[string]any)
	// Aggregate preserved: output_tokens stays completion (8), not 8-3.
	if usage["output_tokens"] != float64(8) {
		t.Fatalf("aggregate output_tokens not preserved: %v", usage)
	}
	if _, ok := usage["reasoning_tokens"]; ok {
		t.Fatalf("reasoning breakdown must not appear in Anthropic: %v", usage)
	}
	for _, k := range []string{"reasoning", "thoughtsTokenCount", "opencode", "cost"} {
		if strings.Contains(string(out), k) {
			t.Fatalf("Anthropic output must not carry %q: %s", k, out)
		}
	}
	// Wrapper with same flag also projects.
	out2, rerr := TranslateChatJSONToAnthropicWithPolicy([]byte(mk(3)), "public", DefaultLimits(), true)
	if rerr != nil {
		t.Fatalf("TranslateChatJSONToAnthropicWithPolicy lossy: %v", rerr)
	}
	var v2 map[string]any
	if err := json.Unmarshal(out2, &v2); err != nil {
		t.Fatalf("wrapper output not JSON: %v", err)
	}
	if v2["usage"].(map[string]any)["output_tokens"] != float64(8) {
		t.Fatalf("wrapper aggregate wrong: %v", v2["usage"])
	}
	// Wrapper strict still refuses.
	if _, rerr := TranslateChatJSONToAnthropicWithPolicy([]byte(mk(3)), "public", DefaultLimits(), false); rerr == nil {
		t.Fatal("wrapper strict must refuse Chat reasoning in Anthropic")
	}
}

func TestChatAnthropicReasoningProjectionUnchanged(t *testing.T) {
	// Zero reasoning is representable everywhere, strict and lossy.
	zeroRaw := `{"id":"c","object":"chat.completion","created":1,"model":"up",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7,` +
		`"completion_tokens_details":{"reasoning_tokens":0}}}`
	resp, rerr := DecodeChatResponse([]byte(zeroRaw), DefaultLimits())
	if rerr != nil {
		t.Fatalf("zero decode: %v", rerr)
	}
	if _, rerr := EncodeAnthropicResponse(resp, "public"); rerr != nil {
		t.Fatalf("zero reasoning must encode strict: %v", rerr)
	}
	if _, rerr := EncodeAnthropicResponseWithPolicy(resp, "public", true); rerr != nil {
		t.Fatalf("zero reasoning must encode lossy: %v", rerr)
	}

	// Responses-source reasoning still refuses in Anthropic even in lossy.
	respResp := respWithUsage(t, DialectResponses, usageWith(t, DialectResponses, 5, 7, func(u ResponseUsage) (ResponseUsage, *RouterError) {
		three := 3
		return u.WithReasoning(&three)
	}))
	if _, rerr := EncodeAnthropicResponseWithPolicy(respResp, "public", true); rerr == nil {
		t.Fatal("Responses reasoning must still refuse in Anthropic even lossy")
	}
	// Gemini-source thoughts still refuse outside Gemini even in lossy.
	thoughts := 2
	gemThought := respWithUsage(t, DialectGemini, usageWith(t, DialectGemini, 5, 7, func(u ResponseUsage) (ResponseUsage, *RouterError) {
		return u.WithThoughts(&thoughts)
	}))
	if _, rerr := EncodeAnthropicResponseWithPolicy(gemThought, "public", true); rerr == nil {
		t.Fatal("Gemini thoughts must still refuse in Anthropic even lossy")
	}

	// Chat reasoning still travels to Responses/Gemini in lossy (not dropped).
	chatReasoned := `{"id":"c","object":"chat.completion","created":1,"model":"up",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":10,"completion_tokens":8,"total_tokens":18,` +
		`"completion_tokens_details":{"reasoning_tokens":3}}}`
	respChat, rerr := DecodeChatResponseWithPolicy([]byte(chatReasoned), DefaultLimits(), true)
	if rerr != nil {
		t.Fatalf("chat decode lossy: %v", rerr)
	}
	if _, rerr := EncodeResponsesResponse(respChat, "public"); rerr != nil {
		t.Fatalf("Chat reasoning must still carry in Responses: %v", rerr)
	}
	if _, rerr := EncodeGeminiResponse(respChat, "public"); rerr != nil {
		t.Fatalf("Chat reasoning must still carry in Gemini: %v", rerr)
	}
	// Encoded Responses still carries the breakdown; Gemini still thoughts.
	rout, _ := EncodeResponsesResponse(respChat, "public")
	if !strings.Contains(string(rout), `"reasoning_tokens":3`) {
		t.Fatalf("Responses must preserve reasoning breakdown: %s", rout)
	}
	gout, _ := EncodeGeminiResponse(respChat, "public")
	if !strings.Contains(string(gout), `"thoughtsTokenCount":3`) {
		t.Fatalf("Gemini must preserve thoughts: %s", gout)
	}
}

func TestChatAnthropicReasoningProjectionStream(t *testing.T) {
	usage := `{"prompt_tokens":10,"completion_tokens":8,"total_tokens":18,"completion_tokens_details":{"reasoning_tokens":3}}`
	mkUpstream := func() string {
		return chatChunk(`{"content":"hi"}`, "", "") +
			chatChunk(`{}`, "stop", "") + chatUsageChunk(usage) + "data: [DONE]\n\n"
	}
	// Strict Chat->Anthropic stream refuses reasoning (no message_stop).
	{
		var out strings.Builder
		tr := &ChatToAnthropicSSETranslator{PublicModel: "p", Limits: DefaultLimits()}
		if rerr := tr.Translate(strings.NewReader(mkUpstream()), &out, nil); rerr == nil || rerr.Status != 502 {
			t.Fatalf("strict stream reasoning must 502, got %v", rerr)
		} else if strings.Contains(out.String(), "message_stop") {
			t.Fatalf("strict stream must emit no success terminal:\n%s", out.String())
		}
	}
	// Lossy projects: message_stop with aggregate output_tokens.
	{
		var out strings.Builder
		tr := &ChatToAnthropicSSETranslator{PublicModel: "p", Limits: DefaultLimits(), AllowPrivateReasoningDrop: true}
		if rerr := tr.Translate(strings.NewReader(mkUpstream()), &out, nil); rerr != nil {
			t.Fatalf("lossy stream projection: %v", rerr)
		}
		s := out.String()
		if !strings.Contains(s, "message_stop") {
			t.Fatalf("lossy stream missing message_stop:\n%s", s)
		}
		if !strings.Contains(s, `"output_tokens":8`) {
			t.Fatalf("lossy stream must preserve aggregate output_tokens=8:\n%s", s)
		}
		if strings.Contains(s, "reasoning_tokens") || strings.Contains(s, "thoughtsTokenCount") {
			t.Fatalf("lossy Anthropic stream must not carry reasoning breakdown:\n%s", s)
		}
	}
	// Lossy stream with DeepSeek gateway + reasoning projects both.
	{
		gw := deepSeekGatewayUsage()
		upstream := chatChunk(`{"content":"hi"}`, "", "") +
			chatChunk(`{}`, "stop", "") + chatUsageChunk(gw) + "data: [DONE]\n\n"
		var out strings.Builder
		tr := &ChatToAnthropicSSETranslator{PublicModel: "p", Limits: DefaultLimits(), AllowPrivateReasoningDrop: true}
		if rerr := tr.Translate(strings.NewReader(upstream), &out, nil); rerr != nil {
			t.Fatalf("lossy gateway+reasoning stream: %v", rerr)
		}
		s := out.String()
		if !strings.Contains(s, `"output_tokens":18`) {
			t.Fatalf("gateway stream aggregate wrong:\n%s", s)
		}
		for _, secret := range []string{"opencode", "billing_source", "cache_write_5m"} {
			if strings.Contains(s, secret) {
				t.Fatalf("stream leaks gateway %q:\n%s", secret, s)
			}
		}
	}
}
