package server

import (
	"github.com/arhammahajan/model-router/internal/router"
)

// commitCatalogShim migrates the old PublishCatalog call site to a typed
// success attempt.
func commitCatalogShim(reg *router.Registry, entries []router.CatalogEntry, unmapped []string, at float64) error {
	return reg.CommitCatalog(router.CatalogAttempt{
		Event: router.SourceEvent{
			LastSuccess: router.MonoPtr(router.Mono(at)),
			LastAttempt: router.MonoPtr(router.Mono(at)),
		},
		Success: &router.CatalogSuccess{Entries: entries, Unmapped: unmapped, Wall: router.Wall(at)},
	})
}
