package transport

import (
	"testing"

	"github.com/arhammahajan/model-router/internal/config"
	"github.com/arhammahajan/model-router/internal/endpoint"
)

func TestUpstreamURLMatchesSharedHelper(t *testing.T) {
	bases := []string{
		"https://h.example",
		"https://h.example/",
		"https://h.example/v1",
		"https://h.example/v1/messages",
		"https://h.example/custom/prefix",
		"https://h.example/custom/prefix/chat/completions",
		"https://h.example/v1beta",
		"https://h.example/v1",
	}
	dialects := []string{
		config.DialectAnthropic, config.DialectOpenAIChat,
		config.DialectOpenAIResponses, config.DialectGemini,
	}
	for _, d := range dialects {
		for _, b := range bases {
			want, herr := endpoint.GenerationEndpoint(b, d)
			got, rerr := UpstreamURL(b, d)
			if herr != nil {
				t.Fatalf("helper error: %v", herr)
			}
			if rerr != nil || got != want {
				t.Fatalf("%s %s: wrapper %q vs helper %q", d, b, got, want)
			}
		}
	}
	if _, rerr := UpstreamURL("https://h.example", "bogus"); rerr == nil || rerr.Status != 500 {
		t.Fatalf("unknown dialect must stay RouterError 500")
	}
}

func TestUpstreamURLGeminiDistinct(t *testing.T) {
	v1, _ := UpstreamURL("https://g.example/v1", config.DialectGemini)
	v1beta, _ := UpstreamURL("https://g.example/v1beta", config.DialectGemini)
	if v1 == v1beta {
		t.Fatalf("v1 vs v1beta must stay distinct")
	}
}
