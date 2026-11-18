// Gemini ingress for the Anthropic upstream: destination-specific decode
// of a GenerateContentRequest body into the shared semantic IR.
//
// Boundary sketch (Gemini -> Anthropic bridge):
//
//	ingress:  Gemini GenerateContentRequest --DecodeGeminiForAnthropic--> IR
//	         --EncodeAnthropicRequest--> Anthropic upstream request.
//	         Never composed via Chat JSON or an Anthropic translator.
//	egress:   Anthropic upstream response --TranslateAnthropicJSONToGemini-->
//	         Gemini GenerateContentResponse (or the SSE / JSON-array
//	         translators in anthropic_to_gemini_stream*.go).
//
// This file holds the Anthropic destination entry point. The shared Gemini
// source validator (gemini_decode*.go) performs the decode under the
// Anthropic destination policy; only the small policy below is
// Anthropic-specific:
//
//   - temperature: Gemini allows 0..2 and Anthropic allows 0..1
//     (https://docs.anthropic.com/en/api/messages). A valid source
//     temperature above 1 has no exact mapping: it is diagnosed as a loss
//     and dropped, never silently clamped.
//   - top_p: both sides span the unit interval, so a valid source value is
//     exact.
//   - topK: a validated non-negative integer maps exactly onto the
//     existing IR ExtraTopK path, which EncodeAnthropicRequest renders as
//     Anthropic top_k. Neither REST reference publishes a tighter numeric
//     bound; the value is forwarded exactly.
//   - maxOutputTokens: a positive integer maps exactly onto
//     Anthropic max_tokens. An absent cap is left absent and the existing
//     EncodeAnthropicRequest bridge default (4096, see
//     DefaultAnthropicMaxTokens) applies. No model-detection is performed.
//   - stopSequences: the official GenerateContentRequest contract caps the
//     list at 5 entries (rejected as invalid_request above the cap in
//     every destination, before any loss plan). Within the cap the list
//     maps exactly onto Anthropic stop_sequences; an empty list is an
//     exact no-op.
//   - function strict: a Gemini FunctionDeclaration carries no strict
//     guarantee and the Anthropic encoder emits no strict member, so
//     nothing is forwarded and no loss is invented.
//   - toolConfig: NONE/AUTO/ANY map exactly onto none/auto/any; a single
//     allowedFunctionNames entry under ANY maps onto a specific choice; a
//     wider allowed set narrows the tool list (approximated loss, via the
//     shared validator); VALIDATED mode uses automatic calling
//     (approximated loss, via the shared validator).
//   - seed/penalties/response format/thinking/safety/media/compound
//     controls: validated for shape by the shared validator (malformed in
//     both modes), then diagnosed as losses with Anthropic wording; the
//     remaining unbridged members are refused specifically.
//
// Null policy mirrors the sibling decoders: explicit null on an optional
// field means absent, except for nested boolean controls, where null is a
// type error. Malformed known fields and unknown nested members are
// validation errors in both strict and lossy modes, before upstream I/O.
//
// Source contracts (verified November 2026, primary docs):
//
//	GenerateContentRequest (model/method live in the URL, never the
//	body; stopSequences up to 5; temperature 0..2; topK integer):
//	https://ai.google.dev/api/generate-content
//	Anthropic Messages create (temperature 0..1, top_p, top_k,
//	stop_sequences, required max_tokens):
//	https://docs.anthropic.com/en/api/messages
package protocol

// DecodeGeminiForAnthropic decodes raw JSON Gemini GenerateContentRequest
// bytes into the IR plus the Anthropic-destination loss plan. publicModel
// is the model from the parsed request URL and stream is the
// method-derived mode (streamGenerateContent). It performs no I/O.
//
// The shared Gemini source validator performs the decode under this
// destination's policy; every admitted source field is represented,
// diagnosed, or rejected through the shared validator. The caller renders
// the IR once via EncodeAnthropicRequest.
func DecodeGeminiForAnthropic(raw []byte, publicModel string, stream bool, lim Limits) (*Generation, []Loss, *RouterError) {
	return decodeGeminiRequest(raw, publicModel, stream, lim, geminiDestAnthropic)
}
