package router

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/arhammahajan/router/internal/capability"
	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/provider"
)

func loadRouterTOML(t *testing.T, body string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("MODEL_ROUTER_API_KEYS", "")
	t.Setenv("MODEL_ROUTER_API_KEY", "")
	p := filepath.Join(dir, "router.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

func TestLocalProviderKindBindingAndSupportedDialects(t *testing.T) {
	cfg := loadRouterTOML(t, `
[server]
[retry]
[providers.llama_cpp]
servers = [{ url = "http://127.0.0.1:9931", name = "llama-1" }]
prefix = "llama/"
context_fallback = 32768

[providers.strata]
servers = [{ url = "http://127.0.0.1:9932", name = "strata-1" }]
prefix = "strata/"
`)
	reg := NewRegistry(cfg)

	// RestoreCached: llama_cpp stays KindUnobserved (openai-chat only), while
	// explicitly configured [providers.strata] binds KindStrata (openai-chat +
	// anthropic) and RuntimeStrata immediately before any live probe.
	err := reg.RestoreCached(DiscoveryInventory{
		Llama: map[string]LlamaInventory{
			"llama-1":  {SucceededAt: 100, Entries: []LlamaEntry{{Name: "llama/m1", Upstream: "m1"}}},
			"strata-1": {SucceededAt: 100, Entries: []LlamaEntry{{Name: "strata/s1", Upstream: "s1"}}},
		},
	})
	if err != nil {
		t.Fatalf("RestoreCached: %v", err)
	}
	snap := reg.Current()

	_, lp, lf := snap.Resolve("llama/m1")
	if lf != Found {
		t.Fatalf("resolve llama/m1: %v", lf)
	}
	if lp.Kind != provider.KindUnobserved {
		t.Fatalf("restored llama_cpp kind = %q, want %q", lp.Kind, provider.KindUnobserved)
	}
	if want := []string{config.DialectOpenAIChat}; !reflect.DeepEqual(lp.SupportedDialects, want) {
		t.Fatalf("restored llama_cpp supported_dialects = %v, want %v", lp.SupportedDialects, want)
	}
	if got := snap.LlamaRuntime("llama-1"); got != capability.RuntimeUnobserved {
		t.Fatalf("restored llama_cpp runtime = %v, want unobserved", got)
	}

	_, sp, sf := snap.Resolve("strata/s1")
	if sf != Found {
		t.Fatalf("resolve strata/s1: %v", sf)
	}
	if sp.Kind != provider.KindStrata {
		t.Fatalf("restored strata kind = %q, want %q", sp.Kind, provider.KindStrata)
	}
	if want := []string{config.DialectOpenAIChat, config.DialectAnthropic}; !reflect.DeepEqual(sp.SupportedDialects, want) {
		t.Fatalf("restored strata supported_dialects = %v, want %v", sp.SupportedDialects, want)
	}
	if got := snap.LlamaRuntime("strata-1"); got != capability.RuntimeStrata {
		t.Fatalf("restored strata runtime = %v, want strata", got)
	}

	// Per-server limit policy isolation: llama-1 has context_fallback=32768;
	// strata-1 has no limit policy and must not inherit llama-1's fallback.
	if got := limVal(t, snap.CanonicalBase("llama/m1", "openai-chat"), "context_window"); got["value"] != float64(32768) || got["kind"] != "fallback" {
		t.Fatalf("llama-1 fallback = %v, want 32768", got)
	}
	if got := limVal(t, snap.CanonicalBase("strata/s1", "openai-chat"), "context_window"); got["value"] != nil {
		t.Fatalf("strata-1 must not inherit llama_cpp fallback, got %v", got)
	}

	// Live probe resolves llama-1 to KindLlamaCpp with openai-chat + anthropic.
	obs := capability.ExtractLlamaObs(map[string]any{}, nil, "single")
	if err := reg.CommitLlama(LlamaAttempt{
		AtWall: 200,
		Layer: LlamaLayer{Servers: map[string]LlamaServerSnapshot{
			"llama-1":  {State: LlamaReachable, Runtime: capability.RuntimeLlamaCpp, Entries: []LlamaEntry{{Name: "llama/m1", Upstream: "m1", Obs: obs}}},
			"strata-1": {State: LlamaReachable, Runtime: capability.RuntimeStrata, Entries: []LlamaEntry{{Name: "strata/s1", Upstream: "s1", Obs: obs}}},
		}},
	}); err != nil {
		t.Fatalf("CommitLlama: %v", err)
	}
	snap = reg.Current()
	_, lp, _ = snap.Resolve("llama/m1")
	if lp.Kind != provider.KindLlamaCpp {
		t.Fatalf("live llama_cpp kind = %q, want %q", lp.Kind, provider.KindLlamaCpp)
	}
	if want := []string{config.DialectOpenAIChat, config.DialectAnthropic}; !reflect.DeepEqual(lp.SupportedDialects, want) {
		t.Fatalf("live llama_cpp supported_dialects = %v, want %v", lp.SupportedDialects, want)
	}
	// Mutating returned SupportedDialects must not affect snapshot.
	lp.SupportedDialects[0] = "mutated"
	_, lp2, _ := snap.Resolve("llama/m1")
	if lp2.SupportedDialects[0] != config.DialectOpenAIChat {
		t.Fatalf("snapshot SupportedDialects mutated: %v", lp2.SupportedDialects)
	}

	// Live /props probe on llama_cpp discovering Strata promotes Kind to KindStrata.
	if err := reg.CommitLlama(LlamaAttempt{
		AtWall: 210,
		Layer: LlamaLayer{Servers: map[string]LlamaServerSnapshot{
			"llama-1":  {State: LlamaReachable, Runtime: capability.RuntimeStrata, Entries: []LlamaEntry{{Name: "llama/m1", Upstream: "m1", Obs: obs}}},
			"strata-1": {State: LlamaReachable, Runtime: capability.RuntimeStrata, Entries: []LlamaEntry{{Name: "strata/s1", Upstream: "s1", Obs: obs}}},
		}},
	}); err != nil {
		t.Fatalf("CommitLlama strata promotion: %v", err)
	}
	snap = reg.Current()
	_, lp, _ = snap.Resolve("llama/m1")
	if lp.Kind != provider.KindStrata {
		t.Fatalf("promoted llama_cpp kind = %q, want %q", lp.Kind, provider.KindStrata)
	}
}
