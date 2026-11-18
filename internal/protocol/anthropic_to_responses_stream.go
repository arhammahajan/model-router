// Anthropic upstream -> Responses stream translation via the shared pipeline.
package protocol

import (
	"io"
)

// --- Streaming ---

// AnthropicToResponsesSSETranslator converts an Anthropic Messages SSE
// stream into Responses API SSE events via the shared pipeline (SSE
// framing, Anthropic source stream decoder, shared lifecycle, Responses
// target event encoder). It retains no parsing state: every Translate
// call builds fresh pipeline objects, so one translator value is reusable
// across generations.
//
// Output indices come from the target encoder in emission order; response,
// message-item and function-call identities are stable across the
// start/delta/done/terminal events of one generation. Translate returns
// nil promptly after a complete message_stop with all blocks closed; EOF
// is not required after the protocol terminal. Any truncation, missing
// usage, unfinished tool identity, invalid tool arguments, duplicate
// preterminal markers, or unrepresentable event is an error with no
// fabricated success.
type AnthropicToResponsesSSETranslator struct {
	PublicModel string
	Limits      Limits
	// Plan restores Responses namespace identities on tool blocks. Nil or
	// empty restores nothing; native streams stay raw.
	Plan *ToolPlan
	// AllowGatewayBillingDrop drops validated gateway billing metadata
	// (usage.cost, usage.opencode) when true (lossy compatibility).
	// False keeps the strict 502.
	AllowGatewayBillingDrop bool
}

// Translate reads Anthropic SSE from r and writes Responses SSE events to
// w, calling flush after each frame. It returns nil promptly after a
// complete message_stop with all blocks closed; EOF is not required after
// the protocol terminal and bytes after the terminal marker belong to the
// next generation and are left unread. Any truncation, missing usage,
// unfinished tool identity, invalid tool arguments, duplicate preterminal
// markers, or unrepresentable event is an error. No second error is
// invented after a successful completion.
func (t *AnthropicToResponsesSSETranslator) Translate(r io.Reader, w io.Writer, flush func()) *RouterError {
	frames := NewSSEFrameReader(r, t.Limits)
	var dec StreamDecoder = NewAnthropicStreamDecoderWithPolicy(t.Limits, t.AllowGatewayBillingDrop)
	if t.Plan != nil {
		dec = t.Plan.WrapDecoder(dec, t.Limits)
	}
	enc := NewResponsesEventEncoder(t.PublicModel, t.Limits)
	return TranslateStream(frames, dec, enc, w, flush, t.Limits, DialectAnthropic)
}
