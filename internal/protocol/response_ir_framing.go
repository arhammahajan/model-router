// Canonical Response IR: SSE framing with preserved event metadata.
//
// Framing is transport shape, not semantics: this file splits a Server-
// Sent Events byte stream into typed frames and bounds them. It never
// interprets payloads. In particular:
//
//   - The SSE `event:` field is preserved in the typed Frame. Existing
//     translators prove it is semantic, not ignorable metadata:
//     Anthropic->Chat requires a nonempty event name matching the body
//     type (a2rCheckEnvelopeType / handleEvent), and Responses->Chat
//     fails a frame whose event name contradicts its body type. A
//     framing layer that throws the event away regresses those proven
//     guards, so the SOURCE decoder checks event/type/lifecycle
//     coherence; framing only carries the name, bounded by the frame
//     bound like every other framing byte.
//   - `[DONE]` is Chat termination ONLY, not a universal terminal. A
//     Responses upstream `[DONE]` without a valid terminal is a
//     truncation (chat_responses_stream.go); a Gemini upstream `[DONE]`
//     before the finish chunk is a truncation (gemini_to_chat_stream_sse.go);
//     Anthropic has no `[DONE]` (message_stop terminates). Framing
//     therefore passes `[DONE]` through as ordinary data; each source
//     decoder decides what it means.
//   - Empty ping / event-only / comment / keepalive handling stays
//     source-specific: framing returns event-only frames (e.g.
//     `event: ping` with no data) and lets the source decoder ignore or
//     reject them per its contract. Pure comments and blank keepalives
//     are skipped without accumulating toward the frame bound, matching
//     all four existing translators.
//   - Clean EOF is a truncation for SSE: every SSE dialect terminates
//     semantically (finish chunk, message_stop, response.completed,
//     Chat [DONE]), never by connection close. EOF before the source
//     terminal is reported here so the pipeline fails instead of
//     emitting a synthetic success.
//
// JSON-array streaming (Gemini) shares the bounded incremental reader
// below (JSONArrayFrameReader): array element boundaries require
// JSON-balanced reads coupled to the `]` terminal, and the existing
// Gemini translators already own that framing pattern. The shared
// reader arrives here as framing only; no existing codec stream is cut
// over to it in this revision.
package protocol

import (
	"bufio"
	"io"
	"strings"
)

// Frame is one typed SSE frame: the coalesced data payload plus the
// frame's event name. HasEvent distinguishes "event: present" from
// "no event field" so source decoders can enforce their own
// event-required vs data-only contracts. Neither field is interpreted
// here.
//
// Closed is framing-terminal metadata for JSON-array sources only (SSE
// always leaves it false): true means this bare frame IS the array
// closing bracket `]` (empty Data). Element frames always carry
// Closed=false; closure is never coalesced onto an element. Source
// decoders for array framing must only emit CompletionEvent when a bare
// Closed frame arrives, so a missing `]` (truncation) can never emit a
// downstream terminal.
type Frame struct {
	Event    string
	HasEvent bool
	Data     string
	Closed   bool
}

// FrameSource is the minimal source-owned framing seam: one framed unit
// at a time under lim.MaxFrameBytes, no goroutines, no payload meaning.
// SSE and incremental JSON-array both satisfy it; the pipeline owns no
// framing beyond calling Next until the lifecycle observes the source
// terminal. Framing terminals are source-owned: SSE returns
// truncation on clean EOF (never success), while array framing returns
// Closed=true only after consuming `]` (EOF before `]` is truncation,
// `[elem,]` trailing comma is invalid JSON).
type FrameSource interface {
	Next() (Frame, *RouterError)
}

// SSEFrameReader pulls one typed framing frame at a time from r under
// lim.MaxFrameBytes, with no goroutines. For SSE it returns one frame
// per blank-line-terminated event block (coalescing multi-line data
// fields with "\n", skipping comments and keepalives); the caller must
// not expect EOF after a semantic terminal. Trailing bytes after the
// terminal are the same HTTP body's unread remainder (body closed,
// never another generation). Downstream write failure stops upstream
// reads immediately: callers check the write result before calling
// Next again.
type SSEFrameReader struct {
	br  *bufio.Reader
	lim Limits
}

// NewSSEFrameReader creates the framing reader for one stream.
func NewSSEFrameReader(r io.Reader, lim Limits) *SSEFrameReader {
	return &SSEFrameReader{br: bufio.NewReaderSize(r, 64*1024), lim: lim}
}

func frameBoundExceeded() *RouterError {
	return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream SSE frame exceeds the size bound"}
}

func frameTruncated() *RouterError {
	return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream ended without completion"}
}

// readFrameLine reads one \n-terminated line bounded by max total bytes
// (including the newline). It reports (line, eof) where eof means the
// reader ended cleanly at a line boundary.
func readFrameLine(br *bufio.Reader, max int) (string, bool, *RouterError) {
	var sb strings.Builder
	for {
		c, err := br.ReadByte()
		if err != nil {
			if sb.Len() == 0 {
				return "", true, nil
			}
			return "", false, frameTruncated()
		}
		if sb.Len()+1 > max {
			return "", false, frameBoundExceeded()
		}
		if c == '\n' {
			s := sb.String()
			return strings.TrimSuffix(s, "\r"), false, nil
		}
		sb.WriteByte(c)
	}
}

// Next returns the next typed frame. Truncation (EOF with no framing
// terminal, mid-frame EOF) is a 502 naming the failure; a bare framing
// violation (oversize frame) is a 502 naming the framing. EOF always
// errors: SSE streams terminate semantically, never by close. Calling
// Next after an error keeps erroring; the reader has no terminal state
// to advance past.
func (f *SSEFrameReader) Next() (Frame, *RouterError) {
	var event string
	var hasEvent bool
	var dataLines []string
	frameBytes := 0
	for {
		line, eof, rerr := readFrameLine(f.br, f.lim.MaxFrameBytes)
		if rerr != nil {
			return Frame{}, rerr
		}
		if eof {
			// Clean EOF ends no SSE frame: without a semantic
			// terminal this is truncation, never success.
			return Frame{}, frameTruncated()
		}
		frameBytes += len(line) + 1
		if frameBytes > f.lim.MaxFrameBytes {
			return Frame{}, frameBoundExceeded()
		}
		if line == "" {
			if !hasEvent && len(dataLines) == 0 {
				// Blank, comment-only, or leading frame: drop
				// and restart accounting so keepalives never
				// accumulate toward the bound.
				frameBytes = 0
				continue
			}
			break
		}
		if strings.HasPrefix(line, ":") {
			continue // SSE comment / keepalive
		}
		name, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch name {
		case "event":
			// Last event field wins per SSE dispatch semantics.
			event, hasEvent = value, true
		case "data":
			dataLines = append(dataLines, value)
		default:
			// Unknown SSE field names are framing, not
			// semantics, and stay ignored here; the event
			// name above is preserved for the source
			// decoder, which owns all semantic checks.
		}
	}
	return Frame{Event: event, HasEvent: hasEvent, Data: strings.Join(dataLines, "\n")}, nil
}

// --- Incremental JSON-array framing (Gemini streamGenerateContent) --------

// JSONArrayFrameReader splits an incrementally streamed JSON array
// (`[` object (`,` object)* `]`) into one Frame per element without
// retaining the whole generation: each Next returns as soon as a
// complete brace-balanced object is accumulated, WITHOUT waiting for
// its terminator (`,` or `]`). The terminator is consumed at the start
// of the following Next call, so the first delta emits before the
// upstream separator arrives. No goroutines; each element bounded by
// lim.MaxFrameBytes inclusive.
//
// Contract (source-owned termination):
//   - Opening `[` is validated on the first Next; missing/invalid `[`
//     is an invalid-response error, EOF before `[` is truncation.
//   - Each element must be a `{...}` object; strings/escapes are tracked
//     so braces inside strings never affect depth.
//   - Each element returns as Frame{Data: elem} (Closed always false).
//     The array close arrives as an explicit bare Frame{Data: "",
//     Closed: true} once `]` is consumed on a later Next call.
//   - After a `,`-terminated element the next frame must be an element:
//     a bare `]` there is a trailing comma (invalid JSON).
//   - EOF anywhere before the closing `]` is truncation (never success).
//   - SSE decoders ignore Closed; array decoders must only emit
//     CompletionEvent when a bare Closed frame arrives (the `]` was
//     consumed by the FrameSource before success is encoded), so array
//     truncation cannot emit a downstream terminal.
type JSONArrayFrameReader struct {
	br             *bufio.Reader
	lim            Limits
	opened         bool
	allowBareClose bool
	done           bool
	pending        bool
}

// NewJSONArrayFrameReader creates the array framing reader for one stream.
func NewJSONArrayFrameReader(r io.Reader, lim Limits) *JSONArrayFrameReader {
	return &JSONArrayFrameReader{br: bufio.NewReaderSize(r, 64*1024), lim: lim, allowBareClose: true}
}

func arrayTruncated() *RouterError {
	return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream ended without completion"}
}

func arrayInvalid() *RouterError {
	return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
}

// Next returns the next array element frame. See the type contract for
// terminator handling and error classification. A complete object is
// returned immediately without consuming its terminator; the terminator
// is validated at the start of the following call and array closure
// arrives as an explicit bare Closed frame.
func (f *JSONArrayFrameReader) Next() (Frame, *RouterError) {
	if f.done {
		return Frame{}, arrayTruncated()
	}
	if !f.opened {
		if rerr := expectArrayByte(f.br, '['); rerr != nil {
			return Frame{}, rerr
		}
		f.opened = true
	}
	if f.pending {
		for {
			nb, err := readArrayByte(f.br)
			if err != nil {
				return Frame{}, arrayTruncated()
			}
			if nb == ' ' || nb == '\n' || nb == '\r' || nb == '\t' {
				continue
			}
			if nb == ',' {
				f.allowBareClose = false
				f.pending = false
				break
			}
			if nb == ']' {
				f.done = true
				f.pending = false
				return Frame{Closed: true}, nil
			}
			return Frame{}, arrayInvalid()
		}
	}
	for {
		b, err := readArrayByte(f.br)
		if err != nil {
			return Frame{}, arrayTruncated()
		}
		if b == ' ' || b == '\n' || b == '\r' || b == '\t' {
			continue
		}
		if b == ']' {
			if !f.allowBareClose {
				return Frame{}, arrayInvalid()
			}
			f.done = true
			return Frame{Closed: true}, nil
		}
		if b != '{' {
			return Frame{}, arrayInvalid()
		}
		elem, rerr := readArrayObject(f.br, f.lim, b)
		if rerr != nil {
			return Frame{}, rerr
		}
		f.pending = true
		return Frame{Data: string(elem)}, nil
	}
}

func readArrayByte(br *bufio.Reader) (byte, error) {
	return br.ReadByte()
}

func expectArrayByte(br *bufio.Reader, want byte) *RouterError {
	for {
		b, err := br.ReadByte()
		if err != nil {
			return arrayTruncated()
		}
		if b == ' ' || b == '\n' || b == '\r' || b == '\t' {
			continue
		}
		if b != want {
			return arrayInvalid()
		}
		return nil
	}
}

// readArrayObject accumulates one brace-balanced JSON object starting with
// the already-consumed opening byte, bounded to lim.MaxFrameBytes
// inclusive. Strings and escapes are tracked so braces inside strings
// never affect depth.
func readArrayObject(br *bufio.Reader, lim Limits, open byte) ([]byte, *RouterError) {
	buf := make([]byte, 0, 4096)
	buf = append(buf, open)
	depth := 1
	inString := false
	escaped := false
	for {
		b, err := br.ReadByte()
		if err != nil {
			return nil, arrayTruncated()
		}
		buf = append(buf, b)
		if len(buf) > lim.MaxFrameBytes {
			return nil, frameBoundExceeded()
		}
		if inString {
			if escaped {
				escaped = false
			} else if b == '\\' {
				escaped = true
			} else if b == '"' {
				inString = false
			}
			continue
		}
		switch b {
		case '"':
			inString = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return buf, nil
			}
			if depth < 0 {
				return nil, arrayInvalid()
			}
		}
	}
}
