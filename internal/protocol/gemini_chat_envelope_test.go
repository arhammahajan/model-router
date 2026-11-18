package protocol

import (
	"bytes"
	"strings"
	"testing"
)

func TestGeminiEnvelopeValidation(t *testing.T) {
	// Closed response requires a stable identity.
	expectGemini502(t, strings.Replace(
		chatEnvelope(chatChoice(`"stop"`, `{"role":"assistant","content":"x"}`), usageTriple(1, 1)),
		`"id":"chatcmpl-1",`, ``, 1))
	// Mistyped allowed envelope members fail before any write.
	expectGemini502(t, `{"id":"chatcmpl-1","object":17,"created":1,"model":"up",
		"choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	// Deprecated finish without a representable call never reports STOP.
	expectGemini502(t, chatEnvelope(
		chatChoice(`"function_call"`, `{"role":"assistant","content":"x"}`), usageTriple(1, 1)))
	// Stream envelope types fail before the first downstream frame.
	bad := g2cSSEData(`{"id":"chatcmpl-s","object":17,"created":1,"model":"up",
		"choices":[{"index":0,"delta":{"content":"hi"}}]}`) +
		g2cChunk(`{}`, `"stop"`, "") + g2cUsageChunk() + "data: [DONE]\n\n"
	var buf bytes.Buffer
	tr := &ChatToGeminiSSETranslator{PublicModel: "p", Limits: DefaultLimits()}
	if rerr := tr.Translate(strings.NewReader(bad), &buf, nil); rerr == nil || rerr.Status != 502 {
		t.Fatalf("expected stream envelope 502, got %v", rerr)
	}
	if buf.Len() != 0 {
		t.Fatalf("envelope failure must precede first write: %q", buf.String())
	}
}
