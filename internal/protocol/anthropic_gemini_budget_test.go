package protocol

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/arhammahajan/router/internal/capability"
)

// Anthropic enabled budget -> Gemini wire: decode admits the well-formed
// Budget intent, the shared planner gates the exact
// generationConfig.thinkingConfig.thinkingBudget on the pinned canonical
// native fact (verified primary GenerateContent REST contract
// https://ai.google.dev/api/generate-content and the thinking ranges in
// https://ai.google.dev/gemini-api/docs/generate-content/thinking),
// otherwise a diagnosed drop. The encoder owns the exact wire member.

func gemBudgetCaps(t *testing.T, apply func(*capability.Contract)) capability.ReasoningCapabilities {
	t.Helper()
	c := capability.Empty("gemini")
	if apply != nil {
		apply(&c)
	}
	c.Normalize()
	return capability.ReasoningFor(c)
}

func gemBudgetPut(t *testing.T, c *capability.Contract, path string, v capability.Value) {
	t.Helper()
	s := "t"
	if err := c.Put(path, v, &s, capability.KindOverride); err != nil {
		t.Fatal(err)
	}
}

func gemExactCaps(t *testing.T) capability.ReasoningCapabilities {
	return gemBudgetCaps(t, func(c *capability.Contract) {
		gemBudgetPut(t, c, "reasoning.budget.supported", capability.BoolValue(true))
		np, ok := capability.NewNativeParam("gemini", "generationConfig.thinkingConfig.thinkingBudget", nil)
		if !ok {
			t.Fatal("bad native param")
		}
		gemBudgetPut(t, c, "reasoning.budget.native_parameter", capability.NativeValue(np))
	})
}

func TestAnthGemBudgetDecodeAdmits(t *testing.T) {
	raw := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":2048}}`
	g, losses, rerr := DecodeAnthropicForGemini([]byte(raw), DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	if len(losses) != 0 {
		t.Fatalf("no-display must be lossless at decode: %v", losses)
	}
	if !g.Reasoning.IsBudget() || g.Reasoning.Origin() != "thinking" {
		t.Fatalf("must be Budget thinking: %+v", g.Reasoning)
	}
	if b, _ := g.Reasoning.Budget(); b != 2048 {
		t.Fatalf("budget must be exact: %d", b)
	}
	// Display is an independent dropped loss alongside the intent.
	rawD := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":512,"display":"summarized"}}`
	g, losses, rerr = DecodeAnthropicForGemini([]byte(rawD), DefaultLimits())
	if rerr != nil {
		t.Fatalf("display decode: %v", rerr)
	}
	if !g.Reasoning.IsBudget() {
		t.Fatalf("display must not block intent: %+v", g.Reasoning)
	}
	found := false
	for _, l := range losses {
		if l.Field == "thinking.display" && l.Action == "dropped" {
			found = true
			if strings.Contains(l.Reason, "512") || strings.Contains(l.Reason, "summarized") {
				t.Fatalf("loss must be value-free: %+v", l)
			}
		}
	}
	if !found {
		t.Fatalf("display loss missing: %v", losses)
	}
	// Responses still refuses; bare adaptive/between_tools refuse everywhere
	// (adaptive with an explicit effort decodes to the named intent).
	if _, _, rerr := DecodeAnthropicForResponses([]byte(raw), DefaultLimits()); rerr == nil || rerr.Status != 400 {
		t.Fatalf("responses must refuse: %v", rerr)
	}
	for _, bad := range []string{
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"adaptive"}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"between_tools"}}`,
	} {
		for _, dec := range []func([]byte, Limits) (*Generation, []Loss, *RouterError){
			DecodeAnthropicRequest, DecodeAnthropicForResponses, DecodeAnthropicForGemini,
		} {
			if _, _, rerr := dec([]byte(bad), DefaultLimits()); rerr == nil || rerr.Status != 400 {
				t.Fatalf("adaptive/between_tools must refuse: %s got %v", bad, rerr)
			}
		}
	}
}

func TestAnthGemBudgetPlanGates(t *testing.T) {
	raw := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":2048}}`
	g, _, _ := DecodeAnthropicForGemini([]byte(raw), DefaultLimits())

	// Trusted exact fact: lossless Budget.
	if p, l := PlanEffort(g.Reasoning, gemExactCaps(t), OrdinaryGeminiRoute()); len(l) != 0 {
		t.Fatalf("trusted must be lossless: %v", l)
	} else if b, _ := p.Budget(); b != 2048 {
		t.Fatalf("trusted must carry exact: %d", b)
	}

	// Unknown (nil) facts: diagnosed drop, never a heuristic.
	if p, l := PlanEffort(g.Reasoning, gemBudgetCaps(t, nil), OrdinaryGeminiRoute()); len(l) != 1 || p.IsPresent() {
		t.Fatalf("unknown must drop: %v %+v", l, p)
	} else if l[0].Field != "thinking" || l[0].Action != "dropped" {
		t.Fatalf("unknown loss: %v", l)
	}

	// Explicit false drops.
	noSupport := gemBudgetCaps(t, func(c *capability.Contract) {
		gemBudgetPut(t, c, "reasoning.budget.supported", capability.BoolValue(false))
		np, _ := capability.NewNativeParam("gemini", "generationConfig.thinkingConfig.thinkingBudget", nil)
		gemBudgetPut(t, c, "reasoning.budget.native_parameter", capability.NativeValue(np))
	})
	if _, l := PlanEffort(g.Reasoning, noSupport, OrdinaryGeminiRoute()); len(l) != 1 {
		t.Fatalf("supported=false must drop: %v", l)
	}

	// Alternate native descriptors drop: wrong protocol, wrong path, nil.
	for _, alt := range []struct {
		name     string
		protocol string
		path     string
		nilNP    bool
	}{
		{"wrong-protocol", "openai-chat", "generationConfig.thinkingConfig.thinkingBudget", false},
		{"wrong-path", "gemini", "reasoning_effort", false},
		{"nil", "", "", true},
	} {
		caps := gemBudgetCaps(t, func(c *capability.Contract) {
			gemBudgetPut(t, c, "reasoning.budget.supported", capability.BoolValue(true))
			if !alt.nilNP {
				np, ok := capability.NewNativeParam(alt.protocol, alt.path, nil)
				if !ok {
					t.Fatal("bad native")
				}
				gemBudgetPut(t, c, "reasoning.budget.native_parameter", capability.NativeValue(np))
			}
		})
		if p, l := PlanEffort(g.Reasoning, caps, OrdinaryGeminiRoute()); len(l) != 1 || p.IsPresent() {
			t.Fatalf("%s must drop: %v %+v", alt.name, l, p)
		}
	}

	// Shared gates still apply: supported/min/max.
	noModel := gemBudgetCaps(t, func(c *capability.Contract) {
		gemBudgetPut(t, c, "reasoning.supported", capability.BoolValue(false))
		gemBudgetPut(t, c, "reasoning.budget.supported", capability.BoolValue(true))
		np, _ := capability.NewNativeParam("gemini", "generationConfig.thinkingConfig.thinkingBudget", nil)
		gemBudgetPut(t, c, "reasoning.budget.native_parameter", capability.NativeValue(np))
	})
	if _, l := PlanEffort(g.Reasoning, noModel, OrdinaryGeminiRoute()); len(l) != 1 {
		t.Fatalf("supported=false must drop: %v", l)
	}
	tooSmall := gemBudgetCaps(t, func(c *capability.Contract) {
		gemBudgetPut(t, c, "reasoning.budget.supported", capability.BoolValue(true))
		np, _ := capability.NewNativeParam("gemini", "generationConfig.thinkingConfig.thinkingBudget", nil)
		gemBudgetPut(t, c, "reasoning.budget.native_parameter", capability.NativeValue(np))
		gemBudgetPut(t, c, "reasoning.budget.min_tokens", capability.IntValue(4096))
	})
	if _, l := PlanEffort(g.Reasoning, tooSmall, OrdinaryGeminiRoute()); len(l) != 1 {
		t.Fatalf("below min must drop: %v", l)
	}
	tooBig := gemBudgetCaps(t, func(c *capability.Contract) {
		gemBudgetPut(t, c, "reasoning.budget.supported", capability.BoolValue(true))
		np, _ := capability.NewNativeParam("gemini", "generationConfig.thinkingConfig.thinkingBudget", nil)
		gemBudgetPut(t, c, "reasoning.budget.native_parameter", capability.NativeValue(np))
		gemBudgetPut(t, c, "reasoning.budget.max_tokens", capability.IntValue(512))
	})
	if _, l := PlanEffort(g.Reasoning, tooBig, OrdinaryGeminiRoute()); len(l) != 1 {
		t.Fatalf("above max must drop: %v", l)
	}

	// int32 wire range: above MaxInt32 drops.
	bigRaw := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":2147483648}}`
	gb, _, _ := DecodeAnthropicForGemini([]byte(bigRaw), DefaultLimits())
	if _, l := PlanEffort(gb.Reasoning, gemExactCaps(t), OrdinaryGeminiRoute()); len(l) != 1 {
		t.Fatalf("int32 overflow must drop: %v", l)
	}

	// Named/Off never emit a Gemini mapping.
	named, _ := NewChatEffort("medium")
	if p, l := PlanEffort(named, gemExactCaps(t), OrdinaryGeminiRoute()); len(l) != 1 || p.IsPresent() {
		t.Fatalf("named must drop for gemini: %v %+v", l, p)
	}
	off := NewAnthropicDisabled()
	if p, l := PlanEffort(off, gemExactCaps(t), OrdinaryGeminiRoute()); len(l) != 1 || p.IsPresent() {
		t.Fatalf("off must drop for gemini: %v %+v", l, p)
	}

	// Strict refuses pre-I/O; lossy keeps header.
	if _, l := PlanEffort(g.Reasoning, gemBudgetCaps(t, nil), OrdinaryGeminiRoute()); len(l) == 1 {
		if _, rerr := EnforcePolicy(l, false); rerr == nil || rerr.Code != CodeTranslationLoss {
			t.Fatalf("strict must 400 translation_loss: %v", rerr)
		}
		if kept, rerr := EnforcePolicy(l, true); rerr != nil || len(kept) != 1 {
			t.Fatalf("lossy must keep header: %v %v", kept, rerr)
		}
	}
}

func TestAnthGemBudgetEncodeWire(t *testing.T) {
	raw := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":777}}`
	g, _, _ := DecodeAnthropicForGemini([]byte(raw), DefaultLimits())
	planned, losses := PlanEffort(g.Reasoning, gemExactCaps(t), OrdinaryGeminiRoute())
	if len(losses) != 0 {
		t.Fatalf("trusted must be lossless: %v", losses)
	}
	out, rerr := EncodeGeminiRequestWithPlan(g, planned)
	if rerr != nil {
		t.Fatal(rerr)
	}
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	genCfg, ok := body["generationConfig"].(map[string]any)
	if !ok {
		t.Fatalf("generationConfig missing: %s", out)
	}
	think, ok := genCfg["thinkingConfig"].(map[string]any)
	if !ok {
		t.Fatalf("thinkingConfig missing: %s", out)
	}
	if v, ok := think["thinkingBudget"].(float64); !ok || int64(v) != 777 {
		t.Fatalf("wire must carry exact 777: %s", out)
	}
	if len(think) != 1 {
		t.Fatalf("thinkingConfig must carry only thinkingBudget: %s", out)
	}
	if !strings.Contains(string(out), `"thinkingBudget":777`) {
		t.Fatalf("exact numeric wire: %s", out)
	}
	// Dropped emits no thinkingConfig.
	planned2, _ := PlanEffort(g.Reasoning, gemBudgetCaps(t, nil), OrdinaryGeminiRoute())
	out2, rerr := EncodeGeminiRequestWithPlan(g, planned2)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if strings.Contains(string(out2), "thinkingBudget") || strings.Contains(string(out2), "thinkingConfig") {
		t.Fatalf("dropped must emit nothing: %s", out2)
	}
	// Named now encodes the exact official thinkingLevel (reviewed
	// Chat/Responses effort -> Gemini unit); Off stays internal misuse
	// (the shared planner drops it pre-I/O, never encodes it).
	if out, rerr := EncodeGeminiRequestWithPlan(g, NamedEffortTarget("low")); rerr != nil {
		t.Fatalf("named must encode thinkingLevel: %v", rerr)
	} else if !strings.Contains(string(out), `"thinkingLevel":"LOW"`) {
		t.Fatalf("named wire must carry LOW: %s", out)
	}
	if _, rerr := EncodeGeminiRequestWithPlan(g, OffEffortTarget()); rerr == nil || rerr.Status != 500 {
		t.Fatalf("off must be 500 misuse: %v", rerr)
	}
}
