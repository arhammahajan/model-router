package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Gemini 4 requires the opaque part-level thoughtSignature back on
// functionCall history parts; a missing signature is a hard upstream
// 400 on every tool-calling conversation turn after the first. The
// router is stateless, so the signature rides in the router-minted
// public tool-call identity across the harness round trip.

const signedCallUpstreamBody = `{"responseId":"XPf9arGkGb6C1sgPkfDFkQU","modelVersion":"gemini-4-argon","candidates":[{"finishReason":"STOP","finishMessage":"Model generated function call(s).","index":0,"content":{"role":"model","parts":[{"thoughtSignature":"EowBCokBAWkUfRPi///kIqNusNnUG5T5oJtfzBTSxTtXCyvhqFqH1swcqZbsQAd7HpSyqtd9YUM","functionCall":{"id":"call_3375893482179694677","name":"get_weather","args":{"city":"Tokyo"}}}]}}],"usageMetadata":{"promptTokenCount":94,"candidatesTokenCount":22,"totalTokenCount":116,"cost":0,"opencode":{"billing_source":"go","list_cost":0.000408,"allowance":0.000816,"tokens":{"input":94,"output":22,"reasoning":0,"cache_read":0,"cache_write_5m":0,"cache_write_1h":0}}}}`

func TestChatToGeminiSignatureRoundTripTwoTurns(t *testing.T) {
	h := newGeminiBridgeHarness(t, geminiBridgeCfg(t))
	tools := `"tools":[{"type":"function","function":{"name":"get_weather","description":"Get current weather for a city","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}]`
	// Turn 1: the lossy bridge returns the tool call with a wrapped
	// public identity; strict refuses the signature outright.
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, signedCallUpstreamBody)
	}
	status, _, data := postGeminiBridge(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-gem-lossy","messages":[{"role":"user","content":"What is the weather in Tokyo? Use the tool."}],"max_tokens":200,`+tools+`}`, nil)
	if status != 200 {
		t.Fatalf("turn 1 lossy must translate, got %d body=%s", status, data)
	}
	var turn1 struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID string `json:"id"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &turn1); err != nil || len(turn1.Choices) != 1 || len(turn1.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("turn 1 must carry one tool call: %s", data)
	}
	publicID := turn1.Choices[0].Message.ToolCalls[0].ID
	if !strings.HasPrefix(publicID, "gts1_") {
		t.Fatalf("public id must be signature-wrapped, got %q", publicID)
	}
	status, _, data = postGeminiBridge(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-gem","messages":[{"role":"user","content":"What is the weather in Tokyo? Use the tool."}],"max_tokens":200,`+tools+`}`, nil)
	if status != 502 || !strings.Contains(string(data), "has no representation") {
		t.Fatalf("turn 1 strict must refuse, got %d body=%s", status, data)
	}
	// Turn 2: the harness echoes the wrapped identity; the upstream
	// request must restore the signed part and the provider id.
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"Cloudy, 18C"}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":120,"candidatesTokenCount":5,"totalTokenCount":125}}`)
	}
	status, _, data = postGeminiBridge(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-gem-lossy","messages":[{"role":"user","content":"What is the weather in Tokyo? Use the tool."},`+
			`{"role":"assistant","content":"","tool_calls":[{"id":`+quoteJSON(publicID)+`,"type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Tokyo\"}"}}]},`+
			`{"role":"tool","tool_call_id":`+quoteJSON(publicID)+`,"content":"{\"temp_c\":18,\"condition\":\"cloudy\"}"}],`+
			`"max_tokens":200,`+tools+`}`, nil)
	if status != 200 {
		t.Fatalf("turn 2 must translate, got %d body=%s", status, data)
	}
	h.mu.Lock()
	up := string(h.lastBody)
	h.mu.Unlock()
	if !strings.Contains(up, `"thoughtSignature":"EowBCokBAWkUfRPi///kIqNusNnUG5T5oJtfzBTSxTtXCyvhqFqH1swcqZbsQAd7HpSyqtd9YUM"`) {
		t.Fatalf("turn 2 upstream must carry the restored signature: %s", up)
	}
	if !strings.Contains(up, `"id":"call_3375893482179694677"`) {
		t.Fatalf("turn 2 upstream must restore the provider id: %s", up)
	}
	if strings.Count(up, `"functionResponse"`) != 1 || !strings.Contains(up, `"functionCall"`) {
		t.Fatalf("turn 2 upstream must carry one call and one response: %s", up)
	}
	if !strings.Contains(string(data), `"content":"Cloudy, 18C"`) {
		t.Fatalf("turn 2 downstream must carry the answer: %s", data)
	}
}

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
