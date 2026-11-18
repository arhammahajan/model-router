package router

import (
	"encoding/json"
	"testing"
)

// Independent review probes for the operational read-path slice
// (docs/go-operational-read-paths-contract.md §1.1 catalog_status,
// §4 null shape). Owns only this file; no production/old-test edits.
//
// RED partition: TestReviewUnmappedModelsEmptyRendersArray fails on the
// current code (empty unmapped renders JSON null, Python renders []).
// GREEN partition: immutable-copy and shadow-exclusion locks.

// RED: empty unmapped must render [] (Python sorted() list), never null.
// Covers both the never-succeeded seed and the valid-empty success path.
func TestReviewUnmappedModelsEmptyRendersArray(t *testing.T) {
	reg := NewRegistry(catalogCfg())

	seed := reg.Current().CatalogStatus(true, "http://127.0.0.1:9/models", reg.Sanitize, 60, Mono(100))
	raw, _ := json.Marshal(seed["unmapped_models"])
	if string(raw) != "[]" {
		t.Fatalf("RED seed: empty unmapped_models must render [], got %s", string(raw))
	}

	if err := commitCatalogShim(reg, nil, nil, 1000); err != nil {
		t.Fatal(err)
	}
	empty := reg.Current().CatalogStatus(true, "http://127.0.0.1:9/models", reg.Sanitize, 60, Mono(1100))
	raw, _ = json.Marshal(empty["unmapped_models"])
	if string(raw) != "[]" {
		t.Fatalf("RED valid-empty: empty unmapped_models must render [], got %s", string(raw))
	}
}

// GREEN: staged source-event times are immutable copies; mutating a
// returned pointer must not move the registry-owned status.
func TestReviewCatalogEventTimesImmutableCopy(t *testing.T) {
	reg := NewRegistry(catalogCfg())
	if err := commitCatalogShim(reg, []CatalogEntry{{Name: "cat-1", Provider: "up", Upstream: "cat-1"}}, nil, 1000); err != nil {
		t.Fatal(err)
	}
	ev := reg.CatalogEvent()
	if ev.LastSuccess == nil {
		t.Fatal("success must stage a last success")
	}
	*ev.LastSuccess = Mono(999999)
	if got := reg.CatalogEvent(); got.LastSuccess == nil || *got.LastSuccess != Mono(1000) {
		t.Fatalf("mutating the returned event moved registry status: %+v", got.LastSuccess)
	}
}

// GREEN: unmapped excludes names shadowed by explicit routes/aliases.
func TestReviewUnmappedExcludesShadowedNames(t *testing.T) {
	reg := NewRegistry(catalogCfg())
	if err := commitCatalogShim(reg, nil, []string{"a", "a-alias", "ghost"}, 1000); err != nil {
		t.Fatal(err)
	}
	got := reg.Current().CatalogStatus(true, "http://127.0.0.1:9/models", reg.Sanitize, 60, Mono(1100))["unmapped_models"]
	raw, _ := json.Marshal(got)
	if string(raw) != `["ghost"]` {
		t.Fatalf("unmapped must exclude explicit-shadowed names, got %s", string(raw))
	}
}
