// Anthropic ingress decoding: closed, typed validation of a Messages API
// request into the semantic IR. Unknown fields are validation errors, never
// silent accepts. Known-but-unbridged fields become Loss records for the
// strict/lossy policy — but only when they are well-formed: malformed
// types/values and unknown nested members are validation errors in both
// modes, before any upstream I/O.
//
// Null policy mirrors the Chat decoder: explicit null on an optional field
// means "absent" and is skipped, except for `stream`, whose contract is an
// explicit bool, and for nested boolean controls, where null is a type
// error. Omitted, null, zero and false are therefore distinguished
// explicitly at every boundary.
//
// Ordering rule: the IR (and the Chat destination) carries assistant text as
// one ordered string plus an ordered tool-call list, and tool results as an
// ordered list. An Anthropic message that interleaves text with tool_use, or
// text with tool_result, has no exact Chat representation: reordering it
// silently would change the conversation, so mixed-order messages are
// rejected in both modes rather than guessed. Pure-text, pure-tool_use and
// pure-tool_result messages keep their order exactly.
//
// Loss records are deterministic (sorted by field path) and bounded; they
// carry field paths and reasons only, never request values or prompt text.
package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// anthropicDestination selects the genuinely different mappings for the
// shared Anthropic source validator: stop_sequences handling and the tool
// strict plan. All other source validation (message shapes, history
// identities, numeric literals, schema fidelity, thinking, tool_choice) is
// destination-neutral and shared.
type anthropicDestination int

const (
	anthropicDestChat anthropicDestination = iota
	anthropicDestResponses
	anthropicDestGemini
)

// DecodeAnthropicRequest decodes raw JSON Anthropic Messages request bytes
// into the IR plus the planned losses. It performs no I/O.
func DecodeAnthropicRequest(raw []byte, lim Limits) (*Generation, []Loss, *RouterError) {
	return decodeAnthropicSource(raw, lim, anthropicDestChat)
}

// decodeAnthropicSource is the single private Anthropic source validator.
// Destination policy differs only for stop_sequences (Chat preserves up to
// 4, Gemini carries at most 5 exactly and drops longer lists, Responses has
// no contract and drops) and tool strict (Chat and Gemini have no slot and
// diagnose an explicit true; Responses carries an explicit flag exactly
// with the omitted default mapping to false). The parallel-disable control
// additionally has no Gemini mapping and is diagnosed there. Public Chat
// and Responses errors and losses are unchanged.
func decodeAnthropicSource(raw []byte, lim Limits, dest anthropicDestination) (*Generation, []Loss, *RouterError) {
	var top map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&top); err != nil {
		return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: "'body' must be a JSON object"}
	}
	if top == nil {
		return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: "'body' must be a JSON object"}
	}
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

	// model: required.
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

	// Closed field gate: every admitted field is represented, diagnosed, or
	// rejected. Known-but-unbridged fields are validated for shape first; a
	// malformed value is a 400 even in lossy mode.
	for key := range top {
		switch key {
		case "model", "messages", "system", "max_tokens",
			"temperature", "top_p", "top_k", "stop_sequences",
			"stream", "tools", "tool_choice":
			// handled below
		case "thinking", "output_config":
			// Validate together below so named effort is available to adaptive thinking.
		default:
			if knownAnthropicField(key) {
				if isNull(top[key]) {
					continue // explicit null means absent.
				}
				if rerr := validateAnthropicLossField(key, top[key], loseDrop); rerr != nil {
					return nil, nil, rerr
				}
				continue
			}
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", key)}
		}
	}
	var outputConfig map[string]json.RawMessage
	if raw := top["output_config"]; len(raw) != 0 && !isNull(raw) {
		if rerr := validateAnthropicLossField("output_config", raw, func(string, string) {}); rerr != nil {
			return nil, nil, rerr
		}
		_ = json.Unmarshal(raw, &outputConfig)
	}
	var effort string
	_ = json.Unmarshal(outputConfig["effort"], &effort)
	if raw := top["thinking"]; len(raw) != 0 {
		if rerr := validateAnthropicThinking(raw, dest, effort, g, loseDrop); rerr != nil {
			return nil, nil, rerr
		}
	}
	if g.Reasoning.Origin() == AnthropicEffortOrigin.Path() {
		lose("output_config.effort", "the same named level controls reasoning only on this destination", true)
		if raw := outputConfig["format"]; len(raw) != 0 && !isNull(raw) {
			loseDrop("output_config.format", "no reviewed bridge mapping")
		}
	} else if outputConfig != nil {
		loseDrop("output_config", "no reviewed bridge mapping")
	}

	// temperature / top_p: finite numbers in the IR contract range.
	// Explicit null means absent; quoted strings are type errors (encoding/json
	// would otherwise coerce a JSON string into json.Number).
	num := func(key string, min, max float64) (*float64, *RouterError) {
		raw, ok := top[key]
		if !ok || isNull(raw) {
			return nil, nil
		}
		if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 && trimmed[0] == '"' {
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
	if g.Temperature, rerr = num("temperature", 0, 1); rerr != nil {
		return nil, nil, rerr
	}
	if g.TopP, rerr = num("top_p", 0, 1); rerr != nil {
		return nil, nil, rerr
	}
	if rawTopK, ok := top["top_k"]; ok && !isNull(rawTopK) {
		i, rerr := jsonInt(rawTopK, "'top_k' must be an integer")
		if rerr != nil || i < 0 {
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: "'top_k' must be a non-negative integer"}
		}
		// No reviewed bridge mapping exists for top_k, so a well-formed
		// value is a diagnosed unsupported loss: strict refuses via
		// EnforcePolicy, lossy drops and reports.
		_ = i
		loseDrop("top_k", "no reviewed bridge mapping")
	}

	// max_tokens: required by the Anthropic contract. Zero is the documented
	// cache-warmup mode (no generation), which has no bridge mapping.
	rawMax, ok := top["max_tokens"]
	if !ok || isNull(rawMax) {
		return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: "'max_tokens' is required"}
	}
	maxTok, rerr := jsonInt(rawMax, "'max_tokens' must be a positive integer")
	if rerr != nil || maxTok <= 0 {
		return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: "'max_tokens' must be a positive integer"}
	}
	mt := int(maxTok)
	g.MaxTokens = &mt

	// stop_sequences: destination policy differs. Chat carries at most 4
	// stop strings; longer lists are diagnosed and truncated. Gemini
	// generationConfig.stopSequences carries at most 5 entries; a longer
	// well-formed list is diagnosed and dropped entirely, never silently
	// truncated. The reviewed Responses scope has no stop-sequence
	// contract, so a well-formed non-empty list is a diagnosed loss and is
	// never forwarded.
	if rawStop, ok := top["stop_sequences"]; ok && !isNull(rawStop) {
		var arr []json.RawMessage
		if err := json.Unmarshal(rawStop, &arr); err != nil {
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: "'stop_sequences' must be an array of strings"}
		}
		for _, e := range arr {
			var s string
			if err := json.Unmarshal(e, &s); err != nil {
				return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: "'stop_sequences' must be an array of strings"}
			}
			if dest == anthropicDestChat || dest == anthropicDestGemini {
				g.Stop = append(g.Stop, s)
			}
		}
		if dest == anthropicDestChat {
			if len(g.Stop) > 4 {
				loseDrop("stop_sequences", "Chat carries at most 4 stop sequences")
				g.Stop = g.Stop[:4]
			}
		} else if dest == anthropicDestGemini {
			if len(g.Stop) > 5 {
				loseDrop("stop_sequences", "Gemini stopSequences supports at most 5 entries")
				g.Stop = nil
			}
		} else if len(arr) > 0 {
			loseDrop("stop_sequences", "no reviewed bridge mapping")
		}
	}

	// system: string shorthand or an array of text blocks.
	if rawSys, ok := top["system"]; ok && !isNull(rawSys) {
		var s string
		if err := json.Unmarshal(rawSys, &s); err == nil {
			if s != "" {
				g.Messages = append(g.Messages, Message{Role: RoleSystem, Texts: []TextContent{{Text: s}}})
			}
		} else {
			var blocks []map[string]json.RawMessage
			if err := json.Unmarshal(rawSys, &blocks); err != nil {
				return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: "'system' must be a string or an array of text blocks"}
			}
			sys := Message{Role: RoleSystem}
			for bi, b := range blocks {
				if b == nil {
					return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
						Msg: "'system' must contain text block objects"}
				}
				t, rerr := decodeAnthropicTextBlock(fmt.Sprintf("system[%d]", bi), b, lose)
				if rerr != nil {
					return nil, nil, rerr
				}
				sys.Texts = append(sys.Texts, *t)
			}
			if len(sys.Texts) > 0 {
				g.Messages = append(g.Messages, sys)
			}
		}
	}

	// messages.
	rawMsgs, ok := top["messages"]
	if !ok || isNull(rawMsgs) {
		return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: "'messages' must be an array"}
	}
	var msgArr []map[string]json.RawMessage
	if err := json.Unmarshal(rawMsgs, &msgArr); err != nil {
		return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: "'messages' must be an array"}
	}
	if len(msgArr) == 0 {
		return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: "'messages' must contain at least one message"}
	}
	hasConversation := false
	for i, item := range msgArr {
		if item == nil {
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: "'messages' must contain objects with string roles"}
		}
		ms, rerr := decodeAnthropicMessage(i, item, lose, lim, dest)
		if rerr != nil {
			return nil, nil, rerr
		}
		for _, message := range ms {
			hasConversation = hasConversation || message.Role != RoleSystem
		}
		g.Messages = append(g.Messages, ms...)
	}
	if !hasConversation {
		return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: "'messages' must contain at least one translatable message"}
	}

	// tools: ordinary JSON-schema function tools only. Provider-native and
	// versioned client tools have no bridge mapping and are rejected
	// specifically, in both modes. The strict plan is destination-specific
	// and lives in decodeAnthropicToolForDest.
	if rawTools, ok := top["tools"]; ok && !isNull(rawTools) {
		var arr []map[string]json.RawMessage
		if err := json.Unmarshal(rawTools, &arr); err != nil {
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: "'tools' must contain objects"}
		}
		names := map[string]bool{}
		for i, item := range arr {
			if item == nil {
				return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: "'tools' must contain objects"}
			}
			def, rerr := decodeAnthropicToolForDest(i, item, lose, lim, dest)
			if rerr != nil {
				return nil, nil, rerr
			}
			if names[def.Name] {
				return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'tools[%d].name' conflicts with another tool definition", i)}
			}
			names[def.Name] = true
			g.Tools = append(g.Tools, *def)
		}
		g.HasTools = true
	}

	// tool_choice.
	if rawTC, ok := top["tool_choice"]; ok && !isNull(rawTC) {
		choice, parallel, rerr := decodeAnthropicToolChoice(rawTC)
		if rerr != nil {
			return nil, nil, rerr
		}
		g.Choice = choice
		g.ParallelToolCalls = parallel
	}

	// A named or forced choice needs tools to act on. auto/none without
	// tools would reach downstream as a dangling control, so it is dropped
	// with a diagnostic instead of being sent.
	if g.Choice != nil && !g.HasTools {
		switch g.Choice.Mode {
		case "specific", "any":
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: "'tool_choice' requires 'tools'"}
		default:
			loseDrop("tool_choice", "tool_choice requires tools")
			g.Choice = nil
		}
	}
	if g.Choice != nil && g.Choice.Mode == "specific" {
		found := false
		for _, t := range g.Tools {
			if t.Name == g.Choice.Name {
				found = true
				break
			}
		}
		if !found {
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: "'tool_choice' names an unknown tool"}
		}
	}
	if g.ParallelToolCalls != nil && !g.HasTools {
		loseDrop("tool_choice", "parallel control requires tools")
		g.ParallelToolCalls = nil
	}
	if dest == anthropicDestGemini && g.ParallelToolCalls != nil {
		// Gemini generations allow parallel function calls by default with
		// no disable control: an explicit disable has no mapping, so it is
		// diagnosed and dropped rather than silently ignored.
		loseDrop("tool_choice.disable_parallel_tool_use", "no reviewed bridge mapping")
		g.ParallelToolCalls = nil
	}

	// Historical call/result identities must match: every tool result must
	// reference a known assistant call ID, and IDs must not collide.
	callIDs := map[string]bool{}
	for mi, m := range g.Messages {
		for ci, tc := range m.ToolCalls {
			if callIDs[tc.ID] {
				return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'messages[%d].tool_calls[%d]' uses a conflicting call identity", mi, ci)}
			}
			callIDs[tc.ID] = true
		}
	}
	seenResults := map[string]bool{}
	for mi, m := range g.Messages {
		for _, r := range m.Results {
			if !callIDs[r.CallID] {
				return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'messages[%d]' references an unmatched tool call identity", mi)}
			}
			if seenResults[r.CallID] {
				return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'messages[%d]' has a conflicting tool result identity", mi)}
			}
			seenResults[r.CallID] = true
		}
	}

	return g, boundLosses(losses), nil
}

// knownAnthropicField is the closed universe of Messages request fields
// recognized by the generation contract. Fields here without a milestone
// bridge become diagnosed losses when well-formed; anything else is a
// validation error.
func knownAnthropicField(key string) bool {
	switch key {
	case "model", "messages", "system", "max_tokens",
		"metadata", "stop_sequences", "stream", "temperature",
		"tool_choice", "tools", "top_k", "top_p",
		"service_tier", "thinking", "output_config", "speed",
		"inference_geo", "container", "diagnostics", "cache_control":
		return true
	}
	return false
}

// validateAnthropicThinking enforces the reasoning boundary: for the Gemini
// destination an explicit opt-out is carried as a diagnosed loss (strict
// refuses via EnforcePolicy, lossy drops and reports), never silently
// dropped and never inferred as model capability. For the Chat and
// Responses destinations the same well-formed disabled shape decodes to the
// closed Off intent (origin "thinking") for the shared planner. Enabled
// with a positive budget decodes to the closed Budget intent (origin
// "thinking") for the Chat and Gemini destinations, reusing the shared
// planner's Budget gates (llama.cpp Chat exact; Gemini exact only on the
// pinned canonical native thinkingBudget fact
// generationConfig.thinkingConfig.thinkingBudget per the verified primary
// GenerateContent REST contract
// https://ai.google.dev/api/generate-content and the thinking ranges in
// https://ai.google.dev/gemini-api/docs/generate-content/thinking,
// otherwise diagnosed drop, no budget-to-level table). Enabled for
// Responses and between_tools keep the hard refusal. Adaptive thinking with
// explicit effort can carry that named level to Chat or Responses.
// Provider-private thinking state must never be silently dropped.
//
// Reviewed contract (https://docs.anthropic.com/en/api/messages):
// thinking = {"type":"disabled"} | {"type":"enabled","budget_tokens":N[,display]}
// | {"type":"adaptive"[,display]} | {"type":"between_tools"}.
// budget_tokens/display are not allowed on disabled: a present non-null
// value there is malformed (400), including wrong types. For enabled,
// budget_tokens is required positive int and display, when present
// non-null, must be summarized/omitted (malformed otherwise, 400 before
// any loss); a well-formed admitted display is an independent diagnosed
// dropped loss (field "thinking.display") alongside the Budget intent.
func validateAnthropicThinking(raw json.RawMessage, dest anthropicDestination, effort string, gen *Generation, lose func(string, string)) *RouterError {
	if isNull(raw) {
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return &RouterError{Status: 400, Code: CodeInvalidGeneration,
			Msg: "Unsupported or invalid request field for translation: \"thinking\""}
	}
	for k := range obj {
		if k != "type" && k != "budget_tokens" && k != "display" {
			return &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: "Unsupported or invalid request field for translation: \"thinking\""}
		}
	}
	rawT, hasType := obj["type"]
	if !hasType || isNull(rawT) {
		return &RouterError{Status: 400, Code: CodeInvalidGeneration,
			Msg: "Unsupported or invalid request field for translation: \"thinking\""}
	}
	var typ string
	if err := json.Unmarshal(rawT, &typ); err != nil || typ == "" {
		return &RouterError{Status: 400, Code: CodeInvalidGeneration,
			Msg: "Unsupported or invalid request field for translation: \"thinking\""}
	}
	switch typ {
	case "disabled":
		// Disabled takes no other members: any present non-null
		// budget_tokens/display is malformed, including wrong types.
		// Null means absent and is ignored.
		for _, k := range []string{"budget_tokens", "display"} {
			if v, ok := obj[k]; ok && !isNull(v) {
				return &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: "'thinking." + k + "' must be absent when type is 'disabled'"}
			}
		}
		if dest == anthropicDestGemini {
			lose("thinking", "no reviewed bridge mapping")
			return nil
		}
		gen.Reasoning = NewAnthropicDisabled()
		return nil
	case "enabled":
		// Enabled requires a positive integer budget_tokens. Null,
		// missing, non-integer, float, bool, string and <1 are malformed
		// (400) before any loss, in both modes. Unknown nested keys were
		// already rejected above (summary included: no new private-output
		// control is admitted here).
		rawB, hasB := obj["budget_tokens"]
		if !hasB || isNull(rawB) {
			return &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: "Unsupported or invalid request field for translation: \"thinking\""}
		}
		budget, rerr := jsonInt(rawB, "Unsupported or invalid request field for translation: \"thinking\"")
		if rerr != nil || budget < 1 {
			return &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: "Unsupported or invalid request field for translation: \"thinking\""}
		}
		// Display, when present non-null, must be a reviewed literal;
		// otherwise malformed (400) before any loss. Null means absent.
		hasDisplay := false
		if rawD, ok := obj["display"]; ok && !isNull(rawD) {
			var d string
			if err := json.Unmarshal(rawD, &d); err != nil || (d != "summarized" && d != "omitted") {
				return &RouterError{Status: 400, Code: CodeInvalidGeneration,
					Msg: "Unsupported or invalid request field for translation: \"thinking\""}
			}
			hasDisplay = true
		}
		if dest == anthropicDestResponses {
			// Retain the existing hard same-protocol refusal. Responses
			// has no budget control (no guessed budget-to-level table).
			return &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: "Anthropic thinking requires a same-protocol route"}
		}
		// Chat and Gemini destinations: independent display loss does not
		// block the exact budget intent (mirrors Python's separate display
		// gate). The Gemini wire is gated later on the pinned canonical
		// native thinkingBudget fact, never on model IDs or URLs.
		if hasDisplay {
			lose("thinking.display", "thinking display has no equivalent control; the destination's default applies")
		}
		intent, ok := NewAnthropicThinkingEnabled(budget)
		if !ok {
			return &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: "Unsupported or invalid request field for translation: \"thinking\""}
		}
		gen.Reasoning = intent
		return nil
	case "adaptive":
		if effort == "" || dest == anthropicDestGemini {
			return &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: "Anthropic thinking requires a same-protocol route"}
		}
		if v := obj["budget_tokens"]; len(v) != 0 && !isNull(v) {
			return &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: "'thinking.budget_tokens' must be absent when type is 'adaptive'"}
		}
		if v := obj["display"]; len(v) != 0 && !isNull(v) {
			var display string
			if json.Unmarshal(v, &display) != nil || (display != "summarized" && display != "omitted") {
				return &RouterError{Status: 400, Code: CodeInvalidGeneration, Msg: "Unsupported or invalid request field for translation: \"thinking.display\""}
			}
			lose("thinking.display", "thinking display has no equivalent control")
		}
		lose("thinking", "adaptive thinking mode has no equivalent control")
		gen.Reasoning = ReasoningIntent{kind: KindNamed, level: effort, origin: AnthropicEffortOrigin}
		return nil
	case "between_tools":
		return &RouterError{Status: 400, Code: CodeInvalidGeneration,
			Msg: "Anthropic thinking requires a same-protocol route"}
	default:
		return &RouterError{Status: 400, Code: CodeInvalidGeneration,
			Msg: "Unsupported or invalid request field for translation: \"thinking\""}
	}
}

// validateAnthropicCacheHint strictly validates one Anthropic cache_control
// optimization hint where the reviewed bridge has no cache service: a
// well-formed value (object with type:"ephemeral" and optional ttl:"5m"|"1h")
// returns nil so the caller records a diagnosed dropped loss; a malformed
// shape or unknown member returns a 400 in both modes before any loss.
// what is the full field path including the trailing "cache_control"
// (for example "cache_control", "tools[0].cache_control" or
// "messages[1].content[0].cache_control").
func validateAnthropicCacheHint(what string, raw json.RawMessage) *RouterError {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s' must be an object", what)}
	}
	for k, v := range obj {
		switch k {
		case "type":
			var t string
			if err := json.Unmarshal(v, &t); err != nil || t != "ephemeral" {
				return &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'%s.type' must be 'ephemeral'", what)}
			}
		case "ttl":
			var ttl string
			if err := json.Unmarshal(v, &ttl); err != nil || (ttl != "5m" && ttl != "1h") {
				return &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'%s.ttl' must be '5m' or '1h'", what)}
			}
		default:
			return &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", what+"."+k)}
		}
	}
	return nil
}

// validateAnthropicLossField strictly validates a known-but-unbridged field:
// a well-formed value becomes a loss, a malformed one is a 400 in both
// modes.
func validateAnthropicLossField(key string, raw json.RawMessage, lose func(string, string)) *RouterError {
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
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
			return nil, bad(fmt.Sprintf("'%s' must be an object", key))
		}
		return obj, nil
	}
	switch key {
	case "service_tier":
		var s string
		if err := json.Unmarshal(raw, &s); err != nil || (s != "auto" && s != "standard_only") {
			return bad("'service_tier' must be 'auto' or 'standard_only'")
		}
		return loss("no reviewed bridge mapping")
	case "speed":
		var s string
		if err := json.Unmarshal(raw, &s); err != nil || (s != "standard" && s != "fast") {
			return bad("'speed' must be 'standard' or 'fast'")
		}
		return loss("no reviewed bridge mapping")
	case "inference_geo":
		return mustStr(true)
	case "metadata":
		obj, rerr := mustObj()
		if rerr != nil {
			return rerr
		}
		for k, v := range obj {
			if k != "user_id" {
				return bad(fmt.Sprintf("Unsupported or invalid request field for translation: %q", "metadata."+k))
			}
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				return bad("'metadata.user_id' must be a string")
			}
		}
		return loss("no reviewed bridge mapping")
	case "container":
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			if s == "" {
				return bad("'container' must be a non-empty string or object")
			}
			return loss("no reviewed bridge mapping")
		}
		obj, rerr := mustObj()
		if rerr != nil {
			return bad("'container' must be a string or object")
		}
		for k, v := range obj {
			switch k {
			case "id":
				if isNull(v) {
					continue
				}
				var id string
				if err := json.Unmarshal(v, &id); err != nil || id == "" {
					return bad("'container.id' must be a string")
				}
			case "skills":
				var skills []map[string]json.RawMessage
				if err := json.Unmarshal(v, &skills); err != nil {
					return bad("'container.skills' must be an array of objects")
				}
			default:
				return bad(fmt.Sprintf("Unsupported or invalid request field for translation: %q", "container."+k))
			}
		}
		return loss("no reviewed bridge mapping")
	case "diagnostics":
		obj, rerr := mustObj()
		if rerr != nil {
			return rerr
		}
		for k, v := range obj {
			if k != "previous_message_id" {
				return bad(fmt.Sprintf("Unsupported or invalid request field for translation: %q", "diagnostics."+k))
			}
			if isNull(v) {
				continue
			}
			var s string
			if err := json.Unmarshal(v, &s); err != nil || s == "" || len(s) > 256 {
				return bad("'diagnostics.previous_message_id' must be a short string")
			}
		}
		return loss("no reviewed bridge mapping")
	case "cache_control":
		if rerr := validateAnthropicCacheHint("cache_control", raw); rerr != nil {
			return rerr
		}
		return loss("no reviewed bridge mapping")
	case "output_config":
		obj, rerr := mustObj()
		if rerr != nil {
			return rerr
		}
		for k, v := range obj {
			switch k {
			case "effort":
				if isNull(v) {
					continue
				}
				var e string
				if err := json.Unmarshal(v, &e); err != nil ||
					(e != "low" && e != "medium" && e != "high" && e != "xhigh" && e != "max") {
					return bad("'output_config.effort' must be 'low', 'medium', 'high', 'xhigh' or 'max'")
				}
			case "format":
				if isNull(v) {
					continue
				}
				var fmap map[string]json.RawMessage
				if err := json.Unmarshal(v, &fmap); err != nil || fmap == nil {
					return bad("'output_config.format' must be an object")
				}
				for fk, fv := range fmap {
					switch fk {
					case "type":
						var t string
						if err := json.Unmarshal(fv, &t); err != nil || t != "json_schema" {
							return bad("'output_config.format.type' must be 'json_schema'")
						}
					case "schema":
						var s map[string]json.RawMessage
						if err := json.Unmarshal(fv, &s); err != nil || s == nil {
							return bad("'output_config.format.schema' must be an object")
						}
					default:
						return bad(fmt.Sprintf("Unsupported or invalid request field for translation: %q", "output_config.format."+fk))
					}
				}
			default:
				return bad(fmt.Sprintf("Unsupported or invalid request field for translation: %q", "output_config."+k))
			}
		}
		return loss("no reviewed bridge mapping")
	default:
		lose(key, "no reviewed bridge mapping")
		return nil
	}
}

// decodeAnthropicMessage decodes one Messages API message. A user message
// carrying both text and tool_result blocks, or an assistant message
// interleaving text with tool_use, is rejected: the IR cannot preserve the
// cross-kind order, and reordering silently would change the conversation.
// Pure messages expand to one or more IR messages in block order.
func decodeAnthropicMessage(i int, item map[string]json.RawMessage, lose func(string, string, bool), lim Limits, dest anthropicDestination) ([]Message, *RouterError) {
	prefix := fmt.Sprintf("messages[%d]", i)
	for key := range item {
		switch key {
		case "role", "content":
		default:
			return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", prefix+"."+key)}
		}
	}
	var role string
	if rawR, ok := item["role"]; ok && !isNull(rawR) {
		_ = json.Unmarshal(rawR, &role)
	}
	// Claude Code can put a plain system message after user content.
	// The OpenAI targets can carry it in that position.
	if role == "system" && dest != anthropicDestGemini {
		var text string
		if json.Unmarshal(item["content"], &text) != nil || text == "" {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'%s.content' must be a non-empty string for system messages", prefix)}
		}
		return []Message{{Role: RoleSystem, Texts: []TextContent{{Text: text}}}}, nil
	}
	if role != "user" && role != "assistant" {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s.role' must be \"user\" or \"assistant\"", prefix)}
	}
	rawC, ok := item["content"]
	if !ok || isNull(rawC) {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s.content' is required", prefix)}
	}
	var s string
	if err := json.Unmarshal(rawC, &s); err == nil {
		if s == "" {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'%s.content' must be a non-empty string or array", prefix)}
		}
		r := RoleUser
		if role == "assistant" {
			r = RoleAssistant
		}
		return []Message{{Role: r, Texts: []TextContent{{Text: s}}}}, nil
	}
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(rawC, &blocks); err != nil {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s.content' must be a string or array", prefix)}
	}
	if len(blocks) == 0 {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s.content' must contain at least one block", prefix)}
	}
	if role == "assistant" {
		return decodeAnthropicAssistantBlocks(prefix, blocks, lose, lim)
	}
	return decodeAnthropicUserBlocks(prefix, blocks, lose, lim)
}

// decodeAnthropicAssistantBlocks handles text/tool_use content. All text
// blocks must precede all tool_use blocks; anything interleaved is rejected
// rather than reordered.
func decodeAnthropicAssistantBlocks(prefix string, blocks []map[string]json.RawMessage, lose func(string, string, bool), lim Limits) ([]Message, *RouterError) {
	m := Message{Role: RoleAssistant}
	seenTool := false
	for bi, b := range blocks {
		if b == nil {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'%s.content' must contain content block objects", prefix)}
		}
		typ := anthropicBlockType(prefix, bi, b)
		if typ == "" {
			return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", fmt.Sprintf("%s.content[%d].type", prefix, bi))}
		}
		switch typ {
		case "text":
			if seenTool {
				return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
					Msg: fmt.Sprintf("'%s.content[%d]' interleaves text with tool calls; mixed order requires a same-protocol route", prefix, bi)}
			}
			t, rerr := decodeAnthropicTextBlock(fmt.Sprintf("%s.content[%d]", prefix, bi), b, lose)
			if rerr != nil {
				return nil, rerr
			}
			m.Texts = append(m.Texts, *t)
		case "tool_use":
			seenTool = true
			tc, rerr := decodeAnthropicToolUse(prefix, bi, b, lim, lose)
			if rerr != nil {
				return nil, rerr
			}
			m.ToolCalls = append(m.ToolCalls, *tc)
		default:
			return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("'%s.content[%d]' block type %q requires a same-protocol route", prefix, bi, typ)}
		}
	}
	return []Message{m}, nil
}

// decodeAnthropicUserBlocks handles text/tool_result content. A message must
// be pure text or pure tool results; mixed messages are rejected rather
// than split in a guessed order.
func decodeAnthropicUserBlocks(prefix string, blocks []map[string]json.RawMessage, lose func(string, string, bool), lim Limits) ([]Message, *RouterError) {
	var texts []TextContent
	var results []ToolResult
	seenText, seenResult := false, false
	for bi, b := range blocks {
		if b == nil {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'%s.content' must contain content block objects", prefix)}
		}
		typ := anthropicBlockType(prefix, bi, b)
		if typ == "" {
			return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", fmt.Sprintf("%s.content[%d].type", prefix, bi))}
		}
		switch typ {
		case "text":
			if seenResult {
				return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
					Msg: fmt.Sprintf("'%s.content[%d]' mixes text with tool results; mixed order requires a same-protocol route", prefix, bi)}
			}
			seenText = true
			t, rerr := decodeAnthropicTextBlock(fmt.Sprintf("%s.content[%d]", prefix, bi), b, lose)
			if rerr != nil {
				return nil, rerr
			}
			texts = append(texts, *t)
		case "tool_result":
			if seenText {
				return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
					Msg: fmt.Sprintf("'%s.content[%d]' mixes text with tool results; mixed order requires a same-protocol route", prefix, bi)}
			}
			seenResult = true
			r, rerr := decodeAnthropicToolResult(prefix, bi, b, lose, lim)
			if rerr != nil {
				return nil, rerr
			}
			results = append(results, *r)
		default:
			return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("'%s.content[%d]' block type %q requires a same-protocol route", prefix, bi, typ)}
		}
	}
	if len(texts) > 0 {
		return []Message{{Role: RoleUser, Texts: texts}}, nil
	}
	return []Message{{Role: RoleTool, Results: results}}, nil
}

// anthropicBlockType returns the block type, or "" when it is missing or
// not a string (a validation error at the call site).
func anthropicBlockType(prefix string, bi int, b map[string]json.RawMessage) string {
	_ = prefix
	_ = bi
	raw, ok := b["type"]
	if !ok || isNull(raw) {
		return ""
	}
	var typ string
	if err := json.Unmarshal(raw, &typ); err != nil {
		return ""
	}
	return typ
}

// decodeAnthropicTextBlock validates one text block. A validated
// cache_control optimization hint becomes a diagnosed dropped loss (strict
// refuses via EnforcePolicy, lossy drops); a malformed hint is a 400 before
// any loss. Citations naming unmapped retrieval features are refused rather
// than silently dropped.
func decodeAnthropicTextBlock(what string, b map[string]json.RawMessage, lose func(string, string, bool)) (*TextContent, *RouterError) {
	for k := range b {
		switch k {
		case "type", "text", "cache_control", "citations":
		default:
			return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", what+"."+k)}
		}
	}
	rawText, ok := b["text"]
	if !ok || isNull(rawText) {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s.text' is required", what)}
	}
	var text string
	if err := json.Unmarshal(rawText, &text); err != nil || text == "" {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s.text' must be a non-empty string", what)}
	}
	if rawCC, ok := b["cache_control"]; ok && !isNull(rawCC) {
		if rerr := validateAnthropicCacheHint(what+".cache_control", rawCC); rerr != nil {
			return nil, rerr
		}
		lose(what+".cache_control", "no reviewed bridge mapping", false)
	}
	if rawCit, ok := b["citations"]; ok && !isNull(rawCit) {
		var cits []json.RawMessage
		if err := json.Unmarshal(rawCit, &cits); err != nil {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'%s.citations' must be an array or null", what)}
		}
		if len(cits) > 0 {
			return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("'%s.citations' requires a same-protocol route", what)}
		}
	}
	return &TextContent{Text: text}, nil
}

// decodeAnthropicToolUse validates one assistant tool_use block. Arguments
// must decode to a JSON object and are preserved as compact raw JSON without
// a float64 round-trip, so large integers and number literals survive
// exactly. Empty input means "no arguments". History identities (id) are
// opaque client/provider handles, not tool names: tool names keep the
// cross-dialect 1-64 grammar, but a history tool_use id only needs to be
// a bounded nonempty string so cross-provider handles (for example
// OpenAI-style call ids echoed through an Anthropic-shaped history)
// round-trip exactly. A validated cache_control optimization hint becomes a
// diagnosed dropped loss (strict refuses via EnforcePolicy, lossy drops);
// a malformed hint is a 400 before any loss.
// Reviewed contracts: Anthropic Messages tool_use block
// (https://docs.anthropic.com/en/api/messages); Gemini FunctionCall id is
// an optional free string echoed by the client
// (https://ai.google.dev/api/generate-content).
func decodeAnthropicToolUse(prefix string, bi int, b map[string]json.RawMessage, lim Limits, lose func(string, string, bool)) (*ToolCall, *RouterError) {
	what := fmt.Sprintf("%s.content[%d]", prefix, bi)
	for k := range b {
		switch k {
		case "type", "id", "name", "input", "cache_control":
		case "caller", "toolset_name":
			return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("'%s' server-tool invocation requires a same-protocol route", what)}
		default:
			return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", what+"."+k)}
		}
	}
	var id, name string
	if rawID, ok := b["id"]; ok && !isNull(rawID) {
		_ = json.Unmarshal(rawID, &id)
	}
	if err := validateCallID(id, lim); err != nil {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s.id' %s", what, err.Error())}
	}
	// Router-minted wrapped identities (Gemini thought-signature
	// passback) restore the provider identity and the opaque signature;
	// any other value passes through untouched.
	sig := ""
	if s, inner, ok := unwrapGeminiSignatureID(id); ok {
		if err := validateCallID(inner, lim); err != nil {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'%s.id' %s", what, err.Error())}
		}
		id, sig = inner, s
	}
	if rawName, ok := b["name"]; ok && !isNull(rawName) {
		_ = json.Unmarshal(rawName, &name)
	}
	if err := validateToolName(name); err != nil {
		// Anthropic names allow up to 128 characters, but the Chat bridge
		// contract requires 1-64: a longer name cannot be carried over.
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s.name' %s", what, err.Error())}
	}
	if rawCC, ok := b["cache_control"]; ok && !isNull(rawCC) {
		if rerr := validateAnthropicCacheHint(what+".cache_control", rawCC); rerr != nil {
			return nil, rerr
		}
		lose(what+".cache_control", "no reviewed bridge mapping", false)
	}
	args := json.RawMessage("{}")
	if rawInput, ok := b["input"]; ok && !isNull(rawInput) {
		compacted, rerr := compactAnthropicInput(what, rawInput, lim)
		if rerr != nil {
			return nil, rerr
		}
		args = compacted
	}
	return &ToolCall{ID: id, Name: name, Arguments: args, Signature: sig}, nil
}

// compactAnthropicInput validates that raw is a JSON object and returns its
// compact form without a float64 round-trip, preserving number literals
// exactly.
func compactAnthropicInput(what string, raw json.RawMessage, lim Limits) (json.RawMessage, *RouterError) {
	if len(raw) > lim.MaxArgsChars {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s.input' exceeds the size bound", what)}
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s.input' must be a JSON object", what)}
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s.input' must be valid JSON", what)}
	}
	return json.RawMessage(compact.Bytes()), nil
}

// decodeAnthropicToolResult validates one user tool_result block. Content
// is plain text: a string is exact, an array of text blocks is concatenated
// in order as an approximation, and anything else requires a
// same-protocol route. is_error has no Chat representation and becomes an
// explicit loss. A validated cache_control optimization hint becomes a
// diagnosed dropped loss (strict refuses via EnforcePolicy, lossy drops);
// a malformed hint is a 400 before any loss. The tool_use_id echoes the
// paired tool_use id verbatim, so it follows the same opaque-handle rule as
// tool_use id (bounded nonempty string), not the tool-name grammar.
func decodeAnthropicToolResult(prefix string, bi int, b map[string]json.RawMessage, lose func(string, string, bool), lim Limits) (*ToolResult, *RouterError) {
	what := fmt.Sprintf("%s.content[%d]", prefix, bi)
	for k := range b {
		switch k {
		case "type", "tool_use_id", "content", "is_error", "cache_control":
		case "toolset_name":
			return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("'%s' server-tool result requires a same-protocol route", what)}
		default:
			return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", what+"."+k)}
		}
	}
	var callID string
	if rawID, ok := b["tool_use_id"]; ok && !isNull(rawID) {
		_ = json.Unmarshal(rawID, &callID)
	}
	if err := validateCallID(callID, lim); err != nil {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s.tool_use_id' %s", what, err.Error())}
	}
	// Wrapped identities restore the provider identity so the result
	// pairs with the unwrapped assistant call; any other value passes
	// through untouched.
	if _, inner, isWrapped := unwrapGeminiSignatureID(callID); isWrapped {
		callID = inner
	}
	content := ""
	if rawC, ok := b["content"]; ok && !isNull(rawC) {
		var s string
		if err := json.Unmarshal(rawC, &s); err == nil {
			content = s
		} else {
			var parts []map[string]json.RawMessage
			if err := json.Unmarshal(rawC, &parts); err != nil {
				return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'%s.content' must be a string or array of text blocks", what)}
			}
			var sb strings.Builder
			for pi, p := range parts {
				if p == nil {
					return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
						Msg: fmt.Sprintf("'%s.content' must contain content block objects", what)}
				}
				var ptype string
				if rawT, ok := p["type"]; ok && !isNull(rawT) {
					_ = json.Unmarshal(rawT, &ptype)
				}
				if ptype != "text" {
					return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
						Msg: fmt.Sprintf("'%s.content[%d]' block type %q requires a same-protocol route", what, pi, ptype)}
				}
				var text string
				if rawT, ok := p["text"]; !ok || isNull(rawT) || json.Unmarshal(rawT, &text) != nil {
					return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
						Msg: fmt.Sprintf("'%s.content[%d].text' must be a string", what, pi)}
				}
				sb.WriteString(text)
			}
			content = sb.String()
			lose(what+".content", "tool result block structure is concatenated to text", true)
		}
	}
	if len(content) > lim.MaxArgsChars {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s.content' exceeds the size bound", what)}
	}
	isErr := false
	if rawE, ok := b["is_error"]; ok && !isNull(rawE) {
		if err := json.Unmarshal(rawE, &isErr); err != nil {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'%s.is_error' must be bool", what)}
		}
		if isErr {
			lose(what+".is_error", "Chat tool results carry no error marker", false)
		}
	}
	if rawCC, ok := b["cache_control"]; ok && !isNull(rawCC) {
		if rerr := validateAnthropicCacheHint(what+".cache_control", rawCC); rerr != nil {
			return nil, rerr
		}
		lose(what+".cache_control", "no reviewed bridge mapping", false)
	}
	return &ToolResult{CallID: callID, Content: content, IsError: false}, nil
}

// compactAnthropicToolSchema validates a tool JSON-schema member and returns
// its compact form without a float64 round-trip, preserving number literals
// exactly. It is shared by the Chat- and Responses-destination tool decoders;
// diagnostics name the source field path either way.
func compactAnthropicToolSchema(prefix, member string, raw json.RawMessage, lim Limits) (json.RawMessage, *RouterError) {
	if len(raw) > lim.MaxArgsChars {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s.%s' exceeds the size bound", prefix, member)}
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s.%s' must be valid JSON", prefix, member)}
	}
	if _, isObj := v.(map[string]any); !isObj {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s.%s' must be a JSON object", prefix, member)}
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s.%s' must be valid JSON", prefix, member)}
	}
	return append(json.RawMessage(nil), compact.Bytes()...), nil
}

// decodeAnthropicTool validates one ordinary function tool definition for
// the Chat destination. The Anthropic "custom" type marker denotes a plain
// JSON-schema function (unlike OpenAI freeform custom tools); any other
// tool type is a provider-native or versioned client tool with no bridge
// mapping.
func decodeAnthropicTool(i int, item map[string]json.RawMessage, lose func(string, string, bool), lim Limits) (*ToolDef, *RouterError) {
	return decodeAnthropicToolForDest(i, item, lose, lim, anthropicDestChat)
}

// decodeAnthropicToolForDest is the single private tool validator. The
// strict plan is destination-specific: Chat and Gemini have no strict slot,
// so an explicit true is a diagnosed loss and omitted/false carry no loss
// with a nil Strict (Gemini FunctionDeclaration carries no strict
// guarantee, so no guarantee requested means no loss); Responses carries an
// explicit flag exactly (omitted/null maps to an explicit false with no
// loss, explicit true/false retained). All other source validation is
// shared, reusing the existing message/schema/tool helpers.
func decodeAnthropicToolForDest(i int, item map[string]json.RawMessage, lose func(string, string, bool), lim Limits, dest anthropicDestination) (*ToolDef, *RouterError) {
	prefix := fmt.Sprintf("tools[%d]", i)
	if rawT, ok := item["type"]; ok && !isNull(rawT) {
		var typ string
		if err := json.Unmarshal(rawT, &typ); err != nil {
			return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", prefix+".type")}
		}
		if typ != "custom" {
			bridge := "Chat"
			if dest == anthropicDestResponses {
				bridge = "Responses"
			} else if dest == anthropicDestGemini {
				bridge = "Gemini"
			}
			return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Tool type %q has no %s bridge mapping; ordinary function tools only", typ, bridge)}
		}
	}
	for k := range item {
		switch k {
		case "type", "name", "description", "input_schema", "strict",
			"cache_control", "allowed_callers", "defer_loading",
			"eager_input_streaming", "input_examples":
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
	if rawS, ok := item["input_schema"]; ok && !isNull(rawS) {
		compacted, rerr := compactAnthropicToolSchema(prefix, "input_schema", rawS, lim)
		if rerr != nil {
			return nil, rerr
		}
		schema = compacted
	}
	var strictPtr *bool
	if dest == anthropicDestChat || dest == anthropicDestGemini {
		// Chat and Gemini carry no strict schema guarantee: omitted or
		// explicit false is exact (no guarantee requested, none carried);
		// explicit true claims an unreviewed guarantee and is diagnosed.
		if rawS, ok := item["strict"]; ok && !isNull(rawS) {
			var b bool
			if err := json.Unmarshal(rawS, &b); err != nil {
				return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'%s.strict' must be bool", prefix)}
			}
			if b {
				lose(prefix+".strict", "strict schema adherence is not carried over the bridge", false)
			}
		}
	} else {
		// Responses strict plan: the Anthropic non-strict default maps
		// exactly to an explicit false with no loss; explicit values are
		// retained exactly.
		if rawS, ok := item["strict"]; !ok || isNull(rawS) {
			v := false
			strictPtr = &v
		} else {
			var b bool
			if err := json.Unmarshal(rawS, &b); err != nil {
				return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'%s.strict' must be bool", prefix)}
			}
			v := b
			strictPtr = &v
		}
	}
	if rawCC, ok := item["cache_control"]; ok && !isNull(rawCC) {
		if rerr := validateAnthropicCacheHint(prefix+".cache_control", rawCC); rerr != nil {
			return nil, rerr
		}
		lose(prefix+".cache_control", "no reviewed bridge mapping", false)
	}
	if rawAC, ok := item["allowed_callers"]; ok && !isNull(rawAC) {
		var callers []json.RawMessage
		if err := json.Unmarshal(rawAC, &callers); err != nil {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'%s.allowed_callers' must be an array of strings", prefix)}
		}
		for _, c := range callers {
			var s string
			if err := json.Unmarshal(c, &s); err != nil || s == "" {
				return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'%s.allowed_callers' must be an array of strings", prefix)}
			}
		}
		lose(prefix+".allowed_callers", "no reviewed bridge mapping", false)
	}
	for _, k := range []string{"defer_loading", "eager_input_streaming"} {
		if rawB, ok := item[k]; ok && !isNull(rawB) {
			var b bool
			if err := json.Unmarshal(rawB, &b); err != nil {
				return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'%s.%s' must be bool", prefix, k)}
			}
			if b {
				lose(prefix+"."+k, "no reviewed bridge mapping", false)
			}
		}
	}
	if rawE, ok := item["input_examples"]; ok && !isNull(rawE) {
		var arr []json.RawMessage
		if err := json.Unmarshal(rawE, &arr); err != nil {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'%s.input_examples' must be an array", prefix)}
		}
		lose(prefix+".input_examples", "no reviewed bridge mapping", false)
	}
	if dest == anthropicDestChat {
		return &ToolDef{Name: name, Description: desc, Schema: schema}, nil
	}
	return &ToolDef{Name: name, Description: desc, Schema: schema, Strict: strictPtr}, nil
}

// decodeAnthropicToolChoice validates the tool_choice control. auto/any map
// to the Chat auto/required contract, a named tool maps to the Chat
// function reference, and none disables tools. disable_parallel_tool_use
// maps to Chat parallel_tool_calls (false only; absent or false is the Chat
// default and needs no control). An explicit name on a non-tool choice has
// no reviewed bridge contract and is rejected.
func decodeAnthropicToolChoice(raw json.RawMessage) (*ToolChoice, *bool, *RouterError) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: "'tool_choice' must be an object"}
	}
	for k := range obj {
		switch k {
		case "type", "name", "disable_parallel_tool_use":
		default:
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", "tool_choice."+k)}
		}
	}
	var typ string
	if rawT, ok := obj["type"]; ok && !isNull(rawT) {
		_ = json.Unmarshal(rawT, &typ)
	}
	switch typ {
	case "auto", "any", "tool", "none":
	default:
		return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: "'tool_choice.type' must be 'auto', 'any', 'tool' or 'none'"}
	}
	// disable_parallel_tool_use is strictly typed: a malformed value is a
	// 400 even in lossy mode.
	var parallel *bool
	if rawD, ok := obj["disable_parallel_tool_use"]; ok && !isNull(rawD) {
		var b bool
		if err := json.Unmarshal(rawD, &b); err != nil {
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: "'tool_choice.disable_parallel_tool_use' must be bool"}
		}
		if b {
			off := false
			parallel = &off
		}
	}
	var name string
	if rawN, ok := obj["name"]; ok && !isNull(rawN) {
		_ = json.Unmarshal(rawN, &name)
	}
	if typ == "tool" {
		if err := validateToolName(name); err != nil {
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'tool_choice.name' %s", err.Error())}
		}
	} else if _, present := obj["name"]; present && !isNull(obj["name"]) {
		return nil, nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
			Msg: "Unsupported or invalid request field for translation: \"tool_choice.name\""}
	}
	choice := &ToolChoice{}
	switch typ {
	case "none":
		choice.Mode = "none"
	case "auto":
		choice.Mode = "auto"
	case "any":
		choice.Mode = "any"
	case "tool":
		choice.Mode = "specific"
		choice.Name = name
	}
	return choice, parallel, nil
}
