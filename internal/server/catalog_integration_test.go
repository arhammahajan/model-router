// Catalog server integration: registry-backed routing over the shared
// pin-per-request path, stale retention, unmapped faults and no-I/O
// read paths.
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arhammahajan/model-router/internal/config"
	"github.com/arhammahajan/model-router/internal/discovery"
)

type catalogScript struct {
	mu   sync.Mutex
	hits int64
	mode string // "ok", "fail", "empty"
}

func (c *catalogScript) handler(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.hits++
	mode := c.mode
	c.mu.Unlock()
	// Trusted header assertion: the catalog token must arrive, and no
	// provider credential may ride along.
	if r.Header.Get("X-Catalog-Token") != "trusted" {
		w.WriteHeader(403)
		return
	}
	if r.Header.Get("Authorization") != "" {
		w.WriteHeader(403)
		return
	}
	switch mode {
	case "fail":
		w.WriteHeader(500)
	case "empty":
		_, _ = w.Write([]byte(`[]`))
	default:
		_, _ = w.Write([]byte(`[{"id": "cat-a"}, {"id": "ghost"}]`))
	}
}

func (c *catalogScript) setMode(m string) {
	c.mu.Lock()
	c.mode = m
	c.mu.Unlock()
}

func (c *catalogScript) hitCount() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits
}

func writeCatalogCfg(t *testing.T, catalogURL string, extra string) string {
	t.Helper()
	body := `
[server]
auth_mode = "none"
allow_dynamic_routes = false
max_generations = 8
[retry]
max_attempts = 1
[providers.up]
dialect = "anthropic"
base_url = "http://127.0.0.1:9"
api_key = "upstream-secret"
[catalog]
url = "` + catalogURL + `"
timeout = 5
refresh_seconds = 60
headers = { "X-Catalog-Token" = "trusted" }
model_providers = { "cat-a" = "up", "cat-b" = "up" }
` + extra
	p := filepath.Join(t.TempDir(), "router.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func anthropicOK(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m",` +
		`"content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn",` +
		`"usage":{"input_tokens":7,"output_tokens":3}}`))
}

// newCatalogHarness wires a router over the scripted catalog plus the
// given upstream handler, and performs the startup refresh.
func newCatalogHarness(t *testing.T, script *catalogScript, upstream func(w http.ResponseWriter, r *http.Request)) (*Server, *discovery.Worker) {
	t.Helper()
	catSrv := httptest.NewServer(http.HandlerFunc(script.handler))
	t.Cleanup(catSrv.Close)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if upstream != nil {
			upstream(w, r)
			return
		}
		anthropicOK(w, r)
	}))
	t.Cleanup(up.Close)
	cfg, err := config.Load(writeCatalogCfg(t, catSrv.URL, ""))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Providers["up"].BaseURL = up.URL
	srv := New(cfg)
	worker := discovery.NewCatalogWorker(cfg.Catalog, cfg.Providers, srv.Registry())
	worker.RefreshOnce(t.Context())
	t.Cleanup(worker.Stop)
	return srv, worker
}

func TestCatalogRouteServesGeneration(t *testing.T) {
	srv, _ := newCatalogHarness(t, &catalogScript{}, nil)
	rs := httptest.NewServer(srv.Handler())
	defer rs.Close()
	status, _, data := post(t, rs.URL+"/v1/chat/completions",
		`{"model":"cat-a","messages":[{"role":"user","content":"Hi"}],"max_tokens":4}`, nil)
	if status != 200 {
		t.Fatalf("catalog route must serve: %d %s", status, data)
	}
}

func TestCatalogUnmapped404(t *testing.T) {
	srv, _ := newCatalogHarness(t, &catalogScript{}, nil)
	// Direct resolve mapping keeps the typed fault covered in-unit.
	if _, _, rerr := srv.resolveModel("ghost"); rerr == nil || rerr.Code != "model_endpoint_unknown" {
		t.Fatalf("unmapped must be model_endpoint_unknown, got %+v", rerr)
	}
	rs := httptest.NewServer(srv.Handler())
	defer rs.Close()
	for _, tc := range []struct{ path, body string }{
		{"/v1/chat/completions", `{"model":"ghost","messages":[{"role":"user","content":"Hi"}],"max_tokens":4}`},
		{"/v1/responses", `{"model":"ghost","input":[{"role":"user","content":[{"type":"input_text","text":"Hi"}]}]}`},
	} {
		status, _, data := post(t, rs.URL+tc.path, tc.body, nil)
		if status != 404 || !strings.Contains(string(data), "model_endpoint_unknown") {
			t.Fatalf("%s: unmapped must be 404 model_endpoint_unknown, got %d %s", tc.path, status, data)
		}
		if !strings.Contains(string(data), "catalog.model_providers") {
			t.Fatalf("%s: operator hint missing: %s", tc.path, data)
		}
	}
}

func TestCatalogStaleServedOnRefreshFailure(t *testing.T) {
	script := &catalogScript{}
	srv, worker := newCatalogHarness(t, script, nil)
	rs := httptest.NewServer(srv.Handler())
	defer rs.Close()
	script.setMode("fail")
	worker.RefreshOnce(t.Context())
	if worker.Failures() == 0 {
		t.Fatal("failure must be recorded")
	}
	// Stale routes still serve; read paths keep working with no I/O.
	status, _, data := post(t, rs.URL+"/v1/chat/completions",
		`{"model":"cat-a","messages":[{"role":"user","content":"Hi"}],"max_tokens":4}`, nil)
	if status != 200 {
		t.Fatalf("stale catalog must serve: %d %s", status, data)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != 200 {
		t.Fatalf("readyz must ignore staleness: %d", rec.Code)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	modelsRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(modelsRec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if err := json.Unmarshal(modelsRec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != 1 || out.Data[0].ID != "cat-a" {
		t.Fatalf("stale listing wrong: %s", modelsRec.Body.String())
	}
}

func TestCatalogValidEmptyReplaces(t *testing.T) {
	script := &catalogScript{}
	srv, worker := newCatalogHarness(t, script, nil)
	rs := httptest.NewServer(srv.Handler())
	defer rs.Close()
	script.setMode("empty")
	worker.RefreshOnce(t.Context())
	status, _, data := post(t, rs.URL+"/v1/chat/completions",
		`{"model":"cat-a","messages":[{"role":"user","content":"Hi"}],"max_tokens":4}`, nil)
	if status != 404 {
		t.Fatalf("empty catalog must remove routes, got %d %s", status, data)
	}
}

func TestCatalogReadPathsDoNoIO(t *testing.T) {
	script := &catalogScript{}
	srv, _ := newCatalogHarness(t, script, nil)
	before := script.hitCount()
	for _, path := range []string{"/livez", "/readyz", "/v1/models"} {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != 200 {
			t.Fatalf("%s: %d", path, rec.Code)
		}
	}
	if got := script.hitCount(); got != before {
		t.Fatalf("read paths must not dial out: hits %d -> %d", before, got)
	}
}

func TestCatalogRequestPinsOldRouteAcrossPublish(t *testing.T) {
	script := &catalogScript{}
	started := make(chan struct{})
	var startOnce sync.Once
	release := make(chan struct{})
	var upstreamSaw atomic.Value
	upstreamSaw.Store("")
	upHandler := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &probe)
		if probe.Model == "cat-a" {
			startOnce.Do(func() { close(started) })
			<-release
		}
		upstreamSaw.Store(probe.Model)
		anthropicOK(w, r)
	}
	srv, _ := newCatalogHarness(t, script, upHandler)
	rs := httptest.NewServer(srv.Handler())
	defer rs.Close()
	type result struct {
		status int
		body   []byte
	}
	first := make(chan result, 1)
	go func() {
		status, _, data := post(t, rs.URL+"/v1/chat/completions",
			`{"model":"cat-a","messages":[{"role":"user","content":"Hi"}],"max_tokens":4}`, nil)
		first <- result{status, data}
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("upstream never saw the first generation")
	}
	// Publish a snapshot that drops cat-a while its generation is open.
	if err := commitCatalogShim(srv.Registry(), nil, nil, 1000); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case res := <-first:
		if res.status != 200 {
			t.Fatalf("pinned request must complete with the old route: %d %s", res.status, res.body)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("pinned request never completed")
	}
	if upstreamSaw.Load().(string) != "cat-a" {
		t.Fatalf("upstream must see the pinned model, got %q", upstreamSaw.Load())
	}
	// Subsequent requests observe the new snapshot.
	status, _, data := post(t, rs.URL+"/v1/chat/completions",
		`{"model":"cat-a","messages":[{"role":"user","content":"Hi"}],"max_tokens":4}`, nil)
	if status != 404 {
		t.Fatalf("post-publish request must miss, got %d %s", status, data)
	}
}

func TestCatalogModelsOrderExplicitThenFetch(t *testing.T) {
	script := &catalogScript{}
	catSrv := httptest.NewServer(http.HandlerFunc(script.handler))
	defer catSrv.Close()
	up := httptest.NewServer(http.HandlerFunc(anthropicOK))
	defer up.Close()
	cfg, err := config.Load(writeCatalogCfg(t, catSrv.URL, `
[[models]]
name = "explicit-zed"
provider = "up"
model = "z"
`))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Providers["up"].BaseURL = up.URL
	srv := New(cfg)
	worker := discovery.NewCatalogWorker(cfg.Catalog, cfg.Providers, srv.Registry())
	worker.RefreshOnce(t.Context())
	defer worker.Stop()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != 2 || out.Data[0].ID != "explicit-zed" || out.Data[1].ID != "cat-a" {
		t.Fatalf("explicit-then-catalog order: %s", rec.Body.String())
	}
}
