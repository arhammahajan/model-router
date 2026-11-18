// Package catalog owns the portable catalog-rule pattern gate: a small
// lexical admission checker plus Go compilation for catalog rule match
// expressions.
//
// Python compiles each rule with re.compile (PCRE-like); Go uses RE2.
// Some constructs compile under both engines with different meaning, and
// some compile under only one side, so Go compilation alone does not
// prove parity. Compile therefore rejects every construct without
// demonstrated identical Python re semantics before delegating to
// regexp.Compile, and maps every failure to a static sentinel error:
// rejection reasons never echo the rule text, so ordinary logs stay
// value-free. The caller prefixes catalog.rules[index].match context.
//
// The admitted subset, the recorded cross-language probes behind it, and
// the operator rewrite hints live in
// docs/go-catalog-regex-portability.md. That document is the proof this
// gate rests on; shrink the rejection set only by extending the proof,
// never by silent acceptance.
package catalog

import (
	"errors"
	"regexp"
	"unicode/utf8"
)

// Static rejection reasons. All are value-free: none interpolates the
// rule text, so the caller can safely add catalog.rules[index].match
// context and log the result.
var (
	// ErrEmptyPattern rejects an empty rule: both engines compile it
	// into a match-everything rule, which is never an intended filter.
	ErrEmptyPattern = errors.New("catalog: rule pattern must not be empty")
	// ErrGroupConstruct rejects every (?...) form except (?:...):
	// lookaround, atomic groups, comments, named groups and, above
	// all, inline flags, whose casefolding differs across engines.
	ErrGroupConstruct = errors.New("catalog: rule pattern uses a group construct without identical Python re semantics; only (?:...) is admitted")
	// ErrBackreference rejects \1-\9 (and octal escapes, which share
	// the leading-digit prefix): RE2 cannot express backreferences.
	ErrBackreference = errors.New("catalog: rule pattern uses a backreference or octal escape without identical Python re semantics")
	// ErrShorthandClass rejects \d \D \w \W \s \S: both engines compile
	// them, but Python matches Unicode characters where Go matches
	// ASCII only.
	ErrShorthandClass = errors.New("catalog: rule pattern uses a Unicode shorthand class without identical Python re semantics; use an explicit range such as [0-9]")
	// ErrWordBoundary rejects \b \B for the same Unicode-word reason:
	// boundary placement differs wherever \w differs.
	ErrWordBoundary = errors.New("catalog: rule pattern uses a word boundary without identical Python re semantics; anchor with ^ $ or \\A instead")
	// ErrPOSIXClass rejects [[:...:]]: Go treats it as a POSIX class
	// while Python reads it as a literal character set.
	ErrPOSIXClass = errors.New("catalog: rule pattern uses a POSIX class without identical Python re semantics; use an explicit range")
	// ErrGoOnlyEscape rejects escapes Go accepts but Python re
	// rejects outright (\p \P \Q): Go-compiles/Python-fails drift.
	ErrGoOnlyEscape = errors.New("catalog: rule pattern uses an escape Go accepts but Python re rejects; use a portable construct")
	// ErrEndAnchor rejects \z and \Z: \Z does not compile in Go, and
	// \z is undocumented in Python, so neither has a stable contract.
	ErrEndAnchor = errors.New("catalog: rule pattern uses \\z or \\Z without a stable cross-language contract; anchor with $ instead (IDs are stripped before matching)")
	// ErrInvalidEscape rejects every other non-portable escape
	// (\u \U \c \E and unknown escapes): Python-only, Go-only, or
	// rejected by both sides.
	ErrInvalidEscape = errors.New("catalog: rule pattern uses an escape without identical Python re semantics")
	// ErrBraceQuantifier rejects brace forms outside {m} {m,} {m,n} —
	// notably Python's {,n} (a Go literal) and bare literal braces,
	// which need an explicit \{ rewrite to stay portable.
	ErrBraceQuantifier = errors.New("catalog: rule pattern uses a brace form without identical Python re semantics; use {m} {m,} or {m,n} after an atom, or escape a literal brace as \\{")
	// ErrInvalidRepetition rejects quantifiers with nothing to repeat
	// and out-of-range {m,n} (m>n): both engines reject these too, so
	// this is a static, value-free spelling of the compile failure.
	ErrInvalidRepetition = errors.New("catalog: rule pattern repeats nothing or repeats out of range")
	// ErrInvalidPattern is the static backstop for anything the
	// admission scan passes but regexp.Compile refuses. regexp errors
	// echo the offending fragment, so the raw error is discarded to
	// keep rejections value-free.
	ErrInvalidPattern = errors.New("catalog: rule pattern does not compile")
)

// Compile checks pattern against the portable subset documented in
// docs/go-catalog-regex-portability.md and returns the compiled RE2
// program.
//
// Matching uses the returned value's MatchString, the exact equivalent
// of Python pattern.search: unanchored search, so rules anchor with
// ^...$ for an exact match. $ parity depends on the documented strip
// precondition (IDs are stripped exactly as Python str.strip does
// before matching); see the portability document.
func Compile(pattern string) (*regexp.Regexp, error) {
	if pattern == "" {
		return nil, ErrEmptyPattern
	}
	if err := checkPortable(pattern); err != nil {
		return nil, err
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, ErrInvalidPattern
	}
	return re, nil
}

// Quantifier-state machine over top-level tokens. It exists for one
// reason: brace and repetition operators are where the engines
// silently disagree ({,n}) or loudly agree to fail (a**, {2} leader),
// and the failure text must never leak the rule.
const (
	qAtom   = iota // previous token is a repeatable atom
	qGreedy        // a greedy quantifier was consumed; one lazy ? allowed
	qLazy          // lazy ? consumed; no further quantifier char allowed
	qNone          // zero-width or opening token; no quantifier allowed
)

// checkPortable admits exactly the documented subset. Anything else
// fails with a static sentinel; anything it passes but RE2 refuses
// fails later as ErrInvalidPattern.
func checkPortable(pattern string) error {
	inClass := false
	classFirst := false // next byte is first in class: ] is literal
	state := qNone
	i, n := 0, len(pattern)
	for i < n {
		c := pattern[i]
		if c >= 0x80 {
			// Non-ASCII bytes have no syntactic meaning in either
			// engine; consume the whole rune as a literal. Unicode
			// literals and explicit ranges are admitted: probed
			// identical (é, [é-ü]).
			_, size := utf8.DecodeRuneInString(pattern[i:])
			i += size
			if inClass {
				classFirst = false
			} else {
				state = qAtom
			}
			continue
		}
		if c == '\\' {
			if i+1 >= n {
				return ErrInvalidEscape
			}
			width, atom, err := checkEscape(pattern[i+1:], inClass)
			if err != nil {
				return err
			}
			i += width
			if inClass {
				classFirst = false
			} else if atom {
				state = qAtom
			} else {
				state = qNone
			}
			continue
		}
		if inClass {
			switch c {
			case ']':
				if classFirst {
					classFirst = false
				} else {
					inClass = false
					state = qAtom
				}
			case '[':
				// Go reads [[:alpha:]] as a POSIX class;
				// Python reads a literal set. Recorded
				// divergence: reject on sight.
				if i+1 < n && pattern[i+1] == ':' {
					return ErrPOSIXClass
				}
				classFirst = false
			default:
				classFirst = false
			}
			i++
			continue
		}
		switch c {
		case '[':
			if i+1 < n && pattern[i+1] == ':' {
				return ErrPOSIXClass
			}
			inClass = true
			classFirst = true
			state = qNone
			i++
		case '(':
			// Only (?:...) has identical semantics. Every other
			// (?...) form — flags, scoped flags, lookaround,
			// atomic groups, comments, named groups and named
			// backreferences — is rejected even when both engines
			// compile it, because flag/casefold semantics differ
			// and the rest has no RE2 counterpart.
			if i+1 < n && pattern[i+1] == '?' {
				if i+2 >= n || pattern[i+2] != ':' {
					return ErrGroupConstruct
				}
				i += 3
			} else {
				i++
			}
			state = qNone
		case ')':
			// Balance is left to regexp.Compile (both engines
			// reject a lone closer); a closer after an atom is a
			// repeatable group end.
			state = qAtom
			i++
		case '{':
			end, lazy, err := scanQuantifier(pattern, i, state)
			if err != nil {
				return err
			}
			i = end
			if lazy {
				state = qLazy
			} else {
				state = qGreedy
			}
		case '*', '+', '?':
			switch state {
			case qAtom:
				state = qGreedy
			case qGreedy:
				// Only the single lazy ? may follow a greedy
				// quantifier; *? +? ?? {m,n}? are admitted
				// (probed identical), anything else repeats a
				// repeat (both engines refuse).
				if c != '?' {
					return ErrInvalidRepetition
				}
				state = qLazy
			default:
				return ErrInvalidRepetition
			}
			i++
		case '^', '$', '|':
			state = qNone
			i++
		case '.':
			// Dot is an atom (repeatable); both engines refuse
			// to match \n with it unless a rejected flag says
			// otherwise (probed identical).
			state = qAtom
			i++
		case '}', ']':
			// Bare closers outside a class are literals in both
			// engines (probed identical).
			state = qAtom
			i++
		default:
			state = qAtom
			i++
		}
	}
	return nil
}

// checkEscape validates the escape starting at rest[0] (the byte after
// the backslash). It returns the total width in bytes including the
// backslash, and whether the escape yields a repeatable character
// (\A is zero-width and is not repeatable).
func checkEscape(rest string, inClass bool) (width int, atom bool, err error) {
	e := rest[0]
	switch {
	case e == 'd' || e == 'D' || e == 'w' || e == 'W' || e == 's' || e == 'S':
		// Recorded divergence (Arabic-Indic digit, é, NBSP):
		// reject inside and outside classes alike.
		return 0, false, ErrShorthandClass
	case e == 'b' || e == 'B':
		// Boundary placement follows \w, so it diverges wherever
		// \w diverges (recorded: a\b on "aé").
		return 0, false, ErrWordBoundary
	case e >= '0' && e <= '9':
		// Backreferences have no RE2 counterpart; octal escapes
		// share the prefix and go with them.
		return 0, false, ErrBackreference
	case e == 'p' || e == 'P' || e == 'Q':
		// Go-only: Python re raises "bad escape". \E alone is
		// rejected by both sides and falls under ErrInvalidEscape.
		return 0, false, ErrGoOnlyEscape
	case e == 'Z' || e == 'z':
		return 0, false, ErrEndAnchor
	case e == 'x':
		// \xHH is admitted (probed identical); \x{...} is
		// Go-only and short/long forms fail on both sides.
		if len(rest) < 3 || !isHex(rest[1]) || !isHex(rest[2]) {
			return 0, false, ErrInvalidEscape
		}
		return 4, true, nil
	case e == 'A':
		if inClass {
			// Meaningless in a class; both engines refuse it
			// there. Outside, \A is admitted (probed identical).
			return 0, false, ErrInvalidEscape
		}
		return 2, false, nil
	case e == 't' || e == 'n' || e == 'r' || e == 'f' || e == 'v' || e == 'a' || e == '\\':
		return 2, true, nil
	case isAdmittedPunct(e):
		// Escaped ASCII punctuation and space match the literal
		// character in both engines (probed: \. \* \/ \: \-).
		return 2, true, nil
	default:
		// \u \U \c \E \k \g \N and every other letter escape:
		// Python-only, Go-only, or refused by both. No quiet
		// drift either way.
		return 0, false, ErrInvalidEscape
	}
}

// scanQuantifier parses {m} {m,} {m,n} at pattern[i]. state is the
// quantifier-state before the brace. It returns the offset past the
// brace (and one lazy ? suffix) and whether that suffix was present.
func scanQuantifier(pattern string, i, state int) (end int, lazy bool, err error) {
	j, n := i+1, len(pattern)
	mStart := j
	for j < n && isDigit(pattern[j]) {
		j++
	}
	mStr := pattern[mStart:j]
	var nStr string
	hasComma := false
	if j < n && pattern[j] == ',' {
		hasComma = true
		j++
		nStart := j
		for j < n && isDigit(pattern[j]) {
			j++
		}
		nStr = pattern[nStart:j]
	}
	// Anything else — {,n}, {abc}, unterminated {2 — is not a shared
	// quantifier. Python's {,n} compiles to a bounded repeat where Go
	// reads a literal (recorded divergence); the rest fails on at
	// least one side. Reject with the brace hint either way.
	if mStr == "" || j >= n || pattern[j] != '}' {
		return 0, false, ErrBraceQuantifier
	}
	// Python accepts leading-zero bounds as repetition while RE2 treats
	// the entire brace form as literal text. Require canonical decimals.
	if (len(mStr) > 1 && mStr[0] == '0') || (len(nStr) > 1 && nStr[0] == '0') {
		return 0, false, ErrBraceQuantifier
	}
	if hasComma && nStr != "" && cmpNum(mStr, nStr) > 0 {
		// {m,n} with m>n: both engines refuse; static spelling.
		return 0, false, ErrInvalidRepetition
	}
	if state != qAtom {
		// {m} with nothing to repeat ({2} leader, a*{2}): both
		// engines refuse; static spelling.
		return 0, false, ErrInvalidRepetition
	}
	j++ // past }
	if j < n && pattern[j] == '?' {
		// Lazy {m,n}?: probed identical.
		return j + 1, true, nil
	}
	return j, false, nil
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isHex(c byte) bool {
	return isDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// isAdmittedPunct reports ASCII punctuation plus space: every byte
// whose backslash escape is a plain literal in both engines.
func isAdmittedPunct(c byte) bool {
	return (c >= 0x20 && c <= 0x2F) ||
		(c >= 0x3A && c <= 0x40) ||
		(c >= 0x5B && c <= 0x60) ||
		(c >= 0x7B && c <= 0x7E)
}

// cmpNum compares nonempty decimal strings numerically.
func cmpNum(a, b string) int {
	a = stripZeros(a)
	b = stripZeros(b)
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func stripZeros(s string) string {
	i := 0
	for i < len(s)-1 && s[i] == '0' {
		i++
	}
	return s[i:]
}
