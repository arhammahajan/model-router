// Anthropic target stream encoder tests: shared pipeline path.
//
// These tests drive ChatToAnthropicSSETranslator.Translate (thin selection
// of SSE framing + ChatStreamDecoder + AnthropicEventEncoder +
// TranslateStream) and the encoder directly. They pin the target-owned
// contract: zero placeholder start, immediate fragments, monotonic block
// indices, opaque IDs, late authoritative usage with cache subtraction,
// malformed refusal as a single sanitized error, truncation, writer
// failure discipline, frame bounds, prompt terminal return, and reuse.
package protocol

import (
	"errors"
	"strings"
	"testing"
)

func mustContain(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Fatalf("output lacks %q:\n%s", w, out)
		}
	}
}

func TestAnthropicEncoderLateUsageCacheSubtraction(t *testing.T) {
	usage := `{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":4}}`
	upstream := chatChunk(`{"content":"hi"}`, "", "") +
		chatChunk(`{}`, "stop", "") + chatUsageChunk(usage) + "data: [DONE]\n\n"
	out, rerr := runChatStream(t, upstream, DefaultLimits())
	if rerr != nil {
		t.Fatalf("stream: %v", rerr)
	}
	// message_start placeholder stays zero; terminal delta corrects.
	mustContain(t, out, `"input_tokens":0`, `"output_tokens":0`)
	mustContain(t, out, `"input_tokens":6`, `"cache_read_input_tokens":4`, `"output_tokens":5`)
	if strings.Index(out, `"input_tokens":0`) > strings.Index(out, `"input_tokens":6`) {
		t.Fatalf("placeholder must precede correction:\n%s", out)
	}
}

func TestAnthropicEncoderOpaqueIDsAndLargeInt(t *testing.T) {
	long := strings.Repeat("z", 100)
	upstream := chatChunk(`{"tool_calls":[{"index":0,"id":"has:colon and space","type":"function","function":{"name":"f","arguments":"{}"}}]}`, "", "") +
		chatChunk(`{"tool_calls":[{"index":1,"id":"`+long+`","type":"function","function":{"name":"g","arguments":"{\"n\":9007199254740993}"}}]}`, "", "") +
		chatChunk(`{}`, "tool_calls", "") +
		chatUsageChunk(`{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}`) +
		"data: [DONE]\n\n"
	out, rerr := runChatStream(t, upstream, DefaultLimits())
	if rerr != nil {
		t.Fatalf("stream: %v", rerr)
	}
	mustContain(t, out, `"has:colon and space"`, long, "9007199254740993", `"stop_reason":"tool_use"`)
	// Emission order follows arrival: index 0 block before index 1.
	if strings.Index(out, "has:colon") > strings.Index(out, long) {
		t.Fatalf("tool order not preserved:\n%s", out)
	}
	// Target indices are monotonic emission order 0, 1.
	i0 := strings.Index(out, `"index":0`)
	i1 := strings.Index(out, `"index":1`)
	if i0 < 0 || i1 < 0 || i0 > i1 {
		t.Fatalf("target indices not monotonic:\n%s", out)
	}
}

func TestAnthropicEncoderMalformedArgsSingleError(t *testing.T) {
	upstream := chatChunk(`{"tool_calls":[{"index":0,"id":"c1","function":{"name":"a","arguments":"[1]"}}]}`, "", "") +
		chatChunk(`{}`, "tool_calls", "") +
		chatUsageChunk(`{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}`) +
		"data: [DONE]\n\n"
	out, rerr := runChatStream(t, upstream, DefaultLimits())
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("malformed args must 502, got %v", rerr)
	}
	if strings.Contains(out, "message_stop") || strings.Contains(out, "message_delta") {
		t.Fatalf("malformed must emit no success terminal:\n%s", out)
	}
	if got := strings.Count(out, "event: error"); got != 1 {
		t.Fatalf("malformed must emit exactly one error event, got %d:\n%s", got, out)
	}
	// The sanitized error frame itself carries no values.
	if idx := strings.LastIndex(out, "event: error"); idx >= 0 {
		if strings.Contains(out[idx:], "[1]") {
			t.Fatalf("error event must not echo values:\n%s", out)
		}
	}
}

func TestAnthropicEncoderTruncationNoTerminal(t *testing.T) {
	upstream := chatChunk(`{"content":"Hi"}`, "", "") + chatChunk(`{}`, "stop", "")
	out, rerr := runChatStream(t, upstream, DefaultLimits())
	if rerr == nil {
		t.Fatalf("truncated stream must fail")
	}
	if strings.Contains(out, "message_stop") || strings.Contains(out, "message_delta") {
		t.Fatalf("truncation must emit no terminal:\n%s", out)
	}
}

func TestAnthropicEncoderFirstDeltaBeforeTerminal(t *testing.T) {
	usage := `{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}`
	upstream := chatChunk(`{"content":"early"}`, "", "") +
		chatChunk(`{}`, "stop", "") + chatUsageChunk(usage) + "data: [DONE]\n\n"
	out, rerr := runChatStream(t, upstream, DefaultLimits())
	if rerr != nil {
		t.Fatalf("stream: %v", rerr)
	}
	delta := strings.Index(out, `"text_delta"`)
	md := strings.Index(out, "event: message_delta")
	if delta < 0 || md < 0 || delta > md {
		t.Fatalf("first delta must precede terminal delta:\n%s", out)
	}
}

func TestAnthropicEncoderUsageRepresentability(t *testing.T) {
	term := func(usage string) string {
		return chatChunk(`{}`, "stop", "") + chatUsageChunk(usage) + "data: [DONE]\n\n"
	}
	cases := map[string]string{
		"reasoning":   `{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3,"completion_tokens_details":{"reasoning_tokens":1}}`,
		"cache_write": `{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7,"prompt_tokens_details":{"cache_write_tokens":1}}`,
		"text":        `{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7,"prompt_tokens_details":{"text_tokens":2}}`,
	}
	for name, usage := range cases {
		out, rerr := runChatStream(t, term(usage), DefaultLimits())
		if rerr == nil || rerr.Status != 502 {
			t.Fatalf("%s usage must 502, got %v", name, rerr)
		}
		if strings.Contains(out, "message_stop") {
			t.Fatalf("%s usage must not succeed:\n%s", name, out)
		}
	}
}

type countWriter struct {
	writes int
	failAt int
}

func (w *countWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.failAt > 0 && w.writes >= w.failAt {
		return 0, errors.New("downstream unavailable")
	}
	return len(p), nil
}

func TestAnthropicEncoderWriterFailureNoAbortWrites(t *testing.T) {
	usage := `{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}`
	upstream := chatChunk(`{"content":"Hello, this is a long enough first write to commit"}`, "", "") +
		chatChunk(`{"content":"more"}`, "", "") +
		chatChunk(`{}`, "stop", "") + chatUsageChunk(usage) + "data: [DONE]\n\n"
	tr := &ChatToAnthropicSSETranslator{PublicModel: "public", Limits: DefaultLimits()}
	cw := &countWriter{failAt: 2}
	rerr := tr.Translate(strings.NewReader(upstream), cw, nil)
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("writer failure must 502, got %v", rerr)
	}
	// First write commits, second fails; Abort must not attempt a third.
	if cw.writes != 2 {
		t.Fatalf("writer failure must stop after the failed write, got %d writes", cw.writes)
	}
}

func TestAnthropicEncoderEncodedFrameBound(t *testing.T) {
	lim := DefaultLimits()
	lim.MaxFrameBytes = 1024
	enc := NewAnthropicEventEncoder("public", lim)
	var sb strings.Builder
	if rerr := enc.Encode(&sb, nil, ResponseStart{ID: "r", Model: "m", Source: DialectChat}); rerr != nil {
		t.Fatalf("start: %v", rerr.Err)
	}
	open, rerr := NewBlockOpen("t0", BlockText, "", "")
	if rerr != nil {
		t.Fatal(rerr)
	}
	if eerr := enc.Encode(&sb, nil, open); eerr != nil {
		t.Fatalf("open: %v", eerr.Err)
	}
	big := strings.Repeat("x", 2048)
	if eerr := enc.Encode(&sb, nil, TextFragment{Item: "t0", Text: big}); eerr == nil || !strings.Contains(eerr.Err.Msg, "size bound") {
		t.Fatalf("oversize encoded frame must fail, got %v", eerr)
	}
}

func TestAnthropicEncoderTranslatorReuse(t *testing.T) {
	usage := `{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}`
	mk := func() string {
		return chatChunk(`{"content":"hi"}`, "", "") + chatChunk(`{}`, "stop", "") +
			chatUsageChunk(usage) + "data: [DONE]\n\n"
	}
	tr := &ChatToAnthropicSSETranslator{PublicModel: "public", Limits: DefaultLimits()}
	var a, b strings.Builder
	if rerr := tr.Translate(strings.NewReader(mk()), &a, nil); rerr != nil {
		t.Fatalf("first use: %v", rerr)
	}
	if rerr := tr.Translate(strings.NewReader(mk()), &b, nil); rerr != nil {
		t.Fatalf("second use: %v", rerr)
	}
	for _, out := range []string{a.String(), b.String()} {
		mustContain(t, out, "event: message_stop", `"model":"public"`)
	}
}

func TestAnthropicEncoderInitialUsagePreserved(t *testing.T) {
	lim := DefaultLimits()
	enc := NewAnthropicEventEncoder("public", lim)
	var sb strings.Builder
	initial := ResponseUsage{
		Prompt: intPtr(10), Completion: intPtr(5), Total: intPtr(15),
		CacheRead:  intPtr(4),
		Provenance: UsageProvenance{Source: DialectAnthropic, Scope: UsageInitial},
	}
	if eerr := enc.Encode(&sb, nil, ResponseStart{ID: "r", Model: "m", Source: DialectAnthropic, InitialUsage: &initial}); eerr != nil {
		t.Fatalf("start: %v", eerr.Err)
	}
	open, _ := NewBlockOpen("t0", BlockText, "", "")
	if eerr := enc.Encode(&sb, nil, open); eerr != nil {
		t.Fatalf("open: %v", eerr.Err)
	}
	out := sb.String()
	// Initial totals preserved via shared accounting (10-4=6), not zeros.
	mustContain(t, out, `"input_tokens":6`, `"cache_read_input_tokens":4`)
	if strings.Contains(out, `"input_tokens":0`) {
		t.Fatalf("initial usage must replace the placeholder:\n%s", out)
	}
}

func TestAnthropicEncoderEmptyGeneration(t *testing.T) {
	upstream := chatChunk(`{}`, "stop", "") +
		chatUsageChunk(`{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}`) +
		"data: [DONE]\n\n"
	out, rerr := runChatStream(t, upstream, DefaultLimits())
	if rerr != nil {
		t.Fatalf("empty generation: %v", rerr)
	}
	mustContain(t, out, "event: message_start", "event: content_block_start", "event: content_block_stop", "event: message_delta", "event: message_stop")
}
