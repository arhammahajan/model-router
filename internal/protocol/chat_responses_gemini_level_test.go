package protocol

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/arhammahajan/model-router/internal/capability"
)

// Chat/Responses effort -> Gemini thinkingLevel: decode admits the intent,
// the shared planner gates the exact wire on the pinned canonical facts,
// the encoder owns the exact official thinkingLevel member.

func gemLevelCaps(t *testing.T, apply func(*capability.Contract)) capability.ReasoningCapabilities {
	t.Helper()
	c := capability.Empty("gemini")
	if apply != nil {
		apply(&c)
	}
	c.Normalize()
	return capability.ReasoningFor(c)
}

func gemLevelPut(t *testing.T, c *capability.Contract, path string, v capability.Value) {
	t.Helper()
	s := "t"
	if err := c.Put(path, v, &s, capability.KindOverride); err != nil {
		t.Fatal(err)
	}
}

func gemLevelExactCaps(t *testing.T) capability.ReasoningCapabilities {
	return gemLevelCaps(t, func(c *capability.Contract) {
		gemLevelPut(t, c, "reasoning.effort.supported", capability.BoolValue(true))
		np, ok := capability.NewNativeParam("gemini", "generationConfig.thinkingConfig.thinkingLevel", nil)
		if !ok {
			t.Fatal("bad native")
		}
		gemLevelPut(t, c, "reasoning.effort.native_parameter", capability.NativeValue(np))
		vals, ok := capability.StringListValue([]string{"minimal", "low", "medium", "high", "xhigh"})
		if !ok {
			t.Fatal("bad vals")
		}
		gemLevelPut(t, c, "reasoning.effort.values", vals)
	})
}

func TestChatGeminiLevelDecode(t *testing.T) {
	g, losses, rerr := DecodeChatForGemini([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"medium"}`), DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	if len(losses) != 0 {
		t.Fatalf("exact effort must emit no decode loss: %v", losses)
	}
	if lvl, ok := g.Reasoning.Level(); !ok || lvl != "medium" || g.Reasoning.Origin() != "reasoning_effort" {
		t.Fatalf("intent: %+v", g.Reasoning)
	}
	// Null absent, empty object absent-equivalent (no reasoning field).
	g, losses, rerr = DecodeChatForGemini([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":null}`), DefaultLimits())
	if rerr != nil || g.Reasoning.IsPresent() || len(losses) != 0 {
		t.Fatalf("null absent: %+v %v %v", g.Reasoning, losses, rerr)
	}
	// Malformed before I/O, both modes share the same boundary.
	for _, raw := range []string{
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":123}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":""}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":true}`,
	} {
		if _, _, rerr := DecodeChatForGemini([]byte(raw), DefaultLimits()); rerr == nil || rerr.Status != 400 {
			t.Fatalf("malformed must 400: %s got %v", raw, rerr)
		}
	}
	// Conflict fails pre-I/O with the effort origin.
	raw := `{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"medium","extra_body":{"thinking":{"type":"enabled","budget_tokens":100}}}`
	if _, _, rerr := DecodeChatForGemini([]byte(raw), DefaultLimits()); rerr == nil || rerr.Status != 400 || !strings.Contains(rerr.Msg, "reasoning_effort") {
		t.Fatalf("conflict must 400 with origin: %v", rerr)
	}
	// Thinking alone stays a diagnosed loss.
	_, losses, rerr = DecodeChatForGemini([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"extra_body":{"thinking":{"type":"enabled","budget_tokens":100}}}`), DefaultLimits())
	if rerr != nil {
		t.Fatalf("thinking alone must not 400: %v", rerr)
	}
	found := false
	for _, l := range losses {
		if l.Field == "extra_body.thinking" {
			found = true
		}
	}
	if !found {
		t.Fatalf("thinking alone must stay loss: %v", losses)
	}
}

func TestResponsesGeminiLevelDecode(t *testing.T) {
	g, losses, rerr := DecodeResponsesForGemini([]byte(`{"model":"m","input":"Hi","reasoning":{"effort":"low"}}`), DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	if len(losses) != 0 {
		t.Fatalf("exact must emit no decode loss: %v", losses)
	}
	if lvl, ok := g.Reasoning.Level(); !ok || lvl != "low" || g.Reasoning.Origin() != "reasoning.effort" {
		t.Fatalf("intent: %+v", g.Reasoning)
	}
	// Null/empty absent.
	g, _, rerr = DecodeResponsesForGemini([]byte(`{"model":"m","input":"Hi","reasoning":null}`), DefaultLimits())
	if rerr != nil || g.Reasoning.IsPresent() {
		t.Fatalf("null absent: %v", rerr)
	}
	g, _, rerr = DecodeResponsesForGemini([]byte(`{"model":"m","input":"Hi","reasoning":{}}`), DefaultLimits())
	if rerr != nil || g.Reasoning.IsPresent() {
		t.Fatalf("empty absent: %v", rerr)
	}
	// Malformed + prohibited summary before I/O.
	for _, raw := range []string{
		`{"model":"m","input":"Hi","reasoning":{"effort":123}}`,
		`{"model":"m","input":"Hi","reasoning":{"effort":""}}`,
		`{"model":"m","input":"Hi","reasoning":{"effort":null}}`,
		`{"model":"m","input":"Hi","reasoning":{"summary":"detailed"}}`,
		`{"model":"m","input":"Hi","reasoning":{"effort":"low","summary":"detailed"}}`,
		`{"model":"m","input":"Hi","reasoning":{"effort":"low","generate_summary":"auto"}}`,
		`{"model":"m","input":"Hi","reasoning":{"bogus":"x"}}`,
	} {
		if _, _, rerr := DecodeResponsesForGemini([]byte(raw), DefaultLimits()); rerr == nil || rerr.Status != 400 {
			t.Fatalf("must 400 pre-I/O: %s got %v", raw, rerr)
		}
	}
}

func TestChatResponsesGeminiLevelPlan(t *testing.T) {
	chat, _ := NewChatEffort("medium")
	// Trusted exact: lossless Named.
	if p, l := PlanEffort(chat, gemLevelExactCaps(t), OrdinaryGeminiRoute()); len(l) != 0 {
		t.Fatalf("trusted must be lossless: %v", l)
	} else if eff, _ := p.Effort(); eff != "medium" {
		t.Fatalf("trusted must carry literal: %+v", p)
	}
	// Responses origin plans identically under the same pin.
	resp, _ := NewResponsesEffort("low")
	if p, l := PlanEffort(resp, gemLevelExactCaps(t), OrdinaryGeminiRoute()); len(l) != 0 {
		t.Fatalf("responses trusted must be lossless: %v", l)
	} else if eff, _ := p.Effort(); eff != "low" {
		t.Fatalf("literal: %+v", p)
	}
	// Official wire covers xhigh (current primary docs) when the reviewed
	// known enum contains it.
	xh, _ := NewChatEffort("xhigh")
	if p, l := PlanEffort(xh, gemLevelExactCaps(t), OrdinaryGeminiRoute()); len(l) != 0 {
		t.Fatalf("xhigh trusted must be lossless: %v", l)
	} else if eff, _ := p.Effort(); eff != "xhigh" {
		t.Fatalf("xhigh literal: %+v", p)
	}
	// Unknown (nil) facts diagnose a drop, never a heuristic.
	if p, l := PlanEffort(chat, gemLevelCaps(t, nil), OrdinaryGeminiRoute()); len(l) != 1 || p.IsPresent() {
		t.Fatalf("unknown must drop: %+v %v", p, l)
	} else if l[0].Field != "reasoning_effort" || l[0].Action != "dropped" {
		t.Fatalf("unknown loss: %+v", l[0])
	}
	// Explicit effort.supported=false drops.
	noEffort := gemLevelCaps(t, func(c *capability.Contract) {
		gemLevelPut(t, c, "reasoning.effort.supported", capability.BoolValue(false))
		np, _ := capability.NewNativeParam("gemini", "generationConfig.thinkingConfig.thinkingLevel", nil)
		gemLevelPut(t, c, "reasoning.effort.native_parameter", capability.NativeValue(np))
		vals, _ := capability.StringListValue([]string{"medium"})
		gemLevelPut(t, c, "reasoning.effort.values", vals)
	})
	if _, l := PlanEffort(chat, noEffort, OrdinaryGeminiRoute()); len(l) != 1 {
		t.Fatalf("supported=false must drop: %v", l)
	}
	// Alternate native descriptors drop: wrong protocol, wrong path, nil.
	for _, alt := range []struct {
		name     string
		protocol string
		path     string
		nilNP    bool
	}{
		{"wrong-protocol", "openai-chat", "generationConfig.thinkingConfig.thinkingLevel", false},
		{"wrong-path", "gemini", "reasoning_effort", false},
		{"nil", "", "", true},
	} {
		caps := gemLevelCaps(t, func(c *capability.Contract) {
			gemLevelPut(t, c, "reasoning.effort.supported", capability.BoolValue(true))
			if !alt.nilNP {
				np, ok := capability.NewNativeParam(alt.protocol, alt.path, nil)
				if !ok {
					t.Fatal("bad native")
				}
				gemLevelPut(t, c, "reasoning.effort.native_parameter", capability.NativeValue(np))
			}
			vals, _ := capability.StringListValue([]string{"medium"})
			gemLevelPut(t, c, "reasoning.effort.values", vals)
		})
		if p, l := PlanEffort(chat, caps, OrdinaryGeminiRoute()); len(l) != 1 || p.IsPresent() {
			t.Fatalf("%s must drop: %+v %v", alt.name, p, l)
		}
	}
	// Known-empty and enum mismatch drop, value-free.
	empty := gemLevelCaps(t, func(c *capability.Contract) {
		gemLevelPut(t, c, "reasoning.effort.supported", capability.BoolValue(true))
		np, _ := capability.NewNativeParam("gemini", "generationConfig.thinkingConfig.thinkingLevel", nil)
		gemLevelPut(t, c, "reasoning.effort.native_parameter", capability.NativeValue(np))
		vals, _ := capability.StringListValue([]string{})
		gemLevelPut(t, c, "reasoning.effort.values", vals)
	})
	if _, l := PlanEffort(chat, empty, OrdinaryGeminiRoute()); len(l) != 1 {
		t.Fatalf("known-empty must drop: %v", l)
	}
	mismatch := gemLevelCaps(t, func(c *capability.Contract) {
		gemLevelPut(t, c, "reasoning.effort.supported", capability.BoolValue(true))
		np, _ := capability.NewNativeParam("gemini", "generationConfig.thinkingConfig.thinkingLevel", nil)
		gemLevelPut(t, c, "reasoning.effort.native_parameter", capability.NativeValue(np))
		vals, _ := capability.StringListValue([]string{"low"})
		gemLevelPut(t, c, "reasoning.effort.values", vals)
	})
	if _, l := PlanEffort(chat, mismatch, OrdinaryGeminiRoute()); len(l) != 1 {
		t.Fatalf("mismatch must drop: %v", l)
	} else if strings.Contains(l[0].Reason, "medium") {
		t.Fatalf("diagnostic must be value-free: %+v", l[0])
	}
	// Unreviewed none and unreviewed literals drop, never clamp.
	none, _ := NewChatEffort("none")
	if p, l := PlanEffort(none, gemLevelExactCaps(t), OrdinaryGeminiRoute()); len(l) != 1 || p.IsPresent() {
		t.Fatalf("none must drop: %+v %v", p, l)
	}
	max, _ := NewChatEffort("max")
	if p, l := PlanEffort(max, gemLevelExactCaps(t), OrdinaryGeminiRoute()); len(l) != 1 || p.IsPresent() {
		t.Fatalf("max must drop, never clamp: %+v %v", p, l)
	}
	// Strict refuses pre-I/O; lossy keeps the diagnosed header.
	if _, l := PlanEffort(chat, gemLevelCaps(t, nil), OrdinaryGeminiRoute()); len(l) == 1 {
		if _, rerr := EnforcePolicy(l, false); rerr == nil || rerr.Code != CodeTranslationLoss {
			t.Fatalf("strict must 400 translation_loss: %v", rerr)
		}
		if kept, rerr := EnforcePolicy(l, true); rerr != nil || len(kept) != 1 {
			t.Fatalf("lossy must keep header: %v %v", kept, rerr)
		}
	}
}

func TestChatResponsesGeminiLevelEncode(t *testing.T) {
	gen := func() *Generation {
		return &Generation{Model: "m", Messages: []Message{{Role: RoleUser, Texts: []TextContent{{Text: "hi"}}}}}
	}
	for _, tc := range []struct{ literal, wire string }{
		{"minimal", "MINIMAL"}, {"low", "LOW"}, {"medium", "MEDIUM"}, {"high", "HIGH"}, {"xhigh", "XHIGH"},
	} {
		out, rerr := EncodeGeminiRequestWithPlan(gen(), NamedEffortTarget(tc.literal))
		if rerr != nil {
			t.Fatalf("%s encode: %v", tc.literal, rerr)
		}
		var body map[string]any
		if err := json.Unmarshal(out, &body); err != nil {
			t.Fatal(err)
		}
		genCfg, ok := body["generationConfig"].(map[string]any)
		if !ok {
			t.Fatalf("%s generationConfig missing: %s", tc.literal, out)
		}
		think, ok := genCfg["thinkingConfig"].(map[string]any)
		if !ok || think["thinkingLevel"] != tc.wire {
			t.Fatalf("%s wire must carry %s: %s", tc.literal, tc.wire, out)
		}
		if len(think) != 1 {
			t.Fatalf("%s thinkingConfig must carry only thinkingLevel: %s", tc.literal, out)
		}
	}
	// Dropped emits no thinkingConfig.
	chat, _ := NewChatEffort("medium")
	planned, _ := PlanEffort(chat, gemLevelCaps(t, nil), OrdinaryGeminiRoute())
	out, rerr := EncodeGeminiRequestWithPlan(gen(), planned)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if strings.Contains(string(out), "thinkingLevel") || strings.Contains(string(out), "thinkingConfig") {
		t.Fatalf("dropped must emit nothing: %s", out)
	}
	// Off and unreviewed literals are internal misuse at the encoder.
	if _, rerr := EncodeGeminiRequestWithPlan(gen(), OffEffortTarget()); rerr == nil || rerr.Status != 500 {
		t.Fatalf("off must be 500 misuse: %v", rerr)
	}
	if _, rerr := EncodeGeminiRequestWithPlan(gen(), NamedEffortTarget("max")); rerr == nil || rerr.Status != 500 {
		t.Fatalf("unreviewed must be 500 misuse: %v", rerr)
	}
}
