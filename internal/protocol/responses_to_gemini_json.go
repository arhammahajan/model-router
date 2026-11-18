// Responses -> Gemini response translation: complete JSON responses decode
// once via DecodeResponsesResponse and render via EncodeGeminiResponse. The
// streaming paths live in responses_to_gemini_stream.go (SSE) and
// responses_to_gemini_stream_array.go (incremental JSON array).
//
// It is never composed via Chat JSON or a Chat translator.
//
// Guiding rule for unrepresentable upstream features (refusal items,
// audio/image/service tools, annotations, logprobs, unknown usage
// members, unknown stop reasons, unknown item types) is: fail the
// translation with HTTP 502, in both strict and lossy modes. A truncated
// or contradictory response never produces a success marker. Errors are
// value-free: they name field paths, never content or secrets.
//
// Call identity: the Responses call_id is the stable call identity and
// is preserved exactly as the Gemini functionCall id (opaque provider
// handle, never constrained by the tool-name grammar). The Responses
// item id (fc_...) is response-item identity, validated for conflicts
// but never used as the call identity.
//
// Usage contract (established, not silently dropped): per the Responses
// contract input_tokens already includes cached tokens and per the
// Gemini REST contract promptTokenCount still includes the cached share
// (cachedContentTokenCount), while thoughtsTokenCount is the reasoning
// share of candidates. total_tokens must equal input+output exactly with
// overflow checks; cached is a subset of input and reasoning a subset of
// output. Any other nonzero accounting detail has no Gemini
// representation and fails closed.
//
// Source contracts (primary references):
//
//	GenerateContentResponse/Candidate/usageMetadata:
//	https://ai.google.dev/api/generate-content
//	Responses create/streaming (response object, output items, usage):
//	https://developers.openai.com/api/reference/resources/responses/methods/create
package protocol

// TranslateResponsesJSONToGemini converts one Responses response into a
// Gemini GenerateContentResponse for the public model name. The public
// model is restored as modelVersion and the upstream Responses id as
// responseId; the candidate index is always 0.
//
// Compatibility wrapper selecting (Responses source decoder,
// Gemini target encoder); it retains no pair semantics of its own.
//
// Strict default: provider-private reasoning OUTPUT items fail closed.
// Use TranslateResponsesJSONToGeminiWithPolicy for the lossy drop.
func TranslateResponsesJSONToGemini(raw []byte, publicModel string, lim Limits) ([]byte, *RouterError) {
	resp, rerr := DecodeResponsesResponse(raw, lim)
	if rerr != nil {
		return nil, rerr
	}
	return EncodeGeminiResponse(resp, publicModel)
}

// TranslateResponsesJSONToGeminiWithPolicy converts one Responses
// response with narrow private-reasoning policy: allowPrivateDrop drops
// the measured reasoning shape, false keeps the strict 502.
func TranslateResponsesJSONToGeminiWithPolicy(raw []byte, publicModel string, lim Limits, allowPrivateDrop bool) ([]byte, *RouterError) {
	resp, rerr := DecodeResponsesResponseWithPolicy(raw, lim, allowPrivateDrop)
	if rerr != nil {
		return nil, rerr
	}
	return EncodeGeminiResponse(resp, publicModel)
}
