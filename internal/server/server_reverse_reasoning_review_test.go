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

// Independent reverse review: Responses->Chat named effort over HTTP.
// Stable source: docs/go-request-reasoning-contract.md sec 9 + forward unit.
// Hermetic: TempDir config, httptest upstream/router, auth none, no env/creds.
func rvrBridgeCfg(t *testing.T, extra string) *config.Config {
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

type rvrHarness struct {
	upstream *httptest.Server
	router   *httptest.Server
	mu       sync.Mutex
	lastBody []byte
	calls    int
}

func rvrNewHarness(t *testing.T, cfg *config.Config) *rvrHarness {
	t.Helper()
	h := &rvrHarness{}
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

func rvrPost(t *testing.T, url, body string) (int, http.Header, []byte) {
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

func rvrUpstream(t *testing.T, h *rvrHarness) map[string]any {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	var v map[string]any
	if err := json.Unmarshal(h.lastBody, &v); err != nil {
		t.Fatalf("upstream not JSON: %v %s", err, h.lastBody)
	}
	return v
}

func rvrCalls(h *rvrHarness) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

func TestRvrSrvExactNoHeader(t *testing.T) {
	h := rvrNewHarness(t, rvrBridgeCfg(t, ""))
	status, hdr, _ := rvrPost(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"Hi","reasoning":{"effort":"medium"}}`)
	if status != 200 || hdr.Get("X-Model-Router-Loss") != "" {
		t.Fatalf("exact must be 200 no header: %d %q", status, hdr.Get("X-Model-Router-Loss"))
	}
	up := rvrUpstream(t, h)
	if up["reasoning_effort"] != "medium" {
		t.Fatalf("wire literal: %v", up)
	}
	if _, has := up["reasoning"]; has {
		t.Fatalf("must not emit Responses field: %v", up)
	}
}

func TestRvrSrvStrictLossyGatesValueFree(t *testing.T) {
	cfg := rvrBridgeCfg(t, `
[capabilities.overrides.bridge-resp.reasoning]
supported = false
[capabilities.overrides.bridge-resp-lossy.reasoning]
supported = false
`)
	h := rvrNewHarness(t, cfg)
	status, _, data := rvrPost(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-resp","input":"Hi","reasoning":{"effort":"medium"}}`)
	if status != 400 || !strings.Contains(string(data), "translation_loss") {
		t.Fatalf("strict must 400 pre-IO: %d %s", status, data)
	}
	if got := rvrCalls(h); got != 0 {
		t.Fatalf("strict must not touch upstream: %d", got)
	}
	status, hdr, _ := rvrPost(t, h.router.URL+"/v1/responses",
		`{"model":"bridge-resp-lossy","input":"Hi","reasoning":{"effort":"medium"}}`)
	if status != 200 || hdr.Get("X-Model-Router-Loss") == "" {
		t.Fatalf("lossy must drop with header: %d", status)
	}
	var loss []map[string]any
	if err := json.Unmarshal([]byte(hdr.Get("X-Model-Router-Loss")), &loss); err != nil {
		t.Fatal(err)
	}
	if len(loss) != 1 || loss[0]["field"] != "reasoning.effort" || loss[0]["action"] != "dropped" {
		t.Fatalf("loss must name closed source: %v", loss)
	}
	for _, v := range loss {
		if s, _ := json.Marshal(v); strings.Contains(string(s), "medium") {
			t.Fatalf("diagnostic must be value-free: %v", v)
		}
	}
	up := rvrUpstream(t, h)
	if _, has := up["reasoning_effort"]; has {
		t.Fatalf("dropped must not emit: %v", up)
	}
}

func TestRvrSrvMalformedPreIOBothModes(t *testing.T) {
	for _, model := range []string{"bridge-resp", "bridge-resp-lossy"} {
		for _, frag := range []string{
			`"reasoning":{"effort":123}`,
			`"reasoning":{"effort":null}`,
			`"reasoning":{"effort":""}`,
			`"reasoning":{"effort":"medium","bogus":1}`,
			`"reasoning":{"summary":null}`,
		} {
			h := rvrNewHarness(t, rvrBridgeCfg(t, ""))
			before := rvrCalls(h)
			status, _, data := rvrPost(t, h.router.URL+"/v1/responses",
				`{"model":"`+model+`","input":"Hi",`+frag+`}`)
			if status != 400 || !strings.Contains(string(data), "invalid_generation_parameter") {
				t.Fatalf("malformed %s %s must 400 generation code, got %d %s", model, frag, status, data)
			}
			if got := rvrCalls(h); got != before {
				t.Fatalf("malformed must not touch upstream: %s", frag)
			}
		}
	}
}

func TestRvrSrvSummaryRefusalBothModes(t *testing.T) {
	for _, model := range []string{"bridge-resp", "bridge-resp-lossy"} {
		for _, frag := range []string{
			`"reasoning":{"summary":"detailed"}`,
			`"reasoning":{"effort":"medium","summary":"detailed"}`,
			`"reasoning":{"generate_summary":"auto"}`,
			`"reasoning":{"effort":"medium","generate_summary":"auto"}`,
		} {
			h := rvrNewHarness(t, rvrBridgeCfg(t, ""))
			before := rvrCalls(h)
			status, _, _ := rvrPost(t, h.router.URL+"/v1/responses",
				`{"model":"`+model+`","input":"Hi",`+frag+`}`)
			if status != 400 {
				t.Fatalf("summary %s %s must refuse, got %d", model, frag, status)
			}
			if got := rvrCalls(h); got != before {
				t.Fatalf("summary refusal must not touch upstream")
			}
		}
	}
}

func TestRvrSrvNullEmptyNoControl(t *testing.T) {
	h := rvrNewHarness(t, rvrBridgeCfg(t, ""))
	for _, body := range []string{
		`{"model":"bridge-resp","input":"Hi","reasoning":null}`,
		`{"model":"bridge-resp","input":"Hi"}`,
		`{"model":"bridge-resp","input":"Hi","reasoning":{}}`,
	} {
		status, hdr, _ := rvrPost(t, h.router.URL+"/v1/responses", body)
		if status != 200 || hdr.Get("X-Model-Router-Loss") != "" {
			t.Fatalf("null/empty must be absent: %s -> %d %q", body, status, hdr.Get("X-Model-Router-Loss"))
		}
	}
	if up := rvrUpstream(t, h); func() bool {
		_, has := up["reasoning_effort"]
		return has
	}() {
		t.Fatalf("absent must not emit: %v", up)
	}
}

func TestRvrSrvLlamaRefusalIncludingNone(t *testing.T) {
	for _, frag := range []string{
		`"reasoning":{"effort":"medium"}`, `"reasoning":{"effort":"none"}`,
	} {
		var mu sync.Mutex
		upHit := 0
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
		body := "[server]\nauth_mode = \"none\"\nallow_dynamic_routes = false\nmax_generations = 8\n[retry]\nmax_attempts = 1\n[providers.llama_cpp]\n[[providers.llama_cpp.servers]]\nname = \"llama-test\"\nurl = \"" + up.URL + "\"\n"
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
		srv := NewWithRegistry(cfg, reg)
		rsrv := httptest.NewServer(srv.Handler())
		defer rsrv.Close()
		status, _, _ := rvrPost(t, rsrv.URL+"/v1/responses", `{"model":"llama-chat","input":"Hi",`+frag+`}`)
		if status != 400 {
			t.Fatalf("llama %s must refuse, got %d", frag, status)
		}
		mu.Lock()
		got := upHit
		mu.Unlock()
		if got != 0 {
			t.Fatalf("llama refusal must not touch upstream: %s", frag)
		}
	}
}
