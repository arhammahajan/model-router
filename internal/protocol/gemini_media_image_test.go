package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

const g11PNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGP4z8DwHwAFAAH/iZk9HQAAAABJRU5ErkJggg=="

func TestG11ImageOrderPreserved(t *testing.T) {
	body := `{"contents":[{"role":"user","parts":[
		{"text":"a"},
		{"inlineData":{"mimeType":"image/png","data":"` + g11PNG + `"}},
		{"text":"b"},
		{"inlineData":{"mimeType":"IMAGE/JPEG","data":"eA=="}}
	]}]}`
	g, losses, rerr := DecodeGeminiRequest([]byte(body), "public", false, DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr.Msg)
	}
	if len(losses) != 0 {
		t.Fatalf("expected no losses, got %v", losses)
	}
	if len(g.Messages) != 1 || len(g.Messages[0].Blocks) != 4 {
		t.Fatalf("ordered blocks missing: %+v", g.Messages)
	}
	got := []string{}
	for _, b := range g.Messages[0].Blocks {
		if b.Kind == ContentKindImage {
			got = append(got, "img:"+b.Image.MIME)
		} else {
			got = append(got, "txt:"+b.Text)
		}
	}
	want := []string{"txt:a", "img:image/png", "txt:b", "img:image/jpeg"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order mismatch: got %v want %v", got, want)
		}
	}
	wire, rerr := EncodeChatRequest(g, AbsentEffort(), "up")
	if rerr != nil {
		t.Fatalf("encode: %v", rerr.Msg)
	}
	var m map[string]any
	if err := json.Unmarshal(wire, &m); err != nil {
		t.Fatal(err)
	}
	msgs, _ := m["messages"].([]any)
	content, _ := msgs[0].(map[string]any)["content"].([]any)
	if len(content) != 4 {
		t.Fatalf("wire content length=%d: %s", len(content), wire)
	}
	second, _ := content[1].(map[string]any)
	iu, _ := second["image_url"].(map[string]any)
	if iu["url"] != "data:image/png;base64,"+g11PNG {
		t.Fatalf("exact MIME/data not preserved: %v", iu["url"])
	}
	fourth, _ := content[3].(map[string]any)
	iu4, _ := fourth["image_url"].(map[string]any)
	if iu4["url"] != "data:image/jpeg;base64,eA==" {
		t.Fatalf("MIME not lowercased exactly: %v", iu4["url"])
	}
}

func TestG11MalformedImageIs400(t *testing.T) {
	for _, tc := range []string{
		`{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"not base64!!"}}]}]}`,
		`{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":""}}]}]}`,
		`{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"not-a-mime","data":"eA=="}}]}]}`,
		`{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":17,"data":"eA=="}}]}]}`,
		`{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png"}}]}]}`,
		`{"contents":[{"role":"assistant","parts":[{"text":"hi"}]}]}`,
	} {
		if _, _, rerr := DecodeGeminiRequest([]byte(tc), "m", false, DefaultLimits()); rerr == nil || rerr.Status != 400 {
			t.Fatalf("expected 400 for %s, got %v", tc, rerr)
		}
		if _, _, rerr := DecodeGeminiRequest([]byte(tc), "m", true, DefaultLimits()); rerr == nil || rerr.Status != 400 {
			t.Fatalf("expected 400 (stream) for %s, got %v", tc, rerr)
		}
	}
	// Oversize base64 is bounded pre-I/O.
	big := strings.Repeat("A", DefaultLimits().MaxArgsChars+1)
	raw := `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"` + big + `"}}]}]}`
	if _, _, rerr := DecodeGeminiRequest([]byte(raw), "m", false, DefaultLimits()); rerr == nil || rerr.Status != 400 {
		t.Fatalf("expected bound 400, got %v", rerr)
	}
}

func TestG11WellFormedUnrepresentableIsLoss(t *testing.T) {
	// Unlisted MIME to Chat: diagnosed loss, strict refuses via policy.
	raw := `{"contents":[{"role":"user","parts":[{"text":"hi"},{"inlineData":{"mimeType":"video/mp4","data":"eA=="}}]}]}`
	g, losses, rerr := DecodeGeminiRequest([]byte(raw), "m", false, DefaultLimits())
	if rerr != nil {
		t.Fatalf("well-formed video must decode with loss, got 400: %v", rerr.Msg)
	}
	if len(losses) == 0 {
		t.Fatalf("video must be diagnosed loss, got none")
	}
	if _, rerr := EnforcePolicy(losses, false); rerr == nil || rerr.Status != 400 {
		t.Fatalf("strict must refuse video loss, got %v", rerr)
	}
	if kept, rerr := EnforcePolicy(losses, true); rerr != nil || len(kept) == 0 {
		t.Fatalf("lossy must keep video loss, got %v %v", kept, rerr)
	}
	_ = g
	// Image display names remain a loss on both additional destinations.
	for _, dec := range []func([]byte, string, bool, Limits) (*Generation, []Loss, *RouterError){
		DecodeGeminiForResponses, DecodeGeminiForAnthropic,
	} {
		img := `{"contents":[{"role":"user","parts":[{"text":"hi"},{"inlineData":{"mimeType":"image/png","data":"eA==","displayName":"attachment"}}]}]}`
		_, losses, rerr := dec([]byte(img), "m", false, DefaultLimits())
		if rerr != nil {
			t.Fatalf("other-dest decode should loss, not 400: %v", rerr.Msg)
		}
		found := false
		for _, l := range losses {
			if strings.HasSuffix(l.Field, ".inlineData.displayName") {
				found = true
			}
		}
		if !found {
			t.Fatalf("other-dest image display name must be diagnosed loss, got %v", losses)
		}
	}
	// Model-produced image keeps the existing model-media loss (strict 400).
	modelImg := `{"contents":[{"role":"model","parts":[{"inlineData":{"mimeType":"image/png","data":"eA=="}}]}]}`
	if _, _, rerr := DecodeGeminiRequest([]byte(modelImg), "m", false, DefaultLimits()); rerr == nil || rerr.Status != 400 {
		t.Fatalf("model image must refuse in strict, got %v", rerr)
	}
	// Mixed content with functionResponse still refused.
	mixed := `{"contents":[{"role":"user","parts":[
		{"inlineData":{"mimeType":"image/png","data":"eA=="}},
		{"functionResponse":{"name":"f","response":{}}}]}],
		"tools":[{"functionDeclarations":[{"name":"f"}]}]}`
	if _, _, rerr := DecodeGeminiRequest([]byte(mixed), "m", false, DefaultLimits()); rerr == nil || rerr.Status != 400 {
		t.Fatalf("image+response mix must be 400, got %v", rerr)
	}
}

func TestGeminiImageCanonicalBase64ForChat(t *testing.T) {
	for _, data := range []string{"_w", "_w==", "/w", "/w=="} {
		raw := `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"` + data + `"}}]}]}`
		gen, losses, err := DecodeGeminiRequest([]byte(raw), "m", false, DefaultLimits())
		if err != nil || len(losses) != 0 {
			t.Fatalf("%q: expected byte-exact translation, got %v and %v", data, err, losses)
		}
		out, err := EncodeChatRequest(gen, AbsentEffort(), "up")
		if err != nil || !strings.Contains(string(out), "data:image/png;base64,/w==") {
			t.Fatalf("%q must encode the same bytes in standard padded base64: %v %s", data, err, out)
		}
	}
	for _, data := range []string{"eA======", "eA== eA==", `eA\n==`} {
		raw := `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"` + data + `"}}]}]}`
		if _, _, err := DecodeGeminiRequest([]byte(raw), "m", false, DefaultLimits()); err == nil || err.Status != 400 {
			t.Fatalf("%q must fail before media translation: %v", data, err)
		}
	}
}

func TestGeminiImageDisplayNameLoss(t *testing.T) {
	raw := `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"eA==","displayName":"sample"}}]}]}`
	_, losses, err := DecodeGeminiRequest([]byte(raw), "m", false, DefaultLimits())
	if err != nil || len(losses) != 1 || losses[0].Field != "contents[0].parts[0].inlineData.displayName" || losses[0].Action != "dropped" {
		t.Fatalf("dropping the display name requires a specific loss: %v %v", err, losses)
	}
}
