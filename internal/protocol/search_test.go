package protocol

import (
	"strings"
	"testing"
)

func TestSearchAnnotationBoundAndRuneOffsets(t *testing.T) {
	results := []SearchResultView{{Label: "[S1]", URL: "https://example.invalid", Title: "Source"}}
	annotations, err := buildSearchAnnotations("📝[S8]é [S1]", results)
	if err != nil || len(annotations) != 1 || annotations[0]["start_index"] != 7 || annotations[0]["end_index"] != 11 {
		t.Fatalf("only the verified marker must have rune offsets: %v %v", annotations, err)
	}
	if _, err := buildSearchAnnotations(strings.Repeat("[S1]", maxSearchAnnotations+1), results); err == nil || err.Status != 502 {
		t.Fatalf("excess citations must fail within the bound: %v", err)
	}
}
