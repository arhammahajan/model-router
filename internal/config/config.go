// Package config loads and validates the router TOML configuration.
//
// Supported subset (milestones 1-2): [server], [retry], [providers.*],
// [[models]]. Any other table, or any unknown key inside a supported table,
// is a startup error with a migration hint — settings are never silently
// ignored. See SUPPORTED.md (go/README.md) for the documented subset.
//
// Parsing uses github.com/pelletier/go-toml/v2 (TOML v1.1) instead of a
// hand-written parser, so duplicate keys, multiline strings/arrays, dotted
// and quoted table names follow the TOML specification. Validation then
// applies an explicit strict schema over the decoded document: TOML-level
// types are preserved (integers stay integers, floats stay floats), and
// ${ENV_VAR} expansion runs after parsing on string values only, so quotes
// and newlines in values cannot alter the document syntax.
//
// Error messages never carry credential-bearing values or source lines, only
// key paths, variable names and expected shapes.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/arhammahajan/router/internal/capability"
	"github.com/arhammahajan/router/internal/endpoint"
	"github.com/arhammahajan/router/internal/provider"
)

// Dialects spoken by upstreams and ingress endpoints.
const (
	DialectOpenAIChat      = provider.DialectOpenAIChat
	DialectOpenAIResponses = provider.DialectOpenAIResponses
	DialectAnthropic       = provider.DialectAnthropic
	DialectGemini          = provider.DialectGemini
)

// UnauthenticatedUpstreamKey is sent when a provider config has no key,
// using a placeholder to avoid sending an empty credential.
const UnauthenticatedUpstreamKey = provider.UnauthenticatedUpstreamKey

// Defaults mirror config.example.toml / Python defaults.
const (
	DefaultHost              = "127.0.0.1"
	DefaultPort              = 4000
	DefaultMaxBodyBytes      = 64 * 1024 * 1024
	DefaultMaxConnections    = 32
	DefaultMaxGenerations    = 8
	DefaultHeaderReadTimeout = 10.0
	DefaultBodyReadTimeout   = 30.0
	DefaultWriteTimeout      = 30.0
	DefaultProviderTimeout   = provider.DefaultProviderTimeout
	DefaultMaxAttempts       = 1
	DefaultBackoffBase       = 0.5
	DefaultBackoffMax        = 8.0
	DefaultDeadline          = 600.0
)

// maxTimeoutSeconds mirrors Python's threading.TIMEOUT_MAX bound applied to
// every duration/number setting: finite, positive and representable in a Go
// time.Duration (9223372036s * 1e9ns fits in an int64).
const maxTimeoutSeconds = provider.MaxTimeoutSeconds

// maxMetadataBytes bounds one retained [[models]] metadata table once
// rendered as canonical JSON.
const maxMetadataBytes = 64 * 1024

// Server holds the [server] table.
type Server struct {
	Host                string
	Port                int
	AuthMode            string // "none" | "required" (omitted is inferred: keys -> required)
	APIKeys             []string
	AllowDynamicRoutes  bool
	MaxBodyBytes        int64
	MaxConnections      int
	MaxGenerations      int
	HeaderReadTimeout   float64
	BodyReadTimeout     float64
	WriteTimeout        float64
	BearerSessionCompat bool // opt-in bearer-session compatibility (requires zero effective API keys)
}

// Retry holds the [retry] table.
type Retry struct {
	MaxAttempts   int
	Statuses      []int
	ConnectErrors bool
	BackoffBase   float64
	BackoffMax    float64
	Deadline      float64
}

// Provider is an alias for provider.Spec, the unified runtime provider struct.
type Provider = provider.Spec

// Model holds one [[models]] entry.
type Model struct {
	Name     string
	Provider string
	Upstream string // upstream model id ("model" key in TOML)
	Aliases  []string
	// Dialect is the effective egress destination override; "" means inherit
	// the provider default (omitted and explicit-equal are both canonicalized
	// to ""). An explicit different value is one of the four accepted
	// dialects and selects the actual wire protocol.
	Dialect string `json:",omitempty"`
	// Disabled marks a reserved-but-unroutable route (TOML enabled = false).
	// The zero value keeps existing programmatic Model literals enabled.
	Disabled bool
	// Metadata is the retained [[models]] metadata table as canonical JSON
	// (nil when the table is absent or empty). It is kept so a future
	// listing endpoint can serve it; it is never logged or echoed in errors.
	Metadata json.RawMessage
}

// EffectiveDialect returns the actual egress destination: the explicit route
// override when present, otherwise the provider default. Aliases carry the
// same Model, so they inherit the same destination. Catalog/llama/dynamic
// routes use a zero Model and inherit the provider default.
func (m *Model) EffectiveDialect(p *Provider) string {
	if m != nil && m.Dialect != "" {
		return m.Dialect
	}
	if p != nil {
		return p.DefaultDialect
	}
	return ""
}

// Config is the validated router configuration.
type Config struct {
	Server    Server
	Retry     Retry
	Providers map[string]*Provider
	Models    []Model
	// CLIProxyAPI is nil when [cliproxyapi] is absent: no proxy discovery.
	// When present, a single trusted provider named "cliproxyapi" (OpenAI
	// Chat dialect) is generated; proxy /v1/models IDs route as opaque IDs.
	CLIProxyAPI *CLIProxyAPIConfig
	// Catalog is nil when [catalog] is absent: no worker, no overhead.
	Catalog *Catalog
	// Llama is nil when [llama_cpp] is absent: no worker, no overhead.
	// Its generated providers are built on demand via LlamaProviders.
	Llama *LlamaConfig
	// DiscoveryCache is nil when [discovery_cache] is absent: disabled.
	DiscoveryCache *DiscoveryCacheConfig
	// CapabilityOverrides holds sparse validated operator overrides keyed
	// by canonical public route name. Unknown/shadowed names validate
	// without warnings; listing applies by canonical name only. Values are
	// closed typed facts; the snapshot deep-copies this table at build so
	// later config mutation cannot alter pinned snapshots.
	CapabilityOverrides map[string]capability.Overrides
	// Search is nil when [search] is absent: no search vertical, no overhead.
	Search *SearchConfig
	// Accounting is nil when [accounting] is absent: no usage metering.
	Accounting *AccountingConfig
	// modelIndex resolves public name or alias -> model entry.
	modelIndex map[string]*Model
}

// LookupModel resolves a public model name or alias to its route.
func (c *Config) LookupModel(name string) (*Model, *Provider, bool) {
	m, ok := c.modelIndex[name]
	if !ok {
		return nil, nil, false
	}
	p, ok := c.Providers[m.Provider]
	if !ok {
		return nil, nil, false
	}
	return m, p, true
}

var envPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnvValues applies ${ENV_VAR} substitution to every string value in
// the parsed document (keys are never expanded). A bare `$` that is not part
// of `${...}` is left alone. Missing or whitespace-only variables are startup
// errors naming the variable; any leftover `${` after valid substitutions is
// an invalid-substitution error. Values already containing `${` sequences
// (for example from an env value) pass through: only document syntax left
// over from expansion is rejected.
func expandEnvValues(v any, where string) error {
	switch node := v.(type) {
	case map[string]any:
		for key, item := range node {
			switch s := item.(type) {
			case string:
				expanded, err := expandString(s, where+"."+key)
				if err != nil {
					return err
				}
				node[key] = expanded
			default:
				if err := expandEnvValues(item, where+"."+key); err != nil {
					return err
				}
			}
		}
	case []any:
		for i, item := range node {
			if s, ok := item.(string); ok {
				expanded, err := expandString(s, where)
				if err != nil {
					return err
				}
				node[i] = expanded
				continue
			}
			if err := expandEnvValues(item, where); err != nil {
				return err
			}
		}
	}
	return nil
}

func expandString(s, where string) (string, error) {
	if !strings.Contains(s, "${") {
		return s, nil
	}
	if strings.Contains(envPattern.ReplaceAllString(s, ""), "${") {
		return "", fmt.Errorf("%s: invalid environment substitution; use ${ENV_VAR}", where)
	}
	var missing string
	out := envPattern.ReplaceAllStringFunc(s, func(match string) string {
		name := match[2 : len(match)-1]
		val, ok := os.LookupEnv(name)
		if !ok || strings.TrimSpace(val) == "" {
			missing = name
			return match
		}
		return val
	})
	if missing != "" {
		return "", fmt.Errorf("%s: environment variable %q is missing or empty", where, missing)
	}
	return out, nil
}

// --- strict decoding helpers (delegated to internal/provider) ---

func asString(v any) (string, bool)       { return provider.AsString(v) }
func asBool(v any) (bool, bool)           { return provider.AsBool(v) }
func asInt(v any) (int64, bool)           { return provider.AsInt(v) }
func asNum(v any) (float64, bool)         { return provider.AsNum(v) }
func asTable(v any) (map[string]any, bool) { return provider.AsTable(v) }
func asArray(v any) ([]any, bool)         { return provider.AsArray(v) }

func finitePositive(v any, where, field string) (float64, error) {
	return provider.FinitePositive(v, where, field)
}

func positiveInt(v any, where, field string) (int64, error) {
	return provider.PositiveInt(v, where, field)
}

func rejectUnknown(table map[string]any, allowed map[string]bool, where, hint string) error {
	return provider.RejectUnknown(table, allowed, where, hint)
}

func checkHeaders(v any, where string, sessionHeader string) (map[string]string, error) {
	return provider.CheckHeaders(v, where, sessionHeader)
}

func mergeLegacyHeaders(kh, keh map[string]string) map[string]string {
	return provider.MergeLegacyHeaders(kh, keh)
}

// --- top-level build ---

func buildConfig(doc map[string]any, configDir string) (*Config, error) {
	topKeys := map[string]bool{
		"server": true, "retry": true, "providers": true, "models": true,
		"catalog": true, "discovery_cache": true, "capabilities": true,
		"search": true, "accounting": true,
	}
	for k := range doc {
		if !topKeys[k] {
			return nil, fmt.Errorf("unsupported table [%s] in this build (%s)", k, migrationHint(k))
		}
	}
	cfg := &Config{Providers: map[string]*Provider{}}
	// server
	srv := Server{
		Host: DefaultHost, Port: DefaultPort, AuthMode: "",
		AllowDynamicRoutes: true, MaxBodyBytes: DefaultMaxBodyBytes,
		MaxConnections: DefaultMaxConnections, MaxGenerations: DefaultMaxGenerations,
		HeaderReadTimeout: DefaultHeaderReadTimeout, BodyReadTimeout: DefaultBodyReadTimeout,
		WriteTimeout: DefaultWriteTimeout,
	}
	if raw, ok := doc["server"]; ok {
		tab, ok := asTable(raw)
		if !ok {
			return nil, fmt.Errorf("server: must be a table")
		}
		if err := fillServer(&srv, tab); err != nil {
			return nil, err
		}
	}
	cfg.Server = srv
	// retry
	r := Retry{MaxAttempts: DefaultMaxAttempts, Statuses: []int{429, 503},
		ConnectErrors: true, BackoffBase: DefaultBackoffBase,
		BackoffMax: DefaultBackoffMax, Deadline: DefaultDeadline}
	if raw, ok := doc["retry"]; ok {
		tab, ok := asTable(raw)
		if !ok {
			return nil, fmt.Errorf("retry: must be a table")
		}
		if err := fillRetry(&r, tab); err != nil {
			return nil, err
		}
	}
	cfg.Retry = r
	// providers (common parse + plugin dispatch for generic, cliproxyapi, llama_cpp, strata)
	var localFleets []localFleetParse
	if raw, ok := doc["providers"]; ok {
		tab, ok := asTable(raw)
		if !ok {
			return nil, fmt.Errorf("providers: must be a table")
		}
		names := make([]string, 0, len(tab))
		for name := range tab {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if name == "" || name != strings.TrimSpace(name) || strings.Contains(name, "/") {
				return nil, fmt.Errorf("providers: names must be non-empty without whitespace padding or '/'")
			}
			sub, ok := asTable(tab[name])
			if !ok {
				return nil, fmt.Errorf("providers.%s: must be a table", name)
			}
			common, err := provider.ParseCommonConfig(name, sub, os.LookupEnv)
			if err != nil {
				return nil, err
			}
			res, err := provider.ForConfigName(name).ParseConfig(common, sub, configDir, os.LookupEnv)
			if err != nil {
				return nil, err
			}
			if res.Provider != nil {
				cfg.Providers[name] = res.Provider
			}
			if res.CLIProxy != nil {
				cfg.CLIProxyAPI = res.CLIProxy
			}
			if len(res.LocalServers) > 0 {
				localFleets = append(localFleets, localFleetParse{tableName: name, result: res})
			}
		}
	}
	// merge local discovery fleets (llama_cpp / strata) after all static providers are known
	if len(localFleets) > 0 {
		llama, err := mergeLocalFleets(localFleets, cfg.Providers)
		if err != nil {
			return nil, err
		}
		cfg.Llama = llama
	}
	// models
	if raw, ok := doc["models"]; ok {
		arr, ok := asArray(raw)
		if !ok {
			return nil, fmt.Errorf("models: must be an array")
		}
		for i, item := range arr {
			tab, ok := asTable(item)
			if !ok {
				return nil, fmt.Errorf("models[%d]: must be a table", i)
			}
			model, err := fillModel(i, tab, cfg.Providers)
			if err != nil {
				return nil, err
			}
			cfg.Models = append(cfg.Models, *model)
		}
	}
	// catalog (after providers: rule/model_providers providers must be known)
	if raw, ok := doc["catalog"]; ok {
		tab, ok := asTable(raw)
		if !ok {
			return nil, fmt.Errorf("catalog: must be a table")
		}
		cat, err := fillCatalog(tab, cfg.Providers)
		if err != nil {
			return nil, err
		}
		cfg.Catalog = cat
	}
	if cfg.CLIProxyAPI != nil && cfg.Catalog != nil {
		return nil, fmt.Errorf("providers.cliproxyapi and [catalog] cannot be combined in this build; use one discovery source")
	}
	if cfg.CLIProxyAPI != nil {
		var err error
		cfg.Catalog, err = cliProxyCatalog(cfg.CLIProxyAPI, cfg.Providers)
		if err != nil {
			return nil, err
		}
	}
	// discovery_cache (optional; absent means disabled).
	if raw, ok := doc["discovery_cache"]; ok {
		tab, ok := asTable(raw)
		if !ok {
			return nil, fmt.Errorf("discovery_cache: must be a table")
		}
		dc, err := fillDiscoveryCache(tab)
		if err != nil {
			return nil, err
		}
		cfg.DiscoveryCache = dc
	}
	// capabilities (optional; absent means no overrides).
	overrides, err := parseCapabilityOverrides(doc)
	if err != nil {
		return nil, err
	}
	cfg.CapabilityOverrides = overrides
	// search (optional; absent means disabled).
	if raw, ok := doc["search"]; ok {
		tab, ok := asTable(raw)
		if !ok {
			return nil, fmt.Errorf("search: must be a table")
		}
		sc, err := fillSearch(tab, cfg.Providers, cfg.Models)
		if err != nil {
			return nil, err
		}
		cfg.Search = sc
	}
	// accounting (optional; absent means disabled).
	if raw, ok := doc["accounting"]; ok {
		tab, ok := asTable(raw)
		if !ok {
			return nil, fmt.Errorf("accounting: must be a table")
		}
		ac, err := fillAccounting(tab)
		if err != nil {
			return nil, err
		}
		cfg.Accounting = ac
	}
	cfg.modelIndex = map[string]*Model{}
	for i := range cfg.Models {
		m := &cfg.Models[i]
		if _, dup := cfg.modelIndex[m.Name]; dup {
			return nil, fmt.Errorf("models: duplicate model name or alias %q", m.Name)
		}
		cfg.modelIndex[m.Name] = m
		for _, a := range m.Aliases {
			if _, dup := cfg.modelIndex[a]; dup {
				return nil, fmt.Errorf("models: duplicate model name or alias %q", a)
			}
			cfg.modelIndex[a] = m
		}
	}
	if err := validateRouteDialectAliases(doc, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func migrationHint(table string) string {
	switch table {
	case "cliproxyapi":
		return "move [cliproxyapi] to [providers.cliproxyapi]"
	case "llama_cpp":
		return "move [llama_cpp] to [providers.llama_cpp]"
	default:
		return "supported: [server], [retry], [providers.<name>], [[models]], [catalog]"
	}
}

// --- [server] ---

var serverKeys = map[string]bool{
	"host": true, "port": true, "auth_mode": true, "api_keys": true,
	"max_body_bytes": true, "max_connections": true, "max_generations": true,
	"header_read_timeout": true, "body_read_timeout": true, "write_timeout": true,
	"allow_dynamic_routes": true, "bearer_session_compat": true,
}

func fillServer(srv *Server, t map[string]any) error {
	if err := rejectUnknown(t, serverKeys, "server",
		" (supported: host, port, auth_mode, api_keys, allow_dynamic_routes, "+
			"max_body_bytes, max_connections, max_generations, header/body/write timeouts, "+
			"bearer_session_compat)"); err != nil {
		return err
	}
	if v, ok := t["host"]; ok {
		s, ok := asString(v)
		if !ok || strings.TrimSpace(s) == "" {
			return fmt.Errorf("server: 'host' must be a non-empty string")
		}
		srv.Host = strings.TrimSpace(s)
	}
	if v, ok := t["port"]; ok {
		n, ok := asInt(v)
		if !ok || n < 1 || n > 65535 {
			return fmt.Errorf("server.port must be an integer between 1 and 65535")
		}
		srv.Port = int(n)
	}
	if v, ok := t["auth_mode"]; ok {
		s, ok := asString(v)
		if !ok || (s != "none" && s != "required") {
			return fmt.Errorf("server.auth_mode must be 'none' or 'required'")
		}
		srv.AuthMode = s
	}
	var configured []string
	if v, ok := t["api_keys"]; ok {
		arr, ok := asArray(v)
		if !ok {
			return fmt.Errorf("server.api_keys: must be an array of non-empty strings")
		}
		for _, e := range arr {
			s, ok := asString(e)
			if !ok || strings.TrimSpace(s) == "" {
				return fmt.Errorf("server.api_keys: must be an array of non-empty strings")
			}
			configured = append(configured, strings.TrimSpace(s))
		}
	}
	// Explicit auth_mode = "none" forbids ambient client keys; otherwise the
	// ROUTER_API_KEY(S) are canonical; MODEL_ROUTER_API_KEY(S) remain legacy aliases.
	if srv.AuthMode == "none" && len(configured) > 0 {
		return fmt.Errorf("server.auth_mode='none' conflicts with server.api_keys")
	}
	keys := append([]string{}, configured...)
	if srv.AuthMode != "none" {
		envKeys := os.Getenv("ROUTER_API_KEYS")
		if envKeys == "" {
			envKeys = os.Getenv("MODEL_ROUTER_API_KEYS")
		}
		if env := envKeys; env != "" {
			for _, part := range strings.Split(env, ",") {
				if strings.TrimSpace(part) != "" {
					keys = append(keys, strings.TrimSpace(part))
				}
			}
		}
		envKey := strings.TrimSpace(os.Getenv("ROUTER_API_KEY"))
		if envKey == "" {
			envKey = strings.TrimSpace(os.Getenv("MODEL_ROUTER_API_KEY"))
		}
		if env := envKey; env != "" {
			keys = append(keys, env)
		}
	}
	// Order-preserving dedup.
	seen := map[string]bool{}
	deduped := keys[:0]
	for _, k := range keys {
		if !seen[k] {
			seen[k] = true
			deduped = append(deduped, k)
		}
	}
	srv.APIKeys = deduped
	if srv.AuthMode == "" {
		// Python inference: an unset auth_mode allows keys when configured
		// and stays open otherwise.
		if len(srv.APIKeys) > 0 {
			srv.AuthMode = "required"
		} else {
			srv.AuthMode = "none"
		}
	} else if srv.AuthMode == "required" && len(srv.APIKeys) == 0 {
		return fmt.Errorf("server.auth_mode='required' requires at least one non-empty API key")
	}
	if v, ok := t["max_body_bytes"]; ok {
		n, err := positiveInt(v, "server", "max_body_bytes")
		if err != nil {
			return err
		}
		srv.MaxBodyBytes = n
	}
	if v, ok := t["max_connections"]; ok {
		n, err := positiveInt(v, "server", "max_connections")
		if err != nil {
			return err
		}
		srv.MaxConnections = int(n)
	}
	if v, ok := t["max_generations"]; ok {
		n, err := positiveInt(v, "server", "max_generations")
		if err != nil {
			return err
		}
		srv.MaxGenerations = int(n)
	}
	if srv.MaxGenerations > srv.MaxConnections {
		return fmt.Errorf("server.max_generations must not exceed max_connections")
	}
	for _, f := range []struct {
		k string
		d *float64
	}{{"header_read_timeout", &srv.HeaderReadTimeout}, {"body_read_timeout", &srv.BodyReadTimeout}, {"write_timeout", &srv.WriteTimeout}} {
		if v, ok := t[f.k]; ok {
			n, err := finitePositive(v, "server", f.k)
			if err != nil {
				return err
			}
			*f.d = n
		}
	}
	if v, ok := t["allow_dynamic_routes"]; ok {
		b, ok := asBool(v)
		if !ok {
			return fmt.Errorf("server.allow_dynamic_routes must be boolean")
		}
		srv.AllowDynamicRoutes = b
	}
	if v, ok := t["bearer_session_compat"]; ok {
		b, ok := asBool(v)
		if !ok {
			return fmt.Errorf("server.bearer_session_compat must be boolean")
		}
		if b && len(srv.APIKeys) > 0 {
			return fmt.Errorf("server.bearer_session_compat requires unauthenticated client access")
		}
		srv.BearerSessionCompat = b
	}
	return nil
}

// --- [retry] ---

var retryKeys = map[string]bool{
	"max_attempts": true, "statuses": true, "connect_errors": true,
	"backoff_base": true, "backoff_max": true, "deadline": true,
}

func fillRetry(r *Retry, t map[string]any) error {
	if err := rejectUnknown(t, retryKeys, "retry",
		" (supported: max_attempts, statuses, connect_errors, backoff_base, backoff_max, deadline)"); err != nil {
		return err
	}
	if v, ok := t["max_attempts"]; ok {
		n, ok := asInt(v)
		if !ok || n < 1 || n > 20 {
			return fmt.Errorf("retry.max_attempts must be an integer between 1 and 20")
		}
		r.MaxAttempts = int(n)
	}
	if v, ok := t["statuses"]; ok {
		arr, ok := asArray(v)
		if !ok {
			return fmt.Errorf("retry.statuses: must be an array of status codes")
		}
		r.Statuses = nil
		for _, e := range arr {
			n, ok := asInt(e)
			if !ok || (n != 429 && (n < 500 || n > 599)) {
				return fmt.Errorf("retry.statuses: only 429 and 5xx are configurable")
			}
			r.Statuses = append(r.Statuses, int(n))
		}
	}
	if v, ok := t["connect_errors"]; ok {
		b, ok := asBool(v)
		if !ok {
			return fmt.Errorf("retry.connect_errors must be boolean")
		}
		r.ConnectErrors = b
	}
	if v, ok := t["backoff_base"]; ok {
		n, err := finitePositive(v, "retry", "backoff_base")
		if err != nil {
			return err
		}
		r.BackoffBase = n
	}
	if v, ok := t["backoff_max"]; ok {
		n, err := finitePositive(v, "retry", "backoff_max")
		if err != nil {
			return err
		}
		r.BackoffMax = n
	}
	if r.BackoffBase > r.BackoffMax {
		return fmt.Errorf("retry.backoff_base must not exceed retry.backoff_max")
	}
	if v, ok := t["deadline"]; ok {
		n, err := finitePositive(v, "retry", "deadline")
		if err != nil {
			return err
		}
		r.Deadline = n
	}
	return nil
}

// --- route dialect alias validation ---

// validateRouteDialectAliases extends the redundant-alias rule to overrides:
// a full alias base that is default-equivalent but NOT equivalent for some
// effective configured route destination is a startup error. It validates the
// provider default, any supported dialects, plus every effective configured
// route dialect (including disabled routes) using temporary raw validation
// inputs; no second runtime endpoint is stored.
func validateRouteDialectAliases(doc map[string]any, cfg *Config) error {
	rawProviders, ok := doc["providers"]
	if !ok {
		return nil
	}
	provTab, ok := asTable(rawProviders)
	if !ok {
		return nil
	}
	for name, p := range cfg.Providers {
		sub, ok := provTab[name]
		if !ok {
			continue
		}
		subTab, ok := asTable(sub)
		if !ok {
			continue
		}
		rawKwargs, ok := subTab["litellm_kwargs"]
		if !ok {
			continue
		}
		kwargsTab, ok := asTable(rawKwargs)
		if !ok {
			continue
		}
		rawAlias, ok := kwargsTab["api_base"]
		if !ok {
			continue
		}
		s, ok := asString(rawAlias)
		if !ok || strings.TrimSpace(s) == "" {
			continue // already rejected during provider parsing
		}
		aliasNormalized, err := provider.NormalizeBaseURL(strings.TrimSpace(s))
		if err != nil {
			continue // already rejected during provider parsing
		}
		dests := map[string]bool{p.DefaultDialect: true}
		for _, d := range p.SupportedDialects {
			if d != "" {
				dests[d] = true
			}
		}
		for i := range cfg.Models {
			m := &cfg.Models[i]
			if m.Provider != name {
				continue
			}
			dests[m.EffectiveDialect(p)] = true
		}
		for dest := range dests {
			want, err1 := endpoint.GenerationEndpoint(p.BaseURL, dest)
			got, err2 := endpoint.GenerationEndpoint(aliasNormalized, dest)
			if err1 != nil || err2 != nil || want != got {
				where := "providers." + name
				return fmt.Errorf("%s.litellm_kwargs.api_base: redundant alias must match base_url for every effective route destination; remove the alias or make it equivalent for all actual destinations", where)
			}
		}
	}
	return nil
}

// resolveCredential delegates to provider.ResolveCredential using os.LookupEnv.
func resolveCredential(where string, keys map[string]any) (string, error) {
	return provider.ResolveCredential(where, keys, os.LookupEnv)
}

// --- [[models]] ---

var modelKeys = map[string]bool{
	"name": true, "provider": true, "model": true, "dialect": true,
	"metadata": true, "enabled": true, "aliases": true,
}

func fillModel(i int, m map[string]any, providers map[string]*Provider) (*Model, error) {
	where := fmt.Sprintf("models[%d]", i)
	for k := range m {
		if !modelKeys[k] {
			return nil, fmt.Errorf("%s: unsupported key %q in this build "+
				"(supported: name, provider, model, aliases, metadata)", where, k)
		}
	}
	get := func(k string) (string, error) {
		v, ok := m[k]
		if !ok {
			return "", fmt.Errorf("%s: missing %q", where, k)
		}
		s, ok := asString(v)
		if !ok || strings.TrimSpace(s) == "" {
			return "", fmt.Errorf("%s: %q must be a non-empty string", where, k)
		}
		return strings.TrimSpace(s), nil
	}
	name, err := get("name")
	if err != nil {
		return nil, err
	}
	provider, err := get("provider")
	if err != nil {
		return nil, err
	}
	if _, ok := providers[provider]; !ok {
		names := make([]string, 0, len(providers))
		for n := range providers {
			names = append(names, n)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("%s: unknown provider %q; known providers: %s", where, provider, strings.Join(names, ", "))
	}
	model := &Model{Name: name, Provider: provider}
	if v, ok := m["model"]; ok {
		upstream, ok := asString(v)
		if !ok || strings.TrimSpace(upstream) == "" {
			return nil, fmt.Errorf("%s: 'model' must be a non-empty string", where)
		}
		model.Upstream = strings.TrimSpace(upstream)
	} else if strings.HasPrefix(name, provider+"/") && strings.Contains(name, "/") {
		model.Upstream = strings.SplitN(name, "/", 2)[1]
	} else {
		model.Upstream = name
	}
	if v, ok := m["dialect"]; ok {
		s, ok := asString(v)
		if !ok {
			return nil, fmt.Errorf("models[%d].dialect: 'dialect' must be a string", i)
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, fmt.Errorf("models[%d].dialect: 'dialect' must be one of openai-chat, openai-responses, anthropic, gemini", i)
		}
		if s != providers[provider].DefaultDialect {
			switch s {
			case DialectOpenAIChat, DialectOpenAIResponses, DialectAnthropic, DialectGemini:
				model.Dialect = s
			default:
				return nil, fmt.Errorf("models[%d].dialect: 'dialect' must be one of openai-chat, openai-responses, anthropic, gemini", i)
			}
		}
		// An explicit dialect equal to the provider's is a harmless no-op:
		// canonicalize to "" so omitted == equal in snapshots and digests.
	}
	if v, ok := m["enabled"]; ok {
		b, ok := asBool(v)
		if !ok {
			return nil, fmt.Errorf("%s: 'enabled' must be boolean", where)
		}
		model.Disabled = !b
	}
	if v, ok := m["aliases"]; ok {
		arr, ok := asArray(v)
		if !ok {
			return nil, fmt.Errorf("%s: 'aliases' must be an array of strings", where)
		}
		for _, e := range arr {
			s, ok := asString(e)
			if !ok || strings.TrimSpace(s) == "" {
				return nil, fmt.Errorf("%s: 'aliases' must be an array of strings", where)
			}
			model.Aliases = append(model.Aliases, strings.TrimSpace(s))
		}
	}
	if v, ok := m["metadata"]; ok {
		if v == nil {
			return model, nil
		}
		tab, ok := asTable(v)
		if !ok {
			return nil, fmt.Errorf("%s: 'metadata' must be a table", where)
		}
		if len(tab) > 0 {
			canonical, err := json.Marshal(tab)
			if err != nil {
				return nil, fmt.Errorf("%s: 'metadata' must be JSON-representable", where)
			}
			if len(canonical) > maxMetadataBytes {
				return nil, fmt.Errorf("%s: 'metadata' exceeds the size bound", where)
			}
			model.Metadata = json.RawMessage(canonical)
		}
	}
	return model, nil
}
