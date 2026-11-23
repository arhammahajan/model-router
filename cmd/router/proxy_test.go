package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arhammahajan/router/internal/routercli/routerctl"
)

const (
	cliProxyInferKey = "cli-proxy-inference-key"
	cliProxyMgmtKey  = "cli-proxy-management-key"
)

func startCLIProxyMock(t *testing.T) (*httptest.Server, *string) {
	t.Helper()
	var patched string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+cliProxyInferKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"opaque/cli-model","owned_by":"anthropic"}]}`))
	})
	mux.HandleFunc("/v0/management/plugins", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+cliProxyMgmtKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"plugins_enabled":true,"plugins":[{"id":"p1","enabled":true,"effective_enabled":true,"registered":true,"configured":true,"supports_oauth":false,"supports_quota":false}]}`))
	})
	mux.HandleFunc("/v0/management/plugins/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+cliProxyMgmtKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		patched = r.Method + " " + r.URL.Path
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/v0/management/auth-files", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+cliProxyMgmtKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"files":[{"name":"cli.json","type":"codex","status":"active","email":"cli@example.com"}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &patched
}

func writeCLIProxyConfig(t *testing.T, baseURL string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "router.toml")
	body := "[server]\nhost=\"127.0.0.1\"\nport=4000\n[cliproxyapi]\nbase_url=\"" + baseURL + "/v1\"\napi_key=\"" + cliProxyInferKey + "\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestProxyHelpAndRouting(t *testing.T) {
	isolateEnv(t)

	for _, args := range [][]string{{"proxy"}, {"proxy", "--help"}, {"proxy", "-h"}, {"help", "proxy"}} {
		code, stdout, _ := captureBoth(t, func() int { return run(args) })
		if code != 0 || !strings.Contains(stdout, "router proxy") {
			t.Fatalf("run(%v) = (%d, %q)", args, code, stdout)
		}
	}
	code, _, _ := captureBoth(t, func() int { return run([]string{"proxy", "bogus"}) })
	if code != 2 {
		t.Fatalf("proxy bogus = %d, want 2", code)
	}
	code, _, _ = captureBoth(t, func() int { return run([]string{"proxy", "plugin", "enable"}) })
	if code != 2 {
		t.Fatalf("proxy plugin enable without --id = %d, want 2", code)
	}
	code, _, _ = captureBoth(t, func() int { return run([]string{"proxy", "plugin", "restart", "--id", "p1"}) })
	if code != 2 {
		t.Fatalf("proxy plugin bad verb = %d, want 2", code)
	}
}

func TestProxyStatusSeam(t *testing.T) {
	isolateEnv(t)
	orig := routerFetchProxyStatus
	t.Cleanup(func() { routerFetchProxyStatus = orig })
	routerFetchProxyStatus = func(_ context.Context, _ routerctl.Options) (routerctl.ProxyStatus, error) {
		return routerctl.ProxyStatus{
			Mode: "external", Endpoint: "http://127.0.0.1:8317/v1",
			HasInferenceKey: true, Reachable: true, Models: 1,
			Management: "missing-key", Note: "set the key",
		}, nil
	}
	code, stdout, _ := captureBoth(t, func() int { return run([]string{"proxy", "status"}) })
	if code != 0 {
		t.Fatalf("proxy status = %d", code)
	}
	for _, want := range []string{"proxy:      external", "reachable:  true", "management: missing-key"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("status output missing %q:\n%s", want, stdout)
		}
	}
	code, stdout, _ = captureBoth(t, func() int { return run([]string{"proxy", "status", "--json"}) })
	if code != 0 {
		t.Fatalf("proxy status --json = %d", code)
	}
	var st routerctl.ProxyStatus
	if err := json.Unmarshal([]byte(stdout), &st); err != nil || st.Mode != "external" {
		t.Fatalf("status json err=%v st=%+v", err, st)
	}

	routerFetchProxyStatus = func(_ context.Context, _ routerctl.Options) (routerctl.ProxyStatus, error) {
		return routerctl.ProxyStatus{Mode: "external", Reachable: false, Management: "unreachable", Note: "down"}, nil
	}
	code, stdout, _ = captureBoth(t, func() int { return run([]string{"proxy", "status"}) })
	if code != 1 || !strings.Contains(stdout, "reachable:  false") {
		t.Fatalf("unreachable status = (%d, %q), want exit 1", code, stdout)
	}
	code, stdout, _ = captureBoth(t, func() int { return run([]string{"proxy", "status", "--json"}) })
	if code != 1 || !strings.Contains(stdout, `"reachable": false`) {
		t.Fatalf("unreachable JSON status = (%d, %q), want exit 1", code, stdout)
	}
}

func TestProxyPluginControlSeam(t *testing.T) {
	isolateEnv(t)
	orig := routerSetProxyPlugin
	t.Cleanup(func() { routerSetProxyPlugin = orig })
	var gotID string
	var gotEnabled bool
	routerSetProxyPlugin = func(_ context.Context, _ routerctl.Options, id string, enabled bool) error {
		gotID, gotEnabled = id, enabled
		return nil
	}
	code, stdout, _ := captureBoth(t, func() int { return run([]string{"proxy", "plugin", "disable", "--id", "p1"}) })
	if code != 0 || gotID != "p1" || gotEnabled {
		t.Fatalf("disable = (%d, %q, id=%q enabled=%t)", code, stdout, gotID, gotEnabled)
	}
	if !strings.Contains(stdout, "plugin p1 enabled=false") {
		t.Fatalf("disable output = %q", stdout)
	}
	code, stdout, _ = captureBoth(t, func() int { return run([]string{"proxy", "plugin", "enable", "--id", "p1", "--json"}) })
	if code != 0 || !strings.Contains(stdout, `"enabled": true`) {
		t.Fatalf("enable --json = (%d, %q)", code, stdout)
	}
}

func TestProxyEndToEnd(t *testing.T) {
	isolateEnv(t)
	srv, patched := startCLIProxyMock(t)
	t.Setenv("MODEL_ROUTER_CONFIG", writeCLIProxyConfig(t, srv.URL))
	t.Setenv("CLIPROXYAPI_MANAGEMENT_KEY", cliProxyMgmtKey)

	code, stdout, _ := captureBoth(t, func() int { return run([]string{"proxy", "status"}) })
	if code != 0 {
		t.Fatalf("proxy status = %d (%q)", code, stdout)
	}
	for _, want := range []string{"proxy:      external", "reachable:  true", "models:     1", "management: ok"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("status missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, cliProxyInferKey) || strings.Contains(stdout, cliProxyMgmtKey) {
		t.Fatalf("status output leaks a key:\n%s", stdout)
	}

	code, stdout, _ = captureBoth(t, func() int { return run([]string{"proxy", "models"}) })
	if code != 0 || !strings.Contains(stdout, "opaque/cli-model (anthropic)") {
		t.Fatalf("proxy models = (%d, %q)", code, stdout)
	}

	code, stdout, _ = captureBoth(t, func() int { return run([]string{"proxy", "models", "--json"}) })
	if code != 0 {
		t.Fatalf("proxy models --json = %d", code)
	}
	var models []routerctl.ProxyModel
	if err := json.Unmarshal([]byte(stdout), &models); err != nil || len(models) != 1 || models[0].ID != "opaque/cli-model" {
		t.Fatalf("models json err=%v models=%+v", err, models)
	}

	code, stdout, _ = captureBoth(t, func() int { return run([]string{"proxy", "plugins"}) })
	if code != 0 || !strings.Contains(stdout, "p1") {
		t.Fatalf("proxy plugins = (%d, %q)", code, stdout)
	}

	code, stdout, _ = captureBoth(t, func() int { return run([]string{"proxy", "plugin", "disable", "--id", "p1"}) })
	if code != 0 || *patched != "PATCH /v0/management/plugins/p1/enabled" {
		t.Fatalf("plugin disable = (%d, %q, patched=%q)", code, stdout, *patched)
	}

	code, stdout, _ = captureBoth(t, func() int { return run([]string{"proxy", "accounts"}) })
	if code != 0 || !strings.Contains(stdout, "cli.json") || !strings.Contains(stdout, "cli@example.com") {
		t.Fatalf("proxy accounts = (%d, %q)", code, stdout)
	}

	// Management commands fail closed without the dedicated key, and the
	// failure names the variable without echoing any secret.
	t.Setenv("CLIPROXYAPI_MANAGEMENT_KEY", "")
	code, _, stderr := captureBoth(t, func() int { return run([]string{"proxy", "plugins"}) })
	if code != 1 || !strings.Contains(stderr, "CLIPROXYAPI_MANAGEMENT_KEY") {
		t.Fatalf("plugins without key = (%d, %q)", code, stderr)
	}
	if strings.Contains(stderr, cliProxyInferKey) {
		t.Fatalf("plugins error leaks inference key: %q", stderr)
	}
}

func TestProxyCLISecretIsolation(t *testing.T) {
	isolateEnv(t)
	// Unreachable proxy: errors must not echo the keys from config or env.
	t.Setenv("MODEL_ROUTER_CONFIG", writeCLIProxyConfig(t, "http://127.0.0.1:1"))
	t.Setenv("CLIPROXYAPI_MANAGEMENT_KEY", cliProxyMgmtKey)
	for _, args := range [][]string{{"proxy", "status"}, {"proxy", "models"}, {"proxy", "plugins"}, {"proxy", "accounts"}} {
		_, stdout, stderr := captureBoth(t, func() int { return run(args) })
		combined := stdout + "\n" + stderr
		if strings.Contains(combined, cliProxyInferKey) || strings.Contains(combined, cliProxyMgmtKey) {
			t.Fatalf("run(%v) leaks a key:\n%s", args, combined)
		}
	}
}
