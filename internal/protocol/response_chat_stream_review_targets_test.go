package protocol

import (
	"bytes"
	"strings"
	"testing"
)

// Independent review probes for the three Chat-upstream runtime stream paths.
// Each probe drives the public translators (shared decoder/pipeline/encoders)
// and pins an acceptance contract. All currently pass; any failure is a finding.
func TestReviewChatStreamNameGateAcrossTargets(t *testing.T) {
	mk := func(name string) string {
		return chatChunk(`{"tool_calls":[{"index":0,"id":"call1","type":"function","function":{"name":"`+name+`","arguments":"{}"}}]}`, "", "") +
			chatChunk(`{}`, "tool_calls", "") +
			chatUsageChunk(`{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}`) +
			"data: [DONE]\n\n"
	}
	// Grammar-invalid name passes Chat->Gemini (proven passthrough).
	if _, rerr := runGeminiStreamForReview(t, mk("has space")); rerr != nil {
		t.Fatalf("Chat->Gemini must pass grammar-invalid name, got %v", rerr)
	}
	// Same bytes refuse Chat->Anthropic and Chat->Responses (target gate).
	if _, rerr := runChatStream(t, mk("has space"), DefaultLimits()); rerr == nil {
		t.Fatal("Chat->Anthropic must refuse grammar-invalid name")
	}
	var out bytes.Buffer
	tr := &ChatToResponsesSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
	if rerr := tr.Translate(strings.NewReader(mk("has space")), &out, nil); rerr == nil {
		t.Fatal("Chat->Responses must refuse grammar-invalid name")
	}
}

func runGeminiStreamForReview(t *testing.T, upstream string) (string, *RouterError) {
	t.Helper()
	var buf bytes.Buffer
	tr := &ChatToGeminiSSETranslator{PublicModel: "p", Limits: DefaultLimits()}
	rerr := tr.Translate(strings.NewReader(upstream), &buf, nil)
	return buf.String(), rerr
}

func TestReviewChatStreamResponsesOpaqueIDsStable(t *testing.T) {
	upstream := chatChunk(`{"tool_calls":[{"index":0,"id":"call:up 1","type":"function","function":{"name":"get_time","arguments":"{}"}}]}`, "", "") +
		chatChunk(`{}`, "tool_calls", "") +
		chatUsageChunk(`{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}`) +
		"data: [DONE]\n\n"
	var out bytes.Buffer
	tr := &ChatToResponsesSSETranslator{PublicModel: "m", Limits: DefaultLimits()}
	if rerr := tr.Translate(strings.NewReader(upstream), &out, nil); rerr != nil {
		t.Fatalf("stream: %v", rerr)
	}
	s := out.String()
	if !strings.Contains(s, `"call_id":"call:up 1"`) {
		t.Fatalf("opaque call_id not preserved verbatim:\n%s", s)
	}
	// Stable across added/done/terminal: at least added + done + terminal carry it.
	if n := strings.Count(s, "call:up 1"); n < 3 {
		t.Fatalf("call id not stable across phases, got %d:\n%s", n, s)
	}
}

func TestReviewChatStreamAbortIdempotent(t *testing.T) {
	// Abort is a failure-path signal only: the pipeline calls it at most once
	// when Encode/decode/framing fails after commitment, never after success.
	// Pin idempotency on a committed failure (malformed args), where the
	// first Abort owns the single error marker and the second is a no-op.
	// Gemini is the exception by design: Abort emits nothing (server owns it).
	mkBad := chatChunk(`{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"a","arguments":"[1]"}}]}`, "", "") +
		chatChunk(`{}`, "tool_calls", "") +
		chatUsageChunk(`{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}`) +
		"data: [DONE]\n\n"
	for _, tc := range []string{"anthropic", "responses"} {
		var buf bytes.Buffer
		var enc EventEncoder
		if tc == "anthropic" {
			enc = NewAnthropicEventEncoder("p", DefaultLimits())
		} else {
			enc = NewResponsesEventEncoder("p", DefaultLimits())
		}
		rerr := TranslateStream(NewSSEFrameReader(strings.NewReader(mkBad), DefaultLimits()), NewChatStreamDecoder(DefaultLimits()), enc, &buf, nil, DefaultLimits(), DialectChat)
		if rerr == nil {
			t.Fatalf("%s malformed setup must fail", tc)
		}
		before := buf.Len()
		enc.Abort(&buf, nil)
		enc.Abort(&buf, nil)
		if buf.Len() != before {
			t.Fatalf("%s abort after pipeline failure must be idempotent (no second write)", tc)
		}
	}
	// Gemini Abort emits nothing even on the failure path.
	{
		var buf bytes.Buffer
		enc := NewGeminiEventEncoder("p", DefaultLimits(), true)
		rerr := TranslateStream(NewSSEFrameReader(strings.NewReader(mkBad), DefaultLimits()), NewChatStreamDecoder(DefaultLimits()), enc, &buf, nil, DefaultLimits(), DialectChat)
		if rerr != nil {
			t.Fatalf("gemini malformed must map, got %v", rerr)
		}
		before := buf.Len()
		enc.Abort(&buf, nil)
		enc.Abort(&buf, nil)
		if buf.Len() != before {
			t.Fatalf("gemini abort must emit nothing")
		}
	}
}
