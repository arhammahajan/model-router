package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/arhammahajan/model-router/internal/router"
)

// Catalog capability listing: end-to-end over HTTP with one pinned
// snapshot plus one clock sample per report. Match renders reviewed facts
// with full provenance; unknown renders empty with catalog availability
// only; mismatch keeps route transport with applicable false.

func capRevTOML() string {
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

func publishCapRevRows(t *testing.T, reg *router.Registry) {
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

func capRevRows(t *testing.T, s *Server) map[string]map[string]any {
	t.Helper()
	out := listedBody(t, s, nil)
	byID := map[string]map[string]any{}
	for _, d := range out["data"].([]any) {
		row := d.(map[string]any)
		byID[row["id"].(string)] = row
	}
	return byID
}

// Match renders reviewed facts with full provenance plus catalog
// availability at the original wall; unknown stays routable with empty
// facts and catalog availability only.
func TestCatalogCapReviewListingMatchUnknown(t *testing.T) {
	p := writeStaticCfg(t, capRevTOML())
	cfg := mustLoad(t, p)
	reg := router.NewRegistry(cfg)
	publishCapRevRows(t, reg)
	s := NewWithRegistry(cfg, reg)
	byID := capRevRows(t, s)
	match := rowRouter(t, byID["glm-5.3-flash"])
	tools := match["tools"].(map[string]any)["supported"].(map[string]any)
	if tools["value"] != true || tools["kind"] != "reviewed" || tools["source"] != "go.review.tools.supported" {
		t.Fatalf("match must render reviewed facts: %v", tools)
	}
	gr := match["sources"].(map[string]any)["go.review"].(map[string]any)
	if gr["applicable"] != true {
		t.Fatalf("match applicable: %v", gr)
	}
	for _, k := range []string{"model_source", "source", "reviewed_on", "repository_revision", "snapshot_sha256", "retrieved_at", "stale_after_seconds", "applicable"} {
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
	// Second known dialect row renders under its own dialect.
	claude := rowRouter(t, byID["claude-haiku-5-5"])
	if claude["sources"].(map[string]any)["go.review"].(map[string]any)["applicable"] != true {
		t.Fatalf("anthropic row applicable: %v", claude["sources"])
	}
	native := claude["protocols"].(map[string]any)["native"].(map[string]any)
	if native["anthropic"].(map[string]any)["value"] != true {
		t.Fatalf("anthropic transport: %v", native)
	}
	unk := rowRouter(t, byID["unknown-model-z"])
	if _, has := unk["sources"].(map[string]any)["go.review"]; has {
		t.Fatalf("unknown must carry no review source")
	}
	if got := unk["tools"].(map[string]any)["supported"].(map[string]any)["value"]; got != nil {
		t.Fatalf("unknown facts must be null: %v", got)
	}
	if unk["sources"].(map[string]any)["catalog"].(map[string]any)["retrieved_at"] != float64(1000) {
		t.Fatalf("unknown availability must use original wall")
	}
	// Null stays null over the wire, never false.
	if unk["streaming"] == nil {
		t.Fatal("streaming leaf missing")
	}
}

// Mismatch keeps route transport with applicable false; static shadowing
// wins with the static base; raw router metadata is overwritten.
func TestCatalogCapReviewListingMismatchShadow(t *testing.T) {
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
	byID := capRevRows(t, s)
	mis := rowRouter(t, byID["glm-5.3-flash"])
	if mis["sources"].(map[string]any)["go.review"].(map[string]any)["applicable"] != false {
		t.Fatal("mismatch applicable must be false")
	}
	native := mis["protocols"].(map[string]any)["native"].(map[string]any)
	if native["anthropic"].(map[string]any)["value"] != true {
		t.Fatal("mismatch keeps route transport")
	}
	st := rowRouter(t, byID["a"])
	if _, has := st["sources"].(map[string]any)["go.review"]; has {
		t.Fatal("shadowed static row must not carry review")
	}
	if _, poisoned := st["injected"]; poisoned {
		t.Fatal("raw router keys must never merge")
	}
	eff := st["reasoning"].(map[string]any)["effort"].(map[string]any)["values"].(map[string]any)
	if eff["value"] != nil {
		t.Fatalf("injected effort must render unknown: %v", eff)
	}
}

// Review TTL is independent: stale flips only on review age>=TTL while the
// catalog availability follows discovery; a failed probe never refreshes
// the review retrieved_at.
func TestCatalogCapReviewListingTTLFollowsReview(t *testing.T) {
	p := writeStaticCfg(t, capRevTOML())
	cfg := mustLoad(t, p)
	reg := router.NewRegistry(cfg)
	publishCapRevRows(t, reg)
	s := NewWithRegistry(cfg, reg)
	byID := capRevRows(t, s)
	gr := rowRouter(t, byID["glm-5.3-flash"])["sources"].(map[string]any)["go.review"].(map[string]any)
	if gr["age_seconds"] == nil || gr["stale"] == nil {
		t.Fatalf("review age/stale must derive: %v", gr)
	}
	if gr["stale"] != false {
		t.Fatalf("fresh review must not be stale: %v", gr)
	}
	// Failed probe keeps the review facts and the original retrieved_at.
	if err := reg.CommitCatalog(router.CatalogAttempt{
		Event: router.SourceEvent{LastSuccess: reg.CatalogEvent().LastSuccess, Failures: 1, Err: "boom"},
	}); err != nil {
		t.Fatal(err)
	}
	s2 := NewWithRegistry(cfg, reg)
	byID2 := capRevRows(t, s2)
	gr2 := rowRouter(t, byID2["glm-5.3-flash"])["sources"].(map[string]any)["go.review"].(map[string]any)
	if gr2["retrieved_at"] != gr["retrieved_at"] {
		t.Fatalf("failed probe must never refresh review retrieved_at: %v -> %v", gr["retrieved_at"], gr2["retrieved_at"])
	}
	if rowRouter(t, byID2["glm-5.3-flash"])["tools"].(map[string]any)["supported"].(map[string]any)["value"] != true {
		t.Fatal("failed probe must retain review facts")
	}
}

// Concurrent GETs pin one snapshot plus one clock sample: stable bodies,
// original wall times, no filesystem/network on the read path.
func TestCatalogCapReviewListingConcurrent(t *testing.T) {
	p := writeStaticCfg(t, capRevTOML())
	cfg := mustLoad(t, p)
	reg := router.NewRegistry(cfg)
	publishCapRevRows(t, reg)
	s := NewWithRegistry(cfg, reg)
	want := capRevRows(t, s)
	stripCapVolatile(want)
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
			stripCapVolatile(byID)
			raw, _ := json.Marshal(byID)
			wantRaw, _ := json.Marshal(want)
			if string(raw) != string(wantRaw) {
				t.Errorf("listing drift")
			}
		}()
	}
	wg.Wait()
}

func stripCapVolatile(byID map[string]map[string]any) {
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
