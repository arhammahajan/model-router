package protocol

import (
	"strings"
	"testing"
)

func TestGeminiIngressShapeValidation(t *testing.T) {
	// Thought state never skips payload validation.
	expectGemini400(t, `{"contents":[
		{"role":"user","parts":[{"text":"hi"}]},
		{"role":"model","parts":[{"thought":true,"text":17},{"text":"ok"}]}]}`, false)
	expectGemini400(t, `{"contents":[{"parts":[{"text":"hi"}]}],
		"systemInstruction":{"parts":[{"thought":true,"text":17}]}}`, false)
	// Typed schemas validate ranges and member shapes.
	expectGemini400(t, `{"contents":[{"parts":[{"text":"hi"}]}],"tools":[{"functionDeclarations":[{"name":"f","parameters":{"type":"OBJECT","required":{}}}]}]}`, false)
	expectGemini400(t, `{"contents":[{"parts":[{"text":"hi"}]}],"tools":[{"functionDeclarations":[{"name":"f","parameters":{"type":"OBJECT","minItems":-1}}]}]}`, false)
	expectGemini400(t, `{"contents":[{"parts":[{"text":"hi"}]}],"tools":[{"functionDeclarations":[{"name":"f","parameters":{"type":"STRING","enum":[]}}]}]}`, false)
	expectGemini400(t, `{"contents":[{"parts":[{"text":"hi"}]}],"tools":[{"functionDeclarations":[{"name":"f","parameters":{"nullable":true}}]}]}`, false)
	expectGemini400(t, `{"contents":[{"parts":[{"text":"hi"}]}],"tools":[{"functionDeclarations":[{"name":"f","parameters":{"type":"OBJECT","anyOf":[{"type":"STRING"}],"nullable":true}}]}]}`, false)
	// Media members validate before any loss is recorded.
	expectGemini400(t, `{"contents":[
		{"role":"user","parts":[{"text":"hi"}]},
		{"role":"model","parts":[{"inlineData":{"bogus":1}},{"text":"ok"}]}]}`, false)
	expectGemini400(t, `{"contents":[
		{"role":"user","parts":[{"text":"hi"}]},
		{"role":"model","parts":[{"inlineData":{"mimeType":17,"data":"eA=="}},{"text":"ok"}]}]}`, false)
	// Function result parts validate element shape.
	expectGemini400(t, `{"contents":[
		{"role":"model","parts":[{"functionCall":{"name":"f","args":{}}}]},
		{"role":"user","parts":[{"functionResponse":{"name":"f","response":{},"parts":[17]}}]}],
		"tools":[{"functionDeclarations":[{"name":"f"}]}]}`, false)
	// Unknown nested members are refused in both modes.
	expectGemini400(t, `{"contents":[{"parts":[{"text":"hi","bogus":1}]}]}`, false)
}

func TestGeminiIngressOrderingDiagnostic(t *testing.T) {
	losses := mustGeminiDecodeLossy(t, `{"contents":[{"parts":[{"text":"hi"}]}],
		"tools":[{"functionDeclarations":[{"name":"f","parameters":{
			"type":"OBJECT",
			"properties":{"a":{"type":"STRING"}},
			"propertyOrdering":["a"]}}]}]}`)
	found := false
	for _, l := range losses {
		if strings.Contains(l.Field, "propertyOrdering") {
			found = true
		}
	}
	if !found {
		t.Fatalf("ordering diagnostic missing: %v", losses)
	}
	expectGemini400(t, `{"contents":[{"parts":[{"text":"hi"}]}],
		"tools":[{"functionDeclarations":[{"name":"f","parameters":{
			"type":"OBJECT","propertyOrdering":[17]}}]}]}`, false)
}
