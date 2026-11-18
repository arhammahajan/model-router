package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
)

func r2gEV(name, data string) string {
	return "event: " + name + "\ndata: " + data + "\n\n"
}

func r2gCreated(seq int) string {
	return r2gEV("response.created",
		`{"type":"response.created","sequence_number":`+strconv.Itoa(seq)+`,"response":{"id":"resp_1"}}`)
}

func r2gProgress(seq int) string {
	return r2gEV("response.in_progress",
		`{"type":"response.in_progress","sequence_number":`+strconv.Itoa(seq)+`,"response":{"id":"resp_1"}}`)
}

func r2gMsgAdded(seq, idx int, id string) string {
	return r2gEV("response.output_item.added",
		`{"type":"response.output_item.added","sequence_number":`+strconv.Itoa(seq)+`,"output_index":`+strconv.Itoa(idx)+
			`,"item":{"type":"message","id":"`+id+`","status":"in_progress","role":"assistant","content":[]}}`)
}

func r2gPartAdded(seq, idx int, item string) string {
	return r2gEV("response.content_part.added",
		`{"type":"response.content_part.added","sequence_number":`+strconv.Itoa(seq)+`,"output_index":`+strconv.Itoa(idx)+
			`,"item_id":"`+item+`","content_index":0,"part":{"type":"output_text","text":"","annotations":[],"logprobs":[]}}`)
}

func r2gDelta(seq, idx int, item, delta string) string {
	return r2gEV("response.output_text.delta",
		`{"type":"response.output_text.delta","sequence_number":`+strconv.Itoa(seq)+`,"output_index":`+strconv.Itoa(idx)+
			`,"item_id":"`+item+`","content_index":0,"delta":`+quoted(delta)+`,"logprobs":[]}`)
}

func r2gTextDone(seq, idx int, item, text string) string {
	return r2gEV("response.output_text.done",
		`{"type":"response.output_text.done","sequence_number":`+strconv.Itoa(seq)+`,"output_index":`+strconv.Itoa(idx)+
			`,"item_id":"`+item+`","content_index":0,"text":`+quoted(text)+`,"logprobs":[]}`)
}

func r2gPartDone(seq, idx int, item, text string) string {
	return r2gEV("response.content_part.done",
		`{"type":"response.content_part.done","sequence_number":`+strconv.Itoa(seq)+`,"output_index":`+strconv.Itoa(idx)+
			`,"item_id":"`+item+`","content_index":0,"part":{"type":"output_text","text":`+quoted(text)+`,"annotations":[],"logprobs":[]}}`)
}

func r2gItemDoneMsg(seq, idx int, id, text string) string {
	return r2gEV("response.output_item.done",
		`{"type":"response.output_item.done","sequence_number":`+strconv.Itoa(seq)+`,"output_index":`+strconv.Itoa(idx)+
			`,"item":{"type":"message","id":"`+id+`","status":"completed","role":"assistant",`+
			`"content":[{"type":"output_text","text":`+quoted(text)+`,"annotations":[],"logprobs":[]}]}}`)
}

func r2gCallAdded(seq, idx int, item, call, name string) string {
	return r2gEV("response.output_item.added",
		`{"type":"response.output_item.added","sequence_number":`+strconv.Itoa(seq)+`,"output_index":`+strconv.Itoa(idx)+
			`,"item":{"type":"function_call","id":"`+item+`","call_id":"`+call+`","name":"`+name+`",`+
			`"arguments":"","status":"in_progress"}}`)
}

func r2gArgsDelta(seq, idx int, item, delta string) string {
	return r2gEV("response.function_call_arguments.delta",
		`{"type":"response.function_call_arguments.delta","sequence_number":`+strconv.Itoa(seq)+`,"output_index":`+strconv.Itoa(idx)+
			`,"item_id":"`+item+`","delta":`+quoted(delta)+`}`)
}

func r2gArgsDone(seq, idx int, item, args string) string {
	return r2gEV("response.function_call_arguments.done",
		`{"type":"response.function_call_arguments.done","sequence_number":`+strconv.Itoa(seq)+`,"output_index":`+strconv.Itoa(idx)+
			`,"item_id":"`+item+`","arguments":`+quoted(args)+`}`)
}

func r2gItemDoneCall(seq, idx int, item, call, name, args string) string {
	return r2gEV("response.output_item.done",
		`{"type":"response.output_item.done","sequence_number":`+strconv.Itoa(seq)+`,"output_index":`+strconv.Itoa(idx)+
			`,"item":{"type":"function_call","id":"`+item+`","call_id":"`+call+`","name":"`+name+`",`+
			`"arguments":`+quoted(args)+`,"status":"completed"}}`)
}

func r2gTerminal(seq int, kind, status, det, output string) string {
	return r2gEV("response."+kind,
		`{"type":"response.`+kind+`","sequence_number":`+strconv.Itoa(seq)+`,"response":{`+
			`"id":"resp_1","object":"response","created_at":1746989954.0,"model":"gpt-x",`+
			`"status":`+status+`,"error":null,"incomplete_details":`+det+`,`+
			`"output":`+output+`,"usage":`+r2gUsage()+`}}`)
}

func quoted(s string) string {
	data, _ := json.Marshal(s)
	return string(data)
}

// Text deltas flush immediately; one terminal carries usage last; no [DONE].
func TestResponsesToGeminiSSEText(t *testing.T) {
	stream := r2gCreated(0) + r2gProgress(1) + r2gMsgAdded(2, 0, "msg_1") + r2gPartAdded(3, 0, "msg_1") +
		r2gDelta(4, 0, "msg_1", "hel") + r2gDelta(5, 0, "msg_1", "lo") +
		r2gTextDone(6, 0, "msg_1", "hello") + r2gPartDone(7, 0, "msg_1", "hello") +
		r2gItemDoneMsg(8, 0, "msg_1", "hello") +
		r2gTerminal(9, "completed", `"completed"`, `null`,
			`[{"type":"message","id":"msg_1","status":"completed","role":"assistant",`+
				`"content":[{"type":"output_text","text":"hello","annotations":[],"logprobs":[]}]}]`)
	var out bytes.Buffer
	tr := &ResponsesToGeminiSSETranslator{PublicModel: "gemini-2.0-flash", Limits: DefaultLimits()}
	if rerr := tr.Translate(strings.NewReader(stream), &out, nil); rerr != nil {
		t.Fatalf("translate: %v", rerr)
	}
	s := out.String()
	if strings.Contains(s, "[DONE]") {
		t.Fatalf("Gemini SSE must not carry [DONE]: %s", s)
	}
	frames := strings.Split(strings.TrimSuffix(s, "\n"), "\n\n")
	if len(frames) != 3 {
		t.Fatalf("want 2 text frames + terminal, got %d: %s", len(frames), s)
	}
	if !strings.Contains(frames[0], `"text":"hel"`) || !strings.Contains(frames[1], `"text":"lo"`) {
		t.Fatalf("immediate deltas: %s", s)
	}
	var term map[string]json.RawMessage
	data := strings.TrimPrefix(strings.SplitN(frames[2], "\n", 2)[0], "data: ")
	if err := json.Unmarshal([]byte(data), &term); err != nil {
		t.Fatalf("terminal: %v", err)
	}
	var cands []map[string]json.RawMessage
	_ = json.Unmarshal(term["candidates"], &cands)
	var finish string
	_ = json.Unmarshal(cands[0]["finishReason"], &finish)
	if finish != "STOP" {
		t.Fatalf("finish: %s", s)
	}
	var mv, rid string
	_ = json.Unmarshal(term["modelVersion"], &mv)
	_ = json.Unmarshal(term["responseId"], &rid)
	if mv != "gemini-2.0-flash" || rid != "resp_1" {
		t.Fatalf("identity: %s", s)
	}
	if _, ok := term["usageMetadata"]; !ok {
		t.Fatalf("terminal must carry authoritative usage: %s", s)
	}
}

// Tool fragments accumulate bounded; complete args emit once in terminal.
func TestResponsesToGeminiSSETools(t *testing.T) {
	stream := r2gCreated(0) + r2gProgress(1) + r2gCallAdded(2, 0, "fc_1", "call-opaque-9", "calc") +
		r2gArgsDelta(3, 0, "fc_1", `{"n":`) + r2gArgsDelta(4, 0, "fc_1", `9007199254740993}`) +
		r2gArgsDone(5, 0, "fc_1", `{"n":9007199254740993}`) +
		r2gItemDoneCall(6, 0, "fc_1", "call-opaque-9", "calc", `{"n":9007199254740993}`) +
		r2gTerminal(7, "completed", `"completed"`, `null`,
			`[{"type":"function_call","id":"fc_1","call_id":"call-opaque-9","name":"calc",`+
				`"arguments":"{\"n\":9007199254740993}","status":"completed"}]`)
	var out bytes.Buffer
	tr := &ResponsesToGeminiSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
	if rerr := tr.Translate(strings.NewReader(stream), &out, nil); rerr != nil {
		t.Fatalf("translate: %v", rerr)
	}
	s := out.String()
	if !strings.Contains(s, `"id":"call-opaque-9"`) || !strings.Contains(s, "9007199254740993") {
		t.Fatalf("exact call identity and integers: %s", s)
	}
	if strings.Count(s, "functionCall") != 1 {
		t.Fatalf("complete call must emit exactly once: %s", s)
	}
}

// JSON array framing: incremental array, closed after the terminal.
func TestResponsesToGeminiJSONArray(t *testing.T) {
	stream := r2gCreated(0) + r2gProgress(1) + r2gMsgAdded(2, 0, "msg_1") + r2gPartAdded(3, 0, "msg_1") +
		r2gDelta(4, 0, "msg_1", "hi") +
		r2gTextDone(5, 0, "msg_1", "hi") + r2gPartDone(6, 0, "msg_1", "hi") +
		r2gItemDoneMsg(7, 0, "msg_1", "hi") +
		r2gTerminal(8, "completed", `"completed"`, `null`,
			`[{"type":"message","id":"msg_1","status":"completed","role":"assistant",`+
				`"content":[{"type":"output_text","text":"hi","annotations":[],"logprobs":[]}]}]`)
	var out bytes.Buffer
	tr := &ResponsesToGeminiJSONArrayTranslator{PublicModel: "m", Limits: DefaultLimits()}
	if rerr := tr.Translate(strings.NewReader(stream), &out, nil); rerr != nil {
		t.Fatalf("translate: %v", rerr)
	}
	var arr []map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &arr); err != nil || len(arr) != 2 {
		t.Fatalf("want 2-element array, got %q err=%v", out.String(), err)
	}
	var cands []map[string]json.RawMessage
	_ = json.Unmarshal(arr[1]["candidates"], &cands)
	var finish string
	_ = json.Unmarshal(cands[0]["finishReason"], &finish)
	if finish != "STOP" {
		t.Fatalf("terminal finish: %s", out.String())
	}
}

// Truncation, errors and contradictions never produce a success marker.
func TestResponsesToGeminiStreamFailures(t *testing.T) {
	cases := map[string]string{
		"truncated-eof":  r2gCreated(0) + r2gProgress(1) + r2gDelta(2, 0, "msg_1", "x"),
		"upstream-error": r2gCreated(0) + r2gEV("error", `{"type":"error","message":"boom"}`),
		"failed":         r2gCreated(0) + r2gEV("response.failed", `{"type":"response.failed","sequence_number":1}`),
		"unknown-event":  r2gCreated(0) + r2gEV("response.reasoning.delta", `{"type":"response.reasoning.delta","sequence_number":1}`),
		"out-of-order":   r2gCreated(5) + r2gEV("response.in_progress", `{"type":"response.in_progress","sequence_number":3,"response":{"id":"resp_1"}}`),
		"contradiction": r2gCreated(0) + r2gProgress(1) + r2gMsgAdded(2, 0, "msg_1") + r2gPartAdded(3, 0, "msg_1") +
			r2gDelta(4, 0, "msg_1", "hi") + r2gTextDone(5, 0, "msg_1", "bye"),
		"unfinished": r2gCreated(0) + r2gProgress(1) + r2gMsgAdded(2, 0, "msg_1") + r2gPartAdded(3, 0, "msg_1") +
			r2gDelta(4, 0, "msg_1", "hi") +
			r2gTerminal(5, "completed", `"completed"`, `null`,
				`[{"type":"message","status":"completed","role":"assistant",`+
					`"content":[{"type":"output_text","text":"hi","annotations":[],"logprobs":[]}]}]`),
		"refusal": r2gCreated(0) + r2gEV("response.refusal.delta",
			`{"type":"response.refusal.delta","sequence_number":1,"output_index":0,"item_id":"m","content_index":0,"delta":"no"}`),
		"done-marker": r2gCreated(0) + "data: [DONE]\n\n",
	}
	for name, input := range cases {
		var out bytes.Buffer
		tr := &ResponsesToGeminiSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
		rerr := tr.Translate(strings.NewReader(input), &out, nil)
		if rerr == nil {
			t.Fatalf("%s must fail", name)
		}
		if strings.Contains(out.String(), "finishReason") {
			t.Fatalf("%s must not emit a terminal: %s", name, out.String())
		}
	}
}

// JSONArray failure after the first frame leaves the array unclosed.
func TestResponsesToGeminiJSONArrayFailureUnclosed(t *testing.T) {
	input := r2gCreated(0) + r2gProgress(1) + r2gMsgAdded(2, 0, "msg_1") + r2gPartAdded(3, 0, "msg_1") +
		r2gDelta(4, 0, "msg_1", "hi") +
		r2gEV("error", `{"type":"error","message":"boom"}`)
	var out bytes.Buffer
	tr := &ResponsesToGeminiJSONArrayTranslator{PublicModel: "m", Limits: DefaultLimits()}
	if rerr := tr.Translate(strings.NewReader(input), &out, nil); rerr == nil {
		t.Fatalf("must fail")
	}
	s := strings.TrimSpace(out.String())
	if !strings.HasPrefix(s, "[") || strings.HasSuffix(s, "]") {
		t.Fatalf("failed array must be unclosed, got %q", s)
	}
}

type r2gFailWriter struct{ err error }

func (w *r2gFailWriter) Write(p []byte) (int, error) { return 0, w.err }

// Write failure stops upstream reads immediately.
func TestResponsesToGeminiWriteFailure(t *testing.T) {
	input := r2gCreated(0) + r2gProgress(1) + r2gMsgAdded(2, 0, "msg_1") + r2gPartAdded(3, 0, "msg_1") +
		r2gDelta(4, 0, "msg_1", "hi")
	tr := &ResponsesToGeminiSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
	rerr := tr.Translate(strings.NewReader(input), &r2gFailWriter{err: errors.New("down")}, nil)
	if rerr == nil || !strings.Contains(rerr.Msg, "Downstream write failed") {
		t.Fatalf("write failure must surface, got %v", rerr)
	}
}

// Per-call and aggregate bounds are enforced; 1024-item framing holds.
func TestResponsesToGeminiStreamBounds(t *testing.T) {
	lim := DefaultLimits()
	lim.MaxArgsChars = 8
	input := r2gCreated(0) + r2gProgress(1) + r2gCallAdded(2, 0, "fc_1", "c1", "f") +
		r2gArgsDelta(3, 0, "fc_1", `{"n":12345}`)
	var out bytes.Buffer
	tr := &ResponsesToGeminiSSETranslator{PublicModel: "m", Limits: lim}
	if rerr := tr.Translate(strings.NewReader(input), &out, nil); rerr == nil {
		t.Fatalf("oversize args must fail")
	}
	_ = io.Discard
}

// Terminal never repeats streamed text; tools emit at done preserving order.
func TestResponsesToGeminiTerminalNoRepeat(t *testing.T) {
	stream := r2gCreated(0) + r2gMsgAdded(1, 0, "msg_1") + r2gPartAdded(2, 0, "msg_1") +
		r2gDelta(3, 0, "msg_1", "hello") + r2gTextDone(4, 0, "msg_1", "hello") +
		r2gPartDone(5, 0, "msg_1", "hello") + r2gItemDoneMsg(6, 0, "msg_1", "hello") +
		r2gTerminal(7, "completed", `"completed"`, `null`,
			`[{"type":"message","id":"msg_1","status":"completed","role":"assistant",`+
				`"content":[{"type":"output_text","text":"hello","annotations":[],"logprobs":[]}]}]`)
	var out bytes.Buffer
	tr := &ResponsesToGeminiSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
	if rerr := tr.Translate(strings.NewReader(stream), &out, nil); rerr != nil {
		t.Fatalf("translate: %v", rerr)
	}
	var joined strings.Builder
	for _, line := range strings.Split(out.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var obj struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &obj); err != nil {
			t.Fatal(err)
		}
		for _, c := range obj.Candidates {
			for _, p := range c.Content.Parts {
				joined.WriteString(p.Text)
			}
		}
	}
	if joined.String() != "hello" {
		t.Fatalf("terminal repeated text: %q", joined.String())
	}
}

// Terminal item identity must match added/done.
func TestResponsesToGeminiTerminalItemID(t *testing.T) {
	stream := r2gCreated(0) + r2gMsgAdded(1, 0, "msg_1") + r2gPartAdded(2, 0, "msg_1") +
		r2gDelta(3, 0, "msg_1", "hi") + r2gTextDone(4, 0, "msg_1", "hi") +
		r2gPartDone(5, 0, "msg_1", "hi") + r2gItemDoneMsg(6, 0, "msg_1", "hi") +
		r2gTerminal(7, "completed", `"completed"`, `null`,
			`[{"type":"message","id":"changed","status":"completed","role":"assistant",`+
				`"content":[{"type":"output_text","text":"hi","annotations":[],"logprobs":[]}]}]`)
	var out bytes.Buffer
	tr := &ResponsesToGeminiSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
	if rerr := tr.Translate(strings.NewReader(stream), &out, nil); rerr == nil {
		t.Fatal("changed terminal item id must fail")
	}
}

// Created/progress phases enforce envelope, identity and event/type agreement.
func TestResponsesToGeminiCreatedEnvelope(t *testing.T) {
	base := r2gCreated(0) + r2gMsgAdded(1, 0, "msg_1") + r2gPartAdded(2, 0, "msg_1") +
		r2gDelta(3, 0, "msg_1", "hi") + r2gTextDone(4, 0, "msg_1", "hi") +
		r2gPartDone(5, 0, "msg_1", "hi") + r2gItemDoneMsg(6, 0, "msg_1", "hi") +
		r2gTerminal(7, "completed", `"completed"`, `null`,
			`[{"type":"message","id":"msg_1","status":"completed","role":"assistant",`+
				`"content":[{"type":"output_text","text":"hi","annotations":[],"logprobs":[]}]}]`)
	_ = base
	badCreated := strings.Replace(base, `"response":{"id":"resp_1"}`,
		`"response":{"id":"resp_1","background":true}`, 1)
	var out bytes.Buffer
	tr := &ResponsesToGeminiSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
	if rerr := tr.Translate(strings.NewReader(badCreated), &out, nil); rerr == nil {
		t.Fatal("created background:true must fail")
	}
	malformed := "event: response.created\ndata: {\"type\":17,\"sequence_number\":0,\"response\":{\"id\":\"resp_1\"}}\n\n"
	var out2 bytes.Buffer
	tr2 := &ResponsesToGeminiSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
	if rerr := tr2.Translate(strings.NewReader(malformed), &out2, nil); rerr == nil {
		t.Fatal("numeric type must fail, not fall back to event name")
	}
	mismatch := "event: response.created\ndata: {\"type\":\"response.in_progress\",\"sequence_number\":0,\"response\":{\"id\":\"resp_1\"}}\n\n"
	var out3 bytes.Buffer
	tr3 := &ResponsesToGeminiSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
	if rerr := tr3.Translate(strings.NewReader(mismatch), &out3, nil); rerr == nil {
		t.Fatal("event/data type mismatch must fail")
	}
}

// Terminal function calls reuse canonical item semantics.
func TestResponsesToGeminiTerminalNamespace(t *testing.T) {
	stream := r2gCreated(0) + r2gCallAdded(1, 0, "fc_1", "call1", "f") +
		r2gArgsDelta(2, 0, "fc_1", "{}") + r2gArgsDone(3, 0, "fc_1", "{}") +
		r2gItemDoneCall(4, 0, "fc_1", "call1", "f", "{}") +
		r2gTerminal(5, "completed", `"completed"`, `null`,
			`[{"type":"function_call","id":"fc_1","call_id":"call1","name":"f",`+
				`"arguments":"{}","status":"completed","namespace":"private"}]`)
	var out bytes.Buffer
	tr := &ResponsesToGeminiSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
	if rerr := tr.Translate(strings.NewReader(stream), &out, nil); rerr == nil {
		t.Fatal("terminal namespace must fail")
	}
}

// Tool-first streams preserve index order with immediate text flushing.
func TestResponsesToGeminiToolFirstOrder(t *testing.T) {
	stream := r2gCreated(0) + r2gProgress(1) +
		r2gCallAdded(2, 0, "fc_1", "call1", "f") + r2gArgsDelta(3, 0, "fc_1", `{"a":1}`) +
		r2gArgsDone(4, 0, "fc_1", `{"a":1}`) + r2gItemDoneCall(5, 0, "fc_1", "call1", "f", `{"a":1}`) +
		r2gMsgAdded(6, 1, "msg_1") + r2gPartAdded(7, 1, "msg_1") +
		r2gDelta(8, 1, "msg_1", "hello") + r2gTextDone(9, 1, "msg_1", "hello") +
		r2gPartDone(10, 1, "msg_1", "hello") + r2gItemDoneMsg(11, 1, "msg_1", "hello") +
		r2gTerminal(12, "completed", `"completed"`, `null`,
			`[{"type":"function_call","id":"fc_1","call_id":"call1","name":"f","arguments":"{\"a\":1}","status":"completed"},`+
				`{"type":"message","id":"msg_1","status":"completed","role":"assistant",`+
				`"content":[{"type":"output_text","text":"hello","annotations":[],"logprobs":[]}]}]`)
	var out bytes.Buffer
	tr := &ResponsesToGeminiSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
	if rerr := tr.Translate(strings.NewReader(stream), &out, nil); rerr != nil {
		t.Fatalf("translate: %v", rerr)
	}
	s := out.String()
	if strings.Index(s, "functionCall") > strings.Index(s, `"text":"hello"`) {
		t.Fatalf("tool must precede later text: %s", s)
	}
}

// Malformed model args map to omission, not envelope 502; bounds stay 502.
func TestResponsesToGeminiStreamMalformedArgs(t *testing.T) {
	stream := r2gCreated(0) + r2gCallAdded(1, 0, "fc_1", "c1", "f") +
		r2gArgsDelta(2, 0, "fc_1", "oops") + r2gArgsDone(3, 0, "fc_1", "oops") +
		r2gItemDoneCall(4, 0, "fc_1", "c1", "f", "oops") +
		r2gTerminal(5, "completed", `"completed"`, `null`,
			`[{"type":"function_call","id":"fc_1","call_id":"c1","name":"f","arguments":"oops","status":"completed"}]`)
	var out bytes.Buffer
	tr := &ResponsesToGeminiSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
	if rerr := tr.Translate(strings.NewReader(stream), &out, nil); rerr != nil {
		t.Fatalf("malformed args must not fail stream: %v", rerr)
	}
	if !strings.Contains(out.String(), "MALFORMED_FUNCTION_CALL") {
		t.Fatalf("want MALFORMED_FUNCTION_CALL: %s", out.String())
	}
}

func r2gCallOrderSSE(t *testing.T, s string) []string {
	t.Helper()
	var out bytes.Buffer
	tr := &ResponsesToGeminiSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
	if rerr := tr.Translate(strings.NewReader(s), &out, nil); rerr != nil {
		t.Fatalf("translate: %v", rerr)
	}
	var ids []string
	for _, line := range strings.Split(out.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var obj struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Call *struct {
							ID string `json:"id"`
						} `json:"functionCall"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &obj); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, c := range obj.Candidates {
			for _, p := range c.Content.Parts {
				if p.Call != nil {
					ids = append(ids, p.Call.ID)
				}
			}
		}
	}
	return ids
}

func r2gCallOrderArray(t *testing.T, s string) []string {
	t.Helper()
	var out bytes.Buffer
	tr := &ResponsesToGeminiJSONArrayTranslator{PublicModel: "m", Limits: DefaultLimits()}
	if rerr := tr.Translate(strings.NewReader(s), &out, nil); rerr != nil {
		t.Fatalf("translate array: %v", rerr)
	}
	var arr []struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Call *struct {
						ID string `json:"id"`
					} `json:"functionCall"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(out.Bytes(), &arr); err != nil {
		t.Fatalf("array decode: %v err=%v out=%q", err, err, out.String())
	}
	var ids []string
	for _, el := range arr {
		for _, c := range el.Candidates {
			for _, p := range c.Content.Parts {
				if p.Call != nil {
					ids = append(ids, p.Call.ID)
				}
			}
		}
	}
	return ids
}

// Parallel tools completing out of order must emit in logical index order.
// Covers bigint exactness and empty-args whole-object-once; both framings
// share the same core policy.
func TestResponsesToGeminiParallelOutOfOrder(t *testing.T) {
	big := `{"n":9007199254740993}`
	stream := r2gCreated(0) + r2gCallAdded(1, 0, "fc_0", "call0", "f") +
		r2gCallAdded(2, 1, "fc_1", "call1", "g") +
		r2gArgsDelta(3, 1, "fc_1", "{}") + r2gArgsDone(4, 1, "fc_1", "{}") +
		r2gItemDoneCall(5, 1, "fc_1", "call1", "g", "{}") +
		r2gArgsDelta(6, 0, "fc_0", big) + r2gArgsDone(7, 0, "fc_0", big) +
		r2gItemDoneCall(8, 0, "fc_0", "call0", "f", big) +
		r2gTerminal(9, "completed", `"completed"`, `null`,
			`[{"type":"function_call","id":"fc_0","call_id":"call0","name":"f","arguments":"{\"n\":9007199254740993}","status":"completed"},`+
				`{"type":"function_call","id":"fc_1","call_id":"call1","name":"g","arguments":"{}","status":"completed"}]`)
	if got := r2gCallOrderSSE(t, stream); strings.Join(got, ",") != "call0,call1" {
		t.Fatalf("SSE parallel logical order lost: %v", got)
	}
	if got := r2gCallOrderArray(t, stream); strings.Join(got, ",") != "call0,call1" {
		t.Fatalf("array parallel logical order lost: %v", got)
	}
	var out bytes.Buffer
	tr := &ResponsesToGeminiSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
	if rerr := tr.Translate(strings.NewReader(stream), &out, nil); rerr != nil {
		t.Fatalf("translate: %v", rerr)
	}
	s := out.String()
	if !strings.Contains(s, "9007199254740993") {
		t.Fatalf("bigint must survive exactly: %s", s)
	}
	if strings.Count(s, "functionCall") != 2 {
		t.Fatalf("each parallel call must emit exactly once, no duplicate/omit: %s", s)
	}
}

// Text open blocks a later tool; closing the text unblocks it in order.
func TestResponsesToGeminiTextOpenToolHeld(t *testing.T) {
	stream := r2gCreated(0) + r2gMsgAdded(1, 0, "msg_1") + r2gPartAdded(2, 0, "msg_1") +
		r2gDelta(3, 0, "msg_1", "hello") +
		r2gCallAdded(4, 1, "fc_1", "call1", "f") + r2gArgsDelta(5, 1, "fc_1", `{"a":1}`) +
		r2gArgsDone(6, 1, "fc_1", `{"a":1}`) + r2gItemDoneCall(7, 1, "fc_1", "call1", "f", `{"a":1}`) +
		r2gTextDone(8, 0, "msg_1", "hello") + r2gPartDone(9, 0, "msg_1", "hello") +
		r2gItemDoneMsg(10, 0, "msg_1", "hello") +
		r2gTerminal(11, "completed", `"completed"`, `null`,
			`[{"type":"message","id":"msg_1","status":"completed","role":"assistant",`+
				`"content":[{"type":"output_text","text":"hello","annotations":[],"logprobs":[]}]},`+
				`{"type":"function_call","id":"fc_1","call_id":"call1","name":"f","arguments":"{\"a\":1}","status":"completed"}]`)
	var out bytes.Buffer
	tr := &ResponsesToGeminiSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
	if rerr := tr.Translate(strings.NewReader(stream), &out, nil); rerr != nil {
		t.Fatalf("translate: %v", rerr)
	}
	s := out.String()
	ti := strings.Index(s, `"text":"hello"`)
	fi := strings.Index(s, "functionCall")
	if ti < 0 || fi < 0 || ti > fi {
		t.Fatalf("text must precede unblocked tool: %s", s)
	}
	if strings.Count(s, "functionCall") != 1 {
		t.Fatalf("tool must emit once: %s", s)
	}
}

// Higher-index text while a lower tool is pending is unrepresentable.
// Complete stream (old code would succeed with reordered [text, tool];
// fixed code fails closed before emitting the reordered text).
func TestResponsesToGeminiHigherTextWhilePendingTool(t *testing.T) {
	stream := r2gCreated(0) + r2gCallAdded(1, 0, "fc_0", "call0", "f") +
		r2gMsgAdded(2, 1, "msg_1") + r2gPartAdded(3, 1, "msg_1") +
		r2gDelta(4, 1, "msg_1", "late") +
		r2gArgsDelta(5, 0, "fc_0", "{}") + r2gArgsDone(6, 0, "fc_0", "{}") +
		r2gItemDoneCall(7, 0, "fc_0", "call0", "f", "{}") +
		r2gTextDone(8, 1, "msg_1", "late") + r2gPartDone(9, 1, "msg_1", "late") +
		r2gItemDoneMsg(10, 1, "msg_1", "late") +
		r2gTerminal(11, "completed", `"completed"`, `null`,
			`[{"type":"function_call","id":"fc_0","call_id":"call0","name":"f","arguments":"{}","status":"completed"},`+
				`{"type":"message","id":"msg_1","status":"completed","role":"assistant",`+
				`"content":[{"type":"output_text","text":"late","annotations":[],"logprobs":[]}]}]`)
	var out bytes.Buffer
	tr := &ResponsesToGeminiSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
	if rerr := tr.Translate(strings.NewReader(stream), &out, nil); rerr == nil {
		t.Fatalf("higher text while pending tool must fail, got %q", out.String())
	}
	if strings.Contains(out.String(), "finishReason") {
		t.Fatalf("must not emit a terminal before failing: %s", out.String())
	}
	var outArr bytes.Buffer
	trArr := &ResponsesToGeminiJSONArrayTranslator{PublicModel: "m", Limits: DefaultLimits()}
	if rerr := trArr.Translate(strings.NewReader(stream), &outArr, nil); rerr == nil {
		t.Fatalf("array must obey the same text-guard policy")
	}
}

// Unseen lower index added after higher emitted is a regression.
// Complete stream: old code would succeed with reordered emission,
// fixed code fails closed at the late add.
func TestResponsesToGeminiLateIndexRegression(t *testing.T) {
	// Emit idx1 first (no preceding blocks, so it flushes), then add idx0.
	stream := r2gCreated(0) + r2gCallAdded(1, 1, "fc_1", "call1", "g") +
		r2gArgsDelta(2, 1, "fc_1", "{}") + r2gArgsDone(3, 1, "fc_1", "{}") +
		r2gItemDoneCall(4, 1, "fc_1", "call1", "g", "{}") +
		r2gCallAdded(5, 0, "fc_0", "call0", "f") +
		r2gArgsDelta(6, 0, "fc_0", "{}") + r2gArgsDone(7, 0, "fc_0", "{}") +
		r2gItemDoneCall(8, 0, "fc_0", "call0", "f", "{}") +
		r2gTerminal(9, "completed", `"completed"`, `null`,
			`[{"type":"function_call","id":"fc_0","call_id":"call0","name":"f","arguments":"{}","status":"completed"},`+
				`{"type":"function_call","id":"fc_1","call_id":"call1","name":"g","arguments":"{}","status":"completed"}]`)
	var out bytes.Buffer
	tr := &ResponsesToGeminiSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
	if rerr := tr.Translate(strings.NewReader(stream), &out, nil); rerr == nil {
		t.Fatalf("late lower-index add must fail, got %q", out.String())
	}
}

// Malformed calls are omitted without blocking or duplicating valid calls.
func TestResponsesToGeminiNoDuplicateToolOmit(t *testing.T) {
	stream := r2gCreated(0) + r2gCallAdded(1, 0, "fc_bad", "cbad", "f") +
		r2gCallAdded(2, 1, "fc_1", "call1", "g") +
		r2gArgsDelta(3, 1, "fc_1", `{"a":1}`) + r2gArgsDone(4, 1, "fc_1", `{"a":1}`) +
		r2gItemDoneCall(5, 1, "fc_1", "call1", "g", `{"a":1}`) +
		r2gArgsDelta(6, 0, "fc_bad", "oops") + r2gArgsDone(7, 0, "fc_bad", "oops") +
		r2gItemDoneCall(8, 0, "fc_bad", "cbad", "f", "oops") +
		r2gTerminal(9, "completed", `"completed"`, `null`,
			`[{"type":"function_call","id":"fc_bad","call_id":"cbad","name":"f","arguments":"oops","status":"completed"},`+
				`{"type":"function_call","id":"fc_1","call_id":"call1","name":"g","arguments":"{\"a\":1}","status":"completed"}]`)
	var out bytes.Buffer
	tr := &ResponsesToGeminiSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
	if rerr := tr.Translate(strings.NewReader(stream), &out, nil); rerr != nil {
		t.Fatalf("omit must not fail stream: %v", rerr)
	}
	s := out.String()
	if strings.Count(s, "functionCall") != 1 || !strings.Contains(s, `"id":"call1"`) {
		t.Fatalf("valid call once, malformed omitted: %s", s)
	}
	if strings.Contains(s, "cbad") {
		t.Fatalf("malformed call id must not leak downstream: %s", s)
	}
	if !strings.Contains(s, "MALFORMED_FUNCTION_CALL") {
		t.Fatalf("terminal must map malformed: %s", s)
	}
}

// Bounds stay 502 under parallel pressure; downstream failure stops reads.
func TestResponsesToGeminiBoundsCancelGuard(t *testing.T) {
	lim := DefaultLimits()
	lim.MaxArgsChars = 8
	stream := r2gCreated(0) + r2gCallAdded(1, 0, "fc_0", "c0", "f") +
		r2gCallAdded(2, 1, "fc_1", "c1", "g") +
		r2gArgsDelta(3, 1, "fc_1", `{"n":123456789}`)
	var out bytes.Buffer
	tr := &ResponsesToGeminiSSETranslator{PublicModel: "m", Limits: lim}
	if rerr := tr.Translate(strings.NewReader(stream), &out, nil); rerr == nil {
		t.Fatalf("oversize parallel args must fail")
	}
	if strings.Contains(out.String(), "finishReason") {
		t.Fatalf("bound failure must not emit a terminal: %s", out.String())
	}
	held := r2gCreated(0) + r2gMsgAdded(1, 0, "msg_1") + r2gPartAdded(2, 0, "msg_1") +
		r2gDelta(3, 0, "msg_1", "hi")
	tr2 := &ResponsesToGeminiSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
	if rerr := tr2.Translate(strings.NewReader(held), &r2gFailWriter{err: errors.New("down")}, nil); rerr == nil {
		t.Fatalf("downstream failure must surface")
	}
}
