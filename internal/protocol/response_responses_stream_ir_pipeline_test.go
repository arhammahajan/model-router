// Responses source stream IR tests (part 2): tools, malformed provenance,
// identities, drift, bounds, and the shared pipeline.
//
// These tests drive NewResponsesStreamDecoder directly except for the final
// pipeline section, which replays decoder traces through TranslateStream
// with the shared fake encoder plus the reusable Responses target encoder.
package protocol

import (
	"bytes"
	"strings"
	"testing"
)

// --- Parallel tools: exact bytes, bigint, empty args -----------------

func TestResponsesStreamIRToolsParallelBigintEmpty(t *testing.T) {
	big := `{"n":9007199254740993}`
	usage := rsUsage(30, 20)
	resp := rsResp(t, "completed", []any{
		rsMsgOut("msg_1", ""),
		rsToolOut("fc_1", "call_A", "get_time", `{"tz":"UTC"}`),
		rsToolOut("fc_2", "call_B", "calc", `{"n":9007199254740993}`),
	}, usage, "")
	frames := []Frame{
		rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
		rsF("response.output_item.added", rsMsgAdded(t, 1, 0, "msg_1")),
		rsF("response.output_item.added", rsToolAdded(t, 2, 1, "fc_1", "call_A", "get_time", "")),
		rsF("response.output_item.added", rsToolAdded(t, 3, 2, "fc_2", "call_B", "calc", "")),
		rsF("response.content_part.added", rsPartAdded(t, 4, 0, "msg_1", "output_text")),
		rsF("response.output_text.done", rsDone(t, 5, 0, "msg_1", "", false)),
		rsF("response.content_part.done", rsPartDone(t, 6, 0, "msg_1", "", false)),
		rsF("response.output_item.done", rsItemDoneMsg(t, 7, 0, "msg_1", "", false)),
		rsF("response.function_call_arguments.delta", rsArgsDelta(t, 8, 1, "fc_1", `{"tz":`)),
		rsF("response.function_call_arguments.delta", rsArgsDelta(t, 9, 2, "fc_2", `{"n":`)),
		rsF("response.function_call_arguments.delta", rsArgsDelta(t, 10, 1, "fc_1", `"UTC"}`)),
		rsF("response.function_call_arguments.delta", rsArgsDelta(t, 11, 2, "fc_2", `9007199254740993}`)),
		rsF("response.function_call_arguments.done", rsArgsDone(t, 12, 1, "fc_1", `{"tz":"UTC"}`)),
		rsF("response.function_call_arguments.done", rsArgsDone(t, 13, 2, "fc_2", big)),
		rsF("response.output_item.done", rsItemDoneTool(t, 14, 1, "fc_1", "call_A", "get_time", `{"tz":"UTC"}`)),
		rsF("response.output_item.done", rsItemDoneTool(t, 15, 2, "fc_2", "call_B", "calc", big)),
		rsF("response.completed", rsTerminal(t, 16, "completed", resp)),
	}
	evs := rsFullSuccess(t, frames)
	// Arrival order is the canonical order and must be sorted by
	// output_index: tool index 1 opens before tool index 2. A reordered
	// arrival is refused as unrepresentable order (see the review
	// tests), never sorted or normalized.
	var kinds []string
	for _, ev := range evs {
		switch e := ev.(type) {
		case BlockOpen:
			kinds = append(kinds, "open:"+string(e.Item))
		case ArgsFragment:
			kinds = append(kinds, "args:"+string(e.Item)+":"+e.Fragment)
		case BlockClose:
			kinds = append(kinds, "close:"+string(e.Item))
		}
	}
	want := []string{
		"open:fc_1", "open:fc_2", "open:msg_1", "close:msg_1",
		`args:fc_1:{"tz":`, `args:fc_2:{"n":`,
		`args:fc_1:"UTC"}`, `args:fc_2:9007199254740993}`,
		"close:fc_1", "close:fc_2",
	}
	if len(kinds) != len(want) {
		t.Fatalf("tool event trace = %q, want %q", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("tool event trace = %q, want %q", kinds, want)
		}
	}
	// Tool blocks carry the logical output_index explicitly; the item and
	// call identities stay opaque and distinct.
	var opens []BlockOpen
	for _, ev := range evs {
		if e, ok := ev.(BlockOpen); ok && e.Kind == BlockTool {
			opens = append(opens, e)
		}
	}
	if len(opens) != 2 || opens[0].Item != "fc_1" || opens[0].SourceIndex == nil || *opens[0].SourceIndex != 1 ||
		string(opens[0].Call) != "call_A" || opens[1].SourceIndex == nil || *opens[1].SourceIndex != 2 {
		t.Fatalf("tool opens = %+v, want indices [1 2]", opens)
	}
	// Bigint arguments survive exactly through the terminal compare.
	complete, ok := evs[len(evs)-1].(CompletionEvent)
	if !ok || complete.Finish.Kind != FinishStop {
		t.Fatalf("terminal = %#v, want stop completion", evs[len(evs)-1])
	}
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
	if len(res.Blocks) != 3 || res.Blocks[0].SourceIndex == nil || *res.Blocks[0].SourceIndex != 1 {
		t.Fatalf("summary blocks = %+v, want tool index 1 first", res.Blocks)
	}
}

func TestResponsesStreamIREmptyArgs(t *testing.T) {
	// Source-absent arguments canonicalize to "{}" exactly once: the
	// block opens, carries one "{}" fragment, and still closes.
	usage := rsUsage(5, 2)
	resp := rsResp(t, "completed", []any{
		rsToolOut("fc_1", "to_1", "ping", "{}"),
	}, usage, "")
	frames := []Frame{
		rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
		rsF("response.output_item.added", rsToolAdded(t, 1, 0, "fc_1", "to_1", "ping", "")),
		rsF("response.function_call_arguments.done", rsArgsDone(t, 2, 0, "fc_1", "")),
		rsF("response.output_item.done", rsItemDoneTool(t, 3, 0, "fc_1", "to_1", "ping", "")),
		rsF("response.completed", rsTerminal(t, 4, "completed", resp)),
	}
	evs := rsFullSuccess(t, frames)
	var opens, args, closes int
	var fragText string
	for _, ev := range evs {
		switch e := ev.(type) {
		case BlockOpen:
			opens++
		case ArgsFragment:
			args++
			fragText = e.Fragment
		case BlockClose:
			closes++
		}
	}
	if opens != 1 || closes != 1 || args != 1 || fragText != "{}" {
		t.Fatalf("empty-arg tool trace: opens=%d args=%d closes=%d frag=%q (want 1/1/1 with canonical {})", opens, args, closes, fragText)
	}
	// The canonical fragment counts in both the per-call and aggregate
	// args bounds: a one-byte aggregate rejects the two-byte fallback.
	lim := DefaultLimits()
	lim.MaxStreamArgsChars = 1
	dec := NewResponsesStreamDecoder(lim)
	for i, f := range frames {
		_, rerr := dec.DecodeFrame(f)
		if rerr != nil {
			if i != 3 {
				t.Fatalf("empty-args bound must fail at item.done, failed at frame %d: %v", i, rerr)
			}
			return
		}
	}
	t.Fatal("empty args bypassed one-byte aggregate")
}

// --- Malformed tools are typed provenance, not 502 --------------------

func TestResponsesStreamIRMalformedClassified(t *testing.T) {
	cases := []struct {
		name     string
		toolName string
		argsFrag string
		termArgs string
	}{
		{"truncated", "a", `{"a":`, `{"a":`},
		{"array", "a", `[1]`, `[1]`},
		{"scalar", "a", `"s"`, `"s"`},
		{"badname", "has space", `{}`, `{}`},
	}
	for _, c := range cases {
		usage := rsUsage(3, 2)
		// The terminal carries the model output verbatim in output_index
		// order (position 0 holds output_index 0); the shared validator
		// classifies the malformed call and omits it.
		termOut := []any{
			rsToolOut("fc_1", "c1", c.toolName, c.termArgs),
			map[string]any{
				"type": "message", "id": "msg_9", "status": "completed", "role": "assistant",
				"content": []any{map[string]any{
					"type": "output_text", "text": "hi",
					"annotations": []any{}, "logprobs": []any{},
				}},
			},
		}
		resp := rsResp(t, "completed", termOut, usage, "")
		frames := []Frame{
			rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
			rsF("response.output_item.added", rsToolAdded(t, 1, 0, "fc_1", "c1", c.toolName, "")),
			rsF("response.function_call_arguments.delta", rsArgsDelta(t, 2, 0, "fc_1", c.argsFrag)),
			rsF("response.function_call_arguments.done", rsArgsDone(t, 3, 0, "fc_1", c.argsFrag)),
			rsF("response.output_item.added", rsMsgAdded(t, 4, 1, "msg_9")),
			rsF("response.content_part.added", rsPartAdded(t, 5, 1, "msg_9", "output_text")),
			rsF("response.output_text.delta", rsDelta(t, 6, 1, "msg_9", "hi", false)),
			rsF("response.output_text.done", rsDone(t, 7, 1, "msg_9", "hi", false)),
			rsF("response.content_part.done", rsPartDone(t, 8, 1, "msg_9", "hi", false)),
			rsF("response.output_item.done", rsItemDoneMsg(t, 9, 1, "msg_9", "hi", false)),
			rsF("response.output_item.done", rsItemDoneTool(t, 10, 0, "fc_1", "c1", c.toolName, c.argsFrag)),
			rsF("response.completed", rsTerminal(t, 11, "completed", resp)),
		}
		evs := rsFullSuccess(t, frames)
		var malformed []MalformedToolEvent
		for _, ev := range evs {
			if e, ok := ev.(MalformedToolEvent); ok {
				malformed = append(malformed, e)
			}
		}
		if len(malformed) != 1 || malformed[0].Item != "fc_1" {
			t.Fatalf("%s: MalformedToolEvent = %+v, want one for fc_1", c.name, malformed)
		}
		if _, ok := evs[len(evs)-1].(CompletionEvent); !ok {
			t.Fatalf("%s: terminal = %#v, want completion (malformed is provenance, not failure)", c.name, evs[len(evs)-1])
		}
		life := NewResponseLifecycle(DefaultLimits(), DialectResponses)
		for _, ev := range evs {
			if rerr := life.Observe(ev); rerr != nil {
				t.Fatalf("%s: lifecycle Observe(%T): %v", c.name, ev, rerr)
			}
		}
		res, rerr := life.Result()
		if rerr != nil {
			t.Fatalf("%s: lifecycle Result: %v", c.name, rerr)
		}
		if res.MalformedTools != 1 {
			t.Fatalf("%s: MalformedTools = %d, want 1", c.name, res.MalformedTools)
		}
	}
	// Identity failures stay 502s, never malformed provenance: a
	// missing name cannot open a block.
	rsMustFailAt(t, []Frame{
		rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
		rsF("response.output_item.added", rsJ(t, map[string]any{
			"type": "response.output_item.added", "sequence_number": 1, "output_index": 0,
			"item": map[string]any{
				"type": "function_call", "id": "fc_1",
				"call_id": "c1", "status": "in_progress",
			},
		})),
	}, 1)
}

// --- Identities: opaque, stable, distinct namespaces ------------------

func TestResponsesStreamIRIdentityRules(t *testing.T) {
	// Same string may name an item and a call: namespaces are distinct.
	usage := rsUsage(5, 2)
	resp := rsResp(t, "completed", []any{
		rsToolOut("shared", "shared", "f", "{}"),
	}, usage, "")
	evs := rsFullSuccess(t, []Frame{
		rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
		rsF("response.output_item.added", rsToolAdded(t, 1, 0, "shared", "shared", "f", "")),
		rsF("response.function_call_arguments.done", rsArgsDone(t, 2, 0, "shared", "")),
		rsF("response.output_item.done", rsItemDoneTool(t, 3, 0, "shared", "shared", "f", "")),
		rsF("response.completed", rsTerminal(t, 4, "completed", resp)),
	})
	if _, ok := evs[len(evs)-1].(CompletionEvent); !ok {
		t.Fatalf("shared item/call identity rejected: %#v", evs[len(evs)-1])
	}
	// Duplicate call ids across blocks conflict at the second added.
	rsMustFailAt(t, []Frame{
		rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
		rsF("response.output_item.added", rsToolAdded(t, 1, 0, "fc_1", "dup", "a", "")),
		rsF("response.output_item.added", rsToolAdded(t, 2, 1, "fc_2", "dup", "b", "")),
	}, 2)
	// Duplicate item ids conflict.
	rsMustFailAt(t, []Frame{
		rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
		rsF("response.output_item.added", rsMsgAdded(t, 1, 0, "dup")),
		rsF("response.output_item.added", rsMsgAdded(t, 2, 1, "dup")),
	}, 2)
	// Duplicate output_index conflicts.
	rsMustFailAt(t, []Frame{
		rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
		rsF("response.output_item.added", rsMsgAdded(t, 1, 0, "msg_1")),
		rsF("response.output_item.added", rsMsgAdded(t, 2, 0, "msg_2")),
	}, 2)
	// Response id stability across created/progress/terminal.
	rsMustFailAt(t, []Frame{
		rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
		rsF("response.in_progress", rsJ(t, map[string]any{
			"type": "response.in_progress", "sequence_number": 1,
			"response": map[string]any{"id": "resp_OTHER", "object": "response"},
		})),
	}, 1)
	// Grammar-invalid names pass the SOURCE (target gate decides).
	evs2 := rsFullSuccess(t, []Frame{
		rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
		rsF("response.output_item.added", rsToolAdded(t, 1, 0, "fc_1", "c1", "has space", "")),
		rsF("response.function_call_arguments.done", rsArgsDone(t, 2, 0, "fc_1", "")),
		rsF("response.output_item.done", rsItemDoneTool(t, 3, 0, "fc_1", "c1", "has space", "")),
		rsF("response.completed", rsTerminal(t, 4, "completed", rsResp(t, "completed", []any{
			rsToolOut("fc_1", "c1", "has space", `{}`),
		}, rsUsage(5, 2), ""))),
	})
	found := false
	for _, ev := range evs2 {
		if e, ok := ev.(MalformedToolEvent); ok && e.Item == "fc_1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("grammar-invalid name must classify malformed, not fail: %#v", evs2)
	}
}

// --- Terminal/output drift --------------------------------------------

func TestResponsesStreamIRTerminalDrift(t *testing.T) {
	base := func() []Frame {
		return []Frame{
			rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
			rsF("response.output_item.added", rsMsgAdded(t, 1, 0, "msg_1")),
			rsF("response.content_part.added", rsPartAdded(t, 2, 0, "msg_1", "output_text")),
			rsF("response.output_text.delta", rsDelta(t, 3, 0, "msg_1", "hi", false)),
			rsF("response.output_text.done", rsDone(t, 4, 0, "msg_1", "hi", false)),
			rsF("response.content_part.done", rsPartDone(t, 5, 0, "msg_1", "hi", false)),
			rsF("response.output_item.done", rsItemDoneMsg(t, 6, 0, "msg_1", "hi", false)),
		}
	}
	usage := rsUsage(2, 1)
	// args.done contradicting the accumulation fails at its frame.
	rsMustFailAt(t, []Frame{
		rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
		rsF("response.output_item.added", rsToolAdded(t, 1, 0, "fc_1", "c1", "f", "")),
		rsF("response.function_call_arguments.delta", rsArgsDelta(t, 2, 0, "fc_1", `{"a":1}`)),
		rsF("response.function_call_arguments.done", rsArgsDone(t, 3, 0, "fc_1", `{"a":2}`)),
	}, 3)
	// Terminal text contradicting streamed deltas fails.
	rsMustFail(t, append(base(),
		rsF("response.completed", rsTerminal(t, 7, "completed",
			rsResp(t, "completed", []any{rsMsgOut("msg_1", "bye")}, usage, ""))),
	))
	// Terminal tool args contradicting streamed args fail.
	toolBase := []Frame{
		rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
		rsF("response.output_item.added", rsToolAdded(t, 1, 0, "fc_1", "c1", "f", "")),
		rsF("response.function_call_arguments.delta", rsArgsDelta(t, 2, 0, "fc_1", `{"a":1}`)),
		rsF("response.function_call_arguments.done", rsArgsDone(t, 3, 0, "fc_1", `{"a":1}`)),
		rsF("response.output_item.done", rsItemDoneTool(t, 4, 0, "fc_1", "c1", "f", `{"a":1}`)),
	}
	rsMustFail(t, append(toolBase,
		rsF("response.completed", rsTerminal(t, 5, "completed",
			rsResp(t, "completed", []any{rsToolOut("fc_1", "c1", "f", `{"a":2}`)}, usage, ""))),
	))
	// Terminal response id drift fails.
	idDrift := rsRespID(t, "resp_OTHER", "gpt-4o", "completed", []any{rsMsgOut("msg_1", "hi")}, usage, "")
	rsMustFail(t, append(base(),
		rsF("response.completed", rsTerminal(t, 7, "completed", idDrift)),
	))
	// Terminal model drift fails (ResponseStart carries real provenance).
	driftResp := strings.Replace(
		rsResp(t, "completed", []any{rsMsgOut("msg_1", "hi")}, usage, ""),
		`"model":"gpt-4o"`, `"model":"gpt-4o-mini"`, 1)
	rsMustFail(t, append(base(),
		rsF("response.completed", rsTerminal(t, 7, "completed", driftResp)),
	))
	// Unfinished block (missing output_item.done) fails at the terminal.
	rsMustFail(t, []Frame{
		rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
		rsF("response.output_item.added", rsMsgAdded(t, 1, 0, "msg_1")),
		rsF("response.content_part.added", rsPartAdded(t, 2, 0, "msg_1", "output_text")),
		rsF("response.output_text.delta", rsDelta(t, 3, 0, "msg_1", "hi", false)),
		rsF("response.output_text.done", rsDone(t, 4, 0, "msg_1", "hi", false)),
		rsF("response.content_part.done", rsPartDone(t, 5, 0, "msg_1", "hi", false)),
		rsF("response.completed", rsTerminal(t, 6, "completed",
			rsResp(t, "completed", []any{rsMsgOut("msg_1", "hi")}, usage, ""))),
	})
}

// --- Refusal union + cache/reasoning carry -----------------------------

func TestResponsesStreamIRRefusalAndUsageCarry(t *testing.T) {
	usage := map[string]any{
		"input_tokens": 100, "output_tokens": 20, "total_tokens": 120,
		"input_tokens_details":  map[string]any{"cached_tokens": 10, "cache_write_tokens": 5},
		"output_tokens_details": map[string]any{"reasoning_tokens": 8},
	}
	resp := rsResp(t, "completed", []any{
		rsMsgOut("msg_1", "partial"),
		map[string]any{
			"type": "message", "id": "msg_2", "status": "completed", "role": "assistant",
			"content": []any{map[string]any{"type": "refusal", "refusal": "no"}},
		},
	}, usage, "")
	frames := []Frame{
		rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
		rsF("response.output_item.added", rsMsgAdded(t, 1, 0, "msg_1")),
		rsF("response.output_item.added", rsMsgAdded(t, 2, 1, "msg_2")),
		rsF("response.content_part.added", rsPartAdded(t, 3, 0, "msg_1", "output_text")),
		rsF("response.content_part.added", rsPartAdded(t, 4, 1, "msg_2", "refusal")),
		rsF("response.output_text.delta", rsDelta(t, 5, 0, "msg_1", "partial", false)),
		rsF("response.refusal.delta", rsDelta(t, 6, 1, "msg_2", "no", true)),
		rsF("response.output_text.done", rsDone(t, 7, 0, "msg_1", "partial", false)),
		rsF("response.refusal.done", rsDone(t, 8, 1, "msg_2", "no", true)),
		rsF("response.content_part.done", rsPartDone(t, 9, 0, "msg_1", "partial", false)),
		rsF("response.content_part.done", rsJ(t, map[string]any{
			"type": "response.content_part.done", "sequence_number": 10,
			"output_index": 1, "item_id": "msg_2", "content_index": 0,
			"part": map[string]any{"type": "refusal", "refusal": "no"},
		})),
		rsF("response.output_item.done", rsItemDoneMsg(t, 11, 0, "msg_1", "partial", false)),
		rsF("response.output_item.done", rsItemDoneMsg(t, 12, 1, "msg_2", "no", true)),
		rsF("response.completed", rsTerminal(t, 13, "completed", resp)),
	}
	evs := rsFullSuccess(t, frames)
	var kinds []string
	for _, ev := range evs {
		switch e := ev.(type) {
		case BlockOpen:
			kinds = append(kinds, "open:"+string(e.Kind)+":"+string(e.Item))
		case TextFragment:
			kinds = append(kinds, "frag:"+string(e.Item)+":"+e.Text)
		case BlockClose:
			kinds = append(kinds, "close:"+string(e.Item))
		}
	}
	want := []string{
		"open:text:msg_1", "open:refusal:msg_2",
		"frag:msg_1:partial", "frag:msg_2:no",
		"close:msg_1", "close:msg_2",
	}
	if len(kinds) != len(want) {
		t.Fatalf("refusal trace = %q, want %q", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("refusal trace = %q, want %q", kinds, want)
		}
	}
	var gotUsage ResponseUsage
	for _, ev := range evs {
		if u, ok := ev.(UsageEvent); ok {
			gotUsage = u.Usage
		}
	}
	if gotUsage.CacheRead == nil || *gotUsage.CacheRead != 10 ||
		gotUsage.CacheCreation == nil || *gotUsage.CacheCreation != 5 ||
		gotUsage.Reasoning == nil || *gotUsage.Reasoning != 8 {
		t.Fatalf("scalar accounting not carried: %+v", gotUsage)
	}
	if gotUsage.Provenance.Source != DialectResponses || gotUsage.Provenance.Scope != UsageTerminal {
		t.Fatalf("usage provenance wrong: %+v", gotUsage.Provenance)
	}
}

// --- Bounds: args retention capped, text unbounded ---------------------

func TestResponsesStreamIRBounds(t *testing.T) {
	lim := Limits{MaxArgsChars: 64, MaxStreamArgsChars: 128, MaxToolCalls: 4, MaxStreamBlocks: 4, MaxFrameBytes: 1024}
	dec := NewResponsesStreamDecoder(lim)
	feed := func(f Frame) *RouterError {
		_, rerr := dec.DecodeFrame(f)
		return rerr
	}
	if rerr := feed(rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o"))); rerr != nil {
		t.Fatalf("created: %v", rerr)
	}
	if rerr := feed(rsF("response.output_item.added", rsToolAdded(t, 1, 0, "fc_1", "c1", "f", ""))); rerr != nil {
		t.Fatalf("added: %v", rerr)
	}
	// One fragment over the per-call cap fails.
	if rerr := feed(rsF("response.function_call_arguments.delta", rsArgsDelta(t, 2, 0, "fc_1", strings.Repeat("x", 65)))); rerr == nil {
		t.Fatal("per-call args overflow accepted")
	} else if !strings.Contains(rerr.Msg, "size bound") {
		t.Fatalf("per-call overflow must name the size bound: %v", rerr)
	}
	// Aggregate overflow across retained fragments fails.
	dec2 := NewResponsesStreamDecoder(lim)
	mustFeed := func(f Frame) {
		t.Helper()
		if _, rerr := dec2.DecodeFrame(f); rerr != nil {
			t.Fatalf("setup frame: %v", rerr)
		}
	}
	mustFeed(rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")))
	mustFeed(rsF("response.output_item.added", rsToolAdded(t, 1, 0, "fc_1", "c1", "f", "")))
	mustFeed(rsF("response.output_item.added", rsToolAdded(t, 2, 1, "fc_2", "c2", "g", "")))
	mustFeed(rsF("response.function_call_arguments.delta", rsArgsDelta(t, 3, 0, "fc_1", strings.Repeat("a", 64))))
	mustFeed(rsF("response.function_call_arguments.delta", rsArgsDelta(t, 4, 1, "fc_2", strings.Repeat("b", 64))))
	if _, rerr := dec2.DecodeFrame(rsF("response.function_call_arguments.delta", rsArgsDelta(t, 5, 0, "fc_1", "x"))); rerr == nil {
		t.Fatal("aggregate args overflow accepted")
	}
	// Distinct-block bound: 5 output items with a 4-block limit.
	dec3 := NewResponsesStreamDecoder(lim)
	mustFeed3 := func(f Frame) *RouterError {
		t.Helper()
		_, rerr := dec3.DecodeFrame(f)
		return rerr
	}
	if rerr := mustFeed3(rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o"))); rerr != nil {
		t.Fatalf("created: %v", rerr)
	}
	for i := 0; i < 4; i++ {
		if rerr := mustFeed3(rsF("response.output_item.added", rsMsgAdded(t, i+1, i, "msg_"+string(rune('a'+i))))); rerr != nil {
			t.Fatalf("added %d: %v", i, rerr)
		}
	}
	if rerr := mustFeed3(rsF("response.output_item.added", rsMsgAdded(t, 5, 4, "msg_e"))); rerr == nil {
		t.Fatal("block overflow accepted")
	}
	// Text has no per-block cap: a 900-byte delta passes under the
	// default 8MB frame budget (testRespLimits caps frames at 1024).
	big := strings.Repeat("t", 900)
	evs := rsFullSuccess(t, []Frame{
		rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
		rsF("response.output_item.added", rsMsgAdded(t, 1, 0, "msg_1")),
		rsF("response.content_part.added", rsPartAdded(t, 2, 0, "msg_1", "output_text")),
		rsF("response.output_text.delta", rsDelta(t, 3, 0, "msg_1", big, false)),
		rsF("response.output_text.done", rsDone(t, 4, 0, "msg_1", big, false)),
		rsF("response.content_part.done", rsPartDone(t, 5, 0, "msg_1", big, false)),
		rsF("response.output_item.done", rsItemDoneMsg(t, 6, 0, "msg_1", big, false)),
		rsF("response.completed", rsTerminal(t, 7, "completed", rsResp(t, "completed", []any{rsMsgOut("msg_1", big)}, rsUsage(2, 1), ""))),
	})
	if _, ok := evs[len(evs)-1].(CompletionEvent); !ok {
		t.Fatalf("large-text stream terminal = %#v", evs[len(evs)-1])
	}
}

// --- Decoder close/provenance ------------------------------------------

func TestResponsesStreamIRCloseProvenance(t *testing.T) {
	dec := NewResponsesStreamDecoder(DefaultLimits())
	evs, rerr := dec.DecodeFrame(rsF("response.created", rsCreatedRaw(t, 0, "resp_9", "gpt-4o-mini")))
	if rerr != nil {
		t.Fatalf("created: %v", rerr)
	}
	start := evs[0].(ResponseStart)
	if string(start.ID) != "resp_9" || start.Model != "gpt-4o-mini" || start.Source != DialectResponses {
		t.Fatalf("actual source metadata not preserved: %+v", start)
	}
	// Close reads nothing and emits no extra events: without the
	// terminal it is a truncation error.
	if tail, rerr := dec.Close(); rerr == nil || len(tail) != 0 {
		t.Fatalf("pre-terminal Close = (%v, %v), want truncation, no events", tail, rerr)
	}
	// After the terminal, Close succeeds silently and further frames fail.
	evs = rsFullSuccess(t, rsTextStream(t, 0, "hi", "hi"))
	_ = evs
	dec2 := NewResponsesStreamDecoder(DefaultLimits())
	for _, f := range rsTextStream(t, 0, "hi", "hi") {
		if _, rerr := dec2.DecodeFrame(f); rerr != nil {
			t.Fatalf("setup: %v", rerr)
		}
	}
	if tail, rerr := dec2.Close(); rerr != nil || len(tail) != 0 {
		t.Fatalf("post-terminal Close = (%v, %v), want nil, no events", tail, rerr)
	}
	if _, rerr := dec2.DecodeFrame(rsF("response.created", rsCreatedRaw(t, 99, "resp_7", "gpt-4o"))); rerr == nil {
		t.Fatal("frame after terminal accepted")
	}
}

// --- Shared pipeline: fake encoder + reusable Responses encoder -------

func TestResponsesStreamIRPipelineFakeEncoder(t *testing.T) {
	lim := testRespLimits()
	frames := rsTextStream(t, 0, "hello", "hello")
	var datas []string
	for _, f := range frames {
		datas = append(datas, f.Data)
	}
	// Real SSE framing with named events: the decoder checks coherence.
	var sb strings.Builder
	events := []string{
		"response.created", "response.output_item.added", "response.content_part.added",
		"response.output_text.delta", "response.output_text.done",
		"response.content_part.done", "response.output_item.done", "response.completed",
	}
	for i, d := range datas {
		sb.WriteString("event: " + events[i] + "\ndata: " + d + "\n\n")
	}
	// Trailing bytes are the same HTTP body's unread remainder: the
	// pipeline must not read past the terminal.
	sb.WriteString("trailing-bytes-are-same-body-remainder")
	dec := NewResponsesStreamDecoder(lim)
	enc := &bufferEncoder{}
	var out bytes.Buffer
	rerr := TranslateStream(NewSSEFrameReader(strings.NewReader(sb.String()), lim), dec, enc, &out, nil, lim, DialectResponses)
	if rerr != nil {
		t.Fatalf("TranslateStream: %v", rerr)
	}
	if len(enc.events) != 6 {
		t.Fatalf("events forwarded = %d, want 6", len(enc.events))
	}
	if enc.aborts != 0 {
		t.Error("Abort called on the success path")
	}
}

func TestResponsesStreamIRPipelineResponsesEncoder(t *testing.T) {
	lim := DefaultLimits()
	frames := rsTextStream(t, 0, "hello", "hello")
	var sb strings.Builder
	events := []string{
		"response.created", "response.output_item.added", "response.content_part.added",
		"response.output_text.delta", "response.output_text.done",
		"response.content_part.done", "response.output_item.done", "response.completed",
	}
	for i, f := range frames {
		sb.WriteString("event: " + events[i] + "\ndata: " + f.Data + "\n\n")
	}
	dec := NewResponsesStreamDecoder(lim)
	enc := NewResponsesEventEncoder("gpt-4o", lim)
	var out bytes.Buffer
	rerr := TranslateStream(NewSSEFrameReader(strings.NewReader(sb.String()), lim), dec, enc, &out, nil, lim, DialectResponses)
	if rerr != nil {
		t.Fatalf("TranslateStream: %v", rerr)
	}
	body := out.String()
	for _, want := range []string{
		"event: response.created", "event: response.output_text.delta",
		"event: response.output_item.done", "event: response.completed",
		`"text":"hello"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("encoder output missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "response.failed") {
		t.Fatalf("success path emitted failure:\n%s", body)
	}
}
