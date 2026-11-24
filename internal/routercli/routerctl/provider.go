// General, thin built-in provider inspection for the router CLI.
//
// Lists the built-in router providers (cliproxyapi, llama_cpp, strata) and
// reports per-provider status, HTTP server host/port, and shared config fields.
// Credentials and raw header values are never exposed.
package routerctl

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/provider"
)

// ProviderEndpoint describes one HTTP server endpoint belonging to a provider.
type ProviderEndpoint struct {
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	Scheme  string `json:"scheme,omitempty"`
	Host    string `json:"host,omitempty"`
	Port    int    `json:"port,omitempty"`
	Status  string `json:"status,omitempty"`
}

// ProviderInfo is a sanitized, uniform summary of one built-in router provider.
type ProviderInfo struct {
	Name                string             `json:"name"`
	Enabled             bool               `json:"enabled"`
	Status              string             `json:"status"`
	Mode                string             `json:"mode,omitempty"`
	BaseURL             string             `json:"base_url,omitempty"`
	Scheme              string             `json:"scheme,omitempty"`
	Host                string             `json:"host,omitempty"`
	Port                int                `json:"port,omitempty"`
	DefaultDialect      string             `json:"default_dialect"`
	SupportedDialects   []string           `json:"supported_dialects"`
	Compatibility       string             `json:"compatibility,omitempty"`
	TimeoutSeconds      float64            `json:"timeout_seconds,omitempty"`
	ProbeTimeoutSeconds float64            `json:"probe_timeout_seconds,omitempty"`
	AuthHeader          string             `json:"auth_header,omitempty"`
	APIKeyConfigured    bool               `json:"api_key_configured"`
	Prefix              string             `json:"prefix,omitempty"`
	Servers             []ProviderEndpoint `json:"servers,omitempty"`
}

// NormalizeProviderName maps common user-facing aliases (cpa, llama.cpp, llama)
// to canonical built-in provider names.
func NormalizeProviderName(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "cpa", "cliproxyapi":
		return provider.KindCLIProxyAPI
	case "llama.cpp", "llama_cpp", "llama":
		return provider.KindLlamaCpp
	case "strata":
		return provider.KindStrata
	default:
		return strings.TrimSpace(name)
	}
}

// FetchProviders returns the built-in router providers (cliproxyapi, llama_cpp,
// strata) with their enabled state, HTTP server endpoint/port, shared fields,
// and live per-provider status.
func FetchProviders(ctx context.Context, opts Options) ([]ProviderInfo, error) {
	var cfg *config.Config
	if strings.TrimSpace(opts.RouterConfig) != "" {
		loaded, err := config.Load(opts.RouterConfig)
		if err != nil {
			return nil, err
		}
		cfg = loaded
	}

	diagStatus := fetchProviderDiagnostics(ctx, opts)
	builtins := provider.BuiltinProviders()
	items := make([]ProviderInfo, 0, len(builtins))

	for _, kind := range builtins {
		switch kind {
		case provider.KindCLIProxyAPI:
			items = append(items, buildCLIProxyInfo(ctx, cfg, diagStatus))
		case provider.KindLlamaCpp:
			items = append(items, buildLocalFleetInfo(ctx, provider.KindLlamaCpp, cfg, diagStatus))
		case provider.KindStrata:
			items = append(items, buildLocalFleetInfo(ctx, provider.KindStrata, cfg, diagStatus))
		}
	}
	return items, nil
}

// FetchProvider returns the sanitized ProviderInfo for a single built-in provider.
func FetchProvider(ctx context.Context, opts Options, name string) (ProviderInfo, error) {
	canon := NormalizeProviderName(name)
	items, err := FetchProviders(ctx, opts)
	if err != nil {
		return ProviderInfo{}, err
	}
	for _, item := range items {
		if item.Name == canon {
			return item, nil
		}
	}
	return ProviderInfo{}, fmt.Errorf("unknown provider %q (available: %s)", name, strings.Join(provider.BuiltinProviders(), ", "))
}

func buildCLIProxyInfo(ctx context.Context, cfg *config.Config, diag map[string]string) ProviderInfo {
	info := ProviderInfo{
		Name:              provider.KindCLIProxyAPI,
		Enabled:           false,
		Status:            "disabled",
		DefaultDialect:    provider.DialectOpenAIChat,
		SupportedDialects: []string{provider.DialectOpenAIChat, provider.DialectOpenAIResponses, provider.DialectAnthropic},
	}
	if cfg == nil || cfg.CLIProxyAPI == nil {
		return info
	}
	p := cfg.Providers[provider.CLIProxyAPIProviderName]
	if p == nil {
		p = cfg.CLIProxyAPI.Provider()
	}
	info.Enabled = true
	if strings.TrimSpace(cfg.CLIProxyAPI.ConfigPath) != "" {
		info.Mode = "embedded"
	} else {
		info.Mode = "external"
	}
	applyCommonSpec(&info, p, cfg.CLIProxyAPI.ProbeTimeout)
	if st, ok := diag[p.Name]; ok && st != "" {
		info.Status = st
	} else {
		info.Status = probeEndpointStatus(ctx, info.Host, info.Port, info.ProbeTimeoutSeconds)
	}
	return info
}

func buildLocalFleetInfo(ctx context.Context, kind string, cfg *config.Config, diag map[string]string) ProviderInfo {
	info := ProviderInfo{
		Name:              kind,
		Enabled:           false,
		Status:            "disabled",
		DefaultDialect:    provider.DialectOpenAIChat,
		SupportedDialects: []string{provider.DialectOpenAIChat, provider.DialectAnthropic},
	}
	if cfg == nil || cfg.Llama == nil {
		return info
	}

	var matched []config.LlamaServer
	for _, s := range cfg.Llama.Servers {
		sKind := s.Kind
		if sKind == "" {
			sKind = provider.KindLlamaCpp
		}
		if sKind == kind {
			matched = append(matched, s)
		}
	}
	if len(matched) == 0 {
		return info
	}

	info.Enabled = true
	provs := cfg.LlamaProviders()
	first := matched[0]
	if p := provs[first.Name]; p != nil {
		applyCommonSpec(&info, p, first.ProbeTimeout)
	} else {
		sanitized, scheme, host, port := parseProviderEndpoint(first.BaseURL)
		info.BaseURL = sanitized
		info.Scheme = scheme
		info.Host = host
		info.Port = port
		info.TimeoutSeconds = first.Timeout
		info.ProbeTimeoutSeconds = first.ProbeTimeout
		info.APIKeyConfigured = strings.TrimSpace(first.APIKey) != ""
	}
	info.Prefix = first.Prefix

	anyUp := false
	allUp := true
	endpoints := make([]ProviderEndpoint, 0, len(matched))
	for _, s := range matched {
		sanitized, scheme, host, port := parseProviderEndpoint(s.BaseURL)
		st := ""
		if d, ok := diag[s.Name]; ok && d != "" {
			st = d
		} else {
			st = probeEndpointStatus(ctx, host, port, s.ProbeTimeout)
		}
		if st == "available" || st == "reachable" {
			anyUp = true
		} else {
			allUp = false
		}
		endpoints = append(endpoints, ProviderEndpoint{
			Name:    s.Name,
			BaseURL: sanitized,
			Scheme:  scheme,
			Host:    host,
			Port:    port,
			Status:  st,
		})
	}
	info.Servers = endpoints
	switch {
	case len(endpoints) == 1:
		info.Status = endpoints[0].Status
	case allUp:
		info.Status = "reachable"
	case anyUp:
		info.Status = "degraded"
	default:
		info.Status = "unreachable"
	}
	return info
}

func applyCommonSpec(info *ProviderInfo, p *config.Provider, probeTimeout float64) {
	if p == nil {
		return
	}
	compat := "strict"
	if p.IsLossy() {
		compat = "lossy"
	} else if p.CompatibilityRaw != "" {
		compat = p.CompatibilityRaw
	}
	auth := p.AuthHeader
	if auth == "" {
		auth = "auto"
	}
	supported := append([]string(nil), p.SupportedDialects...)
	if len(supported) == 0 && p.DefaultDialect != "" {
		supported = []string{p.DefaultDialect}
	}
	sanitized, scheme, host, port := parseProviderEndpoint(p.BaseURL)
	info.BaseURL = sanitized
	info.Scheme = scheme
	info.Host = host
	info.Port = port
	info.DefaultDialect = p.DefaultDialect
	info.SupportedDialects = supported
	info.Compatibility = compat
	info.TimeoutSeconds = p.Timeout
	info.ProbeTimeoutSeconds = probeTimeout
	info.AuthHeader = auth
	info.APIKeyConfigured = strings.TrimSpace(p.APIKey) != ""
}

func parseProviderEndpoint(raw string) (sanitized, scheme, host string, port int) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", "", "", 0
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", "", "", 0
	}
	clean := *u
	clean.User = nil
	clean.RawQuery = ""
	clean.Fragment = ""

	scheme = strings.ToLower(clean.Scheme)
	host = clean.Hostname()
	if p := clean.Port(); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n > 0 {
			port = n
		}
	} else {
		switch scheme {
		case "http":
			port = 80
		case "https":
			port = 443
		}
	}
	return clean.String(), scheme, host, port
}

func fetchProviderDiagnostics(ctx context.Context, opts Options) map[string]string {
	base := baseURL(opts)
	if base == "" {
		return nil
	}
	code, body, err := doGet(ctx, httpClient(opts), base+"/diagnostics", opts.Router.APIKey)
	if err != nil || code != 200 {
		return nil
	}
	var payload struct {
		ProviderDetails []struct {
			Name         string `json:"name"`
			Reachability string `json:"reachability"`
		} `json:"provider_details"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	out := make(map[string]string, len(payload.ProviderDetails))
	for _, p := range payload.ProviderDetails {
		if p.Name != "" && p.Reachability != "" && p.Reachability != "unprobed" {
			out[p.Name] = p.Reachability
		}
	}
	return out
}

func probeEndpointStatus(ctx context.Context, host string, port int, probeTimeoutSec float64) string {
	if !isLoopbackHost(host) || port <= 0 {
		return "configured"
	}
	timeout := time.Second
	if probeTimeoutSec > 0 && probeTimeoutSec < 2.0 {
		timeout = time.Duration(probeTimeoutSec * float64(time.Second))
	}
	dialer := net.Dialer{Timeout: timeout}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "unreachable"
	}
	_ = conn.Close()
	return "reachable"
}

func isLoopbackHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}
