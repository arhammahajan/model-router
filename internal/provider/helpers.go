package provider

import (
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/arhammahajan/router/internal/catalog"
)

// CommonProviderKeys are the keys parsed by ParseCommonConfig for every
// [providers.<name>] table.
var CommonProviderKeys = map[string]bool{
	"base_url":      true,
	"api_key":       true,
	"api_key_env":   true,
	"api_key_file":  true,
	"auth_header":   true,
	"headers":       true,
	"timeout":       true,
	"compatibility": true,
}

// --- strict TOML/JSON decoded-value helpers ---

func AsString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

func AsBool(v any) (bool, bool) {
	b, ok := v.(bool)
	return b, ok
}

func AsInt(v any) (int64, bool) {
	n, ok := v.(int64)
	return n, ok
}

func AsNum(v any) (float64, bool) {
	switch n := v.(type) {
	case int64:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

func AsTable(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

func AsArray(v any) ([]any, bool) {
	a, ok := v.([]any)
	return a, ok
}

// FinitePositive checks that v is a finite number in (0, MaxTimeoutSeconds].
func FinitePositive(v any, where, field string) (float64, error) {
	n, ok := AsNum(v)
	if !ok || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, fmt.Errorf("%s: '%s' must be a finite number", where, field)
	}
	if n <= 0 || n > MaxTimeoutSeconds {
		return 0, fmt.Errorf("%s: '%s' must be a positive number <= %v", where, field, MaxTimeoutSeconds)
	}
	return n, nil
}

// FiniteNonNegative checks that v is a finite number in [0, MaxTimeoutSeconds].
func FiniteNonNegative(v any, where, field string) (float64, error) {
	n, ok := AsNum(v)
	if !ok || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, fmt.Errorf("%s: '%s' must be a finite number", where, field)
	}
	if n < 0 || n > MaxTimeoutSeconds {
		return 0, fmt.Errorf("%s: '%s' must be >= 0 and <= %v", where, field, MaxTimeoutSeconds)
	}
	return n, nil
}

// PositiveInt checks that v is an int64 > 0.
func PositiveInt(v any, where, field string) (int64, error) {
	n, ok := AsInt(v)
	if !ok || n <= 0 {
		return 0, fmt.Errorf("%s: '%s' must be a positive integer", where, field)
	}
	return n, nil
}

// RejectUnknown fails on keys outside allowed. Messages stay value-free.
func RejectUnknown(table map[string]any, allowed map[string]bool, where, hint string) error {
	keys := make([]string, 0, len(table))
	for k := range table {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !allowed[k] {
			if sub, ok := table[k].(map[string]any); ok && len(sub) > 0 {
				return fmt.Errorf("%s.%s: nested tables are not supported in this build%s", where, k, hint)
			}
			return fmt.Errorf("%s.%s: unsupported setting in this build%s", where, k, hint)
		}
	}
	return nil
}

var httpToken = regexp.MustCompile(`^[!#$%&'*+.^_` + "`" + `|~0-9A-Za-z-]+$`)

func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 32 || s[i] > 126 {
			return false
		}
	}
	return true
}

// IsReservedSessionHeader reports whether name is a reserved conversation
// identity header that may never be configured statically.
func IsReservedSessionHeader(name, sessionHeader string) bool {
	lower := strings.ToLower(name)
	switch lower {
	case "x-claude-code-session-id", "session-id", "session_id", "thread-id",
		"conversation_id", "x-session-id", "x-mavis-session-id", "x-opencode-session":
		return true
	}
	return sessionHeader != "" && lower == strings.ToLower(sessionHeader)
}

// CheckHeaders enforces HTTP token names, printable-ASCII values, and the ban
// on static session headers.
func CheckHeaders(v any, where string, sessionHeader string) (map[string]string, error) {
	tab, ok := AsTable(v)
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
		s, ok := AsString(tab[k])
		if !ok || !httpToken.MatchString(k) || !printableASCII(s) {
			return nil, fmt.Errorf("%s: 'headers' must contain HTTP header names and string values", where)
		}
		if IsReservedSessionHeader(k, sessionHeader) {
			return nil, fmt.Errorf("%s: static session headers are forbidden; supply a conversation identity on each request", where)
		}
		out[k] = s
	}
	return out, nil
}

// MergeLegacyHeaders merges kh < keh case-insensitively (last-wins),
// preserving the winner's original spelling.
func MergeLegacyHeaders(kh, keh map[string]string) map[string]string {
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

// CopyHeaders returns a shallow copy of m (always non-nil).
func CopyHeaders(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ResolveCredential resolves api_key > api_key_env > api_key_file > "".
// If env is nil, os.LookupEnv is used.
func ResolveCredential(where string, keys map[string]any, env func(string) (string, bool)) (string, error) {
	if env == nil {
		env = os.LookupEnv
	}
	for _, field := range []string{"api_key_env", "api_key_file"} {
		if v, ok := keys[field]; ok {
			s, ok := AsString(v)
			if !ok || strings.TrimSpace(s) == "" {
				return "", fmt.Errorf("%s: '%s' must be a non-empty string", where, field)
			}
		}
	}
	if v, ok := keys["api_key"]; ok {
		if v == nil {
			return "", fmt.Errorf("%s: 'api_key' cannot be null; omit it for an unauthenticated upstream", where)
		}
		s, ok := AsString(v)
		if !ok {
			return "", fmt.Errorf("%s: 'api_key' must be a string (omit it for an unauthenticated upstream)", where)
		}
		if strings.HasPrefix(s, "os.environ/") {
			return "", fmt.Errorf("%s: use api_key_env for environment references", where)
		}
		return s, nil
	}
	if v, ok := keys["api_key_env"]; ok {
		name, _ := AsString(v)
		val, present := env(name)
		if !present || strings.TrimSpace(val) == "" {
			return "", fmt.Errorf("%s: environment variable %q is missing or empty", where, name)
		}
		return val, nil
	}
	if v, ok := keys["api_key_file"]; ok {
		path, _ := AsString(v)
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
	return "", nil
}

// NormalizeBaseURL enforces a bare http(s) origin/path with no userinfo,
// query, or fragment, and strips trailing slashes.
func NormalizeBaseURL(s string) (string, error) {
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

// StripV1Suffix removes a single trailing "/v1" path segment (after trimming
// trailing slashes) so probe URLs stay at the server root.
func StripV1Suffix(raw string) string {
	s := strings.TrimRight(strings.TrimSpace(raw), "/")
	if strings.HasSuffix(s, "/v1") {
		s = strings.TrimRight(strings.TrimSuffix(s, "/v1"), "/")
	}
	return s
}

// ParseCompatibility parses a "strict" | "lossy" compatibility setting.
// Returns (raw, lossyPtr, err). When absent, raw is "" and lossyPtr is nil.
func ParseCompatibility(tab map[string]any, where string) (string, *bool, error) {
	v, ok := tab["compatibility"]
	if !ok {
		return "", nil, nil
	}
	s, ok := AsString(v)
	if !ok || (s != "strict" && s != "lossy") {
		return "", nil, fmt.Errorf("%s: 'compatibility' must be \"strict\" or \"lossy\"", where)
	}
	lossy := s == "lossy"
	return s, &lossy, nil
}

// ParseAuthHeader validates an auth_header setting.
func ParseAuthHeader(tab map[string]any, where string) (string, bool, error) {
	v, ok := tab["auth_header"]
	if !ok {
		return "", false, nil
	}
	s, ok := AsString(v)
	if !ok {
		return "", false, fmt.Errorf("%s: 'auth_header' must be a string", where)
	}
	s = strings.ToLower(strings.TrimSpace(s))
	if s != "" && s != "bearer" && s != "x-api-key" && s != "api-key" && s != "x-goog-api-key" {
		return "", false, fmt.Errorf("%s: 'auth_header' must be \"bearer\", \"x-api-key\", \"api-key\", \"x-goog-api-key\" or empty", where)
	}
	return s, true, nil
}

// ParseCommonConfig parses the common fields shared by all [providers.<name>]
// tables without rejecting plugin-specific extra keys.
func ParseCommonConfig(name string, tab map[string]any, env func(string) (string, bool)) (CommonConfig, error) {
	where := "providers." + name
	c := CommonConfig{
		Name:    name,
		Headers: map[string]string{},
		Timeout: DefaultProviderTimeout,
	}
	if v, ok := tab["base_url"]; ok {
		base, ok := AsString(v)
		if !ok || strings.TrimSpace(base) == "" {
			return CommonConfig{}, fmt.Errorf("%s: 'base_url' must be a non-empty string", where)
		}
		normalized, err := NormalizeBaseURL(strings.TrimSpace(base))
		if err != nil {
			return CommonConfig{}, fmt.Errorf("%s: %v", where, err)
		}
		c.BaseURL = normalized
		c.HasBaseURL = true
	}
	_, hasKey := tab["api_key"]
	_, hasEnv := tab["api_key_env"]
	_, hasFile := tab["api_key_file"]
	if hasKey || hasEnv || hasFile {
		c.HasAPIKey = true
	}
	key, err := ResolveCredential(where, tab, env)
	if err != nil {
		return CommonConfig{}, err
	}
	c.APIKey = key

	ah, _, err := ParseAuthHeader(tab, where)
	if err != nil {
		return CommonConfig{}, err
	}
	c.AuthHeader = ah

	if v, ok := tab["headers"]; ok {
		hdrs, err := CheckHeaders(v, where, "")
		if err != nil {
			return CommonConfig{}, err
		}
		c.Headers = hdrs
	}
	if v, ok := tab["timeout"]; ok {
		n, err := FinitePositive(v, where, "timeout")
		if err != nil {
			return CommonConfig{}, err
		}
		c.Timeout = n
		c.HasTimeout = true
	}
	compatRaw, lossy, err := ParseCompatibility(tab, where)
	if err != nil {
		return CommonConfig{}, err
	}
	c.CompatibilityRaw = compatRaw
	c.Lossy = lossy
	return c, nil
}

// DefaultAuthHeaders computes the upstream credential header for p when
// sending to destDialect.
//
// If p.AuthHeader is explicitly configured, it is always honored. Otherwise,
// destDialect is used when natively supported by p (else p.DefaultDialect):
//   - anthropic -> x-api-key: <key>
//   - gemini    -> x-goog-api-key: <key>
//   - anything else -> Authorization: Bearer <key>
func DefaultAuthHeaders(p *Spec, destDialect string) map[string]string {
	if p == nil {
		return map[string]string{}
	}
	key := p.EffectiveKey()
	if key == "" {
		return map[string]string{}
	}
	if ah := strings.TrimSpace(p.AuthHeader); ah != "" {
		switch strings.ToLower(ah) {
		case "bearer":
			return map[string]string{"Authorization": "Bearer " + key}
		case "x-api-key", "api-key", "x-goog-api-key":
			return map[string]string{strings.ToLower(ah): key}
		default:
			return map[string]string{ah: key}
		}
	}
	effectiveDialect := p.DefaultDialect
	if destDialect != "" && p.SupportsDialect(destDialect) {
		effectiveDialect = destDialect
	}
	switch effectiveDialect {
	case DialectAnthropic:
		return map[string]string{"x-api-key": key}
	case DialectGemini:
		return map[string]string{"x-goog-api-key": key}
	default:
		return map[string]string{"Authorization": "Bearer " + key}
	}
}

// LocalLimitFields is the closed limit vocabulary for local server fleets.
var LocalLimitFields = []string{
	"context_window",
	"trained_context_window",
	"max_input_tokens",
	"max_output_tokens",
	"default_output_tokens",
}

func isLocalLimitField(k string) bool {
	for _, f := range LocalLimitFields {
		if k == f {
			return true
		}
	}
	return false
}

func localLimitMinimum(key string) int64 {
	if key == "default_output_tokens" {
		return 0
	}
	return 1
}

// CloneLimitOverrides deep-copies a limit override table.
func CloneLimitOverrides(in map[string]map[string]*int64) map[string]map[string]*int64 {
	if in == nil {
		return nil
	}
	out := make(map[string]map[string]*int64, len(in))
	for model, fields := range in {
		cp := make(map[string]*int64, len(fields))
		for k, v := range fields {
			if v == nil {
				cp[k] = nil
			} else {
				nv := *v
				cp[k] = &nv
			}
		}
		out[model] = cp
	}
	return out
}

// CopyInt64Ptr returns a fresh copy of p (nil stays nil).
func CopyInt64Ptr(p *int64) *int64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// ParseLocalLimits validates context_fallback + limit_overrides startup
// value-free before any I/O. where identifies the table (e.g.
// "providers.llama_cpp" or "providers.strata").
func ParseLocalLimits(tab map[string]any, where string) (*int64, map[string]map[string]*int64, error) {
	var fallback *int64
	if raw, ok := tab["context_fallback"]; ok {
		n, ok := AsInt(raw)
		if !ok || n <= 0 {
			return nil, nil, fmt.Errorf("%s: 'context_fallback' must be a positive integer", where)
		}
		fallback = CopyInt64Ptr(&n)
	}
	var overrides map[string]map[string]*int64
	if raw, ok := tab["limit_overrides"]; ok {
		outer, ok := AsTable(raw)
		if !ok {
			return nil, nil, fmt.Errorf("%s.limit_overrides: must be a table", where)
		}
		overrides = make(map[string]map[string]*int64, len(outer))
		names := make([]string, 0, len(outer))
		for k := range outer {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, modelID := range names {
			label := fmt.Sprintf("%s.limit_overrides[%q]", where, modelID)
			if modelID == "" || modelID != strings.TrimSpace(modelID) {
				return nil, nil, fmt.Errorf("%s: model ID must be a nonempty string without surrounding whitespace", label)
			}
			inner, ok := AsTable(outer[modelID])
			if !ok {
				return nil, nil, fmt.Errorf("%s: must be a table/object", label)
			}
			fields := make(map[string]*int64, len(inner))
			keys := make([]string, 0, len(inner))
			for k := range inner {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, key := range keys {
				if !isLocalLimitField(key) {
					return nil, nil, fmt.Errorf("%s: unknown limit %q", label, key)
				}
				minimum := localLimitMinimum(key)
				val := inner[key]
				if val == nil {
					fields[key] = nil
					continue
				}
				n, ok := AsInt(val)
				if !ok || n < minimum {
					return nil, nil, fmt.Errorf("%s.%s: must be an integer >= %d or null", label, key, minimum)
				}
				nv := n
				fields[key] = &nv
			}
			if len(fields) != 0 {
				overrides[modelID] = fields
			}
		}
		if len(overrides) == 0 {
			overrides = nil
		}
	}
	return fallback, overrides, nil
}

// LocalProviderName generates a unique provider name for a local server URL
// using prefix (e.g. "llama" or "strata") against the taken set.
var localGenSanitize = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

func LocalProviderName(prefix, rawURL string, taken map[string]bool) string {
	parsed, err := url.Parse(rawURL)
	base := ""
	if err == nil {
		base = prefix + "-" + strings.Trim(strings.TrimPrefix(rawURL, parsed.Scheme+"://"), "/")
		base = localGenSanitize.ReplaceAllString(base, "-")
		base = strings.Trim(base, "-")
	}
	if base == "" {
		base = prefix
	}
	name := base
	for i := 2; taken[name]; i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	return name
}

func normalizeLocalServerURL(s, where string) (string, error) {
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
