package router

import (
	"testing"

	"github.com/arhammahajan/model-router/internal/config"
)

// rdReviewSnapshotConfig covers an override route with an alias, a disabled
// reservation and a dynamic-capable provider.
func rdReviewSnapshotConfig() *config.Config {
	return &config.Config{
		Server: config.Server{AllowDynamicRoutes: true},
		Providers: map[string]*config.Provider{
			"chatup": {Name: "chatup", Dialect: config.DialectOpenAIChat, BaseURL: "https://up.example", APIKey: "k"},
			"anthup": {Name: "anthup", Dialect: config.DialectAnthropic, BaseURL: "https://up.example", APIKey: "k"},
		},
		Models: []config.Model{
			{Name: "chat-to-anthropic", Provider: "chatup", Upstream: "claude", Dialect: config.DialectAnthropic, Aliases: []string{"cta-alias"}},
			{Name: "chatup/shadowed", Provider: "chatup", Upstream: "x", Dialect: config.DialectGemini, Disabled: true, Aliases: []string{"dis-alias"}},
		},
	}
}

func TestRdReviewDynamicUsesProviderDefault(t *testing.T) {
	snap := New(rdReviewSnapshotConfig())
	for _, tc := range []struct {
		name, provider, dest string
	}{
		{"chatup/gpt-4o-mini", "chatup", config.DialectOpenAIChat},
		{"anthup/claude-x", "anthup", config.DialectAnthropic},
	} {
		m, p, fault := snap.Resolve(tc.name)
		if fault != Found {
			t.Fatalf("dynamic %s fault=%v", tc.name, fault)
		}
		if m.Dialect != "" {
			t.Fatalf("dynamic must carry no dest, got %q", m.Dialect)
		}
		if got := m.EffectiveDialect(p); got != tc.dest {
			t.Fatalf("dynamic %s dest=%q want %q", tc.name, got, tc.dest)
		}
		if p.Dialect != tc.dest {
			t.Fatalf("dynamic %s provider=%q", tc.name, p.Dialect)
		}
	}
}

func TestRdReviewAliasInheritsDisabledReserved(t *testing.T) {
	snap := New(rdReviewSnapshotConfig())
	m, p, fault := snap.Resolve("cta-alias")
	if fault != Found {
		t.Fatalf("alias fault=%v", fault)
	}
	if got := m.EffectiveDialect(p); got != config.DialectAnthropic {
		t.Fatalf("alias dest=%q", got)
	}
	if p.Dialect != config.DialectOpenAIChat {
		t.Fatalf("alias must not mutate provider identity: %q", p.Dialect)
	}
	for _, name := range []string{"chatup/shadowed", "dis-alias"} {
		if _, _, fault := snap.Resolve(name); fault != DisabledReserved {
			t.Fatalf("%s must stay reserved, got %v", name, fault)
		}
	}
	// A disabled explicit name wins over dynamic routing for the same
	// public name (no fallthrough), while unrelated dynamic names work.
	if _, _, fault := snap.Resolve("chatup/other"); fault != Found {
		t.Fatalf("unrelated dynamic must resolve, got %v", fault)
	}
}

func TestRdReviewResolveCopiesIsolateDest(t *testing.T) {
	snap := New(rdReviewSnapshotConfig())
	m, p, fault := snap.Resolve("chat-to-anthropic")
	if fault != Found {
		t.Fatalf("fault=%v", fault)
	}
	m.Dialect = config.DialectGemini
	m.Upstream = "mutated"
	p.Dialect = "mutated"
	m2, p2, fault := snap.Resolve("chat-to-anthropic")
	if fault != Found {
		t.Fatalf("fault=%v", fault)
	}
	if got := m2.EffectiveDialect(p2); got != config.DialectAnthropic {
		t.Fatalf("snapshot dest mutated: %q", got)
	}
	if p2.Dialect != config.DialectOpenAIChat || m2.Upstream != "claude" {
		t.Fatalf("snapshot state mutated: %+v %+v", m2, p2)
	}
	// Listing copies carry the destination and isolate too.
	found := false
	for _, lm := range snap.Models() {
		if lm.Name == "chat-to-anthropic" {
			found = true
			if lm.Dialect != config.DialectAnthropic {
				t.Fatalf("listing dest=%q", lm.Dialect)
			}
			lm.Dialect = "mutated"
		}
	}
	if !found {
		t.Fatal("listing must include override route")
	}
	m3, p3, _ := snap.Resolve("chat-to-anthropic")
	if m3.EffectiveDialect(p3) != config.DialectAnthropic {
		t.Fatal("listing mutation leaked into snapshot")
	}
}
