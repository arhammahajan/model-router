package endpoint

import "testing"

// Review: conservative equivalence — only trailing-slash/suffix rules.
// No host/port/case/encoding normalization is implied.
func TestReviewEndpointAliasConservative(t *testing.T) {
	equalPairs := []struct{ base, alias, dialect string }{
		{"https://h.example", "https://h.example/", "anthropic"},
		{"https://h.example/", "https://h.example/v1/messages/", "anthropic"},
		{"https://h.example/custom/prefix", "https://h.example/custom/prefix/v1/messages", "anthropic"},
		{"https://h.example/custom/prefix/v1", "https://h.example/custom/prefix", "anthropic"},
		{"https://h.example/prefix", "https://h.example/prefix/chat/completions", "openai-chat"},
		{"https://h.example/prefix/chat/completions", "https://h.example/prefix/chat/completions/", "openai-chat"},
		{"https://h.example/v1", "https://h.example/v1/responses", "openai-responses"},
		{"https://h.example", "https://h.example/v1beta", "gemini"},
		{"https://h.example/prefix", "https://h.example/prefix/v1beta", "gemini"},
	}
	for _, tc := range equalPairs {
		a, err1 := GenerationEndpoint(tc.base, tc.dialect)
		b, err2 := GenerationEndpoint(tc.alias, tc.dialect)
		if err1 != nil || err2 != nil || a != b {
			t.Fatalf("%s %q vs %q must be equal: %q vs %q (%v,%v)", tc.dialect, tc.base, tc.alias, a, b, err1, err2)
		}
	}
	divergent := []struct{ base, alias, dialect string }{
		// Encoded paths stay distinct (no decoding).
		{"https://h.example/v1/messages", "https://h.example/v1%2Fmessages", "anthropic"},
		{"https://h.example/v1/messages", "https://h.example/%76%31/messages", "anthropic"},
		// Host case / port / scheme are distinct (no aggressive normalization).
		{"https://h.example/v1/messages", "https://H.EXAMPLE/v1/messages", "anthropic"},
		{"https://h.example/v1/messages", "https://h.example:443/v1/messages", "anthropic"},
		{"https://h.example/v1/messages", "http://h.example/v1/messages", "anthropic"},
		// Double slash is distinct (no path cleaning).
		{"https://h.example/v1/messages", "https://h.example//v1/messages", "anthropic"},
		// Lookalike suffixes must not match.
		{"https://h.example/chat/completions", "https://h.example/xchat/completions", "openai-chat"},
		{"https://h.example/responses", "https://h.example/myresponses", "openai-responses"},
		{"https://h.example/v1/messages", "https://h.example/V1/MESSAGES", "anthropic"},
		// Trailing extra path is distinct.
		{"https://h.example/extra", "https://h.example/chat/completions/extra", "openai-chat"},
		// Gemini v1 vs v1beta stay distinct.
		{"https://g.example/v1", "https://g.example/v1beta", "gemini"},
	}
	for _, tc := range divergent {
		a, _ := GenerationEndpoint(tc.base, tc.dialect)
		b, _ := GenerationEndpoint(tc.alias, tc.dialect)
		if a == b {
			t.Fatalf("%s %q vs %q must stay distinct (got %q)", tc.dialect, tc.base, tc.alias, a)
		}
	}
}

func TestReviewEndpointAliasGeminiOriginEquiv(t *testing.T) {
	origin, _ := GenerationEndpoint("https://g.example", "gemini")
	beta, _ := GenerationEndpoint("https://g.example/v1beta", "gemini")
	if origin != beta {
		t.Fatalf("origin and v1beta must be equivalent: %q vs %q", origin, beta)
	}
	v1, _ := GenerationEndpoint("https://g.example/v1", "gemini")
	if v1 == beta {
		t.Fatalf("v1 and v1beta must stay distinct: %q", v1)
	}
}
