package protocol

import (
	"strings"
	"testing"
)

// Live GPT-5.6 Luna Responses SSE emits protocol `obfuscation` string
// padding on response.output_text.delta. Absent/null is exact (prior
// behavior); a present string is validated as bounded padding and
// discarded with no carriage into output text, events, or terminal
// identity. Malformed or oversized padding fails closed. The member is
// narrow to output_text.delta only: refusal deltas and all other events
// own no obfuscation member, and private/unknown fields stay closed.
//
// Message-phase item/terminal/lineage files are intentionally untouched
// here (root owns them); these tests pin only the text-delta boundary.

func rsDeltaObf(t *testing.T, seq, outIdx int, itemID, delta, obfRaw string) string {
	t.Helper()
	base := `{"type":"response.output_text.delta","sequence_number":` + obfItoa(seq) +
		`,"output_index":` + obfItoa(outIdx) + `,"item_id":` + obfQuote(t, itemID) +
		`,"content_index":0,"delta":` + obfQuote(t, delta)
	if obfRaw != "@@absent@@" {
		base += `,"obfuscation":` + obfRaw
	}
	return base + `}`
}

func obfQuote(t *testing.T, s string) string {
	t.Helper()
	return rsJ(t, s)
}

func obfItoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	p := len(b)
	for n > 0 {
		p--
		b[p] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		p--
		b[p] = '-'
	}
	return string(b[p:])
}

func TestResponsesStreamObfuscationAbsentNullPass(t *testing.T) {
	for _, obf := range []string{"@@absent@@", "null"} {
		usage := rsUsage(5, 2)
		resp := rsResp(t, "completed", []any{rsMsgOut("msg_1", "hi")}, usage, "")
		frames := []Frame{
			rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
			rsF("response.output_item.added", rsMsgAdded(t, 1, 0, "msg_1")),
			rsF("response.content_part.added", rsPartAdded(t, 2, 0, "msg_1", "output_text")),
			rsF("response.output_text.delta", rsDeltaObf(t, 3, 0, "msg_1", "hi", obf)),
			rsF("response.output_text.done", rsDone(t, 4, 0, "msg_1", "hi", false)),
			rsF("response.content_part.done", rsPartDone(t, 5, 0, "msg_1", "hi", false)),
			rsF("response.output_item.done", rsItemDoneMsg(t, 6, 0, "msg_1", "hi", false)),
			rsF("response.completed", rsTerminal(t, 7, "completed", resp)),
		}
		evs := rsFullSuccess(t, frames)
		found := false
		for _, ev := range evs {
			if f, ok := ev.(TextFragment); ok && string(f.Text) == "hi" {
				found = true
			}
		}
		if !found {
			t.Fatalf("obfuscation %q: TextFragment hi missing: %#v", obf, evs)
		}
	}
}

func TestResponsesStreamObfuscationStringDiscardedNoCarriage(t *testing.T) {
	// Valid padding is discarded: done markers still match the deltas
	// alone, fragments carry only delta text, and Chat carries no padding.
	usage := rsUsage(5, 2)
	resp := rsResp(t, "completed", []any{rsMsgOut("msg_1", "hi")}, usage, "")
	obfPad := rsJ(t, "luna-pad-123")
	frames := []Frame{
		rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
		rsF("response.output_item.added", rsMsgAdded(t, 1, 0, "msg_1")),
		rsF("response.content_part.added", rsPartAdded(t, 2, 0, "msg_1", "output_text")),
		rsF("response.output_text.delta", rsDeltaObf(t, 3, 0, "msg_1", "hi", obfPad)),
		rsF("response.output_text.done", rsDone(t, 4, 0, "msg_1", "hi", false)),
		rsF("response.content_part.done", rsPartDone(t, 5, 0, "msg_1", "hi", false)),
		rsF("response.output_item.done", rsItemDoneMsg(t, 6, 0, "msg_1", "hi", false)),
		rsF("response.completed", rsTerminal(t, 7, "completed", resp)),
	}
	evs := rsFullSuccess(t, frames)
	for _, ev := range evs {
		if f, ok := ev.(TextFragment); ok {
			if strings.Contains(string(f.Text), "luna-pad") {
				t.Fatalf("obfuscation carried into TextFragment: %q", string(f.Text))
			}
		}
	}
	// Empty-string padding is also valid discard.
	frames[3] = rsF("response.output_text.delta", rsDeltaObf(t, 3, 0, "msg_1", "hi", `""`))
	evs = rsFullSuccess(t, frames)
	found := false
	for _, ev := range evs {
		if f, ok := ev.(TextFragment); ok && string(f.Text) == "hi" {
			found = true
		}
	}
	if !found {
		t.Fatalf("empty padding must still emit delta hi: %#v", evs)
	}
	// End-to-end Chat translation carries delta text only.
	upstream := r2cCreated() + r2cItemAddedMsg() + r2cPartAdded() +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"item_id\":\"msg_1\",\"delta\":\"hi\",\"obfuscation\":\"pad-xyz\"}\n\n" +
		r2cTextDone("hi") + r2cPartDone("hi") + r2cItemDoneMsg("hi") +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-4.1\",\"status\":\"completed\"," +
		"\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"hi\",\"annotations\":[]}]}]," +
		"\"usage\":{\"input_tokens\":7,\"output_tokens\":3,\"total_tokens\":10}}}\n\n"
	out, rerr := runR2CStream(t, upstream, true, DefaultLimits())
	if rerr != nil {
		t.Fatalf("Chat with obfuscation padding must pass, got %v", rerr)
	}
	if strings.Contains(out, "pad-xyz") {
		t.Fatalf("Chat must not carry obfuscation padding: %s", out)
	}
	if !strings.Contains(out, "hi") {
		t.Fatalf("Chat must carry delta text: %s", out)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Fatalf("Chat missing terminator: %s", out)
	}
}

func TestResponsesStreamObfuscationMalformedInvalid(t *testing.T) {
	for _, obf := range []string{`5`, `true`, `[]`, `{}`, `["x"]`, `{"x":1}`} {
		frames := []Frame{
			rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
			rsF("response.output_item.added", rsMsgAdded(t, 1, 0, "msg_1")),
			rsF("response.content_part.added", rsPartAdded(t, 2, 0, "msg_1", "output_text")),
			rsF("response.output_text.delta", rsDeltaObf(t, 3, 0, "msg_1", "hi", obf)),
		}
		rerr := rsMustFailAt(t, frames, 3)
		if rerr.Msg != "Upstream returned an invalid response" {
			t.Fatalf("obfuscation %q must be invalid, got %q", obf, rerr.Msg)
		}
	}
}

func TestResponsesStreamObfuscationOversizedFails(t *testing.T) {
	lim := DefaultLimits()
	lim.MaxArgsChars = 8
	big := `"` + strings.Repeat("x", 9) + `"`
	dec := NewResponsesStreamDecoder(lim)
	_, rerr := dec.DecodeFrame(rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")))
	if rerr != nil {
		t.Fatalf("created: %v", rerr)
	}
	_, rerr = dec.DecodeFrame(rsF("response.output_item.added", rsMsgAdded(t, 1, 0, "msg_1")))
	if rerr != nil {
		t.Fatalf("added: %v", rerr)
	}
	_, rerr = dec.DecodeFrame(rsF("response.content_part.added", rsPartAdded(t, 2, 0, "msg_1", "output_text")))
	if rerr != nil {
		t.Fatalf("part: %v", rerr)
	}
	_, rerr = dec.DecodeFrame(rsF("response.output_text.delta", rsDeltaObf(t, 3, 0, "msg_1", "hi", big)))
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("oversized obfuscation must fail 502, got %v", rerr)
	}
	if rerr.Msg != "Upstream response exceeds the size bound" {
		t.Fatalf("oversized must name size bound, got %q", rerr.Msg)
	}
	// Boundary passes: exactly MaxArgsChars bytes.
	dec2 := NewResponsesStreamDecoder(lim)
	for i, f := range []Frame{
		rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
		rsF("response.output_item.added", rsMsgAdded(t, 1, 0, "msg_1")),
		rsF("response.content_part.added", rsPartAdded(t, 2, 0, "msg_1", "output_text")),
		rsF("response.output_text.delta", rsDeltaObf(t, 3, 0, "msg_1", "hi", `"`+strings.Repeat("y", 8)+`"`)),
	} {
		if _, rerr := dec2.DecodeFrame(f); rerr != nil {
			t.Fatalf("boundary frame %d must pass, got %v", i, rerr)
		}
	}
}

func TestResponsesStreamObfuscationNarrowUnknownClosed(t *testing.T) {
	// refusal.delta owns no obfuscation: non-null still unrepresentable.
	refusalDelta := rsJ(t, map[string]any{
		"type": "response.refusal.delta", "sequence_number": 3,
		"output_index": 0, "item_id": "msg_1", "content_index": 0,
		"delta": "no", "obfuscation": "pad",
	})
	rerr := rsMustFailAt(t, []Frame{
		rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
		rsF("response.output_item.added", rsMsgAdded(t, 1, 0, "msg_1")),
		rsF("response.content_part.added", rsPartAdded(t, 2, 0, "msg_1", "refusal")),
		rsF("response.refusal.delta", refusalDelta),
	}, 3)
	if !strings.Contains(rerr.Msg, "response.refusal.delta.obfuscation") || !strings.Contains(rerr.Msg, "has no representation") {
		t.Fatalf("refusal obfuscation must name response.refusal.delta.obfuscation as unrepresentable, got %q", rerr.Msg)
	}
	// output_text.done owns no obfuscation: non-null still unrepresentable.
	doneWithObf := rsJ(t, map[string]any{
		"type": "response.output_text.done", "sequence_number": 4,
		"output_index": 0, "item_id": "msg_1", "content_index": 0,
		"text": "hi", "obfuscation": "pad",
	})
	rerr = rsMustFailAt(t, []Frame{
		rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
		rsF("response.output_item.added", rsMsgAdded(t, 1, 0, "msg_1")),
		rsF("response.content_part.added", rsPartAdded(t, 2, 0, "msg_1", "output_text")),
		rsF("response.output_text.delta", rsDelta(t, 3, 0, "msg_1", "hi", false)),
		rsF("response.output_text.done", doneWithObf),
	}, 4)
	if !strings.Contains(rerr.Msg, "has no representation") {
		t.Fatalf("done obfuscation must stay unrepresentable, got %q", rerr.Msg)
	}
	// Unknown non-null members on the delta stay closed.
	unknown := rsJ(t, map[string]any{
		"type": "response.output_text.delta", "sequence_number": 3,
		"output_index": 0, "item_id": "msg_1", "content_index": 0,
		"delta": "hi", "future": "x",
	})
	rerr = rsMustFailAt(t, []Frame{
		rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
		rsF("response.output_item.added", rsMsgAdded(t, 1, 0, "msg_1")),
		rsF("response.content_part.added", rsPartAdded(t, 2, 0, "msg_1", "output_text")),
		rsF("response.output_text.delta", unknown),
	}, 3)
	if !strings.Contains(rerr.Msg, "has no representation") || strings.Contains(rerr.Msg, `"x"`) {
		t.Fatalf("unknown delta member must stay value-free unrepresentable, got %q", rerr.Msg)
	}
	// Private reasoning stays strict in the default decoder.
	rerr = rsMustFailAt(t, []Frame{
		rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
		rsF("response.output_item.added", rsJ(t, map[string]any{
			"type": "response.output_item.added", "sequence_number": 1, "output_index": 0,
			"item": map[string]any{"type": "reasoning", "id": "rs_1", "status": "in_progress", "summary": []any{}},
		})),
	}, 1)
	if !strings.Contains(rerr.Msg, "has no representation") {
		t.Fatalf("private reasoning must stay strict, got %q", rerr.Msg)
	}
}
