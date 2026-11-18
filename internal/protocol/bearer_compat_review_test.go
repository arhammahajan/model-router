package protocol

// Adversarial F6 review: compat origin, literal preservation, native map,
// xhigh/max refusal, all four destinations + local runtime, value-free
// strict/lossy. Hermetic, no I/O.

import (
	"strings"
	"testing"

	"github.com/arhammahajan/model-router/internal/capability"
)

func revCaps(vals ...string) capability.ReasoningCapabilities {
	return capability.ReasoningCapabilities{EffortValues: capability.OptionalStringList{Set: true, List: vals}}
}

func TestBearerCompatReviewOriginLiteral(t *testing.T) {
	for _, lvl := range []string{"xhigh", "max", "minimal", "low"} {
		in, ok := NewCompatEffort(lvl)
		if !ok {
			t.Fatalf("%s must be accepted", lvl)
		}
		if got, _ := in.Level(); got != lvl {
			t.Fatalf("literal must be preserved: %q", got)
		}
		if in.Origin() != "x-model-router-reasoning-effort" {
			t.Fatalf("origin: %q", in.Origin())
		}
		if _, ok := NewGeminiLevel(lvl); ok && (lvl == "xhigh" || lvl == "max") {
			t.Fatalf("GeminiLevel must never accept %s", lvl)
		}
	}
	if _, ok := NewCompatEffort(""); ok {
		t.Fatalf("empty must be rejected")
	}
}

func TestBearerCompatReviewBridgedDestinations(t *testing.T) {
	// Chat ordinary keeps literal, no loss.
	in, _ := NewCompatEffort("xhigh")
	planned, losses := PlanEffort(in, revCaps("xhigh", "low"), OrdinaryRoute())
	if len(losses) != 0 {
		t.Fatalf("exact chat must carry no loss: %+v", losses)
	}
	if e, _ := planned.Effort(); e != "xhigh" {
		t.Fatalf("chat literal: %+v", planned)
	}
	// Responses keeps literal even for budget-like names.
	planned, losses = PlanEffort(in, revCaps("xhigh"), OrdinaryResponsesRoute())
	if len(losses) != 0 {
		t.Fatalf("responses exact: %+v", losses)
	}
	if e, _ := planned.Effort(); e != "xhigh" {
		t.Fatalf("responses literal: %+v", planned)
	}
	// Off on Chat ordinary with none published stays Named none.
	off, _ := NewCompatEffort("none")
	planned, losses = PlanEffort(off, revCaps("none", "low"), OrdinaryRoute())
	if len(losses) != 0 {
		t.Fatalf("off exact: %+v", losses)
	}
	if e, _ := planned.Effort(); e != "none" {
		t.Fatalf("off literal: %+v", planned)
	}
	// Off without none published drops value-free (static reason names 'none',
	// the closed Off level, never an arbitrary carrier echo).
	planned, losses = PlanEffort(off, revCaps("low"), OrdinaryRoute())
	if planned.IsPresent() || len(losses) != 1 {
		t.Fatalf("off undisplayable must drop: %+v %+v", planned, losses)
	}
	if losses[0].Field != "x-model-router-reasoning-effort" || losses[0].Action != "dropped" {
		t.Fatalf("loss origin/action: %+v", losses[0])
	}
	// Explicit budget via compat? Compat never carries budgets (only levels),
	// but planner must not invent one: Named xhigh on Llama maps to drop, not budget.
	llamaCaps := capability.ReasoningCapabilities{EffortValues: capability.OptionalStringList{Set: true, List: []string{"low"}}}
	planned, losses = PlanEffort(in, llamaCaps, LocalRoute(capability.RuntimeLlamaCpp))
	if planned.IsPresent() {
		t.Fatalf("unmapped llama must not emit: %+v", planned)
	}
	if len(losses) != 1 || losses[0].Field != "x-model-router-reasoning-effort" {
		t.Fatalf("llama loss origin: %+v", losses)
	}
	// Strata local keeps literal (ordinary behavior).
	planned, losses = PlanEffort(in, revCaps("xhigh"), LocalRoute(capability.RuntimeStrata))
	if len(losses) != 0 {
		t.Fatalf("strata must keep literal: %+v %+v", planned, losses)
	}
	// Unknown runtime drops with diagnosis, strict refuses, lossy continues.
	planned, losses = PlanEffort(in, capability.ReasoningCapabilities{}, LocalRoute(capability.RuntimeUnobserved))
	if planned.IsPresent() || len(losses) != 1 {
		t.Fatalf("unobserved must drop: %+v %+v", planned, losses)
	}
	if _, rerr := EnforcePolicy(losses, false); rerr == nil {
		t.Fatalf("strict must 400")
	}
	kept, rerr := EnforcePolicy(losses, true)
	if rerr != nil || len(kept) != 1 || kept[0].Action != "dropped" {
		t.Fatalf("lossy must keep header: %+v %+v", kept, rerr)
	}
}

func TestBearerCompatReviewAnthropicTerminal(t *testing.T) {
	in, _ := NewCompatEffort("max")
	planned, losses := PlanEffort(in, capability.ReasoningCapabilities{}, CompatAnthropicRoute())
	if planned.IsPresent() {
		t.Fatalf("anthropic must emit nothing")
	}
	if len(losses) != 1 || losses[0].Field != "x-model-router-reasoning-effort" || losses[0].Action != "dropped" {
		t.Fatalf("terminal loss: %+v", losses)
	}
	if strings.Contains(losses[0].Reason, "max") {
		t.Fatalf("value-free: %+v", losses[0])
	}
	if _, rerr := EnforcePolicy(losses, false); rerr == nil || rerr.Code != CodeTranslationLoss {
		t.Fatalf("strict 400 translation_loss: %+v", rerr)
	}
	kept, rerr := EnforcePolicy(losses, true)
	if rerr != nil || len(kept) != 1 {
		t.Fatalf("lossy continue: %+v %+v", kept, rerr)
	}
	// Absent stays absent.
	planned, losses = PlanEffort(AbsentReasoning(), capability.ReasoningCapabilities{}, CompatAnthropicRoute())
	if planned.IsPresent() || len(losses) != 0 {
		t.Fatalf("absent: %+v %+v", planned, losses)
	}
}
