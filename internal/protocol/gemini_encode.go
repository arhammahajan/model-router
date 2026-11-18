// Chat -> Gemini request encoding. One plan is built and encoded once;
// there is no dry-run encoder that could drift from actual encoding.
//
// Wire contract (Gemini REST reference,
// https://ai.google.dev/api/generate-content):
//   - model and generation method belong to the HTTP URL, never the body:
//     no `model` or `stream` members are emitted here.
//   - contents: ordered conversation turns; system prompts hoisted into
//     systemInstruction (text only); assistant history with tool calls uses
//     role `model` with functionCall parts; tool results use role `user`
//     with functionResponse parts (the generate-content role table allows
//     `user`/`model`; function responses ride as `user` turns carrying
//     functionResponse parts).
//   - tools: one entry carrying functionDeclarations; each declaration
//     uses parametersJsonSchema (full JSON Schema, mutually exclusive with
//     parameters) so the validated Chat schema is carried exactly with no
//     OpenAPI-subset rewriting. Newer models accept the field; older
//     models reject it upstream (preserved, never silently rewritten).
//   - toolConfig.functionCallingConfig: none->NONE, auto->AUTO, any/required
//     ->ANY, specific->ANY+allowedFunctionNames. Omitted choice with tools
//     omits toolConfig (server default AUTO).
//   - generationConfig: temperature, topP (camelCase), maxOutputTokens,
//     stopSequences (at most 5, enforced at decode).
//
// History identity: Chat call/result IDs are preserved exactly on the wire
// (functionCall.id / functionResponse.id are optional identifiers per the
// current REST reference). Unknown id fields are never silently dropped:
// any upstream rejection surfaces as-is. Consecutive tool results are
// merged into one `user` content so all responses follow all calls in one
// turn.
package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
)

// EncodeGeminiRequest renders the IR as a Gemini generateContent request
// body. The model name and streaming method live in the HTTP URL and are
// never emitted here.
func EncodeGeminiRequest(g *Generation) ([]byte, *RouterError) {
	return encodeGeminiRequest(g, AbsentEffort())
}

// EncodeGeminiRequestWithPlan renders the IR with the typed planned Gemini
// effort. Only Absent (emit nothing), Budget (exact
// generationConfig.thinkingConfig.thinkingBudget) and Named (exact
// generationConfig.thinkingConfig.thinkingLevel per the verified primary
// GenerateContent REST contract
// https://ai.google.dev/api/generate-content) are admitted; any other
// planned target is internal misuse. The planner owns the trusted
// capability gates (supported plus the exact native descriptor plus the
// reviewed known enum plus the official wire enum for Named; supported/min/
// max plus the exact BudgetNative descriptor and int32 range for Budget);
// the encoder owns the exact wire members.
func EncodeGeminiRequestWithPlan(g *Generation, planned PlannedEffort) ([]byte, *RouterError) {
	return encodeGeminiRequest(g, planned)
}

func encodeGeminiRequest(g *Generation, planned PlannedEffort) ([]byte, *RouterError) {
	contents, sysInstr, rerr := c2gEncodeContents(g.Messages)
	if rerr != nil {
		return nil, rerr
	}
	body := map[string]any{
		"contents": contents,
	}
	if sysInstr != nil {
		body["systemInstruction"] = sysInstr
	}
	if len(g.Tools) > 0 {
		decls := make([]any, 0, len(g.Tools))
		for _, t := range g.Tools {
			if t.IsCustom() {
				entry := map[string]any{"name": t.Name, "parametersJsonSchema": json.RawMessage(append(json.RawMessage(nil), emulatedCustomSchema()...))}
				if desc := emulatedCustomDescription(t.Description, t.Format); desc != "" {
					entry["description"] = desc
				}
				decls = append(decls, entry)
				continue
			}
			schema := t.Schema
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object"}`)
			}
			entry := map[string]any{"name": t.Name, "parametersJsonSchema": json.RawMessage(append(json.RawMessage(nil), schema...))}
			if t.Description != "" {
				entry["description"] = t.Description
			}
			decls = append(decls, entry)
		}
		body["tools"] = []any{map[string]any{"functionDeclarations": decls}}
		if cfg := c2gToolConfig(g); cfg != nil {
			body["toolConfig"] = cfg
		}
	} else if g.Choice != nil && g.Choice.Mode == "none" {
		// No tools to constrain: omitting toolConfig already means no
		// calls, so nothing is emitted.
	}
	var genCfg map[string]any
	if g.Temperature != nil {
		if genCfg == nil {
			genCfg = map[string]any{}
		}
		genCfg["temperature"] = *g.Temperature
	}
	if g.TopP != nil {
		if genCfg == nil {
			genCfg = map[string]any{}
		}
		genCfg["topP"] = *g.TopP
	}
	if g.MaxTokens != nil {
		if genCfg == nil {
			genCfg = map[string]any{}
		}
		genCfg["maxOutputTokens"] = *g.MaxTokens
	}
	if len(g.Stop) > 0 {
		if genCfg == nil {
			genCfg = map[string]any{}
		}
		genCfg["stopSequences"] = g.Stop
	}
	// The typed target renders directly: Budget emits the exact
	// thinkingBudget, Named emits the exact official thinkingLevel
	// (uppercase conversion); Absent emits nothing. Off never reaches the
	// Gemini encoder via the shared planner (dropped pre-I/O), so it fails
	// as internal misuse rather than guessing a wire mapping.
	switch planned.Kind() {
	case EffortAbsent:
	case EffortBudget:
		budget, ok := planned.Budget()
		if !ok || budget < 1 || budget > 2147483647 {
			return nil, &RouterError{Status: 500, Code: "encoding_error", Msg: "failed to encode upstream request"}
		}
		if genCfg == nil {
			genCfg = map[string]any{}
		}
		genCfg["thinkingConfig"] = map[string]any{"thinkingBudget": budget}
	case EffortNamed:
		level, ok := planned.Effort()
		if !ok {
			return nil, &RouterError{Status: 500, Code: "encoding_error", Msg: "failed to encode upstream request"}
		}
		wire, ok := GeminiThinkingLevelWire(level)
		if !ok {
			return nil, &RouterError{Status: 500, Code: "encoding_error", Msg: "failed to encode upstream request"}
		}
		if genCfg == nil {
			genCfg = map[string]any{}
		}
		genCfg["thinkingConfig"] = map[string]any{"thinkingLevel": wire}
	case EffortOff:
		return nil, &RouterError{Status: 500, Code: "encoding_error", Msg: "failed to encode upstream request"}
	}
	if genCfg != nil {
		body["generationConfig"] = genCfg
	}
	out, err := json.Marshal(body)
	if err != nil {
		return nil, &RouterError{Status: 500, Code: "encoding_error", Msg: "failed to encode upstream request"}
	}
	return out, nil
}

// c2gToolConfig maps the IR tool choice onto Gemini functionCallingConfig.
// Modes per the REST contract: AUTO (default), ANY (constrained to calls),
// NONE (no calls). A named choice constrains ANY to one allowed name.
func c2gToolConfig(g *Generation) map[string]any {
	if g.Choice == nil {
		return nil
	}
	switch g.Choice.Mode {
	case "none":
		return map[string]any{"functionCallingConfig": map[string]any{"mode": "NONE"}}
	case "auto":
		return map[string]any{"functionCallingConfig": map[string]any{"mode": "AUTO"}}
	case "any":
		return map[string]any{"functionCallingConfig": map[string]any{"mode": "ANY"}}
	case "specific":
		return map[string]any{"functionCallingConfig": map[string]any{
			"mode": "ANY", "allowedFunctionNames": []string{g.Choice.Name},
		}}
	default:
		return nil
	}
}

// c2gEncodeContents renders IR messages as Gemini contents plus an optional
// systemInstruction. System texts are hoisted; everything else keeps caller
// order. Consecutive tool results merge into one `user` content so all
// function responses follow all function calls in the turn.
func c2gEncodeContents(msgs []Message) ([]any, map[string]any, *RouterError) {
	callNames := map[string]string{}
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			if _, dup := callNames[tc.ID]; dup {
				continue
			}
			callNames[tc.ID] = tc.Name
		}
	}
	var sysTexts []string
	var contents []any
	var pendingFR []any
	flushFR := func() {
		if len(pendingFR) == 0 {
			return
		}
		contents = append(contents, map[string]any{"role": "user", "parts": pendingFR})
		pendingFR = nil
	}
	for i, m := range msgs {
		switch m.Role {
		case RoleSystem:
			if len(m.ToolCalls) > 0 || len(m.Results) > 0 || len(m.Blocks) > 0 {
				return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'messages[%d]' carries tools on a system message", i)}
			}
			for _, t := range m.Texts {
				sysTexts = append(sysTexts, t.Text)
			}
		case RoleUser:
			if len(m.ToolCalls) > 0 || len(m.Results) > 0 {
				return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'messages[%d]' carries tool state on a user message", i)}
			}
			flushFR()
			if len(m.Blocks) > 0 {
				parts := make([]any, 0, len(m.Blocks))
				for _, b := range m.Blocks {
					switch b.Kind {
					case ContentKindImage:
						parts = append(parts, map[string]any{"inlineData": map[string]any{
							"mimeType": b.Image.MIME, "data": b.Image.Data,
						}})
					case ContentKindText, "":
						if b.Text != "" {
							parts = append(parts, map[string]any{"text": b.Text})
						}
					default:
						// Chat->Gemini audio/file bridging is deferred to a
						// separate reviewed unit; Blocks of those kinds never
						// arise from the Chat->Gemini decoder.
						return nil, nil, &RouterError{Status: 500, Code: "encoding_error", Msg: "failed to encode upstream request"}
					}
				}
				if len(parts) == 0 {
					return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
						Msg: fmt.Sprintf("'messages[%d]' carries no translatable content", i)}
				}
				contents = append(contents, map[string]any{"role": "user", "parts": parts})
				break
			}
			var sb strings.Builder
			for _, t := range m.Texts {
				sb.WriteString(t.Text)
			}
			contents = append(contents, map[string]any{
				"role": "user", "parts": []any{map[string]any{"text": sb.String()}},
			})
		case RoleAssistant:
			if len(m.Results) > 0 || len(m.Blocks) > 0 {
				return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'messages[%d]' carries tool results on an assistant message", i)}
			}
			flushFR()
			var parts []any
			var sb strings.Builder
			for _, t := range m.Texts {
				sb.WriteString(t.Text)
			}
			if sb.Len() > 0 {
				parts = append(parts, map[string]any{"text": sb.String()})
			}
			for _, tc := range m.ToolCalls {
				if strings.TrimSpace(tc.ID) == "" {
					return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
						Msg: fmt.Sprintf("'messages[%d].tool_calls' history entries require stable IDs", i)}
				}
				if tc.IsCustom {
					encoded, rerr := encodeCustomArgs(tc.CustomInput, DefaultLimits())
					if rerr != nil {
						return nil, nil, rerr
					}
					fc := map[string]any{"name": tc.Name, "args": json.RawMessage(append(json.RawMessage(nil), encoded...))}
					fc["id"] = tc.ID
					parts = append(parts, map[string]any{"functionCall": fc})
					continue
				}
				args := tc.Arguments
				if len(args) == 0 {
					args = json.RawMessage("{}")
				}
				// Arguments were validated as a JSON object at decode
				// time and preserved as compact raw JSON: embed them
				// verbatim so large integers survive exactly.
				fc := map[string]any{"name": tc.Name, "args": json.RawMessage(append(json.RawMessage(nil), args...))}
				// Preserve the supplied call identity exactly. Any upstream
				// rejection of the field is preserved, never silently dropped.
				fc["id"] = tc.ID
				parts = append(parts, map[string]any{"functionCall": fc})
			}
			if len(parts) == 0 {
				return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'messages[%d]' carries no translatable content", i)}
			}
			contents = append(contents, map[string]any{"role": "model", "parts": parts})
		case RoleTool:
			if len(m.Texts) > 0 || len(m.ToolCalls) > 0 || len(m.Blocks) > 0 {
				return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'messages[%d]' carries non-result state on a tool message", i)}
			}
			if len(m.Results) == 0 {
				return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'messages[%d]' has no tool results", i)}
			}
			for _, r := range m.Results {
				if strings.TrimSpace(r.CallID) == "" {
					return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
						Msg: fmt.Sprintf("'messages[%d].tool_call_id' is required on tool messages", i)}
				}
				name, ok := callNames[r.CallID]
				if !ok || name == "" {
					return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
						Msg: fmt.Sprintf("'messages[%d]' references an unmatched tool call identity", i)}
				}
				fr := map[string]any{
					"name":     name,
					"response": map[string]any{"output": r.Content},
					"id":       r.CallID,
				}
				pendingFR = append(pendingFR, map[string]any{"functionResponse": fr})
			}
		}
	}
	flushFR()
	var sysInstr map[string]any
	// Hoist system turns into systemInstruction (text only). Empty system
	// texts contribute empty parts which the upstream rejects, so only
	// non-empty system text is carried; an all-empty system block is
	// omitted rather than sent as an empty instruction.
	var sysParts []any
	for _, t := range sysTexts {
		if t == "" {
			continue
		}
		sysParts = append(sysParts, map[string]any{"text": t})
	}
	if len(sysParts) > 0 {
		sysInstr = map[string]any{"parts": sysParts}
	}
	if len(contents) == 0 {
		return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: "'messages' must contain at least one translatable message"}
	}
	return contents, sysInstr, nil
}
