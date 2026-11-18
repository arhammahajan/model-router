package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func g2cOrderedInput(order []int) string {
	var sb strings.Builder
	for _, idx := range order {
		sb.WriteString(g2cChunk(fmt.Sprintf(
			`{"tool_calls":[{"index":%d,"id":"call_%d","function":{"name":"fn_%d","arguments":"{}"}}]}`,
			idx, idx, idx), "", ""))
	}
	sb.WriteString(g2cChunk(`{}`, `"tool_calls"`, ""))
	sb.WriteString(g2cUsageChunk())
	sb.WriteString("data: [DONE]\n\n")
	return sb.String()
}

func g2cTerminalNames(t *testing.T, sse string) []string {
	t.Helper()
	frames := geminiFrames(t, sse)
	last := frames[len(frames)-1]
	c0 := last["candidates"].([]any)[0].(map[string]any)
	parts, _ := c0["content"].(map[string]any)["parts"].([]any)
	var out []string
	for _, p := range parts {
		fc, _ := p.(map[string]any)["functionCall"].(map[string]any)
		out = append(out, fc["name"].(string))
	}
	return out
}

func TestGeminiStreamCallOrderStable(t *testing.T) {
	// Sparse, out-of-order arrivals render in ascending source index.
	input := g2cOrderedInput([]int{7, 3, 5, 0, 2, 6, 1, 4})
	seen := map[string]bool{}
	for i := 0; i < 30; i++ {
		out, rerr := runGeminiSSE(t, input)
		if rerr != nil {
			t.Fatalf("translate: %v", rerr.Msg)
		}
		data, _ := json.Marshal(g2cTerminalNames(t, out))
		seen[string(data)] = true
	}
	if len(seen) != 1 {
		t.Fatalf("nondeterministic order: %v", seen)
	}
	for k := range seen {
		var names []string
		if err := json.Unmarshal([]byte(k), &names); err != nil {
			t.Fatal(err)
		}
		for i, n := range names {
			if n != fmt.Sprintf("fn_%d", i) {
				t.Fatalf("wrong order: %v", names)
			}
		}
	}
	// Array path uses the same stable order.
	aout, rerr := runGeminiArray(t, input)
	if rerr != nil {
		t.Fatalf("array: %v", rerr.Msg)
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(aout), &arr); err != nil {
		t.Fatal(err)
	}
	last := arr[len(arr)-1]
	c0 := last["candidates"].([]any)[0].(map[string]any)
	parts := c0["content"].(map[string]any)["parts"].([]any)
	for i, p := range parts {
		fc := p.(map[string]any)["functionCall"].(map[string]any)
		if fc["name"] != fmt.Sprintf("fn_%d", i) {
			t.Fatalf("array order wrong: %v", parts)
		}
	}
}

func TestGeminiStreamDownstreamBound(t *testing.T) {
	// Tools release as individual frames, so the per-frame bound is
	// pinned with a single call whose early frame alone exceeds
	// MaxFrameBytes (not a batched terminal).
	lim := DefaultLimits()
	lim.MaxFrameBytes = 512
	frag := strings.ReplaceAll(`{"k":"`+strings.Repeat("v", 600)+`"}`, `"`, `\"`)
	var sb strings.Builder
	sb.WriteString(g2cChunk(fmt.Sprintf(
		`{"tool_calls":[{"index":0,"id":"call_0","function":{"name":"fn_0","arguments":"%s"}}]}`,
		frag), "", ""))
	sb.WriteString(g2cChunk(`{}`, `"tool_calls"`, ""))
	sb.WriteString(g2cSSEData(`{"id":"chatcmpl-s","object":"chat.completion.chunk","created":1,"model":"up","choices":[],"usage":{"prompt_tokens":2,"completion_tokens":5,"total_tokens":7}}`))
	sb.WriteString("data: [DONE]\n\n")
	var buf bytes.Buffer
	tr := &ChatToGeminiSSETranslator{PublicModel: "p", Limits: lim}
	if rerr := tr.Translate(strings.NewReader(sb.String()), &buf, nil); rerr == nil || rerr.Status != 502 {
		t.Fatalf("expected downstream bound 502, got %v", rerr)
	}
	var buf2 bytes.Buffer
	tr2 := &ChatToGeminiJSONArrayTranslator{PublicModel: "p", Limits: lim}
	if rerr := tr2.Translate(strings.NewReader(sb.String()), &buf2, nil); rerr == nil || rerr.Status != 502 {
		t.Fatalf("expected array bound 502, got %v", rerr)
	}
	if strings.HasSuffix(strings.TrimSpace(buf2.String()), "]") {
		t.Fatalf("failed array must not close: %q", buf2.String())
	}
	// Oversize arguments fail as bounds, never as a native malformed call.
	big := strings.Repeat("a", DefaultLimits().MaxArgsChars+1)
	raw := chatEnvelope(
		chatChoice(`"tool_calls"`, `{"role":"assistant","content":null,"tool_calls":[
			{"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"k\":\"`+big+`\"}"}}]}`),
		usageTriple(1, 1))
	if _, rerr := TranslateChatJSONToGemini([]byte(raw), "p", DefaultLimits()); rerr == nil || rerr.Status != 502 {
		t.Fatalf("expected args bound 502, got %v", rerr)
	}
	m := mustGeminiJSON(t, chatEnvelope(
		chatChoice(`"stop"`, `{"role":"assistant","content":null,"tool_calls":[
			{"id":"call_1","type":"function","function":{"name":"f","arguments":"[1,2]"}}]}`),
		usageTriple(1, 1)))
	c0 := m["candidates"].([]any)[0].(map[string]any)
	if c0["finishReason"] != "MALFORMED_FUNCTION_CALL" {
		t.Fatalf("genuine malformed call not mapped: %v", c0)
	}
}
