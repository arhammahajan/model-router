// Package transport owns upstream HTTP: credential/header rules, deadlines,
// retry decisions and response validation. One component performs retries;
// no other layer replays a generation.
package transport

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/arhammahajan/model-router/internal/config"
	"github.com/arhammahajan/model-router/internal/endpoint"
	"github.com/arhammahajan/model-router/internal/protocol"
)

// Version is the User-Agent product token.
const Version = "model-router-go/0.1.0"

// dropRequestHeaders are never forwarded upstream: client credentials,
// transport framing and representation metadata.
var dropRequestHeaders = map[string]bool{
	"connection": true, "keep-alive": true, "proxy-authenticate": true,
	"proxy-authorization": true, "te": true, "trailer": true,
	"transfer-encoding": true, "upgrade": true, "host": true,
	"content-length": true, "authorization": true, "x-api-key": true,
	"api-key": true, "x-goog-api-key": true,
	"x-model-router-reasoning-effort": true,
	"x-goog-user-project":             true, "x-goog-quota-user": true,
	"cookie":       true,
	"content-type": true, "content-encoding": true, "content-md5": true,
	"content-digest": true, "repr-digest": true, "digest": true,
	"content-range": true, "content-location": true, "content-language": true,
	"etag": true, "last-modified": true, "signature": true,
	"signature-input": true,
}

// authHeaderNames are scrubbed before the selected configured credential is
// applied, so a provider static header can never leave a second credential
// beside x-api-key/Authorization.
var authHeaderNames = map[string]bool{
	"authorization": true, "x-api-key": true, "api-key": true,
	"x-goog-api-key": true,
}

// sessionHeaderNames are never forwarded from inbound: only the single
// selected identity chosen by request preparation may travel upstream,
// under the provider's configured destination header.
var sessionHeaderNames = map[string]bool{
	"x-claude-code-session-id": true, "session-id": true, "session_id": true,
	"thread-id": true, "conversation_id": true, "x-session-id": true,
	"x-mavis-session-id": true, "x-opencode-session": true,
}

// idempotencyHeaderNames are never forwarded: a client replay key must not
// let the hidden HTTP layer or the upstream treat a generation as replayable.
var idempotencyHeaderNames = map[string]bool{
	"idempotency-key": true, "x-idempotency-key": true,
}

// translationOptIns are the only client-controlled headers on a bridged
// (cross-dialect) request, keyed by destination dialect. Wrong-dialect
// opt-ins (e.g. openai-beta to Anthropic) are dropped.
var translationOptIns = map[string]map[string]bool{
	config.DialectAnthropic:       {"anthropic-version": true, "anthropic-beta": true},
	config.DialectOpenAIChat:      {"openai-beta": true},
	config.DialectOpenAIResponses: {"openai-beta": true},
	config.DialectGemini:          {},
}

// ingressDialect maps the server ingress label to its dialect.
func ingressDialect(ingress string) string {
	switch ingress {
	case "chat":
		return config.DialectOpenAIChat
	case "responses":
		return config.DialectOpenAIResponses
	case "messages", "count_tokens":
		return config.DialectAnthropic
	case "gemini":
		return config.DialectGemini
	}
	return ingress
}

// connectionNominated collects Connection-nominated hop-by-hop headers,
// including repeated Connection fields.
func connectionNominated(h http.Header) map[string]bool {
	out := map[string]bool{}
	for name, values := range h {
		if !strings.EqualFold(name, "connection") {
			continue
		}
		for _, v := range values {
			for _, token := range strings.Split(v, ",") {
				token = strings.ToLower(strings.TrimSpace(token))
				if token != "" {
					out[token] = true
				}
			}
		}
	}
	return out
}

// UpstreamURL joins base_url and the generation path for a dialect,
// mirroring the Python router's suffix-tolerant rules. The derivation itself
// lives in internal/endpoint so config startup comparison reuses the exact
// same implementation; this wrapper only maps the neutral error into the
// existing RouterError shape. Signature unchanged.
func UpstreamURL(baseURL, dialect string) (string, *protocol.RouterError) {
	u, err := endpoint.GenerationEndpoint(baseURL, dialect)
	if err != nil {
		return "", &protocol.RouterError{Status: 500, Code: "internal",
			Msg: fmt.Sprintf("unsupported upstream dialect %q in this build", dialect)}
	}
	return u, nil
}

// CountTokensURL derives the Anthropic token-count endpoint from the same
// suffix-tolerant base rules as generation: <messages>/count_tokens.
func CountTokensURL(baseURL string) (string, *protocol.RouterError) {
	messages, rerr := UpstreamURL(baseURL, config.DialectAnthropic)
	if rerr != nil {
		return "", rerr
	}
	return messages + "/count_tokens", nil
}

// CountTokensEnabled mirrors the Python baseline: only an Anthropic-protocol
// provider known or declared to implement the endpoint. An explicit
// count_tokens setting wins; absent, only the exact first-party host
// https://api.anthropic.com qualifies (userinfo, ports and look-alikes fail).
func CountTokensEnabled(p *config.Provider) bool {
	if p.Dialect != config.DialectAnthropic {
		return false
	}
	if p.CountTokens != nil {
		return *p.CountTokens
	}
	u, err := url.Parse(strings.TrimSpace(p.BaseURL))
	if err != nil {
		return false
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return false
	}
	if u.User != nil {
		return false
	}
	return strings.ToLower(u.Host) == "api.anthropic.com"
}

// GeminiURL builds the upstream generateContent URL for a resolved model.
// The model is client-influenced on dynamic provider/model routes and lives
// in the URL path, so it is validated against traversal/query injection and
// percent-encoded preserving only "/" and ":" (mirroring the Python router's
// quote(name, safe="/:")). Only alt="sse" is forwarded as a query; every
// other client query (including any ?key= credential) is never forwarded.
func GeminiURL(baseURL, upstreamModel, method, alt string) (string, *protocol.RouterError) {
	if method != "generateContent" && method != "streamGenerateContent" {
		return "", &protocol.RouterError{Status: 404, Code: "unknown_route",
			Msg: fmt.Sprintf("Unknown Gemini method %q", method)}
	}
	name := upstreamModel
	if !strings.HasPrefix(name, "models/") && !strings.HasPrefix(name, "tunedModels/") {
		name = "models/" + name
	}
	if strings.ContainsAny(name, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c\x0d\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f\x7f?#%\\&") {
		return "", &protocol.RouterError{Status: 400, Code: "invalid_model",
			Msg: "The upstream model name cannot be used in a Gemini request path"}
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", &protocol.RouterError{Status: 400, Code: "invalid_model",
				Msg: "The upstream model name cannot be used in a Gemini request path"}
		}
	}
	if len(strings.Split(name, "/")) < 2 {
		return "", &protocol.RouterError{Status: 400, Code: "invalid_model",
			Msg: "The upstream model name cannot be used in a Gemini request path"}
	}
	base, rerr := UpstreamURL(baseURL, config.DialectGemini)
	if rerr != nil {
		return "", rerr
	}
	u := base + "/" + escapeGeminiName(name) + ":" + method
	if alt == "sse" {
		u += "?alt=sse"
	}
	return u, nil
}

// escapeGeminiName percent-encodes a validated model resource name,
// preserving "/" and ":" like the Python router's quote(name, safe="/:").
func escapeGeminiName(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '/' || c == ':' ||
			(c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// AuthHeaders returns the upstream credential headers. The key always comes
// from provider config; client credentials never become upstream credentials.
func AuthHeaders(p *config.Provider) map[string]string {
	key := p.EffectiveKey()
	switch strings.ToLower(p.AuthHeader) {
	case "bearer":
		return map[string]string{"Authorization": "Bearer " + key}
	case "x-api-key", "api-key":
		return map[string]string{p.AuthHeader: key}
	}
	if p.Dialect == config.DialectAnthropic {
		return map[string]string{"x-api-key": key}
	}
	if p.Dialect == config.DialectGemini {
		return map[string]string{"x-goog-api-key": key}
	}
	return map[string]string{"Authorization": "Bearer " + key}
}

// OutboundHeaders builds the upstream request headers from inbound client
// headers plus trusted provider headers and auth. sessionID/hasSession is
// the request-scoped selection from request preparation (exactly one
// identity or none): it is emitted under provider.SessionHeader after every
// other source is scrubbed, so statics, client casing and Connection
// nomination cannot smuggle a second identity.
//
//   - Connection-nominated headers are always stripped.
//   - Native (same-dialect) requests forward reviewed non-credential inbound
//     headers; bridged requests forward only destination-dialect opt-ins.
//   - Arbitrary inbound session and idempotency identities are never forwarded.
//   - All auth names are scrubbed before the selected configured credential
//     is applied, so provider statics cannot leave a second credential.
//   - Framing (Content-Type/Accept-Encoding) is enforced last.
//
// ingress is the server ingress label ("chat", "responses", "messages",
// "count_tokens" or "gemini"); dest is the actual egress destination dialect
// (model.EffectiveDialect(provider)). Provider stays configuration identity:
// AuthHeaders(p) keeps the provider-owned choice even when dest differs.
func OutboundHeaders(inbound http.Header, p *config.Provider, ingress, dest string, sessionID string, hasSession bool) map[string]string {
	nominated := connectionNominated(inbound)
	if dest == "" {
		dest = p.Dialect
	}
	src := ingressDialect(ingress)
	native := dest == src

	// Provider-configured session names are also treated as session headers
	// for inbound filtering (defensive; config rejects session wiring).
	extraSession := map[string]bool{}
	if p.SessionHeader != "" {
		extraSession[strings.ToLower(p.SessionHeader)] = true
	}
	for _, s := range p.SessionSourceHeaders {
		extraSession[strings.ToLower(s)] = true
	}

	allowedTranslated := translationOptIns[dest]

	out := map[string]string{}
	for name, values := range inbound {
		lower := strings.ToLower(name)
		if lower == "user-agent" {
			continue
		}
		if dropRequestHeaders[lower] || nominated[lower] {
			continue
		}
		if sessionHeaderNames[lower] || extraSession[lower] {
			continue
		}
		if idempotencyHeaderNames[lower] {
			continue
		}
		if authHeaderNames[lower] {
			continue
		}
		if !native {
			if !allowedTranslated[lower] {
				continue
			}
		}
		if len(values) > 0 && values[0] != "" {
			out[http.CanonicalHeaderKey(name)] = values[0]
		}
	}
	out["Accept-Encoding"] = "identity"
	if _, ok := out["User-Agent"]; !ok {
		out["User-Agent"] = "litellm-model-router/" + Version
	}
	// Legacy translation headers apply to translated traffic only, before
	// provider.headers so provider.headers wins case-insensitively.
	// Native behavior is unchanged.
	if !native {
		for name, value := range p.TranslatedHeaders {
			out[http.CanonicalHeaderKey(name)] = value
		}
	}
	for name, value := range p.Headers {
		out[http.CanonicalHeaderKey(name)] = value
	}
	// Scrub every auth name (inbound leftovers cannot exist, but provider
	// statics can) before applying the single selected credential.
	for name := range out {
		if authHeaderNames[strings.ToLower(name)] {
			delete(out, name)
		}
	}
	for name, value := range AuthHeaders(p) {
		out[http.CanonicalHeaderKey(name)] = value
	}
	// Emit exactly one session identity: scrub every casing of the
	// consumed names (reviewed natives, the router-owned token, the
	// configured destination and sources) so no static, client or
	// nominated value survives beside the selection. Auth above is
	// untouched: the destination is a dedicated identity header.
	if p.SessionHeader != "" {
		want := strings.ToLower(p.SessionHeader)
		for name := range out {
			lower := strings.ToLower(name)
			if sessionHeaderNames[lower] || lower == want || extraSession[lower] {
				delete(out, name)
			}
		}
		if hasSession {
			out[http.CanonicalHeaderKey(p.SessionHeader)] = sessionID
		}
	}
	if dest == config.DialectAnthropic {
		found := false
		for name := range out {
			if strings.ToLower(name) == "anthropic-version" {
				found = true
				break
			}
		}
		if !found {
			out["Anthropic-Version"] = protocol.AnthropicVersion
		}
	}
	// Trusted statics cannot override the serialized body's framing.
	out["Content-Type"] = "application/json"
	out["Accept-Encoding"] = "identity"
	return out
}

// Client is an upstream HTTP client with redirects disabled and the
// deadline/retry policy. Transport is immutable during a request.
//
// The underlying http.Transport is dedicated (never http.DefaultTransport)
// so request cancellation cannot close another request's pooled connection,
// while environment proxy selection is preserved deliberately.
type Client struct {
	http      *http.Client
	transport *http.Transport
}

// NewClient builds the shared upstream client. Redirects are disabled:
// a redirect must never provide an alternate path around credential,
// URL and retry rules.
func NewClient() *Client {
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     60 * time.Second,
	}
	return &Client{
		transport: tr,
		http: &http.Client{
			Transport: tr,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// CloseIdleConnections closes idle upstream keep-alive connections.
func (c *Client) CloseIdleConnections() {
	if c == nil || c.transport == nil {
		return
	}
	c.transport.CloseIdleConnections()
}

// Result is one completed upstream attempt. For non-2xx statuses that
// are not retried, Failed is true and Body carries the bounded upstream
// error body so the server can preserve the upstream status.
type Result struct {
	Status int
	Header http.Header
	Body   []byte // nil for streams; the caller consumes Stream instead
	Stream io.ReadCloser
	Failed bool
}

// streamBody keeps the stream request context alive until the caller closes
// the body: cancelling on return would close the streaming connection while
// the handler is still consuming it.
type streamBody struct {
	rc     io.ReadCloser
	cancel context.CancelFunc
}

func (s *streamBody) Read(p []byte) (int, error) { return s.rc.Read(p) }

func (s *streamBody) Close() error {
	err := s.rc.Close()
	s.cancel()
	return err
}

// Do sends body under the absolute deadline, retrying only eligible failures
// before any output commitment. Retry-After is honored as a minimum wait
// within the deadline. An ambiguous send/read failure, a truncated response
// or started downstream output is never retried.
//
// The incoming context should already carry the absolute request deadline
// (set at handler entry so preparation time counts). Do caps it by
// now+retry.Deadline for callers without one. Per-attempt socket work is
// bounded by the provider timeout clamped to the remaining budget.
func (c *Client) Do(ctx context.Context, p *config.Provider, retry config.Retry,
	url string, headers map[string]string, body []byte, stream bool, maxBytes int64,
) (*Result, *protocol.RouterError) {
	deadline, ok := ctx.Deadline()
	cap := time.Now().Add(time.Duration(retry.Deadline * float64(time.Second)))
	if !ok || cap.Before(deadline) {
		deadline = cap
	}
	attempt := 0
	for {
		attempt++
		res, retryable, rerr := c.attempt(ctx, p, retry, url, headers, body, stream, deadline, maxBytes)
		if rerr == nil {
			return res, nil
		}
		if !retryable || attempt >= retry.MaxAttempts {
			// Non-retried HTTP error statuses still carry their bounded
			// body so the server can preserve the upstream status.
			if res != nil {
				return res, rerr.RouterError
			}
			return nil, rerr.RouterError
		}
		wait := backoff(retry, attempt)
		if ra := rerr.retryAfter; ra > 0 && ra > wait {
			wait = ra
		}
		if time.Now().Add(wait).After(deadline) {
			return nil, &protocol.RouterError{Status: 504, Code: "request_deadline_exceeded",
				Msg: "Overall upstream request deadline exceeded; generation may have been accepted and billed."}
		}
		select {
		case <-ctx.Done():
			return nil, ctxErr(ctx)
		case <-time.After(wait):
		}
	}
}

type attemptError struct {
	*protocol.RouterError
	retryAfter time.Duration
}

func (c *Client) attempt(ctx context.Context, p *config.Provider, retry config.Retry,
	url string, headers map[string]string, body []byte, stream bool, deadline time.Time,
	maxBytes int64,
) (*Result, bool, *attemptError) {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return nil, false, &attemptError{RouterError: &protocol.RouterError{Status: 504,
			Code: "request_deadline_exceeded",
			Msg:  "Overall upstream request deadline exceeded; generation may have been accepted and billed."}}
	}
	perAttempt := remaining
	if p.Timeout > 0 {
		if d := time.Duration(p.Timeout * float64(time.Second)); d < perAttempt {
			perAttempt = d
		}
	}
	reqCtx := ctx
	var cancel context.CancelFunc = func() {}
	if stream {
		// The stream context must live until the caller closes the body;
		// ownership of cancel moves to the streamBody wrapper below.
		var sc context.Context
		sc, cancel = context.WithTimeout(ctx, perAttempt)
		reqCtx = sc
	} else {
		var nc context.Context
		nc, cancel = context.WithTimeout(ctx, perAttempt)
		reqCtx = nc
		defer cancel()
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		if stream {
			cancel()
		}
		return nil, false, &attemptError{RouterError: &protocol.RouterError{Status: 502,
			Code: protocol.CodeUpstreamBad, Msg: "Upstream request failed"}}
	}
	// Disable hidden replay: the default Transport replays requests with
	// GetBody on reused-connection failures. Generations must never replay
	// except through the explicit status/pre-send policy below.
	req.GetBody = nil
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if stream {
			cancel()
		}
		// Context deadlines (provider timeout or absolute deadline) are 504
		// precommit failures, never retried as ambiguous.
		if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) ||
			errors.Is(err, context.Canceled) || isTimeout(err) {
			return nil, false, &attemptError{RouterError: &protocol.RouterError{Status: 504,
				Code: "request_deadline_exceeded",
				Msg:  "Overall upstream request deadline exceeded; generation may have been accepted and billed."}}
		}
		// Only failures where nothing was sent are eligible for retry, and
		// only when connect_errors is enabled; anything after the request
		// left is ambiguous and never replayed.
		return nil, retry.ConnectErrors && isPreSend(err), &attemptError{RouterError: &protocol.RouterError{Status: 502,
			Code: protocol.CodeUpstreamBad, Msg: "Upstream request failed"}}
	}
	if isRedirect(resp.StatusCode) {
		resp.Body.Close()
		if stream {
			cancel()
		}
		return nil, false, &attemptError{RouterError: &protocol.RouterError{Status: 502,
			Code: protocol.CodeUpstreamBad, Msg: "Upstream redirect refused"}}
	}
	if resp.StatusCode == 429 || resp.StatusCode >= 500 {
		ra := parseRetryAfter(resp.Header.Get("Retry-After"))
		b, rerr := readBounded(resp.Body, 64*1024, resp.Header.Get("Content-Encoding"))
		resp.Body.Close()
		if rerr != nil {
			if stream {
				cancel()
			}
			// A truncated error body is ambiguous: never retry. A
			// timeout/deadline during body consumption is a 504
			// precommit failure, never retried.
			return nil, false, &attemptError{RouterError: bodyReadRouterError(ctx, reqCtx, rerr)}
		}
		if int64(len(b)) > 64*1024 {
			if stream {
				cancel()
			}
			// Oversize error bodies are truncated: never retry, never
			// preserve the original status.
			return nil, false, &attemptError{RouterError: &protocol.RouterError{Status: 502,
				Code: protocol.CodeUpstreamBad, Msg: "Upstream response was truncated"}}
		}
		if retryableStatus(retry, resp.StatusCode) {
			if stream {
				cancel()
			}
			// Materialize the bounded body so final exhaustion can
			// preserve status/body/Retry-After; intermediate retries
			// discard it by looping.
			return &Result{Status: resp.StatusCode, Header: resp.Header,
					Body: b, Failed: true}, true, &attemptError{
					RouterError: upErr(resp.StatusCode),
					retryAfter:  ra,
				}
		}
		if stream {
			// Error statuses never stream: cancel the header context only
			// when the body is materialized; the returned Failed result
			// carries no open stream.
			cancel()
			return &Result{Status: resp.StatusCode, Header: resp.Header,
					Body: b, Failed: true}, false,
				&attemptError{RouterError: upErr(resp.StatusCode), retryAfter: ra}
		}
		return &Result{Status: resp.StatusCode, Header: resp.Header,
				Body: b, Failed: true}, false,
			&attemptError{RouterError: upErr(resp.StatusCode), retryAfter: ra}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, rerr := readBounded(resp.Body, 64*1024, resp.Header.Get("Content-Encoding"))
		resp.Body.Close()
		if stream {
			cancel()
		}
		if rerr != nil {
			return nil, false, &attemptError{RouterError: bodyReadRouterError(ctx, reqCtx, rerr)}
		}
		if int64(len(b)) > 64*1024 {
			return nil, false, &attemptError{RouterError: &protocol.RouterError{Status: 502,
				Code: protocol.CodeUpstreamBad, Msg: "Upstream response was truncated"}}
		}
		return &Result{Status: resp.StatusCode, Header: resp.Header,
				Body: b, Failed: true}, false,
			&attemptError{RouterError: upErr(resp.StatusCode)}
	}
	if stream {
		if enc := resp.Header.Get("Content-Encoding"); enc != "" &&
			!strings.EqualFold(strings.TrimSpace(enc), "identity") {
			// Refuse compressed streams: Content-Encoding is stripped on
			// forwarding, so compressed bytes would corrupt the client.
			resp.Body.Close()
			cancel()
			return nil, false, &attemptError{RouterError: &protocol.RouterError{Status: 502,
				Code: protocol.CodeUpstreamBad, Msg: "Upstream response encoding is not supported"}}
		}
		wrapped := &streamBody{rc: resp.Body, cancel: cancel}
		return &Result{Status: resp.StatusCode, Header: resp.Header, Stream: wrapped}, false, nil
	}
	b, rerr := readBounded(resp.Body, maxBytes+1, resp.Header.Get("Content-Encoding"))
	resp.Body.Close()
	if rerr != nil {
		// A truncated response is never retried: the generation might have
		// been accepted and billed. Timeouts/deadlines during body
		// consumption are 504 precommit failures, never retried.
		return nil, false, &attemptError{RouterError: bodyReadRouterError(ctx, reqCtx, rerr)}
	}
	if int64(len(b)) > maxBytes {
		return nil, false, &attemptError{RouterError: &protocol.RouterError{Status: 502,
			Code: protocol.CodeUpstreamBad, Msg: "Upstream response exceeds the size bound"}}
	}
	return &Result{Status: resp.StatusCode, Header: resp.Header, Body: b}, false, nil
}

// readBounded reads r bounded before and after decompression. Only identity
// (or absent) and gzip are accepted; anything else is refused before commit.
//
// Both the raw compressed framing and the decompressed payload share the
// same explicit cap (max): a raw body longer than max is a truncation
// error, and a decompressed body longer than max is returned over-long so
// the caller maps it consistently (success: size-bound 502; error
// statuses: truncated 502, never preserved).
func readBounded(r io.Reader, max int64, contentEncoding string) ([]byte, error) {
	enc := strings.ToLower(strings.TrimSpace(contentEncoding))
	switch enc {
	case "", "identity":
		b, err := io.ReadAll(io.LimitReader(r, max+1))
		if err != nil {
			return nil, err
		}
		return b, nil
	case "gzip":
		raw := &io.LimitedReader{R: r, N: max + 1}
		gr, err := gzip.NewReader(raw)
		if err != nil {
			return nil, err
		}
		defer gr.Close()
		b, err := io.ReadAll(io.LimitReader(gr, max+1))
		if err != nil {
			return nil, err
		}
		if raw.N == 0 {
			return nil, errors.New("response exceeds the size bound")
		}
		return b, nil
	default:
		return nil, errors.New("unsupported content-encoding")
	}
}

// bodyReadRouterError maps a bounded body-read failure to its terminal
// status: timeouts/deadlines during body consumption are 504 precommit
// failures, never retried; anything else is a truncated 502, never retried.
func bodyReadRouterError(ctx, reqCtx context.Context, err error) *protocol.RouterError {
	if ctx.Err() != nil || reqCtx.Err() != nil ||
		errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
		isTimeout(err) || os.IsTimeout(err) {
		return &protocol.RouterError{Status: 504, Code: "request_deadline_exceeded",
			Msg: "Overall upstream request deadline exceeded; generation may have been accepted and billed."}
	}
	return &protocol.RouterError{Status: 502,
		Code: protocol.CodeUpstreamBad, Msg: "Upstream response was truncated"}
}

func isRedirect(status int) bool {
	return status == 301 || status == 302 || status == 303 || status == 307 || status == 308
}

// retryableStatus reports whether a status is configured for replay.
func retryableStatus(r config.Retry, status int) bool {
	for _, s := range r.Statuses {
		if s == status {
			return true
		}
	}
	return false
}

// isTimeout reports network timeouts that must surface as 504, never retry.
func isTimeout(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Timeout() {
		return true
	}
	return false
}

// isPreSend reports whether a transport error happened before anything was
// sent (refused connection, DNS failure). Only those are safe to replay;
// timeouts and mid-request failures are ambiguous and never retried.
func isPreSend(err error) bool {
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		var sysErrno syscall.Errno
		if errors.As(opErr.Err, &sysErrno) {
			return sysErrno == syscall.ECONNREFUSED
		}
	}
	return false
}

func upErr(status int) *protocol.RouterError {
	code := "api_error"
	if status < 500 {
		code = "invalid_request_error"
	}
	return &protocol.RouterError{Status: status, Code: code,
		Msg: fmt.Sprintf("Upstream returned HTTP %d", status)}
}

func ctxErr(ctx context.Context) *protocol.RouterError {
	if ctx.Err() == context.DeadlineExceeded {
		return &protocol.RouterError{Status: 504, Code: "request_deadline_exceeded",
			Msg: "Overall upstream request deadline exceeded; generation may have been accepted and billed."}
	}
	return &protocol.RouterError{Status: 502, Code: protocol.CodeUpstreamBad, Msg: "Upstream request failed"}
}

func backoff(r config.Retry, attempt int) time.Duration {
	cap := r.BackoffMax
	base := r.BackoffBase * math.Pow(2, float64(attempt-1))
	if base > cap {
		base = cap
	}
	// Full jitter in [cap/2, cap], mirroring the Python router.
	half := base / 2
	return time.Duration((half + rand.Float64()*half) * float64(time.Second))
}

func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	// Numeric seconds saturate instead of overflowing time.Duration:
	// an enormous hint must be unaffordable (504), never a replay.
	// time.Duration is int64 nanoseconds, so seconds saturate at
	// maxInt64/1e9 (~292 years).
	if secs, err := strconv.ParseUint(v, 10, 64); err == nil {
		const maxSecs = uint64(1<<63-1) / uint64(time.Second)
		if secs > maxSecs {
			return time.Duration(1<<63 - 1)
		}
		return time.Duration(secs) * time.Second
	} else {
		// A syntactically valid decimal beyond uint64 range reports
		// ErrRange (not ErrSyntax): it must also saturate so the wait
		// is unaffordable, never an ignored hint. Malformed values
		// stay ignored (0) via the date parse below.
		var numErr *strconv.NumError
		if errors.As(err, &numErr) && numErr.Err == strconv.ErrRange {
			return time.Duration(1<<63 - 1)
		}
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
