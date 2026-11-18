// Responses source stream codec: one SSE decoder for the Responses row.
//
// ResponsesStreamDecoder validates the Responses SSE source ONCE,
// independently of destination, and emits canonical semantic events for the
// shared lifecycle (ResponseLifecycle) and the single pipeline
// (TranslateStream). It performs no downstream writes, mints no downstream
// IDs, and enforces no destination policy: target differences (refusal
// rendering, tool-name grammar, usage detail breakout) live in target
// encoders via CheckRepresentable, never here.
//
// Wire contract (proven against the three replaced pair translators in
// chat_responses_stream.go, responses_to_anthropic_stream_state.go and
// responses_to_gemini_stream*.go, which this decoder does NOT call):
//
//   - Framing is owned by FrameSource (NewSSEFrameReader). Frame.Event is
//     checked for coherence with the data body's type: a named event that
//     contradicts data.type fails closed, a malformed data.type cannot fall
//     back to the event name, and data-only frames are supported per the
//     existing contracts (r2gStream.handleFrame precedent).
//   - "[DONE]" is never a Responses terminal: without a valid
//     response.completed/incomplete it is a truncation (chat_responses_stream.go,
//     r2gStream.handleFrame precedent).
//   - Empty data-only frames are malformed (legacy JSON-parse failure);
//     event-only frames have no Responses keepalive contract and fail the
//     same way.
//   - response.created is the required first event and emits ResponseStart.
//     The created response object must carry the actual source id; model
//     may be omitted or null (absence preserved truthfully as "" for the
//     later provider report via ResponseModelEvent, like the accepted
//     Anthropic/Gemini absence seam). A supplied empty or non-string
//     model remains a 502. The first real model on a later
//     response.in_progress/queued envelope or on the actual terminal
//     response object emits a typed ResponseModelEvent BEFORE the
//     terminal UsageEvent plus CompletionEvent.
//   - sequence_number is exact JSON-integer coherence when present
//     (strictly increasing); absent/null is allowed per the existing
//     contracts. It never drives ordering.
//   - output_index is the source-logical position: required exact on item
//     events, unique per generation. Arrival order is canonical (never
//     sorted); tool blocks carry output_index as typed SourceIndex so
//     targets order by provenance instead of reparsing opaque ItemIDs.
//     Text blocks carry no index (current lifecycle forbids it).
//   - Item identity is the source item id (required, opaque, bounded);
//     tool identity is call_id (required, opaque, bounded, never the 1..64
//     name grammar: the complete Responses decoder already superseded that
//     old-stream bug). Item and call identities live in separate namespaces.
//   - Message blocks open at content_part.added (kind from the part type:
//     output_text vs refusal); exactly one part per message item.
//     response.output_text/refusal.delta emits immediate TextFragment events
//     (text never retained: counts plus SHA256 only); empty deltas emit
//     nothing (legacy skip policy). .done payloads must hash-match the
//     streamed deltas; duplicates fail.
//   - Tool arguments buffer under the inherited bounds only (per-call
//     MaxArgsChars, aggregate MaxStreamArgsChars) and emit ArgsFragment
//     events immediately once the block opens. arguments.done must equal
//     the accumulation exactly.
//   - Malformed classification (grammar-invalid nonempty name, non-object
//     argument payload) emits MalformedToolEvent at output_item.done for
//     the target gate (Gemini suppress-and-remap vs Anthropic/Responses
//     refuse), mirroring the complete Responses row. Missing/empty call_id
//     or name cannot open a block and stays a 502 identity failure, as do
//     syntax, size, and envelope violations.
//   - Reasoning OUTPUT items, media parts, hosted tools, namespaces,
//     async calls, and populated annotations/logprobs fail closed.
//     Scalar accounting (reasoning_tokens, cached/cached-write shares)
//     travels in the terminal usage buckets with presence/provenance so
//     the destination gate decides.
//   - terminal response.completed/incomplete reuses the shared
//     DecodeResponsesResponse validator on its embedded response object:
//     that envelope IS the actual source wire (not an intermediate), so
//     output, usage, and finish validation stay single-sourced. The decoder
//     additionally proves the terminal agrees with the stream (id/model
//     stability with absent-to-actual model promotion, per-block hash/
//     identity/args agreement, malformed counts, unfinished-block refusal).
//     Usage arrives only in the terminal, so the first-reported model (if
//     not already known) emits ResponseModelEvent first, then exactly one
//     terminal UsageEvent precedes CompletionEvent.
//   - response.failed and error events are 502 failures, never success.
//     FailureEvent is never emitted (source failures are *RouterError for
//     the pipeline; post-commit failure output belongs to the encoder).
//   - The terminal emits UsageEvent then CompletionEvent and returns
//     promptly (no EOF read after). Close validates state, reads nothing,
//     and returns no events.
//
// Divergence log (same-source predecessors differ; classification
// deliberate unless noted):
//
//  1. call_id grammar: old Responses STREAMS applied validateToolName to
//     call_id (chat_responses_stream.go r2cs, r2g onItemAdded) while the
//     unified complete decoder uses validateCallID only. This decoder
//     follows the complete row (opaque + bounded); the 1..64 grammar
//     stays target-side.
//  2. malformed names/args: old streams failed grammar-invalid names and
//     non-object args as destination-specific 502s; the complete row
//     counts them as MalformedTools. This decoder classifies them with
//     MalformedToolEvent (typed provenance), failing only missing/empty
//     identities, syntax, and size as 502.
//  3. item ids: r2g tolerated absent item ids (keying by output_index);
//     r2cs required them. This decoder requires them (opaque block
//     identity for semantic events), matching r2cs.
//  4. message content at added: r2cs ignored it, r2g required []. This
//     decoder tolerates absent/null and requires [] when present;
//     non-empty content at added is invalid.
//  5. role at added: r2cs tolerated absent roles; the complete row
//     requires assistant. This decoder tolerates absent/null and
//     requires assistant when present (r2cs policy kept).
//  6. multi-part messages: r2g refused content_index != 0; r2cs merged
//     silently. This decoder allows exactly one part per message item
//     (a second content_part.added for the same item is a duplicate);
//     merging would silently alter block structure.
//  7. output_index on created/progress/terminal envelopes: r2cs tolerated
//     it; this decoder refuses unknown non-null envelope members
//     (official envelopes carry none).
//  8. delta after args.done: r2cs deferred the contradiction to the
//     terminal compare; this decoder fails fast (same verdict class).
//  9. terminal model stability: old rows compared ids only; this decoder
//     also compares the model (ResponseStart carries real provenance).
//
// Ordering report (no silent reordering; schema limits stated, not
// widened): canonical order is arrival order of output_item.added.
// output_index travels as SourceIndex on all block kinds (tool and
// single-part text/refusal), so a (theoretical, never observed on
// official servers which emit added events in output order)
// out-of-order-index arrival is refused as unrepresentable order at
// the offending added frame, before success: no guessed sorting, no
// silent normalization. Ordered arrival is the positive case; a reordered
// negative is pinned in the review tests. No hidden ID conventions are
// invented: ItemIDs stay opaque source handles and SourceIndex stays an
// explicit int. The terminal additionally validates raw output array
// positions against source output_index (position p must hold the item
// added with output_index p), not just arrival order over canonical
// items.
//
// Bounds (inherited, none new): per-call args MaxArgsChars, retained args
// aggregate MaxStreamArgsChars, distinct blocks MaxStreamBlocks, tool
// calls MaxToolCalls. Text is unretained and unbounded here by design
// (the lifecycle counts but never caps text bytes; incremental encoders
// forward without buffering).
package protocol

import (
	"encoding/json"
	"fmt"
	"hash"
	"strings"
)

// responsesStreamSlot is one in-flight Responses output item keyed by
// output_index. Text bytes are never retained (hash/length only);
// argument bytes buffer under the Limits bounds for done-exactness and
// terminal consistency (the decoder's only retention).
type responsesStreamSlot struct {
	outIdx int
	itemID string
	kind   string // "message" | "tool" | "custom" | "reasoning"

	// message state (kind assigned at content_part.added)
	partKind     string // "" | "text" | "refusal"
	textHash     hash.Hash
	textLen      int
	textDone     bool
	doneHash     [32]byte
	doneLen      int
	partDone     bool
	itemDone     bool
	doneStatus   string
	messagePhase string

	// tool state (identity complete at output_item.added)
	callID   string
	name     string
	badName  bool // grammar-invalid name: typed malformed provenance
	args     strings.Builder
	argsLen  int
	argsDone bool

	// pendingIdentityBytes tracks this slot's contribution to the
	// decoder's pending (pre-BlockOpen) identity budget. Message item
	// IDs are withheld from the lifecycle until content_part.added
	// opens the block; bound them here so missing parts cannot bypass
	// retention. Released once the block opens (identity flows into
	// the lifecycle) or the empty item closes (nothing retained).
	pendingIdentityBytes int
}

// malformedTool reports the typed source-fact classification for one tool
// block: a grammar-invalid nonempty name or an accumulated argument
// payload that never formed one JSON object. Source-absent arguments
// (empty accumulation) canonicalize to "{}" and are valid, exactly like
// the complete Responses row. Custom blocks classify only on bad names;
// their string input is always well-formed when present.
func (s *responsesStreamSlot) malformedTool() bool {
	if s.kind != "tool" && s.kind != "custom" {
		return false
	}
	if s.badName {
		return true
	}
	if s.kind == "custom" {
		return false
	}
	if s.args.String() == "" {
		return false
	}
	_, rerr := CompactArgs(json.RawMessage(s.args.String()))
	return rerr != nil
}

// ResponsesStreamDecoder decodes Responses SSE frames into canonical
// semantic events. It owns only upstream source state; it performs no
// downstream writes, holds no reader, spawns no goroutines, and enforces
// no destination policy. Use it with NewSSEFrameReader and TranslateStream.
type ResponsesStreamDecoder struct {
	lim Limits
	// allowPrivateDrop drops the measured provider-private reasoning
	// OUTPUT shape (lossy compatibility). False keeps the strict 502.
	// Absent reasoning decodes identically in both modes.
	allowPrivateDrop bool

	started bool
	respID  string
	model   string

	hasSeq  bool
	lastSeq int

	slots     map[int]*responsesStreamSlot
	order     []int
	seenItem  map[string]bool
	seenCall  map[string]bool
	tools     int
	totalArgs int
	malformed int

	// pendingIdentityBytes bounds source-owned message identities
	// withheld from the lifecycle before BlockOpen (mirrors the Chat
	// pending-identity fix 2e51011 without conflating argument
	// limits: this budget is separate from totalArgs and from the
	// lifecycle's own post-open identity budget).
	pendingIdentityBytes int64

	done bool
}

// NewResponsesStreamDecoder creates the Responses source stream decoder
// for one generation (strict: reasoning OUTPUT items are 502).
func NewResponsesStreamDecoder(lim Limits) *ResponsesStreamDecoder {
	return &ResponsesStreamDecoder{
		lim:      lim,
		slots:    map[int]*responsesStreamSlot{},
		seenItem: map[string]bool{},
		seenCall: map[string]bool{},
	}
}

// NewResponsesStreamDecoderWithPolicy creates the Responses source
// stream decoder with narrow private-reasoning policy:
// allowPrivateDrop drops the measured reasoning shape, false keeps the
// strict 502.
func NewResponsesStreamDecoderWithPolicy(lim Limits, allowPrivateDrop bool) *ResponsesStreamDecoder {
	return &ResponsesStreamDecoder{
		lim:              lim,
		allowPrivateDrop: allowPrivateDrop,
		slots:            map[int]*responsesStreamSlot{},
		seenItem:         map[string]bool{},
		seenCall:         map[string]bool{},
	}
}

// DecodeFrame decodes one typed SSE frame into semantic events in wire
// order. It returns promptly at the terminal (no EOF read) and never
// emits FailureEvent (all failures are *RouterError for the pipeline to
// finalize).
func (d *ResponsesStreamDecoder) DecodeFrame(frame Frame) ([]ResponseEvent, *RouterError) {
	if d.done {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent events after completion"}
	}
	data := frame.Data
	if data == "[DONE]" {
		// The Responses terminal is response.completed/incomplete, never
		// [DONE]: an upstream DONE without a valid terminal is a truncation.
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream stream ended without completion"}
	}
	if data == "" {
		// Responses has no ping/keepalive data contract: empty payloads
		// (data-only or event-only) are malformed, matching the legacy
		// JSON-parse failure.
		return nil, rsBad()
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &body); err != nil || body == nil {
		return nil, rsBad()
	}
	var typ string
	if rawT, ok := body["type"]; ok && !isNull(rawT) {
		// A supplied malformed type cannot fall back to the event name.
		if err := json.Unmarshal(rawT, &typ); err != nil || typ == "" {
			return nil, rsBad()
		}
	}
	if frame.Event != "" && typ != "" && frame.Event != typ {
		return nil, rsBad()
	}
	if typ == "" {
		typ = frame.Event
	}
	if typ == "" {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream stream event is missing its event name"}
	}
	// Terminal failures report the upstream error even before creation.
	switch typ {
	case "response.failed", "error":
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream stream reported an error"}
	}
	if !d.started && typ != "response.created" {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream stream sent content before response.created"}
	}
	switch typ {
	case "response.created":
		return d.onCreated(body)
	case "response.in_progress", "response.queued":
		return d.onProgress(body)
	case "response.output_item.added":
		return d.onItemAdded(body)
	case "response.content_part.added":
		return d.onPartAdded(body)
	case "response.output_text.delta":
		return d.onTextDelta(body, false)
	case "response.refusal.delta":
		return d.onTextDelta(body, true)
	case "response.output_text.done":
		return nil, d.onTextDone(body, false)
	case "response.refusal.done":
		return nil, d.onTextDone(body, true)
	case "response.function_call_arguments.delta":
		return d.onArgsDelta(body)
	case "response.function_call_arguments.done":
		return nil, d.onArgsDone(body)
	case "response.custom_tool_call_input.delta":
		return d.onCustomDelta(body)
	case "response.custom_tool_call_input.done":
		return nil, d.onCustomDone(body)
	case "response.content_part.done":
		return nil, d.onPartDone(body)
	case "response.output_item.done":
		return d.onItemDone(body)
	case "response.completed":
		return d.onTerminal(body, "completed")
	case "response.incomplete":
		return d.onTerminal(body, "incomplete")
	default:
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: fmt.Sprintf("Upstream stream event %q has no representation", typ)}
	}
}

// Close validates trailing state after the source terminal. It performs
// no reads and returns no events: UsageEvent and CompletionEvent were
// already emitted at response.completed/incomplete.
func (d *ResponsesStreamDecoder) Close() ([]ResponseEvent, *RouterError) {
	if !d.done {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream ended without completion"}
	}
	return nil, nil
}

func rsBad() *RouterError {
	return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
}

func rsUnrep(what string) *RouterError {
	return &RouterError{Status: 502, Code: CodeUpstreamBad,
		Msg: fmt.Sprintf("Upstream response feature %q has no representation", what)}
}

// responsesCanonicalArgsEqual reports whether two streamed tool-argument
// payloads agree canonically: source-absent "" normalizes to "{}", valid
// JSON objects compare via CompactArgs (whitespace-insensitive,
// numeric-token-preserving via json.Compact), and non-object/malformed
// payloads fall back to raw exactness so the typed malformed
// classification at output_item.done survives. This mirrors the legacy
// Responses→Anthropic canonical compare (compactChatArgs on both sides at
// arguments.done/item.done/terminal), which is why the long-standing
// tool-first positive carries arguments.done {"tz":"UTC"} against
// item.done {"tz": "UTC"}.
func responsesCanonicalArgsEqual(a, b string) bool {
	if a == "" {
		a = "{}"
	}
	if b == "" {
		b = "{}"
	}
	ca, errA := CompactArgs(json.RawMessage(a))
	cb, errB := CompactArgs(json.RawMessage(b))
	if errA != nil || errB != nil {
		return a == b
	}
	return string(ca) == string(cb)
}

// responsesStreamObject decodes a JSON object under an allowlist with the
// same-source null-tolerant policy (r2aCheckEventKeys precedent): unknown
// null members are newer defaults and pass; unknown non-null members fail
// with a neutral value-free path. Non-objects are malformed.
func responsesStreamObject(raw json.RawMessage, allowed map[string]bool, what string) (map[string]json.RawMessage, *RouterError) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, rsBad()
	}
	for k, v := range obj {
		if allowed[k] {
			continue
		}
		if isNull(v) {
			continue
		}
		return nil, rsUnrep(what + "." + k)
	}
	return obj, nil
}

// checkSequence enforces strictly increasing sequence_number when present.
// Absent or null sequence numbers are allowed for senders that omit them;
// present values must be exact non-negative JSON integers.
func (d *ResponsesStreamDecoder) checkSequence(body map[string]json.RawMessage) *RouterError {
	raw, ok := body["sequence_number"]
	if !ok || isNull(raw) {
		return nil
	}
	n, rerr := responseInt(raw)
	if rerr != nil {
		return rsBad()
	}
	if d.hasSeq && n <= d.lastSeq {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream stream sequence is out of order"}
	}
	d.lastSeq, d.hasSeq = n, true
	return nil
}

// opaqueBoundedStream reports whether s is an acceptable opaque source
// identity at this boundary: nonempty after trim, valid UTF-8, within
// the per-field byte budget. No character grammar: provider handles may
// contain spaces or colons.
func opaqueBoundedStream(s string, lim Limits) bool {
	if !validOpaqueID(s) {
		return false
	}
	return len(s) <= lim.MaxArgsChars
}

// streamResponseEnvelope validates a streamed (possibly partial) response
// object with the shared complete-envelope member policy, then validates
// the core source shapes eagerly (the echo allowlist alone does NOT
// validate core: object/model/id/created_at/status/error/incomplete
// shape and phase state are checked here, not deferred to the complete
// decoder), then enforces id/model stability against the generation
// identity.
func (d *ResponsesStreamDecoder) streamResponseEnvelope(raw json.RawMessage, what string) (map[string]json.RawMessage, *RouterError) {
	top, rerr := responsesSourceEnvelope(raw)
	if rerr != nil {
		return nil, rerr
	}
	if rerr := validateStreamResponseCore(top, what); rerr != nil {
		return nil, rerr
	}
	if rawID, ok := top["id"]; ok && !isNull(rawID) {
		var id string
		if err := json.Unmarshal(rawID, &id); err != nil || !opaqueBoundedStream(id, d.lim) {
			return nil, rsBad()
		}
		if d.respID == "" {
			d.respID = id
		} else if d.respID != id {
			return nil, &RouterError{Status: 502, Code: CodeUpstreamBad,
				Msg: "Upstream stream changed its response identity"}
		}
	}
	if rawModel, ok := top["model"]; ok && !isNull(rawModel) {
		var m string
		if err := json.Unmarshal(rawModel, &m); err != nil || strings.TrimSpace(m) == "" {
			return nil, rsBad()
		}
		if d.model == "" {
			d.model = m
		} else if d.model != m {
			return nil, &RouterError{Status: 502, Code: CodeUpstreamBad,
				Msg: "Upstream stream changed its response identity"}
		}
	}
	return top, nil
}

// validateStreamResponseCore validates the core source shapes of a
// streamed (partial) response object for its phase. The shared echo
// allowlist (responsesSourceEnvelope) only checks request-echo members;
// core member VALUES are validated here so a malformed core (e.g.
// object:17) cannot pass the stream on the assumption that the complete
// decoder will see it later. Terminal-only members (output/usage/
// incomplete_details) must be absent/null/empty at created/progress:
// any populated terminal state before the terminal contradicts the
// source phase. No defaults are invented: a missing id still fails
// at response.created via the caller's presence check, and a missing
// model there is truthful absence promoted later via ResponseModelEvent
// (supplied-empty/non-string models fail in streamResponseEnvelope).
func validateStreamResponseCore(top map[string]json.RawMessage, what string) *RouterError {
	phase := "stream"
	if what == "response.created" || what == "response.in_progress" || what == "response.queued" {
		phase = "pre-terminal"
	}
	if rawObj, ok := top["object"]; ok && !isNull(rawObj) {
		var o string
		if err := json.Unmarshal(rawObj, &o); err != nil || o != "response" {
			return rsBad()
		}
	}
	if rawCreated, ok := top["created_at"]; ok && !isNull(rawCreated) {
		if rerr := r2cValidateCreatedAt(rawCreated); rerr != nil {
			return rsBad()
		}
	}
	if rawStatus, ok := top["status"]; ok && !isNull(rawStatus) {
		var st string
		if err := json.Unmarshal(rawStatus, &st); err != nil {
			return rsBad()
		}
		switch phase {
		case "pre-terminal":
			// Created/progress responses are live generations, never
			// terminal states. A completed/incomplete/failed status
			// before the terminal event contradicts the phase.
			if st != "in_progress" && st != "queued" {
				return rsBad()
			}
		default:
			switch st {
			case "in_progress", "queued", "completed", "incomplete", "failed":
			default:
				return rsUnrep("response.status")
			}
		}
	}
	if rawErr, ok := top["error"]; ok && !isNull(rawErr) {
		var errObj map[string]json.RawMessage
		if err := json.Unmarshal(rawErr, &errObj); err != nil {
			return rsBad()
		}
		if len(errObj) > 0 {
			return &RouterError{Status: 502, Code: CodeUpstreamBad,
				Msg: "Upstream stream reported an error"}
		}
	}
	if phase == "pre-terminal" {
		if rawOut, ok := top["output"]; ok && !isNull(rawOut) {
			var arr []json.RawMessage
			if err := json.Unmarshal(rawOut, &arr); err != nil || arr == nil {
				return rsBad()
			}
			if len(arr) != 0 {
				return rsBad()
			}
		}
		if rawUsage, ok := top["usage"]; ok && !isNull(rawUsage) {
			return rsBad()
		}
		if rawDet, ok := top["incomplete_details"]; ok && !isNull(rawDet) {
			return rsBad()
		}
	}
	return nil
}

func (d *ResponsesStreamDecoder) onCreated(body map[string]json.RawMessage) ([]ResponseEvent, *RouterError) {
	if d.started {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream stream sent a duplicate response.created"}
	}
	if _, rerr := responsesStreamObject(mustRaw(body), map[string]bool{
		"type": true, "sequence_number": true, "response": true,
	}, "response.created"); rerr != nil {
		return nil, rerr
	}
	if rerr := d.checkSequence(body); rerr != nil {
		return nil, rerr
	}
	rawResp, ok := body["response"]
	if !ok || isNull(rawResp) {
		return nil, rsBad()
	}
	if _, rerr := d.streamResponseEnvelope(rawResp, "response.created"); rerr != nil {
		return nil, rerr
	}
	// ResponseStart needs the actual source identity; the model may be
	// absent (omitted/null) when the provider reports it later. The
	// terminal never revisits the start, so a later first report arrives
	// as a typed ResponseModelEvent on progress or at the terminal,
	// never as a fabricated start model.
	if d.respID == "" {
		return nil, rsBad()
	}
	d.started = true
	return []ResponseEvent{ResponseStart{ID: ResponseID(d.respID), Model: d.model, Source: DialectResponses}}, nil
}

func (d *ResponsesStreamDecoder) onProgress(body map[string]json.RawMessage) ([]ResponseEvent, *RouterError) {
	typ := "response.in_progress"
	if rawT, ok := body["type"]; ok && !isNull(rawT) {
		_ = json.Unmarshal(rawT, &typ)
	}
	if _, rerr := responsesStreamObject(mustRaw(body), map[string]bool{
		"type": true, "sequence_number": true, "response": true,
	}, typ); rerr != nil {
		return nil, rerr
	}
	if rerr := d.checkSequence(body); rerr != nil {
		return nil, rerr
	}
	hadModel := d.model != ""
	if rawResp, ok := body["response"]; ok && !isNull(rawResp) {
		if _, rerr := d.streamResponseEnvelope(rawResp, typ); rerr != nil {
			return nil, rerr
		}
	}
	// First provider report of a previously absent model is a typed
	// metadata event, emitted immediately in wire order (no whole-
	// generation buffering). A repeated identical model emits nothing;
	// drift already failed inside streamResponseEnvelope.
	if !hadModel && d.model != "" {
		return []ResponseEvent{ResponseModelEvent{Model: d.model}}, nil
	}
	return nil, nil
}

// slotFor resolves the block for an output_index/item_id pair, enforcing
// cross-coherence: the item id must match the added identity.
func (d *ResponsesStreamDecoder) slotFor(outIdx int, itemID string) (*responsesStreamSlot, *RouterError) {
	s, ok := d.slots[outIdx]
	if !ok {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream stream sent a delta for an unknown block"}
	}
	if itemID == "" || itemID != s.itemID {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream returned conflicting response item identities"}
	}
	return s, nil
}

func (d *ResponsesStreamDecoder) onItemAdded(body map[string]json.RawMessage) ([]ResponseEvent, *RouterError) {
	if _, rerr := responsesStreamObject(mustRaw(body), map[string]bool{
		"type": true, "sequence_number": true, "output_index": true, "item": true,
	}, "response.output_item.added"); rerr != nil {
		return nil, rerr
	}
	if rerr := d.checkSequence(body); rerr != nil {
		return nil, rerr
	}
	rawIdx, ok := body["output_index"]
	if !ok || isNull(rawIdx) {
		return nil, rsBad()
	}
	outIdx, rerr := responseInt(rawIdx)
	if rerr != nil {
		return nil, rsBad()
	}
	if _, dup := d.slots[outIdx]; dup {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream returned conflicting response item identities"}
	}
	// Canonical order is arrival order and text blocks carry no
	// SourceIndex, so a reordered output_index cannot preserve ordered
	// semantics: refuse it as unrepresentable before success instead of
	// guessing a sort or silently normalizing. Official servers emit
	// added events in output order, so ordered arrival is the positive
	// case (see the ordering report in the file header).
	for _, prev := range d.order {
		if outIdx < prev {
			return nil, rsUnrep("response.output order")
		}
	}
	rawItem, ok := body["item"]
	if !ok || isNull(rawItem) {
		return nil, rsBad()
	}
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(rawItem, &probe); err != nil || probe.Type == "" {
		return nil, rsBad()
	}
	switch probe.Type {
	case "message":
		return nil, d.onMessageAdded(outIdx, rawItem)
	case "function_call":
		return d.onToolAdded(outIdx, rawItem)
	case "custom_tool_call":
		return d.onCustomAdded(outIdx, rawItem)
	case "reasoning":
		if !d.allowPrivateDrop {
			return nil, rsUnrep("response.output_item.added.item.type")
		}
		return nil, d.onReasoningAdded(outIdx, rawItem)
	default:
		return nil, rsUnrep("response.output_item.added.item.type")
	}
}
