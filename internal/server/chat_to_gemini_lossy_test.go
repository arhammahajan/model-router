package server

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// The observed opencode.ai Gemini-4 response envelope: thoughtSignature
// attached to ordinary text parts, TEXT-modality usage breakdowns, and
// the usage cost/opencode gateway billing members. Strict providers keep
// the 502; lossy providers (compatibility = "lossy") validate and drop
// them so harness traffic translates. Captured from live upstream.

func lossyGemCompleteBody() string {
	return `{"responseId":"vfH9aq3cF92suMYPtvTJ-Ao","modelVersion":"gemini-4-argon","candidates":[{"finishReason":"STOP","index":0,"content":{"role":"model","parts":[{"text":"Hi!","thoughtSignature":"EowBCokBAWkUfRMZ18hkd6hRR6hsYsMtawu0lqI1f1dTa+kb3EQTcVH"}]}}],"usageMetadata":{"candidatesTokensDetails":[{"modality":"TEXT","tokenCount":6}],"promptTokensDetails":[{"modality":"TEXT","tokenCount":36}],"candidatesTokenCount":6,"promptTokenCount":36,"totalTokenCount":42,"cost":0,"opencode":{"billing_source":"go","list_cost":0.000132,"allowance":0.000264,"tokens":{"input":36,"output":6,"reasoning":0,"cache_read":0,"cache_write_5m":0,"cache_write_1h":0}}}}`
}

func TestChatToGeminiObservedEnvelopeStrictVsLossy(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(lossyGemCompleteBody()))
	}
	req := `{"model":"MODEL","messages":[{"role":"user","content":"Say hi in one word"}],"max_tokens":50}`
	// Strict provider: the part signature fails closed with the
	// value-free no-representation path.
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1/chat/completions",
		strings.Replace(req, "MODEL", "bridge-gem", 1), nil)
	if status != 502 {
		t.Fatalf("strict must fail with 502, got %d body=%s", status, data)
	}
	if !strings.Contains(string(data), "has no representation") {
		t.Fatalf("strict must name the unrepresentable feature, got %s", data)
	}
	// Lossy provider: the same envelope translates.
	status, _, data = postGeminiBridge(t, h.router.URL+"/v1/chat/completions",
		strings.Replace(req, "MODEL", "bridge-gem-lossy", 1), nil)
	if status != 200 {
		t.Fatalf("lossy must translate the observed envelope, got %d body=%s", status, data)
	}
	out := string(data)
	if !strings.Contains(out, `"content":"Hi!"`) {
		t.Fatalf("lossy must carry the text: %s", out)
	}
	if !strings.Contains(out, `"prompt_tokens":36`) || !strings.Contains(out, `"completion_tokens":6`) {
		t.Fatalf("lossy must carry authoritative usage: %s", out)
	}
	for _, leak := range []string{"thoughtSignature", "promptTokensDetails", "opencode", "\"cost\""} {
		if strings.Contains(out, leak) {
			t.Fatalf("downstream Chat body must not carry %q: %s", leak, out)
		}
	}
}

func TestChatToGeminiObservedSSEStrictVsLossy(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"index\":0,\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"Hi!\"}]}}],\"usageMetadata\":{\"candidatesTokensDetails\":[{\"modality\":\"TEXT\",\"tokenCount\":2}],\"promptTokensDetails\":[{\"modality\":\"TEXT\",\"tokenCount\":5}],\"candidatesTokenCount\":2,\"promptTokenCount\":5,\"totalTokenCount\":7}}\n\n")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"finishReason\":\"STOP\",\"index\":0,\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"\",\"thoughtSignature\":\"EowBCokBAWkUfRPjxuSuo5LTGZudIKxa\"}]}}],\"usageMetadata\":{\"candidatesTokensDetails\":[{\"modality\":\"TEXT\",\"tokenCount\":6}],\"promptTokensDetails\":[{\"modality\":\"TEXT\",\"tokenCount\":36}],\"candidatesTokenCount\":6,\"promptTokenCount\":36,\"totalTokenCount\":42,\"cost\":0,\"opencode\":{\"billing_source\":\"go\",\"list_cost\":0.000132,\"allowance\":0.000264,\"tokens\":{\"input\":36,\"output\":6,\"reasoning\":0,\"cache_read\":0,\"cache_write_5m\":0,\"cache_write_1h\":0}}}}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	req := `{"model":"MODEL","messages":[{"role":"user","content":"Say hi in one word"}],"max_tokens":50,"stream":true,"stream_options":{"include_usage":true}}`
	// Strict: pre-commit 502, no fabricated success.
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1/chat/completions",
		strings.Replace(req, "MODEL", "bridge-gem", 1), nil)
	if status != 502 {
		t.Fatalf("strict stream must fail with 502, got %d body=%s", status, data)
	}
	// Lossy: the stream delivers text, finish and usage.
	status, headers, data := postGeminiBridge(t, h.router.URL+"/v1/chat/completions",
		strings.Replace(req, "MODEL", "bridge-gem-lossy", 1), nil)
	if status != 200 {
		t.Fatalf("lossy stream must translate, got %d body=%s", status, data)
	}
	if ct := headers.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("SSE framing must be text/event-stream, got %q", ct)
	}
	out := string(data)
	if !strings.Contains(out, `"content":"Hi!"`) {
		t.Fatalf("lossy stream must carry the text: %s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("lossy stream must carry the finish: %s", out)
	}
	if !strings.Contains(out, `"prompt_tokens":36`) {
		t.Fatalf("lossy stream must carry terminal usage: %s", out)
	}
	for _, leak := range []string{"thoughtSignature", "opencode"} {
		if strings.Contains(out, leak) {
			t.Fatalf("downstream stream must not carry %q: %s", leak, out)
		}
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("Chat SSE must terminate with [DONE]: %s", out)
	}
}
