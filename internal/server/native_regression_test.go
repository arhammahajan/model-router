package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arhammahajan/router/internal/config"
)

func geminiPercentCfgPath(t *testing.T) *config.Config {
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

[providers.gemup]
dialect = "gemini"
base_url = "http://127.0.0.1:9"
api_key = "gem-secret"

[[models]]
name = "literal%2Fname"
provider = "gemup"
model = "upstream-percent"

[[models]]
name = "literal/name"
provider = "gemup"
model = "upstream-slash"

[[models]]
name = "prov/model"
provider = "gemup"
model = "prov-model-up"
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

func TestGeminiLiteralPercentAliasRoutesOnce(t *testing.T) {
	h := newNativeHarness(t, geminiPercentCfgPath(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[]}`))
	}
	status, _, data := postNative(t, h.router.URL+"/v1beta/models/literal%252Fname:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lastPath != "/v1beta/models/upstream-percent:generateContent" {
		t.Fatalf("double-decode routed to wrong alias: upstream path %q (want upstream-percent)", h.lastPath)
	}
}

func TestGeminiEncodedSlashResolvesOnce(t *testing.T) {
	h := newNativeHarness(t, geminiPercentCfgPath(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[]}`))
	}
	body := `{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`
	for _, path := range []string{
		"/v1beta/models/prov/model:generateContent",
		"/v1beta/models/prov%2Fmodel:generateContent",
	} {
		status, _, data := postNative(t, h.router.URL+path, body, nil)
		if status != 200 {
			t.Fatalf("path %s status=%d body=%s", path, status, data)
		}
		h.mu.Lock()
		got := h.lastPath
		h.mu.Unlock()
		if got != "/v1beta/models/prov-model-up:generateContent" {
			t.Fatalf("path %s upstream=%q want prov-model-up", path, got)
		}
	}
}

func TestGeminiPercentTraversalCannotEscape(t *testing.T) {
	h := newNativeHarness(t, geminiPercentCfgPath(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[]}`))
	}
	body := `{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`
	for _, path := range []string{
		"/v1beta/models/%2e%2e:generateContent",
		"/v1beta/models/a%2F..%2Fb:generateContent",
		"/v1beta/models/a%3Ab:generateContent",
		"/v1beta/models/a%3Fquery:generateContent",
		"/v1beta/models/a%23frag:generateContent",
		"/v1beta/models/a%25b:generateContent",
	} {
		status, _, data := postNative(t, h.router.URL+path, body, nil)
		if status != 404 && status != 400 {
			t.Fatalf("path %s must be 404/400, got %d body %s", path, status, data)
		}
		var payload map[string]any
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Fatalf("path %s gemini error not JSON: %s", path, data)
		}
		if _, ok := payload["error"]; !ok {
			t.Fatalf("path %s must use google.rpc envelope: %s", path, data)
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.calls != 0 {
		t.Fatalf("traversal probes must not dial upstream, got %d calls last %q", h.calls, h.lastPath)
	}
}

func TestHandlerOverloadIngressShapes(t *testing.T) {
	cfg := nativeCfg(t)
	cfg.Server.MaxConnections = 1
	srv := New(cfg)
	h := srv.Handler()
	// Occupy the single handler slot directly: no network, no body parsing.
	srv.conns <- struct{}{}
	cases := []struct {
		path     string
		wantCode string // envelope marker
		isGemini bool
		isAnth   bool
	}{
		{"/v1/messages", "error", false, true},
		{"/v1/messages/count_tokens", "error", false, true},
		{"/v1beta/models/gem-flash:generateContent", "error", true, false},
		{"/v1/models/gem-flash:generateContent", "error", true, false},
		{"/v1/chat/completions", "error", false, false},
		{"/v1/responses", "error", false, false},
		{"/v1/models", "error", false, false},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(`{"model":"x"}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 503 {
			t.Fatalf("%s overload must be 503, got %d body %s", tc.path, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if tc.isGemini {
			var payload map[string]any
			if err := json.Unmarshal([]byte(body), &payload); err != nil {
				t.Fatalf("%s gemini envelope not JSON: %s", tc.path, body)
			}
			errObj, _ := payload["error"].(map[string]interface{})
			if errObj == nil || errObj["status"] != "UNAVAILABLE" {
				t.Fatalf("%s must use google.rpc UNAVAILABLE, got %s", tc.path, body)
			}
		} else if tc.isAnth {
			var payload map[string]any
			if err := json.Unmarshal([]byte(body), &payload); err != nil {
				t.Fatalf("%s anthropic envelope not JSON: %s", tc.path, body)
			}
			if payload["type"] != "error" {
				t.Fatalf("%s must use anthropic error shape, got %s", tc.path, body)
			}
		} else {
			if !strings.Contains(body, "overloaded") {
				t.Fatalf("%s must use OpenAI chat envelope, got %s", tc.path, body)
			}
		}
	}
	// Slot must be recovered after overload rejections: draining the manual
	// hold lets the next request through (no leak).
	<-srv.conns
	req := httptest.NewRequest(http.MethodGet, "/livez", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("slot not recovered after overload, livez=%d", rec.Code)
	}
}

func TestCountTokensBypassesHeldGenerations(t *testing.T) {
	cfg := nativeCfg(t)
	cfg.Server.MaxGenerations = 1
	release := make(chan struct{})
	h := newNativeHarness(t, cfg)
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/count_tokens") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"input_tokens":3}`))
			return
		}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"chat-up","choices":[]}`))
	}
	done := make(chan int, 1)
	go func() {
		status, _, _ := postNative(t, h.router.URL+"/v1/chat/completions",
			`{"model":"chat-model","messages":[{"role":"user","content":"Hi"}]}`, nil)
		done <- status
	}()
	time.Sleep(300 * time.Millisecond)
	// Generation slot still held: second generation is 503.
	status, _, _ := postNative(t, h.router.URL+"/v1/chat/completions",
		`{"model":"chat-model","messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 503 {
		close(release)
		t.Fatalf("second generation must be 503 while blocked, got %d", status)
	}
	// Count must succeed promptly without touching the generation endpoint.
	status, _, data := postNative(t, h.router.URL+"/v1/messages/count_tokens",
		`{"model":"count-model","messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 200 {
		close(release)
		t.Fatalf("count_tokens must bypass held generations, got %d body %s", status, data)
	}
	if !strings.Contains(string(data), `"input_tokens":3`) {
		close(release)
		t.Fatalf("count body wrong: %s", data)
	}
	h.mu.Lock()
	callsAfterCount := h.calls
	lastPath := h.lastPath
	h.mu.Unlock()
	if !strings.HasSuffix(lastPath, "/count_tokens") {
		close(release)
		t.Fatalf("count must hit the count endpoint, last upstream path %q", lastPath)
	}
	_ = callsAfterCount
	// Unsupported counting also holds no slot: 400 even while blocked.
	status, _, _ = postNative(t, h.router.URL+"/v1/messages/count_tokens",
		`{"model":"compat-claude","messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 400 {
		close(release)
		t.Fatalf("unsupported count must be 400 without a slot, got %d", status)
	}
	// Release and prove slots recovered with no deadlock/race.
	close(release)
	select {
	case got := <-done:
		if got != 200 {
			t.Fatalf("blocked generation status=%d", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocked generation did not finish after release")
	}
	status, _, _ = postNative(t, h.router.URL+"/v1/chat/completions",
		`{"model":"chat-model","messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 200 {
		t.Fatalf("generation slot not recovered, got %d", status)
	}
}
