// Boundary sketch (U1-U4 first slice, read path only):
//
//	Owns: the closed capability value domain, the closed FIELDS/LIMITS
//	vocabulary, sparse override validation (flatten/validate/apply/
//	check-relations/normalize) and the pure render (deep-copy base, apply
//	overrides, normalize, attach freshness.discovery, derive source
//	age/stale at read time). Inputs: an effective-dialect base from
//	Empty, trusted admitted Overrides from config, one typed Discovery
//	per route, one wall timestamp per report. Outputs: a fresh immutable
//	Contract for one listing row (wire JSON: schema_version/sources/
//	nested facts/limits/freshness). No network, filesystem, clock, store
//	lock, worker or registry access here; callers pin one snapshot plus
//	one Clock.Sample. Never mutates its inputs: Clone on entry, copy on
//	put/render. Does NOT own: go/llama acquisition constructors, effort
//	admission, generation/transport/session/persistence, display-name
//	enrichment, cache projection (route-only v2 persists no facts).
//
//	Closed-domain rules: semantic storage never holds map[string]any,
//	any, or reflection. Raw map[string]any exists ONLY at the TOML
//	boundary inside ValidateOverrides/flatten; map[string]any exists ONLY
//	as the final wire-serialization form in render.go. Source timestamps
//	are an explicit optional typed wall time (first slice: never set, so
//	ages stay null); no ISO parsing at runtime.
package capability

import (
	"math"
	"sort"
)

// --- closed vocabularies (private; accessors return copies) ---

var protocols = []string{"openai-chat", "openai-responses", "anthropic", "gemini"}

var modalities = []string{"text", "image", "audio", "video", "pdf", "embedding"}

var sampling = []string{
	"temperature", "top_p", "top_k", "min_p", "typical_p", "seed",
	"frequency_penalty", "presence_penalty", "repeat_penalty", "repeat_last_n",
	"dry_multiplier", "dry_base", "dry_allowed_length", "dry_penalty_last_n",
	"mirostat", "mirostat_tau", "mirostat_eta", "dynatemp_range", "dynatemp_exponent",
	"xtc_probability", "xtc_threshold", "samplers",
	"top_n_sigma", "adaptive_target", "adaptive_decay", "dry_sequence_breakers",
}

var limits = []string{"context_window", "trained_context_window", "max_input_tokens", "max_output_tokens", "default_output_tokens"}

// Protocols returns the four routed dialect names (a copy).
func Protocols() []string { return append([]string(nil), protocols...) }

// Modalities returns the frozen modality vocabulary (a copy).
func Modalities() []string { return append([]string(nil), modalities...) }

// SamplingKeys returns the frozen sampling-key vocabulary (a copy).
func SamplingKeys() []string { return append([]string(nil), sampling...) }

// LimitKeys returns the typed limit keys (a copy).
func LimitKeys() []string { return append([]string(nil), limits...) }

// --- closed value representation ---
//
// Every storable leaf is one of: null (unknown or explicitly withdrawn),
// bool, exact int64, finite float64, string, a unique-nonempty-element
// string list (empty list allowed), or a typed NativeParam whose mapping
// values are primitives only (string may be empty, per the Python source
// validator; empty mapping keys are also accepted to match the source).

type valueTag uint8

const (
	tagNull valueTag = iota
	tagBool
	tagInt
	tagFloat
	tagString
	tagStrings
	tagNative
)

// Value is one closed leaf value. The zero value is null.
type Value struct {
	tag valueTag
	b   bool
	i   int64
	f   float64
	s   string
	ss  []string
	np  NativeParam
}

// Null returns the explicit-null value (renders as JSON null).
func Null() Value { return Value{tag: tagNull} }

// BoolValue stores a boolean.
func BoolValue(b bool) Value { return Value{tag: tagBool, b: b} }

// IntValue stores an exact int64 (never float-roundtripped).
func IntValue(i int64) Value { return Value{tag: tagInt, i: i} }

// FloatValue stores f only when finite.
func FloatValue(f float64) (Value, bool) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return Value{}, false
	}
	return Value{tag: tagFloat, f: f}, true
}

// StringValue stores s verbatim; field validators decide whether empty is
// admitted (effort.default requires nonempty; native mapping values allow
// empty per the source validator).
func StringValue(s string) Value { return Value{tag: tagString, s: s} }

// StringListValue stores a copy of ss when every element is nonempty and
// unique. The empty (or nil) list is valid, matching Python _strings.
func StringListValue(ss []string) (Value, bool) {
	seen := make(map[string]bool, len(ss))
	for _, e := range ss {
		if e == "" || seen[e] {
			return Value{}, false
		}
		seen[e] = true
	}
	return Value{tag: tagStrings, ss: append([]string(nil), ss...)}, true
}

// NativeValue stores a validated native parameter descriptor.
func NativeValue(np NativeParam) Value { return Value{tag: tagNative, np: np.clone()} }

// IsNull reports whether v is null (unknown or withdrawn).
func (v Value) IsNull() bool { return v.tag == tagNull }

// IsFalse reports whether v is an explicit false (never null).
func (v Value) IsFalse() bool { return v.tag == tagBool && !v.b }

// Tag names the stored variant for diagnostics (never a value).
func (v Value) Tag() string {
	switch v.tag {
	case tagBool:
		return "bool"
	case tagInt:
		return "int"
	case tagFloat:
		return "float"
	case tagString:
		return "string"
	case tagStrings:
		return "strings"
	case tagNative:
		return "native"
	default:
		return "null"
	}
}

// Bool returns the boolean payload.
func (v Value) Bool() (bool, bool) {
	if v.tag != tagBool {
		return false, false
	}
	return v.b, true
}

// Int returns the exact int64 payload.
func (v Value) Int() (int64, bool) {
	if v.tag != tagInt {
		return 0, false
	}
	return v.i, true
}

// Float returns the finite float64 payload.
func (v Value) Float() (float64, bool) {
	if v.tag != tagFloat {
		return 0, false
	}
	return v.f, true
}

// String returns the string payload.
func (v Value) String() (string, bool) {
	if v.tag != tagString {
		return "", false
	}
	return v.s, true
}

// StringList returns a copy of the string-list payload.
func (v Value) StringList() ([]string, bool) {
	if v.tag != tagStrings {
		return nil, false
	}
	return append([]string(nil), v.ss...), true
}

// Native returns a copy of the native-parameter payload.
func (v Value) Native() (NativeParam, bool) {
	if v.tag != tagNative {
		return NativeParam{}, false
	}
	return v.np.clone(), true
}

// Equal compares two values by content.
func (v Value) Equal(o Value) bool {
	if v.tag != o.tag {
		return false
	}
	switch v.tag {
	case tagBool:
		return v.b == o.b
	case tagInt:
		return v.i == o.i
	case tagFloat:
		return v.f == o.f
	case tagString:
		return v.s == o.s
	case tagStrings:
		if len(v.ss) != len(o.ss) {
			return false
		}
		for i := range v.ss {
			if v.ss[i] != o.ss[i] {
				return false
			}
		}
		return true
	case tagNative:
		return v.np.equal(o.np)
	default:
		return true
	}
}

func (v Value) clone() Value {
	out := v
	if v.ss != nil {
		out.ss = append([]string(nil), v.ss...)
	}
	if v.tag == tagNative {
		out.np = v.np.clone()
	}
	return out
}

// --- typed native-parameter primitives ---

type scalarTag uint8

const (
	scalarString scalarTag = iota
	scalarBool
	scalarInt
)

// NativeScalar is one primitive native-mapping value: a string (empty
// allowed, per the Python source validator), a bool, or an exact int64.
type NativeScalar struct {
	tag scalarTag
	s   string
	b   bool
	i   int64
}

// StringScalar stores a mapping string (empty allowed).
func StringScalar(s string) NativeScalar { return NativeScalar{tag: scalarString, s: s} }

// BoolScalar stores a mapping bool.
func BoolScalar(b bool) NativeScalar { return NativeScalar{tag: scalarBool, b: b} }

// IntScalar stores a mapping int exactly.
func IntScalar(i int64) NativeScalar { return NativeScalar{tag: scalarInt, i: i} }

// Tag names the scalar variant.
func (s NativeScalar) Tag() string {
	switch s.tag {
	case scalarBool:
		return "bool"
	case scalarInt:
		return "int"
	default:
		return "string"
	}
}

// String returns the string payload.
func (s NativeScalar) String() (string, bool) {
	if s.tag != scalarString {
		return "", false
	}
	return s.s, true
}

// Bool returns the bool payload.
func (s NativeScalar) Bool() (bool, bool) {
	if s.tag != scalarBool {
		return false, false
	}
	return s.b, true
}

// Int returns the int64 payload.
func (s NativeScalar) Int() (int64, bool) {
	if s.tag != scalarInt {
		return 0, false
	}
	return s.i, true
}

func (s NativeScalar) equal(o NativeScalar) bool {
	if s.tag != o.tag {
		return false
	}
	switch s.tag {
	case scalarBool:
		return s.b == o.b
	case scalarInt:
		return s.i == o.i
	default:
		return s.s == o.s
	}
}

// NativeParam is the typed native-parameter descriptor. Mapping keys are
// unrestricted strings (empty keys accepted to match the Python source
// validator, which only requires isinstance(k, str)); mapping values are
// primitives only. A nil values map means absent (no "values" on the wire);
// a non-nil empty map renders as {}.
type NativeParam struct {
	protocol string
	path     string
	values   map[string]NativeScalar
}

// NewNativeParam validates protocol membership and a nonempty path.
// values is deep-copied (nil stays nil).
func NewNativeParam(protocol, path string, values map[string]NativeScalar) (NativeParam, bool) {
	if !isValidDialect(protocol) || path == "" {
		return NativeParam{}, false
	}
	var cp map[string]NativeScalar
	if values != nil {
		cp = make(map[string]NativeScalar, len(values))
		for k, v := range values {
			cp[k] = v
		}
	}
	return NativeParam{protocol: protocol, path: path, values: cp}, true
}

// Protocol returns the descriptor protocol.
func (n NativeParam) Protocol() string { return n.protocol }

// Path returns the descriptor path.
func (n NativeParam) Path() string { return n.path }

// Values returns a copy of the mapping (nil when absent).
func (n NativeParam) Values() map[string]NativeScalar {
	if n.values == nil {
		return nil
	}
	cp := make(map[string]NativeScalar, len(n.values))
	for k, v := range n.values {
		cp[k] = v
	}
	return cp
}

func (n NativeParam) clone() NativeParam {
	var cp map[string]NativeScalar
	if n.values != nil {
		cp = make(map[string]NativeScalar, len(n.values))
		for k, v := range n.values {
			cp[k] = v
		}
	}
	return NativeParam{protocol: n.protocol, path: n.path, values: cp}
}

func (n NativeParam) equal(o NativeParam) bool {
	if n.protocol != o.protocol || n.path != o.path || len(n.values) != len(o.values) {
		return false
	}
	for k, v := range n.values {
		ov, ok := o.values[k]
		if !ok || !v.equal(ov) {
			return false
		}
	}
	return true
}

// --- closed field vocabulary ---

// fieldExpect names the admitted value shape for one known path.
type fieldExpect uint8

const (
	expectBool fieldExpect = iota
	expectNonNegInt
	expectNonEmptyString
	expectStrings
	expectNative
	expectNumber
	expectLimitInt
)

var fieldTable map[string]fieldExpect

func init() {
	fieldTable = map[string]fieldExpect{}
	add := func(path string, e fieldExpect) { fieldTable[path] = e }
	add("reasoning.supported", expectBool)
	add("reasoning.toggle.supported", expectBool)
	add("reasoning.toggle.default", expectBool)
	add("reasoning.toggle.native_parameter", expectNative)
	add("reasoning.effort.supported", expectBool)
	add("reasoning.effort.values", expectStrings)
	add("reasoning.effort.default", expectNonEmptyString)
	add("reasoning.effort.native_parameter", expectNative)
	add("reasoning.budget.supported", expectBool)
	add("reasoning.budget.native_parameter", expectNative)
	for _, k := range []string{"min_tokens", "max_tokens", "default_tokens"} {
		add("reasoning.budget."+k, expectNonNegInt)
	}
	add("tools.supported", expectBool)
	add("tools.parallel", expectBool)
	add("structured_output.supported", expectBool)
	add("structured_output.json_object", expectBool)
	add("structured_output.json_schema", expectBool)
	add("streaming", expectBool)
	add("protocols.known_lossy_features", expectStrings)
	for _, dir := range []string{"input", "output"} {
		for _, mod := range modalities {
			add("modalities."+dir+"."+mod, expectBool)
		}
	}
	for _, p := range protocols {
		add("protocols.native."+p, expectBool)
		add("protocols.translated."+p, expectBool)
	}
	for _, key := range sampling {
		add("sampling."+key+".supported", expectBool)
		if key == "samplers" || key == "dry_sequence_breakers" {
			add("sampling."+key+".default", expectStrings)
		} else {
			add("sampling."+key+".default", expectNumber)
		}
		add("sampling."+key+".native_parameter", expectNative)
		add("sampling."+key+".constraints", expectStrings)
	}
}

// IsField reports whether path is in the closed vocabulary (limits.*
// included for the typed limit leaves).
func IsField(path string) bool {
	if _, ok := fieldTable[path]; ok {
		return true
	}
	if len(path) > len("limits.") && path[:len("limits.")] == "limits." {
		for _, k := range limits {
			if path[len("limits."):] == k {
				return true
			}
		}
	}
	return false
}

// FieldPaths returns the sorted closed leaf vocabulary (limits excluded;
// see LimitKeys).
func FieldPaths() []string {
	out := make([]string, 0, len(fieldTable))
	for p := range fieldTable {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func hasFieldPrefix(path string) bool {
	for p := range fieldTable {
		if len(p) > len(path) && p[:len(path)] == path && p[len(path)] == '.' {
			return true
		}
	}
	return false
}

// validForField ports the Python per-field validators exactly: null is
// always admitted (explicit withdraw); otherwise the value shape must match
// the field. _boolean is exact-bool; budget ints are non-negative int64
// (bools never satisfy an int field); effort.default is a nonempty string;
// _strings elements are nonempty unique strings (empty list allowed);
// _number is an exact int64 or a finite float64; _native is a validated
// descriptor. Limits leaves admit exact int64 (positivity is the deferred
// acquisition's concern, mirroring Python's skip-not-fail review filter).
func validForField(path string, v Value) bool {
	if v.tag == tagNull {
		return true
	}
	if len(path) > len("limits.") && path[:len("limits.")] == "limits." {
		for _, k := range limits {
			if path[len("limits."):] == k {
				return v.tag == tagInt
			}
		}
		return false
	}
	e, ok := fieldTable[path]
	if !ok {
		return false
	}
	switch e {
	case expectBool:
		return v.tag == tagBool
	case expectNonNegInt:
		return v.tag == tagInt && v.i >= 0
	case expectNonEmptyString:
		return v.tag == tagString && v.s != ""
	case expectStrings:
		return v.tag == tagStrings
	case expectNative:
		return v.tag == tagNative
	case expectNumber:
		return v.tag == tagInt || v.tag == tagFloat
	default:
		return false
	}
}

func isValidDialect(d string) bool {
	for _, p := range protocols {
		if p == d {
			return true
		}
	}
	return false
}
