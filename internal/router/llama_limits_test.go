package router

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/arhammahajan/router/internal/capability"
	"github.com/arhammahajan/router/internal/config"
)

// Focused limit-policy publication: prefix/local rename/raw-ID matching,
// alias sharing, loser isolation, cached-unknown/stale, copy/race, and the
// typed Gemini read path.

func limCfg(fallback *int64, ov map[string]map[string]*int64, prefix string) *config.Config {
	return &config.Config{
		Server:    config.Server{},
		Providers: map[string]*config.Provider{},
		Llama: &config.LlamaConfig{
			Servers:         []config.LlamaServer{{URL: "http://127.0.0.1:1111", Name: "llama-a"}, {URL: "http://127.0.0.1:2222", Name: "llama-b"}},
			ProbeTimeout:    2,
			RefreshSeconds:  30,
			Prefix:          prefix,
			ExposeAliases:   true,
			Compatibility:   "strict",
			ContextFallback: fallback,
			LimitOverrides:  ov,
		},
	}
}

func limCommit(t *testing.T, reg *Registry, name string, entries []LlamaEntry, wall float64) {
	t.Helper()
	layer := LlamaLayer{Servers: map[string]LlamaServerSnapshot{}}
	for _, srv := range reg.LlamaOrder() {
		layer.Servers[srv] = LlamaServerSnapshot{State: LlamaNeverSucceeded}
	}
	layer.Servers[name] = LlamaServerSnapshot{State: LlamaReachable, Entries: entries, Runtime: capability.RuntimeLlamaCpp}
	if err := reg.CommitLlama(LlamaAttempt{Layer: layer, AtWall: Wall(wall)}); err != nil {
		t.Fatal(err)
	}
}

func limWire(t *testing.T, c capability.Contract) map[string]any {
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

func limVal(t *testing.T, c capability.Contract, key string) map[string]any {
	t.Helper()
	return limWire(t, capability.Render(c, nil, capability.StaticDiscovery(), 1))["limits"].(map[string]any)[key].(map[string]any)
}

func i64p(v int64) *int64 { return &v }

func TestLlamaLimitsPrefixLocalRawID(t *testing.T) {
	fb := int64(64000)
	ov := map[string]map[string]*int64{"m1": {"context_window": i64p(111)}, "local": {"context_window": i64p(222)}}
	reg := NewRegistry(limCfg(&fb, ov, "pre-"))
	obsEmpty := capability.ExtractLlamaObs(map[string]any{}, nil, "single")
	obsSeen := capability.ExtractLlamaObs(map[string]any{"meta": map[string]any{"n_ctx": json.Number("4096")}}, nil, "single")
	limCommit(t, reg, "llama-a", []LlamaEntry{
		{Name: "pre-m1", Upstream: "m1", Obs: obsEmpty},
		{Name: "pre-m2", Upstream: "m2", Obs: obsEmpty},
		{Name: "pre-local-llama", Upstream: "local", Obs: obsEmpty},
		{Name: "pre-seen", Upstream: "seen", Obs: obsSeen},
	}, 100)
	snap := reg.Current()
	// Raw-ID override wins before prefix.
	if got := limVal(t, snap.CanonicalBase("pre-m1", "openai-chat"), "context_window"); got["value"] != float64(111) || got["kind"] != "override" {
		t.Fatalf("raw-id override: %v", got)
	}
	// No override falls back.
	if got := limVal(t, snap.CanonicalBase("pre-m2", "openai-chat"), "context_window"); got["value"] != float64(64000) || got["kind"] != "fallback" {
		t.Fatalf("fallback: %v", got)
	}
	// local rename matches raw "local".
	if got := limVal(t, snap.CanonicalBase("pre-local-llama", "openai-chat"), "context_window"); got["value"] != float64(222) {
		t.Fatalf("local rename: %v", got)
	}
	// Observed beats fallback.
	if got := limVal(t, snap.CanonicalBase("pre-seen", "openai-chat"), "context_window"); got["value"] != float64(4096) || got["kind"] != "reported" {
		t.Fatalf("observed must beat fallback: %v", got)
	}
}

func TestLlamaLimitsAliasSharesLoserHidden(t *testing.T) {
	reg := NewRegistry(limCfg(i64p(10), map[string]map[string]*int64{"canon": {"context_window": i64p(99)}}, ""))
	obs := capability.ExtractLlamaObs(map[string]any{}, nil, "single")
	layer := LlamaLayer{Servers: map[string]LlamaServerSnapshot{}}
	for _, srv := range reg.LlamaOrder() {
		layer.Servers[srv] = LlamaServerSnapshot{State: LlamaNeverSucceeded}
	}
	layer.Servers["llama-a"] = LlamaServerSnapshot{State: LlamaReachable, Runtime: capability.RuntimeLlamaCpp,
		Entries: []LlamaEntry{{Name: "canon", Upstream: "canon", Aliases: []string{"al"}, Obs: obs}}}
	layer.Servers["llama-b"] = LlamaServerSnapshot{State: LlamaReachable, Runtime: capability.RuntimeLlamaCpp,
		Entries: []LlamaEntry{{Name: "canon", Upstream: "dup", Obs: obs}}}
	if err := reg.CommitLlama(LlamaAttempt{Layer: layer, AtWall: Wall(50)}); err != nil {
		t.Fatal(err)
	}
	snap := reg.Current()
	// Winner publishes; alias shares the canonical fact.
	winner, ok := snap.LlamaBase("canon")
	if !ok {
		t.Fatal("winner must publish")
	}
	if _, ok := snap.LlamaBase("al"); ok {
		t.Fatal("alias must not carry its own base")
	}
	aliasBase := snap.CanonicalBase("al", "openai-chat")
	winnerWire := limWire(t, capability.Render(winner, nil, capability.StaticDiscovery(), 1))
	aliasWire := limWire(t, capability.Render(aliasBase, nil, capability.StaticDiscovery(), 1))
	if winnerWire["limits"].(map[string]any)["context_window"].(map[string]any)["value"] !=
		aliasWire["limits"].(map[string]any)["context_window"].(map[string]any)["value"] {
		t.Fatalf("alias must share canonical fact: %v vs %v", winnerWire, aliasWire)
	}
}

func TestLlamaLimitsRestoredUnknownAndStale(t *testing.T) {
	fb := int64(32000)
	reg := NewRegistry(limCfg(&fb, map[string]map[string]*int64{"m1": {"max_output_tokens": i64p(77)}}, ""))
	inv := DiscoveryInventory{Llama: map[string]LlamaInventory{
		"llama-a": {Entries: []LlamaEntry{{Name: "m1", Upstream: "m1"}}, SucceededAt: Wall(500)},
		"llama-b": {Entries: []LlamaEntry{{Name: "m2", Upstream: "m2"}}, SucceededAt: Wall(500)},
	}}
	if err := reg.RestoreCached(inv); err != nil {
		t.Fatal(err)
	}
	snap := reg.Current()
	// Restored route resolves but carries no live runtime.
	if rt := snap.LlamaRuntime("llama-a"); rt != capability.RuntimeUnobserved {
		t.Fatalf("restored runtime must stay Unobserved, got %v", rt)
	}
	// Explicit policy reapplies on the empty base (config facts, not cached
	// observations): m1 gets fallback+override, m2 gets fallback only.
	b1 := snap.CanonicalBase("m1", "openai-chat")
	if got := limVal(t, b1, "context_window")["value"]; got != float64(32000) {
		t.Fatalf("restored fallback: %v", got)
	}
	if got := limVal(t, b1, "max_output_tokens")["value"]; got != float64(77) {
		t.Fatalf("restored override: %v", got)
	}
	b2 := snap.CanonicalBase("m2", "openai-chat")
	if got := limVal(t, b2, "context_window")["value"]; got != float64(32000) {
		t.Fatalf("restored m2 fallback: %v", got)
	}
	// No fake probe sources.
	wire := limWire(t, capability.Render(b1, nil, snap.LlamaDiscovery("llama-a", 30, Mono(600)), 600))
	srcs := wire["sources"].(map[string]any)
	if _, has := srcs["llama.models"]; has {
		t.Fatalf("restored must not fabricate probe sources: %v", srcs)
	}
	// Stale retains original wall/facts.
	layer := LlamaLayer{Servers: map[string]LlamaServerSnapshot{}}
	for _, srv := range reg.LlamaOrder() {
		layer.Servers[srv] = LlamaServerSnapshot{State: LlamaNeverSucceeded}
	}
	retained := reg.ExportDiscovery().Llama["llama-a"].Entries
	layer.Servers["llama-a"] = LlamaServerSnapshot{State: LlamaUnreachableStale, Runtime: capability.RuntimeUnobserved, Entries: retained}
	if err := reg.CommitLlama(LlamaAttempt{Layer: layer, AtWall: Wall(900)}); err != nil {
		t.Fatal(err)
	}
	if got := limVal(t, reg.Current().CanonicalBase("m1", "openai-chat"), "context_window")["value"]; got != float64(32000) {
		t.Fatalf("stale must retain policy facts: %v", got)
	}
}

func TestLlamaLimitsCopyRace(t *testing.T) {
	fb := int64(100)
	cfg := limCfg(&fb, map[string]map[string]*int64{"m1": {"context_window": i64p(5)}}, "")
	reg := NewRegistry(cfg)
	// Mutating config after New must not reach the snapshot.
	*cfg.Llama.ContextFallback = 999
	*cfg.Llama.LimitOverrides["m1"]["context_window"] = 999
	obs := capability.ExtractLlamaObs(map[string]any{}, nil, "single")
	limCommit(t, reg, "llama-a", []LlamaEntry{{Name: "m1", Upstream: "m1", Obs: obs}}, 10)
	if got := limVal(t, reg.Current().CanonicalBase("m1", "openai-chat"), "context_window")["value"]; got != float64(5) {
		t.Fatalf("config mutation leaked: %v", got)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snap := reg.Current()
			_ = snap.CanonicalBase("m1", "openai-chat")
			_ = snap.RequestCapability(&config.Model{Name: "m1", Provider: "llama-a", Upstream: "m1"}, &config.Provider{Name: "llama-a", DefaultDialect: "openai-chat"})
			_ = snap.ListedModels()
		}()
	}
	wg.Wait()
}

func TestLlamaLimitsGeminiTypedRead(t *testing.T) {
	// Gemini views read typed limits (context fallback), never serialized
	// metadata or generated max_tokens. This mirrors the existing Gemini
	// limit selection: max_input_tokens else context_window.
	reg := NewRegistry(limCfg(i64p(48000), nil, ""))
	obs := capability.ExtractLlamaObs(map[string]any{}, nil, "single")
	limCommit(t, reg, "llama-a", []LlamaEntry{{Name: "m1", Upstream: "m1", Obs: obs}}, 20)
	snap := reg.Current()
	base := snap.CanonicalBase("m1", "openai-chat")
	v, ok := base.Get("limits.context_window")
	if !ok {
		t.Fatal("missing context_window")
	}
	n, ok := v.Int()
	if !ok || n != 48000 {
		t.Fatalf("typed fallback must be exact int64, got %v", v)
	}
}
