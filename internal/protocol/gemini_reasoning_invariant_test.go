package protocol

import "testing"

func TestGeminiLevelConstructorUsesSourceVocabulary(t *testing.T) {
	for _, level := range []string{"none", "xhigh", "max", "LOW", " low", ""} {
		if _, ok := NewGeminiLevel(level); ok {
			t.Errorf("constructor admitted a value outside the Gemini level vocabulary: %q", level)
		}
	}
	for _, level := range []string{"minimal", "low", "medium", "high"} {
		intent, ok := NewGeminiLevel(level)
		if !ok || intent.Kind() != KindNamed || intent.Origin() != GeminiLevelOrigin.Path() {
			t.Errorf("valid Gemini level rejected or changed: %q", level)
		}
	}
}

func TestGeminiDefaultHasNoReasoningOrigin(t *testing.T) {
	for _, value := range []string{`"thinkingBudget":-1`, `"thinkingLevel":"THINKING_LEVEL_UNSPECIFIED"`} {
		raw := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{` + value + `}}}`)
		gen, losses, rerr := DecodeGeminiRequest(raw, "m", false, DefaultLimits())
		if rerr != nil || len(losses) != 0 {
			t.Fatalf("default decoding: %v, losses %v", rerr, losses)
		}
		if gen.Reasoning.IsPresent() || gen.Reasoning.Origin() != "" {
			t.Errorf("default fabricated a control or origin: %+v", gen.Reasoning)
		}
	}
}
