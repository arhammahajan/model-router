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
// returns canonical terminal accounting. Per the REST contract,
// promptTokenCount already includes the cached share
// (cachedContentTokenCount), and totalTokenCount equals prompt +
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
	}, "response.usageMetadata")
	if rerr != nil {
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
	var cacheRead, thoughts, toolUse *int
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
			if n != 0 {
				return ResponseUsage{}, unrep("response.usageMetadata." + k)
			}
			if k == "thoughtsTokenCount" {
				thoughts = &n
			} else {
				toolUse = &n
			}
		}
	}
	for _, k := range []string{"promptTokensDetails", "cacheTokensDetails", "candidatesTokensDetails", "toolUsePromptTokensDetails"} {
		if rawV, ok := obj[k]; ok && !isNull(rawV) {
			var arr []json.RawMessage
			if err := json.Unmarshal(rawV, &arr); err != nil {
				return ResponseUsage{}, bad
			}
			if len(arr) > 0 {
				return ResponseUsage{}, unrep("response.usageMetadata." + k)
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
	usage, rerr = usage.WithThoughts(thoughts)
	if rerr != nil {
		return ResponseUsage{}, rerr
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
