package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// Chat->Gemini bounded image vertical: user image_url data URLs bridge to
// Gemini inlineData, preserving order among text and image parts.

func c2gMediaDecode(t *testing.T, body string) (*Generation, []Loss) {
	t.Helper()
	g, losses, rerr := DecodeChatForGemini([]byte(body), DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode failed: %v", rerr)
	}
	return g, losses
}

func TestC2GImageOrderPreserved(t *testing.T) {
	png := g11PNG
	body := `{"model":"m","messages":[{"role":"user","content":[
		{"type":"text","text":"a"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,` + png + `"}},
		{"type":"text","text":"b"},
		{"type":"image_url","image_url":{"url":"data:IMAGE/JPEG;base64,eA=="}}
	]}]}`
	g, losses := c2gMediaDecode(t, body)
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
	out, rerr := EncodeGeminiRequest(g)
	if rerr != nil {
		t.Fatalf("encode: %v", rerr.Msg)
	}
	var wire struct {
		Contents []struct {
			Role  string `json:"role"`
			Parts []struct {
				Text       string `json:"text"`
				InlineData *struct {
					MIME string `json:"mimeType"`
					Data string `json:"data"`
				} `json:"inlineData"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Contents) != 1 || len(wire.Contents[0].Parts) != 4 {
		t.Fatalf("wire parts length wrong: %s", out)
	}
	if wire.Contents[0].Parts[0].Text != "a" || wire.Contents[0].Parts[2].Text != "b" {
		t.Fatalf("text order wrong: %s", out)
	}
	if wire.Contents[0].Parts[1].InlineData == nil || wire.Contents[0].Parts[1].InlineData.MIME != "image/png" ||
		wire.Contents[0].Parts[1].InlineData.Data != png {
		t.Fatalf("exact MIME/data not preserved: %s", out)
	}
	if wire.Contents[0].Parts[3].InlineData == nil || wire.Contents[0].Parts[3].InlineData.MIME != "image/jpeg" ||
		wire.Contents[0].Parts[3].InlineData.Data != "eA==" {
		t.Fatalf("MIME not lowercased exactly: %s", out)
	}
}

func TestC2GImageCanonicalBase64(t *testing.T) {
	for _, data := range []string{"_w", "_w==", "/w", "/w=="} {
		raw := `{"model":"m","messages":[{"role":"user","content":[
			{"type":"image_url","image_url":{"url":"data:image/png;base64,` + data + `"}}]}]}`
		gen, losses, err := DecodeChatForGemini([]byte(raw), DefaultLimits())
		if err != nil || len(losses) != 0 {
			t.Fatalf("%q: expected exact translation, got %v %v", data, err, losses)
		}
		out, err2 := EncodeGeminiRequest(gen)
		if err2 != nil || !strings.Contains(string(out), `"data":"/w=="`) {
			t.Fatalf("%q must encode the same bytes in standard padded base64: %v %s", data, err2, out)
		}
	}
}

func TestC2GImageMalformedIs400(t *testing.T) {
	cases := []string{
		// bad base64 payload
		`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,not base64!!"}}]}]}`,
		// empty payload
		`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,"}}]}]}`,
		// bad MIME syntax
		`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:not-a-mime;base64,eA=="}}]}]}`,
		// missing ;base64 marker
		`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png,eA=="}}]}]}`,
		// missing comma
		`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64eA=="}}]}]}`,
		// empty url
		`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":""}}]}]}`,
		// non-string url
		`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":17}}]}]}`,
	}
	for _, tc := range cases {
		if _, _, rerr := DecodeChatForGemini([]byte(tc), DefaultLimits()); rerr == nil || rerr.Status != 400 {
			t.Fatalf("expected 400 for %s, got %v", tc, rerr)
		}
	}
	big := strings.Repeat("A", DefaultLimits().MaxArgsChars+1)
	raw := `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + big + `"}}]}]}`
	if _, _, rerr := DecodeChatForGemini([]byte(raw), DefaultLimits()); rerr == nil || rerr.Status != 400 {
		t.Fatalf("expected bound 400, got %v", rerr)
	}
}

func TestC2GImageUnrepresentableIsLoss(t *testing.T) {
	// Remote URL keeps the existing reviewed loss.
	remote := `{"model":"m","messages":[{"role":"user","content":[
		{"type":"text","text":"hi"},
		{"type":"image_url","image_url":{"url":"https://example.invalid/img.png"}}]}]}`
	g, losses, rerr := DecodeChatForGemini([]byte(remote), DefaultLimits())
	if rerr != nil {
		t.Fatalf("remote must decode with loss, got 400: %v", rerr.Msg)
	}
	if len(losses) == 0 {
		t.Fatalf("remote must be diagnosed loss, got none")
	}
	if _, rerr := EnforcePolicy(losses, false); rerr == nil || rerr.Status != 400 {
		t.Fatalf("strict must refuse remote loss, got %v", rerr)
	}
	kept, rerr := EnforcePolicy(losses, true)
	if rerr != nil || len(kept) == 0 {
		t.Fatalf("lossy must keep remote loss, got %v %v", kept, rerr)
	}
	out, rerr := EncodeGeminiRequest(g)
	if rerr != nil {
		t.Fatalf("lossy encode: %v", rerr.Msg)
	}
	if strings.Contains(string(out), "inlineData") {
		t.Fatalf("remote must never reach Gemini wire: %s", out)
	}
	// Unlisted data-URL MIME is a diagnosed loss.
	video := `{"model":"m","messages":[{"role":"user","content":[
		{"type":"text","text":"hi"},
		{"type":"image_url","image_url":{"url":"data:video/mp4;base64,eA=="}}]}]}`
	_, losses, rerr = DecodeChatForGemini([]byte(video), DefaultLimits())
	if rerr != nil {
		t.Fatalf("well-formed video must decode with loss, got 400: %v", rerr.Msg)
	}
	found := false
	for _, l := range losses {
		if strings.Contains(l.Field, "image_url") {
			found = true
		}
	}
	if !found {
		t.Fatalf("video must be diagnosed loss, got %v", losses)
	}
	if _, rerr := EnforcePolicy(losses, false); rerr == nil || rerr.Status != 400 {
		t.Fatalf("strict must refuse video loss, got %v", rerr)
	}
}

func TestC2GImageDetailLoss(t *testing.T) {
	// Absent/auto detail is exact.
	for _, detail := range []string{``, `,"detail":"auto"`} {
		raw := `{"model":"m","messages":[{"role":"user","content":[
			{"type":"text","text":"hi"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,eA=="` + detail + `}}]}]}`
		_, losses, rerr := DecodeChatForGemini([]byte(raw), DefaultLimits())
		if rerr != nil || len(losses) != 0 {
			t.Fatalf("detail %q must be exact, got %v %v", detail, rerr, losses)
		}
	}
	// Explicit low/high has no Gemini equivalent: diagnosed loss, image still bridges.
	for _, detail := range []string{"low", "high"} {
		raw := `{"model":"m","messages":[{"role":"user","content":[
			{"type":"text","text":"hi"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,eA==","detail":"` + detail + `"}}]}]}`
		g, losses, rerr := DecodeChatForGemini([]byte(raw), DefaultLimits())
		if rerr != nil {
			t.Fatalf("detail %q must decode with loss, got 400: %v", detail, rerr.Msg)
		}
		found := false
		for _, l := range losses {
			if l.Field == "messages[0].content[1].image_url.detail" {
				found = true
			}
		}
		if !found {
			t.Fatalf("detail %q must be diagnosed loss, got %v", detail, losses)
		}
		if _, rerr := EnforcePolicy(losses, false); rerr == nil || rerr.Status != 400 {
			t.Fatalf("strict must refuse detail loss, got %v", rerr)
		}
		out, rerr := EncodeGeminiRequest(g)
		if rerr != nil {
			t.Fatalf("lossy encode: %v", rerr.Msg)
		}
		if !strings.Contains(string(out), `"mimeType":"image/png"`) {
			t.Fatalf("detail loss must still bridge the image: %s", out)
		}
	}
}

func TestC2GImageRolePolicy(t *testing.T) {
	// Non-user image_url keeps the existing reviewed loss and never bridges.
	for _, role := range []string{"system", "assistant"} {
		raw := `{"model":"m","messages":[{"role":"` + role + `","content":[
			{"type":"text","text":"hi"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,eA=="}}]}]}`
		g, losses, rerr := DecodeChatForGemini([]byte(raw), DefaultLimits())
		if rerr != nil {
			t.Fatalf("%s image must decode with loss, got 400: %v", role, rerr.Msg)
		}
		if len(losses) == 0 {
			t.Fatalf("%s image must be diagnosed loss, got none", role)
		}
		if len(g.Messages[0].Blocks) != 0 {
			t.Fatalf("%s image must never produce Blocks: %+v", role, g.Messages)
		}
	}
	// String-form image_url data URL also bridges on user messages.
	raw := `{"model":"m","messages":[{"role":"user","content":[
		{"type":"text","text":"hi"},
		{"type":"image_url","image_url":"data:image/gif;base64,eA=="}]}]}`
	g, losses, rerr := DecodeChatForGemini([]byte(raw), DefaultLimits())
	if rerr != nil || len(losses) != 0 {
		t.Fatalf("string data URL must bridge exactly, got %v %v", rerr, losses)
	}
	if len(g.Messages[0].Blocks) != 2 {
		t.Fatalf("string data URL blocks missing: %+v", g.Messages)
	}
}

func TestC2GImageOtherDestUnchanged(t *testing.T) {
	// Anthropic and Responses destinations keep the existing reviewed loss.
	raw := `{"model":"m","messages":[{"role":"user","content":[
		{"type":"text","text":"hi"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,eA=="}}]}]}`
	g, losses, rerr := DecodeChatRequest([]byte(raw), DefaultLimits())
	if rerr != nil {
		t.Fatalf("anthropic dest must loss, not 400: %v", rerr.Msg)
	}
	if len(losses) == 0 || len(g.Messages[0].Blocks) != 0 {
		t.Fatalf("anthropic dest must not bridge: %v %+v", losses, g.Messages)
	}
	g2, losses2, rerr := DecodeChatForResponses([]byte(raw), DefaultLimits())
	if rerr != nil {
		t.Fatalf("responses dest must loss, not 400: %v", rerr.Msg)
	}
	if len(losses2) == 0 || len(g2.Messages[0].Blocks) != 0 {
		t.Fatalf("responses dest must not bridge: %v %+v", losses2, g2.Messages)
	}
	// Malformed data URLs stay 400 on the Gemini destination but keep the
	// existing reviewed loss on the other Chat destinations (native paths).
	bad := `{"model":"m","messages":[{"role":"user","content":[
		{"type":"image_url","image_url":{"url":"data:image/png;base64,not base64!!"}}]}]}`
	if _, _, rerr := DecodeChatForGemini([]byte(bad), DefaultLimits()); rerr == nil || rerr.Status != 400 {
		t.Fatalf("gemini malformed must be 400, got %v", rerr)
	}
	if _, losses, rerr := DecodeChatRequest([]byte(bad), DefaultLimits()); rerr != nil || len(losses) == 0 {
		t.Fatalf("anthropic malformed keeps the reviewed loss, got %v %v", rerr, losses)
	}
	if _, losses, rerr := DecodeChatForResponses([]byte(bad), DefaultLimits()); rerr != nil || len(losses) == 0 {
		t.Fatalf("responses malformed keeps the reviewed loss, got %v %v", rerr, losses)
	}
}
