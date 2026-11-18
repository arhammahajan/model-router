// Gemini thought-signature identity round trip.
//
// Gemini 4 requires the opaque part-level signature back on functionCall
// history parts (a missing signature is a hard upstream 400). These tests
// pin the stateless passback: lossy Gemini source decode mints a wrapped
// public call identity, every target renders it opaquely, every ingress
// decoder restores the provider identity plus the signature, and the
// Gemini destination encoder re-emits the signed part byte-exactly.
package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGeminiSignatureIDWrapUnwrap(t *testing.T) {
	w := wrapGeminiSignatureID("sig-value", "call_1")
	if w == "call_1" || !strings.HasPrefix(w, geminiSignatureIDPrefix) {
		t.Fatalf("wrap must mint a marked identity, got %q", w)
	}
	sig, inner, ok := unwrapGeminiSignatureID(w)
	if !ok || sig != "sig-value" || inner != "call_1" {
		t.Fatalf("unwrap must restore, got (%q,%q,%v)", sig, inner, ok)
	}
	// Refusal cases: empty values never wrap.
	if wrapGeminiSignatureID("", "call_1") != "call_1" {
		t.Fatalf("empty signature must not wrap")
	}
	if wrapGeminiSignatureID("s", "") != "" {
		t.Fatalf("empty identity must not wrap")
	}
	if wrapGeminiSignatureID(strings.Repeat("s", geminiSignatureMaxBytes+1), "call_1") != "call_1" {
		t.Fatalf("oversized signature must not wrap")
	}
	// Non-canonical candidates pass through as plain opaque ids.
	for _, bad := range []string{
		"call_1",
		geminiSignatureIDPrefix,
		geminiSignatureIDPrefix + "!!!!not-base64",
		geminiSignatureIDPrefix + "e30", // {}
		wrapGeminiSignatureID("s", "i") + "x",
	} {
		if _, _, ok := unwrapGeminiSignatureID(bad); ok {
			t.Fatalf("non-canonical %q must not unwrap", bad)
		}
	}
}

func geminiSignedCallResponse(sig, id string) string {
	return `{"responseId":"r","modelVersion":"gemini-4-argon","candidates":[` +
		`{"finishReason":"STOP","finishMessage":"Model generated function call(s).","index":0,` +
		`"content":{"role":"model","parts":[{"thoughtSignature":` + geminiQuote(sig) +
		`,"functionCall":{"id":` + geminiQuote(id) + `,"name":"get_weather","args":{"city":"Tokyo"}}}]}}],` +
		`"usageMetadata":{"promptTokenCount":94,"candidatesTokenCount":22,"totalTokenCount":116}}`
}

func TestGeminiLossySignedCallWrapsIdentity(t *testing.T) {
	const sig = "EowBCokBAWkUfRPi///kIqNusNnUG5T5oJtfzBTSxTtXCyvhqFqH1swcqZbsQAd7HpSyqtd9YUM"
	raw := geminiSignedCallResponse(sig, "call_3375893482179694677")
	// Strict keeps the refusal.
	if _, rerr := DecodeGeminiResponse([]byte(raw), DefaultLimits()); rerr == nil || rerr.Status != 502 {
		t.Fatalf("strict must refuse the signed call, got %v", rerr)
	}
	// Lossy: the public identity wraps signature + provider id.
	resp, rerr := DecodeGeminiResponseWithPolicy([]byte(raw), DefaultLimits(), true)
	if rerr != nil {
		t.Fatalf("lossy signed call: %v", rerr)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("one call expected, got %d", len(resp.Items))
	}
	call, ok := resp.Items[0].(ToolCallItem)
	if !ok {
		t.Fatalf("tool call expected, got %#v", resp.Items[0])
	}
	gotSig, inner, ok := unwrapGeminiSignatureID(string(call.CallID))
	if !ok || gotSig != sig || inner != "call_3375893482179694677" {
		t.Fatalf("wrapped identity must restore, got (%q,%q,%v)", gotSig, inner, ok)
	}
	// A provider-supplied identity that already looks wrapped but is not
	// canonical stays distinct: minting wraps the provider value inside.
	if string(call.CallID) == "call_3375893482179694677" {
		t.Fatalf("public identity must be the wrapped form")
	}
}

func TestGeminiLossySignedCallIdentityRoundTrip(t *testing.T) {
	const sig = "EowBCokBAWkUfRPi///kIqNusNnUG5T5oJtfzBTSxTtXCyvhqFqH1swcqZbsQAd7HpSyqtd9YUM"
	// Turn 1: Gemini response -> Chat response with the wrapped id.
	chat, rerr := TranslateGeminiJSONToChatWithPlanAndPolicy(
		[]byte(geminiSignedCallResponse(sig, "call_3375893482179694677")), "gemini-4-argon", DefaultLimits(), nil, true)
	if rerr != nil {
		t.Fatalf("turn 1 translate: %v", rerr)
	}
	var turn1 struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(chat, &turn1); err != nil {
		t.Fatalf("turn 1 chat body: %v (%s)", err, chat)
	}
	if len(turn1.Choices) != 1 || len(turn1.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("turn 1 must carry one tool call: %s", chat)
	}
	publicID := turn1.Choices[0].Message.ToolCalls[0].ID
	if _, _, ok := unwrapGeminiSignatureID(publicID); !ok {
		t.Fatalf("public id must be wrapped, got %q", publicID)
	}
	// Turn 2: the harness echoes the wrapped id in history and result.
	body := `{"model":"gemini-4-argon","messages":[` +
		`{"role":"user","content":"What is the weather in Tokyo? Use the tool."},` +
		`{"role":"assistant","content":"","tool_calls":[{"id":` + geminiQuote(publicID) +
		`,"type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Tokyo\"}"}}]},` +
		`{"role":"tool","tool_call_id":` + geminiQuote(publicID) + `,"content":"{\"temp_c\":18}"}` +
		`],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]}`
	gen, losses, rerr := DecodeChatForGemini([]byte(body), DefaultLimits())
	if rerr != nil {
		t.Fatalf("turn 2 decode: %v", rerr)
	}
	if len(losses) != 0 {
		t.Fatalf("turn 2 must be lossless, got %#v", losses)
	}
	upstream, rerr := EncodeGeminiRequestWithPlan(gen, AbsentEffort())
	if rerr != nil {
		t.Fatalf("turn 2 encode: %v", rerr)
	}
	var wire struct {
		Contents []struct {
			Role  string `json:"role"`
			Parts []struct {
				Text             string          `json:"text"`
				ThoughtSignature string          `json:"thoughtSignature"`
				FunctionCall     json.RawMessage `json:"functionCall"`
				FunctionResp     json.RawMessage `json:"functionResponse"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(upstream, &wire); err != nil {
		t.Fatalf("turn 2 upstream body: %v (%s)", err, upstream)
	}
	if len(wire.Contents) != 3 {
		t.Fatalf("turn 2 must carry three turns, got %d", len(wire.Contents))
	}
	model := wire.Contents[1]
	if model.Role != "model" || len(model.Parts) != 1 {
		t.Fatalf("model turn shape wrong: %#v", model)
	}
	if model.Parts[0].ThoughtSignature != sig {
		t.Fatalf("model turn must carry the signature, got %q", model.Parts[0].ThoughtSignature)
	}
	var fc struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(model.Parts[0].FunctionCall, &fc); err != nil || fc.ID != "call_3375893482179694677" {
		t.Fatalf("model turn must restore the provider id, got %s", model.Parts[0].FunctionCall)
	}
	var fr struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(wire.Contents[2].Parts[0].FunctionResp, &fr); err != nil || fr.ID != "call_3375893482179694677" {
		t.Fatalf("result turn must restore the provider id, got %s", wire.Contents[2].Parts[0].FunctionResp)
	}
	if wire.Contents[2].Parts[0].ThoughtSignature != "" {
		t.Fatalf("result turn must not carry a signature")
	}
}

func TestGeminiLossySignedCallStreamWraps(t *testing.T) {
	const sig = "EowBCokBAWkUfRPi///kIqNusNnUG5T5oJtfzBTSxTtXCyvhqFqH1swcqZbsQAd7HpSyqtd9YUM"
	frame := `{"candidates":[{"content":{"role":"model","parts":[{"thoughtSignature":` + geminiQuote(sig) +
		`,"functionCall":{"id":"call_9","name":"get_weather","args":{"city":"Tokyo"}}}]},` +
		`"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":2,"totalTokenCount":11}}`
	dec := NewGeminiStreamDecoderWithPolicy(DefaultLimits(), true, true)
	evs, rerr := dec.DecodeFrame(Frame{Data: frame})
	if rerr != nil {
		t.Fatalf("lossy signed stream call: %v", rerr)
	}
	wrapped := ""
	for _, ev := range evs {
		if bo, ok := ev.(BlockOpen); ok && bo.Kind == BlockTool {
			wrapped = string(bo.Call)
		}
	}
	if wrapped == "" {
		t.Fatalf("tool block missing: %#v", evs)
	}
	gotSig, inner, ok := unwrapGeminiSignatureID(wrapped)
	if !ok || gotSig != sig || inner != "call_9" {
		t.Fatalf("stream wrapped identity must restore, got (%q,%q,%v)", gotSig, inner, ok)
	}
}

func TestGeminiSignatureIdentityPassThroughUnwrapped(t *testing.T) {
	// Plain opaque identities (including lookalike prefixes) decode
	// verbatim: nothing is inferred from a prefix alone.
	body := `{"model":"m","messages":[{"role":"assistant","content":"","tool_calls":[{"id":"gts1_plain",` +
		`"type":"function","function":{"name":"f","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"gts1_plain","content":"ok"}]}`
	gen, _, rerr := DecodeChatForGemini([]byte(body), DefaultLimits())
	if rerr != nil {
		t.Fatalf("plain id decode: %v", rerr)
	}
	for _, m := range gen.Messages {
		for _, tc := range m.ToolCalls {
			if tc.ID != "gts1_plain" || tc.Signature != "" {
				t.Fatalf("plain id must pass through, got %#v", tc)
			}
		}
		for _, r := range m.Results {
			if r.CallID != "gts1_plain" {
				t.Fatalf("plain result id must pass through, got %q", r.CallID)
			}
		}
	}
}
