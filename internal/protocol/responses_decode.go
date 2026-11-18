// Responses ingress decoding: closed, typed validation of a Responses API
// request into the semantic IR. Unknown fields are validation errors, never
// silent accepts. Known fields without a Chat bridge mapping become Loss
// records for the strict/lossy policy — but only when they are well-formed:
// malformed types/values and unknown nested members are validation errors
// in both modes, before any upstream I/O.
//
// Null policy mirrors the Chat and Anthropic decoders: explicit null on an
// optional field means "absent" and is skipped, except for `stream` (whose
// contract is an explicit bool) and nested boolean controls, where null is
// a type error. Omitted, null, zero and false are therefore distinguished
// explicitly at every boundary.
//
// Statelessness: the bridge carries no conversation store. Any request that
// refers to upstream-owned state (previous_response_id, conversation,
// prompt templates) or asks for background execution is refused before any
// upstream I/O, in both strict and lossy modes. Lossy mode is not
// permission to invent state the router does not keep.
//
// Scope (milestone 3, Responses -> Chat): text plus ordinary JSON-schema
// function tools, history and results. Anything else with a reviewed shape
// becomes a loss; anything without a reviewed Chat mapping (namespaces,
// hosted tools, reasoning beyond named effort, file/image inputs beyond the reviewed rules)
// is refused specifically, in both modes.
//
// Source: OpenAI Responses create reference
// (https://developers.openai.com/api/reference/resources/responses/methods/create).
package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// responsesDestination selects the small typed destination policy applied
// on top of the shared Responses source validation. Chat keeps the source
// temperature range and explicit strict exactly; Anthropic caps temperature
// at 1 (valid source values above become a diagnosed loss) and refuses
// explicit strict true.
type responsesDestination int

const (
	responsesDestChat responsesDestination = iota
	responsesDestAnthropic
	responsesDestGemini
)

// DecodeResponsesRequest decodes raw JSON Responses request bytes into the
// IR plus the planned losses. It performs no I/O.
func DecodeResponsesRequest(raw []byte, lim Limits) (*Generation, []Loss, *RouterError) {
	return decodeResponsesSource(raw, lim, responsesDestChat)
}

// decodeResponsesSource validates the shared Responses source contract once
// and applies only the small destination policy (temperature range and tool
// strict wording). Source validation, errors, and losses are otherwise
// identical for both destinations.
func decodeResponsesSource(raw []byte, lim Limits, dest responsesDestination) (*Generation, []Loss, *RouterError) {
	var top map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&top); err != nil {
		return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: "'body' must be a JSON object"}
	}
	if top == nil {
		return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: "'body' must be a JSON object"}
	}
	return decodeResponsesObject(top, lim, dest)
}

func decodeResponsesObject(top map[string]json.RawMessage, lim Limits, dest responsesDestination) (*Generation, []Loss, *RouterError) {
	var losses []Loss
	lose := func(field, reason string, approx bool) {
		action := "dropped"
		if approx {
			action = "approximated"
		}
		losses = append(losses, Loss{Field: field, Reason: reason, Action: action})
	}
	loseDrop := func(field, reason string) { lose(field, reason, false) }

	g := &Generation{}

	// model: required for routing.
	rawModel, ok := top["model"]
	if !ok {
		return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: "A model ID is required"}
	}
	var model string
	if err := json.Unmarshal(rawModel, &model); err != nil || strings.TrimSpace(model) == "" {
		return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: "A model ID is required"}
	}
	g.Model = model

	// stream: explicit bool only; null is a type error, not false.
	if rawStream, ok := top["stream"]; ok {
		if isNull(rawStream) {
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: "'stream' must be bool"}
		}
		var b bool
		if err := json.Unmarshal(rawStream, &b); err != nil {
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: "'stream' must be bool"}
		}
		g.Stream = b
	}

	// Closed field gate. Stateful references are refused before any loss
	// accounting; known-but-unbridged fields are validated for shape first
	// (a malformed value is a 400 even in lossy mode).
	// Mirrors the Python baseline: previous_response_id cannot be combined
	// with conversation (invalid_request), while any well-formed state
	// reference alone is unsupported_responses_state.
	if rawPrev, ok := top["previous_response_id"]; ok && !isNull(rawPrev) {
		if rawConv, ok := top["conversation"]; ok && !isNull(rawConv) {
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: "'previous_response_id' cannot be combined with 'conversation'"}
		}
	}
	for key := range top {
		switch key {
		case "model", "input", "instructions", "temperature", "top_p",
			"max_output_tokens", "stream", "stream_options",
			"tools", "tool_choice", "parallel_tool_calls":
			// handled below
		case "previous_response_id", "conversation", "prompt", "background":
			if rerr := refuseResponsesStateful(key, top[key]); rerr != nil {
				return nil, nil, rerr
			}
		case "metadata":
			if !isNull(top[key]) {
				return nil, nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
					Msg: "OpenAI metadata requires a same-protocol route"}
			}
		case "reasoning":
			if isNull(top[key]) {
				continue // explicit null means absent.
			}
			if dest != responsesDestChat && dest != responsesDestGemini {
				// Non-Chat/Gemini destinations retain the current hard refusal in
				// both modes: never silently disabled.
				if _, isObj := responsesNullableObject(top[key]); !isObj {
					return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
						Msg: "'reasoning' must be an object or null"}
				}
				return nil, nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
					Msg: "Responses reasoning requires a same-protocol route"}
			}
			if dest == responsesDestGemini {
				if rerr := decodeResponsesReasoningForGemini(top[key], &g.Reasoning); rerr != nil {
					return nil, nil, rerr
				}
				continue
			}
			if rerr := decodeResponsesReasoningForChat(top[key], &g.Reasoning, loseDrop); rerr != nil {
				return nil, nil, rerr
			}
		default:
			if knownResponsesField(key) {
				if isNull(top[key]) {
					continue // explicit null means absent.
				}
				if rerr := validateResponsesLossField(key, top[key], loseDrop); rerr != nil {
					return nil, nil, rerr
				}
				continue
			}
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", key)}
		}
	}

	num := func(key string, min, max float64) (*float64, *RouterError) {
		raw, ok := top[key]
		if !ok || isNull(raw) {
			return nil, nil
		}
		// json.Number unmarshaling accepts quoted strings; the contract
		// requires a JSON number literal, so a leading quote is malformed
		// in both modes.
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) > 0 && trimmed[0] == '"' {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'%s' must be a number", key)}
		}
		f, rerr := jsonFinite(raw, fmt.Sprintf("'%s' must be a number", key))
		if rerr != nil {
			return nil, rerr
		}
		if f < min || f > max {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'%s' is outside the supported numeric range", key)}
		}
		v := f
		return &v, nil
	}
	var rerr *RouterError
	// Source temperature spans 0-2 per the create reference. The Anthropic
	// destination caps at 1: valid source values above become a diagnosed
	// loss and are dropped, never clamped. The Gemini destination spans the
	// same [0.0, 2.0] range (generationConfig.temperature), so every valid
	// source value is exact and no branch is needed here.
	if g.Temperature, rerr = num("temperature", 0, 2); rerr != nil {
		return nil, nil, rerr
	}
	if dest == responsesDestAnthropic && g.Temperature != nil && *g.Temperature > 1 {
		loseDrop("temperature", "temperature above 1 has no Anthropic representation")
		g.Temperature = nil
	}
	if g.TopP, rerr = num("top_p", 0, 1); rerr != nil {
		return nil, nil, rerr
	}
	if rawMax, ok := top["max_output_tokens"]; ok && !isNull(rawMax) {
		i, rerr := jsonInt(rawMax, "'max_output_tokens' must be a positive integer")
		if rerr != nil || i <= 0 {
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: "'max_output_tokens' must be a positive integer"}
		}
		mt := int(i)
		g.MaxTokens = &mt
	}

	// stream_options: only include_obfuscation is bridged as a diagnosed
	// loss; anything else is a validation error in both modes.
	if rawSO, ok := top["stream_options"]; ok && !isNull(rawSO) {
		var so map[string]json.RawMessage
		if err := json.Unmarshal(rawSO, &so); err != nil || so == nil {
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: "'stream_options' must be an object"}
		}
		for k, v := range so {
			if k != "include_obfuscation" {
				return nil, nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
					Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", "stream_options."+k)}
			}
			if isNull(v) {
				return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: "'stream_options.include_obfuscation' must be bool"}
			}
			var b bool
			if err := json.Unmarshal(v, &b); err != nil {
				return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: "'stream_options.include_obfuscation' must be bool"}
			}
			loseDrop("stream_options."+k, "no reviewed bridge mapping")
		}
	}

	// instructions: a system/developer message inserted into the context.
	// Per the Responses create reference, the request field is a string
	// (or null for absent). Array forms appear only on echoed response
	// objects, never on requests, so a non-string is a 400.
	if rawInstr, ok := top["instructions"]; ok && !isNull(rawInstr) {
		var single string
		if err := json.Unmarshal(rawInstr, &single); err != nil {
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: "'instructions' must be a string"}
		}
		if single != "" {
			g.Messages = append(g.Messages, Message{Role: RoleSystem, Texts: []TextContent{{Text: single}}})
		}
	}

	// Namespace plan: validated once from the source tools + complete
	// history before any flattening. Choices, history, and returned calls
	// share this one plan; provenance comes from its bindings, never from
	// parsing an alias prefix. A nil/empty plan means no namespaces.
	nsPlan, flatTools, rerr := BuildResponsesNamespacePlan(top)
	if rerr != nil {
		return nil, nil, rerr
	}
	losses = append(losses, nsPlan.NamespaceLosses()...)

	// input: string shorthand or an ordered array of items. Item order is
	// preserved one-to-one in the IR; each item becomes exactly one message.
	customResults := map[string]bool{}
	if rawInput, ok := top["input"]; ok && !isNull(rawInput) {
		var single string
		if err := json.Unmarshal(rawInput, &single); err == nil {
			if single != "" {
				g.Messages = append(g.Messages, Message{Role: RoleUser, Texts: []TextContent{{Text: single}}})
			}
		} else {
			var items []json.RawMessage
			if err := json.Unmarshal(rawInput, &items); err != nil {
				return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: "'input' must be a string or an array of input items"}
			}
			for i, item := range items {
				if len(bytes.TrimSpace(item)) == 0 || isNull(item) {
					return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
						Msg: fmt.Sprintf("'input[%d]' must be an input item object", i)}
				}
				m, rerr := decodeResponsesInputItem(i, item, lose, lim, nsPlan, customResults)
				if rerr != nil {
					return nil, nil, rerr
				}
				g.Messages = append(g.Messages, *m)
			}
		}
	}
	if len(g.Messages) == 0 {
		return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: "'input' must contain at least one translatable message"}
	}

	// tools: ordinary JSON-schema function tools plus emulated freeform
	// custom tools, flattened from Responses namespace groups
	// (tools[i].tools[j]) into collision-safe aliases. Origins retain
	// tools[i] vs tools[i].tools[j] for losses.
	names := make(map[string]bool, len(flatTools))
	droppedOptionalSearch := false
	if rawTools, ok := top["tools"]; ok && !isNull(rawTools) {
		for i, item := range flatTools {
			if item == nil {
				return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: "'tools' must contain objects"}
			}
			flatPath := fmt.Sprintf("tools[%d]", i)
			origin, hasOrigin := nsPlan.OriginFor(flatPath)
			if !hasOrigin {
				origin = flatPath
			}
			var typ string
			if rawT, ok := item["type"]; ok && !isNull(rawT) {
				_ = json.Unmarshal(rawT, &typ)
			}
			if dest == responsesDestChat && typ == "web_search" && len(item) == 2 &&
				bytes.Equal(bytes.TrimSpace(item["external_web_access"]), []byte("false")) {
				var choice string
				rawChoice := top["tool_choice"]
				if droppedOptionalSearch || (len(rawChoice) != 0 && !isNull(rawChoice) &&
					(json.Unmarshal(rawChoice, &choice) != nil || (choice != "auto" && choice != "none"))) {
					return nil, nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
						Msg: "Cached web search requires a native route when tool use is forced or restricted"}
				}
				droppedOptionalSearch = true
				nsPlan.dropPlainDeclaration(origin)
				loseDrop(origin, "optional cached web search has no Chat mapping")
				continue
			}
			if typ == "custom" {
				name, desc, format, rerr := parseResponsesCustomSpec(origin, item, lim)
				if rerr != nil {
					return nil, nil, rerr
				}
				// The flattened wire name is the upstream alias; the
				// source identity comes from the namespace binding when
				// present, otherwise the plain name.
				var alias string
				_ = json.Unmarshal(item["name"], &alias)
				source := ToolIdentity{Name: name}
				if bound, ok := nsPlan.SourceFor(alias); ok {
					source = bound
				} else if alias != name {
					return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
						Msg: fmt.Sprintf("'%s.name' conflicts with another tool definition", origin)}
				} else {
					source = ToolIdentity{Name: alias}
				}
				if names[alias] {
					return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
						Msg: fmt.Sprintf("'%s.name' conflicts with another tool definition", origin)}
				}
				names[alias] = true
				nsPlan.addEmulatedCustom(alias, source)
				lose(origin, responsesCustomEmuReason, true)
				g.Tools = append(g.Tools, ToolDef{Kind: ToolKindCustom, Name: alias, Description: desc, Format: format})
				continue
			}
			def, rerr := decodeResponsesToolForDest(origin, item, lose, lim, dest)
			if rerr != nil {
				return nil, nil, rerr
			}
			if names[def.Name] {
				return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'%s.name' conflicts with another tool definition", origin)}
			}
			names[def.Name] = true
			g.Tools = append(g.Tools, *def)
		}
		g.HasTools = !droppedOptionalSearch || len(g.Tools) > 0
	}
	// A function definition sharing an alias with an emulated custom is an
	// ambiguous binding (custom history vs function tool, or vice versa).
	for _, t := range g.Tools {
		if t.Kind != ToolKindCustom && nsPlan.IsEmulatedCustom(t.Name) {
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'tools' has a conflicting tool identity for %q", t.Name)}
		}
	}
	if !nsPlan.Empty() {
		g.ToolPlan = nsPlan
	}

	// tool_choice: flattened through the same namespace plan.
	if rawTC, ok := top["tool_choice"]; ok && !isNull(rawTC) {
		choice, dropped, rerr := decodeResponsesToolChoice(rawTC, lose, dest, nsPlan, names)
		if rerr != nil {
			return nil, nil, rerr
		}
		if !dropped && (!droppedOptionalSearch || g.HasTools) {
			g.Choice = choice
		}
	}

	// parallel_tool_calls: explicit bool; null means absent. The Gemini
	// destination allows parallel function calls by default with no disable
	// control, so an explicit false is a diagnosed loss there, never
	// silently dropped; true or omitted is exact.
	if rawPTC, ok := top["parallel_tool_calls"]; ok && !isNull(rawPTC) {
		var b bool
		if err := json.Unmarshal(rawPTC, &b); err != nil {
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: "'parallel_tool_calls' must be bool"}
		}
		g.ParallelToolCalls = &b
		if dest == responsesDestGemini && !b {
			loseDrop("parallel_tool_calls", "Gemini generations allow parallel calls; disabling has no mapping")
		}
	}

	if (g.Choice != nil && (g.Choice.Mode == "specific" || g.Choice.Mode == "any")) && !g.HasTools {
		return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: "'tool_choice' requires 'tools'"}
	}
	if g.Choice != nil && g.Choice.Mode == "specific" && !names[g.Choice.Name] {
		return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: "'tool_choice' names an unknown tool"}
	}
	if g.Choice != nil && g.Choice.Mode == "specific" {
		wantCustom := g.Choice.IsCustomChoice()
		isCustom := nsPlan.IsEmulatedCustom(g.Choice.Name)
		if wantCustom != isCustom {
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: "'tool_choice' names an unknown tool"}
		}
	}

	// Historical call/result identities must match: every tool result must
	// reference a known assistant call ID, and IDs must not collide.
	// Custom outputs must reference custom calls; function outputs must
	// reference function calls. Mismatched bindings fail closed.
	callIDs := map[string]bool{}
	customCalls := map[string]bool{}
	for mi, m := range g.Messages {
		for _, tc := range m.ToolCalls {
			if callIDs[tc.ID] {
				return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'input[%d]' uses a conflicting call identity", mi)}
			}
			callIDs[tc.ID] = true
			if tc.IsCustom {
				customCalls[tc.ID] = true
			} else if nsPlan.IsEmulatedCustom(tc.Name) {
				return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'input[%d]' uses a conflicting tool identity", mi)}
			}
		}
	}
	for callID := range customResults {
		if !customCalls[callID] {
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'input' references an unmatched tool call identity for %q", callID)}
		}
	}
	seenResults := map[string]bool{}
	for mi, m := range g.Messages {
		for _, r := range m.Results {
			if !callIDs[r.CallID] {
				return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'input[%d]' references an unmatched tool call identity", mi)}
			}
			if seenResults[r.CallID] {
				return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'input[%d]' has a conflicting tool result identity", mi)}
			}
			isCustomResult := customResults[r.CallID]
			isCustomCall := customCalls[r.CallID]
			if isCustomResult != isCustomCall {
				return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'input[%d]' uses a conflicting tool identity", mi)}
			}
			seenResults[r.CallID] = true
		}
	}

	return g, boundResponsesLosses(losses), nil
}

// destBridgeName names the destination bridge in diagnostics. Existing
// Chat/Anthropic wording is preserved byte-for-byte.
func destBridgeName(dest responsesDestination) string {
	switch dest {
	case responsesDestAnthropic:
		return "Anthropic"
	case responsesDestGemini:
		return "Gemini"
	default:
		return "Chat"
	}
}

// boundResponsesLosses sorts losses by field path for deterministic
// diagnostics and caps the record count so headers and error text stay
// finite. It shares the bound with the Chat and Anthropic decoders.
func boundResponsesLosses(losses []Loss) []Loss {
	sort.SliceStable(losses, func(i, j int) bool { return losses[i].Field < losses[j].Field })
	if len(losses) <= maxLossRecords {
		return losses
	}
	kept := append([]Loss(nil), losses[:maxLossRecords-1]...)
	return append(kept, Loss{Field: "losses", Reason: "additional losses truncated at bound", Action: "dropped"})
}

// refuseResponsesStateful validates a stateful request field and always
// refuses it: the stateless bridge keeps no conversation store, runs no
// background generations, and resolves no prompt templates. Explicit null
// means absent; anything else is refused before any upstream I/O, in both
// strict and lossy modes.
func refuseResponsesStateful(key string, raw json.RawMessage) *RouterError {
	if isNull(raw) {
		return nil
	}
	bad := func(msg string) *RouterError {
		return &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: msg}
	}
	refuse := func(msg string) *RouterError {
		return &RouterError{Status: 400, Code: "unsupported_responses_state", Msg: msg}
	}
	switch key {
	case "previous_response_id":
		var s string
		if err := json.Unmarshal(raw, &s); err != nil || s == "" {
			return bad("'previous_response_id' must be a response ID string or null")
		}
		return refuse("Responses conversation state requires a same-protocol route; the bridge is stateless")
	case "conversation":
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			if s == "" {
				return bad("'conversation' must be a conversation ID, object or null")
			}
			return refuse("Responses conversation state requires a same-protocol route; the bridge is stateless")
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
			return bad("'conversation' must be a conversation ID, object or null")
		}
		return refuse("Responses conversation state requires a same-protocol route; the bridge is stateless")
	case "prompt":
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
			return bad("'prompt' must be a prompt template object or null")
		}
		return refuse("Responses prompt templates require a same-protocol route; the bridge is stateless")
	case "background":
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return bad("'background' must be bool")
		}
		if !b {
			return nil
		}
		return refuse("Responses background execution requires a same-protocol route")
	}
	return nil
}

// responsesNullableObject reports whether raw is a JSON object (null counts
// as absent and returns false without an error signal here).
func responsesNullableObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, false
	}
	return obj, true
}

// decodeResponsesReasoningForChat validates the closed Responses reasoning
// object for the Chat destination ONLY. Null is handled by the caller;
// an empty object carries no control. Known nested members (effort,
// summary, generate_summary) must be strings when present: null, numbers,
// booleans and other types are malformed in both modes, and unknown nested
// members are closed-shape errors in both modes (mirroring Python
// _closed_generation_object, which requires exact str types and rejects
// unknown keys before plan_reasoning). Codex's automatic summary request
// becomes a diagnosed loss. Explicit summary formats and generate_summary
// require a Responses destination. Effort plans independently.
func decodeResponsesReasoningForChat(raw json.RawMessage, out *ReasoningIntent, loseDrop func(string, string)) *RouterError {
	obj, isObj := responsesNullableObject(raw)
	if !isObj {
		return &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: "'reasoning' must be an object or null"}
	}
	if len(obj) == 0 {
		return nil // empty object: no control.
	}
	var effort *string
	var summary string
	var hasGenerateSummary bool
	for k, v := range obj {
		switch k {
		case "effort", "summary", "generate_summary":
		default:
			return &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", "reasoning."+k)}
		}
		if isNull(v) {
			return &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("'reasoning.%s' must be a string", k)}
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil || s == "" {
			return &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("'reasoning.%s' must be a string", k)}
		}
		switch k {
		case "effort":
			v := s
			effort = &v
		case "summary":
			summary = s
		case "generate_summary":
			hasGenerateSummary = true
		}
	}
	if (summary != "" && summary != "auto") || hasGenerateSummary {
		return &RouterError{Status: 400, Code: CodeInvalidGeneration,
			Msg: "Responses reasoning requires a same-protocol route"}
	}
	if effort != nil {
		intent, ok := NewResponsesEffort(*effort)
		if !ok {
			return &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: "'reasoning.effort' must be a string"}
		}
		*out = intent
	}
	if summary == "auto" {
		loseDrop("reasoning.summary", "automatic reasoning summaries have no Chat mapping")
	}
	return nil
}

// decodeResponsesReasoningForGemini validates the closed Responses reasoning
// object for the Gemini destination ONLY. Null and empty objects are handled
// by the caller contract (absent). The shape mirrors
// decodeResponsesReasoningForChat: known nested members must be non-empty
// strings, unknown nested members are closed-shape 400s in both modes, and
// well-formed summary/generate_summary controls have no Gemini mapping and
// are refused pre-I/O in both modes even when effort is present. A
// well-formed effort becomes the closed Responses-origin intent; otherwise
// the intent stays absent.
func decodeResponsesReasoningForGemini(raw json.RawMessage, out *ReasoningIntent) *RouterError {
	obj, isObj := responsesNullableObject(raw)
	if !isObj {
		return &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: "'reasoning' must be an object or null"}
	}
	if len(obj) == 0 {
		return nil // empty object: no control.
	}
	var effort *string
	var hasSummary, hasGenerateSummary bool
	for k, v := range obj {
		switch k {
		case "effort", "summary", "generate_summary":
		default:
			return &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", "reasoning."+k)}
		}
		if isNull(v) {
			return &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("'reasoning.%s' must be a string", k)}
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil || s == "" {
			return &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("'reasoning.%s' must be a string", k)}
		}
		switch k {
		case "effort":
			v := s
			effort = &v
		case "summary":
			hasSummary = true
		case "generate_summary":
			hasGenerateSummary = true
		}
	}
	if hasSummary || hasGenerateSummary {
		return &RouterError{Status: 400, Code: CodeInvalidGeneration,
			Msg: "Responses reasoning requires a same-protocol route"}
	}
	if effort != nil {
		intent, ok := NewResponsesEffort(*effort)
		if !ok {
			return &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: "'reasoning.effort' must be a string"}
		}
		*out = intent
	}
	return nil
}

// knownResponsesField is the closed universe of Responses request fields
// recognized by the generation contract. Fields here without a Chat bridge
// become diagnosed losses when well-formed; anything else is a validation
// error.
func knownResponsesField(key string) bool {
	switch key {
	case "model", "input", "instructions", "tools", "tool_choice",
		"parallel_tool_calls", "temperature", "top_p", "max_output_tokens",
		"max_tool_calls", "stream", "stream_options", "store", "metadata", "client_metadata",
		"truncation", "reasoning", "text", "include", "service_tier",
		"safety_identifier", "prompt_cache_key", "prompt_cache_retention",
		"prompt_cache_options", "user", "context_management", "moderation",
		"access_programs", "image", "top_logprobs",
		"previous_response_id", "conversation", "prompt", "background":
		return true
	}
	return false
}

// validateResponsesLossField strictly validates a known-but-unbridged field:
// a well-formed value becomes a loss, a malformed one is a 400 in both
// modes. Only reviewed shapes reach the loss plan; nothing here is
// forwarded to the Chat upstream.
func validateResponsesLossField(key string, raw json.RawMessage, lose func(string, string)) *RouterError {
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
	mustObj := func() (map[string]json.RawMessage, *RouterError) {
		obj, ok := responsesNullableObject(raw)
		if !ok {
			return nil, bad(fmt.Sprintf("'%s' must be an object", key))
		}
		return obj, nil
	}
	switch key {
	case "client_metadata":
		obj, rerr := mustObj()
		if rerr != nil {
			return rerr
		}
		for _, value := range obj {
			var s string
			if isNull(value) || json.Unmarshal(value, &s) != nil {
				return bad("'client_metadata' values must be strings")
			}
		}
		return loss("no reviewed bridge mapping")
	case "store":
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return bad(fmt.Sprintf("'%s' must be bool", key))
		}
		if b {
			return loss("stateless bridge cannot preserve stored state; same-protocol route required")
		}
		// store:false is an exact no-storage request: omitting downstream
		// store preserves the Chat default (no storage) with no loss.
		return nil
	case "max_tool_calls":
		if i, rerr := jsonInt(raw, ""); rerr != nil || i < 0 {
			return bad("'max_tool_calls' must be a non-negative integer")
		}
		return loss("no reviewed bridge mapping")
	case "top_logprobs":
		if i, rerr := jsonInt(raw, ""); rerr != nil || i < 0 || i > 20 {
			return bad("'top_logprobs' must be an integer between 0 and 20")
		}
		return loss("no reviewed bridge mapping")
	case "truncation":
		var s string
		if err := json.Unmarshal(raw, &s); err != nil || (s != "auto" && s != "disabled") {
			return bad("'truncation' must be 'auto' or 'disabled'")
		}
		return loss("no reviewed bridge mapping")
	case "service_tier":
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return bad("'service_tier' must be a string")
		}
		switch s {
		case "auto", "default", "flex", "scale", "priority", "fast", "ultrafast":
			return loss("no reviewed bridge mapping")
		}
		return bad("'service_tier' must be 'auto', 'default', 'flex', 'scale', 'priority', 'fast' or 'ultrafast'")
	case "safety_identifier", "prompt_cache_key":
		return mustStr(true)
	case "user":
		return mustStr(false)
	case "prompt_cache_retention":
		var s string
		if err := json.Unmarshal(raw, &s); err != nil || (s != "in_memory" && s != "24h") {
			return bad("'prompt_cache_retention' must be 'in_memory' or '24h'")
		}
		return loss("no reviewed bridge mapping")
	case "prompt_cache_options":
		obj, rerr := mustObj()
		if rerr != nil {
			return rerr
		}
		for k, v := range obj {
			switch k {
			case "mode":
				var m string
				if err := json.Unmarshal(v, &m); err != nil || (m != "implicit" && m != "explicit") {
					return bad("'prompt_cache_options.mode' must be 'implicit' or 'explicit'")
				}
			case "ttl":
				var ttl string
				if err := json.Unmarshal(v, &ttl); err != nil || (ttl != "30m" && ttl != "12h") {
					return bad("'prompt_cache_options.ttl' must be '30m' or '12h'")
				}
			case "prewarm":
				var b bool
				if err := json.Unmarshal(v, &b); err != nil {
					return bad("'prompt_cache_options.prewarm' must be bool")
				}
			case "comparison_response_id":
				if isNull(v) {
					continue
				}
				var s string
				if err := json.Unmarshal(v, &s); err != nil || s == "" {
					return bad("'prompt_cache_options.comparison_response_id' must be a string")
				}
			default:
				return bad(fmt.Sprintf("Unsupported or invalid request field for translation: %q", "prompt_cache_options."+k))
			}
		}
		return loss("no reviewed bridge mapping")
	case "text":
		obj, rerr := mustObj()
		if rerr != nil {
			return rerr
		}
		for k, v := range obj {
			switch k {
			case "format":
				fobj, ok := responsesNullableObject(v)
				if !ok {
					return bad("'text.format' must be an object")
				}
				var typ string
				rawT, ok := fobj["type"]
				if !ok || isNull(rawT) {
					return bad("'text.format.type' is required")
				}
				if err := json.Unmarshal(rawT, &typ); err != nil ||
					(typ != "text" && typ != "json_object" && typ != "json_schema") {
					return bad("'text.format.type' must be 'text', 'json_object' or 'json_schema'")
				}
				for fk, fv := range fobj {
					switch fk {
					case "type":
					case "name", "description":
						var s string
						if err := json.Unmarshal(fv, &s); err != nil || s == "" {
							return bad(fmt.Sprintf("'text.format.%s' must be a non-empty string", fk))
						}
					case "schema":
						var sv any
						dec := json.NewDecoder(bytes.NewReader(fv))
						dec.UseNumber()
						if err := dec.Decode(&sv); err != nil {
							return bad("'text.format.schema' must be a JSON object")
						}
						if _, isObj := sv.(map[string]any); !isObj {
							return bad("'text.format.schema' must be a JSON object")
						}
					case "strict":
						if isNull(fv) {
							continue
						}
						var b bool
						if err := json.Unmarshal(fv, &b); err != nil {
							return bad("'text.format.strict' must be bool")
						}
					default:
						return bad(fmt.Sprintf("Unsupported or invalid request field for translation: %q", "text.format."+fk))
					}
				}
			case "verbosity":
				if isNull(v) {
					continue
				}
				var s string
				if err := json.Unmarshal(v, &s); err != nil || (s != "low" && s != "medium" && s != "high") {
					return bad("'text.verbosity' must be 'low', 'medium' or 'high'")
				}
			default:
				return bad(fmt.Sprintf("Unsupported or invalid request field for translation: %q", "text."+k))
			}
		}
		return loss("no reviewed bridge mapping")
	case "include":
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err != nil {
			return bad("'include' must be an array of strings")
		}
		for _, e := range arr {
			var s string
			if err := json.Unmarshal(e, &s); err != nil {
				return bad("'include' must be an array of strings")
			}
			switch s {
			case "file_search_call.results", "web_search_call.results",
				"web_search_call.action.sources", "message.input_image.image_url",
				"computer_call_output.output.image_url", "code_interpreter_call.outputs",
				"reasoning.encrypted_content", "message.output_text.logprobs":
			default:
				return bad(fmt.Sprintf("Unsupported or invalid request field for translation: %q", "include value "+s))
			}
		}
		return loss("no reviewed bridge mapping")
	case "context_management":
		var arr []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &arr); err != nil {
			return bad("'context_management' must be an array of objects")
		}
		for i, e := range arr {
			if e == nil {
				return bad("'context_management' must be an array of objects")
			}
			prefix := fmt.Sprintf("'context_management[%d]'", i)
			for k := range e {
				if k != "type" && k != "compact_threshold" {
					return bad(fmt.Sprintf("Unsupported or invalid request field for translation: %q", fmt.Sprintf("context_management[%d].%s", i, k)))
				}
			}
			var typ string
			if rawT, ok := e["type"]; !ok || isNull(rawT) || json.Unmarshal(rawT, &typ) != nil || typ != "compaction" {
				return bad(prefix + ".type' must be 'compaction'")
			}
			if rawC, ok := e["compact_threshold"]; ok && !isNull(rawC) {
				if c, rerr := jsonInt(rawC, ""); rerr != nil || c < 1000 {
					return bad(prefix + ".compact_threshold' must be an integer of at least 1000")
				}
			}
		}
		return loss("no reviewed bridge mapping")
	case "moderation":
		obj, rerr := mustObj()
		if rerr != nil {
			return rerr
		}
		for k, v := range obj {
			switch k {
			case "model":
				var s string
				if err := json.Unmarshal(v, &s); err != nil || s == "" {
					return bad("'moderation.model' must be a non-empty string")
				}
			case "policy":
				pobj, ok := responsesNullableObject(v)
				if !ok {
					return bad("'moderation.policy' must be an object")
				}
				for pk, pv := range pobj {
					if pk != "input" && pk != "output" {
						return bad(fmt.Sprintf("Unsupported or invalid request field for translation: %q", "moderation.policy."+pk))
					}
					mobj, ok := responsesNullableObject(pv)
					if !ok {
						return bad(fmt.Sprintf("'moderation.policy.%s' must be an object", pk))
					}
					for mk, mv := range mobj {
						if mk != "mode" {
							return bad(fmt.Sprintf("Unsupported or invalid request field for translation: %q", fmt.Sprintf("moderation.policy.%s.%s", pk, mk)))
						}
						var mode string
						if err := json.Unmarshal(mv, &mode); err != nil || (mode != "score" && mode != "block") {
							return bad(fmt.Sprintf("'moderation.policy.%s.mode' must be 'score' or 'block'", pk))
						}
					}
				}
			default:
				return bad(fmt.Sprintf("Unsupported or invalid request field for translation: %q", "moderation."+k))
			}
		}
		return loss("no reviewed bridge mapping")
	case "access_programs":
		obj, rerr := mustObj()
		if rerr != nil {
			return rerr
		}
		for k, v := range obj {
			if k != "cyber" {
				return bad(fmt.Sprintf("Unsupported or invalid request field for translation: %q", "access_programs."+k))
			}
			var s string
			if err := json.Unmarshal(v, &s); err != nil ||
				(s != "standard" && s != "daybreak_blue" && s != "daybreak_red") {
				return bad("'access_programs.cyber' must be 'standard', 'daybreak_blue' or 'daybreak_red'")
			}
		}
		return loss("no reviewed bridge mapping")
	case "image":
		obj, rerr := mustObj()
		if rerr != nil {
			return rerr
		}
		for k, v := range obj {
			if k != "context_compression" {
				return bad(fmt.Sprintf("Unsupported or invalid request field for translation: %q", "image."+k))
			}
			var s string
			if err := json.Unmarshal(v, &s); err != nil || (s != "auto" && s != "enabled" && s != "disabled") {
				return bad("'image.context_compression' must be 'auto', 'enabled' or 'disabled'")
			}
		}
		return loss("no reviewed bridge mapping")
	default:
		lose(key, "no reviewed bridge mapping")
		return nil
	}
}

// responsesOrderedText extracts ordered text from a string or an array of
// input content parts. Unknown part types and malformed members are 400s in
// both modes; image parts are diagnosed losses; file parts are refused.
func responsesOrderedText(prefix string, raw json.RawMessage, lose func(string, string, bool)) (string, *RouterError) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: prefix + ".content' must be a string or an array of content parts"}
	}
	var sb strings.Builder
	for pi, p := range parts {
		if p == nil {
			return "", &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: prefix + ".content' must contain content part objects"}
		}
		pp := fmt.Sprintf("%s.content[%d]", prefix, pi)
		var typ string
		if rawT, ok := p["type"]; ok && !isNull(rawT) {
			_ = json.Unmarshal(rawT, &typ)
		}
		switch typ {
		case "input_text":
			for k := range p {
				switch k {
				case "type", "text":
				default:
					return "", &RouterError{Status: 400, Code: CodeInvalidGeneration,
						Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", pp+"."+k)}
				}
			}
			var text string
			if rawT, ok := p["text"]; !ok || isNull(rawT) || json.Unmarshal(rawT, &text) != nil {
				return "", &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: pp + ".text' must be a string"}
			}
			sb.WriteString(text)
		case "input_image":
			if rerr := validateResponsesImagePart(pp, p); rerr != nil {
				return "", rerr
			}
			lose(pp+"."+typ, "no reviewed bridge mapping", false)
		case "input_file":
			return "", &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("%s file inputs require a same-protocol route", pp)}
		default:
			return "", &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", pp+".type")}
		}
	}
	return sb.String(), nil
}

// validateResponsesImagePart checks the reviewed image wrapper shape: at
// least one of file_id/image_url, string members only, known detail enum.
func validateResponsesImagePart(prefix string, p map[string]json.RawMessage) *RouterError {
	for k := range p {
		switch k {
		case "type", "detail", "file_id", "image_url":
		default:
			return &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", prefix+"."+k)}
		}
	}
	if rawD, ok := p["detail"]; ok && !isNull(rawD) {
		var d string
		if err := json.Unmarshal(rawD, &d); err != nil ||
			(d != "low" && d != "high" && d != "auto" && d != "original") {
			return &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: prefix + ".detail' must be 'low', 'high', 'auto' or 'original'"}
		}
	}
	hasRef := false
	for _, k := range []string{"file_id", "image_url"} {
		if rawV, ok := p[k]; ok && !isNull(rawV) {
			var s string
			if err := json.Unmarshal(rawV, &s); err != nil || s == "" {
				return &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: prefix + "." + k + "' must be a non-empty string"}
			}
			hasRef = true
		}
	}
	if !hasRef {
		return &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: prefix + "' requires 'file_id' or 'image_url'"}
	}
	return nil
}

// decodeResponsesInputItem decodes one ordered `input` array element into
// exactly one IR message, preserving the caller's item order. Namespaced
// function_call history flattens through the request plan; custom history
// decodes to typed freeform calls with emulated bindings.
func decodeResponsesInputItem(i int, raw json.RawMessage, lose func(string, string, bool), lim Limits, plan *ToolPlan, customResults map[string]bool) (*Message, *RouterError) {
	prefix := fmt.Sprintf("input[%d]", i)
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s' must be an input item object", prefix)}
	}
	var typ string
	if rawT, ok := obj["type"]; ok && !isNull(rawT) {
		if err := json.Unmarshal(rawT, &typ); err != nil {
			return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", prefix+".type")}
		}
	}
	switch typ {
	case "", "message":
		return decodeResponsesMessageItem(prefix, obj, lose)
	case "function_call":
		return decodeResponsesFunctionCall(prefix, obj, lim, plan)
	case "function_call_output":
		return decodeResponsesFunctionCallOutput(prefix, obj, lose, lim)
	case "custom_tool_call":
		return decodeResponsesCustomCall(prefix, obj, lose, lim, plan)
	case "custom_tool_call_output":
		return decodeResponsesCustomCallOutput(prefix, obj, lose, lim, plan, customResults)
	default:
		return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
			Msg: fmt.Sprintf("'%s' block type %q requires a same-protocol route", prefix, typ)}
	}
}

// decodeResponsesMessageItem decodes a text message input.
func decodeResponsesMessageItem(prefix string, obj map[string]json.RawMessage, lose func(string, string, bool)) (*Message, *RouterError) {
	for k := range obj {
		switch k {
		case "type", "role", "content", "status", "phase", "id":
		default:
			return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", prefix+"."+k)}
		}
	}
	if rawT, ok := obj["type"]; ok && !isNull(rawT) {
		var t string
		_ = json.Unmarshal(rawT, &t)
		if t != "message" {
			return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", prefix+".type")}
		}
	}
	var role string
	if rawR, ok := obj["role"]; ok && !isNull(rawR) {
		_ = json.Unmarshal(rawR, &role)
	}
	if role == "" {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s.role' is required", prefix)}
	}
	// Upstream item IDs are same-dialect artifacts; the bridge preserves
	// call identity through call_id, so a well-formed id is accepted here.
	if rawID, ok := obj["id"]; ok && !isNull(rawID) {
		var id string
		if err := json.Unmarshal(rawID, &id); err != nil || id == "" {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'%s.id' must be a string", prefix)}
		}
	}
	for _, k := range []string{"status", "phase"} {
		if rawV, ok := obj[k]; ok && !isNull(rawV) {
			var s string
			if err := json.Unmarshal(rawV, &s); err != nil || s == "" {
				return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'%s.%s' must be a string", prefix, k)}
			}
		}
	}
	m := &Message{}
	switch role {
	case "system":
		m.Role = RoleSystem
	case "user":
		m.Role = RoleUser
	case "assistant":
		m.Role = RoleAssistant
	case "developer":
		m.Role = RoleSystem
		lose(prefix+".role", "target system instructions cannot preserve developer role precedence", true)
	default:
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s.role' must be 'user', 'system', 'developer' or 'assistant'", prefix)}
	}
	rawC, ok := obj["content"]
	if !ok || isNull(rawC) {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s.content' is required", prefix)}
	}
	text, rerr := responsesOrderedText("'"+prefix+"'", rawC, lose)
	if rerr != nil {
		return nil, rerr
	}
	if text != "" {
		m.Texts = append(m.Texts, TextContent{Text: text})
	}
	return m, nil
}

// decodeResponsesFunctionCall decodes one historical function_call item into
// an assistant message carrying one tool call. The stable call identity is
// call_id; the upstream item id (when present) is validated but not carried
// over the Chat bridge, which has no separate item-identity field.
func decodeResponsesHistoryCallID(prefix string, raw json.RawMessage, lim Limits) (string, *RouterError) {
	id, valid := decodeUnicodeString(raw)
	if !valid {
		return "", &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s.call_id' must be a nonempty string", prefix)}
	}
	if err := validateCallID(id, lim); err != nil {
		return "", &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s.call_id' %s", prefix, err.Error())}
	}
	return id, nil
}

func decodeResponsesFunctionCall(prefix string, obj map[string]json.RawMessage, lim Limits, plan *ToolPlan) (*Message, *RouterError) {
	for k := range obj {
		switch k {
		case "type", "id", "call_id", "name", "arguments", "status", "namespace", "caller":
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
	// Router-minted wrapped identities (Gemini thought-signature
	// passback) restore the provider identity and the opaque
	// signature; any other value passes through untouched.
	sig := ""
	if s, inner, ok := unwrapGeminiSignatureID(callID); ok {
		if err := validateCallID(inner, lim); err != nil {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'%s.call_id' %s", prefix, err.Error())}
		}
		callID, sig = inner, s
	}
	var name string
	if rawN, ok := obj["name"]; ok && !isNull(rawN) {
		_ = json.Unmarshal(rawN, &name)
	}
	if rawNS, ok := obj["namespace"]; ok && !isNull(rawNS) {
		ns, valid := decodeUnicodeString(rawNS)
		if !valid {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'%s.namespace' must be a string", prefix)}
		}
		if ns != "" {
			alias, ok := plan.AliasFor(ns, name)
			if plan.Empty() || !ok {
				return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'%s' names an unknown tool", prefix)}
			}
			name = alias
		}
	}
	if err := validateToolName(name); err != nil {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s.name' %s", prefix, err.Error())}
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
	args, rerr := decodeResponsesArguments(prefix, obj["arguments"], lim)
	if rerr != nil {
		return nil, rerr
	}
	return &Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: callID, Name: name, Arguments: args, Signature: sig}}}, nil
}

// decodeResponsesFunctionCallOutput decodes one function_call_output item
// into a tool message carrying one result. String output is exact; an array
// of text parts is concatenated in order as an approximation; any other
// output shape requires a same-protocol route.
func decodeResponsesFunctionCallOutput(prefix string, obj map[string]json.RawMessage, lose func(string, string, bool), lim Limits) (*Message, *RouterError) {
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
	// Wrapped identities restore the provider identity so the result
	// pairs with the unwrapped assistant call; any other value passes
	// through untouched.
	if _, inner, isWrapped := unwrapGeminiSignatureID(callID); isWrapped {
		callID = inner
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
	return &Message{Role: RoleTool, Results: []ToolResult{{CallID: callID, Content: content}}}, nil
}

// decodeResponsesArguments validates a JSON-stringified tool argument value:
// it must decode to a JSON object (never a scalar or array) and is
// preserved as compact raw JSON without a float64 round-trip, so large
// integers and number literals survive exactly. Absent, null or empty input
// means "no arguments" and is carried as an explicit empty object so empty
// tools never vanish.
func decodeResponsesArguments(prefix string, raw json.RawMessage, lim Limits) (json.RawMessage, *RouterError) {
	bad := &RouterError{Status: 400, Code: CodeInvalidRequest,
		Msg: fmt.Sprintf("'%s.arguments' must be a JSON string holding an object", prefix)}
	if len(raw) == 0 || isNull(raw) {
		return json.RawMessage("{}"), nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, bad
	}
	if strings.TrimSpace(s) == "" {
		return json.RawMessage("{}"), nil
	}
	if len(s) > lim.MaxArgsChars {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s.arguments' exceeds the size bound", prefix)}
	}
	trimmed := bytes.TrimSpace([]byte(s))
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, bad
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(s)); err != nil {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s.arguments' must be valid JSON", prefix)}
	}
	return json.RawMessage(compact.Bytes()), nil
}

// decodeResponsesToolForDest validates the shared ordinary-function-tool
// shape once and applies only the small destination strict policy: omitted
// or null strict is a diagnosed loss for all three destinations, explicit
// false is exact for all three, explicit true is retained for Chat, refused
// for Anthropic where strict guarantees are not reviewed, and diagnosed as
// a loss for Gemini where FunctionDeclarations carry no strict guarantee.
// Strictness is never silently weakened.
func decodeResponsesToolForDest(prefix string, item map[string]json.RawMessage, lose func(string, string, bool), lim Limits, dest responsesDestination) (*ToolDef, *RouterError) {
	var typ string
	if raw, ok := item["type"]; ok && !isNull(raw) {
		if err := json.Unmarshal(raw, &typ); err != nil {
			return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", prefix+".type")}
		}
	}
	if typ != "" && typ != "function" {
		bridge := destBridgeName(dest)
		return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
			Msg: fmt.Sprintf("Tool type %q has no %s bridge mapping; ordinary function tools only", typ, bridge)}
	}
	for k := range item {
		switch k {
		case "type", "name", "description", "parameters", "strict":
		default:
			return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", prefix+"."+k)}
		}
	}
	var name string
	if rawN, ok := item["name"]; ok && !isNull(rawN) {
		_ = json.Unmarshal(rawN, &name)
	}
	if err := validateToolName(name); err != nil {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s.name' %s", prefix, err.Error())}
	}
	var desc string
	if rawD, ok := item["description"]; ok && !isNull(rawD) {
		if err := json.Unmarshal(rawD, &desc); err != nil {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'%s.description' must be a string", prefix)}
		}
	}
	schema := json.RawMessage(`{"type":"object"}`)
	if rawP, ok := item["parameters"]; ok && !isNull(rawP) {
		if len(rawP) > lim.MaxArgsChars {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'%s.parameters' exceeds the size bound", prefix)}
		}
		var v any
		dec := json.NewDecoder(bytes.NewReader(rawP))
		dec.UseNumber()
		if err := dec.Decode(&v); err != nil {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'%s.parameters' must be valid JSON", prefix)}
		}
		if _, isObj := v.(map[string]any); !isObj {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'%s.parameters' must be a JSON object", prefix)}
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, rawP); err != nil {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'%s.parameters' must be valid JSON", prefix)}
		}
		schema = append(json.RawMessage(nil), compact.Bytes()...)
	}
	// Strict retention differs only for explicit true: Responses omits strict
	// attempts strict normalization, Chat omits strict stays nonstrict, and
	// Anthropic has no reviewed strict guarantees. Omitted/null is therefore
	// a diagnosed loss for all three destinations, never silently equated.
	// Explicit false is exact for all three. Explicit true is retained for
	// Chat, refused for Anthropic, and diagnosed as a loss for Gemini where
	// FunctionDeclarations carry no strict guarantee — rather than silently
	// weakened.
	// Sources: function-calling strict mode; migrate-to-responses §5.
	var strictPtr *bool
	if rawS, ok := item["strict"]; !ok || isNull(rawS) {
		lose(prefix+".strict", "implicit strict default differs across protocols; no reviewed normalization", false)
	} else {
		var b bool
		if err := json.Unmarshal(rawS, &b); err != nil {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'%s.strict' must be bool", prefix)}
		}
		if b && dest == responsesDestAnthropic {
			return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("'%s.strict' true requires a same-protocol route; Anthropic strict guarantees are not reviewed", prefix)}
		}
		if b && dest == responsesDestGemini {
			lose(prefix+".strict", "Gemini FunctionDeclarations carry no strict schema guarantee", false)
		}
		v := b
		strictPtr = &v
	}
	return &ToolDef{Name: name, Description: desc, Schema: schema, Strict: strictPtr}, nil
}

// decodeResponsesToolChoice validates the tool_choice control. The string
// union maps onto the IR exactly; a named function choice must reference a
// retained tool (checked by the caller). A namespaced function choice
// flattens through the request plan. An allowed_tools set has no
// reviewed bridge contract for any destination: it is validated, then
// reported as a dropped loss so strict policy refuses it and lossy mode
// reconciles to no forced choice. Hosted-tool choices have no mapping and
// are refused.
func decodeResponsesToolChoice(raw json.RawMessage, lose func(string, string, bool), dest responsesDestination, plan *ToolPlan, names map[string]bool) (*ToolChoice, bool, *RouterError) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "none":
			return &ToolChoice{Mode: "none"}, false, nil
		case "auto":
			return &ToolChoice{Mode: "auto"}, false, nil
		case "required":
			return &ToolChoice{Mode: "any"}, false, nil
		default:
			return nil, false, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: "'tool_choice' must be 'none', 'auto', 'required' or a tool reference"}
		}
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, false, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: "'tool_choice' must be 'none', 'auto', 'required' or a tool reference"}
	}
	for k := range obj {
		switch k {
		case "type", "name", "namespace", "mode", "tools", "server_label":
		default:
			return nil, false, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", "tool_choice."+k)}
		}
	}
	var typ string
	if rawT, ok := obj["type"]; ok && !isNull(rawT) {
		_ = json.Unmarshal(rawT, &typ)
	}
	switch typ {
	case "function":
		name, rerr := decodeResponsesFunctionRef(obj, "tool_choice", plan)
		if rerr != nil {
			return nil, false, rerr
		}
		for _, k := range []string{"mode", "tools", "server_label"} {
			if rawV, ok := obj[k]; ok && !isNull(rawV) {
				return nil, false, &RouterError{Status: 400, Code: CodeInvalidGeneration,
					Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", "tool_choice."+k)}
			}
		}
		return &ToolChoice{Mode: "specific", Name: name}, false, nil
	case "custom":
		name, rerr := decodeResponsesCustomRef(obj, "tool_choice", plan)
		if rerr != nil {
			return nil, false, rerr
		}
		for _, k := range []string{"mode", "tools", "server_label"} {
			if rawV, ok := obj[k]; ok && !isNull(rawV) {
				return nil, false, &RouterError{Status: 400, Code: CodeInvalidGeneration,
					Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", "tool_choice."+k)}
			}
		}
		return &ToolChoice{Mode: "specific", Name: name, Kind: ToolKindCustom}, false, nil
	case "allowed_tools":
		var mode string
		if rawM, ok := obj["mode"]; ok && !isNull(rawM) {
			_ = json.Unmarshal(rawM, &mode)
		}
		if mode != "auto" && mode != "required" {
			return nil, false, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: "'tool_choice.mode' must be 'auto' or 'required'"}
		}
		rawT, ok := obj["tools"]
		if !ok || isNull(rawT) {
			return nil, false, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: "'tool_choice.tools' is required for allowed tool sets"}
		}
		var refs []map[string]json.RawMessage
		if err := json.Unmarshal(rawT, &refs); err != nil {
			return nil, false, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: "'tool_choice.tools' must be an array of tool references"}
		}
		for ti, ref := range refs {
			for k := range ref {
				if k != "type" && k != "name" && k != "namespace" {
					return nil, false, &RouterError{Status: 400, Code: CodeInvalidGeneration,
						Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", fmt.Sprintf("tool_choice.tools[%d].%s", ti, k))}
				}
			}
			if rawNS, ok := ref["namespace"]; ok && !isNull(rawNS) {
				path := fmt.Sprintf("tool_choice.tools[%d]", ti)
				var memberType string
				_ = json.Unmarshal(ref["type"], &memberType)
				var name string
				var rerr *RouterError
				if memberType == "custom" {
					name, rerr = decodeResponsesCustomRef(ref, path, plan)
				} else if memberType == "function" {
					name, rerr = decodeResponsesFunctionRef(ref, path, plan)
				} else {
					return nil, false, &RouterError{Status: 400, Code: CodeInvalidGeneration,
						Msg: fmt.Sprintf("'%s.namespace' requires a function or custom reference", path)}
				}
				if rerr != nil {
					return nil, false, rerr
				}
				if !names[name] {
					return nil, false, &RouterError{Status: 400, Code: CodeInvalidRequest,
						Msg: fmt.Sprintf("'%s' names an unknown tool", path)}
				}
			}
		}
		lose("tool_choice", fmt.Sprintf("allowed tool sets have no %s bridge mapping", destBridgeName(dest)), false)
		return nil, true, nil
	case "file_search", "web_search_preview", "computer", "computer_use_preview",
		"mcp", "code_interpreter", "image_generation",
		"apply_patch", "shell", "programmatic_tool_calling":
		return nil, false, &RouterError{Status: 400, Code: CodeInvalidGeneration,
			Msg: fmt.Sprintf("Tool choice type %q requires a same-protocol route", typ)}
	default:
		return nil, false, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: "'tool_choice.type' must be a reviewed tool reference"}
	}
}

func decodeResponsesFunctionRef(obj map[string]json.RawMessage, path string, plan *ToolPlan) (string, *RouterError) {
	var name string
	_ = json.Unmarshal(obj["name"], &name)
	if err := validateToolName(name); err != nil {
		return "", &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s.name' %s", path, err.Error())}
	}
	if rawNS, ok := obj["namespace"]; ok && !isNull(rawNS) {
		ns, valid := decodeUnicodeString(rawNS)
		if !valid {
			return "", &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'%s.namespace' must be a Unicode string", path)}
		}
		if ns != "" {
			alias, found := plan.AliasFor(ns, name)
			if !found {
				return "", &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'%s' names an unknown tool", path)}
			}
			return alias, nil
		}
	}
	return name, nil
}
