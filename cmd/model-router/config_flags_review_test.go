package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func flagTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv("MODEL_ROUTER_API_KEY", "")
	t.Setenv("MODEL_ROUTER_API_KEYS", "")
	t.Setenv("LLAMA_SERVER_URL", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func writeFlagCfg(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const flagStaticBody = `
[server]
auth_mode = "none"
allow_dynamic_routes = false
[retry]
max_attempts = 1
[providers.chatup]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
[[models]]
name = "m-a"
provider = "chatup"
model = "gpt-4o"
`

func TestFlagDefaultSelectionOrder(t *testing.T) {
	flagTestEnv(t)
	dir := t.TempDir()
	// only router.json present -> must be selected
	p := writeFlagCfg(t, dir, "router.json", `{"server": {"auth_mode": "none"}, "providers": {"up": {"dialect": "anthropic", "base_url": "http://127.0.0.1:9"}}, "models": [{"name": "m", "provider": "up", "model": "x"}]}`)
	cwd, _ := os.Getwd()
	defer os.Chdir(cwd)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	_ = p
	if code, _ := runCapture(t, []string{"--validate-config"}); code != 0 {
		t.Fatalf("default router.json must be found, got %d", code)
	}
}

func TestFlagYAMLDefaultMigration(t *testing.T) {
	flagTestEnv(t)
	dir := t.TempDir()
	writeFlagCfg(t, dir, "router.yaml", "server:\n  auth_mode: none\n")
	cwd, _ := os.Getwd()
	defer os.Chdir(cwd)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	code, out := runCapture(t, []string{"--validate-config"})
	if code != 2 {
		t.Fatalf("yaml default must exit 2, got %d (%s)", code, out)
	}
	if !strings.Contains(out, "use TOML/JSON") {
		t.Fatalf("yaml migration must say use TOML/JSON: %q", out)
	}
}

func TestFlagHelpZeroIO(t *testing.T) {
	flagTestEnv(t)
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "cache.json")
	_ = cachePath
	for _, args := range [][]string{{"--help"}, {"-h"}} {
		code, _ := runCapture(t, args)
		if code != 0 {
			t.Fatalf("%v must exit 0, got %d", args, code)
		}
		if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
			t.Fatalf("help must not create cache")
		}
	}
	// help with no config present must still exit 0 (no config IO)
	cwd, _ := os.Getwd()
	empty := t.TempDir()
	defer os.Chdir(cwd)
	if err := os.Chdir(empty); err != nil {
		t.Fatal(err)
	}
	if code, _ := runCapture(t, []string{"--help"}); code != 0 {
		t.Fatalf("help without config must exit 0")
	}
}

func TestFlagVersionQuickExit(t *testing.T) {
	flagTestEnv(t)
	code, out := runCapture(t, []string{"--version"})
	if code != 0 {
		t.Fatalf("version must exit 0, got %d", code)
	}
	if !strings.Contains(out, "model-router-go") {
		t.Fatalf("version output: %q", out)
	}
	// version must not require config
	cwd, _ := os.Getwd()
	empty := t.TempDir()
	defer os.Chdir(cwd)
	if err := os.Chdir(empty); err != nil {
		t.Fatal(err)
	}
	if code, _ := runCapture(t, []string{"--version"}); code != 0 {
		t.Fatalf("version without config must exit 0")
	}
}

func TestFlagAPIKeyCompatBothOrders(t *testing.T) {
	flagTestEnv(t)
	dir := t.TempDir()
	compatBody := `
[server]
auth_mode = "none"
allow_dynamic_routes = false
bearer_session_compat = true
[retry]
max_attempts = 1
[providers.chatup]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
[[models]]
name = "m-a"
provider = "chatup"
model = "gpt-4o"
`
	cfg := writeFlagCfg(t, dir, "router.toml", compatBody)
	// compat true with no CLI key is admitted
	if code, _ := runCapture(t, []string{"--config", cfg, "--validate-config"}); code != 0 {
		t.Fatalf("keyless compat must validate 0")
	}
	secret := "test-cli-secret-xyz"
	for _, args := range [][]string{
		{"--config", cfg, "--api-key", secret, "--validate-config"},
		{"--api-key", secret, "--config", cfg, "--validate-config"},
	} {
		code, out := runCapture(t, args)
		if code != 2 {
			t.Fatalf("%v compat+key must exit 2, got %d (%s)", args, code, out)
		}
		if strings.Contains(out, secret) {
			t.Fatalf("key must never echo: %q", out)
		}
		if !strings.Contains(out, "bearer_session_compat") {
			t.Fatalf("%v must fail on compat boundary, got %q", args, out)
		}
	}
}

func TestFlagAPIKeyOverrideDedup(t *testing.T) {
	flagTestEnv(t)
	dir := t.TempDir()
	cfg := writeFlagCfg(t, dir, "router.toml", flagStaticBody)
	code, out := runCapture(t, []string{"--config", cfg, "--api-key", " b ", "--api-key", "b", "--api-key", "a", "--validate-config"})
	if code != 0 {
		t.Fatalf("api-key override must validate, got %d (%s)", code, out)
	}
	if strings.Contains(out, " b ") {
		t.Fatalf("keys must be trimmed in errors/output: %q", out)
	}
}

func TestFlagHostValidation(t *testing.T) {
	flagTestEnv(t)
	dir := t.TempDir()
	cfg := writeFlagCfg(t, dir, "router.toml", flagStaticBody)
	if code, _ := runCapture(t, []string{"--config", cfg, "--host", "   ", "--validate-config"}); code != 2 {
		t.Fatalf("blank host must exit 2, got %d", code)
	}
	if code, _ := runCapture(t, []string{"--config", cfg, "--host", " 127.0.0.1 ", "--validate-config"}); code != 0 {
		t.Fatalf("trimmed host must validate, got %d", code)
	}
}

func TestFlagExplicitEnvFiles(t *testing.T) {
	flagTestEnv(t)
	dir := t.TempDir()
	f1 := writeFlagCfg(t, dir, "a.env", "TEST_FLAG_ENV_A=first\n")
	f2 := writeFlagCfg(t, dir, "b.env", "TEST_FLAG_ENV_A=second\n")
	_ = f2
	body := `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "http://127.0.0.1:9"
api_key_env = "TEST_FLAG_ENV_A"
[[models]]
name = "m"
provider = "up"
model = "x"
`
	cfg := writeFlagCfg(t, dir, "router.toml", body)
	os.Unsetenv("TEST_FLAG_ENV_A")
	code, out := runCapture(t, []string{"--config", cfg, "--env-file", f1, "--env-file", f2, "--validate-config"})
	if code != 0 {
		t.Fatalf("explicit env files must load, got %d (%s)", code, out)
	}
	os.Unsetenv("TEST_FLAG_ENV_A")
	// missing explicit file is strict 2
	code, _ = runCapture(t, []string{"--config", cfg, "--env-file", filepath.Join(dir, "missing.env"), "--validate-config"})
	if code != 2 {
		t.Fatalf("missing env file must exit 2, got %d", code)
	}
	// -c alias (static config without env dependency)
	staticCfg := writeFlagCfg(t, dir, "static.toml", flagStaticBody)
	code, out = runCapture(t, []string{"-c", staticCfg, "--validate-config"})
	if code != 0 {
		t.Fatalf("-c alias must work, got %d (%s)", code, out)
	}
}
