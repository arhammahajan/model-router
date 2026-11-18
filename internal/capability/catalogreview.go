// Boundary sketch (catalog capability vertical only):
//
//	Owns: the exact-byte bundled review asset (go:embed sibling JSON,
//	validated once per config construction), the closed typed ReviewRow/
//	ReviewAsset domain, and the pure per-route constructor CatalogReview
//	plus the catalog-availability stamper NoteCatalogAvailability. Inputs:
//	raw JSON bytes at the asset boundary (validated with closed dtypes),
//	an exact model ID plus an effective route dialect at publication time.
//	Outputs: a private Contract per catalog route (Empty base plus reviewed
//	facts with kind reviewed/source go.review.<path> plus the go.review
//	provenance source; unknown IDs have no review source and mismatched
//	dialects retain provenance with applicable=false).
//	No network, filesystem, clock, registry, lock, worker or snapshot
//	access here; callers pin one Snapshot plus one Clock.Sample at render.
//	Does NOT own: admission (config), publication/restoration (registry/
//	snapshot), listing (server), cache identity (discovery), Generation/
//	transport/session/persistence, effort admission, llama acquisition.
//	Closed-domain rules: semantic storage never holds map[string]any or
//	reflection. Raw JSON exists ONLY inside LoadReviewAsset's boundary
//	decode (json.Number preserves int-vs-float); stored facts are closed
//	Values, row maps are private and cloned on copy.
//
// Contract: exactID then row-dialect gate; unknown empty, mismatch keeps
// configured transport with applicable false, match applies reviewed facts.
package capability

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"time"
)

//go:embed opencode-go-v1.json
var embeddedReviewJSON []byte

// EmbeddedReviewBytes returns a copy of the exact bundled asset bytes.
// Callers cannot mutate the embedded image through the result.
func EmbeddedReviewBytes() []byte {
	return append([]byte(nil), embeddedReviewJSON...)
}

// ReviewProvenance is the bundled asset provenance (all strings).
type ReviewProvenance struct {
	Source       string
	ReviewedOn   string
	RepoRevision string
	SnapshotSHA  string
	Verification string
}

// ReviewRow is one validated asset row: exact source ID provenance,
// the reviewed native dialect, and closed typed facts (limits.* included).
type ReviewRow struct {
	source  string
	dialect string
	facts   map[string]Value
}

// ReviewAsset is the validated immutable bundled review: parsed wall
// retrieved time, TTL seconds, typed provenance, exact-ID row map.
type ReviewAsset struct {
	retrievedWall float64
	staleAfter    float64
	provenance    ReviewProvenance
	rows          map[string]ReviewRow
}

// Clone deep-copies the asset (registry pins a seed copy; config mutation
// after construction cannot reach it).
func (a ReviewAsset) Clone() ReviewAsset {
	out := ReviewAsset{
		retrievedWall: a.retrievedWall,
		staleAfter:    a.staleAfter,
		provenance:    a.provenance,
		rows:          make(map[string]ReviewRow, len(a.rows)),
	}
	for id, row := range a.rows {
		cp := ReviewRow{source: row.source, dialect: row.dialect, facts: make(map[string]Value, len(row.facts))}
		for p, v := range row.facts {
			cp.facts[p] = v.clone()
		}
		out.rows[id] = cp
	}
	return out
}

// LoadReviewAsset validates one bundled asset image exactly once per config
// construction (offline, pure, no I/O). Schema/revision/dtype/provenance or
// non-positive limits fail closed with static diagnostics (value-free,
// naming key paths only). Limit rejection is deliberate hardening for
// bundled assets, unlike Python's skip; the frozen asset has no such rows.
func LoadReviewAsset(raw []byte) (ReviewAsset, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var top any
	if err := dec.Decode(&top); err != nil {
		return ReviewAsset{}, fmt.Errorf("catalogreview: invalid bundled review asset")
	}
	var extra any
	if err := dec.Decode(&extra); err != nil {
		if err != io.EOF {
			return ReviewAsset{}, fmt.Errorf("catalogreview: invalid bundled review asset")
		}
	} else {
		return ReviewAsset{}, fmt.Errorf("catalogreview: invalid bundled review asset")
	}
	tab, ok := top.(map[string]any)
	if !ok {
		return ReviewAsset{}, fmt.Errorf("catalogreview: invalid bundled review asset")
	}
	wantKeys := []string{"models", "provenance", "retrieved_at", "revision", "schema_version", "stale_after_seconds"}
	if len(tab) != len(wantKeys) {
		return ReviewAsset{}, fmt.Errorf("catalogreview: invalid bundled review schema")
	}
	for _, k := range wantKeys {
		if _, has := tab[k]; !has {
			return ReviewAsset{}, fmt.Errorf("catalogreview: invalid bundled review schema")
		}
	}
	if !isExactIntNumber(tab["schema_version"], 1) {
		return ReviewAsset{}, fmt.Errorf("catalogreview: invalid bundled review schema")
	}
	if !isExactIntNumber(tab["revision"], 1) {
		return ReviewAsset{}, fmt.Errorf("catalogreview: invalid bundled review revision")
	}
	retrievedStr, ok := tab["retrieved_at"].(string)
	if !ok || retrievedStr == "" {
		return ReviewAsset{}, fmt.Errorf("catalogreview: invalid bundled review retrieved_at")
	}
	ts, err := time.Parse(time.RFC3339, retrievedStr)
	if err != nil {
		return ReviewAsset{}, fmt.Errorf("catalogreview: invalid bundled review retrieved_at")
	}
	retrievedWall := float64(ts.UnixNano()) / 1e9
	staleAfter, ok := exactPositiveInt(tab["stale_after_seconds"])
	if !ok {
		return ReviewAsset{}, fmt.Errorf("catalogreview: invalid bundled review stale_after_seconds")
	}
	provRaw, ok := tab["provenance"].(map[string]any)
	if !ok {
		return ReviewAsset{}, fmt.Errorf("catalogreview: invalid bundled review provenance")
	}
	provKeys := []string{"repository_revision", "reviewed_on", "snapshot_sha256", "source", "verification"}
	if len(provRaw) != len(provKeys) {
		return ReviewAsset{}, fmt.Errorf("catalogreview: invalid bundled review provenance")
	}
	prov := ReviewProvenance{}
	for _, k := range provKeys {
		s, ok := provRaw[k].(string)
		if !ok || s == "" {
			return ReviewAsset{}, fmt.Errorf("catalogreview: invalid bundled review provenance")
		}
		switch k {
		case "source":
			prov.Source = s
		case "reviewed_on":
			prov.ReviewedOn = s
		case "repository_revision":
			prov.RepoRevision = s
		case "snapshot_sha256":
			prov.SnapshotSHA = s
		case "verification":
			prov.Verification = s
		}
	}
	modelsRaw, ok := tab["models"].(map[string]any)
	if !ok {
		return ReviewAsset{}, fmt.Errorf("catalogreview: invalid bundled review models")
	}
	rows := make(map[string]ReviewRow, len(modelsRaw))
	ids := make([]string, 0, len(modelsRaw))
	for id := range modelsRaw {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if id == "" {
			return ReviewAsset{}, fmt.Errorf("catalogreview: invalid bundled review model id")
		}
		rowRaw, ok := modelsRaw[id].(map[string]any)
		if !ok || len(rowRaw) != 3 {
			return ReviewAsset{}, fmt.Errorf("catalogreview: invalid bundled review row")
		}
		src, ok := rowRaw["source"].(string)
		if !ok || src == "" {
			return ReviewAsset{}, fmt.Errorf("catalogreview: invalid bundled review row")
		}
		dialect, ok := rowRaw["dialect"].(string)
		if !ok || !isValidDialect(dialect) {
			return ReviewAsset{}, fmt.Errorf("catalogreview: invalid bundled review row dialect")
		}
		factsRaw, ok := rowRaw["facts"].(map[string]any)
		if !ok {
			return ReviewAsset{}, fmt.Errorf("catalogreview: invalid bundled review row")
		}
		facts := make(map[string]Value, len(factsRaw))
		paths := make([]string, 0, len(factsRaw))
		for p := range factsRaw {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for _, p := range paths {
			if !IsField(p) {
				return ReviewAsset{}, fmt.Errorf("catalogreview: invalid bundled Go review field: %s", p)
			}
			v, ok := jsonValueForField(p, factsRaw[p])
			if !ok {
				return ReviewAsset{}, fmt.Errorf("catalogreview: invalid bundled Go review field: %s", p)
			}
			if isLimit, _ := splitLimit(p); isLimit {
				if v.IsNull() {
					continue
				}
				n, ok := v.Int()
				if !ok || n <= 0 {
					return ReviewAsset{}, fmt.Errorf("catalogreview: invalid bundled Go review field: %s", p)
				}
			}
			facts[p] = v
		}
		rows[id] = ReviewRow{source: src, dialect: dialect, facts: facts}
	}
	return ReviewAsset{retrievedWall: retrievedWall, staleAfter: staleAfter, provenance: prov, rows: rows}, nil
}

// CatalogReview builds the private per-route base: Empty(dialect) plus
// validated row facts (kind reviewed, source go.review.<path>) plus the
// go.review provenance source. Unknown IDs return Empty unchanged (no
// review source); dialect mismatch preserves configured transport facts
// with applicable false. No name-prefix, endpoint-URL, or credential
// inference: exact ID plus row-dialect gate only.
func CatalogReview(modelID, dialect string, asset ReviewAsset) Contract {
	base := Empty(dialect)
	row, ok := asset.rows[modelID]
	if !ok {
		return base
	}
	applicable := row.dialect == dialect
	retrieved := asset.retrievedWall
	ttl := asset.staleAfter
	ver := asset.provenance.Verification
	src := &Source{
		retrievedAt:       &retrieved,
		staleAfterSeconds: &ttl,
		verification:      &ver,
		applicable:        &applicable,
	}
	ms := row.source
	src.modelSource = &ms
	rs := asset.provenance.Source
	src.reviewSource = &rs
	ro := asset.provenance.ReviewedOn
	src.reviewedOn = &ro
	rr := asset.provenance.RepoRevision
	src.repoRevision = &rr
	ss := asset.provenance.SnapshotSHA
	src.snapshotSHA = &ss
	if base.sources == nil {
		base.sources = map[string]*Source{}
	}
	base.sources["go.review"] = src
	if !applicable {
		return base
	}
	paths := make([]string, 0, len(row.facts))
	for p := range row.facts {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		s := "go.review." + p
		base.put(p, row.facts[p].clone(), &s, KindReviewed)
	}
	return base
}

// NoteCatalogAvailability stamps the catalog-availability source at the
// existing original wall success time: retrieved_at plus the fixed
// "availability only" verification. Called for every catalog row,
// including when the review source is absent.
func (c *Contract) NoteCatalogAvailability(retrievedWall float64) {
	if c.sources == nil {
		c.sources = map[string]*Source{}
	}
	w := retrievedWall
	ver := "availability only"
	c.sources["catalog"] = &Source{retrievedAt: &w, verification: &ver}
}

func isExactIntNumber(v any, want int64) bool {
	n, ok := v.(json.Number)
	if !ok {
		return false
	}
	got, ok := parseExactInt64(n.String())
	if !ok || got != want {
		return false
	}
	return true
}

func exactPositiveInt(v any) (float64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	got, ok := parseExactInt64(n.String())
	if !ok || got <= 0 {
		return 0, false
	}
	return float64(got), true
}

func parseExactInt64(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	neg := false
	digits := s
	if s[0] == '-' {
		neg = true
		digits = s[1:]
		if digits == "" {
			return 0, false
		}
	} else if s[0] == '+' {
		return 0, false
	}
	if len(digits) > 1 && digits[0] == '0' {
		return 0, false
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	_ = neg
	return n, true
}

// jsonValueForField converts one raw JSON leaf to a closed Value for a
// known path (null always admitted). Only JSON-decoded types are accepted:
// nil, bool, string, json.Number, []any, map[string]any. Floats with
// fractional parts never satisfy int fields; bools never satisfy ints.
func jsonValueForField(path string, raw any) (Value, bool) {
	if raw == nil {
		return Null(), true
	}
	if isLimit, _ := splitLimit(path); isLimit {
		n, ok := raw.(json.Number)
		if !ok {
			return Value{}, false
		}
		got, ok := parseExactInt64(n.String())
		if !ok {
			return Value{}, false
		}
		return IntValue(got), true
	}
	e, ok := fieldTable[path]
	if !ok {
		return Value{}, false
	}
	switch e {
	case expectBool:
		if b, ok := raw.(bool); ok {
			return BoolValue(b), true
		}
	case expectNonNegInt:
		n, ok := raw.(json.Number)
		if !ok {
			return Value{}, false
		}
		got, ok := parseExactInt64(n.String())
		if !ok || got < 0 {
			return Value{}, false
		}
		return IntValue(got), true
	case expectNonEmptyString:
		if s, ok := raw.(string); ok && s != "" {
			return StringValue(s), true
		}
	case expectStrings:
		arr, ok := raw.([]any)
		if !ok {
			return Value{}, false
		}
		ss := make([]string, 0, len(arr))
		for _, item := range arr {
			s, ok := item.(string)
			if !ok || s == "" {
				return Value{}, false
			}
			ss = append(ss, s)
		}
		return StringListValue(ss)
	case expectNative:
		m, ok := raw.(map[string]any)
		if !ok {
			return Value{}, false
		}
		return nativeFromJSON(m)
	case expectNumber:
		n, ok := raw.(json.Number)
		if !ok {
			return Value{}, false
		}
		if got, ok := parseExactInt64(n.String()); ok {
			return IntValue(got), true
		}
		f, err := n.Float64()
		if err != nil {
			return Value{}, false
		}
		return FloatValue(f)
	}
	return Value{}, false
}

func nativeFromJSON(m map[string]any) (Value, bool) {
	for k := range m {
		if k != "protocol" && k != "path" && k != "values" {
			return Value{}, false
		}
	}
	proto, _ := m["protocol"].(string)
	if !isValidDialect(proto) {
		return Value{}, false
	}
	path, ok := m["path"].(string)
	if !ok || path == "" {
		return Value{}, false
	}
	var vals map[string]NativeScalar
	if raw, has := m["values"]; has {
		vm, ok := raw.(map[string]any)
		if !ok {
			return Value{}, false
		}
		vals = make(map[string]NativeScalar, len(vm))
		for k, item := range vm {
			switch n := item.(type) {
			case string:
				vals[k] = StringScalar(n)
			case bool:
				vals[k] = BoolScalar(n)
			case json.Number:
				got, ok := parseExactInt64(n.String())
				if !ok {
					return Value{}, false
				}
				vals[k] = IntScalar(got)
			default:
				return Value{}, false
			}
		}
	}
	np, ok := NewNativeParam(proto, path, vals)
	if !ok {
		return Value{}, false
	}
	return NativeValue(np), true
}
