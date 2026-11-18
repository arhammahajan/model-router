package router

import (
	"testing"

	"github.com/arhammahajan/model-router/internal/capability"
	"github.com/arhammahajan/model-router/internal/config"
)

// Independent reverse review: same pinned snapshot supplies route, provider
// and canonical capability facts. Stable source: HEAD router_metadata
// empty/canonical/override/normalize. Hermetic programmatic config, no net/env.
func rvrStaticCfg() *config.Config {
	return &config.Config{
		Server: config.Server{},
		Providers: map[string]*config.Provider{
			"chat": {Name: "chat", Dialect: "openai-chat", BaseURL: "http://127.0.0.1:9"},
			"resp": {Name: "resp", Dialect: "openai-responses", BaseURL: "http://127.0.0.1:9"},
		},
		Models: []config.Model{
			{Name: "static-chat", Provider: "chat", Upstream: "u-chat"},
			{Name: "static-resp", Provider: "resp", Upstream: "u-resp"},
		},
	}
}

func TestRvrRouterAliasCanonicalOverride(t *testing.T) {
	cfg := rvrStaticCfg()
	cfg.Models = append(cfg.Models,
		config.Model{Name: "real-resp", Provider: "resp", Upstream: "up-resp", Aliases: []string{"alias-resp"}},
	)
	admitted, err := capability.ValidateOverrides(map[string]any{
		"real-resp":  map[string]any{"reasoning": map[string]any{"effort": map[string]any{"values": []any{"low", "medium"}}}},
		"alias-resp": map[string]any{"reasoning": map[string]any{"effort": map[string]any{"values": []any{"high"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg.CapabilityOverrides = admitted
	reg := NewRegistry(cfg)
	snap := reg.Current()
	m, p, fault := snap.Resolve("alias-resp")
	if fault != Found || m.Name != "real-resp" {
		t.Fatalf("alias must resolve to canonical: %+v %v", m, fault)
	}
	caps := capability.ReasoningFor(snap.RequestCapability(m, p))
	if !caps.EffortValues.Set || len(caps.EffortValues.List) != 2 {
		t.Fatalf("alias must inherit canonical, alias-key ignored: %+v", caps.EffortValues)
	}
}

func TestRvrRouterMetadataIgnoredAndClone(t *testing.T) {
	cfg := rvrStaticCfg()
	cfg.Models[1].Metadata = []byte(`{"router":{"reasoning":{"effort":{"values":["low"]}}}}`)
	reg := NewRegistry(cfg)
	snap := reg.Current()
	m, p, fault := snap.Resolve("static-resp")
	if fault != Found {
		t.Fatalf("resolve: %v", fault)
	}
	if got := capability.ReasoningFor(snap.RequestCapability(m, p)); got.EffortValues.Set {
		t.Fatalf("metadata.router must be ignored: %+v", got.EffortValues)
	}
	c1 := snap.RequestCapability(m, p)
	vals, _ := capability.StringListValue([]string{"MUT"})
	s := "mut"
	_ = c1.Put("reasoning.effort.values", vals, &s, capability.KindOverride)
	c2 := snap.RequestCapability(m, p)
	if got := capability.ReasoningFor(c2); got.EffortValues.Set {
		t.Fatalf("snapshot must own facts: %+v", got.EffortValues)
	}
}

func TestRvrRouterEffectiveDialectOverride(t *testing.T) {
	cfg := rvrStaticCfg()
	cfg.Models = append(cfg.Models,
		config.Model{Name: "over-resp", Provider: "chat", Upstream: "up-over", Dialect: "openai-responses"},
	)
	reg := NewRegistry(cfg)
	snap := reg.Current()
	m, p, fault := snap.Resolve("over-resp")
	if fault != Found {
		t.Fatal("resolve")
	}
	if got := m.EffectiveDialect(p); got != "openai-responses" {
		t.Fatalf("route override must select dest: %q", got)
	}
	if p.Name != "chat" {
		t.Fatalf("provider identity must not change: %+v", p)
	}
}
