package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/arhammahajan/router/internal/config"
)

func legacyWireCfg(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "router.toml")
	body := `
[server]
auth_mode = "none"
allow_dynamic_routes = false
max_generations = 8

[retry]
max_attempts = 1

[providers.anthup]
dialect = "anthropic"
base_url = "http://127.0.0.1:9"
api_key = "anth-secret"
headers = { "X-Prov" = "prov", "X-Shared" = "prov-shared" }

[providers.anthup.litellm_kwargs]
headers = { "X-Legacy" = "legacy", "X-Shared" = "legacy-shared-lo", "x-case" = "legacy-lo" }
extra_headers = { "X-Shared" = "legacy-shared", "X-CASE" = "legacy-hi" }

[[models]]
name = "native-claude"
provider = "anthup"
model = "claude-haiku-4-5"
`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("legacy wire config must load: %v", err)
	}
	return cfg
}

type legacyWireHarness struct {
	upstream *httptest.Server
	router   *httptest.Server
	mu       sync.Mutex
	lastHdr  http.Header
	lastPath string
	lastBody []byte
}

func newLegacyWireHarness(t *testing.T, cfg *config.Config) *legacyWireHarness {
	t.Helper()
	h := &legacyWireHarness{}
	h.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.lastHdr = r.Header.Clone()
		h.lastPath = r.URL.Path
		h.lastBody = body
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		// Minimal valid Chat and Anthropic payloads.
		if strings.Contains(r.URL.Path, "chat") {
			_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	t.Cleanup(h.upstream.Close)
	for _, prov := range cfg.Providers {
		prov.BaseURL = h.upstream.URL
	}
	srv := New(cfg)
	h.router = httptest.NewServer(srv.Handler())
	t.Cleanup(h.router.Close)
	return h
}

func TestLegacyHeadersNativeVsBridge(t *testing.T) {
	h := newLegacyWireHarness(t, legacyWireCfg(t))
	// Native: Anthropic ingress to Anthropic upstream must NOT carry legacy.
	reqBody := `{"model":"native-claude","max_tokens":8,"messages":[{"role":"user","content":"Hi"}]}`
	req, _ := http.NewRequest(http.MethodPost, h.router.URL+"/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("native status=%d", resp.StatusCode)
	}
	h.mu.Lock()
	nativeHdr := h.lastHdr.Clone()
	h.mu.Unlock()
	if v := nativeHdr.Get("X-Legacy"); v != "" {
		t.Fatalf("native must not carry legacy X-Legacy=%q", v)
	}
	if v := nativeHdr.Get("X-Prov"); v != "prov" {
		t.Fatalf("native must carry provider header, got %q", v)
	}

	// Bridge: Chat ingress to Anthropic upstream must carry legacy + provider wins.
	chatBody := `{"model":"native-claude","messages":[{"role":"user","content":"Hi"}]}`
	req2, _ := http.NewRequest(http.MethodPost, h.router.URL+"/v1/chat/completions", strings.NewReader(chatBody))
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	_, _ = io.ReadAll(resp2.Body)
	if resp2.StatusCode != 200 {
		t.Fatalf("bridge status=%d", resp2.StatusCode)
	}
	h.mu.Lock()
	bridgeHdr := h.lastHdr.Clone()
	h.mu.Unlock()
	if v := bridgeHdr.Get("X-Legacy"); v != "legacy" {
		t.Fatalf("bridge must carry legacy X-Legacy, got %q in %v", v, bridgeHdr)
	}
	if v := bridgeHdr.Get("X-Shared"); v != "prov-shared" {
		t.Fatalf("bridge provider must beat legacy X-Shared, got %q in %v", v, bridgeHdr)
	}
	if v := bridgeHdr.Get("X-Case"); v != "prov-shared" && v != "legacy-hi" && v != "" {
		t.Logf("case header: %q", v)
	}
	// Case-insensitive merge: extra_headers beats headers.
	if v := bridgeHdr.Get("X-Case"); v != "legacy-hi" {
		t.Fatalf("extra_headers must beat headers case-insensitively, got %q in %v", v, bridgeHdr)
	}
}
