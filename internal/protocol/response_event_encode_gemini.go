// Shared Gemini target stream encoder: semantic events -> Gemini SSE /
// incremental JSON array.
//
// This encoder is source-neutral: it consumes only lifecycle-validated
// semantic events (never source JSON, never a pair translator, never a
// Chat JSON intermediate, never a map semantic IR) and renders one
// Gemini GenerateContentResponse per downstream frame.
//
// Event/state contract:
//
//   - Pre-commit silence: ResponseStart, BlockOpen, BlockClose and
//     UsageEvent emit zero bytes. No start/role Gemini frames are
//     invented; downstream commitment happens on the first text, tool,
//     or terminal frame only.
//   - Text is immediate: TextFragment renders one Gemini object carrying
//     the text part and flushes, with no retention. A later indexed
//     text fragment that crosses an earlier unfinished text/tool block
//     or a pending completed call refuses before emitting; it is never
//     flattened or reordered.
//   - Tool calls release promptly at BlockClose in reviewed logical
//     (SourceIndex) order: a completed call held behind a smaller-index
//     open block waits; it releases as soon as that block closes. Each
//     call emits exactly once as a complete functionCall object and is
//     marked emitted so the terminal never repeats it. Parallel calls
//     closing out of order release in logical order.
//   - Tool arguments buffer ONLY as needed for one complete Gemini
//     functionCall object, in the target-owned TerminalBuffer under the
//     inherited bounds (MaxArgsChars per call, MaxStreamArgsChars
//     aggregate, MaxToolCalls calls). Empty source-absent arguments
//     render as exact "{}" bytes; validated objects keep exact bigint
//     literals; opaque call IDs echo verbatim.
//   - Terminal assembly: CompletionEvent renders the single terminal
//     object with only the unemitted functionCall parts (already
//     released calls are never repeated), the mapped finishReason and
//     the authoritative usageMetadata. MalformedToolEvent is ordinary
//     malformed-call provenance: the call is suppressed and the terminal
//     carries the native MALFORMED_FUNCTION_CALL finish (no STOP, no
//     fabricated functionCall); valid peers are kept. The full semantic
//     policy summary (all calls) drives CheckRepresentable; the wire
//     terminal carries only unemitted parts. Accounting and finish reuse
//     the shared source-provenance target policy (CheckRepresentable,
//     geminiResponseFinish, geminiResponseUsage), never a second copy.
//     No EncodeGeminiResponse empty-completion text is injected into an
//     already-streamed terminal.
//   - Source-specific name policy (Anthropic tool-name grammar) is
//     checked before an early call is written, not only at the terminal.
//   - Failure ownership: Abort marks the stream failed and stops success,
//     but emits NOTHING itself. The server committer
//     (copyGeminiDownstream) owns the single sanitized post-commit error
//     frame; an Abort-side write would duplicate it. Write failures are
//     sticky EncodeErrors: no extra writes, no further reads, no Abort.
package protocol

import (
	"bytes"
	"encoding/json"
	"io"
	"sort"
)

// GeminiEventEncoder encodes validated response events into one Gemini
// downstream framing: SSE data frames (sse=true, no [DONE] marker) or
// one incrementally written JSON array (sse=false: `[` on the first
// element, `,\r\n` separators, `]` only after a valid terminal).
// One instance serves one generation; translators create a fresh
// encoder per Translate call.
type GeminiEventEncoder struct {
	publicModel string
	lim         Limits
	sse         bool

	committed bool
	failed    bool
	wroteAny  bool
	done      bool

	respID string
	hasID  bool
	model  string
	source Dialect
	// startID/startOrigin preserve the start values for mid-stream
	// responseId echo. Only the terminal sees the final values.
	startID     string
	startOrigin IdentityOrigin
	// idOrigin/modelOrigin mirror the upstream start provenance with
	// late promotions applied. Empty reads as provider.
	idOrigin    IdentityOrigin
	modelOrigin IdentityOrigin
	// sawIdentity latches the at-most-once ProviderIdentityEvent.
	sawIdentity bool
	// minted tracks router-minted call IDs for the terminal summary.
	minted map[CallID]struct{}

	order  []ItemID
	blocks map[ItemID]*geminiBlock

	buf *TerminalBuffer

	malformed int

	usage  *ResponseUsage
	finish *Finish

	hasEmitted bool
	maxEmitted int
}

// geminiBlock is the encoder's per-block ordering record. Argument bytes
// live in the TerminalBuffer, never here.
type geminiBlock struct {
	kind      BlockKind
	call      CallID
	name      string
	index     int
	hasIndex  bool
	closed    bool
	emitted   bool
	malformed bool
}

// NewGeminiEventEncoder creates the Gemini target encoder for one
// generation. publicModel stamps modelVersion on every emitted object;
// lim bounds every encoded frame/element prewrite; sse selects the
// downstream framing.
func NewGeminiEventEncoder(publicModel string, lim Limits, sse bool) *GeminiEventEncoder {
	return &GeminiEventEncoder{
		publicModel: publicModel,
		lim:         lim,
		sse:         sse,
		blocks:      map[ItemID]*geminiBlock{},
		buf:         NewTerminalBuffer(lim),
		minted:      map[CallID]struct{}{},
	}
}

// Committed reports whether any downstream frame was written.
func (e *GeminiEventEncoder) Committed() bool { return e.committed }

// Abort marks the stream failed and stops success. It emits nothing:
// the server committer owns the single post-commit error frame, so an
// Abort-side write would duplicate it.
func (e *GeminiEventEncoder) Abort(w io.Writer, flush func()) { e.failed = true }

func geminiEncodeSemantic(rerr *RouterError) *EncodeError {
	return &EncodeError{Err: rerr, WriteFailed: false}
}

func geminiEncodeWriteFailed(rerr *RouterError) *EncodeError {
	return &EncodeError{Err: rerr, WriteFailed: true}
}

// Encode renders one lifecycle-validated event into the Gemini
// downstream. Events arrive in pipeline order; FailureEvent never
// arrives here (the pipeline finalizes it through Abort only).
func (e *GeminiEventEncoder) Encode(w io.Writer, flush func(), ev ResponseEvent) *EncodeError {
	if e.failed {
		return geminiEncodeWriteFailed(&RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Downstream write failed"})
	}
	switch v := ev.(type) {
	case ResponseStart:
		if e.hasID {
			return geminiEncodeSemantic(&RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent a duplicate response start"})
		}
		e.respID, e.hasID, e.model, e.source = string(v.ID), true, v.Model, v.Source
		e.idOrigin, e.modelOrigin = v.IDOrigin, v.ModelOrigin
		if e.idOrigin == "" {
			e.idOrigin = IdentityProvider
		}
		if e.modelOrigin == "" {
			e.modelOrigin = IdentityProvider
		}
		if (e.idOrigin == IdentityRouterMinted || e.modelOrigin == IdentityRouterMinted) && v.Source != DialectGemini {
			return geminiEncodeSemantic(&RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream response source does not match the requested source"})
		}
		e.startID, e.startOrigin = string(v.ID), e.idOrigin
		return nil
	case ResponseModelEvent:
		return e.encodeModel(v)
	case ProviderIdentityEvent:
		return e.encodeIdentity(v)
	case BlockOpen:
		return e.onBlockOpen(v)
	case TextFragment:
		return e.onText(w, flush, v)
	case ArgsFragment:
		if rerr := e.buf.ObserveArgs(v.Item, v.Fragment); rerr != nil {
			return geminiEncodeSemantic(rerr)
		}
		return nil
	case MalformedToolEvent:
		blk, ok := e.blocks[v.Item]
		if !ok || blk.kind != BlockTool {
			return geminiEncodeSemantic(&RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent a delta for a closed or unopened block"})
		}
		if !blk.malformed {
			blk.malformed = true
			e.malformed++
		}
		return nil
	case BlockClose:
		return e.onBlockClose(w, flush, v)
	case UsageEvent:
		if v.Terminal() {
			usage := v.Usage
			e.usage = &usage
		}
		return nil
	case CompletionEvent:
		return e.onCompletion(w, flush, v)
	default:
		return geminiEncodeSemantic(&RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent an unknown response event"})
	}
}

// encodeModel consumes the typed provider-model report metadata-only:
// it updates the actual upstream model for the terminal policy
// summary and writes zero downstream bytes, leaving the echoed
// responseId and the public modelVersion alias untouched.
func (e *GeminiEventEncoder) encodeModel(v ResponseModelEvent) *EncodeError {
	if !e.hasID {
		return geminiEncodeSemantic(&RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent content before the response start"})
	}
	if e.done {
		return geminiEncodeSemantic(&RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent events after completion"})
	}
	if v.Model == "" {
		return geminiEncodeSemantic(&RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"})
	}
	if e.model == "" {
		e.model = v.Model
		e.modelOrigin = IdentityProvider
		return nil
	}
	if e.model != v.Model {
		return geminiEncodeSemantic(&RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream changed its response identity"})
	}
	return nil
}

// encodeIdentity consumes the late provider responseId metadata-only.
// Mid-stream echo keeps the start value; only the terminal sees the
// final values. At-most-once, pre-terminal only.
func (e *GeminiEventEncoder) encodeIdentity(v ProviderIdentityEvent) *EncodeError {
	if !e.hasID {
		return geminiEncodeSemantic(&RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent content before the response start"})
	}
	if e.done {
		return geminiEncodeSemantic(&RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent events after completion"})
	}
	if e.sawIdentity {
		return geminiEncodeSemantic(&RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent a duplicate response start"})
	}
	if !validOpaqueID(string(v.ID)) || len(string(v.ID)) > e.lim.MaxArgsChars {
		return geminiEncodeSemantic(&RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"})
	}
	if e.source != DialectGemini {
		return geminiEncodeSemantic(&RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream response source does not match the requested source"})
	}
	if e.idOrigin == IdentityProvider {
		return geminiEncodeSemantic(&RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned conflicting response identities"})
	}
	e.sawIdentity = true
	e.respID = string(v.ID)
	e.idOrigin = IdentityProvider
	return nil
}

func (e *GeminiEventEncoder) onBlockOpen(v BlockOpen) *EncodeError {
	if _, dup := e.blocks[v.Item]; dup {
		return geminiEncodeSemantic(&RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream opened a duplicate response block"})
	}
	switch v.Kind {
	case BlockText:
		blk := &geminiBlock{kind: BlockText}
		if v.SourceIndex != nil {
			if *v.SourceIndex < 0 {
				return geminiEncodeSemantic(&RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"})
			}
			if e.hasEmitted && *v.SourceIndex <= e.maxEmitted {
				return geminiEncodeSemantic(&RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream reused a content block index"})
			}
			blk.index, blk.hasIndex = *v.SourceIndex, true
		}
		e.blocks[v.Item] = blk
		e.order = append(e.order, v.Item)
		return nil
	case BlockRefusal:
		// Gemini has no refusal form; folding refusal into text would
		// misreport the generation, so fail closed like the complete
		// row gate (CheckRepresentable refuses refusal for Gemini).
		return geminiEncodeSemantic(&RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream response feature \"response.choices[0].message.refusal\" has no Gemini representation"})
	case BlockTool:
		if v.Call == "" || v.Name == "" {
			return geminiEncodeSemantic(&RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call is missing its identity"})
		}
		callOrigin := v.CallOrigin
		if callOrigin == "" {
			callOrigin = IdentityProvider
		}
		if callOrigin == IdentityRouterMinted {
			if e.source != DialectGemini {
				return geminiEncodeSemantic(&RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream response source does not match the requested source"})
			}
			e.minted[v.Call] = struct{}{}
		}
		blk := &geminiBlock{kind: BlockTool, call: v.Call, name: v.Name}
		if v.SourceIndex != nil {
			if *v.SourceIndex < 0 {
				return geminiEncodeSemantic(&RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"})
			}
			if e.hasEmitted && *v.SourceIndex <= e.maxEmitted {
				return geminiEncodeSemantic(&RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream reused a content block index"})
			}
			blk.index, blk.hasIndex = *v.SourceIndex, true
		}
		e.blocks[v.Item] = blk
		e.order = append(e.order, v.Item)
		return nil
	default:
		return geminiEncodeSemantic(&RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream response block has no representation"})
	}
}

func (e *GeminiEventEncoder) onText(w io.Writer, flush func(), v TextFragment) *EncodeError {
	if v.Text == "" {
		return nil
	}
	blk, ok := e.blocks[v.Item]
	if !ok || blk.closed || blk.kind != BlockText {
		return geminiEncodeSemantic(&RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent a delta for a closed or unopened block"})
	}
	if blk.hasIndex && e.lowerBlocksText(blk.index) {
		return geminiEncodeSemantic(&RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream stream text precedes an incomplete block with no Gemini representation"})
	}
	if eerr := e.writeObject(w, flush, e.textPayload(v.Text)); eerr != nil {
		return eerr
	}
	blk.emitted = true
	if blk.hasIndex && (!e.hasEmitted || blk.index > e.maxEmitted) {
		e.hasEmitted, e.maxEmitted = true, blk.index
	}
	return nil
}

// lowerBlocksText reports whether an open or pending-completed block with
// a smaller logical index would be crossed by emitting content at idx.
func (e *GeminiEventEncoder) lowerBlocksText(idx int) bool {
	for _, item := range e.order {
		b := e.blocks[item]
		if !b.hasIndex || b.index >= idx {
			continue
		}
		if b.closed && b.emitted {
			continue
		}
		return true
	}
	return false
}

func (e *GeminiEventEncoder) onBlockClose(w io.Writer, flush func(), v BlockClose) *EncodeError {
	blk, ok := e.blocks[v.Item]
	if !ok || blk.closed {
		return geminiEncodeSemantic(&RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream closed a block that was not open"})
	}
	blk.closed = true
	if blk.kind == BlockText {
		// An empty text block is resolved at close even though it wrote
		// nothing. It must not hold later completed calls behind a barrier.
		blk.emitted = true
		return e.tryRelease(w, flush)
	}
	// Tool block.
	if blk.malformed {
		// Suppressed, never rendered, but resolved so it never blocks
		// valid peers. Never counted as emitted content for the
		// late-index regression (nothing was written).
		blk.emitted = true
		return e.tryRelease(w, flush)
	}
	if rerr := e.checkToolName(blk.name); rerr != nil {
		return geminiEncodeSemantic(rerr)
	}
	if _, rerr := e.buf.ArgsResult(v.Item); rerr != nil {
		return geminiEncodeSemantic(rerr)
	}
	return e.tryRelease(w, flush)
}

// checkToolName enforces the source-specific tool-name policy before an
// early call is written. Only the Anthropic source carries the grammar
// into Gemini; other sources pass names through (proven by the complete
// row). The terminal CheckRepresentable re-validates the full summary.
func (e *GeminiEventEncoder) checkToolName(name string) *RouterError {
	if e.source == DialectAnthropic {
		if err := validateToolName(name); err != nil {
			return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
		}
	}
	return nil
}

// tryRelease emits completed tool calls in ascending logical order while
// no smaller-index block remains open or pending. Unindexed tools (no
// SourceIndex) emit in arrival order without holding.
func (e *GeminiEventEncoder) tryRelease(w io.Writer, flush func()) *EncodeError {
	for {
		bestItem := ItemID("")
		bestIdx := 0
		bestHasIdx := false
		found := false
		for _, item := range e.order {
			b := e.blocks[item]
			if b.kind != BlockTool || !b.closed || b.emitted || b.malformed {
				continue
			}
			if !b.hasIndex {
				if !found {
					bestItem, bestHasIdx, found = item, false, true
				}
				continue
			}
			if !found || (bestHasIdx && b.index < bestIdx) || !bestHasIdx {
				bestItem, bestIdx, bestHasIdx, found = item, b.index, true, true
			}
		}
		if !found {
			return nil
		}
		if bestHasIdx {
			blocked := false
			for _, item := range e.order {
				b := e.blocks[item]
				if !b.hasIndex || b.index >= bestIdx {
					continue
				}
				if b.closed && b.emitted {
					continue
				}
				blocked = true
				break
			}
			if blocked {
				return nil
			}
		}
		b := e.blocks[bestItem]
		args, rerr := e.buf.ArgsResult(bestItem)
		if rerr != nil {
			return geminiEncodeSemantic(rerr)
		}
		if rerr := e.checkToolName(b.name); rerr != nil {
			return geminiEncodeSemantic(rerr)
		}
		call := map[string]any{"name": b.name, "args": json.RawMessage(args)}
		if b.call != "" {
			call["id"] = string(b.call)
		}
		if eerr := e.writeObject(w, flush, e.toolPayload(call)); eerr != nil {
			return eerr
		}
		b.emitted = true
		if b.hasIndex && (!e.hasEmitted || b.index > e.maxEmitted) {
			e.hasEmitted, e.maxEmitted = true, b.index
		}
	}
}

// textPayload renders one non-terminal Gemini response object carrying
// text parts, byte-identical in shape to the terminal minus
// finishReason and usageMetadata. Mid-stream echo keeps the start
// responseId; router-origin starts omit it.
func (e *GeminiEventEncoder) textPayload(text string) map[string]any {
	candidate := map[string]any{
		"content": map[string]any{"role": "model", "parts": []any{map[string]any{"text": text}}},
		"index":   0,
	}
	out := map[string]any{
		"candidates":   []any{candidate},
		"modelVersion": e.publicModel,
	}
	if e.hasID && e.startOrigin == IdentityProvider && e.startID != "" {
		out["responseId"] = e.startID
	}
	return out
}

// toolPayload renders one non-terminal Gemini response object carrying a
// single complete functionCall part. Mid-stream echo keeps the start
// responseId; router-origin starts omit it.
func (e *GeminiEventEncoder) toolPayload(call map[string]any) map[string]any {
	candidate := map[string]any{
		"content": map[string]any{"role": "model", "parts": []any{map[string]any{"functionCall": call}}},
		"index":   0,
	}
	out := map[string]any{
		"candidates":   []any{candidate},
		"modelVersion": e.publicModel,
	}
	if e.hasID && e.startOrigin == IdentityProvider && e.startID != "" {
		out["responseId"] = e.startID
	}
	return out
}

func (e *GeminiEventEncoder) onCompletion(w io.Writer, flush func(), v CompletionEvent) *EncodeError {
	if e.done {
		return geminiEncodeSemantic(&RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent events after completion"})
	}
	if e.usage == nil {
		return geminiEncodeSemantic(&RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream ended without authoritative usage"})
	}
	finish := v.Finish
	e.finish = &finish
	full, rerr := e.fullPolicyResponse()
	if rerr != nil {
		return geminiEncodeSemantic(rerr)
	}
	if rerr := CheckRepresentable(full, DialectGemini); rerr != nil {
		return geminiEncodeSemantic(rerr)
	}
	term, rerr := e.terminalWireResponse(full)
	if rerr != nil {
		return geminiEncodeSemantic(rerr)
	}
	data, err := json.Marshal(term)
	if err != nil {
		return geminiEncodeSemantic(&RouterError{Status: 500, Code: "encoding_error", Msg: "failed to encode downstream event"})
	}
	if eerr := e.writeRaw(w, flush, data); eerr != nil {
		return eerr
	}
	if !e.sse {
		if _, err := w.Write([]byte("]")); err != nil {
			e.failed = true
			return geminiEncodeWriteFailed(&RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Downstream write failed"})
		}
		if flush != nil {
			flush()
		}
	}
	e.done = true
	return nil
}

// fullPolicyResponse assembles the complete semantic summary (all valid
// calls, emitted or not) for the shared target gate. Malformed calls are
// counted, never rendered.
func (e *GeminiEventEncoder) fullPolicyResponse() (Response, *RouterError) {
	ordered := e.orderedTools()
	var items []ResponseItem
	for _, item := range ordered {
		b := e.blocks[item]
		if b.malformed {
			continue
		}
		args, rerr := e.buf.ArgsResult(item)
		if rerr != nil {
			return Response{}, rerr
		}
		call, rerr := NewToolCallItem(item, b.call, b.name, args)
		if rerr != nil {
			return Response{}, rerr
		}
		items = append(items, call)
	}
	resp, rerr := NewResponse(ResponseID(e.respID), e.model, e.source, items, *e.usage, *e.finish)
	if rerr != nil {
		return Response{}, rerr
	}
	resp.MalformedTools = e.malformed
	resp.IDOrigin = e.idOrigin
	resp.ModelOrigin = e.modelOrigin
	resp.RouterMintedCalls = e.minted
	return resp, nil
}

// orderedTools returns all tool ItemIDs in reviewed logical order:
// indexed tools ascend by SourceIndex, unindexed tools keep arrival
// order after indexed ones (matching the previous terminal sort).
func (e *GeminiEventEncoder) orderedTools() []ItemID {
	ordered := make([]ItemID, 0, len(e.blocks))
	for _, item := range e.order {
		if e.blocks[item].kind == BlockTool {
			ordered = append(ordered, item)
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		ai, aj := e.blocks[ordered[i]], e.blocks[ordered[j]]
		if !ai.hasIndex || !aj.hasIndex {
			if ai.hasIndex != aj.hasIndex {
				return ai.hasIndex
			}
			return false
		}
		return ai.index < aj.index
	})
	return ordered
}

// terminalWireResponse renders the terminal object with only unemitted
// parts: already-released calls are never repeated, and no text is
// duplicated (text streamed earlier). Finish and usage reuse the shared
// helpers on the full policy summary, never a second policy copy.
func (e *GeminiEventEncoder) terminalWireResponse(full Response) (map[string]any, *RouterError) {
	finishReason := "MALFORMED_FUNCTION_CALL"
	if full.MalformedTools == 0 {
		mapped, rerr := geminiResponseFinish(full.Finish)
		if rerr != nil {
			return nil, rerr
		}
		finishReason = mapped
	}
	usage, rerr := geminiResponseUsage(full)
	if rerr != nil {
		return nil, rerr
	}
	var parts []any
	for _, item := range e.orderedTools() {
		b := e.blocks[item]
		if b.malformed || b.emitted {
			continue
		}
		args, rerr := e.buf.ArgsResult(item)
		if rerr != nil {
			return nil, rerr
		}
		call := map[string]any{"name": b.name, "args": json.RawMessage(args)}
		if b.call != "" {
			call["id"] = string(b.call)
		}
		parts = append(parts, map[string]any{"functionCall": call})
		// Mark terminal-carried calls emitted so a repeated completion
		// (rejected by the lifecycle, defensive here) cannot double-emit.
		b.emitted = true
	}
	if parts == nil {
		parts = []any{}
	}
	out := map[string]any{
		"candidates": []any{map[string]any{
			"content":      map[string]any{"role": "model", "parts": parts},
			"index":        0,
			"finishReason": finishReason,
		}},
		"modelVersion": e.publicModel,
	}
	// Terminal sees the final identity; router-origin IDs are never
	// echoed as provider responseId (EncodeGeminiResponse guard).
	if e.hasID && e.idOrigin == IdentityProvider && e.respID != "" {
		out["responseId"] = e.respID
	}
	if usage != nil {
		out["usageMetadata"] = usage
	}
	return out, nil
}

// writeObject marshals one downstream element and writes it with its
// framing prefix, bounded prewrite. Full encoded frames/elements never
// exceed MaxFrameBytes; oversize elements fail before any write.
func (e *GeminiEventEncoder) writeObject(w io.Writer, flush func(), payload map[string]any) *EncodeError {
	data, err := json.Marshal(payload)
	if err != nil {
		return geminiEncodeSemantic(&RouterError{Status: 500, Code: "encoding_error", Msg: "failed to encode downstream event"})
	}
	return e.writeRaw(w, flush, data)
}

func (e *GeminiEventEncoder) writeRaw(w io.Writer, flush func(), data []byte) *EncodeError {
	var prefix, suffix []byte
	if e.sse {
		prefix = []byte("data: ")
		suffix = []byte("\n\n")
	} else if !e.wroteAny {
		prefix = []byte("[")
	} else {
		prefix = []byte(",\r\n")
	}
	if len(prefix)+len(data)+len(suffix) > e.lim.MaxFrameBytes {
		return geminiEncodeSemantic(&RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Downstream frame exceeds the size bound"})
	}
	var buf bytes.Buffer
	buf.Write(prefix)
	buf.Write(data)
	buf.Write(suffix)
	if _, err := w.Write(buf.Bytes()); err != nil {
		e.failed = true
		return geminiEncodeWriteFailed(&RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Downstream write failed"})
	}
	if flush != nil {
		flush()
	}
	e.committed = true
	e.wroteAny = true
	return nil
}
