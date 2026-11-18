// Independent review: mapper fact ownership boundaries not already pinned.
// Hermetic stdlib only. Stable source: git show HEAD:router_metadata/__init__.py
// (mapper facts, no retrieved_at) and HEAD:model_router/constants.py (budgets).
package capability

import (
	"testing"
)

func TestAuditMapperDoubleApplyIdempotent(t *testing.T) {
	base := Empty("openai-chat")
	ApplyLlamaMapper(&base, RuntimeLlamaCpp, 1000)
	first := mustJSON(t, Render(base, nil, StaticDiscovery(), 1000))
	ApplyLlamaMapper(&base, RuntimeLlamaCpp, 2000)
	second := mustJSON(t, Render(base, nil, StaticDiscovery(), 2000))
	eff1 := leaf(t, first, "reasoning.effort.supported")
	eff2 := leaf(t, second, "reasoning.effort.supported")
	if eff1["value"] != true || eff2["value"] != true {
		t.Fatalf("double apply must stay true: %v %v", eff1, eff2)
	}
	// No duplicate conflict accumulation: second apply with identical values
	// must not append new conflict evidence.
	c1, _ := eff1["conflicts"].([]any)
	c2, _ := eff2["conflicts"].([]any)
	if len(c1) != len(c2) {
		t.Fatalf("idempotent apply must not grow conflicts: %d vs %d", len(c1), len(c2))
	}
}

func TestAuditMapperOperatorEmptyAuthoritative(t *testing.T) {
	base := Empty("openai-chat")
	ApplyLlamaMapper(&base, RuntimeLlamaCpp, 1000)
	empty, ok := StringListValue([]string{})
	if !ok {
		t.Fatalf("empty list must validate")
	}
	src := "override.reasoning.effort.values"
	base.put("reasoning.effort.values", empty, &src, KindOverride)
	base.Normalize()
	w := mustJSON(t, base)
	vals := leaf(t, w, "reasoning.effort.values")
	list, ok := vals["value"].([]any)
	if !ok || len(list) != 0 {
		t.Fatalf("operator empty must win over mapper values: %v", vals)
	}
	if vals["kind"] != "override" {
		t.Fatalf("empty kind must stay override: %v", vals)
	}
	// Known-empty must drop every level through planning projection.
	caps := ReasoningFor(base)
	if !caps.EffortValues.Set || len(caps.EffortValues.List) != 0 {
		t.Fatalf("known-empty must stay set: %+v", caps.EffortValues)
	}
}

func TestAuditMapperInvalidRuntimeNoFacts(t *testing.T) {
	base := Empty("openai-chat")
	ApplyLlamaMapper(&base, LocalRuntime(77), 1000)
	w := mustJSON(t, Render(base, nil, StaticDiscovery(), 1000))
	if got := leaf(t, w, "reasoning.effort.supported")["value"]; got != nil {
		t.Fatalf("invalid runtime must add no facts: %v", got)
	}
	srcs := w["sources"].(map[string]any)
	if _, has := srcs["router.llama_thinking_map"]; has {
		t.Fatalf("invalid runtime must publish no claims")
	}
	if _, has := srcs["strata.compat"]; has {
		t.Fatalf("invalid runtime must publish no strata claims")
	}
}

func TestAuditMapperLeavesLimitsUnknown(t *testing.T) {
	for _, rt := range []LocalRuntime{RuntimeLlamaCpp, RuntimeStrata} {
		base := Empty("openai-chat")
		ApplyLlamaMapper(&base, rt, 1000)
		w := mustJSON(t, Render(base, nil, StaticDiscovery(), 1000))
		for _, path := range []string{
			"reasoning.budget.supported", "reasoning.budget.min_tokens",
			"reasoning.budget.max_tokens", "reasoning.budget.default_tokens",
			"reasoning.effort.default",
		} {
			if got := leaf(t, w, path)["value"]; got != nil {
				t.Fatalf("rt %v %s must stay unknown: %v", rt, path, got)
			}
		}
	}
}

func TestAuditMapperBudgetConsistentWithTable(t *testing.T) {
	// The typed literal mapper must agree with the single owned budget table
	// for every recognized level; unknown names stay Absent.
	for _, lvl := range []string{"minimal", "low", "medium", "high", "xhigh"} {
		m, ok := MapLlamaEffort(lvl)
		if !ok || m.Kind != LlamaMapBudget {
			t.Fatalf("%s must be budget: %+v %v", lvl, m, ok)
		}
		b, ok := LlamaBudget(lvl)
		if !ok || b != m.Budget {
			t.Fatalf("%s table/mapper disagree: %d vs %d", lvl, b, m.Budget)
		}
	}
	for _, unk := range []string{"max", "ultra", "none ", " low"} {
		m, ok := MapLlamaEffort(unk)
		if unk == "none " || unk == " low" {
			// Whitespace-padded literals are unknown names, never trimmed.
			if ok || m.Kind != LlamaMapAbsent {
				t.Fatalf("%q must be absent: %+v %v", unk, m, ok)
			}
			continue
		}
		if ok || m.Kind != LlamaMapAbsent {
			t.Fatalf("%q must be absent: %+v %v", unk, m, ok)
		}
	}
}
