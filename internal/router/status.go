package router

// SourceEvent carries stored source facts only, never derived values.
// Times are mono and optional: nil means unknown/never, never a fake
// zero. Loaded/ages/staleness derive at render; the original wall success
// times remain registry-owned (catAt/llamaAt, branded Wall).
type SourceEvent struct {
	LastSuccess *Mono
	LastAttempt *Mono
	NextAttempt *Mono
	Failures    int
	Err         string
	Cached      bool
}

// ServerEvent is one llama server's stored facts. Mode is nil when
// unknown; reachability and staleness derive from the same LlamaLayer
// state at render. URL and provider come from trusted config.
type ServerEvent struct {
	URL    string
	Mode   *string
	Models int
	SourceEvent
}

// CacheEvent is the registry-owned discovery-cache status.
type CacheEvent struct {
	Load     string
	WriteErr *string
	Expired  *int
	Enabled  bool
}

// CatalogSuccess is a successful catalog attempt: entries in fetch order,
// unmapped IDs and the live probe completion wall time. A valid empty
// success is non-nil with no entries; nil means failure.
type CatalogSuccess struct {
	Entries  []CatalogEntry
	Unmapped []string
	Wall     Wall
}

// CatalogAttempt is one catalog completion: the computed status event
// plus the success payload (nil on failure).
type CatalogAttempt struct {
	Event   SourceEvent
	Success *CatalogSuccess
}

// LlamaAttempt is one complete fleet completion: the validated layer,
// the fleet completion wall time, the aggregate status event and the
// per-server facts keyed by trusted provider name.
type LlamaAttempt struct {
	Layer     LlamaLayer
	AtWall    Wall
	Aggregate SourceEvent
	Servers   map[string]ServerEvent
}

// copySourceEvent deep-copies optional mono times.
// MonoPtr boxes a mono timestamp for event construction.
func MonoPtr(m Mono) *Mono {
	v := m
	return &v
}

// WallPtr boxes a wall timestamp for event construction.
func WallPtr(w Wall) *Wall {
	v := w
	return &v
}

func copySourceEvent(e SourceEvent) SourceEvent {
	out := e
	if e.LastSuccess != nil {
		v := *e.LastSuccess
		out.LastSuccess = &v
	}
	if e.LastAttempt != nil {
		v := *e.LastAttempt
		out.LastAttempt = &v
	}
	if e.NextAttempt != nil {
		v := *e.NextAttempt
		out.NextAttempt = &v
	}
	return out
}

// copyServerEvents deep-copies per-server facts.
func copyServerEvents(in map[string]ServerEvent) map[string]ServerEvent {
	if in == nil {
		return map[string]ServerEvent{}
	}
	out := make(map[string]ServerEvent, len(in))
	for k, v := range in {
		cp := copySourceEvent(v.SourceEvent)
		var mode *string
		if v.Mode != nil {
			m := *v.Mode
			mode = &m
		}
		out[k] = ServerEvent{URL: v.URL, Mode: mode, Models: v.Models, SourceEvent: cp}
	}
	return out
}

// copyCacheEvent deep-copies optional cache fields.
func copyCacheEvent(e CacheEvent) CacheEvent {
	out := e
	if e.WriteErr != nil {
		v := *e.WriteErr
		out.WriteErr = &v
	}
	if e.Expired != nil {
		v := *e.Expired
		out.Expired = &v
	}
	return out
}
