package server

import (
	"encoding/json"
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

// Chat/Responses effort -> Gemini thinkingLevel over HTTP: pinned capability
// override selects exact vs drop per model, strict refuses pre-I/O with zero
// upstream calls, lossy continues with the diagnosed header. The fake
// upstream captures the exact generationConfig.thinkingConfig.thinkingLevel
// wire member (official enum, exact uppercase conversion).

func gemLevelBridgeCfg(t *testing.T) *config.Config {
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

[providers.gemup]
dialect = "gemini"
base_url = "http://127.0.0.1:9"
api_key = "gem-secret"

[providers.lossygem]
dialect = "gemini"
base_url = "http://127.0.0.1:9"
api_key = "gem-secret"
compatibility = "lossy"

[[models]]
name = "bridge-gem"
provider = "gemup"
model = "gemini-2.0-flash"

[[models]]
name = "bridge-gem-lossy"
provider = "lossygem"
model = "gemini-2.0-flash"

[capabilities.overrides.bridge-gem.reasoning.effort]
supported = true
values = ["low", "medium", "high"]
[capabilities.overrides.bridge-gem.reasoning.effort.native_parameter]
protocol = "gemini"
path = "generationConfig.thinkingConfig.thinkingLevel"
`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

type gemLevelHarness struct {
	upstream *httptest.Server
	router   *httptest.Server
	mu       sync.Mutex
	lastBody []byte
	lastPath string
	calls    int
}

func newGemLevelHarness(t *testing.T, cfg *config.Config) *gemLevelHarness {
	t.Helper()
	h := &gemLevelHarness{}
	h.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.lastBody = body
		h.lastPath = r.URL.Path
		h.calls++
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"hello"}],"role":"model"},"finishReason":"STOP","index":0}],"modelVersion":"gemini-2.0-flash","responseId":"r1","usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3,"totalTokenCount":10}}`))
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

func gemLevelPost(t *testing.T, url, body string) (int, http.Header, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, data
}

func gemLevelWire(t *testing.T, h *gemLevelHarness) map[string]any {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	var v map[string]any
	if err := json.Unmarshal(h.lastBody, &v); err != nil {
		t.Fatalf("upstream not JSON: %v %s", err, h.lastBody)
	}
	return v
}

func gemLevelCalls(h *gemLevelHarness) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

func gemLevelThinking(t *testing.T, wire map[string]any) map[string]any {
	t.Helper()
	genCfg, ok := wire["generationConfig"].(map[string]any)
	if !ok {
		t.Fatalf("generationConfig missing: %v", wire)
	}
	think, ok := genCfg["thinkingConfig"].(map[string]any)
	if !ok {
		t.Fatalf("thinkingConfig missing: %v", wire)
	}
	return think
}

func TestGemLevelChatExactPinned(t *testing.T) {
	h := newGemLevelHarness(t, gemLevelBridgeCfg(t))
	status, hdr, _ := gemLevelPost(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-gem","messages":[{"role":"user","content":"Hi"}],"reasoning_effort":"medium"}`)
	if status != 200 {
		t.Fatalf("exact status=%d", status)
	}
	if hdr.Get("X-Model-Router-Loss") != "" {
		t.Fatalf("exact must emit no loss header")
	}
	think := gemLevelThinking(t, gemLevelWire(t, h))
	if think["thinkingLevel"] != "MEDIUM" || len(think) != 1 {
		t.Fatalf("wire must carry exact MEDIUM only: %v", think)
	}
}

func TestGemLevelResponsesExactPinned(t *testing.T) {
	h := newGemLevelHarness(t, gemLevelBridgeCfg(t))
	status, hdr, _ := gemLevelPost(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-gem","input":"Hi","reasoning":{"effort":"low"}}`)
	if status != 200 {
		t.Fatalf("exact status=%d", status)
	}
	if hdr.Get("X-Model-Router-Loss") != "" {
		t.Fatalf("exact must emit no loss header")
	}
	think := gemLevelThinking(t, gemLevelWire(t, h))
	if think["thinkingLevel"] != "LOW" || len(think) != 1 {
		t.Fatalf("wire must carry exact LOW only: %v", think)
	}
}

func TestGemLevelStrictDropsPreIO(t *testing.T) {
	h := newGemLevelHarness(t, gemLevelBridgeCfg(t))
	// Enum mismatch drops (xhigh not in the pinned known enum).
	status, _, data := gemLevelPost(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-gem","messages":[{"role":"user","content":"Hi"}],"reasoning_effort":"xhigh"}`)
	if status != 400 || !strings.Contains(string(data), "translation_loss") {
		t.Fatalf("mismatch strict must 400 translation_loss: %d %s", status, data)
	}
	if n := gemLevelCalls(h); n != 0 {
		t.Fatalf("strict must not touch upstream: %d", n)
	}
	// Unreviewed none drops.
	status, _, data = gemLevelPost(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-gem","messages":[{"role":"user","content":"Hi"}],"reasoning_effort":"none"}`)
	if status != 400 || !strings.Contains(string(data), "translation_loss") {
		t.Fatalf("none strict must 400: %d %s", status, data)
	}
	if n := gemLevelCalls(h); n != 0 {
		t.Fatalf("none must not touch upstream: %d", n)
	}
	// Malformed and prohibited summary refuse before I/O.
	for _, body := range []string{
		`{"model":"bridge-gem","messages":[{"role":"user","content":"Hi"}],"reasoning_effort":123}`,
		`{"model":"bridge-gem","messages":[{"role":"user","content":"Hi"}],"reasoning_effort":"medium","extra_body":{"thinking":{"type":"enabled","budget_tokens":100}}}`,
	} {
		status, _, _ := gemLevelPost(t, h.router.URL+"/v1/chat/completions", body)
		if status != 400 {
			t.Fatalf("chat malformed/conflict must 400: %s got %d", body, status)
		}
	}
	for _, body := range []string{
		`{"model":"bridge-gem","input":"Hi","reasoning":{"summary":"detailed"}}`,
		`{"model":"bridge-gem","input":"Hi","reasoning":{"effort":"low","summary":"detailed"}}`,
	} {
		status, _, _ := gemLevelPost(t, h.router.URL+"/v1/responses", body)
		if status != 400 {
			t.Fatalf("responses summary must 400: %s got %d", body, status)
		}
	}
	if n := gemLevelCalls(h); n != 0 {
		t.Fatalf("malformed must not touch upstream: %d", n)
	}
}

func TestGemLevelLossyDropsWithHeader(t *testing.T) {
	h := newGemLevelHarness(t, gemLevelBridgeCfg(t))
	// Same effort without the pinned facts (lossy model has no override)
	// diagnoses a drop and continues without the control.
	status, hdr, _ := gemLevelPost(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-gem-lossy","messages":[{"role":"user","content":"Hi"}],"reasoning_effort":"medium"}`)
	if status != 200 {
		t.Fatalf("lossy status=%d", status)
	}
	loss := hdr.Get("X-Model-Router-Loss")
	if loss == "" || !strings.Contains(loss, "reasoning_effort") {
		t.Fatalf("lossy must carry origin header: %q", loss)
	}
	wire := gemLevelWire(t, h)
	if genCfg, ok := wire["generationConfig"].(map[string]any); ok {
		if _, has := genCfg["thinkingConfig"]; has {
			t.Fatalf("dropped must emit no thinkingConfig: %v", wire)
		}
	}
}
