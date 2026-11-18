// Gemini target event encoder tests: Chat upstream through the shared
// decoder/pipeline into both Gemini downstream framings.
//
// These tests drive the public translators (Translate), never the old
// pair internals: every assertion pins wire behavior of the new
// (NewSSEFrameReader, NewChatStreamDecoder, NewGeminiEventEncoder,
// TranslateStream) selection. Builders are independent of the legacy
// pair-test helpers.
package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// --- Independent wire builders (not shared with pair tests) -----------------

func cgmChunk(id, model, delta, finish string) string {
	choice := `{"index":0`
	if delta != "" {
		choice += `,"delta":` + delta
	}
	if finish != "" {
		choice += `,"finish_reason":` + finish
	} else {
		choice += `,"finish_reason":null`
	}
	choice += `}`
	return "data: " + `{"id":"` + id + `","object":"chat.completion.chunk","created":1,"model":"` + model +
		`","choices":[` + choice + `]}` + "\n\n"
}

func cgmUsageChunk(id, model, usage string) string {
	return "data: " + `{"id":"` + id + `","object":"chat.completion.chunk","created":1,"model":"` + model +
		`","choices":[],"usage":` + usage + `}` + "\n\n"
}

func cgmUsage(prompt, completion int) string {
	return `{"prompt_tokens":` + itoa(prompt) + `,"completion_tokens":` + itoa(completion) +
		`,"total_tokens":` + itoa(prompt+completion) + `}`
}

func cgmRunSSE(t *testing.T, input string) (string, *RouterError) {
	t.Helper()
	var buf bytes.Buffer
	tr := &ChatToGeminiSSETranslator{PublicModel: "public-model", Limits: DefaultLimits()}
	rerr := tr.Translate(strings.NewReader(input), &buf, nil)
	return buf.String(), rerr
}

func cgmRunArray(t *testing.T, input string) (string, *RouterError) {
	t.Helper()
	var buf bytes.Buffer
	tr := &ChatToGeminiJSONArrayTranslator{PublicModel: "public-model", Limits: DefaultLimits()}
	rerr := tr.Translate(strings.NewReader(input), &buf, nil)
	return buf.String(), rerr
}

func cgmFrames(t *testing.T, sse string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, part := range strings.Split(sse, "data: ") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(part), &m); err != nil {
			t.Fatalf("downstream frame not JSON: %q", part)
		}
		out = append(out, m)
	}
	return out
}

func cgmTerminal(t *testing.T, payload string, isArray bool) map[string]any {
	t.Helper()
	if isArray {
		var arr []map[string]any
		if err := json.Unmarshal([]byte(payload), &arr); err != nil {
			t.Fatalf("array not valid JSON: %v %q", err, payload)
		}
		if len(arr) == 0 {
			t.Fatalf("empty array: %q", payload)
		}
		return arr[len(arr)-1]
	}
	frames := cgmFrames(t, payload)
	return frames[len(frames)-1]
}

func cgmCandidate(t *testing.T, frame map[string]any) map[string]any {
	t.Helper()
	cands, ok := frame["candidates"].([]any)
	if !ok || len(cands) != 1 {
		t.Fatalf("want one candidate: %v", frame)
	}
	return cands[0].(map[string]any)
}

// cgmWireArgsN extracts the raw "n" literal from the get_time call's args
// object anywhere on the wire (early tool frames or terminal), decoding
// with UseNumber so the test reader cannot blur the literal through
// float64. Early release means the call no longer lives in the terminal.
func cgmWireArgsN(t *testing.T, out string, isArray bool) string {
	t.Helper()
	var raws []string
	if !isArray {
		for _, part := range strings.Split(out, "data: ") {
			part = strings.TrimSpace(part)
			if part != "" {
				raws = append(raws, part)
			}
		}
	} else {
		dec := json.NewDecoder(strings.NewReader(out))
		dec.UseNumber()
		var arr []json.RawMessage
		if err := dec.Decode(&arr); err != nil || len(arr) == 0 {
			t.Fatalf("array not valid JSON: %v %q", err, out)
		}
		for _, m := range arr {
			raws = append(raws, string(m))
		}
	}
	for _, raw := range raws {
		var frame struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						FunctionCall *struct {
							Name string          `json:"name"`
							Args json.RawMessage `json:"args"`
						} `json:"functionCall"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		}
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&frame); err != nil {
			t.Fatalf("frame not JSON: %v in %q", err, out)
		}
		for _, c := range frame.Candidates {
			for _, p := range c.Content.Parts {
				if p.FunctionCall != nil && p.FunctionCall.Name == "get_time" {
					var args map[string]any
					dec2 := json.NewDecoder(strings.NewReader(string(p.FunctionCall.Args)))
					dec2.UseNumber()
					if err := dec2.Decode(&args); err != nil {
						t.Fatalf("args not an object: %v", err)
					}
					n, ok := args["n"].(json.Number)
					if !ok {
						t.Fatalf("n not a number literal: %v", args)
					}
					return string(n)
				}
			}
		}
	}
	t.Fatalf("get_time call missing: %q", out)
	return ""
}

// cgmTerminalRaw returns the raw bytes of the terminal element: the last
// SSE data payload, or the raw last array element.
func cgmTerminalRaw(t *testing.T, out string, isArray bool) string {
	t.Helper()
	if !isArray {
		parts := strings.Split(out, "data: ")
		last := strings.TrimSpace(parts[len(parts)-1])
		if last == "" {
			t.Fatalf("no frames: %q", out)
		}
		return last
	}
	dec := json.NewDecoder(strings.NewReader(out))
	dec.UseNumber()
	var arr []json.RawMessage
	if err := dec.Decode(&arr); err != nil || len(arr) == 0 {
		t.Fatalf("array not valid JSON: %v %q", err, out)
	}
	return string(arr[len(arr)-1])
}

const cgmID, cgmModel = "chatcmpl-gem", "up-model"

// --- Wire integrity: both framings ------------------------------------------

func TestGeminiEventTextBothFramings(t *testing.T) {
	input := cgmChunk(cgmID, cgmModel, `{"role":"assistant","content":"hel"}`, "") +
		cgmChunk(cgmID, cgmModel, `{"content":"lo"}`, "") +
		cgmChunk(cgmID, cgmModel, `{}`, `"stop"`) +
		cgmUsageChunk(cgmID, cgmModel, cgmUsage(9, 4)) +
		"data: [DONE]\n\n"
	out, rerr := cgmRunSSE(t, input)
	if rerr != nil {
		t.Fatalf("sse translate: %v", rerr.Msg)
	}
	if strings.Contains(out, "[DONE]") {
		t.Fatalf("Gemini SSE must not carry [DONE]: %q", out)
	}
	frames := cgmFrames(t, out)
	if len(frames) != 3 {
		t.Fatalf("want 2 text + 1 terminal frames, got %d: %q", len(frames), out)
	}
	for i, f := range frames {
		if f["modelVersion"] != "public-model" {
			t.Fatalf("frame %d missing public alias modelVersion: %v", i, f)
		}
		if f["responseId"] != cgmID {
			t.Fatalf("frame %d missing preserved response ID: %v", i, f)
		}
		if _, hasUsage := f["usageMetadata"]; hasUsage && i < len(frames)-1 {
			t.Fatalf("non-terminal frame %d must not carry usage: %v", i, f)
		}
	}
	p0 := cgmCandidate(t, frames[0])["content"].(map[string]any)["parts"].([]any)
	if p0[0].(map[string]any)["text"] != "hel" {
		t.Fatalf("first text not immediate: %q", out)
	}
	p1 := cgmCandidate(t, frames[1])["content"].(map[string]any)["parts"].([]any)
	if p1[0].(map[string]any)["text"] != "lo" {
		t.Fatalf("second text not immediate: %q", out)
	}
	last := cgmCandidate(t, frames[2])
	if last["finishReason"] != "STOP" {
		t.Fatalf("terminal finish wrong: %v", last)
	}
	u := frames[2]["usageMetadata"].(map[string]any)
	if u["promptTokenCount"] != float64(9) || u["candidatesTokenCount"] != float64(4) ||
		u["totalTokenCount"] != float64(13) {
		t.Fatalf("terminal usage wrong: %v", u)
	}
	if _, ok := u["thoughtsTokenCount"]; ok {
		t.Fatalf("zero thoughts must be absent, not understated: %v", u)
	}

	aout, rerr := cgmRunArray(t, input)
	if rerr != nil {
		t.Fatalf("array translate: %v", rerr.Msg)
	}
	if !strings.HasPrefix(aout, "[") || !strings.HasSuffix(aout, "]") {
		t.Fatalf("array framing wrong: %q", aout)
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(aout), &arr); err != nil || len(arr) != 3 {
		t.Fatalf("array not incremental-valid: %v %q", err, aout)
	}
	au := arr[2]["usageMetadata"].(map[string]any)
	if au["promptTokenCount"] != float64(9) || au["totalTokenCount"] != float64(13) {
		t.Fatalf("array terminal usage wrong: %v", au)
	}
	if arr[2]["modelVersion"] != "public-model" || arr[2]["responseId"] != cgmID {
		t.Fatalf("array terminal identity wrong: %v", arr[2])
	}
}

// --- Exact args, empty {}, sparse out-of-order tools -------------------------

func TestGeminiEventToolsExactSparseOrdered(t *testing.T) {
	var sb strings.Builder
	// Sparse, out-of-order arrival: logical order is the typed
	// SourceIndex, never arrival order, never reparsed ItemIDs.
	// NOTE (early-release revision): Chat tool blocks close only at
	// [DONE], so all closes arrive together; the encoder releases each
	// complete call promptly as its own frame in logical order before
	// the terminal, instead of batching both calls into the terminal.
	// The terminal therefore carries finish only. Assertions below pin
	// exactly-once identity/order across the whole stream, not
	// incidental batching.
	sb.WriteString(cgmChunk(cgmID, cgmModel,
		`{"role":"assistant","tool_calls":[{"index":5,"id":"call: five","type":"function","function":{"name":"get_time","arguments":"{\"tz\":\"UTC\",\"n\":90071992547"}}]}`, ""))
	sb.WriteString(cgmChunk(cgmID, cgmModel,
		`{"tool_calls":[{"index":5,"function":{"arguments":"40993}"}}]}`, ""))
	sb.WriteString(cgmChunk(cgmID, cgmModel,
		`{"tool_calls":[{"index":2,"id":"call two","type":"function","function":{"name":"get_weather","arguments":""}}]}`, ""))
	sb.WriteString(cgmChunk(cgmID, cgmModel, `{}`, `"tool_calls"`))
	sb.WriteString(cgmUsageChunk(cgmID, cgmModel, cgmUsage(2, 5)))
	sb.WriteString("data: [DONE]\n\n")
	input := sb.String()

	for _, isArray := range []bool{false, true} {
		var out string
		var rerr *RouterError
		if isArray {
			out, rerr = cgmRunArray(t, input)
		} else {
			out, rerr = cgmRunSSE(t, input)
		}
		if rerr != nil {
			t.Fatalf("array=%v translate: %v", isArray, rerr.Msg)
		}
		if !strings.Contains(out, "9007199254740993") {
			t.Fatalf("array=%v args not exact: %q", isArray, out)
		}
		// Collect every functionCall part across the whole stream:
		// each call must appear exactly once, in logical index order,
		// with no terminal duplication.
		var calls []map[string]any
		var terms []map[string]any
		if isArray {
			var arr []map[string]any
			if err := json.Unmarshal([]byte(out), &arr); err != nil {
				t.Fatalf("array=%v not JSON: %v", isArray, err)
			}
			for _, f := range arr {
				c := cgmCandidate(t, f)
				if _, hasFinish := c["finishReason"]; hasFinish {
					terms = append(terms, c)
				}
				for _, p := range c["content"].(map[string]any)["parts"].([]any) {
					if fc, ok := p.(map[string]any)["functionCall"]; ok {
						calls = append(calls, fc.(map[string]any))
					}
				}
			}
		} else {
			frames := cgmFrames(t, out)
			for _, f := range frames {
				c := cgmCandidate(t, f)
				if _, hasFinish := c["finishReason"]; hasFinish {
					terms = append(terms, c)
				}
				for _, p := range c["content"].(map[string]any)["parts"].([]any) {
					if fc, ok := p.(map[string]any)["functionCall"]; ok {
						calls = append(calls, fc.(map[string]any))
					}
				}
			}
		}
		if len(terms) != 1 {
			t.Fatalf("array=%v want exactly one terminal, got %d: %q", isArray, len(terms), out)
		}
		if terms[0]["finishReason"] != "STOP" {
			t.Fatalf("array=%v finish wrong: %v", isArray, terms[0])
		}
		if len(calls) != 2 {
			t.Fatalf("array=%v want 2 calls exactly once across stream, got %v in %q", isArray, calls, out)
		}
		// Logical order: index 2 first, index 5 second.
		if calls[0]["name"] != "get_weather" || calls[0]["id"] != "call two" {
			t.Fatalf("array=%v first call wrong (want index order): %v", isArray, calls[0])
		}
		raw0, _ := json.Marshal(calls[0]["args"])
		if string(raw0) != "{}" {
			t.Fatalf("array=%v empty args not exact {}: %s", isArray, raw0)
		}
		if calls[1]["name"] != "get_time" || calls[1]["id"] != "call: five" {
			t.Fatalf("array=%v opaque call identity lost: %v", isArray, calls[1])
		}
		// Terminal must not repeat released calls.
		if parts := terms[0]["content"].(map[string]any)["parts"].([]any); len(parts) != 0 {
			t.Fatalf("array=%v terminal must not duplicate released calls: %v", isArray, parts)
		}
		// Structural bigint check against the raw wire (plain map decode
		// would blur the literal through float64): the args object must
		// carry the literal exactly.
		if got := cgmWireArgsN(t, out, isArray); got != "9007199254740993" {
			t.Fatalf("array=%v bigint inexact: %q in %q", isArray, got, out)
		}
	}
}

// --- Reasoning/cache accounting through the shared target policy -------------

func TestGeminiEventReasonCacheMapping(t *testing.T) {
	// Chat prompt 12 (incl. cached 4) / completion 7 (incl. reasoning 2)
	// renders as Gemini prompt 12 / candidates 5 / thoughts 2, total 19.
	usage := `{"prompt_tokens":12,"completion_tokens":7,"total_tokens":19,` +
		`"prompt_tokens_details":{"cached_tokens":4},` +
		`"completion_tokens_details":{"reasoning_tokens":2}}`
	input := cgmChunk(cgmID, cgmModel, `{"content":"hi"}`, "") +
		cgmChunk(cgmID, cgmModel, `{}`, `"stop"`) +
		cgmUsageChunk(cgmID, cgmModel, usage) +
		"data: [DONE]\n\n"
	for _, isArray := range []bool{false, true} {
		var out string
		var rerr *RouterError
		if isArray {
			out, rerr = cgmRunArray(t, input)
		} else {
			out, rerr = cgmRunSSE(t, input)
		}
		if rerr != nil {
			t.Fatalf("array=%v translate: %v", isArray, rerr.Msg)
		}
		term := cgmTerminal(t, out, isArray)
		u := term["usageMetadata"].(map[string]any)
		if u["promptTokenCount"] != float64(12) || u["candidatesTokenCount"] != float64(5) ||
			u["thoughtsTokenCount"] != float64(2) || u["totalTokenCount"] != float64(19) ||
			u["cachedContentTokenCount"] != float64(4) {
			t.Fatalf("array=%v usage mapping wrong: %v", isArray, u)
		}
	}
}

// --- Genuine malformed vs size 502 -------------------------------------------

func TestGeminiEventMalformedVsSizeBound(t *testing.T) {
	// Genuine malformed args: native MALFORMED terminal, still success.
	bad := cgmChunk(cgmID, cgmModel,
		`{"tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"f","arguments":"[oops"}}]}`, "") +
		cgmChunk(cgmID, cgmModel, `{}`, `"stop"`) +
		cgmUsageChunk(cgmID, cgmModel, cgmUsage(7, 3)) +
		"data: [DONE]\n\n"
	out, rerr := cgmRunSSE(t, bad)
	if rerr != nil {
		t.Fatalf("malformed must map, not fail: %v", rerr.Msg)
	}
	frames := cgmFrames(t, out)
	c0 := cgmCandidate(t, frames[len(frames)-1])
	if c0["finishReason"] != "MALFORMED_FUNCTION_CALL" {
		t.Fatalf("expected MALFORMED marker: %v", c0)
	}
	if strings.Contains(out, `"functionCall"`) {
		t.Fatalf("malformed call must be suppressed, not rendered: %q", out)
	}
	if strings.Contains(out, `"STOP"`) {
		t.Fatalf("malformed terminal must not carry STOP: %q", out)
	}

	// Oversize args: size 502, never a native malformed call.
	big := strings.Repeat("q", DefaultLimits().MaxArgsChars)
	huge := cgmChunk(cgmID, cgmModel,
		`{"tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"f","arguments":"`+big+`"}}]}`, "")
	if _, rerr := cgmRunSSE(t, huge); rerr == nil || rerr.Status != 502 ||
		strings.Contains(strings.ToLower(rerr.Msg), "malformed") {
		t.Fatalf("oversize args must be size 502, got %v", rerr)
	}
	if out, _ := cgmRunArray(t, huge); strings.HasSuffix(strings.TrimSpace(out), "]") {
		t.Fatalf("failed array must not close: %q", out)
	}
}

// --- Malformed late: text committed, still no STOP ----------------------------

func TestGeminiEventMalformedLateNoStop(t *testing.T) {
	input := cgmChunk(cgmID, cgmModel, `{"content":"hello"}`, "") +
		cgmChunk(cgmID, cgmModel,
			`{"tool_calls":[{"index":0,"id":"call_bad","type":"function","function":{"name":"f","arguments":"not-json{{{"}}]}`, "") +
		cgmChunk(cgmID, cgmModel, `{}`, `"tool_calls"`) +
		cgmUsageChunk(cgmID, cgmModel, cgmUsage(7, 3)) +
		"data: [DONE]\n\n"
	out, rerr := cgmRunSSE(t, input)
	if rerr != nil {
		t.Fatalf("translate: %v", rerr.Msg)
	}
	if strings.Contains(out, `"STOP"`) {
		t.Fatalf("malformed late must preserve no STOP: %q", out)
	}
	frames := cgmFrames(t, out)
	if len(frames) != 2 {
		t.Fatalf("want committed text + MALFORMED terminal, got %d: %q", len(frames), out)
	}
	p0 := cgmCandidate(t, frames[0])["content"].(map[string]any)["parts"].([]any)
	if p0[0].(map[string]any)["text"] != "hello" {
		t.Fatalf("committed peer text lost: %q", out)
	}
	c0 := cgmCandidate(t, frames[1])
	if c0["finishReason"] != "MALFORMED_FUNCTION_CALL" {
		t.Fatalf("expected MALFORMED terminal: %v", c0)
	}

	// Mixed: one valid peer kept alongside the malformed call.
	mixed := cgmChunk(cgmID, cgmModel, `{"content":"hi"}`, "") +
		cgmChunk(cgmID, cgmModel,
			`{"tool_calls":[{"index":0,"id":"good","type":"function","function":{"name":"ok","arguments":"{\"a\":1}"}}]}`, "") +
		cgmChunk(cgmID, cgmModel,
			`{"tool_calls":[{"index":1,"id":"bad","type":"function","function":{"name":"no","arguments":"{{{"}}]}`, "") +
		cgmChunk(cgmID, cgmModel, `{}`, `"tool_calls"`) +
		cgmUsageChunk(cgmID, cgmModel, cgmUsage(7, 3)) +
		"data: [DONE]\n\n"
	mout, rerr := cgmRunSSE(t, mixed)
	if rerr != nil {
		t.Fatalf("mixed translate: %v", rerr.Msg)
	}
	// NOTE (early-release revision): the valid peer releases promptly
	// at its BlockClose, so it appears once in an early frame; the
	// terminal carries MALFORMED finish with no duplicated parts.
	mframes := cgmFrames(t, mout)
	mc0 := cgmCandidate(t, mframes[len(mframes)-1])
	if mc0["finishReason"] != "MALFORMED_FUNCTION_CALL" {
		t.Fatalf("mixed terminal must be MALFORMED: %v", mc0)
	}
	if parts := mc0["content"].(map[string]any)["parts"].([]any); len(parts) != 0 {
		t.Fatalf("mixed terminal must not duplicate the released peer: %v", parts)
	}
	var kept []string
	for _, f := range mframes {
		for _, p := range cgmCandidate(t, f)["content"].(map[string]any)["parts"].([]any) {
			if fc, ok := p.(map[string]any)["functionCall"]; ok {
				kept = append(kept, fc.(map[string]any)["name"].(string))
			}
		}
	}
	if len(kept) != 1 || kept[0] != "ok" {
		t.Fatalf("valid peer must be kept exactly once across stream, got %v in %q", kept, mout)
	}
}

// --- Terminal gates: no success, precommit silence, array unclosed ------------

func TestGeminiEventTerminalGates(t *testing.T) {
	text := cgmChunk(cgmID, cgmModel, `{"content":"hi"}`, "")
	finish := cgmChunk(cgmID, cgmModel, `{}`, `"stop"`)
	for name, input := range map[string]string{
		"no finish":   text + cgmUsageChunk(cgmID, cgmModel, cgmUsage(2, 1)) + "data: [DONE]\n\n",
		"no usage":    text + finish + "data: [DONE]\n\n",
		"no done":     text + finish + cgmUsageChunk(cgmID, cgmModel, cgmUsage(2, 1)),
		"truncated":   text + finish,
		"usage first": cgmUsageChunk(cgmID, cgmModel, cgmUsage(2, 1)) + text + finish + "data: [DONE]\n\n",
		"dup usage":   text + finish + cgmUsageChunk(cgmID, cgmModel, cgmUsage(2, 1)) + cgmUsageChunk(cgmID, cgmModel, cgmUsage(2, 1)) + "data: [DONE]\n\n",
		"dup finish":  text + finish + finish + cgmUsageChunk(cgmID, cgmModel, cgmUsage(2, 1)) + "data: [DONE]\n\n",
		"bad finish":  text + cgmChunk(cgmID, cgmModel, `{}`, `"quantum"`) + cgmUsageChunk(cgmID, cgmModel, cgmUsage(2, 1)) + "data: [DONE]\n\n",
		"after data":  text + finish + cgmUsageChunk(cgmID, cgmModel, cgmUsage(2, 1)) + cgmChunk(cgmID, cgmModel, `{"content":"late"}`, "") + "data: [DONE]\n\n",
	} {
		out, rerr := cgmRunSSE(t, input)
		if rerr == nil || rerr.Status != 502 {
			t.Fatalf("%s: expected 502, got %v (%q)", name, rerr, out)
		}
		aout, _ := cgmRunArray(t, input)
		if strings.HasSuffix(strings.TrimSpace(aout), "]") {
			t.Fatalf("%s: failed array must not close: %q", name, aout)
		}
	}
	// Pre-commit failures write zero bytes.
	precommit := "data: not-json\n\n"
	if out, rerr := cgmRunSSE(t, precommit); rerr == nil || out != "" {
		t.Fatalf("precommit failure must be silent 502, got %v %q", rerr, out)
	}
	if out, rerr := cgmRunArray(t, precommit); rerr == nil || out != "" {
		t.Fatalf("precommit array failure must be silent 502, got %v %q", rerr, out)
	}
}

// --- Unmapped accounting refuses (shared target gate) -------------------------

func TestGeminiEventUnmappedUsageRefuses(t *testing.T) {
	for name, usage := range map[string]string{
		"cache write": `{"prompt_tokens":12,"completion_tokens":7,"total_tokens":19,"prompt_tokens_details":{"cache_write_tokens":1}}`,
		"text share":  `{"prompt_tokens":12,"completion_tokens":7,"total_tokens":19,"prompt_tokens_details":{"text_tokens":3}}`,
	} {
		input := cgmChunk(cgmID, cgmModel, `{"content":"hi"}`, "") +
			cgmChunk(cgmID, cgmModel, `{}`, `"stop"`) +
			cgmUsageChunk(cgmID, cgmModel, usage) +
			"data: [DONE]\n\n"
		if _, rerr := cgmRunSSE(t, input); rerr == nil || rerr.Status != 502 {
			t.Fatalf("%s: expected 502, got %v", name, rerr)
		}
		if out, _ := cgmRunArray(t, input); strings.HasSuffix(strings.TrimSpace(out), "]") {
			t.Fatalf("%s: refused array must not close: %q", name, out)
		}
	}
}

// --- No EOF read past a valid terminal ----------------------------------------

// cgmByteReader serves at most one byte per Read and counts the total
// served. Because framing reads only what it consumes, the served total
// after a valid terminal proves no read past the terminal (buffered
// readers cannot hide read-ahead when every Read yields one byte).
type cgmByteReader struct {
	data   []byte
	served int
}

func (r *cgmByteReader) Read(p []byte) (int, error) {
	if r.served >= len(r.data) {
		return 0, errors.New("read past terminal")
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = r.data[r.served]
	r.served++
	return 1, nil
}

func TestGeminiEventNoEOFReadPastTerminal(t *testing.T) {
	terminal := "data: [DONE]\n\n"
	trailing := cgmChunk("next-gen", cgmModel, `{"content":"next generation"}`, "")
	input := cgmChunk(cgmID, cgmModel, `{"content":"hi"}`, "") +
		cgmChunk(cgmID, cgmModel, `{}`, `"stop"`) +
		cgmUsageChunk(cgmID, cgmModel, cgmUsage(2, 1)) +
		terminal + trailing
	want := len(input) - len(trailing)
	for _, isArray := range []bool{false, true} {
		br := &cgmByteReader{data: []byte(input)}
		var out bytes.Buffer
		var rerr *RouterError
		if isArray {
			tr := &ChatToGeminiJSONArrayTranslator{PublicModel: "p", Limits: DefaultLimits()}
			rerr = tr.Translate(br, &out, nil)
		} else {
			tr := &ChatToGeminiSSETranslator{PublicModel: "p", Limits: DefaultLimits()}
			rerr = tr.Translate(br, &out, nil)
		}
		if rerr != nil {
			t.Fatalf("array=%v translate: %v", isArray, rerr.Msg)
		}
		if strings.Contains(out.String(), "next generation") {
			t.Fatalf("array=%v translator read past [DONE]: %q", isArray, out.String())
		}
		if br.served != want {
			t.Fatalf("array=%v served %d bytes, want exactly through [DONE] (%d)", isArray, br.served, want)
		}
	}
}

// --- Downstream bound prewrite -------------------------------------------------

func TestGeminiEventDownstreamBound(t *testing.T) {
	// NOTE (early-release revision): tools release as individual frames,
	// so eight small calls no longer batch into one oversize terminal.
	// The per-frame bound is pinned with a single call whose early frame
	// alone exceeds MaxFrameBytes.
	lim := DefaultLimits()
	lim.MaxFrameBytes = 512
	bigArgs := `{"k":"` + strings.Repeat("v", 600) + `"}`
	escaped, _ := json.Marshal(bigArgs)
	_ = escaped
	// Build the chunk with the raw args string embedded as a JSON string.
	frag := strings.ReplaceAll(bigArgs, `"`, `\"`)
	input := cgmChunk(cgmID, cgmModel,
		`{"tool_calls":[{"index":0,"id":"call_0","type":"function","function":{"name":"fn_0","arguments":"`+frag+`"}}]}`, "") +
		cgmChunk(cgmID, cgmModel, `{}`, `"tool_calls"`) +
		cgmUsageChunk(cgmID, cgmModel, cgmUsage(2, 5)) +
		"data: [DONE]\n\n"
	var buf bytes.Buffer
	tr := &ChatToGeminiSSETranslator{PublicModel: "p", Limits: lim}
	if rerr := tr.Translate(strings.NewReader(input), &buf, nil); rerr == nil || rerr.Status != 502 {
		t.Fatalf("expected downstream bound 502, got %v", rerr)
	}
	var buf2 bytes.Buffer
	atr := &ChatToGeminiJSONArrayTranslator{PublicModel: "p", Limits: lim}
	if rerr := atr.Translate(strings.NewReader(input), &buf2, nil); rerr == nil || rerr.Status != 502 {
		t.Fatalf("expected array bound 502, got %v", rerr)
	}
	if strings.HasSuffix(strings.TrimSpace(buf2.String()), "]") {
		t.Fatalf("failed array must not close: %q", buf2.String())
	}
}

// --- Write failure: sticky, no secondary write, reads stop ---------------------

type cgmFailWriter struct {
	writes int
}

func (w *cgmFailWriter) Write(p []byte) (int, error) {
	w.writes++
	return 0, errors.New("downstream closed")
}

func TestGeminiEventWriteFailureNoSecondary(t *testing.T) {
	input := cgmChunk(cgmID, cgmModel, `{"content":"hi"}`, "") +
		cgmChunk(cgmID, cgmModel, `{}`, `"stop"`) +
		cgmUsageChunk(cgmID, cgmModel, cgmUsage(2, 1)) +
		"data: [DONE]\n\n" +
		cgmChunk(cgmID, cgmModel, `{"content":"trailing"}`, "")
	for _, isArray := range []bool{false, true} {
		fw := &cgmFailWriter{}
		br := &cgmByteReader{data: []byte(input)}
		var rerr *RouterError
		if isArray {
			tr := &ChatToGeminiJSONArrayTranslator{PublicModel: "p", Limits: DefaultLimits()}
			rerr = tr.Translate(br, fw, nil)
		} else {
			tr := &ChatToGeminiSSETranslator{PublicModel: "p", Limits: DefaultLimits()}
			rerr = tr.Translate(br, fw, nil)
		}
		if rerr == nil || rerr.Status != 502 {
			t.Fatalf("array=%v expected write failure 502, got %v", isArray, rerr)
		}
		// One failed write only: the encoder's Abort must not emit a
		// second error frame, and the pipeline must stop reading.
		if fw.writes != 1 {
			t.Fatalf("array=%v want exactly 1 downstream write, got %d", isArray, fw.writes)
		}
		if br.served >= len(input) {
			t.Fatalf("array=%v upstream must not be drained after write failure", isArray)
		}
	}
}

// --- Encoder Abort emits nothing (server owns the error frame) -----------------

func TestGeminiEventAbortEmitsNothing(t *testing.T) {
	for _, isArray := range []bool{false, true} {
		var buf bytes.Buffer
		enc := NewGeminiEventEncoder("p", DefaultLimits(), !isArray)
		if rerr := TranslateStream(
			NewSSEFrameReader(strings.NewReader(
				cgmChunk(cgmID, cgmModel, `{"content":"hi"}`, "")+
					cgmChunk(cgmID, cgmModel, `{}`, `"stop"`)+
					cgmUsageChunk(cgmID, cgmModel, cgmUsage(2, 1))+
					"data: [DONE]\n\n"), DefaultLimits()),
			NewChatStreamDecoder(DefaultLimits()),
			enc, &buf, nil, DefaultLimits(), DialectChat); rerr != nil {
			t.Fatalf("array=%v setup translate: %v", isArray, rerr)
		}
		committed := enc.Committed()
		before := buf.Len()
		enc.Abort(&buf, nil)
		if buf.Len() != before {
			t.Fatalf("array=%v Abort must not emit (server owns the error frame)", isArray)
		}
		if enc.Committed() != committed {
			t.Fatalf("array=%v Abort must not change commitment", isArray)
		}
	}
}

// --- Translator reuse: fresh encoder state per Translate ------------------------

func TestGeminiEventTranslatorReuse(t *testing.T) {
	input := cgmChunk(cgmID, cgmModel, `{"content":"hi"}`, "") +
		cgmChunk(cgmID, cgmModel, `{}`, `"stop"`) +
		cgmUsageChunk(cgmID, cgmModel, cgmUsage(2, 1)) +
		"data: [DONE]\n\n"
	str := &ChatToGeminiSSETranslator{PublicModel: "p", Limits: DefaultLimits()}
	var first bytes.Buffer
	if rerr := str.Translate(strings.NewReader(input), &first, nil); rerr != nil {
		t.Fatalf("first translate: %v", rerr.Msg)
	}
	var second bytes.Buffer
	if rerr := str.Translate(strings.NewReader(input), &second, nil); rerr != nil {
		t.Fatalf("second translate must start fresh: %v", rerr.Msg)
	}
	if first.String() != second.String() {
		t.Fatalf("reuse mismatch:\n%s\n%s", first.String(), second.String())
	}
	atr := &ChatToGeminiJSONArrayTranslator{PublicModel: "p", Limits: DefaultLimits()}
	var afirst, asecond bytes.Buffer
	if rerr := atr.Translate(strings.NewReader(input), &afirst, nil); rerr != nil {
		t.Fatalf("first array translate: %v", rerr.Msg)
	}
	if rerr := atr.Translate(strings.NewReader(input), &asecond, nil); rerr != nil {
		t.Fatalf("second array translate must start fresh: %v", rerr.Msg)
	}
	if afirst.String() != asecond.String() {
		t.Fatalf("array reuse mismatch:\n%s\n%s", afirst.String(), asecond.String())
	}
}

// --- First text flushes before the terminal is read ------------------------------

func TestGeminiEventFirstTextBeforeTerminal(t *testing.T) {
	input := cgmChunk(cgmID, cgmModel, `{"content":"early"}`, "") +
		cgmChunk(cgmID, cgmModel,
			`{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}`, "") +
		cgmChunk(cgmID, cgmModel, `{}`, `"tool_calls"`) +
		cgmUsageChunk(cgmID, cgmModel, cgmUsage(2, 5)) +
		"data: [DONE]\n\n"
	out, rerr := cgmRunSSE(t, input)
	if rerr != nil {
		t.Fatalf("translate: %v", rerr.Msg)
	}
	early, term := strings.Index(out, "early"), strings.Index(out, `"finishReason"`)
	if early < 0 || term < 0 || early > term {
		t.Fatalf("first text must flush before the terminal: %q", out)
	}
	// NOTE (early-release revision): the tool releases at its BlockClose
	// (all Chat closes arrive at [DONE]) as its own frame, so the stream
	// is text + tool + terminal with the call exactly once and no
	// terminal duplication.
	frames := cgmFrames(t, out)
	if len(frames) != 3 {
		t.Fatalf("want text + tool + terminal frames, got %d: %q", len(frames), out)
	}
	var toolCount int
	for _, f := range frames {
		for _, p := range cgmCandidate(t, f)["content"].(map[string]any)["parts"].([]any) {
			if _, ok := p.(map[string]any)["functionCall"]; ok {
				toolCount++
			}
		}
	}
	if toolCount != 1 {
		t.Fatalf("tool call must appear exactly once, got %d in %q", toolCount, out)
	}
	if parts := cgmCandidate(t, frames[2])["content"].(map[string]any)["parts"].([]any); len(parts) != 0 {
		t.Fatalf("terminal must not duplicate the released call: %q", out)
	}
	aout, rerr := cgmRunArray(t, input)
	if rerr != nil {
		t.Fatalf("array translate: %v", rerr.Msg)
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(aout), &arr); err != nil || len(arr) != 3 {
		t.Fatalf("want text + tool + terminal elements: %v %q", err, aout)
	}
	tp := arr[0]["candidates"].([]any)[0].(map[string]any)["content"].(map[string]any)["parts"].([]any)
	if tp[0].(map[string]any)["text"] != "early" {
		t.Fatalf("array first element must be the text delta: %q", aout)
	}
}

// --- Empty generation still closes with finish + usage ----------------------------

func TestGeminiEventEmptyGeneration(t *testing.T) {
	input := cgmChunk(cgmID, cgmModel, `{}`, `"stop"`) +
		cgmUsageChunk(cgmID, cgmModel, cgmUsage(2, 1)) +
		"data: [DONE]\n\n"
	out, rerr := cgmRunSSE(t, input)
	if rerr != nil {
		t.Fatalf("empty stream: %v", rerr.Msg)
	}
	frames := cgmFrames(t, out)
	c0 := cgmCandidate(t, frames[len(frames)-1])
	if c0["finishReason"] != "STOP" {
		t.Fatalf("empty terminal wrong: %v", c0)
	}
	parts := c0["content"].(map[string]any)["parts"].([]any)
	if len(parts) != 0 {
		t.Fatalf("empty terminal must carry empty parts, not null: %v", c0)
	}
}
