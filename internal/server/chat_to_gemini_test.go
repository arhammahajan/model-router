package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arhammahajan/model-router/internal/config"
)

// geminiBridgeCfg builds a router with Chat ingress models routed to a
// Gemini upstream (strict + lossy) and Gemini ingress models routed to a
// Chat upstream (strict + lossy), plus an Anthropic control for refusal
// coverage. Shared by both bridge test files (same package).
func geminiBridgeCfg(t *testing.T) *config.Config {
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

[providers.lossychat]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
api_key = "chat-secret"
compatibility = "lossy"

[providers.anthup]
dialect = "anthropic"
base_url = "http://127.0.0.1:9"
api_key = "anth-secret"

[[models]]
name = "bridge-gem"
provider = "gemup"
model = "gemini-2.0-flash"

[[models]]
name = "bridge-gem-lossy"
provider = "lossygem"
model = "gemini-2.0-flash"

[[models]]
name = "gem-chat"
provider = "chatup"
model = "gpt-4o-mini"

[[models]]
name = "gem-chat-lossy"
provider = "lossychat"
model = "gpt-4o-mini"

[[models]]
name = "gem-x"
provider = "anthup"
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

type geminiBridgeHarness struct {
	upstream *httptest.Server
	router   *httptest.Server
	mu       sync.Mutex
	lastPath string
	lastRaw  string // raw query
	lastBody []byte
	lastHdr  http.Header
	calls    int
	handler  func(w http.ResponseWriter, r *http.Request)
}

func newGeminiBridgeHarness(t *testing.T, cfg *config.Config) *geminiBridgeHarness {
	t.Helper()
	h := &geminiBridgeHarness{}
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
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
		prov.BaseURL = h.upstream.URL
	}
	srv := New(cfg)
	h.router = httptest.NewServer(srv.Handler())
	t.Cleanup(h.router.Close)
	return h
}

func postGeminiBridge(t *testing.T, url, body string, headers map[string]string) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, data
}

func gemCalls(h *geminiBridgeHarness) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

// Chat ingress -> Gemini upstream: JSON text translates with the public
// model name, system hoisting and generation config on the wire.
func TestChatToGeminiJSONText(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"hello"}],"role":"model"},"finishReason":"STOP","index":0}],"modelVersion":"gemini-2.0-flash","responseId":"resp_gem_1","usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3,"totalTokenCount":10}}`))
	}
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-gem","messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"Hi"}],"max_tokens":16,"temperature":0.5,"top_p":0.9}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if v["model"] != "bridge-gem" {
		t.Fatalf("public model not restored: %s", data)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lastPath != "/v1beta/models/gemini-2.0-flash:generateContent" {
		t.Fatalf("upstream path: %s raw %s", h.lastPath, h.lastRaw)
	}
	if h.lastRaw != "" {
		t.Fatalf("non-stream must not forward a query, got %q", h.lastRaw)
	}
	var wire map[string]any
	if err := json.Unmarshal(h.lastBody, &wire); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"model", "stream"} {
		if _, has := wire[k]; has {
			t.Fatalf("wire body must not carry %q (URL-owned): %s", k, h.lastBody)
		}
	}
	if h.lastHdr.Get("X-Goog-Api-Key") != "gem-secret" {
		t.Fatalf("upstream gemini auth wrong: %v", h.lastHdr)
	}
	if v := h.lastHdr.Get("Authorization"); v != "" {
		t.Fatalf("client auth must never become upstream auth: %q", v)
	}
}

// Chat ingress -> Gemini upstream: function tools encode exactly with large
// integer arguments preserved, and downstream calls restore Chat identity.
func TestChatToGeminiToolsExact(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"get_time","args":{"tz":"UTC","n":9007199254740993},"id":"call_1"}}],"role":"model"},"finishReason":"STOP","index":0}],"modelVersion":"gemini-2.0-flash","responseId":"resp_gem_2","usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":2,"totalTokenCount":11}}`))
	}
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-gem","messages":[{"role":"user","content":"Hi"}],"tools":[{"type":"function","function":{"name":"get_time","parameters":{"type":"object"}}}],"tool_choice":"auto"}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if !strings.Contains(string(data), "9007199254740993") {
		t.Fatalf("large integer not preserved: %s", data)
	}
	if !strings.Contains(string(data), `"id":"call_1"`) {
		t.Fatalf("call identity not restored: %s", data)
	}
	var wire map[string]any
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := json.Unmarshal(h.lastBody, &wire); err != nil {
		t.Fatal(err)
	}
	tools, _ := wire["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("wire tools=%v", wire["tools"])
	}
	decls, _ := tools[0].(map[string]any)["functionDeclarations"].([]any)
	if len(decls) != 1 {
		t.Fatalf("wire declarations=%v", tools[0])
	}
	if decls[0].(map[string]any)["parametersJsonSchema"] == nil {
		t.Fatalf("schema must ride parametersJsonSchema: %s", h.lastBody)
	}
}

// Chat ingress -> Gemini upstream: SSE text translates incrementally with a
// committed terminal and [DONE]; the upstream stream is requested with
// ?alt=sse.
func TestChatToGeminiSSEText(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "alt=sse" {
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hel\"}],\"role\":\"model\"},\"index\":0}],\"modelVersion\":\"gemini-2.0-flash\",\"responseId\":\"resp_gem_s\"}\n\n")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"lo\"}],\"role\":\"model\"},\"finishReason\":\"STOP\",\"index\":0}],\"modelVersion\":\"gemini-2.0-flash\",\"responseId\":\"resp_gem_s\",\"usageMetadata\":{\"promptTokenCount\":7,\"candidatesTokenCount\":3,\"totalTokenCount\":10}}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, headers, data := postGeminiBridge(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-gem","messages":[{"role":"user","content":"Hi"}],"stream":true,"stream_options":{"include_usage":true}}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if ct := headers.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("downstream must be Chat SSE, got %q", ct)
	}
	text := string(data)
	for _, want := range []string{`"content":"hel"`, `"content":"lo"`, `"finish_reason":"stop"`, `"prompt_tokens":7`, "data: [DONE]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %s", want, text)
		}
	}
	if !strings.Contains(text, `"model":"bridge-gem"`) {
		t.Fatalf("public model not restored downstream: %s", text)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lastRaw != "alt=sse" {
		t.Fatalf("upstream stream must request ?alt=sse, got %q", h.lastRaw)
	}
}

// Chat ingress -> Gemini upstream: a standard JSON-array stream is accepted
// via the explicit response Content-Type even though ?alt=sse was requested.
func TestChatToGeminiJSONArrayStream(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[{"candidates":[{"content":{"parts":[{"text":"a"}],"role":"model"},"index":0}]},{"candidates":[{"content":{"parts":[{"text":"b"}],"role":"model"},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3,"totalTokenCount":10}}]`)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-gem","messages":[{"role":"user","content":"Hi"}],"stream":true,"stream_options":{"include_usage":true}}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	text := string(data)
	for _, want := range []string{`"content":"a"`, `"content":"b"`, `"finish_reason":"stop"`, "data: [DONE]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %s", want, text)
		}
	}
}

// Chat ingress -> Gemini upstream: an unsupported upstream encoding is an
// ordinary pre-commit 502, never a sniffed body or a leaked raw body.
func TestChatToGeminiUnsupportedEncoding502(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "not a stream")
	}
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-gem","messages":[{"role":"user","content":"Hi"}],"stream":true}`, nil)
	if status != 502 {
		t.Fatalf("status=%d body=%s, want 502", status, data)
	}
	if strings.Contains(string(data), "not a stream") {
		t.Fatalf("raw upstream body leaked: %s", data)
	}
	if strings.Contains(string(data), "data: [DONE]") {
		t.Fatalf("pre-commit failure must not emit [DONE]: %s", data)
	}
}

// Chat ingress -> Gemini upstream: strict mode refuses a diagnosed loss
// before any upstream I/O.
func TestChatToGeminiStrictLoss(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-gem","messages":[{"role":"user","content":"Hi"}],"parallel_tool_calls":false}`, nil)
	if status != 400 {
		t.Fatalf("status=%d body=%s, want 400", status, data)
	}
	if !strings.Contains(string(data), "translation_loss") {
		t.Fatalf("body lacks translation_loss: %s", data)
	}
	if n := gemCalls(h); n != 0 {
		t.Fatalf("upstream calls=%d, want 0 (pre-I/O refusal)", n)
	}
}

// Chat ingress -> Gemini upstream: malformed source is a 400 with no I/O.
func TestChatToGeminiMalformed(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-gem","messages":[{"role":"user","content":"Hi"}],"bogus":1}`, nil)
	if status != 400 {
		t.Fatalf("status=%d body=%s, want 400", status, data)
	}
	if n := gemCalls(h); n != 0 {
		t.Fatalf("upstream calls=%d, want 0", n)
	}
}

// Chat ingress -> Gemini upstream: client credentials never become upstream
// credentials; only the configured x-goog-api-key (or placeholder) crosses.
func TestChatToGeminiCredentialIsolation(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"hi"}],"role":"model"},"finishReason":"STOP","index":0}],"modelVersion":"gemini-2.0-flash","responseId":"r","usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`))
	}
	status, _, _ := postGeminiBridge(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-gem","messages":[{"role":"user","content":"Hi"}]}`,
		map[string]string{"Authorization": "Bearer client-key", "X-Goog-Api-Key": "client-key", "X-Api-Key": "client-key"})
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if got := h.lastHdr.Get("X-Goog-Api-Key"); got != "gem-secret" {
		t.Fatalf("upstream credential=%q, want configured key", got)
	}
	for _, k := range []string{"Authorization", "X-Api-Key", "Api-Key"} {
		if got := h.lastHdr.Get(k); got != "" {
			t.Fatalf("client credential leaked upstream as %s=%q", k, got)
		}
	}
}

// Chat ingress -> Gemini upstream: an unrepresentable upstream feature after
// commitment... pre-commit here: a thought part fails the translation with
// HTTP 502 and no success marker.
func TestChatToGeminiLateUnsupported(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"hello","thought":true}],"role":"model"},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3,"totalTokenCount":10}}`))
	}
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-gem","messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 502 {
		t.Fatalf("status=%d body=%s, want 502", status, data)
	}
	if strings.Contains(string(data), `"finish_reason":"stop"`) {
		t.Fatalf("no success marker on failure: %s", data)
	}
}

// Chat ingress -> Gemini upstream: post-commit truncation keeps 200 and
// terminates silently without [DONE] or a fabricated finish.
func TestChatToGeminiSSETruncated(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"par\"}],\"role\":\"model\"},\"index\":0}],\"modelVersion\":\"gemini-2.0-flash\",\"responseId\":\"r\"}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Truncate: close without a finish chunk.
	}
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-gem","messages":[{"role":"user","content":"Hi"}],"stream":true}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s, want 200 (headers committed)", status, data)
	}
	text := string(data)
	if !strings.Contains(text, `"content":"par"`) {
		t.Fatalf("committed delta missing: %s", text)
	}
	for _, absent := range []string{"[DONE]", `"finish_reason":"stop"`, `"finish_reason":"tool_calls"`,
		`"finish_reason":"length"`, `"finish_reason":"content_filter"`, `"error"`} {
		if strings.Contains(text, absent) {
			t.Fatalf("truncated stream must not contain %q: %s", absent, text)
		}
	}
}

// Chat ingress -> Gemini upstream: pre-commit invalid stream is an ordinary
// 502, never an empty 200.
func TestChatToGeminiEarlyInvalidStream(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"mystery\":1}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-gem","messages":[{"role":"user","content":"Hi"}],"stream":true}`, nil)
	if status != 502 {
		t.Fatalf("status=%d body=%s, want 502", status, data)
	}
	if strings.Contains(string(data), "data: [DONE]") {
		t.Fatalf("pre-commit failure must not emit [DONE]: %s", data)
	}
}

// Chat ingress -> Gemini upstream: model text containing framing-like tokens
// is payload, never framing. Exactly one terminal [DONE] is emitted.
func TestChatToGeminiModelTextIsNotFraming(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"a\\nevent: error\\nb\"}],\"role\":\"model\"},\"index\":0}],\"modelVersion\":\"m\",\"responseId\":\"r\"}\n\n")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[],\"role\":\"model\"},\"finishReason\":\"STOP\",\"index\":0}],\"modelVersion\":\"m\",\"responseId\":\"r\",\"usageMetadata\":{\"promptTokenCount\":1,\"candidatesTokenCount\":1,\"totalTokenCount\":2}}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-gem","messages":[{"role":"user","content":"Hi"}],"stream":true,"stream_options":{"include_usage":true}}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	text := string(data)
	terminal := 0
	for _, block := range strings.Split(text, "\n\n") {
		if strings.TrimSpace(block) == "data: [DONE]" {
			terminal++
		}
	}
	if terminal != 1 {
		t.Fatalf("terminal [DONE] block emitted %d times, want exactly 1: %s", terminal, text)
	}
	if !strings.Contains(text, "event: error") {
		t.Fatalf("model text payload missing: %s", text)
	}
}

// Chat ingress -> Gemini upstream: an upstream Gemini functionCall with an
// empty args object renders as an empty Chat arguments object, never a
// malformed call: the terminal keeps an ordinary tool_calls finish.
func TestChatToGeminiEmptyArgs(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"get_time","args":{},"id":"call_1"}}],"role":"model"},"finishReason":"STOP","index":0}],"modelVersion":"m","responseId":"r","usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`))
	}
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-gem","messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if !strings.Contains(string(data), `"finish_reason":"tool_calls"`) {
		t.Fatalf("tool terminal missing: %s", data)
	}
	if !strings.Contains(string(data), `"arguments":"{}"`) {
		t.Fatalf("empty args not preserved: %s", data)
	}
}

// geminiTightCfg is a single-slot bridge config with a short absolute
// deadline, for precommit-timeout and flush-failure tests where the slot
// must be proven recovered.
func geminiTightCfg(t *testing.T, deadlineSecs float64) *config.Config {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "router.toml")
	body := fmt.Sprintf(`
[server]
auth_mode = "none"
allow_dynamic_routes = false
max_generations = 1

[retry]
max_attempts = 1
deadline = %v

[providers.gemup]
dialect = "gemini"
base_url = "http://127.0.0.1:9"
api_key = "gem-secret"

[providers.chatup]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
api_key = "chat-secret"

[[models]]
name = "bridge-gem"
provider = "gemup"
model = "gemini-2.0-flash"

[[models]]
name = "gem-chat"
provider = "chatup"
model = "gpt-4o-mini"
`, deadlineSecs)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// Chat ingress -> Gemini upstream: a stalled upstream body past the absolute
// deadline is a pre-commit 504 (never an empty 200), the upstream body is
// closed promptly, and the generation slot is recovered.
func TestChatToGeminiPrecommitTimeout504(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiTightCfg(t, 1))
	closed := make(chan struct{})
	var closeOnce sync.Once
	var calls atomic.Int32
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"hi"}],"role":"model"},"finishReason":"STOP","index":0}],"modelVersion":"m","responseId":"r","usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-r.Context().Done():
			closeOnce.Do(func() { close(closed) })
		case <-time.After(30 * time.Second):
		}
	}
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-gem","messages":[{"role":"user","content":"Hi"}],"stream":true}`, nil)
	if status != 504 {
		t.Fatalf("status=%d body=%s, want 504", status, data)
	}
	if strings.Contains(string(data), "data: [DONE]") {
		t.Fatalf("pre-commit timeout must not emit [DONE]: %s", data)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatalf("stalled upstream was not closed after pre-commit 504")
	}
	status, _, data = postGeminiBridge(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-gem","messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 200 {
		t.Fatalf("slot not recovered after 504: status=%d body=%s", status, data)
	}
}

// Chat ingress -> Gemini upstream: a downstream flush failure cancels the
// upstream body promptly and releases the generation slot.
func TestChatToGeminiFlushFailureCancelsUpstream(t *testing.T) {
	cfg := geminiTightCfg(t, 30)
	var n atomic.Int32
	closed := make(chan struct{})
	var closeOnce sync.Once
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) > 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"hi"}],"role":"model"},"finishReason":"STOP","index":0}],"modelVersion":"m","responseId":"r","usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		// A committing prefix: a text delta forces the translator to commit
		// downstream. The body is then held open; the flush failure must
		// still terminate promptly via the synchronous upstream close.
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"par\"}],\"role\":\"model\"},\"index\":0}],\"modelVersion\":\"m\",\"responseId\":\"r\"}\n\n"))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			closeOnce.Do(func() { close(closed) })
		case <-time.After(30 * time.Second):
		}
	}))
	defer up.Close()
	for _, prov := range cfg.Providers {
		prov.BaseURL = up.URL
	}
	srv := New(cfg)
	body := `{"model":"bridge-gem","messages":[{"role":"user","content":"Hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	fw := &flushFailWriter{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.Handler().ServeHTTP(fw, req)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("flush failure did not terminate the translated stream promptly")
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatalf("upstream was not closed after downstream flush failure")
	}
	routerSrv := httptest.NewServer(srv.Handler())
	defer routerSrv.Close()
	status, _, data := postGeminiBridge(t, routerSrv.URL+"/v1/chat/completions",
		`{"model":"bridge-gem","messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status == 503 {
		t.Fatalf("generation slot leaked after flush failure (body %s)", data)
	}
	if status != 200 {
		t.Fatalf("second request after flush failure: status=%d body=%s", status, data)
	}
}

// Chat ingress -> Gemini upstream: a valid terminal returns promptly without
// waiting for EOF, the upstream body is closed, and the slot is recovered.
func TestChatToGeminiPromptReturnReleasesSlot(t *testing.T) {
	cfg := geminiTightCfg(t, 30)
	var n atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) > 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"hi"}],"role":"model"},"finishReason":"STOP","index":0}],"modelVersion":"m","responseId":"r","usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hel\"}],\"role\":\"model\"},\"index\":0}],\"modelVersion\":\"m\",\"responseId\":\"r\"}\n\n")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"lo\"}],\"role\":\"model\"},\"finishReason\":\"STOP\",\"index\":0}],\"modelVersion\":\"m\",\"responseId\":\"r\",\"usageMetadata\":{\"promptTokenCount\":7,\"candidatesTokenCount\":3,\"totalTokenCount\":10}}\n\n")
		w.(http.Flusher).Flush()
		// Valid terminal sent: hold the body open. The server must return
		// without waiting for EOF and close us via the generation cleanup.
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	}))
	defer up.Close()
	for _, prov := range cfg.Providers {
		prov.BaseURL = up.URL
	}
	routerSrv := httptest.NewServer(New(cfg).Handler())
	defer routerSrv.Close()
	status, _, data := postGeminiBridge(t, routerSrv.URL+"/v1/chat/completions",
		`{"model":"bridge-gem","messages":[{"role":"user","content":"Hi"}],"stream":true,"stream_options":{"include_usage":true}}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if !strings.Contains(string(data), "data: [DONE]") {
		t.Fatalf("valid terminal must end with [DONE]: %s", data)
	}
	status, _, data = postGeminiBridge(t, routerSrv.URL+"/v1/chat/completions",
		`{"model":"bridge-gem","messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 200 {
		t.Fatalf("slot not released after valid terminal without EOF: status=%d body=%s", status, data)
	}
}
