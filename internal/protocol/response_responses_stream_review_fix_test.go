package protocol

import (
	"bytes"
	"strings"
	"testing"
)

// Focused review proof for the Responses stream cutover correction:
// valid tool arguments compare canonically (whitespace-insensitive,
// numeric-token-preserving via CompactArgs/json.Compact), matching the
// legacy Responses→Anthropic canonical compare (compactChatArgs on both
// sides at arguments.done/item.done/terminal). Malformed payloads fall
// back to raw exactness and classify later; contradictory values still
// fail closed with no success marker.
func TestResponsesStreamReviewCanonicalArgs(t *testing.T) {
	bigCompact := `{"n":9007199254740993}`
	bigSpaced := `{"n": 9007199254740993}`
	// Text-first order (text index 0, tool index 1) is accepted by all
	// targets; Chat refuses text-after-tool, so the whitespace proof
	// uses the text-first shape.
	in := r2aCreated(0) + r2aTextItemAdded(1, 0) + r2aPartAdded(2, 0) +
		r2aTextDelta(3, 0, "done") + r2aTextDone(4, 0, "done") + r2aPartDone(5, 0, "done") +
		r2aTextItemDone(6, 0, "done") +
		r2aToolItemAdded(7, 1, "fc_1", "call_1", "get_time") +
		r2aArgsDelta(8, 1, "fc_1", `{"n":`) + r2aArgsDelta(9, 1, "fc_1", `9007199254740993}`) +
		r2aArgsDone(10, 1, "fc_1", bigCompact) +
		r2aToolItemDone(11, 1, "fc_1", "call_1", "get_time", bigSpaced) +
		r2aTerminal(12, false, append(r2aTextOutput("done"), map[string]any{
			"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "get_time",
			"arguments": bigSpaced, "status": "completed"}), r2aUsage())
	// Chat target preserves the exact bigint token.
	var cb bytes.Buffer
	ctr := &ResponsesToChatSSETranslator{PublicModel: "public", IncludeUsage: true, Limits: DefaultLimits()}
	if rerr := ctr.Translate(strings.NewReader(in), &cb, nil); rerr != nil {
		t.Fatalf("chat whitespace-canonical args must succeed, got %v", rerr)
	}
	if !strings.Contains(cb.String(), "9007199254740993") {
		t.Fatalf("chat must preserve bigint token: %s", cb.String())
	}
	// Anthropic target accepts the same whitespace variant (legacy
	// tool-first positive shape).
	var ab bytes.Buffer
	atr := &ResponsesToAnthropicSSETranslator{PublicModel: "public", Limits: DefaultLimits()}
	if rerr := atr.Translate(strings.NewReader(in), &ab, nil); rerr != nil {
		t.Fatalf("anthropic whitespace-canonical args must succeed, got %v", rerr)
	}
	if !strings.Contains(ab.String(), "9007199254740993") {
		t.Fatalf("anthropic must preserve bigint token: %s", ab.String())
	}
	// Contradictory values (different numbers) still fail closed.
	badCompact := `{"n":7}`
	badSpaced := `{"n": 7}`
	bad := r2aCreated(0) + r2aTextItemAdded(1, 0) + r2aPartAdded(2, 0) +
		r2aTextDelta(3, 0, "done") + r2aTextDone(4, 0, "done") + r2aPartDone(5, 0, "done") +
		r2aTextItemDone(6, 0, "done") +
		r2aToolItemAdded(7, 1, "fc_1", "call_1", "get_time") +
		r2aArgsDelta(8, 1, "fc_1", `{"n":`) + r2aArgsDelta(9, 1, "fc_1", `9007199254740993}`) +
		r2aArgsDone(10, 1, "fc_1", bigCompact) +
		r2aToolItemDone(11, 1, "fc_1", "call_1", "get_time", badSpaced) +
		r2aTerminal(12, false, append(r2aTextOutput("done"), map[string]any{
			"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "get_time",
			"arguments": badCompact, "status": "completed"}), r2aUsage())
	for name, run := range map[string]func(string) (string, *RouterError){
		"chat": func(s string) (string, *RouterError) {
			var b bytes.Buffer
			rerr := (&ResponsesToChatSSETranslator{PublicModel: "p", Limits: DefaultLimits()}).Translate(strings.NewReader(s), &b, nil)
			return b.String(), rerr
		},
		"anthropic": func(s string) (string, *RouterError) {
			var b bytes.Buffer
			rerr := (&ResponsesToAnthropicSSETranslator{PublicModel: "p", Limits: DefaultLimits()}).Translate(strings.NewReader(s), &b, nil)
			return b.String(), rerr
		},
	} {
		out, rerr := run(bad)
		if rerr == nil || rerr.Status != 502 {
			t.Fatalf("%s contradictory args must fail 502, got %v (%s)", name, rerr, out)
		}
		if strings.Contains(out, "[DONE]") || strings.Contains(out, "message_stop") {
			t.Fatalf("%s contradictory args must not succeed", name)
		}
	}
}

// Source-absent "" canonicalizes to "{}" on either side of the done
// markers; malformed non-object payloads still classify (not silently
// repaired) per the complete-row policy.
func TestResponsesStreamReviewEmptyCanonical(t *testing.T) {
	in := r2aCreated(0) + r2aToolItemAdded(1, 0, "fc_1", "call_9", "noop") +
		r2aArgsDone(2, 0, "fc_1", "") +
		r2aToolItemDone(3, 0, "fc_1", "call_9", "noop", "{}") +
		r2aTerminal(4, false, []any{map[string]any{
			"type": "function_call", "id": "fc_1", "call_id": "call_9", "name": "noop",
			"arguments": "{}", "status": "completed"}}, r2aUsage())
	out, rerr := runR2AStream(t, in, DefaultLimits())
	if rerr != nil {
		t.Fatalf("empty-vs-{} canonical args must succeed, got %v", rerr)
	}
	if !strings.Contains(out, "message_stop") {
		t.Fatalf("empty canonical tool must complete: %s", out)
	}
}

// Sparse text without content_part.added contradicts the stream: the
// semantic block opens at part.added (BlockOpen), so deltas beforehand
// have no open block. Official servers always emit part.added; the
// corrected fixtures carry the full lifecycle.
func TestResponsesStreamReviewMissingPartAddedFails(t *testing.T) {
	in := r2aCreated(0) + r2aTextItemAdded(1, 0) + r2aTextDelta(2, 0, "Hi") +
		r2aTerminal(3, false, r2aTextOutput("Hi"), r2aUsage())
	if out, rerr := runR2AStream(t, in, DefaultLimits()); rerr == nil {
		t.Fatalf("delta without part.added must fail, got %s", out)
	}
}
