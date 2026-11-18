package catalog

import "testing"

func TestCompileRejectsLeadingZeroRepeatBounds(t *testing.T) {
	// Python treats these as repetition; Go accepts them as literal text.
	// Both engines compiling a pattern is therefore insufficient admission.
	for _, pattern := range []string{`a{01}`, `a{01,2}`, `a{1,02}`, `a{00}`, `a{000,}`} {
		if _, err := Compile(pattern); err != ErrBraceQuantifier {
			t.Errorf("Compile(%q) = %v, want static brace rejection", pattern, err)
		}
	}
}

func TestCompileCanonicalZeroRepeatBounds(t *testing.T) {
	for _, pattern := range []string{`^a{0}$`, `^a{0,2}$`, `^a{0,}$`} {
		r, err := Compile(pattern)
		if err != nil || !r.MatchString("") {
			t.Errorf("canonical zero bound %q rejected: %v", pattern, err)
		}
	}
}
