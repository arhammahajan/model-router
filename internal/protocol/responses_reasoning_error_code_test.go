package protocol

import "testing"

func TestResponsesReasoningMalformedMemberCode(t *testing.T) {
	for _, member := range []string{
		`"effort":123`, `"effort":true`, `"effort":null`,
		`"summary":null`, `"generate_summary":123`,
	} {
		raw := []byte(`{"model":"m","input":"hi","reasoning":{` + member + `}}`)
		_, losses, rerr := DecodeResponsesRequest(raw, DefaultLimits())
		if rerr == nil || rerr.Status != 400 || rerr.Code != CodeInvalidGeneration {
			t.Errorf("malformed reasoning member: got %v, want 400 %s", rerr, CodeInvalidGeneration)
		}
		if len(losses) != 0 {
			t.Errorf("malformed reasoning member produced losses: %v", losses)
		}
	}
}
