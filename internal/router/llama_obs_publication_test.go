package router

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/arhammahajan/model-router/internal/capability"
	"github.com/arhammahajan/model-router/internal/config"
)

func llamaObsPubCfg() *config.Config {
	return &config.Config{
		Server:    config.Server{},
		Providers: map[string]*config.Provider{},
		Llama: &config.LlamaConfig{
			Servers: []config.LlamaServer{
				{URL: "http://127.0.0.1:1111", Name: "llama-a"},
				{URL: "http://127.0.0.1:2222", Name: "llama-b"},
			},
			ProbeTimeout: 2, RefreshSeconds: 30,
		},
	}
}

func llamaObsRowMap(mods []any) map[string]any {
	return map[string]any{"id": "m1", "architecture": map[string]any{"input_modalities": mods}}
}

func llamaObsEntries(name, upstream string, obs *capability.LlamaModelObs) []LlamaEntry {
	return []LlamaEntry{{Name: name, Upstream: upstream, Obs: obs}}
}

func llamaObsCommit(t *testing.T, reg *Registry, name string, entries []LlamaEntry, wall float64) {
	t.Helper()
	layer := LlamaLayer{Servers: map[string]LlamaServerSnapshot{}}
	for _, srv := range reg.LlamaOrder() {
		layer.Servers[srv] = LlamaServerSnapshot{State: LlamaNeverSucceeded}
	}
	layer.Servers[name] = LlamaServerSnapshot{State: LlamaReachable, Entries: entries}
	if err := reg.CommitLlama(LlamaAttempt{Layer: layer, AtWall: Wall(wall)}); err != nil {
		t.Fatal(err)
	}
}

func TestLlamaObsPublishWallAndStale(t *testing.T) {
	reg := NewRegistry(llamaObsPubCfg())
	obs := capability.ExtractLlamaObs(llamaObsRowMap([]any{"text"}), map[string]any{"build_info": "b", "x": 1}, "single")
	llamaObsCommit(t, reg, "llama-a", llamaObsEntries("m1", "m1", obs), 1000)
	snap := reg.Current()
	base, ok := snap.LlamaBase("m1")
	if !ok {
		t.Fatalf("fresh base missing")
	}
	rendered := capability.Render(base, snap.CapabilityOverrides("m1"), snap.LlamaDiscovery("llama-a", 30, Mono(1000)), 1000)
	raw, _ := rendered.MarshalJSON()
	_ = raw
	// Stale retains old wall, not new fleet time.
	layer := LlamaLayer{Servers: map[string]LlamaServerSnapshot{}}
	for _, srv := range reg.LlamaOrder() {
		layer.Servers[srv] = LlamaServerSnapshot{State: LlamaNeverSucceeded}
	}
	// Server a stale (retains obs copy), server b fresh with new obs.
	staleEntries := []LlamaEntry{{Name: "m1", Upstream: "m1", Obs: obs.Clone()}}
	layer.Servers["llama-a"] = LlamaServerSnapshot{State: LlamaUnreachableStale, Entries: staleEntries}
	obs2 := capability.ExtractLlamaObs(llamaObsRowMap([]any{"text", "image"}), map[string]any{"build_info": "b2", "y": 1}, "single")
	layer.Servers["llama-b"] = LlamaServerSnapshot{State: LlamaReachable, Entries: []LlamaEntry{{Name: "m2", Upstream: "m2", Obs: obs2}}}
	if err := reg.CommitLlama(LlamaAttempt{Layer: layer, AtWall: Wall(2000)}); err != nil {
		t.Fatal(err)
	}
	snap = reg.Current()
	b1, ok := snap.LlamaBase("m1")
	if !ok {
		t.Fatalf("stale base missing")
	}
	r1 := capability.Render(b1, nil, snap.LlamaDiscovery("llama-a", 30, Mono(2000)), 2000)
	wire1 := mustLlamaWire(t, r1)
	if got := wire1["sources"].(map[string]any)["llama.models"].(map[string]any)["retrieved_at"]; got != float64(1000) {
		t.Fatalf("stale must keep old wall, got %v", got)
	}
	b2, ok := snap.LlamaBase("m2")
	if !ok {
		t.Fatalf("fresh b missing")
	}
	r2 := capability.Render(b2, nil, snap.LlamaDiscovery("llama-b", 30, Mono(2000)), 2000)
	wire2 := mustLlamaWire(t, r2)
	if got := wire2["sources"].(map[string]any)["llama.models"].(map[string]any)["retrieved_at"]; got != float64(2000) {
		t.Fatalf("fresh must advance, got %v", got)
	}
}

func mustLlamaWire(t *testing.T, c capability.Contract) map[string]any {
	t.Helper()
	raw, err := c.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return mustLlamaJSON(t, raw)
}

func mustLlamaJSON(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestLlamaObsRestoreUnknown(t *testing.T) {
	reg := NewRegistry(llamaObsPubCfg())
	obs := capability.ExtractLlamaObs(llamaObsRowMap([]any{"text"}), map[string]any{"a": 1}, "single")
	llamaObsCommit(t, reg, "llama-a", llamaObsEntries("m1", "m1", obs), 500)
	inv := reg.ExportDiscovery()
	// Route-only cache drops obs: simulate file round-trip with nil obs.
	for name, unit := range inv.Llama {
		cp := make([]LlamaEntry, 0, len(unit.Entries))
		for _, e := range unit.Entries {
			cp = append(cp, LlamaEntry{Name: e.Name, Upstream: e.Upstream, Aliases: e.Aliases})
		}
		inv.Llama[name] = LlamaInventory{Entries: cp, SucceededAt: unit.SucceededAt}
	}
	reg2 := NewRegistry(llamaObsPubCfg())
	if err := reg2.RestoreCached(inv); err != nil {
		t.Fatal(err)
	}
	if _, ok := reg2.Current().LlamaBase("m1"); ok {
		t.Fatalf("restored rows must be unknown until fresh probe")
	}
}

func TestLlamaObsCollisionShadow(t *testing.T) {
	cfg := &config.Config{
		Server:    config.Server{},
		Providers: map[string]*config.Provider{},
		Models:    []config.Model{{Name: "m1", Provider: "static", Upstream: "u"}},
		Llama: &config.LlamaConfig{
			Servers:      []config.LlamaServer{{URL: "http://127.0.0.1:1111", Name: "llama-a"}, {URL: "http://127.0.0.1:2222", Name: "llama-b"}},
			ProbeTimeout: 2,
		},
	}
	cfg.Providers["static"] = &config.Provider{Name: "static", Dialect: "openai-chat"}
	reg := NewRegistry(cfg)
	losing := capability.ExtractLlamaObs(llamaObsRowMap([]any{"text", "image"}), map[string]any{"modalities": map[string]any{"vision": true}}, "single")
	layer := LlamaLayer{Servers: map[string]LlamaServerSnapshot{}}
	for _, srv := range reg.LlamaOrder() {
		layer.Servers[srv] = LlamaServerSnapshot{State: LlamaNeverSucceeded}
	}
	layer.Servers["llama-a"] = LlamaServerSnapshot{State: LlamaReachable, Entries: []LlamaEntry{{Name: "m1", Upstream: "m1", Obs: losing}}}
	layer.Servers["llama-b"] = LlamaServerSnapshot{State: LlamaReachable, Entries: []LlamaEntry{{Name: "m1", Upstream: "m1-dup", Obs: losing}}}
	if err := reg.CommitLlama(LlamaAttempt{Layer: layer, AtWall: Wall(100)}); err != nil {
		t.Fatal(err)
	}
	// Explicit reserves the name: no llama base may leak.
	if _, ok := reg.Current().LlamaBase("m1"); ok {
		t.Fatalf("shadowed losing row must not publish caps")
	}
}

func TestLlamaObsCopyIsolationAndRace(t *testing.T) {
	reg := NewRegistry(llamaObsPubCfg())
	obs := capability.ExtractLlamaObs(llamaObsRowMap([]any{"text"}), map[string]any{"build_info": "b"}, "single")
	entries := []LlamaEntry{{Name: "m1", Upstream: "m1", Obs: obs}}
	llamaObsCommit(t, reg, "llama-a", entries, 100)
	entries[0].Obs = nil
	b, ok := reg.Current().LlamaBase("m1")
	if !ok {
		t.Fatalf("base missing after caller mutation")
	}
	_ = b
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snap := reg.Current()
			if base, ok := snap.LlamaBase("m1"); ok {
				_ = capability.Render(base, snap.CapabilityOverrides("m1"), snap.LlamaDiscovery("llama-a", 30, Mono(100)), 100)
			}
			_ = snap.ListedModels()
		}()
	}
	wg.Wait()
}
