package provider

import (
	"fmt"
	"strings"

	"github.com/arhammahajan/router/internal/capability"
	"github.com/arhammahajan/router/internal/endpoint"
	"github.com/arhammahajan/router/internal/protocol"
)

// CLIProxyAPIPlugin handles [providers.cliproxyapi].
type CLIProxyAPIPlugin struct{}

var cliproxyAPIAllowedKeys = map[string]bool{
	"base_url": true, "api_key": true, "api_key_env": true, "api_key_file": true,
	"auth_header": true, "headers": true, "timeout": true, "compatibility": true,
	"config_path": true, "passthrough": true, "refresh_seconds": true, "probe_timeout": true,
}

func (*CLIProxyAPIPlugin) Kind() string { return KindCLIProxyAPI }

func (*CLIProxyAPIPlugin) ParseConfig(common CommonConfig, raw map[string]any, _ string, _ func(string) (string, bool)) (*ParseResult, error) {
	where := "providers." + common.Name
	if err := RejectUnknown(raw, cliproxyAPIAllowedKeys, where,
		" (supported: base_url, api_key/api_key_env/api_key_file, auth_header, headers, timeout, compatibility, config_path, passthrough, refresh_seconds, probe_timeout)"); err != nil {
		return nil, err
	}
	base := DefaultCLIProxyAPIBaseURL
	if common.HasBaseURL {
		base = common.BaseURL
	}
	configPath := ""
	if v, ok := raw["config_path"]; ok {
		s, ok := AsString(v)
		if !ok {
			return nil, fmt.Errorf("%s: 'config_path' must be a string", where)
		}
		configPath = strings.TrimSpace(s)
	}
	passthrough := false
	if v, ok := raw["passthrough"]; ok {
		b, ok := AsBool(v)
		if !ok {
			return nil, fmt.Errorf("%s: 'passthrough' must be boolean", where)
		}
		passthrough = b
	}
	refreshSeconds := DefaultCLIProxyAPIRefreshSeconds
	if v, ok := raw["refresh_seconds"]; ok {
		n, err := FiniteNonNegative(v, where, "refresh_seconds")
		if err != nil {
			return nil, err
		}
		refreshSeconds = n
	}
	probeTimeout := DefaultCLIProxyAPIProbeTimeout
	if v, ok := raw["probe_timeout"]; ok {
		n, err := FinitePositive(v, where, "probe_timeout")
		if err != nil {
			return nil, err
		}
		probeTimeout = n
	}

	lossy := common.Lossy != nil && *common.Lossy
	timeout := common.Timeout
	if timeout <= 0 {
		timeout = DefaultProviderTimeout
	}

	cli := &CLIProxySpec{
		BaseURL:          base,
		APIKey:           common.APIKey,
		ConfigPath:       configPath,
		Lossy:            lossy,
		Passthrough:      passthrough,
		CompatibilityRaw: common.CompatibilityRaw,
		RefreshSeconds:   refreshSeconds,
		ProbeTimeout:     probeTimeout,
		AuthHeader:       common.AuthHeader,
		Headers:          CopyHeaders(common.Headers),
		Timeout:          timeout,
	}
	return &ParseResult{
		Provider: cli.Provider(),
		CLIProxy: cli,
	}, nil
}

func (*CLIProxyAPIPlugin) AuthHeaders(p *Spec, destDialect string) map[string]string {
	return DefaultAuthHeaders(p, destDialect)
}

func (*CLIProxyAPIPlugin) GenerationEndpoint(baseURL, destDialect string) (string, error) {
	return endpoint.GenerationEndpoint(baseURL, destDialect)
}

func (*CLIProxyAPIPlugin) PrepareNativeBody(_ string, body []byte) ([]byte, bool) {
	return body, false
}

func (*CLIProxyAPIPlugin) EffortScope(destDialect string) protocol.RouteScope {
	switch destDialect {
	case DialectOpenAIResponses:
		return protocol.OrdinaryResponsesRoute()
	case DialectGemini:
		return protocol.OrdinaryGeminiRoute()
	case DialectAnthropic:
		return protocol.CompatAnthropicRoute()
	default:
		return protocol.OrdinaryRoute()
	}
}

func (*CLIProxyAPIPlugin) RefusesUnobservedReasoning() bool { return false }

func (*CLIProxyAPIPlugin) ApplyCapabilities(_ *capability.Contract, _ float64) {}
