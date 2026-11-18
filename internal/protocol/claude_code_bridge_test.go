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
