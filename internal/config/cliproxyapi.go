// Package config cliproxyapi support: the optional [cliproxyapi] table that
// wraps an upstream CLIProxyAPI OpenAI-compatible endpoint.
//
// One generated provider and catalog use the existing translation and discovery
// pipelines. CLIProxyAPI owns the upstream credentials and model routing.
package config

import (
	"fmt"
	"strings"
)

// CLIProxyAPIProviderName is the single generated provider name when
// [cliproxyapi] is configured; an explicit [providers.cliproxyapi] collides
// with it and is rejected at startup.
const CLIProxyAPIProviderName = "cliproxyapi"

// DefaultCLIProxyAPIBaseURL is the conventional local CLIProxyAPI endpoint.
// It keeps a bare [cliproxyapi] table (or one with only credentials)
// usable without repeating the loopback address.
const DefaultCLIProxyAPIBaseURL = "http://127.0.0.1:8317/v1"

// CLIProxyAPIConfig is the validated [cliproxyapi] table. BaseURL is the
// normalized OpenAI-compatible base (no trailing slash, no userinfo/query
// /fragment). APIKey is resolved but raw (stripped at use; "" means
// unauthenticated). ConfigPath is the optional embedded SDK YAML config
// path loaded by the embedded service at startup.
type CLIProxyAPIConfig struct {
	BaseURL          string
	APIKey           string
	ConfigPath       string
	Lossy            bool
	Passthrough      bool
	CompatibilityRaw string
	RefreshSeconds   float64
}

// Provider builds the trusted generated provider from config only: no I/O,
// no payload input. The dialect is always OpenAI Chat; translation to other
// wire protocols happens only through explicit [[models]] dialect overrides,
// never by guessing from discovered IDs.
func (c *CLIProxyAPIConfig) Provider() *Provider {
	if c == nil {
		return nil
	}
	return &Provider{
		Name:             CLIProxyAPIProviderName,
		Dialect:          DialectOpenAIChat,
		BaseURL:          c.BaseURL,
		APIKey:           c.APIKey,
		AuthHeader:       "bearer",
		Headers:          map[string]string{},
		Timeout:          DefaultProviderTimeout,
		Lossy:            c.Lossy,
		Passthrough:      c.Passthrough,
		LitellmProvider:  "openai",
		CompatibilityRaw: c.CompatibilityRaw,
	}
}

// CLIProxyProvider returns the generated trusted provider, or nil when
// [cliproxyapi] is absent.
func (c *Config) CLIProxyProvider() *Provider {
	if c == nil || c.CLIProxyAPI == nil {
		return nil
	}
	return c.CLIProxyAPI.Provider()
}

var cliproxyAPIKeys = map[string]bool{
	"base_url": true, "api_key": true, "api_key_env": true,
	"api_key_file": true, "config_path": true, "compatibility": true,
	"passthrough": true, "refresh_seconds": true,
}

func fillCLIProxyAPI(tab map[string]any) (*CLIProxyAPIConfig, error) {
	const where = "cliproxyapi"
	if err := rejectUnknown(tab, cliproxyAPIKeys, where,
		" (supported: base_url, api_key/api_key_env/api_key_file, config_path, compatibility, passthrough, refresh_seconds)"); err != nil {
		return nil, err
	}
	base := DefaultCLIProxyAPIBaseURL
	if v, ok := tab["base_url"]; ok {
		s, ok := asString(v)
		if !ok || strings.TrimSpace(s) == "" {
			return nil, fmt.Errorf("%s: 'base_url' must be a non-empty string", where)
		}
		normalized, err := normalizeBaseURL(strings.TrimSpace(s))
		if err != nil {
			return nil, fmt.Errorf("%s: %v", where, err)
		}
		base = normalized
	}
	key, err := resolveCredential(where, tab)
	if err != nil {
		return nil, err
	}
	configPath := ""
	if v, ok := tab["config_path"]; ok {
		s, ok := asString(v)
		if !ok {
			return nil, fmt.Errorf("%s: 'config_path' must be a string", where)
		}
		configPath = strings.TrimSpace(s)
	}
	compatRaw := ""
	lossy := false
	if v, ok := tab["compatibility"]; ok {
		s, ok := asString(v)
		if !ok || (s != "strict" && s != "lossy") {
			return nil, fmt.Errorf("%s: 'compatibility' must be \"strict\" or \"lossy\"", where)
		}
		compatRaw = s
		lossy = s == "lossy"
	}
	passthrough := false
	if v, ok := tab["passthrough"]; ok {
		b, ok := asBool(v)
		if !ok {
			return nil, fmt.Errorf("%s: 'passthrough' must be boolean", where)
		}
		passthrough = b
	}
	refreshSeconds := DefaultCatalogRefresh
	if v, ok := tab["refresh_seconds"]; ok {
		n, err := finiteNonNegative(v, where, "refresh_seconds")
		if err != nil {
			return nil, err
		}
		refreshSeconds = n
	}
	return &CLIProxyAPIConfig{
		BaseURL:          base,
		APIKey:           key,
		ConfigPath:       configPath,
		Lossy:            lossy,
		Passthrough:      passthrough,
		CompatibilityRaw: compatRaw,
		RefreshSeconds:   refreshSeconds,
	}, nil
}

// catalog supplies trusted discovery settings for the existing catalog worker.
func (c *CLIProxyAPIConfig) catalog(providers map[string]*Provider) (*Catalog, error) {
	headers := map[string]any{}
	if key := strings.TrimSpace(c.APIKey); key != "" {
		headers["Authorization"] = "Bearer " + key
	}
	return fillCatalog(map[string]any{
		"url":             c.BaseURL + "/models",
		"refresh_seconds": c.RefreshSeconds,
		"headers":         headers,
		"rules":           []any{map[string]any{"match": "^", "provider": CLIProxyAPIProviderName}},
	}, providers)
}
