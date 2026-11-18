// Canonical Response IR: the single streaming pipeline.
//
// TranslateStream bridges one source decoder, the shared lifecycle
// validator, and one destination encoder which stay independent
// (decoder state, validation, and downstream commitment never share
// variables). Additive only: no server/codec callers yet.
//
// Backpressure: each framed unit is read, decoded, validated, encoded,
// and flushed before the next unit is read; retained bytes live only
// in a target-owned TerminalBuffer (or nothing at all for incremental
// encoders). No goroutines; a downstream write failure or failed
// validation stops upstream reads immediately with no replay.
//
// Terminal discipline: the pipeline stops reading as soon as the
// lifecycle observes the SOURCE terminal (CompletionEvent or
// FailureEvent). There is no read past a valid terminal, so a
// completion followed by additional reads cannot hang. Trailing bytes
// after the terminal are the same HTTP body's unread remainder (body
// closed, never another generation). For SSE sources the decoder's
// Close validates trailing state without reading further; for
// JSON-array sources the closing bracket `]` is framing consumed by
// the FrameSource (Closed=true) before the decoder emits success, so
// Close never reads either.
//
// Error finalization is single and consistent across ALL paths: one
// fail() helper aborts in-band exactly once when (and only when)
// downstream output was already committed, then returns the original
// error. In particular:
//   - frame-read, decode, lifecycle, Close, and encoder semantic
//     (representability/invalid-outgoing-frame) errors share the fail
//     path (Abort if committed, at most once);
//   - encoder downstream write/flush failures return directly with NO
//     Abort and NO further reads: the encoder owns post-write-failure
//     state, and an Abort after a failed write would be a secondary
//     write;
//   - Abort is called at most once per stream.
package protocol

import (
	"io"
)

// StreamDecoder decodes one typed frame at a time into semantic events.
// It owns only upstream source state (protocol position, event/type
// coherence, partial identities, running counters); it performs no
// downstream writes and enforces no destination policy. Event/type
// coherence checks (Anthropic envelope-type equality, Responses
// event/body coherence, Chat [DONE] handling, Gemini data-only frames)
// live here, per source contract.
//
// For JSON-array sources the decoder must only emit CompletionEvent
// when Frame.Closed is true (the `]` was consumed by the FrameSource
// before success is encoded); a missing `]` is truncation and must not
// emit a downstream terminal.
type StreamDecoder interface {
	DecodeFrame(frame Frame) ([]ResponseEvent, *RouterError)
	// Close validates the terminal state after the source terminal and
	// returns no events: usage must precede CompletionEvent. It
	// performs no upstream reads for any source: SSE terminals need no
	// EOF read and array `]` was already consumed via Frame.Closed.
	// Calling Close without a preceding source terminal is a truncation
	// error. After a decoder-emitted FailureEvent, Close must succeed
	// without further events (the decoder already finalized).
	Close() ([]ResponseEvent, *RouterError)
}

// EncodeError is the typed encoder failure. It distinguishes the two
// post-commit failure classes without string matching:
//   - WriteFailed=true: downstream write/flush failed. The pipeline
//     must NOT attempt Abort (a secondary write) and must not read
//     further. Err is the original downstream failure.
//   - WriteFailed=false: semantic representability / invalid outgoing
//     frame AFTER commitment (for example a late Refusal the target
//     cannot render, or a target frame over the size bound). The
//     pipeline routes this through fail(): one native Abort if
//     committed, preserving Err.
type EncodeError struct {
	Err         *RouterError
	WriteFailed bool
}

// EventEncoder encodes semantic events into one destination dialect.
// It owns only downstream state (commitment, block index mapping,
// framing); it performs no upstream reads and no source validation.
// Encode must be called with lifecycle-validated events in order.
// A target whose terminal envelope needs complete content keeps its
// own TerminalBuffer; the pipeline never retains on its behalf.
type EventEncoder interface {
	Encode(w io.Writer, flush func(), ev ResponseEvent) *EncodeError
	// Committed reports whether downstream headers/first bytes were
	// committed. The pipeline uses it to choose the failure path:
	// pre-commit failures become HTTP errors, post-commit failures
	// become in-band errors with no success marker.
	Committed() bool
	// Abort reports an in-band failure after commitment, or is a no-op
	// before commitment. It never emits a success terminal, and it
	// must be safe to call at most once per stream.
	Abort(w io.Writer, flush func())
}

// TranslateStream is the single streaming pipeline: read a frame,
// decode it, validate lifecycle order, encode each event, flush, and
// repeat until the source terminal. No whole-generation buffering
// beyond what the target encoder retains in its own TerminalBuffer,
// and no dry-run encode: one plan, one encode.
//
// frames is source-owned framing (SSE or incremental JSON array); the
// pipeline never constructs framing from the raw reader, so array `]`
// validation stays in framing/decoder before any terminal is encoded.
func TranslateStream(frames FrameSource, dec StreamDecoder, enc EventEncoder, w io.Writer, flush func(), lim Limits, source Dialect) *RouterError {
	lifecycle := NewResponseLifecycle(lim, source)
	aborted := false
	fail := func(rerr *RouterError) *RouterError {
		if rerr != nil && enc.Committed() && !aborted {
			aborted = true
			enc.Abort(w, flush)
		}
		return rerr
	}
	// feed validates then encodes each event in order. It reports
	// whether a failure was a downstream write (no Abort, no further
	// reads) or any other failure (fail path: Abort if committed).
	// Lifecycle failures, decoder failures, and encoder semantic
	// failures all funnel through fail(); only WriteFailed bypasses it.
	// A validated FailureEvent goes directly to fail(), which exclusively
	// owns failure output; it is never also sent to Encode.
	feed := func(evs []ResponseEvent) (rerr *RouterError, writeFailed bool) {
		for i, ev := range evs {
			if rerr := lifecycle.Observe(ev); rerr != nil {
				return rerr, false
			}
			// Failure finalization belongs to fail(), never both Encode
			// and Abort. Preserve ordinary pre-commit HTTP failures too.
			if lifecycle.Failed() {
				return lifecycle.Failure(), false
			}
			if _, terminal := ev.(CompletionEvent); terminal {
				if i != len(evs)-1 {
					return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent events after completion"}, false
				}
				// Validate final source state before encoding success.
				tail, rerr := dec.Close()
				if rerr != nil {
					return rerr, false
				}
				if len(tail) != 0 {
					return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent events after completion"}, false
				}
			}
			if eerr := enc.Encode(w, flush, ev); eerr != nil {
				if eerr.WriteFailed {
					return eerr.Err, true
				}
				return eerr.Err, false
			}
		}
		return nil, false
	}
	for {
		// Never read past the source terminal: a valid completion
		// ends upstream consumption immediately.
		if lifecycle.Completed() {
			if lifecycle.Failed() {
				return fail(lifecycle.Failure())
			}
			return nil
		}
		frame, rerr := frames.Next()
		if rerr != nil {
			return fail(rerr)
		}
		evs, rerr := dec.DecodeFrame(frame)
		if rerr != nil {
			return fail(rerr)
		}
		if rerr, writeFailed := feed(evs); rerr != nil {
			if writeFailed {
				return rerr
			}
			return fail(rerr)
		}
	}
}
