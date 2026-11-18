// Anthropic source codec: one complete-response parser for the Anthropic row.
//
// DecodeAnthropicResponse validates an Anthropic Messages complete response
// ONCE, independently of destination, and returns the canonical Response.
// Target encoders (generic worker owns encoders/types/usage/lifecycle) then
// render WITHOUT reparsing the Anthropic bytes; destination differences live
// in CheckRepresentable plus target-owned framing, never in a second parse
// of the source.
//
// Provenance: this decoder replaces the source-validation halves of
// TranslateAnthropicJSON (anthropic_to_chat.go), TranslateAnthropicJSONToGemini
// (anthropic_to_gemini_json.go: a2gemResponseParts + a2gemUsage) and
// TranslateAnthropicJSONToResponses (anthropic_to_responses_response.go:
// a2rDecodeContent + a2rResponsesStatus + a2rRenderUsage). Every accept/refuse
// below cites the behavior those three agreed on; every intentional
// divergence is carried as typed IR provenance (Finish.Raw, opaque IDs) for
// CheckRepresentable instead of being decided here.
//
// Divergence log (existing validators differ; classification deliberate, bug,
// or unresolved — never blind strictest unification):
//
//  1. tool_use input missing/null: DELIBERATE unification to fail closed.
//     The Chat and Responses complete decoders defaulted a missing/null
//     input to {} (anthropic_to_chat.go:329-336,
//     anthropic_to_responses_response.go:293-300) while the Gemini decoder
//     failed it (anthropic_to_gemini_json.go:269-272). The canonical
//     contract (response_ir_types.go: EmptyArgs) requires a present input
//     object for a COMPLETE Anthropic tool_use, so the unified decoder
//     fails missing/null/non-object input like the strictest proven row.
//  2. stop_sequence: DELIBERATE provenance carry. Chat and Gemini map it
//     (chatFinishReason, a2gemFinishReason); Responses has no form and
//     fails it (a2rResponsesStatus). The decoder accepts it as
//     Finish{Stop, "stop_sequence"}; encoders decide representability
//     from Raw. No per-target policy inside this parser.
//  3. refusal stop: DELIBERATE provenance carry. Chat maps it to
//     content_filter and Responses to incomplete/content_filter, while
//     Gemini has no form and fails it. The decoder preserves it as
//     Finish{Refusal, "refusal"} (never downgraded to stop); encoders map.
//  4. tool_use stop with zero tool calls: DELIBERATE parser acceptance.
//     Responses old failed it (a2r toolCount==0 check) while Chat/Gemini
//     rendered their stop mapping. The decoder records the terminal as
//     Finish{ToolCalls, "tool_use"} with zero items; encoders decide.
//  5. Call-ID grammar: old complete-Responses BUG, superseded. a2rDecodeContent
//     applied validateToolName (1..64 name grammar) to the tool_use ID
//     (anthropic_to_responses_response.go:279), while the Chat decoder,
//     the Gemini decoder, and the Responses streaming path treat call IDs
//     as opaque handles. The unified decoder validates IDs with
//     validateCallID only (bounded, trimmed-nonempty, no name grammar);
//     name grammar stays target-side (CheckRepresentable), never here.
//  6. Usage allowlists: NO divergence. The Chat and Responses rows share
//     parseAnthropicUsage and the Gemini row carries an identical copy
//     (a2gemReadCounters + a2gemRejectBilling): four scalar counters,
//     null-absent billing members, everything else fail closed. Unified
//     below with neutral diagnostics.
//  7. Diagnostic envelope: DELIBERATE neutralization. Each old copy named
//     its own destination ("has no Chat/Gemini/Responses representation").
//     The unified decoder reports "has no representation" with a
//     value-free source field path; destinations name themselves.
//  8. String-ID bounds: DELIBERATE tightening matching the Chat row
//     precedent (response_ir_migration.md unit 1): response and call IDs
//     bounded by lim.MaxArgsChars, whitespace-only rejected uniformly.
//     No old complete row bounded the response ID; no case covers it.
//
// What this decoder never does: Chat-JSON intermediates, map[string]any
// semantic IR, target bucket math (no reasoning/thought/tool-use-prompt
// buckets exist on this wire: reasoning output and private state such as
// thinking text, signatures, or opaque echo fail at decode and never enter
// the IR), or per-target stop mapping.
package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
)

// anthropicIRUsage is the parsed scalar accounting of one Anthropic usage
// object with presence: input/output are required wire counters, creation
// and read are optional shares (nil when the wire omitted them or sent an
// explicit null). Scalar reasoning has no Anthropic usage field: it would
// have to be invented, so there is no bucket here.
type anthropicIRUsage struct {
	input, output  int
	creation, read *int
}

// DecodeAnthropicResponse parses one Anthropic Messages complete response
// into the canonical IR. Source validation is destination-independent:
// unknown members, malformed envelopes, bad totals and unmapped nonzero
// accounting fail closed here for every target. Known source stops whose
// targets diverge (stop_sequence, refusal, call-less tool_use) are carried
// as Finish provenance for CheckRepresentable.
//
// Gateway billing metadata (usage.cost, usage.opencode) is strict by
// default: present non-null members fail closed. Use
// DecodeAnthropicResponseWithPolicy with allowDrop for the lossy
// compatibility exclusion (validated shape, dropped, never forwarded).
func DecodeAnthropicResponse(raw []byte, lim Limits) (Response, *RouterError) {
	return decodeAnthropicResponseWithPolicy(raw, lim, false)
}

// DecodeAnthropicResponseWithPolicy parses one Anthropic complete response
// with narrow gateway-billing policy: allowDrop drops validated
// usage.cost/usage.opencode, false keeps the strict 502. Absent/null is
// exact in both modes; malformed or unknown nested members are 502 in both
// modes. Authoritative output/cache counters and terminal verification are
// unchanged.
func DecodeAnthropicResponseWithPolicy(raw []byte, lim Limits, allowDrop bool) (Response, *RouterError) {
	return decodeAnthropicResponseWithPolicy(raw, lim, allowDrop)
}

func decodeAnthropicResponseWithPolicy(raw []byte, lim Limits, allowDrop bool) (Response, *RouterError) {
	bad := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	obj, rerr := chatResponseObject(raw, map[string]bool{
		"id": true, "type": true, "role": true, "model": true,
		"content": true, "stop_reason": true, "stop_sequence": true, "stop_details": true, "usage": true,
	}, "response")
	if rerr != nil {
		return Response{}, rerr
	}
	var typ, role string
	if err := json.Unmarshal(obj["type"], &typ); err != nil || typ != "message" {
		return Response{}, bad
	}
	if err := json.Unmarshal(obj["role"], &role); err != nil || role != "assistant" {
		return Response{}, bad
	}
	id, rerr := responseString(obj, "id")
	if rerr != nil {
		return Response{}, rerr
	}
	if !validOpaqueID(id) {
		return Response{}, bad
	}
	if len(id) > lim.MaxArgsChars {
		return Response{}, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream response exceeds the size bound"}
	}
	model, rerr := responseString(obj, "model")
	if rerr != nil {
		return Response{}, rerr
	}
	rawStop, ok := obj["stop_reason"]
	if !ok || isNull(rawStop) {
		return Response{}, bad
	}
	var stop string
	if err := json.Unmarshal(rawStop, &stop); err != nil {
		return Response{}, bad
	}
	finish, rerr := decodeAnthropicFinish(stop)
	if rerr != nil {
		return Response{}, rerr
	}
	if rawSS, ok := obj["stop_sequence"]; ok && !isNull(rawSS) {
		var ss string
		if err := json.Unmarshal(rawSS, &ss); err != nil {
			return Response{}, bad
		}
	}
	if rerr := decodeAnthropicStopDetails(obj["stop_details"], stop); rerr != nil {
		return Response{}, rerr
	}
	rawUsage, ok := obj["usage"]
	if !ok || isNull(rawUsage) {
		return Response{}, bad
	}
	scalars, rerr := parseAnthropicIRUsageWithPolicy(rawUsage, allowDrop)
	if rerr != nil {
		return Response{}, rerr
	}
	prompt, rerr := anthropicIRPrompt(scalars)
	if rerr != nil {
		return Response{}, rerr
	}
	total := prompt + scalars.output
	if total < prompt || total < scalars.output {
		return Response{}, bad
	}
	usage, rerr := NewTerminalUsage(DialectAnthropic, prompt, scalars.output, total)
	if rerr != nil {
		return Response{}, rerr
	}
	if usage, rerr = usage.WithCache(scalars.creation, scalars.read); rerr != nil {
		return Response{}, rerr
	}
	rawContent, ok := obj["content"]
	if !ok || isNull(rawContent) {
		return Response{}, bad
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(rawContent, &blocks); err != nil || blocks == nil {
		return Response{}, bad
	}
	items, rerr := decodeAnthropicContent(blocks, lim)
	if rerr != nil {
		return Response{}, rerr
	}
	return NewResponse(ResponseID(id), model, DialectAnthropic, items, usage, finish)
}

// decodeAnthropicFinish maps an Anthropic stop_reason onto the closed IR
// Finish, preserving the semantic distinctions the destinations need.
// end_turn and stop_sequence are both natural stops (their Responses
// difference travels in Raw); max_tokens is a length cutoff; tool_use ends
// for calls; refusal is an explicit refusal, never downgraded to stop.
// pause_turn, model_context_window_exceeded and any other reason have no
// reviewed mapping and fail closed; no synthetic stop is invented.
func decodeAnthropicFinish(stop string) (Finish, *RouterError) {
	switch stop {
	case "end_turn", "stop_sequence":
		return Finish{Kind: FinishStop, Raw: stop}, nil
	case "max_tokens":
		return Finish{Kind: FinishLength, Raw: stop}, nil
	case "tool_use":
		return Finish{Kind: FinishToolCalls, Raw: stop}, nil
	case "refusal":
		return Finish{Kind: FinishRefusal, Raw: stop}, nil
	default:
		return Finish{}, &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream stop reason has no representation"}
	}
}

// decodeAnthropicStopDetails validates the optional Messages `stop_details`
// member without carrying it into the IR.
//
// Official schema (Messages API reference; anthropic-sdk-python
// types/message.py + types/refusal_stop_details.py; handling-stop-reasons;
// handle-streaming-refusals):
//   - `stop_details` is present on every Message, null when the stop carries
//     no additional detail. A refusal always carries an object
//     {type:"refusal", category, explanation}, where category is one of
//     cyber/bio/frontier_llm/reasoning_extraction/general_harms or null and
//     explanation is a human-readable string or null (both null are normal,
//     permanent values, never placeholders).
//   - Absent/null is accepted as absent for every stop.
//   - A non-null object is evidence-supported only on a `refusal` stop with
//     type "refusal". Any non-null details on a non-refusal stop fail closed
//     (no silent downgrade of a safety signal to a normal stop).
//   - The object is validated for shape (closed members, typed category and
//     explanation) but never forwarded: category/explanation text is upstream
//     safety metadata with no downstream slot, so it is dropped after
//     validation. Diagnostics stay value-free; upstream strings are never
//     echoed.
func decodeAnthropicStopDetails(raw json.RawMessage, stop string) *RouterError {
	if len(raw) == 0 || isNull(raw) {
		return nil
	}
	unrep := func(what string) *RouterError {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: fmt.Sprintf("Upstream response feature %q has no representation", what)}
	}
	bad := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	obj, rerr := chatResponseObject(raw, map[string]bool{
		"type": true, "category": true, "explanation": true,
	}, "response.stop_details")
	if rerr != nil {
		return rerr
	}
	rawType, ok := obj["type"]
	if !ok || isNull(rawType) {
		return bad
	}
	var typ string
	if err := json.Unmarshal(rawType, &typ); err != nil || typ != "refusal" {
		return unrep("response.stop_details.type")
	}
	if rawCat, ok := obj["category"]; ok && !isNull(rawCat) {
		var cat string
		if err := json.Unmarshal(rawCat, &cat); err != nil {
			return bad
		}
		switch cat {
		case "cyber", "bio", "frontier_llm", "reasoning_extraction", "general_harms":
		default:
			return unrep("response.stop_details.category")
		}
	}
	if rawExp, ok := obj["explanation"]; ok && !isNull(rawExp) {
		var exp string
		if err := json.Unmarshal(rawExp, &exp); err != nil {
			return bad
		}
		_ = exp
	}
	if stop != "refusal" {
		return unrep("response.stop_details")
	}
	return nil
}

// decodeAnthropicContent validates Anthropic content blocks in order and
// returns one IR item per block, preserving the upstream order one-to-one
// (a tool-first response keeps its tool item before later text). Text
// blocks allow only type/text/citations; tool_use blocks allow only
// type/id/name/input; cross-kind and unknown members fail closed. Thinking,
// redacted thinking, server tools, media and any other unrepresentable
// block type fail closed, never dropped.
func decodeAnthropicContent(blocks []json.RawMessage, lim Limits) ([]ResponseItem, *RouterError) {
	bad := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	unrep := func(what string) *RouterError {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: fmt.Sprintf("Upstream response feature %q has no representation", what)}
	}
	items := make([]ResponseItem, 0, len(blocks))
	seenIDs := map[string]bool{}
	toolCount := 0
	totalArgs := 0
	for bi, braw := range blocks {
		what := fmt.Sprintf("response.content[%d]", bi)
		var kindProbe map[string]json.RawMessage
		if err := json.Unmarshal(braw, &kindProbe); err != nil || kindProbe == nil {
			return nil, bad
		}
		rawType, ok := kindProbe["type"]
		if !ok || isNull(rawType) {
			return nil, bad
		}
		var btype string
		if err := json.Unmarshal(rawType, &btype); err != nil {
			return nil, bad
		}
		switch btype {
		case "text":
			b, rerr := chatResponseObject(braw, map[string]bool{
				"type": true, "text": true, "citations": true,
			}, what)
			if rerr != nil {
				return nil, rerr
			}
			rawText, ok := b["text"]
			if !ok || isNull(rawText) {
				return nil, bad
			}
			var text string
			if err := json.Unmarshal(rawText, &text); err != nil {
				return nil, bad
			}
			if rawCit, ok := b["citations"]; ok && !isNull(rawCit) {
				var cits []json.RawMessage
				if err := json.Unmarshal(rawCit, &cits); err != nil || len(cits) > 0 {
					return nil, unrep(what + ".citations")
				}
			}
			items = append(items, TextItem{ID: ItemID(fmt.Sprintf("b%d", len(items))), Text: text})
		case "tool_use":
			b, rerr := chatResponseObject(braw, map[string]bool{
				"type": true, "id": true, "name": true, "input": true,
			}, what)
			if rerr != nil {
				return nil, rerr
			}
			rawID, okID := b["id"]
			rawName, okName := b["name"]
			if !okID || !okName {
				return nil, bad
			}
			var toolID, toolName string
			if err := json.Unmarshal(rawID, &toolID); err != nil {
				return nil, bad
			}
			if err := validateCallID(toolID, lim); err != nil {
				return nil, bad
			}
			if err := json.Unmarshal(rawName, &toolName); err != nil || strings.TrimSpace(toolName) == "" {
				return nil, bad
			}
			if seenIDs[toolID] {
				return nil, &RouterError{Status: 502, Code: CodeUpstreamBad,
					Msg: "Upstream returned conflicting tool call identities"}
			}
			seenIDs[toolID] = true
			toolCount++
			if toolCount > lim.MaxToolCalls {
				return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call count exceeds the size bound"}
			}
			rawInput, ok := b["input"]
			if !ok || isNull(rawInput) {
				return nil, bad
			}
			args, rerr := CompactArgs(rawInput)
			if rerr != nil {
				return nil, rerr
			}
			if len(args) > lim.MaxArgsChars {
				return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call exceeds the size bound"}
			}
			totalArgs += len(args)
			if totalArgs > lim.MaxStreamArgsChars {
				return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call exceeds the size bound"}
			}
			items = append(items, ToolCallItem{
				ID: ItemID(fmt.Sprintf("b%d", len(items))), CallID: CallID(toolID), Name: toolName, Arguments: args,
			})
		default:
			return nil, unrep(what + ".type")
		}
	}
	return items, nil
}

// anthropicNullAbsent lists documented usage members that carry no token
// accounting of their own. An explicit null means the feature is unused and
// is accepted as absent; any non-null value names an unmapped billing
// feature (TTL cache breakdowns, thinking breakdowns, server-tool counts,
// tier/speed/geo) and fails closed rather than being silently dropped.
var anthropicNullAbsent = map[string]bool{
	"cache_creation": true, "output_tokens_details": true,
	"server_tool_use": true, "service_tier": true,
	"speed": true, "inference_geo": true,
}

// anthropicEmptyUsageEcho recognizes zero-valued breakdowns that carry no
// billing or thinking information. Unknown fields remain unsupported.
func anthropicEmptyUsageEcho(key string, raw json.RawMessage, creation int) bool {
	var keys []string
	switch key {
	case "cache_creation":
		if creation != 0 {
			return false
		}
		keys = []string{"ephemeral_1h_input_tokens", "ephemeral_5m_input_tokens"}
	case "output_tokens_details":
		keys = []string{"thinking_tokens"}
	default:
		return false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || len(fields) != len(keys) {
		return false
	}
	for _, key := range keys {
		n, rerr := responseInt(fields[key])
		if rerr != nil || n != 0 {
			return false
		}
	}
	return true
}

// parseAnthropicIRUsage validates one Anthropic usage object and returns its
// scalar components with presence. input_tokens and output_tokens are
// required as non-null integers (a complete Message needs both); the cache
// scalars are optional with missing/null meaning absent. Malformed counters
// (negative, fractional, quoted, overflowing), unknown members and non-null
// unmapped billing members are 502s: usage is never underreported or
// silently trimmed. This unifies parseAnthropicUsage (shared by the Chat and
// Responses rows) and its identical Gemini-row copy with neutral
// diagnostics.
//
// Gateway billing metadata (cost, opencode) is strict by default; see
// parseAnthropicIRUsageWithPolicy for the lossy exclusion.
func parseAnthropicIRUsage(raw json.RawMessage) (anthropicIRUsage, *RouterError) {
	return parseAnthropicIRUsageWithPolicy(raw, false)
}

// parseAnthropicIRUsageWithPolicy validates one Anthropic usage object with
// narrow gateway-billing policy: allowDrop drops validated cost/opencode,
// false keeps the strict 502. Authoritative counters are unchanged.
func parseAnthropicIRUsageWithPolicy(raw json.RawMessage, allowDrop bool) (anthropicIRUsage, *RouterError) {
	var u anthropicIRUsage
	bad := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	unrep := func(what string) *RouterError {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: fmt.Sprintf("Upstream response feature %q has no representation", what)}
	}
	obj, rerr := chatResponseObject(raw, map[string]bool{
		"input_tokens": true, "output_tokens": true,
		"cache_creation_input_tokens": true, "cache_read_input_tokens": true,
		"cache_creation": true, "output_tokens_details": true,
		"server_tool_use": true, "service_tier": true,
		"speed": true, "inference_geo": true,
		"cost": true, "opencode": true,
	}, "response.usage")
	if rerr != nil {
		return u, rerr
	}
	if rerr := validateGatewayBillingMetadata(obj, "response.usage", allowDrop); rerr != nil {
		return u, rerr
	}
	for _, k := range []string{"input_tokens", "output_tokens", "cache_creation_input_tokens", "cache_read_input_tokens"} {
		rawN, ok := obj[k]
		if !ok || isNull(rawN) {
			if k == "input_tokens" || k == "output_tokens" {
				return u, bad
			}
			continue
		}
		n, rerr := responseInt(rawN)
		if rerr != nil {
			return u, rerr
		}
		switch k {
		case "input_tokens":
			u.input = n
		case "output_tokens":
			u.output = n
		case "cache_creation_input_tokens":
			u.creation = &n
		case "cache_read_input_tokens":
			u.read = &n
		}
	}
	creation := 0
	if u.creation != nil {
		creation = *u.creation
	}
	for k := range obj {
		if anthropicNullAbsent[k] {
			if v := obj[k]; !isNull(v) && !anthropicEmptyUsageEcho(k, v, creation) {
				return u, unrep("response.usage." + k)
			}
		}
	}
	return u, nil
}

// anthropicIRPrompt returns the canonical prompt total: per the Messages
// API, input accounting is the summation of input_tokens,
// cache_creation_input_tokens and cache_read_input_tokens, counted exactly
// once. Addition is overflow-checked: usage is never wrapped.
func anthropicIRPrompt(u anthropicIRUsage) (int, *RouterError) {
	bad := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	creation, read := 0, 0
	if u.creation != nil {
		creation = *u.creation
	}
	if u.read != nil {
		read = *u.read
	}
	p := u.input + creation
	if p < u.input || p < creation {
		return 0, bad
	}
	p += read
	if p < u.input || p < creation || p < read {
		return 0, bad
	}
	return p, nil
}
