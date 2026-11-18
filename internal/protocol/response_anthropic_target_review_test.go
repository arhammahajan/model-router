package protocol

import "testing"

func TestAnthropicIRStopSequenceDestinationPolicy(t *testing.T) {
	resp := anthIRDecode(t, anthIRRaw("stop_sequence", `[{"type":"text","text":"hello"}]`, anthIRUsage(1, 1)))
	if _, err := EncodeResponsesResponse(resp, "public"); err == nil || err.Status != 502 {
		t.Fatalf("existing Responses refusal must be preserved: %v", err)
	}
	for _, encode := range []func(Response, string) ([]byte, *RouterError){EncodeChatResponse, EncodeGeminiResponse} {
		if _, err := encode(resp, "public"); err != nil {
			t.Fatalf("reviewed stop mapping must remain available: %v", err)
		}
	}
}
