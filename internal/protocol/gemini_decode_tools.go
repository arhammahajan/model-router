// Gemini ingress tool decoding: functionCall/functionResponse history,
// function declarations, toolConfig and the documented Schema converter.
// See gemini_decode.go for the request entry point and alias/identity policies.
package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
)

func (d *gdecDecoder) functionCall(path string, raw json.RawMessage) (*ToolCall, *RouterError) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, d.bad(fmt.Sprintf("'%s.functionCall' must be an object", path))
	}
	obj, rerr := gdecObject(m, map[string]bool{
		"id": true, "name": true, "args": true, "partialArgs": true, "willContinue": true,
	}, path+".functionCall")
	if rerr != nil {
		return nil, rerr
	}
	if v, ok := obj["partialArgs"]; ok && !isNull(v) {
		return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
			Msg: fmt.Sprintf("'%s.functionCall.partialArgs' requires a same-protocol route", path)}
	}
	if v, ok := obj["willContinue"]; ok && !isNull(v) {
		return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
			Msg: fmt.Sprintf("'%s.functionCall.willContinue' requires a same-protocol route", path)}
	}
	var name string
	if v, ok := obj["name"]; !ok || isNull(v) || json.Unmarshal(v, &name) != nil || name == "" {
		return nil, d.bad(fmt.Sprintf("'%s.functionCall.name' must be a non-empty string", path))
	}
	if err := validateToolName(name); err != nil {
		return nil, d.bad(fmt.Sprintf("'%s.functionCall.name' %s", path, err.Error()))
	}
	var id string
	if v, ok := obj["id"]; ok && !isNull(v) {
		var original string
		if err := json.Unmarshal(v, &original); err != nil {
			return nil, d.bad(fmt.Sprintf("'%s.functionCall.id' must be a string", path))
		}
		preserved, rerr := d.gdecPreserveID(original, path+".functionCall.id")
		if rerr != nil {
			return nil, rerr
		}
		id = preserved
	} else {
		id = d.gdecMintID()
	}
	args := json.RawMessage("{}")
	if v, ok := obj["args"]; ok && !isNull(v) {
		compacted, rerr := gdecCompactObject(fmt.Sprintf("'%s.functionCall.args'", path), v, d.lim)
		if rerr != nil {
			return nil, rerr
		}
		args = compacted
	}
	return &ToolCall{ID: id, Name: name, Arguments: args}, nil
}

func (d *gdecDecoder) functionResponse(path string, raw json.RawMessage) (*ToolResult, *RouterError) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, d.bad(fmt.Sprintf("'%s.functionResponse' must be an object", path))
	}
	obj, rerr := gdecObject(m, map[string]bool{
		"id": true, "name": true, "response": true, "parts": true, "willContinue": true, "scheduling": true,
	}, path+".functionResponse")
	if rerr != nil {
		return nil, rerr
	}
	var name string
	if v, ok := obj["name"]; !ok || isNull(v) || json.Unmarshal(v, &name) != nil || name == "" {
		return nil, d.bad(fmt.Sprintf("'%s.functionResponse.name' must be a non-empty string", path))
	}
	if err := validateToolName(name); err != nil {
		return nil, d.bad(fmt.Sprintf("'%s.functionResponse.name' %s", path, err.Error()))
	}
	if v, ok := obj["parts"]; ok && !isNull(v) {
		var arr []json.RawMessage
		if err := json.Unmarshal(v, &arr); err != nil {
			return nil, d.bad(fmt.Sprintf("'%s.functionResponse.parts' must be an array", path))
		}
		for i, e := range arr {
			if rerr := gdecValidateFunctionResponsePart(fmt.Sprintf("%s.functionResponse.parts[%d]", path, i), e); rerr != nil {
				return nil, rerr
			}
		}
		if len(arr) > 0 {
			d.lose(path+".functionResponse.parts", "multimodal function results have no equivalent", false)
		}
	}
	if v, ok := obj["willContinue"]; ok && !isNull(v) {
		var b bool
		if err := json.Unmarshal(v, &b); err != nil {
			return nil, d.bad(fmt.Sprintf("'%s.functionResponse.willContinue' must be bool", path))
		}
		d.lose(path+".functionResponse.willContinue", "generator scheduling has no equivalent", false)
	}
	if v, ok := obj["scheduling"]; ok && !isNull(v) {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return nil, d.bad(fmt.Sprintf("'%s.functionResponse.scheduling' must be a string", path))
		}
		switch s {
		case "SCHEDULING_UNSPECIFIED", "SILENT", "WHEN_IDLE", "INTERRUPT":
		default:
			return nil, d.bad(fmt.Sprintf("'%s.functionResponse.scheduling' is not a scheduling mode", path))
		}
		d.lose(path+".functionResponse.scheduling", "generator scheduling has no equivalent", false)
	}
	content := "{}"
	rv, ok := obj["response"]
	if !ok || isNull(rv) {
		return nil, d.bad(fmt.Sprintf("'%s.functionResponse.response' is required", path))
	}
	compacted, rerr := gdecCompactObject(fmt.Sprintf("'%s.functionResponse.response'", path), rv, d.lim)
	if rerr != nil {
		return nil, rerr
	}
	content = string(compacted)
	// Correlate to the pending model call: an explicit id must match a
	// pending call and the names must cohere; a missing id matches by name
	// only when unambiguous.
	if v, ok := obj["id"]; ok && !isNull(v) {
		var original string
		if err := json.Unmarshal(v, &original); err != nil {
			return nil, d.bad(fmt.Sprintf("'%s.functionResponse.id' must be a string", path))
		}
		id, rerr := gdecValidateSuppliedID(original, path+".functionResponse.id", d.lim)
		if rerr != nil {
			return nil, rerr
		}
		for i, p := range d.pending {
			if p.id == id {
				if p.name != name {
					return nil, d.bad(fmt.Sprintf("'%s.functionResponse.id' names a call with a conflicting name", path))
				}
				d.pending = append(d.pending[:i], d.pending[i+1:]...)
				return &ToolResult{CallID: p.id, Content: content}, nil
			}
		}
		return nil, d.bad(fmt.Sprintf("'%s.functionResponse.id' has no matching functionCall", path))
	}
	matches := []int{}
	for i, p := range d.pending {
		if p.name == name {
			matches = append(matches, i)
		}
	}
	if len(matches) == 0 {
		return nil, d.bad(fmt.Sprintf("'%s.functionResponse' has no matching functionCall named '%s' in the preceding model turn", path, name))
	}
	if len(matches) > 1 {
		return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
			Msg: fmt.Sprintf("'%s.functionResponse' is ambiguous without an id: %d pending calls share a name", path, len(matches))}
	}
	p := d.pending[matches[0]]
	d.pending = append(d.pending[:matches[0]], d.pending[matches[0]+1:]...)
	return &ToolResult{CallID: p.id, Content: content}, nil
}

// gdecCompactObject validates that raw is a JSON object and returns its
// compact form without a float64 round-trip, so large integers and number
// literals survive exactly.
func gdecCompactObject(what string, raw json.RawMessage, lim Limits) (json.RawMessage, *RouterError) {
	bad := &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: what + " must be a JSON object"}
	if len(raw) > lim.MaxArgsChars {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: what + " exceeds the size bound"}
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, bad
	}
	if _, isObj := v.(map[string]any); !isObj {
		return nil, bad
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return nil, bad
	}
	return json.RawMessage(compact.Bytes()), nil
}

func (d *gdecDecoder) tools(raw json.RawMessage, g *Generation) *RouterError {
	var arr []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return d.bad("'tools' must be an array")
	}
	names := map[string]bool{}
	for ti, item := range arr {
		if item == nil {
			return d.bad("'tools' must contain objects")
		}
		obj, rerr := gdecObject(item, map[string]bool{
			"functionDeclarations": true, "googleSearchRetrieval": true,
			"codeExecution": true, "googleSearch": true, "computerUse": true,
			"urlContext": true, "fileSearch": true, "mcpServers": true,
			"googleMaps": true,
		}, fmt.Sprintf("tools[%d]", ti))
		if rerr != nil {
			return rerr
		}
		for k, v := range obj {
			if k == "functionDeclarations" || isNull(v) {
				continue
			}
			return &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("'tools[%d].%s' requires a same-protocol route", ti, k)}
		}
		rawDecls, ok := obj["functionDeclarations"]
		if !ok || isNull(rawDecls) {
			continue
		}
		var decls []map[string]json.RawMessage
		if err := json.Unmarshal(rawDecls, &decls); err != nil {
			return d.bad(fmt.Sprintf("'tools[%d].functionDeclarations' must be an array", ti))
		}
		for fi, decl := range decls {
			if decl == nil {
				return d.bad(fmt.Sprintf("'tools[%d].functionDeclarations' must contain objects", ti))
			}
			path := fmt.Sprintf("tools[%d].functionDeclarations[%d]", ti, fi)
			def, rerr := d.toolDef(path, decl)
			if rerr != nil {
				return rerr
			}
			if names[def.Name] {
				return d.bad(fmt.Sprintf("'%s' repeats function name '%s'", path, def.Name))
			}
			names[def.Name] = true
			g.Tools = append(g.Tools, *def)
		}
	}
	if len(g.Tools) > 0 {
		g.HasTools = true
	}
	return nil
}

func (d *gdecDecoder) toolDef(path string, decl map[string]json.RawMessage) (*ToolDef, *RouterError) {
	obj, rerr := gdecObject(decl, map[string]bool{
		"name": true, "description": true, "behavior": true,
		"parameters": true, "parametersJsonSchema": true,
		"response": true, "responseJsonSchema": true,
	}, path)
	if rerr != nil {
		return nil, rerr
	}
	var name string
	if v, ok := obj["name"]; !ok || isNull(v) || json.Unmarshal(v, &name) != nil || name == "" {
		return nil, d.bad(fmt.Sprintf("'%s.name' must be a non-empty string", path))
	}
	if err := validateToolName(name); err != nil {
		return nil, d.bad(fmt.Sprintf("'%s.name' %s", path, err.Error()))
	}
	var desc string
	if v, ok := obj["description"]; ok && !isNull(v) {
		if err := json.Unmarshal(v, &desc); err != nil {
			return nil, d.bad(fmt.Sprintf("'%s.description' must be a string", path))
		}
	}
	hasParams := false
	if v, ok := obj["parameters"]; ok && !isNull(v) {
		hasParams = true
	}
	hasJSON := false
	if v, ok := obj["parametersJsonSchema"]; ok && !isNull(v) {
		hasJSON = true
	}
	if hasParams && hasJSON {
		return nil, d.bad(fmt.Sprintf("'%s' sets both parameters and parametersJsonSchema", path))
	}
	schema := json.RawMessage(`{"type":"object"}`)
	if hasJSON {
		compacted, rerr := gdecCompactObject(fmt.Sprintf("'%s.parametersJsonSchema'", path), obj["parametersJsonSchema"], d.lim)
		if rerr != nil {
			return nil, rerr
		}
		schema = compacted
	} else if hasParams {
		converted, rerr := gdecConvertSchema(obj["parameters"], path+".parameters", 0, d.lim)
		if rerr != nil {
			return nil, rerr
		}
		schema = converted
		if gdecSchemaHasOrdering(obj["parameters"]) {
			d.lose(path+".parameters.propertyOrdering", "property ordering has no equivalent on this destination", false)
		}
	}
	if v, ok := obj["behavior"]; ok && !isNull(v) {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return nil, d.bad(fmt.Sprintf("'%s.behavior' must be a string", path))
		}
		d.lose(path+".behavior", "non-blocking function behavior has no equivalent", false)
	}
	for _, k := range []string{"response", "responseJsonSchema"} {
		if v, ok := obj[k]; ok && !isNull(v) {
			if k == "response" {
				if _, rerr := gdecConvertSchema(v, path+"."+k, 0, d.lim); rerr != nil {
					return nil, rerr
				}
				if gdecSchemaHasOrdering(v) {
					d.lose(path+"."+k+".propertyOrdering", "property ordering has no equivalent on this destination", false)
				}
			} else {
				var o map[string]json.RawMessage
				if err := json.Unmarshal(v, &o); err != nil || o == nil {
					return nil, d.bad(fmt.Sprintf("'%s.%s' must be an object", path, k))
				}
			}
			d.lose(path+"."+k, "function response schemas have no equivalent", false)
		}
	}
	return &ToolDef{Name: name, Description: desc, Schema: schema}, nil
}

func (d *gdecDecoder) toolConfig(raw json.RawMessage, g *Generation) *RouterError {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return d.bad("'toolConfig' must be an object")
	}
	obj, rerr := gdecObject(m, map[string]bool{
		"functionCallingConfig": true, "retrievalConfig": true,
	}, "toolConfig")
	if rerr != nil {
		return rerr
	}
	if v, ok := obj["retrievalConfig"]; ok && !isNull(v) {
		if rerr := d.retrievalConfig("toolConfig.retrievalConfig", v); rerr != nil {
			return rerr
		}
	}
	rawCalling, ok := obj["functionCallingConfig"]
	if !ok || isNull(rawCalling) {
		return nil
	}
	var cm map[string]json.RawMessage
	if err := json.Unmarshal(rawCalling, &cm); err != nil || cm == nil {
		return d.bad("'toolConfig.functionCallingConfig' must be an object")
	}
	calling, rerr := gdecObject(cm, map[string]bool{
		"mode": true, "allowedFunctionNames": true,
	}, "toolConfig.functionCallingConfig")
	if rerr != nil {
		return rerr
	}
	where := "toolConfig.functionCallingConfig"
	mode := ""
	if v, ok := calling["mode"]; ok && !isNull(v) {
		if err := json.Unmarshal(v, &mode); err != nil {
			return d.bad(fmt.Sprintf("'%s.mode' must be a string", where))
		}
	}
	switch mode {
	case "", "MODE_UNSPECIFIED", "AUTO", "ANY", "NONE", "VALIDATED":
	default:
		return d.bad(fmt.Sprintf("'%s.mode' is not a function calling mode", where))
	}
	var allowed []string
	if v, ok := calling["allowedFunctionNames"]; ok && !isNull(v) {
		var arr []json.RawMessage
		if err := json.Unmarshal(v, &arr); err != nil {
			return d.bad(fmt.Sprintf("'%s.allowedFunctionNames' must be an array of strings", where))
		}
		for _, e := range arr {
			var s string
			if err := json.Unmarshal(e, &s); err != nil {
				return d.bad(fmt.Sprintf("'%s.allowedFunctionNames' must contain strings", where))
			}
			allowed = append(allowed, s)
		}
		declared := map[string]bool{}
		for _, t := range g.Tools {
			declared[t.Name] = true
		}
		for _, n := range allowed {
			if !declared[n] && len(g.Tools) > 0 {
				return d.bad(fmt.Sprintf("'%s.allowedFunctionNames' names a function that is not declared", where))
			}
		}
	}
	var choice *ToolChoice
	switch mode {
	case "AUTO":
		choice = &ToolChoice{Mode: "auto"}
	case "ANY":
		choice = &ToolChoice{Mode: "any"}
	case "NONE":
		choice = &ToolChoice{Mode: "none"}
	case "VALIDATED":
		choice = &ToolChoice{Mode: "auto"}
		d.lose(where+".mode", "schema-validated calling has no equivalent; automatic calling is used", true)
	}
	if len(allowed) > 0 {
		if len(allowed) == 1 && mode == "ANY" {
			choice = &ToolChoice{Mode: "specific", Name: allowed[0]}
		} else if mode != "NONE" {
			keep := map[string]bool{}
			for _, n := range allowed {
				keep[n] = true
			}
			kept := g.Tools[:0]
			for _, t := range g.Tools {
				if keep[t.Name] {
					kept = append(kept, t)
				}
			}
			g.Tools = kept
			if len(g.Tools) == 0 {
				g.HasTools = false
			}
			d.lose(where+".allowedFunctionNames", "the destination has no allowed-tools control; the tool list was narrowed instead", true)
		}
	}
	g.Choice = choice
	return nil
}
