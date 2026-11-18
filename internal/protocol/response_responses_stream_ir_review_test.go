// Responses source stream review regressions (root probes, adapted).
//
// Each test reproduces one root executable probe against the staged
// Responses source decoder: the mutation must fail closed with the exact
// classification below. Done-payload builders use actual full source
// shapes (names/arguments/content carried, not omitted), so the guards
// below prove strict validation rather than builder unrealism.
package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// rsMutate decodes one frame map, applies edit, and re-encodes it.
func rsMutate(t *testing.T, f Frame, edit func(map[string]any)) Frame {
	t.Helper()
	var b map[string]any
	if err := json.Unmarshal([]byte(f.Data), &b); err != nil {
		t.Fatal(err)
	}
	edit(b)
	f.Data = rsJ(t, b)
	return f
}

// rsReject asserts the contradictory source stream fails closed.
func rsReject(t *testing.T, fs []Frame) {
	t.Helper()
	d := NewResponsesStreamDecoder(DefaultLimits())
	for _, f := range fs {
		if _, err := d.DecodeFrame(f); err != nil {
			return
		}
	}
	t.Fatal("contradictory source stream accepted")
}

func TestResponsesStreamIRReviewCreatedCore(t *testing.T) {
	f := rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o"))
	f = rsMutate(t, f, func(b map[string]any) {
		b["response"].(map[string]any)["object"] = 17
	})
	rsReject(t, []Frame{f})
	// Isolated guard: the malformed core fails at the created frame.
	dec := NewResponsesStreamDecoder(DefaultLimits())
	if _, rerr := dec.DecodeFrame(f); rerr == nil || rerr.Status != 502 {
		t.Fatalf("created core guard = %v, want 502", rerr)
	}
}

func TestResponsesStreamIRReviewDonePayload(t *testing.T) {
	fs := rsTextStream(t, 0, "hi", "hi")
	fs[6] = rsMutate(t, fs[6], func(b map[string]any) {
		i := b["item"].(map[string]any)
		i["role"] = "user"
		i["private_semantics"] = true
	})
	rsReject(t, fs)
	// Isolated guard: the incoherent done item fails at its own frame.
	dec := NewResponsesStreamDecoder(DefaultLimits())
	for i, f := range fs {
		_, rerr := dec.DecodeFrame(f)
		if rerr != nil {
			if i != 6 || rerr.Status != 502 {
				t.Fatalf("done payload guard failed at frame %d: %v", i, rerr)
			}
			if strings.Contains(strings.ToLower(rerr.Msg), "user") {
				t.Fatalf("done payload diagnostic echoes upstream value: %q", rerr.Msg)
			}
			return
		}
	}
	t.Fatal("incoherent done item accepted")
}

func TestResponsesStreamIRReviewTerminalItemID(t *testing.T) {
	fs := rsTextStream(t, 0, "hi", "hi")
	fs[7] = rsMutate(t, fs[7], func(b map[string]any) {
		b["response"].(map[string]any)["output"].([]any)[0].(map[string]any)["id"] = "changed"
	})
	rsReject(t, fs)
	// Isolated guard: the lineage mismatch fails at the terminal frame.
	dec := NewResponsesStreamDecoder(DefaultLimits())
	for i, f := range fs {
		_, rerr := dec.DecodeFrame(f)
		if rerr != nil {
			if i != 7 || rerr.Status != 502 {
				t.Fatalf("terminal lineage guard failed at frame %d: %v", i, rerr)
			}
			return
		}
	}
	t.Fatal("terminal item-ID drift accepted")
}

func TestResponsesStreamIRReviewPartDoneText(t *testing.T) {
	fs := rsTextStream(t, 0, "hi", "hi")
	fs[5] = rsMutate(t, fs[5], func(b map[string]any) {
		b["part"].(map[string]any)["text"] = "DIFFERENT"
	})
	rsReject(t, fs)
	// Isolated guard: the incoherent part payload fails at part.done.
	dec := NewResponsesStreamDecoder(DefaultLimits())
	for i, f := range fs {
		_, rerr := dec.DecodeFrame(f)
		if rerr != nil {
			if i != 5 || rerr.Status != 502 {
				t.Fatalf("part done guard failed at frame %d: %v", i, rerr)
			}
			return
		}
	}
	t.Fatal("contradictory part.done accepted")
}

func TestResponsesStreamIRReviewEmptyArgsBound(t *testing.T) {
	lim := DefaultLimits()
	lim.MaxStreamArgsChars = 1
	d := NewResponsesStreamDecoder(lim)
	fs := []Frame{
		rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
		rsF("response.output_item.added", rsToolAdded(t, 1, 0, "fc_1", "to_1", "ping", "")),
		rsF("response.function_call_arguments.done", rsArgsDone(t, 2, 0, "fc_1", "")),
		rsF("response.output_item.done", rsItemDoneTool(t, 3, 0, "fc_1", "to_1", "ping", "")),
		rsF("response.completed", rsTerminal(t, 4, "completed", rsResp(t, "completed", []any{rsToolOut("fc_1", "to_1", "ping", "{}")}, rsUsage(1, 1), ""))),
	}
	for i, f := range fs {
		if _, err := d.DecodeFrame(f); err != nil {
			if i != 3 {
				t.Fatalf("empty-args bound must fail at item.done, failed at frame %d: %v", i, err)
			}
			return
		}
	}
	t.Fatal("empty args bypassed one-byte aggregate")
}

func TestResponsesStreamIRReviewPendingIdentityBound(t *testing.T) {
	lim := DefaultLimits()
	lim.MaxArgsChars = 128
	lim.MaxStreamArgsChars = 128
	d := NewResponsesStreamDecoder(lim)
	fs := []Frame{
		rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
		rsF("response.output_item.added", rsMsgAdded(t, 1, 0, strings.Repeat("a", 100))),
		rsF("response.output_item.added", rsMsgAdded(t, 2, 1, strings.Repeat("b", 100))),
	}
	for i, f := range fs {
		if _, err := d.DecodeFrame(f); err != nil {
			if i != 2 {
				t.Fatalf("pending bound must fail at second added, failed at frame %d: %v", i, err)
			}
			if err.Status != 502 {
				t.Fatalf("pending bound status = %d, want 502", err.Status)
			}
			if strings.Contains(err.Msg, strings.Repeat("b", 10)) {
				t.Fatalf("pending bound diagnostic echoes identity bytes: %q", err.Msg)
			}
			return
		}
	}
	t.Fatal("pending item IDs exceed retained identity budget without blocks")
}

func TestResponsesStreamIRReviewPendingIdentityReleased(t *testing.T) {
	// Positive release case: once the first message opens its block,
	// its withheld identity flows into the lifecycle and the pending
	// budget is released, so a second 100-byte identity fits.
	lim := DefaultLimits()
	lim.MaxArgsChars = 128
	lim.MaxStreamArgsChars = 128
	d := NewResponsesStreamDecoder(lim)
	idA, idB := strings.Repeat("a", 100), strings.Repeat("b", 100)
	for i, f := range []Frame{
		rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
		rsF("response.output_item.added", rsMsgAdded(t, 1, 0, idA)),
		rsF("response.content_part.added", rsPartAdded(t, 2, 0, idA, "output_text")),
		rsF("response.output_item.added", rsMsgAdded(t, 3, 1, idB)),
	} {
		if _, err := d.DecodeFrame(f); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
	}
}

func TestResponsesStreamIRReviewOrderEnforced(t *testing.T) {
	// Ordered arrival is the positive case (terminal positions match
	// output_index one-to-one).
	usage := rsUsage(2, 1)
	resp := rsResp(t, "completed", []any{
		rsMsgOut("msg_1", "hi"),
		rsToolOut("fc_1", "c1", "f", `{"a":1}`),
	}, usage, "")
	evs := rsFullSuccess(t, []Frame{
		rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
		rsF("response.output_item.added", rsMsgAdded(t, 1, 0, "msg_1")),
		rsF("response.output_item.added", rsToolAdded(t, 2, 1, "fc_1", "c1", "f", "")),
		rsF("response.content_part.added", rsPartAdded(t, 3, 0, "msg_1", "output_text")),
		rsF("response.output_text.delta", rsDelta(t, 4, 0, "msg_1", "hi", false)),
		rsF("response.output_text.done", rsDone(t, 5, 0, "msg_1", "hi", false)),
		rsF("response.content_part.done", rsPartDone(t, 6, 0, "msg_1", "hi", false)),
		rsF("response.function_call_arguments.delta", rsArgsDelta(t, 7, 1, "fc_1", `{"a":1}`)),
		rsF("response.function_call_arguments.done", rsArgsDone(t, 8, 1, "fc_1", `{"a":1}`)),
		rsF("response.output_item.done", rsItemDoneMsg(t, 9, 0, "msg_1", "hi", false)),
		rsF("response.output_item.done", rsItemDoneTool(t, 10, 1, "fc_1", "c1", "f", `{"a":1}`)),
		rsF("response.completed", rsTerminal(t, 11, "completed", resp)),
	})
	if _, ok := evs[len(evs)-1].(CompletionEvent); !ok {
		t.Fatalf("ordered stream terminal = %#v", evs[len(evs)-1])
	}
	// Reordered arrival (index 1 before index 0) is refused as
	// unrepresentable order at the offending added frame.
	rerr := rsMustFailAt(t, []Frame{
		rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
		rsF("response.output_item.added", rsToolAdded(t, 1, 1, "fc_1", "c1", "f", "")),
		rsF("response.output_item.added", rsMsgAdded(t, 2, 0, "msg_1")),
	}, 2)
	if !strings.Contains(rerr.Msg, "has no representation") {
		t.Fatalf("reordered arrival must be unrepresentable order: %v", rerr)
	}
}
