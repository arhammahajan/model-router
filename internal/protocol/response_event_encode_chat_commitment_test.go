package protocol

import (
	"strings"
	"testing"
)

func TestChatEncoderFlushesValidatedStartBeforeContent(t *testing.T) {
	enc := NewChatEventEncoder("public", false, DefaultLimits())
	var out strings.Builder
	flushes := 0
	if err := enc.Encode(&out, func() { flushes++ }, ResponseStart{ID: "m", Source: DialectAnthropic}); err != nil {
		t.Fatal(err)
	}
	if flushes != 1 || !enc.Committed() || !strings.Contains(out.String(), `"role":"assistant"`) {
		t.Fatalf("start did not flush role before content: flushes=%d, output=%s", flushes, out.String())
	}
	if strings.Contains(out.String(), "[DONE]") {
		t.Fatal("start fabricated completion")
	}
}
