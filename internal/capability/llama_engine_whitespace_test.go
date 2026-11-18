package capability

import "testing"

func TestLocalEnginePythonSeparators(t *testing.T) {
	// Python str.split treats U+001C–U+001F as whitespace. Build reports
	// using these separators must not select llama budgets for Strata.
	for _, sep := range []string{"\x1c", "\x1d", "\x1e", "\x1f", "\u0085", "\u00a0"} {
		for _, token := range []string{"strata", "POLYSTRATA"} {
			build := sep + token + sep + "0.1.45"
			if got := LocalEngine(map[string]any{"build_info": build}); got != RuntimeStrata {
				t.Errorf("separator %U: got %v, want Strata", []rune(sep)[0], got)
			}
		}
	}
}
