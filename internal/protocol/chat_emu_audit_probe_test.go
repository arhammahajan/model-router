package protocol

import "testing"

// Orphaned calls of different families must never share one upstream name,
// regardless of call or message order.
func TestProbeAuditOrphanFamilyOrderAnthropic(t *testing.T) {
	// Same assistant message: function orphan first, custom orphan second.
	raw := `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"ghost","arguments":"{}"}},{"id":"c2","type":"custom","custom":{"name":"ghost","input":"x"}}]}]}`
	if _, _, rerr := DecodeChatRequest([]byte(raw), DefaultLimits()); rerr == nil {
		t.Fatalf("anthropic f-then-c orphan collision admitted (want 400 conflicting tool identity)")
	}
	// Control: reverse order must also refuse.
	rev := `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"c2","type":"custom","custom":{"name":"ghost","input":"x"}},{"id":"c1","type":"function","function":{"name":"ghost","arguments":"{}"}}]}]}`
	if _, _, rerr := DecodeChatRequest([]byte(rev), DefaultLimits()); rerr == nil {
		t.Fatalf("anthropic c-then-f orphan collision admitted (want 400)")
	}
}

func TestProbeAuditOrphanFamilyOrderGemini(t *testing.T) {
	raw := `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"ghost","arguments":"{}"}},{"id":"c2","type":"custom","custom":{"name":"ghost","input":"x"}}]}]}`
	if _, _, rerr := DecodeChatForGemini([]byte(raw), DefaultLimits()); rerr == nil {
		t.Fatalf("gemini f-then-c orphan collision admitted (want 400 conflicting tool identity)")
	}
	rev := `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"c2","type":"custom","custom":{"name":"ghost","input":"x"}},{"id":"c1","type":"function","function":{"name":"ghost","arguments":"{}"}}]}]}`
	if _, _, rerr := DecodeChatForGemini([]byte(rev), DefaultLimits()); rerr == nil {
		t.Fatalf("gemini c-then-f orphan collision admitted (want 400)")
	}
}

func TestProbeAuditOrphanFamilyOrderCrossMessage(t *testing.T) {
	// Across messages: function orphan in message 0, custom orphan in message 1.
	raw := `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"ghost","arguments":"{}"}}]},{"role":"assistant","tool_calls":[{"id":"c2","type":"custom","custom":{"name":"ghost","input":"x"}}]}]}`
	if _, _, rerr := DecodeChatRequest([]byte(raw), DefaultLimits()); rerr == nil {
		t.Fatalf("anthropic cross-message f-then-c orphan collision admitted (want 400)")
	}
	if _, _, rerr := DecodeChatForGemini([]byte(raw), DefaultLimits()); rerr == nil {
		t.Fatalf("gemini cross-message f-then-c orphan collision admitted (want 400)")
	}
	// Control: reverse cross-message order must also refuse.
	rev := `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"c2","type":"custom","custom":{"name":"ghost","input":"x"}}]},{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"ghost","arguments":"{}"}}]}]}`
	if _, _, rerr := DecodeChatRequest([]byte(rev), DefaultLimits()); rerr == nil {
		t.Fatalf("anthropic cross-message c-then-f admitted (want 400)")
	}
	if _, _, rerr := DecodeChatForGemini([]byte(rev), DefaultLimits()); rerr == nil {
		t.Fatalf("gemini cross-message c-then-f admitted (want 400)")
	}
}
