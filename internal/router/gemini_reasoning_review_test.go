package router

import (
	"testing"

	"github.com/arhammahajan/router/internal/capability"
	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/protocol"
)

// Independent adversarial review of the Gemini-request pinning and scope
// selection. Hermetic: programmatic config + Commit shims, no network/env.
// Proves the same-pin contract the bridges rely on: resolve, capability,
// and runtime come from one snapshot; stale retains runtime (freshness
// neutral); unknown stays neutral; metadata.router stays untrusted; the
// actual destination (not the provider default) selects the scope.

func revGemStaticCfg() *config.Config {
	return &config.Config{
		Server: config.Server{},
		Providers: map[string]*config.Provider{
			"chat": {Name: "chat", DefaultDialect: "openai-chat", BaseURL: "http://127.0.0.1:9"},
			"resp": {Name: "resp", DefaultDialect: "openai-responses", BaseURL: "http://127.0.0.1:9"},
		},
		Models: []config.Model{
			{Name: "static-chat", Provider: "chat", Upstream: "u-chat"},
			{Name: "static-resp", Provider: "resp", Upstream: "u-resp"},
			{Name: "over-resp", Provider: "chat", Upstream: "u-over", Dialect: "openai-responses"},
		},
	}
}

func revGemLlamaCfg() *config.Config {
	cfg := revGemStaticCfg()
	cfg.Llama = &config.LlamaConfig{
		Servers:        []config.LlamaServer{{URL: "http://127.0.0.1:1111", Name: "llama-a"}},
		ProbeTimeout:   2,
		RefreshSeconds: 30,
	}
	return cfg
}

func revGemCommit(t *testing.T, reg *Registry, snapState map[string]LlamaServerSnapshot, wall float64) {
	t.Helper()
	layer := LlamaLayer{Servers: map[string]LlamaServerSnapshot{}}
	for _, srv := range reg.LlamaOrder() {
		layer.Servers[srv] = LlamaServerSnapshot{State: LlamaNeverSucceeded}
	}
	for name, ss := range snapState {
		layer.Servers[name] = ss
	}
	if err := reg.CommitLlama(LlamaAttempt{Layer: layer, AtWall: Wall(wall)}); err != nil {
		t.Fatal(err)
	}
}

func TestRevGemSamePinStability(t *testing.T) {
	reg := NewRegistry(revGemLlamaCfg())
	revGemCommit(t, reg, map[string]LlamaServerSnapshot{
		"llama-a": {State: LlamaReachable, Runtime: capability.RuntimeLlamaCpp,
			Entries: []LlamaEntry{{Name: "llama-chat", Upstream: "llama-chat"}}},
	}, 1000)
	snap := reg.Current()
	m, p, fault := snap.Resolve("llama-chat")
	if fault != Found {
		t.Fatalf("resolve: %v", fault)
	}
	// Same pin, repeated reads are identical (no repin mix).
	c1 := snap.RequestCapability(m, p)
	c2 := snap.RequestCapability(m, p)
	if capability.ReasoningFor(c1).EffortSupported != capability.ReasoningFor(c2).EffortSupported {
		t.Fatalf("same-pin caps unstable")
	}
	if rt1, rt2 := snap.LlamaRuntime(p.Name), snap.LlamaRuntime(p.Name); rt1 != rt2 || rt1 != capability.RuntimeLlamaCpp {
		t.Fatalf("same-pin runtime unstable: %v %v", rt1, rt2)
	}
	// Actual destination is the generated provider default (openai-chat);
	// a static route override selects Responses without moving providers.
	if got := m.EffectiveDialect(p); got != "openai-chat" {
		t.Fatalf("llama dest must be provider default chat: %q", got)
	}
	mo, po, fault := snap.Resolve("over-resp")
	if fault != Found {
		t.Fatal("override resolve")
	}
	if got := mo.EffectiveDialect(po); got != "openai-responses" {
		t.Fatalf("route override must select dest: %q", got)
	}
	if po.Name != "chat" {
		t.Fatalf("dest override must not move providers: %+v", po)
	}
	// A later publication does not mutate the pinned snapshot.
	revGemCommit(t, reg, map[string]LlamaServerSnapshot{
		"llama-a": {State: LlamaReachable, Runtime: capability.RuntimeStrata,
			Entries: []LlamaEntry{{Name: "llama-chat", Upstream: "llama-chat"}}},
	}, 2000)
	if snap.LlamaRuntime(p.Name) != capability.RuntimeLlamaCpp {
		t.Fatalf("pinned snap must not observe later publication")
	}
	fresh := reg.Current()
	if fresh.LlamaRuntime(p.Name) != capability.RuntimeStrata {
		t.Fatalf("fresh snap must observe publication: %v", fresh.LlamaRuntime(p.Name))
	}
}

func TestRevGemStaleUnknownMetadataScope(t *testing.T) {
	reg := NewRegistry(revGemLlamaCfg())
	// Unknown cached runtime: reachable state, Unobserved engine, no mapper facts.
	revGemCommit(t, reg, map[string]LlamaServerSnapshot{
		"llama-a": {State: LlamaReachable, Runtime: capability.RuntimeUnobserved,
			Entries: []LlamaEntry{{Name: "llama-chat", Upstream: "llama-chat"}}},
	}, 1000)
	snap := reg.Current()
	if !snap.HasLlamaProvider("llama-a") {
		t.Fatalf("reachable unknown must stay a llama provider")
	}
	if snap.LlamaRuntime("llama-a") != capability.RuntimeUnobserved {
		t.Fatalf("unknown must stay Unobserved")
	}
	m, p, fault := snap.Resolve("llama-chat")
	if fault != Found {
		t.Fatalf("resolve: %v", fault)
	}
	// Unknown caps stay neutral (no invented budget/effort facts).
	caps := capability.ReasoningFor(snap.RequestCapability(m, p))
	if caps.BudgetSupported != nil || caps.BudgetMin != nil || caps.BudgetMax != nil {
		t.Fatalf("unknown must invent no budget facts: %+v", caps)
	}
	// Present Gemini budget drops in the shared planner; absent bypasses.
	budget, _ := protocol.NewGeminiBudget(100)
	scope := protocol.LocalRoute(snap.LlamaRuntime(p.Name))
	if planned, losses := protocol.PlanEffort(budget, caps, scope); len(losses) != 1 || planned.IsPresent() {
		t.Fatalf("unobserved must drop present: %+v %v", planned, losses)
	}
	if planned, losses := protocol.PlanEffort(protocol.AbsentReasoning(), caps, scope); len(losses) != 0 || planned.IsPresent() {
		t.Fatalf("absent must bypass: %+v %v", planned, losses)
	}
	// Stale retains the live runtime: freshness alone adds no refusal.
	revGemCommit(t, reg, map[string]LlamaServerSnapshot{
		"llama-a": {State: LlamaUnreachableStale, Runtime: capability.RuntimeLlamaCpp,
			Entries: []LlamaEntry{{Name: "llama-chat", Upstream: "llama-chat"}}},
	}, 2000)
	stale := reg.Current()
	if stale.LlamaRuntime("llama-a") != capability.RuntimeLlamaCpp {
		t.Fatalf("stale must retain runtime: %v", stale.LlamaRuntime("llama-a"))
	}
	ms, ps, fault := stale.Resolve("llama-chat")
	if fault != Found {
		t.Fatalf("stale resolve: %v", fault)
	}
	cs := capability.ReasoningFor(stale.RequestCapability(ms, ps))
	if planned, losses := protocol.PlanEffort(budget, cs, protocol.LocalRoute(stale.LlamaRuntime(ps.Name))); len(losses) != 0 {
		t.Fatalf("stale LlamaCpp must stay exact: %v", losses)
	} else if b, ok := planned.Budget(); !ok || b != 100 {
		t.Fatalf("stale wire: %+v", planned)
	}
	// Responses scope on the same stale pin drops before the encoder.
	if planned, losses := protocol.PlanEffort(budget, cs, protocol.LocalResponsesRoute(stale.LlamaRuntime(ps.Name))); len(losses) != 1 || planned.IsPresent() {
		t.Fatalf("stale responses budget must drop: %+v %v", planned, losses)
	}
}

func TestRevGemMetadataRouterUntrusted(t *testing.T) {
	cfg := revGemStaticCfg()
	meta := []byte(`{"router":{"reasoning":{"budget":{"supported":true,"min_tokens":1}}}}`)
	for i := range cfg.Models {
		if cfg.Models[i].Name == "static-chat" {
			cfg.Models[i].Metadata = append([]byte(nil), meta...)
		}
	}
	reg := NewRegistry(cfg)
	snap := reg.Current()
	m, p, fault := snap.Resolve("static-chat")
	if fault != Found {
		t.Fatalf("resolve: %v", fault)
	}
	caps := capability.ReasoningFor(snap.RequestCapability(m, p))
	if caps.BudgetSupported != nil || caps.BudgetMin != nil {
		t.Fatalf("metadata.router must stay untrusted: %+v", caps)
	}
}
