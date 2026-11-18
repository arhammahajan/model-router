// Shared target stream encoder: semantic events -> Responses SSE.
//
// ResponsesEventEncoder consumes lifecycle-validated semantic events
// source-neutrally: it never parses source JSON, never calls a
// pair translator, and never builds a Chat JSON intermediate. It owns
// only downstream state (commitment, output_index allocation, item
// identities, framing) plus an opt-in bounded TerminalBuffer for the
// terminal envelope reconstruction (Responses requires the full output
// in its completed event). Incremental deltas flush immediately; the
// generation is never withheld.
//
// Wire preserved from the replaced Chat pair translator:
//   - response.created / response.in_progress with usage null,
//   - per text block: added, content_part.added, output_text.delta...,
//   - per tool block: added, function_call_arguments.delta...,
//   - terminal: done events repeating the full content, then
//     response.completed / response.incomplete with authoritative usage.
//
// Sequence numbers are monotonic from zero; output_index allocates in
// emission order; item/part identities stay stable across
// added/delta/done/terminal. Opaque call IDs are preserved verbatim;
// the 1..64 tool-name grammar is enforced here (target-side).
package protocol

import (
	"bytes"
	"encoding/json"
	"io"
)

// ResponsesEventEncoder encodes semantic events into Responses SSE.
type ResponsesEventEncoder struct {
	publicModel string
	lim         Limits

	respID    string
	createdAt int64
	seq       int

	committed   bool
	aborted     bool
	writeFailed bool
	started     bool

	nextIdx int
	order   []ItemID
	blocks  map[ItemID]*responsesBlock

	buf *TerminalBuffer

	source        Dialect
	upstream      ResponseID
	upstreamModel string
	hasStart      bool
	done          bool
	// idOrigin/modelOrigin mirror the upstream start provenance with
	// late promotions applied. Empty reads as provider. Router
	// origins are valid only for the Gemini source.
	idOrigin    IdentityOrigin
	modelOrigin IdentityOrigin
	// sawIdentity latches the at-most-once ProviderIdentityEvent.
	sawIdentity bool
	// minted tracks router-minted call IDs for the terminal summary.
	minted map[CallID]struct{}

	usage     *ResponseUsage
	malformed int

	search *responsesSearchPrelude
}

// responsesBlock tracks one downstream output block in emission order.
type responsesBlock struct {
	kind      BlockKind
	outIdx    int
	msgID     string
	fcID      string
	callID    string
	namespace string
	name      string
}

// NewResponsesEventEncoder creates the Responses target stream encoder
// for one generation.
func NewResponsesEventEncoder(publicModel string, lim Limits) *ResponsesEventEncoder {
	return &ResponsesEventEncoder{
		publicModel: publicModel,
		lim:         lim,
		blocks:      map[ItemID]*responsesBlock{},
		buf:         NewTerminalBuffer(lim),
		minted:      map[CallID]struct{}{},
	}
}

type responsesSearchPrelude struct {
	trace     ResponsesSearchTrace
	id        string
	emitted   bool
	baseUsage ResponseUsage
	budget    *int
}

// SetSearchPrelude reserves the first output item for a completed search.
// Call before encoding the continuation from the upstream model.
func (e *ResponsesEventEncoder) SetSearchPrelude(trace ResponsesSearchTrace, usage ResponseUsage, budget *int) {
	e.search = &responsesSearchPrelude{trace: trace, id: newResponsesID("ws"), baseUsage: usage, budget: budget}
	e.nextIdx = 1
}

func (e *ResponsesEventEncoder) emitSearchPrelude(w io.Writer, flush func()) *EncodeError {
	if e.search == nil || e.search.emitted {
		return nil
	}
	if eerr := e.ensureStarted(w, flush); eerr != nil {
		return eerr
	}
	for _, item := range []struct{ event, status string }{
		{"response.output_item.added", "in_progress"},
		{"response.output_item.done", "completed"},
	} {
		if eerr := e.writeEvent(w, flush, item.event, map[string]any{
			"output_index": 0,
			"item":         renderWebSearchCall(e.search.id, item.status, e.search.trace),
		}); eerr != nil {
			return eerr
		}
	}
	e.search.emitted = true
	return nil
}

// Committed reports whether downstream headers/first bytes were committed.
func (e *ResponsesEventEncoder) Committed() bool { return e.committed }

// Encode encodes one lifecycle-validated event in order.
func (e *ResponsesEventEncoder) Encode(w io.Writer, flush func(), ev ResponseEvent) *EncodeError {
	if e.writeFailed {
		return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Downstream write failed"}, WriteFailed: true}
	}
	switch v := ev.(type) {
	case ResponseStart:
		return e.encodeStart(v)
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
	case MalformedToolEvent:
		e.malformed++
		return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: `Upstream response feature "response.choices[0].message.tool_calls" has no Responses representation`}, WriteFailed: false}
	case BlockClose:
		return nil
	case UsageEvent:
		return e.encodeUsage(v)
	case CompletionEvent:
		return e.encodeCompletion(w, flush, v)
	default:
		return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent an unknown response event"}, WriteFailed: false}
	}
}

// Abort reports an in-band failure after commitment. It emits exactly
// one sanitized response.failed reusing the committed id/model and the
// next sequence number, never a success terminal. Before commitment it
// is a no-op; after a downstream write failure it never writes again.
func (e *ResponsesEventEncoder) Abort(w io.Writer, flush func()) {
	if !e.committed || e.aborted || e.writeFailed {
		return
	}
	e.aborted = true
	resp := map[string]any{
		"id": e.respID, "object": "response",
		"created_at": e.createdAt, "model": e.publicModel,
		"status": "failed",
		"error": map[string]any{
			"code": "server_error", "message": "Upstream stream reported an error",
		},
		"incomplete_details": nil,
		"output":             []any{},
		"usage":              nil,
	}
	payload := map[string]any{"type": "response.failed", "response": resp, "sequence_number": e.seq}
	e.seq++
	var buf bytes.Buffer
	buf.WriteString("event: response.failed\ndata: ")
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	buf.Write(data)
	buf.WriteString("\n\n")
	if buf.Len() > e.lim.MaxFrameBytes {
		return
	}
	if _, err := w.Write(buf.Bytes()); err != nil {
		e.writeFailed = true
		return
	}
	if flush != nil {
		flush()
	}
}

func (e *ResponsesEventEncoder) encodeStart(v ResponseStart) *EncodeError {
	if e.hasStart {
		return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent a duplicate response start"}, WriteFailed: false}
	}
	e.source = v.Source
	e.upstream = v.ID
	e.upstreamModel = v.Model
	e.idOrigin, e.modelOrigin = v.IDOrigin, v.ModelOrigin
	if e.idOrigin == "" {
		e.idOrigin = IdentityProvider
	}
	if e.modelOrigin == "" {
		e.modelOrigin = IdentityProvider
	}
	if (e.idOrigin == IdentityRouterMinted || e.modelOrigin == IdentityRouterMinted) && v.Source != DialectGemini {
		return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream response source does not match the requested source"}, WriteFailed: false}
	}
	e.hasStart = true
	e.respID = newResponsesID("resp")
	e.createdAt = responsesNowUnix()
	return nil
}

// encodeModel consumes the typed provider-model report metadata-only:
// it updates the actual upstream model for the terminal policy
// summary and writes zero downstream bytes, leaving the minted
// response wrapper IDs and the public alias untouched.
func (e *ResponsesEventEncoder) encodeModel(v ResponseModelEvent) *EncodeError {
	if !e.hasStart {
		return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent content before the response start"}, WriteFailed: false}
	}
	if e.done {
		return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent events after completion"}, WriteFailed: false}
	}
	if v.Model == "" {
		return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}, WriteFailed: false}
	}
	if e.upstreamModel == "" {
		e.upstreamModel = v.Model
		e.modelOrigin = IdentityProvider
		return nil
	}
	if e.upstreamModel != v.Model {
		return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream changed its response identity"}, WriteFailed: false}
	}
	return nil
}

func (e *ResponsesEventEncoder) encodeIdentity(v ProviderIdentityEvent) *EncodeError {
	if !e.hasStart {
		return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent content before the response start"}, WriteFailed: false}
	}
	if e.done {
		return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent events after completion"}, WriteFailed: false}
	}
	if e.sawIdentity {
		return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent a duplicate response start"}, WriteFailed: false}
	}
	if !validOpaqueID(string(v.ID)) || len(string(v.ID)) > e.lim.MaxArgsChars {
		return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}, WriteFailed: false}
	}
	if e.source != DialectGemini {
		return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream response source does not match the requested source"}, WriteFailed: false}
	}
	if e.idOrigin == IdentityProvider {
		return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned conflicting response identities"}, WriteFailed: false}
	}
	e.sawIdentity = true
	e.upstream = v.ID
	e.idOrigin = IdentityProvider
	return nil
}

func (e *ResponsesEventEncoder) encodeBlockOpen(w io.Writer, flush func(), v BlockOpen) *EncodeError {
	if _, dup := e.blocks[v.Item]; dup {
		return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream opened a duplicate response block"}, WriteFailed: false}
	}
	if len(e.blocks) >= e.lim.MaxStreamBlocks {
		return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream exceeds the content block bound"}, WriteFailed: false}
	}
	if e.search != nil && (v.Kind == BlockTool || v.Kind == BlockCustom) {
		return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}}
	}
	if eerr := e.emitSearchPrelude(w, flush); eerr != nil {
		return eerr
	}
	blk := &responsesBlock{kind: v.Kind, outIdx: e.nextIdx}
	switch v.Kind {
	case BlockText, BlockRefusal:
		if v.Call != "" || v.Namespace != "" || v.Name != "" {
			return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream text block carries a tool identity"}, WriteFailed: false}
		}
		blk.msgID = newResponsesID("msg")
	case BlockTool, BlockCustom:
		if err := validateToolName(v.Name); err != nil {
			return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}, WriteFailed: false}
		}
		if v.Call == "" {
			return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call is missing its identity"}, WriteFailed: false}
		}
		callOrigin := v.CallOrigin
		if callOrigin == "" {
			callOrigin = IdentityProvider
		}
		if callOrigin == IdentityRouterMinted {
			if e.source != DialectGemini {
				return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream response source does not match the requested source"}, WriteFailed: false}
			}
			e.minted[v.Call] = struct{}{}
		}
		tools := 0
		for _, b := range e.blocks {
			if b.kind == BlockTool || b.kind == BlockCustom {
				tools++
			}
		}
		if tools+1 > e.lim.MaxToolCalls {
			return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call count exceeds the size bound"}, WriteFailed: false}
		}
		if v.Kind == BlockCustom {
			blk.fcID = newResponsesID("ctc")
		} else {
			blk.fcID = newResponsesID("fc")
		}
		blk.callID = string(v.Call)
		blk.namespace = v.Namespace
		blk.name = v.Name
	default:
		return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream response block has no representation"}, WriteFailed: false}
	}
	e.nextIdx++
	e.order = append(e.order, v.Item)
	e.blocks[v.Item] = blk
	if eerr := e.ensureStarted(w, flush); eerr != nil {
		return eerr
	}
	switch v.Kind {
	case BlockText:
		if eerr := e.writeEvent(w, flush, "response.output_item.added", map[string]any{
			"output_index": blk.outIdx,
			"item": map[string]any{
				"type": "message", "id": blk.msgID,
				"status": "in_progress", "role": "assistant", "content": []any{},
			},
		}); eerr != nil {
			return eerr
		}
		return e.writeEvent(w, flush, "response.content_part.added", map[string]any{
			"item_id": blk.msgID, "output_index": blk.outIdx, "content_index": 0,
			"part": map[string]any{
				"type": "output_text", "text": "",
				"annotations": []any{}, "logprobs": []any{},
			},
		})
	case BlockRefusal:
		if eerr := e.writeEvent(w, flush, "response.output_item.added", map[string]any{
			"output_index": blk.outIdx,
			"item": map[string]any{
				"type": "message", "id": blk.msgID,
				"status": "in_progress", "role": "assistant", "content": []any{},
			},
		}); eerr != nil {
			return eerr
		}
		return e.writeEvent(w, flush, "response.content_part.added", map[string]any{
			"item_id": blk.msgID, "output_index": blk.outIdx, "content_index": 0,
			"part": map[string]any{
				"type": "refusal", "refusal": "",
			},
		})
	default:
		if blk.kind == BlockCustom {
			item := map[string]any{
				"type": "custom_tool_call", "id": blk.fcID,
				"call_id": blk.callID, "name": blk.name,
				"input": "", "status": "in_progress",
			}
			if blk.namespace != "" {
				item["namespace"] = blk.namespace
			}
			return e.writeEvent(w, flush, "response.output_item.added", map[string]any{
				"output_index": blk.outIdx,
				"item":         item,
			})
		}
		item := map[string]any{
			"type": "function_call", "id": blk.fcID,
			"call_id": blk.callID, "name": blk.name,
			"arguments": "", "status": "in_progress",
		}
		if blk.namespace != "" {
			item["namespace"] = blk.namespace
		}
		return e.writeEvent(w, flush, "response.output_item.added", map[string]any{
			"output_index": blk.outIdx,
			"item":         item,
		})
	}
}

func (e *ResponsesEventEncoder) encodeText(w io.Writer, flush func(), v TextFragment) *EncodeError {
	blk, ok := e.blocks[v.Item]
	if !ok || (blk.kind != BlockText && blk.kind != BlockRefusal) {
		return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent a delta for a closed or unopened block"}, WriteFailed: false}
	}
	if rerr := e.buf.ObserveText(v.Item, v.Text); rerr != nil {
		return &EncodeError{Err: rerr, WriteFailed: false}
	}
	if blk.kind == BlockRefusal {
		return e.writeEvent(w, flush, "response.refusal.delta", map[string]any{
			"item_id": blk.msgID, "output_index": blk.outIdx, "content_index": 0,
			"delta": v.Text,
		})
	}
	return e.writeEvent(w, flush, "response.output_text.delta", map[string]any{
		"item_id": blk.msgID, "output_index": blk.outIdx, "content_index": 0,
		"delta": v.Text, "logprobs": []any{},
	})
}

func (e *ResponsesEventEncoder) encodeArgs(w io.Writer, flush func(), v ArgsFragment) *EncodeError {
	blk, ok := e.blocks[v.Item]
	if !ok || blk.kind != BlockTool {
		return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent a delta for a closed or unopened block"}, WriteFailed: false}
	}
	if rerr := e.buf.ObserveArgs(v.Item, v.Fragment); rerr != nil {
		return &EncodeError{Err: rerr, WriteFailed: false}
	}
	if v.Fragment == "" {
		return nil
	}
	return e.writeEvent(w, flush, "response.function_call_arguments.delta", map[string]any{
		"item_id": blk.fcID, "output_index": blk.outIdx, "delta": v.Fragment,
	})
}

func (e *ResponsesEventEncoder) encodeCustom(w io.Writer, flush func(), v CustomDelta) *EncodeError {
	blk, ok := e.blocks[v.Item]
	if !ok || blk.kind != BlockCustom {
		return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent a delta for a closed or unopened block"}, WriteFailed: false}
	}
	if rerr := e.buf.ObserveCustom(v.Item, v.Delta); rerr != nil {
		return &EncodeError{Err: rerr, WriteFailed: false}
	}
	if v.Delta == "" {
		return nil
	}
	return e.writeEvent(w, flush, "response.custom_tool_call_input.delta", map[string]any{
		"item_id": blk.fcID, "output_index": blk.outIdx, "delta": v.Delta,
	})
}

func (e *ResponsesEventEncoder) encodeUsage(v UsageEvent) *EncodeError {
	if !v.Terminal() {
		return nil
	}
	if e.search == nil {
		u := v.Usage
		e.usage = &u
		return nil
	}
	agg, rerr := AddSearchUsage(e.search.baseUsage, v.Usage)
	if rerr != nil {
		return &EncodeError{Err: rerr, WriteFailed: false}
	}
	if rerr := CheckSearchOutputBudget(agg, e.search.budget); rerr != nil {
		return &EncodeError{Err: rerr}
	}
	e.usage = &agg
	return nil
}

func (e *ResponsesEventEncoder) encodeCompletion(w io.Writer, flush func(), v CompletionEvent) *EncodeError {
	if !e.hasStart {
		return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent content before the response start"}, WriteFailed: false}
	}
	if e.done {
		return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent events after completion"}, WriteFailed: false}
	}
	if e.usage == nil {
		return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}, WriteFailed: false}
	}
	if e.search != nil && (v.Finish.Kind == FinishToolCalls || e.malformed > 0) {
		return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}}
	}
	resp, rerr := e.assembleResponse(v.Finish)
	if rerr != nil {
		return &EncodeError{Err: rerr, WriteFailed: false}
	}
	if rerr := CheckRepresentable(resp, DialectResponses); rerr != nil {
		return &EncodeError{Err: rerr, WriteFailed: false}
	}
	status, incompleteReason, rerr := responsesResponseStatus(v.Finish)
	if rerr != nil {
		return &EncodeError{Err: rerr, WriteFailed: false}
	}
	if eerr := e.emitSearchPrelude(w, flush); eerr != nil {
		return eerr
	}
	if eerr := e.ensureStarted(w, flush); eerr != nil {
		return eerr
	}
	itemStatus := "completed"
	if status == "incomplete" {
		itemStatus = "incomplete"
	}
	output := make([]any, 0, len(e.order))
	for _, item := range e.order {
		blk := e.blocks[item]
		switch blk.kind {
		case BlockText:
			full := e.buf.TextResult(item)
			var anns any = []any{}
			if e.search != nil {
				cited, rerr := buildSearchAnnotations(full, e.search.trace.Results)
				if rerr != nil {
					return &EncodeError{Err: rerr, WriteFailed: false}
				}
				if len(cited) != 0 {
					anns = cited
				}
			}
			if eerr := e.writeEvent(w, flush, "response.output_text.done", map[string]any{
				"item_id": blk.msgID, "output_index": blk.outIdx, "content_index": 0,
				"text": full, "logprobs": []any{},
			}); eerr != nil {
				return eerr
			}
			if eerr := e.writeEvent(w, flush, "response.content_part.done", map[string]any{
				"item_id": blk.msgID, "output_index": blk.outIdx, "content_index": 0,
				"part": map[string]any{
					"type": "output_text", "text": full,
					"annotations": anns, "logprobs": []any{},
				},
			}); eerr != nil {
				return eerr
			}
			if eerr := e.writeEvent(w, flush, "response.output_item.done", map[string]any{
				"output_index": blk.outIdx,
				"item": map[string]any{
					"type": "message", "id": blk.msgID,
					"status": itemStatus, "role": "assistant",
					"content": []any{map[string]any{
						"type": "output_text", "text": full,
						"annotations": anns, "logprobs": []any{},
					}},
				},
			}); eerr != nil {
				return eerr
			}
			output = append(output, map[string]any{
				"type": "message", "id": blk.msgID,
				"status": itemStatus, "role": "assistant",
				"content": []any{map[string]any{
					"type": "output_text", "text": full,
					"annotations": anns, "logprobs": []any{},
				}},
			})
		case BlockRefusal:
			full := e.buf.TextResult(item)
			if eerr := e.writeEvent(w, flush, "response.refusal.done", map[string]any{
				"item_id": blk.msgID, "output_index": blk.outIdx, "content_index": 0,
				"refusal": full,
			}); eerr != nil {
				return eerr
			}
			if eerr := e.writeEvent(w, flush, "response.content_part.done", map[string]any{
				"item_id": blk.msgID, "output_index": blk.outIdx, "content_index": 0,
				"part": map[string]any{
					"type": "refusal", "refusal": full,
				},
			}); eerr != nil {
				return eerr
			}
			if eerr := e.writeEvent(w, flush, "response.output_item.done", map[string]any{
				"output_index": blk.outIdx,
				"item": map[string]any{
					"type": "message", "id": blk.msgID,
					"status": itemStatus, "role": "assistant",
					"content": []any{map[string]any{
						"type": "refusal", "refusal": full,
					}},
				},
			}); eerr != nil {
				return eerr
			}
			output = append(output, map[string]any{
				"type": "message", "id": blk.msgID,
				"status": itemStatus, "role": "assistant",
				"content": []any{map[string]any{
					"type": "refusal", "refusal": full,
				}},
			})
		case BlockTool:
			raw, rerr := e.buf.ArgsResult(item)
			if rerr != nil {
				return &EncodeError{Err: rerr, WriteFailed: false}
			}
			args := string(raw)
			if eerr := e.writeEvent(w, flush, "response.function_call_arguments.done", map[string]any{
				"item_id": blk.fcID, "output_index": blk.outIdx, "arguments": args,
			}); eerr != nil {
				return eerr
			}
			if eerr := e.writeEvent(w, flush, "response.output_item.done", map[string]any{
				"output_index": blk.outIdx,
				"item": func() map[string]any {
					m := map[string]any{
						"type": "function_call", "id": blk.fcID,
						"call_id": blk.callID, "name": blk.name,
						"arguments": args, "status": itemStatus,
					}
					if blk.namespace != "" {
						m["namespace"] = blk.namespace
					}
					return m
				}(),
			}); eerr != nil {
				return eerr
			}
			output = append(output, func() map[string]any {
				m := map[string]any{
					"type": "function_call", "id": blk.fcID,
					"call_id": blk.callID, "name": blk.name,
					"arguments": args, "status": itemStatus,
				}
				if blk.namespace != "" {
					m["namespace"] = blk.namespace
				}
				return m
			}())
		case BlockCustom:
			full := e.buf.CustomResult(item)
			if eerr := e.writeEvent(w, flush, "response.custom_tool_call_input.done", map[string]any{
				"item_id": blk.fcID, "output_index": blk.outIdx, "input": full,
			}); eerr != nil {
				return eerr
			}
			if eerr := e.writeEvent(w, flush, "response.output_item.done", map[string]any{
				"output_index": blk.outIdx,
				"item": func() map[string]any {
					m := map[string]any{
						"type": "custom_tool_call", "id": blk.fcID,
						"call_id": blk.callID, "name": blk.name,
						"input": full, "status": itemStatus,
					}
					if blk.namespace != "" {
						m["namespace"] = blk.namespace
					}
					return m
				}(),
			}); eerr != nil {
				return eerr
			}
			output = append(output, func() map[string]any {
				m := map[string]any{
					"type": "custom_tool_call", "id": blk.fcID,
					"call_id": blk.callID, "name": blk.name,
					"input": full, "status": itemStatus,
				}
				if blk.namespace != "" {
					m["namespace"] = blk.namespace
				}
				return m
			}())
		default:
			return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream response block has no representation"}, WriteFailed: false}
		}
	}
	usage := responsesResponseUsage(resp)
	terminal := "response.completed"
	if status == "incomplete" {
		terminal = "response.incomplete"
	}
	if e.search != nil {
		output = append([]any{renderWebSearchCall(e.search.id, "completed", e.search.trace)}, output...)
	}
	if eerr := e.writeEvent(w, flush, terminal, map[string]any{
		"response": e.skeleton(status, incompleteReason, output, usage),
	}); eerr != nil {
		return eerr
	}
	e.done = true
	return nil
}

// assembleResponse builds the canonical Response for the
// representability gate from buffered terminal content.
func (e *ResponsesEventEncoder) assembleResponse(finish Finish) (Response, *RouterError) {
	items := make([]ResponseItem, 0, len(e.order))
	for _, item := range e.order {
		blk := e.blocks[item]
		switch blk.kind {
		case BlockText:
			full := e.buf.TextResult(item)
			id, rerr := NewItemID(string(item))
			if rerr != nil {
				return Response{}, rerr
			}
			items = append(items, TextItem{ID: id, Text: full})
		case BlockRefusal:
			full := e.buf.TextResult(item)
			id, rerr := NewItemID(string(item))
			if rerr != nil {
				return Response{}, rerr
			}
			items = append(items, RefusalItem{ID: id, Text: full})
		case BlockCustom:
			full := e.buf.CustomResult(item)
			id, rerr := NewItemID(string(item))
			if rerr != nil {
				return Response{}, rerr
			}
			call, rerr := NewCallID(blk.callID)
			if rerr != nil {
				return Response{}, rerr
			}
			cc, rerr := NewCustomCallItem(id, call, blk.namespace, blk.name, full)
			if rerr != nil {
				return Response{}, rerr
			}
			items = append(items, cc)
		default:
			raw, rerr := e.buf.ArgsResult(item)
			if rerr != nil {
				return Response{}, rerr
			}
			id, rerr := NewItemID(string(item))
			if rerr != nil {
				return Response{}, rerr
			}
			call, rerr := NewCallID(blk.callID)
			if rerr != nil {
				return Response{}, rerr
			}
			call2, rerr := NewToolCallItemWithNamespace(id, call, blk.namespace, blk.name, raw)
			if rerr != nil {
				return Response{}, rerr
			}
			items = append(items, call2)
		}
	}
	if !e.hasStart {
		return Response{}, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	}
	resp, rerr := NewResponse(e.upstream, e.upstreamModel, e.source, items, *e.usage, finish)
	if rerr != nil {
		return Response{}, rerr
	}
	resp.MalformedTools = e.malformed
	resp.IDOrigin = e.idOrigin
	resp.ModelOrigin = e.modelOrigin
	resp.RouterMintedCalls = e.minted
	return resp, nil
}

func (e *ResponsesEventEncoder) skeleton(status, incompleteReason string, output []any, usage map[string]any) map[string]any {
	var incomplete any
	if status == "incomplete" {
		incomplete = map[string]any{"reason": incompleteReason}
	}
	if output == nil {
		output = []any{}
	}
	return map[string]any{
		"id": e.respID, "object": "response",
		"created_at": e.createdAt, "model": e.publicModel,
		"status": status, "error": nil,
		"incomplete_details": incomplete,
		"output":             output,
		"usage":              usage,
	}
}

func (e *ResponsesEventEncoder) ensureStarted(w io.Writer, flush func()) *EncodeError {
	if e.started {
		return nil
	}
	e.started = true
	if eerr := e.writeEvent(w, flush, "response.created", map[string]any{
		"response": e.skeleton("in_progress", "", nil, nil),
	}); eerr != nil {
		return eerr
	}
	return e.writeEvent(w, flush, "response.in_progress", map[string]any{
		"response": e.skeleton("in_progress", "", nil, nil),
	})
}

func (e *ResponsesEventEncoder) writeEvent(w io.Writer, flush func(), name string, payload map[string]any) *EncodeError {
	payload["type"] = name
	payload["sequence_number"] = e.seq
	e.seq++
	var buf bytes.Buffer
	buf.WriteString("event: ")
	buf.WriteString(name)
	buf.WriteString("\ndata: ")
	data, err := json.Marshal(payload)
	if err != nil {
		return &EncodeError{Err: &RouterError{Status: 500, Code: "encoding_error", Msg: "failed to encode downstream event"}, WriteFailed: false}
	}
	buf.Write(data)
	buf.WriteString("\n\n")
	if buf.Len() > e.lim.MaxFrameBytes {
		return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Downstream response exceeds the size bound"}, WriteFailed: false}
	}
	if _, err := w.Write(buf.Bytes()); err != nil {
		e.writeFailed = true
		return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Downstream write failed"}, WriteFailed: true}
	}
	e.committed = true
	if flush != nil {
		flush()
	}
	return nil
}
