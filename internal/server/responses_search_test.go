package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/arhammahajan/router/internal/config"
)

// searchHarness binds one Chat upstream fake plus one SearXNG fake behind
// the router. No live upstream, no credentials, no logs of query/snippets.
type searchHarness struct {
	chatUpstream   *httptest.Server
	searchUpstream *httptest.Server
	router         *httptest.Server
	mu             sync.Mutex
	chatBodies     [][]byte
	chatHeaders    []http.Header
	chatPaths      []string
	chatCalls      int
	searchBodies   []string
	searchHeaders  []http.Header
	searchCalls    int64
	chatHandler    func(call int, w http.ResponseWriter, r *http.Request)
	searchHandler  func(w http.ResponseWriter, r *http.Request)
}

func searchTestCfg(t *testing.T) *config.Config {
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
api_key = "upstream-secret"
compatibility = "lossy"

[providers.strictchat]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
api_key = "upstream-secret"

[[models]]
name = "search-bound"
provider = "chatup"
model = "gpt-4o-mini"

[[models]]
name = "search-bound-alias-holder"
provider = "chatup"
model = "gpt-4o-mini"
aliases = ["search-alias"]

[[models]]
name = "search-strict"
provider = "strictchat"
model = "gpt-4o-mini"

[[models]]
name = "search-unbound"
provider = "chatup"
model = "gpt-4o-mini"

[search.backends.internal]
endpoint = "https://search.example.invalid/searxng"
count = 2

[search.backends.internal.headers]
X-Search-Key = "search-secret"

[search.bindings."search-bound"]
tool = "web_search_preview"
backend = "internal"

[search.bindings."search-bound-alias-holder"]
tool = "web_search_preview"
backend = "internal"
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

func newSearchHarness(t *testing.T, cfg *config.Config) *searchHarness {
	t.Helper()
	h := &searchHarness{}
	h.chatUpstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.chatCalls++
		call := h.chatCalls
		h.chatBodies = append(h.chatBodies, body)
		h.chatHeaders = append(h.chatHeaders, r.Header.Clone())
		h.chatPaths = append(h.chatPaths, r.URL.Path)
		fn := h.chatHandler
		h.mu.Unlock()
		if fn != nil {
			fn(call, w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-up","object":"chat.completion","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`))
	}))
	t.Cleanup(h.chatUpstream.Close)
	h.searchUpstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&h.searchCalls, 1)
		h.mu.Lock()
		h.searchBodies = append(h.searchBodies, r.URL.String())
		h.searchHeaders = append(h.searchHeaders, r.Header.Clone())
		fn := h.searchHandler
		h.mu.Unlock()
		if fn != nil {
			fn(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"url":"https://example.com/a","title":"Alpha","content":"first snippet"},{"url":"https://example.com/b","title":"Beta","content":"second snippet"}]}`))
	}))
	t.Cleanup(h.searchUpstream.Close)
	for _, prov := range cfg.Providers {
		prov.BaseURL = h.chatUpstream.URL
	}
	if cfg.Search != nil {
		for _, be := range cfg.Search.Backends {
			be.Endpoint = h.searchUpstream.URL
		}
	}
	srv := New(cfg)
	h.router = httptest.NewServer(srv.Handler())
	t.Cleanup(h.router.Close)
	return h
}

func postSearch(t *testing.T, url, body string, headers map[string]string) (int, http.Header, []byte) {
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

func chatCompletionWithCall(callID, args string) string {
	return `{"id":"chatcmpl-up","object":"chat.completion","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"` + callID + `","type":"function","function":{"name":"router_web_search","arguments":` + quoteJSONString(args) + `}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`
}

func chatCompletionText(text string, prompt, completion int) string {
	return `{"id":"chatcmpl-up","object":"chat.completion","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":` + quoteJSONString(text) + `},"finish_reason":"stop"}],"usage":{"prompt_tokens":` + itoaJSON(prompt) + `,"completion_tokens":` + itoaJSON(completion) + `,"total_tokens":` + itoaJSON(prompt+completion) + `}}`
}

func quoteJSONString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func itoaJSON(n int) string {
	return strconv.Itoa(n)
}

func mustJSON(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("not JSON: %v (%s)", err, data)
	}
	return v
}

// Wrong-route and unbound routes refuse before any I/O.
func TestSearchWrongRouteRefusedNoIO(t *testing.T) {
	h := newSearchHarness(t, searchTestCfg(t))
	for _, model := range []string{"search-strict", "search-unbound", "ghost-model"} {
		status, _, data := postSearch(t, h.router.URL+"/v1/responses",
			`{"model":"`+model+`","input":"Hi","tools":[{"type":"web_search_preview"}]}`, nil)
		if status != 400 && status != 404 {
			t.Fatalf("model=%s status=%d body=%s, want 4xx refusal", model, status, data)
		}
	}
	h.mu.Lock()
	cc, sc := h.chatCalls, atomic.LoadInt64(&h.searchCalls)
	h.mu.Unlock()
	if cc != 0 || sc != 0 {
		t.Fatalf("wrong-route must be zero-I/O: chat=%d search=%d", cc, sc)
	}
}

// Unsupported controls refuse before I/O with no search and no continuation.
func TestSearchUnsupportedControlsRefused(t *testing.T) {
	cases := []string{
		`{"model":"search-bound","input":"Hi","tools":[{"type":"web_search_preview"}],"store":true}`,
		`{"model":"search-bound","input":"Hi","tools":[{"type":"web_search_preview"}],"max_tool_calls":0}`,
		`{"model":"search-bound","input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.invalid/img"}]}],"tools":[{"type":"web_search_preview"}]}`,
		`{"model":"search-bound","input":"Hi","tools":[{"type":"web_search_preview"}],"tool_choice":"required"}`,
		`{"model":"search-bound","input":"Hi","tools":[{"type":"web_search_preview","filters":{"allowed_domains":["example.com"]}}]}`,
		`{"model":"search-bound","input":"Hi","tools":[{"type":"web_search_preview"}],"include":["web_search_call.results"]}`,
		`{"model":"search-bound","input":"Hi","tools":[{"type":"web_search_preview"}],"previous_response_id":"resp_1"}`,
		`{"model":"search-bound","input":[{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}],"tools":[{"type":"web_search_preview"}]}`,
	}
	for i, body := range cases {
		h := newSearchHarness(t, searchTestCfg(t))
		status, _, _ := postSearch(t, h.router.URL+"/v1/responses", body, nil)
		if status != 400 {
			t.Fatalf("case %d status=%d, want 400", i, status)
		}
		h.mu.Lock()
		cc := h.chatCalls
		h.mu.Unlock()
		if cc != 0 || atomic.LoadInt64(&h.searchCalls) != 0 {
			t.Fatalf("case %d must be zero-I/O: chat=%d search=%d", i, cc, atomic.LoadInt64(&h.searchCalls))
		}
	}
}

// Ordinary text returns without a search: one model round, source echo.
func TestSearchNoCall(t *testing.T) {
	h := newSearchHarness(t, searchTestCfg(t))
	h.chatHandler = func(call int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatCompletionText("hello", 7, 3)))
	}
	status, hdr, data := postSearch(t, h.router.URL+"/v1/responses",
		`{"model":"search-bound","input":"Hi","tools":[{"type":"web_search_preview"}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	v := mustJSON(t, data)
	if v["model"] != "search-bound" {
		t.Fatalf("public model not preserved: %s", data)
	}
	out, _ := v["output"].([]any)
	for _, item := range out {
		m, _ := item.(map[string]any)
		if m["type"] == "web_search_call" {
			t.Fatalf("no-call must not emit web_search_call: %s", data)
		}
	}
	if hdr.Get("X-Model-Router-Loss") == "" {
		t.Fatalf("search substitution must carry loss header")
	}
	tools, _ := v["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("source echo missing: %s", data)
	}
	tool, _ := tools[0].(map[string]any)
	if tool["type"] != "web_search_preview" {
		t.Fatalf("source echo wrong: %v", tool)
	}
	if v["tool_choice"] != "auto" {
		t.Fatalf("choice echo wrong: %s", data)
	}
	if h.chatCalls != 1 || atomic.LoadInt64(&h.searchCalls) != 0 {
		t.Fatalf("no-call accounts one model round: chat=%d search=%d", h.chatCalls, atomic.LoadInt64(&h.searchCalls))
	}
	var up map[string]any
	if err := json.Unmarshal(h.chatBodies[0], &up); err != nil {
		t.Fatal(err)
	}
	toolsUp, _ := up["tools"].([]any)
	if len(toolsUp) != 1 {
		t.Fatalf("synthetic function missing upstream: %v", up["tools"])
	}
}

// Full lineage: same credential/endpoint/session, one search, aggregated
// usage, citation for verified markers only, sources only on request.
func TestSearchCallLineage(t *testing.T) {
	h := newSearchHarness(t, searchTestCfg(t))
	h.chatHandler = func(call int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = w.Write([]byte(chatCompletionWithCall("call_abc", `{"query":"fresh news"}`)))
			return
		}
		_, _ = w.Write([]byte(chatCompletionText("Latest: [S1] and [S2] done.", 11, 5)))
	}
	status, _, data := postSearch(t, h.router.URL+"/v1/responses",
		`{"model":"search-bound","input":"What is new?","tools":[{"type":"web_search_preview"}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	v := mustJSON(t, data)
	out, _ := v["output"].([]any)
	if len(out) < 2 {
		t.Fatalf("output missing search + message: %s", data)
	}
	first, _ := out[0].(map[string]any)
	if first["type"] != "web_search_call" || first["status"] != "completed" {
		t.Fatalf("first output must be completed web_search_call: %v", first)
	}
	action, _ := first["action"].(map[string]any)
	if action["type"] != "search" || action["query"] != "fresh news" {
		t.Fatalf("action must carry actual query: %v", action)
	}
	if _, has := action["sources"]; has {
		t.Fatalf("sources must be absent without include: %s", data)
	}
	id, _ := first["id"].(string)
	if !strings.HasPrefix(id, "ws_") {
		t.Fatalf("web_search_call id must be router-issued ws_: %q", id)
	}
	// Citation annotations only for verified markers.
	var msg map[string]any
	for _, item := range out[1:] {
		m, _ := item.(map[string]any)
		if m["type"] == "message" {
			msg = m
		}
	}
	if msg == nil {
		t.Fatalf("message missing: %s", data)
	}
	content, _ := msg["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content missing: %s", data)
	}
	part, _ := content[0].(map[string]any)
	text, _ := part["text"].(string)
	if !strings.Contains(text, "[S1]") {
		t.Fatalf("final text missing marker: %s", data)
	}
	anns, _ := part["annotations"].([]any)
	if len(anns) != 2 {
		t.Fatalf("want 2 verified annotations, got %d (%s)", len(anns), data)
	}
	seen := map[string]bool{}
	for _, a := range anns {
		m, _ := a.(map[string]any)
		if m["type"] != "url_citation" {
			t.Fatalf("annotation type wrong: %v", m)
		}
		url, _ := m["url"].(string)
		seen[url] = true
		s, _ := m["start_index"].(float64)
		e, _ := m["end_index"].(float64)
		if e <= s {
			t.Fatalf("annotation span invalid: %v", m)
		}
		label := text[int(s):int(e)]
		if label != "[S1]" && label != "[S2]" {
			t.Fatalf("annotation span %q not a verified marker in %q", label, text)
		}
	}
	if !seen["https://example.com/a"] || !seen["https://example.com/b"] {
		t.Fatalf("annotations must use actual result URLs: %v", seen)
	}
	usage, _ := v["usage"].(map[string]any)
	if usage["input_tokens"] != float64(18) || usage["output_tokens"] != float64(8) || usage["total_tokens"] != float64(26) {
		t.Fatalf("usage must aggregate both rounds (7+11,3+5,10+16): %v", usage)
	}
	if h.chatCalls != 2 || atomic.LoadInt64(&h.searchCalls) != 1 {
		t.Fatalf("one search under two model rounds: chat=%d search=%d", h.chatCalls, atomic.LoadInt64(&h.searchCalls))
	}
	// Same credential/endpoint/session over both rounds; opaque ID preserved.
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.chatPaths[0] != h.chatPaths[1] {
		t.Fatalf("endpoint must stay fixed: %v", h.chatPaths)
	}
	if h.chatHeaders[0].Get("Authorization") != "Bearer upstream-secret" ||
		h.chatHeaders[1].Get("Authorization") != "Bearer upstream-secret" {
		t.Fatalf("provider credential must stay fixed")
	}
	var second map[string]any
	if err := json.Unmarshal(h.chatBodies[1], &second); err != nil {
		t.Fatal(err)
	}
	msgs, _ := second["messages"].([]any)
	foundCall, foundTool := false, false
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		if mm["role"] == "assistant" {
			if tcs, ok := mm["tool_calls"].([]any); ok {
				for _, tc := range tcs {
					tcm, _ := tc.(map[string]any)
					if tcm["id"] == "call_abc" {
						foundCall = true
					}
				}
			}
		}
		if mm["role"] == "tool" && mm["tool_call_id"] == "call_abc" {
			foundTool = true
			content, _ := mm["content"].(string)
			if !strings.Contains(content, "example.com/a") {
				t.Fatalf("tool result must carry bounded data: %q", content)
			}
		}
	}
	if !foundCall || !foundTool {
		t.Fatalf("continuation must append original call + result with same ID: %s", h.chatBodies[1])
	}
	if second["tool_choice"] != "none" {
		t.Fatalf("continuation must disable further calls: %v", second["tool_choice"])
	}
}

// Continuation retry cannot rerun the search.
func TestSearchContinuationRetrySingleSearch(t *testing.T) {
	cfg := searchTestCfg(t)
	cfg.Retry.MaxAttempts = 2
	cfg.Retry.Statuses = []int{429}
	h := newSearchHarness(t, cfg)
	var contAttempts int64
	h.chatHandler = func(call int, w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// Reconstruct: handler already consumed body in wrapper; inspect via captured bodies.
		_ = body
		h.mu.Lock()
		last := h.chatBodies[len(h.chatBodies)-1]
		h.mu.Unlock()
		var probe map[string]any
		_ = json.Unmarshal(last, &probe)
		msgs, _ := probe["messages"].([]any)
		isCont := false
		for _, m := range msgs {
			if mm, ok := m.(map[string]any); ok && mm["role"] == "tool" {
				isCont = true
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if !isCont {
			_, _ = w.Write([]byte(chatCompletionWithCall("call_r", `{"query":"retry probe"}`)))
			return
		}
		if atomic.AddInt64(&contAttempts, 1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"error":{"message":"busy","type":"api_error","param":null,"code":"rate_limit"}}`))
			return
		}
		_, _ = w.Write([]byte(chatCompletionText("done [S1]", 11, 5)))
	}
	status, _, data := postSearch(t, h.router.URL+"/v1/responses",
		`{"model":"search-bound","input":"Hi","tools":[{"type":"web_search_preview"}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if atomic.LoadInt64(&h.searchCalls) != 1 {
		t.Fatalf("retry must not rerun search: search=%d chat=%d", atomic.LoadInt64(&h.searchCalls), h.chatCalls)
	}
	if h.chatCalls != 3 {
		t.Fatalf("want first + 429 + success = 3 model calls, got %d", h.chatCalls)
	}
}

// Auth isolation: model and search credentials never cross.
func TestSearchAuthIsolation(t *testing.T) {
	h := newSearchHarness(t, searchTestCfg(t))
	h.chatHandler = func(call int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = w.Write([]byte(chatCompletionWithCall("call_auth", `{"query":"isolation"}`)))
			return
		}
		_, _ = w.Write([]byte(chatCompletionText("ok", 11, 5)))
	}
	status, _, data := postSearch(t, h.router.URL+"/v1/responses",
		`{"model":"search-bound","input":"Hi","tools":[{"type":"web_search_preview"}]}`,
		map[string]string{"Authorization": "Bearer client-key", "X-Api-Key": "client-key"})
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, hdr := range h.chatHeaders {
		if got := hdr.Get("Authorization"); got != "Bearer upstream-secret" {
			t.Fatalf("chat call %d auth=%q, want upstream-secret", i, got)
		}
		if got := hdr.Get("X-Search-Key"); got != "" {
			t.Fatalf("search header leaked to model: %q", got)
		}
	}
	if len(h.searchHeaders) != 1 {
		t.Fatalf("want 1 search call, got %d", len(h.searchHeaders))
	}
	sh := h.searchHeaders[0]
	if got := sh.Get("X-Search-Key"); got != "search-secret" {
		t.Fatalf("search auth=%q, want search-secret", got)
	}
	if got := sh.Get("Authorization"); got == "Bearer upstream-secret" || got == "Bearer client-key" {
		t.Fatalf("model/client credential leaked to search: %q", got)
	}
}

// Backend failure ends sanitized with no continuation.
func TestSearchBackendFailureNoContinuation(t *testing.T) {
	h := newSearchHarness(t, searchTestCfg(t))
	h.chatHandler = func(call int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatCompletionWithCall("call_f", `{"query":"will fail"}`)))
	}
	h.searchHandler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}
	status, _, data := postSearch(t, h.router.URL+"/v1/responses",
		`{"model":"search-bound","input":"Hi","tools":[{"type":"web_search_preview"}]}`, nil)
	if status != 502 {
		t.Fatalf("status=%d body=%s, want 502", status, data)
	}
	if strings.Contains(string(data), "will fail") || strings.Contains(string(data), "boom") {
		t.Fatalf("failure must be sanitized without query/body: %s", data)
	}
	if h.chatCalls != 1 || atomic.LoadInt64(&h.searchCalls) != 1 {
		t.Fatalf("failure makes no continuation: chat=%d search=%d", h.chatCalls, atomic.LoadInt64(&h.searchCalls))
	}
}

// Malformed/duplicate query args and mixed output refuse before search I/O.
func TestSearchMalformedQueryRefused(t *testing.T) {
	badArgs := []string{
		`{"query":"a","extra":1}`,
		`{"query":"a","query":"b"}`,
		`{"query":""}`,
		`{"query":"` + strings.Repeat("x", 501) + `"}`,
		`[1]`,
	}
	for i, args := range badArgs {
		h := newSearchHarness(t, searchTestCfg(t))
		escaped, _ := json.Marshal(args)
		_ = escaped
		h.chatHandler = func(call int, w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			// Emit the bad args verbatim as the tool arguments string.
			_, _ = w.Write([]byte(`{"id":"chatcmpl-up","object":"chat.completion","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_bad","type":"function","function":{"name":"router_web_search","arguments":` + quoteJSONString(args) + `}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`))
		}
		status, _, _ := postSearch(t, h.router.URL+"/v1/responses",
			`{"model":"search-bound","input":"Hi","tools":[{"type":"web_search_preview"}]}`, nil)
		if status != 502 {
			t.Fatalf("case %d status=%d, want 502", i, status)
		}
		if atomic.LoadInt64(&h.searchCalls) != 0 {
			t.Fatalf("case %d must not search: %d", i, atomic.LoadInt64(&h.searchCalls))
		}
	}
}

// Mixed text+tool and second-call finals refuse without success.
func TestSearchMixedAndSecondCallRefused(t *testing.T) {
	h := newSearchHarness(t, searchTestCfg(t))
	h.chatHandler = func(call int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = w.Write([]byte(`{"id":"chatcmpl-up","object":"chat.completion","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"hi","tool_calls":[{"id":"call_m","type":"function","function":{"name":"router_web_search","arguments":"{\"query\":\"x\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`))
			return
		}
		_, _ = w.Write([]byte(chatCompletionText("unreached", 1, 1)))
	}
	status, _, _ := postSearch(t, h.router.URL+"/v1/responses",
		`{"model":"search-bound","input":"Hi","tools":[{"type":"web_search_preview"}]}`, nil)
	if status != 502 {
		t.Fatalf("mixed status=%d, want 502", status)
	}
	if atomic.LoadInt64(&h.searchCalls) != 0 {
		t.Fatalf("mixed must not search")
	}

	h2 := newSearchHarness(t, searchTestCfg(t))
	h2.chatHandler = func(call int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = w.Write([]byte(chatCompletionWithCall("call_2", `{"query":"ok"}`)))
			return
		}
		_, _ = w.Write([]byte(chatCompletionWithCall("call_3", `{"query":"again"}`)))
	}
	status, _, _ = postSearch(t, h2.router.URL+"/v1/responses",
		`{"model":"search-bound","input":"Hi","tools":[{"type":"web_search_preview"}]}`, nil)
	if status != 502 {
		t.Fatalf("second-call status=%d, want 502", status)
	}
	if atomic.LoadInt64(&h2.searchCalls) != 1 {
		t.Fatalf("second-call must search once then refuse: %d", atomic.LoadInt64(&h2.searchCalls))
	}
}
