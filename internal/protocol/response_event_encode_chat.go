// Shared Chat target stream encoder: semantic ResponseEvent -> Chat SSE.
//
// ChatEventEncoder consumes only lifecycle-validated semantic events
// source-neutrally: it never parses source JSON, never calls a pair
// translator, never builds a Chat JSON intermediate, and never maps
// semantic IR. It owns only downstream state (commitment, minted
// completion identity, dense tool indices, framing) plus minimal
// identity/index state. Text and argument bytes are never retained;
// argument fragments forward immediately.
//
// Wire preserved (union of the three replaced Chat downstream
// translators, canonicalized to the documented form):
//   - Minted stable router completion ID (newChatID stream) and
//     created_at per generation (time.Now, matching the shared
//     complete-target policy which mints per response); public model on
//     every chunk.
//   - Role delta first ({"role":"assistant","content":""}), grounded in
//     anthropic_to_chat.go ensureStarted, gemini_to_chat_stream.go
//     ensureStarted, chat_responses_stream.go r2csEnsureStarted. Commitment
//     happens on the first BlockOpen (like Anthropic/Responses targets);
//     an empty generation still commits at the terminal (like G2C/R2C
//     terminal ensureStarted).
//   - Text fragments forward immediately as {"content":...}; refusal
//     fragments as {"refusal":...} (Responses->Chat precedent; Chat source
//     never emits refusal deltas, other sources may).
//   - Tool fragments forward immediately under dense Chat indices
//     (0,1,... in BlockOpen arrival order) with opaque call IDs verbatim.
//     First non-empty fragment for an index carries id/type/name plus the
//     fragment; later fragments carry index plus arguments only (Anthropic
//     precedent). A tool that never emits (no fragments or only empty)
//     emits one identity chunk with exact "{}" at close (Anthropic
//     block-stop precedent), so downstream always sees valid JSON.
//     Empty "" argument fragments emit nothing (Responses precedent),
//     avoiding noise while preserving identity.
//   - SourceIndex is typed provenance only: Chat indices are dense arrival
//     order, never reparsed ItemIDs ("c5" stays opaque) and never sorted
//     by source index. Unsupported interleavings are refused at the
//     terminal, never silently reordered.
//   - Terminal: finish chunk (delta {}, finish_reason via shared
//     chatResponseFinish) then, when includeUsage is true, one separate
//     empty-choices usage chunk ({"choices":[],"usage":{...}} via shared
//     chatResponseUsage) matching the documented stream_options
//     include_usage form and the Gemini->Chat dedicated-terminal test.
//     Difference note: Responses->Chat and Anthropic->Chat embed usage in
//     the finish chunk instead of a separate chunk; this encoder prefers
//     the documented separate chunk (see TestGeminiToChatUsageHasDedicated
//     TerminalChunk). When includeUsage is false no usage appears on the
//     wire, but the terminal UsageEvent is still required for success.
//     Then exactly "data: [DONE]" once, only on valid CompletionEvent
//     with authoritative usage.
//   - Usage: prompt/completion/total verbatim; cached detail read;
//     reasoning detail scalar. Gate via shared CheckRepresentable (which
//     includes CheckUsageRepresentable): Anthropic inclusive prompt
//     (creation+read) is a source-decoder fact rendered verbatim; Chat
//     cached detail read; Responses reasoning scalar maps; Responses
//     cache_write (as CacheCreation, non-Anthropic source) refuses;
//     unknown/nonzero unmapped buckets (Thoughts, ToolUsePrompt,
//     TextTokens for Anthropic/Gemini) refuse, never fabricated zeros.
//     InitialUsage is validated for representability when present but
//     never substituted for the terminal total.
//   - MalformedToolEvent refuses immediately (no fabricated native stop);
//     order gate (text-after-tool, refusal+tools) refuses at the terminal
//     per the complete Chat destination policy, instead of silent reorder.
//   - Failure ownership: Abort latches failure and emits nothing. The
//     Chat-downstream runtime adapters abort silently after commit, never
//     emitting nonstandard error chunks or [DONE]; no server change.
//     Pre-commit semantic errors emit zero bytes. Downstream write
//     failures are sticky EncodeError WriteFailed=true: no secondary
//     writes, no further reads, no Abort after writer failure.
//   - Every encoded SSE frame (including deltas and [DONE]) is bounded by
//     lim.MaxFrameBytes on its full encoded bytes. No whole-text or
//     generation retention, no extra text cap; only identity/index state.
//   - One instance per generation; translators create a fresh encoder per
//     Translate call.
package protocol

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"time"
)

// ChatEventEncoder encodes validated semantic stream events as Chat
// Completions SSE. Use one per generation with NewSSEFrameReader, a
// source StreamDecoder, and TranslateStream.
type ChatEventEncoder struct {
	publicModel  string
	includeUsage bool
	lim          Limits

	id      string
	created int64

	committed   bool
	writeFailed bool
	aborted     bool

	hasStart bool
	source   Dialect
	respID   ResponseID
	model    string
	// idOrigin/modelOrigin mirror the upstream start provenance with
	// late promotions applied. Empty reads as provider. Router
	// origins are valid only for the Gemini source.
	idOrigin    IdentityOrigin
	modelOrigin IdentityOrigin
	// sawIdentity latches the at-most-once ProviderIdentityEvent.
	sawIdentity bool
	// minted tracks router-minted call IDs for the terminal summary.
	minted map[CallID]struct{}

	nextChatIdx int
	order       []ItemID
	blocks      map[ItemID]*chatEventBlock

	malformed int
	usage     *ResponseUsage
	done      bool
	// customBufs buffers freeform custom input per block for the single
	// buffered Chat custom delta at close. Bounded by lim (per-field and
	// aggregate) like the source decoder; text stays incremental.
	customBufs  map[ItemID]*strings.Builder
	customTotal int
	// funcBufs holds function argument fragments delayed behind an
	// earlier pending custom call so mixed Chat SSE emits tool deltas in
	// source item order. Only fragments for functions with an earlier
	// unemitted custom are buffered; function-only generations stay
	// fully incremental. Bounded by lim (per-field MaxArgsChars and
	// aggregate MaxStreamArgsChars shared with customTotal).
	funcBufs  map[ItemID]*pendingFuncArgs
	funcTotal int
}

// pendingFuncArgs holds one deferred function block's ordered fragments.
type pendingFuncArgs struct {
	frags []string
	bytes int
}

// chatEventBlock is the encoder's per-block identity record. No content
// bytes are stored here except custom input buffering in the encoder map.
type chatEventBlock struct {
	kind    BlockKind
	callID  CallID
	name    string
	chatIdx int
	emitted bool
	closed  bool
}

// NewChatEventEncoder creates the Chat target stream encoder for one
// generation. Public model stamps every chunk; lim bounds every encoded
// frame prewrite; includeUsage selects the documented separate
// empty-choices usage chunk.
func NewChatEventEncoder(publicModel string, includeUsage bool, lim Limits) *ChatEventEncoder {
	return &ChatEventEncoder{
		publicModel:  publicModel,
		includeUsage: includeUsage,
		lim:          lim,
		id:           newChatID(true),
		created:      time.Now().Unix(),
		blocks:       map[ItemID]*chatEventBlock{},
		minted:       map[CallID]struct{}{},
		customBufs:   map[ItemID]*strings.Builder{},
		funcBufs:     map[ItemID]*pendingFuncArgs{},
	}
}

// Committed reports whether any downstream frame was written.
func (e *ChatEventEncoder) Committed() bool { return e.committed }

// Abort latches failure and emits nothing: the Chat-downstream adapters
// own the silent-abort contract, so an Abort-side write would duplicate
// or invent a nonstandard error chunk. No-op before commitment, after a
// writer failure, or when called twice.
func (e *ChatEventEncoder) Abort(w io.Writer, flush func()) {
	if !e.committed || e.aborted || e.writeFailed {
		return
	}
	e.aborted = true
}

func chatEncodeSemantic(msg string) *EncodeError {
	return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: msg}, WriteFailed: false}
}

func chatEncodeWriteFailed() *EncodeError {
	return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Downstream write failed"}, WriteFailed: true}
}

// Encode renders one lifecycle-validated event in order. FailureEvent is
// never passed here by the pipeline (it finalizes through Abort only);
// a defensive refusal is returned if it arrives.
func (e *ChatEventEncoder) Encode(w io.Writer, flush func(), ev ResponseEvent) *EncodeError {
	if e.writeFailed || e.aborted {
		return chatEncodeWriteFailed()
	}
	switch v := ev.(type) {
	case ResponseStart:
		return e.encodeStart(w, flush, v)
	case ResponseModelEvent:
		return e.encodeModel(v)
	case ProviderIdentityEvent:
		return e.encodeIdentity(v)
	case BlockOpen:
		return e.encodeBlockOpen(w, flush, v)
	case TextFragment:
		return e.encodeText(w, flush, v)
	case ArgsFragment:
		return e.encodeArgs(w, flush, v)
	case CustomDelta:
		return e.encodeCustom(w, flush, v)
	case BlockClose:
		return e.encodeBlockClose(w, flush, v)
	case MalformedToolEvent:
		e.malformed++
		return chatEncodeSemantic(`Upstream response feature "response.choices[0].message.tool_calls" has no Chat representation`)
	case UsageEvent:
		return e.encodeUsage(v)
	case CompletionEvent:
		return e.encodeCompletion(w, flush, v)
	case FailureEvent:
		return chatEncodeSemantic("Upstream stream reported an error")
	default:
		return chatEncodeSemantic("Upstream sent an unknown response event")
	}
}

func (e *ChatEventEncoder) encodeStart(w io.Writer, flush func(), v ResponseStart) *EncodeError {
	if e.hasStart {
		return chatEncodeSemantic("Upstream sent a duplicate response start")
	}
	if v.Source == "" || string(v.ID) == "" {
		return chatEncodeSemantic("Upstream returned an invalid response")
	}
	// Chat still requires a model at start; Responses may truthfully
	// start without one (provider reports it later via
	// ResponseModelEvent), like the accepted Anthropic/Gemini absence
	// seam. No fake fallback is invented here.
	if v.Source != DialectGemini && v.Source != DialectAnthropic && v.Source != DialectResponses && v.Model == "" {
		return chatEncodeSemantic("Upstream returned an invalid response")
	}
	e.source = v.Source
	e.respID = v.ID
	e.model = v.Model
	e.idOrigin, e.modelOrigin = v.IDOrigin, v.ModelOrigin
	if e.idOrigin == "" {
		e.idOrigin = IdentityProvider
	}
	if e.modelOrigin == "" {
		e.modelOrigin = IdentityProvider
	}
	if (e.idOrigin == IdentityRouterMinted || e.modelOrigin == IdentityRouterMinted) && v.Source != DialectGemini {
		return chatEncodeSemantic("Upstream response source does not match the requested source")
	}
	e.hasStart = true
	if v.InitialUsage != nil {
		// Metadata-only representability gate for the start snapshot.
		// Initial totals are never substituted for terminal accounting;
		// only unmapped nonzero buckets refuse here.
		if rerr := CheckUsageRepresentable(*v.InitialUsage, v.Source, DialectChat); rerr != nil {
			return &EncodeError{Err: rerr, WriteFailed: false}
		}
	}
	return e.ensureStarted(w, flush)
}

// encodeModel consumes the typed provider-model report metadata-only:
// it updates the actual upstream model for the terminal policy
// summary and writes zero downstream bytes, leaving emitted wrapper
// IDs and the public alias untouched. Ordering and drift are enforced
// value-free: meta before start, empty reports, drift after the model
// is known, and reports after completion all fail.
func (e *ChatEventEncoder) encodeModel(v ResponseModelEvent) *EncodeError {
	if !e.hasStart {
		return chatEncodeSemantic("Upstream sent content before the response start")
	}
	if e.done {
		return chatEncodeSemantic("Upstream sent events after completion")
	}
	if v.Model == "" || strings.TrimSpace(v.Model) == "" {
		return chatEncodeSemantic("Upstream returned an invalid response")
	}
	if e.model == "" {
		e.model = v.Model
		e.modelOrigin = IdentityProvider
		return nil
	}
	if e.model != v.Model {
		return chatEncodeSemantic("Upstream stream changed its response identity")
	}
	return nil
}

// encodeIdentity consumes the late provider responseId metadata-only:
// it updates the upstream ID for the terminal policy summary and
// writes zero downstream bytes, leaving minted wrapper IDs and the
// public alias untouched. At-most-once, pre-terminal only, started
// only; router origins are valid only for the Gemini source.
func (e *ChatEventEncoder) encodeIdentity(v ProviderIdentityEvent) *EncodeError {
	if !e.hasStart {
		return chatEncodeSemantic("Upstream sent content before the response start")
	}
	if e.done {
		return chatEncodeSemantic("Upstream sent events after completion")
	}
	if e.sawIdentity {
		return chatEncodeSemantic("Upstream sent a duplicate response start")
	}
	if !validOpaqueID(string(v.ID)) || len(string(v.ID)) > e.lim.MaxArgsChars {
		return chatEncodeSemantic("Upstream returned an invalid response")
	}
	if e.source != DialectGemini {
		return chatEncodeSemantic("Upstream response source does not match the requested source")
	}
	if e.idOrigin == IdentityProvider {
		return chatEncodeSemantic("Upstream returned conflicting response identities")
	}
	e.sawIdentity = true
	e.respID = v.ID
	e.idOrigin = IdentityProvider
	return nil
}

func (e *ChatEventEncoder) encodeBlockOpen(w io.Writer, flush func(), v BlockOpen) *EncodeError {
	if !e.hasStart {
		return chatEncodeSemantic("Upstream sent content before the response start")
	}
	if _, dup := e.blocks[v.Item]; dup {
		return chatEncodeSemantic("Upstream opened a duplicate response block")
	}
	if len(e.blocks) >= e.lim.MaxStreamBlocks {
		return chatEncodeSemantic("Upstream stream exceeds the content block bound")
	}
	switch v.Kind {
	case BlockText, BlockRefusal:
		if v.Call != "" || v.Name != "" {
			return chatEncodeSemantic("Upstream text block carries a tool identity")
		}
		// SourceIndex is typed provenance only and never a tool
		// identity: indexed text/refusal from Anthropic or Responses
		// sources is valid here (Chat indices stay dense arrival
		// order, never sorted by source index).
		e.blocks[v.Item] = &chatEventBlock{kind: v.Kind}
		e.order = append(e.order, v.Item)
	case BlockTool, BlockCustom:
		if v.Call == "" || v.Name == "" {
			return chatEncodeSemantic("Upstream tool call is missing its identity")
		}
		// Count tool calls against the target bound at open time.
		tools := 0
		for _, b := range e.blocks {
			if b.kind == BlockTool || b.kind == BlockCustom {
				tools++
			}
		}
		if tools+1 > e.lim.MaxToolCalls {
			return chatEncodeSemantic("Upstream tool call count exceeds the size bound")
		}
		// Chat destination passes tool names through (proven:
		// Chat->Gemini passes "has space", Gemini->Chat preserves);
		// the 1..64 grammar is enforced only for Anthropic/Responses
		// destinations, so no name-grammar refusal here.
		callOrigin := v.CallOrigin
		if callOrigin == "" {
			callOrigin = IdentityProvider
		}
		if callOrigin == IdentityRouterMinted {
			if e.source != DialectGemini {
				return chatEncodeSemantic("Upstream response source does not match the requested source")
			}
			e.minted[v.Call] = struct{}{}
		}
		e.blocks[v.Item] = &chatEventBlock{kind: v.Kind, callID: v.Call, name: v.Name, chatIdx: e.nextChatIdx}
		e.nextChatIdx++
		e.order = append(e.order, v.Item)
		if v.Kind == BlockCustom {
			e.customBufs[v.Item] = &strings.Builder{}
		}
	default:
		return chatEncodeSemantic("Upstream response block has no representation")
	}
	return e.ensureStarted(w, flush)
}

func (e *ChatEventEncoder) encodeText(w io.Writer, flush func(), v TextFragment) *EncodeError {
	blk, ok := e.blocks[v.Item]
	if !ok || blk.closed || (blk.kind != BlockText && blk.kind != BlockRefusal) {
		return chatEncodeSemantic("Upstream sent a delta for a closed or unopened block")
	}
	delta := map[string]any{"content": v.Text}
	if blk.kind == BlockRefusal {
		delta = map[string]any{"refusal": v.Text}
	}
	return e.writeDelta(w, flush, delta)
}

func (e *ChatEventEncoder) encodeArgs(w io.Writer, flush func(), v ArgsFragment) *EncodeError {
	blk, ok := e.blocks[v.Item]
	if !ok || blk.closed || blk.kind != BlockTool {
		return chatEncodeSemantic("Upstream sent a delta for a closed or unopened block")
	}
	if v.Fragment == "" {
		return nil
	}
	// Preserve source item order when a custom call opened earlier is
	// still pending: function fragments behind it buffer (bounded)
	// instead of emitting ahead of the custom close. Function-only
	// generations never buffer and stay fully incremental.
	if e.blockedByEarlierCustom(v.Item) {
		pend, ok := e.funcBufs[v.Item]
		if !ok {
			pend = &pendingFuncArgs{}
			e.funcBufs[v.Item] = pend
		}
		if pend.bytes+len(v.Fragment) > e.lim.MaxArgsChars ||
			e.customTotal+e.funcTotal+len(v.Fragment) > e.lim.MaxStreamArgsChars {
			return chatEncodeSemantic("Upstream tool call exceeds the size bound")
		}
		pend.frags = append(pend.frags, v.Fragment)
		pend.bytes += len(v.Fragment)
		e.funcTotal += len(v.Fragment)
		return nil
	}
	return e.writeFunctionArgs(w, flush, blk, v.Fragment)
}

func (e *ChatEventEncoder) writeFunctionArgs(w io.Writer, flush func(), blk *chatEventBlock, frag string) *EncodeError {
	if !blk.emitted {
		blk.emitted = true
		return e.writeDelta(w, flush, map[string]any{
			"tool_calls": []any{map[string]any{
				"index": blk.chatIdx, "id": string(blk.callID), "type": "function",
				"function": map[string]any{"name": blk.name, "arguments": frag},
			}},
		})
	}
	return e.writeDelta(w, flush, map[string]any{
		"tool_calls": []any{map[string]any{
			"index": blk.chatIdx,
			"function": map[string]any{
				"arguments": frag,
			},
		}},
	})
}

func (e *ChatEventEncoder) encodeCustom(w io.Writer, flush func(), v CustomDelta) *EncodeError {
	blk, ok := e.blocks[v.Item]
	if !ok || blk.closed || blk.kind != BlockCustom {
		return chatEncodeSemantic("Upstream sent a delta for a closed or unopened block")
	}
	// Buffer custom input for the single buffered Chat custom delta at
	// close. Bounds mirror the source decoder: per-field MaxArgsChars
	// and aggregate MaxStreamArgsChars. Empty deltas emit nothing.
	if v.Delta == "" {
		return nil
	}
	buf, ok := e.customBufs[v.Item]
	if !ok {
		buf = &strings.Builder{}
		e.customBufs[v.Item] = buf
	}
	if buf.Len()+len(v.Delta) > e.lim.MaxArgsChars ||
		e.customTotal+e.funcTotal+len(v.Delta) > e.lim.MaxStreamArgsChars {
		return chatEncodeSemantic("Upstream tool call exceeds the size bound")
	}
	buf.WriteString(v.Delta)
	e.customTotal += len(v.Delta)
	return nil
}

func (e *ChatEventEncoder) encodeBlockClose(w io.Writer, flush func(), v BlockClose) *EncodeError {
	if !e.hasStart {
		return chatEncodeSemantic("Upstream sent content before the response start")
	}
	blk, ok := e.blocks[v.Item]
	if !ok || blk.closed {
		return chatEncodeSemantic("Upstream closed a block that was not open")
	}
	blk.closed = true
	switch blk.kind {
	case BlockTool:
		if e.blockedByEarlierCustom(v.Item) {
			if e.funcBufs[v.Item] == nil {
				e.funcBufs[v.Item] = &pendingFuncArgs{}
			}
			return nil
		}
		if !blk.emitted {
			return e.writeFunctionArgs(w, flush, blk, "{}")
		}
	case BlockCustom:
		return e.flushDeferredTools(w, flush)
	}
	return nil
}

// An earlier pending custom call delays later tool identities. Text and
// tool fragments outside that barrier retain their incremental behavior.
func (e *ChatEventEncoder) blockedByEarlierCustom(item ItemID) bool {
	for _, id := range e.order {
		if id == item {
			return false
		}
		if b := e.blocks[id]; b.kind == BlockCustom && !b.emitted {
			return true
		}
	}
	return false
}

func (e *ChatEventEncoder) flushDeferredTools(w io.Writer, flush func()) *EncodeError {
	for _, id := range e.order {
		blk := e.blocks[id]
		switch blk.kind {
		case BlockCustom:
			if blk.emitted {
				continue
			}
			if !blk.closed {
				return nil
			}
			input := e.customBufs[id].String()
			blk.emitted = true
			if rerr := e.writeDelta(w, flush, map[string]any{
				"tool_calls": []any{map[string]any{
					"index": blk.chatIdx, "id": string(blk.callID), "type": "custom",
					"custom": map[string]any{"name": blk.name, "input": input},
				}},
			}); rerr != nil {
				return rerr
			}
			delete(e.customBufs, id)
			e.customTotal -= len(input)
		case BlockTool:
			pend := e.funcBufs[id]
			if pend == nil {
				continue
			}
			for _, frag := range pend.frags {
				if rerr := e.writeFunctionArgs(w, flush, blk, frag); rerr != nil {
					return rerr
				}
			}
			if blk.closed && !blk.emitted {
				if rerr := e.writeFunctionArgs(w, flush, blk, "{}"); rerr != nil {
					return rerr
				}
			}
			delete(e.funcBufs, id)
			e.funcTotal -= pend.bytes
		}
	}
	return nil
}

func (e *ChatEventEncoder) encodeUsage(v UsageEvent) *EncodeError {
	if !v.Terminal() {
		return nil
	}
	if e.usage != nil {
		return chatEncodeSemantic("Upstream stream sent duplicate usage")
	}
	u := v.Usage
	e.usage = &u
	return nil
}

// encodeCompletion validates the truthful event summary against the
// shared destination contract, then emits the finish chunk, the optional
// separate usage chunk, and [DONE]. The summary reuses CheckRepresentable
// plus the shared finish/usage mapping; unretained text/argument bytes
// are never inspected by the Chat gate, so text/refusal items carry empty
// placeholders and tool items carry EmptyArgs while identities, kinds,
// usage, finish, order, and malformed counts are exact. Custom items
// carry an empty input placeholder (content is buffered per-block, not
// in the gate).
func (e *ChatEventEncoder) encodeCompletion(w io.Writer, flush func(), v CompletionEvent) *EncodeError {
	if !e.hasStart {
		return chatEncodeSemantic("Upstream sent content before the response start")
	}
	if e.done {
		return chatEncodeSemantic("Upstream sent events after completion")
	}
	if e.usage == nil {
		return chatEncodeSemantic("Upstream stream ended without authoritative usage")
	}
	finish := v.Finish
	if rerr := finish.validate(); rerr != nil {
		return &EncodeError{Err: rerr, WriteFailed: false}
	}
	items := make([]ResponseItem, 0, len(e.order))
	for _, item := range e.order {
		blk := e.blocks[item]
		switch blk.kind {
		case BlockText:
			items = append(items, TextItem{ID: item})
		case BlockRefusal:
			items = append(items, RefusalItem{ID: item})
		case BlockTool:
			items = append(items, ToolCallItem{ID: item, CallID: blk.callID, Name: blk.name, Arguments: EmptyArgs()})
		case BlockCustom:
			items = append(items, CustomCallItem{ID: item, CallID: blk.callID, Name: blk.name, Input: ""})
		default:
			return chatEncodeSemantic("Upstream returned an invalid response")
		}
	}
	summary := Response{
		ID:                e.respID,
		Model:             e.model,
		Source:            e.source,
		Items:             items,
		Usage:             *e.usage,
		Finish:            finish,
		MalformedTools:    e.malformed,
		IDOrigin:          e.idOrigin,
		ModelOrigin:       e.modelOrigin,
		RouterMintedCalls: e.minted,
	}
	if rerr := CheckRepresentable(summary, DialectChat); rerr != nil {
		return &EncodeError{Err: rerr, WriteFailed: false}
	}
	mapped, rerr := chatResponseFinish(summary)
	if rerr != nil {
		return &EncodeError{Err: rerr, WriteFailed: false}
	}
	usageMap := chatResponseUsage(summary)
	if rerr := e.ensureStarted(w, flush); rerr != nil {
		return rerr
	}
	if rerr := e.writeFinish(w, flush, mapped); rerr != nil {
		return rerr
	}
	if e.includeUsage {
		if rerr := e.writeUsageChunk(w, flush, usageMap); rerr != nil {
			return rerr
		}
	}
	if rerr := e.writeDONE(w, flush); rerr != nil {
		return rerr
	}
	e.done = true
	return nil
}

func (e *ChatEventEncoder) baseChunk() map[string]any {
	payload := map[string]any{
		"id": e.id, "object": "chat.completion.chunk", "created": e.created,
		"model": e.publicModel,
		"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{}, "finish_reason": nil,
		}},
	}
	if e.includeUsage {
		payload["usage"] = nil
	}
	return payload
}

func (e *ChatEventEncoder) ensureStarted(w io.Writer, flush func()) *EncodeError {
	// A validated source start commits the role chunk immediately, so
	// downstream flush failure cancels upstream even before content arrives.
	payload := e.baseChunk()
	choices := payload["choices"].([]any)
	choice := choices[0].(map[string]any)
	choice["delta"] = map[string]any{"role": "assistant", "content": ""}
	// ensureStarted is idempotent via committed: the role chunk emits
	// exactly once, on the first call.
	if e.committed {
		return nil
	}
	return e.writeFrame(w, flush, payload)
}

func (e *ChatEventEncoder) writeDelta(w io.Writer, flush func(), delta map[string]any) *EncodeError {
	if rerr := e.ensureStarted(w, flush); rerr != nil {
		return rerr
	}
	payload := e.baseChunk()
	choices := payload["choices"].([]any)
	choice := choices[0].(map[string]any)
	choice["delta"] = delta
	return e.writeFrame(w, flush, payload)
}

func (e *ChatEventEncoder) writeFinish(w io.Writer, flush func(), finish string) *EncodeError {
	payload := e.baseChunk()
	choices := payload["choices"].([]any)
	choice := choices[0].(map[string]any)
	choice["delta"] = map[string]any{}
	choice["finish_reason"] = finish
	return e.writeFrame(w, flush, payload)
}

func (e *ChatEventEncoder) writeUsageChunk(w io.Writer, flush func(), usage map[string]any) *EncodeError {
	payload := map[string]any{
		"id": e.id, "object": "chat.completion.chunk", "created": e.created,
		"model": e.publicModel, "choices": []any{}, "usage": usage,
	}
	return e.writeFrame(w, flush, payload)
}

func (e *ChatEventEncoder) writeDONE(w io.Writer, flush func()) *EncodeError {
	const done = "data: [DONE]\n\n"
	if len(done) > e.lim.MaxFrameBytes {
		return chatEncodeSemantic("Downstream response exceeds the size bound")
	}
	if _, err := w.Write([]byte(done)); err != nil {
		e.writeFailed = true
		return chatEncodeWriteFailed()
	}
	if flush != nil {
		flush()
	}
	return nil
}

// writeFrame marshals one downstream chunk and writes it with its SSE
// framing prefix, bounded prewrite. Full encoded frames never exceed
// MaxFrameBytes; oversize frames fail before any write.
func (e *ChatEventEncoder) writeFrame(w io.Writer, flush func(), payload map[string]any) *EncodeError {
	data, err := json.Marshal(payload)
	if err != nil {
		return &EncodeError{Err: &RouterError{Status: 500, Code: "encoding_error", Msg: "failed to encode downstream event"}, WriteFailed: false}
	}
	var buf bytes.Buffer
	buf.WriteString("data: ")
	buf.Write(data)
	buf.WriteString("\n\n")
	if buf.Len() > e.lim.MaxFrameBytes {
		return chatEncodeSemantic("Downstream response exceeds the size bound")
	}
	if _, err := w.Write(buf.Bytes()); err != nil {
		e.writeFailed = true
		return chatEncodeWriteFailed()
	}
	e.committed = true
	if flush != nil {
		flush()
	}
	return nil
}
