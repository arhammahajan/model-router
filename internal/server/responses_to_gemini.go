package server

import (
	"io"
	"net/http"
	"time"

	"github.com/arhammahajan/router/internal/capability"
	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/protocol"
	"github.com/arhammahajan/router/internal/router"
	"github.com/arhammahajan/router/internal/transport"
)

// serveResponsesToGemini bridges POST /v1/responses to a Gemini upstream.
// The request path is decode -> plan -> enforce policy -> encode once: the
// Responses body is decoded into the typed IR with the Gemini-destination
// loss plan, losses are enforced against the provider compatibility only
// (the client cannot select lossy), and the plan is encoded once as a Gemini
// generateContent request for the upstream model. No upstream I/O happens
// before the policy decision.
//
// The model and streaming mode live in the upstream URL: non-streaming uses
// generateContent, streaming uses streamGenerateContent with ?alt=sse. The
// upstream may answer a stream with the standard JSON array; the framing
// decoder is selected from the reviewed upstream Content-Type, never by
// sniffing the body. JSON responses translate to the Responses envelope
// with the public model name and the validated tools/tool_choice echo; SSE
// streams translate incrementally with commitment awareness into the shared
// lazy Responses committer (responses_to_chat.go): before the first encoder
// write no downstream headers are committed, so early upstream
// truncation/invalid/error surfaces as an ordinary Responses HTTP 502.
// After commitment the status is kept and a single response.failed event is
// emitted (the translator emits it on the reviewed paths; the boundary
// emits it once when missing, reusing the committed response ID and the
// next sequence number), without a fabricated response.completed or a
// duplicate codec error.
func (s *Server) serveResponsesToGemini(w http.ResponseWriter, r *http.Request, p *config.Provider, model *config.Model, body []byte, sess router.Session, snap *router.Snapshot) int {
	lim := protocol.DefaultLimits()
	gen, losses, rerr := protocol.DecodeResponsesForGemini(body, lim)
	if rerr != nil {
		s.writeIngressError(w, config.DialectOpenAIResponses, *rerr)
		return rerr.Status
	}
	// Same pinned snapshot plans the closed Responses effort intent for the
	// Gemini destination: exact
	// generationConfig.thinkingConfig.thinkingLevel only on explicit
	// reasoning.effort.supported=true plus the exact gemini native
	// descriptor plus the reviewed known enum plus the official wire enum,
	// otherwise a diagnosed drop. Strict refuses pre-I/O via EnforcePolicy,
	// lossy drops and reports. No model-name, family or URL heuristic is
	// ever consulted.
	contract := snap.RequestCapability(model, p)
	caps := capability.ReasoningFor(contract)
	planned, planLosses := protocol.PlanEffort(gen.Reasoning, caps, protocol.OrdinaryGeminiRoute())
	losses = append(losses, planLosses...)
	kept, rerr := protocol.EnforcePolicy(losses, p.IsLossy())
	if rerr != nil {
		s.writeIngressError(w, config.DialectOpenAIResponses, *rerr)
		return rerr.Status
	}
	upstreamBody, rerr := protocol.EncodeGeminiRequestWithPlan(gen, planned)
	if rerr != nil {
		s.writeIngressError(w, config.DialectOpenAIResponses, *rerr)
		return rerr.Status
	}
	method := "generateContent"
	alt := ""
	if gen.Stream {
		method = "streamGenerateContent"
		alt = "sse"
	}
	dest := model.EffectiveDialect(p)
	url, rerr := transport.GeminiURL(p.BaseURL, model.Upstream, method, alt)
	if rerr != nil {
		s.writeIngressError(w, config.DialectOpenAIResponses, *rerr)
		return rerr.Status
	}
	// Destination is Gemini: the Gemini dialect accepts no inbound headers,
	// so only provider statics, the configured x-goog-api-key credential
	// and framing cross. Client credentials and query keys never leak.
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
		translated, rerr := protocol.TranslateGeminiJSONToResponsesWithPlanAndPolicy(res.Body, model.Name, lim, gen.ToolPlan, p.IsLossy())
		if rerr != nil {
			s.writeIngressError(w, config.DialectOpenAIResponses, *rerr)
			return rerr.Status
		}
		if int64(len(translated)) > s.cfg.Server.MaxBodyBytes {
			s.writeIngressError(w, config.DialectOpenAIResponses, protocol.RouterError{Status: 502,
				Code: protocol.CodeUpstreamBad, Msg: "Upstream response exceeds the size bound"})
			return 502
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
	return s.copyResponsesToGeminiStream(w, r, res, model.Name, lossHeader, gen, p.IsLossy())
}

// copyResponsesToGeminiStream translates an upstream Gemini stream into
// Responses SSE with commitment awareness and bounded downstream writes.
// lossyDrops carries the provider compatibility: true accepts the
// validated routine provider/gateway response extensions (thoughtSignature,
// TEXT-modality usage details, usage.cost/usage.opencode); false keeps the
// strict 502.
// The framing decoder is selected from the reviewed upstream Content-Type:
// text/event-stream uses the SSE translator, application/json the
// incremental JSON-array translator. Any other content type is an ordinary
// pre-commit 502: the body is never probed or buffered to guess the
// framing, and raw upstream bytes never leak into translated errors. It
// never buffers the whole generation and never repairs/retries. The
// validated tools/tool_choice echo rides from the typed plan (never the
// raw body) on lifecycle frames and on the post-commit failure fallback.
// Post-commit failures keep 200 with a single response.failed event and no
// fabricated response.completed.
func (s *Server) copyResponsesToGeminiStream(w http.ResponseWriter, r *http.Request, res *transport.Result, publicModel, lossHeader string, gen *protocol.Generation, lossyDrops bool) int {
	media := upstreamMediaType(res.Header.Get("Content-Type"))
	lim := protocol.DefaultLimits()
	toolsRaw, choiceRaw := responsesRequestEcho(gen)
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
	var tr interface {
		Translate(io.Reader, io.Writer, func()) *protocol.RouterError
	}
	switch media {
	case "text/event-stream":
		tr = &protocol.GeminiToResponsesSSETranslator{PublicModel: publicModel, Limits: lim, Plan: gen.ToolPlan, AllowLossyDrops: lossyDrops}
	case "application/json":
		tr = &protocol.GeminiToResponsesJSONArrayTranslator{PublicModel: publicModel, Limits: lim, Plan: gen.ToolPlan, AllowLossyDrops: lossyDrops}
	default:
		// Unsupported encoding or protocol shape: ordinary pre-commit 502.
		// The upstream body is closed promptly on every path.
		res.Stream.Close()
		s.writeIngressError(w, config.DialectOpenAIResponses, protocol.RouterError{Status: 502,
			Code: protocol.CodeUpstreamBad, Msg: "Upstream response encoding is not supported"})
		return 502
	}
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
