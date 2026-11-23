package server

import (
	"fmt"
	"net/http"
	"time"

	"github.com/arhammahajan/router/internal/capability"
	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/protocol"
	"github.com/arhammahajan/router/internal/router"
	"github.com/arhammahajan/router/internal/transport"
)

// serveChatToResponses bridges POST /v1/chat/completions to an
// openai-responses upstream. The request path is decode -> plan -> enforce
// policy -> encode once: the Chat body is decoded into the typed IR with the
// Responses-destination loss plan, the pinned snapshot's canonical capability
// facts plan the closed effort target, losses are enforced against the
// provider compatibility only (the client cannot select lossy), and the plan
// is encoded once as a Responses request for the upstream model. No upstream
// I/O happens before the policy decision. The same pinned snapshot supplies
// route, provider and capability facts; the handler never repins or trusts
// metadata.router.
//
// The response path reuses the shared transport with the upstream /responses
// endpoint. JSON responses translate to Chat Completions with the public
// model name; SSE streams translate incrementally with commitment awareness:
// before the first encoder write no downstream headers are committed, so
// early upstream truncation/invalid/error surfaces as an ordinary Chat HTTP
// 502. After commitment the status is kept and the stream terminates without
// a synthetic success marker: no [DONE], no fabricated finish_reason, and no
// error frame beyond what the translator already emitted.
func (s *Server) serveChatToResponses(w http.ResponseWriter, r *http.Request, p *config.Provider, model *config.Model, body []byte, sess router.Session, snap *router.Snapshot) int {
	lim := protocol.DefaultLimits()
	gen, losses, rerr := protocol.DecodeChatForResponses(body, lim)
	if rerr != nil {
		s.writeChatError(w, *rerr)
		return rerr.Status
	}
	contract := snap.RequestCapability(model, p)
	caps := capability.ReasoningFor(contract)
	// Chat ingress to a generated Chat provider never reaches this
	// translated pair: handleChat routes same-protocol destinations to
	// serveNative. An explicit route cannot name a generated provider
	// (config rejects unknown providers), so the forward scope is always
	// ordinary here; the reverse bridge owns local native controls.
	planned, planLosses := protocol.PlanEffort(gen.Reasoning, caps, protocol.OrdinaryRoute())
	losses = append(losses, planLosses...)
	kept, rerr := protocol.EnforcePolicy(losses, p.IsLossy())
	if rerr != nil {
		s.writeChatError(w, *rerr)
		return rerr.Status
	}
	upstreamBody, rerr := protocol.EncodeResponsesRequest(gen, planned, model.Upstream)
	if rerr != nil {
		s.writeChatError(w, *rerr)
		return rerr.Status
	}
	dest := model.EffectiveDialect(p)
	url, rerr := transport.UpstreamURL(p.BaseURL, dest)
	if rerr != nil {
		s.writeChatError(w, *rerr)
		return rerr.Status
	}
	headers := transport.OutboundHeaders(r.Header, p, "chat", dest, sess.ID, sess.Has)
	res, rerr := s.up.Do(r.Context(), p, s.cfg.Retry, url, headers, upstreamBody, gen.Stream, s.cfg.Server.MaxBodyBytes)
	if rerr != nil {
		if res != nil && res.Failed {
			forwardRetryHints(w, res.Header)
			rerr = upstreamDetail(res.Status, res.Body)
		}
		s.writeChatError(w, *rerr)
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
		translated, rerr := protocol.TranslateResponsesJSONToChatWithPlanAndPolicy(res.Body, model.Name, lim, gen.ToolPlan, p.IsLossy())
		if rerr != nil {
			s.writeChatError(w, *rerr)
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
	return s.copyChatToResponsesStream(w, r, res, model.Name, gen.StreamIncludeUsage, lossHeader, gen.ToolPlan, p.IsLossy())
}

// chatDownstreamCommitter is a minimal explicit lazy writer/commit wrapper
// for the translated Chat SSE path. No headers are committed before the
// first valid encoder write: WriteHeader(200) plus SSE framing, loss header
// and safe retry hints are set on the first Write. Before commitment a
// translator failure maps to an ordinary Chat HTTP 502; after commitment the
// status is kept and the stream terminates without a synthetic [DONE] or a
// fabricated terminal chunk.
type chatDownstreamCommitter struct {
	w           http.ResponseWriter
	rc          *http.ResponseController
	timeout     time.Duration
	lossHeader  string
	retryHeader http.Header
	committed   bool
	failed      bool
}

func (c *chatDownstreamCommitter) Write(p []byte) (int, error) {
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
	return n, nil
}

// copyChatToResponsesStream translates an upstream Responses SSE stream into
// Chat Completions SSE with commitment awareness and bounded downstream
// writes. It never buffers the whole generation and never repairs/retries.
// Post-commit failures terminate the stream without [DONE]; the translator
// emits only reviewed Chat chunks, so the boundary adds no error frame.
func (s *Server) copyChatToResponsesStream(w http.ResponseWriter, r *http.Request, res *transport.Result, publicModel string, includeUsage bool, lossHeader string, plan *protocol.ToolPlan, allowPrivateDrop bool) int {
	timeout := time.Duration(s.cfg.Server.WriteTimeout * float64(time.Second))
	rc := http.NewResponseController(w)
	// The write deadline always resets for keep-alive reuse.
	if timeout > 0 {
		defer rc.SetWriteDeadline(time.Time{})
	}
	committer := &chatDownstreamCommitter{
		w: w, rc: rc, timeout: timeout,
		lossHeader: lossHeader, retryHeader: res.Header,
	}
	tracker := &anthropicStreamReadTracker{src: res.Stream}
	flushFailed := false
	tr := &protocol.ResponsesToChatSSETranslator{PublicModel: publicModel,
		IncludeUsage: includeUsage, Limits: protocol.DefaultLimits(), Plan: plan, AllowPrivateReasoningDrop: allowPrivateDrop}
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
			// Pre-commit: ordinary Chat HTTP error, never an empty 200.
			// A stalled body past the absolute or provider deadline is a
			// 504; truncation, invalid frames and client cancellation keep
			// the existing 502 semantics.
			tracker.Close()
			if isPrecommitStreamTimeout(tracker, r.Context()) {
				s.writeChatError(w, protocol.RouterError{Status: 504,
					Code: "request_deadline_exceeded",
					Msg:  "Overall upstream request deadline exceeded; generation may have been accepted and billed."})
				return 504
			}
			s.writeChatError(w, *rerrOrUpstream(rerr))
			if rerr != nil {
				return rerr.Status
			}
			return 502
		}
		// Post-commit: keep 200 and terminate without a synthetic success
		// marker. No [DONE], no fabricated finish_reason. Upstream is
		// closed promptly on every path.
		tracker.Close()
		return 200
	}
	return 200
}
