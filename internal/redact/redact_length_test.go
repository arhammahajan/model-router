package redact

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDiagnosticLimitCountsCharactersWithoutRewritingUTF8(t *testing.T) {
	got := Redact(strings.Repeat("界", MaxText+1), nil)
	if !utf8.ValidString(got) || utf8.RuneCountInString(got) != MaxText {
		t.Fatalf("diagnostic limit split UTF-8 or counted bytes: bytes=%d characters=%d", len(got), utf8.RuneCountInString(got))
	}
}
