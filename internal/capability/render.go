package capability

import (
	"encoding/json"
	"sort"
	"strings"
)

// --- typed discovery freshness ---
//
// Discovery is the per-route freshness block. Static routes carry only
// null stale/age (no observation); catalog/llama rows carry the full
// registry-derived block. The wire shape is identical to the previous
// map-based renderer; maps exist only at JSON-serialization time below.

// FreshnessInput is the router-boundary freshness observation: mono times
// in seconds (nil unknown), the error string, and the wall success time.
// It mirrors the registry RefreshReport semantics without importing the
// router package.
type FreshnessInput struct {
	LastSuccess *float64
	LastAttempt *float64
	NextAttempt *float64
	Failures    int
	Err         string
	Cached      bool
	SucceededAt *float64
}

// Discovery is one route's freshness block for the rendered listing.
type Discovery struct {
	full                bool
	stale               *bool
	ageSeconds          *float64
	lastSuccess         *float64
	lastAttempt         *float64
	nextAttempt         *float64
	consecutiveFailures int
	staleAgeSeconds     *float64
	cached              bool
	succeededAt         *float64
}

// StaticDiscovery is the freshness for explicit static routes: no
// observation, so stale and age stay null.
func StaticDiscovery() Discovery {
	return Discovery{}
}

// NewDiscovery derives the catalog/llama freshness block from one registry
// observation plus one mono sample, porting the RefreshReport derivation
// (stale when never succeeded or errored; interval TTL on age).
func NewDiscovery(in FreshnessInput, interval, nowMono float64) Discovery {
	d := Discovery{full: true, consecutiveFailures: in.Failures, cached: in.Cached}
	if in.LastSuccess != nil {
		a := nowMono - *in.LastSuccess
		if a < 0 {
			a = 0
		}
		d.ageSeconds = &a
		d.lastSuccess = copyFloat(in.LastSuccess)
	}
	d.lastAttempt = copyFloat(in.LastAttempt)
	d.nextAttempt = copyFloat(in.NextAttempt)
	stale := in.LastSuccess == nil || in.Err != ""
	if d.ageSeconds != nil {
		if interval > 0 {
			if *d.ageSeconds >= interval {
				stale = true
			}
			sa := *d.ageSeconds - interval
			if sa < 0 {
				sa = 0
			}
			d.staleAgeSeconds = &sa
		} else {
			z := 0.0
			d.staleAgeSeconds = &z
		}
	}
	d.stale = &stale
	d.succeededAt = copyFloat(in.SucceededAt)
	return d
}

func copyFloat(f *float64) *float64 {
	if f == nil {
		return nil
	}
	v := *f
	return &v
}

func (d Discovery) clone() Discovery {
	out := d
	out.stale = copyFloatBool(d.stale)
	out.ageSeconds = copyFloat(d.ageSeconds)
	out.lastSuccess = copyFloat(d.lastSuccess)
	out.lastAttempt = copyFloat(d.lastAttempt)
	out.nextAttempt = copyFloat(d.nextAttempt)
	out.staleAgeSeconds = copyFloat(d.staleAgeSeconds)
	out.succeededAt = copyFloat(d.succeededAt)
	return out
}

// Stale returns a copy of the block's stale flag (nil for static).
func (d Discovery) Stale() *bool { return copyFloatBool(d.stale) }

func copyFloatBool(b *bool) *bool {
	if b == nil {
		return nil
	}
	v := *b
	return &v
}

// --- render ---

// Render ports router_metadata.render: deep-copy the base, apply overrides,
// normalize, attach freshness.discovery, and derive per-source age/stale at
// read time from the typed wall clock. Static/configured assertions carry
// no retrieved_at, so their age/stale stay null. No network or I/O.
func Render(base Contract, overrides Overrides, discovery Discovery, nowWall float64) Contract {
	result := base.Clone()
	if overrides == nil {
		overrides = Overrides{}
	}
	ApplyOverrides(&result, overrides)
	result.Normalize()
	disc := discovery.clone()
	result.freshness = &disc
	discStale := disc.stale
	for _, src := range result.sources {
		if src.retrievedAt == nil {
			src.ageSeconds = nil
			src.stale = nil
			continue
		}
		age := nowWall - *src.retrievedAt
		if age < 0 {
			age = 0
		}
		src.ageSeconds = &age
		if src.staleAfterSeconds != nil {
			b := age >= *src.staleAfterSeconds
			src.stale = &b
		} else if discStale != nil {
			b := *discStale
			src.stale = &b
		} else {
			src.stale = nil
		}
	}
	return result
}

// --- JSON rendering (nested shape mirrors Python) ---
//
// These toWire helpers are the ONLY place map[string]any/slices-of-any may
// appear: the final operational serialization. Semantic storage above never
// uses them.

func setNested(root map[string]any, path string, value any) {
	parts := strings.Split(path, ".")
	cur := root
	for _, p := range parts[:len(parts)-1] {
		next, ok := cur[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[p] = next
		}
		cur = next
	}
	cur[parts[len(parts)-1]] = value
}

func (v Value) wire() any {
	switch v.tag {
	case tagBool:
		return v.b
	case tagInt:
		return v.i
	case tagFloat:
		return v.f
	case tagString:
		return v.s
	case tagStrings:
		return append([]string{}, v.ss...)
	case tagNative:
		m := map[string]any{"protocol": v.np.protocol, "path": v.np.path}
		if v.np.values != nil {
			vals := make(map[string]any, len(v.np.values))
			for k, s := range v.np.values {
				switch s.tag {
				case scalarBool:
					vals[k] = s.b
				case scalarInt:
					vals[k] = s.i
				default:
					vals[k] = s.s
				}
			}
			m["values"] = vals
		}
		return m
	default:
		return nil
	}
}

func (f Fact) wire() map[string]any {
	m := map[string]any{
		"value":  f.value.wire(),
		"source": nil,
		"kind":   f.kind,
	}
	if f.source != nil {
		m["source"] = *f.source
	}
	if f.special != nil {
		m["special"] = *f.special
	}
	if len(f.conflicts) > 0 {
		arr := make([]any, 0, len(f.conflicts))
		for _, e := range f.conflicts {
			em := map[string]any{
				"value":  e.value.wire(),
				"source": nil,
				"kind":   e.kind,
			}
			if e.source != nil {
				em["source"] = *e.source
			}
			arr = append(arr, em)
		}
		m["conflicts"] = arr
	}
	return m
}

func floatOrNil(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}

func (s *Source) wire(includeAge bool) map[string]any {
	m := map[string]any{}
	if s.retrievedAt != nil {
		m["retrieved_at"] = *s.retrievedAt
	}
	if s.staleAfterSeconds != nil {
		m["stale_after_seconds"] = *s.staleAfterSeconds
	}
	if s.verification != nil {
		m["verification"] = *s.verification
	}
	if s.endpoint != nil {
		m["endpoint"] = *s.endpoint
	}
	if s.available != nil {
		m["available"] = *s.available
	}
	if s.applicable != nil {
		m["applicable"] = *s.applicable
	}
	if s.modelSource != nil {
		m["model_source"] = *s.modelSource
	}
	if s.reviewSource != nil {
		m["source"] = *s.reviewSource
	}
	if s.reviewedOn != nil {
		m["reviewed_on"] = *s.reviewedOn
	}
	if s.repoRevision != nil {
		m["repository_revision"] = *s.repoRevision
	}
	if s.snapshotSHA != nil {
		m["snapshot_sha256"] = *s.snapshotSHA
	}
	if s.serverBuild != nil {
		m["server_build"] = *s.serverBuild
	}
	if includeAge {
		m["age_seconds"] = floatOrNil(s.ageSeconds)
		if s.stale != nil {
			m["stale"] = *s.stale
		} else {
			m["stale"] = nil
		}
	}
	return m
}

func (d Discovery) wire() map[string]any {
	if !d.full {
		return map[string]any{"stale": nil, "age_seconds": nil}
	}
	stale := any(nil)
	if d.stale != nil {
		stale = *d.stale
	}
	return map[string]any{
		"last_success":         floatOrNil(d.lastSuccess),
		"last_attempt":         floatOrNil(d.lastAttempt),
		"next_attempt":         floatOrNil(d.nextAttempt),
		"consecutive_failures": d.consecutiveFailures,
		"age_seconds":          floatOrNil(d.ageSeconds),
		"stale":                stale,
		"stale_age_seconds":    floatOrNil(d.staleAgeSeconds),
		"cached":               d.cached,
		"succeeded_at":         floatOrNil(d.succeededAt),
	}
}

// MarshalJSON renders the nested contract shape.
func (c Contract) MarshalJSON() ([]byte, error) {
	root := map[string]any{"schema_version": 1}
	srcMap := make(map[string]any, len(c.sources))
	includeAge := c.freshness != nil
	for name, src := range c.sources {
		srcMap[name] = src.wire(includeAge)
	}
	// llama.props always carries the full key set: retrieved_at and
	// server_build render as explicit null when unobserved, matching the
	// Python source shape. Other sources keep their existing shape.
	if m, ok := srcMap["llama.props"].(map[string]any); ok {
		if _, has := m["retrieved_at"]; !has {
			m["retrieved_at"] = nil
		}
		if _, has := m["server_build"]; !has {
			m["server_build"] = nil
		}
		if _, has := m["endpoint"]; !has {
			m["endpoint"] = nil
		}
		if _, has := m["available"]; !has {
			m["available"] = nil
		}
		if _, has := m["verification"]; !has {
			m["verification"] = nil
		}
	}
	root["sources"] = srcMap
	paths := make([]string, 0, len(c.facts))
	for p := range c.facts {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		setNested(root, p, c.facts[p].wire())
	}
	limMap := make(map[string]any, len(c.limits))
	for k, f := range c.limits {
		limMap[k] = f.wire()
	}
	if c.limitConstraints != nil {
		cons := make(map[string]any, len(c.limitConstraints))
		for k, f := range c.limitConstraints {
			cons[k] = f.wire()
		}
		limMap["constraints"] = cons
	}
	root["limits"] = limMap
	if c.freshness != nil {
		root["freshness"] = map[string]any{"discovery": c.freshness.wire()}
	}
	return json.Marshal(root)
}
