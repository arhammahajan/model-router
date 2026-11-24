package transport

// Session emission contracts: exactly one upstream identity under the
// provider destination on native and bridged requests; nothing forwarded
// without a selection; statics/casing/Connection nomination cannot smuggle
// a second identity; auth and framing stay intact.

import (
	"net/http"
	"testing"

	"github.com/arhammahajan/router/internal/config"
)

func sessProvider() *config.Provider {
	b := true
	_ = b
	return &config.Provider{
		Name:                 "up",
		DefaultDialect:       config.DialectAnthropic,
		BaseURL:              "http://127.0.0.1:9",
		APIKey:               "k",
		Headers:              map[string]string{},
		SessionHeader:        "x-session-out",
		SessionSourceHeaders: []string{"session-id"},
		SessionMissing:       "omit",
	}
}

func TestOutboundEmitsSingleSessionNative(t *testing.T) {
	p := sessProvider()
	in := http.Header{"Session-Id": {"s1"}, "Content-Type": {"application/json"}}
	out := OutboundHeaders(in, p, "messages", "", "s1", true)
	if out["X-Session-Out"] != "s1" {
		t.Fatalf("must emit selected identity: %v", out)
	}
	for name := range out {
		if name == "Session-Id" || name == "Session-ID" {
			t.Fatalf("native identity must be consumed: %v", out)
		}
	}
	vals := 0
	for name, v := range out {
		if len(v) == 0 {
			continue
		}
		_ = v
		if name == "X-Session-Out" {
			vals++
		}
	}
	if vals != 1 {
		t.Fatalf("exactly one identity: %v", out)
	}
}

func TestOutboundNoSessionForwardsNothing(t *testing.T) {
	p := sessProvider()
	in := http.Header{"Session-Id": {"s1"}, "X-Opencode-Session": {"o1"}}
	out := OutboundHeaders(in, p, "messages", "", "", false)
	for name := range out {
		switch name {
		case "Session-Id", "X-Session-Out", "X-Opencode-Session":
			t.Fatalf("no identity without selection: %v", out)
		}
	}
}

func TestOutboundStaticCannotSmuggleSecondIdentity(t *testing.T) {
	p := sessProvider()
	p.Headers = map[string]string{"X-Session-Out": "static", "X-Opencode-Session": "static"}
	in := http.Header{"Session-Id": {"s1"}}
	out := OutboundHeaders(in, p, "messages", "", "s1", true)
	if out["X-Session-Out"] != "s1" {
		t.Fatalf("selection must win over statics: %v", out)
	}
	if _, ok := out["X-Opencode-Session"]; ok {
		t.Fatalf("router-owned token must never go upstream: %v", out)
	}
}

func TestOutboundBridgedEmitsSessionWithOptInsOnly(t *testing.T) {
	p := sessProvider() // anthropic destination
	in := http.Header{"Session-Id": {"s1"}, "Openai-Beta": {"assistants=v2"}, "X-Custom": {"c"}}
	out := OutboundHeaders(in, p, "chat", "", "s1", true)
	if out["X-Session-Out"] != "s1" {
		t.Fatalf("bridged emission: %v", out)
	}
	if _, ok := out["X-Custom"]; ok {
		t.Fatalf("bridged must keep opt-ins only: %v", out)
	}
}

func TestOutboundNominatedSessionDroppedAuthIntact(t *testing.T) {
	p := sessProvider()
	in := http.Header{"X-Session-Out": {"smuggled"}, "Connection": {"x-session-out"}}
	out := OutboundHeaders(in, p, "messages", "", "real1", true)
	if out["X-Session-Out"] != "real1" {
		t.Fatalf("nomination must not smuggle, selection wins: %v", out)
	}
	if out["X-Api-Key"] != "k" {
		t.Fatalf("auth intact: %v", out)
	}
	if out["Content-Type"] != "application/json" {
		t.Fatalf("framing intact: %v", out)
	}
}

func TestOutboundSelectionFlagGatesEmission(t *testing.T) {
	// Mutation guard: a non-empty ID with hasSession=false must emit
	// nothing. Only a real selection authorizes an upstream identity.
	p := sessProvider()
	in := http.Header{"Session-Id": {"s1"}}
	out := OutboundHeaders(in, p, "messages", "", "smuggled", false)
	for name := range out {
		switch name {
		case "Session-Id", "X-Session-Out", "X-Opencode-Session":
			t.Fatalf("unselected identity forwarded: %v", out)
		}
	}
}
