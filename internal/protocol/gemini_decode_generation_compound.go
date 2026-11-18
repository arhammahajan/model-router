// Gemini ingress generation compound controls: bounded, explicit validators
// for speechConfig/imageConfig/mediaResolution/audioTimestamp and the
// toolConfig retrievalConfig. Well-formed values become diagnosed losses
// (strict refuses, lossy drops); malformed types/values and unknown nested
// members are 400 in both modes, before any loss is recorded.
//
// Source contracts (verified November 2026, primary Google REST):
//
//	GenerationConfig.mediaResolution is an ENUM string
//	(MEDIA_RESOLUTION_UNSPECIFIED/LOW/MEDIUM/HIGH), distinct from the
//	Part.mediaResolution object shape:
//	https://ai.google.dev/api/generate-content
//	SpeechConfig (languageCode/voiceConfig/multiSpeakerVoiceConfig,
//	voiceConfig mutually exclusive with multiSpeakerVoiceConfig),
//	ImageConfig (aspectRatio/imageSize enums):
//	https://ai.google.dev/api/generate-content
package protocol

import (
	"encoding/json"
	"fmt"
)

var gdecGenerationMediaResolutionModes = map[string]bool{
	"MEDIA_RESOLUTION_UNSPECIFIED": true,
	"MEDIA_RESOLUTION_LOW":         true,
	"MEDIA_RESOLUTION_MEDIUM":      true,
	"MEDIA_RESOLUTION_HIGH":        true,
}

var gdecImageAspectRatios = map[string]bool{
	"1:1": true, "1:4": true, "4:1": true, "1:8": true, "8:1": true,
	"2:3": true, "3:2": true, "3:4": true, "4:3": true, "4:5": true,
	"5:4": true, "9:16": true, "16:9": true, "21:9": true,
}

var gdecImageSizes = map[string]bool{
	"512": true, "1K": true, "2K": true, "4K": true,
}

// generationSpeechConfig validates generationConfig.speechConfig and records
// the diagnosed loss. Only the documented small subset is admitted.
func (d *gdecDecoder) generationSpeechConfig(where string, raw json.RawMessage) *RouterError {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return d.bad(fmt.Sprintf("'%s' must be an object", where))
	}
	obj, rerr := gdecObject(m, map[string]bool{
		"languageCode": true, "voiceConfig": true, "multiSpeakerVoiceConfig": true,
	}, where)
	if rerr != nil {
		return rerr
	}
	if v, ok := obj["languageCode"]; ok && !isNull(v) {
		var s string
		if err := json.Unmarshal(v, &s); err != nil || s == "" {
			return d.bad(fmt.Sprintf("'%s.languageCode' must be a non-empty string", where))
		}
	}
	hasVoice := false
	if v, ok := obj["voiceConfig"]; ok && !isNull(v) {
		hasVoice = true
		if rerr := gdecValidateVoiceConfig(where+".voiceConfig", v); rerr != nil {
			return rerr
		}
	}
	hasMulti := false
	if v, ok := obj["multiSpeakerVoiceConfig"]; ok && !isNull(v) {
		hasMulti = true
		if rerr := gdecValidateMultiSpeakerVoiceConfig(where+".multiSpeakerVoiceConfig", v); rerr != nil {
			return rerr
		}
	}
	if hasVoice && hasMulti {
		return d.bad(fmt.Sprintf("'%s' sets both voiceConfig and multiSpeakerVoiceConfig", where))
	}
	d.loseDrop(where, "this control has no equivalent on the destination")
	return nil
}

func gdecValidateVoiceConfig(where string, raw json.RawMessage) *RouterError {
	bad := func(msg string) *RouterError {
		return &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: msg}
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return bad(fmt.Sprintf("'%s' must be an object", where))
	}
	obj, rerr := gdecObject(m, map[string]bool{
		"voice": true, "prebuiltVoiceConfig": true,
	}, where)
	if rerr != nil {
		return &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: rerr.Msg}
	}
	if v, ok := obj["voice"]; ok && !isNull(v) {
		var s string
		if err := json.Unmarshal(v, &s); err != nil || s == "" {
			return bad(fmt.Sprintf("'%s.voice' must be a non-empty string", where))
		}
	}
	if v, ok := obj["prebuiltVoiceConfig"]; ok && !isNull(v) {
		var pm map[string]json.RawMessage
		if err := json.Unmarshal(v, &pm); err != nil || pm == nil {
			return bad(fmt.Sprintf("'%s.prebuiltVoiceConfig' must be an object", where))
		}
		pobj, rerr := gdecObject(pm, map[string]bool{"voiceName": true}, where+".prebuiltVoiceConfig")
		if rerr != nil {
			return &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: rerr.Msg}
		}
		if pv, ok := pobj["voiceName"]; ok && !isNull(pv) {
			var s string
			if err := json.Unmarshal(pv, &s); err != nil || s == "" {
				return bad(fmt.Sprintf("'%s.prebuiltVoiceConfig.voiceName' must be a non-empty string", where))
			}
		}
	}
	return nil
}

func gdecValidateMultiSpeakerVoiceConfig(where string, raw json.RawMessage) *RouterError {
	bad := func(msg string) *RouterError {
		return &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: msg}
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return bad(fmt.Sprintf("'%s' must be an object", where))
	}
	obj, rerr := gdecObject(m, map[string]bool{"speakerVoiceConfigs": true}, where)
	if rerr != nil {
		return &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: rerr.Msg}
	}
	v, ok := obj["speakerVoiceConfigs"]
	if !ok || isNull(v) {
		return bad(fmt.Sprintf("'%s.speakerVoiceConfigs' is required", where))
	}
	var arr []map[string]json.RawMessage
	if err := json.Unmarshal(v, &arr); err != nil || len(arr) == 0 {
		return bad(fmt.Sprintf("'%s.speakerVoiceConfigs' must be a non-empty array", where))
	}
	for i, item := range arr {
		if item == nil {
			return bad(fmt.Sprintf("'%s.speakerVoiceConfigs' must contain objects", where))
		}
		ipath := fmt.Sprintf("%s.speakerVoiceConfigs[%d]", where, i)
		iobj, rerr := gdecObject(item, map[string]bool{"speaker": true, "voiceConfig": true}, ipath)
		if rerr != nil {
			return &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: rerr.Msg}
		}
		sv, ok := iobj["speaker"]
		if !ok || isNull(sv) {
			return bad(fmt.Sprintf("'%s.speaker' is required", ipath))
		}
		var speaker string
		if err := json.Unmarshal(sv, &speaker); err != nil || speaker == "" {
			return bad(fmt.Sprintf("'%s.speaker' must be a non-empty string", ipath))
		}
		vv, ok := iobj["voiceConfig"]
		if !ok || isNull(vv) {
			return bad(fmt.Sprintf("'%s.voiceConfig' is required", ipath))
		}
		if rerr := gdecValidateVoiceConfig(ipath+".voiceConfig", vv); rerr != nil {
			return rerr
		}
	}
	return nil
}

// generationImageConfig validates generationConfig.imageConfig and records
// the diagnosed loss. Only the documented aspectRatio/imageSize enums are
// admitted.
func (d *gdecDecoder) generationImageConfig(where string, raw json.RawMessage) *RouterError {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return d.bad(fmt.Sprintf("'%s' must be an object", where))
	}
	obj, rerr := gdecObject(m, map[string]bool{
		"aspectRatio": true, "imageSize": true,
	}, where)
	if rerr != nil {
		return rerr
	}
	if v, ok := obj["aspectRatio"]; ok && !isNull(v) {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return d.bad(fmt.Sprintf("'%s.aspectRatio' must be a string", where))
		}
		if !gdecImageAspectRatios[s] {
			return d.bad(fmt.Sprintf("'%s.aspectRatio' is not an aspect ratio", where))
		}
	}
	if v, ok := obj["imageSize"]; ok && !isNull(v) {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return d.bad(fmt.Sprintf("'%s.imageSize' must be a string", where))
		}
		if !gdecImageSizes[s] {
			return d.bad(fmt.Sprintf("'%s.imageSize' is not an image size", where))
		}
	}
	d.loseDrop(where, "this control has no equivalent on the destination")
	return nil
}

// generationMediaResolution validates the GenerationConfig mediaResolution
// ENUM string (distinct from the Part.mediaResolution object) and records
// the diagnosed loss.
func (d *gdecDecoder) generationMediaResolution(where string, raw json.RawMessage) *RouterError {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return d.bad(fmt.Sprintf("'%s' must be a string", where))
	}
	if !gdecGenerationMediaResolutionModes[s] {
		return d.bad(fmt.Sprintf("'%s' is not a media resolution", where))
	}
	d.loseDrop(where, "this control has no equivalent on the destination")
	return nil
}

// generationAudioTimestamp validates the bool control before recording loss.
func (d *gdecDecoder) generationAudioTimestamp(where string, raw json.RawMessage) *RouterError {
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return d.bad(fmt.Sprintf("'%s' must be bool", where))
	}
	d.loseDrop(where, "this control has no equivalent on the destination")
	return nil
}

// retrievalConfig validates toolConfig.retrievalConfig (latLng/languageCode)
// before recording the diagnosed loss.
func (d *gdecDecoder) retrievalConfig(where string, raw json.RawMessage) *RouterError {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return d.bad(fmt.Sprintf("'%s' must be an object", where))
	}
	obj, rerr := gdecObject(m, map[string]bool{
		"latLng": true, "languageCode": true,
	}, where)
	if rerr != nil {
		return rerr
	}
	if v, ok := obj["latLng"]; ok && !isNull(v) {
		var lm map[string]json.RawMessage
		if err := json.Unmarshal(v, &lm); err != nil || lm == nil {
			return d.bad(fmt.Sprintf("'%s.latLng' must be an object", where))
		}
		lobj, rerr := gdecObject(lm, map[string]bool{
			"latitude": true, "longitude": true,
		}, where+".latLng")
		if rerr != nil {
			return rerr
		}
		if lat, ok := lobj["latitude"]; ok && !isNull(lat) {
			f, rerr := gdecFiniteNumber(where+".latLng.latitude", lat)
			if rerr != nil || f < -90 || f > 90 {
				return d.bad(fmt.Sprintf("'%s.latLng.latitude' is outside the supported range", where))
			}
		}
		if lng, ok := lobj["longitude"]; ok && !isNull(lng) {
			f, rerr := gdecFiniteNumber(where+".latLng.longitude", lng)
			if rerr != nil || f < -180 || f > 180 {
				return d.bad(fmt.Sprintf("'%s.latLng.longitude' is outside the supported range", where))
			}
		}
	}
	if v, ok := obj["languageCode"]; ok && !isNull(v) {
		var s string
		if err := json.Unmarshal(v, &s); err != nil || s == "" {
			return d.bad(fmt.Sprintf("'%s.languageCode' must be a non-empty string", where))
		}
	}
	d.loseDrop(where, "retrieval settings apply to Gemini grounding only")
	return nil
}
