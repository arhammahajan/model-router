// Restore and status-note paths: startup transactional restore and
// registry-owned status selectors. Production live paths (Commit*) live
// in registry.go; all three swap one complete snapshot per call.
package router

import "github.com/arhammahajan/model-router/internal/capability"

type CatalogInventory struct {
	Entries     []CatalogEntry
	SucceededAt Wall
}

// LlamaInventory is one validated per-server llama unit: staged entries
// plus the original wall success time.
type LlamaInventory struct {
	Entries     []LlamaEntry
	SucceededAt Wall
}

// DiscoveryInventory is the coherent staged source pair for restore and
// save. Catalog is nil when never succeeded; Llama is keyed by trusted
// provider name and holds only restored servers.
type DiscoveryInventory struct {
	Catalog *CatalogInventory
	Llama   map[string]LlamaInventory
}

// ExportDiscovery returns deep copies of the staged source layers plus
// their original wall success times, copied under the short publish mutex.
// It never reads Snapshot.Models(): the public list merges servers, drops
// hidden per-server routes, aliases and collision detail, and carries no
// timestamps. Saves use the exported times verbatim and never advance TTL.
func (r *Registry) ExportDiscovery() DiscoveryInventory {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := DiscoveryInventory{Llama: map[string]LlamaInventory{}}
	if r.catOK {
		out.Catalog = &CatalogInventory{
			Entries:     copyCatalogEntries(r.catE),
			SucceededAt: r.catAt,
		}
	}
	for name, at := range r.llamaAt {
		snap, ok := r.llama.Servers[name]
		if !ok || snap.State == LlamaNeverSucceeded {
			continue
		}
		cp := make([]LlamaEntry, 0, len(snap.Entries))
		for _, e := range snap.Entries {
			ec := LlamaEntry{Name: e.Name, Upstream: e.Upstream, Obs: e.Obs.Clone()}
			if e.Aliases != nil {
				ec.Aliases = append([]string(nil), e.Aliases...)
			}
			if e.Metadata != nil {
				ec.Metadata = append([]byte(nil), e.Metadata...)
			}
			cp = append(cp, ec)
		}
		out.Llama[name] = LlamaInventory{Entries: cp, SucceededAt: at}
	}
	return out
}

// RestoreCached validates both layers, runs one build and only then
// assigns the staged layers, cached facts, per-layer success times and
// snapshot pointer together under the publish mutex. It is startup-only:
// it rejects if any live discovery publication already committed. Any
// validation or build failure leaves the pointer and both staged layers
// unchanged. The cache load derives loaded (any valid unexpired unit,
// even with zero rows) versus empty (no units); expired info is absent.
// Cache-enabled restores go through RestoreCachedWithStatus so the expired
// count publishes atomically with the routes.
func (r *Registry) RestoreCached(inv DiscoveryInventory) error {
	load := "empty"
	if inv.Catalog != nil || len(inv.Llama) > 0 {
		load = "loaded"
	}
	return r.restoreLocked(inv, load, nil)
}

// RestoreCachedWithStatus is the CacheStore restore path: one atomic
// publication of routes, cached facts, wall times and the cache event
// including the expired-source count. Missing/invalid outcomes must use
// NoteCacheStatus instead: they are status-only and never set
// livePublished.
func (r *Registry) RestoreCachedWithStatus(inv DiscoveryInventory, load string, expired *int) error {
	return r.restoreLocked(inv, load, expired)
}

func (r *Registry) restoreLocked(inv DiscoveryInventory, load string, expired *int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.livePublished {
		return errCatalogEntry("discovery restore: live publication already committed")
	}
	var stagedE []CatalogEntry
	var stagedU []string
	var catAt Wall
	var catOK bool
	if inv.Catalog != nil {
		if !finiteWall(float64(inv.Catalog.SucceededAt)) {
			return errCatalogEntry("discovery restore: invalid catalog time")
		}
		stagedE = copyCatalogEntries(inv.Catalog.Entries)
		catAt, catOK = inv.Catalog.SucceededAt, true
		for _, e := range stagedE {
			if e.Name == "" || e.Upstream == "" {
				return errCatalogEntry("discovery restore: invalid catalog entry")
			}
		}
	}
	staged := LlamaLayer{Servers: map[string]LlamaServerSnapshot{}}
	nextAt := map[string]Wall{}
	if inv.Llama != nil {
		for name, unit := range inv.Llama {
			if r.trustedLlama[name] == nil {
				return errLlamaEntry("discovery restore: unknown provider")
			}
			if !finiteWall(float64(unit.SucceededAt)) {
				return errLlamaEntry("discovery restore: invalid llama time")
			}
			cp := make([]LlamaEntry, 0, len(unit.Entries))
			for _, e := range unit.Entries {
				if e.Name == "" || e.Upstream == "" {
					return errLlamaEntry("discovery restore: invalid llama entry")
				}
				// Route-only v2 cache persists no observations: restored
				// rows carry nil obs (unknown until a fresh probe), even
				// when the staged inventory carries a live obs. Never
				// reconstruct observations from cached metadata.
				ec := LlamaEntry{Name: e.Name, Upstream: e.Upstream}
				if e.Aliases != nil {
					ec.Aliases = append([]string(nil), e.Aliases...)
				}
				if e.Metadata != nil {
					ec.Metadata = append([]byte(nil), e.Metadata...)
				}
				cp = append(cp, ec)
			}
			staged.Servers[name] = LlamaServerSnapshot{State: LlamaUnreachableStale,
				Runtime: capability.RuntimeUnobserved, Entries: cp}
			nextAt[name] = unit.SucceededAt
		}
	}
	// Fill configured but unrestored servers as never-succeeded so the
	// build sees a complete layer.
	for _, name := range r.llamaOrder {
		if _, ok := staged.Servers[name]; !ok {
			staged.Servers[name] = LlamaServerSnapshot{State: LlamaNeverSucceeded}
		}
	}
	if err := checkLlamaLayer(staged, r.trustedLlama); err != nil {
		return err
	}
	// One clock read maps every wall age onto mono: catalog mono success
	// is nowMono-(wall-at); the llama aggregate stays unknown even with
	// cached per-server times.
	now := r.clock.Sample()
	const cachedErr = "cached discovery; live availability unverified"
	var catEv SourceEvent
	if catOK {
		ms := now.Mono - Mono(float64(now.Wall)-float64(catAt))
		catEv = SourceEvent{LastSuccess: &ms, Failures: 0, Err: cachedErr, Cached: true}
	}
	llamaEv := map[string]ServerEvent{}
	for name, at := range nextAt {
		ms := now.Mono - Mono(float64(now.Wall)-float64(at))
		n := len(staged.Servers[name].Entries)
		var u string
		if p := r.trustedLlama[name]; p != nil {
			u = p.BaseURL
		}
		llamaEv[name] = ServerEvent{URL: u, Models: n,
			SourceEvent: SourceEvent{LastSuccess: &ms, Failures: 0, Err: cachedErr, Cached: true}}
	}
	var llamaAggEv SourceEvent
	if len(nextAt) > 0 {
		llamaAggEv = SourceEvent{Err: cachedErr}
	}
	cacheEv := CacheEvent{Load: load, Expired: expired, Enabled: r.cacheEv.Enabled}
	snap, err := buildSnapshot(r.static, r.trustedLlama, r.llamaOrder, stagedE, stagedU, staged,
		catEv, llamaAggEv, llamaEv, cacheEv, catAt, catOK, nextAt, r.review, r.hasReview)
	if err != nil {
		return err
	}
	r.catE, r.catU = stagedE, stagedU
	r.catAt, r.catOK = catAt, catOK
	r.llama = staged
	r.llamaAt = nextAt
	r.catEv = catEv
	r.llamaAggEv = llamaAggEv
	r.llamaEv = llamaEv
	r.cacheEv = cacheEv
	r.ptr.Store(snap)
	return nil
}

// NoteCacheStatus publishes a status-only cache outcome (missing, invalid,
// incompatible, configuration_changed, disabled, skipped): routes, source
// layers and wall times are untouched and livePublished never flips, so a
// later valid-startup-restore guard still works. A canceled restore may
// publish only this skipped status; it never restores routes.
func (r *Registry) NoteCacheStatus(load string, expired *int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cacheEv = CacheEvent{Load: load, Expired: expired, Enabled: r.cacheEv.Enabled,
		WriteErr: r.cacheEv.WriteErr}
	r.ptr.Store(r.buildSeedLocked())
}

// NoteCacheWriteErr records an async cache write outcome, preserving
// source layers, wall times and the expired-sources presence. A nil error
// clears the write error.
func (r *Registry) NoteCacheWriteErr(err *string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cacheEv = CacheEvent{Load: r.cacheEv.Load, Expired: r.cacheEv.Expired,
		Enabled: r.cacheEv.Enabled, WriteErr: err}
	r.ptr.Store(r.buildSeedLocked())
}

// NoteLlamaFailure records a rejected fleet publication without touching
// routes or wall times: the attempted layer was invalid, so only the
// aggregate status and per-server facts update. Current layers always
// rebuild, so this never fails.
func (r *Registry) NoteLlamaFailure(agg SourceEvent, servers map[string]ServerEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	snap, err := buildSnapshot(r.static, r.trustedLlama, r.llamaOrder, r.catE, r.catU, r.llama,
		r.catEv, agg, servers, r.cacheEv, r.catAt, r.catOK, r.llamaAt, r.review, r.hasReview)
	if err != nil {
		return
	}
	r.llamaAggEv = copySourceEvent(agg)
	r.llamaEv = copyServerEvents(servers)
	r.livePublished = true
	r.ptr.Store(snap)
}

// CatalogEvent returns a copy of the staged catalog source event.
func (r *Registry) CatalogEvent() SourceEvent {
	return copySourceEvent(r.Current().cat)
}

// LlamaAggregate returns a copy of the staged llama aggregate event.
func (r *Registry) LlamaAggregate() SourceEvent {
	return copySourceEvent(r.Current().llamaAgg)
}

// ServerEvents returns a copy of the staged per-server facts.
func (r *Registry) ServerEvents() map[string]ServerEvent {
	return copyServerEvents(r.Current().llamaServers)
}

// LlamaOrder returns the configured server walk order.
func (r *Registry) LlamaOrder() []string {
	return append([]string(nil), r.llamaOrder...)
}

// Clock returns the registry-owned injectable clock.
func (r *Registry) Clock() Clock {
	return r.clock
}
