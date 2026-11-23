package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	routerenv "github.com/arhammahajan/router/internal/routercli/env"
	"github.com/arhammahajan/router/internal/routercli/modelpick"
	"github.com/arhammahajan/router/internal/routercli/routerctl"
)

func captureBoth(t *testing.T, fn func() int) (int, string, string) {
	t.Helper()
	outOrig, errOrig := os.Stdout, os.Stderr
	or, ow, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	er, ew, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = ow, ew
	defer func() {
		os.Stdout, os.Stderr = outOrig, errOrig
	}()
	code := fn()
	_ = ow.Close()
	_ = ew.Close()
	os.Stdout, os.Stderr = outOrig, errOrig
	out, _ := io.ReadAll(or)
	errs, _ := io.ReadAll(er)
	return code, string(out), string(errs)
}

func isolateEnv(t *testing.T) routerenv.Paths {
	t.Helper()
	tmp := t.TempDir()
	paths := routerenv.Paths{
		Home:       filepath.Join(tmp, "home"),
		ConfigHome: filepath.Join(tmp, "config"),
		DataHome:   filepath.Join(tmp, "data"),
		StateHome:  filepath.Join(tmp, "state"),
		CacheHome:  filepath.Join(tmp, "cache"),
	}
	for _, dir := range []string{paths.Home, paths.ConfigHome, paths.DataHome, paths.StateHome, paths.CacheHome} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", paths.Home)
	t.Setenv("XDG_CONFIG_HOME", paths.ConfigHome)
	t.Setenv("XDG_DATA_HOME", paths.DataHome)
	t.Setenv("XDG_STATE_HOME", paths.StateHome)
	t.Setenv("XDG_CACHE_HOME", paths.CacheHome)
	t.Setenv("MODEL_ROUTER_URL", "http://127.0.0.1:4000")
	t.Setenv("MODEL_ROUTER_API_KEY", "test-key")
	t.Setenv("MODEL_ROUTER_BIN", "")
	t.Setenv("ROUTER_BIN", "")
	t.Setenv("HARNESS_ROUTER_BIN", "")
	t.Setenv("MODEL_ROUTER_CONFIG", "")
	t.Setenv("ROUTER_CONFIG", "")
	t.Setenv("HARNESS_ROUTER_CONFIG", "")
	return paths
}

func TestHelpAndVersion(t *testing.T) {
	isolateEnv(t)

	code, _, stderr := captureBoth(t, func() int { return run(nil) })
	if code != 2 || !strings.Contains(stderr, "Usage: router") {
		t.Fatalf("bare run = (%d, %q), want exit 2 with usage", code, stderr)
	}

	for _, args := range [][]string{{"-h"}, {"--help"}, {"help"}} {
		code, stdout, _ := captureBoth(t, func() int { return run(args) })
		if code != 0 || !strings.Contains(stdout, "Usage: router") {
			t.Fatalf("run(%v) = (%d, %q), want exit 0 with usage", args, code, stdout)
		}
	}

	for _, topic := range []string{"start", "stop", "restart", "status", "models", "validate", "logs", "usage"} {
		code, stdout, _ := captureBoth(t, func() int { return run([]string{"help", topic}) })
		if code != 0 || !strings.Contains(stdout, "router "+topic) {
			t.Fatalf("help %s = (%d, %q)", topic, code, stdout)
		}
	}

	code, _, _ = captureBoth(t, func() int { return run([]string{"help", "bogus"}) })
	if code != 2 {
		t.Fatalf("help bogus = %d, want 2", code)
	}

	for _, args := range [][]string{{"version"}, {"--version"}, {"-v"}} {
		code, stdout, _ := captureBoth(t, func() int { return run(args) })
		if code != 0 || !strings.HasPrefix(stdout, "router ") {
			t.Fatalf("run(%v) = (%d, %q)", args, code, stdout)
		}
	}

	code, _, _ = captureBoth(t, func() int { return run([]string{"unknown-cmd"}) })
	if code != 2 {
		t.Fatalf("unknown command = %d, want 2", code)
	}
}

func TestStartStopRestartLogs(t *testing.T) {
	isolateEnv(t)

	origStart := routerStart
	origStop := routerStop
	origRestart := routerRestart
	origLogs := routerLogs
	origFg := routerExecForeground
	t.Cleanup(func() {
		routerStart = origStart
		routerStop = origStop
		routerRestart = origRestart
		routerLogs = origLogs
		routerExecForeground = origFg
	})

	var gotCfg string
	var gotWait time.Duration
	routerStart = func(_ context.Context, opts routerctl.Options, wait time.Duration) error {
		gotCfg = opts.RouterConfig
		gotWait = wait
		return nil
	}
	code, stdout, _ := captureBoth(t, func() int {
		return run([]string{"start", "--config", "/tmp/custom.toml", "--wait", "5s"})
	})
	if code != 0 || !strings.Contains(stdout, "router started at http://127.0.0.1:4000") {
		t.Fatalf("start = (%d, %q)", code, stdout)
	}
	if gotCfg != "/tmp/custom.toml" || gotWait != 5*time.Second {
		t.Fatalf("start opts: cfg=%q wait=%v", gotCfg, gotWait)
	}

	var gotArgv []string
	routerExecForeground = func(argv []string) int {
		gotArgv = append([]string(nil), argv...)
		return 0
	}
	t.Setenv("MODEL_ROUTER_BIN", "/usr/local/bin/routerd")
	code, _, stderr := captureBoth(t, func() int {
		return run([]string{"start", "--foreground", "--config", "/tmp/fg.toml"})
	})
	if code != 0 || !strings.Contains(stderr, "foreground start:") {
		t.Fatalf("start --foreground = (%d, %q)", code, stderr)
	}
	wantArgv := []string{"/usr/local/bin/routerd", "--config", "/tmp/fg.toml"}
	if !reflect.DeepEqual(gotArgv, wantArgv) {
		t.Fatalf("foreground argv = %v, want %v", gotArgv, wantArgv)
	}

	routerStart = func(_ context.Context, _ routerctl.Options, _ time.Duration) error {
		return errors.New("boom")
	}
	code, _, stderr = captureBoth(t, func() int { return run([]string{"start"}) })
	if code != 1 || !strings.Contains(stderr, "boom") {
		t.Fatalf("start error = (%d, %q)", code, stderr)
	}

	stopCalls := 0
	routerStop = func(_ context.Context, _ routerctl.Options, _ time.Duration) error {
		stopCalls++
		return nil
	}
	code, stdout, _ = captureBoth(t, func() int { return run([]string{"stop"}) })
	if code != 0 || stopCalls != 1 || !strings.Contains(stdout, "router stopped") {
		t.Fatalf("stop = (%d, %q, calls=%d)", code, stdout, stopCalls)
	}
	code, _, _ = captureBoth(t, func() int { return run([]string{"stop", "extra"}) })
	if code != 2 {
		t.Fatalf("stop extra = %d, want 2", code)
	}

	var restartWait time.Duration
	routerRestart = func(_ context.Context, _ routerctl.Options, wait time.Duration) error {
		restartWait = wait
		return nil
	}
	code, stdout, _ = captureBoth(t, func() int { return run([]string{"restart", "--wait", "12s"}) })
	if code != 0 || restartWait != 12*time.Second || !strings.Contains(stdout, "router restarted") {
		t.Fatalf("restart = (%d, %q, wait=%v)", code, stdout, restartWait)
	}

	var gotFollow bool
	var gotLines int
	routerLogs = func(_ context.Context, _ routerctl.Options, follow bool, lines int) error {
		gotFollow = follow
		gotLines = lines
		return nil
	}
	code, _, _ = captureBoth(t, func() int { return run([]string{"logs", "--follow", "-n", "25"}) })
	if code != 0 || !gotFollow || gotLines != 25 {
		t.Fatalf("logs = (code=%d, follow=%t, lines=%d)", code, gotFollow, gotLines)
	}
}

func TestStatusAndModelsLiveHTTP(t *testing.T) {
	isolateEnv(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			_, _ = w.Write([]byte(`{"name":"routerd","version":"0.2.0-test"}`))
		case "/livez":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/readyz":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ready"}`))
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-sonnet","owned_by":"anthropic","metadata":{"router":{}}},{"id":"gpt-5","owned_by":"openai"}]}`))
		case "/diagnostics":
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	t.Setenv("MODEL_ROUTER_URL", srv.URL)
	t.Setenv("MODEL_ROUTER_API_KEY", "secret-key")

	// Avoid hitting real systemctl in tests.
	origDetect := routerDetect
	t.Cleanup(func() { routerDetect = origDetect })
	routerDetect = func(paths routerenv.Paths, router routerenv.RouterSettings) routerctl.Options {
		opts := origDetect(paths, router)
		opts.Exec = func(_ context.Context, _ string, _ ...string) ([]byte, error) {
			return nil, errors.New("no systemd")
		}
		return opts
	}

	code, stdout, _ := captureBoth(t, func() int { return run([]string{"status"}) })
	if code != 0 {
		t.Fatalf("status = %d, want 0 (out=%q)", code, stdout)
	}
	for _, want := range []string{"live:     true", "ready:    true", "version:  0.2.0-test", "models:   2"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("status output missing %q:\n%s", want, stdout)
		}
	}

	code, stdout, _ = captureBoth(t, func() int { return run([]string{"status", "--json"}) })
	if code != 0 {
		t.Fatalf("status --json = %d", code)
	}
	var st routerctl.Status
	if err := json.Unmarshal([]byte(stdout), &st); err != nil {
		t.Fatalf("unmarshal status json: %v\n%s", err, stdout)
	}
	if !st.Livez || !st.Ready || st.Models != 2 {
		t.Fatalf("unexpected status json: %+v", st)
	}

	code, stdout, _ = captureBoth(t, func() int { return run([]string{"models"}) })
	if code != 0 || !strings.Contains(stdout, "claude-sonnet") || !strings.Contains(stdout, "gpt-5") {
		t.Fatalf("models = (%d, %q)", code, stdout)
	}

	code, stdout, _ = captureBoth(t, func() int { return run([]string{"models", "--json"}) })
	if code != 0 {
		t.Fatalf("models --json = %d", code)
	}
	var models []routerctl.ModelInfo
	if err := json.Unmarshal([]byte(stdout), &models); err != nil || len(models) != 2 {
		t.Fatalf("models --json parse err=%v models=%+v", err, models)
	}

	// Headless --pick falls back to rendering the rich table.
	origPick := routerPickModel
	t.Cleanup(func() { routerPickModel = origPick })
	routerPickModel = func(_ []modelpick.Model) (modelpick.Model, bool) {
		return modelpick.Model{}, false
	}
	code, stdout, _ = captureBoth(t, func() int { return run([]string{"models", "--pick"}) })
	if code != 0 || !strings.Contains(stdout, "claude-sonnet") {
		t.Fatalf("models --pick table = (%d, %q)", code, stdout)
	}

	routerPickModel = func(ms []modelpick.Model) (modelpick.Model, bool) {
		return ms[0], true
	}
	code, stdout, _ = captureBoth(t, func() int { return run([]string{"models", "--pick"}) })
	if code != 0 || strings.TrimSpace(stdout) != "claude-sonnet" {
		t.Fatalf("models --pick selected = (%d, %q)", code, stdout)
	}

	code, _, _ = captureBoth(t, func() int { return run([]string{"models", "--json", "--pick"}) })
	if code != 2 {
		t.Fatalf("models --json --pick = %d, want 2", code)
	}
}

func TestStatusNotLiveReturnsExit1(t *testing.T) {
	isolateEnv(t)
	origFetch := routerFetchStatus
	t.Cleanup(func() { routerFetchStatus = origFetch })
	routerFetchStatus = func(_ context.Context, opts routerctl.Options) (routerctl.Status, error) {
		return routerctl.Status{URL: opts.Router.URL, Managed: "none", Livez: false}, nil
	}
	code, stdout, _ := captureBoth(t, func() int { return run([]string{"status"}) })
	if code != 1 || !strings.Contains(stdout, "live:     false") {
		t.Fatalf("not-live status = (%d, %q), want exit 1", code, stdout)
	}
}

func TestValidateAndUsage(t *testing.T) {
	paths := isolateEnv(t)

	cfgPath := filepath.Join(paths.ConfigHome, ".router", "router.toml")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatal(err)
	}
	validCfg := `
[server]
host = "127.0.0.1"
port = 4000

[providers.test]
dialect = "openai-chat"
base_url = "https://api.example.com/v1"
api_key = "dummy-key"

[[models]]
name = "demo"
provider = "test"
model = "demo-upstream"
`
	if err := os.WriteFile(cfgPath, []byte(validCfg), 0o600); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := captureBoth(t, func() int { return run([]string{"validate"}) })
	if code != 0 || !strings.Contains(stdout, "configuration valid: "+cfgPath) {
		t.Fatalf("validate detected = (%d, out=%q, err=%q)", code, stdout, stderr)
	}

	badPath := filepath.Join(paths.ConfigHome, "bad.toml")
	if err := os.WriteFile(badPath, []byte("not = [valid toml"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, _ = captureBoth(t, func() int { return run([]string{"validate", "--config", badPath}) })
	if code != 2 {
		t.Fatalf("validate bad = %d, want 2", code)
	}

	// Usage against default XDG_STATE_HOME location.
	usagePath := filepath.Join(paths.StateHome, "router", "accounting.jsonl")
	if err := os.MkdirAll(filepath.Dir(usagePath), 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"ts":1795000000.0,"model":"glm-5.3","provider":"p","ingress":"openai-chat","upstream":"openai-chat","status":200,"input_tokens":120,"output_tokens":7,"cached_tokens":30,"duration_ms":42,"stream":false}` + "\n"
	if err := os.WriteFile(usagePath, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr = captureBoth(t, func() int { return run([]string{"usage", "--by", "model"}) })
	if code != 0 || !strings.Contains(stdout, "glm-5.3") {
		t.Fatalf("usage table = (%d, out=%q, err=%q)", code, stdout, stderr)
	}

	code, stdout, _ = captureBoth(t, func() int { return run([]string{"usage", "--by", "model", "--json"}) })
	if code != 0 || !strings.Contains(stdout, `"model": "glm-5.3"`) {
		t.Fatalf("usage json = (%d, %q)", code, stdout)
	}

	code, _, _ = captureBoth(t, func() int { return run([]string{"usage", "--by", "invalid"}) })
	if code != 2 {
		t.Fatalf("usage bad --by = %d, want 2", code)
	}
}
