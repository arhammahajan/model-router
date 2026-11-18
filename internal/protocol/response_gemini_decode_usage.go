// Gemini source codec: usage and safety-rating validation for the
// complete-response row. Companion to response_gemini_decode.go, which
// owns the envelope, blocked path, finish and content decoding. Split
// only to keep each file under the 700-line module bound; the validator
// is one logical unit rooted at DecodeGeminiResponse.
package protocol

import (
	"encoding/json"
	"fmt"
)

// geminiParseUsage validates one Gemini usageMetadata object and
// returns canonical terminal accounting under the strict policy. Per
// the REST contract, promptTokenCount already includes the cached
// share (cachedContentTokenCount), and totalTokenCount equals prompt +
// tool-use prompt + thoughts + candidates. Only generations with zero
// thought and tool-use accounting are translatable today (no reviewed
// target bucket exists for them), so nonzero shares fail closed and
// total must equal prompt + candidates exactly here. The cached share
// maps to CacheRead with a subset check (cache <= prompt); there is
// no double counting because the canonical Prompt carries
// promptTokenCount verbatim and each target encoder applies its own
// bucket math. Totals and subsets use exact integer arithmetic with
// overflow checks. Unknown null members are accepted as newer
// defaults; unknown non-null members fail closed. Explicit zeros are
// carried as present because zero is representable everywhere.
func geminiParseUsage(raw json.RawMessage) (ResponseUsage, *RouterError) {
	return geminiParseUsageWithPolicy(raw, false)
}

// geminiParseUsageWithPolicy validates one Gemini usageMetadata object
// with narrow lossy-extension policy. allowDrop keeps every strict
// failure identical; when true it additionally accepts, after bounded
// validation, the routine provider/gateway members that no strict
// target bucket renders:
//
//   - cost and opencode: gateway billing members, validated and
//     dropped by the shared validateGatewayBillingMetadata exactly as
//     the Chat/Anthropic/Responses rows drop them.
//   - promptTokensDetails/candidatesTokensDetails/cacheTokensDetails/
//     toolUsePromptTokensDetails TEXT-modality entries: pure
//     breakdowns of totals already carried by the authoritative
//     scalars, so dropping them changes no accounting.
//   - nonzero thoughtsTokenCount: Gemini 4 default thinking reports
//     reasoning-token accounting on non-trivial responses. The share
//     is folded into the canonical completion (Gemini candidates
//     exclude thoughts; Chat and Responses completions include
//     reasoning, and total == prompt + candidates + thoughts on the
//     wire) and carried in the Reasoning bucket, whose reviewed
//     target renderings already exist. Strict keeps the refusal.
//
// Non-TEXT modality entries (media token shares) and malformed entries
// fail closed in both modes, mirroring the Chat row's audio/image
// share failures. Authoritative provider counters stay untouched:
// nothing here relies on gateway accounting.
func geminiParseUsageWithPolicy(raw json.RawMessage, allowDrop bool) (ResponseUsage, *RouterError) {
	bad := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	unrep := func(what string) *RouterError {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: fmt.Sprintf("Upstream response feature %q has no representation", what)}
	}
	obj, rerr := geminiResponseObject(raw, map[string]bool{
		"promptTokenCount": true, "candidatesTokenCount": true, "totalTokenCount": true,
		"cachedContentTokenCount": true, "thoughtsTokenCount": true, "toolUsePromptTokenCount": true,
		"promptTokensDetails": true, "cacheTokensDetails": true, "candidatesTokensDetails": true,
		"toolUsePromptTokensDetails": true, "trafficType": true,
		"cost": true, "opencode": true,
	}, "response.usageMetadata")
	if rerr != nil {
		return ResponseUsage{}, rerr
	}
	if rerr := validateGatewayBillingMetadata(obj, "response.usageMetadata", allowDrop); rerr != nil {
		return ResponseUsage{}, rerr
	}
	var prompt, completion, total int
	for _, k := range []string{"promptTokenCount", "candidatesTokenCount", "totalTokenCount"} {
		rawN, ok := obj[k]
		if !ok || isNull(rawN) {
			return ResponseUsage{}, bad
		}
		n, rerr := responseInt(rawN)
		if rerr != nil {
			return ResponseUsage{}, rerr
		}
		switch k {
		case "promptTokenCount":
			prompt = n
		case "candidatesTokenCount":
			completion = n
		case "totalTokenCount":
			total = n
		}
	}
	var cacheRead, thoughtsZero, toolUse *int
	reasoningShare := 0
	hasReasoningShare := false
	if rawC, ok := obj["cachedContentTokenCount"]; ok && !isNull(rawC) {
		n, rerr := responseInt(rawC)
		if rerr != nil {
			return ResponseUsage{}, rerr
		}
		cacheRead = &n
	}
	for _, k := range []string{"thoughtsTokenCount", "toolUsePromptTokenCount"} {
		if rawV, ok := obj[k]; ok && !isNull(rawV) {
			n, rerr := responseInt(rawV)
			if rerr != nil {
				return ResponseUsage{}, rerr
			}
			if k == "toolUsePromptTokenCount" {
				// No destination maps tool-use prompt accounting
				// today; nonzero fails closed in both modes.
				if n != 0 {
					return ResponseUsage{}, unrep("response.usageMetadata." + k)
				}
				toolUse = &n
				continue
			}
			if n == 0 {
				// Explicit zero is carried as present.
				thoughtsZero = &n
				continue
			}
			// Nonzero thoughts: Gemini 4 default thinking reports
			// reasoning-token accounting on non-trivial responses. The
			// reviewed mapping folds the share into the canonical
			// completion (Gemini candidates exclude thoughts; Chat and
			// Responses completions include reasoning) and carries it in
			// the Reasoning bucket, whose target renderings already
			// exist (Chat completion_tokens_details.reasoning_tokens,
			// Responses output_tokens_details.reasoning_tokens, Gemini
			// thoughtsTokenCount; the Anthropic target drops the
			// breakdown under the lossy projection). Strict providers
			// keep the refusal.
			if !allowDrop {
				return ResponseUsage{}, unrep("response.usageMetadata." + k)
			}
			reasoningShare, hasReasoningShare = n, true
		}
	}
	if hasReasoningShare {
		folded := completion + reasoningShare
		if folded < completion {
			return ResponseUsage{}, bad
		}
		completion = folded
	}
	// Modality token breakdowns: pure decompositions of the totals the
	// authoritative scalars already carry. TEXT entries are dropped in
	// lossy mode after bounded validation; every non-TEXT modality is a
	// media share with no reviewed mapping and fails closed in both
	// modes, mirroring the Chat row's audio/image share failures.
	// Malformed entries are invalid upstream data in both modes.
	const maxModalityDetailEntries = 64
	for _, k := range []string{"promptTokensDetails", "cacheTokensDetails", "candidatesTokensDetails", "toolUsePromptTokensDetails"} {
		if rawV, ok := obj[k]; ok && !isNull(rawV) {
			var arr []json.RawMessage
			if err := json.Unmarshal(rawV, &arr); err != nil {
				return ResponseUsage{}, bad
			}
			if len(arr) > maxModalityDetailEntries {
				return ResponseUsage{}, bad
			}
			if len(arr) == 0 {
				continue
			}
			what := "response.usageMetadata." + k
			for ei, eraw := range arr {
				entry, rerr := geminiResponseObject(eraw, map[string]bool{
					"modality": true, "tokenCount": true,
				}, fmt.Sprintf("%s[%d]", what, ei))
				if rerr != nil {
					return ResponseUsage{}, rerr
				}
				var modality string
				if rawM, ok := entry["modality"]; ok && !isNull(rawM) {
					if err := json.Unmarshal(rawM, &modality); err != nil || modality == "" {
						return ResponseUsage{}, bad
					}
				}
				if rawC, ok := entry["tokenCount"]; ok && !isNull(rawC) {
					if _, rerr := responseInt(rawC); rerr != nil {
						return ResponseUsage{}, bad
					}
				}
				if modality != "TEXT" {
					return ResponseUsage{}, unrep(fmt.Sprintf("%s[%d].modality", what, ei))
				}
			}
			if !allowDrop {
				return ResponseUsage{}, unrep(what)
			}
		}
	}
	if rawT, ok := obj["trafficType"]; ok && !isNull(rawT) {
		var s string
		if err := json.Unmarshal(rawT, &s); err != nil || s == "" {
			return ResponseUsage{}, bad
		}
	}
	usage, rerr := NewTerminalUsage(DialectGemini, prompt, completion, total)
	if rerr != nil {
		return ResponseUsage{}, rerr
	}
	usage, rerr = usage.WithCache(nil, cacheRead)
	if rerr != nil {
		return ResponseUsage{}, rerr
	}
	usage, rerr = usage.WithThoughts(thoughtsZero)
	if rerr != nil {
		return ResponseUsage{}, rerr
	}
	if hasReasoningShare {
		share := reasoningShare
		usage, rerr = usage.WithReasoning(&share)
		if rerr != nil {
			return ResponseUsage{}, rerr
		}
	}
	usage, rerr = usage.WithToolUsePrompt(toolUse)
	if rerr != nil {
		return ResponseUsage{}, rerr
	}
	return usage, nil
}

// geminiAcceptEmptyRatings accepts documented empty safety rating
// defaults. Any populated rating (blocked or probability-bearing)
// carries filter semantics and fails closed rather than being
// stripped, exactly as in all three old paths. category/severity must
// be nonempty strings when present.
func geminiAcceptEmptyRatings(raw json.RawMessage) *RouterError {
	bad := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	unrep := func(what string) *RouterError {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: fmt.Sprintf("Upstream response feature %q has no representation", what)}
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return bad
	}
	for i, rraw := range arr {
		rating, rerr := geminiResponseObject(rraw, map[string]bool{
			"category": true, "probability": true, "blocked": true, "severity": true,
		}, fmt.Sprintf("response.candidates[0].safetyRatings[%d]", i))
		if rerr != nil {
			return rerr
		}
		if rawB, ok := rating["blocked"]; ok && !isNull(rawB) {
			var b bool
			if err := json.Unmarshal(rawB, &b); err != nil {
				return bad
			}
			if b {
				return unrep("response.candidates[0].safetyRatings")
			}
		}
		if rawP, ok := rating["probability"]; ok && !isNull(rawP) {
			var p string
			if err := json.Unmarshal(rawP, &p); err != nil {
				return bad
			}
			switch p {
			case "NEGLIGIBLE", "HARM_PROBABILITY_UNSPECIFIED":
			default:
				return unrep("response.candidates[0].safetyRatings")
			}
		}
		for _, k := range []string{"category", "severity"} {
			if rawV, ok := rating[k]; ok && !isNull(rawV) {
				var s string
				if err := json.Unmarshal(rawV, &s); err != nil || s == "" {
					return bad
				}
			}
		}
	}
	return nil
}
