// Chat -> Anthropic response translation: complete JSON responses. The
// streaming path lives below in ChatToAnthropicSSETranslator.
//
// The guiding rule for unrepresentable upstream features (audio, refusal
// streaming, logprobs, annotations, reasoning accounting, unknown stop
// reasons, unknown usage members) is: fail the translation with HTTP 502.
// A truncated or contradictory response never produces a success marker.
// Errors are value-free: they name field paths, never content or secrets.
package protocol

import (
	"crypto/rand"
	"encoding/hex"
	"io"
)

// newAnthropicMessageID mints a nondeterministic Message ID. Conformance
// normalizes it.
func newAnthropicMessageID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "msg_" + hex.EncodeToString(b[:])
}

// ChatToAnthropicSSETranslator converts a Chat SSE stream into Anthropic
// Messages SSE via the shared pipeline (SSE framing, Chat source stream
// decoder, shared lifecycle, Anthropic target event encoder). It retains
// no parsing state: every Translate call builds fresh pipeline objects,
// so one translator value is reusable across generations.
//
// The pipeline stops reading upstream as soon as the source terminal is
// observed: Translate returns promptly after the Chat [DONE] marker with
// all blocks closed and terminal usage validated. Trailing bytes after the
// terminal are the same HTTP body's unread remainder (body closed, never
// another generation). Truncation, missing usage, duplicate markers, or
// unrepresentable events fail without a synthetic success marker.
//
// Lossy compatibility (AllowPrivateReasoningDrop true) drops substantive
// delta.reasoning_content and the known gateway usage extensions
// (cost, opencode) at decode, and projects the Chat-source reasoning
// breakdown for Anthropic (aggregate output_tokens preserved). False keeps
// the strict 502s. Responses/Gemini accounting unchanged.
type ChatToAnthropicSSETranslator struct {
	PublicModel string
	Limits      Limits
	// AllowPrivateReasoningDrop drops substantive provider-private
	// delta.reasoning_content and the known gateway usage extensions
	// (lossy compatibility), and projects Chat reasoning for Anthropic.
	// False keeps the strict 502. Absent/null/empty is exact in both modes.
	AllowPrivateReasoningDrop bool
}

// Translate reads Chat SSE from r and writes Anthropic SSE events to w,
// calling flush after each frame. See the type comment for terminal and
// truncation behavior.
func (t *ChatToAnthropicSSETranslator) Translate(r io.Reader, w io.Writer, flush func()) *RouterError {
	frames := NewSSEFrameReader(r, t.Limits)
	dec := NewChatStreamDecoderWithPolicy(t.Limits, t.AllowPrivateReasoningDrop)
	enc := NewAnthropicEventEncoderWithPolicy(t.PublicModel, t.Limits, t.AllowPrivateReasoningDrop)
	return TranslateStream(frames, dec, enc, w, flush, t.Limits, DialectChat)
}
