package protocol

import (
	"fmt"
	"testing"
)

func TestChatZeroUsageDetailsAcrossTargets(t *testing.T) {
	for _, target := range []struct {
		name      string
		translate func([]byte, string, Limits) ([]byte, *RouterError)
	}{
		{"anthropic", TranslateChatJSONToAnthropic},
		{"responses", TranslateChatJSONToResponses},
		{"gemini", TranslateChatJSONToGemini},
	} {
		for _, count := range []int{0, 1} {
			t.Run(fmt.Sprintf("%s/audio_%d", target.name, count), func(t *testing.T) {
				raw := []byte(fmt.Sprintf(`{"id":"c","object":"chat.completion","created":1,"model":"up","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3,"prompt_tokens_details":{"text_tokens":0,"audio_tokens":%d},"completion_tokens_details":{"text_tokens":0}}}`, count))
				_, err := target.translate(raw, "public", DefaultLimits())
				if count == 0 && err != nil {
					t.Fatalf("zero accounting buckets must preserve the text response: %v", err)
				}
				if count != 0 && (err == nil || err.Status != 502) {
					t.Fatalf("unmapped nonzero audio accounting must fail: %v", err)
				}
			})
		}
	}
}
