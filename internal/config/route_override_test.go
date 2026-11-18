package config

import (
	"strings"
	"testing"
)

func loadInline(t *testing.T, body string) *Config {
	t.Helper()
	cfg, err := Load(writeCfg(t, body))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return cfg
}

func TestRouteOverrideOmittedEqualAliasDisabled(t *testing.T) {
	body := `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "https://api.anthropic.com"
[[models]]
name = "a"
provider = "up"
model = "x"
[[models]]
name = "b"
provider = "up"
model = "y"
dialect = "anthropic"
aliases = ["b-alias"]
[[models]]
name = "c"
provider = "up"
model = "z"
dialect = "gemini"
aliases = ["c-alias"]
enabled = false
`
	cfg := loadInline(t, body)
	ma, pa, _ := cfg.LookupModel("a")
	if got := ma.EffectiveDialect(pa); got != "anthropic" {
		t.Fatalf("omitted dest = %q", got)
	}
	if ma.Dialect != "" {
		t.Fatalf("omitted must canonicalize to empty, got %q", ma.Dialect)
	}
	mb, pb, _ := cfg.LookupModel("b-alias")
	if mb.Dialect != "" || mb.EffectiveDialect(pb) != "anthropic" {
		t.Fatalf("explicit-equal must canonicalize to inherit: %+v", mb)
	}
	mc, pc, _ := cfg.LookupModel("c")
	if !mc.Disabled || mc.EffectiveDialect(pc) != "gemini" {
		t.Fatalf("disabled override must be admitted and reserved: %+v", mc)
	}
	if _, _, ok := cfg.LookupModel("c-alias"); !ok {
		t.Fatalf("disabled alias must stay reserved in index")
	}
}

func TestRouteOverrideMalformed(t *testing.T) {
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
	for _, line := range []string{`dialect = ""`, `dialect = "sms"`, `dialect = 42`, `dialect = true`} {
		body := base + line + "\n"
		_, err := Load(writeCfg(t, body))
		if err == nil {
			t.Fatalf("%q must be rejected", line)
		}
		if !strings.Contains(err.Error(), "models[0].dialect") {
			t.Fatalf("%q error must name models[0].dialect, got %v", line, err)
		}
	}
}

func TestRouteOverrideAliasCrossValidation(t *testing.T) {
	// Alias default-equivalent for anthropic (base /v1/messages) but not for gemini.
	// Primary https://up.example/v1/messages:
	//   anthropic endpoint = .../v1/messages (already present)
	//   gemini endpoint = .../v1/messages/v1beta
	// Alias https://up.example/v1/messages/ :
	//   normalized strips trailing slash -> same as primary for anthropic,
	//   but different construction? Use a sharper case: alias with /chat/completions suffix.
	// Chat alias is default-equivalent for an openai-chat provider but not for anthropic override.
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
name = "over"
provider = "up"
model = "y"
dialect = "anthropic"
`
	if _, err := Load(writeCfg(t, body)); err == nil {
		t.Fatalf("cross-dialect alias must be rejected")
	} else if !strings.Contains(err.Error(), "litellm_kwargs.api_base") {
		t.Fatalf("alias error must name key path, got %v", err)
	}
	// Disabled override also triggers rejection.
	bodyDisabled := `
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
name = "over"
provider = "up"
model = "y"
dialect = "anthropic"
enabled = false
`
	if _, err := Load(writeCfg(t, bodyDisabled)); err == nil {
		t.Fatalf("disabled cross-dialect alias must be rejected")
	}
	// Equivalent for all dests passes: alias already carries the anthropic suffix.
	bodyOK := `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "https://up.example/v1/messages"
litellm_kwargs = { api_base = "https://up.example/v1/messages" }
[[models]]
name = "over"
provider = "up"
model = "y"
dialect = "gemini"
`
	// Primary .../v1/messages + gemini dest = .../v1/messages/v1beta;
	// alias same base normalizes identically, so equivalent for both dests.
	if _, err := Load(writeCfg(t, bodyOK)); err != nil {
		t.Fatalf("fully-equivalent alias must pass: %v", err)
	}
}

func TestRouteOverridePrimarySuffixDeterministic(t *testing.T) {
	// Primary base containing another dialect's endpoint still follows exact rules.
	body := `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "https://up.example/chat/completions"
[[models]]
name = "m"
provider = "up"
model = "x"
dialect = "gemini"
`
	cfg := loadInline(t, body)
	m, p, _ := cfg.LookupModel("m")
	if got := m.EffectiveDialect(p); got != "gemini" {
		t.Fatalf("dest = %q", got)
	}
}
