package routerctl

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	routerenv "github.com/arhammahajan/router/internal/routercli/env"
)

func writeProviderTestConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "router.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFetchProvidersAllKindsAndSanitization(t *testing.T) {
	// Start a local TCP listener to simulate a reachable local llama_cpp server.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	livePort := ln.Addr().(*net.TCPAddr).Port

	// Pick a closed port to simulate an unreachable local strata server.
	closedLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedPort := closedLn.Addr().(*net.TCPAddr).Port
	closedLn.Close()

	cfgPath := writeProviderTestConfig(t, fmt.Sprintf(`
[server]
auth_mode = "none"

[providers.remote-chat]
dialect = "openai-chat"
base_url = "https://api.example.com/v1"
api_key = "super-secret-remote-key"
compatibility = "lossy"
timeout = 120

[providers.llama_cpp]
ports = [%d]
probe_timeout = 0.5
timeout = 300
prefix = "local/"

[providers.strata]
base_url = "http://127.0.0.1:%d"
probe_timeout = 0.2
`, livePort, closedPort))

	opts := Options{
		Router:       routerenv.RouterSettings{URL: "http://127.0.0.1:1"},
		RouterConfig: cfgPath,
	}
	items, err := FetchProviders(context.Background(), opts)
	if err != nil {
		t.Fatalf("FetchProviders: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("expected 3 providers, got %d: %+v", len(items), items)
	}

	// Ensure no secret value leaks anywhere in the serialized JSON.
	raw, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "super-secret-remote-key") {
		t.Fatalf("secret leaked in ProviderInfo JSON: %s", raw)
	}

	byKind := map[string]ProviderInfo{}
	for _, it := range items {
		byKind[it.Kind] = it
	}

	remote := byKind["generic"]
	if remote.Name != "remote-chat" || remote.Host != "api.example.com" || remote.Port != 443 || remote.Status != "configured" {
		t.Fatalf("remote provider wrong: %+v", remote)
	}
	if remote.AuthHeader != "auto" || !remote.APIKeyConfigured || remote.Compatibility != "lossy" || remote.TimeoutSeconds != 120 {
		t.Fatalf("remote shared fields wrong: %+v", remote)
	}

	llama := byKind["llama_cpp"]
	if llama.Port != livePort || llama.Host != "127.0.0.1" || llama.Status != "reachable" || llama.Prefix != "local/" || llama.ProbeTimeoutSeconds != 0.5 || llama.TimeoutSeconds != 300 {
		t.Fatalf("llama provider wrong: %+v", llama)
	}

	strata := byKind["strata"]
	if strata.Port != closedPort || strata.Host != "127.0.0.1" || strata.Status != "unreachable" || strata.ProbeTimeoutSeconds != 0.2 {
		t.Fatalf("strata provider wrong: %+v", strata)
	}
}

func TestFetchProvidersUsesDiagnosticsAndCLIProxyMode(t *testing.T) {
	diagSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/diagnostics" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"providers":{"cliproxyapi":{"reachability":"available"}}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer diagSrv.Close()

	cpaURL, err := url.Parse(diagSrv.URL)
	if err != nil {
		t.Fatal(err)
	}
	cpaPort, _ := strconv.Atoi(cpaURL.Port())

	cfgPath := writeProviderTestConfig(t, fmt.Sprintf(`
[server]
auth_mode = "none"

[providers.cliproxyapi]
base_url = "http://127.0.0.1:%d/v1"
api_key = "proxy-secret"
passthrough = true
`, cpaPort))

	opts := Options{
		Router:       routerenv.RouterSettings{URL: diagSrv.URL},
		RouterConfig: cfgPath,
	}
	info, err := FetchProvider(context.Background(), opts, "cliproxyapi")
	if err != nil {
		t.Fatalf("FetchProvider: %v", err)
	}
	if info.Kind != "cliproxyapi" || info.Status != "available" || info.Port != cpaPort || info.Mode != "external" {
		t.Fatalf("cliproxyapi provider wrong: %+v", info)
	}
	if len(info.SupportedDialects) != 3 {
		t.Fatalf("expected 3 supported dialects, got %+v", info.SupportedDialects)
	}

	if _, err := FetchProvider(context.Background(), opts, "missing"); err == nil {
		t.Fatal("expected error for unknown provider")
	}
}
