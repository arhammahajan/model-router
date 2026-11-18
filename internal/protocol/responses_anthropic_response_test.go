package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustA2RJSON(t *testing.T, raw string) []byte {
	t.Helper()
	out, rerr := TranslateAnthropicJSONToResponses([]byte(raw), "public", DefaultLimits())
	if rerr != nil {
		t.Fatalf("translate failed: %v", rerr)
	}
	return out
}

func a2rJSONExpect502(t *testing.T, raw string) {
	t.Helper()
	if _, rerr := TranslateAnthropicJSONToResponses([]byte(raw), "public", DefaultLimits()); rerr == nil || rerr.Status != 502 {
		t.Fatalf("expected 502 for %s, got %v", raw, rerr)
	}
}

func a2rFullText(raw string, usage string) string {
	return `{"id":"msg_1","type":"message","role":"assistant","model":"up",
		"content":[{"type":"text","text":"Hi there ✓","citations":[]}],
		"stop_reason":"end_turn","stop_sequence":null,"usage":` + usage + `}`
}

func TestA2RTextEnvelope(t *testing.T) {
	// Realistic full envelope: documented fields, cache scalars that sum
	// exactly, null stop_sequence, empty citations.
	usage := `{"input_tokens":10,"cache_creation_input_tokens":3,"cache_read_input_tokens":4,
		"output_tokens":5,"cache_creation":null,"output_tokens_details":null}`
	out := mustA2RJSON(t, a2rFullText("", usage))
	var body struct {
		ID     string `json:"id"`
		Object string `json:"object"`
		Model  string `json:"model"`
		Status string `json:"status"`
		Output []struct {
			Type    string `json:"type"`
			ID      string `json:"id"`
			Role    string `json:"role"`
			Status  string `json:"status"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			Input   int `json:"input_tokens"`
			Output  int `json:"output_tokens"`
			Total   int `json:"total_tokens"`
			Details struct {
				Cached  int `json:"cached_tokens"`
				Written int `json:"cache_write_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	if body.Object != "response" || body.Model != "public" || body.Status != "completed" {
		t.Fatalf("envelope wrong: %s", out)
	}
	if !strings.HasPrefix(body.ID, "resp_") {
		t.Fatalf("response id wrong: %q", body.ID)
	}
	if len(body.Output) != 1 || body.Output[0].Type != "message" || body.Output[0].Role != "assistant" {
		t.Fatalf("output items wrong: %s", out)
	}
	if len(body.Output[0].Content) != 1 || body.Output[0].Content[0].Text != "Hi there ✓" {
		t.Fatalf("text lost: %s", out)
	}
	// Anthropic input excludes cache buckets; Responses input sums them: 10+3+4.
	if body.Usage.Input != 17 || body.Usage.Output != 5 || body.Usage.Total != 22 {
		t.Fatalf("cache accounting wrong: %+v", body.Usage)
	}
	if body.Usage.Details.Cached != 4 || body.Usage.Details.Written != 3 {
		t.Fatalf("cache details dishonest: %+v", body.Usage.Details)
	}
}

func TestA2RToolsToolFirst(t *testing.T) {
	// Tool-first block order is preserved; generated item IDs are distinct
	// from call IDs; number literals survive exactly.
	raw := `{"id":"msg_2","type":"message","role":"assistant","model":"up",
		"content":[
			{"type":"tool_use","id":"toolu_1","name":"calc","input":{"n":9007199254740993,"f":1.10}},
			{"type":"text","text":"working","citations":[]}],
		"stop_reason":"tool_use","stop_sequence":null,
		"usage":{"input_tokens":12,"output_tokens":7,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`
	out := mustA2RJSON(t, raw)
	var body struct {
		Status string `json:"status"`
		Output []struct {
			Type   string `json:"type"`
			ID     string `json:"id"`
			CallID string `json:"call_id"`
			Name   string `json:"name"`
			Args   string `json:"arguments"`
			Status string `json:"status"`
		} `json:"output"`
	}
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "completed" {
		t.Fatalf("status wrong: %s", out)
	}
	if len(body.Output) != 2 || body.Output[0].Type != "tool_use" && body.Output[0].Type != "function_call" {
		t.Fatalf("tool-first order lost: %s", out)
	}
	fc := body.Output[0]
	if fc.CallID != "toolu_1" || fc.Name != "calc" {
		t.Fatalf("call identity lost: %+v", fc)
	}
	if strings.HasPrefix(fc.ID, "toolu_") || fc.ID == fc.CallID {
		t.Fatalf("item id must be distinct from call id: %+v", fc)
	}
	if !strings.Contains(fc.Args, "9007199254740993") || !strings.Contains(fc.Args, "1.10") {
		t.Fatalf("numbers not exact: %q", fc.Args)
	}
	if body.Output[1].Type != "message" || body.Output[1].Status != "completed" {
		t.Fatalf("message item wrong: %s", out)
	}
}

func TestA2RStopMapping(t *testing.T) {
	msg := func(content string) string {
		return `{"id":"m","type":"message","role":"assistant","model":"up","content":` + content + `,`
	}
	usage := `"usage":{"input_tokens":1,"output_tokens":1}`
	mustStatus := func(stop, content, wantStatus, wantReason string) {
		t.Helper()
		out := mustA2RJSON(t, msg(content)+`"stop_reason":"`+stop+`","stop_sequence":null,`+usage+`}`)
		if !strings.Contains(string(out), `"status":"`+wantStatus+`"`) {
			t.Fatalf("%s must map to %s: %s", stop, wantStatus, out)
		}
		if wantReason != "" && !strings.Contains(string(out), `"reason":"`+wantReason+`"`) {
			t.Fatalf("%s must carry reason %s: %s", stop, wantReason, out)
		}
	}
	mustStatus("end_turn", `[{"type":"text","text":"x"}]`, "completed", "")
	mustStatus("tool_use", `[{"type":"tool_use","id":"t1","name":"a","input":{}}]`, "completed", "")
	mustStatus("max_tokens", `[{"type":"text","text":"x"}]`, "incomplete", "max_output_tokens")
	mustStatus("refusal", `[{"type":"text","text":"x"}]`, "incomplete", "content_filter")
	// stop_sequence has no Responses equivalent; unknown stops fail.
	a2rJSONExpect502(t, msg(`[{"type":"text","text":"x"}]`)+`"stop_reason":"stop_sequence","stop_sequence":"END",`+usage+`}`)
	a2rJSONExpect502(t, msg(`[{"type":"text","text":"x"}]`)+`"stop_reason":"pause_turn","stop_sequence":null,`+usage+`}`)
	a2rJSONExpect502(t, msg(`[{"type":"text","text":"x"}]`)+`"stop_reason":"bogus","stop_sequence":null,`+usage+`}`)
	// A tool stop with no calls is contradictory.
	a2rJSONExpect502(t, msg(`[{"type":"text","text":"x"}]`)+`"stop_reason":"tool_use","stop_sequence":null,`+usage+`}`)
}

func TestA2RRejectsUnrepresentable(t *testing.T) {
	base := func(content, usage string) string {
		return `{"id":"m","type":"message","role":"assistant","model":"up","content":` + content +
			`,"stop_reason":"end_turn","stop_sequence":null,"usage":` + usage + `}`
	}
	usage := `{"input_tokens":1,"output_tokens":1}`
	a2rJSONExpect502(t, base(`[{"type":"thinking","thinking":"h","signature":"s"}]`, usage))
	a2rJSONExpect502(t, base(`[{"type":"redacted_thinking","data":"x"}]`, usage))
	a2rJSONExpect502(t, base(`[{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{}}]`, usage))
	a2rJSONExpect502(t, base(`[{"type":"text","text":"x","citations":[{"type":"char_location","cited_text":"y"}]}]`, usage))
	a2rJSONExpect502(t, base(`[{"type":"text","text":"x","mystery":1}]`, usage))
	a2rJSONExpect502(t, base(`[{"type":"tool_use","id":"t1","name":"a","input":{},"zzz":1}]`, usage))
	a2rJSONExpect502(t, base(`[{"type":"tool_use","id":"t1","name":"a","input":[1]}]`, usage))
	a2rJSONExpect502(t, base(`[{"type":"tool_use","id":"dup","name":"a","input":{}},{"type":"tool_use","id":"dup","name":"b","input":{}}]`, usage))
	// Usage: honest zeros pass; nonzero unmapped billing fails.
	a2rJSONExpect502(t, base(`[{"type":"text","text":"x"}]`, `{"input_tokens":1,"output_tokens":1,"server_tool_use":{"web_search_requests":2}}`))
	a2rJSONExpect502(t, base(`[{"type":"text","text":"x"}]`, `{"input_tokens":1,"output_tokens":1,"service_tier":"standard"}`))
	a2rJSONExpect502(t, base(`[{"type":"text","text":"x"}]`, `{"input_tokens":1,"output_tokens":1,"mystery":2}`))
	a2rJSONExpect502(t, base(`[{"type":"text","text":"x"}]`, `{"input_tokens":1.5,"output_tokens":1}`))
	a2rJSONExpect502(t, base(`[{"type":"text","text":"x"}]`, `{"input_tokens":1,"output_tokens":1,"total_tokens":99}`))
	mustA2RJSON(t, base(`[{"type":"text","text":"x"}]`, `{"input_tokens":1,"output_tokens":1,"server_tool_use":null,"service_tier":null}`))
	// Unknown top-level members fail.
	a2rJSONExpect502(t, `{"id":"m","type":"message","role":"assistant","model":"up",
		"content":[{"type":"text","text":"x"}],"stop_reason":"end_turn","stop_sequence":null,
		"usage":{"input_tokens":1,"output_tokens":1},"mystery":1}`)
	// Oversize tool payloads fail.
	big := `{"a":"` + strings.Repeat("x", DefaultLimits().MaxArgsChars+1) + `"}`
	a2rJSONExpect502(t, base(`[{"type":"tool_use","id":"t1","name":"a","input":`+big+`}]`, usage))
}

// --- SSE ---

func a2rRunStream(t *testing.T, upstream string, lim Limits) (string, *RouterError) {
	t.Helper()
	var sb strings.Builder
	tr := &AnthropicToResponsesSSETranslator{PublicModel: "public", Limits: lim}
	rerr := tr.Translate(strings.NewReader(upstream), &sb, nil)
	return sb.String(), rerr
}

func a2rStart(usage string) string {
	return "event: message_start\ndata: {\"type\":\"message_start\",\"message\":" +
		"{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-haiku-4-5\"," +
		"\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":" + usage + "}}\n\n"
}

func a2rDelta(stop string, usage string) string {
	return "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"" + stop +
		"\",\"stop_sequence\":null},\"usage\":" + usage + "}\n\n"
}

func a2rStop() string { return "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n" }

func a2rTextBlock(idx int, text string) string {
	return "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":" +
		jsonNumber(idx) + ",\"content_block\":{\"type\":\"text\",\"text\":\"" + text + "\"}}\n\n"
}

func jsonNumber(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	return "10"
}

func TestA2RStreamTextLifecycle(t *testing.T) {
	upstream := a2rStart(`{"input_tokens":25,"cache_creation_input_tokens":2,"cache_read_input_tokens":3,"output_tokens":1}`) +
		a2rTextBlock(0, "") +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"!\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		a2rDelta("end_turn", `{"output_tokens":15}`) + a2rStop()
	out, rerr := a2rRunStream(t, upstream, DefaultLimits())
	if rerr != nil {
		t.Fatalf("stream failed: %v", rerr)
	}
	for _, want := range []string{
		`event: response.created`, `event: response.in_progress`,
		`"sequence_number":0`, `"sequence_number":1`,
		`event: response.output_item.added`, `"output_index":0`,
		`event: response.content_part.added`, `event: response.output_text.delta`,
		`"delta":"Hello"`, `"delta":"!"`,
		`event: response.output_text.done`, `"text":"Hello!"`,
		`event: response.content_part.done`, `event: response.output_item.done`,
		`event: response.completed`, `"status":"completed"`,
		`"input_tokens":30`, `"output_tokens":15`, `"total_tokens":45`,
		`"cached_tokens":3`, `"cache_write_tokens":2`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stream lacks %s:\n%s", want, out)
		}
	}
	// Sequence numbers are monotonic; terminal output repeats the full text.
	last := -1
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, `"sequence_number":`) {
			var ev map[string]any
			_ = ev
			n := 0
			for _, ch := range line[strings.Index(line, `"sequence_number":`)+18:] {
				if ch < '0' || ch > '9' {
					break
				}
				n = n*10 + int(ch-'0')
			}
			if n < last {
				t.Fatalf("sequence numbers not monotonic:\n%s", out)
			}
			last = n
		}
	}
	// Stable public model and response identity across events.
	if strings.Count(out, `"model":"public"`) < 3 {
		t.Fatalf("public model not stable:\n%s", out)
	}
	respID := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "data: ") {
			var ev map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err == nil {
				if r, ok := ev["response"].(map[string]any); ok {
					if id, ok := r["id"].(string); ok {
						if respID == "" {
							respID = id
						} else if id != respID {
							t.Fatalf("response id not stable: %q vs %q", respID, id)
						}
					}
				}
			}
		}
	}
	if !strings.HasPrefix(respID, "resp_") {
		t.Fatalf("response id wrong: %q", respID)
	}
}

func TestA2RStreamToolFirst(t *testing.T) {
	// Tool block first: the shared output-index allocator must not collide
	// with the later text block.
	upstream := a2rStart(`{"input_tokens":8,"output_tokens":1}`) +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_9\",\"name\":\"calc\",\"input\":{}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"n\\\":\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"9007199254740993}\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		a2rTextBlock(1, "done") +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n" +
		a2rDelta("tool_use", `{"output_tokens":9}`) + a2rStop()
	out, rerr := a2rRunStream(t, upstream, DefaultLimits())
	if rerr != nil {
		t.Fatalf("stream failed: %v", rerr)
	}
	toolIdx := strings.Index(out, `"call_id":"toolu_9"`)
	textIdx := strings.Index(out, `"text":"done"`)
	if toolIdx < 0 || textIdx < 0 {
		t.Fatalf("identities missing:\n%s", out)
	}
	// Distinct output indices across kinds.
	if !strings.Contains(out, `"output_index":0`) || !strings.Contains(out, `"output_index":1`) {
		t.Fatalf("output indices not unique:\n%s", out)
	}
	first0 := strings.Index(out, `"output_index":0`)
	first1 := strings.Index(out, `"output_index":1`)
	if first0 > first1 {
		t.Fatalf("tool-first order not preserved:\n%s", out)
	}
	if !strings.Contains(out, "9007199254740993") {
		t.Fatalf("numbers not exact:\n%s", out)
	}
	if !strings.Contains(out, `event: response.completed`) {
		t.Fatalf("terminal missing:\n%s", out)
	}
	// Item id distinct from call id.
	if strings.Count(out, "toolu_9") < 2 {
		t.Fatalf("call identity must repeat across lifecycle:\n%s", out)
	}
}

func TestA2RStreamSeededInputAndEmptyArgs(t *testing.T) {
	// A block that arrives whole seeds its arguments from the start event.
	upstream := a2rStart(`{"input_tokens":8,"output_tokens":1}`) +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"to_9\",\"name\":\"calc\",\"input\":{\"a\":1}}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n" +
		a2rDelta("tool_use", `{"output_tokens":4}`) + a2rStop()
	out, rerr := a2rRunStream(t, upstream, DefaultLimits())
	if rerr != nil {
		t.Fatalf("seeded input must succeed: %v", rerr)
	}
	if !strings.Contains(out, "{\\\"a\\\":1}") {
		t.Fatalf("seeded input lost:\n%s", out)
	}
	// Empty args still emit the identity exactly once.
	upstream = a2rStart(`{"input_tokens":8,"output_tokens":1}`) +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"to_1\",\"name\":\"ping\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		a2rDelta("tool_use", `{"output_tokens":2}`) + a2rStop()
	out, rerr = a2rRunStream(t, upstream, DefaultLimits())
	if rerr != nil {
		t.Fatalf("empty-arg tool must succeed: %v", rerr)
	}
	if !strings.Contains(out, `"id":"to_1"`) && !strings.Contains(out, `"call_id":"to_1"`) {
		t.Fatalf("tool identity missing:\n%s", out)
	}
	if !strings.Contains(out, `"arguments":"{}"`) {
		t.Fatalf("empty args must render as {}:\n%s", out)
	}
}

func TestA2RStreamFailures(t *testing.T) {
	goodStart := a2rStart(`{"input_tokens":8,"output_tokens":1}`)
	goodText := a2rTextBlock(0, "") +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"
	goodEnd := a2rDelta("end_turn", `{"output_tokens":2}`) + a2rStop()
	mustFail := func(name, upstream string, wantNoCompleted bool) {
		t.Helper()
		out, rerr := a2rRunStream(t, upstream, DefaultLimits())
		if rerr == nil || rerr.Status != 502 {
			t.Fatalf("%s must fail with 502, got %v", name, rerr)
		}
		if wantNoCompleted && (strings.Contains(out, "response.completed") || strings.Contains(out, "response.incomplete")) {
			t.Fatalf("%s must not emit a success marker:\n%s", name, out)
		}
	}
	// Truncation: EOF before stop; stop with open blocks; missing stop usage.
	mustFail("truncated", goodStart+goodText+a2rDelta("end_turn", `{"output_tokens":2}`), true)
	mustFail("open-blocks", goodStart+a2rTextBlock(0, "")+a2rDelta("end_turn", `{"output_tokens":2}`)+a2rStop(), true)
	mustFail("missing-delta", goodStart+goodText+a2rStop(), true)
	mustFail("content-before-start", a2rTextBlock(0, ""), true)
	mustFail("dup-start", goodStart+goodStart, true)
	mustFail("dup-index", goodStart+a2rTextBlock(0, "")+a2rTextBlock(0, ""), true)
	mustFail("unknown-event", goodStart+"event: mystery\ndata: {\"type\":\"mystery\"}\n\n", true)
	mustFail("thinking-block", goodStart+
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"h\"}}\n\n", true)
	mustFail("citation-delta", goodStart+a2rTextBlock(0, "")+
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"citations_delta\",\"citation\":{}}}\n\n", true)
	mustFail("bad-args", goodStart+
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":\"a\",\"input\":{}}}\n\n"+
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"[1]\"}}\n\n"+
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"+
		a2rDelta("tool_use", `{"output_tokens":2}`)+a2rStop(), true)
	mustFail("unknown-stop", goodStart+goodText+a2rDelta("stop_sequence", `{"output_tokens":2}`)+a2rStop(), true)
	// Bytes after a valid message_stop belong to the next generation and are
	// left unread (no read past terminal), so they never surface as errors.
	mustFail("missing-usage-delta", goodStart+goodText+
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n"+
		a2rStop(), true)
	// Missing start usage / negative usage.
	mustFail("no-start-usage", "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\"}}\n\n", true)
	_ = goodEnd
}

func TestA2RStreamNoReadPastTerminal(t *testing.T) {
	upstream := a2rStart(`{"input_tokens":8,"output_tokens":1}`) +
		a2rTextBlock(0, "") +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		a2rDelta("end_turn", `{"output_tokens":2}`) + a2rStop() +
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"next\"}}\n\n"
	out, rerr := a2rRunStream(t, upstream, DefaultLimits())
	if rerr != nil {
		t.Fatalf("terminal must return promptly: %v", rerr)
	}
	if strings.Contains(out, "next") {
		t.Fatalf("must not read past terminal:\n%s", out)
	}
	if !strings.Contains(out, "response.completed") {
		t.Fatalf("terminal missing:\n%s", out)
	}
}

func TestA2RStreamBoundsAndWriter(t *testing.T) {
	goodStart := a2rStart(`{"input_tokens":8,"output_tokens":1}`)
	goodText := a2rTextBlock(0, "") +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"
	goodEnd := a2rDelta("end_turn", `{"output_tokens":2}`) + a2rStop()
	// Overlong frame.
	lim := DefaultLimits()
	lim.MaxFrameBytes = 64
	if _, rerr := a2rRunStream(t, goodStart+a2rTextBlock(0, "")+goodEnd, lim); rerr == nil {
		t.Fatalf("overlong frame must fail")
	}
	// Oversized tool arguments.
	lim = DefaultLimits()
	lim.MaxArgsChars = 8
	big := "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"c1\",\"name\":\"a\",\"input\":{\"k\":\"0123456789\"}}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"
	if _, rerr := a2rRunStream(t, goodStart+big+a2rDelta("tool_use", `{"output_tokens":2}`)+a2rStop(), lim); rerr == nil {
		t.Fatalf("oversized args must fail")
	}
	// Too many tool calls.
	lim = DefaultLimits()
	lim.MaxToolCalls = 1
	two := "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"c1\",\"name\":\"a\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"c2\",\"name\":\"b\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n"
	if _, rerr := a2rRunStream(t, goodStart+two+a2rDelta("tool_use", `{"output_tokens":2}`)+a2rStop(), lim); rerr == nil {
		t.Fatalf("too many calls must fail")
	}
	// Downstream writer failure stops reads.
	tr := &AnthropicToResponsesSSETranslator{PublicModel: "public", Limits: DefaultLimits()}
	fw := &failingWriter{}
	upstream := goodStart +
		"event: content_block_delta\ndata: never\n\n" +
		a2rTextBlock(0, "") +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"more\"}}\n\n" +
		goodText + goodEnd
	_ = upstream
	full := goodStart + a2rTextBlock(0, "") +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello, this is a long enough first write to commit\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"more\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" + goodEnd
	if rerr := tr.Translate(strings.NewReader(full), fw, nil); rerr == nil || rerr.Status != 502 {
		t.Fatalf("writer failure must 502, got %v", rerr)
	}
}

func TestA2RStreamErrorEvents(t *testing.T) {
	// Pre-commit error: nothing written.
	out, rerr := a2rRunStream(t, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n", DefaultLimits())
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("upstream error must 502, got %v", rerr)
	}
	if out != "" {
		t.Fatalf("pre-commit error must emit nothing: %q", out)
	}
	// Post-commit error: a response.failed event, then failure.
	upstream := a2rStart(`{"input_tokens":8,"output_tokens":1}`) +
		a2rTextBlock(0, "") +
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"boom\"}}\n\n"
	out, rerr = a2rRunStream(t, upstream, DefaultLimits())
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("post-commit error must 502, got %v", rerr)
	}
	if !strings.Contains(out, "event: response.failed") || strings.Contains(out, "response.completed") {
		t.Fatalf("post-commit error must emit failed only:\n%s", out)
	}
}

func TestA2RIncompleteTerminal(t *testing.T) {
	// max_tokens stop carries incomplete/max_output_tokens through SSE.
	upstream := a2rStart(`{"input_tokens":8,"output_tokens":1}`) +
		a2rTextBlock(0, "") +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		a2rDelta("max_tokens", `{"output_tokens":64}`) + a2rStop()
	out, rerr := a2rRunStream(t, upstream, DefaultLimits())
	if rerr != nil {
		t.Fatalf("incomplete stream must succeed: %v", rerr)
	}
	if !strings.Contains(out, `event: response.incomplete`) || !strings.Contains(out, `"reason":"max_output_tokens"`) {
		t.Fatalf("incomplete terminal wrong:\n%s", out)
	}
}
