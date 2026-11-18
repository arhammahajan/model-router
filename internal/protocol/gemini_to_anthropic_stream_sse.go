// Gemini SSE -> Anthropic streaming via the shared IR pipeline.
//
// Upstream framing is Gemini SSE (?alt=sse). The shared NewSSEFrameReader
// splits frames, NewGeminiStreamDecoder validates the source once, and
// NewAnthropicEventEncoder renders Anthropic named SSE. Pre-commit
// failures emit no bytes; post-commit failures emit exactly one typed
// Anthropic error event. Translate returns promptly at the terminal.
package protocol

import "io"

// GeminiToAnthropicSSETranslator converts a Gemini streamGenerateContent
// SSE stream (?alt=sse) into Anthropic Messages named SSE events.
type GeminiToAnthropicSSETranslator struct {
	PublicModel string
	Limits      Limits
	// AllowLossyDrops accepts validated routine provider/gateway response
	// extensions (thoughtSignature, finishMessage, TEXT-modality usage
	// details, usage.cost/usage.opencode, nonzero thoughtsTokenCount via
	// the Reasoning fold) after bounded validation; false keeps the
	// strict 502. On Anthropic targets it also enables the lossy
	// reasoning-breakdown projection.
	AllowLossyDrops bool
}

// Translate reads Gemini SSE from r and writes Anthropic SSE events to w,
// calling flush after each frame. It returns promptly after the valid
// terminal (finishReason chunk) without requiring EOF. Pre-commit failures
// emit no bytes; post-commit failures emit exactly one typed error event.
func (t *GeminiToAnthropicSSETranslator) Translate(r io.Reader, w io.Writer, flush func()) *RouterError {
	frames := NewSSEFrameReader(r, t.Limits)
	dec := NewGeminiStreamDecoderWithPolicy(t.Limits, true, t.AllowLossyDrops)
	enc := NewAnthropicEventEncoderWithPolicy(t.PublicModel, t.Limits, t.AllowLossyDrops)
	return TranslateStream(frames, dec, enc, w, flush, t.Limits, DialectGemini)
}
