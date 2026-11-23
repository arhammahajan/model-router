package cliproxy

import (
	"fmt"
	"net/url"
	"strings"
)

// Mode selects the lifecycle this package performs.
type Mode int

const (
	// Disabled means no [cliproxyapi] settings were provided: no lifecycle,
	// no dialing, no behavior change.
	Disabled Mode = iota
	// External means base_url is set without config_path: a separately
	// running proxy is dialed over HTTP and no SDK Service is managed.
	External
	// Embedded means config_path is set: the SDK Service is built from the
	// YAML file and supervised in-process. Proxy listening, auth and
	// plugins are controlled by that YAML file, not by model-router.
	Embedded
)

// Settings mirrors Config.CLIProxyAPI{BaseURL, APIKey, ConfigPath} field for
// field so the parent wiring stays a one-line translation. It carries no
// upstream OAuth/token material: credentials live in the CLIProxyAPI YAML
// and its auth store, never here.
type Settings struct {
	// BaseURL is the proxy origin the generated provider dials, e.g.
	// "http://127.0.0.1:8317/v1". Required in External mode; in Embedded
	// mode it must match the YAML's listen address when set.
	BaseURL string
	// APIKey is the credential model-router presents to the proxy. It may
	// use ${ENV_VAR} expansion performed by the config layer; this package
	// treats it as opaque and never logs it.
	APIKey string
	// ConfigPath is the optional CLIProxyAPI YAML file enabling the
	// embedded SDK lifecycle. Empty selects External (or Disabled).
	ConfigPath string
}

// FromFields builds Settings from the agreed [cliproxyapi] contract
// fields. The parent wires cfg.CLIProxyAPI through this constructor:
//
//	cliproxy.FromFields(cfg.CLIProxyAPI.BaseURL, cfg.CLIProxyAPI.APIKey, cfg.CLIProxyAPI.ConfigPath)
func FromFields(baseURL, apiKey, configPath string) Settings {
	return Settings{
		BaseURL:    strings.TrimSpace(baseURL),
		APIKey:     strings.TrimSpace(apiKey),
		ConfigPath: strings.TrimSpace(configPath),
	}
}

// Mode reports the lifecycle implied by s. ConfigPath set wins over
// BaseURL: an explicit embedded YAML plus an explicit dial URL is the
// Embedded mode with an operator-asserted address.
func (s Settings) Mode() Mode {
	if strings.TrimSpace(s.ConfigPath) != "" {
		return Embedded
	}
	if strings.TrimSpace(s.BaseURL) != "" {
		return External
	}
	return Disabled
}

// Validate enforces the agreed contract. Messages name fields, never values.
func (s Settings) Validate() error {
	base := strings.TrimSpace(s.BaseURL)
	key := strings.TrimSpace(s.APIKey)
	path := strings.TrimSpace(s.ConfigPath)
	if path == "" && base == "" {
		if key != "" {
			return fmt.Errorf("cliproxy: 'api_key' requires 'base_url' or 'config_path'")
		}
		return nil
	}
	if base != "" {
		if err := checkBaseURL(base); err != nil {
			return err
		}
	}
	if path == "" {
		return nil
	}
	// Embedded: config_path selects the SDK lifecycle; the YAML file owns
	// listening, auth and plugins. Existence/readability is checked at
	// build time so Validate stays free of I/O.
	return nil
}

// checkBaseURL mirrors the router's bare-origin policy for this one field:
// http(s), no userinfo, query or fragment.
func checkBaseURL(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("cliproxy: 'base_url' must start with http:// or https://")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("cliproxy: 'base_url' must start with http:// or https://")
	}
	if u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return fmt.Errorf("cliproxy: 'base_url' must be a bare http(s) origin/path without userinfo, query or fragment")
	}
	return nil
}
