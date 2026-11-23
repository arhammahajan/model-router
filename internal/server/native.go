// Package server native endpoints: shared dispatch seam for same-protocol
// forwarding across the four ingress dialects.
//
// Milestone 3 slice: Chat (existing), Responses, Anthropic Messages and
// Gemini generateContent/streamGenerateContent are natively forwarded.
// Cross-dialect routes that have no codec in this build are refused with an
// ingress-native explicit error before any upstream I/O. Bridged pairs:
// Chat ingress -> Anthropic upstream (serveTranslated in server.go),
// Anthropic ingress -> Chat upstream (serveAnthropicTranslated),
// Chat ingress -> Responses upstream (serveChatToResponses),
// Responses ingress -> Chat upstream (serveResponsesTranslated),
// Anthropic ingress -> Responses upstream (serveAnthropicToResponses) and
// Responses ingress -> Anthropic upstream (serveResponsesToAnthropic).
// Chat ingress -> Gemini upstream (serveChatToGemini) and Gemini ingress ->
// Chat upstream (serveGeminiToChat). Responses ingress -> Gemini upstream
// (serveResponsesToGemini) and Gemini ingress -> Responses upstream
// (serveGeminiToResponses). Anthropic ingress -> Gemini upstream
// (serveAnthropicToGemini) and Gemini ingress -> Anthropic upstream
// (serveGeminiToAnthropic). Any other pair plugs into
// dispatchNativeOrTranslated below.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/protocol"
	"github.com/arhammahajan/router/internal/router"
	"github.com/arhammahajan/router/internal/transport"
)

// MaxCountResponseBytes bounds Anthropic count_tokens responses (success and
// error), mirroring the Python baseline's 64 KiB cap.
const MaxCountResponseBytes = 64 * 1024

// Gemini error status names, mirroring the Python router's google.rpc table
// for the statuses this server can produce.
var geminiErrorStatuses = map[int]string{
	400: "INVALID_ARGUMENT",
	401: "UNAUTHENTICATED",
	403: "PERMISSION_DENIED",
	404: "NOT_FOUND",
	408: "DEADLINE_EXCEEDED",
	409: "ABORTED",
	413: "INVALID_ARGUMENT",
	429: "RESOURCE_EXHAUSTED",
	499: "CANCELLED",
	500: "INTERNAL",
	501: "UNIMPLEMENTED",
	502: "UNAVAILABLE",
	503: "UNAVAILABLE",
	504: "DEADLINE_EXCEEDED",
}

// Anthropic error types for the statuses this server can produce.
var anthropicErrorTypes = map[int]string{
	400: "invalid_request_error",
	401: "authentication_error",
	403: "permission_error",
	404: "not_found_error",
	413: "request_too_large",
	429: "rate_limit_error",
	529: "overloaded_error",
}

// registerNativeRoutes adds the milestone-3 native ingress routes. Called
// once from New; the "/" catch-all in server.go stays the final fallback.
func (s *Server) registerNativeRoutes() {
	s.mux.HandleFunc("/v1/responses", s.handleResponses)
	s.mux.HandleFunc("/responses", s.handleResponses)
	s.mux.HandleFunc("/v1/responses/", s.handleResponsesSubresource)
	s.mux.HandleFunc("/responses/", s.handleResponsesSubresource)
	s.mux.HandleFunc("/v1/messages", s.handleMessages)
	s.mux.HandleFunc("/messages", s.handleMessages)
	s.mux.HandleFunc("/v1/messages/count_tokens", s.handleCountTokens)
	s.mux.HandleFunc("/messages/count_tokens", s.handleCountTokens)
	s.mux.HandleFunc("/v1/messages/", s.handleMessagesSubresource)
	s.mux.HandleFunc("/messages/", s.handleMessagesSubresource)
	// Gemini generation paths are dynamic
	// (/v1beta/models/{name}:{method}); prefix patterns catch them and the
	// handler parses the remainder. Version-less /models/ and /tunedModels/
	// forms are accepted like the Python router.
	s.mux.HandleFunc("/v1beta/models/", s.handleGemini)
	s.mux.HandleFunc("/v1beta/tunedModels/", s.handleGemini)
	s.mux.HandleFunc("/v1/models/", s.handleGemini)
	s.mux.HandleFunc("/v1/tunedModels/", s.handleGemini)
	s.mux.HandleFunc("/models/", s.handleGemini)
	s.mux.HandleFunc("/tunedModels/", s.handleGemini)
}

// nativeNotFound reports ingress-specific 404s for subresources and
// Gemini-like paths that reached the "/" catch-all (list paths, unknown
// methods). It reports whether it handled the request.
func (s *Server) nativeNotFound(w http.ResponseWriter, r *http.Request) bool {
	p := r.URL.Path
	if strings.HasPrefix(p, "/v1/responses/") || strings.HasPrefix(p, "/responses/") ||
		p == "/v1/responses/" || p == "/responses/" {
		s.writeResponsesError(w, protocol.RouterError{Status: 404, Code: "not_found",
			Msg: "Not implemented in this build: no Responses subresources"})
		return true
	}
	if strings.HasPrefix(p, "/v1/messages/") || strings.HasPrefix(p, "/messages/") {
		s.writeAnthropicError(w, 404, "not_found_error", "Not implemented in this build")
		return true
	}
	if strings.HasPrefix(p, "/v1beta/") || strings.HasPrefix(p, "/v1/models/") ||
		strings.HasPrefix(p, "/v1/tunedModels/") || strings.HasPrefix(p, "/models/") ||
		strings.HasPrefix(p, "/tunedModels/") ||
		p == "/v1beta/models" || p == "/v1beta/tunedModels" ||
		p == "/v1/models" || p == "/v1/tunedModels" ||
		p == "/models" || p == "/tunedModels" {
		s.writeGeminiError(w, protocol.RouterError{Status: 404, Code: "model_not_found",
			Msg: "Unknown route or unsupported Gemini method; generateContent and streamGenerateContent are supported"})
		return true
	}
	return false
}

// authedQuery is the shared client-auth check. Gemini ingress additionally
// accepts the documented ?key= query credential (and the x-goog-api-key
// header, handled via clientKeys); every other ingress uses headers only.
// Client credentials are only used for this check and are never forwarded.
func (s *Server) authedQuery(r *http.Request, allowQueryKey bool) bool {
	if s.cfg.Server.AuthMode == "none" {
		return true
	}
	candidates := clientKeys(r.Header)
	if allowQueryKey {
		for _, k := range r.URL.Query()["key"] {
			if strings.TrimSpace(k) != "" {
				candidates = append(candidates, strings.TrimSpace(k))
			}
		}
	}
	for _, c := range candidates {
		for _, k := range s.cfg.Server.APIKeys {
			if subtle.ConstantTimeCompare([]byte(c), []byte(k)) == 1 && c != "" {
				return true
			}
		}
	}
	return false
}

// preamble holds the shared ingress setup: absolute deadline, auth,
// generation slot (unless counting), bounded body and resolved route. The
// snapshot is pinned once here so route, provider and capability facts stay
// on the same state; handlers must use p.snap and never repin.
type preamble struct {
	ctx      context.Context
	cancel   context.CancelFunc
	body     []byte
	model    *config.Model
	provider *config.Provider
	snap     *router.Snapshot
}

// startPreamble enforces method, auth and admission, reads the bounded body
// and resolves the public model. On failure it writes the ingress-native
// error, records status and returns nil. The caller owns p.cancel and the
// generation slot release (deferred inside on success).
func (s *Server) startPreamble(w http.ResponseWriter, r *http.Request, ingress string, needGeneration bool) (*preamble, int) {
	start := time.Now()
	deadline := start.Add(time.Duration(s.cfg.Retry.Deadline * float64(time.Second)))
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	fail := func(status int, rerr protocol.RouterError) (*preamble, int) {
		cancel()
		s.writeIngressError(w, ingress, rerr)
		return nil, status
	}
	if r.Method != http.MethodPost {
		return fail(405, protocol.RouterError{Status: 405, Code: protocol.CodeInvalidRequest, Msg: "Method not allowed"})
	}
	if !s.authedQuery(r, ingress == config.DialectGemini) {
		return fail(401, protocol.RouterError{Status: 401, Code: protocol.CodeUnauthorized, Msg: "Invalid API key"})
	}
	releaseGen := func() {}
	if needGeneration {
		select {
		case s.gens <- struct{}{}:
			releaseGen = func() { <-s.gens }
		case <-ctx.Done():
			return fail(504, protocol.RouterError{Status: 504, Code: "request_deadline_exceeded",
				Msg: "Overall upstream request deadline exceeded; generation may have been accepted and billed."})
		default:
			return fail(503, protocol.RouterError{Status: 503, Code: protocol.CodeOverloaded,
				Msg: "Server is overloaded, try again later"})
		}
	}
	body, rerr := s.readBody(w, r.WithContext(ctx))
	if rerr != nil {
		releaseGen()
		return fail(rerr.Status, *rerr)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		releaseGen()
		return fail(400, protocol.RouterError{Status: 400, Code: protocol.CodeInvalidRequest,
			Msg: "'body' must be a JSON object"})
	}
	var modelName string
	_ = json.Unmarshal(probe["model"], &modelName)
	snap := s.reg.Current()
	model, provider, merr := resolveSnapshotModel(snap, modelName)
	if merr != nil {
		releaseGen()
		// Resolve errors use the ingress shape; model_not_found maps to the
		// ingress 404 vocabulary below. The catalog-unmapped fault keeps
		// its code and operator hint on every ingress, including Gemini
		// (the google.rpc envelope carries the code in details).
		if ingress == config.DialectGemini && merr.Code == "model_not_found" {
			merr = &protocol.RouterError{Status: 404, Code: "model_not_found",
				Msg: fmt.Sprintf("models/%s is not found for API version v1beta. Call ListModels to see the list of available models.", modelName)}
		}
		return fail(merr.Status, *merr)
	}
	p := &preamble{ctx: ctx, body: body, model: model, provider: provider, snap: snap}
	// Wrap cancel so the generation slot is always released.
	origCancel := cancel
	p.cancel = func() { releaseGen(); origCancel() }
	return p, 0
}

// dispatchNativeOrTranslated is the modest seam for bridged pairs. For every
// ingress it returns the route dialect disposition without performing I/O:
// "native" (same protocol), "chat_to_anthropic" (Chat ingress -> Anthropic
// upstream), "anthropic_to_chat" (Anthropic ingress -> Chat upstream),
// "chat_to_responses" (Chat ingress -> Responses upstream),
// "responses_to_chat" (Responses ingress -> Chat upstream),
// "anthropic_to_responses" (Anthropic ingress -> Responses upstream),
// "responses_to_anthropic" (Responses ingress -> Anthropic upstream),
// "chat_to_gemini" (Chat ingress -> Gemini upstream),
// "gemini_to_chat" (Gemini ingress -> Chat upstream),
// "responses_to_gemini" (Responses ingress -> Gemini upstream),
// "gemini_to_responses" (Gemini ingress -> Responses upstream),
// "anthropic_to_gemini" (Anthropic ingress -> Gemini upstream),
// "gemini_to_anthropic" (Gemini ingress -> Anthropic upstream), or a
// *RouterError for an explicit pre-I/O refusal in the ingress shape.
func dispatchNativeOrTranslated(ingress string, routeDialect string) (string, *protocol.RouterError) {
	if routeDialect == ingress {
		return "native", nil
	}
	if ingress == config.DialectOpenAIChat && routeDialect == config.DialectAnthropic {
		return "chat_to_anthropic", nil
	}
	if ingress == config.DialectAnthropic && routeDialect == config.DialectOpenAIChat {
		return "anthropic_to_chat", nil
	}
	if ingress == config.DialectOpenAIChat && routeDialect == config.DialectOpenAIResponses {
		return "chat_to_responses", nil
	}
	if ingress == config.DialectOpenAIResponses && routeDialect == config.DialectOpenAIChat {
		return "responses_to_chat", nil
	}
	if ingress == config.DialectAnthropic && routeDialect == config.DialectOpenAIResponses {
		return "anthropic_to_responses", nil
	}
	if ingress == config.DialectOpenAIResponses && routeDialect == config.DialectAnthropic {
		return "responses_to_anthropic", nil
	}
	if ingress == config.DialectOpenAIChat && routeDialect == config.DialectGemini {
		return "chat_to_gemini", nil
	}
	if ingress == config.DialectGemini && routeDialect == config.DialectOpenAIChat {
		return "gemini_to_chat", nil
	}
	if ingress == config.DialectOpenAIResponses && routeDialect == config.DialectGemini {
		return "responses_to_gemini", nil
	}
	if ingress == config.DialectGemini && routeDialect == config.DialectOpenAIResponses {
		return "gemini_to_responses", nil
	}
	if ingress == config.DialectAnthropic && routeDialect == config.DialectGemini {
		return "anthropic_to_gemini", nil
	}
	if ingress == config.DialectGemini && routeDialect == config.DialectAnthropic {
		return "gemini_to_anthropic", nil
	}
	return "", &protocol.RouterError{Status: 400, Code: "unsupported_route",
		Msg: fmt.Sprintf("Upstream dialect %q is not implemented for this ingress; use a same-protocol route", routeDialect)}
}

// rewriteModelAlias rewrites only the top-level "model" member when the
// public name differs from the upstream ID, preserving every other byte via
// bounded RawMessage (no float64 round-trip for large integers).
func rewriteModelAlias(body []byte, upstream string) ([]byte, *protocol.RouterError) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, &protocol.RouterError{Status: 400, Code: protocol.CodeInvalidRequest,
			Msg: "'body' must be a JSON object"}
	}
	var modelName string
	_ = json.Unmarshal(probe["model"], &modelName)
	if modelName == "" {
		return nil, &protocol.RouterError{Status: 400, Code: protocol.CodeInvalidRequest,
			Msg: "A model ID is required"}
	}
	if modelName == upstream {
		return body, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, &protocol.RouterError{Status: 400, Code: protocol.CodeInvalidRequest,
			Msg: "'body' must be a JSON object"}
	}
	nameBytes, err := json.Marshal(upstream)
	if err != nil {
		return nil, &protocol.RouterError{Status: 500, Code: "encoding_error",
			Msg: "failed to encode upstream request"}
	}
	obj["model"] = nameBytes
	rewritten, err := json.Marshal(obj)
	if err != nil {
		return nil, &protocol.RouterError{Status: 500, Code: "encoding_error",
			Msg: "failed to encode upstream request"}
	}
	return rewritten, nil
}

// serveNativeForward dispatches one prepared native body under the absolute
// deadline and copies the upstream result. wantStream selects streaming;
// streamAsSSE forces text/event-stream (Chat/Responses/Anthropic SSE),
// while Gemini preserves the upstream content type.
func (s *Server) serveNativeForward(w http.ResponseWriter, r *http.Request, p *config.Provider, upstreamBody []byte, wantStream, streamAsSSE bool, url string, ingressLabel, dest string, sess router.Session) int {
	headers := transport.OutboundHeaders(r.Header, p, ingressLabel, dest, sess.ID, sess.Has)
	res, rerr := s.up.Do(pCtx(r), p, s.cfg.Retry, url, headers, upstreamBody, wantStream, s.cfg.Server.MaxBodyBytes)
	if rerr != nil {
		if res != nil && res.Failed {
			return s.copyNativeBody(w, res)
		}
		s.writeIngressError(w, ingressLabel, *rerr)
		return rerr.Status
	}
	if wantStream {
		defer res.Stream.Close()
		if streamAsSSE {
			return s.copyNativeStream(w, r, res)
		}
		return s.copyNativeStreamPreserved(w, r, res)
	}
	return s.copyNativeBody(w, res)
}

func pCtx(r *http.Request) context.Context { return r.Context() }

// writeIngressError renders RouterError in the ingress dialect envelope.
func (s *Server) writeIngressError(w http.ResponseWriter, ingress string, rerr protocol.RouterError) {
	switch ingress {
	case config.DialectAnthropic:
		typ := anthropicErrorTypes[rerr.Status]
		if typ == "" {
			typ = "api_error"
			if rerr.Status < 500 {
				typ = "invalid_request_error"
			}
		}
		if rerr.Code == "count_tokens_unsupported" {
			typ = "invalid_request_error"
		}
		s.writeAnthropicError(w, rerr.Status, typ, rerr.Msg)
	case config.DialectGemini:
		s.writeGeminiError(w, rerr)
	default:
		// Chat and Responses share the OpenAI error envelope.
		s.writeChatError(w, rerr)
	}
}

func (s *Server) writeResponsesError(w http.ResponseWriter, rerr protocol.RouterError) {
	s.writeIngressError(w, config.DialectOpenAIResponses, rerr)
}

// writeGeminiError renders a google.rpc.Status payload: code is the HTTP
// status, status the canonical name, details carries the router code.
func (s *Server) writeGeminiError(w http.ResponseWriter, rerr protocol.RouterError) {
	body := geminiErrorPayload(rerr)
	rc := http.NewResponseController(w)
	timeout := time.Duration(s.cfg.Server.WriteTimeout * float64(time.Second))
	if timeout > 0 {
		rc.SetWriteDeadline(time.Now().Add(timeout))
		defer rc.SetWriteDeadline(time.Time{})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(rerr.Status)
	_, _ = w.Write(body)
}

// geminiErrorPayload builds the google.rpc.Status body for a router error so
// the pre-commit HTTP error and the post-commit translated-SSE error frame
// share one sanitized shape. The message is value-free: raw upstream bodies
// are never copied into translated errors.
func geminiErrorPayload(rerr protocol.RouterError) []byte {
	name := geminiErrorStatuses[rerr.Status]
	if name == "" {
		if rerr.Status >= 500 {
			name = "INTERNAL"
		} else {
			name = "INVALID_ARGUMENT"
		}
	}
	bodyMap := map[string]any{
		"error": map[string]any{
			"code":    rerr.Status,
			"message": sanitize(rerr.Msg),
			"status":  name,
		},
	}
	if rerr.Code != "" {
		reason := ""
		for _, ch := range strings.ToUpper(rerr.Code) {
			if (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_' {
				reason += string(ch)
			} else {
				reason += "_"
			}
		}
		bodyMap["error"].(map[string]any)["details"] = []any{
			map[string]any{"@type": "type.googleapis.com/google.rpc.ErrorInfo",
				"reason": reason, "domain": "router"},
		}
	}
	body, _ := json.Marshal(bodyMap)
	return body
}

// copyNativeStreamPreserved forwards a native stream without forcing
// text/event-stream: the upstream content type is preserved (Gemini uses
// SSE with alt=sse and a JSON array otherwise). Framing and auth headers
// are still filtered; bytes are otherwise opaque.
func (s *Server) copyNativeStreamPreserved(w http.ResponseWriter, r *http.Request, res *transport.Result) int {
	defer res.Stream.Close()
	for name, value := range filteredResponseHeaders(res.Header) {
		w.Header().Set(name, value)
	}
	ct := res.Header.Get("Content-Type")
	if w.Header().Get("Content-Type") == "" {
		if ct != "" {
			w.Header().Set("Content-Type", ct)
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
	}
	w.Header().Set("Cache-Control", "no-cache")
	timeout := time.Duration(s.cfg.Server.WriteTimeout * float64(time.Second))
	rc := http.NewResponseController(w)
	if timeout > 0 {
		rc.SetWriteDeadline(time.Now().Add(timeout))
		defer rc.SetWriteDeadline(time.Time{})
	}
	w.WriteHeader(res.Status)
	dw := &deadlineWriter{w: w, rc: rc, timeout: timeout}
	buf := make([]byte, 32*1024)
	for {
		n, err := res.Stream.Read(buf)
		if n > 0 {
			if _, werr := dw.Write(buf[:n]); werr != nil {
				return 200
			}
			if ferr := rc.Flush(); ferr != nil {
				return 200
			}
			if timeout > 0 {
				rc.SetWriteDeadline(time.Now().Add(timeout))
			}
		}
		if err != nil {
			if ferr := rc.Flush(); ferr != nil {
				return 200
			}
			return 200
		}
		select {
		case <-r.Context().Done():
			return 200
		default:
		}
	}
}
