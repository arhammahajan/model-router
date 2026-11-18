package protocol

import (
	"encoding/json"
	"testing"
)

func TestResponsesStreamItemStatusCannotChangeAtTerminal(t *testing.T) {
	tool := []Frame{
		rsF("response.created", rsCreatedRaw(t, 0, "resp_7", "gpt-4o")),
		rsF("response.output_item.added", rsToolAdded(t, 1, 0, "fc_1", "call_1", "f", "")),
		rsF("response.function_call_arguments.delta", rsArgsDelta(t, 2, 0, "fc_1", "{}")),
		rsF("response.function_call_arguments.done", rsArgsDone(t, 3, 0, "fc_1", "{}")),
		rsF("response.output_item.done", rsItemDoneTool(t, 4, 0, "fc_1", "call_1", "f", "{}")),
		rsF("response.completed", rsTerminal(t, 5, "completed", rsResp(t, "completed", []any{rsToolOut("fc_1", "call_1", "f", "{}")}, rsUsage(1, 1), ""))),
	}
	for _, tc := range []struct {
		name   string
		frames []Frame
	}{{"message", rsTextStream(t, 0, "hi", "hi")}, {"tool", tool}} {
		t.Run(tc.name, func(t *testing.T) {
			fs := tc.frames
			var b map[string]any
			if err := json.Unmarshal([]byte(fs[len(fs)-1].Data), &b); err != nil {
				t.Fatal(err)
			}
			b["response"].(map[string]any)["output"].([]any)[0].(map[string]any)["status"] = "incomplete"
			fs[len(fs)-1].Data = rsJ(t, b)
			d := NewResponsesStreamDecoder(DefaultLimits())
			for i, f := range fs {
				if _, err := d.DecodeFrame(f); err != nil {
					if i != len(fs)-1 {
						t.Fatalf("failed before terminal status check: %v", err)
					}
					return
				}
			}
			t.Fatal("completed item changed to incomplete at terminal")
		})
	}
}
