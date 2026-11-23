package discovery

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/arhammahajan/router/internal/router"
)

func TestLlamaProbeHeaderServerWinsAcrossCasing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Values("X-Shared"); len(got) != 1 || got[0] != "server" {
			t.Errorf("server override lost on %s: %v", r.URL.Path, got)
		}
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"m"}]}`))
		} else {
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	cfg := llamaWorkerCfg(srv.URL, "localup")
	cfg.Llama.Headers = map[string]string{"X-Shared": "global"}
	cfg.Llama.Servers[0].Headers = map[string]string{"x-shared": "server"}
	reg := router.NewRegistry(cfg)
	w := NewLlamaWorker(cfg.Llama, reg)
	defer w.CloseIdleConnections()
	defer w.Stop()
	w.RefreshOnce(context.Background())
	if !reg.Current().Ready() {
		t.Fatalf("probe failed: %s", w.LastError())
	}
}
