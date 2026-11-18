package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Independent review probes for the operational security contract
// (docs/go-operational-read-paths-contract.md §1.1 authorized(), §5).
// Source: stable HEAD:model_router/discovery.py:789-811 — authorized()
// collects the Bearer strip plus x-api-key/api-key/x-goog-api-key plus
// Gemini query_keys and accepts when ANY candidate matches; the scheme
// prefix match is case-insensitive. Owns only this file.
//
// RED partition: the two tests below fail on the current code, which
// short-circuits on the Authorization header alone (case-sensitive).
// GREEN partition: header-only query rejection on the list endpoint.

// RED: any presented header credential must authenticate (Python union).
// Bearer wrong + X-Api-Key correct currently 401s, Python accepts.
func TestReviewAuthAcceptsAnyHeaderCredential(t *testing.T) {
	srv := New(diagnosticsCfg("http://127.0.0.1:9/x", "http://127.0.0.1:9/y", "router-key-1"))
	req := httptest.NewRequest(http.MethodGet, "/diagnostics", nil)
	req.Header.Set("Authorization", "Bearer wrong-key")
	req.Header.Set("X-Api-Key", "router-key-1")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("RED: union of header credentials must authenticate, got %d", rec.Code)
	}
}

// RED: the Bearer scheme prefix is case-insensitive in Python
// (authorization.lower().startswith("bearer ")).
func TestReviewAuthBearerSchemeCaseInsensitive(t *testing.T) {
	srv := New(diagnosticsCfg("http://127.0.0.1:9/x", "http://127.0.0.1:9/y", "router-key-1"))
	req := httptest.NewRequest(http.MethodGet, "/diagnostics", nil)
	req.Header.Set("Authorization", "BEARER router-key-1")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("RED: BEARER scheme must authenticate, got %d", rec.Code)
	}
}

// GREEN: ?key is never accepted on Chat list endpoints (header-only).
func TestReviewModelsRejectsQueryKey(t *testing.T) {
	srv := New(diagnosticsCfg("http://127.0.0.1:9/x", "http://127.0.0.1:9/y", "router-key-1"))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models?key=router-key-1", nil))
	if rec.Code != 401 {
		t.Fatalf("query keys must be rejected on /v1/models, got %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("X-Api-Key", "router-key-1")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("header auth must open /v1/models, got %d", rec.Code)
	}
}
