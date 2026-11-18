package capability

import (
	"testing"
)

// Independent review of the Chat->Responses reasoning vertical projection.
// Stable source: git show HEAD:model_router/reasoning.py + router_metadata.
// Only proves the first-unit gates: explicit false vs unknown neutral,
// known-empty vs unknown, literal preservation, and clone ownership.
// No Python working tree, no network, no env.

func rrBool(v bool) *bool    { b := v; return &b }
func rrStr(s string) *string { v := s; return &v }

func rrPut(t *testing.T, c *Contract, path string, v Value) {
	t.Helper()
	if err := c.Put(path, v, rrStr("review-test"), KindOverride); err != nil {
		t.Fatalf("put %s: %v", path, err)
	}
}

func TestRRUnknownNeutralAllNil(t *testing.T) {
	caps := ReasoningFor(Empty("openai-responses"))
	if caps.Supported != nil || caps.ToggleSupported != nil || caps.EffortSupported != nil || caps.BudgetSupported != nil {
		t.Fatalf("unknown must stay nil: %+v", caps)
	}
	if caps.EffortValues.Set {
		t.Fatalf("unknown values must be unset: %+v", caps.EffortValues)
	}
	if caps.EffortDefault != nil || caps.EffortNative != nil || caps.ToggleNative != nil || caps.BudgetNative != nil {
		t.Fatalf("unknown natives must be nil: %+v", caps)
	}
	if caps.BudgetMin != nil || caps.BudgetMax != nil || caps.BudgetDefault != nil {
		t.Fatalf("unknown budgets must be nil: %+v", caps)
	}
}

func TestRRFalseVsUnknownPreserved(t *testing.T) {
	c := Empty("openai-responses")
	rrPut(t, &c, "reasoning.supported", BoolValue(false))
	rrPut(t, &c, "reasoning.toggle.supported", BoolValue(false))
	rrPut(t, &c, "reasoning.effort.supported", BoolValue(false))
	rrPut(t, &c, "reasoning.budget.supported", BoolValue(false))
	c.Normalize()
	caps := ReasoningFor(c)
	for name, got := range map[string]*bool{
		"supported": caps.Supported, "toggle": caps.ToggleSupported,
		"effort": caps.EffortSupported, "budget": caps.BudgetSupported,
	} {
		if got == nil || *got != false {
			t.Fatalf("%s must be explicit false: %+v", name, caps)
		}
	}
	// True stays true.
	c2 := Empty("openai-responses")
	rrPut(t, &c2, "reasoning.supported", BoolValue(true))
	c2.Normalize()
	if got := ReasoningFor(c2).Supported; got == nil || *got != true {
		t.Fatalf("true must stay true: %+v", got)
	}
}

func TestRRKnownEmptyVsUnknown(t *testing.T) {
	emptyVals, ok := StringListValue([]string{})
	if !ok {
		t.Fatal("empty list must be admitted")
	}
	c := Empty("openai-responses")
	rrPut(t, &c, "reasoning.effort.values", emptyVals)
	c.Normalize()
	caps := ReasoningFor(c)
	if !caps.EffortValues.Set || len(caps.EffortValues.List) != 0 {
		t.Fatalf("known-empty must stay set-empty: %+v", caps.EffortValues)
	}
	unknown := ReasoningFor(Empty("openai-responses"))
	if unknown.EffortValues.Set {
		t.Fatalf("unknown must stay unset")
	}
	// Known values preserved literally.
	vals, ok := StringListValue([]string{"low", "medium"})
	if !ok {
		t.Fatal("bad test values")
	}
	c3 := Empty("openai-responses")
	rrPut(t, &c3, "reasoning.effort.values", vals)
	rrPut(t, &c3, "reasoning.effort.default", StringValue("low"))
	c3.Normalize()
	got := ReasoningFor(c3)
	if !got.EffortValues.Set || len(got.EffortValues.List) != 2 || got.EffortValues.List[0] != "low" {
		t.Fatalf("values must be preserved: %+v", got.EffortValues)
	}
	if got.EffortDefault == nil || *got.EffortDefault != "low" {
		t.Fatalf("default must be preserved: %+v", got.EffortDefault)
	}
}

func TestRRProjectionClonesOwnership(t *testing.T) {
	c := Empty("openai-responses")
	vals, _ := StringListValue([]string{"low"})
	rrPut(t, &c, "reasoning.effort.values", vals)
	np, ok := NewNativeParam("openai-responses", "reasoning.effort", map[string]NativeScalar{"a": StringScalar("b")})
	if !ok {
		t.Fatal("bad native")
	}
	rrPut(t, &c, "reasoning.effort.native_parameter", NativeValue(np))
	rrPut(t, &c, "reasoning.effort.default", StringValue("low"))
	if err := c.Put("reasoning.budget.min_tokens", IntValue(5), rrStr("review-test"), KindOverride); err != nil {
		t.Fatal(err)
	}
	c.Normalize()

	got := ReasoningFor(c)
	// Mutate every mutable projection surface.
	got.EffortValues.List[0] = "MUT"
	if got.EffortDefault != nil {
		*got.EffortDefault = "MUT"
	}
	if got.EffortNative != nil {
		got.EffortNative.Values()["a"] = StringScalar("MUT")
		// Values() returns a copy; also mutate via re-put path is impossible
		// (private fields). Verify contract still clean below.
	}
	if got.BudgetMin != nil {
		*got.BudgetMin = 999
	}
	again := ReasoningFor(c)
	if len(again.EffortValues.List) != 1 || again.EffortValues.List[0] != "low" {
		t.Fatalf("contract list must be unaffected: %+v", again.EffortValues)
	}
	if again.EffortDefault == nil || *again.EffortDefault != "low" {
		t.Fatalf("contract default must be unaffected: %+v", again.EffortDefault)
	}
	if again.BudgetMin == nil || *again.BudgetMin != 5 {
		t.Fatalf("contract budget must be unaffected: %+v", again.BudgetMin)
	}
	if again.EffortNative == nil || again.EffortNative.Path() != "reasoning.effort" {
		t.Fatalf("contract native must be unaffected: %+v", again.EffortNative)
	}

	// Mutating the contract after projection must not affect the projection.
	c2 := Empty("openai-responses")
	rrPut(t, &c2, "reasoning.effort.values", vals)
	c2.Normalize()
	snap := ReasoningFor(c2)
	vals2, _ := StringListValue([]string{"MUTATED"})
	_ = c2.Put("reasoning.effort.values", vals2, rrStr("review-test"), KindOverride)
	if len(snap.EffortValues.List) != 1 || snap.EffortValues.List[0] != "low" {
		t.Fatalf("earlier projection must be frozen: %+v", snap.EffortValues)
	}
}

func TestRRNativeValuesMapIsolation(t *testing.T) {
	c := Empty("openai-responses")
	np, ok := NewNativeParam("openai-chat", "reasoning_effort", map[string]NativeScalar{"off": StringScalar("none")})
	if !ok {
		t.Fatal("bad native")
	}
	rrPut(t, &c, "reasoning.toggle.native_parameter", NativeValue(np))
	c.Normalize()
	got := ReasoningFor(c)
	cp := got.ToggleNative.Values()
	cp["off"] = StringScalar("MUT")
	again := ReasoningFor(c)
	m := again.ToggleNative.Values()
	if m["off"] != (NativeScalar{}) && func() bool { s, _ := m["off"].String(); return s != "none" }() {
		t.Fatalf("native map must be cloned: %v", m)
	}
}
