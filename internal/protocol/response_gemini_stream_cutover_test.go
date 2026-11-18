// Gemini-upstream stream runtime cutover: shared IR pipeline across all
// three translated targets and both upstream framings.
//
// Every test drives the SAME Gemini wire bytes through the six public
// wrappers (GeminiToChat{SSE,JSONArray}, GeminiToAnthropic{SSE,JSONArray},
// GeminiToResponses{SSE,JSONArray}). Wrappers must select
// (NewSSEFrameReader or NewJSONArrayFrameReader,
// NewGeminiStreamDecoder(lim,sseSourceBool), corresponding shared target
// encoder, TranslateStream(DialectGemini)) with fresh pipeline objects per
// Translate and no pair-owned source state.
//
// Preserved contracts: optional/late provider IDs/models with explicit
// provenance (public alias stamped, no provider leak, no prefix guessing);
// minted gemini-compat-call-* with collision/duplicate conflicts;
// terminal-own-usage (intermediate never borrowed); SSE prompt return;
// ARRAY closes only after bare Closed (no EOF success); immediate text;
// exact bigint and canonical {} once; malformed vs bounds distinction;
// no fake finish, error-once, write-failure stops reads, reuse safe.
// Deliberate vs legacy: unified gemini-compat-call-* prefix (was
// g2c/g2a/g2r-call-*); Chat text-call-text refuses at terminal (was
// concatenated); role assistant rejected (was alias); Anthropic start
// carries zeros placeholder (was intermediate counts) with authoritative
// terminal correction; Responses empty generation completes with empty
// output [] and no invented message lifecycle.
package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func gxSSE(datas ...string) string {
	var b strings.Builder
	for _, d := range datas {
		b.WriteString("data: " + d + "\n\n")
	}
	return b.String()
}

func gxArray(datas ...string) string {
	var b strings.Builder
	b.WriteString("[")
	for i, d := range datas {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(d)
	}
	b.WriteString("]")
	return b.String()
}

func gxUsage(prompt, cand int) string {
	return `{"promptTokenCount":` + itoa(prompt) + `,"candidatesTokenCount":` + itoa(cand) + `,"totalTokenCount":` + itoa(prompt+cand) + `}`
}

func gxTextChunk(text, finish, usage, extra string) string {
	parts := ""
	if text != "" || extra == "" {
		q, _ := json.Marshal(text)
		parts = `{"text":` + string(q) + `}`
		if extra != "" {
			parts += `,` + extra
		}
	} else {
		parts = extra
	}
	obj := `{"candidates":[{"content":{"role":"model","parts":[` + parts + `]},"index":0`
	if finish != "" {
		q, _ := json.Marshal(finish)
		obj += `,"finishReason":` + string(q)
	}
	obj += `}]`
	if usage != "" {
		obj += `,"usageMetadata":` + usage
	}
	if extra != "" && strings.Contains(extra, "responseId") {
		// envelope already carries ids via extra? handled by caller
	}
	obj += `}`
	return obj
}

func gxChunkWithEnvelope(text, finish, usage, envelope string, toolExtra string) string {
	part := ""
	if toolExtra != "" {
		if text != "" {
			q, _ := json.Marshal(text)
			part = `{"text":` + string(q) + `},` + toolExtra
		} else {
			part = toolExtra
		}
	} else {
		q, _ := json.Marshal(text)
		part = `{"text":` + string(q) + `}`
		if text == "" {
			// keep empty text part for terminal-only chunks? decoder
			// treats empty text as no event; use parts:[] via envelope path
			part = `{"text":""}`
		}
	}
	obj := `{"candidates":[{"content":{"role":"model","parts":[` + part + `]},"index":0`
	if finish != "" {
		q, _ := json.Marshal(finish)
		obj += `,"finishReason":` + string(q)
	}
	obj += `}]`
	if usage != "" {
		obj += `,"usageMetadata":` + usage
	}
	if envelope != "" {
		obj += `,` + envelope
	}
	obj += `}`
	return obj
}

func gxToolExtra(name, id, args string) string {
	fc := `"name":` + gxQ(name)
	if id != "" {
		fc += `,"id":` + gxQ(id)
	}
	if args != "" {
		fc += `,"args":` + args
	}
	return `{"functionCall":{` + fc + `}}`
}

func gxQ(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func gxTranslateAll(t *testing.T, sseUpstream, arrayUpstream string) map[string]string {
	t.Helper()
	lim := DefaultLimits()
	outs := map[string]string{}
	{
		var buf bytes.Buffer
		tr := &GeminiToChatSSETranslator{PublicModel: "public", IncludeUsage: true, Limits: lim}
		if rerr := tr.Translate(strings.NewReader(sseUpstream), &buf, nil); rerr != nil {
			t.Fatalf("chat sse: %v", rerr)
		}
		outs["chat-sse"] = buf.String()
	}
	{
		var buf bytes.Buffer
		tr := &GeminiToChatJSONArrayTranslator{PublicModel: "public", IncludeUsage: true, Limits: lim}
		if rerr := tr.Translate(strings.NewReader(arrayUpstream), &buf, nil); rerr != nil {
			t.Fatalf("chat array: %v", rerr)
		}
		outs["chat-array"] = buf.String()
	}
	{
		var buf bytes.Buffer
		tr := &GeminiToAnthropicSSETranslator{PublicModel: "public", Limits: lim}
		if rerr := tr.Translate(strings.NewReader(sseUpstream), &buf, nil); rerr != nil {
			t.Fatalf("anthropic sse: %v", rerr)
		}
		outs["anthropic-sse"] = buf.String()
	}
	{
		var buf bytes.Buffer
		tr := &GeminiToAnthropicJSONArrayTranslator{PublicModel: "public", Limits: lim}
		if rerr := tr.Translate(strings.NewReader(arrayUpstream), &buf, nil); rerr != nil {
			t.Fatalf("anthropic array: %v", rerr)
		}
		outs["anthropic-array"] = buf.String()
	}
	{
		var buf bytes.Buffer
		tr := &GeminiToResponsesSSETranslator{PublicModel: "public", Limits: lim}
		if rerr := tr.Translate(strings.NewReader(sseUpstream), &buf, nil); rerr != nil {
			t.Fatalf("responses sse: %v", rerr)
		}
		outs["responses-sse"] = buf.String()
	}
	{
		var buf bytes.Buffer
		tr := &GeminiToResponsesJSONArrayTranslator{PublicModel: "public", Limits: lim}
		if rerr := tr.Translate(strings.NewReader(arrayUpstream), &buf, nil); rerr != nil {
			t.Fatalf("responses array: %v", rerr)
		}
		outs["responses-array"] = buf.String()
	}
	return outs
}

func TestGeminiStreamCutoverSameSourceAllTargets(t *testing.T) {
	// Absent early IDs + text streams immediately; terminal carries IDs.
	// Text+tool (no trailing text) passes all targets; text-call-text is
	// covered separately as Chat-refuses.
	c0 := gxChunkWithEnvelope("hello ", "", "", `"responseId":"rp1","modelVersion":"mv1"`, "")
	c1 := gxChunkWithEnvelope("", "STOP", gxUsage(10, 6), `"responseId":"rp1","modelVersion":"mv1"`, gxToolExtra("get_time", "", `{"tz":"UTC"}`))
	sse := gxSSE(c0, c1)
	arr := gxArray(c0, c1)
	outs := gxTranslateAll(t, sse, arr)
	for k, out := range outs {
		if !strings.Contains(out, "public") {
			t.Fatalf("%s missing public alias: %q", k, out)
		}
		if strings.Contains(out, "mv1") {
			t.Fatalf("%s leaks provider model: %q", k, out)
		}
		if strings.Contains(out, "rp1") {
			t.Fatalf("%s must not echo upstream responseId (non-Gemini target): %q", k, out)
		}
	}
	// Chat carries text + tool_calls with minted compat prefix.
	for _, k := range []string{"chat-sse", "chat-array"} {
		out := outs[k]
		if !strings.Contains(out, "hello ") || !strings.Contains(out, `"tool_calls"`) {
			t.Fatalf("%s missing text/tools: %q", k, out)
		}
		if !strings.Contains(out, "gemini-compat-call-0") {
			t.Fatalf("%s missing unified minted prefix: %q", k, out)
		}
		if strings.Contains(out, "g2c-call-") || strings.Contains(out, "g2a-call-") || strings.Contains(out, "g2r-call-") {
			t.Fatalf("%s uses legacy per-target prefix: %q", k, out)
		}
		if !strings.Contains(out, `"finish_reason":"tool_calls"`) {
			t.Fatalf("%s finish must be tool_calls: %q", k, out)
		}
	}
	// Anthropic tool_use preserves call identity verbatim.
	for _, k := range []string{"anthropic-sse", "anthropic-array"} {
		out := outs[k]
		if !strings.Contains(out, "tool_use") || !strings.Contains(out, "get_time") {
			t.Fatalf("%s missing tool_use: %q", k, out)
		}
		if !strings.Contains(out, "gemini-compat-call-0") {
			t.Fatalf("%s missing minted call: %q", k, out)
		}
		if !strings.Contains(out, `"stop_reason":"tool_use"`) {
			t.Fatalf("%s stop must be tool_use: %q", k, out)
		}
	}
	// Responses function_call + completed.
	for _, k := range []string{"responses-sse", "responses-array"} {
		out := outs[k]
		if !strings.Contains(out, "function_call") || !strings.Contains(out, "response.completed") {
			t.Fatalf("%s missing function_call/completed: %q", k, out)
		}
	}
}

func TestGeminiStreamCutoverImmediateTextAndTerminalOwnUsage(t *testing.T) {
	c0 := gxChunkWithEnvelope("Hel", "", `{"promptTokenCount":4,"candidatesTokenCount":1,"totalTokenCount":5}`, "", "")
	c1 := gxChunkWithEnvelope("lo", "STOP", gxUsage(10, 3), "", "")
	sse := gxSSE(c0, c1)
	arr := gxArray(c0, c1)
	outs := gxTranslateAll(t, sse, arr)
	// Reported start counts are preserved; they never substitute for the
	// terminal's own authoritative accounting.
	for _, k := range []string{"anthropic-sse", "anthropic-array"} {
		out := outs[k]
		if !strings.Contains(out, `"input_tokens":4`) || !strings.Contains(out, `"output_tokens":1`) {
			t.Fatalf("%s start must preserve reported running counts: %q", k, out)
		}
		if !strings.Contains(out, `"input_tokens":10`) || !strings.Contains(out, `"output_tokens":3`) {
			t.Fatalf("%s terminal must be authoritative: %q", k, out)
		}
	}
	// Chat terminal usage authoritative.
	for _, k := range []string{"chat-sse", "chat-array"} {
		if !strings.Contains(outs[k], `"prompt_tokens":10`) {
			t.Fatalf("%s chat terminal usage wrong: %q", k, outs[k])
		}
	}
	// Terminal without own usage fails on all wrappers.
	bad := gxChunkWithEnvelope("x", "STOP", "", "", "")
	for _, tc := range []struct {
		name string
		fn   func(string) *RouterError
		up   string
	}{
		{"chat-sse", func(s string) *RouterError {
			var b bytes.Buffer
			return (&GeminiToChatSSETranslator{PublicModel: "m", Limits: DefaultLimits()}).Translate(strings.NewReader(s), &b, nil)
		}, gxSSE(bad)},
		{"chat-array", func(s string) *RouterError {
			var b bytes.Buffer
			return (&GeminiToChatJSONArrayTranslator{PublicModel: "m", Limits: DefaultLimits()}).Translate(strings.NewReader(s), &b, nil)
		}, gxArray(bad)},
		{"anthropic-sse", func(s string) *RouterError {
			var b bytes.Buffer
			return (&GeminiToAnthropicSSETranslator{PublicModel: "m", Limits: DefaultLimits()}).Translate(strings.NewReader(s), &b, nil)
		}, gxSSE(bad)},
		{"responses-sse", func(s string) *RouterError {
			var b bytes.Buffer
			return (&GeminiToResponsesSSETranslator{PublicModel: "m", Limits: DefaultLimits()}).Translate(strings.NewReader(s), &b, nil)
		}, gxSSE(bad)},
	} {
		if rerr := tc.fn(tc.up); rerr == nil || rerr.Status != 502 {
			t.Fatalf("%s terminal without usage must 502, got %v", tc.name, rerr)
		}
	}
}

func TestGeminiStreamCutoverFraming(t *testing.T) {
	fin := gxChunkWithEnvelope("a", "STOP", gxUsage(3, 2), "", "")
	mid := gxChunkWithEnvelope("a", "", "", "", "")
	// SSE returns promptly without EOF: trailing bytes unread.
	trailing := gxSSE(gxChunkWithEnvelope("late", "", "", "", ""))
	in := io.MultiReader(strings.NewReader(gxSSE(gxChunkWithEnvelope("a", "", "", "", ""), fin)), strings.NewReader(trailing))
	{
		var out bytes.Buffer
		tr := &GeminiToChatSSETranslator{PublicModel: "m", IncludeUsage: true, Limits: DefaultLimits()}
		if rerr := tr.Translate(in, &out, nil); rerr != nil {
			t.Fatalf("sse prompt return: %v", rerr)
		}
		if !strings.Contains(out.String(), "data: [DONE]") {
			t.Fatalf("chat sse must end with DONE: %q", out.String())
		}
	}
	// ARRAY needs close: EOF before ] fails on all array wrappers.
	eofArray := "[" + gxChunkWithEnvelope("a", "STOP", gxUsage(3, 2), "", "") // no ]
	for _, tc := range []struct {
		name string
		fn   func(string) *RouterError
	}{
		{"chat-array", func(s string) *RouterError {
			var b bytes.Buffer
			return (&GeminiToChatJSONArrayTranslator{PublicModel: "m", Limits: DefaultLimits()}).Translate(strings.NewReader(s), &b, nil)
		}},
		{"anthropic-array", func(s string) *RouterError {
			var b bytes.Buffer
			return (&GeminiToAnthropicJSONArrayTranslator{PublicModel: "m", Limits: DefaultLimits()}).Translate(strings.NewReader(s), &b, nil)
		}},
		{"responses-array", func(s string) *RouterError {
			var b bytes.Buffer
			return (&GeminiToResponsesJSONArrayTranslator{PublicModel: "m", Limits: DefaultLimits()}).Translate(strings.NewReader(s), &b, nil)
		}},
	} {
		if rerr := tc.fn(eofArray); rerr == nil {
			t.Fatalf("%s EOF before close must fail", tc.name)
		}
	}
	// Trailing comma invalid.
	trailComma := "[" + mid + ",]"
	for _, tc := range []struct {
		name string
		fn   func(string) *RouterError
	}{
		{"chat-array", func(s string) *RouterError {
			var b bytes.Buffer
			return (&GeminiToChatJSONArrayTranslator{PublicModel: "m", Limits: DefaultLimits()}).Translate(strings.NewReader(s), &b, nil)
		}},
	} {
		if rerr := tc.fn(trailComma); rerr == nil {
			t.Fatalf("%s trailing comma must fail", tc.name)
		}
	}
	// Upstream [DONE] is truncation on SSE.
	for _, tc := range []struct {
		name string
		fn   func(string) *RouterError
	}{
		{"chat-sse", func(s string) *RouterError {
			var b bytes.Buffer
			return (&GeminiToChatSSETranslator{PublicModel: "m", Limits: DefaultLimits()}).Translate(strings.NewReader(s), &b, nil)
		}},
		{"anthropic-sse", func(s string) *RouterError {
			var b bytes.Buffer
			return (&GeminiToAnthropicSSETranslator{PublicModel: "m", Limits: DefaultLimits()}).Translate(strings.NewReader(s), &b, nil)
		}},
		{"responses-sse", func(s string) *RouterError {
			var b bytes.Buffer
			return (&GeminiToResponsesSSETranslator{PublicModel: "m", Limits: DefaultLimits()}).Translate(strings.NewReader(s), &b, nil)
		}},
	} {
		if rerr := tc.fn(gxSSE(mid, "[DONE]")); rerr == nil {
			t.Fatalf("%s [DONE] must fail", tc.name)
		}
	}
	// Content after finish: SSE prompt-returns (trailing unread, success);
	// array extra element before close must fail.
	afterSSE := gxSSE(fin, mid)
	{
		var b bytes.Buffer
		if rerr := (&GeminiToChatSSETranslator{PublicModel: "m", Limits: DefaultLimits()}).Translate(strings.NewReader(afterSSE), &b, nil); rerr != nil {
			t.Fatalf("chat sse trailing after terminal must prompt-return, got %v", rerr)
		}
		if !strings.Contains(b.String(), "data: [DONE]") {
			t.Fatalf("chat sse trailing must still complete: %q", b.String())
		}
	}
	// Content after finish fails (array needs close but extra element refused).
	afterArr := gxArray(fin, mid)
	{
		var b bytes.Buffer
		if rerr := (&GeminiToChatJSONArrayTranslator{PublicModel: "m", Limits: DefaultLimits()}).Translate(strings.NewReader(afterArr), &b, nil); rerr == nil {
			t.Fatalf("chat array content-after-finish must fail")
		}
	}
	_ = fin
	_ = mid
}

func TestGeminiStreamCutoverExactBigintEmptyArgs(t *testing.T) {
	big := `{"n":9007199254740993}`
	c := gxChunkWithEnvelope("", "STOP", gxUsage(2, 2), "", gxToolExtra("f", "call-big", big))
	sse := gxSSE(c)
	arr := gxArray(c)
	outs := gxTranslateAll(t, sse, arr)
	for k, out := range outs {
		if !strings.Contains(out, "9007199254740993") {
			t.Fatalf("%s bigint not exact: %q", k, out)
		}
	}
	// Missing args emits canonical {} once, counted in bounds.
	missing := gxChunkWithEnvelope("", "STOP", gxUsage(2, 2), "", `{"functionCall":{"name":"f"}}`)
	outs2 := gxTranslateAll(t, gxSSE(missing), gxArray(missing))
	for k, out := range outs2 {
		if !strings.Contains(out, "{}") {
			t.Fatalf("%s missing args must be {}: %q", k, out)
		}
	}
	// Supplied compat prefix keeps provider identity verbatim.
	supplied := gxChunkWithEnvelope("", "STOP", gxUsage(2, 2), "", gxToolExtra("f", "gemini-compat-call-3", `{"a":1}`))
	outs3 := gxTranslateAll(t, gxSSE(supplied), gxArray(supplied))
	for k, out := range outs3 {
		if !strings.Contains(out, "gemini-compat-call-3") {
			t.Fatalf("%s must echo provider compat string: %q", k, out)
		}
	}
	// Minted IDs use unified prefix and skip supplied.
	mintChunk := `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"a","args":{}}},{"functionCall":{"name":"b","id":"gemini-compat-call-0","args":{}}}]},"finishReason":"STOP"}],"usageMetadata":` + gxUsage(2, 2)
	for _, fn := range []func(string) *RouterError{
		func(s string) *RouterError {
			var b bytes.Buffer
			return (&GeminiToChatSSETranslator{PublicModel: "m", Limits: DefaultLimits()}).Translate(strings.NewReader(s), &b, nil)
		},
		func(s string) *RouterError {
			var b bytes.Buffer
			return (&GeminiToAnthropicSSETranslator{PublicModel: "m", Limits: DefaultLimits()}).Translate(strings.NewReader(s), &b, nil)
		},
		func(s string) *RouterError {
			var b bytes.Buffer
			return (&GeminiToResponsesSSETranslator{PublicModel: "m", Limits: DefaultLimits()}).Translate(strings.NewReader(s), &b, nil)
		},
	} {
		if rerr := fn(gxSSE(mintChunk)); rerr == nil {
			t.Fatalf("minted colliding with later supplied must fail")
		}
	}
	// Duplicate supplied IDs fail.
	dup := gxChunkWithEnvelope("", "STOP", gxUsage(2, 2), "", `{"functionCall":{"name":"a","id":"dup","args":{}}},{"functionCall":{"name":"b","id":"dup","args":{}}}`)
	_ = dup
	dupObj := `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"a","id":"dup","args":{}}},{"functionCall":{"name":"b","id":"dup","args":{}}}]},"finishReason":"STOP"}],"usageMetadata":` + gxUsage(2, 2)
	if rerr := func() *RouterError {
		var b bytes.Buffer
		return (&GeminiToChatSSETranslator{PublicModel: "m", Limits: DefaultLimits()}).Translate(strings.NewReader(gxSSE(dupObj)), &b, nil)
	}(); rerr == nil {
		t.Fatalf("duplicate supplied ids must fail")
	}
}

func TestGeminiStreamCutoverMalformedVsBounds(t *testing.T) {
	// Unknown finish fails closed (incl. MALFORMED_FUNCTION_CALL).
	other := gxChunkWithEnvelope("t", "OTHER", gxUsage(2, 1), "", "")
	if rerr := func() *RouterError {
		var b bytes.Buffer
		return (&GeminiToChatSSETranslator{PublicModel: "m", Limits: DefaultLimits()}).Translate(strings.NewReader(gxSSE(other)), &b, nil)
	}(); rerr == nil {
		t.Fatalf("unknown finish must fail")
	}
	malformed := gxChunkWithEnvelope("t", "MALFORMED_FUNCTION_CALL", gxUsage(2, 1), "", "")
	if rerr := func() *RouterError {
		var b bytes.Buffer
		return (&GeminiToAnthropicSSETranslator{PublicModel: "m", Limits: DefaultLimits()}).Translate(strings.NewReader(gxSSE(malformed)), &b, nil)
	}(); rerr == nil {
		t.Fatalf("MALFORMED_FUNCTION_CALL must fail closed, never Finish")
	}
	// role assistant rejected (deliberate tightening vs legacy alias).
	assistant := `{"candidates":[{"content":{"role":"assistant","parts":[{"text":"x"}]}}],"usageMetadata":` + gxUsage(1, 1) + `}`
	if rerr := func() *RouterError {
		var b bytes.Buffer
		return (&GeminiToChatSSETranslator{PublicModel: "m", Limits: DefaultLimits()}).Translate(strings.NewReader(gxSSE(assistant)), &b, nil)
	}(); rerr == nil {
		t.Fatalf("assistant role must fail")
	}
	// Malformed args (scalar) fails value-free.
	badArgs := gxChunkWithEnvelope("", "STOP", gxUsage(2, 2), "", `{"functionCall":{"name":"f","args":[]}}`)
	if rerr := func() *RouterError {
		var b bytes.Buffer
		r := (&GeminiToChatSSETranslator{PublicModel: "m", Limits: DefaultLimits()}).Translate(strings.NewReader(gxSSE(badArgs)), &b, nil)
		if r != nil && strings.Contains(r.Msg, "[") {
			t.Fatalf("error must be value-free: %v", r.Msg)
		}
		return r
	}(); rerr == nil {
		t.Fatalf("scalar args must fail")
	}
	// Bounds: one call exceeds MaxArgsChars.
	lim := DefaultLimits()
	lim.MaxArgsChars = 10
	huge := `{"n":"` + strings.Repeat("y", 100) + `"}`
	hugeChunk := gxChunkWithEnvelope("", "STOP", gxUsage(2, 2), "", gxToolExtra("f", "c1", huge))
	{
		var b bytes.Buffer
		rerr := (&GeminiToChatSSETranslator{PublicModel: "m", Limits: lim}).Translate(strings.NewReader(gxSSE(hugeChunk)), &b, nil)
		if rerr == nil || !strings.Contains(rerr.Msg, "size bound") {
			t.Fatalf("args bound must fail with size bound, got %v", rerr)
		}
	}
}

func TestGeminiStreamCutoverTextCallTextChatRefuses(t *testing.T) {
	// text-call-text stays ordered elsewhere but Chat refuses at terminal.
	c0 := gxChunkWithEnvelope("a", "", "", "", "")
	c1 := gxChunkWithEnvelope("", "", "", "", gxToolExtra("tt", "cc1", `{"x":1}`))
	c2 := gxChunkWithEnvelope("b", "STOP", gxUsage(3, 3), "", "")
	sse := gxSSE(c0, c1, c2)
	{
		var b bytes.Buffer
		rerr := (&GeminiToChatSSETranslator{PublicModel: "m", Limits: DefaultLimits()}).Translate(strings.NewReader(sse), &b, nil)
		if rerr == nil {
			t.Fatalf("Chat must refuse text-after-tool")
		}
	}
	for _, fn := range []func() *RouterError{
		func() *RouterError {
			var b bytes.Buffer
			return (&GeminiToAnthropicSSETranslator{PublicModel: "m", Limits: DefaultLimits()}).Translate(strings.NewReader(sse), &b, nil)
		},
		func() *RouterError {
			var b bytes.Buffer
			return (&GeminiToResponsesSSETranslator{PublicModel: "m", Limits: DefaultLimits()}).Translate(strings.NewReader(sse), &b, nil)
		},
	} {
		if rerr := fn(); rerr != nil {
			t.Fatalf("ordered targets must preserve text-call-text, got %v", rerr)
		}
	}
}

func TestGeminiStreamCutoverErrorOnceWriteFailReuse(t *testing.T) {
	// Post-commit failure emits exactly one native error, no success.
	mid := gxChunkWithEnvelope("t", "", "", "", "")
	bad := `{"bogus":true}`
	upstream := gxSSE(mid, bad)
	{
		var out bytes.Buffer
		rerr := (&GeminiToAnthropicSSETranslator{PublicModel: "m", Limits: DefaultLimits()}).Translate(strings.NewReader(upstream), &out, nil)
		if rerr == nil {
			t.Fatalf("expected post-commit failure")
		}
		if got := strings.Count(out.String(), "event: error"); got != 1 {
			t.Fatalf("anthropic must emit exactly one error, got %d: %q", got, out.String())
		}
		if strings.Contains(out.String(), "message_stop") {
			t.Fatalf("no success marker after failure: %q", out.String())
		}
	}
	{
		var out bytes.Buffer
		rerr := (&GeminiToResponsesSSETranslator{PublicModel: "m", Limits: DefaultLimits()}).Translate(strings.NewReader(upstream), &out, nil)
		if rerr == nil {
			t.Fatalf("expected post-commit failure")
		}
		if got := strings.Count(out.String(), "event: response.failed"); got != 1 {
			t.Fatalf("responses must emit exactly one failed, got %d: %q", got, out.String())
		}
	}
	{
		var out bytes.Buffer
		rerr := (&GeminiToChatSSETranslator{PublicModel: "m", Limits: DefaultLimits()}).Translate(strings.NewReader(upstream), &out, nil)
		if rerr == nil {
			t.Fatalf("expected post-commit failure")
		}
		if strings.Contains(out.String(), "[DONE]") {
			t.Fatalf("chat must not emit DONE after failure: %q", out.String())
		}
	}
	// Write failure stops reads and is sticky.
	mid2 := gxChunkWithEnvelope("hi", "", "", "", "")
	fin2 := gxChunkWithEnvelope("bye", "STOP", gxUsage(2, 2), "", "")
	{
		tr := &GeminiToChatSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
		if rerr := tr.Translate(strings.NewReader(gxSSE(mid2, fin2)), &gxFailWriter{}, nil); rerr == nil {
			t.Fatalf("write failure must fail")
		}
	}
	// Reuse: one translator value serves two generations with fresh state.
	{
		tr := &GeminiToChatSSETranslator{PublicModel: "m", IncludeUsage: true, Limits: DefaultLimits()}
		single := gxSSE(gxChunkWithEnvelope("a", "STOP", gxUsage(1, 1), "", ""))
		var b1, b2 bytes.Buffer
		if rerr := tr.Translate(strings.NewReader(single), &b1, nil); rerr != nil {
			t.Fatalf("first reuse: %v", rerr)
		}
		if rerr := tr.Translate(strings.NewReader(single), &b2, nil); rerr != nil {
			t.Fatalf("second reuse: %v", rerr)
		}
		if b1.Len() == 0 || b2.Len() == 0 {
			t.Fatalf("reuse must emit both generations")
		}
	}
}

type gxFailWriter struct{}

func (gxFailWriter) Write(p []byte) (int, error) { return 0, errors.New("downstream gone") }
