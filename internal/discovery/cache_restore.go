package discovery

import (
	"context"
	"log"
	"time"
)

// Restore runs the startup read+validate helper with a 2s handoff
// deadline and selects on ctx. Cancellation abandons the result and the
// load is skipped: a canceled restore may publish only a skipped
// cache-status outcome, never routes. Missing/invalid/incompatible/
// configuration_changed outcomes are status-only publications made before
// any live attempt and never set livePublished, so the later
// valid-startup-restore guard still works. A late FS helper never
// publishes: the handoff channel is depth-1 and only this call reads it.
//
// Successful restores atomically publish routes, cached facts, wall times
// and the cache event (including the expired-source count) in one
// snapshot swap.
func (s *CacheStore) Restore(ctx context.Context, llamaW *LlamaWorker) CacheStatus {
	if s.cfg.DiscoveryCache == nil {
		return s.Status()
	}
	if ctx != nil && ctx.Err() != nil {
		s.skipped()
		return s.Status()
	}
	type loaded struct {
		inv   CachedInventory
		ident string
		st    CacheStatus
		err   error
	}
	ch := make(chan loaded, 1)
	go func() {
		nowWall := float64(wallNow())
		inv, ident, st, err := loadWithIdentity(s.path, s.maxAge, nowWall)
		select {
		case ch <- loaded{inv, ident, st, err}:
		default:
		}
	}()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	var r loaded
	select {
	case r = <-ch:
	case <-timer.C:
		s.skipped()
		return s.Status()
	case <-ctx.Done():
		s.skipped()
		return s.Status()
	}
	if r.err != nil {
		s.reg.NoteCacheStatus(CacheInvalid, nil)
		return s.Status()
	}
	if r.st.Load == CacheIncompatible || r.st.Load == CacheMissing {
		s.reg.NoteCacheStatus(r.st.Load, nil)
		return s.Status()
	}
	if r.ident == "" || r.ident != s.identity {
		s.reg.NoteCacheStatus(CacheConfigurationChanged, nil)
		return s.Status()
	}
	nowWall := float64(wallNow())
	restored, rst, extra := s.buildRestore(r.inv, nowWall)
	if rst.Load == CacheInvalid {
		s.reg.NoteCacheStatus(CacheInvalid, nil)
		return s.Status()
	}
	if ctx != nil && ctx.Err() != nil {
		s.skipped()
		return s.Status()
	}
	total := r.st.ExpiredSources + extra
	load := CacheLoaded
	if restored.Catalog == nil && len(restored.Llama) == 0 {
		load = CacheEmpty
	}
	if err := s.reg.RestoreCachedWithStatus(restored, load, &total); err != nil {
		s.reg.NoteCacheStatus(CacheInvalid, nil)
		return s.Status()
	}
	if llamaW != nil {
		llamaW.seedRetained(restored)
	}
	log.Printf("discovery cache: %s", load)
	return s.Status()
}

func (s *CacheStore) skipped() {
	s.reg.NoteCacheStatus(CacheSkipped, nil)
}
