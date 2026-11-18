// Catalog CLI: offline validation, bounded readiness attempts and the
// subprocess startup path (single refresh-0 attempt before listen,
// stop/serve lifecycle).
package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func catalogTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv("MODEL_ROUTER_API_KEY", "")
	t.Setenv("MODEL_ROUTER_API_KEYS", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func writeCatalogCLICfg(t *testing.T, catalogURL, extra string) string {
	t.Helper()
	body := `
[server]
auth_mode = "none"
allow_dynamic_routes = false
[retry]
max_attempts = 1
[providers.up]
dialect = "anthropic"
base_url = "http://127.0.0.1:9"
[catalog]
url = "` + catalogURL + `"
timeout = 5
refresh_seconds = 0
model_providers = { "cat-a" = "up" }
` + extra
	p := filepath.Join(t.TempDir(), "router.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestValidateConfigStaysOffline(t *testing.T) {
	catalogTestEnv(t)
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	if code := run([]string{"--config", writeCatalogCLICfg(t, srv.URL, ""), "--validate-config"}); code != 0 {
		t.Fatalf("validate must exit 0, got %d", code)
	}
	if hits.Load() != 0 {
		t.Fatalf("validate-config must not touch the catalog: %d hits", hits.Load())
	}
}

func TestCheckReadinessOneBoundedAttempt(t *testing.T) {
	catalogTestEnv(t)
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`[{"id": "cat-a"}]`))
	}))
	defer srv.Close()
	if code := run([]string{"--config", writeCatalogCLICfg(t, srv.URL, ""), "--check-readiness"}); code != 0 {
		t.Fatalf("catalog-ready must exit 0, got %d", code)
	}
	if hits.Load() != 1 {
		t.Fatalf("readiness must perform exactly one attempt: %d", hits.Load())
	}
}

func TestCheckReadinessUnmappedOnlyUnready(t *testing.T) {
	catalogTestEnv(t)
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`[{"id": "ghost"}]`))
	}))
	defer srv.Close()
	if code := run([]string{"--config", writeCatalogCLICfg(t, srv.URL, ""), "--check-readiness"}); code != 3 {
		t.Fatalf("unmapped-only must exit 3, got %d", code)
	}
	if hits.Load() != 1 {
		t.Fatalf("readiness must perform exactly one attempt: %d", hits.Load())
	}
}

// TestServeStartupSubprocess builds the binary and exercises the serve
// path end to end: one startup refresh before listen, catalog-backed
// readiness/models, then signal shutdown.
func TestServeStartupSubprocess(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess startup needs a binary build")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH; cannot build subprocess binary")
	}
	// Capture the real HOME before the hermetic override below: the
	// build must use the shared module/build cache, never the temp HOME
	// (its read-only cache files would break TempDir cleanup).
	realHome, realXDG := os.Getenv("HOME"), os.Getenv("XDG_CONFIG_HOME")
	catalogTestEnv(t)
	var hits atomic.Int64
	catSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`[{"id": "cat-a"}]`))
	}))
	defer catSrv.Close()

	binDir := t.TempDir()
	bin := filepath.Join(binDir, "model-router")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = "."
	build.Env = withHome(os.Environ(), realHome, realXDG)
	t.Logf("building %s", bin)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	t.Logf("build done")

	port := freePort(t)
	cfg := writeCatalogCLICfg(t, catSrv.URL, "")
	// Point the server at the free port via --port.
	var childErr bytes.Buffer
	cmd := exec.Command(bin, "--config", cfg, "--port", fmt.Sprint(port))
	cmd.Env = append(os.Environ(),
		"MODEL_ROUTER_API_KEY=", "MODEL_ROUTER_API_KEYS=",
		"HOME="+t.TempDir(), "XDG_CONFIG_HOME="+t.TempDir())
	cmd.Stdout = io.Discard
	cmd.Stderr = &childErr
	t.Logf("starting %s on port %d", bin, port)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Logf("child pid %d", cmd.Process.Pid)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var finished atomic.Bool
	t.Cleanup(func() {
		if finished.Load() {
			return
		}
		_ = cmd.Process.Kill()
		<-done
	})
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := http.Get(base + "/livez")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		} else {
			t.Logf("livez poll: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("subprocess never became live; child stderr:\n%s", childErr.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("child live")
	if hits.Load() != 1 {
		t.Fatalf("startup must be exactly one attempt (refresh 0): %d", hits.Load())
	}
	// Readiness and listing see catalog routes with no further I/O.
	if resp, err := http.Get(base + "/readyz"); err != nil || resp.StatusCode != 200 {
		if err == nil {
			resp.Body.Close()
		}
		t.Fatalf("readyz: %v", err)
	} else {
		resp.Body.Close()
	}
	resp, err := http.Get(base + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(data), "cat-a") {
		t.Fatalf("models must list the catalog route: %s", data)
	}
	if hits.Load() != 1 {
		t.Fatalf("read paths must not dial out: %d hits", hits.Load())
	}
	// Signal shutdown: SIGTERM must exit 0 promptly.
	if runtime.GOOS == "windows" {
		_ = cmd.Process.Kill()
	} else {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
	select {
	case err := <-done:
		finished.Store(true)
		if err != nil {
			if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 0 {
				t.Fatalf("shutdown exit: %v; child stderr:\n%s", err, childErr.String())
			}
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("subprocess did not exit after SIGTERM; child stderr:\n%s", childErr.String())
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// withHome returns env with HOME/XDG_CONFIG_HOME pointed at the real
// locations (for toolchain cache use), overriding hermetic test values.
func withHome(env []string, home, xdg string) []string {
	out := make([]string, 0, len(env)+2)
	for _, kv := range env {
		if strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "XDG_CONFIG_HOME=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "HOME="+home, "XDG_CONFIG_HOME="+xdg)
}
