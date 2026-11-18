// Python-compatible whitespace stripping shared by catalog config and rows.
package catalog

import "strings"

// pythonStripCutset reproduces Python str.strip() (no-arg) exactly for
// catalog IDs, per docs/go-catalog-regex-portability.md §5: Unicode
// White_Space plus U+001C–U+001F, which Go strings.TrimSpace omits
// (unicode.IsSpace leaves them). The future mapping worker MUST NOT use
// TrimSpace; identical stripping is what makes `$` portable.
//
// Intentional delta from the document snippet: the cutset is written
// with \u escapes (real U+0085/U+00A0 runes), not single \x85\xa0 bytes,
// so strings.Trim matches whole runes.
const pythonStripCutset = "\t\n\x0b\x0c\r \x1c\x1d\x1e\x1f" +
	"\u0085\u00a0\u1680" +
	"\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a" +
	"\u2028\u2029\u202f\u205f\u3000"

// StripModelID strips a catalog ID exactly like Python model_id.strip().
// Blank-after-strip IDs are skipped by the mapper; never mutate IDs to
// force parity beyond this strip.
func StripModelID(s string) string {
	return strings.Trim(s, pythonStripCutset)
}
