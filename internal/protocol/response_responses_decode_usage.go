package protocol

import (
	"encoding/json"
	"fmt"
)

// responsesUsageObject decodes a Responses usage sub-object, accepting
// unknown null members as newer defaults while failing unknown non-null
// members with neutral diagnostics. This matches the pre-cutover
// r2cParseUsage/r2aParseUsage contract (both skipped null unknowns at
// usage level) and the documented decoder policy above. chatResponseObject
// cannot express this: it rejects every unknown member, which is the
// correct policy for output items and incomplete_details (all three
// legacy rows rejected those) but not for accounting sub-objects.
func responsesUsageObject(raw json.RawMessage, allowed map[string]bool, what string) (map[string]json.RawMessage, *RouterError) {
	bad := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, bad
	}
	out := make(map[string]json.RawMessage, len(obj))
	for k, v := range obj {
		if !allowed[k] {
			if isNull(v) {
				continue
			}
			return nil, &RouterError{Status: 502, Code: CodeUpstreamBad,
				Msg: fmt.Sprintf("Upstream response feature %q has no representation", what+"."+k)}
		}
		out[k] = v
	}
	return out, nil
}

// parseResponsesIRUsage validates one Responses usage object and returns
// canonical terminal accounting. input_tokens is the verbatim prompt total
// (never target-subtracted); cached_tokens and cache_write_tokens are
// overlapping CacheRead/CacheCreation subsets; reasoning_tokens is scalar
// Reasoning. Totals must be exact with overflow checks. Unknown null
// members are accepted as newer defaults; unknown non-null members and
// any nonzero unmapped detail fail closed with neutral diagnostics.
func parseResponsesIRUsage(raw json.RawMessage, allowBillingDrop bool) (ResponseUsage, *RouterError) {
	bad := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	unrep := func(what string) *RouterError {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: fmt.Sprintf("Upstream response feature %q has no representation", what)}
	}
	obj, rerr := responsesUsageObject(raw, map[string]bool{
		"input_tokens": true, "input_tokens_details": true,
		"output_tokens": true, "output_tokens_details": true,
		"total_tokens": true, "cost": true, "opencode": true,
	}, "response.usage")
	if rerr != nil {
		return ResponseUsage{}, rerr
	}
	if rerr := validateGatewayBillingMetadata(obj, "response.usage", allowBillingDrop); rerr != nil {
		return ResponseUsage{}, rerr
	}
	var input, output, total int
	for _, k := range []string{"input_tokens", "output_tokens", "total_tokens"} {
		rawN, ok := obj[k]
		if !ok || isNull(rawN) {
			return ResponseUsage{}, bad
		}
		n, rerr := responseInt(rawN)
		if rerr != nil {
			return ResponseUsage{}, rerr
		}
		switch k {
		case "input_tokens":
			input = n
		case "output_tokens":
			output = n
		case "total_tokens":
			total = n
		}
	}
	var cached, creation, reasoning *int
	if rawDet, ok := obj["input_tokens_details"]; ok && !isNull(rawDet) {
		det, rerr := responsesUsageObject(rawDet, map[string]bool{
			"cached_tokens": true, "cache_write_tokens": true,
			"text_tokens": true, "audio_tokens": true, "image_tokens": true,
		}, "response.usage.input_tokens_details")
		if rerr != nil {
			return ResponseUsage{}, rerr
		}
		for k, v := range det {
			if isNull(v) {
				continue
			}
			n, rerr := responseInt(v)
			if rerr != nil {
				return ResponseUsage{}, bad
			}
			switch k {
			case "cached_tokens":
				c := n
				cached = &c
			case "cache_write_tokens":
				c := n
				creation = &c
			default:
				if n != 0 {
					return ResponseUsage{}, unrep("response.usage.input_tokens_details." + k)
				}
			}
		}
	}
	if rawDet, ok := obj["output_tokens_details"]; ok && !isNull(rawDet) {
		det, rerr := responsesUsageObject(rawDet, map[string]bool{
			"reasoning_tokens": true, "audio_tokens": true, "text_tokens": true,
			"accepted_prediction_tokens": true, "rejected_prediction_tokens": true,
		}, "response.usage.output_tokens_details")
		if rerr != nil {
			return ResponseUsage{}, rerr
		}
		for k, v := range det {
			if isNull(v) {
				continue
			}
			n, rerr := responseInt(v)
			if rerr != nil {
				return ResponseUsage{}, bad
			}
			switch k {
			case "reasoning_tokens":
				c := n
				reasoning = &c
			default:
				if n != 0 {
					return ResponseUsage{}, unrep("response.usage.output_tokens_details." + k)
				}
			}
		}
	}
	usage, rerr := NewTerminalUsage(DialectResponses, input, output, total)
	if rerr != nil {
		return ResponseUsage{}, rerr
	}
	if usage, rerr = usage.WithCache(creation, cached); rerr != nil {
		return ResponseUsage{}, rerr
	}
	// Reasoning subset is enforced by WithReasoning; nil stays absent so
	// presence (explicit zero vs missing) is preserved in the bucket.
	if usage, rerr = usage.WithReasoning(reasoning); rerr != nil {
		return ResponseUsage{}, rerr
	}
	return usage, nil
}
