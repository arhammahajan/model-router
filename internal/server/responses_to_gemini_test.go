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

// rgBridgeCfg builds a router with Responses ingress models routed to a
// Gemini upstream (strict + lossy) and Gemini ingress models routed to a
// Responses upstream (strict + lossy).
func rgBridgeCfg(t *testing.T) *config.Config {
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

[providers.respup]
dialect = "openai-responses"
base_url = "http://127.0.0.1:9"
api_key = "resp-secret"

[providers.lossyresp]
dialect = "openai-responses"
base_url = "http://127.0.0.1:9"
api_key = "resp-secret"
compatibility = "lossy"

[[models]]
name = "resp-gem"
provider = "gemup"
model = "gemini-2.0-flash"

[[models]]
name = "resp-gem-lossy"
provider = "lossygem"
model = "gemini-2.0-flash"

[[models]]
name = "gem-resp"
provider = "respup"
model = "gpt-4.1"

[[models]]
name = "gem-resp-lossy"
provider = "lossyresp"
model = "gpt-4.1"
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

type rgBridgeHarness struct {
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

func newRGBridgeHarness(t *testing.T, cfg *config.Config) *rgBridgeHarness {
	t.Helper()
	h := &rgBridgeHarness{}
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

func rgCalls(h *rgBridgeHarness) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

func rgTightCfg(t *testing.T, deadlineSecs float64) *config.Config {
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

[providers.respup]
dialect = "openai-responses"
base_url = "http://127.0.0.1:9"
api_key = "resp-secret"

[[models]]
name = "resp-gem"
provider = "gemup"
model = "gemini-2.0-flash"

[[models]]
name = "gem-resp"
provider = "respup"
model = "gpt-4.1"
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

const rgGeminiJSONText = `{"candidates":[{"content":{"parts":[{"text":"hello"}],"role":"model"},"finishReason":"STOP","index":0}],"modelVersion":"gemini-2.0-flash","responseId":"resp_gem_1","usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3,"totalTokenCount":10}}`

// Responses ingress -> Gemini upstream: JSON text translates with the public
// model name, system hoisting and generation config on the wire.
func TestResponsesToGeminiJSONText(t *testing.T) {
	h := newRGBridgeHarness(t, rgBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(rgGeminiJSONText))
	}
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1/responses",
		`{"model":"resp-gem","input":"Hi","instructions":"Be brief.","max_output_tokens":16,"temperature":0.5,"top_p":0.9}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if v["model"] != "resp-gem" {
		t.Fatalf("public model not restored: %s", data)
	}
	if v["status"] != "completed" {
		t.Fatalf("status=%v body=%s", v["status"], data)
	}
	out, _ := v["output"].([]any)
	if len(out) != 1 {
		t.Fatalf("output=%v", v["output"])
	}
	// Validated echo: no tools means the documented schema default.
	tools, _ := v["tools"].([]any)
	if len(tools) != 0 {
		t.Fatalf("tools echo=%v, want []", v["tools"])
	}
	if v["tool_choice"] != "auto" {
		t.Fatalf("tool_choice echo=%v, want auto", v["tool_choice"])
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
	// System instructions hoist out of the turn list; scalar controls land
	// in generationConfig.
	rawSys, _ := json.Marshal(wire["systemInstruction"])
	var sys struct {
		Parts []struct {
			Text string `json:"text"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(rawSys, &sys); err != nil {
		t.Fatalf("systemInstruction=%v", wire["systemInstruction"])
	}
	if len(sys.Parts) != 1 || sys.Parts[0].Text != "Be brief." {
		t.Fatalf("systemInstruction=%v", wire["systemInstruction"])
	}
	genCfg, _ := wire["generationConfig"].(map[string]any)
	for _, k := range []string{"temperature", "topP", "maxOutputTokens"} {
		if genCfg[k] == nil {
			t.Fatalf("generationConfig missing %q: %v", k, genCfg)
		}
	}
	if h.lastHdr.Get("X-Goog-Api-Key") != "gem-secret" {
		t.Fatalf("upstream gemini auth wrong: %v", h.lastHdr)
	}
	if v := h.lastHdr.Get("Authorization"); v != "" {
		t.Fatalf("client auth must never become upstream auth: %q", v)
	}
}

// Responses ingress -> Gemini upstream: function tools encode exactly with
// large integer arguments preserved, and downstream calls restore identity.
func TestResponsesToGeminiToolsExact(t *testing.T) {
	h := newRGBridgeHarness(t, rgBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"get_time","args":{"tz":"UTC","n":9007199254740993},"id":"call_1"}}],"role":"model"},"finishReason":"STOP","index":0}],"modelVersion":"gemini-2.0-flash","responseId":"resp_gem_2","usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":2,"totalTokenCount":11}}`))
	}
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1/responses",
		`{"model":"resp-gem","input":"Hi","tools":[{"type":"function","name":"get_time","parameters":{"type":"object"},"strict":false}],"tool_choice":"auto"}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if !strings.Contains(string(data), "9007199254740993") {
		t.Fatalf("large integer not preserved: %s", data)
	}
	if !strings.Contains(string(data), `"call_id":"call_1"`) {
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

// Responses ingress -> Gemini upstream: SSE text translates incrementally
// into Responses named events with a completed terminal.
func TestResponsesToGeminiSSEText(t *testing.T) {
	h := newRGBridgeHarness(t, rgBridgeCfg(t))
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
	status, headers, data := postGeminiBridge(t, h.router.URL+"/v1/responses",
		`{"model":"resp-gem","input":"Hi","stream":true}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if ct := headers.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("downstream must be Responses SSE, got %q", ct)
	}
	text := string(data)
	if !strings.Contains(text, "event: response.completed") {
		t.Fatalf("terminal response.completed missing: %s", text)
	}
	if strings.Contains(text, "[DONE]") {
		t.Fatalf("Responses downstream must never contain [DONE]: %s", text)
	}
	if !strings.Contains(text, `"model":"resp-gem"`) {
		t.Fatalf("public model not restored downstream: %s", text)
	}
	frames := parseResponsesFrames(t, text)
	var sawCreated, sawInProgress bool
	for _, fr := range frames {
		switch fr.event {
		case "response.created":
			sawCreated = true
		case "response.in_progress":
			sawInProgress = true
		case "response.failed":
			t.Fatalf("valid stream must not emit response.failed")
		}
		if resp, ok := fr.data["response"].(map[string]any); ok {
			if resp["model"] != "resp-gem" {
				t.Fatalf("frame %s model=%v, want public model", fr.event, resp["model"])
			}
		}
	}
	if !sawCreated || !sawInProgress {
		t.Fatalf("lifecycle incomplete (created=%v in_progress=%v): %s", sawCreated, sawInProgress, text)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lastRaw != "alt=sse" {
		t.Fatalf("upstream stream must request ?alt=sse, got %q", h.lastRaw)
	}
}

// Responses ingress -> Gemini upstream: a standard JSON-array stream is
// accepted via the explicit response Content-Type even though ?alt=sse was
// requested.
func TestResponsesToGeminiJSONArrayStream(t *testing.T) {
	h := newRGBridgeHarness(t, rgBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[{"candidates":[{"content":{"parts":[{"text":"a"}],"role":"model"},"index":0}]},{"candidates":[{"content":{"parts":[{"text":"b"}],"role":"model"},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3,"totalTokenCount":10}}]`)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1/responses",
		`{"model":"resp-gem","input":"Hi","stream":true}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	text := string(data)
	if !strings.Contains(text, "event: response.completed") {
		t.Fatalf("terminal missing: %s", text)
	}
	if strings.Contains(text, "[DONE]") {
		t.Fatalf("Responses downstream must never contain [DONE]: %s", text)
	}
}

// Responses ingress -> Gemini upstream: strict mode refuses a diagnosed
// loss before any upstream I/O.
func TestResponsesToGeminiStrictLoss(t *testing.T) {
	h := newRGBridgeHarness(t, rgBridgeCfg(t))
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1/responses",
		`{"model":"resp-gem","input":"Hi","parallel_tool_calls":false}`, nil)
	if status != 400 {
		t.Fatalf("status=%d body=%s, want 400", status, data)
	}
	if !strings.Contains(string(data), "translation_loss") {
		t.Fatalf("body lacks translation_loss: %s", data)
	}
	if n := rgCalls(h); n != 0 {
		t.Fatalf("upstream calls=%d, want 0 (pre-I/O refusal)", n)
	}
}

// Responses ingress -> Gemini upstream: lossy mode carries the diagnosed
// loss in the header instead of refusing.
func TestResponsesToGeminiLossyHeader(t *testing.T) {
	h := newRGBridgeHarness(t, rgBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(rgGeminiJSONText))
	}
	status, headers, _ := postGeminiBridge(t, h.router.URL+"/v1/responses",
		`{"model":"resp-gem-lossy","input":"Hi","parallel_tool_calls":false}`, nil)
	if status != 200 {
		t.Fatalf("status=%d, want 200", status)
	}
	if headers.Get("X-Model-Router-Loss") == "" {
		t.Fatalf("lossy must carry loss header")
	}
}

// Responses ingress -> Gemini upstream: router-held state references are
// refused before any upstream I/O.
func TestResponsesToGeminiStateRefusal(t *testing.T) {
	h := newRGBridgeHarness(t, rgBridgeCfg(t))
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1/responses",
		`{"model":"resp-gem","input":"Hi","previous_response_id":"resp_prev"}`, nil)
	if status != 400 {
		t.Fatalf("status=%d body=%s, want 400", status, data)
	}
	if n := rgCalls(h); n != 0 {
		t.Fatalf("upstream calls=%d, want 0", n)
	}
}

// Responses ingress -> Gemini upstream: malformed source is a 400 with no I/O.
func TestResponsesToGeminiMalformed(t *testing.T) {
	h := newRGBridgeHarness(t, rgBridgeCfg(t))
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1/responses",
		`{"model":"resp-gem","input":"Hi","bogus_field":1}`, nil)
	if status != 400 {
		t.Fatalf("status=%d body=%s, want 400", status, data)
	}
	if n := rgCalls(h); n != 0 {
		t.Fatalf("upstream calls=%d, want 0", n)
	}
}

// Responses ingress -> Gemini upstream: client credentials and query keys
// never become upstream credentials; no query travels upstream.
func TestResponsesToGeminiCredentialIsolation(t *testing.T) {
	h := newRGBridgeHarness(t, rgBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(rgGeminiJSONText))
	}
	status, _, _ := postGeminiBridge(t, h.router.URL+"/v1/responses?key=client-key",
		`{"model":"resp-gem","input":"Hi"}`,
		map[string]string{"Authorization": "Bearer client-key", "X-Goog-Api-Key": "client-key", "X-Api-Key": "client-key"})
	if status != 200 {
		t.Fatalf("status=%d (?key= must not break Responses ingress)", status)
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
	if h.lastRaw != "" {
		t.Fatalf("query must never travel upstream, got %q", h.lastRaw)
	}
}

// Responses ingress -> Gemini upstream: an unsupported upstream encoding is
// an ordinary pre-commit 502, never a sniffed body or a leaked raw body.
func TestResponsesToGeminiUnsupportedEncoding502(t *testing.T) {
	h := newRGBridgeHarness(t, rgBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "not a stream")
	}
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1/responses",
		`{"model":"resp-gem","input":"Hi","stream":true}`, nil)
	if status != 502 {
		t.Fatalf("status=%d body=%s, want 502", status, data)
	}
	if strings.Contains(string(data), "not a stream") {
		t.Fatalf("raw upstream body leaked: %s", data)
	}
	if strings.Contains(string(data), "response.completed") {
		t.Fatalf("pre-commit failure must not emit completed: %s", data)
	}
}

// Responses ingress -> Gemini upstream: pre-commit invalid stream is an
// ordinary 502, never an empty 200.
func TestResponsesToGeminiEarlyInvalidStream(t *testing.T) {
	h := newRGBridgeHarness(t, rgBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"mystery\":1}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1/responses",
		`{"model":"resp-gem","input":"Hi","stream":true}`, nil)
	if status != 502 {
		t.Fatalf("status=%d body=%s, want 502", status, data)
	}
	if strings.Contains(string(data), "response.completed") {
		t.Fatalf("pre-commit failure must not emit completed: %s", data)
	}
}

// Responses ingress -> Gemini upstream: post-commit truncation keeps 200
// with exactly one response.failed event carrying the public model and the
// next sequence number, never a fabricated completed.
func TestResponsesToGeminiPostcommitSingleFailure(t *testing.T) {
	h := newRGBridgeHarness(t, rgBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"par\"}],\"role\":\"model\"},\"index\":0}],\"modelVersion\":\"gemini-2.0-flash\",\"responseId\":\"resp_gem_s\"}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1/responses",
		`{"model":"resp-gem","input":"Hi","stream":true,"tools":[{"type":"function","name":"get_time","parameters":{"type":"object"},"strict":false}],"tool_choice":{"type":"function","name":"get_time"}}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s, want 200 (headers committed)", status, data)
	}
	text := string(data)
	frames := parseResponsesFrames(t, text)
	failed := 0
	for _, fr := range frames {
		if fr.event == "response.failed" {
			failed++
			resp, _ := fr.data["response"].(map[string]any)
			if resp["model"] != "resp-gem" {
				t.Fatalf("failed frame model=%v, want public model", resp["model"])
			}
			tools, _ := resp["tools"].([]any)
			if len(tools) != 1 {
				t.Fatalf("failed frame must carry the validated echo: %v", resp["tools"])
			}
		}
		if fr.event == "response.completed" {
			t.Fatalf("truncated stream must not fabricate completed")
		}
	}
	if failed != 1 {
		t.Fatalf("want exactly one response.failed, got %d: %s", failed, text)
	}
	if strings.Contains(text, "[DONE]") {
		t.Fatalf("Responses downstream must never contain [DONE]: %s", text)
	}
}

// Responses ingress -> Gemini upstream: model text containing framing-like
// tokens is payload, never framing; exactly one failure is still emitted.
func TestResponsesToGeminiModelTextIsNotFraming(t *testing.T) {
	h := newRGBridgeHarness(t, rgBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"say event: response.failed aloud\"}],\"role\":\"model\"},\"index\":0}],\"modelVersion\":\"m\",\"responseId\":\"r\"}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1/responses",
		`{"model":"resp-gem","input":"Hi","stream":true}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	text := string(data)
	if !strings.Contains(text, "event: response.failed aloud") && !strings.Contains(text, "response.failed aloud") {
		t.Fatalf("model text payload missing: %s", text)
	}
	failed := 0
	for _, fr := range parseResponsesFrames(t, text) {
		if fr.event == "response.failed" {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("want exactly one framed response.failed, got %d: %s", failed, text)
	}
}

// Responses ingress -> Gemini upstream: translated JSON past MaxBodyBytes
// is a sanitized pre-commit 502 before any downstream commit.
func TestResponsesToGeminiTranslatedJSONBodyBound(t *testing.T) {
	cfg := rgBridgeCfg(t)
	cfg.Server.MaxBodyBytes = 512
	h := newRGBridgeHarness(t, cfg)
	source := strings.Repeat("<", 80)
	upstream := fmt.Sprintf(`{"candidates":[{"content":{"role":"model","parts":[{"text":"%s"}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3,"totalTokenCount":10}}`, source)
	if len(upstream) > 512 {
		t.Fatal("fixture must fit the upstream body bound")
	}
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, upstream)
	}
	status, headers, body := postGeminiBridge(t, h.router.URL+"/v1/responses",
		`{"model":"resp-gem","input":"Hi"}`, nil)
	if status != 502 {
		t.Fatalf("expanded response status = %d, want 502 (body bytes %d)", status, len(body))
	}
	text := string(body)
	if !strings.Contains(text, "Upstream response exceeds the size bound") {
		t.Fatalf("error body missing sanitized bound message: %s", text)
	}
	if !strings.Contains(text, "upstream_response_invalid") {
		t.Fatalf("error body missing router code: %s", text)
	}
	if strings.Contains(text, source) || strings.Contains(text, "<") {
		t.Fatalf("error body leaks source bytes: %s", text)
	}
	if loss := headers.Get("X-Model-Router-Loss"); loss != "" {
		t.Fatalf("loss header = %q, want absent on bound rejection", loss)
	}
}

// Responses ingress -> Gemini upstream: safe retry hints cross on both the
// JSON and pre-commit error paths, never raw upstream bodies.
func TestResponsesToGeminiRetryHints(t *testing.T) {
	h := newRGBridgeHarness(t, rgBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "2")
		w.Header().Set("X-RateLimit-Remaining", "5")
		_, _ = w.Write([]byte(rgGeminiJSONText))
	}
	status, headers, _ := postGeminiBridge(t, h.router.URL+"/v1/responses",
		`{"model":"resp-gem","input":"Hi"}`, nil)
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	if headers.Get("Retry-After") != "2" {
		t.Fatalf("Retry-After missing: %v", headers)
	}
	if headers.Get("X-Ratelimit-Remaining") == "" {
		t.Fatalf("rate-limit hint missing: %v", headers)
	}
}

// Responses ingress -> Gemini upstream: a translated provider 504 keeps its
// status through the sanitized envelope.
func TestResponsesToGeminiProvider504(t *testing.T) {
	h := newRGBridgeHarness(t, rgBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(504)
		_, _ = w.Write([]byte(`{"error":"slow"}`))
	}
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1/responses",
		`{"model":"resp-gem","input":"Hi"}`, nil)
	if status != 504 {
		t.Fatalf("status=%d body=%s, want 504", status, data)
	}
	if strings.Contains(string(data), "slow") {
		t.Fatalf("raw upstream body leaked: %s", data)
	}
}

// Responses ingress -> Gemini upstream: a stalled upstream body past the
// absolute deadline is a pre-commit 504, the body is closed promptly, and
// the generation slot is recovered.
func TestResponsesToGeminiPrecommitTimeout504(t *testing.T) {
	h := newRGBridgeHarness(t, rgTightCfg(t, 1))
	closed := make(chan struct{})
	var closeOnce sync.Once
	var calls atomic.Int32
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(rgGeminiJSONText))
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
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1/responses",
		`{"model":"resp-gem","input":"Hi","stream":true}`, nil)
	if status != 504 {
		t.Fatalf("status=%d body=%s, want 504", status, data)
	}
	if strings.Contains(string(data), "response.completed") {
		t.Fatalf("pre-commit timeout must not emit completed: %s", data)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatalf("stalled upstream was not closed after pre-commit 504")
	}
	status, _, data = postGeminiBridge(t, h.router.URL+"/v1/responses",
		`{"model":"resp-gem","input":"Hi"}`, nil)
	if status != 200 {
		t.Fatalf("slot not recovered after 504: status=%d body=%s", status, data)
	}
}

// Responses ingress -> Gemini upstream: a downstream flush failure cancels
// the upstream body promptly and releases the generation slot.
func TestResponsesToGeminiFlushFailureCancelsUpstream(t *testing.T) {
	cfg := rgTightCfg(t, 30)
	var n atomic.Int32
	closed := make(chan struct{})
	var closeOnce sync.Once
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) > 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(rgGeminiJSONText))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
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
	body := `{"model":"resp-gem","input":"Hi","stream":true}`
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
	status, _, data := postGeminiBridge(t, routerSrv.URL+"/v1/responses",
		`{"model":"resp-gem","input":"Hi"}`, nil)
	if status == 503 {
		t.Fatalf("generation slot leaked after flush failure (body %s)", data)
	}
	if status != 200 {
		t.Fatalf("second request after flush failure: status=%d body=%s", status, data)
	}
}

// Responses ingress -> Gemini upstream: a valid terminal returns promptly
// without waiting for EOF, the upstream body is closed, and the slot is
// recovered.
func TestResponsesToGeminiPromptReturnReleasesSlot(t *testing.T) {
	cfg := rgTightCfg(t, 30)
	var n atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) > 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(rgGeminiJSONText))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hel\"}],\"role\":\"model\"},\"index\":0}],\"modelVersion\":\"m\",\"responseId\":\"r\"}\n\n")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"lo\"}],\"role\":\"model\"},\"finishReason\":\"STOP\",\"index\":0}],\"modelVersion\":\"m\",\"responseId\":\"r\",\"usageMetadata\":{\"promptTokenCount\":7,\"candidatesTokenCount\":3,\"totalTokenCount\":10}}\n\n")
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
	status, _, data := postGeminiBridge(t, routerSrv.URL+"/v1/responses",
		`{"model":"resp-gem","input":"Hi","stream":true}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if !strings.Contains(string(data), "event: response.completed") {
		t.Fatalf("valid terminal missing response.completed: %s", data)
	}
	status, _, data = postGeminiBridge(t, routerSrv.URL+"/v1/responses",
		`{"model":"resp-gem","input":"Hi"}`, nil)
	if status != 200 {
		t.Fatalf("slot not released after valid terminal without EOF: status=%d body=%s", status, data)
	}
}
