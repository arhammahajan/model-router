// General, thin provider inspection for the router CLI.
//
// Reads the configured providers (generic [providers.*], [providers.cliproxyapi],
// and expanded [providers.llama_cpp] / [providers.strata] local servers) and
// enriches each with sanitized endpoint/port metadata and per-provider status
// from router /diagnostics (or a bounded local probe when /diagnostics is
// unavailable). Credentials and raw header values are never exposed.
package routerctl

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/provider"
)

// ProviderInfo is a sanitized, uniform summary of one configured provider.
type ProviderInfo struct {
	Name                string   `json:"name"`
	Kind                string   `json:"kind"`
	Status              string   `json:"status"`
	BaseURL             string   `json:"base_url"`
	Scheme              string   `json:"scheme,omitempty"`
	Host                string   `json:"host,omitempty"`
	Port                int      `json:"port,omitempty"`
	DefaultDialect      string   `json:"default_dialect"`
	SupportedDialects   []string `json:"supported_dialects"`
	Compatibility       string   `json:"compatibility"`
	TimeoutSeconds      float64  `json:"timeout_seconds"`
	ProbeTimeoutSeconds float64  `json:"probe_timeout_seconds,omitempty"`
	AuthHeader          string   `json:"auth_header"`
	APIKeyConfigured    bool     `json:"api_key_configured"`
	Mode                string   `json:"mode,omitempty"`
	Prefix              string   `json:"prefix,omitempty"`
}

// FetchProviders loads the router config from opts.RouterConfig, collects every
// configured provider (static providers, cliproxyapi, and expanded local
// llama_cpp/strata servers), and attaches per-provider status.
func FetchProviders(ctx context.Context, opts Options) ([]ProviderInfo, error) {
	if strings.TrimSpace(opts.RouterConfig) == "" {
		return nil, fmt.Errorf("no router config found; pass --config PATH")
	}
	cfg, err := config.Load(opts.RouterConfig)
	if err != nil {
		return nil, err
	}

	diagStatus := fetchProviderDiagnostics(ctx, opts)

	items := make([]ProviderInfo, 0, len(cfg.Providers))
	for _, p := range cfg.Providers {
		if p == nil {
			continue
		}
		info := buildProviderInfo(p, 0, "", "")
		if p.Kind == provider.KindCLIProxyAPI && cfg.CLIProxyAPI != nil {
			info.ProbeTimeoutSeconds = cfg.CLIProxyAPI.ProbeTimeout
			if strings.TrimSpace(cfg.CLIProxyAPI.ConfigPath) != "" {
				info.Mode = "embedded"
			} else {
				info.Mode = "external"
			}
		}
		items = append(items, info)
	}

	if cfg.Llama != nil {
		serverByName := make(map[string]config.LlamaServer, len(cfg.Llama.Servers))
		for _, s := range cfg.Llama.Servers {
			serverByName[s.Name] = s
		}
		for _, p := range cfg.LlamaProviders() {
			if p == nil {
				continue
			}
			srv := serverByName[p.Name]
			info := buildProviderInfo(p, srv.ProbeTimeout, "", srv.Prefix)
			items = append(items, info)
		}
	}

	sort.Slice(items, func(i, j int) bool {
		return items[i].Name < items[j].Name
	})

	for i := range items {
		if st, ok := diagStatus[items[i].Name]; ok && st != "" {
			items[i].Status = st
			continue
		}
		items[i].Status = probeProviderStatus(ctx, items[i])
	}

	return items, nil
}

// FetchProvider returns the sanitized ProviderInfo for a single named provider.
func FetchProvider(ctx context.Context, opts Options, name string) (ProviderInfo, error) {
	items, err := FetchProviders(ctx, opts)
	if err != nil {
		return ProviderInfo{}, err
	}
	for _, item := range items {
		if item.Name == name {
			return item, nil
		}
	}
	return ProviderInfo{}, fmt.Errorf("provider %q not found in %s", name, opts.RouterConfig)
}

func buildProviderInfo(p *config.Provider, probeTimeout float64, mode, prefix string) ProviderInfo {
	kind := p.Kind
	if kind == "" {
		kind = provider.KindGeneric
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
	return ProviderInfo{
		Name:                p.Name,
		Kind:                kind,
		BaseURL:             sanitized,
		Scheme:              scheme,
		Host:                host,
		Port:                port,
		DefaultDialect:      p.DefaultDialect,
		SupportedDialects:   supported,
		Compatibility:       compat,
		TimeoutSeconds:      p.Timeout,
		ProbeTimeoutSeconds: probeTimeout,
		AuthHeader:          auth,
		APIKeyConfigured:    strings.TrimSpace(p.APIKey) != "",
		Mode:                mode,
		Prefix:              prefix,
	}
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
		Providers map[string]struct {
			Reachability string `json:"reachability"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	out := make(map[string]string, len(payload.Providers))
	for name, p := range payload.Providers {
		if p.Reachability != "" {
			out[name] = p.Reachability
		}
	}
	return out
}

func probeProviderStatus(ctx context.Context, info ProviderInfo) string {
	if !isLoopbackHost(info.Host) || info.Port <= 0 {
		return "configured"
	}
	timeout := time.Second
	if info.ProbeTimeoutSeconds > 0 && info.ProbeTimeoutSeconds < 2.0 {
		timeout = time.Duration(info.ProbeTimeoutSeconds * float64(time.Second))
	}
	dialer := net.Dialer{Timeout: timeout}
	addr := net.JoinHostPort(info.Host, strconv.Itoa(info.Port))
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
