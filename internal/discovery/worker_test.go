// Worker state machine: success/failure/stale/empty retention,
// refresh-0 single attempts, try-lock overlap, backoff math and
// idempotent stop.
package discovery

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/router"
)

func workerCfg(t *testing.T, url string) (*config.Config, map[string]*config.Provider) {
	t.Helper()
	providers := map[string]*config.Provider{
		"up": {Name: "up", DefaultDialect: config.DialectAnthropic, BaseURL: "http://127.0.0.1:9"},
	}
	cfg := &config.Config{
		Server:    config.Server{AllowDynamicRoutes: false},
		Providers: providers,
		Catalog: &config.Catalog{
			URL:            url,
			Headers:        map[string]string{},
			Timeout:        5,
			RefreshSeconds: 60,
			ModelProviders: map[string]string{"m1": "up", "m2": "up"},
		},
	}
	return cfg, providers
}

func TestRefreshSuccessFailureStaleEmpty(t *testing.T) {
	var mode atomic.Int32 // 0 ok, 1 fail, 2 empty
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load() {
		case 1:
			w.WriteHeader(500)
			return
		case 2:
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_, _ = w.Write([]byte(`[{"id": "m1"}, {"id": "ghost"}]`))
	}))
	defer srv.Close()
	cfg, providers := workerCfg(t, srv.URL)
	reg := router.NewRegistry(cfg)
	w := NewCatalogWorker(cfg.Catalog, providers, reg)

	w.RefreshOnce(context.Background())
	if w.Failures() != 0 || w.LastError() != "" {
		t.Fatalf("success must clear failures: %d %q", w.Failures(), w.LastError())
	}
	if _, _, fault := reg.Current().Resolve("m1"); fault != router.Found {
		t.Fatal("m1 must route after success")
	}
	if _, _, fault := reg.Current().Resolve("ghost"); fault != router.UnmappedCatalog {
		t.Fatalf("ghost must be unmapped, got %v", fault)
	}

	mode.Store(1)
	w.RefreshOnce(context.Background())
	if w.Failures() != 1 || w.LastError() == "" {
		t.Fatalf("failure must count with static reason: %d %q", w.Failures(), w.LastError())
	}
	// Stale retained: previous routes still resolve, readiness kept.
	if _, _, fault := reg.Current().Resolve("m1"); fault != router.Found {
		t.Fatal("stale m1 must survive failure")
	}
	if !reg.Current().Ready() {
		t.Fatal("readiness must ignore catalog staleness")
	}

	mode.Store(2)
	w.RefreshOnce(context.Background())
	if w.Failures() != 0 {
		t.Fatalf("empty valid must clear failures: %d", w.Failures())
	}
	if _, _, fault := reg.Current().Resolve("m1"); fault == router.Found {
		t.Fatal("valid empty must replace: m1 must be gone")
	}
	if reg.Current().Ready() {
		t.Fatal("empty inventory must be unready (no explicit routes)")
	}
}

func TestRefreshZeroExactlyOneAttempt(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`[{"id": "m1"}]`))
	}))
	defer srv.Close()
	cfg, providers := workerCfg(t, srv.URL)
	cfg.Catalog.RefreshSeconds = 0
	reg := router.NewRegistry(cfg)
	w := NewCatalogWorker(cfg.Catalog, providers, reg)

	w.RefreshOnce(context.Background())
	if hits.Load() != 1 {
		t.Fatalf("startup must be exactly one attempt: %d", hits.Load())
	}
	// Scheduling disabled at 0: Start is a no-op and nothing follows.
	w.Start(context.Background())
	time.Sleep(150 * time.Millisecond)
	if hits.Load() != 1 {
		t.Fatalf("refresh 0 must schedule nothing: %d", hits.Load())
	}
	w.Stop()
	if hits.Load() != 1 {
		t.Fatalf("stop must not attempt: %d", hits.Load())
	}
}

func TestRefreshOverlapReturnsImmediately(t *testing.T) {
	release := make(chan struct{})
	var entered atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered.Add(1)
		<-release
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	cfg, providers := workerCfg(t, srv.URL)
	reg := router.NewRegistry(cfg)
	w := NewCatalogWorker(cfg.Catalog, providers, reg)

	done := make(chan struct{})
	go func() { w.RefreshOnce(context.Background()); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for entered.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if entered.Load() == 0 {
		t.Fatal("first attempt never started")
	}
	// Overlapping attempt must return immediately, not queue behind.
	start := time.Now()
	w.RefreshOnce(context.Background())
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("overlapping refresh must return immediately")
	}
	close(release)
	<-done
}

func TestBackoffCeiling(t *testing.T) {
	cases := []struct {
		n    int
		want float64
	}{
		{1, 5}, {2, 10}, {3, 20}, {4, 40}, {7, 300}, {8, 300}, {100, 300},
	}
	for _, tc := range cases {
		if got := BackoffCeiling(tc.n); got != tc.want {
			t.Fatalf("n=%d: got %v want %v", tc.n, got, tc.want)
		}
	}
}

func TestStopIdempotentAndReusable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	cfg, providers := workerCfg(t, srv.URL)
	cfg.Catalog.RefreshSeconds = 3600
	reg := router.NewRegistry(cfg)
	w := NewCatalogWorker(cfg.Catalog, providers, reg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)
	w.Start(ctx) // second start while running is a no-op
	w.Stop()
	w.Stop() // idempotent
	// Reuse after stop: a manual attempt still works (stop only ends
	// scheduling); starting again after stop is refused, never panics.
	w.RefreshOnce(context.Background())
	w.Start(ctx)
}

func TestConcurrentRefreshAndPublishRace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id": "m1"}]`))
	}))
	defer srv.Close()
	cfg, providers := workerCfg(t, srv.URL)
	reg := router.NewRegistry(cfg)
	w := NewCatalogWorker(cfg.Catalog, providers, reg)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.RefreshOnce(context.Background())
			snap := reg.Current()
			snap.Resolve("m1")
			snap.Models()
		}()
	}
	wg.Wait()
	w.Stop()
}
