// Gemini row source tests: wire -> IR facts, independent of target.
//
// These tests pin DecodeGeminiResponse only: ordered canonical items,
// exact args, opaque identities, usage provenance, finish mapping and
// every 502 boundary. They never test a target encoder and never parse
// Chat JSON: the generic encoders (built in a separate unit) are the
// only consumers, and the current Chat-only encoders are not target
// truth. Cross-target outcomes are asserted only where all three old
// pair translators agreed (cited per test); the four resolved
// divergences are pinned with their rationale and executable proof.
package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func geminiIRUsage(prompt, completion int) string {
	return `{"promptTokenCount":` + g2cItoa(prompt) +
		`,"candidatesTokenCount":` + g2cItoa(completion) +
		`,"totalTokenCount":` + g2cItoa(prompt+completion) + `}`
}

func geminiIRRaw(candidates, usage, envelope string) string {
	return `{"candidates":` + candidates + `,"usageMetadata":` + usage + envelope + `}`
}

func geminiIRTextCandidate(text, finish string) string {
	return `[{"content":{"role":"model","parts":[{"text":` +
		geminiQuote(text) + `}]},"finishReason":"` + finish + `"}]`
}

func geminiQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func geminiIRDecode(t *testing.T, raw string) Response {
	t.Helper()
	resp, rerr := DecodeGeminiResponse([]byte(raw), DefaultLimits())
	if rerr != nil {
		t.Fatalf("DecodeGeminiResponse: %v", rerr)
	}
	return resp
}

func geminiIRExpect502(t *testing.T, raw string) *RouterError {
	t.Helper()
	rerr := func() *RouterError {
		_, rerr := DecodeGeminiResponse([]byte(raw), DefaultLimits())
		return rerr
	}()
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("expected 502, got %v", rerr)
	}
	return rerr
}

// --- Decoder pins source facts once ------------------------------------------

func TestGeminiIRDecoderFacts(t *testing.T) {
	raw := geminiIRRaw(
		geminiIRTextCandidate("hi", "STOP"),
		geminiIRUsage(10, 3),
		`,"modelVersion":"gemini-2.5-flash","responseId":"resp-1"`)
	resp := geminiIRDecode(t, raw)
	if resp.Source != DialectGemini {
		t.Fatalf("provenance lost: %+v", resp)
	}
	if string(resp.ID) != "resp-1" {
		t.Fatalf("provider responseId not preserved: %q", resp.ID)
	}
	if resp.IDOrigin != IdentityProvider {
		t.Fatalf("provider responseId must carry provider origin: %q", resp.IDOrigin)
	}
	if resp.Model != "gemini-2.5-flash" {
		t.Fatalf("modelVersion not preserved: %q", resp.Model)
	}
	if resp.ModelOrigin != IdentityProvider {
		t.Fatalf("provider modelVersion must carry provider origin: %q", resp.ModelOrigin)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items wrong: %#v", resp.Items)
	}
	tx, ok := resp.Items[0].(TextItem)
	if !ok || tx.Text != "hi" || string(tx.ID) != "t0" {
		t.Fatalf("text item wrong: %#v", resp.Items[0])
	}
	u := resp.Usage
	// Prompt is cache-inclusive verbatim: no target bucket math here.
	if u.Prompt == nil || *u.Prompt != 10 || u.Completion == nil || *u.Completion != 3 ||
		u.Total == nil || *u.Total != 13 {
		t.Fatalf("totals wrong: %+v", u)
	}
	if u.Provenance.Source != DialectGemini || u.Provenance.Scope != UsageTerminal {
		t.Fatalf("usage provenance wrong: %+v", u.Provenance)
	}
	if rerr := u.ValidateAuthoritative(); rerr != nil {
		t.Fatalf("decoded usage not authoritative: %v", rerr)
	}
	if resp.Finish.Kind != FinishStop || resp.Finish.Raw != "STOP" {
		t.Fatalf("finish wrong: %+v", resp.Finish)
	}
	if resp.MalformedTools != 0 {
		t.Fatalf("malformed count fabricated: %d", resp.MalformedTools)
	}
}

func TestGeminiIRToolOrderAndExactArgs(t *testing.T) {
	raw := geminiIRRaw(
		`[{"content":{"role":"model","parts":[
			{"functionCall":{"name":"late","args":{},"id":"call-9"}},
			{"text":"mid"},
			{"text":"dle"},
			{"functionCall":{"name":"get_time","args":{"n":9007199254740993}}}
		]},"finishReason":"STOP"}]`,
		geminiIRUsage(12, 7), ``)
	resp := geminiIRDecode(t, raw)
	if len(resp.Items) != 3 {
		t.Fatalf("items wrong: %#v", resp.Items)
	}
	// Tool-first preserved, not collapsed to calls-after-text.
	c0, ok := resp.Items[0].(ToolCallItem)
	if !ok || c0.Name != "late" || string(c0.CallID) != "call-9" {
		t.Fatalf("tool-first call wrong: %#v", resp.Items[0])
	}
	if string(c0.ID) != "c0" {
		t.Fatalf("call item id wrong: %#v", c0)
	}
	if string(c0.Arguments) != `{}` {
		t.Fatalf("empty args must be explicit {}: %s", c0.Arguments)
	}
	tx, ok := resp.Items[1].(TextItem)
	if !ok || tx.Text != "middle" || string(tx.ID) != "t0" {
		t.Fatalf("coalesced text wrong: %#v", resp.Items[1])
	}
	c1, ok := resp.Items[2].(ToolCallItem)
	if !ok || c1.Name != "get_time" || string(c1.ID) != "c1" {
		t.Fatalf("second call wrong: %#v", resp.Items[2])
	}
	// Provider omitted the second id: router-owned, source-scoped mint.
	if !strings.HasPrefix(string(c1.CallID), geminiCompatCallPrefix) {
		t.Fatalf("minted call id not router-scoped: %q", c1.CallID)
	}
	// Big integers survive exactly (no float64 round-trip).
	if string(c1.Arguments) != `{"n":9007199254740993}` {
		t.Fatalf("bigint args not exact: %s", c1.Arguments)
	}
	if resp.Finish.Kind != FinishToolCalls || resp.Finish.Raw != "STOP" {
		t.Fatalf("STOP with calls must be tool_calls: %+v", resp.Finish)
	}
}

func TestGeminiIRMintedIDsSkipReserved(t *testing.T) {
	raw := geminiIRRaw(
		`[{"content":{"role":"model","parts":[
			{"functionCall":{"name":"a","args":{},"id":"gemini-compat-call-0"}},
			{"functionCall":{"name":"b","args":{}}},
			{"functionCall":{"name":"c","args":{},"id":"gemini-compat-call-1"}}
		]},"finishReason":"STOP"}]`,
		geminiIRUsage(4, 1), ``)
	resp := geminiIRDecode(t, raw)
	var ids []string
	for _, it := range resp.Items {
		c, ok := it.(ToolCallItem)
		if !ok {
			t.Fatalf("expected only calls: %#v", it)
		}
		ids = append(ids, string(c.CallID))
	}
	want := []string{"gemini-compat-call-0", "gemini-compat-call-2", "gemini-compat-call-1"}
	if fmt.Sprint(ids) != fmt.Sprint(want) {
		t.Fatalf("minted ids collided or misordered: %v want %v", ids, want)
	}
}

func TestGeminiIRResponseIDAbsentMinted(t *testing.T) {
	resp := geminiIRDecode(t, geminiIRRaw(
		geminiIRTextCandidate("hi", "STOP"), geminiIRUsage(2, 1), ``))
	if string(resp.ID) != geminiCompatResponseID {
		t.Fatalf("absent responseId must mint router-owned id: %q", resp.ID)
	}
	if resp.IDOrigin != IdentityRouterMinted {
		t.Fatalf("absent responseId must carry router origin: %q", resp.IDOrigin)
	}
	if resp.Model != "" {
		t.Fatalf("absent modelVersion must leave Model empty (no fake provider string): %q", resp.Model)
	}
	if resp.ModelOrigin != IdentityRouterMinted {
		t.Fatalf("absent modelVersion must carry router origin: %q", resp.ModelOrigin)
	}
}

// --- Finish mapping ----------------------------------------------------------

func TestGeminiIRFinishMapping(t *testing.T) {
	for finish, kind := range map[string]FinishKind{
		"STOP": FinishStop, "MAX_TOKENS": FinishLength,
		"SAFETY": FinishContentFilter, "RECITATION": FinishContentFilter,
		"BLOCKLIST": FinishContentFilter, "PROHIBITED_CONTENT": FinishContentFilter,
		"SPII": FinishContentFilter, "LANGUAGE": FinishContentFilter,
		"IMAGE_SAFETY": FinishContentFilter, "IMAGE_OTHER": FinishContentFilter,
	} {
		resp := geminiIRDecode(t, geminiIRRaw(
			geminiIRTextCandidate("t", finish), geminiIRUsage(2, 1), ``))
		if resp.Finish.Kind != kind || resp.Finish.Raw != finish {
			t.Fatalf("%s -> %+v, want kind %s", finish, resp.Finish, kind)
		}
	}
	// Raw finish distinguishes the source stop from the Kind.
	resp := geminiIRDecode(t, geminiIRRaw(
		geminiIRTextCandidate("t", "MAX_TOKENS"), geminiIRUsage(2, 1), ``))
	if resp.Finish.Kind == FinishStop || resp.Finish.Raw != "MAX_TOKENS" {
		t.Fatalf("raw finish lost: %+v", resp.Finish)
	}
	// MAX_TOKENS with calls keeps the length kind (all three old paths).
	raw := geminiIRRaw(
		`[{"content":{"role":"model","parts":[{"functionCall":{"name":"f","args":{}}}]},"finishReason":"MAX_TOKENS"}]`,
		geminiIRUsage(2, 1), ``)
	if resp := geminiIRDecode(t, raw); resp.Finish.Kind != FinishLength {
		t.Fatalf("MAX_TOKENS with calls must stay length: %+v", resp.Finish)
	}
	// Unknown stops never become a Finish (all three old paths fail).
	for _, finish := range []string{"OTHER", "MALFORMED_FUNCTION_CALL",
		"FINISH_REASON_UNSPECIFIED", "STOP_CANDIDATE", "QUANTUM", ""} {
		geminiIRExpect502(t, geminiIRRaw(
			geminiIRTextCandidate("t", finish), geminiIRUsage(2, 1), ``))
	}
	// Missing or null finish means not-stopped: invalid complete response.
	geminiIRExpect502(t, geminiIRRaw(
		`[{"content":{"role":"model","parts":[{"text":"t"}]}}]`, geminiIRUsage(2, 1), ``))
	geminiIRExpect502(t, geminiIRRaw(
		`[{"content":{"role":"model","parts":[{"text":"t"}]},"finishReason":null}]`,
		geminiIRUsage(2, 1), ``))
	// A filter terminal with calls contradicts every destination contract.
	geminiIRExpect502(t, geminiIRRaw(
		`[{"content":{"role":"model","parts":[{"functionCall":{"name":"f","args":{}}}]},"finishReason":"SAFETY"}]`,
		geminiIRUsage(2, 1), ``))
}

// --- Prompt-blocked normalization --------------------------------------------

func TestGeminiIRBlocked(t *testing.T) {
	for _, reason := range []string{"SAFETY", "OTHER", "BLOCKLIST", "PROHIBITED_CONTENT", "IMAGE_SAFETY"} {
		for _, cands := range []string{`missing`, `null`, `[]`} {
			raw := `{"promptFeedback":{"blockReason":"` + reason + `"},"usageMetadata":` +
				geminiIRUsage(7, 0) + `}`
			if cands == "null" {
				raw = `{"promptFeedback":{"blockReason":"` + reason + `"},"candidates":null,"usageMetadata":` +
					geminiIRUsage(7, 0) + `}`
			} else if cands == `[]` {
				raw = `{"promptFeedback":{"blockReason":"` + reason + `"},"candidates":[],"usageMetadata":` +
					geminiIRUsage(7, 0) + `}`
			}
			resp := geminiIRDecode(t, raw)
			if resp.Finish.Kind != FinishContentFilter || resp.Finish.Raw != reason {
				t.Fatalf("%s/%s -> %+v", reason, cands, resp.Finish)
			}
			if len(resp.Items) != 0 {
				t.Fatalf("%s/%s must carry no items: %#v", reason, cands, resp.Items)
			}
			if resp.Usage.Prompt == nil || *resp.Usage.Prompt != 7 {
				t.Fatalf("%s/%s must still validate usage: %+v", reason, cands, resp.Usage)
			}
		}
		// Blocked with populated candidates contradicts the block.
		geminiIRExpect502(t, `{"promptFeedback":{"blockReason":"`+reason+`"},"candidates":`+
			geminiIRTextCandidate("x", "STOP")+`,"usageMetadata":`+geminiIRUsage(7, 1)+`}`)
	}
	// Non-blocking reasons proceed to normal candidate validation.
	for _, reason := range []string{`"BLOCK_REASON_UNSPECIFIED"`, `"BLOCK_REASON_UNKNOWN"`, `null`} {
		raw := `{"promptFeedback":{"blockReason":` + reason + `},"candidates":` +
			geminiIRTextCandidate("x", "STOP") + `,"usageMetadata":` + geminiIRUsage(7, 1) + `}`
		if resp := geminiIRDecode(t, raw); resp.Finish.Kind != FinishStop {
			t.Fatalf("%s must not block: %+v", reason, resp.Finish)
		}
	}
	// Unknown block reasons fail closed on every old path.
	geminiIRExpect502(t, `{"promptFeedback":{"blockReason":"QUOTA"},"candidates":`+
		geminiIRTextCandidate("x", "STOP")+`,"usageMetadata":`+geminiIRUsage(7, 1)+`}`)
	// Usage is still required on the blocked path.
	geminiIRExpect502(t, `{"promptFeedback":{"blockReason":"SAFETY"}}`)
	// Malformed promptFeedback fails.
	geminiIRExpect502(t, `{"promptFeedback":{"blockReason":"SAFETY","safetyRatings":{}},"usageMetadata":`+geminiIRUsage(7, 0)+`}`)
	geminiIRExpect502(t, `{"promptFeedback":{"blockReason":7},"usageMetadata":`+geminiIRUsage(7, 0)+`}`)
}

// --- Usage: inclusive prompt, exact counters ----------------------------------

func TestGeminiIRUsage(t *testing.T) {
	// Prompt already includes cache: verbatim, with the share detailed.
	raw := geminiIRRaw(geminiIRTextCandidate("t", "STOP"),
		`{"promptTokenCount":10,"candidatesTokenCount":3,"totalTokenCount":13,
			"cachedContentTokenCount":4}`, ``)
	resp := geminiIRDecode(t, raw)
	if *resp.Usage.Prompt != 10 || *resp.Usage.CacheRead != 4 {
		t.Fatalf("prompt must stay cache-inclusive with share detailed: %+v", resp.Usage)
	}
	// Explicit zeros are carried as present (zero is representable everywhere).
	raw = geminiIRRaw(geminiIRTextCandidate("t", "STOP"),
		`{"promptTokenCount":10,"candidatesTokenCount":3,"totalTokenCount":13,
			"cachedContentTokenCount":0,"thoughtsTokenCount":0,"toolUsePromptTokenCount":0}`, ``)
	resp = geminiIRDecode(t, raw)
	if resp.Usage.CacheRead == nil || *resp.Usage.CacheRead != 0 ||
		resp.Usage.Thoughts == nil || *resp.Usage.Thoughts != 0 ||
		resp.Usage.ToolUsePrompt == nil || *resp.Usage.ToolUsePrompt != 0 {
		t.Fatalf("explicit zeros must stay present: %+v", resp.Usage)
	}
	// Nonzero thoughts/tool-use fail (all three old paths: no target bucket).
	for _, k := range []string{"thoughtsTokenCount", "toolUsePromptTokenCount"} {
		geminiIRExpect502(t, geminiIRRaw(geminiIRTextCandidate("t", "STOP"),
			`{"promptTokenCount":10,"candidatesTokenCount":3,"totalTokenCount":14,`+
				`"`+k+`":1}`, ``))
	}
	// Totals exact with overflow checks; subsets bounded by their totals.
	for _, usage := range []string{
		`{"promptTokenCount":5,"candidatesTokenCount":7,"totalTokenCount":13}`,
		`{"promptTokenCount":5,"candidatesTokenCount":7,"totalTokenCount":12,"cachedContentTokenCount":6}`,
		`{"promptTokenCount":-1,"candidatesTokenCount":7,"totalTokenCount":6}`,
		`{"promptTokenCount":1.5,"candidatesTokenCount":7,"totalTokenCount":8}`,
		`{"promptTokenCount":"10","candidatesTokenCount":7,"totalTokenCount":17}`,
		`{"promptTokenCount":9223372036854775807,"candidatesTokenCount":1,"totalTokenCount":1}`,
		`{"promptTokenCount":5,"candidatesTokenCount":7}`,
		`{"promptTokenCount":5,"candidatesTokenCount":7,"totalTokenCount":null}`,
		`{"promptTokenCount":99999999999999999999999,"candidatesTokenCount":1,"totalTokenCount":1}`,
	} {
		geminiIRExpect502(t, geminiIRRaw(geminiIRTextCandidate("t", "STOP"), usage, ``))
	}
	// Usage object itself required (all three old paths).
	geminiIRExpect502(t, `{"candidates":`+geminiIRTextCandidate("t", "STOP")+`}`)
	geminiIRExpect502(t, `{"candidates":`+geminiIRTextCandidate("t", "STOP")+`,"usageMetadata":null}`)
	// Scalar detail bounds: fractions, negatives and non-numbers fail.
	for _, usage := range []string{
		`{"promptTokenCount":5,"candidatesTokenCount":7,"totalTokenCount":12,"thoughtsTokenCount":1.5}`,
		`{"promptTokenCount":5,"candidatesTokenCount":7,"totalTokenCount":12,"cachedContentTokenCount":-1}`,
		`{"promptTokenCount":5,"candidatesTokenCount":7,"totalTokenCount":12,"cachedContentTokenCount":"4"}`,
	} {
		geminiIRExpect502(t, geminiIRRaw(geminiIRTextCandidate("t", "STOP"), usage, ``))
	}
	// Detail arrays: null/absent/empty accepted; nonempty unrepresentable;
	// non-array malformed.
	okUsage := func(extra string) string {
		return `{"promptTokenCount":5,"candidatesTokenCount":7,"totalTokenCount":12` + extra + `}`
	}
	for _, extra := range []string{
		`, "promptTokensDetails":null`, `, "promptTokensDetails":[]`,
		`, "trafficType":null`,
	} {
		geminiIRDecode(t, geminiIRRaw(geminiIRTextCandidate("t", "STOP"), okUsage(extra), ``))
	}
	for _, extra := range []string{
		`, "promptTokensDetails":[{"modality":"TEXT","tokenCount":1}]`,
		`, "cacheTokensDetails":[{}]`, `, "candidatesTokensDetails":[{}]`,
		`, "toolUsePromptTokensDetails":[{}]`,
		`, "promptTokensDetails":{}`,
		`, "trafficType":""`, `, "trafficType":7`,
	} {
		geminiIRExpect502(t, geminiIRRaw(geminiIRTextCandidate("t", "STOP"), okUsage(extra), ``))
	}
	// Unknown usage members fail closed whether null or not: null
	// tolerance applies only at the envelope level (all three old paths
	// use a null-intolerant strict object for usageMetadata).
	geminiIRExpect502(t, geminiIRRaw(geminiIRTextCandidate("t", "STOP"), okUsage(`, "future":null`), ``))
	geminiIRExpect502(t, geminiIRRaw(geminiIRTextCandidate("t", "STOP"), okUsage(`, "future":1`), ``))
}

// --- Envelope, candidates, content --------------------------------------------

func TestGeminiIREnvelope(t *testing.T) {
	good := geminiIRRaw(geminiIRTextCandidate("t", "STOP"), geminiIRUsage(2, 1), ``)
	// Unknown null members are newer defaults; unknown non-null fail.
	geminiIRDecode(t, geminiIRRaw(geminiIRTextCandidate("t", "STOP"), geminiIRUsage(2, 1), `,"future":null`))
	geminiIRExpect502(t, geminiIRRaw(geminiIRTextCandidate("t", "STOP"), geminiIRUsage(2, 1), `,"future":1`))
	// Non-object envelopes fail.
	for _, raw := range []string{`[]`, `1`, `"x"`, `null`, `{oops`} {
		geminiIRExpect502(t, raw)
	}
	_ = good
	// Metadata must be nonempty strings when present.
	for _, env := range []string{`,"modelVersion":""`, `,"modelVersion":" "`,
		`,"modelVersion":7`, `,"responseId":""`, `,"responseId":"   "`,
		`,"responseId":7`, `,"createTime":""`, `,"createTime":7`} {
		geminiIRExpect502(t, geminiIRRaw(geminiIRTextCandidate("t", "STOP"), geminiIRUsage(2, 1), env))
	}
	// Non-blocked candidates: exactly one well-formed entry.
	geminiIRExpect502(t, `{"candidates":[],"usageMetadata":`+geminiIRUsage(2, 1)+`}`)
	geminiIRExpect502(t, `{"candidates":null,"usageMetadata":`+geminiIRUsage(2, 1)+`}`)
	geminiIRExpect502(t, `{"usageMetadata":`+geminiIRUsage(2, 1)+`}`)
	geminiIRExpect502(t, geminiIRRaw(
		geminiIRTextCandidate("a", "STOP")+`,`+geminiIRTextCandidate("b", "STOP"),
		geminiIRUsage(2, 1), ``))
	geminiIRExpect502(t, `{"candidates":{},"usageMetadata":`+geminiIRUsage(2, 1)+`}`)
	geminiIRExpect502(t, `{"candidates":[7],"usageMetadata":`+geminiIRUsage(2, 1)+`}`)
	// Candidate index must be 0 when present.
	geminiIRExpect502(t, geminiIRRaw(
		`[{"content":{"role":"model","parts":[{"text":"t"}]},"finishReason":"STOP","index":1}]`,
		geminiIRUsage(2, 1), ``))
	geminiIRDecode(t, geminiIRRaw(
		`[{"content":{"role":"model","parts":[{"text":"t"}]},"finishReason":"STOP","index":0}]`,
		geminiIRUsage(2, 1), ``))
	// Content role: missing or "model". The Chat alias "assistant" is
	// rejected here although the old Chat path accepted it (divergence 1).
	geminiIRExpect502(t, geminiIRRaw(
		`[{"content":{"role":"assistant","parts":[{"text":"t"}]},"finishReason":"STOP"}]`,
		geminiIRUsage(2, 1), ``))
	geminiIRExpect502(t, geminiIRRaw(
		`[{"content":{"role":"user","parts":[{"text":"t"}]},"finishReason":"STOP"}]`,
		geminiIRUsage(2, 1), ``))
	geminiIRDecode(t, geminiIRRaw(
		`[{"content":{"parts":[{"text":"t"}]},"finishReason":"STOP"}]`,
		geminiIRUsage(2, 1), ``))
	// Parts: missing/null/non-array fail; multi-payload parts fail.
	for _, cands := range []string{
		`[{"content":{"role":"model"},"finishReason":"STOP"}]`,
		`[{"content":{"role":"model","parts":null},"finishReason":"STOP"}]`,
		`[{"content":{"role":"model","parts":{}},"finishReason":"STOP"}]`,
		`[{"content":{"role":"model","parts":[{"text":"a","inlineData":{}}]},"finishReason":"STOP"}]`,
	} {
		geminiIRExpect502(t, geminiIRRaw(cands, geminiIRUsage(2, 1), ``))
	}
	// Empty parts with STOP is an empty generation, not an error.
	if resp := geminiIRDecode(t, geminiIRRaw(
		`[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}]`,
		geminiIRUsage(2, 1), ``)); len(resp.Items) != 0 {
		t.Fatalf("empty parts must decode to no items: %#v", resp.Items)
	}
	// Unknown candidate members fail (null accepted); unknown part members fail.
	geminiIRExpect502(t, geminiIRRaw(
		`[{"content":{"role":"model","parts":[{"text":"t"}]},"finishReason":"STOP","future":1}]`,
		geminiIRUsage(2, 1), ``))
	geminiIRExpect502(t, geminiIRRaw(
		`[{"content":{"role":"model","parts":[{"text":"t","future":1}]},"finishReason":"STOP"}]`,
		geminiIRUsage(2, 1), ``))
	geminiIRExpect502(t, geminiIRRaw(
		`[{"content":{"role":"model","parts":[{"text":"t"}],"future":1},"finishReason":"STOP"}]`,
		geminiIRUsage(2, 1), ``))
	// Candidate-level coherence: populated safety ratings, citations,
	// grounding, logprobs, continuation and finish messages fail;
	// documented empty defaults pass.
	for _, extra := range []string{
		`, "citationMetadata":{"x":1}`, `, "groundingMetadata":{"x":1}`,
		`, "logprobsResult":{"x":1}`, `, "tokenCountMetadata":{"x":1}`,
		`, "continuationToken":"tok"`, `, "finishMessage":"hi"`,
		`, "avgLogprobs":0.5`, `, "avgLogprobs":"x"`,
		`, "safetyRatings":[{"blocked":true}]`,
		`, "safetyRatings":[{"probability":"HIGH"}]`,
		`, "safetyRatings":[7]`,
	} {
		geminiIRExpect502(t, geminiIRRaw(
			`[{"content":{"role":"model","parts":[{"text":"t"}]},"finishReason":"STOP"`+extra+`}]`,
			geminiIRUsage(2, 1), ``))
	}
	for _, extra := range []string{
		`, "finishMessage":""`, `, "avgLogprobs":0`, `, "safetyRatings":[]`,
		`, "citationMetadata":null`, `, "continuationToken":null`,
	} {
		geminiIRDecode(t, geminiIRRaw(
			`[{"content":{"role":"model","parts":[{"text":"t"}]},"finishReason":"STOP"`+extra+`}]`,
			geminiIRUsage(2, 1), ``))
	}
}

// --- Unsupported source features fail explicitly -------------------------------

func TestGeminiIRUnsupportedParts(t *testing.T) {
	// thought:false is a no-op marker; thought:true needs a reviewed mapping.
	geminiIRDecode(t, geminiIRRaw(
		`[{"content":{"role":"model","parts":[{"thought":false},{"text":"t"}]},"finishReason":"STOP"}]`,
		geminiIRUsage(2, 1), ``))
	for _, part := range []string{
		`{"thought":true}`, `{"thought":"x"}`,
		`{"thoughtSignature":"s"}`, `{"inlineData":{"mimeType":"t","data":"eA=="}}`,
		`{"fileData":{"mimeType":"t","fileUri":"u"}}`, `{"executableCode":{"language":"p","code":"c"}}`,
		`{"codeExecutionResult":{"outcome":"o"}}`, `{"functionResponse":{"name":"f","response":{}}}`,
		`{"toolCall":{"x":1}}`, `{"toolResponse":{"x":1}}`,
		`{"mediaResolution":{"x":1}}`, `{"videoMetadata":{"x":1}}`,
		`{"partMetadata":{"x":1}}`, `{"mediaProcessing":{"x":1}}`,
		`{"text":7}`,
	} {
		geminiIRExpect502(t, geminiIRRaw(
			`[{"content":{"role":"model","parts":[`+part+`]},"finishReason":"STOP"}]`,
			geminiIRUsage(2, 1), ``))
	}
	// functionCall streaming/legacy members.
	for _, fc := range []string{
		`{"name":"f","partialArgs":[{}]}`, `{"name":"f","willContinue":true}`,
		`{"name":"f","thoughtSignature":"s"}`, `{"name":"f","args":[1]}`,
		`{"name":"f","args":"nope"}`, `{"name":"f","args":{oops}}`,
		`{"name":""}`, `{"args":{}}`, `{"name":"has space"}`,
		`{"name":"f","args":{},"id":"  "}`, `{"name":"f","args":{},"id":7}`,
		`{"name":"f","unknown":1}`,
	} {
		geminiIRExpect502(t, geminiIRRaw(
			`[{"content":{"role":"model","parts":[{"functionCall":`+fc+`}]},"finishReason":"STOP"}]`,
			geminiIRUsage(2, 1), ``))
	}
	// Reviewed no-op defaults pass: empty partialArgs, false willContinue,
	// missing/null args (explicit {}).
	for _, fc := range []string{
		`{"name":"f_1-a","partialArgs":[]}`, `{"name":"f","willContinue":false}`,
		`{"name":"f"}`, `{"name":"f","args":null}`,
	} {
		resp := geminiIRDecode(t, geminiIRRaw(
			`[{"content":{"role":"model","parts":[{"functionCall":`+fc+`}]},"finishReason":"STOP"}]`,
			geminiIRUsage(2, 1), ``))
		c, ok := resp.Items[0].(ToolCallItem)
		if !ok || string(c.Arguments) != `{}` {
			t.Fatalf("%s must default args to {}: %#v", fc, resp.Items)
		}
	}
	// Duplicate and conflicting call identities fail.
	geminiIRExpect502(t, geminiIRRaw(
		`[{"content":{"role":"model","parts":[
			{"functionCall":{"name":"a","args":{},"id":"dup"}},
			{"functionCall":{"name":"b","args":{},"id":"dup"}}]},"finishReason":"STOP"}]`,
		geminiIRUsage(2, 1), ``))
}

// --- Body-derived limits, no new text cap ---------------------------------------

func TestGeminiIRLimits(t *testing.T) {
	small := Limits{MaxArgsChars: 32, MaxStreamArgsChars: 48, MaxToolCalls: 2, MaxStreamBlocks: 8, MaxFrameBytes: 8}
	mkcall := func(args string) string {
		return `{"name":"f","args":` + args + `}`
	}
	mkraw := func(parts string) string {
		return geminiIRRaw(
			`[{"content":{"role":"model","parts":[`+parts+`]},"finishReason":"STOP"}]`,
			geminiIRUsage(2, 1), ``)
	}
	// Per-args field bound (existing one-call cap).
	if _, rerr := DecodeGeminiResponse([]byte(mkraw(mkcall(`{"a":"012345678901234567890"}`))), small); rerr == nil {
		t.Fatalf("oversize args must fail")
	}
	// Retained aggregate bound across calls (existing shared aggregate).
	two := mkcall(`{"a":"0123456789"}`) + `,` + mkcall(`{"a":"0123456789"}`)
	if _, rerr := DecodeGeminiResponse([]byte(mkraw(two)), small); rerr == nil {
		t.Fatalf("oversize aggregate must fail")
	}
	// Tool-call count bound.
	three := mkcall(`{}`) + `,` + mkcall(`{}`) + `,` + mkcall(`{}`)
	if _, rerr := DecodeGeminiResponse([]byte(mkraw(three)), small); rerr == nil {
		t.Fatalf("oversize call count must fail")
	}
	// Opaque response ids stay inside the framed identity budget.
	longID := strings.Repeat("r", DefaultLimits().MaxArgsChars+1)
	geminiIRExpect502(t, geminiIRRaw(geminiIRTextCandidate("t", "STOP"), geminiIRUsage(2, 1),
		`,"responseId":"`+longID+`"`))
	// Long text is NOT capped: no per-text-block cap exists anywhere.
	long := strings.Repeat("t", 1<<20)
	resp, rerr := DecodeGeminiResponse([]byte(geminiIRRaw(
		geminiIRTextCandidate(long, "STOP"), geminiIRUsage(2, 1), ``)), DefaultLimits())
	if rerr != nil {
		t.Fatalf("long text must pass: %v", rerr)
	}
	if resp.Items[0].(TextItem).Text != long {
		t.Fatalf("long text not preserved")
	}
}

// --- Runtime cutover: unified wrappers share one decoder ----------------------
//
// The pre-cutover divergence probes (old per-target translators disagreeing
// on role "assistant" and filter+calls) are retired: all three
// TranslateGeminiJSONTo* wrappers now select the same DecodeGeminiResponse,
// so they agree by construction. This test pins the unified runtime
// behavior instead of the old disagreement.

func TestGeminiIRDivergenceProofs(t *testing.T) {
	usage := geminiIRUsage(5, 2)
	// Unified: role "assistant" is rejected on every target (reviewed
	// contract names "model" as the only valid role).
	roleRaw := func(role string) []byte {
		return []byte(geminiIRRaw(
			`[{"content":{"role":"`+role+`","parts":[{"text":"hi"}]},"finishReason":"STOP"}]`,
			usage, ``))
	}
	for _, fn := range []func([]byte, string, Limits) ([]byte, *RouterError){
		TranslateGeminiJSONToChat, TranslateGeminiJSONToAnthropic, TranslateGeminiJSONToResponses,
	} {
		if _, rerr := fn(roleRaw("assistant"), "p", DefaultLimits()); rerr == nil {
			t.Fatalf("unified runtime must reject assistant role")
		}
	}
	if _, rerr := DecodeGeminiResponse(roleRaw("assistant"), DefaultLimits()); rerr == nil {
		t.Fatalf("decoder must follow the reviewed contract and reject assistant role")
	}
	// Unified: filter finish with calls is contradictory on every target.
	filterCalls := []byte(geminiIRRaw(
		`[{"content":{"role":"model","parts":[{"text":"t"},{"functionCall":{"name":"f","args":{}}}]},"finishReason":"SAFETY"}]`,
		usage, ``))
	for _, fn := range []func([]byte, string, Limits) ([]byte, *RouterError){
		TranslateGeminiJSONToChat, TranslateGeminiJSONToAnthropic, TranslateGeminiJSONToResponses,
	} {
		if _, rerr := fn(filterCalls, "p", DefaultLimits()); rerr == nil {
			t.Fatalf("unified runtime must refuse filter+calls as contradictory")
		}
	}
	if _, rerr := DecodeGeminiResponse(filterCalls, DefaultLimits()); rerr == nil {
		t.Fatalf("decoder must refuse filter+calls as contradictory")
	}
	// Agreement: all three map SAFETY text to a filter terminal, and the
	// decoder carries the same kind with the raw source reason.
	filterText := []byte(geminiIRRaw(geminiIRTextCandidate("t", "SAFETY"), usage, ``))
	co, _ := TranslateGeminiJSONToChat(filterText, "p", DefaultLimits())
	ao, _ := TranslateGeminiJSONToAnthropic(filterText, "p", DefaultLimits())
	ro, _ := TranslateGeminiJSONToResponses(filterText, "p", DefaultLimits())
	var cm, am, rm map[string]any
	_ = json.Unmarshal(co, &cm)
	_ = json.Unmarshal(ao, &am)
	_ = json.Unmarshal(ro, &rm)
	choices, _ := cm["choices"].([]any)
	c0, _ := choices[0].(map[string]any)
	if c0["finish_reason"] != "content_filter" {
		t.Fatalf("Chat path must map SAFETY to content_filter: %s", co)
	}
	if am["stop_reason"] != "refusal" {
		t.Fatalf("Anthropic path must map SAFETY to refusal: %s", ao)
	}
	if rm["status"] != "incomplete" {
		t.Fatalf("Responses path must map SAFETY to incomplete: %s", ro)
	}
	resp, rerr := DecodeGeminiResponse(filterText, DefaultLimits())
	if rerr != nil {
		t.Fatalf("decoder must accept filter text: %v", rerr)
	}
	if resp.Finish.Kind != FinishContentFilter || resp.Finish.Raw != "SAFETY" {
		t.Fatalf("filter kind/raw wrong: %+v", resp.Finish)
	}
}

func TestGeminiIRUnknownStopValueFree(t *testing.T) {
	value := "private-upstream-stop-value"
	err := geminiIRExpect502(t, geminiIRRaw(geminiIRTextCandidate("hi", value), geminiIRUsage(1, 1), ""))
	if strings.Contains(err.Msg, value) {
		t.Fatal("unknown stop value leaked into diagnostic")
	}
}
