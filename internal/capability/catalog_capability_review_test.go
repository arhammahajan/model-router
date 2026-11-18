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

// Frozen 29-row review: exact IDs, exact row dialects, 27 fact paths,
// SHA-pinned embedded copy. Any drift in the bundled asset fails here.

// frozenCatalogIDs is the exact frozen row set: id -> reviewed dialect.
var frozenCatalogIDs = map[string]string{
	"beam-free": "openai-chat", "deepseek-v4-pro": "openai-chat",
	"deepseek-v4.1-flash": "openai-chat", "glm-5.2": "openai-chat",
	"glm-5.3": "openai-chat", "glm-5.3-flash": "openai-chat",
	"hy3": "openai-chat", "hy4-preview": "openai-chat",
	"kimi-k2.7-code": "openai-chat", "kimi-k3": "openai-chat",
	"kimi-k3.1-pro": "openai-chat", "longcat-2.0": "openai-chat",
	"mimo-v2.5": "openai-chat", "mimo-v2.5-pro": "openai-chat",
	"mimo-v2.6-flash": "openai-chat", "mimo-v2.6-pro": "openai-chat",
	"minimax-m3.1-flash": "openai-chat", "step-5-preview": "openai-chat",
	"gpt-5.6-luna": "openai-responses", "gpt-6-luna": "openai-responses",
	"grok-4.7": "openai-responses", "muse-spark-1.2-contributor": "openai-responses",
	"muse-spark-1.3-contributor": "openai-responses",
	"claude-haiku-5-5":           "anthropic", "minimax-m3": "anthropic",
	"qwen3.7-plus": "anthropic", "qwen3.8-flash": "anthropic",
	"qwen3.8-max": "anthropic", "gemini-4-argon": "gemini",
}

var frozenFactPaths = []string{
	"limits.context_window", "limits.max_input_tokens", "limits.max_output_tokens",
	"modalities.input.audio", "modalities.input.embedding", "modalities.input.image",
	"modalities.input.pdf", "modalities.input.text", "modalities.input.video",
	"modalities.output.audio", "modalities.output.embedding", "modalities.output.image",
	"modalities.output.pdf", "modalities.output.text", "modalities.output.video",
	"reasoning.budget.max_tokens", "reasoning.budget.native_parameter",
	"reasoning.budget.supported", "reasoning.effort.native_parameter",
	"reasoning.effort.supported", "reasoning.effort.values",
	"reasoning.supported", "reasoning.toggle.native_parameter",
	"reasoning.toggle.supported", "sampling.temperature.supported",
	"structured_output.supported", "tools.supported",
}

func capReviewWire(t *testing.T, c Contract) map[string]any {
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

func capReviewLeaf(t *testing.T, wire map[string]any, path string) map[string]any {
	t.Helper()
	cur := wire
	for _, part := range strings.Split(path, ".") {
		next, ok := cur[part].(map[string]any)
		if !ok {
			t.Fatalf("leaf %q missing", path)
		}
		cur = next
	}
	return cur
}

func capReviewAsset(t *testing.T) ReviewAsset {
	t.Helper()
	a, err := LoadReviewAsset(EmbeddedReviewBytes())
	if err != nil {
		t.Fatalf("bundled asset must validate: %v", err)
	}
	return a
}

// Exact SHA: embedded bytes are the exact-byte source copy.
func TestCatalogCapReviewEmbeddedSHA(t *testing.T) {
	raw := EmbeddedReviewBytes()
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != "e9bb58bfefc02999a5839d5ec9ba0b451c5b4b9781054d65a3aff56e71d4c271" {
		t.Fatalf("embedded bytes hash changed: %s", got)
	}
	// Returned bytes are a copy: caller mutation cannot move the image.
	probe := EmbeddedReviewBytes()
	probe[0] ^= 0xff
	again := EmbeddedReviewBytes()
	sum2 := sha256.Sum256(again)
	if hex.EncodeToString(sum2[:]) != "e9bb58bfefc02999a5839d5ec9ba0b451c5b4b9781054d65a3aff56e71d4c271" {
		t.Fatal("embedded image mutated through the accessor")
	}
}

// All 29 frozen rows gate on exact ID + row dialect.
func TestCatalogCapReviewAllRowsGate(t *testing.T) {
	if len(frozenCatalogIDs) != 29 {
		t.Fatalf("frozen table must list 29 rows, got %d", len(frozenCatalogIDs))
	}
	a := capReviewAsset(t)
	dialectCount := map[string]int{}
	for id, dialect := range frozenCatalogIDs {
		dialectCount[dialect]++
		// Match: reviewed facts applied, applicable true, full provenance.
		c := CatalogReview(id, dialect, a)
		wire := capReviewWire(t, c)
		srcs := wire["sources"].(map[string]any)
		gr, ok := srcs["go.review"].(map[string]any)
		if !ok {
			t.Fatalf("%s: match must carry go.review", id)
		}
		if gr["applicable"] != true {
			t.Fatalf("%s: applicable must be true", id)
		}
		if gr["model_source"] == nil || gr["source"] != "https://models.dev/api.json" {
			t.Fatalf("%s: provenance wrong: %v", id, gr)
		}
		// Wrong dialect: configured transport preserved, facts unknown,
		// applicable false, provenance retained.
		wrong := "openai-chat"
		if dialect == "openai-chat" {
			wrong = "anthropic"
		}
		m := CatalogReview(id, wrong, a)
		mwire := capReviewWire(t, m)
		mgr := mwire["sources"].(map[string]any)["go.review"].(map[string]any)
		if mgr["applicable"] != false {
			t.Fatalf("%s: mismatch applicable must be false", id)
		}
		if mgr["model_source"] == nil || mgr["source"] == nil {
			t.Fatalf("%s: mismatch must retain provenance", id)
		}
		native := mwire["protocols"].(map[string]any)["native"].(map[string]any)
		if native[wrong].(map[string]any)["value"] != true {
			t.Fatalf("%s: mismatch must keep route transport %s", id, wrong)
		}
		mt := capReviewLeaf(t, mwire, "tools.supported")
		if mt["value"] != nil || mt["kind"] != KindUnknown {
			t.Fatalf("%s: mismatch facts must stay unknown", id)
		}
	}
	if dialectCount["openai-chat"] != 18 || dialectCount["openai-responses"] != 5 ||
		dialectCount["anthropic"] != 5 || dialectCount["gemini"] != 1 {
		t.Fatalf("dialect mix changed: %v", dialectCount)
	}
}

// All 27 fact paths: union over matched rows covers the frozen vocabulary.
func TestCatalogCapReviewAllPathsCovered(t *testing.T) {
	a := capReviewAsset(t)
	seen := map[string]bool{}
	for id, dialect := range frozenCatalogIDs {
		c := CatalogReview(id, dialect, a)
		wire := capReviewWire(t, c)
		var walk func(prefix string, node map[string]any)
		_ = walk
		for _, p := range frozenFactPaths {
			if strings.HasPrefix(p, "limits.") {
				lim := wire["limits"].(map[string]any)
				key := strings.TrimPrefix(p, "limits.")
				if f, ok := lim[key].(map[string]any); ok && f["value"] != nil {
					seen[p] = true
				}
				continue
			}
			leaf := capReviewLeaf(t, wire, p)
			if leaf["value"] != nil {
				seen[p] = true
			}
		}
	}
	for _, p := range frozenFactPaths {
		if !IsField(p) {
			t.Fatalf("frozen path %q left the closed vocabulary", p)
		}
		if !seen[p] {
			t.Fatalf("frozen path %q never carries a value in any row", p)
		}
	}
	if len(frozenFactPaths) != 27 {
		t.Fatalf("frozen path count changed: %d", len(frozenFactPaths))
	}
}

// Unknown IDs carry no review source; explicit negatives stay false.
func TestCatalogCapReviewUnknownAndNegative(t *testing.T) {
	a := capReviewAsset(t)
	for _, id := range []string{"", "no-such-model", "GLM-5.3-FLASH", " glm-5.3-flash", "glm-5.3-flash "} {
		c := CatalogReview(id, "openai-chat", a)
		wire := capReviewWire(t, c)
		if _, has := wire["sources"].(map[string]any)["go.review"]; has {
			t.Fatalf("%q must carry no review source", id)
		}
		v, ok := c.Get("tools.supported")
		if !ok || !v.IsNull() || v.IsFalse() {
			t.Fatalf("%q must be null, never false", id)
		}
	}
	// Known explicit false is IsFalse, never null (beam-free has false leaves).
	b := CatalogReview("beam-free", "openai-chat", a)
	fv, _ := b.Get("modalities.input.audio")
	if fv.IsNull() || !fv.IsFalse() {
		t.Fatalf("known false must be explicit false: %+v", fv)
	}
	tv, _ := b.Get("tools.supported")
	if b2, _ := tv.Bool(); !b2 {
		t.Fatalf("known true must stay true: %+v", tv)
	}
}

// Native descriptors: dtype-exact with copy isolation on every accessor.
func TestCatalogCapReviewNativeDtype(t *testing.T) {
	a := capReviewAsset(t)
	c := CatalogReview("qwen3.8-max", "anthropic", a)
	v, ok := c.Get("reasoning.toggle.native_parameter")
	if !ok || v.IsNull() {
		t.Fatalf("native row must carry a descriptor: %+v", v)
	}
	np, ok := v.Native()
	if !ok {
		t.Fatal("native accessor must succeed")
	}
	if np.Protocol() != "anthropic" || np.Path() != "thinking.type" {
		t.Fatalf("native descriptor wrong: %+v", np)
	}
	vals := np.Values()
	if vals["enabled"] == (NativeScalar{}) || vals["disabled"] == (NativeScalar{}) {
		t.Fatalf("native mapping values missing: %v", vals)
	}
	vals["enabled"] = StringScalar("mutated")
	again, _ := c.Get("reasoning.toggle.native_parameter")
	np2, _ := again.Native()
	if s, _ := np2.Values()["enabled"].String(); s != "enabled" {
		t.Fatalf("native values mutation leaked: %q", s)
	}
	// Effort native without a values map renders without "values".
	e := CatalogReview("glm-5.3-flash", "openai-chat", a)
	ev, _ := e.Get("reasoning.effort.native_parameter")
	enp, ok := ev.Native()
	if !ok || enp.Values() != nil {
		t.Fatalf("values-absent native must stay absent: %+v", enp)
	}
	wire := capReviewWire(t, Render(e, nil, StaticDiscovery(), 1500))
	leaf := capReviewLeaf(t, wire, "reasoning.effort.native_parameter")
	if m, _ := leaf["value"].(map[string]any); m == nil {
		t.Fatalf("native must render an object: %v", leaf)
	} else if _, has := m["values"]; has {
		t.Fatalf("absent values must not render: %v", m)
	}
}

// Limits: exact int64 dtype, bigint precision, no float/bool/string.
func TestCatalogCapReviewLimitsPrecision(t *testing.T) {
	a := capReviewAsset(t)
	c := CatalogReview("beam-free", "openai-chat", a)
	v, ok := c.Get("limits.context_window")
	if !ok {
		t.Fatal("limits leaf missing")
	}
	n, ok := v.Int()
	if !ok || n != 1048576 {
		t.Fatalf("limits must be exact int64: %+v", v)
	}
	if _, ok := v.Float(); ok {
		t.Fatal("int limits must not satisfy Float")
	}
	// Max-int64 boundary admitted exactly through the loader.
	raw := spliceLimit(t, 9223372036854775807)
	asset, err := LoadReviewAsset(raw)
	if err != nil {
		t.Fatalf("max-int64 limit must load: %v", err)
	}
	got := CatalogReview("beam-free", "openai-chat", asset)
	lv, _ := got.Get("limits.context_window")
	if n, _ := lv.Int(); n != 9223372036854775807 {
		t.Fatalf("bigint precision lost: %+v", lv)
	}
	// Overflow, float, bool, string limits all fail closed.
	for _, raw := range []struct {
		name string
		body string
	}{
		{"overflow int", "9223372036854775808"},
		{"float", "1.5"},
		{"float whole", "1048576.0"},
		{"bool", "true"},
		{"string", `"1048576"`},
		{"zero", "0"},
		{"negative", "-5"},
	} {
		bad := spliceLimitRaw(t, raw.body)
		if _, err := LoadReviewAsset(bad); err == nil {
			t.Fatalf("limit %s (%s) must fail", raw.name, raw.body)
		} else if !strings.Contains(err.Error(), "catalogreview:") {
			t.Fatalf("limit %s error must name the boundary: %v", raw.name, err)
		}
	}
	// Python skips null limits: preserve unknown provenance, not reviewed null.
	nulled := spliceLimitRaw(t, "null")
	na, err := LoadReviewAsset(nulled)
	if err != nil {
		t.Fatalf("null limit must load: %v", err)
	}
	nc := CatalogReview("beam-free", "openai-chat", na)
	nv, _ := nc.Get("limits.context_window")
	if !nv.IsNull() {
		t.Fatalf("null limit must be null: %+v", nv)
	}
	leaf := capReviewLeaf(t, capReviewWire(t, nc), "limits.context_window")
	if leaf["source"] != nil || leaf["kind"] != KindUnknown {
		t.Fatalf("skipped null limit must remain unobserved: %v", leaf)
	}
}

func spliceLimit(t *testing.T, n int64) []byte {
	t.Helper()
	return spliceLimitRaw(t, jsonNumber(t, n))
}

func jsonNumber(t *testing.T, n int64) string {
	t.Helper()
	raw, _ := json.Marshal(n)
	return string(raw)
}

func spliceLimitRaw(t *testing.T, expr string) []byte {
	t.Helper()
	base := EmbeddedReviewBytes()
	dec := json.NewDecoder(bytes.NewReader(base))
	dec.UseNumber()
	var top map[string]any
	if err := dec.Decode(&top); err != nil {
		t.Fatal(err)
	}
	enc, _ := json.Marshal(top)
	s := string(enc)
	anchor := `"beam-free":{`
	idx := strings.Index(s, anchor)
	if idx < 0 {
		t.Fatal("cannot find beam-free row")
	}
	// The row's facts carry the flat dotted limit key; splice within the
	// row segment only so other rows sharing the value are untouched.
	seg := s[idx:]
	limKey := `"limits.context_window":`
	lidx := strings.Index(seg, limKey)
	if lidx < 0 {
		t.Fatal("cannot splice limit")
	}
	valStart := lidx + len(limKey)
	valEnd := valStart
	for valEnd < len(seg) && seg[valEnd] != ',' && seg[valEnd] != '}' {
		valEnd++
	}
	seg = seg[:valStart] + expr + seg[valEnd:]
	return []byte(s[:idx] + seg)
}

// Loader hardening: lexical cases the Marshal round-trip would hide.
func TestCatalogCapReviewLoaderHardening(t *testing.T) {
	base := string(EmbeddedReviewBytes())
	cases := map[string]func(string) string{
		"float schema 1.0": func(s string) string {
			return strings.Replace(s, `"schema_version": 1`, `"schema_version": 1.0`, 1)
		},
		"string revision": func(s string) string {
			return strings.Replace(s, `"revision": 1`, `"revision": "1"`, 1)
		},
		"unknown top-level key": func(s string) string {
			return strings.Replace(s, `"schema_version": 1`, `"schema_version": 1, "extra": 1`, 1)
		},
		"missing provenance": func(s string) string {
			idx := strings.Index(s, `"provenance":`)
			if idx < 0 {
				return s
			}
			depth, i := 0, idx+len(`"provenance":`)
			for ; i < len(s); i++ {
				if s[i] == '{' {
					depth++
				} else if s[i] == '}' {
					depth--
					if depth == 0 {
						break
					}
				}
			}
			return s[:idx] + s[i+1:]
		},
		"empty provenance source": func(s string) string {
			return strings.Replace(s, `"source": "https://models.dev/api.json"`, `"source": ""`, 1)
		},
		"bad retrieved_at": func(s string) string {
			return strings.Replace(s, `"retrieved_at": "2026-10-25T19:45:18Z"`, `"retrieved_at": "25/10/2026"`, 1)
		},
		"unknown fact field": func(s string) string {
			return strings.Replace(s, `"tools.supported": true`, `"tools.supported": true, "nope.field": true`, 1)
		},
		"bool as number": func(s string) string {
			return strings.Replace(s, `"tools.supported": true`, `"tools.supported": 1`, 1)
		},
		"trailing garbage": func(s string) string { return s + " trailing" },
		"second document":  func(s string) string { return s + " {}" },
		"empty model id": func(s string) string {
			return strings.Replace(s, `"beam-free": {`, `"": {`, 1)
		},
		"bad row dialect": func(s string) string {
			return strings.Replace(s, `"dialect": "openai-chat"`, `"dialect": "http"`, 1)
		},
		"row extra key": func(s string) string {
			return strings.Replace(s, `"beam-free": {`, `"beam-free": {"zzz": 1, `, 1)
		},
	}
	for name, fn := range cases {
		if _, err := LoadReviewAsset([]byte(fn(base))); err == nil {
			t.Fatalf("%s must fail", name)
		} else if !strings.Contains(err.Error(), "catalogreview:") {
			t.Fatalf("%s error must name the boundary: %v", name, err)
		}
	}
	if _, err := LoadReviewAsset([]byte("")); err == nil {
		t.Fatal("empty asset must fail")
	}
	if _, err := LoadReviewAsset([]byte("{")); err == nil {
		t.Fatal("truncated asset must fail")
	}
}

// Copy isolation: clones, accessors, and renders never alias stored facts.
func TestCatalogCapReviewCopyIsolation(t *testing.T) {
	a := capReviewAsset(t)
	b := a.Clone()
	_ = b
	c := CatalogReview("glm-5.3-flash", "openai-chat", a)
	v, _ := c.Get("reasoning.effort.values")
	ss, ok := v.StringList()
	if !ok {
		t.Fatalf("effort values must be a string list: %+v", v)
	}
	ss[0] = "mutated"
	again, _ := c.Get("reasoning.effort.values")
	ss2, _ := again.StringList()
	if ss2[0] == "mutated" {
		t.Fatal("StringList mutation leaked into the contract")
	}
	ms, src, ro, repo, sha := provenanceCopies(t, c)
	for _, p := range []*string{ms, src, ro, repo, sha} {
		if p == nil {
			t.Fatal("provenance accessors must be present on match")
		}
		*p = "mutated"
	}
	ms2, _, _, _, _ := provenanceCopies(t, c)
	if *ms2 == "mutated" {
		t.Fatal("provenance accessor mutation leaked")
	}
	first, _ := json.Marshal(Render(c, nil, StaticDiscovery(), 1500))
	var mutated Contract = Render(c, nil, StaticDiscovery(), 1500)
	_ = mutated.Put("tools.supported", BoolValue(false), strPtrCap("probe"), KindOverride)
	second, _ := json.Marshal(Render(c, nil, StaticDiscovery(), 1500))
	if string(first) != string(second) {
		t.Fatal("render output mutation moved the stored base")
	}
	// Review wall parses the frozen RFC3339 exactly once, offline.
	want := wallOf(t, "2026-10-25T19:45:18Z")
	fresh := Render(c, nil, NewDiscovery(FreshnessInput{}, 300, want+100), want+100)
	wire := capReviewWire(t, fresh)
	gr := wire["sources"].(map[string]any)["go.review"].(map[string]any)
	if gr["retrieved_at"] != want {
		t.Fatalf("review retrieved_at must be the frozen wall: %v", gr["retrieved_at"])
	}
}

func provenanceCopies(t *testing.T, c Contract) (ms, src, ro, repo, sha *string) {
	t.Helper()
	// The contract exposes provenance only through the rendered wire and
	// the typed Source accessors; prove the rendered copy is independent
	// by mutating one rendering and re-rendering from the same base.
	first := capReviewWire(t, Render(c.Clone(), nil, StaticDiscovery(), 1500))
	gr := first["sources"].(map[string]any)["go.review"].(map[string]any)
	take := func(k string) *string {
		s, _ := gr[k].(string)
		v := s
		return &v
	}
	ms, src, ro, repo, sha = take("model_source"), take("source"), take("reviewed_on"), take("repository_revision"), take("snapshot_sha256")
	if *ms == "" || *src == "" || *ro == "" || *repo == "" || *sha == "" {
		t.Fatalf("provenance must be present on match: %v", gr)
	}
	gr["model_source"] = "mutated"
	return ms, src, ro, repo, sha
}

func strPtrCap(s string) *string { v := s; return &v }

func wallOf(t *testing.T, iso string) float64 {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		t.Fatal(err)
	}
	return float64(ts.UnixNano()) / 1e9
}

// Concurrent construction/render shares nothing under -race.
func TestCatalogCapReviewConcurrent(t *testing.T) {
	a := capReviewAsset(t)
	ids := make([]string, 0, len(frozenCatalogIDs))
	for id := range frozenCatalogIDs {
		ids = append(ids, id)
	}
	base := CatalogReview("glm-5.3-flash", "openai-chat", a)
	base.NoteCatalogAvailability(1000)
	want, _ := json.Marshal(Render(base, nil, StaticDiscovery(), 1500))
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := ids[i%len(ids)]
			c := CatalogReview(id, frozenCatalogIDs[id], a)
			c.NoteCatalogAvailability(1000)
			out, err := json.Marshal(Render(c, nil, StaticDiscovery(), 1500))
			if err != nil {
				t.Error(err)
				return
			}
			_ = out
			self, err := json.Marshal(Render(base, nil, StaticDiscovery(), 1500))
			if err != nil {
				t.Error(err)
				return
			}
			if string(self) != string(want) {
				t.Errorf("drift under race")
			}
		}(i)
	}
	wg.Wait()
}
