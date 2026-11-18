// Canonical Response IR: shared semantic types for the response path.
//
// The response path uses the same typed-IR discipline as the request path
// (Generation): decode source -> canonical Response / semantic events ->
// encode destination. Pairwise translators that convert one dialect's JSON
// straight into another dialect's JSON are the migration source, not the
// target architecture. This file is additive: it changes no existing server,
// config, transport, request-decoder, or pair-translator behavior.
//
// Ownership split (see docs/go-response-ir-migration.md):
//   - Source codecs own byte validation (envelopes, unknown-field policy,
//     per-source emptiness rules, argument JSON validity).
//   - This IR owns semantic lifecycle validation (IDs, kinds, order,
//     counts, hashes) plus canonical usage/finish meaning.
//   - Target encoders own framing, placeholders, ID minting, and any
//     terminal-envelope reconstruction (opt-in, bounded, target-owned).
//
// What this file owns:
//   - Dialect provenance tags.
//   - Opaque identity syntax (validated constructors; see the honesty note
//     on NewResponseID about what string aliases cannot enforce).
//   - Ordered content variants TextItem | RefusalItem | ToolCallItem.
//     Refusal is a distinct variant (never folded into text here):
//     Chat->Anthropic streaming has no incremental refusal form and must
//     fail post-commit rather than silently fold, while Chat->Responses
//     renders a refusal output item (see D7 in the contract audit).
//   - Exact argument bytes (CompactArgs / EmptyArgs, numbers preserved).
//   - Closed Finish kinds that preserve the distinctions existing
//     destinations need (stop / length / content-filter / refusal /
//     tool-calls); unknown stops never become a Finish.
//   - The sealed ResponseEvent set for streaming.
//   - Response: one complete generation (non-stream JSON meaning), which
//     streaming must reproduce exactly.
//
// Currently claimed scope is text plus ordinary function tools. Reasoning,
// media, and search have no IR variants on purpose: encountering them is
// an explicit decode failure in the source codec, which also reserves the
// provenance needed to represent them later without changing this seam.
package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Dialect identifies one of the four wire dialects. It is the provenance
// tag carried by decoded Responses and the destination selector for
// representability checks.
type Dialect string

const (
	DialectChat      Dialect = "chat"
	DialectResponses Dialect = "responses"
	DialectAnthropic Dialect = "anthropic"
	DialectGemini    Dialect = "gemini"
)

// MaxIdentityChars bounds TOOL NAMES ONLY (1..64, [A-Za-z0-9_-],
// matching the existing validateToolName contract and the Anthropic
// name grammar). It is NOT an opaque-identity policy: response, item
// and call IDs are provider-minted opaque handles validated by
// validOpaqueID (nonempty after trimming, bounded at decode by
// lim.MaxArgsChars via validateCallID), never by this grammar. See
// TestG2CRegrOpaqueCallIDsPreserved ("has:colon and space", 100-char
// IDs preserved) and chat_decode.go validateCallID.
const MaxIdentityChars = 64

// validOpaqueID reports whether s is acceptable as an opaque response,
// item or call identity: nonempty after trimming whitespace and valid
// UTF-8. There is deliberately NO length cap and NO character grammar
// here: provider handles may contain spaces or colons, and byte budgets
// are enforced at decode (source codec, Limits) and transport, exactly
// like the existing responseString/validateCallID source checks.
func validOpaqueID(s string) bool {
	if s == "" || strings.TrimSpace(s) == "" {
		return false
	}
	return utf8.ValidString(s)
}

// --- Opaque identities ---------------------------------------------------

// ResponseID is the source generation identity (Chat id, Responses id,
// Anthropic id, Gemini responseId). Opaque: never parsed, split, or
// rewritten by the IR; destination encoders mint their own IDs.
type ResponseID string

// ItemID identifies one ordered output item within a generation
// (Responses item id, Chat choice/tool index surrogate, Anthropic block
// surrogate). Distinct from CallID by construction: an item carries
// output position, a call carries function identity.
type ItemID string

// CallID is the upstream tool-call identity echoed back in tool results
// (Chat tool_calls[].id, Responses call_id, Anthropic tool_use id,
// Gemini functionCall id). Opaque: preserved byte-for-byte, never
// namespaced or reinterpreted by the IR.
type CallID string

// validIdentity is the TOOL-NAME grammar only (1..64, no
// whitespace/control, valid UTF-8; full charset enforced by
// validateToolName at encode). It never applies to opaque response,
// item or call IDs, which use validOpaqueID plus the decoding
// lifecycle's lim.MaxArgsChars bound (provider handles may contain
// spaces or colons and run past 64 chars; see TestG2CRegrOpaqueCallIDs).
func validIdentity(s string) bool {
	if len(s) == 0 || len(s) > MaxIdentityChars {
		return false
	}
	for _, r := range s {
		if r <= 0x20 || r == 0x7f {
			return false
		}
	}
	return utf8.ValidString(s)
}

// NewResponseID validates a source response identity without
// interpreting it. Opaque: whitespace-only carries no identity; length
// and framing budgets are enforced by the decoding source codec.
func NewResponseID(s string) (ResponseID, *RouterError) {
	if !validOpaqueID(s) {
		return "", &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response identity"}
	}
	return ResponseID(s), nil
}

// NewItemID validates an output-item identity without interpreting it.
func NewItemID(s string) (ItemID, *RouterError) {
	if !validOpaqueID(s) {
		return "", &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response item identity"}
	}
	return ItemID(s), nil
}

// NewCallID validates an opaque tool-call identity without namespacing
// or otherwise interpreting it.
func NewCallID(s string) (CallID, *RouterError) {
	if !validOpaqueID(s) {
		return "", &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid tool call identity"}
	}
	return CallID(s), nil
}

// NewToolName validates a tool name against the cross-dialect tool
// identity contract (1..64, [A-Za-z0-9_-], mirroring validateToolName):
// the response path preserves names, it does not authorize them, but
// the grammar itself is enforced per target at encode (Anthropic and
// Responses refuse grammar-invalid names; the Chat->Gemini path
// provably passes them through, so IR construction must not refuse
// them). Only names carry this grammar; opaque IDs never do.
func NewToolName(s string) (string, *RouterError) {
	if !validIdentity(s) {
		return "", &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid tool name"}
	}
	return s, nil
}

// IdentityOrigin names who minted an identity: the upstream provider
// or the router (compatibility mint when the provider omitted it).
// Provenance is explicit, never inferred from string prefixes: a
// provider may legitimately supply an id that collides with the
// router's "gemini-compat-" namespace, and only the origin flag
// distinguishes the two.
type IdentityOrigin string

const (
	IdentityProvider     IdentityOrigin = "provider"
	IdentityRouterMinted IdentityOrigin = "router"
)

// --- Ordered content variants (closed) -----------------------------------

// ResponseItem is one ordered output element of a complete generation.
// The interface is sealed: exactly the three structs below implement it,
// so a switch over the three variants is exhaustive and no
// map[string]any payload can pass through untyped.
//
// Honesty note: TextItem/RefusalItem/ToolCallItem are public structs, so
// a malformed literal (empty ID, non-object args) remains constructible.
// NewResponse and ResponseLifecycle.Result re-validate every item, so
// literals never enter encoded output unchecked.
type ResponseItem interface {
	responseItem()
	responseItemID() ItemID
}

// TextItem is one ordered text span. Empty text is representable and
// must be preserved by encoders (an explicitly empty text span still
// carries its message item); it is never silently dropped.
type TextItem struct {
	ID   ItemID
	Text string
}

func (TextItem) responseItem()            {}
func (t TextItem) responseItemID() ItemID { return t.ID }

// RefusalItem is one ordered refusal span. It is distinct from TextItem:
// folding refusal into text is a target-encoder decision the IR must not
// make globally (streaming refusal to Anthropic has no incremental form
// and must fail post-commit; see the contract audit D7).
type RefusalItem struct {
	ID   ItemID
	Text string
}

func (RefusalItem) responseItem()            {}
func (t RefusalItem) responseItemID() ItemID { return t.ID }

// ToolCallItem is one ordered function invocation. Name and CallID are
// opaque source identities; Arguments is the exact compact JSON object
// (never a scalar, array, or string fragment). Namespace is the optional
// Responses namespace restored from the request plan ("" = plain); only
// the Responses target renders it, every other target refuses it via
// CheckRepresentable rather than silently dropping it.
type ToolCallItem struct {
	ID        ItemID
	CallID    CallID
	Namespace string
	Name      string
	Arguments json.RawMessage
}

func (ToolCallItem) responseItem()            {}
func (t ToolCallItem) responseItemID() ItemID { return t.ID }

// CustomCallItem is one ordered freeform custom invocation. Input is the
// exact preserved string (never JSON-decoded, never passed through
// CompactArgs). Namespace mirrors ToolCallItem ("" = plain); only the
// Chat and Responses targets render custom calls, every other target
// refuses them via CheckRepresentable.
type CustomCallItem struct {
	ID        ItemID
	CallID    CallID
	Namespace string
	Name      string
	Input     string
}

func (CustomCallItem) responseItem()            {}
func (c CustomCallItem) responseItemID() ItemID { return c.ID }

// EmptyArgs is the explicit marker for source-absent tool arguments
// (Chat null/missing/empty-string arguments). Decoders use it so
// "absent" never hides inside an empty string; encoders render it
// per destination contract. Note: this defaulting applies to Chat
// source only. A complete Anthropic tool_use block REQUIRES a present
// input object — missing or null Anthropic input must refuse (proven
// by anthropic_to_gemini_json.go a2gemResponseParts), never default
// to {}.
func EmptyArgs() json.RawMessage { return json.RawMessage("{}") }

// CompactArgs validates that raw is a JSON object and returns its compact
// form with number literals preserved exactly (no float64 round-trip, so
// large integers survive). Scalars, arrays, and invalid JSON fail.
func CompactArgs(raw json.RawMessage) (json.RawMessage, *RouterError) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call arguments are not valid JSON"}
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call arguments are not valid JSON"}
	}
	if _, ok := v.(map[string]any); !ok {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call arguments are not a JSON object"}
	}
	if dec.More() {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call arguments are not valid JSON"}
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, trimmed); err != nil {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call arguments are not valid JSON"}
	}
	return json.RawMessage(compact.Bytes()), nil
}

// NewToolCallItem builds a validated tool-call item. Arguments must
// already be a compact JSON object (see CompactArgs); use EmptyArgs for
// source-absent arguments so the normalization is explicit at decode.
// Name must be nonempty; the 1..64 tool-name grammar is enforced
// per-target at encode (CheckRepresentable), never here, because the
// Chat->Gemini path provably renders grammar-invalid names while the
// Chat->Anthropic/Responses paths refuse them.
func NewToolCallItem(id ItemID, call CallID, name string, args json.RawMessage) (ToolCallItem, *RouterError) {
	return NewToolCallItemWithNamespace(id, call, "", name, args)
}

// NewToolCallItemWithNamespace builds a validated tool-call item carrying
// the optional Responses namespace restored from the request plan.
// Namespace == "" is a plain function and behaves exactly as before.
func NewToolCallItemWithNamespace(id ItemID, call CallID, namespace, name string, args json.RawMessage) (ToolCallItem, *RouterError) {
	if !validOpaqueID(string(id)) || !validOpaqueID(string(call)) || strings.TrimSpace(name) == "" {
		return ToolCallItem{}, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call is missing its identity"}
	}
	compacted, rerr := CompactArgs(args)
	if rerr != nil {
		return ToolCallItem{}, rerr
	}
	return ToolCallItem{ID: id, CallID: call, Namespace: namespace, Name: name, Arguments: compacted}, nil
}

// NewCustomCallItem builds a validated custom-call item. Input is the
// exact preserved string (UTF-8, bounded by the source codec); it is
// never JSON-decoded. Name must be nonempty here while its 1..64 grammar
// stays target-side (see NewToolCallItem).
func NewCustomCallItem(id ItemID, call CallID, namespace, name, input string) (CustomCallItem, *RouterError) {
	if !validOpaqueID(string(id)) || !validOpaqueID(string(call)) || strings.TrimSpace(name) == "" {
		return CustomCallItem{}, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call is missing its identity"}
	}
	if !utf8.ValidString(input) {
		return CustomCallItem{}, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call is missing its identity"}
	}
	return CustomCallItem{ID: id, CallID: call, Namespace: namespace, Name: name, Input: input}, nil
}

// --- Terminal finish (closed, distinctions preserved) ----------------------

// FinishKind is one reviewed generation terminal. The set preserves the
// distinctions existing destinations need instead of coarsening them:
//   - FinishStop: natural stop / end_turn / completed.
//   - FinishLength: length / max_tokens / max_output_tokens cutoff.
//   - FinishContentFilter: safety / content-filter cutoff.
//   - FinishRefusal: explicit refusal (never downgraded to stop).
//   - FinishToolCalls: stop for tool calls (tool_use / functionCall).
//
// Unknown source stops are decode failures in the source codec, never
// new FinishKind variants.
type FinishKind string

const (
	FinishStop          FinishKind = "stop"
	FinishLength        FinishKind = "length"
	FinishContentFilter FinishKind = "content_filter"
	FinishRefusal       FinishKind = "refusal"
	FinishToolCalls     FinishKind = "tool_calls"
)

// Finish is the single terminal of a generation. Raw carries the source
// stop string for provenance (encoder mapping, never echoed into
// value-free diagnostics).
type Finish struct {
	Kind FinishKind
	Raw  string
}

// NewFinish validates a terminal kind at the type boundary.
func NewFinish(kind FinishKind, raw string) (Finish, *RouterError) {
	switch kind {
	case FinishStop, FinishLength, FinishContentFilter, FinishRefusal, FinishToolCalls:
		return Finish{Kind: kind, Raw: raw}, nil
	default:
		return Finish{}, &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: fmt.Sprintf("Upstream stop reason %q has no representation", string(kind))}
	}
}

func (f Finish) validate() *RouterError {
	if _, rerr := NewFinish(f.Kind, f.Raw); rerr != nil {
		return rerr
	}
	return nil
}

// --- Semantic events (closed) ----------------------------------------------

// BlockKind distinguishes text spans, refusal spans, and tool-call
// blocks in the ordered content sequence.
type BlockKind string

const (
	BlockText    BlockKind = "text"
	BlockRefusal BlockKind = "refusal"
	BlockTool    BlockKind = "tool"
	BlockCustom  BlockKind = "custom"
)

// ResponseEvent is one semantic stream event. The interface is sealed:
// exactly the structs below implement it, so decoders cannot emit and
// validators cannot receive an unlisted variant.
type ResponseEvent interface {
	responseEvent()
}

// ResponseStart opens a generation. Exactly one per stream, always
// first. Model is the upstream model name; the Anthropic and Gemini
// streaming sources preserve a wire-omitted model as "" (never a fake
// provider string), exactly like the Responses streaming source when
// the provider omits model at response.created and reports it later
// via ResponseModelEvent. Chat still requires a model at start.
// Source names the dialect being decoded. InitialUsage is optional
// start-time accounting for future sources that report it: nil means
// absent (Chat never reports start accounting; the Anthropic target
// renders its zero placeholder). When present it is preserved verbatim
// by target encoders via the shared usage accounting (never fabricated
// when nil).
type ResponseStart struct {
	ID     ResponseID
	Model  string
	Source Dialect
	// InitialUsage carries start-time accounting when the source
	// reports it. Nil means the source reported nothing at start.
	InitialUsage *ResponseUsage
	// IDOrigin names who minted ID. Empty reads as provider (every
	// current emitter never mints, so this documents
	// construction-site fact, never prefix inference). Gemini sets it
	// explicitly on every start: provider iff observed on the wire,
	// router iff minted (geminiCompatResponseID).
	IDOrigin IdentityOrigin
	// ModelOrigin names who supplied Model. Empty reads as provider.
	// Gemini sets it explicitly: provider iff modelVersion observed,
	// router iff absent (Model == "").
	ModelOrigin IdentityOrigin
}

func (ResponseStart) responseEvent() {}

// ResponseModelEvent reports the provider model when a source that
// truthfully started without one (Responses created without model,
// like the accepted Anthropic/Gemini absence seam) later reports it
// in a progress envelope or the actual terminal response object. It is
// the ONLY model-reporting event: no arbitrary map, no guessed IDs or
// source, and no silent default. The lifecycle validates it (started,
// nonempty, immutable once known with a value-free drift failure) and
// retains it in Result.Model so the terminal summary carries the
// actual provider model before CompletionEvent. Target encoders
// consume it metadata-only: they update their upstream-model record
// for policy/terminal summaries and write zero downstream bytes,
// leaving emitted wrapper IDs and the public alias untouched.
type ResponseModelEvent struct {
	Model string
}

func (ResponseModelEvent) responseEvent() {}

// ProviderIdentityEvent reports the late-arriving provider responseId
// when a source that truthfully started with a router-minted ID
// (Gemini absent responseId) later observes it on the wire. Implicit
// origin provider (the decoder observed it); a value equal to the
// minted start ID flips origin provider without changing the string,
// so provider-supplied "gemini-compat-..." strings stay truthful and
// are never added to the minted set. At-most-once, pre-terminal only,
// requires started; differing later values conflict, never a second
// event.
type ProviderIdentityEvent struct {
	ID ResponseID
}

func (ProviderIdentityEvent) responseEvent() {}

// BlockOpen opens one ordered content block identified by its ItemID.
// Blocks open in arrival order; there is deliberately no dense integer
// index requirement (0, 1, 2, ...): sources with sparse or string
// identifiers map them to ItemIDs, and the lifecycle enforces uniqueness
// plus open-before-delta/close instead of inventing index arithmetic.
// A tool block carries its complete opaque identity up front
// (fragmented tool names have no reviewed mapping and fail at source
// decode, so by the time a block opens its identity is final); a text
// or refusal block carries Item only.
//
// Tool blocks from indexed sources (Chat tool_calls[].index, Anthropic
// content index, Responses output_index) additionally carry the
// source-dialect logical position in SourceIndex so targets can apply
// reviewed logical ordering WITHOUT reparsing the opaque ItemID:
// ItemIDs stay opaque handles, never index encodings. Text and refusal
// blocks from indexed sources (Anthropic content index, Responses
// output_index for single-part text/refusal) carry the same logical
// position; Chat text stays unindexed and Chat tools keep their
// tool-index namespace.
type BlockOpen struct {
	Item ItemID
	Kind BlockKind
	Call CallID // tool blocks only
	// Namespace is the optional Responses namespace restored from the
	// request plan (tool blocks only, "" = plain). The restoring stage
	// sets it from the binding table; source decoders always leave it
	// empty and other targets refuse it via CheckRepresentable.
	Namespace string
	Name      string // tool blocks only
	// SourceIndex is the optional source-dialect logical position.
	// Nil means absent; a non-nil zero is an explicit index zero.
	// Any block kind may carry one when its source numbers content
	// (Anthropic content index, Responses output_index, Chat tool
	// index for tools). Chat text never carries one.
	SourceIndex *int
	// CallOrigin names who minted Call. Empty reads as provider.
	// Only the Gemini source decoder ever sets router (minted compat
	// call id); provider-supplied ids — even colliding with the
	// compat prefix — stay provider.
	CallOrigin IdentityOrigin
}

func (BlockOpen) responseEvent() {}

// NewBlockOpen validates block-opening identity eagerly: tool blocks
// require complete item/call/name, text and refusal blocks require item
// and forbid call/name. Item/call identities are opaque (validOpaqueID);
// name must be nonempty here while its 1..64 grammar stays target-side
// (see NewToolCallItem). Namespace stays empty on source-decoded blocks;
// the restoring stage sets it via NewBlockOpenWithNamespace.
func NewBlockOpen(item ItemID, kind BlockKind, call CallID, name string) (BlockOpen, *RouterError) {
	return NewBlockOpenWithNamespace(item, kind, call, "", name)
}

// NewBlockOpenWithNamespace validates a block opening carrying the optional
// Responses namespace restored from the request plan. Text/refusal blocks
// forbid namespace, call and name; tool blocks allow an optional namespace
// with a required nonempty name. Custom blocks carry freeform string input
// downstream and follow the same identity rules as tool blocks.
func NewBlockOpenWithNamespace(item ItemID, kind BlockKind, call CallID, namespace, name string) (BlockOpen, *RouterError) {
	if !validOpaqueID(string(item)) {
		return BlockOpen{}, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream opened a response block without identity"}
	}
	switch kind {
	case BlockText, BlockRefusal:
		if call != "" || namespace != "" || name != "" {
			return BlockOpen{}, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream text block carries a tool identity"}
		}
	case BlockTool, BlockCustom:
		if !validOpaqueID(string(call)) || strings.TrimSpace(name) == "" {
			return BlockOpen{}, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call is missing its identity"}
		}
	default:
		return BlockOpen{}, &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: fmt.Sprintf("Upstream response block kind %q has no representation", string(kind))}
	}
	return BlockOpen{Item: item, Kind: kind, Call: call, Namespace: namespace, Name: name}, nil
}

// NewIndexedBlockOpen validates a block opening with its source-dialect
// logical position for any block kind. Identity rules match NewBlockOpen
// (tools require call/name, text/refusal forbid them); index must be
// nonnegative and is retained in the lifecycle summary so targets order
// by typed provenance instead of reparsing the opaque ItemID. Sources
// without logical indices keep using NewBlockOpen (index absent).
func NewIndexedBlockOpen(item ItemID, kind BlockKind, call CallID, name string, index int) (BlockOpen, *RouterError) {
	if index < 0 {
		return BlockOpen{}, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	}
	open, rerr := NewBlockOpen(item, kind, call, name)
	if rerr != nil {
		return BlockOpen{}, rerr
	}
	open.SourceIndex = intPtr(index)
	return open, nil
}

// NewToolBlockOpen validates a tool block opening with its source-dialect
// logical position. Identity rules match NewBlockOpen; index must be
// nonnegative and is retained in the lifecycle summary so targets order
// by typed provenance instead of reparsing the opaque ItemID. Sources
// without logical indices keep using NewBlockOpen (index absent).
func NewToolBlockOpen(item ItemID, call CallID, name string, index int) (BlockOpen, *RouterError) {
	return NewIndexedBlockOpen(item, BlockTool, call, name, index)
}

// TextFragment appends text to an open text block. Empty fragments are
// accepted at the type boundary: whether an empty delta is legal is a
// source-contract question (only the source decoder may reject it), not
// a universal lifecycle rule. An explicitly empty text span is a TextItem
// with "", never a dropped delta.
type TextFragment struct {
	Item ItemID
	Text string
}

func (TextFragment) responseEvent() {}

// NewTextFragment validates block identity. Emptiness is intentionally
// not rejected here; see the type comment.
func NewTextFragment(item ItemID, text string) (TextFragment, *RouterError) {
	if !validOpaqueID(string(item)) {
		return TextFragment{}, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent a text delta without identity"}
	}
	return TextFragment{Item: item, Text: text}, nil
}

// ArgsFragment appends one argument fragment to an open tool block.
// Fragments are opaque until BlockClose, where the concatenation must
// parse as one JSON object (validated by the source decoder, which sees
// the bytes, and re-validated by any target-owned terminal
// reconstruction). Empty fragments are accepted here for the same
// reason as TextFragment: only the source contract may forbid them.
// A block whose bytes never form an object is NOT a generic failure:
// the source decoder classifies it with MalformedToolEvent (typed
// provenance for the target gate), never a FailureEvent.
type ArgsFragment struct {
	Item     ItemID
	Fragment string
}

func (ArgsFragment) responseEvent() {}

// NewArgsFragment validates block identity. Emptiness is intentionally
// not rejected here; see the type comment.
func NewArgsFragment(item ItemID, fragment string) (ArgsFragment, *RouterError) {
	if !validOpaqueID(string(item)) {
		return ArgsFragment{}, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent a tool argument delta without identity"}
	}
	return ArgsFragment{Item: item, Fragment: fragment}, nil
}

// CustomDelta appends one freeform string fragment to an open custom
// block. Fragments are opaque until BlockClose, where the concatenation
// is the exact preserved custom input (never JSON-decoded). Empty
// fragments are accepted here like ArgsFragment; bounds and terminal
// consistency are enforced by the source decoder and the shared
// lifecycle. Custom deltas never become JSON argument bytes.
type CustomDelta struct {
	Item  ItemID
	Delta string
}

func (CustomDelta) responseEvent() {}

// NewCustomDelta validates block identity for a custom input fragment.
func NewCustomDelta(item ItemID, delta string) (CustomDelta, *RouterError) {
	if !validOpaqueID(string(item)) {
		return CustomDelta{}, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent a tool argument delta without identity"}
	}
	return CustomDelta{Item: item, Delta: delta}, nil
}

// BlockClose closes one open block by ItemID. No deltas for that item
// may follow.
type BlockClose struct {
	Item ItemID
}

func (BlockClose) responseEvent() {}

// MalformedToolEvent classifies one open tool block whose accumulated
// argument bytes are not a valid JSON object (truncated or non-object
// payload). It is typed source-fact provenance, NOT a generic pipeline
// failure: the stream stays structurally valid (usage and completion
// still required) so a later Gemini target can suppress the malformed
// call and report its native malformed finish, while Anthropic and
// Responses target gates refuse. Syntax, identity, and size errors
// remain *RouterError 502s at the source decoder and never become
// this event. Analogous to Response.MalformedTools on the complete
// row, except stream blocks keep their identity (the event names the
// block) while complete-row calls are omitted from Items.
type MalformedToolEvent struct {
	Item ItemID
}

func (MalformedToolEvent) responseEvent() {}

// UsageEvent carries accounting. Non-terminal scopes are informational;
// exactly one terminal UsageEvent per generation is required and must
// be authoritative (see ResponseUsage.ValidateAuthoritative).
type UsageEvent struct {
	Usage ResponseUsage
}

func (UsageEvent) responseEvent() {}

// Terminal reports whether this accounting is authoritative.
func (u UsageEvent) Terminal() bool { return u.Usage.Provenance.Scope == UsageTerminal }

// CompletionEvent is the single semantic terminal of a generation. It
// must come after every block is closed; nothing but the framing
// terminal may follow it. The pipeline stops reading upstream as soon
// as the lifecycle observes it: completion followed by additional reads
// would hang, so reads never continue past this point.
type CompletionEvent struct {
	Finish Finish
}

func (CompletionEvent) responseEvent() {}

// NewCompletionEvent enforces finish closedness at the type boundary.
func NewCompletionEvent(finish Finish) (CompletionEvent, *RouterError) {
	if rerr := finish.validate(); rerr != nil {
		return CompletionEvent{}, rerr
	}
	return CompletionEvent{Finish: finish}, nil
}

// FailureEvent is the post-commit semantic failure: the source stream
// contradicted itself after downstream headers were committed, so no
// success terminal may be emitted. The streaming pipeline centralizes
// failure output in one fail() helper that calls Abort at most once;
// FailureEvent is never passed to EventEncoder.Encode (see
// response_ir_pipeline.go feed(): a validated failure returns the
// terminal error directly, bypassing Encode). Pre-commit failures never
// become events: they return a *RouterError that maps to an HTTP error.
type FailureEvent struct {
	// Code is the machine-readable failure class (CodeUpstreamBad for
	// response-path semantic failures today; kept as a field so future
	// classes do not change the event shape).
	Code string
	// Field is the value-free field path, never content.
	Field string
}

func (FailureEvent) responseEvent() {}

// --- Complete generation ---------------------------------------------------

// Response is one complete generation in canonical form. Items preserve
// source order one-to-one; Usage is authoritative terminal accounting;
// Finish is the single terminal. This is the non-stream meaning that
// streaming must reproduce exactly.
//
// MalformedTools counts source tool calls the decoder omitted as
// malformed (present function object with a missing/empty name or a
// non-object argument payload). This is a Chat-source-row provenance
// fact the IR must carry because the destinations diverge by design:
// Chat->Gemini renders MALFORMED_FUNCTION_CALL while Chat->Anthropic
// and Chat->Responses refuse the same bytes (see CheckRepresentable).
// Decoders set it after NewResponse; it is never negative.
type Response struct {
	ID     ResponseID
	Model  string
	Source Dialect
	Items  []ResponseItem
	Usage  ResponseUsage
	Finish Finish
	// MalformedTools counts omitted malformed source tool calls (Chat
	// row only; see above). Target encoders must not silently drop it:
	// CheckRepresentable refuses it everywhere except its one reviewed
	// rendering (Gemini MALFORMED_FUNCTION_CALL).
	MalformedTools int
	// IDOrigin names who minted ID: provider-supplied or router-minted
	// compatibility (Gemini row only today: responseId absent). Never
	// inferred from the ID string: a provider may supply the same
	// "gemini-compat-" prefix the router mints.
	IDOrigin IdentityOrigin
	// ModelOrigin names who supplied Model: provider modelVersion or
	// router absence (Gemini row only today: modelVersion absent leaves
	// Model empty). Empty Model is valid only with a router origin;
	// provider origins always carry a nonempty model.
	ModelOrigin IdentityOrigin
	// RouterMintedCalls tracks tool call IDs the router minted because
	// the provider omitted functionCall.id (Gemini row only today).
	// Provider-supplied IDs — even ones colliding with the compat
	// prefix — are never added here. Encoders preserve call IDs
	// verbatim (tool-result correlation requires it); this set proves
	// router ownership without rewriting provider IDs.
	RouterMintedCalls map[CallID]struct{}
}

// NewResponse builds a complete generation, enforcing item identity
// syntax and uniqueness (both item IDs and call IDs), argument
// validity, usage authoritativeness, and finish closedness. Item and
// call identities are opaque (validOpaqueID); tool names must be
// nonempty here while their 1..64 grammar stays target-side (see
// NewToolCallItem). Order is preserved as given: reordering is an
// encoder concern, never a decoder rewrite.
//
// Identity provenance defaults to provider: IDOrigin and ModelOrigin
// are set to IdentityProvider when supplied; an absent Gemini model has
// IdentityRouterMinted origin. The Gemini source decoder overrides
// them (plus RouterMintedCalls) when the provider omits responseId,
// modelVersion, or functionCall.id. Empty Model is accepted only for
// the Gemini source (absent modelVersion expresses absence as "" with
// a router origin, never a fake provider string) and the Anthropic
// streaming source (the minimal legacy message_start omits model and
// the staged source decoder preserves that absence as ""); the
// Anthropic COMPLETE decoder still rejects a missing model at its own
// wire boundary. Every other source still requires a nonempty model,
// and empty IDs are never valid.
func NewResponse(id ResponseID, model string, source Dialect, items []ResponseItem, usage ResponseUsage, finish Finish) (Response, *RouterError) {
	if !validOpaqueID(string(id)) {
		return Response{}, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	}
	if model == "" && source != DialectGemini && source != DialectAnthropic {
		return Response{}, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	}
	modelOrigin := IdentityProvider
	if model == "" {
		modelOrigin = IdentityRouterMinted
	}
	if rerr := usage.ValidateAuthoritative(); rerr != nil {
		return Response{}, rerr
	}
	if rerr := finish.validate(); rerr != nil {
		return Response{}, rerr
	}
	seenItems := map[ItemID]bool{}
	seenCalls := map[CallID]bool{}
	for i, it := range items {
		id := it.responseItemID()
		if !validOpaqueID(string(id)) {
			return Response{}, &RouterError{Status: 502, Code: CodeUpstreamBad,
				Msg: fmt.Sprintf("Upstream response item %d has no identity", i)}
		}
		if seenItems[id] {
			return Response{}, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned conflicting tool call identities"}
		}
		seenItems[id] = true
		if call, ok := it.(ToolCallItem); ok {
			if !validOpaqueID(string(call.CallID)) || strings.TrimSpace(call.Name) == "" {
				return Response{}, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call is missing its identity"}
			}
			if _, rerr := CompactArgs(call.Arguments); rerr != nil {
				return Response{}, rerr
			}
			if seenCalls[call.CallID] {
				return Response{}, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned conflicting tool call identities"}
			}
			seenCalls[call.CallID] = true
		}
		if call, ok := it.(CustomCallItem); ok {
			if !validOpaqueID(string(call.CallID)) || strings.TrimSpace(call.Name) == "" {
				return Response{}, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call is missing its identity"}
			}
			if !utf8.ValidString(call.Input) {
				return Response{}, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call is missing its identity"}
			}
			if seenCalls[call.CallID] {
				return Response{}, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned conflicting tool call identities"}
			}
			seenCalls[call.CallID] = true
		}
	}
	return Response{ID: id, Model: model, Source: source, Items: items, Usage: usage, Finish: finish,
		IDOrigin: IdentityProvider, ModelOrigin: modelOrigin}, nil
}
