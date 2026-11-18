// Chat -> Responses response translation: shared helpers plus the thin
// STREAM path through the shared pipeline.
//
// The complete Chat row is decoded by DecodeChatResponse and rendered by
// EncodeResponsesResponse. This file owns only the shared Responses
// framing helpers still referenced by other codecs (newResponsesID,
// renderResponsesResponse) and the thin Chat
// STREAM translator below, which wires one shared source decoder plus
// one reusable target encoder through the single TranslateStream
// pipeline. It performs no source JSON parsing, calls no pair
// translator, builds no Chat JSON intermediate, and maps no semantic
// IR itself.
package protocol

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"time"
)

// responsesNowUnix is the created_at clock, overridable in tests.
var responsesNowUnix = func() int64 { return time.Now().Unix() }

// newResponsesID mints nondeterministic response/item IDs. Conformance
// normalizes them. Generated item identities (fc_*) are always distinct
// from the upstream call identity (call_id) they restore.
func newResponsesID(prefix string) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return prefix + "_" + hex.EncodeToString(b[:])
}

// renderResponsesResponse renders the Responses envelope for one terminal
// response. usage may be nil (pre-commit events carry usage null); output
// items carry their final content.
func renderResponsesResponse(publicModel string, createdAt int64, status, incompleteReason string, output []any, usage map[string]any) ([]byte, *RouterError) {
	var incomplete any
	incomplete = nil
	if status == "incomplete" {
		incomplete = map[string]any{"reason": incompleteReason}
	}
	if output == nil {
		output = []any{}
	}
	out := map[string]any{
		"id": newResponsesID("resp"), "object": "response",
		"created_at": createdAt, "model": publicModel,
		"status": status, "error": nil,
		"incomplete_details": incomplete,
		"output":             output,
		"usage":              usage,
	}
	data, err := json.Marshal(out)
	if err != nil {
		return nil, &RouterError{Status: 500, Code: "encoding_error", Msg: "failed to encode downstream response"}
	}
	return data, nil
}

// --- Streaming ---

// ChatToResponsesSSETranslator converts a Chat SSE stream into Responses
// API SSE events through the single shared pipeline: one SSE frame
// reader, one shared Chat source stream decoder, and one reusable
// Responses target event encoder. Each Translate call builds fresh
// decoder and encoder state, so translator reuse never carries
// generation state across streams.
type ChatToResponsesSSETranslator struct {
	PublicModel string
	Limits      Limits
	// Plan restores Responses namespace identities on tool blocks. Nil or
	// empty restores nothing; native streams stay raw.
	Plan *ToolPlan
	// AllowPrivateReasoningDrop drops substantive provider-private
	// delta.reasoning_content (lossy compatibility). False keeps the
	// strict 502.
	AllowPrivateReasoningDrop bool
}

// Translate reads Chat SSE from r and writes Responses SSE events to w,
// calling flush after each frame. It returns promptly after a valid
// terminal (finish plus authoritative usage plus [DONE]) with all blocks
// closed; EOF is not required after the protocol terminal and bytes
// after the terminal marker are ignored in the same response body; the caller
// closes the body. Any truncation, missing usage, unfinished tool identity,
// invalid tool arguments, duplicate preterminal markers, or
// unrepresentable event is an error with no synthetic success marker.
func (t *ChatToResponsesSSETranslator) Translate(r io.Reader, w io.Writer, flush func()) *RouterError {
	frames := NewSSEFrameReader(r, t.Limits)
	var dec StreamDecoder = NewChatStreamDecoderWithPolicy(t.Limits, t.AllowPrivateReasoningDrop)
	if t.Plan != nil {
		dec = t.Plan.WrapDecoder(dec, t.Limits)
	}
	enc := NewResponsesEventEncoder(t.PublicModel, t.Limits)
	return TranslateStream(frames, dec, enc, w, flush, t.Limits, DialectChat)
}
