// Operational CLI contract (M4 next slice, RED before implementation).
//
// Blackbox tests over run(args) only: stdout JSON is read from the last
// non-empty stdout line (flag/parser debug stays on stderr and is never
// parsed as JSON). Hermetic per-test HOME/XDG/credential env with fake
// httptest transports; no live network, no cache-file side effects unless
// asserted absent.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func opsEnv(t *testing.T) {
	t.Helper()
	t.Setenv("MODEL_ROUTER_API_KEY", "")
	t.Setenv("MODEL_ROUTER_API_KEYS", "")
	t.Setenv("LLAMA_SERVER_URL", "")
	t.Setenv("ROUTER_CONFIG", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func opsCapture(t *testing.T, args []string) (int, string) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		os.Stdout = old
		_ = w.Close()
		_ = r.Close()
	}()
	os.Stdout = w
	code := run(args)
	_ = w.Close()
	os.Stdout = old
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return code, string(data)
}

func opsLastJSON(t *testing.T, stdout string) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[len(lines)-1]) == "" {
		t.Fatalf("no stdout lines to parse as JSON: %q", stdout)
	}
	last := strings.TrimSpace(lines[len(lines)-1])
	var payload map[string]any
	if err := json.Unmarshal([]byte(last), &payload); err != nil {
		t.Fatalf("final stdout line must be JSON: %v\nstdout=%q", err, stdout)
	}
	return payload
}

func opsWriteStaticCfg(t *testing.T, extra string) string {
	t.Helper()
	body := `
[server]
auth_mode = "none"
allow_dynamic_routes = false
[retry]
max_attempts = 1
[providers.chatup]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
[[models]]
name = "m-a"
provider = "chatup"
model = "gpt-4o"
aliases = ["m-alias"]
` + extra
	p := filepath.Join(t.TempDir(), "router.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func opsWriteCatalogCfg(t *testing.T, catalogURL, cachePath, extra string) string {
	t.Helper()
	cacheStanza := ""
	if cachePath != "" {
		cacheStanza = "[discovery_cache]\npath = \"" + cachePath + "\"\n"
	}
	body := `
[server]
auth_mode = "none"
allow_dynamic_routes = false
[retry]
max_attempts = 1
[providers.chatup]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
[catalog]
url = "` + catalogURL + `"
timeout = 5
refresh_seconds = 0
model_providers = { "cat-a" = "chatup" }
` + cacheStanza + extra
	p := filepath.Join(t.TempDir(), "router.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func opsWriteLlamaCfg(t *testing.T, serversTOML string) string {
	t.Helper()
	body := `
[server]
auth_mode = "none"
allow_dynamic_routes = false
[retry]
max_attempts = 1
` + serversTOML
	p := filepath.Join(t.TempDir(), "router.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func opsCatalogFake(t *testing.T, hits *atomic.Int64, body string, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
}

func opsLlamaFake(t *testing.T, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(200)
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data": [{"id": "m1"}]}`))
		case "/props":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(500)
		}
	}))
}

func TestOpsValidateConfigJSON(t *testing.T) {
	opsEnv(t)
	var hits atomic.Int64
	srv := opsCatalogFake(t, &hits, `[{"id":"cat-a"}]`, 0)
	defer srv.Close()
	cachePath := filepath.Join(t.TempDir(), "cache.json")
	cfg := opsWriteCatalogCfg(t, srv.URL, cachePath, "")
	code, out := opsCapture(t, []string{"--config", cfg, "--validate-config"})
	if code != 0 {
		t.Fatalf("validate must exit 0, got %d stdout=%q", code, out)
	}
	payload := opsLastJSON(t, out)
	if payload["status"] != "valid" || payload["network_checked"] != false {
		t.Fatalf("validate JSON must be {status:valid network_checked:false}, got %v", payload)
	}
	if hits.Load() != 0 {
		t.Fatalf("validate must not probe the catalog: %d", hits.Load())
	}
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Fatalf("validate must not create the cache file")
	}
}

func TestOpsValidateInvalidAndUnknownFlags(t *testing.T) {
	opsEnv(t)
	bad := filepath.Join(t.TempDir(), "router.toml")
	if err := os.WriteFile(bad, []byte("not = [valid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _ := opsCapture(t, []string{"--config", bad, "--validate-config"}); code != 2 {
		t.Fatalf("invalid config must exit 2, got %d", code)
	}
	good := opsWriteStaticCfg(t, "")
	if code, _ := opsCapture(t, []string{"--config", good, "--bogus-flag"}); code != 2 {
		t.Fatalf("unknown flag must exit 2, got %d", code)
	}
}

func TestOpsCheckReadinessReadyAliasCounts(t *testing.T) {
	opsEnv(t)
	cfg := opsWriteStaticCfg(t, "")
	for _, flag := range []string{"--check-readiness", "--check"} {
		code, out := opsCapture(t, []string{"--config", cfg, flag})
		if code != 0 {
			t.Fatalf("%s ready must exit 0, got %d stdout=%q", flag, code, out)
		}
		payload := opsLastJSON(t, out)
		if payload["status"] != "ready" {
			t.Fatalf("%s JSON status must be ready: %v", flag, payload)
		}
		if payload["ready"] != true {
			t.Fatalf("%s JSON ready must be true: %v", flag, payload)
		}
		if payload["enabled_routes"] != float64(2) {
			t.Fatalf("%s enabled_routes must count alias (2): %v", flag, payload)
		}
		if payload["usable_routes"] != float64(2) {
			t.Fatalf("%s usable_routes must count alias (2): %v", flag, payload)
		}
		discovery, ok := payload["discovery"].(map[string]any)
		if !ok {
			t.Fatalf("%s JSON must carry discovery summary: %v", flag, payload)
		}
		if len(discovery) != 0 {
			t.Fatalf("%s static-only discovery must list configured sources only ({}): %v", flag, discovery)
		}
	}
}

func TestOpsCheckUnreadyExitsThree(t *testing.T) {
	opsEnv(t)
	// Build disabled-only and dynamic-only configs without a usable named route.
	disabledOnly := filepath.Join(t.TempDir(), "disabled.toml")
	disabledBody := `
[server]
auth_mode = "none"
allow_dynamic_routes = false
[retry]
max_attempts = 1
[providers.chatup]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
[[models]]
name = "a"
provider = "chatup"
model = "x"
enabled = false
`
	if err := os.WriteFile(disabledOnly, []byte(disabledBody), 0o600); err != nil {
		t.Fatal(err)
	}
	dynamicOnly := filepath.Join(t.TempDir(), "dynamic.toml")
	dynamicBody := `
[server]
auth_mode = "none"
allow_dynamic_routes = true
[retry]
max_attempts = 1
[providers.chatup]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
`
	if err := os.WriteFile(dynamicOnly, []byte(dynamicBody), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, cfg := range []string{disabledOnly, dynamicOnly} {
		for _, flag := range []string{"--check-readiness", "--check"} {
			code, out := opsCapture(t, []string{"--config", cfg, flag})
			if code != 3 {
				t.Fatalf("%s %s unready must exit 3, got %d stdout=%q", cfg, flag, code, out)
			}
			payload := opsLastJSON(t, out)
			if payload["status"] != "not_ready" || payload["ready"] != false {
				t.Fatalf("%s JSON must be not_ready/false: %v", flag, payload)
			}
		}
	}
}

func TestOpsOfflineCheckIndeterminate(t *testing.T) {
	opsEnv(t)
	var hits atomic.Int64
	srv := opsCatalogFake(t, &hits, `[{"id":"cat-a"}]`, 0)
	defer srv.Close()
	cachePath := filepath.Join(t.TempDir(), "cache.json")
	cfg := opsWriteCatalogCfg(t, srv.URL, cachePath, "")
	// Even a static-ready config is indeterminate offline: discovery is skipped.
	staticReady := opsWriteStaticCfg(t, "")
	for _, cfgPath := range []string{staticReady, cfg} {
		code, out := opsCapture(t, []string{"--config", cfgPath, "--offline", "--check-readiness"})
		if code != 4 {
			t.Fatalf("offline check must exit 4, got %d stdout=%q", code, out)
		}
		payload := opsLastJSON(t, out)
		if payload["status"] != "indeterminate" || payload["network_checked"] != false ||
			payload["reason"] != "offline_readiness_requires_discovery" {
			t.Fatalf("offline JSON must be indeterminate/no-network/with reason: %v", payload)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("offline check must not probe: %d", hits.Load())
	}
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Fatalf("offline check must not write the cache file")
	}
}

func TestOpsOfflineValidateAllowed(t *testing.T) {
	opsEnv(t)
	cfg := opsWriteStaticCfg(t, "")
	code, out := opsCapture(t, []string{"--config", cfg, "--offline", "--validate-config"})
	if code != 0 {
		t.Fatalf("offline validate must exit 0, got %d stdout=%q", code, out)
	}
	payload := opsLastJSON(t, out)
	if payload["status"] != "valid" {
		t.Fatalf("offline validate JSON must be valid: %v", payload)
	}
}

func TestOpsOfflineServeUsage(t *testing.T) {
	opsEnv(t)
	cfg := opsWriteStaticCfg(t, "")
	if code, _ := opsCapture(t, []string{"--config", cfg, "--offline"}); code != 2 {
		t.Fatalf("offline without check/validate is usage error 2, got %d", code)
	}
	if code, _ := opsCapture(t, []string{"--config", cfg, "--offline", "--port", "1"}); code != 2 {
		t.Fatalf("offline serve must exit 2, got %d", code)
	}
}

func TestOpsBindFailureReturnsOne(t *testing.T) {
	opsEnv(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	cfg := opsWriteStaticCfg(t, "")
	if code, _ := opsCapture(t, []string{"--config", cfg, "--port", fmt.Sprint(port)}); code != 1 {
		t.Fatalf("bind conflict must exit 1, got %d", code)
	}
}

func TestOpsCatalogOneBoundedAttempt(t *testing.T) {
	opsEnv(t)
	var hits atomic.Int64
	srv := opsCatalogFake(t, &hits, `[{"id": "cat-a"}]`, 0)
	defer srv.Close()
	cfg := opsWriteCatalogCfg(t, srv.URL, "", "")
	code, out := opsCapture(t, []string{"--config", cfg, "--check-readiness"})
	if code != 0 {
		t.Fatalf("catalog-ready must exit 0, got %d stdout=%q", code, out)
	}
	payload := opsLastJSON(t, out)
	if payload["status"] != "ready" {
		t.Fatalf("catalog-ready JSON must be ready: %v", payload)
	}
	if hits.Load() != 1 {
		t.Fatalf("readiness must perform exactly one catalog attempt: %d", hits.Load())
	}
}

func TestOpsLlamaOneBoundedAttemptPerServer(t *testing.T) {
	opsEnv(t)
	var hits atomic.Int64
	srv := opsLlamaFake(t, &hits)
	defer srv.Close()
	cfg := opsWriteLlamaCfg(t, fmt.Sprintf("[providers.llama_cpp]\nservers = [\"%s\"]\nrefresh_seconds = 0\n", srv.URL))
	code, out := opsCapture(t, []string{"--config", cfg, "--check-readiness"})
	if code != 0 {
		t.Fatalf("llama-ready must exit 0, got %d stdout=%q", code, out)
	}
	payload := opsLastJSON(t, out)
	if payload["status"] != "ready" {
		t.Fatalf("llama-ready JSON must be ready: %v", payload)
	}
	if hits.Load() != 3 {
		t.Fatalf("llama readiness must be health+models+props (3): %d", hits.Load())
	}
}
