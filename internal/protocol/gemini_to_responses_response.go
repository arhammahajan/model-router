// Gemini -> Responses response translation: complete JSON responses via
// the shared Response IR. Complete-path source validation now lives once
// in DecodeGeminiResponse (response_gemini_decode.go); this file keeps
// only the thin compatibility wrapper.
package protocol

// TranslateGeminiJSONToResponses converts one Gemini generateContent
// response into a Responses API response object. Compatibility wrapper
// selecting (Gemini source decoder, Responses target encoder); it retains
// no pair semantics of its own.
func TranslateGeminiJSONToResponses(raw []byte, publicModel string, lim Limits) ([]byte, *RouterError) {
	resp, rerr := DecodeGeminiResponse(raw, lim)
	if rerr != nil {
		return nil, rerr
	}
	return EncodeResponsesResponse(resp, publicModel)
}
