// Package server owns the HTTP boundary: client auth, admission, body
// limits, ingress routing, downstream commitment and cleanup.
package server

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/arhammahajan/router/internal/accounting"
	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/protocol"
	"github.com/arhammahajan/router/internal/provider"
	"github.com/arhammahajan/router/internal/router"
	"github.com/arhammahajan/router/internal/transport"
)

// Server is the configured HTTP router.
type Server struct {
	cfg   *config.Config
	reg   *router.Registry
	up    *transport.Client
	mux   *http.ServeMux
	conns chan struct{}
	gens  chan struct{}
	// acc is the opt-in usage recorder, built only when
	// [accounting] is configured; nil otherwise (every call site is
	// nil-safe and the hot path is untouched).
	acc *accounting.Recorder
}

// New builds a Server from validated config. It performs no I/O: it
// publishes the static snapshot and leaves catalog refresh to the
// composition root (cmd/routerd), which owns startup inventory.
func New(cfg *config.Config) *Server {
	return NewWithRegistry(cfg, router.NewRegistry(cfg))
}

// NewWithRegistry builds a Server over an existing Registry. The registry
// is shared, so catalog publications after construction are visible to
// subsequent requests; each request still pins one snapshot.
func NewWithRegistry(cfg *config.Config, reg *router.Registry) *Server {
	s := &Server{
		cfg:   cfg,
		reg:   reg,
		up:    transport.NewClient(),
		mux:   http.NewServeMux(),
		conns: make(chan struct{}, cfg.Server.MaxConnections),
		gens:  make(chan struct{}, cfg.Server.MaxGenerations),
	}
	// Usage metering is created only when the [accounting] section is
	// present; a nil recorder leaves every request path untouched.
	if cfg.Accounting != nil {
		s.acc = accounting.New(cfg.Accounting.Path, cfg.Accounting.MaxFileBytes)
	}
	s.mux.HandleFunc("/v1/chat/completions", s.handleChat)
	s.mux.HandleFunc("/chat/completions", s.handleChat)
	s.mux.HandleFunc("/v1/models", s.handleModels)
	s.mux.HandleFunc("/models", s.handleModelsAlias)
	s.mux.HandleFunc("/muse-code/models", s.handleMuseModels)
	s.mux.HandleFunc("/v1/muse-code/models", s.handleMuseModels)
	s.mux.HandleFunc("/v1beta/models", s.handleGeminiModelsList)
	// GET-only exact trailing-slash aliases leave Gemini generation prefixes intact.
	s.mux.HandleFunc("GET /v1/models/{$}", s.handleModels)
	s.mux.HandleFunc("GET /models/{$}", s.handleModelsAlias)
	s.mux.HandleFunc("GET /muse-code/models/{$}", s.handleMuseModels)
	s.mux.HandleFunc("GET /v1/muse-code/models/{$}", s.handleMuseModels)
	s.mux.HandleFunc("/livez", s.handleLive)
	s.mux.HandleFunc("/health", s.handleLive)
	s.mux.HandleFunc("/healthz", s.handleLive)
	s.mux.HandleFunc("/readyz", s.handleReady)
	s.mux.HandleFunc("/diagnostics", s.handleDiagnostics)
	s.mux.HandleFunc("/", s.handleNotFound)
	s.registerNativeRoutes()
	return s
}

// CloseIdleConnections closes idle upstream keep-alive connections.
func (s *Server) CloseIdleConnections() {
	s.up.CloseIdleConnections()
}

// Registry exposes the shared route registry for the composition root
// (catalog startup refresh and periodic workers). Request handlers pin
// via Current; they never mutate through this handle.
func (s *Server) Registry() *router.Registry {
	return s.reg
}

// Handler returns the instrumented mux with handler-level admission.
//
// max_connections bounds concurrent handlers after headers are read. The raw
// listener (LimitListener) separately bounds accepted TCP sockets including
// idle keep-alive and partial-header connections (HTTP/1 scope); its overload
// reply is intentionally an empty 503 with Retry-After before the endpoint is
// known, so it carries no ingress envelope. This handler bound selects the
// ingress envelope from the request path without parsing untrusted bodies:
// Anthropic paths get the Anthropic shape, Gemini paths the google.rpc
// shape, everything else (Chat, Responses, /v1/models list, liveness) the
// shared OpenAI shape. The slot is never held on rejection.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case s.conns <- struct{}{}:
			defer func() { <-s.conns }()
		default:
			s.writeIngressError(w, overloadIngress(r.URL.Path), protocol.RouterError{Status: 503, Code: protocol.CodeOverloaded,
				Msg: "Server is overloaded, try again later"})
			return
		}
		s.mux.ServeHTTP(w, r)
	})
}

// overloadIngress selects the error envelope for handler-level 503s by
// endpoint prefix only. /v1/models (exact) is the OpenAI list endpoint and
// stays Chat-shaped; /v1/models/ (prefix) and every /v1beta/ path are Gemini.
func overloadIngress(path string) string {
	if path == "/v1/messages" || path == "/messages" ||
		strings.HasPrefix(path, "/v1/messages/") || strings.HasPrefix(path, "/messages/") {
		return config.DialectAnthropic
	}
	if strings.HasPrefix(path, "/v1beta/") ||
		strings.HasPrefix(path, "/v1/models/") || strings.HasPrefix(path, "/v1/tunedModels/") ||
		strings.HasPrefix(path, "/models/") || strings.HasPrefix(path, "/tunedModels/") ||
		path == "/v1beta/models" || path == "/v1beta/tunedModels" ||
		path == "/v1/tunedModels" || path == "/tunedModels" {
		return config.DialectGemini
	}
	return config.DialectOpenAIChat
}

// HTTPServer builds the net/http server with header/idle bounds. Body and
// write deadlines are enforced per request, not via server-wide timeouts
// that would kill long SSE streams.
func (s *Server) HTTPServer(addr string) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: time.Duration(s.cfg.Server.HeaderReadTimeout * float64(time.Second)),
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
}

// --- helpers ---

func (s *Server) authed(r *http.Request) bool {
	return s.authedQuery(r, false)
}

func clientKeys(h http.Header) []string {
	var keys []string
	if v := h.Get("Authorization"); len(v) >= 7 && strings.EqualFold(v[:7], "bearer ") {
		if key := strings.TrimSpace(v[7:]); key != "" {
			keys = append(keys, key)
		}
	}
	for _, name := range []string{"X-Api-Key", "Api-Key", "X-Goog-Api-Key"} {
		if key := strings.TrimSpace(h.Get(name)); key != "" {
			keys = append(keys, key)
		}
	}
	return keys
}

func (s *Server) writeChatError(w http.ResponseWriter, rerr protocol.RouterError) {
	writeChatErrorWithTimeout(w, rerr, time.Duration(s.cfg.Server.WriteTimeout*float64(time.Second)))
}

func writeChatErrorWithTimeout(w http.ResponseWriter, rerr protocol.RouterError, timeout time.Duration) {
	typ := "invalid_request_error"
	if rerr.Status >= 500 {
		typ = "api_error"
	}
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": sanitize(rerr.Msg),
			"type":    typ,
			"param":   nil,
			"code":    rerr.Code,
		},
	})
	rc := http.NewResponseController(w)
	if timeout > 0 {
		rc.SetWriteDeadline(time.Now().Add(timeout))
		defer rc.SetWriteDeadline(time.Time{})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(rerr.Status)
	_, _ = w.Write(body)
}

// upstreamDetail renders a non-retried translated upstream error status.
// The message is a stable sanitized status only: raw upstream bodies can
// echo secrets or prompt text, so they are never copied into translated
// errors. Native errors preserve status+body via body forwarding instead.
func upstreamDetail(status int, _ []byte) *protocol.RouterError {
	code := "api_error"
	if status < 500 {
		code = "invalid_request_error"
	}
	return &protocol.RouterError{Status: status, Code: code,
		Msg: fmt.Sprintf("Upstream returned HTTP %d", status)}
}

func sanitize(s string) string {
	if len(s) > 1024 {
		s = s[:1024]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, s)
}

// readBody reads the request body bounded before and after decompression.
// The body_read_timeout applies to the whole read; the connection deadline
// is reset afterwards so keep-alive reuse is unaffected.
func (s *Server) readBody(w http.ResponseWriter, r *http.Request) ([]byte, *protocol.RouterError) {
	max := s.cfg.Server.MaxBodyBytes
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(time.Now().Add(
		time.Duration(s.cfg.Server.BodyReadTimeout * float64(time.Second))))
	defer rc.SetReadDeadline(time.Time{})
	r.Body = http.MaxBytesReader(w, r.Body, max+1)
	var reader io.Reader = r.Body
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))) {
	case "", "identity":
	case "gzip":
		gr, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, &protocol.RouterError{Status: 400, Code: protocol.CodeInvalidRequest,
				Msg: "Request body is not valid gzip"}
		}
		defer gr.Close()
		reader = gr
	default:
		return nil, &protocol.RouterError{Status: 400, Code: protocol.CodeInvalidRequest,
			Msg: "Unsupported Content-Encoding; supported: identity, gzip"}
	}
	data, err := io.ReadAll(io.LimitReader(reader, max+1))
	if err != nil {
		if os.IsTimeout(err) {
			return nil, &protocol.RouterError{Status: 408, Code: protocol.CodeInvalidRequest,
				Msg: "Request body read exceeded the deadline"}
		}
		if strings.Contains(err.Error(), "request body too large") {
			return nil, &protocol.RouterError{Status: 413, Code: protocol.CodeInvalidRequest,
				Msg: "Request body exceeds the size bound"}
		}
		return nil, &protocol.RouterError{Status: 400, Code: protocol.CodeInvalidRequest,
			Msg: "Request body could not be read"}
	}
	if int64(len(data)) > max {
		return nil, &protocol.RouterError{Status: 413, Code: protocol.CodeInvalidRequest,
			Msg: "Request body exceeds the size bound"}
	}
	return data, nil
}

// resolveModel pins the current registry snapshot and maps a public name
// to its route. Handlers that also need capability facts keep their pin and
// call resolveSnapshotModel so resolution and planning use the same state.
func (s *Server) resolveModel(name string) (*config.Model, *config.Provider, *protocol.RouterError) {
	return resolveSnapshotModel(s.reg.Current(), name)
}

func resolveSnapshotModel(snap *router.Snapshot, name string) (*config.Model, *config.Provider, *protocol.RouterError) {
	m, p, fault := snap.Resolve(name)
	switch fault {
	case router.Found:
		return m, p, nil
	case router.UnmappedCatalog:
		return nil, nil, &protocol.RouterError{Status: 404, Code: "model_endpoint_unknown",
			Msg: fmt.Sprintf("Model %q is listed by the catalog but its endpoint compatibility "+
				"is unknown. Configure catalog.model_providers or an explicit [[models]] route "+
				"after reviewing the provider endpoint.", name)}
	default: // router.DisabledReserved | router.NotFound
		return nil, nil, &protocol.RouterError{Status: 404, Code: "model_not_found",
			Msg: fmt.Sprintf("Model %q is not served by this router", name)}
	}
}

// --- endpoints ---

func (s *Server) writeJSON(w http.ResponseWriter, status int, body []byte) {
	rc := http.NewResponseController(w)
	timeout := time.Duration(s.cfg.Server.WriteTimeout * float64(time.Second))
	if timeout > 0 {
		rc.SetWriteDeadline(time.Now().Add(timeout))
		defer rc.SetWriteDeadline(time.Time{})
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, 200, []byte(`{"status":"ok"}`))
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	snap := s.reg.Current()
	status, _ := snap.Readiness()["status"].(string)
	code := 200
	if status != "ready" {
		code = 503
	}
	body, _ := json.Marshal(map[string]any{"status": status})
	s.writeJSON(w, code, body)
}

// handleDiagnostics serves the authenticated diagnostics report. Auth
// order mirrors Python: no configured router keys closes the endpoint
// with 403 diagnostics_disabled before any 401; otherwise headers-only
// router auth applies and ?key is never accepted here.
func (s *Server) handleDiagnostics(w http.ResponseWriter, r *http.Request) {
	if len(s.cfg.Server.APIKeys) == 0 {
		s.writeChatError(w, protocol.RouterError{Status: 403, Code: "diagnostics_disabled",
			Msg: "Diagnostics require configured router authentication"})
		return
	}
	if !s.authed(r) {
		s.writeChatError(w, protocol.RouterError{Status: 401, Code: protocol.CodeUnauthorized, Msg: "Invalid API key"})
		return
	}
	s.writeJSON(w, 200, s.DiagnosticsPayload())
}

// DiagnosticsPayload pins one immutable snapshot plus one clock sample
// and derives every age and staleness from that instant. No locks,
// network, filesystem or refreshes happen on this path.
func (s *Server) DiagnosticsPayload() []byte {
	snap := s.reg.Current()
	now := s.reg.Clock().Sample().Mono
	sanitize := s.reg.Sanitize
	catEnabled := s.cfg.Catalog != nil
	llamaEnabled := s.cfg.Llama != nil
	var catInterval float64
	var catURL string
	if catEnabled {
		catInterval = s.cfg.Catalog.RefreshSeconds
		catURL = s.cfg.Catalog.URL
	}
	var llamaInterval float64
	if llamaEnabled {
		llamaInterval = s.cfg.Llama.RefreshSeconds
	}
	per := snap.ProviderEnabledRoutes()
	dialects := snap.ProviderDialects()
	details := make([]any, 0, len(dialects))
	for _, name := range snap.ProviderNames() {
		reach := "unprobed"
		switch {
		case snap.IsUnreachable(name):
			reach = "unavailable"
		case snap.HasLlamaProvider(name):
			reach = "available"
		}
		details = append(details, map[string]any{
			"name":           name,
			"dialect":        dialects[name],
			"reachability":   reach,
			"enabled_routes": per[name],
		})
	}
	ev := snap.CacheEvent()
	cache := map[string]any{
		"enabled":     ev.Enabled,
		"load":        ev.Load,
		"write_error": writeErrOrNil(ev.WriteErr),
	}
	if ev.Expired != nil {
		cache["expired_sources"] = *ev.Expired
	}
	body := map[string]any{
		"status":           "ok",
		"providers":        snap.ProviderCount(),
		"models":           len(snap.Models()),
		"readiness":        snap.Readiness(),
		"provider_details": details,
		"catalog":          snap.CatalogStatus(catEnabled, catURL, sanitize, catInterval, now),
		"llama_cpp":        snap.LlamaStatus(llamaEnabled, s.reg.LlamaOrder(), llamaInterval, now, sanitize),
		"discovery_cache":  cache,
	}
	sanitized, _ := s.reg.SanitizeReport(body).(map[string]any)
	out, _ := json.Marshal(sanitized)
	return out
}

func writeErrOrNil(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// ReadinessReportJSON renders the CLI check payload: readiness merged
// with the configured-source discovery summary.
func (s *Server) ReadinessReportJSON() []byte {
	out, _ := json.Marshal(s.ReadinessReport())
	return out
}

// ReadinessReport merges readiness with the configured-source discovery
// summary for CLI --check-readiness output.
func (s *Server) ReadinessReport() map[string]any {
	snap := s.reg.Current()
	now := s.reg.Clock().Sample().Mono
	out := snap.Readiness()
	out["discovery"] = snap.DiscoverySummary(
		s.cfg.Catalog != nil, s.cfg.Llama != nil,
		catalogInterval(s.cfg), llamaInterval(s.cfg), now)
	return out
}

func catalogInterval(cfg *config.Config) float64 {
	if cfg.Catalog != nil {
		return cfg.Catalog.RefreshSeconds
	}
	return 0
}

func llamaInterval(cfg *config.Config) float64 {
	if cfg.Llama != nil {
		return cfg.Llama.RefreshSeconds
	}
	return 0
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeChatError(w, protocol.RouterError{Status: 405, Code: protocol.CodeInvalidRequest, Msg: "Method not allowed"})
		return
	}
	if !s.authed(r) {
		s.writeChatError(w, protocol.RouterError{Status: 401, Code: protocol.CodeUnauthorized, Msg: "Invalid API key"})
		return
	}
	// One pinned snapshot + one clock sample per report via the shared
	// listing builder: no network, filesystem, store lock or worker.
	rows, _ := s.buildListing()
	s.writeOpenAIModels(w, rows)
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		if r.Method != http.MethodGet {
			s.writeChatError(w, protocol.RouterError{Status: 404, Code: "not_found",
				Msg: "Not implemented in this build (milestone 2: Chat ingress only)"})
			return
		}
		s.writeIndex(w)
		return
	}
	if s.nativeNotFound(w, r) {
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/messages") || r.URL.Path == "/messages" {
		s.writeAnthropicError(w, 404, "not_found", "Not implemented in this build")
		return
	}
	s.writeChatError(w, protocol.RouterError{Status: 404, Code: "not_found",
		Msg: "Not implemented in this build (milestone 2: Chat ingress only)"})
}

func (s *Server) writeAnthropicError(w http.ResponseWriter, status int, typ, msg string) {
	body, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": typ, "message": msg},
	})
	rc := http.NewResponseController(w)
	timeout := time.Duration(s.cfg.Server.WriteTimeout * float64(time.Second))
	if timeout > 0 {
		rc.SetWriteDeadline(time.Now().Add(timeout))
		defer rc.SetWriteDeadline(time.Time{})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// handleChat serves POST /v1/chat/completions (and the unversioned alias).
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	status := 0
	defer func() {
		log.Printf("route=chat status=%d duration_ms=%d", status, time.Since(start).Milliseconds())
	}()
	// Absolute upstream deadline starts here so preparation (read/decode/
	// encode) counts against the same budget as attempts, backoff and stream
	// consumption, per the design.
	deadline := start.Add(time.Duration(s.cfg.Retry.Deadline * float64(time.Second)))
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()
	if r.Method != http.MethodPost {
		status = 405
		s.writeChatError(w, protocol.RouterError{Status: 405, Code: protocol.CodeInvalidRequest, Msg: "Method not allowed"})
		return
	}
	if !s.authed(r) {
		status = 401
		s.writeChatError(w, protocol.RouterError{Status: 401, Code: protocol.CodeUnauthorized, Msg: "Invalid API key"})
		return
	}
	// Generation slot: no waiting queue; overload is an immediate 503.
	// The slot is held for the whole request and released on every exit,
	// including cancellation and deadline expiry.
	select {
	case s.gens <- struct{}{}:
		defer func() { <-s.gens }()
	case <-ctx.Done():
		status = 504
		s.writeChatError(w, protocol.RouterError{Status: 504, Code: "request_deadline_exceeded",
			Msg: "Overall upstream request deadline exceeded; generation may have been accepted and billed."})
		return
	default:
		status = 503
		s.writeChatError(w, protocol.RouterError{Status: 503, Code: protocol.CodeOverloaded,
			Msg: "Server is overloaded, try again later"})
		return
	}
	body, rerr := s.readBody(w, r)
	if rerr != nil {
		status = rerr.Status
		s.writeChatError(w, *rerr)
		return
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		status = 400
		s.writeChatError(w, protocol.RouterError{Status: 400, Code: protocol.CodeInvalidRequest,
			Msg: "'body' must be a JSON object"})
		return
	}
	var modelName string
	_ = json.Unmarshal(probe["model"], &modelName)
	// One pinned snapshot supplies route, provider and canonical
	// capability facts for the whole request; never repin afterwards.
	snap := s.reg.Current()
	model, provider, resolveErr := resolveSnapshotModel(snap, modelName)
	if resolveErr != nil {
		status = resolveErr.Status
		s.writeChatError(w, *resolveErr)
		return
	}
	dest := provider.ResolveDialect(config.DialectOpenAIChat, model.Dialect)
	translated := dest != config.DialectOpenAIChat
	// Usage accounting hook: wraps the downstream writer only when the
	// [accounting] section is present, so the disabled path is untouched.
	// The deferred finish is the single response-completion seam shared
	// by the native and translated dispatches below — they only return
	// after the response is finished (streams after the terminal chunk) —
	// and it drops every error: metering can never fail a request.
	if s.acc != nil {
		var wantStream bool
		_ = json.Unmarshal(probe["stream"], &wantStream)
		var finishAccounting func(int)
		w, finishAccounting = s.beginAccounting(w, config.DialectOpenAIChat, dest, model.Name, provider.Name, wantStream, start)
		defer func() { finishAccounting(status) }()
	}
	// Opt-in compat pre-step (Chat generation POST only, after the body
	// and model checks above): strip a marked token, inject the native
	// candidate and merge leading plain systems. Unmarked inputs pass
	// through untouched.
	compatHdr := r.Header
	if s.cfg.Server.BearerSessionCompat {
		newBody, newHdr, cerr := router.PrepareChatCompat(r.Header, body)
		if cerr != nil {
			status = cerr.Status
			s.writeChatError(w, *cerr)
			return
		}
		body = newBody
		compatHdr = newHdr
	}
	sess, effBody, serr := prepareSession(provider, compatHdr, body, translated, router.NoCompatFallback())
	if serr != nil {
		status = serr.Status
		s.writeChatError(w, *serr)
		return
	}
	if dest == config.DialectOpenAIChat {
		r2 := r.WithContext(ctx)
		r2.Header = compatHdr
		status = s.serveNative(w, r2, provider, model, effBody, sess, snap)
		return
	}
	if dest == config.DialectAnthropic {
		r2 := r.WithContext(ctx)
		r2.Header = compatHdr
		status = s.serveTranslated(w, r2, provider, model, effBody, sess)
		return
	}
	if dest == config.DialectOpenAIResponses {
		r2 := r.WithContext(ctx)
		r2.Header = compatHdr
		status = s.serveChatToResponses(w, r2, provider, model, effBody, sess, snap)
		return
	}
	if dest == config.DialectGemini {
		r2 := r.WithContext(ctx)
		r2.Header = compatHdr
		status = s.serveChatToGemini(w, r2, provider, model, effBody, sess, snap)
		return
	}
	status = 400
	s.writeChatError(w, protocol.RouterError{Status: 400, Code: "unsupported_route",
		Msg: fmt.Sprintf("Upstream dialect %q is not implemented in this build; use a same-protocol route", dest)})
}

// serveNative forwards a Chat request to an openai-chat upstream, preserving
// unknown native fields. The body is forwarded byte-identical unless the
// public model alias differs from the upstream ID (documented rewrite) or
// the pinned snapshot confirms a LlamaCpp runtime, which consumes the raw
// reasoning_effort member into native thinking controls. Strata,
// Unobserved and ordinary providers pass through raw and untouched.
func (s *Server) serveNative(w http.ResponseWriter, r *http.Request, p *config.Provider,
	model *config.Model, body []byte, sess router.Session, snap *router.Snapshot) int {
	upstreamBody := body
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		s.writeChatError(w, protocol.RouterError{Status: 400, Code: protocol.CodeInvalidRequest,
			Msg: "'body' must be a JSON object"})
		return 400
	}
	var modelName string
	_ = json.Unmarshal(probe["model"], &modelName)
	if modelName == "" {
		s.writeChatError(w, protocol.RouterError{Status: 400, Code: protocol.CodeInvalidRequest,
			Msg: "A model ID is required"})
		return 400
	}
	if modelName != model.Upstream {
		// Preserve every other byte exactly: decode into raw messages so
		// large JSON integers (e.g. 2^53+1) never pass through float64.
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(body, &obj); err != nil {
			s.writeChatError(w, protocol.RouterError{Status: 400, Code: protocol.CodeInvalidRequest,
				Msg: "'body' must be a JSON object"})
			return 400
		}
		nameBytes, err := json.Marshal(model.Upstream)
		if err != nil {
			s.writeChatError(w, protocol.RouterError{Status: 500, Code: "encoding_error",
				Msg: "failed to encode upstream request"})
			return 500
		}
		obj["model"] = nameBytes
		rewritten, err := json.Marshal(obj)
		if err != nil {
			s.writeChatError(w, protocol.RouterError{Status: 500, Code: "encoding_error",
				Msg: "failed to encode upstream request"})
			return 500
		}
		upstreamBody = rewritten
	}
	// Confirmed LlamaCpp only: consume the native effort member into the
	// exact thinking controls. Never a validation, capability gate, loss
	// header or error; Strata/Unobserved/ordinary stay byte-identical.
	// The same pinned snapshot supplies runtime, route and provider.
	dest := p.ResolveDialect(config.DialectOpenAIChat, model.Dialect)
	upstreamBody, _ = provider.ForKind(p.Kind).PrepareNativeBody(dest, upstreamBody)
	var wantStream bool
	_ = json.Unmarshal(probe["stream"], &wantStream)
	url, rerr := transport.GenerationEndpoint(p, dest)
	if rerr != nil {
		s.writeChatError(w, *rerr)
		return rerr.Status
	}
	headers := transport.OutboundHeaders(r.Header, p, "chat", dest, sess.ID, sess.Has)
	// r.Context already carries the absolute handler deadline established in
	// handleChat, so preparation time counts against the same budget.
	res, rerr := s.up.Do(r.Context(), p, s.cfg.Retry, url, headers, upstreamBody, wantStream, s.cfg.Server.MaxBodyBytes)
	if rerr != nil {
		if res != nil && res.Failed {
			// Native upstream errors keep their status and body; only
			// framing/credential headers are filtered.
			return s.copyNativeBody(w, res)
		}
		s.writeChatError(w, *rerr)
		return rerr.Status
	}
	if wantStream {
		defer res.Stream.Close()
		return s.copyNativeStream(w, r, res)
	}
	return s.copyNativeBody(w, res)
}

func filteredResponseHeaders(h http.Header) map[string]string {
	drop := map[string]bool{
		"connection": true, "keep-alive": true, "proxy-authenticate": true,
		"proxy-authorization": true, "te": true, "trailer": true,
		"transfer-encoding": true, "upgrade": true,
		"content-length": true, "content-encoding": true, "set-cookie": true,
		"cookie": true, "authorization": true, "x-api-key": true,
		"api-key": true, "x-goog-api-key": true,
		"x-claude-code-session-id": true, "session-id": true, "session_id": true,
		"thread-id": true, "conversation_id": true, "x-session-id": true,
		"x-mavis-session-id": true, "x-opencode-session": true,
	}
	nominated := map[string]bool{}
	for name, values := range h {
		if !strings.EqualFold(name, "connection") {
			continue
		}
		for _, v := range values {
			for _, token := range strings.Split(v, ",") {
				token = strings.ToLower(strings.TrimSpace(token))
				if token != "" {
					nominated[token] = true
				}
			}
		}
	}
	out := map[string]string{}
	for name, values := range h {
		lower := strings.ToLower(name)
		if drop[lower] || nominated[lower] || len(values) == 0 {
			continue
		}
		out[name] = values[0]
	}
	return out
}

func (s *Server) copyNativeBody(w http.ResponseWriter, res *transport.Result) int {
	data := res.Body
	if int64(len(data)) > s.cfg.Server.MaxBodyBytes {
		s.writeChatError(w, protocol.RouterError{Status: 502, Code: protocol.CodeUpstreamBad,
			Msg: "Upstream response exceeds the size bound"})
		return 502
	}
	for name, value := range filteredResponseHeaders(res.Header) {
		w.Header().Set(name, value)
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	rc := http.NewResponseController(w)
	timeout := time.Duration(s.cfg.Server.WriteTimeout * float64(time.Second))
	if timeout > 0 {
		rc.SetWriteDeadline(time.Now().Add(timeout))
		defer rc.SetWriteDeadline(time.Time{})
	}
	w.WriteHeader(res.Status)
	_, _ = w.Write(data)
	// Reset for keep-alive reuse: a stale write deadline must not apply to
	// the next request on this connection.
	return res.Status
}

func (s *Server) copyNativeStream(w http.ResponseWriter, r *http.Request, res *transport.Result) int {
	defer res.Stream.Close()
	for name, value := range filteredResponseHeaders(res.Header) {
		w.Header().Set(name, value)
	}
	w.Header().Set("Content-Type", "text/event-stream")
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
				// Downstream gone: cancel upstream and stop without markers.
				return 200
			}
			if ferr := rc.Flush(); ferr != nil {
				// Downstream flush failed: stop feeding and release upstream.
				return 200
			}
			if timeout > 0 {
				rc.SetWriteDeadline(time.Now().Add(timeout))
			}
		}
		if err != nil {
			// EOF ends the stream as the upstream framed it; a truncation
			// simply stops without a synthetic success marker.
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

// serveTranslated decodes Chat, plans losses, encodes Anthropic, dispatches
// under the absolute deadline and translates the response.
func (s *Server) serveTranslated(w http.ResponseWriter, r *http.Request, p *config.Provider,
	model *config.Model, body []byte, sess router.Session) int {
	lim := protocol.DefaultLimits()
	gen, losses, rerr := protocol.DecodeChatRequest(body, lim)
	if rerr != nil {
		s.writeChatError(w, *rerr)
		return rerr.Status
	}
	kept, rerr := protocol.EnforcePolicy(losses, p.IsLossy())
	if rerr != nil {
		s.writeChatError(w, *rerr)
		return rerr.Status
	}
	upstreamBody, rerr := protocol.EncodeAnthropicRequest(gen, model.Upstream)
	if rerr != nil {
		s.writeChatError(w, *rerr)
		return rerr.Status
	}
	dest := p.ResolveDialect(config.DialectOpenAIChat, model.Dialect)
	url, rerr := transport.GenerationEndpoint(p, dest)
	if rerr != nil {
		s.writeChatError(w, *rerr)
		return rerr.Status
	}
	headers := transport.OutboundHeaders(r.Header, p, "chat", dest, sess.ID, sess.Has)
	// r.Context already carries the absolute handler deadline established in
	// handleChat, so preparation time counts against the same budget.
	res, rerr := s.up.Do(r.Context(), p, s.cfg.Retry, url, headers, upstreamBody, gen.Stream, s.cfg.Server.MaxBodyBytes)
	if rerr != nil {
		if res != nil && res.Failed {
			forwardRetryHints(w, res.Header)
			rerr = upstreamDetail(res.Status, res.Body)
		}
		s.writeChatError(w, *rerr)
		return rerr.Status
	}
	defer func() {
		if res.Stream != nil {
			res.Stream.Close()
		}
	}()
	lossHeader := protocol.EncodeLosses(kept)
	if !gen.Stream {
		translated, rerr := protocol.TranslateAnthropicJSONToChatWithPlanAndPolicy(res.Body, model.Name, lim, gen.ToolPlan, p.IsLossy())
		if rerr != nil {
			s.writeChatError(w, *rerr)
			return rerr.Status
		}
		forwardRetryHints(w, res.Header)
		w.Header().Set("Content-Type", "application/json")
		if lossHeader != "" {
			w.Header().Set("x-model-router-loss", lossHeader)
		}
		s.writeJSON(w, 200, translated)
		return 200
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	if lossHeader != "" {
		w.Header().Set("x-model-router-loss", lossHeader)
	}
	forwardRetryHints(w, res.Header)
	w.WriteHeader(200)
	timeout := time.Duration(s.cfg.Server.WriteTimeout * float64(time.Second))
	rc := http.NewResponseController(w)
	if timeout > 0 {
		rc.SetWriteDeadline(time.Now().Add(timeout))
		defer rc.SetWriteDeadline(time.Time{})
	}
	dw := &deadlineWriter{w: w, rc: rc, timeout: timeout}
	flushFailed := false
	tr := &protocol.SSETranslator{PublicModel: model.Name,
		IncludeUsage: gen.StreamIncludeUsage, Limits: lim, Plan: gen.ToolPlan,
		AllowGatewayBillingDrop: p.IsLossy()}
	if rerr := tr.Translate(res.Stream, dw, func() {
		if err := rc.Flush(); err != nil {
			// Sticky downstream failure: mark the writer failed so the
			// next chunk fails fast, and close the upstream stream now
			// so a blocked upstream Read unblocks instead of waiting
			// for the provider deadline. Synchronous: no goroutine.
			flushFailed = true
			dw.failed = true
			res.Stream.Close()
		} else if timeout > 0 {
			rc.SetWriteDeadline(time.Now().Add(timeout))
		}
	}); rerr != nil || flushFailed || dw.failed {
		// Headers are committed (or the downstream write failed):
		// terminate without a synthetic success marker. The client sees
		// a truncated stream, never a fake finish. Cancel upstream work.
		res.Stream.Close()
		return 200
	}
	return 200
}

// forwardRetryHints copies only retry/rate-limit hints to the downstream
// error/OK headers; upstream bodies and other headers are never trusted.
func forwardRetryHints(w http.ResponseWriter, h http.Header) {
	if v := h.Get("Retry-After"); v != "" {
		w.Header().Set("Retry-After", v)
	}
	for name, values := range h {
		lower := strings.ToLower(name)
		if strings.Contains(lower, "ratelimit") && len(values) > 0 {
			// Header names are validated by the transport's bounded parser;
			// keep the first value only.
			w.Header().Set(http.CanonicalHeaderKey(name), values[0])
		}
	}
}

// deadlineWriter enforces the per-write downstream deadline: a slow client
// cannot stall a generation indefinitely. A failed write or flush is sticky:
// the SSE translator stops on the first downstream failure and the handler
// cancels upstream work.
type deadlineWriter struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	timeout time.Duration
	failed  bool
}

func (d *deadlineWriter) Write(p []byte) (int, error) {
	if d.failed {
		return 0, fmt.Errorf("downstream write failed")
	}
	if d.timeout > 0 {
		d.rc.SetWriteDeadline(time.Now().Add(d.timeout))
	}
	n, err := d.w.Write(p)
	if err != nil {
		d.failed = true
		return n, err
	}
	return n, nil
}
