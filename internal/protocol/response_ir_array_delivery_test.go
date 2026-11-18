// Array delivery regression: the shared JSONArrayFrameReader must deliver
// each complete object immediately and gate success on the explicit bare
// `]` close, so truncation can never emit a downstream terminal.
package protocol

import (
	"bytes"
	"strings"
	"testing"
)

func TestResponseIRArrayDeliverySuccessNeedsClose(t *testing.T) {
	lim := testRespLimits()
	frames := NewJSONArrayFrameReader(strings.NewReader(`[{"a":1},{"b":2}]`), lim)
	dec := &arrayTerminalDecoder{}
	enc := &recordEncoder{}
	var out bytes.Buffer
	if rerr := TranslateStream(frames, dec, enc, &out, nil, lim, DialectGemini); rerr != nil {
		t.Fatalf("array success: %v", rerr)
	}
	if !hasCompletion(enc.events) {
		t.Fatalf("array success emitted no terminal: %+v", enc.events)
	}
	if enc.aborts != 0 {
		t.Fatalf("success path aborted: %d", enc.aborts)
	}
}

func TestResponseIRArrayDeliveryTruncationNoTerminal(t *testing.T) {
	lim := testRespLimits()
	// Missing `]`: framing truncates before any terminal is encoded.
	frames := NewJSONArrayFrameReader(strings.NewReader(`[{"a":1},`), lim)
	dec := &arrayTerminalDecoder{}
	enc := &recordEncoder{}
	var out bytes.Buffer
	if rerr := TranslateStream(frames, dec, enc, &out, nil, lim, DialectGemini); rerr == nil {
		t.Fatal("array truncation swallowed")
	}
	if hasCompletion(enc.events) {
		t.Fatalf("truncated array emitted downstream terminal: %+v", enc.events)
	}
	if enc.aborts != 1 {
		t.Fatalf("truncation after commit: aborts=%d, want 1", enc.aborts)
	}
}
