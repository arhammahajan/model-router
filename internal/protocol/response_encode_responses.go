// Shared target encoder: canonical Response -> Responses API.
//
// EncodeResponsesResponse renders WITHOUT reparsing source bytes: every
// source fact comes from resp (a source-validated Response), every
// destination difference from CheckRepresentable plus the target-owned
// framing below (message/refusal/function_call items, detail breakout,
// terminal envelope). Source-neutral: no per-source copies, no pair
// translator calls.
//
// Ordering: Responses preserves ordered blocks one-to-one (proven:
// a2rDecodeContent emits one output item per Anthropic block, keeping
// tool-first order; r2a trailing bucket keeps order). Non-Chat sources
// encode items in order (each text/refusal becomes its own message
// item; each call its own function_call). The Chat source keeps the
// proven collapse: one message item carrying concatenated text plus
// refusal parts, then function_call items (former Chat decoder shape).
package protocol

// EncodeResponsesResponse renders a source-validated canonical Response
// as a Responses API response object for the public model name.
func EncodeResponsesResponse(resp Response, publicModel string) ([]byte, *RouterError) {
	return encodeResponsesResponse(resp, publicModel, nil)
}

type ResponsesSearchTrace struct {
	Query          string
	Results        []SearchResultView
	IncludeSources bool
}

// EncodeResponsesSearchResponse renders completed search activity alongside
// the ordinary Chat-derived answer. Citation spans refer to the rendered text.
func EncodeResponsesSearchResponse(resp Response, publicModel string, trace ResponsesSearchTrace) ([]byte, *RouterError) {
	return encodeResponsesResponse(resp, publicModel, &trace)
}

func encodeResponsesResponse(resp Response, publicModel string, trace *ResponsesSearchTrace) ([]byte, *RouterError) {
	if rerr := CheckRepresentable(resp, DialectResponses); rerr != nil {
		return nil, rerr
	}
	status, incompleteReason, rerr := responsesResponseStatus(resp.Finish)
	if rerr != nil {
		return nil, rerr
	}
	itemStatus := "completed"
	if status == "incomplete" {
		itemStatus = "incomplete"
	}
	output, rerr := responsesResponseOutput(resp, itemStatus, trace)
	if rerr != nil {
		return nil, rerr
	}
	if trace != nil {
		output = append([]any{renderWebSearchCall(newResponsesID("ws"), "completed", *trace)}, output...)
	}
	usage := responsesResponseUsage(resp)
	data, rerr := renderResponsesResponse(publicModel, responsesNowUnix(), status, incompleteReason, output, usage)
	if rerr != nil {
		return nil, rerr
	}
	return data, nil
}

// responsesResponseOutput renders ordered output. Chat source keeps the
// single-message collapse; other sources preserve block order.
func responsesResponseOutput(resp Response, itemStatus string, trace *ResponsesSearchTrace) ([]any, *RouterError) {
	if resp.Source == DialectChat {
		var texts, refusals []string
		var calls []any
		for _, it := range resp.Items {
			switch v := it.(type) {
			case TextItem:
				texts = append(texts, v.Text)
			case RefusalItem:
				refusals = append(refusals, v.Text)
			case ToolCallItem:
				calls = append(calls, responsesFunctionOutput(v, itemStatus))
			case CustomCallItem:
				calls = append(calls, responsesCustomOutput(v, itemStatus))
			default:
				return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
			}
		}
		output := make([]any, 0, 1+len(calls))
		var content []any
		joinedText := joinTexts(texts)
		if joinedText != "" {
			var annotations any = []any{}
			if trace != nil {
				cited, rerr := buildSearchAnnotations(joinedText, trace.Results)
				if rerr != nil {
					return nil, rerr
				}
				if len(cited) != 0 {
					annotations = cited
				}
			}
			content = append(content, map[string]any{
				"type": "output_text", "text": joinedText,
				"annotations": annotations, "logprobs": []any{},
			})
		}
		for _, r := range refusals {
			if r == "" {
				continue
			}
			content = append(content, map[string]any{"type": "refusal", "refusal": r})
		}
		if len(content) > 0 || len(calls) == 0 {
			if content == nil {
				content = []any{}
			}
			output = append(output, map[string]any{
				"type": "message", "id": newResponsesID("msg"),
				"status": itemStatus, "role": "assistant", "content": content,
			})
		}
		return append(output, calls...), nil
	}
	output := make([]any, 0, len(resp.Items))
	for _, it := range resp.Items {
		switch v := it.(type) {
		case TextItem:
			var content []any
			if v.Text != "" {
				content = append(content, map[string]any{
					"type": "output_text", "text": v.Text,
					"annotations": []any{}, "logprobs": []any{},
				})
			} else {
				content = []any{}
			}
			output = append(output, map[string]any{
				"type": "message", "id": newResponsesID("msg"),
				"status": itemStatus, "role": "assistant", "content": content,
			})
		case RefusalItem:
			if v.Text == "" {
				continue
			}
			output = append(output, map[string]any{
				"type": "message", "id": newResponsesID("msg"),
				"status": itemStatus, "role": "assistant",
				"content": []any{map[string]any{"type": "refusal", "refusal": v.Text}},
			})
		case ToolCallItem:
			output = append(output, responsesFunctionOutput(v, itemStatus))
		case CustomCallItem:
			output = append(output, responsesCustomOutput(v, itemStatus))
		default:
			return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
		}
	}
	if len(output) == 0 {
		output = append(output, map[string]any{
			"type": "message", "id": newResponsesID("msg"),
			"status": itemStatus, "role": "assistant", "content": []any{},
		})
	}
	return output, nil
}

func responsesFunctionOutput(call ToolCallItem, status string) map[string]any {
	item := map[string]any{
		"type": "function_call", "id": newResponsesID("fc"),
		"call_id": string(call.CallID), "name": call.Name,
		"arguments": string(call.Arguments), "status": status,
	}
	if call.Namespace != "" {
		item["namespace"] = call.Namespace
	}
	return item
}

func responsesCustomOutput(call CustomCallItem, status string) map[string]any {
	item := map[string]any{
		"type": "custom_tool_call", "id": newResponsesID("ctc"),
		"call_id": string(call.CallID), "name": call.Name,
		"input": call.Input, "status": status,
	}
	if call.Namespace != "" {
		item["namespace"] = call.Namespace
	}
	return item
}

// responsesResponseStatus maps the canonical finish onto the Responses
// status contract: length and content_filter truncate to incomplete
// with their reviewed reason; stop and tool_calls complete. Explicit
// refusal also truncates to content_filter (proven: a2rResponsesStatus
// maps Anthropic refusal to incomplete/content_filter; Chat refusal
// travels as content, never as a finish, so this arm only fires for
// non-Chat sources).
func responsesResponseStatus(finish Finish) (status, incompleteReason string, rerr *RouterError) {
	switch finish.Kind {
	case FinishStop, FinishToolCalls:
		return "completed", "", nil
	case FinishLength:
		return "incomplete", "max_output_tokens", nil
	case FinishContentFilter, FinishRefusal:
		return "incomplete", "content_filter", nil
	default:
		return "", "", &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stop reason has no Responses representation"}
	}
}

// responsesResponseUsage renders the Responses usage contract: unlike
// the Anthropic bridge, input_tokens keeps the full prompt total and
// cached shares break out in input_tokens_details; reasoning has a
// native bucket. Details always carry both keys (zero when absent),
// matching the former decoder exactly.
func responsesResponseUsage(resp Response) map[string]any {
	prompt, completion, total := *resp.Usage.Prompt, *resp.Usage.Completion, *resp.Usage.Total
	cached, cacheWrite, reasoning := 0, 0, 0
	if resp.Usage.CacheRead != nil {
		cached = *resp.Usage.CacheRead
	}
	if resp.Usage.CacheCreation != nil {
		cacheWrite = *resp.Usage.CacheCreation
	}
	if resp.Usage.Reasoning != nil {
		reasoning = *resp.Usage.Reasoning
	}
	return map[string]any{
		"input_tokens": prompt,
		"input_tokens_details": map[string]any{
			"cached_tokens": cached, "cache_write_tokens": cacheWrite,
		},
		"output_tokens":         completion,
		"output_tokens_details": map[string]any{"reasoning_tokens": reasoning},
		"total_tokens":          total,
	}
}

// TranslateChatJSONToResponses converts one Chat Completions response
// into a Responses API response object. Compatibility wrapper selecting
// (Chat source decoder, Responses target encoder); it retains no pair
// semantics of its own.
func TranslateChatJSONToResponses(raw []byte, publicModel string, lim Limits) ([]byte, *RouterError) {
	resp, rerr := DecodeChatResponse(raw, lim)
	if rerr != nil {
		return nil, rerr
	}
	return EncodeResponsesResponse(resp, publicModel)
}

// TranslateChatJSONToResponsesWithPolicy converts one Chat response
// with narrow lossy compatibility policy: allowPrivateDrop drops
// substantive reasoning_content and the known gateway usage extensions
// (cost, opencode) at decode, false keeps the strict 502. Reasoning
// accounting still travels to Responses natively.
func TranslateChatJSONToResponsesWithPolicy(raw []byte, publicModel string, lim Limits, allowPrivateDrop bool) ([]byte, *RouterError) {
	resp, rerr := DecodeChatResponseWithPolicy(raw, lim, allowPrivateDrop)
	if rerr != nil {
		return nil, rerr
	}
	return EncodeResponsesResponse(resp, publicModel)
}
