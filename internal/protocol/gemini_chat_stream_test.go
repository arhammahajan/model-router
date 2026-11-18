package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func g2cSSEData(payload string) string {
	return "data: " + payload + "\n\n"
}

func g2cChunk(delta, finish, usage string) string {
	choice := `{"index":0`
	if delta != "" {
		choice += `,"delta":` + delta
	}
	if finish != "" {
		choice += `,"finish_reason":` + finish
	}
	choice += `}`
	ch := `{"id":"chatcmpl-s","object":"chat.completion.chunk","created":1,"model":"up","choices":[` + choice + `]`
	if usage != "" {
		ch += `,"usage":` + usage
	}
	return g2cSSEData(ch + `}`)
}

func g2cUsageChunk() string {
	return g2cSSEData(`{"id":"chatcmpl-s","object":"chat.completion.chunk","created":1,"model":"up","choices":[],"usage":` + usageTriple(9, 4) + `}`)
}

func runGeminiSSE(t *testing.T, input string) (string, *RouterError) {
	t.Helper()
	var buf bytes.Buffer
	tr := &ChatToGeminiSSETranslator{PublicModel: "public-model", Limits: DefaultLimits()}
	rerr := tr.Translate(strings.NewReader(input), &buf, nil)
	return buf.String(), rerr
}

func runGeminiArray(t *testing.T, input string) (string, *RouterError) {
	t.Helper()
	var buf bytes.Buffer
	tr := &ChatToGeminiJSONArrayTranslator{PublicModel: "public-model", Limits: DefaultLimits()}
	rerr := tr.Translate(strings.NewReader(input), &buf, nil)
	return buf.String(), rerr
}

func geminiFrames(t *testing.T, sse string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, part := range strings.Split(sse, "data: ") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(part), &m); err != nil {
			t.Fatalf("downstream frame not JSON: %q", part)
		}
		out = append(out, m)
	}
	return out
}

func TestChatToGeminiStreamText(t *testing.T) {
	input := g2cChunk(`{"role":"assistant","content":"hel"}`, "", "") +
		g2cChunk(`{"content":"lo"}`, "", "") +
		g2cChunk(`{"role":"assistant"}`, `"stop"`, "") +
		g2cUsageChunk() +
		"data: [DONE]\n\n"
	out, rerr := runGeminiSSE(t, input)
	if rerr != nil {
		t.Fatalf("translate: %v", rerr.Msg)
	}
	if strings.Contains(out, "[DONE]") {
		t.Fatalf("Gemini SSE must not carry [DONE]: %q", out)
	}
	frames := geminiFrames(t, out)
	if len(frames) != 3 {
		t.Fatalf("expected 2 text + 1 terminal frames, got %d: %q", len(frames), out)
	}
	p0 := frames[0]["candidates"].([]any)[0].(map[string]any)["content"].(map[string]any)["parts"].([]any)
	if p0[0].(map[string]any)["text"] != "hel" {
		t.Fatalf("delta text not immediate: %q", out)
	}
	last := frames[2]
	c0 := last["candidates"].([]any)[0].(map[string]any)
	if c0["finishReason"] != "STOP" {
		t.Fatalf("terminal finish wrong: %v", c0)
	}
	if last["modelVersion"] != "public-model" || last["responseId"] != "chatcmpl-s" {
		t.Fatalf("identity wrong: %v", last)
	}
	u := last["usageMetadata"].(map[string]any)
	if u["promptTokenCount"] != float64(9) || u["totalTokenCount"] != float64(13) {
		t.Fatalf("terminal usage wrong: %v", u)
	}

	// Same upstream through the array translator is one valid JSON array
	// with identical terminal accounting and no whole-buffer replay.
	aout, rerr := runGeminiArray(t, input)
	if rerr != nil {
		t.Fatalf("array translate: %v", rerr.Msg)
	}
	if !strings.HasPrefix(aout, "[") || !strings.HasSuffix(aout, "]") {
		t.Fatalf("array framing wrong: %q", aout)
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(aout), &arr); err != nil || len(arr) != 3 {
		t.Fatalf("array not incremental-valid: %v %q", err, aout)
	}
	au := arr[2]["usageMetadata"].(map[string]any)
	if au["promptTokenCount"] != float64(9) {
		t.Fatalf("array terminal usage wrong: %v", au)
	}
}

func TestChatToGeminiStreamToolReconstruction(t *testing.T) {
	input := g2cChunk(`{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"a\":"}}]}`, "", "") +
		g2cChunk(`{"tool_calls":[{"index":0,"function":{"arguments":"9007199254740993}"}}]}`, "", "") +
		g2cChunk(`{}`, `"tool_calls"`, "") +
		g2cSSEData(`{"id":"chatcmpl-s","object":"chat.completion.chunk","created":1,"model":"up","choices":[],"usage":{"prompt_tokens":2,"completion_tokens":5,"total_tokens":7}}`) +
		"data: [DONE]\n\n"
	out, rerr := runGeminiSSE(t, input)
	if rerr != nil {
		t.Fatalf("translate: %v", rerr.Msg)
	}
	if !strings.Contains(out, "9007199254740993") {
		t.Fatalf("args not exact: %q", out)
	}
	// Chat tool blocks close only at [DONE]; the encoder releases the
	// complete call as its own frame immediately before the terminal
	// (allowed equivalent grouping). The call must appear exactly once
	// across the stream with authoritative terminal accounting.
	frames := geminiFrames(t, out)
	if len(frames) != 2 {
		t.Fatalf("want tool frame + terminal, got %d: %q", len(frames), out)
	}
	var calls []map[string]any
	for _, f := range frames {
		for _, p := range f["candidates"].([]any)[0].(map[string]any)["content"].(map[string]any)["parts"].([]any) {
			if fc, ok := p.(map[string]any)["functionCall"]; ok {
				calls = append(calls, fc.(map[string]any))
			}
		}
	}
	if len(calls) != 1 {
		t.Fatalf("call must appear exactly once, got %v in %q", calls, out)
	}
	fc := calls[0]
	if fc["name"] != "f" || fc["id"] != "call_1" {
		t.Fatalf("call identity wrong: %v", fc)
	}
	c0 := frames[1]["candidates"].([]any)[0].(map[string]any)
	if c0["finishReason"] != "STOP" {
		t.Fatalf("finish wrong: %v", c0)
	}
	if parts := c0["content"].(map[string]any)["parts"].([]any); len(parts) != 0 {
		t.Fatalf("terminal must not duplicate the released call: %q", out)
	}
}

func TestChatToGeminiStreamTerminalGates(t *testing.T) {
	text := g2cChunk(`{"content":"hi"}`, "", "")
	finish := g2cChunk(`{}`, `"stop"`, "")
	badFinish := g2cChunk(`{}`, `"quantum"`, "")
	// Missing finish / usage / [DONE] can never close successfully.
	for name, input := range map[string]string{
		"no finish":   text + g2cUsageChunk() + "data: [DONE]\n\n",
		"no usage":    text + finish + "data: [DONE]\n\n",
		"no done":     text + finish + g2cUsageChunk(),
		"truncated":   text + finish,
		"usage first": g2cUsageChunk() + text + finish + "data: [DONE]\n\n",
		"dup usage":   text + finish + g2cUsageChunk() + g2cUsageChunk() + "data: [DONE]\n\n",
		"dup finish":  text + finish + finish + g2cUsageChunk() + "data: [DONE]\n\n",
		"bad finish":  text + badFinish + g2cUsageChunk() + "data: [DONE]\n\n",
		"after data":  text + finish + g2cUsageChunk() + g2cChunk(`{"content":"late"}`, "", "") + "data: [DONE]\n\n",
		"mid error":   text + g2cSSEData(`{"error":{"message":"boom"}}`),
	} {
		if _, rerr := runGeminiSSE(t, input); rerr == nil || rerr.Status != 502 {
			t.Fatalf("%s: expected 502, got %v", name, rerr)
		}
		if out, _ := runGeminiArray(t, input); strings.HasSuffix(out, "]") {
			t.Fatalf("%s: failed array must not close: %q", name, out)
		}
	}
	// Malformed args at finish become the dialect failure marker, still
	// with usage and no replay.
	input := g2cChunk(`{"tool_calls":[{"index":0,"id":"a","function":{"name":"f","arguments":"[oops"}}]}`, "", "") +
		g2cChunk(`{}`, `"stop"`, "") + g2cUsageChunk() + "data: [DONE]\n\n"
	out, rerr := runGeminiSSE(t, input)
	if rerr != nil {
		t.Fatalf("malformed args must map, not fail: %v", rerr.Msg)
	}
	frames := geminiFrames(t, out)
	c0 := frames[len(frames)-1]["candidates"].([]any)[0].(map[string]any)
	if c0["finishReason"] != "MALFORMED_FUNCTION_CALL" {
		t.Fatalf("expected MALFORMED marker: %v", c0)
	}
}

func TestChatToGeminiStreamBoundsAndIdentities(t *testing.T) {
	// One oversize argument fragment fails closed.
	big := strings.Repeat("x", DefaultLimits().MaxArgsChars+1)
	in := g2cChunk(`{"tool_calls":[{"index":0,"id":"a","function":{"name":"f","arguments":"`+big[:20]+`"}}]}`, "", "")
	_ = in
	frag := g2cSSEData(`{"id":"chatcmpl-s","object":"chat.completion.chunk","created":1,"model":"up","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"f","arguments":"` + strings.Repeat("y", 100) + `"}}]}}]}`)
	if _, rerr := runGeminiSSE(t, frag); rerr == nil || !strings.Contains(rerr.Msg, "without completion") {
		t.Fatalf("lone fragment must end as truncation, got %v", rerr)
	}
	// An oversize single fragment fails on the size bound, not truncation.
	huge := g2cSSEData(`{"id":"chatcmpl-s","object":"chat.completion.chunk","created":1,"model":"up","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"f","arguments":"` + strings.Repeat("z", 100) + `"}}]}}]}`)
	_ = huge
	bigfrag := `{"index":0,"id":"a","function":{"name":"f","arguments":"` + strings.Repeat("q", DefaultLimits().MaxArgsChars) + `"}}`
	if _, rerr := runGeminiSSE(t, g2cChunk(`{"tool_calls":[`+bigfrag+`]}`, "", "")); rerr == nil || rerr.Status != 502 {
		t.Fatalf("expected args bound, got %v", rerr)
	}
	// 1025 distinct tool slots exceed the call bound.
	var sb strings.Builder
	sb.WriteString(g2cChunk(`{"tool_calls":[{"index":0,"id":"c0","function":{"name":"f","arguments":""}}]}`, "", ""))
	for i := 1; i < 1025; i++ {
		sb.WriteString(g2cSSEData(`{"id":"x","object":"chat.completion.chunk","created":1,"model":"up","choices":[{"index":0,"delta":{"tool_calls":[{"index":` + g2cItoa(i) + `,"id":"c` + g2cItoa(i) + `","function":{"name":"f"}}]}}]}`))
	}
	if _, rerr := runGeminiSSE(t, sb.String()); rerr == nil || rerr.Status != 502 {
		t.Fatalf("expected tool-count bound, got %v", rerr)
	}
	// Conflicting response identities fail closed.
	conflict := g2cSSEData(`{"id":"one","object":"chat.completion.chunk","created":1,"model":"up","choices":[{"index":0,"delta":{"content":"a"}}]}`) +
		g2cSSEData(`{"id":"two","object":"chat.completion.chunk","created":1,"model":"up","choices":[{"index":0,"delta":{"content":"b"}}]}`)
	if _, rerr := runGeminiSSE(t, conflict); rerr == nil || rerr.Status != 502 {
		t.Fatalf("expected identity conflict, got %v", rerr)
	}
	// Unfinished tool identity at finish fails closed.
	unfin := g2cChunk(`{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}`, "", "") +
		g2cChunk(`{}`, `"stop"`, "") + g2cUsageChunk() + "data: [DONE]\n\n"
	if _, rerr := runGeminiSSE(t, unfin); rerr == nil || rerr.Status != 502 {
		t.Fatalf("expected unfinished identity failure, got %v", rerr)
	}
	// Unrepresentable deltas fail closed.
	for _, d := range []string{
		`{"refusal":"no"}`, `{"audio":{}}`, `{"function_call":{"name":"f"}}`,
	} {
		in := g2cChunk(d, "", "") + g2cChunk(`{}`, `"stop"`, "") + g2cUsageChunk() + "data: [DONE]\n\n"
		if _, rerr := runGeminiSSE(t, in); rerr == nil || rerr.Status != 502 {
			t.Fatalf("delta %s: expected 502, got %v", d, rerr)
		}
	}
}

type geminiFailWriter struct{ n int }

func (w *geminiFailWriter) Write(p []byte) (int, error) {
	w.n++
	return 0, errors.New("downstream closed")
}

func TestChatToGeminiStreamWriterFailure(t *testing.T) {
	input := g2cChunk(`{"content":"hi"}`, "", "") +
		g2cChunk(`{}`, `"stop"`, "") + g2cUsageChunk() + "data: [DONE]\n\n"
	tr := &ChatToGeminiSSETranslator{PublicModel: "p", Limits: DefaultLimits()}
	if rerr := tr.Translate(strings.NewReader(input), &geminiFailWriter{}, nil); rerr == nil || rerr.Status != 502 {
		t.Fatalf("expected write failure 502, got %v", rerr)
	}
	atr := &ChatToGeminiJSONArrayTranslator{PublicModel: "p", Limits: DefaultLimits()}
	if rerr := atr.Translate(strings.NewReader(input), &geminiFailWriter{}, nil); rerr == nil || rerr.Status != 502 {
		t.Fatalf("expected array write failure 502, got %v", rerr)
	}
}

func TestChatToGeminiStreamNoEOFRequired(t *testing.T) {
	// Bytes after a valid [DONE] belong to the next generation: return
	// without requiring EOF and leave them unread.
	input := g2cChunk(`{"content":"hi"}`, "", "") +
		g2cChunk(`{}`, `"stop"`, "") + g2cUsageChunk() + "data: [DONE]\n\n" +
		g2cChunk(`{"content":"next generation"}`, "", "")
	r := strings.NewReader(input)
	tr := &ChatToGeminiSSETranslator{PublicModel: "p", Limits: DefaultLimits()}
	out := &bytes.Buffer{}
	if rerr := tr.Translate(r, out, nil); rerr != nil {
		t.Fatalf("translate: %v", rerr.Msg)
	}
	if strings.Contains(out.String(), "next generation") {
		t.Fatalf("translator read past [DONE]: %q", out.String())
	}
	// Empty generation still closes with finish + usage.
	empty := g2cChunk(`{}`, `"stop"`, "") + g2cUsageChunk() + "data: [DONE]\n\n"
	eout, rerr := runGeminiSSE(t, empty)
	if rerr != nil {
		t.Fatalf("empty stream: %v", rerr.Msg)
	}
	eframes := geminiFrames(t, eout)
	ec0 := eframes[len(eframes)-1]["candidates"].([]any)[0].(map[string]any)
	if ec0["finishReason"] != "STOP" {
		t.Fatalf("empty terminal wrong: %v", ec0)
	}
	// Usage overflow in-stream fails closed, never underreports.
	over := g2cChunk(`{"content":"hi"}`, "", "") + g2cChunk(`{}`, `"stop"`, "") +
		g2cSSEData(`{"id":"s","object":"chat.completion.chunk","created":1,"model":"up","choices":[],"usage":{"prompt_tokens":2,"completion_tokens":2,"total_tokens":99}}`) +
		"data: [DONE]\n\n"
	if _, rerr := runGeminiSSE(t, over); rerr == nil || rerr.Status != 502 {
		t.Fatalf("expected usage mismatch 502, got %v", rerr)
	}
}
