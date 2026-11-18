package protocol

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/arhammahajan/model-router/internal/capability"
)

// Independent reverse review: Responses reasoning.effort -> Chat reasoning_effort.
// Stable source: HEAD model_router/{reasoning,protocol}.py (closed reasoning
// object, same-enum exact, malformed before loss, summaries Responses-only).
// Hermetic, no net/env.
func rvrProtoCaps(t *testing.T, fn func(c *capability.Contract)) capability.ReasoningCapabilities {
	t.Helper()
	c := capability.Empty("openai-chat")
	if fn != nil {
		fn(&c)
	}
	c.Normalize()
	return capability.ReasoningFor(c)
}

func rvrProtoPut(t *testing.T, c *capability.Contract, path string, v capability.Value) {
	t.Helper()
	s := "reverse-review"
	if err := c.Put(path, v, &s, capability.KindOverride); err != nil {
		t.Fatalf("put %s: %v", path, err)
	}
}

func TestRvrProtoSharedPlannerOrigins(t *testing.T) {
	chat, ok := NewChatEffort("medium")
	if !ok || chat.Origin() != "reasoning_effort" {
		t.Fatalf("chat origin: %+v", chat)
	}
	resp, ok := NewResponsesEffort("medium")
	if !ok || resp.Origin() != "reasoning.effort" {
		t.Fatalf("responses origin: %+v", resp)
	}
	caps := rvrProtoCaps(t, nil)
	pChat, lChat := PlanEffort(chat, caps, OrdinaryRoute())
	pResp, lResp := PlanEffort(resp, caps, OrdinaryRoute())
	if len(lChat) != 0 || len(lResp) != 0 {
		t.Fatalf("unknown must be exact: %v %v", lChat, lResp)
	}
	eChat, _ := pChat.Effort()
	eResp, _ := pResp.Effort()
	if eChat != "medium" || eResp != "medium" {
		t.Fatalf("single shared planner must preserve literal: %q %q", eChat, eResp)
	}
	// Encoders own wire names: same plan type, different members.
	g := &Generation{Messages: []Message{{Role: RoleUser, Texts: []TextContent{{Text: "hi"}}}}}
	outChat, rerr := EncodeChatRequest(g, pResp, "up")
	if rerr != nil {
		t.Fatal(rerr)
	}
	var bodyChat map[string]any
	if err := json.Unmarshal(outChat, &bodyChat); err != nil {
		t.Fatal(err)
	}
	if bodyChat["reasoning_effort"] != "medium" {
		t.Fatalf("chat wire: %s", outChat)
	}
	if _, has := bodyChat["reasoning"]; has {
		t.Fatalf("chat must not emit Responses field: %s", outChat)
	}
	outResp, rerr := EncodeResponsesRequest(g, pChat, "up")
	if rerr != nil {
		t.Fatal(rerr)
	}
	var bodyResp map[string]any
	if err := json.Unmarshal(outResp, &bodyResp); err != nil {
		t.Fatal(err)
	}
	reasoning, ok := bodyResp["reasoning"].(map[string]any)
	if !ok || reasoning["effort"] != "medium" {
		t.Fatalf("responses wire: %s", outResp)
	}
	if _, has := bodyResp["reasoning_effort"]; has {
		t.Fatalf("responses must not emit Chat field: %s", outResp)
	}
}

func TestRvrProtoNullEmptyAbsent(t *testing.T) {
	for _, raw := range []string{
		`{"model":"m","input":"Hi","reasoning":null}`,
		`{"model":"m","input":"Hi"}`,
		`{"model":"m","input":"Hi","reasoning":{}}`,
	} {
		g, losses, rerr := DecodeResponsesRequest([]byte(raw), DefaultLimits())
		if rerr != nil {
			t.Fatalf("null/empty absent %s: %v", raw, rerr)
		}
		if g.Reasoning.IsPresent() || len(losses) != 0 {
			t.Fatalf("null/empty must be absent no-loss: %s %+v %v", raw, g.Reasoning, losses)
		}
	}
}

func TestRvrProtoMalformedInnerCodeNoLoss(t *testing.T) {
	for _, member := range []string{
		`"effort":123`, `"effort":true`, `"effort":null`, `"effort":""`,
		`"summary":null`, `"summary":123`, `"generate_summary":null`,
		`"effort":"medium","bogus":1`,
	} {
		raw := []byte(`{"model":"m","input":"hi","reasoning":{` + member + `}}`)
		_, losses, rerr := DecodeResponsesRequest(raw, DefaultLimits())
		if rerr == nil || rerr.Status != 400 || rerr.Code != CodeInvalidGeneration {
			t.Fatalf("malformed %s must be 400 %s, got %v", member, CodeInvalidGeneration, rerr)
		}
		if len(losses) != 0 {
			t.Fatalf("malformed must not produce losses: %s %v", member, losses)
		}
	}
	for _, raw := range []string{
		`{"model":"m","input":"hi","reasoning":123}`,
		`{"model":"m","input":"hi","reasoning":"medium"}`,
	} {
		_, losses, rerr := DecodeResponsesRequest([]byte(raw), DefaultLimits())
		if rerr == nil || rerr.Status != 400 || rerr.Code != CodeInvalidRequest {
			t.Fatalf("non-object %s must be 400 %s, got %v", raw, CodeInvalidRequest, rerr)
		}
		if len(losses) != 0 {
			t.Fatalf("malformed must not produce losses: %s", raw)
		}
	}
}

func TestRvrProtoSummaryNeverPartiallyAdmits(t *testing.T) {
	for _, raw := range []string{
		`{"model":"m","input":"Hi","reasoning":{"summary":"detailed"}}`,
		`{"model":"m","input":"Hi","reasoning":{"generate_summary":"auto"}}`,
		`{"model":"m","input":"Hi","reasoning":{"effort":"medium","summary":"detailed"}}`,
		`{"model":"m","input":"Hi","reasoning":{"effort":"medium","generate_summary":"auto"}}`,
	} {
		_, losses, rerr := DecodeResponsesRequest([]byte(raw), DefaultLimits())
		if rerr == nil || rerr.Status != 400 || rerr.Code != CodeInvalidGeneration {
			t.Fatalf("summary must hard-refuse: %s got %v", raw, rerr)
		}
		if len(losses) != 0 {
			t.Fatalf("refusal must not produce losses: %s", raw)
		}
	}
}

func TestRvrProtoGatesLiteralNoClamp(t *testing.T) {
	caps := rvrProtoCaps(t, nil)
	minimal, _ := NewResponsesEffort("minimal")
	planned, losses := PlanEffort(minimal, caps, OrdinaryRoute())
	if len(losses) != 0 {
		t.Fatalf("unknown minimal must be exact: %v", losses)
	}
	if eff, _ := planned.Effort(); eff != "minimal" {
		t.Fatalf("minimal must never become low: %+v", planned)
	}
	c := capability.Empty("openai-chat")
	vals, _ := capability.StringListValue([]string{"low", "medium"})
	rvrProtoPut(t, &c, "reasoning.effort.values", vals)
	c.Normalize()
	known := capability.ReasoningFor(c)
	bad, _ := NewResponsesEffort("high")
	if _, losses := PlanEffort(bad, known, OrdinaryRoute()); len(losses) != 1 {
		t.Fatalf("enum mismatch must drop: %v", losses)
	} else if strings.Contains(losses[0].Reason, "high") {
		t.Fatalf("diagnostic must be value-free: %v", losses[0])
	}
	emptyVals, _ := capability.StringListValue([]string{})
	ce := capability.Empty("openai-chat")
	rvrProtoPut(t, &ce, "reasoning.effort.values", emptyVals)
	ce.Normalize()
	if _, losses := PlanEffort(bad, capability.ReasoningFor(ce), OrdinaryRoute()); len(losses) != 1 {
		t.Fatalf("known-empty must drop: %v", losses)
	}
}

func TestRvrProtoOtherDestRetainsRefusal(t *testing.T) {
	// Anthropic retains the hard refusal for Responses effort; Gemini now
	// decodes effort into the closed Responses-origin intent for the
	// reviewed effort -> thinkingLevel unit (summaries still refuse).
	_, _, rerr := DecodeResponsesForAnthropic([]byte(`{"model":"m","input":"Hi","reasoning":{"effort":"medium"}}`), DefaultLimits())
	if rerr == nil || rerr.Status != 400 || rerr.Code != CodeInvalidGeneration {
		t.Fatalf("anthropic dest must retain refusal: %v", rerr)
	}
	g, losses, rerr := DecodeResponsesForGemini([]byte(`{"model":"m","input":"Hi","reasoning":{"effort":"medium"}}`), DefaultLimits())
	if rerr != nil {
		t.Fatalf("gemini dest must decode effort: %v", rerr)
	}
	if len(losses) != 0 {
		t.Fatalf("gemini effort must emit no decode loss: %v", losses)
	}
	if lvl, ok := g.Reasoning.Level(); !ok || lvl != "medium" || g.Reasoning.Origin() != "reasoning.effort" {
		t.Fatalf("gemini intent: %+v", g.Reasoning)
	}
	for _, raw := range []string{
		`{"model":"m","input":"Hi","reasoning":{"summary":"detailed"}}`,
		`{"model":"m","input":"Hi","reasoning":{"effort":"medium","summary":"detailed"}}`,
	} {
		if _, _, rerr := DecodeResponsesForGemini([]byte(raw), DefaultLimits()); rerr == nil || rerr.Status != 400 {
			t.Fatalf("gemini summary must refuse: %s got %v", raw, rerr)
		}
	}
}
