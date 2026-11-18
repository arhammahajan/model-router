// Chat -> Gemini JSON-array streaming: the same shared pipeline as
// ChatToGeminiSSETranslator (see chat_to_gemini_stream.go), framed
// incrementally as one JSON array:
//
//	`[` object (` ,` object)* `]`
//
// Frames are written through without ever buffering the whole generation:
// the `[` prefix rides on the first object, `,\r\n` separates later
// objects, and `]` closes the array after a valid [DONE]. A stream that
// fails before any frame writes nothing (an ordinary 502, never a partial
// array); a failure after the first frame terminates without `]`, so the
// client detects truncation instead of reading a manufactured success.
package protocol

import (
	"io"
)

// ChatToGeminiJSONArrayTranslator converts a Chat SSE stream into a
// Gemini streamGenerateContent JSON array, written incrementally. It
// enforces the same typed stream state machine and buffer bounds as the
// SSE translator: terminal output only after finish_reason, authoritative
// usage and [DONE]; text deltas flush immediately; complete functionCalls
// render once in the terminal object.
type ChatToGeminiJSONArrayTranslator struct {
	PublicModel string
	Limits      Limits
	// AllowPrivateReasoningDrop drops substantive provider-private
	// delta.reasoning_content (lossy compatibility). False keeps the
	// strict 502.
	AllowPrivateReasoningDrop bool
}

// Translate reads Chat SSE from r and writes the incremental JSON array to
// w, calling flush after each frame. It returns nil promptly after a
// complete terminal sequence (finish_reason, terminal usage, [DONE]) with
// the array closed; EOF is not required after the protocol terminal. Any
// truncation, missing usage, unfinished tool identity, invalid tool
// arguments, duplicate preterminal markers, or unrepresentable event is an
// error with no success marker and no closing `]`.
func (t *ChatToGeminiJSONArrayTranslator) Translate(r io.Reader, w io.Writer, flush func()) *RouterError {
	frames := NewSSEFrameReader(r, t.Limits)
	dec := NewChatStreamDecoderWithPolicy(t.Limits, t.AllowPrivateReasoningDrop)
	enc := NewGeminiEventEncoder(t.PublicModel, t.Limits, false)
	return TranslateStream(frames, dec, enc, w, flush, t.Limits, DialectChat)
}
