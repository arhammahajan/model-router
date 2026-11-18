package protocol

import (
	"strings"
	"testing"
)

func TestGeminiIngressIdentityCoherence(t *testing.T) {
	// Explicit call and result identities must cohere on both id and name.
	expectGemini400(t, `{"contents":[
		{"role":"model","parts":[{"functionCall":{"name":"f","args":{},"id":"call_1"}}]},
		{"role":"user","parts":[{"functionResponse":{"name":"g","response":{},"id":"call_1"}}]}],
		"tools":[{"functionDeclarations":[{"name":"f"},{"name":"g"}]}]}`, false)
	// Unmatched explicit result identity is refused.
	expectGemini400(t, `{"contents":[
		{"role":"model","parts":[{"functionCall":{"name":"f","args":{},"id":"call_1"}}]},
		{"role":"user","parts":[{"functionResponse":{"name":"f","response":{},"id":"call_9"}}]}],
		"tools":[{"functionDeclarations":[{"name":"f"}]}]}`, false)
	// Explicit empty and whitespace-only ids are source errors.
	expectGemini400(t, `{"contents":[
		{"role":"model","parts":[{"functionCall":{"name":"f","args":{},"id":""}}]}],
		"tools":[{"functionDeclarations":[{"name":"f"}]}]}`, false)
	expectGemini400(t, `{"contents":[
		{"role":"model","parts":[{"functionCall":{"name":"f","args":{}}}]},
		{"role":"user","parts":[{"functionResponse":{"name":"f","response":{},"id":"   "}}]}],
		"tools":[{"functionDeclarations":[{"name":"f"}]}]}`, false)
}

func TestGeminiIngressOpaqueIdentityPreserved(t *testing.T) {
	// Opaque ids survive exactly with no diagnostic.
	g := mustGeminiDecode(t, `{"contents":[
		{"role":"model","parts":[{"functionCall":{"name":"f","args":{},"id":"has:colon"}}]},
		{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"x":1},"id":"has:colon"}}]}],
		"tools":[{"functionDeclarations":[{"name":"f"}]}]}`, false)
	if g.Messages[0].ToolCalls[0].ID != "has:colon" {
		t.Fatalf("opaque call id not preserved: %q", g.Messages[0].ToolCalls[0].ID)
	}
	// Minted ids avoid every supplied id, including later ones.
	g2 := mustGeminiDecode(t, `{"contents":[
		{"role":"model","parts":[
			{"functionCall":{"name":"f","args":{}}},
			{"functionCall":{"name":"g","args":{},"id":"gdec-1"}}]},
		{"role":"user","parts":[
			{"functionResponse":{"name":"f","response":{}}},
			{"functionResponse":{"name":"g","response":{},"id":"gdec-1"}}]}],
		"tools":[{"functionDeclarations":[{"name":"f"},{"name":"g"}]}]}`, false)
	if g2.Messages[0].ToolCalls[1].ID != "gdec-1" {
		t.Fatalf("supplied id not preserved: %+v", g2.Messages[0].ToolCalls)
	}
	if g2.Messages[0].ToolCalls[0].ID == "gdec-1" {
		t.Fatalf("minted id collided with supplied id")
	}
	if !strings.HasPrefix(g2.Messages[0].ToolCalls[0].ID, "gdec-") {
		t.Fatalf("minted id not scoped: %q", g2.Messages[0].ToolCalls[0].ID)
	}
}
