package router

import (
	"sync"
	"testing"

	"github.com/arhammahajan/model-router/internal/capability"
)

func TestListedOrderAndSource(t *testing.T) {
	reg := NewRegistry(testConfig())
	listed := reg.Current().ListedModels()
	if len(listed) != 1 || listed[0].Model.Name != "a" || listed[0].Source != "static" {
		t.Fatalf("static listing: %+v", listed)
	}
	if got := listed[0].Model.EffectiveDialect(listed[0].Provider); got != "anthropic" {
		t.Fatalf("effective dialect: %q", got)
	}
	// Canonical rows only: alias adds no row.
	if len(reg.Current().Models()) != 1 {
		t.Fatalf("canonical rows only")
	}
}

func TestListedEffectiveOverride(t *testing.T) {
	cfg := testConfig()
	cfg.Models[0].Dialect = "gemini"
	reg := NewRegistry(cfg)
	l := reg.Current().ListedModels()
	if got := l[0].Model.EffectiveDialect(l[0].Provider); got != "gemini" {
		t.Fatalf("override dialect: %q", got)
	}
}

func TestListedImmutableRace(t *testing.T) {
	reg := NewRegistry(testConfig())
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snap := reg.Current()
			listed := snap.ListedModels()
			if len(listed) != 1 {
				t.Errorf("listed len: %d", len(listed))
			}
			listed[0].Model.Name = "mut"
			_ = snap.CatalogDiscovery(10, 1)
			_ = snap.LlamaDiscovery("x", 10, 1)
		}()
	}
	wg.Wait()
}

// TestCapabilityOverridesSurvivePublish proves the pinned snapshot owns its
// override facts across publications: overrides admitted from config are
// deep-copied at New, republished with statuses by buildSnapshot, and
// returned to readers as caller-owned copies.
func TestCapabilityOverridesSurvivePublish(t *testing.T) {
	cfg := testConfig()
	admitted, err := capability.ValidateOverrides(map[string]any{
		"a": map[string]any{"tools": map[string]any{"supported": true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg.CapabilityOverrides = admitted
	reg := NewRegistry(cfg)
	if err := commitCatalogShim(reg,
		[]CatalogEntry{{Name: "cat-a", Provider: "up", Upstream: "cat-a"}}, nil, 100); err != nil {
		t.Fatal(err)
	}
	snap := reg.Current()
	// Statuses republished alongside overrides: last success at mono 100,
	// sampled at 200 with interval 60, derives stale.
	if stale := snap.CatalogDiscovery(60, 200).Stale(); stale == nil || !*stale {
		t.Fatalf("catalog freshness must republish: %v", stale)
	}
	got := snap.CapabilityOverrides("a")
	if b, ok := got["tools.supported"].Bool(); !ok || !b {
		t.Fatalf("override must survive publish: %v", got)
	}
	// Representative mutation: the returned copy is caller-owned.
	got["tools.supported"] = capability.BoolValue(false)
	again := snap.CapabilityOverrides("a")
	if b, ok := again["tools.supported"].Bool(); !ok || !b {
		t.Fatalf("returned override mutation must not move snapshot: %v", again)
	}
	// Unknown names yield an empty non-nil set.
	if empty := snap.CapabilityOverrides("missing"); empty == nil || len(empty) != 0 {
		t.Fatalf("unknown name must yield empty overrides: %v", empty)
	}
	if n := len(snap.ListedModels()); n != 2 {
		t.Fatalf("static + catalog rows: %d", n)
	}
}
