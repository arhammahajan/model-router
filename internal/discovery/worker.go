// Catalog refresh worker: single-attempt TryLock, startup inventory,
// scheduled success/failure backoff, stale retention and idempotent
// shutdown. The worker owns the only catalog publication path: each
// attempt computes one typed CatalogAttempt and commits it once, then the
// loop waits exactly the published deadline. Jitter is drawn once at
// completion; no independent next-delay re-jitter exists.
package discovery

import (
	"context"
	"log"
	"math"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/arhammahajan/model-router/internal/config"
	"github.com/arhammahajan/model-router/internal/router"
)

// Backoff bounds (adopted from the compat doc, not the [retry]
// generation policy): base 5s, max 300s.
const (
	backoffBase = 5.0
	backoffMax  = 300.0
)

// BackoffCeiling is the jitter ceiling for consecutive failure count n
// (n>=1): min(300, 5*2^(n-1)). The scheduled delay draws uniform from
// [ceiling/2, ceiling].
func BackoffCeiling(n int) float64 {
	if n < 1 {
		n = 1
	}
	shift := n - 1
	if shift > 16 {
		shift = 16
	}
	return math.Min(backoffMax, backoffBase*math.Pow(2, float64(shift)))
}

// Worker refreshes one Registry from one [catalog] table. Static
// providers are read-only and never mutated; per-request network
// discovery never happens (read paths serve the cached snapshot).
// Status lives in the registry snapshot; Failures/LastError are thin
// selectors over it, never duplicate mutable counters.
type Worker struct {
	cat       *config.Catalog
	providers map[string]*config.Provider
	reg       *router.Registry
	client    *http.Client
	clock     router.Clock
	jitter    func(ceil float64) float64
	sanitize  func(string) string

	attemptMu sync.Mutex // TryLock: overlapping refreshes return immediately
	stateMu   sync.Mutex
	cache     *CacheStore
	wake      chan struct{} // depth 1; attempt completion wakes the scheduler

	stopCh   chan struct{}
	stopOnce sync.Once
	loopDone chan struct{}
	started  bool // one periodic-loop lifetime; canceled loops cannot restart
}

// NewCatalogWorker builds the worker. It performs no I/O; the
// composition root owns the first bounded RefreshOnce.
func NewCatalogWorker(cat *config.Catalog, providers map[string]*config.Provider, reg *router.Registry) *Worker {
	return &Worker{
		cat:       cat,
		providers: providers,
		reg:       reg,
		client:    fetchClient(time.Duration(cat.Timeout * float64(time.Second))),
		clock:     reg.Clock(),
		jitter:    defaultJitter,
		sanitize:  reg.Sanitize,
		stopCh:    make(chan struct{}),
		loopDone:  make(chan struct{}),
		wake:      make(chan struct{}, 1),
	}
}

func defaultJitter(ceil float64) float64 {
	return ceil/2 + rand.Float64()*(ceil/2)
}

// Failures reports consecutive catalog failures from the pinned snapshot.
func (w *Worker) Failures() int {
	return w.reg.CatalogEvent().Failures
}

// LastError returns the static failure reason, or "" when healthy.
func (w *Worker) LastError() string {
	return w.reg.CatalogEvent().Err
}

// RefreshOnce performs a single attempt. Overlapping calls return
// immediately, even forced ones (matching Python). On success it commits
// entries, unmapped IDs, wall time and a cleared status in one
// publication; on failure it retains routes, wall times and unmapped IDs
// while updating status. A valid empty catalog replaces: emptiness is
// success. Every owner completion wakes the scheduler after unlocking;
// cancellation publishes nothing. False means another attempt owns the lock,
// not a failed probe.
func (w *Worker) RefreshOnce(ctx context.Context) bool {
	if !w.attemptMu.TryLock() {
		return false
	}
	defer func() {
		w.attemptMu.Unlock()
		w.signalWake()
	}()
	select {
	case <-w.stopCh:
		return true
	default:
	}
	if ctx.Err() != nil {
		return true
	}

	// Stop cancels in-flight probes: link the worker lifetime into the
	// attempt without taking ownership of the caller's context.
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopReq := make(chan struct{})
	defer close(stopReq)
	go func() {
		select {
		case <-w.stopCh:
			cancel()
		case <-stopReq:
		}
	}()

	start := w.clock.Sample()
	entries, unmapped, err := FetchMapped(reqCtx, w.client, w.cat, w.providers)
	end := w.clock.Sample()

	select {
	case <-w.stopCh:
		return true
	default:
	}
	if reqCtx.Err() != nil {
		return true // explicit cancellation: no partial publication
	}

	prior := w.reg.CatalogEvent()
	interval := w.cat.RefreshSeconds
	var attempt router.CatalogAttempt
	if err != nil {
		failures := prior.Failures + 1
		next := nextAfter(end.Mono, interval, w.jitter(BackoffCeiling(failures)))
		attempt = router.CatalogAttempt{
			Event: router.SourceEvent{
				LastSuccess: prior.LastSuccess,
				LastAttempt: monoPtr(start.Mono),
				NextAttempt: next,
				Failures:    failures,
				Err:         w.sanitize(err.Error()),
				Cached:      prior.Cached,
			},
		}
	} else {
		next := nextAfter(end.Mono, interval, interval)
		attempt = router.CatalogAttempt{
			Event: router.SourceEvent{
				LastSuccess: monoPtr(end.Mono),
				LastAttempt: monoPtr(start.Mono),
				NextAttempt: next,
			},
			Success: &router.CatalogSuccess{
				Entries:  copyOutEntries(entries),
				Unmapped: append([]string(nil), unmapped...),
				Wall:     end.Wall,
			},
		}
	}
	if perr := w.reg.CommitCatalog(attempt); perr != nil {
		failures := prior.Failures + 1
		next := nextAfter(end.Mono, interval, w.jitter(BackoffCeiling(failures)))
		fallback := router.CatalogAttempt{
			Event: router.SourceEvent{
				LastSuccess: prior.LastSuccess,
				LastAttempt: monoPtr(start.Mono),
				NextAttempt: next,
				Failures:    failures,
				Err:         w.sanitize(perr.Error()),
				Cached:      prior.Cached,
			},
		}
		_ = w.reg.CommitCatalog(fallback)
		log.Printf("catalog refresh failed: %v (models=%d unmapped=%d failures=%d)",
			perr, len(entries), len(unmapped), failures)
		return true
	}
	if err != nil {
		log.Printf("catalog refresh failed: %v (models=%d unmapped=%d failures=%d)",
			err, len(entries), len(unmapped), attempt.Event.Failures)
		return true
	}
	// Notify outside registry locks: NotifyDirty only does a non-blocking
	// coalesced channel send and performs no I/O itself.
	w.stateMu.Lock()
	cache := w.cache
	w.stateMu.Unlock()
	if cache != nil {
		cache.NotifyDirty()
	}
	log.Printf("catalog refresh succeeded (models=%d unmapped=%d)", len(entries), len(unmapped))
	return true
}

func copyOutEntries(entries []router.CatalogEntry) []router.CatalogEntry {
	out := make([]router.CatalogEntry, 0, len(entries))
	for _, e := range entries {
		cp := e
		if e.Metadata != nil {
			cp.Metadata = append([]byte(nil), e.Metadata...)
		}
		out = append(out, cp)
	}
	return out
}

// nextAfter returns completed+delay iff interval>0, else nil: a zero
// refresh interval never schedules by age.
func nextAfter(completed router.Mono, interval, delay float64) *router.Mono {
	if interval <= 0 {
		return nil
	}
	n := completed + router.Mono(delay)
	return &n
}

func monoPtr(m router.Mono) *router.Mono {
	v := m
	return &v
}

// signalWake wakes the scheduler after a completed attempt. The loop
// re-reads the published deadline, so an overdue manual completion runs
// at most one extra attempt instead of spinning.
func (w *Worker) signalWake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// CacheAttach sets the persistence owner notified after accepted
// publications. A nil store disables notifications.
func (w *Worker) CacheAttach(s *CacheStore) {
	w.stateMu.Lock()
	defer w.stateMu.Unlock()
	w.cache = s
}

// Start begins periodic refresh after the startup attempt. It is a no-op
// when RefreshSeconds==0 and idempotent otherwise: a second call while
// started returns without spawning, even after the first loop is canceled.
func (w *Worker) Start(ctx context.Context) {
	w.stateMu.Lock()
	if w.cat.RefreshSeconds == 0 || w.started {
		w.stateMu.Unlock()
		return
	}
	select {
	case <-w.stopCh:
		w.stateMu.Unlock()
		return
	default:
	}
	w.started = true
	w.stateMu.Unlock()
	go w.loop(ctx)
}

func (w *Worker) loop(ctx context.Context) {
	defer close(w.loopDone)
	for {
		delay := w.waitDelay()
		timer := time.NewTimer(delay)
		select {
		case <-w.stopCh:
			timer.Stop()
			return
		case <-ctx.Done():
			timer.Stop()
			return
		case <-w.wake:
			timer.Stop()
			continue // re-read the published deadline after completion
		case <-timer.C:
		}
		if !w.RefreshOnce(ctx) {
			// A manual owner will wake us after unlocking, even if canceled.
			select {
			case <-w.stopCh:
				return
			case <-ctx.Done():
				return
			case <-w.wake:
			}
		}
	}
}

// waitDelay is the minimal wait seam: the published next_attempt minus
// now, the interval when unpublished, never negative. Hermetic tests
// assert the published deadline without sleeping through it.
func (w *Worker) waitDelay() time.Duration {
	now := w.clock.Sample().Mono
	if next := w.reg.CatalogEvent().NextAttempt; next != nil {
		if *next <= now {
			return 0
		}
		return time.Duration(float64(*next-now) * float64(time.Second))
	}
	return time.Duration(w.cat.RefreshSeconds * float64(time.Second))
}

// Stop is idempotent: it cancels in-flight startup/periodic probes and
// joins the loop and the current attempt before returning, so no
// publication races shutdown. It never touches the registry pointer
// beyond what RefreshOnce already published.
func (w *Worker) Stop() {
	w.stopOnce.Do(func() { close(w.stopCh) })
	w.stateMu.Lock()
	started := w.started
	w.stateMu.Unlock()
	if started {
		<-w.loopDone
	}
	// Join an in-flight attempt; its request context is already canceled
	// above, so the client bound (not the drain bound) releases promptly.
	w.attemptMu.Lock()
	w.attemptMu.Unlock()
}

// CloseIdleConnections releases the catalog client's idle keep-alive
// pool (readiness probes and shutdown paths).
func (w *Worker) CloseIdleConnections() {
	w.client.CloseIdleConnections()
}
