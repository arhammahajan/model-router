// Final bounded acceptance review: single-writer lifecycle, restore
// handoff, retention seeding, single public API and encoding cross-guards.
//
// New file only; no production edits. Hermetic tempdirs, controlled HOME,
// fakes only. Each test is an executable RED guard for a load-bearing root
// lifecycle property; process-owned FS latency, route-only schema-2
// incompatibility, metadata omission and stale/readiness semantics are
// declared trades and are not re-reported here.
package discovery

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arhammahajan/router/internal/router"
)

func finalCatalogAt(t *testing.T, path string) float64 {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("cache JSON: %v", err)
	}
	cat, _ := doc["catalog"].(map[string]any)
	if cat == nil {
		t.Fatalf("catalog unit missing: %s", raw)
	}
	at, _ := cat["succeeded_at"].(float64)
	return at
}

func finalPublishCatalog(t *testing.T, reg *router.Registry, at float64) {
	t.Helper()
	if err := commitCatalogShim(reg, []router.CatalogEntry{{Name: "m1", Provider: "up", Upstream: "m1"}}, nil, at); err != nil {
		t.Fatal(err)
	}
}

// Concurrent Start calls must still yield exactly one writer: the second
// notification coalesces while the first transaction is blocked, and no
// second writer enters.
func TestFinalReviewConcurrentStartKeepsSingleWriter(t *testing.T) {
	cfg := cacheTestConfig(t, t.TempDir())
	store := NewCacheStore(cfg, router.NewRegistry(cfg))
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	store.beforeEncode = func() { entered <- struct{}{}; <-release }
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); store.Start() }()
	}
	wg.Wait()
	store.NotifyDirty()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("single writer did not start")
	}
	store.NotifyDirty()
	select {
	case <-entered:
		t.Fatal("repeated Start created a concurrent writer")
	case <-time.After(100 * time.Millisecond):
	}
	once.Do(func() { close(release) })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	store.Close(ctx)
}

// CLI path never calls Start: queued notifications must still flush exactly
// once under Close instead of being dropped.
func TestFinalReviewCloseFlushesPendingCLIQueue(t *testing.T) {
	cfg := cacheTestConfig(t, t.TempDir())
	reg := router.NewRegistry(cfg)
	finalPublishCatalog(t, reg, 100)
	store := NewCacheStore(cfg, reg)
	store.NotifyDirty()
	store.NotifyDirty()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	store.Close(ctx)
	select {
	case <-store.done:
	case <-time.After(2 * time.Second):
		t.Fatal("CLI-queued flush did not complete")
	}
	if _, err := os.Stat(cfg.DiscoveryCache.Path); err != nil {
		t.Fatalf("Close dropped the pending CLI save: %v", err)
	}
	if got := finalCatalogAt(t, cfg.DiscoveryCache.Path); got != 100 {
		t.Fatalf("CLI flush wrote wrong capture: %v", got)
	}
}

// An expired Close deadline must not start a post-deadline flush, and a
// repeated deferred Close must return immediately without extending the
// network drain. The in-flight transaction may finish (atomic old-or-new)
// but the pending newer capture is skipped.
func TestFinalReviewExpiredCloseSkipsPostDeadlineFlush(t *testing.T) {
	cfg := cacheTestConfig(t, t.TempDir())
	reg := router.NewRegistry(cfg)
	finalPublishCatalog(t, reg, 100)
	store := NewCacheStore(cfg, reg)
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var saves atomic.Int32
	store.beforeEncode = func() { entered <- struct{}{}; <-release }
	store.beforeReplace = func() { saves.Add(1) }
	store.Start()
	store.NotifyDirty()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("writer did not start")
	}
	finalPublishCatalog(t, reg, 200)
	store.NotifyDirty()
	first, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	store.Close(first)
	if time.Since(start) > 3*time.Second {
		t.Fatal("Close did not respect its own deadline")
	}
	second, cancelSecond := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelSecond()
	returned := make(chan struct{})
	go func() { store.Close(second); close(returned) }()
	select {
	case <-returned:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("repeated deferred Close extended the drain budget")
	}
	once.Do(func() { close(release) })
	select {
	case <-store.done:
	case <-time.After(3 * time.Second):
		t.Fatal("writer did not exit")
	}
	// In-flight save may land; the pending post-deadline capture must not.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(cfg.DiscoveryCache.Path); err == nil {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if n := saves.Load(); n != 1 {
		t.Fatalf("post-deadline pending flush must be skipped: saves=%d", n)
	}
	if got := finalCatalogAt(t, cfg.DiscoveryCache.Path); got != 100 {
		t.Fatalf("older capture must not be overtaken after deadline: %v", got)
	}
}

// While the writer is blocked, a newer notification must capture the latest
// export after the in-flight transaction: older captures never overtake.
func TestFinalReviewLatestCaptureWinsWhileBlocked(t *testing.T) {
	cfg := cacheTestConfig(t, t.TempDir())
	reg := router.NewRegistry(cfg)
	finalPublishCatalog(t, reg, 100)
	store := NewCacheStore(cfg, reg)
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var saves atomic.Int32
	store.beforeEncode = func() { entered <- struct{}{}; <-release }
	store.beforeReplace = func() { saves.Add(1) }
	store.Start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		store.Close(ctx)
	}()
	store.NotifyDirty()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("writer did not start")
	}
	finalPublishCatalog(t, reg, 200)
	store.NotifyDirty()
	once.Do(func() { close(release) })
	deadline := time.Now().Add(5 * time.Second)
	for {
		if data, err := os.ReadFile(cfg.DiscoveryCache.Path); err == nil {
			var doc map[string]any
			if json.Unmarshal(data, &doc) == nil {
				if cat, ok := doc["catalog"].(map[string]any); ok && cat != nil {
					if at, _ := cat["succeeded_at"].(float64); at == 200 {
						break
					}
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("newer save never landed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Both transactions ran; poll until the second replace settles.
	deadline = time.Now().Add(3 * time.Second)
	for saves.Load() != 2 && !time.Now().After(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := saves.Load(); n != 2 {
		t.Fatalf("blocked writer must flush the pending latest capture: saves=%d", n)
	}
}

// The dirty slot is depth-1 and coalesced; NotifyDirty never blocks and is
// rejected after Close.
func TestFinalReviewNotifyCoalescedAndPostCloseRejected(t *testing.T) {
	cfg := cacheTestConfig(t, t.TempDir())
	store := NewCacheStore(cfg, router.NewRegistry(cfg))
	if cap(store.dirty) != 1 {
		t.Fatalf("dirty channel must be depth-1, got %d", cap(store.dirty))
	}
	for i := 0; i < 100; i++ {
		store.NotifyDirty()
	}
	if n := len(store.dirty); n > 1 {
		t.Fatalf("notifications must coalesce to one slot, got %d", n)
	}
	store.Start()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	store.Close(ctx)
	select {
	case <-store.done:
	case <-time.After(2 * time.Second):
		t.Fatal("writer did not exit")
	}
	// Drain any flushed pending slot, then prove post-close rejection.
	select {
	case <-store.dirty:
	default:
	}
	store.NotifyDirty()
	select {
	case <-store.dirty:
		t.Fatal("post-close must reject new notifications")
	default:
	}
}

// A canceled startup load publishes nothing, even when the helper was
// already blocked in the filesystem call.
func TestFinalReviewRestoreCanceledNeverPublishesLate(t *testing.T) {
	cfg := cacheTestConfig(t, t.TempDir())
	reg := router.NewRegistry(cfg)
	now := float64(wallNow())
	store := NewCacheStore(cfg, reg)
	before := captureInv(reg)
	writeFile(t, cfg.DiscoveryCache.Path, cacheDoc(store.identity,
		map[string]any{"succeeded_at": now, "models": []any{map[string]any{"id": "m1", "aliases": []any{}}}}, map[string]any{}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if st := store.Restore(ctx, nil); st.Load != CacheSkipped {
		t.Fatalf("canceled must skip, got %+v", st)
	}
	before.assertUnchanged(t, reg)

	// Block the helper mid-read, cancel, then release: the late FS result
	// must still publish nothing.
	orig := cacheOpenRead
	release := make(chan struct{})
	helperDone := make(chan struct{})
	var once sync.Once
	releaseFn := func() { once.Do(func() { close(release) }) }
	defer func() {
		releaseFn()
		select {
		case <-helperDone:
		case <-time.After(5 * time.Second):
		}
		cacheOpenRead = orig
	}()
	cacheOpenRead = func(path string) (*os.File, error) {
		<-release
		defer close(helperDone)
		return orig(path)
	}
	reg2 := router.NewRegistry(cfg)
	store2 := NewCacheStore(cfg, reg2)
	before2 := captureInv(reg2)
	cctx, ccancel := context.WithCancel(context.Background())
	done := make(chan CacheStatus, 1)
	go func() { done <- store2.Restore(cctx, nil) }()
	time.Sleep(100 * time.Millisecond)
	ccancel()
	select {
	case st := <-done:
		if st.Load != CacheSkipped {
			t.Fatalf("cancel during load must skip, got %+v", st)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Restore did not honor cancel")
	}
	releaseFn()
	select {
	case <-helperDone:
	case <-time.After(5 * time.Second):
		t.Fatal("helper did not finish after release")
	}
	time.Sleep(200 * time.Millisecond)
	before2.assertUnchanged(t, reg2)
	if store2.Status().Load != CacheSkipped {
		t.Fatalf("late helper must not overwrite skipped status: %+v", store2.Status())
	}
}

// The startup 2s handoff bounds a stalled read: it skips with an empty seed
// and the late result never publishes.
func TestFinalReviewRestoreHandoffBoundsStalledRead(t *testing.T) {
	cfg := cacheTestConfig(t, t.TempDir())
	reg := router.NewRegistry(cfg)
	store := NewCacheStore(cfg, reg)
	before := captureInv(reg)
	orig := cacheOpenRead
	release := make(chan struct{})
	helperDone := make(chan struct{})
	var once sync.Once
	releaseFn := func() { once.Do(func() { close(release) }) }
	defer func() {
		releaseFn()
		select {
		case <-helperDone:
		case <-time.After(5 * time.Second):
		}
		cacheOpenRead = orig
	}()
	cacheOpenRead = func(path string) (*os.File, error) {
		<-release
		defer close(helperDone)
		return orig(path)
	}
	start := time.Now()
	st := store.Restore(context.Background(), nil)
	if elapsed := time.Since(start); elapsed > 12*time.Second {
		t.Fatalf("2s handoff exceeded: %v", elapsed)
	}
	if st.Load != CacheSkipped {
		t.Fatalf("stalled read must skip, got %+v", st)
	}
	releaseFn()
	select {
	case <-helperDone:
	case <-time.After(5 * time.Second):
		t.Fatal("helper did not finish after release")
	}
	time.Sleep(200 * time.Millisecond)
	before.assertUnchanged(t, reg)
	if store.Status().Load != CacheSkipped {
		t.Fatalf("late helper must not overwrite skipped status: %+v", store.Status())
	}
}

// Retention seeding happens only for an accepted atomic restore: a rejected
// file must not activate any provider in the source worker.
func TestFinalReviewSeedRetainedOnlyOnAcceptedRestore(t *testing.T) {
	cfg := cacheTestConfig(t, t.TempDir())
	now := float64(wallNow())
	reg := router.NewRegistry(cfg)
	store := NewCacheStore(cfg, reg)
	lw := NewLlamaWorker(cfg.Llama, reg)
	before := captureInv(reg)
	writeFile(t, cfg.DiscoveryCache.Path, cacheDoc(store.identity, nil,
		map[string]any{"99": map[string]any{"succeeded_at": now, "models": []any{}}}))
	if st := store.Restore(context.Background(), lw); st.Load != CacheInvalid {
		t.Fatalf("unknown index must be invalid, got %+v", st)
	}
	before.assertUnchanged(t, reg)
	if len(lw.active) != 0 || len(lw.retained) != 0 {
		t.Fatalf("rejected restore must not seed retention: %+v %+v", lw.active, lw.retained)
	}

	reg2 := router.NewRegistry(cfg)
	store2 := NewCacheStore(cfg, reg2)
	lw2 := NewLlamaWorker(cfg.Llama, reg2)
	writeFile(t, cfg.DiscoveryCache.Path, cacheDoc(store2.identity, nil,
		map[string]any{"0": map[string]any{"succeeded_at": now, "models": []any{
			map[string]any{"id": "m1", "aliases": []any{}},
		}}}))
	if st := store2.Restore(context.Background(), lw2); st.Load != CacheLoaded {
		t.Fatalf("valid restore must load, got %+v", st)
	}
	srv := cfg.Llama.Servers[0].Name
	if !lw2.active[srv] || len(lw2.retained[srv]) != 1 {
		t.Fatalf("accepted restore must seed retention: %+v %+v", lw2.active, lw2.retained)
	}
	if m, _, fault := reg2.Current().Resolve(cfg.Llama.Prefix + "m1"); fault != router.Found || m.Upstream != "m1" {
		t.Fatalf("seeded route must resolve: %+v %v", m, fault)
	}
}

// No public save-from-old-snapshot API may exist: the owner exposes exactly
// Status/Restore/NotifyDirty/Start/Close, and no StartupRestore,
// LoadWithDeadline, SyncSave or Save variant.
func TestFinalReviewNoExtraPublicSaveAPI(t *testing.T) {
	typ := reflect.TypeOf(&CacheStore{})
	allowed := map[string]bool{
		"Status": true, "Restore": true, "NotifyDirty": true, "Start": true, "Close": true,
	}
	for i := 0; i < typ.NumMethod(); i++ {
		if name := typ.Method(i).Name; !allowed[name] {
			t.Fatalf("unexpected public CacheStore method %q", name)
		}
	}
	for _, name := range []string{
		"StartupRestore", "LoadWithDeadline", "SyncSave", "Save", "SaveOnce", "Flush", "StartWriter", "Stop",
	} {
		if _, ok := typ.MethodByName(name); ok {
			t.Fatalf("forbidden second-writer API %q must not exist", name)
		}
	}
}

// Cross-guard sanity: full public-name alias admission, local rename,
// strict UTF-8/surrogate JSON, generic credential classes and once-decoded
// URL secrets. The forward alias-vs-canonical collision is accepted at the
// pure file level (raw upstream namespaces) and rejected by the trusted
// config-aware restore using the full pre-b name.
func TestFinalReviewAliasUTF8CredentialCrossGuards(t *testing.T) {
	dir := t.TempDir()
	cfg := cacheTestConfig(t, dir)
	p := cfg.Llama.Prefix
	now := float64(wallNow())

	// File level is prefix-agnostic: raw ids a/b with alias pre-b decode.
	fwd := map[string]any{"succeeded_at": now, "models": []any{
		map[string]any{"id": "a", "aliases": []any{p + "b"}},
		map[string]any{"id": "b", "aliases": []any{}},
	}}
	if _, st, err := LoadDiscoveryCache(writeFinalDoc(t, cfg.DiscoveryCache.Path, cacheDoc("i", nil, map[string]any{"0": fwd})), 86400, now); err != nil || (st.Load != CacheLoaded && st.Load != CacheEmpty) {
		t.Fatalf("file-level forward rows must decode, got %+v %v", st, err)
	}
	// Trusted restore sees the full public-name collision and rejects.
	reg := router.NewRegistry(cfg)
	fwdStore := NewCacheStore(cfg, reg)
	before := captureInv(reg)
	writeFile(t, cfg.DiscoveryCache.Path, cacheDoc(fwdStore.identity, nil, map[string]any{"0": fwd}))
	if st := fwdStore.Restore(context.Background(), nil); st.Load != CacheInvalid {
		t.Fatalf("forward full-name collision must be invalid via trusted restore, got %+v", st)
	}
	before.assertUnchanged(t, reg)

	// Local rename: raw "local" stored, staged prefix+"local-llama" resolves.
	reg2 := router.NewRegistry(cfg)
	store2 := NewCacheStore(cfg, reg2)
	rawLocal := map[string]any{"succeeded_at": now, "models": []any{
		map[string]any{"id": "local", "aliases": []any{}},
	}}
	writeFile(t, cfg.DiscoveryCache.Path, cacheDoc(store2.identity, nil, map[string]any{"0": rawLocal}))
	if st := store2.Restore(context.Background(), nil); st.Load != CacheLoaded {
		t.Fatalf("local id must restore, got %+v", st)
	}
	if m, _, fault := reg2.Current().Resolve(p + "local-llama"); fault != router.Found || m.Upstream != "local" {
		t.Fatalf("local rename broken: %+v %v", m, fault)
	}

	// Bare alias without the exposed prefix fails trusted restore.
	bare := map[string]any{"succeeded_at": now, "models": []any{
		map[string]any{"id": "m1", "aliases": []any{"bare-alias"}},
	}}
	reg3 := router.NewRegistry(cfg)
	store3 := NewCacheStore(cfg, reg3)
	writeFile(t, cfg.DiscoveryCache.Path, cacheDoc(store3.identity, nil, map[string]any{"0": bare}))
	if st := store3.Restore(context.Background(), nil); st.Load != CacheInvalid {
		t.Fatalf("bare alias must fail trusted restore, got %+v", st)
	}

	// Strict UTF-8 and surrogate JSON.
	if _, err := decodeJSONObject(append([]byte(`{"schema_version":2,"identity":"i","catalog":null,"llama":{}}`), 0xff)); err == nil {
		t.Fatal("invalid UTF-8 bytes must be rejected")
	}
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{`{"id":"m` + "\U0001F600" + `"}`, true},
		{`{"id":"` + "m\\ud83d\\ude00" + `"}`, true},
		{`{"id":"` + "\\ud800" + `"}`, false},
		{`{"id":"` + "\\udc00" + `"}`, false},
	} {
		_, err := decodeJSONObject([]byte(tc.raw))
		if (err == nil) != tc.want {
			t.Fatalf("surrogate admission for %q: err=%v", tc.raw, err)
		}
	}

	// Generic credential classes and corpus echoes fail; benign IDs pass.
	sanStore := reviewSanitizerStore(t, t.TempDir(), "secret-key-1")
	for _, id := range []string{"Bearer abc123", "api_key=supersecret", "sk-abc123XYZ", "a\x00b", "secret-key-1", "qsecret1", "pass1"} {
		if sanStore.sanitize.safeID(id) {
			t.Fatalf("credential echo %q must fail admission", id)
		}
	}
	for _, id := range []string{"m1", "acme/model-2", p + "m1-alias", "local"} {
		if !sanStore.sanitize.safeID(id) {
			t.Fatalf("benign id %q must pass admission", id)
		}
	}

	// URL userinfo is decoded exactly once into the corpus.
	cfgU := cacheTestConfig(t, t.TempDir())
	cfgU.Catalog.URL = "http://user%252ffoo:p%252bword@example.test/catalog?q=query%252fvalue"
	sanU := newCacheSanitizer(cfgU)
	for _, secret := range []string{"user%2ffoo", "p%2bword", "query%2fvalue"} {
		if sanU.safeID(secret) {
			t.Fatalf("once-decoded URL secret %q passed admission", secret)
		}
	}
}

func writeFinalDoc(t *testing.T, path string, v any) string {
	t.Helper()
	writeFile(t, path, v)
	return path
}
