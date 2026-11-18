package server

// Adversarial F5/F7/F8 server review: no-compat paths ignore carriers,
// disabled baseline strips without mutation, value-free pre-I/O, sole
// emission + provider-owned credential. Hermetic httptest only, temp
// HOME/XDG, cleared auth env, fake upstream.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arhammahajan/model-router/internal/config"
)

const bearerRevTokA = "router-session-12345678-1234-4234-8234-123456789abc"
const bearerRevTokB = "router-session-aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"

func bearerRevCfg(t *testing.T) *config.Config {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("MODEL_ROUTER_API_KEYS", "")
	t.Setenv("MODEL_ROUTER_API_KEY", "")
	dir := t.TempDir()
	p := filepath.Join(dir, "router.toml")
	body := `
[server]
auth_mode = "none"
allow_dynamic_routes = false
max_generations = 16
bearer_session_compat = true
[retry]
max_attempts = 1
[providers.chatup]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
api_key = "chat-secret"
[providers.gemup]
dialect = "gemini"
base_url = "http://127.0.0.1:9"
api_key = "gem-secret"
[providers.sessup]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
api_key = "chat-secret"
session_header = "x-session-out"
[[models]]
name = "gem-sess"
provider = "sessup"
model = "gpt-4o-mini"
[[models]]
name = "gem-native"
provider = "gemup"
model = "gemini-2.0-flash"
[[models]]
name = "chat-native"
provider = "chatup"
model = "gpt-4o-mini"
[capabilities.overrides.gem-native.reasoning.effort]
values = ["low", "none"]
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

type bearerRevHarness struct {
	upstream *httptest.Server
	router   *httptest.Server
	lastBody []byte
	lastHdr  http.Header
	n        int
}

func newBearerRevHarness(t *testing.T, cfg *config.Config) *bearerRevHarness {
	t.Helper()
	h := &bearerRevHarness{}
	h.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		h.lastBody = b
		h.lastHdr = r.Header.Clone()
		h.n++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(h.upstream.Close)
	for _, p := range cfg.Providers {
		p.BaseURL = h.upstream.URL
	}
	h.router = httptest.NewServer(New(cfg).Handler())
	t.Cleanup(h.router.Close)
	return h
}

func bearerRevPost(t *testing.T, url, body string, hdrs map[string]string) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(http.MethodPost, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdrs {
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

func TestBearerCompatReviewNoCompatIngress(t *testing.T) {
	h := newBearerRevHarness(t, bearerRevCfg(t))
	// Chat ingress ignores ?key=: no query is read on Chat path and no
	// compat session is selected without a marked Bearer.
	status, _ := bearerRevPost(t, h.router.URL+"/v1/chat/completions?key="+bearerRevTokA,
		`{"model":"chat-native","messages":[{"role":"user","content":"hi"}]}`, nil)
	if status != 200 {
		t.Fatalf("chat ?key= must be ignored: %d", status)
	}
	if got := h.lastHdr.Get("X-Session-Out"); got != "" {
		t.Fatalf("chat query must not select identity: %q", got)
	}
	// api-key carrier on Gemini generation is not a compat token.
	status, _ = bearerRevPost(t, h.router.URL+"/v1beta/models/gem-sess:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`,
		map[string]string{"Api-Key": bearerRevTokA})
	if status != 200 {
		t.Fatalf("api-key ingress: %d", status)
	}
	if got := h.lastHdr.Get("X-Session-Out"); got != "" {
		t.Fatalf("api-key must not collect: %q", got)
	}
	// Effort on Chat is stripped, never validated (bogus passes).
	status, _ = bearerRevPost(t, h.router.URL+"/v1/chat/completions",
		`{"model":"chat-native","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"X-Model-Router-Reasoning-Effort": "bogus-level"})
	if status != 200 {
		t.Fatalf("chat effort must be ignored: %d", status)
	}
	if got := h.lastHdr.Get("X-Model-Router-Reasoning-Effort"); got != "" {
		t.Fatalf("effort must be stripped: %q", got)
	}
}

func TestBearerCompatReviewPreIOValueFree(t *testing.T) {
	h := newBearerRevHarness(t, bearerRevCfg(t))
	n0 := h.n
	status, data := bearerRevPost(t, h.router.URL+"/v1beta/models/gem-sess:generateContent",
		`{"contents":[]}`, map[string]string{"Authorization": "Bearer " + bearerRevTokA, "X-Goog-Api-Key": bearerRevTokB})
	if status != 400 {
		t.Fatalf("distinct must 400: %d %s", status, data)
	}
	if strings.Contains(string(data), bearerRevTokA) || strings.Contains(string(data), bearerRevTokB) {
		t.Fatalf("value-free: %s", data)
	}
	if h.n != n0 {
		t.Fatalf("pre-I/O refusal must not touch upstream")
	}
	var gemErr struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(data, &gemErr)
	if !strings.Contains(string(data), "INVALID_SESSION_ID") && !strings.Contains(string(data), "invalid_session_id") {
		t.Fatalf("session code: %s", data)
	}
}

func TestBearerCompatReviewSoleEmission(t *testing.T) {
	h := newBearerRevHarness(t, bearerRevCfg(t))
	status, data := bearerRevPost(t, h.router.URL+"/v1beta/models/gem-sess:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}],"sessionId":"abc"}`,
		map[string]string{"Authorization": "Bearer " + bearerRevTokA})
	if status != 200 {
		t.Fatalf("status=%d %s", status, data)
	}
	if got := h.lastHdr.Get("X-Session-Out"); got != "agy-acp-6cc43f858fbb763301637b5af970e2a46b46f461f27e5a0f41e009c59b827b25" {
		t.Fatalf("ACP must win sole emission: %q", got)
	}
	if got := h.lastHdr.Get("X-Goog-Api-Key"); got != "" {
		t.Fatalf("goog carrier must not travel: %q", got)
	}
	// Provider-owned credential travels (chat-secret), never the client token.
	if got := h.lastHdr.Get("Authorization"); got == "Bearer "+bearerRevTokA {
		t.Fatalf("client auth must never become upstream credential")
	}
	if got := h.lastHdr.Get("Authorization"); got != "Bearer chat-secret" {
		t.Fatalf("provider-owned credential must travel: %q", got)
	}
}

func TestBearerCompatReviewDisabledBaseline(t *testing.T) {
	cfg := bearerRevCfg(t)
	cfg.Server.BearerSessionCompat = false
	h := newBearerRevHarness(t, cfg)
	status, _ := bearerRevPost(t, h.router.URL+"/v1beta/models/gem-sess:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`,
		map[string]string{"Authorization": "Bearer " + bearerRevTokA})
	if status != 200 {
		t.Fatalf("disabled: %d", status)
	}
	if got := h.lastHdr.Get("X-Session-Out"); got != "" {
		t.Fatalf("disabled must not select: %q", got)
	}
	status, _ = bearerRevPost(t, h.router.URL+"/v1beta/models/gem-native:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`,
		map[string]string{"X-Model-Router-Reasoning-Effort": "low"})
	if status != 200 {
		t.Fatalf("disabled effort: %d", status)
	}
	if strings.Contains(string(h.lastBody), "thinkingLevel") {
		t.Fatalf("disabled must not mutate: %s", h.lastBody)
	}
}
