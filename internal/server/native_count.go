package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/arhammahajan/model-router/internal/config"
	"github.com/arhammahajan/model-router/internal/protocol"
	"github.com/arhammahajan/model-router/internal/transport"
)

// handleCountTokens serves POST /v1/messages/count_tokens (and the
// /messages/count_tokens alias) by forwarding to an Anthropic token-count
// endpoint. Only a provider declared (count_tokens=true) or exactly matching
// the first-party https://api.anthropic.com host is eligible; every other
// route is refused before upstream I/O with an Anthropic-shape 400 and no
// local estimate. Counting holds no generation slot and never calls a
// generation endpoint.
func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	status := 0
	defer func() {
		log.Printf("route=count_tokens status=%d duration_ms=%d", status, time.Since(start).Milliseconds())
	}()
	if r.Method != http.MethodPost {
		status = 405
		s.writeIngressError(w, config.DialectAnthropic, protocol.RouterError{Status: 405,
			Code: protocol.CodeInvalidRequest, Msg: "Method not allowed"})
		return
	}
	if !s.authedQuery(r, false) {
		status = 401
		s.writeIngressError(w, config.DialectAnthropic, protocol.RouterError{Status: 401,
			Code: protocol.CodeUnauthorized, Msg: "Invalid API key"})
		return
	}
	// No generation slot: counting must not exhaust generation capacity.
	// Absolute deadline still bounds dispatch, attempts and body reads;
	// ingress body reads stay on the separate body_read_timeout.
	deadline := start.Add(time.Duration(s.cfg.Retry.Deadline * float64(time.Second)))
	reqCtx, reqCancel := context.WithDeadline(r.Context(), deadline)
	defer reqCancel()
	body, rerr := s.readBody(w, r.WithContext(reqCtx))
	if rerr != nil {
		status = rerr.Status
		s.writeIngressError(w, config.DialectAnthropic, *rerr)
		return
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		status = 400
		s.writeIngressError(w, config.DialectAnthropic, protocol.RouterError{Status: 400,
			Code: protocol.CodeInvalidRequest, Msg: "'body' must be a JSON object"})
		return
	}
	var modelName string
	_ = json.Unmarshal(probe["model"], &modelName)
	model, provider, merr := s.resolveModel(modelName)
	if merr != nil {
		status = merr.Status
		s.writeIngressError(w, config.DialectAnthropic, *merr)
		return
	}
	dest := model.EffectiveDialect(provider)
	if dest != config.DialectAnthropic || !transport.CountTokensEnabled(provider) {
		status = 400
		s.writeIngressError(w, config.DialectAnthropic, protocol.RouterError{Status: 400,
			Code: "count_tokens_unsupported",
			Msg:  "Exact token counting is unavailable for this model. The router does not estimate token counts."})
		return
	}
	upstreamBody, rerr := rewriteModelAlias(body, model.Upstream)
	if rerr != nil {
		status = rerr.Status
		s.writeIngressError(w, config.DialectAnthropic, *rerr)
		return
	}
	url, rerr := transport.CountTokensURL(provider.BaseURL)
	if rerr != nil {
		status = rerr.Status
		s.writeIngressError(w, config.DialectAnthropic, *rerr)
		return
	}
	headers := transport.OutboundHeaders(r.Header, provider, config.DialectAnthropic, dest, "", false)
	res, rerr := s.up.Do(reqCtx, provider, s.cfg.Retry, url, headers, upstreamBody, false, MaxCountResponseBytes)
	if rerr != nil {
		if res != nil && res.Failed {
			// Error bodies are untrusted diagnostics: forward only retry
			// hints and a stable sanitized message, never raw bytes.
			forwardRetryHints(w, res.Header)
			status = res.Status
			typ := anthropicErrorTypes[res.Status]
			if typ == "" {
				typ = "api_error"
				if res.Status < 500 {
					typ = "invalid_request_error"
				}
			}
			s.writeAnthropicError(w, res.Status, typ, "Upstream returned an error")
			return
		}
		status = rerr.Status
		s.writeIngressError(w, config.DialectAnthropic, *rerr)
		return
	}
	if tokens, ok := validCountTokens(res.Body); !ok {
		status = 502
		s.writeIngressError(w, config.DialectAnthropic, protocol.RouterError{Status: 502,
			Code: protocol.CodeUpstreamBad, Msg: "The upstream did not return a valid token count"})
		return
	} else {
		_ = tokens
	}
	for name, value := range filteredResponseHeaders(res.Header) {
		w.Header().Set(name, value)
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	s.writeJSON(w, res.Status, res.Body)
	status = res.Status
}

// validCountTokens accepts only the documented success shape
// {"input_tokens": n} with a non-negative integer n. Bool, float, string,
// missing and negative values are all invalid; there is no local estimate.
func validCountTokens(data []byte) (int64, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return 0, false
	}
	raw, ok := obj["input_tokens"]
	if !ok {
		return 0, false
	}
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return 0, false
	}
	// Plain non-negative integer literal only: no quotes, fraction,
	// exponent or sign. Bool/float spellings fail here.
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	if len(s) > 1 && s[0] == '0' {
		// Leading zeros are not canonical JSON numbers; the baseline only
		// documents plain counts, so refuse them rather than guessing.
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
