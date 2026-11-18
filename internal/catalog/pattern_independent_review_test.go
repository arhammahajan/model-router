// Independent adversarial review of the portable catalog pattern gate.
//
// Scope: offline cross-language probes (stdlib Python re 3.14.10 vs Go
// regexp, no Python runtime in this suite) over a bounded generated
// pattern/input cross-product. Claim is boolean-agreement only
// (MatchString == pattern.search hit/miss) on exact Python-stripped
// Unicode catalog IDs under search semantics, plus leftmost-match
// spans where pinned. This is what was checked, not an exhaustive
// proof: new constructs need new probes before admission.
//
// Out of scope by contract, not re-reported here:
//   - a{01}/a{01,2}/a{1,02}/a{00}/a{000,} leading-zero drift (root
//     found, fixed by ErrBraceQuantifier rejection).
//   - foo$/^$ on unstripped trailing-newline inputs (Python matches
//     before \n, Go matches only at end). IDs are stripped exactly
//     as Python str.strip does before matching, so the diverging
//     input cannot reach the matcher. These tests pin stripped
//     inputs only.
//   - Conservative false positives (e.g. (?P<n>...) same-match but
//     rejected, bare-literal braces needing \{): allowed and
//     documented.
//   - Invalid-UTF8 patterns/inputs: JSON catalog names are valid
//     Unicode (boundary worker owns); the gate must only avoid
//     panicking and must stay value-free, which is pinned below.
//   - Empty-pattern rejection text: Config_as_str rejects empty;
//     the helper's ErrEmptyPattern is the gate spelling.
package catalog

import (
	"strings"
	"testing"
)

// Bracket literals, negation, ranges, and bare closers. Every entry
// was executed on both engines; booleans agreed.
func TestIndependentReviewBrackets(t *testing.T) {
	cases := []struct {
		pattern string
		input   string
		want    bool
		find    string
	}{
		{`[]a]`, "a", true, "a"},
		{`[]a]`, "]", true, "]"},
		{`[-a]`, "-", true, "-"},
		{`[-a]`, "a", true, "a"},
		{`[a-]`, "-", true, "-"},
		{`[a\-z]`, "b", false, ""},
		{`[a\-z]`, "-", true, "-"},
		{`[a-z]`, "m", true, "m"},
		{`[a-z]`, "-", false, ""},
		{`[--0]`, ".", true, "."},
		{`[--0]`, "/", true, "/"},
		{`[--0]`, "0", true, "0"},
		{`[--0]`, "a", false, ""},
		{`[.]`, ".", true, "."},
		{`[\.]`, ".", true, "."},
		{`[.]`, "a", false, ""},
		{`[ -~]+`, "hello!", true, "hello!"},
		{`[ -~]+`, "caf", true, "caf"},
		{`[^a]`, "b", true, "b"},
		{`[^a]`, "a", false, ""},
		{`[^/]+/[^/]+`, "a/b", true, "a/b"},
		{`[^/]+/[^/]+`, "ab", false, ""},
		{`[\t]`, "\t", true, "\t"},
		{`[\x41]`, "A", true, "A"},
		{`[a/b]`, "/", true, "/"},
		// Bare closers are literals on both sides.
		{`}`, "}", true, "}"},
		{`}`, "a", false, ""},
		{`]*`, "a", true, ""},
		{`}*`, "a", true, ""},
		{`()*`, "a", true, ""},
		{`(?:)*`, "a", true, ""},
		// Empty alternatives match empty on both sides.
		{`a||b`, "x", true, ""},
		{`a||b`, "a", true, "a"},
		{`(|a)`, "x", true, ""},
		{`(?:|)`, "x", true, ""},
	}
	for _, tc := range cases {
		re, err := Compile(tc.pattern)
		if err != nil {
			t.Fatalf("Compile(%q) = %v, want admit", tc.pattern, err)
		}
		if got := re.MatchString(tc.input); got != tc.want {
			t.Errorf("Compile(%q).MatchString(%q) = %v, want %v (both engines agree per probe)", tc.pattern, tc.input, got, tc.want)
		}
		if got := re.FindString(tc.input); got != tc.find {
			t.Errorf("Compile(%q).FindString(%q) = %q, want %q", tc.pattern, tc.input, got, tc.find)
		}
	}
}

// Both-engines-reject bracket forms stay rejected via the static
// backstop (never echoing the pattern).
func TestIndependentReviewBracketsReject(t *testing.T) {
	for _, pattern := range []string{`[]`, `[^]`, `[z-a]`, `[abc`, `(a`, `a)`} {
		if _, err := Compile(pattern); err != ErrInvalidPattern {
			t.Errorf("Compile(%q) = %v, want ErrInvalidPattern", pattern, err)
		}
	}
}

// Counted bounds, lazy quantifiers, alternation order, greedy spans.
// Spans below are the observed leftmost match on both engines.
func TestIndependentReviewQuantifiers(t *testing.T) {
	cases := []struct {
		pattern string
		input   string
		want    bool
		find    string
	}{
		{`a{0}`, "", true, ""},
		{`a{0,0}`, "", true, ""},
		{`a{0,1}`, "", true, ""},
		{`a{2,2}`, "aa", true, "aa"},
		{`a{0,}`, "", true, ""},
		{`a{0,2}`, "", true, ""},
		{`a{1,10}`, "aaa", true, "aaa"},
		{`a+?`, "aaa", true, "a"},
		{`a{2,3}?`, "aaa", true, "aa"},
		{`(?:a|ab)`, "ab", true, "a"},
		{`(?:ab|a)`, "ab", true, "ab"},
		{`a.*b`, "axbxb", true, "axbxb"},
		{`a.*b`, "aaa", false, ""},
		{`(?:a*)*`, "aaa", true, "aaa"},
		{`(a|ab)*`, "ab", true, "a"},
	}
	for _, tc := range cases {
		re, err := Compile(tc.pattern)
		if err != nil {
			t.Fatalf("Compile(%q) = %v, want admit", tc.pattern, err)
		}
		if got := re.MatchString(tc.input); got != tc.want {
			t.Errorf("Compile(%q).MatchString(%q) = %v, want %v", tc.pattern, tc.input, got, tc.want)
		}
		if tc.want {
			if got := re.FindString(tc.input); got != tc.find {
				t.Errorf("Compile(%q).FindString(%q) = %q, want %q", tc.pattern, tc.input, got, tc.find)
			}
		}
	}
	// a{100} needs a 100-long run on both sides.
	re, err := Compile(`a{100}`)
	if err != nil {
		t.Fatalf("Compile(a{100}) = %v, want admit", err)
	}
	if !re.MatchString(strings.Repeat("a", 100)) {
		t.Errorf("a{100} should match 100 a's on both engines")
	}
	if re.MatchString(strings.Repeat("a", 99)) {
		t.Errorf("a{100} should miss 99 a's on both engines")
	}
}

// Escapes, control characters, literal Unicode. All probed identical.
func TestIndependentReviewEscapesUnicode(t *testing.T) {
	cases := []struct {
		pattern string
		input   string
		want    bool
		find    string
	}{
		{`\x00`, "\x00", true, "\x00"},
		{`\x41`, "A", true, "A"},
		{`\x41A`, "AA", true, "AA"},
		{`\x7f`, "\x7f", true, "\x7f"},
		{`\xff`, "ÿ", true, "ÿ"},
		{`a\tb`, "a\tb", true, "a\tb"},
		{`\t`, "\t", true, "\t"},
		{`\a`, "\a", true, "\a"},
		{`\f`, "\f", true, "\f"},
		{`\v`, "\v", true, "\v"},
		{`a\ b`, "a b", true, "a b"},
		{`a\_b`, "a_b", true, "a_b"},
		{`a\-b`, "a-b", true, "a-b"},
		{`a\.b`, "a.b", true, "a.b"},
		{`a\:b`, "a:b", true, "a:b"},
		{`a\/b`, "a/b", true, "a/b"},
		// Literal newline and literal FS in the pattern are atoms.
		{"a\nb", "a\nb", true, "a\nb"},
		{"a\x1cb", "a\x1cb", true, "a\x1cb"},
		{`café`, "café", true, "café"},
		{"\U0001F600", "\U0001F600", true, "\U0001F600"},
		{`[é-ü]`, "ê", true, "ê"},
		{`[é-ü]`, "a", false, ""},
	}
	for _, tc := range cases {
		re, err := Compile(tc.pattern)
		if err != nil {
			t.Fatalf("Compile(%q) = %v, want admit", tc.pattern, err)
		}
		if got := re.MatchString(tc.input); got != tc.want {
			t.Errorf("Compile(%q).MatchString(%q) = %v, want %v", tc.pattern, tc.input, got, tc.want)
		}
		if tc.want {
			if got := re.FindString(tc.input); got != tc.find {
				t.Errorf("Compile(%q).FindString(%q) = %q, want %q", tc.pattern, tc.input, got, tc.find)
			}
		}
	}
}

// Anchors on stripped inputs only. Trailing-newline inputs are
// deliberately not asserted: Python $ matches before a trailing \n
// while Go $ matches only at end, and the strip precondition removes
// that input class before matching.
func TestIndependentReviewAnchorsStripped(t *testing.T) {
	cases := []struct {
		pattern string
		input   string
		want    bool
	}{
		{`\Afoo`, "foo", true},
		{`\Afoo`, "xfoo", false},
		{`\A`, "", true},
		{`\Aa`, "a", true},
		{`^$`, "", true},
		{`^`, "", true},
		{`$`, "", true},
		{`foo$`, "xfoo", true},
		{`foo$`, "foobar", false},
		{`foo$`, "foo", true},
		{`a$b`, "a\nb", false},
		{`a^b`, "a^b", false},
		{`a\Aa`, "a\na", false},
		{`(\A)*`, "a", true},
		{`(^)*`, "a", true},
		{`(^a)`, "a", true},
		{`(a$)`, "a", true},
	}
	for _, tc := range cases {
		re, err := Compile(tc.pattern)
		if err != nil {
			t.Fatalf("Compile(%q) = %v, want admit", tc.pattern, err)
		}
		if got := re.MatchString(tc.input); got != tc.want {
			t.Errorf("Compile(%q).MatchString(%q) = %v, want %v", tc.pattern, tc.input, got, tc.want)
		}
	}
}

// Invalid-UTF8 patterns cannot arise from valid JSON but must not
// panic and must fail with the static value-free backstop.
func TestIndependentReviewInvalidUTF8(t *testing.T) {
	for _, pattern := range []string{"\xff", "\xfe", "a\xff", "\x80", "a\x80b"} {
		re, err := Compile(pattern)
		if err != ErrInvalidPattern {
			t.Errorf("Compile(%q) = %v, %v; want ErrInvalidPattern", pattern, re, err)
		}
		if err != nil && strings.Contains(err.Error(), "\xff") {
			t.Errorf("error for invalid-UTF8 pattern leaks bytes: %q", err)
		}
	}
	// Invalid-UTF8 inputs must not panic the matcher (IDs are valid
	// Unicode in production; this only pins robustness).
	re, err := Compile(`.*`)
	if err != nil {
		t.Fatalf("Compile(.*) = %v", err)
	}
	for _, input := range []string{"\xff", "a\xffb", "\x80"} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("MatchString(%q) panicked: %v", input, r)
				}
			}()
			_ = re.MatchString(input)
			_ = re.FindString(input)
		}()
	}
}
