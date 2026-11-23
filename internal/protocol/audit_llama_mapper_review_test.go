// Independent review: native applier edge boundaries beyond the pinned matrix.
// Hermetic stdlib only. Stable source: git show HEAD:model_router/common.py
// (_apply_llama_thinking setdefault/explicit-wins semantics).
package protocol

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/arhammahajan/router/internal/capability"
)

func auditNative(t *testing.T, body string) map[string]any {
	t.Helper()
	out, consumed := ApplyLlamaNative([]byte(body))
	if !consumed {
		t.Fatalf("expected consumption: %s", body)
	}
	var m map[string]any
	dec := json.NewDecoder(strings.NewReader(string(out)))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return m
}

func TestAuditNativeNonePreservesBudget(t *testing.T) {
	// none touches only kwargs; an existing budget member (even null) stays.
	m := auditNative(t, `{"model":"m","reasoning_effort":"none","thinking_budget_tokens":999}`)
	raw, _ := json.Marshal(m["thinking_budget_tokens"])
	if string(raw) != "999" {
		t.Fatalf("none must preserve budget: %v", m)
	}
	m = auditNative(t, `{"model":"m","reasoning_effort":"none","thinking_budget_tokens":null}`)
	if m["thinking_budget_tokens"] != nil {
		t.Fatalf("none must preserve null budget: %v", m)
	}
}

func TestAuditNativeBudgetLeavesKwargsUntouched(t *testing.T) {
	// Mapped levels default only the budget member; kwargs pass through
	// verbatim (including explicit null and objects).
	m := auditNative(t, `{"model":"m","reasoning_effort":"low","chat_template_kwargs":{"enable_thinking":true}}`)
	kwargs := m["chat_template_kwargs"].(map[string]any)
	if kwargs["enable_thinking"] != true {
		t.Fatalf("budget must not touch kwargs: %v", m)
	}
	m = auditNative(t, `{"model":"m","reasoning_effort":"low","chat_template_kwargs":null}`)
	if m["chat_template_kwargs"] != nil {
		t.Fatalf("budget must preserve null kwargs: %v", m)
	}
}

func TestAuditNativeFalseZeroKwargsStay(t *testing.T) {
	// Boolean false / zero / empty-string kwargs are non-object/non-null and
	// must remain exactly as sent (only null/absent become the default).
	for _, kv := range []string{`false`, `0`, `""`, `{}`} {
		body := `{"model":"m","reasoning_effort":"none","chat_template_kwargs":` + kv + `}`
		out, consumed := ApplyLlamaNative([]byte(body))
		if !consumed {
			t.Fatalf("expected consumption: %s", body)
		}
		var m map[string]any
		dec := json.NewDecoder(strings.NewReader(string(out)))
		dec.UseNumber()
		if err := dec.Decode(&m); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(m["chat_template_kwargs"])
		want := kv
		if kv == `{}` {
			// Empty object gains the default false member.
			if string(raw) != `{"enable_thinking":false}` {
				t.Fatalf("empty object must gain false: %s", raw)
			}
			continue
		}
		_ = want
		if string(raw) != kv {
			t.Fatalf("kwargs %s must stay, got %s", kv, raw)
		}
	}
}

func TestAuditNativePaddedLevelUnknown(t *testing.T) {
	// Literals are never trimmed: padded levels are unknown and consumed
	// with no control.
	for _, body := range []string{
		`{"model":"m","reasoning_effort":" low"}`,
		`{"model":"m","reasoning_effort":"low "}`,
		`{"model":"m","reasoning_effort":"None"}`,
	} {
		m := auditNative(t, body)
		if _, has := m["thinking_budget_tokens"]; has {
			t.Fatalf("padded must add no budget: %s -> %v", body, m)
		}
		if _, has := m["chat_template_kwargs"]; has {
			t.Fatalf("padded must add no kwargs: %s -> %v", body, m)
		}
	}
}

func TestAuditTranslatedScopeSelectsTarget(t *testing.T) {
	// Same literal, unknown caps: ordinary emits the literal while
	// LlamaCpp emits the exact budget. Strata reuses the ordinary wire.
	ordCaps := capability.ReasoningFor(capability.Empty("openai-chat"))
	low, _ := NewResponsesEffort("low")
	pOrd, lOrd := PlanEffort(low, ordCaps, OrdinaryRoute())
	if len(lOrd) != 0 {
		t.Fatalf("ordinary unknown must be exact: %v", lOrd)
	}
	if eff, ok := pOrd.Effort(); !ok || eff != "low" {
		t.Fatalf("ordinary must emit literal: %+v", pOrd)
	}
	llamaCaps := capability.ReasoningFor(capability.Empty("openai-chat"))
	pLlama, lLlama := PlanEffort(low, llamaCaps, LocalRoute(capability.RuntimeLlamaCpp))
	if len(lLlama) != 0 {
		t.Fatalf("llama mapped must be exact: %v", lLlama)
	}
	if b, ok := pLlama.Budget(); !ok || b != 2048 {
		t.Fatalf("llama must emit budget: %+v", pLlama)
	}
}
