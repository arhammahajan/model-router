package discovery

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arhammahajan/router/internal/router"
)

type opsBusyClock struct {
	calls atomic.Int64
	flood chan struct{}
}

func (c *opsBusyClock) Sample() router.Sample {
	if c.calls.Add(1) == 20 {
		close(c.flood)
	}
	return router.Sample{Mono: 10, Wall: 100}
}

func TestOpsSchedulerWaitsForBusyManualAttempt(t *testing.T) {
	for _, source := range []string{"catalog", "llama"} {
		t.Run(source, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var enterOnce, releaseOnce sync.Once
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				enterOnce.Do(func() { close(entered) })
				<-release
				if r.URL.Path == "/props" || r.URL.Path == "/health" {
					_, _ = w.Write([]byte(`{}`))
				} else {
					_, _ = w.Write([]byte(`{"data":[{"id":"m1"}]}`))
				}
			}))
			defer srv.Close()
			clock := &opsBusyClock{flood: make(chan struct{})}
			var refresh func(context.Context) bool
			var start func(context.Context)
			var stop func()
			var closeIdle func()
			if source == "catalog" {
				cat, providers := workerCfg(t, srv.URL)
				cat.Catalog.RefreshSeconds = 60
				reg := router.NewRegistryWithClock(cat, clock)
				if err := reg.CommitCatalog(router.CatalogAttempt{Event: router.SourceEvent{
					NextAttempt: router.MonoPtr(0), Err: "prior failure", Failures: 1,
				}}); err != nil {
					t.Fatal(err)
				}
				worker := NewCatalogWorker(cat.Catalog, providers, reg)
				refresh, start, stop, closeIdle = worker.RefreshOnce, worker.Start, worker.Stop, worker.CloseIdleConnections
			} else {
				cfg := llamaWorkerCfg(srv.URL, "localup")
				cfg.Llama.RefreshSeconds = 60
				reg := router.NewRegistryWithClock(cfg, clock)
				if err := reg.CommitLlama(router.LlamaAttempt{
					Layer:     router.LlamaLayer{Servers: map[string]router.LlamaServerSnapshot{"localup": {State: router.LlamaNeverSucceeded}}},
					Aggregate: router.SourceEvent{NextAttempt: router.MonoPtr(0), Err: "prior failure", Failures: 1},
					Servers:   map[string]router.ServerEvent{"localup": {URL: srv.URL}},
				}); err != nil {
					t.Fatal(err)
				}
				worker := NewLlamaWorker(cfg.Llama, reg)
				refresh, start, stop, closeIdle = worker.RefreshOnce, worker.Start, worker.Stop, worker.CloseIdleConnections
			}
			defer closeIdle()
			defer stop()
			defer releaseOnce.Do(func() { close(release) })
			done := make(chan struct{})
			go func() { defer close(done); refresh(context.Background()) }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("manual probe did not start")
			}
			start(context.Background())
			select {
			case <-clock.flood:
				t.Fatal("overdue scheduler spun while manual attempt held the lock")
			case <-time.After(50 * time.Millisecond):
			}
			releaseOnce.Do(func() { close(release) })
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("manual attempt did not complete")
			}
		})
	}
}
