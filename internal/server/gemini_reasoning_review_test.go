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

	"github.com/arhammahajan/router/internal/capability"
	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/router"
)

// Independent adversarial review of Gemini thinking -> Chat bridges over
// pinned local runtimes. Hermetic httptest only; no env, network beyond
// loopback, no Python tree. Covers what the ordinary-bridge tests do not:
// LlamaCpp exact wire, Strata literal, Unobserved strict-400-zero-IO vs
// lossy-drop, stale freshness-neutral, thoughts composition, value-free
// diagnostics, XHIGH/malformed pre-IO in both modes.

const revGemChatOK = `{"id":"chatcmpl-up","object":"chat.completion","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`

type revGemHarness struct {
	upstream *httptest.Server
	router   *httptest.Server
	mu       sync.Mutex
	lastPath string
	lastBody []byte
	calls    int
	handler  func(w http.ResponseWriter, r *http.Request)
}

func revGemLlamaCfg(t *testing.T, serverURL, compat string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "router.toml")
	comp := ""
	if compat == "lossy" {
		comp = `compatibility = "lossy"`
	}
	body := `
[server]
auth_mode = "none"
allow_dynamic_routes = false
max_generations = 8
[retry]
max_attempts = 1
[llama_cpp]
` + comp + `
[[llama_cpp.servers]]
name = "llama-test"
url = "` + serverURL + `"
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

func revGemServe(t *testing.T, cfg *config.Config, rt capability.LocalRuntime, state router.LlamaState) *revGemHarness {
	t.Helper()
	h := &revGemHarness{}
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(revGemChatOK))
	}
	h.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.lastPath = r.URL.Path
		h.lastBody = body
		h.calls++
		fn := h.handler
		h.mu.Unlock()
		fn(w, r)
	}))
	t.Cleanup(h.upstream.Close)
	// Point the trusted llama inventory at the fake upstream.
	for _, s := range cfg.Llama.Servers {
		_ = s
	}
	cfg.Llama.Servers[0].URL = h.upstream.URL
	reg := router.NewRegistry(cfg)
	layer := router.LlamaLayer{Servers: map[string]router.LlamaServerSnapshot{
		"llama-test": {
			State:   state,
			Runtime: rt,
			Entries: []router.LlamaEntry{{Name: "llama-chat", Upstream: "llama-chat"}},
		},
	}}
	if err := reg.CommitLlama(router.LlamaAttempt{Layer: layer, AtWall: router.Wall(1000)}); err != nil {
		t.Fatal(err)
	}
	snap := reg.Current()
	if !snap.HasLlamaProvider("llama-test") {
		t.Fatalf("trusted inventory must mark llama provider (state=%v rt=%v)", state, rt)
	}
	srv := NewWithRegistry(cfg, reg)
	h.router = httptest.NewServer(srv.Handler())
	t.Cleanup(h.router.Close)
	return h
}

func revGemPost(t *testing.T, url, body string) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, data
}

func revGemWire(t *testing.T, h *revGemHarness) map[string]any {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	var wire map[string]any
	if err := json.Unmarshal(h.lastBody, &wire); err != nil {
		t.Fatal(err)
	}
	return wire
}

func revGemCalls(h *revGemHarness) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

func revGemReq(thinking string) string {
	return `{"contents":[{"role":"user","parts":[{"text":"Hi"}]}],"generationConfig":{"thinkingConfig":{` + thinking + `}}}`
}

func TestRevGemLlamaCppExactWire(t *testing.T) {
	h := revGemServe(t, revGemLlamaCfg(t, "http://127.0.0.1:9", "strict"), capability.RuntimeLlamaCpp, router.LlamaReachable)
	// Explicit budget renders the exact native member.
	status, headers, data := revGemPost(t, h.router.URL+"/v1beta/models/llama-chat:generateContent", revGemReq(`"thinkingBudget":2048`))
	if status != 200 {
		t.Fatalf("budget status=%d body=%s", status, data)
	}
	if headers.Get("X-Model-Router-Loss") != "" {
		t.Fatalf("exact must carry no loss header")
	}
	if wire := revGemWire(t, h); wire["thinking_budget_tokens"] != float64(2048) {
		t.Fatalf("llama budget wire: %v", wire)
	}
	// Off renders the native toggle.
	status, _, data = revGemPost(t, h.router.URL+"/v1beta/models/llama-chat:generateContent", revGemReq(`"thinkingBudget":0`))
	if status != 200 {
		t.Fatalf("off status=%d body=%s", status, data)
	}
	wire := revGemWire(t, h)
	kw, _ := wire["chat_template_kwargs"].(map[string]any)
	if kw["enable_thinking"] != false {
		t.Fatalf("off wire: %v", wire)
	}
	// Named level maps through the shared literal table (medium=8192).
	status, _, data = revGemPost(t, h.router.URL+"/v1beta/models/llama-chat:generateContent", revGemReq(`"thinkingLevel":"MEDIUM"`))
	if status != 200 {
		t.Fatalf("named status=%d body=%s", status, data)
	}
	if wire := revGemWire(t, h); wire["thinking_budget_tokens"] != float64(8192) {
		t.Fatalf("llama medium wire: %v", wire)
	}
}

func TestRevGemStrataLiteralBudgetDrop(t *testing.T) {
	h := revGemServe(t, revGemLlamaCfg(t, "http://127.0.0.1:9", "strict"), capability.RuntimeStrata, router.LlamaReachable)
	// Named stays literal on Strata.
	status, _, data := revGemPost(t, h.router.URL+"/v1beta/models/llama-chat:generateContent", revGemReq(`"thinkingLevel":"MEDIUM"`))
	if status != 200 {
		t.Fatalf("strata named status=%d body=%s", status, data)
	}
	if wire := revGemWire(t, h); wire["reasoning_effort"] != "medium" {
		t.Fatalf("strata must keep literal: %v", wire)
	}
	// Explicit budget drops pre-IO on Strata (never 500 misuse).
	before := revGemCalls(h)
	status, _, data = revGemPost(t, h.router.URL+"/v1beta/models/llama-chat:generateContent", revGemReq(`"thinkingBudget":2048`))
	if status != 400 || !strings.Contains(string(data), "TRANSLATION_LOSS") {
		t.Fatalf("strata budget strict status=%d body=%s", status, data)
	}
	if n := revGemCalls(h); n != before {
		t.Fatalf("strict must do zero upstream IO")
	}
}

func TestRevGemUnobservedStrictLossy(t *testing.T) {
	hs := revGemServe(t, revGemLlamaCfg(t, "http://127.0.0.1:9", "strict"), capability.RuntimeUnobserved, router.LlamaReachable)
	hl := revGemServe(t, revGemLlamaCfg(t, "http://127.0.0.1:9", "lossy"), capability.RuntimeUnobserved, router.LlamaReachable)
	// Present intent on unknown cached runtime: strict 400 zero-IO.
	before := revGemCalls(hs)
	status, _, data := revGemPost(t, hs.router.URL+"/v1beta/models/llama-chat:generateContent", revGemReq(`"thinkingBudget":100`))
	if status != 400 || !strings.Contains(string(data), "TRANSLATION_LOSS") {
		t.Fatalf("unobserved strict status=%d body=%s", status, data)
	}
	if n := revGemCalls(hs); n != before {
		t.Fatalf("strict unobserved must do zero upstream IO")
	}
	// Lossy drops the control, forwards without it, reports the header.
	status, headers, _ := revGemPost(t, hl.router.URL+"/v1beta/models/llama-chat:generateContent", revGemReq(`"thinkingBudget":100`))
	if status != 200 {
		t.Fatalf("lossy status=%d", status)
	}
	if headers.Get("X-Model-Router-Loss") == "" {
		t.Fatalf("lossy must carry the loss header")
	}
	if strings.Contains(headers.Get("X-Model-Router-Loss"), "100") {
		t.Fatalf("loss header must be static value-free: %q", headers.Get("X-Model-Router-Loss"))
	}
	wire := revGemWire(t, hl)
	if _, has := wire["thinking_budget_tokens"]; has {
		t.Fatalf("dropped budget must emit nothing: %v", wire)
	}
	if _, has := wire["reasoning_effort"]; has {
		t.Fatalf("dropped budget must emit no effort: %v", wire)
	}
	// Absent bypasses the gate entirely: -1, UNSPECIFIED, empty.
	for _, th := range []string{`"thinkingBudget":-1`, `"thinkingLevel":"THINKING_LEVEL_UNSPECIFIED"`, ``} {
		status, headers, data := revGemPost(t, hs.router.URL+"/v1beta/models/llama-chat:generateContent", revGemReq(th))
		if status != 200 {
			t.Fatalf("%q status=%d body=%s", th, status, data)
		}
		if headers.Get("X-Model-Router-Loss") != "" {
			t.Fatalf("%q absent must carry no loss header", th)
		}
		wire := revGemWire(t, hs)
		if _, has := wire["reasoning_effort"]; has {
			t.Fatalf("%q must emit nothing: %v", th, wire)
		}
	}
}

func TestRevGemStaleRetainsRuntime(t *testing.T) {
	// Freshness alone adds no refusal: stale LlamaCpp keeps exact wire.
	h := revGemServe(t, revGemLlamaCfg(t, "http://127.0.0.1:9", "strict"), capability.RuntimeLlamaCpp, router.LlamaUnreachableStale)
	status, headers, data := revGemPost(t, h.router.URL+"/v1beta/models/llama-chat:generateContent", revGemReq(`"thinkingBudget":2048`))
	if status != 200 {
		t.Fatalf("stale exact status=%d body=%s", status, data)
	}
	if headers.Get("X-Model-Router-Loss") != "" {
		t.Fatalf("stale exact must carry no loss header")
	}
	if wire := revGemWire(t, h); wire["thinking_budget_tokens"] != float64(2048) {
		t.Fatalf("stale wire: %v", wire)
	}
}

func TestRevGemThoughtsStrictLossyOrdinary(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(gemReasonChatOK))
	}
	// includeThoughts-only on strict: 400 translation_loss from the existing
	// thoughts loss, zero upstream IO - not a fake intent or refusal code.
	before := gemReasonCalls(h)
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-chat:generateContent", revGemReq(`"includeThoughts":true`), nil)
	if status != 400 || !strings.Contains(string(data), "TRANSLATION_LOSS") {
		t.Fatalf("thoughts strict status=%d body=%s", status, data)
	}
	if n := gemReasonCalls(h); n != before {
		t.Fatalf("thoughts strict must do zero upstream IO")
	}
	// Lossy keeps the independent loss in the header and emits no control.
	status, headers, _ := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-chat-lossy:generateContent", revGemReq(`"includeThoughts":true`), nil)
	if status != 200 {
		t.Fatalf("thoughts lossy status=%d", status)
	}
	if headers.Get("X-Model-Router-Loss") == "" {
		t.Fatalf("thoughts lossy must carry the loss header")
	}
	wire := gemReasonWire(t, h)
	if _, has := wire["reasoning_effort"]; has {
		t.Fatalf("thoughts-only must emit no effort: %v", wire)
	}
}

func TestRevGemMalformedBothModesPreIO(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(gemReasonChatOK))
	}
	for _, th := range []string{
		`"thinkingLevel":"XHIGH"`,
		`"thinkingBudget":100,"thinkingLevel":"LOW"`,
		`"thinkingBudget":true`,
		`"thinkingBudget":1.5`,
		`"bogus":1`,
	} {
		for _, model := range []string{"gem-chat", "gem-chat-lossy"} {
			before := gemReasonCalls(h)
			status, _, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/"+model+":generateContent", revGemReq(th), nil)
			if status != 400 {
				t.Fatalf("%q %s status=%d body=%s", th, model, status, data)
			}
			if n := gemReasonCalls(h); n != before {
				t.Fatalf("%q %s must do zero upstream IO", th, model)
			}
		}
	}
}
