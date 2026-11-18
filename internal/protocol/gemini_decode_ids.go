// Gemini ingress call-identity policy: opaque supplied ids are preserved
// exactly; missing ids mint collision-safe request-scoped gdec-N names.
// See gemini_decode.go for the entry point.
//
// Source contracts (verified November 2026):
//
//	Gemini FunctionCall/FunctionResponse id optional string:
//	https://ai.google.dev/gemini-api/docs/function-calling
//	Chat function tool call id Required[str] (no function-name grammar):
//	https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create
//
// The cross-dialect [A-Za-z0-9_-]{1,64} grammar applies to function *names*
// only; it is never applied to opaque call ids.
package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
)

// gdecValidateSuppliedID enforces source validation for an explicit id:
// non-string is rejected by the caller; here empty/whitespace-only and
// oversized values are 400. Valid opaque ids are returned unchanged.
func gdecValidateSuppliedID(original, what string, lim Limits) (string, *RouterError) {
	if strings.TrimSpace(original) == "" {
		return "", &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s' must be a non-empty string", what)}
	}
	if len(original) > lim.MaxFrameBytes {
		return "", &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s' exceeds the size bound", what)}
	}
	return original, nil
}

// gdecCollectSuppliedIDs reserves every explicit functionCall and
// functionResponse id in the request body before any compatibility id is
// minted, so minted gdec-N names never collide with a supplied one.
func gdecCollectSuppliedIDs(rawContents json.RawMessage) map[string]bool {
	reserved := map[string]bool{}
	var arr []map[string]json.RawMessage
	if err := json.Unmarshal(rawContents, &arr); err != nil {
		return reserved
	}
	for _, item := range arr {
		var parts []map[string]json.RawMessage
		// Role objects carry parts; tolerate both spellings via raw scan.
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(mustRaw(item), &obj); err != nil {
			continue
		}
		var rawParts json.RawMessage
		for k, v := range obj {
			if gdecSnake(k) == "parts" {
				rawParts = v
			}
		}
		if len(rawParts) == 0 || isNull(rawParts) {
			continue
		}
		if err := json.Unmarshal(rawParts, &parts); err != nil {
			continue
		}
		for _, p := range parts {
			for k, v := range p {
				camel := k
				if strings.Contains(k, "_") {
					camel = gdecSnake(k)
				}
				if camel != "functionCall" && camel != "functionResponse" {
					continue
				}
				if isNull(v) {
					continue
				}
				var inner map[string]json.RawMessage
				if err := json.Unmarshal(v, &inner); err != nil {
					continue
				}
				for ik, iv := range inner {
					c2 := ik
					if strings.Contains(ik, "_") {
						c2 = gdecSnake(ik)
					}
					if c2 != "id" || isNull(iv) {
						continue
					}
					var s string
					if err := json.Unmarshal(iv, &s); err != nil {
						continue
					}
					if s != "" {
						reserved[s] = true
					}
				}
			}
		}
		_ = parts
	}
	return reserved
}

// gdecMintID returns a fresh request-scoped compatibility identity that
// avoids every supplied id and every previously minted one.
func (d *gdecDecoder) gdecMintID() string {
	for {
		d.nextID++
		id := fmt.Sprintf("gdec-%d", d.nextID)
		if d.reservedIDs != nil && d.reservedIDs[id] {
			continue
		}
		if d.usedIDs != nil && d.usedIDs[id] {
			continue
		}
		if d.usedIDs == nil {
			d.usedIDs = map[string]bool{}
		}
		d.usedIDs[id] = true
		return id
	}
}

// gdecPreserveID validates an explicit supplied id and preserves it exactly.
func (d *gdecDecoder) gdecPreserveID(original, what string) (string, *RouterError) {
	preserved, rerr := gdecValidateSuppliedID(original, what, d.lim)
	if rerr != nil {
		return "", rerr
	}
	if d.usedIDs == nil {
		d.usedIDs = map[string]bool{}
	}
	d.usedIDs[preserved] = true
	return preserved, nil
}
