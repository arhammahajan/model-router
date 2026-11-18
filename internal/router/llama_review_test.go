// Review probes for transactional publication: failed PublishLlama must
// leave both the cached layer and the pointer untouched, and stale
// activation/readiness semantics must hold across layers.
package router

import (
	"testing"
)

func TestLlamaReviewFailedPublishPreservesCachedLayer(t *testing.T) {
	cfg := llamaCfg()
	reg := NewRegistry(cfg)
	// Seed a good llama layer and a catalog layer.
	publishLlama(t, reg, LlamaLayer{Servers: map[string]LlamaServerSnapshot{
		"llama-a": llamaLayer(LlamaReachable, LlamaEntry{Name: "keep", Upstream: "keep"}),
		"llama-b": llamaLayer(LlamaNeverSucceeded),
	}}, 1000)
	publish(t, reg, []CatalogEntry{{Name: "cat-1", Provider: "up", Upstream: "cat-1"}}, nil, 1000)
	before := reg.Current()

	// Failed llama publish (unknown provider) changes neither pointer nor
	// cached layer: a later catalog publish must still see llama routes.
	if err := commitLlamaShim(reg, LlamaLayer{Servers: map[string]LlamaServerSnapshot{
		"nope": llamaLayer(LlamaReachable, LlamaEntry{Name: "x", Upstream: "x"}),
	}}, 1000); err == nil {
		t.Fatal("unknown provider must fail")
	}
	if reg.Current() != before {
		t.Fatal("failed publish must not swap the pointer")
	}
	publish(t, reg, []CatalogEntry{{Name: "cat-2", Provider: "up", Upstream: "cat-2"}}, nil, 1000)
	snap := reg.Current()
	if _, _, fault := snap.Resolve("keep"); fault != Found {
		t.Fatal("cached llama layer lost after failed publish + catalog publish")
	}
	if _, _, fault := snap.Resolve("cat-2"); fault != Found {
		t.Fatal("catalog layer lost after failed llama publish")
	}

	// Failed llama publish (never-succeeded with entries) also preserves.
	before = reg.Current()
	if err := commitLlamaShim(reg, LlamaLayer{Servers: map[string]LlamaServerSnapshot{
		"llama-a": llamaLayer(LlamaNeverSucceeded, LlamaEntry{Name: "x", Upstream: "x"}),
	}}, 1000); err == nil {
		t.Fatal("never-succeeded with entries must fail")
	}
	if reg.Current() != before {
		t.Fatal("failed publish must not swap the pointer")
	}
	if _, _, fault := reg.Current().Resolve("keep"); fault != Found {
		t.Fatal("cached layer altered by failed publish")
	}
}

// Failed catalog publish must likewise preserve the cached llama layer.
func TestLlamaReviewFailedCatalogPreservesLlama(t *testing.T) {
	cfg := catalogCfg()
	cfg.Llama = llamaCfg().Llama
	reg := NewRegistry(cfg)
	publishLlama(t, reg, LlamaLayer{Servers: map[string]LlamaServerSnapshot{
		"llama-a": llamaLayer(LlamaReachable, LlamaEntry{Name: "lm", Upstream: "lm"}),
		"llama-b": llamaLayer(LlamaNeverSucceeded),
	}}, 1000)
	before := reg.Current()
	if err := commitCatalogShim(reg, []CatalogEntry{{Name: "bad", Provider: "nope", Upstream: "x"}}, nil, 1000); err == nil {
		t.Fatal("unknown catalog provider must fail")
	}
	if reg.Current() != before {
		t.Fatal("failed catalog publish must not swap the pointer")
	}
	if _, _, fault := reg.Current().Resolve("lm"); fault != Found {
		t.Fatal("llama layer lost after failed catalog publish")
	}
}

// Stale providers stay resolvable and listed but never count toward
// readiness; empty-success activates dynamic with zero routes.
func TestLlamaReviewStaleListableButUnready(t *testing.T) {
	cfg := llamaCfg()
	cfg.Server.AllowDynamicRoutes = true
	cfg.Models = nil
	reg := NewRegistry(cfg)
	publishLlama(t, reg, LlamaLayer{Servers: map[string]LlamaServerSnapshot{
		"llama-a": llamaLayer(LlamaUnreachableStale, LlamaEntry{Name: "s1", Upstream: "s1"}),
		"llama-b": llamaLayer(LlamaReachable),
	}}, 1000)
	snap := reg.Current()
	if _, _, fault := snap.Resolve("s1"); fault != Found {
		t.Fatal("stale route must resolve")
	}
	if _, _, fault := snap.Resolve("llama-b/anything"); fault != Found {
		t.Fatal("empty-success provider must route dynamically")
	}
	if _, _, fault := snap.Resolve("llama-a/anything"); fault != Found {
		t.Fatal("stale provider must keep dynamic active")
	}
	if snap.UsableCount() != 0 || snap.Ready() {
		t.Fatalf("stale+empty must be unready: usable=%d", snap.UsableCount())
	}
	names := map[string]bool{}
	for _, m := range snap.Models() {
		names[m.Name] = true
	}
	if !names["s1"] || len(snap.Models()) != 1 {
		t.Fatalf("stale must stay listed exactly once: %+v", names)
	}
}
