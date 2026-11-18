// Responses -> Gemini SSE streaming via the shared pipeline.
package protocol

import (
	"io"
)

// ResponsesToGeminiSSETranslator converts a Responses SSE stream into
// Gemini SSE `data:` frames with no [DONE] terminator via the shared
// pipeline (SSE framing, Responses source stream decoder, shared lifecycle,
// Gemini target event encoder with SSE framing). It retains no parsing
// state: every Translate call builds fresh pipeline objects, so one
// translator value is reusable across generations.
type ResponsesToGeminiSSETranslator struct {
	PublicModel string
	Limits      Limits
	// AllowPrivateReasoningDrop drops the measured provider-private
	// reasoning OUTPUT shape (lossy compatibility). False keeps the
	// strict 502.
	AllowPrivateReasoningDrop bool
}

// Translate reads Responses SSE from r and writes Gemini SSE frames to w,
// calling flush after each frame. It returns promptly after a complete
// terminal sequence; EOF is not required after the protocol terminal. Any
// truncation, missing usage, unfinished tool identity, invalid terminal
// consistency, duplicate markers, or unrepresentable event is an error.
func (t *ResponsesToGeminiSSETranslator) Translate(r io.Reader, w io.Writer, flush func()) *RouterError {
	frames := NewSSEFrameReader(r, t.Limits)
	dec := NewResponsesStreamDecoderWithPolicy(t.Limits, t.AllowPrivateReasoningDrop)
	enc := NewGeminiEventEncoder(t.PublicModel, t.Limits, true)
	return TranslateStream(frames, dec, enc, w, flush, t.Limits, DialectResponses)
}
