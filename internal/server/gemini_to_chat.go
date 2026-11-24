package server

import (
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/arhammahajan/router/internal/capability"
	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/protocol"
	"github.com/arhammahajan/router/internal/provider"
	"github.com/arhammahajan/router/internal/router"
	"github.com/arhammahajan/router/internal/transport"
)

// serveGeminiToChat bridges a Gemini ingress path
// (/v1beta/models/{name}:generateContent or :streamGenerateContent, plus the
// /v1, version-less and tunedModels forms) to an openai-chat upstream. The
// request path is decode -> plan -> enforce policy -> encode once: the
// Gemini body is decoded into the typed IR (the public model and streaming
// mode come from the parsed URL and method, never the body), losses are
// enforced against the provider compatibility only (the client cannot select
// lossy), and the plan is encoded once as a Chat request for the upstream
// model. No upstream I/O happens before the policy decision.
//
// The upstream URL is the plain Chat /chat/completions endpoint: no
// forwarded alt and no client ?key= credential ever travel upstream, and
// EncodeChatRequest forces stream_options.include_usage on streams so the
// Chat terminal usage chunk arrives. JSON responses translate to a proper
// GenerateContentResponse with the public model version; streams translate
// incrementally with commitment awareness into the framing the client asked
// for: ?alt=sse yields text/event-stream data-only frames, a missing alt or
// alt=json yields one incremental JSON array (application/json). Before the
// first encoder write no downstream headers are committed, so early upstream
// truncation/invalid/error surfaces as an ordinary Gemini HTTP 502/504.
// After commitment the status is kept: SSE terminates with one sanitized
// data:{error:google.rpc.Status} frame (no [DONE], no invented STOP or
// usage), and the array terminates without the closing ']' so the client
// detects truncation instead of reading a manufactured success.
func (s *Server) serveGeminiToChat(w http.ResponseWriter, r *http.Request, p *config.Provider, model *config.Model, body []byte, call geminiCall, alt string, sess router.Session, snap *router.Snapshot, compatEffort protocol.ReasoningIntent) int {
	lim := protocol.DefaultLimits()
	stream := call.method == "streamGenerateContent"
	gen, losses, rerr := protocol.DecodeGeminiRequest(body, call.name, stream, lim)
	if rerr != nil {
		s.writeIngressError(w, config.DialectGemini, *rerr)
		return rerr.Status
	}
	if compatEffort.IsPresent() && gen.Reasoning.IsPresent() {
		rerr := protocol.RouterError{Status: 400, Code: protocol.CodeInvalidGeneration, Msg: "Router effort cannot be combined with Gemini thinking controls"}
		s.writeIngressError(w, config.DialectGemini, rerr)
		return rerr.Status
	}
	intent := gen.Reasoning
	if compatEffort.IsPresent() {
		intent = compatEffort
	}
	// One pinned snapshot supplies route, provider and canonical
	// capability facts: plan the closed Gemini intent through the shared
	// planner with the pinned runtime scope, then enforce the provider
	// policy before any upstream I/O. Unknown cached runtimes drop a
	// present intent as a diagnosed loss in the planner.
	dest := p.ResolveDialect(config.DialectGemini, model.Dialect)
	contract := snap.RequestCapability(model, p)
	caps := capability.ReasoningFor(contract)
	scope := provider.ForKind(p.Kind).EffortScope(dest)
	planned, planLosses := protocol.PlanEffort(intent, caps, scope)
	losses = append(losses, planLosses...)
	kept, rerr := protocol.EnforcePolicy(losses, p.IsLossy())
	if rerr != nil {
		s.writeIngressError(w, config.DialectGemini, *rerr)
		return rerr.Status
	}
	upstreamBody, rerr := protocol.EncodeChatRequest(gen, planned, model.Upstream)
	if rerr != nil {
		s.writeIngressError(w, config.DialectGemini, *rerr)
		return rerr.Status
	}
	url, rerr := transport.GenerationEndpoint(p, dest)
	if rerr != nil {
		s.writeIngressError(w, config.DialectGemini, *rerr)
		return rerr.Status
	}
	// Destination is OpenAI Chat: only openai-beta crosses. Client Gemini
	// credentials (headers and ?key=) are auth-only and never forwarded.
	headers := transport.OutboundHeaders(r.Header, p, config.DialectGemini, dest, sess.ID, sess.Has)
	res, rerr := s.up.Do(r.Context(), p, s.cfg.Retry, url, headers, upstreamBody, gen.Stream, s.cfg.Server.MaxBodyBytes)
	if rerr != nil {
		if res != nil && res.Failed {
			forwardRetryHints(w, res.Header)
			rerr = upstreamDetail(res.Status, res.Body)
		}
		s.writeIngressError(w, config.DialectGemini, *rerr)
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
		translated, rerr := protocol.TranslateChatJSONToGeminiWithPolicy(res.Body, call.name, lim, p.IsLossy())
		if rerr != nil {
			s.writeIngressError(w, config.DialectGemini, *rerr)
			return rerr.Status
		}
		if int64(len(translated)) > s.cfg.Server.MaxBodyBytes {
			s.writeIngressError(w, config.DialectGemini, protocol.RouterError{Status: 502,
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
	return s.copyGeminiToChatStream(w, r, res, call.name, alt == "sse", lossHeader, p.IsLossy())
}

// geminiDownstreamCommitter is a minimal explicit lazy writer/commit wrapper
// for the translated Gemini downstream. No headers are committed before the
// first valid encoder write: WriteHeader(200) plus framing, loss header and
// safe retry hints are set on the first Write. The framing is fixed at
// construction from the client's requested alt: SSE (?alt=sse) commits
// text/event-stream, otherwise the incremental JSON array commits
// application/json. Before commitment a translator failure maps to an
// ordinary Gemini HTTP 502/504; after commitment the status is kept. Owns
// the native downstream content type, retry hints, loss header and write
// deadline; no larger framework.
type geminiDownstreamCommitter struct {
	w           http.ResponseWriter
	rc          *http.ResponseController
	timeout     time.Duration
	lossHeader  string
	retryHeader http.Header
	sse         bool
	committed   bool
	failed      bool
}

func (c *geminiDownstreamCommitter) Write(p []byte) (int, error) {
	if c.failed {
		return 0, fmt.Errorf("downstream write failed")
	}
	if !c.committed {
		if c.sse {
			c.w.Header().Set("Content-Type", "text/event-stream")
		} else {
			c.w.Header().Set("Content-Type", "application/json")
		}
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
	return n, nil
}

// copyGeminiToChatStream translates an upstream Chat SSE stream into the
// requested Gemini downstream framing with commitment awareness and bounded
// downstream writes. It never buffers the whole generation and never
// repairs/retries. Post-commit failures keep 200: SSE emits one sanitized
// error frame, the array ends without ']' so the client detects truncation.
// Neither framing fabricates a finish, usage or success marker.
func (s *Server) copyGeminiToChatStream(w http.ResponseWriter, r *http.Request, res *transport.Result, publicModel string, sse bool, lossHeader string, allowPrivateDrop bool) int {
	lim := protocol.DefaultLimits()
	var translate func(io.Reader, io.Writer, func()) *protocol.RouterError
	if sse {
		tr := &protocol.ChatToGeminiSSETranslator{PublicModel: publicModel, Limits: lim, AllowPrivateReasoningDrop: allowPrivateDrop}
		translate = tr.Translate
	} else {
		tr := &protocol.ChatToGeminiJSONArrayTranslator{PublicModel: publicModel, Limits: lim, AllowPrivateReasoningDrop: allowPrivateDrop}
		translate = tr.Translate
	}
	return s.copyGeminiDownstream(w, r, res, sse, lossHeader, translate)
}

// copyGeminiDownstream is the canonical Gemini downstream copy flow shared
// by the Chat, Responses and Anthropic upstream bridges (third identical
// flow factored, no generic framework). The caller selects the translate
// callback from the client framing (SSE vs incremental JSON array); this
// helper owns the lazy Gemini committer, typed read tracker, sticky
// flush/write failure handling, deadline reset on every exit, slot reclaim
// via the caller context, and upstream close on all paths. Pre-commit
// failures map to ordinary Gemini 502/504; post-commit failures keep 200
// with one sanitized data:{error} frame (SSE) or an unclosed array (JSON),
// never STOP/DONE/usage fabrication, no goroutines, no whole-generation
// buffer, no dry-run/retry/repair.
func (s *Server) copyGeminiDownstream(w http.ResponseWriter, r *http.Request, res *transport.Result, sse bool, lossHeader string, translate func(io.Reader, io.Writer, func()) *protocol.RouterError) int {
	timeout := time.Duration(s.cfg.Server.WriteTimeout * float64(time.Second))
	rc := http.NewResponseController(w)
	// The write deadline always resets for keep-alive reuse, including the
	// best-effort post-commit error path below.
	if timeout > 0 {
		defer rc.SetWriteDeadline(time.Time{})
	}
	committer := &geminiDownstreamCommitter{
		w: w, rc: rc, timeout: timeout,
		lossHeader: lossHeader, retryHeader: res.Header, sse: sse,
	}
	tracker := &anthropicStreamReadTracker{src: res.Stream}
	flushFailed := false
	flush := func() {
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
	}
	rerr := translate(tracker, committer, flush)
	if rerr != nil || flushFailed || committer.failed {
		if !committer.committed {
			// Pre-commit: ordinary Gemini HTTP error, never an empty 200.
			// A stalled body past the absolute or provider deadline is a
			// 504; truncation, invalid frames and client cancellation keep
			// the existing 502 semantics.
			tracker.Close()
			if isPrecommitStreamTimeout(tracker, r.Context()) {
				s.writeIngressError(w, config.DialectGemini, protocol.RouterError{Status: 504,
					Code: "request_deadline_exceeded",
					Msg:  "Overall upstream request deadline exceeded; generation may have been accepted and billed."})
				return 504
			}
			s.writeIngressError(w, config.DialectGemini, *rerrOrUpstream(rerr))
			if rerr != nil {
				return rerr.Status
			}
			return 502
		}
		// Post-commit: keep 200. When the downstream already failed, never
		// attempt another raw write that could stall; close upstream
		// promptly instead.
		if committer.failed || flushFailed {
			tracker.Close()
			return 200
		}
		if sse {
			// Best effort: one sanitized error frame in the google.rpc
			// shape. No [DONE], no invented STOP or usage.
			emitGeminiStreamError(committer, rc, timeout)
		}
		// Array framing ends without ']' or a terminal object so the
		// client detects truncation; never repair or retry.
		tracker.Close()
		return 200
	}
	return 200
}

// emitGeminiStreamError best-effort writes one data:{error} frame in the
// shared sanitized google.rpc.Status shape after commitment. A write failure
// here is terminal.
func emitGeminiStreamError(c *geminiDownstreamCommitter, rc *http.ResponseController, timeout time.Duration) {
	frame := append([]byte("data: "), geminiErrorPayload(protocol.RouterError{Status: 502,
		Code: protocol.CodeUpstreamBad, Msg: "Upstream stream reported an error"})...)
	frame = append(frame, '\n', '\n')
	if timeout > 0 {
		rc.SetWriteDeadline(time.Now().Add(timeout))
	}
	_, _ = c.w.Write(frame)
	_ = rc.Flush()
}
