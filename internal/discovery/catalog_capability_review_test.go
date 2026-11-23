package discovery

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/arhammahajan/router/internal/capability"
	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/router"
)

// Catalog capability cache contract: identity invalidates on the opt-in
// (omitted-when-absent so old v2 digests keep), route-only payload carries
// no facts or secrets, restore rebuilds review from the trusted asset with
// original wall times.

func capRevProviders() map[string]*config.Provider {
	return map[string]*config.Provider{
		"up":   {Name: "up", Dialect: config.DialectOpenAIChat, BaseURL: "http://up.example"},
		"chat": {Name: "chat", Dialect: config.DialectOpenAIChat, BaseURL: "http://chat.example"},
	}
}

func capRevCatalog(source string) *config.Catalog {
	return &config.Catalog{
		URL:            "http://127.0.0.1:9/v1/models",
		Timeout:        5,
		RefreshSeconds: 60,
		ModelProviders: map[string]string{"glm-5.3-flash": "chat"},
		MetadataSource: source,
	}
}

func capRevCfg(source string) *config.Config {
	return &config.Config{
		Server:    config.Server{AllowDynamicRoutes: true},
		Providers: capRevProviders(),
		Catalog:   capRevCatalog(source),
	}
}

// metadata_source is a discovery-contract input: absent vs admitted
// invalidates; absent omits the field so existing v2 files keep identity.
func TestCatalogCapReviewCacheIdentity(t *testing.T) {
	absent := capRevCfg("")
	admitted := capRevCfg("opencode-go-v1")
	if cacheIdentity(absent) == cacheIdentity(admitted) {
		t.Fatal("metadata_source must invalidate the cache identity")
	}
	if cacheIdentity(absent) != cacheIdentity(capRevCfg("")) {
		t.Fatal("identity must be deterministic")
	}
	if cacheIdentity(admitted) != cacheIdentity(capRevCfg("opencode-go-v1")) {
		t.Fatal("identity must be deterministic")
	}
	// Timeouts/intervals stay out of the identity: changing refresh alone
	// must not invalidate.
	tweaked := capRevCfg("opencode-go-v1")
	tweaked.Catalog.RefreshSeconds = 5
	tweaked.Catalog.Timeout = 7
	if cacheIdentity(admitted) != cacheIdentity(tweaked) {
		t.Fatal("timeouts/intervals must not affect identity")
	}
	// Digest-only claim: the identity is a hex sha256, never Python bytes.
	for _, cfg := range []*config.Config{absent, admitted} {
		id := cacheIdentity(cfg)
		if len(id) != 64 || strings.Trim(id, "0123456789abcdef") != "" {
			t.Fatalf("identity must be a hex digest: %q", id)
		}
	}
}

// Route-only v2: no facts, endpoints, keys, headers or metadata on disk;
// secret-like IDs are refused, never persisted.
func TestCatalogCapReviewRouteOnlyPayload(t *testing.T) {
	assetCfg := &config.Config{
		Server:    config.Server{AllowDynamicRoutes: true},
		Providers: capRevProviders(),
		Catalog: &config.Catalog{
			URL: "http://127.0.0.1:9/v1/models", Timeout: 5, RefreshSeconds: 60,
			ModelProviders: map[string]string{"glm-5.3-flash": "chat"},
			MetadataSource: "opencode-go-v1",
		},
		DiscoveryCache: &config.DiscoveryCacheConfig{Path: "/tmp/test-cache.json", MaxAgeSeconds: 86400},
	}
	ra, err := capability.LoadReviewAsset(capability.EmbeddedReviewBytes())
	if err != nil {
		t.Fatal(err)
	}
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
		t.Fatal("restored base must carry reviewed facts, not disk facts")
	}
	enc, ok := store.encode(reg.ExportDiscovery())
	if !ok {
		t.Fatal("encode must succeed")
	}
	text := string(enc)
	for _, secret := range []string{"reviewed", "go.review", "capabilit", "facts", "upstream-secret", "http://127.0.0.1:9", "opencode-go-v1", "availability"} {
		if strings.Contains(text, secret) {
			t.Fatalf("route-only payload must not contain %q: %s", secret, text)
		}
	}
	var doc map[string]any
	if err := json.Unmarshal(enc, &doc); err != nil {
		t.Fatal(err)
	}
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
	// A secret-echoing ID is refused at restore: the sanitizer corpus
	// (built from trusted config secrets) rejects it, so a crafted file
	// can never publish attacker-chosen rows.
	secretCfg := capRevCfg("opencode-go-v1")
	secretCfg.Providers["up"].APIKey = "secret-key-1"
	secretCfg.DiscoveryCache = &config.DiscoveryCacheConfig{Path: "/tmp/test-cache.json", MaxAgeSeconds: 86400}
	secretReg := router.NewRegistry(secretCfg)
	secretStore := NewCacheStore(secretCfg, secretReg)
	if secretStore.sanitize.safeID("secret-key-1") {
		t.Fatal("secret echo must fail admission")
	}
	_, rst, _ := secretStore.buildRestore(CachedInventory{
		Catalog: &CachedUnit{At: 1000, Routes: []CachedRoute{{ID: "secret-key-1"}}},
	}, 1500)
	if rst.Load != CacheInvalid {
		t.Fatalf("secret ID must invalidate restore: %+v", rst)
	}
}
