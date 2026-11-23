// Persistence units: transactional restore, export fidelity, timestamps.
package router

import (
	"testing"

	"github.com/arhammahajan/router/internal/config"
)

func restoreCfg() *config.Config {
	cfg := catalogCfg()
	cfg.Llama = llamaCfg().Llama
	return cfg
}

func TestRestoreBadSecondLayerLeavesAllUnchanged(t *testing.T) {
	reg := NewRegistry(restoreCfg())
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
	// Second layer references an unknown provider: whole restore rejects.
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
	if _, _, fault := reg.Current().Resolve("cat-1"); fault != Found {
		t.Fatal("catalog layer changed by failed restore")
	}
	if _, _, fault := reg.Current().Resolve("lm"); fault != Found {
		t.Fatal("llama layer changed by failed restore")
	}
	inv := reg.ExportDiscovery()
	if inv.Catalog == nil || inv.Catalog.SucceededAt != 100 {
		t.Fatalf("catalog time changed: %+v", inv.Catalog)
	}
	if got := inv.Llama["llama-a"].SucceededAt; got != 200 {
		t.Fatalf("llama time changed: %v", got)
	}
}

func TestExportRoundTripsHiddenRows(t *testing.T) {
	reg := NewRegistry(restoreCfg())
	if err := commitLlamaShim(reg, LlamaLayer{Servers: map[string]LlamaServerSnapshot{
		"llama-a": llamaLayer(LlamaReachable,
			LlamaEntry{Name: "pre-hidden", Upstream: "hidden", Aliases: []string{"pre-alias"}}),
		"llama-b": llamaLayer(LlamaNeverSucceeded),
	}}, 500); err != nil {
		t.Fatal(err)
	}
	inv := reg.ExportDiscovery()
	got, ok := inv.Llama["llama-a"]
	if !ok || len(got.Entries) != 1 || got.Entries[0].Upstream != "hidden" {
		t.Fatalf("export must preserve hidden per-server rows: %+v", inv.Llama)
	}
	if len(got.Entries[0].Aliases) != 1 || got.Entries[0].Aliases[0] != "pre-alias" {
		t.Fatalf("export must preserve aliases: %+v", got.Entries)
	}
	if got.SucceededAt != 500 {
		t.Fatalf("export must carry original time: %v", got.SucceededAt)
	}
	// Public listing hides aliases; export must not.
	for _, m := range reg.Current().Models() {
		if m.Name == "pre-alias" {
			t.Fatal("aliases must add no listing rows")
		}
	}
}

func TestRestoreStaleSeedUnreadyButResolves(t *testing.T) {
	reg := NewRegistry(restoreCfg())
	err := reg.RestoreCached(DiscoveryInventory{
		Llama: map[string]LlamaInventory{
			"llama-a": {Entries: []LlamaEntry{{Name: "m1", Upstream: "m1"}}, SucceededAt: 700},
		},
	})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	m, _, fault := reg.Current().Resolve("m1")
	if fault != Found || m.Upstream != "m1" {
		t.Fatalf("restored stale must resolve: %+v %v", m, fault)
	}
	if reg.Current().UsableCount() != 2 {
		// explicit "a" plus its alias; restored stale never counts.
		// catalogCfg has explicit a; llama stale adds 0 usable.
		t.Fatalf("stale must not count: %d", reg.Current().UsableCount())
	}
	inv := reg.ExportDiscovery()
	if got := inv.Llama["llama-a"].SucceededAt; got != 700 {
		t.Fatalf("restore must carry decoded time: %v", got)
	}
}

func TestLiveFailureRetainsOriginalTime(t *testing.T) {
	reg := NewRegistry(restoreCfg())
	if err := commitLlamaShim(reg, LlamaLayer{Servers: map[string]LlamaServerSnapshot{
		"llama-a": llamaLayer(LlamaReachable, LlamaEntry{Name: "m1", Upstream: "m1"}),
		"llama-b": llamaLayer(LlamaNeverSucceeded),
	}}, 800); err != nil {
		t.Fatal(err)
	}
	// Later failure retains stale with the original time.
	if err := commitLlamaShim(reg, LlamaLayer{Servers: map[string]LlamaServerSnapshot{
		"llama-a": llamaLayer(LlamaUnreachableStale, LlamaEntry{Name: "m1", Upstream: "m1"}),
		"llama-b": llamaLayer(LlamaNeverSucceeded),
	}}, 900); err != nil {
		t.Fatal(err)
	}
	if got := reg.ExportDiscovery().Llama["llama-a"].SucceededAt; got != 800 {
		t.Fatalf("stale must retain original time: %v", got)
	}
}

func TestRestoreRejectsAfterLivePublish(t *testing.T) {
	reg := NewRegistry(restoreCfg())
	if err := commitCatalogShim(reg, []CatalogEntry{{Name: "c", Provider: "up", Upstream: "c"}}, nil, 10); err != nil {
		t.Fatal(err)
	}
	err := reg.RestoreCached(DiscoveryInventory{
		Catalog: &CatalogInventory{Entries: []CatalogEntry{{Name: "r", Provider: "up", Upstream: "r"}}, SucceededAt: 20},
	})
	if err == nil {
		t.Fatal("startup-only restore must reject after live publication")
	}
}
