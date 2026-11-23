package discovery

import (
	"reflect"
	"testing"

	"github.com/arhammahajan/router/internal/router"
)

// invCapture pins the staged inventory for unchanged-proof migration:
// status-only publications may swap the pointer while routes, times and
// facts stay identical.
type invCapture struct {
	routes string
	inv    router.DiscoveryInventory
}

func captureInv(reg *router.Registry) invCapture {
	return invCapture{routes: exportRoutes(reg.Current()), inv: reg.ExportDiscovery()}
}

func (c invCapture) assertUnchanged(t *testing.T, reg *router.Registry) {
	t.Helper()
	if got := exportRoutes(reg.Current()); got != c.routes {
		t.Fatalf("inventory routes changed: %q vs %q", got, c.routes)
	}
	if got := reg.ExportDiscovery(); !reflect.DeepEqual(got, c.inv) {
		t.Fatalf("staged inventory changed: %+v vs %+v", got, c.inv)
	}
}

// Test shims migrating old Publish* call sites to typed attempts.
func commitCatalogShim(reg *router.Registry, entries []router.CatalogEntry, unmapped []string, at float64) error {
	return reg.CommitCatalog(router.CatalogAttempt{
		Event: router.SourceEvent{
			LastSuccess: router.MonoPtr(router.Mono(at)),
			LastAttempt: router.MonoPtr(router.Mono(at)),
		},
		Success: &router.CatalogSuccess{Entries: entries, Unmapped: unmapped, Wall: router.Wall(at)},
	})
}

func commitLlamaShim(reg *router.Registry, layer router.LlamaLayer, at float64) error {
	agg := router.SourceEvent{LastAttempt: router.MonoPtr(router.Mono(at))}
	servers := make(map[string]router.ServerEvent, len(layer.Servers))
	for name, snap := range layer.Servers {
		switch snap.State {
		case router.LlamaReachable:
			agg.LastSuccess = router.MonoPtr(router.Mono(at))
			servers[name] = router.ServerEvent{
				Models: len(snap.Entries),
				SourceEvent: router.SourceEvent{
					LastSuccess: router.MonoPtr(router.Mono(at)),
					LastAttempt: router.MonoPtr(router.Mono(at)),
				},
			}
		case router.LlamaUnreachableStale:
			servers[name] = router.ServerEvent{
				Models: len(snap.Entries),
				SourceEvent: router.SourceEvent{
					LastAttempt: router.MonoPtr(router.Mono(at)),
					Failures:    1,
				},
			}
		default:
			servers[name] = router.ServerEvent{
				SourceEvent: router.SourceEvent{LastAttempt: router.MonoPtr(router.Mono(at))},
			}
		}
	}
	return reg.CommitLlama(router.LlamaAttempt{Layer: layer, AtWall: router.Wall(at), Aggregate: agg, Servers: servers})
}
