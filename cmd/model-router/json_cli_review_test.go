package main

// Independent bounded adversarial review: JSON/CLI surface of the Go router.
//
// Scope: --config/-c last-value and explicit-empty rejection, host/port
// supplied-presence bounds, --help/-h/--version zero I/O, positional
// rejection value-free, --verbose bounded counts, default-file order with
// YAML migration, unknown-suffix JSON-then-TOML, credential env flows,
// keyless compat before/after --api-key, and release version identity
// (CLI --version vs server index sharing one immutable BuildVersion).
// No production edits; hermetic tempdirs, controlled HOME/XDG, fakes only.
// Existing helpers runCapture/flagTestEnv/writeFlagCfg are reused.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arhammahajan/model-router/internal/config"
	"github.com/arhammahajan/model-router/internal/server"
)

func TestJSONCLIReviewConfigAliasLastValueEmptyReject(t *testing.T) {
	flagTestEnv(t)
	dir := t.TempDir()
	valid := writeFlagCfg(t, dir, "valid.toml", flagStaticBody)
	missing := filepath.Join(dir, "does-not-exist.toml")
	// Last value wins across mixed long/short spellings.
	for _, args := range [][]string{
		{"--config", missing, "--config", valid, "--validate-config"},
		{"--config", missing, "-c", valid, "--validate-config"},
		{"-c", missing, "--config", valid, "--validate-config"},
		{"-c", missing, "-c", valid, "--validate-config"},
		{"--config", valid, "--config", valid, "--validate-config"},
	} {
		if code, out := runCapture(t, args); code != 0 {
			t.Fatalf("last --config value must win: %v -> %d %s", args, code, out)
		}
	}
	for _, args := range [][]string{
		{"--config", valid, "--config", missing, "--validate-config"},
		{"--config", valid, "-c", missing, "--validate-config"},
	} {
		if code, _ := runCapture(t, args); code != 2 {
			t.Fatalf("last missing value must fail: %v -> %d", args, code)
		}
	}
	// Explicit empty (space or equals form, either spelling) is rejected.
	for _, args := range [][]string{
		{"--config", "", "--validate-config"},
		{"-c", "", "--validate-config"},
		{"--config=", "--validate-config"},
		{"--config", valid, "--config=", "--validate-config"},
		{"--config", valid, "-c", "", "--validate-config"},
	} {
		if code, out := runCapture(t, args); code != 2 {
			t.Fatalf("explicit empty --config must exit 2: %v -> %d %s", args, code, out)
		} else if strings.Contains(out, valid) && strings.Contains(out, ".toml\n") {
			t.Fatalf("empty-config error must not echo paths verbosely: %q", out)
		}
	}
}

func TestJSONCLIReviewHostPortPresenceBounds(t *testing.T) {
	flagTestEnv(t)
	dir := t.TempDir()
	cfg := writeFlagCfg(t, dir, "router.toml", flagStaticBody)
	// Omitted flags leave the file values alone (flag defaults 0/""
	// must not override when unprovided).
	if code, _ := runCapture(t, []string{"--config", cfg, "--validate-config"}); code != 0 {
		t.Fatalf("omitted host/port must validate, got %d", code)
	}
	// Supplied bounds are strict.
	for _, port := range []string{"1", "4000", "65535"} {
		if code, out := runCapture(t, []string{"--config", cfg, "--port", port, "--validate-config"}); code != 0 {
			t.Fatalf("--port %s must validate, got %d %s", port, code, out)
		}
	}
	for _, port := range []string{"0", "-1", "65536", "70000", "abc", "4000.5", ""} {
		args := []string{"--config", cfg, "--validate-config", "--port", port}
		if port == "" {
			args = []string{"--config", cfg, "--validate-config", "--port="}
		}
		if code, _ := runCapture(t, args); code != 2 {
			t.Fatalf("--port %q must exit 2, got %d", port, code)
		}
	}
	if code, _ := runCapture(t, []string{"--config", cfg, "--port=0", "--validate-config"}); code != 2 {
		t.Fatalf("--port=0 equals-form must exit 2")
	}
	// Host: blank rejected, padded value trimmed and admitted.
	for _, h := range []string{"", "   ", "\t"} {
		if code, _ := runCapture(t, []string{"--config", cfg, "--host", h, "--validate-config"}); code != 2 {
			t.Fatalf("blank --host %q must exit 2", h)
		}
	}
	if code, _ := runCapture(t, []string{"--config", cfg, "--host=", "--validate-config"}); code != 2 {
		t.Fatalf("--host= equals-empty must exit 2")
	}
	if code, out := runCapture(t, []string{"--config", cfg, "--host", "  127.0.0.1  ", "--validate-config"}); code != 0 {
		t.Fatalf("padded host must trim and validate, got %d %s", code, out)
	}
}

func TestJSONCLIReviewHelpVersionZeroIO(t *testing.T) {
	flagTestEnv(t)
	empty := t.TempDir()
	cwd, _ := os.Getwd()
	defer os.Chdir(cwd)
	if err := os.Chdir(empty); err != nil {
		t.Fatal(err)
	}
	// No config present: help/version still exit 0 and touch nothing.
	for _, args := range [][]string{{"--help"}, {"-h"}, {"--version"}} {
		before, _ := os.ReadDir(empty)
		code, _ := runCapture(t, args)
		if code != 0 {
			t.Fatalf("%v without config must exit 0, got %d", args, code)
		}
		after, _ := os.ReadDir(empty)
		if len(before) != len(after) {
			t.Fatalf("%v must perform zero I/O, dir changed", args)
		}
	}
	// Help/version ignore even a broken explicit config (parse runs after).
	broken := filepath.Join(empty, "broken.toml")
	if err := os.WriteFile(broken, []byte("[[["), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--help", "--config", broken}, {"-h", "--config", broken}, {"--version", "--config", broken}} {
		if code, _ := runCapture(t, args); code != 0 {
			t.Fatalf("%v with broken config must still exit 0", args)
		}
	}
	// Version names the binary; positionals never leak through errors.
	code, out := runCapture(t, []string{"--config", broken, "--validate-config", "sekrit-positional-zz9"})
	if code != 2 || strings.Contains(out, "sekrit-positional-zz9") {
		t.Fatalf("positional must exit 2 value-free, got %d %q", code, out)
	}
	// --version bypasses positional rejection by design (flag contract);
	// --help behaves the same.
	if code, _ := runCapture(t, []string{"--version", "extra-arg"}); code != 0 {
		t.Fatalf("--version must win over positionals")
	}
}

func TestJSONCLIReviewVerboseBoundedCounts(t *testing.T) {
	flagTestEnv(t)
	dir := t.TempDir()
	body := `
[server]
auth_mode = "none"
allow_dynamic_routes = false
[retry]
max_attempts = 1
[providers.upA]
dialect = "anthropic"
base_url = "http://127.0.0.1:9"
[providers.upB]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
[[models]]
name = "route-one"
provider = "upA"
model = "m1"
[[models]]
name = "route-two"
provider = "upB"
model = "m2"
[catalog]
url = "http://127.0.0.1:9/models"
timeout = 1
refresh_seconds = 0
model_providers = { "cat-a" = "upA" }
[discovery_cache]
path = "` + filepath.Join(dir, "dc.json") + `"
`
	cfg := writeFlagCfg(t, dir, "router.toml", body)
	code, plain := runCapture(t, []string{"--config", cfg, "--validate-config"})
	if code != 0 {
		t.Fatalf("validate: %d %s", code, plain)
	}
	if strings.Contains(plain, "config_loaded") {
		t.Fatalf("quiet run must not emit diagnostics")
	}
	code, verbose := runCapture(t, []string{"--config", cfg, "--validate-config", "--verbose"})
	if code != 0 {
		t.Fatalf("verbose validate: %d %s", code, verbose)
	}
	line := ""
	for _, l := range strings.Split(verbose, "\n") {
		if strings.Contains(l, "debug config_loaded") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("verbose must emit one config_loaded line: %q", verbose)
	}
	for _, want := range []string{"providers=2", "configured_models=2", "catalog=true", "cache=true", "llama=false"} {
		if !strings.Contains(line, want) {
			t.Fatalf("verbose counts wrong, want %q in %q", want, line)
		}
	}
	// Bounded: counts only — no route IDs, URLs, keys, or body logging.
	for _, leak := range []string{"route-one", "route-two", "upA", "cat-a", "127.0.0.1", "http", "sk-", "gpt", "bearer", "token", "body", "content"} {
		if strings.Contains(strings.ToLower(line), strings.ToLower(leak)) {
			t.Fatalf("verbose line must not carry %q: %q", leak, line)
		}
	}
	final := strings.TrimSpace(verbose[strings.LastIndex(verbose, "\n{"):])
	var shape map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(final)), &shape); err != nil {
		t.Fatalf("validate final line must stay JSON: %q", verbose)
	}
}

func TestJSONCLIReviewDefaultOrderAndSuffix(t *testing.T) {
	flagTestEnv(t)
	// router.toml beats router.json: invalid JSON + valid TOML succeeds.
	dir := t.TempDir()
	writeFlagCfg(t, dir, "router.json", `{{{not-json`)
	writeFlagCfg(t, dir, "router.toml", flagStaticBody)
	cwd, _ := os.Getwd()
	defer os.Chdir(cwd)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if code, out := runCapture(t, []string{"--validate-config"}); code != 0 {
		t.Fatalf("router.toml must shadow router.json, got %d %s", code, out)
	}
	// And a broken router.toml is selected (not skipped for good json).
	dir2 := t.TempDir()
	writeFlagCfg(t, dir2, "router.json", `{"server": {"auth_mode": "none"}, "providers": {"up": {"dialect": "anthropic", "base_url": "http://127.0.0.1:9"}}, "models": [{"name": "m", "provider": "up", "model": "x"}]}`)
	writeFlagCfg(t, dir2, "router.toml", `[[[broken`)
	if err := os.Chdir(dir2); err != nil {
		t.Fatal(err)
	}
	if code, _ := runCapture(t, []string{"--validate-config"}); code != 2 {
		t.Fatalf("broken router.toml must be selected over valid router.json, got %d", code)
	}
	// router.toml beats router.yaml (no migration error when TOML exists).
	dir3 := t.TempDir()
	writeFlagCfg(t, dir3, "router.toml", flagStaticBody)
	writeFlagCfg(t, dir3, "router.yaml", "server:\n  auth_mode: none\n")
	if err := os.Chdir(dir3); err != nil {
		t.Fatal(err)
	}
	if code, out := runCapture(t, []string{"--validate-config"}); code != 0 {
		t.Fatalf("router.toml must shadow router.yaml, got %d %s", code, out)
	}
	// Unknown suffix via explicit --config: JSON then TOML.
	dir4 := t.TempDir()
	jcfg := writeFlagCfg(t, dir4, "r.cfg", `{"server": {"auth_mode": "none"}, "providers": {"up": {"dialect": "anthropic", "base_url": "http://127.0.0.1:9"}}, "models": [{"name": "m", "provider": "up", "model": "x"}]}`)
	if err := os.Chdir(dir4); err != nil {
		t.Fatal(err)
	}
	if code, _ := runCapture(t, []string{"--config", jcfg, "--validate-config"}); code != 0 {
		t.Fatalf("unknown suffix JSON must load, got %d", code)
	}
	tcfg := writeFlagCfg(t, dir4, "t.weird", flagStaticBody)
	if code, _ := runCapture(t, []string{"--config", tcfg, "--validate-config"}); code != 0 {
		t.Fatalf("unknown suffix TOML fallback must load, got %d", code)
	}
	// Casefold suffix through the CLI.
	ucfg := writeFlagCfg(t, dir4, "U.JSON", `{"server": {"auth_mode": "none"}, "providers": {"up": {"dialect": "anthropic", "base_url": "http://127.0.0.1:9"}}, "models": [{"name": "m", "provider": "up", "model": "x"}]}`)
	if code, _ := runCapture(t, []string{"--config", ucfg, "--validate-config"}); code != 0 {
		t.Fatalf("casefold .JSON must load, got %d", code)
	}
	// YAML payload under a YAML name never half-parses TOML/JSON.
	ycfg := writeFlagCfg(t, dir4, "y.yaml", flagStaticBody)
	if code, out := runCapture(t, []string{"--config", ycfg, "--validate-config"}); code != 2 || !strings.Contains(out, "use TOML/JSON") {
		t.Fatalf("yaml suffix must be a migration error, got %d %q", code, out)
	}
}

func TestJSONCLIReviewCredentialEnvFlows(t *testing.T) {
	flagTestEnv(t)
	dir := t.TempDir()
	// export prefix, quoting and trailing comments parse; malformed lines skip.
	env := "export TEST_JR_CLI_A=first-val # trailing comment\n" +
		"TEST_JR_CLI_B=\"quoted val\"\n" +
		"TEST_JR_CLI_C='single'\n" +
		"not-a-valid-line-without-equals\n" +
		"=novalue\n"
	f1 := writeFlagCfg(t, dir, "a.env", env)
	f2 := writeFlagCfg(t, dir, "b.env", "TEST_JR_CLI_A=second-val\n")
	body := `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "http://127.0.0.1:9"
api_key_env = "TEST_JR_CLI_A"
[[models]]
name = "m"
provider = "up"
model = "x"
`
	cfg := writeFlagCfg(t, dir, "router.toml", body)
	os.Unsetenv("TEST_JR_CLI_A")
	code, out := runCapture(t, []string{"--config", cfg, "--env-file", f1, "--env-file", f2, "--validate-config"})
	if code != 0 {
		t.Fatalf("first env file must win, got %d %s", code, out)
	}
	if got := os.Getenv("TEST_JR_CLI_B"); got != "quoted val" {
		t.Fatalf("quoted value wrong: %q", got)
	}
	// Existing process env (even present-empty) wins over files.
	os.Unsetenv("TEST_JR_CLI_A")
	t.Setenv("TEST_JR_CLI_A", "")
	if code, out := runCapture(t, []string{"--config", cfg, "--env-file", f1, "--validate-config"}); code != 2 || !strings.Contains(out, "TEST_JR_CLI_A") {
		t.Fatalf("present-empty must win and name the var, got %d %q", code, out)
	}
	os.Unsetenv("TEST_JR_CLI_A")
	// Missing explicit file is strict and value-free.
	if code, out := runCapture(t, []string{"--config", cfg, "--env-file", filepath.Join(dir, "missing.env"), "--validate-config"}); code != 2 {
		t.Fatalf("missing env file must exit 2, got %d %s", code, out)
	} else if strings.Contains(out, "first-val") || strings.Contains(out, "second-val") {
		t.Fatalf("env error must not echo values: %q", out)
	}
	// --no-default-env isolates ambient default credential files.
	xdg := t.TempDir()
	if err := os.MkdirAll(filepath.Join(xdg, "ai"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(xdg, "ai", "credentials.env"), []byte("TEST_JR_CLI_D=from-default\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", xdg)
	dBody := `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "http://127.0.0.1:9"
api_key_env = "TEST_JR_CLI_D"
[[models]]
name = "m"
provider = "up"
model = "x"
`
	dcfg := writeFlagCfg(t, dir, "d.toml", dBody)
	os.Unsetenv("TEST_JR_CLI_D")
	if code, _ := runCapture(t, []string{"--config", dcfg, "--validate-config"}); code != 0 {
		t.Fatalf("default creds must load, got %d", code)
	}
	os.Unsetenv("TEST_JR_CLI_D")
	if code, _ := runCapture(t, []string{"--config", dcfg, "--validate-config", "--no-default-env"}); code != 2 {
		t.Fatalf("--no-default-env must isolate default creds")
	}
}

func TestJSONCLIReviewCompatBeforeAfterAPIKey(t *testing.T) {
	flagTestEnv(t)
	dir := t.TempDir()
	compat := `
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
	cfg := writeFlagCfg(t, dir, "router.toml", compat)
	// Before: keyless compat admitted with zero I/O side effects.
	if code, out := runCapture(t, []string{"--config", cfg, "--validate-config"}); code != 0 {
		t.Fatalf("keyless compat must validate, got %d %s", code, out)
	}
	// After: --api-key on either side of --config trips the same boundary,
	// value-free, before any probe or cache write.
	secret := "cli-canary-zzz-123"
	cacheProbe := filepath.Join(dir, "dc.json")
	for _, args := range [][]string{
		{"--config", cfg, "--api-key", secret, "--validate-config"},
		{"--api-key", secret, "--config", cfg, "--validate-config"},
	} {
		os.Remove(cacheProbe)
		code, out := runCapture(t, args)
		if code != 2 {
			t.Fatalf("%v must exit 2, got %d %s", args, code, out)
		}
		if !strings.Contains(out, "bearer_session_compat") {
			t.Fatalf("%v must name the compat boundary: %q", args, out)
		}
		if strings.Contains(out, secret) {
			t.Fatalf("compat error must not echo the CLI key: %q", out)
		}
		if _, err := os.Stat(cacheProbe); !os.IsNotExist(err) {
			t.Fatalf("compat rejection must precede cache I/O")
		}
	}
}

func TestJSONCLIReviewVersionMatchesServerIndex(t *testing.T) {
	flagTestEnv(t)
	before := server.BuildVersion
	if before == "" {
		t.Fatalf("BuildVersion must be non-empty")
	}
	code, out := runCapture(t, []string{"--version"})
	if code != 0 || !strings.Contains(out, "model-router-go "+before) {
		t.Fatalf("version must print binary and BuildVersion, got %d %q", code, out)
	}
	if server.BuildVersion != before {
		t.Fatalf("BuildVersion mutated at runtime: %q -> %q", before, server.BuildVersion)
	}
	// The served endpoint index shares the same BuildVersion: GET / over
	// the in-process handler, no listeners or network.
	dir := t.TempDir()
	cfgPath := writeFlagCfg(t, dir, "router.toml", flagStaticBody)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(cfg)
	hsrv := srv.HTTPServer("127.0.0.1:0")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	hsrv.Handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("index must serve 200, got %d", rec.Code)
	}
	var idx struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &idx); err != nil {
		t.Fatalf("index must be JSON: %v", err)
	}
	if idx.Name != "model-router-go" || idx.Version != before {
		t.Fatalf("index identity must share BuildVersion: %+v want %q", idx, before)
	}
	if !strings.HasSuffix(strings.TrimSpace(out), before) {
		t.Fatalf("version output must end with BuildVersion %q: %q", before, out)
	}
}

func TestJSONCLIReviewRootCheapCasesPreserved(t *testing.T) {
	flagTestEnv(t)
	dir := t.TempDir()
	valid := writeFlagCfg(t, dir, "valid.toml", flagStaticBody)
	// Long-alias override: -c shares last-value semantics with --config.
	missing := filepath.Join(dir, "nope.toml")
	if code, _ := runCapture(t, []string{"-c", missing, "--config", valid, "--validate-config"}); code != 0 {
		t.Fatalf("-c/--config alias last-value must be preserved")
	}
	// Empty host and zero port stay rejected.
	if code, _ := runCapture(t, []string{"--config", valid, "--host", "", "--validate-config"}); code != 2 {
		t.Fatalf("empty --host rejection must be preserved")
	}
	if code, _ := runCapture(t, []string{"--config", valid, "--port", "0", "--validate-config"}); code != 2 {
		t.Fatalf("zero --port rejection must be preserved")
	}
	// Positionals stay rejected without echoing the value.
	if code, out := runCapture(t, []string{"--config", valid, "--validate-config", "VALUE-LEAK-PROBE"}); code != 2 || strings.Contains(out, "VALUE-LEAK-PROBE") {
		t.Fatalf("positional rejection must be preserved value-free: %d %q", code, out)
	}
	// Verbose stays a single bounded counts line with no identifiers.
	code, verbose := runCapture(t, []string{"--config", valid, "--validate-config", "--verbose"})
	if code != 0 || !strings.Contains(verbose, "debug config_loaded") {
		t.Fatalf("verbose marker must be preserved: %d %q", code, verbose)
	}
	for _, leak := range []string{"gpt-4o", "127.0.0.1", "chatup"} {
		if strings.Contains(verbose, leak) {
			t.Fatalf("verbose must stay identifier-free (saw %q)", leak)
		}
	}
}
