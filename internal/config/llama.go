// Package config llama support: the only accepted llama discovery table is
// [llama_cpp] with its per-server entries. No persistence, enrichment or
// capability tables are admitted here; those stay startup-rejected by the
// top-level schema.
package config

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/arhammahajan/router/internal/catalog"
)

// Llama discovery defaults mirror the Python baseline: conventional local
// port, 2s whole-probe bound, 30s refresh.
const (
	DefaultLlamaURL            = "http://127.0.0.1:9931"
	DefaultLlamaProbeTimeout   = 2.0
	DefaultLlamaRefreshSeconds = 30.0
)

// LlamaServer is one validated llama.cpp server: the probe URL (trailing
// slash stripped), the final provider name (explicit or generated) and the
// merged global+server headers with the selected credential.
type LlamaServer struct {
	URL     string
	Name    string
	APIKey  string // resolved, raw; stripped at use like providers
	Headers map[string]string
}

// LlamaConfig is the validated [llama_cpp] table. Servers are in
// order-preserving deduped URL order with final unique names.
type LlamaConfig struct {
	Servers        []LlamaServer
	Headers        map[string]string // trusted global headers (probe + generation)
	ProbeTimeout   float64           // seconds, whole-probe bound per server
	RefreshSeconds float64           // >=0; 0 means exactly one startup attempt
	Prefix         string
	ExposeAliases  bool
	Compatibility  string // "strict" | "lossy"
	// ContextFallback is the explicit fallback applied only when observed
	// context_window is unknown. Nil means absent (unknown, never the
	// Python implicit default).
	ContextFallback *int64
	// LimitOverrides matches raw upstream IDs exactly. Nil value is an
	// explicit null withdraw. Nil map means absent or admitted empty.
	LimitOverrides map[string]map[string]*int64
}

// LlamaProviders builds the trusted per-server generated providers from
// config only: no I/O, no payload input. Generation base is url + "/v1"
// unless already suffixed; the probe timeout never applies here (the
// generation default does). Global/server headers are generation headers
// too; the selected server credential wins.
func (c *Config) LlamaProviders() map[string]*Provider {
	out := map[string]*Provider{}
	if c.Llama == nil {
		return out
	}
	for _, s := range c.Llama.Servers {
		base := s.URL
		if !strings.HasSuffix(base, "/v1") {
			base += "/v1"
		}
		headers := make(map[string]string, len(s.Headers))
		for k, v := range s.Headers {
			headers[k] = v
		}
		out[s.Name] = &Provider{
			Name:             s.Name,
			Dialect:          DialectOpenAIChat,
			BaseURL:          base,
			APIKey:           s.APIKey,
			Headers:          headers,
			Timeout:          DefaultProviderTimeout,
			Lossy:            c.Llama.Compatibility == "lossy",
			LitellmProvider:  "openai",
			CompatibilityRaw: c.Llama.Compatibility,
		}
	}
	return out
}

var llamaKeys = map[string]bool{
	"headers": true, "servers": true, "ports": true, "timeout": true,
	"refresh_seconds": true, "context_fallback": true, "limit_overrides": true,
	"prefix": true, "aliases": true, "compatibility": true,
	"api_key": true, "api_key_env": true, "api_key_file": true,
}

var llamaServerKeys = map[string]bool{
	"url": true, "name": true, "headers": true,
	"api_key": true, "api_key_env": true, "api_key_file": true,
}

var llamaGenSanitize = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

// splitServerEnv splits LLAMA_SERVER_URL values on semicolons or commas,
// mirroring the Python fallback chain.
func splitServerEnv(value string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(value, func(r rune) bool {
		return r == ';' || r == ','
	}) {
		if trimmed := catalog.StripModelID(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// normalizeLlamaURL strips whitespace and trailing slashes, then enforces
// the credential-isolation predicate: http(s) with no userinfo, query or
// fragment. Secrets must not hide in a URL that also seeds the generated
// provider name, and base+"/health" concat must not break.
func normalizeLlamaURL(s, where string) (string, error) {
	s = strings.TrimRight(catalog.StripModelID(s), "/")
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		return "", fmt.Errorf("%s: server URL must start with http:// or https://", where)
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("%s: server URL must start with http:// or https://", where)
	}
	if u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return "", fmt.Errorf("%s: server URL must be a bare http(s) origin/path without userinfo, query or fragment", where)
	}
	return s, nil
}

// llamaProviderName chooses a generated name against identities reserved
// before probing: explicit names first, then generated in config order.
func llamaProviderName(rawURL string, taken map[string]bool) string {
	parsed, err := url.Parse(rawURL)
	base := ""
	if err == nil {
		// Python urlsplit preserves escaped path spelling. Go URL.Path
		// decodes it, which would silently rename generated providers.
		// Validated URLs have no userinfo, query or fragment, so the raw
		// authority/path after the scheme is exactly the identity input.
		base = "llama-" + strings.Trim(strings.TrimPrefix(rawURL, parsed.Scheme+"://"), "/")
		base = llamaGenSanitize.ReplaceAllString(base, "-")
		base = strings.Trim(base, "-")
	}
	if base == "" {
		base = "llama"
	}
	name := base
	for i := 2; taken[name]; i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	return name
}

func fillLlama(tab map[string]any, providers map[string]*Provider) (*LlamaConfig, error) {
	const where = "llama_cpp"
	if err := rejectUnknown(tab, llamaKeys, where,
		" (supported: headers, servers, ports, timeout, refresh_seconds, "+
			"prefix, aliases, compatibility, per-server url/name/headers, credentials)"); err != nil {
		return nil, err
	}
	compat := "strict"
	if v, ok := tab["compatibility"]; ok {
		s, ok := asString(v)
		if !ok || (s != "strict" && s != "lossy") {
			return nil, fmt.Errorf("%s: 'compatibility' must be \"strict\" or \"lossy\"", where)
		}
		compat = s
	}
	headers := map[string]string{}
	if v, ok := tab["headers"]; ok {
		hdrs, err := checkHeaders(v, where+".headers", "")
		if err != nil {
			return nil, err
		}
		headers = mergeLegacyHeaders(nil, hdrs)
	}
	globalKey, err := resolveCredential(where, tab)
	if err != nil {
		return nil, err
	}
	probe := DefaultLlamaProbeTimeout
	if v, ok := tab["timeout"]; ok {
		n, err := finitePositive(v, where, "timeout")
		if err != nil {
			return nil, err
		}
		probe = n
	}
	refresh := DefaultLlamaRefreshSeconds
	if v, ok := tab["refresh_seconds"]; ok {
		n, err := finiteNonNegative(v, where, "refresh_seconds")
		if err != nil {
			return nil, err
		}
		refresh = n
	}
	// Explicit limit policy is typed and validated here, startup
	// value-free before any I/O. Absent fallback stays unknown; empty
	// overrides are an exact no-op.
	fallback, overrides, err := parseLlamaLimits(tab)
	if err != nil {
		return nil, err
	}
	prefix := ""
	if v, ok := tab["prefix"]; ok {
		s, ok := asString(v)
		if !ok {
			return nil, fmt.Errorf("%s: 'prefix' must be a string", where)
		}
		prefix = s
	}
	expose := true
	if v, ok := tab["aliases"]; ok {
		b, ok := asBool(v)
		if !ok {
			return nil, fmt.Errorf("%s: 'aliases' must be boolean", where)
		}
		expose = b
	}
	// Ports become loopback URLs appended after servers; both lists accept
	// a bare scalar for the single-server case.
	var raws []any
	if v, ok := tab["servers"]; ok {
		switch {
		case isLlamaScalar(v):
			raws = append(raws, v)
		default:
			arr, ok := asArray(v)
			if !ok {
				return nil, fmt.Errorf("%s: 'servers' must be an array", where)
			}
			raws = append(raws, arr...)
		}
	}
	var ports []any
	if v, ok := tab["ports"]; ok {
		if n, ok := asInt(v); ok {
			ports = append(ports, n)
		} else {
			arr, ok := asArray(v)
			if !ok {
				return nil, fmt.Errorf("%s: 'ports' must be an array of integers", where)
			}
			ports = append(ports, arr...)
		}
	}
	type entry struct {
		url     string
		name    string
		key     string
		headers map[string]string
	}
	var entries []entry
	for i, raw := range raws {
		entryWhere := fmt.Sprintf("%s.servers[%d]", where, i)
		switch node := raw.(type) {
		case string:
			u, err := normalizeLlamaURL(node, entryWhere)
			if err != nil {
				return nil, err
			}
			entries = append(entries, entry{url: u, key: globalKey, headers: copyHeaders(headers)})
		case map[string]any:
			if err := rejectUnknown(node, llamaServerKeys, entryWhere,
				" (supported: url, name, headers, credentials)"); err != nil {
				return nil, err
			}
			rawURL, ok := node["url"]
			if !ok {
				return nil, fmt.Errorf("%s: missing %q", entryWhere, "url")
			}
			urlStr, ok := asString(rawURL)
			if !ok || catalog.StripModelID(urlStr) == "" {
				return nil, fmt.Errorf("%s: 'url' must be a non-empty string", entryWhere)
			}
			u, err := normalizeLlamaURL(urlStr, entryWhere)
			if err != nil {
				return nil, err
			}
			name := ""
			if v, ok := node["name"]; ok {
				s, ok := asString(v)
				if !ok {
					return nil, fmt.Errorf("%s: 'name' must be a string", entryWhere)
				}
				name = catalog.StripModelID(s)
			}
			merged := copyHeaders(headers)
			if v, ok := node["headers"]; ok {
				hdrs, err := checkHeaders(v, entryWhere+".headers", "")
				if err != nil {
					return nil, err
				}
				merged = mergeLegacyHeaders(headers, hdrs)
			}
			_, hasKey := node["api_key"]
			_, hasEnv := node["api_key_env"]
			_, hasFile := node["api_key_file"]
			key := globalKey
			if hasKey || hasEnv || hasFile {
				k, err := resolveCredential(entryWhere, node)
				if err != nil {
					return nil, err
				}
				key = k
			}
			entries = append(entries, entry{url: u, name: name, key: key, headers: merged})
		default:
			return nil, fmt.Errorf("%s: must be a URL string or a table with 'url'", entryWhere)
		}
	}
	for i, raw := range ports {
		n, ok := asInt(raw)
		if !ok || n < 1 || n > 65535 {
			return nil, fmt.Errorf("%s.ports[%d]: port must be an integer between 1 and 65535", where, i)
		}
		entries = append(entries, entry{
			url:     fmt.Sprintf("http://127.0.0.1:%d", n),
			key:     globalKey,
			headers: copyHeaders(headers),
		})
	}
	if len(entries) == 0 {
		for _, u := range splitServerEnv(os.Getenv("LLAMA_SERVER_URL")) {
			entries = append(entries, entry{url: u, key: globalKey, headers: copyHeaders(headers)})
		}
		if len(entries) == 0 {
			entries = append(entries, entry{url: DefaultLlamaURL, key: globalKey, headers: copyHeaders(headers)})
		}
		// Env/default URLs pass through the same hardening predicate.
		for i, e := range entries {
			u, err := normalizeLlamaURL(e.url, fmt.Sprintf("%s.servers[%d]", where, i))
			if err != nil {
				return nil, err
			}
			entries[i].url = u
		}
	}
	// Validate every duplicate server before the ordered URL dedup: name
	// checks run for all entries, then repeats collapse keeping the first.
	taken := map[string]bool{}
	for name := range providers {
		taken[name] = true
	}
	explicitIdx := map[string]int{}
	seen := map[string]bool{}
	var servers []LlamaServer
	for i, e := range entries {
		entryWhere := fmt.Sprintf("%s.servers[%d]", where, i)
		name := e.name
		if name != "" {
			if strings.Contains(name, "/") {
				return nil, fmt.Errorf("%s.name: explicit server names must not contain '/'", entryWhere)
			}
			if prev, dup := explicitIdx[name]; dup {
				return nil, fmt.Errorf("%s.name: duplicate explicit provider name %q; "+
					"already used by llama_cpp.servers[%d]. Assign each server a unique name or omit names to generate them.",
					entryWhere, name, prev)
			}
			if providers[name] != nil {
				return nil, fmt.Errorf("%s.name: provider name %q is already configured "+
					"in providers; choose a unique server name or omit it.", entryWhere, name)
			}
			explicitIdx[name] = i
			taken[name] = true
		}
		if seen[e.url] {
			continue
		}
		seen[e.url] = true
		servers = append(servers, LlamaServer{URL: e.url, Name: name, APIKey: e.key, Headers: e.headers})
	}
	// Generated names reserve explicit + static names in config order.
	for i := range servers {
		if servers[i].Name == "" {
			name := llamaProviderName(servers[i].URL, taken)
			servers[i].Name = name
		}
		taken[servers[i].Name] = true
	}
	return &LlamaConfig{
		Servers:         servers,
		Headers:         headers,
		ProbeTimeout:    probe,
		RefreshSeconds:  refresh,
		Prefix:          prefix,
		ExposeAliases:   expose,
		Compatibility:   compat,
		ContextFallback: fallback,
		LimitOverrides:  overrides,
	}, nil
}

// isLlamaScalar reports the bare single-server spellings: a URL string or
// one inline table.
func isLlamaScalar(v any) bool {
	if _, ok := asString(v); ok {
		return true
	}
	_, ok := asTable(v)
	return ok
}

func copyHeaders(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
