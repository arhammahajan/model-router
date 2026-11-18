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

	"github.com/arhammahajan/model-router/internal/capability"
	"github.com/arhammahajan/model-router/internal/config"
)

func strPtr2(s string) *string { v := s; return &v }

func mustKind() string { return capability.KindOverride }

func mustReasoningFor(c capability.Contract) capability.ReasoningCapabilities {
	return capability.ReasoningFor(c)
}

func reasoningBridgeCfg(t *testing.T, extra string) *config.Config {
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

[providers.respup]
dialect = "openai-responses"
base_url = "http://127.0.0.1:9"
api_key = "resp-secret"

[providers.lossyresp]
dialect = "openai-responses"
base_url = "http://127.0.0.1:9"
api_key = "resp-secret"
compatibility = "lossy"

[[models]]
name = "bridge-chat"
provider = "respup"
model = "gpt-4.1"

[[models]]
name = "bridge-chat-lossy"
provider = "lossyresp"
model = "gpt-4.1"
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

type reasoningHarness struct {
	upstream *httptest.Server
	router   *httptest.Server
	mu       sync.Mutex
	lastBody []byte
	calls    int
}

func newReasoningHarness(t *testing.T, cfg *config.Config) *reasoningHarness {
	t.Helper()
	h := &reasoningHarness{}
	h.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.lastBody = body
		h.calls++
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_up","object":"response","created_at":1,"status":"completed","model":"gpt-4.1","output":[{"id":"msg_up","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hello","annotations":[]}]}],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}`))
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

func postChat(t *testing.T, url, body string) (int, http.Header, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, data
}

func upstreamMap(t *testing.T, h *reasoningHarness) map[string]any {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	var v map[string]any
	if err := json.Unmarshal(h.lastBody, &v); err != nil {
		t.Fatalf("upstream not JSON: %v %s", err, h.lastBody)
	}
	return v
}

func upstreamCalls(h *reasoningHarness) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

func TestChatReasoningExactWireNoLoss(t *testing.T) {
	h := newReasoningHarness(t, reasoningBridgeCfg(t, ""))
	status, hdr, _ := postChat(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-chat","messages":[{"role":"user","content":"Hi"}],"reasoning_effort":"medium"}`)
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	if hdr.Get("X-Model-Router-Loss") != "" {
		t.Fatalf("exact must emit no loss header")
	}
	up := upstreamMap(t, h)
	reas, ok := up["reasoning"].(map[string]any)
	if !ok || reas["effort"] != "medium" {
		t.Fatalf("wire must carry reasoning.effort medium: %v", up)
	}
	if _, has := up["reasoning_effort"]; has {
		t.Fatalf("must not emit Chat field: %v", up)
	}
}

func TestChatReasoningStrictRefusesExplicitFalse(t *testing.T) {
	cfg := reasoningBridgeCfg(t, `
[capabilities.overrides.bridge-chat.reasoning]
supported = false
[capabilities.overrides.bridge-chat-lossy.reasoning]
supported = false
`)
	h := newReasoningHarness(t, cfg)
	status, _, data := postChat(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-chat","messages":[{"role":"user","content":"Hi"}],"reasoning_effort":"medium"}`)
	if status != 400 || !strings.Contains(string(data), "translation_loss") {
		t.Fatalf("strict must refuse pre-I/O: %d %s", status, data)
	}
	if got := upstreamCalls(h); got != 0 {
		t.Fatalf("pre-I/O refusal must not touch upstream: %d", got)
	}
	// Lossy drops with header, no reasoning on wire.
	status, hdr, _ := postChat(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-chat-lossy","messages":[{"role":"user","content":"Hi"}],"reasoning_effort":"medium"}`)
	if status != 200 {
		t.Fatalf("lossy status=%d", status)
	}
	if hdr.Get("X-Model-Router-Loss") == "" {
		t.Fatalf("lossy must carry loss header")
	}
	var loss []map[string]any
	if err := json.Unmarshal([]byte(hdr.Get("X-Model-Router-Loss")), &loss); err != nil {
		t.Fatalf("loss header JSON: %v", err)
	}
	if len(loss) != 1 || loss[0]["field"] != "reasoning_effort" || loss[0]["action"] != "dropped" {
		t.Fatalf("loss must name origin: %v", loss)
	}
	up := upstreamMap(t, h)
	if _, has := up["reasoning"]; has {
		t.Fatalf("dropped must not emit reasoning: %v", up)
	}
}

func TestChatReasoningEnumMismatch(t *testing.T) {
	cfg := reasoningBridgeCfg(t, `
[capabilities.overrides.bridge-chat.reasoning.effort]
values = ["low"]
[capabilities.overrides.bridge-chat-lossy.reasoning.effort]
values = ["low"]
`)
	h := newReasoningHarness(t, cfg)
	status, _, _ := postChat(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-chat","messages":[{"role":"user","content":"Hi"}],"reasoning_effort":"high"}`)
	if status != 400 {
		t.Fatalf("enum mismatch strict must 400, got %d", status)
	}
	if got := upstreamCalls(h); got != 0 {
		t.Fatalf("must not touch upstream: %d", got)
	}
	status, hdr, _ := postChat(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-chat-lossy","messages":[{"role":"user","content":"Hi"}],"reasoning_effort":"high"}`)
	if status != 200 || hdr.Get("X-Model-Router-Loss") == "" {
		t.Fatalf("lossy must drop with header: %d", status)
	}
	up := upstreamMap(t, h)
	if _, has := up["reasoning"]; has {
		t.Fatalf("mismatch must not emit: %v", up)
	}
}

func TestChatReasoningEmptyVsUnknown(t *testing.T) {
	cfg := reasoningBridgeCfg(t, `
[capabilities.overrides.bridge-chat.reasoning.effort]
values = []
`)
	h := newReasoningHarness(t, cfg)
	status, _, _ := postChat(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-chat","messages":[{"role":"user","content":"Hi"}],"reasoning_effort":"low"}`)
	if status != 400 {
		t.Fatalf("known empty must drop: %d", status)
	}
	// Unknown (no overrides) passes exact.
	h2 := newReasoningHarness(t, reasoningBridgeCfg(t, ""))
	status, _, _ = postChat(t, h2.router.URL+"/v1/chat/completions",
		`{"model":"bridge-chat","messages":[{"role":"user","content":"Hi"}],"reasoning_effort":"low"}`)
	if status != 200 {
		t.Fatalf("unknown must pass: %d", status)
	}
}

func TestChatReasoningNoneHandling(t *testing.T) {
	h := newReasoningHarness(t, reasoningBridgeCfg(t, ""))
	status, hdr, _ := postChat(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-chat","messages":[{"role":"user","content":"Hi"}],"reasoning_effort":"none"}`)
	if status != 200 || hdr.Get("X-Model-Router-Loss") != "" {
		t.Fatalf("unknown none must be exact: %d header=%q", status, hdr.Get("X-Model-Router-Loss"))
	}
	up := upstreamMap(t, h)
	if up["reasoning"].(map[string]any)["effort"] != "none" {
		t.Fatalf("none literal: %v", up)
	}
	// Known enum without none drops.
	cfg := reasoningBridgeCfg(t, `
[capabilities.overrides.bridge-chat.reasoning.effort]
values = ["low", "medium"]
`)
	h2 := newReasoningHarness(t, cfg)
	status, _, _ = postChat(t, h2.router.URL+"/v1/chat/completions",
		`{"model":"bridge-chat","messages":[{"role":"user","content":"Hi"}],"reasoning_effort":"none"}`)
	if status != 400 {
		t.Fatalf("enum without none must drop: %d", status)
	}
}

func TestChatReasoningNoClamp(t *testing.T) {
	cfg := reasoningBridgeCfg(t, `
[capabilities.overrides.bridge-chat-lossy.reasoning.effort]
values = ["low", "medium"]
`)
	h := newReasoningHarness(t, cfg)
	status, _, _ := postChat(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-chat-lossy","messages":[{"role":"user","content":"Hi"}],"reasoning_effort":"minimal"}`)
	if status != 200 {
		t.Fatalf("lossy status=%d", status)
	}
	up := upstreamMap(t, h)
	if _, has := up["reasoning"]; has {
		t.Fatalf("minimal must not clamp to low: %v", up)
	}
}

func TestChatReasoningMalformedAndConflictBothModes(t *testing.T) {
	for _, model := range []string{"bridge-chat", "bridge-chat-lossy"} {
		status, _, _ := postChat(t, newReasoningHarness(t, reasoningBridgeCfg(t, "")).router.URL+"/v1/chat/completions",
			`{"model":"`+model+`","messages":[{"role":"user","content":"Hi"}],"reasoning_effort":123}`)
		if status != 400 {
			t.Fatalf("malformed %s must 400, got %d", model, status)
		}
		h := newReasoningHarness(t, reasoningBridgeCfg(t, ""))
		before := upstreamCalls(h)
		status, _, data := postChat(t, h.router.URL+"/v1/chat/completions",
			`{"model":"`+model+`","messages":[{"role":"user","content":"Hi"}],"reasoning_effort":"medium","extra_body":{"thinking":{"type":"enabled","budget_tokens":100}}}`)
		if status != 400 || !strings.Contains(string(data), "reasoning_effort") {
			t.Fatalf("conflict %s must 400 with origin, got %d %s", model, status, data)
		}
		if got := upstreamCalls(h); got != before {
			t.Fatalf("conflict must not touch upstream")
		}
	}
}

func TestChatReasoningMetadataRouterIgnored(t *testing.T) {
	cfg := reasoningBridgeCfg(t, `
[[models]]
name = "meta-chat"
provider = "respup"
model = "gpt-4.1"
[models.metadata.router.reasoning.effort]
values = ["low"]
`)
	h := newReasoningHarness(t, cfg)
	status, hdr, _ := postChat(t, h.router.URL+"/v1/chat/completions",
		`{"model":"meta-chat","messages":[{"role":"user","content":"Hi"}],"reasoning_effort":"high"}`)
	if status != 200 || hdr.Get("X-Model-Router-Loss") != "" {
		t.Fatalf("raw metadata.router must be ignored (unknown neutral): %d header=%q", status, hdr.Get("X-Model-Router-Loss"))
	}
	up := upstreamMap(t, h)
	if up["reasoning"].(map[string]any)["effort"] != "high" {
		t.Fatalf("wire: %v", up)
	}
}

func TestChatReasoningCanonicalOverrideAliasEffectiveDest(t *testing.T) {
	cfg := reasoningBridgeCfg(t, `
[providers.chatup]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
api_key = "chat-secret"

[[models]]
name = "real-chat"
provider = "chatup"
model = "up-resp"
dialect = "openai-responses"
aliases = ["alias-chat"]

[capabilities.overrides.real-chat.reasoning.effort]
values = ["low", "medium"]

[capabilities.overrides.alias-chat.reasoning.effort]
values = ["high"]
`)
	h := newReasoningHarness(t, cfg)
	// Alias resolves to canonical override (medium exact), alias-key ignored.
	status, _, _ := postChat(t, h.router.URL+"/v1/chat/completions",
		`{"model":"alias-chat","messages":[{"role":"user","content":"Hi"}],"reasoning_effort":"medium"}`)
	if status != 200 {
		t.Fatalf("alias must inherit canonical facts: %d", status)
	}
	up := upstreamMap(t, h)
	if up["reasoning"].(map[string]any)["effort"] != "medium" {
		t.Fatalf("alias wire: %v", up)
	}
	// high is outside canonical values even though alias-key claims it.
	status, _, _ = postChat(t, h.router.URL+"/v1/chat/completions",
		`{"model":"alias-chat","messages":[{"role":"user","content":"Hi"}],"reasoning_effort":"high"}`)
	if status != 400 {
		t.Fatalf("alias-key override must not apply: %d", status)
	}
	// Effective-dest inheritance: provider chat default + route override
	// responses still plans Responses effort.
	status, _, _ = postChat(t, h.router.URL+"/v1/chat/completions",
		`{"model":"real-chat","messages":[{"role":"user","content":"Hi"}],"reasoning_effort":"low"}`)
	if status != 200 {
		t.Fatalf("effective-dest must plan: %d", status)
	}
}

func TestChatReasoningSnapshotOwnership(t *testing.T) {
	cfg := reasoningBridgeCfg(t, `
[capabilities.overrides.bridge-chat.reasoning.effort]
values = ["low"]
`)
	s := New(cfg)
	snap := s.Registry().Current()
	m, p, fault := snap.Resolve("bridge-chat")
	if fault != 0 {
		t.Fatalf("resolve fault=%v", fault)
	}
	c1 := snap.RequestCapability(m, p)
	vals, ok := capability.StringListValue([]string{"mutated"})
	if !ok {
		t.Fatal("bad test value")
	}
	_ = c1.Put("reasoning.effort.values", vals, strPtr2("test"), mustKind())
	c2 := snap.RequestCapability(m, p)
	caps := mustReasoningFor(c2)
	if len(caps.EffortValues.List) != 1 || caps.EffortValues.List[0] != "low" {
		t.Fatalf("snapshot must own facts: %+v", caps.EffortValues)
	}
}

func TestChatReasoningNativeUnchanged(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "router.toml")
	body := `
[server]
auth_mode = "none"
allow_dynamic_routes = false
[retry]
max_attempts = 1
[providers.chatup]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
api_key = "s"
[[models]]
name = "native-chat"
provider = "chatup"
model = "up-model"
`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	var gotBody []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"up-model","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer up.Close()
	for _, prov := range cfg.Providers {
		prov.BaseURL = up.URL
	}
	srv := httptest.NewServer(New(cfg).Handler())
	defer srv.Close()
	raw := `{"model":"native-chat","messages":[{"role":"user","content":"hi"}],"unknown_field_xyz":1,"bigint":9007199254740993}`
	status, _, _ := postChat(t, srv.URL+"/v1/chat/completions", raw)
	// Unknown fields on native bypass are preserved byte-identical (except
	// model rewrite when alias differs; here names differ so model rewrites
	// but unknown members and bigint survive exactly).
	if status != 200 {
		t.Fatalf("native status=%d", status)
	}
	if !strings.Contains(string(gotBody), "unknown_field_xyz") || !strings.Contains(string(gotBody), "9007199254740993") {
		t.Fatalf("native must preserve unknown/bigint: %s", gotBody)
	}
}

func TestChatReasoningOrdinaryResponseStillTranslates(t *testing.T) {
	h := newReasoningHarness(t, reasoningBridgeCfg(t, ""))
	status, _, data := postChat(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-chat","messages":[{"role":"user","content":"Hi"}],"reasoning_effort":"medium"}`)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	choices, _ := v["choices"].([]any)
	msg, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "hello" {
		t.Fatalf("ordinary fixture must still translate: %v", v)
	}
}

func TestChatReasoningPrivateResponseStillFailsClosed(t *testing.T) {
	cfg := reasoningBridgeCfg(t, "")
	h := newReasoningHarness(t, cfg)
	h.upstream.Config.ErrorLog = nil
	// Swap upstream to emit a private reasoning output item.
	h.upstream.Close()
	priv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_up","object":"response","created_at":1,"status":"completed","model":"gpt-4.1","output":[{"id":"rs_1","type":"reasoning","status":"completed"}],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}`))
	}))
	defer priv.Close()
	for _, prov := range cfg.Providers {
		prov.BaseURL = priv.URL
	}
	srv := httptest.NewServer(New(cfg).Handler())
	defer srv.Close()
	status, _, data := postChat(t, srv.URL+"/v1/chat/completions",
		`{"model":"bridge-chat","messages":[{"role":"user","content":"Hi"}],"reasoning_effort":"medium"}`)
	if status != 502 {
		t.Fatalf("reasoning output must stay fail-closed, got %d %s", status, data)
	}
}
