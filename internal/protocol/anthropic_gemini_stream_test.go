package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// g2aStreamEvents runs one upstream framing through the matching
// translator and splits the downstream Anthropic SSE into events.
func g2aStreamEvents(t *testing.T, upstream, framing string) []map[string]any {
	t.Helper()
	lim := DefaultLimits()
	var out bytes.Buffer
	var rerr *RouterError
	switch framing {
	case "sse":
		tr := &GeminiToAnthropicSSETranslator{PublicModel: "public", Limits: lim}
		rerr = tr.Translate(strings.NewReader(upstream), &out, nil)
	case "array":
		tr := &GeminiToAnthropicJSONArrayTranslator{PublicModel: "public", Limits: lim}
		rerr = tr.Translate(strings.NewReader(upstream), &out, nil)
	default:
		t.Fatalf("unknown framing %q", framing)
	}
	if rerr != nil {
		t.Fatalf("translate failed: %v", rerr)
	}
	return splitG2AEvents(t, out.String())
}

func g2aStreamExpectFail(t *testing.T, upstream, framing string) {
	t.Helper()
	lim := DefaultLimits()
	var out bytes.Buffer
	var rerr *RouterError
	switch framing {
	case "sse":
		tr := &GeminiToAnthropicSSETranslator{PublicModel: "public", Limits: lim}
		rerr = tr.Translate(strings.NewReader(upstream), &out, nil)
	case "array":
		tr := &GeminiToAnthropicJSONArrayTranslator{PublicModel: "public", Limits: lim}
		rerr = tr.Translate(strings.NewReader(upstream), &out, nil)
	}
	if rerr == nil {
		t.Fatalf("expected failure for %q, got success: %s", upstream, out.String())
	}
}

func splitG2AEvents(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, frame := range strings.Split(raw, "\n\n") {
		frame = strings.TrimSpace(frame)
		if frame == "" {
			continue
		}
		lines := strings.Split(frame, "\n")
		if len(lines) != 2 {
			t.Fatalf("bad downstream frame: %q", frame)
		}
		name := strings.TrimPrefix(lines[0], "event: ")
		payload := strings.TrimPrefix(lines[1], "data: ")
		var ev map[string]any
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			t.Fatalf("bad downstream payload: %v", err)
		}
		if ev["type"] != name {
			t.Fatalf("event/payload type mismatch: %q vs %q", name, ev["type"])
		}
		events = append(events, ev)
	}
	return events
}

func g2aSSEChunk(obj string) string {
	return "data: " + obj + "\n\n"
}

func TestG2AStreamTextBothFramings(t *testing.T) {
	for _, framing := range []string{"sse", "array"} {
		var upstream string
		c1 := `{"candidates":[{"content":{"role":"model","parts":[{"text":"hel"}]}}]` +
			`,"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":0,"totalTokenCount":10}}`
		c2 := `{"candidates":[{"content":{"role":"model","parts":[{"text":"lo"}]},"finishReason":"STOP"}],` +
			`"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":3,"totalTokenCount":13}}`
		if framing == "sse" {
			upstream = g2aSSEChunk(c1) + g2aSSEChunk(c2)
		} else {
			upstream = "[" + c1 + "," + c2 + "]"
		}
		events := g2aStreamEvents(t, upstream, framing)
		var kinds []string
		for _, ev := range events {
			kinds = append(kinds, ev["type"].(string))
		}
		want := []string{"message_start", "content_block_start", "content_block_delta",
			"content_block_delta", "content_block_stop", "message_delta", "message_stop"}
		if strings.Join(kinds, ",") != strings.Join(want, ",") {
			t.Fatalf("%s: event order wrong: %v", framing, kinds)
		}
		// Start usage carries the actual running counts when the first
		// chunk already reported them; the terminal corrects
		// authoritatively.
		start := events[0]["message"].(map[string]any)["usage"].(map[string]any)
		if start["input_tokens"] != float64(10) || start["output_tokens"] != float64(0) {
			t.Fatalf("%s: start usage must use actual counts when available: %v", framing, start)
		}
		delta := events[5]["delta"].(map[string]any)
		if delta["stop_reason"] != "end_turn" {
			t.Fatalf("%s: stop wrong: %v", framing, delta)
		}
		usage := events[5]["usage"].(map[string]any)
		if usage["input_tokens"] != float64(10) || usage["output_tokens"] != float64(3) {
			t.Fatalf("%s: terminal usage wrong: %v", framing, usage)
		}
	}
}

func TestG2AStreamStartPlaceholder(t *testing.T) {
	// When no chunk has carried usage yet, message_start emits explicit
	// zeros as a placeholder; the terminal corrects authoritatively.
	mid := `{"candidates":[{"content":{"role":"model","parts":[{"text":"t"}]}}]}`
	fin := `{"candidates":[{"content":{"role":"model","parts":[{"text":"u"}]},"finishReason":"STOP"}],` +
		`"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}}`
	events := g2aStreamEvents(t, g2aSSEChunk(mid)+g2aSSEChunk(fin), "sse")
	start := events[0]["message"].(map[string]any)["usage"].(map[string]any)
	if start["input_tokens"] != float64(0) || start["output_tokens"] != float64(0) {
		t.Fatalf("start usage must be an explicit placeholder: %v", start)
	}
	usage := events[len(events)-2]["usage"].(map[string]any)
	if usage["input_tokens"] != float64(2) || usage["output_tokens"] != float64(1) {
		t.Fatalf("terminal usage wrong: %v", usage)
	}
}

func TestG2AStreamCallAndStartActual(t *testing.T) {
	// A complete function call streams one tool_use block with its full
	// input; the first chunk already carries running usage, so the start
	// reflects actual accounting and the terminal corrects it.
	c1 := `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"calc","args":{"n":7}}}]}}],` +
		`"usageMetadata":{"promptTokenCount":6,"candidatesTokenCount":1,"totalTokenCount":7}}`
	c2 := `{"candidates":[{"content":{"role":"model","parts":[{"text":"done"}]},"finishReason":"STOP"}],` +
		`"usageMetadata":{"promptTokenCount":6,"candidatesTokenCount":2,"totalTokenCount":8}}`
	events := g2aStreamEvents(t, g2aSSEChunk(c1)+g2aSSEChunk(c2), "sse")
	var kinds []string
	for _, ev := range events {
		kinds = append(kinds, ev["type"].(string))
	}
	want := []string{"message_start", "content_block_start", "content_block_delta",
		"content_block_start", "content_block_delta", "content_block_stop",
		"content_block_stop", "message_delta", "message_stop"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("call event order wrong: %v", kinds)
	}
	start := events[0]["message"].(map[string]any)["usage"].(map[string]any)
	if start["input_tokens"] != float64(6) || start["output_tokens"] != float64(1) {
		t.Fatalf("start usage should use actual accounting when available: %v", start)
	}
	toolStart := events[1]["content_block"].(map[string]any)
	if toolStart["type"] != "tool_use" || toolStart["name"] != "calc" {
		t.Fatalf("tool start wrong: %v", toolStart)
	}
	frag := events[2]["delta"].(map[string]any)
	if frag["type"] != "input_json_delta" || frag["partial_json"] != `{"n":7}` {
		t.Fatalf("call input not streamed complete: %v", frag)
	}
	if idx := events[1]["index"]; idx != float64(0) {
		t.Fatalf("tool start index wrong: %v", idx)
	}
	if idx := events[3]["index"]; idx != float64(1) {
		t.Fatalf("text start index wrong: %v", idx)
	}
	if idx := events[5]["index"]; idx != events[1]["index"] {
		t.Fatalf("tool block indices inconsistent: %v vs %v", events[5]["index"], events[1]["index"])
	}
	if idx := events[6]["index"]; idx != events[3]["index"] {
		t.Fatalf("text block indices inconsistent: %v vs %v", events[6]["index"], events[3]["index"])
	}
	delta := events[7]["delta"].(map[string]any)
	if delta["stop_reason"] != "tool_use" {
		t.Fatalf("calls must report tool_use, got %v", delta)
	}
}

func TestG2AStreamFailures(t *testing.T) {
	for _, framing := range []string{"sse", "array"} {
		wrap := func(chunks ...string) string {
			if framing == "sse" {
				return strings.Join(chunks, "")
			}
			return "[" + strings.Join(chunks, ",") + "]"
		}
		chunk := func(obj string) string {
			if framing == "sse" {
				return g2aSSEChunk(obj)
			}
			return obj
		}
		fin := `{"candidates":[{"content":{"role":"model","parts":[{"text":"t"}]},"finishReason":"STOP"}],` +
			`"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}}`
		mid := `{"candidates":[{"content":{"role":"model","parts":[{"text":"t"}]}}]}`
		// Truncation: EOF / missing bracket before the finish chunk.
		g2aStreamExpectFail(t, wrap(chunk(mid)), framing)
		// Terminal chunk without its own usage fails: no underreported total.
		noUsage := `{"candidates":[{"content":{"role":"model","parts":[{"text":"t"}]},"finishReason":"STOP"}]}`
		g2aStreamExpectFail(t, wrap(chunk(mid), chunk(noUsage)), framing)
		// Duplicate finish elements fail where the framing reads past the
		// terminal (JSON array). SSE returns promptly at the terminal, so
		// trailing bytes are left unread rather than diagnosed.
		if framing == "array" {
			g2aStreamExpectFail(t, wrap(chunk(fin), chunk(fin)), framing)
		} else {
			trailing := wrap(chunk(fin)) + g2aSSEChunk(fin)
			var out bytes.Buffer
			tr := &GeminiToAnthropicSSETranslator{PublicModel: "public", Limits: DefaultLimits()}
			if rerr := tr.Translate(strings.NewReader(trailing), &out, nil); rerr != nil {
				t.Fatalf("sse must return promptly at the terminal, got %v", rerr)
			}
			if n := len(splitG2AEvents(t, out.String())); n != 6 {
				t.Fatalf("sse terminal must emit exactly 6 events, got %d", n)
			}
		}
		// Unknown finish reasons fail.
		other := `{"candidates":[{"content":{"role":"model","parts":[{"text":"t"}]},"finishReason":"OTHER"}],` +
			`"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}}`
		g2aStreamExpectFail(t, wrap(chunk(other)), framing)
		// Conflicting tool identities fail.
		dup := `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"a","id":"dup","args":{}}},` +
			`{"functionCall":{"name":"b","id":"dup","args":{}}}]}}]}`
		g2aStreamExpectFail(t, wrap(chunk(dup)), framing)
		// A minted id colliding with a later supplied id fails closed.
		mint := `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"a","args":{}}},` +
			`{"functionCall":{"name":"b","id":"g2a-call-0","args":{}}}]}}]}`
		g2aStreamExpectFail(t, wrap(chunk(mint)), framing)
		// An upstream [DONE] is truncation on the SSE framing.
		if framing == "sse" {
			g2aStreamExpectFail(t, g2aSSEChunk(mid)+"data: [DONE]\n\n", framing)
		} else {
			// A trailing comma is invalid JSON, never a terminal.
			g2aStreamExpectFail(t, "["+mid+",]", framing)
		}
	}
}

func TestG2AStreamPostCommitErrorFraming(t *testing.T) {
	// After commitment, an upstream failure emits exactly one typed
	// Anthropic error and no message_stop; pre-commit failures emit no bytes.
	mid := g2aSSEChunk(`{"candidates":[{"content":{"role":"model","parts":[{"text":"t"}]}}]}`)
	bad := g2aSSEChunk(`{"candidates":[{"content":{"role":"model","parts":[{"text":"t"}]}}],"future":{}}`)
	var out bytes.Buffer
	tr := &GeminiToAnthropicSSETranslator{PublicModel: "public", Limits: DefaultLimits()}
	rerr := tr.Translate(strings.NewReader(mid+bad), &out, nil)
	if rerr == nil {
		t.Fatal("expected post-commit failure")
	}
	events := splitG2AEvents(t, out.String())
	last := events[len(events)-1]
	if last["type"] != "error" {
		t.Fatalf("post-commit failure must end with one error event, got %v", last["type"])
	}
	for _, ev := range events {
		if ev["type"] == "message_stop" {
			t.Fatal("no fabricated message_stop after a post-commit failure")
		}
	}
	// Pre-commit: an invalid first frame emits zero bytes.
	var pre bytes.Buffer
	tr2 := &GeminiToAnthropicSSETranslator{PublicModel: "public", Limits: DefaultLimits()}
	if rerr := tr2.Translate(strings.NewReader(bad), &pre, nil); rerr == nil {
		t.Fatal("expected pre-commit failure")
	}
	if pre.Len() != 0 {
		t.Fatalf("pre-commit failure must emit no bytes, got %q", pre.String())
	}
}

func g2aTerminalUsage(events []map[string]any) map[string]any {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i]["type"] == "message_delta" {
			return events[i]["usage"].(map[string]any)
		}
	}
	return nil
}

func TestG2AStreamCachedFractionsBothFramings(t *testing.T) {
	// The terminal input bucket excludes cached traffic (input =
	// prompt - cached) on both framings; conservation holds
	// (input + cache_read == prompt). Fractions include a near-total
	// cache (5 prompt / 4 cached) which must stay at 1, never negative.
	for _, tc := range []struct{ prompt, cache, output int }{
		{12, 4, 3}, {10, 0, 5}, {5, 4, 2}, {8, 8, 1}, {16, 1, 7},
	} {
		total := tc.prompt + tc.output
		usage := fmt.Sprintf(`{"promptTokenCount":%d,"cachedContentTokenCount":%d,`+
			`"candidatesTokenCount":%d,"totalTokenCount":%d}`, tc.prompt, tc.cache, tc.output, total)
		chunk := `{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP"}],` +
			`"usageMetadata":` + usage + `}`
		out, rerr := TranslateGeminiJSONToAnthropic([]byte(chunk), "public", DefaultLimits())
		if rerr != nil {
			t.Fatalf("JSON p=%d c=%d o=%d failed: %v", tc.prompt, tc.cache, tc.output, rerr)
		}
		var jbody g2aBody
		if err := json.Unmarshal(out, &jbody); err != nil {
			t.Fatal(err)
		}
		want := tc.prompt - tc.cache
		if jbody.Usage.Input != want || jbody.Usage.Output != tc.output || jbody.Usage.Read != tc.cache {
			t.Fatalf("JSON p=%d c=%d o=%d wrong: %+v", tc.prompt, tc.cache, tc.output, jbody.Usage)
		}
		for _, framing := range []string{"sse", "array"} {
			var upstream string
			if framing == "sse" {
				upstream = g2aSSEChunk(chunk)
			} else {
				upstream = "[" + chunk + "]"
			}
			events := g2aStreamEvents(t, upstream, framing)
			term := g2aTerminalUsage(events)
			if term == nil {
				t.Fatalf("%s p=%d: no terminal usage", framing, tc.prompt)
			}
			if term["input_tokens"] != float64(want) || term["output_tokens"] != float64(tc.output) {
				t.Fatalf("%s p=%d c=%d o=%d terminal wrong: %v", framing, tc.prompt, tc.cache, tc.output, term)
			}
			if _, ok := term["cache_read_input_tokens"]; tc.cache > 0 && !ok {
				t.Fatalf("%s p=%d: cache_read missing: %v", framing, tc.prompt, term)
			}
			if tc.cache == 0 {
				if _, ok := term["cache_read_input_tokens"]; ok {
					t.Fatalf("%s p=%d: zero cache must omit cache_read: %v", framing, tc.prompt, term)
				}
			}
		}
	}
}

func TestG2AStreamIntermediateCountsTerminalOverwrite(t *testing.T) {
	// Running counts in message_start are actual intermediate values;
	// the terminal overwrites them authoritatively.
	c1 := `{"candidates":[{"content":{"role":"model","parts":[{"text":"a"}]}}],` +
		`"usageMetadata":{"promptTokenCount":6,"candidatesTokenCount":1,"totalTokenCount":7}}`
	c2 := `{"candidates":[{"content":{"role":"model","parts":[{"text":"b"}]},"finishReason":"STOP"}],` +
		`"usageMetadata":{"promptTokenCount":12,"cachedContentTokenCount":4,"candidatesTokenCount":3,"totalTokenCount":15}}`
	for _, framing := range []string{"sse", "array"} {
		var upstream string
		if framing == "sse" {
			upstream = g2aSSEChunk(c1) + g2aSSEChunk(c2)
		} else {
			upstream = "[" + c1 + "," + c2 + "]"
		}
		events := g2aStreamEvents(t, upstream, framing)
		start := events[0]["message"].(map[string]any)["usage"].(map[string]any)
		if start["input_tokens"] != float64(6) || start["output_tokens"] != float64(1) {
			t.Fatalf("%s: start must carry intermediate counts: %v", framing, start)
		}
		term := g2aTerminalUsage(events)
		if term["input_tokens"] != float64(8) || term["output_tokens"] != float64(3) ||
			term["cache_read_input_tokens"] != float64(4) {
			t.Fatalf("%s: terminal must overwrite authoritatively: %v", framing, term)
		}
	}
}
