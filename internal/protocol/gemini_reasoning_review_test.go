package protocol

import (
	"strings"
	"testing"

	"github.com/arhammahajan/router/internal/capability"
)

// Independent adversarial review of the Gemini-request reasoning vertical.
// Stable source: git show HEAD:model_router/{gemini,reasoning,protocol}.py
// (constants only). Hermetic: no network, env, or Python tree.
//
// Focus: reachable decode/planner gaps the ordinary-bridge tests do not
// pin down - null-mixed conflict absence, snake collision, Unobserved vs
// absent separation, includeThoughts composition, budget/effort gate
// separation on Llama, value-free diagnostics, constructor invariants.

func revGemBody(thinking string) string {
	return `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{` + thinking + `}}}`
}

func revGemChat(t *testing.T, thinking string) (*Generation, []Loss, *RouterError) {
	t.Helper()
	return DecodeGeminiRequest([]byte(revGemBody(thinking)), "m", false, DefaultLimits())
}

func revGemResp(t *testing.T, thinking string) (*Generation, []Loss, *RouterError) {
	t.Helper()
	return DecodeGeminiForResponses([]byte(revGemBody(thinking)), "m", false, DefaultLimits())
}

func revGemCap(fn func(c *capability.Contract)) capability.Contract {
	c := capability.Empty("openai-responses")
	if fn != nil {
		fn(&c)
	}
	c.Normalize()
	return c
}

func revGemCaps(fn func(c *capability.Contract)) capability.ReasoningCapabilities {
	return capability.ReasoningFor(revGemCap(fn))
}

func revGemPut(t *testing.T, c *capability.Contract, path string, v capability.Value) {
	t.Helper()
	s := "gemini-review-test"
	if err := c.Put(path, v, &s, capability.KindOverride); err != nil {
		t.Fatalf("put %s: %v", path, err)
	}
}

func TestRevGemNullMixedConflictAbsent(t *testing.T) {
	// Explicit null on one member means absent: no both-controls refusal.
	g, losses, rerr := revGemChat(t, `"thinkingBudget":null,"thinkingLevel":"LOW"`)
	if rerr != nil {
		t.Fatalf("null budget + level must not conflict: %v", rerr)
	}
	if len(losses) != 0 {
		t.Fatalf("no decode loss expected: %v", losses)
	}
	if lvl, ok := g.Reasoning.Level(); !ok || lvl != "low" {
		t.Fatalf("level must survive null budget: %+v", g.Reasoning)
	}
	g, losses, rerr = revGemChat(t, `"thinkingBudget":100,"thinkingLevel":null`)
	if rerr != nil {
		t.Fatalf("budget + null level must not conflict: %v", rerr)
	}
	if len(losses) != 0 {
		t.Fatalf("no decode loss expected: %v", losses)
	}
	if b, ok := g.Reasoning.Budget(); !ok || b != 100 {
		t.Fatalf("budget must survive null level: %+v", g.Reasoning)
	}
	// Same for the Responses destination decoder.
	g, _, rerr = revGemResp(t, `"thinkingBudget":null,"thinkingLevel":"MEDIUM"`)
	if rerr != nil {
		t.Fatalf("responses null-mixed: %v", rerr)
	}
	if lvl, _ := g.Reasoning.Level(); lvl != "medium" {
		t.Fatalf("responses level: %+v", g.Reasoning)
	}
}

func TestRevGemSnakeCollisionAndAliasOrigin(t *testing.T) {
	// Same member in both spellings is a 400 collision, both modes.
	raw := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingBudget":1,"thinking_budget":2}}}`)
	if _, losses, rerr := DecodeGeminiRequest(raw, "m", false, DefaultLimits()); rerr == nil || rerr.Status != 400 || len(losses) != 0 {
		t.Fatalf("snake collision must 400 pre-loss: %v %v", rerr, losses)
	}
	if _, losses, rerr := DecodeGeminiForResponses(raw, "m", false, DefaultLimits()); rerr == nil || rerr.Status != 400 || len(losses) != 0 {
		t.Fatalf("responses snake collision must 400 pre-loss: %v %v", rerr, losses)
	}
	// Snake-only alias decodes with the canonical static origin.
	g, _, rerr := DecodeGeminiRequest([]byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generation_config":{"thinking_config":{"thinking_level":"HIGH"}}}`), "m", false, DefaultLimits())
	if rerr != nil {
		t.Fatalf("snake alias: %v", rerr)
	}
	if lvl, _ := g.Reasoning.Level(); lvl != "high" {
		t.Fatalf("snake level: %+v", g.Reasoning)
	}
	if got := g.Reasoning.Origin(); got != "generationConfig.thinkingConfig.thinkingLevel" {
		t.Fatalf("alias must keep canonical origin: %q", got)
	}
}

func TestRevGemAbsentBypassesUnobserved(t *testing.T) {
	unknown := revGemCaps(nil)
	unobs := LocalRoute(capability.RuntimeUnobserved)
	unobsResp := LocalResponsesRoute(capability.RuntimeUnobserved)
	// -1, UNSPECIFIED, null, {}, bare thoughts: all absent, all bypass.
	for _, th := range []string{
		`"thinkingBudget":-1`,
		`"thinkingLevel":"THINKING_LEVEL_UNSPECIFIED"`,
		`"thinkingBudget":null`,
		``,
		`"includeThoughts":true`,
		`"includeThoughts":false`,
	} {
		g, _, rerr := revGemChat(t, th)
		if rerr != nil {
			t.Fatalf("%q decode: %v", th, rerr)
		}
		if g.Reasoning.IsPresent() {
			t.Fatalf("%q must be absent: %+v", th, g.Reasoning)
		}
		if planned, losses := PlanEffort(g.Reasoning, unknown, unobs); len(losses) != 0 || planned.IsPresent() {
			t.Fatalf("%q must bypass unobserved gate: %+v %v", th, planned, losses)
		}
		if planned, losses := PlanEffort(g.Reasoning, unknown, unobsResp); len(losses) != 0 || planned.IsPresent() {
			t.Fatalf("%q must bypass responses-unobserved gate: %+v %v", th, planned, losses)
		}
	}
	// Off (budget 0) is present and must drop on unobserved, unlike -1.
	off, _ := NewGeminiBudget(0)
	if planned, losses := PlanEffort(off, unknown, unobs); len(losses) != 1 || planned.IsPresent() {
		t.Fatalf("Off must drop on unobserved: %+v %v", planned, losses)
	}
}

func TestRevGemThoughtsCompositionStrict(t *testing.T) {
	// includeThoughts-only: one independent loss, no intent, no plan loss.
	g, losses, rerr := revGemChat(t, `"includeThoughts":true`)
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	if g.Reasoning.IsPresent() {
		t.Fatalf("thoughts-only must create no intent: %+v", g.Reasoning)
	}
	if len(losses) != 1 || losses[0].Field != "generationConfig.thinkingConfig.includeThoughts" {
		t.Fatalf("thoughts loss missing: %v", losses)
	}
	unknown := revGemCaps(nil)
	if planned, planLosses := PlanEffort(g.Reasoning, unknown, LocalRoute(capability.RuntimeUnobserved)); len(planLosses) != 0 || planned.IsPresent() {
		t.Fatalf("absent must add no plan loss even unobserved: %+v %v", planned, planLosses)
	}
	// Strict refuses on the thoughts loss alone (translation_loss, not a
	// fake intent or refusal code path).
	if _, rerr := EnforcePolicy(losses, false); rerr == nil || rerr.Status != 400 || rerr.Code != CodeTranslationLoss {
		t.Fatalf("thoughts strict must be translation_loss 400: %v", rerr)
	}
	// Thoughts plus a real control: both losses retained, level intact.
	g2, losses2, rerr := revGemChat(t, `"includeThoughts":true,"thinkingLevel":"LOW"`)
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	if lvl, _ := g2.Reasoning.Level(); lvl != "low" {
		t.Fatalf("level lost: %+v", g2.Reasoning)
	}
	if len(losses2) != 1 {
		t.Fatalf("decode must carry exactly the thoughts loss: %v", losses2)
	}
	planned, planLosses := PlanEffort(g2.Reasoning, unknown, LocalRoute(capability.RuntimeUnobserved))
	if len(planLosses) != 1 || planned.IsPresent() {
		t.Fatalf("unobserved must drop present intent: %+v %v", planned, planLosses)
	}
	if planLosses[0].Field != "generationConfig.thinkingConfig.thinkingLevel" {
		t.Fatalf("unobserved loss must carry the level origin: %+v", planLosses[0])
	}
	all := append(append([]Loss{}, losses2...), planLosses...)
	if len(all) != 2 {
		t.Fatalf("composition must keep both losses: %v", all)
	}
}

func TestRevGemBudgetEffortGateSeparation(t *testing.T) {
	// Explicit budgets never consult effort gates; named levels never
	// consult budget gates - on the LlamaCpp Chat route.
	llama := LocalRoute(capability.RuntimeLlamaCpp)
	effOff := revGemCaps(func(c *capability.Contract) {
		revGemPut(t, c, "reasoning.effort.supported", capability.BoolValue(false))
	})
	budget, _ := NewGeminiBudget(2048)
	if _, losses := PlanEffort(budget, effOff, llama); len(losses) != 0 {
		t.Fatalf("budget must ignore effort.supported=false: %v", losses)
	}
	vals, _ := capability.StringListValue([]string{"low"})
	effVals := revGemCaps(func(c *capability.Contract) { revGemPut(t, c, "reasoning.effort.values", vals) })
	if _, losses := PlanEffort(budget, effVals, llama); len(losses) != 0 {
		t.Fatalf("budget must ignore effort values: %v", losses)
	}
	named, _ := NewGeminiLevel("medium")
	bMin := revGemCaps(func(c *capability.Contract) {
		revGemPut(t, c, "reasoning.budget.min_tokens", capability.IntValue(1<<20))
	})
	if _, losses := PlanEffort(named, bMin, llama); len(losses) != 0 {
		t.Fatalf("named must ignore budget.min: %v", losses)
	}
	bMax := revGemCaps(func(c *capability.Contract) { revGemPut(t, c, "reasoning.budget.max_tokens", capability.IntValue(1)) })
	if _, losses := PlanEffort(named, bMax, llama); len(losses) != 0 {
		t.Fatalf("named must ignore budget.max: %v", losses)
	}
	bOff := revGemCaps(func(c *capability.Contract) {
		revGemPut(t, c, "reasoning.budget.supported", capability.BoolValue(false))
	})
	if _, losses := PlanEffort(named, bOff, llama); len(losses) != 0 {
		t.Fatalf("named must ignore budget.supported: %v", losses)
	}
}

func TestRevGemLossHeaderStaticValueFree(t *testing.T) {
	budget, _ := NewGeminiBudget(2048)
	planned, losses := PlanEffort(budget, revGemCaps(nil), OrdinaryRoute())
	if len(losses) != 1 || planned.IsPresent() {
		t.Fatalf("ordinary budget must drop: %+v %v", planned, losses)
	}
	header := EncodeLosses(losses)
	if header == "" {
		t.Fatalf("drop must serialize a loss header")
	}
	if strings.Contains(header, "2048") {
		t.Fatalf("loss header must be static value-free: %s", header)
	}
	if !strings.Contains(header, "generationConfig.thinkingConfig.thinkingBudget") {
		t.Fatalf("loss must carry the static origin: %s", header)
	}
	kept, rerr := EnforcePolicy(losses, true)
	if rerr != nil {
		t.Fatalf("lossy keeps: %v", rerr)
	}
	if EncodeLosses(kept) != header {
		t.Fatalf("lossy must preserve the static header")
	}
}

func TestRevGemMalformedValueFree(t *testing.T) {
	// Overflow / bool-as-int / both-controls / unknown nested are 400
	// pre-loss in both modes, with value-free messages.
	for _, th := range []string{
		`"thinkingBudget":9223372036854775808`,
		`"thinkingBudget":true`,
		`"thinkingBudget":100,"thinkingLevel":"LOW"`,
		`"nope":1`,
		`"thinkingBudget":1.5`,
	} {
		_, losses, rerr := revGemChat(t, th)
		if rerr == nil || rerr.Status != 400 || len(losses) != 0 {
			t.Fatalf("chat %q must 400 pre-loss: %v %v", th, rerr, losses)
		}
		if strings.Contains(rerr.Msg, "9223372036854775808") || strings.Contains(rerr.Msg, "100") {
			t.Fatalf("malformed message must be value-free: %q", rerr.Msg)
		}
		_, losses, rerr = revGemResp(t, th)
		if rerr == nil || rerr.Status != 400 || len(losses) != 0 {
			t.Fatalf("responses %q must 400 pre-loss: %v %v", th, rerr, losses)
		}
	}
	// XHIGH is malformed for every translated destination, value-free.
	for _, th := range []string{`"thinkingLevel":"XHIGH"`} {
		_, _, rerr := revGemChat(t, th)
		if rerr == nil || rerr.Status != 400 || strings.Contains(rerr.Msg, "XHIGH") {
			t.Fatalf("chat XHIGH must 400 value-free: %v", rerr)
		}
		_, _, rerr = revGemResp(t, th)
		if rerr == nil || rerr.Status != 400 {
			t.Fatalf("responses XHIGH must 400: %v", rerr)
		}
	}
}

func TestRevGemConstructorInvariants(t *testing.T) {
	if _, ok := NewGeminiBudget(-1); ok {
		t.Fatalf("-1 must never construct (decodes directly to Absent)")
	}
	if _, ok := NewGeminiBudget(-2); ok {
		t.Fatalf("negative must never construct")
	}
	for _, lvl := range []string{"none", "xhigh", "XHIGH", "LOW", " low", "low ", "", "max"} {
		if _, ok := NewGeminiLevel(lvl); ok {
			t.Fatalf("level constructor admitted %q", lvl)
		}
	}
	intent, ok := NewGeminiBudget(0)
	if !ok || intent.Kind() != KindOff || intent.Origin() != "generationConfig.thinkingConfig.thinkingBudget" {
		t.Fatalf("budget 0 must be Off with budget origin: %+v", intent)
	}
	if lvl, ok := intent.Level(); !ok || lvl != "none" {
		t.Fatalf("Off Level() must report none: %+v", intent)
	}
}
