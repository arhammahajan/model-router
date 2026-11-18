// Catalog snapshot precedence, publication and concurrency.
package router

import (
	"sync"
	"testing"

	"github.com/arhammahajan/model-router/internal/catalog"
	"github.com/arhammahajan/model-router/internal/config"
)

func catalogCfg() *config.Config {
	cfg := testConfig()
	cfg.Catalog = &config.Catalog{
		URL:            "http://127.0.0.1:9/models",
		Timeout:        5,
		RefreshSeconds: 60,
		ModelProviders: map[string]string{"exact-model": "up"},
		Rules:          []config.CatalogRule{mustRuleT("acme/.*", "up")},
	}
	return cfg
}

func mustRuleT(match, provider string) config.CatalogRule {
	re, err := catalog.Compile(match)
	if err != nil {
		panic(err)
	}
	return config.CatalogRule{Match: match, Provider: provider, Pattern: re}
}

func publish(t *testing.T, reg *Registry, entries []CatalogEntry, unmapped []string, at float64) {
	t.Helper()
	if err := commitCatalogShim(reg, entries, unmapped, at); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

func TestExplicitBeatsCatalog(t *testing.T) {
	reg := NewRegistry(catalogCfg())
	publish(t, reg, []CatalogEntry{{Name: "a", Provider: "up", Upstream: "catalog-x"}}, nil, 1000)
	m, _, fault := reg.Current().Resolve("a")
	if fault != Found || m.Upstream != "x" {
		t.Fatalf("explicit must win: %+v %v", m, fault)
	}
}

func TestDisabledReservesAgainstCatalog(t *testing.T) {
	reg := NewRegistry(catalogCfg())
	// "b" is disabled explicit; its alias too.
	publish(t, reg,
		[]CatalogEntry{{Name: "b", Provider: "up", Upstream: "z"}, {Name: "b-alias", Provider: "up", Upstream: "z"}},
		nil, 1000)
	for _, name := range []string{"b", "b-alias"} {
		if _, _, fault := reg.Current().Resolve(name); fault != DisabledReserved {
			t.Fatalf("%q must stay reserved, got %v", name, fault)
		}
	}
}

func TestCatalogUnmappedFault(t *testing.T) {
	reg := NewRegistry(catalogCfg())
	publish(t, reg, nil, []string{"ghost-model"}, 1000)
	if _, _, fault := reg.Current().Resolve("ghost-model"); fault != UnmappedCatalog {
		t.Fatalf("unmapped must fault, got %v", fault)
	}
	// Explicit still checked first even for unmapped IDs: not applicable
	// here, but dynamic must not swallow unmapped.
	if _, _, fault := reg.Current().Resolve("ghost-model"); fault == Found {
		t.Fatal("unmapped must not resolve")
	}
}

func TestCatalogHitAndDynamicFallback(t *testing.T) {
	cfg := catalogCfg()
	cfg.Server.AllowDynamicRoutes = true
	reg := NewRegistry(cfg)
	publish(t, reg, []CatalogEntry{{Name: "cat-1", Provider: "up", Upstream: "cat-1"}}, nil, 1000)
	m, p, fault := reg.Current().Resolve("cat-1")
	if fault != Found || p.Dialect != config.DialectAnthropic || m.Upstream != "cat-1" {
		t.Fatalf("catalog hit broken: %+v %+v %v", m, p, fault)
	}
	m, _, fault = reg.Current().Resolve("up/other")
	if fault != Found || m.Upstream != "other" {
		t.Fatalf("dynamic fallback broken: %+v %v", m, fault)
	}
	if _, _, fault := reg.Current().Resolve("missing"); fault != NotFound {
		t.Fatalf("miss must be NotFound, got %v", fault)
	}
	// Dynamic disabled: miss stays NotFound.
	cfg.Server.AllowDynamicRoutes = false
	reg2 := NewRegistry(cfg)
	if _, _, fault := reg2.Current().Resolve("up/other"); fault != NotFound {
		t.Fatalf("dynamic-deny broken: %v", fault)
	}
}

func TestCatalogModelsOrder(t *testing.T) {
	reg := NewRegistry(catalogCfg())
	publish(t, reg, []CatalogEntry{
		{Name: "cat-b", Provider: "up", Upstream: "cat-b"},
		{Name: "cat-a", Provider: "up", Upstream: "cat-a"},
	}, nil, 1000)
	got := reg.Current().Models()
	if len(got) != 3 || got[0].Name != "a" || got[1].Name != "cat-b" || got[2].Name != "cat-a" {
		t.Fatalf("order must be explicit then catalog fetch: %+v", got)
	}
}

func TestCatalogDuplicatesLastWinsFirstPosition(t *testing.T) {
	reg := NewRegistry(catalogCfg())
	publish(t, reg, []CatalogEntry{
		{Name: "dup", Provider: "up", Upstream: "first"},
		{Name: "other", Provider: "up", Upstream: "other"},
		{Name: "dup", Provider: "up", Upstream: "second"},
	}, nil, 1000)
	got := reg.Current().Models()
	if len(got) != 3 || got[1].Name != "dup" || got[1].Upstream != "second" || got[2].Name != "other" {
		t.Fatalf("duplicate handling wrong: %+v", got)
	}
	m, _, fault := reg.Current().Resolve("dup")
	if fault != Found || m.Upstream != "second" {
		t.Fatalf("resolve must see last value: %+v %v", m, fault)
	}
}

func TestCatalogUsableAndReady(t *testing.T) {
	cfg := testConfig()
	cfg.Server.AllowDynamicRoutes = false
	// Strip explicit routes: catalog-only readiness.
	cfg.Models = nil
	reg := NewRegistry(cfg)
	if reg.Current().Ready() || reg.Current().UsableCount() != 0 {
		t.Fatal("empty static seed must be unready")
	}
	publish(t, reg, []CatalogEntry{{Name: "cat-1", Provider: "up", Upstream: "cat-1"}}, nil, 1000)
	if !reg.Current().Ready() || reg.Current().UsableCount() != 1 {
		t.Fatalf("catalog must count toward readiness: %d", reg.Current().UsableCount())
	}
}

func TestPublishRejectsUnknownProvider(t *testing.T) {
	reg := NewRegistry(catalogCfg())
	before := reg.Current()
	if err := commitCatalogShim(reg, []CatalogEntry{{Name: "x", Provider: "nope", Upstream: "x"}}, nil, 1000); err == nil {
		t.Fatal("unknown provider must fail")
	}
	if reg.Current() != before {
		t.Fatal("failed publish must not swap the pointer")
	}
}

func TestPublishCopiesIsolate(t *testing.T) {
	reg := NewRegistry(catalogCfg())
	entries := []CatalogEntry{{Name: "cat-1", Provider: "up", Upstream: "cat-1", Metadata: []byte(`{"k":"v"}`)}}
	unmapped := []string{"ghost"}
	publish(t, reg, entries, unmapped, 1000)
	entries[0].Name = "mutated"
	entries[0].Metadata[0] = 'X'
	unmapped[0] = "mutated"
	snap := reg.Current()
	if _, _, fault := snap.Resolve("mutated"); fault == Found {
		t.Fatal("entry mutation leaked into snapshot")
	}
	m, _, fault := snap.Resolve("cat-1")
	if fault != Found || string(m.Metadata) != `{"k":"v"}` {
		t.Fatalf("metadata copy broken: %+v %v", m, fault)
	}
	m.Metadata[0] = 'X'
	m2, _, _ := snap.Resolve("cat-1")
	if string(m2.Metadata) != `{"k":"v"}` {
		t.Fatal("returned metadata mutation leaked")
	}
	listed := snap.Models()
	listed[len(listed)-1].Metadata[0] = 'X'
	if again, _, _ := snap.Resolve("cat-1"); string(again.Metadata) != `{"k":"v"}` {
		t.Fatal("listing mutation leaked")
	}
}

func TestCurrentNeverNilAndConcurrentPublish(t *testing.T) {
	reg := NewRegistry(catalogCfg())
	if reg.Current() == nil {
		t.Fatal("Current must never be nil")
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = commitCatalogShim(reg, []CatalogEntry{{Name: "cat-1", Provider: "up", Upstream: "cat-1"}}, nil, 1000)
			snap := reg.Current()
			if snap == nil {
				t.Error("Current nil under publication")
				return
			}
			snap.Resolve("a")
			snap.Resolve("cat-1")
			snap.Resolve("ghost")
			snap.Models()
			snap.UsableCount()
			snap.Ready()
		}(i)
	}
	wg.Wait()
}
