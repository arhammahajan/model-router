package server

import (
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/arhammahajan/model-router/internal/capability"
	"github.com/arhammahajan/model-router/internal/config"
	"github.com/arhammahajan/model-router/internal/protocol"
	"github.com/arhammahajan/model-router/internal/router"
	"github.com/arhammahajan/model-router/internal/transport"
)

// serveChatToGemini bridges POST /v1/chat/completions to a Gemini upstream.
// The request path is decode -> plan -> enforce policy -> encode once: the
// Chat body is decoded into the typed IR with the Gemini-destination loss
// plan, losses are enforced against the provider compatibility only (the
// client cannot select lossy), and the plan is encoded once as a Gemini
// generateContent request for the upstream model. No upstream I/O happens
// before the policy decision.
//
// The model and streaming mode live in the upstream URL: non-streaming uses
// generateContent, streaming uses streamGenerateContent with ?alt=sse. The
// upstream may still answer a stream with the standard JSON array; the
// framing decoder is selected from the reviewed upstream Content-Type, never
// by sniffing the body. JSON responses translate to Chat Completions with
// the public model name; SSE streams translate incrementally with commitment
// awareness into the shared lazy Chat committer (chat_to_responses.go):
// before the first encoder write no downstream headers are committed, so
// early upstream truncation/invalid/error surfaces as an ordinary Chat HTTP
// 502. After commitment the status is kept and the stream terminates without
// a synthetic success marker: no [DONE], no fabricated finish chunk, and no
// error frame beyond what the translator already emitted.
func (s *Server) serveChatToGemini(w http.ResponseWriter, r *http.Request, p *config.Provider, model *config.Model, body []byte, sess router.Session, snap *router.Snapshot) int {
	lim := protocol.DefaultLimits()
	gen, losses, rerr := protocol.DecodeChatForGemini(body, lim)
	if rerr != nil {
		s.writeChatError(w, *rerr)
		return rerr.Status
	}
	// Same pinned snapshot plans the closed Chat effort intent for the
	// Gemini destination: exact
	// generationConfig.thinkingConfig.thinkingLevel only on explicit
	// reasoning.effort.supported=true plus the exact gemini native
	// descriptor plus the reviewed known enum plus the official wire enum,
	// otherwise a diagnosed drop. Strict refuses pre-I/O via EnforcePolicy,
	// lossy drops and reports. No model-name, family or URL heuristic is
	// ever consulted. Numeric budgets keep their existing exact
	// thinkingBudget path; native same-protocol stays raw bypass.
	contract := snap.RequestCapability(model, p)
	caps := capability.ReasoningFor(contract)
	planned, planLosses := protocol.PlanEffort(gen.Reasoning, caps, protocol.OrdinaryGeminiRoute())
	losses = append(losses, planLosses...)
	kept, rerr := protocol.EnforcePolicy(losses, p.IsLossy())
	if rerr != nil {
		s.writeChatError(w, *rerr)
		return rerr.Status
	}
	upstreamBody, rerr := protocol.EncodeGeminiRequestWithPlan(gen, planned)
	if rerr != nil {
		s.writeChatError(w, *rerr)
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
		s.writeChatError(w, *rerr)
		return rerr.Status
	}
	// Destination is Gemini: the Gemini dialect accepts no inbound headers,
	// so only provider statics, the configured x-goog-api-key credential
	// and framing cross. Client credentials and query keys never leak.
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
		translated, rerr := protocol.TranslateGeminiJSONToChatWithPlan(res.Body, model.Name, lim, gen.ToolPlan)
		if rerr != nil {
			s.writeChatError(w, *rerr)
			return rerr.Status
		}
		if int64(len(translated)) > s.cfg.Server.MaxBodyBytes {
			s.writeChatError(w, protocol.RouterError{Status: 502, Code: protocol.CodeUpstreamBad,
				Msg: "Upstream response exceeds the size bound"})
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
	return s.copyChatToGeminiStream(w, r, res, model.Name, gen.StreamIncludeUsage, lossHeader, gen.ToolPlan)
}

// copyChatToGeminiStream translates an upstream Gemini stream into Chat
// Completions SSE with commitment awareness and bounded downstream writes.
// The framing decoder is selected from the reviewed upstream Content-Type:
// text/event-stream uses the SSE translator, application/json the incremental
// JSON-array translator. Any other content type is an ordinary pre-commit
// 502: the body is never probed or buffered to guess the framing, and raw
// upstream bytes never leak into translated errors. It never buffers the
// whole generation and never repairs/retries. Post-commit failures terminate
// the stream silently without [DONE]; the translator emits only reviewed
// Chat chunks, so the boundary adds no error frame.
func (s *Server) copyChatToGeminiStream(w http.ResponseWriter, r *http.Request, res *transport.Result, publicModel string, includeUsage bool, lossHeader string, plan *protocol.ToolPlan) int {
	media := upstreamMediaType(res.Header.Get("Content-Type"))
	lim := protocol.DefaultLimits()
	var translate func(io.Reader, io.Writer, func()) *protocol.RouterError
	switch media {
	case "text/event-stream":
		tr := &protocol.GeminiToChatSSETranslator{PublicModel: publicModel,
			IncludeUsage: includeUsage, Limits: lim, Plan: plan}
		translate = tr.Translate
	case "application/json":
		tr := &protocol.GeminiToChatJSONArrayTranslator{PublicModel: publicModel,
			IncludeUsage: includeUsage, Limits: lim, Plan: plan}
		translate = tr.Translate
	default:
		// Unsupported encoding or protocol shape: ordinary pre-commit 502.
		// The upstream body is closed promptly on every path.
		res.Stream.Close()
		s.writeChatError(w, protocol.RouterError{Status: 502, Code: protocol.CodeUpstreamBad,
			Msg: "Upstream response encoding is not supported"})
		return 502
	}
	timeout := time.Duration(s.cfg.Server.WriteTimeout * float64(time.Second))
	rc := http.NewResponseController(w)
	// The write deadline always resets for keep-alive reuse.
	if timeout > 0 {
		defer rc.SetWriteDeadline(time.Time{})
	}
	// The translated Chat SSE committer is shared with the Chat->Responses
	// path: lazy commit, silent post-commit abort, no [DONE], no fabricated
	// finish chunk. No duplicated committer.
	committer := &chatDownstreamCommitter{
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

// upstreamMediaType returns the lowercased media type of a Content-Type
// header value with any parameters (charset, boundary) stripped. An empty
// or unparseable value yields "" so the caller fails closed.
func upstreamMediaType(contentType string) string {
	media, _, _ := strings.Cut(contentType, ";")
	return strings.ToLower(strings.TrimSpace(media))
}
