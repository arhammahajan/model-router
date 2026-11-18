package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

type responsesStreamEvent struct {
	Type string `json:"type"`
	Seq  int    `json:"sequence_number"`
}

func runResponsesStream(t *testing.T, chunks []string) ([]map[string]any, *RouterError) {
	t.Helper()
	var in bytes.Buffer
	for _, c := range chunks {
		in.WriteString("data: " + c + "\n\n")
	}
	var out bytes.Buffer
	tr := &ChatToResponsesSSETranslator{PublicModel: "public-model", Limits: DefaultLimits()}
	rerr := tr.Translate(&in, &out, nil)
	events := parseResponsesEvents(t, out.String())
	return events, rerr
}

func parseResponsesEvents(t *testing.T, s string) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, frame := range strings.Split(s, "\n\n") {
		frame = strings.TrimSpace(frame)
		if frame == "" {
			continue
		}
		lines := strings.Split(frame, "\n")
		if len(lines) != 2 || !strings.HasPrefix(lines[0], "event: ") ||
			!strings.HasPrefix(lines[1], "data: ") {
			t.Fatalf("bad downstream frame: %q", frame)
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "data: ")), &ev); err != nil {
			t.Fatal(err)
		}
		if ev["type"] != strings.TrimPrefix(lines[0], "event: ") {
			t.Fatalf("event/data type mismatch: %q", frame)
		}
		events = append(events, ev)
	}
	return events
}

func respChatChunk(delta, finish string) string {
	fr := "null"
	if finish != "" {
		fr = `"` + finish + `"`
	}
	return `{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"u","choices":[{"index":0,"delta":` + delta + `,"finish_reason":` + fr + `}]}`
}

func respChatUsageChunk() string {
	return `{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"u","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":5,"total_tokens":17,"prompt_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":2}}}`
}

func TestResponsesStreamTextLifecycle(t *testing.T) {
	origNow := responsesNowUnix
	responsesNowUnix = func() int64 { return 1700000000 }
	defer func() { responsesNowUnix = origNow }()
	events, rerr := runResponsesStream(t, []string{
		respChatChunk(`{"role":"assistant","content":"Hello"}`, ""),
		respChatChunk(`{"content":" world"}`, ""),
		respChatChunk(`{}`, "stop"),
		respChatUsageChunk(),
		"[DONE]",
	})
	if rerr != nil {
		t.Fatalf("translate failed: %v", rerr)
	}
	types := []string{}
	for _, ev := range events {
		types = append(types, ev["type"].(string))
	}
	want := []string{
		"response.created", "response.in_progress",
		"response.output_item.added", "response.content_part.added",
		"response.output_text.delta", "response.output_text.delta",
		"response.output_text.done", "response.content_part.done",
		"response.output_item.done", "response.completed",
	}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("bad lifecycle:\n got %v\nwant %v", types, want)
	}
	// Sequence numbers must be monotonic from zero.
	for i, ev := range events {
		if int(ev["sequence_number"].(float64)) != i {
			t.Fatalf("sequence not monotonic at %d: %v", i, ev)
		}
	}
	created := events[0]["response"].(map[string]any)
	if created["status"] != "in_progress" || created["usage"] != nil {
		t.Fatalf("created must be in_progress with null usage: %v", created)
	}
	if created["model"] != "public-model" {
		t.Fatalf("public model not rendered: %v", created)
	}
	completed := events[len(events)-1]["response"].(map[string]any)
	if completed["status"] != "completed" {
		t.Fatalf("bad terminal: %v", completed)
	}
	usage := completed["usage"].(map[string]any)
	if usage["input_tokens"] != float64(12) || usage["total_tokens"] != float64(17) {
		t.Fatalf("authoritative usage missing: %v", usage)
	}
	output := completed["output"].([]any)
	msg := output[0].(map[string]any)
	text := msg["content"].([]any)[0].(map[string]any)["text"]
	if text != "Hello world" {
		t.Fatalf("terminal must reconstruct full text without rebuffering tricks: %v", text)
	}
	// Item identity must be stable across every event.
	msgID := msg["id"].(string)
	for _, ev := range events[2 : len(events)-1] {
		for _, k := range []string{"item_id", "item"} {
			if v, ok := ev[k]; ok {
				switch v := v.(type) {
				case string:
					if v != msgID {
						t.Fatalf("unstable item id: %v vs %v", v, msgID)
					}
				case map[string]any:
					if v["id"] != msgID {
						t.Fatalf("unstable item id: %v vs %v", v["id"], msgID)
					}
				}
			}
		}
	}
}

func TestResponsesStreamToolsFragmented(t *testing.T) {
	origNow := responsesNowUnix
	responsesNowUnix = func() int64 { return 1700000000 }
	defer func() { responsesNowUnix = origNow }()
	events, rerr := runResponsesStream(t, []string{
		respChatChunk(`{"role":"assistant"}`, ""),
		respChatChunk(`{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]}`, ""),
		respChatChunk(`{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}`, ""),
		respChatChunk(`{"tool_calls":[{"index":1,"id":"call_2","type":"function","function":{"name":"ping","arguments":""}}]}`, ""),
		respChatChunk(`{"tool_calls":[{"index":0,"function":{"arguments":"\"Paris\"}"}}]}`, ""),
		respChatChunk(`{}`, "tool_calls"),
		respChatUsageChunk(),
		"[DONE]",
	})
	if rerr != nil {
		t.Fatalf("translate failed: %v", rerr)
	}
	var added, argDones int
	fcIDs := map[string]bool{}
	callIDs := map[string]bool{}
	for _, ev := range events {
		switch ev["type"] {
		case "response.output_item.added":
			item := ev["item"].(map[string]any)
			if item["type"] == "function_call" {
				added++
				id := item["id"].(string)
				if !strings.HasPrefix(id, "fc_") || fcIDs[id] {
					t.Fatalf("bad generated item id: %v", item)
				}
				fcIDs[id] = true
				callIDs[item["call_id"].(string)] = true
			}
		case "response.function_call_arguments.done":
			argDones++
			if ev["arguments"] == "" {
				t.Fatalf("arguments.done must carry full args: %v", ev)
			}
		}
	}
	if added != 2 || argDones != 2 {
		t.Fatalf("expected 2 calls with done args, got added=%d done=%d", added, argDones)
	}
	if !callIDs["call_1"] || !callIDs["call_2"] {
		t.Fatalf("upstream call identities lost: %v", callIDs)
	}
	completed := events[len(events)-1]["response"].(map[string]any)
	output := completed["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("terminal must carry both calls: %v", output)
	}
	args := map[string]string{}
	for _, item := range output {
		fc := item.(map[string]any)
		args[fc["call_id"].(string)] = fc["arguments"].(string)
	}
	if args["call_1"] != `{"city":"Paris"}` || args["call_2"] != "{}" {
		t.Fatalf("fragmented args misreconstructed: %v", args)
	}
}

func TestResponsesStreamPromptReturnAndCounters(t *testing.T) {
	events, rerr := runResponsesStream(t, []string{
		respChatChunk(`{"content":"Hi"}`, ""),
		respChatChunk(`{}`, "stop"),
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"u","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110,"prompt_tokens_details":{"cached_tokens":40}}}`,
		"[DONE]",
	})
	if rerr != nil {
		t.Fatalf("translate failed: %v", rerr)
	}
	completed := events[len(events)-1]["response"].(map[string]any)
	usage := completed["usage"].(map[string]any)
	if usage["input_tokens"] != float64(100) || usage["total_tokens"] != float64(110) {
		t.Fatalf("counter mismatch: %v", usage)
	}
	if usage["input_tokens_details"].(map[string]any)["cached_tokens"] != float64(40) {
		t.Fatalf("cached counter lost: %v", usage)
	}
}

func TestResponsesStreamFailures(t *testing.T) {
	tooBig := strings.Repeat("x", DefaultLimits().MaxArgsChars+1)
	cases := map[string]struct {
		chunks []string
	}{
		"truncated no DONE": {[]string{respChatChunk(`{"content":"Hi"}`, "stop")}},
		"no usage": {[]string{
			respChatChunk(`{"content":"Hi"}`, ""),
			respChatChunk(`{}`, "stop"),
			"[DONE]"}},
		"usage before finish": {[]string{respChatUsageChunk(), "[DONE]"}},
		"duplicate finish": {[]string{
			respChatChunk(`{}`, "stop"),
			respChatChunk(`{}`, "stop"),
			respChatUsageChunk(), "[DONE]"}},
		"partial identity": {[]string{
			respChatChunk(`{"tool_calls":[{"index":0,"id":"call_1","function":{"arguments":"{}"}}]}`, ""),
			respChatChunk(`{}`, "tool_calls"),
			respChatUsageChunk(), "[DONE]"}},
		"malformed args": {[]string{
			respChatChunk(`{"tool_calls":[{"index":0,"id":"call_1","type":"function",
				"function":{"name":"a","arguments":"[1]"}}]}`, ""),
			respChatChunk(`{}`, "tool_calls"),
			respChatUsageChunk(), "[DONE]"}},
		"oversize args": {[]string{
			respChatChunk(fmt.Sprintf(`{"tool_calls":[{"index":0,"id":"call_1","type":"function",
				"function":{"name":"a","arguments":%q}}]}`, tooBig), ""),
			respChatChunk(`{}`, "tool_calls"),
			respChatUsageChunk(), "[DONE]"}},
		"unknown event": {[]string{
			respChatChunk(`{"refusal":"no"}`, ""),
			"[DONE]"}},
		"upstream error": {[]string{
			respChatChunk(`{"content":"Hi"}`, ""),
			`{"error":{"message":"boom","type":"server_error"}}`}},
		"multiple choices": {[]string{
			`{"id":"c","object":"chat.completion.chunk","created":1,"model":"u","choices":[{"index":0,"delta":{},"finish_reason":null},{"index":1,"delta":{},"finish_reason":null}]}`}},
	}
	for name, tc := range cases {
		_, rerr := runResponsesStream(t, tc.chunks)
		if rerr == nil || rerr.Status != 502 {
			t.Fatalf("%s should fail closed with 502, got %v", name, rerr)
		}
	}
	// A stream error after commit must emit an in-band failure event.
	var in bytes.Buffer
	in.WriteString("data: " + respChatChunk(`{"content":"Hi"}`, "") + "\n\n")
	in.WriteString(`data: {"error":{"message":"boom"}}` + "\n\n")
	var out bytes.Buffer
	tr := &ChatToResponsesSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
	rerr := tr.Translate(&in, &out, nil)
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("expected post-commit failure, got %v", rerr)
	}
	found := false
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.Contains(line, "response.failed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing in-band response.failed after commit: %q", out.String())
	}
}

func TestResponsesStreamWriterFailureStops(t *testing.T) {
	var in bytes.Buffer
	in.WriteString("data: " + respChatChunk(`{"content":"Hi"}`, "") + "\n\n")
	tr := &ChatToResponsesSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
	rerr := tr.Translate(&in, failWriter{}, nil)
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("writer failure must stop the stream, got %v", rerr)
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, fmt.Errorf("downstream closed") }

func TestResponsesStreamLengthIncomplete(t *testing.T) {
	events, rerr := runResponsesStream(t, []string{
		respChatChunk(`{"content":"partial"}`, ""),
		respChatChunk(`{}`, "length"),
		respChatUsageChunk(),
		"[DONE]",
	})
	if rerr != nil {
		t.Fatalf("translate failed: %v", rerr)
	}
	last := events[len(events)-1]
	if last["type"] != "response.incomplete" {
		t.Fatalf("length must end incomplete, got %v", last["type"])
	}
	resp := last["response"].(map[string]any)
	if resp["incomplete_details"].(map[string]any)["reason"] != "max_output_tokens" {
		t.Fatalf("bad incomplete reason: %v", resp)
	}
}

func TestResponsesStreamToolFirstThenText(t *testing.T) {
	events, rerr := runResponsesStream(t, []string{
		respChatChunk(`{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]}`, ""),
		respChatChunk(`{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\"Paris\"}"}}]}`, ""),
		respChatChunk(`{"content":"done"}`, ""),
		respChatChunk(`{}`, "tool_calls"),
		respChatUsageChunk(),
		"[DONE]",
	})
	if rerr != nil {
		t.Fatalf("tool-first then text failed: %v", rerr)
	}
	// Collect output_index per added item and verify uniqueness.
	added := map[string]float64{}
	itemIDs := map[string]bool{}
	for _, ev := range events {
		if ev["type"] == "response.output_item.added" {
			item := ev["item"].(map[string]any)
			idx := ev["output_index"].(float64)
			id := item["id"].(string)
			if _, dup := added[id]; dup {
				t.Fatalf("duplicate added id %s", id)
			}
			added[id] = idx
			itemIDs[id] = true
		}
	}
	if len(added) != 2 {
		t.Fatalf("expected 2 added items (tool+text), got %v", added)
	}
	// Indices must be unique.
	seen := map[float64]bool{}
	for _, idx := range added {
		if seen[idx] {
			t.Fatalf("duplicate output_index %v", idx)
		}
		seen[idx] = true
	}
	// Terminal output must carry both items with stable identities.
	completed := events[len(events)-1]["response"].(map[string]any)
	output := completed["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("terminal must carry tool+text, got %v", output)
	}
	// Deltas must have arrived before completion (no withholding).
	foundDelta := false
	for _, ev := range events {
		if ev["type"] == "response.output_text.delta" || ev["type"] == "response.function_call_arguments.delta" {
			foundDelta = true
			break
		}
	}
	if !foundDelta {
		t.Fatal("first delta must arrive before upstream completion")
	}
	// Call identity must survive.
	foundCall := false
	for _, item := range output {
		m := item.(map[string]any)
		if m["type"] == "function_call" && m["call_id"] == "call_1" {
			foundCall = true
		}
	}
	if !foundCall {
		t.Fatalf("tool call identity lost in terminal: %v", output)
	}
}

func TestResponsesStreamTextToolText(t *testing.T) {
	events, rerr := runResponsesStream(t, []string{
		respChatChunk(`{"content":"hello "}`, ""),
		respChatChunk(`{"tool_calls":[{"index":0,"id":"call_9","type":"function","function":{"name":"ping","arguments":""}}]}`, ""),
		respChatChunk(`{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}`, ""),
		respChatChunk(`{"content":"world"}`, ""),
		respChatChunk(`{}`, "tool_calls"),
		respChatUsageChunk(),
		"[DONE]",
	})
	if rerr != nil {
		t.Fatalf("text->tool->text failed: %v", rerr)
	}
	completed := events[len(events)-1]["response"].(map[string]any)
	output := completed["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("expected text+tool terminal, got %v", output)
	}
	// Text must reconstruct in order despite interleaving.
	var text string
	for _, item := range output {
		m := item.(map[string]any)
		if m["type"] == "message" {
			content := m["content"].([]any)
			text = content[0].(map[string]any)["text"].(string)
		}
	}
	if text != "hello world" {
		t.Fatalf("interleaved text misordered: %q", text)
	}
}

func TestResponsesStreamParallelToolsEmptyArgs(t *testing.T) {
	events, rerr := runResponsesStream(t, []string{
		respChatChunk(`{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"a","arguments":""}}]}`, ""),
		respChatChunk(`{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"b","arguments":""}}]}`, ""),
		respChatChunk(`{}`, "tool_calls"),
		respChatUsageChunk(),
		"[DONE]",
	})
	if rerr != nil {
		t.Fatalf("parallel empty args failed: %v", rerr)
	}
	completed := events[len(events)-1]["response"].(map[string]any)
	output := completed["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("expected 2 parallel calls, got %v", output)
	}
	seenCall := map[string]string{}
	for _, item := range output {
		m := item.(map[string]any)
		if m["arguments"] != "{}" {
			t.Fatalf("empty args must render as {}: %v", m)
		}
		seenCall[m["call_id"].(string)] = m["id"].(string)
	}
	if len(seenCall) != 2 || seenCall["call_a"] == "" || seenCall["call_b"] == "" {
		t.Fatalf("call identities inconsistent: %v", seenCall)
	}
	// Item IDs stable across added/done/terminal.
	addedIDs := map[string]float64{}
	for _, ev := range events {
		if ev["type"] == "response.output_item.added" {
			item := ev["item"].(map[string]any)
			if item["type"] == "function_call" {
				addedIDs[item["id"].(string)] = ev["output_index"].(float64)
			}
		}
		if ev["type"] == "response.output_item.done" {
			item := ev["item"].(map[string]any)
			if item["type"] == "function_call" {
				idx := ev["output_index"].(float64)
				if addedIDs[item["id"].(string)] != idx {
					t.Fatalf("unstable index for %v", item)
				}
			}
		}
	}
}

func TestResponsesStreamAggregateBound(t *testing.T) {
	lim := DefaultLimits()
	lim.MaxStreamArgsChars = 16
	var in bytes.Buffer
	in.WriteString("data: " + respChatChunk(`{"content":"0123456789ABCDEF"}`, "") + "\n\n")
	in.WriteString("data: " + respChatChunk(`{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"a","arguments":"{}"}}]}`, "") + "\n\n")
	var out bytes.Buffer
	tr := &ChatToResponsesSSETranslator{PublicModel: "m", Limits: lim}
	// Text fills the aggregate budget; any tool args on top must fail.
	// First chunk alone fills 16; second text byte exceeds.
	in.WriteString("data: " + respChatChunk(`{"content":"X"}`, "") + "\n\n")
	rerr := tr.Translate(&in, &out, nil)
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("aggregate text bound must fail closed, got %v", rerr)
	}
}

func TestResponsesStreamFinalFrameBound(t *testing.T) {
	lim := DefaultLimits()
	lim.MaxStreamArgsChars = 200
	lim.MaxFrameBytes = 120
	// Text small enough for the aggregate bound but JSON escaping plus
	// envelope exceeds the frame cap on the terminal completed event.
	text := strings.Repeat("x", 100)
	var in bytes.Buffer
	in.WriteString("data: " + respChatChunk(`{"content":`+jsonQuote(text)+`}`, "") + "\n\n")
	in.WriteString("data: " + respChatChunk(`{}`, "stop") + "\n\n")
	in.WriteString("data: " + `{"id":"c","object":"chat.completion.chunk","created":1,"model":"u","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}` + "\n\n")
	in.WriteString("data: [DONE]\n\n")
	var out bytes.Buffer
	tr := &ChatToResponsesSSETranslator{PublicModel: "m", Limits: lim}
	rerr := tr.Translate(&in, &out, nil)
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("final frame over cap must fail without completed, got %v", rerr)
	}
	if strings.Contains(out.String(), "response.completed") {
		t.Fatalf("must not emit response.completed when final frame exceeds cap")
	}
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
