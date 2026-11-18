// Package server Anthropic ingress -> Gemini upstream translation.
//
// The request path is decode -> plan -> enforce policy -> encode once: the
// Anthropic body is decoded into the typed IR with the Gemini-destination
// loss plan, losses are enforced against the provider compatibility only
// (the client cannot select lossy), and the plan is encoded once as a
// Gemini generateContent request. No upstream I/O happens before the policy
// decision.
//
// The model and streaming mode live in the upstream URL: non-streaming uses
// generateContent, streaming uses streamGenerateContent with ?alt=sse.
// Destination is Gemini: only provider statics and the configured
// x-goog-api-key credential cross; client Anthropic credentials and query
// keys never leak, and no destination opt-ins exist. The upstream may answer
// a stream with SSE or the standard JSON array; the framing decoder is
// selected from the reviewed upstream Content-Type, never by sniffing the
// body. JSON responses translate to Anthropic Messages with the public
// model name; SSE streams translate incrementally with commitment awareness
// into the shared lazy Anthropic committer: before the first encoder write
// no downstream headers are committed, so early upstream
// truncation/invalid/error surfaces as an ordinary Anthropic HTTP 502/504.
// After commitment the status is kept, a single Anthropic error event is
// emitted once, and the stream terminates without a synthetic message_stop.
package server

import (
	"io"
	"net/http"
	"time"

	"github.com/arhammahajan/model-router/internal/capability"
	"github.com/arhammahajan/model-router/internal/config"
	"github.com/arhammahajan/model-router/internal/protocol"
	"github.com/arhammahajan/model-router/internal/router"
	"github.com/arhammahajan/model-router/internal/transport"
)

// serveAnthropicToGemini bridges POST /v1/messages to a Gemini upstream.
// The caller owns the preamble (deadline, generation slot) and passes its
// context via r.
func (s *Server) serveAnthropicToGemini(w http.ResponseWriter, r *http.Request, p *config.Provider, model *config.Model, body []byte, sess router.Session, snap *router.Snapshot) int {
	lim := protocol.DefaultLimits()
	gen, losses, rerr := protocol.DecodeAnthropicForGemini(body, lim)
	if rerr != nil {
		s.writeIngressError(w, config.DialectAnthropic, *rerr)
		return rerr.Status
	}
	// Same pinned snapshot plans the closed Budget intent through the
	// shared planner for the Gemini destination: exact
	// generationConfig.thinkingConfig.thinkingBudget only on the pinned
	// canonical native fact, otherwise a diagnosed drop. Strict refuses
	// pre-I/O via EnforcePolicy, lossy drops and reports. No model-name,
	// family or URL heuristic is ever consulted.
	contract := snap.RequestCapability(model, p)
	caps := capability.ReasoningFor(contract)
	planned, planLosses := protocol.PlanEffort(gen.Reasoning, caps, protocol.OrdinaryGeminiRoute())
	losses = append(losses, planLosses...)
	kept, rerr := protocol.EnforcePolicy(losses, p.IsLossy())
	if rerr != nil {
		s.writeIngressError(w, config.DialectAnthropic, *rerr)
		return rerr.Status
	}
	upstreamBody, rerr := protocol.EncodeGeminiRequestWithPlan(gen, planned)
	if rerr != nil {
		s.writeIngressError(w, config.DialectAnthropic, *rerr)
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
		s.writeIngressError(w, config.DialectAnthropic, *rerr)
		return rerr.Status
	}
	// Destination is Gemini: the Gemini dialect accepts no inbound headers,
	// so only provider statics and the configured credential cross. Client
	// Anthropic credentials never become upstream credentials.
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
		translated, rerr := protocol.TranslateGeminiJSONToAnthropicWithPolicy(res.Body, model.Name, lim, p.IsLossy())
		if rerr != nil {
			s.writeIngressError(w, config.DialectAnthropic, *rerr)
			return rerr.Status
		}
		if int64(len(translated)) > s.cfg.Server.MaxBodyBytes {
			s.writeIngressError(w, config.DialectAnthropic, protocol.RouterError{Status: 502,
				Code: protocol.CodeUpstreamBad, Msg: "Upstream response exceeds the size bound"})
			return 502
		}
		forwardRetryHints(w, res.Header)
		w.Header().Set("Content-Type", "application/json")
		if lossHeader != "" {
			w.Header().Set("X-Model-Router-Loss", lossHeader)
		}
		s.writeJSON(w, 200, translated)
		return 200
	}
	return s.copyAnthropicToGeminiStream(w, r, res, model.Name, lossHeader, p.IsLossy())
}

// copyAnthropicToGeminiStream translates an upstream Gemini stream into
// Anthropic Messages SSE with commitment awareness and bounded downstream
// writes. lossyDrops carries the provider compatibility: true accepts the
// validated routine provider/gateway response extensions (thoughtSignature,
// TEXT-modality usage details, usage.cost/usage.opencode); false keeps the
// strict 502.
// The framing decoder is selected from the reviewed upstream
// Content-Type: text/event-stream uses the SSE translator,
// application/json the incremental JSON-array translator. Any other content
// type is an ordinary pre-commit 502: the body is never probed or buffered
// to guess the framing, and raw upstream bytes never leak into translated
// errors. It never buffers the whole generation and never repairs/retries.
// Post-commit failures keep 200 with one Anthropic error event and no
// synthetic message_stop; the translator emits the error on the reviewed
// paths and the boundary emits it once when missing, guarding against
// model-text false positives via framed error detection.
func (s *Server) copyAnthropicToGeminiStream(w http.ResponseWriter, r *http.Request, res *transport.Result, publicModel, lossHeader string, lossyDrops bool) int {
	media := upstreamMediaType(res.Header.Get("Content-Type"))
	lim := protocol.DefaultLimits()
	var translate func(io.Reader, io.Writer, func()) *protocol.RouterError
	switch media {
	case "text/event-stream":
		tr := &protocol.GeminiToAnthropicSSETranslator{PublicModel: publicModel, Limits: lim, AllowLossyDrops: lossyDrops}
		translate = tr.Translate
	case "application/json":
		tr := &protocol.GeminiToAnthropicJSONArrayTranslator{PublicModel: publicModel, Limits: lim, AllowLossyDrops: lossyDrops}
		translate = tr.Translate
	default:
		// Unsupported encoding: ordinary pre-commit 502, upstream closed.
		res.Stream.Close()
		s.writeIngressError(w, config.DialectAnthropic, protocol.RouterError{Status: 502,
			Code: protocol.CodeUpstreamBad, Msg: "Upstream response encoding is not supported"})
		return 502
	}
	timeout := time.Duration(s.cfg.Server.WriteTimeout * float64(time.Second))
	rc := http.NewResponseController(w)
	if timeout > 0 {
		defer rc.SetWriteDeadline(time.Time{})
	}
	committer := &anthropicStreamCommitter{
		w: w, rc: rc, timeout: timeout,
		lossHeader: lossHeader, retryHeader: res.Header,
	}
	tracker := &anthropicStreamReadTracker{src: res.Stream}
	flushFailed := false
	rerr := translate(tracker, committer, func() {
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
		// the reviewed paths; the boundary emits it once when missing.
		// When the downstream already failed, never attempt another raw
		// write that could stall; close upstream promptly instead.
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
