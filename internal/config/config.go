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
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/arhammahajan/model-router/internal/capability"
	"github.com/arhammahajan/model-router/internal/endpoint"
)

// Dialects spoken by upstreams and ingress endpoints.
const (
	DialectOpenAIChat      = "openai-chat"
	DialectOpenAIResponses = "openai-responses"
	DialectAnthropic       = "anthropic"
	DialectGemini          = "gemini"
)

// UnauthenticatedUpstreamKey is sent when a provider config has no key,
// mirroring the Python router's "model-router-no-auth" placeholder.
const UnauthenticatedUpstreamKey = "model-router-no-auth"

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
	DefaultProviderTimeout   = 600.0
	DefaultMaxAttempts       = 1
	DefaultBackoffBase       = 0.5
	DefaultBackoffMax        = 8.0
	DefaultDeadline          = 600.0
)

// maxTimeoutSeconds mirrors Python's threading.TIMEOUT_MAX bound applied to
// every duration/number setting: finite, positive and representable in a Go
// time.Duration (9223372036s * 1e9ns fits in an int64).
const maxTimeoutSeconds = 9223372036.0

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

// Provider holds one [providers.<name>] table.
type Provider struct {
	Name            string
	Dialect         string
	BaseURL         string
	APIKey          string // resolved, stripped; "" means unauthenticated
	Headers         map[string]string
	Timeout         float64
	Lossy           bool // compatibility = "lossy" (or legacy drop_params = true)
	AuthHeader      string
	CountTokens     *bool
	LitellmProvider string // validated default adapter spelling (openai/anthropic/gemini)
	// TranslatedHeaders is the merged legacy litellm_kwargs.headers <
	// extra_headers subset, applied to translated traffic only. Nil when
	// unconfigured; programmatic literals keep nil and prior behavior.
	TranslatedHeaders map[string]string
	SessionHeader     string // dedicated upstream identity header ("": affinity off)
	// Session affinity routing: SessionHeader plus an ordered subset of the
	// reviewed native source headers, an optional prompt_cache_key fallback
	// and omit|reject missing policy. Nil SessionSourceHeaders with a header
	// set means the baseline default order (all seven reviewed natives);
	// an explicit [] disables native selection (validation still applies).
	SessionSourceHeaders  []string
	SessionPromptCacheKey bool
	SessionMissing        string
	CompatibilityRaw      string
}

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
		return p.Dialect
	}
	return ""
}

// Config is the validated router configuration.
type Config struct {
	Server    Server
	Retry     Retry
	Providers map[string]*Provider
	Models    []Model
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

// EffectiveKey returns the stripped key or the unauthenticated placeholder.
func (p *Provider) EffectiveKey() string {
	if strings.TrimSpace(p.APIKey) == "" {
		return UnauthenticatedUpstreamKey
	}
	return strings.TrimSpace(p.APIKey)
}

// IsLossy reports whether the provider opts into diagnosed approximation.
func (p *Provider) IsLossy() bool { return p.Lossy }

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

// --- strict decoding helpers over the decoded document ---
//
// go-toml decodes integers as int64, floats as float64, booleans as bool,
// strings as string, arrays as []any and tables as map[string]any. The
// helpers below keep those types strict: a float never satisfies an integer
// field and a bool never satisfies a numeric field.

func asString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

func asBool(v any) (bool, bool) {
	b, ok := v.(bool)
	return b, ok
}

func asInt(v any) (int64, bool) {
	n, ok := v.(int64)
	return n, ok
}

func asNum(v any) (float64, bool) {
	switch n := v.(type) {
	case int64:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

func asTable(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

func asArray(v any) ([]any, bool) {
	a, ok := v.([]any)
	return a, ok
}

// finitePositive mirrors Python's _config_number: finite, > 0 and
// <= threading.TIMEOUT_MAX (9223372036.0), representable in time.Duration.
func finitePositive(v any, where, field string) (float64, error) {
	n, ok := asNum(v)
	if !ok || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, fmt.Errorf("%s: '%s' must be a finite number", where, field)
	}
	if n <= 0 || n > maxTimeoutSeconds {
		return 0, fmt.Errorf("%s: '%s' must be a positive number <= %v", where, field, maxTimeoutSeconds)
	}
	return n, nil
}

func positiveInt(v any, where, field string) (int64, error) {
	n, ok := asInt(v)
	if !ok || n <= 0 {
		return 0, fmt.Errorf("%s: '%s' must be a positive integer", where, field)
	}
	return n, nil
}

// rejectUnknown fails on keys outside the supported schema. tableName is a
// static path (never a value), so messages stay value-free.
func rejectUnknown(table map[string]any, allowed map[string]bool, where, hint string) error {
	keys := make([]string, 0, len(table))
	for k := range table {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !allowed[k] {
			if sub, ok := table[k].(map[string]any); ok && len(sub) > 0 {
				_ = sub
				return fmt.Errorf("%s.%s: nested tables are not supported in this build%s", where, k, hint)
			}
			return fmt.Errorf("%s.%s: unsupported setting in this build%s", where, k, hint)
		}
	}
	return nil
}

var httpToken = regexp.MustCompile(`^[!#$%&'*+.^_` + "`" + `|~0-9A-Za-z-]+$`)

// checkHeaders enforces HTTP token names and printable-ASCII values, plus
// the Python baseline's ban on static session headers.
func checkHeaders(v any, where string, sessionHeader string) (map[string]string, error) {
	tab, ok := asTable(v)
	if !ok {
		return nil, fmt.Errorf("%s: 'headers' must be a table", where)
	}
	out := map[string]string{}
	names := make([]string, 0, len(tab))
	for k := range tab {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		s, ok := asString(tab[k])
		if !ok || !httpToken.MatchString(k) || !printableASCII(s) {
			return nil, fmt.Errorf("%s: 'headers' must contain HTTP header names and string values", where)
		}
		if isReservedSessionHeader(k, sessionHeader) {
			return nil, fmt.Errorf("%s: static session headers are forbidden; supply a conversation identity on each request", where)
		}
		out[k] = s
	}
	return out, nil
}

func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 32 || s[i] > 126 {
			return false
		}
	}
	return true
}

// nativeSessionHeaders mirrors Python's _NATIVE_SESSION_HEADERS plus the
// router-owned session token: static configured headers may never carry
// conversation identity.
func isReservedSessionHeader(name, sessionHeader string) bool {
	lower := strings.ToLower(name)
	switch lower {
	case "x-claude-code-session-id", "session-id", "session_id", "thread-id",
		"conversation_id", "x-session-id", "x-mavis-session-id", "x-opencode-session":
		return true
	}
	return sessionHeader != "" && lower == strings.ToLower(sessionHeader)
}

// --- top-level build ---

func buildConfig(doc map[string]any) (*Config, error) {
	topKeys := map[string]bool{"server": true, "retry": true, "providers": true, "models": true, "catalog": true, "llama_cpp": true, "discovery_cache": true, "capabilities": true, "search": true, "accounting": true}
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
	// providers
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
			prov, err := fillProvider(name, sub)
			if err != nil {
				return nil, err
			}
			cfg.Providers[name] = prov
		}
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
	// llama (after providers: explicit server names must not collide)
	if raw, ok := doc["llama_cpp"]; ok {
		tab, ok := asTable(raw)
		if !ok {
			return nil, fmt.Errorf("llama_cpp: must be a table")
		}
		llama, err := fillLlama(tab, cfg.Providers)
		if err != nil {
			return nil, err
		}
		cfg.Llama = llama
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
	return "supported: [server], [retry], [providers.<name>], [[models]]"
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
	// MODEL_ROUTER_API_KEY(S) environment fallback mirrors Python behavior.
	if srv.AuthMode == "none" && len(configured) > 0 {
		return fmt.Errorf("server.auth_mode='none' conflicts with server.api_keys")
	}
	keys := append([]string{}, configured...)
	if srv.AuthMode != "none" {
		if env := os.Getenv("MODEL_ROUTER_API_KEYS"); env != "" {
			for _, part := range strings.Split(env, ",") {
				if strings.TrimSpace(part) != "" {
					keys = append(keys, strings.TrimSpace(part))
				}
			}
		}
		if env := strings.TrimSpace(os.Getenv("MODEL_ROUTER_API_KEY")); env != "" {
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

// --- [providers.*] ---

var providerKeys = map[string]bool{
	"dialect": true, "base_url": true, "api_key": true, "api_key_env": true,
	"api_key_file": true, "headers": true, "timeout": true, "compatibility": true,
	"drop_params": true, "auth_header": true, "count_tokens": true,
	"litellm_provider": true, "litellm_kwargs": true,
	"session_header": true, "session_source_headers": true,
	"session_prompt_cache_key": true, "session_missing": true,
}

var nativeSessionSources = map[string]bool{
	"x-claude-code-session-id": true, "session-id": true, "session_id": true,
	"thread-id": true, "conversation_id": true, "x-session-id": true,
	"x-mavis-session-id": true,
}

func fillProvider(name string, t map[string]any) (*Provider, error) {
	where := "providers." + name
	if err := rejectUnknown(t, providerKeys, where,
		" (Supported litellm_kwargs are headers/extra_headers and equivalent api_base/timeout aliases; "+
			"use compatibility/timeout/headers/auth_header for other supported controls. "+
			"Discovery tables beyond [catalog] are later milestones)"); err != nil {
		return nil, err
	}
	p := &Provider{Name: name, Headers: map[string]string{}, Timeout: DefaultProviderTimeout}
	v, ok := t["dialect"]
	if !ok {
		return nil, fmt.Errorf("%s.dialect is required", where)
	}
	s, ok := asString(v)
	if !ok {
		return nil, fmt.Errorf("%s: 'dialect' must be one of openai-chat, openai-responses, anthropic, gemini", where)
	}
	switch s {
	case DialectOpenAIChat, DialectOpenAIResponses, DialectAnthropic, DialectGemini:
		p.Dialect = s
	default:
		return nil, fmt.Errorf("%s: 'dialect' must be one of openai-chat, openai-responses, anthropic, gemini", where)
	}
	v, ok = t["base_url"]
	if !ok {
		return nil, fmt.Errorf("%s.base_url is required", where)
	}
	base, ok := asString(v)
	if !ok || strings.TrimSpace(base) == "" {
		return nil, fmt.Errorf("%s: 'base_url' must be a non-empty string", where)
	}
	normalized, err := normalizeBaseURL(strings.TrimSpace(base))
	if err != nil {
		return nil, fmt.Errorf("%s: %v", where, err)
	}
	p.BaseURL = normalized
	key, err := resolveCredential(where, t)
	if err != nil {
		return nil, err
	}
	p.APIKey = key
	if v, ok := t["headers"]; ok {
		hdrs, err := checkHeaders(v, where, "")
		if err != nil {
			return nil, err
		}
		p.Headers = hdrs
	}
	if v, ok := t["timeout"]; ok {
		n, err := finitePositive(v, where, "timeout")
		if err != nil {
			return nil, err
		}
		p.Timeout = n
	}
	dropPresent := false
	var drop bool
	if v, ok := t["drop_params"]; ok {
		b, ok := asBool(v)
		if !ok {
			return nil, fmt.Errorf("%s: 'drop_params' must be boolean", where)
		}
		dropPresent, drop = true, b
	}
	if v, ok := t["compatibility"]; ok {
		s, ok := asString(v)
		if !ok || (s != "strict" && s != "lossy") {
			return nil, fmt.Errorf("%s: 'compatibility' must be \"strict\" or \"lossy\"", where)
		}
		if dropPresent && drop != (s == "lossy") {
			return nil, fmt.Errorf("%s: 'drop_params' must be boolean and agree with 'compatibility'", where)
		}
		p.CompatibilityRaw = s
		p.Lossy = s == "lossy"
	} else if drop {
		// Legacy LiteLLM-era spelling selects the diagnosed lossy policy.
		p.CompatibilityRaw = "lossy"
		p.Lossy = true
	}
	if v, ok := t["auth_header"]; ok {
		s, ok := asString(v)
		if !ok {
			return nil, fmt.Errorf("%s: 'auth_header' must be a string", where)
		}
		s = strings.ToLower(strings.TrimSpace(s))
		if s != "" && s != "bearer" && s != "x-api-key" && s != "api-key" {
			return nil, fmt.Errorf("%s: 'auth_header' must be \"bearer\", \"x-api-key\", \"api-key\" or empty", where)
		}
		p.AuthHeader = s
	}
	if v, ok := t["count_tokens"]; ok {
		b, ok := asBool(v)
		if !ok {
			return nil, fmt.Errorf("%s: 'count_tokens' must be boolean", where)
		}
		if b && p.Dialect != DialectAnthropic {
			return nil, fmt.Errorf("%s: 'count_tokens' can only be enabled for the anthropic dialect", where)
		}
		p.CountTokens = &b
	}
	if err := fillLitellmProvider(p, t, where); err != nil {
		return nil, err
	}
	if err := fillLitellmKwargs(p, t, where); err != nil {
		return nil, err
	}
	if err := fillSessionPolicy(p, t, where); err != nil {
		return nil, err
	}
	return p, nil
}

// defaultLitellmProvider mirrors the Python baseline dialect defaults
// (config.py default_litellm_provider): openai-chat/openai-responses ->
// "openai", anthropic -> "anthropic", gemini -> "gemini".
func defaultLitellmProvider(dialect string) string {
	switch dialect {
	case DialectAnthropic:
		return "anthropic"
	case DialectGemini:
		return "gemini"
	default:
		return "openai"
	}
}

func fillLitellmProvider(p *Provider, t map[string]any, where string) error {
	want := defaultLitellmProvider(p.Dialect)
	v, ok := t["litellm_provider"]
	if !ok {
		p.LitellmProvider = want
		return nil
	}
	s, ok := asString(v)
	if !ok || s != want {
		return fmt.Errorf("%s.litellm_provider: unsupported in this build; use a native route instead", where)
	}
	p.LitellmProvider = s
	return nil
}

var litellmKwargsAllowed = map[string]bool{"headers": true, "extra_headers": true, "api_base": true, "timeout": true}

func litellmKwargsHint(where, key string) string {
	path := where + ".litellm_kwargs." + key
	switch key {
	case "api_key", "api_key_env", "api_key_file":
		return fmt.Sprintf("%s: unsupported in this build; use api_key/api_key_env/api_key_file instead", path)
	case "custom_llm_provider":
		return fmt.Sprintf("%s: unsupported in this build; use a native route instead", path)
	case "num_retries", "max_retries", "retry_policy", "fallbacks", "context_window_fallback_dict":
		return fmt.Sprintf("%s: unsupported in this build; use [retry] instead", path)
	case "additional_drop_params", "drop_params":
		return fmt.Sprintf("%s: unsupported in this build; use compatibility instead", path)
	case "_skip_mcp_handler":
		return fmt.Sprintf("%s: unsupported in this build; router never executes tools", path)
	default:
		return fmt.Sprintf("%s: unsupported in this build (supported: headers, extra_headers, equivalent api_base/timeout aliases)", path)
	}
}

func fillLitellmKwargs(p *Provider, t map[string]any, where string) error {
	v, ok := t["litellm_kwargs"]
	if !ok || v == nil {
		return nil
	}
	tab, ok := asTable(v)
	if !ok {
		return fmt.Errorf("%s.litellm_kwargs: must be a table", where)
	}
	keys := make([]string, 0, len(tab))
	for k := range tab {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !litellmKwargsAllowed[k] {
			return fmt.Errorf("%s", litellmKwargsHint(where, k))
		}
	}
	if raw, ok := tab["api_base"]; ok {
		if err := checkLegacyAPIBase(p, raw, where); err != nil {
			return err
		}
	}
	if raw, ok := tab["timeout"]; ok {
		if err := checkLegacyTimeout(p, raw, where); err != nil {
			return err
		}
	}
	var kh, keh map[string]string
	if raw, ok := tab["headers"]; ok {
		m, err := checkHeaders(raw, where+".litellm_kwargs.headers", "")
		if err != nil {
			return err
		}
		kh = m
	}
	if raw, ok := tab["extra_headers"]; ok {
		m, err := checkHeaders(raw, where+".litellm_kwargs.extra_headers", "")
		if err != nil {
			return err
		}
		keh = m
	}
	merged := mergeLegacyHeaders(kh, keh)
	if len(merged) > 0 {
		p.TranslatedHeaders = merged
	}
	return nil
}

// checkLegacyAPIBase admits litellm_kwargs.api_base only as a redundant alias:
// it must validate like base_url (http/https, no userinfo/query/fragment) and
// its suffix-expanded generation endpoint must equal the provider base's
// endpoint. No second endpoint is stored; the declared BaseURL is unchanged.
func checkLegacyAPIBase(p *Provider, raw any, where string) error {
	keyPath := where + ".litellm_kwargs.api_base"
	s, ok := asString(raw)
	if !ok || strings.TrimSpace(s) == "" {
		return fmt.Errorf("%s: must be a non-empty string; use base_url instead", keyPath)
	}
	normalized, err := normalizeBaseURL(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("%s: must be a bare http(s) origin/path without userinfo, query or fragment; use base_url instead", keyPath)
	}
	want, err1 := endpoint.GenerationEndpoint(p.BaseURL, p.Dialect)
	got, err2 := endpoint.GenerationEndpoint(normalized, p.Dialect)
	if err1 != nil || err2 != nil || want != got {
		return fmt.Errorf("%s: unsupported in this build; use base_url instead", keyPath)
	}
	return nil
}

// validateRouteDialectAliases extends the redundant-alias rule to overrides:
// a full alias base that is default-equivalent but NOT equivalent for some
// effective configured route destination is a startup error. It validates the
// provider default plus every effective configured route dialect (including
// disabled routes) using temporary raw validation inputs; no second runtime
// endpoint is stored. The primary base_url itself always follows the exact
// existing suffix rules, even when it contains another dialect's endpoint.
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
			continue // already rejected in fillProvider
		}
		aliasNormalized, err := normalizeBaseURL(strings.TrimSpace(s))
		if err != nil {
			continue // already rejected in fillProvider
		}
		dests := map[string]bool{p.Dialect: true}
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

// checkLegacyTimeout admits litellm_kwargs.timeout only as a redundant alias:
// it must be a finite positive strict number equal to the effective provider
// timeout (including the default). No second timeout is stored and retry
// behavior is unchanged.
func checkLegacyTimeout(p *Provider, raw any, where string) error {
	keyPath := where + ".litellm_kwargs.timeout"
	n, ok := asNum(raw)
	if !ok || math.IsNaN(n) || math.IsInf(n, 0) {
		return fmt.Errorf("%s: must be a finite number; use timeout instead", keyPath)
	}
	if n <= 0 || n > maxTimeoutSeconds {
		return fmt.Errorf("%s: must be a positive number <= %v; use timeout instead", keyPath, maxTimeoutSeconds)
	}
	if n != p.Timeout {
		return fmt.Errorf("%s: unsupported in this build; use timeout instead", keyPath)
	}
	return nil
}

// mergeLegacyHeaders merges kwargs.headers < kwargs.extra_headers
// case-insensitively (last-wins), preserving the winner's original
// spelling. Sorted keys make the merge deterministic.
func mergeLegacyHeaders(kh, keh map[string]string) map[string]string {
	if len(kh) == 0 && len(keh) == 0 {
		return nil
	}
	lowerToKey := map[string]string{}
	out := map[string]string{}
	apply := func(m map[string]string) {
		names := make([]string, 0, len(m))
		for k := range m {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			lower := strings.ToLower(k)
			if prev, exists := lowerToKey[lower]; exists {
				delete(out, prev)
			}
			lowerToKey[lower] = k
			out[k] = m[k]
		}
	}
	apply(kh)
	apply(keh)
	return out
}

// normalizeBaseURL enforces an http(s) URL with no userinfo, query or
// fragment, mirroring the credential-isolation policy: credentials travel in
// configured headers, never in URLs.
func normalizeBaseURL(s string) (string, error) {
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("'base_url' must start with http:// or https://")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("'base_url' must start with http:// or https://")
	}
	if u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return "", fmt.Errorf("'base_url' must be a bare http(s) origin/path without userinfo, query or fragment")
	}
	return strings.TrimRight(s, "/"), nil
}

func fillSessionPolicy(p *Provider, t map[string]any, where string) error {
	var sessionHeader string
	if v, ok := t["session_header"]; ok {
		s, ok := asString(v)
		if !ok {
			return fmt.Errorf("%s: 'session_header' must be a string", where)
		}
		sessionHeader = strings.ToLower(s)
		if sessionHeader != "" && (!httpToken.MatchString(sessionHeader) || isDropHeader(sessionHeader)) {
			return fmt.Errorf("%s: 'session_header' must be a dedicated identity header, "+
				"separate from credentials, protocol and transport headers", where)
		}
		// Static headers must not carry conversation identity, even when a
		// session header is configured. Covers provider headers and the
		// merged legacy translation headers (union of all configured
		// legacy names; token/ASCII/hardcoded checks already ran per
		// source in checkHeaders).
		for name := range p.Headers {
			if isReservedSessionHeader(name, sessionHeader) {
				return fmt.Errorf("%s: static session headers are forbidden; supply a conversation identity on each request", where)
			}
		}
		for name := range p.TranslatedHeaders {
			if isReservedSessionHeader(name, sessionHeader) {
				return fmt.Errorf("%s: static session headers are forbidden; supply a conversation identity on each request", where)
			}
		}
	}
	if v, ok := t["session_source_headers"]; ok {
		arr, ok := asArray(v)
		if !ok {
			return fmt.Errorf("%s: 'session_source_headers' must be an array of reviewed header names", where)
		}
		// Non-nil even when empty: explicit [] disables native selection
		// while absent (nil) defaults to all seven reviewed natives.
		p.SessionSourceHeaders = []string{}
		seen := map[string]bool{}
		for _, e := range arr {
			s, ok := asString(e)
			if !ok || !nativeSessionSources[strings.ToLower(s)] {
				return fmt.Errorf("%s: 'session_source_headers' must be an array of reviewed header names", where)
			}
			lower := strings.ToLower(s)
			if seen[lower] {
				return fmt.Errorf("%s: 'session_source_headers' contains duplicate headers", where)
			}
			seen[lower] = true
			p.SessionSourceHeaders = append(p.SessionSourceHeaders, lower)
		}
	}
	if v, ok := t["session_prompt_cache_key"]; ok {
		b, ok := asBool(v)
		if !ok {
			return fmt.Errorf("%s: 'session_prompt_cache_key' must be boolean", where)
		}
		p.SessionPromptCacheKey = b
	}
	if v, ok := t["session_missing"]; ok {
		s, ok := asString(v)
		if !ok || (s != "omit" && s != "reject") {
			return fmt.Errorf("%s: 'session_missing' must be \"omit\" or \"reject\"", where)
		}
		p.SessionMissing = s
	} else {
		p.SessionMissing = "omit"
	}
	p.SessionHeader = sessionHeader
	_, hasSources := t["session_source_headers"]
	_, hasCacheKey := t["session_prompt_cache_key"]
	_, hasMissing := t["session_missing"]
	if sessionHeader == "" && (hasSources || hasCacheKey || hasMissing) {
		return fmt.Errorf("%s: session policy requires 'session_header'", where)
	}
	if sessionHeader != "" && !hasSources {
		// Absent sources default to all seven reviewed native headers in
		// baseline order; explicit [] disables native selection.
		p.SessionSourceHeaders = append(p.SessionSourceHeaders,
			"x-claude-code-session-id", "session-id", "session_id", "thread-id",
			"conversation_id", "x-session-id", "x-mavis-session-id")
	}
	return nil
}

// dropHeaders mirrors the Python baseline's reserved set: credential,
// hop-by-hop, representation and attribution headers that a session identity
// header must stay separate from.
func isDropHeader(lower string) bool {
	switch lower {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
		"te", "trailer", "transfer-encoding", "upgrade",
		"host", "content-length", "authorization", "x-api-key", "api-key",
		"x-goog-api-key", "x-model-router-reasoning-effort",
		"x-goog-user-project", "x-goog-quota-user", "cookie", "user-agent",
		"content-type", "content-encoding", "content-md5", "content-digest",
		"repr-digest", "digest", "content-range", "content-location",
		"content-language", "etag", "last-modified", "signature", "signature-input",
		"accept", "accept-encoding", "anthropic-beta", "anthropic-version", "openai-beta":
		return true
	}
	return false
}

// resolveCredential applies the exact precedence from the baseline: a present
// api_key always wins (stripped at use); else api_key_env is read from the
// environment; else api_key_file is read and stripped; else "". Env/file
// sources must be non-empty strings and resolve to non-empty values.
// Literal os.environ/NAME values are rejected — use api_key_env.
func resolveCredential(where string, keys map[string]any) (string, error) {
	for _, field := range []string{"api_key_env", "api_key_file"} {
		if v, ok := keys[field]; ok {
			s, ok := asString(v)
			if !ok || strings.TrimSpace(s) == "" {
				return "", fmt.Errorf("%s: '%s' must be a non-empty string", where, field)
			}
		}
	}
	if v, ok := keys["api_key"]; ok {
		if v == nil {
			return "", fmt.Errorf("%s: 'api_key' cannot be null; omit it for an unauthenticated upstream", where)
		}
		s, ok := asString(v)
		if !ok {
			return "", fmt.Errorf("%s: 'api_key' must be a string (omit it for an unauthenticated upstream)", where)
		}
		if strings.HasPrefix(s, "os.environ/") {
			return "", fmt.Errorf("%s: use api_key_env for environment references", where)
		}
		return s, nil
	}
	if v, ok := keys["api_key_env"]; ok {
		name, _ := asString(v)
		val, present := os.LookupEnv(name)
		if !present || strings.TrimSpace(val) == "" {
			return "", fmt.Errorf("%s: environment variable %q is missing or empty", where, name)
		}
		return val, nil
	}
	if v, ok := keys["api_key_file"]; ok {
		path, _ := asString(v)
		if strings.HasPrefix(path, "~/") {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("%s: cannot resolve api_key_file", where)
			}
			path = filepath.Join(home, path[2:])
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("%s: cannot read api_key_file", where)
		}
		if strings.TrimSpace(string(data)) == "" {
			return "", fmt.Errorf("%s: api_key_file is empty", where)
		}
		return strings.TrimSpace(string(data)), nil
	}
	// Omitted key: unauthenticated upstream placeholder at send time.
	return "", nil
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
		if s != providers[provider].Dialect {
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
