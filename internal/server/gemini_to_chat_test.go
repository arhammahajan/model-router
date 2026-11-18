package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Gemini ingress -> Chat upstream: JSON text translates to a proper
// GenerateContentResponse with the public model version.
func TestGeminiToChatJSONText(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-up","object":"chat.completion","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`))
	}
	status, headers, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-chat:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}],"systemInstruction":{"parts":[{"text":"Be brief."}]},"generationConfig":{"maxOutputTokens":16,"temperature":0.5,"topP":0.9}}`, nil)
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
	if v["modelVersion"] != "gem-chat" {
		t.Fatalf("public model version not restored: %s", data)
	}
	if v["responseId"] != "chatcmpl-up" {
		t.Fatalf("upstream response id not carried: %s", data)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lastPath != "/chat/completions" {
		t.Fatalf("upstream path=%s, want /chat/completions", h.lastPath)
	}
	if h.lastRaw != "" {
		t.Fatalf("no query must travel upstream, got %q", h.lastRaw)
	}
	var wire map[string]any
	if err := json.Unmarshal(h.lastBody, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["model"] != "gpt-4o-mini" {
		t.Fatalf("upstream model=%v", wire["model"])
	}
	msgs, _ := wire["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("upstream messages=%v", wire["messages"])
	}
	if h.lastHdr.Get("Authorization") != "Bearer chat-secret" {
		t.Fatalf("upstream auth wrong: %v", h.lastHdr)
	}
}

// Gemini ingress -> Chat upstream: function tools round-trip with exact
// arguments (large integers exact) and preserved call identity; tool
// results in history ride unchanged.
func TestGeminiToChatToolsExact(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-up","object":"chat.completion","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_up_1","type":"function","function":{"name":"get_time","arguments":"{\"tz\":\"UTC\",\"n\":9007199254740993}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":9,"completion_tokens":2,"total_tokens":11}}`))
	}
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-chat:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}],"tools":[{"functionDeclarations":[{"name":"get_time","parametersJsonSchema":{"type":"object"}}]}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if !strings.Contains(string(data), "9007199254740993") {
		t.Fatalf("large integer not preserved: %s", data)
	}
	if !strings.Contains(string(data), `"id":"call_up_1"`) {
		t.Fatalf("call identity not preserved: %s", data)
	}
	if !strings.Contains(string(data), `"finishReason":"STOP"`) {
		t.Fatalf("tool terminal must be STOP: %s", data)
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

// Gemini ingress -> Chat upstream: SSE framing (?alt=sse) yields
// text/event-stream data-only frames with no [DONE] and no Chat markers.
func TestGeminiToChatSSEText(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-up\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-4o-mini\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hel\"},\"finish_reason\":null}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-up\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-4o-mini\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"},\"finish_reason\":null}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-up\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-4o-mini\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3,\"total_tokens\":10}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, headers, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-chat:streamGenerateContent?alt=sse",
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
	if so, _ := wire["stream_options"].(map[string]any); so["include_usage"] != true {
		t.Fatalf("upstream must force stream_options.include_usage: %s", h.lastBody)
	}
}

// Gemini ingress -> Chat upstream: missing alt yields the incremental JSON
// array with application/json framing and a closing bracket.
func TestGeminiToChatJSONArrayStream(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"},\"finish_reason\":null}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3,\"total_tokens\":10}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, headers, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-chat:streamGenerateContent",
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
	for _, want := range []string{`"text":"a"`, `"finishReason":"STOP"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %s", want, text)
		}
	}
	if strings.Contains(text, "[DONE]") {
		t.Fatalf("Gemini downstream must never contain [DONE]: %s", text)
	}
}

// Gemini ingress -> Chat upstream: strict mode refuses a diagnosed loss
// before any upstream I/O, in the google.rpc.Status shape.
func TestGeminiToChatStrictLoss(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-chat:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}],"safetySettings":[{"category":"HARM_CATEGORY_HARASSMENT","threshold":"BLOCK_NONE"}]}`, nil)
	if status != 400 {
		t.Fatalf("status=%d body=%s, want 400", status, data)
	}
	if !strings.Contains(string(data), "TRANSLATION_LOSS") {
		t.Fatalf("body lacks TRANSLATION_LOSS: %s", data)
	}
	if n := gemCalls(h); n != 0 {
		t.Fatalf("upstream calls=%d, want 0 (pre-I/O refusal)", n)
	}
}

// Gemini ingress -> Chat upstream: router-held state references are refused
// before any upstream I/O.
func TestGeminiToChatStateRefusal(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-chat:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}],"cachedContent":"cachedContents/abc"}`, nil)
	if status != 400 {
		t.Fatalf("status=%d body=%s, want 400", status, data)
	}
	if n := gemCalls(h); n != 0 {
		t.Fatalf("upstream calls=%d, want 0", n)
	}
}

// Gemini ingress -> Chat upstream: malformed source (mutually exclusive
// schema fields) is a 400 with no I/O.
func TestGeminiToChatMalformed(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-chat:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}],"tools":[{"functionDeclarations":[{"name":"get_time","parameters":{"type":"object"},"parametersJsonSchema":{"type":"object"}}]}]}`, nil)
	if status != 400 {
		t.Fatalf("status=%d body=%s, want 400", status, data)
	}
	if !strings.Contains(string(data), "INVALID_REQUEST") {
		t.Fatalf("body lacks INVALID_REQUEST: %s", data)
	}
	if n := gemCalls(h); n != 0 {
		t.Fatalf("upstream calls=%d, want 0", n)
	}
}

// Gemini ingress -> Chat upstream: client credentials (headers and ?key=)
// are auth-only and stripped; the upstream sees only its configured key,
// and no query travels upstream.
func TestGeminiToChatCredentialIsolation(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}
	status, _, _ := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-chat:generateContent?key=client-key&alt=json",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`,
		map[string]string{"Authorization": "Bearer client-key", "X-Goog-Api-Key": "client-key"})
	if status != 200 {
		t.Fatalf("status=%d (?key= must authenticate)", status)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if got := h.lastHdr.Get("Authorization"); got != "Bearer chat-secret" {
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

// Gemini ingress -> Anthropic upstream: now bridged, so a diagnosed loss is
// refused pre-I/O in the Gemini shape (temperature 1.5 has no Anthropic
// representation), never an unsupported_route.
func TestGeminiToAnthropicRefusedBeforeUpstream(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-x:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}],"generationConfig":{"temperature":1.5}}`, nil)
	if status != 400 {
		t.Fatalf("status=%d body=%s, want 400", status, data)
	}
	if !strings.Contains(string(data), "TRANSLATION_LOSS") {
		t.Fatalf("expected translation_loss (routed, not unsupported_route): %s", data)
	}
	if strings.Contains(string(data), "UNSUPPORTED_ROUTE") {
		t.Fatalf("bridged Gemini->Anthropic must not report unsupported_route: %s", data)
	}
	if n := gemCalls(h); n != 0 {
		t.Fatalf("refusal must precede upstream I/O, got %d calls", n)
	}
}

// Gemini ingress -> Chat upstream: an unrepresentable upstream feature is a
// 502 in the Gemini shape with no success marker.
func TestGeminiToChatLateUnsupported(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_up_1","type":"function","function":{"name":"get_time","arguments":"not-json{{{"}}]}},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`))
	}
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-chat:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}],"tools":[{"functionDeclarations":[{"name":"get_time","parametersJsonSchema":{"type":"object"}}]}]}`, nil)
	if status != 502 {
		t.Fatalf("status=%d body=%s, want 502", status, data)
	}
	if strings.Contains(string(data), `"finishReason":"STOP"`) {
		t.Fatalf("no success marker on failure: %s", data)
	}
	if strings.Contains(string(data), "Traceback") {
		t.Fatalf("raw error leaked: %s", data)
	}
}

// Gemini ingress -> Chat upstream: genuine model-malformed tool arguments
// map to a Gemini 200 with MALFORMED_FUNCTION_CALL, never an ordinary STOP
// and never an invalid function rendering.
func TestGeminiToChatMalformedArgsMarker(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_up_1","type":"function","function":{"name":"","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-chat:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s, want 200", status, data)
	}
	if !strings.Contains(string(data), `"finishReason":"MALFORMED_FUNCTION_CALL"`) {
		t.Fatalf("malformed call must surface MALFORMED_FUNCTION_CALL: %s", data)
	}
	if strings.Contains(string(data), `"finishReason":"STOP"`) {
		t.Fatalf("no ordinary STOP for malformed calls: %s", data)
	}
}

// Gemini ingress -> Chat upstream: pre-commit invalid stream is an ordinary
// Gemini 502, never an empty 200.
func TestGeminiToChatEarlyInvalidStream(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"mystery\":1}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-chat:streamGenerateContent?alt=sse",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status != 502 {
		t.Fatalf("status=%d body=%s, want 502", status, data)
	}
	if strings.Contains(string(data), "[DONE]") {
		t.Fatalf("pre-commit failure must not emit [DONE]: %s", data)
	}
	if !strings.Contains(string(data), "UNAVAILABLE") {
		t.Fatalf("pre-commit must be google.rpc shaped: %s", data)
	}
}

// Gemini ingress -> Chat upstream: post-commit SSE truncation keeps 200 and
// terminates with one sanitized data:{error} frame, never [DONE] or an
// invented STOP/usage.
func TestGeminiToChatSSETruncated(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"par\"},\"finish_reason\":null}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Truncate: close without finish/usage/[DONE].
	}
	status, headers, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-chat:streamGenerateContent?alt=sse",
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
	if !strings.Contains(text, "data:") || !strings.Contains(text, `"error"`) {
		t.Fatalf("post-commit SSE failure must emit one sanitized error frame: %s", text)
	}
	for _, absent := range []string{"[DONE]", `"finishReason"`, `"STOP"`, "Traceback"} {
		if strings.Contains(text, absent) {
			t.Fatalf("truncated stream must not contain %q: %s", absent, text)
		}
	}
}

// Gemini ingress -> Chat upstream: post-commit array truncation keeps 200
// and ends without the closing ']' or a terminal object — the client
// detects truncation instead of reading a manufactured success.
func TestGeminiToChatArrayTruncated(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"par\"},\"finish_reason\":null}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, headers, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-chat:streamGenerateContent",
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

// Gemini ingress -> Chat upstream: model text containing framing-like tokens
// is payload, never framing; the valid terminal completes once.
func TestGeminiToChatModelTextIsNotFraming(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"say event: error aloud\"},\"finish_reason\":null}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-chat:streamGenerateContent?alt=sse",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	text := string(data)
	if !strings.Contains(text, "event: error") {
		t.Fatalf("model text payload missing: %s", text)
	}
	if !strings.Contains(text, `"finishReason":"STOP"`) {
		t.Fatalf("valid terminal missing STOP: %s", text)
	}
	// The payload occurrence lives inside a data JSON string, never as its
	// own framed block: exactly one event framing per emitted object.
	events := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "event:") {
			events++
		}
	}
	if events != 0 {
		t.Fatalf("Gemini SSE downstream must be data-only frames, got %d event: lines: %s", events, text)
	}
}

// Gemini ingress -> Chat upstream: a stalled upstream body past the absolute
// deadline is a pre-commit 504 (never an empty 200), the upstream body is
// closed promptly, and the generation slot is recovered.
func TestGeminiToChatPrecommitTimeout504(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiTightCfg(t, 1))
	closed := make(chan struct{})
	var closeOnce atomic.Bool
	var calls atomic.Int32
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
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
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-chat:streamGenerateContent?alt=sse",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status != 504 {
		t.Fatalf("status=%d body=%s, want 504", status, data)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatalf("stalled upstream was not closed after pre-commit 504")
	}
	status, _, data = postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-chat:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status != 200 {
		t.Fatalf("slot not recovered after 504: status=%d body=%s", status, data)
	}
}

// Gemini ingress -> Chat upstream: a downstream flush failure cancels the
// upstream body promptly and releases the generation slot; no raw write is
// attempted after the downstream failed.
func TestGeminiToChatFlushFailureCancelsUpstream(t *testing.T) {
	cfg := geminiTightCfg(t, 30)
	var n atomic.Int32
	closed := make(chan struct{})
	var closeOnce atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) > 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		// A committing prefix: a content delta forces the translator to
		// commit downstream. The body is then held open; the flush failure
		// must still terminate promptly via the synchronous upstream close.
		_, _ = w.Write([]byte("data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"par\"},\"finish_reason\":null}]}\n\n"))
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
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gem-chat:streamGenerateContent?alt=sse", strings.NewReader(body))
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
	status, _, data := postGeminiBridge(t, routerSrv.URL+"/v1beta/models/gem-chat:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status == 503 {
		t.Fatalf("generation slot leaked after flush failure (body %s)", data)
	}
	if status != 200 {
		t.Fatalf("second request after flush failure: status=%d body=%s", status, data)
	}
}

// Gemini ingress -> Chat upstream: a valid terminal returns promptly without
// waiting for EOF, the upstream body is closed, and the slot is recovered.
func TestGeminiToChatPromptReturnReleasesSlot(t *testing.T) {
	cfg := geminiTightCfg(t, 30)
	var n atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) > 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hel\"},\"finish_reason\":null}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3,\"total_tokens\":10}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
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
	status, _, data := postGeminiBridge(t, routerSrv.URL+"/v1beta/models/gem-chat:streamGenerateContent?alt=sse",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if !strings.Contains(string(data), `"finishReason":"STOP"`) {
		t.Fatalf("valid terminal missing STOP: %s", data)
	}
	status, _, data = postGeminiBridge(t, routerSrv.URL+"/v1beta/models/gem-chat:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status != 200 {
		t.Fatalf("slot not released after valid terminal without EOF: status=%d body=%s", status, data)
	}
}
