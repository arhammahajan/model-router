package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

const responsesCustomEmuReason = "custom tool input is emulated by a string-valued function; native freeform/grammar generation is not preserved"

// emulatedCustomSchema builds the single reviewed emulation shape:
// {"type":"object","properties":{"content":{"type":"string"}},"required":["content"],"additionalProperties":false}.
func emulatedCustomSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"content":{"type":"string"}},"required":["content"],"additionalProperties":false}`)
}

// emulatedCustomDescription appends the grammar as instructions text. It
// never claims native grammar enforcement.
func emulatedCustomDescription(desc string, format *CustomFormat) string {
	if format == nil || format.Type != "grammar" {
		return desc
	}
	prefix := "\n\nReturn content following this " + format.Syntax + " grammar:\n" + format.Definition
	if desc == "" {
		// Python concatenates description + "\n\n..." even when empty,
		// leaving the leading newlines. Preserve exactly.
		return prefix
	}
	return desc + prefix
}

// encodeCustomArgs builds the bounded {"content":...} object bytes.
func encodeCustomArgs(input string, lim Limits) (json.RawMessage, *RouterError) {
	if !utf8.ValidString(input) {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: "'input' must be a Unicode string"}
	}
	if len(input) > lim.MaxArgsChars {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: "'input' exceeds the size bound"}
	}
	encoded, err := json.Marshal(map[string]string{"content": input})
	if err != nil {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: "'input' must be a Unicode string"}
	}
	if len(encoded) > lim.MaxArgsChars {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: "'input' exceeds the size bound"}
	}
	return json.RawMessage(encoded), nil
}

// decodeCustomArgs validates the single reviewed wrapper shape and returns
// the exact freeform string. Any other shape is a malformed/ambiguous
// wrapper and fails closed.
func decodeCustomArgs(raw json.RawMessage, lim Limits) (string, *RouterError) {
	bad := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid translated tool call"}
	if len(raw) == 0 || len(raw) > lim.MaxArgsChars {
		return "", &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call exceeds the size bound"}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return "", bad
	}
	if key, err := dec.Token(); err != nil || key != "content" {
		return "", bad
	}
	var rawContent json.RawMessage
	if err := dec.Decode(&rawContent); err != nil {
		return "", bad
	}
	// The object must end after one member, regardless of how a duplicate
	// key is spelled on the wire.
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return "", bad
	}
	if _, err := dec.Token(); err != io.EOF {
		return "", bad
	}
	input, valid := decodeUnicodeString(rawContent)
	if !valid {
		return "", bad
	}
	if len(input) > lim.MaxArgsChars {
		return "", &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call exceeds the size bound"}
	}
	return input, nil
}

// parseResponsesCustomSpec validates one Responses custom definition.
func parseResponsesCustomSpec(origin string, item map[string]json.RawMessage, lim Limits) (string, string, *CustomFormat, *RouterError) {
	for k := range item {
		switch k {
		case "type", "name", "description", "format":
		default:
			return "", "", nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", origin+"."+k)}
		}
	}
	var typ string
	if raw, ok := item["type"]; ok && !isNull(raw) {
		_ = json.Unmarshal(raw, &typ)
		if typ != "custom" {
			return "", "", nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", origin+".type")}
		}
	}
	var name string
	if rawN, ok := item["name"]; ok && !isNull(rawN) {
		var valid bool
		name, valid = decodeUnicodeString(rawN)
		if !valid {
			name = ""
		} else {
			// decodeUnicodeString accepts any string; enforce tool-name grammar.
			var probe string
			_ = json.Unmarshal(rawN, &probe)
			name = probe
		}
	}
	if err := validateToolName(name); err != nil {
		return "", "", nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s.name' %s", origin, err.Error())}
	}
	var desc string
	if rawD, ok := item["description"]; ok && !isNull(rawD) {
		var valid bool
		desc, valid = decodeUnicodeString(rawD)
		if !valid {
			return "", "", nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'%s.description' must be a string", origin)}
		}
	}
	var format *CustomFormat
	if rawF, ok := item["format"]; ok && !isNull(rawF) {
		var fobj map[string]json.RawMessage
		if err := json.Unmarshal(rawF, &fobj); err != nil || fobj == nil {
			return "", "", nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'%s.format' must be an object", origin)}
		}
		for k := range fobj {
			switch k {
			case "type", "syntax", "definition":
			default:
				return "", "", nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
					Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", origin+".format."+k)}
			}
		}
		var ftype string
		if rawT, ok := fobj["type"]; !ok || isNull(rawT) || json.Unmarshal(rawT, &ftype) != nil {
			return "", "", nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'%s.format.type' must be a string", origin)}
		}
		switch ftype {
		case "text":
			for k := range fobj {
				if k != "type" {
					return "", "", nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
						Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", origin+".format."+k)}
				}
			}
			format = &CustomFormat{Type: "text"}
		case "grammar":
			var syntax string
			if rawS, ok := fobj["syntax"]; !ok || isNull(rawS) || json.Unmarshal(rawS, &syntax) != nil || (syntax != "lark" && syntax != "regex") {
				return "", "", nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'%s.format.syntax' must be 'lark' or 'regex'", origin)}
			}
			definition, valid := decodeUnicodeString(fobj["definition"])
			if !valid || definition == "" {
				return "", "", nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'%s.format.definition' must be a non-empty string", origin)}
			}
			if len(definition) > lim.MaxArgsChars {
				return "", "", nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'%s.format.definition' exceeds the size bound", origin)}
			}
			if strings.TrimSpace(definition) == "" {
				return "", "", nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'%s.format.definition' must be a non-empty string", origin)}
			}
			format = &CustomFormat{Type: "grammar", Syntax: syntax, Definition: definition}
		default:
			return "", "", nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Tool type %q has no Chat bridge mapping; ordinary function tools only", "custom")}
		}
	}
	return name, desc, format, nil
}

// IsEmulatedCustom reports whether alias is an emulated Responses custom.
func (p *ToolPlan) IsEmulatedCustom(alias string) bool {
	if p == nil {
		return false
	}
	_, ok := p.emuCustom[alias]
	return ok
}

// EmulatedSource returns the source identity for an emulated custom alias.
func (p *ToolPlan) EmulatedSource(alias string) (ToolIdentity, bool) {
	if p == nil {
		return ToolIdentity{}, false
	}
	id, ok := p.emuCustom[alias]
	return id, ok
}

// HasEmulatedCustom reports whether the plan carries any emulated customs.
func (p *ToolPlan) HasEmulatedCustom() bool {
	return p != nil && len(p.emuCustom) > 0
}

// addEmulatedCustom records one emulated custom binding. It is called during
// request decode (single-threaded); after decode the plan is read-only.
func (p *ToolPlan) addEmulatedCustom(alias string, source ToolIdentity) {
	if p == nil {
		return
	}
	if p.emuCustom == nil {
		p.emuCustom = map[string]ToolIdentity{}
	}
	if _, ok := p.emuCustom[alias]; !ok {
		p.emuCustom[alias] = source
	}
}

// addNamespacedBinding mints (or reuses) the alias for an orphan namespaced
// history entry after Build. Identical (namespace,name) reuses the alias;
// a colliding alias with a different identity is a strict 400.
func (p *ToolPlan) addNamespacedBinding(namespace, name, origin string) (string, *RouterError) {
	if namespace == "" || name == "" {
		return "", &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: "Tool namespaces and function names must be nonempty strings"}
	}
	alias := NamespaceAlias(namespace, name)
	if prev, ok := p.byAlias[alias]; ok {
		if prev != (ToolIdentity{Namespace: namespace, Name: name}) {
			return "", &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: "Tool namespace alias collides with another function"}
		}
		return alias, nil
	}
	// Collision with a plain reserved name is checked by the caller via
	// duplicate tool detection; here we only guard the binding table.
	if p.byAlias == nil {
		p.byAlias = map[string]ToolIdentity{}
	}
	if p.bySource == nil {
		p.bySource = map[ToolIdentity]string{}
	}
	p.byAlias[alias] = ToolIdentity{Namespace: namespace, Name: name}
	p.bySource[ToolIdentity{Namespace: namespace, Name: name}] = alias
	return alias, nil
}

// decodeResponsesCustomCall validates one historical custom_tool_call item.
func decodeResponsesCustomCall(prefix string, obj map[string]json.RawMessage, lose func(string, string, bool), lim Limits, plan *ToolPlan) (*Message, *RouterError) {
	for k := range obj {
		switch k {
		case "type", "id", "call_id", "name", "input", "status", "namespace", "caller":
		default:
			return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", prefix+"."+k)}
		}
	}
	if rawID, ok := obj["id"]; ok && !isNull(rawID) {
		var id string
		if err := json.Unmarshal(rawID, &id); err != nil || id == "" {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'%s.id' must be a string", prefix)}
		}
	}
	callID, idErr := decodeResponsesHistoryCallID(prefix, obj["call_id"], lim)
	if idErr != nil {
		return nil, idErr
	}
	var name string
	if rawN, ok := obj["name"]; ok && !isNull(rawN) {
		_ = json.Unmarshal(rawN, &name)
	}
	if err := validateToolName(name); err != nil {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s.name' %s", prefix, err.Error())}
	}
	alias := name
	source := ToolIdentity{Name: name}
	if rawNS, ok := obj["namespace"]; ok && !isNull(rawNS) {
		ns, valid := decodeUnicodeString(rawNS)
		if !valid {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'%s.namespace' must be a string", prefix)}
		}
		if ns != "" {
			bound, ok := plan.AliasFor(ns, name)
			if !ok {
				var rerr *RouterError
				bound, rerr = plan.addNamespacedBinding(ns, name, prefix)
				if rerr != nil {
					return nil, rerr
				}
			}
			alias = bound
			if src, ok := plan.SourceFor(alias); ok {
				source = src
			} else {
				source = ToolIdentity{Namespace: ns, Name: name}
			}
		}
	}
	if rawCaller, ok := obj["caller"]; ok && !isNull(rawCaller) {
		return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
			Msg: fmt.Sprintf("'%s.caller' programmatic invocation requires a same-protocol route", prefix)}
	}
	if rawS, ok := obj["status"]; ok && !isNull(rawS) {
		var s string
		if err := json.Unmarshal(rawS, &s); err != nil ||
			(s != "in_progress" && s != "completed" && s != "incomplete") {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'%s.status' must be 'in_progress', 'completed' or 'incomplete'", prefix)}
		}
	}
	input, valid := decodeUnicodeString(obj["input"])
	if !valid {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s.input' must be a string", prefix)}
	}
	if len(input) > lim.MaxArgsChars {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s.input' exceeds the size bound", prefix)}
	}
	if !plan.IsEmulatedCustom(alias) {
		plan.addEmulatedCustom(alias, source)
		lose(prefix, responsesCustomEmuReason, true)
	}
	return &Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: callID, Name: alias, IsCustom: true, CustomInput: input}}}, nil
}

// decodeResponsesCustomCallOutput validates one custom_tool_call_output item.
func decodeResponsesCustomCallOutput(prefix string, obj map[string]json.RawMessage, lose func(string, string, bool), lim Limits, plan *ToolPlan, customResults map[string]bool) (*Message, *RouterError) {
	for k := range obj {
		switch k {
		case "type", "id", "call_id", "output", "status", "name", "namespace", "caller":
		default:
			return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", prefix+"."+k)}
		}
	}
	callID, idErr := decodeResponsesHistoryCallID(prefix, obj["call_id"], lim)
	if idErr != nil {
		return nil, idErr
	}
	if rawID, ok := obj["id"]; ok && !isNull(rawID) {
		var id string
		if err := json.Unmarshal(rawID, &id); err != nil || id == "" {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'%s.id' must be a string", prefix)}
		}
	}
	for _, k := range []string{"status", "name", "namespace"} {
		if rawV, ok := obj[k]; ok && !isNull(rawV) {
			var s string
			if err := json.Unmarshal(rawV, &s); err != nil || s == "" {
				return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'%s.%s' must be a string", prefix, k)}
			}
		}
	}
	if rawCaller, ok := obj["caller"]; ok && !isNull(rawCaller) {
		return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
			Msg: fmt.Sprintf("'%s.caller' programmatic invocation requires a same-protocol route", prefix)}
	}
	content := ""
	if rawOut, ok := obj["output"]; ok && !isNull(rawOut) {
		var s string
		if err := json.Unmarshal(rawOut, &s); err == nil {
			content = s
		} else {
			var parts []map[string]json.RawMessage
			if err := json.Unmarshal(rawOut, &parts); err != nil {
				return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'%s.output' must be a string or an array of text parts", prefix)}
			}
			var sb strings.Builder
			for pi, p := range parts {
				if p == nil {
					return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
						Msg: fmt.Sprintf("'%s.output' must contain content part objects", prefix)}
				}
				var typ string
				if rawT, ok := p["type"]; ok && !isNull(rawT) {
					_ = json.Unmarshal(rawT, &typ)
				}
				if typ != "input_text" {
					return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
						Msg: fmt.Sprintf("'%s.output[%d]' block type %q requires a same-protocol route", prefix, pi, typ)}
				}
				var text string
				if rawT, ok := p["text"]; !ok || isNull(rawT) || json.Unmarshal(rawT, &text) != nil {
					return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
						Msg: fmt.Sprintf("'%s.output[%d].text' must be a string", prefix, pi)}
				}
				sb.WriteString(text)
			}
			content = sb.String()
			lose(prefix+".output", "tool result parts are concatenated to text", true)
		}
	}
	if len(content) > lim.MaxArgsChars {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s.output' exceeds the size bound", prefix)}
	}
	customResults[callID] = true
	return &Message{Role: RoleTool, Results: []ToolResult{{CallID: callID, Content: content}}}, nil
}

// decodeResponsesCustomRef validates a named custom choice reference.
func decodeResponsesCustomRef(obj map[string]json.RawMessage, path string, plan *ToolPlan) (string, *RouterError) {
	var name string
	_ = json.Unmarshal(obj["name"], &name)
	if err := validateToolName(name); err != nil {
		return "", &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: fmt.Sprintf("'%s.name' %s", path, err.Error())}
	}
	if rawNS, ok := obj["namespace"]; ok && !isNull(rawNS) {
		ns, valid := decodeUnicodeString(rawNS)
		if !valid {
			return "", &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: fmt.Sprintf("'%s.namespace' must be a Unicode string", path)}
		}
		if ns != "" {
			alias, found := plan.AliasFor(ns, name)
			if !found {
				return "", &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: fmt.Sprintf("'%s' names an unknown tool", path)}
			}
			return alias, nil
		}
	}
	return name, nil
}
