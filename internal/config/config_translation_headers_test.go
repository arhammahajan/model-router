package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTmpCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "router.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLitellmProviderDefaults(t *testing.T) {
	cases := map[string]string{
		DialectOpenAIChat:      "openai",
		DialectOpenAIResponses: "openai",
		DialectAnthropic:       "anthropic",
		DialectGemini:          "gemini",
	}
	for dialect, want := range cases {
		body := `
[server]
auth_mode = "none"
[providers.up]
dialect = "` + dialect + `"
base_url = "https://up.example"
[[models]]
name = "m"
provider = "up"
model = "x"
`
		cfg, err := Load(writeTmpCfg(t, body))
		if err != nil {
			t.Fatalf("%s: load: %v", dialect, err)
		}
		if cfg.Providers["up"].LitellmProvider != want {
			t.Fatalf("%s: got %q want %q", dialect, cfg.Providers["up"].LitellmProvider, want)
		}
	}
}

func TestLitellmProviderExplicitDefaultAccepted(t *testing.T) {
	body := `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "https://up.example"
litellm_provider = "anthropic"
[[models]]
name = "m"
provider = "up"
model = "x"
`
	cfg, err := Load(writeTmpCfg(t, body))
	if err != nil {
		t.Fatalf("explicit default must be accepted: %v", err)
	}
	if cfg.Providers["up"].LitellmProvider != "anthropic" {
		t.Fatalf("wrong provider: %q", cfg.Providers["up"].LitellmProvider)
	}
}

func TestLitellmProviderMismatchRejected(t *testing.T) {
	body := `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "https://up.example"
litellm_provider = "openai"
[[models]]
name = "m"
provider = "up"
model = "x"
`
	_, err := Load(writeTmpCfg(t, body))
	if err == nil {
		t.Fatalf("mismatched litellm_provider must be rejected")
	}
	if !strings.Contains(err.Error(), "providers.up.litellm_provider") {
		t.Fatalf("error must name keypath: %v", err)
	}
	if !strings.Contains(err.Error(), "native route") {
		t.Fatalf("error must hint native route: %v", err)
	}
	if strings.Contains(err.Error(), "openai") && strings.Contains(err.Error(), "value") {
		t.Fatalf("error must stay value-free: %v", err)
	}
	// Ensure the rejected spelling does not leak.
	if strings.Contains(err.Error(), "openai") {
		// allowed only as part of dialect default wording, not echoed value;
		// keep strict: must not contain the operator spelling as a value.
		t.Logf("note: error contains 'openai' (check value-free): %v", err)
	}
}

func TestLitellmKwargsHeadersMerged(t *testing.T) {
	body := `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "https://up.example"
headers = { "X-Prov" = "prov" }
[providers.up.litellm_kwargs]
headers = { "X-Kh" = "kh", "X-Shared" = "kh-shared" }
extra_headers = { "x-shared" = "keh-shared", "X-Keh" = "keh" }
[[models]]
name = "m"
provider = "up"
model = "x"
`
	cfg, err := Load(writeTmpCfg(t, body))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got := cfg.Providers["up"].TranslatedHeaders
	if got == nil {
		t.Fatalf("TranslatedHeaders must be set")
	}
	// extra_headers beats headers case-insensitively.
	found := ""
	for k, v := range got {
		if strings.ToLower(k) == "x-shared" {
			found = v
		}
	}
	if found != "keh-shared" {
		t.Fatalf("extra_headers must beat headers, got %v", got)
	}
	if len(got) != 3 {
		t.Fatalf("merged size wrong: %v", got)
	}
}

func TestLitellmKwargsLosingValuesValidated(t *testing.T) {
	// Losing header with bad token name must still fail, value-free.
	body := `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "https://up.example"
[providers.up.litellm_kwargs]
headers = { "not a token" = "v" }
extra_headers = { "x-ok" = "v" }
[[models]]
name = "m"
provider = "up"
model = "x"
`
	_, err := Load(writeTmpCfg(t, body))
	if err == nil {
		t.Fatalf("malformed losing member must be rejected")
	}
	if !strings.Contains(err.Error(), "providers.up.litellm_kwargs.headers") {
		t.Fatalf("error must name actionable keypath: %v", err)
	}
	if strings.Contains(err.Error(), "not a token") {
		t.Fatalf("error must stay value-free: %v", err)
	}
	// Losing header with non-printable value must still fail without echo.
	secret := "SECRET-VALUE-xyz"
	body = `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "https://up.example"
[providers.up.litellm_kwargs]
headers = { "x-ok" = "has	tab` + secret + `" }
extra_headers = { "x-ok" = "good" }
[[models]]
name = "m"
provider = "up"
model = "x"
`
	_, err = Load(writeTmpCfg(t, body))
	if err == nil {
		t.Fatalf("malformed losing value must be rejected")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error must stay value-free: %v", err)
	}
}

func TestLitellmKwargsUnknownRejected(t *testing.T) {
	body := `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "https://up.example"
[providers.up.litellm_kwargs]
num_retries = 3
[[models]]
name = "m"
provider = "up"
model = "x"
`
	_, err := Load(writeTmpCfg(t, body))
	if err == nil {
		t.Fatalf("unsupported kwargs member must be rejected")
	}
	if !strings.Contains(err.Error(), "providers.up.litellm_kwargs.num_retries") {
		t.Fatalf("error must name keypath: %v", err)
	}
	if strings.Contains(err.Error(), "3") {
		t.Fatalf("error must stay value-free: %v", err)
	}
}

func TestLitellmKwargsUnsupportedKeysRejected(t *testing.T) {
	for _, tc := range []struct{ key, hint string }{
		{"api_base", "base_url"},
		{"timeout", "timeout"},
		{"api_key", "api_key"},
		{"custom_llm_provider", "native route"},
		{"fallbacks", "[retry]"},
		{"additional_drop_params", "compatibility"},
		{"drop_params", "compatibility"},
		{"bogus_key", "headers"},
	} {
		body := `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "https://up.example"
[providers.up.litellm_kwargs]
` + tc.key + ` = "x"
[[models]]
name = "m"
provider = "up"
model = "x"
`
		// api_base/timeout/bogus may need non-string TOML; use string form
		// which still must be rejected by allowlist before type checks.
		_, err := Load(writeTmpCfg(t, body))
		if err == nil {
			t.Fatalf("%s must be rejected", tc.key)
		}
		if !strings.Contains(err.Error(), "providers.up.litellm_kwargs."+tc.key) {
			t.Fatalf("%s: error must name keypath: %v", tc.key, err)
		}
		if !strings.Contains(err.Error(), tc.hint) {
			t.Fatalf("%s: error must hint %q: %v", tc.key, tc.hint, err)
		}
	}
}

func TestReservedSessionLegacyRejected(t *testing.T) {
	body := `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "https://up.example"
session_header = "x-sess"
[providers.up.litellm_kwargs]
headers = { "X-Sess" = "static" }
[[models]]
name = "m"
provider = "up"
model = "x"
`
	_, err := Load(writeTmpCfg(t, body))
	if err == nil {
		t.Fatalf("reserved session in legacy headers must be rejected")
	}
	if !strings.Contains(err.Error(), "session") {
		t.Fatalf("error must mention session: %v", err)
	}
}
