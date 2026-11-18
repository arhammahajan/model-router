package server

import (
	"net/http"
	"sync/atomic"
	"testing"
)

// Usage overflow and exhausted budget fail closed.
func TestSearchUsageAndBudgetLimits(t *testing.T) {
	h := newSearchHarness(t, searchTestCfg(t))
	h.chatHandler = func(call int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = w.Write([]byte(`{"id":"chatcmpl-up","object":"chat.completion","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_o","type":"function","function":{"name":"router_web_search","arguments":"{\"query\":\"x\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":4611686018427387904,"completion_tokens":1,"total_tokens":4611686018427387905}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"chatcmpl-up","object":"chat.completion","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4611686018427387904,"completion_tokens":1,"total_tokens":4611686018427387905}}`))
	}
	status, _, _ := postSearch(t, h.router.URL+"/v1/responses",
		`{"model":"search-bound","input":"Hi","tools":[{"type":"web_search_preview"}]}`, nil)
	if status != 502 {
		t.Fatalf("overflow status=%d, want 502", status)
	}

	h2 := newSearchHarness(t, searchTestCfg(t))
	h2.chatHandler = func(call int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = w.Write([]byte(`{"id":"chatcmpl-up","object":"chat.completion","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_b","type":"function","function":{"name":"router_web_search","arguments":"{\"query\":\"x\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}}`))
			return
		}
		_, _ = w.Write([]byte(chatCompletionText("unreached", 1, 1)))
	}
	status, _, _ = postSearch(t, h2.router.URL+"/v1/responses",
		`{"model":"search-bound","input":"Hi","tools":[{"type":"web_search_preview"}],"max_output_tokens":2}`, nil)
	if status != 400 {
		t.Fatalf("exhausted budget status=%d, want 400", status)
	}
	if h2.chatCalls != 1 || atomic.LoadInt64(&h2.searchCalls) != 0 {
		t.Fatalf("exhausted budget: no continuation: chat=%d search=%d", h2.chatCalls, atomic.LoadInt64(&h2.searchCalls))
	}
}

// Forged markers and raw URLs never acquire provenance; sources gated.
func TestSearchForgedCitationsRejected(t *testing.T) {
	h := newSearchHarness(t, searchTestCfg(t))
	h.chatHandler = func(call int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = w.Write([]byte(chatCompletionWithCall("call_c", `{"query":"cite me"}`)))
			return
		}
		_, _ = w.Write([]byte(chatCompletionText("See [S1] and [S99] plus https://evil.example/x.", 11, 5)))
	}
	status, _, data := postSearch(t, h.router.URL+"/v1/responses",
		`{"model":"search-bound","input":"Hi","tools":[{"type":"web_search_preview"}],"include":["web_search_call.action.sources"]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	v := mustJSON(t, data)
	out, _ := v["output"].([]any)
	var ws map[string]any
	var anns []any
	for _, item := range out {
		m, _ := item.(map[string]any)
		if m["type"] == "web_search_call" {
			ws = m
		}
		if m["type"] == "message" {
			content, _ := m["content"].([]any)
			for _, p := range content {
				pm, _ := p.(map[string]any)
				if pm["type"] == "output_text" {
					anns, _ = pm["annotations"].([]any)
				}
			}
		}
	}
	if ws == nil {
		t.Fatalf("web_search_call missing: %s", data)
	}
	action, _ := ws["action"].(map[string]any)
	srcs, _ := action["sources"].([]any)
	if len(srcs) != 2 {
		t.Fatalf("include must surface actual URLs: %s", data)
	}
	if len(anns) != 1 {
		t.Fatalf("only [S1] may annotate, got %d (%s)", len(anns), data)
	}
	ann, _ := anns[0].(map[string]any)
	if ann["url"] == "https://evil.example/x" {
		t.Fatalf("model-written URL must never annotate: %s", data)
	}
	if ann["url"] != "https://example.com/a" {
		t.Fatalf("annotation must use actual result URL: %v", ann)
	}

	h2 := newSearchHarness(t, searchTestCfg(t))
	h2.chatHandler = func(call int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = w.Write([]byte(chatCompletionWithCall("call_c2", `{"query":"cite me"}`)))
			return
		}
		_, _ = w.Write([]byte(chatCompletionText("See [S1].", 11, 5)))
	}
	status, _, data = postSearch(t, h2.router.URL+"/v1/responses",
		`{"model":"search-bound","input":"Hi","tools":[{"type":"web_search_preview"}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	v = mustJSON(t, data)
	out, _ = v["output"].([]any)
	for _, item := range out {
		m, _ := item.(map[string]any)
		if m["type"] == "web_search_call" {
			action, _ := m["action"].(map[string]any)
			if _, has := action["sources"]; has {
				t.Fatalf("sources must be gated by include: %s", data)
			}
		}
	}
}

// Alias resolves through the canonical binding; ordinary tools bypass search.
func TestSearchAliasAndOrdinaryBypass(t *testing.T) {
	h := newSearchHarness(t, searchTestCfg(t))
	h.chatHandler = func(call int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = w.Write([]byte(chatCompletionWithCall("call_alias", `{"query":"alias ok"}`)))
			return
		}
		_, _ = w.Write([]byte(chatCompletionText("done [S1]", 11, 5)))
	}
	status, _, data := postSearch(t, h.router.URL+"/v1/responses",
		`{"model":"search-alias","input":"Hi","tools":[{"type":"web_search_preview"}]}`, nil)
	if status != 200 {
		t.Fatalf("alias status=%d body=%s", status, data)
	}
	if atomic.LoadInt64(&h.searchCalls) != 1 {
		t.Fatalf("alias must resolve to canonical binding: search=%d", atomic.LoadInt64(&h.searchCalls))
	}

	h2 := newSearchHarness(t, searchTestCfg(t))
	h2.chatHandler = func(call int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatCompletionText("plain", 7, 3)))
	}
	status, _, data = postSearch(t, h2.router.URL+"/v1/responses",
		`{"model":"search-bound","input":"Hi"}`, nil)
	if status != 200 {
		t.Fatalf("ordinary status=%d body=%s", status, data)
	}
	if atomic.LoadInt64(&h2.searchCalls) != 0 {
		t.Fatalf("ordinary Responses must not search")
	}
	v := mustJSON(t, data)
	out, _ := v["output"].([]any)
	for _, item := range out {
		if m, _ := item.(map[string]any); m["type"] == "web_search_call" {
			t.Fatalf("ordinary must not emit search activity: %s", data)
		}
	}
}
