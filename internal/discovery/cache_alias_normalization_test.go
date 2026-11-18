package discovery

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/arhammahajan/model-router/internal/router"
)

func TestCacheEncoderReservesAllCanonicalNamesBeforeAliases(t *testing.T) {
	cfg := cacheTestConfig(t, t.TempDir())
	s := NewCacheStore(cfg, router.NewRegistry(cfg))
	inv := router.DiscoveryInventory{Llama: map[string]router.LlamaInventory{
		cfg.Llama.Servers[0].Name: {SucceededAt: router.Wall(wallNow()), Entries: []router.LlamaEntry{
			{Name: "pre-m1", Upstream: "m1", Aliases: []string{"pre-m2", "pre-m1"}},
			{Name: "pre-m2", Upstream: "m2", Aliases: []string{"pre-free"}},
		}},
	}}
	raw, ok := s.encode(inv)
	if !ok {
		t.Fatal("valid source inventory must produce a readable cache")
	}
	var doc struct {
		Llama map[string]struct{ Models []struct{ Aliases []string } }
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	models := doc.Llama["0"].Models
	if len(models) != 2 || len(models[0].Aliases) != 0 || len(models[1].Aliases) != 1 || models[1].Aliases[0] != "pre-free" {
		t.Fatalf("canonical names lost precedence in cache: %s", raw)
	}
}

func TestCachePrefixSeparatesRawIDAndAliasNamespaces(t *testing.T) {
	cfg := cacheTestConfig(t, t.TempDir())
	reg := router.NewRegistry(cfg)
	s := NewCacheStore(cfg, reg)
	inv := router.DiscoveryInventory{Llama: map[string]router.LlamaInventory{
		cfg.Llama.Servers[0].Name: {SucceededAt: router.Wall(wallNow()), Entries: []router.LlamaEntry{
			{Name: "pre-pre-next", Upstream: "pre-next"},
			{Name: "pre-raw", Upstream: "raw", Aliases: []string{"pre-next"}},
		}},
	}}
	raw, ok := s.encode(inv)
	if !ok {
		t.Fatal("full alias pre-next does not collide with canonical pre-pre-next")
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	writeFile(t, cfg.DiscoveryCache.Path, doc)
	if st := s.Restore(context.Background(), nil); st.Load != CacheLoaded {
		t.Fatalf("valid prefixed aliases must restore: %+v", st)
	}
	m, _, fault := reg.Current().Resolve("pre-next")
	if fault != router.Found || m.Upstream != "raw" {
		t.Fatalf("alias changed destination: %+v %v", m, fault)
	}
}

func TestCacheRestoreRejectsForwardFullNameAliasCollision(t *testing.T) {
	cfg := cacheTestConfig(t, t.TempDir())
	reg := router.NewRegistry(cfg)
	before := captureInv(reg)
	s := NewCacheStore(cfg, reg)
	unit := map[string]any{"succeeded_at": float64(wallNow()), "models": []any{
		map[string]any{"id": "m1", "aliases": []any{"pre-m2"}},
		map[string]any{"id": "m2", "aliases": []any{}},
	}}
	writeFile(t, cfg.DiscoveryCache.Path, cacheDoc(s.identity, nil, map[string]any{"0": unit}))
	if st := s.Restore(context.Background(), nil); st.Load != CacheInvalid {
		t.Fatalf("alias/canonical collision must reject the file: %+v", st)
	}
	before.assertUnchanged(t, reg)
}
