package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// RED: Anthropic upstream JSON responses translated to Gemini
// GenerateContentResponse objects.

func a2gemAnthropicJSON(t *testing.T, body string) []byte {
	t.Helper()
	raw := []byte(body)
	out, rerr := TranslateAnthropicJSONToGemini(raw, "public-model", DefaultLimits())
	if rerr != nil {
		t.Fatalf("translate: %v", rerr)
	}
	return out
}

func a2gemGeminiCandidate(t *testing.T, out []byte) (map[string]any, map[string]any) {
	t.Helper()
	var top map[string]any
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatalf("output must be JSON: %v", err)
	}
	cands, ok := top["candidates"].([]any)
	if !ok || len(cands) != 1 {
		t.Fatalf("one candidate required: %s", out)
	}
	cand, ok := cands[0].(map[string]any)
	if !ok {
		t.Fatalf("candidate must be an object: %s", out)
	}
	return top, cand
}

// Text with end_turn maps to STOP with conserved usage.
func TestAnthropicJSONToGeminiText(t *testing.T) {
	out := a2gemAnthropicJSON(t, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x",
		"content":[{"type":"text","text":"hello"}],
		"stop_reason":"end_turn","stop_sequence":null,
		"usage":{"input_tokens":10,"cache_creation_input_tokens":3,"cache_read_input_tokens":4,"output_tokens":7}}`)
	top, cand := a2gemGeminiCandidate(t, out)
	if top["modelVersion"] != "public-model" {
		t.Fatalf("public model must be stamped as modelVersion: %s", out)
	}
	if top["responseId"] != "msg_1" {
		t.Fatalf("upstream id must be preserved as responseId: %s", out)
	}
	if cand["index"] != float64(0) || cand["finishReason"] != "STOP" {
		t.Fatalf("candidate must carry index 0 and STOP: %s", out)
	}
	content, ok := cand["content"].(map[string]any)
	if !ok || content["role"] != "model" {
		t.Fatalf("content must be a model content: %s", out)
	}
	parts, ok := content["parts"].([]any)
	if !ok || len(parts) != 1 || parts[0].(map[string]any)["text"] != "hello" {
		t.Fatalf("ordered text part required: %s", out)
	}
	usage, ok := top["usageMetadata"].(map[string]any)
	if !ok {
		t.Fatalf("usageMetadata required: %s", out)
	}
	// prompt = 10 + 3 + 4; cached = 4 (read only, no double count);
	// candidates = 7; total = 17 + 7.
	if usage["promptTokenCount"] != float64(17) || usage["cachedContentTokenCount"] != float64(4) ||
		usage["candidatesTokenCount"] != float64(7) || usage["totalTokenCount"] != float64(24) {
		t.Fatalf("usage must be conserved exactly: %s", out)
	}
}

// stop_sequence and tool_use map to STOP; max_tokens maps to MAX_TOKENS.
func TestAnthropicJSONToGeminiStopReasons(t *testing.T) {
	for stop, want := range map[string]string{
		"stop_sequence": "STOP",
		"tool_use":      "STOP",
		"max_tokens":    "MAX_TOKENS",
	} {
		out := a2gemAnthropicJSON(t, `{"id":"m","type":"message","role":"assistant","model":"c",
			"content":[{"type":"text","text":"t"}],"stop_reason":"`+stop+`",
			"usage":{"input_tokens":1,"output_tokens":1}}`)
		_, cand := a2gemGeminiCandidate(t, out)
		if cand["finishReason"] != want {
			t.Fatalf("%s must map to %s: %s", stop, want, out)
		}
	}
	// Unrepresentable stops fail closed, never STOP.
	for _, stop := range []string{"pause_turn", "refusal", "model_context_window_exceeded", "bogus"} {
		raw := []byte(`{"id":"m","type":"message","role":"assistant","model":"c",
			"content":[{"type":"text","text":"t"}],"stop_reason":"` + stop + `",
			"usage":{"input_tokens":1,"output_tokens":1}}`)
		if _, rerr := TranslateAnthropicJSONToGemini(raw, "p", DefaultLimits()); rerr == nil || rerr.Status != 502 {
			t.Fatalf("%s must fail closed, got %v", stop, rerr)
		}
	}
}

// Tool calls keep order, identities and exact argument literals.
func TestAnthropicJSONToGeminiTools(t *testing.T) {
	out := a2gemAnthropicJSON(t, `{"id":"m","type":"message","role":"assistant","model":"c",
		"content":[
			{"type":"text","text":"lead"},
			{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"n":9007199254740993}},
			{"type":"tool_use","id":"toolu_2","name":"get_time","input":{}}
		],
		"stop_reason":"tool_use","stop_sequence":null,
		"usage":{"input_tokens":5,"output_tokens":9}}`)
	_, cand := a2gemGeminiCandidate(t, out)
	parts := cand["content"].(map[string]any)["parts"].([]any)
	if len(parts) != 3 {
		t.Fatalf("blocks must stay ordered: %s", out)
	}
	if parts[0].(map[string]any)["text"] != "lead" {
		t.Fatalf("text must lead: %s", out)
	}
	call := parts[1].(map[string]any)["functionCall"].(map[string]any)
	if call["id"] != "toolu_1" || call["name"] != "get_weather" {
		t.Fatalf("identity must survive: %s", out)
	}
	if !strings.Contains(string(out), "9007199254740993") {
		t.Fatalf("large integers must survive raw: %s", out)
	}
	if cand["finishReason"] != "STOP" {
		t.Fatalf("tool_use must map to STOP: %s", out)
	}
}

// Thought, signature, media, citation, server-tool and unknown blocks fail
// closed in both modes (there is no lossy downgrade on this path).
func TestAnthropicJSONToGeminiUnrepresentable(t *testing.T) {
	blocks := []string{
		`{"type":"thinking","thinking":"h","signature":"s"}`,
		`{"type":"redacted_thinking","data":"enc"}`,
		`{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{}}`,
		`{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[]}`,
		`{"type":"text","text":"t","citations":[{"type":"char_location","cited_text":"c","document_index":0,"start_char_index":0,"end_char_index":1}]}`,
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}`,
		`{"type":"mystery","x":1}`,
		`{"type":"tool_use","id":"toolu_9","name":"bad name","input":{}}`,
	}
	for _, b := range blocks {
		raw := []byte(`{"id":"m","type":"message","role":"assistant","model":"c","content":[` + b + `],
			"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
		if _, rerr := TranslateAnthropicJSONToGemini(raw, "p", DefaultLimits()); rerr == nil || rerr.Status != 502 {
			t.Fatalf("block must fail closed: %s -> %v", b, rerr)
		}
	}
}

// Non-null unmapped usage/billing members fail; nulls are absent.
func TestAnthropicJSONToGeminiUsagePolicy(t *testing.T) {
	for _, usage := range []string{
		`"usage":{"input_tokens":1,"output_tokens":1,"server_tool_use":{"web_search_requests":1}}`,
		`"usage":{"input_tokens":1,"output_tokens":1,"service_tier":"standard"}`,
		`"usage":{"input_tokens":1,"output_tokens":1,"output_tokens_details":{"thinking_tokens":1}}`,
		`"usage":{"input_tokens":1,"output_tokens":1,"cache_creation":{"ephemeral_5m_input_tokens":1}}`,
	} {
		raw := []byte(`{"id":"m","type":"message","role":"assistant","model":"c",
			"content":[{"type":"text","text":"t"}],"stop_reason":"end_turn",` + usage + `}`)
		if _, rerr := TranslateAnthropicJSONToGemini(raw, "p", DefaultLimits()); rerr == nil || rerr.Status != 502 {
			t.Fatalf("unmapped usage must fail closed: %s -> %v", usage, rerr)
		}
	}
	// Null billing members and a missing cache breakdown are absent.
	out := a2gemAnthropicJSON(t, `{"id":"m","type":"message","role":"assistant","model":"c",
		"content":[{"type":"text","text":"t"}],"stop_reason":"end_turn",
		"usage":{"input_tokens":2,"output_tokens":3,"server_tool_use":null,"service_tier":null}}`)
	top, _ := a2gemGeminiCandidate(t, out)
	usage := top["usageMetadata"].(map[string]any)
	if usage["promptTokenCount"] != float64(2) || usage["totalTokenCount"] != float64(5) {
		t.Fatalf("null billing members must be absent: %s", out)
	}
	if _, hasCached := usage["cachedContentTokenCount"]; hasCached {
		t.Fatalf("zero cached share must be omitted: %s", out)
	}
	// Overflowing totals fail rather than wrap.
	big := `{"id":"m","type":"message","role":"assistant","model":"c",
		"content":[{"type":"text","text":"t"}],"stop_reason":"end_turn",
		"usage":{"input_tokens":9223372036854775807,"output_tokens":1}}`
	if _, rerr := TranslateAnthropicJSONToGemini([]byte(big), "p", DefaultLimits()); rerr == nil || rerr.Status != 502 {
		t.Fatalf("overflowing usage must fail, got %v", rerr)
	}
	// Missing accounting fails.
	for _, usage := range []string{
		`"usage":{"output_tokens":1}`,
		`"usage":{"input_tokens":1}`,
	} {
		raw := []byte(`{"id":"m","type":"message","role":"assistant","model":"c",
			"content":[{"type":"text","text":"t"}],"stop_reason":"end_turn",` + usage + `}`)
		if _, rerr := TranslateAnthropicJSONToGemini(raw, "p", DefaultLimits()); rerr == nil || rerr.Status != 502 {
			t.Fatalf("missing accounting must fail: %s -> %v", usage, rerr)
		}
	}
}

func TestAnthropicJSONToGeminiBlockUnionClosed(t *testing.T) {
	// Cross-kind members are never ignored: text with tool input fails,
	// tool_use with text/citations fails, and unknown members fail.
	for _, content := range []string{
		`[{"type":"text","text":"x","input":{"a":1}}]`,
		`[{"type":"text","text":"x","id":"a"}]`,
		`[{"type":"tool_use","id":"toolu_1","name":"f","input":{},"text":"x"}]`,
		`[{"type":"tool_use","id":"toolu_1","name":"f","input":{},"citations":[]}]`,
		`[{"type":"text","text":"x","bogus":1}]`,
	} {
		raw := []byte(`{"id":"m","type":"message","role":"assistant","model":"c","content":` + content + `,"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
		if _, rerr := TranslateAnthropicJSONToGemini(raw, "p", DefaultLimits()); rerr == nil || rerr.Status != 502 {
			t.Fatalf("cross-kind block must fail closed: %s -> %v", content, rerr)
		}
	}
	// Complete tool_use requires a present input object: missing and null
	// never invent {}.
	for _, content := range []string{
		`[{"type":"tool_use","id":"toolu_1","name":"f"}]`,
		`[{"type":"tool_use","id":"toolu_1","name":"f","input":null}]`,
		`[{"type":"tool_use","id":"toolu_1","name":"f","input":[]}]`,
	} {
		raw := []byte(`{"id":"m","type":"message","role":"assistant","model":"c","content":` + content + `,"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
		if _, rerr := TranslateAnthropicJSONToGemini(raw, "p", DefaultLimits()); rerr == nil || rerr.Status != 502 {
			t.Fatalf("missing tool input must fail, never invent {}: %s -> %v", content, rerr)
		}
	}
}
