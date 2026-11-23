package protocol

// Bearer-compat effort policy: closed constructor origin and the explicit
// Anthropic terminal dropped-loss. Hermetic, no I/O.

import (
	"testing"

	"github.com/arhammahajan/router/internal/capability"
)

func TestCompatEffortOrigin(t *testing.T) {
	intent, ok := NewCompatEffort("xhigh")
	if !ok || !intent.IsPresent() {
		t.Fatalf("xhigh must be accepted literally: %+v %v", intent, ok)
	}
	if lvl, _ := intent.Level(); lvl != "xhigh" {
		t.Fatalf("literal must be preserved: %q", lvl)
	}
	if intent.Origin() != "x-model-router-reasoning-effort" {
		t.Fatalf("origin must be the exact header name: %q", intent.Origin())
	}
	off, ok := NewCompatEffort("none")
	if !ok || !off.IsNone() {
		t.Fatalf("none must map to Off: %+v %v", off, ok)
	}
	if off.Origin() != "x-model-router-reasoning-effort" {
		t.Fatalf("off origin must be the header name: %q", off.Origin())
	}
	if _, ok := NewCompatEffort(""); ok {
		t.Fatalf("empty must be rejected")
	}
	// The Gemini level constructor must never express published xhigh/max:
	// compat keeps its own constructor for those literals.
	if _, ok := NewGeminiLevel("xhigh"); ok {
		t.Fatalf("GeminiLevel must not accept xhigh")
	}
}

func TestCompatAnthropicDroppedLoss(t *testing.T) {
	intent, _ := NewCompatEffort("medium")
	planned, losses := PlanEffort(intent, capability.ReasoningCapabilities{}, CompatAnthropicRoute())
	if planned.IsPresent() {
		t.Fatalf("Anthropic terminal must emit no control: %+v", planned)
	}
	if len(losses) != 1 || losses[0].Field != "x-model-router-reasoning-effort" || losses[0].Action != "dropped" {
		t.Fatalf("loss must name the static header origin: %+v", losses)
	}
	if _, rerr := EnforcePolicy(losses, false); rerr == nil || rerr.Code != CodeTranslationLoss {
		t.Fatalf("strict must 400 translation_loss: %+v", rerr)
	}
	kept, rerr := EnforcePolicy(losses, true)
	if rerr != nil || len(kept) != 1 {
		t.Fatalf("lossy must continue with the header: %+v %+v", kept, rerr)
	}
	// Absent stays absent through the terminal scope.
	planned, losses = PlanEffort(AbsentReasoning(), capability.ReasoningCapabilities{}, CompatAnthropicRoute())
	if planned.IsPresent() || len(losses) != 0 {
		t.Fatalf("absent must stay absent: %+v %+v", planned, losses)
	}
}
