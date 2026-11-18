package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	// Hermetic config tests: the developer shell may export router keys or
	// keep a real credentials.env; neither may influence fixtures.
	os.Unsetenv("MODEL_ROUTER_API_KEY")
	os.Unsetenv("MODEL_ROUTER_API_KEYS")
	os.Unsetenv("ANTHROPIC_API_KEY")
	os.Unsetenv("OPENAI_API_KEY")
	empty, err := os.MkdirTemp("", "cfg-home")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(empty)
	os.Setenv("HOME", empty)
	os.Setenv("XDG_CONFIG_HOME", empty)
	os.Exit(m.Run())
}

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "router.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const minimal = `
[server]
auth_mode = "none"
allow_dynamic_routes = false

[retry]
max_attempts = 1

[providers.up]
dialect = "anthropic"
base_url = "https://api.anthropic.com"
api_key_env = "TEST_UP_KEY_M2"

[[models]]
name = "claude-haiku"
provider = "up"
model = "claude-haiku-4-5"
aliases = ["haiku"]
`

func TestLoadMinimal(t *testing.T) {
	t.Setenv("TEST_UP_KEY_M2", " secret ")
	cfg, err := Load(writeCfg(t, minimal))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Providers["up"].APIKey != " secret " {
		t.Fatalf("key not preserved raw: %q", cfg.Providers["up"].APIKey)
	}
	if cfg.Providers["up"].EffectiveKey() != "secret" {
		t.Fatalf("key not stripped: %q", cfg.Providers["up"].EffectiveKey())
	}
	m, p, ok := cfg.LookupModel("haiku")
	if !ok || p.Dialect != DialectAnthropic || m.Upstream != "claude-haiku-4-5" {
		t.Fatalf("lookup failed: %+v %+v %v", m, p, ok)
	}
}

func TestRejectsLitellmKwargs(t *testing.T) {
	t.Setenv("TEST_UP_KEY_M2", "dummy")
	body := minimal + "\n[providers.up.litellm_kwargs]\nfoo = 1\n"
	// nested provider tables are rejected explicitly
	if _, err := Load(writeCfg(t, body)); err == nil {
		t.Fatalf("expected rejection of nested provider table")
	}
}

func TestRejectsUnsupportedProviderKey(t *testing.T) {
	t.Setenv("TEST_UP_KEY_M2", "dummy")
	body := `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "https://api.anthropic.com"
litellm_kwargs = { foo = 1 }
[[models]]
name = "m"
provider = "up"
model = "x"
`
	if _, err := Load(writeCfg(t, body)); err == nil {
		t.Fatalf("expected rejection of litellm_kwargs")
	}
}

func TestAcceptsSessionAffinity(t *testing.T) {
	body := `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "https://api.anthropic.com"
session_header = "x-sess"
[[models]]
name = "m"
provider = "up"
model = "x"
`
	cfg, err := Load(writeCfg(t, body))
	if err != nil {
		t.Fatalf("session affinity must be accepted: %v", err)
	}
	p := cfg.Providers["up"]
	if p.SessionHeader != "x-sess" {
		t.Fatalf("session_header not normalized: %q", p.SessionHeader)
	}
	if len(p.SessionSourceHeaders) != 7 || p.SessionSourceHeaders[0] != "x-claude-code-session-id" {
		t.Fatalf("absent sources must default to all seven natives: %v", p.SessionSourceHeaders)
	}
	if p.SessionMissing != "omit" {
		t.Fatalf("session_missing must default to omit: %q", p.SessionMissing)
	}
}

func TestCountTokensDialectGate(t *testing.T) {
	body := `
[server]
auth_mode = "none"
[providers.local]
dialect = "openai-chat"
base_url = "http://127.0.0.1:11434/v1"
count_tokens = true
[[models]]
name = "m"
provider = "local"
model = "x"
`
	if _, err := Load(writeCfg(t, body)); err == nil {
		t.Fatalf("expected count_tokens dialect rejection")
	}
}

func TestExampleConfigLoads(t *testing.T) {
	// The shipped example must load with dummy credentials: it exercises
	// the full supported surface (headers, timeouts, aliases, metadata).
	t.Setenv("ANTHROPIC_API_KEY", "dummy")
	t.Setenv("OPENAI_API_KEY", "dummy")
	cfg, err := Load("../../config.example.toml")
	if err != nil {
		t.Fatalf("example config: %v", err)
	}
	if len(cfg.Providers) < 3 || len(cfg.Models) == 0 {
		t.Fatalf("thin parse: %+v", cfg)
	}
}

// --- review regressions: parser strictness ---

func TestRejectsDuplicateKeys(t *testing.T) {
	t.Setenv("TEST_UP_KEY_M2", "dummy")
	body := strings.Replace(minimal, "allow_dynamic_routes = false",
		"allow_dynamic_routes = false\nallow_dynamic_routes = true", 1)
	if _, err := Load(writeCfg(t, body)); err == nil {
		t.Fatalf("expected duplicate-key rejection")
	}
}

func TestRejectsDuplicateTables(t *testing.T) {
	t.Setenv("TEST_UP_KEY_M2", "dummy")
	body := minimal + "\n[providers.up]\ndialect = \"anthropic\"\n"
	if _, err := Load(writeCfg(t, body)); err == nil {
		t.Fatalf("expected duplicate-table rejection")
	}
}

func TestRejectsFloatPort(t *testing.T) {
	t.Setenv("TEST_UP_KEY_M2", "dummy")
	body := strings.Replace(minimal, "auth_mode = \"none\"",
		"auth_mode = \"none\"\nport = 4000.0", 1)
	if _, err := Load(writeCfg(t, body)); err == nil {
		t.Fatalf("expected float-port rejection (floats must not parse as ints)")
	}
}

func TestRejectsOutOfRangePort(t *testing.T) {
	t.Setenv("TEST_UP_KEY_M2", "dummy")
	for _, port := range []string{"0", "65536", "-1"} {
		body := strings.Replace(minimal, "auth_mode = \"none\"",
			"auth_mode = \"none\"\nport = "+port, 1)
		if _, err := Load(writeCfg(t, body)); err == nil {
			t.Fatalf("expected rejection of port %s", port)
		}
	}
}

// Explicit zeros must be validation errors, never silent default restores.
func TestRejectsExplicitZeroLimits(t *testing.T) {
	t.Setenv("TEST_UP_KEY_M2", "dummy")
	cases := []string{
		"max_connections = 0", "max_generations = 0", "max_body_bytes = 0",
		"header_read_timeout = 0", "body_read_timeout = 0", "write_timeout = 0",
	}
	for _, line := range cases {
		body := strings.Replace(minimal, "allow_dynamic_routes = false",
			"allow_dynamic_routes = false\n"+line, 1)
		if _, err := Load(writeCfg(t, body)); err == nil {
			t.Fatalf("expected rejection of explicit zero %q", line)
		}
	}
}

func TestRejectsNonFiniteTimeout(t *testing.T) {
	t.Setenv("TEST_UP_KEY_M2", "dummy")
	body := strings.Replace(minimal, "allow_dynamic_routes = false",
		"allow_dynamic_routes = false\nheader_read_timeout = inf", 1)
	if _, err := Load(writeCfg(t, body)); err == nil {
		t.Fatalf("expected rejection of non-finite timeout")
	}
	body = `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "https://api.anthropic.com"
timeout = nan
[[models]]
name = "m"
provider = "up"
model = "x"
`
	if _, err := Load(writeCfg(t, body)); err == nil {
		t.Fatalf("expected rejection of NaN timeout")
	}
}

func TestSubtableHeadersAndQuotedNames(t *testing.T) {
	body := `
[server]
auth_mode = "none"
[providers."my provider".headers]
"x-custom" = "v"
[providers."my provider"]
dialect = "anthropic"
base_url = "https://api.anthropic.com/"
[[models]]
name = "m"
provider = "my provider"
model = "x"
`
	cfg, err := Load(writeCfg(t, body))
	if err != nil {
		t.Fatalf("valid TOML table syntax must work: %v", err)
	}
	p := cfg.Providers["my provider"]
	if p.Headers["x-custom"] != "v" {
		t.Fatalf("sub-table headers not merged: %+v", p.Headers)
	}
	if p.BaseURL != "https://api.anthropic.com" {
		t.Fatalf("trailing slash not stripped: %q", p.BaseURL)
	}
}

func TestMultilineValues(t *testing.T) {
	body := `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "https://api.anthropic.com"
api_key = """line1
line2"""
[[models]]
name = "m"
provider = "up"
model = "x"
`
	cfg, err := Load(writeCfg(t, body))
	if err != nil {
		t.Fatalf("multiline strings must parse: %v", err)
	}
	if cfg.Providers["up"].APIKey != "line1\nline2" {
		t.Fatalf("multiline value corrupted: %q", cfg.Providers["up"].APIKey)
	}
}

// --- review regressions: auth and credentials ---

func TestAuthModeNoneIgnoresAmbientKeys(t *testing.T) {
	t.Setenv("TEST_UP_KEY_M2", "dummy")
	t.Setenv("MODEL_ROUTER_API_KEY", "ambient")
	t.Setenv("MODEL_ROUTER_API_KEYS", "ambient2")
	cfg, err := Load(writeCfg(t, minimal))
	if err != nil {
		t.Fatalf("explicit none must ignore ambient keys, got: %v", err)
	}
	if len(cfg.Server.APIKeys) != 0 || cfg.Server.AuthMode != "none" {
		t.Fatalf("ambient keys leaked into auth_mode=none: %+v", cfg.Server)
	}
}

func TestOmittedAuthModeInference(t *testing.T) {
	t.Setenv("TEST_UP_KEY_M2", "dummy")
	body := `
[server]
[providers.up]
dialect = "anthropic"
base_url = "https://api.anthropic.com"
[[models]]
name = "m"
provider = "up"
model = "x"
`
	cfg, err := Load(writeCfg(t, body))
	if err != nil {
		t.Fatalf("omitted auth_mode without keys must stay open: %v", err)
	}
	if cfg.Server.AuthMode != "none" {
		t.Fatalf("expected inferred none, got %q", cfg.Server.AuthMode)
	}
	t.Setenv("MODEL_ROUTER_API_KEY", "k")
	cfg, err = Load(writeCfg(t, body))
	if err != nil {
		t.Fatalf("omitted auth_mode with keys must require auth: %v", err)
	}
	if cfg.Server.AuthMode != "required" || len(cfg.Server.APIKeys) != 1 {
		t.Fatalf("expected inferred required with one key: %+v", cfg.Server)
	}
}

func TestRequiredWithoutKeysRejected(t *testing.T) {
	t.Setenv("TEST_UP_KEY_M2", "dummy")
	body := strings.Replace(minimal, "auth_mode = \"none\"", "auth_mode = \"required\"", 1)
	if _, err := Load(writeCfg(t, body)); err == nil {
		t.Fatalf("explicit required without keys must fail")
	}
}

func TestAPIKeysDeduped(t *testing.T) {
	t.Setenv("TEST_UP_KEY_M2", "dummy")
	t.Setenv("MODEL_ROUTER_API_KEYS", "b, a,b ,,")
	t.Setenv("MODEL_ROUTER_API_KEY", "a")
	body := strings.Replace(minimal, "auth_mode = \"none\"",
		"auth_mode = \"required\"\napi_keys = [\"a\", \"c\"]", 1)
	cfg, err := Load(writeCfg(t, body))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := []string{"a", "c", "b"}
	if len(cfg.Server.APIKeys) != len(want) {
		t.Fatalf("dedup wrong: %q", cfg.Server.APIKeys)
	}
	for i := range want {
		if cfg.Server.APIKeys[i] != want[i] {
			t.Fatalf("dedup order wrong: %q want %q", cfg.Server.APIKeys, want)
		}
	}
}

func TestBearerSessionCompatAdmission(t *testing.T) {
	t.Setenv("TEST_UP_KEY_M2", "dummy")
	// true with zero effective keys is admitted (auth_mode none, no env keys).
	body := strings.Replace(minimal, "allow_dynamic_routes = false",
		"allow_dynamic_routes = false\nbearer_session_compat = true", 1)
	cfg, err := Load(writeCfg(t, body))
	if err != nil {
		t.Fatalf("keyless bearer_session_compat=true must be admitted: %v", err)
	}
	if !cfg.Server.BearerSessionCompat {
		t.Fatalf("flag must be stored true")
	}
	// true with any effective key fails value-free. auth_mode none ignores
	// the environment, so exercise the env path without auth_mode none.
	t.Setenv("MODEL_ROUTER_API_KEY", "env-key")
	envBody := strings.Replace(body, "auth_mode = \"none\"\n", "", 1)
	if _, err := Load(writeCfg(t, envBody)); err == nil {
		t.Fatalf("compat true with an env key must fail")
	} else if strings.Contains(err.Error(), "env-key") {
		t.Fatalf("startup failure must be value-free: %v", err)
	}
}

func TestBearerSessionCompatRejectsConfiguredKeys(t *testing.T) {
	t.Setenv("TEST_UP_KEY_M2", "dummy")
	keyed := strings.Replace(minimal, "auth_mode = \"none\"",
		"auth_mode = \"none\"\napi_keys = [\"k1\"]", 1)
	keyed = strings.Replace(keyed, "allow_dynamic_routes = false",
		"allow_dynamic_routes = false\nbearer_session_compat = true", 1)
	if _, err := Load(writeCfg(t, keyed)); err == nil {
		t.Fatalf("compat true with configured keys must fail")
	} else if strings.Contains(err.Error(), "k1") {
		t.Fatalf("startup failure must be value-free: %v", err)
	}
	// Non-boolean stays a type error.
	bad := strings.Replace(minimal, "allow_dynamic_routes = false",
		"allow_dynamic_routes = false\nbearer_session_compat = \"yes\"", 1)
	if _, err := Load(writeCfg(t, bad)); err == nil {
		t.Fatalf("non-boolean compat must fail")
	}
	// false with keys keeps the 477 baseline.
	t.Setenv("MODEL_ROUTER_API_KEY", "env-key")
	off := strings.Replace(minimal, "auth_mode = \"none\"",
		"auth_mode = \"required\"", 1)
	if _, err := Load(writeCfg(t, off)); err != nil {
		t.Fatalf("compat-off baseline with keys must load: %v", err)
	}
}

func TestCredentialPrecedenceTopLevelWins(t *testing.T) {
	t.Setenv("TEST_PREC_ENV", "from-env")
	body := `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "https://api.anthropic.com"
api_key = "top-level"
api_key_env = "TEST_PREC_ENV"
[[models]]
name = "m"
provider = "up"
model = "x"
`
	cfg, err := Load(writeCfg(t, body))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Providers["up"].APIKey != "top-level" {
		t.Fatalf("top-level api_key must win: %q", cfg.Providers["up"].APIKey)
	}
}

func TestCredentialEnvMissingNamesVar(t *testing.T) {
	body := `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "https://api.anthropic.com"
api_key_env = "TEST_MISSING_KEY_XYZ"
[[models]]
name = "m"
provider = "up"
model = "x"
`
	_, err := Load(writeCfg(t, body))
	if err == nil {
		t.Fatalf("missing env credential must fail")
	}
	if !strings.Contains(err.Error(), "TEST_MISSING_KEY_XYZ") {
		t.Fatalf("error must name the variable: %v", err)
	}
}

func TestCredentialOsEnvironRejected(t *testing.T) {
	body := `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "https://api.anthropic.com"
api_key = "os.environ/FOO"
[[models]]
name = "m"
provider = "up"
model = "x"
`
	if _, err := Load(writeCfg(t, body)); err == nil {
		t.Fatalf("os.environ indirection must be rejected")
	}
}

func TestAPIKeyFileFallback(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "key"), []byte("  file-key\n"), 0o600)
	body := `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "https://api.anthropic.com"
api_key_file = "` + filepath.Join(dir, "key") + `"
[[models]]
name = "m"
provider = "up"
model = "x"
`
	cfg, err := Load(writeCfg(t, body))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Providers["up"].EffectiveKey() != "file-key" {
		t.Fatalf("file key not resolved: %q", cfg.Providers["up"].APIKey)
	}
}

func TestCredentialsEnvFileLoaded(t *testing.T) {
	xdg := t.TempDir()
	os.MkdirAll(filepath.Join(xdg, "ai"), 0o700)
	os.WriteFile(filepath.Join(xdg, "ai", "credentials.env"),
		[]byte("# comment\nexport TEST_CRED_FILE_KEY=from-file\nQUOTED='q v' # trailing\n"), 0o600)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())
	body := `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "https://api.anthropic.com"
api_key_env = "TEST_CRED_FILE_KEY"
[[models]]
name = "m"
provider = "up"
model = "x"
`
	cfg, err := Load(writeCfg(t, body))
	if err != nil {
		t.Fatalf("credentials.env must be honored: %v", err)
	}
	if cfg.Providers["up"].APIKey != "from-file" {
		t.Fatalf("wrong key: %q", cfg.Providers["up"].APIKey)
	}
	// Existing environment wins over the file.
	t.Setenv("TEST_CRED_FILE_KEY", "from-env")
	cfg, err = Load(writeCfg(t, body))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Providers["up"].APIKey != "from-env" {
		t.Fatalf("environment must win: %q", cfg.Providers["up"].APIKey)
	}
}

// --- review regressions: URLs and headers ---

func TestBaseURLPolicy(t *testing.T) {
	for _, u := range []string{
		"https://user:pass@api.example.com",
		"https://api.example.com/v1?key=SECRET",
		"https://api.example.com/v1#frag",
		"http://api.example.com?x=1",
		"ftp://api.example.com/v1",
		"api.example.com/v1",
		"",
	} {
		body := `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "` + u + `"
[[models]]
name = "m"
provider = "up"
model = "x"
`
		_, err := Load(writeCfg(t, body))
		if err == nil {
			t.Fatalf("expected rejection of base_url %q", u)
		}
		if strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("error echoes credential-bearing URL: %v", err)
		}
	}
}

func TestHeaderValidation(t *testing.T) {
	bad := []string{
		`headers = { "not a token" = "v" }`,
		"headers = { x-ok = \"has\ttab\" }",
		"headers = { x-ok = \"has\x7fdel\" }",
		`headers = { "X-Session-Id" = "static" }`,
		`headers = { "x-opencode-session" = "static" }`,
	}
	for _, h := range bad {
		body := `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "https://api.anthropic.com"
` + h + `
[[models]]
name = "m"
provider = "up"
model = "x"
`
		if _, err := Load(writeCfg(t, body)); err == nil {
			t.Fatalf("expected rejection of %s", h)
		}
	}
}

// --- review regressions: expansion and secrecy ---

func TestParseThenExpandKeepsSyntax(t *testing.T) {
	// An env value containing quotes/newlines must not alter TOML syntax:
	// expansion runs on parsed values, not raw text.
	t.Setenv("TEST_TRICKY", "a\"b\nc#d")
	body := `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "https://api.anthropic.com"
api_key = "${TEST_TRICKY}"
[[models]]
name = "m"
provider = "up"
model = "x"
`
	cfg, err := Load(writeCfg(t, body))
	if err != nil {
		t.Fatalf("quoted/newline value must survive expansion: %v", err)
	}
	if cfg.Providers["up"].APIKey != "a\"b\nc#d" {
		t.Fatalf("value corrupted: %q", cfg.Providers["up"].APIKey)
	}
}

func TestMissingExpansionNamesVar(t *testing.T) {
	body := `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "https://api.anthropic.com"
api_key = "${TEST_TOTALLY_MISSING_VAR}"
[[models]]
name = "m"
provider = "up"
model = "x"
`
	_, err := Load(writeCfg(t, body))
	if err == nil || !strings.Contains(err.Error(), "TEST_TOTALLY_MISSING_VAR") {
		t.Fatalf("missing variable must be named: %v", err)
	}
	t.Setenv("TEST_BLANK_VAR", "   ")
	body = strings.Replace(body, "TEST_TOTALLY_MISSING_VAR", "TEST_BLANK_VAR", 1)
	if _, err := Load(writeCfg(t, body)); err == nil {
		t.Fatalf("whitespace-only variable must be rejected")
	}
}

func TestInvalidSubstitutionRejected(t *testing.T) {
	t.Setenv("TEST_OK", "v")
	body := `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "https://api.anthropic.com"
api_key = "${TEST_OK} ${"
[[models]]
name = "m"
provider = "up"
model = "x"
`
	if _, err := Load(writeCfg(t, body)); err == nil {
		t.Fatalf("leftover ${ must be rejected")
	}
}

func TestParseErrorsNeverEchoSecrets(t *testing.T) {
	secret := "hunter2-SECRET-VALUE-xyz"
	body := "[server]\nauth_mode = \"none\"\napi_key = \"" + secret + "\n"
	_, err := Load(writeCfg(t, body))
	if err == nil {
		t.Fatalf("expected parse error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("parse error echoes secret: %v", err)
	}
}

// --- review regressions: models ---

func TestMetadataRetained(t *testing.T) {
	body := `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "https://api.anthropic.com"
[[models]]
name = "m"
provider = "up"
model = "x"
metadata = { display_name = " pretty ", context_window = 200000 }
`
	cfg, err := Load(writeCfg(t, body))
	if err != nil {
		t.Fatalf("metadata must be retained, not rejected: %v", err)
	}
	m, _, ok := cfg.LookupModel("m")
	if !ok || len(m.Metadata) == 0 {
		t.Fatalf("metadata not retained: %+v", m)
	}
	if !strings.Contains(string(m.Metadata), "pretty") {
		t.Fatalf("metadata content lost: %s", m.Metadata)
	}
	// Absent metadata stays nil.
	body2 := strings.Replace(body, "metadata = { display_name = \" pretty \", context_window = 200000 }\n", "", 1)
	cfg, err = Load(writeCfg(t, body2))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	m, _, _ = cfg.LookupModel("m")
	if len(m.Metadata) != 0 {
		t.Fatalf("absent metadata must stay empty: %s", m.Metadata)
	}
}

func TestModelDerivationAndAliases(t *testing.T) {
	body := `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "https://api.anthropic.com"
[[models]]
name = "up/claude-x"
provider = "up"
aliases = ["cx"]
[[models]]
name = "plain"
provider = "up"
model = "explicit"
`
	cfg, err := Load(writeCfg(t, body))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	m, _, ok := cfg.LookupModel("up/claude-x")
	if !ok || m.Upstream != "claude-x" {
		t.Fatalf("upstream not derived: %+v", m)
	}
	m, _, ok = cfg.LookupModel("plain")
	if !ok || m.Upstream != "explicit" {
		t.Fatalf("explicit upstream lost: %+v", m)
	}
	if _, _, ok := cfg.LookupModel("cx"); !ok {
		t.Fatalf("alias not indexed")
	}
	if _, err := Load(writeCfg(t, strings.Replace(body, `aliases = ["cx"]`, `aliases = ["  "]`, 1))); err == nil {
		t.Fatalf("blank aliases must be rejected")
	}
}

func TestModelDialectOverrideRejected(t *testing.T) {
	dialect := `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "https://api.anthropic.com"
[[models]]
name = "m"
provider = "up"
model = "x"
dialect = "gemini"
`
	cfg, err := Load(writeCfg(t, dialect))
	if err != nil {
		t.Fatalf("per-route dialect override must now be admitted: %v", err)
	}
	m, p, ok := cfg.LookupModel("m")
	if !ok {
		t.Fatalf("override route must resolve")
	}
	if got := m.EffectiveDialect(p); got != "gemini" {
		t.Fatalf("effective dest = %q, want gemini", got)
	}
	if p.Dialect != "anthropic" {
		t.Fatalf("provider identity must stay anthropic, got %q", p.Dialect)
	}
}

func TestModelEnabledFalseReserved(t *testing.T) {
	body := `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "https://api.anthropic.com"
[[models]]
name = "m"
provider = "up"
model = "x"
enabled = false
`
	cfg, err := Load(writeCfg(t, body))
	if err != nil {
		t.Fatalf("enabled=false must be accepted as reserved: %v", err)
	}
	m, _, ok := cfg.LookupModel("m")
	if !ok {
		t.Fatalf("disabled name must stay reserved in the index")
	}
	if !m.Disabled {
		t.Fatalf("disabled route must be marked")
	}
}

func TestModelEnabledStrictBool(t *testing.T) {
	for _, line := range []string{`enabled = "false"`, `enabled = 0`, `enabled = 1`} {
		body := `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "https://api.anthropic.com"
[[models]]
name = "m"
provider = "up"
model = "x"
` + line + "\n"
		if _, err := Load(writeCfg(t, body)); err == nil {
			t.Fatalf("non-boolean %q must be rejected", line)
		}
	}
}

func TestModelOmittedEnabledStaysEnabled(t *testing.T) {
	t.Setenv("TEST_UP_KEY_M2", "dummy")
	cfg, err := Load(writeCfg(t, minimal))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	m, _, ok := cfg.LookupModel("claude-haiku")
	if !ok || m.Disabled {
		t.Fatalf("omitted enabled must stay enabled: %+v", m)
	}
}

func TestProviderNameValidation(t *testing.T) {
	body := `
[server]
auth_mode = "none"
[providers."a/b"]
dialect = "anthropic"
base_url = "https://api.anthropic.com"
[[models]]
name = "m"
provider = "a/b"
model = "x"
`
	if _, err := Load(writeCfg(t, body)); err == nil {
		t.Fatalf("provider names must reject '/'")
	}
	body = `
[server]
auth_mode = "none"
[providers." padded "]
dialect = "anthropic"
base_url = "https://api.anthropic.com"
`
	if _, err := Load(writeCfg(t, body)); err == nil {
		t.Fatalf("provider names must reject padding")
	}
}

func TestRetryAndCompatAgreement(t *testing.T) {
	t.Setenv("TEST_UP_KEY_M2", "dummy")
	with := func(extra string) string {
		return strings.Replace(minimal, "max_attempts = 1", "max_attempts = 1\n"+extra, 1)
	}
	if _, err := Load(writeCfg(t, with("statuses = [200]"))); err == nil {
		t.Fatalf("non-retryable statuses must be rejected")
	}
	if _, err := Load(writeCfg(t, with("backoff_base = 9\nbackoff_max = 8"))); err == nil {
		t.Fatalf("backoff_base > backoff_max must be rejected")
	}
	body := strings.Replace(minimal, "[providers.up]",
		"[providers.up]\ncompatibility = \"strict\"\ndrop_params = true", 1)
	if _, err := Load(writeCfg(t, body)); err == nil {
		t.Fatalf("drop_params must agree with compatibility")
	}
	body = strings.Replace(minimal, "[providers.up]",
		"[providers.up]\ndrop_params = true", 1)
	cfg, err := Load(writeCfg(t, body))
	if err != nil {
		t.Fatalf("legacy drop_params=true must select lossy: %v", err)
	}
	if !cfg.Providers["up"].IsLossy() {
		t.Fatalf("lossy not selected")
	}
}

func TestUnknownTopTableHint(t *testing.T) {
	t.Setenv("TEST_UP_KEY_M2", "dummy")
	for _, tbl := range []string{"nope"} {
		body := minimal + "\n[" + tbl + "]\n"
		_, err := Load(writeCfg(t, body))
		if err == nil {
			t.Fatalf("expected rejection of [%s]", tbl)
		}
		if tbl != "nope" && !strings.Contains(err.Error(), "milestone") {
			t.Fatalf("expected migration hint for [%s]: %v", tbl, err)
		}
	}
	// [llama_cpp] is the accepted llama discovery table: an empty one
	// falls back to the conventional local default, never a hint. The
	// controlled env below pins the fallback chain off the real env.
	t.Setenv("LLAMA_SERVER_URL", "")
	if _, err := Load(writeCfg(t, minimal+"\n[llama_cpp]\n")); err != nil {
		t.Fatalf("empty [llama_cpp] must be accepted with the local default: %v", err)
	}
	// [catalog] is the accepted discovery table: an empty one fails
	// validation (url required), never a milestone hint.
	if _, err := Load(writeCfg(t, minimal+"\n[catalog]\n")); err == nil ||
		strings.Contains(err.Error(), "milestone") {
		t.Fatalf("empty [catalog] must fail validation without a milestone hint: %v", err)
	}
}

func TestSessionPolicyRequiresHeader(t *testing.T) {
	body := `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "https://api.anthropic.com"
session_source_headers = []
[[models]]
name = "m"
provider = "up"
model = "x"
`
	if _, err := Load(writeCfg(t, body)); err == nil {
		t.Fatalf("session policy without session_header must fail")
	}
}

func TestCountTokensTypeStrict(t *testing.T) {
	t.Setenv("TEST_UP_KEY_M2", "dummy")
	body := strings.Replace(minimal, "[providers.up]",
		"[providers.up]\ncount_tokens = \"yes\"", 1)
	if _, err := Load(writeCfg(t, body)); err == nil {
		t.Fatalf("non-boolean count_tokens must be rejected")
	}
}
