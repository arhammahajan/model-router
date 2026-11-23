// Hermetic cache tests: file shape, admission, restore atomicity,
// single-writer ordering, Close deadline. All tempdirs; HOME controlled;
// fake upstreams only. No sibling files touched.
package discovery

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/router"
)

func cacheTestConfig(t *testing.T, dir string) *config.Config {
	t.Helper()
	path := filepath.Join(dir, "router.toml")
	body := `
[server]
[retry]
[providers.up]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
api_key = "secret-key-1"
[providers.other]
dialect = "openai-chat"
base_url = "http://127.0.0.1:10"
[[models]]
name = "a"
provider = "up"
[catalog]
url = "http://127.0.0.1:9/models"
model_providers = { "m1" = "up" }
[llama_cpp]
prefix = "pre-"
servers = [{ url = "http://127.0.0.1:9931" }, { url = "http://127.0.0.1:9932" }]
[discovery_cache]
path = "` + filepath.Join(dir, "cache.json") + `"
`
	body = body + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.DiscoveryCache == nil {
		t.Fatal("cache must be configured")
	}
	return cfg
}

func writeFile(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func cacheDoc(ident string, cat any, llama map[string]any) map[string]any {
	return map[string]any{"schema_version": 2, "identity": ident, "catalog": cat, "llama": llama}
}

func TestLoadMissingIncompatibleShapes(t *testing.T) {
	dir := t.TempDir()
	cfg := cacheTestConfig(t, dir)
	_, st, err := LoadDiscoveryCache(cfg.DiscoveryCache.Path, 86400, float64(wallNow()))
	if err != nil || st.Load != CacheMissing {
		t.Fatalf("missing: %+v %v", st, err)
	}
	// v1 file loads as incompatible, never parsed.
	writeFile(t, cfg.DiscoveryCache.Path, map[string]any{
		"schema_version": 1, "identity": "x", "catalog": nil, "llama": map[string]any{},
	})
	_, st, _ = LoadDiscoveryCache(cfg.DiscoveryCache.Path, 86400, float64(wallNow()))
	if st.Load != CacheIncompatible {
		t.Fatalf("v1 must be incompatible: %+v", st)
	}
	// Trailing data rejects.
	os.WriteFile(cfg.DiscoveryCache.Path, []byte(`{"schema_version":2,"identity":"i","catalog":null,"llama":{}} trailing`), 0o600)
	_, st, err = LoadDiscoveryCache(cfg.DiscoveryCache.Path, 86400, float64(wallNow()))
	if err == nil || st.Load != CacheInvalid {
		t.Fatalf("trailing must be invalid: %+v %v", st, err)
	}
	// Unknown top key rejects as incompatible (closed top shape).
	writeFile(t, cfg.DiscoveryCache.Path, map[string]any{
		"schema_version": 2, "identity": "i", "catalog": nil, "llama": map[string]any{}, "extra": 1,
	})
	_, st, _ = LoadDiscoveryCache(cfg.DiscoveryCache.Path, 86400, float64(wallNow()))
	if st.Load != CacheIncompatible {
		t.Fatalf("unknown top key must be incompatible: %+v", st)
	}
	// Unknown unit/row/index shapes reject.
	now := float64(wallNow())
	writeFile(t, cfg.DiscoveryCache.Path, cacheDoc("i",
		map[string]any{"succeeded_at": now, "models": []any{}, "extra": 1}, map[string]any{}))
	if _, st, _ := LoadDiscoveryCache(cfg.DiscoveryCache.Path, 86400, now); st.Load != CacheInvalid {
		t.Fatalf("unknown unit key must be invalid: %+v", st)
	}
	writeFile(t, cfg.DiscoveryCache.Path, cacheDoc("i",
		map[string]any{"succeeded_at": now, "models": []any{map[string]any{"id": "m", "aliases": []any{}, "x": 1}}}, map[string]any{}))
	if _, st, _ := LoadDiscoveryCache(cfg.DiscoveryCache.Path, 86400, now); st.Load != CacheInvalid {
		t.Fatalf("unknown row key must be invalid: %+v", st)
	}
	writeFile(t, cfg.DiscoveryCache.Path, cacheDoc("i", nil, map[string]any{"x": map[string]any{"succeeded_at": now, "models": []any{}}}))
	if _, st, _ := LoadDiscoveryCache(cfg.DiscoveryCache.Path, 86400, now); st.Load != CacheInvalid {
		t.Fatalf("unknown index must be invalid: %+v", st)
	}
	// Future time invalid; expired omitted with count.
	writeFile(t, cfg.DiscoveryCache.Path, cacheDoc("i",
		map[string]any{"succeeded_at": now + 100, "models": []any{}}, map[string]any{}))
	if _, st, _ := LoadDiscoveryCache(cfg.DiscoveryCache.Path, 86400, now); st.Load != CacheInvalid {
		t.Fatalf("future time must be invalid: %+v", st)
	}
	writeFile(t, cfg.DiscoveryCache.Path, cacheDoc("i",
		map[string]any{"succeeded_at": now - 90000, "models": []any{map[string]any{"id": "m", "aliases": []any{}}}}, map[string]any{}))
	inv, st, _ := LoadDiscoveryCache(cfg.DiscoveryCache.Path, 100, now)
	if st.Load != CacheEmpty || st.ExpiredSources != 1 || inv.Catalog != nil {
		t.Fatalf("expired must omit: %+v %+v", st, inv)
	}
	// Catalog aliases and full URLs reject.
	writeFile(t, cfg.DiscoveryCache.Path, cacheDoc("i",
		map[string]any{"succeeded_at": now, "models": []any{map[string]any{"id": "m", "aliases": []any{"a"}}}}, map[string]any{}))
	if _, st, _ := LoadDiscoveryCache(cfg.DiscoveryCache.Path, 86400, now); st.Load != CacheInvalid {
		t.Fatalf("catalog alias must be invalid: %+v", st)
	}
	writeFile(t, cfg.DiscoveryCache.Path, cacheDoc("i",
		map[string]any{"succeeded_at": now, "models": []any{map[string]any{"id": "https://evil.example/x", "aliases": []any{}}}}, map[string]any{}))
	if _, st, _ := LoadDiscoveryCache(cfg.DiscoveryCache.Path, 86400, now); st.Load != CacheInvalid {
		t.Fatalf("URL id must be invalid: %+v", st)
	}
}

func TestFileBoundaryBits(t *testing.T) {
	dir := t.TempDir()
	cfg := cacheTestConfig(t, dir)
	now := float64(wallNow())
	writeFile(t, cfg.DiscoveryCache.Path, cacheDoc("i", nil, map[string]any{}))
	// Symlink rejects via O_NOFOLLOW.
	linkDir := t.TempDir()
	link := filepath.Join(linkDir, "link.json")
	if err := os.Symlink(cfg.DiscoveryCache.Path, link); err == nil {
		if _, st, _ := LoadDiscoveryCache(link, 86400, now); st.Load != CacheInvalid {
			t.Fatalf("symlink must be invalid: %+v", st)
		}
	}
	// Non-regular (directory) rejects.
	if _, st, _ := LoadDiscoveryCache(dir, 86400, now); st.Load != CacheInvalid {
		t.Fatalf("non-regular must be invalid: %+v", st)
	}
	// Group/other bits reject.
	if err := os.Chmod(cfg.DiscoveryCache.Path, 0o640); err == nil {
		if _, st, _ := LoadDiscoveryCache(cfg.DiscoveryCache.Path, 86400, now); st.Load != CacheInvalid {
			t.Fatalf("group bits must be invalid: %+v", st)
		}
		_ = os.Chmod(cfg.DiscoveryCache.Path, 0o600)
	}
	// Oversize rejects (file size and MAX+1 read).
	big := make([]byte, CacheMaxBytes+1)
	for i := range big {
		big[i] = 'x'
	}
	os.WriteFile(cfg.DiscoveryCache.Path, big, 0o600)
	if _, st, _ := LoadDiscoveryCache(cfg.DiscoveryCache.Path, 86400, now); st.Load != CacheInvalid {
		t.Fatalf("oversize must be invalid: %+v", st)
	}
}

func TestSecretEchoOmittedOnSaveRejectedOnLoad(t *testing.T) {
	dir := t.TempDir()
	cfg := cacheTestConfig(t, dir)
	reg := router.NewRegistry(cfg)
	// Live publish accepts the secret echo as a route; save must omit it.
	if err := commitCatalogShim(reg, []router.CatalogEntry{{Name: "secret-key-1", Provider: "up", Upstream: "secret-key-1"}}, nil, float64(wallNow())); err != nil {
		// Unknown? "secret-key-1" provider is up, fine.
		t.Fatal(err)
	}
	store := NewCacheStore(cfg, reg)
	if !store.sanitize.safeID("secret-key-1") {
		// sanitizer must catch the resolved api_key echo.
	} else {
		t.Fatal("secret echo must fail admission")
	}
	payload, ok := store.encode(reg.ExportDiscovery())
	if !ok {
		t.Fatal("encode must succeed")
	}
	var doc map[string]any
	if err := json.Unmarshal(payload, &doc); err != nil {
		t.Fatal(err)
	}
	cat := doc["catalog"].(map[string]any)
	if len(cat["models"].([]any)) != 0 {
		t.Fatalf("unsafe row must be omitted, never rewritten: %v", cat)
	}
	// Hand-crafted secret file rejects on restore.
	now := float64(wallNow())
	writeFile(t, cfg.DiscoveryCache.Path, cacheDoc(store.identity,
		map[string]any{"succeeded_at": now, "models": []any{map[string]any{"id": "secret-key-1", "aliases": []any{}}}}, map[string]any{}))
	st := store.Restore(context.Background(), nil)
	if st.Load != CacheInvalid {
		t.Fatalf("secret file must be invalid: %+v", st)
	}
}

func TestRoundTripPrefixLocalAliasHidden(t *testing.T) {
	dir := t.TempDir()
	cfg := cacheTestConfig(t, dir)
	reg := router.NewRegistry(cfg)
	names := []string{}
	for _, s := range cfg.Llama.Servers {
		names = append(names, s.Name)
	}
	if len(names) < 2 {
		t.Fatal("need two llama servers")
	}
	layer := router.LlamaLayer{Servers: map[string]router.LlamaServerSnapshot{
		names[0]: {State: router.LlamaReachable, Entries: []router.LlamaEntry{
			{Name: "pre-m1", Upstream: "m1", Aliases: []string{"pre-m1-alias"}},
			{Name: "pre-local-llama", Upstream: "local", Aliases: []string{"pre-local-alias"}},
		}},
		names[1]: {State: router.LlamaReachable, Entries: []router.LlamaEntry{
			{Name: "pre-hidden", Upstream: "m1", Aliases: []string{"pre-h2"}},
		}},
	}}
	if err := commitLlamaShim(reg, layer, float64(wallNow())); err != nil {
		t.Fatal(err)
	}
	store := NewCacheStore(cfg, reg)
	store.saveOnce()
	data, err := os.ReadFile(cfg.DiscoveryCache.Path)
	if err != nil {
		t.Fatalf("save must write: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["schema_version"].(float64) != 2 {
		t.Fatalf("must write v2: %v", doc)
	}
	// Restore into a fresh registry: names re-derived, upstream preserved.
	reg2 := router.NewRegistry(cfg)
	store2 := NewCacheStore(cfg, reg2)
	st := store2.Restore(context.Background(), nil)
	if st.Load != CacheLoaded {
		t.Fatalf("restore: %+v", st)
	}
	m, _, fault := reg2.Current().Resolve("pre-m1")
	if fault != router.Found || m.Upstream != "m1" {
		t.Fatalf("roundtrip broken: %+v %v", m, fault)
	}
	m, _, fault = reg2.Current().Resolve("pre-local-llama")
	if fault != router.Found || m.Upstream != "local" {
		t.Fatalf("local mapping broken: %+v %v", m, fault)
	}
	m, _, fault = reg2.Current().Resolve("pre-m1-alias")
	if fault != router.Found || m.Upstream != "m1" {
		t.Fatalf("alias roundtrip broken: %+v %v", m, fault)
	}
}

func TestRestoreAtomicBadSecondLayer(t *testing.T) {
	dir := t.TempDir()
	cfg := cacheTestConfig(t, dir)
	reg := router.NewRegistry(cfg)
	if err := commitCatalogShim(reg, []router.CatalogEntry{{Name: "m1", Provider: "up", Upstream: "m1"}}, nil, 100); err != nil {
		t.Fatal(err)
	}
	before := captureInv(reg)
	store := NewCacheStore(cfg, reg)
	now := float64(wallNow())
	writeFile(t, cfg.DiscoveryCache.Path, cacheDoc(store.identity,
		map[string]any{"succeeded_at": now, "models": []any{map[string]any{"id": "m1", "aliases": []any{}}}},
		map[string]any{"99": map[string]any{"succeeded_at": now, "models": []any{}}}))
	st := store.Restore(context.Background(), nil)
	if st.Load != CacheInvalid {
		t.Fatalf("bad second layer must be invalid: %+v", st)
	}
	before.assertUnchanged(t, reg)
	if _, _, fault := reg.Current().Resolve("m1"); fault != router.Found {
		t.Fatal("cached layer changed")
	}
}

func TestCanceledStartupNeverPublishes(t *testing.T) {
	dir := t.TempDir()
	cfg := cacheTestConfig(t, dir)
	reg := router.NewRegistry(cfg)
	store := NewCacheStore(cfg, reg)
	now := float64(wallNow())
	writeFile(t, cfg.DiscoveryCache.Path, cacheDoc(store.identity,
		map[string]any{"succeeded_at": now, "models": []any{map[string]any{"id": "m1", "aliases": []any{}}}},
		map[string]any{}))
	before := captureInv(reg)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	st := store.Restore(ctx, nil)
	if st.Load != CacheSkipped {
		t.Fatalf("canceled must skip: %+v", st)
	}
	before.assertUnchanged(t, reg)
	if store.Status().Load != CacheSkipped {
		t.Fatalf("canceled restore must keep skipped status: %+v", store.Status())
	}
}

func TestSingleWriterOrderingAndClose(t *testing.T) {
	dir := t.TempDir()
	cfg := cacheTestConfig(t, dir)
	reg := router.NewRegistry(cfg)
	store := NewCacheStore(cfg, reg)
	store.Start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		store.Close(ctx)
	}()
	// Older capture must never overtake newer: gate the first replace.
	release := make(chan struct{})
	var once sync.Once
	store.beforeReplace = func() {
		once.Do(func() { <-release })
	}
	if err := commitCatalogShim(reg, []router.CatalogEntry{{Name: "m1", Provider: "up", Upstream: "m1"}}, nil, 100); err != nil {
		t.Fatal(err)
	}
	store.NotifyDirty()
	if err := commitCatalogShim(reg, []router.CatalogEntry{{Name: "m1", Provider: "up", Upstream: "m1"}}, nil, 200); err != nil {
		t.Fatal(err)
	}
	store.NotifyDirty()
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, err := os.ReadFile(cfg.DiscoveryCache.Path)
		if err == nil {
			var doc map[string]any
			if json.Unmarshal(data, &doc) == nil {
				if cat, ok := doc["catalog"].(map[string]any); ok && cat != nil {
					if at, ok := cat["succeeded_at"].(float64); ok && at == 200 {
						break
					}
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("newer save never landed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// After Close, no new notifications and bounded return.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	store.Close(ctx)
	cancel()
	store.NotifyDirty()
	select {
	case <-store.dirty:
		t.Fatal("post-close must reject new notifications")
	default:
	}
}

func TestStalledWriteCloseBounded(t *testing.T) {
	dir := t.TempDir()
	cfg := cacheTestConfig(t, dir)
	reg := router.NewRegistry(cfg)
	store := NewCacheStore(cfg, reg)
	store.Start()
	block := make(chan struct{})
	store.createTemp = func(dir, pattern string) (*os.File, error) {
		<-block
		return createTemp0600(dir, pattern)
	}
	if err := commitCatalogShim(reg, []router.CatalogEntry{{Name: "m1", Provider: "up", Upstream: "m1"}}, nil, 100); err != nil {
		t.Fatal(err)
	}
	store.NotifyDirty()
	time.Sleep(50 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	store.Close(ctx)
	if time.Since(start) > 2*time.Second {
		t.Fatal("Close must not join stalled syscall beyond the drain budget")
	}
	close(block)
	// Let the unblocked writer finish its atomic replace before TempDir
	// cleanup: the file stays atomic old-or-new and no new flush starts.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(cfg.DiscoveryCache.Path); err == nil {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
}

func TestSeedThenFailureRetains(t *testing.T) {
	dir := t.TempDir()
	cfg := cacheTestConfig(t, dir)
	reg := router.NewRegistry(cfg)
	store := NewCacheStore(cfg, reg)
	now := float64(wallNow())
	writeFile(t, cfg.DiscoveryCache.Path, cacheDoc(store.identity, nil,
		map[string]any{"0": map[string]any{"succeeded_at": now, "models": []any{
			map[string]any{"id": "m1", "aliases": []any{}},
		}}}))
	lw := NewLlamaWorker(cfg.Llama, reg)
	st := store.Restore(context.Background(), lw)
	if st.Load != CacheLoaded {
		t.Fatalf("restore: %+v", st)
	}
	if _, _, fault := reg.Current().Resolve("pre-m1"); fault != router.Found {
		t.Fatalf("seeded route must resolve: %v", fault)
	}
	before := reg.ExportDiscovery().Llama
	// Failing probe retains the seeded layer with its original time.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	cfg.Llama.Servers[0].URL = srv.URL
	lw2 := NewLlamaWorker(cfg.Llama, reg)
	// Copy retained/active from the seeded worker to simulate same process.
	lw2.active = lw.active
	lw2.retained = lw.retained
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lw2.RefreshOnce(ctx)
	after := reg.ExportDiscovery().Llama
	for name, unit := range before {
		got, ok := after[name]
		if !ok || got.SucceededAt != unit.SucceededAt {
			t.Fatalf("failure must retain original time for %s", name)
		}
	}
}

func TestExportCopiesIsolate(t *testing.T) {
	dir := t.TempDir()
	cfg := cacheTestConfig(t, dir)
	reg := router.NewRegistry(cfg)
	if err := commitCatalogShim(reg, []router.CatalogEntry{{Name: "m1", Provider: "up", Upstream: "m1"}}, nil, 42); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			inv := reg.ExportDiscovery()
			if inv.Catalog != nil && len(inv.Catalog.Entries) > 0 {
				inv.Catalog.Entries[0].Name = "mutated"
			}
			reg.Current().Resolve("m1")
		}()
	}
	wg.Wait()
	inv := reg.ExportDiscovery()
	if inv.Catalog == nil || inv.Catalog.Entries[0].Name != "m1" || inv.Catalog.SucceededAt != 42 {
		t.Fatalf("export must be coherent copies: %+v", inv.Catalog)
	}
}

func TestRotationInvalidates(t *testing.T) {
	dir := t.TempDir()
	cfg := cacheTestConfig(t, dir)
	reg := router.NewRegistry(cfg)
	store := NewCacheStore(cfg, reg)
	now := float64(wallNow())
	writeFile(t, cfg.DiscoveryCache.Path, cacheDoc(store.identity,
		map[string]any{"succeeded_at": now, "models": []any{map[string]any{"id": "m1", "aliases": []any{}}}}, map[string]any{}))
	// Flip one credential: rotation must invalidate.
	cfg.Providers["up"].APIKey = "rotated-key"
	store2 := NewCacheStore(cfg, reg)
	st := store2.Restore(context.Background(), nil)
	if st.Load != CacheConfigurationChanged {
		t.Fatalf("rotation must invalidate: %+v", st)
	}
}
