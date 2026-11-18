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

	"github.com/arhammahajan/model-router/internal/config"
)

func chatAnthEmuCfg(t *testing.T) *config.Config {
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

[providers.lossyanth]
dialect = "anthropic"
base_url = "http://127.0.0.1:9"
api_key = "anth-secret"
compatibility = "lossy"

[[models]]
name = "chat-anth"
provider = "anthup"
model = "claude-haiku-4-5"

[[models]]
name = "chat-anth-lossy"
provider = "lossyanth"
model = "claude-haiku-4-5"
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

type chatAnthHarness struct {
	upstream *httptest.Server
	router   *httptest.Server
	mu       sync.Mutex
	lastPath string
	lastBody []byte
	calls    int
	handler  func(w http.ResponseWriter, r *http.Request)
}

func newChatAnthHarness(t *testing.T, cfg *config.Config) *chatAnthHarness {
	t.Helper()
	h := &chatAnthHarness{}
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
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

func postChatAnth(t *testing.T, url, body string) (int, http.Header, []byte) {
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

// Chat custom -> Anthropic over actual HTTP: strict refuses pre-I/O,
// lossy emulates upstream and restores the native Chat custom shape.
func TestChatCustomToAnthropicHTTPEmulation(t *testing.T) {
	reqBody := `{"model":"chat-anth-lossy","messages":[{"role":"user","content":"patch this"}],
		"tools":[{"type":"custom","custom":{"name":"patch","description":"Apply"}}],
		"tool_choice":{"type":"custom","custom":{"name":"patch"}}}`
	strictBody := strings.Replace(reqBody, "chat-anth-lossy", "chat-anth", 1)

	// Strict refuses before upstream I/O.
	h0 := newChatAnthHarness(t, chatAnthEmuCfg(t))
	status, _, data := postChatAnth(t, h0.router.URL+"/v1/chat/completions", strictBody)
	if status != 400 {
		t.Fatalf("strict must 400, got %d %s", status, data)
	}
	h0.mu.Lock()
	calls := h0.calls
	h0.mu.Unlock()
	if calls != 0 {
		t.Fatalf("strict must not call upstream, got %d calls", calls)
	}

	// Lossy emulates: upstream sees a single string-valued function.
	h := newChatAnthHarness(t, chatAnthEmuCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x",`+
			`"content":[{"type":"tool_use","id":"call_7","name":"patch","input":{"content":"hello ✓"}}],`+
			`"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":3}}`)
	}
	status, hdr, data := postChatAnth(t, h.router.URL+"/v1/chat/completions", reqBody)
	if status != 200 {
		t.Fatalf("lossy must 200, got %d %s", status, data)
	}
	if got := hdr.Get("X-Model-Router-Loss"); !strings.Contains(got, "approximated") {
		t.Fatalf("lossy header must carry approximation, got %q body %s", got, data)
	}
	h.mu.Lock()
	upBody := append([]byte(nil), h.lastBody...)
	h.mu.Unlock()
	var up map[string]any
	if err := json.Unmarshal(upBody, &up); err != nil {
		t.Fatalf("upstream body must be JSON: %v %s", err, upBody)
	}
	tools, _ := up["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("upstream must carry one emulated tool: %s", upBody)
	}
	tool, _ := tools[0].(map[string]any)
	schema, _ := tool["input_schema"].(map[string]any)
	props, _ := schema["properties"].(map[string]any)
	content, _ := props["content"].(map[string]any)
	if content["type"] != "string" {
		t.Fatalf("upstream must carry {content:string} schema: %s", upBody)
	}
	if !strings.Contains(string(upBody), "patch") {
		t.Fatalf("upstream must name patch: %s", upBody)
	}
	if !strings.Contains(string(data), `"type":"custom"`) || !strings.Contains(string(data), "hello ✓") {
		t.Fatalf("client must see native Chat custom: %s", data)
	}
	var down map[string]any
	if err := json.Unmarshal(data, &down); err != nil {
		t.Fatal(err)
	}
	msg := down["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	call := msg["tool_calls"].([]any)[0].(map[string]any)
	if call["id"] != "call_7" {
		t.Fatalf("opaque ID not preserved: %v", call)
	}

	// Malformed upstream wrapper fails closed as 502.
	h2 := newChatAnthHarness(t, chatAnthEmuCfg(t))
	h2.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x",`+
			`"content":[{"type":"tool_use","id":"c1","name":"patch","input":{"content":17}}],`+
			`"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`)
	}
	status, _, data = postChatAnth(t, h2.router.URL+"/v1/chat/completions", reqBody)
	if status != 502 {
		t.Fatalf("malformed wrapper must 502, got %d %s", status, data)
	}
}

// Chat custom -> Gemini over actual HTTP: lossy emulates, upstream carries
// functionDeclarations with the content schema, client sees Chat custom.
func TestChatCustomToGeminiHTTPEmulation(t *testing.T) {
	reqBody := `{"model":"bridge-gem-lossy","messages":[{"role":"user","content":"patch this"}],
		"tools":[{"type":"custom","custom":{"name":"patch","description":"Apply","format":{"type":"grammar","grammar":{"syntax":"regex","definition":"[a-z]+"}}}}],
		"tool_choice":{"type":"custom","custom":{"name":"patch"}}}`
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"patch","args":{"content":"abc"},"id":"call_9"}}]},"finishReason":"STOP","index":0}],`+
			`"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3,"totalTokenCount":8},"modelVersion":"gemini-x"}`)
	}
	status, hdr, data := postGeminiBridge(t, h.router.URL+"/v1/chat/completions", reqBody, nil)
	if status != 200 {
		t.Fatalf("lossy must 200, got %d %s", status, data)
	}
	if got := hdr.Get("X-Model-Router-Loss"); !strings.Contains(got, "approximated") {
		t.Fatalf("lossy header must carry approximation, got %q", got)
	}
	h.mu.Lock()
	upBody := append([]byte(nil), h.lastBody...)
	h.mu.Unlock()
	if !strings.Contains(string(upBody), "parametersJsonSchema") || !strings.Contains(string(upBody), `"content"`) {
		t.Fatalf("upstream must carry emulated declaration: %s", upBody)
	}
	if !strings.Contains(string(upBody), "[a-z]+") {
		t.Fatalf("grammar must travel as instructions: %s", upBody)
	}
	if !strings.Contains(string(data), `"type":"custom"`) || !strings.Contains(string(data), "abc") {
		t.Fatalf("client must see native Chat custom: %s", data)
	}
}
