// Shared target encoder: canonical Response -> Anthropic Messages.
//
// EncodeAnthropicResponse renders WITHOUT reparsing source bytes: every
// source fact comes from resp (a source-validated Response), every
// destination difference from CheckRepresentable plus the target-owned
// framing below. Source-neutral: Chat, Anthropic, Responses and Gemini
// rows all encode through this function; no per-source copies exist and
// no old pair translator is called.
//
// Ordering: Anthropic preserves ordered blocks one-to-one (proven:
// a2rDecodeContent keeps tool-first order; r2a trailing bucket keeps
// order). Non-Chat sources encode items in order, one block per item
// (refusal becomes a text block: Anthropic has no refusal block type).
// The Chat source keeps the proven fold: refusal wins over text into a
// single leading text block, then tool_use blocks (former Chat decoder
// behavior, pinned by the Chat row tests).
package protocol

import (
	"encoding/json"
)

// EncodeAnthropicResponse renders a source-validated canonical Response
// as an Anthropic Messages response for the public model name.
func EncodeAnthropicResponse(resp Response, publicModel string) ([]byte, *RouterError) {
	return EncodeAnthropicResponseWithPolicy(resp, publicModel, false)
}

// EncodeAnthropicResponseWithPolicy renders a source-validated Response
// as Anthropic Messages with narrow lossy policy:
// allowChatReasoningDrop drops the Chat-source scalar reasoning breakdown
// for Anthropic, preserving aggregate output_tokens (output_tokens stays
// completion). Responses/Gemini reasoning and strict Anthropic are
// unchanged; only source==Chat with dest==Anthropic is affected.
func EncodeAnthropicResponseWithPolicy(resp Response, publicModel string, allowChatReasoningDrop bool) ([]byte, *RouterError) {
	if rerr := CheckRepresentableWithPolicy(resp, DialectAnthropic, allowChatReasoningDrop); rerr != nil {
		return nil, rerr
	}
	stop, rerr := anthropicResponseStop(resp)
	if rerr != nil {
		return nil, rerr
	}
	content, rerr := anthropicResponseContent(resp)
	if rerr != nil {
		return nil, rerr
	}
	usage, rerr := encodeAnthropicUsage(resp.Usage)
	if rerr != nil {
		return nil, rerr
	}
	out := map[string]any{
		"id": newAnthropicMessageID(), "type": "message", "role": "assistant",
		"model": publicModel, "content": content,
		"stop_reason": stop, "stop_sequence": nil, "usage": usage,
	}
	data, err := json.Marshal(out)
	if err != nil {
		return nil, &RouterError{Status: 500, Code: "encoding_error", Msg: "failed to encode downstream response"}
	}
	return data, nil
}

// anthropicResponseContent renders ordered content. Chat source keeps the
// refusal-wins fold; every other source preserves block order one-to-one.
func anthropicResponseContent(resp Response) ([]any, *RouterError) {
	if resp.Source == DialectChat {
		var texts, refusals []string
		var calls []ToolCallItem
		for _, it := range resp.Items {
			switch v := it.(type) {
			case TextItem:
				texts = append(texts, v.Text)
			case RefusalItem:
				refusals = append(refusals, v.Text)
			case ToolCallItem:
				calls = append(calls, v)
			default:
				return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
			}
		}
		text := joinTexts(texts)
		if len(refusals) > 0 {
			text = joinTexts(refusals)
		}
		var content []any
		if text != "" || len(calls) == 0 {
			content = append(content, map[string]any{"type": "text", "text": text})
		}
		for _, c := range calls {
			content = append(content, map[string]any{
				"type": "tool_use", "id": string(c.CallID), "name": c.Name,
				"input": json.RawMessage(c.Arguments),
			})
		}
		if len(content) == 0 {
			content = []any{map[string]any{"type": "text", "text": ""}}
		}
		return content, nil
	}
	content := make([]any, 0, len(resp.Items))
	for _, it := range resp.Items {
		switch v := it.(type) {
		case TextItem:
			content = append(content, map[string]any{"type": "text", "text": v.Text})
		case RefusalItem:
			content = append(content, map[string]any{"type": "text", "text": v.Text})
		case ToolCallItem:
			content = append(content, map[string]any{
				"type": "tool_use", "id": string(v.CallID), "name": v.Name,
				"input": json.RawMessage(v.Arguments),
			})
		default:
			return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
		}
	}
	if len(content) == 0 {
		content = []any{map[string]any{"type": "text", "text": ""}}
	}
	return content, nil
}

// anthropicResponseUsage renders the Anthropic usage contract from the
// canonical inclusive Prompt: input_tokens excludes BOTH cache shares
// and reports them separately (proven: r2aParseUsage returns
// input-cacheRead-cacheWrite and emits both buckets;
// anthropic_to_chat.go prompt() sums input+creation+read). Creation is
// emitted where representable (Responses and Anthropic sources per the
// gate); Chat creation is refused by the gate so this path only sees
// zero there.
func encodeAnthropicUsage(u ResponseUsage) (map[string]any, *RouterError) {
	bad := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	prompt, completion := *u.Prompt, *u.Completion
	creation, cached := 0, 0
	if u.CacheCreation != nil {
		creation = *u.CacheCreation
	}
	if u.CacheRead != nil {
		cached = *u.CacheRead
	}
	sum := creation + cached
	if sum < creation || sum < cached || sum > prompt {
		return nil, bad
	}
	usage := map[string]any{"input_tokens": prompt - sum, "output_tokens": completion}
	if creation > 0 {
		usage["cache_creation_input_tokens"] = creation
	}
	if cached > 0 {
		usage["cache_read_input_tokens"] = cached
	}
	return usage, nil
}

// anthropicResponseStop maps the canonical finish onto the Anthropic
// stop reason, preserving the former decoder's coercion: tool calls
// returned under a bare stop still report tool_use. Content-filter and
// explicit refusal both report refusal (proven: chatFinishReason and
// a2rResponsesStatus map refusal to the filter/refusal terminal).
func anthropicResponseStop(resp Response) (string, *RouterError) {
	hasCalls := false
	for _, it := range resp.Items {
		if _, ok := it.(ToolCallItem); ok {
			hasCalls = true
			break
		}
	}
	switch resp.Finish.Kind {
	case FinishStop:
		if hasCalls {
			return "tool_use", nil
		}
		return "end_turn", nil
	case FinishLength:
		return "max_tokens", nil
	case FinishToolCalls:
		return "tool_use", nil
	case FinishContentFilter, FinishRefusal:
		return "refusal", nil
	default:
		return "", &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stop reason has no Anthropic representation"}
	}
}

// joinTexts concatenates ordered text spans. Decoded generations carry
// at most one span per kind; concatenation keeps hand-built multi-span
// Responses total without inventing separators.
func joinTexts(parts []string) string {
	out := ""
	for _, p := range parts {
		out += p
	}
	return out
}

// TranslateChatJSONToAnthropic converts one Chat Completions response
// into an Anthropic Messages response. Compatibility wrapper selecting
// (Chat source decoder, Anthropic target encoder); it retains no pair
// semantics of its own.
func TranslateChatJSONToAnthropic(raw []byte, publicModel string, lim Limits) ([]byte, *RouterError) {
	resp, rerr := DecodeChatResponse(raw, lim)
	if rerr != nil {
		return nil, rerr
	}
	return EncodeAnthropicResponse(resp, publicModel)
}

// TranslateChatJSONToAnthropicWithPolicy converts one Chat response
// with narrow lossy compatibility policy: allowPrivateDrop drops
// substantive reasoning_content at decode, drops the known gateway usage
// extensions (cost, opencode) after validation, and projects the
// Chat-source reasoning breakdown for Anthropic (aggregate output_tokens
// preserved). False keeps the strict 502s.
func TranslateChatJSONToAnthropicWithPolicy(raw []byte, publicModel string, lim Limits, allowPrivateDrop bool) ([]byte, *RouterError) {
	resp, rerr := DecodeChatResponseWithPolicy(raw, lim, allowPrivateDrop)
	if rerr != nil {
		return nil, rerr
	}
	return EncodeAnthropicResponseWithPolicy(resp, publicModel, allowPrivateDrop)
}
