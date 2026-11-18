package protocol

import (
	"bytes"
	"strings"
	"testing"
)

// Mixed custom/function Chat SSE must emit tool deltas in source item
// order while preserving incremental text and function-only behavior.
func TestChatCustomOrderMixedTextIncremental(t *testing.T) {
	final := `{"id":"resp_1","object":"response","created_at":1710000000,"model":"up","status":"completed",` +
		`"output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi"}]},` +
		`{"type":"custom_tool_call","id":"ctc_1","call_id":"call_c","name":"patch","input":"xyz","status":"completed"},` +
		`{"type":"function_call","id":"fc_1","call_id":"call_f","name":"lookup","arguments":"{\"q\":\"hi\"}","status":"completed"}],` +
		`"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`
	sse := "data: " + `{"type":"response.created","sequence_number":0,"response":{"id":"resp_1","model":"up","status":"in_progress","output":[]}}` + "\n\n" +
		"data: " + `{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"message","id":"msg_1","status":"in_progress","role":"assistant","content":[]}}` + "\n\n" +
		"data: " + `{"type":"response.content_part.added","sequence_number":2,"output_index":0,"item_id":"msg_1","content_index":0,"part":{"type":"output_text","text":""}}` + "\n\n" +
		"data: " + `{"type":"response.output_item.added","sequence_number":3,"output_index":1,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"call_c","name":"patch","input":"","status":"in_progress"}}` + "\n\n" +
		"data: " + `{"type":"response.output_item.added","sequence_number":4,"output_index":2,"item":{"type":"function_call","id":"fc_1","call_id":"call_f","name":"lookup","arguments":"","status":"in_progress"}}` + "\n\n" +
		"data: " + `{"type":"response.output_text.delta","sequence_number":5,"output_index":0,"item_id":"msg_1","content_index":0,"delta":"hi"}` + "\n\n" +
		"data: " + `{"type":"response.custom_tool_call_input.delta","sequence_number":6,"output_index":1,"item_id":"ctc_1","delta":"xyz"}` + "\n\n" +
		"data: " + `{"type":"response.function_call_arguments.delta","sequence_number":7,"output_index":2,"item_id":"fc_1","delta":"{\"q\":\"hi\"}"}` + "\n\n" +
		"data: " + `{"type":"response.output_text.done","sequence_number":8,"output_index":0,"item_id":"msg_1","content_index":0,"text":"hi"}` + "\n\n" +
		"data: " + `{"type":"response.custom_tool_call_input.done","sequence_number":9,"output_index":1,"item_id":"ctc_1","input":"xyz"}` + "\n\n" +
		"data: " + `{"type":"response.function_call_arguments.done","sequence_number":10,"output_index":2,"item_id":"fc_1","arguments":"{\"q\":\"hi\"}"}` + "\n\n" +
		"data: " + `{"type":"response.content_part.done","sequence_number":11,"output_index":0,"item_id":"msg_1","content_index":0,"part":{"type":"output_text","text":"hi"}}` + "\n\n" +
		"data: " + `{"type":"response.output_item.done","sequence_number":12,"output_index":0,"item":{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hi"}]}}` + "\n\n" +
		"data: " + `{"type":"response.output_item.done","sequence_number":13,"output_index":1,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"call_c","name":"patch","input":"xyz","status":"completed"}}` + "\n\n" +
		"data: " + `{"type":"response.output_item.done","sequence_number":14,"output_index":2,"item":{"type":"function_call","id":"fc_1","call_id":"call_f","name":"lookup","arguments":"{\"q\":\"hi\"}","status":"completed"}}` + "\n\n" +
		"data: " + `{"type":"response.completed","sequence_number":15,"response":` + final + `}` + "\n\n"
	tr := &ResponsesToChatSSETranslator{PublicModel: "public", Limits: DefaultLimits()}
	var out bytes.Buffer
	if rerr := tr.Translate(strings.NewReader(sse), &out, nil); rerr != nil {
		t.Fatalf("stream: %v", rerr)
	}
	s := out.String()
	ti := strings.Index(s, `"content":"hi"`)
	ci := strings.Index(s, "call_c")
	fi := strings.Index(s, "call_f")
	if ti < 0 || ci < 0 || fi < 0 {
		t.Fatalf("missing chunks: %s", s)
	}
	if !(ti < ci && ci < fi) {
		t.Fatalf("order must be text,custom,function got ti=%d ci=%d fi=%d: %s", ti, ci, fi, s)
	}
}

// Function-only generations stay incremental: no custom means no
// buffering, deltas emit in arrival order.
func TestChatCustomOrderFunctionOnlyIncremental(t *testing.T) {
	final := `{"id":"resp_1","object":"response","created_at":1710000000,"model":"up","status":"completed",` +
		`"output":[{"type":"function_call","id":"fc_1","call_id":"call_a","name":"aa","arguments":"{\"x\":1}","status":"completed"},` +
		`{"type":"function_call","id":"fc_2","call_id":"call_b","name":"bb","arguments":"{\"y\":2}","status":"completed"}],` +
		`"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`
	sse := "data: " + `{"type":"response.created","sequence_number":0,"response":{"id":"resp_1","model":"up","status":"in_progress","output":[]}}` + "\n\n" +
		"data: " + `{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_a","name":"aa","arguments":"","status":"in_progress"}}` + "\n\n" +
		"data: " + `{"type":"response.output_item.added","sequence_number":2,"output_index":1,"item":{"type":"function_call","id":"fc_2","call_id":"call_b","name":"bb","arguments":"","status":"in_progress"}}` + "\n\n" +
		"data: " + `{"type":"response.function_call_arguments.delta","sequence_number":3,"output_index":0,"item_id":"fc_1","delta":"{\"x\":1}"}` + "\n\n" +
		"data: " + `{"type":"response.function_call_arguments.delta","sequence_number":4,"output_index":1,"item_id":"fc_2","delta":"{\"y\":2}"}` + "\n\n" +
		"data: " + `{"type":"response.function_call_arguments.done","sequence_number":5,"output_index":0,"item_id":"fc_1","arguments":"{\"x\":1}"}` + "\n\n" +
		"data: " + `{"type":"response.function_call_arguments.done","sequence_number":6,"output_index":1,"item_id":"fc_2","arguments":"{\"y\":2}"}` + "\n\n" +
		"data: " + `{"type":"response.output_item.done","sequence_number":7,"output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_a","name":"aa","arguments":"{\"x\":1}","status":"completed"}}` + "\n\n" +
		"data: " + `{"type":"response.output_item.done","sequence_number":8,"output_index":1,"item":{"type":"function_call","id":"fc_2","call_id":"call_b","name":"bb","arguments":"{\"y\":2}","status":"completed"}}` + "\n\n" +
		"data: " + `{"type":"response.completed","sequence_number":9,"response":` + final + `}` + "\n\n"
	tr := &ResponsesToChatSSETranslator{PublicModel: "public", Limits: DefaultLimits()}
	var out bytes.Buffer
	if rerr := tr.Translate(strings.NewReader(sse), &out, nil); rerr != nil {
		t.Fatalf("stream: %v", rerr)
	}
	s := out.String()
	ai, bi := strings.Index(s, "call_a"), strings.Index(s, "call_b")
	if ai < 0 || bi < 0 || ai > bi {
		t.Fatalf("function order not preserved: %s", s)
	}
	if !strings.Contains(s, "[DONE]") {
		t.Fatalf("valid terminal must end with [DONE]")
	}
}

// Buffered function bytes stay bounded: an oversized delta behind a
// pending custom fails with the size bound, never unbounded retention.
func TestChatCustomOrderBoundedMemory(t *testing.T) {
	lim := DefaultLimits()
	lim.MaxArgsChars = 4
	final := `{"id":"resp_1","object":"response","created_at":1710000000,"model":"up","status":"completed",` +
		`"output":[{"type":"custom_tool_call","id":"ctc_1","call_id":"call_c","name":"patch","input":"xy","status":"completed"},` +
		`{"type":"function_call","id":"fc_1","call_id":"call_f","name":"lookup","arguments":"{\"toolong\":1}","status":"completed"}],` +
		`"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`
	sse := "data: " + `{"type":"response.created","sequence_number":0,"response":{"id":"resp_1","model":"up","status":"in_progress","output":[]}}` + "\n\n" +
		"data: " + `{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"call_c","name":"patch","input":"","status":"in_progress"}}` + "\n\n" +
		"data: " + `{"type":"response.output_item.added","sequence_number":2,"output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_f","name":"lookup","arguments":"","status":"in_progress"}}` + "\n\n" +
		"data: " + `{"type":"response.custom_tool_call_input.delta","sequence_number":3,"output_index":0,"item_id":"ctc_1","delta":"xy"}` + "\n\n" +
		"data: " + `{"type":"response.function_call_arguments.delta","sequence_number":4,"output_index":1,"item_id":"fc_1","delta":"{\"toolong\":1}"}` + "\n\n" +
		"data: " + `{"type":"response.custom_tool_call_input.done","sequence_number":5,"output_index":0,"item_id":"ctc_1","input":"xy"}` + "\n\n" +
		"data: " + `{"type":"response.function_call_arguments.done","sequence_number":6,"output_index":1,"item_id":"fc_1","arguments":"{\"toolong\":1}"}` + "\n\n" +
		"data: " + `{"type":"response.output_item.done","sequence_number":7,"output_index":0,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"call_c","name":"patch","input":"xy","status":"completed"}}` + "\n\n" +
		"data: " + `{"type":"response.output_item.done","sequence_number":8,"output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_f","name":"lookup","arguments":"{\"toolong\":1}","status":"completed"}}` + "\n\n" +
		"data: " + `{"type":"response.completed","sequence_number":9,"response":` + final + `}` + "\n\n"
	tr := &ResponsesToChatSSETranslator{PublicModel: "public", Limits: lim}
	var out bytes.Buffer
	rerr := tr.Translate(strings.NewReader(sse), &out, nil)
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("oversize buffered function must 502, got %v", rerr)
	}
	if strings.Contains(out.String(), "[DONE]") {
		t.Fatalf("failed stream must not emit [DONE]")
	}
}

// Downstream write failures are sticky: no secondary writes, no further
// reads, and no synthetic completion.
type failAfterN struct {
	buf   bytes.Buffer
	limit int
	fails int
}

func (f *failAfterN) Write(p []byte) (int, error) {
	if f.buf.Len()+len(p) > f.limit {
		f.fails++
		return 0, errWriteFail
	}
	return f.buf.Write(p)
}

type writeFailErr struct{}

func (writeFailErr) Error() string { return "downstream write failed" }

var errWriteFail = writeFailErr{}

func TestChatCustomOrderWriteFailureSticky(t *testing.T) {
	final := `{"id":"resp_1","object":"response","created_at":1710000000,"model":"up","status":"completed",` +
		`"output":[{"type":"custom_tool_call","id":"ctc_1","call_id":"call_c","name":"patch","input":"xyz","status":"completed"},` +
		`{"type":"function_call","id":"fc_1","call_id":"call_f","name":"lookup","arguments":"{\"q\":\"hi\"}","status":"completed"}],` +
		`"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`
	sse := "data: " + `{"type":"response.created","sequence_number":0,"response":{"id":"resp_1","model":"up","status":"in_progress","output":[]}}` + "\n\n" +
		"data: " + `{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"call_c","name":"patch","input":"","status":"in_progress"}}` + "\n\n" +
		"data: " + `{"type":"response.output_item.added","sequence_number":2,"output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_f","name":"lookup","arguments":"","status":"in_progress"}}` + "\n\n" +
		"data: " + `{"type":"response.custom_tool_call_input.delta","sequence_number":3,"output_index":0,"item_id":"ctc_1","delta":"xyz"}` + "\n\n" +
		"data: " + `{"type":"response.function_call_arguments.delta","sequence_number":4,"output_index":1,"item_id":"fc_1","delta":"{\"q\":\"hi\"}"}` + "\n\n" +
		"data: " + `{"type":"response.custom_tool_call_input.done","sequence_number":5,"output_index":0,"item_id":"ctc_1","input":"xyz"}` + "\n\n" +
		"data: " + `{"type":"response.function_call_arguments.done","sequence_number":6,"output_index":1,"item_id":"fc_1","arguments":"{\"q\":\"hi\"}"}` + "\n\n" +
		"data: " + `{"type":"response.output_item.done","sequence_number":7,"output_index":0,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"call_c","name":"patch","input":"xyz","status":"completed"}}` + "\n\n" +
		"data: " + `{"type":"response.output_item.done","sequence_number":8,"output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_f","name":"lookup","arguments":"{\"q\":\"hi\"}","status":"completed"}}` + "\n\n" +
		"data: " + `{"type":"response.completed","sequence_number":9,"response":` + final + `}` + "\n\n"
	tr := &ResponsesToChatSSETranslator{PublicModel: "public", Limits: DefaultLimits()}
	w := &failAfterN{limit: 10}
	rerr := tr.Translate(strings.NewReader(sse), w, nil)
	if rerr == nil {
		t.Fatalf("write failure must error")
	}
	if w.fails == 0 {
		t.Fatalf("expected a write failure")
	}
	if strings.Contains(w.buf.String(), "[DONE]") {
		t.Fatalf("write failure must not emit [DONE]")
	}
}

// Truncated mixed streams never fabricate completion.
func TestChatCustomOrderNoSyntheticCompletion(t *testing.T) {
	sse := "data: " + `{"type":"response.created","sequence_number":0,"response":{"id":"resp_1","model":"up","status":"in_progress","output":[]}}` + "\n\n" +
		"data: " + `{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"call_c","name":"patch","input":"","status":"in_progress"}}` + "\n\n" +
		"data: " + `{"type":"response.output_item.added","sequence_number":2,"output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_f","name":"lookup","arguments":"","status":"in_progress"}}` + "\n\n" +
		"data: " + `{"type":"response.function_call_arguments.delta","sequence_number":3,"output_index":1,"item_id":"fc_1","delta":"{\"q\":1}"}` + "\n\n"
	tr := &ResponsesToChatSSETranslator{PublicModel: "public", Limits: DefaultLimits()}
	var out bytes.Buffer
	rerr := tr.Translate(strings.NewReader(sse), &out, nil)
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("truncation must 502, got %v", rerr)
	}
	s := out.String()
	if strings.Contains(s, "[DONE]") || strings.Contains(s, `"finish_reason":"tool_calls"`) {
		t.Fatalf("truncated stream must not fabricate completion: %s", s)
	}
}

// Typed MalformedToolEvent for BlockCustom is lifecycle-valid (not a
// function) and refuses for Chat and Responses.
func TestChatCustomMalformedLifecycleParity(t *testing.T) {
	lim := DefaultLimits()
	lc := NewResponseLifecycle(lim, DialectResponses)
	if rerr := lc.Observe(ResponseStart{ID: "r1", Model: "m", Source: DialectResponses}); rerr != nil {
		t.Fatalf("start: %v", rerr)
	}
	open, rerr := NewBlockOpen("c0", BlockCustom, "call_c", "patch")
	if rerr != nil {
		t.Fatalf("open: %v", rerr)
	}
	if rerr := lc.Observe(open); rerr != nil {
		t.Fatalf("open observe: %v", rerr)
	}
	if rerr := lc.Observe(MalformedToolEvent{Item: "c0"}); rerr != nil {
		t.Fatalf("custom malformed must be lifecycle-valid: %v", rerr)
	}
	res, rerr := func() (LifecycleResult, *RouterError) {
		if rerr := lc.Observe(BlockClose{Item: "c0"}); rerr != nil {
			return LifecycleResult{}, rerr
		}
		u, _ := NewTerminalUsage(DialectResponses, 1, 1, 2)
		if rerr := lc.Observe(UsageEvent{Usage: u}); rerr != nil {
			return LifecycleResult{}, rerr
		}
		fin, _ := NewFinish(FinishStop, "completed")
		comp, _ := NewCompletionEvent(fin)
		if rerr := lc.Observe(comp); rerr != nil {
			return LifecycleResult{}, rerr
		}
		return lc.Result()
	}()
	if rerr != nil {
		t.Fatalf("result: %v", rerr)
	}
	if res.MalformedTools != 1 {
		t.Fatalf("malformed count = %d, want 1", res.MalformedTools)
	}
	resp := Response{ID: "r1", Model: "m", Source: DialectResponses,
		Items: []ResponseItem{}, Usage: res.Usage, Finish: res.Finish, MalformedTools: 1}
	if rerr := CheckRepresentable(resp, DialectChat); rerr == nil {
		t.Fatalf("Chat must refuse custom malformed")
	}
	if rerr := CheckRepresentable(resp, DialectResponses); rerr == nil {
		t.Fatalf("Responses must refuse custom malformed")
	}
}

// Stream path with a bad custom name classifies malformed and refuses
// for Chat without treating it as a function.
func TestChatCustomMalformedStreamRefuses(t *testing.T) {
	final := `{"id":"resp_1","object":"response","created_at":1710000000,"model":"up","status":"completed",` +
		`"output":[{"type":"custom_tool_call","id":"ctc_1","call_id":"call_c","name":"has space","input":"x","status":"completed"}],` +
		`"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`
	_ = final
	sse := "data: " + `{"type":"response.created","sequence_number":0,"response":{"id":"resp_1","model":"up","status":"in_progress","output":[]}}` + "\n\n" +
		"data: " + `{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"call_c","name":"has space","input":"","status":"in_progress"}}` + "\n\n" +
		"data: " + `{"type":"response.custom_tool_call_input.delta","sequence_number":2,"output_index":0,"item_id":"ctc_1","delta":"x"}` + "\n\n" +
		"data: " + `{"type":"response.custom_tool_call_input.done","sequence_number":3,"output_index":0,"item_id":"ctc_1","input":"x"}` + "\n\n" +
		"data: " + `{"type":"response.output_item.done","sequence_number":4,"output_index":0,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"call_c","name":"has space","input":"x","status":"completed"}}` + "\n\n" +
		"data: " + `{"type":"response.completed","sequence_number":5,"response":` + final + `}` + "\n\n"
	tr := &ResponsesToChatSSETranslator{PublicModel: "public", Limits: DefaultLimits()}
	var out bytes.Buffer
	rerr := tr.Translate(strings.NewReader(sse), &out, nil)
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("bad custom name stream must 502 for Chat, got %v out=%s", rerr, out.String())
	}
	if strings.Contains(out.String(), `"type":"function"`) {
		t.Fatalf("custom malformed must not render as function: %s", out.String())
	}
}

// Request plan guards both JSON and stream paths with the same semantic
// boundary; compacted JSON args are never reinterpreted as custom input.
func TestChatCustomPlanGuardBothPaths(t *testing.T) {
	genRaw := `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"custom","custom":{"name":"patch"}}]}`
	gen, _, rerr := DecodeChatForResponses([]byte(genRaw), DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode req: %v", rerr)
	}
	plan := gen.ToolPlan
	// Native custom with declared name passes on the JSON path.
	good := `{"id":"resp_1","object":"response","created_at":1710000000,"model":"up","status":"completed","output":[` +
		`{"type":"custom_tool_call","id":"ctc_1","call_id":"c1","name":"patch","input":"hello","status":"completed"}],` +
		`"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`
	out, rerr := TranslateResponsesJSONToChatWithPlan([]byte(good), "public", DefaultLimits(), plan)
	if rerr != nil {
		t.Fatalf("native custom must pass: %v", rerr)
	}
	if !strings.Contains(string(out), `"type":"custom"`) || !strings.Contains(string(out), "hello") {
		t.Fatalf("native custom not preserved: %s", out)
	}
	// Declared custom returned as function refuses (no facade repair to
	// custom input from compacted JSON args).
	wrongFam := `{"id":"resp_1","object":"response","created_at":1710000000,"model":"up","status":"completed","output":[` +
		`{"type":"function_call","id":"fc_1","call_id":"c1","name":"patch","arguments":"{\"q\":\"hi\"}","status":"completed"}],` +
		`"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`
	if _, rerr := TranslateResponsesJSONToChatWithPlan([]byte(wrongFam), "public", DefaultLimits(), plan); rerr == nil {
		t.Fatalf("function with declared custom name must refuse, not reinterpret args as input")
	}
	// Stream path: same two branches through the shared guard.
	finalGood := good
	sseGood := "data: " + `{"type":"response.created","sequence_number":0,"response":{"id":"resp_1","model":"up","status":"in_progress","output":[]}}` + "\n\n" +
		"data: " + `{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"c1","name":"patch","input":"","status":"in_progress"}}` + "\n\n" +
		"data: " + `{"type":"response.custom_tool_call_input.delta","sequence_number":2,"output_index":0,"item_id":"ctc_1","delta":"hello"}` + "\n\n" +
		"data: " + `{"type":"response.custom_tool_call_input.done","sequence_number":3,"output_index":0,"item_id":"ctc_1","input":"hello"}` + "\n\n" +
		"data: " + `{"type":"response.output_item.done","sequence_number":4,"output_index":0,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"c1","name":"patch","input":"hello","status":"completed"}}` + "\n\n" +
		"data: " + `{"type":"response.completed","sequence_number":5,"response":` + finalGood + `}` + "\n\n"
	tr := &ResponsesToChatSSETranslator{PublicModel: "public", Limits: DefaultLimits(), Plan: plan}
	var buf bytes.Buffer
	if rerr := tr.Translate(strings.NewReader(sseGood), &buf, nil); rerr != nil {
		t.Fatalf("native custom stream must pass: %v", rerr)
	}
	if !strings.Contains(buf.String(), `"type":"custom"`) {
		t.Fatalf("stream native custom not preserved: %s", buf.String())
	}
	finalBad := `{"id":"resp_1","object":"response","created_at":1710000000,"model":"up","status":"completed",` +
		`"output":[{"type":"function_call","id":"fc_1","call_id":"c1","name":"patch","arguments":"{}","status":"completed"}],` +
		`"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`
	sseBad := "data: " + `{"type":"response.created","sequence_number":0,"response":{"id":"resp_1","model":"up","status":"in_progress","output":[]}}` + "\n\n" +
		"data: " + `{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"c1","name":"patch","arguments":"","status":"in_progress"}}` + "\n\n" +
		"data: " + `{"type":"response.function_call_arguments.delta","sequence_number":2,"output_index":0,"item_id":"fc_1","delta":"{}"}` + "\n\n" +
		"data: " + `{"type":"response.function_call_arguments.done","sequence_number":3,"output_index":0,"item_id":"fc_1","arguments":"{}"}` + "\n\n" +
		"data: " + `{"type":"response.output_item.done","sequence_number":4,"output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"c1","name":"patch","arguments":"{}","status":"completed"}}` + "\n\n" +
		"data: " + `{"type":"response.completed","sequence_number":5,"response":` + finalBad + `}` + "\n\n"
	var buf2 bytes.Buffer
	if rerr := tr.Translate(strings.NewReader(sseBad), &buf2, nil); rerr == nil {
		t.Fatalf("stream function with declared custom name must refuse")
	}
	if strings.Contains(buf2.String(), "[DONE]") {
		t.Fatalf("refused stream must not emit [DONE]")
	}
}

func TestParallelCustomCallsReleaseInOpenOrder(t *testing.T) {
	enc := NewChatEventEncoder("public", false, DefaultLimits())
	var out bytes.Buffer
	emit := func(ev ResponseEvent) {
		t.Helper()
		if err := enc.Encode(&out, nil, ev); err != nil {
			t.Fatal(err)
		}
	}
	emit(ResponseStart{ID: "r", Model: "up", Source: DialectResponses})
	first, err := NewBlockOpen("b0", BlockCustom, "first_call", "patch")
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewBlockOpen("b1", BlockCustom, "second_call", "patch")
	if err != nil {
		t.Fatal(err)
	}
	emit(first)
	emit(second)
	emit(BlockClose{Item: "b1"})
	if strings.Contains(out.String(), "second_call") {
		t.Fatal("later custom call was emitted ahead of the earlier pending custom call")
	}
	emit(BlockClose{Item: "b0"})
	wire := out.String()
	a, b := strings.Index(wire, "first_call"), strings.Index(wire, "second_call")
	if a < 0 || b < a {
		t.Fatalf("custom identities must be released in open order: %s", wire)
	}
}
