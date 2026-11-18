package protocol

import (
	"strings"
	"testing"
)

func TestAnthropicEncoderExactlyOneStopPerBlock(t *testing.T) {
	usage := `{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}`
	for _, delta := range []string{`{"content":"hello"}`, `{"tool_calls":[{"index":0,"id":"call:opaque","type":"function","function":{"name":"f","arguments":"{}"}}]}`} {
		raw := chatChunk(delta, "", "") + chatChunk(`{}`, "stop", "") + chatUsageChunk(usage) + "data: [DONE]\n\n"
		out, err := runChatStream(t, raw, DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		if n := strings.Count(out, "event: content_block_stop\n"); n != 1 {
			t.Fatalf("got %d block stops, want one", n)
		}
	}
}
func TestAnthropicEncoderInitialUsageInvalidDoesNotFabricateZero(t *testing.T) {
	for _, initial := range []ResponseUsage{
		{Prompt: intPtr(-1), Completion: intPtr(1), Provenance: UsageProvenance{Source: DialectAnthropic, Scope: UsageInitial}},
		{Prompt: intPtr(1), Completion: intPtr(1), Reasoning: intPtr(1), Provenance: UsageProvenance{Source: DialectAnthropic, Scope: UsageInitial}},
		{Prompt: intPtr(1), Completion: intPtr(1), Provenance: UsageProvenance{Source: DialectChat, Scope: UsageInitial}},
	} {
		enc := NewAnthropicEventEncoder("public", DefaultLimits())
		var out strings.Builder
		if err := enc.Encode(&out, nil, ResponseStart{ID: "r", Model: "m", Source: DialectAnthropic, InitialUsage: &initial}); err != nil {
			continue
		}
		open, _ := NewBlockOpen("t", BlockText, "", "")
		if err := enc.Encode(&out, nil, open); err == nil {
			t.Fatal("invalid/unrepresentable initial usage accepted")
		}
		if out.Len() != 0 {
			t.Fatalf("invalid initial usage wrote %d bytes", out.Len())
		}
	}
}
