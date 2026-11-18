// Gemini -> Chat response translation: complete JSON responses via the
// shared Response IR. Complete-path source validation now lives once in
// DecodeGeminiResponse (response_gemini_decode.go); this file keeps only
// the thin compatibility wrapper plus the neutral helpers still used by
// the canonical stream and complete decoders (filter predicate, args
// compaction).
package protocol

import (
	"bytes"
	"encoding/json"
)

// TranslateGeminiJSONToChat converts one Gemini generateContent response
// into a Chat Completions response. Compatibility wrapper selecting
// (Gemini source decoder, Chat target encoder); it retains no pair
// semantics of its own.
func TranslateGeminiJSONToChat(raw []byte, publicModel string, lim Limits) ([]byte, *RouterError) {
	resp, rerr := DecodeGeminiResponse(raw, lim)
	if rerr != nil {
		return nil, rerr
	}
	return EncodeChatResponse(resp, publicModel)
}

// g2cIsFilterFinish reports whether a finish reason maps to content_filter.

func g2cIsFilterFinish(finish string) bool {
	switch finish {
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII",
		"IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT", "IMAGE_RECITATION", "IMAGE_OTHER",
		"LANGUAGE", "NO_IMAGE", "IMAGE_NO_IMAGE", "SPII_MODEL", "BLOCKLIST_MODEL":
		return true
	}
	return false
}

// g2cCompactArgsObject validates a Gemini functionCall args value: it must
// be a JSON object (never a scalar or array) and is preserved as compact
// raw JSON without a float64 round-trip, so large integers and number
// literals survive exactly.

func g2cCompactArgsObject(what string, raw json.RawMessage, lim Limits) (json.RawMessage, *RouterError) {
	bad := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	if len(raw) > lim.MaxArgsChars {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call exceeds the size bound"}
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, bad
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return nil, bad
	}
	// Confirm the compacted value decodes to an object (guards `{}`
	//-shaped scalars and trailing garbage the compactor tolerates).
	var v any
	dec := json.NewDecoder(bytes.NewReader(compact.Bytes()))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, bad
	}
	if _, isObj := v.(map[string]any); !isObj {
		return nil, bad
	}
	_ = what
	return json.RawMessage(compact.Bytes()), nil
}
