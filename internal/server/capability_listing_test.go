package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/router"
)

func mustLoad(t *testing.T, p string) *config.Config {
	t.Helper()
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func listedBody(t *testing.T, s *Server, hdr map[string]string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func rowRouter(t *testing.T, row map[string]any) map[string]any {
	t.Helper()
	meta, ok := row["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata shape: %v", row)
	}
	r, ok := meta["router"].(map[string]any)
	if !ok {
		t.Fatalf("metadata.router missing: %v", meta)
	}
	return r
}

func TestCapabilityListingGolden(t *testing.T) {
	p := writeStaticCfg(t, staticBase+`
[[models]]
name = "a"
provider = "up"
model = "x"
[models.metadata]
display_name = "Pretty"
context_window = 200000
[capabilities.overrides.a.tools]
supported = true
`)
	cfg := mustLoad(t, p)
	s := New(cfg)
	out := listedBody(t, s, nil)
	data := out["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("data: %v", out)
	}
	row := data[0].(map[string]any)
	if row["id"] != "a" || row["owned_by"] != "router" || row["object"] != "model" {
		t.Fatalf("envelope: %v", row)
	}
	r := rowRouter(t, row)
	// Effective dialect: provider anthropic.
	native := r["protocols"].(map[string]any)["native"].(map[string]any)["anthropic"].(map[string]any)
	if native["value"] != true || native["kind"] != "configured" {
		t.Fatalf("effective dest claim: %v", native)
	}
	tools := r["reasoning"].(map[string]any)
	_ = tools
	tsup := r["tools"].(map[string]any)["supported"].(map[string]any)
	if tsup["value"] != true || tsup["kind"] != "override" {
		t.Fatalf("override not applied: %v", tsup)
	}
	// Unknown stays null, never false.
	if r["tools"].(map[string]any)["parallel"].(map[string]any)["value"] != nil {
		t.Fatalf("unknown must be null")
	}
	// Freshness present for static.
	fresh := r["freshness"].(map[string]any)["discovery"].(map[string]any)
	if fresh["stale"] != nil {
		t.Fatalf("static stale null: %v", fresh)
	}
	// Raw metadata preserved exactly.
	meta := row["metadata"].(map[string]any)
	if meta["display_name"] != "Pretty" {
		t.Fatalf("display preserved: %v", meta)
	}
	if n, ok := meta["context_window"].(float64); !ok || n != 200000 {
		t.Fatalf("numeric exact: %v", meta)
	}
}

func TestCapabilityRawRouterInjection(t *testing.T) {
	p := writeStaticCfg(t, staticBase+`
[[models]]
name = "a"
provider = "up"
model = "x"
[models.metadata.router.reasoning.effort]
values = ["low", "high"]
`)
	cfg := mustLoad(t, p)
	s := New(cfg)
	out := listedBody(t, s, nil)
	row := out["data"].([]any)[0].(map[string]any)
	r := rowRouter(t, row)
	eff := r["reasoning"].(map[string]any)["effort"].(map[string]any)
	if eff["values"].(map[string]any)["value"] != nil {
		t.Fatalf("raw router injection must render unknown: %v", eff)
	}
}

func TestCapabilityCanonicalAlias(t *testing.T) {
	p := writeStaticCfg(t, staticBase+`
[[models]]
name = "real"
provider = "up"
model = "x"
aliases = ["alias-a"]
[capabilities.overrides.real.tools]
supported = true
[capabilities.overrides.alias-a.tools]
supported = false
`)
	cfg := mustLoad(t, p)
	s := New(cfg)
	out := listedBody(t, s, nil)
	data := out["data"].([]any)
	if len(data) != 1 || data[0].(map[string]any)["id"] != "real" {
		t.Fatalf("canonical rows only: %v", out)
	}
	r := rowRouter(t, data[0].(map[string]any))
	// Canonical override wins; alias-key override never applies.
	if got := r["tools"].(map[string]any)["supported"].(map[string]any)["value"]; got != true {
		t.Fatalf("alias override must not apply: %v", r["tools"])
	}
	// Alias resolves to same canonical rendering.
	m, _, rerr := s.resolveModel("alias-a")
	if rerr != nil || m.Name != "real" {
		t.Fatalf("alias resolve: %+v %v", m, rerr)
	}
}

func TestCapabilityRestoredStaleFreshness(t *testing.T) {
	p := writeStaticCfg(t, staticBase+`
[[models]]
name = "exp"
provider = "up"
model = "x"
`)
	cfg := mustLoad(t, p)
	reg := router.NewRegistry(cfg)
	// Restore a catalog layer as stale.
	inv := router.DiscoveryInventory{
		Catalog: &router.CatalogInventory{
			Entries:     []router.CatalogEntry{{Name: "cat-a", Provider: "up", Upstream: "cat-a"}},
			SucceededAt: router.Wall(100),
		},
		Llama: map[string]router.LlamaInventory{},
	}
	if err := reg.RestoreCached(inv); err != nil {
		t.Fatal(err)
	}
	s := NewWithRegistry(cfg, reg)
	out := listedBody(t, s, nil)
	byID := map[string]map[string]any{}
	for _, d := range out["data"].([]any) {
		row := d.(map[string]any)
		byID[row["id"].(string)] = row
	}
	// Overrides reapplied after restore.
	if _, ok := byID["exp"]; !ok {
		t.Fatalf("explicit missing: %v", byID)
	}
	catRow, ok := byID["cat-a"]
	if !ok {
		t.Fatalf("restored missing: %v", byID)
	}
	r := rowRouter(t, catRow)
	fresh := r["freshness"].(map[string]any)["discovery"].(map[string]any)
	if _, ok := fresh["age_seconds"]; !ok {
		t.Fatalf("restored freshness: %v", fresh)
	}
	if _, ok := fresh["cached"]; !ok {
		t.Fatalf("restored cached: %v", fresh)
	}
}

func TestCapabilityListingNoIOAndRace(t *testing.T) {
	p := writeStaticCfg(t, staticBase+`
[[models]]
name = "a"
provider = "up"
model = "x"
[models.metadata]
k = "v"
`)
	cfg := mustLoad(t, p)
	s := New(cfg)
	h := s.Handler()
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
			if rec.Code != 200 {
				t.Errorf("code=%d", rec.Code)
			}
			if strings.Contains(rec.Body.String(), `"router"`) == false {
				t.Errorf("router missing")
			}
		}()
	}
	wg.Wait()
}
