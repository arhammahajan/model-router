// SSE framing tests: event metadata preserved, [DONE] passed through,
// truncation strict, reads strict, bounds enforced.
package protocol

import (
	"io"
	"strings"
	"testing"
	"time"
)

func TestResponseIRFrameReaderSSE(t *testing.T) {
	lim := testRespLimits()
	// Coalesced multi-line data, event names preserved, comments and
	// keepalives skipped, [DONE] passed through as ordinary data (it is
	// Chat termination only; the SOURCE decoder decides).
	raw := ": keepalive\n\nevent: message_start\ndata: {\"a\":1}\n\ndata: line1\ndata: line2\n\ndata: [DONE]\n\n"
	fr := NewSSEFrameReader(strings.NewReader(raw), lim)
	f1, rerr := fr.Next()
	if rerr != nil || !f1.HasEvent || f1.Event != "message_start" || f1.Data != `{"a":1}` {
		t.Fatalf("frame1 = %+v %v", f1, rerr)
	}
	f2, rerr := fr.Next()
	if rerr != nil || f2.HasEvent || f2.Data != "line1\nline2" {
		t.Fatalf("frame2 = %+v %v", f2, rerr)
	}
	// [DONE] is NOT a framing terminal: it arrives as a data-only frame.
	f3, rerr := fr.Next()
	if rerr != nil || f3.HasEvent || f3.Data != "[DONE]" {
		t.Fatalf("frame3 ([DONE] passthrough) = %+v %v", f3, rerr)
	}
	// Clean EOF with no semantic terminal is truncation, never success.
	if _, rerr := fr.Next(); rerr == nil {
		t.Error("clean EOF accepted as a frame")
	}
	// Strict reader: errors on extra reads keep erroring, never hang
	// or succeed.
	if _, rerr := fr.Next(); rerr == nil {
		t.Error("extra read past EOF accepted")
	}
}

func TestResponseIRFrameReaderEventOnly(t *testing.T) {
	lim := testRespLimits()
	// Event-only frames (ping) are returned; the source decoder owns
	// ping handling (Anthropic ignores ping; others decide per contract).
	fr := NewSSEFrameReader(strings.NewReader("event: ping\n\n"), lim)
	f, rerr := fr.Next()
	if rerr != nil {
		t.Fatalf("ping frame: %v", rerr)
	}
	if !f.HasEvent || f.Event != "ping" || f.Data != "" {
		t.Fatalf("ping frame mangled: %+v", f)
	}
	// Event/type mismatch is NOT checked here: framing preserves both
	// for the source decoder (Responses->Chat r2csHandleFrame proves
	// the coherence check belongs downstream of framing).
	fr2 := NewSSEFrameReader(strings.NewReader("event: response.created\ndata: {\"type\":\"other\"}\n\n"), lim)
	f2, rerr := fr2.Next()
	if rerr != nil {
		t.Fatalf("mismatched frame: %v", rerr)
	}
	if f2.Event != "response.created" || f2.Data != `{"type":"other"}` {
		t.Fatalf("event/data not preserved verbatim: %+v", f2)
	}
}

func TestResponseIRFrameReaderBounds(t *testing.T) {
	lim := testRespLimits()
	// Oversize frame fails.
	fr := NewSSEFrameReader(strings.NewReader("data: "+strings.Repeat("z", 2048)+"\n\n"), lim)
	if _, rerr := fr.Next(); rerr == nil {
		t.Error("oversize SSE frame accepted")
	}
	// Mid-frame EOF is truncation.
	fr2 := NewSSEFrameReader(strings.NewReader("data: {\"a\":1}\n"), lim)
	if _, rerr := fr2.Next(); rerr == nil {
		t.Error("mid-frame EOF accepted")
	}
	// Keepalives never accumulate toward the bound.
	var sb strings.Builder
	for i := 0; i < 100; i++ {
		sb.WriteString(": keepalive\n\n")
	}
	sb.WriteString("data: ok\n\n")
	fr3 := NewSSEFrameReader(strings.NewReader(sb.String()), lim)
	f, rerr := fr3.Next()
	if rerr != nil || f.Data != "ok" {
		t.Fatalf("keepalive-skipped frame = %+v %v", f, rerr)
	}
}

func TestResponseIRJSONArrayFrameReader(t *testing.T) {
	lim := testRespLimits()
	// Two elements then an explicit bare close: incremental, bounded,
	// Closed only on the bare `]` frame.
	raw := "[{\"a\":1}, {\"b\":2}]"
	fr := NewJSONArrayFrameReader(strings.NewReader(raw), lim)
	f1, rerr := fr.Next()
	if rerr != nil || f1.Data != `{"a":1}` || f1.Closed {
		t.Fatalf("elem1 = %+v %v", f1, rerr)
	}
	// The first delta was emitted without requiring the separator: the
	// reader returns after `}` and validates `,` on the next call.
	f2, rerr := fr.Next()
	if rerr != nil || f2.Data != `{"b":2}` || f2.Closed {
		t.Fatalf("elem2 = %+v %v", f2, rerr)
	}
	f3, rerr := fr.Next()
	if rerr != nil || !f3.Closed || f3.Data != "" {
		t.Fatalf("close = %+v %v", f3, rerr)
	}
	if _, rerr := fr.Next(); rerr == nil {
		t.Error("read past close accepted")
	}
}

func TestResponseIRJSONArrayTruncationAndTrailingComma(t *testing.T) {
	lim := testRespLimits()
	// EOF before `]` is truncation, never success.
	fr := NewJSONArrayFrameReader(strings.NewReader(`[{"a":1},`), lim)
	if _, rerr := fr.Next(); rerr != nil {
		t.Fatalf("elem1: %v", rerr)
	}
	if _, rerr := fr.Next(); rerr == nil {
		t.Error("EOF before `]` accepted")
	}
	// Trailing comma `[elem,]` is invalid JSON, never a terminal.
	fr2 := NewJSONArrayFrameReader(strings.NewReader(`[{"a":1},]`), lim)
	if _, rerr := fr2.Next(); rerr != nil {
		t.Fatalf("elem1: %v", rerr)
	}
	if _, rerr := fr2.Next(); rerr == nil {
		t.Error("trailing comma accepted")
	}
	// Empty array `[]` is a bare close with no finish (decoder must
	// treat it as truncation, not success).
	fr3 := NewJSONArrayFrameReader(strings.NewReader(`[]`), lim)
	f, rerr := fr3.Next()
	if rerr != nil || !f.Closed || f.Data != "" {
		t.Fatalf("bare close = %+v %v", f, rerr)
	}
	// Oversize element fails before any semantic handling.
	fr4 := NewJSONArrayFrameReader(strings.NewReader(`[`+strings.Repeat(" ", 8)+`{"a":"`+strings.Repeat("z", 2048)+`"}`+`]`), lim)
	if f, rerr := fr4.Next(); rerr == nil || f.Data != "" || f.Closed {
		t.Errorf("oversize array element accepted value-free violation: %+v %v", f, rerr)
	}
	// Braces inside strings never affect depth: element first, then the
	// explicit bare close.
	fr5 := NewJSONArrayFrameReader(strings.NewReader(`[{"t":"}{[,]\""}]`), lim)
	f5, rerr := fr5.Next()
	if rerr != nil || f5.Closed || f5.Data == "" {
		t.Fatalf("string-brace element = %+v %v", f5, rerr)
	}
	f5c, rerr := fr5.Next()
	if rerr != nil || !f5c.Closed || f5c.Data != "" {
		t.Fatalf("string-brace close = %+v %v", f5c, rerr)
	}
	// Missing separator between elements is invalid JSON.
	fr6 := NewJSONArrayFrameReader(strings.NewReader(`[{"a":1} {"b":2}]`), lim)
	if _, rerr := fr6.Next(); rerr != nil {
		t.Fatalf("elem1: %v", rerr)
	}
	if f, rerr := fr6.Next(); rerr == nil || f.Data != "" || f.Closed {
		t.Errorf("missing separator accepted value-free violation: %+v %v", f, rerr)
	}
	// Empty malformed framing is invalid, never a bare close.
	for _, bad := range []string{`[}`, `[,]`, `[1]`, `["s"]`, `[`} {
		frb := NewJSONArrayFrameReader(strings.NewReader(bad), lim)
		if f, rerr := frb.Next(); rerr == nil || f.Data != "" || f.Closed {
			t.Errorf("%q accepted value-free violation: %+v %v", bad, f, rerr)
		}
	}
}

func TestResponseIRJSONArrayFirstDeltaIncremental(t *testing.T) {
	lim := testRespLimits()
	// Proves incremental framing: the first element returns while the
	// upstream body is still open (pipe writer blocks after one
	// element until the reader consumes it).
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte(`[{"a":1},`))
		// Keep the body open: do not close yet. The reader must have
		// already returned the first element.
		_, _ = pw.Write([]byte(`{"b":2}]`))
		_ = pw.Close()
	}()
	fr := NewJSONArrayFrameReader(pr, lim)
	f1, rerr := fr.Next()
	if rerr != nil || f1.Data != `{"a":1}` || f1.Closed {
		t.Fatalf("incremental elem1 = %+v %v", f1, rerr)
	}
	f2, rerr := fr.Next()
	if rerr != nil || f2.Data != `{"b":2}` || f2.Closed {
		t.Fatalf("incremental elem2 = %+v %v", f2, rerr)
	}
	f3, rerr := fr.Next()
	if rerr != nil || !f3.Closed || f3.Data != "" {
		t.Fatalf("incremental close = %+v %v", f3, rerr)
	}
}

func TestResponseIRJSONArrayImmediateDeliveryGated(t *testing.T) {
	lim := testRespLimits()
	// Gated EOF: a complete object arrives but the separator never does.
	// The old reader discarded the object (truncation on the first call);
	// the fixed reader delivers it immediately and fails only on the
	// next call when `]` never arrives. Failures stay value-free.
	fr := NewJSONArrayFrameReader(strings.NewReader(`[{"a":1}`), lim)
	f1, rerr := fr.Next()
	if rerr != nil || f1.Data != `{"a":1}` || f1.Closed {
		t.Fatalf("gated elem = %+v %v", f1, rerr)
	}
	if f, rerr := fr.Next(); rerr == nil || f.Data != "" || f.Closed {
		t.Fatalf("gated close accepted value-free violation: %+v %v", f, rerr)
	}
}

func TestResponseIRJSONArrayImmediateDeliveryStalled(t *testing.T) {
	lim := testRespLimits()
	// Gated stall: the separator never arrives AND the body stays open
	// (no EOF). The fixed reader must still return the complete object
	// promptly; waiting for `,`/`]` would hang and delay first text.
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte(`[{"a":1}`))
		// Deliberately withhold the separator and keep the body open.
	}()
	fr := NewJSONArrayFrameReader(pr, lim)
	type result struct {
		f    Frame
		rerr *RouterError
	}
	done := make(chan result, 1)
	go func() {
		f, rerr := fr.Next()
		done <- result{f: f, rerr: rerr}
	}()
	select {
	case got := <-done:
		if got.rerr != nil || got.f.Data != `{"a":1}` || got.f.Closed {
			t.Fatalf("stalled elem = %+v %v", got.f, got.rerr)
		}
	case <-time.After(2 * time.Second):
		_ = pw.Close()
		t.Fatal("stalled separator blocked object delivery")
	}
	_ = pw.Close()
}

func TestResponseIRJSONArrayBoundExact(t *testing.T) {
	// Exact MaxFrameBytes element succeeds; one byte over fails. The
	// bound covers the element only; the bare `]` close carries no Data.
	base := `{"a":"` + `"}`
	pad := testRespLimits().MaxFrameBytes - len(base)
	if pad < 0 {
		t.Fatal("test bound smaller than envelope")
	}
	elem := `{"a":"` + strings.Repeat("z", pad) + `"}`
	if len(elem) != testRespLimits().MaxFrameBytes {
		t.Fatalf("elem len = %d", len(elem))
	}
	fr := NewJSONArrayFrameReader(strings.NewReader(`[`+elem+`]`), testRespLimits())
	f1, rerr := fr.Next()
	if rerr != nil || f1.Data != elem || f1.Closed {
		t.Fatalf("exact-bound elem = %+v %v", f1, rerr)
	}
	fc, rerr := fr.Next()
	if rerr != nil || !fc.Closed || fc.Data != "" {
		t.Fatalf("exact-bound close = %+v %v", fc, rerr)
	}
	over := `{"a":"` + strings.Repeat("z", pad+1) + `"}`
	fr2 := NewJSONArrayFrameReader(strings.NewReader(`[`+over+`]`), testRespLimits())
	if f, rerr := fr2.Next(); rerr == nil || f.Data != "" || f.Closed {
		t.Fatalf("bound+1 accepted value-free violation: %+v %v", f, rerr)
	}
}

func TestResponseIRJSONArrayNoOverreadPastClose(t *testing.T) {
	lim := testRespLimits()
	// The close returns promptly without an EOF read: the body stays
	// open after `]` and the reader must not block or consume past it.
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte(`[{"a":1}]`))
		// Keep the body open: no EOF, no trailing generation.
	}()
	fr := NewJSONArrayFrameReader(pr, lim)
	type result struct {
		f    Frame
		rerr *RouterError
	}
	next := func() result {
		ch := make(chan result, 1)
		go func() {
			f, rerr := fr.Next()
			ch <- result{f: f, rerr: rerr}
		}()
		select {
		case got := <-ch:
			return got
		case <-time.After(2 * time.Second):
			_ = pw.Close()
			t.Fatal("read past `]` blocked without EOF")
			return result{}
		}
	}
	g1 := next()
	if g1.rerr != nil || g1.f.Data != `{"a":1}` || g1.f.Closed {
		t.Fatalf("elem = %+v %v", g1.f, g1.rerr)
	}
	g2 := next()
	if g2.rerr != nil || !g2.f.Closed || g2.f.Data != "" {
		t.Fatalf("close = %+v %v", g2.f, g2.rerr)
	}
	_ = pw.Close()
}
