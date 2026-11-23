package server

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/arhammahajan/router/internal/config"
)

// TestGeminiBridgeTranslatedJSONBodyBound guards the post-translation
// MaxBodyBytes check on both JSON bridge directions. The raw upstream body
// fits the bound but JSON escaping of '<' (to \u003c) expands the translated
// downstream past it, so the bridge must fail closed with a sanitized
// ingress-native 502 before committing any downstream headers or bytes.
func TestGeminiBridgeTranslatedJSONBodyBound(t *testing.T) {
	for _, tc := range []struct {
		name, path, request, upstream string
		wantCode                      string
		makeConfig                    func(*testing.T) *config.Config
	}{
		{"chat", "/v1/chat/completions", `{"model":"bridge-gem","messages":[{"role":"user","content":"hi"}]}`,
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"%s"}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3,"totalTokenCount":10}}`,
			`"code":"upstream_response_invalid"`, geminiBridgeCfg},
		{"gemini", "/v1beta/models/gem-chat:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			`{"id":"c","object":"chat.completion","created":1,"model":"up","choices":[{"index":0,"message":{"role":"assistant","content":"%s"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`,
			`UPSTREAM_RESPONSE_INVALID`, geminiBridgeCfg},
		{"anthropic_to_gemini", "/v1/messages", `{"model":"anth-gem","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"%s"}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3,"totalTokenCount":10}}`,
			`"type":"api_error"`, a2gBridgeCfg},
		{"gemini_to_anthropic", "/v1beta/models/gem-anth:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			`{"id":"m","type":"message","role":"assistant","model":"up","content":[{"type":"text","text":"%s"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":7,"output_tokens":3}}`,
			`UPSTREAM_RESPONSE_INVALID`, g2aBridgeCfg},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.makeConfig(t)
			cfg.Server.MaxBodyBytes = 512
			h := newGeminiBridgeHarness(t, cfg)
			source := strings.Repeat("<", 80)
			upstream := fmt.Sprintf(tc.upstream, source)
			if len(upstream) > 512 {
				t.Fatal("fixture must fit the upstream body bound")
			}
			h.handler = func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, upstream)
			}
			status, headers, body := postGeminiBridge(t, h.router.URL+tc.path, tc.request, nil)
			if status != 502 {
				t.Fatalf("expanded response status = %d, want 502 (body bytes %d)", status, len(body))
			}
			text := string(body)
			// Sanitized ingress-native envelope: stable bound message and
			// router code, never the raw upstream payload.
			if !strings.Contains(text, "Upstream response exceeds the size bound") {
				t.Fatalf("error body missing sanitized bound message: %s", text)
			}
			if !strings.Contains(text, tc.wantCode) {
				t.Fatalf("error body missing router code %q: %s", tc.wantCode, text)
			}
			if strings.Contains(text, source) || strings.Contains(text, "<") || strings.Contains(text, `\u003c`) {
				t.Fatalf("error body leaks source bytes: %s", text)
			}
			if ct := headers.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Fatalf("Content-Type = %q, want application/json", ct)
			}
			// Pre-commit failure: no loss header or translated payload is
			// committed alongside the error.
			if loss := headers.Get("X-Model-Router-Loss"); loss != "" {
				t.Fatalf("loss header = %q, want absent on bound rejection", loss)
			}
		})
	}
}
