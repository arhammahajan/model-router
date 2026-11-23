// Independent adversarial review: fetch mapping precedence and worker
// lifecycle (goroutine ownership). All probes use the real packages only.
package discovery

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"github.com/arhammahajan/router/internal/catalog"
	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/router"
)

// TestReviewExactMappingBeatsRuleWhenBothMatch pins "exact model_providers
// first, else first-rule search" for the overlapping case: the exact entry
// and a catch-all rule both cover "exact-model". Python oracle is
// discovery._catalog_provider_name (model_providers.get wins). The existing
// schema test never overlaps exact and rule on one ID.
func TestReviewExactMappingBeatsRuleWhenBothMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id": "exact-model"}, {"id": "other"}]`))
	}))
	defer srv.Close()
	first, err := catalog.Compile("o")
	if err != nil {
		t.Fatal(err)
	}
	second, err := catalog.Compile(".*")
	if err != nil {
		t.Fatal(err)
	}
	cat := testCatalog(srv.URL)
	cat.Rules = []config.CatalogRule{
		{Match: "o", Provider: "chat", Pattern: first},
		{Match: ".*", Provider: "up", Pattern: second},
	}
	entries, unmapped, err := FetchMapped(context.Background(), fetchClient(5*time.Second), cat, testProviders())
	if err != nil {
		t.Fatal(err)
	}
	if len(unmapped) != 0 {
		t.Fatalf("catch-all rule must leave nothing unmapped: %v", unmapped)
	}
	byName := map[string]router.CatalogEntry{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	// exact-model: exact table ("up") beats even the first matching rule ("chat").
	if byName["exact-model"].Provider != "up" {
		t.Fatalf("exact must beat rule: %+v", byName["exact-model"])
	}
	// other: no exact entry, so the FIRST matching rule wins ("chat", not "up").
	if byName["other"].Provider != "chat" {
		t.Fatalf("first rule must win: %+v", byName["other"])
	}
}

// TestReviewNonMappingMetadataStaysEmpty pins that list/string metadata rows
// map to empty (wire-normalized to {} by /v1/models), matching Python's
// "dict(metadata) if isinstance(metadata, Mapping) else {}". The nested
// mapping form must survive verbatim (numbers preserved exactly).
func TestReviewNonMappingMetadataStaysEmpty(t *testing.T) {
	raw := []byte(`[{"id":"m","metadata":{"n":9007199254740993}},{"id":"l","metadata":[1]},{"id":"s","metadata":"x"}]`)
	rows, err := decodeRows(raw)
	if err != nil {
		t.Fatal(err)
	}
	cat := testCatalog("http://127.0.0.1:9")
	cat.ModelProviders = map[string]string{"m": "up", "l": "up", "s": "up"}
	entries, _, err := mapRows(context.Background(), rows, cat, testProviders())
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]router.CatalogEntry{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	if got := string(byName["m"].Metadata); got != `{"n":9007199254740993}` {
		t.Fatalf("mapping metadata must survive verbatim: %s", got)
	}
	if len(byName["l"].Metadata) != 0 || len(byName["s"].Metadata) != 0 {
		t.Fatalf("non-mapping metadata must stay empty: %+v", byName)
	}
}

// TestReviewWorkerStopJoinsWithoutGoroutineGrowth pins the one-way shutdown:
// RefreshOnce links the worker lifetime into each attempt via a helper
// goroutine that must exit on attempt end or Stop. Bounded settle loop, no
// long sleeps: failure is a leak, not slowness.
func TestReviewWorkerStopJoinsWithoutGoroutineGrowth(t *testing.T) {
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()
	cfg, providers := workerCfg(t, "http://127.0.0.1:9")
	for i := 0; i < 5; i++ {
		w := NewCatalogWorker(cfg.Catalog, providers, router.NewRegistry(cfg))
		w.RefreshOnce(context.Background()) // fails fast: nothing listens
		w.Start(context.Background())
		w.Stop()
		w.Stop() // idempotent
		w.CloseIdleConnections()
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > before {
		t.Fatalf("goroutines grew across worker lifetimes: before=%d after=%d", before, got)
	}
}
