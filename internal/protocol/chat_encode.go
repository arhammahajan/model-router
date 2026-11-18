// Anthropic -> Chat request encoding. One plan is encoded once; the IR
// produced by DecodeAnthropicRequest is rendered as an OpenAI Chat
// Completions request body for the configured upstream model.
package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
)

// EncodeChatRequest renders the IR as a Chat Completions request body. The
// upstream model ID replaces the public ingress model. Streaming requests
// always carry stream_options.include_usage: the reverse streaming translator
// needs the terminal usage chunk, because Chat reports prompt accounting
// only at the end of the stream. Anthropic top_k has no portable Chat wire
// mapping and is diagnosed as a loss at decode time, never forwarded here.
// A present planned named effort is emitted as the Chat wire member
// reasoning_effort; absent (dropped or stripped) emits nothing.
func EncodeChatRequest(gen *Generation, planned PlannedEffort, upstreamModel string) ([]byte, *RouterError) {
	msgs, rerr := encodeChatMessages(gen.Messages)
	if rerr != nil {
		return nil, rerr
	}
	body := map[string]any{
		"model":    upstreamModel,
		"messages": msgs,
		"stream":   gen.Stream,
	}
	if gen.MaxTokens != nil {
		body["max_tokens"] = *gen.MaxTokens
	}
	if gen.Temperature != nil {
		body["temperature"] = *gen.Temperature
	}
	if gen.TopP != nil {
		body["top_p"] = *gen.TopP
	}
	if len(gen.Stop) > 0 {
		body["stop"] = gen.Stop
	}
	if gen.Stream {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	if len(gen.Tools) > 0 {
		tools := make([]any, 0, len(gen.Tools))
		for _, t := range gen.Tools {
			if t.IsCustom() {
				entry := map[string]any{
					"name": t.Name,
					"parameters": json.RawMessage(
						append(json.RawMessage(nil), emulatedCustomSchema()...),
					),
				}
				if desc := emulatedCustomDescription(t.Description, t.Format); desc != "" {
					entry["description"] = desc
				}
				tools = append(tools, map[string]any{"type": "function", "function": entry})
				continue
			}
			entry := map[string]any{
				"name": t.Name,
				"parameters": json.RawMessage(
					append(json.RawMessage(nil), t.Schema...),
				),
			}
			if t.Description != "" {
				entry["description"] = t.Description
			}
			if t.Strict != nil {
				entry["strict"] = *t.Strict
			}
			tools = append(tools, map[string]any{"type": "function", "function": entry})
		}
		body["tools"] = tools
		choice := chatToolChoice(gen)
		if choice != nil {
			body["tool_choice"] = choice
		}
	}
	if gen.ParallelToolCalls != nil {
		body["parallel_tool_calls"] = *gen.ParallelToolCalls
	}
	// The typed target renders directly: ordinary/Strata named effort as
	// reasoning_effort, llama disable as the kwargs toggle, and llama
	// budgets as the exact thinking member. Absent emits nothing.
	switch planned.Kind() {
	case EffortNamed:
		if effort, ok := planned.Effort(); ok {
			body["reasoning_effort"] = effort
		}
	case EffortOff:
		body["chat_template_kwargs"] = map[string]any{"enable_thinking": false}
	case EffortBudget:
		if budget, ok := planned.Budget(); ok {
			body["thinking_budget_tokens"] = budget
		}
	case EffortAbsent:
	}
	out, err := json.Marshal(body)
	if err != nil {
		return nil, &RouterError{Status: 500, Code: "encoding_error", Msg: "failed to encode upstream request"}
	}
	return out, nil
}

// chatToolChoice maps the IR choice to the Chat tool_choice wire contract:
// Anthropic any forces at least one tool call, like Chat required.
//
// Chat tool_choice is a string-or-object union: "none", "auto" and
// "required" are bare strings; only a named function choice is an object.
// See https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create
// (ChatCompletionToolChoiceOption = "none" | "auto" | "required" | named object).
func chatToolChoice(g *Generation) any {
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
		return map[string]any{
			"type":     "function",
			"function": map[string]any{"name": g.Choice.Name},
		}
	default:
		return nil
	}
}

// encodeChatMessages renders IR messages as Chat wire messages. Text blocks
// keep their order by concatenation; tool calls and results keep theirs by
// position. User Blocks (bounded image/audio/PDF verticals) render as an
// ordered content array of text, image_url data-URL, input_audio, and file
// blocks with exact MIME/data preservation. Shapes the decoder never
// produces (results on a non-tool message, text on a tool message, Blocks
// outside a user message) are rejected rather than silently reordered.
func encodeChatMessages(msgs []Message) ([]any, *RouterError) {
	out := make([]any, 0, len(msgs))
	for i, m := range msgs {
		switch m.Role {
		case RoleSystem:
			if len(m.ToolCalls) > 0 || len(m.Results) > 0 || len(m.Blocks) > 0 {
				return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'messages[%d]' carries tools on a system message", i)}
			}
			var sb strings.Builder
			for _, t := range m.Texts {
				sb.WriteString(t.Text)
			}
			out = append(out, map[string]any{"role": "system", "content": sb.String()})
		case RoleUser:
			if len(m.ToolCalls) > 0 || len(m.Results) > 0 {
				return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'messages[%d]' carries tool state on a user message", i)}
			}
			if len(m.Blocks) > 0 {
				arr := make([]any, 0, len(m.Blocks))
				for _, b := range m.Blocks {
					switch b.Kind {
					case ContentKindImage:
						arr = append(arr, map[string]any{
							"type": "image_url",
							"image_url": map[string]any{
								"url": "data:" + b.Image.MIME + ";base64," + b.Image.Data,
							},
						})
					case ContentKindAudio:
						arr = append(arr, map[string]any{
							"type": "input_audio",
							"input_audio": map[string]any{
								"data":   b.Audio.Data,
								"format": b.Audio.Format,
							},
						})
					case ContentKindFile:
						arr = append(arr, map[string]any{
							"type": "file",
							"file": map[string]any{
								"file_data": "data:" + b.File.MIME + ";base64," + b.File.Data,
								"filename":  b.File.Filename,
							},
						})
					case ContentKindText, "":
						if b.Text != "" {
							arr = append(arr, map[string]any{"type": "text", "text": b.Text})
						}
					default:
						return nil, &RouterError{Status: 500, Code: "encoding_error", Msg: "failed to encode upstream request"}
					}
				}
				out = append(out, map[string]any{"role": "user", "content": arr})
				continue
			}
			var sb strings.Builder
			for _, t := range m.Texts {
				sb.WriteString(t.Text)
			}
			out = append(out, map[string]any{"role": "user", "content": sb.String()})
		case RoleAssistant:
			if len(m.Results) > 0 || len(m.Blocks) > 0 {
				return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'messages[%d]' carries tool results on an assistant message", i)}
			}
			var sb strings.Builder
			for _, t := range m.Texts {
				sb.WriteString(t.Text)
			}
			entry := map[string]any{"role": "assistant"}
			if sb.Len() > 0 {
				entry["content"] = sb.String()
			} else if len(m.ToolCalls) == 0 {
				entry["content"] = ""
			} else {
				entry["content"] = nil
			}
			if len(m.ToolCalls) > 0 {
				calls := make([]any, 0, len(m.ToolCalls))
				for _, tc := range m.ToolCalls {
					if tc.IsCustom {
						encoded, rerr := encodeCustomArgs(tc.CustomInput, DefaultLimits())
						if rerr != nil {
							return nil, rerr
						}
						calls = append(calls, map[string]any{
							"id":   tc.ID,
							"type": "function",
							"function": map[string]any{
								"name":      tc.Name,
								"arguments": string(encoded),
							},
						})
						continue
					}
					args := tc.Arguments
					if len(args) == 0 {
						args = json.RawMessage("{}")
					}
					// Arguments were validated as a JSON object at decode
					// time and preserved as compact raw JSON: stringify them
					// verbatim so large integers and number literals
					// survive exactly.
					calls = append(calls, map[string]any{
						"id":   tc.ID,
						"type": "function",
						"function": map[string]any{
							"name":      tc.Name,
							"arguments": string(args),
						},
					})
				}
				entry["tool_calls"] = calls
			}
			out = append(out, entry)
		case RoleTool:
			if len(m.Texts) > 0 || len(m.ToolCalls) > 0 || len(m.Blocks) > 0 {
				return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'messages[%d]' carries non-result state on a tool message", i)}
			}
			if len(m.Results) == 0 {
				return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'messages[%d]' has no tool results", i)}
			}
			for _, r := range m.Results {
				out = append(out, map[string]any{
					"role": "tool", "tool_call_id": r.CallID, "content": r.Content,
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
