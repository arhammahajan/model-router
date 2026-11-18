// Responses -> Anthropic streaming via the shared pipeline.
package protocol

import (
	"io"
)

// ResponsesToAnthropicSSETranslator converts a Responses SSE stream into
// Anthropic Messages SSE events via the shared pipeline (SSE framing,
// Responses source stream decoder, shared lifecycle, Anthropic target event
// encoder). It retains no parsing state: every Translate call builds fresh
// pipeline objects, so one translator value is reusable across generations.
//
// The pipeline stops reading upstream as soon as the source terminal is
// observed: Translate returns promptly after response.completed/incomplete
// with all blocks closed and terminal usage validated. Truncation, missing
// usage, unfinished tool identity, invalid tool arguments, duplicate
// preterminal markers, or unrepresentable events fail without a fabricated
// success.
type ResponsesToAnthropicSSETranslator struct {
	PublicModel string
	Limits      Limits
	// AllowPrivateReasoningDrop drops the measured provider-private
	// reasoning OUTPUT shape (lossy compatibility). False keeps the
	// strict 502.
	AllowPrivateReasoningDrop bool
}

// Translate reads Responses SSE from r and writes Anthropic SSE events to
// w, calling flush after each frame. See the type comment for terminal and
// truncation behavior.
func (t *ResponsesToAnthropicSSETranslator) Translate(r io.Reader, w io.Writer, flush func()) *RouterError {
	frames := NewSSEFrameReader(r, t.Limits)
	dec := NewResponsesStreamDecoderWithPolicy(t.Limits, t.AllowPrivateReasoningDrop)
	enc := NewAnthropicEventEncoder(t.PublicModel, t.Limits)
	return TranslateStream(frames, dec, enc, w, flush, t.Limits, DialectResponses)
}
