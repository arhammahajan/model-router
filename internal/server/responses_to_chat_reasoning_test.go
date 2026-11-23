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

	"github.com/arhammahajan/router/internal/capability"
	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/router"
)

// Reverse named-effort: Responses reasoning.effort -> Chat reasoning_effort.
// Hermetic fakes only: TempDir config, httptest upstream/router, no env creds.

func revBridgeCfg(t *testing.T, extra string) *config.Config {
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

[providers.chatup]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
api_key = "chat-secret"

[providers.lossychat]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
api_key = "chat-secret"
compatibility = "lossy"

[[models]]
name = "bridge-resp"
provider = "chatup"
model = "gpt-4o-mini"

[[models]]
name = "bridge-resp-lossy"
provider = "lossychat"
model = "gpt-4o-mini"
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

type revHarness struct {
	upstream *httptest.Server
	router   *httptest.Server
	mu       sync.Mutex
	lastBody []byte
	calls    int
}

func newRevHarness(t *testing.T, cfg *config.Config) *revHarness {
	t.Helper()
	h := &revHarness{}
	h.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.lastBody = body
		h.calls++
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-up","object":"chat.completion","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`))
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

func postRev(t *testing.T, url, body string) (int, http.Header, []byte) {
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

func revUpstream(t *testing.T, h *revHarness) map[string]any {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	var v map[string]any
	if err := json.Unmarshal(h.lastBody, &v); err != nil {
		t.Fatalf("upstream not JSON: %v %s", err, h.lastBody)
	}
	return v
}

func revCalls(h *revHarness) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

func TestRevExactWireNoLoss(t *testing.T) {
	h := newRevHarness(t, revBridgeCfg(t, ""))
	status, hdr, _ := postRev(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"Hi","reasoning":{"effort":"medium"}}`)
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	if hdr.Get("X-Model-Router-Loss") != "" {
		t.Fatalf("exact must emit no loss header")
	}
	up := revUpstream(t, h)
	if up["reasoning_effort"] != "medium" {
		t.Fatalf("wire must carry reasoning_effort medium: %v", up)
	}
	if _, has := up["reasoning"]; has {
		t.Fatalf("must not emit Responses field on Chat wire: %v", up)
	}
}

func TestRevStrictRefusesExplicitFalse(t *testing.T) {
	cfg := revBridgeCfg(t, `
[capabilities.overrides.bridge-resp.reasoning]
supported = false
[capabilities.overrides.bridge-resp-lossy.reasoning]
supported = false
`)
	h := newRevHarness(t, cfg)
	status, _, data := postRev(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"Hi","reasoning":{"effort":"medium"}}`)
	if status != 400 || !strings.Contains(string(data), "translation_loss") {
		t.Fatalf("strict must refuse pre-I/O: %d %s", status, data)
	}
	if got := revCalls(h); got != 0 {
		t.Fatalf("pre-I/O refusal must not touch upstream: %d", got)
	}
	status, hdr, _ := postRev(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-resp-lossy","input":"Hi","reasoning":{"effort":"medium"}}`)
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
	if len(loss) != 1 || loss[0]["field"] != "reasoning.effort" || loss[0]["action"] != "dropped" {
		t.Fatalf("loss must name origin: %v", loss)
	}
	up := revUpstream(t, h)
	if _, has := up["reasoning_effort"]; has {
		t.Fatalf("dropped must not emit reasoning_effort: %v", up)
	}
}

func TestRevEnumMismatchAndKnownEmpty(t *testing.T) {
	cfg := revBridgeCfg(t, `
[capabilities.overrides.bridge-resp.reasoning.effort]
values = ["low"]
[capabilities.overrides.bridge-resp-lossy.reasoning.effort]
values = ["low"]
`)
	h := newRevHarness(t, cfg)
	status, _, _ := postRev(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"Hi","reasoning":{"effort":"high"}}`)
	if status != 400 {
		t.Fatalf("enum mismatch strict must 400, got %d", status)
	}
	if got := revCalls(h); got != 0 {
		t.Fatalf("must not touch upstream: %d", got)
	}
	status, hdr, _ := postRev(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-resp-lossy","input":"Hi","reasoning":{"effort":"high"}}`)
	if status != 200 || hdr.Get("X-Model-Router-Loss") == "" {
		t.Fatalf("enum mismatch lossy must drop: %d", status)
	}
	emptyCfg := revBridgeCfg(t, `
[capabilities.overrides.bridge-resp.reasoning.effort]
values = []
`)
	h2 := newRevHarness(t, emptyCfg)
	status, _, _ = postRev(t, h2.router.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"Hi","reasoning":{"effort":"low"}}`)
	if status != 400 {
		t.Fatalf("known empty strict must 400, got %d", status)
	}
	h3 := newRevHarness(t, revBridgeCfg(t, ""))
	status, hdr, _ = postRev(t, h3.router.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"Hi","reasoning":{"effort":"low"}}`)
	if status != 200 || hdr.Get("X-Model-Router-Loss") != "" {
		t.Fatalf("unknown must pass exact: %d header=%q", status, hdr.Get("X-Model-Router-Loss"))
	}
}

func TestRevNoneGatingToggleEffort(t *testing.T) {
	h := newRevHarness(t, revBridgeCfg(t, ""))
	status, hdr, _ := postRev(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"Hi","reasoning":{"effort":"none"}}`)
	if status != 200 || hdr.Get("X-Model-Router-Loss") != "" {
		t.Fatalf("unknown none must be exact: %d header=%q", status, hdr.Get("X-Model-Router-Loss"))
	}
	up := revUpstream(t, h)
	if up["reasoning_effort"] != "none" {
		t.Fatalf("none literal: %v", up)
	}
	cfg := revBridgeCfg(t, `
[capabilities.overrides.bridge-resp.reasoning.effort]
values = ["low", "medium"]
`)
	h2 := newRevHarness(t, cfg)
	status, _, _ = postRev(t, h2.router.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"Hi","reasoning":{"effort":"none"}}`)
	if status != 400 {
		t.Fatalf("enum without none must drop: %d", status)
	}
	toggleCfg := revBridgeCfg(t, `
[capabilities.overrides.bridge-resp.reasoning.toggle]
supported = false
[capabilities.overrides.bridge-resp-lossy.reasoning.toggle]
supported = false
`)
	h3 := newRevHarness(t, toggleCfg)
	status, _, _ = postRev(t, h3.router.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"Hi","reasoning":{"effort":"none"}}`)
	if status != 400 {
		t.Fatalf("toggle=false none strict must 400, got %d", status)
	}
	status, hdr, _ = postRev(t, h3.router.URL+"/v1/responses",
		`{"model":"bridge-resp-lossy","input":"Hi","reasoning":{"effort":"none"}}`)
	if status != 200 || hdr.Get("X-Model-Router-Loss") == "" {
		t.Fatalf("toggle=false none lossy must drop: %d", status)
	}
}

func TestRevMinimalLiteralNoClamp(t *testing.T) {
	h := newRevHarness(t, revBridgeCfg(t, ""))
	status, _, _ := postRev(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"Hi","reasoning":{"effort":"minimal"}}`)
	if status != 200 {
		t.Fatalf("unknown minimal must be exact: %d", status)
	}
	up := revUpstream(t, h)
	if up["reasoning_effort"] != "minimal" {
		t.Fatalf("minimal literal must be preserved: %v", up)
	}
	cfg := revBridgeCfg(t, `
[capabilities.overrides.bridge-resp-lossy.reasoning.effort]
values = ["low", "medium"]
`)
	h2 := newRevHarness(t, cfg)
	status, _, _ = postRev(t, h2.router.URL+"/v1/responses",
		`{"model":"bridge-resp-lossy","input":"Hi","reasoning":{"effort":"minimal"}}`)
	if status != 200 {
		t.Fatalf("lossy status=%d", status)
	}
	up = revUpstream(t, h2)
	if _, has := up["reasoning_effort"]; has {
		t.Fatalf("minimal must not clamp to low: %v", up)
	}
}

func TestRevNullEmptyNoControl(t *testing.T) {
	h := newRevHarness(t, revBridgeCfg(t, ""))
	for _, body := range []string{
		`{"model":"bridge-resp","input":"Hi","reasoning":null}`,
		`{"model":"bridge-resp","input":"Hi"}`,
		`{"model":"bridge-resp","input":"Hi","reasoning":{}}`,
	} {
		status, hdr, _ := postRev(t, h.router.URL+"/v1/responses", body)
		if status != 200 || hdr.Get("X-Model-Router-Loss") != "" {
			t.Fatalf("null/empty must be absent exact: %s -> %d header=%q", body, status, hdr.Get("X-Model-Router-Loss"))
		}
	}
	up := revUpstream(t, h)
	if _, has := up["reasoning_effort"]; has {
		t.Fatalf("absent must not emit: %v", up)
	}
}

func TestRevMalformedBothModesPreIO(t *testing.T) {
	for _, model := range []string{"bridge-resp", "bridge-resp-lossy"} {
		for _, frag := range []string{
			`"reasoning":123`,
			`"reasoning":"medium"`,
			`"reasoning":{"effort":123}`,
			`"reasoning":{"effort":true}`,
			`"reasoning":{"effort":null}`,
			`"reasoning":{"effort":""}`,
			`"reasoning":{"effort":"medium","bogus":1}`,
			`"reasoning":{"summary":null}`,
		} {
			h := newRevHarness(t, revBridgeCfg(t, ""))
			before := revCalls(h)
			status, _, _ := postRev(t, h.router.URL+"/v1/responses",
				`{"model":"`+model+`","input":"Hi",`+frag+`}`)
			if status != 400 {
				t.Fatalf("malformed %s %s must 400, got %d", model, frag, status)
			}
			if got := revCalls(h); got != before {
				t.Fatalf("malformed must not touch upstream: %s", frag)
			}
		}
	}
}

func TestRevSummariesRetainRefusalBothModes(t *testing.T) {
	for _, model := range []string{"bridge-resp", "bridge-resp-lossy"} {
		for _, frag := range []string{
			`"reasoning":{"summary":"detailed"}`,
			`"reasoning":{"generate_summary":"auto"}`,
			`"reasoning":{"effort":"medium","summary":"detailed"}`,
		} {
			h := newRevHarness(t, revBridgeCfg(t, ""))
			before := revCalls(h)
			status, _, _ := postRev(t, h.router.URL+"/v1/responses",
				`{"model":"`+model+`","input":"Hi",`+frag+`}`)
			if status != 400 {
				t.Fatalf("summary %s %s must retain refusal, got %d", model, frag, status)
			}
			if got := revCalls(h); got != before {
				t.Fatalf("summary refusal must not touch upstream")
			}
		}
	}
}

func TestRevMetadataRouterIgnoredAliasAndDialect(t *testing.T) {
	cfg := revBridgeCfg(t, `
[[models]]
name = "meta-resp"
provider = "chatup"
model = "gpt-4o-mini"
[models.metadata.router.reasoning.effort]
values = ["low"]

[providers.respup]
dialect = "openai-responses"
base_url = "http://127.0.0.1:9"
api_key = "resp-secret"

[[models]]
name = "real-resp"
provider = "chatup"
model = "up-chat"
aliases = ["alias-resp"]

[capabilities.overrides.real-resp.reasoning.effort]
values = ["low", "medium"]

[capabilities.overrides.alias-resp.reasoning.effort]
values = ["high"]

[[models]]
name = "over-resp"
provider = "respup"
model = "up-over"
dialect = "openai-chat"

[[models]]
name = "native-resp"
provider = "respup"
model = "up-native"
`)
	h := newRevHarness(t, cfg)
	// Raw metadata.router ignored: unknown neutral.
	status, hdr, _ := postRev(t, h.router.URL+"/v1/responses",
		`{"model":"meta-resp","input":"Hi","reasoning":{"effort":"high"}}`)
	if status != 200 || hdr.Get("X-Model-Router-Loss") != "" {
		t.Fatalf("metadata.router must be ignored: %d header=%q", status, hdr.Get("X-Model-Router-Loss"))
	}
	up := revUpstream(t, h)
	if up["reasoning_effort"] != "high" {
		t.Fatalf("wire: %v", up)
	}
	// Alias resolves to canonical; alias-key ignored.
	status, _, _ = postRev(t, h.router.URL+"/v1/responses",
		`{"model":"alias-resp","input":"Hi","reasoning":{"effort":"medium"}}`)
	if status != 200 {
		t.Fatalf("alias must inherit canonical: %d", status)
	}
	status, _, _ = postRev(t, h.router.URL+"/v1/responses",
		`{"model":"alias-resp","input":"Hi","reasoning":{"effort":"high"}}`)
	if status != 400 {
		t.Fatalf("alias-key must not apply: %d", status)
	}
	// Dialect override: provider responses default, route chat translates.
	status, _, _ = postRev(t, h.router.URL+"/v1/responses",
		`{"model":"over-resp","input":"Hi","reasoning":{"effort":"low"}}`)
	if status != 200 {
		t.Fatalf("effective-dest override must translate: %d", status)
	}
	up = revUpstream(t, h)
	if up["reasoning_effort"] != "low" {
		t.Fatalf("override wire: %v", up)
	}
}

func TestRevSnapshotOwnership(t *testing.T) {
	cfg := revBridgeCfg(t, `
[capabilities.overrides.bridge-resp.reasoning.effort]
values = ["low"]
`)
	s := New(cfg)
	snap := s.Registry().Current()
	m, p, fault := snap.Resolve("bridge-resp")
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

func TestRevLlamaRetainsRefusalBothModes(t *testing.T) {
	for _, compat := range []string{"", "\ncompatibility = \"lossy\"\n"} {
		upHit := 0
		var mu sync.Mutex
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			upHit++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
		}))
		defer up.Close()
		dir := t.TempDir()
		p := filepath.Join(dir, "router.toml")
		body := `
[server]
auth_mode = "none"
allow_dynamic_routes = false
max_generations = 8
[retry]
max_attempts = 1
[llama_cpp]
` + compat + `
[[llama_cpp.servers]]
name = "llama-test"
url = "` + up.URL + `"
`
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Load(p)
		if err != nil {
			t.Fatal(err)
		}
		reg := router.NewRegistry(cfg)
		obs := capability.ExtractLlamaObs(map[string]any{"id": "x"}, map[string]any{"build_info": "llama.cpp"}, "single")
		layer := router.LlamaLayer{Servers: map[string]router.LlamaServerSnapshot{
			"llama-test": {State: router.LlamaReachable, Entries: []router.LlamaEntry{{Name: "llama-chat", Upstream: "llama-chat", Obs: obs}}},
		}}
		if err := reg.CommitLlama(router.LlamaAttempt{Layer: layer, AtWall: router.Wall(1000)}); err != nil {
			t.Fatal(err)
		}
		snap := reg.Current()
		if !snap.HasLlamaProvider("llama-test") {
			t.Fatalf("trusted inventory must mark llama provider")
		}
		srv := NewWithRegistry(cfg, reg)
		rsrv := httptest.NewServer(srv.Handler())
		defer rsrv.Close()
		status, _, _ := postRev(t, rsrv.URL+"/v1/responses",
			`{"model":"llama-chat","input":"Hi","reasoning":{"effort":"medium"}}`)
		if status != 400 {
			t.Fatalf("llama effort must retain refusal (compat=%q), got %d", compat, status)
		}
		mu.Lock()
		got := upHit
		mu.Unlock()
		if got != 0 {
			t.Fatalf("llama refusal must not touch upstream: %d", got)
		}
		// Ordinary config chat providers stay neutral: same snapshot, non-llama
		// provider would translate (covered by bridge tests); here just prove
		// membership is provider-scoped, not name-based.
		if snap.HasLlamaProvider("chatup") {
			t.Fatalf("ordinary provider must not be llama-backed")
		}
	}
}

func TestRevNativeFidelityAndOrdinaryResponses(t *testing.T) {
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
[providers.respup]
dialect = "openai-responses"
base_url = "http://127.0.0.1:9"
api_key = "s"
[[models]]
name = "native-chat"
provider = "chatup"
model = "up-model"
[[models]]
name = "native-resp"
provider = "respup"
model = "up-resp"
`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	var gotChat, gotResp []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(r.URL.Path, "chat") {
			gotChat = b
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"up-model","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
			return
		}
		gotResp = b
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_up","object":"response","created_at":1,"status":"completed","model":"up-resp","output":[{"id":"msg_up","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hello","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
	}))
	defer up.Close()
	for _, prov := range cfg.Providers {
		prov.BaseURL = up.URL
	}
	srv := httptest.NewServer(New(cfg).Handler())
	defer srv.Close()
	rawChat := `{"model":"native-chat","messages":[{"role":"user","content":"hi"}],"unknown_field_xyz":1,"bigint":9007199254740993}`
	status, _, _ := postRev(t, srv.URL+"/v1/chat/completions", rawChat)
	if status != 200 {
		t.Fatalf("native chat status=%d", status)
	}
	if !strings.Contains(string(gotChat), "unknown_field_xyz") || !strings.Contains(string(gotChat), "9007199254740993") {
		t.Fatalf("native chat must preserve unknown/bigint: %s", gotChat)
	}
	rawResp := `{"model":"native-resp","input":"hi","unknown_field_xyz":1,"bigint":9007199254740993}`
	status, _, _ = postRev(t, srv.URL+"/v1/responses", rawResp)
	if status != 200 {
		t.Fatalf("native responses status=%d", status)
	}
	if !strings.Contains(string(gotResp), "unknown_field_xyz") || !strings.Contains(string(gotResp), "9007199254740993") {
		t.Fatalf("native responses must preserve unknown/bigint: %s", gotResp)
	}
	// Ordinary Responses->Chat translation without reasoning still works.
	h := newRevHarness(t, revBridgeCfg(t, ""))
	status, _, data := postRev(t, h.router.URL+"/v1/responses", `{"model":"bridge-resp","input":"Hi"}`)
	if status != 200 {
		t.Fatalf("ordinary translation status=%d body=%s", status, data)
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if v["status"] != "completed" {
		t.Fatalf("ordinary fixture must still translate: %v", v)
	}
	// Upstream private/invalid output stays fail-closed (502, never 200).
	badUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"bad-upstream"}`))
	}))
	defer badUp.Close()
	badCfg := revBridgeCfg(t, "")
	for _, prov := range badCfg.Providers {
		prov.BaseURL = badUp.URL
	}
	badSrv := httptest.NewServer(New(badCfg).Handler())
	defer badSrv.Close()
	status, _, badData := postRev(t, badSrv.URL+"/v1/responses", `{"model":"bridge-resp","input":"Hi"}`)
	if status != 502 {
		t.Fatalf("invalid upstream must stay fail-closed, got %d %s", status, badData)
	}
}
