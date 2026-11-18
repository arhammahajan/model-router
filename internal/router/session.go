// Boundary sketch: session extraction belongs to routing/request
// preparation (this package); outbound identity emission belongs to the
// shared HTTP header/transport boundary (internal/transport). One typed
// request-scoped selection (Session), one validation path
// (PrepareSession): no global map, no session store, no identity magic
// hidden in untrusted headers, no per-bridge copies of the policy and no
// whole-generation buffering. Callers pass the selection explicitly; there
// is no generic framework and no old+new duplicate API.
package router

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/arhammahajan/model-router/internal/config"
	"github.com/arhammahajan/model-router/internal/protocol"
)

// defaultNativeSourceOrder mirrors the reviewed Python baseline order:
// the default extraction priority for native identity headers.
var defaultNativeSourceOrder = [...]string{
	"x-claude-code-session-id", "session-id", "session_id", "thread-id",
	"conversation_id", "x-session-id", "x-mavis-session-id",
}

// Session is one request-scoped affinity selection. Has reports whether an
// identity was selected; ID carries it. The zero value means "no policy or
// no identity": emit nothing.
type Session struct {
	ID  string
	Has bool
}

// validSessionValue reports the reviewed grammar: 1-256 ASCII chars,
// [A-Za-z0-9][A-Za-z0-9._:/-]*. No trimming, generation, hashing or
// coercion happens here; callers pass the exact header/body value.
func validSessionValue(s string) bool {
	if len(s) == 0 || len(s) > 256 {
		return false
	}
	c := s[0]
	if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
		return false
	}
	for i := 1; i < len(s); i++ {
		c = s[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			c == '.' || c == '_' || c == ':' || c == '/' || c == '-') {
			return false
		}
	}
	return true
}

// connectionNominated collects Connection-nominated hop-by-hop names from
// repeated Connection fields. Nominated identity headers are skipped
// entirely: neither validated nor selected.
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

// headerValues gathers every value stored under any casing of name,
// preserving repeated header lines. It uses the full value slices, never
// Header.Get-first.
func headerValues(h http.Header, name string) []string {
	var out []string
	for key, values := range h {
		if strings.EqualFold(key, name) {
			out = append(out, values...)
		}
	}
	return out
}

// promptCacheKey extracts the top-level prompt_cache_key raw value without
// decoding the rest of the body. ok=false when the body is not a JSON
// object or the field is absent.
func promptCacheKey(body []byte) (raw json.RawMessage, ok bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, false
	}
	raw, ok = obj["prompt_cache_key"]
	return raw, ok
}

// consumePromptCacheKey removes ONLY the top-level prompt_cache_key member
// via a RawMessage envelope: every other member keeps its exact raw bytes,
// so large JSON integers never pass through float64.
func consumePromptCacheKey(body []byte) []byte {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	if _, ok := obj["prompt_cache_key"]; !ok {
		return body
	}
	delete(obj, "prompt_cache_key")
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// PrepareSession selects the request's conversation identity and prepares
// the effective ingress body. translated reports whether the route bridges
// dialects (native routes preserve the body byte-for-byte). fb is the final
// typed Gemini compat fallback: it wins only when no earlier candidate won
// and is validated only then. An explicit empty source list disables native
// header selection but never body or fallback selection; chat injection
// arrives as a native header candidate, never as fallback.
//
// Order: explicit configured destination header first, configured native
// sources in order, optional body prompt_cache_key last, compat fallback
// last. Every reviewed native header is validated even when it loses or
// native selection is disabled ([]): duplicates (including case variants)
// and invalid values fail closed with value-free 400s.
// Connection-nominated headers are skipped. Missing identity is omitted or
// rejected per session_missing. Without session_header the body and headers
// pass through untouched: no arbitrary identity is forwarded and none is
// inferred.
func PrepareSession(p *config.Provider, hdr http.Header, body []byte, translated bool, fb CompatFallback) (Session, []byte, *protocol.RouterError) {
	if p == nil || p.SessionHeader == "" {
		return Session{}, body, nil
	}
	// Absent sources (nil) mean the baseline default: all seven reviewed
	// natives in order. An explicit [] disables native selection while
	// validation below still applies. Config normalizes the same default;
	// this keeps direct callers consistent.
	sources := p.SessionSourceHeaders
	if sources == nil {
		sources = defaultNativeSourceOrder[:]
	}
	// Ordered unique validation set: destination, configured sources,
	// then every remaining reviewed native in baseline order.
	var names []string
	seen := map[string]bool{}
	add := func(n string) {
		if n == "" || seen[n] {
			return
		}
		seen[n] = true
		names = append(names, n)
	}
	add(p.SessionHeader)
	for _, s := range sources {
		add(strings.ToLower(s))
	}
	for _, n := range defaultNativeSourceOrder {
		add(n)
	}
	enabled := map[string]bool{p.SessionHeader: true}
	for _, s := range sources {
		enabled[strings.ToLower(s)] = true
	}
	nominated := connectionNominated(hdr)
	invalid := &protocol.RouterError{Status: 400, Code: "invalid_session_id",
		Msg: "Conversation identity must be 1-256 ASCII characters: letters, digits, '.', '_', ':', '/' or '-' with a leading letter or digit"}
	winner := ""
	found := false
	for _, name := range names {
		if nominated[name] {
			continue
		}
		values := headerValues(hdr, name)
		if len(values) > 1 {
			return Session{}, nil, &protocol.RouterError{Status: 400, Code: "invalid_session_id",
				Msg: "Duplicate conversation identity header"}
		}
		if len(values) == 0 {
			continue
		}
		if !validSessionValue(values[0]) {
			return Session{}, nil, invalid
		}
		if enabled[name] && !found {
			winner = values[0]
			found = true
		}
	}
	if p.SessionPromptCacheKey {
		if raw, ok := promptCacheKey(body); ok {
			var s string
			if err := json.Unmarshal(raw, &s); err != nil || !validSessionValue(s) {
				return Session{}, nil, invalid
			}
			if !found {
				winner = s
				found = true
			}
		}
	}
	if !found && fb.Has {
		if !validSessionValue(fb.ID) {
			return Session{}, nil, invalid
		}
		winner = fb.ID
		found = true
	}
	if !found {
		if p.SessionMissing == "reject" {
			return Session{}, nil, &protocol.RouterError{Status: 400, Code: "missing_session_id",
				Msg: "Conversation identity required on every turn"}
		}
		return Session{}, body, nil
	}
	effective := body
	if translated && p.SessionPromptCacheKey {
		effective = consumePromptCacheKey(body)
	}
	return Session{ID: winner, Has: true}, effective, nil
}
