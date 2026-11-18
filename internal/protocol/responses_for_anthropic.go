// Responses ingress for the Anthropic destination: closed, typed validation
// of a Responses API request into the semantic IR with an
// Anthropic-destination loss plan. It performs no I/O and reads no
// environment.
//
// This decoder reuses the shared Responses source decoder in
// responses_decode.go and differs from DecodeResponsesRequest only where the
// Anthropic destination contract differs:
//
//   - temperature: Responses allows 0..2, Anthropic allows 0..1
//     (https://docs.anthropic.com/en/api/messages). A valid source
//     temperature above 1 has no exact mapping: it is diagnosed as a loss
//     and dropped (never silently clamped), so strict policy refuses it.
//   - tool strict: the Responses implicit-strict normalization has no
//     Anthropic equivalent (omitted/null is diagnosed); explicit false is
//     exact; explicit true is refused because Anthropic strict guarantees
//     are not reviewed. Strictness is never silently weakened.
//   - max_tokens: Anthropic requires max_tokens; an absent Responses cap is
//     left absent here and the existing EncodeAnthropicRequest bridge
//     default (4096, see DefaultAnthropicMaxTokens) applies. No
//     model-detection is performed.
//
// Scope: text plus ordinary flat function tools, history and results, and
// forced choice. Anything else follows the shared validation (known shape
// becomes a loss, unknown/malformed is a validation error in both modes).
// Stateless references and background execution are refused before any
// upstream I/O in both modes.
package protocol

// DecodeResponsesForAnthropic decodes raw JSON Responses request bytes into
// the IR plus the Anthropic-destination loss plan. It performs no I/O.
func DecodeResponsesForAnthropic(raw []byte, lim Limits) (*Generation, []Loss, *RouterError) {
	return decodeResponsesSource(raw, lim, responsesDestAnthropic)
}
