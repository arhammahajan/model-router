package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func mustG2AJSON(t *testing.T, raw string) []byte {
	t.Helper()
	out, rerr := TranslateGeminiJSONToAnthropic([]byte(raw), "public", DefaultLimits())
	if rerr != nil {
		t.Fatalf("translate failed: %v", rerr)
	}
	return out
}

func g2aJSONExpect502(t *testing.T, raw string) *RouterError {
	t.Helper()
	_, rerr := TranslateGeminiJSONToAnthropic([]byte(raw), "public", DefaultLimits())
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("expected 502 for %s, got %v", raw, rerr)
	}
	return rerr
}

type g2aBody struct {
	ID    string  `json:"id"`
	Type  string  `json:"type"`
	Role  string  `json:"role"`
	Model string  `json:"model"`
	Stop  string  `json:"stop_reason"`
	Seq   *string `json:"stop_sequence"`
	Usage struct {
		Input  int `json:"input_tokens"`
		Output int `json:"output_tokens"`
		Read   int `json:"cache_read_input_tokens"`
	} `json:"usage"`
	Content []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"content"`
}

func decodeG2A(t *testing.T, out []byte) g2aBody {
	t.Helper()
	var body g2aBody
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func g2aTextResp(text string, usage string) string {
	return `{"candidates":[{"content":{"role":"model","parts":[{"text":` + text + `}]},
		"finishReason":"STOP"}],"usageMetadata":` + usage + `}`
}

func g2aUsage(prompt, completion, total int) string {
	return fmt.Sprintf(`{"promptTokenCount":%d,"candidatesTokenCount":%d,"totalTokenCount":%d}`,
		prompt, completion, total)
}

func TestG2ATextEnvelope(t *testing.T) {
	// Closed text response: stable model, end_turn, exact usage with the
	// Gemini prompt total split into input plus cache_read.
	out := mustG2AJSON(t, g2aTextResp(`"hi ✓"`,
		`{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15,
			"cachedContentTokenCount":4}`))
	body := decodeG2A(t, out)
	if body.Type != "message" || body.Role != "assistant" || body.Model != "public" {
		t.Fatalf("envelope wrong: %s", out)
	}
	if !strings.HasPrefix(body.ID, "msg_") {
		t.Fatalf("message id wrong: %q", body.ID)
	}
	if body.Stop != "end_turn" || body.Seq != nil {
		t.Fatalf("stop wrong: %+v", body)
	}
	if len(body.Content) != 1 || body.Content[0].Type != "text" || body.Content[0].Text != "hi ✓" {
		t.Fatalf("text lost: %s", out)
	}
	if body.Usage.Input != 6 || body.Usage.Output != 5 || body.Usage.Read != 4 {
		t.Fatalf("cache accounting wrong: %+v", body.Usage)
	}
}

func TestG2AOrderToolFirstAndInterleaved(t *testing.T) {
	// Upstream part order is preserved: tool-first stays tool-first and
	// text->call->text renders three blocks; number literals survive.
	raw := `{"candidates":[{"content":{"role":"model","parts":[
		{"functionCall":{"name":"calc","args":{"n":9007199254740993}}},
		{"text":"mid"},
		{"functionCall":{"name":"calc","id":"supplied","args":{}}}]},
		"finishReason":"STOP"}],
		"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":4,"totalTokenCount":7}}`
	body := decodeG2A(t, mustG2AJSON(t, raw))
	if body.Stop != "tool_use" {
		t.Fatalf("calls must report tool_use, got %q", body.Stop)
	}
	if len(body.Content) != 3 {
		t.Fatalf("order wrong: %+v", body.Content)
	}
	if body.Content[0].Type != "tool_use" || body.Content[0].ID == "" {
		t.Fatalf("tool-first lost: %+v", body.Content[0])
	}
	if body.Content[1].Type != "text" || body.Content[1].Text != "mid" {
		t.Fatalf("interleaved text lost: %+v", body.Content[1])
	}
	if body.Content[2].Type != "tool_use" || body.Content[2].ID != "supplied" {
		t.Fatalf("supplied id not preserved: %+v", body.Content[2])
	}
	if !strings.Contains(string(body.Content[0].Input), "9007199254740993") {
		t.Fatalf("bigint args not exact: %s", body.Content[0].Input)
	}
	if body.Content[0].ID == body.Content[2].ID {
		t.Fatal("minted id collided with a supplied id")
	}
	// Empty text parts create no blocks; an empty turn still emits one
	// empty text block so content is never empty.
	empty := `{"candidates":[{"content":{"role":"model","parts":[{"text":""}]},
		"finishReason":"STOP"}],
		"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":0,"totalTokenCount":1}}`
	eb := decodeG2A(t, mustG2AJSON(t, empty))
	if len(eb.Content) != 1 || eb.Content[0].Type != "text" || eb.Content[0].Text != "" {
		t.Fatalf("empty turn wrong: %+v", eb.Content)
	}
}

func TestG2AStopReasons(t *testing.T) {
	usage := `{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}`
	mk := func(finish string) string {
		return `{"candidates":[{"content":{"role":"model","parts":[{"text":"t"}]},
			"finishReason":"` + finish + `"}],"usageMetadata":` + usage + `}`
	}
	if body := decodeG2A(t, mustG2AJSON(t, mk("MAX_TOKENS"))); body.Stop != "max_tokens" {
		t.Fatalf("MAX_TOKENS must map to max_tokens, got %q", body.Stop)
	}
	for _, f := range []string{"SAFETY", "RECITATION", "BLOCKLIST", "SPII", "LANGUAGE"} {
		if body := decodeG2A(t, mustG2AJSON(t, mk(f))); body.Stop != "refusal" {
			t.Fatalf("%s must map to refusal, got %q", f, body.Stop)
		}
	}
	for _, f := range []string{"OTHER", "MALFORMED_FUNCTION_CALL", "FINISH_REASON_UNSPECIFIED", "CONTINUATION"} {
		g2aJSONExpect502(t, mk(f))
	}
	// A safety-filter terminal with tool calls is contradictory: no
	// filter+calls completion is ever emitted.
	g2aJSONExpect502(t, `{"candidates":[{"content":{"role":"model","parts":[
		{"functionCall":{"name":"calc","args":{}}}]},"finishReason":"SAFETY"}],
		"usageMetadata":`+usage+`}`)
	// Missing finish and multiple choices fail closed.
	g2aJSONExpect502(t, `{"candidates":[{"content":{"role":"model","parts":[{"text":"t"}]}}],
		"usageMetadata":`+usage+`}`)
	g2aJSONExpect502(t, `{"candidates":[
		{"content":{"role":"model","parts":[{"text":"a"}]},"finishReason":"STOP"},
		{"content":{"role":"model","parts":[{"text":"b"}]},"finishReason":"STOP"}],
		"usageMetadata":`+usage+`}`)
}

func TestG2AUsageClosed(t *testing.T) {
	ok := func(usage string) g2aBody {
		t.Helper()
		return decodeG2A(t, mustG2AJSON(t, g2aTextResp(`"t"`, usage)))
	}
	// Exact totals, no double counting of the cache share.
	if b := ok(g2aUsage(10, 5, 15)); b.Usage.Input != 10 || b.Usage.Output != 5 {
		t.Fatalf("usage wrong: %+v", b.Usage)
	}
	// Mismatched totals, cache overruns and overflows fail rather than
	// underreport.
	for _, u := range []string{
		g2aUsage(10, 5, 14),
		`{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15,"cachedContentTokenCount":11}`,
		`{"promptTokenCount":10.5,"candidatesTokenCount":5,"totalTokenCount":15}`,
		`{"promptTokenCount":"10","candidatesTokenCount":5,"totalTokenCount":15}`,
		`{"candidatesTokenCount":5,"totalTokenCount":15}`,
		`{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15,
			"thoughtsTokenCount":2}`,
		`{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15,
			"toolUsePromptTokenCount":1}`,
		`{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15,
			"promptTokensDetails":[{"modality":"TEXT","tokenCount":3}]}`,
		`{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15,"bogus":1}`,
	} {
		g2aJSONExpect502(t, g2aTextResp(`"t"`, u))
	}
	// Zero thought/tool-use shares and null known members are harmless.
	ok(`{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15,
		"thoughtsTokenCount":0,"toolUsePromptTokenCount":null}`)
}

func TestG2AClosedValidation(t *testing.T) {
	usage := g2aUsage(2, 1, 3)
	mk := func(parts, extra string) string {
		return `{"candidates":[{"content":{"role":"model","parts":[` + parts + `]},
			"finishReason":"STOP"` + extra + `}],"usageMetadata":` + usage + `}`
	}
	// Roles other than missing/model fail, before any downstream write.
	g2aJSONExpect502(t, `{"candidates":[{"content":{"role":"assistant","parts":[{"text":"t"}]},
		"finishReason":"STOP"}],"usageMetadata":`+usage+`}`)
	// Oneof violations, thought state, signatures, media, server tools and
	// partial-arg streaming all fail closed.
	for _, parts := range []string{
		`{"text":"a","functionCall":{"name":"c","args":{}}}`,
		`{"text":"a","thought":true}`,
		`{"text":"a","thoughtSignature":"s"}`,
		`{"functionCall":{"name":"c","args":{},"thoughtSignature":"s"}}`,
		`{"functionCall":{"name":"c","args":{},"partialArgs":["x"]}}`,
		`{"functionCall":{"name":"c","args":{},"willContinue":true}}`,
		`{"functionCall":{"name":"","args":{}}}`,
		`{"inlineData":{"mimeType":"image/png","data":"eA=="}}`,
		`{"executableCode":{"language":"PYTHON","code":"x"}}`,
		`{"toolCall":{"toolType":"X"}}`,
		`{"functionResponse":{"name":"c","response":{"output":"x"}}}`,
		`{"text":"a","bogus":1}`,
	} {
		g2aJSONExpect502(t, mk(parts, ""))
	}
	// Unknown candidate members and populated safety/grounding/citation
	// payloads fail; documented empty ratings pass.
	g2aJSONExpect502(t, mk(`{"text":"t"}`, `,"bogus":1`))
	g2aJSONExpect502(t, mk(`{"text":"t"}`, `,"safetyRatings":[{"category":"HARM","probability":"HIGH"}]`))
	g2aJSONExpect502(t, mk(`{"text":"t"}`, `,"citationMetadata":{"citationSources":[]}`))
	g2aJSONExpect502(t, mk(`{"text":"t"}`, `,"groundingMetadata":{}}`))
	g2aJSONExpect502(t, mk(`{"text":"t"}`, `,"avgLogprobs":0.5`))
	g2aJSONExpect502(t, mk(`{"text":"t"}`, `,"continuationToken":"x"`))
	mustG2AJSON(t, mk(`{"text":"t"}`, `,"safetyRatings":[]`))
	// Conflicting call identities fail.
	g2aJSONExpect502(t, mk(`{"functionCall":{"name":"a","id":"dup","args":{}}},
		{"functionCall":{"name":"b","id":"dup","args":{}}}`, ""))
	// Unknown envelope members: null tolerated, non-null rejected.
	mustG2AJSON(t, `{"candidates":[{"content":{"role":"model","parts":[{"text":"t"}]},
		"finishReason":"STOP"}],"usageMetadata":`+usage+`,"future":null}`)
	g2aJSONExpect502(t, `{"candidates":[{"content":{"role":"model","parts":[{"text":"t"}]},
		"finishReason":"STOP"}],"usageMetadata":`+usage+`,"future":{}}`)
}

func TestG2ABlockedPrompt(t *testing.T) {
	usage := g2aUsage(4, 0, 4)
	// A blocked prompt becomes incomplete/content_filter with terminal
	// usage, never a completion.
	out := mustG2AJSON(t, `{"promptFeedback":{"blockReason":"SAFETY"},
		"usageMetadata":`+usage+`}`)
	body := decodeG2A(t, out)
	if body.Stop != "refusal" {
		t.Fatalf("blocked prompt must refuse, got %q", body.Stop)
	}
	if len(body.Content) != 1 || body.Content[0].Type != "text" {
		t.Fatalf("blocked prompt content wrong: %+v", body.Content)
	}
	// Blocked prompt plus candidates contradicts the response.
	g2aJSONExpect502(t, `{"promptFeedback":{"blockReason":"SAFETY"},
		"candidates":[{"content":{"role":"model","parts":[{"text":"t"}]},"finishReason":"STOP"}],
		"usageMetadata":`+usage+`}`)
	// Blocked prompt without usage fails; unknown block reasons fail.
	g2aJSONExpect502(t, `{"promptFeedback":{"blockReason":"SAFETY"}}`)
	g2aJSONExpect502(t, `{"promptFeedback":{"blockReason":"MYSTERY"},"usageMetadata":`+usage+`}`)
}

func g2aBlockedRaw(candidates string) string {
	return `{"candidates":` + candidates + `,"promptFeedback":{"blockReason":"SAFETY"},` +
		`"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":0,"totalTokenCount":1}}`
}

func TestG2ABlockedCandidateShapesCoherent(t *testing.T) {
	// Absent, null and empty-array candidates are coherent blocked shapes:
	// every framing accepts them as a refusal. Malformed shapes reject,
	// and blocked responses carrying nonempty candidates fail as
	// contradictory.
	for _, cands := range []string{"", "null", "[]"} {
		raw := `{"promptFeedback":{"blockReason":"SAFETY"},` +
			`"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":0,"totalTokenCount":1}}`
		if cands != "" {
			raw = g2aBlockedRaw(cands)
		}
		out, rerr := TranslateGeminiJSONToAnthropic([]byte(raw), "public", DefaultLimits())
		if rerr != nil {
			t.Fatalf("JSON blocked %q rejected: %v", cands, rerr)
		}
		var body g2aBody
		if err := json.Unmarshal(out, &body); err != nil {
			t.Fatal(err)
		}
		if body.Stop != "refusal" {
			t.Fatalf("JSON blocked %q must be refusal: %s", cands, out)
		}
		for _, framing := range []string{"sse", "array"} {
			var upstream string
			if framing == "sse" {
				upstream = g2aSSEChunk(raw)
			} else {
				upstream = "[" + raw + "]"
			}
			events := g2aStreamEvents(t, upstream, framing)
			last := events[len(events)-2]
			if last["type"] != "message_delta" || last["delta"].(map[string]any)["stop_reason"] != "refusal" {
				t.Fatalf("%s blocked %q must end in refusal: %v", framing, cands, last)
			}
		}
	}
	for _, raw := range []string{g2aBlockedRaw(`{}`), g2aBlockedRaw(`"x"`), g2aBlockedRaw(`[{}]`)} {
		if _, rerr := TranslateGeminiJSONToAnthropic([]byte(raw), "public", DefaultLimits()); rerr == nil || rerr.Status != 502 {
			t.Fatalf("malformed/contradictory blocked JSON must be 502: %q got %v", raw, rerr)
		}
		for _, framing := range []string{"sse", "array"} {
			var upstream string
			if framing == "sse" {
				upstream = g2aSSEChunk(raw)
			} else {
				upstream = "[" + raw + "]"
			}
			g2aStreamExpectFail(t, upstream, framing)
		}
	}
	// Blocked prompt with candidate content contradicts the block.
	contradictory := `{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]}}],` +
		`"promptFeedback":{"blockReason":"SAFETY"},` +
		`"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`
	if _, rerr := TranslateGeminiJSONToAnthropic([]byte(contradictory), "public", DefaultLimits()); rerr == nil {
		t.Fatal("blocked JSON with content must fail")
	}
	for _, framing := range []string{"sse", "array"} {
		var upstream string
		if framing == "sse" {
			upstream = g2aSSEChunk(contradictory)
		} else {
			upstream = "[" + contradictory + "]"
		}
		g2aStreamExpectFail(t, upstream, framing)
	}
}
