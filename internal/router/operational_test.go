package router

import (
	"math"
	"sync"
	"testing"

	"github.com/arhammahajan/router/internal/config"
)

// Operational proofs: typed copy boundaries, invalid-pub rollback,
// concurrent both-layer coherence, alias-vs-canon counts, exact nullable
// shapes for initial/empty/partial/stale/cache reports and fake-clock
// freshness. All times hermetic via FakeClock; no sleeps.

func TestOperationalAliasCountsVsCanon(t *testing.T) {
	s := New(testConfig())
	rd := s.Readiness()
	if rd["enabled_routes"] != 2 || rd["usable_routes"] != 2 {
		t.Fatalf("named keys must include aliases: %+v", rd)
	}
	if got := len(s.Models()); got != 1 {
		t.Fatalf("models must list public canonicals only: %d", got)
	}
	if rd["status"] != "ready" || rd["ready"] != true || rd["reason"] != "usable_routes" {
		t.Fatalf("ready shape wrong: %+v", rd)
	}
	per := s.ProviderEnabledRoutes()
	if per["up"] != 2 {
		t.Fatalf("per-provider keys must include aliases: %+v", per)
	}
}

func TestOperationalCommitFailureRetainsRoutes(t *testing.T) {
	reg := NewRegistry(catalogCfg())
	entries := []CatalogEntry{{Name: "cat-1", Provider: "up", Upstream: "cat-1"}}
	if err := commitCatalogShim(reg, entries, []string{"ghost"}, 100); err != nil {
		t.Fatal(err)
	}
	before := reg.Current()
	// Invalid success (unknown provider) rolls back entirely.
	bad := CatalogAttempt{
		Event:   SourceEvent{Failures: 1, Err: "bad"},
		Success: &CatalogSuccess{Entries: []CatalogEntry{{Name: "x", Provider: "nope", Upstream: "x"}}, Wall: Wall(200)},
	}
	if err := reg.CommitCatalog(bad); err == nil {
		t.Fatal("unknown provider must reject the attempt")
	}
	if reg.Current() != before {
		t.Fatal("invalid success must not swap the pointer")
	}
	// Failure attempt (as computed by a worker: prior last success carried
	// forward) keeps routes, wall times and unmapped, updates status.
	fail := CatalogAttempt{
		Event: SourceEvent{LastSuccess: MonoPtr(Mono(100)), LastAttempt: MonoPtr(Mono(50)), Failures: 1, Err: "boom"},
	}
	if err := reg.CommitCatalog(fail); err != nil {
		t.Fatal(err)
	}
	snap := reg.Current()
	if snap == before {
		t.Fatal("failure must publish a status update")
	}
	if _, _, fault := snap.Resolve("cat-1"); fault != Found {
		t.Fatal("failure must retain routes")
	}
	if got := snap.unmappedModels(); len(got) != 1 || got[0] != "ghost" {
		t.Fatalf("failure must retain unmapped: %v", got)
	}
	if inv := reg.ExportDiscovery(); inv.Catalog == nil || inv.Catalog.SucceededAt != Wall(100) {
		t.Fatalf("failure must retain wall time: %+v", inv.Catalog)
	}
	if got := reg.CatalogEvent(); got.Failures != 1 || got.Err != "boom" {
		t.Fatalf("failure status wrong: %+v", got)
	}
	if got := reg.CatalogEvent().LastSuccess; got == nil || *got != Mono(100) {
		t.Fatalf("failure must retain last success: %+v", got)
	}
}

func TestOperationalConcurrentBothLayersOnePin(t *testing.T) {
	reg := NewRegistry(llamaCfg())
	catEntries := func(tag string) []CatalogEntry {
		return []CatalogEntry{{Name: tag, Provider: "up", Upstream: tag}}
	}
	llamaLayerFor := func(tag string) LlamaLayer {
		return LlamaLayer{Servers: map[string]LlamaServerSnapshot{
			"llama-a": {State: LlamaReachable, Entries: []LlamaEntry{{Name: tag, Upstream: tag}}},
			"llama-b": {State: LlamaNeverSucceeded},
		}}
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = commitCatalogShim(reg, catEntries("cat-2"), nil, 1000)
		}()
		go func() {
			defer wg.Done()
			_ = commitLlamaShim(reg, llamaLayerFor("lm-2"), 1000)
		}()
	}
	// Concurrent coherent readers pin one snapshot each.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snap := reg.Current()
			_ = snap.Readiness()
			_ = reg.CatalogEvent()
			_ = reg.LlamaAggregate()
			_ = reg.ExportDiscovery()
		}()
	}
	wg.Wait()
	snap := reg.Current()
	if _, _, fault := snap.Resolve("cat-2"); fault != Found {
		t.Fatal("concurrent catalog publication lost")
	}
	if _, _, fault := snap.Resolve("lm-2"); fault != Found {
		t.Fatal("concurrent llama publication lost")
	}
}

func TestOperationalInitialEmptyPartialStaleCache(t *testing.T) {
	fc := &FakeClock{Mono: 1000, Wall: 2000}
	reg := NewRegistryWithClock(llamaCfg(), fc)

	// Initial: never succeeded, nullable times, unready without named hits.
	zero := reg.Current()
	if _, ok := zero.Readiness()["ready"]; !ok {
		t.Fatal("readiness must carry ready")
	}
	cat := zero.CatalogStatus(true, "http://127.0.0.1:9/models", reg.Sanitize, 60, fc.Mono)
	if cat["loaded"] != false || cat["last_success"] != nil {
		t.Fatalf("initial catalog must be unloaded with null times: %+v", cat["refresh"])
	}
	for _, k := range []string{"enabled", "url", "models", "loaded", "error", "cached", "succeeded_at", "refresh", "unmapped_models"} {
		if _, ok := cat[k]; !ok {
			t.Fatalf("catalog_status missing key %q", k)
		}
	}

	// Valid empty success replaces: loaded with zero models.
	if err := reg.CommitCatalog(CatalogAttempt{
		Event:   SourceEvent{LastSuccess: MonoPtr(fc.Mono), LastAttempt: MonoPtr(fc.Mono)},
		Success: &CatalogSuccess{Wall: fc.Wall},
	}); err != nil {
		t.Fatal(err)
	}
	got := reg.Current().CatalogStatus(true, "http://127.0.0.1:9/models", reg.Sanitize, 60, fc.Mono)
	if got["loaded"] != true || got["models"] != 0 {
		t.Fatalf("empty success must load with zero models: %+v", got)
	}

	// Partial fleet: one reachable, one initial failure.
	reachMode := "single"
	if err := reg.CommitLlama(LlamaAttempt{
		Layer: LlamaLayer{Servers: map[string]LlamaServerSnapshot{
			"llama-a": {State: LlamaReachable, Entries: []LlamaEntry{{Name: "lm-a", Upstream: "lm-a"}}},
			"llama-b": {State: LlamaNeverSucceeded},
		}},
		AtWall:    fc.Wall,
		Aggregate: SourceEvent{LastSuccess: MonoPtr(fc.Mono), LastAttempt: MonoPtr(fc.Mono)},
		Servers: map[string]ServerEvent{
			"llama-a": {URL: "http://127.0.0.1:9931", Mode: &reachMode, Models: 1,
				SourceEvent: SourceEvent{LastSuccess: MonoPtr(fc.Mono), LastAttempt: MonoPtr(fc.Mono)}},
			"llama-b": {URL: "http://127.0.0.1:9932",
				SourceEvent: SourceEvent{LastAttempt: MonoPtr(fc.Mono), Failures: 1, Err: "x: probe failed"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	st := reg.Current().LlamaStatus(true, reg.LlamaOrder(), 30, fc.Mono, reg.Sanitize)
	servers, _ := st["servers"].([]any)
	if len(servers) != 2 {
		t.Fatalf("complete fleet must report both servers: %+v", st)
	}
	byName := map[string]map[string]any{}
	for _, s := range servers {
		m := s.(map[string]any)
		byName[m["provider"].(string)] = m
	}
	a, b := byName["llama-a"], byName["llama-b"]
	if a["reachable"] != true || a["stale"] != false {
		t.Fatalf("reachable server wrong: %+v", a)
	}
	if b["reachable"] != false || b["stale"] != false {
		t.Fatalf("initial failure must report stale:false: %+v", b)
	}
	if b["mode"] != nil || b["last_success"] != nil || b["succeeded_at"] != nil {
		t.Fatalf("initial failure must carry nulls: %+v", b)
	}
	wantKeys := []string{"url", "reachable", "mode", "provider", "models", "stale", "error",
		"last_success", "last_attempt", "consecutive_failures", "cached", "succeeded_at",
		"age_seconds", "stale_age_seconds"}
	for _, m := range byName {
		for _, k := range wantKeys {
			if _, ok := m[k]; !ok {
				t.Fatalf("per-server missing key %q: %+v", k, m)
			}
		}
	}
	agg := reg.LlamaAggregate()
	if agg.LastSuccess == nil {
		t.Fatal("aggregate must carry the reachable last success")
	}
}

func TestOperationalRetainedFailureStaleAndUsable(t *testing.T) {
	fc := &FakeClock{Mono: 100, Wall: 1000}
	reg := NewRegistryWithClock(llamaCfg(), fc)
	mode := "single"
	if err := reg.CommitLlama(LlamaAttempt{
		Layer: LlamaLayer{Servers: map[string]LlamaServerSnapshot{
			"llama-a": {State: LlamaReachable, Entries: []LlamaEntry{{Name: "lm-a", Upstream: "lm-a"}}},
			"llama-b": {State: LlamaNeverSucceeded},
		}},
		AtWall:    fc.Wall,
		Aggregate: SourceEvent{LastSuccess: MonoPtr(fc.Mono), LastAttempt: MonoPtr(fc.Mono)},
		Servers: map[string]ServerEvent{
			"llama-a": {Mode: &mode, Models: 1,
				SourceEvent: SourceEvent{LastSuccess: MonoPtr(fc.Mono), LastAttempt: MonoPtr(fc.Mono)}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// Later failure retains the prior per-server time and marks stale.
	fc.Advance(10)
	if err := reg.CommitLlama(LlamaAttempt{
		Layer: LlamaLayer{Servers: map[string]LlamaServerSnapshot{
			"llama-a": {State: LlamaUnreachableStale, Entries: []LlamaEntry{{Name: "lm-a", Upstream: "lm-a"}}},
			"llama-b": {State: LlamaNeverSucceeded},
		}},
		AtWall: fc.Wall,
		Aggregate: SourceEvent{LastSuccess: MonoPtr(Mono(100)), LastAttempt: MonoPtr(fc.Mono),
			NextAttempt: MonoPtr(Mono(120)), Failures: 1, Err: "probe failed"},
		Servers: map[string]ServerEvent{
			"llama-a": {Models: 1, SourceEvent: SourceEvent{
				LastSuccess: MonoPtr(Mono(100)), LastAttempt: MonoPtr(fc.Mono), Failures: 1, Err: "probe failed"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	snap := reg.Current()
	if _, _, fault := snap.Resolve("lm-a"); fault != Found {
		t.Fatal("stale routes must still resolve")
	}
	if got := snap.UsableRoutes(); got != 2 {
		t.Fatalf("stale provider must be unusable but explicit kept: %d", got)
	}
	st := snap.LlamaStatus(true, reg.LlamaOrder(), 30, fc.Mono, reg.Sanitize)
	for _, s := range st["servers"].([]any) {
		m := s.(map[string]any)
		if m["provider"] == "llama-a" {
			if m["stale"] != true || m["reachable"] != false {
				t.Fatalf("retained failure must be stale: %+v", m)
			}
			if m["succeeded_at"] != float64(1000) {
				t.Fatalf("retained failure must keep original wall time: %+v", m)
			}
		}
	}
}

func TestOperationalFakeClockAgeAndRestoreMapping(t *testing.T) {
	fc := &FakeClock{Mono: 500, Wall: 1500}
	reg := NewRegistryWithClock(catalogCfg(), fc)
	before := captureWall(fc)
	_ = before
	// Restore maps wall age onto mono: monoSuccess = nowMono-(nowWall-at).
	if err := reg.RestoreCached(DiscoveryInventory{
		Catalog: &CatalogInventory{Entries: []CatalogEntry{{Name: "r", Provider: "up", Upstream: "r"}}, SucceededAt: Wall(1400)},
	}); err != nil {
		t.Fatal(err)
	}
	ev := reg.CatalogEvent()
	if ev.LastSuccess == nil || math.Abs(float64(*ev.LastSuccess)-400) > 1e-9 {
		t.Fatalf("restored mono must map wall age: %+v", ev.LastSuccess)
	}
	// Freshness derives from one pinned sample: age 100 at +100.
	fc.Advance(100)
	rep := RefreshReport(reg.CatalogEvent(), 60, fc.Mono)
	if rep["age_seconds"] != 200.0 {
		t.Fatalf("age must derive from pinned now: %+v", rep)
	}
	if rep["stale"] != true {
		t.Fatalf("age beyond interval must be stale: %+v", rep)
	}
	// Restored mono may be negative when the wall time predates the epoch.
	old := &FakeClock{Mono: 10, Wall: 100}
	reg2 := NewRegistryWithClock(catalogCfg(), old)
	if err := reg2.RestoreCached(DiscoveryInventory{
		Catalog: &CatalogInventory{Entries: []CatalogEntry{{Name: "r", Provider: "up", Upstream: "r"}}, SucceededAt: Wall(0)},
	}); err != nil {
		t.Fatal(err)
	}
	if got := reg2.CatalogEvent().LastSuccess; got == nil || *got != Mono(-90) {
		t.Fatalf("restored mono may be negative: %+v", got)
	}
}

func captureWall(fc *FakeClock) Sample { return fc.Sample() }

func TestOperationalCacheEventShape(t *testing.T) {
	cfg := catalogCfg()
	cfg.DiscoveryCache = &config.DiscoveryCacheConfig{Path: "/tmp/x.json", MaxAgeSeconds: 60}
	reg := NewRegistry(cfg)
	ev := reg.Current().CacheEvent()
	if ev.Load != "missing" || !ev.Enabled || ev.WriteErr != nil || ev.Expired != nil {
		t.Fatalf("initial cache event wrong: %+v", ev)
	}
	n := 0
	if err := reg.RestoreCachedWithStatus(DiscoveryInventory{}, "empty", &n); err != nil {
		t.Fatal(err)
	}
	if got := reg.Current().CacheEvent(); got.Load != "empty" || got.Expired == nil || *got.Expired != 0 {
		t.Fatalf("empty restore must carry expired_sources incl zero: %+v", got)
	}
	msg := "cache write failed"
	reg.NoteCacheWriteErr(&msg)
	if got := reg.Current().CacheEvent(); got.WriteErr == nil || got.Expired == nil {
		t.Fatalf("write error must preserve expired presence: %+v", got)
	}
	reg.NoteCacheWriteErr(nil)
	if got := reg.Current().CacheEvent(); got.WriteErr != nil || got.Expired == nil {
		t.Fatalf("write clear must preserve expired presence: %+v", got)
	}
	// Status-only missing outcome keeps routes and wall times.
	inv := reg.ExportDiscovery()
	reg.NoteCacheStatus("missing", nil)
	if got := reg.ExportDiscovery(); len(got.Llama) != len(inv.Llama) || (got.Catalog == nil) != (inv.Catalog == nil) {
		t.Fatal("status-only note must keep staged inventory")
	}
	if got := reg.Current().CacheEvent(); got.Load != "missing" || got.Expired != nil {
		t.Fatalf("missing must drop expired presence: %+v", got)
	}
}
