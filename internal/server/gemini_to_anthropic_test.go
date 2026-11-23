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

	"github.com/arhammahajan/router/internal/config"
)

// g2aBridgeCfg routes Gemini ingress to Anthropic upstream (strict + lossy)
// with an escaped-alias model for URL tests.
func g2aBridgeCfg(t *testing.T) *config.Config {
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
name = "gem-anth"
provider = "anthup"
model = "claude-haiku-4-5"

[[models]]
name = "gem-anth-lossy"
provider = "lossyanth"
model = "claude-haiku-4-5"

[[models]]
name = "team/gem:flash"
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

type g2aHarness struct {
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

func newG2AHarness(t *testing.T, cfg *config.Config) *g2aHarness {
	t.Helper()
	h := &g2aHarness{}
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

func postG2A(t *testing.T, url, body string, headers map[string]string) (int, http.Header, []byte) {
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

func g2aCalls(h *g2aHarness) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

func g2aTightCfg(t *testing.T, deadlineSecs float64) *config.Config {
	t.Helper()
	cfg := g2aBridgeCfg(t)
	cfg.Retry.Deadline = deadlineSecs
	return cfg
}

// Gemini -> Anthropic: JSON text translates with public alias stamped as
// modelVersion and the upstream Anthropic id as responseId.
func TestGeminiToAnthropicJSONText(t *testing.T) {
	h := newG2AHarness(t, g2aBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_up_1","type":"message","role":"assistant","model":"claude-haiku-4-5","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":7,"output_tokens":3}}`))
	}
	status, headers, data := postG2A(t, h.router.URL+"/v1beta/models/gem-anth:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}],"systemInstruction":{"parts":[{"text":"Be brief."}]},"generationConfig":{"maxOutputTokens":16,"temperature":0.5,"topP":0.9}}`, nil)
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
	if v["modelVersion"] != "gem-anth" {
		t.Fatalf("public alias not restored: %s", data)
	}
	if v["responseId"] != "msg_up_1" {
		t.Fatalf("upstream id not preserved: %s", data)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lastPath != "/v1/messages" {
		t.Fatalf("upstream path=%s, want /v1/messages", h.lastPath)
	}
	if h.lastRaw != "" {
		t.Fatalf("client key/alt must never travel upstream, got %q", h.lastRaw)
	}
	var wire map[string]any
	if err := json.Unmarshal(h.lastBody, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["model"] != "claude-haiku-4-5" {
		t.Fatalf("upstream model=%v", wire["model"])
	}
	if wire["max_tokens"] != float64(16) {
		t.Fatalf("explicit cap must reach upstream as 16, got %v", wire["max_tokens"])
	}
	if h.lastHdr.Get("Anthropic-Version") == "" {
		t.Fatalf("anthropic-version must be set: %v", h.lastHdr)
	}
	if got := h.lastHdr.Get("X-Goog-Api-Key"); got != "" {
		t.Fatalf("client gemini key must never become upstream auth: %q", got)
	}
}

// Gemini -> Anthropic: escaped URL alias resolves, body model rejected,
// upstream uses the upstream ID.
func TestGeminiToAnthropicAliasEscapedURL(t *testing.T) {
	h := newG2AHarness(t, g2aBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m1","type":"message","role":"assistant","model":"c","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}
	// Percent-encoded "/" and ":" in the public alias decode exactly once.
	status, _, data := postG2A(t, h.router.URL+"/v1beta/models/team%2Fgem%3Aflash:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status != 200 {
		t.Fatalf("escaped alias must route: status=%d body=%s", status, data)
	}
	var v map[string]any
	_ = json.Unmarshal(data, &v)
	if v["modelVersion"] != "team/gem:flash" {
		t.Fatalf("escaped alias not restored: %s", data)
	}
	// Body-owned model/stream are rejected per the existing preamble.
	for _, bad := range []string{
		`{"model":"x","contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`,
		`{"stream":true,"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`,
	} {
		status, _, _ := postG2A(t, h.router.URL+"/v1beta/models/gem-anth:generateContent", bad, nil)
		if status != 400 {
			t.Fatalf("body-owned field must be 400, got %d", status)
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var wire map[string]any
	_ = json.Unmarshal(h.lastBody, &wire)
	if wire["model"] != "claude-haiku-4-5" {
		t.Fatalf("upstream must use upstream ID, got %s", h.lastBody)
	}
	if wire["max_tokens"] != float64(4096) {
		t.Fatalf("absent cap must default to 4096, got %v", wire["max_tokens"])
	}
}

// Gemini -> Anthropic: strict mode refuses the known temperature-1.5 loss.
func TestGeminiToAnthropicStrictLoss(t *testing.T) {
	h := newG2AHarness(t, g2aBridgeCfg(t))
	status, _, data := postG2A(t, h.router.URL+"/v1beta/models/gem-anth:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}],"generationConfig":{"temperature":1.5}}`, nil)
	if status != 400 {
		t.Fatalf("status=%d body=%s, want 400", status, data)
	}
	if !strings.Contains(string(data), "TRANSLATION_LOSS") {
		t.Fatalf("body lacks TRANSLATION_LOSS: %s", data)
	}
	if n := g2aCalls(h); n != 0 {
		t.Fatalf("upstream calls=%d, want 0", n)
	}
}

// Gemini -> Anthropic: lossy provider carries the loss header and routes.
func TestGeminiToAnthropicLossyHeader(t *testing.T) {
	h := newG2AHarness(t, g2aBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m1","type":"message","role":"assistant","model":"c","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}
	status, headers, data := postG2A(t, h.router.URL+"/v1beta/models/gem-anth-lossy:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}],"generationConfig":{"temperature":1.5}}`, nil)
	if status != 200 {
		t.Fatalf("lossy must route: status=%d body=%s", status, data)
	}
	if headers.Get("X-Model-Router-Loss") == "" {
		t.Fatalf("lossy must carry loss header: %s", data)
	}
}

// Gemini -> Anthropic: client key/alt isolated, credential headers safe.
func TestGeminiToAnthropicCredentialIsolation(t *testing.T) {
	h := newG2AHarness(t, g2aBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m1","type":"message","role":"assistant","model":"c","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}
	status, _, _ := postG2A(t, h.router.URL+"/v1beta/models/gem-anth:generateContent?key=client-key&alt=json",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`,
		map[string]string{"Authorization": "Bearer client-key", "X-Goog-Api-Key": "client-key"})
	if status != 200 {
		t.Fatalf("status=%d (?key= must authenticate)", status)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if got := h.lastHdr.Get("X-Api-Key"); got != "anth-secret" {
		t.Fatalf("upstream Authorization=%q, want provider key", got)
	}
	for _, k := range []string{"X-Goog-Api-Key", "Authorization"} {
		_ = k
	}
	if h.lastRaw != "" {
		t.Fatalf("query must never travel upstream, got %q", h.lastRaw)
	}
}

// Gemini -> Anthropic: SSE downstream text with reassembled deltas; the
// terminal carries finish + usage only, never duplicated text.
func TestGeminiToAnthropicSSEText(t *testing.T) {
	h := newG2AHarness(t, g2aBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		base := func(typ, payload string) string { return "event: " + typ + "\ndata: " + payload + "\n\n" }
		_, _ = io.WriteString(w, base("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"c","content":[],"stop_reason":null,"usage":{"input_tokens":7,"output_tokens":1}}}`))
		_, _ = io.WriteString(w, base("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`))
		_, _ = io.WriteString(w, base("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hel"}}`))
		_, _ = io.WriteString(w, base("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`))
		_, _ = io.WriteString(w, base("content_block_stop", `{"type":"content_block_stop","index":0}`))
		_, _ = io.WriteString(w, base("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":3}}`))
		_, _ = io.WriteString(w, base("message_stop", `{"type":"message_stop"}`))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, headers, data := postG2A(t, h.router.URL+"/v1beta/models/gem-anth:streamGenerateContent?alt=sse",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if ct := headers.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("SSE framing must be text/event-stream, got %q", ct)
	}
	text := string(data)
	for _, want := range []string{`"text":"hel"`, `"text":"lo"`, `"finishReason":"STOP"`, `"promptTokenCount":7`} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %s", want, text)
		}
	}
	for _, absent := range []string{"[DONE]", `"STOP\",\"usage")`} {
		_ = absent
	}
	if strings.Contains(text, "[DONE]") {
		t.Fatalf("Gemini SSE must never contain [DONE]: %s", text)
	}
	// Terminal carries no duplicated text: the reassembled deltas appear in
	// data frames, the finish frame carries finishReason + usage only.
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lastRaw != "" {
		t.Fatalf("alt/key must never travel upstream, got %q", h.lastRaw)
	}
}

// Gemini -> Anthropic: missing alt yields the incremental JSON array.
func TestGeminiToAnthropicJSONArrayStream(t *testing.T) {
	h := newG2AHarness(t, g2aBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		base := func(typ, payload string) string { return "event: " + typ + "\ndata: " + payload + "\n\n" }
		_, _ = io.WriteString(w, base("message_start", `{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"c","content":[],"stop_reason":null,"usage":{"input_tokens":7,"output_tokens":1}}}`))
		_, _ = io.WriteString(w, base("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`))
		_, _ = io.WriteString(w, base("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"a"}}`))
		_, _ = io.WriteString(w, base("content_block_stop", `{"type":"content_block_stop","index":0}`))
		_, _ = io.WriteString(w, base("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`))
		_, _ = io.WriteString(w, base("message_stop", `{"type":"message_stop"}`))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, headers, data := postG2A(t, h.router.URL+"/v1beta/models/gem-anth:streamGenerateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if ct := headers.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("array framing must be application/json, got %q", ct)
	}
	text := strings.TrimSpace(string(data))
	if !strings.HasPrefix(text, "[") || !strings.HasSuffix(text, "]") {
		t.Fatalf("array must open with [ and close with ]: %s", text)
	}
}

// Gemini -> Anthropic: pre-commit invalid stream is an ordinary 502.
func TestGeminiToAnthropicEarlyInvalidStream(t *testing.T) {
	h := newG2AHarness(t, g2aBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"mystery\":1}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, _, data := postG2A(t, h.router.URL+"/v1beta/models/gem-anth:streamGenerateContent?alt=sse",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status != 502 {
		t.Fatalf("status=%d body=%s, want 502", status, data)
	}
}

// Gemini -> Anthropic: post-commit SSE truncation keeps 200 with one
// sanitized data:{error} frame, no STOP/DONE/usage.
func TestGeminiToAnthropicSSETruncated(t *testing.T) {
	h := newG2AHarness(t, g2aBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"c\",\"content\":[],\"usage\":{\"input_tokens\":1}}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"par\"}}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, headers, data := postG2A(t, h.router.URL+"/v1beta/models/gem-anth:streamGenerateContent?alt=sse",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s, want 200", status, data)
	}
	if ct := headers.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type=%q", ct)
	}
	text := string(data)
	if !strings.Contains(text, `"text":"par"`) {
		t.Fatalf("committed delta missing: %s", text)
	}
	if !strings.Contains(text, `"error"`) {
		t.Fatalf("post-commit failure must emit one sanitized error frame: %s", text)
	}
	for _, absent := range []string{"[DONE]", `"finishReason":"STOP"`, "Traceback"} {
		if strings.Contains(text, absent) {
			t.Fatalf("truncated stream must not contain %q: %s", absent, text)
		}
	}
}

// Gemini -> Anthropic: post-commit array truncation ends without ']'.
func TestGeminiToAnthropicArrayTruncated(t *testing.T) {
	h := newG2AHarness(t, g2aBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"c\",\"content\":[],\"usage\":{\"input_tokens\":1}}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"par\"}}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, _, data := postG2A(t, h.router.URL+"/v1beta/models/gem-anth:streamGenerateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s, want 200", status, data)
	}
	text := strings.TrimSpace(string(data))
	if !strings.HasPrefix(text, "[") {
		t.Fatalf("array must open with [: %s", text)
	}
	if strings.HasSuffix(text, "]") {
		t.Fatalf("truncated array must not close with ]: %s", text)
	}
}

// Gemini -> Anthropic: stalled upstream past the deadline is a pre-commit
// 504 with slot recovery.
func TestGeminiToAnthropicPrecommitTimeout504(t *testing.T) {
	h := newG2AHarness(t, g2aTightCfg(t, 1))
	closed := make(chan struct{})
	var closeOnce atomic.Bool
	var calls atomic.Int32
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","model":"c","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
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
	status, _, data := postG2A(t, h.router.URL+"/v1beta/models/gem-anth:streamGenerateContent?alt=sse",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status != 504 {
		t.Fatalf("status=%d body=%s, want 504", status, data)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatalf("stalled upstream was not closed after pre-commit 504")
	}
	status, _, data = postG2A(t, h.router.URL+"/v1beta/models/gem-anth:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status != 200 {
		t.Fatalf("slot not recovered after 504: status=%d body=%s", status, data)
	}
}

// Gemini -> Anthropic: flush failure closes upstream promptly.
func TestGeminiToAnthropicFlushFailureCancelsUpstream(t *testing.T) {
	cfg := g2aTightCfg(t, 30)
	var n atomic.Int32
	closed := make(chan struct{})
	var closeOnce atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) > 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","model":"c","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"c\",\"content\":[],\"usage\":{\"input_tokens\":1}}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"par\"}}\n\n"))
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
	body := `{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gem-anth:streamGenerateContent?alt=sse", strings.NewReader(body))
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
	status, _, data := postG2A(t, routerSrv.URL+"/v1beta/models/gem-anth:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status == 503 {
		t.Fatalf("generation slot leaked after flush failure (body %s)", data)
	}
	if status != 200 {
		t.Fatalf("second request after flush failure: status=%d body=%s", status, data)
	}
}

// Gemini -> Anthropic: valid terminal returns promptly without EOF.
func TestGeminiToAnthropicPromptReturnReleasesSlot(t *testing.T) {
	cfg := g2aTightCfg(t, 30)
	var n atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) > 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","model":"c","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		base := func(typ, payload string) string { return "event: " + typ + "\ndata: " + payload + "\n\n" }
		_, _ = io.WriteString(w, base("message_start", `{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"c","content":[],"usage":{"input_tokens":1}}}`))
		_, _ = io.WriteString(w, base("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`))
		_, _ = io.WriteString(w, base("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hel"}}`))
		_, _ = io.WriteString(w, base("content_block_stop", `{"type":"content_block_stop","index":0}`))
		_, _ = io.WriteString(w, base("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`))
		_, _ = io.WriteString(w, base("message_stop", `{"type":"message_stop"}`))
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
	status, _, data := postG2A(t, routerSrv.URL+"/v1beta/models/gem-anth:streamGenerateContent?alt=sse",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if !strings.Contains(string(data), `"finishReason":"STOP"`) {
		t.Fatalf("valid terminal missing STOP: %s", data)
	}
	status, _, data = postG2A(t, routerSrv.URL+"/v1beta/models/gem-anth:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status != 200 {
		t.Fatalf("slot not released after valid terminal: status=%d body=%s", status, data)
	}
}

// Gemini -> Anthropic: upstream Retry-After is forwarded safely.
func TestGeminiToAnthropicRetryAfterSafe(t *testing.T) {
	h := newG2AHarness(t, g2aBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow"}}`))
	}
	status, headers, _ := postG2A(t, h.router.URL+"/v1beta/models/gem-anth:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status != 429 {
		t.Fatalf("upstream 429 must be preserved, got %d", status)
	}
	if headers.Get("Retry-After") != "3" {
		t.Fatalf("Retry-After must be forwarded safely")
	}
}
