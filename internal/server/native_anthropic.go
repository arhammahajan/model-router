package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/arhammahajan/model-router/internal/config"
	"github.com/arhammahajan/model-router/internal/protocol"
	"github.com/arhammahajan/model-router/internal/router"
	"github.com/arhammahajan/model-router/internal/transport"
)

// handleMessages serves POST /v1/messages (and the /messages alias). Native
// Anthropic routes forward opaquely; Anthropic->Chat, Anthropic->Responses
// and Anthropic->Gemini routes translate via serveAnthropicTranslated,
// serveAnthropicToResponses and serveAnthropicToGemini. Other cross-dialect
// routes are refused before upstream I/O via
// dispatchNativeOrTranslated.
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	status := 0
	defer func() {
		log.Printf("route=anthropic status=%d duration_ms=%d", status, time.Since(start).Milliseconds())
	}()
	p, code := s.startPreamble(w, r, config.DialectAnthropic, true)
	if p == nil {
		status = code
		return
	}
	defer p.cancel()
	r = r.WithContext(p.ctx)

	dest := p.model.EffectiveDialect(p.provider)
	mode, rerr := dispatchNativeOrTranslated(config.DialectAnthropic, dest)
	if rerr != nil {
		status = rerr.Status
		s.writeIngressError(w, config.DialectAnthropic, *rerr)
		return
	}
	sess, effBody, serr := prepareSession(p.provider, r.Header, p.body, mode != "native", router.NoCompatFallback())
	if serr != nil {
		status = serr.Status
		s.writeIngressError(w, config.DialectAnthropic, *serr)
		return
	}
	// Usage accounting hook, mirroring handleChat: one wrap per ingress,
	// finished after the dispatch returns with the downstream bytes
	// Anthropic-shaped on every exit of this ingress.
	if s.acc != nil {
		var probe map[string]json.RawMessage
		_ = json.Unmarshal(effBody, &probe)
		var wantStream bool
		_ = json.Unmarshal(probe["stream"], &wantStream)
		var finishAccounting func(int)
		w, finishAccounting = s.beginAccounting(w, config.DialectAnthropic, dest, p.model.Name, p.provider.Name, wantStream, start)
		defer func() { finishAccounting(status) }()
	}
	if mode == "anthropic_to_chat" {
		status = s.serveAnthropicTranslated(w, r, p.provider, p.model, effBody, sess, p.snap)
		return
	}
	if mode == "anthropic_to_responses" {
		status = s.serveAnthropicToResponses(w, r, p.provider, p.model, effBody, sess, p.snap)
		return
	}
	if mode == "anthropic_to_gemini" {
		status = s.serveAnthropicToGemini(w, r, p.provider, p.model, effBody, sess, p.snap)
		return
	}
	if mode != "native" {
		status = 400
		s.writeIngressError(w, config.DialectAnthropic, protocol.RouterError{Status: 400,
			Code: "unsupported_route",
			Msg:  fmt.Sprintf("Upstream dialect %q is not implemented for this ingress; use a same-protocol route", dest)})
		return
	}
	upstreamBody, rerr := rewriteModelAlias(effBody, p.model.Upstream)
	if rerr != nil {
		status = rerr.Status
		s.writeIngressError(w, config.DialectAnthropic, *rerr)
		return
	}
	var probe map[string]json.RawMessage
	_ = json.Unmarshal(effBody, &probe)
	var wantStream bool
	_ = json.Unmarshal(probe["stream"], &wantStream)
	url, rerr := transport.UpstreamURL(p.provider.BaseURL, dest)
	if rerr != nil {
		status = rerr.Status
		s.writeIngressError(w, config.DialectAnthropic, *rerr)
		return
	}
	status = s.serveNativeForward(w, r, p.provider, upstreamBody, wantStream, true, url, config.DialectAnthropic, dest, sess)
}

// handleMessagesSubresource answers paths under /v1/messages/* (other than
// the exact count_tokens routes, which win by longest mux match) with an
// explicit Anthropic-shape 404.
func (s *Server) handleMessagesSubresource(w http.ResponseWriter, r *http.Request) {
	s.writeAnthropicError(w, 404, "not_found_error", "Not implemented in this build")
}
