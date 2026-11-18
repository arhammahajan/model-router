// Gemini -> Responses streaming tests: SSE and JSON-array upstream
// framings into Responses named SSE downstream.
package protocol

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
)

func TestGeminiToResponsesCombinedTextAndArgsBound(t *testing.T) {
	lim := DefaultLimits()
	lim.MaxStreamArgsChars = 16
	first := g2rTextChunk("0123456789", "", "")
	last := g2rPartsChunk([]string{`{"functionCall":{"id":"call1","name":"f","args":{"x":1}}}`}, "STOP", g2rUsageJSON(1, 1))
	for _, framing := range []string{"sse", "array"} {
		t.Run(framing, func(t *testing.T) {
			var out strings.Builder
			var rerr *RouterError
			if framing == "sse" {
				tr := GeminiToResponsesSSETranslator{PublicModel: "m", Limits: lim}
				rerr = tr.Translate(strings.NewReader(g2rSSEUpstream(first, last)), &out, nil)
			} else {
				tr := GeminiToResponsesJSONArrayTranslator{PublicModel: "m", Limits: lim}
				rerr = tr.Translate(strings.NewReader("["+first+","+last+"]"), &out, nil)
			}
			if rerr == nil || rerr.Status != 502 {
				t.Fatalf("combined 10-byte text and 7-byte args must exceed 16-byte retention bound: %v", rerr)
			}
			if strings.Contains(out.String(), "event: response.completed\n") || strings.Contains(out.String(), "event: response.incomplete\n") {
				t.Fatal("oversize retained output emitted a success marker")
			}
		})
	}
}

type g2rEvent struct {
	name string
	data map[string]any
}

func parseG2REvents(t *testing.T, out string) []g2rEvent {
	t.Helper()
	var events []g2rEvent
	for _, frame := range strings.Split(out, "\n\n") {
		frame = strings.TrimSpace(frame)
		if frame == "" {
			continue
		}
		var name, data string
		for _, line := range strings.Split(frame, "\n") {
			if v, ok := strings.CutPrefix(line, "event: "); ok {
				name = v
			} else if v, ok := strings.CutPrefix(line, "data: "); ok {
				data = v
			}
		}
		if name == "" || data == "" {
			t.Fatalf("malformed downstream frame: %q", frame)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatalf("downstream data not JSON: %v", err)
		}
		events = append(events, g2rEvent{name: name, data: payload})
	}
	return events
}

func g2rSSEUpstream(frames ...string) string {
	var sb strings.Builder
	for _, f := range frames {
		sb.WriteString("data: " + f + "\n\n")
	}
	return sb.String()
}

func g2rUsageJSON(prompt, cand int) string {
	total := prompt + cand
	return `{"promptTokenCount":` + strconv.Itoa(prompt) + `,"candidatesTokenCount":` + strconv.Itoa(cand) +
		`,"totalTokenCount":` + strconv.Itoa(total) + `}`
}

func g2rTextChunk(text, finish, usage string) string {
	part := `{"text":` + strconvQuote(text) + `}`
	return g2rPartsChunk([]string{part}, finish, usage)
}

func g2rPartsChunk(parts []string, finish, usage string) string {
	chunk := `{"candidates":[{"content":{"role":"model","parts":[`
	chunk += strings.Join(parts, ",")
	chunk += `]}`
	if finish != "" {
		chunk += `,"finishReason":"` + finish + `"`
	}
	chunk += `}]`
	if usage != "" {
		chunk += `,"usageMetadata":` + usage
	}
	return chunk + `}`
}

func strconvQuote(s string) string {
	q, _ := json.Marshal(s)
	return string(q)
}

func TestGeminiToResponsesSSETextStream(t *testing.T) {
	upstream := g2rSSEUpstream(
		g2rTextChunk("Hello ", "", ""),
		g2rTextChunk("world", "STOP", g2rUsageJSON(10, 5)),
	)
	var out strings.Builder
	tr := &GeminiToResponsesSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
	if rerr := tr.Translate(strings.NewReader(upstream), &out, nil); rerr != nil {
		t.Fatalf("translate: %v", rerr)
	}
	events := parseG2REvents(t, out.String())
	names := make([]string, 0, len(events))
	for i, e := range events {
		names = append(names, e.name)
		if seq, ok := e.data["sequence_number"].(float64); !ok || int(seq) != i {
			t.Fatalf("event %d (%s): sequence_number = %v, want %d", i, e.name, e.data["sequence_number"], i)
		}
	}
	want := []string{"response.created", "response.in_progress",
		"response.output_item.added", "response.content_part.added",
		"response.output_text.delta", "response.output_text.delta",
		"response.output_text.done", "response.content_part.done",
		"response.output_item.done", "response.completed"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v", names)
	}
	// Stable identities across added/done/terminal.
	msgID := events[2].data["item"].(map[string]any)["id"].(string)
	if events[7].data["item_id"] != msgID || events[8].data["item"].(map[string]any)["id"] != msgID {
		t.Fatalf("item ids unstable")
	}
	terminal := events[len(events)-1].data["response"].(map[string]any)
	if terminal["status"] != "completed" || terminal["model"] != "m" {
		t.Fatalf("terminal = %v", terminal)
	}
	output := terminal["output"].([]any)
	text := output[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]
	if text != "Hello world" {
		t.Fatalf("terminal text = %q", text)
	}
	usage := terminal["usage"].(map[string]any)
	if usage["input_tokens"] != float64(10) || usage["output_tokens"] != float64(5) ||
		usage["total_tokens"] != float64(15) {
		t.Fatalf("usage = %v", usage)
	}
	for _, e := range events[:len(events)-1] {
		if e.name == "response.failed" {
			t.Fatalf("no failed event allowed on success")
		}
	}
}

func TestGeminiToResponsesSSEToolStream(t *testing.T) {
	upstream := g2rSSEUpstream(
		g2rPartsChunk([]string{`{"functionCall":{"name":"lookup","args":{"n":9007199254740993},"id":"call-9"}}`}, "", ""),
		g2rPartsChunk([]string{`{"functionCall":{"name":"other","args":{}}}`}, "STOP", g2rUsageJSON(4, 6)),
	)
	var out strings.Builder
	tr := &GeminiToResponsesSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
	if rerr := tr.Translate(strings.NewReader(upstream), &out, nil); rerr != nil {
		t.Fatalf("translate: %v", rerr)
	}
	events := parseG2REvents(t, out.String())
	var added []g2rEvent
	indices := map[float64]bool{}
	for i, e := range events {
		if e.name == "response.output_item.added" {
			added = append(added, e)
		}
		if seq := e.data["sequence_number"].(float64); int(seq) != i {
			t.Fatalf("event %d: sequence_number = %v", i, seq)
		}
	}
	if len(added) != 2 {
		t.Fatalf("added events = %d", len(added))
	}
	// Explicit monotonic output_index across added items.
	first := added[0].data["output_index"].(float64)
	second := added[1].data["output_index"].(float64)
	if second != first+1 {
		t.Fatalf("output_index not monotonic: %v then %v", first, second)
	}
	indices[first] = true
	indices[second] = true
	// Stable call_id across added/delta/done/terminal.
	callID := added[0].data["item"].(map[string]any)["call_id"].(string)
	if callID != "call-9" {
		t.Fatalf("call_id = %q", callID)
	}
	terminal := events[len(events)-1]
	if terminal.name != "response.completed" {
		t.Fatalf("terminal = %s", terminal.name)
	}
	resp := terminal.data["response"].(map[string]any)
	output := resp["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("terminal output = %v", resp["output"])
	}
	call := output[0].(map[string]any)
	if call["call_id"] != "call-9" || !strings.Contains(call["arguments"].(string), "9007199254740993") {
		t.Fatalf("terminal call = %v", call)
	}
	minted := output[1].(map[string]any)["call_id"].(string)
	if minted == "call-9" || minted == "" {
		t.Fatalf("minted call_id collides: %q", minted)
	}
}

func TestGeminiToResponsesSSEFailures(t *testing.T) {
	// Invalid first chunk emits no bytes (pre-commit).
	var out strings.Builder
	tr := &GeminiToResponsesSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
	rerr := tr.Translate(strings.NewReader(g2rSSEUpstream(`{"bogus":true}`)), &out, nil)
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("expected 502, got %v", rerr)
	}
	if out.Len() != 0 {
		t.Fatalf("pre-commit failure must emit no bytes, got %q", out.String())
	}
	// Upstream [DONE] before finish is truncation, never a terminal.
	out.Reset()
	rerr = tr.Translate(strings.NewReader(g2rSSEUpstream(
		g2rTextChunk("hi", "", ""), `[DONE]`)), &out, nil)
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("expected 502 for early DONE, got %v", rerr)
	}
	events := parseG2REvents(t, out.String())
	failed := 0
	for _, e := range events {
		if e.name == "response.failed" {
			failed++
		}
		if e.name == "response.completed" || e.name == "response.incomplete" {
			t.Fatalf("no success marker allowed after truncation")
		}
	}
	if failed != 1 {
		t.Fatalf("post-commit failure must emit exactly one response.failed, got %d", failed)
	}
	// Terminal chunk without authoritative usage fails.
	out.Reset()
	rerr = tr.Translate(strings.NewReader(g2rSSEUpstream(
		g2rTextChunk("hi", "STOP", ""))), &out, nil)
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("expected 502 for missing terminal usage, got %v", rerr)
	}
	// EOF before finish is truncation.
	out.Reset()
	rerr = tr.Translate(strings.NewReader(g2rSSEUpstream(
		g2rTextChunk("hi", "", ""))), &out, nil)
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("expected 502 for truncation, got %v", rerr)
	}
	// Thought parts fail closed mid-stream.
	out.Reset()
	rerr = tr.Translate(strings.NewReader(g2rSSEUpstream(
		g2rPartsChunk([]string{`{"thought":true}`}, "STOP", g2rUsageJSON(1, 1)))), &out, nil)
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("expected 502 for thought part, got %v", rerr)
	}
	if out.Len() != 0 {
		t.Fatalf("thought failure precedes commitment: no bytes allowed, got %q", out.String())
	}
}

func TestGeminiToResponsesArrayStreams(t *testing.T) {
	// Valid array closes with ] and succeeds without EOF.
	upstream := "[" + g2rTextChunk("Hello ", "", "") + "," +
		g2rTextChunk("world", "STOP", g2rUsageJSON(10, 5)) + "]"
	var out strings.Builder
	tr := &GeminiToResponsesJSONArrayTranslator{PublicModel: "m", Limits: DefaultLimits()}
	if rerr := tr.Translate(strings.NewReader(upstream), &out, nil); rerr != nil {
		t.Fatalf("translate: %v", rerr)
	}
	events := parseG2REvents(t, out.String())
	if events[len(events)-1].name != "response.completed" {
		t.Fatalf("terminal = %s", events[len(events)-1].name)
	}
	resp := events[len(events)-1].data["response"].(map[string]any)
	text := resp["output"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]
	if text != "Hello world" {
		t.Fatalf("terminal text = %q", text)
	}
	// Bytes after the valid terminal belong to the next generation: trailing
	// garbage is left unprocessed and still succeeds.
	out.Reset()
	if rerr := tr.Translate(strings.NewReader(upstream+`{"evil":true}`), &out, nil); rerr != nil {
		t.Fatalf("trailing bytes must not fail the terminal: %v", rerr)
	}
	// Trailing comma is invalid JSON, never a terminal.
	out.Reset()
	if rerr := tr.Translate(strings.NewReader("["+g2rTextChunk("x", "STOP", g2rUsageJSON(1, 1))+`,]`), &out, nil); rerr == nil {
		t.Fatalf("trailing comma must fail")
	} else if rerr.Status != 502 {
		t.Fatalf("trailing comma: status = %d", rerr.Status)
	}
	// EOF before ] is truncation.
	out.Reset()
	if rerr := tr.Translate(strings.NewReader("["+g2rTextChunk("x", "STOP", g2rUsageJSON(1, 1))), &out, nil); rerr == nil {
		t.Fatalf("EOF before ] must fail")
	}
	// A late element after the finish element never succeeds.
	out.Reset()
	late := "[" + g2rTextChunk("x", "STOP", g2rUsageJSON(1, 1)) + "," +
		g2rTextChunk("late", "", "") + "]"
	if rerr := tr.Translate(strings.NewReader(late), &out, nil); rerr == nil {
		t.Fatalf("late element must fail")
	} else if rerr.Status != 502 {
		t.Fatalf("late element: status = %d", rerr.Status)
	}
	events = parseG2REvents(t, out.String())
	for _, e := range events {
		if e.name == "response.completed" || e.name == "response.incomplete" {
			t.Fatalf("late element must never produce a success marker")
		}
	}
}

func TestGeminiToResponsesStreamBlockedPrompt(t *testing.T) {
	blocked := `{"promptFeedback":{"blockReason":"SAFETY"},"usageMetadata":` + g2rUsageJSON(4, 0) + `}`
	var out strings.Builder
	tr := &GeminiToResponsesSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
	if rerr := tr.Translate(strings.NewReader(g2rSSEUpstream(blocked)), &out, nil); rerr != nil {
		t.Fatalf("translate: %v", rerr)
	}
	events := parseG2REvents(t, out.String())
	last := events[len(events)-1]
	if last.name != "response.incomplete" {
		t.Fatalf("terminal = %s", last.name)
	}
	resp := last.data["response"].(map[string]any)
	if resp["incomplete_details"].(map[string]any)["reason"] != "content_filter" {
		t.Fatalf("details = %v", resp["incomplete_details"])
	}
}

func TestGeminiToResponsesStreamBounds(t *testing.T) {
	lim := DefaultLimits()
	lim.MaxArgsChars = 16
	big := `{"functionCall":{"name":"f","args":{"k":"0123456789abcdef"}}}`
	var out strings.Builder
	tr := &GeminiToResponsesSSETranslator{PublicModel: "m", Limits: lim}
	rerr := tr.Translate(strings.NewReader(g2rSSEUpstream(
		g2rPartsChunk([]string{big}, "STOP", g2rUsageJSON(1, 1)))), &out, nil)
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("oversize args must fail, got %v", rerr)
	}
}

// failWriter counts downstream writes and fails every write. It proves a
// downstream failure never triggers additional writes (no failed-event
// retry after a broken sink).
type g2rFailWriter struct {
	writes int
}

func (w *g2rFailWriter) Write(p []byte) (int, error) {
	w.writes++
	return 0, errG2RDownstream
}

var errG2RDownstream = errors.New("downstream write failed")

func TestGeminiToResponsesStreamInterleavedOrder(t *testing.T) {
	upstream := g2rSSEUpstream(
		g2rTextChunk("a", "", ""),
		g2rPartsChunk([]string{`{"functionCall":{"name":"f","args":{},"id":"c1"}}`}, "", ""),
		g2rTextChunk("b", "STOP", g2rUsageJSON(4, 6)),
	)
	var out strings.Builder
	tr := &GeminiToResponsesSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
	if rerr := tr.Translate(strings.NewReader(upstream), &out, nil); rerr != nil {
		t.Fatalf("translate: %v", rerr)
	}
	events := parseG2REvents(t, out.String())
	var added []g2rEvent
	for i, e := range events {
		if seq := e.data["sequence_number"].(float64); int(seq) != i {
			t.Fatalf("event %d (%s): sequence_number = %v", i, e.name, e.data["sequence_number"])
		}
		if e.name == "response.output_item.added" {
			added = append(added, e)
		}
	}
	if len(added) != 3 {
		t.Fatalf("added = %d, want 3 (message, call, message): %v", len(added), out.String())
	}
	if added[0].data["item"].(map[string]any)["type"] != "message" ||
		added[1].data["item"].(map[string]any)["type"] != "function_call" ||
		added[2].data["item"].(map[string]any)["type"] != "message" {
		t.Fatalf("order lost: %v %v %v", added[0].data["item"], added[1].data["item"], added[2].data["item"])
	}
	i0 := added[0].data["output_index"].(float64)
	i1 := added[1].data["output_index"].(float64)
	i2 := added[2].data["output_index"].(float64)
	if !(i0 < i1 && i1 < i2) {
		t.Fatalf("output_index not monotonic: %v %v %v", i0, i1, i2)
	}
	terminal := events[len(events)-1]
	if terminal.name != "response.completed" {
		t.Fatalf("terminal = %s", terminal.name)
	}
	respOut := terminal.data["response"].(map[string]any)["output"].([]any)
	if len(respOut) != 3 ||
		respOut[0].(map[string]any)["type"] != "message" ||
		respOut[1].(map[string]any)["type"] != "function_call" ||
		respOut[2].(map[string]any)["type"] != "message" {
		t.Fatalf("terminal order lost: %v", terminal.data["response"])
	}
}

func TestGeminiToResponsesStreamToolFirstOrder(t *testing.T) {
	upstream := g2rSSEUpstream(
		g2rPartsChunk([]string{`{"functionCall":{"name":"f","args":{},"id":"call1"}}`}, "", ""),
		g2rTextChunk("after", "STOP", g2rUsageJSON(1, 1)),
	)
	var out strings.Builder
	tr := &GeminiToResponsesSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
	if rerr := tr.Translate(strings.NewReader(upstream), &out, nil); rerr != nil {
		t.Fatalf("translate: %v", rerr)
	}
	events := parseG2REvents(t, out.String())
	terminal := events[len(events)-1].data["response"].(map[string]any)
	output := terminal["output"].([]any)
	if len(output) != 2 || output[0].(map[string]any)["type"] != "function_call" ||
		output[1].(map[string]any)["type"] != "message" {
		t.Fatalf("tool-first order lost: %v", output)
	}
}

func TestGeminiToResponsesStreamRoleModelOnly(t *testing.T) {
	// Assistant alias rejected before any write.
	raw := `{"candidates":[{"content":{"role":"assistant","parts":[{"text":"x"}]}}]}`
	var out strings.Builder
	tr := &GeminiToResponsesSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
	rerr := tr.Translate(strings.NewReader(g2rSSEUpstream(raw)), &out, nil)
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("assistant role must fail, got %v", rerr)
	}
	if out.Len() != 0 {
		t.Fatalf("role failure pre-commit must emit no bytes, got %q", out.String())
	}
	// Missing role stays allowed.
	rawOK := `{"candidates":[{"content":{"parts":[{"text":"x"}]},"finishReason":"STOP"}],"usageMetadata":` + g2rUsageJSON(1, 1) + `}`
	out.Reset()
	if rerr := tr.Translate(strings.NewReader(g2rSSEUpstream(rawOK)), &out, nil); rerr != nil {
		t.Fatalf("missing role must succeed: %v", rerr)
	}
}

func TestGeminiToResponsesStreamEOFPostCommitFailed(t *testing.T) {
	// Valid text then EOF truncation: exactly one response.failed.
	upstream := g2rSSEUpstream(g2rTextChunk("x", "", ""))
	var out strings.Builder
	tr := &GeminiToResponsesSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
	rerr := tr.Translate(strings.NewReader(upstream), &out, nil)
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("expected 502, got %v", rerr)
	}
	events := parseG2REvents(t, out.String())
	failed := 0
	for _, e := range events {
		if e.name == "response.failed" {
			failed++
		}
		if e.name == "response.completed" || e.name == "response.incomplete" {
			t.Fatalf("no success marker allowed, got %s", e.name)
		}
	}
	if failed != 1 {
		t.Fatalf("post-commit EOF must emit exactly one response.failed, got %d", failed)
	}
	// Same guarantee for the JSON-array framing: valid element then EOF.
	out.Reset()
	ja := "[" + g2rTextChunk("x", "", "") + ","
	trJA := &GeminiToResponsesJSONArrayTranslator{PublicModel: "m", Limits: DefaultLimits()}
	rerr = trJA.Translate(strings.NewReader(ja), &out, nil)
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("expected 502 for array EOF, got %v", rerr)
	}
	events = parseG2REvents(t, out.String())
	failed = 0
	for _, e := range events {
		if e.name == "response.failed" {
			failed++
		}
		if e.name == "response.completed" || e.name == "response.incomplete" {
			t.Fatalf("no success marker allowed, got %s", e.name)
		}
	}
	if failed != 1 {
		t.Fatalf("array post-commit EOF must emit exactly one response.failed, got %d", failed)
	}
}

func TestGeminiToResponsesStreamWriterFailureNoRetry(t *testing.T) {
	upstream := g2rSSEUpstream(
		g2rTextChunk("hi", "", ""),
		g2rTextChunk("there", "STOP", g2rUsageJSON(1, 1)),
	)
	fw := &g2rFailWriter{}
	tr := &GeminiToResponsesSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
	rerr := tr.Translate(strings.NewReader(upstream), fw, nil)
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("expected downstream 502, got %v", rerr)
	}
	// First downstream write fails; no failed-event retry may follow.
	if fw.writes != 1 {
		t.Fatalf("downstream failure must not trigger additional writes, writes=%d", fw.writes)
	}
}

func TestGeminiToResponsesStreamModelVersionStable(t *testing.T) {
	withVer := func(text, ver, finish, usage string) string {
		chunk := g2rTextChunk(text, finish, usage)
		if ver == "" {
			return chunk
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(chunk), &m); err != nil {
			t.Fatalf("fixture: %v", err)
		}
		m["modelVersion"] = ver
		b, _ := json.Marshal(m)
		return string(b)
	}
	stable := g2rSSEUpstream(
		withVer("a", "m1", "", ""),
		withVer("b", "m1", "STOP", g2rUsageJSON(1, 1)),
	)
	var out strings.Builder
	tr := &GeminiToResponsesSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
	if rerr := tr.Translate(strings.NewReader(stable), &out, nil); rerr != nil {
		t.Fatalf("stable modelVersion must succeed: %v", rerr)
	}
	out.Reset()
	changed := g2rSSEUpstream(
		withVer("a", "m1", "", ""),
		withVer("b", "m2", "STOP", g2rUsageJSON(1, 1)),
	)
	rerr := tr.Translate(strings.NewReader(changed), &out, nil)
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("changed modelVersion must fail, got %v", rerr)
	}
	if strings.Contains(rerr.Msg, "m1") || strings.Contains(rerr.Msg, "m2") {
		t.Fatalf("identity error must not leak values: %q", rerr.Msg)
	}
}

func TestGeminiToResponsesStreamBlockedCoherence(t *testing.T) {
	// Blocked prompt after already emitted text fails even with no calls.
	upstream := g2rSSEUpstream(
		g2rTextChunk("hi", "", ""),
		`{"promptFeedback":{"blockReason":"SAFETY"},"usageMetadata":`+g2rUsageJSON(4, 0)+`}`,
	)
	var out strings.Builder
	tr := &GeminiToResponsesSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
	if rerr := tr.Translate(strings.NewReader(upstream), &out, nil); rerr == nil {
		t.Fatalf("blocked after text must fail")
	} else if rerr.Status != 502 {
		t.Fatalf("blocked after text: status = %d", rerr.Status)
	}
	// Safety finish with calls fails, matching the JSON contract.
	out.Reset()
	upstream = g2rSSEUpstream(
		g2rPartsChunk([]string{
			`{"text":"x"}`,
			`{"functionCall":{"name":"f","args":{},"id":"c1"}}`,
		}, "SAFETY", g2rUsageJSON(4, 6)),
	)
	if rerr := tr.Translate(strings.NewReader(upstream), &out, nil); rerr == nil {
		t.Fatalf("safety+call must fail")
	} else if rerr.Status != 502 {
		t.Fatalf("safety+call: status = %d", rerr.Status)
	}
}

func TestGeminiToResponsesStreamEmptyLifecycle(t *testing.T) {
	// No source content means no output item to open or close. The shared
	// Responses encoder preserves an empty output array, matching the
	// complete Gemini row, rather than inventing an empty message.
	empty := g2rSSEUpstream(
		`{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}],"usageMetadata":` + g2rUsageJSON(2, 0) + `}`,
	)
	var out strings.Builder
	tr := &GeminiToResponsesSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
	if rerr := tr.Translate(strings.NewReader(empty), &out, nil); rerr != nil {
		t.Fatalf("empty generation must succeed: %v", rerr)
	}
	events := parseG2REvents(t, out.String())
	for _, e := range events {
		if e.name == "response.output_item.added" || e.name == "response.output_item.done" {
			t.Fatalf("empty generation invented an output item: %v", out.String())
		}
	}
	terminal := events[len(events)-1]
	if terminal.name != "response.completed" {
		t.Fatalf("terminal = %s", terminal.name)
	}
	output := terminal.data["response"].(map[string]any)["output"].([]any)
	if len(output) != 0 {
		t.Fatalf("empty terminal output = %v", output)
	}
}
