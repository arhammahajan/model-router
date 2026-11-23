package server

import (
	"net/http"
	"time"

	"github.com/arhammahajan/router/internal/capability"
	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/protocol"
	"github.com/arhammahajan/router/internal/router"
	"github.com/arhammahajan/router/internal/transport"
)

// serveAnthropicToResponses bridges POST /v1/messages to an openai-responses
// upstream. The request path is decode -> plan -> enforce policy -> encode
// once: the Anthropic body is decoded into the typed IR with the
// Responses-destination loss plan, losses are enforced against the provider
// compatibility only (the client cannot select lossy), and the plan is
// encoded once as a Responses request for the upstream model. No upstream
// I/O happens before the policy decision.
//
// The response path reuses the shared transport with the upstream
// /responses endpoint. JSON responses translate to Anthropic Messages with
// the public model name; SSE streams translate incrementally with commitment
// awareness: before the first encoder write no downstream headers are
// committed, so early upstream truncation/invalid/error surfaces as an
// ordinary Anthropic HTTP 502. After commitment the status is kept, an
// Anthropic error event is emitted once, and the stream terminates without
// a synthetic message_stop.
func (s *Server) serveAnthropicToResponses(w http.ResponseWriter, r *http.Request, p *config.Provider, model *config.Model, body []byte, sess router.Session, snap *router.Snapshot) int {
	lim := protocol.DefaultLimits()
	gen, losses, rerr := protocol.DecodeAnthropicForResponses(body, lim)
	if rerr != nil {
		s.writeIngressError(w, config.DialectAnthropic, *rerr)
		return rerr.Status
	}
	// Same pinned snapshot plans the closed Anthropic disabled intent
	// through the shared planner for the Responses destination; the
	// Responses-ingress hard refusal for unobserved runtimes is not copied
	// here (the planner diagnoses it).
	contract := snap.RequestCapability(model, p)
	caps := capability.ReasoningFor(contract)
	scope := protocol.OrdinaryResponsesRoute()
	if snap.HasLlamaProvider(p.Name) {
		scope = protocol.LocalResponsesRoute(snap.LlamaRuntime(p.Name))
	}
	planned, planLosses := protocol.PlanEffort(gen.Reasoning, caps, scope)
	losses = append(losses, planLosses...)
	kept, rerr := protocol.EnforcePolicy(losses, p.IsLossy())
	if rerr != nil {
		s.writeIngressError(w, config.DialectAnthropic, *rerr)
		return rerr.Status
	}
	upstreamBody, rerr := protocol.EncodeResponsesRequest(gen, planned, model.Upstream)
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
	// Destination is OpenAI Responses: only openai-beta may cross. Anthropic
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
		translated, rerr := protocol.TranslateResponsesJSONToAnthropicWithPolicy(res.Body, model.Name, lim, p.IsLossy())
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
	return s.copyAnthropicToResponsesStream(w, r, res, model.Name, lossHeader, p.IsLossy())
}

// copyAnthropicToResponsesStream translates an upstream Responses SSE stream
// into Anthropic Messages SSE with commitment awareness and bounded
// downstream writes. It never buffers the whole generation and never
// repairs/retries. The Anthropic downstream committer and single error-event
// fallback are shared with the Chat-source path.
func (s *Server) copyAnthropicToResponsesStream(w http.ResponseWriter, r *http.Request, res *transport.Result, publicModel, lossHeader string, allowPrivateDrop bool) int {
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
	tr := &protocol.ResponsesToAnthropicSSETranslator{PublicModel: publicModel, Limits: protocol.DefaultLimits(), AllowPrivateReasoningDrop: allowPrivateDrop}
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
