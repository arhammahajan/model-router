package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/arhammahajan/model-router/internal/config"
	"github.com/arhammahajan/model-router/internal/router"
)

func loadCfgForTest(p string) (*config.Config, error) {
	return config.Load(p)
}

func reviewCatalogTOML() string {
	return staticBase + `
[providers.chat]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
api_key = "upstream-secret"
[catalog]
url = "http://127.0.0.1:9/v1/models"
metadata_source = "opencode-go-v1"
model_providers = {"glm-5.3-flash" = "chat", "claude-haiku-5-5" = "up", "unknown-model-z" = "up", "a" = "up"}
[[models]]
name = "a"
provider = "up"
model = "x"
`
}

func publishReviewRows(t *testing.T, reg *router.Registry) {
	t.Helper()
	entries := []router.CatalogEntry{
		{Name: "glm-5.3-flash", Provider: "chat", Upstream: "glm-5.3-flash"},
		{Name: "claude-haiku-5-5", Provider: "up", Upstream: "claude-haiku-5-5"},
		{Name: "unknown-model-z", Provider: "up", Upstream: "unknown-model-z"},
		{Name: "a", Provider: "up", Upstream: "catalog-x"},
	}
	if err := commitCatalogShim(reg, entries, nil, 1000); err != nil {
		t.Fatal(err)
	}
}

func getRows(t *testing.T, s *Server) map[string]map[string]any {
	t.Helper()
	out := listedBody(t, s, nil)
	byID := map[string]map[string]any{}
	for _, d := range out["data"].([]any) {
		row := d.(map[string]any)
		byID[row["id"].(string)] = row
	}
	return byID
}

// Match renders reviewed facts with source/kind plus full provenance;
// unknown renders empty with catalog availability only.
func TestFocusReviewListingMatchUnknown(t *testing.T) {
	p := writeStaticCfg(t, reviewCatalogTOML())
	cfg := mustLoad(t, p)
	reg := router.NewRegistry(cfg)
	publishReviewRows(t, reg)
	s := NewWithRegistry(cfg, reg)
	byID := getRows(t, s)
	match := rowRouter(t, byID["glm-5.3-flash"])
	tools := match["tools"].(map[string]any)["supported"].(map[string]any)
	if tools["value"] != true || tools["kind"] != "reviewed" || tools["source"] != "go.review.tools.supported" {
		t.Fatalf("match must render reviewed facts: %v", tools)
	}
	gr := match["sources"].(map[string]any)["go.review"].(map[string]any)
	if gr["applicable"] != true {
		t.Fatalf("match applicable: %v", gr)
	}
	for _, k := range []string{"model_source", "source", "reviewed_on", "repository_revision", "snapshot_sha256", "retrieved_at", "stale_after_seconds"} {
		if _, has := gr[k]; !has {
			t.Fatalf("provenance missing %q: %v", k, gr)
		}
	}
	if gr["source"] != "https://models.dev/api.json" {
		t.Fatalf("review source provenance: %v", gr)
	}
	cat := match["sources"].(map[string]any)["catalog"].(map[string]any)
	if cat["verification"] != "availability only" || cat["retrieved_at"] != float64(1000) {
		t.Fatalf("catalog availability must use original wall: %v", cat)
	}
	// Unknown stays routable with empty facts and no review source.
	unk := rowRouter(t, byID["unknown-model-z"])
	if _, has := unk["sources"].(map[string]any)["go.review"]; has {
		t.Fatalf("unknown must carry no review source")
	}
	if got := unk["tools"].(map[string]any)["supported"].(map[string]any)["value"]; got != nil {
		t.Fatalf("unknown facts must be null: %v", got)
	}
}

// Mismatch keeps route transport with applicable false; static shadowing
// wins with the static base; raw router metadata is overwritten.
func TestFocusReviewListingMismatchShadowInjection(t *testing.T) {
	p := writeStaticCfg(t, staticBase+`
[providers.chat]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
api_key = "upstream-secret"
[catalog]
url = "http://127.0.0.1:9/v1/models"
metadata_source = "opencode-go-v1"
model_providers = {"glm-5.3-flash" = "up"}
[[models]]
name = "a"
provider = "up"
model = "x"
[models.metadata.router.reasoning.effort]
values = ["low"]
`)
	cfg := mustLoad(t, p)
	reg := router.NewRegistry(cfg)
	if err := commitCatalogShim(reg, []router.CatalogEntry{
		{Name: "glm-5.3-flash", Provider: "up", Upstream: "glm-5.3-flash"},
		{Name: "a", Provider: "up", Upstream: "catalog-x"},
	}, nil, 1000); err != nil {
		t.Fatal(err)
	}
	s := NewWithRegistry(cfg, reg)
	byID := getRows(t, s)
	mis := rowRouter(t, byID["glm-5.3-flash"])
	if mis["sources"].(map[string]any)["go.review"].(map[string]any)["applicable"] != false {
		t.Fatalf("mismatch applicable must be false")
	}
	native := mis["protocols"].(map[string]any)["native"].(map[string]any)
	if native["anthropic"].(map[string]any)["value"] != true {
		t.Fatalf("mismatch keeps route transport")
	}
	// Static shadow: explicit row, no catalog base, raw injection gone.
	st := rowRouter(t, byID["a"])
	if _, has := st["sources"].(map[string]any)["go.review"]; has {
		t.Fatalf("shadowed static row must not carry review")
	}
	if _, poisoned := st["injected"]; poisoned {
		t.Fatalf("raw router keys must never merge")
	}
	eff := st["reasoning"].(map[string]any)["effort"].(map[string]any)["values"].(map[string]any)
	if eff["value"] != nil {
		t.Fatalf("injected effort must render unknown: %v", eff)
	}
	// Static transport follows the provider dialect.
	if native := st["protocols"].(map[string]any)["native"].(map[string]any); native["anthropic"].(map[string]any)["value"] != true {
		t.Fatalf("static base follows provider: %v", native)
	}
}

// Override-vs-review conflict keeps evidence with validation.*/conflict on
// the withdrawn leaf; canonical override keys win, alias keys never apply.
func TestFocusReviewListingOverrideConflict(t *testing.T) {
	p := writeStaticCfg(t, reviewCatalogTOML()+`
[capabilities.overrides."glm-5.3-flash".reasoning.budget]
min_tokens = 10
max_tokens = 5
`)
	// Broken relations inside one override are a startup error.
	if _, err := loadCfgForTest(p); err == nil {
		t.Fatal("broken budget relation must be a startup error")
	}
	// Cross-layer conflict: disable tools over reviewed support.
	p2 := writeStaticCfg(t, reviewCatalogTOML()+`
[capabilities.overrides."glm-5.3-flash".tools]
supported = false
[capabilities.overrides."alias-x".tools]
supported = true
`)
	_ = p
	cfg := mustLoad(t, p2)
	reg := router.NewRegistry(cfg)
	publishReviewRows(t, reg)
	s := NewWithRegistry(cfg, reg)
	byID := getRows(t, s)
	got := rowRouter(t, byID["glm-5.3-flash"])["tools"].(map[string]any)["supported"].(map[string]any)
	if got["value"] != false || got["kind"] != "override" {
		t.Fatalf("override must win: %v", got)
	}
	confs, ok := got["conflicts"].([]any)
	if !ok || len(confs) == 0 {
		t.Fatalf("review evidence must be retained: %v", got)
	}
	first := confs[0].(map[string]any)
	if first["source"] != "go.review.tools.supported" || first["kind"] != "reviewed" {
		t.Fatalf("conflict must carry review evidence: %v", first)
	}
}

// Concurrent GETs pin one snapshot plus one clock sample: stable bodies,
// original wall times (never now), no filesystem/network on the read path.
func TestFocusReviewListingConcurrent(t *testing.T) {
	p := writeStaticCfg(t, reviewCatalogTOML())
	cfg := mustLoad(t, p)
	reg := router.NewRegistry(cfg)
	publishReviewRows(t, reg)
	s := NewWithRegistry(cfg, reg)
	want := getRows(t, s)
	stripVolatile(want)
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
				return
			}
			var out map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Errorf("decode: %v", err)
				return
			}
			byID := map[string]map[string]any{}
			for _, d := range out["data"].([]any) {
				row := d.(map[string]any)
				byID[row["id"].(string)] = row
			}
			stripVolatile(byID)
			raw, _ := json.Marshal(byID)
			wantRaw, _ := json.Marshal(want)
			if string(raw) != string(wantRaw) {
				t.Errorf("listing drift")
			}
			// Original wall retained, never save/now clock.
			r := byID["glm-5.3-flash"]["metadata"].(map[string]any)["router"].(map[string]any)
			if r["sources"].(map[string]any)["catalog"].(map[string]any)["retrieved_at"] != float64(1000) {
				t.Errorf("read path must retain original wall")
			}
		}()
	}
	wg.Wait()
}

// stripVolatile removes wall-derived ages/staleness plus the freshness
// block so concurrent-read stability asserts the pinned snapshot facts,
// not the sampled clock.
func stripVolatile(byID map[string]map[string]any) {
	for _, row := range byID {
		meta, _ := row["metadata"].(map[string]any)
		if meta == nil {
			continue
		}
		r, _ := meta["router"].(map[string]any)
		if r == nil {
			continue
		}
		delete(r, "freshness")
		srcs, _ := r["sources"].(map[string]any)
		for _, s := range srcs {
			m, _ := s.(map[string]any)
			if m == nil {
				continue
			}
			delete(m, "age_seconds")
			delete(m, "stale")
		}
	}
}
