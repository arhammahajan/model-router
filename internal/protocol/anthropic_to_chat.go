// Anthropic -> Chat response translation: complete JSON responses and the
// bounded incremental SSE event path with a typed stream state machine.
//
// The guiding rule for unrepresentable upstream features (citations,
// reasoning deltas, unknown stop reasons, unknown accounting fields) is:
// fail the translation with HTTP 502, in both strict and lossy modes. A
// truncated or contradictory stream never produces a success marker. Errors
// are value-free: they name field paths, never content or secrets.
package protocol

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
)

// newChatID mints a nondeterministic completion ID. Conformance normalizes it.
func newChatID(stream bool) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	prefix := "chatcmpl-"
	if stream {
		prefix = "chatcmpl-"
	}
	return prefix + hex.EncodeToString(b[:])
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// responseInt parses a non-negative JSON integer literal. Fractions,
// exponents, quoted strings and overflow all fail: usage accounting and
// block indices must be exact. (encoding/json would otherwise coerce a JSON
// string into a json.Number.)
func responseInt(raw json.RawMessage) (int, *RouterError) {
	bad := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	lit := strings.TrimSpace(string(raw))
	if lit == "" {
		return 0, bad
	}
	for i := 0; i < len(lit); i++ {
		if lit[i] < '0' || lit[i] > '9' {
			return 0, bad
		}
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, bad
	}
	i, err := n.Int64()
	if err != nil || i < 0 {
		return 0, bad
	}
	return int(i), nil
}

func responseString(obj map[string]json.RawMessage, key string) (string, *RouterError) {
	raw, ok := obj[key]
	if !ok {
		return "", &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil || s == "" {
		return "", &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	}
	return s, nil
}

// TranslateAnthropicJSON converts one Anthropic Messages response into a Chat
// Completions response for the public model name. Compatibility wrapper
// selecting (Anthropic source decoder, Chat target encoder); it retains no
// pair semantics of its own. Strict: gateway billing metadata fails closed.
func TranslateAnthropicJSON(raw []byte, publicModel string, lim Limits) ([]byte, *RouterError) {
	return TranslateAnthropicJSONWithPolicy(raw, publicModel, lim, false)
}

// TranslateAnthropicJSONWithPolicy converts one Anthropic response with
// narrow gateway-billing policy: allowDrop drops validated
// usage.cost/usage.opencode, false keeps the strict 502.
func TranslateAnthropicJSONWithPolicy(raw []byte, publicModel string, lim Limits, allowDrop bool) ([]byte, *RouterError) {
	resp, rerr := DecodeAnthropicResponseWithPolicy(raw, lim, allowDrop)
	if rerr != nil {
		return nil, rerr
	}
	return EncodeChatResponse(resp, publicModel)
}

// --- Streaming ---

// SSETranslator converts an Anthropic SSE stream into Chat SSE chunks via
// the shared pipeline (SSE framing, Anthropic source stream decoder,
// shared lifecycle, Chat target event encoder). It retains no parsing
// state: every Translate call builds fresh pipeline objects, so one
// translator value is reusable across generations.
//
// The pipeline stops reading upstream as soon as the source terminal is
// observed: Translate returns promptly after message_stop with all blocks
// closed and terminal usage validated. Trailing bytes after the terminal
// are the same HTTP body's unread remainder (body closed, never another
// generation). Truncation, missing usage, duplicate markers, or
// unrepresentable events fail without a synthetic success marker.
type SSETranslator struct {
	PublicModel  string
	IncludeUsage bool
	Limits       Limits
	// Plan restores emulated Chat customs on tool blocks. Nil or empty
	// restores nothing; native streams stay raw.
	Plan *ToolPlan
	// AllowGatewayBillingDrop drops validated gateway billing metadata
	// (usage.cost, usage.opencode) when true (lossy compatibility).
	// False keeps the strict 502.
	AllowGatewayBillingDrop bool
}

// Translate reads Anthropic SSE from r and writes Chat SSE frames to w,
// calling flush after each frame. It returns nil only after a complete
// message_stop with all blocks closed; any truncation or unrepresentable
// event is an error.
func (t *SSETranslator) Translate(r io.Reader, w io.Writer, flush func()) *RouterError {
	frames := NewSSEFrameReader(r, t.Limits)
	var dec StreamDecoder = NewAnthropicStreamDecoderWithPolicy(t.Limits, t.AllowGatewayBillingDrop)
	if t.Plan != nil {
		dec = t.Plan.WrapDecoder(dec, t.Limits)
	}
	enc := NewChatEventEncoder(t.PublicModel, t.IncludeUsage, t.Limits)
	return TranslateStream(frames, dec, enc, w, flush, t.Limits, DialectAnthropic)
}

func streamIndex(raw json.RawMessage) (int, *RouterError) {
	bad := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	lit := strings.TrimSpace(string(raw))
	if lit == "" {
		return 0, bad
	}
	for i := 0; i < len(lit); i++ {
		if lit[i] < '0' || lit[i] > '9' {
			return 0, bad
		}
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, bad
	}
	i, err := n.Int64()
	if err != nil || i < 0 {
		return 0, bad
	}
	return int(i), nil
}

func mustRaw(m map[string]json.RawMessage) json.RawMessage {
	out, _ := json.Marshal(m)
	return out
}
