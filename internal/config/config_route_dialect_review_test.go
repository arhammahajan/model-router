package config

import (
	"strings"
	"testing"
)

// rdReviewLoad fails the test on unexpected load errors.
func rdReviewLoad(t *testing.T, body string) *Config {
	t.Helper()
	cfg, err := Load(writeCfg(t, body))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return cfg
}

func TestRdReviewDialectCanonicalization(t *testing.T) {
	body := `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "https://api.anthropic.com"
[[models]]
name = "omitted"
provider = "up"
model = "x"
[[models]]
name = "equal"
provider = "up"
model = "y"
dialect = "anthropic"
[[models]]
name = "padded-equal"
provider = "up"
model = "z"
dialect = "  anthropic  "
`
	cfg := rdReviewLoad(t, body)
	for _, name := range []string{"omitted", "equal", "padded-equal"} {
		m, p, ok := cfg.LookupModel(name)
		if !ok {
			t.Fatalf("lookup %s", name)
		}
		if m.Dialect != "" {
			t.Fatalf("%s: omitted/explicit-equal must canonicalize to empty, got %q", name, m.Dialect)
		}
		if m.EffectiveDialect(p) != DialectAnthropic {
			t.Fatalf("%s: dest=%q", name, m.EffectiveDialect(p))
		}
	}
}

func TestRdReviewDialectErrorNamesField(t *testing.T) {
	base := `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "https://api.anthropic.com"
[[models]]
name = "m"
provider = "up"
model = "x"
`
	for _, tc := range []struct {
		line string
		want string
	}{
		{`dialect = ""`, "models[0].dialect"},
		{`dialect = "   "`, "models[0].dialect"},
		{`dialect = "sms"`, "models[0].dialect"},
		{`dialect = "Anthropic"`, "models[0].dialect"},
		{`dialect = 42`, "models[0].dialect"},
		{`dialect = true`, "models[0].dialect"},
	} {
		_, err := Load(writeCfg(t, base+tc.line+"\n"))
		if err == nil {
			t.Fatalf("%q must be rejected", tc.line)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%q error must name %s, got %v", tc.line, tc.want, err)
		}
	}
}

func TestRdReviewNoExplicitAuthHeaderRequired(t *testing.T) {
	// A mismatch override must NOT demand an explicit auth_header: the
	// provider default is an intentional operator choice.
	body := `
[server]
auth_mode = "none"
[providers.chat]
dialect = "openai-chat"
base_url = "https://up.example"
api_key = "k"
[[models]]
name = "to-anthropic"
provider = "chat"
model = "claude-x"
dialect = "anthropic"
`
	cfg := rdReviewLoad(t, body)
	m, p, ok := cfg.LookupModel("to-anthropic")
	if !ok {
		t.Fatal("lookup")
	}
	if p.AuthHeader != "" {
		t.Fatalf("auth_header must stay unset, got %q", p.AuthHeader)
	}
	if got := m.EffectiveDialect(p); got != DialectAnthropic {
		t.Fatalf("dest=%q", got)
	}
}

func TestRdReviewAliasGateScopedPerProvider(t *testing.T) {
	// The alias on provider B is validated against B's effective
	// destinations; provider A without an alias is unaffected.
	body := `
[server]
auth_mode = "none"
[providers.a]
dialect = "openai-chat"
base_url = "https://up.example"
[providers.b]
dialect = "openai-chat"
base_url = "https://up.example"
litellm_kwargs = { api_base = "https://up.example/chat/completions" }
[[models]]
name = "a-native"
provider = "a"
model = "x"
[[models]]
name = "b-over"
provider = "b"
model = "y"
dialect = "anthropic"
`
	if _, err := Load(writeCfg(t, body)); err == nil {
		t.Fatal("cross-dest alias on B must be rejected even with A present")
	} else if !strings.Contains(err.Error(), "providers.b.litellm_kwargs.api_base") {
		t.Fatalf("error must name providers.b path, got %v", err)
	}
	// Alias equivalent for every effective destination passes, incl. two overrides.
	okBody := `
[server]
auth_mode = "none"
[providers.a]
dialect = "anthropic"
base_url = "https://up.example/v1/messages"
litellm_kwargs = { api_base = "https://up.example/v1/messages" }
[[models]]
name = "native"
provider = "a"
model = "x"
[[models]]
name = "to-chat"
provider = "a"
model = "y"
dialect = "openai-chat"
[[models]]
name = "to-gemini"
provider = "a"
model = "z"
dialect = "gemini"
enabled = false
`
	// Chat dest: .../v1/messages/chat/completions for both base and alias;
	// gemini dest: .../v1/messages/v1beta for both. Equivalent everywhere.
	if _, err := Load(writeCfg(t, okBody)); err != nil {
		t.Fatalf("fully-equivalent alias must pass: %v", err)
	}
}

func TestRdReviewAliasGateNeedsAllDests(t *testing.T) {
	// Alias matches the provider default and one override but not another.
	body := `
[server]
auth_mode = "none"
[providers.up]
dialect = "openai-chat"
base_url = "https://up.example"
litellm_kwargs = { api_base = "https://up.example/chat/completions" }
[[models]]
name = "native"
provider = "up"
model = "x"
[[models]]
name = "to-resp"
provider = "up"
model = "y"
dialect = "openai-responses"
`
	// Chat: base->.../chat/completions, alias->.../chat/completions (match).
	// Responses: base->.../responses, alias->.../chat/completions/responses (differ).
	if _, err := Load(writeCfg(t, body)); err == nil {
		t.Fatal("alias equivalent only for default must be rejected")
	}
}
