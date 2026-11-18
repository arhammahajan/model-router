// Chat ingress for Responses upstream: narrow typed decode of a Chat
// Completions request into the shared Generation IR.
//
// The shared Chat source validator in chat_decode.go performs the decode
// under this destination's policy (DecodeChatForResponses below); only the
// tool decoder and the loss validator in this file are
// destination-specific. Do not compose a serialize-then-decode pipeline
// through DecodeChatRequest and do not accept its loss plan here: every
// admitted source field is represented, diagnosed, or rejected under this
// file's policy.
//
// Null policy: explicit null on an optional field means absent and is
// skipped, except for `stream`, whose contract is an explicit bool, and for
// nested bool controls, where null is a type error. Omitted, null, zero and
// false are distinguished explicitly. Malformed known fields and unknown
// nested members are validation errors in both strict and lossy modes.
package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// DecodeChatForResponses decodes a Chat Completions request body into the IR
// plus the Responses-destination loss plan. It performs no I/O.
//
// The shared Chat source validator in chat_decode.go performs the decode
// under this destination's policy; only the tool decoder and the loss
// validator below are destination-specific.
func DecodeChatForResponses(raw []byte, lim Limits) (*Generation, []Loss, *RouterError) {
	return decodeChatSource(raw, lim, chatSourcePolicy{
		dest:            chatDestResponses,
		decodeTool:      cfrDecodeTool,
		validateLoss:    cfrValidateLossField,
		storeLossReason: "stateless Responses translation forces store=false",
	})
}

// cfrDecodeTool validates one function or native custom tool for the
// Responses destination. Function tools keep the existing strict-loss
// contract. Custom tools (type custom with a custom object) are the
// native freeform vertical: text and preserved grammar, exact with no
// loss. Any other type is refused specifically, in both modes.
func cfrDecodeTool(i int, item map[string]json.RawMessage, lim Limits, lose func(string, string)) (*ToolDef, *RouterError) {
	var typ string
	if raw, ok := item["type"]; ok && !isNull(raw) {
		if err := json.Unmarshal(raw, &typ); err != nil {
			return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: tools[%d].type", i)}
		}
	}
	if typ == "custom" {
		for k := range item {
			switch k {
			case "type", "custom":
			default:
				return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
					Msg: fmt.Sprintf("Unsupported or invalid request field for translation: tools[%d].%s", i, k)}
			}
		}
		name, desc, format, rerr := parseChatCustomSpec(i, item, lim)
		if rerr != nil {
			return nil, rerr
		}
		return &ToolDef{Kind: ToolKindCustom, Name: name, Description: desc, Format: format}, nil
	}
	for k := range item {
		switch k {
		case "type", "function":
		default:
			return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: tools[%d].%s", i, k)}
		}
	}
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
	if rawS, ok := fn["strict"]; ok {
		if isNull(rawS) {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'tools[%d].function.strict' must be bool", i)}
		}
		var b bool
		if err := json.Unmarshal(rawS, &b); err != nil {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'tools[%d].function.strict' must be bool", i)}
		}
		// No IR slot for strict in the reviewed scope: diagnose, never drop.
		// The encoder emits strict:false as the portable non-strict fallback;
		// a source true is a diagnosed weakening, enforced by strict policy.
		lose(fmt.Sprintf("tools[%d].function.strict", i), "no reviewed bridge mapping")
	}
	return &ToolDef{Kind: ToolKindFunction, Name: name, Description: desc, Schema: schema}, nil
}

// cfrValidateLossField strictly validates a known-but-unbridged Chat field
// for the Responses destination. Well-formed values become losses; malformed
// values are 400 in both modes.
func cfrValidateLossField(key string, raw json.RawMessage, lose func(string, string)) *RouterError {
	bad := func(msg string) *RouterError {
		return &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: msg}
	}
	loss := func(reason string) *RouterError {
		lose(key, reason)
		return nil
	}
	mustStr := func(nonEmpty bool) *RouterError {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil || (nonEmpty && s == "") {
			return bad(fmt.Sprintf("'%s' must be a string", key))
		}
		return loss("no reviewed bridge mapping")
	}
	mustBool := func() *RouterError {
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return bad(fmt.Sprintf("'%s' must be bool", key))
		}
		return loss("no reviewed bridge mapping")
	}
	mustNum := func() *RouterError {
		lit := strings.TrimSpace(string(raw))
		if lit == "" || lit[0] == '"' || lit[0] == '\'' {
			return bad(fmt.Sprintf("'%s' must be a number", key))
		}
		if _, rerr := jsonFinite(raw, ""); rerr != nil {
			return bad(fmt.Sprintf("'%s' must be a number", key))
		}
		// jsonFinite accepts quoted strings via json.Number coercion;
		// the literal guard above rejects them explicitly.
		return loss("no reviewed bridge mapping")
	}
	mustInt := func() *RouterError {
		if _, rerr := jsonInt(raw, ""); rerr != nil {
			return bad(fmt.Sprintf("'%s' must be an integer", key))
		}
		return loss("no reviewed bridge mapping")
	}
	mustObj := func() (map[string]json.RawMessage, *RouterError) {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
			return nil, bad(fmt.Sprintf("'%s' must be an object", key))
		}
		return obj, nil
	}
	mustStrArray := func() *RouterError {
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err != nil {
			return bad(fmt.Sprintf("'%s' must be an array of strings", key))
		}
		for _, e := range arr {
			var s string
			if err := json.Unmarshal(e, &s); err != nil {
				return bad(fmt.Sprintf("'%s' must be an array of strings", key))
			}
		}
		return loss("no reviewed bridge mapping")
	}
	switch key {
	case "audio":
		obj, rerr := mustObj()
		if rerr != nil {
			return rerr
		}
		// Reviewed shape per Chat Completions audio param:
		// {"voice":..., "format":...}. Unknown members or malformed
		// nested values are 400 even in lossy mode; well-formed is a
		// diagnosed loss, never forwarded.
		for k, v := range obj {
			switch k {
			case "voice", "format":
				if isNull(v) {
					return bad(fmt.Sprintf("'%s.%s' must be a string", key, k))
				}
				var s string
				if err := json.Unmarshal(v, &s); err != nil || s == "" {
					return bad(fmt.Sprintf("'%s.%s' must be a non-empty string", key, k))
				}
			default:
				return bad(fmt.Sprintf("Unsupported or invalid request field for translation: %q", key+"."+k))
			}
		}
		if len(obj) == 0 {
			return bad(fmt.Sprintf("'%s' must be an object", key))
		}
		return loss("no reviewed bridge mapping")
	case "prediction":
		obj, rerr := mustObj()
		if rerr != nil {
			return rerr
		}
		// Reviewed shape: {"type":"content","content":...}.
		for k, v := range obj {
			switch k {
			case "type":
				var t string
				if err := json.Unmarshal(v, &t); err != nil || t != "content" {
					return bad("'prediction.type' must be 'content'")
				}
			case "content":
				if isNull(v) {
					return bad("'prediction.content' must be a string")
				}
				var s string
				if err := json.Unmarshal(v, &s); err == nil {
					if s == "" {
						return bad("'prediction.content' must be a non-empty string")
					}
				} else {
					// Allow array-of-strings content, but validate strictly.
					var arr []json.RawMessage
					if err := json.Unmarshal(v, &arr); err != nil || len(arr) == 0 {
						return bad("'prediction.content' must be a string")
					}
					for _, e := range arr {
						var es string
						if err := json.Unmarshal(e, &es); err != nil {
							return bad("'prediction.content' must be a string")
						}
					}
				}
			default:
				return bad(fmt.Sprintf("Unsupported or invalid request field for translation: %q", key+"."+k))
			}
		}
		if len(obj) == 0 {
			return bad(fmt.Sprintf("'%s' must be an object", key))
		}
		return loss("no reviewed bridge mapping")
	case "frequency_penalty", "presence_penalty":
		return mustNum()
	case "function_call":
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			if s != "none" && s != "auto" {
				return bad("'function_call' must be 'none', 'auto' or a function reference")
			}
			return loss("no reviewed bridge mapping")
		}
		obj, rerr := mustObj()
		if rerr != nil {
			return bad("'function_call' must be 'none', 'auto' or a function reference")
		}
		for k, v := range obj {
			if k != "name" {
				return bad(fmt.Sprintf("Unsupported or invalid request field for translation: %q", "function_call."+k))
			}
			var name string
			if err := json.Unmarshal(v, &name); err != nil || name == "" {
				return bad("'function_call.name' must be a non-empty string")
			}
		}
		return loss("no reviewed bridge mapping")
	case "functions":
		var arr []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &arr); err != nil {
			return bad("'functions' must contain objects")
		}
		for i, fn := range arr {
			for k, v := range fn {
				switch k {
				case "name":
					var name string
					if err := json.Unmarshal(v, &name); err != nil || name == "" {
						return bad(fmt.Sprintf("'functions[%d]'.name' must be a non-empty string", i))
					}
				case "description":
					var s string
					if err := json.Unmarshal(v, &s); err != nil {
						return bad(fmt.Sprintf("'functions[%d]'.description' must be a string", i))
					}
				case "parameters":
					var obj map[string]json.RawMessage
					if err := json.Unmarshal(v, &obj); err != nil || obj == nil {
						return bad(fmt.Sprintf("'functions[%d]'.parameters' must be an object", i))
					}
				default:
					return bad(fmt.Sprintf("Unsupported or invalid request field for translation: %q", fmt.Sprintf("functions[%d].%s", i, k)))
				}
			}
		}
		return loss("no reviewed bridge mapping")
	case "logit_bias":
		obj, rerr := mustObj()
		if rerr != nil {
			return rerr
		}
		for _, v := range obj {
			lit := strings.TrimSpace(string(v))
			if lit == "" || lit[0] == '"' || lit[0] == '\'' {
				return bad("'logit_bias' values must be numbers")
			}
			if _, rerr := jsonFinite(v, ""); rerr != nil {
				return bad("'logit_bias' values must be numbers")
			}
		}
		return loss("no reviewed bridge mapping")
	case "logprobs":
		return mustBool()
	case "modalities":
		return mustStrArray()
	case "verbosity", "prompt_cache_key", "prompt_cache_retention",
		"safety_identifier", "service_tier", "user":
		return mustStr(true)
	case "response_format":
		obj, rerr := mustObj()
		if rerr != nil {
			return rerr
		}
		var typ string
		rawT, ok := obj["type"]
		if !ok {
			return bad("'response_format.type' is required")
		}
		if err := json.Unmarshal(rawT, &typ); err != nil ||
			(typ != "text" && typ != "json_object" && typ != "json_schema") {
			return bad("'response_format.type' must be 'text', 'json_object' or 'json_schema'")
		}
		for k, v := range obj {
			switch k {
			case "type":
			case "json_schema":
				var schema map[string]json.RawMessage
				if err := json.Unmarshal(v, &schema); err != nil || schema == nil {
					return bad("'response_format.json_schema' must be an object")
				}
			default:
				return bad(fmt.Sprintf("Unsupported or invalid request field for translation: %q", "response_format."+k))
			}
		}
		return loss("no reviewed bridge mapping")
	case "seed", "top_logprobs":
		return mustInt()
	case "store":
		// Handled by the caller for exact false-match policy; reaching
		// here means a direct call, so validate strictly.
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return bad("'store' must be bool")
		}
		return loss("stateless Responses translation forces store=false")
	case "web_search_options":
		obj, rerr := mustObj()
		if rerr != nil {
			return rerr
		}
		for k, v := range obj {
			switch k {
			case "search_context_size":
				var s string
				if err := json.Unmarshal(v, &s); err != nil ||
					(s != "low" && s != "medium" && s != "high") {
					return bad("'web_search_options.search_context_size' must be 'low', 'medium' or 'high'")
				}
			case "user_location":
				var loc map[string]json.RawMessage
				if err := json.Unmarshal(v, &loc); err != nil || loc == nil {
					return bad("'web_search_options.user_location' must be an object")
				}
			default:
				return bad(fmt.Sprintf("Unsupported or invalid request field for translation: %q", "web_search_options."+k))
			}
		}
		return loss("no reviewed bridge mapping")
	default:
		lose(key, "no reviewed bridge mapping")
		return nil
	}
}
