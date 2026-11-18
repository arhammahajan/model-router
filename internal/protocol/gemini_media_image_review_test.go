package protocol

import (
	"strings"
	"testing"
)

// Review of the integrated Gemini->Chat inline-image vertical (a8bf3e1).
//
// Finding 1 (primary): model-role inlineData is base64/MIME/bound-validated
// (400 in both modes) even though model media is never bridged. The stable
// Python bridge (HEAD:model_router/gemini.py, model_turn) diagnoses
// model-produced parts as a loss WITHOUT validating the media payload, so
// lossy mode forwards the remaining conversation with a loss header while
// Go hard-400s. Replayed history containing a stale, malformed, or simply
// large (>MaxArgsChars) model-generated image therefore regresses from
// forward-with-loss to refusal. The same fail-closed validation is correct
// on the bridged path (user->Chat), where bytes reach the upstream.
//
// Finding 2 (minor): the mixed-content rejection was extended to images
// ((hasText || hasImage) && hasResponse) but the message still says
// "mixes text with function responses" when only an image is mixed.
//
// Out of scope (already handled elsewhere): URL-safe/unpadded/excessively
// padded base64 and silent inline displayName loss.

// Stable-Python parity: model_turn loses model-produced parts without media
// validation for every destination, so malformed model media must decode
// with a diagnosed loss, not a 400.
func TestReviewModelMalformedImageIsLossNot400(t *testing.T) {
	bodies := map[string]string{
		"bad-base64": `{"contents":[{"role":"model","parts":[{"text":"hi"},{"inlineData":{"mimeType":"image/png","data":"not base64!!"}}]}]}`,
		"bad-mime":   `{"contents":[{"role":"model","parts":[{"text":"hi"},{"inlineData":{"mimeType":"not-a-mime","data":"eA=="}}]}]}`,
	}
	decs := map[string]func([]byte, string, bool, Limits) (*Generation, []Loss, *RouterError){
		"chat":      DecodeGeminiRequest,
		"responses": DecodeGeminiForResponses,
		"anthropic": DecodeGeminiForAnthropic,
	}
	for name, body := range bodies {
		for dest, dec := range decs {
			for _, stream := range []bool{false, true} {
				g, losses, rerr := dec([]byte(body), "m", stream, DefaultLimits())
				if rerr != nil {
					t.Fatalf("%s/%s/stream=%v: malformed model media must decode with a loss, got 400: %v",
						name, dest, stream, rerr.Msg)
				}
				found := false
				for _, l := range losses {
					if strings.Contains(l.Field, "inlineData") {
						found = true
					}
				}
				if !found {
					t.Fatalf("%s/%s/stream=%v: model media must be a diagnosed loss, got %v",
						name, dest, stream, losses)
				}
				if g == nil || len(g.Messages) != 1 || len(g.Messages[0].Texts) != 1 ||
					g.Messages[0].Texts[0].Text != "hi" {
					t.Fatalf("%s/%s/stream=%v: model text must survive alongside the loss, got %+v",
						name, dest, stream, g)
				}
			}
		}
	}
}

// Stable-Python parity: no per-field bound applies to never-forwarded model
// media (Python validates base64 syntax for user turns but bounds nothing).
// A large model-generated image replayed in history must be a diagnosed
// loss in lossy mode, not a bound 400.
func TestReviewModelOversizeImageIsLossNot400(t *testing.T) {
	big := strings.Repeat("A", DefaultLimits().MaxArgsChars+1)
	body := `{"contents":[{"role":"model","parts":[{"text":"hi"},{"inlineData":{"mimeType":"image/png","data":"` + big + `"}}]}]}`
	g, losses, rerr := DecodeGeminiRequest([]byte(body), "m", false, DefaultLimits())
	if rerr != nil {
		t.Fatalf("oversize model media must decode with a loss, got 400: %v", rerr.Msg)
	}
	found := false
	for _, l := range losses {
		if strings.Contains(l.Field, "inlineData") {
			found = true
		}
	}
	if !found {
		t.Fatalf("oversize model media must be a diagnosed loss, got %v", losses)
	}
	if g == nil || len(g.Messages) != 1 {
		t.Fatalf("model text must survive alongside the loss, got %+v", g)
	}
	kept, rerr := EnforcePolicy(losses, true)
	if rerr != nil {
		t.Fatalf("lossy must keep the model-media loss, got %v", rerr)
	}
	wire, rerr := EncodeChatRequest(g, AbsentEffort(), "up")
	if rerr != nil {
		t.Fatalf("lossy encode: %v", rerr.Msg)
	}
	if strings.Contains(string(wire), "image") {
		t.Fatalf("model media must never reach the Chat wire: %s", wire)
	}
	if !strings.Contains(string(wire), `"content":"hi"`) {
		t.Fatalf("model text must forward in lossy mode: %s", wire)
	}
	_ = kept
}

// The vertical extends the mixing rejection to images, but the message was
// not extended with it: an image-only mix reports "mixes text".
func TestReviewImageResponseMixMessage(t *testing.T) {
	mixed := `{"contents":[
		{"role":"model","parts":[{"functionCall":{"name":"f","args":{}}}]},
		{"role":"user","parts":[
			{"inlineData":{"mimeType":"image/png","data":"eA=="}},
			{"functionResponse":{"name":"f","response":{}}}]}],
		"tools":[{"functionDeclarations":[{"name":"f"}]}]}`
	_, _, rerr := DecodeGeminiRequest([]byte(mixed), "m", false, DefaultLimits())
	if rerr == nil || rerr.Status != 400 {
		t.Fatalf("image+response mix must be 400, got %v", rerr)
	}
	if rerr.Code != CodeInvalidGeneration {
		t.Fatalf("mixing rejection must keep CodeInvalidGeneration, got %q", rerr.Code)
	}
	if strings.Contains(rerr.Msg, "mixes text") {
		t.Fatalf("image-only mix must not report \"text\": %q", rerr.Msg)
	}
}
