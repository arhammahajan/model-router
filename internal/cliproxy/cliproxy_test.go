package cliproxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeService stands in for the SDK Service without any I/O.
type fakeService struct {
	run       func(ctx context.Context) error
	shutdown  func(ctx context.Context) error
	shutCalls int
	mu        sync.Mutex
}

func (f *fakeService) Run(ctx context.Context) error {
	if f.run != nil {
		return f.run(ctx)
	}
	<-ctx.Done()
	return ctx.Err()
}

func (f *fakeService) Shutdown(ctx context.Context) error {
	f.mu.Lock()
	f.shutCalls++
	f.mu.Unlock()
	if f.shutdown != nil {
		return f.shutdown(ctx)
	}
	return nil
}

func (f *fakeService) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.shutCalls
}

func TestSettingsMode(t *testing.T) {
	cases := []struct {
		name string
		in   Settings
		want Mode
	}{
		{"empty disabled", Settings{}, Disabled},
		{"base_url external", FromFields("http://127.0.0.1:8317/v1", "", ""), External},
		{"base_url plus key external", FromFields("http://127.0.0.1:8317/v1", "secret", ""), External},
		{"config_path embedded", FromFields("", "", "cliproxy.yaml"), Embedded},
		{"both embedded wins", FromFields("http://127.0.0.1:8317/v1", "secret", "cliproxy.yaml"), Embedded},
	}
	for _, c := range cases {
		if got := c.in.Mode(); got != c.want {
			t.Errorf("%s: Mode() = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSettingsValidate(t *testing.T) {
	cases := []struct {
		name    string
		in      Settings
		wantErr bool
	}{
		{"empty ok", Settings{}, false},
		{"key alone rejected", FromFields("", "k", ""), true},
		{"http ok", FromFields("http://127.0.0.1:8317/v1", "", ""), false},
		{"https ok", FromFields("https://proxy.example.com/", "k", ""), false},
		{"missing scheme rejected", FromFields("127.0.0.1:8317", "", ""), true},
		{"userinfo rejected", FromFields("http://user@127.0.0.1:8317/", "", ""), true},
		{"query rejected", FromFields("http://127.0.0.1:8317/?x=1", "", ""), true},
		{"embedded path ok", FromFields("", "", "cliproxy.yaml"), false},
		{"embedded with base ok", FromFields("http://127.0.0.1:8317/v1", "k", "cliproxy.yaml"), false},
		{"embedded bad base rejected", FromFields("ftp://x/", "", "cliproxy.yaml"), true},
	}
	for _, c := range cases {
		err := c.in.Validate()
		if (err != nil) != c.wantErr {
			t.Errorf("%s: Validate() err = %v, wantErr %v", c.name, err, c.wantErr)
		}
	}
}

func TestManagerDisabledNoop(t *testing.T) {
	m := newTestManager(Settings{})
	if m.Mode() != Disabled {
		t.Fatalf("Mode() = %v, want disabled", m.Mode())
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	if m.Running() {
		t.Fatal("Running() = true, want false")
	}
	if err := m.Wait(); err != nil {
		t.Fatalf("Wait() = %v", err)
	}
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
}

func TestManagerExternalNoop(t *testing.T) {
	built := 0
	m := newTestManager(
		FromFields("http://127.0.0.1:8317/v1", "k", ""),
		WithBuilder(func(string) (service, error) { built++; return &fakeService{}, nil }),
	)
	if m.Mode() != External {
		t.Fatalf("Mode() = %v, want external", m.Mode())
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	if built != 0 {
		t.Fatalf("builder called %d times in external mode, want 0", built)
	}
	if m.Running() {
		t.Fatal("Running() = true, want false")
	}
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
}

func TestManagerEmbeddedBuildError(t *testing.T) {
	sentinel := errors.New("no such config")
	m := newTestManager(
		FromFields("", "", "missing.yaml"),
		WithBuilder(func(string) (service, error) { return nil, sentinel }),
		WithStartupGrace(200*time.Millisecond),
	)
	if err := m.Start(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("Start() = %v, want %v", err, sentinel)
	}
	// Shutdown stays safe after a failed Start.
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
}

func TestManagerEmbeddedRunCancel(t *testing.T) {
	fake := &fakeService{}
	m := newTestManager(
		FromFields("", "", "cliproxy.yaml"),
		WithBuilder(func(string) (service, error) { return fake, nil }),
		WithStartupGrace(200*time.Millisecond),
	)
	ctx, cancel := context.WithCancel(context.Background())
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	if !m.Running() {
		t.Fatal("Running() = false, want true")
	}
	cancel()
	if err := m.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait() = %v, want context.Canceled", err)
	}
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() = %v, want nil (canceled Run is absorbed)", err)
	}
	if got := fake.calls(); got != 1 {
		t.Fatalf("Shutdown calls = %d, want 1", got)
	}
}

func TestManagerEmbeddedAsyncRunError(t *testing.T) {
	sentinel := errors.New("listen bind failure")
	fake := &fakeService{run: func(context.Context) error { return sentinel }}
	m := newTestManager(
		FromFields("", "", "cliproxy.yaml"),
		WithBuilder(func(string) (service, error) { return fake, nil }),
		WithStartupGrace(200*time.Millisecond),
	)
	// The startup grace turns the immediate Run failure into a Start error,
	// even though upstream invokes OnAfterStart before selecting serverErr.
	if err := m.Start(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("Start() = %v, want %v", err, sentinel)
	}
	if m.Running() {
		t.Fatal("Running() = true after failed Start, want false")
	}
	// A failed Start owns its Run result: Shutdown stays quiet.
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() after failed Start = %v, want nil", err)
	}
}

func TestManagerShutdownIdempotent(t *testing.T) {
	fake := &fakeService{}
	m := newTestManager(
		FromFields("", "", "cliproxy.yaml"),
		WithBuilder(func(string) (service, error) { return fake, nil }),
		WithStartupGrace(200*time.Millisecond),
	)
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Shutdown(ctx); err != nil {
		t.Fatalf("first Shutdown() = %v", err)
	}
	if err := m.Shutdown(ctx); err != nil {
		t.Fatalf("second Shutdown() = %v", err)
	}
	if got := fake.calls(); got != 1 {
		t.Fatalf("Shutdown calls = %d, want exactly 1 shared sequence", got)
	}
}

func TestManagerDoubleStartRejected(t *testing.T) {
	m := newTestManager(Settings{})
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("first Start() = %v", err)
	}
	if err := m.Start(context.Background()); err == nil {
		t.Fatal("second Start() = nil, want already-started error")
	}
	_ = m.Shutdown(context.Background())
}

func newTestManager(s Settings, opts ...Option) *Manager {
	m := New(s, opts...)
	m.ready = func(context.Context) bool { return true }
	return m
}

func TestStartWaitsForAuthenticatedModelEndpoint(t *testing.T) {
	var requests atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer proxy-key" {
			t.Error("readiness probe used wrong endpoint or credentials")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if requests.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer proxy.Close()
	m := New(FromFields(proxy.URL+"/v1", "proxy-key", "config.yaml"),
		WithBuilder(func(string) (service, error) { return &fakeService{}, nil }),
		WithStartupGrace(time.Second))
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if requests.Load() < 3 {
		t.Fatal("Start returned before the proxy was ready")
	}
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestStartCancelledBeforeReadiness(t *testing.T) {
	m := newTestManager(FromFields("", "", "config.yaml"),
		WithBuilder(func(string) (service, error) { return &fakeService{}, nil }))
	m.ready = func(context.Context) bool { return false }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Start(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Start = %v", err)
	}
	_ = m.Shutdown(context.Background())
}

func TestStartReadinessTimeoutStopsRun(t *testing.T) {
	m := newTestManager(FromFields("", "", "config.yaml"),
		WithBuilder(func(string) (service, error) { return &fakeService{}, nil }),
		WithStartupGrace(30*time.Millisecond))
	m.ready = func(context.Context) bool { return false }
	if err := m.Start(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Start = %v", err)
	}
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestShutdownDuringBuildStopsService(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	fake := &fakeService{}
	m := newTestManager(FromFields("", "", "config.yaml"),
		WithBuilder(func(string) (service, error) {
			close(entered)
			<-release
			return fake, nil
		}))
	result := make(chan error, 1)
	go func() { result <- m.Start(context.Background()) }()
	<-entered
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-result; err == nil {
		t.Fatal("Start succeeded after Shutdown")
	}
	if fake.calls() != 1 {
		t.Fatal("built service was not shut down")
	}
}
