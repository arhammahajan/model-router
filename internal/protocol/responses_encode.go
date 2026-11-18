// Chat -> Responses request encoding. One plan is encoded once; the IR
// produced by DecodeChatForResponses is rendered as an OpenAI Responses
// request body for the configured upstream model.
//
// Wire contract notes (reviewed against the official Responses reference,
// https://developers.openai.com/api/reference/resources/responses/methods/create,
// https://developers.openai.com/api/docs/guides/function-calling and
// https://developers.openai.com/api/docs/guides/migrate-to-responses):
//   - input items use message content with input_text parts for text and
//     function_call / function_call_output items preserving call_id order.
//   - ordinary flat function tools: {"type":"function","name",...} with an
//     explicit "strict" flag. A supplied ToolDef.Strict (explicit source
//     true/false, currently from the Anthropic ingress decoder) is carried
//     exactly. A nil Strict means the source omitted strict; the encoder
//     emits false to preserve the historical Chat->Responses non-strict
//     semantics (Chat omits strict for non-strict while Responses would
//     otherwise attempt strict normalization). Neither decoder populates
//     Strict without diagnosing the default difference, so an explicit false
//     here is never a silent weakening.
//   - tool_choice wire strings "none"/"auto"/"required" and the named object
//     {"type":"function","name":...} (Responses form, not the Chat nested
//     function form).
//   - stateless storage is forced explicitly with "store":false. A source
//     store=true is diagnosed as a loss at decode time; it is never silently
//     changed here.
//   - no conversation/previous_response_id, no background, no extra_body.
package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
)

// EncodeResponsesRequest renders the IR plus the planned named effort as a
// Responses request body. The upstream model ID replaces the public ingress
// model.
func EncodeResponsesRequest(gen *Generation, planned PlannedEffort, upstreamModel string) ([]byte, *RouterError) {
	input, rerr := rencBuildInput(gen)
	if rerr != nil {
		return nil, rerr
	}
	body := map[string]any{
		"model": upstreamModel,
		"input": input,
		// Stateless translation: never reuse upstream conversation state.
		// Forced explicitly so the wire never depends on an upstream
		// default; a source store=true is a diagnosed loss, not a silent
		// change (see DecodeChatForResponses).
		"store":  false,
		"stream": gen.Stream,
	}
	if gen.MaxTokens != nil {
		body["max_output_tokens"] = *gen.MaxTokens
	}
	if gen.Temperature != nil {
		body["temperature"] = *gen.Temperature
	}
	if gen.TopP != nil {
		body["top_p"] = *gen.TopP
	}
	if len(gen.Tools) > 0 {
		tools := make([]any, 0, len(gen.Tools))
		for _, t := range gen.Tools {
			if t.IsCustom() {
				entry := map[string]any{
					"type": "custom",
					"name": t.Name,
				}
				if t.Description != "" {
					entry["description"] = t.Description
				}
				if t.Format != nil {
					if t.Format.Type == "grammar" {
						entry["format"] = map[string]any{
							"type": "grammar", "syntax": t.Format.Syntax,
							"definition": t.Format.Definition,
						}
					} else {
						entry["format"] = map[string]any{"type": "text"}
					}
				}
				tools = append(tools, entry)
				continue
			}
			strict := false
			if t.Strict != nil {
				strict = *t.Strict
			}
			entry := map[string]any{
				"type":   "function",
				"name":   t.Name,
				"strict": strict,
				"parameters": json.RawMessage(
					append(json.RawMessage(nil), t.Schema...),
				),
			}
			if t.Description != "" {
				entry["description"] = t.Description
			}
			tools = append(tools, entry)
		}
		body["tools"] = tools
		choice := rencToolChoice(gen)
		if choice != nil {
			body["tool_choice"] = choice
		}
	} else if gen.Choice != nil && (gen.Choice.Mode == "specific" || gen.Choice.Mode == "any") {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: "'tool_choice' requires 'tools'"}
	}
	if gen.ParallelToolCalls != nil {
		body["parallel_tool_calls"] = *gen.ParallelToolCalls
	}
	if planned.IsPresent() {
		// The Responses encoder admits only named-effort/absence variants.
		// Budget/off targets are Chat-native controls; reaching this
		// encoder with one is an internal misuse, never a silent drop.
		effort, ok := planned.Effort()
		if !ok {
			return nil, &RouterError{Status: 500, Code: "encoding_error",
				Msg: "failed to encode upstream request"}
		}
		body["reasoning"] = map[string]any{"effort": effort}
	}
	out, err := json.Marshal(body)
	if err != nil {
		return nil, &RouterError{Status: 500, Code: "encoding_error", Msg: "failed to encode upstream request"}
	}
	return out, nil
}

// rencToolChoice maps the IR choice to the Responses tool_choice wire
// contract: "none", "auto" and "required" are bare strings; named choices
// are flat objects ({"type":"function","name":...} or
// {"type":"custom","name":...}), unlike the Chat nested function form.
func rencToolChoice(g *Generation) any {
	if g.Choice == nil {
		return nil
	}
	switch g.Choice.Mode {
	case "none":
		return "none"
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "specific":
		if g.Choice.IsCustomChoice() {
			return map[string]any{"type": "custom", "name": g.Choice.Name}
		}
		return map[string]any{"type": "function", "name": g.Choice.Name}
	default:
		return nil
	}
}

// rencBuildInput renders IR messages as Responses input items. Text becomes
// input_text parts; assistant function calls become function_call items and
// custom calls become custom_tool_call items carrying the Chat call ID as
// call_id; tool results become function_call_output or custom_tool_call_output
// by call identity. Order is preserved; shapes the decoder never produces
// are rejected.
func rencBuildInput(gen *Generation) ([]any, *RouterError) {
	customCalls := customToolResultCallIDs(gen.Messages)
	out := make([]any, 0, len(gen.Messages)+2)
	for i, m := range gen.Messages {
		if m.Role != RoleUser && len(m.Blocks) > 0 {
			return nil, &RouterError{Status: 500, Code: "encoding_error", Msg: "Ordered media requires a user message"}
		}
		switch m.Role {
		case RoleSystem, RoleDeveloper, RoleUser:
			if len(m.ToolCalls) > 0 || len(m.Results) > 0 {
				return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'messages[%d]' carries tool state on a %s message", i, m.Role)}
			}
			parts := make([]any, 0, len(m.Texts))
			if len(m.Blocks) > 0 {
				for _, b := range m.Blocks {
					switch b.Kind {
					case ContentKindText:
						parts = append(parts, map[string]any{"type": "input_text", "text": b.Text})
					case ContentKindImage:
						parts = append(parts, map[string]any{"type": "input_image", "detail": "auto",
							"image_url": "data:" + b.Image.MIME + ";base64," + b.Image.Data})
					case ContentKindFile:
						parts = append(parts, map[string]any{"type": "input_file", "filename": b.File.Filename,
							"file_data": "data:" + b.File.MIME + ";base64," + b.File.Data})
					default:
						return nil, &RouterError{Status: 500, Code: "encoding_error", Msg: "Unsupported media for Responses encoding"}
					}
				}
			} else {
				for _, t := range m.Texts {
					parts = append(parts, map[string]any{"type": "input_text", "text": t.Text})
				}
			}
			out = append(out, map[string]any{"role": m.Role, "content": parts})
		case RoleAssistant:
			if len(m.Results) > 0 {
				return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'messages[%d]' carries tool results on an assistant message", i)}
			}
			if len(m.Texts) > 0 {
				var sb strings.Builder
				for _, t := range m.Texts {
					sb.WriteString(t.Text)
				}
				// History assistant text as an input message. The string
				// form is the documented shorthand for input_text content.
				out = append(out, map[string]any{"role": "assistant", "content": sb.String()})
			}
			for _, tc := range m.ToolCalls {
				if tc.IsCustom {
					out = append(out, map[string]any{
						"type": "custom_tool_call", "call_id": tc.ID,
						"name": tc.Name, "input": tc.CustomInput,
					})
					continue
				}
				args := tc.Arguments
				if len(args) == 0 {
					args = json.RawMessage("{}")
				}
				out = append(out, map[string]any{
					"type": "function_call", "call_id": tc.ID,
					"name": tc.Name, "arguments": string(args),
				})
			}
			if len(m.Texts) == 0 && len(m.ToolCalls) == 0 {
				out = append(out, map[string]any{"role": "assistant", "content": ""})
			}
		case RoleTool:
			if len(m.Texts) > 0 || len(m.ToolCalls) > 0 {
				return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'messages[%d]' carries non-result state on a tool message", i)}
			}
			if len(m.Results) == 0 {
				return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'messages[%d]' has no tool results", i)}
			}
			for _, r := range m.Results {
				if customCalls[r.CallID] {
					out = append(out, map[string]any{
						"type":    "custom_tool_call_output",
						"call_id": r.CallID, "output": r.Content,
					})
					continue
				}
				out = append(out, map[string]any{
					"type":    "function_call_output",
					"call_id": r.CallID, "output": r.Content,
				})
			}
		}
	}
	if len(out) == 0 {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: "'messages' must contain at least one translatable message"}
	}
	return out, nil
}
