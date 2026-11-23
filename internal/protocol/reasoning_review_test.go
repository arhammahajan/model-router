package protocol

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/arhammahajan/router/internal/capability"
)

// Independent review of Chat reasoning_effort -> Responses reasoning.effort.
// Stable source: git show HEAD:model_router/{reasoning,protocol}.py.
// Hermetic: no network, no env, no Python working tree.
// Covers: decode gates, malformed+conflict pre-IO both modes, planner branch
// ordering (supported=false+none exact, toggle/effort/known-empty gates,
// unknown neutral, no clamp/rename, value-free origins), encoder wire.

func rrCap(t *testing.T, fn func(c *capability.Contract)) capability.Contract {
	t.Helper()
	c := capability.Empty("openai-responses")
	apply := func(path string, v capability.Value) {
		t.Helper()
		s := "review-test"
		if err := c.Put(path, v, &s, capability.KindOverride); err != nil {
			t.Fatalf("put %s: %v", path, err)
		}
	}
	_ = apply
	// Build via typed helpers to avoid stringly misuse.
	if fn != nil {
		fn(&c)
	}
	c.Normalize()
	return c
}

func rrCaps(t *testing.T, fn func(c *capability.Contract)) capability.ReasoningCapabilities {
	t.Helper()
	return capability.ReasoningFor(rrCap(t, fn))
}

func rrPutVal(t *testing.T, c *capability.Contract, path string, v capability.Value) {
	t.Helper()
	s := "review-test"
	if err := c.Put(path, v, &s, capability.KindOverride); err != nil {
		t.Fatalf("put %s: %v", path, err)
	}
}

func TestRRDecodeEffortExactNoLoss(t *testing.T) {
	g, losses, rerr := DecodeChatForResponses([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"medium"}`), DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	if len(losses) != 0 {
		t.Fatalf("exact effort must emit no loss: %v", losses)
	}
	lvl, ok := g.Reasoning.Level()
	if !ok || lvl != "medium" {
		t.Fatalf("intent literal lost: %+v", g.Reasoning)
	}
	if got := g.Reasoning.Origin(); got != "reasoning_effort" {
		t.Fatalf("origin must be source path: %q", got)
	}
}

func TestRRDecodeNullAbsent(t *testing.T) {
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

func TestRRDecodeMalformedBothModesPreIO(t *testing.T) {
	// Go's closed Chat thinking shape for this dest: type non-empty,
	// budget_tokens positive int when present, no unknown members.
	// Missing-budget enabled is well-formed here (diagnosed loss, not 400);
	// it is not part of the effort vertical's malformed gate.
	for _, raw := range []string{
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":123}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":""}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":true}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"extra_body":{"thinking":{"type":"enabled","budget_tokens":0}}}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"extra_body":{"thinking":{"type":"enabled","budget_tokens":5,"nope":1}}}`,
	} {
		g, losses, rerr := DecodeChatForResponses([]byte(raw), DefaultLimits())
		if rerr == nil || rerr.Status != 400 {
			t.Fatalf("malformed must 400 pre-IO: %s got gen=%v losses=%v err=%v", raw, g, losses, rerr)
		}
		// Decode errors happen before EnforcePolicy in both modes: even a
		// lossy caller must not get a loss plan.
		if len(losses) != 0 {
			t.Fatalf("malformed must not produce losses: %s %v", raw, losses)
		}
	}
}

func TestRRDecodeConflictPreIOBothModes(t *testing.T) {
	raw := `{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"medium","extra_body":{"thinking":{"type":"enabled","budget_tokens":100}}}`
	_, losses, rerr := DecodeChatForResponses([]byte(raw), DefaultLimits())
	if rerr == nil || rerr.Status != 400 || rerr.Code != CodeInvalidGeneration {
		t.Fatalf("conflict must 400 invalid_generation_parameter: %v losses=%v", rerr, losses)
	}
	if !strings.Contains(rerr.Msg, "reasoning_effort") {
		t.Fatalf("conflict must name origin: %v", rerr.Msg)
	}
	// Thinking alone stays a diagnosed loss (not 400).
	_, losses, rerr = DecodeChatForResponses([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"extra_body":{"thinking":{"type":"enabled","budget_tokens":100}}}`), DefaultLimits())
	if rerr != nil {
		t.Fatalf("thinking alone must not 400: %v", rerr)
	}
	found := false
	for _, l := range losses {
		if l.Field == "extra_body.thinking" && l.Action == "dropped" {
			found = true
		}
	}
	if !found {
		t.Fatalf("thinking alone must stay diagnosed loss: %v", losses)
	}
}

func TestRRPlannerSupportedFalseNoneExact(t *testing.T) {
	c := capability.Empty("openai-responses")
	rrPutVal(t, &c, "reasoning.supported", capability.BoolValue(false))
	c.Normalize()
	caps := capability.ReasoningFor(c)
	none, _ := NewChatEffort("none")
	planned, losses := PlanEffort(none, caps, OrdinaryRoute())
	if len(losses) != 0 || planned.IsPresent() {
		// Python: disable on non-reasoning model strips exactly (no emission, no loss).
		t.Fatalf("supported=false+none must strip exactly: planned=%+v losses=%v", planned, losses)
	}
	named, _ := NewChatEffort("medium")
	_, losses = PlanEffort(named, caps, OrdinaryRoute())
	if len(losses) != 1 || losses[0].Field != "reasoning_effort" || losses[0].Action != "dropped" {
		t.Fatalf("supported=false named must drop with origin: %v", losses)
	}
	if strings.Contains(losses[0].Reason, "medium") {
		t.Fatalf("diagnostic must be value-free: %v", losses[0])
	}
}

func TestRRPlannerNoneGatingToggleEffortEnum(t *testing.T) {
	none, _ := NewChatEffort("none")
	// Unknown neutral: exact none.
	unknown := capability.ReasoningFor(capability.Empty("openai-responses"))
	planned, losses := PlanEffort(none, unknown, OrdinaryRoute())
	if len(losses) != 0 {
		t.Fatalf("unknown none must be neutral: %v", losses)
	}
	if eff, ok := planned.Effort(); !ok || eff != "none" {
		t.Fatalf("unknown none must emit literal none: %+v", planned)
	}
	// Toggle false drops none.
	cT := capability.Empty("openai-responses")
	rrPutVal(t, &cT, "reasoning.toggle.supported", capability.BoolValue(false))
	cT.Normalize()
	if _, losses := PlanEffort(none, capability.ReasoningFor(cT), OrdinaryRoute()); len(losses) != 1 {
		t.Fatalf("toggle=false none must drop: %v", losses)
	}
	// Effort false drops none with the known-values reason.
	cE := capability.Empty("openai-responses")
	rrPutVal(t, &cE, "reasoning.effort.supported", capability.BoolValue(false))
	cE.Normalize()
	_, losses = PlanEffort(none, capability.ReasoningFor(cE), OrdinaryRoute())
	if len(losses) != 1 || !strings.Contains(losses[0].Reason, "cannot express") {
		t.Fatalf("effort=false none must drop as inexpressible: %v", losses)
	}
	// Known enum without none drops.
	vals, _ := capability.StringListValue([]string{"low", "medium"})
	cN := capability.Empty("openai-responses")
	rrPutVal(t, &cN, "reasoning.effort.values", vals)
	cN.Normalize()
	if _, losses := PlanEffort(none, capability.ReasoningFor(cN), OrdinaryRoute()); len(losses) != 1 {
		t.Fatalf("enum-without-none must drop: %v", losses)
	}
	// Known enum with none admits.
	vals2, _ := capability.StringListValue([]string{"low", "none"})
	cY := capability.Empty("openai-responses")
	rrPutVal(t, &cY, "reasoning.effort.values", vals2)
	cY.Normalize()
	if planned, losses := PlanEffort(none, capability.ReasoningFor(cY), OrdinaryRoute()); len(losses) != 0 || !planned.IsPresent() {
		t.Fatalf("enum-with-none must be exact: %+v %v", planned, losses)
	}
}

func TestRRPlannerNamedGatesNoClamp(t *testing.T) {
	// Unknown neutral exact.
	intent, _ := NewChatEffort("medium")
	unknown := capability.ReasoningFor(capability.Empty("openai-responses"))
	planned, losses := PlanEffort(intent, unknown, OrdinaryRoute())
	if len(losses) != 0 {
		t.Fatalf("unknown must pass neutral: %v", losses)
	}
	if eff, _ := planned.Effort(); eff != "medium" {
		t.Fatalf("literal must be preserved: %+v", planned)
	}
	// Effort false drops.
	cE := capability.Empty("openai-responses")
	rrPutVal(t, &cE, "reasoning.effort.supported", capability.BoolValue(false))
	cE.Normalize()
	if _, losses := PlanEffort(intent, capability.ReasoningFor(cE), OrdinaryRoute()); len(losses) != 1 {
		t.Fatalf("effort=false must drop: %v", losses)
	}
	// Known-empty drops every level.
	emptyVals, _ := capability.StringListValue([]string{})
	cK := capability.Empty("openai-responses")
	rrPutVal(t, &cK, "reasoning.effort.values", emptyVals)
	cK.Normalize()
	emptyCaps := capability.ReasoningFor(cK)
	if !emptyCaps.EffortValues.Set {
		t.Fatalf("known-empty must stay set")
	}
	if _, losses := PlanEffort(intent, emptyCaps, OrdinaryRoute()); len(losses) != 1 {
		t.Fatalf("known-empty must drop: %v", losses)
	}
	// Enum mismatch drops, exact passes, never clamps minimal->low.
	vals, _ := capability.StringListValue([]string{"low", "medium"})
	cM := capability.Empty("openai-responses")
	rrPutVal(t, &cM, "reasoning.effort.values", vals)
	cM.Normalize()
	caps := capability.ReasoningFor(cM)
	good, _ := NewChatEffort("medium")
	if _, losses := PlanEffort(good, caps, OrdinaryRoute()); len(losses) != 0 {
		t.Fatalf("known value must be exact: %v", losses)
	}
	bad, _ := NewChatEffort("high")
	_, losses = PlanEffort(bad, caps, OrdinaryRoute())
	if len(losses) != 1 {
		t.Fatalf("unknown value must drop: %v", losses)
	}
	minimal, _ := NewChatEffort("minimal")
	pMin, losses := PlanEffort(minimal, caps, OrdinaryRoute())
	if len(losses) != 1 || pMin.IsPresent() {
		t.Fatalf("minimal must drop, never become low: %+v %v", pMin, losses)
	}
	if strings.Contains(losses[0].Reason, "minimal") {
		t.Fatalf("diagnostic must be value-free: %v", losses[0])
	}
}

func TestRREncoderEmitsRealReasoningEffort(t *testing.T) {
	for _, lvl := range []string{"minimal", "low", "medium", "high", "none"} {
		g, _, rerr := DecodeChatForResponses([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"`+lvl+`"}`), DefaultLimits())
		if rerr != nil {
			t.Fatalf("decode %s: %v", lvl, rerr)
		}
		caps := capability.ReasoningFor(capability.Empty("openai-responses"))
		planned, losses := PlanEffort(g.Reasoning, caps, OrdinaryRoute())
		if len(losses) != 0 {
			t.Fatalf("unknown must be exact for %s: %v", lvl, losses)
		}
		out, rerr := EncodeResponsesRequest(g, planned, "up")
		if rerr != nil {
			t.Fatalf("encode %s: %v", lvl, rerr)
		}
		var body map[string]any
		if err := json.Unmarshal(out, &body); err != nil {
			t.Fatal(err)
		}
		reas, ok := body["reasoning"].(map[string]any)
		if !ok || reas["effort"] != lvl {
			t.Fatalf("wire must carry reasoning.effort %q: %s", lvl, out)
		}
		if _, has := body["reasoning_effort"]; has {
			t.Fatalf("must not emit Chat field on Responses wire: %s", out)
		}
	}
}

func TestRROtherDestLossUnchanged(t *testing.T) {
	// Same Chat body on the Anthropic-destination decoder must NOT decode an
	// intent: reasoning_effort stays a diagnosed loss there.
	_, losses, rerr := DecodeChatRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"medium"}`), DefaultLimits())
	if rerr != nil {
		t.Fatalf("anthropic-dest decode: %v", rerr)
	}
	found := false
	for _, l := range losses {
		if l.Field == "reasoning_effort" {
			found = true
		}
	}
	if !found {
		t.Fatalf("other dest must retain diagnosed loss: %v", losses)
	}
	// Responses-dest decoder still diagnoses stop exactly as before.
	_, losses, rerr = DecodeChatForResponses([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"medium","stop":"x"}`), DefaultLimits())
	if rerr != nil {
		t.Fatalf("stop decode: %v", rerr)
	}
	found = false
	for _, l := range losses {
		if l.Field == "stop" {
			found = true
		}
	}
	if !found {
		t.Fatalf("other source losses must be unchanged: %v", losses)
	}
}

func TestRRLlamaMapperBehavior(t *testing.T) {
	// Migrated vertical: the old blanket llama-loss branch is deleted.
	// Known LlamaCpp maps recognized levels to exact native targets;
	// Strata reuses ordinary named behavior; unknown names drop.
	intent, _ := NewChatEffort("medium")
	caps := capability.ReasoningFor(capability.Empty("openai-responses"))
	planned, losses := PlanEffort(intent, caps, LocalRoute(capability.RuntimeLlamaCpp))
	if len(losses) != 0 {
		t.Fatalf("llama mapped level must be exact: %v", losses)
	}
	if budget, ok := planned.Budget(); !ok || budget != 8192 {
		t.Fatalf("llama medium must map to 8192: %+v", planned)
	}
	none, _ := NewChatEffort("none")
	pNone, lNone := PlanEffort(none, caps, LocalRoute(capability.RuntimeLlamaCpp))
	if len(lNone) != 0 || pNone.Kind() != EffortOff {
		t.Fatalf("llama none must disable: %+v %v", pNone, lNone)
	}
	pStrata, lStrata := PlanEffort(intent, caps, LocalRoute(capability.RuntimeStrata))
	if len(lStrata) != 0 {
		t.Fatalf("strata named must be exact: %v", lStrata)
	}
	if eff, ok := pStrata.Effort(); !ok || eff != "medium" {
		t.Fatalf("strata must preserve literal: %+v", pStrata)
	}
}
