package provider

import (
	"fmt"
	"os"
	"strings"

	"github.com/arhammahajan/router/internal/capability"
	"github.com/arhammahajan/router/internal/catalog"
	"github.com/arhammahajan/router/internal/endpoint"
	"github.com/arhammahajan/router/internal/protocol"
)

// LlamaCppPlugin handles [providers.llama_cpp] and llama.cpp runtime behavior.
type LlamaCppPlugin struct{}

var localFleetAllowedKeys = map[string]bool{
	"base_url": true, "api_key": true, "api_key_env": true, "api_key_file": true,
	"auth_header": true, "headers": true, "timeout": true, "compatibility": true,
	"servers": true, "ports": true, "refresh_seconds": true, "probe_timeout": true,
	"prefix": true, "aliases": true, "context_fallback": true, "limit_overrides": true,
}

var localServerEntryKeys = map[string]bool{
	"url": true, "name": true, "headers": true, "auth_header": true, "timeout": true,
	"api_key": true, "api_key_env": true, "api_key_file": true,
}

func (*LlamaCppPlugin) Kind() string { return KindLlamaCpp }

func (*LlamaCppPlugin) ParseConfig(common CommonConfig, raw map[string]any, _ string, env func(string) (string, bool)) (*ParseResult, error) {
	return parseLocalFleet(localFleetOpts{
		kind:          KindLlamaCpp,
		namePrefix:    "llama",
		defaultPrefix: "llama/",
		envFallback:   "LLAMA_SERVER_URL",
	}, common, raw, env)
}

func (*LlamaCppPlugin) AuthHeaders(p *Spec, destDialect string) map[string]string {
	return DefaultAuthHeaders(p, destDialect)
}

func (*LlamaCppPlugin) GenerationEndpoint(baseURL, destDialect string) (string, error) {
	return endpoint.GenerationEndpoint(baseURL, destDialect)
}

func (*LlamaCppPlugin) PrepareNativeBody(destDialect string, body []byte) ([]byte, bool) {
	if destDialect != DialectOpenAIChat {
		return body, false
	}
	return protocol.ApplyLlamaNative(body)
}

func (*LlamaCppPlugin) EffortScope(destDialect string) protocol.RouteScope {
	switch destDialect {
	case DialectOpenAIResponses:
		return protocol.LocalResponsesRoute(capability.RuntimeLlamaCpp)
	case DialectAnthropic:
		return protocol.CompatAnthropicRoute()
	case DialectGemini:
		return protocol.OrdinaryGeminiRoute()
	default:
		return protocol.LocalRoute(capability.RuntimeLlamaCpp)
	}
}

func (*LlamaCppPlugin) RefusesUnobservedReasoning() bool { return false }

func (*LlamaCppPlugin) ApplyCapabilities(c *capability.Contract, wall float64) {
	capability.ApplyLlamaMapper(c, capability.RuntimeLlamaCpp, wall)
}

// --- shared local fleet config parser (used by llama_cpp and strata) ---

type localFleetOpts struct {
	kind          string
	namePrefix    string
	defaultPrefix string
	envFallback   string
}

func parseLocalFleet(opts localFleetOpts, common CommonConfig, raw map[string]any, env func(string) (string, bool)) (*ParseResult, error) {
	if env == nil {
		env = os.LookupEnv
	}
	where := "providers." + common.Name
	if err := RejectUnknown(raw, localFleetAllowedKeys, where,
		" (supported: base_url, headers, auth_header, timeout, probe_timeout, refresh_seconds, "+
			"servers, ports, prefix, aliases, compatibility, context_fallback, limit_overrides, credentials)"); err != nil {
		return nil, err
	}

	compat := "strict"
	if common.CompatibilityRaw != "" {
		compat = common.CompatibilityRaw
	}
	lossy := compat == "lossy"

	headers := MergeLegacyHeaders(nil, common.Headers)
	if headers == nil {
		headers = map[string]string{}
	}

	genTimeout := common.Timeout
	if genTimeout <= 0 {
		genTimeout = DefaultProviderTimeout
	}

	probeTimeout := DefaultLlamaProbeTimeout
	if v, ok := raw["probe_timeout"]; ok {
		n, err := FinitePositive(v, where, "probe_timeout")
		if err != nil {
			return nil, err
		}
		probeTimeout = n
	}

	refresh := DefaultLlamaRefresh
	if v, ok := raw["refresh_seconds"]; ok {
		n, err := FiniteNonNegative(v, where, "refresh_seconds")
		if err != nil {
			return nil, err
		}
		refresh = n
	}

	fallback, overrides, err := ParseLocalLimits(raw, where)
	if err != nil {
		return nil, err
	}

	prefix := opts.defaultPrefix
	if v, ok := raw["prefix"]; ok {
		s, ok := AsString(v)
		if !ok {
			return nil, fmt.Errorf("%s: 'prefix' must be a string", where)
		}
		prefix = s
	}

	expose := false
	if v, ok := raw["aliases"]; ok {
		b, ok := AsBool(v)
		if !ok {
			return nil, fmt.Errorf("%s: 'aliases' must be boolean", where)
		}
		expose = b
	}

	_, hasServers := raw["servers"]
	_, hasPorts := raw["ports"]
	if common.HasBaseURL && (hasServers || hasPorts) {
		return nil, fmt.Errorf("%s: 'base_url' cannot be combined with 'servers' or 'ports'", where)
	}

	var raws []any
	if v, ok := raw["servers"]; ok {
		if isLocalScalar(v) {
			raws = append(raws, v)
		} else {
			arr, ok := AsArray(v)
			if !ok {
				return nil, fmt.Errorf("%s: 'servers' must be an array", where)
			}
			raws = append(raws, arr...)
		}
	}

	var ports []any
	if v, ok := raw["ports"]; ok {
		if n, ok := AsInt(v); ok {
			ports = append(ports, n)
		} else {
			arr, ok := AsArray(v)
			if !ok {
				return nil, fmt.Errorf("%s: 'ports' must be an array of integers", where)
			}
			ports = append(ports, arr...)
		}
	}

	type entry struct {
		probeURL   string
		baseURL    string
		name       string
		key        string
		authHeader string
		timeout    float64
		headers    map[string]string
	}
	var entries []entry

	if common.HasBaseURL {
		probeURL := StripV1Suffix(common.BaseURL)
		entries = append(entries, entry{
			probeURL:   probeURL,
			baseURL:    probeURL + "/v1",
			key:        common.APIKey,
			authHeader: common.AuthHeader,
			timeout:    genTimeout,
			headers:    CopyHeaders(headers),
		})
	}

	for i, item := range raws {
		entryWhere := fmt.Sprintf("%s.servers[%d]", where, i)
		switch node := item.(type) {
		case string:
			u, err := normalizeLocalServerURL(node, entryWhere)
			if err != nil {
				return nil, err
			}
			probeURL := StripV1Suffix(u)
			entries = append(entries, entry{
				probeURL:   probeURL,
				baseURL:    probeURL + "/v1",
				key:        common.APIKey,
				authHeader: common.AuthHeader,
				timeout:    genTimeout,
				headers:    CopyHeaders(headers),
			})
		case map[string]any:
			if err := RejectUnknown(node, localServerEntryKeys, entryWhere,
				" (supported: url, name, headers, auth_header, timeout, credentials)"); err != nil {
				return nil, err
			}
			rawURL, ok := node["url"]
			if !ok {
				return nil, fmt.Errorf("%s: missing %q", entryWhere, "url")
			}
			urlStr, ok := AsString(rawURL)
			if !ok || catalog.StripModelID(urlStr) == "" {
				return nil, fmt.Errorf("%s: 'url' must be a non-empty string", entryWhere)
			}
			u, err := normalizeLocalServerURL(urlStr, entryWhere)
			if err != nil {
				return nil, err
			}
			probeURL := StripV1Suffix(u)
			name := ""
			if v, ok := node["name"]; ok {
				s, ok := AsString(v)
				if !ok {
					return nil, fmt.Errorf("%s: 'name' must be a string", entryWhere)
				}
				name = catalog.StripModelID(s)
			}
			merged := CopyHeaders(headers)
			if v, ok := node["headers"]; ok {
				hdrs, err := CheckHeaders(v, entryWhere+".headers", "")
				if err != nil {
					return nil, err
				}
				merged = MergeLegacyHeaders(headers, hdrs)
			}
			_, hasKey := node["api_key"]
			_, hasEnv := node["api_key_env"]
			_, hasFile := node["api_key_file"]
			key := common.APIKey
			if hasKey || hasEnv || hasFile {
				k, err := ResolveCredential(entryWhere, node, env)
				if err != nil {
					return nil, err
				}
				key = k
			}
			ah := common.AuthHeader
			if parsedAH, hasAH, err := ParseAuthHeader(node, entryWhere); err != nil {
				return nil, err
			} else if hasAH {
				ah = parsedAH
			}
			srvTimeout := genTimeout
			if v, ok := node["timeout"]; ok {
				n, err := FinitePositive(v, entryWhere, "timeout")
				if err != nil {
					return nil, err
				}
				srvTimeout = n
			}
			entries = append(entries, entry{
				probeURL:   probeURL,
				baseURL:    probeURL + "/v1",
				name:       name,
				key:        key,
				authHeader: ah,
				timeout:    srvTimeout,
				headers:    merged,
			})
		default:
			return nil, fmt.Errorf("%s: must be a URL string or a table with 'url'", entryWhere)
		}
	}

	for i, item := range ports {
		n, ok := AsInt(item)
		if !ok || n < 1 || n > 65535 {
			return nil, fmt.Errorf("%s.ports[%d]: port must be an integer between 1 and 65535", where, i)
		}
		probeURL := fmt.Sprintf("http://127.0.0.1:%d", n)
		entries = append(entries, entry{
			probeURL:   probeURL,
			baseURL:    probeURL + "/v1",
			key:        common.APIKey,
			authHeader: common.AuthHeader,
			timeout:    genTimeout,
			headers:    CopyHeaders(headers),
		})
	}

	if len(entries) == 0 {
		if opts.envFallback != "" {
			if envVal, ok := env(opts.envFallback); ok {
				for _, u := range splitServerEnv(envVal) {
					entries = append(entries, entry{
						probeURL:   u,
						key:        common.APIKey,
						authHeader: common.AuthHeader,
						timeout:    genTimeout,
						headers:    CopyHeaders(headers),
					})
				}
			}
		}
		if len(entries) == 0 {
			entries = append(entries, entry{
				probeURL:   DefaultLlamaURL,
				key:        common.APIKey,
				authHeader: common.AuthHeader,
				timeout:    genTimeout,
				headers:    CopyHeaders(headers),
			})
		}
		for i, e := range entries {
			u, err := normalizeLocalServerURL(e.probeURL, fmt.Sprintf("%s.servers[%d]", where, i))
			if err != nil {
				return nil, err
			}
			probeURL := StripV1Suffix(u)
			entries[i].probeURL = probeURL
			entries[i].baseURL = probeURL + "/v1"
		}
	}

	// Validate every entry (including duplicates) before ordered URL dedup.
	taken := map[string]bool{
		KindLlamaCpp: true,
		KindStrata:   true,
	}
	explicitIdx := map[string]int{}
	seenURL := map[string]bool{}
	var servers []LocalServer
	for i, e := range entries {
		entryWhere := fmt.Sprintf("%s.servers[%d]", where, i)
		name := e.name
		explicit := false
		if name != "" {
			if strings.Contains(name, "/") {
				return nil, fmt.Errorf("%s.name: explicit server names must not contain '/'", entryWhere)
			}
			if name == KindLlamaCpp || name == KindStrata {
				return nil, fmt.Errorf("%s.name: provider name %q is reserved; choose a unique server name or omit it", entryWhere, name)
			}
			if prev, dup := explicitIdx[name]; dup {
				return nil, fmt.Errorf("%s.name: duplicate explicit provider name %q; "+
					"already used by %s.servers[%d]. Assign each server a unique name or omit names to generate them.",
					entryWhere, name, where, prev)
			}
			explicitIdx[name] = i
			taken[name] = true
			explicit = true
		}
		if seenURL[e.probeURL] {
			continue
		}
		seenURL[e.probeURL] = true
		servers = append(servers, LocalServer{
			Name:             name,
			ExplicitName:     explicit,
			URL:              e.probeURL,
			BaseURL:          e.baseURL,
			APIKey:           e.key,
			AuthHeader:       e.authHeader,
			Headers:          e.headers,
			Timeout:          e.timeout,
			ProbeTimeout:     probeTimeout,
			RefreshSeconds:   refresh,
			Kind:             opts.kind,
			Prefix:           prefix,
			ExposeAliases:    expose,
			Lossy:            lossy,
			CompatibilityRaw: compat,
			ContextFallback:  CopyInt64Ptr(fallback),
			LimitOverrides:   CloneLimitOverrides(overrides),
		})
	}

	// Assign generated names for unnamed servers.
	for i := range servers {
		if servers[i].Name == "" {
			servers[i].Name = LocalProviderName(opts.namePrefix, servers[i].URL, taken)
		}
		taken[servers[i].Name] = true
	}

	return &ParseResult{
		LocalServers:    servers,
		Headers:         headers,
		RefreshSeconds:  refresh,
		ProbeTimeout:    probeTimeout,
		Prefix:          prefix,
		ExposeAliases:   expose,
		Compatibility:   compat,
		ContextFallback: fallback,
		LimitOverrides:  overrides,
	}, nil
}

func isLocalScalar(v any) bool {
	if _, ok := AsString(v); ok {
		return true
	}
	_, ok := AsTable(v)
	return ok
}
