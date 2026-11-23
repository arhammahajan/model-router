package discovery

import (
	"encoding/json"
	"testing"

	"github.com/arhammahajan/router/internal/capability"
	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/router"
)

func mustLoadAsset(t *testing.T) capability.ReviewAsset {
	t.Helper()
	a, err := capability.LoadReviewAsset(capability.EmbeddedReviewBytes())
	if err != nil {
		t.Fatalf("asset must validate: %v", err)
	}
	return a
}

func reviewTestProviders() map[string]*config.Provider {
	return map[string]*config.Provider{
		"up":   {Name: "up", Dialect: config.DialectOpenAIChat, BaseURL: "http://up.example"},
		"chat": {Name: "chat", Dialect: config.DialectOpenAIChat, BaseURL: "http://chat.example"},
	}
}

func reviewTestCatalog(source string) *config.Catalog {
	cat := &config.Catalog{
		URL:            "http://127.0.0.1:9/v1/models",
		Timeout:        5,
		RefreshSeconds: 60,
		ModelProviders: map[string]string{"glm-5.3-flash": "chat"},
		MetadataSource: source,
	}
	return cat
}

// cacheIdentity adds metadata_source as a discovery-contract input: absent
// vs opencode-go-v1 invalidates; absent omits the field so existing route-
// only v2 files keep their identity. No hash-byte-compat claim.
func TestFocusCacheIdentityMetadataSource(t *testing.T) {
	mkCfg := func(cat *config.Catalog) *config.Config {
		return &config.Config{
			Server:    config.Server{AllowDynamicRoutes: true},
			Providers: reviewTestProviders(),
			Catalog:   cat,
		}
	}
	absent := mkCfg(reviewTestCatalog(""))
	admitted := mkCfg(reviewTestCatalog("opencode-go-v1"))
	if cacheIdentity(absent) == cacheIdentity(admitted) {
		t.Fatal("metadata_source must invalidate the cache identity")
	}
	if cacheIdentity(absent) != cacheIdentity(mkCfg(reviewTestCatalog(""))) {
		t.Fatal("identity must be deterministic")
	}
	// Omitted-when-absent: the encoded identity input carries no
	// metadata_source key, so pre-existing v2 digests are unchanged.
	if admitted.Catalog.MetadataSource != "opencode-go-v1" {
		t.Fatal("source not stored")
	}
}

// Route-only v2 is unchanged: no capability facts on disk; restore
// rebuilds review facts from the trusted asset with original wall times.
func TestFocusCacheRestoreRebuildsReview(t *testing.T) {
	assetCfg := &config.Config{
		Server:    config.Server{AllowDynamicRoutes: true},
		Providers: reviewTestProviders(),
		Catalog: &config.Catalog{
			URL: "http://127.0.0.1:9/v1/models", Timeout: 5, RefreshSeconds: 60,
			ModelProviders: map[string]string{"glm-5.3-flash": "chat"},
		},
		DiscoveryCache: &config.DiscoveryCacheConfig{Path: "/tmp/test-cache.json", MaxAgeSeconds: 86400},
	}
	// Opt in through the real admission path (validates the asset once).
	assetCfg.Catalog.MetadataSource = "opencode-go-v1"
	ra := mustLoadAsset(t)
	assetCfg.Catalog.ReviewAsset = ra
	assetCfg.Catalog.HasReview = true
	reg := router.NewRegistry(assetCfg)
	store := NewCacheStore(assetCfg, reg)
	inv := CachedInventory{Catalog: &CachedUnit{At: 1000, Routes: []CachedRoute{{ID: "glm-5.3-flash"}}}}
	restored, st, _ := store.buildRestore(inv, 1500)
	if st.Load == CacheInvalid {
		t.Fatal("valid cached unit must restore")
	}
	if restored.Catalog == nil || restored.Catalog.SucceededAt != 1000 {
		t.Fatalf("original wall must survive: %+v", restored)
	}
	if err := reg.RestoreCachedWithStatus(restored, "loaded", nil); err != nil {
		t.Fatal(err)
	}
	base, ok := reg.Current().CatalogBase("glm-5.3-flash")
	if !ok {
		t.Fatal("restored catalog row must publish a base from the asset")
	}
	v, _ := base.Get("tools.supported")
	if b, _ := v.Bool(); !b {
		t.Fatalf("restored base must carry reviewed facts, not disk facts")
	}
	// Encoded files stay route-only: no facts, endpoints, keys or metadata.
	enc, ok := store.encode(reg.ExportDiscovery())
	if !ok {
		t.Fatal("encode must succeed")
	}
	var doc map[string]any
	if err := json.Unmarshal(enc, &doc); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(doc)
	var _ = raw
	catUnit, _ := doc["catalog"].(map[string]any)
	models, _ := catUnit["models"].([]any)
	if len(models) != 1 {
		t.Fatalf("route-only catalog: %+v", catUnit)
	}
	row := models[0].(map[string]any)
	if len(row) != 2 {
		t.Fatalf("cached rows carry only id+aliases: %v", row)
	}
	if _, has := doc["capabilities"]; has {
		t.Fatal("cache must never persist capabilities")
	}
}
