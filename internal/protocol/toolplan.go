// Responses namespace ToolPlan: first M5 runtime unit (ordinary functions only).
//
// Typed caller/interface sketch (implemented below, no stubs):
//   - ToolIdentity  : source identity {Namespace, Name}; empty Namespace = plain.
//   - ToolBinding   : Source identity + flat Alias + static Origin path.
//   - ToolPlan      : immutable request-scoped plan (bindings built once from
//     validated source names + complete history; lookup maps derived
//     internally, never mutated after Build).
//   - Builder       : validates plain/namespace shapes, reserves collisions,
//     mints ns_<sha56> aliases, records origins tools[i] vs tools[i].tools[j].
//   - RestoreResponse: one semantic restoring step after source-neutral decode.
//   - WrapDecoder   : single typed stream stage rewriting BlockOpen identities.
//
// Scope: Responses ingress FUNCTION namespaces only. Plain function names,
// duplicate children across namespaces, unknown names, and schema/version
// failures are strict 400s before loss. Custom/freeform, hosted/vendor,
// versioned client tools, and execution are out of scope for this unit and
// remain refused by the existing validators. Native Responses (same-proto)
// stays raw: a nil/empty plan restores nothing.
package protocol

import (
	"crypto/sha256"
	"encoding/hex"
)

// ToolIdentity is a source tool identity. Namespace == "" means a plain
// top-level function; otherwise the function lives in that namespace group.
type ToolIdentity struct {
	Namespace string
	Name      string
}

// ToolBinding maps one source identity onto its flattened upstream alias.
// Origin is the static source path (tools[i] or tools[i].tools[j]);
// provenance always comes from this binding, never from parsing an alias.
type ToolBinding struct {
	Source ToolIdentity
	Alias  string
	Origin string
}

// ToolPlan is an immutable request-scoped namespace plan. Bindings are fixed
// at Build time; byAlias/bySource are derived indexes, never mutated after.
// Custom entries track native freeform tool names (Chat ingress custom
// vertical) with their source origins; they do not affect Empty, which
// remains namespace-only so native custom restoration stays typed without
// a wrapping plan. Emulated customs track Responses ingress freeform tools
// (approximated as string functions upstream); they do affect Empty because
// restoration must run even for plain (non-namespaced) customs.
type ToolPlan struct {
	bindings     []ToolBinding
	byAlias      map[string]ToolIdentity
	bySource     map[ToolIdentity]string
	origins      map[string]string
	declarations []ToolDeclaration
	customNative map[string]string
	emuCustom    map[string]ToolIdentity
}

// IsNativeCustom reports whether name is a native custom tool for this plan.
func (p *ToolPlan) IsNativeCustom(name string) bool {
	if p == nil {
		return false
	}
	_, ok := p.customNative[name]
	return ok
}

// CustomOrigin returns the static source path for a native custom tool.
func (p *ToolPlan) CustomOrigin(name string) (string, bool) {
	if p == nil {
		return "", false
	}
	o, ok := p.customNative[name]
	return o, ok
}

// Bindings returns the plan bindings in build order. The returned slice is a
// copy; mutating it does not affect the plan.
func (p *ToolPlan) Bindings() []ToolBinding {
	if p == nil {
		return nil
	}
	return append([]ToolBinding(nil), p.bindings...)
}

// Empty reports whether the plan carries no namespace bindings and no
// emulated customs (nil plan counts as empty and restores nothing).
// Native custom entries do not affect emptiness (see struct comment).
func (p *ToolPlan) Empty() bool {
	return p == nil || (len(p.bindings) == 0 && len(p.emuCustom) == 0 && !p.hasNamespaceDeclarations())
}

// AliasFor returns the flattened alias for a source identity, if bound.
func (p *ToolPlan) AliasFor(namespace, name string) (string, bool) {
	if p == nil {
		return "", false
	}
	alias, ok := p.bySource[ToolIdentity{Namespace: namespace, Name: name}]
	return alias, ok
}

// SourceFor returns the source identity for a flattened alias.
// Provenance comes from this binding table, never from prefix parsing:
// a plain tool literally named ns_<sha56> never claims ownership.
func (p *ToolPlan) SourceFor(alias string) (ToolIdentity, bool) {
	if p == nil {
		return ToolIdentity{}, false
	}
	id, ok := p.byAlias[alias]
	return id, ok
}

// OriginFor returns the static source path for a flattened tool path.
func (p *ToolPlan) OriginFor(flatPath string) (string, bool) {
	if p == nil {
		return "", false
	}
	o, ok := p.origins[flatPath]
	return o, ok
}

// NamespaceAlias mints the collision-safe flattened identity for a
// (namespace, name) pair: ns_<sha256(namespace + "\0" + name)[:56]>.
// It mirrors model_router.tool_namespaces.flatten_namespaces exactly.
func NamespaceAlias(namespace, name string) string {
	sum := sha256.Sum256([]byte(namespace + "\x00" + name))
	return "ns_" + hex.EncodeToString(sum[:])[:56]
}

// ToolPlanBuilder validates source names once and builds one immutable plan.
// Reserved holds plain top-level names plus non-namespaced history names so
// an alias can never collide with a real function.
type ToolPlanBuilder struct {
	reserved     map[string]bool
	seen         map[string]ToolIdentity
	bindings     []ToolBinding
	origins      map[string]string
	declarations []ToolDeclaration
	customNative map[string]string
}

// AddNativeCustom records one native custom tool name with its source
// origin. Duplicate identical names reuse the first origin; a different
// tool kind claiming the same name is a strict 400 handled by the caller
// via the reserved set.
func (b *ToolPlanBuilder) AddNativeCustom(name, origin string) {
	if b.customNative == nil {
		b.customNative = map[string]string{}
	}
	if _, ok := b.customNative[name]; !ok {
		b.customNative[name] = origin
	}
	if b.reserved != nil {
		b.reserved[name] = true
	}
}

// NewToolPlanBuilder starts a builder for one Responses request.
func NewToolPlanBuilder(reserved []string) *ToolPlanBuilder {
	r := map[string]bool{}
	for _, n := range reserved {
		r[n] = true
	}
	return &ToolPlanBuilder{reserved: r, seen: map[string]ToolIdentity{}}
}

// AddNamespaced validates one (namespace, name) child and records its
// binding. Empty namespace/name, alias collisions with reserved names or
// with a different identity, are strict 400s before any loss. Repeating
// the identical (namespace, name) reuses the alias; the duplicate
// flattened name is then reported by the tool validator as a conflicting
// tool definition.
func (b *ToolPlanBuilder) AddNamespaced(namespace, name, flatPath, origin string) (string, *RouterError) {
	if namespace == "" || name == "" {
		return "", &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: "Tool namespaces and function names must be nonempty strings"}
	}
	alias := NamespaceAlias(namespace, name)
	if prev, ok := b.seen[alias]; ok {
		if prev != (ToolIdentity{Namespace: namespace, Name: name}) {
			return "", &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: "Tool namespace alias collides with another function"}
		}
		if b.origins == nil {
			b.origins = map[string]string{}
		}
		b.origins[flatPath] = origin
		return alias, nil
	}
	if b.reserved[alias] {
		return "", &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: "Tool namespace alias collides with another function"}
	}
	b.seen[alias] = ToolIdentity{Namespace: namespace, Name: name}
	b.reserved[alias] = true
	b.bindings = append(b.bindings, ToolBinding{
		Source: ToolIdentity{Namespace: namespace, Name: name},
		Alias:  alias,
		Origin: origin,
	})
	if b.origins == nil {
		b.origins = map[string]string{}
	}
	b.origins[flatPath] = origin
	return alias, nil
}

// SetOrigin records the static source path for a flattened tool path.
func (b *ToolPlanBuilder) SetOrigin(flatPath, origin string) {
	if b.origins == nil {
		b.origins = map[string]string{}
	}
	b.origins[flatPath] = origin
}

// Build seals one immutable plan. Bindings are copied; later caller
// mutation cannot affect the plan.
func (b *ToolPlanBuilder) Build() *ToolPlan {
	bindings := append([]ToolBinding(nil), b.bindings...)
	byAlias := make(map[string]ToolIdentity, len(bindings))
	bySource := make(map[ToolIdentity]string, len(bindings))
	for _, bd := range bindings {
		byAlias[bd.Alias] = bd.Source
		bySource[bd.Source] = bd.Alias
	}
	origins := map[string]string{}
	for k, v := range b.origins {
		origins[k] = v
	}
	custom := map[string]string{}
	for k, v := range b.customNative {
		custom[k] = v
	}
	return &ToolPlan{bindings: bindings, byAlias: byAlias, bySource: bySource, origins: origins, declarations: append([]ToolDeclaration(nil), b.declarations...), customNative: custom, emuCustom: map[string]ToolIdentity{}}
}

// RestoreResponse maps flattened upstream tool identities back onto source
// identities using only the binding table. Unknown aliases pass through
// untouched (plain or model-hallucinated names); call IDs stay opaque and
// arguments stay exact. Emulated Responses customs decode their single
// {"content":string} wrapper into CustomCallItems; malformed wrappers or
// mismatched native customs fail closed. A nil/empty plan returns the
// response unchanged.
func (p *ToolPlan) RestoreResponse(resp Response, lim Limits) (Response, *RouterError) {
	if p.Empty() {
		return resp, nil
	}
	if p.HasEmulatedCustom() {
		for _, it := range resp.Items {
			if _, ok := it.(CustomCallItem); ok {
				return Response{}, &RouterError{Status: 502, Code: CodeUpstreamBad,
					Msg: "Upstream response feature \"response.output.custom_tool_call\" has no representation"}
			}
		}
	}
	out := resp
	items := make([]ResponseItem, 0, len(resp.Items))
	for _, it := range resp.Items {
		call, ok := it.(ToolCallItem)
		if !ok {
			items = append(items, it)
			continue
		}
		if src, bound := p.emuCustom[call.Name]; bound {
			input, rerr := decodeCustomArgs(call.Arguments, lim)
			if rerr != nil {
				return Response{}, rerr
			}
			restored, rerr := NewCustomCallItem(call.ID, call.CallID, src.Namespace, src.Name, input)
			if rerr != nil {
				return Response{}, rerr
			}
			items = append(items, restored)
			continue
		}
		src, bound := p.byAlias[call.Name]
		if !bound {
			items = append(items, call)
			continue
		}
		if src.Namespace == "" {
			items = append(items, call)
			continue
		}
		restored, rerr := NewToolCallItemWithNamespace(call.ID, call.CallID, src.Namespace, src.Name, call.Arguments)
		if rerr != nil {
			return Response{}, rerr
		}
		items = append(items, restored)
	}
	out.Items = items
	return out, nil
}

// TranslateChatJSONToResponsesWithPlan decodes one Chat response,
// restores namespace identities from the request plan, and renders the
// Responses envelope. A nil/empty plan behaves exactly like
// TranslateChatJSONToResponses.
func TranslateChatJSONToResponsesWithPlan(raw []byte, publicModel string, lim Limits, plan *ToolPlan) ([]byte, *RouterError) {
	return TranslateChatJSONToResponsesWithPlanAndPolicy(raw, publicModel, lim, plan, false)
}

// TranslateChatJSONToResponsesWithPlanAndPolicy decodes one Chat
// response with narrow lossy compatibility policy (allowPrivateDrop drops
// substantive reasoning_content and the known gateway usage extensions)
// and renders the Responses envelope. Reasoning accounting still travels
// natively.
func TranslateChatJSONToResponsesWithPlanAndPolicy(raw []byte, publicModel string, lim Limits, plan *ToolPlan, allowPrivateDrop bool) ([]byte, *RouterError) {
	resp, rerr := DecodeChatResponseWithPolicy(raw, lim, allowPrivateDrop)
	if rerr != nil {
		return nil, rerr
	}
	restored, rerr := plan.RestoreResponse(resp, lim)
	if rerr != nil {
		return nil, rerr
	}
	return EncodeResponsesResponse(restored, publicModel)
}

// TranslateAnthropicJSONToResponsesWithPlan decodes one Anthropic response,
// restores namespace identities from the request plan, and renders the
// Responses envelope. Strict: gateway billing metadata fails closed.
func TranslateAnthropicJSONToResponsesWithPlan(raw []byte, publicModel string, lim Limits, plan *ToolPlan) ([]byte, *RouterError) {
	return TranslateAnthropicJSONToResponsesWithPlanAndPolicy(raw, publicModel, lim, plan, false)
}

// TranslateAnthropicJSONToResponsesWithPlanAndPolicy decodes one Anthropic
// response with narrow gateway-billing policy, restores namespace
// identities, and renders the Responses envelope. allowDrop drops
// validated usage.cost/usage.opencode, false keeps the strict 502.
func TranslateAnthropicJSONToResponsesWithPlanAndPolicy(raw []byte, publicModel string, lim Limits, plan *ToolPlan, allowDrop bool) ([]byte, *RouterError) {
	resp, rerr := DecodeAnthropicResponseWithPolicy(raw, lim, allowDrop)
	if rerr != nil {
		return nil, rerr
	}
	restored, rerr := plan.RestoreResponse(resp, lim)
	if rerr != nil {
		return nil, rerr
	}
	return EncodeResponsesResponse(restored, publicModel)
}

// TranslateGeminiJSONToResponsesWithPlan decodes one Gemini response,
// restores namespace identities from the request plan, and renders the
// Responses envelope. A nil/empty plan behaves exactly like
// TranslateGeminiJSONToResponses. Strict: gateway/provider response
// extensions fail closed.
func TranslateGeminiJSONToResponsesWithPlan(raw []byte, publicModel string, lim Limits, plan *ToolPlan) ([]byte, *RouterError) {
	return TranslateGeminiJSONToResponsesWithPlanAndPolicy(raw, publicModel, lim, plan, false)
}

// TranslateGeminiJSONToResponsesWithPlanAndPolicy decodes one Gemini
// response with narrow lossy-extension policy, restores namespace
// identities from the request plan, and renders the Responses
// envelope. allowDrop drops validated response extensions
// (thoughtSignature, TEXT-modality usage details, usage.cost/
// usage.opencode); false keeps the strict 502.
func TranslateGeminiJSONToResponsesWithPlanAndPolicy(raw []byte, publicModel string, lim Limits, plan *ToolPlan, allowDrop bool) ([]byte, *RouterError) {
	resp, rerr := DecodeGeminiResponseWithPolicy(raw, lim, allowDrop)
	if rerr != nil {
		return nil, rerr
	}
	restored, rerr := plan.RestoreResponse(resp, lim)
	if rerr != nil {
		return nil, rerr
	}
	return EncodeResponsesResponse(restored, publicModel)
}

// restoringDecoder wraps one StreamDecoder and rewrites tool BlockOpen
// identities from the binding table. It buffers nothing, reads nothing
// extra, and never touches text or argument bytes.
type restoringDecoder struct {
	inner StreamDecoder
	plan  *ToolPlan
}

// WrapDecoder returns a StreamDecoder that restores namespace identities
// and emulated Responses customs on tool blocks. A nil/empty plan returns
// the inner decoder unchanged. Close passes through without extra reads.
func (p *ToolPlan) WrapDecoder(inner StreamDecoder, lim Limits) StreamDecoder {
	if p.Empty() || inner == nil {
		return inner
	}
	if p.HasEmulatedCustom() {
		return &emulatedStreamDecoder{inner: inner, plan: p, lim: lim, bufs: map[ItemID]*emuStreamBuf{}}
	}
	return &restoringDecoder{inner: inner, plan: p}
}

// DecodeFrame decodes one frame then restores tool block identities.
func (d *restoringDecoder) DecodeFrame(frame Frame) ([]ResponseEvent, *RouterError) {
	events, rerr := d.inner.DecodeFrame(frame)
	if rerr != nil {
		return nil, rerr
	}
	return d.plan.restoreEvents(events)
}

// Close validates terminal state without extra reads.
func (d *restoringDecoder) Close() ([]ResponseEvent, *RouterError) {
	events, rerr := d.inner.Close()
	if rerr != nil {
		return nil, rerr
	}
	return d.plan.restoreEvents(events)
}

// restoreEvents rewrites tool BlockOpen names via the binding table.
func (p *ToolPlan) restoreEvents(events []ResponseEvent) ([]ResponseEvent, *RouterError) {
	if p.Empty() {
		return events, nil
	}
	out := make([]ResponseEvent, 0, len(events))
	for _, ev := range events {
		open, ok := ev.(BlockOpen)
		if !ok || open.Kind != BlockTool {
			out = append(out, ev)
			continue
		}
		src, bound := p.byAlias[open.Name]
		if !bound || src.Namespace == "" {
			out = append(out, ev)
			continue
		}
		restored, rerr := NewBlockOpenWithNamespace(open.Item, open.Kind, open.Call, src.Namespace, src.Name)
		if rerr != nil {
			return nil, rerr
		}
		restored.SourceIndex = open.SourceIndex
		restored.CallOrigin = open.CallOrigin
		out = append(out, restored)
	}
	return out, nil
}
