package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestValidCountTokensShapes(t *testing.T) {
	cases := []struct {
		body string
		ok   bool
		n    int64
	}{
		{`{"input_tokens":0}`, true, 0},
		{`{"input_tokens":12}`, true, 12},
		{`{"input_tokens":12,"extra":1}`, true, 12},
		{`{}`, false, 0},
		{`{"input_tokens":-1}`, false, 0},
		{`{"input_tokens":true}`, false, 0},
		{`{"input_tokens":false}`, false, 0},
		{`{"input_tokens":1.0}`, false, 0},
		{`{"input_tokens":1.5}`, false, 0},
		{`{"input_tokens":"12"}`, false, 0},
		{`{"input_tokens":null}`, false, 0},
		{`not json`, false, 0},
		{`[]`, false, 0},
	}
	for _, tc := range cases {
		n, ok := validCountTokens([]byte(tc.body))
		if ok != tc.ok || (ok && n != tc.n) {
			t.Fatalf("validCountTokens(%q) = (%d,%v), want (%d,%v)", tc.body, n, ok, tc.n, tc.ok)
		}
	}
}

func TestCountTokensDisabledNoUpstream(t *testing.T) {
	h := newNativeHarness(t, nativeCfg(t))
	status, _, data := postNative(t, h.router.URL+"/v1/messages/count_tokens",
		`{"model":"compat-claude","messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 400 {
		t.Fatalf("disabled count must be 400, got %d body %s", status, data)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("count error not JSON: %s", data)
	}
	if payload["type"] != "error" {
		t.Fatalf("expected anthropic error shape: %s", data)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.calls != 0 {
		t.Fatalf("disabled count must not dial upstream, got %d calls", h.calls)
	}
}

func TestCountTokensUpstreamValidation(t *testing.T) {
	h := newNativeHarness(t, nativeCfg(t))
	for _, tc := range []struct {
		name, upstream string
	}{
		{"bool", `{"input_tokens":true}`},
		{"float", `{"input_tokens":1.5}`},
		{"negative", `{"input_tokens":-2}`},
		{"missing", `{"other":1}`},
		{"notjson", `oops`},
	} {
		body := tc.upstream
		h.handler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}
		status, _, data := postNative(t, h.router.URL+"/v1/messages/count_tokens",
			`{"model":"count-model","messages":[{"role":"user","content":"Hi"}]}`, nil)
		if status != 502 {
			t.Fatalf("%s: got %d body %s, want 502", tc.name, status, data)
		}
	}
	// Oversize count body is a 502 without a valid count.
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":1,"pad":"` + strings.Repeat("x", 64*1024) + `"}`))
	}
	status, _, _ := postNative(t, h.router.URL+"/v1/messages/count_tokens",
		`{"model":"count-model","messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 502 {
		t.Fatalf("oversize count must be 502, got %d", status)
	}
}

func TestCountTokensNonAnthropicRouteRefused(t *testing.T) {
	h := newNativeHarness(t, nativeCfg(t))
	status, _, data := postNative(t, h.router.URL+"/v1/messages/count_tokens",
		`{"model":"chat-model","messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 400 {
		t.Fatalf("non-anthropic count route must be 400, got %d body %s", status, data)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.calls != 0 {
		t.Fatalf("refused count must not dial upstream, got %d calls", h.calls)
	}
}
