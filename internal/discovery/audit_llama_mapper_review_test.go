// Independent review: engine identity from SERVER props only, cache
// route-only shape, and stale/empty publication boundaries. Hermetic fakes.
package discovery

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/arhammahajan/router/internal/capability"
	"github.com/arhammahajan/router/internal/router"
)

func TestAuditDiscoveryRouterModeEngineFromGlobalBuild(t *testing.T) {
	// Router mode ignores global props for observations but still uses the
	// global build_info as the sole server engine identity.
	f := &llamaProbe{propsOn: true, props: `{"role": "router", "build_info": "strata fleet", "modalities": {"vision": true}}`}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	cfg := llamaWorkerCfg(srv.URL, "s1")
	_, reg := refreshLlama(t, cfg)
	if got := reg.Current().LlamaRuntime("s1"); got != capability.RuntimeStrata {
		t.Fatalf("router global build must identify Strata: %v", got)
	}
}

func TestAuditDiscoveryModelFlagsNeverEngine(t *testing.T) {
	// Row-level engine-like metadata (metadata.engine, id substrings,
	// max_model_len legacy marker) never selects the runtime.
	f := &llamaProbe{propsOn: true,
		props:  `{"build_info": "llama.cpp b1"}`,
		models: `{"data": [{"id": "polystrata-qwen", "max_model_len": 4096, "metadata": {"engine": "strata"}}]}`}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	cfg := llamaWorkerCfg(srv.URL, "s1")
	_, reg := refreshLlama(t, cfg)
	if got := reg.Current().LlamaRuntime("s1"); got != capability.RuntimeLlamaCpp {
		t.Fatalf("model flags must not select engine: %v", got)
	}
}

func TestAuditDiscoveryNullObjectBuildDefaults(t *testing.T) {
	// Null and object build_info are malformed evidence: live success still
	// defaults to LlamaCpp, never Strata, never Unobserved.
	for _, props := range []string{
		`{"build_info": null}`,
		`{"build_info": {"v": "strata"}}`,
		`{"build_info": ["strata"]}`,
	} {
		f := &llamaProbe{propsOn: true, props: props}
		srv := httptest.NewServer(f.handler())
		cfg := llamaWorkerCfg(srv.URL, "s1")
		_, reg := refreshLlama(t, cfg)
		if got := reg.Current().LlamaRuntime("s1"); got != capability.RuntimeLlamaCpp {
			t.Fatalf("props %s must default LlamaCpp: %v", props, got)
		}
		srv.Close()
	}
}

func TestAuditDiscoveryCacheOmitsRuntime(t *testing.T) {
	// The route-only v2 file carries IDs and aliases only: no engine,
	// runtime, build_info, observations or capabilities survive a save.
	cfg := cacheTestConfig(t, t.TempDir())
	reg := router.NewRegistry(cfg)
	s := NewCacheStore(cfg, reg)
	inv := router.DiscoveryInventory{Llama: map[string]router.LlamaInventory{
		cfg.Llama.Servers[0].Name: {SucceededAt: router.Wall(wallNow()), Entries: []router.LlamaEntry{
			{Name: "pre-m1", Upstream: "m1"},
		}},
	}}
	raw, ok := s.encode(inv)
	if !ok {
		t.Fatalf("encode must succeed")
	}
	lower := strings.ToLower(string(raw))
	for _, needle := range []string{"runtime", "engine", "build_info", "strata", "thinking", "reasoning"} {
		if strings.Contains(lower, needle) {
			t.Fatalf("cache must omit %q: %s", needle, raw)
		}
	}
}

func TestAuditDiscoveryStaleRetainsEngine(t *testing.T) {
	f := &llamaProbe{propsOn: true, props: `{"build_info": "strata fleet"}`}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	cfg := llamaWorkerCfg(srv.URL, "s1")
	w, reg := refreshLlama(t, cfg)
	if got := reg.Current().LlamaRuntime("s1"); got != capability.RuntimeStrata {
		t.Fatalf("setup: %v", got)
	}
	f.mu.Lock()
	f.health = 500
	f.mu.Unlock()
	w.RefreshOnce(context.Background())
	if got := reg.Current().LlamaRuntime("s1"); got != capability.RuntimeStrata {
		t.Fatalf("stale must retain Strata: %v", got)
	}
	if !reg.Current().HasLlamaProvider("s1") {
		t.Fatalf("stale keeps membership")
	}
}
