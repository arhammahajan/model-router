package router

import "testing"

func TestOpsCatalogCountIncludesShadowedSourceInventory(t *testing.T) {
	reg := NewRegistry(catalogCfg())
	if err := reg.CommitCatalog(CatalogAttempt{
		Event: SourceEvent{LastSuccess: MonoPtr(1)},
		Success: &CatalogSuccess{Wall: 100, Entries: []CatalogEntry{
			{Name: "a", Provider: "up", Upstream: "hidden"},
			{Name: "visible", Provider: "up", Upstream: "visible"},
			{Name: "visible", Provider: "up", Upstream: "last-wins"},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	report := reg.Current().CatalogStatus(true, "http://example.test/catalog", nil, 0, 1)
	if report["models"] != 2 {
		t.Fatalf("catalog models counts its own distinct inventory, including shadowed IDs: %v", report)
	}
	if len(reg.Current().catalog) != 1 {
		t.Fatal("only the unshadowed catalog route should be publicly listed")
	}
}

func TestOpsLlamaServerCountsUseAcceptedCanonicals(t *testing.T) {
	reg := NewRegistry(llamaCfg())
	if err := reg.CommitLlama(LlamaAttempt{
		AtWall: 100, Aggregate: SourceEvent{LastSuccess: MonoPtr(1)},
		Layer: LlamaLayer{Servers: map[string]LlamaServerSnapshot{
			"llama-a": {State: LlamaReachable, Entries: []LlamaEntry{
				{Name: "a", Upstream: "reserved"},
				{Name: "m1", Upstream: "m1", Aliases: []string{"claimed"}},
				{Name: "m1", Upstream: "duplicate"},
			}},
			"llama-b": {State: LlamaReachable, Entries: []LlamaEntry{
				{Name: "claimed", Upstream: "shadowed-by-earlier-alias"},
				{Name: "m2", Upstream: "m2"},
			}},
		}},
		Servers: map[string]ServerEvent{
			"llama-a": {Models: 3, SourceEvent: SourceEvent{LastSuccess: MonoPtr(1)}},
			"llama-b": {Models: 2, SourceEvent: SourceEvent{LastSuccess: MonoPtr(1)}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	for name, event := range reg.ServerEvents() {
		if event.Models != 1 {
			t.Fatalf("%s models must count accepted canonicals, not raw rows: %d", name, event.Models)
		}
	}
}
