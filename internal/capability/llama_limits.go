// Llama limit policy: explicit operator facts layered between observed
// evidence and the runtime mapper.
//
// Types first: LlamaLimitPolicy is the closed operator policy (fallback
// plus per-upstream overrides). Callers: ApplyLlamaLimits ports the
// observed _llama_limits tail (fallback only when unknown, overrides win)
// with closed KindFallback/KindOverride. No arbitrary map facts, no I/O,
// no clock, no registry. Publication order is ObservedLlama ->
// ApplyLlamaLimits -> ApplyLlamaMapper -> canonical overrides ->
// Normalize; the same atomic routes/time/snapshot carries all three.
package capability

import "sort"

// Llama limit provenance (exact wire strings).
const (
	LlamaFallbackSource = "llama_cpp.context_fallback"
	LlamaOverrideSource = "llama_cpp.limit_overrides"
)

// LlamaLimitPolicy is the trusted explicit limit policy. Fallback nil
// means absent (unknown, never an implicit default). Overrides keys are
// raw upstream IDs; inner nil is an explicit null withdraw.
type LlamaLimitPolicy struct {
	Fallback  *int64
	Overrides map[string]map[string]*int64
}

// NewLlamaLimitPolicy deep-copies the trusted config policy. Nil stays nil;
// empty overrides stay nil (admitted no-op).
func NewLlamaLimitPolicy(fallback *int64, overrides map[string]map[string]*int64) *LlamaLimitPolicy {
	p := &LlamaLimitPolicy{}
	if fallback != nil {
		v := *fallback
		p.Fallback = &v
	}
	if len(overrides) > 0 {
		p.Overrides = make(map[string]map[string]*int64, len(overrides))
		for model, fields := range overrides {
			cp := make(map[string]*int64, len(fields))
			for k, v := range fields {
				if v == nil {
					cp[k] = nil
				} else {
					nv := *v
					cp[k] = &nv
				}
			}
			p.Overrides[model] = cp
		}
	}
	return p
}

// Clone deep-copies the policy (nil stays nil).
func (p *LlamaLimitPolicy) Clone() *LlamaLimitPolicy {
	if p == nil {
		return nil
	}
	return NewLlamaLimitPolicy(p.Fallback, p.Overrides)
}

// Empty reports whether the policy carries no explicit facts.
func (p *LlamaLimitPolicy) Empty() bool {
	return p == nil || (p.Fallback == nil && len(p.Overrides) == 0)
}

// Covers reports whether the policy carries any explicit fact for one
// raw upstream ID: a fallback covers every model, an override covers its
// exact upstream key.
func (p *LlamaLimitPolicy) Covers(upstream string) bool {
	if p == nil {
		return false
	}
	if p.Fallback != nil {
		return true
	}
	if len(p.Overrides) == 0 {
		return false
	}
	_, ok := p.Overrides[upstream]
	return ok
}

// ApplyLlamaLimits layers explicit fallback/overrides onto a private base
// built from observed evidence. Fallback applies only when
// limits.context_window is null; overrides match the raw upstream ID
// exactly and replace the leaf wholesale (override 0 and override null
// both clear any observed -1/-2 special). Conflicts preserve per the
// existing put semantics.
func ApplyLlamaLimits(c *Contract, upstream string, policy *LlamaLimitPolicy) {
	if c == nil || policy == nil {
		return
	}
	if policy.Fallback != nil {
		if v, ok := c.Get("limits.context_window"); ok && v.IsNull() {
			c.put("limits.context_window", IntValue(*policy.Fallback), strPtr(LlamaFallbackSource), KindFallback)
		}
	}
	ov, ok := policy.Overrides[upstream]
	if !ok {
		return
	}
	keys := make([]string, 0, len(ov))
	for k := range ov {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := ov[k]
		var val Value
		if v == nil {
			val = Null()
		} else {
			val = IntValue(*v)
		}
		c.put("limits."+k, val, strPtr(LlamaOverrideSource), KindOverride)
	}
}
