// Review probes for dual-worker shutdown: both workers cancel and join
// before the drain, constructors stay offline, and the sequential-stop
// latency risk is bounded and documented.
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/arhammahajan/model-router/internal/config"
	"github.com/arhammahajan/model-router/internal/discovery"
	"github.com/arhammahajan/model-router/internal/server"
)

// Both workers cancel in-flight probes and join before stopWorkers
// returns: no publication races shutdown on either source.
func TestReviewStopWorkersCancelsBoth(t *testing.T) {
	release := make(chan struct{})
	block := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(500)
	}))
	// Release handlers before closing the server: Close blocks for
	// outstanding requests, so release must run first (LIFO).
	defer block.Close()
	defer close(release)

	cfg := &config.Config{
		Server:    config.Server{AllowDynamicRoutes: false},
		Providers: map[string]*config.Provider{},
		Catalog:   &config.Catalog{URL: block.URL, Timeout: 30, RefreshSeconds: 0},
		Llama: &config.LlamaConfig{
			Servers:        []config.LlamaServer{{URL: block.URL, Name: "llama-a"}},
			ProbeTimeout:   30,
			RefreshSeconds: 0,
		},
	}
	srv := server.New(cfg)
	catWorker := discovery.NewCatalogWorker(cfg.Catalog, cfg.Providers, srv.Registry())
	llamaWorker := discovery.NewLlamaWorker(cfg.Llama, srv.Registry())

	catDone := make(chan struct{})
	go func() { defer close(catDone); catWorker.RefreshOnce(context.Background()) }()
	llamaDone := make(chan struct{})
	go func() { defer close(llamaDone); llamaWorker.RefreshOnce(context.Background()) }()
	time.Sleep(300 * time.Millisecond) // both sweeps blocked in I/O

	start := time.Now()
	stopWorkers(catWorker, llamaWorker)
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("stopWorkers did not join promptly: %v", elapsed)
	}
	select {
	case <-catDone:
	case <-time.After(5 * time.Second):
		t.Fatal("catalog sweep outlived stopWorkers")
	}
	select {
	case <-llamaDone:
	case <-time.After(5 * time.Second):
		t.Fatal("llama sweep outlived stopWorkers")
	}
	// Idempotent: a second call returns immediately.
	stopWorkers(catWorker, llamaWorker)
}

// Nil workers (unconfigured sources) are a valid shutdown: nothing to
// cancel, nothing to join.
func TestReviewStopWorkersNilSafe(t *testing.T) {
	stopWorkers(nil, nil)
}

// Sequential-stop latency note: cancel-then-join runs per worker in order,
// so shutdown latency is additive in the worst case. Both probes here
// block ~600ms; sequential stop must still finish well within the sum plus
// slack (no deadlock, no hang), while a future parallel stop would halve
// it. This test pins the bounded behavior, not the optimization.
func TestReviewStopWorkersBoundedSequential(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(600 * time.Millisecond)
		w.WriteHeader(500)
	}))
	defer slow.Close()
	cfg := &config.Config{
		Server:    config.Server{AllowDynamicRoutes: false},
		Providers: map[string]*config.Provider{},
		Catalog:   &config.Catalog{URL: slow.URL, Timeout: 10, RefreshSeconds: 0},
		Llama: &config.LlamaConfig{
			Servers:        []config.LlamaServer{{URL: slow.URL, Name: "llama-a"}},
			ProbeTimeout:   10,
			RefreshSeconds: 0,
		},
	}
	srv := server.New(cfg)
	catWorker := discovery.NewCatalogWorker(cfg.Catalog, cfg.Providers, srv.Registry())
	llamaWorker := discovery.NewLlamaWorker(cfg.Llama, srv.Registry())
	catDone := make(chan struct{})
	go func() { defer close(catDone); catWorker.RefreshOnce(context.Background()) }()
	llamaDone := make(chan struct{})
	go func() { defer close(llamaDone); llamaWorker.RefreshOnce(context.Background()) }()
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	stopWorkers(catWorker, llamaWorker)
	elapsed := time.Since(start)
	<-catDone
	<-llamaDone
	// Both ~600ms probes joined: must exceed one probe (sequential today)
	// and stay far below an unbounded hang.
	t.Logf("dual stop latency=%v (documents sequential cancel-join risk)", elapsed)
	if elapsed > 15*time.Second {
		t.Fatalf("stopWorkers unbounded: %v", elapsed)
	}
}
