// Shared target encoder: canonical Response -> Gemini.
//
// EncodeGeminiResponse renders WITHOUT reparsing source bytes: every
// source fact comes from resp (a source-validated Response), every
// destination difference from CheckRepresentable plus the target-owned
// framing below (ordered parts, MALFORMED_FUNCTION_CALL terminal,
// usage subsets, envelope). Source-neutral: no per-source copies, no
// pair translator calls. Parts stay in canonical order (proven:
// a2gemResponseParts preserves block order, Gemini parts are ordered).
package protocol

import (
	"encoding/json"
)

// EncodeGeminiResponse renders a source-validated canonical Response as
// a GenerateContentResponse for the public model version.
func EncodeGeminiResponse(resp Response, publicModel string) ([]byte, *RouterError) {
	if rerr := CheckRepresentable(resp, DialectGemini); rerr != nil {
		return nil, rerr
	}
	finishReason, rerr := geminiResponseFinish(resp.Finish)
	if rerr != nil {
		return nil, rerr
	}
	// Omitted malformed tool calls surface as MALFORMED_FUNCTION_CALL
	// (the former Chat decoder's terminal override); valid calls still
	// render. CheckRepresentable already refused this provenance for
	// every other destination.
	if resp.MalformedTools > 0 {
		finishReason = "MALFORMED_FUNCTION_CALL"
	}
	var parts []any
	for _, it := range resp.Items {
		switch v := it.(type) {
		case TextItem:
			if v.Text != "" {
				parts = append(parts, map[string]any{"text": v.Text})
			}
		case ToolCallItem:
			call := map[string]any{"name": v.Name, "args": json.RawMessage(v.Arguments)}
			if v.CallID != "" {
				call["id"] = string(v.CallID)
			}
			parts = append(parts, map[string]any{"functionCall": call})
		case RefusalItem:
			// Unreachable: the gate refuses refusal for Gemini. Kept
			// explicit so a future gate change fails closed here
			// instead of silently dropping refusal text.
			return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream response feature \"response.choices[0].message.refusal\" has no Gemini representation"}
		default:
			return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
		}
	}
	if parts == nil {
		parts = []any{}
	}
	usage, rerr := geminiResponseUsage(resp)
	if rerr != nil {
		return nil, rerr
	}
	// Destination policy: a provider-supplied upstream response ID is
	// preserved as responseId (proven: TranslateAnthropicJSONToGemini
	// preserves the Anthropic id; Chat row preserves its id).
	// Router-minted compatibility IDs (Gemini source with absent
	// responseId, IDOrigin == router) are NEVER echoed: emitting the
	// synthesized constant as a provider responseId would fabricate
	// provider metadata. Origin is explicit, never inferred from the
	// string: a provider-supplied "gemini-compat-..." ID keeps a
	// provider origin and still echoes. Router-minted wrapper IDs live
	// in other targets (Anthropic msg_, Responses resp_/fc_); this
	// encoder never mints a Gemini responseId.
	out := map[string]any{
		"candidates": []any{map[string]any{
			"content":      map[string]any{"role": "model", "parts": parts},
			"index":        0,
			"finishReason": finishReason,
		}},
		"modelVersion": publicModel,
	}
	if resp.IDOrigin == IdentityProvider && resp.ID != "" {
		out["responseId"] = string(resp.ID)
	}
	if usage != nil {
		out["usageMetadata"] = usage
	}
	data, err := json.Marshal(out)
	if err != nil {
		return nil, &RouterError{Status: 500, Code: "encoding_error", Msg: "failed to encode downstream response"}
	}
	return data, nil
}

// geminiResponseUsage renders usageMetadata from canonical accounting.
// Canonical Prompt is the inclusive total (Chat prompt_tokens, Anthropic
// input+creation+read, Responses input_tokens, Gemini promptTokenCount),
// so promptTokenCount is verbatim and only the read share is detailed
// (proven: a2gemUsageFromScalars aggregates creation into the prompt,
// reports read only; c2gemUsage details cached only; r2gParseUsage
// details cached only).
//
// Scalar reasoning (Reasoning bucket: Chat reasoning_tokens, Responses
// reasoning_tokens) and Gemini thought accounting (Thoughts bucket:
// thoughtsTokenCount) both render as thoughtsTokenCount; the gate allows
// each only where a reviewed mapping exists, and the encoder must not
// silently drop a nonzero bucket the gate allowed. Both shares are
// subsets of completion: candidates = completion - thoughts, with
// overflow and subset checks (never wrapped or underreported).
func geminiResponseUsage(resp Response) (map[string]any, *RouterError) {
	bad := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	completion := *resp.Usage.Completion
	thoughts := 0
	if resp.Usage.Reasoning != nil {
		thoughts += *resp.Usage.Reasoning
		if thoughts < 0 {
			return nil, bad
		}
	}
	if resp.Usage.Thoughts != nil {
		thoughts += *resp.Usage.Thoughts
		if thoughts < *resp.Usage.Thoughts {
			return nil, bad
		}
	}
	if thoughts < 0 || thoughts > completion {
		return nil, bad
	}
	cached := 0
	if resp.Usage.CacheRead != nil {
		cached = *resp.Usage.CacheRead
	}
	if cached < 0 || cached > *resp.Usage.Prompt {
		return nil, bad
	}
	usage := map[string]any{
		"promptTokenCount":     *resp.Usage.Prompt,
		"candidatesTokenCount": completion - thoughts,
		"totalTokenCount":      *resp.Usage.Total,
	}
	if thoughts > 0 {
		usage["thoughtsTokenCount"] = thoughts
	}
	if cached > 0 {
		usage["cachedContentTokenCount"] = cached
	}
	return usage, nil
}

// geminiResponseFinish maps the canonical finish onto the Gemini
// finishReason contract. Only documented-equivalent stops map; refusal
// finishes (explicit or safety-family) have no Gemini counterpart and
// fail closed (proven: a2gemFinishReason refuses refusal/pause_turn).
func geminiResponseFinish(finish Finish) (string, *RouterError) {
	switch finish.Kind {
	case FinishStop, FinishToolCalls:
		return "STOP", nil
	case FinishLength:
		return "MAX_TOKENS", nil
	case FinishContentFilter:
		return "SAFETY", nil
	default:
		return "", &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stop reason has no Gemini representation"}
	}
}

// TranslateChatJSONToGemini converts one Chat Completions response
// into a GenerateContentResponse. Compatibility wrapper selecting
// (Chat source decoder, Gemini target encoder); it retains no pair
// semantics of its own.
func TranslateChatJSONToGemini(raw []byte, publicModel string, lim Limits) ([]byte, *RouterError) {
	resp, rerr := DecodeChatResponse(raw, lim)
	if rerr != nil {
		return nil, rerr
	}
	return EncodeGeminiResponse(resp, publicModel)
}

// TranslateChatJSONToGeminiWithPolicy converts one Chat response with
// narrow lossy compatibility policy: allowPrivateDrop drops substantive
// reasoning_content and the known gateway usage extensions (cost,
// opencode) at decode, false keeps the strict 502. Reasoning accounting
// still travels to Gemini natively.
func TranslateChatJSONToGeminiWithPolicy(raw []byte, publicModel string, lim Limits, allowPrivateDrop bool) ([]byte, *RouterError) {
	resp, rerr := DecodeChatResponseWithPolicy(raw, lim, allowPrivateDrop)
	if rerr != nil {
		return nil, rerr
	}
	return EncodeGeminiResponse(resp, publicModel)
}
