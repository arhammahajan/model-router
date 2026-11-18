package protocol

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func runR2CStream(t *testing.T, upstream string, includeUsage bool, lim Limits) (string, *RouterError) {
	t.Helper()
	var sb strings.Builder
	tr := &ResponsesToChatSSETranslator{PublicModel: "public", IncludeUsage: includeUsage, Limits: lim}
	rerr := tr.Translate(strings.NewReader(upstream), &sb, nil)
	return sb.String(), rerr
}

func r2cCreated() string {
	return "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"status\":\"in_progress\"}}\n\n"
}

func r2cItemAddedMsg() string {
	return "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"in_progress\"}}\n\n"
}

func r2cPartAdded() string {
	return "event: response.content_part.added\ndata: {\"type\":\"response.content_part.added\",\"output_index\":0,\"content_index\":0,\"item_id\":\"msg_1\",\"part\":{\"type\":\"output_text\"}}\n\n"
}

func r2cTextDelta(s string) string {
	return "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"item_id\":\"msg_1\",\"delta\":" + quoteForR2C(s) + "}\n\n"
}

func quoteForR2C(s string) string {
	q, _ := json.Marshal(s)
	return string(q)
}

func r2cFuncAdded() string {
	return "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":1,\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"in_progress\",\"call_id\":\"call_abc\",\"name\":\"get_time\",\"arguments\":\"\"}}\n\n"
}

func r2cArgsDelta(s string) string {
	return "event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":1,\"item_id\":\"fc_1\",\"delta\":" + quoteForR2C(s) + "}\n\n"
}

func r2cArgsDone(s string) string {
	return "event: response.function_call_arguments.done\ndata: {\"type\":\"response.function_call_arguments.done\",\"output_index\":1,\"item_id\":\"fc_1\",\"arguments\":" + quoteForR2C(s) + "}\n\n"
}

func r2cTerminalCompleted(text, args string) string {
	return "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-4.1\",\"status\":\"completed\"," +
		"\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":" + quoteForR2C(text) + ",\"annotations\":[]}]}," +
		"{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"completed\",\"call_id\":\"call_abc\",\"name\":\"get_time\",\"arguments\":" + quoteForR2C(args) + "}]," +
		"\"usage\":{\"input_tokens\":100,\"input_tokens_details\":{\"cached_tokens\":10},\"output_tokens\":12,\"total_tokens\":112}}}\n\n"
}

// Full-lifecycle done markers required by the official Responses stream
// contract (output_text.done → content_part.done → output_item.done) and
// proven by the passing realistic wire test. Legacy minimal fixtures that
// omitted them contradict the terminal lineage (doneStatus stays empty).
func r2cTextDone(s string) string {
	return "event: response.output_text.done\ndata: {\"type\":\"response.output_text.done\",\"output_index\":0,\"content_index\":0,\"item_id\":\"msg_1\",\"text\":" + quoteForR2C(s) + "}\n\n"
}

func r2cPartDone(s string) string {
	return "event: response.content_part.done\ndata: {\"type\":\"response.content_part.done\",\"output_index\":0,\"content_index\":0,\"item_id\":\"msg_1\",\"part\":{\"type\":\"output_text\",\"text\":" + quoteForR2C(s) + ",\"annotations\":[]}}\n\n"
}

func r2cItemDoneMsg(s string) string {
	return "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":" + quoteForR2C(s) + ",\"annotations\":[]}]}}\n\n"
}

func r2cItemDoneTool(args string) string {
	return "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":1,\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"completed\",\"call_id\":\"call_abc\",\"name\":\"get_time\",\"arguments\":" + quoteForR2C(args) + "}}\n\n"
}

func TestR2CStreamTextAndTool(t *testing.T) {
	upstream := r2cCreated() + r2cItemAddedMsg() + r2cPartAdded() +
		r2cTextDelta("Hello") + r2cTextDone("Hello") + r2cPartDone("Hello") + r2cItemDoneMsg("Hello") +
		r2cFuncAdded() + r2cArgsDelta(`{"tz":"`) + r2cArgsDelta(`UTC"}`) +
		r2cArgsDone(`{"tz":"UTC"}`) + r2cItemDoneTool(`{"tz":"UTC"}`) + r2cTerminalCompleted("Hello", `{"tz":"UTC"}`)
	out, rerr := runR2CStream(t, upstream, true, DefaultLimits())
	if rerr != nil {
		t.Fatalf("stream failed: %v", rerr)
	}
	if !strings.Contains(out, `"content":"Hello"`) {
		t.Fatalf("missing text delta: %s", out)
	}
	if !strings.Contains(out, `"id":"call_abc"`) || !strings.Contains(out, `"name":"get_time"`) {
		t.Fatalf("tool identity must be call_id: %s", out)
	}
	if !strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Fatalf("missing finish: %s", out)
	}
	if !strings.Contains(out, `"prompt_tokens":100`) || !strings.Contains(out, `"cached_tokens":10`) {
		t.Fatalf("usage/cache wrong: %s", out)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Fatalf("missing terminator: %s", out)
	}
	// Terminal repeats whole output: it must validate, not duplicate content.
	if strings.Count(out, "Hello") != 1 {
		t.Fatalf("terminal must not duplicate content: %s", out)
	}
}

func TestR2CStreamFailures(t *testing.T) {
	// Unknown semantic event fails.
	if _, rerr := runR2CStream(t, r2cCreated()+r2cItemAddedMsg()+
		"event: response.reasoning.delta\ndata: {\"type\":\"response.reasoning.delta\"}\n\n", true, DefaultLimits()); rerr == nil {
		t.Fatalf("reasoning event must fail")
	}
	// Contradictory terminal fails, no success marker.
	upstream := r2cCreated() + r2cItemAddedMsg() + r2cPartAdded() + r2cTextDelta("Hello") +
		r2cTextDone("Hello") + r2cPartDone("Hello") + r2cItemDoneMsg("Hello") +
		r2cFuncAdded() + r2cArgsDelta(`{}`) + r2cArgsDone(`{}`) + r2cItemDoneTool(`{}`) +
		r2cTerminalCompleted("DIFFERENT", `{}`)
	out, rerr := runR2CStream(t, upstream, true, DefaultLimits())
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("contradictory terminal must fail, got %v", rerr)
	}
	if strings.Contains(out, "[DONE]") {
		t.Fatalf("failure must not emit success: %s", out)
	}
	// Truncated stream fails.
	if _, rerr := runR2CStream(t, r2cCreated()+r2cItemAddedMsg(), true, DefaultLimits()); rerr == nil {
		t.Fatalf("truncated stream must fail")
	}
	// Duplicate output index fails.
	if _, rerr := runR2CStream(t, r2cCreated()+r2cItemAddedMsg()+r2cItemAddedMsg(), true, DefaultLimits()); rerr == nil {
		t.Fatalf("duplicate output index must fail")
	}
	// Bounds: overlong frame.
	lim := DefaultLimits()
	lim.MaxFrameBytes = 64
	big := r2cCreated() + "event: response.output_text.delta\ndata: " + strings.Repeat("x", 100) + "\n\n"
	if _, rerr := runR2CStream(t, big, true, lim); rerr == nil {
		t.Fatalf("overlong frame must fail")
	}
	// Empty-arg tool still emits identity. Dense indexing: the single
	// tool is output_index 0 (position 0 holds output_index 0 per the
	// terminal lineage contract); the legacy sparse index 1 with a
	// single-item output contradicts dense positions.
	upstream = r2cCreated() +
		"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"in_progress\",\"call_id\":\"call_abc\",\"name\":\"get_time\",\"arguments\":\"\"}}\n\n" +
		"event: response.function_call_arguments.done\ndata: {\"type\":\"response.function_call_arguments.done\",\"output_index\":0,\"item_id\":\"fc_1\",\"arguments\":\"\"}\n\n" +
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"completed\",\"call_id\":\"call_abc\",\"name\":\"get_time\",\"arguments\":\"\"}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-4.1\",\"status\":\"completed\"," +
		"\"output\":[{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"completed\",\"call_id\":\"call_abc\",\"name\":\"get_time\",\"arguments\":\"\"}]," +
		"\"usage\":{\"input_tokens\":5,\"output_tokens\":2,\"total_tokens\":7}}}\n\n"
	out, rerr = runR2CStream(t, upstream, false, DefaultLimits())
	if rerr != nil {
		t.Fatalf("empty-arg tool must succeed: %v", rerr)
	}
	if !strings.Contains(out, `"id":"call_abc"`) {
		t.Fatalf("empty tool identity missing: %s", out)
	}
	if strings.Contains(out, "prompt_tokens") {
		t.Fatalf("usage must be omitted when include_usage=false: %s", out)
	}
}

func TestR2CStreamWriteFailureStops(t *testing.T) {
	upstream := r2cCreated() + r2cItemAddedMsg() + r2cPartAdded() + r2cTextDelta("Hello")
	tr := &ResponsesToChatSSETranslator{PublicModel: "public", Limits: DefaultLimits()}
	w := &r2cFailWriter{failAfter: 1}
	rerr := tr.Translate(strings.NewReader(upstream), w, nil)
	if rerr == nil || !errors.Is(rerr, errR2CFail) && rerr.Msg != "Downstream write failed" {
		t.Fatalf("write failure must stop reads: %v", rerr)
	}
}

// Real-wire regression: response.content_part.added.part carries
// annotations:[] (and documented empty logprobs). Harmless empty arrays must
// be accepted across added/delta/done/terminal; populated semantic content
// must fail closed with no DONE.
func TestR2CStreamRealisticEmptyAnnotations(t *testing.T) {
	upstream :=
		"event: response.created\ndata: {\"type\": \"response.created\", \"response\": {\"id\": \"resp_up\", \"object\": \"response\", \"created_at\": 1, \"status\": \"in_progress\", \"model\": \"gpt-4.1\", \"output\": []}, \"sequence_number\": 0}\n\n" +
			"event: response.in_progress\ndata: {\"type\": \"response.in_progress\", \"response\": {\"id\": \"resp_up\", \"object\": \"response\", \"created_at\": 1, \"status\": \"in_progress\", \"model\": \"gpt-4.1\", \"output\": []}, \"sequence_number\": 1}\n\n" +
			"event: response.output_item.added\ndata: {\"type\": \"response.output_item.added\", \"output_index\": 0, \"item\": {\"id\": \"msg_up\", \"type\": \"message\", \"status\": \"in_progress\"}, \"sequence_number\": 2}\n\n" +
			"event: response.content_part.added\ndata: {\"type\": \"response.content_part.added\", \"item_id\": \"msg_up\", \"output_index\": 0, \"content_index\": 0, \"part\": {\"type\": \"output_text\", \"text\": \"\", \"annotations\": []}, \"sequence_number\": 3}\n\n" +
			"event: response.output_text.delta\ndata: {\"type\": \"response.output_text.delta\", \"item_id\": \"msg_up\", \"output_index\": 0, \"content_index\": 0, \"delta\": \"hel\", \"sequence_number\": 4}\n\n" +
			"event: response.output_text.delta\ndata: {\"type\": \"response.output_text.delta\", \"item_id\": \"msg_up\", \"output_index\": 0, \"content_index\": 0, \"delta\": \"lo\", \"sequence_number\": 5}\n\n" +
			"event: response.output_text.done\ndata: {\"type\": \"response.output_text.done\", \"item_id\": \"msg_up\", \"output_index\": 0, \"content_index\": 0, \"text\": \"hello\", \"sequence_number\": 6}\n\n" +
			"event: response.content_part.done\ndata: {\"type\": \"response.content_part.done\", \"item_id\": \"msg_up\", \"output_index\": 0, \"content_index\": 0, \"part\": {\"type\": \"output_text\", \"text\": \"hello\", \"annotations\": []}, \"sequence_number\": 7}\n\n" +
			"event: response.output_item.done\ndata: {\"type\": \"response.output_item.done\", \"output_index\": 0, \"item\": {\"id\": \"msg_up\", \"type\": \"message\", \"status\": \"completed\", \"role\": \"assistant\", \"content\": [{\"type\": \"output_text\", \"text\": \"hello\", \"annotations\": []}]}, \"sequence_number\": 8}\n\n" +
			"event: response.completed\ndata: {\"type\": \"response.completed\", \"response\": {\"id\": \"resp_up\", \"object\": \"response\", \"created_at\": 1, \"status\": \"completed\", \"model\": \"gpt-4.1\", \"output\": [{\"id\": \"msg_up\", \"type\": \"message\", \"status\": \"completed\", \"role\": \"assistant\", \"content\": [{\"type\": \"output_text\", \"text\": \"hello\", \"annotations\": []}]}], \"usage\": {\"input_tokens\": 7, \"output_tokens\": 3, \"total_tokens\": 10}}, \"sequence_number\": 9}\n\n"
	out, rerr := runR2CStream(t, upstream, true, DefaultLimits())
	if rerr != nil {
		t.Fatalf("realistic empty annotations must succeed, got %v", rerr)
	}
	if !strings.Contains(out, `"content":"hel"`) || !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("missing content/finish: %s", out)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Fatalf("missing terminator: %s", out)
	}
	// Populated annotations must fail closed with no success marker.
	populated := strings.Replace(upstream,
		`"part": {"type": "output_text", "text": "", "annotations": []}`,
		`"part": {"type": "output_text", "text": "", "annotations": [{"type": "url_citation", "url": "https://example.com"}]}`,
		1)
	out2, rerr := runR2CStream(t, populated, true, DefaultLimits())
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("populated annotations must fail with 502, got %v", rerr)
	}
	if strings.Contains(out2, "[DONE]") {
		t.Fatalf("failure must not emit DONE: %s", out2)
	}
	// Populated logprobs in delta must also fail closed.
	withLP := strings.Replace(upstream,
		`"delta": "hel"`,
		`"delta": "hel", "logprobs": [{"token": "hel"}]`,
		1)
	out3, rerr := runR2CStream(t, withLP, true, DefaultLimits())
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("populated logprobs must fail with 502, got %v", rerr)
	}
	if strings.Contains(out3, "[DONE]") {
		t.Fatalf("failure must not emit DONE: %s", out3)
	}
}

func TestR2CStreamReasoningUsage(t *testing.T) {
	upstream := r2cCreated() + r2cItemAddedMsg() + r2cPartAdded() + r2cTextDelta("hi") +
		"event: response.output_text.done\ndata: {\"type\":\"response.output_text.done\",\"output_index\":0,\"content_index\":0,\"item_id\":\"msg_1\",\"text\":\"hi\"}\n\n" +
		r2cPartDone("hi") + r2cItemDoneMsg("hi") +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-4.1\",\"status\":\"completed\"," +
		"\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"hi\",\"annotations\":[]}]}]," +
		"\"usage\":{\"input_tokens\":7,\"input_tokens_details\":{\"cached_tokens\":4},\"output_tokens\":3,\"output_tokens_details\":{\"reasoning_tokens\":2},\"total_tokens\":10}}}\n\n"
	out, rerr := runR2CStream(t, upstream, true, DefaultLimits())
	if rerr != nil {
		t.Fatalf("reasoning accounting must succeed, got %v", rerr)
	}
	if !strings.Contains(out, `"prompt_tokens":7`) || !strings.Contains(out, `"cached_tokens":4`) {
		t.Fatalf("prompt/cache accounting lost: %s", out)
	}
	if !strings.Contains(out, `"reasoning_tokens":2`) {
		t.Fatalf("reasoning accounting must map to Chat completion details: %s", out)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Fatalf("missing terminator: %s", out)
	}
}

type r2cFailWriter struct {
	n         int
	failAfter int
}

var errR2CFail = errors.New("r2c downstream fail")

func (w *r2cFailWriter) Write(p []byte) (int, error) {
	w.n++
	if w.n > w.failAfter {
		return 0, errR2CFail
	}
	return len(p), nil
}
