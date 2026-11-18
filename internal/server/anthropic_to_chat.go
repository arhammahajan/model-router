// Package server Anthropic ingress -> OpenAI Chat upstream translation.
//
// The request path is decode -> plan -> enforce policy -> encode once: the
// Anthropic body is decoded into the typed IR, losses are enforced against
// the provider compatibility only (the client cannot select lossy), and the
// plan is encoded once as a Chat request for the upstream model. No upstream
// I/O happens before the policy decision.
//
// The response path reuses the shared transport with the upstream
// /chat/completions endpoint. JSON responses translate to Anthropic Messages
// with the public model name; SSE streams translate incrementally with
// commitment awareness: before the first encoder write no downstream headers
// are committed, so early upstream truncation/invalid/error surfaces as an
// ordinary Anthropic HTTP 502. After commitment the status is kept, an
// Anthropic error event is emitted once, and the stream terminates without a
// synthetic message_stop.
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/arhammahajan/model-router/internal/capability"
	"github.com/arhammahajan/model-router/internal/config"
	"github.com/arhammahajan/model-router/internal/protocol"
	"github.com/arhammahajan/model-router/internal/router"
	"github.com/arhammahajan/model-router/internal/transport"
)

// serveAnthropicTranslated bridges POST /v1/messages to an openai-chat
// upstream. The caller passes the same pinned snapshot from the preamble so
// route, provider and capability facts stay on one state; the handler never
// repins.
func (s *Server) serveAnthropicTranslated(w http.ResponseWriter, r *http.Request, p *config.Provider, model *config.Model, body []byte, sess router.Session, snap *router.Snapshot) int {
	lim := protocol.DefaultLimits()
	gen, losses, rerr := protocol.DecodeAnthropicRequest(body, lim)
	if rerr != nil {
		s.writeIngressError(w, config.DialectAnthropic, *rerr)
		return rerr.Status
	}
	// Same pinned snapshot plans the closed Anthropic disabled intent
	// through the shared planner; the Responses-ingress hard refusal for
	// unobserved runtimes is not copied here (the planner diagnoses it).
	contract := snap.RequestCapability(model, p)
	caps := capability.ReasoningFor(contract)
	scope := protocol.OrdinaryRoute()
	if snap.HasLlamaProvider(p.Name) {
		scope = protocol.LocalRoute(snap.LlamaRuntime(p.Name))
	}
	planned, planLosses := protocol.PlanEffort(gen.Reasoning, caps, scope)
	losses = append(losses, planLosses...)
	kept, rerr := protocol.EnforcePolicy(losses, p.IsLossy())
	if rerr != nil {
		s.writeIngressError(w, config.DialectAnthropic, *rerr)
		return rerr.Status
	}
	upstreamBody, rerr := protocol.EncodeChatRequest(gen, planned, model.Upstream)
	if rerr != nil {
		s.writeIngressError(w, config.DialectAnthropic, *rerr)
		return rerr.Status
	}
	dest := model.EffectiveDialect(p)
	url, rerr := transport.UpstreamURL(p.BaseURL, dest)
	if rerr != nil {
		s.writeIngressError(w, config.DialectAnthropic, *rerr)
		return rerr.Status
	}
	// Destination is OpenAI Chat: only openai-beta may cross. Anthropic
	// version/beta headers are dropped by the shared transport, and client
	// credentials never become upstream credentials.
	headers := transport.OutboundHeaders(r.Header, p, config.DialectAnthropic, dest, sess.ID, sess.Has)
	res, rerr := s.up.Do(r.Context(), p, s.cfg.Retry, url, headers, upstreamBody, gen.Stream, s.cfg.Server.MaxBodyBytes)
	if rerr != nil {
		if res != nil && res.Failed {
			forwardRetryHints(w, res.Header)
			rerr = anthropicUpstreamDetail(res.Status)
		}
		s.writeIngressError(w, config.DialectAnthropic, *rerr)
		return rerr.Status
	}
	// Every exit closes the upstream body. For streams ownership moves to
	// the copy loop below, which closes on return; the deferred close is a
	// safety net for the JSON path and pre-commit stream failures.
	defer func() {
		if res.Stream != nil {
			res.Stream.Close()
		}
	}()
	lossHeader := protocol.EncodeLosses(kept)
	if !gen.Stream {
		translated, rerr := protocol.TranslateChatJSONToAnthropicWithPolicy(res.Body, model.Name, lim, p.IsLossy())
		if rerr != nil {
			s.writeIngressError(w, config.DialectAnthropic, *rerr)
			return rerr.Status
		}
		forwardRetryHints(w, res.Header)
		w.Header().Set("Content-Type", "application/json")
		if lossHeader != "" {
			w.Header().Set("X-Model-Router-Loss", lossHeader)
		}
		s.writeJSON(w, 200, translated)
		return 200
	}
	return s.copyAnthropicTranslatedStream(w, r, res, model.Name, lossHeader, p.IsLossy())
}

// anthropicUpstreamDetail renders a non-retried translated upstream error
// status. The message is a stable sanitized status only: raw upstream bodies
// can echo secrets or prompt text, so they are never copied into translated
// errors. Status is preserved; only safe retry hints are forwarded.
func anthropicUpstreamDetail(status int) *protocol.RouterError {
	code := "api_error"
	if status < 500 {
		code = "invalid_request_error"
	}
	return &protocol.RouterError{Status: status, Code: code,
		Msg: fmt.Sprintf("Upstream returned HTTP %d", status)}
}

// anthropicStreamCommitter is a minimal explicit lazy writer/commit wrapper
// for the translated SSE path. No headers are committed before the first
// valid encoder write: WriteHeader(200) plus the SSE framing, loss header
// and safe retry hints are set on the first Write. Before commitment a
// translator failure maps to an ordinary Anthropic HTTP 502; after
// commitment the status is kept and the boundary emits a single Anthropic
// error event.
type anthropicStreamCommitter struct {
	w           http.ResponseWriter
	rc          *http.ResponseController
	timeout     time.Duration
	lossHeader  string
	retryHeader http.Header
	committed   bool
	failed      bool
	errorSent   bool
}

func (c *anthropicStreamCommitter) Write(p []byte) (int, error) {
	if c.failed {
		return 0, fmt.Errorf("downstream write failed")
	}
	if !c.committed {
		c.w.Header().Set("Content-Type", "text/event-stream")
		c.w.Header().Set("Cache-Control", "no-cache")
		if c.lossHeader != "" {
			c.w.Header().Set("X-Model-Router-Loss", c.lossHeader)
		}
		forwardRetryHints(c.w, c.retryHeader)
		c.w.WriteHeader(200)
		c.committed = true
	}
	if c.timeout > 0 {
		c.rc.SetWriteDeadline(time.Now().Add(c.timeout))
	}
	n, err := c.w.Write(p)
	if err != nil {
		c.failed = true
		return n, err
	}
	if detectsAnthropicErrorEvent(p) {
		c.errorSent = true
	}
	return n, nil
}

// detectsAnthropicErrorEvent reports whether a downstream frame carries an
// actual Anthropic SSE error event, so the boundary does not emit a second
// one. Only the explicit SSE framing counts: a line whose field name is
// "event" with value "error". Prompt or tool data containing the substring
// "event: error" inside a data: JSON payload must never mark errorSent.
func detectsAnthropicErrorEvent(p []byte) bool {
	start := 0
	for i := 0; i <= len(p); i++ {
		var line string
		if i == len(p) {
			if start >= len(p) {
				break
			}
			line = string(p[start:])
		} else if p[i] == '\n' {
			line = string(p[start:i])
			start = i + 1
		} else {
			continue
		}
		line = strings.TrimSuffix(line, "\r")
		name, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		if strings.TrimSpace(name) != "event" {
			continue
		}
		if strings.TrimSpace(value) == "error" {
			return true
		}
	}
	return false
}

// anthropicStreamReadTracker records the first non-EOF upstream read error
// with its type preserved. The protocol translator erases reader errors into
// a plain 502, so the pre-commit boundary checks this tracker (typed, never
// string-matched) to distinguish absolute/provider deadline expiry (504)
// from truncation or invalid bodies (502). No goroutine.
type anthropicStreamReadTracker struct {
	src      io.ReadCloser
	firstErr error
}

func (t *anthropicStreamReadTracker) Read(p []byte) (int, error) {
	n, err := t.src.Read(p)
	if err != nil && err != io.EOF && t.firstErr == nil {
		t.firstErr = err
	}
	return n, err
}

func (t *anthropicStreamReadTracker) Close() error { return t.src.Close() }

// isPrecommitStreamTimeout reports whether a pre-commit stream failure is a
// deadline expiry. Typed only: context deadline, network timeout or OS
// timeout, plus an expired absolute handler deadline. Context cancellation
// (client gone) keeps the existing 502 semantics and is not a timeout.
func isPrecommitStreamTimeout(tracker *anthropicStreamReadTracker, ctx context.Context) bool {
	if ctx.Err() == context.DeadlineExceeded {
		return true
	}
	err := tracker.firstErr
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	if os.IsTimeout(err) {
		return true
	}
	return false
}

// copyAnthropicTranslatedStream translates an upstream Chat SSE stream into
// Anthropic Messages SSE with commitment awareness and bounded downstream
// writes. It never buffers the whole generation and never repairs/retries.
func (s *Server) copyAnthropicTranslatedStream(w http.ResponseWriter, r *http.Request, res *transport.Result, publicModel, lossHeader string, allowPrivateDrop bool) int {
	timeout := time.Duration(s.cfg.Server.WriteTimeout * float64(time.Second))
	rc := http.NewResponseController(w)
	// The write deadline always resets for keep-alive reuse, including the
	// best-effort post-commit error event path below.
	if timeout > 0 {
		defer rc.SetWriteDeadline(time.Time{})
	}
	committer := &anthropicStreamCommitter{
		w: w, rc: rc, timeout: timeout,
		lossHeader: lossHeader, retryHeader: res.Header,
	}
	tracker := &anthropicStreamReadTracker{src: res.Stream}
	flushFailed := false
	tr := &protocol.ChatToAnthropicSSETranslator{PublicModel: publicModel, Limits: protocol.DefaultLimits(), AllowPrivateReasoningDrop: allowPrivateDrop}
	rerr := tr.Translate(tracker, committer, func() {
		if timeout > 0 {
			rc.SetWriteDeadline(time.Now().Add(timeout))
		}
		if ferr := rc.Flush(); ferr != nil {
			// Downstream gone: fail fast, close upstream synchronously so
			// a blocked upstream Read unblocks, and release the slot via
			// the deferred preamble cancel. No goroutine.
			flushFailed = true
			committer.failed = true
			tracker.Close()
		} else if timeout > 0 {
			rc.SetWriteDeadline(time.Now().Add(timeout))
		}
	})
	if rerr != nil || flushFailed || committer.failed {
		if !committer.committed {
			// Pre-commit: ordinary Anthropic HTTP error, never an empty
			// 200. A stalled body past the absolute or provider deadline
			// is a 504; truncation, invalid frames and client cancellation
			// keep the existing 502 semantics.
			tracker.Close()
			if isPrecommitStreamTimeout(tracker, r.Context()) {
				s.writeIngressError(w, config.DialectAnthropic, protocol.RouterError{Status: 504,
					Code: "request_deadline_exceeded",
					Msg:  "Overall upstream request deadline exceeded; generation may have been accepted and billed."})
				return 504
			}
			s.writeIngressError(w, config.DialectAnthropic, *rerrOrUpstream(rerr))
			if rerr != nil {
				return rerr.Status
			}
			return 502
		}
		// Post-commit: keep 200, emit one error event, terminate without a
		// synthetic message_stop. The translator emits the error event on
		// several paths; the boundary emits it once when missing. When the
		// downstream already failed, never attempt another raw write that
		// could stall; close upstream promptly instead.
		if committer.failed || flushFailed {
			tracker.Close()
			return 200
		}
		if !committer.errorSent {
			emitAnthropicStreamError(committer, rc, timeout)
		}
		tracker.Close()
		return 200
	}
	return 200
}

func rerrOrUpstream(rerr *protocol.RouterError) *protocol.RouterError {
	if rerr != nil {
		return rerr
	}
	return &protocol.RouterError{Status: 502, Code: protocol.CodeUpstreamBad, Msg: "Upstream stream failed"}
}

// emitAnthropicStreamError best-effort writes one Anthropic error event
// after commitment. A write failure here is terminal.
func emitAnthropicStreamError(c *anthropicStreamCommitter, rc *http.ResponseController, timeout time.Duration) {
	payload := "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"Upstream stream reported an error\"}}\n\n"
	if timeout > 0 {
		rc.SetWriteDeadline(time.Now().Add(timeout))
	}
	_, _ = c.w.Write([]byte(payload))
	c.errorSent = true
	_ = rc.Flush()
}
