// Focused llama publication: precedence, stale activation, ordering,
// deep-copy isolation, failed-publication rollback and the concurrent
// catalog+llama no-lost-update race.
package router

import (
	"sync"
	"testing"

	"github.com/arhammahajan/router/internal/config"
)

func llamaCfg() *config.Config {
	cfg := testConfig()
	cfg.Llama = &config.LlamaConfig{
		Servers: []config.LlamaServer{
			{URL: "http://127.0.0.1:9931", Name: "llama-a"},
			{URL: "http://127.0.0.1:9932", Name: "llama-b"},
		},
		ProbeTimeout:  1,
		ExposeAliases: true,
		Compatibility: "strict",
	}
	return cfg
}

func publishLlama(t *testing.T, reg *Registry, layer LlamaLayer, at float64) {
	t.Helper()
	if err := commitLlamaShim(reg, layer, at); err != nil {
		t.Fatalf("publish llama: %v", err)
	}
}

func llamaLayer(state LlamaState, entries ...LlamaEntry) LlamaServerSnapshot {
	return LlamaServerSnapshot{State: state, Entries: entries}
}

func TestLlamaPrecedenceExplicitOverLlamaOverCatalog(t *testing.T) {
	reg2cfg := llamaCfg()
	reg2cfg.Catalog = catalogCfg().Catalog
	reg := NewRegistry(reg2cfg)
	publishLlama(t, reg, LlamaLayer{Servers: map[string]LlamaServerSnapshot{
		"llama-a": llamaLayer(LlamaReachable,
			LlamaEntry{Name: "a", Upstream: "x"},
			LlamaEntry{Name: "shared", Upstream: "s"},
		),
		"llama-b": llamaLayer(LlamaReachable),
	}}, 1000)
	publish(t, reg, []CatalogEntry{
		{Name: "shared", Provider: "up", Upstream: "catalog-x"},
		{Name: "cat-only", Provider: "up", Upstream: "cat-only"},
	}, nil, 1000)
	// Explicit "a" (upstream x) beats llama "a".
	m, p, fault := reg.Current().Resolve("a")
	if fault != Found || m.Upstream != "x" || p.Name != "up" {
		t.Fatalf("explicit must win: %+v %+v %v", m, p, fault)
	}
	// Llama "shared" beats catalog "shared" (provider is the llama one).
	m, p, fault = reg.Current().Resolve("shared")
	if fault != Found || m.Upstream != "s" || p.Name != "llama-a" {
		t.Fatalf("llama must win over catalog: %+v %+v %v", m, p, fault)
	}
	if _, _, fault := reg.Current().Resolve("cat-only"); fault != Found {
		t.Fatalf("catalog hit broken: %v", fault)
	}
}

func TestLlamaDynamicNeedsActiveProvider(t *testing.T) {
	reg := NewRegistry(llamaCfg())
	// Never-succeeded: dynamic provider/model stays disabled.
	publishLlama(t, reg, LlamaLayer{Servers: map[string]LlamaServerSnapshot{
		"llama-a": llamaLayer(LlamaNeverSucceeded),
		"llama-b": llamaLayer(LlamaNeverSucceeded),
	}}, 1000)
	if _, _, fault := reg.Current().Resolve("llama-a/anything"); fault != NotFound {
		t.Fatalf("inactive provider must not route dynamically: %v", fault)
	}
	// Valid empty success activates the provider with zero routes.
	publishLlama(t, reg, LlamaLayer{Servers: map[string]LlamaServerSnapshot{
		"llama-a": llamaLayer(LlamaReachable),
		"llama-b": llamaLayer(LlamaNeverSucceeded),
	}}, 1000)
	m, _, fault := reg.Current().Resolve("llama-a/anything")
	if fault != Found || m.Upstream != "anything" {
		t.Fatalf("empty success must activate dynamic: %+v %v", m, fault)
	}
	if _, _, fault := reg.Current().Resolve("llama-b/anything"); fault != NotFound {
		t.Fatalf("never-succeeded must stay inactive: %v", fault)
	}
}

func TestLlamaUnreachableStaleRetainsButUnready(t *testing.T) {
	cfg := llamaCfg()
	cfg.Server.AllowDynamicRoutes = false
	cfg.Models = nil // llama-only readiness
	reg := NewRegistry(cfg)
	publishLlama(t, reg, LlamaLayer{Servers: map[string]LlamaServerSnapshot{
		"llama-a": llamaLayer(LlamaReachable, LlamaEntry{Name: "m1", Upstream: "m1"}),
		"llama-b": llamaLayer(LlamaNeverSucceeded),
	}}, 1000)
	if got := reg.Current().UsableCount(); got != 1 {
		t.Fatalf("usable=%d want 1", got)
	}
	if !reg.Current().Ready() {
		t.Fatal("must be ready")
	}
	// Later failure retains the route but drops readiness.
	publishLlama(t, reg, LlamaLayer{Servers: map[string]LlamaServerSnapshot{
		"llama-a": llamaLayer(LlamaUnreachableStale, LlamaEntry{Name: "m1", Upstream: "m1"}),
		"llama-b": llamaLayer(LlamaNeverSucceeded),
	}}, 1000)
	snap := reg.Current()
	if got := snap.UsableCount(); got != 0 {
		t.Fatalf("stale must not count: %d", got)
	}
	if snap.Ready() {
		t.Fatal("stale-only router must not be ready")
	}
	m, _, fault := snap.Resolve("m1")
	if fault != Found || m.Upstream != "m1" {
		t.Fatalf("stale route must still resolve: %+v %v", m, fault)
	}
	if len(snap.Models()) != 1 || snap.Models()[0].Name != "m1" {
		t.Fatalf("stale route must stay listed: %+v", snap.Models())
	}
}

func TestLlamaAliasCanonicalOrdering(t *testing.T) {
	reg := NewRegistry(llamaCfg())
	// Server A's alias claims "x"; server B's canonical "x" loses (first
	// wins: A's canonicals then aliases precede B).
	publishLlama(t, reg, LlamaLayer{Servers: map[string]LlamaServerSnapshot{
		"llama-a": llamaLayer(LlamaReachable,
			LlamaEntry{Name: "canon-a", Upstream: "ca", Aliases: []string{"x"}}),
		"llama-b": llamaLayer(LlamaReachable,
			LlamaEntry{Name: "x", Upstream: "cb"}),
	}}, 1000)
	m, _, fault := reg.Current().Resolve("x")
	if fault != Found || m.Name != "canon-a" || m.Upstream != "ca" {
		t.Fatalf("alias must win by order: %+v %v", m, fault)
	}
	// A filtered canonical publishes no aliases: explicit "a" reserves the
	// name, so its aliases must not leak either.
	publishLlama(t, reg, LlamaLayer{Servers: map[string]LlamaServerSnapshot{
		"llama-a": llamaLayer(LlamaReachable,
			LlamaEntry{Name: "a", Upstream: "ca", Aliases: []string{"sneaky"}}),
		"llama-b": llamaLayer(LlamaReachable),
	}}, 1000)
	if _, _, fault := reg.Current().Resolve("sneaky"); fault != NotFound {
		t.Fatalf("alias of filtered canonical must not publish: %v", fault)
	}
	if _, _, fault := reg.Current().Resolve("a"); fault != Found {
		t.Fatalf("explicit must survive: %v", fault)
	}
	got := reg.Current().Models()
	for _, mm := range got {
		if mm.Name == "sneaky" || mm.Name == "x" {
			t.Fatalf("aliases must add no listing rows: %+v", got)
		}
	}
}

func TestLlamaDisabledExplicitReserves(t *testing.T) {
	reg := NewRegistry(llamaCfg())
	publishLlama(t, reg, LlamaLayer{Servers: map[string]LlamaServerSnapshot{
		"llama-a": llamaLayer(LlamaReachable,
			LlamaEntry{Name: "b", Upstream: "x", Aliases: []string{"b-alias"}}),
		"llama-b": llamaLayer(LlamaReachable),
	}}, 1000)
	for _, name := range []string{"b", "b-alias"} {
		if _, _, fault := reg.Current().Resolve(name); fault != DisabledReserved {
			t.Fatalf("%q must stay reserved, got %v", name, fault)
		}
	}
}

func TestLlamaModelsOrder(t *testing.T) {
	reg := NewRegistry(catalogCfg())
	reg = NewRegistry(func() *config.Config {
		c := catalogCfg()
		c.Llama = llamaCfg().Llama
		return c
	}())
	publishLlama(t, reg, LlamaLayer{Servers: map[string]LlamaServerSnapshot{
		"llama-b": llamaLayer(LlamaReachable, LlamaEntry{Name: "lb", Upstream: "lb"}),
		"llama-a": llamaLayer(LlamaReachable, LlamaEntry{Name: "la", Upstream: "la"}),
	}}, 1000)
	publish(t, reg, []CatalogEntry{{Name: "cat-1", Provider: "up", Upstream: "cat-1"}}, nil, 1000)
	got := reg.Current().Models()
	var names []string
	for _, m := range got {
		names = append(names, m.Name)
	}
	// Explicit "a", then llama in configured server order (a before b),
	// then catalog.
	want := []string{"a", "la", "lb", "cat-1"}
	if len(names) != len(want) {
		t.Fatalf("order=%q want %q", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("order=%q want %q", names, want)
		}
	}
}

func TestLlamaPublishRejectsUnknown(t *testing.T) {
	reg := NewRegistry(llamaCfg())
	before := reg.Current()
	if err := commitLlamaShim(reg, LlamaLayer{Servers: map[string]LlamaServerSnapshot{
		"nope": llamaLayer(LlamaReachable, LlamaEntry{Name: "x", Upstream: "x"}),
	}}, 1000); err == nil {
		t.Fatal("unknown provider must fail")
	}
	if reg.Current() != before {
		t.Fatal("failed llama publish must not swap the pointer")
	}
	if err := commitLlamaShim(reg, LlamaLayer{Servers: map[string]LlamaServerSnapshot{
		"llama-a": {State: LlamaState(9)},
	}}, 1000); err == nil {
		t.Fatal("unknown state must fail")
	}
	if reg.Current() != before {
		t.Fatal("failed llama publish must not swap the pointer")
	}
	if err := commitLlamaShim(reg, LlamaLayer{Servers: map[string]LlamaServerSnapshot{
		"llama-a": llamaLayer(LlamaNeverSucceeded, LlamaEntry{Name: "x", Upstream: "x"}),
	}}, 1000); err == nil {
		t.Fatal("never-succeeded with entries must fail")
	}
	if reg.Current() != before {
		t.Fatal("failed llama publish must not swap the pointer")
	}
}

func TestLlamaPinAndCopyIsolation(t *testing.T) {
	reg := NewRegistry(llamaCfg())
	layer := LlamaLayer{Servers: map[string]LlamaServerSnapshot{
		"llama-a": llamaLayer(LlamaReachable,
			LlamaEntry{Name: "m", Upstream: "m", Aliases: []string{"al"}, Metadata: []byte(`{"k":"v"}`)}),
		"llama-b": llamaLayer(LlamaNeverSucceeded),
	}}
	publishLlama(t, reg, layer, 1000)
	pinned := reg.Current()
	// Mutating the published inputs leaks nothing.
	layer.Servers["llama-a"].Entries[0].Name = "mutated"
	publishLlama(t, reg, LlamaLayer{Servers: map[string]LlamaServerSnapshot{
		"llama-a": llamaLayer(LlamaReachable, LlamaEntry{Name: "m2", Upstream: "m2"}),
		"llama-b": llamaLayer(LlamaNeverSucceeded),
	}}, 1000)
	if _, _, fault := pinned.Resolve("m2"); fault == Found {
		t.Fatal("pinned snapshot moved under later publish")
	}
	m, _, fault := pinned.Resolve("al")
	if fault != Found || m.Name != "m" || string(m.Metadata) != `{"k":"v"}` {
		t.Fatalf("alias/metadata broken: %+v %v", m, fault)
	}
	m.Metadata[0] = 'X'
	m2, _, _ := pinned.Resolve("m")
	if string(m2.Metadata) != `{"k":"v"}` {
		t.Fatal("returned metadata mutation leaked")
	}
}

func TestCatalogAndLlamaNoLostUpdate(t *testing.T) {
	cfg := catalogCfg()
	cfg.Llama = llamaCfg().Llama
	reg := NewRegistry(cfg)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = commitCatalogShim(reg, []CatalogEntry{{Name: "cat-1", Provider: "up", Upstream: "cat-1"}}, nil, 1000)
			_ = commitLlamaShim(reg, LlamaLayer{Servers: map[string]LlamaServerSnapshot{
				"llama-a": llamaLayer(LlamaReachable, LlamaEntry{Name: "lm", Upstream: "lm"}),
				"llama-b": llamaLayer(LlamaNeverSucceeded),
			}}, 1000)
			snap := reg.Current()
			if snap == nil {
				t.Error("Current nil under publication")
				return
			}
			snap.Resolve("a")
			snap.Resolve("lm")
			snap.Resolve("cat-1")
			snap.Models()
			snap.UsableCount()
			snap.Ready()
		}(i)
	}
	wg.Wait()
	// Final snapshot must contain both layers: no lost update, no half state.
	snap := reg.Current()
	if _, _, fault := snap.Resolve("cat-1"); fault != Found {
		t.Fatal("catalog layer lost")
	}
	if _, _, fault := snap.Resolve("lm"); fault != Found {
		t.Fatal("llama layer lost")
	}
	if got := snap.UsableCount(); got != 4 { // explicit a + a-alias + lm + cat-1
		t.Fatalf("usable=%d want 4", got)
	}
	names := map[string]bool{}
	for _, m := range snap.Models() {
		names[m.Name] = true
	}
	if !names["a"] || !names["lm"] || !names["cat-1"] {
		t.Fatalf("listing lost a layer: %+v", names)
	}
}
