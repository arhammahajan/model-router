package protocol

import (
	"math"

	"github.com/arhammahajan/router/internal/capability"
)

// PlannedEffort is the closed typed target shared by both OpenAI effort
// bridges. Absent means nothing is emitted. Named carries the literal
// effort string for ordinary/Strata rendering as reasoning.effort
// (Responses) or reasoning_effort (Chat). Off renders the llama native
// chat_template_kwargs toggle; Budget renders the exact llama thinking
// budget. Encoders alone choose the wire members: the Chat encoder renders
// all four variants; the Responses encoder admits only Named/Absent and
// fails any other combination as internal misuse.
type PlannedEffort struct {
	kind   EffortKind
	effort string
	budget int64
}

// EffortKind names the closed target variants.
type EffortKind uint8

const (
	// EffortAbsent emits nothing.
	EffortAbsent EffortKind = iota
	// EffortNamed emits the literal effort string.
	EffortNamed
	// EffortOff emits the llama disable-thinking toggle.
	EffortOff
	// EffortBudget emits the exact llama thinking budget.
	EffortBudget
)

// AbsentEffort returns the no-op target.
func AbsentEffort() PlannedEffort { return PlannedEffort{} }

// NamedEffortTarget returns the exact effort target (literal preserved).
func NamedEffortTarget(effort string) PlannedEffort {
	return PlannedEffort{kind: EffortNamed, effort: effort}
}

// OffEffortTarget returns the disable-thinking target.
func OffEffortTarget() PlannedEffort { return PlannedEffort{kind: EffortOff} }

// BudgetEffortTarget returns the exact budget target.
func BudgetEffortTarget(budget int64) PlannedEffort {
	return PlannedEffort{kind: EffortBudget, budget: budget}
}

// Kind returns the closed target variant.
func (p PlannedEffort) Kind() EffortKind { return p.kind }

// IsPresent reports whether any control must be emitted.
func (p PlannedEffort) IsPresent() bool { return p.kind != EffortAbsent }

// Effort returns the literal effort to emit (Named only).
func (p PlannedEffort) Effort() (string, bool) {
	if p.kind != EffortNamed {
		return "", false
	}
	return p.effort, true
}

// Budget returns the thinking budget to emit (Budget only).
func (p PlannedEffort) Budget() (int64, bool) {
	if p.kind != EffortBudget {
		return 0, false
	}
	return p.budget, true
}

// EffortTarget is the closed planning destination. Chat covers ordinary
// and local Chat routes; Responses covers ordinary and local Responses
// routes; Gemini covers the Gemini generateContent route; Anthropic is the
// explicit bearer-compat unsupported-loss policy (validated carrier
// diagnosed dropped, never an adapter approximation).
type EffortTarget int

const (
	// TargetChat plans for a Chat destination.
	TargetChat EffortTarget = iota
	// TargetResponses plans for a Responses destination.
	TargetResponses
	// TargetGemini plans for a Gemini generateContent destination.
	TargetGemini
	// TargetAnthropic is the terminal compat dropped-loss policy.
	TargetAnthropic
)

// RouteScope selects the planning policy for one request. Ordinary covers
// every non-local route. Local carries the pinned server runtime: LlamaCpp
// maps recognized levels to native controls, Strata reuses ordinary named
// behavior, and Unobserved never reaches the planner with a present intent
// via the Responses bridge (the handler refuses before I/O) while the
// Gemini ingress drops a present intent as a diagnosed loss here. Responses
// selects the Responses destination policy: Named levels stay literal even
// on LlamaCpp, Off stays a Named "none", and explicit budgets always drop
// (the Responses encoder admits only Named/Absent, so a Budget target
// would be internal misuse). Anthropic selects the terminal compat policy:
// every present intent drops as a diagnosed loss with the static compat
// header origin, strict refuses pre-I/O, lossy continues without a control.
type RouteScope struct {
	Local   bool
	Runtime capability.LocalRuntime
	Target  EffortTarget
}

// OrdinaryRoute plans a Chat-destination request with no local runtime.
func OrdinaryRoute() RouteScope { return RouteScope{Target: TargetChat} }

// LocalRoute plans a Chat-destination request for a generated provider
// with the pinned runtime.
func LocalRoute(rt capability.LocalRuntime) RouteScope {
	return RouteScope{Local: true, Runtime: rt, Target: TargetChat}
}

// OrdinaryResponsesRoute plans a Responses-destination request with no
// local runtime.
func OrdinaryResponsesRoute() RouteScope { return RouteScope{Target: TargetResponses} }

// OrdinaryGeminiRoute plans a Gemini-destination request with no local
// runtime. Only the pinned canonical contract gates the exact wire; model
// IDs, families and provider URLs never do.
func OrdinaryGeminiRoute() RouteScope { return RouteScope{Target: TargetGemini} }

// LocalResponsesRoute plans a Responses-destination request for a
// generated provider with the pinned runtime.
func LocalResponsesRoute(rt capability.LocalRuntime) RouteScope {
	return RouteScope{Local: true, Runtime: rt, Target: TargetResponses}
}

// CompatAnthropicRoute plans a bearer-compat carrier for the Anthropic
// destination: the terminal diagnosed dropped-loss policy. No local runtime
// mapping applies and no adapter budget is invented.
func CompatAnthropicRoute() RouteScope { return RouteScope{Target: TargetAnthropic} }

func droppedEffortLoss(field, reason string) Loss {
	return Loss{Field: field, Reason: reason, Action: "dropped"}
}

// PlanEffort plans one validated intent for the OpenAI destinations, the
// Gemini generateContent destination and the bearer-compat Anthropic
// terminal policy from the canonical trusted capability facts. It is the
// single generalized planner for ordinary named effort, Off, explicit
// budgets and local native controls; there are no pair-owned gates. Branch
// ordering mirrors Python reasoning.plan_reasoning: reasoning.supported=false first (Off
// strips exactly), then the Unobserved local-runtime gate (present intent
// drops; absent bypasses), then Off's toggle gate (LlamaCpp maps directly
// for Chat, otherwise ordinary/Strata effort/enum checks; Gemini drops),
// then explicit Budget gates (budget-supported/min/max only, never effort
// gates; LlamaCpp Chat maps exactly, Gemini maps exactly only on the
// pinned native thinkingBudget fact, everything else drops), then Named
// checks: Gemini maps exactly only on explicit effort support plus the
// exact thinkingLevel native descriptor plus the reviewed known enum plus
// the official wire enum (otherwise diagnosed drop); other destinations
// check effort support plus known enum before the runtime mapping; derived
// Llama budget tables are never gated on budget support/bounds; Off never
// maps to Gemini). Unknown
// (nil) facts are neutral except for the Gemini native fact, which needs
// an explicit supported=true plus the exact native descriptor; explicit
// false and known-enum mismatch diagnose a dropped loss. A known-empty
// enum admits nothing. Literals are never renamed, clamped, or defaulted.
// The loss field is the closed source origin.
func PlanEffort(intent ReasoningIntent, caps capability.ReasoningCapabilities, scope RouteScope) (PlannedEffort, []Loss) {
	if !intent.IsPresent() {
		return AbsentEffort(), nil
	}
	origin := intent.Origin()

	// Bearer-compat Anthropic terminal policy: the validated carrier is
	// diagnosed dropped with the static compat header origin. No adapter
	// approximation and no model-name heuristic is inferred.
	if scope.Target == TargetAnthropic {
		return AbsentEffort(), []Loss{droppedEffortLoss(origin, "no reviewed Anthropic wire mapping; adapter approximation not ported")}
	}

	if caps.Supported != nil && !*caps.Supported {
		if intent.IsNone() {
			return AbsentEffort(), nil
		}
		return AbsentEffort(), []Loss{droppedEffortLoss(origin, "model capability is unsupported")}
	}

	// Unknown cached local runtime with a present intent: the Gemini
	// ingress drops a diagnosed loss here (strict refuses pre-I/O, lossy
	// continues without the control). Absent bypasses this gate above.
	// The Responses ingress retains its existing hard preflight refusal in
	// the bridge, so this gate is unreachable there.
	if scope.Local && scope.Runtime == capability.RuntimeUnobserved {
		return AbsentEffort(), []Loss{droppedEffortLoss(origin, "local runtime is unknown; no live probe has established an engine")}
	}

	if intent.IsNone() {
		if scope.Target == TargetGemini {
			return AbsentEffort(), []Loss{droppedEffortLoss(origin, "no reviewed Gemini thinking mapping for this control")}
		}
		if caps.ToggleSupported != nil && !*caps.ToggleSupported {
			return AbsentEffort(), []Loss{droppedEffortLoss(origin, "model cannot disable thinking")}
		}
		// LlamaCpp Chat maps Off directly to the native toggle. Every
		// other combination (ordinary/Strata Chat, any Responses
		// including LlamaCpp) checks effort/enum and emits Named "none".
		if scope.Local && scope.Runtime == capability.RuntimeLlamaCpp && scope.Target == TargetChat {
			return OffEffortTarget(), nil
		}
		if caps.EffortSupported != nil && !*caps.EffortSupported {
			return AbsentEffort(), []Loss{droppedEffortLoss(origin, "model's known effort values cannot express 'none'")}
		}
		if caps.EffortValues.Set && !containsEffort(caps.EffortValues.List, "none") {
			return AbsentEffort(), []Loss{droppedEffortLoss(origin, "model's known effort values cannot express 'none'")}
		}
		return NamedEffortTarget("none"), nil
	}

	if budget, ok := intent.Budget(); ok {
		if caps.BudgetSupported != nil && !*caps.BudgetSupported {
			return AbsentEffort(), []Loss{droppedEffortLoss(origin, "model budget control is unsupported")}
		}
		if caps.BudgetMin != nil && budget < *caps.BudgetMin {
			return AbsentEffort(), []Loss{droppedEffortLoss(origin, "budget is outside the model's known bounds")}
		}
		if caps.BudgetMax != nil && budget > *caps.BudgetMax {
			return AbsentEffort(), []Loss{droppedEffortLoss(origin, "budget is outside the model's known bounds")}
		}
		// Gemini generateContent renders the exact thinkingBudget only on
		// the pinned native fact: explicit reasoning.budget.supported=true
		// plus BudgetNative protocol gemini at
		// generationConfig.thinkingConfig.thinkingBudget (verified primary
		// GenerateContent REST contract:
		// https://ai.google.dev/api/generate-content,
		// https://ai.google.dev/gemini-api/docs/generate-content/thinking).
		// Unknown/false/alternate descriptors drop with a diagnosed loss,
		// never a model-name or URL heuristic. The int32 wire range is
		// checked here; the encoder owns the exact member.
		if scope.Target == TargetGemini {
			if caps.BudgetSupported == nil || !*caps.BudgetSupported {
				return AbsentEffort(), []Loss{droppedEffortLoss(origin, "model budget control is unsupported")}
			}
			if caps.BudgetNative == nil || caps.BudgetNative.Protocol() != "gemini" ||
				caps.BudgetNative.Path() != "generationConfig.thinkingConfig.thinkingBudget" {
				return AbsentEffort(), []Loss{droppedEffortLoss(origin, "model budget control is unsupported")}
			}
			if budget < 1 || budget > math.MaxInt32 {
				return AbsentEffort(), []Loss{droppedEffortLoss(origin, "budget is outside the model's known bounds")}
			}
			return BudgetEffortTarget(budget), nil
		}
		// Explicit budgets render only on confirmed LlamaCpp Chat.
		// Ordinary Chat, Strata Chat and every Responses destination
		// drop with a diagnosed loss (never silent, never 500 misuse).
		if scope.Local && scope.Runtime == capability.RuntimeLlamaCpp && scope.Target == TargetChat {
			return BudgetEffortTarget(budget), nil
		}
		return AbsentEffort(), []Loss{droppedEffortLoss(origin, "native thinking budget requires a native route")}
	}

	level, _ := intent.Level()
	if scope.Target == TargetGemini {
		// Gemini generateContent renders the exact thinkingLevel only on
		// the pinned canonical facts: explicit reasoning.effort.supported=true
		// plus EffortNative protocol gemini at
		// generationConfig.thinkingConfig.thinkingLevel (verified primary
		// GenerateContent REST contract:
		// https://ai.google.dev/api/generate-content) plus the reviewed
		// known effort.values enum containing the requested literal, plus
		// the current official wire enum (exact uppercase conversion).
		// Unknown/false/alternate descriptors, unknown/known-empty/enum
		// mismatch and unreviewed literals diagnose a dropped loss, never a
		// clamp, guess or model-name heuristic. Off ("none") never reaches
		// here (handled above).
		if caps.EffortSupported == nil || !*caps.EffortSupported {
			return AbsentEffort(), []Loss{droppedEffortLoss(origin, "model capability is unsupported")}
		}
		if caps.EffortNative == nil || caps.EffortNative.Protocol() != "gemini" ||
			caps.EffortNative.Path() != "generationConfig.thinkingConfig.thinkingLevel" {
			return AbsentEffort(), []Loss{droppedEffortLoss(origin, "model effort control is unsupported")}
		}
		if !caps.EffortValues.Set || !containsEffort(caps.EffortValues.List, level) {
			return AbsentEffort(), []Loss{droppedEffortLoss(origin, "effort is outside the model's known values")}
		}
		if _, ok := GeminiThinkingLevelWire(level); !ok {
			return AbsentEffort(), []Loss{droppedEffortLoss(origin, "no reviewed Gemini thinking mapping for this control")}
		}
		return NamedEffortTarget(level), nil
	}
	if caps.EffortSupported != nil && !*caps.EffortSupported {
		return AbsentEffort(), []Loss{droppedEffortLoss(origin, "model capability is unsupported")}
	}
	if caps.EffortValues.Set && !containsEffort(caps.EffortValues.List, level) {
		return AbsentEffort(), []Loss{droppedEffortLoss(origin, "effort is outside the model's known values")}
	}
	// Responses destinations always keep the literal (even on LlamaCpp):
	// the Responses encoder admits only Named/Absent.
	if scope.Target == TargetResponses {
		return NamedEffortTarget(level), nil
	}
	if scope.Local && scope.Runtime == capability.RuntimeLlamaCpp {
		return planLlamaEffort(intent)
	}
	return NamedEffortTarget(level), nil
}

// GeminiThinkingLevelWire maps a closed Chat/Responses effort literal to
// the current official Gemini generateContent thinkingLevel wire enum
// (verified primary REST contract
// https://ai.google.dev/api/generate-content: THINKING_LEVEL_UNSPECIFIED,
// MINIMAL, LOW, MEDIUM, HIGH, XHIGH). Mapping is exact uppercase conversion
// only; THINKING_LEVEL_UNSPECIFIED is never emitted (absent) and any other
// literal is unreviewed. The planner gates emission on the pinned
// capability facts; the encoder owns this exact member.
func GeminiThinkingLevelWire(level string) (string, bool) {
	switch level {
	case "minimal":
		return "MINIMAL", true
	case "low":
		return "LOW", true
	case "medium":
		return "MEDIUM", true
	case "high":
		return "HIGH", true
	case "xhigh":
		return "XHIGH", true
	default:
		return "", false
	}
}

func planLlamaEffort(intent ReasoningIntent) (PlannedEffort, []Loss) {
	level, _ := intent.Level()
	mapped, ok := capability.MapLlamaEffort(level)
	if ok {
		switch mapped.Kind {
		case capability.LlamaMapDisable:
			return OffEffortTarget(), nil
		case capability.LlamaMapBudget:
			return BudgetEffortTarget(mapped.Budget), nil
		}
	}
	return AbsentEffort(), []Loss{droppedEffortLoss(intent.Origin(), "llama.cpp has no control for this effort level")}
}

func containsEffort(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
