// Shared target stream encoder: canonical ResponseEvent -> Anthropic SSE.
//
// AnthropicEventEncoder is source-neutral: it never parses source JSON,
// never calls pair translators, never constructs Chat JSON, and never
// maps semantic IR. It encodes lifecycle-validated ResponseEvents in
// order, owning only downstream framing (commitment, Anthropic block
// index mapping, message_start/message_delta/message_stop framing).
// Every Chat source stream selects it via TranslateStream with
// DialectChat; other sources will select the same encoder later.
//
// Wire contract (proven by the replaced ChatToAnthropicSSETranslator):
//   - message_start carries the public model and a zero usage
//     placeholder (Chat prompt totals arrive only in the terminal
//     chunk). When ResponseStart.InitialUsage is present (future
//     sources), its prompt/completion totals are preserved via the
//     shared Anthropic accounting instead of the placeholder.
//   - Text and tool fragments forward immediately; nothing is retained.
//   - Target block indices are monotonic emission order (0, 1, ...),
//     unique and stable; ItemIDs are never parsed.
//   - Tool names enforce the shared 1..64 grammar at encode (target
//     policy); call/response/item IDs stay opaque and are never run
//     through the name grammar.
//   - Terminal usage arrives late: message_delta carries the
//     authoritative totals with input = prompt - cache shares (shared
//     encodeAnthropicUsage), never underreported zeros.
//   - MalformedToolEvent and unrepresentable usage/finish are semantic
//     refusals (EncodeError WriteFailed=false): the pipeline Aborts
//     with exactly one sanitized native event:error when committed,
//     never a message_stop.
//   - Downstream write/flush failures are EncodeError WriteFailed=true:
//     no Abort write follows, state is sticky, and the pipeline stops
//     reading upstream.
//   - Every encoded SSE frame (including deltas) is bounded by
//     lim.MaxFrameBytes on its full encoded bytes. Argument bytes are
//     bounded upstream (decoder + lifecycle); this encoder adds no text
//     retention and no extra text cap.
package protocol

import (
	"bytes"
	"encoding/json"
	"io"
	"sort"
	"strings"
)

// AnthropicEventEncoder encodes semantic stream events as Anthropic
// Messages SSE. Use one per generation with NewSSEFrameReader,
// a source StreamDecoder, and TranslateStream.
type AnthropicEventEncoder struct {
	publicModel string
	lim         Limits
	// allowReasoningBreakdownDrop projects the Chat or Responses reasoning breakdown
	// for Anthropic (aggregate output_tokens preserved). False keeps the
	// strict 502. Responses/Gemini accounting unchanged; only
	// source==Chat with dest==Anthropic is affected.
	allowReasoningBreakdownDrop bool

	msgID     string
	committed bool
	aborted   bool
	writeFail bool

	started bool
	source  Dialect
	respID  ResponseID
	model   string
	initial *ResponseUsage
	// hasStart tracks the upstream ResponseStart independently of the
	// downstream commitment flag (started); done latches the terminal
	// so a late metadata report fails instead of rewriting history.
	hasStart bool
	done     bool
	// idOrigin/modelOrigin mirror the upstream start provenance with
	// late promotions applied. Empty reads as provider. Router
	// origins are valid only for the Gemini source.
	idOrigin    IdentityOrigin
	modelOrigin IdentityOrigin
	// sawIdentity latches the at-most-once ProviderIdentityEvent.
	sawIdentity bool
	// minted tracks router-minted call IDs for the terminal summary.
	minted map[CallID]struct{}

	nextIndex int
	anthIdx   map[ItemID]int
	kinds     map[ItemID]BlockKind
	toolName  map[ItemID]string
	toolCall  map[ItemID]CallID

	malformed int
	usage     *ResponseUsage
}

// NewAnthropicEventEncoder creates the Anthropic target stream encoder
// for one generation. Public model stamps message_start; lim bounds
// encoded frames. Strict mode rejects nonzero reasoning breakdowns.
func NewAnthropicEventEncoder(publicModel string, lim Limits) *AnthropicEventEncoder {
	return NewAnthropicEventEncoderWithPolicy(publicModel, lim, false)
}

// NewAnthropicEventEncoderWithPolicy creates the Anthropic target stream
// encoder with narrow lossy policy: allowReasoningBreakdownDrop drops the
// Chat or Responses scalar reasoning breakdown, preserving aggregate
// output_tokens. Strict mode rejects nonzero reasoning breakdowns.
func NewAnthropicEventEncoderWithPolicy(publicModel string, lim Limits, allowReasoningBreakdownDrop bool) *AnthropicEventEncoder {
	return &AnthropicEventEncoder{
		publicModel:                 publicModel,
		lim:                         lim,
		allowReasoningBreakdownDrop: allowReasoningBreakdownDrop,
		msgID:                       newAnthropicMessageID(),
		anthIdx:                     map[ItemID]int{},
		kinds:                       map[ItemID]BlockKind{},
		toolName:                    map[ItemID]string{},
		toolCall:                    map[ItemID]CallID{},
		minted:                      map[CallID]struct{}{},
	}
}

// Committed reports whether downstream output was committed.
func (e *AnthropicEventEncoder) Committed() bool { return e.committed }

// Abort emits one sanitized native event:error after commitment, or is
// a no-op before commitment, after a writer failure, or when called
// twice. It never emits a success terminal.
func (e *AnthropicEventEncoder) Abort(w io.Writer, flush func()) {
	if !e.committed || e.aborted || e.writeFail {
		return
	}
	e.aborted = true
	payload := map[string]any{
		"type":  "error",
		"error": map[string]any{"type": "api_error", "message": "Upstream stream reported an error"},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	var buf bytes.Buffer
	buf.WriteString("event: error\n")
	buf.WriteString("data: ")
	buf.Write(data)
	buf.WriteString("\n\n")
	if buf.Len() > e.lim.MaxFrameBytes {
		return
	}
	_, _ = w.Write(buf.Bytes())
	if flush != nil {
		flush()
	}
}

// Encode encodes one lifecycle-validated event. Events must arrive in
// lifecycle order.
func (e *AnthropicEventEncoder) Encode(w io.Writer, flush func(), ev ResponseEvent) *EncodeError {
	if e.writeFail {
		return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Downstream write failed"}, WriteFailed: true}
	}
	switch v := ev.(type) {
	case ResponseStart:
		e.source = v.Source
		e.respID = v.ID
		e.model = v.Model
		e.initial = v.InitialUsage
		e.idOrigin, e.modelOrigin = v.IDOrigin, v.ModelOrigin
		if e.idOrigin == "" {
			e.idOrigin = IdentityProvider
		}
		if e.modelOrigin == "" {
			e.modelOrigin = IdentityProvider
		}
		if (e.idOrigin == IdentityRouterMinted || e.modelOrigin == IdentityRouterMinted) && v.Source != DialectGemini {
			return anthropicEncodeFail("Upstream response source does not match the requested source")
		}
		e.hasStart = true
		return nil
	case ResponseModelEvent:
		return e.encodeModel(v)
	case ProviderIdentityEvent:
		return e.encodeIdentity(v)
	case BlockOpen:
		return e.encodeBlockOpen(w, flush, v)
	case TextFragment:
		return e.encodeTextDelta(w, flush, v)
	case ArgsFragment:
		return e.encodeArgsDelta(w, flush, v)
	case BlockClose:
		return e.encodeBlockStop(w, flush, v)
	case MalformedToolEvent:
		e.malformed++
		return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: `Upstream response feature "response.choices[0].message.tool_calls" has no Anthropic representation`}, WriteFailed: false}
	case UsageEvent:
		if !v.Terminal() {
			return nil
		}
		cp := v.Usage
		e.usage = &cp
		return nil
	case CompletionEvent:
		return e.encodeCompletion(w, flush, v)
	case FailureEvent:
		// The pipeline centralizes failure output in fail() and never
		// passes FailureEvent to Encode. Defensive refusal only.
		return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream reported an error"}, WriteFailed: false}
	default:
		return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}, WriteFailed: false}
	}
}

func anthropicEncodeFail(msg string) *EncodeError {
	return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: msg}, WriteFailed: false}
}

func (e *AnthropicEventEncoder) writeFrame(w io.Writer, flush func(), name string, payload map[string]any) *EncodeError {
	data, err := json.Marshal(payload)
	if err != nil {
		return &EncodeError{Err: &RouterError{Status: 500, Code: "encoding_error", Msg: "failed to encode downstream event"}, WriteFailed: false}
	}
	var buf bytes.Buffer
	buf.WriteString("event: ")
	buf.WriteString(name)
	buf.WriteString("\ndata: ")
	buf.Write(data)
	buf.WriteString("\n\n")
	if buf.Len() > e.lim.MaxFrameBytes {
		return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream SSE frame exceeds the size bound"}, WriteFailed: false}
	}
	if _, err := w.Write(buf.Bytes()); err != nil {
		e.writeFail = true
		return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Downstream write failed"}, WriteFailed: true}
	}
	e.committed = true
	if flush != nil {
		flush()
	}
	return nil
}

// startUsage renders message_start accounting: preserved initial totals
// via the shared Anthropic mapping when the source supplied them, else
// the explicit zero placeholder (Chat: prompt totals arrive only at the
// terminal chunk).
func (e *AnthropicEventEncoder) startUsage() (map[string]any, *RouterError) {
	if e.initial == nil {
		return map[string]any{"input_tokens": 0, "output_tokens": 0}, nil
	}
	u := *e.initial
	bad := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned invalid initial usage accounting"}
	if u.Provenance.Source != e.source || u.Provenance.Scope != UsageInitial || !nonNegative(u.Prompt) || !nonNegative(u.Completion) {
		return nil, bad
	}
	if u.Total == nil {
		total := *u.Prompt + *u.Completion
		if total < *u.Prompt || total < *u.Completion {
			return nil, bad
		}
		u.Total = intPtr(total)
	}
	// Reuse the accounting validator for this snapshot without claiming
	// that these initial counts are authoritative terminal usage.
	u.Provenance.Scope = UsageTerminal
	if err := u.ValidateAuthoritative(); err != nil {
		return nil, err
	}
	if err := CheckUsageRepresentableWithPolicy(u, e.source, DialectAnthropic, e.allowReasoningBreakdownDrop); err != nil {
		return nil, err
	}
	return encodeAnthropicUsage(u)
}

func (e *AnthropicEventEncoder) ensureStarted(w io.Writer, flush func()) *EncodeError {
	if e.started {
		return nil
	}
	usage, rerr := e.startUsage()
	if rerr != nil {
		return &EncodeError{Err: rerr}
	}
	e.started = true
	return e.writeFrame(w, flush, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": e.msgID, "type": "message", "role": "assistant",
			"model": e.publicModel, "content": []any{},
			"stop_reason": nil, "stop_sequence": nil,
			"usage": usage,
		},
	})
}

func (e *AnthropicEventEncoder) encodeBlockOpen(w io.Writer, flush func(), open BlockOpen) *EncodeError {
	if _, dup := e.anthIdx[open.Item]; dup {
		return anthropicEncodeFail("Upstream opened a duplicate response block")
	}
	switch open.Kind {
	case BlockText, BlockRefusal:
		if open.Call != "" || open.Name != "" {
			return anthropicEncodeFail("Upstream text block carries a tool identity")
		}
	case BlockTool:
		if open.Call == "" || open.Name == "" {
			return anthropicEncodeFail("Upstream tool call is missing its identity")
		}
		if err := validateToolName(open.Name); err != nil {
			return anthropicEncodeFail("Upstream returned an invalid response")
		}
		callOrigin := open.CallOrigin
		if callOrigin == "" {
			callOrigin = IdentityProvider
		}
		if callOrigin == IdentityRouterMinted {
			if e.source != DialectGemini {
				return anthropicEncodeFail("Upstream response source does not match the requested source")
			}
			e.minted[open.Call] = struct{}{}
		}
	default:
		return anthropicEncodeFail("Upstream response block has no representation")
	}
	if rerr := e.ensureStarted(w, flush); rerr != nil {
		return rerr
	}
	idx := e.nextIndex
	e.nextIndex++
	e.anthIdx[open.Item] = idx
	e.kinds[open.Item] = open.Kind
	if open.Kind == BlockTool {
		e.toolName[open.Item] = open.Name
		e.toolCall[open.Item] = open.Call
	}
	var block map[string]any
	if open.Kind == BlockTool {
		block = map[string]any{"type": "tool_use", "id": string(open.Call), "name": open.Name, "input": map[string]any{}}
	} else {
		block = map[string]any{"type": "text", "text": ""}
	}
	return e.writeFrame(w, flush, "content_block_start", map[string]any{
		"type": "content_block_start", "index": idx, "content_block": block,
	})
}

func (e *AnthropicEventEncoder) encodeTextDelta(w io.Writer, flush func(), frag TextFragment) *EncodeError {
	idx, ok := e.anthIdx[frag.Item]
	if !ok {
		return anthropicEncodeFail("Upstream sent a delta for a closed or unopened block")
	}
	kind, ok := e.kinds[frag.Item]
	if !ok || (kind != BlockText && kind != BlockRefusal) {
		return anthropicEncodeFail("Upstream sent a delta for a closed or unopened block")
	}
	return e.writeFrame(w, flush, "content_block_delta", map[string]any{
		"type": "content_block_delta", "index": idx,
		"delta": map[string]any{"type": "text_delta", "text": frag.Text},
	})
}

func (e *AnthropicEventEncoder) encodeArgsDelta(w io.Writer, flush func(), frag ArgsFragment) *EncodeError {
	idx, ok := e.anthIdx[frag.Item]
	if !ok {
		return anthropicEncodeFail("Upstream sent a delta for a closed or unopened block")
	}
	if kind, ok := e.kinds[frag.Item]; !ok || kind != BlockTool {
		return anthropicEncodeFail("Upstream sent a delta for a closed or unopened block")
	}
	return e.writeFrame(w, flush, "content_block_delta", map[string]any{
		"type": "content_block_delta", "index": idx,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": frag.Fragment},
	})
}

func (e *AnthropicEventEncoder) encodeBlockStop(w io.Writer, flush func(), close BlockClose) *EncodeError {
	idx, ok := e.anthIdx[close.Item]
	if !ok {
		return anthropicEncodeFail("Upstream closed a block that was not open")
	}
	return e.writeFrame(w, flush, "content_block_stop", map[string]any{
		"type": "content_block_stop", "index": idx,
	})
}

// encodeModel consumes the typed provider-model report metadata-only:
// it updates the actual upstream model for the terminal policy
// summary and writes zero downstream bytes. Ordering and drift are
// enforced value-free.
func (e *AnthropicEventEncoder) encodeModel(v ResponseModelEvent) *EncodeError {
	if !e.hasStart {
		return anthropicEncodeFail("Upstream sent content before the response start")
	}
	if e.done {
		return anthropicEncodeFail("Upstream sent events after completion")
	}
	if v.Model == "" || strings.TrimSpace(v.Model) == "" {
		return anthropicEncodeFail("Upstream returned an invalid response")
	}
	if e.model == "" {
		e.model = v.Model
		e.modelOrigin = IdentityProvider
		return nil
	}
	if e.model != v.Model {
		return anthropicEncodeFail("Upstream stream changed its response identity")
	}
	return nil
}

// encodeIdentity consumes the late provider responseId metadata-only.
// It updates the upstream ID for the terminal policy summary and
// writes zero downstream bytes. At-most-once, pre-terminal only.
func (e *AnthropicEventEncoder) encodeIdentity(v ProviderIdentityEvent) *EncodeError {
	if !e.hasStart {
		return anthropicEncodeFail("Upstream sent content before the response start")
	}
	if e.done {
		return anthropicEncodeFail("Upstream sent events after completion")
	}
	if e.sawIdentity {
		return anthropicEncodeFail("Upstream sent a duplicate response start")
	}
	if !validOpaqueID(string(v.ID)) || len(string(v.ID)) > e.lim.MaxArgsChars {
		return anthropicEncodeFail("Upstream returned an invalid response")
	}
	if e.source != DialectGemini {
		return anthropicEncodeFail("Upstream response source does not match the requested source")
	}
	if e.idOrigin == IdentityProvider {
		return anthropicEncodeFail("Upstream returned conflicting response identities")
	}
	e.sawIdentity = true
	e.respID = v.ID
	e.idOrigin = IdentityProvider
	return nil
}

// encodeCompletion validates the truthful event summary against the
// shared destination contract, then emits block stops, the authoritative
// message_delta, and message_stop. The summary reuses
// CheckRepresentableWithPolicy plus the shared stop/usage mapping;
// argument bytes are never inspected by the Anthropic gate, so tool items
// carry EmptyArgs placeholders while identities, kinds, usage, finish,
// and malformed counts are exact. Lossy reasoning is projected by
// dropping the breakdown and preserving aggregate output_tokens.
func (e *AnthropicEventEncoder) encodeCompletion(w io.Writer, flush func(), complete CompletionEvent) *EncodeError {
	if !e.hasStart {
		return anthropicEncodeFail("Upstream sent content before the response start")
	}
	if e.done {
		return anthropicEncodeFail("Upstream sent events after completion")
	}
	if e.usage == nil {
		return anthropicEncodeFail("Upstream stream ended without authoritative usage")
	}
	items := make([]ResponseItem, 0, len(e.anthIdx))
	seenOrder := make([]ItemID, 0, len(e.anthIdx))
	for item := range e.anthIdx {
		seenOrder = append(seenOrder, item)
	}
	sort.Slice(seenOrder, func(i, j int) bool { return e.anthIdx[seenOrder[i]] < e.anthIdx[seenOrder[j]] })
	for _, item := range seenOrder {
		switch e.kinds[item] {
		case BlockText:
			items = append(items, TextItem{ID: item})
		case BlockRefusal:
			items = append(items, RefusalItem{ID: item})
		case BlockTool:
			items = append(items, ToolCallItem{ID: item, CallID: e.toolCall[item], Name: e.toolName[item], Arguments: EmptyArgs()})
		default:
			return anthropicEncodeFail("Upstream returned an invalid response")
		}
	}
	summary := Response{
		ID:                e.respID,
		Model:             e.model,
		Source:            e.source,
		Items:             items,
		Usage:             *e.usage,
		Finish:            complete.Finish,
		MalformedTools:    e.malformed,
		IDOrigin:          e.idOrigin,
		ModelOrigin:       e.modelOrigin,
		RouterMintedCalls: e.minted,
	}
	if rerr := CheckRepresentableWithPolicy(summary, DialectAnthropic, e.allowReasoningBreakdownDrop); rerr != nil {
		return &EncodeError{Err: rerr, WriteFailed: false}
	}
	stop, rerr := anthropicResponseStop(summary)
	if rerr != nil {
		return &EncodeError{Err: rerr, WriteFailed: false}
	}
	usage, rerr := encodeAnthropicUsage(summary.Usage)
	if rerr != nil {
		return &EncodeError{Err: rerr, WriteFailed: false}
	}
	if rerr := e.ensureStarted(w, flush); rerr != nil {
		return rerr
	}
	if len(seenOrder) == 0 {
		idx := e.nextIndex
		if rerr := e.writeFrame(w, flush, "content_block_start", map[string]any{
			"type": "content_block_start", "index": idx,
			"content_block": map[string]any{"type": "text", "text": ""},
		}); rerr != nil {
			return rerr
		}
		if rerr := e.writeFrame(w, flush, "content_block_stop", map[string]any{
			"type": "content_block_stop", "index": idx,
		}); rerr != nil {
			return rerr
		}
	}
	if rerr := e.writeFrame(w, flush, "message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason": stop, "stop_sequence": nil,
		},
		"usage": usage,
	}); rerr != nil {
		return rerr
	}
	if rerr := e.writeFrame(w, flush, "message_stop", map[string]any{"type": "message_stop"}); rerr != nil {
		return rerr
	}
	e.done = true
	return nil
}
