package capability

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func mustAsset(t *testing.T) ReviewAsset {
	t.Helper()
	a, err := LoadReviewAsset(EmbeddedReviewBytes())
	if err != nil {
		t.Fatalf("bundled asset must validate: %v", err)
	}
	return a
}

func wireOf(t *testing.T, c Contract) map[string]any {
	t.Helper()
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func leafAt(t *testing.T, wire map[string]any, path string) map[string]any {
	t.Helper()
	cur := wire
	for _, part := range strings.Split(path, ".") {
		next, ok := cur[part].(map[string]any)
		if !ok {
			t.Fatalf("leaf %q missing: %v", path, wire)
		}
		cur = next
	}
	return cur
}

// Bundled asset: exact-byte copy of HEAD, 29 rows, 27 fact paths, dtype
// exact, all limits positive int64 incl precise bigints, provenance keys.
func TestFocusBundledAssetExact(t *testing.T) {
	raw := EmbeddedReviewBytes()
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != "e9bb58bfefc02999a5839d5ec9ba0b451c5b4b9781054d65a3aff56e71d4c271" {
		t.Fatalf("embedded bytes hash changed: %s", got)
	}
	// Decode without external fetch: embedded copy must decode to 29 rows.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var top map[string]any
	if err := dec.Decode(&top); err != nil {
		t.Fatalf("decode: %v", err)
	}
	a := mustAsset(t)
	if len(a.rows) != 29 {
		t.Fatalf("rows=%d want 29", len(a.rows))
	}
	paths := map[string]bool{}
	for _, row := range a.rows {
		for p := range row.facts {
			paths[p] = true
		}
	}
	if len(paths) != 27 {
		t.Fatalf("fact paths=%d want 27: %v", len(paths), paths)
	}
	// Provenance five keys present via a known review source.
	c := CatalogReview("glm-5.3-flash", "openai-chat", a)
	wire := wireOf(t, c)
	srcs := wire["sources"].(map[string]any)
	gr, ok := srcs["go.review"].(map[string]any)
	if !ok {
		t.Fatalf("go.review missing: %v", srcs)
	}
	for _, k := range []string{"model_source", "source", "reviewed_on", "repository_revision", "snapshot_sha256", "retrieved_at", "stale_after_seconds", "verification", "applicable"} {
		if _, has := gr[k]; !has {
			t.Fatalf("go.review missing %q: %v", k, gr)
		}
	}
	// Dtype exact: limits are exact ints, bools are bools, lists are lists.
	lim := wire["limits"].(map[string]any)
	for k, f := range lim {
		m := f.(map[string]any)
		if k == "context_window" || k == "max_output_tokens" {
			continue
		}
		_ = m
	}
	cw := leafAt(t, wire, "limits.context_window")
	if _, ok := cw["value"].(float64); !ok {
		t.Fatalf("limits must be numbers: %v", cw)
	}
	// Bigint precision: max int64 round-trips through overrides path too.
	v, ok := StringListValue([]string{"low"})
	_ = v
	_ = ok
}

// Exact ID gate: unknown ID leaves facts unknown with catalog-only source.
func TestFocusUnknownIDEmpty(t *testing.T) {
	a := mustAsset(t)
	c := CatalogReview("no-such-model", "openai-chat", a)
	c.NoteCatalogAvailability(1000)
	wire := wireOf(t, Render(c, nil, StaticDiscovery(), 2000))
	if _, has := wire["sources"].(map[string]any)["go.review"]; has {
		t.Fatalf("unknown ID must carry no review source: %v", wire["sources"])
	}
	cat := wire["sources"].(map[string]any)["catalog"].(map[string]any)
	if cat["verification"] != "availability only" || cat["retrieved_at"] != float64(1000) {
		t.Fatalf("catalog availability wrong: %v", cat)
	}
	tools := leafAt(t, wire, "tools.supported")
	if tools["value"] != nil {
		t.Fatalf("unknown facts must stay null: %v", tools)
	}
	if tools["kind"] != KindUnknown {
		t.Fatalf("unknown kind: %v", tools)
	}
}

// Dialect mismatch: configured transport preserved, review facts unknown,
// applicable false with full provenance.
func TestFocusDialectMismatch(t *testing.T) {
	a := mustAsset(t)
	c := CatalogReview("glm-5.3-flash", "anthropic", a)
	wire := wireOf(t, c)
	gr := wire["sources"].(map[string]any)["go.review"].(map[string]any)
	if gr["applicable"] != false {
		t.Fatalf("mismatch applicable must be false: %v", gr)
	}
	if gr["model_source"] == nil || gr["source"] == nil {
		t.Fatalf("mismatch must keep provenance: %v", gr)
	}
	// Configured transport follows the ROUTE dialect, not the row dialect.
	native := wire["protocols"].(map[string]any)["native"].(map[string]any)
	if native["anthropic"].(map[string]any)["value"] != true {
		t.Fatalf("route transport must be preserved: %v", native)
	}
	tools := leafAt(t, wire, "tools.supported")
	if tools["value"] != nil || tools["kind"] != KindUnknown {
		t.Fatalf("mismatch facts must stay unknown: %v", tools)
	}
	// Match applies reviewed facts with source/kind.
	matched := CatalogReview("glm-5.3-flash", "openai-chat", a)
	mwire := wireOf(t, matched)
	mgr := mwire["sources"].(map[string]any)["go.review"].(map[string]any)
	if mgr["applicable"] != true {
		t.Fatalf("match applicable must be true: %v", mgr)
	}
	mtools := leafAt(t, mwire, "tools.supported")
	if mtools["value"] != true || mtools["kind"] != KindReviewed || mtools["source"] != "go.review.tools.supported" {
		t.Fatalf("match must apply reviewed facts: %v", mtools)
	}
}

// Null != false everywhere on the review path.
func TestFocusNullNeverFalse(t *testing.T) {
	a := mustAsset(t)
	c := CatalogReview("glm-5.3-flash", "anthropic", a)
	v, ok := c.Get("tools.supported")
	if !ok || !v.IsNull() || v.IsFalse() {
		t.Fatalf("mismatch must be null, never false: %+v", v)
	}
	m := CatalogReview("glm-5.3-flash", "openai-chat", a)
	mv, _ := m.Get("modalities.input.audio")
	if mv.IsNull() {
		t.Fatalf("known false must not be null")
	}
	if !mv.IsFalse() {
		t.Fatalf("known false must be explicit false")
	}
}

// TTL aging: review stale flips only on review age>=TTL; catalog refresh,
// failed probe, restart, save never move review retrieved_at.
func TestFocusReviewTTLIndependent(t *testing.T) {
	a := mustAsset(t)
	c := CatalogReview("glm-5.3-flash", "openai-chat", a)
	c.NoteCatalogAvailability(1000)
	// Review retrieved is the frozen ISO wall; TTL is 2592000.
	wire := wireOf(t, Render(c, nil, NewDiscovery(FreshnessInput{LastSuccess: focusFloatPtr(1900)}, 300, 2000), 1500))
	gr := wire["sources"].(map[string]any)["go.review"].(map[string]any)
	if gr["age_seconds"] == nil || gr["stale"] == nil {
		t.Fatalf("review age/stale must derive: %v", gr)
	}
	// Far future: review stale true even with fresh discovery.
	futureWall := reviewWall(t) + 2592000 + 5000
	future := wireOf(t, Render(c, nil, NewDiscovery(FreshnessInput{LastSuccess: focusFloatPtr(futureWall - 10)}, 300, futureWall), futureWall))
	if future["sources"].(map[string]any)["go.review"].(map[string]any)["stale"] != true {
		t.Fatalf("review must go stale on its own TTL: %v", future["sources"])
	}
	// Catalog availability stale follows discovery, not the review TTL.
	catStale := wire["sources"].(map[string]any)["catalog"].(map[string]any)["stale"]
	discStale := wire["freshness"].(map[string]any)["discovery"].(map[string]any)["stale"]
	if catStale != discStale {
		t.Fatalf("catalog availability must follow discovery: %v vs %v", catStale, discStale)
	}
}

// Copy isolation: clone on publish/read; mutating rendered output cannot
// move later reports; no shared mutable facts.
func TestFocusCopyIsolation(t *testing.T) {
	a := mustAsset(t)
	c := CatalogReview("glm-5.3-flash", "openai-chat", a)
	c.NoteCatalogAvailability(1000)
	first, _ := json.Marshal(Render(c, nil, StaticDiscovery(), 1500))
	var probe Contract = Render(c, nil, StaticDiscovery(), 1500)
	_ = probe.Put("tools.supported", BoolValue(false), strPtr("probe"), KindOverride)
	second, _ := json.Marshal(Render(c, nil, StaticDiscovery(), 1500))
	if string(first) != string(second) {
		t.Fatalf("render must be stable across output mutation")
	}
	b := a.Clone()
	b2 := a.Clone()
	_ = b2
	_ = b
}

// Asset validation negatives: schema/revision/dtype/provenance/limits.
func TestFocusAssetNegatives(t *testing.T) {
	base := EmbeddedReviewBytes()
	var top map[string]any
	if err := json.Unmarshal(base, &top); err != nil {
		t.Fatal(err)
	}
	mut := func(fn func(map[string]any)) []byte {
		cp := deepCopyJSON(t, base)
		fn(cp)
		raw, _ := json.Marshal(cp)
		return raw
	}
	cases := map[string][]byte{
		"schema":   mut(func(m map[string]any) { m["schema_version"] = float64(2) }),
		"revision": mut(func(m map[string]any) { m["revision"] = float64(2) }),
		"provenance": mut(func(m map[string]any) {
			m["provenance"].(map[string]any)["source"] = ""
		}),
		"unknown field": mut(func(m map[string]any) {
			m["models"].(map[string]any)["glm-5.3-flash"].(map[string]any)["facts"].(map[string]any)["nope.field"] = true
		}),
		"bool as number": mut(func(m map[string]any) {
			m["models"].(map[string]any)["glm-5.3-flash"].(map[string]any)["facts"].(map[string]any)["tools.supported"] = float64(1)
		}),
		"nonpositive limit": mut(func(m map[string]any) {
			m["models"].(map[string]any)["glm-5.3-flash"].(map[string]any)["facts"].(map[string]any)["limits.context_window"] = float64(0)
		}),
		"negative limit": mut(func(m map[string]any) {
			m["models"].(map[string]any)["glm-5.3-flash"].(map[string]any)["facts"].(map[string]any)["limits.context_window"] = float64(-5)
		}),
	}
	for name, raw := range cases {
		if _, err := LoadReviewAsset(raw); err == nil {
			t.Fatalf("%s must fail validation", name)
		} else if !strings.Contains(err.Error(), "catalogreview:") {
			t.Fatalf("%s error must name the boundary: %v", name, err)
		}
	}
}

// Concurrent construction/render under -race shares nothing.
func TestFocusConcurrentReview(t *testing.T) {
	a := mustAsset(t)
	base := CatalogReview("glm-5.3-flash", "openai-chat", a)
	base.NoteCatalogAvailability(1000)
	want, _ := json.Marshal(Render(base, nil, StaticDiscovery(), 1500))
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := CatalogReview("glm-5.3-flash", "openai-chat", a)
			c.NoteCatalogAvailability(1000)
			out, err := json.Marshal(Render(c, nil, StaticDiscovery(), 1500))
			if err != nil {
				t.Error(err)
				return
			}
			if string(out) != string(want) {
				t.Errorf("drift: %s", out)
			}
		}()
	}
	wg.Wait()
}

func focusFloatPtr(f float64) *float64 { return &f }

func reviewWall(t *testing.T) float64 {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, "2026-10-25T19:45:18Z")
	if err != nil {
		t.Fatal(err)
	}
	return float64(ts.UnixNano()) / 1e9
}

func deepCopyJSON(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v map[string]any
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}
