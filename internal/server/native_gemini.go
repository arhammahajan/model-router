package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/arhammahajan/router/internal/capability"
	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/protocol"
	"github.com/arhammahajan/router/internal/router"
	"github.com/arhammahajan/router/internal/transport"
)

// knownUnsupportedGeminiMethods mirrors the Python router's recognized but
// unimplemented Gemini methods: explicit 501, not a silent forward.
var knownUnsupportedGeminiMethods = map[string]bool{
	"countTokens": true, "countMessageTokens": true, "embedContent": true,
	"batchEmbedContents": true, "batchGenerateContent": true,
	"asyncBatchEmbedContent": true, "predict": true, "predictLongRunning": true,
	"bidiGenerateContent": true, "generateAnswer": true,
}

// geminiCall is a parsed /{version|""}/{models|tunedModels}/{name}:{method}
// ingress path. Name is percent-decoded and may contain "/" and ":".
type geminiCall struct {
	kind   string
	name   string
	method string
}

// parseGeminiPath parses an escaped request path for generation methods. The
// input must be r.URL.EscapedPath(): net/http has already decoded URL.Path
// once, so unescaping Path again would double-decode legitimate "%2F"
// sequences in model aliases. EscapedPath preserves the wire encoding and a
// single PathUnescape here decodes exactly once, matching Python's
// urlsplit(self.path).path + unquote(name) single decode. Splitting on "/"
// before decoding keeps an encoded "%2F" inside the name (both raw "/" and
// "%2F" resolve to the same "/" model, per the baseline); splitting on the
// last ":" before decoding keeps an encoded "%3A" inside the name instead of
// treating it as the method separator. It accepts the /v1beta and /v1
// version prefixes as well as version-less /models/ and /tunedModels/ forms.
// It returns ok=false when the path is not a "<resource>:<Method>" shape.
func parseGeminiPath(path string) (geminiCall, bool) {
	rest := path
	for _, prefix := range []string{"/v1beta", "/v1"} {
		if rest == prefix {
			return geminiCall{}, false
		}
		if strings.HasPrefix(rest, prefix+"/") {
			rest = rest[len(prefix):]
			break
		}
	}
	var kind string
	for _, k := range []string{"models", "tunedModels"} {
		head := "/" + k + "/"
		if strings.HasPrefix(rest, head) && len(rest) > len(head) {
			kind = k
			rest = rest[len(head):]
			break
		}
	}
	if kind == "" {
		return geminiCall{}, false
	}
	name, method, ok := splitGeminiMethod(rest)
	if !ok {
		return geminiCall{}, false
	}
	decoded, err := url.PathUnescape(name)
	if err != nil {
		return geminiCall{}, false
	}
	if decoded == "" {
		return geminiCall{}, false
	}
	return geminiCall{kind: kind, name: decoded, method: method}, true
}

func splitGeminiMethod(rest string) (string, string, bool) {
	idx := strings.LastIndex(rest, ":")
	if idx <= 0 || idx == len(rest)-1 {
		return "", "", false
	}
	name, method := rest[:idx], rest[idx+1:]
	for _, ch := range method {
		if !((ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z')) {
			return "", "", false
		}
	}
	return name, method, true
}

// handleGemini serves POST /v1beta/models/{name}:generateContent and
// :streamGenerateContent (plus /v1 and version-less forms, tunedModels).
// The model and streaming mode come from the URL; a body that also names
// "model" or "stream" is rejected. Only alt=sse|json is accepted and only
// alt=sse is forwarded; any ?key= credential is auth-only and stripped.
// All other query parameters are rejected before upstream I/O.
func (s *Server) handleGemini(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	status := 0
	defer func() {
		log.Printf("route=gemini status=%d duration_ms=%d", status, time.Since(start).Milliseconds())
	}()
	fail := func(code int, rerr protocol.RouterError) {
		status = code
		s.writeIngressError(w, config.DialectGemini, rerr)
	}
	// Discovery GETs route before the generation 405: list + resource get
	// share one pinned snapshot per report; POST behavior below is unchanged.
	if r.Method == http.MethodGet {
		if handled, code := s.serveGeminiGetDispatch(w, r); handled {
			status = code
			return
		}
		fail(404, protocol.RouterError{Status: 404, Code: "unknown_route",
			Msg: "Unknown route or unsupported Gemini method; generateContent and streamGenerateContent are supported"})
		return
	}
	if r.Method != http.MethodPost {
		fail(405, protocol.RouterError{Status: 405, Code: protocol.CodeInvalidRequest, Msg: "Method not allowed"})
		return
	}
	call, ok := parseGeminiPath(r.URL.EscapedPath())
	if !ok {
		fail(404, protocol.RouterError{Status: 404, Code: "unknown_route",
			Msg: "Unknown route or unsupported Gemini method; generateContent and streamGenerateContent are supported"})
		return
	}
	if call.method != "generateContent" && call.method != "streamGenerateContent" {
		if knownUnsupportedGeminiMethods[call.method] {
			fail(501, protocol.RouterError{Status: 501, Code: "unimplemented_method",
				Msg: "Method " + call.method + " is not implemented by this router; generateContent and streamGenerateContent are supported"})
			return
		}
		fail(404, protocol.RouterError{Status: 404, Code: "unknown_route",
			Msg: "Unknown route or unsupported Gemini method; generateContent and streamGenerateContent are supported"})
		return
	}
	// Query allowlist: alt (0-1 of sse|json) and key (auth-only, stripped).
	// Anything else is rejected before credentials or the query travel.
	q := r.URL.Query()
	for name := range q {
		if name != "alt" && name != "key" {
			fail(400, protocol.RouterError{Status: 400, Code: protocol.CodeInvalidRequest,
				Msg: "Unsupported query parameter; only 'alt' and 'key' are accepted"})
			return
		}
	}
	alts := q["alt"]
	alt := ""
	if len(alts) > 0 {
		if len(alts) > 1 || (alts[0] != "sse" && alts[0] != "json") {
			fail(400, protocol.RouterError{Status: 400, Code: protocol.CodeInvalidRequest,
				Msg: "'alt' must be 'sse' or 'json'"})
			return
		}
		alt = alts[0]
	}
	if !s.authedQuery(r, true) {
		fail(401, protocol.RouterError{Status: 401, Code: protocol.CodeUnauthorized, Msg: "Invalid API key"})
		return
	}
	// Absolute deadline covers preparation, attempts, backoff and full
	// stream consumption; ingress body reads stay on the separate
	// body_read_timeout inside readBody.
	deadline := start.Add(time.Duration(s.cfg.Retry.Deadline * float64(time.Second)))
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()
	select {
	case s.gens <- struct{}{}:
		defer func() { <-s.gens }()
	case <-ctx.Done():
		fail(504, protocol.RouterError{Status: 504, Code: "request_deadline_exceeded",
			Msg: "Overall upstream request deadline exceeded; generation may have been accepted and billed."})
		return
	default:
		fail(503, protocol.RouterError{Status: 503, Code: protocol.CodeOverloaded,
			Msg: "Server is overloaded, try again later"})
		return
	}
	body, rerr := s.readBody(w, r.WithContext(ctx))
	if rerr != nil {
		fail(rerr.Status, *rerr)
		return
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		fail(400, protocol.RouterError{Status: 400, Code: protocol.CodeInvalidRequest,
			Msg: "'body' must be a JSON object"})
		return
	}
	if _, has := obj["model"]; has {
		fail(400, protocol.RouterError{Status: 400, Code: protocol.CodeInvalidRequest,
			Msg: "Invalid JSON payload received. Unknown name \"model\": Cannot find field."})
		return
	}
	if _, has := obj["stream"]; has {
		fail(400, protocol.RouterError{Status: 400, Code: protocol.CodeInvalidRequest,
			Msg: "Invalid JSON payload received. Unknown name \"stream\": Cannot find field."})
		return
	}
	// Pin the current snapshot once where the model is resolved; the
	// same pin resolves the route and supplies capability facts and the
	// local runtime to both translated bridges below. The custom URL
	// preamble keeps the path-owned model/method (never startPreamble,
	// which reads a body "model").
	snap := s.reg.Current()
	model, provider, merr := resolveSnapshotModel(snap, call.name)
	if merr != nil {
		if merr.Code != "model_not_found" {
			fail(merr.Status, *merr)
			return
		}
		fail(404, protocol.RouterError{Status: 404, Code: "model_not_found",
			Msg: "models/" + call.name + " is not found for API version v1beta. Call ListModels to see the list of available models."})
		return
	}
	dest := model.EffectiveDialect(provider)
	mode, derr := dispatchNativeOrTranslated(config.DialectGemini, dest)
	if derr != nil {
		fail(derr.Status, *derr)
		return
	}
	// Usage accounting hook, mirroring handleChat: one wrap per ingress,
	// finished after the dispatch returns with the downstream bytes
	// Gemini-shaped on every exit of this ingress. The fail closure keeps
	// the unwrapped writer; those errors still record with zero tokens.
	if s.acc != nil {
		var finishAccounting func(int)
		w, finishAccounting = s.beginAccounting(w, config.DialectGemini, dest, model.Name, provider.Name,
			call.method == "streamGenerateContent", start)
		defer func() { finishAccounting(status) }()
	}
	// Opt-in compat pre-step (Gemini generation POST only): collect
	// carrier tokens, consume sessionId and validate/mutate the effort
	// carrier against the SAME pinned snapshot used for planning below.
	// Headers stay intact; the fallback threads into session selection.
	var fb router.CompatFallback
	compatEffort := protocol.AbsentReasoning()
	if s.cfg.Server.BearerSessionCompat {
		contract := snap.RequestCapability(model, provider)
		published := capability.ReasoningFor(contract).EffortValues
		newBody, newFb, newEffort, cerr := router.PrepareGeminiCompat(r.Header, r.URL.Query()["key"], body, published, dest)
		if cerr != nil {
			fail(cerr.Status, *cerr)
			return
		}
		body = newBody
		fb = newFb
		compatEffort = newEffort
	}
	// One shared affinity prep for the native and all three Gemini bridges:
	// select the identity and consume prompt_cache_key on translated routes.
	sess, effBody, serr := prepareSession(provider, r.Header, body, mode != "native", fb)
	if serr != nil {
		fail(serr.Status, *serr)
		return
	}
	body = effBody
	if mode == "gemini_to_chat" {
		// Branch before native forwarding: the parsed URL model/method
		// selects the public name and the streaming mode; the body carries
		// neither. The auth, query allowlist, deadline, generation slot
		// and body handling above are shared with the native path.
		r = r.WithContext(ctx)
		status = s.serveGeminiToChat(w, r, provider, model, body, call, alt, sess, snap, compatEffort)
		return
	}
	if mode == "gemini_to_responses" {
		// Same shared preamble as the Chat bridge: URL-owned model/method,
		// auth, query allowlist, deadline, generation slot and body.
		r = r.WithContext(ctx)
		status = s.serveGeminiToResponses(w, r, provider, model, body, call, alt, sess, snap, compatEffort)
		return
	}
	if mode == "gemini_to_anthropic" {
		// Same shared preamble: URL-owned model/method, auth, query
		// allowlist, deadline, generation slot and body.
		r = r.WithContext(ctx)
		status = s.serveGeminiToAnthropic(w, r, provider, model, body, call, alt, sess, compatEffort)
		return
	}
	if mode != "native" {
		fail(400, protocol.RouterError{Status: 400, Code: "unsupported_route",
			Msg: "Upstream dialect is not implemented for this ingress; use a same-protocol route"})
		return
	}
	// Dynamic provider/model routes resolve the public name; the upstream
	// path always uses the configured upstream model ID, escaped.
	upstreamName := providerUpstreamName(provider, model, call.kind)
	forwardAlt := ""
	if alt == "sse" {
		forwardAlt = "sse"
	}
	u, rerr := transport.GeminiURL(provider.BaseURL, upstreamName, call.method, forwardAlt)
	if rerr != nil {
		fail(rerr.Status, *rerr)
		return
	}
	wantStream := call.method == "streamGenerateContent"
	// Gemini streams (SSE or JSON array) preserve the upstream content
	// type: never force text/event-stream.
	r = r.WithContext(ctx)
	status = s.serveNativeForward(w, r, provider, body, wantStream, false, u, config.DialectGemini, dest, sess)
}

// providerUpstreamName selects the upstream model resource segment: the
// configured upstream ID, preserving an explicit models|tunedModels prefix.
func providerUpstreamName(_ *config.Provider, model *config.Model, _ string) string {
	return model.Upstream
}
