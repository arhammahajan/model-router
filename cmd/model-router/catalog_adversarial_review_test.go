// Independent adversarial review: composition-root lifecycle -- listen
// failure handling and signal-during-startup cancellation.
package main

import (
	"fmt"
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

// TestReviewListenFailureStopsWorkerAndExitsOne pins that a bind failure
// after the synchronous startup attempt still exits 1 promptly with the
// worker stopped: exactly one catalog attempt (the startup inventory), no
// serve, no hang. In-process via run(), mirroring the existing CLI tests.
func TestReviewListenFailureStopsWorkerAndExitsOne(t *testing.T) {
	catalogTestEnv(t)
	var hits atomic.Int64
	catSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`[{"id": "cat-a"}]`))
	}))
	defer catSrv.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	cfg := writeCatalogCLICfg(t, catSrv.URL, "")
	start := time.Now()
	if code := run([]string{"--config", cfg, "--port", fmt.Sprint(port)}); code != 1 {
		t.Fatalf("listen failure must exit 1, got %d", code)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("listen failure took too long: %v", elapsed)
	}
	if hits.Load() != 1 {
		t.Fatalf("startup inventory must run exactly once before bind: %d hits", hits.Load())
	}
}

// TestReviewSignalDuringStartupCancelsFirstProbe pins "signal installed
// before startup I/O": with a catalog that never answers promptly, SIGTERM
// during the first probe must cancel it and exit 0 without ever listening.
// Subprocess only (real signals); skips mirror the existing startup test.
func TestReviewSignalDuringStartupCancelsFirstProbe(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess signal path needs a binary build")
	}
	if runtime.GOOS == "windows" {
		t.Skip("SIGTERM semantics are unix-only")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH; cannot build subprocess binary")
	}
	realHome, realXDG := os.Getenv("HOME"), os.Getenv("XDG_CONFIG_HOME")
	catalogTestEnv(t)
	release := make(chan struct{})
	var hits atomic.Int64
	catSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-release:
			_, _ = w.Write([]byte(`[{"id": "cat-a"}]`))
		case <-time.After(30 * time.Second):
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer catSrv.Close()
	defer close(release)

	binDir := t.TempDir()
	bin := filepath.Join(binDir, "model-router")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = "."
	build.Env = withHome(os.Environ(), realHome, realXDG)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	port := freePort(t)
	cfg := writeCatalogCLICfg(t, catSrv.URL, "")
	cmd := exec.Command(bin, "--config", cfg, "--port", fmt.Sprint(port))
	cmd.Env = append(os.Environ(),
		"MODEL_ROUTER_API_KEY=", "MODEL_ROUTER_API_KEYS=",
		"HOME="+t.TempDir(), "XDG_CONFIG_HOME="+t.TempDir())
	cmd.Stdout = nil
	var childErr strings.Builder
	cmd.Stderr = &childErr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
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
	// Wait until the startup probe is in flight, then interrupt it.
	deadline := time.Now().Add(10 * time.Second)
	for hits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if hits.Load() != 1 {
		t.Fatalf("startup probe never started: %d hits; child stderr:\n%s", hits.Load(), childErr.String())
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-done:
		finished.Store(true)
		if err != nil {
			t.Fatalf("signal-during-startup must exit 0, got %v; child stderr:\n%s", err, childErr.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("subprocess ignored SIGTERM during startup; child stderr:\n%s", childErr.String())
	}
	if hits.Load() != 1 {
		t.Fatalf("canceled startup must not retry: %d hits", hits.Load())
	}
	// The process must never have bound the port.
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatal("canceled startup must not listen")
	}
}
