package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// Route-only discovery cache contract (M4 slice, go-discovery-persistence-contract).
//
// CLI-only: exercises run() with hermetic TempDir HOME/XDG, explicit
// LLAMA_SERVER_URL, fake httptest upstreams and controlled cache paths.
// No new production APIs, no sibling reads, no live config.
//
// Baseline without [discovery_cache] support must show honest RED here
// (config rejects the table); tests never conditional-skip.

const (
	dccProbeTimeout = 1
	dccMaxAge       = 3600.0
)

func dccEnv(t *testing.T) {
	t.Helper()
	t.Setenv("MODEL_ROUTER_API_KEY", "")
	t.Setenv("MODEL_ROUTER_API_KEYS", "")
	t.Setenv("LLAMA_SERVER_URL", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

// dccWriteCfg writes a hermetic router.toml. catalogURL empty means no
// [catalog]; llamaURLs empty means no [llama_cpp]. extraModels appends raw
// TOML (explicit [[models]] metadata controls). cachePath empty omits
// [discovery_cache].
func dccWriteCfg(t *testing.T, catalogURL string, llamaURLs []string, cachePath string, extraModels string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("[server]\nauth_mode = \"none\"\nallow_dynamic_routes = false\n[retry]\nmax_attempts = 1\n")
	b.WriteString("[providers.up]\ndialect = \"anthropic\"\nbase_url = \"http://127.0.0.1:9\"\n")
	if catalogURL != "" {
		fmt.Fprintf(&b, "[catalog]\nurl = %q\ntimeout = %d\nrefresh_seconds = 0\n", catalogURL, dccProbeTimeout)
		b.WriteString("[catalog.model_providers]\n\"cat-a\" = \"up\"\n")
	}
	if len(llamaURLs) > 0 {
		b.WriteString("[llama_cpp]\nservers = [")
		for i, u := range llamaURLs {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%q", u)
		}
		fmt.Fprintf(&b, "]\ntimeout = %d\nrefresh_seconds = 0\n", dccProbeTimeout)
	}
	if cachePath != "" {
		fmt.Fprintf(&b, "[discovery_cache]\npath = %q\nmax_age_seconds = %v\n", cachePath, dccMaxAge)
	}
	b.WriteString(extraModels)
	p := filepath.Join(t.TempDir(), "router.toml")
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func dccCatalogSrv(t *testing.T, hits *atomic.Int64, payload *atomic.Value, healthy *atomic.Bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if healthy != nil && !healthy.Load() {
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		s, _ := payload.Load().(string)
		_, _ = w.Write([]byte(s))
	}))
}

func dccLlamaSrv(t *testing.T, hits *atomic.Int64, payload *atomic.Value, healthy *atomic.Bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health", "/v1/models", "/props":
			hits.Add(1)
		}
		bad := healthy != nil && !healthy.Load()
		switch r.URL.Path {
		case "/health":
			if bad {
				w.WriteHeader(500)
				return
			}
			w.WriteHeader(200)
		case "/v1/models":
			if bad {
				w.WriteHeader(500)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			s, _ := payload.Load().(string)
			_, _ = w.Write([]byte(s))
		default:
			w.WriteHeader(500)
		}
	}))
}

func dccReadCache(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("cache JSON: %v", err)
	}
	return m
}

func dccWriteCache(t *testing.T, path string, m map[string]any) {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
}

func dccCacheAt(t *testing.T, path string) float64 {
	t.Helper()
	m := dccReadCache(t, path)
	cat, _ := m["catalog"].(map[string]any)
	if cat == nil {
		t.Fatalf("cache missing catalog unit: %v", m)
	}
	at, _ := cat["succeeded_at"].(float64)
	return at
}

func dccAssertMode0600(t *testing.T, path string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat cache: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("cache mode=%o want 600", fi.Mode().Perm())
	}
}

func dccAssertNoWorldReadable(t *testing.T, dir string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		fi, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Fatalf("cache dir entry %s mode=%o", e.Name(), fi.Mode().Perm())
		}
	}
}

func TestDiscoveryCacheContractWritesRouteOnlyV2(t *testing.T) {
	dccEnv(t)
	var catHits, llamaHits atomic.Int64
	var catPayload, llamaPayload atomic.Value
	catPayload.Store(`[{"id": "cat-a"}]`)
	llamaPayload.Store(`{"data": [{"id": "m1"}]}`)
	var catHealthy, llamaHealthy atomic.Bool
	catHealthy.Store(true)
	llamaHealthy.Store(true)
	catSrv := dccCatalogSrv(t, &catHits, &catPayload, &catHealthy)
	defer catSrv.Close()
	llamaSrv := dccLlamaSrv(t, &llamaHits, &llamaPayload, &llamaHealthy)
	defer llamaSrv.Close()
	cachePath := filepath.Join(t.TempDir(), "sub", "dc.json")
	cfg := dccWriteCfg(t, catSrv.URL, []string{llamaSrv.URL}, cachePath, "")
	if code := run([]string{"--config", cfg, "--check-readiness"}); code != 0 {
		t.Fatalf("seed readiness must exit 0, got %d", code)
	}
	raw, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("cache must exist after success: %v", err)
	}
	m := dccReadCache(t, cachePath)
	if v, _ := m["schema_version"].(float64); v != 2 {
		t.Fatalf("schema_version=%v want 2: %s", v, raw)
	}
	id, _ := m["identity"].(string)
	if len(id) == 0 {
		t.Fatalf("identity must be present: %s", raw)
	}
	cat, _ := m["catalog"].(map[string]any)
	if cat == nil {
		t.Fatalf("catalog unit missing: %s", raw)
	}
	models, _ := cat["models"].([]any)
	if len(models) != 1 {
		t.Fatalf("catalog models=%v want [cat-a]: %s", models, raw)
	}
	row, _ := models[0].(map[string]any)
	if row["id"] != "cat-a" {
		t.Fatalf("catalog row=%v: %s", row, raw)
	}
	for _, k := range []string{"metadata", "capabilities", "endpoint", "base_url", "api_key", "headers", "url"} {
		if _, ok := row[k]; ok {
			t.Fatalf("row must not carry %s: %s", k, raw)
		}
	}
	s := string(raw)
	for _, leak := range []string{catSrv.URL, llamaSrv.URL, "router-no-auth"} {
		if leak != "" && strings.Contains(s, leak) {
			t.Fatalf("cache must not persist URL/key material %q: %s", leak, s)
		}
	}
	llama, _ := m["llama"].(map[string]any)
	if len(llama) == 0 {
		t.Fatalf("llama section missing: %s", raw)
	}
	dccAssertMode0600(t, cachePath)
	dccAssertNoWorldReadable(t, filepath.Dir(cachePath))
}

func TestDiscoveryCacheContractStaleRetention(t *testing.T) {
	dccEnv(t)
	// Catalog-only: outage must stay ready via cache with identical time.
	var hits atomic.Int64
	var payload atomic.Value
	payload.Store(`[{"id": "cat-a"}]`)
	var healthy atomic.Bool
	healthy.Store(true)
	srv := dccCatalogSrv(t, &hits, &payload, &healthy)
	defer srv.Close()
	cachePath := filepath.Join(t.TempDir(), "c.json")
	cfg := dccWriteCfg(t, srv.URL, nil, cachePath, "")
	if code := run([]string{"--config", cfg, "--check-readiness"}); code != 0 {
		t.Fatalf("seed must exit 0, got %d", code)
	}
	before := dccCacheAt(t, cachePath)
	healthy.Store(false)
	if code := run([]string{"--config", cfg, "--check-readiness"}); code != 0 {
		t.Fatalf("catalog cache must keep ready=0 during outage, got %d", code)
	}
	if got := dccCacheAt(t, cachePath); got != before {
		t.Fatalf("failed probe must not erase/advance unit time: %v -> %v", before, got)
	}
	dccAssertMode0600(t, cachePath)

	// Llama-only: stale stays unready, same time preserved.
	var lhits atomic.Int64
	var lpayload atomic.Value
	lpayload.Store(`{"data": [{"id": "m1"}]}`)
	var lhealthy atomic.Bool
	lhealthy.Store(true)
	lsrv := dccLlamaSrv(t, &lhits, &lpayload, &lhealthy)
	defer lsrv.Close()
	lcache := filepath.Join(t.TempDir(), "l.json")
	lcfg := dccWriteCfg(t, "", []string{lsrv.URL}, lcache, "")
	if code := run([]string{"--config", lcfg, "--check-readiness"}); code != 0 {
		t.Fatalf("llama seed must exit 0, got %d", code)
	}
	lm := dccReadCache(t, lcache)
	lsec, _ := lm["llama"].(map[string]any)
	if len(lsec) == 0 {
		t.Fatalf("llama unit missing: %v", lm)
	}
	var lbefore float64
	for _, v := range lsec {
		u, _ := v.(map[string]any)
		lbefore, _ = u["succeeded_at"].(float64)
	}
	lhealthy.Store(false)
	if code := run([]string{"--config", lcfg, "--check-readiness"}); code != 3 {
		t.Fatalf("stale llama must exit 3, got %d", code)
	}
	lm2 := dccReadCache(t, lcache)
	for _, v := range lm2["llama"].(map[string]any) {
		u, _ := v.(map[string]any)
		if at, _ := u["succeeded_at"].(float64); at != lbefore {
			t.Fatalf("stale llama time must be preserved: %v -> %v", lbefore, at)
		}
	}
}

func TestDiscoveryCacheContractEmptyRefreshReplaces(t *testing.T) {
	dccEnv(t)
	var hits atomic.Int64
	var payload atomic.Value
	payload.Store(`[{"id": "cat-a"}]`)
	var healthy atomic.Bool
	healthy.Store(true)
	srv := dccCatalogSrv(t, &hits, &payload, &healthy)
	defer srv.Close()
	cachePath := filepath.Join(t.TempDir(), "c.json")
	cfg := dccWriteCfg(t, srv.URL, nil, cachePath, "")
	if code := run([]string{"--config", cfg, "--check-readiness"}); code != 0 {
		t.Fatalf("seed must exit 0, got %d", code)
	}
	payload.Store(`[]`)
	if code := run([]string{"--config", cfg, "--check-readiness"}); code != 3 {
		t.Fatalf("empty catalog must exit 3 (no models), got %d", code)
	}
	m := dccReadCache(t, cachePath)
	cat, _ := m["catalog"].(map[string]any)
	models, _ := cat["models"].([]any)
	if len(models) != 0 {
		t.Fatalf("empty success must replace cache with []: %v", m)
	}
}

func TestDiscoveryCacheContractValidateConfigNoIO(t *testing.T) {
	dccEnv(t)
	var hits atomic.Int64
	var payload atomic.Value
	payload.Store(`[{"id": "cat-a"}]`)
	var healthy atomic.Bool
	healthy.Store(true)
	srv := dccCatalogSrv(t, &hits, &payload, &healthy)
	defer srv.Close()
	cachePath := filepath.Join(t.TempDir(), "missing", "dc.json")
	cfg := dccWriteCfg(t, "http://127.0.0.1:9", nil, cachePath, "")
	if code := run([]string{"--config", cfg, "--validate-config"}); code != 0 {
		t.Fatalf("validate must exit 0, got %d", code)
	}
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Fatalf("validate must not create cache file")
	}
	// Closed-port catalog + validate with live URL configured: no probe.
	cache2 := filepath.Join(t.TempDir(), "v2.json")
	cfg2 := dccWriteCfg(t, srv.URL, nil, cache2, "")
	before := hits.Load()
	if code := run([]string{"--config", cfg2, "--validate-config"}); code != 0 {
		t.Fatalf("validate must exit 0, got %d", code)
	}
	if hits.Load() != before {
		t.Fatalf("validate must not probe catalog")
	}
	if _, err := os.Stat(cache2); !os.IsNotExist(err) {
		t.Fatal("validate must not create the configured cache file")
	}
}

func TestDiscoveryCacheContractInvalidCachesIgnored(t *testing.T) {
	dccEnv(t)
	newSeed := func(t *testing.T) (cfg, cachePath string, healthy *atomic.Bool) {
		var hits atomic.Int64
		var payload atomic.Value
		payload.Store(`[{"id": "cat-a"}]`)
		h := &atomic.Bool{}
		h.Store(true)
		srv := dccCatalogSrv(t, &hits, &payload, h)
		t.Cleanup(srv.Close)
		cp := filepath.Join(t.TempDir(), "dc.json")
		c := dccWriteCfg(t, srv.URL, nil, cp, "")
		if code := run([]string{"--config", c, "--check-readiness"}); code != 0 {
			t.Fatalf("seed must exit 0, got %d", code)
		}
		return c, cp, h
	}
	mutate := func(t *testing.T, cachePath string, fn func(m map[string]any)) {
		t.Helper()
		m := dccReadCache(t, cachePath)
		fn(m)
		dccWriteCache(t, cachePath, m)
	}
	cases := map[string]func(t *testing.T, m map[string]any){
		"unknownTopKey": func(t *testing.T, m map[string]any) { m["zzz_unknown"] = 1 },
		"v1":            func(t *testing.T, m map[string]any) { m["schema_version"] = 1 },
		"expired":       func(t *testing.T, m map[string]any) { m["catalog"].(map[string]any)["succeeded_at"] = 0.0 },
		"future":        func(t *testing.T, m map[string]any) { m["catalog"].(map[string]any)["succeeded_at"] = 9999999999.0 },
		"badUnitShape":  func(t *testing.T, m map[string]any) { m["catalog"] = "nope" },
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			dccEnv(t)
			cfg, cp, healthy := newSeed(t)
			mutate(t, cp, func(m map[string]any) { fn(t, m) })
			healthy.Store(false) // live cold path fails
			if code := run([]string{"--config", cfg, "--check-readiness"}); code != 3 {
				t.Fatalf("%s cache must be ignored (live cold -> 1), got %d", name, code)
			}
			dccAssertMode0600(t, cp)
		})
	}
	t.Run("rotatedCredential", func(t *testing.T) {
		dccEnv(t)
		var hits atomic.Int64
		var payload atomic.Value
		payload.Store(`[{"id": "cat-a"}]`)
		var healthy atomic.Bool
		healthy.Store(true)
		srv := dccCatalogSrv(t, &hits, &payload, &healthy)
		defer srv.Close()
		cp := filepath.Join(t.TempDir(), "dc.json")
		cfgA := dccWriteCfg(t, srv.URL, nil, cp, "")
		// Inject a credential into the provider after seeding by rewriting cfg.
		if code := run([]string{"--config", cfgA, "--check-readiness"}); code != 0 {
			t.Fatalf("seed must exit 0, got %d", code)
		}
		// Rewrite same cfg but with an api_key (rotation changes identity).
		raw, err := os.ReadFile(cfgA)
		if err != nil {
			t.Fatal(err)
		}
		rotated := strings.Replace(string(raw),
			"base_url = \"http://127.0.0.1:9\"",
			"base_url = \"http://127.0.0.1:9\"\napi_key = \"rotated-secret-001\"", 1)
		cfgB := filepath.Join(t.TempDir(), "router2.toml")
		if err := os.WriteFile(cfgB, []byte(rotated), 0o600); err != nil {
			t.Fatal(err)
		}
		healthy.Store(false)
		if code := run([]string{"--config", cfgB, "--check-readiness"}); code != 3 {
			t.Fatalf("rotated credential must invalidate cache (->3), got %d", code)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		dccEnv(t)
		cfg, cp, healthy := newSeed(t)
		target := cp + ".real"
		if err := os.Rename(cp, target); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, cp); err != nil {
			t.Skip("symlink not supported")
		}
		healthy.Store(false)
		if code := run([]string{"--config", cfg, "--check-readiness"}); code != 3 {
			t.Fatalf("symlink cache must be ignored (->3), got %d", code)
		}
	})
	t.Run("unsafePerms", func(t *testing.T) {
		dccEnv(t)
		cfg, cp, healthy := newSeed(t)
		if err := os.Chmod(cp, 0o644); err != nil {
			t.Fatal(err)
		}
		healthy.Store(false)
		if code := run([]string{"--config", cfg, "--check-readiness"}); code != 3 {
			t.Fatalf("world-readable cache must be ignored (->3), got %d", code)
		}
	})
}

func TestDiscoveryCacheContractNoPartialRestore(t *testing.T) {
	dccEnv(t)
	var catHits, h1, h2 atomic.Int64
	var catPayload, p1, p2 atomic.Value
	catPayload.Store(`[{"id": "cat-a"}]`)
	p1.Store(`{"data": [{"id": "m1"}]}`)
	p2.Store(`{"data": [{"id": "m2"}]}`)
	var catHealthy, ok1, ok2 atomic.Bool
	catHealthy.Store(true)
	ok1.Store(true)
	ok2.Store(true)
	catSrv := dccCatalogSrv(t, &catHits, &catPayload, &catHealthy)
	defer catSrv.Close()
	s1 := dccLlamaSrv(t, &h1, &p1, &ok1)
	defer s1.Close()
	s2 := dccLlamaSrv(t, &h2, &p2, &ok2)
	defer s2.Close()
	cachePath := filepath.Join(t.TempDir(), "dc.json")
	cfg := dccWriteCfg(t, catSrv.URL, []string{s1.URL, s2.URL}, cachePath, "")
	if code := run([]string{"--config", cfg, "--check-readiness"}); code != 0 {
		t.Fatalf("seed must exit 0, got %d", code)
	}
	// Corrupt the second llama unit; keep catalog unit valid.
	m := dccReadCache(t, cachePath)
	if ll, ok := m["llama"].(map[string]any); ok {
		ll["1"] = "corrupt"
	} else {
		t.Fatalf("llama section missing: %v", m)
	}
	dccWriteCache(t, cachePath, m)
	catHealthy.Store(false)
	ok1.Store(false)
	ok2.Store(false)
	if code := run([]string{"--config", cfg, "--check-readiness"}); code != 3 {
		t.Fatalf("corrupt second llama must fail whole restore (->3), got %d", code)
	}
}

func TestDiscoveryCacheContractSafeIDAndMetadataOmitted(t *testing.T) {
	dccEnv(t)
	secret := "sekDCCSecret001"
	headerTok := "tokDCCHeader002"
	big := strings.Repeat("B", 2048)
	var hits atomic.Int64
	var payload atomic.Value
	payload.Store(fmt.Sprintf(`[{"id": "cat-a"}, {"id": %q}, {"id": "http://example.com/x"}, {"id": "cat-meta", "metadata": {"owner": "x", "big": %q}}]`, secret, big))
	var healthy atomic.Bool
	healthy.Store(true)
	srv := dccCatalogSrv(t, &hits, &payload, &healthy)
	defer srv.Close()
	cachePath := filepath.Join(t.TempDir(), "dc.json")
	var b strings.Builder
	b.WriteString("[server]\nauth_mode = \"none\"\nallow_dynamic_routes = false\n[retry]\nmax_attempts = 1\n")
	b.WriteString("[providers.up]\ndialect = \"anthropic\"\nbase_url = \"http://127.0.0.1:9\"\napi_key = \"" + secret + "\"\n")
	fmt.Fprintf(&b, "[catalog]\nurl = %q\ntimeout = 1\nrefresh_seconds = 0\nheaders = { \"X-Cache-Probe\" = %q }\n", srv.URL, headerTok)
	b.WriteString("[catalog.model_providers]\n\"cat-a\" = \"up\"\n\"cat-meta\" = \"up\"\n")
	fmt.Fprintf(&b, "%q = \"up\"\n%q = \"up\"\n", secret, "http://example.com/x")
	fmt.Fprintf(&b, "[discovery_cache]\npath = %q\nmax_age_seconds = 3600\n", cachePath)
	cfg := filepath.Join(t.TempDir(), "router.toml")
	if err := os.WriteFile(cfg, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"--config", cfg, "--check-readiness"}); code != 0 {
		t.Fatalf("seed must exit 0, got %d", code)
	}
	raw, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("cache must exist: %v", err)
	}
	s := string(raw)
	for _, leak := range []string{secret, headerTok, big, "http://example.com/x", "owner", "capabilities"} {
		if strings.Contains(s, leak) {
			t.Fatalf("cache must omit secret/URL/metadata %q: %s", leak, s)
		}
	}
	if !strings.Contains(s, "cat-a") {
		t.Fatalf("positive control cat-a must persist: %s", s)
	}
	dccAssertMode0600(t, cachePath)
}

func TestDiscoveryCacheContractPrefixLocalAliasRoundtrip(t *testing.T) {
	dccEnv(t)
	var hits atomic.Int64
	var payload atomic.Value
	payload.Store(`{"data": [{"id": "local"}, {"id": "m1", "aliases": ["extra"]}]}`)
	var healthy atomic.Bool
	healthy.Store(true)
	srv := dccLlamaSrv(t, &hits, &payload, &healthy)
	defer srv.Close()
	cachePath := filepath.Join(t.TempDir(), "dc.json")
	body := "[server]\nauth_mode = \"none\"\nallow_dynamic_routes = false\n[retry]\nmax_attempts = 1\n" +
		"[llama_cpp]\nservers = [" + fmt.Sprintf("%q", srv.URL) + "]\n" +
		"timeout = 1\nrefresh_seconds = 0\nprefix = \"p-\"\naliases = true\n" +
		"[discovery_cache]\npath = " + fmt.Sprintf("%q", cachePath) + "\nmax_age_seconds = 3600\n"
	cfg := filepath.Join(t.TempDir(), "router.toml")
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"--config", cfg, "--check-readiness"}); code != 0 {
		t.Fatalf("llama prefix seed must exit 0, got %d", code)
	}
	raw, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("cache must exist: %v", err)
	}
	s := string(raw)
	if strings.Contains(s, "p-local-llama") || !strings.Contains(s, `"local"`) {
		t.Fatalf("cache must store raw upstream id local, not renamed: %s", s)
	}
	if !strings.Contains(s, `"m1"`) {
		t.Fatalf("cache must store raw llama id m1: %s", s)
	}
	if !strings.Contains(s, `"p-extra"`) {
		t.Fatalf("cache must preserve the full prefixed alias: %s", s)
	}
	for _, leak := range []string{srv.URL} {
		if strings.Contains(s, leak) {
			t.Fatalf("cache must not persist URL %q: %s", leak, s)
		}
	}
}
