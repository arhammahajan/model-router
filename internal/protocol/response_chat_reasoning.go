// Chat source private-reasoning compatibility: provider-private
// reasoning_content handling for the Chat complete and SSE decoders.
//
// Wire contract (observed gateway shape for deepseek-class Chat
// upstreams): a Chat assistant message may carry `reasoning_content`
// alongside `content`, and a Chat delta may carry
// `delta.reasoning_content` alongside `delta.content`. Measured native
// kimi-class Chat complete messages may instead carry `reasoning`
// (private string) alongside `reasoning_details` (array of
// {type:'reasoning.text', text, format, index} with detail text equal
// to reasoning). The fields are provider-private chain-of-thought: they
// must never be echoed, logged, projected to public text, or invented
// as continuation state. The decoders below drop them without
// retention; diagnostics name the field path only, never content. Only
// the measured complete-row `reasoning`/`reasoning_details` shape is
// admitted (bounded, closed type variant); native streaming already
// surfaces reasoning only as delta.reasoning_content, so no stream
// change applies.
//
// Policy (narrow propagation, destination-independent):
//   - Absent, null, or empty-string reasoning_content is exact: it
//     carries no reasoning and decodes identically in both modes.
//   - Malformed reasoning_content (non-string, non-null JSON) is a 502
//     "Upstream returned an invalid response" in both modes, before any
//     downstream commit.
//   - Substantive (nonempty string) reasoning_content is strict-502 with
//     a value-free `has no representation` path in the default
//     decoder, and an explicitly dropped compatibility in the policy
//     decoder when allowPrivateDrop is true. The strict constructor
//     and translators keep the 502; the WithPolicy constructor and
//     WithPolicy translators drop. The caller threads the provider
//     compatibility (lossy) as allowPrivateDrop; request-path losses
//     are untouched. Dropped bytes are never buffered, never emitted
//     as TextFragment, never added to usage, and never used to invent
//     tool or continuation state.
//   - The same lossy flag also drops the known gateway usage extensions
//     (usage.cost, usage.opencode) after bounded validation; see
//     response_chat_usage_gateway.go. Gateway bytes never enter the IR
//     and never affect authoritative totals.
//   - Terminal gating is unchanged: finish plus authoritative usage
//     plus [DONE] remain required; truncation or contradictory state
//     never becomes a synthetic success.
package protocol

import (
	"encoding/json"
	"fmt"
)

// validateChatReasoningContent classifies one reasoning_content value.
// ok is false only for malformed JSON types (non-string, non-null).
// empty reports absent/null/"" (exact, no reasoning). nonempty reports
// a substantive private string to drop-or-refuse per policy. Errors
// are value-free and never carry content.
func validateChatReasoningContent(raw json.RawMessage, what string) (empty, nonempty bool, rerr *RouterError) {
	if isNull(raw) {
		return true, false, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return false, false, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	}
	if s == "" {
		return true, false, nil
	}
	return false, true, nil
}

// validateChatReasoningDetails validates one Chat complete `reasoning_details`
// value against the measured native shape: absent/null/[] is exact (no
// reasoning); otherwise an array of {type:'reasoning.text', text, format,
// index} entries. Unknown members or unknown type variants refuse closed
// (value-free path) in both modes; malformed JSON types fail invalid in
// both modes. All text/format values are length-bounded and discarded
// after validation; nonempty reports a substantive private block to
// drop-or-refuse per policy. Errors never carry content.
func validateChatReasoningDetails(raw json.RawMessage, lim Limits, what string) (nonempty bool, rerr *RouterError) {
	bad := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	unrep := func(path string) *RouterError {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: fmt.Sprintf("Upstream response feature %q has no representation", path)}
	}
	if isNull(raw) {
		return false, nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return false, bad
	}
	if len(arr) == 0 {
		return false, nil
	}
	if len(arr) > lim.MaxToolCalls {
		return false, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream response exceeds the size bound"}
	}
	seenIdx := map[int]bool{}
	for i, eraw := range arr {
		ewhat := fmt.Sprintf("%s[%d]", what, i)
		entry, rerr := chatResponseObject(eraw, map[string]bool{
			"type": true, "text": true, "format": true, "index": true,
		}, ewhat)
		if rerr != nil {
			return false, rerr
		}
		rawType, ok := entry["type"]
		if !ok || isNull(rawType) {
			return false, bad
		}
		var typ string
		if err := json.Unmarshal(rawType, &typ); err != nil {
			return false, bad
		}
		if typ != "reasoning.text" {
			return false, unrep(ewhat + ".type")
		}
		rawText, ok := entry["text"]
		if !ok || isNull(rawText) {
			return false, bad
		}
		var text string
		if err := json.Unmarshal(rawText, &text); err != nil {
			return false, bad
		}
		if len(text) > lim.MaxArgsChars {
			return false, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream response exceeds the size bound"}
		}
		if text != "" {
			nonempty = true
		}
		if rawFormat, ok := entry["format"]; !ok || isNull(rawFormat) {
			return false, bad
		} else {
			var format string
			if err := json.Unmarshal(rawFormat, &format); err != nil {
				return false, bad
			}
			if len(format) > lim.MaxArgsChars {
				return false, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream response exceeds the size bound"}
			}
		}
		rawIdx, ok := entry["index"]
		if !ok || isNull(rawIdx) {
			return false, bad
		}
		idx, rerr := responseInt(rawIdx)
		if rerr != nil {
			return false, bad
		}
		if idx < 0 || idx >= lim.MaxStreamBlocks {
			return false, bad
		}
		if seenIdx[idx] {
			return false, bad
		}
		seenIdx[idx] = true
	}
	return nonempty, nil
}
