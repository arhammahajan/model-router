package server

import (
	"encoding/json"
	"fmt"
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

// rdReviewCfg builds providers for every default dialect plus a session
// provider and a counting provider; routes cover the six bridge pairs the
// slice tests do not already exercise end to end.
func rdReviewCfg(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "router.toml")
	body := `
[server]
auth_mode = "none"
allow_dynamic_routes = true
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

[providers.sessup]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
api_key = "sess-secret"
session_header = "x-sess-out"
session_prompt_cache_key = true
[providers.sessup.litellm_kwargs]
headers = { "X-Legacy" = "legacy" }

[providers.countup]
dialect = "anthropic"
base_url = "http://127.0.0.1:9"
api_key = "anth-secret"
count_tokens = true

[[models]]
name = "a2r"
provider = "anthup"
model = "resp-x"
dialect = "openai-responses"

[[models]]
name = "a2g"
provider = "gemup"
model = "gem-x"
dialect = "gemini"

[[models]]
name = "r2a"
provider = "chatup"
model = "claude-x"
dialect = "anthropic"

[[models]]
name = "r2g"
provider = "chatup"
model = "gem-x"
dialect = "gemini"

[[models]]
name = "g2r"
provider = "respup"
model = "resp-x"
dialect = "openai-responses"

[[models]]
name = "g2a"
provider = "anthup"
model = "claude-x"
dialect = "anthropic"

[[models]]
name = "aff-chat"
provider = "sessup"
model = "sess-up"

[[models]]
name = "aff-x"
provider = "sessup"
model = "sess-ax"
dialect = "anthropic"
aliases = ["aff-alias"]

[[models]]
name = "dis-x"
provider = "chatup"
model = "gone"
dialect = "gemini"
enabled = false
aliases = ["dis-alias"]

[[models]]
name = "cnt"
provider = "countup"
model = "claude-x"

[[models]]
name = "cnt-away"
provider = "countup"
model = "gpt-x"
dialect = "openai-chat"

[[models]]
name = "gem-native"
provider = "gemup"
model = "gem-plain"

[[models]]
name = "gem-sse-native"
provider = "gemup"
model = "gem-sse"
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

type rdReviewHarness struct {
	upstream *httptest.Server
	router   *httptest.Server
	mu       sync.Mutex
	lastPath string
	lastRaw  string
	lastBody []byte
	lastHdr  http.Header
	calls    int
}

func newRdReviewHarness(t *testing.T) *rdReviewHarness {
	t.Helper()
	h := &rdReviewHarness{}
	h.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.lastPath = r.URL.Path
		h.lastRaw = r.URL.RawQuery
		h.lastBody = body
		h.lastHdr = r.Header.Clone()
		h.calls++
		h.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/count_tokens"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"input_tokens":5}`))
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
		case strings.HasSuffix(r.URL.Path, "/responses"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","status":"completed","model":"m","output":[{"type":"message","id":"m1","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
		case strings.HasSuffix(r.URL.Path, "/v1/messages"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
		default:
			// Gemini: non-streaming generateContent answers a single
			// object; streams answer framing per URL (array or SSE).
			// The gem-sse upstream answers SSE to prove preservation.
			if strings.Contains(r.URL.Path, "gem-sse") {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"chunk\":1}\n\n"))
				return
			}
			if strings.Contains(r.URL.Path, ":streamGenerateContent") {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`[{"candidates":[{"content":{"parts":[{"text":"hi"}],"role":"model"},"finishReason":"STOP","index":0}],"modelVersion":"gem","responseId":"r1","usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}]`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"hi"}],"role":"model"},"finishReason":"STOP","index":0}],"modelVersion":"gem","responseId":"r1","usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`))
		}
	}))
	t.Cleanup(h.upstream.Close)
	cfg := rdReviewCfg(t)
	for _, prov := range cfg.Providers {
		prov.BaseURL = h.upstream.URL
	}
	srv := New(cfg)
	h.router = httptest.NewServer(srv.Handler())
	t.Cleanup(h.router.Close)
	return h
}

func rdPost(t *testing.T, url, body string, headers map[string]string) (int, http.Header, []byte) {
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
	return resp.StatusCode, resp.Header, data
}

func (h *rdReviewHarness) reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = 0
	h.lastPath, h.lastRaw, h.lastBody, h.lastHdr = "", "", nil, nil
}

func (h *rdReviewHarness) last() (string, string, []byte, http.Header, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastPath, h.lastRaw, h.lastBody, h.lastHdr.Clone(), h.calls
}

// TestRdReviewRemainingBridges drives the six pairs the slice tests do not
// already cover end to end: destination selects URL, provider keeps auth,
// translation preserves the client contract shape.
func TestRdReviewRemainingBridges(t *testing.T) {
	h := newRdReviewHarness(t)
	anthBody := `{"model":"%s","max_tokens":8,"messages":[{"role":"user","content":"Hi"}]}`
	cases := []struct {
		name, url, body, wantPath, wantClient, wantAuth, noAuth, wantOptIn string
	}{
		{"a2r anthropic->responses", "/v1/messages", fmt.Sprintf(anthBody, "a2r"),
			"/responses", `"type":"message"`, "X-Api-Key:anth-secret", "Authorization", "Openai-Beta"},
		{"a2g anthropic->gemini", "/v1/messages", fmt.Sprintf(anthBody, "a2g"),
			"/v1beta/models/gem-x:generateContent", `"type":"message"`, "X-Goog-Api-Key:gem-secret", "Authorization", ""},
		{"r2a responses->anthropic", "/v1/responses", `{"model":"r2a","input":"hi"}`,
			"/v1/messages", `"object":"response"`, "Authorization:Bearer chat-secret", "X-Api-Key", "Anthropic-Beta"},
		{"r2g responses->gemini", "/v1/responses", `{"model":"r2g","input":"hi"}`,
			"/v1beta/models/gem-x:generateContent", `"object":"response"`, "Authorization:Bearer chat-secret", "", ""},
	}
	for _, tc := range cases {
		h.reset()
		status, _, data := rdPost(t, h.router.URL+tc.url, tc.body, map[string]string{"Authorization": "Bearer evil", "X-Goog-Api-Key": "evil", "Openai-Beta": "evil-opt", "Anthropic-Beta": "evil-opt"})
		if status != 200 {
			t.Fatalf("%s status=%d body=%s", tc.name, status, data)
		}
		if !strings.Contains(string(data), tc.wantClient) {
			t.Fatalf("%s client shape missing %s: %s", tc.name, tc.wantClient, data)
		}
		path, raw, _, hdr, calls := h.last()
		if calls != 1 || !strings.Contains(path, tc.wantPath) {
			t.Fatalf("%s upstream path=%q calls=%d want %q", tc.name, path, calls, tc.wantPath)
		}
		kv := strings.SplitN(tc.wantAuth, ":", 2)
		if hdr.Get(kv[0]) != kv[1] {
			t.Fatalf("%s provider auth must stay, got %v", tc.name, hdr)
		}
		if tc.noAuth != "" && hdr.Get(tc.noAuth) != "" {
			t.Fatalf("%s foreign credential leaked: %v", tc.name, hdr)
		}
		if raw != "" {
			t.Fatalf("%s query must not forward: %q", tc.name, raw)
		}
		// Destination opt-ins cross; every other dialect's do not.
		for _, opt := range []string{"Openai-Beta", "Anthropic-Beta"} {
			got := hdr.Get(opt)
			if opt == tc.wantOptIn {
				if got != "evil-opt" {
					t.Fatalf("%s dest opt-in %s must survive: %v", tc.name, opt, hdr)
				}
				continue
			}
			if got != "" {
				t.Fatalf("%s wrong-dialect opt-in %s leaked: %v", tc.name, opt, hdr)
			}
		}
	}
	// Gemini ingress bridges (model in URL).
	for _, tc := range []struct{ model, wantPath, wantAuth string }{
		{"g2r", "/responses", "Authorization:Bearer resp-secret"},
		{"g2a", "/v1/messages", "X-Api-Key:anth-secret"},
	} {
		h.reset()
		status, _, data := rdPost(t, h.router.URL+"/v1beta/models/"+tc.model+":generateContent",
			`{"contents":[{"parts":[{"text":"hi"}]}]}`, nil)
		if status != 200 {
			t.Fatalf("gemini %s status=%d body=%s", tc.model, status, data)
		}
		if !strings.Contains(string(data), "candidates") {
			t.Fatalf("gemini %s client shape: %s", tc.model, data)
		}
		path, _, _, hdr, calls := h.last()
		if calls != 1 || !strings.Contains(path, tc.wantPath) {
			t.Fatalf("gemini %s path=%q calls=%d", tc.model, path, calls)
		}
		kv := strings.SplitN(tc.wantAuth, ":", 2)
		if hdr.Get(kv[0]) != kv[1] {
			t.Fatalf("gemini %s auth: %v", tc.model, hdr)
		}
	}
}

// TestRdReviewStreamSelection proves stream framing follows the destination:
// Chat natives force SSE while Gemini preserves the upstream content type
// (JSON array stays JSON, SSE stays SSE).
func TestRdReviewStreamSelection(t *testing.T) {
	h := newRdReviewHarness(t)
	// Chat native stream (dynamic provider-default route) forces SSE.
	h.reset()
	status, rhdr, _ := rdPost(t, h.router.URL+"/v1/chat/completions",
		`{"model":"chatup/dyn-native","messages":[{"role":"user","content":"Hi"}],"stream":true}`, nil)
	if status != 200 {
		t.Fatalf("dynamic native stream status=%d", status)
	}
	if ct := rhdr.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("chat native stream must force SSE, got %q", ct)
	}
	// Gemini native with a JSON-array upstream preserves application/json.
	req, _ := http.NewRequest(http.MethodPost, h.router.URL+"/v1beta/models/gem-native:streamGenerateContent",
		strings.NewReader(`{"contents":[{"parts":[{"text":"hi"}]}]}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("gemini native stream status=%d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("gemini native must preserve JSON framing, got %q", ct)
	}
	// Gemini native with an SSE upstream preserves the SSE framing.
	req2, _ := http.NewRequest(http.MethodPost, h.router.URL+"/v1beta/models/gem-sse-native:streamGenerateContent?alt=sse",
		strings.NewReader(`{"contents":[{"parts":[{"text":"hi"}]}]}`))
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if ct := resp2.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("gemini SSE must stay SSE, got %q", ct)
	}
}

// TestRdReviewAffinity proves native bodies stay byte-identical (unknown
// fields, bigint, prompt_cache_key) while translated routes consume the
// cache key and keep a single session identity.
func TestRdReviewAffinity(t *testing.T) {
	h := newRdReviewHarness(t)
	big := `{"model":"aff-chat","messages":[{"role":"user","content":"Hi"}],"mystery":{"n":9007199254740993},"prompt_cache_key":"sess-cache-1"}`
	h.reset()
	status, _, _ := rdPost(t, h.router.URL+"/v1/chat/completions", big, map[string]string{"Session-Id": "sess-1"})
	if status != 200 {
		t.Fatalf("native status=%d", status)
	}
	_, _, wbody, whdr, _ := h.last()
	if !strings.Contains(string(wbody), "9007199254740993") {
		t.Fatalf("native must preserve bigint: %s", wbody)
	}
	if !strings.Contains(string(wbody), `"prompt_cache_key":"sess-cache-1"`) {
		t.Fatalf("native must preserve cache key: %s", wbody)
	}
	if whdr.Get("X-Sess-Out") != "sess-1" {
		t.Fatalf("native session: %v", whdr)
	}
	if _, ok := whdr["X-Legacy"]; ok {
		t.Fatalf("native must not carry legacy: %v", whdr)
	}
	// Translated via alias: same dest, strict-clean body, cache key consumed.
	h.reset()
	status, _, _ = rdPost(t, h.router.URL+"/v1/chat/completions",
		`{"model":"aff-alias","messages":[{"role":"user","content":"Hi"}],"prompt_cache_key":"sess-cache-9"}`,
		map[string]string{"Session-Id": "sess-2", "Anthropic-Beta": "ab", "Openai-Beta": "ob"})
	if status != 200 {
		t.Fatalf("translated status=%d", status)
	}
	_, _, wbody, whdr, _ = h.last()
	var wire map[string]json.RawMessage
	_ = json.Unmarshal(wbody, &wire)
	if _, ok := wire["prompt_cache_key"]; ok {
		t.Fatalf("translated must consume cache key: %s", wbody)
	}
	if whdr.Get("X-Sess-Out") != "sess-2" {
		t.Fatalf("translated session: %v", whdr)
	}
	if whdr.Get("Anthropic-Beta") != "ab" {
		t.Fatalf("dest opt-in must survive: %v", whdr)
	}
	if _, ok := whdr["Openai-Beta"]; ok {
		t.Fatalf("wrong-dialect opt-in must drop: %v", whdr)
	}
	if whdr.Get("X-Legacy") != "legacy" {
		t.Fatalf("translated must carry legacy: %v", whdr)
	}
	if whdr.Get("Authorization") != "Bearer sess-secret" {
		t.Fatalf("provider auth must stay bearer: %v", whdr)
	}
}

// TestRdReviewReservationDynamicAndCount covers disabled reservation,
// dynamic provider-default fallback and the two count-token gates.
func TestRdReviewReservationDynamicAndCount(t *testing.T) {
	h := newRdReviewHarness(t)
	for _, model := range []string{"dis-x", "dis-alias"} {
		h.reset()
		status, _, data := rdPost(t, h.router.URL+"/v1/chat/completions",
			`{"model":"`+model+`","messages":[{"role":"user","content":"Hi"}]}`, nil)
		if status != 404 {
			t.Fatalf("disabled %s must be reserved, got %d %s", model, status, data)
		}
		if _, _, _, _, calls := h.last(); calls != 0 {
			t.Fatalf("disabled %s must have 0 upstream I/O", model)
		}
	}
	// Dynamic route inherits the provider default (chat) natively.
	h.reset()
	status, _, data := rdPost(t, h.router.URL+"/v1/chat/completions",
		`{"model":"chatup/dyn-x","messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 200 {
		t.Fatalf("dynamic status=%d %s", status, data)
	}
	path, _, wbody, _, _ := h.last()
	if !strings.Contains(path, "/chat/completions") {
		t.Fatalf("dynamic must use provider default path: %q", path)
	}
	if !strings.Contains(string(wbody), `"model":"dyn-x"`) {
		t.Fatalf("dynamic must rewrite public name: %s", wbody)
	}
	// Count away from anthropic refuses with 0 I/O and ingress shape.
	h.reset()
	status, _, data = rdPost(t, h.router.URL+"/v1/messages/count_tokens",
		`{"model":"cnt-away","messages":[{"role":"user","content":"Hi"}],"max_tokens":4}`, nil)
	if status != 400 {
		t.Fatalf("count away must refuse, got %d %s", status, data)
	}
	if !strings.Contains(string(data), "invalid_request_error") {
		t.Fatalf("count refusal must be ingress-native: %s", data)
	}
	if _, _, _, _, calls := h.last(); calls != 0 {
		t.Fatal("count refusal must have 0 upstream I/O")
	}
	// Both gates pass: real count forwards.
	h.reset()
	status, _, data = rdPost(t, h.router.URL+"/v1/messages/count_tokens",
		`{"model":"cnt","messages":[{"role":"user","content":"Hi"}],"max_tokens":4}`, nil)
	if status != 200 || !strings.Contains(string(data), "input_tokens") {
		t.Fatalf("count success status=%d %s", status, data)
	}
}

// TestRdReviewDiagnosticsHonest proves provider_details keeps the configured
// provider dialect (never the route override) and counts aliases as routes.
func TestRdReviewDiagnosticsHonest(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "router.toml")
	body := `
[server]
auth_mode = "required"
api_keys = ["diag-key"]
allow_dynamic_routes = false

[retry]
max_attempts = 1

[providers.chatup]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
api_key = "chat-secret"

[[models]]
name = "over"
provider = "chatup"
model = "claude-x"
dialect = "anthropic"
aliases = ["over-alias"]
`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(cfg)
	router := httptest.NewServer(srv.Handler())
	defer router.Close()
	req, _ := http.NewRequest(http.MethodGet, router.URL+"/diagnostics", nil)
	req.Header.Set("Authorization", "Bearer diag-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("diagnostics status=%d %s", resp.StatusCode, data)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	var details []map[string]json.RawMessage
	if err := json.Unmarshal(payload["provider_details"], &details); err != nil || len(details) != 1 {
		t.Fatalf("provider_details: %s", data)
	}
	var dialect string
	_ = json.Unmarshal(details[0]["dialect"], &dialect)
	if dialect != "openai-chat" {
		t.Fatalf("diagnostics must report provider default, got %q", dialect)
	}
	var routes float64
	_ = json.Unmarshal(details[0]["enabled_routes"], &routes)
	if routes != 2 {
		t.Fatalf("enabled_routes must count alias, got %v", routes)
	}
}

// TestRdReviewListingNoDest proves the listing carries no per-route
// destination outside the capability contract: effective destinations
// render ONLY as configured transport facts inside metadata.router.
func TestRdReviewListingNoDest(t *testing.T) {
	h := newRdReviewHarness(t)
	req, _ := http.NewRequest(http.MethodGet, h.router.URL+"/v1/models", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("models status=%d", resp.StatusCode)
	}
	var out struct {
		Data []struct {
			ID       string          `json:"id"`
			Metadata json.RawMessage `json:"metadata"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) == 0 {
		t.Fatalf("listing empty: %s", data)
	}
	for _, row := range out.Data {
		var meta map[string]json.RawMessage
		if err := json.Unmarshal(row.Metadata, &meta); err != nil {
			t.Fatal(err)
		}
		routerRaw, ok := meta["router"]
		if !ok {
			t.Fatalf("row %q must carry metadata.router: %s", row.ID, row.Metadata)
		}
		var contract map[string]any
		if err := json.Unmarshal(routerRaw, &contract); err != nil {
			t.Fatal(err)
		}
		if _, ok := contract["protocols"]; !ok {
			t.Fatalf("row %q router must carry configured transport facts", row.ID)
		}
		delete(meta, "router")
		rest, _ := json.Marshal(meta)
		if strings.Contains(string(rest), "anthropic") || strings.Contains(string(rest), "dialect") {
			t.Fatalf("listing must not smuggle dest outside router: %s", rest)
		}
	}
}
