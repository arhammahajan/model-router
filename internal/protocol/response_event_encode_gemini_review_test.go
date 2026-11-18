package protocol

import (
	"strings"
	"testing"
)

func TestGeminiEventEncoderPreservesAccountingSource(t *testing.T) {
	enc := NewGeminiEventEncoder("public", DefaultLimits(), true)
	var out strings.Builder
	usage, err := NewTerminalUsage(DialectAnthropic, 17, 7, 24)
	if err != nil {
		t.Fatal(err)
	}
	usage, err = usage.WithCache(intPtr(3), intPtr(4))
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range []ResponseEvent{ResponseStart{ID: "msg-source", Model: "actual-model", Source: DialectAnthropic}, UsageEvent{Usage: usage}, CompletionEvent{Finish: Finish{Kind: FinishStop}}} {
		if err := enc.Encode(&out, nil, ev); err != nil {
			t.Fatalf("representable Anthropic creation accounting refused: %v", err.Err)
		}
	}
	if !strings.Contains(out.String(), `"promptTokenCount":17`) || !strings.Contains(out.String(), `"cachedContentTokenCount":4`) {
		t.Fatalf("usage not preserved: %s", out.String())
	}
}
func TestGeminiEventEncoderKeepsSourceSpecificNameGate(t *testing.T) {
	enc := NewGeminiEventEncoder("public", DefaultLimits(), true)
	var out strings.Builder
	open, err := NewBlockOpen("item", BlockTool, "opaque", "has space")
	if err != nil {
		t.Fatal(err)
	}
	usage, err := NewTerminalUsage(DialectAnthropic, 1, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range []ResponseEvent{ResponseStart{ID: "msg-source", Model: "actual-model", Source: DialectAnthropic}, open, ArgsFragment{Item: "item", Fragment: "{}"}, BlockClose{Item: "item"}, UsageEvent{Usage: usage}} {
		if err := enc.Encode(&out, nil, ev); err != nil {
			return
		}
	}
	if err := enc.Encode(&out, nil, CompletionEvent{Finish: Finish{Kind: FinishToolCalls}}); err == nil {
		t.Fatal("Anthropic source name-grammar gate lost")
	}
	if out.Len() != 0 {
		t.Fatal("invalid name committed before source policy check")
	}
}
