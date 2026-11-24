package discovery

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/arhammahajan/router/internal/capability"
	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/provider"
	"github.com/arhammahajan/router/internal/router"
)

func TestCoexistingLlamaCppAndStrataDiscoveryAndCacheRoundTrip(t *testing.T) {
	var mu sync.Mutex
	var strataAuthHeader string
	var llamaAuthHeader string

	llamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		llamaAuthHeader = r.Header.Get("Authorization")
		mu.Unlock()
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"qwen-local","aliases":["qwen-fast"]}]}`))
		case "/props":
			_, _ = w.Write([]byte(`{"build_info":"b4000-llama"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer llamaSrv.Close()

	// Strata server omits build_info in /props; explicit [providers.strata]
	// must still pin RuntimeStrata and KindStrata.
	strataSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		strataAuthHeader = r.Header.Get("x-api-key")
		mu.Unlock()
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"strata-model","aliases":["strata-alias"]}]}`))
		case "/props":
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer strataSrv.Close()

	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("MODEL_ROUTER_API_KEYS", "")
	t.Setenv("MODEL_ROUTER_API_KEY", "")
	cachePath := filepath.Join(dir, "cache.json")
	cfgPath := filepath.Join(dir, "router.toml")
	body := `
[server]
[retry]

[providers.llama_cpp]
prefix = "llama/"
aliases = true
probe_timeout = 1.5
api_key = "llama-probe-secret"
context_fallback = 32768
servers = [{ url = "` + llamaSrv.URL + `", name = "llama-node" }]

[providers.strata]
prefix = "strata/"
aliases = false
probe_timeout = 3.0
api_key = "strata-probe-secret"
auth_header = "x-api-key"
context_fallback = 16384
servers = [{ url = "` + strataSrv.URL + `", name = "strata-node" }]

[discovery_cache]
path = "` + cachePath + `"
`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	// LlamaFleetBound should reflect the max per-server probe_timeout (3.0s) + 1s.
	if bound := LlamaFleetBound(cfg.Llama); bound != 4*time.Second {
		t.Fatalf("LlamaFleetBound = %v, want 4s", bound)
	}

	reg := router.NewRegistry(cfg)
	worker := NewLlamaWorker(cfg.Llama, reg)
	if ok := worker.RefreshOnce(context.Background()); !ok {
		t.Fatal("RefreshOnce failed")
	}

	mu.Lock()
	gotLlamaAuth, gotStrataAuth := llamaAuthHeader, strataAuthHeader
	mu.Unlock()
	if gotLlamaAuth != "Bearer llama-probe-secret" {
		t.Fatalf("llama probe Authorization = %q, want Bearer llama-probe-secret", gotLlamaAuth)
	}
	if gotStrataAuth != "strata-probe-secret" {
		t.Fatalf("strata probe x-api-key = %q, want strata-probe-secret", gotStrataAuth)
	}

	snap := reg.Current()
	// llama_cpp route + alias resolve with KindLlamaCpp.
	if m, p, f := snap.Resolve("llama/qwen-local"); f != router.Found || m.Upstream != "qwen-local" || p.Kind != provider.KindLlamaCpp {
		t.Fatalf("llama canonical resolve: m=%+v p=%+v f=%v", m, p, f)
	}
	if m, _, f := snap.Resolve("llama/qwen-fast"); f != router.Found || m.Upstream != "qwen-local" {
		t.Fatalf("llama alias resolve: m=%+v f=%v", m, f)
	}
	// strata route resolves with KindStrata and RuntimeStrata; aliases disabled.
	if m, p, f := snap.Resolve("strata/strata-model"); f != router.Found || m.Upstream != "strata-model" || p.Kind != provider.KindStrata {
		t.Fatalf("strata canonical resolve: m=%+v p=%+v f=%v", m, p, f)
	}
	if _, _, f := snap.Resolve("strata/strata-alias"); f != router.NotFound {
		t.Fatalf("strata alias must not be exposed when aliases=false, got fault=%v", f)
	}
	if got := snap.LlamaRuntime("strata-node"); got != capability.RuntimeStrata {
		t.Fatalf("strata runtime = %v, want strata", got)
	}

	// Save discovery cache and restore into a fresh registry.
	store := NewCacheStore(cfg, reg)
	store.saveOnce()
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("cache file not written: %v", err)
	}

	reg2 := router.NewRegistry(cfg)
	worker2 := NewLlamaWorker(cfg.Llama, reg2)
	store2 := NewCacheStore(cfg, reg2)
	st := store2.Restore(context.Background(), worker2)
	if st.Load != CacheLoaded {
		t.Fatalf("cache restore status = %+v, want loaded", st)
	}

	snap2 := reg2.Current()
	// Restored llama_cpp server binds KindUnobserved until live probe.
	if _, p, f := snap2.Resolve("llama/qwen-local"); f != router.Found || p.Kind != provider.KindUnobserved {
		t.Fatalf("restored llama resolve: p=%+v f=%v", p, f)
	}
	if _, _, f := snap2.Resolve("llama/qwen-fast"); f != router.Found {
		t.Fatalf("restored llama alias resolve: f=%v", f)
	}
	// Restored strata server binds KindStrata + RuntimeStrata immediately.
	if _, p, f := snap2.Resolve("strata/strata-model"); f != router.Found || p.Kind != provider.KindStrata {
		t.Fatalf("restored strata resolve: p=%+v f=%v", p, f)
	}
	if got := snap2.LlamaRuntime("strata-node"); got != capability.RuntimeStrata {
		t.Fatalf("restored strata runtime = %v, want strata", got)
	}

	// Identity rotates when per-server settings change.
	baseID := cacheIdentity(cfg)
	mutKind := *cfg
	mutLlama := *cfg.Llama
	mutLlama.Servers = append([]config.LlamaServer(nil), cfg.Llama.Servers...)
	mutLlama.Servers[0].ProbeTimeout = 9.0
	mutKind.Llama = &mutLlama
	if cacheIdentity(&mutKind) == baseID {
		t.Fatal("per-server probe_timeout change must invalidate cache identity")
	}

	// cacheSecrets includes CLIProxyAPI headers and per-server BaseURL secrets.
	cfg.CLIProxyAPI = &config.CLIProxyAPIConfig{
		BaseURL: "http://127.0.0.1:8317/v1",
		Headers: map[string]string{"X-CLI-Secret": "cli-header-tok"},
	}
	cfg.Llama.Servers[0].BaseURL = "http://u:basepass@127.0.0.1:9931/v1"
	san := newCacheSanitizer(cfg)
	if san.safeID("cli-header-tok") {
		t.Fatal("CLIProxyAPI header secret must be rejected by cache sanitizer")
	}
	if san.safeID("basepass") {
		t.Fatal("Llama server BaseURL secret must be rejected by cache sanitizer")
	}
}
