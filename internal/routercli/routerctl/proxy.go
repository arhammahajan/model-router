// Proxy management surface for the router CLI.
//
// The model-router daemon owns translation and discovery over the proxy's
// OpenAI-compatible endpoint; upstream credentials and plugins belong to
// CLIProxyAPI. This file gives the CLI a read-only view of that proxy
// (status, model inventory, plugin inventory, account summary) plus one
// explicit plugin enable/disable control, all against upstream management
// endpoints verified in the CLIProxyAPI v8 source:
//
//   - GET {proxy-base}/models with the proxy inference key (OpenAI list)
//   - GET /v0/management/plugins with the management key
//   - PATCH /v0/management/plugins/:id/enabled {"enabled":bool}
//   - GET /v0/management/auth-files with the management key
//
// Management authentication accepts "Authorization: Bearer <key>" or the
// "X-Management-Key" header. The management key comes only from the
// CLIPROXYAPI_MANAGEMENT_KEY environment variable: it is never read from
// the router config and the proxy inference key is never substituted, so
// the two credentials cannot be confused. No key, token, or raw auth-file
// content is ever written to terminal output, JSON output, or errors:
// account and plugin payloads are projected onto small safe structs and
// raw bodies are discarded.
package routerctl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/arhammahajan/router/internal/cliproxy"
	"github.com/arhammahajan/router/internal/config"
)

// ProxyManagementKeyEnv is the only source of the proxy management key.
// The router config still carries only the proxy inference key.
const ProxyManagementKeyEnv = "CLIPROXYAPI_MANAGEMENT_KEY"

// Proxy management route paths, relative to the proxy origin (the
// configured base URL with any trailing "/v1" stripped). Verified against
// the CLIProxyAPI v8 management route table; the plugin enabled control
// exists only on the v0 surface, so v0 is used throughout for consistency.
const (
	proxyManagementPath = "/v0/management"
)

// ProxyModeName reports the configured proxy lifecycle for display and
// JSON: "disabled", "external", or "embedded".
func ProxyModeName(m cliproxy.Mode) string {
	switch m {
	case cliproxy.Embedded:
		return "embedded"
	case cliproxy.External:
		return "external"
	default:
		return "disabled"
	}
}

// ProxyEndpoint is the resolved proxy surface: sanitized URLs plus key
// presence flags. Credential fields are excluded from JSON.
type ProxyEndpoint struct {
	Mode               string // "disabled", "external", "embedded"
	BaseURL            string // sanitized proxy OpenAI base, "" when disabled
	ManagementURL      string // sanitized ".../v0/management", "" when disabled
	InferenceKey       string `json:"-"` // proxy api_key from the router config ("")
	ManagementKey      string `json:"-"` // from CLIPROXYAPI_MANAGEMENT_KEY ("")
	HasInferenceKey    bool
	HasManagementKey   bool
	EmbeddedConfigPath string // CLIProxyAPI YAML path, "" unless embedded
}

// ProxySettings resolves the [cliproxyapi] table from the router config
// file named by opts plus the management key environment variable. A
// missing [cliproxyapi] table resolves to Mode "disabled" without an
// error: the caller reports the state instead of failing.
func ProxySettings(opts Options) (ProxyEndpoint, error) {
	ep := ProxyEndpoint{Mode: "disabled"}
	path := strings.TrimSpace(opts.RouterConfig)
	if path == "" {
		return ep, fmt.Errorf("no router config found; pass --config PATH")
	}
	cfg, err := config.LoadWithOptions(path, config.LoadOptions{})
	if err != nil {
		return ep, fmt.Errorf("load router config %s: %w", path, err)
	}
	if cfg.CLIProxyAPI == nil {
		return ep, nil
	}
	settings := cliproxy.FromFields(cfg.CLIProxyAPI.BaseURL, cfg.CLIProxyAPI.APIKey, cfg.CLIProxyAPI.ConfigPath)
	ep.Mode = ProxyModeName(settings.Mode())
	ep.BaseURL = sanitizeProxyURL(settings.BaseURL)
	ep.ManagementURL = proxyManagementURL(settings.BaseURL)
	ep.InferenceKey = strings.TrimSpace(settings.APIKey)
	ep.HasInferenceKey = ep.InferenceKey != ""
	ep.EmbeddedConfigPath = strings.TrimSpace(settings.ConfigPath)
	if key := proxyManagementKey(); key != "" {
		ep.ManagementKey = key
		ep.HasManagementKey = true
	}
	return ep, nil
}

// proxyManagementKey reads the management key from its dedicated
// environment variable. Anything else (including the proxy inference
// key) is never accepted: management auth must stay separate.
func proxyManagementKey() string {
	return strings.TrimSpace(os.Getenv(ProxyManagementKeyEnv))
}

// managementKeyOrError returns the configured management key or a usage
// error that names the variable without echoing any secret.
func managementKeyOrError(ep ProxyEndpoint) (string, error) {
	if ep.Mode == "disabled" {
		return "", fmt.Errorf("proxy is disabled: no [cliproxyapi] in the router config")
	}
	if !ep.HasManagementKey {
		return "", fmt.Errorf("proxy management key is not configured: set %s (it is separate from the proxy inference key)", ProxyManagementKeyEnv)
	}
	return ep.ManagementKey, nil
}

// sanitizeProxyURL renders a configured endpoint for display, JSON, and
// error text: surrounding whitespace and a trailing slash go away, and
// any userinfo is dropped so credentials can never leak into output.
func sanitizeProxyURL(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		u.User = nil
		return strings.TrimRight(u.String(), "/")
	}
	return strings.TrimRight(s, "/")
}

// proxyManagementURL derives the management root from the configured
// proxy base URL: the proxy serves the OpenAI API under an optional
// "/v1" suffix and the management API at the origin, so a trailing
// "/v1" path segment is stripped before appending the management path.
func proxyManagementURL(base string) string {
	s := strings.TrimRight(strings.TrimSpace(base), "/")
	if s == "" {
		return ""
	}
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		u.User = nil
		u.RawQuery = ""
		u.Fragment = ""
		p := strings.TrimRight(u.Path, "/")
		if p == "/v1" {
			p = ""
		} else {
			p = strings.TrimSuffix(p, "/v1")
		}
		u.Path = p
		return strings.TrimRight(u.String(), "/") + proxyManagementPath
	}
	trimmed := strings.TrimSuffix(s, "/v1")
	return strings.TrimRight(trimmed, "/") + proxyManagementPath
}

func proxyHTTPClient(opts Options) *http.Client {
	return &http.Client{
		Timeout: opts.httpTimeout(),
		// Never follow redirects while holding credentials: a 3xx that
		// points elsewhere must surface as a response, not as a
		// credential leak to another host.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// proxyGet performs an authenticated GET and returns the status code and
// a bounded body. Errors name the sanitized URL and the failure class,
// never the key or the body.
func proxyGet(ctx context.Context, client *http.Client, url, key, what string) (int, []byte, error) {
	code, body, err := proxyDo(ctx, client, http.MethodGet, url, key, nil, what)
	return code, body, err
}

func proxyDo(ctx context.Context, client *http.Client, method, url, key string, payload []byte, what string) (int, []byte, error) {
	var rdr io.Reader
	if payload != nil {
		rdr = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return 0, nil, fmt.Errorf("proxy %s: invalid endpoint: %w", what, err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("proxy %s: %v", what, sanitizedClientError(err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("proxy %s: status %d: read body: %v", what, resp.StatusCode, err)
	}
	return resp.StatusCode, body, nil
}

// sanitizedClientError flattens *url.Error chains to the operation and
// sanitized address without internal detail that could echo secrets.
func sanitizedClientError(err error) string {
	if uerr, ok := err.(*url.Error); ok {
		sanitized := sanitizeProxyURL(strings.TrimSpace(uerr.URL))
		if sanitized == "" {
			sanitized = "proxy endpoint"
		}
		op := strings.ToLower(string(uerr.Op))
		if strings.Contains(op, "dial") || strings.Contains(op, "connect") {
			return fmt.Sprintf("%s: connection refused or unreachable", sanitized)
		}
		if osIsTimeout(uerr) {
			return fmt.Sprintf("%s: timed out", sanitized)
		}
		return fmt.Sprintf("%s: %s failed", sanitized, uerr.Op)
	}
	return strings.TrimSpace(err.Error())
}

// ProxyModel is one proxy model entry: the opaque upstream ID plus the
// provider metadata the proxy reports. IDs are passed through untouched.
type ProxyModel struct {
	ID      string `json:"id"`
	OwnedBy string `json:"owned_by,omitempty"`
}

// ListProxyModels returns the proxy's own GET {base}/models listing using
// the configured proxy inference key. It tolerates both {"data": [...]}
// and bare [...] bodies and skips rows without an id.
func ListProxyModels(ctx context.Context, opts Options) ([]ProxyModel, error) {
	ep, err := ProxySettings(opts)
	if err != nil {
		return nil, err
	}
	if ep.Mode == "disabled" {
		return nil, fmt.Errorf("proxy is disabled: no [cliproxyapi] in the router config")
	}
	code, body, err := proxyGet(ctx, proxyHTTPClient(opts), ep.BaseURL+"/models", ep.InferenceKey, "models")
	if err != nil {
		return nil, err
	}
	switch code {
	case http.StatusOK:
		return parseProxyModels(body)
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("proxy models: status %d: proxy inference key rejected", code)
	default:
		return nil, fmt.Errorf("proxy models: status %d", code)
	}
}

func parseProxyModels(body []byte) ([]ProxyModel, error) {
	var raw json.RawMessage = body
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && len(envelope.Data) > 0 {
		raw = envelope.Data
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, fmt.Errorf("proxy models listing must contain an array")
	}
	var items []struct {
		ID      string `json:"id"`
		OwnedBy string `json:"owned_by"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("parse proxy models listing: %w", err)
	}
	out := make([]ProxyModel, 0, len(items))
	for _, it := range items {
		if strings.TrimSpace(it.ID) == "" {
			continue
		}
		out = append(out, ProxyModel{ID: it.ID, OwnedBy: strings.TrimSpace(it.OwnedBy)})
	}
	return out, nil
}

// ProxyStatus is the point-in-time proxy overview for `router proxy
// status`. Degradation is expressed in the fields, never as an error,
// except for config load failures: a missing [cliproxyapi] table is the
// Disabled mode, an unreachable proxy flips Reachable, and management
// state (which needs its own key) is reported separately.
type ProxyStatus struct {
	Mode               string   `json:"mode"`
	Endpoint           string   `json:"endpoint,omitempty"`
	ManagementEndpoint string   `json:"management_endpoint,omitempty"`
	HasInferenceKey    bool     `json:"has_inference_key"`
	HasManagementKey   bool     `json:"has_management_key"`
	Reachable          bool     `json:"reachable"`
	Models             int      `json:"models"`
	ModelIDs           []string `json:"model_ids,omitempty"`
	Management         string   `json:"management"`
	Note               string   `json:"note,omitempty"`
}

// FetchProxyStatus resolves the proxy configuration, probes the
// authenticated model endpoint for liveness plus a bounded model count,
// and — only when the management key is configured — probes the plugin
// inventory endpoint for management reachability.
func FetchProxyStatus(ctx context.Context, opts Options) (ProxyStatus, error) {
	ep, err := ProxySettings(opts)
	if err != nil {
		return ProxyStatus{}, err
	}
	st := ProxyStatus{
		Mode:               ep.Mode,
		Endpoint:           ep.BaseURL,
		ManagementEndpoint: ep.ManagementURL,
		HasInferenceKey:    ep.HasInferenceKey,
		HasManagementKey:   ep.HasManagementKey,
	}
	if ep.Mode == "disabled" {
		st.Management = "disabled"
		st.Note = "no [cliproxyapi] in the router config"
		return st, nil
	}
	client := proxyHTTPClient(opts)
	if code, body, err := proxyGet(ctx, client, ep.BaseURL+"/models", ep.InferenceKey, "models"); err == nil {
		switch code {
		case http.StatusOK:
			models, perr := parseProxyModels(body)
			if perr != nil {
				st.Note = "proxy models endpoint returned an invalid model list"
			} else {
				st.Reachable = true
				st.Models = len(models)
				ids := make([]string, 0, len(models))
				for _, m := range models {
					ids = append(ids, m.ID)
				}
				sort.Strings(ids)
				if len(ids) > 200 {
					ids = ids[:200]
				}
				st.ModelIDs = ids
			}
		case http.StatusUnauthorized, http.StatusForbidden:
			st.Note = "proxy inference key rejected"
		default:
			st.Note = fmt.Sprintf("proxy models endpoint returned status %d", code)
		}
	} else {
		st.Note = strings.TrimPrefix(err.Error(), "proxy models: ")
	}
	st.Management = probeProxyManagement(ctx, client, ep)
	return st, nil
}

// probeProxyManagement classifies management availability without
// leaking secrets: "ok", "missing-key", "unauthorized", "unreachable",
// or "unavailable" (any other non-200 state).
func probeProxyManagement(ctx context.Context, client *http.Client, ep ProxyEndpoint) string {
	if ep.Mode == "disabled" {
		return "disabled"
	}
	key, err := managementKeyOrError(ep)
	if err != nil {
		return "missing-key"
	}
	code, _, err := proxyGet(ctx, client, ep.ManagementURL+"/plugins", key, "plugins")
	if err != nil {
		return "unreachable"
	}
	switch code {
	case http.StatusOK:
		return "ok"
	case http.StatusUnauthorized, http.StatusForbidden:
		return "unauthorized"
	case http.StatusNotFound:
		return "unavailable"
	default:
		return "unavailable"
	}
}

// ProxyPlugin is the safe projection of one management plugin entry:
// identity plus capability flags. Descriptions, config values, and other
// operator content stay server-side.
type ProxyPlugin struct {
	ID               string   `json:"id"`
	Name             string   `json:"name,omitempty"`
	Version          string   `json:"version,omitempty"`
	Enabled          bool     `json:"enabled"`
	EffectiveEnabled bool     `json:"effective_enabled"`
	Registered       bool     `json:"registered"`
	Configured       bool     `json:"configured"`
	SupportsOAuth    bool     `json:"supports_oauth"`
	OAuthProviders   []string `json:"oauth_providers,omitempty"`
	SupportsQuota    bool     `json:"supports_quota"`
}

// ListProxyPlugins returns the proxy plugin inventory from GET
// /v0/management/plugins. The management key is required.
func ListProxyPlugins(ctx context.Context, opts Options) ([]ProxyPlugin, error) {
	ep, err := ProxySettings(opts)
	if err != nil {
		return nil, err
	}
	key, err := managementKeyOrError(ep)
	if err != nil {
		return nil, err
	}
	code, body, err := proxyGet(ctx, proxyHTTPClient(opts), ep.ManagementURL+"/plugins", key, "plugins")
	if err != nil {
		return nil, err
	}
	switch code {
	case http.StatusOK:
		return parseProxyPlugins(body)
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("proxy plugins: status %d: management key rejected", code)
	case http.StatusNotFound:
		return nil, fmt.Errorf("proxy plugins: status 404: management API unavailable on this proxy")
	default:
		return nil, fmt.Errorf("proxy plugins: status %d", code)
	}
}

func parseProxyPlugins(body []byte) ([]ProxyPlugin, error) {
	var decoded struct {
		Plugins []struct {
			ID               string   `json:"id"`
			Enabled          bool     `json:"enabled"`
			EffectiveEnabled bool     `json:"effective_enabled"`
			Registered       bool     `json:"registered"`
			Configured       bool     `json:"configured"`
			SupportsOAuth    bool     `json:"supports_oauth"`
			OAuthProvider    string   `json:"oauth_provider"`
			OAuthProviders   []string `json:"oauth_providers"`
			SupportsQuota    bool     `json:"supports_quota"`
			Metadata         *struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"metadata"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("parse proxy plugins listing: %w", err)
	}
	out := make([]ProxyPlugin, 0, len(decoded.Plugins))
	for _, p := range decoded.Plugins {
		if strings.TrimSpace(p.ID) == "" {
			continue
		}
		providers := append([]string(nil), p.OAuthProviders...)
		if single := strings.TrimSpace(p.OAuthProvider); single != "" {
			found := false
			for _, have := range providers {
				if strings.EqualFold(strings.TrimSpace(have), single) {
					found = true
					break
				}
			}
			if !found {
				providers = append(providers, single)
			}
		}
		sort.Strings(providers)
		plugin := ProxyPlugin{
			ID:               p.ID,
			Enabled:          p.Enabled,
			EffectiveEnabled: p.EffectiveEnabled,
			Registered:       p.Registered,
			Configured:       p.Configured,
			SupportsOAuth:    p.SupportsOAuth,
			OAuthProviders:   providers,
			SupportsQuota:    p.SupportsQuota,
		}
		if p.Metadata != nil {
			plugin.Name = strings.TrimSpace(p.Metadata.Name)
			plugin.Version = strings.TrimSpace(p.Metadata.Version)
		}
		out = append(out, plugin)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// SetProxyPluginEnabled flips one plugin via PATCH
// /v0/management/plugins/:id/enabled {"enabled":...}. The change
// persists proxy-side and takes effect after the proxy reloads; it is an
// explicit operator action with no extra confirmation. The management
// key is required.
func SetProxyPluginEnabled(ctx context.Context, opts Options, id string, enabled bool) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("plugin id is required")
	}
	if strings.ContainsAny(id, "/?#") {
		return fmt.Errorf("invalid plugin id %q", id)
	}
	ep, err := ProxySettings(opts)
	if err != nil {
		return err
	}
	key, err := managementKeyOrError(ep)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]bool{"enabled": enabled})
	if err != nil {
		return fmt.Errorf("encode plugin request: %w", err)
	}
	target := ep.ManagementURL + "/plugins/" + url.PathEscape(id) + "/enabled"
	code, _, err := proxyDo(ctx, proxyHTTPClient(opts), http.MethodPatch, target, key, payload, "plugin")
	if err != nil {
		return err
	}
	switch code {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("proxy plugin %s: status %d: management key rejected", id, code)
	case http.StatusNotFound:
		return fmt.Errorf("proxy plugin %s: not found", id)
	case http.StatusBadRequest:
		return fmt.Errorf("proxy plugin %s: request rejected (status 400)", id)
	default:
		return fmt.Errorf("proxy plugin %s: status %d", id, code)
	}
}

// ProxyAccount is the safe projection of one CLIProxyAPI credential
// entry: identity plus availability flags. Tokens, secrets, file paths,
// sizes, and quota payloads stay server-side; the raw listing body is
// never surfaced.
type ProxyAccount struct {
	Name        string `json:"name"`
	Type        string `json:"type,omitempty"`
	Status      string `json:"status,omitempty"`
	Disabled    bool   `json:"disabled"`
	Unavailable bool   `json:"unavailable"`
	Email       string `json:"email,omitempty"`
	Account     string `json:"account,omitempty"`
}

// ListProxyAccounts returns the sanitized credential summary from GET
// /v0/management/auth-files. The management key is required.
func ListProxyAccounts(ctx context.Context, opts Options) ([]ProxyAccount, error) {
	ep, err := ProxySettings(opts)
	if err != nil {
		return nil, err
	}
	key, err := managementKeyOrError(ep)
	if err != nil {
		return nil, err
	}
	code, body, err := proxyGet(ctx, proxyHTTPClient(opts), ep.ManagementURL+"/auth-files", key, "accounts")
	if err != nil {
		return nil, err
	}
	switch code {
	case http.StatusOK:
		return parseProxyAccounts(body)
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("proxy accounts: status %d: management key rejected", code)
	case http.StatusNotFound:
		return nil, fmt.Errorf("proxy accounts: status 404: management API unavailable on this proxy")
	default:
		return nil, fmt.Errorf("proxy accounts: status %d", code)
	}
}

func parseProxyAccounts(body []byte) ([]ProxyAccount, error) {
	var decoded struct {
		Files []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Type        string `json:"type"`
			Provider    string `json:"provider"`
			Status      string `json:"status"`
			Disabled    bool   `json:"disabled"`
			Unavailable bool   `json:"unavailable"`
			Email       string `json:"email"`
			Account     string `json:"account"`
		} `json:"files"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("parse proxy accounts listing: %w", err)
	}
	out := make([]ProxyAccount, 0, len(decoded.Files))
	for _, f := range decoded.Files {
		name := strings.TrimSpace(f.Name)
		if name == "" {
			name = strings.TrimSpace(f.ID)
		}
		if name == "" {
			continue
		}
		typ := strings.TrimSpace(f.Type)
		if typ == "" {
			typ = strings.TrimSpace(f.Provider)
		}
		out = append(out, ProxyAccount{
			Name:        name,
			Type:        typ,
			Status:      strings.TrimSpace(f.Status),
			Disabled:    f.Disabled,
			Unavailable: f.Unavailable,
			Email:       strings.TrimSpace(f.Email),
			Account:     strings.TrimSpace(f.Account),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// osIsTimeout reports whether err (or its chain) is a deadline timeout,
// without importing net-specific types into the public path.
func osIsTimeout(err error) bool {
	type timeout interface{ Timeout() bool }
	for err != nil {
		if t, ok := err.(timeout); ok && t.Timeout() {
			return true
		}
		uerr, ok := err.(*url.Error)
		if !ok {
			return false
		}
		err = uerr.Err
		if err != nil && strings.Contains(strings.ToLower(err.Error()), "timeout") {
			return true
		}
	}
	return false
}
