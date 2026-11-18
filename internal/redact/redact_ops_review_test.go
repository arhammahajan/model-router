package redact

import (
	"strings"
	"testing"
)

// Independent review probes for the shared-corpus redaction contract
// (docs/go-operational-read-paths-contract.md §1.2, §7): pure
// configured-secret extraction plus encoded-form expansion in this
// neutral package. Owns only this file. All GREEN: locks the encoded
// forms the older vacuous loop lists without asserting.

// GREEN: URL-encoded space forms (+ and %20) redact via the expanded corpus.
func TestReviewEncodedSpaceFormsRedacted(t *testing.T) {
	corpus := ExpandCorpus([]string{"tok en"})
	for _, form := range []string{"tok en", "tok+en", "tok%20en"} {
		if got := Redact("v="+form, corpus); strings.Contains(got, form) {
			t.Fatalf("encoded form leaked: %q -> %q", form, got)
		}
	}
}

// GREEN: non-ASCII secrets redact in \\u-escaped JSON form (ONE decode,
// no cache-ID allowlist reuse).
func TestReviewNonASCIIEscapedFormRedacted(t *testing.T) {
	corpus := ExpandCorpus([]string{"café"})
	form := `caf\u00e9`
	found := false
	for _, c := range corpus {
		if strings.Contains(c, form) {
			found = true
		}
	}
	if !found {
		t.Fatalf("corpus must carry the \\u-escaped form: %q", corpus)
	}
	if got := Redact("v="+form, corpus); strings.Contains(got, form) {
		t.Fatalf("escaped form leaked: %q", got)
	}
}
