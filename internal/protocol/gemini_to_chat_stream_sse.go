// Gemini SSE -> Chat streaming via the shared IR pipeline.
//
// Upstream framing is Gemini SSE (?alt=sse): `data: {...}` per partial
// GenerateContentResponse, no event names, no [DONE] sentinel. The shared
// NewSSEFrameReader splits frames, NewGeminiStreamDecoder validates the
// source once (terminal-own usage, SSE prompt return, immediate text,
// exact bigint/empty args), and NewChatEventEncoder renders Chat SSE.
// Translate returns promptly after the valid terminal without requiring
// EOF; bytes after the terminal belong to the next generation and are
// left unread.
package protocol

import "io"

// GeminiToChatSSETranslator converts a Gemini streamGenerateContent SSE
// stream (?alt=sse) into Chat Completions SSE chunks.
type GeminiToChatSSETranslator struct {
	PublicModel  string
	IncludeUsage bool
	Limits       Limits
	// Plan restores emulated Chat customs on tool blocks. Nil or empty
	// restores nothing.
	Plan *ToolPlan
}

// Translate reads Gemini SSE from r and writes Chat SSE chunks to w,
// calling flush after each frame. It returns promptly after the valid
// terminal (finishReason chunk) without requiring EOF; bytes after the
// terminal belong to the next generation and are left unread.
func (t *GeminiToChatSSETranslator) Translate(r io.Reader, w io.Writer, flush func()) *RouterError {
	frames := NewSSEFrameReader(r, t.Limits)
	var dec StreamDecoder = NewGeminiStreamDecoder(t.Limits, true)
	if t.Plan != nil {
		dec = t.Plan.WrapDecoder(dec, t.Limits)
	}
	enc := NewChatEventEncoder(t.PublicModel, t.IncludeUsage, t.Limits)
	return TranslateStream(frames, dec, enc, w, flush, t.Limits, DialectGemini)
}
