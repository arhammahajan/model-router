// Canonical top-level Responses `reasoning` echo validator.
//
// Source contract (verified against
// https://developers.openai.com/api/reference/resources/responses/methods/create):
// the top-level `reasoning` object on a response envelope carries request
// echo configuration. Its `summary` (and deprecated `generate_summary`) is an
// optional enum string "auto" | "concise" | "detailed" | null. This is
// different from an OUTPUT `reasoning` item, whose `summary` is an array of
// content parts. Output reasoning items remain unmappable and must still fail
// in the output decoders; this helper only validates the top-level echo and
// never materializes chain-of-thought.
//
// Known members: effort/summary/generate_summary/context/mode. Enum controls
// must use their documented strings or null; mode also accepts arbitrary
// strings per the create reference. Unknown null members are accepted as
// newer defaults; unknown non-null members fail. All failures are 502
// CodeUpstreamBad and value-free: they name the field path, never content.
//
// Destination-neutral: messages name `response.reasoning[.field]` with no
// destination framing, so Chat, Anthropic and Gemini envelopes share it.
package protocol

import (
	"encoding/json"
	"fmt"
)

// validateResponsesReasoningEcho validates the top-level `reasoning` echo
// value from a Responses response envelope. Null is accepted (absent/default).
func validateResponsesReasoningEcho(raw json.RawMessage) *RouterError {
	invalid := func(field string) *RouterError {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: fmt.Sprintf("Upstream response feature %q is invalid", field)}
	}
	if isNull(raw) {
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return invalid("response.reasoning")
	}
	for k, v := range obj {
		switch k {
		case "mode":
			if isNull(v) {
				continue
			}
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				return invalid("response.reasoning.mode")
			}
		case "effort", "summary", "generate_summary", "context":
			if isNull(v) {
				continue
			}
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				return invalid("response.reasoning." + k)
			}
			valid := false
			switch k {
			case "effort":
				switch s {
				case "none", "minimal", "low", "medium", "high", "xhigh", "max":
					valid = true
				}
			case "context":
				valid = s == "auto" || s == "current_turn" || s == "all_turns"
			default:
				valid = s == "auto" || s == "concise" || s == "detailed"
			}
			if !valid {
				return invalid("response.reasoning." + k)
			}
		default:
			if isNull(v) {
				continue
			}
			return invalid("response.reasoning." + k)
		}
	}
	return nil
}
