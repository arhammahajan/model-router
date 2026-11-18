package protocol

// Chat -> Responses native custom response guard: shared semantic boundary
// threading the Chat request ToolPlan into both the complete-JSON and
// streaming Responses -> Chat response paths.
//
// Direct upstream native contract: the typed source decoders stay generic
// (they decode function_call vs custom_tool_call by wire family only).
// This stage validates AFTER decode and BEFORE encode:
//
//   - preserve native CustomCallItem/BlockCustom when the family matches
//     the request plan;
//   - reject a declared custom returned as a function_call (wrong family);
//   - reject a custom_tool_call whose name was never declared custom in
//     the request plan (hallucinated custom);
//   - ordinary unknown function names pass through untouched.
//
// Compacted JSON arguments are never reinterpreted as raw custom input:
// whitespace and non-JSON custom payloads cannot survive a JSON
// round-trip, so family mismatches fail closed instead of being
// repaired (deliberate difference from the Python LiteLLM facade).

// CheckResponsesFamilyForChat validates one decoded Responses response
// against the Chat request plan for the Chat destination. It is the
// shared semantic check used by both the JSON and stream paths.
func (p *ToolPlan) CheckResponsesFamilyForChat(resp Response) *RouterError {
	bad := func() *RouterError {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream response feature \"response.choices[0].message.tool_calls\" has no Chat representation"}
	}
	for _, it := range resp.Items {
		switch v := it.(type) {
		case ToolCallItem:
			if p.IsNativeCustom(v.Name) {
				return bad()
			}
		case CustomCallItem:
			if !p.IsNativeCustom(v.Name) {
				return bad()
			}
		}
	}
	return nil
}

// TranslateResponsesJSONToChatWithPlan converts one Responses response
// into Chat Completions, enforcing the Chat request plan's native custom
// family contract after decode and before encode. A nil plan declares
// no custom tools.
func TranslateResponsesJSONToChatWithPlan(raw []byte, publicModel string, lim Limits, plan *ToolPlan) ([]byte, *RouterError) {
	return TranslateResponsesJSONToChatWithPlanAndPolicy(raw, publicModel, lim, plan, false)
}

func TranslateResponsesJSONToChatWithPlanAndPolicy(raw []byte, publicModel string, lim Limits, plan *ToolPlan, allowPrivateDrop bool) ([]byte, *RouterError) {
	resp, rerr := DecodeResponsesResponseWithPolicy(raw, lim, allowPrivateDrop)
	if rerr != nil {
		return nil, rerr
	}
	if rerr := plan.CheckResponsesFamilyForChat(resp); rerr != nil {
		return nil, rerr
	}
	return EncodeChatResponse(resp, publicModel)
}

// chatCustomGuardDecoder is the shared stream stage rewriting nothing:
// it validates BlockOpen family against the request plan. Decoders stay
// generic; this wrapper owns the plan check.
type chatCustomGuardDecoder struct {
	inner StreamDecoder
	plan  *ToolPlan
}

// WrapResponsesForChat returns a StreamDecoder that validates Responses
// BlockOpen family against the Chat request plan. Close passes through
// without extra reads.
func (p *ToolPlan) WrapResponsesForChat(inner StreamDecoder) StreamDecoder {
	if inner == nil {
		return inner
	}
	return &chatCustomGuardDecoder{inner: inner, plan: p}
}

// DecodeFrame decodes one frame then validates tool block identities.
func (d *chatCustomGuardDecoder) DecodeFrame(frame Frame) ([]ResponseEvent, *RouterError) {
	events, rerr := d.inner.DecodeFrame(frame)
	if rerr != nil {
		return nil, rerr
	}
	return d.plan.checkGuardEvents(events)
}

// Close validates terminal state without extra reads.
func (d *chatCustomGuardDecoder) Close() ([]ResponseEvent, *RouterError) {
	events, rerr := d.inner.Close()
	if rerr != nil {
		return nil, rerr
	}
	return d.plan.checkGuardEvents(events)
}

// checkGuardEvents enforces the native family contract on BlockOpen
// events: declared customs must arrive as BlockCustom, and BlockCustom
// names must have been declared.
func (p *ToolPlan) checkGuardEvents(events []ResponseEvent) ([]ResponseEvent, *RouterError) {
	bad := &RouterError{Status: 502, Code: CodeUpstreamBad,
		Msg: "Upstream response feature \"response.choices[0].message.tool_calls\" has no Chat representation"}
	for _, ev := range events {
		open, ok := ev.(BlockOpen)
		if !ok {
			continue
		}
		switch open.Kind {
		case BlockTool:
			if p.IsNativeCustom(open.Name) {
				return nil, bad
			}
		case BlockCustom:
			if !p.IsNativeCustom(open.Name) {
				return nil, bad
			}
		}
	}
	return events, nil
}
