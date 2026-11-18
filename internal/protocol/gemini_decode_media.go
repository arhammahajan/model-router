// Gemini inlineData is validated and classified before destination admission.
package protocol

import (
	"encoding/json"
	"fmt"
)

// gdecValidateInlineMedia validates user inlineData before loss policy or
// I/O and returns the closed ordered content variant to bridge. A nil
// block with no error is a well-formed but unlisted media type; the caller
// records the existing diagnosed media loss. named reports a present valid
// displayName (image/audio: always a loss; PDF: only meaningful with
// filenameFallback). filenameFallback is true only for a PDF whose
// displayName is present but unsafe, where the block already carries the
// default filename and the caller must diagnose the fallback.
func gdecValidateInlineMedia(path string, raw json.RawMessage, lim Limits) (block *ContentBlock, named bool, filenameFallback bool, err *RouterError) {
	bad := func(msg string) *RouterError {
		return &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: msg}
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, false, false, bad(fmt.Sprintf("'%s.inlineData' must be an object", path))
	}
	obj, rerr := gdecObject(m, gdecInlineDataKeys, path+".inlineData")
	if rerr != nil {
		return nil, false, false, &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: rerr.Msg}
	}
	mimeRaw, okMime := obj["mimeType"]
	dataRaw, okData := obj["data"]
	if !okMime || isNull(mimeRaw) || !okData || isNull(dataRaw) {
		return nil, false, false, bad(fmt.Sprintf("'%s.inlineData' must set mimeType and data", path))
	}
	var mime, data string
	if err := json.Unmarshal(mimeRaw, &mime); err != nil || mime == "" {
		return nil, false, false, bad(fmt.Sprintf("'%s.inlineData.mimeType' must be a non-empty string", path))
	}
	if err := json.Unmarshal(dataRaw, &data); err != nil || data == "" {
		return nil, false, false, bad(fmt.Sprintf("'%s.inlineData.data' must be a non-empty string", path))
	}
	mime, data, rerr = validateInlinePayload(path+".inlineData.mimeType", path+".inlineData.data", mime, data, lim)
	if rerr != nil {
		return nil, false, false, rerr
	}
	var name string
	if display, present := obj["displayName"]; present && !isNull(display) {
		var valid bool
		if name, valid = decodeUnicodeString(display); !valid || name == "" {
			return nil, false, false, bad(fmt.Sprintf("'%s.inlineData.displayName' must be a non-empty string", path))
		}
		named = true
	}
	if imageAllow[mime] {
		return &ContentBlock{Kind: ContentKindImage, Image: ImageContent{MIME: mime, Data: data}}, named, false, nil
	}
	if format, ok := audioFormatByMIME[mime]; ok {
		return &ContentBlock{Kind: ContentKindAudio, Audio: AudioContent{Format: format, Data: data}}, named, false, nil
	}
	if mime == pdfMIME {
		filename := pdfDefaultFilename
		if named && pdfSafeFilename(name) {
			filename = name
		} else if named {
			filenameFallback = true
		}
		return &ContentBlock{Kind: ContentKindFile, File: FileContent{MIME: mime, Data: data, Filename: filename}}, named, filenameFallback, nil
	}
	return nil, named, false, nil
}
