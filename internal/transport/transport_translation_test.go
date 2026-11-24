package transport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/arhammahajan/router/internal/config"
)

func legacyProvider(dialect string) *config.Provider {
	return &config.Provider{
		Name:              "up",
		DefaultDialect:    dialect,
		BaseURL:           "https://up.example",
		APIKey:            "k",
		Headers:           map[string]string{},
		TranslatedHeaders: map[string]string{"X-Legacy": "legacy", "X-Shared": "legacy-shared"},
		Timeout:           5,
	}
}

func TestTranslatedLegacyOnlyOnTranslated(t *testing.T) {
	p := legacyProvider(config.DialectAnthropic)
	// Native: same dialect ingress must not carry legacy.
	native := OutboundHeaders(http.Header{}, p, "messages", "", "", false)
	if _, ok := native["X-Legacy"]; ok {
		t.Fatalf("native must not carry legacy translation headers: %v", native)
	}
	// Translated: chat ingress to anthropic must carry legacy.
	bridged := OutboundHeaders(http.Header{}, p, "chat", "", "", false)
	if bridged["X-Legacy"] != "legacy" {
		t.Fatalf("translated must carry legacy: %v", bridged)
	}
}

func TestTranslatedPrecedenceAllCases(t *testing.T) {
	p := &config.Provider{
		Name:           "up",
		DefaultDialect: config.DialectAnthropic,
		BaseURL:        "https://up.example",
		APIKey:         "k",
		Headers: map[string]string{
			"X-Shared":       "prov",
			"Anthropic-Beta": "prov-beta",
			"X-Prov-Only":    "prov",
		},
		TranslatedHeaders: map[string]string{
			"X-Shared":       "legacy",
			"Anthropic-Beta": "legacy-beta",
			"X-Legacy-Only":  "legacy",
		},
		Timeout: 5,
	}
	in := http.Header{}
	in.Set("Anthropic-Beta", "inbound-beta")
	in.Set("X-Inbound-Optin", "inbound")
	// Anthropic dest, chat ingress => translated; inbound X-* non-opt-in drops.
	out := OutboundHeaders(in, p, "chat", "", "", false)
	if out["X-Shared"] != "prov" {
		t.Fatalf("provider.headers must beat legacy: %v", out)
	}
	if out["Anthropic-Beta"] != "prov-beta" {
		t.Fatalf("provider must beat legacy+inbound (case-insensitive): %v", out)
	}
	if out["X-Legacy-Only"] != "legacy" {
		t.Fatalf("legacy-only must survive: %v", out)
	}
	if out["X-Prov-Only"] != "prov" {
		t.Fatalf("provider-only must survive: %v", out)
	}
	// Inbound non-opt-in must not forward on translated.
	if _, ok := out["X-Inbound-Optin"]; ok {
		t.Fatalf("non-opt-in inbound must drop on translated: %v", out)
	}
}

func TestTranslatedStaticDoubleCredentialsScrub(t *testing.T) {
	p := &config.Provider{
		Name:           "up",
		DefaultDialect: config.DialectAnthropic,
		BaseURL:        "https://up.example",
		APIKey:         "real-key",
		Headers:        map[string]string{"Authorization": "Bearer stale"},
		TranslatedHeaders: map[string]string{
			"Authorization": "Bearer legacy-stale",
			"X-Api-Key":     "stale-key",
			"X-Legacy":      "v",
		},
		Timeout: 5,
	}
	out := OutboundHeaders(http.Header{}, p, "chat", "", "", false)
	if out["X-Api-Key"] != "real-key" {
		t.Fatalf("single selected credential missing: %v", out)
	}
	if v := out["Authorization"]; v != "" {
		t.Fatalf("second credential must be scrubbed, got Authorization=%q in %v", v, out)
	}
	count := 0
	for name := range out {
		switch name {
		case "X-Api-Key", "Authorization", "Api-Key", "X-Goog-Api-Key":
			count++
		}
	}
	if count != 1 {
		t.Fatalf("exactly one wire credential required, got %v", out)
	}
	if out["X-Legacy"] != "v" {
		t.Fatalf("non-credential legacy must survive scrub: %v", out)
	}
}

func TestTranslatedWrongOptinClientIsolation(t *testing.T) {
	p := legacyProvider(config.DialectAnthropic)
	p.TranslatedHeaders["Openai-Beta"] = "legacy-openai"
	in := http.Header{}
	in.Set("Openai-Beta", "client-openai")
	out := OutboundHeaders(in, p, "chat", "", "", false)
	if v := out["Openai-Beta"]; v != "legacy-openai" {
		t.Fatalf("legacy trusted openai-beta must survive (provider trusts), inbound wrong-dialect must not win: got %q in %v", v, out)
	}
	// Without legacy, wrong-dialect inbound must drop.
	p2 := &config.Provider{Name: "up", DefaultDialect: config.DialectAnthropic, BaseURL: "https://x", APIKey: "k", Timeout: 5}
	out2 := OutboundHeaders(in, p2, "chat", "", "", false)
	if _, ok := out2["Openai-Beta"]; ok {
		t.Fatalf("wrong-dialect inbound must drop without legacy: %v", out2)
	}
}

func TestTranslatedSessionAllSourceIsolation(t *testing.T) {
	p := &config.Provider{
		Name:                 "up",
		DefaultDialect:       config.DialectAnthropic,
		BaseURL:              "https://x",
		APIKey:               "k",
		Headers:              map[string]string{"X-Session-Out": "static-prov", "X-Custom": "v"},
		TranslatedHeaders:    map[string]string{"X-Session-Out": "static-legacy", "X-Legacy": "v"},
		SessionHeader:        "x-session-out",
		SessionSourceHeaders: []string{"session-id"},
		Timeout:              5,
	}
	in := http.Header{"Session-Id": {"s1"}, "X-Session-Out": {"smuggled"}}
	out := OutboundHeaders(in, p, "chat", "", "real1", true)
	if out["X-Session-Out"] != "real1" {
		t.Fatalf("selection must win over all statics: %v", out)
	}
	// Exactly one session identity.
	n := 0
	for name := range out {
		if name == "X-Session-Out" {
			n++
		}
		if name == "Session-Id" {
			t.Fatalf("native identity must be consumed: %v", out)
		}
	}
	if n != 1 {
		t.Fatalf("exactly one session identity: %v", out)
	}
}

func TestTranslatedRetryIdentityStable(t *testing.T) {
	// Retry policy is unchanged by legacy headers: same provider/model,
	// same headers on every attempt, one retry then success.
	var calls atomic.Int32
	var gotLegacy atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`busy`))
			return
		}
		if r.Header.Get("X-Legacy") == "legacy" {
			gotLegacy.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()
	p := legacyProvider(config.DialectOpenAIChat)
	p.BaseURL = up.URL
	retry := config.Retry{MaxAttempts: 3, Statuses: []int{429}, ConnectErrors: true, BackoffBase: 0.001, BackoffMax: 0.002, Deadline: 5}
	hdrs := OutboundHeaders(http.Header{}, p, "messages", "", "", false)
	if hdrs["X-Legacy"] != "legacy" {
		t.Fatalf("translated fixture must carry legacy: %v", hdrs)
	}
	c := NewClient()
	res, rerr := c.Do(context.Background(), p, retry, up.URL, hdrs, []byte(`{}`), false, 1<<20)
	if rerr != nil || res == nil || res.Status != 200 {
		t.Fatalf("retry must succeed with legacy headers: res=%v err=%v", res, rerr)
	}
	if calls.Load() != 2 {
		t.Fatalf("retry identity must stay same-provider same-headers, got %d calls", calls.Load())
	}
	if gotLegacy.Load() != 1 {
		t.Fatalf("retry must reuse identical headers, legacy seen %d", gotLegacy.Load())
	}
}
