package router

import (
	"testing"

	"github.com/arhammahajan/router/internal/capability"
	"github.com/arhammahajan/router/internal/config"
)

// Independent adversarial review for Anthropic Off snapshot pinning.
// Scope: same pinned p.snap -> RequestCapability canonical base+override+
// Normalize -> ReasoningFor; aliases/dynamic/stale/restore no repin;
// provider auth identity unchanged; metadata.router ignored.

func offRevStaticCfg() *config.Config {
	return &config.Config{
		Server: config.Server{},
		Providers: map[string]*config.Provider{
			"chat": {Name: "chat", Dialect: "openai-chat", BaseURL: "http://127.0.0.1:9", APIKey: "chat-secret"},
			"resp": {Name: "resp", Dialect: "openai-responses", BaseURL: "http://127.0.0.1:9", APIKey: "resp-secret"},
		},
		Models: []config.Model{
			{Name: "static-chat", Provider: "chat", Upstream: "u-chat"},
			{Name: "static-resp", Provider: "resp", Upstream: "u-resp"},
			{Name: "real-off", Provider: "chat", Upstream: "u-off", Aliases: []string{"alias-off"}},
			{Name: "off-model", Provider: "chat", Upstream: "u-off2", Disabled: true},
		},
	}
}

func TestOffRevCanonicalMatchesListing(t *testing.T) {
	reg := NewRegistry(offRevStaticCfg())
	snap := reg.Current()
	for _, lm := range snap.ListedModels() {
		eff := lm.Model.EffectiveDialect(lm.Provider)
		var want capability.Contract
		switch lm.Source {
		case "catalog":
			if b, ok := snap.CatalogBase(lm.Model.Name); ok {
				want = b
			} else {
				want = capability.Empty(eff)
			}
		case "llama":
			if b, ok := snap.LlamaBase(lm.Model.Name); ok {
				want = b
			} else {
				want = capability.Empty(eff)
			}
		default:
			want = capability.Empty(eff)
		}
		got := snap.CanonicalBase(lm.Model.Name, eff)
		wj, _ := want.MarshalJSON()
		gj, _ := got.MarshalJSON()
		if string(wj) != string(gj) {
			t.Fatalf("CanonicalBase must match listing for %s", lm.Model.Name)
		}
	}
}

func TestOffRevAliasDynamicDisabled(t *testing.T) {
	cfg := offRevStaticCfg()
	cfg.Server.AllowDynamicRoutes = true
	admitted, err := capability.ValidateOverrides(map[string]any{
		"real-off":  map[string]any{"reasoning": map[string]any{"effort": map[string]any{"values": []any{"low", "none"}}}},
		"alias-off": map[string]any{"reasoning": map[string]any{"effort": map[string]any{"values": []any{"high"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg.CapabilityOverrides = admitted
	reg := NewRegistry(cfg)
	snap := reg.Current()
	m, p, fault := snap.Resolve("alias-off")
	if fault != Found || m.Name != "real-off" {
		t.Fatalf("alias must resolve to canonical: %+v %v", m, fault)
	}
	caps := capability.ReasoningFor(snap.RequestCapability(m, p))
	if !caps.EffortValues.Set || len(caps.EffortValues.List) != 2 {
		t.Fatalf("alias inherits canonical override only: %+v", caps.EffortValues)
	}
	if _, _, fault := snap.Resolve("off-model"); fault != DisabledReserved {
		t.Fatalf("disabled reserved: %v", fault)
	}
	md, pd, fault := snap.Resolve("chat/dyn-model")
	if fault != Found {
		t.Fatalf("dynamic resolve: %v", fault)
	}
	if got := md.EffectiveDialect(pd); got != "openai-chat" {
		t.Fatalf("dynamic inherits provider dest: %q", got)
	}
	if _, ok := snap.CatalogBase("chat/dyn-model"); ok {
		t.Fatal("dynamic base unknown")
	}
	if pd.APIKey != "chat-secret" || pd.Name != "chat" {
		t.Fatalf("dynamic provider identity: %+v", pd)
	}
}

func TestOffRevMetadataRouterIgnored(t *testing.T) {
	cfg := offRevStaticCfg()
	meta := []byte(`{"router":{"reasoning":{"effort":{"values":["low"]}}}}`)
	for i := range cfg.Models {
		if cfg.Models[i].Name == "static-chat" {
			cfg.Models[i].Metadata = append([]byte(nil), meta...)
		}
	}
	reg := NewRegistry(cfg)
	snap := reg.Current()
	m, p, fault := snap.Resolve("static-chat")
	if fault != Found {
		t.Fatalf("resolve: %v", fault)
	}
	if got := capability.ReasoningFor(snap.RequestCapability(m, p)); got.EffortValues.Set {
		t.Fatalf("metadata.router ignored: %+v", got.EffortValues)
	}
}

func TestOffRevPinningNoRepin(t *testing.T) {
	reg := NewRegistry(offRevStaticCfg())
	snap1 := reg.Current()
	m, p, _ := snap1.Resolve("static-chat")
	c1 := snap1.RequestCapability(m, p)
	vals, _ := capability.StringListValue([]string{"MUT"})
	s := "mut"
	_ = c1.Put("reasoning.effort.values", vals, &s, capability.KindOverride)
	c2 := snap1.RequestCapability(m, p)
	if got := capability.ReasoningFor(c2); got.EffortValues.Set {
		t.Fatalf("snapshot owns facts: %+v", got.EffortValues)
	}
	admitted, err := capability.ValidateOverrides(map[string]any{
		"static-chat": map[string]any{"reasoning": map[string]any{"effort": map[string]any{"values": []any{"low"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg2 := offRevStaticCfg()
	cfg2.CapabilityOverrides = admitted
	reg2 := NewRegistry(cfg2)
	snap2 := reg2.Current()
	m2, p2, _ := snap2.Resolve("static-chat")
	if got := capability.ReasoningFor(snap2.RequestCapability(m2, p2)); !got.EffortValues.Set {
		t.Fatalf("new snapshot carries new facts")
	}
	if got := capability.ReasoningFor(snap1.RequestCapability(m, p)); got.EffortValues.Set {
		t.Fatalf("pinned snapshot retains old facts")
	}
}

func TestOffRevSupportedFalseNormalizePin(t *testing.T) {
	cfg := offRevStaticCfg()
	admitted, err := capability.ValidateOverrides(map[string]any{
		"static-chat": map[string]any{"reasoning": map[string]any{"supported": false}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg.CapabilityOverrides = admitted
	reg := NewRegistry(cfg)
	snap := reg.Current()
	m, p, fault := snap.Resolve("static-chat")
	if fault != Found {
		t.Fatalf("resolve: %v", fault)
	}
	caps := capability.ReasoningFor(snap.RequestCapability(m, p))
	if caps.Supported == nil || *caps.Supported {
		t.Fatalf("supported false pinned: %+v", caps.Supported)
	}
	if caps.ToggleSupported != nil && *caps.ToggleSupported {
		t.Fatalf("Normalize must suppress toggle positive: %+v", caps.ToggleSupported)
	}
	if caps.EffortValues.Set {
		t.Fatalf("Normalize withdraws values: %+v", caps.EffortValues)
	}
}

func TestOffRevDestVsIdentity(t *testing.T) {
	cfg := offRevStaticCfg()
	cfg.Models = append(cfg.Models, config.Model{Name: "over-resp", Provider: "chat", Upstream: "up-over", Dialect: "openai-responses"})
	reg := NewRegistry(cfg)
	snap := reg.Current()
	mo, po, fault := snap.Resolve("over-resp")
	if fault != Found {
		t.Fatal("resolve")
	}
	if got := mo.EffectiveDialect(po); got != "openai-responses" {
		t.Fatalf("route dest: %q", got)
	}
	if po.Dialect != "openai-chat" || po.Name != "chat" || po.APIKey != "chat-secret" {
		t.Fatalf("provider identity unchanged: %+v", po)
	}
}
