package protocol

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

var errReviewDownstream = errors.New("downstream gone")

type reviewGatedReader struct {
	data    []byte
	pos     int
	gate    int
	out     *bytes.Buffer
	want    string
	checked bool
	failed  bool
}

func (r *reviewGatedReader) Read(p []byte) (int, error) {
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

// Independent review probes for the Anthropic stream cutover (7c7d880).
// Each probe is executable evidence, not a comment:
// - ModelEvent cross-merge: Anthropic empty-model start promoted later
//   stays metadata-only across all four shared encoders.
// - Chat start flush failure: validated start flushes immediately, so a
//   downstream failure fails fast without draining stalled upstream.
// - Gemini empty-text barrier: an empty text close releases a held tool
//   before the terminal (regression for 7c7d880).

func TestReviewAnthropicModelEventZeroWire(t *testing.T) {
	lim := DefaultLimits()
	life := NewResponseLifecycle(lim, DialectAnthropic)
	start := ResponseStart{ID: "msg_9", Model: "", Source: DialectAnthropic}
	if rerr := life.Observe(start); rerr != nil {
		t.Fatalf("anthropic empty-model start: %v", rerr)
	}
	// Open/close one text block, then late model report, then terminal.
	toolOpen, rerr := NewToolBlockOpen("b1", "call:1", "f", 1)
	if rerr != nil {
		t.Fatalf("tool open: %v", rerr)
	}
	textOpen, rerr := NewIndexedBlockOpen("b0", BlockText, "", "", 0)
	if rerr != nil {
		t.Fatalf("text open: %v", rerr)
	}
	evs := []ResponseEvent{
		textOpen,
		TextFragment{Item: "b0", Text: "hi"},
		BlockClose{Item: "b0"},
		toolOpen,
		ArgsFragment{Item: "b1", Fragment: `{"n":1}`},
		BlockClose{Item: "b1"},
		ResponseModelEvent{Model: "claude-x"},
		UsageEvent{Usage: mustReviewTerminalUsage(t, 10, 5)},
		mustReviewFinish(t, FinishToolCalls),
	}
	for i, ev := range evs {
		if rerr := life.Observe(ev); rerr != nil {
			t.Fatalf("observe[%d](%T): %v", i, ev, rerr)
		}
	}
	res, rerr := life.Result()
	if rerr != nil {
		t.Fatalf("Result: %v", rerr)
	}
	if res.Model != "claude-x" || string(res.ID) != "msg_9" || res.Source != DialectAnthropic {
		t.Fatalf("source summary not truthful: %+v", res)
	}
	// Every shared encoder must consume the ModelEvent with zero bytes.
	all := append([]ResponseEvent{start}, evs...)
	for name, enc := range map[string]EventEncoder{
		"chat":      NewChatEventEncoder("public", true, lim),
		"responses": NewResponsesEventEncoder("public", lim),
		"anthropic": NewAnthropicEventEncoder("public", lim),
		"gemini":    NewGeminiEventEncoder("public", lim, true),
	} {
		var buf bytes.Buffer
		for i, ev := range all {
			before := buf.Len()
			if eerr := enc.Encode(&buf, nil, ev); eerr != nil {
				t.Fatalf("%s Encode[%d](%T): %v", name, i, ev, eerr.Err)
			}
			if _, ok := ev.(ResponseModelEvent); ok && buf.Len() != before {
				t.Fatalf("%s ModelEvent wrote %d bytes", name, buf.Len()-before)
			}
		}
		out := buf.String()
		if strings.Contains(out, "claude-x") {
			t.Fatalf("%s leaked provider model: %q", name, out)
		}
	}
}

func TestReviewChatStartFlushFailureFast(t *testing.T) {
	// Valid minimal wire, but downstream fails on the very first write
	// (the validated role chunk). The pipeline must fail fast without
	// draining upstream.
	upstream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_9\",\"usage\":{\"input_tokens\":8}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":3}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	rdr := &reviewCountReader{data: []byte(upstream)}
	w := &reviewFailFirstWriter{}
	tr := &SSETranslator{PublicModel: "p", IncludeUsage: true, Limits: DefaultLimits()}
	rerr := tr.Translate(rdr, w, func() { w.flushed = true })
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("start flush failure must 502, got %v", rerr)
	}
	if w.writes != 1 {
		t.Fatalf("want exactly one downstream attempt, got %d", w.writes)
	}
	if rdr.served >= len(upstream) {
		t.Fatalf("upstream drained after write failure (served %d of %d)", rdr.served, len(upstream))
	}
	if strings.Contains(w.buf.String(), "[DONE]") {
		t.Fatalf("success marker after write failure: %q", w.buf.String())
	}
	// Encoder-level stickiness: further Encode stays WriteFailed.
	enc2 := NewChatEventEncoder("p", true, DefaultLimits())
	bad := &reviewFailFirstWriter{}
	if eerr := enc2.Encode(bad, nil, ResponseStart{ID: "m", Model: "x", Source: DialectAnthropic}); eerr == nil || !eerr.WriteFailed {
		t.Fatalf("start flush failure must be WriteFailed, got %v", eerr)
	}
	if eerr := enc2.Encode(bad, nil, TextFragment{Item: "b0", Text: "late"}); eerr == nil || !eerr.WriteFailed {
		t.Fatalf("post-failure Encode must stay WriteFailed, got %v", eerr)
	}
}

type reviewFailFirstWriter struct {
	writes  int
	flushed bool
	buf     bytes.Buffer
}

func mustReviewTerminalUsage(t *testing.T, prompt, completion int) ResponseUsage {
	t.Helper()
	u, rerr := NewTerminalUsage(DialectAnthropic, prompt, completion, prompt+completion)
	if rerr != nil {
		t.Fatalf("NewTerminalUsage: %v", rerr)
	}
	return u
}

func mustReviewFinish(t *testing.T, kind FinishKind) CompletionEvent {
	t.Helper()
	f, rerr := NewFinish(kind, string(kind))
	if rerr != nil {
		t.Fatalf("NewFinish: %v", rerr)
	}
	ev, rerr := NewCompletionEvent(f)
	if rerr != nil {
		t.Fatalf("NewCompletionEvent: %v", rerr)
	}
	return ev
}

func (w *reviewFailFirstWriter) Write(p []byte) (int, error) {
	w.writes++
	return 0, errReviewDownstream
}

type reviewCountReader struct {
	data   []byte
	served int
}

func (r *reviewCountReader) Read(p []byte) (int, error) {
	if r.served >= len(r.data) {
		return 0, io.EOF
	}
	p[0] = r.data[r.served]
	r.served++
	return 1, nil
}

func TestReviewGeminiEmptyTextBarrierBeforeTerminal(t *testing.T) {
	// Empty text at index 0 closes, then tool at index 1 must release
	// before the terminal is read (7c7d880 regression).
	in := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_9\",\"model\":\"claude-x\",\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":5,\"output_tokens\":1}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"f\",\"input\":{}}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":2}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	for _, isArray := range []bool{false, true} {
		gate := strings.Index(in, "event: message_delta")
		var out bytes.Buffer
		gr := &reviewGatedReader{data: []byte(in), gate: gate, out: &out, want: `"functionCall"`}
		var rerr *RouterError
		if isArray {
			tr := &AnthropicToGeminiJSONArrayTranslator{PublicModel: "p", Limits: DefaultLimits()}
			rerr = tr.Translate(gr, &out, nil)
		} else {
			tr := &AnthropicToGeminiSSETranslator{PublicModel: "p", Limits: DefaultLimits()}
			rerr = tr.Translate(gr, &out, nil)
		}
		if rerr != nil {
			t.Fatalf("array=%v: %v", isArray, rerr.Msg)
		}
		if gr.failed {
			t.Fatalf("array=%v tool missing before terminal: %q", isArray, out.String())
		}
		if got := strings.Count(out.String(), `"functionCall"`); got != 1 {
			t.Fatalf("array=%v want exactly one call, got %d in %q", isArray, got, out.String())
		}
		if strings.Count(out.String(), `"finishReason"`) != 1 {
			t.Fatalf("array=%v want exactly one terminal, got %q", isArray, out.String())
		}
	}
}
