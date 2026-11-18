// Gemini -> Responses JSON response translation tests.
package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func g2rJSON(t *testing.T, body string) map[string]any {
	t.Helper()
	out, rerr := TranslateGeminiJSONToResponses([]byte(body), "public-model", DefaultLimits())
	if rerr != nil {
		t.Fatalf("translate failed: %v", rerr)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("downstream not JSON: %v", err)
	}
	return decoded
}

func g2rJSONErr(t *testing.T, body string) *RouterError {
	t.Helper()
	_, rerr := TranslateGeminiJSONToResponses([]byte(body), "public-model", DefaultLimits())
	if rerr == nil {
		t.Fatalf("expected error for %s", body)
	}
	return rerr
}

func g2rUsage(body string) map[string]any {
	return map[string]any{
		"promptTokenCount": 10, "candidatesTokenCount": 5, "totalTokenCount": 15,
	}
}

func TestGeminiToResponsesTextCompleted(t *testing.T) {
	usage, _ := json.Marshal(g2rUsage(""))
	raw := `{"candidates":[{"content":{"role":"model","parts":[{"text":"Hello"}]},
		"finishReason":"STOP","index":0}],"usageMetadata":` + string(usage) + `}`
	decoded := g2rJSON(t, raw)
	if decoded["model"] != "public-model" {
		t.Fatalf("model = %v", decoded["model"])
	}
	if decoded["status"] != "completed" {
		t.Fatalf("status = %v", decoded["status"])
	}
	output := decoded["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output = %v", decoded["output"])
	}
	item := output[0].(map[string]any)
	if item["type"] != "message" || item["role"] != "assistant" {
		t.Fatalf("item = %v", item)
	}
	content := item["content"].([]any)
	part := content[0].(map[string]any)
	if part["type"] != "output_text" || part["text"] != "Hello" {
		t.Fatalf("part = %v", part)
	}
	usageOut := decoded["usage"].(map[string]any)
	if usageOut["input_tokens"] != float64(10) || usageOut["output_tokens"] != float64(5) ||
		usageOut["total_tokens"] != float64(15) {
		t.Fatalf("usage = %v", usageOut)
	}
	det := usageOut["input_tokens_details"].(map[string]any)
	if det["cached_tokens"] != float64(0) {
		t.Fatalf("input details = %v", det)
	}
}

func TestGeminiToResponsesToolCallsPreserved(t *testing.T) {
	usage, _ := json.Marshal(map[string]any{
		"promptTokenCount": 4, "candidatesTokenCount": 6, "totalTokenCount": 10,
	})
	raw := `{"candidates":[{"content":{"role":"model","parts":[
		{"text":"Calling"},
		{"functionCall":{"name":"lookup","args":{"n":9007199254740993},"id":"supplied-1"}},
		{"functionCall":{"name":"other","args":{}}}]},
		"finishReason":"STOP"}],"usageMetadata":` + string(usage) + `}`
	decoded := g2rJSON(t, raw)
	output := decoded["output"].([]any)
	if len(output) != 3 {
		t.Fatalf("output len = %d: %v", len(output), decoded["output"])
	}
	if output[0].(map[string]any)["type"] != "message" {
		t.Fatalf("first item must be the text message: %v", output[0])
	}
	call := output[1].(map[string]any)
	if call["type"] != "function_call" || call["call_id"] != "supplied-1" || call["name"] != "lookup" {
		t.Fatalf("call = %v", call)
	}
	args := call["arguments"].(string)
	if !strings.Contains(args, "9007199254740993") {
		t.Fatalf("large integer not preserved: %q", args)
	}
	var v any
	dec := json.NewDecoder(strings.NewReader(args))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("args not JSON: %v", err)
	}
	if _, ok := v.(map[string]any); !ok {
		t.Fatalf("args must be an object: %q", args)
	}
	// Missing ids mint scoped compat ids distinct from supplied ones.
	minted := output[2].(map[string]any)["call_id"].(string)
	if minted == "supplied-1" || minted == "" {
		t.Fatalf("minted id collides or empty: %q", minted)
	}
}

func TestGeminiToResponsesMintedIDsCollisionSafe(t *testing.T) {
	usage, _ := json.Marshal(map[string]any{
		"promptTokenCount": 4, "candidatesTokenCount": 6, "totalTokenCount": 10,
	})
	raw := `{"candidates":[{"content":{"role":"model","parts":[
		{"functionCall":{"name":"a","args":{},"id":"g2r-call-0"}},
		{"functionCall":{"name":"b","args":{}}}]},
		"finishReason":"STOP"}],"usageMetadata":` + string(usage) + `}`
	decoded := g2rJSON(t, raw)
	output := decoded["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("output = %v", decoded["output"])
	}
	first := output[0].(map[string]any)["call_id"].(string)
	second := output[1].(map[string]any)["call_id"].(string)
	if first != "g2r-call-0" {
		t.Fatalf("supplied id not preserved: %q", first)
	}
	if second == "g2r-call-0" || second == "" {
		t.Fatalf("minted id collides or empty: %q", second)
	}
	if output[0].(map[string]any)["id"] == output[1].(map[string]any)["id"] {
		t.Fatalf("item ids must be distinct")
	}
}

func TestGeminiToResponsesTerminalMapping(t *testing.T) {
	usage, _ := json.Marshal(map[string]any{
		"promptTokenCount": 4, "candidatesTokenCount": 6, "totalTokenCount": 10,
	})
	cases := []struct {
		finish   string
		status   string
		inReason string
	}{
		{"STOP", "completed", ""},
		{"MAX_TOKENS", "incomplete", "max_output_tokens"},
		{"SAFETY", "incomplete", "content_filter"},
		{"RECITATION", "incomplete", "content_filter"},
		{"BLOCKLIST", "incomplete", "content_filter"},
	}
	for _, c := range cases {
		raw := `{"candidates":[{"content":{"role":"model","parts":[{"text":"x"}]},
			"finishReason":` + `"` + c.finish + `"` + `}],"usageMetadata":` + string(usage) + `}`
		decoded := g2rJSON(t, raw)
		if decoded["status"] != c.status {
			t.Fatalf("%s: status = %v", c.finish, decoded["status"])
		}
		det := decoded["incomplete_details"]
		if c.inReason == "" {
			if det != nil {
				t.Fatalf("%s: incomplete_details = %v", c.finish, det)
			}
		} else {
			m := det.(map[string]any)
			if m["reason"] != c.inReason {
				t.Fatalf("%s: reason = %v", c.finish, det)
			}
		}
	}
	for _, finish := range []string{"MALFORMED_FUNCTION_CALL", "STOP_CANDIDATE", "OTHER", ""} {
		raw := `{"candidates":[{"content":{"role":"model","parts":[{"text":"x"}]},
			"finishReason":` + `"` + finish + `"` + `}],"usageMetadata":` + string(usage) + `}`
		if finish == "" {
			raw = `{"candidates":[{"content":{"role":"model","parts":[{"text":"x"}]}}],
				"usageMetadata":` + string(usage) + `}`
		}
		rerr := g2rJSONErr(t, raw)
		if rerr.Status != 502 {
			t.Fatalf("%q: status = %d, want 502", finish, rerr.Status)
		}
	}
}

func TestGeminiToResponsesBlockedPrompt(t *testing.T) {
	usage, _ := json.Marshal(map[string]any{
		"promptTokenCount": 4, "candidatesTokenCount": 0, "totalTokenCount": 4,
	})
	raw := `{"promptFeedback":{"blockReason":"SAFETY"},"usageMetadata":` + string(usage) + `}`
	decoded := g2rJSON(t, raw)
	if decoded["status"] != "incomplete" {
		t.Fatalf("status = %v", decoded["status"])
	}
	det := decoded["incomplete_details"].(map[string]any)
	if det["reason"] != "content_filter" {
		t.Fatalf("details = %v", det)
	}
	// Contradictory blocked responses never succeed.
	raw = `{"promptFeedback":{"blockReason":"SAFETY"},
		"candidates":[{"content":{"role":"model","parts":[{"text":"x"}]},"finishReason":"STOP"}],
		"usageMetadata":` + string(usage) + `}`
	if rerr := g2rJSONErr(t, raw); rerr.Status != 502 {
		t.Fatalf("contradictory blocked: status = %d", rerr.Status)
	}
}

func TestGeminiToResponsesUsageStrict(t *testing.T) {
	mk := func(usage string) string {
		return `{"candidates":[{"content":{"role":"model","parts":[{"text":"x"}]},
			"finishReason":"STOP"}],"usageMetadata":` + usage + `}`
	}
	// Missing usage never succeeds.
	if rerr := g2rJSONErr(t, `{"candidates":[{"content":{"role":"model","parts":[{"text":"x"}]},
		"finishReason":"STOP"}]}`); rerr.Status != 502 {
		t.Fatalf("missing usage: status = %d", rerr.Status)
	}
	// Nonzero thoughts and tool-use accounting have no exact Responses map.
	for _, u := range []string{
		`{"promptTokenCount":4,"candidatesTokenCount":6,"totalTokenCount":12,"thoughtsTokenCount":2}`,
		`{"promptTokenCount":4,"candidatesTokenCount":6,"totalTokenCount":12,"toolUsePromptTokenCount":2}`,
		`{"promptTokenCount":4,"candidatesTokenCount":6,"totalTokenCount":99}`,
		`{"promptTokenCount":4,"candidatesTokenCount":6,"totalTokenCount":10,"cachedContentTokenCount":99}`,
		`{"promptTokenCount":4,"candidatesTokenCount":6,"totalTokenCount":10,"promptTokensDetails":[{"x":1}]}`,
	} {
		if rerr := g2rJSONErr(t, mk(u)); rerr.Status != 502 {
			t.Fatalf("usage %s: status = %d, want 502", u, rerr.Status)
		}
	}
	// Cached share is reported honestly without double counting.
	decoded := g2rJSON(t, mk(`{"promptTokenCount":10,"candidatesTokenCount":5,
		"totalTokenCount":15,"cachedContentTokenCount":3}`))
	usageOut := decoded["usage"].(map[string]any)
	if usageOut["input_tokens"] != float64(10) {
		t.Fatalf("input must equal promptTokenCount verbatim: %v", usageOut)
	}
	det := usageOut["input_tokens_details"].(map[string]any)
	if det["cached_tokens"] != float64(3) {
		t.Fatalf("cached = %v", det)
	}
}

func TestGeminiToResponsesUnrepresentable(t *testing.T) {
	usage := `{"promptTokenCount":4,"candidatesTokenCount":6,"totalTokenCount":10}`
	mk := func(candidate, extra string) string {
		return `{"candidates":[{"content":{"role":"model","parts":[{"text":"x"}]},
			"finishReason":"STOP",` + candidate + `}],"usageMetadata":` + usage + extra + `}`
	}
	bodies := []string{
		mk(`"citationMetadata":{"x":1}`, ""),
		mk(`"groundingMetadata":{"x":1}`, ""),
		mk(`"logprobsResult":{"x":1}`, ""),
		mk(`"continuationToken":"x"`, ""),
		mk(`"avgLogprobs":0.5`, ""),
		mk(`"finishMessage":"nope"`, ""),
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"x"}]},"finishReason":"STOP"},
		 {"content":{"role":"model","parts":[{"text":"y"}]},"finishReason":"STOP"}],
		 "usageMetadata":` + usage + `}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"x"}]},"finishReason":"STOP"}],
		 "usageMetadata":` + usage + `,"unknownFuture":"x"}`,
	}
	for _, b := range bodies {
		if rerr := g2rJSONErr(t, b); rerr.Status != 502 {
			t.Fatalf("body %s: status = %d, want 502", b, rerr.Status)
		}
	}
	// Unknown null members are accepted as newer defaults.
	decoded := g2rJSON(t, mk(`"citationMetadata":null`, ""))
	_ = decoded
	g2rJSON(t, `{"candidates":[{"content":{"role":"model","parts":[{"text":"x"}]},
		"finishReason":"STOP"}],"usageMetadata":`+usage+`,"unknownFuture":null}`)
	// Populated safety ratings fail; empty defaults pass.
	rerr := g2rJSONErr(t, mk(`"safetyRatings":[{"category":"HARM","probability":"HIGH"}]`, ""))
	if rerr.Status != 502 {
		t.Fatalf("populated ratings: status = %d", rerr.Status)
	}
	g2rJSON(t, mk(`"safetyRatings":[{"category":"HARM","probability":"NEGLIGIBLE"}]`, ""))
	// Thought and media parts fail closed, never stripped.
	for _, part := range []string{
		`{"thought":true}`,
		`{"thoughtSignature":"abc"}`,
		`{"inlineData":{"mimeType":"image/png","data":"AA=="}}`,
		`{"executableCode":{"language":"PYTHON","code":"x"}}`,
		`{"functionResponse":{"name":"f","response":{"output":"x"}}}`,
		`{"text":"a","functionCall":{"name":"f","args":{}}}`,
	} {
		raw := `{"candidates":[{"content":{"role":"model","parts":[` + part + `]},
			"finishReason":"STOP"}],"usageMetadata":` + usage + `}`
		if rerr := g2rJSONErr(t, raw); rerr.Status != 502 {
			t.Fatalf("part %s: status = %d, want 502", part, rerr.Status)
		}
	}
	// Conflicting call identities fail.
	raw := `{"candidates":[{"content":{"role":"model","parts":[
		{"functionCall":{"name":"a","args":{},"id":"dup"}},
		{"functionCall":{"name":"b","args":{},"id":"dup"}}]},
		"finishReason":"STOP"}],"usageMetadata":` + usage + `}`
	if rerr := g2rJSONErr(t, raw); rerr.Status != 502 {
		t.Fatalf("dup ids: status = %d", rerr.Status)
	}
	// Non-object args fail.
	raw = `{"candidates":[{"content":{"role":"model","parts":[
		{"functionCall":{"name":"a","args":[1,2]}}]},
		"finishReason":"STOP"}],"usageMetadata":` + usage + `}`
	if rerr := g2rJSONErr(t, raw); rerr.Status != 502 {
		t.Fatalf("array args: status = %d", rerr.Status)
	}
}

func TestGeminiToResponsesToolFirstPreservesOrder(t *testing.T) {
	usage, _ := json.Marshal(map[string]any{
		"promptTokenCount": 4, "candidatesTokenCount": 6, "totalTokenCount": 10,
	})
	raw := `{"candidates":[{"content":{"role":"model","parts":[
		{"functionCall":{"name":"f","args":{},"id":"call1"}},
		{"text":"after"}]},
		"finishReason":"STOP"}],"usageMetadata":` + string(usage) + `}`
	decoded := g2rJSON(t, raw)
	output := decoded["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("output len = %d: %v", len(output), decoded["output"])
	}
	if output[0].(map[string]any)["type"] != "function_call" {
		t.Fatalf("tool-first order lost: %v", decoded["output"])
	}
	if output[1].(map[string]any)["type"] != "message" {
		t.Fatalf("tool-first order lost: %v", decoded["output"])
	}
	msg := output[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if msg["text"] != "after" {
		t.Fatalf("text = %v", msg)
	}
}

func TestGeminiToResponsesInterleavedPreservesOrder(t *testing.T) {
	usage, _ := json.Marshal(map[string]any{
		"promptTokenCount": 4, "candidatesTokenCount": 6, "totalTokenCount": 10,
	})
	raw := `{"candidates":[{"content":{"role":"model","parts":[
		{"text":"a"},
		{"functionCall":{"name":"f","args":{},"id":"c1"}},
		{"text":"b"}]},
		"finishReason":"STOP"}],"usageMetadata":` + string(usage) + `}`
	decoded := g2rJSON(t, raw)
	output := decoded["output"].([]any)
	if len(output) != 3 {
		t.Fatalf("output len = %d: %v", len(output), decoded["output"])
	}
	if output[0].(map[string]any)["type"] != "message" ||
		output[1].(map[string]any)["type"] != "function_call" ||
		output[2].(map[string]any)["type"] != "message" {
		t.Fatalf("interleaved order lost: %v", decoded["output"])
	}
	first := output[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]
	second := output[2].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]
	if first != "a" || second != "b" {
		t.Fatalf("texts = %q %q", first, second)
	}
}

func TestGeminiToResponsesRoleModelOnly(t *testing.T) {
	usage, _ := json.Marshal(map[string]any{
		"promptTokenCount": 4, "candidatesTokenCount": 6, "totalTokenCount": 10,
	})
	mk := func(roleFragment string) string {
		return `{"candidates":[{"content":{` + roleFragment + `"parts":[{"text":"x"}]},
			"finishReason":"STOP"}],"usageMetadata":` + string(usage) + `}`
	}
	// Explicit model role succeeds.
	g2rJSON(t, mk(`"role":"model",`))
	// Missing role stays allowed (consistent with existing policy).
	g2rJSON(t, mk(``))
	// Chat assistant alias is rejected on a Gemini response.
	if rerr := g2rJSONErr(t, mk(`"role":"assistant",`)); rerr.Status != 502 {
		t.Fatalf("assistant role: status = %d, want 502", rerr.Status)
	}
	// Malformed roles fail.
	if rerr := g2rJSONErr(t, mk(`"role":42,`)); rerr.Status != 502 {
		t.Fatalf("numeric role: status = %d, want 502", rerr.Status)
	}
}

func TestGeminiToResponsesFilterCallsContradiction(t *testing.T) {
	usage, _ := json.Marshal(map[string]any{
		"promptTokenCount": 4, "candidatesTokenCount": 6, "totalTokenCount": 10,
	})
	raw := `{"candidates":[{"content":{"role":"model","parts":[
		{"text":"x"},
		{"functionCall":{"name":"f","args":{},"id":"c1"}}]},
		"finishReason":"SAFETY"}],"usageMetadata":` + string(usage) + `}`
	if rerr := g2rJSONErr(t, raw); rerr.Status != 502 {
		t.Fatalf("filter+call: status = %d, want 502", rerr.Status)
	}
}
