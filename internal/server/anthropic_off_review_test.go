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
)

// Independent adversarial review for Anthropic disabled bridge planning.
// Scope: same pinned p.snap -> RequestCapability -> ReasoningFor ->
// LocalRuntime+Effective dest -> PlanEffort -> EnforcePolicy -> encoder;
// gate order, strict 0 I/O, loss headers, native passthrough, auth.

type offRevHarness struct {
	upstream *httptest.Server
	router   *httptest.Server
	mu       sync.Mutex
	lastBody []byte
	calls    int
}

func offRevCfg(t *testing.T, extra string) *config.Config {
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

[providers.chatup]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
api_key = "chat-secret"

[providers.lossychat]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
api_key = "chat-secret"
compatibility = "lossy"

[providers.respup]
dialect = "openai-responses"
base_url = "http://127.0.0.1:9"
api_key = "resp-secret"

[providers.lossyresp]
dialect = "openai-responses"
base_url = "http://127.0.0.1:9"
api_key = "resp-secret"
compatibility = "lossy"

[providers.anthup]
dialect = "anthropic"
base_url = "http://127.0.0.1:9"
api_key = "anth-secret"

[[models]]
name = "r-chat"
provider = "chatup"
model = "gpt-4o-mini"

[[models]]
name = "r-chat-lossy"
provider = "lossychat"
model = "gpt-4o-mini"

[[models]]
name = "r-resp"
provider = "respup"
model = "gpt-4.1"

[[models]]
name = "r-resp-lossy"
provider = "lossyresp"
model = "gpt-4.1"

[[models]]
name = "r-native"
provider = "anthup"
model = "claude-haiku-4-5"

[[models]]
name = "r-alias-target"
provider = "chatup"
model = "gpt-4o-mini"
aliases = ["r-alias"]
` + extra
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

const offRevChatResp = `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`
const offRevRespResp = `{"id":"resp_up","object":"response","created_at":1,"status":"completed","model":"gpt-4.1","output":[{"id":"m","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hello","annotations":[]}]}],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}`
const offRevNativeResp = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-4-5","content":[{"type":"text","text":"native-hi"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":2}}`

func offRevNew(t *testing.T, cfg *config.Config) *offRevHarness {
	t.Helper()
	h := &offRevHarness{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.lastBody = append([]byte(nil), body...)
		h.calls++
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		switch {
		case strings.HasPrefix(p, "/responses"):
			_, _ = w.Write([]byte(offRevRespResp))
		case strings.HasPrefix(p, "/messages"):
			_, _ = w.Write([]byte(offRevNativeResp))
		default:
			_, _ = w.Write([]byte(offRevChatResp))
		}
	}))
	t.Cleanup(up.Close)
	for _, prov := range cfg.Providers {
		prov.BaseURL = up.URL
	}
	h.upstream = up
	srv := New(cfg)
	h.router = httptest.NewServer(srv.Handler())
	t.Cleanup(h.router.Close)
	return h
}

func offRevCalls(h *offRevHarness) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

func offRevWire(t *testing.T, h *offRevHarness) map[string]any {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	var v map[string]any
	if err := json.Unmarshal(h.lastBody, &v); err != nil {
		t.Fatalf("upstream not JSON: %v %s", err, h.lastBody)
	}
	return v
}

func offRevRaw(h *offRevHarness) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return string(h.lastBody)
}

func TestOffRevChatResponsesExact(t *testing.T) {
	h := offRevNew(t, offRevCfg(t, ""))
	for _, tc := range []struct {
		model string
		check func(wire map[string]any)
	}{
		{"r-chat", func(w map[string]any) {
			if w["reasoning_effort"] != "none" {
				t.Fatalf("chat wire none: %v", w)
			}
			if _, has := w["thinking"]; has {
				t.Fatalf("no thinking leak: %v", w)
			}
		}},
		{"r-resp", func(w map[string]any) {
			reas, ok := w["reasoning"].(map[string]any)
			if !ok || reas["effort"] != "none" {
				t.Fatalf("responses wire none: %v", w)
			}
			if _, has := w["reasoning_effort"]; has {
				t.Fatalf("responses must not emit chat field: %v", w)
			}
		}},
	} {
		status, hdr, _ := postBridge(t, h.router.URL+"/v1/messages",
			`{"model":"`+tc.model+`","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"disabled"}}`, nil)
		if status != 200 || hdr.Get("X-Model-Router-Loss") != "" {
			t.Fatalf("%s exact no header: %d %q", tc.model, status, hdr.Get("X-Model-Router-Loss"))
		}
		tc.check(offRevWire(t, h))
	}
}

func TestOffRevAliasExact(t *testing.T) {
	h := offRevNew(t, offRevCfg(t, ""))
	status, hdr, _ := postBridge(t, h.router.URL+"/v1/messages",
		`{"model":"r-alias","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"disabled"}}`, nil)
	if status != 200 || hdr.Get("X-Model-Router-Loss") != "" {
		t.Fatalf("alias exact: %d %q", status, hdr.Get("X-Model-Router-Loss"))
	}
	if got := offRevWire(t, h); got["reasoning_effort"] != "none" {
		t.Fatalf("alias wire none: %v", got)
	}
}

func TestOffRevToggleFalseStrictLossy(t *testing.T) {
	extra := "[capabilities.overrides.r-chat.reasoning.toggle]\nsupported = false\n[capabilities.overrides.r-chat-lossy.reasoning.toggle]\nsupported = false\n"
	h := offRevNew(t, offRevCfg(t, extra))
	status, _, data := postBridge(t, h.router.URL+"/v1/messages",
		`{"model":"r-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"disabled"}}`, nil)
	if status != 400 || !strings.Contains(string(data), "thinking") {
		t.Fatalf("strict toggle 400 thinking: %d %s", status, data)
	}
	if got := offRevCalls(h); got != 0 {
		t.Fatalf("strict 0 I/O: %d", got)
	}
	status, hdr, _ := postBridge(t, h.router.URL+"/v1/messages",
		`{"model":"r-chat-lossy","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"disabled"}}`, nil)
	if status != 200 || hdr.Get("X-Model-Router-Loss") == "" || !strings.Contains(hdr.Get("X-Model-Router-Loss"), "thinking") {
		t.Fatalf("lossy drop header thinking: %d %q", status, hdr.Get("X-Model-Router-Loss"))
	}
	if _, has := offRevWire(t, h)["reasoning_effort"]; has {
		t.Fatalf("dropped emits nothing: %v", offRevWire(t, h))
	}
}

func TestOffRevKnownEmptyStrictLossy(t *testing.T) {
	extra := "[capabilities.overrides.r-chat.reasoning.effort]\nvalues = []\n[capabilities.overrides.r-chat-lossy.reasoning.effort]\nvalues = []\n"
	h := offRevNew(t, offRevCfg(t, extra))
	status, _, data := postBridge(t, h.router.URL+"/v1/messages",
		`{"model":"r-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"disabled"}}`, nil)
	if status != 400 || !strings.Contains(string(data), "thinking") {
		t.Fatalf("known-empty strict 400: %d %s", status, data)
	}
	if got := offRevCalls(h); got != 0 {
		t.Fatalf("strict 0 I/O: %d", got)
	}
	status, hdr, _ := postBridge(t, h.router.URL+"/v1/messages",
		`{"model":"r-chat-lossy","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"disabled"}}`, nil)
	if status != 200 || !strings.Contains(hdr.Get("X-Model-Router-Loss"), "thinking") {
		t.Fatalf("known-empty lossy header: %d %q", status, hdr.Get("X-Model-Router-Loss"))
	}
}

func TestOffRevSupportedFalseStrips(t *testing.T) {
	extra := "[capabilities.overrides.r-chat.reasoning]\nsupported = false\n"
	h := offRevNew(t, offRevCfg(t, extra))
	status, hdr, _ := postBridge(t, h.router.URL+"/v1/messages",
		`{"model":"r-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"disabled"}}`, nil)
	if status != 200 || hdr.Get("X-Model-Router-Loss") != "" {
		t.Fatalf("supported=false strips no loss: %d %q", status, hdr.Get("X-Model-Router-Loss"))
	}
	if _, has := offRevWire(t, h)["reasoning_effort"]; has {
		t.Fatalf("stripped emits nothing: %v", offRevWire(t, h))
	}
}

func TestOffRevMalformedBeatsLoss(t *testing.T) {
	h := offRevNew(t, offRevCfg(t, ""))
	for _, raw := range []string{
		`{"model":"r-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"disabled","budget_tokens":1}}`,
		`{"model":"r-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"disabled","summary":{}}}`,
		`{"model":"r-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"enabled","budget_tokens":1024}}`,
	} {
		status, _, _ := postBridge(t, h.router.URL+"/v1/messages", raw, nil)
		if status != 400 {
			t.Fatalf("malformed 400: %s got %d", raw, status)
		}
	}
	if got := offRevCalls(h); got != 0 {
		t.Fatalf("malformed 0 I/O: %d", got)
	}
	raw := `{"model":"r-chat-lossy","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"disabled","budget_tokens":1},"service_tier":"auto"}`
	status, _, _ := postBridge(t, h.router.URL+"/v1/messages", raw, nil)
	if status != 400 {
		t.Fatalf("malformed beats other loss even lossy: %d", status)
	}
}

func TestOffRevNullPlusLoss(t *testing.T) {
	h := offRevNew(t, offRevCfg(t, ""))
	status, _, data := postBridge(t, h.router.URL+"/v1/messages",
		`{"model":"r-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":null,"service_tier":"auto"}`, nil)
	if status != 400 || !strings.Contains(string(data), "service_tier") || strings.Contains(string(data), "thinking\":") {
		t.Fatalf("null+loss names service_tier only: %d %s", status, data)
	}
	status, hdr, _ := postBridge(t, h.router.URL+"/v1/messages",
		`{"model":"r-chat-lossy","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":null,"service_tier":"auto"}`, nil)
	if status != 200 || !strings.Contains(hdr.Get("X-Model-Router-Loss"), "service_tier") {
		t.Fatalf("lossy null+loss: %d %q", status, hdr.Get("X-Model-Router-Loss"))
	}
	if strings.Contains(hdr.Get("X-Model-Router-Loss"), "thinking") {
		t.Fatalf("null thinking must not be a loss: %q", hdr.Get("X-Model-Router-Loss"))
	}
	if _, has := offRevWire(t, h)["reasoning_effort"]; has {
		t.Fatalf("absent emits nothing: %v", offRevWire(t, h))
	}
}

func TestOffRevNativePassthrough(t *testing.T) {
	h := offRevNew(t, offRevCfg(t, ""))
	status, hdr, _ := postBridge(t, h.router.URL+"/v1/messages",
		`{"model":"r-native","max_tokens":64,"messages":[{"role":"user","content":"Hi"},{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"get_time","input":{"n":9007199254740993}}]}],"thinking":{"type":"disabled"},"custom_note":"keep-me"}`, nil)
	if status != 200 || hdr.Get("X-Model-Router-Loss") != "" {
		t.Fatalf("native bypass: %d %q", status, hdr.Get("X-Model-Router-Loss"))
	}
	raw := offRevRaw(h)
	if !strings.Contains(raw, `"disabled"`) || !strings.Contains(raw, "9007199254740993") || !strings.Contains(raw, "keep-me") {
		t.Fatalf("native preserves thinking+bigint+unknown verbatim: %s", raw)
	}
}

func TestOffRevOutputConfigCoexist(t *testing.T) {
	h := offRevNew(t, offRevCfg(t, ""))
	status, _, data := postBridge(t, h.router.URL+"/v1/messages",
		`{"model":"r-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"disabled"},"output_config":{"effort":"high"}}`, nil)
	if status != 400 || !strings.Contains(string(data), "output_config") {
		t.Fatalf("strict output_config 400: %d %s", status, data)
	}
	if got := offRevCalls(h); got != 0 {
		h.mu.Lock()
		n := h.calls
		h.mu.Unlock()
		_ = n
		t.Fatalf("strict 0 I/O: %d", got)
	}
	status, hdr, _ := postBridge(t, h.router.URL+"/v1/messages",
		`{"model":"r-chat-lossy","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"disabled"},"output_config":{"effort":"high"}}`, nil)
	if status != 200 || !strings.Contains(hdr.Get("X-Model-Router-Loss"), "output_config") {
		t.Fatalf("lossy output_config header: %d %q", status, hdr.Get("X-Model-Router-Loss"))
	}
	if strings.Contains(hdr.Get("X-Model-Router-Loss"), "thinking") {
		t.Fatalf("disabled not a loss alongside output_config: %q", hdr.Get("X-Model-Router-Loss"))
	}
	if got := offRevWire(t, h); got["reasoning_effort"] != "none" {
		t.Fatalf("disabled still none: %v", got)
	}
}

func TestOffRevStrataAndUnobserved(t *testing.T) {
	// Confirmed Strata supports native Anthropic (/v1/messages), so the
	// Anthropic thinking payload passes through untouched without Chat
	// translation.
	up, rsrv, _ := llamaVerticalServer(t, capability.RuntimeStrata, "")
	status, hdr, _ := postBridge(t, rsrv.URL+"/v1/messages",
		`{"model":"llama-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"disabled"}}`, nil)
	if status != 200 {
		t.Fatalf("strata native anthropic: %d", status)
	}
	if hdr.Get("X-Model-Router-Loss") != "" {
		t.Fatalf("strata native anthropic must emit no loss: %q", hdr.Get("X-Model-Router-Loss"))
	}
	got := up.last(t)
	thinking, ok := got["thinking"].(map[string]any)
	if !ok || thinking["type"] != "disabled" {
		t.Fatalf("strata native anthropic must preserve thinking untouched: %v", got)
	}
	if _, has := got["reasoning_effort"]; has {
		t.Fatalf("strata native anthropic must not synthesize reasoning_effort: %v", got)
	}
	up2, rsrv2, _ := llamaVerticalServer(t, capability.RuntimeUnobserved, "")
	status, _, data := postBridge(t, rsrv2.URL+"/v1/messages",
		`{"model":"llama-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"disabled"}}`, nil)
	if status != 400 || !strings.Contains(string(data), "thinking") {
		t.Fatalf("unobserved strict diagnosed 400: %d %s", status, data)
	}
	if n := len(up2.bodies); n != 0 {
		t.Fatalf("unobserved strict 0 I/O: %d", n)
	}
	up3, rsrv3, _ := llamaVerticalServer(t, capability.RuntimeUnobserved, "compatibility = \"lossy\"\n")
	status, hdr, _ = postBridge(t, rsrv3.URL+"/v1/messages",
		`{"model":"llama-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"disabled"}}`, nil)
	if status != 200 || !strings.Contains(hdr.Get("X-Model-Router-Loss"), "thinking") {
		t.Fatalf("unobserved lossy header: %d %q", status, hdr.Get("X-Model-Router-Loss"))
	}
	_ = up3
}

func TestOffRevLlamaExactNoHeader(t *testing.T) {
	// Confirmed llama_cpp supports native Anthropic (/v1/messages), so the
	// Anthropic thinking payload passes through untouched with no loss header.
	up, rsrv, _ := llamaVerticalServer(t, capability.RuntimeLlamaCpp, "")
	status, hdr, _ := postBridge(t, rsrv.URL+"/v1/messages",
		`{"model":"llama-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"disabled"}}`, nil)
	if status != 200 {
		t.Fatalf("llama native anthropic off: %d", status)
	}
	if hdr.Get("X-Model-Router-Loss") != "" {
		t.Fatalf("llama native anthropic must emit no loss: %q", hdr.Get("X-Model-Router-Loss"))
	}
	got := up.last(t)
	thinking, ok := got["thinking"].(map[string]any)
	if !ok || thinking["type"] != "disabled" {
		t.Fatalf("llama native anthropic must preserve thinking untouched: %v", got)
	}
	if _, has := got["reasoning_effort"]; has {
		t.Fatalf("llama no effort: %v", got)
	}
}
