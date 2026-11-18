// Responses -> Anthropic JSON response translation: complete JSON
// responses decode once via DecodeResponsesResponse and render via
// EncodeAnthropicResponse. The streaming path is
// ResponsesToAnthropicSSETranslator in
// responses_to_anthropic_stream_state.go (shared pipeline).
package protocol

// TranslateResponsesJSONToAnthropic converts one Responses response into an
// Anthropic Messages response object for the public model name.
//
// Compatibility wrapper selecting (Responses source decoder,
// Anthropic target encoder); it retains no pair semantics of its own.
//
// Strict default: provider-private reasoning OUTPUT items fail closed.
// Use TranslateResponsesJSONToAnthropicWithPolicy for the lossy drop.
func TranslateResponsesJSONToAnthropic(raw []byte, publicModel string, lim Limits) ([]byte, *RouterError) {
	resp, rerr := DecodeResponsesResponse(raw, lim)
	if rerr != nil {
		return nil, rerr
	}
	return EncodeAnthropicResponse(resp, publicModel)
}

// TranslateResponsesJSONToAnthropicWithPolicy converts one Responses
// response with narrow private-reasoning policy: allowPrivateDrop drops
// the measured reasoning shape, false keeps the strict 502.
func TranslateResponsesJSONToAnthropicWithPolicy(raw []byte, publicModel string, lim Limits, allowPrivateDrop bool) ([]byte, *RouterError) {
	resp, rerr := DecodeResponsesResponseWithPolicy(raw, lim, allowPrivateDrop)
	if rerr != nil {
		return nil, rerr
	}
	return EncodeAnthropicResponseWithPolicy(resp, publicModel, allowPrivateDrop)
}
