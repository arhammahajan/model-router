package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/arhammahajan/model-router/internal/config"
)

func overrideCfg(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "router.toml")
	body := `
[server]
auth_mode = "none"
allow_dynamic_routes = false
max_generations = 16

[retry]
max_attempts = 1

[providers.chatup]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
api_key = "chat-secret"

[providers.respup]
dialect = "openai-responses"
base_url = "http://127.0.0.1:9"
api_key = "resp-secret"

[providers.anthup]
dialect = "anthropic"
base_url = "http://127.0.0.1:9"
api_key = "anth-secret"

[providers.gemup]
dialect = "gemini"
base_url = "http://127.0.0.1:9"
api_key = "gem-secret"

[providers.countup]
dialect = "anthropic"
base_url = "http://127.0.0.1:9"
api_key = "anth-secret"
count_tokens = true

[[models]]
name = "chat-native"
provider = "chatup"
model = "gpt-4o-mini"
[[models]]
name = "chat-to-anthropic"
provider = "chatup"
model = "claude-x"
dialect = "anthropic"
[[models]]
name = "chat-to-responses"
provider = "chatup"
model = "resp-x"
dialect = "openai-responses"
[[models]]
name = "chat-to-gemini"
provider = "chatup"
model = "gemini-x"
dialect = "gemini"

[[models]]
name = "anth-native"
provider = "anthup"
model = "claude-x"
[[models]]
name = "anth-to-chat"
provider = "anthup"
model = "gpt-x"
dialect = "openai-chat"

[[models]]
name = "resp-native"
provider = "respup"
model = "resp-x"
[[models]]
name = "resp-to-chat"
provider = "respup"
model = "gpt-x"
dialect = "openai-chat"

[[models]]
name = "gem-native"
provider = "gemup"
model = "gemini-x"
[[models]]
name = "gem-to-chat"
provider = "gemup"
model = "gpt-x"
dialect = "openai-chat"

[[models]]
name = "count-native"
provider = "countup"
model = "claude-x"
[[models]]
name = "count-to-chat"
provider = "countup"
model = "gpt-x"
dialect = "openai-chat"
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

type overrideHarness struct {
	upstream *httptest.Server
	router   *httptest.Server
	mu       sync.Mutex
	lastPath string
	lastRaw  string
	lastBody []byte
	lastHdr  http.Header
	calls    int
}

func newOverrideHarness(t *testing.T, cfg *config.Config) *overrideHarness {
	t.Helper()
	h := &overrideHarness{}
	h.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.lastPath = r.URL.Path
		h.lastRaw = r.URL.RawQuery
		h.lastBody = body
		h.lastHdr = r.Header.Clone()
		h.calls++
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
		case strings.HasSuffix(r.URL.Path, "/responses"):
			_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","status":"completed","model":"m","output":[{"type":"message","id":"m1","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
		case strings.HasSuffix(r.URL.Path, "/v1/messages"):
			_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
		default:
			// Gemini generateContent/streamGenerateContent
			_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"hi"}],"role":"model"},"finishReason":"STOP","index":0}],"modelVersion":"gemini-2.0-flash","responseId":"resp_gem_1","usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`))
		}
	}))
	t.Cleanup(h.upstream.Close)
	for _, prov := range cfg.Providers {
		prov.BaseURL = h.upstream.URL
	}
	srv := New(cfg)
	h.router = httptest.NewServer(srv.Handler())
	t.Cleanup(h.router.Close)
	return h
}

func postJSON(t *testing.T, url, body string, headers map[string]string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func TestOverrideMatrixDispatchSuffix(t *testing.T) {
	h := newOverrideHarness(t, overrideCfg(t))
	cases := []struct {
		ingress string
		url     string
		body    func(model string) string
		model   string
		want    string
	}{
		{"chat", "/v1/chat/completions", func(m string) string {
			return `{"model":"` + m + `","messages":[{"role":"user","content":"hi"}]}`
		}, "chat-native", "/chat/completions"},
		{"chat", "/v1/chat/completions", func(m string) string {
			return `{"model":"` + m + `","messages":[{"role":"user","content":"hi"}]}`
		}, "chat-to-anthropic", "/v1/messages"},
		{"chat", "/v1/chat/completions", func(m string) string {
			return `{"model":"` + m + `","messages":[{"role":"user","content":"hi"}]}`
		}, "chat-to-responses", "/responses"},
		{"chat", "/v1/chat/completions", func(m string) string {
			return `{"model":"` + m + `","messages":[{"role":"user","content":"hi"}]}`
		}, "chat-to-gemini", "/v1beta"},
		{"anthropic", "/v1/messages", func(m string) string {
			return `{"model":"` + m + `","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
		}, "anth-native", "/v1/messages"},
		{"anthropic", "/v1/messages", func(m string) string {
			return `{"model":"` + m + `","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
		}, "anth-to-chat", "/chat/completions"},
		{"responses", "/v1/responses", func(m string) string {
			return `{"model":"` + m + `","input":"hi"}`
		}, "resp-native", "/responses"},
		{"responses", "/v1/responses", func(m string) string {
			return `{"model":"` + m + `","input":"hi"}`
		}, "resp-to-chat", "/chat/completions"},
	}
	for _, tc := range cases {
		h.mu.Lock()
		h.calls = 0
		h.mu.Unlock()
		status, data := postJSON(t, h.router.URL+tc.url, tc.body(tc.model), nil)
		if status != 200 {
			t.Fatalf("%s %s status=%d body=%s", tc.ingress, tc.model, status, data)
		}
		h.mu.Lock()
		path, calls := h.lastPath, h.calls
		h.mu.Unlock()
		if calls != 1 || !strings.Contains(path, tc.want) {
			t.Fatalf("%s %s upstream path=%q calls=%d want suffix %q", tc.ingress, tc.model, path, calls, tc.want)
		}
	}
	// Gemini ingress native + bridge (model in URL, no model field).
	for _, tc := range []struct{ model, want string }{
		{"gem-native", "/v1beta"},
		{"gem-to-chat", "/chat/completions"},
	} {
		h.mu.Lock()
		h.calls = 0
		h.mu.Unlock()
		url := h.router.URL + "/v1beta/models/" + tc.model + ":generateContent"
		status, data := postJSON(t, url, `{"contents":[{"parts":[{"text":"hi"}]}]}`, nil)
		if status != 200 {
			t.Fatalf("gemini %s status=%d body=%s", tc.model, status, data)
		}
		h.mu.Lock()
		path := h.lastPath
		h.mu.Unlock()
		if !strings.Contains(path, tc.want) {
			t.Fatalf("gemini %s path=%q want %q", tc.model, path, tc.want)
		}
	}
	// Actual dest drives native classification even when provider differs:
	// anth provider overriding to chat, called via chat ingress, must be native chat.
	h.mu.Lock()
	h.calls = 0
	h.mu.Unlock()
	status, data := postJSON(t, h.router.URL+"/v1/chat/completions", `{"model":"anth-to-chat","messages":[{"role":"user","content":"hi"}],"mystery_keep":7}`, nil)
	if status != 200 {
		t.Fatalf("provider-different native status=%d %s", status, data)
	}
	h.mu.Lock()
	path, hdr, body := h.lastPath, h.lastHdr, h.lastBody
	h.mu.Unlock()
	if !strings.Contains(path, "/chat/completions") {
		t.Fatalf("dest-native path=%q", path)
	}
	var wire map[string]json.RawMessage
	_ = json.Unmarshal(body, &wire)
	if _, ok := wire["mystery_keep"]; !ok {
		t.Fatalf("native must preserve unknown field: %s", body)
	}
	if hdr.Get("X-Api-Key") != "anth-secret" {
		t.Fatalf("provider auth must stay anthropic: %v", hdr)
	}
	if hdr.Get("Anthropic-Version") != "" {
		t.Fatalf("chat dest must not default anthropic-version: %v", hdr)
	}
}

func TestOverrideAuthShapesAndScrub(t *testing.T) {
	h := newOverrideHarness(t, overrideCfg(t))
	// Chat provider (Bearer default) overriding to anthropic: Bearer must stay, no x-api-key.
	status, data := postJSON(t, h.router.URL+"/v1/chat/completions", `{"model":"chat-to-anthropic","messages":[{"role":"user","content":"hi"}]}`, map[string]string{"X-Goog-Api-Key": "client-key", "Openai-Beta": "x"})
	if status != 200 {
		t.Fatalf("bridge status=%d %s", status, data)
	}
	h.mu.Lock()
	hdr := h.lastHdr.Clone()
	raw := h.lastRaw
	h.mu.Unlock()
	if hdr.Get("Authorization") != "Bearer chat-secret" {
		t.Fatalf("bearer provider must stay bearer: %v", hdr)
	}
	if hdr.Get("X-Api-Key") != "" || hdr.Get("X-Goog-Api-Key") == "client-key" {
		t.Fatalf("client key must be scrubbed, single credential only: %v", hdr)
	}
	if hdr.Get("Openai-Beta") != "" {
		t.Fatalf("wrong-dialect opt-in must drop: %v", hdr)
	}
	if raw != "" {
		t.Fatalf("query must not forward: %q", raw)
	}
	// Gemini ingress with ?key= query: accepted for auth, never forwarded.
	req, _ := http.NewRequest(http.MethodPost, h.router.URL+"/v1beta/models/gem-native:generateContent?key=client-key&alt=sse", strings.NewReader(`{"contents":[{"parts":[{"text":"hi"}]}]}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	h.mu.Lock()
	raw2 := h.lastRaw
	h.mu.Unlock()
	if strings.Contains(raw2, "key=") {
		t.Fatalf("client ?key must be scrubbed: %q", raw2)
	}
}

func TestOverrideCountTokensTwoGate(t *testing.T) {
	h := newOverrideHarness(t, overrideCfg(t))
	// Away from anthropic refuses with 0 upstream I/O.
	h.mu.Lock()
	h.calls = 0
	h.mu.Unlock()
	status, _ := postJSON(t, h.router.URL+"/v1/messages/count_tokens", `{"model":"count-to-chat","messages":[{"role":"user","content":"hi"}],"max_tokens":4}`, nil)
	if status != 400 {
		t.Fatalf("override away must refuse, got %d", status)
	}
	h.mu.Lock()
	calls := h.calls
	h.mu.Unlock()
	if calls != 0 {
		t.Fatalf("refusal must have 0 upstream I/O, got %d", calls)
	}
	// To anthropic from non-count provider refuses (provider default chat, override anthropic needs count).
	// chat-to-anthropic provider (chatup) has no count_tokens; gate requires provider enabled.
	status, _ = postJSON(t, h.router.URL+"/v1/messages/count_tokens", `{"model":"chat-to-anthropic","messages":[{"role":"user","content":"hi"}],"max_tokens":4}`, nil)
	if status != 400 {
		t.Fatalf("override to anthropic without provider count must refuse, got %d", status)
	}
	// Success when both gates pass.
	h.upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.lastPath = r.URL.Path
		h.calls++
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":5}`))
	})
	status, data := postJSON(t, h.router.URL+"/v1/messages/count_tokens", `{"model":"count-native","messages":[{"role":"user","content":"hi"}],"max_tokens":4}`, nil)
	if status != 200 || !strings.Contains(string(data), "input_tokens") {
		t.Fatalf("count success status=%d body=%s", status, data)
	}
}
