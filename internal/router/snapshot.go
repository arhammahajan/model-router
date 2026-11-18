// Package router owns the immutable static route snapshot: validated
// explicit [[models]] routes plus request-time dynamic provider/model
// resolution. One Snapshot is built per process from defensive copies and
// never mutated afterwards, so concurrent ingress handlers share it
// without locks. Returned values are fresh copies; callers cannot mutate
// stored state through inputs or outputs.
package router

import (
	"sort"
	"strings"

	"github.com/arhammahajan/model-router/internal/capability"
	"github.com/arhammahajan/model-router/internal/catalog"
	"github.com/arhammahajan/model-router/internal/config"
)

// Snapshot is the immutable routing table: explicit static routes plus
// llama canonicals in server order plus catalog entries in fetch order,
// with the catalog-unmapped ID set. The unreachable set names active llama
// providers whose last probe failed: their stale routes still resolve but
// never count toward readiness.
//
// The snapshot also pins one coherent diagnostic source status: staged
// source events, per-server facts and the cache event are swapped with the
// routes, so one request pins routes and status together. Times are facts
// only; ages and staleness derive at render from one sampled Mono.
// Alias totals are pinned for readiness/provider counts, which count
// effective named map keys including aliases.
type Snapshot struct {
	capabilityOverrides map[string]capability.Overrides
	// index resolves explicit public name or alias -> models position.
	index map[string]int
	// models holds all explicit routes in config order (enabled and
	// disabled); disabled entries stay reserved and never route.
	models []config.Model
	// providers holds defensive copies keyed by provider name, including
	// active llama-generated providers (reachable and stale).
	providers map[string]*config.Provider
	// allowDynamic mirrors server.allow_dynamic_routes for provider/model
	// fallback after explicit (including disabled) names are checked.
	allowDynamic bool
	// llama holds llama canonicals in configured-server then fetch order
	// (all enabled, including stale).
	llama []config.Model
	// llamaIndex resolves llama canonical name or alias -> llama position.
	llamaIndex map[string]int
	// unreachable names active llama providers excluded from readiness.
	unreachable map[string]struct{}
	// catalog holds mapped catalog routes in fetch order (all enabled).
	catalog []config.Model
	// catalogIndex resolves catalog name -> catalog position.
	catalogIndex map[string]int
	// catalogModels counts distinct source IDs before public precedence filtering.
	catalogModels int
	// unmapped holds catalog IDs with no trusted provider mapping.
	unmapped map[string]struct{}
	// cat is the staged catalog source event.
	cat SourceEvent
	// llamaAgg is the staged llama aggregate source event.
	llamaAgg SourceEvent
	// llamaServers holds per-server facts keyed by trusted provider name.
	llamaServers map[string]ServerEvent
	// llamaProviders names providers active from the llama layer.
	llamaProviders map[string]struct{}
	// cache is the staged discovery-cache event.
	cache CacheEvent
	// explicitAliases counts enabled explicit aliases per provider.
	explicitAliases map[string]int
	// llamaAliases counts published llama aliases per provider.
	llamaAliases map[string]int
	// catAt/catOK are the original catalog wall success time and presence.
	catAt Wall
	catOK bool
	// llamaAt holds original per-server wall success times by provider.
	llamaAt map[string]Wall
	// catalogCaps holds the private per-route catalog capability bases,
	// keyed by listed catalog name. Bases carry reviewed facts (when the
	// review source is selected) plus the catalog-availability source at
	// the original wall success time. Overrides, normalize and freshness
	// apply at render; the stored bases are never mutated after build.
	catalogCaps map[string]capability.Contract
	// llamaCaps holds the private per-route llama capability bases, keyed
	// by listed llama canonical name. Bases are ObservedLlama at the
	// original per-server wall success time (stale servers reuse their
	// prior wall, never the new fleet time), layered with explicit limit
	// policy then the runtime mapper facts for live-known or
	// stale-retained runtimes (Unobserved restores add no mapper).
	// Restored route-only rows have no observation: with an explicit
	// policy they carry an empty base plus config facts (no fake probe
	// evidence); without a policy they have no base and read as Empty.
	llamaCaps map[string]capability.Contract
	// llamaRuntime holds the pinned per-server engine runtime keyed by
	// trusted generated provider name. Request preparation and listing
	// share this same-pin selector; caches stay route-only.
	llamaRuntime map[string]capability.LocalRuntime
	// llamaPolicy is the trusted explicit limit policy deep-copied from
	// config at Registry New. Nil means absent (unknown). It publishes
	// atomically with routes, wall times and runtimes; later config
	// mutation cannot reach pinned snapshots.
	llamaPolicy *capability.LlamaLimitPolicy
}

// New builds an immutable snapshot from validated config. It deep-copies
// models (aliases, metadata bytes), providers (headers map, source-header
// slice, count_tokens flag), capability overrides and the explicit llama
// limit policy, plus the name/alias index, so later mutation of
// the source config cannot alter the snapshot.
func New(cfg *config.Config) *Snapshot {
	s := &Snapshot{
		capabilityOverrides: capability.CloneOverrideSet(cfg.CapabilityOverrides),
		index:               make(map[string]int, len(cfg.Models)*2),
		providers:           make(map[string]*config.Provider, len(cfg.Providers)),
		allowDynamic:        cfg.Server.AllowDynamicRoutes,
		llamaIndex:          map[string]int{},
		unreachable:         map[string]struct{}{},
		catalogIndex:        map[string]int{},
		unmapped:            map[string]struct{}{},
		llamaServers:        map[string]ServerEvent{},
		llamaProviders:      map[string]struct{}{},
		explicitAliases:     map[string]int{},
		llamaAliases:        map[string]int{},
		llamaAt:             map[string]Wall{},
		catalogCaps:         map[string]capability.Contract{},
		llamaCaps:           map[string]capability.Contract{},
		llamaRuntime:        map[string]capability.LocalRuntime{},
	}
	if cfg.Llama != nil {
		s.llamaPolicy = capability.NewLlamaLimitPolicy(cfg.Llama.ContextFallback, cfg.Llama.LimitOverrides)
	}
	for name, p := range cfg.Providers {
		s.providers[name] = copyProvider(p)
	}
	for _, m := range cfg.Models {
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
	return s
}

// copyModel deep-copies one model entry.
func copyModel(m config.Model) config.Model {
	out := m
	if m.Aliases != nil {
		out.Aliases = append([]string(nil), m.Aliases...)
	}
	if m.Metadata != nil {
		out.Metadata = append([]byte(nil), m.Metadata...)
	}
	return out
}

// copyProvider deep-copies one provider entry.
func copyProvider(p *config.Provider) *config.Provider {
	if p == nil {
		return nil
	}
	out := *p
	if p.Headers != nil {
		out.Headers = make(map[string]string, len(p.Headers))
		for k, v := range p.Headers {
			out.Headers[k] = v
		}
	}
	if p.TranslatedHeaders != nil {
		out.TranslatedHeaders = make(map[string]string, len(p.TranslatedHeaders))
		for k, v := range p.TranslatedHeaders {
			out.TranslatedHeaders[k] = v
		}
	}
	if p.SessionSourceHeaders != nil {
		out.SessionSourceHeaders = append([]string{}, p.SessionSourceHeaders...)
	}
	if p.CountTokens != nil {
		b := *p.CountTokens
		out.CountTokens = &b
	}
	return &out
}

// Resolve maps a public model name to its provider route.
//
// Precedence: explicit name/alias first (a disabled explicit name is
// reserved and never falls through to llama, catalog or dynamic routing),
// then llama hit (alias resolves to its canonical), then catalog hit, then
// catalog-unmapped fault, then dynamic "provider/model-id" when enabled.
// The provider must be in-snapshot (active): an unreachable-stale llama
// route still resolves, but a never-succeeded llama provider has no
// dynamic fallback. It returns fresh copies on success; the fault selects
// the caller's ingress envelope.
func (s *Snapshot) Resolve(name string) (*config.Model, *config.Provider, ResolveFault) {
	if pos, found := s.index[name]; found {
		stored := &s.models[pos]
		if stored.Disabled {
			return nil, nil, DisabledReserved
		}
		p, ok := s.providers[stored.Provider]
		if !ok {
			return nil, nil, NotFound
		}
		m := copyModel(*stored)
		return &m, copyProvider(p), Found
	}
	if pos, found := s.llamaIndex[name]; found {
		stored := &s.llama[pos]
		p, ok := s.providers[stored.Provider]
		if !ok {
			return nil, nil, NotFound
		}
		m := copyModel(*stored)
		return &m, copyProvider(p), Found
	}
	if pos, found := s.catalogIndex[name]; found {
		stored := &s.catalog[pos]
		p, ok := s.providers[stored.Provider]
		if !ok {
			return nil, nil, NotFound
		}
		m := copyModel(*stored)
		return &m, copyProvider(p), Found
	}
	if _, found := s.unmapped[name]; found {
		return nil, nil, UnmappedCatalog
	}
	if s.allowDynamic {
		if prov, id, ok := strings.Cut(name, "/"); ok {
			id = catalog.StripModelID(id)
			if p, exists := s.providers[prov]; exists && id != "" {
				m := config.Model{Name: name, Provider: prov, Upstream: id}
				return &m, copyProvider(p), Found
			}
		}
	}
	return nil, nil, NotFound
}

// UsableCount reports usable effective named keys including aliases:
// enabled explicit routes plus reachable llama canonicals plus catalog
// entries. Dynamic-only configurations count as zero: readiness requires
// at least one usable named route. Catalog staleness never removes usable
// routes; llama unreachability does.
func (s *Snapshot) UsableCount() int {
	return s.UsableRoutes()
}

// Ready reports whether at least one enabled explicit, llama or catalog
// route is usable (reachable).
func (s *Snapshot) Ready() bool {
	return s.UsableRoutes() > 0
}

// ProviderCount reports the registry size: static plus active llama
// providers. Matches diagnostics `providers`.
func (s *Snapshot) ProviderCount() int {
	return len(s.providers)
}

// ProviderDialects returns provider name to dialect.
func (s *Snapshot) ProviderDialects() map[string]string {
	out := make(map[string]string, len(s.providers))
	for name, p := range s.providers {
		out[name] = p.Dialect
	}
	return out
}

// ProviderNames returns sorted provider names.
func (s *Snapshot) ProviderNames() []string {
	out := make([]string, 0, len(s.providers))
	for name := range s.providers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// IsUnreachable reports whether the provider's last probe failed while
// retaining stale routes.
func (s *Snapshot) IsUnreachable(name string) bool {
	_, bad := s.unreachable[name]
	return bad
}

// HasLlamaProvider reports whether the provider is active from the llama
// layer (reachable or stale).
func (s *Snapshot) HasLlamaProvider(name string) bool {
	_, ok := s.llamaProviders[name]
	return ok
}

// LlamaRuntime returns the pinned engine runtime for a generated
// provider. Unobserved means cache-only, restored or never-probed: no live
// probe has established an engine. Non-local providers are Unobserved.
func (s *Snapshot) LlamaRuntime(name string) capability.LocalRuntime {
	if s == nil {
		return capability.RuntimeUnobserved
	}
	if rt, ok := s.llamaRuntime[name]; ok {
		return rt
	}
	return capability.RuntimeUnobserved
}

// ProviderEnabledRoutes counts effective named keys per provider,
// aliases included.
func (s *Snapshot) ProviderEnabledRoutes() map[string]int {
	return s.providerEnabledRoutes()
}

// CacheEvent returns a copy of the staged cache event.
func (s *Snapshot) CacheEvent() CacheEvent {
	return copyCacheEvent(s.cache)
}

// Models returns deep copies of the enabled explicit routes in config
// order followed by llama canonicals in server order followed by catalog
// routes in fetch order, for the /v1/models listing. Stale retained llama
// routes are listed; aliases add no rows. Each call allocates fresh slices
// so callers cannot mutate snapshot state.
func (s *Snapshot) Models() []config.Model {
	out := make([]config.Model, 0, len(s.models)+len(s.llama)+len(s.catalog))
	for _, m := range s.models {
		if m.Disabled {
			continue
		}
		out = append(out, copyModel(m))
	}
	for _, m := range s.llama {
		out = append(out, copyModel(m))
	}
	for _, m := range s.catalog {
		out = append(out, copyModel(m))
	}
	return out
}

// CatalogBase returns a caller-owned copy of the private per-route catalog
// base for one listed catalog name (reviewed facts plus catalog
// availability at the original wall time, before overrides/normalize).
// Static, llama and dynamic routes have no stored base.
func (s *Snapshot) CatalogBase(name string) (capability.Contract, bool) {
	if s == nil || s.catalogCaps == nil {
		return capability.Contract{}, false
	}
	base, ok := s.catalogCaps[name]
	if !ok {
		return capability.Contract{}, false
	}
	return base.Clone(), true
}

// LlamaBase returns a caller-owned copy of the private per-route llama
// base for one listed llama canonical name (observed facts at the
// original per-server wall time, before overrides/normalize). Losing
// collisions, aliases, static, catalog and dynamic routes have no base.
func (s *Snapshot) LlamaBase(name string) (capability.Contract, bool) {
	if s == nil || s.llamaCaps == nil {
		return capability.Contract{}, false
	}
	base, ok := s.llamaCaps[name]
	if !ok {
		return capability.Contract{}, false
	}
	return base.Clone(), true
}
