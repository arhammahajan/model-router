// Final bounded acceptance review: transactional restore atomicity,
// startup-only restore, export coherence and isolation.
//
// New file only; no production edits. Stale-unready and route-only
// semantics are declared trades and are not re-reported here.
package router

import (
	"sync"
	"testing"
)

func TestFinalReviewRestoreAtomicLeavesAllUnchanged(t *testing.T) {
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
	beforeExport := reg.ExportDiscovery()
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
	if after.Catalog == nil || after.Catalog.SucceededAt != beforeExport.Catalog.SucceededAt ||
		len(after.Catalog.Entries) != len(beforeExport.Catalog.Entries) ||
		after.Catalog.Entries[0].Name != "cat-1" {
		t.Fatalf("failed restore changed the staged catalog layer: %+v", after.Catalog)
	}
	if got := after.Llama["llama-a"].SucceededAt; got != beforeExport.Llama["llama-a"].SucceededAt {
		t.Fatalf("failed restore changed staged llama times: %v", got)
	}
	if _, _, fault := reg.Current().Resolve("cat-1"); fault != Found {
		t.Fatal("catalog layer changed by failed restore")
	}
	if _, _, fault := reg.Current().Resolve("lm"); fault != Found {
		t.Fatal("llama layer changed by failed restore")
	}
}

func TestFinalReviewRestoreStartupOnlyRejectsAfterLivePublish(t *testing.T) {
	reg := NewRegistry(restoreCfg())
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
		t.Fatal("rejected late restore swapped the pointer")
	}
	inv := reg.ExportDiscovery()
	if inv.Catalog == nil || inv.Catalog.SucceededAt != 10 || inv.Catalog.Entries[0].Name != "c" {
		t.Fatalf("rejected late restore changed staged state: %+v", inv.Catalog)
	}
}

func TestFinalReviewExportCoherentAndIsolated(t *testing.T) {
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
		t.Fatalf("export must carry the original time verbatim: %v", got.SucceededAt)
	}
	// Public listing hides aliases; export must not.
	for _, m := range reg.Current().Models() {
		if m.Name == "pre-alias" {
			t.Fatal("aliases must add no listing rows")
		}
	}
	// Mutating an export must not affect staged state.
	inv.Llama["llama-a"].Entries[0].Name = "mutated"
	inv.Catalog = &CatalogInventory{Entries: []CatalogEntry{{Name: "ghost", Provider: "up", Upstream: "ghost"}}, SucceededAt: 1}
	fresh := reg.ExportDiscovery()
	if fresh.Llama["llama-a"].Entries[0].Name != "pre-hidden" {
		t.Fatal("export must be deep copies")
	}
	if fresh.Catalog != nil {
		t.Fatal("export mutation leaked into staged state")
	}
	// Concurrent export/resolve smoke: no race, stable reads.
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = reg.ExportDiscovery()
			reg.Current().Resolve("pre-hidden")
		}()
	}
	wg.Wait()
}
