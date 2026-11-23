package protocol

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/arhammahajan/router/internal/capability"
)

// Reverse named-effort: Responses reasoning.effort -> Chat reasoning_effort.
// Stable source: git show HEAD:model_router/{reasoning,protocol}.py,
// HEAD:router_metadata/__init__.py (closed reasoning object, effort origin
// reasoning.effort, summary/generate_summary Responses-only). Hermetic.

func rcCap(t *testing.T, fn func(c *capability.Contract)) capability.Contract {
	t.Helper()
	c := capability.Empty("openai-chat")
	if fn != nil {
		fn(&c)
	}
	c.Normalize()
	return c
}

func rcCaps(t *testing.T, fn func(c *capability.Contract)) capability.ReasoningCapabilities {
	t.Helper()
	return capability.ReasoningFor(rcCap(t, fn))
}

func rcPut(t *testing.T, c *capability.Contract, path string, v capability.Value) {
	t.Helper()
	s := "review-test"
	if err := c.Put(path, v, &s, capability.KindOverride); err != nil {
		t.Fatalf("put %s: %v", path, err)
	}
}

func TestRCDecodeEffortExactNoLoss(t *testing.T) {
	g, losses, rerr := DecodeResponsesRequest([]byte(`{"model":"m","input":"Hi","reasoning":{"effort":"medium"}}`), DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	if len(losses) != 0 {
		t.Fatalf("exact effort must emit no decode loss: %v", losses)
	}
	lvl, ok := g.Reasoning.Level()
	if !ok || lvl != "medium" {
		t.Fatalf("intent literal lost: %+v", g.Reasoning)
	}
	if got := g.Reasoning.Origin(); got != "reasoning.effort" {
		t.Fatalf("origin must be source path: %q", got)
	}
}

func TestResponsesAutomaticSummaryLossPreservesEffort(t *testing.T) {
	raw := []byte(`{"model":"m","input":"Hi","reasoning":{"effort":"medium","summary":"auto"}}`)
	gen, losses, rerr := DecodeResponsesRequest(raw, DefaultLimits())
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(losses) != 1 || losses[0].Field != "reasoning.summary" || losses[0].Action != "dropped" {
		t.Fatalf("expected automatic summary loss, got %v", losses)
	}
	if _, rerr := EnforcePolicy(losses, false); rerr == nil || rerr.Code != CodeTranslationLoss {
		t.Fatalf("strict route must reject the loss: %v", rerr)
	}
	planned, reasoningLosses := PlanEffort(gen.Reasoning, rcCaps(t, nil), OrdinaryRoute())
	if len(reasoningLosses) != 0 {
		t.Fatalf("unexpected effort loss: %v", reasoningLosses)
	}
	out, rerr := EncodeChatRequest(gen, planned, "upstream")
	if rerr != nil || !strings.Contains(string(out), `"reasoning_effort":"medium"`) || strings.Contains(string(out), "summary") {
		t.Fatalf("wanted only named effort on Chat wire, got %s, %v", out, rerr)
	}
}

func TestRCDecodeNullEmptyNoControl(t *testing.T) {
	for _, raw := range []string{
		`{"model":"m","input":"Hi","reasoning":null}`,
		`{"model":"m","input":"Hi"}`,
		`{"model":"m","input":"Hi","reasoning":{}}`,
	} {
		g, losses, rerr := DecodeResponsesRequest([]byte(raw), DefaultLimits())
		if rerr != nil {
			t.Fatalf("null/empty must be absent %s: %v", raw, rerr)
		}
		if g.Reasoning.IsPresent() {
			t.Fatalf("null/empty must be absent: %s %+v", raw, g.Reasoning)
		}
		if len(losses) != 0 {
			t.Fatalf("null/empty must emit no loss: %s %v", raw, losses)
		}
	}
}

func TestRCDecodeMalformedBothModesPreIO(t *testing.T) {
	for _, raw := range []string{
		`{"model":"m","input":"Hi","reasoning":123}`,
		`{"model":"m","input":"Hi","reasoning":"medium"}`,
		`{"model":"m","input":"Hi","reasoning":[1]}`,
		`{"model":"m","input":"Hi","reasoning":{"effort":123}}`,
		`{"model":"m","input":"Hi","reasoning":{"effort":true}}`,
		`{"model":"m","input":"Hi","reasoning":{"effort":null}}`,
		`{"model":"m","input":"Hi","reasoning":{"effort":""}}`,
		`{"model":"m","input":"Hi","reasoning":{"summary":null}}`,
		`{"model":"m","input":"Hi","reasoning":{"generate_summary":123}}`,
		`{"model":"m","input":"Hi","reasoning":{"effort":"medium","bogus":1}}`,
		`{"model":"m","input":"Hi","reasoning":{"effort":"medium","summary":null}}`,
	} {
		g, losses, rerr := DecodeResponsesRequest([]byte(raw), DefaultLimits())
		if rerr == nil || rerr.Status != 400 {
			t.Fatalf("malformed must 400 pre-IO: %s got gen=%v losses=%v err=%v", raw, g, losses, rerr)
		}
		if len(losses) != 0 {
			t.Fatalf("malformed must not produce losses: %s %v", raw, losses)
		}
	}
}

func TestRCDecodeSummariesRetainRefusalBothModes(t *testing.T) {
	for _, raw := range []string{
		`{"model":"m","input":"Hi","reasoning":{"summary":"detailed"}}`,
		`{"model":"m","input":"Hi","reasoning":{"generate_summary":"auto"}}`,
		`{"model":"m","input":"Hi","reasoning":{"effort":"medium","summary":"detailed"}}`,
		`{"model":"m","input":"Hi","reasoning":{"effort":"medium","generate_summary":"auto"}}`,
	} {
		_, _, rerr := DecodeResponsesRequest([]byte(raw), DefaultLimits())
		if rerr == nil || rerr.Status != 400 || rerr.Code != CodeInvalidGeneration {
			t.Fatalf("summary must retain hard refusal: %s got %v", raw, rerr)
		}
		// Anthropic retains the hard refusal for effort; Gemini decodes
		// effort into the closed intent (summaries still refuse there).
		_, _, rerr2 := DecodeResponsesForAnthropic([]byte(`{"model":"m","input":"Hi","reasoning":{"effort":"medium"}}`), DefaultLimits())
		if rerr2 == nil || rerr2.Status != 400 || rerr2.Code != CodeInvalidGeneration {
			t.Fatalf("anthropic dest must retain refusal: %s got %v", raw, rerr2)
		}
		if g, losses, rerr3 := DecodeResponsesForGemini([]byte(`{"model":"m","input":"Hi","reasoning":{"effort":"medium"}}`), DefaultLimits()); rerr3 != nil || len(losses) != 0 || !g.Reasoning.IsPresent() {
			t.Fatalf("gemini dest must decode effort: %s got %v %v", raw, rerr3, losses)
		}
		if _, _, rerr4 := DecodeResponsesForGemini([]byte(raw), DefaultLimits()); rerr4 == nil || rerr4.Status != 400 {
			t.Fatalf("gemini summary must refuse: %s got %v", raw, rerr4)
		}
	}
}

func TestRCPlannerSupportedFalseNoneExact(t *testing.T) {
	c := capability.Empty("openai-chat")
	rcPut(t, &c, "reasoning.supported", capability.BoolValue(false))
	c.Normalize()
	caps := capability.ReasoningFor(c)
	none, _ := NewResponsesEffort("none")
	planned, losses := PlanEffort(none, caps, OrdinaryRoute())
	if len(losses) != 0 || planned.IsPresent() {
		t.Fatalf("supported=false+none must strip exactly: %+v %v", planned, losses)
	}
	named, _ := NewResponsesEffort("medium")
	_, losses = PlanEffort(named, caps, OrdinaryRoute())
	if len(losses) != 1 || losses[0].Field != "reasoning.effort" || losses[0].Action != "dropped" {
		t.Fatalf("supported=false named must drop with origin: %v", losses)
	}
	if strings.Contains(losses[0].Reason, "medium") {
		t.Fatalf("diagnostic must be value-free: %v", losses[0])
	}
}

func TestRCPlannerNoneGatingToggleEffortEnum(t *testing.T) {
	none, _ := NewResponsesEffort("none")
	unknown := capability.ReasoningFor(capability.Empty("openai-chat"))
	planned, losses := PlanEffort(none, unknown, OrdinaryRoute())
	if len(losses) != 0 {
		t.Fatalf("unknown none must be neutral: %v", losses)
	}
	if eff, ok := planned.Effort(); !ok || eff != "none" {
		t.Fatalf("unknown none must emit literal none: %+v", planned)
	}
	cT := capability.Empty("openai-chat")
	rcPut(t, &cT, "reasoning.toggle.supported", capability.BoolValue(false))
	cT.Normalize()
	if _, losses := PlanEffort(none, capability.ReasoningFor(cT), OrdinaryRoute()); len(losses) != 1 {
		t.Fatalf("toggle=false none must drop: %v", losses)
	}
	cE := capability.Empty("openai-chat")
	rcPut(t, &cE, "reasoning.effort.supported", capability.BoolValue(false))
	cE.Normalize()
	_, losses = PlanEffort(none, capability.ReasoningFor(cE), OrdinaryRoute())
	if len(losses) != 1 || !strings.Contains(losses[0].Reason, "cannot express") {
		t.Fatalf("effort=false none must drop as inexpressible: %v", losses)
	}
	vals, _ := capability.StringListValue([]string{"low", "medium"})
	cN := capability.Empty("openai-chat")
	rcPut(t, &cN, "reasoning.effort.values", vals)
	cN.Normalize()
	if _, losses := PlanEffort(none, capability.ReasoningFor(cN), OrdinaryRoute()); len(losses) != 1 {
		t.Fatalf("enum-without-none must drop: %v", losses)
	}
	vals2, _ := capability.StringListValue([]string{"low", "none"})
	cY := capability.Empty("openai-chat")
	rcPut(t, &cY, "reasoning.effort.values", vals2)
	cY.Normalize()
	if planned, losses := PlanEffort(none, capability.ReasoningFor(cY), OrdinaryRoute()); len(losses) != 0 || !planned.IsPresent() {
		t.Fatalf("enum-with-none must be exact: %+v %v", planned, losses)
	}
}

func TestRCPlannerNamedGatesNoClamp(t *testing.T) {
	intent, _ := NewResponsesEffort("medium")
	unknown := capability.ReasoningFor(capability.Empty("openai-chat"))
	planned, losses := PlanEffort(intent, unknown, OrdinaryRoute())
	if len(losses) != 0 {
		t.Fatalf("unknown must pass neutral: %v", losses)
	}
	if eff, _ := planned.Effort(); eff != "medium" {
		t.Fatalf("literal must be preserved: %+v", planned)
	}
	cE := capability.Empty("openai-chat")
	rcPut(t, &cE, "reasoning.effort.supported", capability.BoolValue(false))
	cE.Normalize()
	if _, losses := PlanEffort(intent, capability.ReasoningFor(cE), OrdinaryRoute()); len(losses) != 1 {
		t.Fatalf("effort=false must drop: %v", losses)
	}
	emptyVals, _ := capability.StringListValue([]string{})
	cK := capability.Empty("openai-chat")
	rcPut(t, &cK, "reasoning.effort.values", emptyVals)
	cK.Normalize()
	emptyCaps := capability.ReasoningFor(cK)
	if !emptyCaps.EffortValues.Set {
		t.Fatalf("known-empty must stay set")
	}
	if _, losses := PlanEffort(intent, emptyCaps, OrdinaryRoute()); len(losses) != 1 {
		t.Fatalf("known-empty must drop: %v", losses)
	}
	vals, _ := capability.StringListValue([]string{"low", "medium"})
	cM := capability.Empty("openai-chat")
	rcPut(t, &cM, "reasoning.effort.values", vals)
	cM.Normalize()
	caps := capability.ReasoningFor(cM)
	good, _ := NewResponsesEffort("medium")
	if _, losses := PlanEffort(good, caps, OrdinaryRoute()); len(losses) != 0 {
		t.Fatalf("known value must be exact: %v", losses)
	}
	bad, _ := NewResponsesEffort("high")
	_, losses = PlanEffort(bad, caps, OrdinaryRoute())
	if len(losses) != 1 {
		t.Fatalf("unknown value must drop: %v", losses)
	}
	minimal, _ := NewResponsesEffort("minimal")
	pMin, losses := PlanEffort(minimal, caps, OrdinaryRoute())
	if len(losses) != 1 || pMin.IsPresent() {
		t.Fatalf("minimal must drop, never become low: %+v %v", pMin, losses)
	}
	if strings.Contains(losses[0].Reason, "minimal") {
		t.Fatalf("diagnostic must be value-free: %v", losses[0])
	}
}

func TestRCEncoderEmitsChatWire(t *testing.T) {
	for _, lvl := range []string{"minimal", "low", "medium", "high", "none"} {
		g, _, rerr := DecodeResponsesRequest([]byte(`{"model":"m","input":"Hi","reasoning":{"effort":"`+lvl+`"}}`), DefaultLimits())
		if rerr != nil {
			t.Fatalf("decode %s: %v", lvl, rerr)
		}
		caps := capability.ReasoningFor(capability.Empty("openai-chat"))
		planned, losses := PlanEffort(g.Reasoning, caps, OrdinaryRoute())
		if len(losses) != 0 {
			t.Fatalf("unknown must be exact for %s: %v", lvl, losses)
		}
		out, rerr := EncodeChatRequest(g, planned, "up")
		if rerr != nil {
			t.Fatalf("encode %s: %v", lvl, rerr)
		}
		var body map[string]any
		if err := json.Unmarshal(out, &body); err != nil {
			t.Fatal(err)
		}
		if body["reasoning_effort"] != lvl {
			t.Fatalf("wire must carry reasoning_effort %q: %s", lvl, out)
		}
		if _, has := body["reasoning"]; has {
			t.Fatalf("must not emit Responses field on Chat wire: %s", out)
		}
	}
}

func TestRCPlannerLlamaBudgetMapping(t *testing.T) {
	// Migrated vertical: Responses effort on a known LlamaCpp server maps
	// to the exact native budget rendered on the Chat wire; none maps to
	// the disable toggle; unknown names drop with the source origin.
	intent, _ := NewResponsesEffort("medium")
	caps := capability.ReasoningFor(capability.Empty("openai-chat"))
	planned, losses := PlanEffort(intent, caps, LocalRoute(capability.RuntimeLlamaCpp))
	if len(losses) != 0 {
		t.Fatalf("llama mapped level must be exact: %v", losses)
	}
	if budget, ok := planned.Budget(); !ok || budget != 8192 {
		t.Fatalf("llama medium must map to 8192: %+v", planned)
	}
	none, _ := NewResponsesEffort("none")
	pNone, lNone := PlanEffort(none, caps, LocalRoute(capability.RuntimeLlamaCpp))
	if len(lNone) != 0 || pNone.Kind() != EffortOff {
		t.Fatalf("llama none must disable: %+v %v", pNone, lNone)
	}
	unknown, _ := NewResponsesEffort("ultra")
	pUnk, lUnk := PlanEffort(unknown, caps, LocalRoute(capability.RuntimeLlamaCpp))
	if len(lUnk) != 1 || lUnk[0].Field != "reasoning.effort" || pUnk.IsPresent() {
		t.Fatalf("unknown must drop with source origin: %+v %v", pUnk, lUnk)
	}
}

func TestRCOriginClosed(t *testing.T) {
	chat, _ := NewChatEffort("medium")
	if chat.Origin() != "reasoning_effort" {
		t.Fatalf("chat origin: %q", chat.Origin())
	}
	resp, _ := NewResponsesEffort("medium")
	if resp.Origin() != "reasoning.effort" {
		t.Fatalf("responses origin: %q", resp.Origin())
	}
	if ChatEffortOrigin.Path() != "reasoning_effort" || ResponsesEffortOrigin.Path() != "reasoning.effort" {
		t.Fatalf("origin paths must be static closed values")
	}
}
