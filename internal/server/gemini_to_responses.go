package server

import (
	"io"
	"net/http"

	"github.com/arhammahajan/model-router/internal/capability"
	"github.com/arhammahajan/model-router/internal/config"
	"github.com/arhammahajan/model-router/internal/protocol"
	"github.com/arhammahajan/model-router/internal/router"
	"github.com/arhammahajan/model-router/internal/transport"
)

// serveGeminiToResponses bridges a Gemini ingress path
// (/v1beta/models/{name}:generateContent or :streamGenerateContent, plus the
// /v1, version-less and tunedModels forms) to an openai-responses upstream.
// The request path is decode -> plan -> enforce policy -> encode once: the
// Gemini body is decoded into the typed IR under the Responses-destination
// policy (the public model and streaming mode come from the parsed URL and
// method, never the body), losses are enforced against the provider
// compatibility only (the client cannot select lossy), and the plan is
// encoded once as a Responses request for the upstream model. No upstream
// I/O happens before the policy decision.
//
// The upstream URL is the plain Responses /responses endpoint: no forwarded
// alt and no client ?key= credential ever travel upstream, and
// EncodeResponsesRequest forces store:false so no upstream conversation
// state is reused. JSON responses translate to a proper
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
func (s *Server) serveGeminiToResponses(w http.ResponseWriter, r *http.Request, p *config.Provider, model *config.Model, body []byte, call geminiCall, alt string, sess router.Session, snap *router.Snapshot, compatEffort protocol.ReasoningIntent) int {
	lim := protocol.DefaultLimits()
	stream := call.method == "streamGenerateContent"
	gen, losses, rerr := protocol.DecodeGeminiForResponses(body, call.name, stream, lim)
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
	// Same pinned snapshot as the Chat bridge: plan the closed intent
	// for the Responses destination through the shared planner. Explicit
	// budgets drop as diagnosed losses here, never as encoder misuse.
	contract := snap.RequestCapability(model, p)
	caps := capability.ReasoningFor(contract)
	scope := protocol.OrdinaryResponsesRoute()
	if snap.HasLlamaProvider(p.Name) {
		scope = protocol.LocalResponsesRoute(snap.LlamaRuntime(p.Name))
	}
	planned, planLosses := protocol.PlanEffort(intent, caps, scope)
	losses = append(losses, planLosses...)
	kept, rerr := protocol.EnforcePolicy(losses, p.IsLossy())
	if rerr != nil {
		s.writeIngressError(w, config.DialectGemini, *rerr)
		return rerr.Status
	}
	upstreamBody, rerr := protocol.EncodeResponsesRequest(gen, planned, model.Upstream)
	if rerr != nil {
		s.writeIngressError(w, config.DialectGemini, *rerr)
		return rerr.Status
	}
	dest := model.EffectiveDialect(p)
	url, rerr := transport.UpstreamURL(p.BaseURL, dest)
	if rerr != nil {
		s.writeIngressError(w, config.DialectGemini, *rerr)
		return rerr.Status
	}
	// Destination is OpenAI Responses: only openai-beta crosses. Client
	// Gemini credentials (headers and ?key=) are auth-only and never
	// forwarded.
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
		translated, rerr := protocol.TranslateResponsesJSONToGeminiWithPolicy(res.Body, call.name, lim, p.IsLossy())
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
	return s.copyGeminiToResponsesStream(w, r, res, call.name, alt == "sse", lossHeader, p.IsLossy())
}

// copyGeminiToResponsesStream translates an upstream Responses SSE stream
// into the requested Gemini downstream framing via the canonical shared
// copy flow. The caller selects the Responses->Gemini translator from the
// client framing; commitment, deadlines and truncation semantics live in
// copyGeminiDownstream.
func (s *Server) copyGeminiToResponsesStream(w http.ResponseWriter, r *http.Request, res *transport.Result, publicModel string, sse bool, lossHeader string, allowPrivateDrop bool) int {
	lim := protocol.DefaultLimits()
	var translate func(io.Reader, io.Writer, func()) *protocol.RouterError
	if sse {
		tr := &protocol.ResponsesToGeminiSSETranslator{PublicModel: publicModel, Limits: lim, AllowPrivateReasoningDrop: allowPrivateDrop}
		translate = tr.Translate
	} else {
		tr := &protocol.ResponsesToGeminiJSONArrayTranslator{PublicModel: publicModel, Limits: lim, AllowPrivateReasoningDrop: allowPrivateDrop}
		translate = tr.Translate
	}
	return s.copyGeminiDownstream(w, r, res, sse, lossHeader, translate)
}
