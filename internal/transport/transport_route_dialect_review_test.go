package transport

import (
	"net/http"
	"strings"
	"testing"

	"github.com/arhammahajan/router/internal/config"
)

// rdReviewProvider builds a provider with a known default credential shape,
// one legacy translated header and framing-relevant statics.
func rdReviewProvider(dialect, authHeader string) *config.Provider {
	return &config.Provider{
		Name: "up", DefaultDialect: dialect, BaseURL: "https://up.example",
		APIKey: "k", Headers: map[string]string{}, Timeout: 5,
		AuthHeader:        authHeader,
		TranslatedHeaders: map[string]string{"X-Legacy": "legacy"},
	}
}

func rdReviewIngressCases() []struct{ ingress, src string } {
	return []struct{ ingress, src string }{
		{"chat", config.DialectOpenAIChat},
		{"responses", config.DialectOpenAIResponses},
		{"messages", config.DialectAnthropic},
		{"count_tokens", config.DialectAnthropic},
		{"gemini", config.DialectGemini},
	}
}

func TestRdReviewDestDrivesNativeClassification(t *testing.T) {
	dests := []string{config.DialectOpenAIChat, config.DialectOpenAIResponses, config.DialectAnthropic, config.DialectGemini}
	for _, tc := range rdReviewIngressCases() {
		for _, dest := range dests {
			p := rdReviewProvider(config.DialectOpenAIChat, "")
			in := http.Header{}
			in.Set("X-Ordinary", "keep")
			in.Set("Anthropic-Beta", "a-beta")
			in.Set("Openai-Beta", "o-beta")
			out := OutboundHeaders(in, p, tc.ingress, dest, "", false)
			native := tc.src == dest
			if native {
				if out["X-Ordinary"] != "keep" {
					t.Fatalf("%s->%s native must pass ordinary unknown: %v", tc.ingress, dest, out)
				}
				if _, ok := out["X-Legacy"]; ok {
					t.Fatalf("%s->%s native must not carry legacy: %v", tc.ingress, dest, out)
				}
				continue
			}
			if _, ok := out["X-Ordinary"]; ok {
				t.Fatalf("%s->%s bridged must drop ordinary unknown: %v", tc.ingress, dest, out)
			}
			if _, ok := out["X-Legacy"]; !ok {
				t.Fatalf("%s->%s translated must carry legacy: %v", tc.ingress, dest, out)
			}
			// Only the destination opt-in family survives.
			switch dest {
			case config.DialectAnthropic:
				if out["Anthropic-Beta"] != "a-beta" {
					t.Fatalf("anthropic dest must keep anthropic-beta: %v", out)
				}
				if _, ok := out["Openai-Beta"]; ok {
					t.Fatalf("anthropic dest must drop openai-beta: %v", out)
				}
				if out["Anthropic-Version"] == "" {
					t.Fatalf("anthropic dest must default version: %v", out)
				}
			case config.DialectOpenAIChat, config.DialectOpenAIResponses:
				if out["Openai-Beta"] != "o-beta" {
					t.Fatalf("%s dest must keep openai-beta: %v", dest, out)
				}
				if _, ok := out["Anthropic-Beta"]; ok {
					t.Fatalf("%s dest must drop anthropic-beta: %v", dest, out)
				}
				if _, ok := out["Anthropic-Version"]; ok {
					t.Fatalf("%s dest must not default anthropic-version: %v", dest, out)
				}
			case config.DialectGemini:
				if _, ok := out["Anthropic-Beta"]; ok {
					t.Fatalf("gemini dest must drop anthropic-beta: %v", out)
				}
				if _, ok := out["Openai-Beta"]; ok {
					t.Fatalf("gemini dest must drop openai-beta: %v", out)
				}
			}
		}
	}
}

func TestRdReviewAuthStaysProviderOwnedAllDest(t *testing.T) {
	type want struct{ key, val string }
	providers := map[string]want{
		config.DialectOpenAIChat:      {"Authorization", "Bearer k"},
		config.DialectOpenAIResponses: {"Authorization", "Bearer k"},
		config.DialectAnthropic:       {"X-Api-Key", "k"},
		config.DialectGemini:          {"X-Goog-Api-Key", "k"},
	}
	dests := []string{config.DialectOpenAIChat, config.DialectOpenAIResponses, config.DialectAnthropic, config.DialectGemini}
	for provDialect, w := range providers {
		for _, dest := range dests {
			p := rdReviewProvider(provDialect, "")
			out := OutboundHeaders(http.Header{}, p, "chat", dest, "", false)
			if out[w.key] != w.val {
				t.Fatalf("provider %s dest %s: got %v want %s=%s", provDialect, dest, out, w.key, w.val)
			}
			n := 0
			for name := range out {
				switch name {
				case "X-Api-Key", "Authorization", "Api-Key", "X-Goog-Api-Key":
					n++
				}
			}
			if n != 1 {
				t.Fatalf("provider %s dest %s: exactly one credential, got %v", provDialect, dest, out)
			}
		}
	}
	// Explicit auth_header is honored for every destination; no silent adapt.
	p := rdReviewProvider(config.DialectAnthropic, "bearer")
	out := OutboundHeaders(http.Header{}, p, "chat", config.DialectOpenAIChat, "", false)
	if out["Authorization"] != "Bearer k" {
		t.Fatalf("explicit bearer must win on chat dest: %v", out)
	}
	if _, ok := out["X-Api-Key"]; ok {
		t.Fatalf("explicit bearer must not leave x-api-key: %v", out)
	}
}

func TestRdReviewCredentialAndHopScrubAllDest(t *testing.T) {
	dests := []string{config.DialectOpenAIChat, config.DialectOpenAIResponses, config.DialectAnthropic, config.DialectGemini}
	for _, dest := range dests {
		p := rdReviewProvider(config.DialectOpenAIChat, "")
		in := http.Header{}
		in.Set("Authorization", "Bearer evil")
		in.Set("X-Api-Key", "evil")
		in.Set("X-Goog-Api-Key", "evil")
		in.Set("Api-Key", "evil")
		in.Set("Connection", "x-sneak")
		in.Set("X-Sneak", "sneak")
		in.Set("Idempotency-Key", "replay")
		in.Set("X-Idempotency-Key", "replay")
		out := OutboundHeaders(in, p, "chat", dest, "", false)
		for _, k := range []string{"X-Api-Key", "X-Goog-Api-Key", "Api-Key", "X-Sneak", "Idempotency-Key", "X-Idempotency-Key"} {
			if _, ok := out[k]; ok {
				t.Fatalf("dest %s: %s must be scrubbed: %v", dest, k, out)
			}
		}
		// The single surviving Authorization is the provider credential,
		// never the inbound client value.
		if out["Authorization"] != "Bearer k" {
			t.Fatalf("dest %s: single provider credential required: %v", dest, out)
		}
	}
}

func TestRdReviewUpstreamURLExactSuffixPerDest(t *testing.T) {
	cases := []struct{ base, dest, want string }{
		{"https://up.example", config.DialectOpenAIChat, "https://up.example/chat/completions"},
		{"https://up.example", config.DialectAnthropic, "https://up.example/v1/messages"},
		{"https://up.example", config.DialectOpenAIResponses, "https://up.example/responses"},
		{"https://up.example", config.DialectGemini, "https://up.example/v1beta"},
		// Primary base carrying another dialect's endpoint suffix is NOT
		// re-normalized: exact suffix rules apply per actual destination.
		{"https://up.example/chat/completions", config.DialectAnthropic, "https://up.example/chat/completions/v1/messages"},
		{"https://up.example/chat/completions", config.DialectOpenAIChat, "https://up.example/chat/completions"},
		{"https://up.example/chat/completions", config.DialectGemini, "https://up.example/chat/completions/v1beta"},
		{"https://up.example/v1/messages", config.DialectOpenAIChat, "https://up.example/v1/messages/chat/completions"},
		{"https://up.example/v1/messages", config.DialectAnthropic, "https://up.example/v1/messages"},
		{"https://up.example/v1", config.DialectAnthropic, "https://up.example/v1/messages"},
		{"https://up.example/v1/", config.DialectOpenAIChat, "https://up.example/v1/chat/completions"},
	}
	for _, tc := range cases {
		got, rerr := UpstreamURL(tc.base, tc.dest)
		if rerr != nil {
			t.Fatalf("UpstreamURL(%q,%q): %v", tc.base, tc.dest, rerr)
		}
		if got != tc.want {
			t.Fatalf("UpstreamURL(%q,%q)=%q want %q", tc.base, tc.dest, got, tc.want)
		}
	}
	if _, rerr := UpstreamURL("https://up.example", "sms"); rerr == nil {
		t.Fatal("unknown dest must fail, not guess")
	}
}

func TestRdReviewCountTokensURLAndGate(t *testing.T) {
	u, rerr := CountTokensURL("https://up.example")
	if rerr != nil {
		t.Fatal(rerr)
	}
	if u != "https://up.example/v1/messages/count_tokens" {
		t.Fatalf("count_tokens url=%q", u)
	}
	anth := rdReviewProvider(config.DialectAnthropic, "")
	anth.CountTokens = func() *bool { b := true; return &b }()
	if !CountTokensEnabled(anth) {
		t.Fatal("explicit count_tokens=true on anthropic provider must enable")
	}
	chat := rdReviewProvider(config.DialectOpenAIChat, "")
	if CountTokensEnabled(chat) {
		t.Fatal("chat provider must never count, even as an override destination")
	}
	off := rdReviewProvider(config.DialectAnthropic, "")
	off.CountTokens = func() *bool { b := false; return &b }()
	if CountTokensEnabled(off) {
		t.Fatal("count_tokens=false must disable")
	}
}

func TestRdReviewGeminiURLQueryAllowlist(t *testing.T) {
	u, rerr := GeminiURL("https://up.example", "flash", "generateContent", "")
	if rerr != nil {
		t.Fatal(rerr)
	}
	if strings.Contains(u, "?") {
		t.Fatalf("non-sse must carry no query: %s", u)
	}
	u, rerr = GeminiURL("https://up.example", "flash", "streamGenerateContent", "sse")
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !strings.HasSuffix(u, "?alt=sse") {
		t.Fatalf("sse alt must forward exactly: %s", u)
	}
	if _, rerr := GeminiURL("https://up.example", "../x", "generateContent", ""); rerr == nil {
		t.Fatal("traversal model must fail")
	}
}

func TestRdReviewSessionSingleIdentityBothModes(t *testing.T) {
	p := rdReviewProvider(config.DialectAnthropic, "")
	p.SessionHeader = "x-sess-out"
	p.SessionSourceHeaders = []string{"session-id"}
	in := http.Header{"Session-Id": {"s1"}}
	for _, dest := range []string{config.DialectAnthropic, config.DialectOpenAIChat} {
		out := OutboundHeaders(in, p, "messages", dest, "s1", true)
		if out["X-Sess-Out"] != "s1" {
			t.Fatalf("dest %s session: %v", dest, out)
		}
		if _, ok := out["Session-Id"]; ok {
			t.Fatalf("dest %s: source must not leak beside selection: %v", dest, out)
		}
		if out["Content-Type"] != "application/json" || out["Accept-Encoding"] != "identity" {
			t.Fatalf("dest %s framing: %v", dest, out)
		}
	}
}
