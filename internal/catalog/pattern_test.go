package catalog

import (
	"strings"
	"testing"
)

// Accepted patterns: every entry compiles under Compile and matches as
// recorded. Go-side results below were executed against RE2; the
// Python-side results they must agree with are recorded in
// docs/go-catalog-regex-portability.md (offline stdlib re probes, no
// Python runs in this suite by design).
func TestCompileAccepted(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		input   string
		want    bool
		find    string
	}{
		// Anchored exact rule, the common catalog shape.
		{"exact", `^openai/gpt-4$`, "openai/gpt-4", true, "openai/gpt-4"},
		{"exactReject", `^openai/gpt-4$`, "openai/gpt-4o", false, ""},
		// Search semantics: MatchString is Python pattern.search.
		{"search", `openai/`, "xxopenai/yy", true, "openai/"},
		// Non-capturing alternation with escaped dot.
		{"alternation", `(?:gpt-4|gpt-3\.5)`, "gpt-3.5", true, "gpt-3.5"},
		{"alternationDot", `(?:gpt-4|gpt-3\.5)`, "gpt-3x5", false, ""},
		// Ranges, negation, one-or-more.
		{"classRange", `[a-z0-9-]+`, "abc-123", true, "abc-123"},
		{"negated", `[^/]+/[^/]+`, "a/b", true, "a/b"},
		{"negatedNoSlash", `[^/]+/[^/]+`, "ab", false, ""},
		// Greedy bounded repeats (probed identical incl. {m,}).
		{"count", `a{2}`, "aa", true, "aa"},
		{"countOpen", `a{2,}`, "aaaa", true, "aaaa"},
		{"zero", `a{0}b`, "b", true, "b"},
		// Lazy quantifiers (probed identical: leftmost-shortest).
		{"lazyPlus", `a+?`, "aaa", true, "a"},
		{"lazyCount", `a{2,3}?`, "aaa", true, "aa"},
		// Escapes: hex, tab, punctuation (all probed identical).
		{"hex", `\x41`, "A", true, "A"},
		{"tab", `a\tb`, "a\tb", true, "a\tb"},
		{"colon", `a\:b`, "a:b", true, "a:b"},
		{"slash", `a\/b`, "a/b", true, "a/b"},
		// Start anchor \A (probed identical); ^ likewise.
		{"startAnchor", `\Afoo`, "foo", true, "foo"},
		{"startAnchorMiss", `\Afoo`, "xfoo", false, ""},
		// End anchor $ on stripped input (probed identical; the
		// trailing-newline divergence cannot occur post-strip).
		{"endAnchor", `foo$`, "foo", true, "foo"},
		// Unicode literals and explicit ranges (probed identical).
		{"unicodeLit", `^café-.*$`, "café-x", true, "café-x"},
		{"unicodeRange", `[é-ü]`, "ê", true, "ê"},
		// Dot refuses \n on both sides (probed identical).
		{"dotNewline", `.`, "\n", false, ""},
		// Empty group compiles on both sides; kept admitted as the
		// degenerate non-capturing case.
		{"emptyGroup", `()`, "x", true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			re, err := Compile(tc.pattern)
			if err != nil {
				t.Fatalf("Compile(%q) = %v, want nil", tc.pattern, err)
			}
			if got := re.MatchString(tc.input); got != tc.want {
				t.Errorf("MatchString(%q) = %v, want %v", tc.input, got, tc.want)
			}
			if tc.want {
				if got := re.FindString(tc.input); got != tc.find {
					t.Errorf("FindString(%q) = %q, want %q", tc.input, got, tc.find)
				}
			}
		})
	}
}

// Rejected patterns: each entry names the exact static sentinel it must
// fail with. Comments cite the recorded divergence class; full probe
// transcripts live in docs/go-catalog-regex-portability.md.
func TestCompileRejected(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		want    error
	}{
		{"empty", ``, ErrEmptyPattern},
		// Inline flags: (?i)i matches İ in Python, not in Go.
		{"flagGlobal", `(?i)foo`, ErrGroupConstruct},
		{"flagScoped", `(?i:foo)`, ErrGroupConstruct},
		{"flagDotall", `(?s:.)`, ErrGroupConstruct},
		{"flagMultiline", `(?m:^foo)`, ErrGroupConstruct},
		// Lookaround / atomic / comments: Python-only (Go refuses).
		{"lookahead", `(?=a)b`, ErrGroupConstruct},
		{"negLookahead", `(?!a)b`, ErrGroupConstruct},
		{"lookbehind", `(?<=a)b`, ErrGroupConstruct},
		{"negLookbehind", `(?<!a)b`, ErrGroupConstruct},
		{"atomic", `(?>a)b`, ErrGroupConstruct},
		{"comment", `(?#comment)a`, ErrGroupConstruct},
		// Named groups: same match result on both sides today, but
		// only (?:...) is admitted; rewrite is mechanical.
		{"namedGroup", `(?P<n>a)`, ErrGroupConstruct},
		{"namedBackref", `(?P<n>a)(?P=n)`, ErrGroupConstruct},
		// Numeric backreferences: no RE2 counterpart.
		{"backref", `a\1`, ErrBackreference},
		{"octal", `\0`, ErrBackreference},
		// Unicode shorthand classes: Python matches ٣/é/NBSP, Go
		// matches ASCII only (recorded divergence).
		{"digit", `\d`, ErrShorthandClass},
		{"notDigit", `\D`, ErrShorthandClass},
		{"word", `\w`, ErrShorthandClass},
		{"notWord", `\W`, ErrShorthandClass},
		{"space", `\s`, ErrShorthandClass},
		{"notSpace", `\S`, ErrShorthandClass},
		{"shorthandInClass", `[\d]`, ErrShorthandClass},
		// Word boundaries follow \w, so they diverge too (a\b on
		// "aé": Go matches, Python does not).
		{"boundary", `\bfoo`, ErrWordBoundary},
		{"notBoundary", `a\B`, ErrWordBoundary},
		// POSIX class: Go reads a class, Python a literal set.
		{"posix", `[[:alpha:]]`, ErrPOSIXClass},
		{"posixLead", `[[:alpha:]x]`, ErrPOSIXClass},
		// Go-only escapes: Python raises "bad escape".
		{"unicodeClass", `\p{L}`, ErrGoOnlyEscape},
		{"notUnicodeClass", `\P{L}`, ErrGoOnlyEscape},
		{"quoting", `\Qabc\E`, ErrGoOnlyEscape},
		// End anchors without a stable contract: \Z does not
		// compile in Go; \z is undocumented in Python.
		{"capZ", `\Z`, ErrEndAnchor},
		{"lowZ", `\z`, ErrEndAnchor},
		{"lowZRule", `foo\z`, ErrEndAnchor},
		// Python-only / doubly-refused escapes.
		{"unicodeEscape", `\u0041`, ErrInvalidEscape},
		{"hexBraces", `\x{41}`, ErrInvalidEscape},
		{"controlC", `\cA`, ErrInvalidEscape},
		{"loneE", `\E`, ErrInvalidEscape},
		{"trailingBackslash", `abc\`, ErrInvalidEscape},
		{"assertInClass", `[\A]`, ErrInvalidEscape},
		// Braces: {,n} compiles both ways with different meaning
		// (Python bounded repeat, Go literal); bare braces need an
		// explicit \{ rewrite (conservative, documented).
		{"commaQuant", `a{,3}`, ErrBraceQuantifier},
		{"bareBrace", `a{b`, ErrBraceQuantifier},
		{"loneBrace", `{`, ErrBraceQuantifier},
		// Repetition errors both engines also refuse; the gate
		// spells them statically instead of echoing RE2 text.
		{"inverted", `a{3,2}`, ErrInvalidRepetition},
		{"leaderCount", `{2}`, ErrInvalidRepetition},
		{"doubleStar", `a**`, ErrInvalidRepetition},
		{"doubleLazy", `a*??`, ErrInvalidRepetition},
		{"leaderStar", `*`, ErrInvalidRepetition},
		// Backstop: admitted by the scan, refused by RE2 (both
		// engines refuse these too; probes on file).
		{"unbalancedOpen", `(a`, ErrInvalidPattern},
		{"unbalancedClose", `a)`, ErrInvalidPattern},
		{"unclosedClass", `[abc`, ErrInvalidPattern},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			re, err := Compile(tc.pattern)
			if err == nil {
				t.Fatalf("Compile(%q) = %v, want %v", tc.pattern, re, tc.want)
			}
			if err != tc.want {
				t.Errorf("Compile(%q) error = %q, want %q", tc.pattern, err, tc.want)
			}
			// err == tc.want is sentinel identity: the rejection is
			// a package-level constant, hence value-free. Longer
			// smuggling is pinned by TestCompileErrorValueFree.
		})
	}
}

// Rejections must never carry the rule text: operators will prefix
// catalog.rules[index].match themselves, and patterns may name
// not-yet-public models. Every sentinel is static by construction;
// this test pins the property across a hostile input.
func TestCompileErrorValueFree(t *testing.T) {
	secret := "sk-PROBE-secret-model-zzz"
	for _, pattern := range []string{
		`(?i)` + secret,
		secret + `\d+`,
		`[[:` + secret,
		secret + `\p{L}`,
		secret + `\z`,
		secret + `\u0041`,
		secret + `{,2}`,
		`(` + secret,
	} {
		_, err := Compile(pattern)
		if err == nil {
			t.Fatalf("Compile(%q) succeeded, want rejection", pattern)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error for %q leaks rule text: %q", pattern, err)
		}
	}
}
