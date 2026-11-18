package discovery

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/arhammahajan/model-router/internal/config"
	"github.com/arhammahajan/model-router/internal/router"
)

func TestLlamaFleetDeadlineRetainsSuccessfulServers(t *testing.T) {
	good := httptest.NewServer((&llamaProbe{propsOn: true, props: `{}`}).handler())
	defer good.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer bad.Close()
	cfg := llamaWorkerCfg(good.URL, "good")
	cfg.Llama.ProbeTimeout = 0.05
	cfg.Llama.Servers = append(cfg.Llama.Servers, config.LlamaServer{URL: bad.URL, Name: "bad"})
	reg := router.NewRegistry(cfg)
	w := NewLlamaWorker(cfg.Llama, reg)
	defer w.CloseIdleConnections()
	defer w.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), LlamaFleetBound(cfg.Llama))
	defer cancel()
	w.RefreshOnce(ctx)
	if ctx.Err() != nil {
		t.Fatal("fleet deadline raced the per-server timeout")
	}
	if !reg.Current().Ready() || reg.Current().UsableCount() != 1 {
		t.Fatalf("successful server lost when its peer timed out: %s", w.LastError())
	}
}

func TestLlamaFleetBoundSaturatesWithoutShorteningProbe(t *testing.T) {
	cfg := &config.LlamaConfig{ProbeTimeout: 9e9, Servers: make([]config.LlamaServer, 9)}
	if bound := LlamaFleetBound(cfg); bound < time.Duration(9e9*float64(time.Second)) {
		t.Fatalf("fleet bound shortens a valid per-server timeout: %v", bound)
	}
}
