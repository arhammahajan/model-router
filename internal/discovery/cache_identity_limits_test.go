package discovery

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/router"
)

// Focused identity + route-only encoding for the explicit limit policy.

func mustLoadCfg(t *testing.T, path string) *config.Config {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	return cfg
}

// Focused identity + route-only encoding for the explicit limit policy.

func limCacheCfg(t *testing.T, dir, extra string) string {
	t.Helper()
	path := dir + "/router.toml"
	body := `
[server]
[retry]
[providers.up]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
api_key = "secret-key-1"
[[models]]
name = "a"
provider = "up"
[llama_cpp]
prefix = "pre-"
servers = [{ url = "http://127.0.0.1:9931" }, { url = "http://127.0.0.1:9932" }]
` + extra + `
[discovery_cache]
path = "` + dir + `/cache.json"
` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLimitsIdentityRotationAndDefault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("MODEL_ROUTER_API_KEYS", "")
	t.Setenv("MODEL_ROUTER_API_KEY", "")
	base := limCacheCfg(t, dir, "")
	cfg := mustLoadCfg(t, base)
	store := NewCacheStore(cfg, router.NewRegistry(cfg))
	baseline := store.identity
	// Empty outer and inner tables omit the same facts as absent policy.
	for _, declaration := range []string{"[llama_cpp.limit_overrides]\n", "[llama_cpp.limit_overrides.m]\n"} {
		emptyPath := limCacheCfg(t, t.TempDir(), declaration)
		emptyCfg := mustLoadCfg(t, emptyPath)
		emptyCfg.DiscoveryCache = cfg.DiscoveryCache
		if got := cacheIdentity(emptyCfg); got != baseline {
			t.Fatalf("empty overrides must preserve the baseline identity: %s", declaration)
		}
	}
	// Explicit fallback rotates.
	dir2 := t.TempDir()
	p2 := limCacheCfg(t, dir2, "context_fallback = 64000\n")
	cfg2 := mustLoadCfg(t, p2)
	cfg2.DiscoveryCache = cfg.DiscoveryCache
	if got := cacheIdentity(cfg2); got == baseline {
		t.Fatal("explicit fallback must rotate the identity")
	}
	dir3 := t.TempDir()
	p3 := limCacheCfg(t, dir3, "[llama_cpp.limit_overrides.m]\ncontext_window = 10\n")
	cfg3 := mustLoadCfg(t, p3)
	cfg3.DiscoveryCache = cfg.DiscoveryCache
	if got := cacheIdentity(cfg3); got == baseline {
		t.Fatal("nonempty overrides must rotate the identity")
	}
}

func TestLimitsRouteOnlyEncoding(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("MODEL_ROUTER_API_KEYS", "")
	t.Setenv("MODEL_ROUTER_API_KEY", "")
	path := limCacheCfg(t, dir, "[llama_cpp.limit_overrides.m1]\ncontext_window = 99\n")
	cfg := mustLoadCfg(t, path)
	reg := router.NewRegistry(cfg)
	names := []string{}
	for _, s := range cfg.Llama.Servers {
		names = append(names, s.Name)
	}
	layer := router.LlamaLayer{Servers: map[string]router.LlamaServerSnapshot{
		names[0]: {State: router.LlamaReachable, Entries: []router.LlamaEntry{{Name: "pre-m1", Upstream: "m1"}}},
	}}
	if err := commitLlamaShim(reg, layer, float64(wallNow())); err != nil {
		t.Fatal(err)
	}
	store := NewCacheStore(cfg, reg)
	payload, ok := store.encode(reg.ExportDiscovery())
	if !ok {
		t.Fatal("encode must succeed")
	}
	var doc struct {
		Identity string                                `json:"identity"`
		Llama    map[string]map[string]json.RawMessage `json:"llama"`
	}
	if err := json.Unmarshal(payload, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Identity != store.identity {
		t.Fatal("encoded identity must match the store fingerprint")
	}
	unit := doc.Llama["0"]
	if len(doc.Llama) != 1 || len(unit) != 2 || unit["succeeded_at"] == nil {
		t.Fatalf("llama cache unit must contain only models and succeeded_at: %s", payload)
	}
	var models []map[string]json.RawMessage
	if err := json.Unmarshal(unit["models"], &models); err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || len(models[0]) != 2 || string(models[0]["id"]) != `"m1"` || string(models[0]["aliases"]) != "[]" {
		t.Fatalf("route-only file must contain only model IDs and aliases: %s", payload)
	}
	if err := os.WriteFile(cfg.DiscoveryCache.Path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	// Restore into a fresh registry: live publication guards forbid
	// restoring over the publishing registry.
	reg2 := router.NewRegistry(cfg)
	store2 := NewCacheStore(cfg, reg2)
	st := store2.Restore(context.Background(), nil)
	if st.Load != CacheLoaded {
		t.Fatalf("restore: %+v", st)
	}
}
