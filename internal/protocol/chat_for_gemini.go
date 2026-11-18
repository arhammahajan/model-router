// Chat ingress for Gemini upstream: narrow typed decode of a Chat
// Completions request into the shared Generation IR with a
// Gemini-destination loss plan. It performs no I/O and reads no environment.
//
// The shared Chat source validator in chat_decode.go (decodeChatSource)
// performs the decode under the Gemini destination policy; this file keeps
// only the Gemini-specific tool decoder below. The destination differences
// versus DecodeChatRequest live in that policy:
//
//   - stop: Chat stop sequences map exactly onto Gemini
//     generationConfig.stopSequences (camelCase per the REST contract).
//     Gemini supports at most 5 entries; more than 5 has no exact mapping
//     and is diagnosed as a loss (never silently truncated).
//   - extra_body.top_k: diagnosed as a loss, never forwarded as Gemini topK.
//     The Chat extra_body extension is an Anthropic-native control; carrying
//     it to Gemini topK would be capability inference.
//   - store: Gemini generateContent is stateless with no store parameter.
//     Explicit false matches exactly; true is a diagnosed loss.
//   - tools strict: Gemini FunctionDeclaration has no strict guarantee.
//     Omitted or explicit false is exact (no guarantee requested); explicit
//     true is a diagnosed loss, never silently weakened.
//   - parallel_tool_calls=false: no Gemini mapping, diagnosed as a loss;
//     true or omitted is exact (Gemini default allows parallel calls).
//   - temperature/top_p/max_tokens: exact when portable (0..2, 0..1,
//     positive int to maxOutputTokens). n>1 is a loss (candidateCount>1
//     has no reviewed streaming mapping and is never requested here).
//   - metadata: same-protocol route; explicit null means absent, any present
//     value is rejected.
//
// Null policy mirrors the sibling decoders: explicit null on an optional
// field means absent, except for `stream` (explicit bool) and nested bool
// controls (null is a type error). Malformed known fields and unknown
// nested members are validation errors in both modes.
//
// Source: GenerateContentRequest contents/systemInstruction/tools/
// toolConfig/generationConfig, FunctionDeclaration
// parameters/parametersJsonSchema mutual exclusivity, and stopSequences
// (at most 5): https://ai.google.dev/api/generate-content
package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// DecodeChatForGemini decodes a Chat Completions request body into the IR
// plus the Gemini-destination loss plan. It performs no I/O.
// DecodeChatForGemini decodes a Chat Completions request body into the IR
// plus the Gemini-destination loss plan. It performs no I/O.
//
// The shared Chat source validator in chat_decode.go performs the decode
// under this destination's policy (stop cap, store statelessness,
// parallel-disable loss, top_k/strict losses); only the tool decoder below
// is destination-specific.
func DecodeChatForGemini(raw []byte, lim Limits) (*Generation, []Loss, *RouterError) {
	return decodeChatSource(raw, lim, chatSourcePolicy{
		dest:            chatDestGemini,
		decodeTool:      c2gDecodeTool,
		validateLoss:    validateLossField,
		storeLossReason: "stateless Gemini translation carries no stored state",
	})
}

// c2gDecodeTool validates one ordinary function tool for the Gemini
// destination. Omitted or explicit false strict is exact (Gemini carries no
// strict guarantee, so no guarantee requested means no loss); explicit true
// claims an unreviewed guarantee and is a diagnosed loss, never silently
// weakened. The returned ToolDef carries no Strict slot: the Gemini wire
// has no strict member.
func c2gDecodeTool(i int, item map[string]json.RawMessage, lim Limits, lose func(string, string)) (*ToolDef, *RouterError) {
	for k := range item {
		switch k {
		case "type", "function", "custom":
		default:
			return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: tools[%d].%s", i, k)}
		}
	}
	var typ string
	if raw, ok := item["type"]; ok && !isNull(raw) {
		if err := json.Unmarshal(raw, &typ); err != nil {
			return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: tools[%d].type", i)}
		}
	}
	// Freeform custom tools are emulated as string-valued functions on the
	// Gemini destination (see responsesCustomEmuReason); the caller records
	// the approximation loss. Ordinary function tools keep the existing
	// strict-loss contract.
	if typ == "custom" {
		name, desc, format, rerr := parseChatCustomSpec(i, item, lim)
		if rerr != nil {
			return nil, rerr
		}
		return &ToolDef{Kind: ToolKindCustom, Name: name, Description: desc, Format: format}, nil
	}
	// Milestone scope supports ordinary JSON-schema function tools only.
	if typ != "" && typ != "function" {
		return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
			Msg: fmt.Sprintf("Tool type %q has no Chat bridge mapping; ordinary function tools only", typ)}
	}
	rawFn, ok := item["function"]
	if !ok || isNull(rawFn) {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'tools[%d].function' is required", i)}
	}
	var fn map[string]json.RawMessage
	if err := json.Unmarshal(rawFn, &fn); err != nil {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'tools[%d].function' must be an object", i)}
	}
	var name string
	if rawN, ok := fn["name"]; ok && !isNull(rawN) {
		_ = json.Unmarshal(rawN, &name)
	}
	if err := validateToolName(name); err != nil {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'tools[%d].function.name' %s", i, err.Error())}
	}
	var desc string
	if rawD, ok := fn["description"]; ok && !isNull(rawD) {
		if err := json.Unmarshal(rawD, &desc); err != nil {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'tools[%d].function.description' must be a string", i)}
		}
	}
	schema := json.RawMessage(`{"type":"object"}`)
	if rawP, ok := fn["parameters"]; ok && !isNull(rawP) {
		if len(rawP) > lim.MaxArgsChars {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'tools[%d].function.parameters' exceeds the size bound", i)}
		}
		var v any
		dec := json.NewDecoder(bytes.NewReader(rawP))
		dec.UseNumber()
		if err := dec.Decode(&v); err != nil {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'tools[%d].function.parameters' must be valid JSON", i)}
		}
		if _, isObj := v.(map[string]any); !isObj {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'tools[%d].function.parameters' must be a JSON object", i)}
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, rawP); err != nil {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'tools[%d].function.parameters' must be valid JSON", i)}
		}
		schema = append(json.RawMessage(nil), compact.Bytes()...)
	}
	for k := range fn {
		switch k {
		case "name", "description", "parameters", "strict":
		default:
			return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: tools[%d].function.%s", i, k)}
		}
	}
	if rawS, ok := fn["strict"]; ok && !isNull(rawS) {
		var b bool
		if err := json.Unmarshal(rawS, &b); err != nil {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'tools[%d].function.strict' must be bool", i)}
		}
		if b {
			lose(fmt.Sprintf("tools[%d].function.strict", i), "Gemini carries no strict schema guarantee")
		}
	}
	return &ToolDef{Name: name, Description: desc, Schema: schema}, nil
}
