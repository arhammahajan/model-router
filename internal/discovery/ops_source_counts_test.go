package discovery

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/router"
)

func TestOpsLlamaRetainsAcceptedModelCountOnFailure(t *testing.T) {
	var failed atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" && failed.Load() {
			w.WriteHeader(503)
			return
		}
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"reserved"},{"id":"m"},{"id":"m"}]}`))
		} else {
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	cfg := llamaWorkerCfg(srv.URL, "localup")
	cfg.Providers["static"] = &config.Provider{Name: "static", Dialect: config.DialectOpenAIChat, BaseURL: srv.URL}
	cfg.Models = []config.Model{{Name: "reserved", Provider: "static", Upstream: "explicit"}}
	reg := router.NewRegistry(cfg)
	worker := NewLlamaWorker(cfg.Llama, reg)
	defer worker.CloseIdleConnections()
	defer worker.Stop()
	worker.RefreshOnce(context.Background())
	before := reg.ServerEvents()["localup"]
	if before.Models != 1 {
		t.Fatalf("fresh model count includes suppressed rows: %d", before.Models)
	}
	failed.Store(true)
	worker.RefreshOnce(context.Background())
	after := reg.ServerEvents()["localup"]
	if after.Models != 1 || after.LastSuccess == nil || before.LastSuccess == nil || *after.LastSuccess != *before.LastSuccess {
		t.Fatalf("outage changed retained source facts: before=%+v after=%+v", before, after)
	}
}
