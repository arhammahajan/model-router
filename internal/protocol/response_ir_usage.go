// Canonical Response IR: usage accounting with presence and provenance.
//
// Source validation is independent of destination representability: a
// source decoder accepts what its wire contract reports, and
// CheckRepresentable decides per (source, destination) pair what an
// already-validated Response may become. A new destination never
// silently widens or refuses what sources accept.
//
// Proven mappings this file preserves (see the contract audit D2-D5 and
// the cross-product tests in response_ir_usage_test.go):
//   - Scalar reasoning accounting (a NUMBER of reasoning tokens) travels:
//     Chat reasoning_tokens -> Gemini thoughtsTokenCount, Responses
//     output_tokens_details.reasoning_tokens; Responses reasoning_tokens
//     -> Chat completion_tokens_details.reasoning_tokens, Gemini
//     thoughtsTokenCount. Any nonzero scalar reasoning -> Anthropic
//     refuses (no Anthropic usage field carries it).
//   - Scalar reasoning accounting is distinct from reasoning OUTPUT or
//     reasoning STATE (thinking text, thought signatures, opaque echo):
//     those fail at source decode and never populate these buckets.
//   - Chat text_tokens accounting has no Anthropic or Gemini bucket: the
//     Responses path treats it as harmless-subsumed (even nonzero), so
//     the Chat source decoder carries it in TextTokens and each target
//     encoder (via CheckRepresentable) decides carry vs refuse. Zero is
//     representable everywhere (TestChatZeroUsageDetailsAcrossTargets);
//     nonzero refuses in Anthropic/Gemini, matching the proven
//     per-target parsers.
//   - Anthropic cache_creation_input_tokens -> Gemini counts promptly
//     (prompt = input + creation + read, cached = read only). A blanket
//     "cache creation refuses in Gemini" rule would break that existing
//     mapping (anthropic_to_gemini_json.go a2gemUsageFromScalars), so
//     creation representability in Gemini varies by source: Anthropic
//     creation travels, Chat/Responses creation refuses (proven: neither
//     Chat c2gemUsage nor Responses r2gParseUsage has a creation bucket).
//     Cache creation is distinct from cache READ (cached_tokens), which
//     every target renders or drops per its own contract.
//   - Gemini thoughtsTokenCount / toolUsePromptTokenCount nonzero values
//     fail at Gemini source decode today; the Thoughts and ToolUsePrompt
//     buckets exist so provenance stays explicit, and representability
//     refuses them everywhere except their native destination until a
//     reviewed mapping lands.
//   - Zero-valued known buckets are representable everywhere (they add
//     no accounting); only nonzero unmapped buckets refuse.
package protocol

import (
	"fmt"
)

// UsageScope records where in the stream lifecycle accounting was
// reported. Initial counters are placeholders some dialects require at
// start; only terminal counters are authoritative.
type UsageScope string

const (
	// UsageInitial marks a pre-accounting placeholder (for example the
	// zero usage some destinations require in their start event). It
	// must never be reported downstream as a total.
	UsageInitial UsageScope = "initial"
	// UsageCumulative marks running counters on an intermediate chunk
	// (Gemini usageMetadata). Informative only; the terminal still must
	// arrive. Multiple cumulative updates are valid; they never satisfy
	// the terminal requirement.
	UsageCumulative UsageScope = "cumulative"
	// UsageTerminal marks authoritative final accounting. Exactly one
	// terminal usage is required per generation.
	UsageTerminal UsageScope = "terminal"
)

// UsageProvenance records which dialect reported accounting and in which
// scope, so representability checks know what was claimed versus what
// was derived. Unknown accounting members never reach the IR: the
// source decoder fails on them instead.
type UsageProvenance struct {
	Source Dialect
	Scope  UsageScope
}

// ResponseUsage carries generation accounting bucket by bucket. Each
// bucket is a pointer: nil means absent (the source did not report it),
// non-nil means reported (including an explicit zero, which is
// representable everywhere). Nothing in this IR fabricates zeros for
// missing terminal accounting: absent counters stay absent until
// authoritative terminal accounting arrives.
//
// Pointer presence replaces bool+int pairs deliberately: presence IS
// the nil check, so a hand-built literal cannot claim "present" with a
// contradictory value the way a HasX=true/negative-int combination
// could. ValidateAuthoritative still re-checks every non-nil bucket, so
// literals with negative or subset-violating counters fail exactly like
// malformed wire bytes.
type ResponseUsage struct {
	Prompt, Completion, Total *int

	CacheCreation, CacheRead *int

	// Reasoning holds scalar reasoning-token accounting where the
	// source reports it as a number (Chat completion_tokens_details
	// reasoning_tokens, Responses output_tokens_details
	// reasoning_tokens). Sources that report reasoning only as
	// opaque/unsupported state fail at decode; they never set this
	// bucket.
	Reasoning *int

	// Thoughts holds Gemini scalar thought accounting
	// (thoughtsTokenCount) when a reviewed mapping populates it.
	// Gemini source decode refuses nonzero thoughts today, so this
	// bucket only carries values a reviewed source mapping produced.
	Thoughts *int

	// ToolUsePrompt holds Gemini scalar tool-use prompt accounting
	// (toolUsePromptTokenCount) when a reviewed mapping populates it.
	// No destination has a reviewed representation today; nonzero
	// values refuse everywhere (closed default, not a silent drop).
	ToolUsePrompt *int

	// TextTokens holds Chat scalar text-token accounting (the sum of
	// prompt_tokens_details.text_tokens and
	// completion_tokens_details.text_tokens) when a reviewed source
	// mapping populates it. Only the Chat row sets this bucket: the
	// Responses path treats text as harmless-subsumed (carried in the
	// prompt/completion totals), while Anthropic and Gemini have no
	// text bucket and refuse nonzero values. Zero is representable
	// everywhere; absent (nil) and explicit zero behave identically in
	// CheckRepresentable.
	TextTokens *int

	Provenance UsageProvenance
}

func intPtr(n int) *int { return &n }

func nonNegative(n *int) bool { return n != nil && *n >= 0 }

// NewTerminalUsage builds authoritative terminal accounting. Every total
// must be present and exact (total == prompt+completion with overflow
// check). Anything else is an accounting failure, not a best effort.
func NewTerminalUsage(source Dialect, prompt, completion, total int) (ResponseUsage, *RouterError) {
	bad := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned invalid usage accounting"}
	if prompt < 0 || completion < 0 || total < 0 {
		return ResponseUsage{}, bad
	}
	if sum := prompt + completion; sum < prompt || sum < completion || sum != total {
		return ResponseUsage{}, bad
	}
	return ResponseUsage{
		Prompt: intPtr(prompt), Completion: intPtr(completion), Total: intPtr(total),
		Provenance: UsageProvenance{Source: source, Scope: UsageTerminal},
	}, nil
}

// WithCache attaches cache buckets to terminal usage. Each share must
// fit inside the prompt total (cache <= prompt, overflow-safe).
// Provenance decides the combined policy, matching the wire contracts:
//   - Anthropic and Responses report disjoint portions (input excludes
//     the cache buckets; Responses input includes them but r2a rejects
//     cache_write > input-cache_read): creation+read <= prompt,
//     overflow-safe, else the accounting is contradictory.
//   - Chat (prompt_tokens_details) and Gemini (cached-only) carry
//     overlapping/subset shares with no combined proof: only each
//     share <= prompt plus overflow-safe addition is enforced, never a
//     blanket combined rejection.
//
// Canonical Prompt is the inclusive total each wire contract reports
// (Chat prompt_tokens includes cached; Anthropic prompt is
// input+creation+read; Responses input_tokens verbatim; Gemini
// promptTokenCount verbatim).
func (u ResponseUsage) WithCache(creation, read *int) (ResponseUsage, *RouterError) {
	bad := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned invalid cache accounting"}
	if u.Prompt == nil || *u.Prompt < 0 {
		return ResponseUsage{}, bad
	}
	for _, bucket := range []*int{creation, read} {
		if bucket == nil {
			continue
		}
		if *bucket < 0 || *bucket > *u.Prompt {
			return ResponseUsage{}, bad
		}
	}
	if creation != nil && read != nil {
		if sum := *creation + *read; sum < *creation || sum < *read {
			return ResponseUsage{}, bad
		}
		// Disjoint-portion sources must fit together inside the total.
		// Proven by anthropic_to_chat.go prompt() (input+creation+read
		// with overflow checks) and responses_to_anthropic_json.go
		// r2aParseUsage (cacheWrite > input-cacheRead fails).
		if u.Provenance.Source == DialectAnthropic || u.Provenance.Source == DialectResponses {
			if *creation+*read > *u.Prompt {
				return ResponseUsage{}, bad
			}
		}
	}
	u.CacheCreation, u.CacheRead = creation, read
	return u, nil
}

// WithReasoning attaches scalar reasoning-token accounting. Reasoning is
// a subset of the completion total (proven by every source decoder that
// populates it); the subset is enforced here and re-checked by
// ValidateAuthoritative so literals cannot smuggle contradictions.
func (u ResponseUsage) WithReasoning(reasoning *int) (ResponseUsage, *RouterError) {
	bad := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned invalid usage accounting"}
	if reasoning == nil {
		u.Reasoning = nil
		return u, nil
	}
	if u.Completion == nil || *reasoning < 0 || *reasoning > *u.Completion {
		return ResponseUsage{}, bad
	}
	u.Reasoning = reasoning
	return u, nil
}

// WithThoughts attaches Gemini scalar thought accounting. Subset policy
// for thoughts lives in the target encoders that map reasoning into
// thoughts (e.g. Chat reasoning must fit its completion total); this
// constructor enforces non-negativity only and documents that choice.
func (u ResponseUsage) WithThoughts(thoughts *int) (ResponseUsage, *RouterError) {
	bad := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned invalid usage accounting"}
	if thoughts == nil {
		u.Thoughts = nil
		return u, nil
	}
	if *thoughts < 0 {
		return ResponseUsage{}, bad
	}
	u.Thoughts = thoughts
	return u, nil
}

// WithToolUsePrompt attaches Gemini scalar tool-use prompt accounting.
// Non-negativity only; no destination maps it today (see
// CheckRepresentable).
func (u ResponseUsage) WithToolUsePrompt(n *int) (ResponseUsage, *RouterError) {
	bad := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned invalid usage accounting"}
	if n == nil {
		u.ToolUsePrompt = nil
		return u, nil
	}
	if *n < 0 {
		return ResponseUsage{}, bad
	}
	u.ToolUsePrompt = n
	return u, nil
}

// ValidateAuthoritative enforces the terminal-accounting contract shared
// by stream assemblers and complete decoders: terminal scope, all three
// totals present, exact, and overflow-checked, plus re-validation of
// every optional bucket so a hand-built literal with a negative or
// subset-violating counter fails exactly like malformed wire bytes.
// Malformed counters in a supposedly authoritative struct are never
// silently trimmed.
func (u ResponseUsage) ValidateAuthoritative() *RouterError {
	bad := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned invalid usage accounting"}
	if u.Provenance.Scope != UsageTerminal {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream ended without authoritative usage"}
	}
	if u.Prompt == nil || u.Completion == nil || u.Total == nil {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream ended without authoritative usage"}
	}
	if *u.Prompt < 0 || *u.Completion < 0 || *u.Total < 0 {
		return bad
	}
	if sum := *u.Prompt + *u.Completion; sum < *u.Prompt || sum < *u.Completion || sum != *u.Total {
		return bad
	}
	for _, bucket := range []*int{u.CacheCreation, u.CacheRead} {
		if bucket == nil {
			continue
		}
		if *bucket < 0 || *bucket > *u.Prompt {
			return bad
		}
	}
	if u.Reasoning != nil && (*u.Reasoning < 0 || *u.Reasoning > *u.Completion) {
		return bad
	}
	if u.Thoughts != nil && *u.Thoughts < 0 {
		return bad
	}
	if u.ToolUsePrompt != nil && *u.ToolUsePrompt < 0 {
		return bad
	}
	// Provenance-aware combined cache policy, mirroring WithCache:
	// Anthropic/Responses shares are disjoint portions and must fit
	// together; Chat/Gemini shares are overlapping/subset with no
	// combined proof, so only individual bounds apply.
	if u.CacheCreation != nil && u.CacheRead != nil &&
		(u.Provenance.Source == DialectAnthropic || u.Provenance.Source == DialectResponses) {
		if sum := *u.CacheCreation + *u.CacheRead; sum < *u.CacheCreation || sum < *u.CacheRead || sum > *u.Prompt {
			return bad
		}
	}
	// Text accounting is a reported counter, never a subset of a
	// specific total the IR re-derives: the existing Chat parsers only
	// require it non-negative (responseInt rejects negatives at the
	// wire). Zero and absent are both representable everywhere.
	if u.TextTokens != nil && *u.TextTokens < 0 {
		return bad
	}
	return nil
}

// CheckRepresentable enforces the destination contract for an already
// source-validated Response, before any downstream byte is committed.
// It reports the first unrepresentable feature as a 502 with a
// value-free field path. Source decoders must NOT call this: keeping
// source validation destination-independent is what prevents a new
// destination from silently widening or refusing what sources accept.
//
// Representability varies by (source, destination) where accounting
// semantics require it; it never codifies a false
// common-denominator restriction. In particular a blanket "nonzero
// reasoning refuses everywhere" rule is wrong: Chat reasoning travels
// to Gemini (thoughtsTokenCount) and Responses (reasoning_tokens), and
// Responses reasoning travels to Chat and Gemini. Likewise a blanket
// "cache creation refuses in Chat" rule is wrong: Anthropic creation
// counts into the Chat prompt today.
//
// Strict: nonzero reasoning refuses in Anthropic for every source.
// Lossy (CheckRepresentableWithPolicy with allowChatReasoningForAnthropic
// true) projects the Chat-source reasoning breakdown for Anthropic: the
// breakdown is dropped while aggregate output_tokens is preserved
// (output_tokens stays completion). Responses/Gemini accounting and strict
// Anthropic are unchanged; only source==Chat with dest==Anthropic is
// affected.
func CheckRepresentable(resp Response, dest Dialect) *RouterError {
	return CheckRepresentableWithPolicy(resp, dest, false)
}

// CheckRepresentableWithPolicy enforces the destination contract with
// narrow lossy policy: allowChatReasoningForAnthropic drops the
// Chat-source scalar reasoning breakdown for the Anthropic target,
// preserving aggregate output_tokens. All other source/destination
// combinations behave strictly.
func CheckRepresentableWithPolicy(resp Response, dest Dialect, allowChatReasoningForAnthropic bool) *RouterError {
	unrep := func(what string) *RouterError {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: fmt.Sprintf("Upstream response feature %q has no %s representation", what, string(dest))}
	}
	source := resp.Source
	// Namespaced tool identities have a Responses representation only.
	// Every other destination refuses them explicitly rather than
	// silently dropping the namespace (which would collide distinct
	// tools). Source decoders never set Namespace; only the request
	// plan restoration does.
	if dest != DialectResponses {
		for _, item := range resp.Items {
			if call, ok := item.(ToolCallItem); ok && call.Namespace != "" {
				return unrep("response.output.function_call.namespace")
			}
		}
	}
	if source == DialectResponses && dest == DialectAnthropic {
		for _, item := range resp.Items {
			if _, refusal := item.(RefusalItem); refusal {
				return unrep("response.output.refusal")
			}
		}
	}
	// Preserve the reviewed distinction: a matched Anthropic stop sequence
	// maps to Chat/Gemini stop, but has no approved Responses mapping yet.
	if source == DialectAnthropic && dest == DialectResponses && resp.Finish.Raw == "stop_sequence" {
		return unrep("response.stop_reason")
	}

	if rerr := CheckUsageRepresentableWithPolicy(resp.Usage, source, dest, allowChatReasoningForAnthropic); rerr != nil {
		return rerr
	}

	// Structural representability, source-neutral: every source decoder
	// produces the same typed Items/Finish, so the same destination
	// limits apply regardless of provenance. Source gates below are
	// explicit provenance policies, never per-pair parse copies.
	//
	// Omitted malformed tool calls render only as Gemini
	// MALFORMED_FUNCTION_CALL; Chat decoders prove Anthropic and
	// Responses refuse the same bytes, and no other source row mints
	// MalformedTools today.
	if resp.MalformedTools > 0 && dest != DialectGemini {
		return unrep("response.choices[0].message.tool_calls")
	}
	// Refusal has no Gemini form (c2gemResponseParts refuses nonempty
	// refusal, g2c/a2gem reject thought/refusal blocks); Anthropic
	// folds it into text and Responses renders a refusal output item.
	if dest == DialectGemini {
		for _, it := range resp.Items {
			if _, ok := it.(RefusalItem); ok {
				return unrep("response.choices[0].message.refusal")
			}
		}
	}
	// Custom calls have a Chat and Responses representation only (native
	// freeform contract). Every other destination refuses them explicitly.
	// Namespaces on custom calls follow the same Responses-only rule.
	hasCustom := false
	for _, item := range resp.Items {
		if _, ok := item.(CustomCallItem); ok {
			hasCustom = true
			break
		}
	}
	if hasCustom && dest != DialectChat && dest != DialectResponses {
		return unrep("response.output.custom_tool_call")
	}
	if dest != DialectResponses {
		for _, item := range resp.Items {
			if call, ok := item.(CustomCallItem); ok && call.Namespace != "" {
				return unrep("response.output.custom_tool_call.namespace")
			}
		}
	}
	// Tool names carry the 1..64 grammar in Anthropic and Responses
	// (every pair decoder applies validateToolName); the Gemini and
	// Chat paths check nonempty only and pass names through (proven:
	// Chat->Gemini passes "has space", Gemini->Chat preserves it).
	// Anthropic->Gemini also retains its reviewed name-grammar refusal;
	// introducing the shared gate must not widen that existing contract.
	if dest == DialectAnthropic || dest == DialectResponses || (source == DialectAnthropic && dest == DialectGemini) {
		for _, it := range resp.Items {
			if call, ok := it.(ToolCallItem); ok {
				if err := validateToolName(call.Name); err != nil {
					return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
				}
			}
			if call, ok := it.(CustomCallItem); ok {
				if err := validateToolName(call.Name); err != nil {
					return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
				}
			}
		}
	}
	// A tool_calls finish with no calls has no Responses form
	// (TranslateChatJSONToResponses and Anthropic a2r both fail
	// closed); the other destinations render their stop mapping
	// regardless. (A malformed-only generation is already refused
	// above.) Custom calls count as calls for this purpose.
	if dest == DialectResponses && resp.Finish.Kind == FinishToolCalls {
		hasCall := false
		for _, it := range resp.Items {
			if _, ok := it.(ToolCallItem); ok {
				hasCall = true
				break
			}
			if _, ok := it.(CustomCallItem); ok {
				hasCall = true
				break
			}
		}
		if !hasCall {
			return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned a tool finish with no tool calls"}
		}
	}
	// Chat wire order: content is one string plus a tool_calls array, so
	// interleaved text-after-tool has no representation. Anthropic,
	// Responses and Gemini preserve ordered blocks one-to-one (proven:
	// a2rDecodeContent keeps tool-first order, r2a trailing bucket
	// keeps order, Gemini parts stay ordered), while Chat decoders
	// concatenate (r2cDecodeOutput, g2cDecodeContent). Refusing
	// text-after-tool is a tightening vs the legacy silent reorder,
	// recorded as a bugfix: the wire cannot distinguish
	// [tool,text] from [text,tool]. Custom calls order like function calls.
	if dest == DialectChat {
		seenTool := false
		seenRefusal := false
		for _, it := range resp.Items {
			switch it.(type) {
			case ToolCallItem, CustomCallItem:
				seenTool = true
			case TextItem, RefusalItem:
				if _, ok := it.(RefusalItem); ok {
					seenRefusal = true
				}
				if seenTool {
					return unrep("response.items order")
				}
			}
		}
		// Responses proves refusal+tools has no Chat form (r2c refuses
		// "response.output refusal with function calls").
		if seenRefusal && seenTool {
			return unrep("response.output refusal with function calls")
		}
	}
	switch dest {
	case DialectChat, DialectAnthropic, DialectResponses, DialectGemini:
		return nil
	default:
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: fmt.Sprintf("Unknown destination dialect %q", string(dest))}
	}
}

// CheckUsageRepresentable applies source-aware target policy to validated
// usage facts, including initial snapshots. It does not invent content or
// finish state merely to reuse the complete-response policy.
//
// Strict: nonzero reasoning refuses in Anthropic for every source.
// Lossy (allowChatReasoningForAnthropic true) projects Chat-source
// reasoning for Anthropic: the breakdown is dropped, aggregate
// output_tokens (completion) is preserved. Responses/Gemini accounting
// and strict Anthropic are unchanged.
func CheckUsageRepresentable(usage ResponseUsage, source, dest Dialect) *RouterError {
	return CheckUsageRepresentableWithPolicy(usage, source, dest, false)
}

// CheckUsageRepresentableWithPolicy applies source-aware target policy
// with narrow lossy policy: allowChatReasoningForAnthropic drops the
// Chat-source scalar reasoning breakdown for the Anthropic target,
// preserving aggregate output_tokens. Only source==Chat with
// dest==Anthropic is affected; Responses/Gemini reasoning and strict
// Anthropic still refuse.
func CheckUsageRepresentableWithPolicy(usage ResponseUsage, source, dest Dialect, allowChatReasoningForAnthropic bool) *RouterError {
	unrep := func(what string) *RouterError {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: fmt.Sprintf("Upstream response feature %q has no %s representation", what, string(dest))}
	}
	nonzero := func(n *int) bool { return n != nil && *n != 0 }
	// Scalar reasoning accounting: refuses only where no reviewed
	// mapping exists. Anthropic has no reasoning usage field. Strict
	// refuses every source there (Chat->Anthropic, Responses->Anthropic
	// proven). Lossy projects Chat reasoning for Anthropic by dropping
	// the breakdown while preserving aggregate output_tokens; Responses
	// reasoning still refuses in Anthropic even in lossy, and Gemini
	// thoughts still refuse outside Gemini. Chat and Responses reasoning
	// travel to Chat, Gemini (thoughts), and Responses natively.
	if nonzero(usage.Reasoning) && dest == DialectAnthropic {
		if !(allowChatReasoningForAnthropic && source == DialectChat) {
			return unrep("response.usage.reasoning_tokens")
		}
		// Lossy Chat->Anthropic: fall through, breakdown dropped,
		// aggregate preserved by the target encoders (output_tokens stays
		// completion).
	}
	// Gemini-provenance thoughts: only Gemini renders thoughtsTokenCount
	// natively today. (Gemini source decode refuses nonzero thoughts, so
	// this arm is a backstop preserving that refusal if a future
	// mapping ever populates the bucket.)
	if nonzero(usage.Thoughts) && dest != DialectGemini {
		return unrep("response.usage.thoughtsTokenCount")
	}
	// Tool-use prompt accounting has no reviewed representation on any
	// destination today: closed refusal, never a silent zero.
	if nonzero(usage.ToolUsePrompt) {
		return unrep("response.usage.tool_use_prompt_tokens")
	}

	// Cache creation: Anthropic creation counts into the Chat prompt
	// (existing Anthropic->Chat summation), Responses cache_write maps
	// to Anthropic cache_creation_input_tokens, Chat cache_write
	// travels to Responses details, and Anthropic creation travels to
	// Gemini (prompt = input+creation+read per a2gemUsageFromScalars,
	// cached = read only). Every other nonzero combination refuses:
	// Chat cache_write has no Anthropic or Gemini representation
	// (proven), Responses cache_write has no Chat or Gemini
	// representation (proven: Responses r2gParseUsage refuses nonzero
	// input detail shares), and nothing maps Chat creation into
	// Gemini (proven: c2gemUsage has no creation bucket).
	if nonzero(usage.CacheCreation) {
		switch dest {
		case DialectChat:
			if source != DialectAnthropic {
				return unrep("response.usage.cache_creation_input_tokens")
			}
		case DialectAnthropic:
			if source != DialectResponses && source != DialectAnthropic {
				return unrep("response.usage.cache_creation_input_tokens")
			}
		case DialectResponses:
			// All sources detail creation in Responses: representable.
		case DialectGemini:
			// Only Anthropic creation sums into the Gemini prompt;
			// Chat/Responses creation has no Gemini bucket.
			if source != DialectAnthropic {
				return unrep("response.usage.cache_creation_input_tokens")
			}
		default:
			return &RouterError{Status: 502, Code: CodeUpstreamBad,
				Msg: fmt.Sprintf("Unknown destination dialect %q", string(dest))}
		}
	}

	// Chat text accounting: Responses treats text_tokens as
	// harmless-subsumed (no bucket, totals already carry it), while
	// Anthropic and Gemini have no text bucket at all. Nonzero text
	// refuses there (proven by parseChatUsage/c2gemUsage); zero is
	// representable everywhere.
	if nonzero(usage.TextTokens) && (dest == DialectAnthropic || dest == DialectGemini) {
		return unrep("response.usage.text_tokens")
	}

	return nil
}
