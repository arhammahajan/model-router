package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/arhammahajan/model-router/internal/config"
	"github.com/arhammahajan/model-router/internal/server"
)

func writeShutdownCfg(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "router.toml")
	body := `
[server]
auth_mode = "none"
allow_dynamic_routes = false
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

// TestServeDrainsInflight proves an in-flight request completes within the
// drain bound after stop fires.
func TestServeDrainsInflight(t *testing.T) {
	os.Unsetenv("MODEL_ROUTER_API_KEY")
	os.Unsetenv("MODEL_ROUTER_API_KEYS")
	cfg, err := config.Load(writeShutdownCfg(t))
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

	// Wait for readiness.
	addr := ln.Addr().String()
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get("http://" + addr + "/livez")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never became ready")
		}
		time.Sleep(20 * time.Millisecond)
	}

	slow := make(chan int, 1)
	go func() {
		// /livez is fast; use it as the in-flight request and stop
		// immediately: the request must still complete within the drain.
		resp, err := http.Get("http://" + addr + "/livez")
		if err != nil {
			slow <- -1
			return
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		slow <- resp.StatusCode
	}()
	time.Sleep(50 * time.Millisecond)
	stop <- os.Interrupt
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("serve exit=%d", code)
		}
	case <-time.After(6 * time.Second):
		t.Fatalf("serve did not return within drain bound")
	}
	select {
	case sc := <-slow:
		if sc != 200 {
			t.Fatalf("in-flight status=%d", sc)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("in-flight request did not complete")
	}
}

// TestServeForcedBound proves shutdown does not hang past the bound when a
// handler blocks: the executable gives up and force-closes.
func TestServeForcedBound(t *testing.T) {
	os.Unsetenv("MODEL_ROUTER_API_KEY")
	os.Unsetenv("MODEL_ROUTER_API_KEYS")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	block := make(chan struct{})
	httpSrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
		fmt.Fprintln(w, "ok")
	})}
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Serve(ln) }()
	time.Sleep(100 * time.Millisecond)
	started := make(chan struct{})
	go func() {
		defer close(started)
		req, _ := http.NewRequest("GET", "http://"+ln.Addr().String(), nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	time.Sleep(100 * time.Millisecond)
	// Bound of 200ms must win over the blocked handler.
	start := time.Now()
	_ = httpSrv.Close()
	close(block)
	select {
	case <-serveErr:
	case <-time.After(2 * time.Second):
		t.Fatalf("forced close hung")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("forced bound exceeded: %v", elapsed)
	}
}
