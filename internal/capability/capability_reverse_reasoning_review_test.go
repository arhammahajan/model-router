package capability

import (
	"testing"
)

// Independent reverse review: ReasoningFor projection for the Responses->Chat
// named-effort slice. Stable source: HEAD router_metadata/__init__.py
// (empty/overrides/normalize) + reasoning.py fact gates. Hermetic, no env/net.
func rvrCapStr(s string) *string { v := s; return &v }

func rvrCapPut(t *testing.T, c *Contract, path string, v Value) {
	t.Helper()
	if err := c.Put(path, v, rvrCapStr("reverse-review"), KindOverride); err != nil {
		t.Fatalf("put %s: %v", path, err)
	}
}

func TestRvrCapUnknownNeutralChat(t *testing.T) {
	caps := ReasoningFor(Empty("openai-chat"))
	if caps.Supported != nil || caps.ToggleSupported != nil || caps.EffortSupported != nil {
		t.Fatalf("unknown must stay nil: %+v", caps)
	}
	if caps.EffortValues.Set {
		t.Fatalf("unknown values must be unset")
	}
}

func TestRvrCapFalsePreservedChat(t *testing.T) {
	c := Empty("openai-chat")
	rvrCapPut(t, &c, "reasoning.supported", BoolValue(false))
	rvrCapPut(t, &c, "reasoning.toggle.supported", BoolValue(false))
	rvrCapPut(t, &c, "reasoning.effort.supported", BoolValue(false))
	c.Normalize()
	caps := ReasoningFor(c)
	if caps.Supported == nil || *caps.Supported != false {
		t.Fatalf("supported false lost: %+v", caps.Supported)
	}
	if caps.ToggleSupported == nil || *caps.ToggleSupported != false {
		t.Fatalf("toggle false lost")
	}
	if caps.EffortSupported == nil || *caps.EffortSupported != false {
		t.Fatalf("effort false lost")
	}
}

func TestRvrCapKnownEmptyVsUnknownChat(t *testing.T) {
	emptyVals, ok := StringListValue([]string{})
	if !ok {
		t.Fatal("empty list must be admitted")
	}
	c := Empty("openai-chat")
	rvrCapPut(t, &c, "reasoning.effort.values", emptyVals)
	c.Normalize()
	got := ReasoningFor(c)
	if !got.EffortValues.Set || len(got.EffortValues.List) != 0 {
		t.Fatalf("known-empty must stay set-empty: %+v", got.EffortValues)
	}
	if unknown := ReasoningFor(Empty("openai-chat")); unknown.EffortValues.Set {
		t.Fatalf("unknown must stay unset")
	}
}

func TestRvrCapProjectionCloneIsolation(t *testing.T) {
	c := Empty("openai-chat")
	vals, _ := StringListValue([]string{"low", "medium"})
	rvrCapPut(t, &c, "reasoning.effort.values", vals)
	c.Normalize()
	got := ReasoningFor(c)
	got.EffortValues.List[0] = "MUT"
	again := ReasoningFor(c)
	if again.EffortValues.List[0] != "low" {
		t.Fatalf("contract list must be unaffected: %+v", again.EffortValues)
	}
}
