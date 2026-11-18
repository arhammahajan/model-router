package capability

import (
	"fmt"
	"sort"
	"strings"
)

// Fact kinds (closed provenance vocabulary).
const (
	KindUnknown    = "unknown"
	KindReported   = "reported"
	KindInferred   = "inferred"
	KindConfigured = "configured"
	KindOverride   = "override"
	KindReviewed   = "reviewed"
	KindConflict   = "conflict"
	KindFallback   = "fallback"
	KindConstraint = "constraint"
)

func validKind(k string) bool {
	switch k {
	case KindUnknown, KindReported, KindInferred, KindConfigured,
		KindOverride, KindReviewed, KindConflict, KindFallback, KindConstraint:
		return true
	}
	return false
}

// Fact is one typed leaf: value + provenance + conflict evidence.
// special is the closed default-output marker: nil normally, otherwise
// exactly "unlimited" (-1) or "until_context" (-2).
// Only limits.default_output_tokens may carry it,
// and only with a null value.
type Fact struct {
	value     Value
	source    *string
	kind      string
	conflicts []Fact
	special   *string
}

// LimitSpecialUnlimited renders a -1 default as null with marker unlimited.
const LimitSpecialUnlimited = "unlimited"

// LimitSpecialUntilContext renders a -2 default as null with marker until_context.
const LimitSpecialUntilContext = "until_context"

func (f Fact) clone() Fact {
	out := Fact{value: f.value.clone(), kind: f.kind}
	if f.source != nil {
		v := *f.source
		out.source = &v
	}
	if f.special != nil {
		v := *f.special
		out.special = &v
	}
	if len(f.conflicts) > 0 {
		out.conflicts = make([]Fact, len(f.conflicts))
		for i, e := range f.conflicts {
			out.conflicts[i] = e.clone()
		}
	}
	return out
}

// Contract is the private per-route capability value. All maps are
// unexported: invalid fields, values, or provenance cannot be smuggled in
// except through the validated Put and the admitted Overrides path.
type Contract struct {
	sources          map[string]*Source
	facts            map[string]Fact
	limits           map[string]Fact
	limitConstraints map[string]Fact
	freshness        *Discovery
}

// Empty returns the unknown skeleton plus configured transport claims.
func Empty(dialect string) Contract {
	c := Contract{
		sources: map[string]*Source{},
		facts:   map[string]Fact{},
		limits:  map[string]Fact{},
	}
	for _, path := range FieldPaths() {
		c.facts[path] = Fact{value: Null(), kind: KindUnknown}
	}
	for _, k := range limits {
		c.limits[k] = Fact{value: Null(), kind: KindUnknown}
	}
	if isValidDialect(dialect) {
		c.put("protocols.native."+dialect, BoolValue(true), strPtr("routing.dialect"), KindConfigured)
		for _, p := range protocols {
			c.put("protocols.translated."+p, BoolValue(p != dialect), strPtr("routing.adapters"), KindConfigured)
		}
	}
	ver := "configured transport and adapter paths only"
	c.sources["routing"] = &Source{verification: &ver}
	return c
}

func strPtr(s string) *string { v := s; return &v }

// put stores without validation; trusted callers only (Empty with
// constructor-built values, ApplyOverrides with admitted Overrides,
// Normalize with null/false or carried-over valid values).
func (c *Contract) put(path string, v Value, source *string, kind string) {
	var prev Fact
	isLimit, limitKey := splitLimit(path)
	if isLimit {
		if f, ok := c.limits[limitKey]; ok {
			prev = f
		} else {
			prev = Fact{kind: KindUnknown}
		}
	} else {
		if f, ok := c.facts[path]; ok {
			prev = f
		} else {
			prev = Fact{kind: KindUnknown}
		}
	}
	var srcCopy *string
	if source != nil {
		v := *source
		srcCopy = &v
	}
	result := Fact{value: v.clone(), source: srcCopy, kind: kind}
	var conflicts []Fact
	conflicts = append(conflicts, prev.conflicts...)
	if !prev.value.IsNull() && !prev.value.Equal(v) {
		ev := Fact{value: prev.value.clone(), kind: prev.kind}
		if prev.source != nil {
			s := *prev.source
			ev.source = &s
		}
		conflicts = append(conflicts, ev)
	}
	if len(conflicts) > 0 {
		result.conflicts = conflicts
	}
	if isLimit {
		c.limits[limitKey] = result
		return
	}
	c.facts[path] = result
}

// Put validates field, value, and kind, then stores with conflict evidence
// (including explicit nulls). Unknown fields, mistyped values, and unknown
// kinds are errors, never silent drops.
func (c *Contract) Put(path string, v Value, source *string, kind string) error {
	if !IsField(path) {
		return fmt.Errorf("%s: unknown capability field", path)
	}
	if !validForField(path, v) {
		return fmt.Errorf("%s: invalid capability value", path)
	}
	if !validKind(kind) {
		return fmt.Errorf("%s: invalid capability kind", path)
	}
	c.put(path, v, source, kind)
	return nil
}

func splitLimit(path string) (bool, string) {
	if strings.HasPrefix(path, "limits.") {
		key := strings.TrimPrefix(path, "limits.")
		for _, k := range limits {
			if key == k {
				return true, key
			}
		}
	}
	return false, ""
}

// Get returns the leaf value; ok is false for unknown paths.
func (c *Contract) Get(path string) (Value, bool) {
	if isLimit, key := splitLimit(path); isLimit {
		if f, ok := c.limits[key]; ok {
			return f.value.clone(), true
		}
		return Null(), false
	}
	if f, ok := c.facts[path]; ok {
		return f.value.clone(), true
	}
	return Null(), false
}

func (c *Contract) getInt(path string) (int64, bool) {
	v, ok := c.Get(path)
	if !ok {
		return 0, false
	}
	return v.Int()
}

// CheckRelations validates budget and effort relations.
func (c *Contract) CheckRelations() error {
	lo, loOK := c.getInt("reasoning.budget.min_tokens")
	hi, hiOK := c.getInt("reasoning.budget.max_tokens")
	if loOK && hiOK && lo > hi {
		return fmt.Errorf("reasoning.budget: min_tokens exceeds max_tokens")
	}
	if def, ok := c.getInt("reasoning.budget.default_tokens"); ok {
		if (loOK && def < lo) || (hiOK && def > hi) {
			return fmt.Errorf("reasoning.budget.default_tokens: outside the budget range")
		}
	}
	values, hasValues := c.Get("reasoning.effort.values")
	def, hasDef := c.Get("reasoning.effort.default")
	if !values.IsNull() && !def.IsNull() && hasValues && hasDef {
		if ss, ok := values.StringList(); ok {
			if ds, ok := def.String(); ok {
				found := false
				for _, s := range ss {
					if s == ds {
						found = true
						break
					}
				}
				if !found {
					return fmt.Errorf("reasoning.effort.default: absent from effort values")
				}
			}
		}
	}
	return nil
}

// Normalize ports router_metadata.normalize: an explicit false parent
// suppresses dependent positives, and sparse-override cross-layer conflicts
// resolve toward the explicit assertion with validation.*/conflict on the
// withdrawn leaf.
func (c *Contract) Normalize() {
	roots := []string{"reasoning", "reasoning.toggle", "reasoning.effort", "reasoning.budget", "tools", "structured_output"}
	for _, k := range sampling {
		roots = append(roots, "sampling."+k)
	}
	for _, root := range roots {
		sup, ok := c.facts[root+".supported"]
		if !ok || !sup.value.IsFalse() {
			continue
		}
		for _, path := range FieldPaths() {
			if !strings.HasPrefix(path, root+".") || path == root+".supported" {
				continue
			}
			negative := Null()
			if strings.HasSuffix(path, ".supported") || path == "tools.parallel" ||
				path == "structured_output.json_object" || path == "structured_output.json_schema" {
				negative = BoolValue(false)
			}
			c.put(path, negative, sup.source, sup.kind)
		}
	}
	lo, loOK := c.getInt("reasoning.budget.min_tokens")
	hi, hiOK := c.getInt("reasoning.budget.max_tokens")
	if loOK && hiOK && lo > hi {
		key := "reasoning.budget.min_tokens"
		if c.facts["reasoning.budget.min_tokens"].kind == KindOverride {
			key = "reasoning.budget.max_tokens"
		}
		c.put(key, Null(), strPtr("validation.budget_range"), KindConflict)
	}
	if def, ok := c.getInt("reasoning.budget.default_tokens"); ok {
		lo2, lo2OK := c.getInt("reasoning.budget.min_tokens")
		hi2, hi2OK := c.getInt("reasoning.budget.max_tokens")
		if lo2OK && def < lo2 {
			if c.facts["reasoning.budget.default_tokens"].kind == KindOverride {
				c.put("reasoning.budget.min_tokens", Null(), strPtr("validation.budget_default"), KindConflict)
			} else {
				c.put("reasoning.budget.default_tokens", Null(), strPtr("validation.budget_default"), KindConflict)
			}
		} else if hi2OK && def > hi2 {
			if c.facts["reasoning.budget.default_tokens"].kind == KindOverride {
				c.put("reasoning.budget.max_tokens", Null(), strPtr("validation.budget_default"), KindConflict)
			} else {
				c.put("reasoning.budget.default_tokens", Null(), strPtr("validation.budget_default"), KindConflict)
			}
		}
	}
	values, hasValues := c.Get("reasoning.effort.values")
	def, hasDef := c.Get("reasoning.effort.default")
	if !values.IsNull() && !def.IsNull() && hasValues && hasDef {
		if ss, ok := values.StringList(); ok {
			if ds, ok := def.String(); ok && ds != "" {
				found := false
				for _, s := range ss {
					if s == ds {
						found = true
						break
					}
				}
				if !found {
					if c.facts["reasoning.effort.default"].kind == KindOverride {
						c.put("reasoning.effort.values", Null(), strPtr("validation.effort_default"), KindConflict)
					} else {
						c.put("reasoning.effort.default", Null(), strPtr("validation.effort_default"), KindConflict)
					}
				}
			}
		}
	}
}

// Clone deep-copies the contract.
func (c Contract) Clone() Contract {
	out := Contract{
		sources:          make(map[string]*Source, len(c.sources)),
		facts:            make(map[string]Fact, len(c.facts)),
		limits:           make(map[string]Fact, len(c.limits)),
		limitConstraints: make(map[string]Fact, len(c.limitConstraints)),
	}
	for k, s := range c.sources {
		out.sources[k] = s.clone()
	}
	for k, f := range c.facts {
		out.facts[k] = f.clone()
	}
	for k, f := range c.limits {
		out.limits[k] = f.clone()
	}
	for k, f := range c.limitConstraints {
		out.limitConstraints[k] = f.clone()
	}
	if c.limitConstraints == nil {
		out.limitConstraints = nil
	}
	if c.freshness != nil {
		d := c.freshness.clone()
		out.freshness = &d
	}
	return out
}

// --- sparse override admission ---
//
// ValidateOverrides/flatten are the ONLY entry points that accept raw
// map[string]any: the TOML-decoded boundary shape (tables as
// map[string]any, arrays as []any, scalars as string/bool/int64/float64).
// Any other Go type at a leaf is a startup error, never a silent drop:
// go-toml never produces it, so it can only be a programming error.
// Leaf conversion goes through the same closed validators as Put.

// Overrides is one model's admitted flattened overrides: closed path to
// closed value. Values are already validated for their paths.
type Overrides map[string]Value

// Clone deep-copies the set.
func (o Overrides) Clone() Overrides {
	if o == nil {
		return Overrides{}
	}
	out := make(Overrides, len(o))
	for path, v := range o {
		out[path] = v.clone()
	}
	return out
}

// CloneOverrideSet deep-copies the snapshot-owned override table across the
// config-to-snapshot and snapshot-to-reader ownership boundaries.
func CloneOverrideSet(values map[string]Overrides) map[string]Overrides {
	out := make(map[string]Overrides, len(values))
	for name, fields := range values {
		out[name] = fields.Clone()
	}
	return out
}

// typedValueForField converts one raw TOML leaf to a closed Value for a
// known path (null always admitted). Only TOML-decoded types are accepted.
func typedValueForField(path string, raw any) (Value, bool) {
	if raw == nil {
		return Null(), true
	}
	e, ok := fieldTable[path]
	if !ok {
		if isLimit, _ := splitLimit(path); isLimit {
			if n, ok := raw.(int64); ok {
				return IntValue(n), true
			}
			return Value{}, false
		}
		return Value{}, false
	}
	switch e {
	case expectBool:
		if b, ok := raw.(bool); ok {
			return BoolValue(b), true
		}
	case expectNonNegInt:
		if n, ok := raw.(int64); ok && n >= 0 {
			return IntValue(n), true
		}
	case expectNonEmptyString:
		if s, ok := raw.(string); ok && s != "" {
			return StringValue(s), true
		}
	case expectStrings:
		if arr, ok := raw.([]any); ok {
			ss := make([]string, 0, len(arr))
			for _, item := range arr {
				s, ok := item.(string)
				if !ok {
					return Value{}, false
				}
				ss = append(ss, s)
			}
			return StringListValue(ss)
		}
	case expectNative:
		if m, ok := raw.(map[string]any); ok {
			if np, ok := nativeFromRaw(m); ok {
				return NativeValue(np), true
			}
		}
	case expectNumber:
		switch n := raw.(type) {
		case int64:
			return IntValue(n), true
		case float64:
			return FloatValue(n)
		}
	}
	return Value{}, false
}

// nativeFromRaw ports Python _native exactly: keys subset of
// protocol/path/values, protocol in PROTOCOLS, path a nonempty string,
// values absent or a string-keyed mapping of string/bool/int leaves.
// Empty mapping keys are accepted: Python only requires isinstance(k,
// str), it does not require a truthy key.
func nativeFromRaw(m map[string]any) (NativeParam, bool) {
	for k := range m {
		if k != "protocol" && k != "path" && k != "values" {
			return NativeParam{}, false
		}
	}
	proto, _ := m["protocol"].(string)
	if !isValidDialect(proto) {
		return NativeParam{}, false
	}
	path, ok := m["path"].(string)
	if !ok || path == "" {
		return NativeParam{}, false
	}
	var vals map[string]NativeScalar
	if raw, has := m["values"]; has {
		vm, ok := raw.(map[string]any)
		if !ok {
			return NativeParam{}, false
		}
		vals = make(map[string]NativeScalar, len(vm))
		for k, item := range vm {
			switch n := item.(type) {
			case string:
				vals[k] = StringScalar(n)
			case bool:
				vals[k] = BoolScalar(n)
			case int64:
				vals[k] = IntScalar(n)
			default:
				return NativeParam{}, false
			}
		}
	}
	return NewNativeParam(proto, path, vals)
}

func flatten(raw any, prefix string) (Overrides, error) {
	if prefix != "" {
		if _, ok := fieldTable[prefix]; ok {
			v, ok := typedValueForField(prefix, raw)
			if !ok {
				return nil, fmt.Errorf("%s: invalid capability value", prefix)
			}
			return Overrides{prefix: v}, nil
		}
	}
	m, ok := raw.(map[string]any)
	if !ok || len(m) == 0 {
		if prefix == "" {
			return nil, fmt.Errorf("override: expected a nonempty capability object")
		}
		return nil, fmt.Errorf("%s: expected a nonempty capability object", prefix)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	result := Overrides{}
	for _, key := range keys {
		if strings.Contains(key, ".") {
			return nil, fmt.Errorf("%s: capability keys must be simple field names", prefix)
		}
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		if !IsField(path) && !hasFieldPrefix(path) {
			return nil, fmt.Errorf("%s: unknown capability field", path)
		}
		sub, err := flatten(m[key], path)
		if err != nil {
			return nil, err
		}
		for k, v := range sub {
			result[k] = v
		}
	}
	return result, nil
}

// ValidateOverrides ports router_metadata.validate_overrides: malformed
// tables, bad model keys, unknown fields, dotted keys, empty objects, bad
// value types, and broken relations are startup errors naming the model key
// and path (value-free). Unknown names validate without warnings.
func ValidateOverrides(raw any) (map[string]Overrides, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("capabilities.overrides: must be a table/object")
	}
	if m == nil {
		return map[string]Overrides{}, nil
	}
	result := make(map[string]Overrides, len(m))
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, model := range keys {
		if model == "" || model != strings.TrimSpace(model) {
			return nil, fmt.Errorf("capabilities.overrides: expected exact nonempty public model IDs")
		}
		flat, err := flatten(m[model], "")
		if err != nil {
			return nil, fmt.Errorf("capabilities.overrides[%q]: %v", model, err)
		}
		probe := Empty("openai-chat")
		ApplyOverrides(&probe, flat)
		if err := probe.CheckRelations(); err != nil {
			return nil, fmt.Errorf("capabilities.overrides[%q]: %v", model, err)
		}
		result[model] = flat
	}
	return result, nil
}

// ApplyOverrides ports router_metadata.apply_overrides. Values are
// pre-admitted, so storage cannot fail; the override source marker mirrors
// the Python contract.
func ApplyOverrides(c *Contract, values Overrides) {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, path := range keys {
		src := "override." + path
		c.put(path, values[path], &src, KindOverride)
	}
	if len(values) > 0 {
		ver := "operator assertion"
		c.sources["override"] = &Source{verification: &ver}
	}
}
