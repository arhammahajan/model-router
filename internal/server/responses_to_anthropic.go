package server

import (
	"net/http"
	"time"

	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/protocol"
	"github.com/arhammahajan/router/internal/router"
	"github.com/arhammahajan/router/internal/transport"
)

// serveResponsesToAnthropic bridges POST /v1/responses to an anthropic
// upstream. The request path is decode -> plan -> enforce policy -> encode
// once: the Responses body is decoded into the typed IR with the
// Anthropic-destination loss plan, losses are enforced against the provider
// compatibility only (the client cannot select lossy), and the plan is
// encoded once as an Anthropic request for the upstream model. No upstream
// I/O happens before the policy decision.
//
// The response path reuses the shared transport with the upstream
// /v1/messages endpoint. JSON responses translate to the Responses envelope
// with the public model name and the validated tools/tool_choice echo; SSE
// streams translate incrementally with commitment awareness: before the first
// encoder write no downstream headers are committed, so early upstream
// truncation/invalid/error surfaces as an ordinary Responses HTTP 502. After
// commitment the status is kept, a single response.failed event is emitted
// (the translator emits it on the reviewed paths; the boundary emits it once
// when missing, reusing the committed response ID and the next sequence
// number), and the stream terminates without a fabricated
// response.completed or a duplicate codec error.
func (s *Server) serveResponsesToAnthropic(w http.ResponseWriter, r *http.Request, p *config.Provider, model *config.Model, body []byte, sess router.Session) int {
	lim := protocol.DefaultLimits()
	gen, losses, rerr := protocol.DecodeResponsesForAnthropic(body, lim)
	if rerr != nil {
		s.writeIngressError(w, config.DialectOpenAIResponses, *rerr)
		return rerr.Status
	}
	kept, rerr := protocol.EnforcePolicy(losses, p.IsLossy())
	if rerr != nil {
		s.writeIngressError(w, config.DialectOpenAIResponses, *rerr)
		return rerr.Status
	}
	upstreamBody, rerr := protocol.EncodeAnthropicRequest(gen, model.Upstream)
	if rerr != nil {
		s.writeIngressError(w, config.DialectOpenAIResponses, *rerr)
		return rerr.Status
	}
	dest := model.EffectiveDialect(p)
	url, rerr := transport.UpstreamURL(p.BaseURL, dest)
	if rerr != nil {
		s.writeIngressError(w, config.DialectOpenAIResponses, *rerr)
		return rerr.Status
	}
	headers := transport.OutboundHeaders(r.Header, p, config.DialectOpenAIResponses, dest, sess.ID, sess.Has)
	res, rerr := s.up.Do(r.Context(), p, s.cfg.Retry, url, headers, upstreamBody, gen.Stream, s.cfg.Server.MaxBodyBytes)
	if rerr != nil {
		if res != nil && res.Failed {
			forwardRetryHints(w, res.Header)
			rerr = upstreamDetail(res.Status, res.Body)
		}
		s.writeIngressError(w, config.DialectOpenAIResponses, *rerr)
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
		translated, rerr := protocol.TranslateAnthropicJSONToResponsesWithPlanAndPolicy(res.Body, model.Name, lim, gen.ToolPlan, p.IsLossy())
		if rerr != nil {
			s.writeIngressError(w, config.DialectOpenAIResponses, *rerr)
			return rerr.Status
		}
		toolsRaw, choiceRaw := responsesRequestEcho(gen)
		enriched, rerr := enrichResponsesEnvelope(translated, toolsRaw, choiceRaw, s.cfg.Server.MaxBodyBytes)
		if rerr != nil {
			s.writeIngressError(w, config.DialectOpenAIResponses, *rerr)
			return rerr.Status
		}
		forwardRetryHints(w, res.Header)
		w.Header().Set("Content-Type", "application/json")
		if lossHeader != "" {
			w.Header().Set("X-Model-Router-Loss", lossHeader)
		}
		s.writeJSON(w, 200, enriched)
		return 200
	}
	return s.copyResponsesToAnthropicStream(w, r, res, model.Name, lossHeader, gen, p.IsLossy())
}

// copyResponsesToAnthropicStream translates an upstream Anthropic SSE stream
// into Responses SSE with commitment awareness and bounded downstream
// writes. It never buffers the whole generation and never repairs/retries.
// The Responses downstream committer, validated tools/tool_choice echo and
// single response.failed fallback are shared with the Chat-source path.
func (s *Server) copyResponsesToAnthropicStream(w http.ResponseWriter, r *http.Request, res *transport.Result, publicModel, lossHeader string, gen *protocol.Generation, allowGatewayDrop bool) int {
	timeout := time.Duration(s.cfg.Server.WriteTimeout * float64(time.Second))
	rc := http.NewResponseController(w)
	// The write deadline always resets for keep-alive reuse, including the
	// best-effort post-commit error path below.
	if timeout > 0 {
		defer rc.SetWriteDeadline(time.Time{})
	}
	committer := &responsesStreamCommitter{
		w: w, rc: rc, timeout: timeout,
		lossHeader: lossHeader, retryHeader: res.Header,
	}
	tracker := &anthropicStreamReadTracker{src: res.Stream}
	flushFailed := false
	lim := protocol.DefaultLimits()
	tr := &protocol.AnthropicToResponsesSSETranslator{PublicModel: publicModel, Limits: lim, Plan: gen.ToolPlan, AllowGatewayBillingDrop: allowGatewayDrop}
	toolsRaw, choiceRaw := responsesRequestEcho(gen)
	echo := &responsesEchoWriter{dst: committer, tools: toolsRaw, choice: choiceRaw, maxFrame: lim.MaxFrameBytes}
	rerr := tr.Translate(tracker, echo, func() {
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
			// Pre-commit: ordinary Responses HTTP error, never an empty
			// 200. A stalled body past the absolute or provider deadline
			// is a 504; truncation, invalid frames and client cancellation
			// keep the existing 502 semantics.
			tracker.Close()
			if isPrecommitStreamTimeout(tracker, r.Context()) {
				s.writeIngressError(w, config.DialectOpenAIResponses, protocol.RouterError{Status: 504,
					Code: "request_deadline_exceeded",
					Msg:  "Overall upstream request deadline exceeded; generation may have been accepted and billed."})
				return 504
			}
			s.writeIngressError(w, config.DialectOpenAIResponses, *rerrOrUpstream(rerr))
			if rerr != nil {
				return rerr.Status
			}
			return 502
		}
		// Post-commit: keep 200, emit one response.failed event when the
		// translator did not already emit one, and terminate without a
		// fabricated response.completed or a duplicate codec error. When
		// the downstream already failed, never attempt another raw write
		// that could stall; close upstream promptly instead.
		if committer.failed || flushFailed {
			tracker.Close()
			return 200
		}
		if !committer.failedSent {
			emitResponsesStreamError(committer, rc, timeout, publicModel, toolsRaw, choiceRaw)
		}
		tracker.Close()
		return 200
	}
	return 200
}
