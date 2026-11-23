package protocol

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/arhammahajan/router/internal/capability"
)

// Gemini-request reasoning unit: decode of generationConfig.thinkingConfig
// into the closed intent plus shared-planner behavior for both OpenAI
// destinations. Stable source: git show HEAD:model_router/{gemini,
// reasoning,protocol}.py. Hermetic: no network, env, or Python tree.

func gemBody(thinking string) string {
	return `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{` + thinking + `}}}`
}

func gemDecodeChat(t *testing.T, thinking string) (*Generation, []Loss, *RouterError) {
	t.Helper()
	return DecodeGeminiRequest([]byte(gemBody(thinking)), "m", false, DefaultLimits())
}

func gemDecodeResp(t *testing.T, thinking string) (*Generation, []Loss, *RouterError) {
	t.Helper()
	return DecodeGeminiForResponses([]byte(gemBody(thinking)), "m", false, DefaultLimits())
}

func gemCap(fn func(c *capability.Contract)) capability.Contract {
	c := capability.Empty("openai-responses")
	if fn != nil {
		fn(&c)
	}
	c.Normalize()
	return c
}

func gemCaps(fn func(c *capability.Contract)) capability.ReasoningCapabilities {
	return capability.ReasoningFor(gemCap(fn))
}

func gemPut(t *testing.T, c *capability.Contract, path string, v capability.Value) {
	t.Helper()
	s := "gemini-reasoning-test"
	if err := c.Put(path, v, &s, capability.KindOverride); err != nil {
		t.Fatalf("put %s: %v", path, err)
	}
}

func gemInt(v int64) *int64 { return &v }

func TestGeminiBudgetDecodeMatrix(t *testing.T) {
	// -1: absent, no loss, no origin.
	g, losses, rerr := gemDecodeChat(t, `"thinkingBudget":-1`)
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	if g.Reasoning.IsPresent() || len(losses) != 0 {
		t.Fatalf("-1 must be absent without loss: %+v %v", g.Reasoning, losses)
	}
	// 0: Off with budget origin.
	g, losses, rerr = gemDecodeChat(t, `"thinkingBudget":0`)
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	if len(losses) != 0 {
		t.Fatalf("budget 0 must emit no decode loss: %v", losses)
	}
	if g.Reasoning.Kind() != KindOff || !g.Reasoning.IsNone() {
		t.Fatalf("budget 0 must be Off: %+v", g.Reasoning)
	}
	if b, ok := g.Reasoning.Budget(); ok || b != 0 {
		t.Fatalf("Off must carry no budget: %+v", g.Reasoning)
	}
	if got := g.Reasoning.Origin(); got != "generationConfig.thinkingConfig.thinkingBudget" {
		t.Fatalf("origin: %q", got)
	}
	// Positive: Budget exact, no loss.
	g, losses, rerr = gemDecodeChat(t, `"thinkingBudget":100`)
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	if len(losses) != 0 {
		t.Fatalf("positive budget must emit no decode loss: %v", losses)
	}
	b, ok := g.Reasoning.Budget()
	if !ok || b != 100 {
		t.Fatalf("budget not preserved: %+v", g.Reasoning)
	}
	if got := g.Reasoning.Origin(); got != "generationConfig.thinkingConfig.thinkingBudget" {
		t.Fatalf("origin: %q", got)
	}
	// Same for the Responses destination decoder.
	g, losses, rerr = gemDecodeResp(t, `"thinkingBudget":100`)
	if rerr != nil {
		t.Fatalf("responses decode: %v", rerr)
	}
	if b, ok := g.Reasoning.Budget(); !ok || b != 100 || len(losses) != 0 {
		t.Fatalf("responses budget: %+v %v", g.Reasoning, losses)
	}
	g, _, rerr = gemDecodeResp(t, `"thinkingBudget":0`)
	if rerr != nil {
		t.Fatalf("responses decode: %v", rerr)
	}
	if g.Reasoning.Kind() != KindOff {
		t.Fatalf("responses budget 0 must be Off: %+v", g.Reasoning)
	}
}

func TestGeminiLevelDecodeMatrix(t *testing.T) {
	g, losses, rerr := gemDecodeChat(t, `"thinkingLevel":"THINKING_LEVEL_UNSPECIFIED"`)
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	if g.Reasoning.IsPresent() || len(losses) != 0 {
		t.Fatalf("UNSPECIFIED must be absent: %+v %v", g.Reasoning, losses)
	}
	for src, want := range map[string]string{
		`"thinkingLevel":"MINIMAL"`: "minimal",
		`"thinkingLevel":"LOW"`:     "low",
		`"thinkingLevel":"MEDIUM"`:  "medium",
		`"thinkingLevel":"HIGH"`:    "high",
	} {
		g, losses, rerr = gemDecodeChat(t, src)
		if rerr != nil {
			t.Fatalf("decode %s: %v", src, rerr)
		}
		if len(losses) != 0 {
			t.Fatalf("level must emit no decode loss %s: %v", src, losses)
		}
		lvl, ok := g.Reasoning.Level()
		if !ok || lvl != want {
			t.Fatalf("level %s -> %q, got %+v", src, want, g.Reasoning)
		}
		if got := g.Reasoning.Origin(); got != "generationConfig.thinkingConfig.thinkingLevel" {
			t.Fatalf("origin: %q", got)
		}
	}
	// Null members are absent; {} is absent without loss.
	for _, th := range []string{`"thinkingBudget":null`, `"thinkingLevel":null`, ``} {
		g, losses, rerr = gemDecodeChat(t, th)
		if rerr != nil {
			t.Fatalf("decode %q: %v", th, rerr)
		}
		if g.Reasoning.IsPresent() || len(losses) != 0 {
			t.Fatalf("null/empty must be absent: %q %+v %v", th, g.Reasoning, losses)
		}
	}
}

func TestGeminiThinkingMalformed(t *testing.T) {
	for _, th := range []string{
		`"thinkingBudget":100,"thinkingLevel":"LOW"`,
		`"thinkingBudget":true`,
		`"thinkingBudget":1.5`,
		`"thinkingBudget":100.0`,
		`"thinkingBudget":"100"`,
		`"thinkingBudget":-2`,
		`"thinkingBudget":9223372036854775808`,
		`"thinkingBudget":-9223372036854775809`,
		`"thinkingLevel":123`,
		`"thinkingLevel":true`,
		`"thinkingLevel":""`,
		`"thinkingLevel":"XHIGH"`,
		`"thinkingLevel":"HIGHER"`,
		`"thinkingLevel":"low"`,
		`"thinkingLevel":"high "`,
		`"nope":1`,
		`"includeThoughts":"yes"`,
	} {
		if _, losses, rerr := gemDecodeChat(t, th); rerr == nil || rerr.Status != 400 || len(losses) != 0 {
			t.Fatalf("chat malformed %q must 400 pre-loss: %v %v", th, rerr, losses)
		}
		if _, losses, rerr := gemDecodeResp(t, th); rerr == nil || rerr.Status != 400 || len(losses) != 0 {
			t.Fatalf("responses malformed %q must 400 pre-loss: %v %v", th, rerr, losses)
		}
	}
	// Null includeThoughts is absent (stable Python: explicit null reads
	// as None and is skipped); it is not a type error here.
	gNull, _, rerrNull := gemDecodeChat(t, `"includeThoughts":null`)
	if rerrNull != nil {
		t.Fatalf("null thoughts: %v", rerrNull)
	}
	if gNull.Reasoning.IsPresent() {
		t.Fatalf("null thoughts must be absent: %+v", gNull.Reasoning)
	}
	// Snake alias works; collision is a 400.
	g, _, rerr := DecodeGeminiRequest([]byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generation_config":{"thinking_config":{"thinking_budget":50}}}`), "m", false, DefaultLimits())
	if rerr != nil {
		t.Fatalf("snake alias: %v", rerr)
	}
	if b, ok := g.Reasoning.Budget(); !ok || b != 50 {
		t.Fatalf("snake budget: %+v", g.Reasoning)
	}
	if got := g.Reasoning.Origin(); got != "generationConfig.thinkingConfig.thinkingBudget" {
		t.Fatalf("canonical origin through alias: %q", got)
	}
	raw := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingBudget":1}},"generation_config":{"thinkingConfig":{}}}`)
	if _, _, rerr := DecodeGeminiRequest(raw, "m", false, DefaultLimits()); rerr == nil || rerr.Status != 400 {
		t.Fatalf("collision must 400: %v", rerr)
	}
}

func TestGeminiIncludeThoughts(t *testing.T) {
	// includeThoughts=true keeps its independent loss and creates no intent.
	g, losses, rerr := gemDecodeChat(t, `"includeThoughts":true`)
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	if g.Reasoning.IsPresent() {
		t.Fatalf("bare thoughts must create no intent: %+v", g.Reasoning)
	}
	found := false
	for _, l := range losses {
		if l.Field == "generationConfig.thinkingConfig.includeThoughts" {
			found = true
		}
	}
	if !found {
		t.Fatalf("thoughts loss missing: %v", losses)
	}
	// includeThoughts=false: no intent, no loss.
	g, losses, rerr = gemDecodeChat(t, `"includeThoughts":false`)
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	if g.Reasoning.IsPresent() || len(losses) != 0 {
		t.Fatalf("false thoughts must be silent absence: %+v %v", g.Reasoning, losses)
	}
	// Thoughts plus a real control: intent present, thoughts loss retained.
	g, losses, rerr = gemDecodeChat(t, `"includeThoughts":true,"thinkingLevel":"LOW"`)
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	if lvl, _ := g.Reasoning.Level(); lvl != "low" {
		t.Fatalf("level lost with thoughts: %+v", g.Reasoning)
	}
}

func TestGeminiAnthropicThinkingUnchanged(t *testing.T) {
	// Valid Anthropic-destination controls stay diagnosed losses (no intent).
	_, losses, rerr := DecodeGeminiForAnthropic([]byte(gemBody(`"thinkingBudget":100`)), "m", false, DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	found := false
	for _, l := range losses {
		if l.Field == "generationConfig.thinkingConfig.thinkingBudget" {
			found = true
		}
	}
	if !found {
		t.Fatalf("anthropic budget must stay a loss: %v", losses)
	}
	_, losses, rerr = DecodeGeminiForAnthropic([]byte(gemBody(`"thinkingLevel":"LOW"`)), "m", false, DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	found = false
	for _, l := range losses {
		if l.Field == "generationConfig.thinkingConfig.thinkingLevel" {
			found = true
		}
	}
	if !found {
		t.Fatalf("anthropic level must stay a loss: %v", losses)
	}
	// XHIGH is malformed for every translated destination, Anthropic included.
	if _, _, rerr := DecodeGeminiForAnthropic([]byte(gemBody(`"thinkingLevel":"XHIGH"`)), "m", false, DefaultLimits()); rerr == nil || rerr.Status != 400 {
		t.Fatalf("anthropic XHIGH must 400: %v", rerr)
	}
}

func TestGeminiPlanOff(t *testing.T) {
	off, _ := NewGeminiBudget(0)
	if !off.IsNone() || off.Kind() != KindOff {
		t.Fatalf("budget 0 must be Off: %+v", off)
	}
	// Supported=false strips Off exactly.
	caps := gemCaps(func(c *capability.Contract) {
		gemPut(t, c, "reasoning.supported", capability.BoolValue(false))
	})
	if planned, losses := PlanEffort(off, caps, OrdinaryRoute()); len(losses) != 0 || planned.IsPresent() {
		t.Fatalf("supported=false Off must strip: %+v %v", planned, losses)
	}
	// Toggle=false drops Off.
	caps = gemCaps(func(c *capability.Contract) {
		gemPut(t, c, "reasoning.toggle.supported", capability.BoolValue(false))
	})
	if _, losses := PlanEffort(off, caps, OrdinaryRoute()); len(losses) != 1 {
		t.Fatalf("toggle=false Off must drop: %v", losses)
	}
	// Unknown caps: Chat ordinary exact Named none; LlamaCpp Chat exact Off.
	unknown := gemCaps(nil)
	if planned, losses := PlanEffort(off, unknown, OrdinaryRoute()); len(losses) != 0 {
		t.Fatalf("unknown Off must be neutral: %v", losses)
	} else if eff, ok := planned.Effort(); !ok || eff != "none" {
		t.Fatalf("ordinary Off must emit none: %+v", planned)
	}
	if planned, losses := PlanEffort(off, unknown, LocalRoute(capability.RuntimeLlamaCpp)); len(losses) != 0 || planned.Kind() != EffortOff {
		t.Fatalf("llama Off: %+v %v", planned, losses)
	}
	// Responses: Named none even on LlamaCpp.
	if planned, losses := PlanEffort(off, unknown, LocalResponsesRoute(capability.RuntimeLlamaCpp)); len(losses) != 0 {
		t.Fatalf("responses llama Off: %v", losses)
	} else if eff, ok := planned.Effort(); !ok || eff != "none" {
		t.Fatalf("responses Off must be Named none: %+v", planned)
	}
	// Known enum without none drops Off.
	vals, _ := capability.StringListValue([]string{"low", "medium"})
	caps = gemCaps(func(c *capability.Contract) { gemPut(t, c, "reasoning.effort.values", vals) })
	if _, losses := PlanEffort(off, caps, OrdinaryRoute()); len(losses) != 1 {
		t.Fatalf("enum-without-none must drop Off: %v", losses)
	}
}

func TestGeminiPlanBudget(t *testing.T) {
	budget, _ := NewGeminiBudget(2048)
	unknown := gemCaps(nil)
	// LlamaCpp Chat: exact budget wire.
	if planned, losses := PlanEffort(budget, unknown, LocalRoute(capability.RuntimeLlamaCpp)); len(losses) != 0 {
		t.Fatalf("llama budget: %v", losses)
	} else if b, ok := planned.Budget(); !ok || b != 2048 {
		t.Fatalf("llama budget wire: %+v", planned)
	}
	// Ordinary Chat, Strata Chat and every Responses destination drop.
	for _, scope := range []RouteScope{
		OrdinaryRoute(), LocalRoute(capability.RuntimeStrata),
		OrdinaryResponsesRoute(), LocalResponsesRoute(capability.RuntimeLlamaCpp),
		LocalResponsesRoute(capability.RuntimeStrata),
	} {
		if planned, losses := PlanEffort(budget, unknown, scope); len(losses) != 1 || planned.IsPresent() {
			t.Fatalf("budget must drop %+v: %+v %v", scope, planned, losses)
		}
	}
	// Budget gates: supported/min/max. Effort gates must NOT apply.
	cNoBudget := gemCaps(func(c *capability.Contract) {
		gemPut(t, c, "reasoning.budget.supported", capability.BoolValue(false))
	})
	if _, losses := PlanEffort(budget, cNoBudget, LocalRoute(capability.RuntimeLlamaCpp)); len(losses) != 1 || !strings.Contains(losses[0].Reason, "budget") {
		t.Fatalf("budget.supported=false: %v", losses)
	}
	cMin := gemCaps(func(c *capability.Contract) { gemPut(t, c, "reasoning.budget.min_tokens", capability.IntValue(4096)) })
	if _, losses := PlanEffort(budget, cMin, LocalRoute(capability.RuntimeLlamaCpp)); len(losses) != 1 {
		t.Fatalf("budget min: %v", losses)
	}
	cMax := gemCaps(func(c *capability.Contract) { gemPut(t, c, "reasoning.budget.max_tokens", capability.IntValue(1024)) })
	if _, losses := PlanEffort(budget, cMax, LocalRoute(capability.RuntimeLlamaCpp)); len(losses) != 1 {
		t.Fatalf("budget max: %v", losses)
	}
	// Named levels ignore budget gates: effort-supported false still drops
	// named, but budget-supported false must NOT drop named.
	named, _ := NewGeminiLevel("medium")
	cBudgetFalse := gemCaps(func(c *capability.Contract) {
		gemPut(t, c, "reasoning.budget.supported", capability.BoolValue(false))
	})
	if _, losses := PlanEffort(named, cBudgetFalse, LocalRoute(capability.RuntimeLlamaCpp)); len(losses) != 0 {
		t.Fatalf("named must ignore budget.supported: %v", losses)
	}
	cEffortFalse := gemCaps(func(c *capability.Contract) {
		gemPut(t, c, "reasoning.effort.supported", capability.BoolValue(false))
	})
	if _, losses := PlanEffort(named, cEffortFalse, OrdinaryRoute()); len(losses) != 1 {
		t.Fatalf("named keeps effort gate: %v", losses)
	}
	_ = gemInt
}

func TestGeminiPlanNamed(t *testing.T) {
	named, _ := NewGeminiLevel("medium")
	unknown := gemCaps(nil)
	// Ordinary + Strata: literal; Responses (even LlamaCpp): literal.
	for _, scope := range []RouteScope{
		OrdinaryRoute(), LocalRoute(capability.RuntimeStrata),
		OrdinaryResponsesRoute(), LocalResponsesRoute(capability.RuntimeLlamaCpp),
	} {
		if planned, losses := PlanEffort(named, unknown, scope); len(losses) != 0 {
			t.Fatalf("named neutral %+v: %v", scope, losses)
		} else if eff, ok := planned.Effort(); !ok || eff != "medium" {
			t.Fatalf("literal %+v: %+v", scope, planned)
		}
	}
	// LlamaCpp Chat maps through the shared table (medium=8192), never clamped.
	if planned, losses := PlanEffort(named, unknown, LocalRoute(capability.RuntimeLlamaCpp)); len(losses) != 0 {
		t.Fatalf("llama named: %v", losses)
	} else if b, ok := planned.Budget(); !ok || b != 8192 {
		t.Fatalf("llama medium: %+v", planned)
	}
	minimal, _ := NewGeminiLevel("minimal")
	if p, losses := PlanEffort(minimal, unknown, LocalRoute(capability.RuntimeLlamaCpp)); len(losses) != 0 {
		t.Fatalf("llama minimal: %v", losses)
	} else if b, ok := p.Budget(); !ok || b != 1024 {
		t.Fatalf("llama minimal budget: %+v", p)
	}
	// Known-empty admits nothing; unknown stays neutral; no clamp/rename.
	vals, _ := capability.StringListValue([]string{"low", "medium"})
	caps := gemCaps(func(c *capability.Contract) { gemPut(t, c, "reasoning.effort.values", vals) })
	if p, losses := PlanEffort(minimal, caps, OrdinaryRoute()); len(losses) != 1 || p.IsPresent() {
		t.Fatalf("minimal must drop, never low: %+v %v", p, losses)
	}
	empty, _ := capability.StringListValue([]string{})
	emptyCaps := gemCaps(func(c *capability.Contract) { gemPut(t, c, "reasoning.effort.values", empty) })
	if _, losses := PlanEffort(named, emptyCaps, OrdinaryRoute()); len(losses) != 1 {
		t.Fatalf("known-empty must drop: %v", losses)
	}
	if _, losses := PlanEffort(named, unknown, OrdinaryRoute()); len(losses) != 0 {
		t.Fatalf("unknown must be neutral: %v", losses)
	}
}

func TestGeminiPlanUnobserved(t *testing.T) {
	budget, _ := NewGeminiBudget(100)
	named, _ := NewGeminiLevel("low")
	off, _ := NewGeminiBudget(0)
	unknown := gemCaps(nil)
	// Present intent on unknown cached runtimes drops in the shared planner.
	for _, intent := range []ReasoningIntent{budget, named, off} {
		for _, scope := range []RouteScope{
			LocalRoute(capability.RuntimeUnobserved),
			LocalResponsesRoute(capability.RuntimeUnobserved),
		} {
			if planned, losses := PlanEffort(intent, unknown, scope); len(losses) != 1 || planned.IsPresent() {
				t.Fatalf("unobserved must drop %+v: %+v %v", scope, planned, losses)
			} else if losses[0].Action != "dropped" || losses[0].Field != intent.Origin() {
				t.Fatalf("unobserved loss: %+v", losses[0])
			}
		}
	}
	// Absent bypasses the gate (defaults, -1, UNSPECIFIED, bare thoughts).
	if planned, losses := PlanEffort(AbsentReasoning(), unknown, LocalRoute(capability.RuntimeUnobserved)); len(losses) != 0 || planned.IsPresent() {
		t.Fatalf("absent must bypass: %+v %v", planned, losses)
	}
	// Supported=false + Off still strips exactly (no invented refusal).
	caps := gemCaps(func(c *capability.Contract) {
		gemPut(t, c, "reasoning.supported", capability.BoolValue(false))
	})
	if planned, losses := PlanEffort(off, caps, LocalRoute(capability.RuntimeUnobserved)); len(losses) != 0 || planned.IsPresent() {
		t.Fatalf("supported=false Off strips: %+v %v", planned, losses)
	}
}

func TestGeminiEncodeWire(t *testing.T) {
	gen := func() *Generation {
		return &Generation{Model: "m", Messages: []Message{{Role: RoleUser, Texts: []TextContent{{Text: "hi"}}}}}
	}
	// Chat renders all four targets.
	cases := []struct {
		name    string
		planned PlannedEffort
		check   func(t *testing.T, body map[string]any)
	}{
		{"absent", AbsentEffort(), func(t *testing.T, body map[string]any) {
			if _, has := body["reasoning_effort"]; has {
				t.Fatalf("absent must emit nothing")
			}
		}},
		{"named", NamedEffortTarget("low"), func(t *testing.T, body map[string]any) {
			if body["reasoning_effort"] != "low" {
				t.Fatalf("named wire: %v", body)
			}
		}},
		{"off", OffEffortTarget(), func(t *testing.T, body map[string]any) {
			kw, ok := body["chat_template_kwargs"].(map[string]any)
			if !ok || kw["enable_thinking"] != false {
				t.Fatalf("off wire: %v", body)
			}
		}},
		{"budget", BudgetEffortTarget(2048), func(t *testing.T, body map[string]any) {
			v, ok := body["thinking_budget_tokens"].(float64)
			if !ok || v != 2048 {
				t.Fatalf("budget wire: %v", body)
			}
		}},
	}
	for _, tc := range cases {
		out, rerr := EncodeChatRequest(gen(), tc.planned, "up")
		if rerr != nil {
			t.Fatalf("%s encode: %v", tc.name, rerr)
		}
		var body map[string]any
		if err := json.Unmarshal(out, &body); err != nil {
			t.Fatal(err)
		}
		tc.check(t, body)
	}
	// Responses admits only Named/Absent; Budget/Off are internal misuse.
	out, rerr := EncodeResponsesRequest(gen(), NamedEffortTarget("low"), "up")
	if rerr != nil {
		t.Fatalf("responses named: %v", rerr)
	}
	var rbody map[string]any
	if err := json.Unmarshal(out, &rbody); err != nil {
		t.Fatal(err)
	}
	if reas, ok := rbody["reasoning"].(map[string]any); !ok || reas["effort"] != "low" {
		t.Fatalf("responses wire: %s", out)
	}
	if _, rerr := EncodeResponsesRequest(gen(), BudgetEffortTarget(100), "up"); rerr == nil || rerr.Status != 500 {
		t.Fatalf("responses budget must be 500 misuse: %v", rerr)
	}
	if _, rerr := EncodeResponsesRequest(gen(), OffEffortTarget(), "up"); rerr == nil || rerr.Status != 500 {
		t.Fatalf("responses off must be 500 misuse: %v", rerr)
	}
	// The planner never sends those to the Responses encoder: Budget drops first.
	budget, _ := NewGeminiBudget(100)
	if planned, losses := PlanEffort(budget, gemCaps(nil), OrdinaryResponsesRoute()); len(losses) != 1 || planned.IsPresent() {
		t.Fatalf("responses budget must drop pre-encoder: %+v %v", planned, losses)
	}
}

func TestGeminiOriginClosed(t *testing.T) {
	chat, _ := NewChatEffort("medium")
	if chat.Origin() != "reasoning_effort" {
		t.Fatalf("chat: %q", chat.Origin())
	}
	resp, _ := NewResponsesEffort("medium")
	if resp.Origin() != "reasoning.effort" {
		t.Fatalf("responses: %q", resp.Origin())
	}
	budget, _ := NewGeminiBudget(1)
	if budget.Origin() != "generationConfig.thinkingConfig.thinkingBudget" {
		t.Fatalf("budget: %q", budget.Origin())
	}
	level, _ := NewGeminiLevel("low")
	if level.Origin() != "generationConfig.thinkingConfig.thinkingLevel" {
		t.Fatalf("level: %q", level.Origin())
	}
	if GeminiBudgetOrigin.Path() != "generationConfig.thinkingConfig.thinkingBudget" ||
		GeminiLevelOrigin.Path() != "generationConfig.thinkingConfig.thinkingLevel" {
		t.Fatalf("paths must be static closed values")
	}
}
