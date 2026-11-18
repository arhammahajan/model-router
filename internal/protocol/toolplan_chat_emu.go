package protocol

// Chat ingress emulated custom restoration to the Chat destination.
//
// Chat freeform customs bound for Anthropic/Gemini are approximated
// upstream as single {"content":string} functions (see
// responsesCustomEmuReason, emulatedCustomSchema and
// emulatedCustomDescription). The returned function calls are restored
// here to the native Chat custom call shape via the shared typed
// ToolPlan stage: order, opaque IDs and exact input are preserved, text
// stays incremental, and ambiguous wrappers, frame limits, truncation
// and writer errors fail closed. No codec is copied and no JSON
// intermediate is introduced: this file only selects the existing
// restore stage plus the Chat target encoder.

// TranslateAnthropicJSONToChatWithPlan decodes one Anthropic response,
// restores emulated Chat customs from the request plan, and renders the
// Chat envelope. A nil/empty plan behaves exactly like
// TranslateAnthropicJSON. Strict: gateway billing metadata fails closed.
func TranslateAnthropicJSONToChatWithPlan(raw []byte, publicModel string, lim Limits, plan *ToolPlan) ([]byte, *RouterError) {
	return TranslateAnthropicJSONToChatWithPlanAndPolicy(raw, publicModel, lim, plan, false)
}

// TranslateAnthropicJSONToChatWithPlanAndPolicy decodes one Anthropic
// response with narrow gateway-billing policy, restores emulated Chat
// customs, and renders the Chat envelope. allowDrop drops validated
// usage.cost/usage.opencode, false keeps the strict 502.
func TranslateAnthropicJSONToChatWithPlanAndPolicy(raw []byte, publicModel string, lim Limits, plan *ToolPlan, allowDrop bool) ([]byte, *RouterError) {
	resp, rerr := DecodeAnthropicResponseWithPolicy(raw, lim, allowDrop)
	if rerr != nil {
		return nil, rerr
	}
	restored, rerr := plan.RestoreResponse(resp, lim)
	if rerr != nil {
		return nil, rerr
	}
	return EncodeChatResponse(restored, publicModel)
}

// TranslateGeminiJSONToChatWithPlan decodes one Gemini response,
// restores emulated Chat customs from the request plan, and renders the
// Chat envelope. A nil/empty plan behaves exactly like
// TranslateGeminiJSONToChat.
func TranslateGeminiJSONToChatWithPlan(raw []byte, publicModel string, lim Limits, plan *ToolPlan) ([]byte, *RouterError) {
	resp, rerr := DecodeGeminiResponse(raw, lim)
	if rerr != nil {
		return nil, rerr
	}
	restored, rerr := plan.RestoreResponse(resp, lim)
	if rerr != nil {
		return nil, rerr
	}
	return EncodeChatResponse(restored, publicModel)
}
