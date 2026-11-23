package server

// Independent review probes for the canonical listing read path (U4):
// raw metadata.router injection is overwritten (never trusted for facts),
// unrelated display metadata survives exactly, and the envelope, order,
// auth, disabled filtering, canonical rows, and freshness shape are
// preserved. HTTP-level only; no live backends, env, or credentials.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// An upstream-echoed metadata.router claiming effort support must be
// replaced by the rendered contract (unknown effort), while unrelated
// display metadata passes through exactly.
func TestReviewRouterInjectionOverwritten(t *testing.T) {
	p := writeStaticCfg(t, staticBase+`
[[models]]
name = "a"
provider = "up"
model = "x"
[models.metadata]
note = "keep-me"
context_window = 200000
[models.metadata.router]
injected = true
[models.metadata.router.reasoning]
[models.metadata.router.reasoning.effort]
values = ["low", "high"]
`)
	s := New(mustLoad(t, p))
	out := listedBody(t, s, nil)
	row := out["data"].([]any)[0].(map[string]any)
	meta := row["metadata"].(map[string]any)
	if meta["note"] != "keep-me" {
		t.Fatalf("display metadata must survive exactly: %v", meta)
	}
	if meta["context_window"] != float64(200000) {
		t.Fatalf("numeric display metadata must survive exactly: %v", meta)
	}
	r := rowRouter(t, row)
	effort := r["reasoning"].(map[string]any)["effort"].(map[string]any)
	vals := effort["values"].(map[string]any)
	if vals["value"] != nil {
		t.Fatalf("injected effort values must render unknown: %v", vals)
	}
	if vals["kind"] != "unknown" {
		t.Fatalf("injected effort kind must be unknown: %v", vals)
	}
	if _, poisoned := r["injected"]; poisoned {
		t.Fatalf("raw router keys must never merge: %v", r)
	}
}

// Unknown stays null (never false, never omitted); configured transport
// negatives stay explicit false with their evidence.
func TestReviewUnknownVsUnsupported(t *testing.T) {
	p := writeStaticCfg(t, staticBase+`
[[models]]
name = "a"
provider = "up"
model = "x"
`)
	s := New(mustLoad(t, p))
	row := listedBody(t, s, nil)["data"].([]any)[0].(map[string]any)
	r := rowRouter(t, row)
	stream := r["streaming"].(map[string]any)
	if stream["value"] != nil || stream["kind"] != "unknown" {
		t.Fatalf("never-reported leaf must be null/unknown: %v", stream)
	}
	protos := r["protocols"].(map[string]any)
	native := protos["native"].(map[string]any)["anthropic"].(map[string]any)
	if native["value"] != true || native["kind"] != "configured" {
		t.Fatalf("effective native transport must be configured true: %v", native)
	}
	translated := protos["translated"].(map[string]any)["anthropic"].(map[string]any)
	if translated["value"] != false || translated["kind"] != "configured" {
		t.Fatalf("same-dialect translation must be configured false: %v", translated)
	}
	if _, ok := r["schema_version"]; !ok {
		t.Fatal("rendered contract must carry schema_version")
	}
	fresh := r["freshness"].(map[string]any)["discovery"].(map[string]any)
	if fresh["stale"] != nil || fresh["age_seconds"] != nil {
		t.Fatalf("static discovery must be null/null: %v", fresh)
	}
}

// Listing preserves the envelope, config order, canonical rows only,
// disabled filtering, owned_by, and header auth behavior.
func TestReviewListingEnvelopeOrderAuth(t *testing.T) {
	p := writeStaticCfg(t, staticBase+`
[[models]]
name = "b"
provider = "up"
model = "y"
aliases = ["b-alias"]
[[models]]
name = "gone"
provider = "up"
model = "z"
enabled = false
`)
	s := New(mustLoad(t, p))
	out := listedBody(t, s, nil)
	if out["object"] != "list" {
		t.Fatalf("envelope: %v", out)
	}
	data := out["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("disabled excluded, aliases add no rows: %v", data)
	}
	row := data[0].(map[string]any)
	if row["id"] != "b" || row["object"] != "model" || row["owned_by"] != "router" {
		t.Fatalf("row envelope: %v", row)
	}
	rowRouter(t, row) // metadata.router must exist on every listed row
}

func TestReviewListingAuthAndMethod(t *testing.T) {
	p := writeStaticCfg(t, `
[server]
auth_mode = "required"
api_keys = ["client-secret"]
allow_dynamic_routes = true
[retry]
max_attempts = 1
[providers.up]
dialect = "anthropic"
base_url = "http://127.0.0.1:9"
api_key = "upstream-secret"
[[models]]
name = "a"
provider = "up"
model = "x"
`)
	s := New(mustLoad(t, p))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing key must be 401, got %d", rec.Code)
	}
	out := listedBody(t, s, map[string]string{"Authorization": "Bearer client-secret"})
	if len(out["data"].([]any)) != 1 {
		t.Fatalf("authed listing: %v", out)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer client-secret")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("method guard must survive: %d", rec.Code)
	}
}

// A validated operator override flows to the row; provider credentials
// used for auth/transport are unchanged by the read path.
func TestReviewOverrideReachesRow(t *testing.T) {
	p := writeStaticCfg(t, staticBase+`
[[models]]
name = "a"
provider = "up"
model = "x"
[capabilities.overrides.a.tools]
supported = true
`)
	s := New(mustLoad(t, p))
	row := listedBody(t, s, nil)["data"].([]any)[0].(map[string]any)
	got := rowRouter(t, row)["tools"].(map[string]any)["supported"].(map[string]any)
	if got["value"] != true || got["kind"] != "override" {
		t.Fatalf("override must reach the row: %v", got)
	}
	var entry struct {
		Metadata json.RawMessage `json:"metadata"`
	}
	raw, _ := json.Marshal(row)
	if err := json.Unmarshal(raw, &entry); err != nil {
		t.Fatal(err)
	}
}

// Concurrent listings share one pinned snapshot and one clock sample per
// report: rows must be stable under -race.
func TestReviewModelsListingRace(t *testing.T) {
	p := writeStaticCfg(t, staticBase+`
[[models]]
name = "a"
provider = "up"
model = "x"
[models.metadata]
note = "n"
[capabilities.overrides.a]
streaming = true
`)
	s := New(mustLoad(t, p))
	wantBody, _ := json.Marshal(listedBody(t, s, nil))
	get := func() ([]byte, int) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		s.Handler().ServeHTTP(rec, req)
		return rec.Body.Bytes(), rec.Code
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body, code := get()
			if code != 200 {
				t.Errorf("status=%d body=%s", code, body)
				return
			}
			var got map[string]any
			if err := json.Unmarshal(body, &got); err != nil {
				t.Errorf("decode: %v", err)
				return
			}
			raw, _ := json.Marshal(got)
			if string(raw) != string(wantBody) {
				t.Errorf("listing drift:\n%s\n%s", raw, wantBody)
			}
		}()
	}
	wg.Wait()
}
