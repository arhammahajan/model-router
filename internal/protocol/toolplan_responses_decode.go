package protocol

import (
	"encoding/json"
	"fmt"
	"slices"
)

// ToolDeclaration retains source declaration boundaries separately from flat
// function bindings. Empty namespaces are plain tools; namespace groups may
// contain zero tools. Returned declarations are copies, not mutable plan state.
type ToolDeclaration struct {
	Namespace      string
	Description    string
	HasDescription bool
	Start, Count   int
	Origin         string
}

func (p *ToolPlan) Declarations() []ToolDeclaration {
	if p == nil {
		return nil
	}
	return append([]ToolDeclaration(nil), p.declarations...)
}

// dropPlainDeclaration removes a rejected top-level tool before the plan is
// attached to Generation. Later declarations index the admitted tools, while
// their diagnostic origins still refer to the source request.
func (p *ToolPlan) dropPlainDeclaration(origin string) {
	for i, d := range p.declarations {
		if d.Origin != origin {
			continue
		}
		for j := i + 1; j < len(p.declarations); j++ {
			p.declarations[j].Start--
		}
		p.declarations = slices.Delete(p.declarations, i, i+1)
		return
	}
}

func (p *ToolPlan) hasNamespaceDeclarations() bool {
	if p == nil {
		return false
	}
	for _, d := range p.declarations {
		if d.Namespace != "" {
			return true
		}
	}
	return false
}

// NamespaceLosses diagnoses group descriptions that cannot survive flattening.
// Their source declaration is retained for the client echo; the loss does not
// claim that the backend received or enforced the group description.
func (p *ToolPlan) NamespaceLosses() []Loss {
	var losses []Loss
	if p == nil {
		return nil
	}
	for _, d := range p.declarations {
		if d.Namespace != "" && d.Description != "" {
			losses = append(losses, Loss{Field: d.Origin + ".description", Reason: "namespace description has no flat-tool equivalent", Action: "dropped"})
		}
	}
	return losses
}

// BuildResponsesNamespacePlan parses the namespace boundary once. Definitions,
// source history and choices share bindings; source declarations remain ordered
// for effective tool echoes. RawMessage maps exist only at this wire boundary.
func BuildResponsesNamespacePlan(top map[string]json.RawMessage) (*ToolPlan, []map[string]json.RawMessage, *RouterError) {
	var arr []map[string]json.RawMessage
	if raw, ok := top["tools"]; ok && !isNull(raw) {
		if json.Unmarshal(raw, &arr) != nil {
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: "'tools' must contain objects"}
		}
	}
	var history []map[string]json.RawMessage
	if raw, ok := top["input"]; ok && !isNull(raw) {
		_ = json.Unmarshal(raw, &history)
	}
	var reserved []string
	for _, item := range arr {
		var typ, name string
		_ = json.Unmarshal(item["type"], &typ)
		if typ != "namespace" && json.Unmarshal(item["name"], &name) == nil {
			reserved = append(reserved, name)
		}
	}
	for _, item := range history {
		var name, ns string
		_ = json.Unmarshal(item["namespace"], &ns)
		if ns == "" && json.Unmarshal(item["name"], &name) == nil {
			reserved = append(reserved, name)
		}
	}
	builder := NewToolPlanBuilder(reserved)
	var flat []map[string]json.RawMessage
	for i, item := range arr {
		if item == nil {
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: "'tools' must contain objects"}
		}
		var typ string
		_ = json.Unmarshal(item["type"], &typ)
		origin := fmt.Sprintf("tools[%d]", i)
		if typ != "namespace" {
			builder.SetOrigin(fmt.Sprintf("tools[%d]", len(flat)), origin)
			builder.declarations = append(builder.declarations, ToolDeclaration{Start: len(flat), Count: 1, Origin: origin})
			flat = append(flat, item)
			continue
		}
		for key := range item {
			switch key {
			case "type", "name", "description", "tools":
			default:
				return nil, nil, &RouterError{Status: 400, Code: CodeInvalidGeneration, Msg: origin + ": unsupported namespace field"}
			}
		}
		ns, ok := decodeUnicodeString(item["name"])
		if !ok || ns == "" {
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: origin + ".name must be a non-empty Unicode string"}
		}
		declaration := ToolDeclaration{Namespace: ns, Start: len(flat), Origin: origin}
		if raw, ok := item["description"]; ok && !isNull(raw) {
			if json.Unmarshal(raw, &declaration.Description) != nil {
				return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: origin + ".description must be a string"}
			}
			declaration.HasDescription = true
		}
		var children []map[string]json.RawMessage
		raw, ok := item["tools"]
		if !ok || isNull(raw) || json.Unmarshal(raw, &children) != nil {
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: origin + " must contain a function tools array"}
		}
		for j, child := range children {
			childOrigin := fmt.Sprintf("%s.tools[%d]", origin, j)
			if child == nil {
				return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: childOrigin + " must be a tool object"}
			}
			var childType, childName string
			if raw, ok := child["type"]; ok && !isNull(raw) {
				if json.Unmarshal(raw, &childType) != nil {
					return nil, nil, &RouterError{Status: 400, Code: CodeInvalidGeneration, Msg: childOrigin + ".type must be a string"}
				}
			}
			if childType != "" && childType != "function" && childType != "custom" {
				return nil, nil, &RouterError{Status: 400, Code: CodeInvalidGeneration, Msg: childOrigin + ": only function and custom namespace children have a bridge mapping"}
			}
			_ = json.Unmarshal(child["name"], &childName)
			if err := validateToolName(childName); err != nil {
				return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: childOrigin + ".name " + err.Error()}
			}
			alias, rerr := builder.AddNamespaced(ns, childName, fmt.Sprintf("tools[%d]", len(flat)), childOrigin)
			if rerr != nil {
				return nil, nil, rerr
			}
			clone := make(map[string]json.RawMessage, len(child))
			for key, value := range child {
				clone[key] = value
			}
			clone["name"], _ = json.Marshal(alias)
			flat = append(flat, clone)
		}
		declaration.Count = len(flat) - declaration.Start
		builder.declarations = append(builder.declarations, declaration)
	}
	// Historical calls need not still be declared in this turn's tools. They
	// receive explicit bindings just like the source namespace flattener.
	for i, item := range history {
		var typ string
		_ = json.Unmarshal(item["type"], &typ)
		if typ != "function_call" && typ != "custom_tool_call" {
			continue
		}
		raw, hasNS := item["namespace"]
		if !hasNS || isNull(raw) {
			continue
		}
		ns, ok := decodeUnicodeString(raw)
		if !ok {
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: fmt.Sprintf("input[%d].namespace must be a Unicode string", i)}
		}
		if ns == "" {
			continue
		}
		var name string
		_ = json.Unmarshal(item["name"], &name)
		if err := validateToolName(name); err != nil {
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: fmt.Sprintf("input[%d].name is invalid", i)}
		}
		if _, rerr := builder.AddNamespaced(ns, name, "", fmt.Sprintf("input[%d]", i)); rerr != nil {
			return nil, nil, rerr
		}
	}
	return builder.Build(), flat, nil
}
