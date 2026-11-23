package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeReadinessCfg(t *testing.T, models string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "router.toml")
	body := `
[server]
auth_mode = "none"
allow_dynamic_routes = true
[retry]
max_attempts = 1
[providers.up]
dialect = "anthropic"
base_url = "http://127.0.0.1:9"
` + models
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCheckReadinessEnabledRule(t *testing.T) {
	t.Setenv("MODEL_ROUTER_API_KEY", "")
	t.Setenv("MODEL_ROUTER_API_KEYS", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	enabled := writeReadinessCfg(t, "[[models]]\nname = \"a\"\nprovider = \"up\"\nmodel = \"x\"\n")
	if code := run([]string{"--config", enabled, "--check-readiness"}); code != 0 {
		t.Fatalf("enabled named route must exit 0, got %d", code)
	}
	allDisabled := writeReadinessCfg(t, "[[models]]\nname = \"a\"\nprovider = \"up\"\nmodel = \"x\"\nenabled = false\n")
	if code := run([]string{"--config", allDisabled, "--check-readiness"}); code != 3 {
		t.Fatalf("all-disabled must exit 3, got %d", code)
	}
	dynamicOnly := writeReadinessCfg(t, "")
	if code := run([]string{"--config", dynamicOnly, "--check-readiness"}); code != 3 {
		t.Fatalf("dynamic-only must exit 3, got %d", code)
	}
}
