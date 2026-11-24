package cliproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	sdkcliproxy "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// defaultStartupGrace bounds the wait for the embedded proxy's model endpoint.
const defaultStartupGrace = 30 * time.Second

// service is the minimal SDK Service surface this package supervises.
// *sdkcliproxy.Service satisfies it; fakes stand in for tests.
type service interface {
	Run(ctx context.Context) error
	Shutdown(ctx context.Context) error
}

// buildFunc constructs the supervised service for an embedded config path.
type buildFunc func(configPath string) (service, error)

// buildSDKService is the production builder: YAML loading via the public
// sdk/config API and Service construction via the public sdk/cliproxy
// Builder API. No upstream internal packages are referenced.
func buildSDKService(configPath string) (service, error) {
	cfg, err := sdkconfig.LoadConfig(configPath)
	if err != nil {
		return nil, fmt.Errorf("cliproxy: could not load embedded configuration: %w", err)
	}
	svc, err := sdkcliproxy.NewBuilder().
		WithConfig(cfg).
		WithConfigPath(configPath).
		Build()
	if err != nil {
		return nil, fmt.Errorf("cliproxy: could not build embedded service: %w", err)
	}
	return svc, nil
}

// Option configures a Manager.
type Option func(*Manager)

// WithBuilder overrides the service constructor (tests).
func WithBuilder(b buildFunc) Option {
	return func(m *Manager) { m.build = b }
}

// WithStartupGrace overrides the synchronous startup check bound (tests).
func WithStartupGrace(d time.Duration) Option {
	return func(m *Manager) {
		if d > 0 {
			m.grace = d
		}
	}
}

// Manager supervises at most one CLIProxyAPI lifecycle. The zero value is
// not usable; build with New. Start is single-shot; Shutdown is idempotent.
// Wait and Shutdown may both observe the Run result: it is recorded once
// and broadcast, never consumed by a single reader.
type Manager struct {
	settings Settings
	build    buildFunc
	grace    time.Duration
	ready    func(context.Context) error

	mu          sync.Mutex
	started     bool
	startFailed bool
	svc         service
	runCancel   context.CancelFunc
	finished    chan struct{} // closed when Run returns; broadcast to Wait/Shutdown
	runErr      error
	shutdown    chan struct{}
	shutdownErr error
}

// New returns a Manager for s. Validation runs at Start so misconfiguration
// surfaces with the rest of server startup, not at construction.
func New(s Settings, opts ...Option) *Manager {
	m := &Manager{settings: s, build: buildSDKService, grace: defaultStartupGrace}
	for _, o := range opts {
		o(m)
	}
	m.ready = m.probeReady
	return m
}

// Mode reports the lifecycle mode of the configured settings.
func (m *Manager) Mode() Mode { return m.settings.Mode() }

// Running reports whether an embedded service is currently supervised:
// started in Embedded mode, not failed, not shut down, and Run has not
// returned yet.
func (m *Manager) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.svc == nil || m.shutdown != nil || m.startFailed || m.finished == nil {
		return false
	}
	select {
	case <-m.finished:
		return false
	default:
		return true
	}
}

// Start validates the settings and, in Embedded mode only, builds the SDK
// Service and begins supervising it. Build failures return synchronously.
// Start waits for the authenticated model endpoint and reports early Run
// failures. Disabled and External modes validate only and return nil:
// no process is managed and a separately running proxy (if any) is left
// alone. The service stops when ctx is cancelled or Shutdown is called.
func (m *Manager) Start(ctx context.Context) error {
	if m == nil {
		return fmt.Errorf("cliproxy: manager is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.settings.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	if m.started || m.shutdown != nil {
		m.mu.Unlock()
		return errors.New("cliproxy: manager already started or shut down")
	}
	m.started = true
	m.mu.Unlock()
	if m.settings.Mode() != Embedded {
		return nil
	}
	svc, err := m.build(m.settings.ConfigPath)
	if err != nil {
		return err
	}
	if svc == nil {
		return fmt.Errorf("cliproxy: embedded builder returned no service")
	}
	runCtx, cancel := context.WithCancel(ctx)
	finished := make(chan struct{})
	m.mu.Lock()
	if m.shutdown != nil {
		m.mu.Unlock()
		cancel()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		_ = svc.Shutdown(cleanupCtx)
		return errors.New("cliproxy: manager shut down during startup")
	}
	m.svc = svc
	m.runCancel = cancel
	m.finished = finished
	m.mu.Unlock()
	go func() {
		err := svc.Run(runCtx)
		m.mu.Lock()
		m.runErr = err
		m.mu.Unlock()
		close(finished)
	}()
	startupCtx, startupCancel := context.WithTimeout(ctx, m.grace)
	defer startupCancel()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	var lastProbeErr error
	for {
		select {
		case <-finished:
			cancel()
			m.mu.Lock()
			m.startFailed = true
			err := m.runErr
			m.mu.Unlock()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err == nil {
				return errors.New("cliproxy: embedded service exited before startup completed")
			}
			return fmt.Errorf("cliproxy: embedded service failed to start: %w", err)
		case <-startupCtx.Done():
			cancel()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if lastProbeErr != nil {
				return fmt.Errorf("cliproxy: embedded service not ready at %s: %w (last probe: %v)", m.probeURL(), startupCtx.Err(), lastProbeErr)
			}
			return fmt.Errorf("cliproxy: embedded service not ready at %s: %w", m.probeURL(), startupCtx.Err())
		case <-tick.C:
			if err := m.ready(startupCtx); err == nil {
				select {
				case <-finished:
					continue
				default:
					return nil
				}
			} else if startupCtx.Err() == nil || lastProbeErr == nil {
				lastProbeErr = err
			}
		}
	}
}

// Wait blocks until the supervised service exits and returns its Run
// result (nil for Disabled/External modes, or context.Canceled after a
// cancellation-driven stop). It never blocks Shutdown.
func (m *Manager) Wait() error {
	m.mu.Lock()
	finished := m.finished
	m.mu.Unlock()
	if finished == nil {
		return nil
	}
	<-finished
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runErr
}

// Shutdown stops the supervised service. It is idempotent: concurrent and
// repeated calls share one shutdown sequence and return its result.
// Disabled/External managers return nil. context.Canceled from a
// cancellation-driven Run is absorbed so normal signal shutdown is quiet.
func (m *Manager) Shutdown(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	if m.shutdown != nil {
		done := m.shutdown
		m.mu.Unlock()
		<-done
		m.mu.Lock()
		err := m.shutdownErr
		m.mu.Unlock()
		return err
	}
	done := make(chan struct{})
	m.shutdown = done
	svc := m.svc
	cancel := m.runCancel
	finished := m.finished
	failed := m.startFailed
	m.mu.Unlock()

	defer close(done)
	var err error
	if cancel != nil {
		cancel()
	}
	// A failed Start owns its Run result already: Run has returned, so
	// there is nothing to stop or await and Shutdown stays quiet.
	if failed {
		m.mu.Lock()
		m.shutdownErr = nil
		m.mu.Unlock()
		return nil
	}
	if svc != nil {
		if shutErr := svc.Shutdown(ctx); shutErr != nil && !errors.Is(shutErr, context.Canceled) {
			err = shutErr
		}
	}
	if finished != nil {
		select {
		case <-finished:
			if runErr := m.getRunErr(); runErr != nil && !errors.Is(runErr, context.Canceled) {
				err = errors.Join(err, runErr)
			}
		case <-ctx.Done():
			if ctx.Err() != nil && !errors.Is(ctx.Err(), context.Canceled) {
				err = errors.Join(err, ctx.Err())
			}
		}
	}
	m.mu.Lock()
	m.shutdownErr = err
	m.mu.Unlock()
	return err
}

// getRunErr returns the recorded terminal Run error, if any.
func (m *Manager) getRunErr() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runErr
}

func (m *Manager) probeURL() string {
	return strings.TrimRight(m.settings.BaseURL, "/") + "/models"
}

// probeReady checks the same authenticated endpoint used by discovery.
func (m *Manager) probeReady(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.probeURL(), nil)
	if err != nil {
		return err
	}
	req.Close = true
	if m.settings.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+m.settings.APIKey)
	}
	client := &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected HTTP status %d", resp.StatusCode)
	}
	return nil
}
