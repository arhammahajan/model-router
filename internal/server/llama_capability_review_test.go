package server

import (
	"testing"

	"github.com/arhammahajan/router/internal/capability"
	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/router"
)

// Independent review of the llama listing read path: observed base
// selection, no extra I/O, private copies, override precedence.

func llamaCapRevCfg() *config.Config {
	return &config.Config{
		Server: config.Server{
			AllowDynamicRoutes: true, AuthMode: "none",
			MaxConnections: config.DefaultMaxConnections,
			MaxGenerations: config.DefaultMaxGenerations,
		},
		Providers: map[string]*config.Provider{},
		Models:    []config.Model{},
		Llama: &config.LlamaConfig{
			Servers:      []config.LlamaServer{{URL: "http://127.0.0.1:9", Name: "llama-test"}},
			ProbeTimeout: 2, RefreshSeconds: 30,
		},
	}
}

func llamaCapRevFind(t *testing.T, body map[string]any, id string) map[string]any {
	t.Helper()
	data, ok := body["data"].([]any)
	if !ok {
		t.Fatalf("data shape: %v", body)
	}
	for _, item := range data {
		m := item.(map[string]any)
		if m["id"] == id {
			return m
		}
	}
	t.Fatalf("%s missing: %v", id, body)
	return nil
}

func TestLlamaCapRevListingRestoredUnknown(t *testing.T) {
	cfg := llamaCapRevCfg()
	reg := router.NewRegistry(cfg)
	inv := router.DiscoveryInventory{
		Llama: map[string]router.LlamaInventory{
			"llama-test": {
				Entries:     []router.LlamaEntry{{Name: "m1", Upstream: "m1", Metadata: []byte(`{"router":{"tools":{"supported":{"value":true}}}}`)}},
				SucceededAt: router.Wall(500),
			},
		},
	}
	if err := reg.RestoreCached(inv); err != nil {
		t.Fatal(err)
	}
	s := NewWithRegistry(cfg, reg)
	r := rowRouter(t, llamaCapRevFind(t, listedBody(t, s, nil), "m1"))
	if got := r["tools"].(map[string]any)["supported"].(map[string]any)["value"]; got != nil {
		t.Fatalf("restored unknown must not be faked from metadata, got %v", got)
	}
}

func TestLlamaCapRevListingOverrideOverObserved(t *testing.T) {
	cfg := llamaCapRevCfg()
	over, err := capability.ValidateOverrides(map[string]any{
		"m1": map[string]any{"tools": map[string]any{"supported": false}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg.CapabilityOverrides = over
	reg := router.NewRegistry(cfg)
	obs := capability.ExtractLlamaObs(
		map[string]any{"architecture": map[string]any{"input_modalities": []any{"text", "image"}}},
		map[string]any{"modalities": map[string]any{"vision": true}}, "single")
	layer := router.LlamaLayer{Servers: map[string]router.LlamaServerSnapshot{
		"llama-test": {State: router.LlamaReachable, Entries: []router.LlamaEntry{{Name: "m1", Upstream: "m1", Obs: obs}}},
	}}
	if err := reg.CommitLlama(router.LlamaAttempt{Layer: layer, AtWall: router.Wall(1000)}); err != nil {
		t.Fatal(err)
	}
	s := NewWithRegistry(cfg, reg)
	r := rowRouter(t, llamaCapRevFind(t, listedBody(t, s, nil), "m1"))
	got := r["tools"].(map[string]any)["supported"].(map[string]any)
	if got["value"] != false || got["kind"] != "override" {
		t.Fatalf("explicit override must win over observed, got %v", got)
	}
	if got["source"] != "override.tools.supported" {
		t.Fatalf("override source: %v", got)
	}
}

func TestLlamaCapRevListingPreservesCustomMetadata(t *testing.T) {
	cfg := llamaCapRevCfg()
	reg := router.NewRegistry(cfg)
	obs := capability.ExtractLlamaObs(nil, map[string]any{"build_info": "b"}, "single")
	layer := router.LlamaLayer{Servers: map[string]router.LlamaServerSnapshot{
		"llama-test": {State: router.LlamaReachable, Entries: []router.LlamaEntry{
			{Name: "m1", Upstream: "m1", Obs: obs, Metadata: []byte(`{"display_name":"Pretty","router":{"tools":{"supported":{"value":true}}}}`)},
		}},
	}}
	if err := reg.CommitLlama(router.LlamaAttempt{Layer: layer, AtWall: router.Wall(1000)}); err != nil {
		t.Fatal(err)
	}
	s := NewWithRegistry(cfg, reg)
	row := llamaCapRevFind(t, listedBody(t, s, nil), "m1")
	meta := row["metadata"].(map[string]any)
	if meta["display_name"] != "Pretty" {
		t.Fatalf("custom metadata must be preserved: %v", meta)
	}
	r := rowRouter(t, row)
	if got := r["tools"].(map[string]any)["supported"].(map[string]any)["value"]; got != nil {
		t.Fatalf("raw router.tools injection must stay overwritten, got %v", got)
	}
}

func TestLlamaCapRevListingPrivateCopies(t *testing.T) {
	cfg := llamaCapRevCfg()
	reg := router.NewRegistry(cfg)
	obs := capability.ExtractLlamaObs(
		map[string]any{"architecture": map[string]any{"input_modalities": []any{"text", "image"}}},
		nil, "single")
	layer := router.LlamaLayer{Servers: map[string]router.LlamaServerSnapshot{
		"llama-test": {State: router.LlamaReachable, Entries: []router.LlamaEntry{{Name: "m1", Upstream: "m1", Obs: obs}}},
	}}
	if err := reg.CommitLlama(router.LlamaAttempt{Layer: layer, AtWall: router.Wall(1000)}); err != nil {
		t.Fatal(err)
	}
	s := NewWithRegistry(cfg, reg)
	first := rowRouter(t, llamaCapRevFind(t, listedBody(t, s, nil), "m1"))
	first["tools"] = map[string]any{"mutated": true}
	second := rowRouter(t, llamaCapRevFind(t, listedBody(t, s, nil), "m1"))
	if _, bad := second["tools"].(map[string]any)["mutated"]; bad {
		t.Fatalf("rendered listing mutation leaked into next read")
	}
	if got := second["tools"].(map[string]any)["supported"].(map[string]any)["value"]; got != nil {
		t.Fatalf("second read must still render unknown tools, got %v", got)
	}
}
