package protocol

// ReasoningIntent is the closed validated effort control shared by the
// OpenAI targets and the Gemini thinking ingress. Absent means no effort
// was supplied. Named carries a literal effort string; Gemini levels use
// the reviewed minimal/low/medium/high vocabulary. Off disables thinking (Gemini
// budget 0, or Chat/Responses "none"). Budget carries an explicit positive
// thinking budget (Gemini thinkingBudget > 0 only). Compat carries a
// bearer-compat header effort for non-Gemini destinations. Private fields prevent
// forging invalid controls outside the validated constructors.
type EffortOrigin int

const (
	// ChatEffortOrigin is the Chat "reasoning_effort" source path.
	ChatEffortOrigin EffortOrigin = iota + 1
	// ResponsesEffortOrigin is the Responses "reasoning.effort" source path.
	ResponsesEffortOrigin
	// GeminiBudgetOrigin is the Gemini thinkingBudget source path.
	GeminiBudgetOrigin
	// GeminiLevelOrigin is the Gemini thinkingLevel source path.
	GeminiLevelOrigin
	// CompatEffortOrigin is the bearer-compat "x-model-router-reasoning-effort" source path.
	CompatEffortOrigin
	// AnthropicThinkingOrigin is the Anthropic "thinking" disabled source path.
	AnthropicThinkingOrigin
)

// Path returns the static source path for diagnostics and loss fields.
func (o EffortOrigin) Path() string {
	switch o {
	case ChatEffortOrigin:
		return "reasoning_effort"
	case ResponsesEffortOrigin:
		return "reasoning.effort"
	case GeminiBudgetOrigin:
		return "generationConfig.thinkingConfig.thinkingBudget"
	case GeminiLevelOrigin:
		return "generationConfig.thinkingConfig.thinkingLevel"
	case CompatEffortOrigin:
		return "x-model-router-reasoning-effort"
	case AnthropicThinkingOrigin:
		return "thinking"
	default:
		return ""
	}
}

// ReasoningKind names the closed intent variants.
type ReasoningKind uint8

const (
	// KindAbsent means no control was supplied.
	KindAbsent ReasoningKind = iota
	// KindNamed carries a literal effort level.
	KindNamed
	// KindOff disables thinking.
	KindOff
	// KindBudget carries an explicit positive budget.
	KindBudget
)

type ReasoningIntent struct {
	kind   ReasoningKind
	level  string
	budget int64
	origin EffortOrigin
}

// AbsentReasoning returns the no-effort control.
func AbsentReasoning() ReasoningIntent { return ReasoningIntent{} }

// NewChatEffort validates one literal Chat effort string and returns the
// closed control. Empty strings are rejected. "none" maps to the Off
// variant (disables thinking) with the Chat origin; every other literal
// maps to Named. Level() still reports "none" for the Off variant so
// existing disabled-branch checks keep working.
func NewChatEffort(level string) (ReasoningIntent, bool) {
	if level == "" {
		return ReasoningIntent{}, false
	}
	if level == "none" {
		return ReasoningIntent{kind: KindOff, level: "none", origin: ChatEffortOrigin}, true
	}
	return ReasoningIntent{kind: KindNamed, level: level, origin: ChatEffortOrigin}, true
}

// NewResponsesEffort validates one literal Responses effort string and
// returns the closed control. Empty strings are rejected, mirroring the
// Chat gate so an unknown-neutral plan never emits an empty wire member.
// "none" maps to Off with the Responses origin.
func NewResponsesEffort(level string) (ReasoningIntent, bool) {
	if level == "" {
		return ReasoningIntent{}, false
	}
	if level == "none" {
		return ReasoningIntent{kind: KindOff, level: "none", origin: ResponsesEffortOrigin}, true
	}
	return ReasoningIntent{kind: KindNamed, level: level, origin: ResponsesEffortOrigin}, true
}

// NewGeminiBudget validates a Gemini thinkingBudget value: 0 disables
// thinking (Off), a positive value is an explicit Budget. Negative values
// never reach here (-1 decodes directly to Absent before any constructor;
// anything below -1 is a 400 at the decode boundary).
func NewGeminiBudget(budget int64) (ReasoningIntent, bool) {
	if budget == 0 {
		return ReasoningIntent{kind: KindOff, level: "none", origin: GeminiBudgetOrigin}, true
	}
	if budget > 0 {
		return ReasoningIntent{kind: KindBudget, budget: budget, origin: GeminiBudgetOrigin}, true
	}
	return ReasoningIntent{}, false
}

// NewGeminiLevel accepts the canonical lowercase spellings of the reviewed
// Gemini level vocabulary. Disable is represented by NewGeminiBudget(0),
// not a level named "none".
func NewGeminiLevel(level string) (ReasoningIntent, bool) {
	switch level {
	case "minimal", "low", "medium", "high":
		return ReasoningIntent{kind: KindNamed, level: level, origin: GeminiLevelOrigin}, true
	default:
		return ReasoningIntent{}, false
	}
}

// NewCompatEffort validates one bearer-compat header effort string and
// returns the closed control. Empty strings are rejected. "none" maps to
// the Off variant with the compat origin; every other literal maps to
// Named with the compat origin (published literals such as xhigh/max are
// preserved here, never routed through the Gemini level constructor).
// Level() still reports "none" for the Off variant.
func NewCompatEffort(level string) (ReasoningIntent, bool) {
	if level == "" {
		return ReasoningIntent{}, false
	}
	if level == "none" {
		return ReasoningIntent{kind: KindOff, level: "none", origin: CompatEffortOrigin}, true
	}
	return ReasoningIntent{kind: KindNamed, level: level, origin: CompatEffortOrigin}, true
}

// NewAnthropicDisabled validates the closed Anthropic thinking disabled
// control into the existing Off variant ("none") with the thinking origin.
// Disabled takes no other members; malformed shapes are rejected at the
// decode boundary before this constructor runs.
func NewAnthropicDisabled() ReasoningIntent {
	return ReasoningIntent{kind: KindOff, level: "none", origin: AnthropicThinkingOrigin}
}

// NewAnthropicThinkingEnabled validates an Anthropic thinking enabled
// positive budget into the existing Budget variant with the thinking origin.
// Non-positive budgets never reach here (decode rejects them as malformed);
// the constructor guards the bound for typed-encoder ownership.
func NewAnthropicThinkingEnabled(budget int64) (ReasoningIntent, bool) {
	if budget < 1 {
		return ReasoningIntent{}, false
	}
	return ReasoningIntent{kind: KindBudget, budget: budget, origin: AnthropicThinkingOrigin}, true
}

// IsPresent reports whether an effort was supplied.
func (r ReasoningIntent) IsPresent() bool { return r.kind != KindAbsent }

// Kind returns the closed intent variant.
func (r ReasoningIntent) Kind() ReasoningKind { return r.kind }

// Level returns the literal effort string (Named, plus "none" for Off to
// preserve the existing disabled-branch contract).
func (r ReasoningIntent) Level() (string, bool) {
	switch r.kind {
	case KindNamed:
		return r.level, true
	case KindOff:
		return "none", true
	default:
		return "", false
	}
}

// Budget returns the thinking budget (Budget only).
func (r ReasoningIntent) Budget() (int64, bool) {
	if r.kind != KindBudget {
		return 0, false
	}
	return r.budget, true
}

// IsNone reports whether the control disables thinking ("none"/Off).
func (r ReasoningIntent) IsNone() bool {
	if r.kind == KindOff {
		return true
	}
	return r.kind == KindNamed && r.level == "none"
}

// IsBudget reports whether the control carries an explicit budget.
func (r ReasoningIntent) IsBudget() bool { return r.kind == KindBudget }

// Origin is the static source path for diagnostics.
func (r ReasoningIntent) Origin() string { return r.origin.Path() }
