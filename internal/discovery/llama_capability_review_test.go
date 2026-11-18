package discovery

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/arhammahajan/model-router/internal/capability"
	"github.com/arhammahajan/model-router/internal/config"
	"github.com/arhammahajan/model-router/internal/router"
)

func mustCapRevWire(t *testing.T, base capability.Contract, snap *router.Snapshot, model, provider string) map[string]any {
	t.Helper()
	rendered := capability.Render(base, snap.CapabilityOverrides(model), snap.LlamaDiscovery(provider, 30, 0), 1000)
	raw, err := rendered.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Independent review of the llama worker boundary: optional props carry
// evidence only, never routes; one probe per server; no per-model fan-out.

func llamaCapRevCfg(url string) *config.Config {
	return &config.Config{
		Server:    config.Server{AllowDynamicRoutes: true},
		Providers: map[string]*config.Provider{},
		Llama: &config.LlamaConfig{
			Servers:      []config.LlamaServer{{URL: url, Name: "llama-test"}},
			ProbeTimeout: 2, RefreshSeconds: 30,
		},
	}
}

func llamaCapRevServe(t *testing.T, models, props string, code int, check func(r *http.Request)) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(models))
	})
	mux.HandleFunc("/props", func(w http.ResponseWriter, r *http.Request) {
		if check != nil {
			check(r)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(props))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestLlamaCapRevTwoObjectsPropsKeepsRoutes(t *testing.T) {
	srv := llamaCapRevServe(t, `{"data":[{"id":"m1"}]}`, `{"a":1} {"b":2}`, 200, nil)
	reg := router.NewRegistry(llamaCapRevCfg(srv.URL))
	w := NewLlamaWorker(llamaCapRevCfg(srv.URL).Llama, reg)
	if !w.RefreshOnce(context.Background()) {
		t.Fatalf("refresh owned")
	}
	snap := reg.Current()
	if len(snap.ListedModels()) == 0 {
		t.Fatalf("stacked JSON props must not drop valid routes")
	}
	if _, ok := snap.LlamaBase("m1"); !ok {
		t.Fatalf("route must exist even when props evidence is dropped")
	}
	// Evidence is dropped: base must be unknown, not faked.
	base, _ := snap.LlamaBase("m1")
	rendered := base.Clone()
	_ = rendered
}

func TestLlamaCapRevNumberPropsKeepsRoutes(t *testing.T) {
	srv := llamaCapRevServe(t, `{"data":[{"id":"m1"}]}`, `42`, 200, nil)
	reg := router.NewRegistry(llamaCapRevCfg(srv.URL))
	w := NewLlamaWorker(llamaCapRevCfg(srv.URL).Llama, reg)
	if !w.RefreshOnce(context.Background()) {
		t.Fatalf("refresh owned")
	}
	if len(reg.Current().ListedModels()) == 0 {
		t.Fatalf("non-object props must not drop valid routes")
	}
}

func TestLlamaCapRevRouterPropsIgnoredLive(t *testing.T) {
	srv := llamaCapRevServe(t, `{"data":[{"id":"m1"}]}`, `{"role":"router","modalities":{"vision":true}}`, 200,
		func(r *http.Request) {
			if got := r.URL.Query().Get("autoload"); got != "false" {
				t.Errorf("autoload=false required, got %q", r.URL.RawQuery)
			}
			if strings.Contains(r.URL.RawQuery, "model=") {
				t.Errorf("no per-model fan-out, got %q", r.URL.RawQuery)
			}
		})
	reg := router.NewRegistry(llamaCapRevCfg(srv.URL))
	w := NewLlamaWorker(llamaCapRevCfg(srv.URL).Llama, reg)
	if !w.RefreshOnce(context.Background()) {
		t.Fatalf("refresh owned")
	}
	snap := reg.Current()
	base, ok := snap.LlamaBase("m1")
	if !ok {
		t.Fatalf("router row must still list")
	}
	wire := mustCapRevWire(t, base, snap, "m1", "llama-test")
	modal := wire["modalities"].(map[string]any)["input"].(map[string]any)["image"].(map[string]any)
	if modal["value"] != nil {
		t.Fatalf("router global props must stay unknown, got %v", modal)
	}
}

func TestLlamaCapRevLoadedRowsStillNoFanout(t *testing.T) {
	models := `{"data":[{"id":"m1","status":{"value":"loaded"}},{"id":"m2","status":{"value":"sleeping"}}]}`
	var propsCalls int
	srv := llamaCapRevServe(t, models, `{"modalities":{"vision":true}}`, 200,
		func(r *http.Request) {
			propsCalls++
			if strings.Contains(r.URL.RawQuery, "model=") {
				t.Errorf("per-model fan-out deferred, got %q", r.URL.RawQuery)
			}
		})
	reg := router.NewRegistry(llamaCapRevCfg(srv.URL))
	w := NewLlamaWorker(llamaCapRevCfg(srv.URL).Llama, reg)
	if !w.RefreshOnce(context.Background()) {
		t.Fatalf("refresh owned")
	}
	if propsCalls != 1 {
		t.Fatalf("exactly one optional props probe for loaded rows, got %d", propsCalls)
	}
	if len(reg.Current().ListedModels()) != 2 {
		t.Fatalf("loaded rows must both publish: %v", reg.Current().ListedModels())
	}
}
