package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/discovery"
	"github.com/arhammahajan/router/internal/server"
)

func TestCatalogReviewServeErrorStopsWorker(t *testing.T) {
	catalogTestEnv(t)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer upstream.Close()
	cfg, err := config.Load(writeShutdownCfg(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Catalog = &config.Catalog{URL: upstream.URL, Timeout: 1, RefreshSeconds: 0, ModelProviders: map[string]string{"m1": "anth"}}
	srv := server.New(cfg)
	worker := discovery.NewCatalogWorker(cfg.Catalog, cfg.Providers, srv.Registry())
	defer worker.Stop()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = listener.Close()
	if code := serve(listener, srv, worker, nil, nil, time.Second, nil); code != 1 {
		t.Fatalf("closed listener must fail serving: %d", code)
	}
	worker.RefreshOnce(context.Background())
	if calls.Load() != 0 {
		t.Fatal("serve returned without stopping its worker")
	}
}
