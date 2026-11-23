package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/arhammahajan/router/internal/config"
)

// Review: with a custom prefix alias, native and bridge still share the one
// BaseURL-derived generation endpoint; the alias is admission-only.
func TestReviewAliasCustomPrefixOneEndpoint(t *testing.T) {
	body := `
[server]
auth_mode = "none"
allow_dynamic_routes = false
max_generations = 8

[retry]
max_attempts = 1

[providers.anthup]
dialect = "anthropic"
base_url = "http://127.0.0.1:9/custom/prefix"
api_key = "anth-secret"
timeout = 30

[providers.anthup.litellm_kwargs]
api_base = "http://127.0.0.1:9/custom/prefix/v1/messages"
timeout = 30

[[models]]
name = "native-claude"
provider = "anthup"
model = "claude-haiku-4-5"
`
	p := filepath.Join(t.TempDir(), "router.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("redundant prefix alias must load: %v", err)
	}
	if cfg.Providers["anthup"].BaseURL != "http://127.0.0.1:9/custom/prefix" {
		t.Fatalf("BaseURL must be unchanged, got %q", cfg.Providers["anthup"].BaseURL)
	}
	h := &nativeHarness{}
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}
	h.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.lastPath = r.URL.Path
		h.lastRaw = r.URL.RawQuery
		h.lastBody = body
		h.lastHdr = r.Header.Clone()
		h.calls++
		fn := h.handler
		h.mu.Unlock()
		fn(w, r)
	}))
	t.Cleanup(h.upstream.Close)
	for _, prov := range cfg.Providers {
		prov.BaseURL = h.upstream.URL + "/custom/prefix"
	}
	srv := New(cfg)
	h.router = httptest.NewServer(srv.Handler())
	t.Cleanup(h.router.Close)
	status, _, data := postNative(t, h.router.URL+"/v1/messages",
		`{"model":"native-claude","max_tokens":8,"messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 200 {
		t.Fatalf("native status=%d body=%s", status, data)
	}
	h.mu.Lock()
	nativePath := h.lastPath
	h.mu.Unlock()
	if nativePath != "/custom/prefix/v1/messages" {
		t.Fatalf("native upstream path %q", nativePath)
	}
	status, _, data = postNative(t, h.router.URL+"/v1/chat/completions",
		`{"model":"native-claude","messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 200 {
		t.Fatalf("bridge status=%d body=%s", status, data)
	}
	h.mu.Lock()
	bridgePath := h.lastPath
	h.mu.Unlock()
	if bridgePath != "/custom/prefix/v1/messages" {
		t.Fatalf("bridge upstream path %q (want same /custom/prefix/v1/messages)", bridgePath)
	}
}
