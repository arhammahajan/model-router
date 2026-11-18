package capability

import (
	"testing"
)

// Independent adversarial review for Anthropic Off capability facts.
// Scope: unknown neutral, known-empty vs unknown, supported/toggle/effort
// gates, Normalize suppression, clone isolation. Planner gate order lives in
// protocol; here we prove the facts it consumes are exact.

func offRevPut(t *testing.T, c *Contract, path string, v Value) {
	t.Helper()
	s := "offrev"
	if err := c.Put(path, v, &s, KindOverride); err != nil {
		t.Fatalf("put %s: %v", path, err)
	}
}

func TestOffRevUnknownNeutral(t *testing.T) {
	caps := ReasoningFor(Empty("openai-chat"))
	if caps.Supported != nil || caps.ToggleSupported != nil || caps.EffortSupported != nil {
		t.Fatalf("unknown must be nil: %+v", caps)
	}
	if caps.EffortValues.Set {
		t.Fatalf("unknown values must be unset: %+v", caps.EffortValues)
	}
	if caps.EffortDefault != nil || caps.BudgetSupported != nil {
		t.Fatalf("unknown defaults nil: %+v", caps)
	}
}

func TestOffRevKnownEmptyVsUnknown(t *testing.T) {
	c := Empty("openai-chat")
	empty, ok := StringListValue([]string{})
	if !ok {
		t.Fatalf("empty list must be admitted")
	}
	offRevPut(t, &c, "reasoning.effort.values", empty)
	caps := ReasoningFor(c)
	if !caps.EffortValues.Set || len(caps.EffortValues.List) != 0 {
		t.Fatalf("known-empty must be Set empty: %+v", caps.EffortValues)
	}
	if _, ok := StringListValue([]string{"low", "low"}); ok {
		t.Fatalf("duplicate values must be rejected")
	}
	if _, ok := StringListValue([]string{""}); ok {
		t.Fatalf("empty element must be rejected")
	}
	if _, ok := StringListValue([]string{"low", ""}); ok {
		t.Fatalf("empty element among values must be rejected")
	}
}

func TestOffRevFalsePreserved(t *testing.T) {
	c := Empty("openai-chat")
	offRevPut(t, &c, "reasoning.supported", BoolValue(false))
	offRevPut(t, &c, "reasoning.toggle.supported", BoolValue(false))
	offRevPut(t, &c, "reasoning.effort.supported", BoolValue(false))
	caps := ReasoningFor(c)
	if caps.Supported == nil || *caps.Supported {
		t.Fatalf("supported false: %+v", caps.Supported)
	}
	if caps.ToggleSupported == nil || *caps.ToggleSupported {
		t.Fatalf("toggle false: %+v", caps.ToggleSupported)
	}
	if caps.EffortSupported == nil || *caps.EffortSupported {
		t.Fatalf("effort false: %+v", caps.EffortSupported)
	}
}

func TestOffRevNormalizeSupportedFalse(t *testing.T) {
	c := Empty("openai-chat")
	offRevPut(t, &c, "reasoning.supported", BoolValue(false))
	offRevPut(t, &c, "reasoning.toggle.supported", BoolValue(true))
	vals, _ := StringListValue([]string{"low", "none"})
	offRevPut(t, &c, "reasoning.effort.values", vals)
	c.Normalize()
	caps := ReasoningFor(c)
	if caps.Supported == nil || *caps.Supported {
		t.Fatalf("supported stays false: %+v", caps.Supported)
	}
	if caps.ToggleSupported == nil || *caps.ToggleSupported {
		t.Fatalf("Normalize must suppress toggle positive under supported=false: %+v", caps.ToggleSupported)
	}
	if caps.EffortValues.Set {
		t.Fatalf("Normalize must withdraw values under supported=false: %+v", caps.EffortValues)
	}
}

func TestOffRevNormalizeToggleFalseKeepsEffort(t *testing.T) {
	c := Empty("openai-chat")
	offRevPut(t, &c, "reasoning.toggle.supported", BoolValue(false))
	vals, _ := StringListValue([]string{"low", "none"})
	offRevPut(t, &c, "reasoning.effort.values", vals)
	c.Normalize()
	caps := ReasoningFor(c)
	if caps.ToggleSupported == nil || *caps.ToggleSupported {
		t.Fatalf("toggle false preserved")
	}
	if !caps.EffortValues.Set || len(caps.EffortValues.List) != 2 {
		t.Fatalf("toggle=false must not withdraw effort values: %+v", caps.EffortValues)
	}
}

func TestOffRevProjectionCloneIsolation(t *testing.T) {
	c := Empty("openai-chat")
	vals, _ := StringListValue([]string{"low", "none"})
	offRevPut(t, &c, "reasoning.effort.values", vals)
	caps := ReasoningFor(c)
	caps.EffortValues.List[0] = "MUT"
	caps2 := ReasoningFor(c)
	if caps2.EffortValues.List[0] != "low" {
		t.Fatalf("projection must clone: %+v", caps2.EffortValues)
	}
	mut, _ := StringListValue([]string{"MUT"})
	offRevPut(t, &c, "reasoning.effort.values", mut)
	if caps2.EffortValues.List[0] != "low" {
		t.Fatalf("earlier projection must not observe later put: %+v", caps2.EffortValues)
	}
}

func TestOffRevEffortValuesContent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values []string
		set    bool
	}{
		{"unknown", nil, false},
		{"empty", []string{}, true},
		{"withNone", []string{"low", "none"}, true},
		{"withoutNone", []string{"low", "medium"}, true},
	} {
		c := Empty("openai-chat")
		if tc.values != nil {
			v, ok := StringListValue(tc.values)
			if !ok {
				t.Fatalf("%s: StringListValue rejected", tc.name)
			}
			offRevPut(t, &c, "reasoning.effort.values", v)
		}
		caps := ReasoningFor(c)
		if caps.EffortValues.Set != tc.set {
			t.Fatalf("%s: Set=%v want %v", tc.name, caps.EffortValues.Set, tc.set)
		}
	}
}
