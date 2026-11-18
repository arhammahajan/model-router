// Local runtime discovery: server engine from SERVER props (first
// build_info token, casefolded), router/swap global props ignored for
// observations but still read for engine identity, malformed props
// default LlamaCpp only after live success, cache-only servers stay
// Unobserved, stale retains, and empty servers publish runtime.
package discovery

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/arhammahajan/model-router/internal/capability"
	"github.com/arhammahajan/model-router/internal/router"
)

func runtimeOf(t *testing.T, reg *router.Registry, name string) capability.LocalRuntime {
	t.Helper()
	return reg.Current().LlamaRuntime(name)
}

func TestRuntimeStrataFromServerProps(t *testing.T) {
	f := &llamaProbe{propsOn: true, props: `{"build_info": "PolyStrata 0.1.45", "role": "router"}`}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	cfg := llamaWorkerCfg(srv.URL, "s1")
	_, reg := refreshLlama(t, cfg)
	if got := runtimeOf(t, reg, "s1"); got != capability.RuntimeStrata {
		t.Fatalf("router-mode global build must still identify strata: %v", got)
	}
}

func TestRuntimeIgnoresModelFlags(t *testing.T) {
	// Global modality/template/limit evidence never becomes engine
	// evidence: only build_info identifies. A non-strata build with
	// strata-ish model metadata stays LlamaCpp.
	f := &llamaProbe{propsOn: true,
		props:  `{"build_info": "llama.cpp", "modalities": {"vision": true}}`,
		models: `{"data": [{"id": "strata-x", "metadata": {"engine": "strata"}}]}`}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	cfg := llamaWorkerCfg(srv.URL, "s1")
	_, reg := refreshLlama(t, cfg)
	if got := runtimeOf(t, reg, "s1"); got != capability.RuntimeLlamaCpp {
		t.Fatalf("model metadata must not select engine: %v", got)
	}
}

func TestRuntimeMalformedPropsDefaultsLlamaCpp(t *testing.T) {
	for _, props := range []string{
		`{"build_info": 42}`,
		`{"build_info": ""}`,
		`{"build_info": "   "}`,
		`{"other": true}`,
	} {
		f := &llamaProbe{propsOn: true, props: props}
		srv := httptest.NewServer(f.handler())
		cfg := llamaWorkerCfg(srv.URL, "s1")
		_, reg := refreshLlama(t, cfg)
		if got := runtimeOf(t, reg, "s1"); got != capability.RuntimeLlamaCpp {
			t.Fatalf("props %s must default LlamaCpp on live probe: %v", props, got)
		}
		srv.Close()
	}
	// Props endpoint failing entirely still defaults LlamaCpp on a live
	// successful models probe.
	f := &llamaProbe{propsOn: false}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	cfg := llamaWorkerCfg(srv.URL, "s1")
	_, reg := refreshLlama(t, cfg)
	if got := runtimeOf(t, reg, "s1"); got != capability.RuntimeLlamaCpp {
		t.Fatalf("absent props must default LlamaCpp: %v", got)
	}
}

func TestRuntimeEmptyServerDynamic(t *testing.T) {
	// A valid empty server publishes its runtime; routes stay dynamic.
	f := &llamaProbe{propsOn: true, props: `{"build_info": "llama.cpp"}`, models: `{"data": []}`}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	cfg := llamaWorkerCfg(srv.URL, "s1")
	_, reg := refreshLlama(t, cfg)
	if got := runtimeOf(t, reg, "s1"); got != capability.RuntimeLlamaCpp {
		t.Fatalf("empty server must publish runtime: %v", got)
	}
	if !reg.Current().HasLlamaProvider("s1") {
		t.Fatalf("empty server stays local (dynamic available)")
	}
}

func TestRuntimeStaleRetainsAndFailedNeverInvents(t *testing.T) {
	f := &llamaProbe{propsOn: true, props: `{"build_info": "strata fleet"}`}
	srv := httptest.NewServer(f.handler())
	cfg := llamaWorkerCfg(srv.URL, "s1")
	w, reg := refreshLlama(t, cfg)
	if got := runtimeOf(t, reg, "s1"); got != capability.RuntimeStrata {
		t.Fatalf("strata: %v", got)
	}
	// A later failure retains the old runtime with stale routes.
	f.mu.Lock()
	f.health = 500
	f.mu.Unlock()
	w.RefreshOnce(context.Background())
	snap := reg.Current()
	if got := snap.LlamaRuntime("s1"); got != capability.RuntimeStrata {
		t.Fatalf("stale must retain runtime: %v", got)
	}
	if !snap.HasLlamaProvider("s1") {
		t.Fatalf("stale keeps membership")
	}
	// A never-succeeded server invents nothing.
	f2 := &llamaProbe{health: 500}
	srv2 := httptest.NewServer(f2.handler())
	defer srv2.Close()
	cfg2 := llamaWorkerCfg(srv2.URL, "s2")
	_, reg2 := refreshLlama(t, cfg2)
	if got := runtimeOf(t, reg2, "s2"); got != capability.RuntimeUnobserved {
		t.Fatalf("first failure must not invent runtime: %v", got)
	}
}

func TestRuntimeSwappedEngine(t *testing.T) {
	// A server that changes its build between probes publishes the new
	// engine atomically with the new routes.
	f := &llamaProbe{propsOn: true, props: `{"build_info": "llama.cpp"}`}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	cfg := llamaWorkerCfg(srv.URL, "s1")
	w, reg := refreshLlama(t, cfg)
	if got := runtimeOf(t, reg, "s1"); got != capability.RuntimeLlamaCpp {
		t.Fatalf("initial: %v", got)
	}
	f.mu.Lock()
	f.props = `{"build_info": "polystrata 0.2"}`
	f.mu.Unlock()
	w.RefreshOnce(context.Background())
	if got := runtimeOf(t, reg, "s1"); got != capability.RuntimeStrata {
		t.Fatalf("swapped build must republish: %v", got)
	}
}
