package protocol

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// RED: Anthropic upstream SSE translated to Gemini downstream framing.

func a2gemSseInput(lines ...string) string {
	return strings.Join(lines, "\n") + "\n"
}

func a2gemStartEvent() string {
	return `event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-x","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":1}}}` + "\n"
}

func a2gemTranslateSSE(t *testing.T, input string) string {
	t.Helper()
	tr := &AnthropicToGeminiSSETranslator{PublicModel: "public-model", Limits: DefaultLimits()}
	var buf bytes.Buffer
	if rerr := tr.Translate(strings.NewReader(input), &buf, nil); rerr != nil {
		t.Fatalf("translate: %v", rerr)
	}
	return buf.String()
}

func a2gemSseFrames(t *testing.T, out string) []map[string]any {
	t.Helper()
	var frames []map[string]any
	for _, chunk := range strings.Split(out, "data: ") {
		chunk = strings.TrimSpace(chunk)
		if chunk == "" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(chunk), &obj); err != nil {
			t.Fatalf("frame must be JSON: %q: %v", chunk, err)
		}
		frames = append(frames, obj)
	}
	return frames
}

// Text streams immediately; the terminal carries finish + usage only.
func TestAnthropicSSEToGeminiText(t *testing.T) {
	in := a2gemSseInput(
		a2gemStartEvent(),
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":7}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	)
	out := a2gemTranslateSSE(t, in)
	if strings.Contains(out, "[DONE]") {
		t.Fatalf("Gemini SSE must not use DONE: %q", out)
	}
	frames := a2gemSseFrames(t, out)
	if len(frames) != 3 {
		t.Fatalf("two text frames plus terminal required, got %d: %q", len(frames), out)
	}
	first := frames[0]["candidates"].([]any)[0].(map[string]any)
	parts := first["content"].(map[string]any)["parts"].([]any)
	if len(parts) != 1 || parts[0].(map[string]any)["text"] != "Hel" {
		t.Fatalf("first text must flush immediately: %q", out)
	}
	if _, hasFinish := first["finishReason"]; hasFinish {
		t.Fatalf("non-terminal frames carry no finish: %q", out)
	}
	last := frames[2]
	cand := last["candidates"].([]any)[0].(map[string]any)
	if cand["finishReason"] != "STOP" {
		t.Fatalf("terminal must carry STOP: %q", out)
	}
	if len(cand["content"].(map[string]any)["parts"].([]any)) != 0 {
		t.Fatalf("terminal must not duplicate text: %q", out)
	}
	usage := last["usageMetadata"].(map[string]any)
	if usage["promptTokenCount"] != float64(10) || usage["candidatesTokenCount"] != float64(7) ||
		usage["totalTokenCount"] != float64(17) {
		t.Fatalf("terminal usage must be authoritative: %q", out)
	}
	if last["modelVersion"] != "public-model" || last["responseId"] != "msg_1" {
		t.Fatalf("terminal must stamp model and response identity: %q", out)
	}
}

// A tool_use emits exactly once, complete, at block stop.
func TestAnthropicSSEToGeminiTool(t *testing.T) {
	in := a2gemSseInput(
		a2gemStartEvent(),
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"loc\":"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"1,\"unit\":\"c\"}"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":20}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	)
	frames := a2gemSseFrames(t, a2gemTranslateSSE(t, in))
	if len(frames) != 2 {
		t.Fatalf("one tool frame plus terminal required, got %d", len(frames))
	}
	call := frames[0]["candidates"].([]any)[0].(map[string]any)["content"].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionCall"].(map[string]any)
	if call["id"] != "toolu_1" || call["name"] != "get_weather" {
		t.Fatalf("complete call identity required: %v", call)
	}
	args, _ := json.Marshal(call["args"])
	var decoded map[string]any
	if err := json.Unmarshal(args, &decoded); err != nil || decoded["unit"] != "c" || decoded["loc"] != float64(1) {
		t.Fatalf("complete args required once: %v", call["args"])
	}
	last := frames[1]["candidates"].([]any)[0].(map[string]any)
	if last["finishReason"] != "STOP" || len(last["content"].(map[string]any)["parts"].([]any)) != 0 {
		t.Fatalf("terminal carries finish only: %v", last)
	}
}

// Parallel complete calls release by logical index.
func TestAnthropicSSEToGeminiParallelOrder(t *testing.T) {
	in := a2gemSseInput(
		a2gemStartEvent(),
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_0","name":"a","input":{}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"b","input":{}}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":1}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":3}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	)
	frames := a2gemSseFrames(t, a2gemTranslateSSE(t, in))
	if len(frames) != 3 {
		t.Fatalf("two ordered tool frames plus terminal required, got %d", len(frames))
	}
	first := frames[0]["candidates"].([]any)[0].(map[string]any)["content"].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionCall"].(map[string]any)
	second := frames[1]["candidates"].([]any)[0].(map[string]any)["content"].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionCall"].(map[string]any)
	if first["id"] != "toolu_0" || second["id"] != "toolu_1" {
		t.Fatalf("calls must release by logical index: %v %v", first["id"], second["id"])
	}
}

// A block that arrives whole (seeded args, no deltas) is preserved.
func TestAnthropicSSEToGeminiSeededTool(t *testing.T) {
	in := a2gemSseInput(
		a2gemStartEvent(),
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_9","name":"calc","input":{"n":9007199254740993}}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":9}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	)
	out := a2gemTranslateSSE(t, in)
	if !strings.Contains(out, "9007199254740993") {
		t.Fatalf("seeded args must survive raw: %q", out)
	}
	frames := a2gemSseFrames(t, out)
	if len(frames) != 2 {
		t.Fatalf("one tool frame plus terminal required, got %d: %q", len(frames), out)
	}
}

// Later text while an earlier call is incomplete fails closed.
func TestAnthropicSSEToGeminiReorderFails(t *testing.T) {
	in := a2gemSseInput(
		a2gemStartEvent(),
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_0","name":"a","input":{}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":"late"}}`,
		``,
	)
	tr := &AnthropicToGeminiSSETranslator{PublicModel: "p", Limits: DefaultLimits()}
	var buf bytes.Buffer
	if rerr := tr.Translate(strings.NewReader(in), &buf, nil); rerr == nil || rerr.Status != 502 {
		t.Fatalf("later text before an incomplete call must fail closed, got %v", rerr)
	}
}

// Strict event/type agreement and mandatory terminal accounting.
func TestAnthropicSSEToGeminiStrictness(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
	}{
		{"data only frame", []string{"data: {}", ""}},
		{"unknown event", []string{"event: bogus", `data: {"type":"bogus"}`, ""}},
		{"thinking delta", []string{
			a2gemStartEvent(),
			`event: content_block_start`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
			``,
		}},
		{"duplicate start", []string{a2gemStartEvent(), a2gemStartEvent()}},
		{"delta unknown block", []string{
			a2gemStartEvent(),
			`event: content_block_delta`,
			`data: {"type":"content_block_delta","index":3,"delta":{"type":"text_delta","text":"x"}}`,
			``,
		}},
		{"truncated", []string{a2gemStartEvent()}},
		{"stop without accounting", []string{
			a2gemStartEvent(),
			`event: message_stop`,
			`data: {"type":"message_stop"}`,
			``,
		}},
		{"refusal stop", []string{
			a2gemStartEvent(),
			`event: message_delta`,
			`data: {"type":"message_delta","delta":{"stop_reason":"refusal","stop_sequence":null},"usage":{"output_tokens":1}}`,
			``,
			`event: message_stop`,
			`data: {"type":"message_stop"}`,
			``,
		}},
		{"unfinished block at stop", []string{
			a2gemStartEvent(),
			`event: content_block_start`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"x"}}`,
			``,
			`event: message_delta`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`,
			``,
			`event: message_stop`,
			`data: {"type":"message_stop"}`,
			``,
		}},
		{"error event", []string{
			a2gemStartEvent(),
			`event: error`,
			`data: {"type":"error","error":{"type":"overloaded_error","message":"busy"}}`,
			``,
		}},
	}
	for _, tc := range cases {
		tr := &AnthropicToGeminiSSETranslator{PublicModel: "p", Limits: DefaultLimits()}
		var buf bytes.Buffer
		if rerr := tr.Translate(strings.NewReader(a2gemSseInput(tc.lines...)), &buf, nil); rerr == nil || rerr.Status != 502 {
			t.Fatalf("%s must fail closed, got %v", tc.name, rerr)
		}
	}
}

// The JSON-array framer writes one incremental array; failures after the
// first frame leave it unclosed, and usage rides on the terminal object.
func TestAnthropicJSONArrayToGemini(t *testing.T) {
	in := a2gemSseInput(
		a2gemStartEvent(),
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"hi"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens","stop_sequence":null},"usage":{"output_tokens":4}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	)
	tr := &AnthropicToGeminiJSONArrayTranslator{PublicModel: "p", Limits: DefaultLimits()}
	var buf bytes.Buffer
	if rerr := tr.Translate(strings.NewReader(in), &buf, nil); rerr != nil {
		t.Fatalf("translate: %v", rerr)
	}
	out := strings.TrimSpace(buf.String())
	if !strings.HasPrefix(out, "[") || !strings.HasSuffix(out, "]") {
		t.Fatalf("array must open and close: %q", out)
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(out), &arr); err != nil || len(arr) != 2 {
		t.Fatalf("two incremental objects required: %q: %v", out, err)
	}
	last := arr[1]["candidates"].([]any)[0].(map[string]any)
	if last["finishReason"] != "MAX_TOKENS" {
		t.Fatalf("terminal finish must map: %q", out)
	}
	if _, ok := arr[1]["usageMetadata"]; !ok {
		t.Fatalf("terminal usage required: %q", out)
	}

	// Mid-array failure leaves the array unclosed and carries no success.
	bad := a2gemSseInput(
		a2gemStartEvent(),
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"hi"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"nope"}}`,
		``,
	)
	tr2 := &AnthropicToGeminiJSONArrayTranslator{PublicModel: "p", Limits: DefaultLimits()}
	var buf2 bytes.Buffer
	if rerr := tr2.Translate(strings.NewReader(bad), &buf2, nil); rerr == nil || rerr.Status != 502 {
		t.Fatalf("mid-array failure must be 502, got %v", rerr)
	}
	partial := strings.TrimSpace(buf2.String())
	if !strings.HasPrefix(partial, "[") || strings.HasSuffix(partial, "]") {
		t.Fatalf("failed array must stay unclosed: %q", partial)
	}
}

func a2gemTranslateFails(t *testing.T, input string) (string, *RouterError) {
	t.Helper()
	tr := &AnthropicToGeminiSSETranslator{PublicModel: "p", Limits: DefaultLimits()}
	var buf bytes.Buffer
	rerr := tr.Translate(strings.NewReader(input), &buf, nil)
	if rerr == nil {
		t.Fatalf("expected 502, got success: %q", buf.String())
	}
	if rerr.Status != 502 {
		t.Fatalf("expected 502, got %v", rerr)
	}
	return buf.String(), rerr
}

func TestAnthropicSSEToGeminiPingValidation(t *testing.T) {
	base := func(extra string) string {
		return a2gemSseInput(
			a2gemStartEvent(),
			`event: content_block_start`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"hi"}}`,
			``,
			extra,
			`event: content_block_stop`,
			`data: {"type":"content_block_stop","index":0}`,
			``,
			`event: message_delta`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`,
			``,
			`event: message_stop`,
			`data: {"type":"message_stop"}`,
			``,
		)
	}
	// Empty keepalive and strict ping object pass.
	for _, extra := range []string{
		"event: ping\n" + `data: ` + "\n\n",
		"event: ping\n" + `data: {"type":"ping"}` + "\n\n",
	} {
		tr := &AnthropicToGeminiSSETranslator{PublicModel: "p", Limits: DefaultLimits()}
		var buf bytes.Buffer
		if rerr := tr.Translate(strings.NewReader(base(extra)), &buf, nil); rerr != nil {
			t.Fatalf("valid ping must pass: %v", rerr)
		}
	}
	// Mismatched semantic payload, malformed JSON, non-object and unknown
	// members fail closed.
	for _, payload := range []string{
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"s"}}`,
		`{bad`,
		`[]`,
		`{"type":"pong"}`,
		`{"type":"ping","extra":1}`,
	} {
		out, _ := a2gemTranslateFails(t, base("event: ping\n"+`data: `+payload+"\n\n"))
		if strings.Contains(out, `"thinking":"s"`) {
			t.Fatalf("ping payload must not leak semantics: %q", out)
		}
	}
}

func TestAnthropicSSEToGeminiInterleavedTextFails(t *testing.T) {
	in := a2gemSseInput(
		a2gemStartEvent(),
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"a"}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":"B"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"c"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":1}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	)
	out, _ := a2gemTranslateFails(t, in)
	if strings.Contains(out, `"text":"B"`) {
		t.Fatalf("higher text must not emit before refusal: %q", out)
	}
}

func TestAnthropicSSEToGeminiTextBehindPendingTool(t *testing.T) {
	in := a2gemSseInput(
		a2gemStartEvent(),
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"f","input":{}}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":1}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"text","text":"late"}}`,
		``,
	)
	out, _ := a2gemTranslateFails(t, in)
	if strings.Contains(out, `"text":"late"`) {
		t.Fatalf("text behind unfinished lower blocks must not emit: %q", out)
	}
}

func TestAnthropicSSEToGeminiLateIndexRegression(t *testing.T) {
	in := a2gemSseInput(
		a2gemStartEvent(),
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":"B"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":1}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"a"}}`,
		``,
	)
	a2gemTranslateFails(t, in)
}

func TestAnthropicSSEToGeminiEmptyArgsAggregate(t *testing.T) {
	in := a2gemSseInput(
		a2gemStartEvent(),
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call1","name":"f","input":{}}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":1}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	)
	lim := DefaultLimits()
	lim.MaxStreamArgsChars = 1
	tr := &AnthropicToGeminiSSETranslator{PublicModel: "p", Limits: lim}
	var buf bytes.Buffer
	if rerr := tr.Translate(strings.NewReader(in), &buf, nil); rerr == nil || rerr.Status != 502 {
		t.Fatalf("empty args fallback (2 bytes) must exceed aggregate 1, got %v", rerr)
	}
}

func TestAnthropicSSEToGeminiReuseResets(t *testing.T) {
	tr := &AnthropicToGeminiSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
	mk := func(id string) string {
		return a2gemSseInput(
			"event: message_start",
			`data: {"type":"message_start","message":{"id":"`+id+`","type":"message","role":"assistant","model":"c","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}}`,
			``,
			`event: content_block_start`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"hi"}}`,
			``,
			`event: content_block_stop`,
			`data: {"type":"content_block_stop","index":0}`,
			``,
			`event: message_delta`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`,
			``,
			`event: message_stop`,
			`data: {"type":"message_stop"}`,
			``,
		)
	}
	for i, id := range []string{"m1", "m2"} {
		var out bytes.Buffer
		if rerr := tr.Translate(strings.NewReader(mk(id)), &out, nil); rerr != nil {
			t.Fatalf("generation %d rejected: %v", i, rerr)
		}
		if !strings.Contains(out.String(), id) {
			t.Fatalf("generation %d must carry its own id: %q", i, out.String())
		}
	}
}
