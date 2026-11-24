// Discovery persistence: optional route-only disk cache (schema v2).
//
// Go owns schema v2; a Python v1 file loads as incompatible, never parsed.
// Endpoints, keys, headers, metadata and capabilities are never persisted
// and never read. The file carries route IDs and full alias names only;
// providers are re-resolved from trusted config at restore.
package discovery

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/redact"
)

// --- secret corpus + sanitizer (mirrors Python ErrorSanitizer) ---

func cacheSecrets(cfg *config.Config) []string {
	var out []string
	add := func(s string) {
		if s != "" {
			out = append(out, s)
			if len(s) > 7 && strings.EqualFold(s[:7], "bearer ") {
				out = append(out, strings.TrimSpace(s[7:]))
			}
		}
	}
	for _, k := range cfg.Server.APIKeys {
		add(strings.TrimSpace(k))
	}
	for _, p := range cfg.Providers {
		add(p.APIKey)
		for _, v := range p.Headers {
			add(v)
		}
		for _, v := range p.TranslatedHeaders {
			add(v)
		}
		addURLSecrets(&out, p.BaseURL)
	}
	if cfg.CLIProxyAPI != nil {
		add(cfg.CLIProxyAPI.APIKey)
		for _, v := range cfg.CLIProxyAPI.Headers {
			add(v)
		}
		addURLSecrets(&out, cfg.CLIProxyAPI.BaseURL)
	}
	if cfg.Catalog != nil {
		for _, v := range cfg.Catalog.Headers {
			add(v)
		}
		addURLSecrets(&out, cfg.Catalog.URL)
	}
	if cfg.Llama != nil {
		for _, v := range cfg.Llama.Headers {
			add(v)
		}
		for _, s := range cfg.Llama.Servers {
			add(s.APIKey)
			for _, v := range s.Headers {
				add(v)
			}
			addURLSecrets(&out, s.URL)
			addURLSecrets(&out, s.BaseURL)
		}
	}
	return out
}

func addURLSecrets(out *[]string, raw string) {
	u, err := url.Parse(raw)
	if err != nil {
		return
	}
	if u.User != nil {
		// net/url has already percent-decoded userinfo. Decoding again
		// changes literal percent escapes and can miss the real secret.
		if name := u.User.Username(); name != "" {
			*out = append(*out, name)
		}
		if pw, ok := u.User.Password(); ok && pw != "" {
			*out = append(*out, pw)
		}
	}
	q := u.Query()
	for _, vals := range q {
		for _, v := range vals {
			if v != "" {
				*out = append(*out, v)
			}
		}
	}
}

type cacheSanitizer struct {
	secrets []string
}

func newCacheSanitizer(cfg *config.Config) *cacheSanitizer {
	return &cacheSanitizer{secrets: redact.ExpandCorpus(cacheSecrets(cfg))}
}

// These admission patterns mirror the generic credential classes in
// Python ErrorSanitizer. Matches are rejected, never rewritten into IDs.
var cacheCredentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)bearer\s+[^\s"',;<>\[\]{}]+`),
	regexp.MustCompile(`(?i)(?:[\p{L}\p{N}_-]*(?:key|token|secret|password|credential)|authorization|cookie)["']?\s*[:=]\s*["']?[^\s"',;&<>\[\]{}]+`),
	regexp.MustCompile(`(?:sk-[\p{L}\p{N}_.-]+|AIza[\p{L}\p{N}_-]{20,}|gsk_[\p{L}\p{N}_]{20,})`),
}

func (c *cacheSanitizer) sanitizeOne(s string) string {
	for _, secret := range c.secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "<redacted>")
		}
	}
	return s
}

// safeID applies full admission: basic checks plus secret-corpus sanitizer.
func (c *cacheSanitizer) safeID(s string) bool {
	if !basicSafeID(s) {
		return false
	}
	for _, pattern := range cacheCredentialPatterns {
		if pattern.MatchString(s) {
			return false
		}
	}
	return c.sanitizeOne(s) == s
}

// --- deterministic identity (digest only, no byte-compat claim) ---

func cacheIdentity(cfg *config.Config) string {
	type providerView struct {
		Kind              string            `json:"kind,omitempty"`
		DefaultDialect    string            `json:"default_dialect"`
		SupportedDialects []string          `json:"supported_dialects,omitempty"`
		BaseURL           string            `json:"base_url"`
		APIKey            string            `json:"api_key"`
		CountTokens       *bool             `json:"count_tokens,omitempty"`
		Headers           map[string]string `json:"headers"`
		Timeout           float64           `json:"timeout"`
		Lossy             bool              `json:"lossy"`
		AuthHeader        string            `json:"auth_header"`
		LitellmProvider   string            `json:"litellm_provider"`
		TranslatedHeaders map[string]string `json:"translated_headers,omitempty"`
		SessionHeader     string            `json:"session_header,omitempty"`
		SessionSources    []string          `json:"session_sources,omitempty"`
		SessionCacheKey   bool              `json:"session_cache_key,omitempty"`
		SessionMissing    string            `json:"session_missing,omitempty"`
	}
	type modelView struct {
		Provider string   `json:"provider"`
		Upstream string   `json:"upstream"`
		Dialect  string   `json:"dialect,omitempty"`
		Aliases  []string `json:"aliases,omitempty"`
		Disabled bool     `json:"disabled,omitempty"`
	}
	providers := map[string]providerView{}
	names := make([]string, 0, len(cfg.Providers))
	for n := range cfg.Providers {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := cfg.Providers[n]
		var countTokens *bool
		if p.CountTokens != nil {
			b := *p.CountTokens
			countTokens = &b
		}
		providers[n] = providerView{
			Kind: p.Kind, DefaultDialect: p.DefaultDialect,
			SupportedDialects: append([]string(nil), p.SupportedDialects...),
			BaseURL:           p.BaseURL, APIKey: p.APIKey, CountTokens: countTokens,
			Headers: copyStrMap(p.Headers), Timeout: p.Timeout, Lossy: p.Lossy,
			AuthHeader: p.AuthHeader, LitellmProvider: p.LitellmProvider,
			TranslatedHeaders: copyStrMap(p.TranslatedHeaders),
			SessionHeader:     p.SessionHeader, SessionSources: append([]string(nil), p.SessionSourceHeaders...),
			SessionCacheKey: p.SessionPromptCacheKey, SessionMissing: p.SessionMissing,
		}
	}
	models := map[string]modelView{}
	for _, m := range cfg.Models {
		models[m.Name] = modelView{Provider: m.Provider, Upstream: m.Upstream, Dialect: m.Dialect, Aliases: append([]string(nil), m.Aliases...), Disabled: m.Disabled}
	}
	type catalogView struct {
		URL            string            `json:"url"`
		Headers        map[string]string `json:"headers"`
		ModelProviders map[string]string `json:"model_providers"`
		Rules          []string          `json:"rules"`
		MetadataSource string            `json:"metadata_source,omitempty"`
	}
	var cat *catalogView
	if cfg.Catalog != nil {
		rules := make([]string, 0, len(cfg.Catalog.Rules))
		for _, r := range cfg.Catalog.Rules {
			rules = append(rules, r.Match+"\x00"+r.Provider)
		}
		sort.Strings(rules)
		cat = &catalogView{URL: cfg.Catalog.URL, Headers: copyStrMap(cfg.Catalog.Headers), ModelProviders: copyStrMap(cfg.Catalog.ModelProviders), Rules: rules, MetadataSource: cfg.Catalog.MetadataSource}
	}
	type llamaServerView struct {
		URL           string                       `json:"url"`
		Name          string                       `json:"name"`
		Kind          string                       `json:"kind,omitempty"`
		APIKey        string                       `json:"api_key"`
		AuthHeader    string                       `json:"auth_header,omitempty"`
		Headers       map[string]string            `json:"headers"`
		Timeout       float64                      `json:"timeout,omitempty"`
		ProbeTimeout  float64                      `json:"probe_timeout,omitempty"`
		Prefix           string                       `json:"prefix,omitempty"`
		ExposeAliases    bool                         `json:"expose_aliases,omitempty"`
		CompatibilityRaw string                       `json:"compatibility,omitempty"`
		Lossy            bool                         `json:"lossy,omitempty"`
		Fallback         *int64                       `json:"context_fallback,omitempty"`
		Overrides        map[string]map[string]*int64 `json:"limit_overrides,omitempty"`
	}
	type llamaView struct {
		Servers []llamaServerView `json:"servers"`
		Headers map[string]string `json:"headers"`
		Prefix  string            `json:"prefix"`
		Expose  bool              `json:"expose_aliases"`
		Compat  string            `json:"compatibility"`
		// Explicit limit policy participates in the identity only when
		// present: absent fallback and empty overrides omit the fields so
		// existing baseline identities are unchanged.
		Fallback *int64 `json:"context_fallback,omitempty"`
		// Raw upstream overrides; nil inner values render as JSON null
		// (explicit withdraw). Sorted at marshal time by encoding/json.
		Overrides map[string]map[string]*int64 `json:"limit_overrides,omitempty"`
	}
	copyOverrides := func(in map[string]map[string]*int64) map[string]map[string]*int64 {
		if len(in) == 0 {
			return nil
		}
		cp := make(map[string]map[string]*int64, len(in))
		for model, fields := range in {
			inner := make(map[string]*int64, len(fields))
			for k, v := range fields {
				if v == nil {
					inner[k] = nil
				} else {
					nv := *v
					inner[k] = &nv
				}
			}
			cp[model] = inner
		}
		return cp
	}
	var llama *llamaView
	if cfg.Llama != nil {
		lv := &llamaView{Headers: copyStrMap(cfg.Llama.Headers), Prefix: cfg.Llama.Prefix, Expose: cfg.Llama.ExposeAliases, Compat: cfg.Llama.Compatibility}
		for _, s := range cfg.Llama.Servers {
			var sfb *int64
			if s.ContextFallback != nil {
				v := *s.ContextFallback
				sfb = &v
			}
			lv.Servers = append(lv.Servers, llamaServerView{
				URL: s.URL, Name: s.Name, Kind: s.Kind, APIKey: s.APIKey,
				AuthHeader: s.AuthHeader, Headers: copyStrMap(s.Headers),
				Timeout: s.Timeout, ProbeTimeout: s.ProbeTimeout,
				Prefix: s.Prefix, ExposeAliases: s.ExposeAliases,
				CompatibilityRaw: s.CompatibilityRaw, Lossy: s.Lossy,
				Fallback: sfb, Overrides: copyOverrides(s.LimitOverrides),
			})
		}
		if cfg.Llama.ContextFallback != nil {
			v := *cfg.Llama.ContextFallback
			lv.Fallback = &v
		}
		lv.Overrides = copyOverrides(cfg.Llama.LimitOverrides)
		llama = lv
	}
	doc := map[string]any{"providers": providers, "models": models, "catalog": cat, "llama": llama}
	raw, _ := json.Marshal(doc)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func copyStrMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
