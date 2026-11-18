// Gemini ingress for the Responses upstream: destination-specific decode
// of a GenerateContentRequest body into the shared semantic IR.
//
// Boundary sketch (Gemini -> Responses bridge):
//
//	ingress:  Gemini GenerateContentRequest --DecodeGeminiForResponses--> IR
//	         --EncodeResponsesRequest--> Responses upstream request.
//	         Never composed via Chat JSON or a Chat translator.
//	egress:   Responses upstream response --TranslateResponsesJSONToGemini-->
//	         Gemini GenerateContentResponse (or the SSE / JSON-array
//	         translators in responses_to_gemini_stream*.go).
//
// This file holds the Responses destination policy. The shared Gemini
// source validator (gemini_decode*.go) performs the decode; only the
// destination wording and the stopSequences plan below are
// Responses-specific:
//
//   - stopSequences: validated for shape (array of non-empty strings,
//     malformed in both modes), then diagnosed as a loss and dropped.
//     The reviewed Responses scope has no stop-sequence contract, and
//     nothing is forwarded.
//   - function strict: a Gemini FunctionDeclaration carries no strict
//     guarantee. Omitted strict is therefore the known non-strict default
//     and EncodeResponsesRequest emits explicit strict:false for it with
//     no invented loss. (There is no explicit-true member to weaken.)
//   - temperature/top_p/maxOutputTokens: portable ranges (0..2, 0..1,
//     positive int) map exactly; remaining generationConfig members keep
//     their shared validation and become losses with Responses wording.
//   - toolConfig VALIDATED mode and multi-name allowedFunctionNames keep
//     the shared narrowing behaviour (automatic calling / narrowed tool
//     list with an approximated loss).
//
// Null policy mirrors DecodeGeminiRequest: explicit null on an optional
// field means absent, except for nested boolean controls, where null is a
// type error. Malformed known fields and unknown nested members are
// validation errors in both strict and lossy modes.
//
// Source: GenerateContentRequest (model/method live in the URL, never the
// body) https://ai.google.dev/api/generate-content; Responses create
// https://developers.openai.com/api/reference/resources/responses/methods/create
package protocol

// geminiDestination selects the small typed destination policy applied on
// top of the shared Gemini source validation. Chat keeps stopSequences
// and Chat-worded losses; Responses validates stopSequences and then
// drops them, with Responses-worded losses; Anthropic keeps stopSequences
// (Anthropic stop_sequences is native) with Anthropic-worded losses.
type geminiDestination int

const (
	geminiDestChat geminiDestination = iota
	geminiDestResponses
	geminiDestAnthropic
)

// DecodeGeminiForResponses decodes raw JSON Gemini GenerateContentRequest
// bytes into the IR plus the Responses-destination loss plan. publicModel
// is the model from the parsed request URL and stream is the
// method-derived mode (streamGenerateContent). It performs no I/O.
//
// The shared Gemini source validator performs the decode under this
// destination's policy (stop cap, wording); every admitted source field
// is represented, diagnosed, or rejected through the shared validator.
func DecodeGeminiForResponses(raw []byte, publicModel string, stream bool, lim Limits) (*Generation, []Loss, *RouterError) {
	return decodeGeminiRequest(raw, publicModel, stream, lim, geminiDestResponses)
}

// geminiEncoderControlLoss names the destination encoder that carries no
// equivalent for a source control. The Chat string is preserved exactly;
// the Responses and Anthropic strings are the same claim for those
// encoders.
func (d *gdecDecoder) geminiEncoderControlLoss(control string) string {
	switch d.dest {
	case geminiDestResponses:
		return "the Responses encoder carries no " + control + " control"
	case geminiDestAnthropic:
		return "the Anthropic encoder carries no " + control + " control"
	default:
		return "the Go Chat encoder carries no " + control + " control"
	}
}

// geminiNoRepresentationLoss names a source semantic with no destination
// representation. The Chat string is preserved exactly.
func (d *gdecDecoder) geminiNoRepresentationLoss(noun string) string {
	switch d.dest {
	case geminiDestResponses:
		return noun + " have no Responses representation"
	case geminiDestAnthropic:
		return noun + " have no Anthropic representation"
	default:
		return noun + " have no Chat representation"
	}
}
