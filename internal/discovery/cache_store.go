// Discovery persistence: optional route-only disk cache (schema v2).
//
// Go owns schema v2; a Python v1 file loads as incompatible, never parsed.
// Endpoints, keys, headers, metadata and capabilities are never persisted
// and never read. The file carries route IDs and full alias names only;
// providers are re-resolved from trusted config at restore.
package discovery

import (
	"context"
	"os"
	"sync"
	"sync/atomic"

	"github.com/arhammahajan/model-router/internal/config"
	"github.com/arhammahajan/model-router/internal/router"
)

// --- CacheStore: one process-owned I/O owner, depth-1 coalesced ---

// CacheStore owns the single writer for one cache file. Network workers
// only call NotifyDirty; the owner exports, encodes and replaces without
// holding registry or worker locks across filesystem I/O.
type CacheStore struct {
	cfg      *config.Config
	reg      *router.Registry
	path     string
	maxAge   float64
	identity string
	sanitize *cacheSanitizer

	dirty     chan struct{}
	stopCh    chan struct{}
	closed    atomic.Bool
	startOnce sync.Once
	done      chan struct{}
	closeCtx  context.Context // written before stopCh closes
	mu        sync.Mutex

	// closeOnce makes Close idempotent: the background writer is
	// stopped exactly once and post-close notifications stay rejected.
	closeOnce sync.Once

	// Per-store I/O gates (defaults copied from the package vars at
	// construction). Tests set these before Start; they are never
	// mutated while the writer runs, so no shared-variable races.
	mkdirAll      func(string, os.FileMode) error
	createTemp    func(string, string) (*os.File, error)
	renameFn      func(string, string) error
	fsyncDirFn    func(string) error
	beforeEncode  func()
	beforeReplace func()
}

// NewCacheStore builds the owner. It performs no I/O.
func NewCacheStore(cfg *config.Config, reg *router.Registry) *CacheStore {
	s := &CacheStore{
		cfg: cfg, reg: reg,
		dirty:  make(chan struct{}, 1),
		stopCh: make(chan struct{}),
		done:   make(chan struct{}),
	}
	s.mkdirAll = cacheMkdirAll
	s.createTemp = cacheCreateTemp
	s.renameFn = cacheRename
	s.fsyncDirFn = cacheFsyncDir
	if cfg.DiscoveryCache != nil {
		s.path = cfg.DiscoveryCache.Path
		s.maxAge = cfg.DiscoveryCache.MaxAgeSeconds
		s.identity = cacheIdentity(cfg)
		s.sanitize = newCacheSanitizer(cfg)
	}
	return s
}

// Status returns the last value-free load/write outcome: a thin selector
// over the registry-owned cache event, never a mutable mirror.
func (s *CacheStore) Status() CacheStatus {
	ev := s.reg.CacheStatus()
	st := CacheStatus{Load: ev.Load}
	if ev.Expired != nil {
		st.ExpiredSources = *ev.Expired
	}
	if ev.WriteErr != nil {
		w := *ev.WriteErr
		st.WriteErr = &w
	}
	return st
}

// Start begins the background single writer. No-op when disabled.
func (s *CacheStore) Start() {
	if s.cfg.DiscoveryCache == nil || s.closed.Load() {
		return
	}
	s.startWriter()
}

func (s *CacheStore) startWriter() {
	s.startOnce.Do(func() { go s.run() })
}

func (s *CacheStore) run() {
	defer close(s.done)
	for {
		select {
		case <-s.stopCh:
			s.flushPending(false)
			return
		default:
		}
		select {
		case <-s.stopCh:
			s.flushPending(false)
			return
		case <-s.dirty:
			select {
			case <-s.stopCh:
				s.flushPending(true)
				return
			default:
			}
			s.saveOnce()
		}
	}
}

// Only the owner may flush: shutdown never starts a second writer or
// captures an older inventory in parallel with an in-flight transaction.
func (s *CacheStore) flushPending(pending bool) {
	if s.closeCtx.Err() != nil {
		return
	}
	if !pending {
		select {
		case <-s.dirty:
			pending = true
		default:
		}
	}
	if pending {
		s.saveOnce()
	}
}

// NotifyDirty coalesces one save request. It never blocks and never does
// I/O: the single writer consumes it. Rejected after Close.
func (s *CacheStore) NotifyDirty() {
	if s.cfg.DiscoveryCache == nil || s.closed.Load() {
		return
	}
	select {
	case s.dirty <- struct{}{}:
	default:
	}
}
