package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/server"
)

func writeAdmissionCfg(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "router.toml")
	body := `
[server]
auth_mode = "none"
allow_dynamic_routes = false
max_connections = 2
max_generations = 2
[retry]
max_attempts = 1
[providers.anth]
dialect = "anthropic"
base_url = "http://127.0.0.1:9"
[[models]]
name = "m"
provider = "anth"
model = "x"
`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func admissionProbe(t *testing.T, addr string) (net.Conn, int) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = fmt.Fprint(c, "GET /livez HTTP/1.1\r\nHost: x\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return c, resp.StatusCode
}

// TestServeUsesAdmissionListener proves the executable path wraps the raw
// listener: with both raw slots held idle, the next socket gets an immediate
// minimal 503 before sending valid headers; closing one slot reclaims it.
func TestServeUsesAdmissionListener(t *testing.T) {
	os.Unsetenv("MODEL_ROUTER_API_KEY")
	os.Unsetenv("MODEL_ROUTER_API_KEYS")
	cfg, err := config.Load(writeAdmissionCfg(t))
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(cfg)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan os.Signal, 1)
	done := make(chan int, 1)
	go func() { done <- serve(ln, srv, nil, nil, nil, 3*time.Second, stop) }()
	defer func() {
		stop <- os.Interrupt
		select {
		case <-done:
		case <-time.After(6 * time.Second):
		}
	}()
	addr := ln.Addr().String()
	// Confirm that both connections hold slots while idle in keep-alive.
	idle1, status := admissionProbe(t, addr)
	defer idle1.Close()
	if status != http.StatusOK {
		t.Fatalf("first slot returned %d", status)
	}
	idle2, status := admissionProbe(t, addr)
	defer idle2.Close()
	if status != http.StatusOK {
		t.Fatalf("second slot returned %d", status)
	}

	// Next socket must get an immediate minimal 503 without valid headers.
	over, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer over.Close()
	_ = over.SetReadDeadline(time.Now().Add(3 * time.Second))
	data, _ := io.ReadAll(over)
	got := string(data)
	if !strings.HasPrefix(got, "HTTP/1.1 503") {
		t.Fatalf("expected serve-level 503 while full, got %q", got)
	}
	if !strings.Contains(got, "Retry-After: 1") || !strings.Contains(got, "Content-Length: 0") {
		t.Fatalf("serve 503 should be minimal with Retry-After, got %q", got)
	}

	// Wait for the server to observe the closed socket and reclaim its slot.
	_ = idle1.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		next, status := admissionProbe(t, addr)
		_ = next.Close()
		if status == http.StatusOK {
			return
		}
		if status != http.StatusServiceUnavailable || time.Now().After(deadline) {
			t.Fatalf("expected reclaimed slot to serve 200, got %d", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
