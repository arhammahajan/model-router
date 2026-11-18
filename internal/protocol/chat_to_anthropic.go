// Chat -> Anthropic request encoding. One plan is built and encoded once;
// there is no dry-run encoder that could drift from actual encoding.
package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
)

// DefaultAnthropicMaxTokens is the bridge default for Anthropic's required
// max_tokens when the Chat request carries no cap. Documented in go/README.md.
const DefaultAnthropicMaxTokens = 4096

// AnthropicVersion is the default anthropic-version header.
const AnthropicVersion = "2023-06-01"

// EncodeAnthropicRequest renders the IR as an Anthropic Messages request body.
func EncodeAnthropicRequest(g *Generation, upstreamModel string) ([]byte, *RouterError) {
	system, msgs, rerr := encodeAnthropicMessages(g.Messages)
	if rerr != nil {
		return nil, rerr
	}
	maxTokens := DefaultAnthropicMaxTokens
	if g.MaxTokens != nil {
		maxTokens = *g.MaxTokens
	}
	body := map[string]any{
		"model":      upstreamModel,
		"max_tokens": maxTokens,
		"messages":   msgs,
		"stream":     g.Stream,
	}
	if len(system) > 0 {
		body["system"] = system
	}
	if g.Temperature != nil {
		body["temperature"] = *g.Temperature
	}
	if g.TopP != nil {
		body["top_p"] = *g.TopP
	}
	if g.ExtraTopK != nil {
		body["top_k"] = *g.ExtraTopK
	}
	if len(g.Stop) > 0 {
		body["stop_sequences"] = g.Stop
	}
	if len(g.Tools) > 0 {
		tools := make([]any, 0, len(g.Tools))
		for _, t := range g.Tools {
			if t.IsCustom() {
				entry := map[string]any{"name": t.Name, "input_schema": json.RawMessage(emulatedCustomSchema())}
				if desc := emulatedCustomDescription(t.Description, t.Format); desc != "" {
					entry["description"] = desc
				}
				tools = append(tools, entry)
				continue
			}
			entry := map[string]any{"name": t.Name, "input_schema": json.RawMessage(t.Schema)}
			if t.Description != "" {
				entry["description"] = t.Description
			}
			tools = append(tools, entry)
		}
		body["tools"] = tools
		choice := anthropicToolChoice(g)
		if choice != nil {
			body["tool_choice"] = choice
		}
	}
	out, err := json.Marshal(body)
	if err != nil {
		return nil, &RouterError{Status: 500, Code: "encoding_error", Msg: "failed to encode upstream request"}
	}
	return out, nil
}

func anthropicToolChoice(g *Generation) map[string]any {
	mode := "auto"
	if g.Choice != nil {
		mode = g.Choice.Mode
	} else if !g.HasTools {
		return nil
	}
	disableParallel := g.ParallelToolCalls != nil && !*g.ParallelToolCalls
	switch mode {
	case "none":
		return map[string]any{"type": "none"}
	case "any":
		c := map[string]any{"type": "any"}
		if disableParallel {
			c["disable_parallel_tool_use"] = true
		}
		return c
	case "specific":
		c := map[string]any{"type": "tool", "name": g.Choice.Name}
		if disableParallel {
			c["disable_parallel_tool_use"] = true
		}
		return c
	default: // auto
		c := map[string]any{"type": "auto"}
		if disableParallel {
			c["disable_parallel_tool_use"] = true
		}
		return c
	}
}

// anthropicMessage is an Anthropic wire message (system hoisted separately).
type anthropicMessage struct {
	Role    string `json:"role"`
	Content []any  `json:"content"`
}

// encodeAnthropicMessages converts IR messages, hoisting system prompts and
// merging consecutive same-role messages (Anthropic requires alternation).
// Tool messages become user-role tool_result blocks.
func encodeAnthropicMessages(msgs []Message) ([]any, []any, *RouterError) {
	var system []any
	var out []anthropicMessage
	flush := func(role string, blocks []any) {
		if len(blocks) == 0 {
			return
		}
		if role == "system" {
			system = append(system, blocks...)
			return
		}
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Content = append(out[n-1].Content, blocks...)
			return
		}
		out = append(out, anthropicMessage{Role: role, Content: blocks})
	}
	for i, m := range msgs {
		if m.Role != RoleUser && len(m.Blocks) > 0 {
			return nil, nil, &RouterError{Status: 500, Code: "encoding_error", Msg: "Ordered media requires a user message"}
		}
		switch m.Role {
		case RoleSystem:
			var blocks []any
			for _, t := range m.Texts {
				blocks = append(blocks, map[string]any{"type": "text", "text": t.Text})
			}
			flush("system", blocks)
		case RoleUser:
			var blocks []any
			if len(m.Blocks) > 0 {
				for _, b := range m.Blocks {
					switch b.Kind {
					case ContentKindText:
						blocks = append(blocks, map[string]any{"type": "text", "text": b.Text})
					case ContentKindImage:
						blocks = append(blocks, map[string]any{"type": "image", "source": map[string]any{
							"type": "base64", "media_type": b.Image.MIME, "data": b.Image.Data,
						}})
					case ContentKindFile:
						doc := map[string]any{"type": "document", "source": map[string]any{
							"type": "base64", "media_type": b.File.MIME, "data": b.File.Data,
						}}
						if b.File.Filename != "" {
							doc["title"] = b.File.Filename
						}
						blocks = append(blocks, doc)
					default:
						return nil, nil, &RouterError{Status: 500, Code: "encoding_error", Msg: "Unsupported media for Anthropic encoding"}
					}
				}
				flush("user", blocks)
				continue
			}
			for _, t := range m.Texts {
				blocks = append(blocks, map[string]any{"type": "text", "text": t.Text})
			}
			flush("user", blocks)
		case RoleAssistant:
			var blocks []any
			for _, t := range m.Texts {
				blocks = append(blocks, map[string]any{"type": "text", "text": t.Text})
			}
			for _, tc := range m.ToolCalls {
				if tc.IsCustom {
					encoded, rerr := encodeCustomArgs(tc.CustomInput, DefaultLimits())
					if rerr != nil {
						return nil, nil, rerr
					}
					entry := map[string]any{"type": "tool_use", "name": tc.Name, "input": encoded}
					if tc.ID != "" {
						entry["id"] = tc.ID
					} else {
						return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
							Msg: fmt.Sprintf("'messages[%d].tool_calls' history entries require stable IDs", i)}
					}
					blocks = append(blocks, entry)
					continue
				}
				// Arguments were validated as a JSON object at decode time
				// and preserved as compact raw JSON: embed them verbatim so
				// large integers and number literals survive exactly.
				args := tc.Arguments
				if len(args) == 0 {
					args = json.RawMessage("{}")
				}
				entry := map[string]any{"type": "tool_use", "name": tc.Name, "input": args}
				if tc.ID != "" {
					entry["id"] = tc.ID
				} else {
					// Anthropic requires tool_use IDs; a missing history ID
					// cannot be carried over. Reject rather than invent one:
					// invented identities would break call/result matching.
					return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
						Msg: fmt.Sprintf("'messages[%d].tool_calls' history entries require stable IDs", i)}
				}
				blocks = append(blocks, entry)
			}
			flush("assistant", blocks)
		case RoleTool:
			var blocks []any
			for _, r := range m.Results {
				if strings.TrimSpace(r.CallID) == "" {
					return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
						Msg: fmt.Sprintf("'messages[%d].tool_call_id' is required on tool messages", i)}
				}
				entry := map[string]any{
					"type": "tool_result", "tool_use_id": r.CallID,
					"content": r.Content,
				}
				if r.IsError {
					entry["is_error"] = true
				}
				blocks = append(blocks, entry)
			}
			flush("user", blocks)
		}
	}
	// An empty conversation is rejected by the upstream; fail closed here so
	// the router never sends a degenerate generation.
	if len(out) == 0 && len(system) == 0 {
		return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: "'messages' must contain at least one translatable message"}
	}
	final := make([]any, 0, len(out))
	for _, m := range out {
		final = append(final, map[string]any{"role": m.Role, "content": m.Content})
	}
	return system, final, nil
}

// SystemBlocks returns hoisted system blocks for the request body.
func SystemBlocksFor(msgs []Message) []any {
	var system []any
	for _, m := range msgs {
		if m.Role != RoleSystem {
			continue
		}
		for _, t := range m.Texts {
			system = append(system, map[string]any{"type": "text", "text": t.Text})
		}
	}
	return system
}
