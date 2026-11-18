package capability

// ReasoningCapabilities is the closed request-time projection of the
// trusted capability contract for reasoning planning. Nil booleans mean
// unknown (neutral); explicit false means unsupported. EffortValues
// distinguishes known-empty (Set true, empty List) from unknown
// (Set false). All values are cloned; callers cannot mutate the contract.
type OptionalStringList struct {
	Set  bool
	List []string
}

// ReasoningCapabilities carries the planner-relevant facts.
type ReasoningCapabilities struct {
	Supported       *bool
	ToggleSupported *bool
	EffortSupported *bool
	BudgetSupported *bool
	EffortValues    OptionalStringList
	EffortDefault   *string
	EffortNative    *NativeParam
	ToggleNative    *NativeParam
	BudgetNative    *NativeParam
	BudgetMin       *int64
	BudgetMax       *int64
	BudgetDefault   *int64
}

// ReasoningFor projects the private normalized trusted contract into the
// closed capability facts used by request planning. It never reads opaque
// metadata; callers pass the already-resolved canonical contract.
func ReasoningFor(c Contract) ReasoningCapabilities {
	out := ReasoningCapabilities{}
	if v, ok := c.Get("reasoning.supported"); ok && !v.IsNull() {
		if b, ok := v.Bool(); ok {
			bv := b
			out.Supported = &bv
		}
	}
	if v, ok := c.Get("reasoning.toggle.supported"); ok && !v.IsNull() {
		if b, ok := v.Bool(); ok {
			bv := b
			out.ToggleSupported = &bv
		}
	}
	if v, ok := c.Get("reasoning.effort.supported"); ok && !v.IsNull() {
		if b, ok := v.Bool(); ok {
			bv := b
			out.EffortSupported = &bv
		}
	}
	if v, ok := c.Get("reasoning.budget.supported"); ok && !v.IsNull() {
		if b, ok := v.Bool(); ok {
			bv := b
			out.BudgetSupported = &bv
		}
	}
	if v, ok := c.Get("reasoning.effort.values"); ok && !v.IsNull() {
		if ss, ok := v.StringList(); ok {
			out.EffortValues = OptionalStringList{Set: true, List: append([]string(nil), ss...)}
		}
	}
	if v, ok := c.Get("reasoning.effort.default"); ok && !v.IsNull() {
		if s, ok := v.String(); ok {
			sv := s
			out.EffortDefault = &sv
		}
	}
	if v, ok := c.Get("reasoning.effort.native_parameter"); ok && !v.IsNull() {
		if np, ok := v.Native(); ok {
			cp := np
			out.EffortNative = &cp
		}
	}
	if v, ok := c.Get("reasoning.toggle.native_parameter"); ok && !v.IsNull() {
		if np, ok := v.Native(); ok {
			cp := np
			out.ToggleNative = &cp
		}
	}
	if v, ok := c.Get("reasoning.budget.native_parameter"); ok && !v.IsNull() {
		if np, ok := v.Native(); ok {
			cp := np
			out.BudgetNative = &cp
		}
	}
	if v, ok := c.Get("reasoning.budget.min_tokens"); ok && !v.IsNull() {
		if n, ok := v.Int(); ok {
			nv := n
			out.BudgetMin = &nv
		}
	}
	if v, ok := c.Get("reasoning.budget.max_tokens"); ok && !v.IsNull() {
		if n, ok := v.Int(); ok {
			nv := n
			out.BudgetMax = &nv
		}
	}
	if v, ok := c.Get("reasoning.budget.default_tokens"); ok && !v.IsNull() {
		if n, ok := v.Int(); ok {
			nv := n
			out.BudgetDefault = &nv
		}
	}
	return out
}
