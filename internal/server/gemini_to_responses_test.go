package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const grResponsesJSONText = `{"id":"resp_up","object":"response","created_at":1,"model":"gpt-4.1","status":"completed","error":null,"incomplete_details":null,"output":[{"id":"msg_up","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hello","annotations":[]}]}],"usage":{"input_tokens":7,"input_tokens_details":{"cached_tokens":2},"output_tokens":3,"output_tokens_details":{"reasoning_tokens":1},"total_tokens":10}}`

const grResponsesUsage = `{"input_tokens":7,"input_tokens_details":{"cached_tokens":2},"output_tokens":3,"output_tokens_details":{"reasoning_tokens":1},"total_tokens":10}`

// grResponsesSSEText builds a minimal valid Responses SSE text stream with
// the given terminal sequence number and response id.
func grResponsesSSEText(seq int, respID string) string {
	var b strings.Builder
	ev := func(name, data string) {
		b.WriteString("event: " + name + "\ndata: " + data + "\n\n")
	}
	ev("response.created", fmt.Sprintf(`{"type":"response.created","sequence_number":0,"response":{"id":%q}}`, respID))
	ev("response.in_progress", fmt.Sprintf(`{"type":"response.in_progress","sequence_number":1,"response":{"id":%q}}`, respID))
	ev("response.output_item.added", `{"type":"response.output_item.added","sequence_number":2,"output_index":0,"item":{"type":"message","id":"msg_1","status":"in_progress","role":"assistant","content":[]}}`)
	ev("response.content_part.added", `{"type":"response.content_part.added","sequence_number":3,"output_index":0,"item_id":"msg_1","content_index":0,"part":{"type":"output_text","text":"","annotations":[],"logprobs":[]}}`)
	ev("response.output_text.delta", `{"type":"response.output_text.delta","sequence_number":4,"output_index":0,"item_id":"msg_1","content_index":0,"delta":"hel","logprobs":[]}`)
	ev("response.output_text.delta", `{"type":"response.output_text.delta","sequence_number":5,"output_index":0,"item_id":"msg_1","content_index":0,"delta":"lo","logprobs":[]}`)
	ev("response.output_text.done", `{"type":"response.output_text.done","sequence_number":6,"output_index":0,"item_id":"msg_1","content_index":0,"text":"hello","logprobs":[]}`)
	ev("response.content_part.done", `{"type":"response.content_part.done","sequence_number":7,"output_index":0,"item_id":"msg_1","content_index":0,"part":{"type":"output_text","text":"hello","annotations":[],"logprobs":[]}}`)
	ev("response.output_item.done", `{"type":"response.output_item.done","sequence_number":8,"output_index":0,"item":{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hello","annotations":[],"logprobs":[]}]}}`)
	ev("response.completed", fmt.Sprintf(`{"type":"response.completed","sequence_number":%d,"response":{"id":%q,"object":"response","created_at":1746989954.0,"model":"gpt-x","status":"completed","error":null,"incomplete_details":null,"output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hello","annotations":[],"logprobs":[]}]}],"usage":%s}}`, seq, respID, grResponsesUsage))
	return b.String()
}

// grResponsesSSECustom builds a valid Responses SSE text stream for an
// arbitrary full text split into the given delta parts.
func grResponsesSSECustom(seq int, respID, itemID, full string, parts []string) string {
	var b strings.Builder
	ev := func(name, data string) {
		b.WriteString("event: " + name + "\ndata: " + data + "\n\n")
	}
	q, _ := json.Marshal(full)
	quoted := string(q)
	ev("response.created", fmt.Sprintf(`{"type":"response.created","sequence_number":0,"response":{"id":%q}}`, respID))
	ev("response.in_progress", fmt.Sprintf(`{"type":"response.in_progress","sequence_number":1,"response":{"id":%q}}`, respID))
	ev("response.output_item.added", fmt.Sprintf(`{"type":"response.output_item.added","sequence_number":2,"output_index":0,"item":{"type":"message","id":%q,"status":"in_progress","role":"assistant","content":[]}}`, itemID))
	ev("response.content_part.added", fmt.Sprintf(`{"type":"response.content_part.added","sequence_number":3,"output_index":0,"item_id":%q,"content_index":0,"part":{"type":"output_text","text":"","annotations":[],"logprobs":[]}}`, itemID))
	n := 4
	for _, p := range parts {
		pq, _ := json.Marshal(p)
		ev("response.output_text.delta", fmt.Sprintf(`{"type":"response.output_text.delta","sequence_number":%d,"output_index":0,"item_id":%q,"content_index":0,"delta":%s,"logprobs":[]}`, n, itemID, string(pq)))
		n++
	}
	ev("response.output_text.done", fmt.Sprintf(`{"type":"response.output_text.done","sequence_number":%d,"output_index":0,"item_id":%q,"content_index":0,"text":%s,"logprobs":[]}`, n, itemID, quoted))
	n++
	ev("response.content_part.done", fmt.Sprintf(`{"type":"response.content_part.done","sequence_number":%d,"output_index":0,"item_id":%q,"content_index":0,"part":{"type":"output_text","text":%s,"annotations":[],"logprobs":[]}}`, n, itemID, quoted))
	n++
	ev("response.output_item.done", fmt.Sprintf(`{"type":"response.output_item.done","sequence_number":%d,"output_index":0,"item":{"type":"message","id":%q,"status":"completed","role":"assistant","content":[{"type":"output_text","text":%s,"annotations":[],"logprobs":[]}]}}`, n, itemID, quoted))
	n++
	ev("response.completed", fmt.Sprintf(`{"type":"response.completed","sequence_number":%d,"response":{"id":%q,"object":"response","created_at":1746989954.0,"model":"gpt-x","status":"completed","error":null,"incomplete_details":null,"output":[{"type":"message","id":%q,"status":"completed","role":"assistant","content":[{"type":"output_text","text":%s,"annotations":[],"logprobs":[]}]}],"usage":%s}}`, seq, respID, itemID, quoted, grResponsesUsage))
	return b.String()
}

// Gemini ingress -> Responses upstream: JSON text translates to a proper
// GenerateContentResponse with the public model version.
func TestGeminiToResponsesJSONText(t *testing.T) {
	h := newRGBridgeHarness(t, rgBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(grResponsesJSONText))
	}
	status, headers, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-resp:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}],"generationConfig":{"temperature":0.5,"maxOutputTokens":16,"topP":0.9}}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if ct := headers.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("downstream content-type=%q, want application/json", ct)
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if v["modelVersion"] != "gem-resp" {
		t.Fatalf("public model version not restored: %s", data)
	}
	if v["responseId"] != "resp_up" {
		t.Fatalf("upstream response id not carried: %s", data)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lastPath != "/responses" {
		t.Fatalf("upstream path=%s, want /responses", h.lastPath)
	}
	if h.lastRaw != "" {
		t.Fatalf("no query must travel upstream, got %q", h.lastRaw)
	}
	var wire map[string]any
	if err := json.Unmarshal(h.lastBody, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["model"] != "gpt-4.1" {
		t.Fatalf("upstream model=%v", wire["model"])
	}
	if wire["store"] != false {
		t.Fatalf("upstream must force store:false, got %v", wire["store"])
	}
	for _, k := range []string{"alt", "key"} {
		if _, has := wire[k]; has {
			t.Fatalf("wire must not carry %q", k)
		}
	}
	if h.lastHdr.Get("Authorization") != "Bearer resp-secret" {
		t.Fatalf("upstream auth wrong: %v", h.lastHdr)
	}
}

// Gemini ingress -> Responses upstream: function tools round-trip with exact
// arguments (large integers exact) and preserved opaque call identity.
func TestGeminiToResponsesToolsExact(t *testing.T) {
	h := newRGBridgeHarness(t, rgBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_up","object":"response","created_at":1,"model":"gpt-4.1","status":"completed","error":null,"incomplete_details":null,"output":[{"type":"function_call","id":"fc_9","call_id":"call-opaque-1","name":"calc","arguments":"{\"n\":9007199254740993}","status":"completed"}],"usage":` + grResponsesUsage + `}`))
	}
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-resp:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}],"tools":[{"functionDeclarations":[{"name":"calc","parametersJsonSchema":{"type":"object"}}]}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if !strings.Contains(string(data), "9007199254740993") {
		t.Fatalf("large integer not preserved: %s", data)
	}
	if !strings.Contains(string(data), `"id":"call-opaque-1"`) {
		t.Fatalf("call identity not preserved: %s", data)
	}
	if strings.Contains(string(data), "fc_9") {
		t.Fatalf("item id must not leak into the call identity: %s", data)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var wire map[string]any
	if err := json.Unmarshal(h.lastBody, &wire); err != nil {
		t.Fatal(err)
	}
	tools, _ := wire["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("upstream tools=%v", wire["tools"])
	}
}

// Gemini ingress -> Responses upstream: SSE framing (?alt=sse) yields
// text/event-stream data-only frames with no [DONE] and no Chat markers.
func TestGeminiToResponsesSSEText(t *testing.T) {
	h := newRGBridgeHarness(t, rgBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, grResponsesSSEText(9, "resp_1"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, headers, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-resp:streamGenerateContent?alt=sse",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if ct := headers.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("SSE framing must be text/event-stream, got %q", ct)
	}
	text := string(data)
	for _, want := range []string{`"text":"hel"`, `"text":"lo"`, `"finishReason":"STOP"`, `"promptTokenCount":7`} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %s", want, text)
		}
	}
	for _, absent := range []string{"[DONE]", "Traceback", "chat.completion.chunk"} {
		if strings.Contains(text, absent) {
			t.Fatalf("SSE downstream must not contain %q: %s", absent, text)
		}
	}
	if !strings.Contains(text, `"modelVersion":"gem-resp"`) {
		t.Fatalf("public model version missing: %s", text)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lastRaw != "" {
		t.Fatalf("alt/key must never travel upstream, got %q", h.lastRaw)
	}
	var wire map[string]any
	if err := json.Unmarshal(h.lastBody, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["stream"] != true {
		t.Fatalf("upstream must stream: %s", h.lastBody)
	}
	if wire["store"] != false {
		t.Fatalf("upstream must force store:false: %s", h.lastBody)
	}
}

// Gemini ingress -> Responses upstream: missing alt yields the incremental
// JSON array with application/json framing and a closing bracket.
func TestGeminiToResponsesJSONArrayStream(t *testing.T) {
	h := newRGBridgeHarness(t, rgBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, grResponsesSSEText(9, "resp_1"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, headers, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-resp:streamGenerateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if ct := headers.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("array framing must be application/json, got %q", ct)
	}
	text := strings.TrimSpace(string(data))
	if !strings.HasPrefix(text, "[") || !strings.HasSuffix(text, "]") {
		t.Fatalf("array must open with [ and close with ]: %s", text)
	}
	for _, want := range []string{`"text":"hel"`, `"finishReason":"STOP"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %s", want, text)
		}
	}
	if strings.Contains(text, "[DONE]") {
		t.Fatalf("Gemini downstream must never contain [DONE]: %s", text)
	}
}

// Gemini ingress -> Responses upstream: strict mode refuses a diagnosed
// loss before any upstream I/O, in the google.rpc.Status shape.
func TestGeminiToResponsesStrictLoss(t *testing.T) {
	h := newRGBridgeHarness(t, rgBridgeCfg(t))
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-resp:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}],"generationConfig":{"stopSequences":["a"]}}`, nil)
	if status != 400 {
		t.Fatalf("status=%d body=%s, want 400", status, data)
	}
	if !strings.Contains(string(data), "TRANSLATION_LOSS") {
		t.Fatalf("body lacks TRANSLATION_LOSS: %s", data)
	}
	if n := rgCalls(h); n != 0 {
		t.Fatalf("upstream calls=%d, want 0 (pre-I/O refusal)", n)
	}
}

// Gemini ingress -> Responses upstream: lossy mode carries the diagnosed
// loss in the header instead of refusing.
func TestGeminiToResponsesLossyHeader(t *testing.T) {
	h := newRGBridgeHarness(t, rgBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(grResponsesJSONText))
	}
	status, headers, _ := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-resp-lossy:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}],"generationConfig":{"stopSequences":["a"]}}`, nil)
	if status != 200 {
		t.Fatalf("status=%d, want 200", status)
	}
	if headers.Get("X-Model-Router-Loss") == "" {
		t.Fatalf("lossy must carry loss header")
	}
}

// Gemini ingress -> Responses upstream: router-held state references are
// refused before any upstream I/O.
func TestGeminiToResponsesStateRefusal(t *testing.T) {
	h := newRGBridgeHarness(t, rgBridgeCfg(t))
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-resp:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}],"cachedContent":"cachedContents/abc"}`, nil)
	if status != 400 {
		t.Fatalf("status=%d body=%s, want 400", status, data)
	}
	if n := rgCalls(h); n != 0 {
		t.Fatalf("upstream calls=%d, want 0", n)
	}
}

// Gemini ingress -> Responses upstream: malformed source is a 400 with no I/O.
func TestGeminiToResponsesMalformed(t *testing.T) {
	h := newRGBridgeHarness(t, rgBridgeCfg(t))
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-resp:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}],"tools":[{"functionDeclarations":[{"name":"get_time","parameters":{"type":"object"},"parametersJsonSchema":{"type":"object"}}]}]}`, nil)
	if status != 400 {
		t.Fatalf("status=%d body=%s, want 400", status, data)
	}
	if n := rgCalls(h); n != 0 {
		t.Fatalf("upstream calls=%d, want 0", n)
	}
}

// Gemini ingress -> Responses upstream: client credentials (headers and
// ?key=) are auth-only and stripped; no query travels upstream.
func TestGeminiToResponsesCredentialIsolation(t *testing.T) {
	h := newRGBridgeHarness(t, rgBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(grResponsesJSONText))
	}
	status, _, _ := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-resp:generateContent?key=client-key&alt=json",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`,
		map[string]string{"Authorization": "Bearer client-key", "X-Goog-Api-Key": "client-key"})
	if status != 200 {
		t.Fatalf("status=%d (?key= must authenticate)", status)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if got := h.lastHdr.Get("Authorization"); got != "Bearer resp-secret" {
		t.Fatalf("upstream Authorization=%q, want configured key", got)
	}
	for _, k := range []string{"X-Goog-Api-Key", "X-Api-Key"} {
		if got := h.lastHdr.Get(k); got != "" {
			t.Fatalf("client credential leaked upstream as %s=%q", k, got)
		}
	}
	if h.lastRaw != "" {
		t.Fatalf("query must never travel upstream, got %q", h.lastRaw)
	}
}

// Gemini ingress -> Responses upstream: an unrepresentable upstream feature
// is a 502 in the Gemini shape with no success marker.
func TestGeminiToResponsesLateUnsupported(t *testing.T) {
	h := newRGBridgeHarness(t, rgBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"r","object":"response","created_at":1,"model":"m","status":"failed","error":null,"incomplete_details":null,"output":[],"usage":` + grResponsesUsage + `}`))
	}
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-resp:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status != 502 {
		t.Fatalf("status=%d body=%s, want 502", status, data)
	}
	if strings.Contains(string(data), `"finishReason":"STOP"`) {
		t.Fatalf("no success marker on failure: %s", data)
	}
}

// Gemini ingress -> Responses upstream: translated JSON past MaxBodyBytes
// is a sanitized pre-commit 502.
func TestGeminiToResponsesTranslatedJSONBodyBound(t *testing.T) {
	cfg := rgBridgeCfg(t)
	cfg.Server.MaxBodyBytes = 512
	h := newRGBridgeHarness(t, cfg)
	source := strings.Repeat("<", 80)
	upstream := fmt.Sprintf(`{"id":"resp_up","object":"response","created_at":1,"model":"gpt-4.1","status":"completed","error":null,"incomplete_details":null,"output":[{"id":"msg_up","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"%s","annotations":[]}]}],"usage":%s}`, source, grResponsesUsage)
	if len(upstream) > 512 {
		t.Fatal("fixture must fit the upstream body bound")
	}
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, upstream)
	}
	status, _, body := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-resp:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status != 502 {
		t.Fatalf("expanded response status = %d, want 502 (body bytes %d)", status, len(body))
	}
	text := string(body)
	if !strings.Contains(text, "Upstream response exceeds the size bound") {
		t.Fatalf("error body missing sanitized bound message: %s", text)
	}
	if !strings.Contains(text, "UPSTREAM_RESPONSE_INVALID") {
		t.Fatalf("error body missing router code: %s", text)
	}
	if strings.Contains(text, source) || strings.Contains(text, "<") {
		t.Fatalf("error body leaks source bytes: %s", text)
	}
}

// Gemini ingress -> Responses upstream: safe retry hints cross on the JSON
// path, never raw upstream bodies.
func TestGeminiToResponsesRetryHints(t *testing.T) {
	h := newRGBridgeHarness(t, rgBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "2")
		w.Header().Set("X-RateLimit-Remaining", "5")
		_, _ = w.Write([]byte(grResponsesJSONText))
	}
	status, headers, _ := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-resp:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
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

// Gemini ingress -> Responses upstream: a translated provider 504 keeps its
// status through the sanitized envelope.
func TestGeminiToResponsesProvider504(t *testing.T) {
	h := newRGBridgeHarness(t, rgBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(504)
		_, _ = w.Write([]byte(`{"error":"slow"}`))
	}
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-resp:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status != 504 {
		t.Fatalf("status=%d body=%s, want 504", status, data)
	}
	if strings.Contains(string(data), "slow") {
		t.Fatalf("raw upstream body leaked: %s", data)
	}
}

// Gemini ingress -> Responses upstream: pre-commit invalid stream is an
// ordinary Gemini 502, never an empty 200.
func TestGeminiToResponsesEarlyInvalidStream(t *testing.T) {
	h := newRGBridgeHarness(t, rgBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.failed\ndata: {\"type\":\"response.failed\"}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-resp:streamGenerateContent?alt=sse",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status != 502 {
		t.Fatalf("status=%d body=%s, want 502", status, data)
	}
	if !strings.Contains(string(data), "UNAVAILABLE") {
		t.Fatalf("pre-commit must be google.rpc shaped: %s", data)
	}
}

// Gemini ingress -> Responses upstream: post-commit SSE truncation keeps 200
// and terminates with one sanitized data:{error} frame, never [DONE] or an
// invented STOP/usage.
func TestGeminiToResponsesSSETruncated(t *testing.T) {
	h := newRGBridgeHarness(t, rgBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Committing prefix: a created envelope plus an added item/part
		// and one text delta. Then truncate without the terminal
		// response.completed.
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_1\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"sequence_number\":1,\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"msg_1\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]}}\n\n")
		_, _ = io.WriteString(w, "event: response.content_part.added\ndata: {\"type\":\"response.content_part.added\",\"sequence_number\":2,\"output_index\":0,\"item_id\":\"msg_1\",\"content_index\":0,\"part\":{\"type\":\"output_text\",\"text\":\"\",\"annotations\":[],\"logprobs\":[]}}\n\n")
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":3,\"output_index\":0,\"item_id\":\"msg_1\",\"content_index\":0,\"delta\":\"par\",\"logprobs\":[]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, headers, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-resp:streamGenerateContent?alt=sse",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s, want 200 (headers committed)", status, data)
	}
	if ct := headers.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type=%q", ct)
	}
	text := string(data)
	if !strings.Contains(text, `"text":"par"`) {
		t.Fatalf("committed delta missing: %s", text)
	}
	if strings.Count(text, `"error"`) != 1 {
		t.Fatalf("post-commit SSE failure must emit exactly one sanitized error frame: %s", text)
	}
	for _, absent := range []string{"[DONE]", `"finishReason"`, `"STOP"`, "Traceback"} {
		if strings.Contains(text, absent) {
			t.Fatalf("truncated stream must not contain %q: %s", absent, text)
		}
	}
}

// Gemini ingress -> Responses upstream: post-commit array truncation keeps
// 200 and ends without the closing ']' or a terminal object.
func TestGeminiToResponsesArrayTruncated(t *testing.T) {
	h := newRGBridgeHarness(t, rgBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_1\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"sequence_number\":1,\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"msg_1\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]}}\n\n")
		_, _ = io.WriteString(w, "event: response.content_part.added\ndata: {\"type\":\"response.content_part.added\",\"sequence_number\":2,\"output_index\":0,\"item_id\":\"msg_1\",\"content_index\":0,\"part\":{\"type\":\"output_text\",\"text\":\"\",\"annotations\":[],\"logprobs\":[]}}\n\n")
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":3,\"output_index\":0,\"item_id\":\"msg_1\",\"content_index\":0,\"delta\":\"par\",\"logprobs\":[]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, headers, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-resp:streamGenerateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s, want 200 (headers committed)", status, data)
	}
	if ct := headers.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type=%q", ct)
	}
	text := strings.TrimSpace(string(data))
	if !strings.HasPrefix(text, "[") {
		t.Fatalf("array must open with [: %s", text)
	}
	if strings.HasSuffix(text, "]") {
		t.Fatalf("truncated array must not close with ]: %s", text)
	}
	for _, absent := range []string{`"finishReason"`, "[DONE]", "Traceback"} {
		if strings.Contains(text, absent) {
			t.Fatalf("truncated array must not contain %q: %s", absent, text)
		}
	}
}

// Gemini ingress -> Responses upstream: model text containing framing-like
// tokens is payload, never framing; the valid terminal completes once.
func TestGeminiToResponsesModelTextIsNotFraming(t *testing.T) {
	h := newRGBridgeHarness(t, rgBridgeCfg(t))
	full := "say event: error aloud"
	stream := grResponsesSSECustom(9, "resp_1", "msg_1", full, []string{"say event: ", "error aloud"})
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stream)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-resp:streamGenerateContent?alt=sse",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	text := string(data)
	var joined strings.Builder
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var frame struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame); err != nil {
			t.Fatal(err)
		}
		for _, candidate := range frame.Candidates {
			for _, part := range candidate.Content.Parts {
				joined.WriteString(part.Text)
			}
		}
	}
	if joined.String() != full {
		t.Fatalf("joined model text = %q, want %q", joined.String(), full)
	}
	if !strings.Contains(text, `"finishReason":"STOP"`) {
		t.Fatalf("valid terminal missing STOP: %s", text)
	}
	if c := strings.Count(text, `"error":{"code"`); c != 0 {
		t.Fatalf("model text must not trigger a framed error (%d): %s", c, text)
	}
}

// Gemini ingress -> Responses upstream: a stalled upstream body past the
// absolute deadline is a pre-commit 504, the body is closed, and the slot
// is recovered.
func TestGeminiToResponsesPrecommitTimeout504(t *testing.T) {
	h := newRGBridgeHarness(t, rgTightCfg(t, 1))
	closed := make(chan struct{})
	var closeOnce atomic.Bool
	var calls atomic.Int32
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(grResponsesJSONText))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-r.Context().Done():
			closeOnce.Store(true)
			close(closed)
		case <-time.After(30 * time.Second):
		}
	}
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-resp:streamGenerateContent?alt=sse",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status != 504 {
		t.Fatalf("status=%d body=%s, want 504", status, data)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatalf("stalled upstream was not closed after pre-commit 504")
	}
	status, _, data = postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-resp:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status != 200 {
		t.Fatalf("slot not recovered after 504: status=%d body=%s", status, data)
	}
}

// Gemini ingress -> Responses upstream: a downstream flush failure cancels
// the upstream body promptly and releases the slot; no raw write is
// attempted after the downstream failed.
func TestGeminiToResponsesFlushFailureCancelsUpstream(t *testing.T) {
	cfg := rgTightCfg(t, 30)
	var n atomic.Int32
	closed := make(chan struct{})
	var closeOnce atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) > 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(grResponsesJSONText))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_1\"}}\n\n" +
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":1,\"output_index\":0,\"item_id\":\"msg_1\",\"content_index\":0,\"delta\":\"par\",\"logprobs\":[]}\n\n"))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			if closeOnce.CompareAndSwap(false, true) {
				close(closed)
			}
		case <-time.After(30 * time.Second):
		}
	}))
	defer up.Close()
	for _, prov := range cfg.Providers {
		prov.BaseURL = up.URL
	}
	srv := New(cfg)
	body := `{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gem-resp:streamGenerateContent?alt=sse", strings.NewReader(body))
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
	status, _, data := postGeminiBridge(t, routerSrv.URL+"/v1beta/models/gem-resp:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status == 503 {
		t.Fatalf("generation slot leaked after flush failure (body %s)", data)
	}
	if status != 200 {
		t.Fatalf("second request after flush failure: status=%d body=%s", status, data)
	}
}

// Gemini ingress -> Responses upstream: a valid terminal returns promptly
// without waiting for EOF, the upstream body is closed, and the slot is
// recovered.
func TestGeminiToResponsesPromptReturnReleasesSlot(t *testing.T) {
	cfg := rgTightCfg(t, 30)
	var n atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) > 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(grResponsesJSONText))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, grResponsesSSEText(9, "resp_1"))
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
	status, _, data := postGeminiBridge(t, routerSrv.URL+"/v1beta/models/gem-resp:streamGenerateContent?alt=sse",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if !strings.Contains(string(data), `"finishReason":"STOP"`) {
		t.Fatalf("valid terminal missing STOP: %s", data)
	}
	status, _, data = postGeminiBridge(t, routerSrv.URL+"/v1beta/models/gem-resp:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status != 200 {
		t.Fatalf("slot not released after valid terminal without EOF: status=%d body=%s", status, data)
	}
}
