// Gemini ingress FunctionResponsePart validation: closed, typed validation of
// the multimodal result payload before any loss is recorded.
//
// Source contracts (verified November 2026, primary Google REST):
//
//	FunctionResponse.response required object, FunctionResponsePart data
//	ONLY inlineData (FunctionResponseBlob) or fileData
//	(FunctionResponseFileData) with exactly one payload, plus optional
//	mediaProcessing enum:
//	https://ai.google.dev/api/generate-content (Content/FunctionResponse)
//	https://aistudio.google.com/api-reference/generate-content (FunctionResponsePart data oneof)
package protocol

import (
	"encoding/json"
	"fmt"
)

// gdecFunctionResponsePartKeys is the closed member set for one
// FunctionResponsePart: exactly one data payload plus optional
// mediaProcessing. Generic Part members (text/functionCall/...) are not
// part of this union and are rejected.
func gdecFunctionResponsePartKeys() map[string]bool {
	return map[string]bool{
		"inlineData": true, "fileData": true, "mediaProcessing": true,
	}
}

var gdecFunctionResponseMediaProcessing = map[string]bool{
	"MEDIA_PROCESSING_UNSPECIFIED": true, "STATIC": true, "AGENTIC": true,
}

// gdecValidateFunctionResponsePart validates one FunctionResponsePart
// element. Well-formed parts are eligible for diagnosed loss (the Chat
// destination has no multimodal result bridge); malformed types/values and
// unknown members are 400 in both modes, before any loss is recorded.
func gdecValidateFunctionResponsePart(path string, raw json.RawMessage) *RouterError {
	bad := func(msg string) *RouterError {
		return &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: msg}
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return bad(fmt.Sprintf("'%s' must be an object", path))
	}
	obj, rerr := gdecObject(m, gdecFunctionResponsePartKeys(), path)
	if rerr != nil {
		return &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: rerr.Msg}
	}
	_, hasInline := obj["inlineData"]
	if v, ok := obj["inlineData"]; ok && isNull(v) {
		hasInline = false
		_ = v
	}
	_, hasFile := obj["fileData"]
	if v, ok := obj["fileData"]; ok && isNull(v) {
		hasFile = false
		_ = v
	}
	hasInline = hasInline && !isNull(obj["inlineData"])
	hasFile = hasFile && !isNull(obj["fileData"])
	if hasInline == hasFile {
		return bad(fmt.Sprintf("'%s' must set exactly one of inlineData or fileData", path))
	}
	if hasInline {
		if rerr := gdecValidateFunctionResponseBlob(path+".inlineData", obj["inlineData"]); rerr != nil {
			return rerr
		}
	} else {
		if rerr := gdecValidateFunctionResponseFileData(path+".fileData", obj["fileData"]); rerr != nil {
			return rerr
		}
	}
	if v, ok := obj["mediaProcessing"]; ok && !isNull(v) {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return bad(fmt.Sprintf("'%s.mediaProcessing' must be a string", path))
		}
		if !gdecFunctionResponseMediaProcessing[s] {
			return bad(fmt.Sprintf("'%s.mediaProcessing' is not a media processing mode", path))
		}
	}
	return nil
}

// gdecValidateFunctionResponseBlob validates a FunctionResponseBlob:
// mimeType and data are required non-empty strings, displayName is an
// optional string. Unknown members are rejected.
func gdecValidateFunctionResponseBlob(path string, raw json.RawMessage) *RouterError {
	bad := func(msg string) *RouterError {
		return &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: msg}
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return bad(fmt.Sprintf("'%s' must be an object", path))
	}
	obj, rerr := gdecObject(m, map[string]bool{
		"mimeType": true, "data": true, "displayName": true,
	}, path)
	if rerr != nil {
		return &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: rerr.Msg}
	}
	for _, k := range []string{"mimeType", "data"} {
		v, ok := obj[k]
		if !ok || isNull(v) {
			return bad(fmt.Sprintf("'%s.%s' is required", path, k))
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil || s == "" {
			return bad(fmt.Sprintf("'%s.%s' must be a non-empty string", path, k))
		}
	}
	if v, ok := obj["displayName"]; ok && !isNull(v) {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return bad(fmt.Sprintf("'%s.displayName' must be a string", path))
		}
	}
	return nil
}

// gdecValidateFunctionResponseFileData validates a FunctionResponseFileData:
// fileUri is required, mimeType/displayName are optional strings.
// fileURI is accepted as a case-variant alias for fileUri; setting both is
// a collision. Unknown members are rejected.
func gdecValidateFunctionResponseFileData(path string, raw json.RawMessage) *RouterError {
	bad := func(msg string) *RouterError {
		return &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: msg}
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return bad(fmt.Sprintf("'%s' must be an object", path))
	}
	obj, rerr := gdecObject(m, map[string]bool{
		"mimeType": true, "fileUri": true, "fileURI": true, "displayName": true,
	}, path)
	if rerr != nil {
		return &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: rerr.Msg}
	}
	_, hasLower := obj["fileUri"]
	_, hasUpper := obj["fileURI"]
	hasLower = hasLower && !isNull(obj["fileUri"])
	hasUpper = hasUpper && !isNull(obj["fileURI"])
	if hasLower && hasUpper {
		return &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s' sets fileUri more than once", path)}
	}
	uriKey := ""
	if hasLower {
		uriKey = "fileUri"
	} else if hasUpper {
		uriKey = "fileURI"
	} else {
		return bad(fmt.Sprintf("'%s.fileUri' is required", path))
	}
	var uri string
	if err := json.Unmarshal(obj[uriKey], &uri); err != nil || uri == "" {
		return bad(fmt.Sprintf("'%s.%s' must be a non-empty string", path, uriKey))
	}
	for _, k := range []string{"mimeType", "displayName"} {
		if v, ok := obj[k]; ok && !isNull(v) {
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				return bad(fmt.Sprintf("'%s.%s' must be a string", path, k))
			}
			if k == "mimeType" && s == "" {
				return bad(fmt.Sprintf("'%s.%s' must be a non-empty string", path, k))
			}
		}
	}
	return nil
}
