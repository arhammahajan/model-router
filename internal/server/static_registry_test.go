package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/arhammahajan/model-router/internal/config"
)

func writeStaticCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "router.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const staticBase = `
[server]
auth_mode = "none"
allow_dynamic_routes = true
[retry]
max_attempts = 1
[providers.up]
dialect = "anthropic"
base_url = "http://127.0.0.1:9"
api_key = "upstream-secret"
`

func TestDisabledAliasCannotBypassViaDynamic(t *testing.T) {
	p := writeStaticCfg(t, staticBase+`
[[models]]
name = "real"
provider = "up"
model = "x"
[[models]]
name = "off"
provider = "up"
model = "y"
enabled = false
aliases = ["off-alias", "up/off-alias"]
`)
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	var upstreamCalls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer up.Close()
	for _, prov := range cfg.Providers {
		prov.BaseURL = up.URL
	}
	srv := httptest.NewServer(New(cfg).Handler())
	defer srv.Close()
	for _, model := range []string{"off", "off-alias", "up/off-alias"} {
		status, _, data := post(t, srv.URL+"/v1/chat/completions",
			`{"model":"`+model+`","messages":[{"role":"user","content":"Hi"}],"max_tokens":4}`, nil)
		if status != 404 || !strings.Contains(string(data), "model_not_found") {
			t.Fatalf("disabled %q must be 404 model_not_found, got %d %s", model, status, data)
		}
	}
	if got := upstreamCalls.Load(); got != 0 {
		t.Fatalf("disabled refusals must not touch upstream, got %d calls", got)
	}
	// Enabled literal still routes.
	status, _, _ := post(t, srv.URL+"/v1/chat/completions",
		`{"model":"real","messages":[{"role":"user","content":"Hi"}],"max_tokens":4}`, nil)
	if status == 404 {
		t.Fatalf("enabled literal must still route (got 404)")
	}
}

func TestModelsAuthFilteringOrderMetadata(t *testing.T) {
	p := writeStaticCfg(t, `
[server]
auth_mode = "required"
api_keys = ["client-secret"]
allow_dynamic_routes = true
[retry]
max_attempts = 1
[providers.up]
dialect = "anthropic"
base_url = "http://127.0.0.1:9"
api_key = "x"
[[models]]
name = "first"
provider = "up"
model = "up-1"
[models.metadata]
display_name = "First"
context_window = 200000
[[models]]
name = "off"
provider = "up"
model = "up-off"
enabled = false
[[models]]
name = "second"
provider = "up"
model = "up-2"
`)
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	s := New(cfg)
	// Unauthenticated listing must be refused with zero upstream I/O (no upstream exists).
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("unauthenticated listing must be 401, got %d %s", rec.Code, rec.Body.String())
	}
	// Authenticated listing: enabled-only, config order, exact metadata.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer client-secret")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Object string `json:"object"`
		Data   []struct {
			ID       string          `json:"id"`
			Object   string          `json:"object"`
			OwnedBy  string          `json:"owned_by"`
			Metadata json.RawMessage `json:"metadata"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Object != "list" || len(out.Data) != 2 {
		t.Fatalf("enabled-only listing wrong: %s", rec.Body.String())
	}
	if out.Data[0].ID != "first" || out.Data[1].ID != "second" {
		t.Fatalf("order must follow config: %s", rec.Body.String())
	}
	if out.Data[0].Object != "model" || out.Data[0].OwnedBy != "model-router" {
		t.Fatalf("minimal shape changed: %+v", out.Data[0])
	}
	meta := string(out.Data[0].Metadata)
	if !strings.Contains(meta, `"First"`) || !strings.Contains(meta, "200000") {
		t.Fatalf("metadata lost: %s", meta)
	}
	if strings.Contains(meta, "200000.0") {
		t.Fatalf("numeric fidelity: %s", meta)
	}
	var absent map[string]json.RawMessage
	if err := json.Unmarshal(out.Data[1].Metadata, &absent); err != nil {
		t.Fatal(err)
	}
	if len(absent) != 1 || absent["router"] == nil {
		t.Fatalf("absent metadata must carry only router: %s", out.Data[1].Metadata)
	}
	// Query-key auth must not apply to the Chat listing surface.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/models?key=client-secret", nil)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("listing ?key= must not authenticate, got %d", rec.Code)
	}
}

func TestReadinessUsableNamedRoutes(t *testing.T) {
	cases := []struct {
		name   string
		models string
		ready  int
	}{
		{"all disabled unready", "[[models]]\nname = \"a\"\nprovider = \"up\"\nmodel = \"x\"\nenabled = false\n", 503},
		{"one enabled ready", "[[models]]\nname = \"a\"\nprovider = \"up\"\nmodel = \"x\"\n", 200},
		{"dynamic only unready", "", 503},
	}
	for _, tc := range cases {
		p := writeStaticCfg(t, staticBase+tc.models)
		cfg, err := config.Load(p)
		if err != nil {
			t.Fatal(err)
		}
		s := New(cfg)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != tc.ready {
			t.Fatalf("%s: readyz=%d want %d body=%s", tc.name, rec.Code, tc.ready, rec.Body.String())
		}
		// /livez stays open and independent.
		rec = httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/livez", nil))
		if rec.Code != 200 {
			t.Fatalf("%s: livez must stay 200", tc.name)
		}
	}
}

func TestSnapshotPinnedAgainstPostNewMutation(t *testing.T) {
	p := writeStaticCfg(t, staticBase+`
[[models]]
name = "a"
provider = "up"
model = "x"
`)
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	s := New(cfg)
	cfg.Models[0].Name = "mutated"
	cfg.Providers["up"].BaseURL = "http://127.0.0.1:1"
	if _, _, rerr := s.resolveModel("a"); rerr != nil {
		t.Fatalf("snapshot must be pinned, got %v", rerr)
	}
}

func TestStaticReadPathRace(t *testing.T) {
	p := writeStaticCfg(t, staticBase+`
[[models]]
name = "a"
provider = "up"
model = "x"
[models.metadata]
k = "v"
`)
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	s := New(cfg)
	h := s.Handler()
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = s.resolveModel("a")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
			rec2 := httptest.NewRecorder()
			h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		}()
	}
	wg.Wait()
}
