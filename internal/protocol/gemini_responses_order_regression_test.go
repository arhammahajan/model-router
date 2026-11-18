package protocol

import (
	"strings"
	"testing"
)

func TestResponsesToGeminiInterleavedTextOrder(t *testing.T) {
	stream := r2gCreated(0) + r2gMsgAdded(1, 0, "msg0") + r2gPartAdded(2, 0, "msg0") +
		r2gDelta(3, 0, "msg0", "a") + r2gMsgAdded(4, 1, "msg1") + r2gPartAdded(5, 1, "msg1") +
		r2gDelta(6, 1, "msg1", "B") + r2gDelta(7, 0, "msg0", "c") +
		r2gTextDone(8, 0, "msg0", "ac") + r2gPartDone(9, 0, "msg0", "ac") + r2gItemDoneMsg(10, 0, "msg0", "ac") +
		r2gTextDone(11, 1, "msg1", "B") + r2gPartDone(12, 1, "msg1", "B") + r2gItemDoneMsg(13, 1, "msg1", "B") +
		r2gTerminal(14, "completed", `"completed"`, `null`,
			`[{"type":"message","id":"msg0","status":"completed","role":"assistant","content":[{"type":"output_text","text":"ac"}]},{"type":"message","id":"msg1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"B"}]}]`)
	for _, framing := range []string{"sse", "array"} {
		t.Run(framing, func(t *testing.T) {
			var out strings.Builder
			var rerr *RouterError
			if framing == "sse" {
				tr := ResponsesToGeminiSSETranslator{PublicModel: "public", Limits: DefaultLimits()}
				rerr = tr.Translate(strings.NewReader(stream), &out, nil)
			} else {
				tr := ResponsesToGeminiJSONArrayTranslator{PublicModel: "public", Limits: DefaultLimits()}
				rerr = tr.Translate(strings.NewReader(stream), &out, nil)
			}
			if rerr == nil || rerr.Status != 502 {
				t.Fatalf("interleaved items would reorder ac,B to a,B,c: %v", rerr)
			}
			if strings.Contains(out.String(), `"text":"B"`) || strings.Contains(out.String(), `"finishReason"`) {
				t.Fatal("reordered text or a success marker was emitted")
			}
		})
	}
}
