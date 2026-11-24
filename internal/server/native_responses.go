package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/protocol"
	"github.com/arhammahajan/router/internal/provider"
	"github.com/arhammahajan/router/internal/router"
	"github.com/arhammahajan/router/internal/transport"
)

// handleResponses serves POST /v1/responses (and the /responses alias) by
// natively forwarding to an openai-responses upstream. Stateful generation
// fields (previous_response_id, conversation, background, store, ...) pass
// through untouched: the router keeps no conversation store. Responses->Chat,
// Responses->Anthropic and Responses->Gemini routes translate via
// serveResponsesTranslated, serveResponsesToAnthropic and
// serveResponsesToGemini; cross-dialect routes without a codec are refused
// before upstream I/O.
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	status := 0
	defer func() {
		log.Printf("route=responses status=%d duration_ms=%d", status, time.Since(start).Milliseconds())
	}()
	p, code := s.startPreamble(w, r, config.DialectOpenAIResponses, true)
	if p == nil {
		status = code
		return
	}
	defer p.cancel()
	r = r.WithContext(p.ctx)

	dest := p.provider.ResolveDialect(config.DialectOpenAIResponses, p.model.Dialect)
	mode, rerr := dispatchNativeOrTranslated(config.DialectOpenAIResponses, dest)
	if rerr != nil {
		status = rerr.Status
		s.writeIngressError(w, config.DialectOpenAIResponses, *rerr)
		return
	}
	sess, effBody, serr := prepareSession(p.provider, r.Header, p.body, mode != "native", router.NoCompatFallback())
	if serr != nil {
		status = serr.Status
		s.writeIngressError(w, config.DialectOpenAIResponses, *serr)
		return
	}
	// Usage accounting hook, mirroring handleChat: one wrap per ingress,
	// finished after the dispatch returns with the downstream bytes
	// Responses-shaped on every exit of this ingress.
	if s.acc != nil {
		var probe map[string]json.RawMessage
		_ = json.Unmarshal(effBody, &probe)
		var wantStream bool
		_ = json.Unmarshal(probe["stream"], &wantStream)
		var finishAccounting func(int)
		w, finishAccounting = s.beginAccounting(w, config.DialectOpenAIResponses, dest, p.model.Name, p.provider.Name, wantStream, start)
		defer func() { finishAccounting(status) }()
	}
	if mode == "responses_to_chat" {
		status = s.serveResponsesTranslated(w, r, p.provider, p.model, effBody, sess, p.snap)
		return
	}
	if mode == "responses_to_anthropic" {
		status = s.serveResponsesToAnthropic(w, r, p.provider, p.model, effBody, sess)
		return
	}
	if mode == "responses_to_gemini" {
		status = s.serveResponsesToGemini(w, r, p.provider, p.model, effBody, sess, p.snap)
		return
	}
	if mode != "native" {
		status = 400
		s.writeIngressError(w, config.DialectOpenAIResponses, protocol.RouterError{Status: 400,
			Code: "unsupported_route",
			Msg:  fmt.Sprintf("Upstream dialect %q is not implemented for this ingress; use a same-protocol route", dest)})
		return
	}
	upstreamBody, rerr := rewriteModelAlias(effBody, p.model.Upstream)
	if rerr != nil {
		status = rerr.Status
		s.writeIngressError(w, config.DialectOpenAIResponses, *rerr)
		return
	}
	upstreamBody, _ = provider.ForKind(p.provider.Kind).PrepareNativeBody(dest, upstreamBody)
	var probe map[string]json.RawMessage
	_ = json.Unmarshal(effBody, &probe)
	var wantStream bool
	_ = json.Unmarshal(probe["stream"], &wantStream)
	url, rerr := transport.GenerationEndpoint(p.provider, dest)
	if rerr != nil {
		status = rerr.Status
		s.writeIngressError(w, config.DialectOpenAIResponses, *rerr)
		return
	}
	status = s.serveNativeForward(w, r, p.provider, upstreamBody, wantStream, true, url, config.DialectOpenAIResponses, dest, sess)
}

// handleResponsesSubresource answers any verb under /v1/responses/* with an
// explicit Responses-shape 404. There is no conversation store and no
// subresource verbs in this build.
func (s *Server) handleResponsesSubresource(w http.ResponseWriter, r *http.Request) {
	s.writeIngressError(w, config.DialectOpenAIResponses, protocol.RouterError{Status: 404,
		Code: "not_found", Msg: "Not implemented in this build: no Responses subresources"})
}
