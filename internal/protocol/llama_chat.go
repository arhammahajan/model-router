// Llama native Chat applier: byte-level rendering of reasoning_effort for
// a confirmed generated LlamaCpp server. It is pure: no validation,
// capability gates, Generation decoding, loss headers or errors. Unknown
// names, "max", non-string, null and empty efforts are consumed without
// adding controls. Strata, Unobserved and ordinary providers never reach
// it and pass through raw.
//
// Wire rules (section 1), all via RawMessage member edits that preserve
// exact subvalues and bigints:
//   - absent: return the original bytes unchanged.
//   - none with absent OR null kwargs: kwargs={enable_thinking:false}.
//   - none with an object kwargs: preserve every explicit key/value
//     (including an explicit true or null) and add false only if absent.
//   - none with a non-object/non-null kwargs: leave it untouched.
//   - mapped level: thinking_budget_tokens defaults only if the member is
//     absent; any existing member (including null) wins.
package protocol

import (
	"bytes"
	"encoding/json"

	"github.com/arhammahajan/router/internal/capability"
)

// ApplyLlamaNative consumes the raw reasoning_effort member for a
// confirmed LlamaCpp server and renders the native thinking controls
// directly. It returns the outbound body and whether a present effort was
// consumed. Absent effort returns the input bytes identically; malformed
// bodies pass through untouched. It never fails.
func ApplyLlamaNative(body []byte) ([]byte, bool) {
	var top map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&top); err != nil || top == nil {
		return body, false
	}
	raw, ok := top["reasoning_effort"]
	if !ok {
		return body, false
	}
	delete(top, "reasoning_effort")
	var level string
	if err := json.Unmarshal(raw, &level); err != nil || level == "" {
		// Null, non-string, empty or malformed: consumed, no control.
		return remarshalNative(top, body)
	}
	applyNativeLevel(level, top)
	return remarshalNative(top, body)
}

// applyNativeLevel renders the shared literal mapper result. Native policy
// consumes unknown names without a diagnostic; translated policy reports them.
func applyNativeLevel(level string, top map[string]json.RawMessage) {
	mapped, ok := capability.MapLlamaEffort(level)
	if !ok {
		return
	}
	switch mapped.Kind {
	case capability.LlamaMapDisable:
		applyNativeNone(top)
	case capability.LlamaMapBudget:
		if _, has := top["thinking_budget_tokens"]; !has {
			raw, err := json.Marshal(mapped.Budget)
			if err == nil {
				top["thinking_budget_tokens"] = raw
			}
		}
	}
}

// applyNativeNone merges the disable-thinking default into
// chat_template_kwargs: absent OR null becomes an object; an existing
// object wins key-by-key (an explicit true or null is preserved, false is
// added only when the key is absent); a non-object/non-null stays.
func applyNativeNone(top map[string]json.RawMessage) {
	raw, has := top["chat_template_kwargs"]
	if !has || isNull(raw) {
		top["chat_template_kwargs"] = json.RawMessage(`{"enable_thinking":false}`)
		return
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return
	}
	if _, has := obj["enable_thinking"]; !has {
		obj["enable_thinking"] = json.RawMessage(`false`)
	}
	merged, err := json.Marshal(obj)
	if err != nil {
		return
	}
	top["chat_template_kwargs"] = merged
}

// remarshalNative re-encodes the member map. Values stay exact RawMessages
// (no float64 round-trip); only key order/whitespace may normalize.
func remarshalNative(top map[string]json.RawMessage, fallback []byte) ([]byte, bool) {
	out, err := json.Marshal(top)
	if err != nil {
		return fallback, true
	}
	return out, true
}
