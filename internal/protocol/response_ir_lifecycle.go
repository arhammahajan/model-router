// Canonical Response IR: semantic lifecycle validation without retention.
//
// ResponseLifecycle is the single validator shared by stream decoders
// and complete decoders. It enforces: one start, ordered unique blocks,
// kind-correct deltas, closed-before-terminal, exactly one terminal
// usage plus one completion, unique call identities, and every count
// bound. It retains NO text and NO argument bytes: per-block byte
// counts plus a running hash per block are all it keeps, so validation
// state is O(blocks), never O(generation bytes).
//
// This separation is deliberate (root blocker 1): incremental encoders
// for Chat, Anthropic, and Gemini forward text without retaining it,
// and must never pay a whole-generation buffer or fail a new per-block
// text cap. Text/argument retention exists only as an explicit,
// target-owned, opt-in TerminalBuffer (see below), instantiated solely
// by targets whose terminal envelope requires reconstruction (today:
// Responses). The completion validator never owns text.
//
// Bound provenance (all inherited from the existing pair translators,
// none new):
//   - args per-field: lim.MaxArgsChars (existing one-call cap);
//   - text+args retained aggregate: lim.MaxStreamArgsChars (the existing
//     shared text-plus-args aggregate; existing translators cap retained
//     text by this aggregate, with NO per-text-block cap);
//   - frames: lim.MaxFrameBytes (framing layer, never lifecycle);
//   - distinct blocks: lim.MaxStreamBlocks; tool calls: lim.MaxToolCalls.
package protocol

import (
	"crypto/sha256"
	"hash"
	"strings"
)

// lifeBlock is lifecycle state for one open block: kind, identity,
// closure, and integrity counters. No content bytes are stored.
type lifeBlock struct {
	kind      BlockKind
	call      CallID
	namespace string
	name      string
	closed    bool
	malformed bool
	// index/hasIndex retain the source-dialect logical position
	// for the summary; any block kind from an indexed source may
	// carry one.
	index     int
	hasIndex  bool
	textBytes int64
	textHash  hash.Hash
	argsBytes int64
	argsHash  hash.Hash
}

// ResponseLifecycle validates one generation's semantic event order.
// See the package comment for what it retains (nothing) and what it
// enforces (everything structural).
//
// Identity policy: response, item and call IDs are opaque provider
// handles (validOpaqueID: nonempty after trim, valid UTF-8, may contain
// spaces/colons), each bounded by lim.MaxArgsChars at this boundary.
// Tool NAMES are nonempty here; their 1..64 grammar is enforced
// target-side (CheckRepresentable), never here, because the
// Chat->Gemini path provably passes grammar-invalid names. Retained
// identity bytes (IDs + names held in the block/call maps) are budgeted
// cumulatively under lim.MaxStreamArgsChars, separate from text/argument
// content bytes (which this validator only counts and hashes, never
// retains), so 1MB IDs x 1024 blocks cannot build GB retention.
type ResponseLifecycle struct {
	lim    Limits
	source Dialect

	sawStart  bool
	completed bool
	respID    ResponseID
	model     string
	// idOrigin/modelOrigin mirror the start provenance. Empty start
	// origins read as provider; router origins are permitted only for
	// the Gemini source. A late ProviderIdentityEvent flips idOrigin
	// to provider; a late ResponseModelEvent sets modelOrigin to
	// provider on first promotion.
	idOrigin    IdentityOrigin
	modelOrigin IdentityOrigin
	// sawIdentity latches the at-most-once ProviderIdentityEvent.
	sawIdentity bool
	// mintedCalls tracks router-minted tool call IDs (BlockOpen with
	// CallOrigin router). Provider-supplied IDs — even colliding with
	// the compat prefix — never enter this set.
	mintedCalls map[CallID]struct{}

	blocks map[ItemID]*lifeBlock
	order  []ItemID
	calls  map[CallID]bool
	// indices tracks claimed source-dialect tool positions so two
	// blocks never share one logical index.
	indices map[int]bool
	tools   int

	// identBytes counts retained identity/name bytes (response ID once
	// plus per-block item/call/name). Bounded by MaxStreamArgsChars,
	// independent of text/argument content counters.
	identBytes int64

	sawTerminalUsage bool
	usage            ResponseUsage
	finish           *Finish

	failed  bool
	failErr *RouterError
}

// NewResponseLifecycle creates the validator for one generation decoded
// from source.
func NewResponseLifecycle(lim Limits, source Dialect) *ResponseLifecycle {
	return &ResponseLifecycle{lim: lim, source: source, blocks: map[ItemID]*lifeBlock{}, calls: map[CallID]bool{}, indices: map[int]bool{}, mintedCalls: map[CallID]struct{}{}}
}

// Completed reports whether a terminal (completion or failure) has been
// observed. The pipeline stops reading upstream as soon as this is
// true: completion followed by additional reads would hang.
func (l *ResponseLifecycle) Completed() bool { return l.completed }

// Failed reports whether a validated FailureEvent terminated the stream.
// A failed stream never produces a success summary and the pipeline must
// return the failure error (never nil), with Abort if already committed.
func (l *ResponseLifecycle) Failed() bool { return l.failed }

// Failure returns the terminal failure error after a validated
// FailureEvent, or nil if the stream did not fail.
func (l *ResponseLifecycle) Failure() *RouterError { return l.failErr }

// Observe validates one semantic event in stream order.
func (l *ResponseLifecycle) Observe(ev ResponseEvent) *RouterError {
	if l.completed {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent events after completion"}
	}
	switch e := ev.(type) {
	case ResponseStart:
		return l.observeStart(e)
	case ResponseModelEvent:
		return l.observeModel(e)
	case ProviderIdentityEvent:
		return l.observeProviderIdentity(e)
	case BlockOpen:
		return l.observeBlockOpen(e)
	case TextFragment:
		return l.observeText(e)
	case ArgsFragment:
		return l.observeArgs(e)
	case CustomDelta:
		return l.observeCustom(e)
	case MalformedToolEvent:
		return l.observeMalformedTool(e)
	case BlockClose:
		return l.observeBlockClose(e)
	case UsageEvent:
		return l.observeUsage(e)
	case CompletionEvent:
		return l.observeCompletion(e)
	case FailureEvent:
		return l.observeFailure(e)
	default:
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent an unknown response event"}
	}
}

func (l *ResponseLifecycle) requireStarted() *RouterError {
	if !l.sawStart {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent content before the response start"}
	}
	return nil
}

// opaqueBounded reports whether s is an acceptable opaque identity at
// this boundary: nonempty after trim, valid UTF-8, and within the
// per-field byte budget. No character grammar: provider handles may
// contain spaces, colons, or other punctuation (proven by
// TestG2CRegrOpaqueCallIDsPreserved and the Chat row long-ID test).
func opaqueBounded(s string, lim Limits) bool {
	if !validOpaqueID(s) {
		return false
	}
	return len(s) <= lim.MaxArgsChars
}

func (l *ResponseLifecycle) addIdentBytes(n int) *RouterError {
	next, rerr := addBytes(l.identBytes, n)
	if rerr != nil {
		return rerr
	}
	if next > int64(l.lim.MaxStreamArgsChars) {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream response exceeds the size bound"}
	}
	l.identBytes = next
	return nil
}

func (l *ResponseLifecycle) observeStart(e ResponseStart) *RouterError {
	if l.sawStart {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent a duplicate response start"}
	}
	if !opaqueBounded(string(e.ID), l.lim) {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	}
	// An empty Model is valid only for sources whose wire omits it:
	// Gemini (absent modelVersion), Anthropic streaming (the minimal
	// legacy message_start.message carries id+usage only, with absence
	// preserved as "" by the source decoder; see chat_sse_text.json and
	// response_anthropic_stream_decode.go), and Responses streaming
	// (response.created may omit model while the provider reports it
	// later in a progress envelope or the actual terminal response
	// object via ResponseModelEvent; see response_responses_stream_decode.go).
	// A supplied-empty model string is rejected at source decode, so ""
	// here always means the member was omitted or null. Chat still
	// requires a model.
	if e.Model == "" && e.Source != DialectGemini && e.Source != DialectAnthropic && e.Source != DialectResponses {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	}
	// Source provenance must agree with the configured source: a decoder
	// emitting events tagged for another dialect indicates a wiring bug,
	// not a valid generation. Empty source never matches.
	if e.Source == "" || e.Source != l.source {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream response source does not match the requested source"}
	}
	if rerr := l.addIdentBytes(len(string(e.ID))); rerr != nil {
		return rerr
	}
	l.sawStart = true
	l.respID, l.model = e.ID, e.Model
	// Origins default to provider for existing emitters that never
	// mint. Router origins are permitted only for the Gemini source.
	l.idOrigin, l.modelOrigin = e.IDOrigin, e.ModelOrigin
	if l.idOrigin == "" {
		l.idOrigin = IdentityProvider
	}
	if l.modelOrigin == "" {
		l.modelOrigin = IdentityProvider
	}
	if (l.idOrigin == IdentityRouterMinted || l.modelOrigin == IdentityRouterMinted) && e.Source != DialectGemini {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream response source does not match the requested source"}
	}
	return nil
}

// observeModel records the provider model when a source that started
// without one later reports it. It is metadata-only: no downstream
// frame, no identity rewrite. Rules: start must come first (meta
// before start is a static order failure), the reported model must be
// nonempty (supplied-empty is rejected at source decode, so "" here
// is always a bug), the model is immutable once known (a different
// reported model fails value-free, never echoed), and the retained
// Result.Model is updated. A repeated identical report is a no-op, so
// decoders emit the event only on the first report. After completion
// no event is accepted (Observe already rejects post-terminal events).
func (l *ResponseLifecycle) observeModel(e ResponseModelEvent) *RouterError {
	if rerr := l.requireStarted(); rerr != nil {
		return rerr
	}
	if strings.TrimSpace(e.Model) == "" {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	}
	if l.model == "" {
		l.model = e.Model
		l.modelOrigin = IdentityProvider
		return nil
	}
	if l.model != e.Model {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream changed its response identity"}
	}
	return nil
}

// observeProviderIdentity records the late provider responseId when a
// Gemini source that started router-minted later observes it. Rules
// mirror observeModel: started, at-most-once, pre-terminal only (the
// Observe gate already rejects post-completion events), nonempty
// opaque bounded ID with identity-budget accounting, immutable once
// provider-known (differing values conflict value-free). A value equal
// to the minted start ID flips origin provider without changing the
// string and never enters the minted set. A second event always fails.
func (l *ResponseLifecycle) observeProviderIdentity(e ProviderIdentityEvent) *RouterError {
	if rerr := l.requireStarted(); rerr != nil {
		return rerr
	}
	if l.sawIdentity {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent a duplicate response start"}
	}
	if !opaqueBounded(string(e.ID), l.lim) {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	}
	if l.finish != nil {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent content after the finish marker"}
	}
	if l.idOrigin == IdentityProvider {
		// Provider already known: any late identity contradicts.
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned conflicting response identities"}
	}
	if l.source != DialectGemini {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream response source does not match the requested source"}
	}
	if rerr := l.addIdentBytes(len(string(e.ID))); rerr != nil {
		return rerr
	}
	l.sawIdentity = true
	l.respID = e.ID
	l.idOrigin = IdentityProvider
	return nil
}

func (l *ResponseLifecycle) observeBlockOpen(e BlockOpen) *RouterError {
	if rerr := l.requireStarted(); rerr != nil {
		return rerr
	}
	if l.finish != nil {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent content after the finish marker"}
	}
	if !opaqueBounded(string(e.Item), l.lim) {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream opened a response block without identity"}
	}
	if _, dup := l.blocks[e.Item]; dup {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream opened a duplicate response block"}
	}
	if len(l.blocks) >= l.lim.MaxStreamBlocks {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream exceeds the content block bound"}
	}
	switch e.Kind {
	case BlockText, BlockRefusal:
		if e.Call != "" || e.Namespace != "" || e.Name != "" {
			return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream text block carries a tool identity"}
		}
		if e.SourceIndex != nil {
			if *e.SourceIndex < 0 {
				return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
			}
			if l.indices[*e.SourceIndex] {
				return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned conflicting tool call identities"}
			}
			l.indices[*e.SourceIndex] = true
		}
	case BlockTool:
		if !opaqueBounded(string(e.Call), l.lim) || strings.TrimSpace(e.Name) == "" || len(e.Name) > l.lim.MaxArgsChars {
			return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call is missing its identity"}
		}
		if l.calls[e.Call] {
			return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned conflicting tool call identities"}
		}
		callOrigin := e.CallOrigin
		if callOrigin == "" {
			callOrigin = IdentityProvider
		}
		if callOrigin == IdentityRouterMinted && l.source != DialectGemini {
			return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream response source does not match the requested source"}
		}
		if e.SourceIndex != nil {
			if *e.SourceIndex < 0 {
				return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
			}
			if l.indices[*e.SourceIndex] {
				return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned conflicting tool call identities"}
			}
		}
		if l.tools+1 > l.lim.MaxToolCalls {
			return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call count exceeds the size bound"}
		}
		l.calls[e.Call] = true
		callOrigin2 := e.CallOrigin
		if callOrigin2 == "" {
			callOrigin2 = IdentityProvider
		}
		if callOrigin2 == IdentityRouterMinted {
			l.mintedCalls[e.Call] = struct{}{}
		}
		if e.SourceIndex != nil {
			l.indices[*e.SourceIndex] = true
		}
		l.tools++
	case BlockCustom:
		if !opaqueBounded(string(e.Call), l.lim) || strings.TrimSpace(e.Name) == "" || len(e.Name) > l.lim.MaxArgsChars {
			return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call is missing its identity"}
		}
		if l.calls[e.Call] {
			return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned conflicting tool call identities"}
		}
		if e.SourceIndex != nil {
			if *e.SourceIndex < 0 {
				return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
			}
			if l.indices[*e.SourceIndex] {
				return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned conflicting tool call identities"}
			}
		}
		if l.tools+1 > l.lim.MaxToolCalls {
			return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call count exceeds the size bound"}
		}
		l.calls[e.Call] = true
		if e.SourceIndex != nil {
			l.indices[*e.SourceIndex] = true
		}
		l.tools++
	default:
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream response block has no representation"}
	}
	if rerr := l.addIdentBytes(len(string(e.Item)) + len(string(e.Call)) + len(e.Namespace) + len(e.Name)); rerr != nil {
		return rerr
	}
	l.blocks[e.Item] = &lifeBlock{kind: e.Kind, call: e.Call, namespace: e.Namespace, name: e.Name, index: 0, hasIndex: false, textHash: sha256.New(), argsHash: sha256.New()}
	if e.SourceIndex != nil {
		l.blocks[e.Item].index = *e.SourceIndex
		l.blocks[e.Item].hasIndex = true
	}
	l.order = append(l.order, e.Item)
	return nil
}

func (l *ResponseLifecycle) openBlock(item ItemID, kind BlockKind) (*lifeBlock, *RouterError) {
	if rerr := l.requireStarted(); rerr != nil {
		return nil, rerr
	}
	if l.finish != nil {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent content after the finish marker"}
	}
	b, ok := l.blocks[item]
	if !ok || b.closed || b.kind != kind {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent a delta for a closed or unopened block"}
	}
	return b, nil
}

func addBytes(total int64, n int) (int64, *RouterError) {
	next := total + int64(n)
	if n < 0 || next < total {
		return 0, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream response exceeds the size bound"}
	}
	return next, nil
}

// observeText validates a text fragment for an open text/refusal block.
// It counts bytes and folds them into the block hash; it stores nothing.
// There is deliberately no per-block or per-generation text content
// bound here: incremental encoders forward text without retaining it,
// and existing translators bound retained text only through the shared
// text-plus-args aggregate owned by the target reconstructor below.
func (l *ResponseLifecycle) observeText(e TextFragment) *RouterError {
	b, rerr := l.openBlock(e.Item, BlockText)
	if rerr != nil {
		if b == nil {
			// Refusal blocks accept text fragments too; retry with the
			// refusal kind before failing.
			var rerr2 *RouterError
			b, rerr2 = l.openBlock(e.Item, BlockRefusal)
			if rerr2 != nil {
				return rerr
			}
		} else {
			return rerr
		}
	}
	next, rerr := addBytes(b.textBytes, len(e.Text))
	if rerr != nil {
		return rerr
	}
	b.textBytes = next
	_, _ = b.textHash.Write([]byte(e.Text))
	return nil
}

// observeArgs validates an argument fragment for an open tool block. It
// counts bytes and folds them into the block hash; it stores nothing.
// Argument JSON validity is enforced by the source decoder (which sees
// the bytes) and re-checked by any target-owned TerminalBuffer at
// block close, never here.
func (l *ResponseLifecycle) observeArgs(e ArgsFragment) *RouterError {
	b, rerr := l.openBlock(e.Item, BlockTool)
	if rerr != nil {
		return rerr
	}
	if b.malformed {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent arguments after malformed tool classification"}
	}
	next, rerr := addBytes(b.argsBytes, len(e.Fragment))
	if rerr != nil {
		return rerr
	}
	b.argsBytes = next
	_, _ = b.argsHash.Write([]byte(e.Fragment))
	return nil
}

// observeCustom validates a freeform input fragment for an open custom
// block. Bounds mirror observeArgs (per-field MaxArgsChars at the source
// decoder, aggregate here via byte counting); no JSON validation occurs.
func (l *ResponseLifecycle) observeCustom(e CustomDelta) *RouterError {
	b, rerr := l.openBlock(e.Item, BlockCustom)
	if rerr != nil {
		return rerr
	}
	if b.malformed {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent arguments after malformed tool classification"}
	}
	next, rerr := addBytes(b.argsBytes, len(e.Delta))
	if rerr != nil {
		return rerr
	}
	b.argsBytes = next
	_, _ = b.argsHash.Write([]byte(e.Delta))
	return nil
}

func (l *ResponseLifecycle) observeBlockClose(e BlockClose) *RouterError {
	if rerr := l.requireStarted(); rerr != nil {
		return rerr
	}
	b, ok := l.blocks[e.Item]
	if !ok || b.closed {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream closed a block that was not open"}
	}
	if l.finish != nil {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent content after the finish marker"}
	}
	b.closed = true
	return nil
}

// observeMalformedTool records the typed malformed-args classification
// for one open tool block. It is provenance, not failure: the stream
// stays structurally valid and must still close the block and terminate
// with usage plus completion. Only the source decoder emits this event
// (it alone sees the argument bytes); syntax, identity, and size errors
// never arrive here.
func (l *ResponseLifecycle) observeMalformedTool(e MalformedToolEvent) *RouterError {
	if rerr := l.requireStarted(); rerr != nil {
		return rerr
	}
	if !opaqueBounded(string(e.Item), l.lim) {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent a tool argument delta without identity"}
	}
	b, ok := l.blocks[e.Item]
	if !ok || b.closed || (b.kind != BlockTool && b.kind != BlockCustom) {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent a delta for a closed or unopened block"}
	}
	if l.finish != nil {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent content after the finish marker"}
	}
	if b.malformed {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent a delta for a closed or unopened block"}
	}
	b.malformed = true
	return nil
}

func (l *ResponseLifecycle) observeUsage(e UsageEvent) *RouterError {
	if rerr := l.requireStarted(); rerr != nil {
		return rerr
	}
	if e.Usage.Provenance.Scope == UsageTerminal {
		if l.sawTerminalUsage {
			return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream sent duplicate usage"}
		}
		if rerr := e.Usage.ValidateAuthoritative(); rerr != nil {
			return rerr
		}
		l.sawTerminalUsage = true
		l.usage = e.Usage
		return nil
	}
	// Initial/cumulative counters are informational: when all three
	// totals are present they must be internally exact (overflow-safe),
	// but they never satisfy the terminal requirement. Multiple
	// cumulative updates are valid.
	if e.Usage.Prompt != nil && e.Usage.Completion != nil && e.Usage.Total != nil {
		p, c, t := *e.Usage.Prompt, *e.Usage.Completion, *e.Usage.Total
		if p < 0 || c < 0 || t < 0 {
			return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned invalid usage accounting"}
		}
		if sum := p + c; sum < p || sum < c || sum != t {
			return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned invalid usage accounting"}
		}
	}
	return nil
}

func (l *ResponseLifecycle) observeCompletion(e CompletionEvent) *RouterError {
	if rerr := l.requireStarted(); rerr != nil {
		return rerr
	}
	if l.finish != nil {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent a duplicate finish marker"}
	}
	if rerr := e.Finish.validate(); rerr != nil {
		return rerr
	}
	for _, item := range l.order {
		if b := l.blocks[item]; !b.closed {
			return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream finished with an unclosed content block"}
		}
	}
	fin := e.Finish
	l.finish = &fin
	l.completed = true
	return nil
}

// observeFailure validates a post-commit semantic failure. Pre-commit
// failures never become events (the source decoder returns a
// *RouterError for an HTTP error instead), so a FailureEvent requires a
// started stream and non-empty value-free Code/Field. An empty or
// pre-start FailureEvent is rejected and never marks completion, and a
// validated failure records its error for the pipeline to return (never
// nil success).
func (l *ResponseLifecycle) observeFailure(e FailureEvent) *RouterError {
	if rerr := l.requireStarted(); rerr != nil {
		return rerr
	}
	if e.Code == "" || e.Field == "" {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent an invalid response failure"}
	}
	l.completed = true
	l.failed = true
	l.failErr = &RouterError{Status: 502, Code: e.Code, Msg: "Upstream stream failed at " + e.Field}
	return nil
}

// BlockSummary is the validated structural record of one content block:
// identity, kind, closure, byte counts, and integrity hashes. It carries
// no content bytes: encoders that need terminal reconstruction keep
// their own bounded buffers (TerminalBuffer).
//
// Hashes are SHA256 over the exact fragment bytes (existing Responses
// terminal integrity uses SHA256 to avoid collisions); they prove content
// flowed through validation without retention, never equality of meaning
// alone.
type BlockSummary struct {
	Item ItemID
	Kind BlockKind
	Call CallID
	// Namespace is the restored Responses namespace ("" = plain),
	// retained from the typed BlockOpen identity for the terminal
	// summary so targets gate on facts, never on ID prefixes.
	Namespace string
	Name      string
	// SourceIndex retains the source-dialect logical position
	// (Chat tool index, Anthropic content index, Responses
	// output_index) so targets order by typed provenance instead of
	// reparsing the opaque ItemID. Any block kind from an indexed
	// source may carry one; Chat text never does.
	SourceIndex *int
	// Malformed marks a tool block whose argument bytes never formed a
	// valid JSON object (typed source classification for the target
	// gate, not a validation failure).
	Malformed bool
	TextBytes int64
	TextHash  [32]byte
	ArgsBytes int64
	ArgsHash  [32]byte
}

// LifecycleResult is the validated summary of a complete generation:
// the non-stream meaning streaming must reproduce exactly.
type LifecycleResult struct {
	ID     ResponseID
	Model  string
	Source Dialect
	Blocks []BlockSummary
	Usage  ResponseUsage
	Finish Finish
	// MalformedTools counts tool blocks classified malformed by the
	// source decoder (MalformedToolEvent), mirroring
	// Response.MalformedTools on the complete row. Malformed streams
	// still produce a success summary: the target gate decides
	// (Gemini suppress-and-remap, Anthropic/Responses refuse).
	MalformedTools int
	// IDOrigin/ModelOrigin mirror the start provenance with late
	// promotions applied. Empty reads as provider for sources that
	// never mint; Gemini carries explicit router origins.
	IDOrigin    IdentityOrigin
	ModelOrigin IdentityOrigin
	// RouterMintedCalls mirrors Response.RouterMintedCalls: tool call
	// IDs the router minted (Gemini only). Never contains
	// provider-supplied IDs.
	RouterMintedCalls map[CallID]struct{}
}

// Result assembles the validated summary. It requires the full terminal
// contract: every block closed, exactly one authoritative terminal
// usage, and exactly one completion. A truncated stream (no completion,
// no terminal usage) or a failed stream (FailureEvent observed) never
// produces a success summary.
func (l *ResponseLifecycle) Result() (LifecycleResult, *RouterError) {
	fail := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream ended without completion"}
	if l.failed {
		return LifecycleResult{}, fail
	}
	if !l.sawStart {
		return LifecycleResult{}, fail
	}
	if l.finish == nil {
		return LifecycleResult{}, fail
	}
	if !l.sawTerminalUsage {
		return LifecycleResult{}, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream ended without authoritative usage"}
	}
	blocks := make([]BlockSummary, 0, len(l.order))
	malformed := 0
	for _, item := range l.order {
		b := l.blocks[item]
		var th, ah [32]byte
		copy(th[:], b.textHash.Sum(nil))
		copy(ah[:], b.argsHash.Sum(nil))
		if b.malformed {
			malformed++
		}
		var sourceIndex *int
		if b.hasIndex {
			sourceIndex = intPtr(b.index)
		}
		blocks = append(blocks, BlockSummary{
			Item: item, Kind: b.kind, Call: b.call, Namespace: b.namespace, Name: b.name,
			SourceIndex: sourceIndex, Malformed: b.malformed,
			TextBytes: b.textBytes, TextHash: th,
			ArgsBytes: b.argsBytes, ArgsHash: ah,
		})
	}
	return LifecycleResult{ID: l.respID, Model: l.model, Source: l.source, Blocks: blocks, Usage: l.usage, Finish: *l.finish, MalformedTools: malformed, IDOrigin: l.idOrigin, ModelOrigin: l.modelOrigin, RouterMintedCalls: l.mintedCalls}, nil
}

// --- Target-owned terminal reconstruction (opt-in only) --------------------

// TerminalBuffer is the OPTIONAL, target-owned bounded reconstruction
// for terminal envelopes that require complete content (today: the
// Responses response.completed object with its full output items).
// Incremental encoders (Chat, Anthropic, Gemini) never instantiate it:
// they forward fragments without retaining them.
//
// Bound provenance (inherited from the existing pair translators):
// each argument field is capped by lim.MaxArgsChars; retained
// text+args combined are capped by lim.MaxStreamArgsChars (the existing
// shared aggregate); there is NO per-text-block cap. Items are capped
// by lim.MaxToolCalls at close time.
type TerminalBuffer struct {
	lim   Limits
	text  map[ItemID]*bufferSlot
	args  map[ItemID]*bufferSlot
	kinds map[ItemID]BlockKind
	total int
	// identBytes budgets retained identity bytes (item IDs first seen
	// here), separate from text/argument content bytes, under the same
	// MaxStreamArgsChars aggregate so distinct-block flooding with 1MB
	// IDs cannot build unbounded retention.
	identBytes int64
}

type bufferSlot struct {
	data []byte
}

func NewTerminalBuffer(lim Limits) *TerminalBuffer {
	return &TerminalBuffer{lim: lim, text: map[ItemID]*bufferSlot{}, args: map[ItemID]*bufferSlot{}, kinds: map[ItemID]BlockKind{}}
}

func (b *TerminalBuffer) slotFor(m map[ItemID]*bufferSlot, item ItemID, kind BlockKind) *bufferSlot {
	if s, ok := m[item]; ok {
		return s
	}
	s := &bufferSlot{}
	m[item] = s
	b.kinds[item] = kind
	return s
}

// ObserveText retains one text fragment under the shared aggregate
// bound. No per-block text cap is applied (existing translators never
// had one); the aggregate bound is the only text retention limit.
// Overflow-safe: length checks subtract before comparing so a huge
// fragment cannot wrap past the bound. Independent of the lifecycle:
// opaque item syntax plus lim.MaxArgsChars, distinct-block count,
// cumulative identity budget, and kind coherence are re-checked here
// so standalone use cannot smuggle invalid state.
func (b *TerminalBuffer) ObserveText(item ItemID, text string) *RouterError {
	if !opaqueBounded(string(item), b.lim) {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream opened a response block without identity"}
	}
	if kind, ok := b.kinds[item]; ok && kind != BlockText && kind != BlockRefusal {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent a delta for a closed or unopened block"}
	}
	if _, ok := b.kinds[item]; !ok {
		if len(b.kinds) >= b.lim.MaxStreamBlocks {
			return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream exceeds the content block bound"}
		}
		next, rerr := addBytes(b.identBytes, len(string(item)))
		if rerr != nil || next > int64(b.lim.MaxStreamArgsChars) {
			return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream response exceeds the size bound"}
		}
		b.identBytes = next
	}
	if len(text) > b.lim.MaxStreamArgsChars-b.total {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream response exceeds the size bound"}
	}
	s := b.slotFor(b.text, item, BlockText)
	s.data = append(s.data, text...)
	b.total += len(text)
	return nil
}

// ObserveArgs retains one argument fragment under the per-field and
// aggregate bounds. Both checks are overflow-safe (subtract before
// compare). Independent of the lifecycle: opaque item syntax plus
// lim.MaxArgsChars, per-field and aggregate caps, cumulative identity
// budget, distinct-block and tool-call counts, and kind coherence are
// all re-checked here.
func (b *TerminalBuffer) ObserveArgs(item ItemID, frag string) *RouterError {
	if !opaqueBounded(string(item), b.lim) {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream opened a response block without identity"}
	}
	if kind, ok := b.kinds[item]; ok && kind != BlockTool {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent a delta for a closed or unopened block"}
	}
	if _, ok := b.kinds[item]; !ok {
		if len(b.kinds) >= b.lim.MaxStreamBlocks {
			return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream exceeds the content block bound"}
		}
		next, rerr := addBytes(b.identBytes, len(string(item)))
		if rerr != nil || next > int64(b.lim.MaxStreamArgsChars) {
			return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream response exceeds the size bound"}
		}
		b.identBytes = next
		tools := 0
		for _, k := range b.kinds {
			if k == BlockTool || k == BlockCustom {
				tools++
			}
		}
		if tools+1 > b.lim.MaxToolCalls {
			return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call count exceeds the size bound"}
		}
	}
	s := b.slotFor(b.args, item, BlockTool)
	if len(frag) > b.lim.MaxArgsChars-len(s.data) || len(frag) > b.lim.MaxStreamArgsChars-b.total {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call exceeds the size bound"}
	}
	s.data = append(s.data, frag...)
	b.total += len(frag)
	return nil
}

// ObserveCustom retains one freeform custom input fragment under the
// per-field and aggregate bounds. No JSON validation occurs; the exact
// string is preserved. Tool-call counting includes custom blocks.
func (b *TerminalBuffer) ObserveCustom(item ItemID, frag string) *RouterError {
	if !opaqueBounded(string(item), b.lim) {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream opened a response block without identity"}
	}
	if kind, ok := b.kinds[item]; ok && kind != BlockCustom {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent a delta for a closed or unopened block"}
	}
	if _, ok := b.kinds[item]; !ok {
		if len(b.kinds) >= b.lim.MaxStreamBlocks {
			return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream exceeds the content block bound"}
		}
		next, rerr := addBytes(b.identBytes, len(string(item)))
		if rerr != nil || next > int64(b.lim.MaxStreamArgsChars) {
			return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream response exceeds the size bound"}
		}
		b.identBytes = next
		tools := 0
		for _, k := range b.kinds {
			if k == BlockTool || k == BlockCustom {
				tools++
			}
		}
		if tools+1 > b.lim.MaxToolCalls {
			return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call count exceeds the size bound"}
		}
	}
	s := b.slotFor(b.args, item, BlockCustom)
	if len(frag) > b.lim.MaxArgsChars-len(s.data) || len(frag) > b.lim.MaxStreamArgsChars-b.total {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call exceeds the size bound"}
	}
	s.data = append(s.data, frag...)
	b.total += len(frag)
	return nil
}

// CustomResult returns the retained freeform input for one custom block.
func (b *TerminalBuffer) CustomResult(item ItemID) string {
	if s, ok := b.args[item]; ok {
		return string(s.data)
	}
	return ""
}

// ArgsResult validates the accumulated arguments for one tool block as
// a complete JSON object (empty accumulation means source-absent args)
// and returns the compact exact bytes.
func (b *TerminalBuffer) ArgsResult(item ItemID) ([]byte, *RouterError) {
	raw := []byte("{}")
	if s, ok := b.args[item]; ok && len(s.data) > 0 {
		raw = s.data
	}
	return CompactArgs(raw)
}

// TextResult returns the retained text for one block.
func (b *TerminalBuffer) TextResult(item ItemID) string {
	if s, ok := b.text[item]; ok {
		return string(s.data)
	}
	return ""
}
