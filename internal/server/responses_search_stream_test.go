package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// Helpers for streamed search tests. The fake Chat upstream speaks SSE;
// the router must pipe it through the existing decoder/lifecycle/encoder.

func searchStreamChunk(deltaJSON, finish string) string {
	fr := "null"
	if finish != "" {
		fr = `"` + finish + `"`
	}
	return `{"id":"chatcmpl-up","object":"chat.completion.chunk","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"delta":` + deltaJSON + `,"finish_reason":` + fr + `}]}`
}

func searchStreamUsageChunk(prompt, completion int) string {
	return `{"id":"chatcmpl-up","object":"chat.completion.chunk","created":1,"model":"gpt-4o-mini","choices":[],"usage":{"prompt_tokens":` + itoaJSON(prompt) + `,"completion_tokens":` + itoaJSON(completion) + `,"total_tokens":` + itoaJSON(prompt+completion) + `}}`
}

func writeSearchStreamText(w http.ResponseWriter, deltas []string, prompt, completion int, finish string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, d := range deltas {
		delta, _ := json.Marshal(map[string]any{"content": d})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", searchStreamChunk(string(delta), ""))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", searchStreamChunk(`{}`, finish))
	_, _ = fmt.Fprintf(w, "data: %s\n\n", searchStreamUsageChunk(prompt, completion))
	_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
}

func parseSearchStreamEvents(t *testing.T, body string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, frame := range strings.Split(body, "\n\n") {
		frame = strings.TrimSpace(frame)
		if frame == "" {
			continue
		}
		lines := strings.Split(frame, "\n")
		if len(lines) != 2 {
			t.Fatalf("bad downstream frame: %q", frame)
		}
		if !strings.HasPrefix(lines[0], "event: ") || !strings.HasPrefix(lines[1], "data: ") {
			t.Fatalf("bad downstream frame: %q", frame)
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "data: ")), &ev); err != nil {
			t.Fatal(err)
		}
		if ev["type"] != strings.TrimPrefix(lines[0], "event: ") {
			t.Fatalf("event/data mismatch: %q", frame)
		}
		out = append(out, ev)
	}
	return out
}

func postSearchStream(t *testing.T, url, body string) (int, http.Header, string) {
	t.Helper()
	status, hdr, data := postSearch(t, url, body, nil)
	return status, hdr, string(data)
}

func searchStreamTypes(events []map[string]any) []string {
	types := make([]string, 0, len(events))
	for _, ev := range events {
		s, _ := ev["type"].(string)
		types = append(types, s)
	}
	return types
}

// Full lineage over SSE: same credential/endpoint/call ID, one search,
// aggregated usage, verified citations, sources gated, loss + echo.
func TestSearchStreamCallLineage(t *testing.T) {
	h := newSearchHarness(t, searchTestCfg(t))
	h.chatHandler = func(call int, w http.ResponseWriter, r *http.Request) {
		if call == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(chatCompletionWithCall("call_abc", `{"query":"fresh news"}`)))
			return
		}
		writeSearchStreamText(w, []string{"Latest: [S", "1] and [S2] done."}, 11, 5, "stop")
	}
	status, hdr, body := postSearchStream(t, h.router.URL+"/v1/responses",
		`{"model":"search-bound","input":"What is new?","tools":[{"type":"web_search_preview"}],"stream":true}`)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if ct := hdr.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("stream must be SSE, got %q", ct)
	}
	if hdr.Get("X-Model-Router-Loss") == "" {
		t.Fatalf("search substitution must carry loss header")
	}
	events := parseSearchStreamEvents(t, body)
	types := searchStreamTypes(events)
	// Order: created, in_progress, search added/done, then message flow, completed.
	wantPrefix := []string{"response.created", "response.in_progress", "response.output_item.added", "response.output_item.done"}
	for i, want := range wantPrefix {
		if i >= len(types) || types[i] != want {
			t.Fatalf("prefix mismatch at %d: got %v want %v", i, types, wantPrefix)
		}
	}
	if types[len(types)-1] != "response.completed" {
		t.Fatalf("terminal must be completed, got %v", types)
	}
	for _, ev := range events {
		if ev["type"] == "response.failed" {
			t.Fatalf("no failure expected: %v", ev)
		}
	}
	// Search item at output_index 0 with router-issued ws_* ID.
	added := events[2]
	if added["output_index"] != float64(0) {
		t.Fatalf("search must be output_index 0: %v", added)
	}
	item, _ := added["item"].(map[string]any)
	if item["type"] != "web_search_call" {
		t.Fatalf("search added must carry web_search_call: %v", item)
	}
	wsID, _ := item["id"].(string)
	if !strings.HasPrefix(wsID, "ws_") {
		t.Fatalf("search id must be router-issued ws_: %q", wsID)
	}
	action, _ := item["action"].(map[string]any)
	if action["type"] != "search" || action["query"] != "fresh news" {
		t.Fatalf("search action must carry actual query: %v", action)
	}
	if _, has := action["sources"]; has {
		t.Fatalf("sources must be absent without include")
	}
	done := events[3]
	doneItem, _ := done["item"].(map[string]any)
	if doneItem["id"] != wsID || doneItem["status"] != "completed" {
		t.Fatalf("search done must reuse id with completed status: %v", doneItem)
	}
	// Stable identity/index and echo on created/in_progress/completed.
	for _, ev := range events {
		if ev["type"] == "response.created" || ev["type"] == "response.in_progress" || ev["type"] == "response.completed" {
			resp, _ := ev["response"].(map[string]any)
			if resp["model"] != "search-bound" {
				t.Fatalf("public model not preserved: %v", resp)
			}
			if resp["id"] == "" {
				t.Fatalf("response id missing: %v", resp)
			}
		}
	}
	created, _ := events[0]["response"].(map[string]any)
	completed, _ := events[len(events)-1]["response"].(map[string]any)
	if created["id"] != completed["id"] {
		t.Fatalf("downstream id must be stable: %v vs %v", created["id"], completed["id"])
	}
	// Sequence numbers monotonic from zero.
	for i, ev := range events {
		if int(ev["sequence_number"].(float64)) != i {
			t.Fatalf("sequence not monotonic at %d: %v", i, ev)
		}
	}
	// Message flow follows at subsequent output_index values.
	seenMsgIdx := map[float64]bool{}
	for _, ev := range events[4:] {
		if idx, ok := ev["output_index"]; ok {
			if f, ok := idx.(float64); ok && ev["type"] != "response.completed" && ev["type"] != "response.failed" {
				if f == 0 {
					t.Fatalf("model content must not reuse search index 0: %v", ev)
				}
				seenMsgIdx[f] = true
			}
		}
	}
	if len(seenMsgIdx) == 0 {
		t.Fatalf("final answer must follow at subsequent output_index values: %v", types)
	}
	// Terminal output: search first, then message with verified annotations.
	out, _ := completed["output"].([]any)
	if len(out) < 2 {
		t.Fatalf("terminal output must carry search + message: %v", completed)
	}
	first, _ := out[0].(map[string]any)
	if first["type"] != "web_search_call" || first["id"] != wsID {
		t.Fatalf("terminal must repeat search item first with stable id: %v", first)
	}
	var msg map[string]any
	for _, it := range out[1:] {
		if m, _ := it.(map[string]any); m["type"] == "message" {
			msg = m
		}
	}
	if msg == nil {
		t.Fatalf("terminal message missing: %v", completed)
	}
	content, _ := msg["content"].([]any)
	part, _ := content[0].(map[string]any)
	if part["text"] != "Latest: [S1] and [S2] done." {
		t.Fatalf("split marker must reassemble without a second copy: %v", part["text"])
	}
	anns, _ := part["annotations"].([]any)
	if len(anns) != 2 {
		t.Fatalf("want 2 verified annotations, got %d (%v)", len(anns), part)
	}
	text, _ := part["text"].(string)
	for _, a := range anns {
		m, _ := a.(map[string]any)
		s, _ := m["start_index"].(float64)
		e, _ := m["end_index"].(float64)
		if e <= s || int(e) > len(text) {
			t.Fatalf("bad span: %v", m)
		}
		// Rune offsets: exact markers in the emitted text.
		label := text[int(s):int(e)]
		if label != "[S1]" && label != "[S2]" {
			t.Fatalf("span %q is not a verified marker in %q", label, text)
		}
	}
	usage, _ := completed["usage"].(map[string]any)
	if usage["input_tokens"] != float64(18) || usage["output_tokens"] != float64(8) || usage["total_tokens"] != float64(26) {
		t.Fatalf("usage must aggregate both rounds (7+11,3+5,10+16): %v", usage)
	}
	if h.chatCalls != 2 || atomic.LoadInt64(&h.searchCalls) != 1 {
		t.Fatalf("one search under two model rounds: chat=%d search=%d", h.chatCalls, atomic.LoadInt64(&h.searchCalls))
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.chatPaths[0] != h.chatPaths[1] {
		t.Fatalf("endpoint must stay fixed: %v", h.chatPaths)
	}
	if h.chatHeaders[0].Get("Authorization") != "Bearer upstream-secret" ||
		h.chatHeaders[1].Get("Authorization") != "Bearer upstream-secret" {
		t.Fatalf("provider credential must stay fixed")
	}
	var firstRequest map[string]any
	if err := json.Unmarshal(h.chatBodies[0], &firstRequest); err != nil {
		t.Fatal(err)
	}
	if firstRequest["stream"] != false || firstRequest["stream_options"] != nil {
		t.Fatalf("first model request must ask for a complete response: %s", h.chatBodies[0])
	}
	var second map[string]any
	if err := json.Unmarshal(h.chatBodies[1], &second); err != nil {
		t.Fatal(err)
	}
	options, _ := second["stream_options"].(map[string]any)
	if second["stream"] != true || options["include_usage"] != true {
		t.Fatalf("continuation must request SSE with usage: %s", h.chatBodies[1])
	}
	if second["tool_choice"] != "none" {
		t.Fatalf("continuation must disable further calls: %v", second["tool_choice"])
	}
	msgs, _ := second["messages"].([]any)
	foundCall, foundTool := false, false
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		if mm["role"] == "assistant" {
			if tcs, ok := mm["tool_calls"].([]any); ok {
				for _, tc := range tcs {
					if tcm, _ := tc.(map[string]any); tcm["id"] == "call_abc" {
						foundCall = true
					}
				}
			}
		}
		if mm["role"] == "tool" && mm["tool_call_id"] == "call_abc" {
			foundTool = true
		}
	}
	if !foundCall || !foundTool {
		t.Fatalf("continuation must preserve opaque call ID: %s", h.chatBodies[1])
	}
}

// No-call streams the already-decoded complete answer with no search item.
func TestSearchStreamNoCall(t *testing.T) {
	h := newSearchHarness(t, searchTestCfg(t))
	h.chatHandler = func(call int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatCompletionText("hello", 7, 3)))
	}
	status, hdr, body := postSearchStream(t, h.router.URL+"/v1/responses",
		`{"model":"search-bound","input":"Hi","tools":[{"type":"web_search_preview"}],"stream":true}`)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, body)
	}
	events := parseSearchStreamEvents(t, body)
	for _, ev := range events {
		if item, ok := ev["item"].(map[string]any); ok {
			if item["type"] == "web_search_call" {
				t.Fatalf("no-call must not emit web_search_call: %v", ev)
			}
		}
		if resp, ok := ev["response"].(map[string]any); ok && ev["type"] == "response.completed" {
			if out, ok := resp["output"].([]any); ok {
				for _, it := range out {
					if m, _ := it.(map[string]any); m["type"] == "web_search_call" {
						t.Fatalf("terminal must not carry search: %v", resp)
					}
				}
			}
		}
	}
	if hdr.Get("X-Model-Router-Loss") == "" {
		t.Fatalf("no-call stream must carry loss header")
	}
	completed := events[len(events)-1]
	if completed["type"] != "response.completed" {
		t.Fatalf("no-call must complete, got %v", completed["type"])
	}
	if h.chatCalls != 1 || atomic.LoadInt64(&h.searchCalls) != 0 {
		t.Fatalf("no-call accounts one model round: chat=%d search=%d", h.chatCalls, atomic.LoadInt64(&h.searchCalls))
	}
}

// Strict/unbound routes and unsupported controls refuse pre-I/O with no events.
func TestSearchStreamAdmissionRefused(t *testing.T) {
	for _, model := range []string{"search-strict", "search-unbound", "ghost-model"} {
		h := newSearchHarness(t, searchTestCfg(t))
		status, _, body := postSearchStream(t, h.router.URL+"/v1/responses",
			`{"model":"`+model+`","input":"Hi","tools":[{"type":"web_search_preview"}],"stream":true}`)
		if status != 400 && status != 404 {
			t.Fatalf("model=%s status=%d body=%s, want 4xx", model, status, body)
		}
		if strings.Contains(body, "event:") {
			t.Fatalf("refusal must not emit SSE: %s", body)
		}
		if h.chatCalls != 0 || atomic.LoadInt64(&h.searchCalls) != 0 {
			t.Fatalf("refusal must be zero-I/O")
		}
	}
	cases := []string{
		`{"model":"search-bound","input":"Hi","tools":[{"type":"web_search_preview"}],"stream":true,"store":true}`,
		`{"model":"search-bound","input":"Hi","tools":[{"type":"web_search_preview"}],"stream":true,"tool_choice":"required"}`,
		`{"model":"search-bound","input":"Hi","tools":[{"type":"web_search_preview"}],"stream":true,"include":["web_search_call.results"]}`,
		`{"model":"search-bound","input":"Hi","tools":[{"type":"web_search_preview"}],"stream":true,"max_tool_calls":0}`,
	}
	for i, req := range cases {
		h := newSearchHarness(t, searchTestCfg(t))
		status, _, body := postSearchStream(t, h.router.URL+"/v1/responses", req)
		if status != 400 {
			t.Fatalf("case %d status=%d body=%s, want 400", i, status, body)
		}
		if strings.Contains(body, "event:") {
			t.Fatalf("case %d must not emit SSE", i)
		}
		if h.chatCalls != 0 || atomic.LoadInt64(&h.searchCalls) != 0 {
			t.Fatalf("case %d must be zero-I/O", i)
		}
	}
	// Absent stream stays on the complete path and never streams.
	h := newSearchHarness(t, searchTestCfg(t))
	status, hdr, _ := postSearch(t, h.router.URL+"/v1/responses",
		`{"model":"search-bound","input":"Hi","tools":[{"type":"web_search_preview"}]}`, nil)
	if status != 200 || strings.Contains(hdr.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("absent stream must stay complete: status=%d ct=%q", status, hdr.Get("Content-Type"))
	}
}

// Malformed query args and mixed output refuse pre-commit with no SSE.
func TestSearchStreamMalformedRefused(t *testing.T) {
	h := newSearchHarness(t, searchTestCfg(t))
	h.chatHandler = func(call int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-up","object":"chat.completion","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_bad","type":"function","function":{"name":"router_web_search","arguments":"{\"query\":\"a\",\"extra\":1}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`))
	}
	status, _, body := postSearchStream(t, h.router.URL+"/v1/responses",
		`{"model":"search-bound","input":"Hi","tools":[{"type":"web_search_preview"}],"stream":true}`)
	if status != 502 {
		t.Fatalf("malformed status=%d body=%s, want 502", status, body)
	}
	if strings.Contains(body, "event:") {
		t.Fatalf("pre-commit failure must not emit SSE: %s", body)
	}
	if atomic.LoadInt64(&h.searchCalls) != 0 {
		t.Fatalf("malformed must not search")
	}
}

// Second-call finish post-commit emits one failed event and no success.
func TestSearchStreamSecondCallFailed(t *testing.T) {
	h := newSearchHarness(t, searchTestCfg(t))
	h.chatHandler = func(call int, w http.ResponseWriter, r *http.Request) {
		if call == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(chatCompletionWithCall("call_2", `{"query":"ok"}`)))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\n", searchStreamChunk(`{"content":"First answer delta"}`, ""))
		_, _ = fmt.Fprintf(w, "data: %s\n\n", searchStreamChunk(`{"tool_calls":[{"index":0,"id":"call_3","type":"function","function":{"name":"router_web_search","arguments":"{\"query\":\"again\"}"}}]}`, ""))
		_, _ = fmt.Fprintf(w, "data: %s\n\n", searchStreamChunk(`{}`, "tool_calls"))
		_, _ = fmt.Fprintf(w, "data: %s\n\n", searchStreamUsageChunk(11, 5))
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, _, body := postSearchStream(t, h.router.URL+"/v1/responses",
		`{"model":"search-bound","input":"Hi","tools":[{"type":"web_search_preview"}],"stream":true}`)
	if status != 200 {
		t.Fatalf("post-commit keeps 200, got %d body=%s", status, body)
	}
	events := parseSearchStreamEvents(t, body)
	types := searchStreamTypes(events)
	failed, completed := 0, 0
	for _, typ := range types {
		if typ == "response.failed" {
			failed++
		}
		if typ == "response.completed" || typ == "response.incomplete" {
			completed++
		}
	}
	if failed != 1 || completed != 0 {
		t.Fatalf("must emit one failed and no success: %v", types)
	}
	if atomic.LoadInt64(&h.searchCalls) != 1 {
		t.Fatalf("second-call must search once then refuse: %d", atomic.LoadInt64(&h.searchCalls))
	}
}

// Search backend failure is sanitized pre-commit with no SSE.
func TestSearchStreamBackendFailure(t *testing.T) {
	h := newSearchHarness(t, searchTestCfg(t))
	h.chatHandler = func(call int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatCompletionWithCall("call_f", `{"query":"will fail"}`)))
	}
	h.searchHandler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}
	status, _, body := postSearchStream(t, h.router.URL+"/v1/responses",
		`{"model":"search-bound","input":"Hi","tools":[{"type":"web_search_preview"}],"stream":true}`)
	if status != 502 {
		t.Fatalf("status=%d body=%s, want 502", status, body)
	}
	if strings.Contains(body, "will fail") || strings.Contains(body, "boom") || strings.Contains(body, "event:") {
		t.Fatalf("failure must be sanitized with no SSE: %s", body)
	}
	if h.chatCalls != 1 || atomic.LoadInt64(&h.searchCalls) != 1 {
		t.Fatalf("failure makes no continuation: chat=%d search=%d", h.chatCalls, atomic.LoadInt64(&h.searchCalls))
	}
}

// Continuation retry resends retained bytes without a second search.
func TestSearchStreamContinuationRetry(t *testing.T) {
	cfg := searchTestCfg(t)
	cfg.Retry.MaxAttempts = 2
	cfg.Retry.Statuses = []int{429}
	h := newSearchHarness(t, cfg)
	var contAttempts int64
	h.chatHandler = func(call int, w http.ResponseWriter, r *http.Request) {
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
		if !isCont {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(chatCompletionWithCall("call_r", `{"query":"retry probe"}`)))
			return
		}
		if atomic.AddInt64(&contAttempts, 1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"error":{"message":"busy","type":"api_error","param":null,"code":"rate_limit"}}`))
			return
		}
		writeSearchStreamText(w, []string{"done [S1]"}, 11, 5, "stop")
	}
	status, _, body := postSearchStream(t, h.router.URL+"/v1/responses",
		`{"model":"search-bound","input":"Hi","tools":[{"type":"web_search_preview"}],"stream":true}`)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if atomic.LoadInt64(&h.searchCalls) != 1 {
		t.Fatalf("retry must not rerun search: %d", atomic.LoadInt64(&h.searchCalls))
	}
	if h.chatCalls != 3 {
		t.Fatalf("want first + 429 + success = 3 calls, got %d", h.chatCalls)
	}
	events := parseSearchStreamEvents(t, body)
	if got := searchStreamTypes(events); got[len(got)-1] != "response.completed" {
		t.Fatalf("retry must still complete: %v", got)
	}
}

// Forged markers and raw URLs never acquire provenance; sources gated.
func TestSearchStreamCitationsGated(t *testing.T) {
	h := newSearchHarness(t, searchTestCfg(t))
	h.chatHandler = func(call int, w http.ResponseWriter, r *http.Request) {
		if call == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(chatCompletionWithCall("call_c", `{"query":"cite me"}`)))
			return
		}
		writeSearchStreamText(w, []string{"See [S1] and [S99] plus https://evil.example/x."}, 11, 5, "stop")
	}
	status, _, body := postSearchStream(t, h.router.URL+"/v1/responses",
		`{"model":"search-bound","input":"Hi","tools":[{"type":"web_search_preview"}],"stream":true,"include":["web_search_call.action.sources"]}`)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, body)
	}
	events := parseSearchStreamEvents(t, body)
	// Search item carries actual URLs only when requested.
	searchAdded := events[2]
	item, _ := searchAdded["item"].(map[string]any)
	action, _ := item["action"].(map[string]any)
	srcs, _ := action["sources"].([]any)
	if len(srcs) != 2 {
		t.Fatalf("include must surface actual URLs: %v", action)
	}
	completed := events[len(events)-1]
	resp, _ := completed["response"].(map[string]any)
	out, _ := resp["output"].([]any)
	var anns []any
	for _, it := range out {
		if m, _ := it.(map[string]any); m["type"] == "message" {
			content, _ := m["content"].([]any)
			for _, p := range content {
				if pm, _ := p.(map[string]any); pm["type"] == "output_text" {
					anns, _ = pm["annotations"].([]any)
				}
			}
		}
	}
	if len(anns) != 1 {
		t.Fatalf("only [S1] may annotate, got %d (%s)", len(anns), body)
	}
	ann, _ := anns[0].(map[string]any)
	if ann["url"] == "https://evil.example/x" {
		t.Fatalf("model-written URL must never annotate: %s", body)
	}

	h2 := newSearchHarness(t, searchTestCfg(t))
	h2.chatHandler = func(call int, w http.ResponseWriter, r *http.Request) {
		if call == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(chatCompletionWithCall("call_c2", `{"query":"cite me"}`)))
			return
		}
		writeSearchStreamText(w, []string{"See [S1]."}, 11, 5, "stop")
	}
	status, _, body = postSearchStream(t, h2.router.URL+"/v1/responses",
		`{"model":"search-bound","input":"Hi","tools":[{"type":"web_search_preview"}],"stream":true}`)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, body)
	}
	events = parseSearchStreamEvents(t, body)
	searchAdded = events[2]
	item, _ = searchAdded["item"].(map[string]any)
	action, _ = item["action"].(map[string]any)
	if _, has := action["sources"]; has {
		t.Fatalf("sources must be gated by include: %s", body)
	}
}

// Pre-search budget exhaustion fails closed with no SSE; terminal
// overflow fails post-commit with one failed event.
func TestSearchStreamBudgetLimits(t *testing.T) {
	h := newSearchHarness(t, searchTestCfg(t))
	h.chatHandler = func(call int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-up","object":"chat.completion","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_b","type":"function","function":{"name":"router_web_search","arguments":"{\"query\":\"x\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}}`))
	}
	status, _, body := postSearchStream(t, h.router.URL+"/v1/responses",
		`{"model":"search-bound","input":"Hi","tools":[{"type":"web_search_preview"}],"stream":true,"max_output_tokens":2}`)
	if status != 400 {
		t.Fatalf("exhausted budget status=%d body=%s, want 400", status, body)
	}
	if strings.Contains(body, "event:") {
		t.Fatalf("exhausted budget must not emit SSE: %s", body)
	}
	if h.chatCalls != 1 || atomic.LoadInt64(&h.searchCalls) != 0 {
		t.Fatalf("exhausted budget: no continuation: chat=%d search=%d", h.chatCalls, atomic.LoadInt64(&h.searchCalls))
	}

	h2 := newSearchHarness(t, searchTestCfg(t))
	h2.chatHandler = func(call int, w http.ResponseWriter, r *http.Request) {
		if call == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chatcmpl-up","object":"chat.completion","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_o","type":"function","function":{"name":"router_web_search","arguments":"{\"query\":\"x\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":4611686018427387904,"completion_tokens":1,"total_tokens":4611686018427387905}}`))
			return
		}
		writeSearchStreamText(w, []string{"hi"}, 4611686018427387904, 1, "stop")
	}
	// Complete-path overflow is pre-commit; stream aggregates at terminal.
	// With a huge first round the terminal sum overflows checked
	// arithmetic and must fail closed post-commit when streaming.
	status, _, body = postSearchStream(t, h2.router.URL+"/v1/responses",
		`{"model":"search-bound","input":"Hi","tools":[{"type":"web_search_preview"}],"stream":true}`)
	if status != 200 {
		t.Fatalf("overflow keeps 200 post-commit, got %d body=%s", status, body)
	}
	events := parseSearchStreamEvents(t, body)
	types := searchStreamTypes(events)
	failed, completed := 0, 0
	for _, typ := range types {
		if typ == "response.failed" {
			failed++
		}
		if typ == "response.completed" {
			completed++
		}
	}
	if failed != 1 || completed != 0 {
		t.Fatalf("overflow must fail once with no success: %v", types)
	}
	_ = io.Discard
}
