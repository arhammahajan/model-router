// Gemini SSE -> Responses streaming via the shared IR pipeline.
//
// Upstream framing is Gemini SSE (?alt=sse). The shared NewSSEFrameReader
// splits frames, NewGeminiStreamDecoder validates the source once, and
// NewResponsesEventEncoder renders Responses named SSE. Pre-commit
// failures emit no bytes; post-commit failures emit exactly one
// response.failed. Translate returns promptly at the terminal.
package protocol

import "io"

// GeminiToResponsesSSETranslator converts a Gemini streamGenerateContent
// SSE stream (?alt=sse) into Responses API named SSE events.
type GeminiToResponsesSSETranslator struct {
	PublicModel string
	Limits      Limits
	// Plan restores Responses namespace identities on tool blocks. Nil or
	// empty restores nothing; native streams stay raw.
	Plan *ToolPlan
}

// Translate reads Gemini SSE from r and writes Responses SSE events to w,
// calling flush after each frame. It returns promptly after the valid
// terminal (finishReason chunk) without requiring EOF. Pre-commit failures
// emit no bytes; post-commit failures emit exactly one response.failed.
func (t *GeminiToResponsesSSETranslator) Translate(r io.Reader, w io.Writer, flush func()) *RouterError {
	frames := NewSSEFrameReader(r, t.Limits)
	var dec StreamDecoder = NewGeminiStreamDecoder(t.Limits, true)
	if t.Plan != nil {
		dec = t.Plan.WrapDecoder(dec, t.Limits)
	}
	enc := NewResponsesEventEncoder(t.PublicModel, t.Limits)
	return TranslateStream(frames, dec, enc, w, flush, t.Limits, DialectGemini)
}
