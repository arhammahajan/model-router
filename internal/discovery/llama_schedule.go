package discovery

import (
	"context"
	"time"
)

// Start begins periodic refresh after the startup attempt. It is a no-op
// when RefreshSeconds==0 and idempotent otherwise: a second call while
// started returns without spawning, even after the first loop is canceled.
func (w *LlamaWorker) Start(ctx context.Context) {
	w.stateMu.Lock()
	if w.refresh == 0 || w.started {
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

func (w *LlamaWorker) loop(ctx context.Context) {
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

// waitDelay is the minimal wait seam: the published aggregate
// next_attempt minus now, the interval when unpublished, never negative.
func (w *LlamaWorker) waitDelay() time.Duration {
	now := w.clock.Sample().Mono
	if next := w.reg.LlamaAggregate().NextAttempt; next != nil {
		if *next <= now {
			return 0
		}
		return time.Duration(float64(*next-now) * float64(time.Second))
	}
	return time.Duration(w.refresh * float64(time.Second))
}

// Stop is idempotent: it cancels in-flight startup/periodic probes and
// joins the loop and the current attempt before returning, so no
// publication races shutdown. It never touches the registry pointer
// beyond what RefreshOnce already published.
func (w *LlamaWorker) Stop() {
	w.stopOnce.Do(func() { close(w.stopCh) })
	w.stateMu.Lock()
	started := w.started
	w.stateMu.Unlock()
	if started {
		<-w.loopDone
	}
	// Join an in-flight attempt; its request context is already canceled
	// above, so the per-server probe bounds (not the drain bound) release
	// promptly.
	w.attemptMu.Lock()
	w.attemptMu.Unlock()
}

// CloseIdleConnections releases the llama client's idle keep-alive pool
// (readiness probes and shutdown paths).
func (w *LlamaWorker) CloseIdleConnections() {
	w.client.CloseIdleConnections()
}
