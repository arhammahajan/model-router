package protocol

import (
	"strings"
	"testing"
)

func TestChatStreamPendingIdentitiesBound(t *testing.T) {
	for _, field := range []string{"id", "name"} {
		t.Run(field, func(t *testing.T) {
			lim := DefaultLimits()
			lim.MaxArgsChars = 128
			lim.MaxStreamArgsChars = 128
			d := NewChatStreamDecoder(lim)
			for i := 0; i < 2; i++ {
				value := strings.Repeat(string(rune('a'+i)), 100)
				var delta string
				if field == "id" {
					delta = `{"tool_calls":[{"index":` + itoa(i) + `,"id":"` + value + `","function":{}}]}`
				} else {
					delta = `{"tool_calls":[{"index":` + itoa(i) + `,"function":{"name":"` + value + `"}}]}`
				}
				_, err := d.DecodeFrame(Frame{Data: csChunk(delta, "")})
				if i == 0 && err != nil {
					t.Fatalf("first identity fits: %v", err)
				}
				if i == 1 && (err == nil || err.Status != 502 || strings.Contains(err.Msg, value)) {
					t.Fatalf("pending identity bound must fail value-free: %v", err)
				}
			}
		})
	}
}

func TestChatStreamPendingIdentityReleasedAtBlockOpen(t *testing.T) {
	lim := DefaultLimits()
	lim.MaxArgsChars = 128
	lim.MaxStreamArgsChars = 128
	d := NewChatStreamDecoder(lim)
	for _, delta := range []string{
		`{"tool_calls":[{"index":0,"id":"` + strings.Repeat("a", 100) + `","function":{}}]}`,
		`{"tool_calls":[{"index":0,"function":{"name":"f"}}]}`,
		`{"tool_calls":[{"index":1,"id":"` + strings.Repeat("b", 100) + `","function":{}}]}`,
	} {
		if _, err := d.DecodeFrame(Frame{Data: csChunk(delta, "")}); err != nil {
			t.Fatal(err)
		}
	}
}
