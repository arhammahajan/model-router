package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Review of the integrated Gemini->Chat inline-image vertical (a8bf3e1),
// hermetic HTTP layer.
//
// Stable-Python parity (HEAD:model_router/gemini.py, model_turn): a model
// turn carrying malformed inlineData is a diagnosed loss, not a malformed
// request. Strict refuses with TRANSLATION_LOSS pre-I/O; lossy forwards the
// remaining conversation with a loss header and never forwards image bytes.
// The vertical's fail-closed media validation (correct for the bridged
// user->Chat path) also fires on the never-bridged model path, so lossy
// mode hard-400s instead of forwarding.
func TestReviewLossyModelBadImageForwards(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}
	body := `{"contents":[{"role":"model","parts":[{"text":"hi"},{"inlineData":{"mimeType":"image/png","data":"not base64!!"}}]}]}`
	status, headers, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-chat-lossy:generateContent", body, nil)
	if status != 200 {
		t.Fatalf("lossy model bad image: status=%d body=%s, want 200 with a loss header", status, data)
	}
	if headers.Get("X-Model-Router-Loss") == "" {
		t.Fatalf("lossy model bad image must carry a loss header")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var wire map[string]any
	if err := json.Unmarshal(h.lastBody, &wire); err != nil {
		t.Fatal(err)
	}
	msgs, _ := wire["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("upstream messages=%s, want the single surviving text turn", h.lastBody)
	}
	m0, _ := msgs[0].(map[string]any)
	if m0["role"] != "assistant" || m0["content"] != "hi" {
		t.Fatalf("model text must forward exactly: %s", h.lastBody)
	}
	if strings.Contains(string(h.lastBody), "image") {
		t.Fatalf("malformed model media must never reach the upstream wire: %s", h.lastBody)
	}
}

// Strict mode must refuse malformed model media as a diagnosed
// translation loss (pre-I/O), not as a malformed request.
func TestReviewStrictModelBadImageIsTranslationLoss(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	body := `{"contents":[{"role":"model","parts":[{"text":"hi"},{"inlineData":{"mimeType":"image/png","data":"not base64!!"}}]}]}`
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-chat:generateContent", body, nil)
	if status != 400 {
		t.Fatalf("strict model bad image: status=%d body=%s, want 400", status, data)
	}
	if !strings.Contains(string(data), "TRANSLATION_LOSS") {
		t.Fatalf("strict model bad image must be TRANSLATION_LOSS, got: %s", data)
	}
	if n := gemCalls(h); n != 0 {
		t.Fatalf("refusal must precede upstream I/O, got %d calls", n)
	}
}
