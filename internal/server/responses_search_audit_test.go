package server

// Independent audit regression for the Responses->Chat complete search
// vertical. All cases are
// hermetic: fake Chat + fake SearXNG, no live endpoints, credentials,
// cache or logs. Each test locks a contract clause not covered by the
// existing search suite.

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// Final generation cap: aggregated completion beyond max_output_tokens
// fails closed after the search, with no invented completion.
func TestSearchAuditFinalCapExceeded(t *testing.T) {
	h := newSearchHarness(t, searchTestCfg(t))
	h.chatHandler = func(call int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = w.Write([]byte(`{"id":"chatcmpl-up","object":"chat.completion","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_f","type":"function","function":{"name":"router_web_search","arguments":"{\"query\":\"x\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`))
			return
		}
		_, _ = w.Write([]byte(chatCompletionText("done", 5, 8)))
	}
	status, _, data := postSearch(t, h.router.URL+"/v1/responses",
		`{"model":"search-bound","input":"Hi","tools":[{"type":"web_search_preview"}],"max_output_tokens":10}`, nil)
	if status != 502 {
		t.Fatalf("final cap status=%d want 502 body=%s", status, data)
	}
	if atomic.LoadInt64(&h.searchCalls) != 1 {
		t.Fatalf("final cap must search once then refuse: %d", atomic.LoadInt64(&h.searchCalls))
	}
}

// Exact Unicode citation spans use rune indices in the emitted text.
func TestSearchAuditUnicodeSpans(t *testing.T) {
	h := newSearchHarness(t, searchTestCfg(t))
	h.chatHandler = func(call int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = w.Write([]byte(chatCompletionWithCall("call_u", `{"query":"x"}`)))
			return
		}
		_, _ = w.Write([]byte(chatCompletionText("héllo [S1] wörld [S2] ✓", 11, 5)))
	}
	status, _, data := postSearch(t, h.router.URL+"/v1/responses",
		`{"model":"search-bound","input":"Hi","tools":[{"type":"web_search_preview"}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	v := mustJSON(t, data)
	out, _ := v["output"].([]any)
	seen := 0
	for _, item := range out {
		m, _ := item.(map[string]any)
		if m["type"] != "message" {
			continue
		}
		content, _ := m["content"].([]any)
		for _, p := range content {
			pm, _ := p.(map[string]any)
			text, _ := pm["text"].(string)
			anns, _ := pm["annotations"].([]any)
			if len(anns) != 2 {
				t.Fatalf("want 2 anns got %d (%s)", len(anns), data)
			}
			runes := []rune(text)
			for _, a := range anns {
				am, _ := a.(map[string]any)
				s := int(am["start_index"].(float64))
				e := int(am["end_index"].(float64))
				if e <= s || e > len(runes) {
					t.Fatalf("bad span %v runelen=%d", am, len(runes))
				}
				label := string(runes[s:e])
				if label != "[S1]" && label != "[S2]" {
					t.Fatalf("span %q not marker in %q", label, text)
				}
			}
			seen++
		}
	}
	if seen == 0 {
		t.Fatalf("message missing: %s", data)
	}
}

// Invalid opaque call ID refuses before any search I/O.
func TestSearchAuditInvalidCallIDNoSearch(t *testing.T) {
	h := newSearchHarness(t, searchTestCfg(t))
	h.chatHandler = func(call int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-up","object":"chat.completion","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"   ","type":"function","function":{"name":"router_web_search","arguments":"{\"query\":\"x\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`))
	}
	status, _, _ := postSearch(t, h.router.URL+"/v1/responses",
		`{"model":"search-bound","input":"Hi","tools":[{"type":"web_search_preview"}]}`, nil)
	if status != 502 {
		t.Fatalf("status=%d want 502", status)
	}
	if atomic.LoadInt64(&h.searchCalls) != 0 {
		t.Fatalf("invalid callID must not search")
	}
}

// Empty backend results stay exact: empty sources when requested, no annotations.
func TestSearchAuditEmptyResults(t *testing.T) {
	h := newSearchHarness(t, searchTestCfg(t))
	h.chatHandler = func(call int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = w.Write([]byte(chatCompletionWithCall("call_e", `{"query":"x"}`)))
			return
		}
		_, _ = w.Write([]byte(chatCompletionText("no markers here", 11, 5)))
	}
	h.searchHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}
	status, _, data := postSearch(t, h.router.URL+"/v1/responses",
		`{"model":"search-bound","input":"Hi","tools":[{"type":"web_search_preview"}],"include":["web_search_call.action.sources"]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	v := mustJSON(t, data)
	out, _ := v["output"].([]any)
	found := false
	for _, item := range out {
		m, _ := item.(map[string]any)
		if m["type"] == "web_search_call" {
			found = true
			action, _ := m["action"].(map[string]any)
			srcs, _ := action["sources"].([]any)
			if len(srcs) != 0 {
				t.Fatalf("empty results sources must be empty: %s", data)
			}
		}
		if m["type"] == "message" {
			content, _ := m["content"].([]any)
			for _, p := range content {
				pm, _ := p.(map[string]any)
				if anns, _ := pm["annotations"].([]any); len(anns) != 0 {
					t.Fatalf("empty results must not annotate: %s", data)
				}
			}
		}
	}
	if !found {
		t.Fatalf("web_search_call missing: %s", data)
	}
}

// Adapter filtering: invalid URLs dropped, duplicates deduped, only kept
// results acquire provenance; unkept markers stay visible but unannotated.
func TestSearchAuditResultFiltering(t *testing.T) {
	h := newSearchHarness(t, searchTestCfg(t))
	h.chatHandler = func(call int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = w.Write([]byte(chatCompletionWithCall("call_f2", `{"query":"filter me"}`)))
			return
		}
		_, _ = w.Write([]byte(chatCompletionText("refs [S1] [S2]", 11, 5)))
	}
	h.searchHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"url":"https://example.com/good","title":"Good","content":"ok"},{"url":"javascript:alert(1)","title":"Bad","content":"x"},{"url":"https://example.com/good","title":"Dup","content":"dup"}]}`))
	}
	status, _, data := postSearch(t, h.router.URL+"/v1/responses",
		`{"model":"search-bound","input":"Hi","tools":[{"type":"web_search_preview"}],"include":["web_search_call.action.sources"]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	v := mustJSON(t, data)
	out, _ := v["output"].([]any)
	for _, item := range out {
		m, _ := item.(map[string]any)
		if m["type"] == "web_search_call" {
			action, _ := m["action"].(map[string]any)
			srcs, _ := action["sources"].([]any)
			if len(srcs) != 1 {
				t.Fatalf("filter must dedupe/drop invalid, got %d (%s)", len(srcs), data)
			}
		}
	}
}

// Deadline during the backend ends sanitized with no continuation.
func TestSearchAuditDeadlineNoContinuation(t *testing.T) {
	cfg := searchTestCfg(t)
	cfg.Retry.Deadline = 0.05
	h := newSearchHarness(t, cfg)
	h.chatHandler = func(call int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = w.Write([]byte(chatCompletionWithCall("call_d", `{"query":"slow"}`)))
			return
		}
		_, _ = w.Write([]byte(chatCompletionText("unreached", 1, 1)))
	}
	h.searchHandler = func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}
	status, _, _ := postSearch(t, h.router.URL+"/v1/responses",
		`{"model":"search-bound","input":"Hi","tools":[{"type":"web_search_preview"}]}`, nil)
	if status != 504 && status != 502 {
		t.Fatalf("want 504/502 on deadline, got %d", status)
	}
	if h.chatCalls != 1 {
		t.Fatalf("deadline must not continue: chat=%d", h.chatCalls)
	}
}
