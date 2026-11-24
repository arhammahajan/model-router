// Chat source codec: one complete-response parser for the Chat row.
//
// DecodeChatResponse validates a Chat Completions response ONCE,
// independently of destination, and returns the canonical Response.
// The three target encoders (EncodeAnthropicResponse,
// EncodeResponsesResponse, EncodeGeminiResponse) then render WITHOUT
// reparsing the original Chat bytes; destination differences live in
// CheckRepresentable plus target-owned framing, never in a second
// parse of the source.
//
// Provenance: this decoder replaces the source-validation halves of
// TranslateChatJSONToAnthropic (decodeChatResponseMessage +
// parseChatUsage), TranslateChatJSONToGemini (c2gemResponseParts +
// c2gemUsage) and TranslateChatJSONToResponses
// (decodeChatResponseMessageForResponses + parseChatUsageForResponses).
// Every accept/refuse below cites the behavior those three agreed on;
// every intentional divergence is carried as typed IR provenance
// (RefusalItem, MalformedTools, TextTokens) for CheckRepresentable
// instead of being decided here.
package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
)

// chatUsageBuckets is the source-neutral Chat accounting parse shared
// by the complete decoder and the three Chat stream translators. The
// per-target *Usage helpers in the pair files keep only their target
// rendering (input subtraction, detail breakout, usageMetadata) over
// these buckets; all envelope/allowlists/subset policy lives here, so
// the three former copies cannot drift.
type chatUsageBuckets struct {
	prompt, completion, total int
	cached, cacheWrite        *int
	reasoning                 *int
	// textPrompt/textCompletion carry nonzero text_tokens shares per
	// detail section (nil when absent or zero). The decoder sums them
	// into ResponseUsage.TextTokens; target wrappers refuse each
	// nonzero share with its own wire path.
	textPrompt, textCompletion *int
}

// DecodeChatResponse parses one Chat Completions complete response
// into the canonical IR. Source validation is destination-independent:
// unknown members, malformed envelopes, bad totals and universally
// unmapped nonzero accounting fail closed here for every target.
// Known target-divergent features (refusal text, malformed tool calls,
// text accounting) are carried as IR provenance for CheckRepresentable.
//
// Strict default: substantive provider-private reasoning_content fails
// closed with a value-free path; absent/null/empty decodes exactly.
// Use DecodeChatResponseWithPolicy with allowPrivateDrop for the lossy
// compatibility drop (documented in response_chat_reasoning.go).
func DecodeChatResponse(raw []byte, lim Limits) (Response, *RouterError) {
	return decodeChatResponse(raw, lim, false)
}

// DecodeChatResponseWithPolicy parses one Chat complete response with
// narrow lossy compatibility policy: allowPrivateDrop drops substantive
// reasoning_content and, when true, also drops the known gateway usage
// extensions (usage.cost, usage.opencode with its closed billing/tokens
// shape) after bounded validation. False keeps the strict 502 for both.
// Absent/null/empty is exact in both modes.
func DecodeChatResponseWithPolicy(raw []byte, lim Limits, allowPrivateDrop bool) (Response, *RouterError) {
	return decodeChatResponse(raw, lim, allowPrivateDrop)
}

func decodeChatResponse(raw []byte, lim Limits, allowPrivateDrop bool) (Response, *RouterError) {
	bad := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	obj, rerr := chatResponseObject(raw, map[string]bool{
		"id": true, "object": true, "created": true, "model": true,
		"choices": true, "usage": true, "system_fingerprint": true,
		"service_tier": true,
	}, "response")
	if rerr != nil {
		return Response{}, rerr
	}
	var id, model string
	if rawID, ok := obj["id"]; !ok || isNull(rawID) || json.Unmarshal(rawID, &id) != nil || !validOpaqueID(id) {
		return Response{}, bad
	}
	if len(id) > lim.MaxArgsChars {
		return Response{}, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream response exceeds the size bound"}
	}
	if rawModel, ok := obj["model"]; !ok || isNull(rawModel) || json.Unmarshal(rawModel, &model) != nil || model == "" {
		return Response{}, bad
	}
	if rawObj, ok := obj["object"]; ok && !isNull(rawObj) {
		var o string
		if err := json.Unmarshal(rawObj, &o); err != nil || o != "chat.completion" {
			return Response{}, bad
		}
	}
	if rawCreated, ok := obj["created"]; ok && !isNull(rawCreated) {
		if _, rerr := responseInt(rawCreated); rerr != nil {
			return Response{}, bad
		}
	}
	for _, k := range []string{"system_fingerprint", "service_tier"} {
		if rawV, ok := obj[k]; ok && !isNull(rawV) {
			var s string
			if err := json.Unmarshal(rawV, &s); err != nil {
				return Response{}, bad
			}
		}
	}
	rawChoices, ok := obj["choices"]
	if !ok || isNull(rawChoices) {
		return Response{}, bad
	}
	var choices []json.RawMessage
	if err := json.Unmarshal(rawChoices, &choices); err != nil || len(choices) != 1 {
		// Multiple (or zero) choices have no single-generation form on
		// any target; all three former decoders failed these closed.
		return Response{}, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned multiple choices with no representation"}
	}
	text, refusal, calls, malformed, rerr := decodeChatAssistantMessageWithPolicy(choices[0], lim, allowPrivateDrop)
	if rerr != nil {
		return Response{}, rerr
	}
	finish, rerr := decodeChatFinish(choices[0])
	if rerr != nil {
		return Response{}, rerr
	}
	rawUsage, ok := obj["usage"]
	if !ok || isNull(rawUsage) {
		return Response{}, bad
	}
	buckets, rerr := parseChatUsageBucketsWithPolicy(rawUsage, allowPrivateDrop)
	if rerr != nil {
		return Response{}, rerr
	}
	usage, rerr := NewTerminalUsage(DialectChat, buckets.prompt, buckets.completion, buckets.total)
	if rerr != nil {
		return Response{}, rerr
	}
	usage.CacheCreation, usage.CacheRead = buckets.cacheWrite, buckets.cached
	usage.Reasoning = buckets.reasoning
	if text, rerr := chatTextTotal(buckets); rerr != nil {
		return Response{}, rerr
	} else {
		usage.TextTokens = text
	}
	items := make([]ResponseItem, 0, 2+len(calls))
	if text != "" {
		items = append(items, TextItem{ID: "t0", Text: text})
	}
	if refusal != "" {
		items = append(items, RefusalItem{ID: "r0", Text: refusal})
	}
	items = append(items, calls...)
	resp, rerr := NewResponse(ResponseID(id), model, DialectChat, items, usage, finish)
	if rerr != nil {
		return Response{}, rerr
	}
	resp.MalformedTools = malformed
	return resp, nil
}

// chatResponseObject decodes a JSON object, rejecting non-objects and
// any member outside the allowed set. `what` is a static value-free
// field path. This is the Chat-source counterpart of strictObject:
// one shared validator, so the three former per-target copies cannot
// drift. Diagnostics name the Chat source path only; destination
// framing owns its own messages.
func chatResponseObject(raw json.RawMessage, allowed map[string]bool, what string) (map[string]json.RawMessage, *RouterError) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, k := range keys {
		if !allowed[k] {
			return nil, &RouterError{Status: 502, Code: CodeUpstreamBad,
				Msg: fmt.Sprintf("Upstream response feature %q has no representation", what+"."+k)}
		}
	}
	return obj, nil
}

// decodeChatFinish maps a Chat finish_reason onto the closed IR Finish.
// The deprecated function_call finish and any unknown reason fail
// closed on every target; content_filter stays distinct from stop so
// each encoder can render its own refusal/incomplete mapping.
func decodeChatFinish(choiceRaw json.RawMessage) (Finish, *RouterError) {
	var choice map[string]json.RawMessage
	if err := json.Unmarshal(choiceRaw, &choice); err != nil || choice == nil {
		return Finish{}, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	}
	rawFinish, ok := choice["finish_reason"]
	if !ok || isNull(rawFinish) {
		return Finish{}, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	}
	var finish string
	if err := json.Unmarshal(rawFinish, &finish); err != nil {
		return Finish{}, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	}
	switch finish {
	case "stop":
		return Finish{Kind: FinishStop, Raw: finish}, nil
	case "length":
		return Finish{Kind: FinishLength, Raw: finish}, nil
	case "content_filter":
		return Finish{Kind: FinishContentFilter, Raw: finish}, nil
	case "tool_calls":
		return Finish{Kind: FinishToolCalls, Raw: finish}, nil
	default:
		return Finish{}, &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream stop reason has no representation"}
	}
}

// decodeChatMessage validates one Chat assistant message and splits it
// into text, refusal and tool-call payloads in source order (text, then
// refusal, then calls — the order every target rendered). It returns
// the count of malformed tool calls omitted from the items: a present
// function object with a missing/empty/non-string name or a non-object
// argument payload. All three former decoders agreed on every other
// failure; malformed calls diverge by design (Gemini renders
// MALFORMED_FUNCTION_CALL, Anthropic/Responses refuse), so they are
// carried as provenance, never silently dropped nor failed here.
func decodeChatAssistantMessage(choiceRaw json.RawMessage, lim Limits) (text, refusal string, calls []ResponseItem, malformed int, rerr *RouterError) {
	return decodeChatAssistantMessageWithPolicy(choiceRaw, lim, false)
}

// decodeChatAssistantMessageWithPolicy validates one Chat assistant
// message with narrow private-reasoning policy. reasoning_content is
// provider-private: absent/null/"" is exact, malformed is 502 in both
// modes, substantive is strict-502 unless allowPrivateDrop drops it.
// Dropped bytes never become text, refusal, or tool state.
func decodeChatAssistantMessageWithPolicy(choiceRaw json.RawMessage, lim Limits, allowPrivateDrop bool) (text, refusal string, calls []ResponseItem, malformed int, rerr *RouterError) {
	bad := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	unrep := func(what string) *RouterError {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: fmt.Sprintf("Upstream response feature %q has no representation", what)}
	}
	choice, rerr := chatResponseObject(choiceRaw, map[string]bool{
		"index": true, "message": true, "finish_reason": true, "logprobs": true,
		"native_finish_reason": true,
	}, "response.choices[0]")
	if rerr != nil {
		return "", "", nil, 0, rerr
	}
	if rawIdx, ok := choice["index"]; ok && !isNull(rawIdx) {
		idx, rerr := responseInt(rawIdx)
		if rerr != nil || idx != 0 {
			return "", "", nil, 0, bad
		}
	}
	if rawLP, ok := choice["logprobs"]; ok && !isNull(rawLP) {
		return "", "", nil, 0, unrep("response.choices[0].logprobs")
	}
	rawMsg, ok := choice["message"]
	if !ok || isNull(rawMsg) {
		return "", "", nil, 0, bad
	}
	msg, rerr := chatResponseObject(rawMsg, map[string]bool{
		"role": true, "content": true, "tool_calls": true, "refusal": true,
		"audio": true, "function_call": true, "annotations": true, "name": true,
		"reasoning_content": true, "reasoning": true, "reasoning_details": true,
	}, "response.choices[0].message")
	if rerr != nil {
		return "", "", nil, 0, rerr
	}
	var role string
	if rawRole, ok := msg["role"]; !ok || json.Unmarshal(rawRole, &role) != nil || role != "assistant" {
		return "", "", nil, 0, bad
	}
	if rawName, ok := msg["name"]; ok && !isNull(rawName) {
		var name string
		if err := json.Unmarshal(rawName, &name); err != nil || name != "" {
			return "", "", nil, 0, unrep("response.choices[0].message.name")
		}
	}
	if rawAudio, ok := msg["audio"]; ok && !isNull(rawAudio) {
		return "", "", nil, 0, unrep("response.choices[0].message.audio")
	}
	if rawFn, ok := msg["function_call"]; ok && !isNull(rawFn) {
		return "", "", nil, 0, unrep("response.choices[0].message.function_call")
	}
	if rawAnn, ok := msg["annotations"]; ok && !isNull(rawAnn) {
		var anns []json.RawMessage
		if err := json.Unmarshal(rawAnn, &anns); err != nil || len(anns) > 0 {
			return "", "", nil, 0, unrep("response.choices[0].message.annotations")
		}
	}
	if rawRC, ok := msg["reasoning_content"]; ok && !isNull(rawRC) {
		_, nonempty, rerr := validateChatReasoningContent(rawRC, "response.choices[0].message.reasoning_content")
		if rerr != nil {
			return "", "", nil, 0, rerr
		}
		if nonempty && !allowPrivateDrop {
			return "", "", nil, 0, unrep("response.choices[0].message.reasoning_content")
		}
		// Empty string is exact; substantive is dropped when allowed.
		// Never projected to text, never retained.
	}
	// Measured native kimi-class complete shape: `reasoning` (private
	// string) plus `reasoning_details` (bounded reasoning.text entries).
	// Same policy as reasoning_content: absent/null/empty is exact,
	// malformed or unknown variants refuse in both modes, substantive
	// is strict-502 unless the provider lossy drop applies. Dropped
	// bytes never become text, refusal, or tool state.
	if rawR, ok := msg["reasoning"]; ok && !isNull(rawR) {
		_, nonempty, rerr := validateChatReasoningContent(rawR, "response.choices[0].message.reasoning")
		if rerr != nil {
			return "", "", nil, 0, rerr
		}
		if nonempty {
			var s string
			if err := json.Unmarshal(rawR, &s); err != nil {
				return "", "", nil, 0, bad
			}
			if len(s) > lim.MaxArgsChars {
				return "", "", nil, 0, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream response exceeds the size bound"}
			}
			if !allowPrivateDrop {
				return "", "", nil, 0, unrep("response.choices[0].message.reasoning")
			}
		}
		// Empty string is exact; substantive is dropped when allowed.
	}
	if rawRD, ok := msg["reasoning_details"]; ok && !isNull(rawRD) {
		nonempty, rerr := validateChatReasoningDetails(rawRD, lim, "response.choices[0].message.reasoning_details")
		if rerr != nil {
			return "", "", nil, 0, rerr
		}
		if nonempty && !allowPrivateDrop {
			return "", "", nil, 0, unrep("response.choices[0].message.reasoning_details")
		}
		// Empty array is exact; substantive entries drop when allowed.
	}
	if rawC, ok := msg["content"]; ok && !isNull(rawC) {
		var s string
		if err := json.Unmarshal(rawC, &s); err != nil {
			return "", "", nil, 0, bad
		}
		text = s
	}
	if rawRef, ok := msg["refusal"]; ok && !isNull(rawRef) {
		var r string
		if err := json.Unmarshal(rawRef, &r); err != nil {
			return "", "", nil, 0, bad
		}
		refusal = r
	}
	if rawTC, ok := msg["tool_calls"]; ok && !isNull(rawTC) {
		var arr []json.RawMessage
		if err := json.Unmarshal(rawTC, &arr); err != nil {
			return "", "", nil, 0, bad
		}
		seen := map[string]bool{}
		totalArgs := 0
		for ti, traw := range arr {
			what := fmt.Sprintf("response.choices[0].message.tool_calls[%d]", ti)
			tc, rerr := chatResponseObject(traw, map[string]bool{
				"id": true, "type": true, "function": true,
			}, what)
			if rerr != nil {
				return "", "", nil, 0, rerr
			}
			var id, typ string
			rawID, ok := tc["id"]
			if !ok || json.Unmarshal(rawID, &id) != nil {
				return "", "", nil, 0, bad
			}
			if err := validateCallID(id, lim); err != nil {
				return "", "", nil, 0, bad
			}
			if rawT, ok := tc["type"]; ok && !isNull(rawT) {
				if err := json.Unmarshal(rawT, &typ); err != nil {
					return "", "", nil, 0, bad
				}
			}
			if typ != "" && typ != "function" {
				return "", "", nil, 0, unrep(what + ".type")
			}
			if seen[id] {
				return "", "", nil, 0, &RouterError{Status: 502, Code: CodeUpstreamBad,
					Msg: "Upstream returned conflicting tool call identities"}
			}
			seen[id] = true
			rawFn, ok := tc["function"]
			if !ok || isNull(rawFn) {
				return "", "", nil, 0, bad
			}
			fn, rerr := chatResponseObject(rawFn, map[string]bool{
				"name": true, "arguments": true,
			}, what+".function")
			if rerr != nil {
				return "", "", nil, 0, rerr
			}
			var name string
			nameOK := false
			if rawN, ok := fn["name"]; ok && !isNull(rawN) {
				if err := json.Unmarshal(rawN, &name); err == nil && strings.TrimSpace(name) != "" {
					nameOK = true
				}
			}
			args := EmptyArgs()
			argsOK := nameOK
			if argsOK {
				if rawArgs, ok := fn["arguments"]; ok && !isNull(rawArgs) {
					var s string
					if err := json.Unmarshal(rawArgs, &s); err != nil {
						argsOK = false
					} else if strings.TrimSpace(s) != "" {
						if len(s) > lim.MaxArgsChars {
							return "", "", nil, 0, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call exceeds the size bound"}
						}
						compacted, rerr := CompactArgs(json.RawMessage(s))
						if rerr != nil {
							argsOK = false
						} else {
							args = compacted
						}
					}
				}
			}
			if !argsOK {
				// Malformed tool call: every former decoder agreed the
				// call is unusable, but only Gemini renders it
				// (MALFORMED_FUNCTION_CALL); the others refuse. Carry
				// the count for CheckRepresentable.
				malformed++
				continue
			}
			totalArgs += len(args)
			if len(args) > lim.MaxArgsChars || totalArgs > lim.MaxStreamArgsChars {
				return "", "", nil, 0, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call exceeds the size bound"}
			}
			calls = append(calls, ToolCallItem{
				ID: ItemID(fmt.Sprintf("c%d", len(calls))), CallID: CallID(id), Name: name, Arguments: args,
			})
		}
		if len(arr) > lim.MaxToolCalls {
			return "", "", nil, 0, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call count exceeds the size bound"}
		}
	}
	return text, refusal, calls, malformed, nil
}

// parseChatUsageBuckets validates one Chat usage object ONCE for every
// destination and returns the raw buckets. Totals must be present,
// exact (total == prompt+completion, overflow-checked); every detail
// bucket is optional with missing/null meaning absent. Universally
// unmapped nonzero members (audio, image, prediction shares) fail
// closed here because no target renders them; target-divergent shares
// (cached, cache_write, reasoning, text) are carried for the target
// gate. Zero known buckets are harmless everywhere.
//
// Strict: unknown top-level members (cost, opencode) fail closed.
// Lossy (parseChatUsageBucketsWithPolicy with allowLossy true) drops the
// two known gateway members after bounded validation; all other unknown
// or nonzero unmapped members still fail.
func parseChatUsageBuckets(raw json.RawMessage) (chatUsageBuckets, *RouterError) {
	return parseChatUsageBucketsWithPolicy(raw, false)
}

// parseChatUsageBucketsWithPolicy validates one Chat usage object with
// narrow gateway policy: allowLossy drops the known DeepSeek-class gateway
// extensions (usage.cost, usage.opencode with its closed billing/tokens
// shape) after bounded validation; false keeps the strict 502. Gateway
// bytes are dropped, never carried: authoritative top scalar equations
// still govern, with no reliance on optional gateway accounting. Nonzero
// media shares and any other unknown members fail in both modes with
// value-free paths.
func parseChatUsageBucketsWithPolicy(raw json.RawMessage, allowLossy bool) (chatUsageBuckets, *RouterError) {
	var b chatUsageBuckets
	bad := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	unrep := func(what string) *RouterError {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: fmt.Sprintf("Upstream response feature %q has no representation", what)}
	}
	obj, rerr := chatResponseObject(raw, map[string]bool{
		"prompt_tokens": true, "completion_tokens": true, "total_tokens": true,
		"prompt_tokens_details": true, "completion_tokens_details": true,
		"prompt_cache_hit_tokens": true, "prompt_cache_miss_tokens": true,
		"cost": true, "opencode": true,
	}, "response.usage")
	if rerr != nil {
		return b, rerr
	}
	if rerr := validateGatewayBillingMetadata(obj, "response.usage", allowLossy); rerr != nil {
		return b, rerr
	}
	for _, k := range []string{"prompt_tokens", "completion_tokens", "total_tokens"} {
		rawN, ok := obj[k]
		if !ok || isNull(rawN) {
			return b, bad
		}
		n, rerr := responseInt(rawN)
		if rerr != nil {
			return b, rerr
		}
		switch k {
		case "prompt_tokens":
			b.prompt = n
		case "completion_tokens":
			b.completion = n
		case "total_tokens":
			b.total = n
		}
	}
	if rawDet, ok := obj["prompt_tokens_details"]; ok && !isNull(rawDet) {
		det, rerr := chatResponseObject(rawDet, map[string]bool{
			"cached_tokens": true, "audio_tokens": true, "image_tokens": true,
			"text_tokens": true, "cache_write_tokens": true, "cached_creation_tokens": true,
		}, "response.usage.prompt_tokens_details")
		if rerr != nil {
			return b, rerr
		}
		var cachedCreation *int
		for k, v := range det {
			if isNull(v) {
				continue
			}
			n, rerr := responseInt(v)
			if rerr != nil {
				return b, bad
			}
			switch k {
			case "cached_tokens":
				b.cached = &n
			case "cache_write_tokens":
				b.cacheWrite = &n
			case "cached_creation_tokens":
				cachedCreation = &n
			case "text_tokens":
				if n != 0 {
					b.textPrompt = &n
				}
			default:
				// audio_tokens, image_tokens: no target renders
				// them; nonzero fails closed for every target.
				if n != 0 {
					return b, unrep("response.usage.prompt_tokens_details." + k)
				}
			}
		}
		if cachedCreation != nil {
			switch {
			case b.cacheWrite == nil, *b.cacheWrite == 0:
				if *cachedCreation != 0 {
					b.cacheWrite = cachedCreation
				}
			case *cachedCreation != 0 && *b.cacheWrite != *cachedCreation:
				return b, bad
			}
		}
	}
	// DeepSeek exposes the same cache partition as top-level hit/miss
	// counters. Reconcile each representation against the prompt total.
	for _, k := range []string{"prompt_cache_hit_tokens", "prompt_cache_miss_tokens"} {
		rawCount, ok := obj[k]
		if !ok || isNull(rawCount) {
			continue
		}
		cached, rerr := responseInt(rawCount)
		if rerr != nil || cached > b.prompt {
			return b, bad
		}
		if k == "prompt_cache_miss_tokens" {
			cached = b.prompt - cached
		}
		if b.cached != nil && *b.cached != cached {
			return b, bad
		}
		b.cached = &cached
	}
	if rawDet, ok := obj["completion_tokens_details"]; ok && !isNull(rawDet) {
		det, rerr := chatResponseObject(rawDet, map[string]bool{
			"accepted_prediction_tokens": true, "audio_tokens": true,
			"reasoning_tokens": true, "rejected_prediction_tokens": true,
			"text_tokens": true,
		}, "response.usage.completion_tokens_details")
		if rerr != nil {
			return b, rerr
		}
		for k, v := range det {
			if isNull(v) {
				continue
			}
			n, rerr := responseInt(v)
			if rerr != nil {
				return b, rerr
			}
			switch k {
			case "reasoning_tokens":
				b.reasoning = &n
			case "text_tokens":
				if n != 0 {
					b.textCompletion = &n
				}
			default:
				// accepted/rejected prediction and audio shares have
				// no representation on any target.
				if n != 0 {
					return b, unrep("response.usage.completion_tokens_details." + k)
				}
			}
		}
	}
	if b.cached != nil && *b.cached > b.prompt {
		return b, bad
	}
	if b.cacheWrite != nil && *b.cacheWrite > b.prompt {
		return b, bad
	}
	if b.reasoning != nil && *b.reasoning > b.completion {
		return b, bad
	}
	sum := b.prompt + b.completion
	if sum < b.prompt || sum < b.completion {
		return b, bad
	}
	if b.total != sum {
		return b, bad
	}
	return b, nil
}

// chatTextTotal sums the carried nonzero text shares for
// ResponseUsage.TextTokens. The sum is only a nonzero marker (no
// target renders the value); near-maxint shares fail closed.
func chatTextTotal(b chatUsageBuckets) (*int, *RouterError) {
	total := 0
	for _, share := range []*int{b.textPrompt, b.textCompletion} {
		if share == nil {
			continue
		}
		s, ok := chatUsageAdd(total, *share)
		if !ok {
			return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
		}
		total = s
	}
	if total == 0 {
		return nil, nil
	}
	return &total, nil
}

// chatUsageAdd guards bucket addition against overflow: counters near
// maxint fail closed, never wrap.
func chatUsageAdd(a, b int) (int, bool) {
	if s := a + b; s >= a && s >= b {
		return s, true
	}
	return 0, false
}
