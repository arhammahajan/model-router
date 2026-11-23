// Independent review: publication isolation and same-pin selector boundaries.
// Hermetic: no network, no filesystem.
package router

import (
	"sync"
	"testing"

	"github.com/arhammahajan/router/internal/capability"
	"github.com/arhammahajan/router/internal/config"
)

func auditRouterCfg() *config.Config {
	return &config.Config{
		Server:    config.Server{},
		Providers: map[string]*config.Provider{},
		Llama: &config.LlamaConfig{
			Servers:        []config.LlamaServer{{URL: "http://127.0.0.1:1111", Name: "llama-test"}},
			ProbeTimeout:   2,
			RefreshSeconds: 30,
		},
	}
}

func auditCommit(t *testing.T, reg *Registry, rt capability.LocalRuntime, obs *capability.LlamaModelObs) {
	t.Helper()
	layer := LlamaLayer{Servers: map[string]LlamaServerSnapshot{}}
	for _, srv := range reg.LlamaOrder() {
		layer.Servers[srv] = LlamaServerSnapshot{State: LlamaNeverSucceeded}
	}
	layer.Servers["llama-test"] = LlamaServerSnapshot{State: LlamaReachable, Runtime: rt,
		Entries: []LlamaEntry{{Name: "llama-chat", Upstream: "llama-chat", Obs: obs}}}
	if err := reg.CommitLlama(LlamaAttempt{Layer: layer, AtWall: Wall(1000)}); err != nil {
		t.Fatal(err)
	}
}

func auditObs() *capability.LlamaModelObs {
	return capability.ExtractLlamaObs(map[string]any{"id": "x"}, map[string]any{"build_info": "llama.cpp"}, "single")
}

func TestAuditRouterInputMutationIsolation(t *testing.T) {
	// Mutating the caller-owned layer after CommitLlama must not affect the
	// pinned snapshot (copy across the publication boundary).
	reg := NewRegistry(auditRouterCfg())
	obs := auditObs()
	entries := []LlamaEntry{{Name: "llama-chat", Upstream: "llama-chat", Obs: obs, Aliases: []string{"a"}}}
	layer := LlamaLayer{Servers: map[string]LlamaServerSnapshot{
		"llama-test": {State: LlamaReachable, Runtime: capability.RuntimeLlamaCpp, Entries: entries},
	}}
	if err := reg.CommitLlama(LlamaAttempt{Layer: layer, AtWall: Wall(1000)}); err != nil {
		t.Fatal(err)
	}
	// Mutate caller inputs: entries, aliases, runtime value.
	entries[0].Name = "mutated"
	entries[0].Aliases[0] = "mutated"
	layer.Servers["llama-test"] = LlamaServerSnapshot{State: LlamaReachable, Runtime: capability.RuntimeStrata}
	snap := reg.Current()
	if got := snap.LlamaRuntime("llama-test"); got != capability.RuntimeLlamaCpp {
		t.Fatalf("runtime must be isolated: %v", got)
	}
	if _, _, fault := snap.Resolve("llama-chat"); fault != Found {
		t.Fatalf("canonical must survive input mutation")
	}
	if _, _, fault := snap.Resolve("mutated"); fault == Found {
		t.Fatalf("mutation must not leak into snapshot")
	}
}

func TestAuditRouterConcurrentPinAcrossEngineSwap(t *testing.T) {
	// Readers pinning before an engine swap keep the old runtime; new pins
	// see the new engine. No lock on the read path.
	reg := NewRegistry(auditRouterCfg())
	auditCommit(t, reg, capability.RuntimeLlamaCpp, auditObs())
	before := reg.Current()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := before.LlamaRuntime("llama-test"); got != capability.RuntimeLlamaCpp {
				t.Errorf("old pin must stay LlamaCpp: %v", got)
			}
		}()
	}
	layer := LlamaLayer{Servers: map[string]LlamaServerSnapshot{
		"llama-test": {State: LlamaReachable, Runtime: capability.RuntimeStrata,
			Entries: []LlamaEntry{{Name: "llama-chat", Upstream: "llama-chat", Obs: auditObs()}}},
	}}
	if err := reg.CommitLlama(LlamaAttempt{Layer: layer, AtWall: Wall(2000)}); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if got := reg.Current().LlamaRuntime("llama-test"); got != capability.RuntimeStrata {
		t.Fatalf("new pin must see Strata: %v", got)
	}
	if got := before.LlamaRuntime("llama-test"); got != capability.RuntimeLlamaCpp {
		t.Fatalf("old pin must not change: %v", got)
	}
}

func TestAuditRouterRestoreDropsLiveObs(t *testing.T) {
	// Even when the staged inventory carries a live observation, restore
	// must publish route-only rows with no base and Unobserved runtime.
	reg := NewRegistry(auditRouterCfg())
	inv := DiscoveryInventory{Llama: map[string]LlamaInventory{
		"llama-test": {Entries: []LlamaEntry{{Name: "llama-chat", Upstream: "llama-chat", Obs: auditObs()}}, SucceededAt: Wall(900)},
	}}
	if err := reg.RestoreCached(inv); err != nil {
		t.Fatal(err)
	}
	snap := reg.Current()
	if got := snap.LlamaRuntime("llama-test"); got != capability.RuntimeUnobserved {
		t.Fatalf("restore must be Unobserved: %v", got)
	}
	if _, ok := snap.LlamaBase("llama-chat"); ok {
		t.Fatalf("restored row must carry no base")
	}
	if !snap.HasLlamaProvider("llama-test") {
		t.Fatalf("restored unknown keeps membership")
	}
}

func TestAuditRouterDynamicUsesPinnedRuntime(t *testing.T) {
	// Dynamic provider/model routes on an empty server resolve and plan
	// through the pinned server runtime, not through caps.
	reg := NewRegistry(auditRouterCfg())
	layer := LlamaLayer{Servers: map[string]LlamaServerSnapshot{
		"llama-test": {State: LlamaReachable, Runtime: capability.RuntimeLlamaCpp},
	}}
	if err := reg.CommitLlama(LlamaAttempt{Layer: layer, AtWall: Wall(1000)}); err != nil {
		t.Fatal(err)
	}
	snap := reg.Current()
	if !snap.HasLlamaProvider("llama-test") {
		t.Fatalf("empty server stays local")
	}
	if got := snap.LlamaRuntime("llama-test"); got != capability.RuntimeLlamaCpp {
		t.Fatalf("empty server must publish runtime: %v", got)
	}
}
