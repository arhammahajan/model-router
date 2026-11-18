package protocol

import (
	"encoding/json"
	"testing"
)

func TestChatToolResultContentPreservedExactly(t *testing.T) {
	for _, content := range []string{"noon", "", "α\n🙂"} {
		t.Run(content, func(t *testing.T) {
			quoted, _ := json.Marshal(content)
			raw := []byte(`{"model":"m","messages":[{"role":"user","content":"Hi"},{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":` + string(quoted) + `}]}`)
			for _, destination := range []string{"anthropic", "responses"} {
				var gen *Generation
				var err *RouterError
				if destination == "anthropic" {
					gen, _, err = DecodeChatRequest(raw, DefaultLimits())
				} else {
					gen, _, err = DecodeChatForResponses(raw, DefaultLimits())
				}
				if err != nil {
					t.Fatal(err)
				}
				var wire []byte
				if destination == "anthropic" {
					wire, err = EncodeAnthropicRequest(gen, "upstream")
				} else {
					wire, err = EncodeResponsesRequest(gen, AbsentEffort(), "upstream")
				}
				if err != nil {
					t.Fatal(err)
				}
				var body struct {
					Messages []struct {
						Content []struct {
							Content string `json:"content"`
						} `json:"content"`
					} `json:"messages"`
					Input []struct {
						Output string `json:"output"`
					} `json:"input"`
				}
				if decodeErr := json.Unmarshal(wire, &body); decodeErr != nil {
					t.Fatal(decodeErr)
				}
				var got string
				if destination == "anthropic" {
					got = body.Messages[2].Content[0].Content
				} else {
					got = body.Input[2].Output
				}
				if got != content {
					t.Errorf("%s tool result = %q, want %q", destination, got, content)
				}
			}
		})
	}
}
