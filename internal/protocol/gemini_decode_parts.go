// Gemini ingress part validation: every known nested shape is validated
// before any thought loss, metadata drop, or media loss is recorded.
// Malformed types/values and unknown nested members are 400 in both modes.
package protocol

import (
	"encoding/json"
	"fmt"
)

var gdecInlineDataKeys = map[string]bool{"mimeType": true, "data": true, "displayName": true}
var gdecFileDataKeys = map[string]bool{"mimeType": true, "fileUri": true, "fileURI": true, "displayName": true}
var gdecMetaKeys = map[string]bool{
	"videoMetadata": true, "mediaResolution": true, "partMetadata": true,
	"mediaProcessing": true, "audioTranscription": true, "speechMetadata": true,
}

// gdecValidatePartShapes validates the types of all known members of one
// normalized (camelCase) part before the caller records any loss or
// continues past thought state. Unknown members are already rejected by
// gdecObject; here mistyped known members are 400.
func gdecValidatePartShapes(path string, part map[string]json.RawMessage) *RouterError {
	bad := func(msg string) *RouterError {
		return &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: msg}
	}
	if v, ok := part["text"]; ok && !isNull(v) {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return bad(fmt.Sprintf("'%s.text' must be a string", path))
		}
	}
	for _, k := range []string{"inlineData", "fileData"} {
		v, ok := part[k]
		if !ok || isNull(v) {
			continue
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(v, &obj); err != nil || obj == nil {
			return bad(fmt.Sprintf("'%s.%s' must be an object", path, k))
		}
		allowed := gdecInlineDataKeys
		if k == "fileData" {
			allowed = gdecFileDataKeys
		}
		norm, rerr := gdecObject(obj, allowed, path+"."+k)
		if rerr != nil {
			return &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: rerr.Msg}
		}
		for fk, fv := range norm {
			if isNull(fv) {
				continue
			}
			var s string
			if err := json.Unmarshal(fv, &s); err != nil || s == "" {
				return bad(fmt.Sprintf("'%s.%s.%s' must be a non-empty string", path, k, fk))
			}
		}
	}
	for _, k := range []string{"functionCall", "functionResponse", "executableCode", "codeExecutionResult", "toolCall", "toolResponse"} {
		v, ok := part[k]
		if !ok || isNull(v) {
			continue
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(v, &obj); err != nil || obj == nil {
			return bad(fmt.Sprintf("'%s.%s' must be an object", path, k))
		}
	}
	if v, ok := part["thought"]; ok && !isNull(v) {
		var b bool
		if err := json.Unmarshal(v, &b); err != nil {
			return bad(fmt.Sprintf("'%s.thought' must be bool", path))
		}
	}
	if v, ok := part["thoughtSignature"]; ok && !isNull(v) {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return bad(fmt.Sprintf("'%s.thoughtSignature' must be a string", path))
		}
	}
	for k := range gdecMetaKeys {
		v, ok := part[k]
		if !ok || isNull(v) {
			continue
		}
		if rerr := gdecValidatePartMetadata(path, k, v); rerr != nil {
			return rerr
		}
	}
	return nil
}
