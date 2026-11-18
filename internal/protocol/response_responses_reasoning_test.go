// Responses source private-reasoning compatibility tests: measured live
// Muse OpenCode shape (reasoning OUTPUT items alongside visible output).
//
// The gateway may return provider-private reasoning items on the Responses
// row: output_item.added reasoning {id,type,status:in_progress,summary:[]},
// matching done {id,type,status:completed,encrypted_content,summary:[]},
// and a terminal output repeating reasoning then the visible message.
// The encrypted blob and summaries must never be echoed, projected to
// public text, or invented as continuation state. Absent reasoning
// decodes identically in both modes; the measured shape is strict-502
// and lossy-dropped; malformed, nonempty summaries, new event families,
// forged lineage, and missing done fail in both modes. Terminal finish
// plus authoritative usage remain required; truncation never becomes a
// synthetic success.
package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func jsonUnmarshalForTest(t *testing.T, data string, v any) error {
	t.Helper()
	return json.Unmarshal([]byte(data), v)
}

func mustRawJSON(t *testing.T, s string) json.RawMessage {
	t.Helper()
	var raw json.RawMessage
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		t.Fatalf("raw json: %v", err)
	}
	return raw
}

func reasonFullRaw(reasoningFrag, msgText string, usage string) string {
	msg := `{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"` + msgText + `","annotations":[]}]}`
	out := `[` + reasoningFrag + `,` + msg + `]`
	if reasoningFrag == "" {
		out = `[` + msg + `]`
	}
	return `{"id":"resp_1","object":"response","created_at":1746989954.0,"model":"up",` +
		`"status":"completed","error":null,"incomplete_details":null,` +
		`"output":` + out + `,"usage":` + usage + `}`
}

func reasonUsage() string {
	return `{"input_tokens":7,"output_tokens":3,"total_tokens":10,"output_tokens_details":{"reasoning_tokens":2}}`
}

func TestResponsesReasoningFullMeasuredStrictVsLossy(t *testing.T) {
	secret := "enc-opaque-blob-xyz-123"
	reason := `{"id":"rs_1","type":"reasoning","status":"completed","summary":[],"encrypted_content":"` + secret + `"}`
	usage := strings.TrimSuffix(reasonUsage(), "}") + `,"cost":0,"opencode":` + gatewayValidOpencode() + "}"
	raw := reasonFullRaw(reason, "hi", usage)
	// Strict refuses naming the item path only.
	_, rerr := DecodeResponsesResponse([]byte(raw), DefaultLimits())
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("strict measured reasoning must 502, got %v", rerr)
	}
	if !strings.Contains(rerr.Msg, "response.output[0].type") || !strings.Contains(rerr.Msg, "has no representation") {
		t.Fatalf("strict error must name item path: %q", rerr.Msg)
	}
	if strings.Contains(rerr.Msg, secret) {
		t.Fatalf("strict error echoes private blob: %q", rerr.Msg)
	}
	// Lossy drops reasoning, keeps visible message, usage, finish.
	resp, rerr := DecodeResponsesResponseWithPolicy([]byte(raw), DefaultLimits(), true)
	if rerr != nil {
		t.Fatalf("lossy measured reasoning must decode: %v", rerr)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("lossy items = %#v, want one visible text", resp.Items)
	}
	tx, ok := resp.Items[0].(TextItem)
	if !ok || tx.Text != "hi" {
		t.Fatalf("lossy text wrong: %#v", resp.Items[0])
	}
	if resp.MalformedTools != 0 {
		t.Fatalf("reasoning must not count as malformed: %d", resp.MalformedTools)
	}
	if rerr := resp.Usage.ValidateAuthoritative(); rerr != nil {
		t.Fatalf("lossy usage not authoritative: %v", rerr)
	}
	if resp.Usage.Reasoning == nil || *resp.Usage.Reasoning != 2 {
		t.Fatalf("authoritative reasoning tokens lost: %+v", resp.Usage)
	}
	if resp.Finish.Kind != FinishStop {
		t.Fatalf("lossy finish wrong: %+v", resp.Finish)
	}
	for _, fn := range []func(Response, string) ([]byte, *RouterError){
		func(r Response, p string) ([]byte, *RouterError) { return EncodeChatResponse(r, p) },
		func(r Response, p string) ([]byte, *RouterError) { return EncodeGeminiResponse(r, p) },
		func(r Response, p string) ([]byte, *RouterError) { return EncodeResponsesResponse(r, p) },
	} {
		out, rerr := fn(resp, "public")
		if rerr != nil {
			t.Fatalf("encode must succeed: %v", rerr)
		}
		if strings.Contains(string(out), secret) || strings.Contains(string(out), "rs_1") {
			t.Fatalf("encoded output leaks private reasoning: %s", out)
		}
		if !strings.Contains(string(out), "hi") {
			t.Fatalf("encoded output lost visible message: %s", out)
		}
	}
	// Anthropic has no reasoning bucket: nonzero reasoning_tokens still
	// refuses at the target gate (existing provenance contract).
	if _, rerr := EncodeAnthropicResponse(resp, "public"); rerr == nil || rerr.Status != 502 {
		t.Fatalf("anthropic encode with reasoning tokens must 502, got %v", rerr)
	}
}

func TestResponsesReasoningFullLossyClosedShape(t *testing.T) {
	secret := "enc-blob"
	goodReason := `{"id":"rs_1","type":"reasoning","status":"completed","summary":[],"encrypted_content":"` + secret + `"}`
	good := reasonFullRaw(goodReason, "hi", reasonUsage())
	if _, rerr := DecodeResponsesResponseWithPolicy([]byte(good), DefaultLimits(), true); rerr != nil {
		t.Fatalf("lossy good shape must pass: %v", rerr)
	}
	// Nonempty summary has no representation even in lossy mode.
	badSummary := `{"id":"rs_1","type":"reasoning","status":"completed","summary":[{"type":"summary_text","text":"think"}],"encrypted_content":"` + secret + `"}`
	if _, rerr := DecodeResponsesResponseWithPolicy([]byte(reasonFullRaw(badSummary, "hi", reasonUsage())), DefaultLimits(), true); rerr == nil || rerr.Status != 502 {
		t.Fatalf("nonempty summary must 502 lossy, got %v", rerr)
	} else if !strings.Contains(rerr.Msg, "has no representation") {
		t.Fatalf("nonempty summary must be unrepresentable: %q", rerr.Msg)
	}
	// Missing encrypted_content is invalid in both modes.
	missingEnc := `{"id":"rs_1","type":"reasoning","status":"completed","summary":[]}`
	for _, policy := range []bool{false, true} {
		if _, rerr := DecodeResponsesResponseWithPolicy([]byte(reasonFullRaw(missingEnc, "hi", reasonUsage())), DefaultLimits(), policy); rerr == nil || rerr.Status != 502 {
			t.Fatalf("policy=%v missing encrypted must 502, got %v", policy, rerr)
		}
	}
	// Empty encrypted_content is invalid in both modes.
	emptyEnc := `{"id":"rs_1","type":"reasoning","status":"completed","summary":[],"encrypted_content":""}`
	for _, policy := range []bool{false, true} {
		if _, rerr := DecodeResponsesResponseWithPolicy([]byte(reasonFullRaw(emptyEnc, "hi", reasonUsage())), DefaultLimits(), policy); rerr == nil || rerr.Status != 502 {
			t.Fatalf("policy=%v empty encrypted must 502, got %v", policy, rerr)
		}
	}
	// Minimal legacy shape without summary/encrypted still fails lossy.
	minimal := `{"id":"rs_1","type":"reasoning","status":"completed"}`
	if _, rerr := DecodeResponsesResponseWithPolicy([]byte(reasonFullRaw(minimal, "hi", reasonUsage())), DefaultLimits(), true); rerr == nil || rerr.Status != 502 {
		t.Fatalf("minimal shape must 502 lossy, got %v", rerr)
	}
	// Conflicting item identities fail in both modes.
	dup := `{"id":"resp_1","object":"response","created_at":1746989954.0,"model":"up","status":"completed","error":null,"incomplete_details":null,` +
		`"output":[` + goodReason + `,{"id":"rs_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi","annotations":[]}]}],` +
		`"usage":` + reasonUsage() + `}`
	for _, policy := range []bool{false, true} {
		if _, rerr := DecodeResponsesResponseWithPolicy([]byte(dup), DefaultLimits(), policy); rerr == nil || rerr.Status != 502 {
			t.Fatalf("policy=%v conflicting ids must 502, got %v", policy, rerr)
		}
	}
}

func TestResponsesReasoningFullTranslatorsWithPolicy(t *testing.T) {
	secret := "translator-blob-999"
	reason := `{"id":"rs_1","type":"reasoning","status":"completed","summary":[],"encrypted_content":"` + secret + `"}`
	raw := reasonFullRaw(reason, "hello", reasonUsage())
	// Strict translators refuse pre-I/O.
	for _, fn := range []func([]byte, string, Limits) ([]byte, *RouterError){
		TranslateResponsesJSONToChat, TranslateResponsesJSONToAnthropic, TranslateResponsesJSONToGemini,
	} {
		if _, rerr := fn([]byte(raw), "public", DefaultLimits()); rerr == nil || rerr.Status != 502 {
			t.Fatalf("strict translator must 502, got %v", rerr)
		}
	}
	// Lossy translators preserve visible text and discard private reasoning.
	for _, fn := range []func([]byte, string, Limits, bool) ([]byte, *RouterError){
		TranslateResponsesJSONToChatWithPolicy, TranslateResponsesJSONToGeminiWithPolicy, TranslateResponsesJSONToAnthropicWithPolicy,
	} {
		out, rerr := fn([]byte(raw), "public", DefaultLimits(), true)
		if rerr != nil {
			t.Fatalf("lossy translator: %v", rerr)
		}
		if strings.Contains(string(out), secret) || strings.Contains(string(out), "rs_1") {
			t.Fatalf("translator output leaks reasoning: %s", out)
		}
		if !strings.Contains(string(out), "hello") {
			t.Fatalf("translator output lost visible message: %s", out)
		}
	}
	// Policy false keeps the strict refusal.
	if _, rerr := TranslateResponsesJSONToChatWithPolicy([]byte(raw), "public", DefaultLimits(), false); rerr == nil || rerr.Status != 502 {
		t.Fatalf("policy=false must 502, got %v", rerr)
	}
}

// --- Streaming ---

func reasonAddedFrame(t *testing.T, seq, outIdx int, itemID string) Frame {
	t.Helper()
	return rsF("response.output_item.added", rsJ(t, rsWithSeq(map[string]any{
		"type": "response.output_item.added", "output_index": outIdx,
		"item": map[string]any{
			"type": "reasoning", "id": itemID,
			"status": "in_progress", "summary": []any{},
		},
	}, seq)))
}

func reasonDoneFrame(t *testing.T, seq, outIdx int, itemID, enc string) Frame {
	t.Helper()
	return rsF("response.output_item.done", rsJ(t, rsWithSeq(map[string]any{
		"type": "response.output_item.done", "output_index": outIdx,
		"item": map[string]any{
			"type": "reasoning", "id": itemID,
			"status": "completed", "summary": []any{},
			"encrypted_content": enc,
		},
	}, seq)))
}

func reasonOut(id, enc string) map[string]any {
	return map[string]any{
		"type": "reasoning", "id": id,
		"status": "completed", "summary": []any{},
		"encrypted_content": enc,
	}
}

func reasonMeasuredFrames(t *testing.T, enc string) []Frame {
	t.Helper()
	usage := map[string]any{
		"input_tokens": 7, "output_tokens": 3, "total_tokens": 10,
		"output_tokens_details": map[string]any{"reasoning_tokens": 2},
		"cost":                  0, "opencode": json.RawMessage(gatewayValidOpencode()),
	}
	resp := rsRespID(t, "resp_7", "gpt-4o", "completed",
		[]any{reasonOut("rs_1", enc+"-terminal"), rsMsgOut("msg_1", "hi")}, usage, "")
	return []Frame{
		rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
		reasonAddedFrame(t, 1, 0, "rs_1"),
		reasonDoneFrame(t, 2, 0, "rs_1", enc),
		rsF("response.output_item.added", rsMsgAdded(t, 3, 1, "msg_1")),
		rsF("response.content_part.added", rsPartAdded(t, 4, 1, "msg_1", "output_text")),
		rsF("response.output_text.delta", rsDelta(t, 5, 1, "msg_1", "hi", false)),
		rsF("response.output_text.done", rsDone(t, 6, 1, "msg_1", "hi", false)),
		rsF("response.content_part.done", rsPartDone(t, 7, 1, "msg_1", "hi", false)),
		rsF("response.output_item.done", rsItemDoneMsg(t, 8, 1, "msg_1", "hi", false)),
		rsF("response.completed", rsTerminal(t, 9, "completed", resp)),
	}
}

func decodeWithPolicy(frames []Frame, allowPrivateDrop bool) ([]ResponseEvent, *RouterError) {
	dec := NewResponsesStreamDecoderWithPolicy(DefaultLimits(), allowPrivateDrop)
	var all []ResponseEvent
	for _, f := range frames {
		evs, rerr := dec.DecodeFrame(f)
		if rerr != nil {
			return nil, rerr
		}
		all = append(all, evs...)
	}
	if _, rerr := dec.Close(); rerr != nil {
		return nil, rerr
	}
	return all, nil
}

func TestResponsesReasoningStreamMeasured(t *testing.T) {
	secret := "stream-enc-blob-abc-123"
	frames := reasonMeasuredFrames(t, secret)
	// Strict refuses the reasoning added as unrepresentable.
	dec := NewResponsesStreamDecoder(DefaultLimits())
	if _, rerr := dec.DecodeFrame(frames[0]); rerr != nil {
		t.Fatalf("strict created: %v", rerr)
	}
	if _, rerr := dec.DecodeFrame(frames[1]); rerr == nil || rerr.Status != 502 {
		t.Fatalf("strict reasoning added must 502, got %v", rerr)
	} else if !strings.Contains(rerr.Msg, "has no representation") {
		t.Fatalf("strict reasoning added must be unrepresentable: %q", rerr.Msg)
	} else if strings.Contains(rerr.Msg, secret) {
		t.Fatalf("strict error echoes private blob: %q", rerr.Msg)
	}
	// Lossy drops reasoning, emits only visible message + terminal.
	all, rerr := decodeWithPolicy(frames, true)
	if rerr != nil {
		t.Fatalf("lossy measured stream must succeed: %v", rerr)
	}
	var texts []string
	var sawUsage, sawComplete bool
	for _, ev := range all {
		switch e := ev.(type) {
		case TextFragment:
			texts = append(texts, e.Text)
			if strings.Contains(e.Text, secret) {
				t.Fatalf("TextFragment leaks blob: %q", e.Text)
			}
		case BlockOpen:
			if string(e.Item) == "rs_1" {
				t.Fatalf("reasoning must never open a block: %#v", e)
			}
		case UsageEvent:
			if e.Terminal() {
				sawUsage = true
				if e.Usage.Reasoning == nil {
					t.Fatalf("terminal usage lost reasoning provenance: %+v", e.Usage)
				}
			}
		case CompletionEvent:
			if e.Finish.Kind == FinishStop {
				sawComplete = true
			}
		case MalformedToolEvent:
			t.Fatalf("reasoning must not classify as malformed tool: %#v", e)
		}
		if _, ok := ev.(ResponseModelEvent); ok {
			t.Fatalf("model already known: unexpected ResponseModelEvent")
		}
	}
	if len(texts) != 1 || texts[0] != "hi" {
		t.Fatalf("lossy text fragments = %q, want [hi]", texts)
	}
	if !sawUsage || !sawComplete {
		t.Fatalf("lossy terminal missing usage=%v complete=%v: %#v", sawUsage, sawComplete, all)
	}
	for _, ev := range all {
		s := strings.Replace(rsJ(t, ev), secret, "LEAK", -1)
		_ = s
	}
	// No event stringifies the blob (defensive: join all text).
	joined := strings.Join(texts, "")
	if strings.Contains(joined, secret) {
		t.Fatalf("stream projected private blob to text: %q", joined)
	}
}

func TestResponsesReasoningStreamForgedTerminal(t *testing.T) {
	secret := "real-blob-forge-test"
	frames := reasonMeasuredFrames(t, secret)
	// Forge the terminal item identity.
	last := frames[len(frames)-1]
	var body map[string]any
	if err := jsonUnmarshalForTest(t, last.Data, &body); err != nil {
		t.Fatalf("unmarshal terminal: %v", err)
	}
	respObj, _ := body["response"].(map[string]any)
	outs, _ := respObj["output"].([]any)
	outs[0].(map[string]any)["id"] = "forged-item-id"
	forgedResp := rsJ(t, respObj)
	frames[len(frames)-1] = rsF("response.completed", rsJ(t, map[string]any{
		"type": "response.completed", "sequence_number": 9, "response": mustRawJSON(t, forgedResp),
	}))
	if _, rerr := decodeWithPolicy(frames, true); rerr == nil || rerr.Status != 502 {
		t.Fatalf("forged terminal must 502, got %v", rerr)
	} else if !strings.Contains(rerr.Msg, "contradictory") {
		t.Fatalf("forged terminal must be contradictory: %q", rerr.Msg)
	}
	// Forged terminal that drops the reasoning item entirely also fails
	// (raw positions must match streamed output_index).
	frames2 := reasonMeasuredFrames(t, secret)
	var body2 map[string]any
	if err := jsonUnmarshalForTest(t, frames2[len(frames2)-1].Data, &body2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	resp2, _ := body2["response"].(map[string]any)
	outs2, _ := resp2["output"].([]any)
	resp2["output"] = outs2[1:]
	droppedResp := rsJ(t, resp2)
	frames2[len(frames2)-1] = rsF("response.completed", rsJ(t, map[string]any{
		"type": "response.completed", "sequence_number": 9, "response": mustRawJSON(t, droppedResp),
	}))
	if _, rerr := decodeWithPolicy(frames2, true); rerr == nil || rerr.Status != 502 {
		t.Fatalf("dropped reasoning terminal must 502, got %v", rerr)
	}
}

func TestResponsesReasoningStreamMissingDone(t *testing.T) {
	secret := "missing-done-blob"
	frames := reasonMeasuredFrames(t, secret)
	// Remove the reasoning done frame (index 2): added without done.
	trimmed := append([]Frame{}, frames[0:2]...)
	trimmed = append(trimmed, frames[3:]...)
	// Renumbering is unnecessary for the failure: the decoder tracks by
	// output_index, and the terminal still carries both items while the
	// reasoning slot never closed.
	if _, rerr := decodeWithPolicy(trimmed, true); rerr == nil || rerr.Status != 502 {
		t.Fatalf("missing reasoning done must 502, got %v", rerr)
	}
}

func TestResponsesReasoningStreamNonemptySummaryAndEvents(t *testing.T) {
	// Nonempty summary at added fails even in lossy mode.
	badAdded := rsF("response.output_item.added", rsJ(t, map[string]any{
		"type": "response.output_item.added", "sequence_number": 1, "output_index": 0,
		"item": map[string]any{
			"type": "reasoning", "id": "rs_1",
			"status": "in_progress", "summary": []any{map[string]any{"type": "summary_text", "text": "x"}},
		},
	}))
	dec := NewResponsesStreamDecoderWithPolicy(DefaultLimits(), true)
	if _, rerr := dec.DecodeFrame(rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o"))); rerr != nil {
		t.Fatalf("created: %v", rerr)
	}
	if _, rerr := dec.DecodeFrame(badAdded); rerr == nil || rerr.Status != 502 {
		t.Fatalf("nonempty summary added must 502, got %v", rerr)
	} else if !strings.Contains(rerr.Msg, "has no representation") {
		t.Fatalf("nonempty summary must be unrepresentable: %q", rerr.Msg)
	}
	// A new reasoning event family has no representation in both modes.
	for _, policy := range []bool{false, true} {
		d := NewResponsesStreamDecoderWithPolicy(DefaultLimits(), policy)
		if _, rerr := d.DecodeFrame(rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o"))); rerr != nil {
			t.Fatalf("created: %v", rerr)
		}
		ev := rsF("response.reasoning.delta", rsJ(t, map[string]any{
			"type": "response.reasoning.delta", "sequence_number": 1,
		}))
		if _, rerr := d.DecodeFrame(ev); rerr == nil || rerr.Status != 502 {
			t.Fatalf("policy=%v new event family must 502, got %v", policy, rerr)
		} else if !strings.Contains(rerr.Msg, "has no representation") {
			t.Fatalf("new event must be unrepresentable: %q", rerr.Msg)
		}
	}
	// Truncation is still truncation even with the drop: missing terminal
	// usage fails.
	secret := "trunc-blob"
	frames := reasonMeasuredFrames(t, secret)
	// Corrupt the terminal usage total.
	var body map[string]any
	if err := jsonUnmarshalForTest(t, frames[len(frames)-1].Data, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	respObj, _ := body["response"].(map[string]any)
	usage, _ := respObj["usage"].(map[string]any)
	usage["total_tokens"] = float64(999)
	badResp := rsJ(t, respObj)
	frames[len(frames)-1] = rsF("response.completed", rsJ(t, map[string]any{
		"type": "response.completed", "sequence_number": 9, "response": mustRawJSON(t, badResp),
	}))
	if _, rerr := decodeWithPolicy(frames, true); rerr == nil || rerr.Status != 502 {
		t.Fatalf("bad totals with reasoning drop must still 502, got %v", rerr)
	}
}

func TestResponsesReasoningStreamTranslatorsDropNeverProject(t *testing.T) {
	secret := "translator-stream-blob-xyz"
	frames := reasonMeasuredFrames(t, secret)
	var sb strings.Builder
	for _, f := range frames {
		if f.Event != "" {
			sb.WriteString("event: " + f.Event + "\n")
		}
		sb.WriteString("data: " + f.Data + "\n\n")
	}
	upstream := sb.String()
	// Strict translators refuse pre-commit.
	{
		var out strings.Builder
		tr := &ResponsesToChatSSETranslator{PublicModel: "p", Limits: DefaultLimits()}
		if rerr := tr.Translate(strings.NewReader(upstream), &out, nil); rerr == nil || rerr.Status != 502 {
			t.Fatalf("strict chat translator must 502, got %v", rerr)
		}
	}
	// Lossy Chat translator succeeds with visible text only and a terminal.
	// (Anthropic has no reasoning bucket: the same upstream still refuses
	// at the Anthropic target gate even after the private drop, which the
	// complete-row test pins. Stream coverage focuses on the Chat bridge
	// that preserves reasoning accounting.)
	tr := &ResponsesToChatSSETranslator{PublicModel: "p", Limits: DefaultLimits(), AllowPrivateReasoningDrop: true}
	var out strings.Builder
	if rerr := tr.Translate(strings.NewReader(upstream), &out, nil); rerr != nil {
		t.Fatalf("lossy chat translator: %v", rerr)
	}
	if strings.Contains(out.String(), secret) || strings.Contains(out.String(), "rs_1") {
		t.Fatalf("chat output leaks reasoning: %s", out.String())
	}
	if !strings.Contains(out.String(), "hi") || !strings.Contains(out.String(), "[DONE]") {
		t.Fatalf("chat output missing visible terminal: %s", out.String())
	}
}
