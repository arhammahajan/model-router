// Responses ingress for the Gemini upstream: narrow typed decode of a
// Responses API request into the shared Generation IR with a
// Gemini-destination loss plan. It performs no I/O and reads no environment.
//
// The shared Responses source validator in responses_decode.go performs the
// decode under the Gemini destination policy; only the tool-strict wording
// and the parallel-disable loss below are destination-specific. The
// differences versus DecodeResponsesRequest live in that policy:
//
//   - temperature: Responses allows 0..2 and Gemini generationConfig
//     temperature spans the same [0.0, 2.0] range
//     (https://ai.google.dev/api/generate-content), so a valid source
//     temperature is exact, never clamped or diagnosed.
//   - top_p: Responses allows 0..1 and Gemini topP spans the same unit
//     interval, so a valid source value is exact.
//   - max_output_tokens: a positive integer maps exactly onto
//     generationConfig.maxOutputTokens.
//   - tool strict: a Gemini FunctionDeclaration carries no strict schema
//     guarantee (https://ai.google.dev/api/generate-content). Omitted or
//     null strict is a diagnosed loss (the Responses implicit-strict
//     normalization has no Gemini equivalent, so it is never silently
//     equated); explicit false is exact (no guarantee requested, none
//     carried); explicit true is a diagnosed loss, never silently weakened.
//   - parallel_tool_calls: Gemini generations allow parallel function calls
//     by default with no disable control; explicit false is a diagnosed
//     loss, while true or omitted is exact.
//   - tool_choice: "none"/"auto"/"required" and named function choices map
//     exactly onto functionCallingConfig NONE/AUTO/ANY/specific; an allowed
//     tool set has no reviewed mapping and is diagnosed as a loss via the
//     shared helper.
//
// Null policy mirrors the sibling decoders: explicit null on an optional
// field means absent, except for `stream` (explicit bool) and nested bool
// controls (null is a type error). Malformed known fields and unknown nested
// members are validation errors in both modes, before any upstream I/O.
//
// Source contracts (verified November 2026, primary docs):
//
//	Responses create/streaming (https://developers.openai.com/api/reference/resources/responses/methods/create)
//	GenerateContentRequest contents/systemInstruction/tools/toolConfig/generationConfig,
//	FunctionDeclaration parameters vs parametersJsonSchema mutual exclusivity:
//	https://ai.google.dev/api/generate-content
//	Function calling (optional call/response ids):
//	https://ai.google.dev/gemini-api/docs/function-calling
package protocol

// DecodeResponsesForGemini decodes raw JSON Responses request bytes into
// the IR plus the Gemini-destination loss plan. It performs no I/O. The
// caller renders the IR once via EncodeGeminiRequest; the public model and
// the streaming method live in the HTTP URL and are never emitted in the
// body.
func DecodeResponsesForGemini(raw []byte, lim Limits) (*Generation, []Loss, *RouterError) {
	return decodeResponsesSource(raw, lim, responsesDestGemini)
}
