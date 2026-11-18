// Anthropic -> Gemini response streaming: bounded incremental translation
// of an Anthropic SSE stream into Gemini downstream frames (SSE data
// frames in AnthropicToGeminiSSETranslator; incremental JSON array in
// AnthropicToGeminiJSONArrayTranslator, see
// anthropic_to_gemini_stream_array.go).
//
// Event/state contract (grounded in the primary sources cited in
// anthropic_to_gemini_json.go and the Anthropic streaming reference):
//
//	Upstream (Anthropic SSE): named events with matching type members —
//	message_start (full Message envelope with empty content and the input
//	accounting), then per-block content_block_start / content_block_delta /
//	content_block_stop triplets keyed by logical content index, then one or
//	more message_delta events (stop_reason plus cumulative authoritative
//	usage), then message_stop. usage counts in message_delta are cumulative
//	totals, never increments. ping events are keepalive; error events and
//	any other name fail closed.
//	Downstream (Gemini): a sequence of GenerateContentResponse objects —
//	as SSE `data: {...}` frames with no [DONE] marker, or as one JSON
//	array written incrementally (`[` frame (`,` frame)* `]`, never a
//	whole-generation buffer). Each object carries one candidate (index 0)
//	with model content parts; only the terminal object carries
//	finishReason and usageMetadata. Only text and complete functionCall
//	parts are ever emitted; thinking, signatures, citations, media,
//	server tools and provider-private state are never fabricated.
//	State machine: text deltas flush downstream immediately with no
//	retention; tool fragments accumulate under the Limits bounds (1 MiB
//	per call, 8 MiB combined, 1024 blocks, 8 MiB per frame) and are
//	emitted exactly once, as complete validated functionCall parts.
//	Complete parallel calls release by logical index: a closed call waits
//	while any smaller index is still open, so emission order always
//	matches content order. Later text while an earlier block is still
//	incomplete fails closed — emitting it would change the order, and the
//	generation is never buffered whole to reorder it.
//	The terminal object is written only after message_stop with all blocks
//	closed, the stop reason and the cumulative usage accounted. It carries
//	no text (nothing is duplicated) and no DONE marker. Midstream failures
//	are sanitized 502s with no success marker (no terminal object; for the
//	array path no `]`). A downstream write failure stops upstream reads
//	immediately. A valid message_stop returns promptly without requiring
//	EOF.
//
// Source contracts (verified November 2026):
//
//	Anthropic event flow, delta types and cumulative message_delta usage:
//	https://platform.claude.com/docs/en/build-with-claude/streaming
//	Anthropic stop reasons and usage summation:
//	https://docs.anthropic.com/en/api/messages
//	Gemini streamGenerateContent (SSE frames; otherwise one JSON array):
//	https://ai.google.dev/api/rest/v1beta/models/streamGenerateContent
package protocol

import (
	"io"
)

// AnthropicToGeminiSSETranslator converts an Anthropic SSE stream into
// Gemini SSE `data:` frames with no [DONE] terminator via the shared
// pipeline (SSE framing, Anthropic source stream decoder, shared
// lifecycle, Gemini target event encoder). It retains no parsing state:
// every Translate call builds fresh pipeline objects, so one translator
// value is reusable across generations.
type AnthropicToGeminiSSETranslator struct {
	PublicModel string
	Limits      Limits
	// AllowGatewayBillingDrop drops validated gateway billing metadata
	// (usage.cost, usage.opencode) when true (lossy compatibility).
	// False keeps the strict 502.
	AllowGatewayBillingDrop bool
}

// Translate reads Anthropic SSE from r and writes Gemini SSE frames to w,
// calling flush after each frame. It returns nil promptly after a complete
// message_stop; EOF is not required after the protocol terminal. Any
// truncation, missing accounting, unfinished blocks, invalid tool
// arguments, duplicate preterminal markers, or unrepresentable event is an
// error with no success marker.
func (t *AnthropicToGeminiSSETranslator) Translate(r io.Reader, w io.Writer, flush func()) *RouterError {
	frames := NewSSEFrameReader(r, t.Limits)
	dec := NewAnthropicStreamDecoderWithPolicy(t.Limits, t.AllowGatewayBillingDrop)
	enc := NewGeminiEventEncoder(t.PublicModel, t.Limits, true)
	return TranslateStream(frames, dec, enc, w, flush, t.Limits, DialectAnthropic)
}
