// Server runtime publication: atomic publish/copy/retention, stale
// retention, restore reset, failed-publication rollback, empty-server
// runtime, same-pin selector, and configured mapper facts through listing
// and request planning. Hermetic: no network, no filesystem.
package router

import (
	"strings"
	"sync"
	"testing"

	"github.com/arhammahajan/router/internal/capability"
	"github.com/arhammahajan/router/internal/config"
)

func llamaRuntimeCfg() *config.Config {
	return &config.Config{
		Server:    config.Server{},
		Providers: map[string]*config.Provider{},
		Llama: &config.LlamaConfig{
			Servers: []config.LlamaServer{
				{URL: "http://127.0.0.1:1111", Name: "llama-test"},
			},
			ProbeTimeout: 2, RefreshSeconds: 30,
		},
	}
}

func commitLlamaRuntime(t *testing.T, reg *Registry, rt capability.LocalRuntime, obs *capability.LlamaModelObs) {
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

func llamaRuntimeObs() *capability.LlamaModelObs {
	return capability.ExtractLlamaObs(map[string]any{"id": "x"}, map[string]any{"build_info": "llama.cpp"}, "single")
}

func wireLeaf(t *testing.T, root map[string]any, path string) map[string]any {
	t.Helper()
	cur := root
	for _, p := range strings.Split(path, ".") {
		next, ok := cur[p].(map[string]any)
		if !ok {
			t.Fatalf("missing path %s", path)
		}
		cur = next
	}
	return cur
}

func TestRuntimePublishesAtomically(t *testing.T) {
	reg := NewRegistry(llamaRuntimeCfg())
	commitLlamaRuntime(t, reg, capability.RuntimeLlamaCpp, llamaRuntimeObs())
	snap := reg.Current()
	if got := snap.LlamaRuntime("llama-test"); got != capability.RuntimeLlamaCpp {
		t.Fatalf("runtime: %v", got)
	}
	if !snap.HasLlamaProvider("llama-test") {
		t.Fatalf("membership must hold alongside runtime")
	}
	// Same pin serves concurrent request planning and listing.
	var mu sync.Mutex
	var seen capability.LocalRuntime
	done := make(chan bool)
	for i := 0; i < 8; i++ {
		go func() {
			rt := reg.Current().LlamaRuntime("llama-test")
			mu.Lock()
			seen = rt
			mu.Unlock()
			done <- true
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	mu.Lock()
	defer mu.Unlock()
	if seen != capability.RuntimeLlamaCpp {
		t.Fatalf("concurrent pin: %v", seen)
	}
}

func TestRuntimeStrataSnapshot(t *testing.T) {
	reg := NewRegistry(llamaRuntimeCfg())
	obs := capability.ExtractLlamaObs(map[string]any{"id": "x"}, map[string]any{"build_info": "PolyStrata 0.1.45"}, "single")
	commitLlamaRuntime(t, reg, capability.RuntimeStrata, obs)
	snap := reg.Current()
	if got := snap.LlamaRuntime("llama-test"); got != capability.RuntimeStrata {
		t.Fatalf("runtime: %v", got)
	}
}

func TestRuntimeUnknownProvider(t *testing.T) {
	reg := NewRegistry(llamaRuntimeCfg())
	if got := reg.Current().LlamaRuntime("nope"); got != capability.RuntimeUnobserved {
		t.Fatalf("unknown provider must be Unobserved: %v", got)
	}
	var nilSnap *Snapshot
	if got := nilSnap.LlamaRuntime("x"); got != capability.RuntimeUnobserved {
		t.Fatalf("nil snapshot must be Unobserved: %v", got)
	}
	if reg.Current().HasLlamaProvider("nope") {
		t.Fatalf("unknown never becomes local")
	}
}

func TestRuntimeInvalidRejected(t *testing.T) {
	// A failed publication rolls back both runtime and staged layers:
	// routes, times and facts stay as committed before.
	reg := NewRegistry(llamaRuntimeCfg())
	commitLlamaRuntime(t, reg, capability.RuntimeLlamaCpp, llamaRuntimeObs())
	before := reg.Current()
	layer := LlamaLayer{Servers: map[string]LlamaServerSnapshot{
		"llama-test": {State: LlamaReachable, Runtime: capability.LocalRuntime(77),
			Entries: []LlamaEntry{{Name: "llama-chat", Upstream: "llama-chat", Obs: llamaRuntimeObs()}}},
	}}
	if err := reg.CommitLlama(LlamaAttempt{Layer: layer, AtWall: Wall(2000)}); err == nil {
		t.Fatalf("unknown runtime must be rejected")
	}
	after := reg.Current()
	if after.LlamaRuntime("llama-test") != capability.RuntimeLlamaCpp {
		t.Fatalf("runtime must roll back: %v", after.LlamaRuntime("llama-test"))
	}
	if len(after.Models()) != len(before.Models()) {
		t.Fatalf("staged layers must roll back")
	}
}

func TestRuntimeStaleRetains(t *testing.T) {
	reg := NewRegistry(llamaRuntimeCfg())
	obs := llamaRuntimeObs()
	commitLlamaRuntime(t, reg, capability.RuntimeLlamaCpp, obs)
	layer := LlamaLayer{Servers: map[string]LlamaServerSnapshot{
		"llama-test": {State: LlamaUnreachableStale, Runtime: capability.RuntimeLlamaCpp,
			Entries: []LlamaEntry{{Name: "llama-chat", Upstream: "llama-chat", Obs: obs.Clone()}}},
	}}
	if err := reg.CommitLlama(LlamaAttempt{Layer: layer, AtWall: Wall(2000)}); err != nil {
		t.Fatal(err)
	}
	snap := reg.Current()
	if got := snap.LlamaRuntime("llama-test"); got != capability.RuntimeLlamaCpp {
		t.Fatalf("stale must retain runtime: %v", got)
	}
	if !snap.HasLlamaProvider("llama-test") {
		t.Fatalf("stale keeps membership")
	}
	// Stale keeps mapper facts: the retained engine still plans.
	base, ok := snap.LlamaBase("llama-chat")
	if !ok {
		t.Fatalf("stale base missing")
	}
	wire := mustLlamaWire(t, capability.Render(base, nil, snap.LlamaDiscovery("llama-test", 30, Mono(2000)), 2000))
	if got := wireLeaf(t, wire, "reasoning.effort.supported")["value"]; got != true {
		t.Fatalf("stale must keep mapper facts: %v", got)
	}
}

func TestRuntimeRestoreResets(t *testing.T) {
	// Restore drops runtime to Unobserved, ignoring any programmatic live
	// runtime in the staged inventory, just as it drops live Obs. The
	// restored server keeps route-only membership and publishes no mapper
	// claims until a successful probe.
	reg := NewRegistry(llamaRuntimeCfg())
	inv := DiscoveryInventory{Llama: map[string]LlamaInventory{
		"llama-test": {Entries: []LlamaEntry{{Name: "llama-chat", Upstream: "llama-chat", Obs: llamaRuntimeObs()}}, SucceededAt: Wall(900)},
	}}
	if err := reg.RestoreCached(inv); err != nil {
		t.Fatal(err)
	}
	snap := reg.Current()
	if got := snap.LlamaRuntime("llama-test"); got != capability.RuntimeUnobserved {
		t.Fatalf("restore must reset runtime: %v", got)
	}
	if !snap.HasLlamaProvider("llama-test") {
		t.Fatalf("restored unknown keeps membership")
	}
	if _, ok := snap.LlamaBase("llama-chat"); ok {
		t.Fatalf("restored row must carry no base")
	}
}

func TestRuntimeEmptyServerAvailable(t *testing.T) {
	// A valid empty server still publishes its runtime and stays
	// available for dynamic routing.
	reg := NewRegistry(llamaRuntimeCfg())
	layer := LlamaLayer{Servers: map[string]LlamaServerSnapshot{
		"llama-test": {State: LlamaReachable, Runtime: capability.RuntimeLlamaCpp},
	}}
	if err := reg.CommitLlama(LlamaAttempt{Layer: layer, AtWall: Wall(1000)}); err != nil {
		t.Fatal(err)
	}
	snap := reg.Current()
	if got := snap.LlamaRuntime("llama-test"); got != capability.RuntimeLlamaCpp {
		t.Fatalf("empty server must publish runtime: %v", got)
	}
	if !snap.HasLlamaProvider("llama-test") {
		t.Fatalf("empty server stays local")
	}
}

func TestRuntimeMapperFactsSharedSelector(t *testing.T) {
	// Listing and request planning share the same pinned selector: the
	// configured mapper facts appear with source/kind/verification, no
	// invented reasoning.supported/budget/default, and operator
	// false/empty overrides stay authoritative through Normalize.
	reg := NewRegistry(llamaRuntimeCfg())
	commitLlamaRuntime(t, reg, capability.RuntimeLlamaCpp, llamaRuntimeObs())
	snap := reg.Current()
	base := snap.CanonicalBase("llama-chat", "openai-chat")
	wire := mustLlamaWire(t, capability.Render(base, snap.CapabilityOverrides("llama-chat"),
		snap.LlamaDiscovery("llama-test", 30, Mono(1000)), 1000))
	if got := wireLeaf(t, wire, "reasoning.effort.supported")["value"]; got != true {
		t.Fatalf("mapper supported: %v", got)
	}
	vals := wireLeaf(t, wire, "reasoning.effort.values")["value"].([]any)
	if len(vals) != 6 {
		t.Fatalf("mapper values: %v", vals)
	}
	if got := wireLeaf(t, wire, "reasoning.supported")["value"]; got != nil {
		t.Fatalf("no invented reasoning.supported: %v", got)
	}
	if got := wireLeaf(t, wire, "reasoning.budget.supported")["value"]; got != nil {
		t.Fatalf("no invented budget support: %v", got)
	}
	if got := wireLeaf(t, wire, "reasoning.toggle.supported")["value"]; got != nil {
		t.Fatalf("no llama toggle: %v", got)
	}
	if got := wireLeaf(t, wire, "reasoning.effort.native_parameter")["kind"]; got != "configured" {
		t.Fatalf("mapper kind: %v", got)
	}
	// The request selector sees the same base through the pinned snapshot.
	model, provider, fault := snap.Resolve("llama-chat")
	if fault != Found {
		t.Fatalf("resolve: %v", fault)
	}
	req := snap.RequestCapability(model, provider)
	reqWire := mustLlamaWire(t, capability.Render(req, nil, capability.StaticDiscovery(), 2000))
	if got := wireLeaf(t, reqWire, "reasoning.effort.values")["value"]; got == nil {
		t.Fatalf("request planning must share mapper base")
	}
}

func TestRuntimeOperatorOverrideAuthoritative(t *testing.T) {
	// Operator false/empty stays authoritative through overrides+Normalize
	// (applied after the mapper facts), keeping conflict evidence.
	cfg := llamaRuntimeCfg()
	raw := map[string]any{"llama-chat": map[string]any{
		"reasoning": map[string]any{"effort": map[string]any{"supported": false}},
	}}
	ov, err := capability.ValidateOverrides(raw)
	if err != nil {
		t.Fatal(err)
	}
	cfg.CapabilityOverrides = ov
	reg := NewRegistry(cfg)
	commitLlamaRuntime(t, reg, capability.RuntimeLlamaCpp, llamaRuntimeObs())
	snap := reg.Current()
	base := snap.CanonicalBase("llama-chat", "openai-chat")
	wire := mustLlamaWire(t, capability.Render(base, snap.CapabilityOverrides("llama-chat"),
		snap.LlamaDiscovery("llama-test", 30, Mono(1000)), 1000))
	eff := wireLeaf(t, wire, "reasoning.effort.supported")
	if eff["value"] != false || eff["kind"] != "override" {
		t.Fatalf("operator false must win: %v", eff)
	}
	confs, _ := eff["conflicts"].([]any)
	if len(confs) == 0 {
		t.Fatalf("mapper conflict must be kept: %v", eff)
	}
	if got := wireLeaf(t, wire, "reasoning.effort.values")["value"]; got != nil {
		t.Fatalf("Normalize must suppress dependents: %v", got)
	}
}

func TestRuntimeStrataFacts(t *testing.T) {
	reg := NewRegistry(llamaRuntimeCfg())
	obs := capability.ExtractLlamaObs(map[string]any{"id": "x"}, map[string]any{"build_info": "strata"}, "single")
	commitLlamaRuntime(t, reg, capability.RuntimeStrata, obs)
	snap := reg.Current()
	base := snap.CanonicalBase("llama-chat", "openai-chat")
	wire := mustLlamaWire(t, capability.Render(base, nil, capability.StaticDiscovery(), 2000))
	vals := wireLeaf(t, wire, "reasoning.effort.values")["value"].([]any)
	if len(vals) != 4 {
		t.Fatalf("strata values: %v", vals)
	}
	if got := wireLeaf(t, wire, "reasoning.toggle.supported")["value"]; got != true {
		t.Fatalf("strata toggle: %v", wire)
	}
	if got := wireLeaf(t, wire, "reasoning.budget.supported")["value"]; got != nil {
		t.Fatalf("strata must not invent budget: %v", got)
	}
}
