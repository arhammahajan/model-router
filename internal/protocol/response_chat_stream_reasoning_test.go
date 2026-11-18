// Kimi-class native Chat SSE delta.reasoning variant: live instances vary
// between delta.reasoning (string) and delta.reasoning_content. Both are
// provider-private chain-of-thought with identical policy: absent/null/""
// is exact, malformed is 502 in both modes, substantive is strict-502 and
// lossy-dropped. Dropped bytes never become text and never affect finish;
// finish plus authoritative usage plus [DONE] remain required.
package protocol

import (
	"strings"
	"testing"
)

func TestChatStreamReasoningVariantSubstantiveBeforeVisible(t *testing.T) {
	secret := "kimi-stream-private-abc"
	usage := csUsage(9, 4)
	datas := []string{
		csReasonChunk(`{"reasoning":"`+secret+`"}`, ""),
		csReasonChunk(`{"reasoning":" more"}`, ""),
		csReasonChunk(`{"content":"hi"}`, ""),
		csReasonChunk(`{}`, "stop"),
		csUsageChunk(usage),
		"[DONE]",
	}
	// Strict: first reasoning delta is 502 with the value-free path.
	dec := NewChatStreamDecoderWithPolicy(DefaultLimits(), false)
	if _, rerr := dec.DecodeFrame(Frame{Data: datas[0]}); rerr == nil || rerr.Status != 502 {
		t.Fatalf("strict reasoning delta must 502, got %v", rerr)
	} else {
		if !strings.Contains(rerr.Msg, "chunk.choices[0].delta.reasoning") {
			t.Fatalf("strict stream error must name path: %q", rerr.Msg)
		}
		if strings.Contains(rerr.Msg, secret) {
			t.Fatalf("strict stream error echoes private content: %q", rerr.Msg)
		}
	}
	// Lossy: drops reasoning, emits only visible text, terminates.
	dec = NewChatStreamDecoderWithPolicy(DefaultLimits(), true)
	var all []ResponseEvent
	for _, d := range datas {
		evs, rerr := dec.DecodeFrame(Frame{Data: d})
		if rerr != nil {
			t.Fatalf("lossy DecodeFrame(%q): %v", d, rerr)
		}
		all = append(all, evs...)
	}
	if tail, rerr := dec.Close(); rerr != nil || len(tail) != 0 {
		t.Fatalf("Close = %v %v, want no events", tail, rerr)
	}
	var texts []string
	for _, ev := range all {
		if f, ok := ev.(TextFragment); ok {
			texts = append(texts, f.Text)
		}
	}
	if len(texts) != 1 || texts[0] != "hi" {
		t.Fatalf("lossy text fragments = %q, want [hi]", texts)
	}
	var sawUsage, sawComplete bool
	for _, ev := range all {
		if u, ok := ev.(UsageEvent); ok && u.Terminal() {
			sawUsage = true
		}
		if c, ok := ev.(CompletionEvent); ok && c.Finish.Kind == FinishStop {
			sawComplete = true
		}
	}
	if !sawUsage || !sawComplete {
		t.Fatalf("lossy terminal missing usage=%v complete=%v: %#v", sawUsage, sawComplete, all)
	}
	var joined string
	for _, s := range texts {
		joined += s
	}
	if strings.Contains(joined, secret) || strings.Contains(joined, "more") {
		t.Fatalf("stream projected private reasoning to text: %q", joined)
	}
}

func TestChatStreamReasoningVariantExact(t *testing.T) {
	usage := csUsage(9, 4)
	// Null and empty-string reasoning alongside content/finish is exact in
	// both modes; omitted entirely is identical.
	for _, policy := range []bool{false, true} {
		for _, frag := range []string{`{"reasoning":null}`, `{"reasoning":""}`} {
			dec := NewChatStreamDecoderWithPolicy(DefaultLimits(), policy)
			datas := []string{
				csReasonChunk(`{"content":"hi"}`, ""),
				csReasonChunk(frag, "stop"),
				csUsageChunk(usage),
				"[DONE]",
			}
			var all []ResponseEvent
			for _, d := range datas {
				evs, rerr := dec.DecodeFrame(Frame{Data: d})
				if rerr != nil {
					t.Fatalf("policy=%v frag=%s DecodeFrame(%q): %v", policy, frag, d, rerr)
				}
				all = append(all, evs...)
			}
			if _, rerr := dec.Close(); rerr != nil {
				t.Fatalf("policy=%v frag=%s Close: %v", policy, frag, rerr)
			}
			if _, ok := all[len(all)-1].(CompletionEvent); !ok {
				t.Fatalf("policy=%v frag=%s terminal = %T, want CompletionEvent", policy, frag, all[len(all)-1])
			}
		}
	}
}

func TestChatStreamReasoningVariantMalformed(t *testing.T) {
	// Malformed reasoning types fail in both modes at the first frame.
	for _, frag := range []string{`{"reasoning":5}`, `{"reasoning":true}`, `{"reasoning":[]}`, `{"reasoning":{}}`} {
		for _, policy := range []bool{false, true} {
			dec := NewChatStreamDecoderWithPolicy(DefaultLimits(), policy)
			if _, rerr := dec.DecodeFrame(Frame{Data: csReasonChunk(frag, "")}); rerr == nil || rerr.Status != 502 {
				t.Fatalf("policy=%v malformed %s must 502, got %v", policy, frag, rerr)
			}
		}
	}
}

func TestChatStreamReasoningVariantPostFinish(t *testing.T) {
	usage := csUsage(5, 2)
	// Empty reasoning after the finish marker stays exact in both modes.
	for _, policy := range []bool{false, true} {
		dec := NewChatStreamDecoderWithPolicy(DefaultLimits(), policy)
		datas := []string{
			csReasonChunk(`{"content":"hi"}`, ""),
			csReasonChunk(`{}`, "stop"),
			csReasonChunk(`{"reasoning":""}`, ""),
			csReasonChunk(`{"reasoning":null}`, ""),
			csUsageChunk(usage),
			"[DONE]",
		}
		for _, d := range datas {
			if _, rerr := dec.DecodeFrame(Frame{Data: d}); rerr != nil {
				t.Fatalf("policy=%v post-finish empty DecodeFrame(%q): %v", policy, d, rerr)
			}
		}
		if _, rerr := dec.Close(); rerr != nil {
			t.Fatalf("policy=%v post-finish empty Close: %v", policy, rerr)
		}
	}
	// Substantive reasoning after finish is rejected even with the lossy
	// drop: terminal content is closed and only empty deltas may follow.
	for _, policy := range []bool{false, true} {
		dec := NewChatStreamDecoderWithPolicy(DefaultLimits(), policy)
		prefix := []string{
			csReasonChunk(`{"content":"hi"}`, ""),
			csReasonChunk(`{}`, "stop"),
		}
		for _, d := range prefix {
			if _, rerr := dec.DecodeFrame(Frame{Data: d}); rerr != nil {
				t.Fatalf("policy=%v prefix DecodeFrame(%q): %v", policy, d, rerr)
			}
		}
		if _, rerr := dec.DecodeFrame(Frame{Data: csReasonChunk(`{"reasoning":"late"}`, "")}); rerr == nil || rerr.Status != 502 {
			t.Fatalf("policy=%v post-finish substantive must 502, got %v", policy, rerr)
		}
	}
}

func TestChatStreamReasoningVariantNoTruncationExcuse(t *testing.T) {
	// Reasoning drop does not excuse truncation: usage plus [DONE] remain
	// required.
	dec := NewChatStreamDecoderWithPolicy(DefaultLimits(), true)
	truncated := []string{
		csReasonChunk(`{"reasoning":"think"}`, ""),
		csReasonChunk(`{"content":"hi"}`, ""),
		csReasonChunk(`{}`, "stop"),
		"[DONE]",
	}
	failed := false
	for _, d := range truncated {
		if _, rerr := dec.DecodeFrame(Frame{Data: d}); rerr != nil {
			failed = true
			break
		}
	}
	if !failed {
		if _, rerr := dec.Close(); rerr == nil {
			t.Fatal("truncated stream (missing usage) must fail even with reasoning drop")
		}
	}
}
