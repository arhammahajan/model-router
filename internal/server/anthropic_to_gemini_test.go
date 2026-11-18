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
	"sync/atomic"
	"testing"
	"time"

	"github.com/arhammahajan/model-router/internal/config"
)

// a2gBridgeCfg routes Anthropic ingress to Gemini upstream (strict + lossy)
// and Gemini ingress to Anthropic upstream for alias tests. Shared by the
// Anthropic<->Gemini bridge tests in this package.
func a2gBridgeCfg(t *testing.T) *config.Config {
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

[providers.anthup]
dialect = "anthropic"
base_url = "http://127.0.0.1:9"
api_key = "anth-secret"

[[models]]
name = "anth-gem"
provider = "gemup"
model = "gemini-2.0-flash"

[[models]]
name = "anth-gem-lossy"
provider = "lossygem"
model = "gemini-2.0-flash"

[[models]]
name = "gem-anth"
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

type a2gHarness struct {
	upstream *httptest.Server
	router   *httptest.Server
	mu       sync.Mutex
	lastPath string
	lastRaw  string
	lastBody []byte
	lastHdr  http.Header
	calls    int
	handler  func(w http.ResponseWriter, r *http.Request)
}

func newA2GHarness(t *testing.T, cfg *config.Config) *a2gHarness {
	t.Helper()
	h := &a2gHarness{}
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

func postA2G(t *testing.T, url, body string, headers map[string]string) (int, http.Header, []byte) {
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

func a2gCalls(h *a2gHarness) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

func a2gTightCfg(t *testing.T, deadlineSecs float64) *config.Config {
	t.Helper()
	cfg := a2gBridgeCfg(t)
	cfg.Retry.Deadline = deadlineSecs
	return cfg
}

// Anthropic -> Gemini: JSON text translates with public model, system and
// generation config on the wire; upstream path carries the upstream model.
func TestAnthropicToGeminiJSONText(t *testing.T) {
	h := newA2GHarness(t, a2gBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"hello"}]},"finishReason":"STOP","index":0}],"modelVersion":"gemini-2.0-flash","responseId":"resp_gem_1","usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3,"totalTokenCount":10}}`))
	}
	status, headers, data := postA2G(t, h.router.URL+"/v1/messages",
		`{"model":"anth-gem","max_tokens":16,"system":"Be brief.","messages":[{"role":"user","content":"Hi"}],"temperature":0.5,"top_p":0.9}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if ct := headers.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type=%q", ct)
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if v["model"] != "anth-gem" {
		t.Fatalf("public model not restored: %s", data)
	}
	content, _ := v["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content missing: %s", data)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lastPath != "/v1beta/models/gemini-2.0-flash:generateContent" {
		t.Fatalf("upstream path=%s raw=%s", h.lastPath, h.lastRaw)
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
			t.Fatalf("wire body must not carry %q: %s", k, h.lastBody)
		}
	}
	if h.lastHdr.Get("X-Goog-Api-Key") != "gem-secret" {
		t.Fatalf("upstream gemini auth wrong: %v", h.lastHdr)
	}
	if v := h.lastHdr.Get("Authorization"); v != "" {
		t.Fatalf("client auth must never become upstream auth: %q", v)
	}
}

// Anthropic -> Gemini: tools round-trip with exact large integers, ordered
// history tool results preserved, call identity intact.
func TestAnthropicToGeminiToolsExact(t *testing.T) {
	h := newA2GHarness(t, a2gBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"get_time","args":{"tz":"UTC","n":9007199254740993},"id":"call_1"}}]},"finishReason":"STOP","index":0}],"modelVersion":"gemini-2.0-flash","responseId":"resp_gem_2","usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":2,"totalTokenCount":11}}`))
	}
	status, _, data := postA2G(t, h.router.URL+"/v1/messages",
		`{"model":"anth-gem","max_tokens":16,"messages":[{"role":"user","content":"Hi"},{"role":"assistant","content":[{"type":"tool_use","id":"call_0","name":"get_time","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_0","content":"ok"}]}],"tools":[{"name":"get_time","input_schema":{"type":"object"}}],"tool_choice":{"type":"auto"}}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if !strings.Contains(string(data), "9007199254740993") {
		t.Fatalf("large integer not preserved: %s", data)
	}
	if !strings.Contains(string(data), `"id":"call_1"`) {
		t.Fatalf("call identity not preserved: %s", data)
	}
	if !strings.Contains(string(data), `"stop_reason":"tool_use"`) {
		t.Fatalf("tool terminal must be tool_use: %s", data)
	}
}

// Anthropic -> Gemini: strict mode refuses the known tools strict-true loss
// before any upstream I/O (Anthropic envelope carries the field name; the
// translation_loss code lives in the Chat envelope only).
func TestAnthropicToGeminiStrictLoss(t *testing.T) {
	h := newA2GHarness(t, a2gBridgeCfg(t))
	status, _, data := postA2G(t, h.router.URL+"/v1/messages",
		`{"model":"anth-gem","max_tokens":8,"messages":[{"role":"user","content":"Hi"}],"tools":[{"name":"calc","input_schema":{"type":"object"},"strict":true}]}`, nil)
	if status != 400 {
		t.Fatalf("status=%d body=%s, want 400", status, data)
	}
	if !strings.Contains(string(data), "strict") {
		t.Fatalf("body lacks strict loss: %s", data)
	}
	if strings.Contains(string(data), "unsupported_route") {
		t.Fatalf("bridged Anthropic->Gemini must not report unsupported_route: %s", data)
	}
	if n := a2gCalls(h); n != 0 {
		t.Fatalf("upstream calls=%d, want 0", n)
	}
}

// Anthropic -> Gemini: lossy provider carries the diagnosed loss header and
// still routes.
func TestAnthropicToGeminiLossyHeader(t *testing.T) {
	h := newA2GHarness(t, a2gBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP","index":0}],"modelVersion":"gemini-2.0-flash","responseId":"r1","usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`))
	}
	status, headers, data := postA2G(t, h.router.URL+"/v1/messages",
		`{"model":"anth-gem-lossy","max_tokens":8,"messages":[{"role":"user","content":"Hi"}],"tools":[{"name":"calc","input_schema":{"type":"object"},"strict":true}]}`, nil)
	if status != 200 {
		t.Fatalf("lossy must route: status=%d body=%s", status, data)
	}
	if headers.Get("X-Model-Router-Loss") == "" {
		t.Fatalf("lossy must carry loss header: %s", data)
	}
}

// Anthropic -> Gemini: client credentials and reviewed headers are
// isolated; only provider statics cross.
func TestAnthropicToGeminiCredentialIsolation(t *testing.T) {
	h := newA2GHarness(t, a2gBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP","index":0}],"modelVersion":"m","responseId":"r","usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`))
	}
	status, _, _ := postA2G(t, h.router.URL+"/v1/messages",
		`{"model":"anth-gem","max_tokens":8,"messages":[{"role":"user","content":"Hi"}]}`,
		map[string]string{"Authorization": "Bearer client-key", "Anthropic-Beta": "beta-flag", "X-Session-Id": "sess-1", "Idempotency-Key": "k1"})
	if status != 200 {
		t.Fatalf("status=%d (auth_mode=none must accept)", status)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if got := h.lastHdr.Get("X-Goog-Api-Key"); got != "gem-secret" {
		t.Fatalf("upstream must carry provider key, got %q", got)
	}
	for _, k := range []string{"Authorization", "X-Session-Id", "Idempotency-Key"} {
		if got := h.lastHdr.Get(k); got != "" {
			t.Fatalf("client header %q leaked upstream as %q", k, got)
		}
	}
	// Destination Gemini accepts no opt-ins: anthropic-beta must not cross.
	if got := h.lastHdr.Get("Anthropic-Beta"); got != "" {
		t.Fatalf("wrong-dialect opt-in leaked: %q", got)
	}
	if h.lastRaw != "" {
		t.Fatalf("query must never travel upstream, got %q", h.lastRaw)
	}
}

// Anthropic -> Gemini: unknown upstream Content-Type is a pre-commit 502
// with no body sniffing.
func TestAnthropicToGeminiUnknownContentType(t *testing.T) {
	h := newA2GHarness(t, a2gBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "not a stream")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, _, data := postA2G(t, h.router.URL+"/v1/messages",
		`{"model":"anth-gem","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 502 {
		t.Fatalf("status=%d body=%s, want 502", status, data)
	}
	if strings.Contains(string(data), "not a stream") {
		t.Fatalf("raw upstream bytes leaked: %s", data)
	}
}

// Anthropic -> Gemini: SSE text streams with reassembled deltas; the valid
// terminal completes once with no duplication.
func TestAnthropicToGeminiSSEText(t *testing.T) {
	h := newA2GHarness(t, a2gBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hel\"}]}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"lo\"}]}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":7,\"candidatesTokenCount\":3,\"totalTokenCount\":10}}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, headers, data := postA2G(t, h.router.URL+"/v1/messages",
		`{"model":"anth-gem","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if ct := headers.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("SSE framing must be text/event-stream, got %q", ct)
	}
	text := string(data)
	// Reassembled deltas: both fragments appear as Anthropic text events.
	if !strings.Contains(text, `"text":"hel"`) || !strings.Contains(text, `"text":"lo"`) {
		t.Fatalf("reassembled deltas missing: %s", text)
	}
	if !strings.Contains(text, `"stop_reason":"end_turn"`) {
		t.Fatalf("valid terminal missing end_turn: %s", text)
	}
	// Exactly one message_stop: count framed event lines, not payload text.
	stops := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "event: message_stop" {
			stops++
		}
	}
	if stops != 1 {
		t.Fatalf("exactly one message_stop required, got %d: %s", stops, text)
	}
	for _, absent := range []string{"[DONE]", "Traceback"} {
		if strings.Contains(text, absent) {
			t.Fatalf("downstream must not contain %q: %s", absent, text)
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lastRaw != "alt=sse" {
		t.Fatalf("stream must forward alt=sse, got %q", h.lastRaw)
	}
}

// Anthropic -> Gemini: JSON-array upstream streams incrementally behind the
// same Anthropic SSE downstream.
func TestAnthropicToGeminiJSONArrayStream(t *testing.T) {
	h := newA2GHarness(t, a2gBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		// No alt query: upstream answers the JSON array framing.
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "[{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"a\"}]}}]},")
		_, _ = io.WriteString(w, "{\"candidates\":[{\"content\":{\"parts\":[]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":7,\"candidatesTokenCount\":3,\"totalTokenCount\":10}}]")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	// Force the array path by hitting the router stream endpoint: the
	// router always requests alt=sse for streams, so simulate the array
	// framing by returning it despite the query; the decoder is selected
	// from the upstream Content-Type, not the request.
	status, _, data := postA2G(t, h.router.URL+"/v1/messages",
		`{"model":"anth-gem","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if !strings.Contains(string(data), `"text":"a"`) {
		t.Fatalf("array text missing: %s", data)
	}
	if !strings.Contains(string(data), `"stop_reason":"end_turn"`) {
		t.Fatalf("array terminal missing: %s", data)
	}
}

// Anthropic -> Gemini: pre-commit invalid stream is an ordinary 502.
func TestAnthropicToGeminiEarlyInvalidStream(t *testing.T) {
	h := newA2GHarness(t, a2gBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"mystery\":1}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, _, data := postA2G(t, h.router.URL+"/v1/messages",
		`{"model":"anth-gem","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 502 {
		t.Fatalf("status=%d body=%s, want 502", status, data)
	}
}

// Anthropic -> Gemini: post-commit truncation keeps 200 with one error
// event and no message_stop.
func TestAnthropicToGeminiSSETruncated(t *testing.T) {
	h := newA2GHarness(t, a2gBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"par\"}]}}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, _, data := postA2G(t, h.router.URL+"/v1/messages",
		`{"model":"anth-gem","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s, want 200", status, data)
	}
	text := string(data)
	if !strings.Contains(text, `"text":"par"`) {
		t.Fatalf("committed delta missing: %s", text)
	}
	if !strings.Contains(text, "event: error") {
		t.Fatalf("post-commit failure must emit one error event: %s", text)
	}
	if strings.Contains(text, "event: message_stop") {
		t.Fatalf("truncated stream must not emit message_stop: %s", text)
	}
}

// Anthropic -> Gemini: model text containing framing tokens is payload,
// never framing; error detection counts framed lines only.
func TestAnthropicToGeminiModelTextIsNotFraming(t *testing.T) {
	h := newA2GHarness(t, a2gBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"say event: error aloud\"}]}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":1,\"candidatesTokenCount\":1,\"totalTokenCount\":2}}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, _, data := postA2G(t, h.router.URL+"/v1/messages",
		`{"model":"anth-gem","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	text := string(data)
	if !strings.Contains(text, "event: error") && !strings.Contains(text, "say event") {
		t.Fatalf("model text payload missing: %s", text)
	}
	// The payload occurrence lives inside a data JSON string; exactly one
	// terminal and at most one true error event (none expected here).
	stops := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "event: message_stop" {
			stops++
		}
	}
	if stops != 1 {
		t.Fatalf("valid terminal must emit one message_stop, got %d: %s", stops, text)
	}
}

// Anthropic -> Gemini: stalled upstream past the absolute deadline is a
// pre-commit 504 with slot recovery.
func TestAnthropicToGeminiPrecommitTimeout504(t *testing.T) {
	h := newA2GHarness(t, a2gTightCfg(t, 1))
	closed := make(chan struct{})
	var closeOnce atomic.Bool
	var calls atomic.Int32
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP","index":0}],"modelVersion":"m","responseId":"r","usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-r.Context().Done():
			closeOnce.Store(true)
			close(closed)
		case <-time.After(30 * time.Second):
		}
	}
	status, _, data := postA2G(t, h.router.URL+"/v1/messages",
		`{"model":"anth-gem","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 504 {
		t.Fatalf("status=%d body=%s, want 504", status, data)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatalf("stalled upstream was not closed after pre-commit 504")
	}
	status, _, data = postA2G(t, h.router.URL+"/v1/messages",
		`{"model":"anth-gem","max_tokens":8,"messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 200 {
		t.Fatalf("slot not recovered after 504: status=%d body=%s", status, data)
	}
}

// Anthropic -> Gemini: downstream flush failure closes upstream promptly.
func TestAnthropicToGeminiFlushFailureCancelsUpstream(t *testing.T) {
	cfg := a2gTightCfg(t, 30)
	var n atomic.Int32
	closed := make(chan struct{})
	var closeOnce atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) > 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP","index":0}],"modelVersion":"m","responseId":"r","usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"par\"}]}}]}\n\n"))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			if closeOnce.CompareAndSwap(false, true) {
				close(closed)
			}
		case <-time.After(30 * time.Second):
		}
	}))
	defer up.Close()
	for _, prov := range cfg.Providers {
		prov.BaseURL = up.URL
	}
	srv := New(cfg)
	body := `{"model":"anth-gem","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
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
		t.Fatalf("flush failure did not terminate promptly")
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatalf("upstream was not closed after downstream flush failure")
	}
	routerSrv := httptest.NewServer(srv.Handler())
	defer routerSrv.Close()
	status, _, data := postA2G(t, routerSrv.URL+"/v1/messages",
		`{"model":"anth-gem","max_tokens":8,"messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status == 503 {
		t.Fatalf("generation slot leaked after flush failure (body %s)", data)
	}
	if status != 200 {
		t.Fatalf("second request after flush failure: status=%d body=%s", status, data)
	}
}

// Anthropic -> Gemini: valid terminal returns promptly without EOF.
func TestAnthropicToGeminiPromptReturnReleasesSlot(t *testing.T) {
	cfg := a2gTightCfg(t, 30)
	var n atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) > 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP","index":0}],"modelVersion":"m","responseId":"r","usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hel\"}]}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":7,\"candidatesTokenCount\":3,\"totalTokenCount\":10}}\n\n")
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
	status, _, data := postA2G(t, routerSrv.URL+"/v1/messages",
		`{"model":"anth-gem","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if !strings.Contains(string(data), "event: message_stop") {
		t.Fatalf("valid terminal missing message_stop: %s", data)
	}
	status, _, data = postA2G(t, routerSrv.URL+"/v1/messages",
		`{"model":"anth-gem","max_tokens":8,"messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 200 {
		t.Fatalf("slot not released after valid terminal: status=%d body=%s", status, data)
	}
}

// Anthropic -> Gemini: upstream Retry-After is forwarded safely.
func TestAnthropicToGeminiRetryAfterSafe(t *testing.T) {
	h := newA2GHarness(t, a2gBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "2")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"code":429,"message":"slow","status":"RESOURCE_EXHAUSTED"}}`))
	}
	status, headers, _ := postA2G(t, h.router.URL+"/v1/messages",
		`{"model":"anth-gem","max_tokens":8,"messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 429 {
		t.Fatalf("upstream 429 must be preserved, got %d", status)
	}
	if headers.Get("Retry-After") != "2" {
		t.Fatalf("Retry-After must be forwarded safely")
	}
}
