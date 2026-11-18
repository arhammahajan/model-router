// Capability listing: immutable snapshot-owned read rendering.
//
// One pinned Snapshot plus one Clock.Sample per report derives every
// freshness value; no network, filesystem, store lock or worker runs on
// this path. Bases are empty(effectiveDialect) for static, catalog,
// llama and restored routes (unknown upstream support preserved);
// trusted config overrides reapply after restore. Raw metadata.router
// is overwritten, never merged; unrelated metadata bytes stay exact.
package router

import (
	"github.com/arhammahajan/model-router/internal/capability"
	"github.com/arhammahajan/model-router/internal/config"
)

// CapabilityOverrides returns copied operator facts from this pinned snapshot.
func (s *Snapshot) CapabilityOverrides(name string) capability.Overrides {
	if o, ok := s.capabilityOverrides[name]; ok {
		return o.Clone()
	}
	return capability.Overrides{}
}

// ListedModel is one canonical listing row with its provider and source.
type ListedModel struct {
	Model    config.Model
	Provider *config.Provider
	Source   string // "static" | "llama" | "catalog"
}

// ListedModels returns deep copies in listing order: enabled explicit
// routes in config order, then llama canonicals, then catalog entries.
// Aliases add no rows. Callers cannot mutate snapshot state.
func (s *Snapshot) ListedModels() []ListedModel {
	var out []ListedModel
	for _, m := range s.models {
		if m.Disabled {
			continue
		}
		p := s.providers[m.Provider]
		if p == nil {
			continue
		}
		out = append(out, ListedModel{Model: copyModel(m), Provider: copyProvider(p), Source: "static"})
	}
	for _, m := range s.llama {
		p := s.providers[m.Provider]
		if p == nil {
			continue
		}
		out = append(out, ListedModel{Model: copyModel(m), Provider: copyProvider(p), Source: "llama"})
	}
	for _, m := range s.catalog {
		p := s.providers[m.Provider]
		if p == nil {
			continue
		}
		out = append(out, ListedModel{Model: copyModel(m), Provider: copyProvider(p), Source: "catalog"})
	}
	if out == nil {
		out = []ListedModel{}
	}
	return out
}

// CatalogDiscovery renders the catalog freshness block for one report.
func (s *Snapshot) CatalogDiscovery(interval float64, now Mono) capability.Discovery {
	var succeededAt *float64
	if s.catOK {
		f := float64(s.catAt)
		succeededAt = &f
	}
	return capability.NewDiscovery(capability.FreshnessInput{
		LastSuccess: monoFloatPtr(s.cat.LastSuccess),
		LastAttempt: monoFloatPtr(s.cat.LastAttempt),
		NextAttempt: monoFloatPtr(s.cat.NextAttempt),
		Failures:    s.cat.Failures,
		Err:         s.cat.Err,
		Cached:      s.cat.Cached,
		SucceededAt: succeededAt,
	}, interval, float64(now))
}

// LlamaDiscovery renders one llama provider's freshness block. It mirrors
// Python's per-route status (mono success + error only) plus cached and
// wall success facts owned by the snapshot.
func (s *Snapshot) LlamaDiscovery(provider string, interval float64, now Mono) capability.Discovery {
	ev, ok := s.llamaServers[provider]
	if !ok {
		return capability.StaticDiscovery()
	}
	var succeededAt *float64
	if at, has := s.llamaAt[provider]; has {
		f := float64(at)
		succeededAt = &f
	}
	return capability.NewDiscovery(capability.FreshnessInput{
		LastSuccess: monoFloatPtr(ev.LastSuccess),
		Err:         ev.Err,
		Cached:      ev.Cached,
		SucceededAt: succeededAt,
	}, interval, float64(now))
}

func monoFloatPtr(m *Mono) *float64 {
	if m == nil {
		return nil
	}
	f := float64(*m)
	return &f
}
