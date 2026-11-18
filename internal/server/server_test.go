package server

import (
	"bytes"
	"compress/gzip"
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

	"github.com/arhammahajan/model-router/internal/config"
)

func TestMain(m *testing.M) {
	os.Unsetenv("MODEL_ROUTER_API_KEY")
	os.Unsetenv("MODEL_ROUTER_API_KEYS")
	testHome, err := os.MkdirTemp("", "server-test-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", testHome)
	os.Setenv("XDG_CONFIG_HOME", testHome)
	code := m.Run()
	os.RemoveAll(testHome)
	os.Exit(code)
}

// harness spins up a fake upstream plus the router under test.
type harness struct {
	upstream *httptest.Server
	router   *httptest.Server
	mu       sync.Mutex
	// last request seen by the fake upstream
	lastPath    string
	lastBody    []byte
	lastHeaders http.Header
	// scripted upstream behavior
	handler func(w http.ResponseWriter, r *http.Request)
}

func writeRouterCfg(t *testing.T, upstreamURL, lossy string) string {
	t.Helper()
	body := `
[server]
auth_mode = "none"
allow_dynamic_routes = false
max_generations = 8

[retry]
max_attempts = 1

[providers.anth]
dialect = "anthropic"
base_url = "` + upstreamURL + `"
api_key = "upstream-secret"

[providers.local]
dialect = "openai-chat"
base_url = "` + upstreamURL + `"
api_key = "local-secret"

[providers.lossyup]
dialect = "anthropic"
base_url = "` + upstreamURL + `"
api_key = "upstream-secret"
compatibility = "lossy"

[[models]]
name = "claude-haiku"
provider = "anth"
model = "claude-haiku-4-5"

[[models]]
name = "native-llama"
provider = "local"
model = "llama3.1"

[[models]]
name = "llama3.1"
provider = "local"
model = "llama3.1"

[[models]]
name = "lossy-model"
provider = "lossyup"
model = "claude-haiku-4-5"
`
	_ = lossy
	// The lossy-model entry uses the anth provider; lossy coverage uses a
	// dedicated config written by TestLossy below.
	dir := t.TempDir()
	p := filepath.Join(dir, "router.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func newHarness(t *testing.T, cfgPath string) *harness {
	t.Helper()
	h := &harness{}
	h.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.lastPath = r.URL.Path
		h.lastBody = body
		h.lastHeaders = r.Header.Clone()
		fn := h.handler
		h.mu.Unlock()
		fn(w, r)
	}))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m",` +
			`"content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn",` +
			`"usage":{"input_tokens":7,"output_tokens":3}}`))
	}
	t.Cleanup(h.upstream.Close)
	// Point the router config at the fake upstream by rewriting base_url.
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "router.toml")
	placeholder := ""
	_ = placeholder
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = raw
	for _, prov := range cfg.Providers {
		prov.BaseURL = h.upstream.URL
	}
	_ = p
	srv := New(cfg)
	router := httptest.NewServer(srv.Handler())
	t.Cleanup(router.Close)
	h.router = router
	return h
}

func post(t *testing.T, url, body string, headers map[string]string) (int, http.Header, []byte) {
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

func TestTranslatedJSON(t *testing.T) {
	h := newHarness(t, writeRouterCfg(t, "http://placeholder", ""))
	status, _, data := post(t, h.router.URL+"/v1/chat/completions",
		`{"model":"claude-haiku","messages":[{"role":"user","content":"Hi"}],"max_tokens":16}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	var out struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
			Total        int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out.Model != "claude-haiku" || out.Choices[0].Message.Content != "hello" {
		t.Fatalf("bad translation: %s", data)
	}
	if out.Usage.Total != 10 {
		t.Fatalf("bad usage: %s", data)
	}
	// Upstream wire assertions.
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lastPath != "/v1/messages" {
		t.Fatalf("upstream path: %s", h.lastPath)
	}
	var wire map[string]any
	if err := json.Unmarshal(h.lastBody, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["model"] != "claude-haiku-4-5" {
		t.Fatalf("upstream model not replaced: %v", wire["model"])
	}
	if wire["max_tokens"] != float64(16) {
		t.Fatalf("max_tokens not carried: %v", wire["max_tokens"])
	}
	if h.lastHeaders.Get("X-Api-Key") != "upstream-secret" {
		t.Fatalf("upstream credential missing: %v", h.lastHeaders)
	}
	if h.lastHeaders.Get("Anthropic-Version") == "" {
		t.Fatalf("anthropic-version missing")
	}
	if h.lastHeaders.Get("Authorization") != "" {
		t.Fatalf("client auth must never become upstream auth")
	}
}

func TestStrictRejectsLossyField(t *testing.T) {
	h := newHarness(t, writeRouterCfg(t, "http://placeholder", ""))
	status, _, data := post(t, h.router.URL+"/v1/chat/completions",
		`{"model":"claude-haiku","messages":[{"role":"user","content":"Hi"}],"seed":7}`, nil)
	if status != 400 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if !strings.Contains(string(data), "translation_loss") {
		t.Fatalf("expected translation_loss code: %s", data)
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	h := newHarness(t, writeRouterCfg(t, "http://placeholder", ""))
	status, _, data := post(t, h.router.URL+"/v1/chat/completions",
		`{"model":"claude-haiku","messages":[],"bogus":1}`, nil)
	if status != 400 || !strings.Contains(string(data), "invalid_generation_parameter") {
		t.Fatalf("status=%d body=%s", status, data)
	}
}

func TestNativePreservesUnknownFields(t *testing.T) {
	h := newHarness(t, writeRouterCfg(t, "http://placeholder", ""))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,` +
			`"model":"llama3.1","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}
	reqBody := `{"model":"native-llama","messages":[{"role":"user","content":"Hi"}],"mystery_field":{"nested":[1,2,3]},"future_flag":true}`
	// native-llama's public name differs from the upstream ID, so the router
	// rewrites only the model field; unknown fields must survive. Use an
	// alias-equal model instead: post with the upstream ID via dynamic route
	// disabled... so instead assert unknown fields survive the rewrite.
	status, _, data := post(t, h.router.URL+"/v1/chat/completions", reqBody, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var wire map[string]any
	if err := json.Unmarshal(h.lastBody, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["model"] != "llama3.1" {
		t.Fatalf("model not replaced: %v", wire["model"])
	}
	mystery, ok := wire["mystery_field"].(map[string]any)
	if !ok || mystery["nested"] == nil {
		t.Fatalf("unknown fields lost: %s", h.lastBody)
	}
	if wire["future_flag"] != true {
		t.Fatalf("unknown scalar lost: %s", h.lastBody)
	}
	if h.lastHeaders.Get("Authorization") != "Bearer local-secret" {
		t.Fatalf("native upstream auth wrong: %v", h.lastHeaders)
	}
}

func TestNativeAliasRewritePreservesLargeNumbers(t *testing.T) {
	h := newHarness(t, writeRouterCfg(t, "http://placeholder", ""))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":1,` +
			`"model":"llama3.1","choices":[]}`))
	}
	// 2^53+1 is not exactly representable as float64; a map[string]any
	// rewrite would silently change it to 9007199254740992.
	reqBody := `{"model":"native-llama","messages":[{"role":"user","content":"Hi"}],` +
		`"mystery_field":{"nested":[9007199254740993]}}`
	status, _, data := post(t, h.router.URL+"/v1/chat/completions", reqBody, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !strings.Contains(string(h.lastBody), "9007199254740993") {
		t.Fatalf("large integer not preserved byte-exactly: %s", h.lastBody)
	}
	if strings.Contains(string(h.lastBody), "9007199254740992") {
		t.Fatalf("float64 precision loss in alias rewrite: %s", h.lastBody)
	}
}

func TestNativeByteIdenticalWhenAliasMatches(t *testing.T) {
	h := newHarness(t, writeRouterCfg(t, "http://placeholder", ""))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}
	// Public name equals the upstream ID: the router forwards client bytes
	// untouched, preserving unknown native fields byte-identically.
	reqBody := `{"model":"llama3.1","messages":[{"role":"user","content":"Hi"}],"mystery_field":{"nested":[1,2,3]}}`
	status, _, _ := post(t, h.router.URL+"/v1/chat/completions", reqBody, nil)
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if string(h.lastBody) != reqBody {
		t.Fatalf("native bytes not preserved:\n got %s\nwant %s", h.lastBody, reqBody)
	}
}

func TestLossyDropsWithHeader(t *testing.T) {
	h := newHarness(t, writeRouterCfg(t, "http://placeholder", ""))
	status, headers, data := post(t, h.router.URL+"/v1/chat/completions",
		`{"model":"lossy-model","messages":[{"role":"user","content":"Hi"}],"seed":7}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	loss := headers.Get("X-Model-Router-Loss")
	if loss == "" {
		t.Fatalf("lossy response must carry x-model-router-loss")
	}
	var losses []map[string]string
	if err := json.Unmarshal([]byte(loss), &losses); err != nil || len(losses) == 0 {
		t.Fatalf("bad loss header: %q", loss)
	}
	if losses[0]["field"] != "seed" || losses[0]["action"] != "dropped" {
		t.Fatalf("loss record wrong: %v", losses)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if strings.Contains(string(h.lastBody), "seed") {
		t.Fatalf("dropped field leaked upstream: %s", h.lastBody)
	}
}

func TestTranslatedSSE(t *testing.T) {
	h := newHarness(t, writeRouterCfg(t, "http://placeholder", ""))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for _, f := range []string{
			`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":5}}}` + "\n\n",
			`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n",
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hey"}}` + "\n\n",
			`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}` + "\n\n",
			`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}` + "\n\n",
			`event: message_stop` + "\n" + `data: {"type":"message_stop"}` + "\n\n",
		} {
			_, _ = w.Write([]byte(f))
			flusher.Flush()
		}
	}
	status, _, data := post(t, h.router.URL+"/v1/chat/completions",
		`{"model":"claude-haiku","messages":[{"role":"user","content":"Hi"}],"stream":true,"stream_options":{"include_usage":true}}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	body := string(data)
	if !strings.Contains(body, `"content":"hey"`) || !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("bad downstream SSE:\n%s", body)
	}
	if !strings.Contains(body, `"prompt_tokens":5`) {
		t.Fatalf("usage missing (include_usage):\n%s", body)
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Fatalf("missing terminator:\n%s", body)
	}
}

func TestTruncatedUpstreamSSEHasNoSuccessMarker(t *testing.T) {
	h := newHarness(t, writeRouterCfg(t, "http://placeholder", ""))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start\n" +
			`data: {"type":"message_start","message":{"id":"m"}}` + "\n\n"))
		// EOF without message_stop: a truncated generation.
	}
	status, _, data := post(t, h.router.URL+"/v1/chat/completions",
		`{"model":"claude-haiku","messages":[{"role":"user","content":"Hi"}],"stream":true}`, nil)
	if status != 200 {
		t.Fatalf("committed stream keeps status 200, got %d", status)
	}
	if strings.Contains(string(data), "[DONE]") || strings.Contains(string(data), "finish_reason\":\"stop") {
		t.Fatalf("truncated stream must not carry a success marker:\n%s", data)
	}
}
func TestAuthRequired(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "router.toml")
	os.WriteFile(p, []byte(`
[server]
auth_mode = "required"
api_keys = ["client-secret"]
[providers.anth]
dialect = "anthropic"
base_url = "http://127.0.0.1:9"
[[models]]
name = "m"
provider = "anth"
model = "x"
`), 0o600)
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(cfg).Handler())
	defer srv.Close()
	status, _, _ := post(t, srv.URL+"/v1/chat/completions",
		`{"model":"m","messages":[]}`, nil)
	if status != 401 {
		t.Fatalf("unauthenticated must be 401, got %d", status)
	}
	status, _, _ = post(t, srv.URL+"/v1/chat/completions",
		`{"model":"m","messages":[]}`, map[string]string{"Authorization": "Bearer client-secret"})
	if status == 401 {
		t.Fatalf("authenticated request rejected")
	}
}

func TestRedirectRefused(t *testing.T) {
	h := newHarness(t, writeRouterCfg(t, "http://placeholder", ""))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://example.invalid/evil")
		w.WriteHeader(302)
	}
	status, _, _ := post(t, h.router.URL+"/v1/chat/completions",
		`{"model":"claude-haiku","messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 502 {
		t.Fatalf("redirect must be refused with 502, got %d", status)
	}
}

func TestOverload503(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "router.toml")
	os.WriteFile(p, []byte(`
[server]
auth_mode = "none"
max_connections = 4
max_generations = 1
[providers.anth]
dialect = "anthropic"
base_url = "http://127.0.0.1:9"
[[models]]
name = "m"
provider = "anth"
model = "x"
`), 0o600)
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","model":"x",` +
			`"content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer up.Close()
	for _, prov := range cfg.Providers {
		prov.BaseURL = up.URL
	}
	srv := httptest.NewServer(New(cfg).Handler())
	defer srv.Close()
	first := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"Hi"}]}`))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			first <- -1
			return
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		first <- resp.StatusCode
	}()
	time.Sleep(300 * time.Millisecond) // let the first request take the slot
	status, _, _ := post(t, srv.URL+"/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":"Hi"}]}`, nil)
	close(release)
	if status != 503 {
		t.Fatalf("second concurrent generation must be 503, got %d", status)
	}
	if got := <-first; got != 200 {
		t.Fatalf("first request status=%d", got)
	}
}

func TestTranslatedDropsCrossDialectHeaders(t *testing.T) {
	h := newHarness(t, writeRouterCfg(t, "http://placeholder", ""))
	status, _, data := post(t, h.router.URL+"/v1/chat/completions",
		`{"model":"claude-haiku","messages":[{"role":"user","content":"Hi"}]}`,
		map[string]string{
			"Openai-Beta":     "assistants=v2",
			"Connection":      "X-Private",
			"X-Private":       "s3cr3t",
			"X-Session-Id":    "sess-1",
			"Idempotency-Key": "k-1",
		})
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, k := range []string{"Openai-Beta", "X-Private", "X-Session-Id", "Idempotency-Key", "Connection"} {
		if v := h.lastHeaders.Get(k); v != "" {
			t.Fatalf("inbound %q must not reach Anthropic (got %q)", k, v)
		}
	}
}

func TestTranslatedErrorSanitizedWithRetryAfter(t *testing.T) {
	h := newHarness(t, writeRouterCfg(t, "http://placeholder", ""))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"sk-secret prompt hello"}}`))
	}
	status, headers, data := post(t, h.router.URL+"/v1/chat/completions",
		`{"model":"claude-haiku","messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 429 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if strings.Contains(string(data), "sk-secret") || strings.Contains(string(data), "prompt hello") {
		t.Fatalf("translated error must not echo upstream body: %s", data)
	}
	if headers.Get("Retry-After") != "2" {
		t.Fatalf("Retry-After must be forwarded, got %q", headers.Get("Retry-After"))
	}
}

func TestNativeResponseHeadersFiltered(t *testing.T) {
	h := newHarness(t, writeRouterCfg(t, "http://placeholder", ""))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "sess=1")
		w.Header().Set("Authorization", "Bearer upstream-secret")
		w.Header().Set("Connection", "X-Evil")
		w.Header().Set("X-Evil", "1")
		w.Header().Set("Content-Encoding", "identity")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"llama3.1","choices":[]}`))
	}
	status, headers, _ := post(t, h.router.URL+"/v1/chat/completions",
		`{"model":"llama3.1","messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	for _, k := range []string{"Set-Cookie", "Authorization", "X-Evil", "Content-Encoding"} {
		if v := headers.Get(k); v != "" {
			t.Fatalf("response header %q must not be forwarded (got %q)", k, v)
		}
	}
}

func TestNativeGzipUpstreamDecoded(t *testing.T) {
	h := newHarness(t, writeRouterCfg(t, "http://placeholder", ""))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzipBytes(t, `{"id":"c","object":"chat.completion","created":1,"model":"llama3.1","choices":[]}`)
		_, _ = w.Write(gz)
	}
	status, headers, data := post(t, h.router.URL+"/v1/chat/completions",
		`{"model":"llama3.1","messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if headers.Get("Content-Encoding") != "" {
		t.Fatalf("Content-Encoding must be stripped after decode, got %q", headers.Get("Content-Encoding"))
	}
	if !strings.Contains(string(data), `"model":"llama3.1"`) {
		t.Fatalf("gzip body not decoded: %q", data)
	}
}

func TestNativeUnsupportedEncodingRefused(t *testing.T) {
	h := newHarness(t, writeRouterCfg(t, "http://placeholder", ""))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "br")
		_, _ = w.Write([]byte(`xxx`))
	}
	status, _, _ := post(t, h.router.URL+"/v1/chat/completions",
		`{"model":"llama3.1","messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 502 {
		t.Fatalf("unsupported encoding must be 502, got %d", status)
	}
}

func gzipBytes(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestModelsListingServesRetainedMetadata(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "router.toml")
	os.WriteFile(p, []byte(`
[server]
auth_mode = "none"
[providers.anth]
dialect = "anthropic"
base_url = "http://127.0.0.1:9"
api_key = "x"
[[models]]
name = "m"
provider = "anth"
model = "up-id"
[models.metadata]
display_name = "pretty"
context_window = 200000
`), 0o600)
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(cfg)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data []struct {
			ID       string          `json:"id"`
			Metadata json.RawMessage `json:"metadata"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != 1 {
		t.Fatalf("want 1 model, got %s", rec.Body.String())
	}
	meta := string(out.Data[0].Metadata)
	if !strings.Contains(meta, `"pretty"`) || !strings.Contains(meta, "200000") {
		t.Fatalf("retained metadata lost: %s", meta)
	}
	if strings.Contains(meta, "200000.0") {
		t.Fatalf("metadata float round-trip: %s", meta)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out.Data[0].Metadata, &decoded); err != nil {
		t.Fatal(err)
	}
	if n, ok := decoded["context_window"].(float64); !ok || n != 200000 {
		t.Fatalf("metadata integer not exact: %s", meta)
	}
}

// flushFailWriter simulates a downstream gone mid-stream: writes succeed
// but every flush reports an error via FlushError.
type flushFailWriter struct {
	header http.Header
	status int
}

func (f *flushFailWriter) Header() http.Header {
	if f.header == nil {
		f.header = http.Header{}
	}
	return f.header
}

func (f *flushFailWriter) Write(p []byte) (int, error) { return len(p), nil }

func (f *flushFailWriter) WriteHeader(status int) { f.status = status }

func (f *flushFailWriter) FlushError() error { return errors.New("downstream gone") }

func TestTranslatedFlushFailureClosesUpstreamPromptly(t *testing.T) {
	var clientGone = make(chan struct{})
	var goneOnce sync.Once
	var n atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			_, _ = w.Write([]byte("event: message_start\n" +
				`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":5}}}` + "\n\n"))
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				goneOnce.Do(func() { close(clientGone) })
			case <-time.After(10 * time.Second):
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","model":"x",` +
			`"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",` +
			`"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer up.Close()
	dir := t.TempDir()
	p := filepath.Join(dir, "router.toml")
	os.WriteFile(p, []byte(`
[server]
auth_mode = "none"
max_generations = 1
[retry]
deadline = 10
[providers.anth]
dialect = "anthropic"
base_url = "http://127.0.0.1:9"
api_key = "upstream-secret"
[[models]]
name = "m"
provider = "anth"
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
	body := `{"model":"m","messages":[{"role":"user","content":"Hi"}],"stream":true}`
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
		t.Fatalf("flush failure did not close upstream promptly (provider deadline 10s)")
	}
	select {
	case <-clientGone:
	case <-time.After(5 * time.Second):
		t.Fatalf("upstream was not closed after downstream flush failure")
	}
	// The generation slot must be released: a second request is not 503.
	routerSrv := httptest.NewServer(srv.Handler())
	defer routerSrv.Close()
	status, _, data := post(t, routerSrv.URL+"/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status == 503 {
		t.Fatalf("generation slot leaked after flush failure (body %s)", data)
	}
	if status != 200 {
		t.Fatalf("second request after flush failure: status=%d body=%s", status, data)
	}
}
