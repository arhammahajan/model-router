// Llama worker: probe order, failure semantics, row mapping trust,
// activation/stale retention, header/credential isolation, fixed fleet
// concurrency bounds and stop-cancels-probe.
package discovery

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/router"
)

type llamaProbe struct {
	mu      sync.Mutex
	paths   []string
	auth    []string
	globals map[string][]string
	health  int
	models  string
	props   string
	propsOn bool
}

func (f *llamaProbe) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path+"?"+r.URL.RawQuery)
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		if f.globals == nil {
			f.globals = map[string][]string{}
		}
		for k := range r.Header {
			f.globals[k] = append(f.globals[k], r.URL.Path)
		}
		health, models, props := f.health, f.models, f.props
		f.mu.Unlock()
		switch r.URL.Path {
		case "/health":
			if health == 0 {
				health = 200
			}
			w.WriteHeader(health)
		case "/v1/models":
			if models == "" {
				models = `{"data": [{"id": "m1"}]}`
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(models))
		case "/props":
			if !f.propsOn {
				w.WriteHeader(500)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(props))
		default:
			w.WriteHeader(404)
		}
	}
}

func (f *llamaProbe) orderedPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...)
}

func llamaWorkerCfg(url, name string) *config.Config {
	return &config.Config{
		Server:    config.Server{AllowDynamicRoutes: true},
		Providers: map[string]*config.Provider{},
		Llama: &config.LlamaConfig{
			Servers: []config.LlamaServer{
				{URL: url, Name: name, Headers: map[string]string{}},
			},
			ProbeTimeout:  5,
			ExposeAliases: true,
			Compatibility: "strict",
		},
	}
}

func refreshLlama(t *testing.T, cfg *config.Config) (*LlamaWorker, *router.Registry) {
	t.Helper()
	reg := router.NewRegistry(cfg)
	w := NewLlamaWorker(cfg.Llama, reg)
	w.RefreshOnce(context.Background())
	return w, reg
}

func TestLlamaProbeOrder(t *testing.T) {
	f := &llamaProbe{propsOn: true, props: `{"a":1}`}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	_, reg := refreshLlama(t, llamaWorkerCfg(srv.URL, "llama-a"))
	if _, _, fault := reg.Current().Resolve("m1"); fault != router.Found {
		t.Fatalf("row must publish: %v", fault)
	}
	got := f.orderedPaths()
	want := []string{"/health?", "/v1/models?", "/props?autoload=false"}
	if len(got) != len(want) {
		t.Fatalf("probe order=%q want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("probe order=%q want %q", got, want)
		}
	}
}

func TestLlamaHealthAndModelsFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		health int
		models string
	}{
		{"health-400", 400, ""},
		{"health-500", 500, ""},
		{"models-3xx-means-non2xx", 0, ""}, // placeholder, replaced below
		{"models-bad-json", 0, `{{{`},
		{"models-non-list", 0, `{"data": {"id": "m1"}}`},
		{"models-no-list", 0, `{"other": []}`},
		{"models-bare-object", 0, `{"id": "m1"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/health":
					w.WriteHeader(tc.health)
				case "/v1/models":
					if tc.name == "models-3xx-means-non2xx" {
						w.Header().Set("Location", "/elsewhere")
						w.WriteHeader(301)
						return
					}
					body := tc.models
					if body == "" {
						body = `{"data": [{"id": "m1"}]}`
					}
					_, _ = w.Write([]byte(body))
				default:
					w.WriteHeader(500)
				}
			}))
			defer srv.Close()
			w, reg := refreshLlama(t, llamaWorkerCfg(srv.URL, "llama-a"))
			if w.Failures() == 0 {
				t.Fatal("failure must count")
			}
			if _, _, fault := reg.Current().Resolve("m1"); fault == router.Found {
				t.Fatal("failed probe must publish nothing")
			}
			if _, _, fault := reg.Current().Resolve("llama-a/m1"); fault != router.NotFound {
				t.Fatalf("initial failure leaves dynamic disabled: %v", fault)
			}
		})
	}
	// Health 3xx is not followed and is not itself a failure.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.Header().Set("Location", "/elsewhere")
			w.WriteHeader(302)
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data": [{"id": "m1"}]}`))
		default:
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	_, reg := refreshLlama(t, llamaWorkerCfg(srv.URL, "llama-a"))
	if _, _, fault := reg.Current().Resolve("m1"); fault != router.Found {
		t.Fatalf("health 3xx must not fail: %v", fault)
	}
}

func TestLlamaAllStatusesPublish(t *testing.T) {
	f := &llamaProbe{models: `{"data": [
		{"id": "loaded", "status": {"value": "loaded"}},
		{"id": "sleeping", "status": {"value": "sleeping"}},
		{"id": "weird", "status": {"value": "bogus"}},
		{"id": "nostatus"},
		{"id": "  "},
		{"id": 7},
		"bare-string",
		7,
		{"noid": true}
	]}`}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	_, reg := refreshLlama(t, llamaWorkerCfg(srv.URL, "llama-a"))
	for _, name := range []string{"loaded", "sleeping", "weird", "nostatus"} {
		if _, _, fault := reg.Current().Resolve(name); fault != router.Found {
			t.Fatalf("%q must publish regardless of status: %v", name, fault)
		}
	}
	// Non-mapping rows and blank/non-string IDs never publish.
	for _, name := range []string{"bare-string", "7"} {
		if _, _, fault := reg.Current().Resolve(name); fault != router.NotFound {
			t.Fatalf("%q must not publish: %v", name, fault)
		}
	}
}

func TestLlamaPropsOptional(t *testing.T) {
	// Props 500 keeps the attempt alive with {}.
	f := &llamaProbe{}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	w, reg := refreshLlama(t, llamaWorkerCfg(srv.URL, "llama-a"))
	if w.Failures() != 0 {
		t.Fatalf("props failure must not fail: %d", w.Failures())
	}
	if _, _, fault := reg.Current().Resolve("m1"); fault != router.Found {
		t.Fatal("models must still publish")
	}
	// Non-object props are also {}.
	f2 := &llamaProbe{propsOn: true, props: `[1,2]`}
	srv2 := httptest.NewServer(f2.handler())
	defer srv2.Close()
	w2, _ := refreshLlama(t, llamaWorkerCfg(srv2.URL, "llama-a"))
	if w2.Failures() != 0 {
		t.Fatalf("non-object props must not fail: %d", w2.Failures())
	}
}

func TestLlamaAliasesLocalPrefix(t *testing.T) {
	f := &llamaProbe{models: `{"data": [
		{"id": "local", "meta": {"llamaswap": {"aliases": ["swap-a", 7, "  "]}}},
		{"id": "m1", "aliases": "bare"},
		{"id": "m2", "aliases": ["row-a"], "meta": {"llamaswap": {"aliases": "notalist"}}},
		{"id": "m3", "aliases": ["row-b"], "meta": {"llamaswap": {"aliases": ["swap-wins"]}}}
	]}`}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	cfg := llamaWorkerCfg(srv.URL, "llama-a")
	cfg.Llama.Prefix = "p-"
	_, reg := refreshLlama(t, cfg)
	for _, name := range []string{"p-local-llama", "p-swap-a", "p-m1", "p-bare", "p-m2", "p-row-a", "p-m3", "p-swap-wins"} {
		if _, _, fault := reg.Current().Resolve(name); fault != router.Found {
			t.Fatalf("%q must resolve: %v", name, fault)
		}
	}
	// Non-string aliases are skipped, never coerced.
	if _, _, fault := reg.Current().Resolve("p-7"); fault != router.NotFound {
		t.Fatalf("numeric alias must not publish: %v", fault)
	}
	m, _, _ := reg.Current().Resolve("p-swap-a")
	if m.Name != "p-local-llama" {
		t.Fatalf("alias must point at canonical: %+v", m)
	}
	if got := len(reg.Current().Models()); got != 4 {
		t.Fatalf("aliases add no rows: %d", got)
	}
	// Metadata is the verbatim row copy with exact numbers.
	f3 := &llamaProbe{models: `{"data": [{"id": "m1", "metadata": {"n": 1234567890123456789}}]}`}
	srv3 := httptest.NewServer(f3.handler())
	defer srv3.Close()
	_, reg3 := refreshLlama(t, llamaWorkerCfg(srv3.URL, "llama-a"))
	m3, _, _ := reg3.Current().Resolve("m1")
	if !strings.Contains(string(m3.Metadata), "1234567890123456789") {
		t.Fatalf("verbatim metadata lost: %s", m3.Metadata)
	}
}

func TestLlamaCredentialHeaderIsolation(t *testing.T) {
	var gotAuth, gotGlobal, gotSrv, gotAgent atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			gotAuth.Store(r.Header.Get("Authorization"))
			gotGlobal.Store(r.Header.Get("X-Llama-Global"))
			gotSrv.Store(r.Header.Get("X-Llama-Srv"))
			gotAgent.Store(r.Header.Get("User-Agent"))
		}
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(200)
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data": []}`))
		default:
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	cfg := llamaWorkerCfg(srv.URL, "llama-a")
	cfg.Llama.Headers = map[string]string{"X-Llama-Global": "g", "Authorization": "Bogus static"}
	cfg.Llama.Servers[0].Headers = map[string]string{"X-Llama-Srv": "s"}
	cfg.Llama.Servers[0].APIKey = "  server-key "
	refreshLlama(t, cfg)
	if got, _ := gotAuth.Load().(string); got != "Bearer server-key" {
		t.Fatalf("selected credential must win as Bearer: %q", got)
	}
	if got, _ := gotGlobal.Load().(string); got != "g" {
		t.Fatalf("global header missing: %q", got)
	}
	if got, _ := gotSrv.Load().(string); got != "s" {
		t.Fatalf("server header missing: %q", got)
	}
	if got, _ := gotAgent.Load().(string); got == "" {
		t.Fatal("User-Agent must be set")
	}
	// Empty key emits no placeholder.
	var noAuth atomic.Value
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			noAuth.Store(r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(200)
		case "/v1/models":
			_, _ = w.Write([]byte(`[]`))
		default:
			w.WriteHeader(500)
		}
	}))
	defer srv2.Close()
	refreshLlama(t, llamaWorkerCfg(srv2.URL, "llama-a"))
	if got, _ := noAuth.Load().(string); got != "" {
		t.Fatalf("empty key must emit no auth: %q", got)
	}
}

func TestLlamaOversizeAndEncoding(t *testing.T) {
	// Oversized models body fails the server (4 MiB bound).
	big := `{"data": [` + strings.Repeat(`{"id":"m"},`, 400000) + `{"id":"z"}]}`
	f := &llamaProbe{models: big}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	w, _ := refreshLlama(t, llamaWorkerCfg(srv.URL, "llama-a"))
	if w.Failures() == 0 {
		t.Fatal("oversize body must fail")
	}
	// Unknown content encoding fails the server.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(200)
		case "/v1/models":
			w.Header().Set("Content-Encoding", "br")
			_, _ = w.Write([]byte(`[]`))
		default:
			w.WriteHeader(500)
		}
	}))
	defer srv2.Close()
	w2, _ := refreshLlama(t, llamaWorkerCfg(srv2.URL, "llama-a"))
	if w2.Failures() == 0 {
		t.Fatal("bad encoding must fail")
	}
}

func TestLlamaStaleRetentionAndEmptyActivation(t *testing.T) {
	var mode atomic.Int32 // 0 ok, 1 fail, 2 empty
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			if mode.Load() == 1 {
				w.WriteHeader(500)
				return
			}
			w.WriteHeader(200)
		case "/v1/models":
			if mode.Load() == 1 {
				w.WriteHeader(500)
				return
			}
			if mode.Load() == 2 {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`{"data": [{"id": "m1"}]}`))
		default:
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	cfg := llamaWorkerCfg(srv.URL, "llama-a")
	reg := router.NewRegistry(cfg)
	w := NewLlamaWorker(cfg.Llama, reg)
	w.RefreshOnce(context.Background())
	if _, _, fault := reg.Current().Resolve("m1"); fault != router.Found {
		t.Fatal("initial success must publish")
	}
	mode.Store(1)
	w.RefreshOnce(context.Background())
	snap := reg.Current()
	if _, _, fault := snap.Resolve("m1"); fault != router.Found {
		t.Fatal("failure must retain last good routes")
	}
	if snap.UsableCount() != 0 || snap.Ready() {
		t.Fatal("failure must drop readiness")
	}
	// Valid empty replaces: provider stays active with zero routes.
	mode.Store(2)
	w.RefreshOnce(context.Background())
	if got := reg.Current().UsableCount(); got != 0 {
		t.Fatalf("empty success has no routes: %d", got)
	}
	if _, _, fault := reg.Current().Resolve("llama-a/fresh"); fault != router.Found {
		t.Fatalf("empty success keeps dynamic active: %v", fault)
	}
}

func TestLlamaFixedPoolCoversAllWaves(t *testing.T) {
	// Ten servers over an 8-worker pool: every server is probed and
	// published; slow early servers never cancel later waves.
	srvs := make([]*httptest.Server, 0, 10)
	for i := 0; i < 10; i++ {
		i := i
		srvs = append(srvs, httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/health":
				if i < 8 {
					time.Sleep(300 * time.Millisecond)
				}
				w.WriteHeader(200)
			case "/v1/models":
				_, _ = w.Write([]byte(`{"data": [{"id": "m` + string(rune('0'+i/10)) + string(rune('0'+i%10)) + `"}]}`))
			default:
				w.WriteHeader(500)
			}
		})))
	}
	defer func() {
		for _, s := range srvs {
			s.Close()
		}
	}()
	cfg := &config.Config{
		Server:    config.Server{AllowDynamicRoutes: true},
		Providers: map[string]*config.Provider{},
		Llama:     &config.LlamaConfig{ProbeTimeout: 10, Compatibility: "strict"},
	}
	for i, s := range srvs {
		cfg.Llama.Servers = append(cfg.Llama.Servers, config.LlamaServer{
			URL: s.URL, Name: "llama-" + string(rune('a'+i/10)) + string(rune('0'+i%10)),
		})
	}
	reg := router.NewRegistry(cfg)
	w := NewLlamaWorker(cfg.Llama, reg)
	w.RefreshOnce(context.Background())
	if w.Failures() != 0 {
		t.Fatalf("all waves must succeed: %d %q", w.Failures(), w.LastError())
	}
	if got := reg.Current().UsableCount(); got != 10 {
		t.Fatalf("usable=%d want 10", got)
	}
}

func TestLlamaFleetBoundWaves(t *testing.T) {
	one := &config.LlamaConfig{
		Servers:      []config.LlamaServer{{URL: "http://127.0.0.1:1", Name: "a"}},
		ProbeTimeout: 2,
	}
	if got := LlamaFleetBound(one); got != 3*time.Second {
		t.Fatalf("single server bound=%v", got)
	}
	many := &config.LlamaConfig{ProbeTimeout: 2}
	for i := 0; i < 10; i++ {
		many.Servers = append(many.Servers, config.LlamaServer{URL: "http://127.0.0.1:1", Name: "a"})
	}
	if got := LlamaFleetBound(many); got != 5*time.Second {
		t.Fatalf("10 servers over 8 workers need 2 waves: %v", got)
	}
}

func TestLlamaStopCancelsInflight(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			<-release
			return
		}
		w.WriteHeader(500)
	}))
	defer srv.Close()
	defer close(release)
	cfg := llamaWorkerCfg(srv.URL, "llama-a")
	reg := router.NewRegistry(cfg)
	w := NewLlamaWorker(cfg.Llama, reg)
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.RefreshOnce(context.Background())
	}()
	time.Sleep(200 * time.Millisecond)
	w.Stop()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RefreshOnce did not return after Stop")
	}
	// No publish may race shutdown: the pointer still pins the seed.
	if _, _, fault := reg.Current().Resolve("llama-a/m"); fault != router.NotFound {
		t.Fatalf("stopped refresh must publish nothing: %v", fault)
	}
	w.Stop() // idempotent
}

func TestLlamaWorkerStartStopGuards(t *testing.T) {
	cfg := llamaWorkerCfg("http://127.0.0.1:9", "llama-a")
	cfg.Llama.RefreshSeconds = 0
	reg := router.NewRegistry(cfg)
	w := NewLlamaWorker(cfg.Llama, reg)
	w.Start(context.Background()) // refresh 0: no loop
	w.Stop()
	w.Stop()
	// Started loops join on Stop; a second Start never respawns.
	cfg.Llama.RefreshSeconds = 60
	w2 := NewLlamaWorker(cfg.Llama, reg)
	ctx, cancel := context.WithCancel(context.Background())
	w2.Start(ctx)
	w2.Start(ctx)
	cancel()
	w2.Stop()
}

func TestLlamaFailureBackoffCappedAtRefresh(t *testing.T) {
	cases := []struct {
		failures int
		interval float64
		want     float64
	}{
		{1, 30, 5},
		{2, 30, 10},
		{3, 30, 20},
		{4, 30, 30},
		{200, 30, 30},
		{10, 600, 300},
		{10, 0, 300},
	}
	for _, tc := range cases {
		if got := llamaFailureBackoff(tc.failures, tc.interval); got != tc.want {
			t.Errorf("llamaFailureBackoff(%d, %v) = %v, want %v", tc.failures, tc.interval, got, tc.want)
		}
	}
}
