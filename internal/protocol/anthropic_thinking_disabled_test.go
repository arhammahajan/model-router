package protocol

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/arhammahajan/model-router/internal/capability"
)

func TestAnthropicDisabledConstructor(t *testing.T) {
	got := NewAnthropicDisabled()
	if got.Kind() != KindOff {
		t.Fatalf("kind must be Off, got %v", got.Kind())
	}
	lvl, ok := got.Level()
	if !ok || lvl != "none" {
		t.Fatalf("level must be none, got %q %v", lvl, ok)
	}
	if !got.IsNone() || !got.IsPresent() {
		t.Fatalf("disabled must be present none: %+v", got)
	}
	if got.Origin() != "thinking" || AnthropicThinkingOrigin.Path() != "thinking" {
		t.Fatalf("origin must be thinking, got %q %q", got.Origin(), AnthropicThinkingOrigin.Path())
	}
	if _, ok := got.Budget(); ok {
		t.Fatalf("disabled must not carry budget")
	}
}

func mustAnthropicDisabledDecode(t *testing.T, raw string, decode func([]byte, Limits) (*Generation, []Loss, *RouterError)) *Generation {
	t.Helper()
	g, losses, rerr := decode([]byte(raw), DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	if len(losses) != 0 {
		t.Fatalf("disabled must be lossless at decode, got %v", losses)
	}
	if !g.Reasoning.IsPresent() || !g.Reasoning.IsNone() {
		t.Fatalf("intent must be Off none: %+v", g.Reasoning)
	}
	if g.Reasoning.Origin() != "thinking" {
		t.Fatalf("origin must be thinking, got %q", g.Reasoning.Origin())
	}
	return g
}

func TestAnthropicDisabledDecodeChatResponses(t *testing.T) {
	base := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`
	mustAnthropicDisabledDecode(t, base, DecodeAnthropicRequest)
	mustAnthropicDisabledDecode(t, base, DecodeAnthropicForResponses)
}

func TestAnthropicDisabledDecodeGeminiKeepsLoss(t *testing.T) {
	body := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`
	g, losses, rerr := DecodeAnthropicForGemini([]byte(body), DefaultLimits())
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
		}
	}
	if !found {
		t.Fatalf("gemini must keep diagnosed thinking loss, got %v", losses)
	}
}

func TestAnthropicDisabledNullAbsent(t *testing.T) {
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
					t.Fatalf("null extras must be absent-tolerant intent no-loss: %s %+v %v", raw, g.Reasoning, losses)
				}
				continue
			}
			if g.Reasoning.IsPresent() || len(losses) != 0 {
				t.Fatalf("absent must stay absent no-loss: %s %+v %v", raw, g.Reasoning, losses)
			}
		}
	}
}

func TestAnthropicDisabledMalformed(t *testing.T) {
	bad := []string{
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled","budget_tokens":1024}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled","display":"summarized"}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled","budget_tokens":"lots"}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled","display":7}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled","mystery":1}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"adaptive"}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"between_tools"}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"mystery"}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"budget_tokens":1024}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":7}}`,
	}
	for _, raw := range bad {
		for _, decode := range []func([]byte, Limits) (*Generation, []Loss, *RouterError){
			DecodeAnthropicRequest, DecodeAnthropicForResponses, DecodeAnthropicForGemini,
		} {
			if _, _, rerr := decode([]byte(raw), DefaultLimits()); rerr == nil || rerr.Status != 400 {
				t.Fatalf("malformed must 400 both modes: %s got %v", raw, rerr)
			}
		}
	}
}

func anthDisabledContract(t *testing.T, apply func(c *capability.Contract)) capability.ReasoningCapabilities {
	t.Helper()
	c := capability.Empty("openai-chat")
	if apply != nil {
		apply(&c)
	}
	c.Normalize()
	return capability.ReasoningFor(c)
}

func TestAnthropicDisabledPlanEncodeExact(t *testing.T) {
	raw := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`
	g, _, _ := DecodeAnthropicRequest([]byte(raw), DefaultLimits())

	// Unknown caps: exact, no loss, no header.
	planned, losses := PlanEffort(g.Reasoning, anthDisabledContract(t, nil), OrdinaryRoute())
	if len(losses) != 0 || !planned.IsPresent() {
		t.Fatalf("unknown must plan lossless: %v %+v", losses, planned)
	}
	wire, rerr := EncodeChatRequest(g, planned, "up")
	if rerr != nil {
		t.Fatal(rerr)
	}
	var chatWire map[string]any
	if err := json.Unmarshal(wire, &chatWire); err != nil {
		t.Fatal(err)
	}
	if chatWire["reasoning_effort"] != "none" {
		t.Fatalf("chat wire must carry none: %s", wire)
	}

	g2, _, _ := DecodeAnthropicForResponses([]byte(raw), DefaultLimits())
	planned2, losses2 := PlanEffort(g2.Reasoning, capability.ReasoningFor(capability.Empty("openai-responses")), OrdinaryResponsesRoute())
	if len(losses2) != 0 {
		t.Fatalf("responses unknown must be lossless: %v", losses2)
	}
	wire2, rerr := EncodeResponsesRequest(g2, planned2, "up")
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !strings.Contains(string(wire2), `"effort":"none"`) {
		t.Fatalf("responses wire must carry none: %s", wire2)
	}

	// supported=false strips with no loss.
	noThink := anthDisabledContract(t, func(c *capability.Contract) {
		_ = c.Put("reasoning.supported", capability.BoolValue(false), anthStrPtr("test"), capability.KindOverride)
	})
	if p, l := PlanEffort(g.Reasoning, noThink, OrdinaryRoute()); len(l) != 0 || p.IsPresent() {
		t.Fatalf("supported=false must strip no-loss: %v %+v", l, p)
	}

	// toggle=false drops.
	noToggle := anthDisabledContract(t, func(c *capability.Contract) {
		_ = c.Put("reasoning.toggle.supported", capability.BoolValue(false), anthStrPtr("test"), capability.KindOverride)
	})
	if p, l := PlanEffort(g.Reasoning, noToggle, OrdinaryRoute()); len(l) != 1 || p.IsPresent() {
		t.Fatalf("toggle=false must drop: %v %+v", l, p)
	} else if l[0].Field != "thinking" {
		t.Fatalf("loss origin must be thinking: %v", l)
	}

	// Known enum without none drops.
	noNoneVals, _ := capability.StringListValue([]string{"low", "medium"})
	noNone := anthDisabledContract(t, func(c *capability.Contract) {
		_ = c.Put("reasoning.effort.supported", capability.BoolValue(true), anthStrPtr("test"), capability.KindOverride)
		_ = c.Put("reasoning.effort.values", noNoneVals, anthStrPtr("test"), capability.KindOverride)
	})
	if _, l := PlanEffort(g.Reasoning, noNone, OrdinaryRoute()); len(l) != 1 {
		t.Fatalf("enum without none must drop: %v", l)
	}

	// Known LlamaCpp Chat maps off to the exact toggle.
	llamaCaps := anthDisabledContract(t, nil)
	if p, l := PlanEffort(g.Reasoning, llamaCaps, LocalRoute(capability.RuntimeLlamaCpp)); len(l) != 0 || p.Kind() != EffortOff {
		t.Fatalf("llama off must be exact toggle: %v %+v", l, p)
	} else {
		lw, rerr := EncodeChatRequest(g, p, "up")
		if rerr != nil {
			t.Fatal(rerr)
		}
		if !strings.Contains(string(lw), "enable_thinking") || strings.Contains(string(lw), "reasoning_effort") {
			t.Fatalf("llama wire must be kwargs only: %s", lw)
		}
	}

	// Unknown local runtime drops via the planner (no ingress hard refusal here).
	if p, l := PlanEffort(g.Reasoning, anthDisabledContract(t, nil), LocalRoute(capability.RuntimeUnobserved)); len(l) != 1 || p.IsPresent() {
		t.Fatalf("unobserved must drop: %v %+v", l, p)
	}
}

func anthStrPtr(s string) *string { v := s; return &v }

func TestAnthropicDisabledOutputConfigUnchanged(t *testing.T) {
	raw := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"output_config":{"effort":"high"}}`
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
			t.Fatalf("output_config must stay diagnosed loss: %v", losses)
		}
	}
}

func TestAnthropicDisabledBigIntPreserved(t *testing.T) {
	raw := `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"get_time","input":{"n":9007199254740993}}]}],"thinking":{"type":"disabled"}}`
	g, losses, rerr := DecodeAnthropicRequest([]byte(raw), DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	if len(losses) != 0 || !g.Reasoning.IsNone() {
		t.Fatalf("disabled+bigint must be intent no-loss: %v %+v", losses, g.Reasoning)
	}
	if len(g.Messages) == 0 {
		t.Fatalf("messages missing")
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
