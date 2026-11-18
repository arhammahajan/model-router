package server

// Bearer-compat seam tests: Gemini + Chat ingress wiring with the flag on,
// across all four destinations, session affinity, errors and no-compat
// paths. Hermetic httptest only; no env, no network beyond loopback.

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

const compatSeamTokA = "router-session-12345678-1234-4234-8234-123456789abc"
const compatSeamTokB = "router-session-aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"

const compatChatOK = `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`

const compatAnthropicOK = `{"id":"msg_up_1","type":"message","role":"assistant","model":"claude-haiku-4-5","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":7,"output_tokens":3}}`

const compatGeminiOK = `{"candidates":[{"content":{"parts":[{"text":"hello"}],"role":"model"}}]}`

func compatSeamCfg(t *testing.T, extra string) *config.Config {
	t.Helper()
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

[providers.lossychat]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
api_key = "chat-secret"
compatibility = "lossy"

[providers.gemup]
dialect = "gemini"
base_url = "http://127.0.0.1:9"
api_key = "gem-secret"

[providers.resup]
dialect = "openai-responses"
base_url = "http://127.0.0.1:9"
api_key = "resp-secret"

[providers.anthup]
dialect = "anthropic"
base_url = "http://127.0.0.1:9"
api_key = "anth-secret"

[providers.lossyanth]
dialect = "anthropic"
base_url = "http://127.0.0.1:9"
api_key = "anth-secret"
compatibility = "lossy"

[providers.sessup]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
api_key = "chat-secret"
session_header = "x-session-out"

[[models]]
name = "gem-native"
provider = "gemup"
model = "gemini-2.0-flash"

[[models]]
name = "gem-chat"
provider = "chatup"
model = "gpt-4o-mini"

[[models]]
name = "gem-chat-lossy"
provider = "lossychat"
model = "gpt-4o-mini"

[[models]]
name = "gem-resp"
provider = "resup"
model = "gpt-4.1"

[[models]]
name = "gem-anth"
provider = "anthup"
model = "claude-haiku-4-5"

[[models]]
name = "gem-anth-lossy"
provider = "lossyanth"
model = "claude-haiku-4-5"

[[models]]
name = "chat-native"
provider = "chatup"
model = "gpt-4o-mini"

[[models]]
name = "chat-sess"
provider = "sessup"
model = "gpt-4o-mini"

[[models]]
name = "gem-sess"
provider = "sessup"
model = "gpt-4o-mini"

[capabilities.overrides.gem-chat.reasoning.effort]
values = ["low", "medium", "high", "xhigh", "none"]
[capabilities.overrides.gem-chat-lossy.reasoning.effort]
values = ["low", "medium", "high", "xhigh", "none"]
[capabilities.overrides.gem-resp.reasoning.effort]
values = ["low", "medium", "high", "xhigh", "none"]
[capabilities.overrides.gem-anth.reasoning.effort]
values = ["low", "medium", "high", "xhigh", "none"]
[capabilities.overrides.gem-anth-lossy.reasoning.effort]
values = ["low", "medium", "high", "xhigh", "none"]
[capabilities.overrides.gem-native.reasoning.effort]
values = ["low", "medium", "high", "none"]
` + extra
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

type compatSeamHarness struct {
	upstream *httptest.Server
	router   *httptest.Server
	mu       sync.Mutex
	lastPath string
	lastBody []byte
	lastHdr  http.Header
	n        int
}

func newCompatSeamHarness(t *testing.T, cfg *config.Config) *compatSeamHarness {
	t.Helper()
	h := &compatSeamHarness{}
	h.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.lastPath = r.URL.Path
		h.lastBody = body
		h.lastHdr = r.Header.Clone()
		h.n++
		h.mu.Unlock()
		stream := strings.Contains(string(body), `"stream":true`)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/chat/completions") && stream:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-up\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-4o-mini\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			_, _ = w.Write([]byte(compatChatOK))
		case strings.HasSuffix(r.URL.Path, "/responses"):
			_, _ = w.Write([]byte(grResponsesJSONText))
		case strings.HasSuffix(r.URL.Path, "/messages"):
			_, _ = w.Write([]byte(compatAnthropicOK))
		default:
			_, _ = w.Write([]byte(compatGeminiOK))
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

func (h *compatSeamHarness) wire() map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	var v map[string]any
	if err := json.Unmarshal(h.lastBody, &v); err != nil {
		return nil
	}
	return v
}

func (h *compatSeamHarness) header(name string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastHdr.Get(name)
}

func (h *compatSeamHarness) calls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n
}

func postCompat(t *testing.T, url, body string, headers map[string]string) (int, http.Header, []byte) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(http.MethodPost, url, reader)
	if err != nil {
		t.Fatal(err)
	}
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
	return resp.StatusCode, resp.Header, data
}

func TestCompatSeamConflictingTokens(t *testing.T) {
	h := newCompatSeamHarness(t, compatSeamCfg(t, ""))
	status, _, data := postCompat(t, h.router.URL+"/v1beta/models/gem-chat:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`,
		map[string]string{"Authorization": "Bearer " + compatSeamTokA, "X-Goog-Api-Key": compatSeamTokB})
	if status != 400 || !strings.Contains(string(data), "INVALID_SESSION_ID") {
		t.Fatalf("conflict must 400: %d %s", status, data)
	}
	if strings.Contains(string(data), compatSeamTokA) {
		t.Fatalf("no token echo: %s", data)
	}
	if got := h.calls(); got != 0 {
		t.Fatalf("pre-I/O refusal must not touch upstream: %d", got)
	}
}

func TestCompatSeamSessionFallbackEndToEnd(t *testing.T) {
	h := newCompatSeamHarness(t, compatSeamCfg(t, ""))
	// sessionId "abc" hashes to the frozen ACP golden and is selected.
	status, _, data := postCompat(t, h.router.URL+"/v1beta/models/gem-sess:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}],"sessionId":"abc"}`,
		map[string]string{"Authorization": "Bearer " + compatSeamTokA})
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if got := h.header("X-Session-Out"); got != "agy-acp-6cc43f858fbb763301637b5af970e2a46b46f461f27e5a0f41e009c59b827b25" {
		t.Fatalf("ACP hash must win: %q", got)
	}
	// Launch token alone becomes the fallback identity.
	status, _, data = postCompat(t, h.router.URL+"/v1beta/models/gem-sess:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`,
		map[string]string{"X-Goog-Api-Key": compatSeamTokA})
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if got := h.header("X-Session-Out"); got != compatSeamTokA {
		t.Fatalf("token fallback must be selected: %q", got)
	}
	// Credential headers are consumed, never forwarded.
	if got := h.header("X-Goog-Api-Key"); got != "" {
		t.Fatalf("credential must not travel upstream: %q", got)
	}
	if got := h.header("Authorization"); got == "Bearer "+compatSeamTokA {
		t.Fatalf("client auth must not travel upstream: %q", got)
	}
	// ?key= token works on Gemini ingress.
	status, _, data = postCompat(t, h.router.URL+"/v1beta/models/gem-sess:generateContent?key="+compatSeamTokB,
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, nil)
	if status != 200 {
		t.Fatalf("query key status=%d body=%s", status, data)
	}
	if got := h.header("X-Session-Out"); got != compatSeamTokB {
		t.Fatalf("query fallback must be selected: %q", got)
	}
}

func TestCompatSeamEffortAllDestinations(t *testing.T) {
	h := newCompatSeamHarness(t, compatSeamCfg(t, ""))
	post := func(model, effort string) (int, http.Header) {
		status, hdr, data := postCompat(t, h.router.URL+"/v1beta/models/"+model+":generateContent",
			`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`,
			map[string]string{"X-Model-Router-Reasoning-Effort": effort})
		if status != 200 {
			t.Fatalf("%s effort %s: status=%d body=%s", model, effort, status, data)
		}
		return status, hdr
	}
	// Chat: exact literal, no loss.
	_, hdr := post("gem-chat", "medium")
	if hdr.Get("X-Model-Router-Loss") != "" {
		t.Fatalf("exact chat must carry no loss")
	}
	if h.wire()["reasoning_effort"] != "medium" {
		t.Fatalf("chat wire must carry medium: %v", h.wire())
	}
	// Responses: named literal preserved.
	_, hdr = post("gem-resp", "xhigh")
	if hdr.Get("X-Model-Router-Loss") != "" {
		t.Fatalf("exact responses must carry no loss")
	}
	if h.wire()["reasoning"].(map[string]any)["effort"] != "xhigh" {
		t.Fatalf("responses wire must preserve xhigh: %v", h.wire())
	}
	// Native Gemini: direct mutation, no plan.
	status, _, data := postCompat(t, h.router.URL+"/v1beta/models/gem-native:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`,
		map[string]string{"X-Model-Router-Reasoning-Effort": "low"})
	if status != 200 {
		t.Fatalf("native status=%d body=%s", status, data)
	}
	if !strings.Contains(string(h.lastBody), `"thinkingLevel":"LOW"`) {
		t.Fatalf("native must mutate thinkingLevel: %s", h.lastBody)
	}
	// Native none -> budget 0.
	status, _, data = postCompat(t, h.router.URL+"/v1beta/models/gem-native:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`,
		map[string]string{"X-Model-Router-Reasoning-Effort": "none"})
	if status != 200 || !strings.Contains(string(h.lastBody), `"thinkingBudget":0`) {
		t.Fatalf("native none must set budget 0: %d %s", status, h.lastBody)
	}
	// Anthropic strict refuses; lossy drops with the static header origin.
	status, _, data = postCompat(t, h.router.URL+"/v1beta/models/gem-anth:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`,
		map[string]string{"X-Model-Router-Reasoning-Effort": "medium"})
	if status != 400 || !strings.Contains(string(data), "TRANSLATION_LOSS") {
		t.Fatalf("anthropic strict must refuse: %d %s", status, data)
	}
	status, hdr, _ = postCompat(t, h.router.URL+"/v1beta/models/gem-anth-lossy:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`,
		map[string]string{"X-Model-Router-Reasoning-Effort": "medium"})
	if status != 200 {
		t.Fatalf("anthropic lossy status=%d", status)
	}
	var loss []map[string]any
	if err := json.Unmarshal([]byte(hdr.Get("X-Model-Router-Loss")), &loss); err != nil {
		t.Fatalf("loss header: %v", err)
	}
	if len(loss) != 1 || loss[0]["field"] != "x-model-router-reasoning-effort" || loss[0]["action"] != "dropped" {
		t.Fatalf("loss must name the header origin: %v", loss)
	}
	// Effort header is always stripped upstream.
	if got := h.header("X-Model-Router-Reasoning-Effort"); got != "" {
		t.Fatalf("effort carrier must not travel upstream: %q", got)
	}
}

func TestCompatSeamEffortRejects(t *testing.T) {
	h := newCompatSeamHarness(t, compatSeamCfg(t, ""))
	cases := []struct {
		name   string
		model  string
		header map[string]string
		body   string
	}{
		{"unpublished", "gem-chat", map[string]string{"X-Model-Router-Reasoning-Effort": "max"}, `{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`},
		{"native-xhigh", "gem-native", map[string]string{"X-Model-Router-Reasoning-Effort": "xhigh"}, `{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`},
		{"body-conflict", "gem-chat", map[string]string{"X-Model-Router-Reasoning-Effort": "low"}, `{"contents":[{"role":"user","parts":[{"text":"Hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingLevel":"LOW"}}}`},
		{"null-conflict", "gem-chat", map[string]string{"X-Model-Router-Reasoning-Effort": "low"}, `{"contents":[{"role":"user","parts":[{"text":"Hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingBudget":null}}}`},
		{"nonobject-config", "gem-chat", map[string]string{"X-Model-Router-Reasoning-Effort": "low"}, `{"contents":[{"role":"user","parts":[{"text":"Hi"}]}],"generationConfig":[]}`},
		{"unknown-caps", "gem-sess", map[string]string{"X-Model-Router-Reasoning-Effort": "medium"}, `{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`},
	}
	for _, tc := range cases {
		status, _, data := postCompat(t, h.router.URL+"/v1beta/models/"+tc.model+":generateContent", tc.body, tc.header)
		if status != 400 {
			t.Fatalf("%s must 400, got %d %s", tc.name, status, data)
		}
		if strings.Contains(string(data), "xhigh") || strings.Contains(string(data), "max") {
			t.Fatalf("%s must be value-free: %s", tc.name, data)
		}
	}
	if got := h.calls(); got != 0 {
		t.Fatalf("all effort rejects are pre-I/O: %d upstream calls", got)
	}
}

func TestCompatSeamChat(t *testing.T) {
	h := newCompatSeamHarness(t, compatSeamCfg(t, ""))
	body := `{"model":"chat-sess","messages":[{"role":"system","content":"a"},{"role":"system","content":"b"},{"role":"user","content":"hi"}]}`
	status, _, data := postCompat(t, h.router.URL+"/v1/chat/completions", body,
		map[string]string{"Authorization": "Bearer " + compatSeamTokA})
	if status != 200 {
		t.Fatalf("chat status=%d body=%s", status, data)
	}
	if got := h.header("X-Session-Out"); got != compatSeamTokA {
		t.Fatalf("chat token must select identity: %q", got)
	}
	if got := h.header("Authorization"); got == "Bearer "+compatSeamTokA {
		t.Fatalf("marked client auth must not travel upstream: %q", got)
	}
	wire := h.wire()
	msgs := wire["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["content"] != "a\n\nb" {
		t.Fatalf("chat systems must merge: %v", wire)
	}
	// Unmarked requests are untouched.
	status, _, data = postCompat(t, h.router.URL+"/v1/chat/completions",
		`{"model":"chat-native","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"Authorization": "Bearer ordinary"})
	if status != 200 {
		t.Fatalf("unmarked status=%d body=%s", status, data)
	}
	if got := h.header("X-Session-Out"); got != "" {
		t.Fatalf("unmarked must not select identity: %q", got)
	}
	// Distinct marked tokens reject.
	req, _ := http.NewRequest(http.MethodPost, h.router.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+compatSeamTokA)
	req.Header.Add("Authorization", "Bearer "+compatSeamTokB)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("distinct chat tokens must 400: %d %s", resp.StatusCode, data)
	}
}

func TestCompatSeamNoCompatPaths(t *testing.T) {
	h := newCompatSeamHarness(t, compatSeamCfg(t, ""))
	// GET list with conflicting tokens: no compat step, not a 400 session error.
	req, _ := http.NewRequest(http.MethodGet, h.router.URL+"/v1beta/models", nil)
	req.Header.Set("Authorization", "Bearer "+compatSeamTokA)
	req.Header.Set("X-Goog-Api-Key", compatSeamTokB)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(data), "INVALID_SESSION_ID") {
		t.Fatalf("GET must not run compat: %s", data)
	}
	// Responses ingress with a carrier token: ignored, no session.
	status, _, _ := postCompat(t, h.router.URL+"/v1/responses",
		`{"model":"gem-resp","input":"hi"}`,
		map[string]string{"Authorization": "Bearer " + compatSeamTokA})
	if status != 200 {
		t.Fatalf("responses ingress must ignore compat: %d", status)
	}
	// Anthropic ingress with carrier tokens: ignored.
	status, _, _ = postCompat(t, h.router.URL+"/v1/messages",
		`{"model":"gem-anth","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"Authorization": "Bearer " + compatSeamTokA})
	if status != 200 {
		t.Fatalf("anthropic ingress must ignore compat: %d", status)
	}
	// Count tokens with conflicting tokens: no compat step, never a session error.
	status, _, data = postCompat(t, h.router.URL+"/v1/messages/count_tokens",
		`{"model":"gem-anth","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"Authorization": "Bearer " + compatSeamTokA, "X-Goog-Api-Key": compatSeamTokB})
	if strings.Contains(string(data), "INVALID_SESSION_ID") {
		t.Fatalf("count must not run compat: %s", data)
	}
	// Effort header on a non-Gemini ingress is stripped, never validated.
	status, _, _ = postCompat(t, h.router.URL+"/v1/chat/completions",
		`{"model":"chat-native","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"X-Model-Router-Reasoning-Effort": "bogus-level"})
	if status != 200 {
		t.Fatalf("off-path effort must be ignored: %d", status)
	}
	if got := h.header("X-Model-Router-Reasoning-Effort"); got != "" {
		t.Fatalf("effort must be stripped upstream: %q", got)
	}
}

func TestCompatSeamDisabledBaseline(t *testing.T) {
	// Same routes with the flag off: tokens ignored, effort stripped raw.
	cfg := compatSeamCfg(t, "")
	cfg.Server.BearerSessionCompat = false
	h := newCompatSeamHarness(t, cfg)
	status, _, data := postCompat(t, h.router.URL+"/v1beta/models/gem-sess:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`,
		map[string]string{"Authorization": "Bearer " + compatSeamTokA})
	if status != 200 {
		t.Fatalf("disabled status=%d body=%s", status, data)
	}
	if got := h.header("X-Session-Out"); got != "" {
		t.Fatalf("disabled must not select compat identity: %q", got)
	}
	status, _, data = postCompat(t, h.router.URL+"/v1beta/models/gem-native:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`,
		map[string]string{"X-Model-Router-Reasoning-Effort": "low"})
	if status != 200 {
		t.Fatalf("disabled effort status=%d body=%s", status, data)
	}
	if strings.Contains(string(h.lastBody), "thinkingLevel") {
		t.Fatalf("disabled effort must not mutate: %s", h.lastBody)
	}
}

func TestCompatSeamStreamingAndConcurrent(t *testing.T) {
	h := newCompatSeamHarness(t, compatSeamCfg(t, ""))
	// Streaming uses the same hook: compat session resolves, SSE framing kept.
	status, hdr, _ := postCompat(t, h.router.URL+"/v1beta/models/gem-sess:streamGenerateContent?alt=sse",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}],"sessionId":"abc"}`,
		map[string]string{"Authorization": "Bearer " + compatSeamTokA})
	if status != 200 {
		t.Fatalf("stream status=%d", status)
	}
	if ct := hdr.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("sse content-type=%q", ct)
	}
	if got := h.header("X-Session-Out"); got != "agy-acp-6cc43f858fbb763301637b5af970e2a46b46f461f27e5a0f41e009c59b827b25" {
		t.Fatalf("stream must resolve compat session: %q", got)
	}
	// Concurrent distinct IDs keep retry identity stable with no shared state.
	var wg sync.WaitGroup
	errs := make([]string, 8)
	toks := []string{
		"router-session-11111111-1111-4111-8111-111111111111",
		"router-session-22222222-2222-4222-8222-222222222222",
		"router-session-33333333-3333-4333-8333-333333333333",
		"router-session-44444444-4444-4444-8444-444444444444",
		"router-session-55555555-5555-4555-8555-555555555555",
		"router-session-66666666-6666-4666-8666-666666666666",
		"router-session-77777777-7777-4777-8777-777777777777",
		"router-session-88888888-8888-4888-8888-888888888888",
	}
	for i, tok := range toks {
		wg.Add(1)
		go func(i int, tok string) {
			defer wg.Done()
			status, _, data := postCompat(t, h.router.URL+"/v1beta/models/gem-sess:generateContent",
				`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`,
				map[string]string{"X-Goog-Api-Key": tok})
			if status != 200 {
				errs[i] = string(data)
			}
		}(i, tok)
	}
	wg.Wait()
	for i, e := range errs {
		if e != "" {
			t.Fatalf("request %d failed: %s", i, e)
		}
	}
}
