package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// Gemini->Chat bounded audio/PDF verticals: user inlineData audio (WAV/MP3
// MIME variants) to input_audio and inline PDF to file, preserving order
// among text, image, audio, and file blocks.

func TestGAPOrderPreserved(t *testing.T) {
	body := `{"contents":[{"role":"user","parts":[
		{"text":"a"},
		{"inlineData":{"mimeType":"audio/wav","data":"eA=="}},
		{"text":"b"},
		{"inlineData":{"mimeType":"application/pdf","data":"eA==","displayName":"a b.pdf"}},
		{"text":"c"},
		{"inlineData":{"mimeType":"image/png","data":"` + g11PNG + `"}}
	]}]}`
	g, losses, rerr := DecodeGeminiRequest([]byte(body), "public", false, DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr.Msg)
	}
	if len(losses) != 0 {
		t.Fatalf("expected no losses, got %v", losses)
	}
	if len(g.Messages) != 1 || len(g.Messages[0].Blocks) != 6 {
		t.Fatalf("ordered blocks missing: %+v", g.Messages)
	}
	got := []string{}
	for _, b := range g.Messages[0].Blocks {
		switch b.Kind {
		case ContentKindText:
			got = append(got, "txt:"+b.Text)
		case ContentKindAudio:
			got = append(got, "aud:"+b.Audio.Format)
		case ContentKindFile:
			got = append(got, "file:"+b.File.Filename)
		case ContentKindImage:
			got = append(got, "img:"+b.Image.MIME)
		default:
			t.Fatalf("unexpected kind %q", b.Kind)
		}
	}
	want := []string{"txt:a", "aud:wav", "txt:b", "file:a b.pdf", "txt:c", "img:image/png"}
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
	if len(content) != 6 {
		t.Fatalf("wire content length=%d: %s", len(content), wire)
	}
	// audio block
	second, _ := content[1].(map[string]any)
	if second["type"] != "input_audio" {
		t.Fatalf("audio type wrong: %s", wire)
	}
	ia, _ := second["input_audio"].(map[string]any)
	if ia["format"] != "wav" || ia["data"] != "eA==" {
		t.Fatalf("audio payload wrong: %s", wire)
	}
	// pdf block
	fourth, _ := content[3].(map[string]any)
	if fourth["type"] != "file" {
		t.Fatalf("file type wrong: %s", wire)
	}
	f, _ := fourth["file"].(map[string]any)
	if f["file_data"] != "data:application/pdf;base64,eA==" || f["filename"] != "a b.pdf" {
		t.Fatalf("file payload wrong: %s", wire)
	}
}

func TestGAPAudioFormatMapping(t *testing.T) {
	cases := map[string]string{
		"audio/wav":   "wav",
		"audio/x-wav": "wav",
		"audio/wave":  "wav",
		"AUDIO/MPEG":  "mp3",
		"audio/mp3":   "mp3",
	}
	for mime, format := range cases {
		raw := `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"` + mime + `","data":"eA=="}}]}]}`
		g, losses, err := DecodeGeminiRequest([]byte(raw), "m", false, DefaultLimits())
		if err != nil || len(losses) != 0 {
			t.Fatalf("%s: expected exact translation, got %v %v", mime, err, losses)
		}
		if len(g.Messages) != 1 || len(g.Messages[0].Blocks) != 1 || g.Messages[0].Blocks[0].Kind != ContentKindAudio {
			t.Fatalf("%s: audio block missing: %+v", mime, g.Messages)
		}
		if g.Messages[0].Blocks[0].Audio.Format != format {
			t.Fatalf("%s: format=%q want %q", mime, g.Messages[0].Blocks[0].Audio.Format, format)
		}
		out, err2 := EncodeChatRequest(g, AbsentEffort(), "up")
		if err2 != nil || !strings.Contains(string(out), `"format":"`+format+`"`) {
			t.Fatalf("%s: wire format wrong: %v %s", mime, err2, out)
		}
	}
}

func TestGAPAudioCanonicalBase64(t *testing.T) {
	for _, data := range []string{"_w", "/w", "_w==", "/w=="} {
		raw := `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"audio/mpeg","data":"` + data + `"}}]}]}`
		g, losses, err := DecodeGeminiRequest([]byte(raw), "m", false, DefaultLimits())
		if err != nil || len(losses) != 0 {
			t.Fatalf("%q: expected exact translation, got %v %v", data, err, losses)
		}
		out, err2 := EncodeChatRequest(g, AbsentEffort(), "up")
		if err2 != nil || !strings.Contains(string(out), `"data":"/w=="`) {
			t.Fatalf("%q must encode the same bytes in standard padded base64: %v %s", data, err2, out)
		}
	}
}

func TestGAPPDFfilename(t *testing.T) {
	// Absent displayName uses the default with no loss.
	raw := `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"application/pdf","data":"eA=="}}]}]}`
	g, losses, err := DecodeGeminiRequest([]byte(raw), "m", false, DefaultLimits())
	if err != nil || len(losses) != 0 {
		t.Fatalf("absent filename must be exact, got %v %v", err, losses)
	}
	if got := g.Messages[0].Blocks[0].File.Filename; got != "document.pdf" {
		t.Fatalf("default filename=%q want document.pdf", got)
	}
	out, _ := EncodeChatRequest(g, AbsentEffort(), "up")
	if !strings.Contains(string(out), `"filename":"document.pdf"`) {
		t.Fatalf("default filename missing on wire: %s", out)
	}
	// Safe displayName is preserved exactly with no loss.
	raw = `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"application/pdf","data":"eA==","displayName":"report 1.pdf"}}]}]}`
	g, losses, err = DecodeGeminiRequest([]byte(raw), "m", false, DefaultLimits())
	if err != nil || len(losses) != 0 {
		t.Fatalf("safe filename must be exact, got %v %v", err, losses)
	}
	if got := g.Messages[0].Blocks[0].File.Filename; got != "report 1.pdf" {
		t.Fatalf("safe filename=%q", got)
	}
	// Unsafe displayName falls back with an explicit dropped loss that never
	// leaks the value.
	for _, bad := range []string{"../evil.pdf", "a/b.pdf", strings.Repeat("a", 101) + ".pdf"} {
		raw = `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"application/pdf","data":"eA==","displayName":` + gapJSON(t, bad) + `}}]}]}`
		g, losses, err = DecodeGeminiRequest([]byte(raw), "m", false, DefaultLimits())
		if err != nil {
			t.Fatalf("unsafe filename %q must decode with loss, got 400: %v", bad, err.Msg)
		}
		if len(losses) != 1 || losses[0].Field != "contents[0].parts[0].inlineData.displayName" || losses[0].Action != "dropped" {
			t.Fatalf("unsafe filename %q must be a dropped displayName loss, got %v", bad, losses)
		}
		if strings.Contains(EncodeLosses(losses), bad) || strings.Contains(losses[0].Reason, bad) {
			t.Fatalf("loss must never leak the filename value: %v", losses)
		}
		if got := g.Messages[0].Blocks[0].File.Filename; got != "document.pdf" {
			t.Fatalf("unsafe filename %q must fall back, got %q", bad, got)
		}
		if _, rerr := EnforcePolicy(losses, false); rerr == nil {
			t.Fatalf("strict must refuse unsafe filename fallback")
		}
		kept, rerr := EnforcePolicy(losses, true)
		if rerr != nil {
			t.Fatalf("lossy must keep fallback loss: %v", rerr)
		}
		_ = kept
		out, _ := EncodeChatRequest(g, AbsentEffort(), "up")
		if !strings.Contains(string(out), `"filename":"document.pdf"`) {
			t.Fatalf("fallback filename missing on wire: %s", out)
		}
	}
}

func gapJSON(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestGAPMalformedIs400(t *testing.T) {
	for _, tc := range []string{
		`{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"audio/wav","data":"not base64!!"}}]}]}`,
		`{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"audio/wav","data":""}}]}]}`,
		`{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"application/pdf","data":"not base64!!"}}]}]}`,
		`{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"application/pdf","data":""}}]}]}`,
		`{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"not-a-mime","data":"eA=="}}]}]}`,
		`{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"audio/wav"}}]}]}`,
		`{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"audio/wav","data":"eA==","displayName":17}}]}]}`,
		`{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"audio/wav","data":"eA==","displayName":""}}]}]}`,
		`{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"application/pdf","data":"eA==","displayName":""}}]}]}`,
	} {
		if _, _, rerr := DecodeGeminiRequest([]byte(tc), "m", false, DefaultLimits()); rerr == nil || rerr.Status != 400 {
			t.Fatalf("expected 400 for %s, got %v", tc, rerr)
		}
		if _, _, rerr := DecodeGeminiRequest([]byte(tc), "m", true, DefaultLimits()); rerr == nil || rerr.Status != 400 {
			t.Fatalf("expected 400 (stream) for %s, got %v", tc, rerr)
		}
	}
	big := strings.Repeat("A", DefaultLimits().MaxArgsChars+1)
	for _, mime := range []string{"audio/wav", "application/pdf"} {
		raw := `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"` + mime + `","data":"` + big + `"}}]}]}`
		if _, _, rerr := DecodeGeminiRequest([]byte(raw), "m", false, DefaultLimits()); rerr == nil || rerr.Status != 400 {
			t.Fatalf("expected bound 400 for %s, got %v", mime, rerr)
		}
	}
}

func TestGAPUnrepresentableIsLoss(t *testing.T) {
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
	// Audio is carried only to Chat.
	for _, dec := range []func([]byte, string, bool, Limits) (*Generation, []Loss, *RouterError){
		DecodeGeminiForResponses, DecodeGeminiForAnthropic,
	} {
		audio := `{"contents":[{"role":"user","parts":[{"text":"hi"},{"inlineData":{"mimeType":"audio/wav","data":"eA=="}}]}]}`
		_, losses, rerr := dec([]byte(audio), "m", false, DefaultLimits())
		if rerr != nil || len(losses) != 1 || !strings.HasSuffix(losses[0].Field, ".inlineData") {
			t.Fatalf("other-dest audio must be diagnosed as a loss: %v %v", losses, rerr)
		}
		if _, err := EnforcePolicy(losses, false); err == nil {
			t.Fatal("strict must refuse unsupported audio")
		}
	}
	// Model-produced audio/PDF keeps the existing model-media loss (never 400).
	for _, mime := range []string{"audio/wav", "application/pdf"} {
		model := `{"contents":[{"role":"model","parts":[{"text":"hi"},{"inlineData":{"mimeType":"` + mime + `","data":"not base64!!"}}]}]}`
		_, losses, rerr := DecodeGeminiRequest([]byte(model), "m", false, DefaultLimits())
		if rerr != nil {
			t.Fatalf("model %s malformed must be loss, got 400: %v", mime, rerr.Msg)
		}
		if len(losses) == 0 {
			t.Fatalf("model %s must be diagnosed loss", mime)
		}
	}
	// Mixed content with functionResponse still refused.
	mixed := `{"contents":[{"role":"user","parts":[
		{"inlineData":{"mimeType":"audio/wav","data":"eA=="}},
		{"functionResponse":{"name":"f","response":{}}}]}],
		"tools":[{"functionDeclarations":[{"name":"f"}]}]}`
	if _, _, rerr := DecodeGeminiRequest([]byte(mixed), "m", false, DefaultLimits()); rerr == nil || rerr.Status != 400 {
		t.Fatalf("audio+response mix must be 400, got %v", rerr)
	}
	mixedPDF := `{"contents":[{"role":"user","parts":[
		{"inlineData":{"mimeType":"application/pdf","data":"eA=="}},
		{"functionResponse":{"name":"f","response":{}}}]}],
		"tools":[{"functionDeclarations":[{"name":"f"}]}]}`
	if _, _, rerr := DecodeGeminiRequest([]byte(mixedPDF), "m", false, DefaultLimits()); rerr == nil || rerr.Status != 400 {
		t.Fatalf("pdf+response mix must be 400, got %v", rerr)
	}
}

func TestGAPDisplayNameLoss(t *testing.T) {
	// Image and audio display names have no Chat equivalent: dropped loss
	// while the media still bridges.
	for _, mime := range []string{"image/png", "audio/wav"} {
		raw := `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"` + mime + `","data":"eA==","displayName":"sample"}}]}]}`
		g, losses, err := DecodeGeminiRequest([]byte(raw), "m", false, DefaultLimits())
		if err != nil || len(losses) != 1 || losses[0].Field != "contents[0].parts[0].inlineData.displayName" || losses[0].Action != "dropped" {
			t.Fatalf("%s display name loss wrong: %v %v", mime, err, losses)
		}
		if _, rerr := EnforcePolicy(losses, false); rerr == nil {
			t.Fatalf("%s strict must refuse displayName loss", mime)
		}
		out, rerr := EncodeChatRequest(g, AbsentEffort(), "up")
		if rerr != nil {
			t.Fatalf("%s lossy encode: %v", mime, rerr)
		}
		if mime == "image/png" && !strings.Contains(string(out), "image_url") {
			t.Fatalf("%s loss must still bridge: %s", mime, out)
		}
		if mime == "audio/wav" && !strings.Contains(string(out), "input_audio") {
			t.Fatalf("%s loss must still bridge: %s", mime, out)
		}
	}
}
