package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/arhammahajan/router/internal/config"
)

// Diagnostics proofs: auth order (403 before 401, headers-only, no ?key),
// exact Python shape with nullable keys, secret redaction and read-path
// isolation (no network/FS/clock/lock dependence, no refresh triggered).

func diagnosticsCfg(catURL, llamaURL string, keys ...string) *config.Config {
	return &config.Config{
		Server: config.Server{
			Host:               "127.0.0.1",
			Port:               0,
			APIKeys:            keys,
			AllowDynamicRoutes: false,
			MaxConnections:     8,
			MaxGenerations:     8,
		},
		Providers: map[string]*config.Provider{
			"up": {
				Name:           "up",
				DefaultDialect: config.DialectAnthropic,
				BaseURL:        "http://127.0.0.1:9",
				APIKey:         "sk-testproviderkey1",
				Headers:        map[string]string{"X-Custom": "header-secret-value"},
				Timeout:        5,
			},
		},
		Models: []config.Model{
			{Name: "a", Provider: "up", Upstream: "x", Aliases: []string{"a-alias"}},
		},
		Catalog: &config.Catalog{
			URL:            catURL + "?token=url-query-secret",
			Timeout:        5,
			RefreshSeconds: 60,
			ModelProviders: map[string]string{},
		},
		Llama: &config.LlamaConfig{
			Servers: []config.LlamaServer{
				{URL: llamaURL, Name: "llama-a"},
			},
			ProbeTimeout:   5,
			RefreshSeconds: 30,
			ExposeAliases:  true,
			Compatibility:  "strict",
		},
	}
}

func TestDiagnosticsAuthOrder(t *testing.T) {
	var hits atomic.Int64
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer fake.Close()

	open := New(diagnosticsCfg(fake.URL, fake.URL))
	rec := httptest.NewRecorder()
	open.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/diagnostics", nil))
	if rec.Code != 403 {
		t.Fatalf("no configured keys must close with 403, got %d", rec.Code)
	}
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != "diagnostics_disabled" {
		t.Fatalf("wrong 403 code: %+v", envelope.Error)
	}

	closed := New(diagnosticsCfg(fake.URL, fake.URL, "router-key-1"))
	// No auth header -> 401 even though the endpoint exists.
	rec = httptest.NewRecorder()
	closed.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/diagnostics", nil))
	if rec.Code != 401 {
		t.Fatalf("unauthed diagnostics must be 401, got %d", rec.Code)
	}
	// ?key is never accepted on Chat read endpoints.
	rec = httptest.NewRecorder()
	closed.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/diagnostics?key=router-key-1", nil))
	if rec.Code != 401 {
		t.Fatalf("query keys must be rejected on diagnostics, got %d", rec.Code)
	}
	// Headers-only auth succeeds.
	req := httptest.NewRequest(http.MethodGet, "/diagnostics", nil)
	req.Header.Set("Authorization", "Bearer router-key-1")
	rec = httptest.NewRecorder()
	closed.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("header auth must open diagnostics, got %d %s", rec.Code, rec.Body.String())
	}
	if hits.Load() != 0 {
		t.Fatalf("diagnostics must not dial discovery: %d hits", hits.Load())
	}
}

func TestDiagnosticsShapeAndRedaction(t *testing.T) {
	var hits atomic.Int64
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer fake.Close()
	srv := New(diagnosticsCfg(fake.URL, fake.URL, "router-key-1"))
	before := srv.Registry().Current()
	req := httptest.NewRequest(http.MethodGet, "/diagnostics", nil)
	req.Header.Set("X-Api-Key", "router-key-1")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("diagnostics must be 200, got %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"status", "providers", "models", "readiness", "provider_details", "catalog", "llama_cpp", "discovery_cache"} {
		if _, ok := body[k]; !ok {
			t.Fatalf("diagnostics missing key %q: %s", k, rec.Body.String())
		}
	}
	if len(body) != 8 {
		t.Fatalf("diagnostics must render exactly the contract keys: %v", body)
	}
	rd, _ := body["readiness"].(map[string]any)
	for _, k := range []string{"status", "ready", "enabled_routes", "usable_routes", "reason"} {
		if _, ok := rd[k]; !ok {
			t.Fatalf("readiness missing key %q", k)
		}
	}
	if rd["enabled_routes"] != float64(2) || rd["usable_routes"] != float64(2) {
		t.Fatalf("effective named keys must include aliases: %+v", rd)
	}
	dc, _ := body["discovery_cache"].(map[string]any)
	if _, ok := dc["expired_sources"]; ok {
		t.Fatalf("expired_sources must be absent before a successful restore: %+v", dc)
	}
	raw := rec.Body.String()
	for _, secret := range []string{"router-key-1", "sk-testproviderkey1", "header-secret-value", "url-query-secret"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("diagnostic report leaked a secret %q", secret)
		}
	}
	if strings.Contains(raw, "<redacted>@") && strings.Contains(raw, "url-query-secret") {
		t.Fatal("redaction marker without secret removal")
	}
	if srv.Registry().Current() != before {
		t.Fatal("diagnostics must pin the snapshot without publishing")
	}
	if hits.Load() != 0 {
		t.Fatalf("diagnostics must not trigger discovery: %d", hits.Load())
	}
}

func TestLivezReadyzOpenShapes(t *testing.T) {
	srv := New(diagnosticsCfg("http://127.0.0.1:9/x", "http://127.0.0.1:9/y"))
	for _, path := range []string{"/livez", "/health", "/healthz"} {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"status":"ok"}` {
			t.Fatalf("%s must stay open with the lock-free shape: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"status":"ready"}` {
		t.Fatalf("readyz must map ready to 200/ready: %d %s", rec.Code, rec.Body.String())
	}
}
