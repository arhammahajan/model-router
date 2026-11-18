package protocol

import (
	"strings"
	"testing"
)

func TestResponsesFinalAnswerPhase(t *testing.T) {
	base := reasonFullRaw("", "hello", reasonUsage())
	withPhase := func(raw, phase string) string {
		return strings.ReplaceAll(raw, `"role":"assistant"`, `"role":"assistant","phase":`+phase)
	}
	out, rerr := TranslateResponsesJSONToChat([]byte(withPhase(base, `"final_answer"`)), "public", DefaultLimits())
	if rerr != nil || !strings.Contains(string(out), "hello") || strings.Contains(string(out), "phase") {
		t.Fatalf("final answer failed to translate: %s, %v", out, rerr)
	}
	for _, phase := range []string{`"commentary"`, `"unknown"`, `false`} {
		if _, rerr := DecodeResponsesResponse([]byte(withPhase(base, phase)), DefaultLimits()); rerr == nil || rerr.Status != 502 {
			t.Fatalf("unsupported response phase accepted: %v", rerr)
		}
	}

	out, rerr = TranslateResponsesJSONToChatWithPolicy([]byte(withPhase(base, `"commentary"`)), "public", DefaultLimits(), true)
	if rerr != nil || !strings.Contains(string(out), "hello") || strings.Contains(string(out), "phase") {
		t.Fatalf("lossy commentary lost visible text: %s, %v", out, rerr)
	}

	frames := rsTextStream(t, 0, "hello", "hello")
	for i := range frames {
		frames[i].Data = withPhase(frames[i].Data, `"final_answer"`)
	}
	rsFullSuccess(t, frames)
	for _, i := range []int{1, 6, 7} {
		bad := append([]Frame(nil), frames...)
		bad[i].Data = strings.ReplaceAll(bad[i].Data, `"final_answer"`, `"commentary"`)
		rsMustFailAt(t, bad, i)
		if _, rerr := decodeWithPolicy(bad, true); rerr == nil || !strings.Contains(rerr.Msg, "changed its message phase") {
			t.Fatalf("changing phase within a message must fail even in lossy: %v", rerr)
		}
	}
	for i := range frames {
		frames[i].Data = strings.ReplaceAll(frames[i].Data, `"final_answer"`, `"commentary"`)
	}
	if _, rerr := decodeWithPolicy(frames, true); rerr != nil {
		t.Fatalf("consistent lossy commentary failed: %v", rerr)
	}
}
