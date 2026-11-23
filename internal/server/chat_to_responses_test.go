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

	"github.com/arhammahajan/router/internal/config"
)

// responsesBridgeCfg builds a router with Chat ingress models routed to a
// Responses upstream (strict + lossy) and Responses ingress models routed to
// a Chat upstream (strict + lossy), plus native controls.
func responsesBridgeCfg(t *testing.T) *config.Config {
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

[providers.respup]
dialect = "openai-responses"
base_url = "http://127.0.0.1:9"
api_key = "resp-secret"

[providers.lossyresp]
dialect = "openai-responses"
base_url = "http://127.0.0.1:9"
api_key = "resp-secret"
compatibility = "lossy"

[providers.chatup]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
api_key = "upstream-secret"

[providers.lossychat]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
api_key = "upstream-secret"
compatibility = "lossy"

[[models]]
name = "bridge-chat"
provider = "respup"
model = "gpt-4.1"

[[models]]
name = "bridge-chat-lossy"
provider = "lossyresp"
model = "gpt-4.1"

[[models]]
name = "bridge-resp"
provider = "chatup"
model = "gpt-4o-mini"

[[models]]
name = "bridge-resp-lossy"
provider = "lossychat"
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

type responsesBridgeHarness struct {
	upstream *httptest.Server
	router   *httptest.Server
	mu       sync.Mutex
	lastPath string
	lastBody []byte
	lastHdr  http.Header
	calls    int
	handler  func(w http.ResponseWriter, r *http.Request)
}

func newResponsesBridgeHarness(t *testing.T, cfg *config.Config) *responsesBridgeHarness {
	t.Helper()
	h := &responsesBridgeHarness{}
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_up","object":"response","created_at":1,"status":"completed","model":"gpt-4.1","output":[{"id":"msg_up","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hello","annotations":[]}]}],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}`))
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

func postResponsesBridge(t *testing.T, url, body string, headers map[string]string) (int, http.Header, []byte) {
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

func (h *responsesBridgeHarness) upstreamBody(t *testing.T) map[string]any {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	var v map[string]any
	if err := json.Unmarshal(h.lastBody, &v); err != nil {
		t.Fatalf("upstream body not JSON: %v", err)
	}
	return v
}

// Chat ingress -> Responses upstream: JSON text translates and restores the
// public model downstream while sending the upstream ID upstream.
func TestChatToResponsesJSONText(t *testing.T) {
	h := newResponsesBridgeHarness(t, responsesBridgeCfg(t))
	status, _, data := postResponsesBridge(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-chat","messages":[{"role":"user","content":"Hi"}],"max_tokens":16,"temperature":0.5}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	choices, _ := v["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices=%v", v["choices"])
	}
	msg, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "hello" {
		t.Fatalf("content=%v", msg["content"])
	}
	up := h.upstreamBody(t)
	if up["model"] != "gpt-4.1" {
		t.Fatalf("upstream model=%v", up["model"])
	}
	if up["store"] != false {
		t.Fatalf("upstream store=%v, want explicit false", up["store"])
	}
	if up["max_output_tokens"] != float64(16) {
		t.Fatalf("upstream max_output_tokens=%v", up["max_output_tokens"])
	}
}

// Chat ingress -> Responses upstream: strict mode refuses a diagnosed loss
// before any upstream I/O.
func TestChatToResponsesStrictLoss(t *testing.T) {
	h := newResponsesBridgeHarness(t, responsesBridgeCfg(t))
	status, _, data := postResponsesBridge(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-chat","messages":[{"role":"user","content":"Hi"}],"seed":7}`, nil)
	if status != 400 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if !strings.Contains(string(data), "translation_loss") {
		t.Fatalf("body lacks translation_loss: %s", data)
	}
	h.mu.Lock()
	calls := h.calls
	h.mu.Unlock()
	if calls != 0 {
		t.Fatalf("upstream calls=%d, want 0 (pre-I/O refusal)", calls)
	}
}

// Chat ingress -> Responses upstream: lossy mode drops the diagnosed loss
// with a loss header instead of refusing.
func TestChatToResponsesLossyDrop(t *testing.T) {
	h := newResponsesBridgeHarness(t, responsesBridgeCfg(t))
	status, headers, _ := postResponsesBridge(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-chat-lossy","messages":[{"role":"user","content":"Hi"}],"seed":7}`, nil)
	if status != 200 {
		t.Fatalf("status=%d, want 200", status)
	}
	if headers.Get("X-Model-Router-Loss") == "" {
		t.Fatalf("lossy must carry loss header")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if strings.Contains(string(h.lastBody), "seed") {
		t.Fatalf("lossy must not forward seed: %s", h.lastBody)
	}
}

// Chat ingress -> Responses upstream: client credentials never become
// upstream credentials.
func TestChatToResponsesCredentialIsolation(t *testing.T) {
	h := newResponsesBridgeHarness(t, responsesBridgeCfg(t))
	status, _, _ := postResponsesBridge(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-chat","messages":[{"role":"user","content":"Hi"}]}`,
		map[string]string{"X-Api-Key": "client-key", "Authorization": "Bearer resp-secret"})
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	h.mu.Lock()
	hdr := h.lastHdr
	h.mu.Unlock()
	if got := hdr.Get("Authorization"); got != "Bearer resp-secret" {
		t.Fatalf("upstream Authorization=%q", got)
	}
	if got := hdr.Get("X-Api-Key"); got != "" {
		t.Fatalf("client X-Api-Key leaked upstream: %q", got)
	}
}

// Chat ingress -> Responses upstream: SSE text terminates with [DONE] and a
// terminal finish reason.
func TestChatToResponsesSSEText(t *testing.T) {
	h := newResponsesBridgeHarness(t, responsesBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		frames := []string{
			"event: response.created\ndata: {\"type\": \"response.created\", \"response\": {\"id\": \"resp_up\", \"object\": \"response\", \"created_at\": 1, \"status\": \"in_progress\", \"model\": \"gpt-4.1\", \"output\": []}, \"sequence_number\": 0}\n\n",
			"event: response.in_progress\ndata: {\"type\": \"response.in_progress\", \"response\": {\"id\": \"resp_up\", \"object\": \"response\", \"created_at\": 1, \"status\": \"in_progress\", \"model\": \"gpt-4.1\", \"output\": []}, \"sequence_number\": 1}\n\n",
			"event: response.output_item.added\ndata: {\"type\": \"response.output_item.added\", \"output_index\": 0, \"item\": {\"id\": \"msg_up\", \"type\": \"message\", \"status\": \"in_progress\"}, \"sequence_number\": 2}\n\n",
			"event: response.content_part.added\ndata: {\"type\": \"response.content_part.added\", \"item_id\": \"msg_up\", \"output_index\": 0, \"content_index\": 0, \"part\": {\"type\": \"output_text\", \"text\": \"\"}, \"sequence_number\": 3}\n\n",
			"event: response.output_text.delta\ndata: {\"type\": \"response.output_text.delta\", \"item_id\": \"msg_up\", \"output_index\": 0, \"content_index\": 0, \"delta\": \"hello\", \"sequence_number\": 4}\n\n",
			"event: response.output_text.done\ndata: {\"type\": \"response.output_text.done\", \"item_id\": \"msg_up\", \"output_index\": 0, \"content_index\": 0, \"text\": \"hello\", \"sequence_number\": 5}\n\n",
			"event: response.content_part.done\ndata: {\"type\": \"response.content_part.done\", \"item_id\": \"msg_up\", \"output_index\": 0, \"content_index\": 0, \"part\": {\"type\": \"output_text\", \"text\": \"hello\"}, \"sequence_number\": 6}\n\n",
			"event: response.output_item.done\ndata: {\"type\": \"response.output_item.done\", \"output_index\": 0, \"item\": {\"id\": \"msg_up\", \"type\": \"message\", \"status\": \"completed\", \"role\": \"assistant\", \"content\": [{\"type\": \"output_text\", \"text\": \"hello\", \"annotations\": []}]}, \"sequence_number\": 7}\n\n",
			"event: response.completed\ndata: {\"type\": \"response.completed\", \"response\": {\"id\": \"resp_up\", \"object\": \"response\", \"created_at\": 1, \"status\": \"completed\", \"model\": \"gpt-4.1\", \"output\": [{\"id\": \"msg_up\", \"type\": \"message\", \"status\": \"completed\", \"role\": \"assistant\", \"content\": [{\"type\": \"output_text\", \"text\": \"hello\", \"annotations\": []}]}], \"usage\": {\"input_tokens\": 7, \"output_tokens\": 3, \"total_tokens\": 10}}, \"sequence_number\": 8}\n\n",
		}
		for _, f := range frames {
			_, _ = io.WriteString(w, f)
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, _, data := postResponsesBridge(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-chat","messages":[{"role":"user","content":"Hi"}],"stream":true}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	text := string(data)
	if !strings.Contains(text, "data: [DONE]") {
		t.Fatalf("missing terminal [DONE]: %s", text)
	}
	if !strings.Contains(text, `"finish_reason":"stop"`) {
		t.Fatalf("missing terminal finish_reason: %s", text)
	}
	if !strings.Contains(text, `"content":"hello"`) {
		t.Fatalf("missing delta text: %s", text)
	}
}

// responsesBridgeTightCfg is a single-slot bridge config with a short
// absolute deadline, for precommit-timeout and flush-failure tests where the
// slot must be proven recovered. Shared by both bridge test files (same
// package).
func responsesBridgeTightCfg(t *testing.T, deadlineSecs float64) *config.Config {
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

[providers.respup]
dialect = "openai-responses"
base_url = "http://127.0.0.1:9"
api_key = "resp-secret"

[providers.chatup]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
api_key = "upstream-secret"

[[models]]
name = "bridge-chat"
provider = "respup"
model = "gpt-4.1"

[[models]]
name = "bridge-resp"
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

// Chat ingress -> Responses upstream: a stalled upstream body past the
// absolute deadline is a pre-commit 504 (never an empty 200), the upstream
// body is closed promptly, and the generation slot is recovered.
func TestChatToResponsesPrecommitTimeout504(t *testing.T) {
	h := newResponsesBridgeHarness(t, responsesBridgeTightCfg(t, 1))
	closed := make(chan struct{})
	var closeOnce sync.Once
	var calls atomic.Int32
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp_up","object":"response","created_at":1,"status":"completed","model":"gpt-4.1","output":[{"id":"msg_up","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hello","annotations":[]}]}],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}`))
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
	status, _, data := postResponsesBridge(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-chat","messages":[{"role":"user","content":"Hi"}],"stream":true}`, nil)
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
	status, _, data = postResponsesBridge(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-chat","messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 200 {
		t.Fatalf("slot not recovered after 504: status=%d body=%s", status, data)
	}
}

// Chat ingress -> Responses upstream: a downstream flush failure cancels the
// upstream body promptly and releases the generation slot.
func TestChatToResponsesFlushFailureCancelsUpstream(t *testing.T) {
	cfg := responsesBridgeTightCfg(t, 30)
	var n atomic.Int32
	closed := make(chan struct{})
	var closeOnce sync.Once
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) > 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp_up","object":"response","created_at":1,"status":"completed","model":"gpt-4.1","output":[{"id":"msg_up","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hello","annotations":[]}]}],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		// A committing prefix: the role chunk alone emits nothing, so include
		// a content delta that forces the translator to commit downstream.
		// The body is then held open; the flush failure must still terminate
		// promptly via the synchronous upstream close.
		_, _ = w.Write([]byte("event: response.created\ndata: {\"type\": \"response.created\", \"response\": {\"id\": \"resp_up\", \"object\": \"response\", \"created_at\": 1, \"status\": \"in_progress\", \"model\": \"gpt-4.1\", \"output\": []}, \"sequence_number\": 0}\n\n" +
			"event: response.in_progress\ndata: {\"type\": \"response.in_progress\", \"response\": {\"id\": \"resp_up\", \"object\": \"response\", \"created_at\": 1, \"status\": \"in_progress\", \"model\": \"gpt-4.1\", \"output\": []}, \"sequence_number\": 1}\n\n" +
			"event: response.output_item.added\ndata: {\"type\": \"response.output_item.added\", \"output_index\": 0, \"item\": {\"id\": \"msg_up\", \"type\": \"message\", \"status\": \"in_progress\"}, \"sequence_number\": 2}\n\n" +
			"event: response.content_part.added\ndata: {\"type\": \"response.content_part.added\", \"item_id\": \"msg_up\", \"output_index\": 0, \"content_index\": 0, \"part\": {\"type\": \"output_text\", \"text\": \"\"}, \"sequence_number\": 3}\n\n" +
			"event: response.output_text.delta\ndata: {\"type\": \"response.output_text.delta\", \"item_id\": \"msg_up\", \"output_index\": 0, \"content_index\": 0, \"delta\": \"par\", \"sequence_number\": 4}\n\n"))
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
	body := `{"model":"bridge-chat","messages":[{"role":"user","content":"Hi"}],"stream":true}`
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
	status, _, data := postResponsesBridge(t, routerSrv.URL+"/v1/chat/completions",
		`{"model":"bridge-chat","messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status == 503 {
		t.Fatalf("generation slot leaked after flush failure (body %s)", data)
	}
	if status != 200 {
		t.Fatalf("second request after flush failure: status=%d body=%s", status, data)
	}
}

// Chat ingress -> Responses upstream: an unsupported event after commitment
// keeps 200 and terminates silently — no [DONE], no fabricated finish.
func TestChatToResponsesLateUnsupportedSilent(t *testing.T) {
	h := newResponsesBridgeHarness(t, responsesBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		frames := []string{
			"event: response.created\ndata: {\"type\": \"response.created\", \"response\": {\"id\": \"resp_up\", \"object\": \"response\", \"created_at\": 1, \"status\": \"in_progress\", \"model\": \"gpt-4.1\", \"output\": []}, \"sequence_number\": 0}\n\n",
			"event: response.in_progress\ndata: {\"type\": \"response.in_progress\", \"response\": {\"id\": \"resp_up\", \"object\": \"response\", \"created_at\": 1, \"status\": \"in_progress\", \"model\": \"gpt-4.1\", \"output\": []}, \"sequence_number\": 1}\n\n",
			"event: response.output_item.added\ndata: {\"type\": \"response.output_item.added\", \"output_index\": 0, \"item\": {\"id\": \"msg_up\", \"type\": \"message\", \"status\": \"in_progress\"}, \"sequence_number\": 2}\n\n",
			"event: response.content_part.added\ndata: {\"type\": \"response.content_part.added\", \"item_id\": \"msg_up\", \"output_index\": 0, \"content_index\": 0, \"part\": {\"type\": \"output_text\", \"text\": \"\"}, \"sequence_number\": 3}\n\n",
			"event: response.output_text.delta\ndata: {\"type\": \"response.output_text.delta\", \"item_id\": \"msg_up\", \"output_index\": 0, \"content_index\": 0, \"delta\": \"par\", \"sequence_number\": 4}\n\n",
			"event: response.mystery\ndata: {\"type\": \"response.mystery\", \"sequence_number\": 5}\n\n",
		}
		for _, f := range frames {
			_, _ = io.WriteString(w, f)
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, _, data := postResponsesBridge(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-chat","messages":[{"role":"user","content":"Hi"}],"stream":true}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s, want 200 (headers committed)", status, data)
	}
	text := string(data)
	if !strings.Contains(text, `"content":"par"`) {
		t.Fatalf("committed delta missing before failure: %s", text)
	}
	if strings.Contains(text, "data: [DONE]") {
		t.Fatalf("post-commit failure must not emit [DONE]: %s", text)
	}
	if strings.Contains(text, `"finish_reason":"stop"`) {
		t.Fatalf("post-commit failure must not fabricate finish: %s", text)
	}
}

// Chat ingress -> Responses upstream: a valid terminal (finish + terminal
// usage + [DONE]) returns promptly without waiting for EOF, and the
// generation slot is released even while the upstream body stays open.
func TestChatToResponsesPromptReturnReleasesSlot(t *testing.T) {
	cfg := responsesBridgeTightCfg(t, 30)
	var n atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) > 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp_up","object":"response","created_at":1,"status":"completed","model":"gpt-4.1","output":[{"id":"msg_up","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hello","annotations":[]}]}],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		frames := []string{
			"event: response.created\ndata: {\"type\": \"response.created\", \"response\": {\"id\": \"resp_up\", \"object\": \"response\", \"created_at\": 1, \"status\": \"in_progress\", \"model\": \"gpt-4.1\", \"output\": []}, \"sequence_number\": 0}\n\n",
			"event: response.in_progress\ndata: {\"type\": \"response.in_progress\", \"response\": {\"id\": \"resp_up\", \"object\": \"response\", \"created_at\": 1, \"status\": \"in_progress\", \"model\": \"gpt-4.1\", \"output\": []}, \"sequence_number\": 1}\n\n",
			"event: response.output_item.added\ndata: {\"type\": \"response.output_item.added\", \"output_index\": 0, \"item\": {\"id\": \"msg_up\", \"type\": \"message\", \"status\": \"in_progress\"}, \"sequence_number\": 2}\n\n",
			"event: response.content_part.added\ndata: {\"type\": \"response.content_part.added\", \"item_id\": \"msg_up\", \"output_index\": 0, \"content_index\": 0, \"part\": {\"type\": \"output_text\", \"text\": \"\"}, \"sequence_number\": 3}\n\n",
			"event: response.output_text.delta\ndata: {\"type\": \"response.output_text.delta\", \"item_id\": \"msg_up\", \"output_index\": 0, \"content_index\": 0, \"delta\": \"hello\", \"sequence_number\": 4}\n\n",
			"event: response.output_text.done\ndata: {\"type\": \"response.output_text.done\", \"item_id\": \"msg_up\", \"output_index\": 0, \"content_index\": 0, \"text\": \"hello\", \"sequence_number\": 5}\n\n",
			"event: response.content_part.done\ndata: {\"type\": \"response.content_part.done\", \"item_id\": \"msg_up\", \"output_index\": 0, \"content_index\": 0, \"part\": {\"type\": \"output_text\", \"text\": \"hello\"}, \"sequence_number\": 6}\n\n",
			"event: response.output_item.done\ndata: {\"type\": \"response.output_item.done\", \"output_index\": 0, \"item\": {\"id\": \"msg_up\", \"type\": \"message\", \"status\": \"completed\", \"role\": \"assistant\", \"content\": [{\"type\": \"output_text\", \"text\": \"hello\", \"annotations\": []}]}, \"sequence_number\": 7}\n\n",
			"event: response.completed\ndata: {\"type\": \"response.completed\", \"response\": {\"id\": \"resp_up\", \"object\": \"response\", \"created_at\": 1, \"status\": \"completed\", \"model\": \"gpt-4.1\", \"output\": [{\"id\": \"msg_up\", \"type\": \"message\", \"status\": \"completed\", \"role\": \"assistant\", \"content\": [{\"type\": \"output_text\", \"text\": \"hello\", \"annotations\": []}]}], \"usage\": {\"input_tokens\": 7, \"output_tokens\": 3, \"total_tokens\": 10}}, \"sequence_number\": 8}\n\n",
		}
		for _, f := range frames {
			_, _ = io.WriteString(w, f)
		}
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
	status, _, data := postResponsesBridge(t, routerSrv.URL+"/v1/chat/completions",
		`{"model":"bridge-chat","messages":[{"role":"user","content":"Hi"}],"stream":true}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if !strings.Contains(string(data), "data: [DONE]") {
		t.Fatalf("valid terminal must end with [DONE]: %s", data)
	}
	status, _, data = postResponsesBridge(t, routerSrv.URL+"/v1/chat/completions",
		`{"model":"bridge-chat","messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 200 {
		t.Fatalf("slot not released after valid terminal without EOF: status=%d body=%s", status, data)
	}
}

// Chat ingress -> Responses upstream: model text containing framing-like
// tokens is payload, never framing. Exactly one terminal [DONE] is emitted.
func TestChatToResponsesModelTextIsNotFraming(t *testing.T) {
	h := newResponsesBridgeHarness(t, responsesBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		frames := []string{
			"event: response.created\ndata: {\"type\": \"response.created\", \"response\": {\"id\": \"resp_up\", \"object\": \"response\", \"created_at\": 1, \"status\": \"in_progress\", \"model\": \"gpt-4.1\", \"output\": []}, \"sequence_number\": 0}\n\n",
			"event: response.in_progress\ndata: {\"type\": \"response.in_progress\", \"response\": {\"id\": \"resp_up\", \"object\": \"response\", \"created_at\": 1, \"status\": \"in_progress\", \"model\": \"gpt-4.1\", \"output\": []}, \"sequence_number\": 1}\n\n",
			"event: response.output_item.added\ndata: {\"type\": \"response.output_item.added\", \"output_index\": 0, \"item\": {\"id\": \"msg_up\", \"type\": \"message\", \"status\": \"in_progress\"}, \"sequence_number\": 2}\n\n",
			"event: response.content_part.added\ndata: {\"type\": \"response.content_part.added\", \"item_id\": \"msg_up\", \"output_index\": 0, \"content_index\": 0, \"part\": {\"type\": \"output_text\", \"text\": \"\"}, \"sequence_number\": 3}\n\n",
			"event: response.output_text.delta\ndata: {\"type\": \"response.output_text.delta\", \"item_id\": \"msg_up\", \"output_index\": 0, \"content_index\": 0, \"delta\": \"say data: [DONE] aloud\", \"sequence_number\": 4}\n\n",
			"event: response.output_text.done\ndata: {\"type\": \"response.output_text.done\", \"item_id\": \"msg_up\", \"output_index\": 0, \"content_index\": 0, \"text\": \"say data: [DONE] aloud\", \"sequence_number\": 5}\n\n",
			"event: response.content_part.done\ndata: {\"type\": \"response.content_part.done\", \"item_id\": \"msg_up\", \"output_index\": 0, \"content_index\": 0, \"part\": {\"type\": \"output_text\", \"text\": \"say data: [DONE] aloud\"}, \"sequence_number\": 6}\n\n",
			"event: response.output_item.done\ndata: {\"type\": \"response.output_item.done\", \"output_index\": 0, \"item\": {\"id\": \"msg_up\", \"type\": \"message\", \"status\": \"completed\", \"role\": \"assistant\", \"content\": [{\"type\": \"output_text\", \"text\": \"say data: [DONE] aloud\", \"annotations\": []}]}, \"sequence_number\": 7}\n\n",
			"event: response.completed\ndata: {\"type\": \"response.completed\", \"response\": {\"id\": \"resp_up\", \"object\": \"response\", \"created_at\": 1, \"status\": \"completed\", \"model\": \"gpt-4.1\", \"output\": [{\"id\": \"msg_up\", \"type\": \"message\", \"status\": \"completed\", \"role\": \"assistant\", \"content\": [{\"type\": \"output_text\", \"text\": \"say data: [DONE] aloud\", \"annotations\": []}]}], \"usage\": {\"input_tokens\": 7, \"output_tokens\": 3, \"total_tokens\": 10}}, \"sequence_number\": 8}\n\n",
		}
		for _, f := range frames {
			_, _ = io.WriteString(w, f)
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, _, data := postResponsesBridge(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-chat","messages":[{"role":"user","content":"Hi"}],"stream":true}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	text := string(data)
	// The payload occurrence lives inside a data JSON string, never as its
	// own framed block: exactly one block may equal the terminal marker.
	terminal := 0
	for _, block := range strings.Split(text, "\n\n") {
		if strings.TrimSpace(block) == "data: [DONE]" {
			terminal++
		}
	}
	if terminal != 1 {
		t.Fatalf("terminal [DONE] block emitted %d times, want exactly 1: %s", terminal, text)
	}
	if !strings.Contains(text, `"finish_reason":"stop"`) {
		t.Fatalf("valid terminal missing finish_reason: %s", text)
	}
}

// Chat ingress -> Responses upstream: pre-commit invalid stream is an
// ordinary 502, never an empty 200.
func TestChatToResponsesEarlyInvalidStream(t *testing.T) {
	h := newResponsesBridgeHarness(t, responsesBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"mystery\":1}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, _, data := postResponsesBridge(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-chat","messages":[{"role":"user","content":"Hi"}],"stream":true}`, nil)
	if status != 502 {
		t.Fatalf("status=%d body=%s, want 502", status, data)
	}
	if strings.Contains(string(data), "data: [DONE]") {
		t.Fatalf("pre-commit failure must not emit [DONE]: %s", data)
	}
}

// Chat ingress -> Responses upstream: post-commit truncation keeps 200 and
// terminates without [DONE] or a fabricated finish.
func TestChatToResponsesMidstreamTruncation(t *testing.T) {
	h := newResponsesBridgeHarness(t, responsesBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Valid prefix (created/in_progress/added/part-added/delta),
		// then EOF without terminal events: truncation after commit.
		valid := []string{
			"event: response.created\ndata: {\"type\": \"response.created\", \"response\": {\"id\": \"resp_up\", \"object\": \"response\", \"created_at\": 1, \"status\": \"in_progress\", \"model\": \"gpt-4.1\", \"output\": []}, \"sequence_number\": 0}\n\n",
			"event: response.in_progress\ndata: {\"type\": \"response.in_progress\", \"response\": {\"id\": \"resp_up\", \"object\": \"response\", \"created_at\": 1, \"status\": \"in_progress\", \"model\": \"gpt-4.1\", \"output\": []}, \"sequence_number\": 1}\n\n",
			"event: response.output_item.added\ndata: {\"type\": \"response.output_item.added\", \"output_index\": 0, \"item\": {\"id\": \"msg_up\", \"type\": \"message\", \"status\": \"in_progress\"}, \"sequence_number\": 2}\n\n",
			"event: response.content_part.added\ndata: {\"type\": \"response.content_part.added\", \"item_id\": \"msg_up\", \"output_index\": 0, \"content_index\": 0, \"part\": {\"type\": \"output_text\", \"text\": \"\"}, \"sequence_number\": 3}\n\n",
			"event: response.output_text.delta\ndata: {\"type\": \"response.output_text.delta\", \"item_id\": \"msg_up\", \"output_index\": 0, \"content_index\": 0, \"delta\": \"par\", \"sequence_number\": 4}\n\n",
		}
		for _, f := range valid {
			_, _ = io.WriteString(w, f)
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Truncate: close without terminal events.
	}
	status, _, data := postResponsesBridge(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-chat","messages":[{"role":"user","content":"Hi"}],"stream":true}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s, want 200 (headers committed)", status, data)
	}
	text := string(data)
	if strings.Contains(text, "data: [DONE]") {
		t.Fatalf("truncated stream must not contain [DONE]: %s", text)
	}
	if strings.Contains(text, `"finish_reason":"stop"`) {
		t.Fatalf("truncated stream must not fabricate finish: %s", text)
	}
	if !strings.Contains(text, `"content":"par"`) {
		t.Fatalf("committed delta missing before EOF: %s", text)
	}
}
