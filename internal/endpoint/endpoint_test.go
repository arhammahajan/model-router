package endpoint

import "testing"

func TestGenerationEndpointAllDialects(t *testing.T) {
	cases := []struct{ base, dialect, want string }{
		{"https://h.example", "anthropic", "https://h.example/v1/messages"},
		{"https://h.example/", "anthropic", "https://h.example/v1/messages"},
		{"https://h.example/v1", "anthropic", "https://h.example/v1/messages"},
		{"https://h.example/v1/", "anthropic", "https://h.example/v1/messages"},
		{"https://h.example/v1/messages", "anthropic", "https://h.example/v1/messages"},
		{"https://h.example/v1/messages/", "anthropic", "https://h.example/v1/messages"},
		{"https://h.example/custom/prefix", "anthropic", "https://h.example/custom/prefix/v1/messages"},
		{"https://h.example/custom/prefix/v1/messages", "anthropic", "https://h.example/custom/prefix/v1/messages"},
		{"https://h.example", "openai-chat", "https://h.example/chat/completions"},
		{"https://h.example/chat/completions", "openai-chat", "https://h.example/chat/completions"},
		{"https://h.example/chat/completions/", "openai-chat", "https://h.example/chat/completions"},
		{"https://h.example/prefix", "openai-chat", "https://h.example/prefix/chat/completions"},
		{"https://h.example", "openai-responses", "https://h.example/responses"},
		{"https://h.example/v1", "openai-responses", "https://h.example/v1/responses"},
		{"https://h.example/v1/responses", "openai-responses", "https://h.example/v1/responses"},
		{"https://h.example/prefix", "openai-responses", "https://h.example/prefix/responses"},
		{"https://h.example", "gemini", "https://h.example/v1beta"},
		{"https://h.example/v1beta", "gemini", "https://h.example/v1beta"},
		{"https://h.example/v1", "gemini", "https://h.example/v1"},
		{"https://h.example/prefix", "gemini", "https://h.example/prefix/v1beta"},
	}
	for _, tc := range cases {
		got, err := GenerationEndpoint(tc.base, tc.dialect)
		if err != nil || got != tc.want {
			t.Fatalf("%s %s: got %q err %v want %q", tc.dialect, tc.base, got, err, tc.want)
		}
	}
}

func TestGenerationEndpointGeminiV1VsV1betaDistinct(t *testing.T) {
	v1, _ := GenerationEndpoint("https://g.example/v1", "gemini")
	v1beta, _ := GenerationEndpoint("https://g.example/v1beta", "gemini")
	if v1 == v1beta {
		t.Fatalf("v1 and v1beta must stay distinct: %q", v1)
	}
}

func TestGenerationEndpointUnknownDialect(t *testing.T) {
	if _, err := GenerationEndpoint("https://h.example", "bogus"); err == nil {
		t.Fatalf("unknown dialect must error")
	}
}
