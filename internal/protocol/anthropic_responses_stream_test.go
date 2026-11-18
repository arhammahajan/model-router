package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

// r2aSSEFrame renders one Responses SSE frame; the translator keys off the
// data payload's type member.
func r2aSSEFrame(payload map[string]any) string {
	data, _ := json.Marshal(payload)
	return "data: " + string(data) + "\n\n"
}

func r2aCreated(seq int) string {
	return r2aSSEFrame(map[string]any{"type": "response.created", "sequence_number": seq,
		"response": map[string]any{"id": "resp_up", "object": "response", "model": "up-model"}})
}

func r2aTextItemAdded(seq, outIdx int) string {
	return r2aSSEFrame(map[string]any{"type": "response.output_item.added", "sequence_number": seq,
		"output_index": outIdx,
		"item":         map[string]any{"type": "message", "id": "msg_1", "status": "in_progress", "role": "assistant", "content": []any{}}})
}

func r2aPartAdded(seq, outIdx int) string {
	return r2aSSEFrame(map[string]any{"type": "response.content_part.added", "sequence_number": seq,
		"output_index": outIdx, "item_id": "msg_1", "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}, "logprobs": []any{}}})
}

func r2aTextDelta(seq, outIdx int, delta string) string {
	return r2aSSEFrame(map[string]any{"type": "response.output_text.delta", "sequence_number": seq,
		"output_index": outIdx, "item_id": "msg_1", "content_index": 0,
		"delta": delta, "logprobs": []any{}})
}

func r2aTextDone(seq, outIdx int, text string) string {
	return r2aSSEFrame(map[string]any{"type": "response.output_text.done", "sequence_number": seq,
		"output_index": outIdx, "item_id": "msg_1",
		"text": text, "logprobs": []any{}})
}

func r2aPartDone(seq, outIdx int, text string) string {
	return r2aSSEFrame(map[string]any{"type": "response.content_part.done", "sequence_number": seq,
		"output_index": outIdx, "item_id": "msg_1", "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}, "logprobs": []any{}}})
}

func r2aTextItemDone(seq, outIdx int, text string) string {
	return r2aSSEFrame(map[string]any{"type": "response.output_item.done", "sequence_number": seq,
		"output_index": outIdx,
		"item": map[string]any{"type": "message", "id": "msg_1", "status": "completed", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}, "logprobs": []any{}}}}})
}

func r2aToolItemAdded(seq, outIdx int, itemID, callID, name string) string {
	return r2aSSEFrame(map[string]any{"type": "response.output_item.added", "sequence_number": seq,
		"output_index": outIdx,
		"item": map[string]any{"type": "function_call", "id": itemID, "call_id": callID,
			"name": name, "arguments": "", "status": "in_progress"}})
}

func r2aArgsDelta(seq, outIdx int, itemID, delta string) string {
	return r2aSSEFrame(map[string]any{"type": "response.function_call_arguments.delta", "sequence_number": seq,
		"output_index": outIdx, "item_id": itemID, "delta": delta})
}

func r2aArgsDone(seq, outIdx int, itemID, args string) string {
	return r2aSSEFrame(map[string]any{"type": "response.function_call_arguments.done", "sequence_number": seq,
		"output_index": outIdx, "item_id": itemID, "arguments": args})
}

func r2aToolItemDone(seq, outIdx int, itemID, callID, name, args string) string {
	return r2aSSEFrame(map[string]any{"type": "response.output_item.done", "sequence_number": seq,
		"output_index": outIdx,
		"item": map[string]any{"type": "function_call", "id": itemID, "call_id": callID,
			"name": name, "arguments": args, "status": "completed"}})
}

func r2aTerminal(seq int, incomplete bool, output []any, usage map[string]any) string {
	typ := "response.completed"
	status := "completed"
	resp := map[string]any{"id": "resp_up", "object": "response", "model": "up-model",
		"status": status, "error": nil, "output": output, "usage": usage}
	if incomplete {
		typ = "response.incomplete"
		status = "incomplete"
		resp["status"] = status
		resp["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	return r2aSSEFrame(map[string]any{"type": typ, "sequence_number": seq, "response": resp})
}

func r2aUsage() map[string]any {
	return map[string]any{"input_tokens": 100, "output_tokens": 20, "total_tokens": 120,
		"input_tokens_details": map[string]any{"cached_tokens": 30}}
}

func r2aTextOutput(text string) []any {
	return []any{map[string]any{"type": "message", "id": "msg_1", "status": "completed", "role": "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}, "logprobs": []any{}}}}}
}

type r2aDownEvent struct {
	name string
	data map[string]any
}

func parseR2AEvents(t *testing.T, out string) []r2aDownEvent {
	t.Helper()
	var evs []r2aDownEvent
	for _, frame := range strings.Split(out, "\n\n") {
		frame = strings.TrimSpace(frame)
		if frame == "" {
			continue
		}
		var name, dataStr string
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "event: ") {
				name = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				dataStr = strings.TrimPrefix(line, "data: ")
			}
		}
		if name == "" || dataStr == "" {
			t.Fatalf("bad downstream frame: %q", frame)
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(dataStr), &data); err != nil {
			t.Fatal(err)
		}
		if data["type"] != name {
			t.Fatalf("downstream event %q has no matching payload type", name)
		}
		evs = append(evs, r2aDownEvent{name, data})
	}
	return evs
}

func r2aEventNames(evs []r2aDownEvent) []string {
	var names []string
	for _, e := range evs {
		names = append(names, e.name)
	}
	return names
}

func runR2AStream(t *testing.T, in string, lim Limits) (string, *RouterError) {
	t.Helper()
	tr := &ResponsesToAnthropicSSETranslator{PublicModel: "public-model", Limits: lim}
	var buf bytes.Buffer
	rerr := tr.Translate(strings.NewReader(in), &buf, func() {})
	return buf.String(), rerr
}

func TestR2AStreamTextLifecycle(t *testing.T) {
	in := r2aCreated(0) + r2aTextItemAdded(1, 0) + r2aPartAdded(2, 0) +
		r2aTextDelta(3, 0, "Hel") + r2aTextDelta(4, 0, "lo") +
		r2aTextDone(5, 0, "Hello") + r2aPartDone(6, 0, "Hello") +
		r2aTextItemDone(7, 0, "Hello") +
		r2aTerminal(8, false, r2aTextOutput("Hello"), r2aUsage())
	out, rerr := runR2AStream(t, in, DefaultLimits())
	if rerr != nil {
		t.Fatalf("translate: %+v", rerr)
	}
	evs := parseR2AEvents(t, out)
	want := []string{"message_start", "content_block_start", "content_block_delta",
		"content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if strings.Join(r2aEventNames(evs), ",") != strings.Join(want, ",") {
		t.Fatalf("bad lifecycle: %v", r2aEventNames(evs))
	}
	// Start carries usage zeros as documented placeholders. The shared
	// Anthropic encoder omits zero cache buckets (unified zero-bucket
	// policy: encodeAnthropicUsage only emits creation/read when >0), so
	// the Responses row placeholder is the 2-key zero form; absent cache
	// keys read as zero, never as unknown.
	startUsage := evs[0].data["message"].(map[string]any)["usage"].(map[string]any)
	for _, k := range []string{"input_tokens", "output_tokens"} {
		if startUsage[k] != float64(0) {
			t.Fatalf("start usage must be zero placeholder: %v", startUsage)
		}
	}
	for _, k := range []string{"cache_creation_input_tokens", "cache_read_input_tokens"} {
		if v, ok := startUsage[k]; ok && v != float64(0) {
			t.Fatalf("start usage must be zero placeholder: %v", startUsage)
		}
	}
	// Terminal delta carries the authoritative totals: 100-30=70.
	delta := evs[5].data
	if delta["delta"].(map[string]any)["stop_reason"] != "end_turn" {
		t.Fatalf("bad delta stop: %v", delta)
	}
	usage := delta["usage"].(map[string]any)
	if usage["input_tokens"] != float64(70) || usage["cache_read_input_tokens"] != float64(30) {
		t.Fatalf("bad terminal usage: %v", usage)
	}
	if evs[1].data["index"] != float64(0) || evs[2].data["index"] != float64(0) {
		t.Fatalf("bad block index: %v %v", evs[1].data, evs[2].data)
	}
	if evs[2].data["delta"].(map[string]any)["text"] != "Hel" {
		t.Fatalf("bad delta text: %v", evs[2].data)
	}
}

func TestR2AStreamToolFirst(t *testing.T) {
	in := r2aCreated(0) + r2aToolItemAdded(1, 0, "fc_1", "call_1", "get_time") +
		r2aArgsDelta(2, 0, "fc_1", `{"tz":`) + r2aArgsDelta(3, 0, "fc_1", `"UTC"}`) +
		r2aArgsDone(4, 0, "fc_1", `{"tz":"UTC"}`) +
		r2aToolItemDone(5, 0, "fc_1", "call_1", "get_time", `{"tz": "UTC"}`) +
		r2aTextItemAdded(6, 1) + r2aPartAdded(7, 1) + r2aTextDelta(8, 1, "done") +
		r2aTextDone(9, 1, "done") + r2aPartDone(10, 1, "done") +
		r2aTextItemDone(11, 1, "done") +
		r2aTerminal(12, false, append([]any{map[string]any{
			"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "get_time",
			"arguments": `{"tz":"UTC"}`, "status": "completed"}}, r2aTextOutput("done")...), r2aUsage())
	out, rerr := runR2AStream(t, in, DefaultLimits())
	if rerr != nil {
		t.Fatalf("translate: %+v", rerr)
	}
	evs := parseR2AEvents(t, out)
	// Tool-first keeps distinct indexes: tool 0, text 1.
	if evs[1].name != "content_block_start" || evs[1].data["index"] != float64(0) {
		t.Fatalf("tool must start block 0: %v %v", evs[1].name, evs[1].data)
	}
	block := evs[1].data["content_block"].(map[string]any)
	if block["type"] != "tool_use" || block["id"] != "call_1" {
		t.Fatalf("tool identity must be call_id: %v", block)
	}
	var textStart int = -1
	for i, e := range evs {
		if e.name == "content_block_start" && e.data["content_block"].(map[string]any)["type"] == "text" {
			textStart = i
		}
	}
	if textStart < 0 || evs[textStart].data["index"] != float64(1) {
		t.Fatalf("text must start block 1: %v", r2aEventNames(evs))
	}
	last := evs[len(evs)-2]
	if last.name != "message_delta" || last.data["delta"].(map[string]any)["stop_reason"] != "tool_use" {
		t.Fatalf("bad terminal: %v", last)
	}
}

func TestR2AStreamEmptyArgs(t *testing.T) {
	in := r2aCreated(0) + r2aToolItemAdded(1, 0, "fc_1", "call_9", "noop") +
		r2aArgsDone(2, 0, "fc_1", "") +
		r2aToolItemDone(3, 0, "fc_1", "call_9", "noop", "") +
		r2aTerminal(4, false, []any{map[string]any{
			"type": "function_call", "id": "fc_1", "call_id": "call_9", "name": "noop",
			"arguments": "", "status": "completed"}}, r2aUsage())
	out, rerr := runR2AStream(t, in, DefaultLimits())
	if rerr != nil {
		t.Fatalf("translate: %+v", rerr)
	}
	evs := parseR2AEvents(t, out)
	if evs[len(evs)-2].data["delta"].(map[string]any)["stop_reason"] != "tool_use" {
		t.Fatalf("empty-args tool must complete: %v", r2aEventNames(evs))
	}
}

func TestR2AStreamUnrepresentable(t *testing.T) {
	base := r2aCreated(0) + r2aTextItemAdded(1, 0)
	cases := []string{
		base + r2aSSEFrame(map[string]any{"type": "response.refusal.delta", "sequence_number": 2,
			"output_index": 0, "item_id": "msg_1", "delta": "no"}),
		base + r2aSSEFrame(map[string]any{"type": "response.reasoning_summary_text.delta", "sequence_number": 2}),
		base + r2aSSEFrame(map[string]any{"type": "response.mcp_call_arguments.delta", "sequence_number": 2}),
		base + r2aSSEFrame(map[string]any{"type": "response.future_widget", "sequence_number": 2}),
		base + r2aSSEFrame(map[string]any{"type": "response.failed", "sequence_number": 2}),
		base + r2aSSEFrame(map[string]any{"type": "error", "sequence_number": 2, "message": "boom"}),
	}
	for i, in := range cases {
		out, rerr := runR2AStream(t, in, DefaultLimits())
		if rerr == nil || rerr.Status != 502 {
			t.Fatalf("case %d: expected 502, got %v (%s)", i, rerr, out)
		}
		if strings.Contains(out, "message_stop") {
			t.Fatalf("case %d: no success marker on failure", i)
		}
	}
}

func TestR2AStreamTruncation(t *testing.T) {
	full := r2aCreated(0) + r2aTextItemAdded(1, 0) + r2aPartAdded(2, 0) +
		r2aTextDelta(3, 0, "Hi") + r2aTextDone(4, 0, "Hi") + r2aPartDone(5, 0, "Hi") +
		r2aTextItemDone(6, 0, "Hi")
	// EOF before terminal.
	if out, rerr := runR2AStream(t, full, DefaultLimits()); rerr == nil {
		t.Fatalf("truncated stream must fail: %s", out)
	} else if strings.Contains(out, "message_stop") {
		t.Fatalf("no success marker on truncation")
	}
	// Terminal without usage.
	noUsage := full + r2aSSEFrame(map[string]any{"type": "response.completed", "sequence_number": 7,
		"response": map[string]any{"id": "resp_up", "status": "completed", "error": nil,
			"output": r2aTextOutput("Hi")}})
	if _, rerr := runR2AStream(t, noUsage, DefaultLimits()); rerr == nil {
		t.Fatalf("missing usage must fail")
	}
	// Unfinished args: completed without arguments.done/item validation.
	unfin := r2aCreated(0) + r2aToolItemAdded(1, 0, "fc_1", "call_1", "t") +
		r2aArgsDelta(2, 0, "fc_1", `{"a":`) +
		r2aSSEFrame(map[string]any{"type": "response.completed", "sequence_number": 3,
			"response": map[string]any{"id": "resp_up", "status": "completed", "error": nil,
				"output": []any{map[string]any{"type": "function_call", "id": "fc_1",
					"call_id": "call_1", "name": "t", "arguments": `{"a":1}`, "status": "completed"}},
				"usage": r2aUsage()}})
	if _, rerr := runR2AStream(t, unfin, DefaultLimits()); rerr == nil {
		t.Fatalf("unfinished args must fail")
	}
	// Inconsistent terminal text.
	badTerm := full + r2aTerminal(7, false, r2aTextOutput("different"), r2aUsage())
	if _, rerr := runR2AStream(t, badTerm, DefaultLimits()); rerr == nil {
		t.Fatalf("inconsistent terminal must fail")
	}
	// Unknown stop on incomplete.
	badStop := r2aSSEFrame(map[string]any{"type": "response.incomplete", "sequence_number": 7,
		"response": map[string]any{"id": "resp_up", "status": "incomplete", "error": nil,
			"incomplete_details": map[string]any{"reason": "content_filter"},
			"output":             r2aTextOutput("Hi"), "usage": r2aUsage()}})
	if _, rerr := runR2AStream(t, full+badStop, DefaultLimits()); rerr == nil {
		t.Fatalf("content_filter stop must fail")
	}
}

func TestR2AStreamIdentity(t *testing.T) {
	// Conflicting response identities.
	in := r2aCreated(0) + r2aSSEFrame(map[string]any{"type": "response.in_progress",
		"sequence_number": 1, "response": map[string]any{"id": "resp_other"}})
	if _, rerr := runR2AStream(t, in, DefaultLimits()); rerr == nil {
		t.Fatalf("conflicting response id must fail")
	}
	// Duplicate call identity.
	in2 := r2aCreated(0) + r2aToolItemAdded(1, 0, "fc_1", "call_1", "t") +
		r2aToolItemAdded(2, 1, "fc_2", "call_1", "t")
	if _, rerr := runR2AStream(t, in2, DefaultLimits()); rerr == nil {
		t.Fatalf("duplicate call id must fail")
	}
	// Sequence replay.
	in3 := r2aCreated(0) + r2aCreated(0)
	if _, rerr := runR2AStream(t, in3, DefaultLimits()); rerr == nil {
		t.Fatalf("sequence replay must fail")
	}
	// Duplicate stream start.
	in4 := r2aCreated(0) + r2aSSEFrame(map[string]any{"type": "response.created",
		"sequence_number": 1, "response": map[string]any{"id": "resp_up"}})
	if _, rerr := runR2AStream(t, in4, DefaultLimits()); rerr == nil {
		t.Fatalf("duplicate start must fail")
	}
}

func TestR2AStreamIncomplete(t *testing.T) {
	// Full official lifecycle: item.added → part.added → delta →
	// text.done → part.done → item.done → incomplete terminal. Legacy
	// sparse fixtures omitted part.added/part.done; the source decoder
	// opens the semantic block at content_part.added (BlockOpen), so
	// deltas without it contradict the stream.
	in := r2aCreated(0) + r2aTextItemAdded(1, 0) + r2aPartAdded(2, 0) + r2aTextDelta(3, 0, "par") +
		r2aTextDone(4, 0, "par") + r2aPartDone(5, 0, "par") + r2aTextItemDone(6, 0, "par") +
		r2aTerminal(7, true, r2aTextOutput("par"), r2aUsage())
	out, rerr := runR2AStream(t, in, DefaultLimits())
	if rerr != nil {
		t.Fatalf("translate: %+v", rerr)
	}
	evs := parseR2AEvents(t, out)
	if evs[len(evs)-2].data["delta"].(map[string]any)["stop_reason"] != "max_tokens" {
		t.Fatalf("bad incomplete stop: %v", evs[len(evs)-2].data)
	}
}

func TestR2AStreamBounds(t *testing.T) {
	// Per-frame bound.
	small := DefaultLimits()
	small.MaxFrameBytes = 64
	if _, rerr := runR2AStream(t, r2aCreated(0), small); rerr == nil {
		t.Fatalf("oversize frame must fail")
	}
	// Per-arg bound.
	smallArgs := DefaultLimits()
	smallArgs.MaxArgsChars = 4
	in := r2aCreated(0) + r2aToolItemAdded(1, 0, "fc_1", "call_1", "t") +
		r2aArgsDelta(2, 0, "fc_1", `{"ab":12}`)
	if _, rerr := runR2AStream(t, in, smallArgs); rerr == nil {
		t.Fatalf("oversize args must fail")
	}
	// Item bound.
	smallItems := DefaultLimits()
	smallItems.MaxToolCalls = 1
	in2 := r2aCreated(0) + r2aToolItemAdded(1, 0, "fc_1", "call_1", "t") +
		r2aToolItemAdded(2, 1, "fc_2", "call_2", "t")
	if _, rerr := runR2AStream(t, in2, smallItems); rerr == nil {
		t.Fatalf("oversize item count must fail")
	}
}

type r2aFailWriter struct {
	failAfter int
	written   int
	buf       bytes.Buffer
}

func (w *r2aFailWriter) Write(p []byte) (int, error) {
	if w.written+len(p) > w.failAfter {
		return 0, errors.New("downstream gone")
	}
	w.written += len(p)
	return w.buf.Write(p)
}

type r2aCountReader struct {
	chunks []string
	idx    int
	reads  int
}

func (r *r2aCountReader) Read(p []byte) (int, error) {
	r.reads++
	if r.idx >= len(r.chunks) {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[r.idx])
	r.idx++
	return n, nil
}

func TestR2AStreamWriteFailureStopsReads(t *testing.T) {
	in := r2aCreated(0) + r2aTextItemAdded(1, 0) + r2aPartAdded(2, 0) + r2aTextDelta(3, 0, "Hello") +
		r2aTextDone(4, 0, "Hello") + r2aPartDone(5, 0, "Hello") + r2aTextItemDone(6, 0, "Hello") +
		r2aTerminal(7, false, r2aTextOutput("Hello"), r2aUsage())
	var frames []string
	for _, f := range strings.SplitAfter(in, "\n\n") {
		if strings.TrimSpace(f) != "" {
			frames = append(frames, f)
		}
	}
	rdr := &r2aCountReader{chunks: frames}
	w := &r2aFailWriter{failAfter: 10}
	tr := &ResponsesToAnthropicSSETranslator{PublicModel: "public-model", Limits: DefaultLimits()}
	rerr := tr.Translate(rdr, w, func() {})
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("write failure must fail: %v", rerr)
	}
	// The first downstream write fails, so upstream reads must stop
	// immediately instead of draining the stream. The semantic block
	// opens at content_part.added (BlockOpen emits message_start +
	// content_block_start there; item.added alone emits nothing), so the
	// valid prefix consumes 3 frames (created, item.added, part.added)
	// before the first write fails — not 2 as in the legacy sparse
	// lifecycle where the first delta triggered the start.
	if rdr.idx > 3 {
		t.Fatalf("reads continued after write failure: %d frames consumed", rdr.idx)
	}
	// No second attempt may emit a success marker after the failure.
	if strings.Contains(w.buf.String(), "message_stop") {
		t.Fatalf("no success marker after write failure")
	}
}

func TestR2AStreamNoReadPastTerminal(t *testing.T) {
	terminal := r2aCreated(0) + r2aTextItemAdded(1, 0) + r2aPartAdded(2, 0) + r2aTextDelta(3, 0, "Hi") +
		r2aTextDone(4, 0, "Hi") + r2aPartDone(5, 0, "Hi") + r2aTextItemDone(6, 0, "Hi") +
		r2aTerminal(7, false, r2aTextOutput("Hi"), r2aUsage())
	trailing := "data: {not valid json}\n\n"
	// Each Read delivers one frame; the translator must return right after
	// the terminal without processing the next generation's bytes (which
	// are deliberately invalid: reading them would fail the translation).
	var frames []string
	for _, f := range strings.SplitAfter(terminal, "\n\n") {
		if strings.TrimSpace(f) != "" {
			frames = append(frames, f)
		}
	}
	rdr := &r2aCountReader{chunks: append(frames, trailing)}
	var buf bytes.Buffer
	tr := &ResponsesToAnthropicSSETranslator{PublicModel: "public-model", Limits: DefaultLimits()}
	if rerr := tr.Translate(rdr, &buf, func() {}); rerr != nil {
		t.Fatalf("translate: %+v", rerr)
	}
	if rdr.idx != len(frames) {
		t.Fatalf("read past terminal: consumed %d of %d frames", rdr.idx, len(frames)+1)
	}
}

func r2aTerminalWithOutput(seq int, output []any, usage map[string]any, mutateResp func(map[string]any)) string {
	typ := "response.completed"
	resp := map[string]any{"id": "resp_up", "object": "response", "model": "up-model",
		"status": "completed", "error": nil, "output": output, "usage": usage}
	if mutateResp != nil {
		mutateResp(resp)
	}
	data, _ := json.Marshal(map[string]any{"type": typ, "sequence_number": seq, "response": resp})
	return "data: " + string(data) + "\n\n"
}

func TestR2AStreamTerminalIdentityMismatch(t *testing.T) {
	base := func(output []any) string {
		return r2aCreated(0) + r2aTextItemAdded(1, 0) + r2aPartAdded(2, 0) +
			r2aTextDelta(3, 0, "Hello") + r2aTextDone(4, 0, "Hello") + r2aPartDone(5, 0, "Hello") +
			r2aTextItemDone(6, 0, "Hello")
	}
	// Mutated terminal id: msg_1 -> changed must fail without success marker.
	changedID := []any{map[string]any{"type": "message", "id": "changed", "status": "completed", "role": "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": "Hello", "annotations": []any{}, "logprobs": []any{}}}}}
	if out, rerr := runR2AStream(t, base(nil)+r2aTerminal(7, false, changedID, r2aUsage()), DefaultLimits()); rerr == nil || rerr.Status != 502 {
		t.Fatalf("mutated terminal id must fail 502, got %v (%s)", rerr, out)
	} else if strings.Contains(out, "message_stop") {
		t.Fatalf("mutated id must not emit success marker")
	}
	// Mutated role: assistant -> user must fail.
	userRole := []any{map[string]any{"type": "message", "id": "msg_1", "status": "completed", "role": "user",
		"content": []any{map[string]any{"type": "output_text", "text": "Hello", "annotations": []any{}, "logprobs": []any{}}}}}
	if out, rerr := runR2AStream(t, base(nil)+r2aTerminal(7, false, userRole, r2aUsage()), DefaultLimits()); rerr == nil || rerr.Status != 502 {
		t.Fatalf("mutated terminal role must fail 502, got %v (%s)", rerr, out)
	} else if strings.Contains(out, "message_stop") {
		t.Fatalf("mutated role must not emit success marker")
	}
	// Nonempty logprobs in terminal part must fail.
	lpText := []any{map[string]any{"type": "message", "id": "msg_1", "status": "completed", "role": "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": "Hello", "annotations": []any{}, "logprobs": []any{map[string]any{"x": 1}}}}}}
	if out, rerr := runR2AStream(t, base(nil)+r2aTerminal(7, false, lpText, r2aUsage()), DefaultLimits()); rerr == nil || rerr.Status != 502 {
		t.Fatalf("terminal logprobs must fail 502, got %v (%s)", rerr, out)
	} else if strings.Contains(out, "message_stop") {
		t.Fatalf("terminal logprobs must not emit success marker")
	}
	// Unknown terminal envelope feature must fail (canonical validator).
	unknownEnv := base(nil) + r2aTerminalWithOutput(7, r2aTextOutput("Hello"), r2aUsage(), func(resp map[string]any) {
		resp["background"] = true
	})
	if out, rerr := runR2AStream(t, unknownEnv, DefaultLimits()); rerr == nil || rerr.Status != 502 {
		t.Fatalf("unknown terminal envelope must fail 502, got %v (%s)", rerr, out)
	} else if strings.Contains(out, "message_stop") {
		t.Fatalf("unknown envelope must not emit success marker")
	}
	// Namespaced tool semantics in terminal must fail.
	nsTool := []any{map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "t",
		"arguments": "{}", "status": "completed", "namespace": "ns"}}
	toolBase := r2aCreated(0) + r2aToolItemAdded(1, 0, "fc_1", "call_1", "t") +
		r2aArgsDone(2, 0, "fc_1", "{}") + r2aToolItemDone(3, 0, "fc_1", "call_1", "t", "{}")
	if out, rerr := runR2AStream(t, toolBase+r2aTerminal(4, false, nsTool, r2aUsage()), DefaultLimits()); rerr == nil || rerr.Status != 502 {
		t.Fatalf("namespaced terminal tool must fail 502, got %v (%s)", rerr, out)
	} else if strings.Contains(out, "message_stop") {
		t.Fatalf("namespaced tool must not emit success marker")
	}
}

func TestR2AStreamLifecycleGuards(t *testing.T) {
	full := func() string {
		return r2aCreated(0) + r2aTextItemAdded(1, 0) + r2aPartAdded(2, 0) +
			r2aTextDelta(3, 0, "Hi") + r2aTextDone(4, 0, "Hi") + r2aPartDone(5, 0, "Hi") +
			r2aTextItemDone(6, 0, "Hi") + r2aTerminal(7, false, r2aTextOutput("Hi"), r2aUsage())
	}
	// Missing initial created must fail.
	noCreated := r2aTextItemAdded(0, 0) + r2aTerminal(1, false, r2aTextOutput("Hi"), r2aUsage())
	if _, rerr := runR2AStream(t, noCreated, DefaultLimits()); rerr == nil || rerr.Status != 502 {
		t.Fatalf("missing created must fail, got %v", rerr)
	}
	// Duplicate created must fail.
	if _, rerr := runR2AStream(t, r2aCreated(0)+r2aCreated(1)+full()[len(r2aCreated(0)):], DefaultLimits()); rerr == nil {
		t.Fatalf("duplicate created must fail")
	}
	// Duplicate output_text.done must fail.
	dupDone := r2aCreated(0) + r2aTextItemAdded(1, 0) + r2aTextDelta(2, 0, "Hi") +
		r2aTextDone(3, 0, "Hi") + r2aTextDone(4, 0, "Hi")
	if _, rerr := runR2AStream(t, dupDone, DefaultLimits()); rerr == nil {
		t.Fatalf("duplicate text done must fail")
	}
	// Delta after done must fail.
	afterDone := r2aCreated(0) + r2aTextItemAdded(1, 0) + r2aTextDelta(2, 0, "Hi") +
		r2aTextDone(3, 0, "Hi") + r2aTextDelta(4, 0, "more")
	if _, rerr := runR2AStream(t, afterDone, DefaultLimits()); rerr == nil {
		t.Fatalf("delta after done must fail")
	}
	// Unknown event member must fail even in an otherwise valid stream.
	unknownFull := r2aCreated(0) + r2aTextItemAdded(1, 0) + r2aPartAdded(2, 0) +
		r2aSSEFrame(map[string]any{"type": "response.output_text.delta", "sequence_number": 3,
			"output_index": 0, "item_id": "msg_1", "delta": "Hi", "mystery": 1}) +
		r2aTextDone(4, 0, "Hi") + r2aPartDone(5, 0, "Hi") +
		r2aTextItemDone(6, 0, "Hi") + r2aTerminal(7, false, r2aTextOutput("Hi"), r2aUsage())
	if out, rerr := runR2AStream(t, unknownFull, DefaultLimits()); rerr == nil || rerr.Status != 502 {
		t.Fatalf("unknown event member must fail, got %v (%s)", rerr, out)
	}
	// Named SSE event must match data.type; mismatch fails, data-only passes.
	// Full valid stream with one mismatched named event must fail: the text
	// hash matches (delta Hi + done Hi) so only the event/type mismatch
	// distinguishes it.
	namedMismatchFull := r2aCreated(0) + r2aTextItemAdded(1, 0) + r2aPartAdded(2, 0) +
		r2aTextDelta(3, 0, "Hi") +
		"event: response.output_text.delta\n" + r2aSSEFrame(map[string]any{
		"type": "response.output_text.done", "sequence_number": 4,
		"output_index": 0, "item_id": "msg_1", "text": "Hi", "logprobs": []any{}}) +
		r2aPartDone(5, 0, "Hi") + r2aTextItemDone(6, 0, "Hi") +
		r2aTerminal(7, false, r2aTextOutput("Hi"), r2aUsage())
	if _, rerr := runR2AStream(t, namedMismatchFull, DefaultLimits()); rerr == nil {
		t.Fatalf("event/type mismatch must fail")
	}
	_ = full
}
