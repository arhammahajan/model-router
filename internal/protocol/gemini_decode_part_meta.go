// Gemini ingress Part metadata validation: bounded, explicit validators for
// the known metadata members before any thought loss, metadata drop, or
// media loss is recorded. Malformed types/values and unknown nested members
// are 400 in both modes.
//
// Source contracts (verified November 2026, primary Google REST):
//
//	Part.videoMetadata/mediaResolution/mediaProcessing/audioTranscription/
//	speechMetadata/partMetadata shapes:
//	https://ai.google.dev/api/generate-content
package protocol

import (
	"encoding/json"
	"fmt"
)

var gdecPartMediaProcessingModes = map[string]bool{
	"MEDIA_PROCESSING_UNSPECIFIED": true, "STATIC": true, "AGENTIC": true,
}

var gdecPartMediaResolutionLevels = map[string]bool{
	"MEDIA_RESOLUTION_UNSPECIFIED": true,
	"MEDIA_RESOLUTION_LOW":         true,
	"MEDIA_RESOLUTION_MEDIUM":      true,
	"MEDIA_RESOLUTION_HIGH":        true,
	"MEDIA_RESOLUTION_ULTRA_HIGH":  true,
}

var gdecVideoContentLayers = map[string]bool{
	"CONTENT_LAYER_UNSPECIFIED": true, "FRAMES": true, "AUDIO": true, "TRANSCRIPT": true,
}

// gdecValidatePartMetadata validates one known Part metadata member. It is
// called from gdecValidatePartShapes before any loss is recorded.
func gdecValidatePartMetadata(path, key string, raw json.RawMessage) *RouterError {
	bad := func(msg string) *RouterError {
		return &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: msg}
	}
	switch key {
	case "mediaProcessing":
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return bad(fmt.Sprintf("'%s.%s' must be a string", path, key))
		}
		if !gdecPartMediaProcessingModes[s] {
			return bad(fmt.Sprintf("'%s.%s' is not a media processing mode", path, key))
		}
		return nil
	case "videoMetadata":
		return gdecValidateVideoMetadata(path+"."+key, raw)
	case "mediaResolution":
		return gdecValidatePartMediaResolution(path+"."+key, raw)
	case "partMetadata":
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
			return bad(fmt.Sprintf("'%s.%s' must be an object", path, key))
		}
		return nil
	case "audioTranscription":
		return gdecValidateAudioTranscription(path+"."+key, raw)
	case "speechMetadata":
		return gdecValidateSpeechMetadata(path+"."+key, raw)
	default:
		return bad(fmt.Sprintf("'%s.%s' is not a metadata member", path, key))
	}
}

func gdecValidateVideoMetadata(where string, raw json.RawMessage) *RouterError {
	bad := func(msg string) *RouterError {
		return &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: msg}
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return bad(fmt.Sprintf("'%s' must be an object", where))
	}
	obj, rerr := gdecObject(m, map[string]bool{
		"startOffset": true, "endOffset": true, "fps": true, "contentLayers": true,
	}, where)
	if rerr != nil {
		return &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: rerr.Msg}
	}
	for _, k := range []string{"startOffset", "endOffset"} {
		if v, ok := obj[k]; ok && !isNull(v) {
			var s string
			if err := json.Unmarshal(v, &s); err != nil || s == "" {
				return bad(fmt.Sprintf("'%s.%s' must be a non-empty string", where, k))
			}
		}
	}
	if v, ok := obj["fps"]; ok && !isNull(v) {
		f, rerr := gdecFiniteNumber(where+".fps", v)
		if rerr != nil || f <= 0 || f > 24 {
			return bad(fmt.Sprintf("'%s.fps' is outside the supported range", where))
		}
	}
	if v, ok := obj["contentLayers"]; ok && !isNull(v) {
		var arr []json.RawMessage
		if err := json.Unmarshal(v, &arr); err != nil {
			return bad(fmt.Sprintf("'%s.contentLayers' must be an array of strings", where))
		}
		for _, e := range arr {
			var s string
			if err := json.Unmarshal(e, &s); err != nil {
				return bad(fmt.Sprintf("'%s.contentLayers' must be an array of strings", where))
			}
			if !gdecVideoContentLayers[s] {
				return bad(fmt.Sprintf("'%s.contentLayers' is not a content layer", where))
			}
		}
	}
	return nil
}

func gdecValidatePartMediaResolution(where string, raw json.RawMessage) *RouterError {
	bad := func(msg string) *RouterError {
		return &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: msg}
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return bad(fmt.Sprintf("'%s' must be an object", where))
	}
	obj, rerr := gdecObject(m, map[string]bool{"level": true}, where)
	if rerr != nil {
		return &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: rerr.Msg}
	}
	if v, ok := obj["level"]; ok && !isNull(v) {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return bad(fmt.Sprintf("'%s.level' must be a string", where))
		}
		if !gdecPartMediaResolutionLevels[s] {
			return bad(fmt.Sprintf("'%s.level' is not a media resolution", where))
		}
	}

	return nil
}

func gdecValidateAudioTranscription(where string, raw json.RawMessage) *RouterError {
	bad := func(msg string) *RouterError {
		return &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: msg}
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return bad(fmt.Sprintf("'%s' must be an object", where))
	}
	obj, rerr := gdecObject(m, map[string]bool{
		"text": true, "speakerLabel": true, "words": true,
	}, where)
	if rerr != nil {
		return &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: rerr.Msg}
	}
	if v, ok := obj["text"]; ok && !isNull(v) {
		var s string
		if err := json.Unmarshal(v, &s); err != nil || s == "" {
			return bad(fmt.Sprintf("'%s.text' must be a non-empty string", where))
		}
	}
	for _, k := range []string{"speakerLabel"} {
		if v, ok := obj[k]; ok && !isNull(v) {
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				return bad(fmt.Sprintf("'%s.%s' must be a string", where, k))
			}
		}
	}
	if v, ok := obj["words"]; ok && !isNull(v) {
		var arr []map[string]json.RawMessage
		if err := json.Unmarshal(v, &arr); err != nil {
			return bad(fmt.Sprintf("'%s.words' must be an array", where))
		}
		for i, item := range arr {
			if item == nil {
				return bad(fmt.Sprintf("'%s.words' must contain objects", where))
			}
			ipath := fmt.Sprintf("%s.words[%d]", where, i)
			iobj, rerr := gdecObject(item, map[string]bool{
				"word": true, "startOffset": true, "endOffset": true,
			}, ipath)
			if rerr != nil {
				return &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: rerr.Msg}
			}
			wv, ok := iobj["word"]
			if !ok || isNull(wv) {
				return bad(fmt.Sprintf("'%s.word' is required", ipath))
			}
			var w string
			if err := json.Unmarshal(wv, &w); err != nil || w == "" {
				return bad(fmt.Sprintf("'%s.word' must be a non-empty string", ipath))
			}
			for _, k := range []string{"startOffset", "endOffset"} {
				if ev, ok := iobj[k]; ok && !isNull(ev) {
					var s string
					if err := json.Unmarshal(ev, &s); err != nil || s == "" {
						return bad(fmt.Sprintf("'%s.%s' must be a non-empty string", ipath, k))
					}
				}
			}
		}
	}
	return nil
}

func gdecValidateSpeechMetadata(where string, raw json.RawMessage) *RouterError {
	bad := func(msg string) *RouterError {
		return &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: msg}
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return bad(fmt.Sprintf("'%s' must be an object", where))
	}
	obj, rerr := gdecObject(m, map[string]bool{"speaker": true, "style": true}, where)
	if rerr != nil {
		return &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: rerr.Msg}
	}
	for _, k := range []string{"speaker", "style"} {
		if v, ok := obj[k]; ok && !isNull(v) {
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				return bad(fmt.Sprintf("'%s.%s' must be a string", where, k))
			}
		}
	}
	return nil
}
