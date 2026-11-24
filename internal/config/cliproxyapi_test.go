package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/arhammahajan/router/internal/provider"
)

func proxyMinimal(extra string) string {
	return `
[server]
auth_mode = "none"
allow_dynamic_routes = true
[retry]
max_attempts = 1
` + extra
}

func TestCLIProxyAPIDefaults(t *testing.T) {
	cfg, err := Load(writeCfg(t, proxyMinimal("[providers.cliproxyapi]\n")))
	if err != nil {
		t.Fatalf("bare [providers.cliproxyapi] must load with defaults: %v", err)
	}
	if cfg.CLIProxyAPI == nil {
		t.Fatal("CLIProxyAPI must be present")
	}
	if cfg.CLIProxyAPI.BaseURL != DefaultCLIProxyAPIBaseURL {
		t.Fatalf("default base_url = %q, want %q", cfg.CLIProxyAPI.BaseURL, DefaultCLIProxyAPIBaseURL)
	}
	p := cfg.CLIProxyProvider()
	if p == nil || p.Name != CLIProxyAPIProviderName || p.Kind != provider.KindCLIProxyAPI || p.DefaultDialect != DialectOpenAIChat {
		t.Fatalf("generated provider wrong: %+v", p)
	}
	if p.BaseURL != DefaultCLIProxyAPIBaseURL || p.LitellmProvider != "openai" || p.AuthHeader != "" {
		t.Fatalf("generated provider endpoint/auth wrong: %+v", p)
	}
	if want := []string{DialectOpenAIChat}; !reflect.DeepEqual(p.SupportedDialects, want) {
		t.Fatalf("default SupportedDialects = %v, want %v", p.SupportedDialects, want)
	}
	if cfg.Catalog == nil || cfg.Catalog.RefreshSeconds != DefaultCLIProxyAPIRefreshSeconds {
		t.Fatalf("default proxy catalog refresh = %+v, want %v", cfg.Catalog, DefaultCLIProxyAPIRefreshSeconds)
	}
	if cfg.Catalog.Timeout != DefaultCLIProxyAPIProbeTimeout {
		t.Fatalf("default proxy catalog timeout = %v, want %v", cfg.Catalog.Timeout, DefaultCLIProxyAPIProbeTimeout)
	}
	if _, ok := cfg.Providers[CLIProxyAPIProviderName]; !ok {
		t.Fatal("generated provider must be present in Providers")
	}
}

func TestCLIProxyAPIPassthroughAndAuthHeader(t *testing.T) {
	body := proxyMinimal("[providers.cliproxyapi]\npassthrough = true\nprobe_timeout = 12\ntimeout = 120\n")
	cfg, err := Load(writeCfg(t, body))
	if err != nil {
		t.Fatalf("passthrough config must load: %v", err)
	}
	p := cfg.Providers[CLIProxyAPIProviderName]
	want := []string{DialectOpenAIChat, DialectOpenAIResponses, DialectAnthropic}
	if !reflect.DeepEqual(p.SupportedDialects, want) {
		t.Fatalf("SupportedDialects = %v, want %v", p.SupportedDialects, want)
	}
	if p.AuthHeader != "" {
		t.Fatalf("AuthHeader must stay empty by default, got %q", p.AuthHeader)
	}
	if p.Timeout != 120 {
		t.Fatalf("Timeout = %v, want 120", p.Timeout)
	}
	if cfg.Catalog.Timeout != 12 {
		t.Fatalf("Catalog.Timeout = %v, want 12", cfg.Catalog.Timeout)
	}
}

func TestCLIProxyAPIContract(t *testing.T) {
	t.Setenv("CLIPROXYAPI_API_KEY", "proxy-secret")
	body := proxyMinimal("[providers.cliproxyapi]\nbase_url = \"http://127.0.0.1:8317/v1\"\napi_key = \"${CLIPROXYAPI_API_KEY}\"\n")
	cfg, err := Load(writeCfg(t, body))
	if err != nil {
		t.Fatalf("contract load: %v", err)
	}
	if cfg.CLIProxyAPI.BaseURL != "http://127.0.0.1:8317/v1" {
		t.Fatalf("base_url = %q", cfg.CLIProxyAPI.BaseURL)
	}
	if cfg.CLIProxyAPI.APIKey != "proxy-secret" {
		t.Fatalf("api_key not resolved: %q", cfg.CLIProxyAPI.APIKey)
	}
	if cfg.CLIProxyProvider().EffectiveKey() != "proxy-secret" {
		t.Fatalf("effective key wrong: %q", cfg.CLIProxyProvider().EffectiveKey())
	}
}

func TestCLIProxyAPIExplicitRouteKeepsDialectOverride(t *testing.T) {
	body := proxyMinimal("[providers.cliproxyapi]\nbase_url = \"http://127.0.0.1:8317/v1\"\n[[models]]\nname = \"chat-route\"\nprovider = \"cliproxyapi\"\nmodel = \"opencode-go/glm-5.3\"\ndialect = \"anthropic\"\n")
	cfg, err := Load(writeCfg(t, body))
	if err != nil {
		t.Fatalf("explicit route on generated provider must load: %v", err)
	}
	m, p, ok := cfg.LookupModel("chat-route")
	if !ok {
		t.Fatal("explicit route missing")
	}
	if m.Upstream != "opencode-go/glm-5.3" {
		t.Fatalf("opaque upstream lost: %+v", m)
	}
	if got := m.EffectiveDialect(p); got != DialectAnthropic {
		t.Fatalf("translation override lost: %q", got)
	}
	if p.DefaultDialect != DialectOpenAIChat {
		t.Fatalf("provider dialect must stay openai-chat, got %q", p.DefaultDialect)
	}
}

func TestCLIProxyAPIRejectsTopLevelTable(t *testing.T) {
	body := proxyMinimal("[cliproxyapi]\n")
	err := func() error {
		_, err := Load(writeCfg(t, body))
		return err
	}()
	if err == nil {
		t.Fatal("top-level [cliproxyapi] must be rejected")
	}
	if !strings.Contains(err.Error(), "providers.cliproxyapi") {
		t.Fatalf("expected migration hint to [providers.cliproxyapi], got: %v", err)
	}
}

func TestCLIProxyAPIRejectsCatalogCombination(t *testing.T) {
	body := proxyMinimal("[providers.cliproxyapi]\n[catalog]\nurl = \"http://127.0.0.1:9\"\nmodel_providers = { m = \"cliproxyapi\" }\n")
	if _, err := Load(writeCfg(t, body)); err == nil {
		t.Fatal("[catalog] with [providers.cliproxyapi] must fail (shared layer)")
	}
}

func TestCLIProxyAPIRejectsBadBaseURL(t *testing.T) {
	for _, u := range []string{"ftp://x/v1", "http://user:pass@h/v1", "http://h/v1?x=1", ""} {
		body := proxyMinimal("[providers.cliproxyapi]\nbase_url = \"" + u + "\"\n")
		if _, err := Load(writeCfg(t, body)); err == nil {
			t.Fatalf("bad base_url %q must fail", u)
		} else if strings.Contains(err.Error(), "pass") {
			t.Fatalf("error echoes secret: %v", err)
		}
	}
}

func TestCLIProxyAPIConfigPathOptional(t *testing.T) {
	cfg, err := Load(writeCfg(t, proxyMinimal("[providers.cliproxyapi]\nconfig_path = \"/tmp/sdk.yaml\"\n")))
	if err != nil {
		t.Fatalf("config_path must be accepted: %v", err)
	}
	if cfg.CLIProxyAPI.ConfigPath != "/tmp/sdk.yaml" {
		t.Fatalf("config_path lost: %q", cfg.CLIProxyAPI.ConfigPath)
	}
	cfg, err = Load(writeCfg(t, proxyMinimal("[providers.cliproxyapi]\n")))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CLIProxyAPI.ConfigPath != "" {
		t.Fatalf("omitted config_path must stay empty: %q", cfg.CLIProxyAPI.ConfigPath)
	}
}

func TestCLIProxyAPICompatibility(t *testing.T) {
	cfg, err := Load(writeCfg(t, proxyMinimal("[providers.cliproxyapi]\ncompatibility = \"lossy\"\n")))
	if err != nil {
		t.Fatalf("compatibility=lossy must be accepted: %v", err)
	}
	p := cfg.CLIProxyProvider()
	if !p.Lossy || p.CompatibilityRaw != "lossy" {
		t.Fatalf("provider lossy=%v raw=%q, want true/lossy", p.Lossy, p.CompatibilityRaw)
	}
	cfg, err = Load(writeCfg(t, proxyMinimal("[providers.cliproxyapi]\ncompatibility = \"strict\"\n")))
	if err != nil {
		t.Fatalf("compatibility=strict must be accepted: %v", err)
	}
	if cfg.CLIProxyProvider().Lossy || cfg.CLIProxyProvider().CompatibilityRaw != "strict" {
		t.Fatalf("strict must leave Lossy=false")
	}
	if _, err := Load(writeCfg(t, proxyMinimal("[providers.cliproxyapi]\ncompatibility = \"invalid\"\n"))); err == nil {
		t.Fatal("invalid compatibility must fail")
	}
}

func TestCLIProxyAPIRefreshSeconds(t *testing.T) {
	cfg, err := Load(writeCfg(t, proxyMinimal("[providers.cliproxyapi]\nrefresh_seconds = 60\n")))
	if err != nil {
		t.Fatalf("refresh_seconds=60 must load: %v", err)
	}
	if cfg.CLIProxyAPI.RefreshSeconds != 60 || cfg.Catalog == nil || cfg.Catalog.RefreshSeconds != 60 {
		t.Fatalf("refresh_seconds = %v / catalog=%+v, want 60", cfg.CLIProxyAPI.RefreshSeconds, cfg.Catalog)
	}

	cfg, err = Load(writeCfg(t, proxyMinimal("[providers.cliproxyapi]\nrefresh_seconds = 0\n")))
	if err != nil {
		t.Fatalf("refresh_seconds=0 must load: %v", err)
	}
	if cfg.CLIProxyAPI.RefreshSeconds != 0 || cfg.Catalog == nil || cfg.Catalog.RefreshSeconds != 0 {
		t.Fatalf("refresh_seconds = %v / catalog=%+v, want 0", cfg.CLIProxyAPI.RefreshSeconds, cfg.Catalog)
	}

	if _, err := Load(writeCfg(t, proxyMinimal("[providers.cliproxyapi]\nrefresh_seconds = -1\n"))); err == nil {
		t.Fatal("negative refresh_seconds must fail")
	}
}
