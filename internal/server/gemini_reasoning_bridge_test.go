package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Gemini thinking ingress -> Chat/Responses upstreams through the shared
// planner. Hermetic httptest harness only; no env, no network beyond
// loopback, no Python tree. Static routes carry unknown (neutral)
// capability facts, so named levels and Off are exact while explicit
// budgets drop outside a confirmed LlamaCpp runtime.

const gemReasonChatOK = `{"id":"chatcmpl-up","object":"chat.completion","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`

func gemReasonWire(t *testing.T, h *geminiBridgeHarness) map[string]any {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	var wire map[string]any
	if err := json.Unmarshal(h.lastBody, &wire); err != nil {
		t.Fatal(err)
	}
	return wire
}

func gemReasonCalls(h *geminiBridgeHarness) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

func TestGeminiReasonLevelToChatExact(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(gemReasonChatOK))
	}
	body := `{"contents":[{"role":"user","parts":[{"text":"Hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingLevel":"LOW"}}}`
	// Unknown caps are neutral: the literal is exact with no loss header.
	status, headers, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-chat:generateContent", body, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if headers.Get("X-Model-Router-Loss") != "" {
		t.Fatalf("exact must carry no loss header")
	}
	wire := gemReasonWire(t, h)
	if wire["reasoning_effort"] != "low" {
		t.Fatalf("wire must carry literal low: %v", wire)
	}
	if n := gemReasonCalls(h); n != 1 {
		t.Fatalf("one upstream call, got %d", n)
	}
	if h.lastPath != "/chat/completions" {
		t.Fatalf("upstream path=%s", h.lastPath)
	}
}

func TestGeminiReasonBudgetToChatStrictLossy(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(gemReasonChatOK))
	}
	body := `{"contents":[{"role":"user","parts":[{"text":"Hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingBudget":100}}}`
	// Strict: diagnosed loss refuses pre-I/O with zero upstream IO.
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-chat:generateContent", body, nil)
	if status != 400 {
		t.Fatalf("strict status=%d body=%s", status, data)
	}
	if !strings.Contains(string(data), "TRANSLATION_LOSS") {
		t.Fatalf("strict must be TRANSLATION_LOSS: %s", data)
	}
	if n := gemReasonCalls(h); n != 0 {
		t.Fatalf("strict must do zero upstream IO, got %d", n)
	}
	// Lossy: drops the control, forwards without it, reports the header.
	status, headers, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-chat-lossy:generateContent", body, nil)
	if status != 200 {
		t.Fatalf("lossy status=%d body=%s", status, data)
	}
	if headers.Get("X-Model-Router-Loss") == "" {
		t.Fatalf("lossy must carry the loss header")
	}
	wire := gemReasonWire(t, h)
	if _, has := wire["thinking_budget_tokens"]; has {
		t.Fatalf("ordinary Chat must drop explicit budgets: %v", wire)
	}
	if _, has := wire["reasoning_effort"]; has {
		t.Fatalf("dropped budget must emit no effort member: %v", wire)
	}
}

func TestGeminiReasonOffToChatExact(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(gemReasonChatOK))
	}
	// Budget 0 disables: unknown caps stay neutral, so strict is exact.
	status, headers, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-chat:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingBudget":0}}}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if headers.Get("X-Model-Router-Loss") != "" {
		t.Fatalf("Off exact must carry no loss header")
	}
	if wire := gemReasonWire(t, h); wire["reasoning_effort"] != "none" {
		t.Fatalf("Off must emit none on ordinary Chat: %v", wire)
	}
}

func TestGeminiReasonDefaultAbsent(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(gemReasonChatOK))
	}
	for _, th := range []string{
		`"thinkingBudget":-1`,
		`"thinkingLevel":"THINKING_LEVEL_UNSPECIFIED"`,
		``,
		`"includeThoughts":false`,
	} {
		status, headers, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-chat:generateContent",
			`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}],"generationConfig":{"thinkingConfig":{`+th+`}}}`, nil)
		if status != 200 {
			t.Fatalf("%q status=%d body=%s", th, status, data)
		}
		if headers.Get("X-Model-Router-Loss") != "" {
			t.Fatalf("%q absent must carry no loss header", th)
		}
		wire := gemReasonWire(t, h)
		if _, has := wire["reasoning_effort"]; has {
			t.Fatalf("%q must emit nothing: %v", th, wire)
		}
		if _, has := wire["thinking_budget_tokens"]; has {
			t.Fatalf("%q must emit nothing: %v", th, wire)
		}
	}
}

func TestGeminiReasonMalformedBothModes(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(gemReasonChatOK))
	}
	for _, th := range []string{
		`"thinkingLevel":"XHIGH"`,
		`"thinkingBudget":100,"thinkingLevel":"LOW"`,
		`"thinkingBudget":true`,
		`"thinkingBudget":1.5`,
		`"thinkingBudget":-2`,
		`"thinkingLevel":"high"`,
		`"bogus":1`,
	} {
		for _, model := range []string{"gem-chat", "gem-chat-lossy"} {
			before := gemReasonCalls(h)
			status, _, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/"+model+":generateContent",
				`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}],"generationConfig":{"thinkingConfig":{`+th+`}}}`, nil)
			if status != 400 {
				t.Fatalf("%q %s status=%d body=%s", th, model, status, data)
			}
			if n := gemReasonCalls(h); n != before {
				t.Fatalf("%q %s must do zero upstream IO", th, model)
			}
		}
	}
}

func TestGeminiReasonToResponsesExactAndDrop(t *testing.T) {
	h := newRGBridgeHarness(t, rgBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(grResponsesJSONText))
	}
	post := func(model, thinking string) (int, http.Header, []byte, map[string]any) {
		status, headers, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/"+model+":generateContent",
			`{"contents":[{"role":"user","parts":[{"text":"Hi"}]}],"generationConfig":{"thinkingConfig":{`+thinking+`}}}`, nil)
		h.mu.Lock()
		defer h.mu.Unlock()
		var wire map[string]any
		if h.calls > 0 {
			_ = json.Unmarshal(h.lastBody, &wire)
		}
		return status, headers, data, wire
	}
	// Named level is exact on Responses with unknown caps.
	if status, headers, data, wire := post("gem-resp", `"thinkingLevel":"MEDIUM"`); status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	} else if headers.Get("X-Model-Router-Loss") != "" {
		t.Fatalf("exact must carry no loss header")
	} else if reas, ok := wire["reasoning"].(map[string]any); !ok || reas["effort"] != "medium" {
		t.Fatalf("responses wire must carry effort medium: %v", wire)
	}
	// Explicit budgets drop before the encoder (never 500 misuse).
	if status, _, data, _ := post("gem-resp", `"thinkingBudget":100`); status != 400 || !strings.Contains(string(data), "TRANSLATION_LOSS") {
		t.Fatalf("strict budget status=%d body=%s", status, data)
	}
	if status, headers, _, wire := post("gem-resp-lossy", `"thinkingBudget":100`); status != 200 {
		t.Fatalf("lossy status=%d", status)
	} else if headers.Get("X-Model-Router-Loss") == "" {
		t.Fatalf("lossy must carry the loss header")
	} else if _, has := wire["reasoning"]; has {
		t.Fatalf("dropped budget must emit no reasoning member: %v", wire)
	}
	// Off is exact none on Responses.
	if status, _, data, wire := post("gem-resp", `"thinkingBudget":0`); status != 200 {
		t.Fatalf("off status=%d body=%s", status, data)
	} else if reas, ok := wire["reasoning"].(map[string]any); !ok || reas["effort"] != "none" {
		t.Fatalf("off wire: %v", wire)
	}
	// XHIGH is malformed on both providers.
	for _, model := range []string{"gem-resp", "gem-resp-lossy"} {
		if status, _, data, _ := post(model, `"thinkingLevel":"XHIGH"`); status != 400 {
			t.Fatalf("XHIGH %s status=%d body=%s", model, status, data)
		}
	}
}
