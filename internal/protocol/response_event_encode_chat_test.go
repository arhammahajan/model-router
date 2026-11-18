// Chat target stream encoder tests: semantic events -> Chat SSE.
//
// These tests drive TranslateStream with fakeStreamDecoder (canonical
// events tagged Anthropic/Responses/Gemini/Chat) plus NewChatEventEncoder,
// and the encoder directly. They pin the target-owned contract without
// calling any legacy pair translator in production: role first, immediate
// fragments, dense stable indices, opaque call IDs, finish once,
// documented separate usage chunk, [DONE] only on valid terminal.
//
// Deliberate wire choices recorded here (vs legacy equivalents):
//   - Final usage is a separate empty-choices chunk (documented
//     stream_options.include_usage form, Gemini->Chat precedent). Legacy
//     Responses->Chat and Anthropic->Chat embed usage in the finish chunk;
//     this encoder prefers the documented form.
//   - Ordinary chunks carry "usage":null when includeUsage is true
//     (Gemini->Chat precedent); legacy R2C/A2C omit the key entirely.
//   - Tool identity rides the first non-empty args fragment (Anthropic
//     precedent); empty tools emit one identity+"{}" at close. Legacy R2C
//     emits a separate empty identity at item-added time.
package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func chatEncUsage(source Dialect, prompt, completion int) ResponseUsage {
	u, err := NewTerminalUsage(source, prompt, completion, prompt+completion)
	if err != nil {
		panic(err)
	}
	return u
}

func chatEncRun(t *testing.T, dec *fakeStreamDecoder, includeUsage bool, lim Limits) (string, *RouterError) {
	t.Helper()
	enc := NewChatEventEncoder("public", includeUsage, lim)
	var out bytes.Buffer
	raw := strings.Repeat("data: x\n\n", len(dec.frames))
	rerr := TranslateStream(sseSource(raw, lim), dec, enc, &out, nil, lim, dec.frames[0][0].(ResponseStart).Source)
	return out.String(), rerr
}

func chatEncStart(source Dialect) ResponseEvent {
	return ResponseStart{ID: "src-1", Model: "up-model", Source: source}
}

func chatEncTextBlock(t *testing.T, item string) (BlockOpen, BlockClose) {
	t.Helper()
	open, rerr := NewBlockOpen(ItemID(item), BlockText, "", "")
	if rerr != nil {
		t.Fatal(rerr)
	}
	return open, BlockClose{Item: ItemID(item)}
}

func chatEncToolBlock(t *testing.T, item, call, name string, idx int) (BlockOpen, BlockClose) {
	t.Helper()
	open, rerr := NewToolBlockOpen(ItemID(item), CallID(call), name, idx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	return open, BlockClose{Item: ItemID(item)}
}

func chatEncFinish(t *testing.T, kind FinishKind) CompletionEvent {
	t.Helper()
	ev, rerr := NewCompletionEvent(mustFinish(t, kind))
	if rerr != nil {
		t.Fatal(rerr)
	}
	return ev
}

func chatEncChunks(t *testing.T, out string) []map[string]any {
	t.Helper()
	var chunks []map[string]any
	for _, part := range strings.Split(out, "data: ") {
		part = strings.TrimSpace(part)
		if part == "" || part == "[DONE]" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(part), &m); err != nil {
			t.Fatalf("chunk not JSON: %v %q", err, part)
		}
		chunks = append(chunks, m)
	}
	return chunks
}

func TestChatEncoderRoleTextToolFlow(t *testing.T) {
	lim := testRespLimits()
	openT, closeT := chatEncTextBlock(t, "t0")
	openC, closeC := chatEncToolBlock(t, "c0", "call-A", "get_time", 0)
	usage := chatEncUsage(DialectChat, 5, 2)
	dec := &fakeStreamDecoder{frames: [][]ResponseEvent{
		{chatEncStart(DialectChat), openT},
		{TextFragment{Item: "t0", Text: "Hello"}},
		{closeT, openC},
		{ArgsFragment{Item: "c0", Fragment: `{"tz":"`}},
		{ArgsFragment{Item: "c0", Fragment: `UTC"}`}},
		{closeC, UsageEvent{Usage: usage}, chatEncFinish(t, FinishStop)},
	}}
	out, rerr := chatEncRun(t, dec, true, lim)
	if rerr != nil {
		t.Fatalf("stream: %v", rerr)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Fatalf("missing DONE:\n%s", out)
	}
	chunks := chatEncChunks(t, out)
	if len(chunks) < 5 {
		t.Fatalf("want role+text+tool+finish+usage, got %d:\n%s", len(chunks), out)
	}
	first := chunks[0]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if first["role"] != "assistant" {
		t.Fatalf("first delta must be role:\n%s", out)
	}
	for _, c := range chunks {
		if c["model"] != "public" {
			t.Fatalf("public model on every chunk:\n%s", out)
		}
	}
	if !strings.Contains(out, `"content":"Hello"`) {
		t.Fatalf("text missing:\n%s", out)
	}
	if !strings.Contains(out, `"id":"call-A"`) || !strings.Contains(out, `"name":"get_time"`) {
		t.Fatalf("tool identity missing:\n%s", out)
	}
	if !strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Fatalf("stop+tools must coerce to tool_calls:\n%s", out)
	}
	// Documented separate usage chunk immediately before DONE.
	if !strings.Contains(out, `"choices":[]`) || !strings.Contains(out, `"prompt_tokens":5`) {
		t.Fatalf("separate usage chunk missing:\n%s", out)
	}
	idx := strings.Index(out, `"finish_reason":"tool_calls"`)
	uidx := strings.Index(out, `"choices":[]`)
	didx := strings.Index(out, "data: [DONE]")
	if !(idx < uidx && uidx < didx) {
		t.Fatalf("finish < usage < DONE order wrong:\n%s", out)
	}
	// Ordinary chunks carry usage null when includeUsage.
	if !strings.Contains(out, `"usage":null`) {
		t.Fatalf("ordinary chunks must carry usage null when includeUsage:\n%s", out)
	}
}

func TestChatEncoderOpaqueIDsBigintEmpty(t *testing.T) {
	lim := DefaultLimits()
	long := strings.Repeat("z", 90)
	open1, close1 := chatEncToolBlock(t, "c0", "has:colon and space", "f", 5)
	open2, close2 := chatEncToolBlock(t, "c1", long, "g", 2)
	usage := chatEncUsage(DialectChat, 5, 2)
	big := `{"n":9007199254740993}`
	dec := &fakeStreamDecoder{frames: [][]ResponseEvent{
		{chatEncStart(DialectChat), open1},
		{ArgsFragment{Item: "c0", Fragment: big}, close1, open2},
		{close2, UsageEvent{Usage: usage}, chatEncFinish(t, FinishToolCalls)},
	}}
	out, rerr := chatEncRun(t, dec, false, lim)
	if rerr != nil {
		t.Fatalf("stream: %v", rerr)
	}
	if !strings.Contains(out, "has:colon and space") || !strings.Contains(out, long) {
		t.Fatalf("opaque IDs lost:\n%s", out)
	}
	if !strings.Contains(out, "9007199254740993") {
		t.Fatalf("bigint not exact:\n%s", out)
	}
	// Empty tool (no fragments) emits identity+"{}".
	openE, closeE := chatEncToolBlock(t, "e0", "empty-call", "ping", 0)
	dec2 := &fakeStreamDecoder{frames: [][]ResponseEvent{
		{chatEncStart(DialectChat), openE, closeE, UsageEvent{Usage: usage}, chatEncFinish(t, FinishToolCalls)},
	}}
	out2, rerr := chatEncRun(t, dec2, false, lim)
	if rerr != nil {
		t.Fatalf("empty tool: %v", rerr)
	}
	if !strings.Contains(out2, `"id":"empty-call"`) || !strings.Contains(out2, `"arguments":"{}"`) {
		t.Fatalf("empty tool must emit {}:\n%s", out2)
	}
	// Dense Chat indices follow arrival, not source indices.
	i0 := strings.Index(out, `"index":0`)
	i1 := strings.Index(out, `"index":1`)
	if i0 < 0 || i1 < 0 || i0 > i1 {
		t.Fatalf("dense indices not monotonic arrival:\n%s", out)
	}
}

func TestChatEncoderUsageCrossSource(t *testing.T) {
	lim := testRespLimits()
	mk := func(source Dialect, u ResponseUsage) (string, *RouterError) {
		open, close := chatEncTextBlock(t, "t0")
		dec := &fakeStreamDecoder{frames: [][]ResponseEvent{
			{ResponseStart{ID: "s", Model: "m", Source: source}, open, TextFragment{Item: "t0", Text: "hi"}, close, UsageEvent{Usage: u}, chatEncFinish(t, FinishStop)},
		}}
		return chatEncRun(t, dec, true, lim)
	}
	// Anthropic inclusive prompt (input+creation+read) renders verbatim
	// with cached detail read.
	au, _ := NewTerminalUsage(DialectAnthropic, 12, 3, 15)
	au, _ = au.WithCache(intPtr(2), intPtr(4))
	out, rerr := mk(DialectAnthropic, au)
	if rerr != nil {
		t.Fatalf("anthropic cache: %v", rerr)
	}
	if !strings.Contains(out, `"prompt_tokens":12`) || !strings.Contains(out, `"cached_tokens":4`) {
		t.Fatalf("anthropic cache mapping wrong:\n%s", out)
	}
	// Responses reasoning scalar maps to Chat detail.
	ru, _ := NewTerminalUsage(DialectResponses, 7, 5, 12)
	ru, _ = ru.WithReasoning(intPtr(2))
	out, rerr = mk(DialectResponses, ru)
	if rerr != nil {
		t.Fatalf("responses reasoning: %v", rerr)
	}
	if !strings.Contains(out, `"reasoning_tokens":2`) {
		t.Fatalf("reasoning not mapped:\n%s", out)
	}
	// Responses cache_write (CacheCreation, non-Anthropic) refuses Chat.
	bu, _ := NewTerminalUsage(DialectResponses, 7, 2, 9)
	bu, _ = bu.WithCache(intPtr(1), nil)
	if _, rerr := mk(DialectResponses, bu); rerr == nil {
		t.Fatal("responses cache_write must refuse Chat")
	}
	// Unknown nonzero buckets refuse, never fabricated zeros.
	// Note: TextTokens nonzero is harmless-subsumed for the Chat
	// destination (totals carry it), so it passes here; it refuses
	// only for Anthropic/Gemini destinations per the shared gate.
	for name, mut := range map[string]func(*ResponseUsage){
		"thoughts": func(u *ResponseUsage) { u.Thoughts = intPtr(1) },
		"tool_use": func(u *ResponseUsage) { u.ToolUsePrompt = intPtr(1) },
	} {
		u, _ := NewTerminalUsage(DialectAnthropic, 5, 2, 7)
		mut(&u)
		if _, rerr := mk(DialectAnthropic, u); rerr == nil {
			t.Fatalf("%s nonzero must refuse Chat", name)
		}
	}
	// Zero known buckets are representable.
	zu, _ := NewTerminalUsage(DialectAnthropic, 5, 2, 7)
	zero := 0
	zu.TextTokens = &zero
	if _, rerr := mk(DialectAnthropic, zu); rerr != nil {
		t.Fatalf("zero text must pass: %v", rerr)
	}
}

func TestChatEncoderTerminalFinishMapping(t *testing.T) {
	lim := testRespLimits()
	cases := []struct {
		kind FinishKind
		want string
	}{
		{FinishStop, "stop"},
		{FinishLength, "length"},
		{FinishContentFilter, "content_filter"},
		{FinishRefusal, "content_filter"},
		{FinishToolCalls, "tool_calls"},
	}
	for _, c := range cases {
		open, close := chatEncTextBlock(t, "t0")
		dec := &fakeStreamDecoder{frames: [][]ResponseEvent{
			{chatEncStart(DialectChat), open, TextFragment{Item: "t0", Text: "x"}, close, UsageEvent{Usage: chatEncUsage(DialectChat, 1, 1)}, chatEncFinish(t, c.kind)},
		}}
		out, rerr := chatEncRun(t, dec, false, lim)
		if rerr != nil {
			t.Fatalf("%s: %v", c.kind, rerr)
		}
		if !strings.Contains(out, `"finish_reason":"`+c.want+`"`) {
			t.Fatalf("%s want %s:\n%s", c.kind, c.want, out)
		}
	}
	// Explicit refusal finish maps to content_filter.
	open, close := chatEncToolBlock(t, "c0", "call1", "f", 0)
	_ = open
	_ = close
	ropen, _ := NewBlockOpen("r0", BlockRefusal, "", "")
	dec := &fakeStreamDecoder{frames: [][]ResponseEvent{
		{chatEncStart(DialectChat), ropen, TextFragment{Item: "r0", Text: "no"}, BlockClose{Item: "r0"}, UsageEvent{Usage: chatEncUsage(DialectChat, 1, 1)}, chatEncFinish(t, FinishRefusal)},
	}}
	out, rerr := chatEncRun(t, dec, false, lim)
	if rerr != nil {
		t.Fatalf("refusal finish: %v", rerr)
	}
	if !strings.Contains(out, `"finish_reason":"content_filter"`) {
		t.Fatalf("refusal must map to content_filter:\n%s", out)
	}
}

func TestChatEncoderMalformedOrderRefuse(t *testing.T) {
	lim := testRespLimits()
	// Malformed refuses immediately, no DONE.
	open, close := chatEncToolBlock(t, "c0", "c1", "f", 0)
	_ = close
	dec := &fakeStreamDecoder{frames: [][]ResponseEvent{
		{chatEncStart(DialectChat), open, MalformedToolEvent{Item: "c0"}},
	}}
	out, rerr := chatEncRun(t, dec, true, lim)
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("malformed must 502, got %v", rerr)
	}
	if strings.Contains(out, "[DONE]") {
		t.Fatalf("malformed must not DONE:\n%s", out)
	}
	// Text-after-tool refuses at terminal (complete Chat policy).
	ot, ct := chatEncTextBlock(t, "t0")
	oc, cc := chatEncToolBlock(t, "c0", "call1", "f", 0)
	dec2 := &fakeStreamDecoder{frames: [][]ResponseEvent{
		{chatEncStart(DialectChat), oc, ArgsFragment{Item: "c0", Fragment: "{}"}, cc, ot, TextFragment{Item: "t0", Text: "late"}, ct, UsageEvent{Usage: chatEncUsage(DialectChat, 1, 1)}, chatEncFinish(t, FinishStop)},
	}}
	out, rerr = chatEncRun(t, dec2, false, lim)
	if rerr == nil {
		t.Fatal("text-after-tool must refuse")
	}
	if strings.Contains(out, "[DONE]") {
		t.Fatalf("order refusal must not DONE:\n%s", out)
	}
	// Refusal+tools refuses.
	ro, _ := NewBlockOpen("r0", BlockRefusal, "", "")
	dec3 := &fakeStreamDecoder{frames: [][]ResponseEvent{
		{chatEncStart(DialectChat), ro, TextFragment{Item: "r0", Text: "no"}, BlockClose{Item: "r0"}, oc, ArgsFragment{Item: "c0", Fragment: "{}"}, cc, UsageEvent{Usage: chatEncUsage(DialectChat, 1, 1)}, chatEncFinish(t, FinishStop)},
	}}
	// Fresh tool block for dec3 (oc/cc reuse same IDs is fine: new decoder run).
	_ = dec3
	out, rerr = chatEncRun(t, dec3, false, lim)
	if rerr == nil {
		t.Fatal("refusal+tools must refuse")
	}
	if strings.Contains(out, "[DONE]") {
		t.Fatalf("refusal+tools must not DONE:\n%s", out)
	}
}

func TestChatEncoderFirstDeltaBeforeTerminalLargeText(t *testing.T) {
	lim := DefaultLimits()
	open, close := chatEncTextBlock(t, "t0")
	big := strings.Repeat("x", 2_000_000)
	usage := chatEncUsage(DialectGemini, 9, 4)
	dec := &fakeStreamDecoder{frames: [][]ResponseEvent{
		{ResponseStart{ID: "g", Model: "m", Source: DialectGemini}, open, TextFragment{Item: "t0", Text: "early"}, TextFragment{Item: "t0", Text: big}, close, UsageEvent{Usage: usage}, chatEncFinish(t, FinishStop)},
	}}
	enc := NewChatEventEncoder("public", false, lim)
	var out bytes.Buffer
	raw := strings.Repeat("data: x\n\n", len(dec.frames))
	if rerr := TranslateStream(sseSource(raw, lim), dec, enc, &out, nil, lim, DialectGemini); rerr != nil {
		t.Fatalf("large text: %v", rerr)
	}
	s := out.String()
	if strings.Index(s, "early") < 0 || strings.Index(s, `"finish_reason":"stop"`) < 0 {
		t.Fatalf("missing deltas:\n%.200s", s)
	}
	if strings.Index(s, "early") > strings.Index(s, `"finish_reason":"stop"`) {
		t.Fatal("first delta must precede terminal (no whole-text buffering)")
	}
}

func TestChatEncoderFrameBound(t *testing.T) {
	lim := DefaultLimits()
	lim.MaxFrameBytes = 512
	enc := NewChatEventEncoder("public", false, lim)
	var sb strings.Builder
	if eerr := enc.Encode(&sb, nil, ResponseStart{ID: "r", Model: "m", Source: DialectChat}); eerr != nil {
		t.Fatal(eerr.Err)
	}
	open, _ := NewBlockOpen("t0", BlockText, "", "")
	if eerr := enc.Encode(&sb, nil, open); eerr != nil {
		t.Fatal(eerr.Err)
	}
	if eerr := enc.Encode(&sb, nil, TextFragment{Item: "t0", Text: strings.Repeat("y", 2048)}); eerr == nil {
		t.Fatal("oversize encoded frame must fail")
	}
}

type chatFailWriter struct{ writes int }

func (w *chatFailWriter) Write(p []byte) (int, error) {
	w.writes++
	return 0, errors.New("downstream closed")
}

func TestChatEncoderWriterFailureNoSecondary(t *testing.T) {
	lim := DefaultLimits()
	open, _ := chatEncTextBlock(t, "t0")
	dec := &fakeStreamDecoder{frames: [][]ResponseEvent{
		{chatEncStart(DialectChat), open, TextFragment{Item: "t0", Text: "hi"}, BlockClose{Item: "t0"}, UsageEvent{Usage: chatEncUsage(DialectChat, 2, 1)}, chatEncFinish(t, FinishStop)},
	}}
	enc := NewChatEventEncoder("p", false, lim)
	fw := &chatFailWriter{}
	br := &cgmByteReader{data: []byte("data: x\n\n")}
	rerr := TranslateStream(sseSource("data: x\n\n", lim), dec, enc, fw, nil, lim, DialectChat)
	_ = br
	if rerr == nil {
		t.Fatal("write failure swallowed")
	}
	if fw.writes != 1 {
		t.Fatalf("want exactly 1 downstream write, got %d", fw.writes)
	}
	// Sticky: further Encode is WriteFailed, Abort emits nothing.
	var extra bytes.Buffer
	if eerr := enc.Encode(&extra, nil, TextFragment{Item: "t0", Text: "late"}); eerr == nil || !eerr.WriteFailed {
		t.Fatalf("post-failure Encode must be WriteFailed: %v", eerr)
	}
	before := fw.writes
	enc.Abort(fw, nil)
	if fw.writes != before {
		t.Fatal("Abort after write failure must not write")
	}
}

func TestChatEncoderAbortNoOutputIdempotent(t *testing.T) {
	lim := testRespLimits()
	open, _ := chatEncTextBlock(t, "t0")
	dec := &fakeStreamDecoder{frames: [][]ResponseEvent{
		{chatEncStart(DialectChat), open, TextFragment{Item: "t0", Text: "hi"}},
	}}
	enc := NewChatEventEncoder("p", true, lim)
	var buf bytes.Buffer
	frames := NewSSEFrameReader(strings.NewReader("data: x\n\n"), lim)
	if rerr := TranslateStream(frames, dec, enc, &buf, nil, lim, DialectChat); rerr != nil {
		// Decode hits truncation (no terminal) after committing text;
		// pipeline Aborts once. Output must have no DONE/error chunk.
		if !strings.Contains(rerr.Msg, "completion") && rerr.Status != 502 {
			t.Fatalf("unexpected: %v", rerr)
		}
	}
	if strings.Contains(buf.String(), "[DONE]") {
		t.Fatalf("failure must not DONE:\n%s", buf.String())
	}
	before := buf.Len()
	enc.Abort(&buf, nil)
	enc.Abort(&buf, nil)
	if buf.Len() != before {
		t.Fatal("Abort must emit nothing and be idempotent")
	}
}

func TestChatEncoderSourceGuardAndUsagePresence(t *testing.T) {
	lim := testRespLimits()
	// Delta before start: zero bytes.
	enc := NewChatEventEncoder("p", false, lim)
	var sb strings.Builder
	if eerr := enc.Encode(&sb, nil, TextFragment{Item: "t0", Text: "hi"}); eerr == nil {
		t.Fatal("content before start must fail")
	}
	if sb.Len() != 0 {
		t.Fatal("pre-commit failure must emit zero bytes")
	}
	// Terminal without usage fails even when includeUsage=false.
	open, close := chatEncTextBlock(t, "t0")
	dec := &fakeStreamDecoder{frames: [][]ResponseEvent{
		{chatEncStart(DialectChat), open, TextFragment{Item: "t0", Text: "hi"}, close, chatEncFinish(t, FinishStop)},
	}}
	if _, rerr := chatEncRun(t, dec, false, lim); rerr == nil {
		t.Fatal("missing terminal usage must fail even with includeUsage=false")
	}
	// InitialUsage never substitutes for terminal.
	initial := chatEncUsage(DialectChat, 9, 9)
	initial.Provenance.Scope = UsageInitial
	dec2 := &fakeStreamDecoder{frames: [][]ResponseEvent{
		{ResponseStart{ID: "s", Model: "m", Source: DialectChat, InitialUsage: &initial}, open, TextFragment{Item: "t0", Text: "hi"}, close, chatEncFinish(t, FinishStop)},
	}}
	if _, rerr := chatEncRun(t, dec2, true, lim); rerr == nil {
		t.Fatal("initial usage must not satisfy terminal")
	}
	// Unrepresentable initial refuses before commitment with zero bytes.
	badInit := chatEncUsage(DialectResponses, 7, 2)
	badInit, _ = badInit.WithCache(intPtr(1), nil)
	badInit.Provenance.Scope = UsageInitial
	enc2 := NewChatEventEncoder("p", true, testRespLimits())
	var sb2 strings.Builder
	if eerr := enc2.Encode(&sb2, nil, ResponseStart{ID: "s", Model: "m", Source: DialectResponses, InitialUsage: &badInit}); eerr == nil {
		t.Fatal("unrepresentable initial must refuse")
	}
	if sb2.Len() != 0 {
		t.Fatal("pre-commit initial refusal must emit zero bytes")
	}
	// includeUsage=false omits usage on wire.
	open3, close3 := chatEncTextBlock(t, "t0")
	dec3 := &fakeStreamDecoder{frames: [][]ResponseEvent{
		{chatEncStart(DialectChat), open3, TextFragment{Item: "t0", Text: "hi"}, close3, UsageEvent{Usage: chatEncUsage(DialectChat, 3, 1)}, chatEncFinish(t, FinishStop)},
	}}
	out, rerr := chatEncRun(t, dec3, false, lim)
	if rerr != nil {
		t.Fatalf("includeUsage=false: %v", rerr)
	}
	if strings.Contains(out, "prompt_tokens") {
		t.Fatalf("includeUsage=false must omit accounting:\n%s", out)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Fatalf("missing DONE:\n%s", out)
	}
}

func TestChatEncoderEmptyGeneration(t *testing.T) {
	lim := testRespLimits()
	dec := &fakeStreamDecoder{frames: [][]ResponseEvent{
		{chatEncStart(DialectChat), UsageEvent{Usage: chatEncUsage(DialectChat, 1, 1)}, chatEncFinish(t, FinishStop)},
	}}
	out, rerr := chatEncRun(t, dec, true, lim)
	if rerr != nil {
		t.Fatalf("empty: %v", rerr)
	}
	if !strings.Contains(out, `"role":"assistant"`) || !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("empty must still role+finish:\n%s", out)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Fatalf("missing DONE:\n%s", out)
	}
}
