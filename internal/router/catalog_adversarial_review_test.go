// Independent adversarial review: dynamic-route whitespace parity.
//
// Python oracle (model_router/discovery.py, resolve): dynamic routing splits
// at the FIRST "/" (partition) and REQUIRES the remainder to be non-blank
// after Python str.strip; the stored upstream model is the STRIPPED
// remainder (upstream_model=upstream.strip()). A blank remainder is a clean
// miss (None -> 404), never routed.
//
// Go under review (internal/router/snapshot.go, Resolve): splits at the
// first "/" (strings.Cut, same) but accepts any non-empty remainder
// verbatim, including whitespace-only, and stores it unstripped.
//
// Repro: Resolve("up/ ") is Found(Upstream=" ") in Go vs NotFound in Python;
// Resolve("up/  x  ") stores Upstream="  x  " in Go vs "x" in Python.
// End to end, POST {"model":"up/ "} spends a generation slot and upstream
// I/O on a blank model where Python 404s without I/O.
package router

import (
	"testing"

	"github.com/arhammahajan/model-router/internal/config"
)

func TestReviewDynamicWhitespaceMatchesPythonOracle(t *testing.T) {
	cfg := &config.Config{
		Server: config.Server{AllowDynamicRoutes: true},
		Providers: map[string]*config.Provider{
			"up": {Name: "up", Dialect: config.DialectOpenAIChat},
		},
	}
	s := New(cfg)

	// "up/ " -- Python: upstream " ".strip() is empty -> NotFound.
	if m, _, fault := s.Resolve("up/ "); fault != NotFound {
		t.Errorf("Resolve(%q) = fault %v upstream %q; Python oracle is NotFound (blank upstream after strip)",
			"up/ ", fault, reviewModelUpstream(m))
	}
	// "up/  x  " -- Python: upstream stored stripped ("x").
	if m, _, fault := s.Resolve("up/  x  "); fault != Found || reviewModelUpstream(m) != "x" {
		t.Errorf("Resolve(%q) = fault %v upstream %q; Python oracle is Found with upstream %q",
			"up/  x  ", fault, reviewModelUpstream(m), "x")
	}
	// Controls that already agree with the oracle (must keep passing).
	if _, _, fault := s.Resolve("up/"); fault != NotFound {
		t.Errorf("Resolve(%q) = fault %v; want NotFound", "up/", fault)
	}
	if m, _, fault := s.Resolve("up/x/y"); fault != Found || reviewModelUpstream(m) != "x/y" {
		t.Errorf("Resolve(%q) = fault %v upstream %q; want Found/x/y", "up/x/y", fault, reviewModelUpstream(m))
	}
}

// TestReviewUnmappedBeatsDynamicForProviderShapedID pins the contract order
// "catalog-unmapped fault before dynamic provider/model" for the only shape
// where the order is observable: a provider-shaped ID ("up/ghost") with
// dynamic routes enabled. Existing coverage only exercises slash-free
// unmapped IDs, for which dynamic could never match.
func TestReviewUnmappedBeatsDynamicForProviderShapedID(t *testing.T) {
	cfg := catalogCfg()
	cfg.Server.AllowDynamicRoutes = true
	reg := NewRegistry(cfg)
	publish(t, reg, nil, []string{"up/ghost"}, 1000)
	if _, _, fault := reg.Current().Resolve("up/ghost"); fault != UnmappedCatalog {
		t.Fatalf("provider-shaped unmapped ID must fault UnmappedCatalog even with dynamic on, got %v", fault)
	}
	// And a non-listed provider-shaped ID still falls through to dynamic.
	m, _, fault := reg.Current().Resolve("up/other")
	if fault != Found || m.Upstream != "other" {
		t.Fatalf("dynamic fallback must survive alongside unmapped set: %+v %v", m, fault)
	}
}

func reviewModelUpstream(m *config.Model) string {
	if m == nil {
		return "<nil>"
	}
	return m.Upstream
}
