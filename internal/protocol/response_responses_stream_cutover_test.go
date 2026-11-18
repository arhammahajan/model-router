package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

// Responses-upstream stream cutover: one shared source decoder, framing,
// pipeline and reusable Chat/Anthropic/Gemini target event encoders.
//
// Every test drives the SAME Responses SSE wire bytes through all public
// wrappers (ResponsesToChatSSETranslator, ResponsesToAnthropicSSETranslator,
// ResponsesToGeminiSSETranslator, ResponsesToGeminiJSONArrayTranslator).
// Wrappers must select (NewSSEFrameReader, NewResponsesStreamDecoder,
// target encoder, TranslateStream(DialectResponses)) with fresh pipeline
// objects per Translate and no other semantic state.

func rsCutSSE(t *testing.T, data string) string {
	t.Helper()
	var body map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &body); err != nil {
		t.Fatalf("bad fixture json: %v", err)
	}
	var typ string
	if rawT, ok := body["type"]; ok {
		_ = json.Unmarshal(rawT, &typ)
	}
	if typ == "" {
		return "data: " + data + "\n\n"
	}
	return "event: " + typ + "\ndata: " + data + "\n\n"
}

func rsCutStream(t *testing.T, datas []string) string {
	t.Helper()
	var sb strings.Builder
	for _, d := range datas {
		sb.WriteString(rsCutSSE(t, d))
	}
	return sb.String()
}

func rsCutUsage(prompt, completion int) map[string]any {
	return map[string]any{
		"input_tokens": prompt, "output_tokens": completion,
		"total_tokens": prompt + completion,
	}
}

func rsCutRunAll(t *testing.T, upstream string) (chat, anth, gemSSE, gemArr string) {
	t.Helper()
	var cb, ab, gb, jb bytes.Buffer
	lim := DefaultLimits()
	ctr := &ResponsesToChatSSETranslator{PublicModel: "public", IncludeUsage: true, Limits: lim}
	if rerr := ctr.Translate(strings.NewReader(upstream), &cb, nil); rerr != nil {
		t.Fatalf("chat: %v", rerr)
	}
	atr := &ResponsesToAnthropicSSETranslator{PublicModel: "public", Limits: lim}
	if rerr := atr.Translate(strings.NewReader(upstream), &ab, nil); rerr != nil {
		t.Fatalf("anthropic: %v", rerr)
	}
	gtr := &ResponsesToGeminiSSETranslator{PublicModel: "public", Limits: lim}
	if rerr := gtr.Translate(strings.NewReader(upstream), &gb, nil); rerr != nil {
		t.Fatalf("gemini sse: %v", rerr)
	}
	jtr := &ResponsesToGeminiJSONArrayTranslator{PublicModel: "public", Limits: DefaultLimits()}
	if rerr := jtr.Translate(strings.NewReader(upstream), &jb, nil); rerr != nil {
		t.Fatalf("gemini array: %v", rerr)
	}
	return cb.String(), ab.String(), gb.String(), jb.String()
}

func rsCutFullTextTool(t *testing.T) string {
	t.Helper()
	bigArgs := `{"n":9007199254740993}`
	usage := rsCutUsage(25, 15)
	resp := rsResp(t, "completed", []any{
		rsMsgOut("msg_1", "hello"),
		rsToolOut("fc_1", "call:opaque 1", "get_time", bigArgs),
	}, usage, "")
	return rsCutStream(t, []string{
		rsCreatedRaw(t, 0, "resp_7", "gpt-4o"),
		rsMsgAdded(t, 1, 0, "msg_1"),
		rsPartAdded(t, 2, 0, "msg_1", "output_text"),
		rsDelta(t, 3, 0, "msg_1", "hello", false),
		rsDone(t, 4, 0, "msg_1", "hello", false),
		rsPartDone(t, 5, 0, "msg_1", "hello", false),
		rsItemDoneMsg(t, 6, 0, "msg_1", "hello", false),
		rsToolAdded(t, 7, 1, "fc_1", "call:opaque 1", "get_time", ""),
		rsArgsDelta(t, 8, 1, "fc_1", `{"n":`),
		rsArgsDelta(t, 9, 1, "fc_1", `9007199254740993}`),
		rsArgsDone(t, 10, 1, "fc_1", bigArgs),
		rsItemDoneTool(t, 11, 1, "fc_1", "call:opaque 1", "get_time", bigArgs),
		rsTerminal(t, 12, "completed", resp),
	})
}

func TestResponsesStreamCutoverCrossTargetSameSource(t *testing.T) {
	upstream := rsCutFullTextTool(t)
	chat, anth, gemSSE, gemArr := rsCutRunAll(t, upstream)
	if !strings.Contains(chat, `"content":"hello"`) || !strings.Contains(chat, `"call:opaque 1"`) {
		t.Fatalf("chat missing text/tool: %s", chat)
	}
	if !strings.Contains(chat, "9007199254740993") || !strings.Contains(chat, `"finish_reason":"tool_calls"`) {
		t.Fatalf("chat bigint/finish: %s", chat)
	}
	if !strings.HasSuffix(chat, "data: [DONE]\n\n") {
		t.Fatalf("chat missing DONE: %s", chat)
	}
	if !strings.Contains(anth, `"text_delta"`) || !strings.Contains(anth, `"call:opaque 1"`) {
		t.Fatalf("anthropic missing text/tool: %s", anth)
	}
	if !strings.Contains(anth, "9007199254740993") || !strings.Contains(anth, `"stop_reason":"tool_use"`) {
		t.Fatalf("anthropic bigint/stop: %s", anth)
	}
	if !strings.Contains(anth, "event: message_stop") {
		t.Fatalf("anthropic missing stop: %s", anth)
	}
	if !strings.Contains(gemSSE, `"text":"hello"`) || !strings.Contains(gemSSE, `"call:opaque 1"`) {
		t.Fatalf("gemini sse missing text/tool: %s", gemSSE)
	}
	if !strings.Contains(gemSSE, "9007199254740993") || !strings.Contains(gemSSE, `"finishReason":"STOP"`) {
		t.Fatalf("gemini sse bigint/finish: %s", gemSSE)
	}
	if strings.Contains(gemSSE, "[DONE]") {
		t.Fatalf("gemini sse must not carry DONE: %s", gemSSE)
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(gemArr), &arr); err != nil || len(arr) < 2 {
		t.Fatalf("gemini array invalid: %q err=%v", gemArr, err)
	}
	if !strings.Contains(gemArr, `"call:opaque 1"`) || !strings.Contains(gemArr, "9007199254740993") {
		t.Fatalf("gemini array missing tool/bigint: %s", gemArr)
	}
}

func TestResponsesStreamCutoverLateModel(t *testing.T) {
	usage := rsCutUsage(7, 3)
	respWithModel := rsRespID(t, "resp_7", "gpt-4o", "completed", []any{rsMsgOut("msg_1", "hi")}, usage, "")
	upstream := rsCutStream(t, []string{
		rsCreatedRaw(t, 0, "resp_7", ""),
		rsMsgAdded(t, 1, 0, "msg_1"),
		rsPartAdded(t, 2, 0, "msg_1", "output_text"),
		rsDelta(t, 3, 0, "msg_1", "hi", false),
		rsDone(t, 4, 0, "msg_1", "hi", false),
		rsPartDone(t, 5, 0, "msg_1", "hi", false),
		rsItemDoneMsg(t, 6, 0, "msg_1", "hi", false),
		rsTerminal(t, 7, "completed", respWithModel),
	})
	chat, anth, gemSSE, gemArr := rsCutRunAll(t, upstream)
	if !strings.Contains(chat, `"finish_reason":"stop"`) {
		t.Fatalf("chat late model: %s", chat)
	}
	if !strings.Contains(anth, "event: message_stop") {
		t.Fatalf("anthropic late model: %s", anth)
	}
	if !strings.Contains(gemSSE, `"finishReason":"STOP"`) {
		t.Fatalf("gemini late model: %s", gemSSE)
	}
	var arr []any
	if err := json.Unmarshal([]byte(gemArr), &arr); err != nil {
		t.Fatalf("gemini array late model: %v %s", err, gemArr)
	}
}

func TestResponsesStreamCutoverCacheReasoning(t *testing.T) {
	usageCached := map[string]any{
		"input_tokens": 12, "output_tokens": 7, "total_tokens": 19,
		"input_tokens_details": map[string]any{"cached_tokens": 4},
	}
	respCached := rsResp(t, "completed", []any{rsMsgOut("msg_1", "hi")}, usageCached, "")
	cached := rsCutStream(t, []string{
		rsCreatedRaw(t, 0, "resp_7", "gpt-4o"),
		rsMsgAdded(t, 1, 0, "msg_1"),
		rsPartAdded(t, 2, 0, "msg_1", "output_text"),
		rsDelta(t, 3, 0, "msg_1", "hi", false),
		rsDone(t, 4, 0, "msg_1", "hi", false),
		rsPartDone(t, 5, 0, "msg_1", "hi", false),
		rsItemDoneMsg(t, 6, 0, "msg_1", "hi", false),
		rsTerminal(t, 7, "completed", respCached),
	})
	chat, anth, gemSSE, _ := rsCutRunAll(t, cached)
	if !strings.Contains(chat, `"prompt_tokens":12`) || !strings.Contains(chat, `"cached_tokens":4`) {
		t.Fatalf("chat cache: %s", chat)
	}
	if !strings.Contains(anth, `"input_tokens":8`) || !strings.Contains(anth, `"cache_read_input_tokens":4`) {
		t.Fatalf("anthropic cache subtraction: %s", anth)
	}
	if !strings.Contains(gemSSE, `"promptTokenCount":12`) || !strings.Contains(gemSSE, `"cachedContentTokenCount":4`) {
		t.Fatalf("gemini cache: %s", gemSSE)
	}
	usageReason := map[string]any{
		"input_tokens": 7, "output_tokens": 3, "total_tokens": 10,
		"output_tokens_details": map[string]any{"reasoning_tokens": 2},
	}
	respReason := rsResp(t, "completed", []any{rsMsgOut("msg_1", "hi")}, usageReason, "")
	reason := rsCutStream(t, []string{
		rsCreatedRaw(t, 0, "resp_7", "gpt-4o"),
		rsMsgAdded(t, 1, 0, "msg_1"),
		rsPartAdded(t, 2, 0, "msg_1", "output_text"),
		rsDelta(t, 3, 0, "msg_1", "hi", false),
		rsDone(t, 4, 0, "msg_1", "hi", false),
		rsPartDone(t, 5, 0, "msg_1", "hi", false),
		rsItemDoneMsg(t, 6, 0, "msg_1", "hi", false),
		rsTerminal(t, 7, "completed", respReason),
	})
	var cb bytes.Buffer
	if rerr := (&ResponsesToChatSSETranslator{PublicModel: "p", IncludeUsage: true, Limits: DefaultLimits()}).Translate(strings.NewReader(reason), &cb, nil); rerr != nil {
		t.Fatalf("chat reasoning: %v", rerr)
	}
	if !strings.Contains(cb.String(), `"reasoning_tokens":2`) {
		t.Fatalf("chat reasoning detail: %s", cb.String())
	}
	var gb bytes.Buffer
	if rerr := (&ResponsesToGeminiSSETranslator{PublicModel: "p", Limits: DefaultLimits()}).Translate(strings.NewReader(reason), &gb, nil); rerr != nil {
		t.Fatalf("gemini reasoning: %v", rerr)
	}
	if !strings.Contains(gb.String(), `"thoughtsTokenCount":2`) {
		t.Fatalf("gemini thoughts: %s", gb.String())
	}
	var ab bytes.Buffer
	rerr := (&ResponsesToAnthropicSSETranslator{PublicModel: "p", Limits: DefaultLimits()}).Translate(strings.NewReader(reason), &ab, nil)
	if rerr == nil || !strings.Contains(rerr.Msg, "reasoning_tokens") {
		t.Fatalf("anthropic reasoning must refuse, got %v %s", rerr, ab.String())
	}
	if strings.Contains(ab.String(), "message_stop") {
		t.Fatalf("anthropic refusal must not succeed: %s", ab.String())
	}
}

func TestResponsesStreamCutoverMalformedOutcome(t *testing.T) {
	badArgs := `[1]`
	respBad := rsResp(t, "completed", []any{rsToolOut("fc_1", "c1", "has space", badArgs)}, rsCutUsage(1, 1), "")
	upstream := rsCutStream(t, []string{
		rsCreatedRaw(t, 0, "resp_7", "gpt-4o"),
		rsToolAdded(t, 1, 0, "fc_1", "c1", "has space", ""),
		rsArgsDelta(t, 2, 0, "fc_1", "[1]"),
		rsArgsDone(t, 3, 0, "fc_1", badArgs),
		rsItemDoneTool(t, 4, 0, "fc_1", "c1", "has space", badArgs),
		rsTerminal(t, 5, "completed", respBad),
	})
	var gb bytes.Buffer
	if rerr := (&ResponsesToGeminiSSETranslator{PublicModel: "p", Limits: DefaultLimits()}).Translate(strings.NewReader(upstream), &gb, nil); rerr != nil {
		t.Fatalf("gemini malformed must succeed with native terminal, got %v", rerr)
	}
	if !strings.Contains(gb.String(), "MALFORMED_FUNCTION_CALL") {
		t.Fatalf("gemini malformed finish: %s", gb.String())
	}
	if strings.Contains(gb.String(), "has space") && strings.Contains(gb.String(), "functionCall") {
		t.Fatalf("gemini must suppress malformed call: %s", gb.String())
	}
	for name, tr := range map[string]func(string) (string, *RouterError){
		"chat": func(s string) (string, *RouterError) {
			var b bytes.Buffer
			rerr := (&ResponsesToChatSSETranslator{PublicModel: "p", Limits: DefaultLimits()}).Translate(strings.NewReader(s), &b, nil)
			return b.String(), rerr
		},
		"anthropic": func(s string) (string, *RouterError) {
			var b bytes.Buffer
			rerr := (&ResponsesToAnthropicSSETranslator{PublicModel: "p", Limits: DefaultLimits()}).Translate(strings.NewReader(s), &b, nil)
			return b.String(), rerr
		},
	} {
		out, rerr := tr(upstream)
		if rerr == nil || rerr.Status != 502 {
			t.Fatalf("%s malformed must 502, got %v %s", name, rerr, out)
		}
		if strings.Contains(out, "[DONE]") || strings.Contains(out, "message_stop") {
			t.Fatalf("%s malformed must not succeed: %s", name, out)
		}
	}
}

func TestResponsesStreamCutoverImmediateDeliveryNoEOFPastTerminal(t *testing.T) {
	upstream := rsCutFullTextTool(t)
	trailing := "data: {not valid json}\n\n"
	var frames []string
	for _, f := range strings.SplitAfter(upstream, "\n\n") {
		if strings.TrimSpace(f) != "" {
			frames = append(frames, f)
		}
	}
	rdr := &rsCutCountReader{chunks: append(frames, trailing)}
	var cb bytes.Buffer
	tr := &ResponsesToChatSSETranslator{PublicModel: "p", IncludeUsage: true, Limits: DefaultLimits()}
	if rerr := tr.Translate(rdr, &cb, nil); rerr != nil {
		t.Fatalf("translate: %v", rerr)
	}
	if rdr.idx != len(frames) {
		t.Fatalf("read past terminal: %d of %d", rdr.idx, len(frames)+1)
	}
	out := cb.String()
	if strings.Index(out, `"content":"hello"`) > strings.Index(out, `"finish_reason":"tool_calls"`) {
		t.Fatalf("text must precede terminal (immediate delivery): %s", out)
	}
	var gb bytes.Buffer
	rdr2 := &rsCutCountReader{chunks: append(frames, trailing)}
	gtr := &ResponsesToGeminiSSETranslator{PublicModel: "p", Limits: DefaultLimits()}
	if rerr := gtr.Translate(rdr2, &gb, nil); rerr != nil {
		t.Fatalf("gemini translate: %v", rerr)
	}
	if rdr2.idx != len(frames) {
		t.Fatalf("gemini read past terminal: %d", rdr2.idx)
	}
	if strings.Count(gb.String(), "functionCall") != 1 {
		t.Fatalf("gemini tool must emit exactly once: %s", gb.String())
	}
}

func TestResponsesStreamCutoverTruncationNoFabricatedSuccess(t *testing.T) {
	full := rsCutFullTextTool(t)
	cut := full[:strings.LastIndex(full, "event: response.completed")]
	for name, run := range map[string]func(string) (string, *RouterError){
		"chat": func(s string) (string, *RouterError) {
			var b bytes.Buffer
			rerr := (&ResponsesToChatSSETranslator{PublicModel: "p", Limits: DefaultLimits()}).Translate(strings.NewReader(s), &b, nil)
			return b.String(), rerr
		},
		"anthropic": func(s string) (string, *RouterError) {
			var b bytes.Buffer
			rerr := (&ResponsesToAnthropicSSETranslator{PublicModel: "p", Limits: DefaultLimits()}).Translate(strings.NewReader(s), &b, nil)
			return b.String(), rerr
		},
		"gemini-sse": func(s string) (string, *RouterError) {
			var b bytes.Buffer
			rerr := (&ResponsesToGeminiSSETranslator{PublicModel: "p", Limits: DefaultLimits()}).Translate(strings.NewReader(s), &b, nil)
			return b.String(), rerr
		},
		"gemini-array": func(s string) (string, *RouterError) {
			var b bytes.Buffer
			rerr := (&ResponsesToGeminiJSONArrayTranslator{PublicModel: "p", Limits: DefaultLimits()}).Translate(strings.NewReader(s), &b, nil)
			return b.String(), rerr
		},
	} {
		out, rerr := run(cut)
		if rerr == nil {
			t.Fatalf("%s truncation must fail", name)
		}
		if strings.Contains(out, "[DONE]") || strings.Contains(out, "message_stop") || strings.Contains(out, `"finishReason"`) {
			t.Fatalf("%s must not fabricate success: %s", name, out)
		}
		if strings.HasSuffix(out, "]") && name == "gemini-array" {
			t.Fatalf("array must not close on failure: %s", out)
		}
		out2, rerr := run(cut + "data: [DONE]\n\n")
		if rerr == nil {
			t.Fatalf("%s [DONE] without terminal must fail", name)
		}
		if strings.Contains(out2, "[DONE]") && name == "chat" {
			t.Fatalf("chat [DONE] truncation must not succeed: %s", out2)
		}
	}
}

type rsCutCountReader struct {
	chunks []string
	idx    int
}

func (r *rsCutCountReader) Read(p []byte) (int, error) {
	if r.idx >= len(r.chunks) {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[r.idx])
	r.idx++
	return n, nil
}

func TestResponsesStreamCutoverWriterFailureStopsReads(t *testing.T) {
	upstream := rsCutFullTextTool(t)
	var frames []string
	for _, f := range strings.SplitAfter(upstream, "\n\n") {
		if strings.TrimSpace(f) != "" {
			frames = append(frames, f)
		}
	}
	for name, mk := range map[string]func() (func(*rsCutCountReader, *rsCutFailWriter) *RouterError, *rsCutCountReader, *rsCutFailWriter){
		"chat": func() (func(*rsCutCountReader, *rsCutFailWriter) *RouterError, *rsCutCountReader, *rsCutFailWriter) {
			r := &rsCutCountReader{chunks: frames}
			w := &rsCutFailWriter{failAfter: 1}
			tr := &ResponsesToChatSSETranslator{PublicModel: "p", Limits: DefaultLimits()}
			return func(r *rsCutCountReader, w *rsCutFailWriter) *RouterError {
				return tr.Translate(r, w, nil)
			}, r, w
		},
		"anthropic": func() (func(*rsCutCountReader, *rsCutFailWriter) *RouterError, *rsCutCountReader, *rsCutFailWriter) {
			r := &rsCutCountReader{chunks: frames}
			w := &rsCutFailWriter{failAfter: 1}
			tr := &ResponsesToAnthropicSSETranslator{PublicModel: "p", Limits: DefaultLimits()}
			return func(r *rsCutCountReader, w *rsCutFailWriter) *RouterError {
				return tr.Translate(r, w, nil)
			}, r, w
		},
		"gemini": func() (func(*rsCutCountReader, *rsCutFailWriter) *RouterError, *rsCutCountReader, *rsCutFailWriter) {
			r := &rsCutCountReader{chunks: frames}
			w := &rsCutFailWriter{failAfter: 1}
			tr := &ResponsesToGeminiSSETranslator{PublicModel: "p", Limits: DefaultLimits()}
			return func(r *rsCutCountReader, w *rsCutFailWriter) *RouterError {
				return tr.Translate(r, w, nil)
			}, r, w
		},
	} {
		fn, r, w := mk()
		rerr := fn(r, w)
		if rerr == nil {
			t.Fatalf("%s writer failure must fail", name)
		}
		if r.idx >= len(frames) {
			t.Fatalf("%s reads continued past terminal after write failure: %d", name, r.idx)
		}
		if strings.Contains(w.buf.String(), "[DONE]") || strings.Contains(w.buf.String(), "message_stop") {
			t.Fatalf("%s must not succeed after write failure: %s", name, w.buf.String())
		}
	}
}

type rsCutFailWriter struct {
	n         int
	failAfter int
	buf       bytes.Buffer
}

func (w *rsCutFailWriter) Write(p []byte) (int, error) {
	w.n++
	if w.n > w.failAfter {
		return 0, errors.New("downstream gone")
	}
	return w.buf.Write(p)
}

func TestResponsesStreamCutoverReuse(t *testing.T) {
	upstream := rsCutFullTextTool(t)
	ctr := &ResponsesToChatSSETranslator{PublicModel: "p", IncludeUsage: true, Limits: DefaultLimits()}
	var a, b bytes.Buffer
	if rerr := ctr.Translate(strings.NewReader(upstream), &a, nil); rerr != nil {
		t.Fatalf("first: %v", rerr)
	}
	if rerr := ctr.Translate(strings.NewReader(upstream), &b, nil); rerr != nil {
		t.Fatalf("second: %v", rerr)
	}
	if a.String() == "" || b.String() == "" {
		t.Fatalf("reuse must emit both generations")
	}
	atr := &ResponsesToAnthropicSSETranslator{PublicModel: "p", Limits: DefaultLimits()}
	var c, d bytes.Buffer
	if rerr := atr.Translate(strings.NewReader(upstream), &c, nil); rerr != nil {
		t.Fatalf("anthropic first: %v", rerr)
	}
	if rerr := atr.Translate(strings.NewReader(upstream), &d, nil); rerr != nil {
		t.Fatalf("anthropic second: %v", rerr)
	}
	gtr := &ResponsesToGeminiSSETranslator{PublicModel: "p", Limits: DefaultLimits()}
	var e, f bytes.Buffer
	if rerr := gtr.Translate(strings.NewReader(upstream), &e, nil); rerr != nil {
		t.Fatalf("gemini first: %v", rerr)
	}
	if rerr := gtr.Translate(strings.NewReader(upstream), &f, nil); rerr != nil {
		t.Fatalf("gemini second: %v", rerr)
	}
	jtr := &ResponsesToGeminiJSONArrayTranslator{PublicModel: "p", Limits: DefaultLimits()}
	var g, h bytes.Buffer
	if rerr := jtr.Translate(strings.NewReader(upstream), &g, nil); rerr != nil {
		t.Fatalf("array first: %v", rerr)
	}
	if rerr := jtr.Translate(strings.NewReader(upstream), &h, nil); rerr != nil {
		t.Fatalf("array second: %v", rerr)
	}
}
