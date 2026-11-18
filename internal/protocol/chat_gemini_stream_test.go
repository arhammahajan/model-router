// Gemini -> Chat streaming tests: SSE and JSON-array framings.
package protocol

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func g2cProviderChunk(text, finish string, prompt, cand int, extra string) string {
	var b strings.Builder
	b.WriteString(`{"candidates":[{"content":{"role":"model","parts":[`)
	wrote := false
	if text != "" || extra == "" {
		b.WriteString(`{"text":` + g2cQuote(text) + `}`)
		wrote = true
	}
	if extra != "" {
		if wrote {
			b.WriteString(",")
		}
		b.WriteString(extra)
	}
	b.WriteString(`]},"index":0`)
	if finish != "" {
		b.WriteString(`,"finishReason":` + g2cQuote(finish))
	}
	b.WriteString(`}],`)
	b.WriteString(`"usageMetadata":{"promptTokenCount":` + itoa(prompt) +
		`,"candidatesTokenCount":` + itoa(cand) +
		`,"totalTokenCount":` + itoa(prompt+cand) + `},`)
	b.WriteString(`"modelVersion":"gemini-2.5-flash","responseId":"resp-constant"}`)
	return b.String()
}

func g2cQuote(s string) string {
	q, _ := json.Marshal(s)
	return string(q)
}

func g2cSSE(chunks ...string) io.Reader {
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString("data: " + c + "\n\n")
	}
	return strings.NewReader(b.String())
}

func g2cArray(chunks ...string) io.Reader {
	var b strings.Builder
	b.WriteString("[\n")
	for i, c := range chunks {
		// Pretty-print each element so the parser exercises
		// multi-line elements and bare separators.
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, []byte(c), "", "  "); err != nil {
			panic(err)
		}
		b.Write(pretty.Bytes())
		if i+1 < len(chunks) {
			b.WriteString("\n,\n")
		} else {
			b.WriteString("\n")
		}
	}
	b.WriteString("]\n")
	return strings.NewReader(b.String())
}

// g2cCollectDownstream parses Chat SSE output into finish/usage/text/calls.
type g2cDownstream struct {
	text     strings.Builder
	finish   string
	hasUsage bool
	prompt   int
	complete int
	calls    map[int]map[string]string
	done     bool
	frames   int
}

func g2cCollect(t *testing.T, out []byte) *g2cDownstream {
	t.Helper()
	d := &g2cDownstream{calls: map[int]map[string]string{}}
	frames := bytes.Split(out, []byte("\n\n"))
	for _, f := range frames {
		f = bytes.TrimSpace(f)
		if len(f) == 0 {
			continue
		}
		d.frames++
		line := string(f)
		if line == "data: [DONE]" {
			d.done = true
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			t.Fatalf("bad downstream frame: %q", line)
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			t.Fatalf("downstream not JSON: %v", err)
		}
		if u, ok := chunk["usage"].(map[string]any); ok {
			d.hasUsage = true
			d.prompt = int(u["prompt_tokens"].(float64))
			d.complete = int(u["completion_tokens"].(float64))
		}
		choices := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		ch := choices[0].(map[string]any)
		if fr, ok := ch["finish_reason"]; ok && fr != nil {
			d.finish = fr.(string)
		}
		delta, _ := ch["delta"].(map[string]any)
		if c, ok := delta["content"].(string); ok {
			d.text.WriteString(c)
		}
		if tcs, ok := delta["tool_calls"].([]any); ok {
			for _, tc := range tcs {
				tm := tc.(map[string]any)
				idx := int(tm["index"].(float64))
				entry := d.calls[idx]
				if entry == nil {
					entry = map[string]string{}
					d.calls[idx] = entry
				}
				if id, ok := tm["id"].(string); ok && id != "" {
					entry["id"] = id
				}
				if fn, ok := tm["function"].(map[string]any); ok {
					if n, ok := fn["name"].(string); ok && n != "" {
						entry["name"] = n
					}
					if a, ok := fn["arguments"].(string); ok && a != "" {
						entry["args"] += a
					}
				}
			}
		}
	}
	return d
}

func TestG2CSSETextStream(t *testing.T) {
	in := g2cSSE(
		g2cProviderChunk("Hello", "", 11, 2, ""),
		g2cProviderChunk(" world", "", 11, 4, ""),
		g2cProviderChunk("", "STOP", 11, 6, ""),
	)
	tr := &GeminiToChatSSETranslator{PublicModel: "public-model", IncludeUsage: true, Limits: DefaultLimits()}
	var out bytes.Buffer
	if rerr := tr.Translate(in, &out, nil); rerr != nil {
		t.Fatalf("translate: %v", rerr)
	}
	d := g2cCollect(t, out.Bytes())
	if d.text.String() != "Hello world" {
		t.Fatalf("text = %q", d.text.String())
	}
	if d.finish != "stop" || !d.done {
		t.Fatalf("finish=%q done=%v", d.finish, d.done)
	}
	if !d.hasUsage || d.prompt != 11 || d.complete != 6 {
		t.Fatalf("usage prompt=%d complete=%d has=%v", d.prompt, d.complete, d.hasUsage)
	}
}

func TestG2CJSONArrayTextStream(t *testing.T) {
	in := g2cArray(
		g2cProviderChunk("Hello", "", 11, 2, ""),
		g2cProviderChunk(" world", "", 11, 4, ""),
		g2cProviderChunk("", "STOP", 11, 6, ""),
	)
	tr := &GeminiToChatJSONArrayTranslator{PublicModel: "public-model", IncludeUsage: true, Limits: DefaultLimits()}
	var out bytes.Buffer
	if rerr := tr.Translate(in, &out, nil); rerr != nil {
		t.Fatalf("translate: %v", rerr)
	}
	d := g2cCollect(t, out.Bytes())
	if d.text.String() != "Hello world" || d.finish != "stop" || !d.done {
		t.Fatalf("text=%q finish=%q done=%v", d.text.String(), d.finish, d.done)
	}
	if !d.hasUsage || d.prompt != 11 || d.complete != 6 {
		t.Fatalf("terminal usage must be last running counts: %+v", d)
	}
}

func TestG2CStreamToolCalls(t *testing.T) {
	fc := func(name, args, id string) string {
		return `{"functionCall":{"name":` + g2cQuote(name) + `,"args":` + args + `,"id":` + g2cQuote(id) + `}}`
	}
	for _, mk := range []struct {
		name string
		in   io.Reader
		tr   interface {
			Translate(io.Reader, io.Writer, func()) *RouterError
		}
	}{
		{"sse", g2cSSE(
			g2cProviderChunk("Hi", "", 9, 1, ""),
			g2cProviderChunk("", "", 9, 3, fc("get_weather", `{"city":"SF"}`, "call_1")),
			g2cProviderChunk("", "STOP", 9, 5, fc("get_time", `{}`, "call_2")),
		), &GeminiToChatSSETranslator{PublicModel: "m", IncludeUsage: false, Limits: DefaultLimits()}},
		{"array", g2cArray(
			g2cProviderChunk("Hi", "", 9, 1, ""),
			g2cProviderChunk("", "", 9, 3, fc("get_weather", `{"city":"SF"}`, "call_1")),
			g2cProviderChunk("", "STOP", 9, 5, fc("get_time", `{}`, "call_2")),
		), &GeminiToChatJSONArrayTranslator{PublicModel: "m", IncludeUsage: false, Limits: DefaultLimits()}},
	} {
		var out bytes.Buffer
		if rerr := mk.tr.Translate(mk.in, &out, nil); rerr != nil {
			t.Fatalf("%s: %v", mk.name, rerr)
		}
		d := g2cCollect(t, out.Bytes())
		if d.finish != "tool_calls" || !d.done {
			t.Fatalf("%s: finish=%q done=%v", mk.name, d.finish, d.done)
		}
		if len(d.calls) != 2 {
			t.Fatalf("%s: calls=%v", mk.name, d.calls)
		}
		if d.calls[0]["id"] != "call_1" || d.calls[0]["args"] != `{"city":"SF"}` {
			t.Fatalf("%s: call0=%v", mk.name, d.calls[0])
		}
		// Tool indexes unique across the stream.
		if d.calls[0]["id"] == d.calls[1]["id"] {
			t.Fatalf("%s: duplicate downstream ids", mk.name)
		}
		// Usage omitted when not requested.
		if d.hasUsage {
			t.Fatalf("%s: usage must be omitted", mk.name)
		}
	}
}

func TestG2CStreamNoReadPastTerminal(t *testing.T) {
	// Trailing bytes after the protocol terminal belong to the next
	// generation and must be left unread.
	trailing := "data: " + g2cProviderChunk("late", "", 1, 99, "") + "\n\n"
	in := io.MultiReader(g2cSSE(
		g2cProviderChunk("a", "", 3, 1, ""),
		g2cProviderChunk("", "STOP", 3, 2, ""),
	), strings.NewReader(trailing))
	tr := &GeminiToChatSSETranslator{PublicModel: "m", IncludeUsage: true, Limits: DefaultLimits()}
	var out bytes.Buffer
	if rerr := tr.Translate(in, &out, nil); rerr != nil {
		t.Fatalf("translate: %v", rerr)
	}
	d := g2cCollect(t, out.Bytes())
	if d.text.String() != "a" || d.complete != 2 {
		t.Fatalf("must stop at terminal: %+v", d)
	}
	rest, _ := io.ReadAll(in)
	if !strings.Contains(string(rest), "late") {
		t.Fatalf("trailing bytes must be left unread, got %q", rest)
	}
	// JSON array: return after `]` without requiring EOF.
	after := `{"late":true}`
	in2 := io.MultiReader(g2cArray(g2cProviderChunk("a", "", 3, 1, ""), g2cProviderChunk("", "STOP", 3, 2, "")), strings.NewReader(after))
	tr2 := &GeminiToChatJSONArrayTranslator{PublicModel: "m", IncludeUsage: false, Limits: DefaultLimits()}
	var out2 bytes.Buffer
	if rerr := tr2.Translate(in2, &out2, nil); rerr != nil {
		t.Fatalf("array: %v", rerr)
	}
	rest2, _ := io.ReadAll(in2)
	if string(rest2) != after {
		t.Fatalf("array must leave post-`]` bytes unread, got %q", rest2)
	}
}

func TestG2CStreamBlockedPrompt(t *testing.T) {
	blocked := `{"promptFeedback":{"blockReason":"SAFETY"},"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":0,"totalTokenCount":7}}`
	tr := &GeminiToChatSSETranslator{PublicModel: "m", IncludeUsage: false, Limits: DefaultLimits()}
	var out bytes.Buffer
	if rerr := tr.Translate(strings.NewReader("data: "+blocked+"\n\n"), &out, nil); rerr != nil {
		t.Fatalf("blocked SSE: %v", rerr)
	}
	d := g2cCollect(t, out.Bytes())
	if d.finish != "content_filter" || !d.done {
		t.Fatalf("blocked must be content_filter+DONE, got %+v", d)
	}
	tr2 := &GeminiToChatJSONArrayTranslator{PublicModel: "m", IncludeUsage: false, Limits: DefaultLimits()}
	var out2 bytes.Buffer
	if rerr := tr2.Translate(strings.NewReader("[\n"+blocked+"\n]\n"), &out2, nil); rerr != nil {
		t.Fatalf("blocked array: %v", rerr)
	}
	d2 := g2cCollect(t, out2.Bytes())
	if d2.finish != "content_filter" || !d2.done {
		t.Fatalf("blocked array must be content_filter+DONE, got %+v", d2)
	}
}

func TestG2CStreamTruncationFails(t *testing.T) {
	// SSE EOF before finish: no DONE, no finish.
	tr := &GeminiToChatSSETranslator{PublicModel: "m", IncludeUsage: false, Limits: DefaultLimits()}
	var out bytes.Buffer
	rerr := tr.Translate(g2cSSE(g2cProviderChunk("a", "", 3, 1, "")), &out, nil)
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("truncated SSE must be 502, got %v", rerr)
	}
	if bytes.Contains(out.Bytes(), []byte("[DONE]")) {
		t.Fatalf("truncation must not emit [DONE]")
	}
	// Upstream [DONE] sentinel (not part of Gemini SSE) is truncation.
	var out2 bytes.Buffer
	rerr = tr.Translate(strings.NewReader("data: "+g2cProviderChunk("a", "", 3, 1, "")+"\n\ndata: [DONE]\n\n"), &out2, nil)
	if rerr == nil {
		t.Fatalf("upstream [DONE] without finish must fail")
	}
	// Array EOF before `]` fails without a fabricated finish.
	tr2 := &GeminiToChatJSONArrayTranslator{PublicModel: "m", IncludeUsage: false, Limits: DefaultLimits()}
	var out3 bytes.Buffer
	open := "[\n" + g2cProviderChunk("a", "", 3, 1, "")
	if rerr := tr2.Translate(strings.NewReader(open), &out3, nil); rerr == nil {
		t.Fatalf("truncated array must fail")
	}
	if bytes.Contains(out3.Bytes(), []byte("[DONE]")) {
		t.Fatalf("truncated array must not emit [DONE]")
	}
	// Array close without any finish fails.
	var out4 bytes.Buffer
	if rerr := tr2.Translate(g2cArray(g2cProviderChunk("a", "", 3, 1, "")), &out4, nil); rerr == nil {
		t.Fatalf("array without finish must fail")
	}
}

func TestG2CStreamContradictionsFail(t *testing.T) {
	mkSSE := func(chunks ...string) *RouterError {
		tr := &GeminiToChatSSETranslator{PublicModel: "m", IncludeUsage: false, Limits: DefaultLimits()}
		var out bytes.Buffer
		return tr.Translate(g2cSSE(chunks...), &out, nil)
	}
	// Duplicate tool ids.
	fc := `{"functionCall":{"name":"f","args":{},"id":"dup"}}`
	if rerr := mkSSE(g2cProviderChunk("", "", 2, 1, fc), g2cProviderChunk("", "STOP", 2, 2, fc)); rerr == nil {
		t.Fatalf("duplicate ids must fail")
	}
	// Response identity change mid-stream.
	c1 := strings.Replace(g2cProviderChunk("a", "", 2, 1, ""), "resp-constant", "resp-A", 1)
	c2 := strings.Replace(g2cProviderChunk("b", "STOP", 2, 2, ""), "resp-constant", "resp-B", 1)
	if rerr := mkSSE(c1, c2); rerr == nil {
		t.Fatalf("identity change must fail")
	}
	// Partial args / willContinue refused.
	pa := `{"functionCall":{"name":"f","partialArgs":[{"jsonPath":"$.a"}],"willContinue":true}}`
	if rerr := mkSSE(g2cProviderChunk("", "", 2, 1, pa)); rerr == nil {
		t.Fatalf("partialArgs must be refused")
	}
	// Thought signature refused.
	ts := `{"text":"t","thoughtSignature":"opaque"}`
	if rerr := mkSSE(g2cProviderChunk("", "", 2, 1, ts)); rerr == nil {
		t.Fatalf("thoughtSignature must fail")
	}
	// Unknown finish reason fails.
	if rerr := mkSSE(g2cProviderChunk("x", "MALFORMED_FUNCTION_CALL", 2, 2, "")); rerr == nil {
		t.Fatalf("unknown finish must fail")
	}
	// Safety finish with calls is contradictory.
	fc2 := `{"functionCall":{"name":"f","args":{},"id":"c1"}}`
	if rerr := mkSSE(g2cProviderChunk("", "SAFETY", 2, 2, fc2)); rerr == nil {
		t.Fatalf("safety with calls must fail")
	}
	// Missing terminal usage fails instead of underreporting.
	noUsage := `{"candidates":[{"content":{"role":"model","parts":[{"text":"x"}]},"finishReason":"STOP"}]}`
	if rerr := mkSSE(noUsage); rerr == nil {
		t.Fatalf("missing usage must fail")
	}
}

func TestG2CStreamBoundsAndBackpressure(t *testing.T) {
	lim := DefaultLimits()
	lim.MaxFrameBytes = 64
	tr := &GeminiToChatSSETranslator{PublicModel: "m", IncludeUsage: false, Limits: lim}
	var out bytes.Buffer
	big := strings.Repeat("x", 128)
	if rerr := tr.Translate(g2cSSE(g2cProviderChunk(big, "", 2, 1, "")), &out, nil); rerr == nil {
		t.Fatalf("oversize frame must fail")
	}
	// Oversize tool args fail.
	lim2 := DefaultLimits()
	lim2.MaxArgsChars = 8
	tr2 := &GeminiToChatSSETranslator{PublicModel: "m", IncludeUsage: false, Limits: lim2}
	var out2 bytes.Buffer
	fc := `{"functionCall":{"name":"f","args":{"k":"0123456789"},"id":"c1"}}`
	if rerr := tr2.Translate(g2cSSE(g2cProviderChunk("", "STOP", 2, 2, fc)), &out2, nil); rerr == nil {
		t.Fatalf("oversize args must fail")
	}
	// Too many calls fail.
	lim3 := DefaultLimits()
	lim3.MaxToolCalls = 1
	tr3 := &GeminiToChatSSETranslator{PublicModel: "m", IncludeUsage: false, Limits: lim3}
	var out3 bytes.Buffer
	fc1 := `{"functionCall":{"name":"f","args":{},"id":"c1"}}`
	fc2 := `{"functionCall":{"name":"g","args":{},"id":"c2"}}`
	if rerr := tr3.Translate(g2cSSE(g2cProviderChunk("", "STOP", 2, 2, fc1+","+fc2)), &out3, nil); rerr == nil {
		// Two calls may arrive in separate parts of one chunk; build that shape instead.
		t.Fatalf("expected bound failure, got nil (shape may differ)")
	}
	// Downstream write failure stops the stream with an error and no DONE.
	tr4 := &GeminiToChatSSETranslator{PublicModel: "m", IncludeUsage: false, Limits: DefaultLimits()}
	if rerr := tr4.Translate(g2cSSE(g2cProviderChunk("a", "", 2, 1, "")), g2cFailWriter{}, nil); rerr == nil {
		t.Fatalf("write failure must error")
	}
}

type g2cFailWriter struct{}

func (g2cFailWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
