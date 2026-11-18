package protocol

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// Gated regression proofs for the shared Gemini target encoder.
//
// These tests drive the public Anthropic->Gemini translators (SSE and
// incremental JSON array) and prove the ordering contract the held
// cutover exposed: complete calls release at content_block_stop before
// the generation terminal, never in the terminal; parallel closes
// release in logical SourceIndex order; later text crossing earlier
// unfinished/pending content refuses before the wrong bytes emit; late
// logical-index regression refuses; malformed calls suppress exactly
// one call. The gated reader proves early emission happened before the
// upstream terminal was even read.

type geminiGatedReader struct {
	data []byte
	pos  int
	gate int
	out  *bytes.Buffer
	want string

	checked bool
	failed  bool
}

func (r *geminiGatedReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	if !r.checked && r.pos >= r.gate {
		r.checked = true
		if !strings.Contains(r.out.String(), r.want) {
			r.failed = true
		}
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = r.data[r.pos]
	r.pos++
	return 1, nil
}

func geminiGatedTranslate(t *testing.T, input, wantBeforeTerminal string, isArray bool) (string, *RouterError) {
	t.Helper()
	gate := strings.Index(input, "event: message_delta")
	if gate < 0 {
		t.Fatalf("test input must carry a message_delta terminal: %q", input)
	}
	var out bytes.Buffer
	gr := &geminiGatedReader{data: []byte(input), gate: gate, out: &out, want: wantBeforeTerminal}
	var rerr *RouterError
	if isArray {
		tr := &AnthropicToGeminiJSONArrayTranslator{PublicModel: "public-model", Limits: DefaultLimits()}
		rerr = tr.Translate(gr, &out, nil)
	} else {
		tr := &AnthropicToGeminiSSETranslator{PublicModel: "public-model", Limits: DefaultLimits()}
		rerr = tr.Translate(gr, &out, nil)
	}
	if rerr == nil && gr.failed {
		t.Fatalf("array=%v tool frame missing before terminal read: %q", isArray, out.String())
	}
	if rerr == nil && !gr.checked {
		t.Fatalf("array=%v gate never reached (terminal unread?): %q", isArray, out.String())
	}
	return out.String(), rerr
}

func geminiAllCalls(t *testing.T, out string, isArray bool) []map[string]any {
	t.Helper()
	var calls []map[string]any
	collect := func(payload string) {
		var m map[string]any
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			t.Fatalf("downstream frame not JSON: %v in %q", err, out)
		}
		cands, _ := m["candidates"].([]any)
		if len(cands) != 1 {
			t.Fatalf("want one candidate: %q", out)
		}
		for _, p := range cands[0].(map[string]any)["content"].(map[string]any)["parts"].([]any) {
			if fc, ok := p.(map[string]any)["functionCall"]; ok {
				calls = append(calls, fc.(map[string]any))
			}
		}
	}
	if isArray {
		var arr []json.RawMessage
		if err := json.Unmarshal([]byte(out), &arr); err != nil {
			t.Fatalf("array not JSON: %v in %q", err, out)
		}
		for _, m := range arr {
			collect(string(m))
		}
		return calls
	}
	for _, part := range strings.Split(out, "data: ") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		collect(part)
	}
	return calls
}

func geminiTerminals(t *testing.T, out string, isArray bool) []map[string]any {
	t.Helper()
	var terms []map[string]any
	collect := func(payload string) {
		var m map[string]any
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			t.Fatalf("frame not JSON: %v", err)
		}
		c := m["candidates"].([]any)[0].(map[string]any)
		if _, ok := c["finishReason"]; ok {
			terms = append(terms, c)
		}
	}
	if isArray {
		var arr []json.RawMessage
		if err := json.Unmarshal([]byte(out), &arr); err != nil {
			t.Fatalf("array not JSON: %v", err)
		}
		for _, m := range arr {
			collect(string(m))
		}
		return terms
	}
	for _, part := range strings.Split(out, "data: ") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		collect(part)
	}
	return terms
}

// First completed tool emits before the upstream terminal is read.
func TestGeminiGatedEarlyToolBeforeTerminal(t *testing.T) {
	in := a2gemSseInput(
		a2gemStartEvent(),
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"loc\":1}"}}`,
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
	for _, isArray := range []bool{false, true} {
		out, rerr := geminiGatedTranslate(t, in, `"get_weather"`, isArray)
		if rerr != nil {
			t.Fatalf("array=%v translate: %v", isArray, rerr.Msg)
		}
		calls := geminiAllCalls(t, out, isArray)
		if len(calls) != 1 || calls[0]["id"] != "toolu_1" {
			t.Fatalf("array=%v want one early call exactly once, got %v in %q", isArray, calls, out)
		}
		terms := geminiTerminals(t, out, isArray)
		if len(terms) != 1 || terms[0]["finishReason"] != "STOP" {
			t.Fatalf("array=%v terminal wrong: %v in %q", isArray, terms, out)
		}
		if parts := terms[0]["content"].(map[string]any)["parts"].([]any); len(parts) != 0 {
			t.Fatalf("array=%v terminal must not repeat the released call: %q", isArray, out)
		}
	}
}

// A call held behind earlier open text releases when that text closes.
func TestGeminiGatedBarrierReleaseOnTextClose(t *testing.T) {
	in := a2gemSseInput(
		a2gemStartEvent(),
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"a"}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"f","input":{}}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":1}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":2}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	)
	for _, isArray := range []bool{false, true} {
		out, rerr := geminiGatedTranslate(t, in, `"functionCall"`, isArray)
		if rerr != nil {
			t.Fatalf("array=%v translate: %v", isArray, rerr.Msg)
		}
		if !strings.Contains(out, `"text":"a"`) {
			t.Fatalf("array=%v text lost: %q", isArray, out)
		}
		textPos, toolPos := strings.Index(out, `"text":"a"`), strings.Index(out, `"functionCall"`)
		if textPos < 0 || toolPos < 0 || textPos > toolPos {
			t.Fatalf("array=%v text must precede the released call: %q", isArray, out)
		}
		calls := geminiAllCalls(t, out, isArray)
		if len(calls) != 1 {
			t.Fatalf("array=%v held call must release exactly once on text close, got %v in %q", isArray, calls, out)
		}
	}
}

func TestGeminiGatedEmptyTextReleasesTool(t *testing.T) {
	in := a2gemSseInput(
		a2gemStartEvent(),
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"f","input":{}}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":1}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":2}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	)
	for _, isArray := range []bool{false, true} {
		out, rerr := geminiGatedTranslate(t, in, `"functionCall"`, isArray)
		if rerr != nil {
			t.Fatalf("array=%v translate: %v", isArray, rerr.Msg)
		}
		if calls := geminiAllCalls(t, out, isArray); len(calls) != 1 {
			t.Fatalf("array=%v want exactly one call, got %d", isArray, len(calls))
		}
	}
}

// Parallel closes out of order release in logical SourceIndex order.
func TestGeminiGatedParallelOutOfOrder(t *testing.T) {
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
	for _, isArray := range []bool{false, true} {
		out, rerr := geminiGatedTranslate(t, in, `"toolu_0"`, isArray)
		if rerr != nil {
			t.Fatalf("array=%v translate: %v", isArray, rerr.Msg)
		}
		calls := geminiAllCalls(t, out, isArray)
		if len(calls) != 2 || calls[0]["id"] != "toolu_0" || calls[1]["id"] != "toolu_1" {
			t.Fatalf("array=%v logical order violated: %v in %q", isArray, calls, out)
		}
	}
}

// Unsupported interleavings refuse before the wrong content emits.
func TestGeminiGatedInterleavingRefusals(t *testing.T) {
	interleaved := a2gemSseInput(
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
	)
	behind := a2gemSseInput(
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
	late := a2gemSseInput(
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
	for _, isArray := range []bool{false, true} {
		for name, in := range map[string]string{"interleaved": interleaved, "behind": behind} {
			var out string
			var rerr *RouterError
			var buf bytes.Buffer
			if isArray {
				tr := &AnthropicToGeminiJSONArrayTranslator{PublicModel: "p", Limits: DefaultLimits()}
				rerr = tr.Translate(strings.NewReader(in), &buf, nil)
			} else {
				tr := &AnthropicToGeminiSSETranslator{PublicModel: "p", Limits: DefaultLimits()}
				rerr = tr.Translate(strings.NewReader(in), &buf, nil)
			}
			out = buf.String()
			if rerr == nil || rerr.Status != 502 {
				t.Fatalf("array=%v %s: expected 502, got %v (%q)", isArray, name, rerr, out)
			}
			if strings.Contains(out, `"text":"B"`) || strings.Contains(out, `"text":"late"`) {
				t.Fatalf("array=%v %s: wrong content emitted before refusal: %q", isArray, name, out)
			}
			if isArray && strings.HasSuffix(strings.TrimSpace(out), "]") {
				t.Fatalf("array=%v %s: failed array must not close: %q", isArray, name, out)
			}
		}
		// Late logical-index regression: higher-index content already
		// emitted, so the failure is post-commit; the earlier content
		// stays, but the late lower-index content must never emit.
		for _, isArr := range []bool{false, true} {
			var buf bytes.Buffer
			var rerr *RouterError
			if isArr {
				tr := &AnthropicToGeminiJSONArrayTranslator{PublicModel: "p", Limits: DefaultLimits()}
				rerr = tr.Translate(strings.NewReader(late), &buf, nil)
			} else {
				tr := &AnthropicToGeminiSSETranslator{PublicModel: "p", Limits: DefaultLimits()}
				rerr = tr.Translate(strings.NewReader(late), &buf, nil)
			}
			out := buf.String()
			if rerr == nil || rerr.Status != 502 {
				t.Fatalf("array=%v late: expected 502, got %v (%q)", isArr, rerr, out)
			}
			if strings.Contains(out, `"text":"a"`) {
				t.Fatalf("array=%v late: regressed content emitted: %q", isArr, out)
			}
			if isArr && strings.HasSuffix(strings.TrimSpace(out), "]") {
				t.Fatalf("array=%v late: failed array must not close: %q", isArr, out)
			}
		}
	}
}

// Malformed calls suppress exactly one call; valid peers are retained.
func TestGeminiGatedMalformedPeers(t *testing.T) {
	in := a2gemSseInput(
		a2gemStartEvent(),
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"good","name":"ok","input":{"a":1}}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"bad","name":"no","input":{}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{{{"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":1}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":3}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	)
	for _, isArray := range []bool{false, true} {
		var out string
		var rerr *RouterError
		var buf bytes.Buffer
		if isArray {
			tr := &AnthropicToGeminiJSONArrayTranslator{PublicModel: "public-model", Limits: DefaultLimits()}
			rerr = tr.Translate(strings.NewReader(in), &buf, nil)
		} else {
			tr := &AnthropicToGeminiSSETranslator{PublicModel: "public-model", Limits: DefaultLimits()}
			rerr = tr.Translate(strings.NewReader(in), &buf, nil)
		}
		out = buf.String()
		if rerr != nil {
			t.Fatalf("array=%v malformed must map, not fail: %v", isArray, rerr.Msg)
		}
		if strings.Contains(out, `"STOP"`) {
			t.Fatalf("array=%v malformed must not carry STOP: %q", isArray, out)
		}
		calls := geminiAllCalls(t, out, isArray)
		if len(calls) != 1 || calls[0]["name"] != "ok" {
			t.Fatalf("array=%v valid peer must survive exactly once, got %v in %q", isArray, calls, out)
		}
		terms := geminiTerminals(t, out, isArray)
		if len(terms) != 1 || terms[0]["finishReason"] != "MALFORMED_FUNCTION_CALL" {
			t.Fatalf("array=%v terminal must be MALFORMED: %q", isArray, out)
		}
	}
}

// Writer and bound failures: sticky, no secondary writes, array unclosed.
func TestGeminiGatedWriterAndBound(t *testing.T) {
	in := a2gemSseInput(
		a2gemStartEvent(),
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
	for _, isArray := range []bool{false, true} {
		fw := &cgmFailWriter{}
		br := &cgmByteReader{data: []byte(in)}
		var rerr *RouterError
		if isArray {
			tr := &AnthropicToGeminiJSONArrayTranslator{PublicModel: "p", Limits: DefaultLimits()}
			rerr = tr.Translate(br, fw, nil)
		} else {
			tr := &AnthropicToGeminiSSETranslator{PublicModel: "p", Limits: DefaultLimits()}
			rerr = tr.Translate(br, fw, nil)
		}
		if rerr == nil || rerr.Status != 502 {
			t.Fatalf("array=%v expected write 502, got %v", isArray, rerr)
		}
		if fw.writes != 1 {
			t.Fatalf("array=%v want exactly 1 downstream write, got %d", isArray, fw.writes)
		}
		if br.served >= len(in) {
			t.Fatalf("array=%v upstream must not be drained after write failure", isArray)
		}
	}
	// Per-frame bound: one early tool frame alone exceeds the limit.
	lim := DefaultLimits()
	lim.MaxFrameBytes = 512
	frag := strings.ReplaceAll(`{"k":"`+strings.Repeat("v", 600)+`"}`, `"`, `\"`)
	big := a2gemSseInput(
		a2gemStartEvent(),
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t","name":"f","input":{}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"`+frag+`"}}`,
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
	var buf bytes.Buffer
	tr := &AnthropicToGeminiSSETranslator{PublicModel: "p", Limits: lim}
	if rerr := tr.Translate(strings.NewReader(big), &buf, nil); rerr == nil || rerr.Status != 502 {
		t.Fatalf("expected bound 502, got %v", rerr)
	}
	var buf2 bytes.Buffer
	atr := &AnthropicToGeminiJSONArrayTranslator{PublicModel: "p", Limits: lim}
	if rerr := atr.Translate(strings.NewReader(big), &buf2, nil); rerr == nil || rerr.Status != 502 {
		t.Fatalf("expected array bound 502, got %v", rerr)
	}
	if strings.HasSuffix(strings.TrimSpace(buf2.String()), "]") {
		t.Fatalf("failed array must not close: %q", buf2.String())
	}
}
