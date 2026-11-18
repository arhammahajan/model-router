package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// --- Wire lifecycle: monotonic seq, stable IDs, null pre-terminal usage ---

func TestResponsesEncoderTextWire(t *testing.T) {
	origNow := responsesNowUnix
	responsesNowUnix = func() int64 { return 1700000000 }
	defer func() { responsesNowUnix = origNow }()
	events, rerr := runResponsesStream(t, []string{
		respChatChunk(`{"role":"assistant","content":"Hello"}`, ""),
		respChatChunk(`{"content":" world"}`, ""),
		respChatChunk(`{}`, "stop"),
		respChatUsageChunk(),
		"[DONE]",
	})
	if rerr != nil {
		t.Fatalf("translate failed: %v", rerr)
	}
	types := []string{}
	for _, ev := range events {
		types = append(types, ev["type"].(string))
	}
	want := []string{
		"response.created", "response.in_progress",
		"response.output_item.added", "response.content_part.added",
		"response.output_text.delta", "response.output_text.delta",
		"response.output_text.done", "response.content_part.done",
		"response.output_item.done", "response.completed",
	}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("bad lifecycle:\n got %v\nwant %v", types, want)
	}
	for i, ev := range events {
		if int(ev["sequence_number"].(float64)) != i {
			t.Fatalf("sequence not monotonic at %d: %v", i, ev)
		}
	}
	created := events[0]["response"].(map[string]any)
	if created["status"] != "in_progress" || created["usage"] != nil {
		t.Fatalf("created must be in_progress with null usage: %v", created)
	}
	if created["model"] != "public-model" {
		t.Fatalf("public model not rendered: %v", created)
	}
	completed := events[len(events)-1]["response"].(map[string]any)
	if completed["status"] != "completed" {
		t.Fatalf("bad terminal: %v", completed)
	}
	output := completed["output"].([]any)
	msg := output[0].(map[string]any)
	if msg["content"].([]any)[0].(map[string]any)["text"] != "Hello world" {
		t.Fatalf("terminal must repeat full text: %v", msg)
	}
	msgID := msg["id"].(string)
	for _, ev := range events[2 : len(events)-1] {
		for _, k := range []string{"item_id", "item"} {
			if v, ok := ev[k]; ok {
				switch v := v.(type) {
				case string:
					if v != msgID {
						t.Fatalf("unstable item id: %v vs %v", v, msgID)
					}
				case map[string]any:
					if v["id"] != msgID {
						t.Fatalf("unstable item id: %v vs %v", v["id"], msgID)
					}
				}
			}
		}
	}
}

// --- Immediacy: deltas flush before the terminal, no target re-decode ---

func TestResponsesEncoderImmediacy(t *testing.T) {
	var out bytes.Buffer
	enc := NewResponsesEventEncoder("m", DefaultLimits())
	mustEncode := func(ev ResponseEvent) {
		t.Helper()
		if eerr := enc.Encode(&out, nil, ev); eerr != nil {
			t.Fatalf("encode %#v: %v", ev, eerr)
		}
	}
	start, _ := NewResponseID("chatcmpl-1")
	mustEncode(ResponseStart{ID: start, Model: "u", Source: DialectChat})
	open, _ := NewBlockOpen("t0", BlockText, "", "")
	mustEncode(open)
	frag, _ := NewTextFragment("t0", "hi")
	mustEncode(frag)
	s := out.String()
	if !strings.Contains(s, "response.output_text.delta") {
		t.Fatalf("first delta must flush immediately, got %q", s)
	}
	if strings.Contains(s, "response.completed") || strings.Contains(s, "response.output_text.done") {
		t.Fatalf("terminal must not emit before completion: %q", s)
	}
	if enc.Committed() != true {
		t.Fatalf("encoder must report committed after first write")
	}
}

// --- Opaque IDs, exact bigint, empty args ---

func TestResponsesEncoderOpaqueBigintEmpty(t *testing.T) {
	events, rerr := runResponsesStream(t, []string{
		respChatChunk(`{"tool_calls":[{"index":0,"id":"has:colon and space","type":"function","function":{"name":"calc","arguments":""}}]}`, ""),
		respChatChunk(`{"tool_calls":[{"index":0,"function":{"arguments":"{\"n\":9007199254740993}"}}]}`, ""),
		respChatChunk(`{"tool_calls":[{"index":1,"id":"call_empty","type":"function","function":{"name":"ping","arguments":""}}]}`, ""),
		respChatChunk(`{}`, "tool_calls"),
		respChatUsageChunk(),
		"[DONE]",
	})
	if rerr != nil {
		t.Fatalf("translate failed: %v", rerr)
	}
	completed := events[len(events)-1]["response"].(map[string]any)
	output := completed["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("expected 2 calls, got %v", output)
	}
	byCall := map[string]string{}
	for _, item := range output {
		m := item.(map[string]any)
		byCall[m["call_id"].(string)] = m["arguments"].(string)
	}
	if byCall["has:colon and space"] != `{"n":9007199254740993}` {
		t.Fatalf("opaque id/bigint lost: %v", byCall)
	}
	if byCall["call_empty"] != "{}" {
		t.Fatalf("empty args must render {}: %v", byCall)
	}
}

// --- Tools-first then text: unique indices, no silent reorder ---

func TestResponsesEncoderToolFirstThenText(t *testing.T) {
	events, rerr := runResponsesStream(t, []string{
		respChatChunk(`{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]}`, ""),
		respChatChunk(`{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\"Paris\"}"}}]}`, ""),
		respChatChunk(`{"content":"done"}`, ""),
		respChatChunk(`{}`, "tool_calls"),
		respChatUsageChunk(),
		"[DONE]",
	})
	if rerr != nil {
		t.Fatalf("tool-first then text failed: %v", rerr)
	}
	added := map[string]float64{}
	for _, ev := range events {
		if ev["type"] == "response.output_item.added" {
			item := ev["item"].(map[string]any)
			added[item["id"].(string)] = ev["output_index"].(float64)
		}
	}
	if len(added) != 2 {
		t.Fatalf("expected 2 added items, got %v", added)
	}
	seen := map[float64]bool{}
	for _, idx := range added {
		if seen[idx] {
			t.Fatalf("duplicate output_index %v", idx)
		}
		seen[idx] = true
	}
	completed := events[len(events)-1]["response"].(map[string]any)
	if len(completed["output"].([]any)) != 2 {
		t.Fatalf("terminal must carry tool+text, got %v", completed["output"])
	}
}

// --- Cache + reasoning gates ---

func TestResponsesEncoderCacheReasoning(t *testing.T) {
	events, rerr := runResponsesStream(t, []string{
		respChatChunk(`{"content":"Hi"}`, ""),
		respChatChunk(`{}`, "stop"),
		respChatUsageChunk(),
		"[DONE]",
	})
	if rerr != nil {
		t.Fatalf("translate failed: %v", rerr)
	}
	completed := events[len(events)-1]["response"].(map[string]any)
	usage := completed["usage"].(map[string]any)
	if usage["input_tokens"] != float64(12) || usage["total_tokens"] != float64(17) {
		t.Fatalf("authoritative usage missing: %v", usage)
	}
	det := usage["input_tokens_details"].(map[string]any)
	if det["cached_tokens"] != float64(3) {
		t.Fatalf("cached share lost: %v", usage)
	}
	outDet := usage["output_tokens_details"].(map[string]any)
	if outDet["reasoning_tokens"] != float64(2) {
		t.Fatalf("reasoning share lost: %v", usage)
	}
}

// --- Early/late invalid: precommit zero bytes, value-free diagnostics ---

func TestResponsesEncoderEarlyLateInvalid(t *testing.T) {
	// Precommit: usage before finish emits zero bytes.
	var out bytes.Buffer
	tr := &ChatToResponsesSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
	var in bytes.Buffer
	in.WriteString("data: " + respChatUsageChunk() + "\n\n")
	in.WriteString("data: [DONE]\n\n")
	rerr := tr.Translate(&in, &out, nil)
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("usage before finish must fail, got %v", rerr)
	}
	if out.Len() != 0 {
		t.Fatalf("precommit failure must emit zero bytes, got %q", out.String())
	}
	// Late: unknown finish value is static, never echoes the secret.
	secret := "private-finish-value"
	var out2 bytes.Buffer
	var in2 bytes.Buffer
	in2.WriteString("data: " + respChatChunk(`{}`, secret) + "\n\n")
	rerr = tr.Translate(&in2, &out2, nil)
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("unknown finish must fail, got %v", rerr)
	}
	if strings.Contains(rerr.Msg, secret) || strings.Contains(out2.String(), secret) {
		t.Fatalf("secret finish value echoed: %v %q", rerr, out2.String())
	}
	// Truncation and missing-usage cases.
	for name, chunks := range map[string][]string{
		"truncated no DONE": {respChatChunk(`{"content":"Hi"}`, "stop")},
		"no usage": {
			respChatChunk(`{"content":"Hi"}`, ""),
			respChatChunk(`{}`, "stop"),
			"[DONE]",
		},
		"duplicate finish": {
			respChatChunk(`{}`, "stop"),
			respChatChunk(`{}`, "stop"),
			respChatUsageChunk(), "[DONE]",
		},
	} {
		if _, rerr := runResponsesStream(t, chunks); rerr == nil || rerr.Status != 502 {
			t.Fatalf("%s must fail closed, got %v", name, rerr)
		}
	}
}

// --- Tool name grammar is target-side; opaque call IDs kept ---

func TestResponsesEncoderNameGrammarOpaqueIDs(t *testing.T) {
	// Grammar-invalid name passes source decode but refuses at the target.
	if _, rerr := runResponsesStream(t, []string{
		respChatChunk(`{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"has space","arguments":"{}"}}]}`, ""),
		respChatChunk(`{}`, "tool_calls"),
		respChatUsageChunk(),
		"[DONE]",
	}); rerr == nil || rerr.Status != 502 {
		t.Fatalf("grammar-invalid name must refuse at target, got %v", rerr)
	}
	// Opaque call IDs with punctuation pass end to end.
	events, rerr := runResponsesStream(t, []string{
		respChatChunk(`{"tool_calls":[{"index":0,"id":"has:colon and space","type":"function","function":{"name":"f","arguments":"{}"}}]}`, ""),
		respChatChunk(`{}`, "tool_calls"),
		respChatUsageChunk(),
		"[DONE]",
	})
	if rerr != nil {
		t.Fatalf("opaque call id must pass, got %v", rerr)
	}
	completed := events[len(events)-1]["response"].(map[string]any)
	found := false
	for _, item := range completed["output"].([]any) {
		if item.(map[string]any)["call_id"] == "has:colon and space" {
			found = true
		}
	}
	if !found {
		t.Fatalf("opaque call id lost: %v", completed["output"])
	}
}

// --- Malformed args: exactly one sanitized response.failed, never completed ---

func TestResponsesEncoderMalformedSingleFailed(t *testing.T) {
	var in bytes.Buffer
	in.WriteString("data: " + respChatChunk(`{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"a","arguments":"[1]"}}]}`, "") + "\n\n")
	in.WriteString("data: " + respChatChunk(`{}`, "tool_calls") + "\n\n")
	in.WriteString("data: " + respChatUsageChunk() + "\n\n")
	in.WriteString("data: [DONE]\n\n")
	var out bytes.Buffer
	tr := &ChatToResponsesSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
	rerr := tr.Translate(&in, &out, nil)
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("malformed args must fail, got %v", rerr)
	}
	s := out.String()
	if strings.Count(s, "event: response.failed") != 1 {
		t.Fatalf("must emit exactly one response.failed, got %q", s)
	}
	if strings.Contains(s, "response.completed") || strings.Contains(s, "response.incomplete") {
		t.Fatalf("malformed must never complete: %q", s)
	}
	for _, frame := range strings.Split(s, "\n\n") {
		if strings.Contains(frame, "event: response.failed") && strings.Contains(frame, "[1]") {
			t.Fatalf("failed event must not echo args: %q", frame)
		}
	}
}

// --- Bounded retention and downstream frame cap ---

func TestResponsesEncoderBounds(t *testing.T) {
	lim := DefaultLimits()
	lim.MaxStreamArgsChars = 16
	var in bytes.Buffer
	in.WriteString("data: " + respChatChunk(`{"content":"0123456789ABCDEF"}`, "") + "\n\n")
	in.WriteString("data: " + respChatChunk(`{"content":"X"}`, "") + "\n\n")
	var out bytes.Buffer
	tr := &ChatToResponsesSSETranslator{PublicModel: "m", Limits: lim}
	if rerr := tr.Translate(&in, &out, nil); rerr == nil || rerr.Status != 502 {
		t.Fatalf("aggregate text bound must fail, got %v", rerr)
	}
	// Final frame over the downstream cap fails without a completed marker.
	lim2 := DefaultLimits()
	lim2.MaxStreamArgsChars = 200
	lim2.MaxFrameBytes = 120
	text := strings.Repeat("x", 100)
	q, _ := json.Marshal(text)
	var in2 bytes.Buffer
	in2.WriteString("data: " + respChatChunk(`{"content":`+string(q)+`}`, "") + "\n\n")
	in2.WriteString("data: " + respChatChunk(`{}`, "stop") + "\n\n")
	in2.WriteString("data: " + `{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"u","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}` + "\n\n")
	in2.WriteString("data: [DONE]\n\n")
	var out2 bytes.Buffer
	tr2 := &ChatToResponsesSSETranslator{PublicModel: "m", Limits: lim2}
	if rerr := tr2.Translate(&in2, &out2, nil); rerr == nil || rerr.Status != 502 {
		t.Fatalf("final frame over cap must fail, got %v", rerr)
	}
	if strings.Contains(out2.String(), "response.completed") {
		t.Fatalf("must not emit completed when final frame exceeds cap")
	}
}

// --- Valid terminal returns without EOF: trailing bytes unread ---

func TestResponsesEncoderNoReadPastTerminal(t *testing.T) {
	var in bytes.Buffer
	in.WriteString("data: " + respChatChunk(`{"content":"Hi"}`, "") + "\n\n")
	in.WriteString("data: " + respChatChunk(`{}`, "stop") + "\n\n")
	in.WriteString("data: " + `{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"u","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}` + "\n\n")
	in.WriteString("data: [DONE]\n\n")
	in.WriteString("data: " + respChatChunk(`{"content":"NEXT-GENERATION"}`, "") + "\n\n")
	var out bytes.Buffer
	tr := &ChatToResponsesSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
	if rerr := tr.Translate(&in, &out, nil); rerr != nil {
		t.Fatalf("valid terminal must succeed without EOF, got %v", rerr)
	}
	if strings.Contains(out.String(), "NEXT-GENERATION") {
		t.Fatalf("must not read past the terminal: %q", out.String())
	}
}

// --- Writer failure: sticky WriteFailed, no Abort, no extra reads ---

type responsesFailWriter struct {
	n         int
	failAfter int
	writes    int
}

func (w *responsesFailWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.n >= w.failAfter {
		return 0, fmt.Errorf("downstream closed")
	}
	w.n++
	return len(p), nil
}

func TestResponsesEncoderWriterFailureNoAbort(t *testing.T) {
	var in bytes.Buffer
	in.WriteString("data: " + respChatChunk(`{"content":"Hi"}`, "") + "\n\n")
	in.WriteString("data: " + respChatChunk(`{}`, "stop") + "\n\n")
	in.WriteString("data: " + `{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"u","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}` + "\n\n")
	in.WriteString("data: [DONE]\n\n")
	w := &responsesFailWriter{failAfter: 1}
	tr := &ChatToResponsesSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
	rerr := tr.Translate(&in, w, nil)
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("writer failure must stop the stream, got %v", rerr)
	}
	if rerr.Msg != "Downstream write failed" {
		t.Fatalf("writer failure must surface write error, got %v", rerr)
	}
	// Only the writes up to the failure may exist; Abort must not add a
	// response.failed after a downstream write failure.
	if w.writes > 2 {
		t.Fatalf("no extra writes after failure: %d", w.writes)
	}
}

// Encoder-level stickiness: after WriteFailed, Encode and Abort stay silent.
func TestResponsesEncoderStickyWriteFailed(t *testing.T) {
	enc := NewResponsesEventEncoder("m", DefaultLimits())
	start, _ := NewResponseID("r")
	if eerr := enc.Encode(failWriter{}, nil, ResponseStart{ID: start, Model: "u", Source: DialectChat}); eerr != nil {
		t.Fatalf("start must not write: %v", eerr)
	}
	open, _ := NewBlockOpen("t0", BlockText, "", "")
	eerr := enc.Encode(failWriter{}, nil, open)
	if eerr == nil || !eerr.WriteFailed {
		t.Fatalf("first downstream failure must be WriteFailed, got %v", eerr)
	}
	frag, _ := NewTextFragment("t0", "hi")
	if eerr := enc.Encode(failWriter{}, nil, frag); eerr == nil || !eerr.WriteFailed {
		t.Fatalf("sticky state must stay WriteFailed, got %v", eerr)
	}
	var out bytes.Buffer
	enc.Abort(&out, nil)
	if out.Len() != 0 {
		t.Fatalf("Abort after write failure must not write, got %q", out.String())
	}
}

// --- No duplicate response.failed: post-commit upstream error emits one ---

func TestResponsesEncoderNoDuplicateFailed(t *testing.T) {
	var in bytes.Buffer
	in.WriteString("data: " + respChatChunk(`{"content":"Hi"}`, "") + "\n\n")
	in.WriteString(`data: {"error":{"message":"boom","type":"server_error"}}` + "\n\n")
	var out bytes.Buffer
	tr := &ChatToResponsesSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
	rerr := tr.Translate(&in, &out, nil)
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("expected post-commit failure, got %v", rerr)
	}
	s := out.String()
	if strings.Count(s, "event: response.failed") != 1 {
		t.Fatalf("exactly one response.failed, got %q", s)
	}
	if strings.Contains(s, "boom") {
		t.Fatalf("failed event must be sanitized: %q", s)
	}
}

// --- Translator reuse: fresh encoder state per Translate ---

func TestResponsesEncoderTranslatorReuse(t *testing.T) {
	tr := &ChatToResponsesSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
	mk := func() bytes.Buffer {
		var in bytes.Buffer
		in.WriteString("data: " + respChatChunk(`{"content":"Hi"}`, "") + "\n\n")
		in.WriteString("data: " + respChatChunk(`{}`, "stop") + "\n\n")
		in.WriteString("data: " + `{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"u","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}` + "\n\n")
		in.WriteString("data: [DONE]\n\n")
		return in
	}
	var out1, out2 bytes.Buffer
	in1 := mk()
	if rerr := tr.Translate(&in1, &out1, nil); rerr != nil {
		t.Fatalf("first translate: %v", rerr)
	}
	in2 := mk()
	if rerr := tr.Translate(&in2, &out2, nil); rerr != nil {
		t.Fatalf("reused translator must start fresh, got %v", rerr)
	}
	seq1 := strings.Count(out1.String(), "sequence_number")
	seq2 := strings.Count(out2.String(), "sequence_number")
	if seq1 == 0 || seq1 != seq2 {
		t.Fatalf("reuse must reproduce the full sequence: %d vs %d", seq1, seq2)
	}
}

// --- Length truncates to incomplete ---

func TestResponsesEncoderLengthIncomplete(t *testing.T) {
	events, rerr := runResponsesStream(t, []string{
		respChatChunk(`{"content":"partial"}`, ""),
		respChatChunk(`{}`, "length"),
		respChatUsageChunk(),
		"[DONE]",
	})
	if rerr != nil {
		t.Fatalf("translate failed: %v", rerr)
	}
	last := events[len(events)-1]
	if last["type"] != "response.incomplete" {
		t.Fatalf("length must end incomplete, got %v", last["type"])
	}
	resp := last["response"].(map[string]any)
	if resp["incomplete_details"].(map[string]any)["reason"] != "max_output_tokens" {
		t.Fatalf("bad incomplete reason: %v", resp)
	}
}
