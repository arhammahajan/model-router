package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Responses ingress -> Chat upstream: JSON text translates into the Responses
// envelope with the public model and restored usage.
func TestResponsesToChatJSONText(t *testing.T) {
	h := newResponsesBridgeHarness(t, responsesBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-up","object":"chat.completion","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`))
	}
	status, _, data := postResponsesBridge(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"Hi","instructions":"Be brief.","max_output_tokens":16,"temperature":0.5}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if v["status"] != "completed" {
		t.Fatalf("status=%v body=%s", v["status"], data)
	}
	up := h.upstreamBody(t)
	if up["model"] != "gpt-4o-mini" {
		t.Fatalf("upstream model=%v", up["model"])
	}
	msgs, _ := up["messages"].([]any)
	if len(msgs) < 2 {
		t.Fatalf("upstream messages=%v", up["messages"])
	}
}

// Responses ingress -> Chat upstream: stateful references are refused before
// any upstream I/O. The reviewed contract reports code
// unsupported_responses_state; the Responses request codec currently reports
// invalid_generation_parameter (protocol fix 444f8a1b owns the mapping — the
// server returns the codec's verdict faithfully).
func TestResponsesToChatStateRefusal(t *testing.T) {
	h := newResponsesBridgeHarness(t, responsesBridgeCfg(t))
	status, _, data := postResponsesBridge(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"Hi","previous_response_id":"resp_prev"}`, nil)
	if status != 400 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if !strings.Contains(string(data), "unsupported_responses_state") {
		t.Fatalf("body lacks unsupported_responses_state: %s", data)
	}
	h.mu.Lock()
	calls := h.calls
	h.mu.Unlock()
	if calls != 0 {
		t.Fatalf("upstream calls=%d, want 0 (pre-I/O refusal)", calls)
	}
}

// Responses ingress -> Chat upstream: strict mode refuses a diagnosed loss
// before any upstream I/O.
func TestResponsesToChatStrictLoss(t *testing.T) {
	h := newResponsesBridgeHarness(t, responsesBridgeCfg(t))
	status, _, data := postResponsesBridge(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"Hi","store":true}`, nil)
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

// Responses ingress -> Chat upstream: lossy mode reconciles the diagnosed
// loss with a loss header instead of refusing.
func TestResponsesToChatLossyStore(t *testing.T) {
	h := newResponsesBridgeHarness(t, responsesBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-up","object":"chat.completion","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`))
	}
	status, headers, _ := postResponsesBridge(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-resp-lossy","input":"Hi","store":true}`, nil)
	if status != 200 {
		t.Fatalf("status=%d, want 200", status)
	}
	if headers.Get("X-Model-Router-Loss") == "" {
		t.Fatalf("lossy must carry loss header")
	}
}

// Responses ingress -> Chat upstream: client credentials never become
// upstream credentials.
func TestResponsesToChatCredentialIsolation(t *testing.T) {
	h := newResponsesBridgeHarness(t, responsesBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-up","object":"chat.completion","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`))
	}
	status, _, _ := postResponsesBridge(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"Hi"}`,
		map[string]string{"X-Api-Key": "client-key", "Authorization": "Bearer upstream-secret"})
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	h.mu.Lock()
	hdr := h.lastHdr
	h.mu.Unlock()
	if got := hdr.Get("Authorization"); got != "Bearer upstream-secret" {
		t.Fatalf("upstream Authorization=%q", got)
	}
	if got := hdr.Get("X-Api-Key"); got != "" {
		t.Fatalf("client X-Api-Key leaked upstream: %q", got)
	}
}

// Responses ingress -> Chat upstream: SSE text carries lifecycle events with
// sequence numbers and ends with response.completed.
func TestResponsesToChatSSEText(t *testing.T) {
	h := newResponsesBridgeHarness(t, responsesBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		frames := []string{
			"data: {\"id\": \"chatcmpl-up\", \"object\": \"chat.completion.chunk\", \"created\": 1, \"model\": \"gpt-4o-mini\", \"choices\": [{\"index\": 0, \"delta\": {\"role\": \"assistant\"}, \"finish_reason\": null}]}\n\n",
			"data: {\"id\": \"chatcmpl-up\", \"object\": \"chat.completion.chunk\", \"created\": 1, \"model\": \"gpt-4o-mini\", \"choices\": [{\"index\": 0, \"delta\": {\"content\": \"hel\"}, \"finish_reason\": null}]}\n\n",
			"data: {\"id\": \"chatcmpl-up\", \"object\": \"chat.completion.chunk\", \"created\": 1, \"model\": \"gpt-4o-mini\", \"choices\": [{\"index\": 0, \"delta\": {\"content\": \"lo\"}, \"finish_reason\": null}]}\n\n",
			"data: {\"id\": \"chatcmpl-up\", \"object\": \"chat.completion.chunk\", \"created\": 1, \"model\": \"gpt-4o-mini\", \"choices\": [{\"index\": 0, \"delta\": {}, \"finish_reason\": \"stop\"}]}\n\n",
			"data: {\"id\": \"chatcmpl-up\", \"object\": \"chat.completion.chunk\", \"created\": 1, \"model\": \"gpt-4o-mini\", \"choices\": [], \"usage\": {\"prompt_tokens\": 7, \"completion_tokens\": 3, \"total_tokens\": 10}}\n\n",
			"data: [DONE]\n\n",
		}
		for _, f := range frames {
			_, _ = io.WriteString(w, f)
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, _, data := postResponsesBridge(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"Hi","stream":true}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	text := string(data)
	if !strings.Contains(text, "event: response.completed") {
		t.Fatalf("missing terminal response.completed: %s", text)
	}
	if !strings.Contains(text, `"sequence_number":9`) {
		t.Fatalf("missing terminal sequence_number: %s", text)
	}
	if strings.Contains(text, "[DONE]") {
		t.Fatalf("Responses stream must not contain [DONE]: %s", text)
	}
	if !strings.Contains(text, `"delta":"hel"`) || !strings.Contains(text, `"text":"hello"`) {
		t.Fatalf("missing delta/terminal text: %s", text)
	}
}

// Responses ingress -> Chat upstream: pre-commit invalid stream is an
// ordinary 502, never an empty 200.
func TestResponsesToChatEarlyInvalidStream(t *testing.T) {
	h := newResponsesBridgeHarness(t, responsesBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"mystery\":1}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, _, data := postResponsesBridge(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"Hi","stream":true}`, nil)
	if status != 502 {
		t.Fatalf("status=%d body=%s, want 502", status, data)
	}
	if strings.Contains(string(data), "response.completed") {
		t.Fatalf("pre-commit failure must not emit completed: %s", data)
	}
}

// Responses ingress -> Chat upstream: post-commit truncation keeps 200,
// emits one response.failed with the committed response ID, and never
// fabricates response.completed.
func TestResponsesToChatMidstreamTruncation(t *testing.T) {
	h := newResponsesBridgeHarness(t, responsesBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		frames := []string{
			"data: {\"id\": \"chatcmpl-up\", \"object\": \"chat.completion.chunk\", \"created\": 1, \"model\": \"gpt-4o-mini\", \"choices\": [{\"index\": 0, \"delta\": {\"role\": \"assistant\"}, \"finish_reason\": null}]}\n\n",
			"data: {\"id\": \"chatcmpl-up\", \"object\": \"chat.completion.chunk\", \"created\": 1, \"model\": \"gpt-4o-mini\", \"choices\": [{\"index\": 0, \"delta\": {\"content\": \"par\"}, \"finish_reason\": null}]}\n\n",
		}
		for _, f := range frames {
			_, _ = io.WriteString(w, f)
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Truncate: close without finish, usage or [DONE].
	}
	status, _, data := postResponsesBridge(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"Hi","stream":true}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s, want 200 (headers committed)", status, data)
	}
	text := string(data)
	if !strings.Contains(text, `"delta":"par"`) {
		t.Fatalf("committed delta missing before EOF: %s", text)
	}
	if !strings.Contains(text, "event: response.failed") {
		t.Fatalf("post-commit failure must emit response.failed: %s", text)
	}
	if strings.Contains(text, "event: response.completed") {
		t.Fatalf("truncated stream must not fabricate completed: %s", text)
	}
	if strings.Contains(text, "[DONE]") {
		t.Fatalf("Responses stream must not contain [DONE]: %s", text)
	}
	if got := strings.Count(text, "event: response.failed"); got != 1 {
		t.Fatalf("response.failed emitted %d times, want exactly 1: %s", got, text)
	}
	// The fallback failure reuses the committed response ID and the next
	// sequence number (2 events committed: created=0, in_progress=1).
	var failedID string
	var failedSeq = -1
	for _, block := range strings.Split(text, "\n\n") {
		if !strings.Contains(block, "event: response.failed") {
			continue
		}
		for _, line := range strings.Split(block, "\n") {
			if rest, ok := strings.CutPrefix(line, "data: "); ok {
				var payload struct {
					Response struct {
						ID string `json:"id"`
					} `json:"response"`
					SequenceNumber int `json:"sequence_number"`
				}
				if err := json.Unmarshal([]byte(rest), &payload); err != nil {
					t.Fatalf("response.failed data not JSON: %v", err)
				}
				failedID = payload.Response.ID
				failedSeq = payload.SequenceNumber
			}
		}
	}
	if !strings.HasPrefix(failedID, "resp_") || failedID == "" {
		t.Fatalf("response.failed id=%q, want the committed resp_ ID", failedID)
	}
	var createdID string
	for _, block := range strings.Split(text, "\n\n") {
		if !strings.Contains(block, "event: response.created") {
			continue
		}
		for _, line := range strings.Split(block, "\n") {
			if rest, ok := strings.CutPrefix(line, "data: "); ok {
				var payload struct {
					Response struct {
						ID string `json:"id"`
					} `json:"response"`
				}
				if err := json.Unmarshal([]byte(rest), &payload); err != nil {
					t.Fatalf("response.created data not JSON: %v", err)
				}
				createdID = payload.Response.ID
			}
		}
	}
	if failedID != createdID {
		t.Fatalf("response.failed id=%q != created id=%q", failedID, createdID)
	}
	if failedSeq != 5 {
		t.Fatalf("response.failed sequence_number=%d, want 5 (created,in_progress,added,part-added,delta precede)", failedSeq)
	}
}

// Responses ingress -> Chat upstream: the JSON envelope echoes the validated
// source tool configuration — ordinary function tools with explicit
// strictness plus the forced named tool choice — from the typed plan, with
// large integers exact and the public model restored.
func TestResponsesToChatToolsEcho(t *testing.T) {
	h := newResponsesBridgeHarness(t, responsesBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-up","object":"chat.completion","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_up","type":"function","function":{"name":"get_time","arguments":"{\"tz\":\"UTC\",\"n\":9007199254740993}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`))
	}
	status, _, data := postResponsesBridge(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-resp","input":[{"role":"user","content":"What time?"}],"tools":[{"type":"function","name":"get_time","description":"clock","parameters":{"type":"object","properties":{"tz":{"type":"string"}}},"strict":false}],"tool_choice":{"type":"function","name":"get_time"}}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if v["model"] != "bridge-resp" {
		t.Fatalf("model=%v body=%s, want public model", v["model"], data)
	}
	tools, _ := v["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools echo missing: %s", data)
	}
	tool, _ := tools[0].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "get_time" {
		t.Fatalf("tool echo identity wrong: %v", tool)
	}
	if tool["strict"] != false {
		t.Fatalf("explicit strict:false must echo exactly, got %v (%s)", tool["strict"], data)
	}
	params, _ := json.Marshal(tool["parameters"])
	if !strings.Contains(string(params), "9007199254740993") && !strings.Contains(string(params), `"tz"`) {
		t.Fatalf("tool parameters echo wrong: %s", params)
	}
	choice, _ := v["tool_choice"].(map[string]any)
	if choice["type"] != "function" || choice["name"] != "get_time" {
		t.Fatalf("tool_choice echo wrong: %v (%s)", choice, data)
	}
	out, _ := v["output"].([]any)
	if len(out) == 0 {
		t.Fatalf("output missing: %s", data)
	}
	first, _ := out[0].(map[string]any)
	if first["type"] != "function_call" || first["call_id"] != "call_up" || first["name"] != "get_time" {
		t.Fatalf("output call identity wrong: %v (%s)", first, data)
	}
	args, _ := first["arguments"].(string)
	if !strings.Contains(args, "9007199254740993") {
		t.Fatalf("large integer argument not preserved exactly: %q", args)
	}
}

// Responses ingress -> Chat upstream: omitted tool strictness is an implicit
// diagnosed loss (strict mode refuses before I/O); lossy mode reconciles to
// the effective non-strict upstream guarantee and echoes strict:false.
func TestResponsesToChatImplicitStrictLoss(t *testing.T) {
	h := newResponsesBridgeHarness(t, responsesBridgeCfg(t))
	status, _, data := postResponsesBridge(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"Hi","tools":[{"type":"function","name":"get_time","parameters":{"type":"object"}}]}`, nil)
	if status != 400 {
		t.Fatalf("status=%d body=%s, want 400", status, data)
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
	h2 := newResponsesBridgeHarness(t, responsesBridgeCfg(t))
	h2.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-up","object":"chat.completion","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`))
	}
	status, headers, data := postResponsesBridge(t, h2.router.URL+"/v1/responses",
		`{"model":"bridge-resp-lossy","input":"Hi","tools":[{"type":"function","name":"get_time","parameters":{"type":"object"}}]}`, nil)
	if status != 200 {
		t.Fatalf("lossy status=%d body=%s, want 200", status, data)
	}
	if headers.Get("X-Model-Router-Loss") == "" {
		t.Fatalf("lossy must carry loss header")
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	tools, _ := v["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools echo missing: %s", data)
	}
	tool, _ := tools[0].(map[string]any)
	if tool["strict"] != false {
		t.Fatalf("implicit strict must echo the effective non-strict guarantee (false), got %v (%s)", tool["strict"], data)
	}
}

// Responses ingress -> Chat upstream: unknown source tool members are
// validation errors refused before any upstream I/O, never echoed.
func TestResponsesToChatUnknownSourceFieldRefused(t *testing.T) {
	h := newResponsesBridgeHarness(t, responsesBridgeCfg(t))
	status, _, data := postResponsesBridge(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"Hi","tools":[{"type":"function","name":"get_time","parameters":{"type":"object"},"bogus":"SECRET-VALUE-12345"}]}`, nil)
	if status != 400 {
		t.Fatalf("status=%d body=%s, want 400", status, data)
	}
	h.mu.Lock()
	calls := h.calls
	h.mu.Unlock()
	if calls != 0 {
		t.Fatalf("upstream calls=%d, want 0 (pre-I/O refusal)", calls)
	}
	if !strings.Contains(string(data), "tools[0].bogus") {
		t.Fatalf("error should name the rejected field path: %s", data)
	}
	if strings.Contains(string(data), "SECRET-VALUE-12345") {
		t.Fatalf("error must not echo the unvalidated member value: %s", data)
	}
}

// Responses ingress -> Chat upstream: ignored lossy tool choice is never
// echoed as if enforced. An allowed_tools set is validated then dropped, so
// the echo carries the documented default "auto".
func TestResponsesToChatDroppedChoiceNotEchoed(t *testing.T) {
	h := newResponsesBridgeHarness(t, responsesBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-up","object":"chat.completion","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`))
	}
	status, headers, data := postResponsesBridge(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-resp-lossy","input":"Hi","tools":[{"type":"function","name":"get_time","parameters":{"type":"object"},"strict":true}],"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"get_time"}]}}`, nil)
	if status != 200 {
		t.Fatalf("lossy status=%d body=%s, want 200", status, data)
	}
	if headers.Get("X-Model-Router-Loss") == "" {
		t.Fatalf("dropped choice must carry a loss header")
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if v["tool_choice"] != "auto" {
		t.Fatalf("dropped lossy choice must echo the default, got %v (%s)", v["tool_choice"], data)
	}
}

// parseResponsesFrames splits a Responses SSE body into ordered (event,
// payload) pairs. Only lines beginning with "event:"/"data:" carry framing.
func parseResponsesFrames(t *testing.T, text string) []struct {
	event string
	data  map[string]any
} {
	t.Helper()
	var out []struct {
		event string
		data  map[string]any
	}
	for _, block := range strings.Split(text, "\n\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		var ev, raw string
		for _, line := range strings.Split(block, "\n") {
			if rest, ok := strings.CutPrefix(line, "event: "); ok {
				ev = rest
			} else if rest, ok := strings.CutPrefix(line, "data: "); ok {
				raw = rest
			}
		}
		if ev == "" || raw == "" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			t.Fatalf("frame data not JSON: %v (%s)", err, raw)
		}
		out = append(out, struct {
			event string
			data  map[string]any
		}{ev, payload})
	}
	return out
}

// Responses ingress -> Chat upstream: SSE lifecycle events echo the validated
// source tools/tool_choice consistently with the JSON path, under the public
// model identity and a gapless sequence.
func TestResponsesToChatSSEToolsEcho(t *testing.T) {
	h := newResponsesBridgeHarness(t, responsesBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		frames := []string{
			"data: {\"id\": \"chatcmpl-up\", \"object\": \"chat.completion.chunk\", \"created\": 1, \"model\": \"gpt-4o-mini\", \"choices\": [{\"index\": 0, \"delta\": {\"role\": \"assistant\"}, \"finish_reason\": null}]}\n\n",
			"data: {\"id\": \"chatcmpl-up\", \"object\": \"chat.completion.chunk\", \"created\": 1, \"model\": \"gpt-4o-mini\", \"choices\": [{\"index\": 0, \"delta\": {\"tool_calls\": [{\"index\": 0, \"id\": \"call_up\", \"type\": \"function\", \"function\": {\"name\": \"get_time\", \"arguments\": \"{\\\"tz\\\":\\\"UTC\\\"}\"}}]}, \"finish_reason\": null}]}\n\n",
			"data: {\"id\": \"chatcmpl-up\", \"object\": \"chat.completion.chunk\", \"created\": 1, \"model\": \"gpt-4o-mini\", \"choices\": [{\"index\": 0, \"delta\": {}, \"finish_reason\": \"tool_calls\"}]}\n\n",
			"data: {\"id\": \"chatcmpl-up\", \"object\": \"chat.completion.chunk\", \"created\": 1, \"model\": \"gpt-4o-mini\", \"choices\": [], \"usage\": {\"prompt_tokens\": 7, \"completion_tokens\": 3, \"total_tokens\": 10}}\n\n",
			"data: [DONE]\n\n",
		}
		for _, f := range frames {
			_, _ = io.WriteString(w, f)
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, _, data := postResponsesBridge(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"Hi","stream":true,"tools":[{"type":"function","name":"get_time","description":"clock","parameters":{"type":"object"},"strict":true}],"tool_choice":{"type":"function","name":"get_time"}}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	frames := parseResponsesFrames(t, string(data))
	if len(frames) == 0 {
		t.Fatalf("no SSE frames: %s", data)
	}
	for i, fr := range frames {
		seq, _ := fr.data["sequence_number"].(float64)
		if int(seq) != i {
			t.Fatalf("frame %d (%s) sequence_number=%v, want %d", i, fr.event, fr.data["sequence_number"], i)
		}
		// Only lifecycle events carry a response object; deltas and item
		// adds pass through untouched.
		resp, ok := fr.data["response"].(map[string]any)
		if !ok {
			continue
		}
		if resp["model"] != "bridge-resp" {
			t.Fatalf("frame %d (%s) model=%v, want public model", i, fr.event, resp["model"])
		}
		tools, _ := resp["tools"].([]any)
		if len(tools) != 1 {
			t.Fatalf("frame %d (%s) tools echo missing: %v", i, fr.event, resp["tools"])
		}
		tool, _ := tools[0].(map[string]any)
		if tool["name"] != "get_time" || tool["strict"] != true {
			t.Fatalf("frame %d (%s) tool echo wrong: %v", i, fr.event, tool)
		}
		choice, _ := resp["tool_choice"].(map[string]any)
		if choice["type"] != "function" || choice["name"] != "get_time" {
			t.Fatalf("frame %d (%s) tool_choice echo wrong: %v", i, fr.event, resp["tool_choice"])
		}
	}
	var sawCreated, sawInProgress bool
	last := frames[len(frames)-1]
	if last.event != "response.completed" {
		t.Fatalf("terminal event=%s, want response.completed", last.event)
	}
	for _, fr := range frames {
		switch fr.event {
		case "response.created":
			sawCreated = true
		case "response.in_progress":
			sawInProgress = true
		case "response.failed":
			t.Fatalf("valid stream must not emit response.failed")
		}
	}
	if !sawCreated || !sawInProgress {
		t.Fatalf("lifecycle echo incomplete (created=%v in_progress=%v): %s", sawCreated, sawInProgress, data)
	}
	completed, _ := last.data["response"].(map[string]any)
	if completed["model"] != "bridge-resp" {
		t.Fatalf("completed model=%v, want public model", completed["model"])
	}
	out, _ := completed["output"].([]any)
	if len(out) == 0 {
		t.Fatalf("completed output missing: %s", data)
	}
	call, _ := out[0].(map[string]any)
	if call["type"] != "function_call" || call["call_id"] != "call_up" || call["name"] != "get_time" {
		t.Fatalf("completed call identity wrong: %v", call)
	}
	if strings.Contains(string(data), "[DONE]") {
		t.Fatalf("Responses stream must not contain [DONE]: %s", data)
	}
}

// Responses ingress -> Chat upstream: model text containing a framing-like
// token is payload, never framing. A valid terminal still completes with no
// spurious response.failed.
func TestResponsesToChatModelTextIsNotFraming(t *testing.T) {
	h := newResponsesBridgeHarness(t, responsesBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		frames := []string{
			"data: {\"id\": \"chatcmpl-up\", \"object\": \"chat.completion.chunk\", \"created\": 1, \"model\": \"gpt-4o-mini\", \"choices\": [{\"index\": 0, \"delta\": {\"role\": \"assistant\"}, \"finish_reason\": null}]}\n\n",
			"data: {\"id\": \"chatcmpl-up\", \"object\": \"chat.completion.chunk\", \"created\": 1, \"model\": \"gpt-4o-mini\", \"choices\": [{\"index\": 0, \"delta\": {\"content\": \"say event: response.failed aloud\"}, \"finish_reason\": null}]}\n\n",
			"data: {\"id\": \"chatcmpl-up\", \"object\": \"chat.completion.chunk\", \"created\": 1, \"model\": \"gpt-4o-mini\", \"choices\": [{\"index\": 0, \"delta\": {}, \"finish_reason\": \"stop\"}]}\n\n",
			"data: {\"id\": \"chatcmpl-up\", \"object\": \"chat.completion.chunk\", \"created\": 1, \"model\": \"gpt-4o-mini\", \"choices\": [], \"usage\": {\"prompt_tokens\": 7, \"completion_tokens\": 3, \"total_tokens\": 10}}\n\n",
			"data: [DONE]\n\n",
		}
		for _, f := range frames {
			_, _ = io.WriteString(w, f)
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, _, data := postResponsesBridge(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"Hi","stream":true}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	text := string(data)
	if !strings.Contains(text, "event: response.completed") {
		t.Fatalf("valid terminal missing response.completed: %s", text)
	}
	// The payload text contains the token five times (delta, done, part,
	// item, completed output); none may become a framed event line.
	for _, fr := range parseResponsesFrames(t, text) {
		if fr.event == "response.failed" {
			t.Fatalf("model text must not trigger a framed response.failed: %s", text)
		}
	}
}

// Responses ingress -> Chat upstream: the post-commit response.failed frame
// preserves the public model identity (never empty or upstream) and carries
// the validated echo consistently with the lifecycle events.
func TestResponsesToChatPostcommitFailureIdentity(t *testing.T) {
	h := newResponsesBridgeHarness(t, responsesBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		frames := []string{
			"data: {\"id\": \"chatcmpl-up\", \"object\": \"chat.completion.chunk\", \"created\": 1, \"model\": \"gpt-4o-mini\", \"choices\": [{\"index\": 0, \"delta\": {\"role\": \"assistant\"}, \"finish_reason\": null}]}\n\n",
			"data: {\"id\": \"chatcmpl-up\", \"object\": \"chat.completion.chunk\", \"created\": 1, \"model\": \"gpt-4o-mini\", \"choices\": [{\"index\": 0, \"delta\": {\"content\": \"par\"}, \"finish_reason\": null}]}\n\n",
		}
		for _, f := range frames {
			_, _ = io.WriteString(w, f)
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, _, data := postResponsesBridge(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"Hi","stream":true,"tools":[{"type":"function","name":"get_time","parameters":{"type":"object"},"strict":false}],"tool_choice":{"type":"function","name":"get_time"}}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s, want 200 (headers committed)", status, data)
	}
	frames := parseResponsesFrames(t, string(data))
	var failed map[string]any
	var failedSeq = -1
	for _, fr := range frames {
		if fr.event == "response.failed" {
			if failed != nil {
				t.Fatalf("multiple response.failed frames")
			}
			failed = fr.data
			if s, ok := fr.data["sequence_number"].(float64); ok {
				failedSeq = int(s)
			}
		}
		if fr.event == "response.completed" {
			t.Fatalf("truncated stream must not fabricate completed")
		}
	}
	if failed == nil {
		t.Fatalf("post-commit failure must emit response.failed: %s", data)
	}
	resp, _ := failed["response"].(map[string]any)
	if resp["model"] != "bridge-resp" {
		t.Fatalf("failed frame model=%v, want public model (never empty or upstream)", resp["model"])
	}
	if failedSeq != len(frames)-1 {
		t.Fatalf("failed sequence_number=%d, want the next unique number %d", failedSeq, len(frames)-1)
	}
	tools, _ := resp["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("failed frame must carry the validated echo: %v", resp["tools"])
	}
}

// Responses ingress -> Chat upstream: a stalled upstream body past the
// absolute deadline is a pre-commit 504 (never an empty 200), the upstream
// body is closed promptly, and the generation slot is recovered.
func TestResponsesToChatPrecommitTimeout504(t *testing.T) {
	h := newResponsesBridgeHarness(t, responsesBridgeTightCfg(t, 1))
	closed := make(chan struct{})
	var closeOnce sync.Once
	var calls atomic.Int32
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chatcmpl-up","object":"chat.completion","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`))
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
	status, _, data := postResponsesBridge(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"Hi","stream":true}`, nil)
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
	status, _, data = postResponsesBridge(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"Hi"}`, nil)
	if status != 200 {
		t.Fatalf("slot not recovered after 504: status=%d body=%s", status, data)
	}
}

// Responses ingress -> Chat upstream: a downstream flush failure cancels the
// upstream body promptly and releases the generation slot.
func TestResponsesToChatFlushFailureCancelsUpstream(t *testing.T) {
	cfg := responsesBridgeTightCfg(t, 30)
	var n atomic.Int32
	closed := make(chan struct{})
	var closeOnce sync.Once
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) > 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chatcmpl-up","object":"chat.completion","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		// A committing prefix: the role chunk alone emits nothing, so include
		// a content delta that forces the translator to commit downstream.
		// The body is then held open; the flush failure must still terminate
		// promptly via the synchronous upstream close.
		_, _ = w.Write([]byte("data: {\"id\": \"chatcmpl-up\", \"object\": \"chat.completion.chunk\", \"created\": 1, \"model\": \"gpt-4o-mini\", \"choices\": [{\"index\": 0, \"delta\": {\"role\": \"assistant\"}, \"finish_reason\": null}]}\n\n" +
			"data: {\"id\": \"chatcmpl-up\", \"object\": \"chat.completion.chunk\", \"created\": 1, \"model\": \"gpt-4o-mini\", \"choices\": [{\"index\": 0, \"delta\": {\"content\": \"par\"}, \"finish_reason\": null}]}\n\n"))
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
	body := `{"model":"bridge-resp","input":"Hi","stream":true}`
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
	status, _, data := postResponsesBridge(t, routerSrv.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"Hi"}`, nil)
	if status == 503 {
		t.Fatalf("generation slot leaked after flush failure (body %s)", data)
	}
	if status != 200 {
		t.Fatalf("second request after flush failure: status=%d body=%s", status, data)
	}
}

// Responses ingress -> Chat upstream: a valid terminal (finish + terminal
// usage + [DONE]) returns promptly without waiting for EOF, and the
// generation slot is released even while the upstream body stays open.
func TestResponsesToChatPromptReturnReleasesSlot(t *testing.T) {
	cfg := responsesBridgeTightCfg(t, 30)
	var n atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) > 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chatcmpl-up","object":"chat.completion","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		frames := []string{
			"data: {\"id\": \"chatcmpl-up\", \"object\": \"chat.completion.chunk\", \"created\": 1, \"model\": \"gpt-4o-mini\", \"choices\": [{\"index\": 0, \"delta\": {\"role\": \"assistant\"}, \"finish_reason\": null}]}\n\n",
			"data: {\"id\": \"chatcmpl-up\", \"object\": \"chat.completion.chunk\", \"created\": 1, \"model\": \"gpt-4o-mini\", \"choices\": [{\"index\": 0, \"delta\": {\"content\": \"hello\"}, \"finish_reason\": null}]}\n\n",
			"data: {\"id\": \"chatcmpl-up\", \"object\": \"chat.completion.chunk\", \"created\": 1, \"model\": \"gpt-4o-mini\", \"choices\": [{\"index\": 0, \"delta\": {}, \"finish_reason\": \"stop\"}]}\n\n",
			"data: {\"id\": \"chatcmpl-up\", \"object\": \"chat.completion.chunk\", \"created\": 1, \"model\": \"gpt-4o-mini\", \"choices\": [], \"usage\": {\"prompt_tokens\": 7, \"completion_tokens\": 3, \"total_tokens\": 10}}\n\n",
			"data: [DONE]\n\n",
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
	status, _, data := postResponsesBridge(t, routerSrv.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"Hi","stream":true}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if !strings.Contains(string(data), "event: response.completed") {
		t.Fatalf("valid terminal missing response.completed: %s", data)
	}
	status, _, data = postResponsesBridge(t, routerSrv.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"Hi"}`, nil)
	if status != 200 {
		t.Fatalf("slot not released after valid terminal without EOF: status=%d body=%s", status, data)
	}
}
