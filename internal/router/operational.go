package router

import (
	"math"
	"sort"
)

// Operational renderers derive ages and staleness at render from one
// pinned snapshot plus one sampled Mono. Frozen snapshots carry
// events/times only; no worker/store lock or I/O happens here.
//
// Shapes mirror Python discovery.readiness_payload, discovery_summary,
// catalog_status, llama_cpp_status and diagnostics_payload key-for-key:
// readiness {status,ready,enabled_routes,usable_routes,reason};
// per-source {stale,age_seconds,stale_age_seconds,consecutive_failures}.

// RefreshReport is the derived per-source freshness block.
func RefreshReport(ev SourceEvent, interval float64, now Mono) map[string]any {
	var age *float64
	if ev.LastSuccess != nil {
		a := math.Max(0, float64(now-*ev.LastSuccess))
		age = &a
	}
	stale := ev.LastSuccess == nil || ev.Err != ""
	var staleAge *float64
	if age != nil {
		if interval > 0 {
			if *age >= interval {
				stale = true
			}
			sa := math.Max(0, *age-interval)
			staleAge = &sa
		} else {
			z := 0.0
			staleAge = &z
		}
	}
	return map[string]any{
		"last_success":         monoFloat(ev.LastSuccess),
		"last_attempt":         monoFloat(ev.LastAttempt),
		"next_attempt":         monoFloat(ev.NextAttempt),
		"consecutive_failures": ev.Failures,
		"age_seconds":          floatOrNil(age),
		"stale":                stale,
		"stale_age_seconds":    floatOrNil(staleAge),
	}
}

func monoFloat(m *Mono) any {
	if m == nil {
		return nil
	}
	f := float64(*m)
	return f
}

func floatOrNil(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}

func wallFloat(w *Wall) any {
	if w == nil {
		return nil
	}
	return float64(*w)
}

func errOrNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// providerEnabledRoutes counts effective named keys per provider including
// aliases: enabled explicit canonicals+aliases, all llama canonicals+aliases
// (stale included), and catalog entries.
func (s *Snapshot) providerEnabledRoutes() map[string]int {
	out := map[string]int{}
	for i := range s.models {
		m := &s.models[i]
		if m.Disabled {
			continue
		}
		out[m.Provider] += 1 + len(m.Aliases)
	}
	for i := range s.llama {
		out[s.llama[i].Provider]++
	}
	for name, n := range s.llamaAliases {
		out[name] += n
	}
	for i := range s.catalog {
		out[s.catalog[i].Provider]++
	}
	return out
}

// EnabledRoutes is the effective named map size including aliases.
func (s *Snapshot) EnabledRoutes() int {
	n := 0
	for i := range s.models {
		if !s.models[i].Disabled {
			n++
		}
	}
	for _, c := range s.explicitAliases {
		n += c
	}
	n += len(s.llama)
	for _, c := range s.llamaAliases {
		n += c
	}
	return n + len(s.catalog)
}

// UsableRoutes excludes keys whose provider is unreachable.
func (s *Snapshot) UsableRoutes() int {
	per := s.providerEnabledRoutes()
	n := 0
	for name, c := range per {
		if _, bad := s.unreachable[name]; !bad {
			n += c
		}
	}
	return n
}

// Readiness renders {status,ready,enabled_routes,usable_routes,reason}.
func (s *Snapshot) Readiness() map[string]any {
	enabled := s.EnabledRoutes()
	usable := s.UsableRoutes()
	status := "not_ready"
	reason := "no_usable_named_routes"
	ready := false
	if usable > 0 {
		status = "ready"
		reason = "usable_routes"
		ready = true
	}
	return map[string]any{
		"status":         status,
		"ready":          ready,
		"enabled_routes": enabled,
		"usable_routes":  usable,
		"reason":         reason,
	}
}

// DiscoverySummary renders per-configured-source freshness.
func (s *Snapshot) DiscoverySummary(catEnabled, llamaEnabled bool, catInterval, llamaInterval float64, now Mono) map[string]any {
	out := map[string]any{}
	if catEnabled {
		r := RefreshReport(s.cat, catInterval, now)
		out["catalog"] = map[string]any{
			"stale":                r["stale"],
			"age_seconds":          r["age_seconds"],
			"stale_age_seconds":    r["stale_age_seconds"],
			"consecutive_failures": r["consecutive_failures"],
		}
	}
	if llamaEnabled {
		r := RefreshReport(s.llamaAgg, llamaInterval, now)
		out["llama_cpp"] = map[string]any{
			"stale":                r["stale"],
			"age_seconds":          r["age_seconds"],
			"stale_age_seconds":    r["stale_age_seconds"],
			"consecutive_failures": r["consecutive_failures"],
		}
	}
	return out
}

// CatalogStatus renders the catalog block. sanitize applies to the URL;
// unmapped excludes names shadowed by explicit/llama routes.
func (s *Snapshot) CatalogStatus(enabled bool, url string, sanitize func(string) string, interval float64, now Mono) map[string]any {
	var succeededAt any
	if s.catOK {
		succeededAt = float64(s.catAt)
	}
	return map[string]any{
		"enabled":         enabled,
		"url":             urlOrNil(enabled, url, sanitize),
		"models":          s.catalogModels,
		"loaded":          s.cat.LastSuccess != nil,
		"error":           errOrNil(s.cat.Err),
		"cached":          s.cat.Cached,
		"succeeded_at":    succeededAt,
		"refresh":         RefreshReport(s.cat, interval, now),
		"unmapped_models": s.unmappedModels(),
	}
}

// LlamaStatus renders the llama block. order fixes server walk order;
// sanitize applies to server URLs.
func (s *Snapshot) LlamaStatus(enabled bool, order []string, interval float64, now Mono, sanitize func(string) string) map[string]any {
	servers := make([]any, 0, len(s.llamaServers))
	names := order
	if names == nil {
		for name := range s.llamaServers {
			names = append(names, name)
		}
		sort.Strings(names)
	} else {
		seen := map[string]bool{}
		var filt []string
		for _, name := range names {
			if seen[name] {
				continue
			}
			seen[name] = true
			if _, ok := s.llamaServers[name]; ok {
				filt = append(filt, name)
			}
		}
		// Include events for providers outside the walk order (defensive).
		var extra []string
		for name := range s.llamaServers {
			if !seen[name] {
				extra = append(extra, name)
			}
		}
		sort.Strings(extra)
		names = append(filt, extra...)
	}
	for _, name := range names {
		ev := s.llamaServers[name]
		_, unreachable := s.unreachable[name]
		_, active := s.llamaProviders[name]
		reachable := active && !unreachable
		var age, staleAge *float64
		if ev.LastSuccess != nil {
			a := math.Max(0, float64(now-*ev.LastSuccess))
			age = &a
			if interval > 0 {
				sa := math.Max(0, a-interval)
				staleAge = &sa
			} else {
				z := 0.0
				staleAge = &z
			}
		}
		u := ev.URL
		if sanitize != nil {
			u = sanitize(u)
		}
		var succeededAt any
		if at, ok := s.llamaAt[name]; ok {
			succeededAt = float64(at)
		}
		servers = append(servers, map[string]any{
			"url":                  u,
			"reachable":            reachable,
			"mode":                 modeOrNil(ev.Mode),
			"provider":             name,
			"models":               ev.Models,
			"stale":                unreachable,
			"error":                errOrNil(ev.Err),
			"last_success":         monoFloat(ev.LastSuccess),
			"last_attempt":         monoFloat(ev.LastAttempt),
			"consecutive_failures": ev.Failures,
			"cached":               ev.Cached,
			"succeeded_at":         succeededAt,
			"age_seconds":          floatOrNil(age),
			"stale_age_seconds":    floatOrNil(staleAge),
		})
	}
	loaded := len(s.llamaProviders) > 0 || s.llamaAgg.LastSuccess != nil
	return map[string]any{
		"enabled": enabled,
		"servers": servers,
		"models":  len(s.llama),
		"loaded":  loaded,
		"error":   errOrNil(s.llamaAgg.Err),
		"refresh": RefreshReport(s.llamaAgg, interval, now),
	}
}

func modeOrNil(m *string) any {
	if m == nil {
		return nil
	}
	return *m
}

func urlOrNil(enabled bool, url string, sanitize func(string) string) any {
	if !enabled {
		return nil
	}
	if sanitize != nil {
		return sanitize(url)
	}
	return url
}

func (s *Snapshot) unmappedModels() []string {
	out := []string{}
	for id := range s.unmapped {
		if _, reserved := s.index[id]; reserved {
			continue
		}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
