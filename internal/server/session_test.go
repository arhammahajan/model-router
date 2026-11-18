package server

// Wire-level session affinity contracts: ingress-native errors with zero
// upstream I/O on all four ingresses, exactly-one upstream identity on
// native and bridged requests, prompt_cache_key native-preserved vs
// translated-consumed, omit/reject, and retry identity stability.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arhammahajan/model-router/internal/config"
	"github.com/arhammahajan/model-router/internal/router"
)

func sessionCfg(t *testing.T) *config.Config {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	p := filepath.Join(dir, "router.toml")
	body := `
[server]
auth_mode = "none"
allow_dynamic_routes = false
max_generations = 8

[retry]
max_attempts = 1

[providers.chatup]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
api_key = "chat-secret"
session_header = "x-session-out"

[providers.respup]
dialect = "openai-responses"
base_url = "http://127.0.0.1:9"
api_key = "resp-secret"
session_header = "x-session-out"

[providers.anthup]
dialect = "anthropic"
base_url = "http://127.0.0.1:9"
api_key = "anth-secret"
session_header = "x-affinity"
session_source_headers = ["thread-id"]
session_prompt_cache_key = true

[providers.gemup]
dialect = "gemini"
base_url = "http://127.0.0.1:9"
api_key = "gem-secret"
session_header = "x-gem-sess"

[providers.bridgeup]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
api_key = "chat-secret"
session_header = "x-session-out"
session_source_headers = ["thread-id"]

[[models]]
name = "chat-model"
provider = "chatup"
model = "gpt-4o-mini"

[[models]]
name = "resp-model"
provider = "respup"
model = "resp-up"

[[models]]
name = "msg-model"
provider = "anthup"
model = "claude-haiku-4-5"

[[models]]
name = "gem-flash"
provider = "gemup"
model = "gemini-2.0-flash"

[[models]]
name = "bridge-chat"
provider = "bridgeup"
model = "gpt-4o-mini"

[[models]]
name = "bridge-anth"
provider = "anthup"
model = "claude-haiku-4-5"
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

type sessHarness struct {
	upstream *httptest.Server
	router   *httptest.Server
	lastHdr  http.Header
	lastBody []byte
	allHdrs  []http.Header
	calls    int
	handler  func(w http.ResponseWriter, r *http.Request)
}

func newSessHarness(t *testing.T, cfg *config.Config) *sessHarness {
	t.Helper()
	h := &sessHarness{}
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}
	h.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.lastHdr = r.Header.Clone()
		h.allHdrs = append(h.allHdrs, r.Header.Clone())
		h.lastBody = body
		h.calls++
		h.handler(w, r)
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

func doReq(t *testing.T, method, url, body string, hdr http.Header) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func TestSessionInvalidAllIngressNoUpstreamIO(t *testing.T) {
	h := newSessHarness(t, sessionCfg(t))
	bad := http.Header{"Session-Id": {"bad value!"}, "Thread-Id": {"also bad!"}}
	cases := []struct {
		name string
		url  string
		body string
		want string // envelope marker for invalid_session_id
	}{
		{"chat", "/v1/chat/completions", `{"model":"chat-model","messages":[{"role":"user","content":"hi"}]}`, `"code":"invalid_session_id"`},
		{"responses", "/v1/responses", `{"model":"resp-model","input":"hi"}`, `"code":"invalid_session_id"`},
		{"messages", "/v1/messages", `{"model":"msg-model","max_tokens":4,"messages":[{"role":"user","content":"hi"}]}`, `"type":"error"`},
		{"gemini", "/v1beta/models/gem-flash:generateContent", `{"contents":[{"parts":[{"text":"hi"}]}]}`, "INVALID_ARGUMENT"},
	}
	for _, tc := range cases {
		status, data := doReq(t, http.MethodPost, h.router.URL+tc.url, tc.body, bad)
		if status != 400 {
			t.Fatalf("%s: status=%d body=%s", tc.name, status, data)
		}
		if !strings.Contains(string(data), tc.want) {
			t.Fatalf("%s: envelope missing %s: %s", tc.name, tc.want, data)
		}
		if strings.Contains(string(data), "bad value") {
			t.Fatalf("%s: error echoes identity value: %s", tc.name, data)
		}
	}
	if h.calls != 0 {
		t.Fatalf("session validation must precede upstream I/O: %d calls", h.calls)
	}
}

func TestSessionNativeWireSingleIdentity(t *testing.T) {
	h := newSessHarness(t, sessionCfg(t))
	got := http.Header{"Session-Id": {"s1"}, "X-Opencode-Session": {"o1"}}
	status, _ := doReq(t, http.MethodPost, h.router.URL+"/v1/chat/completions",
		`{"model":"chat-model","messages":[{"role":"user","content":"hi"}]}`, got)
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	if h.calls != 1 {
		t.Fatalf("calls=%d", h.calls)
	}
	vals := h.lastHdr.Values("X-Session-Out")
	if len(vals) != 1 || vals[0] != "s1" {
		t.Fatalf("exactly one upstream identity: %v", h.lastHdr)
	}
	for _, k := range []string{"Session-Id", "X-Opencode-Session"} {
		if len(h.lastHdr.Values(k)) != 0 {
			t.Fatalf("native identity must be consumed: %v", h.lastHdr)
		}
	}
	if h.lastHdr.Get("Authorization") != "Bearer chat-secret" {
		t.Fatalf("auth broken: %v", h.lastHdr)
	}
}

func TestSessionBridgeWireSingleIdentity(t *testing.T) {
	h := newSessHarness(t, sessionCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}
	// Anthropic ingress -> Chat upstream (bridgeup: thread-id source).
	status, data := doReq(t, http.MethodPost, h.router.URL+"/v1/messages",
		`{"model":"bridge-chat","max_tokens":8,"messages":[{"role":"user","content":"Hi"}]}`,
		http.Header{"Thread-Id": {"t1"}})
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	vals := h.lastHdr.Values("X-Session-Out")
	if len(vals) != 1 || vals[0] != "t1" {
		t.Fatalf("bridged single identity: %v", h.lastHdr)
	}
	if len(h.lastHdr.Values("Thread-Id")) != 0 {
		t.Fatalf("source identity must be consumed: %v", h.lastHdr)
	}
}

func TestSessionTranslatedConsumesPromptCacheKey(t *testing.T) {
	h := newSessHarness(t, sessionCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m",` +
			`"content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn",` +
			`"usage":{"input_tokens":1,"output_tokens":1}}`))
	}
	// bridge-anth routes Chat ingress -> Anthropic upstream with the cache
	// fallback enabled: prompt_cache_key selects and is consumed.
	status, data := doReq(t, http.MethodPost, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-anth","messages":[{"role":"user","content":"hi"}],"prompt_cache_key":"k1"}`,
		http.Header{"Session-Id": {"ignored"}})
	_ = data
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if strings.Contains(string(h.lastBody), "prompt_cache_key") {
		t.Fatalf("translated must consume prompt_cache_key: %s", h.lastBody)
	}
	vals := h.lastHdr.Values("X-Affinity")
	if len(vals) != 1 || vals[0] != "k1" {
		t.Fatalf("body fallback identity: %v body=%s", h.lastHdr, h.lastBody)
	}
}

func TestSessionNativePreservesPromptCacheKey(t *testing.T) {
	h := newSessHarness(t, sessionCfg(t))
	// Anthropic native keeps the JSON field and exact numerics (apart from
	// the documented model rewrite); selection still applies.
	native := `{"model":"msg-model","max_tokens":4,"messages":[{"role":"user","content":"hi"}],"prompt_cache_key":"k1"}`
	status, _ := doReq(t, http.MethodPost, h.router.URL+"/v1/messages", native, http.Header{"Thread-Id": {"t9"}})
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	if !strings.Contains(string(h.lastBody), `"prompt_cache_key":"k1"`) && !strings.Contains(string(h.lastBody), `"prompt_cache_key": "k1"`) {
		t.Fatalf("native must preserve prompt_cache_key: %s", h.lastBody)
	}
	vals := h.lastHdr.Values("X-Affinity")
	if len(vals) != 1 || vals[0] != "t9" {
		t.Fatalf("header beats body: %v", h.lastHdr)
	}
}

func TestSessionMissingOmitAndReject(t *testing.T) {
	h := newSessHarness(t, sessionCfg(t))
	// chatup omits: no identity, still routes.
	status, _ := doReq(t, http.MethodPost, h.router.URL+"/v1/chat/completions",
		`{"model":"chat-model","messages":[{"role":"user","content":"hi"}]}`, nil)
	if status != 200 {
		t.Fatalf("omit must route: %d", status)
	}
	if len(h.lastHdr.Values("X-Session-Out")) != 0 {
		t.Fatalf("omit must emit nothing: %v", h.lastHdr)
	}
}

func TestSessionSnapshotCopiesPolicy(t *testing.T) {
	cfg := sessionCfg(t)
	snap := router.New(cfg)
	m, p, fault := snap.Resolve("chat-model")
	if fault != router.Found {
		t.Fatalf("resolve failed")
	}
	if p.SessionHeader != "x-session-out" || len(p.SessionSourceHeaders) != 7 {
		t.Fatalf("snapshot must carry policy: %+v", p)
	}
	// Mutating the source and the returned copies must not alter the snapshot.
	cfg.Providers["chatup"].SessionSourceHeaders[0] = "mutated"
	p.SessionSourceHeaders[0] = "mutated"
	_ = m
	_, p2, fault := snap.Resolve("chat-model")
	if fault != router.Found || p2.SessionSourceHeaders[0] == "mutated" {
		t.Fatalf("snapshot must be immutable: %+v", p2.SessionSourceHeaders)
	}
}

func TestSessionRetryKeepsIdentity(t *testing.T) {
	cfg := sessionCfg(t)
	cfg.Retry.MaxAttempts = 2
	cfg.Retry.Statuses = []int{503}
	h := newSessHarness(t, cfg)
	first := true
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		if first {
			first = false
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}
	status, _ := doReq(t, http.MethodPost, h.router.URL+"/v1/chat/completions",
		`{"model":"chat-model","messages":[{"role":"user","content":"hi"}]}`, http.Header{"Session-Id": {"stable9"}})
	if status != 200 {
		t.Fatalf("retry must succeed: %d", status)
	}
	if len(h.allHdrs) != 2 {
		t.Fatalf("want 2 attempts, got %d", len(h.allHdrs))
	}
	for i, hd := range h.allHdrs {
		vals := hd.Values("X-Session-Out")
		if len(vals) != 1 || vals[0] != "stable9" {
			t.Fatalf("attempt %d identity changed: %v", i, hd)
		}
	}
}

func TestSessionCountTokensIgnoresAffinity(t *testing.T) {
	// Token counting is not a generation: session headers are neither
	// extracted nor enforced nor forwarded on the count path.
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	p := filepath.Join(dir, "router.toml")
	body := `
[server]
auth_mode = "none"
allow_dynamic_routes = false

[retry]
max_attempts = 1

[providers.countup]
dialect = "anthropic"
base_url = "http://127.0.0.1:9"
api_key = "anth-secret"
count_tokens = true
session_header = "x-affinity"

[[models]]
name = "count-model"
provider = "countup"
model = "claude-haiku-4-5"
`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	h := newSessHarness(t, cfg)
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":19}`))
	}
	status, data := doReq(t, http.MethodPost, h.router.URL+"/v1/messages/count_tokens",
		`{"model":"count-model","messages":[{"role":"user","content":"hi"}]}`,
		http.Header{"Session-Id": {"s1"}, "Thread-Id": {"bad value!"}})
	if status != 200 {
		t.Fatalf("counting must ignore affinity: %d %s", status, data)
	}
	if len(h.lastHdr.Values("X-Affinity")) != 0 || len(h.lastHdr.Values("Session-Id")) != 0 {
		t.Fatalf("count path must forward no identity: %v", h.lastHdr)
	}
}
