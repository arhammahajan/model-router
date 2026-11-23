// Review probes for fetch hardening and lifecycle guards: whole-probe
// deadlines, encoding bounds, header coverage on every probe path,
// cancel-before-I/O, overlap coalescing and no-post-Stop publication.
package discovery

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/router"
)

func gzipBytes(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// The per-server whole-probe bound covers the complete health→models→props
// sequence: a health handler slower than the bound fails the server without
// hanging the sweep.
func TestLlamaReviewWholeProbeDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			time.Sleep(2 * time.Second)
			return
		}
		w.WriteHeader(500)
	}))
	defer srv.Close()
	cfg := llamaWorkerCfg(srv.URL, "llama-a")
	cfg.Llama.ProbeTimeout = 0.3
	reg := router.NewRegistry(cfg)
	w := NewLlamaWorker(cfg.Llama, reg)
	start := time.Now()
	w.RefreshOnce(context.Background())
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("probe exceeded its whole bound: %v", elapsed)
	}
	if w.Failures() == 0 {
		t.Fatal("slow health must fail the server")
	}
	if _, _, fault := reg.Current().Resolve("llama-a/x"); fault != router.NotFound {
		t.Fatal("timed-out probe must not activate the provider")
	}
}

// Stacked encodings are not identity/gzip: the models fetch fails.
func TestLlamaReviewStackedEncodingRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(200)
		case "/v1/models":
			w.Header().Set("Content-Encoding", "gzip, gzip")
			_, _ = w.Write([]byte(`{"data": [{"id": "m1"}]}`))
		default:
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	w, reg := refreshLlama(t, llamaWorkerCfg(srv.URL, "llama-a"))
	if w.Failures() == 0 {
		t.Fatal("stacked encoding must fail")
	}
	if _, _, fault := reg.Current().Resolve("m1"); fault == router.Found {
		t.Fatal("stacked encoding must publish nothing")
	}
}

// A gzip bomb (small on the wire, >4MiB decoded) fails; a valid small gzip
// body succeeds through the same path.
func TestLlamaReviewGzipBombDecodedBound(t *testing.T) {
	big := `{"data": [` + strings.Repeat(`{"id":"m"},`, 500000) + `{"id":"z"}]}`
	bomb := gzipBytes(t, []byte(big))
	if len(bomb) >= 4*1024*1024 {
		t.Fatalf("bomb fixture not compressed: %d", len(bomb))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(200)
		case "/v1/models":
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write(bomb)
		default:
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	w, _ := refreshLlama(t, llamaWorkerCfg(srv.URL, "llama-a"))
	if w.Failures() == 0 {
		t.Fatal("decoded-over-cap body must fail")
	}

	small := gzipBytes(t, []byte(`{"data": [{"id": "ok"}]}`))
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(200)
		case "/v1/models":
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write(small)
		default:
			w.WriteHeader(500)
		}
	}))
	defer srv2.Close()
	_, reg2 := refreshLlama(t, llamaWorkerCfg(srv2.URL, "llama-a"))
	if _, _, fault := reg2.Current().Resolve("ok"); fault != router.Found {
		t.Fatal("valid gzip body must publish")
	}
}

// Trailing JSON after the model list is a shape failure, not a silent
// truncation.
func TestLlamaReviewTrailingJSONRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(200)
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data": [{"id": "m1"}]} {"stray": true}`))
		default:
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	w, reg := refreshLlama(t, llamaWorkerCfg(srv.URL, "llama-a"))
	if w.Failures() == 0 {
		t.Fatal("trailing JSON must fail")
	}
	if _, _, fault := reg.Current().Resolve("m1"); fault == router.Found {
		t.Fatal("trailing JSON must publish nothing")
	}
}

// Trusted headers and User-Agent ride every probe path; auth names stay
// scrubbed and the selected key wins as a single Bearer everywhere.
func TestLlamaReviewHeadersOnAllPaths(t *testing.T) {
	type seen struct{ auth, global, srv, ua string }
	got := map[string]seen{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got[r.URL.Path] = seen{
			auth:   r.Header.Get("Authorization"),
			global: r.Header.Get("X-Llama-Global"),
			srv:    r.Header.Get("X-Llama-Srv"),
			ua:     r.Header.Get("User-Agent"),
		}
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(200)
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data": []}`))
		default:
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	cfg := llamaWorkerCfg(srv.URL, "llama-a")
	cfg.Llama.Headers = map[string]string{"X-Llama-Global": "g", "Authorization": "static-bogus"}
	cfg.Llama.Servers[0].Headers = map[string]string{"X-Llama-Srv": "s"}
	cfg.Llama.Servers[0].APIKey = "k"
	refreshLlama(t, cfg)
	for _, p := range []string{"/health", "/v1/models", "/props"} {
		s, ok := got[p]
		if !ok {
			t.Fatalf("probe path %s not hit", p)
		}
		if s.auth != "Bearer k" {
			t.Fatalf("%s auth=%q want single Bearer", p, s.auth)
		}
		if s.global != "g" || s.srv != "s" || s.ua == "" {
			t.Fatalf("%s headers lost: %+v", p, s)
		}
	}
}

// A context canceled before the first byte of I/O publishes nothing and
// records no failure state change.
func TestLlamaReviewCanceledBeforeFirstIO(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("canceled sweep must not dial")
		w.WriteHeader(500)
	}))
	defer srv.Close()
	cfg := llamaWorkerCfg(srv.URL, "llama-a")
	reg := router.NewRegistry(cfg)
	w := NewLlamaWorker(cfg.Llama, reg)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w.RefreshOnce(ctx)
	if w.Failures() != 0 {
		t.Fatalf("pre-canceled sweep must not count a failure: %d", w.Failures())
	}
	if _, _, fault := reg.Current().Resolve("llama-a/x"); fault != router.NotFound {
		t.Fatal("pre-canceled sweep must publish nothing")
	}
}

// Overlapping refreshes coalesce: while one sweep blocks in /health, a
// second RefreshOnce returns promptly without publishing.
func TestLlamaReviewOverlappingRefreshCoalesced(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			<-release
			return
		}
		w.WriteHeader(500)
	}))
	defer srv.Close()
	defer close(release)
	cfg := llamaWorkerCfg(srv.URL, "llama-a")
	cfg.Llama.ProbeTimeout = 10
	reg := router.NewRegistry(cfg)
	w := NewLlamaWorker(cfg.Llama, reg)
	done := make(chan struct{})
	go func() { defer close(done); w.RefreshOnce(context.Background()) }()
	time.Sleep(300 * time.Millisecond) // let the first sweep block in /health
	start := time.Now()
	w.RefreshOnce(context.Background()) // must TryLock-fail fast
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("overlapping refresh did not coalesce: %v", elapsed)
	}
	w.Stop()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("blocked sweep did not return after Stop")
	}
	if _, _, fault := reg.Current().Resolve("llama-a/x"); fault != router.NotFound {
		t.Fatal("stopped sweep must publish nothing")
	}
}

// Refresh after Stop is a no-op: no post-shutdown publication.
func TestLlamaReviewNoPostStopPublish(t *testing.T) {
	f := &llamaProbe{models: `{"data": [{"id": "m1"}]}`}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	cfg := llamaWorkerCfg(srv.URL, "llama-a")
	reg := router.NewRegistry(cfg)
	w := NewLlamaWorker(cfg.Llama, reg)
	w.Stop()
	w.RefreshOnce(context.Background())
	if _, _, fault := reg.Current().Resolve("m1"); fault == router.Found {
		t.Fatal("post-Stop refresh must publish nothing")
	}
	w.Stop() // idempotent
}

// Nine servers over the fixed pool of 8 need two waves: the fleet bound is
// 2× the per-server probe timeout plus scheduling/publication overhead.
func TestLlamaReviewFleetBoundNine(t *testing.T) {
	cfg := &config.LlamaConfig{ProbeTimeout: 2}
	for i := 0; i < 9; i++ {
		cfg.Servers = append(cfg.Servers, config.LlamaServer{URL: "http://127.0.0.1:1", Name: "s"})
	}
	// Two probe waves plus the root scheduling/publication allowance:
	// an equal parent/child deadline discarded successful peers on timeout.
	if got := LlamaFleetBound(cfg); got != 5*time.Second {
		t.Fatalf("9 servers need 2 waves: bound=%v", got)
	}
}
