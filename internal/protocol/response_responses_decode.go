// Responses source codec: one complete-response parser for the Responses row.
//
// DecodeResponsesResponse validates a Responses response ONCE,
// independently of destination, and returns the canonical Response.
// The three target encoders (generic worker owns encoders/types/usage/
// lifecycle) then render WITHOUT reparsing the Responses bytes;
// destination differences live in CheckRepresentable plus target-owned
// framing, never in a second parse of the source.
//
// Provenance: this decoder replaces the source-validation halves of
// TranslateResponsesJSONToChat (r2cDecodeOutput + r2cParseUsage +
// r2cValidateResponseEnvelope), TranslateResponsesJSONToAnthropic
// (r2aDecodeOutput + r2aParseUsage + r2aValidateResponseEnvelope) and
// TranslateResponsesJSONToGemini (r2gDecodeOutput + r2gParseUsage +
// r2gValidateResponseEnvelope). Every accept below cites the behavior
// those three agreed on; every intentional divergence is carried as
// typed IR provenance (RefusalItem, MalformedTools, Finish.Raw,
// Reasoning/Cache buckets) for CheckRepresentable instead of being
// decided here.
//
// Divergence log (existing validators differ; classification deliberate,
// bug, or unresolved - never blind strictest unification):
//
//  1. call_id grammar: old COMPLETE-Responses BUG, superseded. r2c and r2a
//     applied validateToolName (1..64 name grammar) to call_id
//     (responses_to_chat_response.go:372, responses_to_anthropic_output.go:163)
//     while r2g correctly used validateCallID (opaque, bounded).
//     The unified decoder validates call IDs with validateCallID only;
//     name grammar stays target-side (CheckRepresentable), never here.
//     Pure probe: call_id "has:colon and space" passes r2g, fails r2c/r2a.
//  2. malformed name/args: DELIBERATE provenance carry. r2c/r2a fail closed
//     on missing/empty/grammar-invalid names and non-object args strings,
//     while r2g omits them and reports malformed -> MALFORMED_FUNCTION_CALL.
//     The unified decoder counts them in Response.MalformedTools and skips
//     the item (same shape as the Chat row); encoders decide
//     representability from that count. Pure probe: name "has space" or
//     arguments "[1]" fails r2c/r2a, renders malformed in r2g.
//  3. usage buckets: DELIBERATE neutral carry, no target subtraction.
//     r2c allows reasoning (maps to Chat details), refuses nonzero
//     cache_write; r2a allows cache_write (maps to Anthropic creation),
//     refuses nonzero reasoning; r2g allows reasoning (thoughts), refuses
//     nonzero cache_write. The unified decoder carries CacheRead,
//     CacheCreation and Reasoning verbatim with prompt = input_tokens
//     verbatim (never input-cache-write subtraction like r2aParseUsage).
//     CheckRepresentable decides per (source,dest). Pure probe:
//     cache_write_tokens:5 passes r2a, fails r2c/r2g; reasoning_tokens:3
//     passes r2c/r2g, fails r2a.
//  4. refusal content: DELIBERATE provenance carry. r2c concatenates refusal
//     and maps completed+refusal to content_filter; r2a/r2g fail any refusal
//     part. The unified decoder carries ordered RefusalItems and derives
//     Finish from status only (completed -> Stop, incomplete reasons ->
//     Length/ContentFilter); encoders map Stop+Refusal per destination.
//     Pure probe: output_text "hi" + refusal "no" passes r2c, fails r2a/r2g.
//  5. refusal+tools combination: DELIBERATE parser acceptance. r2c fails
//     "response.output refusal with function calls" for Chat; r2a/r2g never
//     reach the check (refusal alone fails). The unified decoder carries
//     both; encoders refuse the combination where unmapped. Never a source
//     validation failure.
//  6. empty text/output: NO divergence carried as failure. r2c keeps empty
//     text in its concatenation, r2a/r2g drop empty spans; r2a fails
//     text-less call-less output while r2c/r2g render stop. The unified
//     decoder drops empty text/refusal spans (no item) and allows zero-item
//     output as Stop; encoders decide emptiness representability.
//  7. order: DELIBERATE order preservation. r2c concatenates all text then
//     appends calls (order-normalizing); r2a preserves via text/trailing
//     buckets; r2g preserves per-message/part order. The unified decoder
//     preserves output+content order one-to-one (one Text/Refusal item per
//     non-empty part, one ToolCall per function_call, in wire order).
//  8. diagnostic envelope: DELIBERATE neutralization. Each old copy named
//     its own destination ("has no Chat/Anthropic/Gemini representation").
//     The unified decoder reports "has no representation" with a value-free
//     source field path; destinations name themselves. Unknown enum values
//     (status, incomplete reason) never enter RouterError.Msg (Anthropic
//     review ee3c76d caught this class).
//
// What this decoder never does: Chat-JSON intermediates, map[string]any
// semantic IR, target bucket math (no Anthropic subtraction, no Gemini
// thoughts derivation), per-target stop mapping, or private-state
// materialization. Reasoning OUTPUT items, media parts, native hosted
// tools, and opaque private state fail at decode and never enter the IR;
// scalar reasoning accounting travels in ResponseUsage.Reasoning.
package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
)

// DecodeResponsesResponse parses one Responses complete response into the
// canonical IR. Source validation is destination-independent: unknown
// members, malformed envelopes, bad totals and universally unmapped
// nonzero accounting fail closed here for every target. Known
// target-divergent features (refusal text, malformed calls, cache
// creation, scalar reasoning) are carried as IR provenance for
// CheckRepresentable.
//
// Strict default: provider-private reasoning OUTPUT items fail closed
// with a value-free path. Use DecodeResponsesResponseWithPolicy with
// allowPrivateDrop for the lossy compatibility drop (documented in
// response_responses_reasoning.go).
func DecodeResponsesResponse(raw []byte, lim Limits) (Response, *RouterError) {
	return decodeResponsesResponseWithPolicy(raw, lim, false)
}

// DecodeResponsesResponseWithPolicy parses one Responses complete
// response with narrow private-reasoning policy: allowPrivateDrop drops
// the measured reasoning shape, false keeps the strict 502. Absent
// reasoning decodes identically in both modes; malformed and nonempty
// summaries fail in both modes.
func DecodeResponsesResponseWithPolicy(raw []byte, lim Limits, allowPrivateDrop bool) (Response, *RouterError) {
	return decodeResponsesResponseWithPolicy(raw, lim, allowPrivateDrop)
}

func decodeResponsesResponseWithPolicy(raw []byte, lim Limits, allowPrivateDrop bool) (Response, *RouterError) {
	bad := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	unrep := func(what string) *RouterError {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: fmt.Sprintf("Upstream response feature %q has no representation", what)}
	}
	obj, rerr := responsesSourceEnvelope(raw)
	if rerr != nil {
		return Response{}, rerr
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
	if rawObj, ok := obj["object"]; ok && !isNull(rawObj) {
		var o string
		if err := json.Unmarshal(rawObj, &o); err != nil || o != "response" {
			return Response{}, bad
		}
	}
	if rawCreated, ok := obj["created_at"]; ok && !isNull(rawCreated) {
		if rerr := r2cValidateCreatedAt(rawCreated); rerr != nil {
			return Response{}, bad
		}
	}
	rawStatus, ok := obj["status"]
	if !ok || isNull(rawStatus) {
		return Response{}, bad
	}
	var status string
	if err := json.Unmarshal(rawStatus, &status); err != nil {
		return Response{}, bad
	}
	if rawErr, ok := obj["error"]; ok && !isNull(rawErr) {
		var errObj map[string]json.RawMessage
		if err := json.Unmarshal(rawErr, &errObj); err != nil {
			return Response{}, bad
		}
		if len(errObj) > 0 {
			return Response{}, &RouterError{Status: 502, Code: CodeUpstreamBad,
				Msg: "Upstream response reported an error"}
		}
	}
	var finish Finish
	switch status {
	case "completed":
		finish = Finish{Kind: FinishStop, Raw: status}
	case "incomplete":
		rawDet, ok := obj["incomplete_details"]
		if !ok || isNull(rawDet) {
			return Response{}, bad
		}
		det, rerr := chatResponseObject(rawDet, map[string]bool{"reason": true}, "response.incomplete_details")
		if rerr != nil {
			return Response{}, rerr
		}
		rawReason, ok := det["reason"]
		if !ok || isNull(rawReason) {
			return Response{}, bad
		}
		var reason string
		if err := json.Unmarshal(rawReason, &reason); err != nil {
			return Response{}, bad
		}
		switch reason {
		case "max_output_tokens":
			finish = Finish{Kind: FinishLength, Raw: reason}
		case "content_filter":
			finish = Finish{Kind: FinishContentFilter, Raw: reason}
		default:
			return Response{}, unrep("response.incomplete_details.reason")
		}
	case "failed":
		return Response{}, &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream response reported an error"}
	default:
		// failed/cancelled/in_progress/queued and any future terminal:
		// refuse value-free, never fabricate stop. Old rows interpolated
		// the unknown status into Msg; that leaks upstream enums.
		return Response{}, unrep("response.status")
	}
	rawOut, ok := obj["output"]
	if !ok || isNull(rawOut) {
		return Response{}, bad
	}
	var items []json.RawMessage
	if err := json.Unmarshal(rawOut, &items); err != nil || items == nil {
		return Response{}, bad
	}
	irItems, malformed, rerr := decodeResponsesOutputWithPolicy(items, lim, allowPrivateDrop)
	if rerr != nil {
		return Response{}, rerr
	}
	rawUsage, ok := obj["usage"]
	if !ok || isNull(rawUsage) {
		return Response{}, bad
	}
	usage, rerr := parseResponsesIRUsage(rawUsage, allowPrivateDrop)
	if rerr != nil {
		return Response{}, rerr
	}
	resp, rerr := NewResponse(ResponseID(id), model, DialectResponses, irItems, usage, finish)
	if rerr != nil {
		return Response{}, rerr
	}
	resp.MalformedTools = malformed
	return resp, nil
}

// responsesSourceEnvelope validates the full Responses response envelope
// with destination-neutral diagnostics. Documented request echoes with
// defaults validate and are ignored; non-default features (background:true,
// non-text formats) and unknown non-null members fail as unrepresentable;
// malformed echo values fail as invalid. Null is always accepted as
// absent/default.
func responsesSourceEnvelope(raw json.RawMessage) (map[string]json.RawMessage, *RouterError) {
	bad := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	unrep := func(what string) *RouterError {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: fmt.Sprintf("Upstream response feature %q has no representation", what)}
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil || top == nil {
		return nil, bad
	}
	core := map[string]bool{
		"id": true, "object": true, "created_at": true, "model": true,
		"status": true, "output": true, "usage": true,
		"error": true, "incomplete_details": true,
	}
	for k, v := range top {
		if core[k] {
			continue
		}
		if isNull(v) {
			continue
		}
		switch k {
		case "background":
			var b bool
			if err := json.Unmarshal(v, &b); err != nil {
				return nil, bad
			}
			if b {
				return nil, unrep("response.background")
			}
		case "instructions":
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				return nil, bad
			}
		case "max_output_tokens":
			if _, rerr := responseInt(v); rerr != nil {
				return nil, bad
			}
		case "parallel_tool_calls":
			var b bool
			if err := json.Unmarshal(v, &b); err != nil {
				return nil, bad
			}
		case "previous_response_id":
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				return nil, bad
			}
		case "prompt_cache_retention":
			// Live GPT-5.6 Luna Responses upstream echoes the request
			// control as "24h" on ordinary generations without a cache
			// policy (observed in completed JSON and created SSE).
			// Reviewed values ("24h", "in_memory") are exact absent
			// controls (no cache policy to carry): validate and discard.
			// Malformed values fail invalid; future string values have no
			// representation. Shared by full and all stream envelope
			// phases via this helper.
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				return nil, bad
			}
			if s != "24h" && s != "in_memory" {
				return nil, unrep("response.prompt_cache_retention")
			}
		case "reasoning":
			if rerr := validateResponsesReasoningEcho(v); rerr != nil {
				return nil, rerr
			}
		case "store":
			var b bool
			if err := json.Unmarshal(v, &b); err != nil {
				return nil, bad
			}
		case "temperature", "top_p":
			lit := strings.TrimSpace(string(v))
			if lit == "" || lit[0] == '"' || lit[0] == '\'' {
				return nil, bad
			}
			var n json.Number
			if err := json.Unmarshal(v, &n); err != nil {
				return nil, bad
			}
			if _, err := n.Float64(); err != nil {
				return nil, bad
			}
		case "text":
			// Live GPT-5.6 Luna Responses upstream echoes the request
			// control as {"format":{"type":"text"},"verbosity":"medium"}
			// on ordinary generations (observed in completed JSON).
			// Reviewed verbosity values ("low", "medium", "high") are
			// exact absent controls (no verbosity to carry): validate
			// and discard. Malformed values fail invalid; future string
			// values have no representation. Shared by full and all
			// stream envelope phases via this helper.
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(v, &obj); err != nil || obj == nil {
				return nil, bad
			}
			for tk, tv := range obj {
				if tk == "verbosity" {
					if isNull(tv) {
						continue
					}
					var s string
					if err := json.Unmarshal(tv, &s); err != nil {
						return nil, bad
					}
					if s != "low" && s != "medium" && s != "high" {
						return nil, unrep("response.text.verbosity")
					}
					continue
				}
				if tk != "format" {
					if isNull(tv) {
						continue
					}
					return nil, unrep("response.text." + tk)
				}
				var fmtObj map[string]json.RawMessage
				if err := json.Unmarshal(tv, &fmtObj); err != nil || fmtObj == nil {
					return nil, bad
				}
				for fk, fv := range fmtObj {
					if fk != "type" {
						if isNull(fv) {
							continue
						}
						return nil, unrep("response.text.format." + fk)
					}
					var t string
					if err := json.Unmarshal(fv, &t); err != nil || t != "text" {
						if err != nil {
							return nil, bad
						}
						return nil, unrep("response.text.format.type")
					}
				}
			}
		case "tool_choice":
			var s string
			if err := json.Unmarshal(v, &s); err == nil {
				if s != "auto" && s != "none" && s != "required" {
					return nil, bad
				}
			} else {
				var obj map[string]json.RawMessage
				if err := json.Unmarshal(v, &obj); err != nil || obj == nil {
					return nil, bad
				}
			}
		case "top_logprobs":
			// Live OpenCode Responses upstream echoes the request control as
			// 0 on ordinary generations (observed in completed JSON and in
			// created SSE). Zero/absent/null are exact absent controls (no
			// logprobs to carry); nonzero requests logprobs with no
			// representation; malformed fails invalid. Shared by full and
			// all stream envelope phases via this helper.
			n, rerr := responseInt(v)
			if rerr != nil {
				return nil, bad
			}
			if n != 0 {
				return nil, unrep("response.top_logprobs")
			}
		case "tools":
			var arr []json.RawMessage
			if err := json.Unmarshal(v, &arr); err != nil {
				return nil, bad
			}
		case "tool_usage":
			// Live GPT-5.6 Luna Responses upstream echoes request-adjacent
			// usage as {"image_gen":{"input_tokens":0,"output_tokens":0,
			// "total_tokens":0,"input_tokens_details":{"image_tokens":0,
			// "text_tokens":0},"output_tokens_details":{"image_tokens":0,
			// "text_tokens":0}},"web_search":{"num_requests":0}} on
			// ordinary generations without tool execution (observed in
			// completed JSON). Structurally valid zero-only known members
			// are exact absent controls (no tool execution to carry):
			// validate and discard. Nonzero known counters and unknown
			// non-null members have no representation; malformed values
			// fail invalid. Null at any level is absent/default. Shared
			// by full and all stream envelope phases via this helper.
			if rerr := validateResponsesToolUsageEcho(v); rerr != nil {
				return nil, rerr
			}
		case "truncation":
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				return nil, bad
			}
			if s != "auto" && s != "disabled" {
				return nil, unrep("response.truncation")
			}
		case "user":
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				return nil, bad
			}
		case "metadata":
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(v, &obj); err != nil || obj == nil {
				return nil, bad
			}
		case "service_tier":
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				return nil, bad
			}
		case "completed_at":
			if rerr := r2cValidateCreatedAt(v); rerr != nil {
				return nil, bad
			}
		default:
			return nil, unrep("response." + k)
		}
	}
	return top, nil
}

// validateResponsesToolUsageEcho validates the top-level `tool_usage` echo
// value from a Responses response envelope. Null/absent is exact absent
// (no tool execution). Known members are `image_gen` and `web_search` with
// zero-only counters; all zeros validate and discard. Nonzero known
// counters and unknown non-null members have no representation (value-free
// path, never values); malformed shapes fail invalid. Null at any nested
// level is absent/default. No tool execution is performed.
func validateResponsesToolUsageEcho(raw json.RawMessage) *RouterError {
	bad := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	unrep := func(what string) *RouterError {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: fmt.Sprintf("Upstream response feature %q has no representation", what)}
	}
	if isNull(raw) {
		return nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil || top == nil {
		return bad
	}
	for k, v := range top {
		if isNull(v) {
			continue
		}
		switch k {
		case "image_gen":
			var gen map[string]json.RawMessage
			if err := json.Unmarshal(v, &gen); err != nil || gen == nil {
				return bad
			}
			for gk, gv := range gen {
				if isNull(gv) {
					continue
				}
				switch gk {
				case "input_tokens", "output_tokens", "total_tokens":
					n, rerr := responseInt(gv)
					if rerr != nil {
						return bad
					}
					if n != 0 {
						return unrep("response.tool_usage.image_gen." + gk)
					}
				case "input_tokens_details", "output_tokens_details":
					var det map[string]json.RawMessage
					if err := json.Unmarshal(gv, &det); err != nil || det == nil {
						return bad
					}
					for dk, dv := range det {
						if isNull(dv) {
							continue
						}
						if dk != "image_tokens" && dk != "text_tokens" {
							return unrep("response.tool_usage.image_gen." + gk + "." + dk)
						}
						n, rerr := responseInt(dv)
						if rerr != nil {
							return bad
						}
						if n != 0 {
							return unrep("response.tool_usage.image_gen." + gk + "." + dk)
						}
					}
				default:
					return unrep("response.tool_usage.image_gen." + gk)
				}
			}
		case "web_search":
			var ws map[string]json.RawMessage
			if err := json.Unmarshal(v, &ws); err != nil || ws == nil {
				return bad
			}
			for wk, wv := range ws {
				if isNull(wv) {
					continue
				}
				if wk != "num_requests" {
					return unrep("response.tool_usage.web_search." + wk)
				}
				n, rerr := responseInt(wv)
				if rerr != nil {
					return bad
				}
				if n != 0 {
					return unrep("response.tool_usage.web_search." + wk)
				}
			}
		default:
			return unrep("response.tool_usage." + k)
		}
	}
	return nil
}

// decodeResponsesOutput splits Responses output items into ordered IR items.
// Message items contribute one TextItem per non-empty output_text part and
// one RefusalItem per non-empty refusal part, in wire order; function_call
// items contribute one ToolCallItem each, in wire order. Only message items
// with output_text/refusal content and function_call items are supported;
// every other item type (reasoning, media, hosted tools, private state)
// fails closed. Malformed function calls (missing/empty/grammar-invalid
// name, non-object args) are omitted and counted, never fabricated.
//
// Strict default: reasoning OUTPUT items fail closed. Use
// decodeResponsesOutputWithPolicy with allowPrivateDrop for the lossy
// private-reasoning drop (response_responses_reasoning.go).
func decodeResponsesOutput(items []json.RawMessage, lim Limits) ([]ResponseItem, int, *RouterError) {
	return decodeResponsesOutputWithPolicy(items, lim, false)
}

// validateResponsesMessagePhase accepts the final answer label on ordinary
// replies. A lossy bridge can also retain visible commentary as assistant
// text while omitting the phase label. The terminal finish is independent.
func validateResponsesMessagePhase(raw json.RawMessage, path string, allowLossy bool, seen *string) *RouterError {
	if len(raw) == 0 || isNull(raw) {
		return nil
	}
	var phase string
	if json.Unmarshal(raw, &phase) != nil {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	}
	if phase != "final_answer" && !(allowLossy && phase == "commentary") {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: fmt.Sprintf("Upstream response feature %q has no representation", path)}
	}
	if seen != nil {
		if *seen != "" && *seen != phase {
			return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream changed its message phase"}
		}
		*seen = phase
	}
	return nil
}

func decodeResponsesOutputWithPolicy(items []json.RawMessage, lim Limits, allowPrivateDrop bool) ([]ResponseItem, int, *RouterError) {
	bad := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	unrep := func(what string) *RouterError {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: fmt.Sprintf("Upstream response feature %q has no representation", what)}
	}
	out := []ResponseItem{}
	seenCall := map[string]bool{}
	seenItem := map[string]bool{}
	totalArgs := 0
	malformed := 0
	for oi, raw := range items {
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil || probe.Type == "" {
			return nil, 0, bad
		}
		switch probe.Type {
		case "message":
			msg, rerr := chatResponseObject(raw, map[string]bool{
				"id": true, "type": true, "role": true, "content": true, "status": true, "phase": true,
			}, fmt.Sprintf("response.output[%d]", oi))
			if rerr != nil {
				return nil, 0, rerr
			}
			if rerr := validateResponsesMessagePhase(msg["phase"], fmt.Sprintf("response.output[%d].phase", oi), allowPrivateDrop, nil); rerr != nil {
				return nil, 0, rerr
			}
			var role string
			if rawRole, ok := msg["role"]; !ok || json.Unmarshal(rawRole, &role) != nil || role != "assistant" {
				return nil, 0, bad
			}
			if rawSt, ok := msg["status"]; ok && !isNull(rawSt) {
				var st string
				if err := json.Unmarshal(rawSt, &st); err != nil ||
					(st != "completed" && st != "incomplete") {
					return nil, 0, bad
				}
			}
			if rawID, ok := msg["id"]; ok && !isNull(rawID) {
				var mid string
				if err := json.Unmarshal(rawID, &mid); err != nil || strings.TrimSpace(mid) == "" {
					return nil, 0, bad
				}
				if seenItem[mid] {
					return nil, 0, &RouterError{Status: 502, Code: CodeUpstreamBad,
						Msg: "Upstream returned conflicting response item identities"}
				}
				seenItem[mid] = true
			}
			rawContent, ok := msg["content"]
			if !ok || isNull(rawContent) {
				return nil, 0, bad
			}
			var parts []json.RawMessage
			if err := json.Unmarshal(rawContent, &parts); err != nil || parts == nil {
				return nil, 0, bad
			}
			for pi, praw := range parts {
				part, rerr := chatResponseObject(praw, map[string]bool{
					"type": true, "text": true, "annotations": true, "logprobs": true,
					"refusal": true,
				}, fmt.Sprintf("response.output[%d].content[%d]", oi, pi))
				if rerr != nil {
					return nil, 0, rerr
				}
				var ptype string
				if err := json.Unmarshal(part["type"], &ptype); err != nil {
					return nil, 0, bad
				}
				switch ptype {
				case "output_text":
					rawT, ok := part["text"]
					if !ok || isNull(rawT) {
						return nil, 0, bad
					}
					var s string
					if err := json.Unmarshal(rawT, &s); err != nil {
						return nil, 0, bad
					}
					if rawAnn, ok := part["annotations"]; ok && !isNull(rawAnn) {
						var anns []json.RawMessage
						if err := json.Unmarshal(rawAnn, &anns); err != nil || len(anns) > 0 {
							return nil, 0, unrep(fmt.Sprintf("response.output[%d].content[%d].annotations", oi, pi))
						}
					}
					if rawLP, ok := part["logprobs"]; ok && !isNull(rawLP) {
						var lps []json.RawMessage
						if err := json.Unmarshal(rawLP, &lps); err != nil || len(lps) > 0 {
							return nil, 0, unrep(fmt.Sprintf("response.output[%d].content[%d].logprobs", oi, pi))
						}
					}
					if s == "" {
						continue
					}
					out = append(out, TextItem{ID: ItemID(fmt.Sprintf("b%d", len(out))), Text: s})
				case "refusal":
					rawR, ok := part["refusal"]
					if !ok || isNull(rawR) {
						return nil, 0, bad
					}
					var s string
					if err := json.Unmarshal(rawR, &s); err != nil {
						return nil, 0, bad
					}
					if s == "" {
						continue
					}
					out = append(out, RefusalItem{ID: ItemID(fmt.Sprintf("b%d", len(out))), Text: s})
				default:
					return nil, 0, unrep(fmt.Sprintf("response.output[%d].content[%d].type", oi, pi))
				}
			}
		case "function_call":
			fc, rerr := chatResponseObject(raw, map[string]bool{
				"type": true, "id": true, "call_id": true, "name": true,
				"arguments": true, "status": true, "namespace": true,
				"caller": true, "async": true,
			}, fmt.Sprintf("response.output[%d]", oi))
			if rerr != nil {
				return nil, 0, rerr
			}
			for _, k := range []string{"namespace", "caller"} {
				if rawV, ok := fc[k]; ok && !isNull(rawV) {
					return nil, 0, unrep(fmt.Sprintf("response.output[%d].%s", oi, k))
				}
			}
			if rawA, ok := fc["async"]; ok && !isNull(rawA) {
				var bAsync bool
				if err := json.Unmarshal(rawA, &bAsync); err != nil {
					return nil, 0, bad
				}
				if bAsync {
					return nil, 0, unrep(fmt.Sprintf("response.output[%d].async", oi))
				}
			}
			var callID string
			if rawC, ok := fc["call_id"]; !ok || json.Unmarshal(rawC, &callID) != nil || strings.TrimSpace(callID) == "" {
				return nil, 0, bad
			}
			if err := validateCallID(callID, lim); err != nil {
				return nil, 0, bad
			}
			if seenCall[callID] {
				return nil, 0, &RouterError{Status: 502, Code: CodeUpstreamBad,
					Msg: "Upstream returned conflicting tool call identities"}
			}
			seenCall[callID] = true
			if rawI, ok := fc["id"]; ok && !isNull(rawI) {
				var iid string
				if err := json.Unmarshal(rawI, &iid); err != nil || strings.TrimSpace(iid) == "" {
					return nil, 0, bad
				}
				if seenItem[iid] {
					return nil, 0, &RouterError{Status: 502, Code: CodeUpstreamBad,
						Msg: "Upstream returned conflicting response item identities"}
				}
				seenItem[iid] = true
			}
			if rawSt, ok := fc["status"]; ok && !isNull(rawSt) {
				var st string
				if err := json.Unmarshal(rawSt, &st); err != nil ||
					(st != "completed" && st != "incomplete") {
					return nil, 0, bad
				}
			}
			var name string
			nameOK := false
			if rawN, ok := fc["name"]; ok && !isNull(rawN) {
				if err := json.Unmarshal(rawN, &name); err == nil && strings.TrimSpace(name) != "" {
					if validateToolName(name) == nil {
						nameOK = true
					}
				}
			}
			args := EmptyArgs()
			argsOK := nameOK
			if argsOK {
				if rawA, ok := fc["arguments"]; ok && !isNull(rawA) {
					var s string
					if err := json.Unmarshal(rawA, &s); err != nil {
						argsOK = false
					} else if strings.TrimSpace(s) != "" {
						if len(s) > lim.MaxArgsChars {
							return nil, 0, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call exceeds the size bound"}
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
				malformed++
				continue
			}
			totalArgs += len(args)
			if len(args) > lim.MaxArgsChars || totalArgs > lim.MaxStreamArgsChars {
				return nil, 0, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call exceeds the size bound"}
			}
			out = append(out, ToolCallItem{
				ID: ItemID(fmt.Sprintf("b%d", len(out))), CallID: CallID(callID), Name: name, Arguments: args,
			})
			if len(seenCall) > lim.MaxToolCalls {
				return nil, 0, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call count exceeds the size bound"}
			}
		case "custom_tool_call":
			cc, rerr := chatResponseObject(raw, map[string]bool{
				"type": true, "id": true, "call_id": true, "name": true,
				"input": true, "status": true, "namespace": true,
				"caller": true,
			}, fmt.Sprintf("response.output[%d]", oi))
			if rerr != nil {
				return nil, 0, rerr
			}
			for _, k := range []string{"namespace", "caller"} {
				if rawV, ok := cc[k]; ok && !isNull(rawV) {
					return nil, 0, unrep(fmt.Sprintf("response.output[%d].%s", oi, k))
				}
			}
			var callID string
			if rawC, ok := cc["call_id"]; !ok || json.Unmarshal(rawC, &callID) != nil || strings.TrimSpace(callID) == "" {
				return nil, 0, bad
			}
			if err := validateCallID(callID, lim); err != nil {
				return nil, 0, bad
			}
			if seenCall[callID] {
				return nil, 0, &RouterError{Status: 502, Code: CodeUpstreamBad,
					Msg: "Upstream returned conflicting tool call identities"}
			}
			seenCall[callID] = true
			if len(seenCall) > lim.MaxToolCalls {
				return nil, 0, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call count exceeds the size bound"}
			}
			// Provider item IDs are optional for complete responses. Track
			// duplicates separately from canonical block and call identity.
			if rawI, ok := cc["id"]; ok && !isNull(rawI) {
				itemID, valid := decodeUnicodeString(rawI)
				if !valid || validateCallID(itemID, lim) != nil {
					return nil, 0, bad
				}
				if seenItem[itemID] {
					return nil, 0, &RouterError{Status: 502, Code: CodeUpstreamBad,
						Msg: "Upstream returned conflicting response item identities"}
				}
				seenItem[itemID] = true
			}
			if rawSt, ok := cc["status"]; ok && !isNull(rawSt) {
				var st string
				if err := json.Unmarshal(rawSt, &st); err != nil ||
					(st != "completed" && st != "incomplete") {
					return nil, 0, bad
				}
			}
			var name string
			if rawN, ok := cc["name"]; ok && !isNull(rawN) {
				_ = json.Unmarshal(rawN, &name)
			}
			if len(name) > lim.MaxArgsChars {
				return nil, 0, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call exceeds the size bound"}
			}
			if validateToolName(name) != nil {
				malformed++
				continue
			}
			var input string
			inputOK := true
			if rawIn, ok := cc["input"]; ok && !isNull(rawIn) {
				input, inputOK = decodeUnicodeString(rawIn)
				if inputOK && len(input) > lim.MaxArgsChars {
					return nil, 0, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call exceeds the size bound"}
				}
			}
			if !inputOK {
				malformed++
				continue
			}
			totalArgs += len(input)
			if len(input) > lim.MaxArgsChars || totalArgs > lim.MaxStreamArgsChars {
				return nil, 0, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call exceeds the size bound"}
			}
			out = append(out, CustomCallItem{
				ID: ItemID(fmt.Sprintf("b%d", len(out))), CallID: CallID(callID), Name: name, Input: input,
			})
		case "reasoning":
			if !allowPrivateDrop {
				return nil, 0, unrep(fmt.Sprintf("response.output[%d].type", oi))
			}
			if rerr := decodeResponsesReasoningCompleteItem(raw, oi, lim, seenItem); rerr != nil {
				return nil, 0, rerr
			}
			// Dropped: no IR item, no malformed count, no continuation.
			continue
		default:
			return nil, 0, unrep(fmt.Sprintf("response.output[%d].type", oi))
		}
	}
	return out, malformed, nil
}
