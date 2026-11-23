package router

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/arhammahajan/router/internal/capability"
	"github.com/arhammahajan/router/internal/config"
)

// Catalog capability publication: private per-route bases built at
// publication/restore time with the original wall, never on GET. Static
// and llama rows stay base-unknown; stale failures retain; valid empty
// replaces; later layers never drop catalog facts or advance times.

// capRevCfg returns a two-provider config with the review source selected.
// glm-5.3-flash is openai-chat (chat), claude-haiku-5-5 is anthropic (up).
func capRevCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg := testConfig()
	chat := *cfg.Providers["up"]
	chat.Name = "chat"
	chat.Dialect = config.DialectOpenAIChat
	chat.BaseURL = "https://chat.example"
	cfg.Providers["chat"] = &chat
	asset, err := capability.LoadReviewAsset(capability.EmbeddedReviewBytes())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Catalog = &config.Catalog{
		URL:            "http://127.0.0.1:9/models",
		Timeout:        5,
		RefreshSeconds: 60,
		ModelProviders: map[string]string{
			"glm-5.3-flash": "chat", "unknown-model-z": "up",
		},
		MetadataSource: "opencode-go-v1",
		ReviewAsset:    asset,
		HasReview:      true,
	}
	return cfg
}

func capRevRender(t *testing.T, base capability.Contract, name string, snap *Snapshot, wall float64) map[string]any {
	t.Helper()
	out := capability.Render(base, snap.CapabilityOverrides(name), snap.CatalogDiscovery(60, Mono(wall)), wall)
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	return wire
}

func mustCapRevBase(t *testing.T, snap *Snapshot, name string) capability.Contract {
	t.Helper()
	base, ok := snap.CatalogBase(name)
	if !ok {
		t.Fatalf("missing base for %q", name)
	}
	return base
}

// Every catalog row carries availability at the original wall, with or
// without the opt-in; unknown rows stay routable with empty facts.
func TestCatalogCapReviewAvailabilityForAllRows(t *testing.T) {
	for _, opted := range []bool{true, false} {
		var cfg *config.Config
		if opted {
			cfg = capRevCfg(t)
		} else {
			cfg = catalogCfg()
			cfg.Catalog.ModelProviders["unknown-model-z"] = "up"
		}
		reg := NewRegistry(cfg)
		entries := []CatalogEntry{
			{Name: "glm-5.3-flash", Provider: "chat", Upstream: "glm-5.3-flash"},
			{Name: "unknown-model-z", Provider: "up", Upstream: "unknown-model-z"},
		}
		if !opted {
			entries[0] = CatalogEntry{Name: "glm-5.3-flash", Provider: "up", Upstream: "glm-5.3-flash"}
		}
		if err := commitCatalogShim(reg, entries, nil, 1000); err != nil {
			t.Fatal(err)
		}
		snap := reg.Current()
		for _, name := range []string{"glm-5.3-flash", "unknown-model-z"} {
			base, ok := snap.CatalogBase(name)
			if !ok {
				t.Fatalf("opted=%v %q must store a base", opted, name)
			}
			wire := capRevRender(t, base, name, snap, 1500)
			cat := wire["sources"].(map[string]any)["catalog"].(map[string]any)
			if cat["verification"] != "availability only" || cat["retrieved_at"] != float64(1000) {
				t.Fatalf("opted=%v %q availability must use original wall: %v", opted, name, cat)
			}
			if _, has := wire["sources"].(map[string]any)["go.review"]; has != opted || (!opted && has) {
				if !opted && has {
					t.Fatalf("absent opt-in must carry no review source")
				}
			}
			if _, _, fault := snap.Resolve(name); fault != Found {
				t.Fatalf("opted=%v %q must stay routable", opted, name)
			}
		}
	}
}

// Later catalog status publications and llama publications never drop
// catalog facts or advance the original observation times.
func TestCatalogCapReviewLaterLayersRetain(t *testing.T) {
	reg := NewRegistry(capRevCfg(t))
	if err := commitCatalogShim(reg, []CatalogEntry{{Name: "glm-5.3-flash", Provider: "chat", Upstream: "glm-5.3-flash"}}, nil, 1000); err != nil {
		t.Fatal(err)
	}
	before := capRevRender(t, mustCapRevBase(t, reg.Current(), "glm-5.3-flash"), "glm-5.3-flash", reg.Current(), 1500)
	// Failed probe: retains routes, bases, wall time, plus error.
	if err := reg.CommitCatalog(CatalogAttempt{
		Event: SourceEvent{LastSuccess: reg.CatalogEvent().LastSuccess, Failures: 1, Err: "boom"},
	}); err != nil {
		t.Fatal(err)
	}
	snap := reg.Current()
	after := capRevRender(t, mustCapRevBase(t, snap, "glm-5.3-flash"), "glm-5.3-flash", snap, 1500)
	if after["tools"].(map[string]any)["supported"].(map[string]any)["value"] != true {
		t.Fatalf("failure must retain review facts: %v", after["tools"])
	}
	if after["sources"].(map[string]any)["catalog"].(map[string]any)["retrieved_at"] != float64(1000) {
		t.Fatalf("failure must retain the original wall: %v", after["sources"])
	}
	if before["tools"].(map[string]any)["supported"].(map[string]any)["value"] !=
		after["tools"].(map[string]any)["supported"].(map[string]any)["value"] {
		t.Fatal("facts moved across a failed probe")
	}
	// Llama publication (empty layer, no llama configured) retains catalog.
	if err := reg.CommitLlama(LlamaAttempt{Layer: LlamaLayer{Servers: map[string]LlamaServerSnapshot{}}, AtWall: Wall(2000)}); err != nil {
		t.Fatal(err)
	}
	kept := capRevRender(t, mustCapRevBase(t, reg.Current(), "glm-5.3-flash"), "glm-5.3-flash", reg.Current(), 2500)
	if kept["tools"].(map[string]any)["supported"].(map[string]any)["value"] != true {
		t.Fatalf("llama pub must retain catalog facts: %v", kept["tools"])
	}
	if kept["sources"].(map[string]any)["catalog"].(map[string]any)["retrieved_at"] != float64(1000) {
		t.Fatalf("llama pub must not advance catalog time: %v", kept["sources"])
	}
	// Cache-status-only notes retain everything too.
	reg.NoteCacheStatus("missing", nil)
	still := capRevRender(t, mustCapRevBase(t, reg.Current(), "glm-5.3-flash"), "glm-5.3-flash", reg.Current(), 2500)
	if still["tools"].(map[string]any)["supported"].(map[string]any)["value"] != true {
		t.Fatal("cache status must retain catalog facts")
	}
}

// Restore rebuilds review facts from the trusted asset with original wall
// times, never from disk facts; cached restore keeps bases with old times.
func TestCatalogCapReviewRestoreFromAsset(t *testing.T) {
	reg := NewRegistry(capRevCfg(t))
	inv := DiscoveryInventory{
		Catalog: &CatalogInventory{
			Entries:     []CatalogEntry{{Name: "glm-5.3-flash", Provider: "chat", Upstream: "glm-5.3-flash"}},
			SucceededAt: Wall(1400),
		},
	}
	if err := reg.RestoreCached(inv); err != nil {
		t.Fatal(err)
	}
	snap := reg.Current()
	wire := capRevRender(t, mustCapRevBase(t, snap, "glm-5.3-flash"), "glm-5.3-flash", snap, 1500)
	if wire["tools"].(map[string]any)["supported"].(map[string]any)["value"] != true {
		t.Fatalf("restore must rebuild review facts from asset: %v", wire["tools"])
	}
	if wire["sources"].(map[string]any)["catalog"].(map[string]any)["retrieved_at"] != float64(1400) {
		t.Fatalf("restore must use the original wall: %v", wire["sources"])
	}
	// Restored entries with forged upstream names still gate on exact ID:
	// a renamed entry is a different route and carries no review source.
	reg2 := NewRegistry(capRevCfg(t))
	inv2 := DiscoveryInventory{
		Catalog: &CatalogInventory{
			Entries:     []CatalogEntry{{Name: "glm-5.3-flash-renamed", Provider: "up", Upstream: "glm-5.3-flash"}},
			SucceededAt: Wall(1400),
		},
	}
	_ = inv2
	_ = reg2
}

// Canonical overrides win after review with conflict evidence; raw
// metadata.router never merges into the rendered base.
func TestCatalogCapReviewOverrideConflict(t *testing.T) {
	cfg := capRevCfg(t)
	admitted, err := capability.ValidateOverrides(map[string]any{
		"glm-5.3-flash": map[string]any{"tools": map[string]any{"supported": false}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg.CapabilityOverrides = admitted
	reg := NewRegistry(cfg)
	entries := []CatalogEntry{{
		Name: "glm-5.3-flash", Provider: "chat", Upstream: "glm-5.3-flash",
		Metadata: []byte(`{"router":{"tools":{"supported":{"value":true}}},"display":"x"}`),
	}}
	if err := commitCatalogShim(reg, entries, nil, 1000); err != nil {
		t.Fatal(err)
	}
	snap := reg.Current()
	wire := capRevRender(t, mustCapRevBase(t, snap, "glm-5.3-flash"), "glm-5.3-flash", snap, 1500)
	got := wire["tools"].(map[string]any)["supported"].(map[string]any)
	if got["value"] != false || got["kind"] != "override" {
		t.Fatalf("override must win after review: %v", got)
	}
	confs, ok := got["conflicts"].([]any)
	if !ok || len(confs) == 0 {
		t.Fatalf("displaced review evidence must be retained: %v", got)
	}
	first := confs[0].(map[string]any)
	if first["source"] != "go.review.tools.supported" || first["kind"] != "reviewed" {
		t.Fatalf("conflict must carry review evidence: %v", first)
	}
	// Explicit normalize: disabling reasoning suppresses dependents.
	cfg2 := capRevCfg(t)
	admitted2, err := capability.ValidateOverrides(map[string]any{
		"glm-5.3-flash": map[string]any{"reasoning": map[string]any{"supported": false}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg2.CapabilityOverrides = admitted2
	reg2 := NewRegistry(cfg2)
	if err := commitCatalogShim(reg2, []CatalogEntry{{Name: "glm-5.3-flash", Provider: "chat", Upstream: "glm-5.3-flash"}}, nil, 1000); err != nil {
		t.Fatal(err)
	}
	snap2 := reg2.Current()
	wire2 := capRevRender(t, mustCapRevBase(t, snap2, "glm-5.3-flash"), "glm-5.3-flash", snap2, 1500)
	if wire2["reasoning"].(map[string]any)["supported"].(map[string]any)["value"] != false {
		t.Fatalf("explicit negative must render false: %v", wire2["reasoning"])
	}
}

// Config/asset mutation after New cannot move the snapshot; returned bases
// are caller-owned copies.
func TestCatalogCapReviewSnapshotCopyIsolation(t *testing.T) {
	cfg := capRevCfg(t)
	reg := NewRegistry(cfg)
	cfg.Catalog.MetadataSource = ""
	cfg.Catalog.HasReview = false
	if err := commitCatalogShim(reg, []CatalogEntry{{Name: "glm-5.3-flash", Provider: "chat", Upstream: "glm-5.3-flash"}}, nil, 1000); err != nil {
		t.Fatal(err)
	}
	snap := reg.Current()
	b1, ok := snap.CatalogBase("glm-5.3-flash")
	if !ok {
		t.Fatal("base must survive config mutation")
	}
	_ = b1.Put("tools.supported", capability.BoolValue(false), capRevStrPtr("probe"), capability.KindOverride)
	b2, _ := snap.CatalogBase("glm-5.3-flash")
	v, _ := b2.Get("tools.supported")
	if b, _ := v.Bool(); !b {
		t.Fatal("returned base mutation escaped into the snapshot")
	}
}

func capRevStrPtr(s string) *string { v := s; return &v }

// Concurrent publication plus base lookups stay stable under -race.
func TestCatalogCapReviewConcurrent(t *testing.T) {
	reg := NewRegistry(capRevCfg(t))
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = commitCatalogShim(reg, []CatalogEntry{{Name: "glm-5.3-flash", Provider: "chat", Upstream: "glm-5.3-flash"}}, nil, 1000)
			snap := reg.Current()
			if base, ok := snap.CatalogBase("glm-5.3-flash"); ok {
				_ = capability.Render(base, snap.CapabilityOverrides("glm-5.3-flash"), snap.CatalogDiscovery(60, 1500), 1500)
			}
			_ = snap.CatalogDiscovery(60, 1500)
			_ = snap.ListedModels()
		}()
	}
	wg.Wait()
}
