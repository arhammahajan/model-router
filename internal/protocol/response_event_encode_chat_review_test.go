package protocol

import (
	"strings"
	"testing"
)

func TestChatEncoderAnthropicAbsentModelIsNotInvented(t *testing.T) {
	enc := NewChatEventEncoder("public", true, DefaultLimits())
	var out strings.Builder
	initial := ResponseUsage{Prompt: intPtr(5), Provenance: UsageProvenance{Source: DialectAnthropic, Scope: UsageInitial}}
	usage, err := NewTerminalUsage(DialectAnthropic, 5, 2, 7)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range []ResponseEvent{
		ResponseStart{ID: "msg-minimal", Source: DialectAnthropic, InitialUsage: &initial},
		BlockOpen{Item: "text", Kind: BlockText}, TextFragment{Item: "text", Text: "hi"}, BlockClose{Item: "text"},
		UsageEvent{Usage: usage}, CompletionEvent{Finish: Finish{Kind: FinishStop}},
	} {
		if err := enc.Encode(&out, nil, ev); err != nil {
			t.Fatalf("truthful absent model refused: %v", err.Err)
		}
	}
	if !strings.Contains(out.String(), `"model":"public"`) || !strings.Contains(out.String(), `"prompt_tokens":5`) || !strings.Contains(out.String(), "[DONE]") {
		t.Fatalf("client model or accounting lost: %s", out.String())
	}
}
