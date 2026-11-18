package protocol

import (
	"strings"
	"testing"
)

func TestClaudeCodeAdaptiveEffortAndInlineSystem(t *testing.T) {
	raw := []byte(`{"model":"m","max_tokens":512,"messages":[{"role":"user","content":"question"},{"role":"system","content":"follow-up instruction"}],"thinking":{"type":"adaptive"},"output_config":{"effort":"low"}}`)
	gen, losses, err := DecodeAnthropicForResponses(raw, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(losses) != 2 || losses[0].Field != "output_config.effort" || losses[1].Field != "thinking" {
		t.Fatalf("unexpected translation losses: %v", losses)
	}
	if _, err := EnforcePolicy(losses, false); err == nil || err.Code != CodeTranslationLoss {
		t.Fatalf("strict mode must refuse adaptive projection: %v", err)
	}
	plan, planLosses := PlanEffort(gen.Reasoning, anthDisabledContract(t, nil), OrdinaryResponsesRoute())
	if len(planLosses) != 0 {
		t.Fatalf("unexpected effort loss: %v", planLosses)
	}
	out, err := EncodeResponsesRequest(gen, plan, "upstream")
	user := strings.Index(string(out), `"role":"user"`)
	system := strings.Index(string(out), `"role":"system"`)
	if err != nil || user < 0 || system <= user || !strings.Contains(string(out), `"reasoning":{"effort":"low"}`) {
		t.Fatalf("lost effort or message order: %s, %v", out, err)
	}
}

func TestClaudeCodeAdaptiveEffortGemini(t *testing.T) {
	// Captured Claude Code 2.1 shape: thinking:{"type":"adaptive"} plus
	// output_config.effort, metadata, a system array and an inline
	// role:"system" message. The Gemini destination now decodes the
	// named effort instead of refusing the adaptive mode.
	raw := []byte(`{"model":"m","max_tokens":512,` +
		`"system":[{"type":"text","text":"You are Claude Code."}],` +
		`"messages":[{"role":"user","content":"question"},{"role":"system","content":"follow-up instruction"}],` +
		`"thinking":{"type":"adaptive"},"output_config":{"effort":"medium"},` +
		`"metadata":{"user_id":"user_1"}}`)
	gen, losses, err := DecodeAnthropicForGemini(raw, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	level, isNamed := gen.Reasoning.Level()
	if !isNamed || level != "medium" {
		t.Fatalf("adaptive+effort must decode to the named intent, got %+v", gen.Reasoning)
	}
	fields := map[string]bool{}
	for _, l := range losses {
		fields[l.Field] = true
	}
	for _, want := range []string{"metadata", "output_config.effort", "thinking"} {
		if !fields[want] {
			t.Fatalf("expected diagnosed loss %q, got %+v", want, losses)
		}
	}
	// Strict refuses the losses pre-I/O; lossy keeps them for the header.
	if _, rerr := EnforcePolicy(losses, false); rerr == nil || rerr.Code != CodeTranslationLoss {
		t.Fatalf("strict mode must refuse the adaptive projection: %v", rerr)
	}
	if _, rerr := EnforcePolicy(losses, true); rerr != nil {
		t.Fatalf("lossy mode must proceed: %v", rerr)
	}
	// Shared planner: unknown native facts (gemini-4-argon carries no
	// thinkingLevel descriptor) drop the level; the wire never guesses.
	plan, planLosses := PlanEffort(gen.Reasoning, gemBudgetCaps(t, nil), OrdinaryGeminiRoute())
	if plan.IsPresent() || len(planLosses) != 1 {
		t.Fatalf("unknown caps must drop the named level: %+v %+v", plan, planLosses)
	}
	out, rerr := EncodeGeminiRequestWithPlan(gen, plan)
	if rerr != nil {
		t.Fatal(rerr)
	}
	s := string(out)
	if strings.Contains(s, "thinkingConfig") {
		t.Fatalf("no thinking control may be invented: %s", s)
	}
	if !strings.Contains(s, `"text":"You are Claude Code."`) || !strings.Contains(s, `"text":"follow-up instruction"`) {
		t.Fatalf("system array and inline system message must both reach systemInstruction: %s", s)
	}
	if !strings.Contains(s, `"text":"question"`) {
		t.Fatalf("user content missing: %s", s)
	}
}

func TestClaudeCodeToolArgumentsDoneName(t *testing.T) {
	frames := []Frame{
		rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
		rsF("response.output_item.added", rsToolAdded(t, 1, 0, "fc_1", "call_1", "Read", "")),
		rsF("response.function_call_arguments.delta", rsArgsDelta(t, 2, 0, "fc_1", "{}")),
		rsF("response.function_call_arguments.done", `{"type":"response.function_call_arguments.done","sequence_number":3,"output_index":0,"item_id":"fc_1","arguments":"{}","name":"Read"}`),
		rsF("response.output_item.done", rsItemDoneTool(t, 4, 0, "fc_1", "call_1", "Read", "{}")),
		rsF("response.completed", rsTerminal(t, 5, "completed", rsResp(t, "completed", []any{rsToolOut("fc_1", "call_1", "Read", "{}")}, rsUsage(1, 1), ""))),
	}
	rsFullSuccess(t, frames)
	frames[3].Data = strings.Replace(frames[3].Data, `"Read"`, `"other"`, 1)
	rsMustFailAt(t, frames, 3)
}
