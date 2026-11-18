package protocol

import (
	"strings"
	"testing"
)

func TestResponsesIRRefusalDestinationPolicy(t *testing.T) {
	raw := respIRRaw("completed", `[{"id":"m1","type":"message","role":"assistant","status":"completed","content":[{"type":"refusal","refusal":"no"}]}]`, respIRUsage(1, 1), "")
	resp := respIRDecode(t, raw)
	body, err := EncodeChatResponse(resp, "public")
	if err != nil || !strings.Contains(string(body), `"finish_reason":"content_filter"`) {
		t.Errorf("completed refusal must preserve Chat filter outcome: %v %s", err, body)
	}
	for _, encode := range []func(Response, string) ([]byte, *RouterError){EncodeAnthropicResponse, EncodeGeminiResponse} {
		if _, err := encode(resp, "public"); err == nil || err.Status != 502 {
			t.Errorf("unreviewed refusal mapping must remain rejected: %v", err)
		}
	}
}
