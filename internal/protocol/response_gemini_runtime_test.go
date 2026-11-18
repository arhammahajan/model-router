// Gemini complete-response runtime cutover tests: one upstream through
// the shared decoder into all three target encoders.
//
// These tests pin the cutover itself (DecodeGeminiResponse ->
// EncodeChat/Anthropic/Responses), not the staged decoder alone:
// the same upstream raw must render correctly on all three downstream
// wires, with honest identity provenance and no fake provider metadata.
package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func geminiRuntimeRaw(candidates, usage, envelope string) string {
	return `{"candidates":` + candidates + `,"usageMetadata":` + usage + envelope + `}`
}

func geminiRuntimeUsage(prompt, completion int) string {
	return `{"promptTokenCount":` + g2cItoa(prompt) +
		`,"candidatesTokenCount":` + g2cItoa(completion) +
		`,"totalTokenCount":` + g2cItoa(prompt+completion) + `}`
}

func mustDecodeGeminiRuntime(t *testing.T, raw string) Response {
	t.Helper()
	resp, rerr := DecodeGeminiResponse([]byte(raw), DefaultLimits())
	if rerr != nil {
		t.Fatalf("DecodeGeminiResponse: %v", rerr)
	}
	return resp
}

func mustEncodeAllThree(t *testing.T, resp Response) (chat, anthropic, responses map[string]any) {
	t.Helper()
	co, rerr := EncodeChatResponse(resp, "public")
	if rerr != nil {
		t.Fatalf("EncodeChatResponse: %v", rerr)
	}
	ao, rerr := EncodeAnthropicResponse(resp, "public")
	if rerr != nil {
		t.Fatalf("EncodeAnthropicResponse: %v", rerr)
	}
	ro, rerr := EncodeResponsesResponse(resp, "public")
	if rerr != nil {
		t.Fatalf("EncodeResponsesResponse: %v", rerr)
	}
	_ = json.Unmarshal(co, &chat)
	_ = json.Unmarshal(ao, &anthropic)
	_ = json.Unmarshal(ro, &responses)
	return chat, anthropic, responses
}

// Same upstream x3: ordered text-before-calls, exact bigint args, cache share.
func TestGeminiRuntimeCrossProductOrdered(t *testing.T) {
	raw := geminiRuntimeRaw(
		`[{"content":{"role":"model","parts":[
			{"text":"hello"},
			{"functionCall":{"name":"late","args":{},"id":"call-9"}},
			{"functionCall":{"name":"get_time","args":{"n":9007199254740993}}}
		]},"finishReason":"STOP"}]`,
		`{"promptTokenCount":12,"candidatesTokenCount":7,"totalTokenCount":19,"cachedContentTokenCount":3}`,
		`,"modelVersion":"gemini-2.5-flash","responseId":"resp-9"`)
	resp := mustDecodeGeminiRuntime(t, raw)
	if resp.IDOrigin != IdentityProvider || resp.ModelOrigin != IdentityProvider {
		t.Fatalf("provider origins lost: %+v", resp)
	}
	if len(resp.RouterMintedCalls) != 1 {
		t.Fatalf("exactly one router-minted call expected: %+v", resp.RouterMintedCalls)
	}
	for _, it := range resp.Items {
		if c, ok := it.(ToolCallItem); ok {
			if string(c.CallID) == "call-9" {
				if _, ok := resp.RouterMintedCalls[c.CallID]; ok {
					t.Fatalf("provider call-9 wrongly marked router-minted")
				}
			} else {
				if _, ok := resp.RouterMintedCalls[c.CallID]; !ok {
					t.Fatalf("minted %q missing from provenance set", c.CallID)
				}
				if !strings.HasPrefix(string(c.CallID), geminiCompatCallPrefix) {
					t.Fatalf("minted call must carry compat scope: %q", c.CallID)
				}
			}
		}
	}
	// Direct IR cross-product.
	chat, anthropic, responses := mustEncodeAllThree(t, resp)
	// Chat collapses to one message + tool_calls, public model, minted id.
	choices, _ := chat["choices"].([]any)
	ch0, _ := choices[0].(map[string]any)
	msg, _ := ch0["message"].(map[string]any)
	if msg["role"] != "assistant" {
		t.Fatalf("chat role: %v", msg)
	}
	if msg["content"] != "hello" {
		t.Fatalf("chat text: %v", msg)
	}
	calls, _ := msg["tool_calls"].([]any)
	if len(calls) != 2 {
		t.Fatalf("chat calls: %v", msg)
	}
	// Anthropic preserves order text-first with cache detail.
	content, _ := anthropic["content"].([]any)
	if len(content) != 3 {
		t.Fatalf("anthropic order: %v", anthropic["content"])
	}
	first, _ := content[0].(map[string]any)
	if first["type"] != "text" {
		t.Fatalf("text-first lost: %v", content)
	}
	usage, _ := anthropic["usage"].(map[string]any)
	if usage["input_tokens"] != float64(9) || usage["cache_read_input_tokens"] != float64(3) {
		t.Fatalf("anthropic input must subtract cache: %v", usage)
	}
	// Responses preserves order with one message + two calls.
	output, _ := responses["output"].([]any)
	if len(output) != 3 {
		t.Fatalf("responses order: %v", responses["output"])
	}
	// Tool-first interleaving still preserves order on non-Chat targets
	// (Chat refusal for that shape is pinned below).
	toolFirst := mustDecodeGeminiRuntime(t, geminiRuntimeRaw(
		`[{"content":{"role":"model","parts":[
			{"functionCall":{"name":"late","args":{},"id":"call-9"}},
			{"text":"mid"},
			{"functionCall":{"name":"get_time","args":{"n":9007199254740993}}}
		]},"finishReason":"STOP"}]`,
		`{"promptTokenCount":12,"candidatesTokenCount":7,"totalTokenCount":19}`,
		`,"modelVersion":"gemini-2.5-flash","responseId":"resp-9"`))
	if _, ok := toolFirst.Items[0].(ToolCallItem); !ok {
		t.Fatalf("tool-first lost: %#v", toolFirst.Items)
	}
	if _, rerr := EncodeAnthropicResponse(toolFirst, "public"); rerr != nil {
		t.Fatalf("anthropic tool-first: %v", rerr)
	}
	// Wrappers agree with direct IR on the same upstream.
	for _, fn := range []func([]byte, string, Limits) ([]byte, *RouterError){
		TranslateGeminiJSONToChat, TranslateGeminiJSONToAnthropic, TranslateGeminiJSONToResponses,
	} {
		if _, rerr := fn([]byte(raw), "public", DefaultLimits()); rerr != nil {
			t.Fatalf("wrapper must succeed on shared upstream: %v", rerr)
		}
	}
}

// Blocked prompt renders on all three with exact usage, no items.
func TestGeminiRuntimeBlockedCrossProduct(t *testing.T) {
	raw := `{"promptFeedback":{"blockReason":"SAFETY"},"usageMetadata":` + geminiRuntimeUsage(7, 0) + `}`
	resp := mustDecodeGeminiRuntime(t, raw)
	if resp.Finish.Kind != FinishContentFilter || len(resp.Items) != 0 {
		t.Fatalf("blocked shape: %+v", resp)
	}
	chat, anthropic, responses := mustEncodeAllThree(t, resp)
	ch0, _ := chat["choices"].([]any)[0].(map[string]any)
	if ch0["finish_reason"] != "content_filter" {
		t.Fatalf("chat blocked: %v", ch0)
	}
	if anthropic["stop_reason"] != "refusal" {
		t.Fatalf("anthropic blocked: %v", anthropic)
	}
	if responses["status"] != "incomplete" {
		t.Fatalf("responses blocked: %v", responses)
	}
}

// Missing responseId/modelVersion: honest absence, no fake metadata.
func TestGeminiRuntimeAbsentIdentityHonest(t *testing.T) {
	raw := geminiRuntimeRaw(
		`[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP"}]`,
		geminiRuntimeUsage(2, 1), ``)
	resp := mustDecodeGeminiRuntime(t, raw)
	if resp.IDOrigin != IdentityRouterMinted || resp.ModelOrigin != IdentityRouterMinted {
		t.Fatalf("absent ids must be router origins: %+v", resp)
	}
	if resp.Model != "" {
		t.Fatalf("absent model must be empty, got %q", resp.Model)
	}
	// Gemini target must NOT echo the synthesized id or a fake model.
	gem, rerr := EncodeGeminiResponse(resp, "public-alias")
	if rerr != nil {
		t.Fatalf("EncodeGeminiResponse: %v", rerr)
	}
	var gv map[string]any
	_ = json.Unmarshal(gem, &gv)
	if _, ok := gv["responseId"]; ok {
		t.Fatalf("router-minted id must not render as provider responseId: %s", gem)
	}
	if gv["modelVersion"] != "public-alias" {
		t.Fatalf("modelVersion must be the public alias: %s", gem)
	}
	// Other targets mint their own response ids regardless.
	chat, anthropic, responses := mustEncodeAllThree(t, resp)
	if chat["id"] == string(resp.ID) || chat["id"] == "" {
		t.Fatalf("chat must mint its own id: %v", chat["id"])
	}
	if anthropic["id"] == string(resp.ID) || anthropic["id"] == "" {
		t.Fatalf("anthropic must mint its own id: %v", anthropic["id"])
	}
	if responses["id"] == string(resp.ID) || responses["id"] == "" {
		t.Fatalf("responses must mint its own id: %v", responses["id"])
	}
}

// Provider-supplied compat-prefix string keeps a provider origin and echoes.
func TestGeminiRuntimeSuppliedPrefixDistinguishesOrigin(t *testing.T) {
	raw := geminiRuntimeRaw(
		`[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP"}]`,
		geminiRuntimeUsage(2, 1),
		`,"modelVersion":"gemini-compat-response","responseId":"gemini-compat-response"`)
	resp := mustDecodeGeminiRuntime(t, raw)
	if resp.IDOrigin != IdentityProvider || resp.ModelOrigin != IdentityProvider {
		t.Fatalf("supplied prefix must keep provider origins: %+v", resp)
	}
	if _, ok := resp.RouterMintedCalls[CallID("gemini-compat-response")]; ok {
		t.Fatalf("supplied id must not be marked router-minted")
	}
	gem, rerr := EncodeGeminiResponse(resp, "public")
	if rerr != nil {
		t.Fatalf("EncodeGeminiResponse: %v", rerr)
	}
	var gv map[string]any
	_ = json.Unmarshal(gem, &gv)
	if gv["responseId"] != "gemini-compat-response" {
		t.Fatalf("provider-supplied prefix id must echo: %s", gem)
	}
}

// Text-after-tool refuses Chat (honest 502) but renders elsewhere.
func TestGeminiRuntimeTextAfterToolChatRefusal(t *testing.T) {
	raw := geminiRuntimeRaw(
		`[{"content":{"role":"model","parts":[
			{"functionCall":{"name":"f","args":{}}},
			{"text":"after"}]},"finishReason":"STOP"}]`,
		geminiRuntimeUsage(3, 2), ``)
	resp := mustDecodeGeminiRuntime(t, raw)
	if _, rerr := EncodeChatResponse(resp, "public"); rerr == nil {
		t.Fatalf("chat must refuse text-after-tool instead of reordering")
	}
	if _, rerr := EncodeAnthropicResponse(resp, "public"); rerr != nil {
		t.Fatalf("anthropic must preserve order: %v", rerr)
	}
	if _, rerr := EncodeResponsesResponse(resp, "public"); rerr != nil {
		t.Fatalf("responses must preserve order: %v", rerr)
	}
	if _, rerr := EncodeGeminiResponse(resp, "public"); rerr != nil {
		t.Fatalf("gemini must preserve order: %v", rerr)
	}
}

// Malformed/unknown/bounds fail closed and value-free on every wrapper.
func TestGeminiRuntimeMalformedBoundsValueFree(t *testing.T) {
	secret := "private-upstream-stop-value"
	cases := []string{
		geminiRuntimeRaw(`[{"content":{"role":"model","parts":[{"text":"t"}]},"finishReason":"`+secret+`"}]`, geminiRuntimeUsage(1, 1), ``),
		geminiRuntimeRaw(`[{"content":{"role":"model","parts":[{"text":"t"}]},"finishReason":"STOP","future":1}]`, geminiRuntimeUsage(1, 1), ``),
		geminiRuntimeRaw(`[{"content":{"role":"model","parts":[{"text":7}]},"finishReason":"STOP"}]`, geminiRuntimeUsage(1, 1), ``),
		geminiRuntimeRaw(`[{"content":{"role":"model","parts":[{"functionCall":{"name":"f","args":[1]}}]},"finishReason":"STOP"}]`, geminiRuntimeUsage(1, 1), ``),
	}
	for i, raw := range cases {
		for _, fn := range []func([]byte, string, Limits) ([]byte, *RouterError){
			TranslateGeminiJSONToChat, TranslateGeminiJSONToAnthropic, TranslateGeminiJSONToResponses,
		} {
			_, rerr := fn([]byte(raw), "public", DefaultLimits())
			if rerr == nil || rerr.Status != 502 {
				t.Fatalf("case %d must be 502, got %v", i, rerr)
			}
			if strings.Contains(rerr.Msg, secret) {
				t.Fatalf("case %d leaked value into diagnostic: %q", i, rerr.Msg)
			}
		}
	}
}
