package transport

import (
	"net/http"
	"strings"
	"testing"

	"github.com/arhammahajan/router/internal/config"
)

func TestUpstreamURLResponsesAndGeminiBase(t *testing.T) {
	if u, _ := UpstreamURL("https://api.example.com/v1", config.DialectOpenAIResponses); u != "https://api.example.com/v1/responses" {
		t.Fatalf("responses base: %s", u)
	}
	if u, _ := UpstreamURL("https://api.example.com/v1/responses", config.DialectOpenAIResponses); u != "https://api.example.com/v1/responses" {
		t.Fatalf("responses suffix: %s", u)
	}
	if u, _ := UpstreamURL("https://gen.example.com", config.DialectGemini); u != "https://gen.example.com/v1beta" {
		t.Fatalf("gemini base: %s", u)
	}
	if u, _ := UpstreamURL("https://gen.example.com/v1beta", config.DialectGemini); u != "https://gen.example.com/v1beta" {
		t.Fatalf("gemini v1beta suffix: %s", u)
	}
	if u, _ := UpstreamURL("https://gen.example.com/v1/", config.DialectGemini); u != "https://gen.example.com/v1" {
		t.Fatalf("gemini v1 suffix: %s", u)
	}
	if u, _ := CountTokensURL("https://api.anthropic.com"); u != "https://api.anthropic.com/v1/messages/count_tokens" {
		t.Fatalf("count url origin: %s", u)
	}
	if u, _ := CountTokensURL("https://api.anthropic.com/v1"); u != "https://api.anthropic.com/v1/messages/count_tokens" {
		t.Fatalf("count url v1: %s", u)
	}
}

func TestGeminiURLValidationAndEscaping(t *testing.T) {
	u, rerr := GeminiURL("https://gen.example.com", "gemini-2.0-flash", "generateContent", "")
	if rerr != nil || u != "https://gen.example.com/v1beta/models/gemini-2.0-flash:generateContent" {
		t.Fatalf("basic gemini url: %s err %v", u, rerr)
	}
	u, rerr = GeminiURL("https://gen.example.com/v1beta", "models/gemini-2.0-flash", "streamGenerateContent", "sse")
	if rerr != nil || u != "https://gen.example.com/v1beta/models/gemini-2.0-flash:streamGenerateContent?alt=sse" {
		t.Fatalf("prefixed+alt: %s err %v", u, rerr)
	}
	// alt=json is accepted but never forwarded as a query.
	u, rerr = GeminiURL("https://gen.example.com", "x", "generateContent", "json")
	if rerr != nil || strings.Contains(u, "?") {
		t.Fatalf("alt=json must not forward a query: %s err %v", u, rerr)
	}
	for _, bad := range []string{"", "a/../b", "a/./b", "a//b", "a?b", "a#b", "a%b", "a&b", "a\\b", "a\nb"} {
		if _, rerr := GeminiURL("https://gen.example.com", bad, "generateContent", ""); rerr == nil {
			t.Fatalf("traversal/query model %q must be rejected", bad)
		}
	}
	if _, rerr := GeminiURL("https://gen.example.com", "x", "bogus", ""); rerr == nil {
		t.Fatalf("unknown method must be rejected")
	}
	// Space is escaped, "/" and ":" preserved.
	u, rerr = GeminiURL("https://gen.example.com", "my model/v1", "generateContent", "")
	if rerr != nil || !strings.Contains(u, "my%20model/v1:generateContent") {
		t.Fatalf("escaping wrong: %s err %v", u, rerr)
	}
}

func TestCountTokensEnabled(t *testing.T) {
	mk := func(dialect, base string, ct *bool) *config.Provider {
		return &config.Provider{Name: "p", Dialect: dialect, BaseURL: base, CountTokens: ct}
	}
	tr, fl := true, false
	cases := []struct {
		name string
		p    *config.Provider
		want bool
	}{
		{"explicit true arbitrary", mk(config.DialectAnthropic, "http://127.0.0.1:9", &tr), true},
		{"explicit false first-party", mk(config.DialectAnthropic, "https://api.anthropic.com", &fl), false},
		{"absent first-party", mk(config.DialectAnthropic, "https://api.anthropic.com", nil), true},
		{"absent arbitrary", mk(config.DialectAnthropic, "http://127.0.0.1:9", nil), false},
		{"absent look-alike", mk(config.DialectAnthropic, "https://api.anthropic.com.evil.com", nil), false},
		{"absent port", mk(config.DialectAnthropic, "https://api.anthropic.com:443", nil), false},
		{"absent userinfo", mk(config.DialectAnthropic, "https://user@api.anthropic.com", nil), false},
		{"absent http", mk(config.DialectAnthropic, "http://api.anthropic.com", nil), false},
		{"non-anthropic true", mk(config.DialectOpenAIChat, "https://api.anthropic.com", &tr), false},
	}
	for _, tc := range cases {
		if got := CountTokensEnabled(tc.p); got != tc.want {
			t.Fatalf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestAuthHeadersGeminiDefault(t *testing.T) {
	p := &config.Provider{Name: "g", Dialect: config.DialectGemini, BaseURL: "https://x", APIKey: "k"}
	if h := AuthHeaders(p); h["x-goog-api-key"] != "k" {
		t.Fatalf("gemini default must be x-goog-api-key: %v", h)
	}
	p.AuthHeader = "bearer"
	if h := AuthHeaders(p); h["Authorization"] != "Bearer k" {
		t.Fatalf("bearer override: %v", h)
	}
	// Empty key sends the unauthenticated placeholder, never an empty header.
	p2 := &config.Provider{Name: "g", Dialect: config.DialectGemini, BaseURL: "https://x"}
	if h := AuthHeaders(p2); h["x-goog-api-key"] != "router-no-auth" {
		t.Fatalf("empty gemini key must use placeholder: %v", h)
	}
}

func TestOutboundNativeGeminiAndResponses(t *testing.T) {
	gem := &config.Provider{Name: "g", Dialect: config.DialectGemini, BaseURL: "https://x", APIKey: "s", Headers: map[string]string{}}
	in := http.Header{}
	in.Set("X-Custom", "keep")
	in.Set("Authorization", "Bearer client")
	in.Set("X-Goog-Api-Key", "client-key")
	in.Set("X-Session-Id", "sess")
	in.Set("Idempotency-Key", "k")
	out := OutboundHeaders(in, gem, config.DialectGemini, "", "", false)
	if out["X-Custom"] != "keep" {
		t.Fatalf("native custom header lost: %v", out)
	}
	if out["X-Goog-Api-Key"] != "s" {
		t.Fatalf("provider gemini credential missing: %v", out)
	}
	for _, k := range []string{"X-Session-Id", "X-Session-ID", "Idempotency-Key"} {
		if _, ok := out[k]; ok {
			t.Fatalf("session/idempotency forwarded: %v", out)
		}
	}
	resp := &config.Provider{Name: "r", Dialect: config.DialectOpenAIResponses, BaseURL: "https://x", APIKey: "s", Headers: map[string]string{}}
	out = OutboundHeaders(in, resp, config.DialectOpenAIResponses, "", "", false)
	if out["Authorization"] != "Bearer s" {
		t.Fatalf("responses auth wrong: %v", out)
	}
}
