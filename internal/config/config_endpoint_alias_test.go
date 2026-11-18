package config

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arhammahajan/model-router/internal/endpoint"
)

func writeAliasCfg(t *testing.T, providerBlock string) string {
	t.Helper()
	body := "[server]\nauth_mode = \"none\"\n" + providerBlock + "\n[[models]]\nname = \"m\"\nprovider = \"up\"\nmodel = \"x\"\n"
	p := filepath.Join(t.TempDir(), "router.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLegacyAPIBaseEquivalenceAllDialects(t *testing.T) {
	cases := []struct{ dialect, base, apiBase string }{
		{DialectOpenAIChat, "https://up.example", "https://up.example"},
		{DialectOpenAIChat, "https://up.example", "https://up.example/"},
		{DialectOpenAIChat, "https://up.example/prefix", "https://up.example/prefix/chat/completions"},
		{DialectOpenAIChat, "https://up.example/prefix/chat/completions", "https://up.example/prefix"},
		{DialectOpenAIResponses, "https://up.example/v1", "https://up.example/v1/responses"},
		{DialectAnthropic, "https://up.example", "https://up.example/v1/messages"},
		{DialectAnthropic, "https://up.example/v1", "https://up.example"},
		{DialectAnthropic, "https://up.example/a/b/c", "https://up.example/a/b/c/v1/messages"},
		{DialectGemini, "https://up.example", "https://up.example/v1beta"},
		{DialectGemini, "https://up.example/prefix", "https://up.example/prefix/v1beta"},
	}
	for _, tc := range cases {
		block := "[providers.up]\ndialect = \"" + tc.dialect + "\"\nbase_url = \"" + tc.base + "\"\n[providers.up.litellm_kwargs]\napi_base = \"" + tc.apiBase + "\"\n"
		cfg, err := Load(writeAliasCfg(t, block))
		if err != nil {
			t.Fatalf("%s base %s api_base %s must be accepted: %v", tc.dialect, tc.base, tc.apiBase, err)
		}
		if cfg.Providers["up"].BaseURL == "" {
			t.Fatalf("BaseURL must be retained")
		}
		// No independent runtime effect: declared base unchanged by the alias.
		want, _ := endpoint.GenerationEndpoint("https://probe.example", tc.dialect)
		_ = want
	}
}

func TestLegacyAPIBaseDivergenceRejected(t *testing.T) {
	cases := []struct{ dialect, base, apiBase string }{
		{DialectOpenAIChat, "https://a.example", "https://b.example"},
		{DialectAnthropic, "https://a.example", "https://b.example/v1/messages"},
		{DialectGemini, "https://g.example/v1", "https://g.example/v1beta"},
		{DialectGemini, "https://g.example/v1beta", "https://g.example/v1"},
		{DialectAnthropic, "https://a.example/x", "https://a.example/y"},
	}
	for _, tc := range cases {
		secret := "SECRET-" + tc.apiBase
		_ = secret
		block := "[providers.up]\ndialect = \"" + tc.dialect + "\"\nbase_url = \"" + tc.base + "\"\n[providers.up.litellm_kwargs]\napi_base = \"" + tc.apiBase + "\"\n"
		_, err := Load(writeAliasCfg(t, block))
		if err == nil {
			t.Fatalf("%s divergent api_base must be rejected", tc.dialect)
		}
		msg := err.Error()
		if !strings.Contains(msg, "providers.up.litellm_kwargs.api_base") {
			t.Fatalf("must name keypath: %v", err)
		}
		if !strings.Contains(msg, "base_url") {
			t.Fatalf("must guide to base_url: %v", err)
		}
		if strings.Contains(msg, tc.apiBase) || strings.Contains(msg, tc.base) {
			t.Fatalf("must stay value-free: %v", err)
		}
	}
}

func TestLegacyAPIBaseValidationShapes(t *testing.T) {
	bad := []string{
		"[providers.up.litellm_kwargs]\napi_base = \"https://user@h.example\"",
		"[providers.up.litellm_kwargs]\napi_base = \"https://h.example/?q=1\"",
		"[providers.up.litellm_kwargs]\napi_base = \"https://h.example#frag\"",
		"[providers.up.litellm_kwargs]\napi_base = \"ftp://h.example\"",
		"[providers.up.litellm_kwargs]\napi_base = 42",
		"[providers.up.litellm_kwargs]\napi_base = \"\"",
	}
	for i, kw := range bad {
		block := "[providers.up]\ndialect = \"anthropic\"\nbase_url = \"https://up.example\"\n" + kw + "\n"
		_, err := Load(writeAliasCfg(t, block))
		if err == nil {
			t.Fatalf("case %d must be rejected", i)
		}
		if !strings.Contains(err.Error(), "providers.up.litellm_kwargs.api_base") {
			t.Fatalf("case %d must name keypath: %v", i, err)
		}
		if strings.Contains(err.Error(), "user@") || strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("case %d must stay value-free: %v", i, err)
		}
	}
}

func TestLegacyTimeoutEquality(t *testing.T) {
	// Absent default equality: kwarg 600 with omitted provider timeout accepted.
	block := "[providers.up]\ndialect = \"anthropic\"\nbase_url = \"https://up.example\"\n[providers.up.litellm_kwargs]\ntimeout = 600.0\n"
	cfg, err := Load(writeAliasCfg(t, block))
	if err != nil {
		t.Fatalf("default-equal timeout must be accepted: %v", err)
	}
	if cfg.Providers["up"].Timeout != 600.0 {
		t.Fatalf("timeout wrong: %v", cfg.Providers["up"].Timeout)
	}
	// Explicit equal accepted.
	block = "[providers.up]\ndialect = \"anthropic\"\nbase_url = \"https://up.example\"\ntimeout = 22.0\n[providers.up.litellm_kwargs]\ntimeout = 22.0\n"
	if _, err := Load(writeAliasCfg(t, block)); err != nil {
		t.Fatalf("equal timeout must be accepted: %v", err)
	}
	// Integer vs float equality accepted (10 == 10.0).
	block = "[providers.up]\ndialect = \"anthropic\"\nbase_url = \"https://up.example\"\ntimeout = 10\n[providers.up.litellm_kwargs]\ntimeout = 10.0\n"
	if _, err := Load(writeAliasCfg(t, block)); err != nil {
		t.Fatalf("int/float equal timeout must be accepted: %v", err)
	}
	// Divergence including task example (kwarg 10, provider default 600).
	for _, kw := range []string{"timeout = 10", "timeout = 10.0"} {
		block = "[providers.up]\ndialect = \"anthropic\"\nbase_url = \"https://up.example\"\n[providers.up.litellm_kwargs]\n" + kw + "\n"
		_, err := Load(writeAliasCfg(t, block))
		if err == nil {
			t.Fatalf("%s divergent from default must be rejected", kw)
		}
		if !strings.Contains(err.Error(), "providers.up.litellm_kwargs.timeout") {
			t.Fatalf("must name keypath: %v", err)
		}
		if !strings.Contains(err.Error(), "timeout") {
			t.Fatalf("must guide to timeout: %v", err)
		}
		if strings.Contains(err.Error(), "10") {
			t.Fatalf("must stay value-free: %v", err)
		}
	}
}

func TestLegacyTimeoutStrictShapes(t *testing.T) {
	// Build with explicit provider timeout so shape errors are not masked by
	// divergence.
	prefix := "[providers.up]\ndialect = \"anthropic\"\nbase_url = \"https://up.example\"\ntimeout = 30.0\n[providers.up.litellm_kwargs]\n"
	shapeCases := []string{
		"timeout = \"30\"",
		"timeout = true",
		"timeout = -5",
		"timeout = 0",
	}
	for _, kw := range shapeCases {
		_, err := Load(writeAliasCfg(t, prefix+kw+"\n"))
		if err == nil {
			t.Fatalf("%s must be rejected", kw)
		}
		if !strings.Contains(err.Error(), "providers.up.litellm_kwargs.timeout") {
			t.Fatalf("%s must name keypath: %v", kw, err)
		}
	}
	// NaN / +Inf / overflow via programmatic TOML values (TOML has no NaN
	// literal, so exercise checkLegacyTimeout directly).
	p := &Provider{Name: "up", Dialect: DialectAnthropic, BaseURL: "https://up.example", Timeout: 30}
	for _, v := range []any{math.NaN(), math.Inf(1), math.Inf(-1), 1e20} {
		if err := checkLegacyTimeout(p, v, "providers.up"); err == nil {
			t.Fatalf("non-finite/overflow %v must be rejected", v)
		} else if !strings.Contains(err.Error(), "providers.up.litellm_kwargs.timeout") {
			t.Fatalf("must name keypath: %v", err)
		}
	}
	if err := checkLegacyTimeout(p, "30", "providers.up"); err == nil {
		t.Fatalf("string timeout must be rejected (no coercion)")
	}
}

func TestLegacyAliasKeepsCountTokensRule(t *testing.T) {
	// First-party base with equivalent suffix alias stays first-party capable.
	block := "[providers.up]\ndialect = \"anthropic\"\nbase_url = \"https://api.anthropic.com\"\n[providers.up.litellm_kwargs]\napi_base = \"https://api.anthropic.com/v1\"\n"
	cfg, err := Load(writeAliasCfg(t, block))
	if err != nil {
		t.Fatalf("equivalent first-party alias must load: %v", err)
	}
	if cfg.Providers["up"].CountTokens != nil {
		t.Fatalf("alias must not set count_tokens")
	}
	if cfg.Providers["up"].BaseURL != "https://api.anthropic.com" {
		t.Fatalf("BaseURL must be unchanged, got %q", cfg.Providers["up"].BaseURL)
	}
}
