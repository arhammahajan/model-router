package protocol

import (
	"strings"
	"testing"
)

// Regression: sequential blocks reusing one tool ID must fail. Block 0
// closes before block 1 opens, so an open-block-only scan misses it.
func TestA2RDuplicateClosedToolID(t *testing.T) {
	upstream := a2rStart(`{"input_tokens":8,"output_tokens":1}`) +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_1\",\"name\":\"a\",\"input\":{}}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_1\",\"name\":\"a\",\"input\":{}}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n" +
		a2rDelta("tool_use", `{"output_tokens":2}`) + a2rStop()
	out, rerr := a2rRunStream(t, upstream, DefaultLimits())
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("duplicate closed tool ID must fail with 502, got %v", rerr)
	}
	if strings.Contains(out, "response.completed") || strings.Contains(out, "response.incomplete") {
		t.Fatalf("duplicate ID must not emit success:\n%s", out)
	}
}

// Regression: terminal payload must be validated. Non-JSON, mismatched
// type, and unknown fields must all fail with no success marker.
func TestA2RTerminalPayloadValidated(t *testing.T) {
	goodStart := a2rStart(`{"input_tokens":8,"output_tokens":1}`)
	goodText := a2rTextBlock(0, "") +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"
	goodDelta := a2rDelta("end_turn", `{"output_tokens":2}`)
	cases := map[string]string{
		"non-json":      goodStart + goodText + goodDelta + "event: message_stop\ndata: not-json\n\n",
		"mismatched":    goodStart + goodText + goodDelta + "event: message_stop\ndata: {\"type\":\"ping\"}\n\n",
		"unknown-field": goodStart + goodText + goodDelta + "event: message_stop\ndata: {\"type\":\"message_stop\",\"unmapped\":\"secret\"}\n\n",
		"error-type":    goodStart + goodText + goodDelta + "event: message_stop\ndata: {\"type\":\"error\"}\n\n",
	}
	for name, upstream := range cases {
		out, rerr := a2rRunStream(t, upstream, DefaultLimits())
		if rerr == nil || rerr.Status != 502 {
			t.Fatalf("%s terminal must fail with 502, got %v", name, rerr)
		}
		if strings.Contains(out, "response.completed") || strings.Contains(out, "response.incomplete") {
			t.Fatalf("%s terminal must not emit success:\n%s", name, out)
		}
		if strings.Contains(out, "secret") || strings.Contains(out, "not-json") {
			t.Fatalf("%s terminal must not echo payload bytes:\n%s", name, out)
		}
	}
}

// Valid ping and a real standard stream must stay green.
func TestA2RValidPingStaysGreen(t *testing.T) {
	upstream := a2rStart(`{"input_tokens":8,"output_tokens":1}`) +
		"event: ping\ndata: {\"type\":\"ping\"}\n\n" +
		a2rTextBlock(0, "") +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		a2rDelta("end_turn", `{"output_tokens":2}`) + a2rStop()
	out, rerr := a2rRunStream(t, upstream, DefaultLimits())
	if rerr != nil {
		t.Fatalf("valid ping must stay green: %v", rerr)
	}
	if !strings.Contains(out, "response.completed") {
		t.Fatalf("valid ping stream must complete:\n%s", out)
	}
}
