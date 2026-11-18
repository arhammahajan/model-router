package capability

// Adversarial F6 review: published values come from the same pinned trusted
// contract, never raw metadata. Hermetic, no I/O.

import (
	"testing"
)

func revCapPut(t *testing.T, c *Contract, path string, v Value) {
	t.Helper()
	if err := c.Put(path, v, nil, KindReviewed); err != nil {
		t.Fatal(err)
	}
}

func TestBearerCompatReviewEffortValuesProjection(t *testing.T) {
	// Unknown (absent key) stays unknown: Set false.
	empty := Empty("openai-chat")
	empty.Normalize()
	got := ReasoningFor(empty)
	if got.EffortValues.Set {
		t.Fatalf("absent must be unknown: %+v", got.EffortValues)
	}
	// Known-empty stays empty (rejects all), distinct from unknown.
	c := Empty("openai-chat")
	emptyVals, ok := StringListValue([]string{})
	if !ok {
		t.Fatal("empty list must be admitted")
	}
	revCapPut(t, &c, "reasoning.effort.values", emptyVals)
	c.Normalize()
	got = ReasoningFor(c)
	if !got.EffortValues.Set || len(got.EffortValues.List) != 0 {
		t.Fatalf("empty must be Set+empty: %+v", got.EffortValues)
	}
	// Literals preserved exactly, no fold/rename.
	c2 := Empty("openai-chat")
	vals, ok := StringListValue([]string{"xhigh", "max", "none", "minimal"})
	if !ok {
		t.Fatal("literals must be admitted")
	}
	revCapPut(t, &c2, "reasoning.effort.values", vals)
	c2.Normalize()
	got = ReasoningFor(c2)
	if !got.EffortValues.Set || len(got.EffortValues.List) != 4 || got.EffortValues.List[0] != "xhigh" {
		t.Fatalf("literals: %+v", got.EffortValues)
	}
	// Clone isolation: caller mutation must not affect contract.
	got.EffortValues.List[0] = "mutated"
	again := ReasoningFor(c2)
	if again.EffortValues.List[0] != "xhigh" {
		t.Fatalf("must clone: %+v", again.EffortValues)
	}
}
