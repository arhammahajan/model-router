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

	"github.com/arhammahajan/model-router/internal/config"
	"github.com/arhammahajan/model-router/internal/router"
)

func llamaObsBoundaryCfg(url string) *config.Config {
	return &config.Config{
		Server:    config.Server{AllowDynamicRoutes: true},
		Providers: map[string]*config.Provider{},
		Llama: &config.LlamaConfig{
			Servers:      []config.LlamaServer{{URL: url, Name: "llama-test"}},
			ProbeTimeout: 2, RefreshSeconds: 30,
		},
	}
}

func llamaObsServe(t *testing.T, models, props string, propsCode int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var propsCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(models))
	})
	mux.HandleFunc("/props", func(w http.ResponseWriter, r *http.Request) {
		propsCalls.Add(1)
		if r.URL.Query().Get("autoload") != "false" {
			t.Errorf("props must use autoload=false, got %q", r.URL.RawQuery)
		}
		if strings.Contains(r.URL.RawQuery, "model=") {
			t.Errorf("no per-model fan-out allowed, got %q", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(propsCode)
		_, _ = w.Write([]byte(props))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &propsCalls
}

func TestLlamaObsOneOptionalPropsProbe(t *testing.T) {
	srv, propsCalls := llamaObsServe(t, `{"data":[{"id":"m1"},{"id":"m2"}]}`, `{"build_info":"llama.cpp","modalities":{"vision":true}}`, 200)
	cfg := llamaObsBoundaryCfg(srv.URL)
	reg := router.NewRegistry(cfg)
	w := NewLlamaWorker(cfg.Llama, reg)
	if !w.RefreshOnce(context.Background()) {
		t.Fatalf("refresh owned")
	}
	if got := propsCalls.Load(); got != 1 {
		t.Fatalf("exactly one optional props probe, got %d", got)
	}
	snap := reg.Current()
	if _, ok := snap.LlamaBase("m1"); !ok {
		t.Fatalf("m1 base missing")
	}
	if _, ok := snap.LlamaBase("m2"); !ok {
		t.Fatalf("m2 base missing (same global props, no fan-out)")
	}
}

func TestLlamaObsBadPropsKeepsRoutes(t *testing.T) {
	for _, bad := range []struct {
		name string
		body string
		code int
	}{
		{"trailing", `{"build_info":"b"} trailing`, 200},
		{"array", `[1,2]`, 200},
		{"empty", ``, 200},
		{"status", `{"build_info":"b"}`, 500},
	} {
		t.Run(bad.name, func(t *testing.T) {
			srv, _ := llamaObsServe(t, `{"data":[{"id":"m1"}]}`, bad.body, bad.code)
			cfg := llamaObsBoundaryCfg(srv.URL)
			reg := router.NewRegistry(cfg)
			w := NewLlamaWorker(cfg.Llama, reg)
			if !w.RefreshOnce(context.Background()) {
				t.Fatalf("refresh owned")
			}
			snap := reg.Current()
			if len(snap.ListedModels()) == 0 {
				t.Fatalf("bad props must not drop valid routes")
			}
		})
	}
}

func TestLlamaObsNoExtraGETRace(t *testing.T) {
	srv, _ := llamaObsServe(t, `{"data":[{"id":"m1"}]}`, `{}`, 200)
	cfg := llamaObsBoundaryCfg(srv.URL)
	reg := router.NewRegistry(cfg)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := NewLlamaWorker(cfg.Llama, reg)
			w.RefreshOnce(context.Background())
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("race deadlock")
	}
}
