// Responses -> Chat streaming via the shared pipeline.
package protocol

import (
	"io"
)

// ResponsesToChatSSETranslator converts a Responses SSE stream into Chat
// Completions SSE chunks via the shared pipeline (SSE framing, Responses
// source stream decoder, shared lifecycle, Chat target event encoder). It
// retains no parsing state: every Translate call builds fresh pipeline
// objects, so one translator value is reusable across generations.
//
// The pipeline stops reading upstream as soon as the source terminal is
// observed: Translate returns promptly after response.completed/incomplete
// with all blocks closed and terminal usage validated. Trailing bytes after
// the terminal are the same HTTP body's unread remainder (body closed,
// never another generation). Truncation, missing usage, duplicate markers,
// or unrepresentable events fail without a synthetic success marker.
type ResponsesToChatSSETranslator struct {
	PublicModel  string
	IncludeUsage bool
	Limits       Limits
	// Plan threads the Chat request native custom plan into the stream
	// response path. Nil or custom-free plans guard nothing and preserve
	// the native passthrough.
	Plan *ToolPlan
	// AllowPrivateReasoningDrop drops the measured provider-private
	// reasoning OUTPUT shape (lossy compatibility). False keeps the
	// strict 502.
	AllowPrivateReasoningDrop bool
}

// Translate reads Responses SSE from r and writes Chat SSE frames to w,
// calling flush after each frame. See the type comment for terminal and
// truncation behavior.
func (t *ResponsesToChatSSETranslator) Translate(r io.Reader, w io.Writer, flush func()) *RouterError {
	frames := NewSSEFrameReader(r, t.Limits)
	var dec StreamDecoder = NewResponsesStreamDecoderWithPolicy(t.Limits, t.AllowPrivateReasoningDrop)
	if t.Plan != nil {
		dec = t.Plan.WrapResponsesForChat(dec)
	}
	enc := NewChatEventEncoder(t.PublicModel, t.IncludeUsage, t.Limits)
	return TranslateStream(frames, dec, enc, w, flush, t.Limits, DialectResponses)
}
