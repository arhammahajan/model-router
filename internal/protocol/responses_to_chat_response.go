// Responses -> Chat response translation: complete JSON responses decode
// once via DecodeResponsesResponse and render via EncodeChatResponse. The
// streaming path lives in chat_responses_stream.go.
//
// Guiding rule for unrepresentable upstream features (reasoning items,
// audio/image/service tools, annotations, logprobs, unknown usage members,
// unknown stop reasons, unknown item types) is: fail the translation with
// HTTP 502. A truncated or contradictory response never produces a success
// marker. Errors are value-free: they name field paths, never content.
package protocol

import (
	"encoding/json"
	"strings"
)

// TranslateResponsesJSONToChat converts one Responses response into a Chat
// Completions response for the public model name.
//
// Compatibility wrapper selecting (Responses source decoder,
// Chat target encoder); it retains no pair semantics of its own.
//
// Strict default: provider-private reasoning OUTPUT items fail closed.
// Use TranslateResponsesJSONToChatWithPolicy for the lossy drop.
func TranslateResponsesJSONToChat(raw []byte, publicModel string, lim Limits) ([]byte, *RouterError) {
	resp, rerr := DecodeResponsesResponse(raw, lim)
	if rerr != nil {
		return nil, rerr
	}
	return EncodeChatResponse(resp, publicModel)
}

// TranslateResponsesJSONToChatWithPolicy converts one Responses response
// with narrow private-reasoning policy: allowPrivateDrop drops the
// measured reasoning shape, false keeps the strict 502.
func TranslateResponsesJSONToChatWithPolicy(raw []byte, publicModel string, lim Limits, allowPrivateDrop bool) ([]byte, *RouterError) {
	resp, rerr := DecodeResponsesResponseWithPolicy(raw, lim, allowPrivateDrop)
	if rerr != nil {
		return nil, rerr
	}
	return EncodeChatResponse(resp, publicModel)
}

// r2cValidateCreatedAt accepts integer or float unix timestamps (the official
// sample uses 1746989954.0). Quoted strings, bools and non-finite numbers
// fail. Shared by the complete decoder and the Responses stream decoder.
func r2cValidateCreatedAt(raw json.RawMessage) *RouterError {
	bad := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	lit := strings.TrimSpace(string(raw))
	if lit == "" || lit[0] == '"' || lit[0] == '\'' {
		return bad
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return bad
	}
	f, err := n.Float64()
	if err != nil {
		return bad
	}
	if f != f || f > 1e18 || f < -1e18 {
		return bad
	}
	// NaN/Inf check without importing math: Float64 already rejects Inf?
	// Guard overflow via string check.
	if lit == "NaN" || lit == "Infinity" || lit == "-Infinity" {
		return bad
	}
	return nil
}
