package protocol

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/arhammahajan/router/internal/capability"
)

// Independent adversarial review for Anthropic thinking disabled -> Off.
// Scope: NewAnthropicDisabled KindOff none, origin thinking; Chat/Responses
// closed intent vs Gemini diagnosed loss; malformed before loss; null guard;
// display/summary/enabled/adaptive/between_tools/output_config unchanged;
// full semantics via pinned snapshot -> RequestCapability -> ReasoningFor ->
// PlanEffort -> EnforcePolicy -> encoder; gate order and Origin static.

func offRevStrPtr(s string) *string { v := s; return &v }

func offRevCaps(t *testing.T, apply func(*capability.Contract)) capability.ReasoningCapabilities {
	t.Helper()
	c := capability.Empty("openai-chat")
	if apply != nil {
		apply(&c)
	}
	c.Normalize()
	return capability.ReasoningFor(c)
}

func offRevDecode(t *testing.T, raw string, decode func([]byte, Limits) (*Generation, []Loss, *RouterError)) *Generation {
	t.Helper()
	g, losses, rerr := decode([]byte(raw), DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	if len(losses) != 0 {
		t.Fatalf("disabled must be lossless at decode: %v", losses)
	}
	if !g.Reasoning.IsPresent() || !g.Reasoning.IsNone() {
		t.Fatalf("intent must be present none: %+v", g.Reasoning)
	}
	if g.Reasoning.Kind() != KindOff {
		t.Fatalf("kind must be Off: %v", g.Reasoning.Kind())
	}
	if g.Reasoning.Origin() != "thinking" {
		t.Fatalf("origin must be thinking: %q", g.Reasoning.Origin())
	}
	if _, ok := g.Reasoning.Budget(); ok {
		t.Fatalf("disabled must not carry budget")
	}
	if _, ok := g.Reasoning.Level(); !ok {
		t.Fatalf("disabled must report level none")
	} else {
		lvl, _ := g.Reasoning.Level()
		if lvl != "none" {
			t.Fatalf("level must be none: %q", lvl)
		}
	}
	return g
}

func TestOffRevConstructorStatic(t *testing.T) {
	got := NewAnthropicDisabled()
	if got.Kind() != KindOff {
		t.Fatalf("kind must be Off")
	}
	if !got.IsNone() || !got.IsPresent() {
		t.Fatalf("must be present none: %+v", got)
	}
	if got.Origin() != "thinking" || AnthropicThinkingOrigin.Path() != "thinking" {
		t.Fatalf("origin must be static thinking")
	}
	if got.Origin() == "" || strings.Contains(got.Origin(), "prompt") {
		t.Fatalf("origin must be value-free static")
	}
	if _, ok := got.Budget(); ok {
		t.Fatalf("no budget")
	}
	lvl, ok := got.Level()
	if !ok || lvl != "none" {
		t.Fatalf("level none: %q %v", lvl, ok)
	}
}

func TestOffRevDecodeDestSplit(t *testing.T) {
	base := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`
	offRevDecode(t, base, DecodeAnthropicRequest)
	offRevDecode(t, base, DecodeAnthropicForResponses)
	g, losses, rerr := DecodeAnthropicForGemini([]byte(base), DefaultLimits())
	if rerr != nil {
		t.Fatalf("gemini decode: %v", rerr)
	}
	if g.Reasoning.IsPresent() {
		t.Fatalf("gemini must not carry intent: %+v", g.Reasoning)
	}
	found := false
	for _, l := range losses {
		if l.Field == "thinking" {
			found = true
			if l.Action != "dropped" {
				t.Fatalf("gemini loss must be dropped: %+v", l)
			}
		}
	}
	if !found {
		t.Fatalf("gemini must keep diagnosed thinking loss: %v", losses)
	}
	if _, rerr := EnforcePolicy(losses, false); rerr == nil || rerr.Code != CodeTranslationLoss {
		t.Fatalf("gemini strict must refuse: %v", rerr)
	}
}

func TestOffRevMalformedBeforeLoss(t *testing.T) {
	bad := []string{
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled","budget_tokens":1024}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled","display":"summarized"}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled","display":{}}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled","summary":{"foo":1}}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled","mystery":1}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled","budget_tokens":"lots"}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled","display":7}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"Disabled"}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"adaptive"}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"between_tools"}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"mystery"}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"budget_tokens":1024}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":7}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":null}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":"disabled"}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":[]}`,
	}
	for _, raw := range bad {
		for _, decode := range []func([]byte, Limits) (*Generation, []Loss, *RouterError){
			DecodeAnthropicRequest, DecodeAnthropicForResponses, DecodeAnthropicForGemini,
		} {
			if _, _, rerr := decode([]byte(raw), DefaultLimits()); rerr == nil || rerr.Status != 400 {
				t.Fatalf("malformed must 400: %s got %v", raw, rerr)
			}
		}
	}
}

func TestOffRevNullAbsentGuard(t *testing.T) {
	for _, raw := range []string{
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":null}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled","budget_tokens":null,"display":null}}`,
	} {
		for _, decode := range []func([]byte, Limits) (*Generation, []Loss, *RouterError){
			DecodeAnthropicRequest, DecodeAnthropicForResponses,
		} {
			g, losses, rerr := decode([]byte(raw), DefaultLimits())
			if rerr != nil {
				t.Fatalf("raw %s: %v", raw, rerr)
			}
			if strings.Contains(raw, `"disabled"`) {
				if !g.Reasoning.IsNone() || len(losses) != 0 {
					t.Fatalf("null extras absent-tolerant: %s %+v %v", raw, g.Reasoning, losses)
				}
				continue
			}
			if g.Reasoning.IsPresent() || len(losses) != 0 {
				t.Fatalf("absent stays absent: %s %+v %v", raw, g.Reasoning, losses)
			}
		}
	}
}

func TestOffRevNullPlusOtherLoss(t *testing.T) {
	raw := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":null,"service_tier":"auto"}`
	for _, decode := range []func([]byte, Limits) (*Generation, []Loss, *RouterError){
		DecodeAnthropicRequest, DecodeAnthropicForResponses,
	} {
		g, losses, rerr := decode([]byte(raw), DefaultLimits())
		if rerr != nil {
			t.Fatalf("decode: %v", rerr)
		}
		if g.Reasoning.IsPresent() {
			t.Fatalf("null thinking must stay absent: %+v", g.Reasoning)
		}
		if len(losses) != 1 || losses[0].Field != "service_tier" {
			t.Fatalf("only service_tier loss: %v", losses)
		}
	}
}

func TestOffRevMalformedBeatsOtherLoss(t *testing.T) {
	raw := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled","budget_tokens":5},"service_tier":"auto"}`
	for _, decode := range []func([]byte, Limits) (*Generation, []Loss, *RouterError){
		DecodeAnthropicRequest, DecodeAnthropicForResponses, DecodeAnthropicForGemini,
	} {
		if _, _, rerr := decode([]byte(raw), DefaultLimits()); rerr == nil || rerr.Status != 400 {
			t.Fatalf("malformed must 400 before loss: %s got %v", raw, rerr)
		}
	}
}

func TestOffRevNotBroadened(t *testing.T) {
	for _, raw := range []string{
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"output_config":{"effort":"high"}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"output_config":{"effort":null}}`,
	} {
		for _, decode := range []func([]byte, Limits) (*Generation, []Loss, *RouterError){
			DecodeAnthropicRequest, DecodeAnthropicForResponses,
		} {
			g, losses, rerr := decode([]byte(raw), DefaultLimits())
			if rerr != nil {
				t.Fatalf("output_config decode: %v", rerr)
			}
			if g.Reasoning.IsPresent() {
				t.Fatalf("output_config must not become intent: %+v", g.Reasoning)
			}
			found := false
			for _, l := range losses {
				if l.Field == "output_config" {
					found = true
				}
			}
			if !found {
				t.Fatalf("output_config stays diagnosed loss: %v", losses)
			}
		}
	}
	combined := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"},"output_config":{"effort":"high"}}`
	for _, decode := range []func([]byte, Limits) (*Generation, []Loss, *RouterError){
		DecodeAnthropicRequest, DecodeAnthropicForResponses,
	} {
		g, losses, rerr := decode([]byte(combined), DefaultLimits())
		if rerr != nil {
			t.Fatalf("combined decode: %v", rerr)
		}
		if !g.Reasoning.IsNone() {
			t.Fatalf("disabled intent must survive alongside output_config: %+v", g.Reasoning)
		}
		if len(losses) != 1 || losses[0].Field != "output_config" {
			t.Fatalf("only output_config loss: %v", losses)
		}
	}
}

func TestOffRevPlanGateOrder(t *testing.T) {
	raw := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`
	g, _, _ := DecodeAnthropicRequest([]byte(raw), DefaultLimits())

	if p, l := PlanEffort(g.Reasoning, offRevCaps(t, nil), OrdinaryRoute()); len(l) != 0 || !p.IsPresent() {
		t.Fatalf("unknown neutral exact: %v %+v", l, p)
	}
	withNone, _ := capability.StringListValue([]string{"low", "none"})
	if p, l := PlanEffort(g.Reasoning, offRevCaps(t, func(c *capability.Contract) {
		_ = c.Put("reasoning.effort.values", withNone, offRevStrPtr("t"), capability.KindOverride)
	}), OrdinaryRoute()); len(l) != 0 {
		t.Fatalf("known-with-none exact: %v %+v", l, p)
	}
	noThink := offRevCaps(t, func(c *capability.Contract) {
		_ = c.Put("reasoning.supported", capability.BoolValue(false), offRevStrPtr("t"), capability.KindOverride)
	})
	if p, l := PlanEffort(g.Reasoning, noThink, OrdinaryRoute()); len(l) != 0 || p.IsPresent() {
		t.Fatalf("supported=false strips no loss: %v %+v", l, p)
	}
	both := capability.Empty("openai-chat")
	_ = both.Put("reasoning.supported", capability.BoolValue(false), offRevStrPtr("t"), capability.KindOverride)
	_ = both.Put("reasoning.toggle.supported", capability.BoolValue(false), offRevStrPtr("t"), capability.KindOverride)
	both.Normalize()
	if p, l := PlanEffort(g.Reasoning, capability.ReasoningFor(both), OrdinaryRoute()); len(l) != 0 || p.IsPresent() {
		t.Fatalf("supported=false wins over toggle=false: %v %+v", l, p)
	}
	if p, l := PlanEffort(g.Reasoning, offRevCaps(t, func(c *capability.Contract) {
		_ = c.Put("reasoning.toggle.supported", capability.BoolValue(false), offRevStrPtr("t"), capability.KindOverride)
	}), OrdinaryRoute()); len(l) != 1 || p.IsPresent() || l[0].Field != "thinking" {
		t.Fatalf("toggle=false drops thinking: %v %+v", l, p)
	}
	if p, l := PlanEffort(g.Reasoning, offRevCaps(t, func(c *capability.Contract) {
		_ = c.Put("reasoning.toggle.supported", capability.BoolValue(false), offRevStrPtr("t"), capability.KindOverride)
	}), OrdinaryResponsesRoute()); len(l) != 1 || p.IsPresent() || l[0].Field != "thinking" {
		t.Fatalf("toggle=false drops for Responses: %v %+v", l, p)
	}
	if _, l := PlanEffort(g.Reasoning, offRevCaps(t, func(c *capability.Contract) {
		_ = c.Put("reasoning.effort.supported", capability.BoolValue(false), offRevStrPtr("t"), capability.KindOverride)
	}), OrdinaryRoute()); len(l) != 1 {
		t.Fatalf("effort.supported=false drops Off: %v", l)
	}
	emptyVals, _ := capability.StringListValue([]string{})
	if _, l := PlanEffort(g.Reasoning, offRevCaps(t, func(c *capability.Contract) {
		_ = c.Put("reasoning.effort.values", emptyVals, offRevStrPtr("t"), capability.KindOverride)
	}), OrdinaryRoute()); len(l) != 1 {
		t.Fatalf("known-empty not neutral: %v", l)
	}
	noNoneVals, _ := capability.StringListValue([]string{"low", "medium"})
	if _, l := PlanEffort(g.Reasoning, offRevCaps(t, func(c *capability.Contract) {
		_ = c.Put("reasoning.effort.values", noNoneVals, offRevStrPtr("t"), capability.KindOverride)
	}), OrdinaryRoute()); len(l) != 1 {
		t.Fatalf("enum without none drops: %v", l)
	}
	if p, l := PlanEffort(g.Reasoning, offRevCaps(t, nil), LocalRoute(capability.RuntimeLlamaCpp)); len(l) != 0 || p.Kind() != EffortOff {
		t.Fatalf("llama Chat exact toggle: %v %+v", l, p)
	}
	if p, l := PlanEffort(g.Reasoning, offRevCaps(t, nil), LocalResponsesRoute(capability.RuntimeLlamaCpp)); len(l) != 0 {
		t.Fatalf("llama Responses stays literal: %v %+v", l, p)
	} else if eff, ok := p.Effort(); !ok || eff != "none" {
		t.Fatalf("llama Responses must be named none: %+v", p)
	}
	if p, l := PlanEffort(g.Reasoning, offRevCaps(t, nil), LocalRoute(capability.RuntimeStrata)); len(l) != 0 {
		t.Fatalf("strata Chat exact: %v", l)
	} else if eff, ok := p.Effort(); !ok || eff != "none" {
		t.Fatalf("strata Chat must be named none: %+v", p)
	}
	if p, l := PlanEffort(g.Reasoning, offRevCaps(t, nil), LocalResponsesRoute(capability.RuntimeStrata)); len(l) != 0 {
		t.Fatalf("strata Responses exact: %v", l)
	} else if eff, ok := p.Effort(); !ok || eff != "none" {
		t.Fatalf("strata Responses named none: %+v", p)
	}
	if p, l := PlanEffort(g.Reasoning, offRevCaps(t, nil), LocalRoute(capability.RuntimeUnobserved)); len(l) != 1 || p.IsPresent() || l[0].Field != "thinking" {
		t.Fatalf("unobserved diagnosed drop: %v %+v", l, p)
	}
	if _, rerr := EnforcePolicy(func() []Loss {
		_, l := PlanEffort(g.Reasoning, offRevCaps(t, nil), LocalRoute(capability.RuntimeUnobserved))
		return l
	}(), false); rerr == nil || rerr.Code != CodeTranslationLoss {
		t.Fatalf("unobserved strict must 400")
	}
}

func TestOffRevEncodeWire(t *testing.T) {
	raw := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`
	g, _, _ := DecodeAnthropicRequest([]byte(raw), DefaultLimits())
	planned, _ := PlanEffort(g.Reasoning, offRevCaps(t, nil), OrdinaryRoute())
	wire, rerr := EncodeChatRequest(g, planned, "up")
	if rerr != nil {
		t.Fatal(rerr)
	}
	var chatWire map[string]any
	if err := json.Unmarshal(wire, &chatWire); err != nil {
		t.Fatal(err)
	}
	if chatWire["reasoning_effort"] != "none" {
		t.Fatalf("chat wire none: %s", wire)
	}
	g2, _, _ := DecodeAnthropicForResponses([]byte(raw), DefaultLimits())
	planned2, _ := PlanEffort(g2.Reasoning, capability.ReasoningFor(capability.Empty("openai-responses")), OrdinaryResponsesRoute())
	wire2, rerr := EncodeResponsesRequest(g2, planned2, "up")
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !strings.Contains(string(wire2), `"effort":"none"`) {
		t.Fatalf("responses wire none: %s", wire2)
	}
	if _, rerr := EncodeResponsesRequest(g2, OffEffortTarget(), "up"); rerr == nil || rerr.Status != 500 {
		t.Fatalf("Responses encoder must reject Off misuse: %v", rerr)
	}
	if _, rerr := EncodeResponsesRequest(g2, BudgetEffortTarget(5), "up"); rerr == nil || rerr.Status != 500 {
		t.Fatalf("Responses encoder must reject Budget misuse: %v", rerr)
	}
}

func TestOffRevBigIntStreamCoexist(t *testing.T) {
	raw := `{"model":"m","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"get_time","input":{"n":9007199254740993}}]}],"thinking":{"type":"disabled"}}`
	g, losses, rerr := DecodeAnthropicRequest([]byte(raw), DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	if len(losses) != 0 || !g.Reasoning.IsNone() || !g.Stream {
		t.Fatalf("bigint+stream+disabled: %v %+v %v", losses, g.Reasoning, g.Stream)
	}
	found := false
	for _, m := range g.Messages {
		for _, tc := range m.ToolCalls {
			if strings.Contains(string(tc.Arguments), "9007199254740993") {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("bigint must survive exactly")
	}
}
