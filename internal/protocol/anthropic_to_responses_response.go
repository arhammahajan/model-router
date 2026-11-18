// Anthropic upstream -> Responses JSON translation: complete Messages
// responses into the Responses envelope with exact usage accounting.
//
// The guiding rule for unrepresentable upstream features (thinking and
// redacted-thinking blocks, server tools, citations, unknown stop reasons,
// unknown accounting members, private state) is: fail the translation with
// HTTP 502, in both strict and lossy modes. Errors are value-free: they name
// field paths, never content or secrets.
//
// Source contracts:
//   - Messages response and usage accounting
//     (https://docs.anthropic.com/en/api/messages): input_tokens excludes
//     the cache buckets (cache_creation_input_tokens,
//     cache_read_input_tokens); the Responses input total includes them,
//     so Responses input is the exact sum and output is unchanged. The
//     cached share is reported honestly (read -> cached_tokens,
//     creation -> cache_write_tokens per openai-python response_usage.py).
package protocol

// TranslateAnthropicJSONToResponses converts one Anthropic Messages response
// into a Responses API response object for the public model name.
// Compatibility wrapper selecting (Anthropic source decoder, Responses
// target encoder); it retains no pair semantics of its own. Strict:
// gateway billing metadata fails closed.
func TranslateAnthropicJSONToResponses(raw []byte, publicModel string, lim Limits) ([]byte, *RouterError) {
	return TranslateAnthropicJSONToResponsesWithPolicy(raw, publicModel, lim, false)
}

// TranslateAnthropicJSONToResponsesWithPolicy converts one Anthropic
// response with narrow gateway-billing policy: allowDrop drops validated
// usage.cost/usage.opencode, false keeps the strict 502.
func TranslateAnthropicJSONToResponsesWithPolicy(raw []byte, publicModel string, lim Limits, allowDrop bool) ([]byte, *RouterError) {
	resp, rerr := DecodeAnthropicResponseWithPolicy(raw, lim, allowDrop)
	if rerr != nil {
		return nil, rerr
	}
	return EncodeResponsesResponse(resp, publicModel)
}
