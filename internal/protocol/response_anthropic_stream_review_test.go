package protocol

import "testing"

func TestAnthropicStreamMessageDeltaEndsContent(t *testing.T) {
	start := asFr("message_start", asStartData(asMinMsg(`"m"`, `{"input_tokens":1}`)))
	block := asFr("content_block_start", asBlockStart(0, `{"type":"text","text":""}`))
	delta := asFr("message_delta", asMsgDelta(`{"stop_reason":"end_turn"}`, `{"output_tokens":1}`))
	for _, tc := range []struct {
		name   string
		frames []Frame
	}{
		{"late block", []Frame{start, delta, block}},
		{"unclosed block", []Frame{start, block, delta}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := NewAnthropicStreamDecoder(DefaultLimits())
			for i, f := range tc.frames {
				_, err := d.DecodeFrame(f)
				if i < 2 && err != nil {
					t.Fatal(err)
				}
				if i == 2 && err == nil {
					t.Fatal("content/message-delta phase violation accepted")
				}
			}
		})
	}
}
