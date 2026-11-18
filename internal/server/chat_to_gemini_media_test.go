package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Chat->Gemini image vertical over HTTP: ordered user text/image_url data
// URLs bridge to Gemini inlineData with no loss header; remote URLs keep the
// existing reviewed loss; malformed data URLs are 400 pre-I/O.
func TestC2GChatToGeminiImageOrderHTTP(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}],"role":"model"},"finishReason":"STOP","index":0}],"modelVersion":"gemini-2.0-flash","responseId":"r","usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3,"totalTokenCount":10}}`))
	}
	body := `{"model":"bridge-gem","messages":[{"role":"user","content":[
		{"type":"text","text":"a"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,` + g11PNG + `"}},
		{"type":"text","text":"b"},
		{"type":"image_url","image_url":{"url":"data:image/gif;base64,eA=="}}
	]}]}`
	status, headers, _ := postGeminiBridge(t, h.router.URL+"/v1/chat/completions", body, nil)
	if status != 200 {
		t.Fatalf("status=%d, want 200", status)
	}
	if loss := headers.Get("X-Model-Router-Loss"); loss != "" {
		t.Fatalf("image bridge must carry no loss, got %s", loss)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var wire struct {
		Contents []struct {
			Role  string `json:"role"`
			Parts []struct {
				Text       string `json:"text"`
				InlineData *struct {
					MIME string `json:"mimeType"`
					Data string `json:"data"`
				} `json:"inlineData"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(h.lastBody, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Contents) != 1 || len(wire.Contents[0].Parts) != 4 {
		t.Fatalf("ordered parts length wrong: %s", h.lastBody)
	}
	if wire.Contents[0].Parts[0].Text != "a" || wire.Contents[0].Parts[2].Text != "b" {
		t.Fatalf("text order wrong: %s", h.lastBody)
	}
	if wire.Contents[0].Parts[1].InlineData == nil || wire.Contents[0].Parts[1].InlineData.Data != g11PNG {
		t.Fatalf("exact data not preserved: %s", h.lastBody)
	}
	if wire.Contents[0].Parts[3].InlineData == nil || wire.Contents[0].Parts[3].InlineData.MIME != "image/gif" {
		t.Fatalf("gif missing: %s", h.lastBody)
	}
}

func TestC2GChatToGeminiImageLossAndMalformedHTTP(t *testing.T) {
	// Remote URL: strict 400 with no I/O; lossy bridges text-only with a header.
	remote := `{"model":"bridge-gem","messages":[{"role":"user","content":[
		{"type":"text","text":"hi"},
		{"type":"image_url","image_url":{"url":"https://example.invalid/img.png"}}]}]}`
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1/chat/completions", remote, nil)
	if status != 400 || !strings.Contains(string(data), "translation_loss") {
		t.Fatalf("strict remote status=%d body=%s, want 400 translation_loss", status, data)
	}
	if n := gemCalls(h); n != 0 {
		t.Fatalf("strict upstream calls=%d, want 0", n)
	}
	hl := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	hl.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"hi"}],"role":"model"},"finishReason":"STOP","index":0}],"modelVersion":"m","responseId":"r","usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`))
	}
	remoteLossy := strings.Replace(remote, `"bridge-gem"`, `"bridge-gem-lossy"`, 1)
	status, headers, _ := postGeminiBridge(t, hl.router.URL+"/v1/chat/completions", remoteLossy, nil)
	if status != 200 {
		t.Fatalf("lossy remote status=%d, want 200", status)
	}
	if headers.Get("X-Model-Router-Loss") == "" {
		t.Fatalf("lossy remote must carry a loss header")
	}
	hl.mu.Lock()
	if strings.Contains(string(hl.lastBody), "inlineData") {
		t.Fatalf("remote must never reach Gemini wire: %s", hl.lastBody)
	}
	hl.mu.Unlock()
	// Malformed data URL: 400 pre-I/O in both modes.
	bad := `{"model":"bridge-gem","messages":[{"role":"user","content":[
		{"type":"image_url","image_url":{"url":"data:image/png;base64,not base64!!"}}]}]}`
	for _, model := range []string{"bridge-gem", "bridge-gem-lossy"} {
		hb := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
		payload := strings.Replace(bad, `"bridge-gem"`, `"`+model+`"`, 1)
		status, _, _ := postGeminiBridge(t, hb.router.URL+"/v1/chat/completions", payload, nil)
		if status != 400 {
			t.Fatalf("%s malformed status=%d, want 400", model, status)
		}
		if n := gemCalls(hb); n != 0 {
			t.Fatalf("%s malformed upstream calls=%d, want 0", model, n)
		}
	}
}
