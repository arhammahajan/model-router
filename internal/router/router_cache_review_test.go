// Review probes for transactional restore and publication precedence:
// both-sources atomicity (pointer AND staged-layer rollback), startup-only
// rejection, two-pass canonical-before-alias first-wins (incl. forward
// collisions and suppressed-row alias hygiene), hidden per-server rows, and
// per-unit timestamp rules. New file only; no production edits.
package router

import (
	"testing"

	"github.com/arhammahajan/model-router/internal/config"
)

func reviewRestoreCfg() *config.Config {
	cfg := catalogCfg()
	cfg.Llama = llamaCfg().Llama
	return cfg
}

func TestReviewRestoreAtomicBothLayersRollback(t *testing.T) {
	reg := NewRegistry(reviewRestoreCfg())
	if err := commitCatalogShim(reg, []CatalogEntry{{Name: "cat-1", Provider: "up", Upstream: "cat-1"}}, nil, 100); err != nil {
		t.Fatal(err)
	}
	if err := commitLlamaShim(reg, LlamaLayer{Servers: map[string]LlamaServerSnapshot{
		"llama-a": llamaLayer(LlamaReachable, LlamaEntry{Name: "lm", Upstream: "lm"}),
		"llama-b": llamaLayer(LlamaNeverSucceeded),
	}}, 200); err != nil {
		t.Fatal(err)
	}
	before := reg.Current()
	beforeInv := reg.ExportDiscovery()
	// Failing second layer (unknown provider) must leave the pointer AND
	// both staged layers (incl. per-unit times) unchanged.
	bad := DiscoveryInventory{
		Catalog: &CatalogInventory{Entries: []CatalogEntry{{Name: "cat-2", Provider: "up", Upstream: "cat-2"}}, SucceededAt: 300},
		Llama: map[string]LlamaInventory{
			"nope": {Entries: []LlamaEntry{{Name: "x", Upstream: "x"}}, SucceededAt: 300},
		},
	}
	if err := reg.RestoreCached(bad); err == nil {
		t.Fatal("unknown provider must reject the whole restore")
	}
	if reg.Current() != before {
		t.Fatal("failed restore must not swap the pointer")
	}
	after := reg.ExportDiscovery()
	if after.Catalog == nil || len(after.Catalog.Entries) != len(beforeInv.Catalog.Entries) ||
		after.Catalog.SucceededAt != beforeInv.Catalog.SucceededAt {
		t.Fatalf("catalog layer changed by failed restore: %+v", after.Catalog)
	}
	if got := after.Llama["llama-a"].SucceededAt; got != beforeInv.Llama["llama-a"].SucceededAt {
		t.Fatalf("llama layer changed by failed restore: %v", got)
	}
	if _, _, fault := reg.Current().Resolve("cat-1"); fault != Found {
		t.Fatal("catalog layer changed by failed restore")
	}
	if _, _, fault := reg.Current().Resolve("lm"); fault != Found {
		t.Fatal("llama layer changed by failed restore")
	}
	// A catalog-only failure (unknown provider) with a valid llama side
	// must also roll both back.
	bad2 := DiscoveryInventory{
		Catalog: &CatalogInventory{Entries: []CatalogEntry{{Name: "c", Provider: "nope", Upstream: "c"}}, SucceededAt: 400},
		Llama: map[string]LlamaInventory{
			"llama-a": {Entries: []LlamaEntry{{Name: "lm2", Upstream: "lm2"}}, SucceededAt: 400},
		},
	}
	if err := reg.RestoreCached(bad2); err == nil {
		t.Fatal("catalog failure must reject the whole restore")
	}
	if reg.Current() != before {
		t.Fatal("second failed restore must not swap the pointer")
	}
	if got := reg.ExportDiscovery().Llama["llama-a"].SucceededAt; got != 200 {
		t.Fatalf("llama time changed by catalog-failed restore: %v", got)
	}
}

func TestReviewRestoreStartupOnlyNoOverwrite(t *testing.T) {
	reg := NewRegistry(reviewRestoreCfg())
	if err := commitCatalogShim(reg, []CatalogEntry{{Name: "c", Provider: "up", Upstream: "c"}}, nil, 10); err != nil {
		t.Fatal(err)
	}
	before := reg.Current()
	err := reg.RestoreCached(DiscoveryInventory{
		Catalog: &CatalogInventory{Entries: []CatalogEntry{{Name: "r", Provider: "up", Upstream: "r"}}, SucceededAt: 20},
	})
	if err == nil {
		t.Fatal("startup-only restore must reject after live publication")
	}
	if reg.Current() != before {
		t.Fatal("rejected restore must not swap the pointer")
	}
	if _, _, fault := reg.Current().Resolve("r"); fault == Found {
		t.Fatal("rejected restore must not publish")
	}
}

func TestReviewTwoPassCanonicalBeforeAlias(t *testing.T) {
	reg := NewRegistry(reviewRestoreCfg())
	// Within one server, an alias claimed by an earlier row must lose to a
	// later canonical: canonicals publish in the first pass.
	if err := commitLlamaShim(reg, LlamaLayer{Servers: map[string]LlamaServerSnapshot{
		"llama-a": llamaLayer(LlamaReachable,
			LlamaEntry{Name: "pre-c1", Upstream: "c1", Aliases: []string{"pre-x"}},
			LlamaEntry{Name: "pre-x", Upstream: "x"},
		),
		"llama-b": llamaLayer(LlamaNeverSucceeded),
	}}, 100); err != nil {
		t.Fatal(err)
	}
	m, _, fault := reg.Current().Resolve("pre-x")
	if fault != Found || m.Upstream != "x" {
		t.Fatalf("canonical must win over earlier alias: %+v %v", m, fault)
	}
	// A filtered canonical publishes no aliases: an explicit collision on
	// the canonical suppresses that row's aliases entirely.
	reg2 := NewRegistry(reviewRestoreCfg())
	if err := commitLlamaShim(reg2, LlamaLayer{Servers: map[string]LlamaServerSnapshot{
		"llama-a": llamaLayer(LlamaReachable,
			LlamaEntry{Name: "a", Upstream: "shadowed", Aliases: []string{"pre-leak"}},
		),
		"llama-b": llamaLayer(LlamaNeverSucceeded),
	}}, 100); err != nil {
		t.Fatal(err)
	}
	if _, _, fault := reg2.Current().Resolve("pre-leak"); fault == Found {
		t.Fatal("suppressed row must contribute no aliases")
	}
	// Explicit names reserve against both sources; llama wins over catalog.
	reg3 := NewRegistry(reviewRestoreCfg())
	if err := commitLlamaShim(reg3, LlamaLayer{Servers: map[string]LlamaServerSnapshot{
		"llama-a": llamaLayer(LlamaReachable, LlamaEntry{Name: "shared", Upstream: "llama-s"}),
		"llama-b": llamaLayer(LlamaNeverSucceeded),
	}}, 100); err != nil {
		t.Fatal(err)
	}
	if err := commitCatalogShim(reg3, []CatalogEntry{
		{Name: "shared", Provider: "up", Upstream: "catalog-s"},
		{Name: "a", Provider: "up", Upstream: "catalog-shadow"},
	}, nil, 100); err != nil {
		t.Fatal(err)
	}
	m, p, fault := reg3.Current().Resolve("shared")
	if fault != Found || m.Upstream != "llama-s" || p.Name != "llama-a" {
		t.Fatalf("llama must win over catalog: %+v %+v %v", m, p, fault)
	}
	m, _, fault = reg3.Current().Resolve("a")
	if fault != Found || m.Upstream != "x" {
		t.Fatalf("explicit must win over catalog: %+v %v", m, fault)
	}
}

func TestReviewHiddenRowsStayPerServer(t *testing.T) {
	reg := NewRegistry(reviewRestoreCfg())
	// The same upstream on two servers stays hidden per server: the public
	// list shows one row, but export keeps both server layers.
	if err := commitLlamaShim(reg, LlamaLayer{Servers: map[string]LlamaServerSnapshot{
		"llama-a": llamaLayer(LlamaReachable,
			LlamaEntry{Name: "pre-one", Upstream: "shared", Aliases: []string{"pre-a1"}}),
		"llama-b": llamaLayer(LlamaReachable,
			LlamaEntry{Name: "pre-two", Upstream: "shared", Aliases: []string{"pre-b1"}}),
	}}, 500); err != nil {
		t.Fatal(err)
	}
	inv := reg.ExportDiscovery()
	a, ok := inv.Llama["llama-a"]
	if !ok || len(a.Entries) != 1 || a.Entries[0].Upstream != "shared" || a.SucceededAt != 500 {
		t.Fatalf("per-server layer lost: %+v", inv.Llama)
	}
	b, ok := inv.Llama["llama-b"]
	if !ok || len(b.Entries) != 1 || b.Entries[0].Upstream != "shared" {
		t.Fatalf("hidden second-server row lost: %+v", inv.Llama)
	}
	if len(a.Entries[0].Aliases) != 1 || a.Entries[0].Aliases[0] != "pre-a1" {
		t.Fatalf("per-server aliases lost: %+v", a.Entries)
	}
}

func TestReviewFreshSuccessOnlyUnitAt(t *testing.T) {
	reg := NewRegistry(reviewRestoreCfg())
	if err := commitLlamaShim(reg, LlamaLayer{Servers: map[string]LlamaServerSnapshot{
		"llama-a": llamaLayer(LlamaReachable, LlamaEntry{Name: "m1", Upstream: "m1"}),
		"llama-b": llamaLayer(LlamaReachable, LlamaEntry{Name: "m2", Upstream: "m2"}),
	}}, 800); err != nil {
		t.Fatal(err)
	}
	// Later: server A fails (stale, retains 800), server B succeeds anew.
	if err := commitLlamaShim(reg, LlamaLayer{Servers: map[string]LlamaServerSnapshot{
		"llama-a": llamaLayer(LlamaUnreachableStale, LlamaEntry{Name: "m1", Upstream: "m1"}),
		"llama-b": llamaLayer(LlamaReachable, LlamaEntry{Name: "m2", Upstream: "m2"}),
	}}, 900); err != nil {
		t.Fatal(err)
	}
	inv := reg.ExportDiscovery()
	if got := inv.Llama["llama-a"].SucceededAt; got != 800 {
		t.Fatalf("stale server must retain original time, got %v", got)
	}
	if got := inv.Llama["llama-b"].SucceededAt; got != 900 {
		t.Fatalf("fresh success must advance only its unit, got %v", got)
	}
	// Catalog success replaces (even empty); failure retains by publishing nothing.
	if err := commitCatalogShim(reg, []CatalogEntry{{Name: "cat-1", Provider: "up", Upstream: "cat-1"}}, nil, 1000); err != nil {
		t.Fatal(err)
	}
	if err := commitCatalogShim(reg, nil, nil, 1100); err != nil {
		t.Fatal(err)
	}
	inv2 := reg.ExportDiscovery()
	if inv2.Catalog == nil || len(inv2.Catalog.Entries) != 0 || inv2.Catalog.SucceededAt != 1100 {
		t.Fatalf("valid-empty catalog success must replace with new time: %+v", inv2.Catalog)
	}
}
