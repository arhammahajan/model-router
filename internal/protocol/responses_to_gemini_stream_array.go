// Responses -> Gemini JSON-array streaming via the shared pipeline.
package protocol

import (
	"io"
)

// ResponsesToGeminiJSONArrayTranslator converts a Responses SSE stream
// into a Gemini streamGenerateContent JSON array via the shared pipeline
// (SSE framing, Responses source stream decoder, shared lifecycle, Gemini
// target event encoder with array framing). It retains no parsing state:
// every Translate call builds fresh pipeline objects, so one translator
// value is reusable across generations.
type ResponsesToGeminiJSONArrayTranslator struct {
	PublicModel string
	Limits      Limits
	// AllowPrivateReasoningDrop drops the measured provider-private
	// reasoning OUTPUT shape (lossy compatibility). False keeps the
	// strict 502.
	AllowPrivateReasoningDrop bool
}

// Translate reads Responses SSE from r and writes the incremental JSON
// array to w, calling flush after each frame. It returns promptly after a
// complete terminal event with the array closed; EOF is not required after
// the protocol terminal. Any truncation, missing usage, unfinished tool
// identity, invalid terminal consistency, duplicate markers, or
// unrepresentable event is an error with no success marker.
func (t *ResponsesToGeminiJSONArrayTranslator) Translate(r io.Reader, w io.Writer, flush func()) *RouterError {
	frames := NewSSEFrameReader(r, t.Limits)
	dec := NewResponsesStreamDecoderWithPolicy(t.Limits, t.AllowPrivateReasoningDrop)
	enc := NewGeminiEventEncoder(t.PublicModel, t.Limits, false)
	return TranslateStream(frames, dec, enc, w, flush, t.Limits, DialectResponses)
}
