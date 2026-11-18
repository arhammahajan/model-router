package protocol

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/arhammahajan/model-router/internal/capability"
)

// Bounded Anthropic thinking enabled unit: Budget intent with origin
// thinking for the Chat and Gemini destinations. Ordinary Chat/Responses
// never invent a budget-to-level table (diagnosed drop); Responses keeps
// the hard same-protocol refusal; Gemini admits the well-formed intent at
// decode and gates the exact generationConfig.thinkingConfig.thinkingBudget
// wire on the pinned canonical native fact (verified primary
// https://ai.google.dev/api/generate-content and
// https://ai.google.dev/gemini-api/docs/generate-content/thinking),
// otherwise a diagnosed drop. Adaptive/between_tools unchanged.

func enabledCaps(t *testing.T, apply func(*capability.Contract)) capability.ReasoningCapabilities {
	t.Helper()
	c := capability.Empty("openai-chat")
	if apply != nil {
		apply(&c)
	}
	c.Normalize()
	return capability.ReasoningFor(c)
}

func enabledStrPtr(s string) *string { v := s; return &v }

func TestAnthropicEnabledConstructor(t *testing.T) {
	got, ok := NewAnthropicThinkingEnabled(1024)
	if !ok {
		t.Fatalf("valid budget must construct")
	}
	if got.Kind() != KindBudget {
		t.Fatalf("kind must be Budget, got %v", got.Kind())
	}
	if !got.IsBudget() || !got.IsPresent() || got.IsNone() {
		t.Fatalf("budget must be present non-none: %+v", got)
	}
	b, ok := got.Budget()
	if !ok || b != 1024 {
		t.Fatalf("budget must survive exactly: %v %v", b, ok)
	}
	if got.Origin() != "thinking" || AnthropicThinkingOrigin.Path() != "thinking" {
		t.Fatalf("origin must be thinking: %q", got.Origin())
	}
	if lvl, ok := got.Level(); ok {
		t.Fatalf("budget must not report level: %q", lvl)
	}
	for _, bad := range []int64{0, -1, -100} {
		if _, ok := NewAnthropicThinkingEnabled(bad); ok {
			t.Fatalf("non-positive must not construct: %d", bad)
		}
	}
}

func TestAnthropicEnabledDecodeChatExact(t *testing.T) {
	raw := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":1024}}`
	g, losses, rerr := DecodeAnthropicRequest([]byte(raw), DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	if len(losses) != 0 {
		t.Fatalf("no-display must be lossless at decode: %v", losses)
	}
	if !g.Reasoning.IsBudget() || g.Reasoning.Origin() != "thinking" {
		t.Fatalf("must be Budget thinking: %+v", g.Reasoning)
	}
	if b, _ := g.Reasoning.Budget(); b != 1024 {
		t.Fatalf("budget must be exact: %d", b)
	}
}

func TestAnthropicEnabledDecodeChatDisplayLoss(t *testing.T) {
	for _, d := range []string{"summarized", "omitted"} {
		raw := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":512,"display":"` + d + `"}}`
		g, losses, rerr := DecodeAnthropicRequest([]byte(raw), DefaultLimits())
		if rerr != nil {
			t.Fatalf("display %s decode: %v", d, rerr)
		}
		if !g.Reasoning.IsBudget() {
			t.Fatalf("display must not block intent: %+v", g.Reasoning)
		}
		found := false
		for _, l := range losses {
			if l.Field == "thinking.display" {
				found = true
				if l.Action != "dropped" {
					t.Fatalf("display loss must be dropped: %+v", l)
				}
				if strings.Contains(l.Reason, "1024") || strings.Contains(l.Reason, d) {
					t.Fatalf("loss must be value-free: %+v", l)
				}
			}
		}
		if !found {
			t.Fatalf("admitted display must be independent loss: %v", losses)
		}
	}
	// Null display is absent-tolerant.
	raw := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":512,"display":null}}`
	g, losses, rerr := DecodeAnthropicRequest([]byte(raw), DefaultLimits())
	if rerr != nil {
		t.Fatalf("null display decode: %v", rerr)
	}
	if len(losses) != 0 || !g.Reasoning.IsBudget() {
		t.Fatalf("null display absent: %v %+v", losses, g.Reasoning)
	}
}

func TestAnthropicEnabledMalformedBeforeLoss(t *testing.T) {
	bad := []string{
		// Missing/null budget.
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled"}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":null}}`,
		// Non-positive.
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":0}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":-5}}`,
		// Wrong types / fractions / strings / bools / overflow.
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":"1024"}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":1024.0}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":10.5}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":true}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":9223372036854775808}}`,
		// Display malformed.
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":8,"display":"verbose"}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":8,"display":7}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":8,"display":""}}`,
		// Unknown keys (summary is not admitted in this closed shape).
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":8,"summary":"x"}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":8,"mystery":1}}`,
		// Type shape.
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":7,"budget_tokens":8}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"budget_tokens":8}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":null,"budget_tokens":8}}`,
	}
	for _, raw := range bad {
		if _, _, rerr := DecodeAnthropicRequest([]byte(raw), DefaultLimits()); rerr == nil || rerr.Status != 400 {
			t.Fatalf("malformed must 400 Chat: %s got %v", raw, rerr)
		}
		// Responses/Gemini validate shape before the hard refusal too.
		for _, decode := range []func([]byte, Limits) (*Generation, []Loss, *RouterError){
			DecodeAnthropicForResponses, DecodeAnthropicForGemini,
		} {
			if _, _, rerr := decode([]byte(raw), DefaultLimits()); rerr == nil || rerr.Status != 400 {
				t.Fatalf("malformed must 400 all dests: %s got %v", raw, rerr)
			}
		}
	}
}

func TestAnthropicEnabledResponsesGeminiRefusal(t *testing.T) {
	// Well-formed enabled budget keeps the existing hard refusal for
	// Responses (no budget control). Gemini admits the well-formed Budget
	// intent at decode; the shared planner gates the exact wire on the
	// pinned canonical native fact. Adaptive/between_tools stay refused
	// everywhere.
	good := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":1024}}`
	if _, _, rerr := DecodeAnthropicForResponses([]byte(good), DefaultLimits()); rerr == nil || rerr.Status != 400 || rerr.Code != CodeInvalidGeneration {
		t.Fatalf("enabled must refuse Responses: got %v", rerr)
	}
	g, losses, rerr := DecodeAnthropicForGemini([]byte(good), DefaultLimits())
	if rerr != nil {
		t.Fatalf("gemini must admit well-formed enabled: %v", rerr)
	}
	if !g.Reasoning.IsBudget() || g.Reasoning.Origin() != "thinking" {
		t.Fatalf("gemini must carry Budget thinking: %+v", g.Reasoning)
	}
	if b, _ := g.Reasoning.Budget(); b != 1024 {
		t.Fatalf("gemini budget must be exact: %d", b)
	}
	if len(losses) != 0 {
		t.Fatalf("no-display gemini must be lossless at decode: %v", losses)
	}
	for _, raw := range []string{
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"adaptive"}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"between_tools"}}`,
	} {
		for _, decode := range []func([]byte, Limits) (*Generation, []Loss, *RouterError){
			DecodeAnthropicRequest, DecodeAnthropicForResponses, DecodeAnthropicForGemini,
		} {
			if _, _, rerr := decode([]byte(raw), DefaultLimits()); rerr == nil || rerr.Status != 400 {
				t.Fatalf("adaptive/between_tools must refuse: %s got %v", raw, rerr)
			}
		}
	}
}

func TestAnthropicEnabledPlanGates(t *testing.T) {
	raw := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":2048}}`
	raw += `}`
	g, _, _ := DecodeAnthropicRequest([]byte(raw), DefaultLimits())

	// Unknown caps + llama Chat: exact.
	if p, l := PlanEffort(g.Reasoning, enabledCaps(t, nil), LocalRoute(capability.RuntimeLlamaCpp)); len(l) != 0 || p.Kind() != EffortBudget {
		t.Fatalf("llama unknown must be exact: %v %+v", l, p)
	} else if b, _ := p.Budget(); b != 2048 {
		t.Fatalf("budget must carry exactly: %d", b)
	}

	// Unknown caps + ordinary Chat: diagnosed drop, no table.
	if p, l := PlanEffort(g.Reasoning, enabledCaps(t, nil), OrdinaryRoute()); len(l) != 1 || p.IsPresent() {
		t.Fatalf("ordinary must drop: %v %+v", l, p)
	} else if l[0].Field != "thinking" || l[0].Action != "dropped" {
		t.Fatalf("ordinary loss origin thinking dropped: %v", l)
	}

	// Budget ignores toggle/effort gates (only budget facts gate it).
	noToggle := enabledCaps(t, func(c *capability.Contract) {
		_ = c.Put("reasoning.toggle.supported", capability.BoolValue(false), enabledStrPtr("t"), capability.KindOverride)
		_ = c.Put("reasoning.effort.supported", capability.BoolValue(false), enabledStrPtr("t"), capability.KindOverride)
	})
	if p, l := PlanEffort(g.Reasoning, noToggle, LocalRoute(capability.RuntimeLlamaCpp)); len(l) != 0 || p.Kind() != EffortBudget {
		t.Fatalf("budget must ignore toggle/effort gates: %v %+v", l, p)
	}

	// Supported=false strips? No: Budget is not None, so diagnosed drop.
	noSupport := enabledCaps(t, func(c *capability.Contract) {
		_ = c.Put("reasoning.supported", capability.BoolValue(false), enabledStrPtr("t"), capability.KindOverride)
	})
	if p, l := PlanEffort(g.Reasoning, noSupport, LocalRoute(capability.RuntimeLlamaCpp)); len(l) != 1 || p.IsPresent() {
		t.Fatalf("supported=false must drop budget: %v %+v", l, p)
	}

	// BudgetSupported=false drops.
	noBudget := enabledCaps(t, func(c *capability.Contract) {
		_ = c.Put("reasoning.budget.supported", capability.BoolValue(false), enabledStrPtr("t"), capability.KindOverride)
	})
	if p, l := PlanEffort(g.Reasoning, noBudget, LocalRoute(capability.RuntimeLlamaCpp)); len(l) != 1 || p.IsPresent() || l[0].Field != "thinking" {
		t.Fatalf("budget unsupported must drop thinking: %v %+v", l, p)
	}

	// Min/max bounds.
	tooSmall := enabledCaps(t, func(c *capability.Contract) {
		_ = c.Put("reasoning.budget.min_tokens", capability.IntValue(4096), enabledStrPtr("t"), capability.KindOverride)
	})
	if _, l := PlanEffort(g.Reasoning, tooSmall, LocalRoute(capability.RuntimeLlamaCpp)); len(l) != 1 {
		t.Fatalf("below min must drop: %v", l)
	}
	tooBig := enabledCaps(t, func(c *capability.Contract) {
		_ = c.Put("reasoning.budget.max_tokens", capability.IntValue(512), enabledStrPtr("t"), capability.KindOverride)
	})
	if _, l := PlanEffort(g.Reasoning, tooBig, LocalRoute(capability.RuntimeLlamaCpp)); len(l) != 1 {
		t.Fatalf("above max must drop: %v", l)
	}

	// Unobserved runtime drops via shared planner loss.
	if p, l := PlanEffort(g.Reasoning, enabledCaps(t, nil), LocalRoute(capability.RuntimeUnobserved)); len(l) != 1 || p.IsPresent() || l[0].Field != "thinking" {
		t.Fatalf("unobserved must drop: %v %+v", l, p)
	}

	// Strict refuses before I/O; lossy keeps header.
	if _, l := PlanEffort(g.Reasoning, enabledCaps(t, nil), OrdinaryRoute()); len(l) == 1 {
		if _, rerr := EnforcePolicy(l, false); rerr == nil || rerr.Code != CodeTranslationLoss {
			t.Fatalf("strict must 400 translation_loss: %v", rerr)
		}
		if kept, rerr := EnforcePolicy(l, true); rerr != nil || len(kept) != 1 {
			t.Fatalf("lossy must keep header: %v %v", kept, rerr)
		}
	}
}

func TestAnthropicEnabledEncodeWire(t *testing.T) {
	raw := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":777}}`
	_ = raw
	g, _, _ := DecodeAnthropicRequest([]byte(`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":777}}`), DefaultLimits())
	planned, losses := PlanEffort(g.Reasoning, enabledCaps(t, nil), LocalRoute(capability.RuntimeLlamaCpp))
	if len(losses) != 0 {
		t.Fatalf("llama must be lossless: %v", losses)
	}
	wire, rerr := EncodeChatRequest(g, planned, "up")
	if rerr != nil {
		t.Fatal(rerr)
	}
	var m map[string]any
	if err := json.Unmarshal(wire, &m); err != nil {
		t.Fatal(err)
	}
	if n, ok := m["thinking_budget_tokens"].(float64); !ok || int64(n) != 777 {
		t.Fatalf("llama wire must carry exact 777: %s", wire)
	}
	if _, has := m["reasoning_effort"]; has {
		t.Fatalf("budget must not emit effort: %s", wire)
	}
	// Ordinary drop emits nothing.
	planned2, _ := PlanEffort(g.Reasoning, enabledCaps(t, nil), OrdinaryRoute())
	wire2, rerr := EncodeChatRequest(g, planned2, "up")
	if rerr != nil {
		t.Fatal(rerr)
	}
	if strings.Contains(string(wire2), "thinking_budget") || strings.Contains(string(wire2), "reasoning_effort") {
		t.Fatalf("dropped must emit nothing: %s", wire2)
	}
	// Responses encoder rejects Budget as internal misuse.
	if _, rerr := EncodeResponsesRequest(g, planned, "up"); rerr == nil || rerr.Status != 500 {
		t.Fatalf("responses must reject Budget misuse: %v", rerr)
	}
}
