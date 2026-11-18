// Anthropic ingress for the Responses upstream: destination-specific decode
// of a Messages API request into the shared semantic IR.
//
// This is a thin destination policy over the single private Anthropic
// source validator in anthropic_decode.go. Source validation (message
// shapes, history identities, numeric literals, schema fidelity,
// thinking, tool_choice) is shared; only the stop/tool-strict loss plan is
// Responses-specific:
//
//   - stop_sequences: the reviewed Responses scope has no stop-sequence
//     contract, so a well-formed non-empty list is a diagnosed loss and is
//     never forwarded.
//   - tool strict: the Anthropic contract defaults to non-strict
//     (strict:true is opt-in:
//     https://platform.claude.com/docs/en/agents-and-tools/tool-use/strict-tool-use),
//     and EncodeResponsesRequest emits an explicit strict:false for that
//     default, so omitted/null maps exactly with no loss. Explicit
//     true/false are retained exactly. This differs from a Responses source
//     omitted strict, which is legitimately diagnosed when crossing to
//     non-strict destinations.
//
// Do not compose a serialize-then-decode pipeline through
// DecodeAnthropicRequest and do not accept its loss plan here: every
// admitted source field is represented, diagnosed, or rejected through the
// shared validator.
//
// Reviewed contracts:
//   - Anthropic Messages (https://docs.anthropic.com/en/api/messages)
//   - OpenAI Responses create
//     (https://developers.openai.com/api/reference/resources/responses/methods/create)
package protocol

// DecodeAnthropicForResponses decodes raw JSON Anthropic Messages request
// bytes into the IR plus the Responses-destination loss plan. It performs no
// I/O.
func DecodeAnthropicForResponses(raw []byte, lim Limits) (*Generation, []Loss, *RouterError) {
	return decodeAnthropicSource(raw, lim, anthropicDestResponses)
}
