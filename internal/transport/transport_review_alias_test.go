package transport

import (
	"testing"

	"github.com/arhammahajan/router/internal/endpoint"
)

// Review: wrapper reuses the shared helper exactly, including edge inputs.
func TestReviewUpstreamURLMatchesHelperOnEdges(t *testing.T) {
	bases := []string{
		"https://h.example",
		"https://h.example/",
		"https://h.example/v1/messages/",
		"https://h.example/custom/prefix",
		"https://h.example/custom/prefix/v1/messages/",
		"https://h.example/v1%2Fmessages",
		"https://h.example//v1/messages",
		"https://H.EXAMPLE/v1/messages",
		"https://h.example:443/v1/messages",
		"https://h.example/xchat/completions",
		"https://h.example/chat/completions/extra",
		"https://g.example/v1",
		"https://g.example/v1beta",
		"https://g.example",
	}
	dialects := []string{"anthropic", "openai-chat", "openai-responses", "gemini"}
	for _, d := range dialects {
		for _, b := range bases {
			want, herr := endpoint.GenerationEndpoint(b, d)
			got, rerr := UpstreamURL(b, d)
			if herr != nil {
				t.Fatalf("helper error: %v", herr)
			}
			if rerr != nil || got != want {
				t.Fatalf("%s %q: wrapper %q vs helper %q", d, b, got, want)
			}
		}
	}
	if _, rerr := UpstreamURL("https://h.example", "bogus"); rerr == nil || rerr.Status != 500 {
		t.Fatalf("unknown dialect must stay RouterError 500")
	}
}

func TestReviewCountTokensFirstPartyExact(t *testing.T) {
	cases := []struct {
		base string
		want bool
	}{
		{"https://api.anthropic.com", true},
		{"https://api.anthropic.com/", true},
		{"https://api.anthropic.com:443", false},
		{"https://user@api.anthropic.com", false},
		{"https://api.anthropic.com.evil.example", false},
		{"http://api.anthropic.com", false},
	}
	for _, tc := range cases {
		p := testProvider("anthropic", tc.base)
		if got := CountTokensEnabled(p); got != tc.want {
			t.Fatalf("CountTokensEnabled(%q) = %v, want %v", tc.base, got, tc.want)
		}
	}
	// Explicit true/false wins regardless of host (anthropic only).
	for _, v := range []bool{true, false} {
		p := testProvider("anthropic", "https://other.example")
		b := v
		p.CountTokens = &b
		if got := CountTokensEnabled(p); got != v {
			t.Fatalf("explicit count_tokens=%v must win", v)
		}
	}
}
