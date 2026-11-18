package server

import (
	"testing"

	"github.com/arhammahajan/model-router/internal/capability"
	"github.com/arhammahajan/model-router/internal/config"
	"github.com/arhammahajan/model-router/internal/router"
)

func TestLlamaObsListingBaseSelection(t *testing.T) {
	cfg := &config.Config{
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
	reg := router.NewRegistry(cfg)
	row := map[string]any{
		"architecture": map[string]any{"input_modalities": []any{"text", "image"}},
		"metadata":     map[string]any{"router": map[string]any{"reasoning": map[string]any{"effort": map[string]any{"values": map[string]any{"value": []any{"x"}}}}}},
	}
	props := map[string]any{"build_info": "llama.cpp ver", "modalities": map[string]any{"vision": true}}
	obs := capability.ExtractLlamaObs(row, props, "single")
	layer := router.LlamaLayer{Servers: map[string]router.LlamaServerSnapshot{
		"llama-test": {State: router.LlamaReachable, Entries: []router.LlamaEntry{{Name: "m1", Upstream: "m1", Obs: obs}}},
	}}
	if err := reg.CommitLlama(router.LlamaAttempt{Layer: layer, AtWall: router.Wall(1000)}); err != nil {
		t.Fatal(err)
	}
	s := NewWithRegistry(cfg, reg)
	body := listedBody(t, s, nil)
	data, ok := body["data"].([]any)
	if !ok || len(data) == 0 {
		t.Fatalf("no rows: %v", body)
	}
	var found map[string]any
	for _, item := range data {
		m := item.(map[string]any)
		if m["id"] == "m1" {
			found = m
		}
	}
	if found == nil {
		t.Fatalf("m1 missing: %v", body)
	}
	r := rowRouter(t, found)
	img := r["modalities"].(map[string]any)["input"].(map[string]any)["image"].(map[string]any)
	if img["value"] != true {
		t.Fatalf("observed image: %v", img)
	}
	eff := r["reasoning"].(map[string]any)["effort"].(map[string]any)["values"].(map[string]any)
	if eff["value"] != nil {
		t.Fatalf("injected router metadata must be overwritten, got %v", eff)
	}
	srcs := r["sources"].(map[string]any)
	lp, ok := srcs["llama.props"].(map[string]any)
	if !ok || lp["server_build"] != "llama.cpp ver" || lp["available"] != true {
		t.Fatalf("llama.props provenance: %v", srcs)
	}
}
