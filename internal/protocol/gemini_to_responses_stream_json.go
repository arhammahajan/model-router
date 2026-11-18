// Gemini JSON-array -> Responses streaming via the shared IR pipeline.
//
// Upstream framing is the default JSON array. NewJSONArrayFrameReader
// splits elements, NewGeminiStreamDecoder arms the finish element and
// emits the terminal only on Frame{Closed:true}, and
// NewResponsesEventEncoder renders Responses named SSE. EOF before `]`
// fails without a success marker.
package protocol

import "io"

// GeminiToResponsesJSONArrayTranslator converts a Gemini
// streamGenerateContent JSON-array stream (default framing, no ?alt=sse)
// into Responses API named SSE events.
type GeminiToResponsesJSONArrayTranslator struct {
	PublicModel string
	Limits      Limits
	// Plan restores Responses namespace identities on tool blocks. Nil or
	// empty restores nothing; native streams stay raw.
	Plan *ToolPlan
	// AllowLossyDrops accepts validated routine provider/gateway response
	// extensions (thoughtSignature, finishMessage, TEXT-modality usage
	// details, usage.cost/usage.opencode, nonzero thoughtsTokenCount via
	// the Reasoning fold) after bounded validation; false keeps the
	// strict 502.
	AllowLossyDrops bool
}

// Translate reads the JSON array from r and writes Responses SSE events to
// w, calling flush after each frame. It returns after the closing bracket
// without requiring EOF. Pre-commit failures emit no bytes; post-commit
// failures emit exactly one response.failed.
func (t *GeminiToResponsesJSONArrayTranslator) Translate(r io.Reader, w io.Writer, flush func()) *RouterError {
	frames := NewJSONArrayFrameReader(r, t.Limits)
	var dec StreamDecoder = NewGeminiStreamDecoderWithPolicy(t.Limits, false, t.AllowLossyDrops)
	if t.Plan != nil {
		dec = t.Plan.WrapDecoder(dec, t.Limits)
	}
	enc := NewResponsesEventEncoder(t.PublicModel, t.Limits)
	return TranslateStream(frames, dec, enc, w, flush, t.Limits, DialectGemini)
}
