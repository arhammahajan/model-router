package capability

// Closed-domain tests. Construction uses the typed API (constructors,
// validated Put, admitted Overrides); every behavioral assertion stays on
// the wire JSON so the Python contract shape is locked. Raw TOML-boundary
// literals use go-toml's decoded types only (map[string]any, []any,
// string, bool, int64, float64): narrower Go types (int, []string, ...)
// never occur from TOML and are fail-closed at admission.

import (
	"encoding/json"
	"math"
	"strings"
	"sync"
	"testing"
)

func mustJSON(t *testing.T, c Contract) map[string]any {
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

func mustNumbered(t *testing.T, c Contract) map[string]any {
	t.Helper()
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func leaf(t *testing.T, root map[string]any, path string) map[string]any {
	t.Helper()
	cur := map[string]any(root)
	parts := strings.Split(path, ".")
	for i, p := range parts {
		next, ok := cur[p].(map[string]any)
		if !ok {
			t.Fatalf("missing path %s", path)
		}
		if i == len(parts)-1 {
			return next
		}
		cur = next
	}
	return cur
}

func mustStrings(t *testing.T, ss []string) Value {
	t.Helper()
	v, ok := StringListValue(ss)
	if !ok {
		t.Fatalf("invalid string list %q", ss)
	}
	return v
}

func mustNative(t *testing.T, protocol, path string, values map[string]NativeScalar) Value {
	t.Helper()
	np, ok := NewNativeParam(protocol, path, values)
	if !ok {
		t.Fatalf("invalid native param %q %q", protocol, path)
	}
	return NativeValue(np)
}

func mustPut(t *testing.T, c *Contract, path string, v Value, source, kind string) {
	t.Helper()
	if err := c.Put(path, v, &source, kind); err != nil {
		t.Fatalf("Put(%s): %v", path, err)
	}
}

func TestGoldenUnknownShape(t *testing.T) {
	c := Empty("openai-chat")
	out := mustJSON(t, c)
	if out["schema_version"] != float64(1) {
		t.Fatalf("schema_version: %v", out["schema_version"])
	}
	for _, p := range []string{"reasoning.supported", "tools.supported", "streaming", "modalities.input.text"} {
		fact := leaf(t, out, p)
		if fact["value"] != nil || fact["source"] != nil || fact["kind"] != "unknown" {
			t.Fatalf("%s not unknown: %v", p, fact)
		}
	}
	// Configured transport claims.
	native := out["protocols"].(map[string]any)["native"].(map[string]any)["openai-chat"].(map[string]any)
	if native["value"] != true || native["kind"] != "configured" {
		t.Fatalf("native dialect claim: %v", native)
	}
	trans := out["protocols"].(map[string]any)["translated"].(map[string]any)
	for _, p := range Protocols() {
		f := trans[p].(map[string]any)
		want := p != "openai-chat"
		if f["value"] != want {
			t.Fatalf("translated %s: %v", p, f)
		}
	}
}

func TestFalseVsNull(t *testing.T) {
	base := Empty("openai-chat")
	rendered := Render(base, Overrides{"reasoning.supported": BoolValue(false)}, StaticDiscovery(), 1000)
	out := mustJSON(t, rendered)
	rs := out["reasoning"].(map[string]any)["supported"].(map[string]any)
	if rs["value"] != false || rs["kind"] != "override" {
		t.Fatalf("explicit false: %v", rs)
	}
	// Unknown stays null, never false.
	if out["tools"].(map[string]any)["supported"].(map[string]any)["value"] != nil {
		t.Fatalf("unknown must be null")
	}
	// Explicit null withdraws to unknown.
	base2 := Empty("openai-chat")
	r2 := Render(base2, Overrides{"tools.supported": Null()}, StaticDiscovery(), 1000)
	o2 := mustJSON(t, r2)
	if o2["tools"].(map[string]any)["supported"].(map[string]any)["value"] != nil {
		t.Fatalf("null withdraw must be null")
	}
	if o2["tools"].(map[string]any)["supported"].(map[string]any)["kind"] != "override" {
		t.Fatalf("null withdraw kind: %v", o2["tools"])
	}
}

func TestFullFieldShape(t *testing.T) {
	if len(FieldPaths()) < 100 {
		t.Fatalf("FIELDS too small: %d", len(FieldPaths()))
	}
	// Every sampling key has 4 leaves.
	for _, k := range SamplingKeys() {
		for _, lf := range []string{"supported", "default", "native_parameter", "constraints"} {
			if !IsField("sampling." + k + "." + lf) {
				t.Fatalf("missing sampling.%s.%s", k, lf)
			}
		}
	}
	for _, k := range LimitKeys() {
		if k == "" || !IsField("limits."+k) {
			t.Fatalf("bad limit %q", k)
		}
	}
	if IsField("nope.supported") || IsField("reasoning") || IsField("") {
		t.Fatalf("unknown paths must not validate")
	}
	c := Empty("anthropic")
	out := mustJSON(t, c)
	if _, ok := out["limits"]; !ok {
		t.Fatalf("limits missing")
	}
	if _, ok := out["freshness"]; ok {
		t.Fatalf("pre-render must have no freshness")
	}
	r := Render(c, Overrides{}, StaticDiscovery(), 1000)
	ro := mustJSON(t, r)
	if _, ok := ro["freshness"]; !ok {
		t.Fatalf("post-render must have freshness")
	}
	disc := ro["freshness"].(map[string]any)["discovery"].(map[string]any)
	if disc["stale"] != nil {
		t.Fatalf("static stale must be null")
	}
	if disc["age_seconds"] != nil {
		t.Fatalf("static age must be null")
	}
	if len(disc) != 2 {
		t.Fatalf("static discovery must carry only stale/age_seconds: %v", disc)
	}
}

func TestOverrideBoundaries(t *testing.T) {
	// Narrow Go types never occur from TOML (go-toml decodes integers as
	// int64, arrays as []any): they are fail-closed at admission.
	narrowInt := map[string]any{"m": map[string]any{"reasoning": map[string]any{"budget": map[string]any{"min_tokens": 10}}}}
	if _, err := ValidateOverrides(narrowInt); err == nil {
		t.Fatalf("non-TOML int must reject")
	}
	narrowList := map[string]any{"m": map[string]any{"reasoning": map[string]any{"effort": map[string]any{"values": []string{"low"}}}}}
	if _, err := ValidateOverrides(narrowList); err == nil {
		t.Fatalf("non-TOML []string must reject")
	}
	bad := []struct {
		name string
		raw  map[string]any
	}{
		{"unknown", map[string]any{"m": map[string]any{"nope": map[string]any{"supported": true}}}},
		{"dotted", map[string]any{"m": map[string]any{"reasoning.supported": true}}},
		{"empty", map[string]any{"m": map[string]any{}}},
		{"badtype", map[string]any{"m": map[string]any{"reasoning": map[string]any{"supported": "yes"}}}},
		{"badnative", map[string]any{"m": map[string]any{"reasoning": map[string]any{"toggle": map[string]any{"native_parameter": map[string]any{"protocol": "nope", "path": "x"}}}}}},
	}
	for _, tc := range bad {
		_, err := ValidateOverrides(tc.raw)
		if err == nil {
			t.Fatalf("%s must reject", tc.name)
		}
		if strings.Contains(err.Error(), "yes") || strings.Contains(err.Error(), "nope.supported: true") {
			t.Fatalf("error carries value: %v", err)
		}
	}
	// Broken budget relation.
	_, err := ValidateOverrides(map[string]any{"m": map[string]any{"reasoning": map[string]any{"budget": map[string]any{"min_tokens": int64(10), "max_tokens": int64(5)}}}})
	if err == nil {
		t.Fatalf("budget relation must reject")
	}
	// Effort default outside values.
	_, err = ValidateOverrides(map[string]any{"m": map[string]any{"reasoning": map[string]any{"effort": map[string]any{"values": []any{"low"}, "default": "high"}}}})
	if err == nil {
		t.Fatalf("effort default must reject")
	}
	// Unknown model names validate without warnings.
	ok, err := ValidateOverrides(map[string]any{"absent-model": map[string]any{"tools": map[string]any{"supported": true}}})
	if err != nil {
		t.Fatalf("unknown names must validate: %v", err)
	}
	got, found := ok["absent-model"]["tools.supported"]
	if !found {
		t.Fatalf("unknown names must validate")
	}
	if b, isBool := got.Bool(); !isBool || !b {
		t.Fatalf("unknown names must validate: %v", got)
	}
	// Untrimmed / blank model keys reject.
	for _, k := range []string{"", "  ", " bad"} {
		if _, err := ValidateOverrides(map[string]any{k: map[string]any{"tools": map[string]any{"supported": true}}}); err == nil {
			t.Fatalf("bad key %q must reject", k)
		}
	}
	// Non-table raw rejects.
	if _, err := ValidateOverrides([]any{"x"}); err == nil {
		t.Fatalf("non-table must reject")
	}
}

// TestPythonFieldValidatorGoldens ports the Python source validators
// (_boolean/_number/_strings/_native, effort.default, budget ints) case by
// case through the TOML boundary: only go-toml's decoded types admitted.
func TestPythonFieldValidatorGoldens(t *testing.T) {
	cases := []struct {
		name  string
		path  string
		raw   any
		valid bool
	}{
		{"bool-true", "reasoning.supported", true, true},
		{"bool-false", "reasoning.supported", false, true},
		{"bool-int", "reasoning.supported", int64(1), false},
		{"bool-string", "reasoning.supported", "true", false},
		{"bool-null", "reasoning.supported", nil, true},
		{"budget-zero", "reasoning.budget.min_tokens", int64(0), true},
		{"budget-negative", "reasoning.budget.min_tokens", int64(-1), false},
		{"budget-bool", "reasoning.budget.min_tokens", true, false},
		{"budget-float", "reasoning.budget.min_tokens", 1.0, false},
		{"effort-default", "reasoning.effort.default", "low", true},
		{"effort-default-empty", "reasoning.effort.default", "", false},
		{"effort-values-empty", "reasoning.effort.values", []any{}, true},
		{"effort-values-ok", "reasoning.effort.values", []any{"low", "high"}, true},
		{"effort-values-dupe", "reasoning.effort.values", []any{"low", "low"}, false},
		{"effort-values-empty-elem", "reasoning.effort.values", []any{""}, false},
		{"effort-values-nonstr", "reasoning.effort.values", []any{int64(1)}, false},
		{"effort-values-scalar", "reasoning.effort.values", "low", false},
		{"number-int", "sampling.temperature.default", int64(1), true},
		{"number-negative", "sampling.temperature.default", int64(-3), true},
		{"number-float", "sampling.temperature.default", 0.7, true},
		{"number-nan", "sampling.temperature.default", math.NaN(), false},
		{"number-inf", "sampling.temperature.default", math.Inf(1), false},
		{"number-string", "sampling.temperature.default", "x", false},
		{"samplers-list", "sampling.samplers.default", []any{"a"}, true},
		{"samplers-number", "sampling.samplers.default", 0.5, false},
		{"lossy-empty", "protocols.known_lossy_features", []any{}, true},
	}
	for _, tc := range cases {
		raw := map[string]any{"m": map[string]any{}}
		// Build the nested path.
		parts := strings.Split(tc.path, ".")
		cur := raw["m"].(map[string]any)
		for _, p := range parts[:len(parts)-1] {
			next := map[string]any{}
			cur[p] = next
			cur = next
		}
		cur[parts[len(parts)-1]] = tc.raw
		_, err := ValidateOverrides(raw)
		if tc.valid && err != nil {
			t.Fatalf("%s must admit: %v", tc.name, err)
		}
		if !tc.valid && err == nil {
			t.Fatalf("%s must reject", tc.name)
		}
	}
}

func TestTypedLimitLeaves(t *testing.T) {
	// Limits are inadmissible via overrides (no FIELDS under limits.*) but
	// the typed leaf path admits exact int64 for the deferred acquisition
	// units, mirroring Python's skip-not-fail review filter.
	if _, err := ValidateOverrides(map[string]any{"m": map[string]any{"limits": map[string]any{"max_input_tokens": int64(5)}}}); err == nil {
		t.Fatalf("limits must stay inadmissible via overrides")
	}
	if v, ok := typedValueForField("limits.max_input_tokens", int64(5)); !ok || func() bool {
		n, isInt := v.Int()
		return !isInt || n != 5
	}() {
		t.Fatalf("limit leaf must admit int64")
	}
	if _, ok := typedValueForField("limits.max_input_tokens", true); ok {
		t.Fatalf("limit leaf must reject bool")
	}
	if _, ok := typedValueForField("limits.bogus", int64(5)); ok {
		t.Fatalf("unknown limit must reject")
	}
}

func nativeRaw(proto, path string, values any) map[string]any {
	m := map[string]any{"protocol": proto, "path": path}
	if values != nil {
		m["values"] = values
	}
	return m
}

func wrapNative(t *testing.T, raw map[string]any) map[string]any {
	t.Helper()
	return map[string]any{"m": map[string]any{"reasoning": map[string]any{"toggle": map[string]any{"native_parameter": raw}}}}
}

// TestNativeEmptyKeyMatchesPython locks the source-faithful hardening
// choice: Python _native requires isinstance(k, str) but never bool(k), so
// an empty mapping key is valid upstream. We accept it exactly like the
// source instead of inventing a restriction.
func TestNativeEmptyKeyMatchesPython(t *testing.T) {
	admitted, err := ValidateOverrides(wrapNative(t,
		nativeRaw("openai-chat", "reasoning_effort", map[string]any{"": "none", "off": "none"})))
	if err != nil {
		t.Fatalf("empty native key must match Python admission: %v", err)
	}
	got := admitted["m"]["reasoning.toggle.native_parameter"]
	np, ok := got.Native()
	if !ok {
		t.Fatalf("native value shape: %v", got)
	}
	vals := np.Values()
	s, ok := vals[""].String()
	if !ok || s != "none" {
		t.Fatalf("empty key must survive: %v", vals)
	}
	// Genuine violations still reject.
	for name, raw := range map[string]map[string]any{
		"bad-protocol": wrapNative(t, nativeRaw("nope", "x", nil))["m"].(map[string]any),
		"empty-path":   wrapNative(t, nativeRaw("openai-chat", "", nil))["m"].(map[string]any),
		"extra-key":    {"reasoning": map[string]any{"toggle": map[string]any{"native_parameter": map[string]any{"protocol": "openai-chat", "path": "x", "bogus": true}}}},
		"float-value":  wrapNative(t, nativeRaw("openai-chat", "x", map[string]any{"k": 0.5}))["m"].(map[string]any),
		"nil-values":   wrapNative(t, nativeRaw("openai-chat", "x", map[string]any{"k": nil}))["m"].(map[string]any),
	} {
		_ = name
		if _, err := ValidateOverrides(map[string]any{"m": raw}); err == nil {
			t.Fatalf("%s must reject", name)
		}
	}
}

func TestConflictsRetained(t *testing.T) {
	base := Empty("openai-chat")
	// Simulate inherited positive then sparse disable.
	mustPut(t, &base, "reasoning.effort.values", mustStrings(t, []string{"low", "high"}), "inherited", KindReported)
	mustPut(t, &base, "reasoning.effort.default", StringValue("low"), "inherited", KindReported)
	rendered := Render(base, Overrides{"reasoning.supported": BoolValue(false)}, StaticDiscovery(), 1000)
	out := mustJSON(t, rendered)
	effVals := out["reasoning"].(map[string]any)["effort"].(map[string]any)["values"].(map[string]any)
	if effVals["value"] != nil {
		t.Fatalf("effort values must suppress to null: %v", effVals)
	}
	if _, ok := effVals["conflicts"]; !ok {
		t.Fatalf("conflicts must be retained: %v", effVals)
	}
	// Cross-layer budget repair prefers explicit.
	base2 := Empty("openai-chat")
	mustPut(t, &base2, "reasoning.budget.min_tokens", IntValue(10), "inherited", KindReported)
	mustPut(t, &base2, "reasoning.budget.max_tokens", IntValue(20), "inherited", KindReported)
	r2 := Render(base2, Overrides{"reasoning.budget.default_tokens": IntValue(99)}, StaticDiscovery(), 1000)
	o2 := mustJSON(t, r2)
	def := o2["reasoning"].(map[string]any)["budget"].(map[string]any)["default_tokens"].(map[string]any)
	// Explicit default kept, bound withdrawn with conflict evidence.
	if def["value"] != float64(99) {
		t.Fatalf("explicit default kept: %v", def)
	}
	maxF := o2["reasoning"].(map[string]any)["budget"].(map[string]any)["max_tokens"].(map[string]any)
	if maxF["value"] != nil || maxF["kind"] != "conflict" {
		t.Fatalf("bound conflict: %v", maxF)
	}
}

// TestFullContractWireBaseline locks the complete rendered wire shape:
// null/false/list/native/empty-key leaves, conflict evidence, and full
// int64 precision (no float round-trip).
func TestFullContractWireBaseline(t *testing.T) {
	base := Empty("openai-chat")
	mustPut(t, &base, "reasoning.budget.max_tokens", IntValue(20), "inherited", KindReported)
	mustPut(t, &base, "reasoning.effort.values", mustStrings(t, []string{"low", "high"}), "inherited", KindReported)
	mustPut(t, &base, "reasoning.effort.default", StringValue("low"), "inherited", KindReported)
	raw := map[string]any{"m": map[string]any{
		"tools":     map[string]any{"supported": true, "parallel": false},
		"streaming": true,
		"reasoning": map[string]any{
			"budget": map[string]any{"min_tokens": int64(9223372036854775807)},
			"effort": map[string]any{"native_parameter": nativeRaw("openai-chat", "reasoning_effort",
				map[string]any{"off": "none", "": true, "n": int64(3)})},
		},
		"sampling":  map[string]any{"temperature": map[string]any{"supported": true, "default": 0.25, "constraints": []any{}}},
		"protocols": map[string]any{"known_lossy_features": []any{"x"}},
	}}
	admitted, err := ValidateOverrides(raw)
	if err != nil {
		t.Fatal(err)
	}
	rendered := Render(base, admitted["m"], StaticDiscovery(), 1000)
	out := mustNumbered(t, rendered)
	tools := out["tools"].(map[string]any)
	if tools["supported"].(map[string]any)["value"] != true {
		t.Fatalf("tools.supported: %v", tools)
	}
	if tools["parallel"].(map[string]any)["value"] != false {
		t.Fatalf("explicit false must stay false: %v", tools)
	}
	budget := out["reasoning"].(map[string]any)["budget"].(map[string]any)
	if budget["min_tokens"].(map[string]any)["value"] != json.Number("9223372036854775807") {
		t.Fatalf("bigint must not float-roundtrip: %v", budget["min_tokens"])
	}
	if budget["max_tokens"].(map[string]any)["value"] != nil {
		t.Fatalf("withdrawn bound must be null: %v", budget["max_tokens"])
	}
	if _, ok := budget["max_tokens"].(map[string]any)["conflicts"]; !ok {
		t.Fatalf("withdrawn bound keeps evidence: %v", budget["max_tokens"])
	}
	native := out["reasoning"].(map[string]any)["effort"].(map[string]any)["native_parameter"].(map[string]any)["value"].(map[string]any)
	if native["protocol"] != "openai-chat" || native["path"] != "reasoning_effort" {
		t.Fatalf("native shape: %v", native)
	}
	vals := native["values"].(map[string]any)
	if vals["off"] != "none" || vals[""] != true || vals["n"] != json.Number("3") {
		t.Fatalf("native primitive values incl empty key: %v", vals)
	}
	lossy := out["protocols"].(map[string]any)["known_lossy_features"].(map[string]any)
	if got := lossy["value"].([]any); len(got) != 1 || got[0] != "x" {
		t.Fatalf("string list: %v", lossy)
	}
	// Raw JSON bytes still carry the full bigint exactly.
	rawBytes, err := json.Marshal(rendered)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rawBytes), "9223372036854775807") {
		t.Fatalf("bigint missing from wire bytes")
	}
}

func TestTypedSourceWallTime(t *testing.T) {
	mk := func(retrieved, ttl *float64) Contract {
		base := Empty("openai-chat")
		ver := "upstream report"
		base.sources["go.review"] = &Source{retrievedAt: retrieved, staleAfterSeconds: ttl, verification: &ver}
		return base
	}
	f64 := func(f float64) *float64 { v := f; return &v }
	// TTL branch: age 100 >= ttl 60 -> stale true.
	r := Render(mk(f64(900), f64(60)), Overrides{}, StaticDiscovery(), 1000)
	src := mustJSON(t, r)["sources"].(map[string]any)["go.review"].(map[string]any)
	if src["age_seconds"] != float64(100) || src["stale"] != true || src["retrieved_at"] != float64(900) {
		t.Fatalf("wall derivation: %v", src)
	}
	if src["stale_after_seconds"] != float64(60) || src["verification"] != "upstream report" {
		t.Fatalf("provenance: %v", src)
	}
	// Future retrieved_at clamps to zero age.
	r = Render(mk(f64(1200), f64(60)), Overrides{}, StaticDiscovery(), 1000)
	src = mustJSON(t, r)["sources"].(map[string]any)["go.review"].(map[string]any)
	if src["age_seconds"] != float64(0) {
		t.Fatalf("negative age must clamp: %v", src)
	}
	// Discovery fallback branch: no TTL, discovery stale applies.
	disc := NewDiscovery(FreshnessInput{LastSuccess: f64(990), Cached: true}, 10, 1000)
	r = Render(mk(f64(900), nil), Overrides{}, disc, 1000)
	src = mustJSON(t, r)["sources"].(map[string]any)["go.review"].(map[string]any)
	if src["stale"] != true {
		t.Fatalf("discovery stale fallback: %v", src)
	}
	// Unobserved source stays null even with a stale discovery.
	r = Render(mk(nil, nil), Overrides{}, disc, 1000)
	src = mustJSON(t, r)["sources"].(map[string]any)["go.review"].(map[string]any)
	if src["age_seconds"] != nil || src["stale"] != nil {
		t.Fatalf("unobserved must stay null: %v", src)
	}
}

func TestClosedPutRejects(t *testing.T) {
	c := Empty("openai-chat")
	if err := c.Put("nope.supported", BoolValue(true), strPtr("s"), KindOverride); err == nil {
		t.Fatalf("unknown field must reject")
	}
	if err := c.Put("limits.bogus", IntValue(1), strPtr("s"), KindOverride); err == nil {
		t.Fatalf("unknown limit must reject")
	}
	if err := c.Put("reasoning.supported", StringValue("yes"), strPtr("s"), KindOverride); err == nil {
		t.Fatalf("mistyped value must reject")
	}
	if err := c.Put("reasoning.effort.default", StringValue(""), strPtr("s"), KindOverride); err == nil {
		t.Fatalf("empty effort default must reject")
	}
	if err := c.Put("reasoning.supported", BoolValue(true), strPtr("s"), "bogus"); err == nil {
		t.Fatalf("unknown kind must reject")
	}
	if _, ok := NewNativeParam("nope", "x", nil); ok {
		t.Fatalf("unknown protocol must reject")
	}
	if _, ok := NewNativeParam("openai-chat", "", nil); ok {
		t.Fatalf("empty path must reject")
	}
	if _, ok := FloatValue(math.NaN()); ok {
		t.Fatalf("NaN must reject")
	}
	if _, ok := StringListValue([]string{"a", "a"}); ok {
		t.Fatalf("duplicate list must reject")
	}
	if _, ok := StringListValue([]string{""}); ok {
		t.Fatalf("empty element must reject")
	}
	// Valid puts still store with evidence.
	if err := c.Put("tools.supported", BoolValue(true), strPtr("s"), KindOverride); err != nil {
		t.Fatalf("valid put: %v", err)
	}
	v, ok := c.Get("tools.supported")
	if !ok {
		t.Fatalf("known path must read")
	}
	if b, isBool := v.Bool(); !isBool || !b {
		t.Fatalf("stored value: %v", v)
	}
	if _, ok := c.Get("nope.supported"); ok {
		t.Fatalf("unknown path must not read")
	}
}

func TestCopyIsolation(t *testing.T) {
	admitted, err := ValidateOverrides(map[string]any{"m": map[string]any{
		"tools":     map[string]any{"supported": true},
		"reasoning": map[string]any{"effort": map[string]any{"values": []any{"low"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	orig := admitted["m"]
	cp := orig.Clone()
	cp["tools.supported"] = BoolValue(false)
	delete(cp, "reasoning.effort.values")
	if b, _ := orig["tools.supported"].Bool(); !b {
		t.Fatalf("clone must not alias input")
	}
	if _, ok := orig["reasoning.effort.values"]; !ok {
		t.Fatalf("clone delete must not alias input")
	}
	set := CloneOverrideSet(admitted)
	set["m"]["tools.supported"] = BoolValue(false)
	if b, _ := admitted["m"]["tools.supported"].Bool(); !b {
		t.Fatalf("set clone must not alias input")
	}
	// Returned facts are copies: mutating a Get result cannot move storage.
	base := Empty("openai-chat")
	mustPut(t, &base, "reasoning.effort.values", mustStrings(t, []string{"low"}), "inherited", KindReported)
	got, _ := base.Get("reasoning.effort.values")
	ss, _ := got.StringList()
	ss[0] = "mutated"
	got2, _ := base.Get("reasoning.effort.values")
	ss2, _ := got2.StringList()
	if ss2[0] != "low" {
		t.Fatalf("Get must return copies")
	}
	// Source pointers are copied on store.
	psrc := "orig"
	if err := base.Put("tools.supported", BoolValue(true), &psrc, KindOverride); err != nil {
		t.Fatal(err)
	}
	psrc = "mutated"
	rendered := Render(base, Overrides{}, StaticDiscovery(), 1000)
	leafOut := leaf(t, mustJSON(t, rendered), "tools.supported")
	if leafOut["source"] != "orig" {
		t.Fatalf("source pointer must be copied: %v", leafOut)
	}
}

func TestRenderIsolationAndRace(t *testing.T) {
	base := Empty("openai-chat")
	over := Overrides{"tools.supported": BoolValue(true)}
	disc := NewDiscovery(FreshnessInput{LastSuccess: floatPtr(999), Cached: true}, 10, 1000)
	r1 := Render(base, over, disc, 1000)
	// Representative mutations of inputs and outputs must not leak.
	over["tools.supported"] = BoolValue(false)
	r1Clone := r1.Clone()
	_ = r1Clone.Put("tools.supported", BoolValue(false), strPtr("x"), KindOverride)
	r2 := Render(base, Overrides{"tools.supported": BoolValue(true)}, StaticDiscovery(), 1000)
	v, _ := r2.Get("tools.supported")
	if b, _ := v.Bool(); !b {
		t.Fatalf("render mutated input")
	}
	vb, _ := base.Get("tools.supported")
	if !vb.IsNull() {
		t.Fatalf("base mutated")
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = Render(base, Overrides{"tools.supported": BoolValue(true)}, StaticDiscovery(), 1000)
			_, _ = base.Get("tools.supported")
			_ = over.Clone()
		}()
	}
	wg.Wait()
}

func floatPtr(f float64) *float64 { v := f; return &v }
