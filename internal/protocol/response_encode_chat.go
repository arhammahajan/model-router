// Shared target encoder: canonical Response -> Chat Completions.
//
// EncodeChatResponse renders a source-validated canonical Response as a
// Chat Completions response for the public model name, WITHOUT
// reparsing source bytes and WITHOUT calling any pair translator.
// Every source fact comes from resp; every destination difference from
// CheckRepresentable plus the target-owned framing below.
//
// Destination policy (proven by the three legacy Chat translators):
//   - Response ID is router-minted (newChatID); upstream IDs are never
//     passed through (Anthropic/Responses/Gemini translators all mint).
//     Tool-call IDs preserve the upstream call identity verbatim
//     (Anthropic tool_use id, Responses call_id, Gemini functionCall
//     id or router-minted g2c-call-N); the router never namespaces them.
//   - Text concatenates in order; refusal renders as the message
//     refusal field; tool calls render as tool_calls with stringified
//     exact arguments. Grammar-invalid names pass through (proven:
//     Chat->Gemini passes them, Gemini->Chat preserves them); the gate
//     refuses them only for Anthropic/Responses.
//   - Order: the Chat wire (one content string + one tool_calls array)
//     cannot represent text-after-tool interleaving. The gate refuses
//     it explicitly (response.items order) instead of silently
//     reordering (legacy r2cDecodeOutput/g2cDecodeContent concatenated).
//     Refusal plus tools has no Chat form (proven: r2c refuses
//     "response.output refusal with function calls").
//   - Usage: prompt/completion/total verbatim; cached share details as
//     prompt_tokens_details.cached_tokens when nonzero; scalar reasoning
//     details as completion_tokens_details.reasoning_tokens when
//     nonzero. Cache creation is summed in the prompt (Anthropic
//     creation counts into the Chat prompt) with no separate Chat
//     bucket. TextTokens is harmless-subsumed (totals carry it).
//   - Finish: stop/length/content_filter/tool_calls map directly;
//     explicit refusal maps to content_filter (proven: Anthropic
//     refusal and Gemini safety map there). A bare stop with calls
//     coerces to tool_calls (proven in all three translators).
package protocol

import (
	"encoding/json"
	"time"
)

// EncodeChatResponse renders a source-validated canonical Response as a
// Chat Completions response for the public model name.
func EncodeChatResponse(resp Response, publicModel string) ([]byte, *RouterError) {
	if rerr := CheckRepresentable(resp, DialectChat); rerr != nil {
		return nil, rerr
	}
	finish, rerr := chatResponseFinish(resp)
	if rerr != nil {
		return nil, rerr
	}
	var text, refusal string
	var calls []any
	// The representability gate has checked that content precedes calls.
	// A single pass preserves the relative order of function and custom calls.
	for _, it := range resp.Items {
		switch v := it.(type) {
		case TextItem:
			text += v.Text
		case RefusalItem:
			refusal += v.Text
		case ToolCallItem:
			calls = append(calls, map[string]any{
				"id":   string(v.CallID),
				"type": "function",
				"function": map[string]any{
					"name":      v.Name,
					"arguments": string(v.Arguments),
				},
			})
		case CustomCallItem:
			calls = append(calls, map[string]any{
				"id":   string(v.CallID),
				"type": "custom",
				"custom": map[string]any{
					"name":  v.Name,
					"input": v.Input,
				},
			})
		}
	}
	message := map[string]any{"role": "assistant", "content": text}
	if refusal != "" {
		message["refusal"] = refusal
	}
	if len(calls) > 0 {
		message["tool_calls"] = calls
	}
	usage := chatResponseUsage(resp)
	out := map[string]any{
		"id":      newChatID(false),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   publicModel,
		"choices": []any{map[string]any{
			"index": 0, "message": message, "finish_reason": finish,
		}},
		"usage": usage,
	}
	data, err := json.Marshal(out)
	if err != nil {
		return nil, &RouterError{Status: 500, Code: "encoding_error", Msg: "failed to encode downstream response"}
	}
	return data, nil
}

// chatResponseFinish maps the canonical finish onto the Chat
// finish_reason contract. A bare stop with calls coerces to tool_calls;
// explicit refusal becomes content_filter.
func chatResponseFinish(resp Response) (string, *RouterError) {
	hasCalls := false
	for _, it := range resp.Items {
		if _, ok := it.(ToolCallItem); ok {
			hasCalls = true
			break
		}
		if _, ok := it.(CustomCallItem); ok {
			hasCalls = true
			break
		}
	}
	switch resp.Finish.Kind {
	case FinishStop:
		for _, it := range resp.Items {
			if _, ok := it.(RefusalItem); ok {
				return "content_filter", nil
			}
		}
		if hasCalls {
			return "tool_calls", nil
		}
		return "stop", nil
	case FinishLength:
		return "length", nil
	case FinishContentFilter, FinishRefusal:
		return "content_filter", nil
	case FinishToolCalls:
		return "tool_calls", nil
	default:
		return "", &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stop reason has no Chat representation"}
	}
}

// chatResponseUsage renders the Chat usage contract: totals verbatim,
// cached share detailed when nonzero, scalar reasoning detailed when
// nonzero. Creation stays summed in the prompt (no Chat bucket);
// TextTokens stays subsumed in the totals.
func chatResponseUsage(resp Response) map[string]any {
	usage := map[string]any{
		"prompt_tokens":     *resp.Usage.Prompt,
		"completion_tokens": *resp.Usage.Completion,
		"total_tokens":      *resp.Usage.Total,
	}
	if resp.Usage.CacheRead != nil && *resp.Usage.CacheRead > 0 {
		usage["prompt_tokens_details"] = map[string]any{"cached_tokens": *resp.Usage.CacheRead}
	}
	if resp.Usage.Reasoning != nil && *resp.Usage.Reasoning > 0 {
		usage["completion_tokens_details"] = map[string]any{"reasoning_tokens": *resp.Usage.Reasoning}
	}
	return usage
}
