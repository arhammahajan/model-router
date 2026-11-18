package protocol

import (
	"strings"
	"testing"
)

func TestGeminiDecodeGenerationCompoundNarrow(t *testing.T) {
	base := func(gen string) string {
		return `{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{` + gen + `}}`
	}
	// mediaResolution is an ENUM string, not an object or number.
	expectGemini400(t, base(`"mediaResolution":{}`), false)
	expectGemini400(t, base(`"mediaResolution":17`), false)
	expectGemini400(t, base(`"mediaResolution":"ZOOM"`), false)
	// audioTimestamp must be bool before any loss.
	expectGemini400(t, base(`"audioTimestamp":"yes"`), false)
	expectGemini400(t, base(`"audioTimestamp":17`), false)
	expectGemini400(t, base(`"audioTimestamp":{}`), false)
	// speechConfig: unknown members and malformed nested values are 400.
	expectGemini400(t, base(`"speechConfig":{"bogus":1}`), false)
	expectGemini400(t, base(`"speechConfig":{"languageCode":17}`), false)
	expectGemini400(t, base(`"speechConfig":{"voiceConfig":"x"}`), false)
	expectGemini400(t, base(`"speechConfig":{"voiceConfig":{"bogus":1}}`), false)
	expectGemini400(t, base(`"speechConfig":{"voiceConfig":{"voice":17}}`), false)
	expectGemini400(t, base(`"speechConfig":{"voiceConfig":{},"multiSpeakerVoiceConfig":{"speakerVoiceConfigs":[]}}`), false)
	expectGemini400(t, base(`"speechConfig":{"multiSpeakerVoiceConfig":{"speakerVoiceConfigs":[{"speaker":"","voiceConfig":{}}]}}`), false)
	expectGemini400(t, base(`"speechConfig":{"multiSpeakerVoiceConfig":{"bogus":1}}`), false)
	// imageConfig: unknown members and bad enums are 400.
	expectGemini400(t, base(`"imageConfig":{"bogus":1}`), false)
	expectGemini400(t, base(`"imageConfig":{"aspectRatio":"square"}`), false)
	expectGemini400(t, base(`"imageConfig":{"imageSize":"huge"}`), false)
	expectGemini400(t, base(`"imageConfig":{"aspectRatio":17}`), false)
	// retrievalConfig: closed latLng/languageCode validation.
	expectGemini400(t, `{"contents":[{"parts":[{"text":"hi"}]}],"toolConfig":{"retrievalConfig":{"bogus":1}}}`, false)
	expectGemini400(t, `{"contents":[{"parts":[{"text":"hi"}]}],"toolConfig":{"retrievalConfig":{"latLng":"x"}}}`, false)
	expectGemini400(t, `{"contents":[{"parts":[{"text":"hi"}]}],"toolConfig":{"retrievalConfig":{"latLng":{"latitude":"x"}}}}`, false)
	// Part metadata: malformed values are 400 in both modes.
	expectGemini400(t, `{"contents":[{"parts":[{"text":"hi","mediaProcessing":17}]}]}`, false)
	expectGemini400(t, `{"contents":[{"parts":[{"text":"hi","mediaProcessing":"ZOOM"}]}]}`, false)
	expectGemini400(t, `{"contents":[{"parts":[{"text":"hi","videoMetadata":{"fps":"fast"}}]}]}`, false)
	expectGemini400(t, `{"contents":[{"parts":[{"text":"hi","videoMetadata":{"bogus":1}}]}]}`, false)
	expectGemini400(t, `{"contents":[{"parts":[{"text":"hi","mediaResolution":"LOW"}]}]}`, false)
	// Typed schemas: quoted numerics are invalid source constraints.
	expectGemini400(t, `{"contents":[{"parts":[{"text":"hi"}]}],"tools":[{"functionDeclarations":[{"name":"f","parameters":{"type":"OBJECT","minimum":"0.5"}}]}]}`, false)
	expectGemini400(t, `{"contents":[{"parts":[{"text":"hi"}]}],"tools":[{"functionDeclarations":[{"name":"f","parameters":{"type":"OBJECT","maximum":"0.5"}}]}]}`, false)
	expectGemini400(t, `{"contents":[{"parts":[{"text":"hi"}]}],"tools":[{"functionDeclarations":[{"name":"f","parameters":{"type":"OBJECT","minimum":10,"maximum":5}}]}]}`, false)
}

func TestGeminiDecodeGenerationCompoundKnownLoss(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"mediaResolution", `{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"mediaResolution":"MEDIA_RESOLUTION_LOW"}}`, "generationConfig.mediaResolution"},
		{"audioTimestamp", `{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"audioTimestamp":true}}`, "generationConfig.audioTimestamp"},
		{"speechConfig", `{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"speechConfig":{"languageCode":"en-US"}}}`, "generationConfig.speechConfig"},
		{"imageConfig", `{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"imageConfig":{"aspectRatio":"16:9","imageSize":"1K"}}}`, "generationConfig.imageConfig"},
	}
	for _, tc := range cases {
		_, losses, rerr := DecodeGeminiRequest([]byte(tc.body), "public-model", false, DefaultLimits())
		if rerr != nil {
			t.Fatalf("%s valid must decode: %v", tc.name, rerr.Msg)
		}
		found := false
		for _, l := range losses {
			if strings.Contains(l.Field, tc.want) {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s loss missing %q: %v", tc.name, tc.want, losses)
		}
		if _, rerr := EnforcePolicy(losses, false); rerr == nil || rerr.Code != CodeTranslationLoss {
			t.Fatalf("%s strict must refuse, got %v", tc.name, rerr)
		}
		kept, rerr := EnforcePolicy(losses, true)
		if rerr != nil {
			t.Fatalf("%s lossy must keep: %v", tc.name, rerr.Msg)
		}
		found = false
		for _, l := range kept {
			if strings.Contains(l.Field, tc.want) {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s lossy plan missing %q: %v", tc.name, tc.want, kept)
		}
	}
}
