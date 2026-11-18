// Gemini JSON-array -> Chat streaming via the shared IR pipeline.
//
// Upstream framing is the default JSON array: one GenerateContentResponse
// object per element, closed by a bare `]`. NewJSONArrayFrameReader splits
// elements, NewGeminiStreamDecoder arms the finish element and emits the
// terminal only on Frame{Closed:true}, and NewChatEventEncoder renders
// Chat SSE. EOF before `]` and trailing commas fail without success.
package protocol

import "io"

// GeminiToChatJSONArrayTranslator converts a Gemini streamGenerateContent
// JSON-array stream (default framing, no ?alt=sse) into Chat Completions
// SSE chunks.
type GeminiToChatJSONArrayTranslator struct {
	PublicModel  string
	IncludeUsage bool
	Limits       Limits
	// Plan restores emulated Chat customs on tool blocks. Nil or empty
	// restores nothing.
	Plan *ToolPlan
}

// Translate reads the JSON array from r and writes Chat SSE chunks to w,
// calling flush after each frame. It returns after the closing bracket
// without requiring EOF; bytes after `]` belong to the next generation and
// are left unread.
func (t *GeminiToChatJSONArrayTranslator) Translate(r io.Reader, w io.Writer, flush func()) *RouterError {
	frames := NewJSONArrayFrameReader(r, t.Limits)
	var dec StreamDecoder = NewGeminiStreamDecoder(t.Limits, false)
	if t.Plan != nil {
		dec = t.Plan.WrapDecoder(dec, t.Limits)
	}
	enc := NewChatEventEncoder(t.PublicModel, t.IncludeUsage, t.Limits)
	return TranslateStream(frames, dec, enc, w, flush, t.Limits, DialectGemini)
}
