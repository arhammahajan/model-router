// Gemini JSON-array -> Anthropic streaming via the shared IR pipeline.
//
// Upstream framing is the default JSON array. NewJSONArrayFrameReader
// splits elements, NewGeminiStreamDecoder arms the finish element and
// emits the terminal only on Frame{Closed:true}, and
// NewAnthropicEventEncoder renders Anthropic named SSE. EOF before `]`
// fails without a success marker.
package protocol

import "io"

// GeminiToAnthropicJSONArrayTranslator converts a Gemini
// streamGenerateContent JSON-array stream (default framing, no ?alt=sse)
// into Anthropic Messages named SSE events.
type GeminiToAnthropicJSONArrayTranslator struct {
	PublicModel string
	Limits      Limits
}

// Translate reads the JSON array from r and writes Anthropic SSE events to
// w, calling flush after each frame. It returns after the closing bracket
// without requiring EOF. Pre-commit failures emit no bytes; post-commit
// failures emit exactly one typed error event.
func (t *GeminiToAnthropicJSONArrayTranslator) Translate(r io.Reader, w io.Writer, flush func()) *RouterError {
	frames := NewJSONArrayFrameReader(r, t.Limits)
	dec := NewGeminiStreamDecoder(t.Limits, false)
	enc := NewAnthropicEventEncoder(t.PublicModel, t.Limits)
	return TranslateStream(frames, dec, enc, w, flush, t.Limits, DialectGemini)
}
