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

// a2rBridgeCfg builds a router with Anthropic ingress models routed to a
// Responses upstream (strict + lossy).
func a2rBridgeCfg(t *testing.T) *config.Config {
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
base_url = "http://127.0.0.1:9/v1"
api_key = "resp-secret"

[providers.lossyresp]
dialect = "openai-responses"
base_url = "http://127.0.0.1:9/v1"
api_key = "resp-secret"
compatibility = "lossy"

[[models]]
name = "bridge-anth"
provider = "respup"
model = "gpt-4.1"

[[models]]
name = "bridge-anth-lossy"
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

// a2rBridgeTightCfg is a single-slot Anthropic->Responses config with a
// tunable absolute deadline, for precommit-timeout and flush-failure tests
// where the slot must be proven recovered.
func a2rBridgeTightCfg(t *testing.T, deadlineSecs float64) *config.Config {
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
base_url = "http://127.0.0.1:9/v1"
api_key = "resp-secret"

[[models]]
name = "bridge-anth"
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

type a2rBridgeHarness struct {
	upstream *httptest.Server
	router   *httptest.Server
	mu       sync.Mutex
	lastPath string
	lastBody []byte
	lastHdr  http.Header
	calls    int
	handler  func(w http.ResponseWriter, r *http.Request)
}

func newA2RBridgeHarness(t *testing.T, cfg *config.Config) *a2rBridgeHarness {
	t.Helper()
	h := &a2rBridgeHarness{}
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_up","object":"response","created_at":1,"model":"gpt-4.1",` +
			`"status":"completed",` +
			`"output":[{"id":"msg_up","type":"message","role":"assistant","status":"completed",` +
			`"content":[{"type":"output_text","text":"hello","annotations":[]}]}],` +
			`"usage":{"input_tokens":12,"input_tokens_details":{"cached_tokens":4},"output_tokens":5,"total_tokens":17}}`))
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

func postA2RBridge(t *testing.T, url, body string, headers map[string]string) (int, http.Header, []byte) {
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

func (h *a2rBridgeHarness) upstreamBody(t *testing.T) map[string]any {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	var v map[string]any
	if err := json.Unmarshal(h.lastBody, &v); err != nil {
		t.Fatalf("upstream body not JSON: %v", err)
	}
	return v
}

func (h *a2rBridgeHarness) numCalls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

// Anthropic ingress -> Responses upstream: JSON text translates, the public
// model is restored downstream, and usage carries the reviewed accounting.
func TestAnthropicToResponsesJSONText(t *testing.T) {
	h := newA2RBridgeHarness(t, a2rBridgeCfg(t))
	status, _, data := postA2RBridge(t, h.router.URL+"/v1/messages",
		`{"model":"bridge-anth","system":"Be brief.","max_tokens":16,"temperature":0.5,"top_p":0.9,`+
			`"messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if v["model"] != "bridge-anth" {
		t.Fatalf("public model not restored: %v", v["model"])
	}
	content, _ := v["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["text"] != "hello" {
		t.Fatalf("content=%v", v["content"])
	}
	usage, _ := v["usage"].(map[string]any)
	// Responses input total already includes cached tokens; Anthropic splits
	// the cached share out of input_tokens.
	if usage["input_tokens"] != float64(8) {
		t.Fatalf("usage input_tokens=%v, want 8", usage["input_tokens"])
	}
	if usage["cache_read_input_tokens"] != float64(4) {
		t.Fatalf("usage cache_read=%v, want 4", usage["cache_read_input_tokens"])
	}
	if usage["output_tokens"] != float64(5) {
		t.Fatalf("usage output=%v, want 5", usage["output_tokens"])
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
	if h.lastPath != "/responses" {
		t.Fatalf("upstream path=%s", h.lastPath)
	}
}

// Anthropic ingress -> Responses upstream: explicit strict:false is exact,
// tool identity and large integers are preserved, and tool-result content
// is exact on the wire.
func TestAnthropicToResponsesJSONTools(t *testing.T) {
	h := newA2RBridgeHarness(t, a2rBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_up","object":"response","created_at":1,"model":"gpt-4.1",` +
			`"status":"completed",` +
			`"output":[{"type":"function_call","id":"fc_up","call_id":"call_up","name":"get_time",` +
			`"arguments":"{\"tz\":\"UTC\",\"n\":9007199254740993}","status":"completed"}],` +
			`"usage":{"input_tokens":12,"output_tokens":7,"total_tokens":19}}`))
	}
	status, _, data := postA2RBridge(t, h.router.URL+"/v1/messages",
		`{"model":"bridge-anth","max_tokens":64,`+
			`"messages":[{"role":"user","content":"What time?"},`+
			`{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"get_time","input":{"tz":"UTC","n":9007199254740993}}]},`+
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"noon"}]}],`+
			`"tool_choice":{"type":"tool","name":"get_time"},`+
			`"tools":[{"name":"get_time","strict":false,"input_schema":{"type":"object","properties":{"tz":{"type":"string"}}}}]}`, nil)
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
	wire := string(h.lastBody)
	if !strings.Contains(wire, `"output":"noon"`) {
		t.Fatalf("tool-result content not exact on the wire: %s", wire)
	}
	if !strings.Contains(wire, `"call_id":"call_1"`) {
		t.Fatalf("tool call identity lost on the wire: %s", wire)
	}
	if !strings.Contains(wire, "9007199254740993") {
		t.Fatalf("large integer not exact on the wire: %s", wire)
	}
	if !strings.Contains(wire, `"strict":false`) {
		t.Fatalf("explicit strict:false lost on the wire: %s", wire)
	}
	_ = up
}

// Anthropic ingress -> Responses upstream: top_k has no reviewed mapping,
// so strict mode refuses before upstream I/O.
func TestAnthropicToResponsesStrictLoss(t *testing.T) {
	h := newA2RBridgeHarness(t, a2rBridgeCfg(t))
	status, _, data := postA2RBridge(t, h.router.URL+"/v1/messages",
		`{"model":"bridge-anth","max_tokens":16,"top_k":5,`+
			`"messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 400 {
		t.Fatalf("status=%d body=%s, want strict 400", status, data)
	}
	if !strings.Contains(string(data), "top_k") {
		t.Fatalf("body lacks the loss field: %s", data)
	}
	if h.numCalls() != 0 {
		t.Fatalf("upstream calls=%d, want 0 (pre-I/O refusal)", h.numCalls())
	}
}

// Anthropic ingress -> Responses upstream: lossy mode forwards with a loss
// header instead of refusing, and never forwards the dropped control.
func TestAnthropicToResponsesLossyDrop(t *testing.T) {
	h := newA2RBridgeHarness(t, a2rBridgeCfg(t))
	status, headers, _ := postA2RBridge(t, h.router.URL+"/v1/messages",
		`{"model":"bridge-anth-lossy","max_tokens":16,"top_k":5,`+
			`"messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d, want 200", status)
	}
	if headers.Get("X-Model-Router-Loss") == "" {
		t.Fatalf("lossy must carry loss header")
	}
	if strings.Contains(string(h.lastBody), "top_k") {
		t.Fatalf("lossy must not forward top_k: %s", h.lastBody)
	}
}

// Anthropic ingress -> Responses upstream: malformed requests fail before
// upstream I/O.
func TestAnthropicToResponsesMalformed(t *testing.T) {
	h := newA2RBridgeHarness(t, a2rBridgeCfg(t))
	status, _, _ := postA2RBridge(t, h.router.URL+"/v1/messages",
		`{"model":"bridge-anth","max_tokens":16,"bogus":1,`+
			`"messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 400 {
		t.Fatalf("status=%d, want 400", status)
	}
	if h.numCalls() != 0 {
		t.Fatalf("upstream calls=%d, want 0", h.numCalls())
	}
}

// Anthropic ingress -> Responses upstream: client credentials never become
// upstream credentials, and only Responses-destination opt-ins cross.
func TestAnthropicToResponsesCredentialIsolation(t *testing.T) {
	h := newA2RBridgeHarness(t, a2rBridgeCfg(t))
	status, _, _ := postA2RBridge(t, h.router.URL+"/v1/messages",
		`{"model":"bridge-anth","max_tokens":16,"messages":[{"role":"user","content":"Hi"}]}`,
		map[string]string{
			"Authorization":     "Bearer evil-client-key",
			"X-Api-Key":         "evil-client-key",
			"Anthropic-Version": "2020-01-01",
			"Anthropic-Beta":    "prompt-caching-2024-07-31",
			"Openai-Beta":       "assistants=v2",
		})
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
	if got := hdr.Get("Openai-Beta"); got != "assistants=v2" {
		t.Fatalf("Responses opt-in must cross: %q", got)
	}
	if got := hdr.Get("Anthropic-Beta"); got != "" {
		t.Fatalf("Anthropic opt-in must not cross to Responses: %q", got)
	}
	if got := hdr.Get("Anthropic-Version"); got != "" {
		t.Fatalf("Anthropic version must not cross to Responses: %q", got)
	}
}

var a2rSSETextFrames = []string{
	"event: response.created\ndata: {\"type\": \"response.created\", \"response\": {\"id\": \"resp_up\", \"object\": \"response\", \"created_at\": 1, \"status\": \"in_progress\", \"model\": \"gpt-4.1\", \"output\": []}, \"sequence_number\": 0}\n\n",
	"event: response.in_progress\ndata: {\"type\": \"response.in_progress\", \"response\": {\"id\": \"resp_up\", \"object\": \"response\", \"created_at\": 1, \"status\": \"in_progress\", \"model\": \"gpt-4.1\", \"output\": []}, \"sequence_number\": 1}\n\n",
	"event: response.output_item.added\ndata: {\"type\": \"response.output_item.added\", \"output_index\": 0, \"item\": {\"id\": \"msg_up\", \"type\": \"message\", \"status\": \"in_progress\", \"role\": \"assistant\", \"content\": []}, \"sequence_number\": 2}\n\n",
	"event: response.content_part.added\ndata: {\"type\": \"response.content_part.added\", \"item_id\": \"msg_up\", \"output_index\": 0, \"content_index\": 0, \"part\": {\"type\": \"output_text\", \"text\": \"\"}, \"sequence_number\": 3}\n\n",
	"event: response.output_text.delta\ndata: {\"type\": \"response.output_text.delta\", \"item_id\": \"msg_up\", \"output_index\": 0, \"content_index\": 0, \"delta\": \"hel\", \"sequence_number\": 4}\n\n",
	"event: response.output_text.delta\ndata: {\"type\": \"response.output_text.delta\", \"item_id\": \"msg_up\", \"output_index\": 0, \"content_index\": 0, \"delta\": \"lo\", \"sequence_number\": 5}\n\n",
	"event: response.output_text.done\ndata: {\"type\": \"response.output_text.done\", \"item_id\": \"msg_up\", \"output_index\": 0, \"content_index\": 0, \"text\": \"hello\", \"sequence_number\": 6}\n\n",
	"event: response.content_part.done\ndata: {\"type\": \"response.content_part.done\", \"item_id\": \"msg_up\", \"output_index\": 0, \"content_index\": 0, \"part\": {\"type\": \"output_text\", \"text\": \"hello\"}, \"sequence_number\": 7}\n\n",
	"event: response.output_item.done\ndata: {\"type\": \"response.output_item.done\", \"output_index\": 0, \"item\": {\"id\": \"msg_up\", \"type\": \"message\", \"status\": \"completed\", \"role\": \"assistant\", \"content\": [{\"type\": \"output_text\", \"text\": \"hello\", \"annotations\": []}]}, \"sequence_number\": 8}\n\n",
	"event: response.completed\ndata: {\"type\": \"response.completed\", \"response\": {\"id\": \"resp_up\", \"object\": \"response\", \"created_at\": 1, \"status\": \"completed\", \"model\": \"gpt-4.1\", \"output\": [{\"id\": \"msg_up\", \"type\": \"message\", \"status\": \"completed\", \"role\": \"assistant\", \"content\": [{\"type\": \"output_text\", \"text\": \"hello\", \"annotations\": []}]}], \"usage\": {\"input_tokens\": 12, \"input_tokens_details\": {\"cached_tokens\": 4}, \"output_tokens\": 5, \"total_tokens\": 17}}, \"sequence_number\": 9}\n\n",
}

// Anthropic ingress -> Responses upstream: SSE text translates into the
// Anthropic event lifecycle — placeholder start usage, authoritative
// terminal delta, message_stop, no [DONE].
func TestAnthropicToResponsesSSEText(t *testing.T) {
	h := newA2RBridgeHarness(t, a2rBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range a2rSSETextFrames {
			_, _ = io.WriteString(w, f)
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, _, data := postA2RBridge(t, h.router.URL+"/v1/messages",
		`{"model":"bridge-anth","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"stream":true}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	text := string(data)
	for _, want := range []string{"event: message_start", `"input_tokens":0`, "text_delta",
		"event: message_delta", `"stop_reason":"end_turn"`, "event: message_stop"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
	if strings.Contains(text, "[DONE]") {
		t.Fatalf("Anthropic downstream must not emit [DONE]: %s", text)
	}
	if strings.Contains(text, "response.completed") {
		t.Fatalf("Responses marker must not leak downstream: %s", text)
	}
}

// Anthropic ingress -> Responses upstream: function_call deltas become
// input_json_delta events with the exact argument bytes at the terminal.
func TestAnthropicToResponsesSSETools(t *testing.T) {
	h := newA2RBridgeHarness(t, a2rBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		frames := []string{
			"event: response.created\ndata: {\"type\": \"response.created\", \"response\": {\"id\": \"resp_up\", \"object\": \"response\", \"created_at\": 1, \"status\": \"in_progress\", \"model\": \"gpt-4.1\", \"output\": []}, \"sequence_number\": 0}\n\n",
			"event: response.in_progress\ndata: {\"type\": \"response.in_progress\", \"response\": {\"id\": \"resp_up\", \"object\": \"response\", \"created_at\": 1, \"status\": \"in_progress\", \"model\": \"gpt-4.1\", \"output\": []}, \"sequence_number\": 1}\n\n",
			"event: response.output_item.added\ndata: {\"type\": \"response.output_item.added\", \"output_index\": 0, \"item\": {\"id\": \"fc_up\", \"type\": \"function_call\", \"call_id\": \"call_1\", \"name\": \"get_time\", \"arguments\": \"\", \"status\": \"in_progress\"}, \"sequence_number\": 2}\n\n",
			"event: response.function_call_arguments.delta\ndata: {\"type\": \"response.function_call_arguments.delta\", \"item_id\": \"fc_up\", \"output_index\": 0, \"delta\": \"{\\\"tz\\\":\\\"UTC\\\"\", \"sequence_number\": 3}\n\n",
			"event: response.function_call_arguments.delta\ndata: {\"type\": \"response.function_call_arguments.delta\", \"item_id\": \"fc_up\", \"output_index\": 0, \"delta\": \",\\\"n\\\":9007199254740993}\", \"sequence_number\": 4}\n\n",
			"event: response.function_call_arguments.done\ndata: {\"type\": \"response.function_call_arguments.done\", \"item_id\": \"fc_up\", \"output_index\": 0, \"arguments\": \"{\\\"tz\\\":\\\"UTC\\\",\\\"n\\\":9007199254740993}\", \"sequence_number\": 5}\n\n",
			"event: response.output_item.done\ndata: {\"type\": \"response.output_item.done\", \"output_index\": 0, \"item\": {\"id\": \"fc_up\", \"type\": \"function_call\", \"call_id\": \"call_1\", \"name\": \"get_time\", \"arguments\": \"{\\\"tz\\\":\\\"UTC\\\",\\\"n\\\":9007199254740993}\", \"status\": \"completed\"}, \"sequence_number\": 6}\n\n",
			"event: response.completed\ndata: {\"type\": \"response.completed\", \"response\": {\"id\": \"resp_up\", \"object\": \"response\", \"created_at\": 1, \"status\": \"completed\", \"model\": \"gpt-4.1\", \"output\": [{\"type\": \"function_call\", \"id\": \"fc_up\", \"call_id\": \"call_1\", \"name\": \"get_time\", \"arguments\": \"{\\\"tz\\\":\\\"UTC\\\",\\\"n\\\":9007199254740993}\", \"status\": \"completed\"}], \"usage\": {\"input_tokens\": 12, \"output_tokens\": 7, \"total_tokens\": 19}}, \"sequence_number\": 7}\n\n",
		}
		for _, f := range frames {
			_, _ = io.WriteString(w, f)
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, _, data := postA2RBridge(t, h.router.URL+"/v1/messages",
		`{"model":"bridge-anth","max_tokens":64,"messages":[{"role":"user","content":"What time?"}],"stream":true,`+
			`"tools":[{"name":"get_time","strict":false,"input_schema":{"type":"object","properties":{"tz":{"type":"string"}}}}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	text := string(data)
	if !strings.Contains(text, "input_json_delta") {
		t.Fatalf("missing tool deltas: %s", text)
	}
	if !strings.Contains(text, "9007199254740993") {
		t.Fatalf("large integer not exact: %s", text)
	}
	if strings.Contains(text, "9007199254740992") {
		t.Fatalf("float64 precision loss: %s", text)
	}
	if strings.Count(text, "event: error") != 0 {
		t.Fatalf("valid terminal must not emit an error event: %s", text)
	}
	if !strings.Contains(text, "event: message_stop") {
		t.Fatalf("valid terminal must end with message_stop: %s", text)
	}
}

// Anthropic ingress -> Responses upstream: garbage upstream SSE before
// commitment is an ordinary 502, never an empty 200.
func TestAnthropicToResponsesEarlyInvalidStream(t *testing.T) {
	h := newA2RBridgeHarness(t, a2rBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: not-json\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, _, data := postA2RBridge(t, h.router.URL+"/v1/messages",
		`{"model":"bridge-anth","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"stream":true}`, nil)
	if status != 502 {
		t.Fatalf("status=%d body=%s, want 502", status, data)
	}
	if strings.Contains(string(data), "event: message_start") {
		t.Fatalf("pre-commit failure must not commit headers: %s", data)
	}
}

// Anthropic ingress -> Responses upstream: a stalled upstream body past the
// absolute deadline is a pre-commit 504, the upstream body is closed
// promptly, and the generation slot is recovered.
func TestAnthropicToResponsesPrecommitTimeout504(t *testing.T) {
	h := newA2RBridgeHarness(t, a2rBridgeTightCfg(t, 1))
	closed := make(chan struct{})
	var closeOnce sync.Once
	var calls atomic.Int32
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp_up","object":"response","created_at":1,"model":"gpt-4.1",` +
				`"status":"completed",` +
				`"output":[{"id":"msg_up","type":"message","role":"assistant","status":"completed",` +
				`"content":[{"type":"output_text","text":"hello","annotations":[]}]}],` +
				`"usage":{"input_tokens":12,"output_tokens":5,"total_tokens":17}}`))
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
	status, _, data := postA2RBridge(t, h.router.URL+"/v1/messages",
		`{"model":"bridge-anth","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"stream":true}`, nil)
	if status != 504 {
		t.Fatalf("status=%d body=%s, want 504", status, data)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatalf("stalled upstream was not closed after pre-commit 504")
	}
	status, _, data = postA2RBridge(t, h.router.URL+"/v1/messages",
		`{"model":"bridge-anth","max_tokens":16,"messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 200 {
		t.Fatalf("slot not recovered after 504: status=%d body=%s", status, data)
	}
}

// Anthropic ingress -> Responses upstream: truncation after commitment keeps
// 200 and emits exactly one Anthropic error event — never a synthetic
// message_stop and never a duplicate.
func TestAnthropicToResponsesPostCommitTruncation(t *testing.T) {
	h := newA2RBridgeHarness(t, a2rBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range a2rSSETextFrames[:5] {
			_, _ = io.WriteString(w, f)
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Truncate: close without terminal events.
	}
	status, _, data := postA2RBridge(t, h.router.URL+"/v1/messages",
		`{"model":"bridge-anth","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"stream":true}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s, want 200 (headers committed)", status, data)
	}
	text := string(data)
	if !strings.Contains(text, "text_delta") {
		t.Fatalf("committed delta missing before failure: %s", text)
	}
	if n := strings.Count(text, "event: error\n"); n != 1 {
		t.Fatalf("want exactly one error event, got %d: %s", n, text)
	}
	if strings.Contains(text, "event: message_stop") {
		t.Fatalf("post-commit failure must not fabricate message_stop: %s", text)
	}
}

// Anthropic ingress -> Responses upstream: an unrepresentable event after
// commitment keeps 200 with one error event and no success marker.
func TestAnthropicToResponsesMidstreamUnsupported(t *testing.T) {
	h := newA2RBridgeHarness(t, a2rBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		frames := append([]string{}, a2rSSETextFrames[:5]...)
		frames = append(frames,
			"event: response.mystery\ndata: {\"type\": \"response.mystery\", \"sequence_number\": 5}\n\n")
		for _, f := range frames {
			_, _ = io.WriteString(w, f)
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, _, data := postA2RBridge(t, h.router.URL+"/v1/messages",
		`{"model":"bridge-anth","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"stream":true}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s, want 200 (headers committed)", status, data)
	}
	text := string(data)
	if n := strings.Count(text, "event: error\n"); n != 1 {
		t.Fatalf("want exactly one error event, got %d: %s", n, text)
	}
	if strings.Contains(text, "event: message_stop") {
		t.Fatalf("failure must not fabricate message_stop: %s", text)
	}
}

// Anthropic ingress -> Responses upstream: model text containing
// framing-like tokens is payload, never framing. One truncation emits one
// error frame.
func TestAnthropicToResponsesModelTextIsNotFraming(t *testing.T) {
	h := newA2RBridgeHarness(t, a2rBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		frames := []string{
			"event: response.created\ndata: {\"type\": \"response.created\", \"response\": {\"id\": \"resp_up\", \"object\": \"response\", \"created_at\": 1, \"status\": \"in_progress\", \"model\": \"gpt-4.1\", \"output\": []}, \"sequence_number\": 0}\n\n",
			"event: response.in_progress\ndata: {\"type\": \"response.in_progress\", \"response\": {\"id\": \"resp_up\", \"object\": \"response\", \"created_at\": 1, \"status\": \"in_progress\", \"model\": \"gpt-4.1\", \"output\": []}, \"sequence_number\": 1}\n\n",
			"event: response.output_item.added\ndata: {\"type\": \"response.output_item.added\", \"output_index\": 0, \"item\": {\"id\": \"msg_up\", \"type\": \"message\", \"status\": \"in_progress\", \"role\": \"assistant\", \"content\": []}, \"sequence_number\": 2}\n\n",
			"event: response.content_part.added\ndata: {\"type\": \"response.content_part.added\", \"item_id\": \"msg_up\", \"output_index\": 0, \"content_index\": 0, \"part\": {\"type\": \"output_text\", \"text\": \"\"}, \"sequence_number\": 3}\n\n",
			"event: response.output_text.delta\ndata: {\"type\": \"response.output_text.delta\", \"item_id\": \"msg_up\", \"output_index\": 0, \"content_index\": 0, \"delta\": \"say event: error aloud\", \"sequence_number\": 4}\n\n",
		}
		for _, f := range frames {
			_, _ = io.WriteString(w, f)
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, _, data := postA2RBridge(t, h.router.URL+"/v1/messages",
		`{"model":"bridge-anth","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"stream":true}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s, want 200 (headers committed)", status, data)
	}
	text := string(data)
	if !strings.Contains(text, "say event: error aloud") {
		t.Fatalf("model text payload missing: %s", text)
	}
	// The payload line is a "data:" line, so only the real error event
	// matches this framed prefix.
	if n := strings.Count(text, "event: error\n"); n != 1 {
		t.Fatalf("payload text must not count as framing; want 1 error frame, got %d: %s", n, text)
	}
}

// Anthropic ingress -> Responses upstream: a valid terminal returns promptly
// without waiting for EOF, and the generation slot is released while the
// upstream body stays open.
func TestAnthropicToResponsesPromptReturnReleasesSlot(t *testing.T) {
	cfg := a2rBridgeTightCfg(t, 30)
	var n atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) > 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp_up","object":"response","created_at":1,"model":"gpt-4.1",` +
				`"status":"completed",` +
				`"output":[{"id":"msg_up","type":"message","role":"assistant","status":"completed",` +
				`"content":[{"type":"output_text","text":"hello","annotations":[]}]}],` +
				`"usage":{"input_tokens":12,"output_tokens":5,"total_tokens":17}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range a2rSSETextFrames {
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
	status, _, data := postA2RBridge(t, routerSrv.URL+"/v1/messages",
		`{"model":"bridge-anth","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"stream":true}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if !strings.Contains(string(data), "event: message_stop") {
		t.Fatalf("valid terminal must end with message_stop: %s", data)
	}
	status, _, data = postA2RBridge(t, routerSrv.URL+"/v1/messages",
		`{"model":"bridge-anth","max_tokens":16,"messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 200 {
		t.Fatalf("slot not released after valid terminal without EOF: status=%d body=%s", status, data)
	}
}

// Anthropic ingress -> Responses upstream: a downstream flush failure
// cancels the upstream body promptly and releases the generation slot.
func TestAnthropicToResponsesFlushFailureCancelsUpstream(t *testing.T) {
	cfg := a2rBridgeTightCfg(t, 30)
	var n atomic.Int32
	closed := make(chan struct{})
	var closeOnce sync.Once
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) > 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp_up","object":"response","created_at":1,"model":"gpt-4.1",` +
				`"status":"completed",` +
				`"output":[{"id":"msg_up","type":"message","role":"assistant","status":"completed",` +
				`"content":[{"type":"output_text","text":"hello","annotations":[]}]}],` +
				`"usage":{"input_tokens":12,"output_tokens":5,"total_tokens":17}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		// A committing prefix forces the translator to commit downstream;
		// the body is then held open so the flush failure must terminate
		// promptly via the synchronous upstream close.
		for _, f := range a2rSSETextFrames[:5] {
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
	body := `{"model":"bridge-anth","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"stream":true}`
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
		t.Fatalf("flush failure did not terminate the translated stream promptly")
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatalf("upstream was not closed after downstream flush failure")
	}
	routerSrv := httptest.NewServer(srv.Handler())
	defer routerSrv.Close()
	status, _, data := postA2RBridge(t, routerSrv.URL+"/v1/messages",
		`{"model":"bridge-anth","max_tokens":16,"messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status == 503 {
		t.Fatalf("generation slot leaked after flush failure (body %s)", data)
	}
	if status != 200 {
		t.Fatalf("second request after flush failure: status=%d body=%s", status, data)
	}
}
