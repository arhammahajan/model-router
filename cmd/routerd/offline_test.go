package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CLI operational proofs: exact exits and final-JSON shapes for validate,
// online check (ready/unready), offline variants, usage errors and the
// --check alias. Hermetic: closed-port upstreams, controlled HOME/XDG,
// tempfiles only; offline paths assert no cache file appears.

func writeOfflineCfg(t *testing.T, dir, catalogURL string) string {
	t.Helper()
	p := filepath.Join(dir, "router.toml")
	body := `
[server]
auth_mode = "none"
allow_dynamic_routes = false
[retry]
max_attempts = 1
[providers.up]
dialect = "anthropic"
base_url = "http://127.0.0.1:9"
[[models]]
name = "a"
provider = "up"
model = "x"
[catalog]
url = "` + catalogURL + `"
timeout = 1
refresh_seconds = 0
model_providers = { "cat-a" = "up" }
[discovery_cache]
path = "` + filepath.Join(dir, "cache.json") + `"
`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func runCapture(t *testing.T, args []string) (int, string) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = w, w
	code := run(args)
	_ = w.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	var sb strings.Builder
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			sb.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	_ = r.Close()
	return code, sb.String()
}

func lastJSONLine(t *testing.T, out string) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var body map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &body); err != nil {
		t.Fatalf("final line must be JSON: %q: %v", out, err)
	}
	return body
}

func TestOfflineCheckIsIndeterminate(t *testing.T) {
	catalogTestEnv(t)
	dir := t.TempDir()
	cfg := writeOfflineCfg(t, dir, "http://127.0.0.1:9/models")
	code, out := runCapture(t, []string{"--config", cfg, "--check-readiness", "--offline"})
	if code != 4 {
		t.Fatalf("offline check must exit 4, got %d (%s)", code, out)
	}
	body := lastJSONLine(t, out)
	if body["status"] != "indeterminate" || body["network_checked"] != false ||
		body["reason"] != "offline_readiness_requires_discovery" {
		t.Fatalf("offline JSON shape wrong: %v", body)
	}
	if _, err := os.Stat(filepath.Join(dir, "cache.json")); !os.IsNotExist(err) {
		t.Fatal("offline check must not touch the cache filesystem")
	}
}

func TestOfflineValidateAndServeUsage(t *testing.T) {
	catalogTestEnv(t)
	dir := t.TempDir()
	cfg := writeOfflineCfg(t, dir, "http://127.0.0.1:9/models")
	if code, _ := runCapture(t, []string{"--config", cfg, "--validate-config", "--offline"}); code != 0 {
		t.Fatalf("offline validate must exit 0, got %d", code)
	}
	if code, _ := runCapture(t, []string{"--config", cfg, "--offline"}); code != 2 {
		t.Fatalf("offline serve must be a usage error, got %d", code)
	}
	if code, _ := runCapture(t, []string{"--config", cfg, "--check-readiness", "--offline", "--validate-config"}); code != 2 {
		t.Fatalf("conflicting flags must exit 2, got %d", code)
	}
}

func TestCheckAliasAndConfigErrors(t *testing.T) {
	catalogTestEnv(t)
	dir := t.TempDir()
	cfg := writeOfflineCfg(t, dir, "http://127.0.0.1:9/models")
	// --check aliases --check-readiness (static explicit route is ready).
	code, out := runCapture(t, []string{"--config", cfg, "--check"})
	if code != 0 {
		t.Fatalf("--check must exit 0 when ready, got %d (%s)", code, out)
	}
	body := lastJSONLine(t, out)
	if body["ready"] != true || body["status"] != "ready" {
		t.Fatalf("ready JSON shape wrong: %v", body)
	}
	if _, ok := body["discovery"]; !ok {
		t.Fatalf("check JSON must merge discovery summary: %v", body)
	}
	// Missing config is a config error, not a runtime failure.
	if code, _ := runCapture(t, []string{"--config", filepath.Join(dir, "nope.toml"), "--check-readiness"}); code != 2 {
		t.Fatalf("missing config must exit 2, got %d", code)
	}
	// Bad flag usage exits 2.
	if code, _ := runCapture(t, []string{"--bogus-flag"}); code != 2 {
		t.Fatalf("bad flags must exit 2, got %d", code)
	}
}

func TestValidateJSONShape(t *testing.T) {
	catalogTestEnv(t)
	dir := t.TempDir()
	cfg := writeOfflineCfg(t, dir, "http://127.0.0.1:9/models")
	code, out := runCapture(t, []string{"--config", cfg, "--validate-config"})
	if code != 0 {
		t.Fatalf("validate must exit 0, got %d", code)
	}
	body := lastJSONLine(t, out)
	if body["status"] != "valid" || body["network_checked"] != false || len(body) != 2 {
		t.Fatalf("validate JSON shape wrong: %v", body)
	}
}
