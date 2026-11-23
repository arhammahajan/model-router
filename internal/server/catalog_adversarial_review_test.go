// Independent adversarial review: server-side catalog boundaries -- the
// generation-side half of the trust boundary and constructor I/O freedom.
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/arhammahajan/router/internal/config"
)

// TestReviewCatalogGenerationKeepsTrustedHeadersOffUpstream pins that the
// trusted [catalog] headers touch ONLY the catalog GET: a catalog-backed
// generation must reach the upstream without X-Catalog-Token (or any catalog
// header) while still carrying provider credentials. The fetch-side half is
// covered in discovery; this is the generation side.
func TestReviewCatalogGenerationKeepsTrustedHeadersOffUpstream(t *testing.T) {
	var gotCatalogToken, gotProviderKey string
	upstream := func(w http.ResponseWriter, r *http.Request) {
		gotCatalogToken = r.Header.Get("X-Catalog-Token")
		gotProviderKey = r.Header.Get("X-Api-Key")
		anthropicOK(w, r)
	}
	script := &catalogScript{}
	srv, _ := newCatalogHarness(t, script, upstream)
	rs := httptest.NewServer(srv.Handler())
	defer rs.Close()
	status, _, data := post(t, rs.URL+"/v1/chat/completions",
		`{"model":"cat-a","messages":[{"role":"user","content":"Hi"}],"max_tokens":4}`, nil)
	if status != 200 {
		t.Fatalf("catalog route must serve: %d %s", status, data)
	}
	if gotCatalogToken != "" {
		t.Fatalf("catalog header leaked into generation: %q", gotCatalogToken)
	}
	if gotProviderKey != "upstream-secret" {
		t.Fatalf("provider credential missing or wrong on generation: %q", gotProviderKey)
	}
}

// TestReviewServerConstructorPerformsNoCatalogIO pins that Server.New owns
// no startup inventory: before the composition root runs RefreshOnce, no
// catalog GET may happen, readiness reflects the static seed only, and the
// listing is static-only.
func TestReviewServerConstructorPerformsNoCatalogIO(t *testing.T) {
	script := &catalogScript{}
	catSrv := httptest.NewServer(http.HandlerFunc(script.handler))
	defer catSrv.Close()
	cfg, err := config.Load(writeCatalogCfg(t, catSrv.URL, ""))
	if err != nil {
		t.Fatal(err)
	}
	srv := New(cfg) // no worker, no RefreshOnce: must be I/O-free
	if got := script.hitCount(); got != 0 {
		t.Fatalf("server construction performed catalog I/O: %d hits", got)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != 503 {
		t.Fatalf("pre-refresh readiness must be 503 on empty static seed: %d %s", rec.Code, rec.Body.String())
	}
	modelsRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(modelsRec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(modelsRec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != 0 {
		t.Fatalf("pre-refresh listing must be static-only: %s", modelsRec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"status":"not_ready"`) {
		t.Fatalf("readiness payload must stay Go-shaped: %s", rec.Body.String())
	}
}
