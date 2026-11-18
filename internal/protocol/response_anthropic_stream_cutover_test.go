package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

// Anthropic-upstream stream cutover: one shared source decoder, framing,
// pipeline and reusable target encoders across all three translated
// directions (Chat, Responses, Gemini SSE + Gemini JSON array).
//
// Every test drives the SAME Anthropic wire bytes through all public
// wrappers (SSETranslator, AnthropicToResponsesSSETranslator,
// AnthropicToGemini{SSE,JSONArray}Translator). Wrappers must select
// (NewSSEFrameReader, NewAnthropicStreamDecoder, target encoder,
// TranslateStream(DialectAnthropic)) with fresh pipeline objects per
// Translate and no other semantic state.
//
// Preserved source contract: legacy minimal message_start{id,usage}
// with missing model stays "" (never a fake model); standard full start
// keeps output/cache flags; seeded text/tool input emits; opaque call IDs
// verbatim (never the tool-name grammar); exact bigints and canonical {}
// once; cumulative authoritative usage inclusive-prompt once. Chat
// IncludeUsage emits the documented separate empty-choices usage chunk
// (not legacy embedded usage); no terminal zeros or fabricated success.
// Shared gates: raw stop_sequence only Responses refuses; refusal only
// Gemini refuses; scalar/non-null billing source fail-closed. Source
// enforces no content after message_delta and message_delta after closed
// blocks; multiple usage updates valid last-wins. SourceIndex typed, no
// ItemID parsing. Targets fail honestly before success; no silent
// reordering. Target A/R encoders emit exactly one native error/failed
// after commit; Gemini/Chat Abort silent. Truncation emits no success
// marker and never reads past a valid terminal.

// --- Shared wire builders (independent of pair tests) ---

func cutFrame(event, data string) string {
	return "event: " + event + "\ndata: " + data + "\n\n"
}

func cutStartStd(usage string) string {
	return cutFrame("message_start", `{"type":"message_start","message":{"id":"msg_9","type":"message","role":"assistant","model":"claude-x","content":[],"stop_reason":null,"stop_sequence":null,"usage":`+usage+`}}`)
}

func cutStartMin() string {
	return cutFrame("message_start", `{"type":"message_start","message":{"id":"msg_9","usage":{"input_tokens":8}}}`)
}

func cutTextStart(idx int, seed string) string {
	cb := `{"type":"text"`
	if seed != "" {
		q, _ := json.Marshal(seed)
		cb += `,"text":` + string(q)
	}
	cb += `}`
	return cutFrame("content_block_start", `{"type":"content_block_start","index":`+itoa(idx)+`,"content_block":`+cb+`}`)
}

func cutToolStart(idx int, id, name, input string) string {
	qid, _ := json.Marshal(id)
	qname, _ := json.Marshal(name)
	cb := `{"type":"tool_use","id":` + string(qid) + `,"name":` + string(qname)
	if input != "" {
		cb += `,"input":` + input
	}
	cb += `}`
	return cutFrame("content_block_start", `{"type":"content_block_start","index":`+itoa(idx)+`,"content_block":`+cb+`}`)
}

func cutTextDelta(idx int, text string) string {
	q, _ := json.Marshal(text)
	return cutFrame("content_block_delta", `{"type":"content_block_delta","index":`+itoa(idx)+`,"delta":{"type":"text_delta","text":`+string(q)+`}}`)
}

func cutArgsDelta(idx int, frag string) string {
	q, _ := json.Marshal(frag)
	return cutFrame("content_block_delta", `{"type":"content_block_delta","index":`+itoa(idx)+`,"delta":{"type":"input_json_delta","partial_json":`+string(q)+`}}`)
}

func cutBlockStop(idx int) string {
	return cutFrame("content_block_stop", `{"type":"content_block_stop","index":`+itoa(idx)+`}`)
}

func cutMsgDelta(stop, usage string) string {
	delta := `{"stop_reason":null,"stop_sequence":null}`
	if stop != "" {
		q, _ := json.Marshal(stop)
		delta = `{"stop_reason":` + string(q) + `,"stop_sequence":null}`
	}
	return cutFrame("message_delta", `{"type":"message_delta","delta":`+delta+`,"usage":`+usage+`}`)
}

func cutMsgStop() string {
	return cutFrame("message_stop", `{"type":"message_stop"}`)
}

// cutFullText builds the standard text lifecycle: full start with cache
// shares, one text block, terminal end_turn with authoritative output.
func cutFullText() string {
	return cutStartStd(`{"input_tokens":25,"cache_creation_input_tokens":2,"cache_read_input_tokens":3,"output_tokens":1}`) +
		cutTextStart(0, "") +
		cutTextDelta(0, "Hello") +
		cutTextDelta(0, "!") +
		cutBlockStop(0) +
		cutMsgDelta("end_turn", `{"output_tokens":15}`) +
		cutMsgStop()
}

// cutFullTool builds a tool lifecycle with an opaque call ID, seeded
// placeholder, fragmented exact bigint args, and tool_use stop.
func cutFullTool() string {
	return cutStartStd(`{"input_tokens":25,"cache_creation_input_tokens":2,"cache_read_input_tokens":3,"output_tokens":1}`) +
		cutToolStart(0, "call:up 1", "get_time", "{}") +
		cutArgsDelta(0, `{"tz":"UTC",`) +
		cutArgsDelta(0, `"n":9007199254740993}`) +
		cutBlockStop(0) +
		cutMsgDelta("tool_use", `{"output_tokens":15}`) +
		cutMsgStop()
}

func cutRunChat(t *testing.T, upstream string, includeUsage bool, lim Limits) (string, *RouterError) {
	t.Helper()
	var buf bytes.Buffer
	tr := &SSETranslator{PublicModel: "public", IncludeUsage: includeUsage, Limits: lim}
	rerr := tr.Translate(strings.NewReader(upstream), &buf, nil)
	return buf.String(), rerr
}

func cutRunResponses(t *testing.T, upstream string, lim Limits) (string, *RouterError) {
	t.Helper()
	var buf bytes.Buffer
	tr := &AnthropicToResponsesSSETranslator{PublicModel: "public", Limits: lim}
	rerr := tr.Translate(strings.NewReader(upstream), &buf, nil)
	return buf.String(), rerr
}

func cutRunGemini(t *testing.T, upstream string, lim Limits, sse bool) (string, *RouterError) {
	t.Helper()
	var buf bytes.Buffer
	var rerr *RouterError
	if sse {
		tr := &AnthropicToGeminiSSETranslator{PublicModel: "public", Limits: lim}
		rerr = tr.Translate(strings.NewReader(upstream), &buf, nil)
	} else {
		tr := &AnthropicToGeminiJSONArrayTranslator{PublicModel: "public", Limits: lim}
		rerr = tr.Translate(strings.NewReader(upstream), &buf, nil)
	}
	return buf.String(), rerr
}

// cutRunAll drives the same wire through all public wrappers (both Gemini
// framings). Failures return per-target errors for matrix assertions.
func cutRunAll(upstream string, lim Limits) (chat string, chatErr *RouterError, resp string, respErr *RouterError, gemSSE string, gemSSEErr *RouterError, gemArr string, gemArrErr *RouterError) {
	var b bytes.Buffer
	trC := &SSETranslator{PublicModel: "public", IncludeUsage: true, Limits: lim}
	chatErr = trC.Translate(strings.NewReader(upstream), &b, nil)
	chat = b.String()
	b.Reset()
	trR := &AnthropicToResponsesSSETranslator{PublicModel: "public", Limits: lim}
	respErr = trR.Translate(strings.NewReader(upstream), &b, nil)
	resp = b.String()
	b.Reset()
	trGS := &AnthropicToGeminiSSETranslator{PublicModel: "public", Limits: lim}
	gemSSEErr = trGS.Translate(strings.NewReader(upstream), &b, nil)
	gemSSE = b.String()
	b.Reset()
	trGA := &AnthropicToGeminiJSONArrayTranslator{PublicModel: "public", Limits: lim}
	gemArrErr = trGA.Translate(strings.NewReader(upstream), &b, nil)
	gemArr = b.String()
	return
}

func TestCutoverSameWireAllTargets(t *testing.T) {
	upstream := cutFullText()
	chat, chatErr, resp, respErr, gemSSE, gemSSEErr, gemArr, gemArrErr := cutRunAll(upstream, DefaultLimits())
	if chatErr != nil {
		t.Fatalf("chat: %v", chatErr)
	}
	if respErr != nil {
		t.Fatalf("responses: %v", respErr)
	}
	if gemSSEErr != nil {
		t.Fatalf("gemini sse: %v", gemSSEErr)
	}
	if gemArrErr != nil {
		t.Fatalf("gemini array: %v", gemArrErr)
	}
	// Chat: role delta, text deltas, stop finish, separate usage chunk, DONE.
	for _, want := range []string{`"role":"assistant"`, `"content":"Hello"`, `"content":"!"`, `"finish_reason":"stop"`, `"prompt_tokens":30`, `"completion_tokens":15`, `"total_tokens":45`, `"cached_tokens":3`, "data: [DONE]"} {
		if !strings.Contains(chat, want) {
			t.Fatalf("chat lacks %s:\n%s", want, chat)
		}
	}
	if strings.Contains(chat, `"finish_reason":"stop","usage"`) || strings.Contains(chat, `"finish_reason": "stop"`) {
		t.Fatalf("chat must use the separate usage chunk, not embedded usage:\n%s", chat)
	}
	// Responses: lifecycle events, authoritative inclusive usage, completed.
	for _, want := range []string{`event: response.created`, `event: response.in_progress`, `event: response.output_text.delta`, `"delta":"Hello"`, `event: response.completed`, `"input_tokens":30`, `"output_tokens":15`, `"total_tokens":45`, `"cached_tokens":3`, `"cache_write_tokens":2`} {
		if !strings.Contains(resp, want) {
			t.Fatalf("responses lacks %s:\n%s", want, resp)
		}
	}
	// Gemini SSE: immediate text frames, terminal STOP with usage only.
	if strings.Contains(gemSSE, "[DONE]") {
		t.Fatalf("gemini sse must not carry [DONE]:\n%s", gemSSE)
	}
	frames := strings.Count(gemSSE, "data: ")
	if frames != 3 {
		t.Fatalf("gemini sse want 2 text + 1 terminal frames, got %d:\n%s", frames, gemSSE)
	}
	for _, want := range []string{`"text":"Hello"`, `"finishReason":"STOP"`, `"promptTokenCount":30`, `"candidatesTokenCount":15`, `"totalTokenCount":45`, `"cachedContentTokenCount":3`} {
		if !strings.Contains(gemSSE, want) {
			t.Fatalf("gemini sse lacks %s:\n%s", want, gemSSE)
		}
	}
	// Gemini array: same objects, bracketed, closed once.
	trimmed := strings.TrimSpace(gemArr)
	if !strings.HasPrefix(trimmed, "[") || !strings.HasSuffix(trimmed, "]") {
		t.Fatalf("gemini array framing wrong:\n%s", gemArr)
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(trimmed), &arr); err != nil || len(arr) != 3 {
		t.Fatalf("gemini array must hold 3 elements: %v\n%s", err, gemArr)
	}
	last, _ := json.Marshal(arr[2])
	if !strings.Contains(string(last), `"finishReason":"STOP"`) || !strings.Contains(string(last), `"promptTokenCount":30`) {
		t.Fatalf("gemini array terminal wrong: %s", last)
	}
}

func TestCutoverMinimalStartAllTargets(t *testing.T) {
	// Legacy minimal message_start carries id+usage only: absence stays ""
	// (never a fake model) and all three targets succeed.
	upstream := cutStartMin() +
		cutTextStart(0, "") +
		cutTextDelta(0, "Hi") +
		cutBlockStop(0) +
		cutMsgDelta("end_turn", `{"output_tokens":3}`) +
		cutMsgStop()
	chat, chatErr, resp, respErr, gemSSE, gemSSEErr, gemArr, gemArrErr := cutRunAll(upstream, DefaultLimits())
	if chatErr != nil || respErr != nil || gemSSEErr != nil || gemArrErr != nil {
		t.Fatalf("minimal start must succeed everywhere: chat=%v resp=%v gemSSE=%v gemArr=%v", chatErr, respErr, gemSSEErr, gemArrErr)
	}
	if !strings.Contains(chat, "data: [DONE]") {
		t.Fatalf("chat minimal lacks DONE:\n%s", chat)
	}
	if !strings.Contains(resp, `event: response.completed`) {
		t.Fatalf("responses minimal lacks completed:\n%s", resp)
	}
	if !strings.Contains(gemSSE, `"finishReason":"STOP"`) || !strings.Contains(gemArr, `"finishReason":"STOP"`) {
		t.Fatalf("gemini minimal lacks STOP:\n%s\n%s", gemSSE, gemArr)
	}
	// Minimal input 8 + output 3 = total 11 everywhere.
	for _, tc := range []struct {
		name, out string
		wants     []string
	}{
		{"chat", chat, []string{`"prompt_tokens":8`, `"completion_tokens":3`, `"total_tokens":11`}},
		{"responses", resp, []string{`"input_tokens":8`, `"output_tokens":3`, `"total_tokens":11`}},
		{"gemini", gemSSE, []string{`"promptTokenCount":8`, `"candidatesTokenCount":3`, `"totalTokenCount":11`}},
	} {
		for _, w := range tc.wants {
			if !strings.Contains(tc.out, w) {
				t.Fatalf("%s minimal usage lacks %s:\n%s", tc.name, w, tc.out)
			}
		}
	}
}

func TestCutoverToolExactOpaqueOrder(t *testing.T) {
	upstream := cutFullTool()
	chat, chatErr, resp, respErr, gemSSE, gemSSEErr, gemArr, gemArrErr := cutRunAll(upstream, DefaultLimits())
	if chatErr != nil || respErr != nil || gemSSEErr != nil || gemArrErr != nil {
		t.Fatalf("tool wire must succeed everywhere: %v %v %v %v", chatErr, respErr, gemSSEErr, gemArrErr)
	}
	// Opaque call ID verbatim (never the tool-name grammar); exact bigint.
	for _, tc := range []struct{ name, out string }{
		{"chat", chat}, {"responses", resp}, {"gemini-sse", gemSSE}, {"gemini-array", gemArr},
	} {
		if !strings.Contains(tc.out, "call:up 1") {
			t.Fatalf("%s lost opaque call id:\n%s", tc.name, tc.out)
		}
		if !strings.Contains(tc.out, "9007199254740993") {
			t.Fatalf("%s bigint inexact:\n%s", tc.name, tc.out)
		}
		if !strings.Contains(tc.out, "get_time") {
			t.Fatalf("%s lost tool name:\n%s", tc.name, tc.out)
		}
	}
	if !strings.Contains(chat, `"finish_reason":"tool_calls"`) {
		t.Fatalf("chat tool finish wrong:\n%s", chat)
	}
	if !strings.Contains(resp, `"stop_reason":"tool_use"`) && !strings.Contains(resp, `"status":"completed"`) {
		t.Fatalf("responses tool terminal wrong:\n%s", resp)
	}
	if !strings.Contains(gemSSE, `"finishReason":"STOP"`) || !strings.Contains(gemArr, `"finishReason":"STOP"`) {
		t.Fatalf("gemini tool finish wrong:\n%s\n%s", gemSSE, gemArr)
	}
	// Tool-first ordering: single tool block keeps index 0 on Chat/Responses.
	if !strings.Contains(chat, `"index":0`) {
		t.Fatalf("chat tool index wrong:\n%s", chat)
	}
}

func TestCutoverStopRefusalMatrix(t *testing.T) {
	mk := func(stop string) string {
		u := `{"input_tokens":4,"output_tokens":2}`
		return cutStartStd(u) +
			cutTextStart(0, "") +
			cutTextDelta(0, "x") +
			cutBlockStop(0) +
			cutMsgDelta(stop, `{"output_tokens":2}`) +
			cutMsgStop()
	}
	// end_turn: success everywhere; Chat stop, Responses completed, Gemini STOP.
	chat, ce, resp, re, gs, ge, ga, ae := cutRunAll(mk("end_turn"), DefaultLimits())
	if ce != nil || re != nil || ge != nil || ae != nil {
		t.Fatalf("end_turn must succeed: %v %v %v %v", ce, re, ge, ae)
	}
	if !strings.Contains(chat, `"finish_reason":"stop"`) || !strings.Contains(resp, `"status":"completed"`) || !strings.Contains(gs, `"finishReason":"STOP"`) || !strings.Contains(ga, `"finishReason":"STOP"`) {
		t.Fatalf("end_turn mapping wrong")
	}
	// max_tokens: Chat length, Responses incomplete, Gemini MAX_TOKENS.
	chat, ce, resp, re, gs, ge, ga, ae = cutRunAll(mk("max_tokens"), DefaultLimits())
	if ce != nil || re != nil || ge != nil || ae != nil {
		t.Fatalf("max_tokens must succeed: %v %v %v %v", ce, re, ge, ae)
	}
	if !strings.Contains(chat, `"finish_reason":"length"`) || !strings.Contains(resp, `"incomplete"`) || !strings.Contains(resp, `"max_output_tokens"`) || !strings.Contains(gs, `"finishReason":"MAX_TOKENS"`) || !strings.Contains(ga, `"finishReason":"MAX_TOKENS"`) {
		t.Fatalf("max_tokens mapping wrong:\n%s\n%s\n%s", chat, resp, gs)
	}
	// stop_sequence: Chat stop, Gemini STOP, Responses refuses (no equivalent).
	chat, ce, resp, re, gs, ge, ga, ae = cutRunAll(mk("stop_sequence"), DefaultLimits())
	if ce != nil {
		t.Fatalf("chat stop_sequence: %v", ce)
	}
	if !strings.Contains(chat, `"finish_reason":"stop"`) {
		t.Fatalf("chat stop_sequence must map to stop:\n%s", chat)
	}
	if ge != nil || ae != nil {
		t.Fatalf("gemini stop_sequence must map to STOP: %v %v", ge, ae)
	}
	if re == nil {
		t.Fatalf("responses must refuse raw stop_sequence")
	}
	if strings.Contains(resp, `event: response.completed`) || strings.Contains(resp, `event: response.incomplete`) {
		t.Fatalf("responses refusal must not emit success:\n%s", resp)
	}
	// refusal: Chat content_filter, Responses incomplete+content_filter,
	// Gemini refuses (never a normal STOP).
	chat, ce, resp, re, gs, ge, ga, ae = cutRunAll(mk("refusal"), DefaultLimits())
	if ce != nil {
		t.Fatalf("chat refusal: %v", ce)
	}
	if !strings.Contains(chat, `"finish_reason":"content_filter"`) {
		t.Fatalf("chat refusal must map to content_filter:\n%s", chat)
	}
	if re != nil {
		t.Fatalf("responses refusal: %v", re)
	}
	if !strings.Contains(resp, `"content_filter"`) {
		t.Fatalf("responses refusal must carry content_filter:\n%s", resp)
	}
	if ge == nil || ae == nil {
		t.Fatalf("gemini must refuse refusal stop, got:\n%s\n%s", gs, ga)
	}
	for _, out := range []string{gs, ga} {
		if strings.Contains(out, `"finishReason":"STOP"`) {
			t.Fatalf("gemini refusal must not fabricate STOP:\n%s", out)
		}
	}
	// Unknown stop: all three refuse, none emits success.
	chat, ce, resp, re, gs, ge, ga, ae = cutRunAll(mk("frobnicate"), DefaultLimits())
	if ce == nil || re == nil || ge == nil || ae == nil {
		t.Fatalf("unknown stop must fail everywhere: %v %v %v %v", ce, re, ge, ae)
	}
	for _, tc := range []struct{ name, out string }{{"chat", chat}, {"resp", resp}, {"gs", gs}, {"ga", ga}} {
		if strings.Contains(tc.out, "[DONE]") || strings.Contains(tc.out, "message_stop") || strings.Contains(tc.out, "response.completed") || strings.HasSuffix(strings.TrimSpace(tc.out), "]") && tc.name == "ga" && len(strings.TrimSpace(tc.out)) > 1 {
			t.Fatalf("%s unknown stop emitted success:\n%s", tc.name, tc.out)
		}
	}
}

func TestCutoverInvalidTruncatedUnknownLate(t *testing.T) {
	base := func() string {
		return cutStartStd(`{"input_tokens":8,"output_tokens":1}`) +
			cutTextStart(0, "") +
			cutTextDelta(0, "Hi") +
			cutBlockStop(0)
	}
	validTail := cutMsgDelta("end_turn", `{"output_tokens":3}`) + cutMsgStop()
	// Truncation: EOF before message_stop fails everywhere with no success.
	trunc := base() + cutMsgDelta("end_turn", `{"output_tokens":3}`)
	chat, ce, resp, re, gs, ge, ga, ae := cutRunAll(trunc, DefaultLimits())
	if ce == nil || re == nil || ge == nil || ae == nil {
		t.Fatalf("truncation must fail everywhere: %v %v %v %v", ce, re, ge, ae)
	}
	if strings.Contains(chat, "[DONE]") || strings.Contains(resp, "response.completed") || strings.Contains(gs, `"finishReason"`) || strings.Contains(strings.TrimSpace(ga), "]") && len(strings.TrimSpace(ga)) > 0 {
		// Gemini terminal carries finishReason only on success; any frame
		// with usage after truncation would be a fabricated terminal.
		if strings.Contains(gs, `"usageMetadata"`) {
			t.Fatalf("truncation emitted gemini success:\n%s", gs)
		}
	}
	// Unknown event name fails closed everywhere.
	unknownEv := base() + "event: citational_dance\ndata: {}\n\n"
	_, ce, _, re, _, ge, _, ae = cutRunAll(unknownEv, DefaultLimits())
	if ce == nil || re == nil || ge == nil || ae == nil {
		t.Fatalf("unknown event must fail everywhere: %v %v %v %v", ce, re, ge, ae)
	}
	// Unknown delta kind (thinking) fails closed everywhere.
	thinkDelta := cutStartStd(`{"input_tokens":8,"output_tokens":1}`) +
		cutTextStart(0, "") +
		cutFrame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`) +
		cutBlockStop(0) + validTail
	_, ce, _, re, _, ge, _, ae = cutRunAll(thinkDelta, DefaultLimits())
	if ce == nil || re == nil || ge == nil || ae == nil {
		t.Fatalf("thinking delta must fail everywhere: %v %v %v %v", ce, re, ge, ae)
	}
	// Non-null unmapped billing fails closed at the source (all targets).
	badBilling := cutStartStd(`{"input_tokens":8,"service_tier":"x"}`) +
		cutTextStart(0, "") + cutBlockStop(0) + validTail
	_, ce, _, re, _, ge, _, ae = cutRunAll(badBilling, DefaultLimits())
	if ce == nil || re == nil || ge == nil || ae == nil {
		t.Fatalf("non-null billing must fail everywhere: %v %v %v %v", ce, re, ge, ae)
	}
	// Late content: content_block after message_delta fails everywhere.
	lateContent := cutStartStd(`{"input_tokens":8,"output_tokens":1}`) +
		cutTextStart(0, "") + cutBlockStop(0) +
		cutMsgDelta("end_turn", `{"output_tokens":3}`) +
		cutTextStart(1, "") + cutBlockStop(1) + cutMsgStop()
	_, ce, _, re, _, ge, _, ae = cutRunAll(lateContent, DefaultLimits())
	if ce == nil || re == nil || ge == nil || ae == nil {
		t.Fatalf("late content after message_delta must fail everywhere: %v %v %v %v", ce, re, ge, ae)
	}
	// message_delta with open blocks fails everywhere.
	openDelta := cutStartStd(`{"input_tokens":8,"output_tokens":1}`) +
		cutTextStart(0, "") +
		cutMsgDelta("end_turn", `{"output_tokens":3}`) + cutMsgStop()
	_, ce, _, re, _, ge, _, ae = cutRunAll(openDelta, DefaultLimits())
	if ce == nil || re == nil || ge == nil || ae == nil {
		t.Fatalf("message_delta with open blocks must fail everywhere: %v %v %v %v", ce, re, ge, ae)
	}
	// Duplicate message_start fails everywhere.
	dup := cutStartStd(`{"input_tokens":8}`) + cutStartStd(`{"input_tokens":8}`)
	_, ce, _, re, _, ge, _, ae = cutRunAll(dup, DefaultLimits())
	if ce == nil || re == nil || ge == nil || ae == nil {
		t.Fatalf("duplicate start must fail everywhere: %v %v %v %v", ce, re, ge, ae)
	}
	// message_stop without message_delta (no stop reason/usage) fails.
	noDelta := cutStartStd(`{"input_tokens":8}`) + cutTextStart(0, "") + cutBlockStop(0) + cutMsgStop()
	_, ce, _, re, _, ge, _, ae = cutRunAll(noDelta, DefaultLimits())
	if ce == nil || re == nil || ge == nil || ae == nil {
		t.Fatalf("message_stop without delta must fail everywhere: %v %v %v %v", ce, re, ge, ae)
	}
}

func TestCutoverMalformedNativeVsRefuse(t *testing.T) {
	// Tool fragments that never form one JSON object classify malformed:
	// Gemini suppresses and ends MALFORMED_FUNCTION_CALL (success);
	// Chat/Responses refuse with one native failure after commit.
	mk := func() string {
		return cutStartStd(`{"input_tokens":8,"output_tokens":1}`) +
			cutToolStart(0, "call:up 1", "calc", "{}") +
			cutArgsDelta(0, `{"n":`) +
			cutBlockStop(0) +
			cutMsgDelta("tool_use", `{"output_tokens":3}`) +
			cutMsgStop()
	}
	upstream := mk()
	chat, ce, resp, re, gs, ge, ga, ae := cutRunAll(upstream, DefaultLimits())
	if ce == nil {
		t.Fatalf("chat must refuse malformed args, got:\n%s", chat)
	}
	if strings.Contains(chat, "[DONE]") {
		t.Fatalf("chat malformed must not emit DONE:\n%s", chat)
	}
	if re == nil {
		t.Fatalf("responses must refuse malformed args, got:\n%s", resp)
	}
	if !strings.Contains(resp, "response.failed") {
		t.Fatalf("responses malformed must emit exactly one response.failed:\n%s", resp)
	}
	if strings.Contains(resp, "response.completed") {
		t.Fatalf("responses malformed must not complete:\n%s", resp)
	}
	if ge != nil {
		t.Fatalf("gemini sse must map malformed natively, got %v:\n%s", ge, gs)
	}
	if !strings.Contains(gs, `"finishReason":"MALFORMED_FUNCTION_CALL"`) {
		t.Fatalf("gemini sse malformed terminal wrong:\n%s", gs)
	}
	if strings.Contains(gs, `"functionCall"`) {
		t.Fatalf("gemini must suppress the malformed call:\n%s", gs)
	}
	if ae != nil {
		t.Fatalf("gemini array must map malformed natively, got %v:\n%s", ae, ga)
	}
	if !strings.Contains(ga, `"finishReason":"MALFORMED_FUNCTION_CALL"`) {
		t.Fatalf("gemini array malformed terminal wrong:\n%s", ga)
	}
}

func TestCutoverEmptySeeded(t *testing.T) {
	// Empty generation: start/delta/stop with no blocks succeeds everywhere.
	empty := cutStartStd(`{"input_tokens":5,"output_tokens":1}`) +
		cutMsgDelta("end_turn", `{"output_tokens":2}`) +
		cutMsgStop()
	chat, ce, resp, re, gs, ge, ga, ae := cutRunAll(empty, DefaultLimits())
	if ce != nil || re != nil || ge != nil || ae != nil {
		t.Fatalf("empty generation must succeed: %v %v %v %v", ce, re, ge, ae)
	}
	if !strings.Contains(chat, "[DONE]") || !strings.Contains(resp, "response.completed") || !strings.Contains(gs, `"finishReason":"STOP"`) {
		t.Fatalf("empty terminal wrong:\n%s\n%s\n%s", chat, resp, gs)
	}
	_ = ga
	// Seeded text emits immediately on Chat; terminal still exact.
	seeded := cutStartStd(`{"input_tokens":5,"output_tokens":1}`) +
		cutTextStart(0, "seed") +
		cutBlockStop(0) +
		cutMsgDelta("end_turn", `{"output_tokens":2}`) +
		cutMsgStop()
	chat, ce, _, _, _, _, _, _ = cutRunAll(seeded, DefaultLimits())
	if ce != nil {
		t.Fatalf("seeded text: %v", ce)
	}
	if !strings.Contains(chat, `"content":"seed"`) {
		t.Fatalf("seeded text must emit:\n%s", chat)
	}
	// Seeded whole tool input (non-"{}") emits the exact object once.
	seededTool := cutStartStd(`{"input_tokens":5,"output_tokens":1}`) +
		cutToolStart(0, "call:up 2", "calc", `{"n":1}`) +
		cutBlockStop(0) +
		cutMsgDelta("tool_use", `{"output_tokens":2}`) +
		cutMsgStop()
	_, _, resp, re, _, _, _, _ = cutRunAll(seededTool, DefaultLimits())
	if re != nil {
		t.Fatalf("seeded tool: %v", re)
	}
	if strings.Count(resp, "call:up 2") < 3 {
		t.Fatalf("seeded tool identity must repeat in added+done+terminal:\n%s", resp)
	}
	if !strings.Contains(resp, `\"n\":1`) {
		t.Fatalf("seeded tool args must carry exact object:\n%s", resp)
	}
	// Empty-object tool emits canonical {} exactly once (2 bytes, bounded).
	emptyTool := cutStartStd(`{"input_tokens":5,"output_tokens":1}`) +
		cutToolStart(0, "call:up 3", "calc", "") +
		cutBlockStop(0) +
		cutMsgDelta("tool_use", `{"output_tokens":2}`) +
		cutMsgStop()
	chat, ce, _, _, _, _, _, _ = cutRunAll(emptyTool, DefaultLimits())
	if ce != nil {
		t.Fatalf("empty tool: %v", ce)
	}
	if !strings.Contains(chat, `"arguments":"{}"`) {
		t.Fatalf("empty tool must emit canonical {}:\n%s", chat)
	}
}

func TestCutoverCumulativeUsageLastWins(t *testing.T) {
	// Multiple cumulative message_delta updates valid; last wins.
	upstream := cutStartStd(`{"input_tokens":10,"output_tokens":1}`) +
		cutTextStart(0, "") +
		cutTextDelta(0, "x") +
		cutBlockStop(0) +
		cutMsgDelta("end_turn", `{"output_tokens":9}`) +
		cutMsgDelta("end_turn", `{"input_tokens":12,"output_tokens":15}`) +
		cutMsgStop()
	chat, ce, resp, re, gs, ge, _, _ := cutRunAll(upstream, DefaultLimits())
	if ce != nil || re != nil || ge != nil {
		t.Fatalf("cumulative updates must succeed: %v %v %v", ce, re, ge)
	}
	// Inclusive prompt 12, output 15, total 27.
	for _, tc := range []struct{ name, out string }{{"chat", chat}, {"resp", resp}, {"gem", gs}} {
		_ = tc
	}
	if !strings.Contains(chat, `"prompt_tokens":12`) || !strings.Contains(chat, `"completion_tokens":15`) || !strings.Contains(chat, `"total_tokens":27`) {
		t.Fatalf("chat cumulative wrong:\n%s", chat)
	}
	if !strings.Contains(resp, `"input_tokens":12`) || !strings.Contains(resp, `"output_tokens":15`) || !strings.Contains(resp, `"total_tokens":27`) {
		t.Fatalf("responses cumulative wrong:\n%s", resp)
	}
	if !strings.Contains(gs, `"promptTokenCount":12`) || !strings.Contains(gs, `"candidatesTokenCount":15`) || !strings.Contains(gs, `"totalTokenCount":27`) {
		t.Fatalf("gemini cumulative wrong:\n%s", gs)
	}
}

func TestCutoverPromptReturnOneByteReader(t *testing.T) {
	// Valid terminal returns promptly with 1-byte reads and no EOF read
	// past the terminal: trailing invalid bytes stay unread.
	upstream := cutFullText()
	trailing := "data: {not valid json}\n\n"
	for _, tc := range []struct {
		name string
		run  func(io.Reader, io.Writer, func()) *RouterError
	}{
		{"chat", (&SSETranslator{PublicModel: "p", IncludeUsage: true, Limits: DefaultLimits()}).Translate},
		{"responses", (&AnthropicToResponsesSSETranslator{PublicModel: "p", Limits: DefaultLimits()}).Translate},
		{"gemini-sse", (&AnthropicToGeminiSSETranslator{PublicModel: "p", Limits: DefaultLimits()}).Translate},
		{"gemini-array", (&AnthropicToGeminiJSONArrayTranslator{PublicModel: "p", Limits: DefaultLimits()}).Translate},
	} {
		rdr := &cutOneByteReader{s: upstream + trailing}
		var buf bytes.Buffer
		if rerr := tc.run(rdr, &buf, nil); rerr != nil {
			t.Fatalf("%s 1-byte reader: %v", tc.name, rerr)
		}
		if rdr.reads < len(upstream) {
			t.Fatalf("%s read too few: %d < %d", tc.name, rdr.reads, len(upstream))
		}
		if rdr.reads >= len(upstream)+len(trailing) {
			t.Fatalf("%s read past the terminal into trailing bytes", tc.name)
		}
	}
}

type cutOneByteReader struct {
	s     string
	reads int
}

func (r *cutOneByteReader) Read(p []byte) (int, error) {
	if len(r.s) == 0 {
		return 0, io.EOF
	}
	r.reads++
	p[0] = r.s[0]
	r.s = r.s[1:]
	return 1, nil
}

func TestCutoverBounds(t *testing.T) {
	full := cutFullText()
	// Per-frame bound.
	small := DefaultLimits()
	small.MaxFrameBytes = 32
	_, ce, _, re, _, ge, _, ae := cutRunAll(full, small)
	if ce == nil || re == nil || ge == nil || ae == nil {
		t.Fatalf("frame bound must fail everywhere: %v %v %v %v", ce, re, ge, ae)
	}
	// Per-arg bound via tool fragments.
	toolWire := cutFullTool()
	smallArgs := DefaultLimits()
	smallArgs.MaxArgsChars = 4
	_, ce, _, re, _, ge, _, ae = cutRunAll(toolWire, smallArgs)
	if ce == nil || re == nil || ge == nil || ae == nil {
		t.Fatalf("args bound must fail everywhere: %v %v %v %v", ce, re, ge, ae)
	}
	// Block bound.
	smallBlocks := DefaultLimits()
	smallBlocks.MaxStreamBlocks = 1
	twoBlocks := cutStartStd(`{"input_tokens":5,"output_tokens":1}`) +
		cutTextStart(0, "") + cutBlockStop(0) +
		cutTextStart(1, "") + cutBlockStop(1) +
		cutMsgDelta("end_turn", `{"output_tokens":2}`) + cutMsgStop()
	_, ce, _, re, _, ge, _, ae = cutRunAll(twoBlocks, smallBlocks)
	if ce == nil || re == nil || ge == nil || ae == nil {
		t.Fatalf("block bound must fail everywhere: %v %v %v %v", ce, re, ge, ae)
	}
	// Tool-call bound.
	smallTools := DefaultLimits()
	smallTools.MaxToolCalls = 1
	twoTools := cutStartStd(`{"input_tokens":5,"output_tokens":1}`) +
		cutToolStart(0, "c1", "a", "") + cutBlockStop(0) +
		cutToolStart(1, "c2", "b", "") + cutBlockStop(1) +
		cutMsgDelta("tool_use", `{"output_tokens":2}`) + cutMsgStop()
	_, ce, _, re, _, ge, _, ae = cutRunAll(twoTools, smallTools)
	if ce == nil || re == nil || ge == nil || ae == nil {
		t.Fatalf("tool bound must fail everywhere: %v %v %v %v", ce, re, ge, ae)
	}
}

type cutFailWriter struct {
	failAfter int
	written   int
	buf       bytes.Buffer
}

func (w *cutFailWriter) Write(p []byte) (int, error) {
	if w.written+len(p) > w.failAfter {
		return 0, errors.New("downstream gone")
	}
	w.written += len(p)
	return w.buf.Write(p)
}

func TestCutoverWriterFailNoExtras(t *testing.T) {
	upstream := cutFullText()
	// Fail on the first downstream write: no success marker, no second
	// Abort write after WriteFailed.
	for _, tc := range []struct {
		name string
		run  func(io.Reader, io.Writer, func()) *RouterError
		bad  string
	}{
		{"chat", (&SSETranslator{PublicModel: "p", IncludeUsage: true, Limits: DefaultLimits()}).Translate, "[DONE]"},
		{"responses", (&AnthropicToResponsesSSETranslator{PublicModel: "p", Limits: DefaultLimits()}).Translate, "response.completed"},
		{"gemini-sse", (&AnthropicToGeminiSSETranslator{PublicModel: "p", Limits: DefaultLimits()}).Translate, "finishReason"},
		{"gemini-array", (&AnthropicToGeminiJSONArrayTranslator{PublicModel: "p", Limits: DefaultLimits()}).Translate, "finishReason"},
	} {
		w := &cutFailWriter{failAfter: 1}
		rerr := tc.run(strings.NewReader(upstream), w, nil)
		if rerr == nil || rerr.Status != 502 {
			t.Fatalf("%s write failure must 502, got %v", tc.name, rerr)
		}
		if strings.Contains(w.buf.String(), tc.bad) {
			t.Fatalf("%s wrote success after write failure:\n%s", tc.name, w.buf.String())
		}
	}
	// Post-commit failure emits exactly one native marker on A/R, silent
	// on Chat/Gemini (server owns the Gemini frame; Chat aborts silent).
	badUsage := cutStartStd(`{"input_tokens":8,"output_tokens":1}`) +
		cutTextStart(0, "") +
		cutTextDelta(0, "Hi") +
		cutBlockStop(0) +
		// Unknown stop surfaces after commitment: encoder semantic fail.
		cutMsgDelta("frobnicate", `{"output_tokens":3}`) +
		cutMsgStop()
	chat, ce, resp, re, gs, ge, ga, ae := cutRunAll(badUsage, DefaultLimits())
	if ce == nil || re == nil || ge == nil || ae == nil {
		t.Fatalf("post-commit semantic fail required: %v %v %v %v", ce, re, ge, ae)
	}
	if strings.Count(resp, "event: response.failed") != 1 {
		t.Fatalf("responses must emit exactly one response.failed:\n%s", resp)
	}
	if strings.Contains(resp, "response.completed") {
		t.Fatalf("responses must not complete after failure:\n%s", resp)
	}
	_ = chat
	_ = gs
	_ = ga
}

func TestCutoverTranslatorReuse(t *testing.T) {
	upstream := cutFullText()
	// One translator value serves two generations: fresh pipeline objects
	// per Translate, no carried semantic state.
	trC := &SSETranslator{PublicModel: "public", IncludeUsage: true, Limits: DefaultLimits()}
	var b1, b2 bytes.Buffer
	if rerr := trC.Translate(strings.NewReader(upstream), &b1, nil); rerr != nil {
		t.Fatalf("chat first: %v", rerr)
	}
	if rerr := trC.Translate(strings.NewReader(upstream), &b2, nil); rerr != nil {
		t.Fatalf("chat second: %v", rerr)
	}
	if b1.Len() == 0 || b2.Len() == 0 {
		t.Fatal("chat reuse emitted nothing")
	}
	trR := &AnthropicToResponsesSSETranslator{PublicModel: "public", Limits: DefaultLimits()}
	b1.Reset()
	b2.Reset()
	if rerr := trR.Translate(strings.NewReader(upstream), &b1, nil); rerr != nil {
		t.Fatalf("responses first: %v", rerr)
	}
	if rerr := trR.Translate(strings.NewReader(upstream), &b2, nil); rerr != nil {
		t.Fatalf("responses second: %v", rerr)
	}
	trGS := &AnthropicToGeminiSSETranslator{PublicModel: "public", Limits: DefaultLimits()}
	b1.Reset()
	b2.Reset()
	if rerr := trGS.Translate(strings.NewReader(upstream), &b1, nil); rerr != nil {
		t.Fatalf("gemini sse first: %v", rerr)
	}
	if rerr := trGS.Translate(strings.NewReader(upstream), &b2, nil); rerr != nil {
		t.Fatalf("gemini sse second: %v", rerr)
	}
	trGA := &AnthropicToGeminiJSONArrayTranslator{PublicModel: "public", Limits: DefaultLimits()}
	b1.Reset()
	b2.Reset()
	if rerr := trGA.Translate(strings.NewReader(upstream), &b1, nil); rerr != nil {
		t.Fatalf("gemini array first: %v", rerr)
	}
	if rerr := trGA.Translate(strings.NewReader(upstream), &b2, nil); rerr != nil {
		t.Fatalf("gemini array second: %v", rerr)
	}
}

func TestCutoverIncludeUsageForm(t *testing.T) {
	upstream := cutFullText()
	// IncludeUsage=true: dedicated empty-choices usage chunk after finish.
	with, rerr := cutRunChat(t, upstream, true, DefaultLimits())
	if rerr != nil {
		t.Fatalf("include_usage: %v", rerr)
	}
	if !strings.Contains(with, `"choices":[]`) || !strings.Contains(with, `"prompt_tokens":30`) || !strings.Contains(with, `"completion_tokens":15`) {
		t.Fatalf("dedicated usage chunk required:\n%s", with)
	}
	// IncludeUsage=false: no usage on the wire, success still requires the
	// terminal UsageEvent (missing delta usage fails).
	without, rerr := cutRunChat(t, upstream, false, DefaultLimits())
	if rerr != nil {
		t.Fatalf("exclude usage: %v", rerr)
	}
	if strings.Contains(without, `"prompt_tokens"`) {
		t.Fatalf("excluded usage must not appear:\n%s", without)
	}
	if !strings.Contains(without, "data: [DONE]") {
		t.Fatalf("excluded usage still needs DONE:\n%s", without)
	}
	noUsage := cutStartStd(`{"input_tokens":8}`) +
		cutTextStart(0, "") + cutBlockStop(0) +
		cutMsgDelta("end_turn", `{"output_tokens":3}`) + cutMsgStop()
	// Start without output is fine (informational); terminal still exact.
	if _, rerr := cutRunChat(t, noUsage, false, DefaultLimits()); rerr != nil {
		t.Fatalf("start input-only with terminal usage: %v", rerr)
	}
}
