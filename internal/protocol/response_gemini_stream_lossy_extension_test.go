// Gemini stream row lossy-extension policy tests.
//
// Observed opencode.ai gateway SSE frames carry TEXT-modality usage
// details on every intermediate chunk, a thoughtSignature on the
// terminal part, and cost/opencode gateway billing on the terminal
// usage. Strict keeps the 502 failures; lossy (the provider
// compatibility flag) validates and drops them. Everything else keeps
// failing closed in both modes.
package protocol

import (
	"strings"
	"testing"
)

// geminiStreamLossyChunk1 mirrors the observed first SSE element:
// immediate text plus intermediate usage with modality details.
func geminiStreamLossyChunk1() string {
	return `{"responseId":"bPL9avGyMN6FytMPjsPAwA4","modelVersion":"gemini-4-argon","candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"Hi!"}]}}],` +
		`"usageMetadata":{"candidatesTokensDetails":[{"modality":"TEXT","tokenCount":2}],"promptTokensDetails":[{"modality":"TEXT","tokenCount":5}],"candidatesTokenCount":2,"promptTokenCount":5,"totalTokenCount":7}}`
}

// geminiStreamLossyChunk2 mirrors the observed terminal element: an
// empty text part carrying the opaque signature, finish, and terminal
// usage with gateway billing.
func geminiStreamLossyChunk2() string {
	return `{"responseId":"bPL9avGyMN6FytMPjsPAwA4","modelVersion":"gemini-4-argon","candidates":[{"finishReason":"STOP","index":0,"content":{"role":"model","parts":[{"text":"","thoughtSignature":"EowBCokBAWkUfRPjxuSuo5LTGZudIKxa"}]}}],` +
		`"usageMetadata":{"candidatesTokensDetails":[{"modality":"TEXT","tokenCount":6}],"promptTokensDetails":[{"modality":"TEXT","tokenCount":36}],"candidatesTokenCount":6,"promptTokenCount":36,"totalTokenCount":42,` +
		`"cost":0,"opencode":{"billing_source":"go","list_cost":0.000132,"allowance":0.000264,"tokens":{"input":36,"output":6,"reasoning":0,"cache_read":0,"cache_write_5m":0,"cache_write_1h":0}}}}`
}

func geminiStreamLossyText(t *testing.T, evs []ResponseEvent) string {
	t.Helper()
	var b strings.Builder
	for _, ev := range evs {
		if tf, ok := ev.(TextFragment); ok {
			b.WriteString(tf.Text)
		}
	}
	return b.String()
}

func TestGeminiStreamLossyObservedSSE(t *testing.T) {
	// Strict: the first chunk already fails on the usage details.
	dec := NewGeminiStreamDecoder(DefaultLimits(), true)
	_, rerr := dec.DecodeFrame(Frame{Data: geminiStreamLossyChunk1()})
	if rerr == nil || rerr.Status != 502 || !strings.Contains(rerr.Msg, "response.usageMetadata.promptTokensDetails") {
		t.Fatalf("strict must fail on usage details, got %v", rerr)
	}
	// Lossy: both observed chunks decode; text, finish and terminal
	// accounting survive.
	dec = NewGeminiStreamDecoderWithPolicy(DefaultLimits(), true, true)
	var all []ResponseEvent
	for i, d := range []string{geminiStreamLossyChunk1(), geminiStreamLossyChunk2()} {
		evs, rerr := dec.DecodeFrame(Frame{Data: d})
		if rerr != nil {
			t.Fatalf("lossy frame[%d]: %v", i, rerr)
		}
		all = append(all, evs...)
	}
	if _, rerr := dec.Close(); rerr != nil {
		t.Fatalf("lossy close: %v", rerr)
	}
	if got := geminiStreamLossyText(t, all); got != "Hi!" {
		t.Fatalf("lossy must deliver the text, got %q", got)
	}
	complete := false
	usage := ResponseUsage{}
	for _, ev := range all {
		if _, ok := ev.(CompletionEvent); ok {
			complete = true
		}
		if u, ok := ev.(UsageEvent); ok {
			usage = u.Usage
		}
	}
	if !complete {
		t.Fatalf("lossy must deliver the completion event")
	}
	if usage.Prompt == nil || *usage.Prompt != 36 || usage.Completion == nil || *usage.Completion != 6 ||
		usage.Total == nil || *usage.Total != 42 {
		t.Fatalf("lossy must deliver terminal usage, got %#v", usage)
	}
}

func TestGeminiStreamLossyObservedJSONArray(t *testing.T) {
	dec := NewGeminiStreamDecoderWithPolicy(DefaultLimits(), false, true)
	var all []ResponseEvent
	for i, d := range []string{geminiStreamLossyChunk1(), geminiStreamLossyChunk2()} {
		evs, rerr := dec.DecodeFrame(Frame{Data: d})
		if rerr != nil {
			t.Fatalf("lossy element[%d]: %v", i, rerr)
		}
		all = append(all, evs...)
	}
	evs, rerr := dec.DecodeFrame(Frame{Closed: true})
	if rerr != nil {
		t.Fatalf("lossy closing bracket: %v", rerr)
	}
	all = append(all, evs...)
	if _, rerr := dec.Close(); rerr != nil {
		t.Fatalf("lossy close: %v", rerr)
	}
	if got := geminiStreamLossyText(t, all); got != "Hi!" {
		t.Fatalf("lossy array must deliver the text, got %q", got)
	}
	complete := false
	for _, ev := range all {
		if _, ok := ev.(CompletionEvent); ok {
			complete = true
		}
	}
	if !complete {
		t.Fatalf("lossy array must deliver the completion event")
	}
}

func TestGeminiStreamLossySignatureOnToolCall(t *testing.T) {
	frame := `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"f","args":{"a":1},"thoughtSignature":"sig"}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1,"totalTokenCount":6}}`
	dec := NewGeminiStreamDecoder(DefaultLimits(), true)
	if _, rerr := dec.DecodeFrame(Frame{Data: frame}); rerr == nil ||
		!strings.Contains(rerr.Msg, "functionCall.thoughtSignature") {
		t.Fatalf("strict must fail on the call signature, got %v", rerr)
	}
	dec = NewGeminiStreamDecoderWithPolicy(DefaultLimits(), true, true)
	evs, rerr := dec.DecodeFrame(Frame{Data: frame})
	if rerr != nil {
		t.Fatalf("lossy call signature: %v", rerr)
	}
	sawTool := false
	for _, ev := range evs {
		if bo, ok := ev.(BlockOpen); ok && bo.Kind == BlockTool {
			sawTool = true
		}
	}
	if !sawTool {
		t.Fatalf("lossy must emit the tool block, got %#v", evs)
	}
}

func TestGeminiStreamLossyThoughtsAndFinishMessage(t *testing.T) {
	// Observed tool-call terminal: finishMessage plus a part-level
	// signature on the functionCall part, and terminal usage carrying
	// default-thinking thoughts (46+7+37=90).
	frame := `{"candidates":[{"finishReason":"STOP","finishMessage":"Model generated function call(s).","index":0,"content":{"role":"model","parts":[{"thoughtSignature":"sig","functionCall":{"name":"get_weather","args":{"city":"Tokyo"}}}]}}],"usageMetadata":{"candidatesTokenCount":7,"promptTokenCount":46,"thoughtsTokenCount":37,"totalTokenCount":90}}`
	dec := NewGeminiStreamDecoder(DefaultLimits(), true)
	if _, rerr := dec.DecodeFrame(Frame{Data: frame}); rerr == nil ||
		!strings.Contains(rerr.Msg, "finishMessage") {
		t.Fatalf("strict must fail on the finish message, got %v", rerr)
	}
	dec = NewGeminiStreamDecoderWithPolicy(DefaultLimits(), true, true)
	evs, rerr := dec.DecodeFrame(Frame{Data: frame})
	if rerr != nil {
		t.Fatalf("lossy tool terminal: %v", rerr)
	}
	sawTool, sawUsage := false, false
	for _, ev := range evs {
		if bo, ok := ev.(BlockOpen); ok && bo.Kind == BlockTool {
			sawTool = true
		}
		if u, ok := ev.(UsageEvent); ok {
			sawUsage = true
			if u.Usage.Completion == nil || *u.Usage.Completion != 44 || u.Usage.Reasoning == nil || *u.Usage.Reasoning != 37 {
				t.Fatalf("lossy must fold thoughts into terminal usage, got %#v", u.Usage)
			}
		}
	}
	if !sawTool || !sawUsage {
		t.Fatalf("lossy must deliver the tool block and usage, saw tool=%v usage=%v", sawTool, sawUsage)
	}
	// Nonzero thoughts without the fold equation fails in lossy.
	badTotal := strings.Replace(frame, `"totalTokenCount":90`, `"totalTokenCount":53`, 1)
	dec = NewGeminiStreamDecoderWithPolicy(DefaultLimits(), true, true)
	if _, rerr := dec.DecodeFrame(Frame{Data: badTotal}); rerr == nil || rerr.Status != 502 {
		t.Fatalf("fold equation must hold in lossy streams, got %v", rerr)
	}
}

func TestGeminiStreamLossyStillFailClosed(t *testing.T) {
	cases := []struct {
		name  string
		frame string
		want  string
	}{
		{
			name:  "image modality share",
			frame: `{"candidates":[{"content":{"role":"model","parts":[{"text":"t"}]}}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1,"totalTokenCount":6,"promptTokensDetails":[{"modality":"IMAGE","tokenCount":5}]}}`,
			want:  "promptTokensDetails[0].modality",
		},
		{
			name:  "malformed signature type",
			frame: `{"candidates":[{"content":{"role":"model","parts":[{"text":"t","thoughtSignature":5}]}}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1,"totalTokenCount":6}}`,
			want:  "",
		},
		{
			name:  "thought part",
			frame: `{"candidates":[{"content":{"role":"model","parts":[{"text":"t","thought":true,"thoughtSignature":"s"}]}}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1,"totalTokenCount":6}}`,
			want:  "parts[0].thought",
		},
		{
			name:  "media part",
			frame: `{"candidates":[{"content":{"role":"model","parts":[{"inlineData":{"mimeType":"image/png","data":"eA=="}}]}}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1,"totalTokenCount":6}}`,
			want:  "parts[0].inlineData",
		},
		{
			name:  "invalid gateway cost",
			frame: `{"candidates":[{"content":{"role":"model","parts":[{"text":"t"}]}}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1,"totalTokenCount":6,"cost":"free"}}`,
			want:  "",
		},
	}
	for _, tc := range cases {
		for _, allowDrop := range []bool{false, true} {
			dec := NewGeminiStreamDecoderWithPolicy(DefaultLimits(), true, allowDrop)
			_, rerr := dec.DecodeFrame(Frame{Data: tc.frame})
			if rerr == nil || rerr.Status != 502 {
				t.Fatalf("%s (allowDrop=%v): expected 502, got %v", tc.name, allowDrop, rerr)
			}
			if tc.want != "" && !strings.Contains(rerr.Msg, tc.want) {
				t.Fatalf("%s (allowDrop=%v): expected path %q, got %q", tc.name, allowDrop, tc.want, rerr.Msg)
			}
		}
	}
}
