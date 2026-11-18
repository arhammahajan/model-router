// Gemini -> Anthropic response translation: complete JSON responses via
// the shared Response IR. Complete-path source validation now lives once
// in DecodeGeminiResponse (response_gemini_decode.go); this file keeps
// only the thin compatibility wrapper.
package protocol

// TranslateGeminiJSONToAnthropic converts one Gemini generateContent
// response into an Anthropic Messages response object. Compatibility
// wrapper selecting (Gemini source decoder, Anthropic target encoder);
// it retains no pair semantics of its own. Strict: gateway/provider
// response extensions fail closed.
func TranslateGeminiJSONToAnthropic(raw []byte, publicModel string, lim Limits) ([]byte, *RouterError) {
	return TranslateGeminiJSONToAnthropicWithPolicy(raw, publicModel, lim, false)
}

// TranslateGeminiJSONToAnthropicWithPolicy converts one Gemini
// generateContent response into an Anthropic Messages response object
// with narrow lossy-extension policy. allowDrop drops validated
// response extensions (thoughtSignature, TEXT-modality usage details,
// usage.cost/usage.opencode, nonzero thoughtsTokenCount via the
// Reasoning fold) and projects the reasoning breakdown for the
// Anthropic target; false keeps the strict 502s. Compatibility wrapper
// selecting (Gemini source decoder policy, Anthropic target encoder
// policy); it retains no pair semantics of its own.
func TranslateGeminiJSONToAnthropicWithPolicy(raw []byte, publicModel string, lim Limits, allowDrop bool) ([]byte, *RouterError) {
	resp, rerr := DecodeGeminiResponseWithPolicy(raw, lim, allowDrop)
	if rerr != nil {
		return nil, rerr
	}
	return EncodeAnthropicResponseWithPolicy(resp, publicModel, allowDrop)
}
