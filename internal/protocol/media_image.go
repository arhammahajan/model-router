// Shared neutral image validation for the bounded image verticals.
//
// Both directions bridge the same finite MIME set (image/png, image/jpeg,
// image/webp, image/gif) between Gemini inlineData and Chat image_url data
// URLs, preserving order and exact bytes. Neither direction owns the MIME
// allowlist, the MIME syntax rule, or the base64 canonicalization: this
// file is the single owner so the two decoders cannot drift.
//
// The Gemini->Chat audio (WAV/MP3 MIME variants to input_audio) and PDF
// (application/pdf to file) verticals reuse the same MIME syntax rule,
// base64 canonicalization, and per-field bound from this file, so all media
// directions share one validation/canonicalization policy. Image behavior
// is preserved exactly.
//
// Source (verified November 2026, primary docs only; grounded from
// HEAD:model_router/gemini.py):
//
//	GenerateContentRequest Content/Part inlineData{mimeType,data,displayName}:
//	https://ai.google.dev/api/generate-content
//	Chat Completions content array with image_url data URLs:
//	https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create
//	ProtoJSON bytes accept padded or unpadded standard or URL-safe base64;
//	Chat data URLs use standard padded base64:
//	https://protobuf.dev/programming-guides/json/
//	Data URLs:
//	https://www.rfc-editor.org/rfc/rfc2397
package protocol

import (
	"encoding/base64"
	"regexp"
	"strings"
)

// imageAllow is the finite common MIME set bridged in both directions.
var imageAllow = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/webp": true,
	"image/gif":  true,
}

// inlineMediaMIME is the shared media-type syntax rule.
var inlineMediaMIME = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.+-]*/[A-Za-z0-9][A-Za-z0-9.+-]*$`)

// canonicalInlineBase64 validates base64 (padded or unpadded standard or
// URL-safe, matching ProtoJSON bytes) and returns the standard padded
// encoding of the same bytes. Chat data URLs always use standard padded
// base64, so both directions canonicalize to it without decoding pixels.
func canonicalInlineBase64(data string) (string, bool) {
	if data == "" || strings.ContainsAny(data, "\r\n") {
		return "", false
	}
	encoding := base64.StdEncoding
	if strings.ContainsAny(data, "-_") {
		encoding = base64.URLEncoding
	}
	if !strings.ContainsRune(data, '=') {
		encoding = encoding.WithPadding(base64.NoPadding)
	}
	decoded, err := encoding.Strict().DecodeString(data)
	if err != nil {
		return "", false
	}
	return base64.StdEncoding.EncodeToString(decoded), true
}

func validateImagePayload(mimePath, dataPath, mime, data string, lim Limits) (*ImageContent, *RouterError) {
	mime, data, rerr := validateInlinePayload(mimePath, dataPath, mime, data, lim)
	if rerr != nil || !imageAllow[mime] {
		return nil, rerr
	}
	return &ImageContent{MIME: mime, Data: data}, nil
}

// validateInlinePayload checks the wire syntax and per-field bound before
// selecting a supported media kind.
func validateInlinePayload(mimePath, dataPath, mime, data string, lim Limits) (string, string, *RouterError) {
	bad := func(msg string) (string, string, *RouterError) {
		return "", "", &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: msg}
	}
	if !inlineMediaMIME.MatchString(mime) {
		return bad("'" + mimePath + "' must be a media type")
	}
	if len(data) > lim.MaxArgsChars {
		return bad("'" + dataPath + "' exceeds the size bound")
	}
	canon, valid := canonicalInlineBase64(data)
	if !valid {
		return bad("'" + dataPath + "' must be base64")
	}
	if len(canon) > lim.MaxArgsChars {
		return bad("'" + dataPath + "' exceeds the size bound")
	}
	return strings.ToLower(mime), canon, nil
}

// splitImageDataURL splits a data: URL into its MIME header and base64
// payload. isData is false for non-data (remote) URLs, which the caller
// keeps as the existing diagnosed loss without fetching. A data: URL
// without a comma separator, without a ;base64 marker, or with an empty
// MIME header is malformed (true).
func splitImageDataURL(url string) (mime, data string, isData, malformed bool) {
	const scheme = "data:"
	const encoding = ";base64"
	if len(url) < len(scheme) || !strings.EqualFold(url[:len(scheme)], scheme) {
		return "", "", false, false
	}
	comma := strings.Index(url, ",")
	if comma < 0 {
		return "", "", true, true
	}
	header := url[len(scheme):comma]
	payload := url[comma+1:]
	if payload == "" {
		return "", "", true, true
	}
	if len(header) <= len(encoding) || !strings.EqualFold(header[len(header)-len(encoding):], encoding) {
		return "", "", true, true
	}
	mime = header[:len(header)-len(encoding)]
	return mime, payload, true, false
}

// audioFormatByMIME is the finite WAV/MP3 MIME-variant set the Python
// bridge carries as Chat input_audio (HEAD:model_router/gemini.py _AUDIO_FORMATS).
// Keys are lowercased MIMEs; values are the Chat input_audio formats.
var audioFormatByMIME = map[string]string{
	"audio/wav":   "wav",
	"audio/x-wav": "wav",
	"audio/wave":  "wav",
	"audio/mpeg":  "mp3",
	"audio/mp3":   "mp3",
}

// pdfMIME is the single PDF MIME bridged to Chat file blocks.
const pdfMIME = "application/pdf"

// pdfDefaultFilename is the Chat filename when inlineData carries no safe
// displayName (matches the Python bridge default).
const pdfDefaultFilename = "document.pdf"

// pdfFilenameOK is the safe Chat filename rule, mirroring the Python
// bridge _FILENAME ([\w. -]{1,100}). \p{L}/\p{N} approximate Python's
// Unicode \w; path separators, control characters, and other shells are
// never preserved. An unsafe displayName falls back to the default with an
// explicit loss rather than leaking the value or failing the request.
var pdfFilenameOK = regexp.MustCompile(`^[\p{L}\p{N}_. \-]{1,100}$`)

func pdfSafeFilename(name string) bool {
	return pdfFilenameOK.MatchString(name)
}
