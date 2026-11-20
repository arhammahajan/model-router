package adapters

import "testing"

func TestAllAdaptersUnique(t *testing.T) {
	all := All()
	if len(all) != 19 {
		t.Fatalf("expected 19 adapters, got %d", len(all))
	}
	seen := make(map[string]bool, len(all))
	for _, a := range all {
		if a == nil {
			t.Fatal("nil adapter in All()")
		}
		name := a.Agent()
		if name == "" {
			t.Fatal("empty Agent() in All()")
		}
		if seen[name] {
			t.Fatalf("duplicate adapter for agent %q", name)
		}
		seen[name] = true
	}
}
