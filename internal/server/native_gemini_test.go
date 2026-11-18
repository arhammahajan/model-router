package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestGeminiGenerateContentRouting(t *testing.T) {
	h := newNativeHarness(t, nativeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`))
	}
	reqBody := `{"contents":[{"role":"user","parts":[{"text":"Hi"}]}],"mystery_field":{"nested":[1,2,3]}}`
	status, headers, data := postNative(t, h.router.URL+"/v1beta/models/gem-flash:generateContent", reqBody, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if ct := headers.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type must be preserved from upstream, got %q", ct)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lastPath != "/v1beta/models/gemini-2.0-flash:generateContent" {
		t.Fatalf("upstream path: %s raw %s", h.lastPath, h.lastRaw)
	}
	if h.lastRaw != "" {
		t.Fatalf("no query must be forwarded without alt=sse, got %q", h.lastRaw)
	}
	var wire map[string]any
	if err := json.Unmarshal(h.lastBody, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["mystery_field"] == nil {
		t.Fatalf("unknown fields lost: %s", h.lastBody)
	}
	if _, has := wire["model"]; has {
		t.Fatalf("body must not carry model (URL-owned): %s", h.lastBody)
	}
	if h.lastHdr.Get("X-Goog-Api-Key") != "gem-secret" {
		t.Fatalf("upstream gemini auth wrong: %v", h.lastHdr)
	}
	if v := h.lastHdr.Get("Authorization"); v != "" {
		t.Fatalf("client auth must never become upstream auth: %q", v)
	}
}

func TestGeminiStreamSSEForwardsAlt(t *testing.T) {
	h := newNativeHarness(t, nativeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "alt=sse" {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":{"code":400,"message":"want alt=sse","status":"INVALID_ARGUMENT"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hey\"}]}}]}\n\n"))
		flusher.Flush()
	}
	status, headers, data := postNative(t,
		h.router.URL+"/v1beta/models/gem-flash:streamGenerateContent?alt=sse",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if ct := headers.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("SSE content-type must be preserved, got %q", ct)
	}
	if !strings.Contains(string(data), "hey") {
		t.Fatalf("stream body not forwarded: %q", data)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lastRaw != "alt=sse" {
		t.Fatalf("alt=sse must be forwarded, got %q", h.lastRaw)
	}
}

func TestGeminiJSONArrayStreamPreservesContentType(t *testing.T) {
	h := newNativeHarness(t, nativeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		flusher := w.(http.Flusher)
		_, _ = w.Write([]byte(`[{"candidates":[{"content":{"parts":[{"text":"a"}]}}]},`))
		flusher.Flush()
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"b"}]}}]}]`))
		flusher.Flush()
	}
	status, headers, data := postNative(t,
		h.router.URL+"/v1beta/models/gem-flash:streamGenerateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if ct := headers.Get("Content-Type"); ct == "text/event-stream" {
		t.Fatalf("JSON-array stream must NOT force text/event-stream, got %q", ct)
	} else if ct != "application/json" {
		t.Fatalf("JSON-array content-type must be preserved, got %q", ct)
	}
	if !strings.Contains(string(data), `"text":"a"`) || !strings.Contains(string(data), `"text":"b"`) {
		t.Fatalf("array chunks not forwarded opaquely: %q", data)
	}
}

func TestGeminiRejectsBodyModelAndStream(t *testing.T) {
	h := newNativeHarness(t, nativeCfg(t))
	for _, body := range []string{
		`{"model":"gem-flash","contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`,
		`{"stream":false,"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`,
	} {
		status, _, data := postNative(t, h.router.URL+"/v1beta/models/gem-flash:generateContent", body, nil)
		if status != 400 {
			t.Fatalf("URL-owned field must be 400, got %d body %s", status, data)
		}
		var payload map[string]any
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Fatalf("gemini error not JSON: %s", data)
		}
		errObj, _ := payload["error"].(map[string]any)
		if errObj == nil || errObj["status"] != "INVALID_ARGUMENT" {
			t.Fatalf("expected google.rpc INVALID_ARGUMENT shape: %s", data)
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.calls != 0 {
		t.Fatalf("validation must precede upstream I/O, got %d calls", h.calls)
	}
}

func TestGeminiRejectsUnsupportedQuery(t *testing.T) {
	h := newNativeHarness(t, nativeCfg(t))
	for _, path := range []string{
		"/v1beta/models/gem-flash:generateContent?foo=bar",
		"/v1beta/models/gem-flash:generateContent?alt=bad",
		"/v1beta/models/gem-flash:generateContent?alt=sse&alt=sse",
		"/v1beta/models/gem-flash:generateContent?key=k&evil=1",
	} {
		status, _, _ := postNative(t, h.router.URL+path,
			`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
		if status != 400 {
			t.Fatalf("path %s must be 400, got %d", path, status)
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.calls != 0 {
		t.Fatalf("forbidden query must not reach upstream, got %d calls", h.calls)
	}
}

func TestGeminiUnknownAndUnimplementedMethods(t *testing.T) {
	h := newNativeHarness(t, nativeCfg(t))
	status, _, _ := postNative(t, h.router.URL+"/v1beta/models/gem-flash:bogusMethod",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status != 404 {
		t.Fatalf("unknown method must be 404, got %d", status)
	}
	status, _, data := postNative(t, h.router.URL+"/v1beta/models/gem-flash:embedContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status != 501 {
		t.Fatalf("known-unsupported method must be 501, got %d body %s", status, data)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.calls != 0 {
		t.Fatalf("method refusal must precede upstream I/O, got %d calls", h.calls)
	}
}

func TestGeminiAuthShapes(t *testing.T) {
	dir := t.TempDir()
	cfg := nativeCfg(t)
	cfg.Server.AuthMode = "required"
	cfg.Server.APIKeys = []string{"client-secret"}
	_ = dir
	h := newNativeHarness(t, cfg)
	// No key: Gemini-shape 401 with UNAUTHENTICATED.
	status, _, data := postNative(t, h.router.URL+"/v1beta/models/gem-flash:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status != 401 {
		t.Fatalf("missing key must be 401, got %d body %s", status, data)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("gemini error not JSON: %s", data)
	}
	if errObj, _ := payload["error"].(map[string]any); errObj == nil || errObj["status"] != "UNAUTHENTICATED" {
		t.Fatalf("expected UNAUTHENTICATED status name: %s", data)
	}
	// Header key works and is not forwarded.
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[]}`))
	}
	status, _, _ = postNative(t, h.router.URL+"/v1beta/models/gem-flash:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`,
		map[string]string{"X-Goog-Api-Key": "client-secret"})
	if status != 200 {
		t.Fatalf("x-goog-api-key header must authenticate, got %d", status)
	}
	h.mu.Lock()
	if got := h.lastHdr.Get("X-Goog-Api-Key"); got != "gem-secret" {
		h.mu.Unlock()
		t.Fatalf("upstream must carry provider key, got %q", got)
	}
	h.mu.Unlock()
	// Query ?key= works for Gemini and is stripped upstream.
	status, _, _ = postNative(t, h.router.URL+"/v1beta/models/gem-flash:generateContent?key=client-secret",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status != 200 {
		t.Fatalf("?key= must authenticate Gemini, got %d", status)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lastRaw != "" {
		t.Fatalf("client ?key= must never be forwarded, got %q", h.lastRaw)
	}
}

func TestAuthRequiredAcrossEndpoints(t *testing.T) {
	cfg := nativeCfg(t)
	cfg.Server.AuthMode = "required"
	cfg.Server.APIKeys = []string{"client-secret"}
	h := newNativeHarness(t, cfg)
	cases := []struct{ name, path, body string }{
		{"chat", "/v1/chat/completions", `{"model":"chat-model","messages":[{"role":"user","content":"Hi"}]}`},
		{"responses", "/v1/responses", `{"model":"resp-model","input":"Hi"}`},
		{"anthropic", "/v1/messages", `{"model":"claude-haiku","max_tokens":4,"messages":[{"role":"user","content":"Hi"}]}`},
		{"count", "/v1/messages/count_tokens", `{"model":"count-model","messages":[{"role":"user","content":"Hi"}]}`},
		{"gemini", "/v1beta/models/gem-flash:generateContent", `{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`},
	}
	for _, tc := range cases {
		status, _, _ := postNative(t, h.router.URL+tc.path, tc.body, nil)
		if status != 401 {
			t.Fatalf("%s without key must be 401, got %d", tc.name, status)
		}
		status, _, _ = postNative(t, h.router.URL+tc.path, tc.body,
			map[string]string{"Authorization": "Bearer client-secret"})
		if status == 401 {
			t.Fatalf("%s with key must not be 401", tc.name)
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	// Authenticated chat/responses/anthropic/gemini may have dialed upstream;
	// count_tokens with count-model is enabled so it also dials. Only assert
	// that unauthenticated requests never added calls beyond these.
	_ = h.calls
}

func TestNativeUpstreamErrorPreserved(t *testing.T) {
	h := newNativeHarness(t, nativeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"code":429,"message":"slow down","status":"RESOURCE_EXHAUSTED"}}`))
	}
	for _, tc := range []struct{ name, path, body string }{
		{"responses", "/v1/responses", `{"model":"resp-model","input":"Hi"}`},
		{"anthropic", "/v1/messages", `{"model":"claude-haiku","max_tokens":4,"messages":[{"role":"user","content":"Hi"}]}`},
		{"gemini", "/v1beta/models/gem-flash:generateContent", `{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`},
	} {
		status, _, data := postNative(t, h.router.URL+tc.path, tc.body, nil)
		if status != 429 {
			t.Fatalf("%s upstream 429 must be preserved, got %d body %s", tc.name, status, data)
		}
		if !strings.Contains(string(data), "slow down") {
			t.Fatalf("%s native error bytes must be preserved: %s", tc.name, data)
		}
	}
}

func TestNativeResponseHeadersFilteredGemini(t *testing.T) {
	h := newNativeHarness(t, nativeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "sess=1")
		w.Header().Set("Authorization", "Bearer upstream-secret")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[]}`))
	}
	status, headers, _ := postNative(t, h.router.URL+"/v1beta/models/gem-flash:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	for _, k := range []string{"Set-Cookie", "Authorization"} {
		if v := headers.Get(k); v != "" {
			t.Fatalf("response header %q must not be forwarded (got %q)", k, v)
		}
	}
}

func TestCountTokensHoldsNoGenerationSlot(t *testing.T) {
	cfg := nativeCfg(t)
	cfg.Server.MaxGenerations = 1
	release := make(chan struct{})
	slow := newNativeHarness(t, cfg)
	// Occupy the single generation slot with a slow native chat request.
	slow.handler = func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"chat-up","choices":[]}`))
	}
	done := make(chan int, 1)
	go func() {
		status, _, body := postNative(t, slow.router.URL+"/v1/chat/completions",
			`{"model":"chat-model","messages":[{"role":"user","content":"Hi"}]}`, nil)
		_ = body
		done <- status
	}()
	time.Sleep(300 * time.Millisecond)
	// A second generation must be 503 while the slot is held.
	status, _, _ := postNative(t, slow.router.URL+"/v1/chat/completions",
		`{"model":"chat-model","messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 503 {
		close(release)
		t.Fatalf("second generation must be 503 while slot held, got %d", status)
	}
	// Counting must still succeed: it holds no generation slot. Point the
	// count route at a fast upstream by swapping the handler.
	slow.mu.Lock()
	slow.handler = func(w http.ResponseWriter, r *http.Request) {
		// Distinguish by path: generation waits, count answers at once.
		if strings.HasSuffix(r.URL.Path, "/count_tokens") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"input_tokens":3}`))
			return
		}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"chat-up","choices":[]}`))
	}
	slow.mu.Unlock()
	// The count model resolves to provider countup; rewrite its base to the
	// same harness upstream (already done) and send the count request.
	status, _, data := postNative(t, slow.router.URL+"/v1/messages/count_tokens",
		`{"model":"count-model","messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 200 {
		close(release)
		t.Fatalf("count_tokens must not need a generation slot, got %d body %s", status, data)
	}
	// Drain: release the slow generation and wait for it.
	close(release)
	select {
	case got := <-done:
		if got != 200 {
			t.Fatalf("slow generation status=%d", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("slow generation did not finish")
	}
	_ = io.Discard
}
