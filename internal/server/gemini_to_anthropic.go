// Package server Gemini ingress -> Anthropic upstream translation.
//
// The request path is decode -> plan -> enforce policy -> encode once: the
// Gemini body is decoded into the typed IR under the Anthropic-destination
// policy (the public model and streaming mode come from the parsed URL and
// method, never the body), losses are enforced against the provider
// compatibility only (the client cannot select lossy), and the plan is
// encoded once as an Anthropic request for the upstream model. No upstream
// I/O happens before the policy decision. Absent max_tokens leaves the
// existing EncodeAnthropicRequest bridge default (4096); no
// model-detection is performed.
//
// The upstream URL is the plain Anthropic /v1/messages endpoint: never
// forward the client key/alt, and only destination-dialect opt-ins cross.
// JSON responses translate to a proper GenerateContentResponse with the
// public model version from call.name; streams translate incrementally
// with commitment awareness into the framing the client asked for:
// ?alt=sse yields text/event-stream data-only frames, a missing alt or
// alt=json yields one incremental JSON array (application/json). The
// Anthropic upstream is always SSE, so the downstream framing selects the
// AnthropicToGemini translator. Before the first encoder write no
// downstream headers are committed, so early upstream
// truncation/invalid/error surfaces as an ordinary Gemini HTTP 502/504.
// After commitment the status is kept: SSE terminates with one sanitized
// data:{error:google.rpc.Status} frame (no [DONE], no invented STOP or
// usage), and the array terminates without the closing ']' so the client
// detects truncation instead of reading a manufactured success.
package server

import (
	"io"
	"net/http"

	"github.com/arhammahajan/router/internal/capability"
	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/protocol"
	"github.com/arhammahajan/router/internal/router"
	"github.com/arhammahajan/router/internal/transport"
)

// serveGeminiToAnthropic bridges a Gemini ingress path
// (/v1beta/models/{name}:generateContent or :streamGenerateContent, plus the
// /v1, version-less and tunedModels forms) to an anthropic upstream.
func (s *Server) serveGeminiToAnthropic(w http.ResponseWriter, r *http.Request, p *config.Provider, model *config.Model, body []byte, call geminiCall, alt string, sess router.Session, compatEffort protocol.ReasoningIntent) int {
	lim := protocol.DefaultLimits()
	stream := call.method == "streamGenerateContent"
	gen, losses, rerr := protocol.DecodeGeminiForAnthropic(body, call.name, stream, lim)
	if rerr != nil {
		s.writeIngressError(w, config.DialectGemini, *rerr)
		return rerr.Status
	}
	if compatEffort.IsPresent() && gen.Reasoning.IsPresent() {
		rerr := protocol.RouterError{Status: 400, Code: protocol.CodeInvalidGeneration, Msg: "Router effort cannot be combined with Gemini thinking controls"}
		s.writeIngressError(w, config.DialectGemini, rerr)
		return rerr.Status
	}
	// Deliberate terminal policy: the validated compat carrier is
	// diagnosed dropped through the single shared planner with the
	// explicit closed Anthropic destination. No adapter approximation.
	if compatEffort.IsPresent() {
		_, planLosses := protocol.PlanEffort(compatEffort, capability.ReasoningCapabilities{}, protocol.CompatAnthropicRoute())
		losses = append(losses, planLosses...)
	}
	kept, rerr := protocol.EnforcePolicy(losses, p.IsLossy())
	if rerr != nil {
		s.writeIngressError(w, config.DialectGemini, *rerr)
		return rerr.Status
	}
	upstreamBody, rerr := protocol.EncodeAnthropicRequest(gen, model.Upstream)
	if rerr != nil {
		s.writeIngressError(w, config.DialectGemini, *rerr)
		return rerr.Status
	}
	dest := p.ResolveDialect(config.DialectGemini, model.Dialect)
	url, rerr := transport.GenerationEndpoint(p, dest)
	if rerr != nil {
		s.writeIngressError(w, config.DialectGemini, *rerr)
		return rerr.Status
	}
	// Destination is Anthropic: only anthropic-version/beta opt-ins cross.
	// Client Gemini credentials (headers and ?key=) are auth-only and never
	// forwarded, and no ?alt= query ever travels upstream.
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
		translated, rerr := protocol.TranslateAnthropicJSONToGeminiWithPolicy(res.Body, call.name, lim, p.IsLossy())
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
	return s.copyGeminiToAnthropicStream(w, r, res, call.name, alt == "sse", lossHeader, p.IsLossy())
}

// copyGeminiToAnthropicStream translates an upstream Anthropic SSE stream
// into the requested Gemini downstream framing via the canonical shared
// copy flow. The Anthropic upstream is always SSE, so the client framing
// selects the translator: SSE (?alt=sse) or the incremental JSON array.
func (s *Server) copyGeminiToAnthropicStream(w http.ResponseWriter, r *http.Request, res *transport.Result, publicModel string, sse bool, lossHeader string, allowGatewayDrop bool) int {
	lim := protocol.DefaultLimits()
	var translate func(io.Reader, io.Writer, func()) *protocol.RouterError
	if sse {
		tr := &protocol.AnthropicToGeminiSSETranslator{PublicModel: publicModel, Limits: lim, AllowGatewayBillingDrop: allowGatewayDrop}
		translate = tr.Translate
	} else {
		tr := &protocol.AnthropicToGeminiJSONArrayTranslator{PublicModel: publicModel, Limits: lim, AllowGatewayBillingDrop: allowGatewayDrop}
		translate = tr.Translate
	}
	return s.copyGeminiDownstream(w, r, res, sse, lossHeader, translate)
}
