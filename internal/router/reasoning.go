package router

import (
	"github.com/arhammahajan/router/internal/capability"
	"github.com/arhammahajan/router/internal/config"
)

// CanonicalBase returns a caller-owned copy of the private per-route base
// for one canonical route name: catalog base, llama base, or Empty with the
// effective dialect for static/dynamic routes. It shares the base selection
// used by listing rendering and request planning. Restored route-only rows
// have no observation: with an explicit trusted policy the empty base is
// layered with config facts here as well (no fake probe evidence), so
// listing and planning share the same selector without per-GET I/O.
func (s *Snapshot) CanonicalBase(canonicalName string, effDialect string) capability.Contract {
	if s == nil {
		return capability.Empty(effDialect)
	}
	if _, ok := s.catalogIndex[canonicalName]; ok {
		if stored, ok := s.CatalogBase(canonicalName); ok {
			return stored
		}
		return capability.Empty(effDialect)
	}
	if pos, ok := s.llamaIndex[canonicalName]; ok {
		// Public aliases share the canonical fact: resolve the position
		// to its canonical name/upstream first.
		canonical := canonicalName
		upstream := ""
		if pos >= 0 && pos < len(s.llama) {
			canonical = s.llama[pos].Name
			upstream = s.llama[pos].Upstream
		}
		if stored, ok := s.LlamaBase(canonical); ok {
			return stored
		}
		// Restored route-only rows have no stored base: reapply the
		// explicit trusted policy on the empty base (config facts, not
		// cached observations).
		if s.llamaPolicy != nil && !s.llamaPolicy.Empty() && upstream != "" && s.llamaPolicy.Covers(upstream) {
			base := capability.Empty(effDialect)
			capability.ApplyLlamaLimits(&base, upstream, s.llamaPolicy)
			return base
		}
		return capability.Empty(effDialect)
	}
	return capability.Empty(effDialect)
}

// RequestCapability builds the private canonical capability contract for one
// already-resolved route from the same pinned snapshot: base plus trusted
// operator overrides plus Normalize. Freshness is neutral for planning.
// Raw metadata.router is never read. The result is caller-owned.
func (s *Snapshot) RequestCapability(model *config.Model, provider *config.Provider) capability.Contract {
	if s == nil || model == nil {
		return capability.Empty("")
	}
	eff := model.EffectiveDialect(provider)
	base := s.CanonicalBase(model.Name, eff)
	overrides := s.CapabilityOverrides(model.Name)
	result := base.Clone()
	capability.ApplyOverrides(&result, overrides)
	result.Normalize()
	return result
}
