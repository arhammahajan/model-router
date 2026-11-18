package protocol

import (
	"fmt"
	"strings"
	"testing"
)

func TestChatResponsesOpaqueCallID(t *testing.T) {
	for _, id := range []string{strings.Repeat("i", 75), "call:opaque/id"} {
		raw := []byte(fmt.Sprintf(`{"id":"c","object":"chat.completion","created":1,"model":"up","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":%q,"type":"function","function":{"name":"get_time","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`, id))
		body, err := TranslateChatJSONToResponses(raw, "public", DefaultLimits())
		if err != nil || !strings.Contains(string(body), id) {
			t.Fatalf("opaque call identity must survive: %v", err)
		}
		lim := DefaultLimits()
		lim.MaxArgsChars = len(id) - 1
		if _, err := TranslateChatJSONToResponses(raw, "public", lim); err == nil || err.Status != 502 {
			t.Fatalf("oversized identity must fail: %v", err)
		}
	}
}
