// Package config search tables: opt-in SearXNG backend + per-route binding.
//
// TOML shape (accepted root contract):
//
//	[search.backends.internal]
//	endpoint = "https://search.example.invalid/searxng"
//	count = 5
//	[search.backends.internal.headers]
//	Authorization = "Bearer ..."
//
//	[search.bindings."public-search-model"]
//	tool = "web_search_preview"
//	backend = "internal"
//
// A binding names an enabled configured canonical model whose effective
// dialect is Chat and whose provider allows lossy translation. Aliases
// resolve through the canonical model at request time. Discovery and
// dynamic models have no binding. Unknown keys, backend references, tools
// and ineligible model keys fail validation without I/O.
package config

import (
	"fmt"
	"sort"
	"strings"

	"github.com/arhammahajan/router/internal/searxng"
)

// SearchBackend is one validated SearXNG backend.
type SearchBackend struct {
	Name     string
	Endpoint string
	Count    int
	Headers  map[string]string
}

// SearchBinding binds a canonical public route to a backend.
type SearchBinding struct {
	Model   string // canonical model name
	Tool    string // always "web_search_preview"
	Backend string // backend name
}

// SearchConfig holds the opt-in search tables. Nil means disabled.
type SearchConfig struct {
	Backends map[string]*SearchBackend
	Bindings map[string]*SearchBinding // keyed by canonical model name
}

// SearchBindingFor resolves a canonical model name to its backend.
// Aliases resolve through the canonical model: callers pass m.Name.
func (c *Config) SearchBindingFor(canonical string) (*SearchBinding, *SearchBackend, bool) {
	if c == nil || c.Search == nil {
		return nil, nil, false
	}
	b, ok := c.Search.Bindings[canonical]
	if !ok {
		return nil, nil, false
	}
	be, ok := c.Search.Backends[b.Backend]
	if !ok {
		return nil, nil, false
	}
	return b, be, true
}

var searchTopKeys = map[string]bool{"backends": true, "bindings": true}

func fillSearch(tab map[string]any, providers map[string]*Provider, models []Model) (*SearchConfig, error) {
	if err := rejectUnknown(tab, searchTopKeys, "search",
		" (supported: [search.backends.<name>], [search.bindings.<model>])"); err != nil {
		return nil, err
	}
	out := &SearchConfig{
		Backends: map[string]*SearchBackend{},
		Bindings: map[string]*SearchBinding{},
	}
	if raw, ok := tab["backends"]; ok {
		btab, ok := asTable(raw)
		if !ok {
			return nil, fmt.Errorf("search.backends: must be a table")
		}
		names := make([]string, 0, len(btab))
		for n := range btab {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, name := range names {
			if name == "" || name != strings.TrimSpace(name) || strings.Contains(name, "/") {
				return nil, fmt.Errorf("search.backends: names must be non-empty without whitespace padding or '/'")
			}
			sub, ok := asTable(btab[name])
			if !ok {
				return nil, fmt.Errorf("search.backends.%s: must be a table", name)
			}
			be, err := fillSearchBackend(name, sub)
			if err != nil {
				return nil, err
			}
			out.Backends[name] = be
		}
	}
	if raw, ok := tab["bindings"]; ok {
		btab, ok := asTable(raw)
		if !ok {
			return nil, fmt.Errorf("search.bindings: must be a table")
		}
		// Canonical lookup: key must equal an explicit [[models]] name.
		byCanonical := map[string]*Model{}
		for i := range models {
			m := &models[i]
			if _, dup := byCanonical[m.Name]; !dup {
				byCanonical[m.Name] = m
			}
		}
		names := make([]string, 0, len(btab))
		for n := range btab {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, key := range names {
			sub, ok := asTable(btab[key])
			if !ok {
				return nil, fmt.Errorf("search.bindings.%q: must be a table", key)
			}
			bd, err := fillSearchBinding(key, sub, byCanonical, providers, out.Backends)
			if err != nil {
				return nil, err
			}
			if _, dup := out.Bindings[bd.Model]; dup {
				return nil, fmt.Errorf("search.bindings: duplicate binding for model %q", bd.Model)
			}
			out.Bindings[bd.Model] = bd
		}
	}
	return out, nil
}

var searchBackendKeys = map[string]bool{"endpoint": true, "count": true, "headers": true}
var searchBindingKeys = map[string]bool{"tool": true, "backend": true}

func fillSearchBackend(name string, t map[string]any) (*SearchBackend, error) {
	where := "search.backends." + name
	if err := rejectUnknown(t, searchBackendKeys, where,
		" (supported: endpoint, count, headers)"); err != nil {
		return nil, err
	}
	rawEp, ok := t["endpoint"]
	if !ok {
		return nil, fmt.Errorf("%s.endpoint is required", where)
	}
	ep, ok := asString(rawEp)
	if !ok || strings.TrimSpace(ep) == "" {
		return nil, fmt.Errorf("%s: 'endpoint' must be a non-empty string", where)
	}
	be := &SearchBackend{Name: name, Endpoint: strings.TrimSpace(ep), Count: 5}
	if v, ok := t["count"]; ok {
		n, ok := asInt(v)
		if !ok || n < 1 || n > 10 {
			return nil, fmt.Errorf("%s: 'count' must be an integer between 1 and 10", where)
		}
		be.Count = int(n)
	}
	if v, ok := t["headers"]; ok {
		hdrs, err := checkHeaders(v, where, "")
		if err != nil {
			return nil, err
		}
		// Search headers share the static header boundary plus the
		// SearXNG hop/framing boundary enforced by the client at send
		// time. Reject hop headers here so misconfiguration fails at
		// startup, not per request. Framing (accept, accept-encoding,
		// user-agent, content-type/length) is enforced last at send, so
		// statics carrying those names are also rejected here.
		for name := range hdrs {
			lower := strings.ToLower(name)
			if isSearchForbiddenHeader(lower) {
				return nil, fmt.Errorf("%s: static search headers must not carry transport framing", where)
			}
		}
		if len(hdrs) > 0 {
			be.Headers = hdrs
		}
	}
	if _, err := searxng.NewClient(be.Endpoint, be.Headers); err != nil {
		return nil, fmt.Errorf("%s: %v", where, err)
	}
	return be, nil
}

func isSearchForbiddenHeader(lower string) bool {
	switch lower {
	case "connection", "proxy-connection", "keep-alive", "proxy-authenticate",
		"proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade",
		"host", "content-length", "content-type", "content-encoding",
		"accept", "accept-encoding", "user-agent":
		return true
	}
	return false
}

func fillSearchBinding(key string, t map[string]any, byCanonical map[string]*Model, providers map[string]*Provider, backends map[string]*SearchBackend) (*SearchBinding, error) {
	where := fmt.Sprintf("search.bindings.%q", key)
	if err := rejectUnknown(t, searchBindingKeys, where,
		` (supported: tool = "web_search_preview", backend = "<name>")`); err != nil {
		return nil, err
	}
	m, ok := byCanonical[key]
	if !ok {
		return nil, fmt.Errorf("%s: unknown model %q; bindings name an enabled configured canonical model", where, key)
	}
	if m.Disabled {
		return nil, fmt.Errorf("%s: model %q is disabled", where, key)
	}
	p, ok := providers[m.Provider]
	if !ok {
		return nil, fmt.Errorf("%s: unknown provider for model %q", where, key)
	}
	if m.EffectiveDialect(p) != DialectOpenAIChat {
		return nil, fmt.Errorf("%s: model %q must resolve to an openai-chat route for search substitution", where, key)
	}
	if !p.IsLossy() {
		return nil, fmt.Errorf("%s: model %q requires a lossy provider for approximated search substitution", where, key)
	}
	rawTool, ok := t["tool"]
	if !ok {
		return nil, fmt.Errorf("%s.tool is required", where)
	}
	tool, ok := asString(rawTool)
	if !ok || tool != "web_search_preview" {
		return nil, fmt.Errorf("%s: 'tool' must be \"web_search_preview\"", where)
	}
	rawBe, ok := t["backend"]
	if !ok {
		return nil, fmt.Errorf("%s.backend is required", where)
	}
	beName, ok := asString(rawBe)
	if !ok || strings.TrimSpace(beName) == "" {
		return nil, fmt.Errorf("%s: 'backend' must be a non-empty string", where)
	}
	beName = strings.TrimSpace(beName)
	if _, ok := backends[beName]; !ok {
		return nil, fmt.Errorf("%s: unknown search backend %q", where, beName)
	}
	return &SearchBinding{Model: m.Name, Tool: "web_search_preview", Backend: beName}, nil
}
