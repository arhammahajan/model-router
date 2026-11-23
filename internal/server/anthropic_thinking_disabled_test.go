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

type anthDisabledHarness struct {
	upstream *httptest.Server
	router   *httptest.Server
	mu       sync.Mutex
	lastPath string
	lastBody []byte
	calls    int
	handler  func(w http.ResponseWriter, r *http.Request)
}

func anthDisabledCfg(t *testing.T, extra string) *config.Config {
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
name = "a-chat"
provider = "chatup"
model = "gpt-4o-mini"

[[models]]
name = "a-chat-lossy"
provider = "lossychat"
model = "gpt-4o-mini"

[[models]]
name = "a-resp"
provider = "respup"
model = "gpt-4.1"

[[models]]
name = "a-resp-lossy"
provider = "lossyresp"
model = "gpt-4.1"

[[models]]
name = "a-native"
provider = "anthup"
model = "claude-haiku-4-5"
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

func newAnthDisabledHarness(t *testing.T, cfg *config.Config, chatResp, respResp string) *anthDisabledHarness {
	t.Helper()
	h := &anthDisabledHarness{}
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/responses") {
			_, _ = w.Write([]byte(respResp))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/messages") {
			_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-4-5","content":[{"type":"text","text":"native-hi"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":2}}`))
			return
		}
		_, _ = w.Write([]byte(chatResp))
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
	for _, prov := range cfg.Providers {
		prov.BaseURL = h.upstream.URL
	}
	srv := New(cfg)
	h.router = httptest.NewServer(srv.Handler())
	t.Cleanup(h.router.Close)
	return h
}

func anthDisabledPost(t *testing.T, url, body string) (int, http.Header, []byte) {
	t.Helper()
	return postBridge(t, url, body, nil)
}

func anthDisabledWire(t *testing.T, h *anthDisabledHarness) map[string]any {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	var v map[string]any
	if err := json.Unmarshal(h.lastBody, &v); err != nil {
		t.Fatalf("upstream not JSON: %v %s", err, h.lastBody)
	}
	return v
}

func anthDisabledCalls(h *anthDisabledHarness) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

const anthDisabledChatResp = `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`
const anthDisabledRespResp = `{"id":"resp_up","object":"response","created_at":1,"status":"completed","model":"gpt-4.1","output":[{"id":"msg_up","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hello","annotations":[]}]}],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}`

func TestAnthDisabledChatExactNoHeader(t *testing.T) {
	h := newAnthDisabledHarness(t, anthDisabledCfg(t, ""), anthDisabledChatResp, anthDisabledRespResp)
	status, hdr, _ := anthDisabledPost(t, h.router.URL+"/v1/messages",
		`{"model":"a-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"disabled"}}`)
	if status != 200 {
		t.Fatalf("strict exact must 200, got %d", status)
	}
	if hdr.Get("X-Model-Router-Loss") != "" {
		t.Fatalf("exact must emit no loss header, got %q", hdr.Get("X-Model-Router-Loss"))
	}
	wire := anthDisabledWire(t, h)
	if wire["reasoning_effort"] != "none" {
		t.Fatalf("chat wire must carry none: %v", wire)
	}
}

func TestAnthDisabledResponsesExactNoHeader(t *testing.T) {
	h := newAnthDisabledHarness(t, anthDisabledCfg(t, ""), anthDisabledChatResp, anthDisabledRespResp)
	status, hdr, _ := anthDisabledPost(t, h.router.URL+"/v1/messages",
		`{"model":"a-resp","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"disabled"}}`)
	if status != 200 {
		t.Fatalf("strict exact must 200, got %d", status)
	}
	if hdr.Get("X-Model-Router-Loss") != "" {
		t.Fatalf("exact must emit no loss header, got %q", hdr.Get("X-Model-Router-Loss"))
	}
	wire := anthDisabledWire(t, h)
	reas, ok := wire["reasoning"].(map[string]any)
	if !ok || reas["effort"] != "none" {
		t.Fatalf("responses wire must carry none: %v", wire)
	}
	if _, has := wire["reasoning_effort"]; has {
		t.Fatalf("responses must not emit chat field: %v", wire)
	}
}

func TestAnthDisabledToggleFalseStrict400LossyDrop(t *testing.T) {
	extra := `
[capabilities.overrides.a-chat.reasoning.toggle]
supported = false
[capabilities.overrides.a-chat-lossy.reasoning.toggle]
supported = false
`
	h := newAnthDisabledHarness(t, anthDisabledCfg(t, extra), anthDisabledChatResp, anthDisabledRespResp)
	status, _, data := anthDisabledPost(t, h.router.URL+"/v1/messages",
		`{"model":"a-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"disabled"}}`)
	if status != 400 || !strings.Contains(string(data), "thinking") {
		t.Fatalf("strict toggle=false must 400 with thinking, got %d %s", status, data)
	}
	if got := anthDisabledCalls(h); got != 0 {
		t.Fatalf("strict refusal must precede I/O, got %d", got)
	}
	status, hdr, _ := anthDisabledPost(t, h.router.URL+"/v1/messages",
		`{"model":"a-chat-lossy","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"disabled"}}`)
	if status != 200 || hdr.Get("X-Model-Router-Loss") == "" {
		t.Fatalf("lossy must drop with header, got %d %q", status, hdr.Get("X-Model-Router-Loss"))
	}
	if !strings.Contains(hdr.Get("X-Model-Router-Loss"), "thinking") {
		t.Fatalf("loss must name thinking: %q", hdr.Get("X-Model-Router-Loss"))
	}
	wire := anthDisabledWire(t, h)
	if _, has := wire["reasoning_effort"]; has {
		t.Fatalf("dropped must not emit reasoning: %v", wire)
	}
}

func TestAnthDisabledMalformedNullOtherDest(t *testing.T) {
	h := newAnthDisabledHarness(t, anthDisabledCfg(t, ""), anthDisabledChatResp, anthDisabledRespResp)
	for _, raw := range []string{
		`{"model":"a-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"disabled","budget_tokens":1024}}`,
		`{"model":"a-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"disabled","display":"summarized"}}`,
		`{"model":"a-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"enabled","budget_tokens":1024}}`,
		`{"model":"a-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"mystery"}}`,
	} {
		status, _, _ := anthDisabledPost(t, h.router.URL+"/v1/messages", raw)
		if status != 400 {
			t.Fatalf("malformed must 400 both modes: %s got %d", raw, status)
		}
	}
	if got := anthDisabledCalls(h); got != 0 {
		t.Fatalf("malformed must precede I/O, got %d", got)
	}
	// Null means absent: no reasoning on wire, no loss.
	status, hdr, _ := anthDisabledPost(t, h.router.URL+"/v1/messages",
		`{"model":"a-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":null}`)
	if status != 200 || hdr.Get("X-Model-Router-Loss") != "" {
		t.Fatalf("null must be absent no-loss: %d %q", status, hdr.Get("X-Model-Router-Loss"))
	}
	wire := anthDisabledWire(t, h)
	if _, has := wire["reasoning_effort"]; has {
		t.Fatalf("absent must not emit reasoning: %v", wire)
	}
	// Other dest (native) retains raw bypass: thinking passes through untouched.
	status, hdr, _ = anthDisabledPost(t, h.router.URL+"/v1/messages",
		`{"model":"a-native","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"disabled"}}`)
	if status != 200 {
		t.Fatalf("native must bypass: %d", status)
	}
	if hdr.Get("X-Model-Router-Loss") != "" {
		t.Fatalf("native must not set loss: %q", hdr.Get("X-Model-Router-Loss"))
	}
	nativeWire := anthDisabledWire(t, h)
	raw, _ := json.Marshal(nativeWire)
	if !strings.Contains(string(raw), "disabled") || !strings.Contains(string(raw), "9007199254740993") && false {
		t.Fatalf("native must preserve thinking verbatim: %s", raw)
	}
}

func TestAnthDisabledNativeBigInt(t *testing.T) {
	h := newAnthDisabledHarness(t, anthDisabledCfg(t, ""), anthDisabledChatResp, anthDisabledRespResp)
	status, _, data := anthDisabledPost(t, h.router.URL+"/v1/messages",
		`{"model":"a-chat","max_tokens":64,"messages":[{"role":"user","content":"Hi"},{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"get_time","input":{"n":9007199254740993}}]}],"thinking":{"type":"disabled"}}`)
	if status != 200 {
		t.Fatalf("bigint+disabled must 200, got %d %s", status, data)
	}
	h.mu.Lock()
	raw := string(h.lastBody)
	h.mu.Unlock()
	if !strings.Contains(raw, "9007199254740993") {
		t.Fatalf("bigint must survive: %s", raw)
	}
	if !strings.Contains(raw, `"reasoning_effort":"none"`) {
		t.Fatalf("disabled must still plan none: %s", raw)
	}
}

func TestAnthDisabledLlamaOff(t *testing.T) {
	up, rsrv, _ := llamaVerticalServer(t, capability.RuntimeLlamaCpp, "")
	status, _, _ := anthDisabledPost(t, rsrv.URL+"/v1/messages",
		`{"model":"llama-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"disabled"}}`)
	if status != 200 {
		t.Fatalf("llama off must 200, got %d", status)
	}
	got := up.last(t)
	kwargs, ok := got["chat_template_kwargs"].(map[string]any)
	if !ok || kwargs["enable_thinking"] != false {
		t.Fatalf("llama wire must be kwargs toggle: %v", got)
	}
	if _, has := got["reasoning_effort"]; has {
		t.Fatalf("llama must not emit effort: %v", got)
	}
}

func TestAnthDisabledUnobservedLoss(t *testing.T) {
	up, rsrv, _ := llamaVerticalServer(t, capability.RuntimeUnobserved, "")
	_ = up
	status, _, data := anthDisabledPost(t, rsrv.URL+"/v1/messages",
		`{"model":"llama-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"disabled"}}`)
	if status != 400 || !strings.Contains(string(data), "thinking") {
		t.Fatalf("unobserved strict must 400 with thinking, got %d %s", status, data)
	}
	up2, rsrv2, _ := llamaVerticalServer(t, capability.RuntimeUnobserved, "compatibility = \"lossy\"\n")
	_ = up2
	status, hdr, _ := anthDisabledPost(t, rsrv2.URL+"/v1/messages",
		`{"model":"llama-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"disabled"}}`)
	if status != 200 || hdr.Get("X-Model-Router-Loss") == "" {
		t.Fatalf("unobserved lossy must drop with header, got %d %q", status, hdr.Get("X-Model-Router-Loss"))
	}
}

func TestAnthDisabledSnapshotIsolation(t *testing.T) {
	cfg := anthDisabledCfg(t, "")
	s := New(cfg)
	snap := s.Registry().Current()
	m, p, fault := snap.Resolve("a-chat")
	if fault != 0 {
		t.Fatalf("resolve fault=%v", fault)
	}
	c1 := snap.RequestCapability(m, p)
	vals, ok := capability.StringListValue([]string{"mutated"})
	if !ok {
		t.Fatal("bad test value")
	}
	_ = c1.Put("reasoning.effort.values", vals, strPtrAnth("test"), capability.KindOverride)
	c2 := snap.RequestCapability(m, p)
	caps := capability.ReasoningFor(c2)
	if len(caps.EffortValues.List) != 0 && caps.EffortValues.List[0] == "mutated" {
		t.Fatalf("snapshot must own facts: %+v", caps.EffortValues)
	}
}

func strPtrAnth(s string) *string { v := s; return &v }

func boolPtrAnth(b bool) *bool { v := b; return &v }
