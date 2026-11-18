package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeLoadFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func isolateEnv(t *testing.T) {
	t.Helper()
	t.Setenv("MODEL_ROUTER_API_KEY", "")
	t.Setenv("MODEL_ROUTER_API_KEYS", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("TEST_UP_KEY_M2", "dummy")
	empty := t.TempDir()
	t.Setenv("HOME", empty)
	t.Setenv("XDG_CONFIG_HOME", empty)
}

func TestLoadJSONStrictIntBool(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	body := `{
  "server": {"auth_mode": "none", "port": 4000, "allow_dynamic_routes": false},
  "retry": {"max_attempts": 1},
  "providers": {"up": {"dialect": "anthropic", "base_url": "https://api.anthropic.com"}},
  "models": [{"name": "m", "provider": "up", "model": "x"}]
}`
	p := writeLoadFile(t, dir, "router.json", body)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("JSON load: %v", err)
	}
	if cfg.Server.Port != 4000 || cfg.Server.AuthMode != "none" {
		t.Fatalf("bad server: %+v", cfg.Server)
	}
	// float must not satisfy int
	bad := strings.Replace(body, `"port": 4000`, `"port": 4000.0`, 1)
	p2 := writeLoadFile(t, dir, "bad.json", bad)
	if _, err := Load(p2); err == nil {
		t.Fatalf("float port must be rejected")
	}
	// bool must not satisfy int/string
	bad2 := strings.Replace(body, `"port": 4000`, `"port": true`, 1)
	p3 := writeLoadFile(t, dir, "bad2.json", bad2)
	if _, err := Load(p3); err == nil {
		t.Fatalf("bool port must be rejected")
	}
}

func TestLoadJSONSuffixCasefold(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	body := `{"server": {"auth_mode": "none"}, "providers": {"up": {"dialect": "anthropic", "base_url": "https://api.anthropic.com"}}, "models": [{"name": "m", "provider": "up", "model": "x"}]}`
	p := writeLoadFile(t, dir, "ROUTER.JSON", body)
	if _, err := Load(p); err != nil {
		t.Fatalf("casefold suffix: %v", err)
	}
	p2 := writeLoadFile(t, dir, "ROUTER.TOML", "[server]\nauth_mode = \"none\"\n[providers.up]\ndialect = \"anthropic\"\nbase_url = \"https://api.anthropic.com\"\n[[models]]\nname = \"m\"\nprovider = \"up\"\nmodel = \"x\"\n")
	if _, err := Load(p2); err != nil {
		t.Fatalf("casefold toml: %v", err)
	}
}

func TestLoadJSONTrailingData(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	body := `{"server": {"auth_mode": "none"}} {"trailing": 1}`
	p := writeLoadFile(t, dir, "router.json", body)
	if _, err := Load(p); err == nil {
		t.Fatalf("trailing data must be rejected")
	}
}

func TestLoadJSONBigintMetadataExact(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	body := `{
  "server": {"auth_mode": "none"},
  "providers": {"up": {"dialect": "anthropic", "base_url": "https://api.anthropic.com"}},
  "models": [{"name": "m", "provider": "up", "model": "x", "metadata": {"big": 9007199254740993, "small": 42}}]
}`
	p := writeLoadFile(t, dir, "router.json", body)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("bigint metadata: %v", err)
	}
	m, _, ok := cfg.LookupModel("m")
	if !ok {
		t.Fatalf("lookup")
	}
	s := string(m.Metadata)
	if !strings.Contains(s, "9007199254740993") {
		t.Fatalf("bigint must be exact, got %s", s)
	}
	if !strings.Contains(s, "42") {
		t.Fatalf("small int missing: %s", s)
	}
}

func TestLoadJSONOverflowStrictReject(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	body := `{
  "server": {"auth_mode": "none", "port": 9999999999999999999999},
  "providers": {"up": {"dialect": "anthropic", "base_url": "https://api.anthropic.com"}},
  "models": [{"name": "m", "provider": "up", "model": "x"}]
}`
	p := writeLoadFile(t, dir, "router.json", body)
	_, err := Load(p)
	if err == nil {
		t.Fatalf("overflow port must be rejected, not float-rounded")
	}
	if strings.Contains(err.Error(), "9999999999") {
		t.Fatalf("overflow error must be value-free: %v", err)
	}
}

func TestLoadJSONNullCapWithdraw(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	body := `{
  "server": {"auth_mode": "none"},
  "providers": {"up": {"dialect": "anthropic", "base_url": "https://api.anthropic.com"}},
  "models": [{"name": "m", "provider": "up", "model": "x"}],
  "capabilities": {"overrides": {"m": {"reasoning": {"supported": null}}}}
}`
	p := writeLoadFile(t, dir, "router.json", body)
	if _, err := Load(p); err != nil {
		t.Fatalf("null cap withdraw must be valid: %v", err)
	}
}

func TestLoadSchemelessJSONThenTOML(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	j := `{"server": {"auth_mode": "none"}, "providers": {"up": {"dialect": "anthropic", "base_url": "https://api.anthropic.com"}}, "models": [{"name": "m", "provider": "up", "model": "x"}]}`
	p := writeLoadFile(t, dir, "router", j)
	if _, err := Load(p); err != nil {
		t.Fatalf("schemeless JSON: %v", err)
	}
	tomlBody := "[server]\nauth_mode = \"none\"\n[providers.up]\ndialect = \"anthropic\"\nbase_url = \"https://api.anthropic.com\"\n[[models]]\nname = \"m\"\nprovider = \"up\"\nmodel = \"x\"\n"
	p2 := writeLoadFile(t, dir, "router2", tomlBody)
	if _, err := Load(p2); err != nil {
		t.Fatalf("schemeless TOML: %v", err)
	}
}

func TestLoadYAMLReject(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	p := writeLoadFile(t, dir, "router.yaml", "server:\n  auth_mode: none\n")
	_, err := Load(p)
	if err == nil {
		t.Fatalf("YAML must be rejected")
	}
	if !strings.Contains(err.Error(), "use TOML/JSON") {
		t.Fatalf("YAML error must say use TOML/JSON: %v", err)
	}
	p2 := writeLoadFile(t, dir, "router.YML", "server:\n  auth_mode: none\n")
	_, err = Load(p2)
	if err == nil || !strings.Contains(err.Error(), "use TOML/JSON") {
		t.Fatalf("YML casefold reject: %v", err)
	}
}

func TestLoadWithOptionsNoDefaultEnv(t *testing.T) {
	isolateEnv(t)
	xdg := t.TempDir()
	if err := os.MkdirAll(filepath.Join(xdg, "ai"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(xdg, "ai", "credentials.env"), []byte("TEST_NODEFAULT_KEY=from-default\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	body := "[server]\nauth_mode = \"none\"\n[providers.up]\ndialect = \"anthropic\"\nbase_url = \"https://api.anthropic.com\"\napi_key_env = \"TEST_NODEFAULT_KEY\"\n[[models]]\nname = \"m\"\nprovider = \"up\"\nmodel = \"x\"\n"
	p := writeLoadFile(t, dir, "router.toml", body)
	// default behavior loads it
	if _, err := Load(p); err != nil {
		t.Fatalf("default should load: %v", err)
	}
	// NoDefaultEnv isolates
	os.Unsetenv("TEST_NODEFAULT_KEY")
	if _, err := LoadWithOptions(p, LoadOptions{NoDefaultEnv: true}); err == nil {
		t.Fatalf("NoDefaultEnv must isolate default creds")
	}
}

func TestLoadExplicitEnvPrecedence(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	f1 := writeLoadFile(t, dir, "a.env", "TEST_PREC_A=first\nTEST_PREC_B=first-b\n")
	f2 := writeLoadFile(t, dir, "b.env", "TEST_PREC_A=second\nTEST_PREC_C=second-c\n")
	body := "[server]\nauth_mode = \"none\"\n[providers.up]\ndialect = \"anthropic\"\nbase_url = \"https://api.anthropic.com\"\napi_key_env = \"TEST_PREC_A\"\n[[models]]\nname = \"m\"\nprovider = \"up\"\nmodel = \"x\"\n"
	p := writeLoadFile(t, dir, "router.toml", body)
	os.Unsetenv("TEST_PREC_A")
	os.Unsetenv("TEST_PREC_B")
	os.Unsetenv("TEST_PREC_C")
	cfg, err := LoadWithOptions(p, LoadOptions{EnvFiles: []string{f1, f2}})
	if err != nil {
		t.Fatalf("explicit load: %v", err)
	}
	if cfg.Providers["up"].APIKey != "first" {
		t.Fatalf("first file must win, got %q", cfg.Providers["up"].APIKey)
	}
	if os.Getenv("TEST_PREC_B") != "first-b" || os.Getenv("TEST_PREC_C") != "second-c" {
		t.Fatalf("both files must load distinct keys")
	}
	// existing env wins incl present empty
	os.Unsetenv("TEST_PREC_A")
	os.Unsetenv("TEST_PREC_B")
	os.Unsetenv("TEST_PREC_C")
	t.Setenv("TEST_PREC_A", "")
	// need fresh process env? t.Setenv sets empty present; loader must not overwrite
	if _, err := LoadWithOptions(p, LoadOptions{EnvFiles: []string{f1}}); err == nil {
		t.Fatalf("present-empty must win, so missing key error expected")
	} else if !strings.Contains(err.Error(), "TEST_PREC_A") {
		t.Fatalf("must name var: %v", err)
	}
}

func TestLoadExplicitMissingStrict(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	p := writeLoadFile(t, dir, "router.toml", "[server]\nauth_mode=\"none\"\n[providers.up]\ndialect=\"anthropic\"\nbase_url=\"https://api.anthropic.com\"\n[[models]]\nname=\"m\"\nprovider=\"up\"\nmodel=\"x\"\n")
	missing := filepath.Join(dir, "nope.env")
	_, err := LoadWithOptions(p, LoadOptions{EnvFiles: []string{missing}})
	if err == nil {
		t.Fatalf("missing explicit file must fail")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("value-free: %v", err)
	}
	// private file content must never echo
	priv := writeLoadFile(t, dir, "priv.env", "K=hunter2-secret-value\n")
	_ = priv
	// malformed json must not echo secrets
	badJSON := writeLoadFile(t, dir, "bad.json", `{"server": {"auth_mode": "hunter2-secret-value"`)
	if _, err := Load(badJSON); err != nil && strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("malformed json must not echo: %v", err)
	}
}
