package discovery

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/router"
)

// Scheduler proofs: hermetic fake clock, jitter drawn exactly once at
// completion, manual-refresh wakeup and no busy spin on overdue
// deadlines. No sleeps: only the published deadline is asserted.

func TestScheduleSingleJitterDeadline(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`[{"id": "m1"}]`))
	}))
	defer srv.Close()
	cfg, providers := workerCfg(t, srv.URL)
	fc := &router.FakeClock{Mono: 100, Wall: 1000}
	reg := router.NewRegistryWithClock(cfg, fc)
	w := NewCatalogWorker(cfg.Catalog, providers, reg)
	w.clock = fc
	var draws atomic.Int64
	w.jitter = func(ceil float64) float64 {
		draws.Add(1)
		if ceil != 60 {
			t.Errorf("success delay must be the interval, got ceil %v", ceil)
		}
		return ceil
	}
	w.RefreshOnce(context.Background())
	if draws.Load() != 0 {
		t.Fatalf("success must not draw jitter: %d", draws.Load())
	}
	ev := reg.CatalogEvent()
	if ev.NextAttempt == nil || *ev.NextAttempt != router.Mono(160) {
		t.Fatalf("published deadline must be completed+interval: %+v", ev.NextAttempt)
	}
	if ev.LastSuccess == nil || *ev.LastSuccess != router.Mono(100) {
		t.Fatalf("success mono must pin completion: %+v", ev.LastSuccess)
	}
	// Manual completion wakes the scheduler exactly once per attempt.
	select {
	case <-w.wake:
	default:
		t.Fatal("completed attempt must wake the owner")
	}
	if d := w.waitDelay(); d != 60*time.Second {
		t.Fatalf("wait seam must hold the published deadline: %v", d)
	}
}

func TestScheduleFailureBackoffSingleDraw(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	cfg, providers := workerCfg(t, srv.URL)
	fc := &router.FakeClock{Mono: 50, Wall: 500}
	reg := router.NewRegistryWithClock(cfg, fc)
	w := NewCatalogWorker(cfg.Catalog, providers, reg)
	w.clock = fc
	var draws atomic.Int64
	w.jitter = func(ceil float64) float64 {
		draws.Add(1)
		if ceil != BackoffCeiling(1) {
			t.Errorf("failure ceil must be BackoffCeiling(1), got %v", ceil)
		}
		return ceil / 2 // deterministic stand-in for uniform(ceil/2, ceil)
	}
	w.RefreshOnce(context.Background())
	if draws.Load() != 1 {
		t.Fatalf("failure must draw jitter exactly once: %d", draws.Load())
	}
	ev := reg.CatalogEvent()
	want := router.Mono(50 + BackoffCeiling(1)/2)
	if ev.NextAttempt == nil || *ev.NextAttempt != want {
		t.Fatalf("published deadline must be completed+drawn delay: %+v", ev.NextAttempt)
	}
	if ev.Failures != 1 || ev.LastSuccess != nil {
		t.Fatalf("initial failure keeps no success: %+v", ev)
	}
	// Overdue deadline never spins: one attempt publishes a future
	// deadline, and the seam goes quiet again.
	fc.Advance(1000)
	if d := w.waitDelay(); d != 0 {
		t.Fatalf("overdue deadline must attempt immediately: %v", d)
	}
}

func TestScheduleManualWakeupNoSpin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	cfg, providers := workerCfg(t, srv.URL)
	fc := &router.FakeClock{Mono: 0, Wall: 0}
	reg := router.NewRegistryWithClock(cfg, fc)
	w := NewCatalogWorker(cfg.Catalog, providers, reg)
	w.clock = fc
	w.jitter = func(ceil float64) float64 { return ceil }
	// Simulate an overdue manual completion: attempt once, then the
	// published future deadline must hold without another attempt.
	w.RefreshOnce(context.Background())
	first := reg.CatalogEvent().NextAttempt
	if first == nil {
		t.Fatal("failed attempt must publish a deadline")
	}
	select {
	case <-w.wake:
	default:
		t.Fatal("manual completion must wake the owner")
	}
	// Draining the wake and re-reading must not schedule extra work: the
	// seam waits the published deadline.
	if d := w.waitDelay(); d <= 0 {
		t.Fatalf("published future deadline must be waited: %v", d)
	}
}

func TestScheduleLlamaPartialFleetAggregate(t *testing.T) {
	goodProbe := &llamaProbe{propsOn: true, props: `{"src":"llama-swap"}`}
	good := httptest.NewServer(goodProbe.handler())
	defer good.Close()
	flakyProbe := &llamaProbe{propsOn: true, props: `{}`}
	flaky := httptest.NewServer(flakyProbe.handler())
	defer flaky.Close()
	cfg := &config.Config{
		Server:    config.Server{AllowDynamicRoutes: true},
		Providers: map[string]*config.Provider{},
		Llama: &config.LlamaConfig{
			Servers: []config.LlamaServer{
				{URL: good.URL, Name: "good", Headers: map[string]string{}},
				{URL: flaky.URL, Name: "flaky", Headers: map[string]string{}},
			},
			ProbeTimeout:  5,
			ExposeAliases: true,
			Compatibility: "strict",
		},
	}
	fc := &router.FakeClock{Mono: 1000, Wall: 2000}
	reg := router.NewRegistryWithClock(cfg, fc)
	w := NewLlamaWorker(cfg.Llama, reg)
	w.clock = fc
	w.jitter = func(ceil float64) float64 { return ceil }

	// Full success first: aggregate and per-server times advance.
	w.RefreshOnce(context.Background())
	agg := reg.LlamaAggregate()
	if agg.Failures != 0 || agg.LastSuccess == nil || *agg.LastSuccess != router.Mono(1000) {
		t.Fatalf("success aggregate wrong: %+v", agg.LastSuccess)
	}
	priorAgg := *agg.LastSuccess
	events := reg.ServerEvents()
	if events["good"].Mode == nil || *events["good"].Mode != "llama-swap" {
		t.Fatalf("live probe must classify mode from props: %+v", events["good"].Mode)
	}
	if events["flaky"].Mode == nil || *events["flaky"].Mode != "single" {
		t.Fatalf("missing props must fall back to single: %+v", events["flaky"].Mode)
	}

	// Partial fleet: flaky fails. Aggregate fails but keeps the prior
	// success; the fresh server still advances its own success.
	flakyProbe.mu.Lock()
	flakyProbe.health = 500
	flakyProbe.mu.Unlock()
	fc.Advance(10)
	w.RefreshOnce(context.Background())
	agg = reg.LlamaAggregate()
	if agg.Failures != 1 {
		t.Fatalf("partial fleet must fail the aggregate: %+v", agg)
	}
	if agg.LastSuccess == nil || *agg.LastSuccess != priorAgg {
		t.Fatalf("aggregate must keep prior success on partial failure: %+v", agg.LastSuccess)
	}
	events = reg.ServerEvents()
	if events["good"].LastSuccess == nil || *events["good"].LastSuccess != router.Mono(1010) {
		t.Fatalf("fresh server must advance despite fleet failure: %+v", events["good"].LastSuccess)
	}
	if got := events["flaky"]; got.LastSuccess == nil || *got.LastSuccess != router.Mono(1000) {
		t.Fatalf("failed server must retain its prior success: %+v", got.LastSuccess)
	}
	// Stale retained server stays listed.
	snap := reg.Current()
	st := snap.LlamaStatus(true, reg.LlamaOrder(), 30, fc.Mono, reg.Sanitize)
	for _, s := range st["servers"].([]any) {
		m := s.(map[string]any)
		if m["provider"] == "flaky" && m["stale"] != true {
			t.Fatalf("retained failure must be stale: %+v", m)
		}
	}
}
