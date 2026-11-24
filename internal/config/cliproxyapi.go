// Package config cliproxyapi support: the optional [providers.cliproxyapi]
// table that wraps an upstream CLIProxyAPI endpoint.
package config

import (
	"strings"

	"github.com/arhammahajan/router/internal/provider"
)

// CLIProxyAPI constants re-exported from internal/provider.
const (
	CLIProxyAPIProviderName          = provider.CLIProxyAPIProviderName
	DefaultCLIProxyAPIBaseURL        = provider.DefaultCLIProxyAPIBaseURL
	DefaultCLIProxyAPIRefreshSeconds = provider.DefaultCLIProxyAPIRefreshSeconds
	DefaultCLIProxyAPIProbeTimeout   = provider.DefaultCLIProxyAPIProbeTimeout
)

// CLIProxyAPIConfig is an alias for provider.CLIProxySpec.
type CLIProxyAPIConfig = provider.CLIProxySpec

// CLIProxyProvider returns the generated trusted provider, or nil when
// [providers.cliproxyapi] is absent.
func (c *Config) CLIProxyProvider() *Provider {
	if c == nil || c.CLIProxyAPI == nil {
		return nil
	}
	return c.CLIProxyAPI.Provider()
}

// cliProxyCatalog supplies trusted discovery settings for the existing catalog
// worker from a validated CLIProxyAPIConfig.
func cliProxyCatalog(cli *CLIProxyAPIConfig, providers map[string]*Provider) (*Catalog, error) {
	headers := map[string]any{}
	for k, v := range cli.Headers {
		headers[k] = v
	}
	if key := strings.TrimSpace(cli.APIKey); key != "" {
		headers["Authorization"] = "Bearer " + key
	}
	timeout := cli.ProbeTimeout
	if timeout <= 0 {
		timeout = DefaultCLIProxyAPIProbeTimeout
	}
	return fillCatalog(map[string]any{
		"url":             cli.BaseURL + "/models",
		"timeout":         timeout,
		"refresh_seconds": cli.RefreshSeconds,
		"headers":         headers,
		"rules":           []any{map[string]any{"match": "^", "provider": CLIProxyAPIProviderName}},
	}, providers)
}
