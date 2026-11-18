// Streaming pipeline tests: source-terminal discipline, single error
// finalization, backpressure, no reads past terminal.
//
// Every non-write error path (frame read, decode, lifecycle, Close,
// encoder semantic) shares one fail() with at-most-once Abort; encoder
// downstream write failures return with no Abort and no further reads.
// Fake decoders/encoders count calls to prove the pipeline never reads
// past a valid terminal and never duplicates errors.
package protocol

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

type fakeStreamDecoder struct {
	frames [][]ResponseEvent
	calls  int
	closed bool
	// closeErr, if set, fails Close (trailing-state validation).
	closeErr *RouterError
	// tail events returned by Close.
	tail []ResponseEvent
	// terminalSeen tracks whether the decoder observed its own
	// terminal; Close without a terminal is truncation.
	terminalSeen bool
}

func (f *fakeStreamDecoder) DecodeFrame(frame Frame) ([]ResponseEvent, *RouterError) {
	if f.calls >= len(f.frames) {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "unexpected frame"}
	}
	evs := f.frames[f.calls]
	f.calls++
	for _, ev := range evs {
		if _, ok := ev.(CompletionEvent); ok {
			f.terminalSeen = true
		}
		if _, ok := ev.(FailureEvent); ok {
			f.terminalSeen = true
		}
	}
	return evs, nil
}

func (f *fakeStreamDecoder) Close() ([]ResponseEvent, *RouterError) {
	f.closed = true
	if f.closeErr != nil {
		return nil, f.closeErr
	}
	if !f.terminalSeen {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream ended without completion"}
	}
	return f.tail, nil
}

type bufferEncoder struct {
	events    []ResponseEvent
	committed bool
	aborts    int
	// failOn, if > 0, fails the Encode call with that 1-based event
	// count to simulate a downstream write failure.
	failOn int
	// semanticFailOn, if > 0, fails with a semantic (representability)
	// error at that 1-based event count: Abort if committed.
	semanticFailOn int
	semanticErr    *RouterError
}

func (f *bufferEncoder) Encode(w io.Writer, flush func(), ev ResponseEvent) *EncodeError {
	f.events = append(f.events, ev)
	f.committed = true
	if f.failOn > 0 && len(f.events) == f.failOn {
		return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Downstream write failed"}, WriteFailed: true}
	}
	if f.semanticFailOn > 0 && len(f.events) == f.semanticFailOn {
		rerr := f.semanticErr
		if rerr == nil {
			rerr = &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream response feature \"refusal\" has no downstream representation"}
		}
		return &EncodeError{Err: rerr, WriteFailed: false}
	}
	return nil
}

func (f *bufferEncoder) Committed() bool { return f.committed }
func (f *bufferEncoder) Abort(w io.Writer, flush func()) {
	f.aborts++
}

func sseSource(raw string, lim Limits) FrameSource {
	return NewSSEFrameReader(strings.NewReader(raw), lim)
}

func TestResponseIRTranslateStreamPipeline(t *testing.T) {
	lim := testRespLimits()
	complete, _ := NewCompletionEvent(mustFinish(t, FinishStop))
	start := ResponseStart{ID: "r1", Model: "m", Source: DialectChat}
	bs, _ := NewBlockOpen("t0", BlockText, "", "")
	usage := UsageEvent{Usage: mustTerminalUsage(t, 5, 2)}
	dec := &fakeStreamDecoder{frames: [][]ResponseEvent{
		{start, bs},
		{TextFragment{Item: "t0", Text: "hello"}, BlockClose{Item: "t0"}},
		{usage, complete},
	}}
	enc := &bufferEncoder{}
	var out bytes.Buffer
	raw := "data: f1\n\ndata: f2\n\ndata: f3\n\n"
	rerr := TranslateStream(sseSource(raw, lim), dec, enc, &out, nil, lim, DialectChat)
	if rerr != nil {
		t.Fatalf("TranslateStream: %v", rerr)
	}
	if len(enc.events) != 6 {
		t.Fatalf("events forwarded = %d, want 6", len(enc.events))
	}
	if !dec.closed {
		t.Error("decoder Close not called at source terminal")
	}
	if enc.aborts != 0 {
		t.Error("Abort called on the success path")
	}
}

func TestResponseIRTranslateStreamStopsAtTerminal(t *testing.T) {
	lim := testRespLimits()
	complete, _ := NewCompletionEvent(mustFinish(t, FinishStop))
	start := ResponseStart{ID: "r1", Model: "m", Source: DialectChat}
	bs, _ := NewBlockOpen("t0", BlockText, "", "")
	usage := UsageEvent{Usage: mustTerminalUsage(t, 5, 2)}
	// The decoder reports completion on the FIRST frame. The pipeline
	// must not read again: a completion followed by additional reads
	// would hang. Trailing bytes are the same HTTP body's unread
	// remainder (body closed, never another generation) and stay unread.
	dec := &fakeStreamDecoder{frames: [][]ResponseEvent{
		{start, bs, TextFragment{Item: "t0", Text: "hi"}, BlockClose{Item: "t0"}, usage, complete},
	}}
	enc := &bufferEncoder{}
	var out bytes.Buffer
	raw := "data: only-frame-needed\n\ntrailing-bytes-are-same-body-remainder"
	rerr := TranslateStream(sseSource(raw, lim), dec, enc, &out, nil, lim, DialectChat)
	if rerr != nil {
		t.Fatalf("TranslateStream: %v", rerr)
	}
	if dec.calls != 1 {
		t.Fatalf("DecodeFrame calls = %d, want 1 (no read past terminal)", dec.calls)
	}
	if !dec.closed {
		t.Error("decoder Close not called after terminal")
	}
}

func TestResponseIRTranslateStreamDecodeError(t *testing.T) {
	lim := testRespLimits()
	// Decode error after commit: exactly one Abort.
	badDec := &failDecoder{}
	enc := &bufferEncoder{}
	var out bytes.Buffer
	rerr := TranslateStream(sseSource("data: f1\n\ndata: f2\n\n", lim), badDec, enc, &out, nil, lim, DialectChat)
	if rerr == nil {
		t.Fatal("decode error swallowed")
	}
	if !enc.committed || enc.aborts != 1 {
		t.Fatalf("post-commit decode error: committed=%v aborts=%d, want exactly one Abort", enc.committed, enc.aborts)
	}
}

func TestResponseIRTranslateStreamTruncation(t *testing.T) {
	lim := testRespLimits()
	// Frame error (truncation: EOF before terminal) after commit:
	// exactly one Abort, no duplicate error.
	dec := &fakeStreamDecoder{frames: [][]ResponseEvent{
		{ResponseStart{ID: "r1", Model: "m", Source: DialectChat}},
	}}
	enc := &bufferEncoder{}
	var out bytes.Buffer
	rerr := TranslateStream(sseSource("data: f1\n\n", lim), dec, enc, &out, nil, lim, DialectChat)
	if rerr == nil {
		t.Fatal("truncation swallowed")
	}
	if enc.aborts != 1 {
		t.Fatalf("post-commit truncation: aborts=%d, want 1", enc.aborts)
	}

	// Frame error before commit: no Abort (pre-commit failure maps to
	// an HTTP error, never an in-band abort).
	dec3 := &fakeStreamDecoder{}
	enc3 := &bufferEncoder{}
	var out3 bytes.Buffer
	rerr = TranslateStream(sseSource("", lim), dec3, enc3, &out3, nil, lim, DialectChat)
	if rerr == nil {
		t.Fatal("empty-stream truncation swallowed")
	}
	if enc3.aborts != 0 {
		t.Fatalf("pre-commit truncation aborted: aborts=%d", enc3.aborts)
	}
}

func TestResponseIRTranslateStreamWriteFailure(t *testing.T) {
	lim := testRespLimits()
	// Encode (write) failure: NO Abort, NO further reads. The encoder
	// owns post-write-failure state; an Abort would be a secondary
	// write after a downstream failure.
	complete, _ := NewCompletionEvent(mustFinish(t, FinishStop))
	start := ResponseStart{ID: "r1", Model: "m", Source: DialectChat}
	bs, _ := NewBlockOpen("t0", BlockText, "", "")
	usage := UsageEvent{Usage: mustTerminalUsage(t, 5, 2)}
	dec := &fakeStreamDecoder{frames: [][]ResponseEvent{
		{start, bs},
		{TextFragment{Item: "t0", Text: "hello"}, BlockClose{Item: "t0"}},
		{usage, complete},
	}}
	enc := &bufferEncoder{failOn: 2}
	var out bytes.Buffer
	rerr := TranslateStream(sseSource("data: f1\n\ndata: f2\n\ndata: f3\n\n", lim), dec, enc, &out, nil, lim, DialectChat)
	if rerr == nil {
		t.Fatal("write failure swallowed")
	}
	if rerr.Msg != "Downstream write failed" {
		t.Fatalf("original write error not preserved: %v", rerr)
	}
	if enc.aborts != 0 {
		t.Fatalf("Abort after write failure: aborts=%d (secondary write)", enc.aborts)
	}
	// Frame 1 decodes to two events; the write fails on the second.
	// The pipeline must return without reading frame 2.
	if dec.calls != 1 {
		t.Fatalf("reads continued past write failure: DecodeFrame calls=%d", dec.calls)
	}
}

func TestResponseIRTranslateStreamLateSemanticAbortsOnce(t *testing.T) {
	lim := testRespLimits()
	// Late representability failure AFTER commit (for example a Refusal
	// the target cannot render incrementally): exactly one native Abort,
	// original error preserved, no further reads. This is the
	// distinction from a downstream write failure (no Abort).
	complete, _ := NewCompletionEvent(mustFinish(t, FinishStop))
	start := ResponseStart{ID: "r1", Model: "m", Source: DialectChat}
	bs, _ := NewBlockOpen("t0", BlockText, "", "")
	usage := UsageEvent{Usage: mustTerminalUsage(t, 5, 2)}
	_ = complete
	_ = usage
	dec := &fakeStreamDecoder{frames: [][]ResponseEvent{
		{start, bs},
		{TextFragment{Item: "t0", Text: "hello"}, BlockClose{Item: "t0"}},
		{UsageEvent{Usage: mustTerminalUsage(t, 5, 2)}},
	}}
	want := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream response feature \"refusal\" has no downstream representation"}
	enc := &bufferEncoder{semanticFailOn: 3, semanticErr: want}
	var out bytes.Buffer
	rerr := TranslateStream(sseSource("data: f1\n\ndata: f2\n\ndata: f3\n\n", lim), dec, enc, &out, nil, lim, DialectChat)
	if rerr == nil {
		t.Fatal("late semantic failure swallowed")
	}
	if rerr != want {
		t.Fatalf("original semantic error not preserved: got %v want %v", rerr, want)
	}
	if enc.aborts != 1 {
		t.Fatalf("late semantic failure: aborts=%d, want exactly 1", enc.aborts)
	}
	if dec.calls != 2 {
		t.Fatalf("reads continued past semantic failure: DecodeFrame calls=%d, want 2", dec.calls)
	}
}

func TestResponseIRTranslateStreamPrecommitSemanticNoAbort(t *testing.T) {
	lim := testRespLimits()
	// Semantic failure before any downstream commit: no Abort
	// (pre-commit maps to an HTTP error), original error preserved.
	dec := &fakeStreamDecoder{frames: [][]ResponseEvent{
		{ResponseStart{ID: "r1", Model: "m", Source: DialectChat}},
	}}
	want := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream response feature \"x\" has no downstream representation"}
	enc := &bufferEncoder{semanticFailOn: 1, semanticErr: want}
	// Ensure the encoder reports not-committed before the failing Encode.
	enc.committed = false
	preEnc := &precommitSemanticEncoder{err: want}
	var out bytes.Buffer
	rerr := TranslateStream(sseSource("data: f1\n\n", lim), dec, preEnc, &out, nil, lim, DialectChat)
	if rerr == nil {
		t.Fatal("precommit semantic failure swallowed")
	}
	if rerr != want {
		t.Fatalf("original error not preserved: got %v", rerr)
	}
	if preEnc.aborts != 0 {
		t.Fatalf("precommit semantic failure aborted: aborts=%d", preEnc.aborts)
	}
}

type precommitSemanticEncoder struct {
	err    *RouterError
	aborts int
}

func (e *precommitSemanticEncoder) Encode(w io.Writer, flush func(), ev ResponseEvent) *EncodeError {
	return &EncodeError{Err: e.err, WriteFailed: false}
}
func (e *precommitSemanticEncoder) Committed() bool                 { return false }
func (e *precommitSemanticEncoder) Abort(w io.Writer, flush func()) { e.aborts++ }

func TestResponseIRTranslateStreamFailureEventPrecommit(t *testing.T) {
	lim := testRespLimits()
	// A validated FailureEvent before downstream commit must return its
	// failure error (never nil success) with no Abort.
	dec := &failureContractDecoder{events: []ResponseEvent{
		ResponseStart{ID: "r1", Model: "m", Source: DialectChat},
		FailureEvent{Code: CodeUpstreamBad, Field: "response.output"},
	}}
	enc := &neverCommitEncoder{}
	var out bytes.Buffer
	rerr := TranslateStream(sseSource("data: f1\n\n", lim), dec, enc, &out, nil, lim, DialectChat)
	if rerr == nil {
		t.Fatal("precommit FailureEvent returned nil success")
	}
	if enc.aborts != 0 {
		t.Fatalf("precommit FailureEvent aborted: aborts=%d", enc.aborts)
	}
}

type neverCommitEncoder struct {
	events []ResponseEvent
	aborts int
}

func (e *neverCommitEncoder) Encode(w io.Writer, flush func(), ev ResponseEvent) *EncodeError {
	e.events = append(e.events, ev)
	return nil
}
func (e *neverCommitEncoder) Committed() bool                 { return false }
func (e *neverCommitEncoder) Abort(w io.Writer, flush func()) { e.aborts++ }

func TestResponseIRTranslateStreamFailureEventPostcommit(t *testing.T) {
	lim := testRespLimits()
	// FailureEvent after commit: exactly one Abort, failure error
	// preserved.
	start := ResponseStart{ID: "r1", Model: "m", Source: DialectChat}
	bs, _ := NewBlockOpen("t0", BlockText, "", "")
	dec := &failureContractDecoder{events: []ResponseEvent{
		start, bs, TextFragment{Item: "t0", Text: "hi"},
		FailureEvent{Code: CodeUpstreamBad, Field: "response.output"},
	}}
	enc := &bufferEncoder{}
	var out bytes.Buffer
	rerr := TranslateStream(sseSource("data: f1\n\n", lim), dec, enc, &out, nil, lim, DialectChat)
	if rerr == nil {
		t.Fatal("postcommit FailureEvent swallowed")
	}
	if enc.aborts != 1 {
		t.Fatalf("postcommit FailureEvent: aborts=%d, want 1", enc.aborts)
	}
}

type failureContractDecoder struct {
	events []ResponseEvent
	used   bool
}

func (f *failureContractDecoder) DecodeFrame(frame Frame) ([]ResponseEvent, *RouterError) {
	if f.used {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "unexpected frame"}
	}
	f.used = true
	return f.events, nil
}
func (f *failureContractDecoder) Close() ([]ResponseEvent, *RouterError) { return nil, nil }

func TestResponseIRTranslateStreamCloseError(t *testing.T) {
	lim := testRespLimits()
	// Close error after terminal: exactly one Abort.
	complete, _ := NewCompletionEvent(mustFinish(t, FinishStop))
	start := ResponseStart{ID: "r1", Model: "m", Source: DialectChat}
	bs, _ := NewBlockOpen("t0", BlockText, "", "")
	usage := UsageEvent{Usage: mustTerminalUsage(t, 5, 2)}
	dec := &fakeStreamDecoder{
		frames:   [][]ResponseEvent{{start, bs, TextFragment{Item: "t0", Text: "hi"}, BlockClose{Item: "t0"}, usage, complete}},
		closeErr: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream ended without completion"},
	}
	enc := &bufferEncoder{}
	var out bytes.Buffer
	rerr := TranslateStream(sseSource("data: f1\n\n", lim), dec, enc, &out, nil, lim, DialectChat)
	if rerr == nil {
		t.Fatal("Close error swallowed")
	}
	if enc.aborts != 1 {
		t.Fatalf("post-commit Close error: aborts=%d, want 1", enc.aborts)
	}
}

func TestResponseIRTranslateStreamOrderViolation(t *testing.T) {
	lim := testRespLimits()
	// Source-order violation (duplicate start) after commit: fail
	// path, exactly one Abort, never a duplicate error.
	start := ResponseStart{ID: "r1", Model: "m", Source: DialectChat}
	dec := &fakeStreamDecoder{frames: [][]ResponseEvent{
		{start},
		{start},
	}}
	enc := &bufferEncoder{}
	var out bytes.Buffer
	rerr := TranslateStream(sseSource("data: f1\n\ndata: f2\n\n", lim), dec, enc, &out, nil, lim, DialectChat)
	if rerr == nil {
		t.Fatal("order violation swallowed")
	}
	if enc.aborts != 1 {
		t.Fatalf("post-commit order violation: aborts=%d, want 1", enc.aborts)
	}
}

// failDecoder commits one event, then fails the next DecodeFrame.
type failDecoder struct {
	n int
}

func (f *failDecoder) DecodeFrame(frame Frame) ([]ResponseEvent, *RouterError) {
	f.n++
	if f.n == 1 {
		return []ResponseEvent{ResponseStart{ID: "r1", Model: "m", Source: DialectChat}}, nil
	}
	return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent an unrepresentable event"}
}

func (f *failDecoder) Close() ([]ResponseEvent, *RouterError) { return nil, nil }

// errWriter fails all writes: proves no secondary write follows.
type errWriter struct{ writes int }

func (w *errWriter) Write(p []byte) (int, error) {
	w.writes++
	return 0, errors.New("downstream write failed")
}

func TestResponseIRTranslateStreamWriteFailureNoExtraWrite(t *testing.T) {
	lim := testRespLimits()
	start := ResponseStart{ID: "r1", Model: "m", Source: DialectChat}
	dec := &fakeStreamDecoder{frames: [][]ResponseEvent{{start}}}
	we := &writeEncoder{}
	w := &errWriter{}
	rerr := TranslateStream(sseSource("data: f1\n\n", lim), dec, we, w, nil, lim, DialectChat)
	if rerr == nil {
		t.Fatal("write failure swallowed")
	}
	if w.writes != 1 {
		t.Fatalf("writes=%d, want exactly 1 (no secondary write after failure)", w.writes)
	}
	if dec.calls != 1 {
		t.Fatalf("DecodeFrame calls=%d after write failure, want no further reads", dec.calls)
	}
	if we.aborts != 0 {
		t.Fatalf("Abort after write failure: aborts=%d", we.aborts)
	}
}

type writeEncoder struct {
	committed bool
	aborts    int
}

func (e *writeEncoder) Encode(w io.Writer, flush func(), ev ResponseEvent) *EncodeError {
	if _, err := w.Write([]byte("chunk")); err != nil {
		return &EncodeError{Err: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Downstream write failed"}, WriteFailed: true}
	}
	e.committed = true
	return nil
}

func (e *writeEncoder) Committed() bool { return e.committed }
func (e *writeEncoder) Abort(w io.Writer, flush func()) {
	e.aborts++
}

// Array-source probe: truncation (missing `]`) must not emit a terminal.
// The decoder only emits Completion when Frame.Closed is true.
type arrayTerminalDecoder struct {
	elems   int
	started bool
	usage   ResponseUsage
	closed  bool
}

func (d *arrayTerminalDecoder) DecodeFrame(frame Frame) ([]ResponseEvent, *RouterError) {
	var evs []ResponseEvent
	if !d.started {
		d.started = true
		d.usage = ResponseUsage{Prompt: intPtr(5), Completion: intPtr(2), Total: intPtr(7), Provenance: UsageProvenance{Source: DialectGemini, Scope: UsageTerminal}}
		evs = append(evs, ResponseStart{ID: "r1", Model: "m", Source: DialectGemini})
	}
	if frame.Data != "" {
		d.elems++
	}
	// Object delivery is immediate; only a later bare close can complete.
	if frame.Closed {
		if frame.Data != "" || d.elems == 0 {
			return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream ended without completion"}
		}
		d.closed = true
		fin, _ := NewFinish(FinishStop, "STOP")
		complete, _ := NewCompletionEvent(fin)
		evs = append(evs, UsageEvent{Usage: d.usage}, complete)
	}
	return evs, nil
}

func (d *arrayTerminalDecoder) Close() ([]ResponseEvent, *RouterError) {
	if !d.closed {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream ended without completion"}
	}
	return nil, nil
}

type recordEncoder struct {
	events    []ResponseEvent
	committed bool
	aborts    int
}

func (e *recordEncoder) Encode(w io.Writer, flush func(), ev ResponseEvent) *EncodeError {
	e.events = append(e.events, ev)
	e.committed = true
	return nil
}
func (e *recordEncoder) Committed() bool                 { return e.committed }
func (e *recordEncoder) Abort(w io.Writer, flush func()) { e.aborts++ }

func hasCompletion(evs []ResponseEvent) bool {
	for _, ev := range evs {
		if _, ok := ev.(CompletionEvent); ok {
			return true
		}
	}
	return false
}

func TestResponseIRTranslateStreamArrayTruncationNoTerminal(t *testing.T) {
	lim := testRespLimits()
	// Array missing `]`: framing truncates before any terminal is
	// encoded downstream.
	raw := `[{"a":1},`
	frames := NewJSONArrayFrameReader(strings.NewReader(raw), lim)
	dec := &arrayTerminalDecoder{}
	enc := &recordEncoder{}
	var out bytes.Buffer
	rerr := TranslateStream(frames, dec, enc, &out, nil, lim, DialectGemini)
	if rerr == nil {
		t.Fatal("array truncation swallowed")
	}
	if hasCompletion(enc.events) {
		t.Fatalf("truncated array emitted downstream terminal: %+v", enc.events)
	}
	if enc.aborts != 1 {
		t.Fatalf("truncation after commit: aborts=%d, want 1", enc.aborts)
	}
}

func TestResponseIRTranslateStreamArraySuccessNeedsClose(t *testing.T) {
	lim := testRespLimits()
	raw := `[{"a":1}]`
	frames := NewJSONArrayFrameReader(strings.NewReader(raw), lim)
	dec := &arrayTerminalDecoder{}
	enc := &recordEncoder{}
	var out bytes.Buffer
	rerr := TranslateStream(frames, dec, enc, &out, nil, lim, DialectGemini)
	if rerr != nil {
		t.Fatalf("array success: %v", rerr)
	}
	if !hasCompletion(enc.events) {
		t.Fatalf("array success emitted no terminal: %+v", enc.events)
	}
	if enc.aborts != 0 {
		t.Fatalf("success path aborted: %d", enc.aborts)
	}
}
