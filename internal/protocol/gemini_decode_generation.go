// Gemini ingress generation controls: temperature/top_p/max tokens/stop
// mapping plus reviewed losses for the remaining generationConfig members.
// See gemini_decode.go for the request entry point and alias/identity policies.
package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

func (d *gdecDecoder) generation(raw json.RawMessage, g *Generation) *RouterError {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return d.bad("'generationConfig' must be an object")
	}
	obj, rerr := gdecObject(m, map[string]bool{
		"stopSequences": true, "responseMimeType": true, "responseSchema": true,
		"responseJsonSchema": true, "responseModalities": true, "candidateCount": true,
		"maxOutputTokens": true, "temperature": true, "topP": true, "topK": true,
		"seed": true, "presencePenalty": true, "frequencyPenalty": true,
		"responseLogprobs": true, "logprobs": true, "enableEnhancedCivicAnswers": true,
		"speechConfig": true, "thinkingConfig": true, "mediaResolution": true,
		"imageConfig": true, "audioTimestamp": true,
	}, "generationConfig")
	if rerr != nil {
		return rerr
	}
	num := func(key string, min, max float64) (float64, bool, *RouterError) {
		v, ok := obj[key]
		if !ok || isNull(v) {
			return 0, false, nil
		}
		f, rerr := jsonFinite(v, fmt.Sprintf("'generationConfig.%s' must be a number", key))
		if rerr != nil {
			return 0, false, rerr
		}
		if f < min || f > max {
			return 0, false, d.bad(fmt.Sprintf("'generationConfig.%s' is outside the supported numeric range", key))
		}
		return f, true, nil
	}
	intKey := func(key string, low int64) (int64, bool, *RouterError) {
		v, ok := obj[key]
		if !ok || isNull(v) {
			return 0, false, nil
		}
		i, rerr := jsonInt(v, fmt.Sprintf("'generationConfig.%s' must be an integer", key))
		if rerr != nil || i < low {
			return 0, false, d.bad(fmt.Sprintf("'generationConfig.%s' must be an integer at least %d", key, low))
		}
		return i, true, nil
	}
	if f, ok, rerr := num("temperature", 0, 2); rerr != nil {
		return rerr
	} else if ok {
		if d.dest == geminiDestAnthropic && f > 1 {
			// Anthropic temperature spans 0..1. A valid Gemini source
			// value above 1 has no exact mapping: it is diagnosed as a
			// loss and dropped, never silently clamped.
			d.loseDrop("generationConfig.temperature", "temperature above 1 has no Anthropic representation")
		} else {
			v := f
			g.Temperature = &v
		}
	}
	if f, ok, rerr := num("topP", 0, 1); rerr != nil {
		return rerr
	} else if ok {
		v := f
		g.TopP = &v
	}
	if i, ok, rerr := intKey("maxOutputTokens", 1); rerr != nil {
		return rerr
	} else if ok {
		v := int(i)
		g.MaxTokens = &v
	}
	if v, ok := obj["stopSequences"]; ok && !isNull(v) {
		var arr []json.RawMessage
		if err := json.Unmarshal(v, &arr); err != nil {
			return d.bad("'generationConfig.stopSequences' must be an array of strings")
		}
		var seq []string
		for _, e := range arr {
			var s string
			if err := json.Unmarshal(e, &s); err != nil || s == "" {
				return d.bad("'generationConfig.stopSequences' must contain non-empty strings")
			}
			seq = append(seq, s)
		}
		// The official GenerateContentRequest contract caps stopSequences
		// at 5 entries. Longer lists are rejected as invalid_request in
		// every destination and both policy modes, before any loss plan.
		if len(seq) > 5 {
			return d.bad("'generationConfig.stopSequences' must contain at most 5 strings")
		}
		if d.dest == geminiDestResponses {
			// Validated above, then dropped: the reviewed Responses
			// scope has no stop-sequence contract and nothing is
			// forwarded. An empty list is an exact no-op.
			if len(seq) > 0 {
				d.loseDrop("generationConfig.stopSequences", "Responses has no stop-sequence contract")
			}
		} else {
			g.Stop = append(g.Stop, seq...)
		}
	}
	// Portable controls end here. Everything below is validated for shape
	// and then diagnosed as a loss (strict refuses, lossy drops) or
	// refused outright where no honest mapping exists.
	if i, ok, rerr := intKey("topK", 0); rerr != nil {
		return rerr
	} else if ok {
		if d.dest == geminiDestAnthropic {
			// A validated non-negative topK maps exactly onto the
			// existing IR ExtraTopK/encoder path (Anthropic top_k). Both
			// contracts accept non-negative integers with the same
			// "top K candidates" semantics; neither REST reference
			// publishes a tighter numeric bound, so the value is
			// forwarded exactly and the upstream enforces its own range.
			v := int(i)
			g.ExtraTopK = &v
		} else {
			_ = i
			d.loseDrop("generationConfig.topK", "no reviewed bridge mapping")
		}
	}
	if _, ok, rerr := intKey("seed", -9223372036854775807-1); rerr != nil {
		return rerr
	} else if ok {
		d.loseDrop("generationConfig.seed", d.geminiEncoderControlLoss("seed"))
	}
	for _, k := range []string{"presencePenalty", "frequencyPenalty"} {
		if f, ok, rerr := num(k, -2, 2); rerr != nil {
			return rerr
		} else if ok {
			_ = f
			d.loseDrop("generationConfig."+k, d.geminiEncoderControlLoss("penalty"))
		}
	}
	if v, ok := obj["candidateCount"]; ok && !isNull(v) {
		i, rerr := jsonInt(v, "'generationConfig.candidateCount' must be an integer")
		if rerr != nil || i < 1 {
			return d.bad("'generationConfig.candidateCount' must be a positive integer")
		}
		if i != 1 {
			d.loseDrop("generationConfig.candidateCount", "multiple candidates have no equivalent; one is generated")
		}
	}
	if v, ok := obj["responseLogprobs"]; ok && !isNull(v) {
		var b bool
		if err := json.Unmarshal(v, &b); err != nil {
			return d.bad("'generationConfig.responseLogprobs' must be bool")
		}
		if b {
			d.loseDrop("generationConfig.responseLogprobs", "log probabilities are not translated")
		}
	}
	if v, ok := obj["logprobs"]; ok && !isNull(v) {
		i, rerr := jsonInt(v, "'generationConfig.logprobs' must be an integer")
		if rerr != nil || i < 0 || i > 20 {
			return d.bad("'generationConfig.logprobs' must be an integer between 0 and 20")
		}
		d.loseDrop("generationConfig.logprobs", "log probabilities are not translated")
	}
	if v, ok := obj["responseModalities"]; ok && !isNull(v) {
		var arr []json.RawMessage
		if err := json.Unmarshal(v, &arr); err != nil {
			return d.bad("'generationConfig.responseModalities' must be an array of strings")
		}
		for _, e := range arr {
			var s string
			if err := json.Unmarshal(e, &s); err != nil {
				return d.bad("'generationConfig.responseModalities' must be an array of strings")
			}
			if s != "TEXT" && s != "MODALITY_UNSPECIFIED" {
				d.loseDrop("generationConfig.responseModalities", "non-text output modalities are not produced by this destination")
				break
			}
		}
	}
	if rerr := d.responseFormat(obj); rerr != nil {
		return rerr
	}
	if v, ok := obj["thinkingConfig"]; ok && !isNull(v) {
		if rerr := d.thinking(v, g); rerr != nil {
			return rerr
		}
	}
	for _, k := range []string{"speechConfig", "imageConfig"} {
		if v, ok := obj[k]; ok && !isNull(v) {
			if k == "speechConfig" {
				if rerr := d.generationSpeechConfig("generationConfig."+k, v); rerr != nil {
					return rerr
				}
			} else {
				if rerr := d.generationImageConfig("generationConfig."+k, v); rerr != nil {
					return rerr
				}
			}
		}
	}
	if v, ok := obj["mediaResolution"]; ok && !isNull(v) {
		if rerr := d.generationMediaResolution("generationConfig.mediaResolution", v); rerr != nil {
			return rerr
		}
	}
	if v, ok := obj["enableEnhancedCivicAnswers"]; ok && !isNull(v) {
		var b bool
		if err := json.Unmarshal(v, &b); err != nil {
			return d.bad("'generationConfig.enableEnhancedCivicAnswers' must be bool")
		}
		d.loseDrop("generationConfig.enableEnhancedCivicAnswers", "this control has no equivalent on the destination")
	}
	if v, ok := obj["audioTimestamp"]; ok && !isNull(v) {
		if rerr := d.generationAudioTimestamp("generationConfig.audioTimestamp", v); rerr != nil {
			return rerr
		}
	}
	return nil
}

func (d *gdecDecoder) responseFormat(obj map[string]json.RawMessage) *RouterError {
	mimeRaw, hasMime := obj["responseMimeType"]
	schemaRaw, hasSchema := obj["responseSchema"], false
	if v, ok := obj["responseSchema"]; ok && !isNull(v) {
		schemaRaw, hasSchema = v, true
	}
	jsonRaw, hasJSON := obj["responseJsonSchema"], false
	if v, ok := obj["responseJsonSchema"]; ok && !isNull(v) {
		jsonRaw, hasJSON = v, true
	}
	if !hasMime && !hasSchema && !hasJSON {
		return nil
	}
	mime := ""
	if hasMime && !isNull(mimeRaw) {
		if err := json.Unmarshal(mimeRaw, &mime); err != nil {
			return d.bad("'generationConfig.responseMimeType' must be a string")
		}
	}
	if hasSchema && hasJSON {
		return d.bad("'generationConfig' sets both responseSchema and responseJsonSchema")
	}
	if (hasSchema || hasJSON) && mime != "application/json" {
		if mime == "text/x.enum" {
			d.loseDrop("generationConfig.responseMimeType", "enum output has no equivalent")
			return nil
		}
		return d.bad("'generationConfig.responseSchema' requires responseMimeType 'application/json'")
	}
	if hasSchema {
		if _, rerr := gdecConvertSchema(schemaRaw, "generationConfig.responseSchema", 0, d.lim); rerr != nil {
			return rerr
		}
		if gdecSchemaHasOrdering(schemaRaw) {
			d.loseDrop("generationConfig.responseSchema.propertyOrdering", "property ordering has no equivalent on this destination")
		}
	}
	if hasJSON {
		if _, rerr := gdecCompactObject("'generationConfig.responseJsonSchema'", jsonRaw, d.lim); rerr != nil {
			return rerr
		}
	}
	if mime == "" || mime == "text/plain" {
		if !hasSchema && !hasJSON {
			return nil
		}
	}
	d.loseDrop("generationConfig.responseMimeType", d.geminiEncoderControlLoss("response format"))
	return nil
}

func (d *gdecDecoder) thinking(raw json.RawMessage, g *Generation) *RouterError {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return d.bad("'generationConfig.thinkingConfig' must be an object")
	}
	obj, rerr := gdecObject(m, map[string]bool{
		"includeThoughts": true, "thinkingBudget": true, "thinkingLevel": true,
	}, "generationConfig.thinkingConfig")
	if rerr != nil {
		return rerr
	}
	where := "generationConfig.thinkingConfig"
	if v, ok := obj["includeThoughts"]; ok && !isNull(v) {
		var b bool
		if err := json.Unmarshal(v, &b); err != nil {
			return d.bad(fmt.Sprintf("'%s.includeThoughts' must be bool", where))
		}
		if b {
			d.loseDrop(where+".includeThoughts", d.geminiNoRepresentationLoss("thought summaries"))
		}
	}
	budgetRaw, hasBudget := obj["thinkingBudget"]
	levelRaw, hasLevel := obj["thinkingLevel"]
	hasBudget = hasBudget && !isNull(budgetRaw)
	hasLevel = hasLevel && !isNull(levelRaw)
	if hasBudget && hasLevel {
		return d.bad(fmt.Sprintf("'%s' sets both thinkingBudget and thinkingLevel", where))
	}
	if hasBudget {
		i, rerr := jsonInt(budgetRaw, "integer")
		if rerr != nil || i < -1 {
			return d.bad(fmt.Sprintf("'%s.thinkingBudget' must be an integer at least -1", where))
		}
		if i == -1 {
			// Model dynamic default: no plan, no loss, no origin.
		} else if d.dest == geminiDestAnthropic {
			d.loseDrop(where+".thinkingBudget", d.geminiNoRepresentationLoss("reasoning budgets"))
		} else if intent, ok := NewGeminiBudget(i); ok {
			g.Reasoning = intent
		} else {
			return d.bad(fmt.Sprintf("'%s.thinkingBudget' must be an integer at least -1", where))
		}
	}
	if hasLevel {
		var s string
		if err := json.Unmarshal(levelRaw, &s); err != nil {
			return d.bad(fmt.Sprintf("'%s.thinkingLevel' must be a string", where))
		}
		switch s {
		case "THINKING_LEVEL_UNSPECIFIED":
		case "MINIMAL", "LOW", "MEDIUM", "HIGH":
			if d.dest == geminiDestAnthropic {
				d.loseDrop(where+".thinkingLevel", d.geminiNoRepresentationLoss("reasoning levels"))
			} else if intent, ok := NewGeminiLevel(strings.ToLower(s)); ok {
				g.Reasoning = intent
			} else {
				return d.bad(fmt.Sprintf("'%s.thinkingLevel' is not a thinking level", where))
			}
		case "XHIGH":
			// Shared source grammar has no XHIGH (MINIMAL/LOW/MEDIUM/
			// HIGH only): reject for every translated destination to
			// match the source. Native raw passthrough is unchanged.
			return d.bad(fmt.Sprintf("'%s.thinkingLevel' is not a thinking level", where))
		default:
			return d.bad(fmt.Sprintf("'%s.thinkingLevel' is not a thinking level", where))
		}
	}
	return nil
}

// gdecSchemaKeys is the documented Gemini Schema (OpenAPI subset) member
// set. Unknown members are rejected, never normalized by guessing.
func gdecSchemaKeys() map[string]bool {
	return map[string]bool{
		"type": true, "format": true, "title": true, "description": true,
		"nullable": true, "enum": true, "maxItems": true, "minItems": true,
		"properties": true, "required": true, "minProperties": true,
		"maxProperties": true, "minLength": true, "maxLength": true,
		"pattern": true, "example": true, "anyOf": true,
		"propertyOrdering": true, "default": true, "items": true,
		"minimum": true, "maximum": true,
	}
}

var gdecSchemaInts = map[string]bool{
	"maxItems": true, "minItems": true, "minProperties": true,
	"maxProperties": true, "minLength": true, "maxLength": true,
}

// gdecSchemaHasOrdering reports whether raw schema text contains a
// non-null propertyOrdering member at any depth, for loss diagnostics.
func gdecSchemaHasOrdering(raw json.RawMessage) bool {
	var node map[string]json.RawMessage
	if err := json.Unmarshal(raw, &node); err != nil || node == nil {
		return false
	}
	for k, v := range node {
		if k == "propertyOrdering" || gdecSnake(k) == "propertyOrdering" {
			if !isNull(v) {
				return true
			}
		}
		if k == "properties" && !isNull(v) {
			var props map[string]json.RawMessage
			if err := json.Unmarshal(v, &props); err == nil {
				for _, pv := range props {
					if gdecSchemaHasOrdering(pv) {
						return true
					}
				}
			}
		}
		if k == "items" && !isNull(v) {
			if gdecSchemaHasOrdering(v) {
				return true
			}
		}
		if k == "anyOf" && !isNull(v) {
			var arr []json.RawMessage
			if err := json.Unmarshal(v, &arr); err == nil {
				for _, e := range arr {
					if gdecSchemaHasOrdering(e) {
						return true
					}
				}
			}
		}
	}
	return false
}

// gdecConvertSchema validates one Gemini Schema value against the
// documented REST shape and returns its compact form. Type names are
// lowercased per the documented Type enum (STRING -> string, ...);
// nullable widens the type; integer-valued bounds accept numeric strings.
// minimum/maximum must be exact JSON numbers (quoted strings are rejected)
// and preserve their literals; propertyOrdering is validated and diagnosed
// as a loss when meaning cannot be preserved. Nullable/type/anyOf
// combinations that cannot be preserved honestly are refused. Anything
// outside the documented shape is a 400, never a guessed schema.
//
// Source contracts (verified November 2026, primary Google REST):
//
//	Schema minimum/maximum are numbers, integer bounds are string-encoded
//	int64, type is a required Type enum:
//	https://ai.google.dev/api/generate-content
func gdecConvertSchema(raw json.RawMessage, where string, depth int, lim Limits) (json.RawMessage, *RouterError) {
	bad := &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: fmt.Sprintf("'%s' must be a valid function schema", where)}
	if depth > 32 {
		return nil, bad
	}
	if len(raw) > lim.MaxArgsChars {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: fmt.Sprintf("'%s' exceeds the size bound", where)}
	}
	var node map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&node); err != nil || node == nil {
		return nil, bad
	}
	obj, rerr := gdecObject(node, gdecSchemaKeys(), where)
	if rerr != nil {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: rerr.Msg}
	}
	result := map[string]any{}
	intVals := map[string]int64{}
	for k, v := range obj {
		if isNull(v) {
			continue
		}
		switch k {
		case "propertyOrdering":
			var arr []json.RawMessage
			if err := json.Unmarshal(v, &arr); err != nil {
				return nil, bad
			}
			for i, e := range arr {
				var s string
				if err := json.Unmarshal(e, &s); err != nil || s == "" {
					return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
						Msg: fmt.Sprintf("'%s.propertyOrdering[%d]' must be a non-empty string", where, i)}
				}
			}
			// Validated; the display-order meaning has no Chat
			// JSON-schema representation, so the caller records a loss.
			// Dropped here, never emitted downstream.
		case "type":
			var t string
			if err := json.Unmarshal(v, &t); err != nil {
				return nil, bad
			}
			switch t {
			case "STRING", "string":
				result["type"] = "string"
			case "NUMBER", "number":
				result["type"] = "number"
			case "INTEGER", "integer":
				result["type"] = "integer"
			case "BOOLEAN", "boolean":
				result["type"] = "boolean"
			case "ARRAY", "array":
				result["type"] = "array"
			case "OBJECT", "object":
				result["type"] = "object"
			case "NULL", "null":
				result["type"] = "null"
			case "TYPE_UNSPECIFIED":
				return nil, bad
			default:
				return nil, bad
			}
		case "format", "title", "description", "pattern":
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				return nil, bad
			}
			result[k] = s
		case "enum":
			var arr []json.RawMessage
			if err := json.Unmarshal(v, &arr); err != nil || len(arr) == 0 {
				return nil, bad
			}
			out := make([]any, 0, len(arr))
			for _, e := range arr {
				var val any
				d2 := json.NewDecoder(bytes.NewReader(e))
				d2.UseNumber()
				if err := d2.Decode(&val); err != nil {
					return nil, bad
				}
				out = append(out, val)
			}
			result["enum"] = out
		case "required":
			var arr []json.RawMessage
			if err := json.Unmarshal(v, &arr); err != nil {
				return nil, bad
			}
			out := make([]any, 0, len(arr))
			for i, e := range arr {
				var s string
				if err := json.Unmarshal(e, &s); err != nil || s == "" {
					return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
						Msg: fmt.Sprintf("'%s.required[%d]' must be a non-empty string", where, i)}
				}
				out = append(out, s)
			}
			result["required"] = out
		case "properties":
			var props map[string]json.RawMessage
			if err := json.Unmarshal(v, &props); err != nil || props == nil {
				return nil, bad
			}
			out := map[string]any{}
			names := make([]string, 0, len(props))
			for n := range props {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				c, rerr := gdecConvertSchema(props[n], where+".properties."+n, depth+1, lim)
				if rerr != nil {
					return nil, rerr
				}
				out[n] = json.RawMessage(c)
			}
			result["properties"] = out
		case "items":
			c, rerr := gdecConvertSchema(v, where+".items", depth+1, lim)
			if rerr != nil {
				return nil, rerr
			}
			result["items"] = json.RawMessage(c)
		case "anyOf":
			var arr []json.RawMessage
			if err := json.Unmarshal(v, &arr); err != nil || len(arr) == 0 {
				return nil, bad
			}
			out := make([]any, 0, len(arr))
			for i, e := range arr {
				c, rerr := gdecConvertSchema(e, fmt.Sprintf("%s.anyOf[%d]", where, i), depth+1, lim)
				if rerr != nil {
					return nil, rerr
				}
				out = append(out, json.RawMessage(c))
			}
			result["anyOf"] = out
		case "minimum", "maximum":
			f, rerr := gdecFiniteNumber(where+"."+k, v)
			if rerr != nil {
				return nil, rerr
			}
			// Preserve the literal without float64 round-trip.
			var val any
			d2 := json.NewDecoder(bytes.NewReader(v))
			d2.UseNumber()
			if err := d2.Decode(&val); err != nil {
				return nil, bad
			}
			result[k] = val
			_ = f
		case "example", "default":
			var val any
			d2 := json.NewDecoder(bytes.NewReader(v))
			d2.UseNumber()
			if err := d2.Decode(&val); err != nil {
				return nil, bad
			}
			result[k] = val
		default:
			if gdecSchemaInts[k] {
				var s string
				if err := json.Unmarshal(v, &s); err == nil {
					if s == "" {
						return nil, bad
					}
					for i := 0; i < len(s); i++ {
						if s[i] < '0' || s[i] > '9' {
							return nil, bad
						}
					}
					var n json.Number = json.Number(s)
					i, err := n.Int64()
					if err != nil || i < 0 {
						return nil, bad
					}
					result[k] = i
					intVals[k] = i
					continue
				}
				var f json.Number
				if err := json.Unmarshal(v, &f); err != nil {
					return nil, bad
				}
				i, err := f.Int64()
				if err != nil || i < 0 {
					return nil, bad
				}
				result[k] = i
				intVals[k] = i
				continue
			}
			if k == "nullable" {
				continue
			}
			return nil, bad
		}
	}
	if min, ok := intVals["minItems"]; ok {
		if max, ok := intVals["maxItems"]; ok && max < min {
			return nil, bad
		}
	}
	if min, ok := intVals["minProperties"]; ok {
		if max, ok := intVals["maxProperties"]; ok && max < min {
			return nil, bad
		}
	}
	if min, ok := intVals["minLength"]; ok {
		if max, ok := intVals["maxLength"]; ok && max < min {
			return nil, bad
		}
	}
	if minRaw, ok := obj["minimum"]; ok && !isNull(minRaw) {
		if maxRaw, ok := obj["maximum"]; ok && !isNull(maxRaw) {
			minF, rerr := gdecFiniteNumber(where+".minimum", minRaw)
			if rerr != nil {
				return nil, rerr
			}
			maxF, rerr := gdecFiniteNumber(where+".maximum", maxRaw)
			if rerr != nil {
				return nil, rerr
			}
			if maxF < minF {
				return nil, bad
			}
		}
	}
	if v, ok := obj["nullable"]; ok && !isNull(v) {
		var b bool
		if err := json.Unmarshal(v, &b); err != nil {
			return nil, bad
		}
		if b {
			_, hasType := result["type"]
			_, hasAnyOf := result["anyOf"]
			switch {
			case hasType && hasAnyOf:
				return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'%s' sets nullable with both type and anyOf with no honest mapping", where)}
			case hasType:
				t := result["type"].(string)
				result["type"] = []string{t, "null"}
			case hasAnyOf:
				arr := result["anyOf"].([]any)
				result["anyOf"] = append(arr, map[string]any{"type": "null"})
			default:
				return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'%s.nullable' has no type constraint to widen", where)}
			}
		}
	}
	data, err := json.Marshal(result)
	if err != nil {
		return nil, bad
	}
	return json.RawMessage(data), nil
}

// gdecFiniteNumber validates that raw is an exact JSON number literal (not a
// quoted string, bool, null or other type) and returns its finite value.
// Quoted numerics like "0.5" are rejected: the official Schema
// minimum/maximum are numbers, so a quoted source constraint is malformed.
func gdecFiniteNumber(where string, raw json.RawMessage) (float64, *RouterError) {
	bad := &RouterError{Status: 400, Code: CodeInvalidRequest,
		Msg: fmt.Sprintf("'%s' must be a finite JSON number", where)}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] == '"' {
		return 0, bad
	}
	c := trimmed[0]
	if c != '-' && (c < '0' || c > '9') {
		return 0, bad
	}
	var n json.Number
	if err := json.Unmarshal(trimmed, &n); err != nil {
		return 0, bad
	}
	f, err := n.Float64()
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, bad
	}
	// Confirm the literal round-trips as a JSON number (rejects quoted or
	// trailing data that Unmarshal alone might coerce).
	var check any
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	if err := dec.Decode(&check); err != nil {
		return 0, bad
	}
	if _, ok := check.(json.Number); !ok {
		return 0, bad
	}
	return f, nil
}
