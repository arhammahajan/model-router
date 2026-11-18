package capability

// Independent review probes for the closed typed domain (U1-U3):
// exact-source validator cross-product, normalize/relation repair,
// conflict retention, freshness math, render/input isolation, and the
// pre-admitted Overrides trust boundary. Every behavioral assertion stays
// on the wire JSON (the Python contract shape). Raw literals use only
// go-toml decoded types (map[string]any, []any, string, bool, int64,
// float64).

import (
	"encoding/json"
	"math"
	"strings"
	"sync"
	"testing"
)

func reviewWire(t *testing.T, c Contract) map[string]any {
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

func reviewLeaf(t *testing.T, wire map[string]any, path string) map[string]any {
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

func reviewAdmit(t *testing.T, raw any) map[string]Overrides {
	t.Helper()
	got, err := ValidateOverrides(raw)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	return got
}

// Booleans are exact: numbers, strings, and null-shaped values must not
// satisfy a boolean field (Python `type(value) is bool`).
func TestReviewBoolRejectsNumbersAndStrings(t *testing.T) {
	for _, raw := range []any{int64(1), int64(0), float64(1), "true", "false"} {
		_, err := ValidateOverrides(map[string]any{
			"m": map[string]any{"streaming": raw},
		})
		if err == nil {
			t.Fatalf("streaming=%T must be rejected", raw)
		}
		if !strings.Contains(err.Error(), "streaming") {
			t.Fatalf("error must name the path: %v", err)
		}
	}
	if _, err := ValidateOverrides(map[string]any{
		"m": map[string]any{"streaming": true},
	}); err != nil {
		t.Fatalf("streaming=true must be admitted: %v", err)
	}
}

// Budget ints are exact int64: bools, negatives, and floats must fail
// (Python `type(v) is int`, `v >= 0`); the int64 maximum round-trips
// exactly through the wire.
func TestReviewBudgetIntExactness(t *testing.T) {
	for _, raw := range []any{true, false, int64(-1), float64(5), float64(5.0), "5"} {
		_, err := ValidateOverrides(map[string]any{
			"m": map[string]any{"reasoning": map[string]any{"budget": map[string]any{"max_tokens": raw}}},
		})
		if err == nil {
			t.Fatalf("max_tokens=%T(%v) must be rejected", raw, raw)
		}
	}
	const maxInt64 = int64(9223372036854775807)
	got := reviewAdmit(t, map[string]any{
		"m": map[string]any{"reasoning": map[string]any{"budget": map[string]any{"max_tokens": maxInt64}}},
	})
	rendered := Render(Empty("openai-chat"), got["m"], StaticDiscovery(), 1000)
	leaf := reviewLeaf(t, reviewWire(t, rendered), "reasoning.budget.max_tokens")
	if leaf["value"] != float64(maxInt64) {
		t.Fatalf("bigint must survive exactly: %v", leaf["value"])
	}
	if leaf["kind"] != KindOverride {
		t.Fatalf("kind: %v", leaf)
	}
}

// String lists are nonempty unique elements; an empty list is valid
// (Python `_strings`: empty allowed) and duplicates are rejected.
func TestReviewStringListUniqueness(t *testing.T) {
	mk := func(v any) map[string]any {
		return map[string]any{"m": map[string]any{"reasoning": map[string]any{"effort": map[string]any{"values": v}}}}
	}
	if _, err := ValidateOverrides(mk([]any{"low", "low"})); err == nil {
		t.Fatal("duplicate effort values must be rejected")
	}
	if _, err := ValidateOverrides(mk([]any{"low", ""})); err == nil {
		t.Fatal("empty effort value must be rejected")
	}
	if _, err := ValidateOverrides(mk("low")); err == nil {
		t.Fatal("scalar effort values must be rejected")
	}
	got := reviewAdmit(t, mk([]any{}))
	rendered := Render(Empty("openai-chat"), got["m"], StaticDiscovery(), 1000)
	leaf := reviewLeaf(t, reviewWire(t, rendered), "reasoning.effort.values")
	arr, ok := leaf["value"].([]any)
	if !ok {
		t.Fatalf("explicit empty list must render as [], not null (got %v)", leaf["value"])
	}
	if len(arr) != 0 {
		t.Fatalf("empty list must stay empty: %v", arr)
	}
}

// Native descriptors port `_native` exactly: keys subset of
// protocol/path/values, known protocol, nonempty path, primitive-only
// mapping. Empty mapping keys and empty string values are admitted
// (Python only requires isinstance(k, str)); floats in the mapping fail.
func TestReviewNativeBoundary(t *testing.T) {
	mk := func(v any) map[string]any {
		return map[string]any{"m": map[string]any{
			"reasoning": map[string]any{"effort": map[string]any{"native_parameter": v}}}}
	}
	valid := map[string]any{"protocol": "openai-chat", "path": "reasoning_effort"}
	if _, err := ValidateOverrides(mk(valid)); err != nil {
		t.Fatalf("bare native descriptor: %v", err)
	}
	withVals := map[string]any{"protocol": "openai-chat", "path": "p",
		"values": map[string]any{"": "empty-key-ok", "s": "", "b": true, "i": int64(3)}}
	got := reviewAdmit(t, mk(withVals))
	rendered := Render(Empty("openai-chat"), got["m"], StaticDiscovery(), 1000)
	leaf := reviewLeaf(t, reviewWire(t, rendered), "reasoning.effort.native_parameter")
	vals, ok := leaf["value"].(map[string]any)["values"].(map[string]any)
	if !ok || vals[""] != "empty-key-ok" || vals["s"] != "" || vals["b"] != true || vals["i"] != float64(3) {
		t.Fatalf("native mapping must round-trip incl empty key/string: %v", leaf["value"])
	}
	for name, bad := range map[string]any{
		"bad-protocol":   map[string]any{"protocol": "http", "path": "p"},
		"missing-proto":  map[string]any{"path": "p"},
		"empty-path":     map[string]any{"protocol": "openai-chat", "path": ""},
		"extra-key":      map[string]any{"protocol": "openai-chat", "path": "p", "extra": true},
		"float-mapping":  map[string]any{"protocol": "openai-chat", "path": "p", "values": map[string]any{"x": float64(1.5)}},
		"nonmap-values":  map[string]any{"protocol": "openai-chat", "path": "p", "values": "x"},
		"nonstring-path": map[string]any{"protocol": "openai-chat", "path": int64(1)},
	} {
		if _, err := ValidateOverrides(mk(bad)); err == nil {
			t.Fatalf("%s must be rejected", name)
		}
	}
	// Absent values omits the wire key; present-but-empty renders {}.
	bare := reviewLeaf(t, reviewWire(t, Render(Empty("openai-chat"),
		reviewAdmit(t, mk(valid))["m"], StaticDiscovery(), 1000)), "reasoning.effort.native_parameter")
	if _, has := bare["value"].(map[string]any)["values"]; has {
		t.Fatalf("absent values must omit the key: %v", bare["value"])
	}
	emptyVals := map[string]any{"protocol": "openai-chat", "path": "p", "values": map[string]any{}}
	ev := reviewLeaf(t, reviewWire(t, Render(Empty("openai-chat"),
		reviewAdmit(t, mk(emptyVals))["m"], StaticDiscovery(), 1000)), "reasoning.effort.native_parameter")
	vals, ok = ev["value"].(map[string]any)["values"].(map[string]any)
	if !ok || len(vals) != 0 {
		t.Fatalf("empty values map must render as {{}}, not null: %v", ev["value"])
	}
}

// Sampling numeric defaults admit int64 or finite float64 (Python
// `_number`); NaN/Inf fail closed at admission.
func TestReviewSamplingNumberBoundary(t *testing.T) {
	mk := func(v any) map[string]any {
		return map[string]any{"m": map[string]any{"sampling": map[string]any{"temperature": map[string]any{"default": v}}}}
	}
	if _, err := ValidateOverrides(mk(int64(1))); err != nil {
		t.Fatalf("int sampling default: %v", err)
	}
	if _, err := ValidateOverrides(mk(float64(0.7))); err != nil {
		t.Fatalf("float sampling default: %v", err)
	}
	for _, bad := range []any{math.NaN(), math.Inf(1), math.Inf(-1), "hot", true} {
		if _, err := ValidateOverrides(mk(bad)); err == nil {
			t.Fatalf("sampling default %v must be rejected", bad)
		}
	}
}

// Sparse disable suppresses dependents and keeps the withdrawing
// evidence (Python `normalize`): values/defaults become null, sibling
// `.supported` leaves become false, all carrying the override source.
func TestReviewNormalizeSparseDisable(t *testing.T) {
	base := Empty("openai-chat")
	if err := base.Put("reasoning.effort.values", reviewStrings(t, "low", "high"), strPtr("go.review.effort.values"), KindReviewed); err != nil {
		t.Fatal(err)
	}
	if err := base.Put("reasoning.effort.default", StringValue("low"), strPtr("go.review.effort.default"), KindReviewed); err != nil {
		t.Fatal(err)
	}
	if err := base.Put("reasoning.effort.supported", BoolValue(true), strPtr("go.review.effort.supported"), KindReviewed); err != nil {
		t.Fatal(err)
	}
	got := reviewAdmit(t, map[string]any{
		"m": map[string]any{"reasoning": map[string]any{"effort": map[string]any{"supported": false}}},
	})
	rendered := Render(base, got["m"], StaticDiscovery(), 1000)
	wire := reviewWire(t, rendered)
	vals := reviewLeaf(t, wire, "reasoning.effort.values")
	if vals["value"] != nil {
		t.Fatalf("disabled effort values must withdraw to null: %v", vals)
	}
	def := reviewLeaf(t, wire, "reasoning.effort.default")
	if def["value"] != nil {
		t.Fatalf("disabled effort default must withdraw to null: %v", def)
	}
	if sup := reviewLeaf(t, wire, "reasoning.effort.supported"); sup["value"] != false {
		t.Fatalf("explicit disable stays false: %v", sup)
	}
	// Displaced reviewed evidence is retained under conflicts.
	confs, ok := vals["conflicts"].([]any)
	if !ok || len(confs) == 0 {
		t.Fatalf("conflict evidence must be retained: %v", vals)
	}
	first, _ := confs[0].(map[string]any)
	if first["source"] != "go.review.effort.values" || first["kind"] != KindReviewed {
		t.Fatalf("conflict must carry displaced evidence: %v", first)
	}
}

// Cross-layer relation repair prefers the explicit assertion and
// withdraws the conflicting derived claim with validation.*/conflict
// (Python normalize tail). Within-override breakage is rejected at
// admission instead.
func TestReviewRelationRepairPrefersExplicit(t *testing.T) {
	base := Empty("openai-chat")
	if err := base.Put("reasoning.budget.max_tokens", IntValue(5), strPtr("go.review.x"), KindReviewed); err != nil {
		t.Fatal(err)
	}
	got := reviewAdmit(t, map[string]any{
		"m": map[string]any{"reasoning": map[string]any{"budget": map[string]any{"min_tokens": int64(10)}}},
	})
	rendered := Render(base, got["m"], StaticDiscovery(), 1000)
	wire := reviewWire(t, rendered)
	maxLeaf := reviewLeaf(t, wire, "reasoning.budget.max_tokens")
	if maxLeaf["value"] != nil || maxLeaf["source"] != "validation.budget_range" || maxLeaf["kind"] != KindConflict {
		t.Fatalf("inherited max must withdraw to validation.*/conflict: %v", maxLeaf)
	}
	if minLeaf := reviewLeaf(t, wire, "reasoning.budget.min_tokens"); minLeaf["value"] != float64(10) {
		t.Fatalf("explicit min must survive: %v", minLeaf)
	}
	// Same breakage inside one override is a startup error, never repair.
	_, err := ValidateOverrides(map[string]any{"m": map[string]any{
		"reasoning": map[string]any{"budget": map[string]any{
			"min_tokens": int64(9), "max_tokens": int64(2)}}}})
	if err == nil {
		t.Fatal("min>max inside one override must be a startup error")
	}
	_, err = ValidateOverrides(map[string]any{"m": map[string]any{
		"reasoning": map[string]any{"effort": map[string]any{
			"values": []any{"low"}, "default": "high"}}}})
	if err == nil {
		t.Fatal("effort default outside values must be a startup error")
	}
}

// Freshness math ports `_refresh_status` exactly: single pin, single
// clock, no I/O. Static rows carry null stale/age; sources without an
// observation never derive age/stale even under a stale discovery.
func TestReviewFreshnessTransitions(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	never := NewDiscovery(FreshnessInput{}, 60, 200)
	if s := never.Stale(); s == nil || !*s {
		t.Fatalf("never-succeeded must be stale: %v", s)
	}
	if got := reviewWire(t, Render(Empty("openai-chat"), nil, never, 999))["freshness"]; got == nil {
		t.Fatal("freshness block must exist")
	}
	okAt := NewDiscovery(FreshnessInput{LastSuccess: f(100)}, 60, 130)
	if s := okAt.Stale(); s == nil || *s {
		t.Fatalf("fresh success must not be stale: %v", s)
	}
	aged := NewDiscovery(FreshnessInput{LastSuccess: f(100)}, 60, 200)
	if s := aged.Stale(); s == nil || !*s {
		t.Fatalf("aged past interval must be stale: %v", s)
	}
	errd := NewDiscovery(FreshnessInput{LastSuccess: f(190), Err: "boom"}, 60, 200)
	if s := errd.Stale(); s == nil || !*s {
		t.Fatalf("errored source must be stale: %v", s)
	}
	future := NewDiscovery(FreshnessInput{LastSuccess: f(300)}, 60, 200)
	wire := reviewWire(t, Render(Empty("openai-chat"), nil, future, 999))
	disc := wire["freshness"].(map[string]any)["discovery"].(map[string]any)
	if disc["age_seconds"] != float64(0) {
		t.Fatalf("future success clamps age to 0: %v", disc)
	}
	static := StaticDiscovery()
	if s := static.Stale(); s != nil {
		t.Fatalf("static stale must be null: %v", s)
	}
	rw := reviewWire(t, Render(Empty("openai-chat"), nil, static, 999))
	disc, _ = rw["freshness"].(map[string]any)["discovery"].(map[string]any)
	if disc["stale"] != nil || disc["age_seconds"] != nil {
		t.Fatalf("static discovery must be null/null: %v", disc)
	}
	// First slice sets no retrieved_at: per-source age/stale stay null
	// even when the discovery block itself is stale.
	srcs := reviewWire(t, Render(Empty("openai-chat"), nil, aged, 999))["sources"].(map[string]any)
	for name, s := range srcs {
		m := s.(map[string]any)
		if m["age_seconds"] != nil || m["stale"] != nil {
			t.Fatalf("unobserved source %q must derive null age/stale: %v", name, m)
		}
	}
}

// Render never mutates its inputs: re-rendering the same base plus the
// same overrides is stable, and mutating a rendered contract cannot move
// later reports.
func TestReviewRenderInputIsolation(t *testing.T) {
	base := Empty("anthropic")
	got := reviewAdmit(t, map[string]any{
		"m": map[string]any{"tools": map[string]any{"supported": true}}})
	first, err := json.Marshal(Render(base, got["m"], StaticDiscovery(), 1000))
	if err != nil {
		t.Fatal(err)
	}
	var probe Contract = Render(base, got["m"], StaticDiscovery(), 1000)
	probeMustPut(t, &probe, "tools.supported", BoolValue(false))
	second, err := json.Marshal(Render(base, got["m"], StaticDiscovery(), 1000))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("render must be stable across output mutation:\n%s\n%s", first, second)
	}
	if v, _ := base.Get("tools.supported"); !v.IsNull() {
		t.Fatal("base must not gain the override")
	}
	if _, ok := got["m"]["tools.supported"]; !ok {
		t.Fatal("overrides map must be intact")
	}
}

// Returned values are caller-owned copies: mutating a fetched string
// list or native mapping cannot move stored facts.
func TestReviewReturnedValueIsolation(t *testing.T) {
	var c Contract = Empty("openai-chat")
	np, ok := NewNativeParam("openai-chat", "p", map[string]NativeScalar{"k": StringScalar("v")})
	if !ok {
		t.Fatal("native constructor")
	}
	if err := c.Put("sampling.temperature.native_parameter", NativeValue(np), strPtr("t"), KindOverride); err != nil {
		t.Fatal(err)
	}
	v, ok := c.Get("sampling.temperature.native_parameter")
	if !ok {
		t.Fatal("get native")
	}
	got, ok := v.Native()
	if !ok {
		t.Fatal("native payload")
	}
	mut := got.Values()
	mut["k"] = StringScalar("MUT")
	mut["new"] = BoolScalar(true)
	again, _ := c.Get("sampling.temperature.native_parameter")
	np2, _ := again.Native()
	if s, _ := np2.Values()["k"].String(); s != "v" {
		t.Fatalf("stored native mapping moved: %v", np2.Values())
	}
	if _, has := np2.Values()["new"]; has {
		t.Fatal("stored native mapping gained a key")
	}
}

// Overrides admit only validated values, so ApplyOverrides cannot fail
// on admitted input; unknown fields can never be constructed through
// the admission boundary.
func TestReviewOverridesTrustBoundary(t *testing.T) {
	if !IsField("limits.context_window") {
		t.Fatal("typed limit leaves must be addressable")
	}
	if IsField("limits.bogus") || IsField("reasoning.nope") {
		t.Fatal("unknown paths must stay outside the vocabulary")
	}
	for _, p := range FieldPaths() {
		if strings.Contains(p, "limits.") {
			t.Fatalf("limits must not leak into the fact vocabulary: %q", p)
		}
	}
	// The TOML boundary rejects the "limits" table prefix exactly like
	// Python `_flatten` (FIELDS has no limits.* entries).
	if _, err := ValidateOverrides(map[string]any{
		"m": map[string]any{"limits": map[string]any{"context_window": int64(1)}}}); err == nil {
		t.Fatal("limits.* overrides must be rejected like the Python source")
	}
}

// Concurrent renders share nothing: one base plus one admitted set under
// -race must stay stable and mutation-free.
func TestReviewRenderRace(t *testing.T) {
	base := Empty("gemini")
	got := reviewAdmit(t, map[string]any{
		"m": map[string]any{"streaming": true,
			"sampling": map[string]any{"temperature": map[string]any{"default": float64(0.7)}}}})
	want, _ := json.Marshal(Render(base, got["m"], StaticDiscovery(), 1000))
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := json.Marshal(Render(base, got["m"], StaticDiscovery(), 1000))
			if err != nil {
				t.Error(err)
				return
			}
			if string(out) != string(want) {
				t.Errorf("render drift: %s", out)
			}
		}()
	}
	wg.Wait()
	if v, _ := base.Get("streaming"); !v.IsNull() {
		t.Fatal("base mutated under concurrent render")
	}
}

func reviewStrings(t *testing.T, ss ...string) Value {
	t.Helper()
	v, ok := StringListValue(ss)
	if !ok {
		t.Fatalf("mustStrings(%v)", ss)
	}
	return v
}

func probeMustPut(t *testing.T, c *Contract, path string, v Value) {
	t.Helper()
	if err := c.Put(path, v, strPtr("probe"), KindOverride); err != nil {
		t.Fatalf("probe put %s: %v", path, err)
	}
}
