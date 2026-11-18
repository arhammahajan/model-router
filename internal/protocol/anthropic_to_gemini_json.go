// Anthropic -> Gemini response translation: complete JSON responses. The
// streaming paths live in anthropic_to_gemini_stream.go (SSE) and
// anthropic_to_gemini_stream_array.go (JSON array).
//
// The guiding rule for unrepresentable upstream features (thinking and
// signature blocks, server tools and their results, citations, media,
// refusal and pause stops, unknown stop reasons, TTL cache breakdowns,
// server-tool billing and other unmapped usage members) is: fail the
// translation with HTTP 502, in both strict and lossy modes. A truncated
// or contradictory response never produces a success marker. Errors are
// value-free: they name field paths, never content or secrets.
//
// Event/state contract (grounded in the primary sources below):
//
//	Upstream (Anthropic Message): a typed message envelope (id, type
//	"message", role "assistant", model) with ordered content blocks
//	(text, tool_use), a stop_reason, an optional stop_sequence echo, and
//	a usage object. Input accounting is the summation of input_tokens,
//	cache_creation_input_tokens and cache_read_input_tokens.
//	Downstream (Gemini): one GenerateContentResponse with a single
//	candidate (index 0) carrying the ordered parts, a mapped finishReason,
//	modelVersion stamped from the public model, the upstream id as
//	responseId, and usageMetadata.
//	Usage conservation: promptTokenCount is input plus cache creation
//	plus cache read (Gemini promptTokenCount still includes cached
//	content and the API publishes no cache-write bucket, so creation
//	tokens aggregate into the prompt and only the read share is reported
//	separately); candidatesTokenCount is output_tokens; totalTokenCount is
//	the precise checked sum. No write-bucket billing is invented and no
//	unmapped member is silently dropped.
//	Stop mapping: end_turn, stop_sequence and tool_use map to STOP
//	(STOP covers the natural stop point and provided stop sequences);
//	max_tokens maps to MAX_TOKENS. pause_turn (a continuation contract),
//	refusal (never a normal STOP) and model_context_window_exceeded have
//	no honest mapping and fail closed, as does any unknown reason.
//
// Source contracts (verified November 2026):
//
//	GenerateContentResponse/Candidate/usageMetadata (promptTokenCount still
//	includes cached content; totalTokenCount covers prompt plus candidates):
//	https://ai.google.dev/api/generate-content
//	Anthropic Messages response (stop reasons, usage summation,
//	stop_sequence echo):
//	https://docs.anthropic.com/en/api/messages
package protocol

// TranslateAnthropicJSONToGemini converts one Anthropic Messages response
// into a GenerateContentResponse for the public model name. Compatibility
// wrapper selecting (Anthropic source decoder, Gemini target encoder); it
// retains no pair semantics of its own. Strict: gateway billing metadata
// fails closed.
func TranslateAnthropicJSONToGemini(raw []byte, publicModel string, lim Limits) ([]byte, *RouterError) {
	return TranslateAnthropicJSONToGeminiWithPolicy(raw, publicModel, lim, false)
}

// TranslateAnthropicJSONToGeminiWithPolicy converts one Anthropic response
// with narrow gateway-billing policy: allowDrop drops validated
// usage.cost/usage.opencode, false keeps the strict 502.
func TranslateAnthropicJSONToGeminiWithPolicy(raw []byte, publicModel string, lim Limits, allowDrop bool) ([]byte, *RouterError) {
	resp, rerr := DecodeAnthropicResponseWithPolicy(raw, lim, allowDrop)
	if rerr != nil {
		return nil, rerr
	}
	return EncodeGeminiResponse(resp, publicModel)
}
