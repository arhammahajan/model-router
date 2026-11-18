// Gemini source stream codec: one decoder for the Gemini row.
//
// GeminiStreamDecoder validates the Gemini streamGenerateContent source
// ONCE, independently of destination, and emits canonical semantic events
// for the shared lifecycle (ResponseLifecycle) and the single pipeline
// (TranslateStream). It performs no downstream writes, mints no downstream
// IDs, owns no reader, spawns no goroutines, and enforces no destination
// policy. Use it with NewSSEFrameReader (?alt=sse) or
// NewJSONArrayFrameReader (default array) and TranslateStream.
//
// Framing modes are explicit (sse flag):
//   - SSE (?alt=sse): the finish chunk emits BlockClose* + UsageEvent +
//     CompletionEvent and returns promptly (no EOF read past the valid
//     terminal). Close validates state and reads nothing.
//   - JSON array (default): the finish element arms the pending terminal
//     and emits deltas only; BlockClose* + UsageEvent + CompletionEvent
//     emit only on the separate bare Frame{Closed:true}. Post-finish
//     elements are content-after-finish 502s. EOF before close and
//     trailing commas are framing errors that can never succeed (framing
//     owns them; the decoder only enforces no terminal without close).
//
// Wire contract (proven against gemini_to_chat_stream.go,
// gemini_to_anthropic_stream*.go, gemini_to_responses_stream*.go and the
// complete row DecodeGeminiResponse, none of which this decoder calls):
//   - Each frame/element is one partial GenerateContentResponse under the
//     complete-row envelope allowlist (never the Chat-worded
//     g2cValidateResponseEnvelope): candidates, promptFeedback,
//     usageMetadata, modelVersion, responseId, createTime. Unknown null
//     members are newer defaults; unknown non-null members fail closed.
//   - ResponseStart emits on the first content-bearing or
//     terminal-signalling chunk so early text streams immediately:
//     provider responseId/modelVersion when present, else the minted
//     geminiCompatResponseID (router) / "" (router). Later arrivals become
//     ProviderIdentityEvent / ResponseModelEvent (at-most-once,
//     pre-terminal, stability conflicts value-free).
//   - Per chunk, text runs emit BlockOpen(t<n>) + TextFragment immediately
//     (empty text emits nothing, creates no block; a tool call ends the
//     run, so text-call-text stays three ordered items). Each functionCall
//     validates via geminiParseFunctionCall and emits BlockOpen(c<n>,
//     CallOrigin) + ArgsFragment immediately (missing args emit exact {}
//     counted in both bounds). Absent id mints gemini-compat-call-<n>
//     skipping every seen id; colliding/supplied-duplicate ids conflict.
//   - thought=true, thoughtSignature, media, executableCode,
//     functionResponse, server tools, nonempty *TokensDetails, nonzero
//     thoughts/toolUse fail closed. role, when present, must be model.
//   - Usage: every present usageMetadata validates via geminiParseUsage
//     (exact totals, cache subset, overflow). Reported counts on the start
//     chunk become InitialUsage; later intermediate counts emit nothing.
//     The terminal chunk's own usage becomes the single terminal UsageEvent
//     (terminal-own-usage, never borrowed).
//   - Finish maps via closed geminiDecodeFinish; blocked promptFeedback
//     reuses geminiDecodePromptBlocked with the legacy contradiction
//     rules. Unknown finishes (incl. MALFORMED_FUNCTION_CALL) decode-fail.
//   - Retention: slot identity table (bounded by MaxToolCalls x
//     MaxArgsChars, already counted by the lifecycle identity budget) plus
//     bounded finish/usage scalars. No text/args retention, no
//     whole-generation buffering. Text is never retained.
//
// Bounds are the inherited Limits (MaxArgsChars, MaxStreamArgsChars,
// MaxStreamBlocks, MaxToolCalls); identity bytes are budgeted as in the
// Chat decoder (pending pre-start identities bounded separately until
// they flow into the lifecycle).
package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
)

// GeminiStreamDecoder decodes Gemini stream frames into canonical semantic
// events. It owns only upstream source state; it performs no downstream
// writes, holds no reader, spawns no goroutines, and enforces no
// destination policy. One instance serves one generation.
type GeminiStreamDecoder struct {
	lim Limits
	sse bool

	started bool
	done    bool

	hasProviderID    bool
	providerID       string
	hasProviderModel bool
	providerModel    string

	startIDOrigin    IdentityOrigin
	startModelOrigin IdentityOrigin
	identityEmitted  bool
	modelEmitted     bool

	pendingIdentityBytes int64

	order     []ItemID
	textCount int
	callCount int
	curText   ItemID
	hasText   bool

	seenCalls map[string]bool
	minted    map[CallID]struct{}
	nextMint  int
	totalArgs int
	toolCount int
	blocks    int

	blocked     bool
	blockReason string

	pendingTerminal bool
	pendingFinish   Finish
	pendingUsage    ResponseUsage
}

// NewGeminiStreamDecoder creates the Gemini source stream decoder for one
// generation. sse selects the framing contract: true for ?alt=sse
// (terminal returns promptly), false for the default JSON array (finish
// arms until Frame{Closed:true}). The flag is explicit because the same
// bytes cannot distinguish a prompt terminal from an armed one.
func NewGeminiStreamDecoder(lim Limits, sse bool) *GeminiStreamDecoder {
	return &GeminiStreamDecoder{
		lim:       lim,
		sse:       sse,
		seenCalls: map[string]bool{},
		minted:    map[CallID]struct{}{},
	}
}

func geminiStreamBad() *RouterError {
	return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
}

func geminiStreamUnrep(what string) *RouterError {
	return &RouterError{Status: 502, Code: CodeUpstreamBad,
		Msg: fmt.Sprintf("Upstream response feature %q has no representation", what)}
}

func geminiStreamContradiction() *RouterError {
	return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned a contradictory blocked response"}
}

// DecodeFrame decodes one typed frame into semantic events in wire order.
// It never takes reader/writer ownership and never emits FailureEvent.
func (d *GeminiStreamDecoder) DecodeFrame(frame Frame) ([]ResponseEvent, *RouterError) {
	if d.done {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent events after completion"}
	}
	if frame.Closed {
		return d.handleClose()
	}
	if d.pendingTerminal {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent content after the finish marker"}
	}
	data := frame.Data
	if data == "" {
		return nil, nil
	}
	if data == "[DONE]" {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream ended without completion"}
	}
	if d.sse && frame.HasEvent {
		// Gemini SSE carries no event names; a named frame from a proxy
		// is not part of the translated contract. Data still carries
		// the object; the name is ignored like the legacy SSE path.
	}
	return d.handleObject([]byte(data))
}

// Close validates trailing state after the source terminal. It performs
// no reads and returns no events: success was already emitted.
func (d *GeminiStreamDecoder) Close() ([]ResponseEvent, *RouterError) {
	if !d.done {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream ended without completion"}
	}
	return nil, nil
}

func (d *GeminiStreamDecoder) handleClose() ([]ResponseEvent, *RouterError) {
	if d.sse {
		return nil, geminiStreamBad()
	}
	if d.done {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent events after completion"}
	}
	if !d.pendingTerminal {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream ended without completion"}
	}
	evs := d.closeAll()
	evs = append(evs, UsageEvent{Usage: d.pendingUsage})
	complete, rerr := NewCompletionEvent(d.pendingFinish)
	if rerr != nil {
		return nil, rerr
	}
	evs = append(evs, complete)
	d.done = true
	d.pendingTerminal = false
	return evs, nil
}

func (d *GeminiStreamDecoder) closeAll() []ResponseEvent {
	var evs []ResponseEvent
	for _, item := range d.order {
		evs = append(evs, BlockClose{Item: item})
	}
	d.hasText = false
	d.curText = ""
	return evs
}

// handleObject validates one GenerateContentResponse object and returns
// its events. For SSE the terminal events emit now; for arrays the
// finish arms pendingTerminal and only deltas return.
func (d *GeminiStreamDecoder) handleObject(data []byte) ([]ResponseEvent, *RouterError) {
	top, rerr := d.validateEnvelope(data)
	if rerr != nil {
		return nil, rerr
	}
	if rerr := d.observeIdentityMembers(top); rerr != nil {
		return nil, rerr
	}
	if rawPF, ok := top["promptFeedback"]; ok && !isNull(rawPF) {
		blocked, reason, rerr := geminiDecodePromptBlocked(rawPF)
		if rerr != nil {
			return nil, rerr
		}
		if blocked {
			d.blocked = true
			d.blockReason = reason
		}
	}
	rawCands, present := top["candidates"]
	cands, absent, malformed := geminiNormalizeCandidates(rawCands, present)
	if malformed {
		return nil, geminiStreamBad()
	}
	if absent {
		if !d.blocked {
			return nil, geminiStreamBad()
		}
		return d.handleBlockedTerminal(top)
	}
	if len(cands) != 1 {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned multiple choices with no representation"}
	}
	contentEvs, finishStr, hasFinish, rerr := d.handleCandidate(cands[0])
	if rerr != nil {
		return nil, rerr
	}
	var chunkUsage *ResponseUsage
	chunkHasUsage := false
	if rawU, ok := top["usageMetadata"]; ok && !isNull(rawU) {
		u, rerr := geminiParseUsage(rawU)
		if rerr != nil {
			return nil, rerr
		}
		cp := u
		chunkUsage, chunkHasUsage = &cp, true
	}
	if !hasFinish {
		if d.blocked {
			return nil, geminiStreamContradiction()
		}
		var evs []ResponseEvent
		if !d.started {
			if len(contentEvs) == 0 {
				return nil, nil
			}
			evs = append(evs, d.emitStart(chunkUsage))
		}
		evs = append(evs, d.emitLateIdentity()...)
		evs = append(evs, contentEvs...)
		return evs, nil
	}
	hasCalls := d.toolCount > 0
	irFinish, rerr := geminiDecodeFinish(finishStr, hasCalls)
	if rerr != nil {
		return nil, rerr
	}
	if d.blocked && !isFilterFinish(irFinish) {
		return nil, geminiStreamContradiction()
	}
	if d.blocked && hasCalls {
		return nil, geminiStreamContradiction()
	}
	if !chunkHasUsage {
		return nil, geminiStreamBad()
	}
	var evs []ResponseEvent
	if !d.started {
		evs = append(evs, d.emitStart(chunkUsage))
	}
	evs = append(evs, d.emitLateIdentity()...)
	evs = append(evs, contentEvs...)
	if d.sse {
		evs = append(evs, d.closeAll()...)
		evs = append(evs, UsageEvent{Usage: *chunkUsage})
		complete, rerr := NewCompletionEvent(irFinish)
		if rerr != nil {
			return nil, rerr
		}
		evs = append(evs, complete)
		d.done = true
		return evs, nil
	}
	d.pendingTerminal = true
	d.pendingFinish = irFinish
	d.pendingUsage = *chunkUsage
	return evs, nil
}

func isFilterFinish(f Finish) bool {
	return f.Kind == FinishContentFilter
}

func (d *GeminiStreamDecoder) handleBlockedTerminal(top map[string]json.RawMessage) ([]ResponseEvent, *RouterError) {
	if d.blocks > 0 {
		return nil, geminiStreamContradiction()
	}
	var chunkUsage *ResponseUsage
	if rawU, ok := top["usageMetadata"]; ok && !isNull(rawU) {
		u, rerr := geminiParseUsage(rawU)
		if rerr != nil {
			return nil, rerr
		}
		cp := u
		chunkUsage = &cp
	}
	if chunkUsage == nil {
		return nil, geminiStreamBad()
	}
	finish := Finish{Kind: FinishContentFilter, Raw: d.blockReason}
	var evs []ResponseEvent
	if !d.started {
		evs = append(evs, d.emitStart(chunkUsage))
	}
	evs = append(evs, d.emitLateIdentity()...)
	if d.sse {
		evs = append(evs, UsageEvent{Usage: *chunkUsage})
		complete, rerr := NewCompletionEvent(finish)
		if rerr != nil {
			return nil, rerr
		}
		evs = append(evs, complete)
		d.done = true
		return evs, nil
	}
	d.pendingTerminal = true
	d.pendingFinish = finish
	d.pendingUsage = *chunkUsage
	return evs, nil
}

func (d *GeminiStreamDecoder) validateEnvelope(data []byte) (map[string]json.RawMessage, *RouterError) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil || top == nil {
		return nil, geminiStreamBad()
	}
	for k, v := range top {
		switch k {
		case "candidates", "promptFeedback", "usageMetadata", "modelVersion", "responseId", "createTime":
		default:
			if isNull(v) {
				continue
			}
			return nil, geminiStreamUnrep("response." + k)
		}
	}
	if rawCT, ok := top["createTime"]; ok && !isNull(rawCT) {
		var s string
		if err := json.Unmarshal(rawCT, &s); err != nil || strings.TrimSpace(s) == "" {
			return nil, geminiStreamBad()
		}
	}
	return top, nil
}

func (d *GeminiStreamDecoder) observeIdentityMembers(top map[string]json.RawMessage) *RouterError {
	if rawRID, ok := top["responseId"]; ok && !isNull(rawRID) {
		var s string
		if err := json.Unmarshal(rawRID, &s); err != nil || !validOpaqueID(s) {
			return geminiStreamBad()
		}
		if len(s) > d.lim.MaxArgsChars {
			return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream response exceeds the size bound"}
		}
		if !d.hasProviderID {
			d.hasProviderID = true
			d.providerID = s
		} else if d.providerID != s {
			return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream changed its response identity"}
		}
	}
	if rawMV, ok := top["modelVersion"]; ok && !isNull(rawMV) {
		var s string
		if err := json.Unmarshal(rawMV, &s); err != nil || strings.TrimSpace(s) == "" {
			return geminiStreamBad()
		}
		if !d.hasProviderModel {
			d.hasProviderModel = true
			d.providerModel = s
		} else if d.providerModel != s {
			return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream changed its response identity"}
		}
	}
	pending := 0
	if !d.started {
		if d.hasProviderID {
			pending += len(d.providerID)
		}
		if d.hasProviderModel {
			pending += len(d.providerModel)
		}
		next, rerr := addBytes(0, pending)
		if rerr != nil || next > int64(d.lim.MaxStreamArgsChars) {
			return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream response exceeds the size bound"}
		}
		_ = next
		d.pendingIdentityBytes = int64(pending)
		if d.pendingIdentityBytes > int64(d.lim.MaxStreamArgsChars) {
			return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream response exceeds the size bound"}
		}
	}
	return nil
}

func (d *GeminiStreamDecoder) emitStart(usage *ResponseUsage) ResponseEvent {
	var id ResponseID
	var idOrigin IdentityOrigin
	if d.hasProviderID {
		id, idOrigin = ResponseID(d.providerID), IdentityProvider
	} else {
		id, idOrigin = ResponseID(geminiCompatResponseID), IdentityRouterMinted
	}
	model, modelOrigin := "", IdentityRouterMinted
	if d.hasProviderModel {
		model, modelOrigin = d.providerModel, IdentityProvider
	}
	d.startIDOrigin, d.startModelOrigin = idOrigin, modelOrigin
	d.started = true
	d.pendingIdentityBytes = 0
	if idOrigin == IdentityProvider {
		d.identityEmitted = true
	}
	if modelOrigin == IdentityProvider {
		d.modelEmitted = true
	}
	var initial *ResponseUsage
	if usage != nil {
		// Preserve reported running counts at start without satisfying the
		// separate terminal-accounting requirement or retaining later updates.
		copyUsage := *usage
		copyUsage.Provenance.Scope = UsageInitial
		initial = &copyUsage
	}
	return ResponseStart{ID: id, Model: model, Source: DialectGemini, IDOrigin: idOrigin, ModelOrigin: modelOrigin, InitialUsage: initial}
}

func (d *GeminiStreamDecoder) emitLateIdentity() []ResponseEvent {
	var evs []ResponseEvent
	if !d.started {
		return nil
	}
	if d.hasProviderID && !d.identityEmitted && d.startIDOrigin == IdentityRouterMinted {
		d.identityEmitted = true
		evs = append(evs, ProviderIdentityEvent{ID: ResponseID(d.providerID)})
	}
	if d.hasProviderModel && !d.modelEmitted && d.startModelOrigin == IdentityRouterMinted {
		d.modelEmitted = true
		evs = append(evs, ResponseModelEvent{Model: d.providerModel})
	}
	return evs
}

// handleCandidate validates one candidate and returns content events
// plus the optional finish reason. It emits no terminal events.
