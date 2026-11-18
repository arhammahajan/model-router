package router

// Test shims migrating old Publish* call sites to typed attempts.
// Production legacy Publish* methods are removed; these helpers build
// success attempts so pre-existing route/time assertions keep their
// meaning. New tests use CommitCatalog/CommitLlama directly.
func commitCatalogShim(reg *Registry, entries []CatalogEntry, unmapped []string, at float64) error {
	return reg.CommitCatalog(CatalogAttempt{
		Event: SourceEvent{
			LastSuccess: MonoPtr(Mono(at)),
			LastAttempt: MonoPtr(Mono(at)),
		},
		Success: &CatalogSuccess{Entries: entries, Unmapped: unmapped, Wall: Wall(at)},
	})
}

func commitLlamaShim(reg *Registry, layer LlamaLayer, at float64) error {
	agg := SourceEvent{LastAttempt: MonoPtr(Mono(at))}
	servers := make(map[string]ServerEvent, len(layer.Servers))
	for name, snap := range layer.Servers {
		switch snap.State {
		case LlamaReachable:
			agg.LastSuccess = MonoPtr(Mono(at))
			servers[name] = ServerEvent{
				Models:      len(snap.Entries),
				SourceEvent: SourceEvent{LastSuccess: MonoPtr(Mono(at)), LastAttempt: MonoPtr(Mono(at))},
			}
		case LlamaUnreachableStale:
			servers[name] = ServerEvent{
				Models:      len(snap.Entries),
				SourceEvent: SourceEvent{LastAttempt: MonoPtr(Mono(at)), Failures: 1},
			}
		default:
			servers[name] = ServerEvent{SourceEvent: SourceEvent{LastAttempt: MonoPtr(Mono(at))}}
		}
	}
	for name, ev := range servers {
		ev.URL = shimURL(reg, name)
		servers[name] = ev
	}
	return reg.CommitLlama(LlamaAttempt{Layer: layer, AtWall: Wall(at), Aggregate: agg, Servers: servers})
}

func shimURL(reg *Registry, name string) string {
	if p := reg.trustedLlama[name]; p != nil {
		return p.BaseURL
	}
	return ""
}
