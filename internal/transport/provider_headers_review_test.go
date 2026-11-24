package transport

import (
	"net/http"
	"strings"
	"testing"

	"github.com/arhammahajan/router/internal/config"
)

func reviewProvider() *config.Provider {
	return &config.Provider{
		Name:    "up",
		DefaultDialect: config.DialectAnthropic,
		BaseURL: "https://up.example",
		APIKey:  "real-key",
		Headers: map[string]string{
			"Anthropic-Beta": "prov",
			"X-Shared":       "prov",
			"X-Prov-Only":    "prov",
		},
		TranslatedHeaders: map[string]string{
			"anthropic-beta": "legacy",
			"x-shared":       "legacy",
			"X-Legacy-Only":  "legacy",
		},
		Timeout: 5,
	}
}

// Full chain on a translated request, case-insensitively:
// inbound opt-in < legacy < provider.headers < single credential; the
// selected session identity beats every static.
func TestReviewFourLayerPrecedence(t *testing.T) {
	p := reviewProvider()
	p.SessionHeader = "x-sess-out"
	p.SessionSourceHeaders = []string{"session-id"}
	p.Headers["X-Sess-Out"] = "prov-static"
	p.TranslatedHeaders["X-SESS-OUT"] = "legacy-static"

	in := http.Header{}
	in.Set("Anthropic-Beta", "inbound")
	in.Set("Session-Id", "s1")
	in.Set("X-Sess-Out", "smuggled")

	out := OutboundHeaders(in, p, "chat", "", "real1", true)
	if out["Anthropic-Beta"] != "prov" {
		t.Fatalf("provider must beat legacy+inbound: %v", out)
	}
	if out["X-Shared"] != "prov" {
		t.Fatalf("provider must beat legacy: %v", out)
	}
	if out["X-Legacy-Only"] != "legacy" {
		t.Fatalf("legacy-only must survive: %v", out)
	}
	if out["X-Sess-Out"] != "real1" {
		t.Fatalf("selected session must beat all statics: %v", out)
	}
	if _, ok := out["Session-Id"]; ok {
		t.Fatalf("native identity must be consumed: %v", out)
	}
	// Single credential: anthropic destination uses x-api-key.
	if out["X-Api-Key"] != "real-key" {
		t.Fatalf("single selected credential missing: %v", out)
	}
	for name := range out {
		if strings.EqualFold(name, "Authorization") {
			t.Fatalf("second credential must not survive: %v", out)
		}
	}
}

// Native and count_tokens ingress must ignore TranslatedHeaders entirely
// while keeping provider headers and ordinary inbound forwarding.
func TestReviewNativeAndCountTokensIgnoreLegacy(t *testing.T) {
	p := reviewProvider()
	in := http.Header{}
	in.Set("X-Inbound", "client")
	in.Set("Anthropic-Beta", "inbound")

	for _, ingress := range []string{"messages", "count_tokens"} {
		out := OutboundHeaders(in, p, ingress, "", "", false)
		if _, ok := out["X-Legacy-Only"]; ok {
			t.Fatalf("%s must not carry legacy: %v", ingress, out)
		}
		for name, v := range out {
			if strings.EqualFold(name, "anthropic-beta") && v == "legacy" {
				t.Fatalf("%s legacy must not override: %v", ingress, out)
			}
		}
		if out["X-Prov-Only"] != "prov" {
			t.Fatalf("%s must keep provider headers: %v", ingress, out)
		}
		if out["X-Inbound"] != "client" {
			t.Fatalf("%s native must forward ordinary inbound: %v", ingress, out)
		}
	}
}

// Client-controlled hop nomination, idempotency keys and wrong-dialect
// opt-ins must drop on translated traffic even with legacy configured;
// trusted legacy still rides.
func TestReviewClientIsolationWithLegacy(t *testing.T) {
	p := reviewProvider()
	in := http.Header{}
	in.Set("Connection", "X-Nominated")
	in.Set("X-Nominated", "client")
	in.Set("Idempotency-Key", "client-key")
	in.Set("X-Idempotency-Key", "client-key2")
	in.Set("Openai-Beta", "client-wrong-dialect")
	in.Set("Anthropic-Beta", "inbound-optin")

	out := OutboundHeaders(in, p, "chat", "", "", false)
	for name := range out {
		lower := strings.ToLower(name)
		if lower == "x-nominated" {
			t.Fatalf("Connection-nominated header must drop: %v", out)
		}
		if lower == "idempotency-key" || lower == "x-idempotency-key" {
			t.Fatalf("client idempotency must not forward: %v", out)
		}
		if lower == "openai-beta" {
			t.Fatalf("wrong-dialect inbound must drop: %v", out)
		}
	}
	if out["X-Legacy-Only"] != "legacy" {
		t.Fatalf("trusted legacy must survive isolation: %v", out)
	}
	// Inbound opt-in loses to legacy loses to provider (covered above, but
	// pin the inbound<legacy link explicitly here).
	if out["Anthropic-Beta"] != "prov" {
		t.Fatalf("provider must win the opt-in chain: %v", out)
	}
	p2 := &config.Provider{
		Name: "up", DefaultDialect: config.DialectAnthropic,
		BaseURL: "https://x", APIKey: "k", Timeout: 5,
		TranslatedHeaders: map[string]string{"X-Legacy": "v"},
	}
	in2 := http.Header{}
	in2.Set("Anthropic-Beta", "inbound-only")
	out2 := OutboundHeaders(in2, p2, "chat", "", "", false)
	if out2["Anthropic-Beta"] != "inbound-only" {
		t.Fatalf("inbound opt-in without legacy/provider contention must forward: %v", out2)
	}
}

// Auth-name statics in every layer collapse to exactly one wire credential,
// case-insensitively, while non-credential legacy survives.
func TestReviewAuthScrubAllLayers(t *testing.T) {
	p := &config.Provider{
		Name:    "up",
		DefaultDialect: config.DialectAnthropic,
		BaseURL: "https://up.example",
		APIKey:  "real-key",
		Headers: map[string]string{
			"AUTHORIZATION": "Bearer prov-stale",
			"X-Prov":        "prov",
		},
		TranslatedHeaders: map[string]string{
			"authorization": "Bearer legacy-stale",
			"X-API-KEY":     "legacy-stale",
			"X-Legacy":      "v",
		},
		Timeout: 5,
	}
	out := OutboundHeaders(http.Header{}, p, "chat", "", "", false)
	if out["X-Api-Key"] != "real-key" {
		t.Fatalf("single selected credential missing: %v", out)
	}
	n := 0
	for name := range out {
		switch strings.ToLower(name) {
		case "authorization", "x-api-key", "api-key", "x-goog-api-key":
			n++
		}
	}
	if n != 1 {
		t.Fatalf("exactly one wire credential required, got %v", out)
	}
	if out["X-Legacy"] != "v" || out["X-Prov"] != "prov" {
		t.Fatalf("non-credential trusted headers must survive scrub: %v", out)
	}
}

// Framing is enforced last: legacy or provider statics cannot override the
// serialized JSON body's Content-Type or the identity Accept-Encoding.
func TestReviewFramingEnforcedOverLegacy(t *testing.T) {
	p := &config.Provider{
		Name:    "up",
		DefaultDialect: config.DialectAnthropic,
		BaseURL: "https://up.example",
		APIKey:  "real-key",
		Headers: map[string]string{
			"Content-Type":    "text/evil",
			"Accept-Encoding": "gzip",
		},
		TranslatedHeaders: map[string]string{
			"content-type":    "text/legacy-evil",
			"accept-encoding": "br",
		},
		Timeout: 5,
	}
	out := OutboundHeaders(http.Header{}, p, "chat", "", "", false)
	if out["Content-Type"] != "application/json" {
		t.Fatalf("Content-Type must be enforced: %v", out)
	}
	if out["Accept-Encoding"] != "identity" {
		t.Fatalf("Accept-Encoding must be enforced: %v", out)
	}
}
