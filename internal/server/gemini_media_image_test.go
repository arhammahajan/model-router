package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

const g11PNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGP4z8DwHwAFAAH/iZk9HQAAAABJRU5ErkJggg=="

// Gemini->Chat image vertical: ordered text/image interleaves to exact
// Chat image_url data URLs with no loss header.
func TestG11GeminiToChatImageOrder(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}
	body := `{"contents":[{"role":"user","parts":[
		{"text":"a"},
		{"inlineData":{"mimeType":"image/png","data":"` + g11PNG + `"}},
		{"text":"b"},
		{"inlineData":{"mimeType":"image/gif","data":"eA=="}}
	]}]}`
	status, headers, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-chat:generateContent", body, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s, want 200", status, data)
	}
	if loss := headers.Get("X-Model-Router-Loss"); loss != "" {
		t.Fatalf("image bridge must carry no loss, got %s", loss)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var wire map[string]any
	if err := json.Unmarshal(h.lastBody, &wire); err != nil {
		t.Fatal(err)
	}
	msgs, _ := wire["messages"].([]any)
	content, _ := msgs[0].(map[string]any)["content"].([]any)
	if len(content) != 4 {
		t.Fatalf("ordered content length=%d: %s", len(content), h.lastBody)
	}
	types := []string{}
	for _, c := range content {
		m, _ := c.(map[string]any)
		types = append(types, m["type"].(string))
	}
	for i, want := range []string{"text", "image_url", "text", "image_url"} {
		if types[i] != want {
			t.Fatalf("order mismatch %v: %s", types, h.lastBody)
		}
	}
	second, _ := content[1].(map[string]any)["image_url"].(map[string]any)
	if second["url"] != "data:image/png;base64,"+g11PNG {
		t.Fatalf("exact data not preserved: %v", second["url"])
	}
	if !strings.Contains(string(data), `"finishReason":"STOP"`) {
		t.Fatalf("translated terminal missing STOP: %s", data)
	}
}

// Malformed image payloads are 400 pre-I/O in both modes (strict and lossy).
func TestG11GeminiToChatImageMalformedNoIO(t *testing.T) {
	for _, tc := range []struct{ name, part string }{
		{"bad-base64", `{"mimeType":"image/png","data":"not base64!!"}`},
		{"empty-data", `{"mimeType":"image/png","data":""}`},
		{"bad-mime", `{"mimeType":"not-a-mime","data":"eA=="}`},
	} {
		for _, model := range []string{"gem-chat", "gem-chat-lossy"} {
			h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
			body := `{"contents":[{"role":"user","parts":[{"text":"hi"},{"inlineData":` + tc.part + `}]}]}`
			status, _, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/"+model+":generateContent", body, nil)
			if status != 400 {
				t.Fatalf("%s %s: status=%d body=%s, want 400", tc.name, model, status, data)
			}
			if n := gemCalls(h); n != 0 {
				t.Fatalf("%s %s: upstream calls=%d, want 0", tc.name, model, n)
			}
		}
	}
	// Unknown role is 400 pre-I/O.
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	status, _, _ := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-chat:generateContent",
		`{"contents":[{"role":"assistant","parts":[{"text":"hi"},{"inlineData":{"mimeType":"image/png","data":"eA=="}}]}]}`, nil)
	if status != 400 {
		t.Fatalf("unknown role status=%d, want 400", status)
	}
	if n := gemCalls(h); n != 0 {
		t.Fatalf("unknown role upstream calls=%d, want 0", n)
	}
}

// Audio to Anthropic refuses in strict mode. Video to Chat loses only the
// unsupported part in lossy mode.
func TestG11GeminiUnsupportedMedia(t *testing.T) {
	audio := `{"contents":[{"role":"user","parts":[{"text":"hi"},{"inlineData":{"mimeType":"audio/wav","data":"UklGRg=="}}]}]}`
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1beta/models/gem-x:generateContent", audio, nil)
	if status != 400 {
		t.Fatalf("strict status=%d body=%s, want 400", status, data)
	}
	if !strings.Contains(string(data), "TRANSLATION_LOSS") {
		t.Fatalf("strict body lacks TRANSLATION_LOSS: %s", data)
	}
	if n := gemCalls(h); n != 0 {
		t.Fatalf("strict upstream calls=%d, want 0", n)
	}
	h2 := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	h2.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}
	status, headers, _ := postGeminiBridge(t, h2.router.URL+"/v1beta/models/gem-chat-lossy:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"hi"},{"inlineData":{"mimeType":"video/mp4","data":"eA=="}}]}]}`, nil)
	if status != 200 {
		t.Fatalf("lossy video status=%d, want 200", status)
	}
	if headers.Get("X-Model-Router-Loss") == "" {
		t.Fatalf("lossy video must carry a loss header")
	}
	h2.mu.Lock()
	defer h2.mu.Unlock()
	if strings.Contains(string(h2.lastBody), "image_url") {
		t.Fatalf("unlisted media must never reach Chat wire: %s", h2.lastBody)
	}
}
