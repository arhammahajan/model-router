package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arhammahajan/router/internal/config"
)

func mustLoadCfg(t *testing.T, p string) *config.Config {
	t.Helper()
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func gemJSONHandler(body string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

// Anthropic enabled budget -> Gemini wire over HTTP: exact
// generationConfig.thinkingConfig.thinkingBudget only on the pinned
// canonical trusted fact, otherwise a diagnosed drop (strict 400 pre-I/O,
// lossy 200 with header). Covers trusted override, effective dialect, no
// untrusted metadata, independent display loss, both modes and exact wire.

func a2gBudgetCfg(t *testing.T, extra string) string {
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

[providers.chatup]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
api_key = "chat-secret"

[[models]]
name = "anth-gem"
provider = "gemup"
model = "gemini-2.0-flash"

[[models]]
name = "anth-gem-lossy"
provider = "lossygem"
model = "gemini-2.0-flash"

[[models]]
name = "eff-gem"
provider = "chatup"
model = "gemini-2.0-flash"
dialect = "gemini"

[[models]]
name = "alias-gem"
provider = "gemup"
model = "gemini-2.0-flash"
` + extra
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func a2gBudgetGemJSON() string {
	return `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP","index":0}],"modelVersion":"gemini-2.0-flash","responseId":"r1","usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`
}

func a2gBudgetWire(t *testing.T, h *a2gHarness) map[string]any {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	var wire map[string]any
	if err := json.Unmarshal(h.lastBody, &wire); err != nil {
		t.Fatal(err)
	}
	return wire
}

func TestAnthGemBudgetTrustedExact(t *testing.T) {
	p := a2gBudgetCfg(t, `
[capabilities.overrides."anth-gem".reasoning.budget]
supported = true
native_parameter = { protocol = "gemini", path = "generationConfig.thinkingConfig.thinkingBudget" }
`)
	cfg := mustLoadCfg(t, p)
	h := newA2GHarness(t, cfg)
	h.handler = gemJSONHandler(a2gBudgetGemJSON())
	status, hdr, _ := postA2G(t, h.router.URL+"/v1/messages",
		`{"model":"anth-gem","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"enabled","budget_tokens":2048}}`, nil)
	if status != 200 {
		t.Fatalf("trusted must 200, got %d", status)
	}
	if hdr.Get("X-Model-Router-Loss") != "" {
		t.Fatalf("exact must emit no loss: %q", hdr.Get("X-Model-Router-Loss"))
	}
	wire := a2gBudgetWire(t, h)
	genCfg, ok := wire["generationConfig"].(map[string]any)
	if !ok {
		t.Fatalf("generationConfig missing: %v", wire)
	}
	think, ok := genCfg["thinkingConfig"].(map[string]any)
	if !ok {
		t.Fatalf("thinkingConfig missing: %v", wire)
	}
	if v, ok := think["thinkingBudget"].(float64); !ok || int64(v) != 2048 {
		t.Fatalf("wire must carry exact 2048: %v", wire)
	}
	h.mu.Lock()
	raw := string(h.lastBody)
	h.mu.Unlock()
	if !strings.Contains(raw, `"thinkingBudget":2048`) {
		t.Fatalf("exact numeric wire: %s", raw)
	}
	// Model ID / provider URL never gate: upstream model differs from
	// public name yet the trusted fact still maps exactly.
	if h.lastPath != "/v1beta/models/gemini-2.0-flash:generateContent" {
		t.Fatalf("upstream path: %s", h.lastPath)
	}
}

func TestAnthGemBudgetEffectiveDialectAndAlias(t *testing.T) {
	p := a2gBudgetCfg(t, `
[capabilities.overrides."eff-gem".reasoning.budget]
supported = true
native_parameter = { protocol = "gemini", path = "generationConfig.thinkingConfig.thinkingBudget" }

[capabilities.overrides."alias-gem".reasoning.budget]
supported = true
native_parameter = { protocol = "gemini", path = "generationConfig.thinkingConfig.thinkingBudget" }
`)
	cfg := mustLoadCfg(t, p)
	h := newA2GHarness(t, cfg)
	h.handler = gemJSONHandler(a2gBudgetGemJSON())
	// Effective dialect: provider is chat, model route overrides to gemini.
	status, _, _ := postA2G(t, h.router.URL+"/v1/messages",
		`{"model":"eff-gem","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"enabled","budget_tokens":1024}}`, nil)
	if status != 200 {
		t.Fatalf("effective-dialect must route to gemini: %d", status)
	}
	wire := a2gBudgetWire(t, h)
	if think, ok := wire["generationConfig"].(map[string]any)["thinkingConfig"].(map[string]any); !ok || think["thinkingBudget"] != float64(1024) {
		t.Fatalf("effective-dialect wire: %v", wire)
	}
	// Canonical override applies by canonical name (alias-gem is its own
	// canonical row here); an alias-key override never leaks across rows.
	// Resolve alias-gem directly: exact.
	status, _, _ = postA2G(t, h.router.URL+"/v1/messages",
		`{"model":"alias-gem","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"enabled","budget_tokens":512}}`, nil)
	if status != 200 {
		t.Fatalf("canonical override must apply: %d", status)
	}
}

func TestAnthGemBudgetNoUntrustedMetadata(t *testing.T) {
	p := a2gBudgetCfg(t, "")
	cfg := mustLoadCfg(t, p)
	// Untrusted per-model metadata must never gate the wire: even a
	// budget-shaped router blob stays ignored (unknown neutral => drop).
	for i := range cfg.Models {
		if cfg.Models[i].Name == "anth-gem" {
			cfg.Models[i].Metadata = json.RawMessage(`{"router":{"reasoning":{"budget":{"supported":{"value":true},"native_parameter":{"value":{"protocol":"gemini","path":"generationConfig.thinkingConfig.thinkingBudget"}}}}}}`)
		}
	}
	h := newA2GHarness(t, cfg)
	status, _, _ := postA2G(t, h.router.URL+"/v1/messages",
		`{"model":"anth-gem","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"enabled","budget_tokens":1024}}`, nil)
	if status != 400 {
		t.Fatalf("metadata must be ignored (strict drop), got %d", status)
	}
	if got := a2gCalls(h); got != 0 {
		t.Fatalf("drop must precede I/O, got %d", got)
	}
}

func TestAnthGemBudgetDisplayLossBothModes(t *testing.T) {
	p := a2gBudgetCfg(t, `
[capabilities.overrides."anth-gem".reasoning.budget]
supported = true
native_parameter = { protocol = "gemini", path = "generationConfig.thinkingConfig.thinkingBudget" }

[capabilities.overrides."anth-gem-lossy".reasoning.budget]
supported = true
native_parameter = { protocol = "gemini", path = "generationConfig.thinkingConfig.thinkingBudget" }
`)
	cfg := mustLoadCfg(t, p)
	h := newA2GHarness(t, cfg)
	h.handler = gemJSONHandler(a2gBudgetGemJSON())
	// Strict: display loss refuses pre-I/O, no upstream call.
	status, _, data := postA2G(t, h.router.URL+"/v1/messages",
		`{"model":"anth-gem","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"enabled","budget_tokens":512,"display":"summarized"}}`, nil)
	if status != 400 || !strings.Contains(string(data), "thinking.display") {
		t.Fatalf("strict display must 400 thinking.display, got %d %s", status, data)
	}
	if got := a2gCalls(h); got != 0 {
		t.Fatalf("strict display must precede I/O, got %d", got)
	}
	// Lossy: exact budget wire plus display loss header.
	status, hdr, _ := postA2G(t, h.router.URL+"/v1/messages",
		`{"model":"anth-gem-lossy","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"enabled","budget_tokens":512,"display":"omitted"}}`, nil)
	if status != 200 || hdr.Get("X-Model-Router-Loss") == "" {
		t.Fatalf("lossy display must 200 with header, got %d %q", status, hdr.Get("X-Model-Router-Loss"))
	}
	if !strings.Contains(hdr.Get("X-Model-Router-Loss"), "thinking.display") {
		t.Fatalf("header must name display: %q", hdr.Get("X-Model-Router-Loss"))
	}
	wire := a2gBudgetWire(t, h)
	if think, ok := wire["generationConfig"].(map[string]any)["thinkingConfig"].(map[string]any); !ok || think["thinkingBudget"] != float64(512) {
		t.Fatalf("lossy must still carry exact budget: %v", wire)
	}
}

func TestAnthGemBudgetAlternatePathDrops(t *testing.T) {
	p := a2gBudgetCfg(t, `
[capabilities.overrides."anth-gem".reasoning.budget]
supported = true
native_parameter = { protocol = "openai-chat", path = "reasoning_effort" }
`)
	cfg := mustLoadCfg(t, p)
	h := newA2GHarness(t, cfg)
	status, _, _ := postA2G(t, h.router.URL+"/v1/messages",
		`{"model":"anth-gem","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"enabled","budget_tokens":1024}}`, nil)
	if status != 400 {
		t.Fatalf("alternate native path must drop (400), got %d", status)
	}
	if got := a2gCalls(h); got != 0 {
		t.Fatalf("alternate drop must precede I/O, got %d", got)
	}
}
