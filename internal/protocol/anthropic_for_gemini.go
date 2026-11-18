// Anthropic ingress for the Gemini upstream: destination-specific decode
// of a Messages API request into the shared semantic IR.
//
// This is a thin destination policy over the single private Anthropic
// source validator in anthropic_decode.go. Source validation (message
// shapes, history identities, numeric literals, schema fidelity,
// thinking, tool_choice) is shared; only the Gemini-destination loss plan
// is specific:
//
//   - max_tokens: required by the Anthropic contract; a positive integer
//     maps exactly onto generationConfig.maxOutputTokens.
//   - temperature/top_p: the Anthropic 0..1 range maps exactly onto the
//     Gemini generationConfig range; valid source values are exact, never
//     clamped or diagnosed.
//   - stop_sequences: at most 5 entries map exactly onto
//     generationConfig.stopSequences
//     (https://ai.google.dev/api/generate-content); more than 5 has no
//     exact mapping and is diagnosed as a loss that drops the field,
//     never silently truncated. Malformed entries are 400 in both modes.
//   - tool strict: a Gemini FunctionDeclaration carries no strict schema
//     guarantee. Omitted or explicit false is exact (no guarantee
//     requested, none carried); explicit true is a diagnosed loss, never
//     silently weakened.
//   - parallel disable (tool_choice.disable_parallel_tool_use=true):
//     Gemini generations allow parallel calls by default with no disable
//     control, so an explicit disable is a diagnosed loss.
//   - top_k: no reviewed bridge mapping exists. The shared IR ExtraTopK
//     channel exists for the Chat bridge, but EncodeGeminiRequest honors
//     no topK; forwarding an Anthropic-native control onto a foreign topK
//     would be capability inference, so a well-formed value is a
//     diagnosed loss.
//   - thinking: explicit disabled is a diagnosed loss; enabled with a
//     positive budget decodes to the closed Budget intent for the shared
//     planner (exact generationConfig.thinkingConfig.thinkingBudget only
//     on the pinned canonical native thinkingBudget fact, otherwise a
//     diagnosed drop); adaptive/between_tools require a same-protocol
//     route.
//
// Null policy mirrors the sibling decoders: explicit null on an optional
// field means absent, except for `stream` (explicit bool) and nested bool
// controls (null is a type error). Malformed known fields and unknown
// nested members are validation errors in both modes, before any upstream
// I/O.
//
// Reviewed contracts:
//   - Anthropic Messages (https://docs.anthropic.com/en/api/messages)
//   - GenerateContentRequest generationConfig/tools/toolConfig,
//     FunctionDeclaration parametersJsonSchema:
//     https://ai.google.dev/api/generate-content
package protocol

// DecodeAnthropicForGemini decodes raw JSON Anthropic Messages request
// bytes into the IR plus the Gemini-destination loss plan. It performs no
// I/O. The caller renders the IR once via EncodeGeminiRequest; the public
// model and the streaming method live in the HTTP URL and are never
// emitted in the body.
func DecodeAnthropicForGemini(raw []byte, lim Limits) (*Generation, []Loss, *RouterError) {
	return decodeAnthropicSource(raw, lim, anthropicDestGemini)
}
