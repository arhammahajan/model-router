// Translated planner for local runtimes: known-engine gates, none
// branch ordering, enum/unknown/max handling, strict zero-I/O and lossy
// drop/header behavior, and Chat/Responses wire rendering. Hermetic.
package protocol

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/arhammahajan/model-router/internal/capability"
)

func llamaCaps(t *testing.T, mutate func(*capability.Contract)) capability.ReasoningCapabilities {
	t.Helper()
	base := capability.Empty("openai-chat")
	capability.ApplyLlamaMapper(&base, capability.RuntimeLlamaCpp, 1000)
	if mutate != nil {
		mutate(&base)
	}
	return capability.ReasoningFor(base)
}

func TestTranslatedLlamaCppGates(t *testing.T) {
	newEffort := func(s string) ReasoningIntent {
		i, ok := NewResponsesEffort(s)
		if !ok {
			t.Fatalf("intent %q", s)
		}
		return i
	}
	// reasoning.supported=false first: none strips exactly, named drops.
	unsupported := llamaCaps(t, func(c *capability.Contract) {
		_ = c.Put("reasoning.supported", capability.BoolValue(false), strPtr("t"), capability.KindOverride)
	})
	if p, l := PlanEffort(newEffort("none"), unsupported, LocalRoute(capability.RuntimeLlamaCpp)); len(l) != 0 || p.IsPresent() {
		t.Fatalf("none on unsupported must strip: %+v %v", p, l)
	}
	if _, l := PlanEffort(newEffort("low"), unsupported, LocalRoute(capability.RuntimeLlamaCpp)); len(l) != 1 {
		t.Fatalf("named on unsupported must drop: %v", l)
	}
	// Toggle gate: none with toggle=false drops even though the enum
	// admits none (LlamaCpp checks toggle first, then maps directly).
	noToggle := llamaCaps(t, func(c *capability.Contract) {
		_ = c.Put("reasoning.toggle.supported", capability.BoolValue(false), strPtr("t"), capability.KindOverride)
	})
	if _, l := PlanEffort(newEffort("none"), noToggle, LocalRoute(capability.RuntimeLlamaCpp)); len(l) != 1 {
		t.Fatalf("none without toggle must drop: %v", l)
	}
	// LlamaCpp none maps directly after the toggle gate: no effort/enum
	// check (unlike ordinary/Strata). Narrow the enum to exclude none.
	narrow := llamaCaps(t, func(c *capability.Contract) {
		v, _ := capability.StringListValue([]string{"low", "medium"})
		_ = c.Put("reasoning.effort.values", v, strPtr("t"), capability.KindOverride)
	})
	if p, l := PlanEffort(newEffort("none"), narrow, LocalRoute(capability.RuntimeLlamaCpp)); len(l) != 0 || p.Kind() != EffortOff {
		t.Fatalf("llama none must map despite narrow enum: %+v %v", p, l)
	}
	// Effort gate: explicit effort=false drops named.
	effOff := llamaCaps(t, func(c *capability.Contract) {
		_ = c.Put("reasoning.effort.supported", capability.BoolValue(false), strPtr("t"), capability.KindOverride)
	})
	if _, l := PlanEffort(newEffort("low"), effOff, LocalRoute(capability.RuntimeLlamaCpp)); len(l) != 1 {
		t.Fatalf("effort=false must drop: %v", l)
	}
	// Known enum without the level drops; unknown max drops as loss.
	if _, l := PlanEffort(newEffort("ultra"), llamaCaps(t, nil), LocalRoute(capability.RuntimeLlamaCpp)); len(l) != 1 {
		t.Fatalf("unknown must drop: %v", l)
	}
	if p, l := PlanEffort(newEffort("max"), llamaCaps(t, nil), LocalRoute(capability.RuntimeLlamaCpp)); len(l) != 1 || p.IsPresent() {
		t.Fatalf("max must drop with no emission: %+v %v", p, l)
	}
	// Diagnostics are value-free static paths.
	for _, lvl := range []string{"ultra", "max"} {
		_, l := PlanEffort(newEffort(lvl), llamaCaps(t, nil), LocalRoute(capability.RuntimeLlamaCpp))
		if l[0].Field != "reasoning.effort" || strings.Contains(l[0].Reason, lvl) {
			t.Fatalf("diagnostic must be value-free: %+v", l)
		}
	}
}

func TestTranslatedLlamaCppExactWire(t *testing.T) {
	gen, _, rerr := DecodeResponsesRequest([]byte(`{"model":"m","input":"hi","reasoning":{"effort":"high"}}`), DefaultLimits())
	if rerr != nil {
		t.Fatal(rerr)
	}
	planned, losses := PlanEffort(gen.Reasoning, llamaCaps(t, nil), LocalRoute(capability.RuntimeLlamaCpp))
	if len(losses) != 0 {
		t.Fatalf("losses: %v", losses)
	}
	out, rerr := EncodeChatRequest(gen, planned, "up")
	if rerr != nil {
		t.Fatal(rerr)
	}
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	if body["thinking_budget_tokens"] != float64(16384) {
		t.Fatalf("exact budget member: %s", out)
	}
	if _, has := body["reasoning_effort"]; has {
		t.Fatalf("no effort member on llama wire: %s", out)
	}
	// none renders kwargs false, no effort member.
	genNone, _, _ := DecodeResponsesRequest([]byte(`{"model":"m","input":"hi","reasoning":{"effort":"none"}}`), DefaultLimits())
	pNone, lNone := PlanEffort(genNone.Reasoning, llamaCaps(t, nil), LocalRoute(capability.RuntimeLlamaCpp))
	if len(lNone) != 0 || pNone.Kind() != EffortOff {
		t.Fatalf("none: %+v %v", pNone, lNone)
	}
	outNone, rerr := EncodeChatRequest(genNone, pNone, "up")
	if rerr != nil {
		t.Fatal(rerr)
	}
	var bodyNone map[string]any
	_ = json.Unmarshal(outNone, &bodyNone)
	kwargs, ok := bodyNone["chat_template_kwargs"].(map[string]any)
	if !ok || kwargs["enable_thinking"] != false {
		t.Fatalf("kwargs wire: %s", outNone)
	}
}

func TestTranslatedStrataNamed(t *testing.T) {
	// Strata reuses ordinary named planning: the admitted literal is
	// preserved, capability-gated, with no llama budget conversion.
	base := capability.Empty("openai-chat")
	capability.ApplyLlamaMapper(&base, capability.RuntimeStrata, 1000)
	caps := capability.ReasoningFor(base)
	intent, _ := NewResponsesEffort("medium")
	planned, losses := PlanEffort(intent, caps, LocalRoute(capability.RuntimeStrata))
	if len(losses) != 0 {
		t.Fatalf("strata admitted literal must be exact: %v", losses)
	}
	out, rerr := EncodeChatRequest(mustGen(t, intent), planned, "up")
	if rerr != nil {
		t.Fatal(rerr)
	}
	var body map[string]any
	_ = json.Unmarshal(out, &body)
	if body["reasoning_effort"] != "medium" {
		t.Fatalf("strata wire must carry literal: %s", out)
	}
	if _, has := body["thinking_budget_tokens"]; has {
		t.Fatalf("strata must not convert budgets: %s", out)
	}
	// Strata none follows the ordinary effort/enum gate (unlike LlamaCpp).
	none, _ := NewResponsesEffort("none")
	pNone, lNone := PlanEffort(none, caps, LocalRoute(capability.RuntimeStrata))
	if len(lNone) != 0 || pNone.Kind() != EffortNamed {
		t.Fatalf("strata none is ordinary: %+v %v", pNone, lNone)
	}
}

func mustGen(t *testing.T, intent ReasoningIntent) *Generation {
	t.Helper()
	g, _, rerr := DecodeResponsesRequest([]byte(`{"model":"m","input":"hi"}`), DefaultLimits())
	if rerr != nil {
		t.Fatal(rerr)
	}
	g.Reasoning = intent
	return g
}

func TestResponsesEncoderRejectsChatNative(t *testing.T) {
	// The Responses encoder admits only named-effort/absence variants;
	// budget/off targets are internal misuse, never silent drops.
	gen, _, _ := DecodeResponsesRequest([]byte(`{"model":"m","input":"hi"}`), DefaultLimits())
	for _, planned := range []PlannedEffort{BudgetEffortTarget(1024), OffEffortTarget()} {
		if _, rerr := EncodeResponsesRequest(gen, planned, "up"); rerr == nil || rerr.Status != 500 {
			t.Fatalf("must be internal misuse: %+v %v", planned, rerr)
		}
	}
	if _, rerr := EncodeResponsesRequest(gen, AbsentEffort(), "up"); rerr != nil {
		t.Fatalf("absent must encode: %v", rerr)
	}
}

func TestTranslatedLlamaEnforcePolicy(t *testing.T) {
	// Strict refuses diagnosed losses before I/O (zero-loss exact maps
	// pass); lossy drops and reports via header encoding.
	caps := llamaCaps(t, nil)
	bad, _ := NewResponsesEffort("ultra")
	_, losses := PlanEffort(bad, caps, LocalRoute(capability.RuntimeLlamaCpp))
	if _, rerr := EnforcePolicy(losses, false); rerr == nil {
		t.Fatalf("strict must refuse diagnosed loss")
	}
	kept, rerr := EnforcePolicy(losses, true)
	if rerr != nil {
		t.Fatalf("lossy must pass: %v", rerr)
	}
	if hdr := EncodeLosses(kept); !strings.Contains(hdr, "reasoning.effort") {
		t.Fatalf("lossy header must name static path: %q", hdr)
	}
	good, _ := NewResponsesEffort("low")
	_, exact := PlanEffort(good, caps, LocalRoute(capability.RuntimeLlamaCpp))
	if len(exact) != 0 {
		t.Fatalf("exact maps emit no loss: %v", exact)
	}
}
