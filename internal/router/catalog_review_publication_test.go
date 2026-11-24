package router

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/arhammahajan/router/internal/capability"
	"github.com/arhammahajan/router/internal/config"
)

func reviewAssetForTest(t *testing.T) capability.ReviewAsset {
	t.Helper()
	a, err := capability.LoadReviewAsset(capability.EmbeddedReviewBytes())
	if err != nil {
		t.Fatalf("asset must validate: %v", err)
	}
	return a
}

// reviewCatalogCfg returns a two-provider config (up=anthropic,
// chat=openai-chat) with the review source selected. Exact IDs:
// glm-5.3-flash is openai-chat, claude-haiku-5-5 is anthropic.
func reviewCatalogCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg := testConfig()
	chat := *cfg.Providers["up"]
	chat.Name = "chat"
	chat.DefaultDialect = config.DialectOpenAIChat
	chat.BaseURL = "https://chat.example"
	cfg.Providers["chat"] = &chat
	asset := reviewAssetForTest(t)
	cfg.Catalog = &config.Catalog{
		URL:            "http://127.0.0.1:9/models",
		Timeout:        5,
		RefreshSeconds: 60,
		ModelProviders: map[string]string{
			"glm-5.3-flash":    "chat",
			"claude-haiku-5-5": "up",
			"unknown-model-z":  "up",
			"a":                "up",
		},
		MetadataSource: "opencode-go-v1",
		ReviewAsset:    asset,
		HasReview:      true,
	}
	return cfg
}

func renderBase(t *testing.T, base capability.Contract, name string, snap *Snapshot, wall float64) map[string]any {
	t.Helper()
	out := capability.Render(base, snap.CapabilityOverrides(name), snap.CatalogDiscovery(60, Mono(wall)), wall)
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	return wire
}

// Publication builds private per-route bases: match applies reviewed facts,
// mismatch keeps transport with applicable false, unknown stays empty; every
// catalog row carries availability at the original wall success.
func TestFocusCatalogPublicationBases(t *testing.T) {
	reg := NewRegistry(reviewCatalogCfg(t))
	entries := []CatalogEntry{
		{Name: "glm-5.3-flash", Provider: "chat", Upstream: "glm-5.3-flash"},
		{Name: "claude-haiku-5-5", Provider: "up", Upstream: "claude-haiku-5-5"},
		{Name: "unknown-model-z", Provider: "up", Upstream: "unknown-model-z"},
	}
	if err := commitCatalogShim(reg, entries, nil, 1000); err != nil {
		t.Fatal(err)
	}
	snap := reg.Current()
	// Match: reviewed facts present with source/kind.
	base, ok := snap.CatalogBase("glm-5.3-flash")
	if !ok {
		t.Fatal("match must store a base")
	}
	wire := renderBase(t, base, "glm-5.3-flash", snap, 1500)
	tools := wire["tools"].(map[string]any)["supported"].(map[string]any)
	if tools["value"] != true || tools["kind"] != "reviewed" || tools["source"] != "go.review.tools.supported" {
		t.Fatalf("match must apply reviewed facts: %v", tools)
	}
	// Mismatch gate: publish the openai-chat row under the anthropic
	// provider and re-gate by the new (wrong) dialect.
	reg2 := NewRegistry(reviewCatalogCfg(t))
	if err := commitCatalogShim(reg2, []CatalogEntry{{Name: "glm-5.3-flash", Provider: "up", Upstream: "glm-5.3-flash"}}, nil, 1000); err != nil {
		t.Fatal(err)
	}
	mis := renderBase(t, mustBase(t, reg2.Current(), "glm-5.3-flash"), "glm-5.3-flash", reg2.Current(), 1500)
	if mis["sources"].(map[string]any)["go.review"].(map[string]any)["applicable"] != false {
		t.Fatalf("mismatch applicable must be false: %v", mis["sources"])
	}
	mtools := mis["tools"].(map[string]any)["supported"].(map[string]any)
	if mtools["value"] != nil {
		t.Fatalf("mismatch facts must stay unknown: %v", mtools)
	}
	native := mis["protocols"].(map[string]any)["native"].(map[string]any)
	if native["anthropic"].(map[string]any)["value"] != true {
		t.Fatalf("mismatch keeps route transport: %v", native)
	}
	// Unknown ID stays routable with empty facts plus catalog availability.
	ubase, ok := snap.CatalogBase("unknown-model-z")
	if !ok {
		t.Fatal("unknown catalog row must still store a base")
	}
	uwire := renderBase(t, ubase, "unknown-model-z", snap, 1500)
	if _, has := uwire["sources"].(map[string]any)["go.review"]; has {
		t.Fatalf("unknown must carry no review source")
	}
	utools := uwire["tools"].(map[string]any)["supported"].(map[string]any)
	if utools["value"] != nil {
		t.Fatalf("unknown facts must be null: %v", utools)
	}
	if _, _, fault := snap.Resolve("unknown-model-z"); fault != Found {
		t.Fatal("catalog unknown IDs remain routable")
	}
}

// Without the opt-in, catalog rows render empty plus availability only.
func TestFocusCatalogAbsentOptin(t *testing.T) {
	cfg := catalogCfg() // no metadata_source
	reg := NewRegistry(cfg)
	if err := commitCatalogShim(reg, []CatalogEntry{{Name: "glm-5.3-flash", Provider: "up", Upstream: "glm-5.3-flash"}}, nil, 1000); err != nil {
		t.Fatal(err)
	}
	snap := reg.Current()
	base, ok := snap.CatalogBase("glm-5.3-flash")
	if !ok {
		t.Fatal("absent opt-in must still store an availability base")
	}
	wire := renderBase(t, base, "glm-5.3-flash", snap, 1500)
	if _, has := wire["sources"].(map[string]any)["go.review"]; has {
		t.Fatalf("absent opt-in must carry no review source")
	}
	cat := wire["sources"].(map[string]any)["catalog"].(map[string]any)
	if cat["verification"] != "availability only" || cat["retrieved_at"] != float64(1000) {
		t.Fatalf("availability must use the original wall: %v", cat)
	}
}

// Static explicit routes keep their base even when shadowing a catalog ID;
// llama and dynamic rows stay base-unknown.
func TestFocusShadowAndLlamaBases(t *testing.T) {
	reg := NewRegistry(reviewCatalogCfg(t))
	if err := commitCatalogShim(reg, []CatalogEntry{{Name: "a", Provider: "up", Upstream: "catalog-x"}}, nil, 1000); err != nil {
		t.Fatal(err)
	}
	snap := reg.Current()
	m, _, fault := snap.Resolve("a")
	if fault != Found || m.Upstream != "x" {
		t.Fatalf("explicit must shadow catalog: %+v %v", m, fault)
	}
	if _, ok := snap.CatalogBase("a"); ok {
		t.Fatal("shadowed catalog ID must publish no catalog base")
	}
	if _, ok := snap.CatalogBase("llama-only"); ok {
		t.Fatal("llama/dynamic rows are base-unknown")
	}
}

// Overrides apply after review with conflict evidence; aliases add no rows
// and alias keys never apply.
func TestFocusOverrideAfterReview(t *testing.T) {
	cfg := reviewCatalogCfg(t)
	admitted, err := capability.ValidateOverrides(map[string]any{
		"glm-5.3-flash": map[string]any{"tools": map[string]any{"supported": false}},
		"alias-x":       map[string]any{"streaming": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg.CapabilityOverrides = admitted
	reg := NewRegistry(cfg)
	if err := commitCatalogShim(reg, []CatalogEntry{{Name: "glm-5.3-flash", Provider: "chat", Upstream: "glm-5.3-flash"}}, nil, 1000); err != nil {
		t.Fatal(err)
	}
	snap := reg.Current()
	wire := renderBase(t, mustBase(t, snap, "glm-5.3-flash"), "glm-5.3-flash", snap, 1500)
	got := wire["tools"].(map[string]any)["supported"].(map[string]any)
	if got["value"] != false || got["kind"] != "override" {
		t.Fatalf("override must win after review: %v", got)
	}
	confs, ok := got["conflicts"].([]any)
	if !ok || len(confs) == 0 {
		t.Fatalf("displaced review evidence must be retained: %v", got)
	}
	first := confs[0].(map[string]any)
	if first["source"] != "go.review.tools.supported" || first["kind"] != "reviewed" {
		t.Fatalf("conflict must carry review evidence: %v", first)
	}
	// Alias-keyed overrides never reach the canonical row.
	stream := wire["streaming"].(map[string]any)
	if stream["value"] != nil {
		t.Fatalf("alias keys must not apply: %v", stream)
	}
}

// Stale failure retains routes, bases, wall time and age; valid empty
// inventory replaces instead of retaining.
func TestFocusStaleRetainAndEmptyReplace(t *testing.T) {
	reg := NewRegistry(reviewCatalogCfg(t))
	if err := commitCatalogShim(reg, []CatalogEntry{{Name: "glm-5.3-flash", Provider: "chat", Upstream: "glm-5.3-flash"}}, nil, 1000); err != nil {
		t.Fatal(err)
	}
	before := renderBase(t, mustBase(t, reg.Current(), "glm-5.3-flash"), "glm-5.3-flash", reg.Current(), 1500)
	// Failed probe: routes, bases and wall time retained with error + age.
	if err := reg.CommitCatalog(CatalogAttempt{
		Event: SourceEvent{LastSuccess: reg.CatalogEvent().LastSuccess, Failures: 1, Err: "boom"},
	}); err != nil {
		t.Fatal(err)
	}
	snap := reg.Current()
	after := renderBase(t, mustBase(t, snap, "glm-5.3-flash"), "glm-5.3-flash", snap, 1500)
	if after["tools"].(map[string]any)["supported"].(map[string]any)["value"] != true {
		t.Fatalf("failure must retain review facts: %v", after["tools"])
	}
	if after["sources"].(map[string]any)["catalog"].(map[string]any)["retrieved_at"] != float64(1000) {
		t.Fatalf("failure must retain the original wall, never now: %v", after["sources"])
	}
	_ = before
	// Valid empty inventory replaces.
	if err := commitCatalogShim(reg, nil, nil, 2000); err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Current().CatalogBase("glm-5.3-flash"); ok {
		t.Fatal("valid empty must replace retained bases")
	}
	if len(reg.Current().ListedModels()) != 1 { // only explicit "a"
		t.Fatalf("valid empty must replace routes: %+v", reg.Current().ListedModels())
	}
}

// Restore rebuilds review facts from the asset with the original wall time,
// never from disk facts; cached restore keeps bases with original times.
func TestFocusRestoreRebuildsFromAsset(t *testing.T) {
	reg := NewRegistry(reviewCatalogCfg(t))
	inv := DiscoveryInventory{
		Catalog: &CatalogInventory{
			Entries:     []CatalogEntry{{Name: "glm-5.3-flash", Provider: "chat", Upstream: "glm-5.3-flash"}},
			SucceededAt: Wall(1400),
		},
	}
	if err := reg.RestoreCached(inv); err != nil {
		t.Fatal(err)
	}
	snap := reg.Current()
	wire := renderBase(t, mustBase(t, snap, "glm-5.3-flash"), "glm-5.3-flash", snap, 1500)
	if wire["tools"].(map[string]any)["supported"].(map[string]any)["value"] != true {
		t.Fatalf("restore must rebuild review facts from asset: %v", wire["tools"])
	}
	if wire["sources"].(map[string]any)["catalog"].(map[string]any)["retrieved_at"] != float64(1400) {
		t.Fatalf("restore must use the original wall: %v", wire["sources"])
	}
}

// Config/asset mutation after New cannot move the snapshot; returned bases
// are caller-owned copies.
func TestFocusSnapshotCopyIsolation(t *testing.T) {
	cfg := reviewCatalogCfg(t)
	reg := NewRegistry(cfg)
	cfg.Catalog.MetadataSource = ""
	cfg.Catalog.HasReview = false
	if err := commitCatalogShim(reg, []CatalogEntry{{Name: "glm-5.3-flash", Provider: "chat", Upstream: "glm-5.3-flash"}}, nil, 1000); err != nil {
		t.Fatal(err)
	}
	snap := reg.Current()
	b1, ok := snap.CatalogBase("glm-5.3-flash")
	if !ok {
		t.Fatal("base must survive config mutation")
	}
	_ = b1.Put("tools.supported", capability.BoolValue(false), strPtr("probe"), capability.KindOverride)
	b2, _ := snap.CatalogBase("glm-5.3-flash")
	v, _ := b2.Get("tools.supported")
	if b, _ := v.Bool(); !b {
		t.Fatal("returned base mutation escaped into the snapshot")
	}
}

// Concurrent publication plus base lookups stay stable under -race.
func TestFocusConcurrentPublication(t *testing.T) {
	reg := NewRegistry(reviewCatalogCfg(t))
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = commitCatalogShim(reg, []CatalogEntry{{Name: "glm-5.3-flash", Provider: "chat", Upstream: "glm-5.3-flash"}}, nil, 1000)
			snap := reg.Current()
			if base, ok := snap.CatalogBase("glm-5.3-flash"); ok {
				_ = capability.Render(base, snap.CapabilityOverrides("glm-5.3-flash"), snap.CatalogDiscovery(60, 1500), 1500)
			}
			_ = snap.CatalogDiscovery(60, 1500)
			_ = snap.ListedModels()
		}()
	}
	wg.Wait()
}

func mustBase(t *testing.T, snap *Snapshot, name string) capability.Contract {
	t.Helper()
	base, ok := snap.CatalogBase(name)
	if !ok {
		t.Fatalf("missing base for %q", name)
	}
	return base
}

func strPtr(s string) *string { v := s; return &v }
