// Discovery persistence: optional route-only disk cache (schema v2).
//
// Go owns schema v2; a Python v1 file loads as incompatible, never parsed.
// Endpoints, keys, headers, metadata and capabilities are never persisted
// and never read. The file carries route IDs and full alias names only;
// providers are re-resolved from trusted config at restore.
package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/router"
)

func (s *CacheStore) buildRestore(inv CachedInventory, nowWall float64) (router.DiscoveryInventory, CacheStatus, int) {
	out := router.DiscoveryInventory{Llama: map[string]router.LlamaInventory{}}
	expired := 0
	if inv.Catalog != nil {
		if nowWall-inv.Catalog.At >= s.maxAge {
			expired++
		} else {
			entries, ok := s.resolveCatalog(inv.Catalog.Routes)
			if !ok {
				return router.DiscoveryInventory{}, CacheStatus{Load: CacheInvalid}, expired
			}
			out.Catalog = &router.CatalogInventory{Entries: entries, SucceededAt: router.Wall(inv.Catalog.At)}
		}
	}
	if s.cfg.Llama != nil {
		for idx, unit := range inv.Llama {
			if idx < 0 || idx >= len(s.cfg.Llama.Servers) {
				return router.DiscoveryInventory{}, CacheStatus{Load: CacheInvalid}, expired
			}
			if nowWall-unit.At >= s.maxAge {
				expired++
				continue
			}
			srv := s.cfg.Llama.Servers[idx]
			entries, ok := s.resolveLlama(unit.Routes, srv)
			if !ok {
				return router.DiscoveryInventory{}, CacheStatus{Load: CacheInvalid}, expired
			}
			out.Llama[srv.Name] = router.LlamaInventory{Entries: entries, SucceededAt: router.Wall(unit.At)}
		}
	} else if len(inv.Llama) > 0 {
		return router.DiscoveryInventory{}, CacheStatus{Load: CacheInvalid}, expired
	}
	return out, CacheStatus{Load: CacheLoaded}, expired
}

func (s *CacheStore) resolveCatalog(routes []CachedRoute) ([]router.CatalogEntry, bool) {
	if s.cfg.Catalog == nil {
		if len(routes) > 0 {
			return nil, false
		}
		return nil, true
	}
	var out []router.CatalogEntry
	for _, r := range routes {
		if !s.sanitize.safeID(r.ID) {
			return nil, false
		}
		for _, a := range r.Aliases {
			if !s.sanitize.safeID(a) {
				return nil, false
			}
		}
		if len(r.Aliases) > 0 {
			return nil, false
		}
		provider := ""
		if p, ok := s.cfg.Catalog.ModelProviders[r.ID]; ok {
			provider = p
		} else {
			for _, rule := range s.cfg.Catalog.Rules {
				if rule.Pattern != nil && rule.Pattern.MatchString(r.ID) {
					provider = rule.Provider
					break
				}
			}
		}
		if provider == "" {
			continue // no trusted mapping: skip, as Python continues
		}
		if _, known := s.cfg.Providers[provider]; !known {
			return nil, false
		}
		out = append(out, router.CatalogEntry{Name: r.ID, Provider: provider, Upstream: r.ID})
	}
	return out, true
}

func (s *CacheStore) resolveLlama(routes []CachedRoute, srv config.LlamaServer) ([]router.LlamaEntry, bool) {
	if s.cfg.Llama == nil {
		return nil, false
	}
	prefix, expose := serverPrefixExpose(s.cfg.Llama, srv)
	seenCanon := map[string]bool{}
	seenAlias := map[string]bool{}
	var out []router.LlamaEntry
	for _, r := range routes {
		name := prefix + mapLocal(r.ID)
		if seenCanon[name] {
			return nil, false
		}
		seenCanon[name] = true
	}
	for _, r := range routes {
		if !s.sanitize.safeID(r.ID) {
			return nil, false
		}
		for _, a := range r.Aliases {
			if !s.sanitize.safeID(a) {
				return nil, false
			}
			if !expose {
				return nil, false
			}
			if !strings.HasPrefix(a, prefix) {
				return nil, false
			}
		}
		if !expose && len(r.Aliases) > 0 {
			return nil, false
		}
		for _, a := range r.Aliases {
			if seenCanon[a] || seenAlias[a] {
				return nil, false
			}
			seenAlias[a] = true
		}
		public := r.ID
		if r.ID == "local" {
			public = "local-llama"
		}
		name := prefix + public
		var aliases []string
		for _, a := range r.Aliases {
			aliases = append(aliases, a)
		}
		out = append(out, router.LlamaEntry{Name: name, Upstream: r.ID, Aliases: aliases})
	}
	return out, true
}

// saveOnce captures the latest export, encodes and atomically replaces.
// No registry or worker lock is held across filesystem I/O.
func (s *CacheStore) saveOnce() {
	if s.cfg.DiscoveryCache == nil {
		return
	}
	inv := s.reg.ExportDiscovery()
	if s.beforeEncode != nil {
		s.beforeEncode()
	}
	payload, ok := s.encode(inv)
	if !ok {
		return
	}
	if s.beforeReplace != nil {
		s.beforeReplace()
	}
	s.atomicReplace(payload)
}

func (s *CacheStore) encode(inv router.DiscoveryInventory) ([]byte, bool) {
	// Normalize each source in order: first canonical wins, then first
	// alias wins; suppressed rows contribute no aliases. Unsafe rows are
	// omitted; never write a file the reader would reject.
	var catUnit map[string]any
	if inv.Catalog != nil {
		seen := map[string]bool{}
		var models []any
		for _, e := range inv.Catalog.Entries {
			id := e.Upstream
			if id == "" {
				id = e.Name
			}
			if !s.sanitize.safeID(id) || seen[id] {
				continue
			}
			seen[id] = true
			models = append(models, map[string]any{"id": id, "aliases": []any{}})
		}
		if models == nil {
			models = []any{}
		}
		catUnit = map[string]any{"succeeded_at": inv.Catalog.SucceededAt, "models": models}
	}
	llama := map[string]any{}
	if s.cfg.Llama != nil {
		for idx, srv := range s.cfg.Llama.Servers {
			unit, ok := inv.Llama[srv.Name]
			if !ok {
				continue
			}
			var models []any
			for _, row := range s.normalizedLlamaRows(unit.Entries, srv) {
				models = append(models, map[string]any{"id": row.ID, "aliases": row.Aliases})
			}
			if models == nil {
				models = []any{}
			}
			llama[itoa(idx)] = map[string]any{"succeeded_at": unit.SucceededAt, "models": models}
		}
	}
	doc := map[string]any{
		"schema_version": CacheVersion,
		"identity":       s.identity,
		"catalog":        catUnitOrNull(catUnit),
		"llama":          llama,
	}
	raw, err := json.Marshal(doc)
	if err != nil || len(raw) > CacheMaxBytes {
		s.reg.NoteCacheWriteErr(writeErrMsg())
		log.Printf("discovery cache: write failed")
		return nil, false
	}
	// Every written file must load: self-validate before replacing.
	if _, _, err := validateEncoded(raw, s.maxAge, wallNow(), s.sanitize, s.cfg); err != nil {
		s.reg.NoteCacheWriteErr(writeErrMsg())
		log.Printf("discovery cache: write failed")
		return nil, false
	}
	return raw, true
}

func mapLocal(id string) string {
	if id == "local" {
		return "local-llama"
	}
	return id
}

func catUnitOrNull(u map[string]any) any {
	if u == nil {
		return nil
	}
	return u
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}

func (s *CacheStore) atomicReplace(payload []byte) {
	expanded := s.path
	dir := filepath.Dir(expanded)
	if err := s.mkdirAll(dir, 0o700); err != nil {
		s.reg.NoteCacheWriteErr(writeErrMsg())
		log.Printf("discovery cache: write failed")
		return
	}
	tmp, err := s.createTemp(dir, "."+filepath.Base(expanded)+".")
	if err != nil {
		s.reg.NoteCacheWriteErr(writeErrMsg())
		log.Printf("discovery cache: write failed")
		return
	}
	tmpName := tmp.Name()
	failed := true
	defer func() {
		_ = tmp.Close()
		if failed {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(payload); err != nil {
		s.reg.NoteCacheWriteErr(writeErrMsg())
		log.Printf("discovery cache: write failed")
		return
	}
	if err := tmp.Sync(); err != nil {
		s.reg.NoteCacheWriteErr(writeErrMsg())
		log.Printf("discovery cache: write failed")
		return
	}
	if err := tmp.Close(); err != nil {
		s.reg.NoteCacheWriteErr(writeErrMsg())
		log.Printf("discovery cache: write failed")
		return
	}
	if err := s.renameFn(tmpName, expanded); err != nil {
		s.reg.NoteCacheWriteErr(writeErrMsg())
		log.Printf("discovery cache: write failed")
		return
	}
	failed = false
	_ = s.fsyncDirFn(dir)
	s.reg.NoteCacheWriteErr(nil)
}

func writeErrMsg() *string {
	s := "cache write failed"
	return &s
}

// Close rejects new notifications and performs at most one pending flush
// within ctx. It may return without joining a stalled regular-file
// syscall; the file stays atomic old-or-new.
func (s *CacheStore) Close(ctx context.Context) {
	if s.cfg.DiscoveryCache == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	first := false
	s.closeOnce.Do(func() {
		first = true
		s.closed.Store(true)
		s.closeCtx = ctx
		s.startWriter() // CLI may only have queued work, without Start.
		close(s.stopCh)
	})
	if !first {
		return // deferred cleanup must not start another post-drain wait
	}
	select {
	case <-s.done:
	case <-ctx.Done():
	}
}

func wallNow() float64 {
	return float64(time.Now().UnixNano()) / 1e9
}

// validateEncoded self-checks an encoded payload before replace: it must
// decode under the same file rules and pass secret-corpus and prefix
// admission, so every written file loads.
func validateEncoded(raw []byte, maxAge, nowWall float64, san *cacheSanitizer, cfg *config.Config) (CachedInventory, CacheStatus, error) {
	if len(raw) > CacheMaxBytes {
		return CachedInventory{}, CacheStatus{Load: CacheInvalid}, fmt.Errorf("too large")
	}
	m, err := decodeJSONObject(raw)
	if err != nil {
		return CachedInventory{}, CacheStatus{Load: CacheInvalid}, err
	}
	if !isClosedTop(m) || !isCacheVersion(m) {
		return CachedInventory{}, CacheStatus{Load: CacheIncompatible}, fmt.Errorf("version")
	}
	ident, _ := m["identity"].(string)
	if ident == "" {
		return CachedInventory{}, CacheStatus{Load: CacheInvalid}, fmt.Errorf("identity")
	}
	catRaw := m["catalog"]
	var cat *CachedUnit
	if catRaw != nil {
		u, _, err := decodeUnit(catRaw, true)
		if err != nil || u == nil {
			return CachedInventory{}, CacheStatus{Load: CacheInvalid}, fmt.Errorf("catalog")
		}
		for _, r := range u.Routes {
			if !san.safeID(r.ID) {
				return CachedInventory{}, CacheStatus{Load: CacheInvalid}, fmt.Errorf("id")
			}
			if len(r.Aliases) > 0 {
				return CachedInventory{}, CacheStatus{Load: CacheInvalid}, fmt.Errorf("alias")
			}
		}
		cat = u
		_ = cat
	}
	llamaRaw, ok := m["llama"].(map[string]any)
	if !ok {
		return CachedInventory{}, CacheStatus{Load: CacheInvalid}, fmt.Errorf("llama")
	}
	for key, unitRaw := range llamaRaw {
		idx, ok := parseServerIndex(key)
		if !ok {
			return CachedInventory{}, CacheStatus{Load: CacheInvalid}, fmt.Errorf("index")
		}
		if cfg.Llama == nil || idx < 0 || idx >= len(cfg.Llama.Servers) {
			return CachedInventory{}, CacheStatus{Load: CacheInvalid}, fmt.Errorf("index")
		}
		prefix, expose := serverPrefixExpose(cfg.Llama, cfg.Llama.Servers[idx])
		u, _, err := decodeUnit(unitRaw, false)
		if err != nil || u == nil {
			return CachedInventory{}, CacheStatus{Load: CacheInvalid}, fmt.Errorf("unit")
		}
		for _, r := range u.Routes {
			if !san.safeID(r.ID) {
				return CachedInventory{}, CacheStatus{Load: CacheInvalid}, fmt.Errorf("id")
			}
			for _, a := range r.Aliases {
				if !san.safeID(a) {
					return CachedInventory{}, CacheStatus{Load: CacheInvalid}, fmt.Errorf("alias")
				}
				if !expose {
					return CachedInventory{}, CacheStatus{Load: CacheInvalid}, fmt.Errorf("alias")
				}
				if !strings.HasPrefix(a, prefix) {
					return CachedInventory{}, CacheStatus{Load: CacheInvalid}, fmt.Errorf("prefix")
				}
			}
			if !expose && len(r.Aliases) > 0 {
				return CachedInventory{}, CacheStatus{Load: CacheInvalid}, fmt.Errorf("alias")
			}
		}
	}
	return CachedInventory{}, CacheStatus{Load: CacheLoaded}, nil
}
