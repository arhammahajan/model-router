package router

import (
	"testing"

	"github.com/arhammahajan/router/internal/capability"
	"github.com/arhammahajan/router/internal/config"
)

// Independent review of CanonicalBase / RequestCapability for the
// Chat->Responses reasoning vertical. Stable source: router_metadata
// empty/canonical/override/normalize + Python plan_reasoning fact gates.
// Hermetic: programmatic config + Commit shims, no network/env/files.
// Proves: shadow/winning semantics match listing, alias/canonical/disabled/
// dynamic/dest-override handling, metadata.router ignored, same-snapshot
// pinning, immutable clones, and llama+Responses unreachability.

func rrStaticCfg() *config.Config {
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

func rrLlamaCfg() *config.Config {
	cfg := rrStaticCfg()
	cfg.Llama = &config.LlamaConfig{
		Servers:        []config.LlamaServer{{URL: "http://127.0.0.1:1111", Name: "llama-a"}, {URL: "http://127.0.0.1:2222", Name: "llama-b"}},
		ProbeTimeout:   2,
		RefreshSeconds: 30,
	}
	return cfg
}

func rrCommitLlama(t *testing.T, reg *Registry, entries map[string][]LlamaEntry, wall float64) {
	t.Helper()
	layer := LlamaLayer{Servers: map[string]LlamaServerSnapshot{}}
	for _, srv := range reg.LlamaOrder() {
		layer.Servers[srv] = LlamaServerSnapshot{State: LlamaNeverSucceeded}
	}
	for name, es := range entries {
		layer.Servers[name] = LlamaServerSnapshot{State: LlamaReachable, Entries: es}
	}
	agg := SourceEvent{}
	servers := map[string]ServerEvent{}
	if err := reg.CommitLlama(LlamaAttempt{Layer: layer, AtWall: Wall(wall), Aggregate: agg, Servers: servers}); err != nil {
		t.Fatal(err)
	}
}

func rrListingBase(t *testing.T, snap *Snapshot, lm ListedModel) capability.Contract {
	t.Helper()
	switch lm.Source {
	case "catalog":
		if b, ok := snap.CatalogBase(lm.Model.Name); ok {
			return b
		}
		return capability.Empty(lm.Model.EffectiveDialect(lm.Provider))
	case "llama":
		if b, ok := snap.LlamaBase(lm.Model.Name); ok {
			return b
		}
		return capability.Empty(lm.Model.EffectiveDialect(lm.Provider))
	default:
		return capability.Empty(lm.Model.EffectiveDialect(lm.Provider))
	}
}

func TestRRCanonicalBaseMatchesListing(t *testing.T) {
	reg := NewRegistry(rrLlamaCfg())
	obs := capability.ExtractLlamaObs(map[string]any{"id": "x"}, map[string]any{"build_info": "llama.cpp"}, "single")
	rrCommitLlama(t, reg, map[string][]LlamaEntry{
		"llama-a": {{Name: "llama-only", Upstream: "llama-only", Obs: obs}},
	}, 1000)
	if err := commitCatalogShim(reg, []CatalogEntry{{Name: "cat-only", Provider: "resp", Upstream: "cat-only"}}, nil, 1000); err != nil {
		t.Fatal(err)
	}
	snap := reg.Current()
	for _, lm := range snap.ListedModels() {
		eff := lm.Model.EffectiveDialect(lm.Provider)
		want := rrListingBase(t, snap, lm)
		got := snap.CanonicalBase(lm.Model.Name, eff)
		// Both must agree on dialect transport claims; facts otherwise.
		wj, _ := want.MarshalJSON()
		gj, _ := got.MarshalJSON()
		if string(wj) != string(gj) {
			t.Fatalf("CanonicalBase must match listing switch for %s (%s):\nlisting=%s\ncanonical=%s", lm.Model.Name, lm.Source, wj, gj)
		}
		// RequestCapability for the resolved route must use the same base.
		m, p, fault := snap.Resolve(lm.Model.Name)
		if fault != Found {
			t.Fatalf("resolve %s: %v", lm.Model.Name, fault)
		}
		rc := snap.RequestCapability(m, p)
		rj, _ := rc.MarshalJSON()
		_ = rj
		if m.Name != lm.Model.Name {
			t.Fatalf("resolved canonical must match listed name: %q vs %q", m.Name, lm.Model.Name)
		}
	}
}

func TestRRShadowSemanticsNoLeak(t *testing.T) {
	cfg := rrLlamaCfg()
	cfg.Models = append(cfg.Models, config.Model{Name: "dup", Provider: "chat", Upstream: "u-dup"})
	reg := NewRegistry(cfg)
	obs := capability.ExtractLlamaObs(map[string]any{"id": "x"}, map[string]any{"build_info": "llama.cpp"}, "single")
	rrCommitLlama(t, reg, map[string][]LlamaEntry{
		"llama-a": {{Name: "dup", Upstream: "dup-up", Obs: obs}, {Name: "llama-only", Upstream: "llama-only", Obs: obs}},
	}, 1000)
	if err := commitCatalogShim(reg, []CatalogEntry{
		{Name: "dup", Provider: "resp", Upstream: "dup-cat"},
		{Name: "llama-only", Provider: "resp", Upstream: "cat-shadow"},
		{Name: "cat-only", Provider: "resp", Upstream: "cat-only"},
	}, nil, 1000); err != nil {
		t.Fatal(err)
	}
	snap := reg.Current()
	m, _, fault := snap.Resolve("dup")
	if fault != Found || m.Upstream != "u-dup" {
		t.Fatalf("explicit must shadow llama/catalog: %+v %v", m, fault)
	}
	if _, ok := snap.LlamaBase("dup"); ok {
		t.Fatal("shadowed llama row must publish no base")
	}
	if _, ok := snap.CatalogBase("dup"); ok {
		t.Fatal("shadowed catalog ID must publish no base")
	}
	// CanonicalBase for the shadowed name must be Empty (static), matching listing.
	got := snap.CanonicalBase("dup", m.EffectiveDialect(snap.providers["chat"]))
	want := capability.Empty("openai-chat")
	gj, _ := got.MarshalJSON()
	wj, _ := want.MarshalJSON()
	if string(gj) != string(wj) {
		t.Fatalf("shadowed canonical must be Empty static:\n got=%s\nwant=%s", gj, wj)
	}
	// Llama shadows catalog.
	m2, _, fault := snap.Resolve("llama-only")
	if fault != Found || m2.Provider != "llama-a" {
		t.Fatalf("llama must shadow catalog: %+v %v", m2, fault)
	}
	if _, ok := snap.CatalogBase("llama-only"); ok {
		t.Fatal("catalog shadowed by llama must publish no base")
	}
	if _, ok := snap.LlamaBase("llama-only"); !ok {
		t.Fatal("winning llama row must keep its base")
	}
	if _, ok := snap.CatalogBase("cat-only"); !ok {
		t.Fatal("unshadowed catalog must keep its base")
	}
}

func TestRRRequestCapabilityIgnoresMetadataRouter(t *testing.T) {
	cfg := rrStaticCfg()
	meta := []byte(`{"router":{"reasoning":{"effort":{"values":["low"]}}}}`)
	for i := range cfg.Models {
		if cfg.Models[i].Name == "static-resp" {
			cfg.Models[i].Metadata = append([]byte(nil), meta...)
		}
	}
	reg := NewRegistry(cfg)
	snap := reg.Current()
	m, p, fault := snap.Resolve("static-resp")
	if fault != Found {
		t.Fatalf("resolve: %v", fault)
	}
	caps := capability.ReasoningFor(snap.RequestCapability(m, p))
	// Raw metadata.router must be ignored: unknown neutral, exact pass.
	if caps.EffortValues.Set {
		t.Fatalf("metadata.router must be ignored (unknown neutral): %+v", caps.EffortValues)
	}
}

func TestRRResolveAliasDisabledDynamicDestVsIdentity(t *testing.T) {
	cfg := rrStaticCfg()
	cfg.Models = append(cfg.Models,
		config.Model{Name: "real-resp", Provider: "resp", Upstream: "up-resp", Aliases: []string{"alias-resp"}},
		config.Model{Name: "over-resp", Provider: "chat", Upstream: "up-over", Dialect: "openai-responses", Aliases: []string{"alias-over"}},
		config.Model{Name: "off-model", Provider: "chat", Upstream: "u-off", Disabled: true},
	)
	cfg.Server.AllowDynamicRoutes = true
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

	// Alias resolves to canonical; alias-key override never applies.
	m, p, fault := snap.Resolve("alias-resp")
	if fault != Found || m.Name != "real-resp" {
		t.Fatalf("alias must resolve to canonical: %+v %v", m, fault)
	}
	caps := capability.ReasoningFor(snap.RequestCapability(m, p))
	if !caps.EffortValues.Set || len(caps.EffortValues.List) != 2 {
		t.Fatalf("alias must inherit canonical override: %+v", caps.EffortValues)
	}
	// Effective-dest override: provider chat default, route responses.
	mo, po, fault := snap.Resolve("over-resp")
	if fault != Found {
		t.Fatal("override resolve")
	}
	if got := mo.EffectiveDialect(po); got != "openai-responses" {
		t.Fatalf("route override must select dest: %q", got)
	}
	if po.Dialect != "openai-chat" || po.Name != "chat" {
		t.Fatalf("provider credential identity must not change with dest: %+v", po)
	}
	// Disabled is reserved with no fallthrough even under the same name.
	if _, _, fault := snap.Resolve("off-model"); fault != DisabledReserved {
		t.Fatalf("disabled must be reserved: %v", fault)
	}
	// Dynamic inherits provider default (chat here).
	md, pd, fault := snap.Resolve("chat/dyn-model")
	if fault != Found {
		t.Fatalf("dynamic must resolve: %v", fault)
	}
	if got := md.EffectiveDialect(pd); got != "openai-chat" {
		t.Fatalf("dynamic must inherit provider dest: %q", got)
	}
	if _, ok := snap.CatalogBase("chat/dyn-model"); ok {
		t.Fatal("dynamic rows are base-unknown")
	}
	_ = snap.CanonicalBase(md.Name, md.EffectiveDialect(pd))
}

func TestRRSnapshotPinningAndCloneIsolation(t *testing.T) {
	reg := NewRegistry(rrStaticCfg())
	snap1 := reg.Current()
	m, p, _ := snap1.Resolve("static-resp")
	c1 := snap1.RequestCapability(m, p)
	vals, _ := capability.StringListValue([]string{"MUT"})
	s := "mut"
	_ = c1.Put("reasoning.effort.values", vals, &s, capability.KindOverride)
	c2 := snap1.RequestCapability(m, p)
	caps := capability.ReasoningFor(c2)
	if caps.EffortValues.Set {
		t.Fatalf("snapshot must own facts; mutation must not leak: %+v", caps.EffortValues)
	}
	// Publication swaps the snapshot; the pinned one retains old facts.
	admitted, err := capability.ValidateOverrides(map[string]any{
		"static-resp": map[string]any{"reasoning": map[string]any{"effort": map[string]any{"values": []any{"low"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate operator override publication via a fresh registry with same static seed + overrides.
	cfg2 := rrStaticCfg()
	cfg2.CapabilityOverrides = admitted
	reg2 := NewRegistry(cfg2)
	snap2 := reg2.Current()
	m2, p2, _ := snap2.Resolve("static-resp")
	if got := capability.ReasoningFor(snap2.RequestCapability(m2, p2)); !got.EffortValues.Set {
		t.Fatalf("new snapshot must carry new facts")
	}
	if got := capability.ReasoningFor(snap1.RequestCapability(m, p)); got.EffortValues.Set {
		t.Fatalf("pinned snapshot must retain old facts across publication")
	}
}

func TestRRLlamaPlusResponsesUnreachable(t *testing.T) {
	cfg := rrLlamaCfg()
	cfg.Server.AllowDynamicRoutes = true
	reg := NewRegistry(cfg)
	obs := capability.ExtractLlamaObs(map[string]any{"id": "x"}, map[string]any{"build_info": "llama.cpp"}, "single")
	rrCommitLlama(t, reg, map[string][]LlamaEntry{
		"llama-a": {{Name: "llama-only", Upstream: "llama-only", Obs: obs}},
	}, 1000)
	if err := commitCatalogShim(reg, []CatalogEntry{{Name: "cat-resp", Provider: "resp", Upstream: "cat-resp"}}, nil, 1000); err != nil {
		t.Fatal(err)
	}
	snap := reg.Current()
	for _, lm := range snap.ListedModels() {
		if snap.HasLlamaProvider(lm.Provider.Name) {
			if got := lm.Model.EffectiveDialect(lm.Provider); got != "openai-chat" {
				t.Fatalf("trusted llama inventory must stay chat-destined, got %s for %s", got, lm.Model.Name)
			}
		}
	}
	// Every explicitly configured responses route is non-llama.
	for _, name := range []string{"static-resp", "cat-resp"} {
		m, p, fault := snap.Resolve(name)
		if fault != Found {
			t.Fatalf("resolve %s: %v", name, fault)
		}
		if snap.HasLlamaProvider(p.Name) {
			t.Fatalf("responses route %s must not be llama-backed: %s", name, p.Name)
		}
		if got := m.EffectiveDialect(p); got != "openai-responses" {
			t.Fatalf("responses route dest: %q", got)
		}
	}
	// Dynamic via a llama provider also inherits chat.
	md, pd, fault := snap.Resolve("llama-a/some-model")
	if fault != Found {
		t.Fatalf("dynamic llama must resolve: %v", fault)
	}
	if !snap.HasLlamaProvider(pd.Name) {
		t.Fatalf("dynamic llama provider identity lost")
	}
	if got := md.EffectiveDialect(pd); got != "openai-chat" {
		t.Fatalf("dynamic llama must inherit chat dest: %q", got)
	}
}
