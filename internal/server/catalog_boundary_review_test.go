package server

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCatalogReviewGeminiKeepsUnmappedFault(t *testing.T) {
	srv, _ := newCatalogHarness(t, &catalogScript{}, nil)
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()
	status, _, body := post(t, httpSrv.URL+"/v1beta/models/ghost:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status != 404 || !strings.Contains(string(body), `"reason":"MODEL_ENDPOINT_UNKNOWN"`) ||
		!strings.Contains(string(body), "catalog.model_providers") {
		t.Fatalf("Gemini lost catalog routing fault: %d %s", status, body)
	}
}
