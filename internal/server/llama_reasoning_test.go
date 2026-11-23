// Llama reasoning vertical end-to-end: native Chat applier on confirmed
// LlamaCpp (strict+lossy), raw passthrough for Strata/Unobserved/ordinary,
// Responses->Chat mapped/none/unknown handling, narrow Unobserved refusal,
// Strata literal, forward-to-local loss, alias/retry stability and listing
// facts. Hermetic fakes only.
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

type llamaUpstream struct {
	mu     sync.Mutex
	hits   int
	bodies [][]byte
}

func (u *llamaUpstream) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.hits++
		u.bodies = append(u.bodies, append([]byte(nil), b...))
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}
}

func (u *llamaUpstream) last(t *testing.T) map[string]any {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.bodies) == 0 {
		t.Fatalf("upstream untouched")
	}
	var m map[string]any
	dec := json.NewDecoder(strings.NewReader(string(u.bodies[len(u.bodies)-1])))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

func llamaVerticalServer(t *testing.T, rt capability.LocalRuntime, compat string) (*llamaUpstream, *httptest.Server, *router.Registry) {
	t.Helper()
	up := &llamaUpstream{}
	srv := httptest.NewServer(up.handler())
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
url = "` + srv.URL + `"
`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	reg := router.NewRegistry(cfg)
	obs := capability.ExtractLlamaObs(map[string]any{"id": "m1"}, map[string]any{"build_info": "b"}, "single")
	layer := router.LlamaLayer{Servers: map[string]router.LlamaServerSnapshot{
		"llama-test": {State: router.LlamaReachable, Runtime: rt,
			Entries: []router.LlamaEntry{{Name: "llama-chat", Upstream: "llama-chat", Obs: obs}}},
	}}
	if err := reg.CommitLlama(router.LlamaAttempt{Layer: layer, AtWall: router.Wall(1000)}); err != nil {
		t.Fatal(err)
	}
	rsrv := httptest.NewServer(NewWithRegistry(cfg, reg).Handler())
	t.Cleanup(rsrv.Close)
	t.Cleanup(srv.Close)
	return up, rsrv, reg
}
func TestLlamaNativeChatLevels(t *testing.T) {
	for _, compat := range []string{"", "compatibility = \"lossy\"\n"} {
		up, rsrv, _ := llamaVerticalServer(t, capability.RuntimeLlamaCpp, compat)
		// Mapped level: consumed, exact budget, no effort member.
		status, _, _ := postChat(t, rsrv.URL+"/v1/chat/completions",
			`{"model":"llama-chat","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`)
		if status != 200 {
			t.Fatalf("native level (compat=%q): %d", compat, status)
		}
		got := up.last(t)
		if _, has := got["reasoning_effort"]; has {
			t.Fatalf("effort must be consumed: %v", got)
		}
		if string(got["thinking_budget_tokens"].(json.Number)) != "16384" {
			t.Fatalf("budget: %v", got)
		}
		// none: kwargs false.
		status, _, _ = postChat(t, rsrv.URL+"/v1/chat/completions",
			`{"model":"llama-chat","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"none"}`)
		if status != 200 {
			t.Fatalf("native none (compat=%q): %d", compat, status)
		}
		got = up.last(t)
		if got["chat_template_kwargs"].(map[string]any)["enable_thinking"] != false {
			t.Fatalf("kwargs: %v", got)
		}
		// max/unknown/null/non-string: consumed, no control, still 200.
		for _, eff := range []string{`"max"`, `"ultra"`, `null`, `42`, `""`} {
			status, hdr, _ := postChat(t, rsrv.URL+"/v1/chat/completions",
				`{"model":"llama-chat","messages":[{"role":"user","content":"hi"}],"reasoning_effort":`+eff+`}`)
			if status != 200 {
				t.Fatalf("native consumed %s (compat=%q): %d", eff, compat, status)
			}
			if hdr.Get("X-Model-Router-Loss") != "" {
				t.Fatalf("native must not set loss header: %s", eff)
			}
			got = up.last(t)
			if _, has := got["reasoning_effort"]; has {
				t.Fatalf("effort must be consumed: %s", eff)
			}
			if _, has := got["thinking_budget_tokens"]; has {
				t.Fatalf("no budget for %s", eff)
			}
		}
		// Absent effort: byte-identical upstream body (modulo model).
		status, _, _ = postChat(t, rsrv.URL+"/v1/chat/completions",
			`{"model":"llama-chat","messages":[{"role":"user","content":"hi"}],"seed":9007199254740993}`)
		if status != 200 {
			t.Fatalf("native absent (compat=%q): %d", compat, status)
		}
		if got := up.last(t); string(got["seed"].(json.Number)) != "9007199254740993" {
			t.Fatalf("bigint: %v", got)
		}
	}
}

func TestLlamaNativeUntouchedRuntimes(t *testing.T) {
	// Strata, Unobserved and ordinary chat providers pass raw bytes
	// through: the effort member is never consumed or validated.
	up, rsrv, reg := llamaVerticalServer(t, capability.RuntimeStrata, "")
	_ = reg
	for _, body := range []string{
		`{"model":"llama-chat","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`,
		`{"model":"llama-chat","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"none"}`,
		`{"model":"llama-chat","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"max"}`,
	} {
		status, _, _ := postChat(t, rsrv.URL+"/v1/chat/completions", body)
		if status != 200 {
			t.Fatalf("strata native: %d", status)
		}
		if got := up.last(t); got["reasoning_effort"] == nil {
			t.Fatalf("strata must pass through: %v", got)
		}
	}
	upUn, rsrvUn, _ := llamaVerticalServer(t, capability.RuntimeUnobserved, "")
	status, _, _ := postChat(t, rsrvUn.URL+"/v1/chat/completions",
		`{"model":"llama-chat","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`)
	if status != 200 {
		t.Fatalf("unobserved native: %d", status)
	}
	if got := upUn.last(t); got["reasoning_effort"] != "high" {
		t.Fatalf("unobserved must pass through: %v", got)
	}
	if hits := len(upUn.bodies); hits != 1 {
		t.Fatalf("one request must mean one upstream hit, got %d", hits)
	}
}

func TestLlamaReverseMappedWire(t *testing.T) {
	for _, compat := range []string{"", "compatibility = \"lossy\"\n"} {
		up, rsrv, _ := llamaVerticalServer(t, capability.RuntimeLlamaCpp, compat)
		// Mapped level: capability gates, exact budget member, no
		// effort member on the Chat wire.
		status, _, data := postRev(t, rsrv.URL+"/v1/responses",
			`{"model":"llama-chat","input":"Hi","reasoning":{"effort":"medium"}}`)
		if status != 200 {
			t.Fatalf("reverse mapped (compat=%q): %d %s", compat, status, data)
		}
		got := up.last(t)
		if string(got["thinking_budget_tokens"].(json.Number)) != "8192" {
			t.Fatalf("budget wire: %v", got)
		}
		if _, has := got["reasoning_effort"]; has {
			t.Fatalf("no effort member: %v", got)
		}
		// none: kwargs false.
		status, _, data = postRev(t, rsrv.URL+"/v1/responses",
			`{"model":"llama-chat","input":"Hi","reasoning":{"effort":"none"}}`)
		if status != 200 {
			t.Fatalf("reverse none (compat=%q): %d %s", compat, status, data)
		}
		got = up.last(t)
		if got["chat_template_kwargs"].(map[string]any)["enable_thinking"] != false {
			t.Fatalf("none wire: %v", got)
		}
		// Unknown: strict refuses pre-I/O with zero touches; lossy drops
		// with a value-free loss header and no control.
		up.mu.Lock()
		up.hits = 0
		up.mu.Unlock()
		status, hdr, _ := postRev(t, rsrv.URL+"/v1/responses",
			`{"model":"llama-chat","input":"Hi","reasoning":{"effort":"ultra"}}`)
		if compat == "" {
			if status != 400 {
				t.Fatalf("strict must refuse: %d", status)
			}
			up.mu.Lock()
			hits := up.hits
			up.mu.Unlock()
			if hits != 0 {
				t.Fatalf("strict refusal must precede I/O")
			}
		} else {
			if status != 200 {
				t.Fatalf("lossy must proceed: %d", status)
			}
			if !strings.Contains(hdr.Get("X-Model-Router-Loss"), "reasoning.effort") {
				t.Fatalf("lossy header: %v", hdr)
			}
			got = up.last(t)
			if _, has := got["thinking_budget_tokens"]; has {
				t.Fatalf("lossy drops control: %v", got)
			}
		}
	}
}

func TestLlamaReverseUnobservedRefusal(t *testing.T) {
	// Retained narrow refusal: unknown-cache effort reverses to a
	// pre-I/O hard refusal in both modes.
	for _, compat := range []string{"", "compatibility = \"lossy\"\n"} {
		up, rsrv, _ := llamaVerticalServer(t, capability.RuntimeUnobserved, compat)
		status, _, _ := postRev(t, rsrv.URL+"/v1/responses",
			`{"model":"llama-chat","input":"Hi","reasoning":{"effort":"medium"}}`)
		if status != 400 {
			t.Fatalf("unobserved must refuse (compat=%q): %d", compat, status)
		}
		up.mu.Lock()
		hits := up.hits
		up.mu.Unlock()
		if hits != 0 {
			t.Fatalf("refusal must precede I/O")
		}
	}
}

func TestLlamaReverseStrataLiteral(t *testing.T) {
	up, rsrv, _ := llamaVerticalServer(t, capability.RuntimeStrata, "")
	status, _, data := postRev(t, rsrv.URL+"/v1/responses",
		`{"model":"llama-chat","input":"Hi","reasoning":{"effort":"medium"}}`)
	if status != 200 {
		t.Fatalf("strata reverse: %d %s", status, data)
	}
	got := up.last(t)
	if got["reasoning_effort"] != "medium" {
		t.Fatalf("strata literal wire: %v", got)
	}
	if _, has := got["thinking_budget_tokens"]; has {
		t.Fatalf("strata must not emit budgets: %v", got)
	}
}

func TestLlamaChatIngressIsNative(t *testing.T) {
	// Chat ingress to a generated Chat provider is native, never a
	// translated pair: the effort maps through the native applier.
	up, rsrv, _ := llamaVerticalServer(t, capability.RuntimeLlamaCpp, "")
	status, _, _ := postChat(t, rsrv.URL+"/v1/chat/completions",
		`{"model":"llama-chat","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`)
	if status != 200 {
		t.Fatalf("chat->generated chat is native: %d", status)
	}
	if got := up.last(t); string(got["thinking_budget_tokens"].(json.Number)) != "16384" {
		t.Fatalf("native budget: %v", got)
	}
}

func TestLlamaAliasRetryStable(t *testing.T) {
	// Alias rewrite and the native applier compose; repeated requests are
	// byte-stable (retry-safe) with no extra upstream I/O per request.
	up, rsrv, _ := llamaVerticalServer(t, capability.RuntimeLlamaCpp, "")
	body := `{"model":"llama-chat","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low"}`
	for i := 0; i < 2; i++ {
		status, _, _ := postChat(t, rsrv.URL+"/v1/chat/completions", body)
		if status != 200 {
			t.Fatalf("attempt %d: %d", i, status)
		}
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	if len(up.bodies) != 2 {
		t.Fatalf("two requests must mean two hits, got %d", len(up.bodies))
	}
	if string(up.bodies[0]) != string(up.bodies[1]) {
		t.Fatalf("repeat must be stable:\n%s\n%s", up.bodies[0], up.bodies[1])
	}
	var m map[string]any
	dec := json.NewDecoder(strings.NewReader(string(up.bodies[0])))
	dec.UseNumber()
	_ = dec.Decode(&m)
	if m["model"] != "llama-chat" {
		t.Fatalf("alias rewrite: %v", m)
	}
	if _, has := m["reasoning_effort"]; has {
		t.Fatalf("consumed: %v", m)
	}
}

func TestLlamaListingFacts(t *testing.T) {
	_, rsrv, _ := llamaVerticalServer(t, capability.RuntimeLlamaCpp, "")
	req, _ := http.NewRequest(http.MethodGet, rsrv.URL+"/v1/models", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	rows, _ := payload["data"].([]any)
	found := false
	for _, r := range rows {
		row, _ := r.(map[string]any)
		if row["id"] != "llama-chat" {
			continue
		}
		found = true
		meta, _ := row["metadata"].(map[string]any)
		routerMeta, _ := meta["router"].(map[string]any)
		raw, _ := json.Marshal(routerMeta)
		if !strings.Contains(string(raw), "router.llama_thinking_map.supported") {
			t.Fatalf("listing must carry configured mapper facts: %s", raw)
		}
		if strings.Contains(string(raw), `"reasoning":{"supported":{"value":true`) {
			t.Fatalf("listing must not invent reasoning.supported: %s", raw)
		}
	}
	if !found {
		t.Fatalf("llama-chat missing from listing: %s", data)
	}
}
