package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigFlagAliasesUseLastValue(t *testing.T) {
	flagTestEnv(t)
	dir := t.TempDir()
	valid := writeFlagCfg(t, dir, "valid.toml", flagStaticBody)
	invalid := filepath.Join(dir, "does-not-exist.toml")
	for _, args := range [][]string{
		{"--config", invalid, "-c", valid, "--validate-config"},
		{"-c", invalid, "--config", valid, "--validate-config"},
	} {
		if code, out := runCapture(t, args); code != 0 {
			t.Fatalf("aliases must share last-value semantics: %d %s", code, out)
		}
	}
}

func TestConfigExplicitEmptyFlagsReject(t *testing.T) {
	flagTestEnv(t)
	valid := writeFlagCfg(t, t.TempDir(), "valid.toml", flagStaticBody)
	for _, flags := range [][]string{{"--host", ""}, {"--port", "0"}, {"--port", "65536"}} {
		args := append([]string{"--config", valid, "--validate-config"}, flags...)
		if code, out := runCapture(t, args); code != 2 {
			t.Fatalf("explicit invalid flag must reject: %v -> %d %s", flags, code, out)
		}
	}
}

func TestConfigPositionalArgumentsRejectBeforeLoading(t *testing.T) {
	flagTestEnv(t)
	valid := writeFlagCfg(t, t.TempDir(), "valid.toml", flagStaticBody)
	if code, out := runCapture(t, []string{"--config", valid, "--validate-config", "unexpected-secret-argument"}); code != 2 || strings.Contains(out, "unexpected-secret-argument") {
		t.Fatalf("positionals must reject value-free: %d %s", code, out)
	}
}

func TestConfigVerboseHasSafeRouterOutput(t *testing.T) {
	flagTestEnv(t)
	valid := writeFlagCfg(t, t.TempDir(), "valid.toml", flagStaticBody)
	code, plain := runCapture(t, []string{"--config", valid, "--validate-config"})
	if code != 0 {
		t.Fatal(code, plain)
	}
	code, verbose := runCapture(t, []string{"--config", valid, "--validate-config", "--verbose"})
	if code != 0 || !strings.Contains(verbose, "debug config_loaded") || strings.Contains(plain, "debug config_loaded") || strings.Contains(verbose, "gpt-4o") || strings.Contains(verbose, "127.0.0.1") {
		t.Fatalf("verbose must add only safe router diagnostics: %d %s", code, verbose)
	}
}
