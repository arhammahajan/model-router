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

// r2aBridgeCfg builds a router with Responses ingress models routed to an
// Anthropic upstream (strict + lossy).
func r2aBridgeCfg(t *testing.T) *config.Config {
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
api_key = "upstream-secret"

[providers.lossyanth]
dialect = "anthropic"
base_url = "http://127.0.0.1:9"
api_key = "upstream-secret"
compatibility = "lossy"

[[models]]
name = "bridge-resp"
provider = "anthup"
model = "claude-haiku-4-5"

[[models]]
name = "bridge-resp-lossy"
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

// r2aBridgeTightCfg is a single-slot Responses->Anthropic config with a
// tunable absolute deadline, for precommit-timeout and flush-failure tests
// where the slot must be proven recovered.
func r2aBridgeTightCfg(t *testing.T, deadlineSecs float64) *config.Config {
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

[providers.anthup]
dialect = "anthropic"
base_url = "http://127.0.0.1:9"
api_key = "upstream-secret"

[[models]]
name = "bridge-resp"
provider = "anthup"
model = "claude-haiku-4-5"
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

type r2aBridgeHarness struct {
	upstream *httptest.Server
	router   *httptest.Server
	mu       sync.Mutex
	lastPath string
	lastBody []byte
	lastHdr  http.Header
	calls    int
	handler  func(w http.ResponseWriter, r *http.Request)
}

func newR2ABridgeHarness(t *testing.T, cfg *config.Config) *r2aBridgeHarness {
	t.Helper()
	h := &r2aBridgeHarness{}
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_up","type":"message","role":"assistant","model":"claude-haiku-4-5",` +
			`"content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn",` +
			`"usage":{"input_tokens":8,"output_tokens":5,"cache_creation_input_tokens":1,"cache_read_input_tokens":4}}`))
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

func (h *r2aBridgeHarness) upstreamBody(t *testing.T) map[string]any {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	var v map[string]any
	if err := json.Unmarshal(h.lastBody, &v); err != nil {
		t.Fatalf("upstream body not JSON: %v", err)
	}
	return v
}

func (h *r2aBridgeHarness) numCalls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

// Responses ingress -> Anthropic upstream: JSON text translates, the public
// model is restored downstream, and Responses usage sums the cached buckets.
func TestResponsesToAnthropicJSONText(t *testing.T) {
	h := newR2ABridgeHarness(t, r2aBridgeCfg(t))
	status, _, data := postResponsesBridge(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"Hi","instructions":"Be brief.","temperature":0.5,"top_p":0.9}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if v["model"] != "bridge-resp" {
		t.Fatalf("public model not restored: %v", v["model"])
	}
	out, _ := v["output"].([]any)
	if len(out) != 1 {
		t.Fatalf("output=%v", v["output"])
	}
	msg, _ := out[0].(map[string]any)
	content, _ := msg["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["text"] != "hello" {
		t.Fatalf("content=%v", msg["content"])
	}
	usage, _ := v["usage"].(map[string]any)
	// Responses input total = Anthropic input + cache buckets (8+1+4).
	if usage["input_tokens"] != float64(13) {
		t.Fatalf("usage input_tokens=%v, want 13", usage["input_tokens"])
	}
	if det, _ := usage["input_tokens_details"].(map[string]any); det["cached_tokens"] != float64(4) {
		t.Fatalf("usage cached_tokens=%v", usage["input_tokens_details"])
	}
	// Tools echo comes from the validated plan: documented default.
	tools, _ := v["tools"].([]any)
	if len(tools) != 0 {
		t.Fatalf("tools echo=%v, want []", v["tools"])
	}
	if v["tool_choice"] != "auto" {
		t.Fatalf("tool_choice echo=%v, want auto", v["tool_choice"])
	}
	up := h.upstreamBody(t)
	if up["model"] != "claude-haiku-4-5" {
		t.Fatalf("upstream model=%v", up["model"])
	}
	// Absent Responses cap falls back to the documented Anthropic default.
	if up["max_tokens"] != float64(4096) {
		t.Fatalf("upstream max_tokens=%v, want default 4096", up["max_tokens"])
	}
	if h.lastPath != "/v1/messages" {
		t.Fatalf("upstream path=%s", h.lastPath)
	}
}

// Responses ingress -> Anthropic upstream: explicit strict:false is exact,
// large integers stay exact, and upstream auth is the provider key.
func TestResponsesToAnthropicJSONTools(t *testing.T) {
	h := newR2ABridgeHarness(t, r2aBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_up","type":"message","role":"assistant","model":"claude-haiku-4-5",` +
			`"content":[{"type":"tool_use","id":"call_up","name":"get_time","input":{"tz":"UTC","n":9007199254740993}}],` +
			`"stop_reason":"tool_use",` +
			`"usage":{"input_tokens":8,"output_tokens":5}}`))
	}
	status, _, data := postResponsesBridge(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"What time?","tools":[{"type":"function","name":"get_time","strict":false,`+
			`"parameters":{"type":"object","properties":{"tz":{"type":"string"}}}}],`+
			`"tool_choice":{"type":"function","name":"get_time"}}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if !strings.Contains(string(data), "9007199254740993") {
		t.Fatalf("large integer not exact downstream: %s", data)
	}
	if strings.Contains(string(data), "9007199254740992") {
		t.Fatalf("float64 precision loss downstream: %s", data)
	}
	up := h.upstreamBody(t)
	tools, _ := up["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("upstream tools=%v", up["tools"])
	}
	if tools[0].(map[string]any)["name"] != "get_time" {
		t.Fatalf("upstream tool=%v", tools[0])
	}
}

// Responses ingress -> Anthropic upstream: temperature above the Anthropic
// range is a diagnosed loss, refused in strict mode before upstream I/O.
func TestResponsesToAnthropicStrictLoss(t *testing.T) {
	h := newR2ABridgeHarness(t, r2aBridgeCfg(t))
	status, _, data := postResponsesBridge(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"Hi","temperature":1.5}`, nil)
	if status != 400 {
		t.Fatalf("status=%d body=%s, want strict 400", status, data)
	}
	if !strings.Contains(string(data), "translation_loss") {
		t.Fatalf("body lacks translation_loss: %s", data)
	}
	if h.numCalls() != 0 {
		t.Fatalf("upstream calls=%d, want 0 (pre-I/O refusal)", h.numCalls())
	}
}

// Responses ingress -> Anthropic upstream: lossy mode forwards with a loss
// header instead of refusing.
func TestResponsesToAnthropicLossyDrop(t *testing.T) {
	h := newR2ABridgeHarness(t, r2aBridgeCfg(t))
	status, headers, _ := postResponsesBridge(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-resp-lossy","input":"Hi","temperature":1.5}`, nil)
	if status != 200 {
		t.Fatalf("status=%d, want 200", status)
	}
	if headers.Get("X-Model-Router-Loss") == "" {
		t.Fatalf("lossy must carry loss header")
	}
	if h.numCalls() != 1 {
		t.Fatalf("upstream calls=%d, want 1", h.numCalls())
	}
}

// Responses ingress -> Anthropic upstream: malformed requests fail before
// upstream I/O.
func TestResponsesToAnthropicMalformed(t *testing.T) {
	h := newR2ABridgeHarness(t, r2aBridgeCfg(t))
	status, _, _ := postResponsesBridge(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-resp","bogus_field":1}`, nil)
	if status != 400 {
		t.Fatalf("status=%d, want 400", status)
	}
	if h.numCalls() != 0 {
		t.Fatalf("upstream calls=%d, want 0", h.numCalls())
	}
}

// Responses ingress -> Anthropic upstream: stateful references refuse with
// the stable state code before upstream I/O.
func TestResponsesToAnthropicStateRefusal(t *testing.T) {
	h := newR2ABridgeHarness(t, r2aBridgeCfg(t))
	status, _, data := postResponsesBridge(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"Hi","previous_response_id":"resp_1"}`, nil)
	if status != 400 {
		t.Fatalf("status=%d body=%s, want 400", status, data)
	}
	if !strings.Contains(string(data), "unsupported_responses_state") {
		t.Fatalf("body lacks unsupported_responses_state: %s", data)
	}
	if h.numCalls() != 0 {
		t.Fatalf("upstream calls=%d, want 0", h.numCalls())
	}
}

// Responses ingress -> Anthropic upstream: client credentials never become
// upstream credentials, and only Anthropic-destination opt-ins cross.
func TestResponsesToAnthropicCredentialIsolation(t *testing.T) {
	h := newR2ABridgeHarness(t, r2aBridgeCfg(t))
	status, _, _ := postResponsesBridge(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"Hi"}`,
		map[string]string{
			"Authorization":     "Bearer evil-client-key",
			"X-Api-Key":         "evil-client-key",
			"Anthropic-Beta":    "prompt-caching-2024-07-31",
			"Openai-Beta":       "assistants=v2",
			"Anthropic-Version": "2020-01-01",
		})
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	h.mu.Lock()
	hdr := h.lastHdr
	h.mu.Unlock()
	if got := hdr.Get("X-Api-Key"); got != "upstream-secret" {
		t.Fatalf("upstream X-Api-Key=%q", got)
	}
	if got := hdr.Get("Authorization"); got != "" {
		t.Fatalf("client Bearer leaked upstream: %q", got)
	}
	if got := hdr.Get("Anthropic-Beta"); got != "prompt-caching-2024-07-31" {
		t.Fatalf("Anthropic opt-in must cross: %q", got)
	}
	if got := hdr.Get("Openai-Beta"); got != "" {
		t.Fatalf("Chat opt-in must not cross to Anthropic: %q", got)
	}
	if got := hdr.Get("Anthropic-Version"); got != "2020-01-01" {
		t.Fatalf("client Anthropic-Version must be preserved: %q", got)
	}
}

var r2aSSETextFrames = []string{
	"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_up\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-haiku-4-5\",\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":7,\"cache_creation_input_tokens\":0,\"cache_read_input_tokens\":0}}}\n\n",
	"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hel\"}}\n\n",
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"lo\"}}\n\n",
	"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
	"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":5}}\n\n",
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
}

// Responses ingress -> Anthropic upstream: SSE text translates into the
// Responses event lifecycle with the validated echo and no [DONE].
func TestResponsesToAnthropicSSEText(t *testing.T) {
	h := newR2ABridgeHarness(t, r2aBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range r2aSSETextFrames {
			_, _ = io.WriteString(w, f)
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, _, data := postResponsesBridge(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"Hi","stream":true}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	text := string(data)
	for _, want := range []string{"response.created", "response.output_text.delta", "response.completed", `"text":"hello"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
	if strings.Contains(text, "[DONE]") {
		t.Fatalf("Responses downstream must not emit [DONE]: %s", text)
	}
	if strings.Contains(text, "response.failed") {
		t.Fatalf("valid terminal must not emit response.failed: %s", text)
	}
	if strings.Contains(text, "bridge-resp") == false {
		t.Fatalf("public model missing downstream: %s", text)
	}
}

// Responses ingress -> Anthropic upstream: tool_use blocks become
// function_call items with exact argument bytes.
func TestResponsesToAnthropicSSETools(t *testing.T) {
	h := newR2ABridgeHarness(t, r2aBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		frames := []string{
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_up\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-haiku-4-5\",\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":7,\"cache_creation_input_tokens\":0,\"cache_read_input_tokens\":0}}}\n\n",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_1\",\"name\":\"get_time\",\"input\":{}}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"tz\\\":\\\"UTC\\\"\"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\",\\\"n\\\":9007199254740993}\"}}\n\n",
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":5}}\n\n",
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		}
		for _, f := range frames {
			_, _ = io.WriteString(w, f)
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, _, data := postResponsesBridge(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"What time?","stream":true,`+
			`"tools":[{"type":"function","name":"get_time","strict":false,`+
			`"parameters":{"type":"object","properties":{"tz":{"type":"string"}}}}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	text := string(data)
	if !strings.Contains(text, "function_call") {
		t.Fatalf("missing function_call item: %s", text)
	}
	if !strings.Contains(text, "9007199254740993") {
		t.Fatalf("large integer not exact: %s", text)
	}
	if strings.Contains(text, "9007199254740992") {
		t.Fatalf("float64 precision loss: %s", text)
	}
	if strings.Count(text, "response.failed") != 0 {
		t.Fatalf("valid terminal must not emit response.failed: %s", text)
	}
}

// Responses ingress -> Anthropic upstream: garbage upstream SSE before
// commitment is an ordinary 502, never an empty 200.
func TestResponsesToAnthropicEarlyInvalidStream(t *testing.T) {
	h := newR2ABridgeHarness(t, r2aBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: not-json\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, _, data := postResponsesBridge(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"Hi","stream":true}`, nil)
	if status != 502 {
		t.Fatalf("status=%d body=%s, want 502", status, data)
	}
	if strings.Contains(string(data), "event: response.created") {
		t.Fatalf("pre-commit failure must not commit headers: %s", data)
	}
}

// Responses ingress -> Anthropic upstream: a stalled upstream body past the
// absolute deadline is a pre-commit 504, the upstream body is closed
// promptly, and the generation slot is recovered.
func TestResponsesToAnthropicPrecommitTimeout504(t *testing.T) {
	h := newR2ABridgeHarness(t, r2aBridgeTightCfg(t, 1))
	closed := make(chan struct{})
	var closeOnce sync.Once
	var calls atomic.Int32
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"msg_up","type":"message","role":"assistant","model":"claude-haiku-4-5",` +
				`"content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn",` +
				`"usage":{"input_tokens":8,"output_tokens":5}}`))
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
	status, _, data := postResponsesBridge(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"Hi","stream":true}`, nil)
	if status != 504 {
		t.Fatalf("status=%d body=%s, want 504", status, data)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatalf("stalled upstream was not closed after pre-commit 504")
	}
	status, _, data = postResponsesBridge(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"Hi"}`, nil)
	if status != 200 {
		t.Fatalf("slot not recovered after 504: status=%d body=%s", status, data)
	}
}

// Responses ingress -> Anthropic upstream: truncation after commitment keeps
// 200 and emits exactly one response.failed with the committed response ID
// and the public model — never a fabricated completed or a duplicate.
func TestResponsesToAnthropicPostCommitTruncation(t *testing.T) {
	h := newR2ABridgeHarness(t, r2aBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		frames := r2aSSETextFrames[:4] // created, in_progress, item added, part added, one delta
		for _, f := range frames {
			_, _ = io.WriteString(w, f)
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Truncate: close without terminal events.
	}
	status, _, data := postResponsesBridge(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"Hi","stream":true}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s, want 200 (headers committed)", status, data)
	}
	text := string(data)
	if !strings.Contains(text, `"delta":"hel"`) {
		t.Fatalf("committed delta missing before failure: %s", text)
	}
	if strings.Count(text, "event: response.failed") != 1 {
		t.Fatalf("want exactly one response.failed, got %d: %s", strings.Count(text, "event: response.failed"), text)
	}
	if strings.Contains(text, "response.completed") {
		t.Fatalf("post-commit failure must not fabricate completed: %s", text)
	}
	if !strings.Contains(text, `"model":"bridge-resp"`) {
		t.Fatalf("failure frame must carry the public model: %s", text)
	}
}

// Responses ingress -> Anthropic upstream: an unrepresentable event after
// commitment keeps 200 with one response.failed and no success marker.
func TestResponsesToAnthropicMidstreamUnsupported(t *testing.T) {
	h := newR2ABridgeHarness(t, r2aBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		frames := append([]string{}, r2aSSETextFrames[:5]...)
		// Citations have no Responses representation: a mid-stream block
		// carrying one fails the translation after commitment.
		frames = append(frames,
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\",\"citations\":[{\"type\":\"x\"}]}}\n\n")
		for _, f := range frames {
			_, _ = io.WriteString(w, f)
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, _, data := postResponsesBridge(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"Hi","stream":true}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s, want 200 (headers committed)", status, data)
	}
	text := string(data)
	if strings.Count(text, "event: response.failed") != 1 {
		t.Fatalf("want exactly one response.failed, got %d: %s", strings.Count(text, "event: response.failed"), text)
	}
	if strings.Contains(text, "response.completed") {
		t.Fatalf("failure must not fabricate completed: %s", text)
	}
}

// Responses ingress -> Anthropic upstream: model text containing framing-like
// tokens is payload, never framing. One truncation emits one failure frame.
func TestResponsesToAnthropicModelTextIsNotFraming(t *testing.T) {
	h := newR2ABridgeHarness(t, r2aBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		frames := []string{
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_up\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-haiku-4-5\",\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":7,\"cache_creation_input_tokens\":0,\"cache_read_input_tokens\":0}}}\n\n",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"say event: response.failed aloud\"}}\n\n",
		}
		for _, f := range frames {
			_, _ = io.WriteString(w, f)
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, _, data := postResponsesBridge(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"Hi","stream":true}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s, want 200 (headers committed)", status, data)
	}
	text := string(data)
	if !strings.Contains(text, "say event: response.failed aloud") {
		t.Fatalf("model text payload missing: %s", text)
	}
	// Count framed events, not payload substrings: the payload line is a
	// "data:" line, so only real event lines match this prefix.
	if n := strings.Count(text, "event: response.failed\n"); n != 1 {
		t.Fatalf("payload text must not count as framing; want 1 failed frame, got %d: %s", n, text)
	}
}

// Responses ingress -> Anthropic upstream: a valid terminal returns promptly
// without waiting for EOF, and the generation slot is released while the
// upstream body stays open.
func TestResponsesToAnthropicPromptReturnReleasesSlot(t *testing.T) {
	cfg := r2aBridgeTightCfg(t, 30)
	var n atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) > 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"msg_up","type":"message","role":"assistant","model":"claude-haiku-4-5",` +
				`"content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn",` +
				`"usage":{"input_tokens":8,"output_tokens":5}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range r2aSSETextFrames {
			_, _ = io.WriteString(w, f)
		}
		w.(http.Flusher).Flush()
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
	status, _, data := postResponsesBridge(t, routerSrv.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"Hi","stream":true}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if !strings.Contains(string(data), "event: response.completed") {
		t.Fatalf("valid terminal must end with response.completed: %s", data)
	}
	status, _, data = postResponsesBridge(t, routerSrv.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"Hi"}`, nil)
	if status != 200 {
		t.Fatalf("slot not released after valid terminal without EOF: status=%d body=%s", status, data)
	}
}

// Responses ingress -> Anthropic upstream: a downstream flush failure
// cancels the upstream body promptly and releases the generation slot.
func TestResponsesToAnthropicFlushFailureCancelsUpstream(t *testing.T) {
	cfg := r2aBridgeTightCfg(t, 30)
	var n atomic.Int32
	closed := make(chan struct{})
	var closeOnce sync.Once
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) > 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"msg_up","type":"message","role":"assistant","model":"claude-haiku-4-5",` +
				`"content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn",` +
				`"usage":{"input_tokens":8,"output_tokens":5}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		// A committing prefix forces the translator to commit downstream;
		// the body is then held open so the flush failure must terminate
		// promptly via the synchronous upstream close.
		for _, f := range r2aSSETextFrames[:4] {
			_, _ = io.WriteString(w, f)
		}
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
	body := `{"model":"bridge-resp","input":"Hi","stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
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
	status, _, data := postResponsesBridge(t, routerSrv.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"Hi"}`, nil)
	if status == 503 {
		t.Fatalf("generation slot leaked after flush failure (body %s)", data)
	}
	if status != 200 {
		t.Fatalf("second request after flush failure: status=%d body=%s", status, data)
	}
}
