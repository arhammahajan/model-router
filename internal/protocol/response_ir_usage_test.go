// Usage accounting and destination representability tests.
//
// Cross-product matrix: the same source feature MUST produce the
// per-target accept/refuse the existing pairwise translators prove.
// A blanket "nonzero reasoning refuses everywhere" or "cache creation
// refuses in Chat" rule fails these tests (it would change
// Chat->Gemini thoughts, Responses->Chat/Gemini reasoning, and
// Anthropic->Chat prompt summation). Citations name the pair code that
// proves each cell.
package protocol

import (
	"testing"
)

func usageWith(t *testing.T, source Dialect, prompt, completion int, apply func(ResponseUsage) (ResponseUsage, *RouterError)) ResponseUsage {
	t.Helper()
	u, rerr := NewTerminalUsage(source, prompt, completion, prompt+completion)
	if rerr != nil {
		t.Fatalf("NewTerminalUsage: %v", rerr)
	}
	u, rerr = apply(u)
	if rerr != nil {
		t.Fatalf("apply: %v", rerr)
	}
	return u
}

func respWithUsage(t *testing.T, source Dialect, u ResponseUsage) Response {
	t.Helper()
	id, _ := NewResponseID("r1")
	resp, rerr := NewResponse(id, "m", source, []ResponseItem{TextItem{ID: "t", Text: "x"}}, u, mustFinish(t, FinishStop))
	if rerr != nil {
		t.Fatalf("NewResponse: %v", rerr)
	}
	return resp
}

// --- Totals: presence, exactness, no fabricated zeros --------------------------

func TestResponseIRUsageAccounting(t *testing.T) {
	if _, rerr := NewTerminalUsage(DialectChat, 5, 2, 8); rerr == nil {
		t.Error("total != prompt+completion accepted")
	}
	if _, rerr := NewTerminalUsage(DialectChat, -1, 2, 1); rerr == nil {
		t.Error("negative prompt accepted")
	}
	if _, rerr := NewTerminalUsage(DialectChat, 5, 2, 7); rerr != nil {
		t.Fatalf("valid usage: %v", rerr)
	}
	// Fabricated zeros: a zero-value struct is not authoritative.
	if rerr := (ResponseUsage{}).ValidateAuthoritative(); rerr == nil {
		t.Error("zero-value usage accepted as authoritative")
	}
	// Non-terminal scope never satisfies the terminal requirement
	// (audit D1: placeholder/cumulative vs authoritative-once).
	nonTerm := mustTerminalUsage(t, 5, 2)
	nonTerm.Provenance.Scope = UsageCumulative
	if rerr := nonTerm.ValidateAuthoritative(); rerr == nil {
		t.Error("cumulative usage accepted as authoritative")
	}
	// Literals with negative or subset-violating optional counters fail
	// exactly like malformed wire bytes (no bool+int smuggling).
	bad := mustTerminalUsage(t, 5, 2)
	neg := -1
	bad.CacheRead = &neg
	if rerr := bad.ValidateAuthoritative(); rerr == nil {
		t.Error("negative cache bucket in literal accepted")
	}
	bad2 := mustTerminalUsage(t, 5, 2)
	big := 99
	bad2.Reasoning = &big // 99 > completion 2
	if rerr := bad2.ValidateAuthoritative(); rerr == nil {
		t.Error("reasoning > completion in literal accepted")
	}
	bad3 := mustTerminalUsage(t, 5, 2)
	bad3.Prompt = nil
	if rerr := bad3.ValidateAuthoritative(); rerr == nil {
		t.Error("missing prompt total accepted as authoritative")
	}
}

// --- Cache: each share <= prompt, combined may exceed ---------------------------

func TestResponseIRCacheSubsets(t *testing.T) {
	// chat_to_gemini_response.go c2gemUsage: cached > prompt fails.
	u := mustTerminalUsage(t, 5, 2)
	if _, rerr := u.WithCache(intPtr(6), nil); rerr == nil {
		t.Error("cache_creation > prompt accepted")
	}
	if _, rerr := u.WithCache(nil, intPtr(6)); rerr == nil {
		t.Error("cache_read larger than prompt accepted")
	}
	if _, rerr := u.WithCache(intPtr(-1), nil); rerr == nil {
		t.Error("negative cache bucket accepted")
	}
	// Overlapping subsets: each fits, combined exceeds. Allowed.
	u2, rerr := u.WithCache(intPtr(4), intPtr(4))
	if rerr != nil {
		t.Fatalf("overlapping cache subsets: %v", rerr)
	}
	if *u2.CacheCreation != 4 || *u2.CacheRead != 4 {
		t.Fatalf("cache buckets not attached: %+v", u2)
	}
	// Combined overflow still fails (overflow-safe, not combined<=prompt).
	huge := int(^uint(0) >> 1)
	u3 := mustTerminalUsage(t, huge, 0)
	_ = u3
	u4 := mustTerminalUsage(t, 5, 2)
	if _, rerr := u4.WithCache(intPtr(5), intPtr(huge)); rerr == nil {
		t.Error("overflowing cache combination accepted")
	}
}

// --- Scalar reasoning cross-product ---------------------------------------------

func TestResponseIRReasoningCrossProduct(t *testing.T) {
	three := 3
	// Chat reasoning_tokens=3 is carried to Gemini as thoughtsTokenCount
	// (chat_to_gemini_response.go c2gemUsage) and to Responses as
	// reasoning_tokens (parseChatUsageForResponses), but has no
	// Anthropic representation (chat_to_anthropic_response.go refuses
	// any nonzero completion detail).
	chatReasoned := respWithUsage(t, DialectChat, usageWith(t, DialectChat, 5, 7, func(u ResponseUsage) (ResponseUsage, *RouterError) {
		return u.WithReasoning(&three)
	}))
	if rerr := CheckRepresentable(chatReasoned, DialectGemini); rerr != nil {
		t.Errorf("Chat reasoning -> Gemini refused: %v", rerr)
	}
	if rerr := CheckRepresentable(chatReasoned, DialectResponses); rerr != nil {
		t.Errorf("Chat reasoning -> Responses refused: %v", rerr)
	}
	if rerr := CheckRepresentable(chatReasoned, DialectAnthropic); rerr == nil {
		t.Error("Chat reasoning -> Anthropic accepted; must refuse (no reasoning usage field)")
	}
	if rerr := CheckRepresentable(chatReasoned, DialectChat); rerr != nil {
		t.Errorf("Chat reasoning -> Chat refused: %v", rerr)
	}

	// Responses reasoning_tokens is carried to Chat
	// (responses_to_chat_response.go r2c: reasoning subset of
	// completion) and to Gemini as thoughtsTokenCount
	// (responses_to_gemini_json.go), but refused in Anthropic
	// (responses_to_anthropic usage: folding into output_tokens would
	// misreport the text total).
	respReasoned := respWithUsage(t, DialectResponses, usageWith(t, DialectResponses, 5, 7, func(u ResponseUsage) (ResponseUsage, *RouterError) {
		return u.WithReasoning(&three)
	}))
	if rerr := CheckRepresentable(respReasoned, DialectChat); rerr != nil {
		t.Errorf("Responses reasoning -> Chat refused: %v", rerr)
	}
	if rerr := CheckRepresentable(respReasoned, DialectGemini); rerr != nil {
		t.Errorf("Responses reasoning -> Gemini refused: %v", rerr)
	}
	if rerr := CheckRepresentable(respReasoned, DialectAnthropic); rerr == nil {
		t.Error("Responses reasoning -> Anthropic accepted; must refuse")
	}

	// Plain responses are representable everywhere; zero reasoning is.
	plain := respWithUsage(t, DialectGemini, mustTerminalUsage(t, 5, 2))
	for _, d := range []Dialect{DialectChat, DialectResponses, DialectAnthropic, DialectGemini} {
		if rerr := CheckRepresentable(plain, d); rerr != nil {
			t.Errorf("plain response unrepresentable in %s: %v", d, rerr)
		}
	}
	zero := 0
	zeroReasoned := respWithUsage(t, DialectResponses, usageWith(t, DialectResponses, 5, 2, func(u ResponseUsage) (ResponseUsage, *RouterError) {
		return u.WithReasoning(&zero)
	}))
	for _, d := range []Dialect{DialectChat, DialectResponses, DialectAnthropic, DialectGemini} {
		if rerr := CheckRepresentable(zeroReasoned, d); rerr != nil {
			t.Errorf("zero reasoning unrepresentable in %s: %v", d, rerr)
		}
	}

	// Gemini-provenance thoughts refuse everywhere except Gemini
	// (Gemini source decode refuses nonzero thoughts today; this arm
	// preserves that refusal as a backstop).
	thoughts := 2
	gemThought := respWithUsage(t, DialectGemini, usageWith(t, DialectGemini, 5, 7, func(u ResponseUsage) (ResponseUsage, *RouterError) {
		return u.WithThoughts(&thoughts)
	}))
	for _, d := range []Dialect{DialectChat, DialectResponses, DialectAnthropic} {
		if rerr := CheckRepresentable(gemThought, d); rerr == nil {
			t.Errorf("Gemini thoughts representable in %s; must refuse", d)
		}
	}
	if rerr := CheckRepresentable(gemThought, DialectGemini); rerr != nil {
		t.Errorf("Gemini thoughts -> Gemini refused: %v", rerr)
	}

	// Tool-use prompt accounting has no reviewed representation
	// anywhere today: closed refusal, never a silent zero.
	toolUse := 1
	toolResp := respWithUsage(t, DialectGemini, usageWith(t, DialectGemini, 5, 2, func(u ResponseUsage) (ResponseUsage, *RouterError) {
		return u.WithToolUsePrompt(&toolUse)
	}))
	for _, d := range []Dialect{DialectChat, DialectResponses, DialectAnthropic, DialectGemini} {
		if rerr := CheckRepresentable(toolResp, d); rerr == nil {
			t.Errorf("tool-use prompt accounting representable in %s; must refuse", d)
		}
	}

	if rerr := CheckRepresentable(plain, Dialect("quantum")); rerr == nil {
		t.Error("unknown destination accepted")
	}
}

// --- Cache-creation cross-product: source matters --------------------------------

func TestResponseIRCacheCreationCrossProduct(t *testing.T) {
	two := 2
	// Anthropic cache_creation counts into the Chat prompt
	// (anthropic_to_chat.go prompt() = input+creation+read): Chat
	// accepts it. (Prompt here already includes the creation share,
	// as the Anthropic source decoder reports it.)
	anthCached := respWithUsage(t, DialectAnthropic, usageWith(t, DialectAnthropic, 10, 2, func(u ResponseUsage) (ResponseUsage, *RouterError) {
		return u.WithCache(intPtr(2), nil)
	}))
	if rerr := CheckRepresentable(anthCached, DialectChat); rerr != nil {
		t.Errorf("Anthropic cache creation -> Chat refused: %v", rerr)
	}

	// Responses cache_write travels to Anthropic cache_creation (r2a
	// maps both buckets) but has no Chat representation
	// (responses_to_chat_response.go carries cached_tokens only).
	respWrite := respWithUsage(t, DialectResponses, usageWith(t, DialectResponses, 10, 2, func(u ResponseUsage) (ResponseUsage, *RouterError) {
		return u.WithCache(&two, nil)
	}))
	if rerr := CheckRepresentable(respWrite, DialectAnthropic); rerr != nil {
		t.Errorf("Responses cache_write -> Anthropic refused: %v", rerr)
	}
	if rerr := CheckRepresentable(respWrite, DialectChat); rerr == nil {
		t.Error("Responses cache_write -> Chat accepted; must refuse (only cached_tokens carries)")
	}

	// Chat cache_write travels to Responses details but has no
	// Anthropic or Gemini representation (both proven refusals).
	chatWrite := respWithUsage(t, DialectChat, usageWith(t, DialectChat, 10, 2, func(u ResponseUsage) (ResponseUsage, *RouterError) {
		return u.WithCache(&two, nil)
	}))
	if rerr := CheckRepresentable(chatWrite, DialectResponses); rerr != nil {
		t.Errorf("Chat cache_write -> Responses refused: %v", rerr)
	}
	if rerr := CheckRepresentable(chatWrite, DialectAnthropic); rerr == nil {
		t.Error("Chat cache_write -> Anthropic accepted; must refuse")
	}
	if rerr := CheckRepresentable(chatWrite, DialectGemini); rerr == nil {
		t.Error("Chat cache_write -> Gemini accepted; must refuse (no Gemini counterpart)")
	}

	// Anthropic cache_creation travels to Gemini: the Gemini prompt is
	// input+creation+read and only the read share is detailed
	// (anthropic_to_gemini_json.go a2gemUsageFromScalars). Refusing it
	// would break that existing summation.
	anthCreation := respWithUsage(t, DialectAnthropic, usageWith(t, DialectAnthropic, 15, 2, func(u ResponseUsage) (ResponseUsage, *RouterError) {
		return u.WithCache(&two, nil)
	}))
	if rerr := CheckRepresentable(anthCreation, DialectGemini); rerr != nil {
		t.Errorf("Anthropic cache_creation -> Gemini refused: %v", rerr)
	}
	// Chat and Responses creation still have no Gemini bucket
	// (c2gemUsage and r2gParseUsage both refuse nonzero creation).
	if rerr := CheckRepresentable(chatWrite, DialectGemini); rerr == nil {
		t.Error("Chat cache_write -> Gemini accepted; must refuse")
	}
	respWriteGem := respWithUsage(t, DialectResponses, usageWith(t, DialectResponses, 10, 2, func(u ResponseUsage) (ResponseUsage, *RouterError) {
		return u.WithCache(&two, nil)
	}))
	if rerr := CheckRepresentable(respWriteGem, DialectGemini); rerr == nil {
		t.Error("Responses cache_write -> Gemini accepted; must refuse")
	}
}

// --- Chat text accounting: harmless in Responses, refused elsewhere ----------

func TestResponseIRTextTokensCrossProduct(t *testing.T) {
	five := 5
	texted := respWithUsage(t, DialectChat, ResponseUsage{
		Prompt: intPtr(10), Completion: intPtr(2), Total: intPtr(12),
		TextTokens: &five,
		Provenance: UsageProvenance{Source: DialectChat, Scope: UsageTerminal},
	})
	// NewResponse must accept the carried text bucket (terminal with
	// exact totals); only destinations decide carry vs refuse.
	if rerr := texted.Usage.ValidateAuthoritative(); rerr != nil {
		t.Fatalf("text bucket rejected as authoritative: %v", rerr)
	}
	if rerr := CheckRepresentable(texted, DialectResponses); rerr != nil {
		t.Errorf("Chat text_tokens -> Responses refused: %v", rerr)
	}
	if rerr := CheckRepresentable(texted, DialectChat); rerr != nil {
		t.Errorf("Chat text_tokens -> Chat refused: %v", rerr)
	}
	if rerr := CheckRepresentable(texted, DialectAnthropic); rerr == nil {
		t.Error("Chat text_tokens -> Anthropic accepted; must refuse (no text bucket)")
	}
	if rerr := CheckRepresentable(texted, DialectGemini); rerr == nil {
		t.Error("Chat text_tokens -> Gemini accepted; must refuse (no text bucket)")
	}
	// Zero text is representable everywhere.
	zero := 0
	zeroText := respWithUsage(t, DialectChat, ResponseUsage{
		Prompt: intPtr(10), Completion: intPtr(2), Total: intPtr(12),
		TextTokens: &zero,
		Provenance: UsageProvenance{Source: DialectChat, Scope: UsageTerminal},
	})
	for _, d := range []Dialect{DialectChat, DialectResponses, DialectAnthropic, DialectGemini} {
		if rerr := CheckRepresentable(zeroText, d); rerr != nil {
			t.Errorf("zero text unrepresentable in %s: %v", d, rerr)
		}
	}
	// Negative text is malformed, never authoritative.
	neg := -1
	negText := mustTerminalUsage(t, 10, 2)
	negText.TextTokens = &neg
	if rerr := negText.ValidateAuthoritative(); rerr == nil {
		t.Error("negative text accepted as authoritative")
	}
}
