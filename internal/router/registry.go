// Package router registry: the immutable-snapshot publication paths for
// catalog and llama routes.
//
// A Registry is built once from validated config (the static seed plus the
// trusted llama-generated provider inventory) and then only ever swaps in
// fully built immutable Snapshots through PublishCatalog/PublishLlama,
// which atomically rebuild from the staged layers. Readers pin one
// Snapshot per request with Current and never observe a half-published
// table. Only the short publish mutex serializes writers; there are no
// locks on the read path.
package router

import (
	"math"
	"sync"
	"sync/atomic"

	"github.com/arhammahajan/router/internal/capability"
	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/redact"
)

// ResolveFault names the resolve outcome. Callers map faults to ingress
// envelopes; there is no boolean-compat path.
type ResolveFault int

const (
	// Found means the name resolved to a usable route.
	Found ResolveFault = iota
	// DisabledReserved means an explicit (possibly aliased) name is
	// configured but disabled: reserved, never routed, no fallthrough.
	DisabledReserved
	// UnmappedCatalog means the catalog lists the ID but no trusted
	// provider mapping covers it: 404 model_endpoint_unknown.
	UnmappedCatalog
	// NotFound means no explicit, llama, catalog or dynamic route matched.
	NotFound
)

// CatalogEntry is one mapped catalog route in fetch order. Provider names
// a trusted configured provider; the payload never supplies endpoints,
// keys or capabilities. Metadata is the raw row metadata copy.
type CatalogEntry struct {
	Name     string
	Provider string
	Upstream string
	Metadata []byte
}

// LlamaState is the per-server publication state. Reachable (even with a
// valid empty model list) and UnreachableStale activate their generated
// provider; NeverSucceeded never does.
type LlamaState uint8

const (
	// LlamaNeverSucceeded means the server never probed successfully:
	// no provider, no routes.
	LlamaNeverSucceeded LlamaState = iota
	// LlamaReachable means the last probe succeeded: provider active.
	LlamaReachable
	// LlamaUnreachableStale means a later probe failed: the provider
	// stays active with retained routes, excluded only from readiness.
	LlamaUnreachableStale
)

// LlamaEntry is one canonical llama route in fetch order. Aliases are
// candidates published after the server's canonicals; canonical rows win
// within each server, first-wins across servers. Obs is the optional
// closed llama observation (nil means no observed evidence, including
// cache restores); it is cloned on every ownership boundary.
type LlamaEntry struct {
	Name     string
	Upstream string
	Aliases  []string
	Metadata []byte
	Obs      *capability.LlamaModelObs
}

// LlamaServerSnapshot is one server's validated publication: the state,
// its engine runtime, plus its canonical entries in fetch order.
// NeverSucceeded requires an empty entry list. Runtime is Unobserved when
// no live probe has established it (cache restore); unknown never becomes
// non-local. It publishes atomically with routes, status and wall times.
type LlamaServerSnapshot struct {
	State   LlamaState
	Runtime capability.LocalRuntime
	Entries []LlamaEntry
}

// LlamaLayer is the validated llama publication keyed by generated
// provider name. Keys must reference the registry's trusted
// generated-provider inventory; the registry walks configured server order.
type LlamaLayer struct {
	Servers map[string]LlamaServerSnapshot
}

// Registry holds the current immutable Snapshot. The static seed owns the
// trusted explicit-route and provider state; CommitCatalog/CommitLlama are
// the only live mutation paths and each rebuilds from the staged layers,
// so concurrent catalog and llama publications never lose the other
// source. Failed publications update status while keeping the cached
// layers: routes, wall times and unmapped IDs are retained. RestoreCached
// is the startup-only transactional restore path: it validates both
// layers and swaps them atomically.
type Registry struct {
	static *Snapshot
	mu     sync.Mutex // short publish mutex only
	clock  Clock
	// corpus is the expanded app-owned secret corpus for diagnostic
	// redaction, built once from validated config.
	corpus []string
	// trustedLlama is the config-built generated-provider inventory,
	// copied from trusted config, never from payloads.
	trustedLlama map[string]*config.Provider
	// llamaOrder is the configured server (provider) walk order.
	llamaOrder []string
	// catE/catU/llama are the cached independent route layers.
	catE  []CatalogEntry
	catU  []string
	llama LlamaLayer
	// catAt/catOK and llamaAt are the source-owned wall success times.
	// Catalog has one unit; llama is keyed by trusted provider name.
	catAt   Wall
	catOK   bool
	llamaAt map[string]Wall
	// review is the snapshot-owned validated bundled asset copy plus its
	// presence flag. Copied once from trusted config at construction; later
	// config mutation cannot reach published snapshots.
	review    capability.ReviewAsset
	hasReview bool
	// catEv/llamaAggEv/llamaEv are the staged source status layers.
	catEv      SourceEvent
	llamaAggEv SourceEvent
	llamaEv    map[string]ServerEvent
	// cacheEv is the staged discovery-cache event.
	cacheEv CacheEvent
	// livePublished marks any committed live discovery publication.
	// Restore is startup-only and rejects after this flips. Status-only
	// cache outcomes (missing/invalid/skipped) never set it.
	livePublished bool
	ptr           atomic.Pointer[Snapshot]
}

// NewRegistry builds the static seed from validated config and publishes
// it. It performs no I/O.
func NewRegistry(cfg *config.Config) *Registry {
	return NewRegistryWithClock(cfg, SystemClock{})
}

// NewRegistryWithClock builds the registry with an injected clock for
// hermetic tests. Production uses SystemClock.
func NewRegistryWithClock(cfg *config.Config, clock Clock) *Registry {
	if clock == nil {
		clock = SystemClock{}
	}
	trusted := map[string]*config.Provider{}
	var order []string
	if cfg.Llama != nil {
		trusted = cfg.LlamaProviders()
		order = make([]string, 0, len(cfg.Llama.Servers))
		for _, s := range cfg.Llama.Servers {
			order = append(order, s.Name)
		}
	}
	s := New(cfg)
	r := &Registry{
		static:       s,
		clock:        clock,
		corpus:       redact.ExpandCorpus(config.ConfigSecrets(cfg)),
		trustedLlama: trusted,
		llamaOrder:   order,
		llama:        LlamaLayer{Servers: map[string]LlamaServerSnapshot{}},
		llamaAt:      map[string]Wall{},
		llamaEv:      map[string]ServerEvent{},
		cacheEv:      CacheEvent{Enabled: cfg.DiscoveryCache != nil, Load: initialCacheLoad(cfg)},
	}
	if cfg.Catalog != nil && cfg.Catalog.HasReview {
		r.review = cfg.Catalog.ReviewAsset.Clone()
		r.hasReview = true
	}
	// Pin the seed with zero statuses so Current never returns nil.
	r.ptr.Store(r.buildSeedLocked())
	return r
}

// initialCacheLoad is the pre-restore cache load value.
func initialCacheLoad(cfg *config.Config) string {
	if cfg.DiscoveryCache == nil {
		return "disabled"
	}
	return "missing"
}

// Sanitize redacts one diagnostic string with the registry-owned
// app-secret corpus. Read paths use it for URLs and errors.
func (r *Registry) Sanitize(s string) string {
	return redact.Redact(s, r.corpus)
}

// SanitizeReport recursively sanitizes report keys and values with the
// registry-owned corpus, mirroring Python _sanitize_details.
func (r *Registry) SanitizeReport(v any) any {
	return redact.SanitizeDetails(v, r.corpus)
}

// CacheStatus returns a copy of the registry-owned cache event: the thin
// selector over canonical status, never a mutable mirror.
func (r *Registry) CacheStatus() CacheEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return copyCacheEvent(r.cacheEv)
}

// buildSeedLocked rebuilds the seed snapshot with the staged statuses.
// Callers hold mu.
func (r *Registry) buildSeedLocked() *Snapshot {
	snap, err := buildSnapshot(r.static, r.trustedLlama, r.llamaOrder,
		r.catE, r.catU, r.llama,
		r.catEv, r.llamaAggEv, r.llamaEv, r.cacheEv,
		r.catAt, r.catOK, r.llamaAt, r.review, r.hasReview)
	if err != nil {
		// Seed layers are empty and always build; fall back to static.
		return r.static
	}
	return snap
}

// Current pins the latest complete Snapshot for one request. It never
// returns nil: the constructor stores the static seed before sharing.
func (r *Registry) Current() *Snapshot {
	if s := r.ptr.Load(); s != nil {
		return s
	}
	return r.static
}

// CommitCatalog stages one catalog attempt and swaps one complete snapshot.
// Success (non-nil) validates and stages entries/unmapped plus the wall
// time; failure (nil) keeps routes, wall times and unmapped IDs while
// updating status. Both publish atomically under the publish mutex.
func (r *Registry) CommitCatalog(attempt CatalogAttempt) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	stagedE := r.catE
	stagedU := r.catU
	catAt, catOK := r.catAt, r.catOK
	if attempt.Success != nil {
		stagedE = copyCatalogEntries(attempt.Success.Entries)
		stagedU = append([]string(nil), attempt.Success.Unmapped...)
		catAt, catOK = attempt.Success.Wall, true
	}
	snap, err := buildSnapshot(r.static, r.trustedLlama, r.llamaOrder, stagedE, stagedU, r.llama,
		attempt.Event, r.llamaAggEv, r.llamaEv, r.cacheEv, catAt, catOK, r.llamaAt, r.review, r.hasReview)
	if err != nil {
		return err
	}
	if attempt.Success != nil {
		r.catE, r.catU = stagedE, stagedU
		r.catAt, r.catOK = catAt, catOK
	}
	r.catEv = copySourceEvent(attempt.Event)
	r.livePublished = true
	r.ptr.Store(snap)
	return nil
}

// CommitLlama stages one complete fleet attempt and swaps one complete
// snapshot. The layer carries every configured server; reachable servers
// advance to the fleet wall time while stale servers retain theirs. A
// partial fleet (some servers up, some down) publishes routes, aggregate
// status and per-server facts in one publication. Failures retain prior
// per-server times; only the status changes.
func (r *Registry) CommitLlama(attempt LlamaAttempt) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	staged := copyLlamaLayer(attempt.Layer)
	if err := checkLlamaLayer(staged, r.trustedLlama); err != nil {
		return err
	}
	nextAt := make(map[string]Wall, len(r.llamaAt)+len(staged.Servers))
	for k, v := range r.llamaAt {
		nextAt[k] = v
	}
	for name, s := range staged.Servers {
		switch s.State {
		case LlamaReachable:
			nextAt[name] = attempt.AtWall
		case LlamaUnreachableStale:
			// Retain the original time; a stale unit without a prior
			// time carries none (restores always supply one).
			if _, ok := nextAt[name]; !ok {
				delete(nextAt, name)
			}
		default:
			delete(nextAt, name)
		}
	}
	// Absent servers are inactive and carry no time.
	for name := range nextAt {
		if _, ok := staged.Servers[name]; !ok {
			delete(nextAt, name)
		} else if staged.Servers[name].State == LlamaNeverSucceeded {
			delete(nextAt, name)
		}
	}
	snap, err := buildSnapshot(r.static, r.trustedLlama, r.llamaOrder, r.catE, r.catU, staged,
		r.catEv, attempt.Aggregate, attempt.Servers, r.cacheEv, r.catAt, r.catOK, nextAt, r.review, r.hasReview)
	if err != nil {
		return err
	}
	r.llama = staged
	r.llamaAt = nextAt
	r.llamaAggEv = copySourceEvent(attempt.Aggregate)
	r.llamaEv = copyServerEvents(snap.llamaServers)
	r.livePublished = true
	r.ptr.Store(snap)
	return nil
}

func copyWallTimes(in map[string]Wall) map[string]Wall {
	out := make(map[string]Wall, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// checkLlamaLayer rejects unknown providers, unknown states and malformed
// entries before any build. Collision skipping is not fatal and happens
// at build time.
func checkLlamaLayer(layer LlamaLayer, trusted map[string]*config.Provider) error {
	for name, snap := range layer.Servers {
		if trusted[name] == nil {
			return errLlamaEntry("llama layer: unknown provider")
		}
		if snap.State != LlamaNeverSucceeded && snap.State != LlamaReachable &&
			snap.State != LlamaUnreachableStale {
			return errLlamaEntry("llama layer: unknown server state")
		}
		if !snap.Runtime.Valid() {
			return errLlamaEntry("llama layer: unknown server runtime")
		}
		if snap.State == LlamaNeverSucceeded && len(snap.Entries) > 0 {
			return errLlamaEntry("llama layer: unreached server must publish no entries")
		}
		for _, e := range snap.Entries {
			if e.Name == "" || e.Upstream == "" {
				return errLlamaEntry("llama layer: entry with empty name or upstream")
			}
		}
	}
	return nil
}

// buildSnapshot stages a complete immutable Snapshot from the static seed
// plus both layers and the staged statuses. It copies everything it keeps:
// callers cannot mutate the result through the inputs afterwards. Alias
// totals, provider activation and wall-time copies are pinned here so one
// request derives readiness and diagnostics from one snapshot alone.
// Catalog capability bases are built here at publication time (never on
// GET): reviewed facts from the trusted asset when selected, otherwise
// empty bases, plus catalog availability at the original wall time.
func buildSnapshot(static *Snapshot, trusted map[string]*config.Provider, order []string,
	catE []CatalogEntry, catU []string, layer LlamaLayer,
	catEv SourceEvent, llamaAggEv SourceEvent, llamaEv map[string]ServerEvent, cacheEv CacheEvent,
	catAt Wall, catOK bool, llamaAt map[string]Wall, review capability.ReviewAsset, hasReview bool) (*Snapshot, error) {
	s := &Snapshot{
		capabilityOverrides: static.capabilityOverrides,
		index:               make(map[string]int, len(static.index)),
		providers:           make(map[string]*config.Provider, len(static.providers)+len(trusted)),
		models:              make([]config.Model, 0, len(static.models)),
		allowDynamic:        static.allowDynamic,
		llamaIndex:          map[string]int{},
		unreachable:         map[string]struct{}{},
		catalogIndex:        map[string]int{},
		unmapped:            map[string]struct{}{},
		cat:                 copySourceEvent(catEv),
		llamaAgg:            copySourceEvent(llamaAggEv),
		llamaServers:        copyServerEvents(llamaEv),
		llamaProviders:      map[string]struct{}{},
		cache:               copyCacheEvent(cacheEv),
		explicitAliases:     map[string]int{},
		llamaAliases:        map[string]int{},
		catAt:               catAt,
		catOK:               catOK,
		llamaAt:             copyWallTimes(llamaAt),
		catalogCaps:         map[string]capability.Contract{},
		llamaCaps:           map[string]capability.Contract{},
		llamaRuntime:        map[string]capability.LocalRuntime{},
		llamaPolicy:         static.llamaPolicy.Clone(),
	}
	for k, v := range static.index {
		s.index[k] = v
	}
	for name, p := range static.providers {
		s.providers[name] = copyProvider(p)
	}
	for _, m := range static.models {
		cp := copyModel(m)
		pos := len(s.models)
		s.models = append(s.models, cp)
		s.index[cp.Name] = pos
		for _, a := range cp.Aliases {
			s.index[a] = pos
		}
		if !cp.Disabled {
			s.explicitAliases[cp.Provider] += len(cp.Aliases)
		}
	}
	// Activate llama providers in configured order: reachable and stale
	// both activate (stale is excluded from readiness only); never-succeeded
	// servers (or servers absent from the layer) contribute nothing.
	active := map[string]bool{}
	for _, name := range order {
		snap, ok := layer.Servers[name]
		if !ok || snap.State == LlamaNeverSucceeded {
			continue
		}
		p, known := trusted[name]
		if !known {
			return nil, errLlamaEntry("llama layer: unknown provider")
		}
		s.providers[name] = copyProvider(p)
		active[name] = true
		s.llamaProviders[name] = struct{}{}
		// The engine runtime pins atomically with routes, status and wall
		// times on the same snapshot; concurrent requests share the pin.
		s.llamaRuntime[name] = snap.Runtime
		if snap.State == LlamaUnreachableStale {
			s.unreachable[name] = struct{}{}
		}
	}
	// Walk configured server order, each server's canonicals then aliases.
	seen := map[string]struct{}{}
	for _, name := range order {
		if !active[name] {
			continue
		}
		snap := layer.Servers[name]
		// Only the exact rows accepted in this pass may contribute aliases.
		// A name alone cannot distinguish a winning row from a duplicate
		// canonical or a row shadowed by an earlier server's alias.
		publishedRows := map[int]int{}
		for row, e := range snap.Entries {
			if _, reserved := s.index[e.Name]; reserved {
				continue // explicit (even disabled) reserves the name
			}
			if _, dup := seen[e.Name]; dup {
				continue // first wins across servers
			}
			m := config.Model{Name: e.Name, Provider: name, Upstream: e.Upstream}
			if len(e.Metadata) > 0 {
				m.Metadata = append([]byte(nil), e.Metadata...)
			}
			publishedRows[row] = len(s.llama)
			seen[e.Name] = struct{}{}
			s.llamaIndex[e.Name] = len(s.llama)
			s.llama = append(s.llama, m)
			// Private per-route llama base at the original per-server
			// wall time. Stale servers reuse their prior wall, never the
			// new fleet time. Order: ObservedLlama -> explicit limit
			// policy -> runtime mapper. Unobserved (restored) rows get no
			// mapper; with an explicit policy they still carry an empty
			// base plus config facts (no fake probe evidence). Losing
			// rows store no base, so their caps cannot leak through
			// aliases or later lookups. Public aliases share the
			// canonical fact via Resolve canonicalization (no shadow leak).
			if e.Obs != nil {
				if at, ok := llamaAt[name]; ok {
					base := capability.ObservedLlama(e.Obs, float64(at))
					capability.ApplyLlamaLimits(&base, e.Upstream, s.llamaPolicy)
					if rt := snap.Runtime; rt == capability.RuntimeLlamaCpp ||
						rt == capability.RuntimeStrata {
						capability.ApplyLlamaMapper(&base, rt, float64(at))
					}
					s.llamaCaps[e.Name] = base
				}
			} else if s.llamaPolicy != nil && !s.llamaPolicy.Empty() {
				if at, ok := llamaAt[name]; ok && s.llamaPolicy.Covers(e.Upstream) {
					base := capability.ObservedLlama(nil, float64(at))
					capability.ApplyLlamaLimits(&base, e.Upstream, s.llamaPolicy)
					s.llamaCaps[e.Name] = base
				}
			}
		}
		if event, ok := s.llamaServers[name]; ok && snap.State == LlamaReachable {
			event.Models = len(publishedRows)
			s.llamaServers[name] = event
		}
		for row, e := range snap.Entries {
			pos, published := publishedRows[row]
			if !published {
				continue // a filtered canonical publishes no aliases
			}
			for _, alias := range e.Aliases {
				if alias == "" {
					continue
				}
				if _, reserved := s.index[alias]; reserved {
					continue
				}
				if _, dup := seen[alias]; dup {
					continue
				}
				seen[alias] = struct{}{}
				s.llamaIndex[alias] = pos
				s.llamaAliases[name]++
			}
		}
	}
	// Catalog below llama: explicit wins, llama wins over catalog.
	seenCat := map[string]int{}
	sourceCatalog := map[string]struct{}{}
	for _, e := range catE {
		if e.Name == "" {
			return nil, errCatalogEntry("catalog entry: empty model name")
		}
		if _, known := s.providers[e.Provider]; !known {
			return nil, errCatalogEntry("catalog entry: unknown provider")
		}
		sourceCatalog[e.Name] = struct{}{}
		if _, reserved := s.index[e.Name]; reserved {
			continue // explicit (even disabled) reserves the name
		}
		if _, dup := seen[e.Name]; dup {
			continue // llama (canonical or alias) reserves the name
		}
		m := config.Model{Name: e.Name, Provider: e.Provider, Upstream: e.Upstream}
		if len(e.Metadata) > 0 {
			m.Metadata = append([]byte(nil), e.Metadata...)
		}
		if prev, dup := seenCat[e.Name]; dup {
			s.catalog[prev] = m // last value wins, first position kept
			continue
		}
		seenCat[e.Name] = len(s.catalog)
		s.catalogIndex[e.Name] = len(s.catalog)
		s.catalog = append(s.catalog, m)
	}
	s.catalogModels = len(sourceCatalog)
	for _, id := range catU {
		if id == "" {
			continue
		}
		if _, reserved := s.index[id]; reserved {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		if _, dup := seenCat[id]; dup {
			continue
		}
		s.unmapped[id] = struct{}{}
	}
	// Private per-route catalog bases: explicit shadowing already removed
	// those rows above, so stored bases cover listed catalog rows only.
	// Stored at publication/restore time with the original wall success;
	// GET only looks up, applies overrides, and renders with one clock.
	for _, m := range s.catalog {
		p := s.providers[m.Provider]
		dialect := ""
		if p != nil {
			dialect = m.EffectiveDialect(p)
		}
		var base capability.Contract
		if hasReview {
			base = capability.CatalogReview(m.Name, dialect, review)
		} else {
			base = capability.Empty(dialect)
		}
		if catOK {
			base.NoteCatalogAvailability(float64(catAt))
		}
		s.catalogCaps[m.Name] = base
	}
	return s, nil
}

func copyCatalogEntries(entries []CatalogEntry) []CatalogEntry {
	out := make([]CatalogEntry, 0, len(entries))
	for _, e := range entries {
		cp := e
		if e.Metadata != nil {
			cp.Metadata = append([]byte(nil), e.Metadata...)
		}
		out = append(out, cp)
	}
	return out
}

func copyLlamaLayer(layer LlamaLayer) LlamaLayer {
	out := LlamaLayer{Servers: make(map[string]LlamaServerSnapshot, len(layer.Servers))}
	for name, snap := range layer.Servers {
		cp := LlamaServerSnapshot{State: snap.State, Runtime: snap.Runtime}
		for _, e := range snap.Entries {
			ec := LlamaEntry{Name: e.Name, Upstream: e.Upstream, Obs: e.Obs.Clone()}
			if e.Aliases != nil {
				ec.Aliases = append([]string(nil), e.Aliases...)
			}
			if e.Metadata != nil {
				ec.Metadata = append([]byte(nil), e.Metadata...)
			}
			cp.Entries = append(cp.Entries, ec)
		}
		out.Servers[name] = cp
	}
	return out
}

// CatalogInventory is one validated catalog unit: staged entries plus the
// original wall success time. A nil Catalog means never succeeded.
func finiteWall(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0
}

// NewWithCatalog builds a new immutable Snapshot from a static seed plus
// catalog entries in fetch order. Providers and explicit routes are
// deep-copied from the seed; callers cannot mutate the result through the
// inputs. Unknown entry providers are a validation error. Llama layers are
// empty here; use CommitLlama on a Registry for the merged build.
func NewWithCatalog(static *Snapshot, entries []CatalogEntry, unmapped []string) (*Snapshot, error) {
	return buildSnapshot(static, nil, nil, copyCatalogEntries(entries),
		append([]string(nil), unmapped...), LlamaLayer{Servers: map[string]LlamaServerSnapshot{}},
		SourceEvent{}, SourceEvent{}, map[string]ServerEvent{}, CacheEvent{}, 0, false, map[string]Wall{},
		capability.ReviewAsset{}, false)
}

// catalogEntryError is a static validation failure: it never carries the
// entry name or provider, so ordinary logs stay value-free.
type catalogEntryError struct{ msg string }

func (e *catalogEntryError) Error() string { return e.msg }

func errCatalogEntry(msg string) error { return &catalogEntryError{msg} }

func errLlamaEntry(msg string) error { return &catalogEntryError{msg} }
