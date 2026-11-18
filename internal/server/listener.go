package server

import (
	"net"
	"sync"
	"time"
)

// admissionRejectWriteTimeout bounds the overload 503 write so a slow or
// non-reading peer cannot stall the accept loop. Python never stalls the
// accept loop: it sends the 503 with a non-blocking socket and closes.
// Go has no non-blocking Conn write, so a tiny write deadline is the
// explicit bounded equivalent.
const admissionRejectWriteTimeout = 500 * time.Millisecond

// admissionRejectResponse is the minimal overload reply. It mirrors the
// Python accept-loop rejection: 503, close, no body, Retry-After 1.
var admissionRejectResponse = []byte("HTTP/1.1 503 Service Unavailable\r\n" +
	"Connection: close\r\nContent-Length: 0\r\nRetry-After: 1\r\n\r\n")

// LimitListener wraps ln with raw-socket admission bound by the configured
// server.max_connections. Each accepted TCP socket holds one slot until its
// Close, including idle keep-alive and partial-header connections. Overload
// never queues: the extra socket gets a bounded minimal 503 and is closed
// promptly in the accept loop, without spawning a goroutine per rejection.
//
// The bound is distinct from the handler-level s.conns/s.gens semaphores,
// which run after headers are read. This listener counts raw sockets only
// (HTTP/1 scope); it does not cap per-stream multiplexing.
func (s *Server) LimitListener(ln net.Listener) net.Listener {
	return LimitListenerWithCapacity(ln, s.cfg.Server.MaxConnections)
}

// LimitListenerWithCapacity is the exported admission constructor. It keeps
// the core bound testable without Server fields while ownership stays on the
// server boundary via LimitListener.
func LimitListenerWithCapacity(ln net.Listener, maxConns int) net.Listener {
	if al, ok := ln.(*admissionListener); ok && al != nil {
		return ln
	}
	if maxConns <= 0 {
		maxConns = 1
	}
	return &admissionListener{inner: ln, sem: make(chan struct{}, maxConns)}
}

type admissionListener struct {
	inner net.Listener
	sem   chan struct{}
}

func (l *admissionListener) Accept() (net.Conn, error) {
	for {
		c, err := l.inner.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.sem <- struct{}{}:
			return &countedConn{Conn: c, release: func() { <-l.sem }}, nil
		default:
			rejectAdmission(c)
		}
	}
}

func (l *admissionListener) Close() error {
	return l.inner.Close()
}

func (l *admissionListener) Addr() net.Addr {
	return l.inner.Addr()
}

// rejectAdmission sends the minimal 503 with a tiny bounded write deadline
// then closes. Best-effort: write errors are ignored. Synchronous in the
// accept loop: no goroutine per rejection, stall bounded by the deadline.
func rejectAdmission(c net.Conn) {
	_ = c.SetWriteDeadline(time.Now().Add(admissionRejectWriteTimeout))
	_, _ = c.Write(admissionRejectResponse)
	_ = c.Close()
}

// countedConn releases its admission slot exactly once, on first Close.
// Keep-alive reuse holds the slot across requests; only transport Close
// reclaims capacity.
type countedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *countedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
