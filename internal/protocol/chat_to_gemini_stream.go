// Chat -> Gemini response streaming: bounded incremental translation of a
// Chat SSE stream into Gemini downstream SSE data frames with no [DONE]
// marker. See chat_to_gemini_stream_array.go for the incremental JSON
// array framing of the same stream.
//
// This translator is a thin selection over the shared pipeline: one SSE
// frame reader (framing), one Chat source stream decoder (byte
// validation, semantic events), and one reusable Gemini target event
// encoder (downstream framing). It owns no source parsing, no event
// state, and no target policy of its own. For the wire contract (text
// immediacy, tool buffering bounds, terminal gating, failure ownership)
// see response_event_encode_gemini.go and
// response_chat_stream_decode.go.
package protocol

import (
	"io"
)

// ChatToGeminiSSETranslator converts a Chat SSE stream into Gemini SSE
// `data:` frames with no [DONE] terminator, enforcing the typed stream
// state machine and all buffer bounds through the shared pipeline.
type ChatToGeminiSSETranslator struct {
	PublicModel string
	Limits      Limits
	// AllowPrivateReasoningDrop drops substantive provider-private
	// delta.reasoning_content (lossy compatibility). False keeps the
	// strict 502.
	AllowPrivateReasoningDrop bool
}

// Translate reads Chat SSE from r and writes Gemini SSE frames to w,
// calling flush after each frame. It returns nil promptly after a complete
// terminal sequence (finish_reason, terminal usage, [DONE]); EOF is not
// required after the protocol terminal. Any truncation, missing usage,
// unfinished tool identity, invalid tool arguments, duplicate preterminal
// markers, or unrepresentable event is an error with no success marker.
// A downstream write failure stops upstream reads immediately.
func (t *ChatToGeminiSSETranslator) Translate(r io.Reader, w io.Writer, flush func()) *RouterError {
	frames := NewSSEFrameReader(r, t.Limits)
	dec := NewChatStreamDecoderWithPolicy(t.Limits, t.AllowPrivateReasoningDrop)
	enc := NewGeminiEventEncoder(t.PublicModel, t.Limits, true)
	return TranslateStream(frames, dec, enc, w, flush, t.Limits, DialectChat)
}
