package protocol

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/arhammahajan/model-router/internal/capability"
)

func testContractWith(apply func(c *capability.Contract)) capability.Contract {
	c := capability.Empty("openai-responses")
	apply(&c)
	c.Normalize()
	return c
}

func boolPtr(b bool) *bool { v := b; return &v }

func TestCFREffortDecodesToIntent(t *testing.T) {
	g, losses, rerr := DecodeChatForResponses([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"medium"}`), DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	if len(losses) != 0 {
		t.Fatalf("exact effort must emit no loss, got %v", losses)
	}
	lvl, ok := g.Reasoning.Level()
	if !ok || lvl != "medium" {
		t.Fatalf("intent not preserved: %+v", g.Reasoning)
	}
}

func TestCFREffortNullAbsent(t *testing.T) {
	g, losses, rerr := DecodeChatForResponses([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":null}`), DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	if g.Reasoning.IsPresent() {
		t.Fatalf("null must be absent")
	}
	if len(losses) != 0 {
		t.Fatalf("null must emit no loss: %v", losses)
	}
}

func TestCFREffortMalformed(t *testing.T) {
	for _, raw := range []string{
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":123}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":""}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":true}`,
	} {
		if _, _, rerr := DecodeChatForResponses([]byte(raw), DefaultLimits()); rerr == nil || rerr.Status != 400 {
			t.Fatalf("malformed must 400: %s got %v", raw, rerr)
		}
	}
}

func TestCFREffortThinkingConflict(t *testing.T) {
	raw := `{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"medium","extra_body":{"thinking":{"type":"enabled","budget_tokens":100}}}`
	_, _, rerr := DecodeChatForResponses([]byte(raw), DefaultLimits())
	if rerr == nil || rerr.Status != 400 || rerr.Code != CodeInvalidGeneration {
		t.Fatalf("conflict must fail pre-I/O with invalid_generation_parameter, got %v", rerr)
	}
	if !strings.Contains(rerr.Msg, "reasoning_effort") {
		t.Fatalf("conflict diagnostic must name origin: %v", rerr.Msg)
	}
	// Thinking alone remains a diagnosed loss, not a 400.
	_, losses, rerr := DecodeChatForResponses([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"extra_body":{"thinking":{"type":"enabled","budget_tokens":100}}}`), DefaultLimits())
	if rerr != nil {
		t.Fatalf("thinking alone must not 400: %v", rerr)
	}
	found := false
	for _, l := range losses {
		if l.Field == "extra_body.thinking" {
			found = true
		}
	}
	if !found {
		t.Fatalf("thinking alone must stay a loss: %v", losses)
	}
}

func TestPlanUnknownNeutralExact(t *testing.T) {
	intent, _ := NewChatEffort("medium")
	caps := capability.ReasoningFor(capability.Empty("openai-responses"))
	planned, losses := PlanEffort(intent, caps, OrdinaryRoute())
	if len(losses) != 0 {
		t.Fatalf("unknown caps must be neutral: %v", losses)
	}
	eff, ok := planned.Effort()
	if !ok || eff != "medium" {
		t.Fatalf("literal must be preserved: %+v", planned)
	}
}

func TestPlanSupportedFalse(t *testing.T) {
	intent, _ := NewChatEffort("medium")
	contract := testContractWith(func(c *capability.Contract) {
		_ = c.Put("reasoning.supported", capability.BoolValue(false), strPtr("test"), capability.KindOverride)
	})
	caps := capability.ReasoningFor(contract)
	_, losses := PlanEffort(intent, caps, OrdinaryRoute())
	if len(losses) != 1 || losses[0].Field != "reasoning_effort" || losses[0].Action != "dropped" {
		t.Fatalf("explicit false must drop with origin: %v", losses)
	}
	// none may be stripped exactly on a nonreasoning model.
	noneIntent, _ := NewChatEffort("none")
	planned, losses := PlanEffort(noneIntent, caps, OrdinaryRoute())
	if len(losses) != 0 || planned.IsPresent() {
		t.Fatalf("none on nonreasoning model must strip exactly: %+v %v", planned, losses)
	}
}

func TestPlanEffortSupportedFalse(t *testing.T) {
	intent, _ := NewChatEffort("high")
	contract := testContractWith(func(c *capability.Contract) {
		_ = c.Put("reasoning.effort.supported", capability.BoolValue(false), strPtr("test"), capability.KindOverride)
	})
	caps := capability.ReasoningFor(contract)
	_, losses := PlanEffort(intent, caps, OrdinaryRoute())
	if len(losses) != 1 || losses[0].Field != "reasoning_effort" {
		t.Fatalf("effort.supported=false must drop: %v", losses)
	}
}

func TestPlanEnumMismatchVsExact(t *testing.T) {
	vals, _ := capability.StringListValue([]string{"low", "medium"})
	contract := testContractWith(func(c *capability.Contract) {
		_ = c.Put("reasoning.effort.values", vals, strPtr("test"), capability.KindOverride)
	})
	caps := capability.ReasoningFor(contract)
	good, _ := NewChatEffort("medium")
	if _, losses := PlanEffort(good, caps, OrdinaryRoute()); len(losses) != 0 {
		t.Fatalf("known value must be exact: %v", losses)
	}
	bad, _ := NewChatEffort("high")
	_, losses := PlanEffort(bad, caps, OrdinaryRoute())
	if len(losses) != 1 || losses[0].Field != "reasoning_effort" {
		t.Fatalf("unknown value must drop: %v", losses)
	}
	if strings.Contains(losses[0].Reason, "high") {
		t.Fatalf("diagnostic must be value-free: %v", losses[0].Reason)
	}
}

func TestPlanKnownEmptyVsUnknown(t *testing.T) {
	emptyVals, _ := capability.StringListValue([]string{})
	emptyContract := testContractWith(func(c *capability.Contract) {
		_ = c.Put("reasoning.effort.values", emptyVals, strPtr("test"), capability.KindOverride)
	})
	emptyCaps := capability.ReasoningFor(emptyContract)
	if !emptyCaps.EffortValues.Set || len(emptyCaps.EffortValues.List) != 0 {
		t.Fatalf("known empty must stay known: %+v", emptyCaps.EffortValues)
	}
	intent, _ := NewChatEffort("low")
	if _, losses := PlanEffort(intent, emptyCaps, OrdinaryRoute()); len(losses) != 1 {
		t.Fatalf("known empty must drop every level: %v", losses)
	}
	unknownCaps := capability.ReasoningFor(capability.Empty("openai-responses"))
	if unknownCaps.EffortValues.Set {
		t.Fatalf("unknown must stay unknown")
	}
	if _, losses := PlanEffort(intent, unknownCaps, OrdinaryRoute()); len(losses) != 0 {
		t.Fatalf("unknown must pass neutral: %v", losses)
	}
}

func TestPlanNoneGating(t *testing.T) {
	none, _ := NewChatEffort("none")
	// Unknown caps: exact none.
	unknown := capability.ReasoningFor(capability.Empty("openai-responses"))
	if planned, losses := PlanEffort(none, unknown, OrdinaryRoute()); len(losses) != 0 || !planned.IsPresent() {
		t.Fatalf("unknown none must be exact: %v %+v", losses, planned)
	}
	// Known enum without none: drop.
	vals, _ := capability.StringListValue([]string{"low", "medium"})
	cNoNone := testContractWith(func(c *capability.Contract) {
		_ = c.Put("reasoning.effort.values", vals, strPtr("test"), capability.KindOverride)
	})
	if _, losses := PlanEffort(none, capability.ReasoningFor(cNoNone), OrdinaryRoute()); len(losses) != 1 {
		t.Fatalf("enum without none must drop: %v", losses)
	}
	// Toggle false: drop even with unknown enum.
	cToggleFalse := testContractWith(func(c *capability.Contract) {
		_ = c.Put("reasoning.toggle.supported", capability.BoolValue(false), strPtr("test"), capability.KindOverride)
	})
	if _, losses := PlanEffort(none, capability.ReasoningFor(cToggleFalse), OrdinaryRoute()); len(losses) != 1 {
		t.Fatalf("toggle false must drop none: %v", losses)
	}
}

func TestPlanNoClamp(t *testing.T) {
	vals, _ := capability.StringListValue([]string{"low", "medium"})
	contract := testContractWith(func(c *capability.Contract) {
		_ = c.Put("reasoning.effort.values", vals, strPtr("test"), capability.KindOverride)
	})
	caps := capability.ReasoningFor(contract)
	minimal, _ := NewChatEffort("minimal")
	planned, losses := PlanEffort(minimal, caps, OrdinaryRoute())
	if len(losses) != 1 || planned.IsPresent() {
		t.Fatalf("minimal must drop, never become low: %v %+v", losses, planned)
	}
}

func TestPlanLlamaBudgetMapping(t *testing.T) {
	// Migrated vertical: named effort on a known LlamaCpp server maps to
	// the exact native budget; none maps to the disable toggle; names
	// outside the table (including max) are diagnosed losses.
	intent, _ := NewChatEffort("medium")
	caps := capability.ReasoningFor(capability.Empty("openai-responses"))
	planned, losses := PlanEffort(intent, caps, LocalRoute(capability.RuntimeLlamaCpp))
	if len(losses) != 0 {
		t.Fatalf("mapped level must be exact: %v", losses)
	}
	if budget, ok := planned.Budget(); !ok || budget != 8192 {
		t.Fatalf("medium must map to 8192: %+v", planned)
	}
	none, _ := NewChatEffort("none")
	pNone, lNone := PlanEffort(none, caps, LocalRoute(capability.RuntimeLlamaCpp))
	if len(lNone) != 0 || pNone.Kind() != EffortOff {
		t.Fatalf("llama none must disable: %+v %v", pNone, lNone)
	}
	max, _ := NewChatEffort("max")
	if pMax, lMax := PlanEffort(max, caps, LocalRoute(capability.RuntimeLlamaCpp)); len(lMax) != 1 || pMax.IsPresent() {
		t.Fatalf("max must drop: %+v %v", pMax, lMax)
	}
	// Strata reuses ordinary named behavior: the literal is preserved.
	pStrata, lStrata := PlanEffort(intent, caps, LocalRoute(capability.RuntimeStrata))
	if len(lStrata) != 0 {
		t.Fatalf("strata named must be exact: %v", lStrata)
	}
	if eff, ok := pStrata.Effort(); !ok || eff != "medium" {
		t.Fatalf("strata must preserve literal: %+v", pStrata)
	}
}

func TestEncodeReasoningEffortWire(t *testing.T) {
	g, _, rerr := DecodeChatForResponses([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"medium"}`), DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	caps := capability.ReasoningFor(capability.Empty("openai-responses"))
	planned, losses := PlanEffort(g.Reasoning, caps, OrdinaryRoute())
	if len(losses) != 0 {
		t.Fatalf("losses: %v", losses)
	}
	out, rerr := EncodeResponsesRequest(g, planned, "up")
	if rerr != nil {
		t.Fatalf("encode: %v", rerr)
	}
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	reas, ok := body["reasoning"].(map[string]any)
	if !ok || reas["effort"] != "medium" {
		t.Fatalf("wire must carry reasoning.effort: %s", out)
	}
	if _, has := body["reasoning_effort"]; has {
		t.Fatalf("must not emit Chat field on Responses wire: %s", out)
	}
}

func TestReasoningForCloneOwnership(t *testing.T) {
	c := capability.Empty("openai-responses")
	vals, _ := capability.StringListValue([]string{"low"})
	_ = c.Put("reasoning.effort.values", vals, strPtr("test"), capability.KindOverride)
	got := capability.ReasoningFor(c)
	got.EffortValues.List[0] = "mutated"
	again := capability.ReasoningFor(c)
	if again.EffortValues.List[0] != "low" {
		t.Fatalf("projection must clone: %v", again.EffortValues)
	}
}

func strPtr(s string) *string { v := s; return &v }

var _ = boolPtr
