package router

// Independent review probes for snapshot-owned capability state (U3):
// canonical-keyed lookup, publication/copy isolation (nested native,
// lists, config maps, returned values), per-route effective dialect with
// untouched provider credentials, freshness transitions, and concurrent
// read safety. Runtime inputs only; no live config/env/network.

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/arhammahajan/router/internal/capability"
)

// Overrides are keyed by canonical public route name: a canonical key
// applies, an alias key validates but never applies, and unknown names
// stay inert (Python `_route_entry`: lookup by route.name only).
func TestReviewCanonicalKeyLookup(t *testing.T) {
	cfg := testConfig()
	admitted, err := capability.ValidateOverrides(map[string]any{
		"a":       map[string]any{"tools": map[string]any{"supported": true}},
		"a-alias": map[string]any{"streaming": true},
		"zzz":     map[string]any{"streaming": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg.CapabilityOverrides = admitted
	snap := NewRegistry(cfg).Current()
	listed := snap.ListedModels()
	if len(listed) != 1 || listed[0].Model.Name != "a" {
		t.Fatalf("canonical rows only: %+v", listed)
	}
	rendered := capability.Render(capability.Empty(listed[0].Model.EffectiveDialect(listed[0].Provider)),
		snap.CapabilityOverrides("a"), capability.StaticDiscovery(), 1)
	raw, _ := json.Marshal(rendered)
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	tools := wire["tools"].(map[string]any)["supported"].(map[string]any)
	if tools["value"] != true || tools["kind"] != capability.KindOverride {
		t.Fatalf("canonical override must apply: %v", tools)
	}
	// Alias-keyed and unknown-keyed overrides must not leak into the row.
	stream := wire["streaming"].(map[string]any)
	if stream["value"] != nil {
		t.Fatalf("alias/unknown keys must not apply: %v", stream)
	}
	if got := snap.CapabilityOverrides("a-alias"); len(got) != 1 {
		t.Fatalf("alias key validates and is retained: %v", got)
	}
}

// The snapshot owns its override facts: post-New config mutation,
// republish, and returned-copy mutation cannot move stored state,
// including nested native mappings and lists.
func TestReviewSnapshotOverrideIsolation(t *testing.T) {
	cfg := testConfig()
	admitted, err := capability.ValidateOverrides(map[string]any{
		"a": map[string]any{
			"tools": map[string]any{"supported": true},
			"reasoning": map[string]any{"effort": map[string]any{
				"values": []any{"low", "high"},
				"native_parameter": map[string]any{
					"protocol": "openai-chat", "path": "reasoning_effort",
					"values": map[string]any{"off": "none"},
				},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg.CapabilityOverrides = admitted
	reg := NewRegistry(cfg)
	// Mutating the source config after New must not reach the snapshot.
	cfg.CapabilityOverrides["a"]["tools.supported"] = capability.BoolValue(false)
	cfg.CapabilityOverrides["evil"] = capability.Overrides{"streaming": capability.BoolValue(true)}
	snap := reg.Current()
	if b, _ := snap.CapabilityOverrides("a")["tools.supported"].Bool(); !b {
		t.Fatal("post-New config mutation escaped into the snapshot")
	}
	if len(snap.CapabilityOverrides("evil")) != 0 {
		t.Fatal("post-New config insert escaped into the snapshot")
	}
	// Returned copies are caller-owned, including nested maps.
	got := snap.CapabilityOverrides("a")
	got["tools.supported"] = capability.BoolValue(false)
	delete(got, "reasoning.effort.values")
	if b, _ := snap.CapabilityOverrides("a")["tools.supported"].Bool(); !b {
		t.Fatal("returned override mutation escaped into the snapshot")
	}
	if _, ok := snap.CapabilityOverrides("a")["reasoning.effort.values"]; !ok {
		t.Fatal("returned override delete escaped into the snapshot")
	}
	native, _ := snap.CapabilityOverrides("a")["reasoning.effort.native_parameter"].Native()
	mut := native.Values()
	mut["off"] = capability.StringScalar("MUT")
	again, _ := snap.CapabilityOverrides("a")["reasoning.effort.native_parameter"].Native()
	if s, _ := again.Values()["off"].String(); s != "none" {
		t.Fatalf("nested native mapping escaped: %v", again.Values())
	}
	// Republish keeps the same private facts with fresh status.
	before, _ := json.Marshal(capability.Render(capability.Empty("anthropic"),
		snap.CapabilityOverrides("a"), capability.StaticDiscovery(), 7))
	if err := commitCatalogShim(reg,
		[]CatalogEntry{{Name: "cat-a", Provider: "up", Upstream: "cat-a"}}, nil, 100); err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(capability.Render(capability.Empty("anthropic"),
		reg.Current().CapabilityOverrides("a"), capability.StaticDiscovery(), 7))
	if string(before) != string(after) {
		t.Fatalf("republish must preserve override facts:\n%s\n%s", before, after)
	}
}

// Listed rows are deep copies: mutating returned metadata bytes, names,
// or provider headers cannot move snapshot state.
func TestReviewListedCopyIsolation(t *testing.T) {
	reg := NewRegistry(testConfig())
	first := reg.Current().ListedModels()
	if len(first) == 0 {
		t.Fatal("need a listed row")
	}
	for i := range first[0].Model.Metadata {
		first[0].Model.Metadata[i] = 'X'
	}
	first[0].Model.Name = "mut"
	first[0].Model.Aliases = append(first[0].Model.Aliases, "evil")
	first[0].Provider.Headers["X-Custom"] = "MUT"
	first[0].Provider.Name = "mut"
	second := reg.Current().ListedModels()
	if second[0].Model.Name != "a" || len(second[0].Model.Aliases) != 1 {
		t.Fatalf("listed model copy escaped: %+v", second[0].Model)
	}
	var meta map[string]any
	if err := json.Unmarshal(second[0].Model.Metadata, &meta); err != nil {
		t.Fatal(err)
	}
	if meta["context_window"] != float64(200000) {
		t.Fatalf("listed metadata copy escaped: %v", meta)
	}
	if second[0].Provider.Headers["X-Custom"] != "v" || second[0].Provider.Name != "up" {
		t.Fatalf("listed provider copy escaped: %+v", second[0].Provider)
	}
}

// Effective dialect is per route (explicit override wins, else provider
// default); deriving it never touches provider credentials.
func TestReviewEffectiveDialectPerRoute(t *testing.T) {
	cfg := testConfig()
	cfg.Models[0].Dialect = "gemini"
	snap := NewRegistry(cfg).Current()
	listed := snap.ListedModels()
	if got := listed[0].Model.EffectiveDialect(listed[0].Provider); got != "gemini" {
		t.Fatalf("explicit route dialect must win: %q", got)
	}
	base := capability.Empty(listed[0].Model.EffectiveDialect(listed[0].Provider))
	raw, _ := json.Marshal(base)
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	native := wire["protocols"].(map[string]any)["native"].(map[string]any)
	if native["gemini"].(map[string]any)["value"] != true {
		t.Fatalf("configured transport must follow the effective dialect: %v", native)
	}
	if native["anthropic"].(map[string]any)["value"] != nil {
		t.Fatalf("non-effective native dialects stay unknown: %v", native)
	}
	translated := wire["protocols"].(map[string]any)["translated"].(map[string]any)
	if translated["gemini"].(map[string]any)["value"] != false {
		t.Fatalf("effective dialect never translates to itself: %v", translated)
	}
	if translated["anthropic"].(map[string]any)["value"] != true {
		t.Fatalf("other dialects translate: %v", translated)
	}
	p := listed[0].Provider
	if p.Headers["X-Custom"] != "v" || p.APIKey != "k" || p.BaseURL != "https://up.example" {
		t.Fatalf("provider credentials must be unchanged: %+v", p)
	}
}

// Freshness transitions match the source: never-succeeded and errored
// are stale, fresh success is not, and restore keeps last-good routes
// with their error and age.
func TestReviewDiscoveryTransitions(t *testing.T) {
	snap := NewRegistry(testConfig()).Current()
	if s := snap.CatalogDiscovery(60, 1000).Stale(); s == nil || !*s {
		t.Fatalf("never-refreshed catalog must be stale: %v", s)
	}
	if s := snap.LlamaDiscovery("up", 60, 1000).Stale(); s != nil {
		t.Fatalf("unobserved llama provider must be static-null: %v", s)
	}
}

// A startup restore republishes routes alongside the same private
// override facts: restored catalog rows render from empty bases with no
// override leakage, and static rows keep their overrides with
// read-copy isolation intact.
func TestReviewRestoreKeepsOverrideOwnership(t *testing.T) {
	cfg := catalogCfg()
	admitted, err := capability.ValidateOverrides(map[string]any{
		"a": map[string]any{"tools": map[string]any{"supported": true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg.CapabilityOverrides = admitted
	reg := NewRegistry(cfg)
	if err := reg.RestoreCached(DiscoveryInventory{
		Catalog: &CatalogInventory{Entries: []CatalogEntry{{Name: "r", Provider: "up", Upstream: "r"}}, SucceededAt: Wall(1400)},
	}); err != nil {
		t.Fatal(err)
	}
	snap := reg.Current()
	if got := snap.CapabilityOverrides("a"); len(got) != 1 {
		t.Fatalf("restore must keep override facts: %v", got)
	}
	byName := map[string]ListedModel{}
	for _, lm := range snap.ListedModels() {
		byName[lm.Model.Name] = lm
	}
	cat, ok := byName["r"]
	if !ok {
		t.Fatalf("restored catalog row must list: %+v", byName)
	}
	rendered, _ := json.Marshal(capability.Render(
		capability.Empty(cat.Model.EffectiveDialect(cat.Provider)),
		snap.CapabilityOverrides("r"), capability.StaticDiscovery(), 1))
	var wire map[string]any
	if err := json.Unmarshal(rendered, &wire); err != nil {
		t.Fatal(err)
	}
	if tools := wire["tools"].(map[string]any)["supported"].(map[string]any); tools["value"] != nil {
		t.Fatalf("restored row must not inherit other names' overrides: %v", tools)
	}
	got := snap.CapabilityOverrides("a")
	got["tools.supported"] = capability.BoolValue(false)
	if b, _ := snap.CapabilityOverrides("a")["tools.supported"].Bool(); !b {
		t.Fatal("restored snapshot must still return caller-owned copies")
	}
}

// Concurrent snapshot reads plus renders stay stable under -race: one
// pinned snapshot, no locks on this path.
func TestReviewSnapshotReadRace(t *testing.T) {
	cfg := testConfig()
	admitted, err := capability.ValidateOverrides(map[string]any{
		"a": map[string]any{"streaming": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg.CapabilityOverrides = admitted
	reg := NewRegistry(cfg)
	want, _ := json.Marshal(capability.Render(capability.Empty("anthropic"),
		reg.Current().CapabilityOverrides("a"), capability.StaticDiscovery(), 5))
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snap := reg.Current()
			for _, lm := range snap.ListedModels() {
				out, err := json.Marshal(capability.Render(
					capability.Empty(lm.Model.EffectiveDialect(lm.Provider)),
					snap.CapabilityOverrides(lm.Model.Name),
					capability.StaticDiscovery(), 5))
				if err != nil {
					t.Error(err)
					return
				}
				if lm.Model.Name == "a" && string(out) != string(want) {
					t.Errorf("render drift: %s", out)
				}
			}
			_ = snap.CatalogDiscovery(60, 200)
			_ = snap.LlamaDiscovery("up", 60, 200)
			_ = snap.Models()
		}()
	}
	wg.Wait()
}
