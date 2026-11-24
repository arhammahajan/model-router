package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arhammahajan/router/internal/endpoint"
)

func reviewAliasCfg(t *testing.T, providerBlock string) string {
	t.Helper()
	body := "[server]\nauth_mode = \"none\"\n" + providerBlock + "\n[[models]]\nname = \"m\"\nprovider = \"up\"\nmodel = \"x\"\n"
	p := filepath.Join(t.TempDir(), "router.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReviewAliasDialectConstantsMatchEndpoint(t *testing.T) {
	if DialectAnthropic != endpoint.DialectAnthropic ||
		DialectOpenAIChat != endpoint.DialectOpenAIChat ||
		DialectOpenAIResponses != endpoint.DialectOpenAIResponses ||
		DialectGemini != endpoint.DialectGemini {
		t.Fatalf("dialect spellings must match endpoint package")
	}
}

func TestReviewAliasConservativeRejected(t *testing.T) {
	cases := []struct{ dialect, base, apiBase string }{
		{DialectAnthropic, "https://a.example/v1/messages", "https://a.example/v1%2Fmessages"},
		{DialectAnthropic, "https://a.example/v1/messages", "https://a.example/%76%31/messages"},
		{DialectAnthropic, "https://a.example/v1/messages", "https://A.example/v1/messages"},
		{DialectAnthropic, "https://a.example/v1/messages", "https://a.example:443/v1/messages"},
		{DialectAnthropic, "https://a.example/v1/messages", "http://a.example/v1/messages"},
		{DialectAnthropic, "https://a.example/v1/messages", "https://a.example//v1/messages"},
		{DialectOpenAIChat, "https://a.example/chat/completions", "https://a.example/xchat/completions"},
		{DialectOpenAIResponses, "https://a.example/responses", "https://a.example/myresponses"},
		{DialectAnthropic, "https://a.example/v1/messages", "https://a.example/V1/MESSAGES"},
		{DialectGemini, "https://g.example/v1", "https://g.example/v1beta"},
		{DialectGemini, "https://g.example/v1beta", "https://g.example/v1"},
	}
	for _, tc := range cases {
		secretBase := "SECRET-BASE-" + tc.base
		secretAlias := "SECRET-ALIAS-" + tc.apiBase
		_ = secretBase
		_ = secretAlias
		block := "[providers.up]\ndialect = \"" + tc.dialect + "\"\nbase_url = \"" + tc.base + "\"\n[providers.up.litellm_kwargs]\napi_base = \"" + tc.apiBase + "\"\n"
		_, err := Load(reviewAliasCfg(t, block))
		if err == nil {
			t.Fatalf("%s base %q alias %q must be rejected", tc.dialect, tc.base, tc.apiBase)
		}
		msg := err.Error()
		if !strings.Contains(msg, "providers.up.litellm_kwargs.api_base") {
			t.Fatalf("must name keypath: %v", err)
		}
		if !strings.Contains(msg, "base_url") {
			t.Fatalf("must guide to base_url: %v", err)
		}
		if strings.Contains(msg, tc.base) || strings.Contains(msg, tc.apiBase) {
			t.Fatalf("must stay value-free: %v", err)
		}
	}
}

func TestReviewAliasCustomPrefixBothDirections(t *testing.T) {
	pairs := []struct{ dialect, base, alias string }{
		{DialectAnthropic, "https://up.example/prefix", "https://up.example/prefix/v1/messages"},
		{DialectAnthropic, "https://up.example/prefix/v1/messages", "https://up.example/prefix/"},
		{DialectAnthropic, "https://up.example/prefix/v1", "https://up.example/prefix"},
		{DialectOpenAIChat, "https://up.example/prefix", "https://up.example/prefix/chat/completions/"},
		{DialectOpenAIChat, "https://up.example/prefix/chat/completions", "https://up.example/prefix"},
		{DialectOpenAIResponses, "https://up.example/prefix", "https://up.example/prefix/responses/"},
		{DialectGemini, "https://up.example/prefix", "https://up.example/prefix/v1beta/"},
	}
	for _, tc := range pairs {
		block := "[providers.up]\ndialect = \"" + tc.dialect + "\"\nbase_url = \"" + tc.base + "\"\n[providers.up.litellm_kwargs]\napi_base = \"" + tc.alias + "\"\n"
		cfg, err := Load(reviewAliasCfg(t, block))
		if err != nil {
			t.Fatalf("%s base %q alias %q must be accepted: %v", tc.dialect, tc.base, tc.alias, err)
		}
		// Declared base unchanged; no second endpoint stored.
		want, _ := endpoint.GenerationEndpoint(tc.base, tc.dialect)
		got, _ := endpoint.GenerationEndpoint(cfg.Providers["up"].BaseURL, tc.dialect)
		if want != got {
			t.Fatalf("BaseURL must be unchanged: %q vs %q", want, got)
		}
		if cfg.Providers["up"].TranslatedHeaders != nil {
			t.Fatalf("api_base must not populate TranslatedHeaders")
		}
	}
}

func TestReviewAliasTimeoutIntFloatDefault(t *testing.T) {
	// Integer 600 equals the omitted default 600.0.
	block := "[providers.up]\ndialect = \"anthropic\"\nbase_url = \"https://up.example\"\n[providers.up.litellm_kwargs]\ntimeout = 600\n"
	cfg, err := Load(reviewAliasCfg(t, block))
	if err != nil {
		t.Fatalf("int 600 must equal default: %v", err)
	}
	if cfg.Providers["up"].Timeout != 600.0 {
		t.Fatalf("effective timeout must stay 600, got %v", cfg.Providers["up"].Timeout)
	}
	// Explicit float/int equality.
	for _, kw := range []string{"timeout = 22", "timeout = 22.0"} {
		block = "[providers.up]\ndialect = \"anthropic\"\nbase_url = \"https://up.example\"\ntimeout = 22.0\n[providers.up.litellm_kwargs]\n" + kw + "\n"
		if _, err := Load(reviewAliasCfg(t, block)); err != nil {
			t.Fatalf("%s must be accepted: %v", kw, err)
		}
	}
	// Divergence stays rejected and value-free.
	block = "[providers.up]\ndialect = \"anthropic\"\nbase_url = \"https://up.example\"\ntimeout = 22.0\n[providers.up.litellm_kwargs]\ntimeout = 23.0\n"
	_, err = Load(reviewAliasCfg(t, block))
	if err == nil {
		t.Fatalf("divergent timeout must be rejected")
	}
	if !strings.Contains(err.Error(), "providers.up.litellm_kwargs.timeout") || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("must name keypath and guide: %v", err)
	}
	if strings.Contains(err.Error(), "23") || strings.Contains(err.Error(), "22") {
		t.Fatalf("must stay value-free: %v", err)
	}
	// Strict shapes.
	prefix := "[providers.up]\ndialect = \"anthropic\"\nbase_url = \"https://up.example\"\ntimeout = 30.0\n[providers.up.litellm_kwargs]\n"
	for _, kw := range []string{"timeout = \"30\"", "timeout = true", "timeout = 0", "timeout = -1", "timeout = nan", "timeout = inf"} {
		_, err := Load(reviewAliasCfg(t, prefix+kw+"\n"))
		if err == nil {
			t.Fatalf("%s must be rejected", kw)
		}
		if !strings.Contains(err.Error(), "providers.up.litellm_kwargs.timeout") {
			t.Fatalf("%s must name keypath: %v", kw, err)
		}
	}
}

func TestReviewAliasUnsupportedKeysStillRejected(t *testing.T) {
	keys := []struct{ toml, wantHint string }{
		{"num_retries = 3", "[retry]"},
		{"max_retries = 3", "[retry]"},
		{"fallbacks = []", "[retry]"},
		{"custom_llm_provider = \"openai\"", "native route"},
		{"api_key = \"SECRET-999\"", "api_key"},
		{"additional_drop_params = []", "compatibility"},
		{"drop_params = true", "compatibility"},
		{"_skip_mcp_handler = true", "never executes tools"},
		{"bogus_key = 1", "unsupported"},
	}
	for _, tc := range keys {
		block := "[providers.up]\ndialect = \"anthropic\"\nbase_url = \"https://up.example\"\ntimeout = 11.0\n" +
			"[providers.up.litellm_kwargs]\napi_base = \"https://up.example/v1/messages\"\ntimeout = 11.0\n" + tc.toml + "\n"
		_, err := Load(reviewAliasCfg(t, block))
		if err == nil {
			t.Fatalf("%s must still be rejected", tc.toml)
		}
		msg := err.Error()
		if !strings.Contains(msg, "providers.up.litellm_kwargs.") {
			t.Fatalf("%s must name keypath: %v", tc.toml, err)
		}
		if !strings.Contains(msg, tc.wantHint) {
			t.Fatalf("%s must hint %q: %v", tc.toml, tc.wantHint, err)
		}
		if strings.Contains(msg, "SECRET-999") {
			t.Fatalf("must stay value-free: %v", err)
		}
	}
}

func TestReviewAliasKeepsCountTokensExactRule(t *testing.T) {
	// Explicit true/false preserved; alias never sets it.
	for _, v := range []string{"true", "false"} {
		block := "[providers.up]\ndialect = \"anthropic\"\nbase_url = \"https://up.example\"\ncount_tokens = " + v + "\n[providers.up.litellm_kwargs]\napi_base = \"https://up.example/v1/messages\"\ntimeout = 600\n"
		cfg, err := Load(reviewAliasCfg(t, block))
		if err != nil {
			t.Fatalf("count_tokens=%s with alias must load: %v", v, err)
		}
		if cfg.Providers["up"].CountTokens == nil || *cfg.Providers["up"].CountTokens != (v == "true") {
			t.Fatalf("count_tokens=%s must be preserved", v)
		}
	}
	// Non-anthropic explicit true still rejected even with aliases.
	block := "[providers.up]\ndialect = \"openai-chat\"\nbase_url = \"https://up.example\"\ncount_tokens = true\n[providers.up.litellm_kwargs]\napi_base = \"https://up.example/chat/completions\"\ntimeout = 600\n"
	if _, err := Load(reviewAliasCfg(t, block)); err == nil {
		t.Fatalf("non-anthropic count_tokens=true must stay rejected")
	}
	// First-party host still qualifies; lookalikes do not.
	fp := &Provider{Name: "up", DefaultDialect: DialectAnthropic, BaseURL: "https://api.anthropic.com"}
	if got := func() bool {
		// CountTokensEnabled is in transport, but BaseURL preservation is the
		// config-side contract: alias must leave the declared base intact.
		block := "[providers.up]\ndialect = \"anthropic\"\nbase_url = \"https://api.anthropic.com\"\n[providers.up.litellm_kwargs]\napi_base = \"https://api.anthropic.com/v1\"\n"
		cfg, err := Load(reviewAliasCfg(t, block))
		if err != nil {
			return false
		}
		return cfg.Providers["up"].BaseURL == fp.BaseURL && cfg.Providers["up"].CountTokens == nil
	}(); !got {
		t.Fatalf("first-party alias must keep BaseURL and nil count_tokens")
	}
}
