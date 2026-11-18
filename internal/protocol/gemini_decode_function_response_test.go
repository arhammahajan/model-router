package protocol

import (
	"strings"
	"testing"
)

func geminiFunctionBase(contents string) string {
	return `{"contents":[` + contents + `],"tools":[{"functionDeclarations":[{"name":"f"}]}]}`
}

func TestGeminiDecodeFunctionResponsePartsNarrow(t *testing.T) {
	modelCall := `{"role":"model","parts":[{"functionCall":{"name":"f","args":{}}}]},`
	userWith := func(fr string) string {
		return geminiFunctionBase(modelCall + `{"role":"user","parts":[{"functionResponse":` + fr + `}]}`)
	}
	// text payload is not part of the FunctionResponsePart union.
	expectGemini400(t, userWith(`{"name":"f","response":{},"parts":[{"text":17}]}`), false)
	expectGemini400(t, userWith(`{"name":"f","response":{},"parts":[{"text":"hi"}]}`), false)
	// functionCall is the wrong source union for result parts.
	expectGemini400(t, userWith(`{"name":"f","response":{},"parts":[{"functionCall":{"name":"f"}}]}`), false)
	// Unknown members are rejected, never silently dropped.
	expectGemini400(t, userWith(`{"name":"f","response":{},"parts":[{"inlineData":{"mimeType":"image/png","data":"eA=="},"bogus":1}]}`), false)
	expectGemini400(t, userWith(`{"name":"f","response":{},"parts":[17]}`), false)
	// Malformed inline/file shapes are 400 before any loss.
	expectGemini400(t, userWith(`{"name":"f","response":{},"parts":[{"inlineData":{"mimeType":17,"data":"eA=="}}]}`), false)
	expectGemini400(t, userWith(`{"name":"f","response":{},"parts":[{"inlineData":{"mimeType":"image/png"}}]}`), false)
	expectGemini400(t, userWith(`{"name":"f","response":{},"parts":[{"inlineData":{"mimeType":"image/png","data":"eA==","bogus":1}}]}`), false)
	expectGemini400(t, userWith(`{"name":"f","response":{},"parts":[{"fileData":{"mimeType":"image/png"}}]}`), false)
	expectGemini400(t, userWith(`{"name":"f","response":{},"parts":[{"fileData":{"fileUri":17}}]}`), false)
	// Exactly one payload: none and both are malformed.
	expectGemini400(t, userWith(`{"name":"f","response":{},"parts":[{}]}`), false)
	expectGemini400(t, userWith(`{"name":"f","response":{},"parts":[{"inlineData":{"mimeType":"image/png","data":"eA=="},"fileData":{"fileUri":"gs://b/o"}}]}`), false)
	// fileUri/fileURI collision is a duplicate.
	expectGemini400(t, userWith(`{"name":"f","response":{},"parts":[{"fileData":{"fileUri":"gs://b/o","fileURI":"gs://b/o"}}]}`), false)
	// Bad mediaProcessing enum is malformed.
	expectGemini400(t, userWith(`{"name":"f","response":{},"parts":[{"inlineData":{"mimeType":"image/png","data":"eA=="},"mediaProcessing":"ZOOM"}]}`), false)
	expectGemini400(t, userWith(`{"name":"f","response":{},"parts":[{"inlineData":{"mimeType":"image/png","data":"eA=="},"mediaProcessing":17}]}`), false)
	// Missing required response object is a source error, not a default.
	expectGemini400(t, userWith(`{"name":"f"}`), false)
	expectGemini400(t, userWith(`{"name":"f","response":null}`), false)
	expectGemini400(t, userWith(`{"name":"f","response":17}`), false)
}

func TestGeminiDecodeFunctionResponsePartsKnownLoss(t *testing.T) {
	validInline := geminiFunctionBase(
		`{"role":"model","parts":[{"functionCall":{"name":"f","args":{}}}]},` +
			`{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"ok":true},` +
			`"parts":[{"inlineData":{"mimeType":"image/png","data":"eA==","displayName":"b"}}]}}]}`)
	_, losses, rerr := DecodeGeminiRequest([]byte(validInline), "public-model", false, DefaultLimits())
	if rerr != nil {
		t.Fatalf("valid inline part must decode: %v", rerr.Msg)
	}
	if len(losses) == 0 {
		t.Fatalf("valid multimodal result must be diagnosed loss, got none")
	}
	if _, rerr := EnforcePolicy(losses, false); rerr == nil || rerr.Code != CodeTranslationLoss {
		t.Fatalf("strict must refuse known loss, got %v", rerr)
	}
	kept, rerr := EnforcePolicy(losses, true)
	if rerr != nil {
		t.Fatalf("lossy must keep plan: %v", rerr.Msg)
	}
	found := false
	for _, l := range kept {
		if strings.Contains(l.Field, "functionResponse.parts") {
			found = true
		}
	}
	if !found {
		t.Fatalf("loss plan missing parts field: %v", kept)
	}

	validFile := geminiFunctionBase(
		`{"role":"model","parts":[{"functionCall":{"name":"f","args":{}}}]},` +
			`{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"ok":true},` +
			`"parts":[{"fileData":{"fileUri":"gs://b/o","mimeType":"image/png"}}]}}]}`)
	_, losses2, rerr := DecodeGeminiRequest([]byte(validFile), "public-model", false, DefaultLimits())
	if rerr != nil {
		t.Fatalf("valid file part must decode: %v", rerr.Msg)
	}
	if _, rerr := EnforcePolicy(losses2, false); rerr == nil {
		t.Fatalf("strict must refuse file result loss")
	}
	if _, rerr := EnforcePolicy(losses2, true); rerr != nil {
		t.Fatalf("lossy must keep file plan: %v", rerr.Msg)
	}
}
