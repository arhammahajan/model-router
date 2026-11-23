package router

import (
	"encoding/json"
	"testing"

	"github.com/arhammahajan/router/internal/capability"
	"github.com/arhammahajan/router/internal/config"
)

// Independent review of llama publication ownership: per-entry closed obs,
// per-server walls, route-only cache, collision isolation.

func llamaCapRevCfg() *config.Config {
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

func llamaCapRevCommit(t *testing.T, reg *Registry, name string, entries []LlamaEntry, wall float64) {
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

func llamaCapRevWire(t *testing.T, c capability.Contract) map[string]any {
	t.Helper()
	raw, err := c.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestLlamaCapRevRestoreDropsLiveObs(t *testing.T) {
	reg := NewRegistry(llamaCapRevCfg())
	live := capability.ExtractLlamaObs(
		map[string]any{"architecture": map[string]any{"input_modalities": []any{"text", "image"}}},
		map[string]any{"modalities": map[string]any{"vision": true}}, "single")
	inv := DiscoveryInventory{
		Llama: map[string]LlamaInventory{
			"llama-a": {Entries: []LlamaEntry{{Name: "m1", Upstream: "m1", Obs: live}}, SucceededAt: Wall(500)},
		},
	}
	if err := reg.RestoreCached(inv); err != nil {
		t.Fatal(err)
	}
	// Route-only v2 cache: live obs input must not become policy.
	if _, ok := reg.Current().LlamaBase("m1"); ok {
		t.Fatalf("restored live obs must stay unknown until fresh probe")
	}
	snap := reg.Current()
	if len(snap.ListedModels()) == 0 {
		t.Fatalf("restored route must still list")
	}
}

func TestLlamaCapRevStaleRetainsUnknownAfterRestore(t *testing.T) {
	reg := NewRegistry(llamaCapRevCfg())
	inv := DiscoveryInventory{
		Llama: map[string]LlamaInventory{
			"llama-a": {Entries: []LlamaEntry{{Name: "m1", Upstream: "m1"}}, SucceededAt: Wall(500)},
		},
	}
	if err := reg.RestoreCached(inv); err != nil {
		t.Fatal(err)
	}
	// Next failed probe retains the restored unknown entry as stale.
	layer := LlamaLayer{Servers: map[string]LlamaServerSnapshot{}}
	for _, srv := range reg.LlamaOrder() {
		layer.Servers[srv] = LlamaServerSnapshot{State: LlamaNeverSucceeded}
	}
	retained := reg.ExportDiscovery().Llama["llama-a"].Entries
	layer.Servers["llama-a"] = LlamaServerSnapshot{State: LlamaUnreachableStale, Entries: retained}
	if err := reg.CommitLlama(LlamaAttempt{
		Layer:     layer,
		AtWall:    Wall(900),
		Aggregate: SourceEvent{Failures: 1, Err: "cached discovery; live availability unverified"},
		Servers:   reg.ServerEvents(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Current().LlamaBase("m1"); ok {
		t.Fatalf("stale retained unknown must not gain caps without fresh success")
	}
}

func TestLlamaCapRevAliasHasNoBase(t *testing.T) {
	reg := NewRegistry(llamaCapRevCfg())
	obs := capability.ExtractLlamaObs(
		map[string]any{"architecture": map[string]any{"input_modalities": []any{"text", "image"}}},
		map[string]any{"modalities": map[string]any{"vision": true}}, "single")
	layer := LlamaLayer{Servers: map[string]LlamaServerSnapshot{}}
	for _, srv := range reg.LlamaOrder() {
		layer.Servers[srv] = LlamaServerSnapshot{State: LlamaNeverSucceeded}
	}
	layer.Servers["llama-a"] = LlamaServerSnapshot{State: LlamaReachable,
		Entries: []LlamaEntry{{Name: "canon", Upstream: "canon", Aliases: []string{"al"}, Obs: obs}}}
	if err := reg.CommitLlama(LlamaAttempt{Layer: layer, AtWall: Wall(100)}); err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Current().LlamaBase("canon"); !ok {
		t.Fatalf("winner must publish a base")
	}
	if _, ok := reg.Current().LlamaBase("al"); ok {
		t.Fatalf("alias must not carry its own base")
	}
}

func TestLlamaCapRevLosingObsCannotLeak(t *testing.T) {
	cfg := &config.Config{
		Server:    config.Server{},
		Providers: map[string]*config.Provider{"static": {Name: "static", Dialect: "openai-chat"}},
		Models:    []config.Model{{Name: "m1", Provider: "static", Upstream: "u"}},
		Llama: &config.LlamaConfig{
			Servers:      []config.LlamaServer{{URL: "http://127.0.0.1:1111", Name: "llama-a"}},
			ProbeTimeout: 2,
		},
	}
	reg := NewRegistry(cfg)
	obs := capability.ExtractLlamaObs(
		map[string]any{"architecture": map[string]any{"input_modalities": []any{"text", "image"}}},
		map[string]any{"modalities": map[string]any{"vision": true}}, "single")
	layer := LlamaLayer{Servers: map[string]LlamaServerSnapshot{
		"llama-a": {State: LlamaReachable, Entries: []LlamaEntry{{Name: "m1", Upstream: "dup", Obs: obs}}},
	}}
	if err := reg.CommitLlama(LlamaAttempt{Layer: layer, AtWall: Wall(100)}); err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Current().LlamaBase("m1"); ok {
		t.Fatalf("explicit-shadowed losing obs must publish no base")
	}
}

func TestLlamaCapRevExportMutationIsolated(t *testing.T) {
	reg := NewRegistry(llamaCapRevCfg())
	obs := capability.ExtractLlamaObs(
		map[string]any{"meta": map[string]any{"n_ctx": json.Number("10")}},
		map[string]any{"build_info": "b"}, "single")
	llamaCapRevCommit(t, reg, "llama-a", []LlamaEntry{{Name: "m1", Upstream: "m1", Obs: obs}}, 100)
	inv := reg.ExportDiscovery()
	inv.Llama["llama-a"].Entries[0].Name = "mutated"
	inv.Llama["llama-a"].Entries[0].Obs = nil
	base, ok := reg.Current().LlamaBase("m1")
	if !ok {
		t.Fatalf("export mutation must not disturb published base")
	}
	wire := llamaCapRevWire(t, capability.Render(base, nil, capability.StaticDiscovery(), 100))
	lims := wire["limits"].(map[string]any)["context_window"].(map[string]any)
	if lims["value"] != float64(10) {
		t.Fatalf("published base changed after export mutation: %v", lims)
	}
}
