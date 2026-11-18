package server

// Review proof for streamed search at d202746 with root corrections
// 1a86f07 (first POST stream:false, second true/include_usage, deferred
// close, shared committer). Fake HTTP only; no live config or keys.

import (
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Stream backend deadline stays pre-commit: sanitized HTTP error, no SSE,
// no continuation.
func TestReviewStreamSearchBackendDeadlinePreCommit(t *testing.T) {
	cfg := searchTestCfg(t)
	cfg.Retry.Deadline = 0.05
	h := newSearchHarness(t, cfg)
	h.chatHandler = func(call int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatCompletionWithCall("call_dl", `{"query":"slow"}`)))
	}
	h.searchHandler = func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}
	status, _, body := postSearchStream(t, h.router.URL+"/v1/responses",
		`{"model":"search-bound","input":"Hi","tools":[{"type":"web_search_preview"}],"stream":true}`)
	if status != 504 && status != 502 {
		t.Fatalf("deadline status=%d body=%s, want 504/502", status, body)
	}
	if strings.Contains(body, "event:") {
		t.Fatalf("deadline must not emit SSE: %s", body)
	}
	if h.chatCalls != 1 {
		t.Fatalf("deadline must not continue: chat=%d", h.chatCalls)
	}
	if atomic.LoadInt64(&h.searchCalls) != 1 {
		t.Fatalf("deadline must attempt one search: %d", atomic.LoadInt64(&h.searchCalls))
	}
}

// Tool-first final stream refuses pre-commit: the search prelude is
// deferred until the first valid translatable frame, so a post-decision
// tool block with no prior text never commits downstream bytes.
func TestReviewStreamSearchToolFirstPreCommit(t *testing.T) {
	h := newSearchHarness(t, searchTestCfg(t))
	h.chatHandler = func(call int, w http.ResponseWriter, r *http.Request) {
		if call == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(chatCompletionWithCall("call_tf", `{"query":"ok"}`)))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\n", searchStreamChunk(`{"tool_calls":[{"index":0,"id":"call_late","type":"function","function":{"name":"router_web_search","arguments":"{\"query\":\"again\"}"}}]}`, ""))
		_, _ = fmt.Fprintf(w, "data: %s\n\n", searchStreamChunk(`{}`, "tool_calls"))
		_, _ = fmt.Fprintf(w, "data: %s\n\n", searchStreamUsageChunk(11, 5))
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, _, body := postSearchStream(t, h.router.URL+"/v1/responses",
		`{"model":"search-bound","input":"Hi","tools":[{"type":"web_search_preview"}],"stream":true}`)
	if status != 502 {
		t.Fatalf("tool-first status=%d body=%s, want pre-commit 502", status, body)
	}
	if strings.Contains(body, "event:") {
		t.Fatalf("tool-first must not emit SSE: %s", body)
	}
	if atomic.LoadInt64(&h.searchCalls) != 1 {
		t.Fatalf("tool-first must search once then refuse: %d", atomic.LoadInt64(&h.searchCalls))
	}
}

// Empty final answer still closes cleanly with exactly one search item.
func TestReviewStreamSearchEmptyFinalCompletes(t *testing.T) {
	h := newSearchHarness(t, searchTestCfg(t))
	h.chatHandler = func(call int, w http.ResponseWriter, r *http.Request) {
		if call == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(chatCompletionWithCall("call_empty", `{"query":"ok"}`)))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\n", searchStreamChunk(`{}`, "stop"))
		_, _ = fmt.Fprintf(w, "data: %s\n\n", searchStreamUsageChunk(11, 5))
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, _, body := postSearchStream(t, h.router.URL+"/v1/responses",
		`{"model":"search-bound","input":"Hi","tools":[{"type":"web_search_preview"}],"stream":true}`)
	if status != 200 {
		t.Fatalf("empty final status=%d body=%s, want 200", status, body)
	}
	events := parseSearchStreamEvents(t, body)
	types := searchStreamTypes(events)
	if len(types) == 0 || types[len(types)-1] != "response.completed" {
		t.Fatalf("empty final must complete, got %v", types)
	}
	added, done := 0, 0
	for _, ev := range events {
		if ev["type"] == "response.output_item.added" {
			if item, _ := ev["item"].(map[string]any); item["type"] == "web_search_call" {
				added++
			}
		}
		if ev["type"] == "response.output_item.done" {
			if item, _ := ev["item"].(map[string]any); item["type"] == "web_search_call" {
				done++
			}
		}
		if ev["type"] == "response.failed" {
			t.Fatalf("empty final must not fail: %s", body)
		}
	}
	if added != 1 || done != 1 {
		t.Fatalf("search item must appear exactly once (added=%d done=%d): %s", added, done, body)
	}
}
