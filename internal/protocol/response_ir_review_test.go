package protocol

import (
	"bytes"
	"testing"
)

func TestReviewCloseErrorNoSuccess(t *testing.T) {
	lim := testRespLimits()
	done, _ := NewCompletionEvent(mustFinish(t, FinishStop))
	dec := &fakeStreamDecoder{frames: [][]ResponseEvent{{ResponseStart{ID: "r", Model: "m", Source: DialectChat}, UsageEvent{Usage: mustTerminalUsage(t, 2, 1)}, done}}, closeErr: &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "bad terminal state"}}
	enc := &bufferEncoder{}
	var out bytes.Buffer
	if e := TranslateStream(sseSource("data: x\n\n", lim), dec, enc, &out, nil, lim, DialectChat); e == nil {
		t.Fatal("close error swallowed")
	}
	for _, ev := range enc.events {
		if _, ok := ev.(CompletionEvent); ok {
			t.Fatal("success terminal was encoded before source Close failed")
		}
	}
}
func TestReviewFailureFinalizedOnce(t *testing.T) {
	lim := testRespLimits()
	dec := &fakeStreamDecoder{frames: [][]ResponseEvent{{ResponseStart{ID: "r", Model: "m", Source: DialectChat}, FailureEvent{Code: CodeUpstreamBad, Field: "upstream"}}}}
	enc := &bufferEncoder{}
	var out bytes.Buffer
	if e := TranslateStream(sseSource("data: x\n\n", lim), dec, enc, &out, nil, lim, DialectChat); e == nil {
		t.Fatal("failure swallowed")
	}
	finalizations := enc.aborts
	for _, ev := range enc.events {
		if _, ok := ev.(FailureEvent); ok {
			finalizations++
		}
	}
	if finalizations != 1 {
		t.Fatalf("native failure finalized %d times, want1", finalizations)
	}
}

func TestReviewCompletionMustEndFrame(t *testing.T) {
	lim := testRespLimits()
	done, _ := NewCompletionEvent(mustFinish(t, FinishStop))
	dec := &fakeStreamDecoder{frames: [][]ResponseEvent{{ResponseStart{ID: "r", Model: "m", Source: DialectChat}, UsageEvent{Usage: mustTerminalUsage(t, 2, 1)}, done, TextFragment{Item: "late", Text: "invalid"}}}}
	enc := &bufferEncoder{}
	var out bytes.Buffer
	if e := TranslateStream(sseSource("data: x\n\n", lim), dec, enc, &out, nil, lim, DialectChat); e == nil {
		t.Fatal("events after terminal accepted")
	}
	for _, ev := range enc.events {
		if _, ok := ev.(CompletionEvent); ok {
			t.Fatal("success was encoded before invalid trailing events")
		}
	}
}
