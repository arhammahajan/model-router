package server

import (
	"encoding/json"
	"errors"
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

	"github.com/arhammahajan/router/internal/config"
)

// bridgeCfg builds a router with Anthropic ingress models routed to a
// Chat upstream (strict + lossy) plus native controls.
func bridgeCfg(t *testing.T) *config.Config {
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

[providers.anthup]
dialect = "anthropic"
base_url = "http://127.0.0.1:9"
api_key = "anth-secret"

[[models]]
name = "bridge-chat"
provider = "chatup"
model = "gpt-4o-mini"

[[models]]
name = "bridge-lossy"
provider = "lossychat"
model = "gpt-4o-mini"

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
		t.Fatal(err)
	}
	return cfg
}

type bridgeHarness struct {
	upstream *httptest.Server
	router   *httptest.Server
	mu       sync.Mutex
	lastPath string
	lastBody []byte
	lastHdr  http.Header
	calls    int
	handler  func(w http.ResponseWriter, r *http.Request)
}

func newBridgeHarness(t *testing.T, cfg *config.Config) *bridgeHarness {
	t.Helper()
	h := &bridgeHarness{}
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`))
	}
	h.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.lastPath = r.URL.Path
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

func postBridge(t *testing.T, url, body string, headers map[string]string) (int, http.Header, []byte) {
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

func TestAnthropicToChatJSONText(t *testing.T) {
	h := newBridgeHarness(t, bridgeCfg(t))
	status, _, data := postBridge(t, h.router.URL+"/v1/messages",
		`{"model":"bridge-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	var out struct {
		Type    string `json:"type"`
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			Input  int `json:"input_tokens"`
			Output int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out.Type != "message" || out.Model != "bridge-chat" {
		t.Fatalf("envelope wrong: %s", data)
	}
	if len(out.Content) != 1 || out.Content[0].Text != "hello" {
		t.Fatalf("content wrong: %s", data)
	}
	if out.StopReason != "end_turn" {
		t.Fatalf("stop wrong: %s", data)
	}
	if out.Usage.Input != 7 || out.Usage.Output != 3 {
		t.Fatalf("usage wrong: %s", data)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lastPath != "/chat/completions" {
		t.Fatalf("upstream path: %s", h.lastPath)
	}
	var wire map[string]any
	if err := json.Unmarshal(h.lastBody, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["model"] != "gpt-4o-mini" {
		t.Fatalf("upstream model not replaced: %s", h.lastBody)
	}
	if h.lastHdr.Get("Authorization") != "Bearer chat-secret" {
		t.Fatalf("upstream auth wrong: %v", h.lastHdr)
	}
	if h.lastHdr.Get("X-Api-Key") != "" {
		t.Fatalf("chat upstream must not carry x-api-key: %v", h.lastHdr)
	}
	if h.lastHdr.Get("Anthropic-Version") != "" || h.lastHdr.Get("Anthropic-Beta") != "" {
		t.Fatalf("anthropic headers must not cross to Chat: %v", h.lastHdr)
	}
}

func TestAnthropicToChatJSONToolsHistoryBigInt(t *testing.T) {
	h := newBridgeHarness(t, bridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-9","object":"chat.completion","created":1,"model":"gpt-4o-mini",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":null,` +
			`"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_time","arguments":"{\"tz\":\"UTC\",\"n\":9007199254740993}"}}]},` +
			`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":12,"completion_tokens":7,"total_tokens":19}}`))
	}
	req := `{"model":"bridge-chat","max_tokens":64,` +
		`"messages":[{"role":"user","content":"What time?"},` +
		`{"role":"assistant","content":[{"type":"text","text":"checking"},{"type":"tool_use","id":"call_1","name":"get_time","input":{"tz":"UTC","n":9007199254740993}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"noon"}]}],` +
		`"tools":[{"name":"get_time","description":"clock","input_schema":{"type":"object"}}],` +
		`"tool_choice":{"type":"tool","name":"get_time"}}`
	status, _, data := postBridge(t, h.router.URL+"/v1/messages", req, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if !strings.Contains(string(data), `"id":"call_1"`) || !strings.Contains(string(data), "9007199254740993") {
		t.Fatalf("call identity/bigint lost: %s", data)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !strings.Contains(string(h.lastBody), "9007199254740993") {
		t.Fatalf("bigint not preserved upstream: %s", h.lastBody)
	}
	var wire struct {
		ToolChoice map[string]any `json:"tool_choice"`
		Messages   []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(h.lastBody, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.ToolChoice["type"] != "function" {
		t.Fatalf("specific choice wrong: %s", h.lastBody)
	}
	foundTool := false
	for _, m := range wire.Messages {
		if m.Role == "tool" && m.ToolCallID == "call_1" {
			foundTool = true
		}
	}
	if !foundTool {
		t.Fatalf("tool result not mapped: %s", h.lastBody)
	}
}

// EXPECTED correct behavior per OpenAI reference
// (https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods):
// none/auto/required are STRINGS, not objects. Current codec emits objects.
// KNOWN PENDING codec dependency (protocolfix worker): fails until integrated.
func TestAnthropicToChatToolChoiceStringWire(t *testing.T) {
	for tc, want := range map[string]string{
		`{"type":"auto"}`: "auto",
		`{"type":"any"}`:  "required",
		`{"type":"none"}`: "none",
	} {
		h := newBridgeHarness(t, bridgeCfg(t))
		req := `{"model":"bridge-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],` +
			`"tools":[{"name":"a","input_schema":{"type":"object"}}],"tool_choice":` + tc + `}`
		status, _, _ := postBridge(t, h.router.URL+"/v1/messages", req, nil)
		if status != 200 {
			t.Fatalf("%s: status=%d", tc, status)
		}
		h.mu.Lock()
		raw := string(h.lastBody)
		h.mu.Unlock()
		// The wire form must be a JSON string, e.g. "tool_choice":"auto".
		var wire map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &wire); err != nil {
			t.Fatal(err)
		}
		var s string
		if err := json.Unmarshal(wire["tool_choice"], &s); err != nil || s != want {
			t.Fatalf("%s: upstream tool_choice must be string %q, got %s", tc, want, wire["tool_choice"])
		}
	}
}

// EXPECTED: top_k has no Chat bridge mapping. Strict must refuse before any
// upstream I/O; lossy must drop with a loss header and MUST NOT send
// extra_body.top_k upstream. Current codec forwards extra_body.top_k.
// KNOWN PENDING codec dependency: fails until integrated.
func TestAnthropicToChatTopKStrictRejectLossyDrop(t *testing.T) {
	h := newBridgeHarness(t, bridgeCfg(t))
	req := `{"model":"bridge-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"top_k":5}`
	status, _, data := postBridge(t, h.router.URL+"/v1/messages", req, nil)
	if status != 400 || !strings.Contains(string(data), "top_k") {
		t.Fatalf("strict top_k must 400 translation_loss, got %d %s", status, data)
	}
	h.mu.Lock()
	calls := h.calls
	h.mu.Unlock()
	if calls != 0 {
		t.Fatalf("strict refusal must precede upstream I/O")
	}

	h2 := newBridgeHarness(t, bridgeCfg(t))
	req2 := `{"model":"bridge-lossy","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"top_k":5}`
	status, headers, _ := postBridge(t, h2.router.URL+"/v1/messages", req2, nil)
	if status != 200 {
		t.Fatalf("lossy top_k must 200, got %d", status)
	}
	if headers.Get("X-Model-Router-Loss") == "" {
		t.Fatalf("lossy must carry loss header")
	}
	h2.mu.Lock()
	defer h2.mu.Unlock()
	if strings.Contains(string(h2.lastBody), "top_k") {
		t.Fatalf("lossy must not forward top_k: %s", h2.lastBody)
	}
}

// Disabled thinking maps exactly to named none when capabilities are unknown.
// Both strict and lossy providers use the same planner and emit no loss header.
func TestAnthropicToChatThinkingDisabledExact(t *testing.T) {
	for _, model := range []string{"bridge-chat", "bridge-lossy"} {
		h := newBridgeHarness(t, bridgeCfg(t))
		req := `{"model":"` + model + `","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"disabled"}}`
		status, headers, data := postBridge(t, h.router.URL+"/v1/messages", req, nil)
		if status != 200 || headers.Get("X-Model-Router-Loss") != "" {
			t.Fatalf("exact disabled intent must succeed without loss: %d %s", status, data)
		}
		h.mu.Lock()
		calls := h.calls
		body := append([]byte(nil), h.lastBody...)
		h.mu.Unlock()
		var upstream map[string]json.RawMessage
		if err := json.Unmarshal(body, &upstream); err != nil {
			t.Fatal(err)
		}
		if calls != 1 || string(upstream["reasoning_effort"]) != `"none"` || upstream["thinking"] != nil {
			t.Fatalf("disabled intent must use the Chat wire once: calls=%d body=%s", calls, body)
		}
	}
}

// EXPECTED: Anthropic input_tokens = Chat prompt_tokens minus cached
// prompt tokens, with usage consistency checks. Current codec copies
// prompt_tokens verbatim. KNOWN PENDING codec dependency.
func TestAnthropicToChatUsageCacheSubtraction(t *testing.T) {
	h := newBridgeHarness(t, bridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"m",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":12,"completion_tokens":7,"total_tokens":19,"prompt_tokens_details":{"cached_tokens":4}}}`))
	}
	status, _, data := postBridge(t, h.router.URL+"/v1/messages",
		`{"model":"bridge-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	var out struct {
		Usage struct {
			Input     int `json:"input_tokens"`
			Output    int `json:"output_tokens"`
			CacheRead int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	// 12 prompt minus 4 cached = 8 honest input.
	if out.Usage.Input != 8 || out.Usage.CacheRead != 4 || out.Usage.Output != 7 {
		t.Fatalf("usage must subtract cache (want 8/7/4), got %+v body %s", out.Usage, data)
	}
}

func TestAnthropicToChatStrictLossNoIO(t *testing.T) {
	h := newBridgeHarness(t, bridgeCfg(t))
	status, _, data := postBridge(t, h.router.URL+"/v1/messages",
		`{"model":"bridge-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"service_tier":"auto"}`, nil)
	// Anthropic envelope carries the loss as an invalid_request_error message;
	// the translation_loss code lives in the Chat envelope only.
	if status != 400 || !strings.Contains(string(data), "service_tier") {
		t.Fatalf("strict must 400 translation_loss, got %d %s", status, data)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.calls != 0 {
		t.Fatalf("strict refusal must precede upstream I/O")
	}
}

func TestAnthropicToChatLossyHeader(t *testing.T) {
	h := newBridgeHarness(t, bridgeCfg(t))
	status, headers, data := postBridge(t, h.router.URL+"/v1/messages",
		`{"model":"bridge-lossy","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"service_tier":"auto"}`, nil)
	if status != 200 {
		t.Fatalf("lossy must 200, got %d %s", status, data)
	}
	loss := headers.Get("X-Model-Router-Loss")
	if loss == "" || !strings.Contains(loss, "service_tier") {
		t.Fatalf("lossy must carry loss header, got %q body %s", loss, data)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if strings.Contains(string(h.lastBody), "service_tier") {
		t.Fatalf("dropped field leaked upstream: %s", h.lastBody)
	}
}

func TestAnthropicToChatMalformedNoIO(t *testing.T) {
	h := newBridgeHarness(t, bridgeCfg(t))
	status, _, _ := postBridge(t, h.router.URL+"/v1/messages",
		`{"model":"bridge-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"bogus":1}`, nil)
	if status != 400 {
		t.Fatalf("malformed must 400, got %d", status)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.calls != 0 {
		t.Fatalf("validation failure must precede upstream I/O")
	}
}

func TestAnthropicToChatCacheUsagePassthrough(t *testing.T) {
	h := newBridgeHarness(t, bridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"m",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`))
	}
	status, _, data := postBridge(t, h.router.URL+"/v1/messages",
		`{"model":"bridge-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if !strings.Contains(string(data), `"input_tokens":10`) {
		t.Fatalf("usage wrong: %s", data)
	}
}

func TestAnthropicToChatTotalsOverflow502(t *testing.T) {
	h := newBridgeHarness(t, bridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// cached exceeds prompt: inconsistent accounting must fail closed.
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"m",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7,"prompt_tokens_details":{"cached_tokens":9}}}`))
	}
	status, _, data := postBridge(t, h.router.URL+"/v1/messages",
		`{"model":"bridge-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 502 {
		t.Fatalf("inconsistent usage must 502, got %d %s", status, data)
	}
	// Anthropic error shape, sanitized.
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil || payload["type"] != "error" {
		t.Fatalf("must be anthropic error shape: %s", data)
	}
}

func TestAnthropicToChatUnknownResponseFeature502(t *testing.T) {
	h := newBridgeHarness(t, bridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"m",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"hi","logprobs":{}},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}
	status, _, data := postBridge(t, h.router.URL+"/v1/messages",
		`{"model":"bridge-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 502 {
		t.Fatalf("unrepresentable must 502, got %d %s", status, data)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil || payload["type"] != "error" {
		t.Fatalf("must be anthropic error shape: %s", data)
	}
}

func TestAnthropicToChatUpstreamErrorSanitized(t *testing.T) {
	h := newBridgeHarness(t, bridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"message":"sk-secret prompt hello","code":429}}`))
	}
	status, headers, data := postBridge(t, h.router.URL+"/v1/messages",
		`{"model":"bridge-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 429 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if strings.Contains(string(data), "sk-secret") {
		t.Fatalf("error must be sanitized: %s", data)
	}
	if headers.Get("Retry-After") != "2" {
		t.Fatalf("Retry-After must be forwarded, got %q", headers.Get("Retry-After"))
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil || payload["type"] != "error" {
		t.Fatalf("must be anthropic error shape: %s", data)
	}
}

func TestAnthropicToChatHeaderIsolation(t *testing.T) {
	h := newBridgeHarness(t, bridgeCfg(t))
	status, _, data := postBridge(t, h.router.URL+"/v1/messages",
		`{"model":"bridge-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}]}`,
		map[string]string{
			"Authorization":     "Bearer client-secret",
			"Anthropic-Version": "2020-01-01",
			"Anthropic-Beta":    "beta-1",
			"Openai-Beta":       "assistants=v2",
			"X-Session-Id":      "sess-1",
			"Idempotency-Key":   "k-1",
			"Connection":        "X-Private",
			"X-Private":         "s3cr3t",
		})
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, k := range []string{"Anthropic-Version", "Anthropic-Beta", "X-Session-Id", "Idempotency-Key", "X-Private", "Connection"} {
		if v := h.lastHdr.Get(k); v != "" {
			t.Fatalf("inbound %q must not reach Chat (got %q)", k, v)
		}
	}
	if got := h.lastHdr.Get("Openai-Beta"); got != "assistants=v2" {
		t.Fatalf("openai-beta must forward, got %q", got)
	}
	if got := h.lastHdr.Get("Authorization"); got != "Bearer chat-secret" {
		t.Fatalf("upstream auth must be provider key, got %q", got)
	}
}

func TestAnthropicToChatSSETextLifecycle(t *testing.T) {
	h := newBridgeHarness(t, bridgeCfg(t))
	usage := `{"prompt_tokens":12,"completion_tokens":7,"total_tokens":19}`
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, f := range []string{
			`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}` + "\n\n",
			`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}` + "\n\n",
			`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
			`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":` + usage + `}` + "\n\n",
			"data: [DONE]\n\n",
		} {
			_, _ = w.Write([]byte(f))
			fl.Flush()
		}
	}
	status, headers, data := postBridge(t, h.router.URL+"/v1/messages",
		`{"model":"bridge-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"stream":true}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	body := string(data)
	for _, want := range []string{
		`event: message_start`, `"input_tokens":0`, `"output_tokens":0`,
		`event: content_block_start`, `"text_delta"`, `"text":"Hello"`,
		`event: content_block_stop`, `event: message_delta`,
		`"stop_reason":"end_turn"`, `"input_tokens":12`, `"output_tokens":7`,
		`event: message_stop`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream lacks %s:\n%s", want, body)
		}
	}
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("anthropic stream must not contain [DONE]:\n%s", body)
	}
	if headers.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content-type wrong: %q", headers.Get("Content-Type"))
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var wire map[string]any
	if err := json.Unmarshal(h.lastBody, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["stream"] != true {
		t.Fatalf("upstream must stream: %s", h.lastBody)
	}
	if wire["stream_options"] == nil {
		t.Fatalf("streaming must request upstream usage: %s", h.lastBody)
	}
}

func TestAnthropicToChatSSEToolsBigInt(t *testing.T) {
	h := newBridgeHarness(t, bridgeCfg(t))
	usage := `{"prompt_tokens":30,"completion_tokens":20,"total_tokens":50}`
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, f := range []string{
			`data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n",
			`data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_A","type":"function","function":{"name":"get_time","arguments":""}}]},"finish_reason":null}]}` + "\n\n",
			`data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"tz\":\"UTC\",\"n\":9007199254740993}"}}]},"finish_reason":null}]}` + "\n\n",
			`data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n",
			`data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":` + usage + `}` + "\n\n",
			"data: [DONE]\n\n",
		} {
			_, _ = w.Write([]byte(f))
			fl.Flush()
		}
	}
	status, _, data := postBridge(t, h.router.URL+"/v1/messages",
		`{"model":"bridge-chat","max_tokens":64,"messages":[{"role":"user","content":"Hi"}],"stream":true,"tools":[{"name":"get_time","input_schema":{"type":"object"}}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	body := string(data)
	for _, want := range []string{`"id":"call_A"`, `"name":"get_time"`, `9007199254740993`, `"stop_reason":"tool_use"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream lacks %s:\n%s", want, body)
		}
	}
}

func TestAnthropicToChatSSEEarlyInvalid502(t *testing.T) {
	h := newBridgeHarness(t, bridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + `{"id":"c","mystery":1}` + "\n\n"))
	}
	status, _, data := postBridge(t, h.router.URL+"/v1/messages",
		`{"model":"bridge-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"stream":true}`, nil)
	// Pre-commit failure: ordinary Anthropic 502, never an empty 200.
	if status != 502 {
		t.Fatalf("early invalid must 502, got %d body %s", status, data)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil || payload["type"] != "error" {
		t.Fatalf("must be anthropic error shape: %s", data)
	}
	if strings.Contains(string(data), "message_stop") {
		t.Fatalf("must not emit success marker: %s", data)
	}
}

func TestAnthropicToChatSSEMidstreamTruncation(t *testing.T) {
	h := newBridgeHarness(t, bridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}` + "\n\n"))
		// EOF without finish/usage/DONE: midstream truncation.
	}
	status, _, data := postBridge(t, h.router.URL+"/v1/messages",
		`{"model":"bridge-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"stream":true}`, nil)
	// Post-commit failure keeps 200 with one error event and no stop.
	if status != 200 {
		t.Fatalf("midstream truncation keeps 200, got %d body %s", status, data)
	}
	body := string(data)
	if !strings.Contains(body, "event: error") {
		t.Fatalf("must emit error event:\n%s", body)
	}
	if strings.Count(body, "event: error") != 1 {
		t.Fatalf("must emit exactly one error event:\n%s", body)
	}
	if strings.Contains(body, "message_stop") {
		t.Fatalf("must not emit success marker:\n%s", body)
	}
}

func countFramedAnthropicErrorEvents(body string) int {
	n := 0
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "event: error" {
			n++
		}
	}
	return n
}

func TestDetectsAnthropicErrorEventFraming(t *testing.T) {
	// Actual encoder framing must match.
	if !detectsAnthropicErrorEvent([]byte("event: error\ndata: {\"type\":\"error\"}\n\n")) {
		t.Fatalf("framed error event must match")
	}
	if !detectsAnthropicErrorEvent([]byte("event:error\r\ndata: {}\r\n\r\n")) {
		t.Fatalf("event field without space must match")
	}
	// Prompt/tool data containing the substring must never match.
	for _, p := range [][]byte{
		[]byte("data: {\"delta\":{\"text\":\"event: error\"}}\n\n"),
		[]byte("event: content_block_delta\ndata: {\"text\":\"event: error\"}\n\n"),
		[]byte("data: event: error\n\n"),
		[]byte(": event: error keepalive\n\n"),
	} {
		if detectsAnthropicErrorEvent(p) {
			t.Fatalf("prompt data must not mark errorSent: %q", p)
		}
	}
}

func TestAnthropicToChatSSEContentEventTextStillEmitsError(t *testing.T) {
	h := newBridgeHarness(t, bridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Legitimate prompt data containing the substring "event: error".
		_, _ = w.Write([]byte("data: " + `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"event: error"},"finish_reason":null}]}` + "\n\n"))
		// EOF without finish/usage/DONE: midstream truncation must still
		// emit one framed error event.
	}
	status, _, data := postBridge(t, h.router.URL+"/v1/messages",
		`{"model":"bridge-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"stream":true}`, nil)
	if status != 200 {
		t.Fatalf("truncation keeps 200, got %d body %s", status, data)
	}
	body := string(data)
	if got := countFramedAnthropicErrorEvents(body); got != 1 {
		t.Fatalf("must emit exactly one framed error event, got %d:\n%s", got, body)
	}
	if strings.Contains(body, "message_stop") {
		t.Fatalf("must not emit success marker:\n%s", body)
	}
}

func TestAnthropicToChatSSENativeErrorNoDuplicate(t *testing.T) {
	h := newBridgeHarness(t, bridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		_, _ = w.Write([]byte("data: " + `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}` + "\n\n"))
		fl.Flush()
		_, _ = w.Write([]byte("data: " + `{"error":{"message":"boom"}}` + "\n\n"))
		fl.Flush()
	}
	status, _, data := postBridge(t, h.router.URL+"/v1/messages",
		`{"model":"bridge-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"stream":true}`, nil)
	if status != 200 {
		t.Fatalf("upstream error keeps 200, got %d body %s", status, data)
	}
	body := string(data)
	if got := countFramedAnthropicErrorEvents(body); got != 1 {
		t.Fatalf("translator-native error must appear exactly once, got %d:\n%s", got, body)
	}
	if strings.Contains(body, "message_stop") {
		t.Fatalf("must not emit success marker:\n%s", body)
	}
}

// flushFailWriter simulates a downstream gone mid-stream.
type bridgeFlushFail struct {
	header http.Header
	status int
}

func (f *bridgeFlushFail) Header() http.Header {
	if f.header == nil {
		f.header = http.Header{}
	}
	return f.header
}
func (f *bridgeFlushFail) Write(p []byte) (int, error) { return len(p), nil }
func (f *bridgeFlushFail) WriteHeader(status int)      { f.status = status }
func (f *bridgeFlushFail) FlushError() error           { return errors.New("downstream gone") }

func TestAnthropicToChatFlushFailureClosesUpstream(t *testing.T) {
	var clientGone = make(chan struct{})
	var goneOnce sync.Once
	var n atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			_, _ = w.Write([]byte("data: " + `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}` + "\n\n"))
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				goneOnce.Do(func() { close(clientGone) })
			case <-time.After(10 * time.Second):
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer up.Close()
	dir := t.TempDir()
	p := filepath.Join(dir, "router.toml")
	_ = os.WriteFile(p, []byte(`
[server]
auth_mode = "none"
max_generations = 1
[retry]
deadline = 10
[providers.chatup]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
api_key = "x"
[[models]]
name = "m"
provider = "chatup"
model = "x"
`), 0o600)
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, prov := range cfg.Providers {
		prov.BaseURL = up.URL
	}
	srv := New(cfg)
	body := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"Hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	fw := &bridgeFlushFail{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.Handler().ServeHTTP(fw, req)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("flush failure did not terminate promptly")
	}
	select {
	case <-clientGone:
	case <-time.After(5 * time.Second):
		t.Fatalf("upstream was not closed after downstream flush failure")
	}
	routerSrv := httptest.NewServer(srv.Handler())
	defer routerSrv.Close()
	status, _, data := postBridge(t, routerSrv.URL+"/v1/messages",
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status == 503 {
		t.Fatalf("generation slot leaked after flush failure (body %s)", data)
	}
	if status != 200 {
		t.Fatalf("second request after flush failure: status=%d body=%s", status, data)
	}
}

func bridgeStallConfig(t *testing.T, retryDeadline, providerTimeout float64) *config.Config {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "router.toml")
	retryLine := ""
	if retryDeadline > 0 {
		retryLine = "\ndeadline = " + formatFloat(retryDeadline)
	}
	provTimeout := ""
	if providerTimeout > 0 {
		provTimeout = "\ntimeout = " + formatFloat(providerTimeout)
	}
	body := `
[server]
auth_mode = "none"
allow_dynamic_routes = false
max_generations = 2

[retry]
max_attempts = 1` + retryLine + `

[providers.chatup]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
api_key = "chat-secret"` + provTimeout + `

[[models]]
name = "bridge-chat"
provider = "chatup"
model = "gpt-4o-mini"
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

func formatFloat(f float64) string {
	return strings.TrimRight(strings.TrimRight(jsonNumber(f), "0"), ".")
}

func jsonNumber(f float64) string {
	// Avoid importing strconv in test header churn; fmt is already available
	// via other helpers? Use Sprintf-style through encoding/json.
	b, _ := json.Marshal(f)
	return string(b)
}

func TestAnthropicToChatSSEPrecommitAbsoluteDeadline504(t *testing.T) {
	cfg := bridgeStallConfig(t, 0.4, 0)
	h := newBridgeHarness(t, cfg)
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}
	status, _, data := postBridge(t, h.router.URL+"/v1/messages",
		`{"model":"bridge-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"stream":true}`, nil)
	if status != 504 {
		t.Fatalf("stalled pre-commit body must 504, got %d body %s", status, data)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil || payload["type"] != "error" {
		t.Fatalf("must be anthropic error shape: %s", data)
	}
	if strings.Contains(string(data), "message_stop") {
		t.Fatalf("pre-commit failure must not emit success marker: %s", data)
	}
	h.mu.Lock()
	calls := h.calls
	h.mu.Unlock()
	if calls != 1 {
		t.Fatalf("must make exactly one upstream request, got %d", calls)
	}
	// Slot recovered: a normal non-stream request succeeds.
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}
	status, _, data = postBridge(t, h.router.URL+"/v1/messages",
		`{"model":"bridge-chat","max_tokens":8,"messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 200 {
		t.Fatalf("slot must be recovered after pre-commit timeout, got %d %s", status, data)
	}
}

func TestAnthropicToChatSSEPrecommitProviderTimeout504(t *testing.T) {
	cfg := bridgeStallConfig(t, 10, 0.3)
	h := newBridgeHarness(t, cfg)
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}
	status, _, data := postBridge(t, h.router.URL+"/v1/messages",
		`{"model":"bridge-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"stream":true}`, nil)
	if status != 504 {
		t.Fatalf("provider-timeout stall must 504, got %d body %s", status, data)
	}
	h.mu.Lock()
	calls := h.calls
	h.mu.Unlock()
	if calls != 1 {
		t.Fatalf("must make exactly one upstream request, got %d", calls)
	}
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}
	status, _, data = postBridge(t, h.router.URL+"/v1/messages",
		`{"model":"bridge-chat","max_tokens":8,"messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 200 {
		t.Fatalf("slot must be recovered after provider timeout, got %d %s", status, data)
	}
}

func TestAnthropicToChatSSESlowInvalidStays502(t *testing.T) {
	cfg := bridgeStallConfig(t, 10, 0)
	h := newBridgeHarness(t, cfg)
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Small in-deadline delay: still a pre-commit invalid body,
		// never a timeout.
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write([]byte("data: " + `{"id":"c","mystery":1}` + "\n\n"))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
	}
	status, _, data := postBridge(t, h.router.URL+"/v1/messages",
		`{"model":"bridge-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"stream":true}`, nil)
	if status != 502 {
		t.Fatalf("slow invalid body must stay 502, got %d %s", status, data)
	}
}
