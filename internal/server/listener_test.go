package server

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func testWrappedListener(t *testing.T, cap int) (net.Listener, string) {
	t.Helper()
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inner.Close() })
	wrapped := LimitListenerWithCapacity(inner, cap)
	return wrapped, wrapped.Addr().String()
}

// acceptLoop collects admitted server-side conns until done closes.
func acceptLoop(wrapped net.Listener) (chan net.Conn, chan error) {
	admitted := make(chan net.Conn, 64)
	errch := make(chan error, 1)
	go func() {
		for {
			c, err := wrapped.Accept()
			if err != nil {
				errch <- err
				close(admitted)
				return
			}
			admitted <- c
		}
	}()
	return admitted, errch
}

func waitAdmitted(t *testing.T, admitted chan net.Conn) net.Conn {
	t.Helper()
	select {
	case c, ok := <-admitted:
		if !ok {
			t.Fatalf("accept loop closed")
		}
		return c
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for admitted conn")
		return nil
	}
}

func dialRaw(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func readOverload(t *testing.T, c net.Conn) string {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	data, err := io.ReadAll(c)
	if err != nil {
		// Timeout or reset still must have delivered the 503 first if any.
		if len(data) == 0 {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				t.Fatalf("overload conn got no 503 within deadline: %v", err)
			}
		}
	}
	return string(data)
}

// TestAdmissionRejectsOverloadWithMinimal503 fills the raw slots with idle
// clients, then proves the next socket gets an immediate 503 before sending
// any valid headers.
func TestAdmissionRejectsOverloadWithMinimal503(t *testing.T) {
	wrapped, addr := testWrappedListener(t, 1)
	admitted, _ := acceptLoop(wrapped)

	idle := dialRaw(t, addr)
	_ = idle.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	srvConn := waitAdmitted(t, admitted)
	defer srvConn.Close()

	// Next raw socket must get 503 immediately, without sending headers.
	over := dialRaw(t, addr)
	got := readOverload(t, over)
	if !strings.HasPrefix(got, "HTTP/1.1 503") {
		t.Fatalf("expected immediate 503, got %q", got)
	}
	if !strings.Contains(got, "Retry-After: 1") {
		t.Fatalf("503 missing Retry-After: %q", got)
	}
	if !strings.Contains(got, "Content-Length: 0") {
		t.Fatalf("503 should be minimal empty body, got %q", got)
	}
}

// TestAdmissionTricklingHeadersHoldSlot proves partial/header-trickling
// clients still occupy capacity.
func TestAdmissionTricklingHeadersHoldSlot(t *testing.T) {
	wrapped, addr := testWrappedListener(t, 1)
	admitted, _ := acceptLoop(wrapped)

	trickle := dialRaw(t, addr)
	srvConn := waitAdmitted(t, admitted)
	defer srvConn.Close()

	// Send a partial request line slowly; slot must stay held.
	_, _ = trickle.Write([]byte("GET /livez HTTP/1."))
	time.Sleep(100 * time.Millisecond)
	_, _ = trickle.Write([]byte("1\r\nHost: x\r\n"))

	over := dialRaw(t, addr)
	got := readOverload(t, over)
	if !strings.HasPrefix(got, "HTTP/1.1 503") {
		t.Fatalf("trickling client should hold slot, overload got %q", got)
	}
}

// TestAdmissionReclaimsCapacityOnClose proves closing one admitted socket
// frees exactly one slot.
func TestAdmissionReclaimsCapacityOnClose(t *testing.T) {
	wrapped, addr := testWrappedListener(t, 1)
	admitted, _ := acceptLoop(wrapped)

	idle := dialRaw(t, addr)
	srvConn := waitAdmitted(t, admitted)

	over := dialRaw(t, addr)
	if got := readOverload(t, over); !strings.HasPrefix(got, "HTTP/1.1 503") {
		t.Fatalf("expected 503 while full, got %q", got)
	}

	// Reclaim: close server side (transport Close path) and client.
	_ = srvConn.Close()
	_ = idle.Close()
	time.Sleep(100 * time.Millisecond)

	next := dialRaw(t, addr)
	select {
	case c := <-admitted:
		defer c.Close()
		_ = next.Close()
	case <-time.After(3 * time.Second):
		t.Fatalf("capacity was not reclaimed after Close")
	}
}

// TestAdmissionKeepAliveHoldsSlot proves a keep-alive connection keeps its
// raw slot after a completed response, until transport close.
func TestAdmissionKeepAliveHoldsSlot(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	wrapped := LimitListenerWithCapacity(inner, 1)
	httpSrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "2")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ok"))
	})}
	go func() { _ = httpSrv.Serve(wrapped) }()
	defer func() { _ = httpSrv.Close() }()
	addr := wrapped.Addr().String()
	time.Sleep(100 * time.Millisecond)

	// First client: complete one request but keep socket open (keep-alive).
	keep, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer keep.Close()
	fmt.Fprintf(keep, "GET / HTTP/1.1\r\nHost: x\r\nConnection: keep-alive\r\n\r\n")
	br := bufio.NewReader(keep)
	_ = keep.SetReadDeadline(time.Now().Add(3 * time.Second))
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("keepalive request failed: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	// Socket stays open: slot must still be held.

	over, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer over.Close()
	_ = over.SetReadDeadline(time.Now().Add(3 * time.Second))
	data, _ := io.ReadAll(over)
	if !strings.HasPrefix(string(data), "HTTP/1.1 503") {
		t.Fatalf("keep-alive socket should hold slot, overload got %q", string(data))
	}
}

// TestAdmissionConcurrentCloseReleasesOnce proves double/concurrent Close
// releases exactly once: no hang, no extra capacity.
func TestAdmissionConcurrentCloseReleasesOnce(t *testing.T) {
	wrapped, addr := testWrappedListener(t, 1)
	admitted, _ := acceptLoop(wrapped)

	cli := dialRaw(t, addr)
	srvConn := waitAdmitted(t, admitted)
	_ = cli.Close()

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = srvConn.Close() }()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("concurrent Close hung (double-release would block)")
	}
	time.Sleep(100 * time.Millisecond)

	// Exactly one slot reclaimed: one new conn admitted, next still 503.
	next := dialRaw(t, addr)
	_ = next
	var reclaimed net.Conn
	select {
	case reclaimed = <-admitted:
	case <-time.After(3 * time.Second):
		t.Fatalf("slot not reclaimed after concurrent Close")
	}
	defer reclaimed.Close()

	over := dialRaw(t, addr)
	if got := readOverload(t, over); !strings.HasPrefix(got, "HTTP/1.1 503") {
		t.Fatalf("double-release leaked capacity, overload got %q", got)
	}
}

// TestAdmissionCloseUnblocksAccept proves shutdown unblocks Accept and
// closes owned resources.
func TestAdmissionCloseUnblocksAccept(t *testing.T) {
	wrapped, _ := testWrappedListener(t, 2)
	done := make(chan error, 1)
	go func() {
		_, err := wrapped.Accept()
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	_ = wrapped.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("expected Accept error after Close")
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("Accept did not unblock after Close")
	}
}

// TestAdmissionDoesNotDoubleWrap guards against wrapping twice and sharing
// tokens with the handler semaphore.
func TestAdmissionDoesNotDoubleWrap(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer inner.Close()
	first := LimitListenerWithCapacity(inner, 2)
	second := LimitListenerWithCapacity(first, 2)
	if second != first {
		t.Fatalf("double wrap should return the same listener")
	}
}
