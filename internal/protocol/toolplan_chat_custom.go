package protocol

import (
	"encoding/json"
	"fmt"
)

// Chat custom tool wire (OpenAI Chat Completions freeform):
//   {"type":"custom","custom":{"name":..., "description"?,
//     "format"?: {"type":"text"} | {"type":"grammar","grammar":{"syntax","definition"}}}}
// Responses native custom wire:
//   {"type":"custom","name":..., "description"?,
//     "format"?: {"type":"text"} | {"type":"grammar","syntax","definition"}}
// The grammar syntax/definition are preserved exactly for the native route.

// parseChatCustomSpec validates one Chat custom definition (source path
// tools[i]) and returns its typed payload. Malformed shapes are 400s in
// both modes; unknown format types have no bridge mapping.
func parseChatCustomSpec(i int, item map[string]json.RawMessage, lim Limits) (name, desc string, format *CustomFormat, rerr *RouterError) {
	for k := range item {
		switch k {
		case "type", "custom":
		default:
			return "", "", nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: tools[%d].%s", i, k)}
		}
	}
	rawSpec, ok := item["custom"]
	if !ok || isNull(rawSpec) {
		return "", "", nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'tools[%d].custom' is required", i)}
	}
	var spec map[string]json.RawMessage
	if err := json.Unmarshal(rawSpec, &spec); err != nil || spec == nil {
		return "", "", nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'tools[%d].custom' must be an object", i)}
	}
	for k := range spec {
		switch k {
		case "name", "description", "format":
		default:
			return "", "", nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: tools[%d].custom.%s", i, k)}
		}
	}
	var cname string
	if rawN, ok := spec["name"]; ok && !isNull(rawN) {
		_ = json.Unmarshal(rawN, &cname)
	}
	if err := validateToolName(cname); err != nil {
		return "", "", nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'tools[%d].custom.name' %s", i, err.Error())}
	}
	if rawD, ok := spec["description"]; ok && !isNull(rawD) {
		if err := json.Unmarshal(rawD, &desc); err != nil {
			return "", "", nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'tools[%d].custom.description' must be a string", i)}
		}
	}
	if rawF, ok := spec["format"]; ok && !isNull(rawF) {
		var fobj map[string]json.RawMessage
		if err := json.Unmarshal(rawF, &fobj); err != nil || fobj == nil {
			return "", "", nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'tools[%d].custom.format' must be an object", i)}
		}
		for k := range fobj {
			switch k {
			case "type", "grammar":
			default:
				return "", "", nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
					Msg: fmt.Sprintf("Unsupported or invalid request field for translation: tools[%d].custom.format.%s", i, k)}
			}
		}
		var ftype string
		if rawT, ok := fobj["type"]; !ok || isNull(rawT) || json.Unmarshal(rawT, &ftype) != nil {
			return "", "", nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'tools[%d].custom.format.type' must be a string", i)}
		}
		switch ftype {
		case "text":
			for k := range fobj {
				if k != "type" {
					return "", "", nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
						Msg: fmt.Sprintf("Unsupported or invalid request field for translation: tools[%d].custom.format.%s", i, k)}
				}
			}
			format = &CustomFormat{Type: "text"}
		case "grammar":
			rawG, ok := fobj["grammar"]
			if !ok || isNull(rawG) {
				return "", "", nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'tools[%d].custom.format.grammar' is required", i)}
			}
			var gobj map[string]json.RawMessage
			if err := json.Unmarshal(rawG, &gobj); err != nil || gobj == nil {
				return "", "", nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'tools[%d].custom.format.grammar' must be an object", i)}
			}
			for k := range gobj {
				switch k {
				case "syntax", "definition":
				default:
					return "", "", nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
						Msg: fmt.Sprintf("Unsupported or invalid request field for translation: tools[%d].custom.format.grammar.%s", i, k)}
				}
			}
			var syntax string
			if rawS, ok := gobj["syntax"]; !ok || isNull(rawS) || json.Unmarshal(rawS, &syntax) != nil || (syntax != "lark" && syntax != "regex") {
				return "", "", nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'tools[%d].custom.format.grammar.syntax' must be 'lark' or 'regex'", i)}
			}
			definition, valid := decodeUnicodeString(gobj["definition"])
			if !valid || definition == "" {
				return "", "", nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'tools[%d].custom.format.grammar.definition' must be a non-empty string", i)}
			}
			if len(definition) > lim.MaxArgsChars {
				return "", "", nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'tools[%d].custom.format.grammar.definition' exceeds the size bound", i)}
			}
			format = &CustomFormat{Type: "grammar", Syntax: syntax, Definition: definition}
		default:
			return "", "", nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Tool type %q has no Chat bridge mapping; ordinary function tools only", "custom")}
		}
	}
	return cname, desc, format, nil
}

// decodeChatCustomToolCall validates one assistant history call of the
// form {"id","type":"custom","custom":{"name","input"}}.
func decodeChatCustomToolCall(mi, ci int, c map[string]json.RawMessage, lim Limits) (*ToolCall, *RouterError) {
	prefix := fmt.Sprintf("messages[%d].tool_calls[%d]", mi, ci)
	for k := range c {
		switch k {
		case "id", "type", "custom":
		default:
			return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", prefix+"."+k)}
		}
	}
	rawFn, ok := c["custom"]
	if !ok || isNull(rawFn) {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s.custom' is required", prefix)}
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(rawFn, &obj); err != nil || obj == nil {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s.custom' must be an object", prefix)}
	}
	for k := range obj {
		switch k {
		case "name", "input":
		default:
			return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", prefix+".custom."+k)}
		}
	}
	var id string
	if rawID, ok := c["id"]; ok && !isNull(rawID) {
		if err := json.Unmarshal(rawID, &id); err != nil {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'%s.id' must be a string", prefix)}
		}
	}
	if err := validateCallID(id, lim); err != nil {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s.id' %s", prefix, err.Error())}
	}
	var name string
	if rawN, ok := obj["name"]; ok && !isNull(rawN) {
		_ = json.Unmarshal(rawN, &name)
	}
	if err := validateToolName(name); err != nil {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s.custom.name' %s", prefix, err.Error())}
	}
	var input string
	if rawIn, ok := obj["input"]; ok && !isNull(rawIn) {
		var valid bool
		input, valid = decodeUnicodeString(rawIn)
		if !valid {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'%s.custom.input' must be a string", prefix)}
		}
	}
	if len(input) > lim.MaxArgsChars {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s.custom.input' exceeds the size bound", prefix)}
	}
	return &ToolCall{ID: id, Name: name, IsCustom: true, CustomInput: input}, nil
}

// decodeChatCustomChoice validates {"type":"custom","custom":{"name":...}}.
func decodeChatCustomChoice(raw map[string]json.RawMessage) (string, *RouterError) {
	for k := range raw {
		switch k {
		case "type", "custom":
		default:
			return "", &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", "tool_choice."+k)}
		}
	}
	rawC, ok := raw["custom"]
	if !ok || isNull(rawC) {
		return "", &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: "'tool_choice.custom' must be an object"}
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(rawC, &obj); err != nil || obj == nil {
		return "", &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: "'tool_choice.custom' must be an object"}
	}
	for k := range obj {
		if k != "name" {
			return "", &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", "tool_choice.custom."+k)}
		}
	}
	var name string
	if rawN, ok := obj["name"]; ok && !isNull(rawN) {
		_ = json.Unmarshal(rawN, &name)
	}
	if err := validateToolName(name); err != nil {
		return "", &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'tool_choice.custom.name' %s", err.Error())}
	}
	return name, nil
}

// customToolResultCallIDs maps tool result call IDs to their custom-call
// identities for Responses history rendering.
func customToolResultCallIDs(msgs []Message) map[string]bool {
	out := map[string]bool{}
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			if tc.IsCustom {
				out[tc.ID] = true
			}
		}
	}
	return out
}
