package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/arhammahajan/router/internal/capability"
)

// Bounded Anthropic thinking enabled HTTP wire: exact llama budget,
// strict-loss policy before I/O, Responses refusal, Gemini trusted-fact
// gating. Gemini admits the well-formed intent and emits the exact
// generationConfig.thinkingConfig.thinkingBudget only on the pinned
// canonical native fact; otherwise a diagnosed drop (strict 400 pre-I/O,
// lossy 200 with header). No live creds/cache.

func TestAnthEnabledLlamaExactWire(t *testing.T) {
	// Confirmed llama_cpp supports native Anthropic (/v1/messages), so the
	// Anthropic thinking budget passes through untouched with no loss header.
	up, rsrv, _ := llamaVerticalServer(t, capability.RuntimeLlamaCpp, "")
	status, hdr, _ := anthDisabledPost(t, rsrv.URL+"/v1/messages",
		`{"model":"llama-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"enabled","budget_tokens":2048}}`)
	if status != 200 {
		t.Fatalf("llama native anthropic must 200, got %d", status)
	}
	if hdr.Get("X-Model-Router-Loss") != "" {
		t.Fatalf("native anthropic must emit no loss: %q", hdr.Get("X-Model-Router-Loss"))
	}
	got := up.last(t)
	if _, has := got["reasoning_effort"]; has {
		t.Fatalf("native anthropic must not emit effort: %v", got)
	}
	thinking, ok := got["thinking"].(map[string]any)
	if !ok || thinking["type"] != "enabled" {
		t.Fatalf("llama native anthropic must preserve thinking: %v", got)
	}
	// Exact numeric check via raw body.
	up.mu.Lock()
	body := string(up.bodies[len(up.bodies)-1])
	up.mu.Unlock()
	if !strings.Contains(body, `"budget_tokens":2048`) {
		t.Fatalf("wire must preserve exact budget_tokens 2048: %s", body)
	}
}

func TestAnthEnabledOrdinaryStrictLossyNoIO(t *testing.T) {
	h := newAnthDisabledHarness(t, anthDisabledCfg(t, ""), anthDisabledChatResp, anthDisabledRespResp)
	// Strict ordinary Chat has no budget control: diagnosed drop, 400, no I/O.
	status, _, data := anthDisabledPost(t, h.router.URL+"/v1/messages",
		`{"model":"a-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"enabled","budget_tokens":1024}}`)
	if status != 400 || !strings.Contains(string(data), "thinking") {
		t.Fatalf("strict ordinary must 400 thinking, got %d %s", status, data)
	}
	if got := anthDisabledCalls(h); got != 0 {
		t.Fatalf("strict refusal must precede I/O, got %d", got)
	}
	// Lossy drops with header, emits no reasoning, still 200.
	status, hdr, _ := anthDisabledPost(t, h.router.URL+"/v1/messages",
		`{"model":"a-chat-lossy","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"enabled","budget_tokens":1024}}`)
	if status != 200 || hdr.Get("X-Model-Router-Loss") == "" {
		t.Fatalf("lossy must drop with header, got %d %q", status, hdr.Get("X-Model-Router-Loss"))
	}
	if !strings.Contains(hdr.Get("X-Model-Router-Loss"), "thinking") {
		t.Fatalf("loss must name thinking: %q", hdr.Get("X-Model-Router-Loss"))
	}
	wire := anthDisabledWire(t, h)
	if _, has := wire["reasoning_effort"]; has {
		t.Fatalf("dropped must not emit effort: %v", wire)
	}
	if _, has := wire["thinking_budget_tokens"]; has {
		t.Fatalf("ordinary must not emit native budget: %v", wire)
	}
}

func TestAnthEnabledDisplayLlamaStrictLossy(t *testing.T) {
	// Confirmed llama_cpp supports native Anthropic (/v1/messages), so
	// thinking.display passes through natively untouched in both strict and
	// lossy modes with no loss header.
	for _, tc := range []struct {
		name    string
		compat  string
		display string
	}{
		{"strict", "", "summarized"},
		{"lossy", "compatibility = \"lossy\"\n", "omitted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up, rsrv, _ := llamaVerticalServer(t, capability.RuntimeLlamaCpp, tc.compat)
			status, hdr, data := anthDisabledPost(t, rsrv.URL+"/v1/messages",
				`{"model":"llama-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"enabled","budget_tokens":512,"display":"`+tc.display+`"}}`)
			if status != 200 {
				t.Fatalf("native anthropic (%s) must 200, got %d %s", tc.name, status, data)
			}
			if hdr.Get("X-Model-Router-Loss") != "" {
				t.Fatalf("native anthropic (%s) must emit no loss header, got %q", tc.name, hdr.Get("X-Model-Router-Loss"))
			}
			got := up.last(t)
			thinking, ok := got["thinking"].(map[string]any)
			if !ok || thinking["display"] != tc.display {
				t.Fatalf("native anthropic (%s) must preserve thinking.display=%q: %v", tc.name, tc.display, got)
			}
		})
	}
}

func TestAnthEnabledResponsesGeminiRefusalNoIO(t *testing.T) {
	h := newAnthDisabledHarness(t, anthDisabledCfg(t, ""), anthDisabledChatResp, anthDisabledRespResp)
	// Responses has no budget control: hard same-protocol refusal, no I/O.
	status, _, data := anthDisabledPost(t, h.router.URL+"/v1/messages",
		`{"model":"a-resp","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"enabled","budget_tokens":1024}}`)
	if status != 400 {
		t.Fatalf("responses enabled must 400, got %d %s", status, data)
	}
	if got := anthDisabledCalls(h); got != 0 {
		t.Fatalf("responses refusal must precede I/O, got %d", got)
	}
	// Gemini without the trusted native fact: diagnosed runtime drop.
	// Strict 400 pre-I/O, lossy 200 with header and no budget wire.
	gh := newA2GHarness(t, a2gBridgeCfg(t))
	post := func(url, body string) (int, []byte) {
		s, _, d := postA2G(t, url, body, nil)
		return s, d
	}
	status, _ = post(gh.router.URL+"/v1/messages",
		`{"model":"anth-gem","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"enabled","budget_tokens":1024}}`)
	if status != 400 {
		t.Fatalf("gemini enabled without trusted fact must 400, got %d", status)
	}
	if got := a2gCalls(gh); got != 0 {
		t.Fatalf("gemini drop must precede I/O, got %d", got)
	}
	// Lossy drops with header, still 200.
	gh.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP","index":0}],"modelVersion":"gemini-2.0-flash","responseId":"r1","usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`))
	}
	status, hdr, _ := postA2G(t, gh.router.URL+"/v1/messages",
		`{"model":"anth-gem-lossy","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"enabled","budget_tokens":1024}}`, nil)
	if status != 200 || hdr.Get("X-Model-Router-Loss") == "" {
		t.Fatalf("gemini lossy must drop with header, got %d %q", status, hdr.Get("X-Model-Router-Loss"))
	}
	if !strings.Contains(hdr.Get("X-Model-Router-Loss"), "thinking") {
		t.Fatalf("loss must name thinking: %q", hdr.Get("X-Model-Router-Loss"))
	}
}

func TestAnthEnabledMalformedNullNativeWire(t *testing.T) {
	h := newAnthDisabledHarness(t, anthDisabledCfg(t, ""), anthDisabledChatResp, anthDisabledRespResp)
	for _, raw := range []string{
		`{"model":"a-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"enabled"}}`,
		`{"model":"a-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"enabled","budget_tokens":0}}`,
		`{"model":"a-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"enabled","budget_tokens":-3}}`,
		`{"model":"a-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"enabled","budget_tokens":"1024"}}`,
		`{"model":"a-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"enabled","budget_tokens":8,"display":"verbose"}}`,
		`{"model":"a-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"adaptive"}}`,
		`{"model":"a-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"between_tools"}}`,
	} {
		status, _, _ := anthDisabledPost(t, h.router.URL+"/v1/messages", raw)
		if status != 400 {
			t.Fatalf("malformed must 400: %s got %d", raw, status)
		}
	}
	if got := anthDisabledCalls(h); got != 0 {
		t.Fatalf("malformed must precede I/O, got %d", got)
	}
	// Null means absent: no reasoning, no loss.
	status, hdr, _ := anthDisabledPost(t, h.router.URL+"/v1/messages",
		`{"model":"a-chat","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":null}`)
	if status != 200 || hdr.Get("X-Model-Router-Loss") != "" {
		t.Fatalf("null must be absent no-loss: %d %q", status, hdr.Get("X-Model-Router-Loss"))
	}
	// Native same-protocol bypass stays raw (byte/numeric fidelity).
	status, hdr, _ = anthDisabledPost(t, h.router.URL+"/v1/messages",
		`{"model":"a-native","max_tokens":16,"messages":[{"role":"user","content":"Hi"}],"thinking":{"type":"enabled","budget_tokens":1024}}`)
	if status != 200 {
		t.Fatalf("native must bypass: %d", status)
	}
	if hdr.Get("X-Model-Router-Loss") != "" {
		t.Fatalf("native must not set loss: %q", hdr.Get("X-Model-Router-Loss"))
	}
}
