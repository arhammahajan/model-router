// Anthropic -> Gemini JSON-array streaming: the same shared upstream
// state machine as AnthropicToGeminiSSETranslator (see
// anthropic_to_gemini_stream.go for the event/state contract), framed
// incrementally as one JSON array:
//
//	`[` object (` ,` object)* `]`
//
// Frames are written through without ever buffering the whole generation:
// the `[` prefix rides on the first object, `,\r\n` separates later
// objects, and `]` closes the array after a valid message_stop. A stream
// that fails before any frame writes nothing (an ordinary 502, never a
// partial array); a failure after the first frame terminates without `]`,
// so the client detects truncation instead of reading a manufactured
// success.
package protocol

import (
	"io"
)

// AnthropicToGeminiJSONArrayTranslator converts an Anthropic SSE stream
// into a Gemini streamGenerateContent JSON array via the shared pipeline
// (SSE framing, Anthropic source stream decoder, shared lifecycle, Gemini
// target event encoder with array framing). It retains no parsing state:
// every Translate call builds fresh pipeline objects, so one translator
// value is reusable across generations.
type AnthropicToGeminiJSONArrayTranslator struct {
	PublicModel string
	Limits      Limits
	// AllowGatewayBillingDrop drops validated gateway billing metadata
	// (usage.cost, usage.opencode) when true (lossy compatibility).
	// False keeps the strict 502.
	AllowGatewayBillingDrop bool
}

// Translate reads Anthropic SSE from r and writes the incremental JSON
// array to w, calling flush after each frame. It returns nil promptly
// after the complete message_stop with the array closed; EOF is not
// required after the protocol terminal. Any truncation, missing usage,
// unfinished tool identity, invalid tool arguments, duplicate preterminal
// markers, or unrepresentable event is an error with no success marker.
func (t *AnthropicToGeminiJSONArrayTranslator) Translate(r io.Reader, w io.Writer, flush func()) *RouterError {
	frames := NewSSEFrameReader(r, t.Limits)
	dec := NewAnthropicStreamDecoderWithPolicy(t.Limits, t.AllowGatewayBillingDrop)
	enc := NewGeminiEventEncoder(t.PublicModel, t.Limits, false)
	return TranslateStream(frames, dec, enc, w, flush, t.Limits, DialectAnthropic)
}
