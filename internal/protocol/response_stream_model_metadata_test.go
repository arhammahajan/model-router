// Responses stream model-metadata tests: absent-at-created promotion.
//
// Source fact (proven by the accepted legacy fixtures): real Responses
// senders omit model at response.created (legacy r2cCreated carries only
// {id,status}, r2gCreated only {id}) and report it later in a progress
// envelope or the actual terminal response object. The staged decoder
// used to require model at created, which forced either fixture
// fabrication or a whole-generation buffer. This file pins the narrow
// fix: created emits ResponseStart with Model "" (truthful absence,
// never a fake provider string), the first provider report emits one
// typed ResponseModelEvent in wire order, the lifecycle retains it in
// Result.Model, and all four target encoders consume it metadata-only
// (zero downstream bytes, stable wrapper IDs, stable public alias).
//
// What is NOT relaxed: complete-wire DecodeResponsesResponse still
// requires an actual model; NewResponse still rejects empty models for
// non-Gemini/Anthropic complete rows; drift/empty/meta-before-start/
// after-completion all fail static with value-free diagnostics; Chat
// source streams still require a model and emit no model events.
package protocol

import (
	"bytes"
	"strings"
	"testing"
)

// --- Builders (staged-only; legacy r2cCreated/r2gCreated untouched) ---

func rsCreatedNoModel(t *testing.T, seq int) string {
	t.Helper()
	return rsCreatedRaw(t, seq, "resp_7", "")
}

func rsProgressModel(t *testing.T, seq int, model string) string {
	t.Helper()
	resp := map[string]any{"id": "resp_7"}
	if model != "" {
		resp["model"] = model
	}
	return rsJ(t, rsWithSeq(map[string]any{
		"type": "response.in_progress", "response": resp,
	}, seq))
}

// rsTextNoModelProgress decodes to: Start("") , BlockOpen, Text("hel"),
// ModelEvent(gpt-4o), Text("lo"), BlockClose, Usage, Completion.
func rsTextNoModelProgress(t *testing.T) []Frame {
	t.Helper()
	usage := rsUsage(25, 15)
	resp := rsResp(t, "completed", []any{rsMsgOut("msg_1", "hello")}, usage, "")
	return []Frame{
		rsF("response.created", rsCreatedNoModel(t, 0)),
		rsF("response.output_item.added", rsMsgAdded(t, 1, 0, "msg_1")),
		rsF("response.content_part.added", rsPartAdded(t, 2, 0, "msg_1", "output_text")),
		rsF("response.output_text.delta", rsDelta(t, 3, 0, "msg_1", "hel", false)),
		rsF("response.in_progress", rsProgressModel(t, 4, "gpt-4o")),
		rsF("response.output_text.delta", rsDelta(t, 5, 0, "msg_1", "lo", false)),
		rsF("response.output_text.done", rsDone(t, 6, 0, "msg_1", "hello", false)),
		rsF("response.content_part.done", rsPartDone(t, 7, 0, "msg_1", "hello", false)),
		rsF("response.output_item.done", rsItemDoneMsg(t, 8, 0, "msg_1", "hello", false)),
		rsF("response.completed", rsTerminal(t, 9, "completed", resp)),
	}
}

// rsToolNoModelTerminal omits model until the actual terminal object:
// Start(""), tool open/fragments/close, then ModelEvent + Usage +
// Completion all from the terminal frame.
func rsToolNoModelTerminal(t *testing.T) []Frame {
	t.Helper()
	usage := rsUsage(25, 15)
	resp := rsResp(t, "completed", []any{rsToolOut("fc_1", "call_9", "get_time", `{"tz":"UTC"}`)}, usage, "")
	return []Frame{
		rsF("response.created", rsCreatedNoModel(t, 0)),
		rsF("response.output_item.added", rsToolAdded(t, 1, 0, "fc_1", "call_9", "get_time", "")),
		rsF("response.function_call_arguments.delta", rsArgsDelta(t, 2, 0, "fc_1", `{"tz":"`)),
		rsF("response.function_call_arguments.delta", rsArgsDelta(t, 3, 0, "fc_1", `UTC"}`)),
		rsF("response.function_call_arguments.done", rsArgsDone(t, 4, 0, "fc_1", `{"tz":"UTC"}`)),
		rsF("response.output_item.done", rsItemDoneTool(t, 5, 0, "fc_1", "call_9", "get_time", `{"tz":"UTC"}`)),
		rsF("response.completed", rsTerminal(t, 6, "completed", resp)),
	}
}

func rsDecodeAll(t *testing.T, frames []Frame) ([]ResponseEvent, *ResponsesStreamDecoder) {
	t.Helper()
	dec := NewResponsesStreamDecoder(DefaultLimits())
	var all []ResponseEvent
	for i, f := range frames {
		evs, rerr := dec.DecodeFrame(f)
		if rerr != nil {
			t.Fatalf("DecodeFrame[%d]: %v", i, rerr)
		}
		all = append(all, evs...)
	}
	if tail, rerr := dec.Close(); rerr != nil {
		t.Fatalf("Close: %v", rerr)
	} else if len(tail) != 0 {
		t.Fatalf("Close returned %d events, want none", len(tail))
	}
	return all, dec
}

func rsObserveAll(t *testing.T, evs []ResponseEvent) *ResponseLifecycle {
	t.Helper()
	life := NewResponseLifecycle(DefaultLimits(), DialectResponses)
	for i, ev := range evs {
		if rerr := life.Observe(ev); rerr != nil {
			t.Fatalf("lifecycle Observe[%d](%T): %v", i, ev, rerr)
		}
	}
	return life
}

func eventKinds(evs []ResponseEvent) []string {
	kinds := make([]string, 0, len(evs))
	for _, ev := range evs {
		switch ev.(type) {
		case ResponseStart:
			kinds = append(kinds, "start")
		case ResponseModelEvent:
			kinds = append(kinds, "model")
		case BlockOpen:
			kinds = append(kinds, "open")
		case TextFragment:
			kinds = append(kinds, "text")
		case ArgsFragment:
			kinds = append(kinds, "args")
		case BlockClose:
			kinds = append(kinds, "close")
		case MalformedToolEvent:
			kinds = append(kinds, "malformed")
		case UsageEvent:
			kinds = append(kinds, "usage")
		case CompletionEvent:
			kinds = append(kinds, "completion")
		default:
			kinds = append(kinds, "unknown")
		}
	}
	return kinds
}

// --- Absent-at-created promotion: progress variant --------------------

func TestResponsesStreamModelProgressPromotion(t *testing.T) {
	frames := rsTextNoModelProgress(t)
	dec := NewResponsesStreamDecoder(DefaultLimits())
	var all []ResponseEvent
	perFrame := make([]int, len(frames))
	for i, f := range frames {
		evs, rerr := dec.DecodeFrame(f)
		if rerr != nil {
			t.Fatalf("DecodeFrame[%d]: %v", i, rerr)
		}
		perFrame[i] = len(evs)
		all = append(all, evs...)
	}
	if _, rerr := dec.Close(); rerr != nil {
		t.Fatalf("Close: %v", rerr)
	}
	// Absent start is truthful, not a provider default: Model "".
	start, ok := all[0].(ResponseStart)
	if !ok || string(start.ID) != "resp_7" || start.Model != "" || start.Source != DialectResponses {
		t.Fatalf("ev[0] = %#v, want empty-model ResponseStart resp_7", all[0])
	}
	// No whole-generation buffering: the first text delta (frame 3)
	// emits BEFORE the model report (frame 4).
	if perFrame[3] != 1 {
		t.Fatalf("frame 3 (first delta) events = %d, want 1 immediate TextFragment", perFrame[3])
	}
	if tf, ok := all[2].(TextFragment); !ok || tf.Text != "hel" {
		t.Fatalf("ev[2] = %#v, want TextFragment hel before any model", all[2])
	}
	modelIdx := -1
	for i, ev := range all {
		if m, ok := ev.(ResponseModelEvent); ok {
			modelIdx = i
			if m.Model != "gpt-4o" {
				t.Fatalf("model event = %#v, want gpt-4o", ev)
			}
		}
	}
	if modelIdx != 3 {
		t.Fatalf("model event at index %d (kinds %v), want 3 right after first text", modelIdx, eventKinds(all))
	}
	// Repeated identical model at the terminal emits nothing extra.
	models := 0
	for _, ev := range all {
		if _, ok := ev.(ResponseModelEvent); ok {
			models++
		}
	}
	if models != 1 {
		t.Fatalf("model events = %d, want exactly 1 (kinds %v)", models, eventKinds(all))
	}
	// Lifecycle retains the actual terminal model.
	life := rsObserveAll(t, all)
	res, rerr := life.Result()
	if rerr != nil {
		t.Fatalf("Result: %v", rerr)
	}
	if res.Model != "gpt-4o" || string(res.ID) != "resp_7" || res.Source != DialectResponses {
		t.Fatalf("Result = %+v, want Model gpt-4o", res)
	}
}

// --- Absent-at-created promotion: terminal-only variant --------------

func TestResponsesStreamModelTerminalPromotion(t *testing.T) {
	all, _ := rsDecodeAll(t, rsToolNoModelTerminal(t))
	kinds := eventKinds(all)
	// Tool open, two arg fragments, close, then model BEFORE usage.
	want := []string{"start", "open", "args", "args", "close", "model", "usage", "completion"}
	if len(kinds) != len(want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", kinds, want)
		}
	}
	m, ok := all[5].(ResponseModelEvent)
	if !ok || m.Model != "gpt-4o" {
		t.Fatalf("ev[5] = %#v, want ModelEvent gpt-4o before Usage", all[5])
	}
	if _, ok := all[6].(UsageEvent); !ok {
		t.Fatalf("ev[6] = %#v, want UsageEvent after ModelEvent", all[6])
	}
	life := rsObserveAll(t, all)
	res, rerr := life.Result()
	if rerr != nil {
		t.Fatalf("Result: %v", rerr)
	}
	if res.Model != "gpt-4o" {
		t.Fatalf("Result.Model = %q, want gpt-4o", res.Model)
	}
}

// --- Each target consumes the metadata with zero bytes ---------------

func rsEncodeAll(t *testing.T, enc EventEncoder, evs []ResponseEvent) (string, []int) {
	t.Helper()
	var buf bytes.Buffer
	before := make([]int, 0, len(evs))
	after := make([]int, 0, len(evs))
	for i, ev := range evs {
		before = append(before, buf.Len())
		if eerr := enc.Encode(&buf, nil, ev); eerr != nil {
			t.Fatalf("target Encode[%d](%T): %v", i, ev, eerr.Err)
		}
		after = append(after, buf.Len())
	}
	return buf.String(), diffLens(before, after)
}

func diffLens(before, after []int) []int {
	d := make([]int, len(before))
	for i := range before {
		d[i] = after[i] - before[i]
	}
	return d
}

func rsAssertModelZeroBytes(t *testing.T, evs []ResponseEvent, lens []int) {
	t.Helper()
	for i, ev := range evs {
		if _, ok := ev.(ResponseModelEvent); ok && lens[i] != 0 {
			t.Fatalf("ModelEvent wrote %d bytes, want metadata-only 0", lens[i])
		}
	}
}

func TestResponsesStreamModelTargetsMetadataOnly(t *testing.T) {
	all, _ := rsDecodeAll(t, rsTextNoModelProgress(t))
	life := rsObserveAll(t, all)
	if _, rerr := life.Result(); rerr != nil {
		t.Fatalf("Result: %v", rerr)
	}
	lim := DefaultLimits()

	chat := NewChatEventEncoder("public", true, lim)
	chatOut, chatLens := rsEncodeAll(t, chat, all)
	rsAssertModelZeroBytes(t, all, chatLens)
	if !strings.Contains(chatOut, `"model":"public"`) || strings.Contains(chatOut, "gpt-4o") {
		t.Fatalf("Chat wire must stamp only the public alias: %s", chatOut)
	}
	if !strings.HasSuffix(chatOut, "data: [DONE]\n\n") {
		t.Fatalf("Chat wire missing terminator: %s", chatOut)
	}
	if strings.Contains(chatOut, `"id":"resp_7"`) {
		t.Fatalf("Chat wire must mint its wrapper ID, never echo upstream: %s", chatOut)
	}

	anth := NewAnthropicEventEncoder("public", lim)
	anthOut, anthLens := rsEncodeAll(t, anth, all)
	rsAssertModelZeroBytes(t, all, anthLens)
	if !strings.Contains(anthOut, `"model":"public"`) || strings.Contains(anthOut, "gpt-4o") {
		t.Fatalf("Anthropic wire must stamp only the public alias: %s", anthOut)
	}
	if strings.Contains(anthOut, `"id":"resp_7"`) {
		t.Fatalf("Anthropic wire must mint its wrapper ID, never echo upstream: %s", anthOut)
	}

	respEnc := NewResponsesEventEncoder("public", lim)
	respOut, respLens := rsEncodeAll(t, respEnc, all)
	rsAssertModelZeroBytes(t, all, respLens)
	if !strings.Contains(respOut, `"model":"public"`) || strings.Contains(respOut, "gpt-4o") {
		t.Fatalf("Responses wire must stamp only the public alias: %s", respOut)
	}
	// Generated resp_<hex> IDs can legitimately start with resp_7.
	// Refuse the exact upstream identity, not that random prefix.
	if strings.Contains(respOut, `"id":"resp_7"`) {
		t.Fatalf("Responses wire must mint its wrapper IDs, never echo upstream: %s", respOut)
	}

	gem := NewGeminiEventEncoder("public", lim, true)
	gemOut, gemLens := rsEncodeAll(t, gem, all)
	rsAssertModelZeroBytes(t, all, gemLens)
	if !strings.Contains(gemOut, `"modelVersion":"public"`) || strings.Contains(gemOut, "gpt-4o") {
		t.Fatalf("Gemini wire must stamp only the public alias: %s", gemOut)
	}
	if !strings.Contains(gemOut, `"responseId":"resp_7"`) {
		t.Fatalf("Gemini wire must keep the echoed upstream responseId stable: %s", gemOut)
	}
}

func TestResponsesStreamModelToolTargetsMetadataOnly(t *testing.T) {
	all, _ := rsDecodeAll(t, rsToolNoModelTerminal(t))
	lim := DefaultLimits()

	chat := NewChatEventEncoder("public", true, lim)
	chatOut, chatLens := rsEncodeAll(t, chat, all)
	rsAssertModelZeroBytes(t, all, chatLens)
	if !strings.Contains(chatOut, `"id":"call_9"`) || !strings.Contains(chatOut, `"name":"get_time"`) {
		t.Fatalf("Chat tool identity must be the upstream call_id: %s", chatOut)
	}
	if strings.Contains(chatOut, "gpt-4o") {
		t.Fatalf("Chat wire must not leak the provider model: %s", chatOut)
	}

	respEnc := NewResponsesEventEncoder("public", lim)
	respOut, respLens := rsEncodeAll(t, respEnc, all)
	rsAssertModelZeroBytes(t, all, respLens)
	if !strings.Contains(respOut, `"call_id":"call_9"`) || strings.Contains(respOut, "gpt-4o") {
		t.Fatalf("Responses tool wire wrong: %s", respOut)
	}

	gem := NewGeminiEventEncoder("public", lim, true)
	gemOut, gemLens := rsEncodeAll(t, gem, all)
	rsAssertModelZeroBytes(t, all, gemLens)
	if !strings.Contains(gemOut, `"modelVersion":"public"`) || strings.Contains(gemOut, "gpt-4o") {
		t.Fatalf("Gemini tool wire must stamp only the public alias: %s", gemOut)
	}
}

// --- Static failures: drift / empty / order / completion --------------

func TestResponsesStreamModelStaticFailures(t *testing.T) {
	// Drift at progress: second report differs.
	frames := []Frame{
		rsF("response.created", rsCreatedNoModel(t, 0)),
		rsF("response.in_progress", rsProgressModel(t, 1, "gpt-4o")),
		rsF("response.in_progress", rsProgressModel(t, 2, "other-model")),
	}
	dec := NewResponsesStreamDecoder(DefaultLimits())
	var rerr *RouterError
	for i, f := range frames {
		if _, e := dec.DecodeFrame(f); e != nil {
			if i != 2 {
				t.Fatalf("drift guard fired at frame %d, want frame 2: %v", i, e)
			}
			rerr = e
		}
	}
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("model drift must fail 502, got %v", rerr)
	}
	if strings.Contains(rerr.Msg, "other-model") || strings.Contains(rerr.Msg, "gpt-4o") {
		t.Fatalf("drift diagnostic echoes upstream value: %q", rerr.Msg)
	}
	// Drift at terminal against a known progress model.
	usage := rsUsage(1, 1)
	badResp := rsRespID(t, "resp_7", "other-model", "completed", []any{rsMsgOut("msg_1", "hi")}, usage, "")
	rerr = rsMustFail(t, []Frame{
		rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
		rsF("response.output_item.added", rsMsgAdded(t, 1, 0, "msg_1")),
		rsF("response.content_part.added", rsPartAdded(t, 2, 0, "msg_1", "output_text")),
		rsF("response.output_text.delta", rsDelta(t, 3, 0, "msg_1", "hi", false)),
		rsF("response.output_text.done", rsDone(t, 4, 0, "msg_1", "hi", false)),
		rsF("response.content_part.done", rsPartDone(t, 5, 0, "msg_1", "hi", false)),
		rsF("response.output_item.done", rsItemDoneMsg(t, 6, 0, "msg_1", "hi", false)),
		rsF("response.completed", rsTerminal(t, 7, "completed", badResp)),
	})
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("terminal drift must fail 502, got %v", rerr)
	}
	// Supplied-empty model at progress stays a 502 (omission is
	// absent/null only; "" is a wire value).
	created := rsCreatedRaw(t, 0, "resp_7", "gpt-4o")
	emptyProgress := strings.Replace(rsProgressModel(t, 1, "x"), `"model":"x"`, `"model":""`, 1)
	rsMustFailAt(t, []Frame{
		rsF("response.created", created),
		rsF("response.in_progress", emptyProgress),
	}, 1)

	// Lifecycle: meta before start, empty report, drift, after completion.
	life := NewResponseLifecycle(DefaultLimits(), DialectResponses)
	if rerr := life.Observe(ResponseModelEvent{Model: "gpt-4o"}); rerr == nil {
		t.Fatal("model before start accepted")
	}
	observeAll(t, life, ResponseStart{ID: "r1", Model: "", Source: DialectResponses})
	if rerr := life.Observe(ResponseModelEvent{Model: ""}); rerr == nil {
		t.Fatal("empty model report accepted")
	}
	observeAll(t, life, ResponseModelEvent{Model: "gpt-4o"})
	if rerr := life.Observe(ResponseModelEvent{Model: "other"}); rerr == nil {
		t.Fatal("model drift accepted")
	} else if strings.Contains(rerr.Msg, "other") {
		t.Fatalf("drift diagnostic echoes value: %q", rerr.Msg)
	}
	// Identical repeat is a no-op, then terminal contract closes.
	observeAll(t, life, ResponseModelEvent{Model: "gpt-4o"})
	observeAll(t, life,
		BlockOpen{Item: "t0", Kind: BlockText},
		TextFragment{Item: "t0", Text: "hi"},
		BlockClose{Item: "t0"},
		UsageEvent{Usage: mustTerminalUsage(t, 5, 2)},
		CompletionEvent{Finish: mustFinish(t, FinishStop)})
	if rerr := life.Observe(ResponseModelEvent{Model: "gpt-4o"}); rerr == nil {
		t.Fatal("model after completion accepted")
	}
	if _, rerr := life.Result(); rerr != nil {
		t.Fatalf("Result after promotion: %v", rerr)
	}

	// Encoders: meta before start and after completion fail with no bytes.
	lim := DefaultLimits()
	var buf bytes.Buffer
	chat := NewChatEventEncoder("public", true, lim)
	if eerr := chat.Encode(&buf, nil, ResponseModelEvent{Model: "m"}); eerr == nil {
		t.Fatal("Chat model before start accepted")
	}
	if buf.Len() != 0 {
		t.Fatalf("Chat pre-start failure wrote %d bytes", buf.Len())
	}
	anth := NewAnthropicEventEncoder("public", lim)
	if eerr := anth.Encode(&buf, nil, ResponseModelEvent{Model: "m"}); eerr == nil {
		t.Fatal("Anthropic model before start accepted")
	}
	respEnc := NewResponsesEventEncoder("public", lim)
	if eerr := respEnc.Encode(&buf, nil, ResponseModelEvent{Model: "m"}); eerr == nil {
		t.Fatal("Responses model before start accepted")
	}
	gem := NewGeminiEventEncoder("public", lim, true)
	if eerr := gem.Encode(&buf, nil, ResponseModelEvent{Model: "m"}); eerr == nil {
		t.Fatal("Gemini model before start accepted")
	}
}

// --- Missing model at the final complete envelope stays 502 ----------

func TestResponsesStreamModelMissingAtComplete(t *testing.T) {
	usage := rsUsage(2, 1)
	// Terminal envelope without a model is not valid complete wire:
	// the shared validator still requires an actual model.
	noModelResp := rsRespID(t, "resp_7", "", "completed", []any{rsMsgOut("msg_1", "hi")}, usage, "")
	frames := []Frame{
		rsF("response.created", rsCreatedNoModel(t, 0)),
		rsF("response.output_item.added", rsMsgAdded(t, 1, 0, "msg_1")),
		rsF("response.content_part.added", rsPartAdded(t, 2, 0, "msg_1", "output_text")),
		rsF("response.output_text.delta", rsDelta(t, 3, 0, "msg_1", "hi", false)),
		rsF("response.output_text.done", rsDone(t, 4, 0, "msg_1", "hi", false)),
		rsF("response.content_part.done", rsPartDone(t, 5, 0, "msg_1", "hi", false)),
		rsF("response.output_item.done", rsItemDoneMsg(t, 6, 0, "msg_1", "hi", false)),
		rsF("response.completed", rsTerminal(t, 7, "completed", noModelResp)),
	}
	dec := NewResponsesStreamDecoder(DefaultLimits())
	for i, f := range frames {
		evs, rerr := dec.DecodeFrame(f)
		if rerr != nil {
			if i != 7 || rerr.Status != 502 {
				t.Fatalf("missing terminal model must fail 502 at frame 7, got frame %d: %v", i, rerr)
			}
			for _, ev := range evs {
				if _, ok := ev.(UsageEvent); ok {
					t.Fatal("failed terminal must not emit UsageEvent")
				}
				if _, ok := ev.(CompletionEvent); ok {
					t.Fatal("failed terminal must not emit CompletionEvent")
				}
			}
			return
		}
	}
	t.Fatal("terminal without model accepted")
}

// --- Chat source still requires a model; emits no model events -------

func TestResponsesStreamModelChatSourceContract(t *testing.T) {
	// Chat target stage: Responses may start empty (truthful absence),
	// Chat may not; no fake fallback is invented either way.
	lim := DefaultLimits()
	var buf bytes.Buffer
	chatEmpty := NewChatEventEncoder("public", true, lim)
	if eerr := chatEmpty.Encode(&buf, nil, ResponseStart{ID: "r", Model: "", Source: DialectResponses}); eerr != nil {
		t.Fatalf("Chat stage must accept empty-model Responses start: %v", eerr.Err)
	}
	// Start flushes the public role immediately; a later model report
	// changes only metadata and must not emit another wire frame.
	if !strings.Contains(buf.String(), `"role":"assistant"`) {
		t.Fatal("Chat start did not flush the assistant role")
	}
	before := buf.Len()
	if eerr := chatEmpty.Encode(&buf, nil, ResponseModelEvent{Model: "provider-model"}); eerr != nil {
		t.Fatalf("late model report failed: %v", eerr.Err)
	}
	if buf.Len() != before {
		t.Fatal("late model report wrote downstream bytes")
	}
	chatBad := NewChatEventEncoder("public", true, lim)
	if eerr := chatBad.Encode(&buf, nil, ResponseStart{ID: "r", Model: "", Source: DialectChat}); eerr == nil {
		t.Fatal("Chat stage accepted empty-model Chat start")
	}
	// Actual runtime Chat source streams never emit model events.
	evs := csFullSuccess(t, append([]string{csChunk(`{"role":"assistant","content":"hi"}`, "")}, csTerm("stop", 5, 2)...))
	for _, ev := range evs {
		if _, ok := ev.(ResponseModelEvent); ok {
			t.Fatalf("Chat source emitted a model event: %#v", ev)
		}
	}
}
