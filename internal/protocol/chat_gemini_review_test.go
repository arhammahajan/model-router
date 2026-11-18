// Gemini -> Chat review regressions: independent red-before-green coverage
// for the root-reviewed findings on the ingress-to-Gemini pure codecs.
//
// Every test below names the finding it guards; all must fail on the
// pre-fix code and pass after the fix, without changing any shared
// helper (g2cProviderChunk is preserved verbatim) or any other worker's
// files.
package protocol

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestGeminiToChatUsageHasDedicatedTerminalChunk(t *testing.T) {
	var out bytes.Buffer
	tr := &GeminiToChatSSETranslator{PublicModel: "m", IncludeUsage: true, Limits: DefaultLimits()}
	if rerr := tr.Translate(g2cSSE(g2cProviderChunk("hi", "STOP", 7, 3, "")), &out, nil); rerr != nil {
		t.Fatal(rerr)
	}
	frames := strings.Split(strings.TrimSpace(out.String()), "\n\n")
	usageChunks := 0
	for i, frame := range frames {
		if frame == "data: [DONE]" {
			continue
		}
		var chunk map[string]json.RawMessage
		if err := json.Unmarshal([]byte(strings.TrimPrefix(frame, "data: ")), &chunk); err != nil {
			t.Fatal(err)
		}
		usage, present := chunk["usage"]
		if !present {
			t.Fatal("include_usage requires null usage on ordinary chunks")
		}
		if !isNull(usage) {
			usageChunks++
			if string(chunk["choices"]) != "[]" || i != len(frames)-2 {
				t.Fatal("usage must be a separate empty-choices chunk immediately before DONE")
			}
		}
	}
	if usageChunks != 1 {
		t.Fatalf("usage chunks = %d, want 1", usageChunks)
	}
}

// g2cRegrNoUsageChunk builds a terminal STOP chunk that carries extra text
// but deliberately omits usageMetadata.
func g2cRegrNoUsageChunk(text string) string {
	q, _ := json.Marshal(text)
	return `{"candidates":[{"content":{"role":"model","parts":[{"text":` + string(q) +
		`}]},"index":0,"finishReason":"STOP"}],` +
		`"modelVersion":"gemini-2.5-flash","responseId":"resp-constant"}`
}

// g2cRegrBlockedNoUsage builds a candidacy-less blocked-prompt chunk that
// deliberately omits usageMetadata.
func g2cRegrBlockedNoUsage() string {
	return `{"promptFeedback":{"blockReason":"SAFETY"},` +
		`"modelVersion":"gemini-2.5-flash","responseId":"resp-constant"}`
}

// Finding 2: terminal usage must be authoritative. A final STOP chunk with
// extra text but no usageMetadata must fail (502, no DONE) instead of
// succeeding with stale intermediate counts. Covers SSE and JSON-array
// framings with IncludeUsage true and false.
func TestG2CRegrTerminalUsageAuthoritative(t *testing.T) {
	for _, includeUsage := range []bool{true, false} {
		first := g2cProviderChunk("a", "", 7, 1, "")
		last := g2cRegrNoUsageChunk("late")
		tr := &GeminiToChatSSETranslator{PublicModel: "m", IncludeUsage: includeUsage, Limits: DefaultLimits()}
		var out bytes.Buffer
		rerr := tr.Translate(g2cSSE(first, last), &out, nil)
		if rerr == nil || rerr.Status != 502 {
			t.Fatalf("SSE includeUsage=%v: terminal without usage must be 502, got %v", includeUsage, rerr)
		}
		if bytes.Contains(out.Bytes(), []byte("[DONE]")) {
			t.Fatalf("SSE includeUsage=%v: failed terminal must not emit [DONE]", includeUsage)
		}
		tr2 := &GeminiToChatJSONArrayTranslator{PublicModel: "m", IncludeUsage: includeUsage, Limits: DefaultLimits()}
		var out2 bytes.Buffer
		rerr = tr2.Translate(g2cArray(first, last), &out2, nil)
		if rerr == nil || rerr.Status != 502 {
			t.Fatalf("array includeUsage=%v: terminal without usage must be 502, got %v", includeUsage, rerr)
		}
		if bytes.Contains(out2.Bytes(), []byte("[DONE]")) {
			t.Fatalf("array includeUsage=%v: failed terminal must not emit [DONE]", includeUsage)
		}
	}
}

// Finding 2, blocked-prompt variant: a blocked chunk without its own usage
// must fail even when an earlier chunk carried usage.
func TestG2CRegrBlockedUsageAuthoritative(t *testing.T) {
	for _, includeUsage := range []bool{true, false} {
		first := g2cProviderChunk("a", "", 7, 1, "")
		blocked := g2cRegrBlockedNoUsage()
		tr := &GeminiToChatSSETranslator{PublicModel: "m", IncludeUsage: includeUsage, Limits: DefaultLimits()}
		var out bytes.Buffer
		rerr := tr.Translate(g2cSSE(first, blocked), &out, nil)
		if rerr == nil || rerr.Status != 502 {
			t.Fatalf("SSE blocked includeUsage=%v: terminal without usage must be 502, got %v", includeUsage, rerr)
		}
		if bytes.Contains(out.Bytes(), []byte("[DONE]")) {
			t.Fatalf("SSE blocked includeUsage=%v: failed terminal must not emit [DONE]", includeUsage)
		}
		tr2 := &GeminiToChatJSONArrayTranslator{PublicModel: "m", IncludeUsage: includeUsage, Limits: DefaultLimits()}
		var out2 bytes.Buffer
		rerr = tr2.Translate(g2cArray(first, blocked), &out2, nil)
		if rerr == nil || rerr.Status != 502 {
			t.Fatalf("array blocked includeUsage=%v: terminal without usage must be 502, got %v", includeUsage, rerr)
		}
		if bytes.Contains(out2.Bytes(), []byte("[DONE]")) {
			t.Fatalf("array blocked includeUsage=%v: failed terminal must not emit [DONE]", includeUsage)
		}
	}
}

// Finding 3: Part.data is a oneof union. A single part carrying both text
// and functionCall must be rejected in the complete-JSON and stream paths.
func TestG2CRegrUnionPartRejected(t *testing.T) {
	part := `{"text":"both","functionCall":{"name":"f","args":{}}}`
	body := `{"candidates":[{"content":{"role":"model","parts":[` + part +
		`]},"finishReason":"STOP"}],"usageMetadata":` + g2cUsage(5, 2) + `}`
	if _, rerr := TranslateGeminiJSONToChat([]byte(body), "m", DefaultLimits()); rerr == nil || rerr.Status != 502 {
		t.Fatalf("complete union part must be 502, got %v", rerr)
	}
	chunk := `{"candidates":[{"content":{"role":"model","parts":[` + part +
		`]},"index":0,"finishReason":"STOP"}],"usageMetadata":` + g2cUsage(5, 2) +
		`,"modelVersion":"gemini-2.5-flash","responseId":"resp-constant"}`
	tr := &GeminiToChatSSETranslator{PublicModel: "m", IncludeUsage: false, Limits: DefaultLimits()}
	var out bytes.Buffer
	if rerr := tr.Translate(g2cSSE(chunk), &out, nil); rerr == nil || rerr.Status != 502 {
		t.Fatalf("SSE union part must be 502, got %v", rerr)
	}
	tr2 := &GeminiToChatJSONArrayTranslator{PublicModel: "m", IncludeUsage: false, Limits: DefaultLimits()}
	var out2 bytes.Buffer
	if rerr := tr2.Translate(g2cArray(chunk), &out2, nil); rerr == nil || rerr.Status != 502 {
		t.Fatalf("array union part must be 502, got %v", rerr)
	}
}

// Finding 4: a complete functionCall with args:{} must reassemble to the
// valid object "{}", emitted exactly once (never the empty string).
func TestG2CRegrEmptyArgsEmittedOnce(t *testing.T) {
	fc := `{"functionCall":{"name":"f","args":{},"id":"call_1"}}`
	mk := func() (string, string) {
		return g2cProviderChunk("Hi", "", 9, 1, ""),
			g2cProviderChunk("", "STOP", 9, 5, fc)
	}
	a, b := mk()
	tr := &GeminiToChatSSETranslator{PublicModel: "m", IncludeUsage: false, Limits: DefaultLimits()}
	var out bytes.Buffer
	if rerr := tr.Translate(g2cSSE(a, b), &out, nil); rerr != nil {
		t.Fatalf("SSE: %v", rerr)
	}
	if got := g2cCollect(t, out.Bytes()).calls[0]["args"]; got != `{}` {
		t.Fatalf("SSE empty args must reassemble to {}, got %q", got)
	}
	a, b = mk()
	tr2 := &GeminiToChatJSONArrayTranslator{PublicModel: "m", IncludeUsage: false, Limits: DefaultLimits()}
	var out2 bytes.Buffer
	if rerr := tr2.Translate(g2cArray(a, b), &out2, nil); rerr != nil {
		t.Fatalf("array: %v", rerr)
	}
	if got := g2cCollect(t, out2.Bytes()).calls[0]["args"]; got != `{}` {
		t.Fatalf("array empty args must reassemble to {}, got %q", got)
	}
}

// Finding 1: a trailing comma before the closing bracket is invalid JSON
// and must fail, never complete with [DONE].
func TestG2CRegrArrayTrailingCommaFails(t *testing.T) {
	elem := g2cProviderChunk("", "STOP", 7, 2, "")
	in := strings.NewReader("[" + elem + ",]")
	tr := &GeminiToChatJSONArrayTranslator{PublicModel: "m", IncludeUsage: true, Limits: DefaultLimits()}
	var out bytes.Buffer
	rerr := tr.Translate(in, &out, nil)
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("trailing comma must be 502, got %v", rerr)
	}
	if bytes.Contains(out.Bytes(), []byte("[DONE]")) {
		t.Fatalf("trailing comma must not emit [DONE]")
	}
}

// Finding 1, inclusive element bound: an element of exactly MaxFrameBytes
// succeeds; one byte over fails without [DONE].
func TestG2CRegrArrayElementBoundInclusive(t *testing.T) {
	elem := g2cProviderChunk("", "STOP", 7, 2, "")
	lim := DefaultLimits()
	lim.MaxFrameBytes = len(elem)
	tr := &GeminiToChatJSONArrayTranslator{PublicModel: "m", IncludeUsage: true, Limits: lim}
	var out bytes.Buffer
	if rerr := tr.Translate(strings.NewReader("["+elem+"]"), &out, nil); rerr != nil {
		t.Fatalf("element at exactly MaxFrameBytes must succeed, got %v", rerr)
	}
	lim2 := DefaultLimits()
	lim2.MaxFrameBytes = len(elem) - 1
	tr2 := &GeminiToChatJSONArrayTranslator{PublicModel: "m", IncludeUsage: true, Limits: lim2}
	var out2 bytes.Buffer
	rerr := tr2.Translate(strings.NewReader("["+elem+"]"), &out2, nil)
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("element one byte over MaxFrameBytes must be 502, got %v", rerr)
	}
	if bytes.Contains(out2.Bytes(), []byte("[DONE]")) {
		t.Fatalf("oversize element must not emit [DONE]")
	}
}

// Finding 5: the JSON-array framing must propagate downstream write
// failures as errors without [DONE] (SSE already covers this).
func TestG2CRegrArrayWriterFailure(t *testing.T) {
	elem := g2cProviderChunk("a", "", 3, 1, "")
	tr := &GeminiToChatJSONArrayTranslator{PublicModel: "m", IncludeUsage: false, Limits: DefaultLimits()}
	if rerr := tr.Translate(strings.NewReader("["+elem+"]"), g2cFailWriter{}, nil); rerr == nil {
		t.Fatalf("array write failure must error")
	}
}

// Finding 6: opaque call IDs must be preserved exactly, never rejected or
// remapped for tool-name grammar. Covers Chat ingress and both Gemini
// response paths, plus pre-reservation of supplied ids in complete JSON.
func TestG2CRegrOpaqueCallIDsPreserved(t *testing.T) {
	odd := "has:colon and space"
	raw := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"` + odd +
		`","type":"function","function":{"name":"f","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"` + odd + `","content":"ok"}]}`)
	if _, _, rerr := DecodeChatForGemini(raw, DefaultLimits()); rerr != nil {
		t.Fatalf("Chat ingress must preserve opaque id, got %v", rerr)
	}
	long := strings.Repeat("x", 100)
	rawLong := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"` + long +
		`","type":"function","function":{"name":"f","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"` + long + `","content":"ok"}]}`)
	if _, _, rerr := DecodeChatForGemini(rawLong, DefaultLimits()); rerr != nil {
		t.Fatalf("Chat ingress must preserve 100-char id, got %v", rerr)
	}
	upstream := `{"candidates":[{"content":{"role":"model","parts":[` +
		`{"functionCall":{"name":"f","args":{},"id":"` + odd + `"}}]},"finishReason":"STOP"}],"usageMetadata":` +
		g2cUsage(5, 2) + `}`
	v := g2cTranslate(t, upstream)
	calls := v["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)
	if calls[0].(map[string]any)["id"] != odd {
		t.Fatalf("complete must preserve opaque id, got %v", calls[0])
	}
	chunk := `{"candidates":[{"content":{"role":"model","parts":[` +
		`{"functionCall":{"name":"f","args":{},"id":"` + odd + `"}}]},"index":0,"finishReason":"STOP"}],"usageMetadata":` +
		g2cUsage(5, 2) + `,"modelVersion":"gemini-2.5-flash","responseId":"resp-constant"}`
	tr := &GeminiToChatSSETranslator{PublicModel: "m", IncludeUsage: false, Limits: DefaultLimits()}
	var out bytes.Buffer
	if rerr := tr.Translate(g2cSSE(chunk), &out, nil); rerr != nil {
		t.Fatalf("SSE must preserve opaque id, got %v", rerr)
	}
	if got := g2cCollect(t, out.Bytes()).calls[0]["id"]; got != odd {
		t.Fatalf("SSE opaque id = %q, want %q", got, odd)
	}
}

// Finding 6, reservation: an unidentified call must not mint an id that a
// later supplied id uses; complete JSON reserves all supplied ids first.
func TestG2CRegrCompatIDReservation(t *testing.T) {
	v := g2cTranslate(t, `{"candidates":[{"content":{"role":"model","parts":[
		{"functionCall":{"name":"f","args":{}}},
		{"functionCall":{"name":"f","args":{},"id":"g2c-call-0"}}]},"finishReason":"STOP"}],
		"usageMetadata":`+g2cUsage(5, 3)+`}`)
	calls := v["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)
	id0 := calls[0].(map[string]any)["id"].(string)
	id1 := calls[1].(map[string]any)["id"].(string)
	if id0 == id1 {
		t.Fatalf("minted id must avoid later supplied id: %v", calls)
	}
	if id1 != "g2c-call-0" {
		t.Fatalf("supplied id must be preserved, got %q", id1)
	}
}
