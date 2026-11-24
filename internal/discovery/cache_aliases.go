package discovery

import (
	"strings"

	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/router"
)

// Normalize each server independently, keeping even routes hidden by
// another server or source. All canonical names reserve before aliases.
func (s *CacheStore) normalizedLlamaRows(entries []router.LlamaEntry, srv config.LlamaServer) []CachedRoute {
	prefix, expose := serverPrefixExpose(s.cfg.Llama, srv)
	seen := map[string]bool{}
	var accepted []router.LlamaEntry
	for _, e := range entries {
		name := prefix + mapLocal(e.Upstream)
		if !s.sanitize.safeID(e.Upstream) || e.Name != name || seen[name] {
			continue
		}
		safe := true
		for _, alias := range e.Aliases {
			if !expose || !s.sanitize.safeID(alias) || !strings.HasPrefix(alias, prefix) {
				safe = false
				break
			}
		}
		if !safe {
			continue // an omitted row cannot reserve names or aliases
		}
		seen[name] = true
		accepted = append(accepted, e)
	}
	rows := make([]CachedRoute, 0, len(accepted))
	for _, e := range accepted {
		row := CachedRoute{ID: e.Upstream, Aliases: []string{}}
		for _, alias := range e.Aliases {
			if !seen[alias] {
				seen[alias] = true
				row.Aliases = append(row.Aliases, alias)
			}
		}
		rows = append(rows, row)
	}
	return rows
}
