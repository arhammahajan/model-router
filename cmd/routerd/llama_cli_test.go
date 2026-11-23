// Llama CLI: offline validation, bounded readiness attempts per source,
// llama-only installs, and stale-retained read paths with no per-request
// network I/O.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/discovery"
	"github.com/arhammahajan/router/internal/server"
)

func writeLlamaCLICfg(t *testing.T, serversTOML string) string {
	t.Helper()
	t.Setenv("LLAMA_SERVER_URL", "")
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

func llamaFake(t *testing.T, hits *atomic.Int64, rows string, healthy *atomic.Bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" || r.URL.Path == "/v1/models" || r.URL.Path == "/props" {
			hits.Add(1)
		}
		switch r.URL.Path {
		case "/health":
			if healthy != nil && !healthy.Load() {
				w.WriteHeader(500)
				return
			}
			w.WriteHeader(200)
		case "/v1/models":
			if healthy != nil && !healthy.Load() {
				w.WriteHeader(500)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(rows))
		default:
			w.WriteHeader(500)
		}
	}))
}

func TestLlamaValidateConfigStaysOffline(t *testing.T) {
	catalogTestEnv(t)
	var hits atomic.Int64
	var healthy atomic.Bool
	healthy.Store(true)
	srv := llamaFake(t, &hits, `{"data": [{"id": "m1"}]}`, &healthy)
	defer srv.Close()
	cfg := fmt.Sprintf("[llama_cpp]\nservers = [\"%s\"]\nrefresh_seconds = 0\n", srv.URL)
	if code := run([]string{"--config", writeLlamaCLICfg(t, cfg), "--validate-config"}); code != 0 {
		t.Fatalf("validate must exit 0, got %d", code)
	}
	if hits.Load() != 0 {
		t.Fatalf("validate-config must not touch llama servers: %d hits", hits.Load())
	}
}

func TestLlamaCheckReadinessBoundedPerServer(t *testing.T) {
	catalogTestEnv(t)
	var hits atomic.Int64
	var healthy atomic.Bool
	healthy.Store(true)
	s1 := llamaFake(t, &hits, `{"data": [{"id": "a1"}]}`, &healthy)
	defer s1.Close()
	s2 := llamaFake(t, &hits, `{"data": [{"id": "b1"}, {"id": "b2"}]}`, &healthy)
	defer s2.Close()
	cfg := fmt.Sprintf("[llama_cpp]\nservers = [\"%s\", \"%s\"]\nrefresh_seconds = 0\n", s1.URL, s2.URL)
	if code := run([]string{"--config", writeLlamaCLICfg(t, cfg), "--check-readiness"}); code != 0 {
		t.Fatalf("llama-ready must exit 0, got %d", code)
	}
	// Exactly one fleet attempt: health+models+props per server.
	if hits.Load() != 6 {
		t.Fatalf("readiness must perform exactly one attempt per server (3 GETs each): %d", hits.Load())
	}
}

func TestLlamaCheckReadinessFailureUnready(t *testing.T) {
	catalogTestEnv(t)
	var hits atomic.Int64
	var healthy atomic.Bool // false: every probe fails
	srv := llamaFake(t, &hits, `{"data": [{"id": "m1"}]}`, &healthy)
	defer srv.Close()
	cfg := fmt.Sprintf("[llama_cpp]\nservers = [\"%s\"]\nrefresh_seconds = 0\n", srv.URL)
	if code := run([]string{"--config", writeLlamaCLICfg(t, cfg), "--check-readiness"}); code != 3 {
		t.Fatalf("failed llama must exit 3, got %d", code)
	}
	if hits.Load() != 1 {
		t.Fatalf("failed readiness still performs one bounded attempt: %d", hits.Load())
	}
}

func TestLlamaOnlyZeroProvidersReady(t *testing.T) {
	catalogTestEnv(t)
	// No [providers] table at all: llama-only installs are valid.
	var hits atomic.Int64
	var healthy atomic.Bool
	healthy.Store(true)
	srv := llamaFake(t, &hits, `{"data": [{"id": "m1"}]}`, &healthy)
	defer srv.Close()
	cfg := fmt.Sprintf("[llama_cpp]\nservers = [\"%s\"]\nrefresh_seconds = 0\n", srv.URL)
	loaded, err := config.Load(writeLlamaCLICfg(t, cfg))
	if err != nil {
		t.Fatalf("llama-only must load: %v", err)
	}
	if len(loaded.Providers) != 0 || len(loaded.LlamaProviders()) != 1 {
		t.Fatalf("providers=%d llama=%d", len(loaded.Providers), len(loaded.LlamaProviders()))
	}
	if code := run([]string{"--config", writeLlamaCLICfg(t, cfg), "--check-readiness"}); code != 0 {
		t.Fatalf("llama-only ready must exit 0, got %d", code)
	}
}

func TestLlamaServeStaleRetainedNoRequestIO(t *testing.T) {
	catalogTestEnv(t)
	var hits atomic.Int64
	var healthy atomic.Bool
	healthy.Store(true)
	srv := llamaFake(t, &hits, `{"data": [{"id": "m1", "aliases": ["m-alias"]}]}`, &healthy)
	cfg, err := config.Load(writeLlamaCLICfg(t,
		fmt.Sprintf("[llama_cpp]\nservers = [\"%s\"]\ntimeout = 5\nrefresh_seconds = 0\n", srv.URL)))
	if err != nil {
		t.Fatal(err)
	}
	s := server.New(cfg)
	worker := discovery.NewLlamaWorker(cfg.Llama, s.Registry())
	defer worker.Stop()
	worker.RefreshOnce(t.Context())
	if !s.Registry().Current().Ready() {
		t.Fatal("seeded llama must be ready")
	}
	api := httptest.NewServer(s.Handler())
	defer api.Close()
	models := func() []string {
		t.Helper()
		resp, err := http.Get(api.URL + "/v1/models")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var payload struct {
			Data []struct {
				ID       string         `json:"id"`
				OwnedBy  string         `json:"owned_by"`
				Metadata map[string]any `json:"metadata"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, m := range payload.Data {
			if m.OwnedBy != "router" {
				t.Fatalf("owned_by=%q", m.OwnedBy)
			}
			out = append(out, m.ID)
		}
		return out
	}
	ready := func() int {
		t.Helper()
		resp, err := http.Get(api.URL + "/readyz")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	before := hits.Load()
	if got := models(); len(got) != 1 || got[0] != "m1" {
		t.Fatalf("listing=%q", got)
	}
	if code := ready(); code != 200 {
		t.Fatalf("readyz=%d", code)
	}
	if hits.Load() != before {
		t.Fatal("read paths must not dial upstream")
	}
	// Outage: stale routes stay listed and resolvable, readiness drops.
	healthy.Store(false)
	worker.RefreshOnce(t.Context())
	if code := ready(); code != 503 {
		t.Fatalf("stale-only readyz=%d, want 503", code)
	}
	if got := models(); len(got) != 1 || got[0] != "m1" {
		t.Fatalf("stale listing=%q", got)
	}
	if hits.Load() <= before {
		t.Fatal("refresh must have attempted the probe")
	}
	afterRefresh := hits.Load()
	_ = models()
	_ = ready()
	if hits.Load() != afterRefresh {
		t.Fatal("stale read paths must not dial upstream")
	}
}
