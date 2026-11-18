package server

import (
	"net/http/httptest"
	"testing"

	"github.com/arhammahajan/model-router/internal/capability"
)

func TestReadGeminiInputLimitFallsBackFromNull(t *testing.T) {
	c := capability.Empty("openai-chat")
	if err := c.Put("limits.context_window", capability.IntValue(9007199254740993), nil, capability.KindConfigured); err != nil {
		t.Fatal(err)
	}
	row := listingRow{name: "m", contract: c}
	if got := geminiResourceFor(row)["inputTokenLimit"]; got != int64(9007199254740993) {
		t.Fatalf("null max_input_tokens must use known context_window exactly: %v", got)
	}
	if err := c.Put("limits.max_input_tokens", capability.IntValue(32000), nil, capability.KindConfigured); err != nil {
		t.Fatal(err)
	}
	row.contract = c
	if got := geminiResourceFor(row)["inputTokenLimit"]; got != int64(32000) {
		t.Fatalf("explicit max_input_tokens must take priority: %v", got)
	}
	row.contract = capability.Empty("gemini")
	if _, ok := geminiResourceFor(row)["inputTokenLimit"]; ok {
		t.Fatal("unknown input limit must be omitted")
	}
}

func TestReadModelViewsAllowTrailingSlash(t *testing.T) {
	cfg := mustLoad(t, readCfg(t, `
[[models]]
name="m"
provider="up"
model="x"
`))
	srv := httptest.NewServer(New(cfg).Handler())
	defer srv.Close()
	for _, path := range []string{"/v1/models/", "/models/", "/muse-code/models/", "/v1/muse-code/models/"} {
		code, _, body := get(t, srv.URL+path, nil)
		if code != 200 {
			t.Fatalf("GET %s must use its model view: %d %s", path, code, body)
		}
	}
	code, _, _ := get(t, srv.URL+"/models%2f", nil)
	if code == 200 {
		t.Fatal("encoded slash must not be reinterpreted as a resource separator")
	}
}
