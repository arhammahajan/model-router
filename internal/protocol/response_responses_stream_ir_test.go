// Responses source stream IR tests: direct frame -> semantic traces.
//
// These tests drive NewResponsesStreamDecoder directly, independent of any
// target: every assertion pins source-wire validation (envelopes,
// allowlists, finish and usage gates, identity rules) without calling the
// three legacy pair translators. Wire shapes mirror the legacy fixtures'
// reviewed contracts (chat_responses_stream.go,
// responses_to_anthropic_stream_*.go, responses_to_gemini_stream*.go).
package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// --- Independent wire builders (not shared with pair tests) ---------------

func rsJ(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// rsF builds an SSE frame: empty event means data-only (no event name).
func rsF(event, data string) Frame {
	if event == "" {
		return Frame{Data: data}
	}
	return Frame{Event: event, HasEvent: true, Data: data}
}

func rsSeq(seq int) map[string]any {
	if seq < 0 {
		return nil
	}
	return map[string]any{"sequence_number": seq}
}

func rsWithSeq(m map[string]any, seq int) map[string]any {
	if seq >= 0 {
		m["sequence_number"] = seq
	}
	return m
}

// rsCreatedRaw builds created with explicit id/model (empty means omit).
func rsCreatedRaw(t *testing.T, seq int, id, model string) string {
	t.Helper()
	resp := map[string]any{"object": "response", "created_at": 1700000000, "status": "in_progress"}
	if id != "" {
		resp["id"] = id
	}
	if model != "" {
		resp["model"] = model
	}
	return rsJ(t, rsWithSeq(map[string]any{
		"type": "response.created", "response": resp,
	}, seq))
}

func rsMsgAdded(t *testing.T, seq, outIdx int, itemID string) string {
	t.Helper()
	return rsJ(t, rsWithSeq(map[string]any{
		"type": "response.output_item.added", "output_index": outIdx,
		"item": map[string]any{
			"type": "message", "id": itemID,
			"status": "in_progress", "role": "assistant", "content": []any{},
		},
	}, seq))
}

func rsToolAdded(t *testing.T, seq, outIdx int, itemID, callID, name, seed string) string {
	t.Helper()
	item := map[string]any{
		"type": "function_call", "id": itemID,
		"call_id": callID, "name": name,
		"status": "in_progress",
	}
	if seed != "" {
		item["arguments"] = seed
	}
	return rsJ(t, rsWithSeq(map[string]any{
		"type": "response.output_item.added", "output_index": outIdx, "item": item,
	}, seq))
}

func rsPartAdded(t *testing.T, seq, outIdx int, itemID, ptype string) string {
	t.Helper()
	part := map[string]any{"type": ptype}
	if ptype == "output_text" {
		part["text"] = ""
		part["annotations"] = []any{}
		part["logprobs"] = []any{}
	} else {
		part["refusal"] = ""
	}
	return rsJ(t, rsWithSeq(map[string]any{
		"type": "response.content_part.added", "output_index": outIdx,
		"item_id": itemID, "content_index": 0, "part": part,
	}, seq))
}

func rsDelta(t *testing.T, seq, outIdx int, itemID, delta string, refusal bool) string {
	t.Helper()
	typ := "response.output_text.delta"
	if refusal {
		typ = "response.refusal.delta"
	}
	return rsJ(t, rsWithSeq(map[string]any{
		"type": typ, "output_index": outIdx,
		"item_id": itemID, "content_index": 0, "delta": delta,
	}, seq))
}

func rsDone(t *testing.T, seq, outIdx int, itemID, text string, refusal bool) string {
	t.Helper()
	typ, key := "response.output_text.done", "text"
	if refusal {
		typ, key = "response.refusal.done", "refusal"
	}
	return rsJ(t, rsWithSeq(map[string]any{
		"type": typ, "output_index": outIdx,
		"item_id": itemID, "content_index": 0, key: text,
	}, seq))
}

func rsArgsDelta(t *testing.T, seq, outIdx int, itemID, frag string) string {
	t.Helper()
	return rsJ(t, rsWithSeq(map[string]any{
		"type": "response.function_call_arguments.delta", "output_index": outIdx,
		"item_id": itemID, "delta": frag,
	}, seq))
}

func rsArgsDone(t *testing.T, seq, outIdx int, itemID, args string) string {
	t.Helper()
	return rsJ(t, rsWithSeq(map[string]any{
		"type": "response.function_call_arguments.done", "output_index": outIdx,
		"item_id": itemID, "arguments": args,
	}, seq))
}

func rsPartDone(t *testing.T, seq, outIdx int, itemID, text string, refusal bool) string {
	t.Helper()
	part := map[string]any{"type": "output_text", "text": text}
	if refusal {
		part = map[string]any{"type": "refusal", "refusal": text}
	} else {
		part["annotations"] = []any{}
		part["logprobs"] = []any{}
	}
	return rsJ(t, rsWithSeq(map[string]any{
		"type": "response.content_part.done", "output_index": outIdx,
		"item_id": itemID, "content_index": 0, "part": part,
	}, seq))
}

func rsItemDoneMsg(t *testing.T, seq, outIdx int, itemID, text string, refusal bool) string {
	t.Helper()
	content := []any{}
	if text != "" {
		if refusal {
			content = append(content, map[string]any{"type": "refusal", "refusal": text})
		} else {
			content = append(content, map[string]any{
				"type": "output_text", "text": text,
				"annotations": []any{}, "logprobs": []any{},
			})
		}
	}
	return rsJ(t, rsWithSeq(map[string]any{
		"type": "response.output_item.done", "output_index": outIdx,
		"item": map[string]any{
			"type": "message", "id": itemID, "status": "completed",
			"role": "assistant", "content": content,
		},
	}, seq))
}

func rsItemDoneTool(t *testing.T, seq, outIdx int, itemID, callID, name, args string) string {
	t.Helper()
	return rsJ(t, rsWithSeq(map[string]any{
		"type": "response.output_item.done", "output_index": outIdx,
		"item": map[string]any{
			"type": "function_call", "id": itemID,
			"call_id": callID, "name": name, "arguments": args,
			"status": "completed",
		},
	}, seq))
}

func rsMsgOut(id, text string) map[string]any {
	return map[string]any{
		"type": "message", "id": id,
		"status": "completed", "role": "assistant",
		"content": []any{map[string]any{
			"type": "output_text", "text": text,
			"annotations": []any{}, "logprobs": []any{},
		}},
	}
}

func rsToolOut(id, callID, name, args string) map[string]any {
	return map[string]any{
		"type": "function_call", "id": id,
		"call_id": callID, "name": name,
		"arguments": args, "status": "completed",
	}
}

func rsUsage(prompt, completion int) map[string]any {
	return map[string]any{
		"input_tokens": prompt, "output_tokens": completion,
		"total_tokens": prompt + completion,
	}
}

func rsResp(t *testing.T, status string, output []any, usage map[string]any, reason string) string {
	t.Helper()
	return rsRespID(t, "resp_7", "gpt-4o", status, output, usage, reason)
}

func rsRespID(t *testing.T, id, model, status string, output []any, usage map[string]any, reason string) string {
	t.Helper()
	resp := map[string]any{
		"id": id, "object": "response",
		"created_at": 1700000000, "model": model,
		"status": status, "error": nil,
		"output": output, "usage": usage,
	}
	if status == "incomplete" {
		resp["incomplete_details"] = map[string]any{"reason": reason}
	}
	return rsJ(t, resp)
}

func rsTerminal(t *testing.T, seq int, kind, resp string) string {
	t.Helper()
	var raw json.RawMessage
	if err := json.Unmarshal([]byte(resp), &raw); err != nil {
		t.Fatalf("resp: %v", err)
	}
	return rsJ(t, rsWithSeq(map[string]any{
		"type": "response." + kind, "response": raw,
	}, seq))
}

// rsFullSuccess decodes a frame sequence and validates Close-before-success.
func rsFullSuccess(t *testing.T, frames []Frame) []ResponseEvent {
	t.Helper()
	dec := NewResponsesStreamDecoder(DefaultLimits())
	var all []ResponseEvent
	for i, f := range frames {
		evs, rerr := dec.DecodeFrame(f)
		if rerr != nil {
			t.Fatalf("DecodeFrame[%d]: %v", i, rerr)
		}
		// Message BlockOpens emit at part-added; surface them in order.
		all = append(all, evs...)
	}
	tail, rerr := dec.Close()
	if rerr != nil {
		t.Fatalf("Close: %v", rerr)
	}
	if len(tail) != 0 {
		t.Fatalf("Close returned %d events, want none", len(tail))
	}
	return all
}

// rsMustFailAt asserts DecodeFrame itself fails at the exact frame index.
func rsMustFailAt(t *testing.T, frames []Frame, at int) *RouterError {
	t.Helper()
	dec := NewResponsesStreamDecoder(DefaultLimits())
	for i, f := range frames {
		_, rerr := dec.DecodeFrame(f)
		if rerr != nil {
			if i != at {
				t.Fatalf("failure at frame %d, want guard at frame %d (%v)", i, at, rerr)
			}
			if rerr.Status != 502 {
				t.Fatalf("frame %d: status=%d, want 502 (%v)", i, rerr.Status, rerr)
			}
			return rerr
		}
	}
	t.Fatalf("expected 502 at frame %d, all frames accepted", at)
	return nil
}

// rsMustFail asserts a 502 somewhere in the sequence or at Close.
func rsMustFail(t *testing.T, frames []Frame) *RouterError {
	t.Helper()
	dec := NewResponsesStreamDecoder(DefaultLimits())
	for i, f := range frames {
		if _, rerr := dec.DecodeFrame(f); rerr != nil {
			if rerr.Status != 502 {
				t.Fatalf("frame %d: status=%d, want 502 (%v)", i, rerr.Status, rerr)
			}
			return rerr
		}
	}
	if _, rerr := dec.Close(); rerr != nil {
		if rerr.Status != 502 {
			t.Fatalf("Close: status=%d, want 502 (%v)", rerr.Status, rerr)
		}
		return rerr
	}
	t.Fatalf("expected 502 failure, stream accepted")
	return nil
}

func rsTextStream(t *testing.T, seq0 int, text, doneText string) []Frame {
	t.Helper()
	usage := rsUsage(25, 15)
	resp := rsResp(t, "completed", []any{rsMsgOut("msg_1", doneText)}, usage, "")
	return []Frame{
		rsF("response.created", rsCreatedRaw(t, seq0, "resp_7", "gpt-4o")),
		rsF("response.output_item.added", rsMsgAdded(t, seq0+1, 0, "msg_1")),
		rsF("response.content_part.added", rsPartAdded(t, seq0+2, 0, "msg_1", "output_text")),
		rsF("response.output_text.delta", rsDelta(t, seq0+3, 0, "msg_1", text, false)),
		rsF("response.output_text.done", rsDone(t, seq0+4, 0, "msg_1", doneText, false)),
		rsF("response.content_part.done", rsPartDone(t, seq0+5, 0, "msg_1", doneText, false)),
		rsF("response.output_item.done", rsItemDoneMsg(t, seq0+6, 0, "msg_1", doneText, false)),
		rsF("response.completed", rsTerminal(t, seq0+7, "completed", resp)),
	}
}

func rsStart(t *testing.T, ev ResponseEvent) ResponseStart {
	t.Helper()
	start, ok := ev.(ResponseStart)
	if !ok {
		t.Fatalf("first event = %T, want ResponseStart", ev)
	}
	if start.Source != DialectResponses || string(start.ID) != "resp_7" || start.Model != "gpt-4o" {
		t.Fatalf("start provenance wrong: %+v", start)
	}
	return start
}

// --- Text trace: start first, immediate deltas, terminal order ---------

func TestResponsesStreamIRTextTrace(t *testing.T) {
	evs := rsFullSuccess(t, rsTextStream(t, 0, "hello", "hello"))
	if len(evs) != 6 {
		t.Fatalf("events = %d, want 6: %#v", len(evs), evs)
	}
	rsStart(t, evs[0])
	open, ok := evs[1].(BlockOpen)
	if !ok || open.Item != "msg_1" || open.Kind != BlockText {
		t.Fatalf("ev[1] = %#v, want text BlockOpen msg_1", evs[1])
	}
	if open.SourceIndex == nil || *open.SourceIndex != 0 {
		t.Fatalf("text BlockOpen must carry output_index 0: %+v", open)
	}
	if frag, ok := evs[2].(TextFragment); !ok || frag.Item != "msg_1" || frag.Text != "hello" {
		t.Fatalf("ev[2] = %#v, want TextFragment hello", evs[2])
	}
	if close, ok := evs[3].(BlockClose); !ok || close.Item != "msg_1" {
		t.Fatalf("ev[3] = %#v, want BlockClose msg_1", evs[3])
	}
	usage, ok := evs[4].(UsageEvent)
	if !ok || !usage.Terminal() {
		t.Fatalf("ev[4] = %#v, want terminal UsageEvent", evs[4])
	}
	if *usage.Usage.Prompt != 25 || *usage.Usage.Completion != 15 || *usage.Usage.Total != 40 {
		t.Fatalf("usage totals wrong: %+v", usage.Usage)
	}
	if usage.Usage.Provenance.Source != DialectResponses {
		t.Fatalf("usage provenance wrong: %+v", usage.Usage.Provenance)
	}
	complete, ok := evs[5].(CompletionEvent)
	if !ok || complete.Finish.Kind != FinishStop || complete.Finish.Raw != "completed" {
		t.Fatalf("ev[5] = %#v, want stop CompletionEvent", evs[5])
	}
	// The trace validates coherently through the shared lifecycle.
	life := NewResponseLifecycle(DefaultLimits(), DialectResponses)
	for _, ev := range evs {
		if rerr := life.Observe(ev); rerr != nil {
			t.Fatalf("lifecycle Observe(%T): %v", ev, rerr)
		}
	}
	res, rerr := life.Result()
	if rerr != nil {
		t.Fatalf("lifecycle Result: %v", rerr)
	}
	if len(res.Blocks) != 1 || res.Blocks[0].Kind != BlockText || res.Blocks[0].SourceIndex == nil || *res.Blocks[0].SourceIndex != 0 {
		t.Fatalf("blocks = %+v, want one text block with output_index 0", res.Blocks)
	}
}

func TestResponsesStreamIRStartFirst(t *testing.T) {
	dec := NewResponsesStreamDecoder(DefaultLimits())
	if _, rerr := dec.Close(); rerr == nil {
		t.Fatal("Close without any frame accepted (truncation must fail)")
	}
	// Content before created fails.
	dec2 := NewResponsesStreamDecoder(DefaultLimits())
	if _, rerr := dec2.DecodeFrame(rsF("response.output_text.delta", rsDelta(t, 0, 0, "msg_1", "hi", false))); rerr == nil {
		t.Fatal("delta before created accepted")
	}
	// [DONE] without a valid terminal is a truncation.
	dec3 := NewResponsesStreamDecoder(DefaultLimits())
	if _, rerr := dec3.DecodeFrame(Frame{Data: "[DONE]"}); rerr == nil {
		t.Fatal("[DONE] without terminal accepted")
	}
	// Empty data-only frame is malformed.
	dec4 := NewResponsesStreamDecoder(DefaultLimits())
	if _, rerr := dec4.DecodeFrame(Frame{Data: ""}); rerr == nil {
		t.Fatal("empty data-only frame accepted")
	}
	// response.failed as the first event reports the upstream error.
	rerr := rsMustFailAt(t, []Frame{
		rsF("response.failed", rsJ(t, map[string]any{"type": "response.failed", "sequence_number": 0})),
	}, 0)
	if !strings.Contains(rerr.Msg, "reported an error") {
		t.Fatalf("failed event error = %q", rerr.Msg)
	}
	// Duplicate created fails at the second frame.
	rerr = rsMustFailAt(t, []Frame{
		rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
		rsF("response.created", rsCreatedRaw(t, 1, "resp_7", "gpt-4o")),
	}, 1)
	if !strings.Contains(rerr.Msg, "duplicate") {
		t.Fatalf("duplicate created error = %q", rerr.Msg)
	}
	// Created without a model preserves truthful absence (like the
	// accepted Anthropic/Gemini seam): the provider omits model at
	// created and reports it later in a progress envelope or the actual
	// terminal response object via ResponseModelEvent. Rationale: prior
	// review (r2cCreated/r2gCreated wire evidence: created carries only
	// id+status/id) proves real Responses senders omit model at start,
	// and a start-time model requirement forced either fixture
	// fabrication or a whole-generation buffer. Supplied-empty or
	// non-string models still fail at the created frame below.
	decAbs := NewResponsesStreamDecoder(DefaultLimits())
	evs, rerr := decAbs.DecodeFrame(rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "")))
	if rerr != nil {
		t.Fatalf("absent created model must start truthfully, got %v", rerr)
	}
	if len(evs) != 1 {
		t.Fatalf("absent created model events = %d, want 1", len(evs))
	}
	start, ok := evs[0].(ResponseStart)
	if !ok || start.Model != "" || string(start.ID) != "resp_7" {
		t.Fatalf("absent created start = %#v, want empty-model ResponseStart", evs[0])
	}
	// Supplied-empty model (present member, empty string) stays a 502:
	// "" here is a wire value, not omission.
	decEmpty := NewResponsesStreamDecoder(DefaultLimits())
	createdEmpty := strings.Replace(rsCreatedRaw(t, 0, "resp_7", "x"), `"model":"x"`, `"model":""`, 1)
	if _, rerr := decEmpty.DecodeFrame(rsF("response.created", createdEmpty)); rerr == nil || rerr.Status != 502 {
		t.Fatalf("supplied-empty created model must fail with 502, got %v", rerr)
	}
}

// --- Named event / data.type coherence, data-only support -----------

func TestResponsesStreamIREventCoherence(t *testing.T) {
	// Explicit mismatch between the SSE event name and data.type fails and
	// never falls back.
	rerr := rsMustFailAt(t, []Frame{
		rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
		rsF("response.output_text.delta", rsDelta(t, 1, 0, "msg_1", "hi", false)),
	}, 1)
	_ = rerr
	mismatch := rsJ(t, map[string]any{
		"type": "response.refusal.delta", "output_index": 0,
		"item_id": "msg_1", "content_index": 0, "delta": "x",
	})
	rerr = rsMustFail(t, []Frame{
		rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
		rsF("response.output_item.added", rsMsgAdded(t, 1, 0, "msg_1")),
		rsF("response.content_part.added", rsPartAdded(t, 2, 0, "msg_1", "output_text")),
		rsF("response.output_text.delta", mismatch),
	})
	if rerr == nil {
		t.Fatal("event/type mismatch accepted")
	}
	// A malformed data.type cannot fall back to the event name.
	badType := `{"type": 7, "output_index": 0, "item_id": "msg_1", "delta": "x"}`
	rsMustFail(t, []Frame{
		rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
		rsF("response.output_text.delta", badType),
	})
	// Data-only frames (no event name) work per the existing contracts.
	evs := rsFullSuccess(t, []Frame{
		rsF("", rsCreatedRaw(t, -1, "resp_7", "gpt-4o")),
		rsF("", rsMsgAdded(t, -1, 0, "msg_1")),
		rsF("", rsPartAdded(t, -1, 0, "msg_1", "output_text")),
		rsF("", rsDelta(t, -1, 0, "msg_1", "hi", false)),
		rsF("", rsDone(t, -1, 0, "msg_1", "hi", false)),
		rsF("", rsPartDone(t, -1, 0, "msg_1", "hi", false)),
		rsF("", rsItemDoneMsg(t, -1, 0, "msg_1", "hi", false)),
		rsF("", rsTerminal(t, -1, "completed", rsResp(t, "completed", []any{rsMsgOut("msg_1", "hi")}, rsUsage(2, 1), ""))),
	})
	if _, ok := evs[len(evs)-1].(CompletionEvent); !ok {
		t.Fatalf("data-only stream terminal = %#v", evs[len(evs)-1])
	}
	// Neither event nor type is a missing-event failure.
	rsMustFail(t, []Frame{
		rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
		rsF("", `{"sequence_number": 1}`),
	})
}

// --- Sequence coherence ---------------------------------------------

func TestResponsesStreamIRSequence(t *testing.T) {
	mk := func(seqs []int) []Frame {
		frames := []Frame{
			rsF("response.created", rsCreatedRaw(t, seqs[0], "resp_7", "gpt-4o")),
			rsF("response.output_item.added", rsMsgAdded(t, seqs[1], 0, "msg_1")),
			rsF("response.content_part.added", rsPartAdded(t, seqs[2], 0, "msg_1", "output_text")),
		}
		return frames
	}
	// Reordered or repeated sequence numbers fail at the offending frame.
	rsMustFailAt(t, mk([]int{5, 3, 4}), 1)
	rsMustFailAt(t, mk([]int{0, 1, 1}), 2)
	// Fractions are not exact JSON integers.
	badSeq := strings.Replace(rsCreatedRaw(t, 0, "resp_7", "gpt-4o"), `"sequence_number":0`, `"sequence_number":1.5`, 1)
	rsMustFailAt(t, []Frame{rsF("response.created", badSeq)}, 0)
	// Absent sequence numbers pass (senders may omit them).
	evs := rsFullSuccess(t, []Frame{
		rsF("response.created", rsCreatedRaw(t, -1, "resp_7", "gpt-4o")),
		rsF("response.output_item.added", rsMsgAdded(t, -1, 0, "msg_1")),
		rsF("response.content_part.added", rsPartAdded(t, -1, 0, "msg_1", "output_text")),
		rsF("response.output_text.delta", rsDelta(t, -1, 0, "msg_1", "hi", false)),
		rsF("response.output_text.done", rsDone(t, -1, 0, "msg_1", "hi", false)),
		rsF("response.content_part.done", rsPartDone(t, -1, 0, "msg_1", "hi", false)),
		rsF("response.output_item.done", rsItemDoneMsg(t, -1, 0, "msg_1", "hi", false)),
		rsF("response.completed", rsTerminal(t, -1, "completed", rsResp(t, "completed", []any{rsMsgOut("msg_1", "hi")}, rsUsage(2, 1), ""))),
	})
	rsStart(t, evs[0])
}

// --- Unknown members: null-tolerant, non-null fails value-free --------

func TestResponsesStreamIRUnknownFields(t *testing.T) {
	// Unknown null members are newer defaults and pass.
	evs := rsFullSuccess(t, []Frame{
		rsF("response.created", strings.Replace(rsCreatedRaw(t, 0, "resp_7", "gpt-4o"), `"status":`, `"future":null,"status":`, 1)),
		rsF("response.output_item.added", rsMsgAdded(t, 1, 0, "msg_1")),
		rsF("response.content_part.added", rsPartAdded(t, 2, 0, "msg_1", "output_text")),
		rsF("response.output_text.delta", rsDelta(t, 3, 0, "msg_1", "hi", false)),
		rsF("response.output_text.done", rsDone(t, 4, 0, "msg_1", "hi", false)),
		rsF("response.content_part.done", rsPartDone(t, 5, 0, "msg_1", "hi", false)),
		rsF("response.output_item.done", rsItemDoneMsg(t, 6, 0, "msg_1", "hi", false)),
		rsF("response.completed", rsTerminal(t, 7, "completed", rsResp(t, "completed", []any{rsMsgOut("msg_1", "hi")}, rsUsage(2, 1), ""))),
	})
	if _, ok := evs[len(evs)-1].(CompletionEvent); !ok {
		t.Fatalf("null-unknown stream terminal = %#v", evs[len(evs)-1])
	}
	// Unknown non-null members fail with a value-free feature path.
	created := rsCreatedRaw(t, 0, "resp_7", "gpt-4o")
	rerr := rsMustFail(t, []Frame{rsF("response.created",
		strings.Replace(created, `"status":`, `"future":1,"status":`, 1))})
	if !strings.Contains(rerr.Msg, "has no representation") {
		t.Fatalf("unknown member must name the feature path: %v", rerr)
	}
	rerr = rsMustFail(t, []Frame{
		rsF("response.created", created),
		rsF("response.output_item.added",
			strings.Replace(rsMsgAdded(t, 1, 0, "msg_1"), `"content":`, `"future":true,"content":`, 1)),
	})
	if !strings.Contains(rerr.Msg, "has no representation") {
		t.Fatalf("unknown member must name the feature path: %v", rerr)
	}
	// Unknown event types fail closed naming the event (a vendor
	// vocabulary name, never content); unknown FINISH values stay fully
	// static per the value-free finish precedent.
	rerr2 := rsMustFail(t, []Frame{
		rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
		rsF("response.mcp_call.delta", rsJ(t, map[string]any{"type": "response.mcp_call.delta", "sequence_number": 1})),
	})
	if !strings.Contains(rerr2.Msg, "has no representation") {
		t.Fatalf("unknown event must be unrepresentable: %v", rerr2)
	}
	// Output reasoning items fail closed (scalar reasoning travels in
	// usage; reasoning state never enters the IR).
	reasoningAdded := rsJ(t, map[string]any{
		"type": "response.output_item.added", "sequence_number": 1, "output_index": 0,
		"item": map[string]any{"type": "reasoning", "id": "rs_1", "status": "in_progress"},
	})
	rerr = rsMustFail(t, []Frame{
		rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
		rsF("response.output_item.added", reasoningAdded),
	})
	if !strings.Contains(rerr.Msg, "has no representation") {
		t.Fatalf("reasoning item must be unrepresentable: %v", rerr)
	}
}

// --- Finish mapping: completed/incomplete reasons, failed never success -

func TestResponsesStreamIRFinishMapping(t *testing.T) {
	for _, c := range []struct {
		kind   string
		status string
		reason string
		want   FinishKind
		raw    string
	}{
		{"completed", "completed", "", FinishStop, "completed"},
		{"incomplete", "incomplete", "max_output_tokens", FinishLength, "max_output_tokens"},
		{"incomplete", "incomplete", "content_filter", FinishContentFilter, "content_filter"},
	} {
		usage := rsUsage(3, 2)
		resp := rsResp(t, c.status, []any{rsMsgOut("msg_1", "hi")}, usage, c.reason)
		frames := []Frame{
			rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
			rsF("response.output_item.added", rsMsgAdded(t, 1, 0, "msg_1")),
			rsF("response.content_part.added", rsPartAdded(t, 2, 0, "msg_1", "output_text")),
			rsF("response.output_text.delta", rsDelta(t, 3, 0, "msg_1", "hi", false)),
			rsF("response.output_text.done", rsDone(t, 4, 0, "msg_1", "hi", false)),
			rsF("response.content_part.done", rsPartDone(t, 5, 0, "msg_1", "hi", false)),
			rsF("response.output_item.done", rsItemDoneMsg(t, 6, 0, "msg_1", "hi", false)),
			rsF("response."+c.kind, rsTerminal(t, 7, c.kind, resp)),
		}
		evs := rsFullSuccess(t, frames)
		complete, ok := evs[len(evs)-1].(CompletionEvent)
		if !ok || complete.Finish.Kind != c.want || complete.Finish.Raw != c.raw {
			t.Fatalf("kind %s = %#v, want %s/%s", c.kind, evs[len(evs)-1], c.want, c.raw)
		}
	}
	// Unknown incomplete reasons fail value-free.
	usage := rsUsage(3, 2)
	resp := rsResp(t, "incomplete", []any{rsMsgOut("msg_1", "hi")}, usage, "quantum")
	secret := "private-upstream-incomplete-reason"
	respSecret := rsResp(t, "incomplete", []any{rsMsgOut("msg_1", "hi")}, usage, secret)
	for _, bad := range []string{resp, respSecret} {
		rerr := rsMustFail(t, []Frame{
			rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
			rsF("response.output_item.added", rsMsgAdded(t, 1, 0, "msg_1")),
			rsF("response.content_part.added", rsPartAdded(t, 2, 0, "msg_1", "output_text")),
			rsF("response.output_text.delta", rsDelta(t, 3, 0, "msg_1", "hi", false)),
			rsF("response.output_text.done", rsDone(t, 4, 0, "msg_1", "hi", false)),
			rsF("response.content_part.done", rsPartDone(t, 5, 0, "msg_1", "hi", false)),
			rsF("response.output_item.done", rsItemDoneMsg(t, 6, 0, "msg_1", "hi", false)),
			rsF("response.incomplete", rsTerminal(t, 7, "incomplete", bad)),
		})
		if strings.Contains(rerr.Msg, secret) {
			t.Fatalf("incomplete reason echoes upstream value: %q", rerr.Msg)
		}
		if !strings.Contains(rerr.Msg, "has no representation") {
			t.Fatalf("unknown incomplete reason must be unrepresentable: %v", rerr)
		}
	}
	// Event/status mismatch contradicts the terminal.
	respOK := rsResp(t, "completed", []any{rsMsgOut("msg_1", "hi")}, rsUsage(3, 2), "")
	rsMustFail(t, []Frame{
		rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
		rsF("response.output_item.added", rsMsgAdded(t, 1, 0, "msg_1")),
		rsF("response.content_part.added", rsPartAdded(t, 2, 0, "msg_1", "output_text")),
		rsF("response.output_text.delta", rsDelta(t, 3, 0, "msg_1", "hi", false)),
		rsF("response.output_text.done", rsDone(t, 4, 0, "msg_1", "hi", false)),
		rsF("response.content_part.done", rsPartDone(t, 5, 0, "msg_1", "hi", false)),
		rsF("response.output_item.done", rsItemDoneMsg(t, 6, 0, "msg_1", "hi", false)),
		rsF("response.incomplete", rsTerminal(t, 7, "incomplete", respOK)),
	})
}
