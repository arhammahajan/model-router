// Chat ingress decoding: closed, typed validation of a Chat Completions
// request into the semantic IR. Unknown fields are validation errors, never
// silent accepts. Known fields without a milestone-2 bridge mapping become
// Loss records for the strict/lossy policy — but only when they are
// well-formed: malformed types/values and unknown nested members are
// validation errors in both modes, before any upstream I/O.
//
// Null policy (mirroring the Python baseline): explicit null on an optional
// field means "absent" and is skipped, except for `stream`, whose contract
// is an explicit boolean, and for nested boolean controls, where null is a
// type error. Omitted, null, zero and false are therefore distinguished
// explicitly at every boundary.
//
// Loss records are deterministic (sorted by field path) and bounded; they
// carry field paths and reasons only, never request values or prompt text.
package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// maxLossRecords bounds the diagnosed loss list of one request. The known
// loss-field universe is small; per-index entries (tools[i], messages[i]…)
// are what can grow, so the bound keeps the header and error text finite.
const maxLossRecords = 64

// DecodeChatRequest decodes raw JSON Chat request bytes into the IR plus the
// planned losses. It performs no I/O.
func DecodeChatRequest(raw []byte, lim Limits) (*Generation, []Loss, *RouterError) {
	return decodeChatSource(raw, lim, chatSourcePolicy{
		dest:         chatDestAnthropic,
		decodeTool:   decodeChatToolAnthropic,
		validateLoss: validateLossField,
	})
}

// decodeChatToolAnthropic adapts decodeChatTool to the shared source
// policy hook. The Anthropic destination carries no strict slot in the
// reviewed scope: an explicit bool is validated and otherwise ignored.
// Freeform custom tools are emulated as string-valued functions (see
// responsesCustomEmuReason); the definition loss is recorded by the
// caller as an approximation so strict policy refuses pre-I/O.
func decodeChatToolAnthropic(i int, item map[string]json.RawMessage, lim Limits, _ func(string, string)) (*ToolDef, *RouterError) {
	var typ string
	if raw, ok := item["type"]; ok && !isNull(raw) {
		_ = json.Unmarshal(raw, &typ)
	}
	if typ == "custom" {
		name, desc, format, rerr := parseChatCustomSpec(i, item, lim)
		if rerr != nil {
			return nil, rerr
		}
		return &ToolDef{Kind: ToolKindCustom, Name: name, Description: desc, Format: format}, nil
	}
	return decodeChatTool(i, item, lim)
}

// chatDest selects the destination contract for the shared Chat source
// validator. The three Chat ingress entrypoints share one source contract
// and differ only where the destination wire contract differs.
type chatDest int

const (
	// chatDestAnthropic targets the Anthropic bridge (DecodeChatRequest).
	chatDestAnthropic chatDest = iota
	// chatDestResponses targets the Responses bridge.
	chatDestResponses
	// chatDestGemini targets the Gemini bridge.
	chatDestGemini
)

// chatSourcePolicy is the typed destination policy for decodeChatSource:
// one private Chat source validator plus per-destination hooks, so the
// ~400-line top-level decode is written once instead of three times.
// Wrappers preserve the public Decode* APIs and their exact loss and
// error behavior; no serialize/decode intermediate is involved.
type chatSourcePolicy struct {
	dest chatDest
	// decodeTool validates one ordinary function tool for the destination.
	decodeTool func(i int, item map[string]json.RawMessage, lim Limits, lose func(string, string)) (*ToolDef, *RouterError)
	// validateLoss validates a known-but-unbridged top-level field for
	// the destination. The Anthropic and Responses validators differ
	// observably (quoted numerics, audio/prediction shapes), so each
	// destination keeps its own function rather than a merged one.
	validateLoss func(key string, raw json.RawMessage, lose func(string, string)) *RouterError
	// storeLossReason diagnoses an explicit store:true for destinations
	// without stored state. Unused for the Anthropic destination, which
	// routes store through validateLoss instead.
	storeLossReason string
}

// decodeChatSource validates a Chat Completions request body into the IR
// under the destination policy. Malformed known fields and unknown nested
// members are validation errors in both modes; well-formed known fields
// without a reviewed destination mapping become diagnosed losses.
func decodeChatSource(raw []byte, lim Limits, pol chatSourcePolicy) (*Generation, []Loss, *RouterError) {
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

	// Closed Chat effort control for the Responses and Gemini
	// destinations. Other destinations retain their existing
	// diagnosed-loss behavior.
	var cfrEffortLevel *string

	// stream: explicit bool only; null is a type error, not false.
	if rawStream, ok := top["stream"]; ok {
		if strings.TrimSpace(string(rawStream)) == "null" {
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
		case "model", "messages", "temperature", "top_p", "max_tokens",
			"max_completion_tokens", "stop", "stream", "stream_options",
			"tools", "tool_choice", "parallel_tool_calls", "extra_body":
			// handled below
		default:
			if key == "metadata" {
				// Same-protocol route: never silently dropped. The
				// Anthropic entrypoint rejects any metadata key; the
				// Responses and Gemini entrypoints treat null as
				// absent and reject any present value.
				if pol.dest == chatDestAnthropic {
					return nil, nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
						Msg: "OpenAI metadata requires a same-protocol route"}
				}
				if rawM, ok := top[key]; ok && !isNull(rawM) {
					return nil, nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
						Msg: "OpenAI metadata requires a same-protocol route"}
				}
				continue
			}
			if key == "store" && pol.dest != chatDestAnthropic {
				// Destinations without stored state: explicit false
				// matches exactly; true would change the request and is
				// a diagnosed loss. Malformed is 400.
				rawS := top[key]
				if isNull(rawS) {
					continue
				}
				var b bool
				if err := json.Unmarshal(rawS, &b); err != nil {
					return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
						Msg: "'store' must be bool"}
				}
				if b {
					loseDrop("store", pol.storeLossReason)
				}
				continue
			}
			if knownChatField(key) {
				if key == "n" {
					// n is validated with the numeric fields below.
					continue
				}
				if isNull(top[key]) {
					continue // explicit null means absent.
				}
				if key == "reasoning_effort" && (pol.dest == chatDestResponses || pol.dest == chatDestGemini) {
					var s string
					if err := json.Unmarshal(top[key], &s); err != nil || s == "" {
						return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
							Msg: "'reasoning_effort' must be a string"}
					}
					v := s
					cfrEffortLevel = &v
					continue
				}
				if rerr := pol.validateLoss(key, top[key], loseDrop); rerr != nil {
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
		if pol.dest != chatDestAnthropic {
			// Reject quoted numerics: encoding/json coerces some quoted
			// values into json.Number, so check the literal first. The
			// Anthropic entrypoint predates this guard and keeps its
			// historical behavior verbatim.
			lit := strings.TrimSpace(string(raw))
			if lit == "" {
				return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'%s' must be a number", key)}
			}
			if lit[0] == '"' || lit[0] == '\'' {
				return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'%s' must be a number", key)}
			}
			if lit[0] != '-' && lit[0] != '+' && (lit[0] < '0' || lit[0] > '9') && lit[0] != '.' {
				return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'%s' must be a number", key)}
			}
		}
		if pol.dest == chatDestAnthropic {
			var n json.Number
			if err := json.Unmarshal(raw, &n); err != nil {
				return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'%s' must be a number", key)}
			}
			f, err := n.Float64()
			if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
				return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: "'body' must contain finite JSON numbers"}
			}
			if f < min || f > max {
				return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'%s' is outside the supported numeric range", key)}
			}
			return &f, nil
		}
		f, rerr := jsonFinite(raw, fmt.Sprintf("'%s' must be a number", key))
		if rerr != nil {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'%s' must be a number", key)}
		}
		if f < min || f > max {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'%s' is outside the supported numeric range", key)}
		}
		return &f, nil
	}
	intPos := func(key string) (*int, *RouterError) {
		raw, ok := top[key]
		if !ok || isNull(raw) {
			return nil, nil
		}
		i, rerr := jsonInt(raw, fmt.Sprintf("'%s' must be a positive integer", key))
		if rerr != nil || i <= 0 {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'%s' must be a positive integer", key)}
		}
		v := int(i)
		return &v, nil
	}
	var rerr *RouterError
	if g.Temperature, rerr = num("temperature", 0, 2); rerr != nil {
		return nil, nil, rerr
	}
	if g.TopP, rerr = num("top_p", 0, 1); rerr != nil {
		return nil, nil, rerr
	}
	maxTok, rerr := intPos("max_tokens")
	if rerr != nil {
		return nil, nil, rerr
	}
	maxComp, rerr := intPos("max_completion_tokens")
	if rerr != nil {
		return nil, nil, rerr
	}
	if maxTok != nil && maxComp != nil {
		return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: "'max_tokens' cannot be combined with 'max_completion_tokens'"}
	}
	g.MaxTokens = maxTok
	if g.MaxTokens == nil {
		g.MaxTokens = maxComp
	}
	if rawN, ok := top["n"]; ok && !isNull(rawN) {
		i, rerr := jsonInt(rawN, "'n' must be a positive integer")
		if rerr != nil || i <= 0 {
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: "'n' must be a positive integer"}
		}
		if i != 1 {
			loseDrop("n", "no reviewed bridge mapping")
		}
	}

	// stop: string or array of strings. The Anthropic destination preserves
	// it; Responses has no stop-sequence contract in the reviewed scope
	// (well-formed values are a diagnosed loss); Gemini maps it onto
	// generationConfig.stopSequences (at most 5 entries: more than that has
	// no exact mapping and is diagnosed, never truncated).
	if rawStop, ok := top["stop"]; ok && !isNull(rawStop) {
		var single string
		if err := json.Unmarshal(rawStop, &single); err == nil {
			g.Stop = []string{single}
		} else {
			var arr []json.RawMessage
			if err := json.Unmarshal(rawStop, &arr); err != nil {
				return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: "'stop' must be a string or array of strings"}
			}
			for _, e := range arr {
				var s string
				if err := json.Unmarshal(e, &s); err != nil {
					return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
						Msg: "'stop' must be a string or array of strings"}
				}
				g.Stop = append(g.Stop, s)
			}
		}
		switch pol.dest {
		case chatDestResponses:
			g.Stop = nil
			loseDrop("stop", "no reviewed bridge mapping")
		case chatDestGemini:
			if len(g.Stop) > 5 {
				loseDrop("stop", "Gemini stopSequences supports at most 5 entries")
				g.Stop = nil
			}
		case chatDestAnthropic:
			// Preserved exactly.
		}
	}

	// stream_options: only include_usage is bridged; include_obfuscation is
	// a diagnosed loss; anything else is a validation error in both modes.
	if rawSO, ok := top["stream_options"]; ok && !isNull(rawSO) {
		var so map[string]json.RawMessage
		if err := json.Unmarshal(rawSO, &so); err != nil {
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: "'stream_options' must be an object"}
		}
		for k, v := range so {
			if isNull(v) {
				return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'stream_options.%s' must be bool", k)}
			}
			var b bool
			if err := json.Unmarshal(v, &b); err != nil {
				return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'stream_options.%s' must be bool", k)}
			}
			switch k {
			case "include_usage":
				g.StreamIncludeUsage = b
			case "include_obfuscation":
				loseDrop("stream_options."+k, "no reviewed bridge mapping")
			default:
				return nil, nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
					Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", "stream_options."+k)}
			}
		}
	}

	// extra_body: closed generation extension. Known members are strictly
	// typed; unknown members are validation errors in both modes. top_k is
	// an Anthropic-native control: the Anthropic destination stashes it
	// for the encoder, while the Responses and Gemini destinations
	// diagnose it (never forwarded as a foreign topK).
	var extraTopK *int
	var cfrHasThinking bool
	if rawEB, ok := top["extra_body"]; ok && !isNull(rawEB) {
		var eb map[string]json.RawMessage
		if err := json.Unmarshal(rawEB, &eb); err != nil {
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: "Unsupported or invalid extra_body generation extension"}
		}
		for k, v := range eb {
			switch k {
			case "top_k":
				i, rerr := jsonInt(v, "Unsupported or invalid extra_body generation extension")
				if rerr != nil || i < 0 {
					return nil, nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
						Msg: "Unsupported or invalid extra_body generation extension"}
				}
				if pol.dest == chatDestAnthropic {
					tk := int(i)
					extraTopK = &tk
				} else {
					loseDrop("extra_body."+k, "no reviewed bridge mapping")
				}
			case "min_p", "repetition_penalty":
				f, rerr := jsonFinite(v, "Unsupported or invalid extra_body generation extension")
				if rerr != nil {
					return nil, nil, rerr
				}
				_ = f
				loseDrop("extra_body."+k, "no reviewed bridge mapping")
			case "thinking":
				if rerr := validateThinking(v); rerr != nil {
					return nil, nil, rerr
				}
				if pol.dest == chatDestResponses || pol.dest == chatDestGemini {
					cfrHasThinking = true
				}
				loseDrop("extra_body."+k, "no reviewed bridge mapping")
			default:
				return nil, nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
					Msg: "Unsupported or invalid extra_body generation extension"}
			}
		}
	}

	// messages: destination-neutral text-plus-ordinary-tools scope.
	// Chat effort + thinking conflict fails pre-I/O in both modes for the
	// migrated Responses and Gemini destinations, even though thinking
	// alone remains a diagnosed loss.
	if (pol.dest == chatDestResponses || pol.dest == chatDestGemini) && cfrEffortLevel != nil && cfrHasThinking {
		return nil, nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
			Msg: "Unsupported or invalid reasoning_effort for translation: reasoning effort cannot be combined with a thinking object"}
	}
	if cfrEffortLevel != nil {
		if intent, ok := NewChatEffort(*cfrEffortLevel); ok {
			g.Reasoning = intent
		} else {
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: "'reasoning_effort' must be a string"}
		}
	}
	rawMsgs, ok := top["messages"]
	if !ok || isNull(rawMsgs) {
		return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: "'messages' must be an array"}
	}
	msgs, rerr := decodeChatMessages(rawMsgs, lose, lim, pol.dest)
	if rerr != nil {
		return nil, nil, rerr
	}
	g.Messages = msgs

	// tools: ordinary JSON-schema function tools, plus native freeform
	// custom tools for the Responses destination only. Any other tool type
	// is rejected specifically, in both modes.
	if rawTools, ok := top["tools"]; ok && !isNull(rawTools) {
		var arr []map[string]json.RawMessage
		if err := json.Unmarshal(rawTools, &arr); err != nil {
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: "'tools' must contain objects"}
		}
		names := map[string]bool{}
		customKinds := map[string]bool{}
		for i, item := range arr {
			def, rerr := pol.decodeTool(i, item, lim, loseDrop)
			if rerr != nil {
				return nil, nil, rerr
			}
			if names[def.Name] {
				return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'tools[%d].function.name' conflicts with another tool definition", i)}
			}
			names[def.Name] = true
			customKinds[def.Name] = def.IsCustom()
			g.Tools = append(g.Tools, *def)
		}
		g.HasTools = true
		// stash kinds for choice kind-matching below
		_ = customKinds
	}

	// tool_choice.
	if rawTC, ok := top["tool_choice"]; ok && !isNull(rawTC) {
		choice, rerr := decodeChatToolChoiceForDest(rawTC, pol.dest)
		if rerr != nil {
			return nil, nil, rerr
		}
		g.Choice = choice
	}

	// parallel_tool_calls: explicit bool; null means absent. Gemini has no
	// parallel-disable mapping in the reviewed contract: false is a
	// diagnosed loss, true (or omitted) is exact.
	if rawPTC, ok := top["parallel_tool_calls"]; ok && !isNull(rawPTC) {
		var b bool
		if err := json.Unmarshal(rawPTC, &b); err != nil {
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: "'parallel_tool_calls' must be bool"}
		}
		if pol.dest == chatDestGemini && !b {
			loseDrop("parallel_tool_calls", "no reviewed bridge mapping")
		} else {
			g.ParallelToolCalls = &b
		}
	}

	if (g.Choice != nil && (g.Choice.Mode == "specific" || g.Choice.Mode == "any")) && !g.HasTools {
		return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: "'tool_choice' requires 'tools'"}
	}

	// A named choice must reference a retained tool being sent, with a
	// matching family (custom choices reference custom tools, function
	// choices reference function tools). Reconciling to auto would
	// silently alter the request; reject instead.
	if g.Choice != nil && g.Choice.Mode == "specific" {
		found := false
		kindOK := false
		for _, t := range g.Tools {
			if t.Name == g.Choice.Name {
				found = true
				wantCustom := g.Choice.IsCustomChoice()
				if t.IsCustom() == wantCustom {
					kindOK = true
				}
				break
			}
		}
		if !found {
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: "'tool_choice' names an unknown tool"}
		}
		if !kindOK {
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: "'tool_choice' names an unknown tool"}
		}
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

	// Stash extra top_k for the encoder via a synthetic loss-free channel.
	if extraTopK != nil {
		g.ExtraTopK = extraTopK
	}
	// Chat -> Responses native custom plan: track custom definition and
	// history identities for the request-scoped plan. Native custom is
	// exact (no loss); history orphans without a current definition are
	// admitted with their own binding, mirroring the Python flattener.
	if pol.dest == chatDestResponses {
		builder := NewToolPlanBuilder(nil)
		for i, t := range g.Tools {
			if t.IsCustom() {
				builder.AddNativeCustom(t.Name, fmt.Sprintf("tools[%d]", i))
			}
		}
		for mi, m := range g.Messages {
			for ci, tc := range m.ToolCalls {
				if tc.IsCustom {
					builder.AddNativeCustom(tc.Name, fmt.Sprintf("messages[%d].tool_calls[%d]", mi, ci))
				}
			}
		}
		// An empty plan still declares that no custom replies are expected.
		g.ToolPlan = builder.Build()
	}
	// Chat -> Anthropic/Gemini emulated custom plan: freeform customs are
	// approximated as single {"content":string} functions upstream. Each
	// definition and each orphan history custom carries its own
	// approximation diagnostic so strict policy refuses pre-I/O; the
	// shared typed restore stage maps returned functions back to Chat
	// custom calls. Function/custom family mismatches fail closed.
	if pol.dest == chatDestAnthropic || pol.dest == chatDestGemini {
		hasCustomDef := false
		for _, t := range g.Tools {
			if t.IsCustom() {
				hasCustomDef = true
				break
			}
		}
		hasCustomHist := false
		for _, m := range g.Messages {
			for _, tc := range m.ToolCalls {
				if tc.IsCustom {
					hasCustomHist = true
					break
				}
			}
		}
		if hasCustomDef || hasCustomHist {
			funcNames := map[string]bool{}
			for _, t := range g.Tools {
				if !t.IsCustom() {
					funcNames[t.Name] = true
				}
			}
			plan := NewToolPlanBuilder(nil).Build()
			for i, t := range g.Tools {
				if t.IsCustom() {
					lose(fmt.Sprintf("tools[%d]", i), responsesCustomEmuReason, true)
					plan.addEmulatedCustom(t.Name, ToolIdentity{Name: t.Name})
				}
			}
			for mi, m := range g.Messages {
				for _, tc := range m.ToolCalls {
					if tc.IsCustom {
						if funcNames[tc.Name] {
							return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
								Msg: fmt.Sprintf("'messages[%d]' uses a conflicting tool identity", mi)}
						}
						if !plan.IsEmulatedCustom(tc.Name) {
							lose(fmt.Sprintf("messages[%d].tool_calls", mi), responsesCustomEmuReason, true)
							plan.addEmulatedCustom(tc.Name, ToolIdentity{Name: tc.Name})
						}
					} else {
						if plan.IsEmulatedCustom(tc.Name) {
							return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
								Msg: fmt.Sprintf("'messages[%d]' uses a conflicting tool identity", mi)}
						}
						funcNames[tc.Name] = true
					}
				}
			}
			if !plan.Empty() {
				g.ToolPlan = plan
			}
		}
	}
	return g, boundLosses(losses), nil
}

// boundLosses sorts losses by field path for deterministic diagnostics and
// caps the record count so headers and error text stay finite.
func boundLosses(losses []Loss) []Loss {
	sort.SliceStable(losses, func(i, j int) bool { return losses[i].Field < losses[j].Field })
	if len(losses) <= maxLossRecords {
		return losses
	}
	kept := append([]Loss(nil), losses[:maxLossRecords-1]...)
	return append(kept, Loss{Field: "losses", Reason: "additional losses truncated at bound", Action: "dropped"})
}

func isNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

// jsonInt parses a JSON integer literal strictly: no fractions, exponents,
// quoted strings, or non-numeric types, so 1.0 and "1" never satisfy an
// integer field. (encoding/json would otherwise coerce a JSON string into a
// json.Number.)
func jsonInt(raw json.RawMessage, msg string) (int64, *RouterError) {
	bad := &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: msg}
	lit := strings.TrimSpace(string(raw))
	if lit == "" {
		return 0, bad
	}
	digits := strings.TrimPrefix(lit, "-")
	digits = strings.TrimPrefix(digits, "+")
	if digits == "" {
		return 0, bad
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, bad
		}
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, bad
	}
	i, err := n.Int64()
	if err != nil {
		return 0, bad
	}
	return i, nil
}

func jsonFinite(raw json.RawMessage, msg string) (float64, *RouterError) {
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: msg}
	}
	f, err := n.Float64()
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: msg}
	}
	return f, nil
}

// validateLossField strictly validates a known-but-unbridged field: a
// well-formed value becomes a loss, a malformed one is a 400 in both modes.
func validateLossField(key string, raw json.RawMessage, lose func(string, string)) *RouterError {
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
		if _, rerr := jsonFinite(raw, ""); rerr != nil {
			return bad(fmt.Sprintf("'%s' must be a number", key))
		}
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
	case "audio", "prediction":
		if _, rerr := mustObj(); rerr != nil {
			return rerr
		}
		return loss("no reviewed bridge mapping")
	case "frequency_penalty", "presence_penalty":
		return mustNum()
	case "function_call":
		// String form ("none"/"auto") or an explicit function reference.
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
			prefix := fmt.Sprintf("'functions[%d]'", i)
			for k, v := range fn {
				switch k {
				case "name":
					var name string
					if err := json.Unmarshal(v, &name); err != nil || name == "" {
						return bad(prefix + ".name' must be a non-empty string")
					}
				case "description":
					var s string
					if err := json.Unmarshal(v, &s); err != nil {
						return bad(prefix + ".description' must be a string")
					}
				case "parameters":
					var obj map[string]json.RawMessage
					if err := json.Unmarshal(v, &obj); err != nil || obj == nil {
						return bad(prefix + ".parameters' must be an object")
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
			if _, rerr := jsonFinite(v, ""); rerr != nil {
				return bad("'logit_bias' values must be numbers")
			}
		}
		return loss("no reviewed bridge mapping")
	case "logprobs", "store":
		return mustBool()
	case "modalities":
		return mustStrArray()
	case "reasoning_effort", "verbosity", "prompt_cache_key", "prompt_cache_retention",
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

func validateThinking(raw json.RawMessage) *RouterError {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return &RouterError{Status: 400, Code: CodeInvalidGeneration,
			Msg: "Unsupported or invalid extra_body generation extension"}
	}
	for k, v := range obj {
		switch k {
		case "type":
			var s string
			if err := json.Unmarshal(v, &s); err != nil || s == "" {
				return &RouterError{Status: 400, Code: CodeInvalidGeneration,
					Msg: "Unsupported or invalid extra_body generation extension"}
			}
		case "budget_tokens":
			i, rerr := jsonInt(v, "Unsupported or invalid extra_body generation extension")
			if rerr != nil || i <= 0 {
				return &RouterError{Status: 400, Code: CodeInvalidGeneration,
					Msg: "Unsupported or invalid extra_body generation extension"}
			}
		default:
			return &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: "Unsupported or invalid extra_body generation extension"}
		}
	}
	return nil
}

// knownChatField is the closed universe of Chat request fields recognized by
// the Python router's generation contract. Fields here without a milestone-2
// bridge become diagnosed losses when well-formed; anything else is a
// validation error.
func knownChatField(key string) bool {
	switch key {
	case "messages", "audio", "frequency_penalty", "function_call", "functions",
		"logit_bias", "logprobs", "max_completion_tokens", "max_tokens",
		"modalities", "n", "parallel_tool_calls", "prediction",
		"presence_penalty", "reasoning_effort", "response_format", "seed",
		"stop", "store", "stream", "stream_options", "temperature", "tools",
		"tool_choice", "top_logprobs", "top_p", "verbosity",
		"web_search_options", "extra_body", "model",
		"prompt_cache_key", "prompt_cache_retention", "safety_identifier",
		"service_tier", "user":
		return true
	}
	return false
}

func decodeChatMessages(raw json.RawMessage, lose func(string, string, bool), lim Limits, dest chatDest) ([]Message, *RouterError) {
	var arr []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: "'messages' must be an array"}
	}
	var out []Message
	for i, item := range arr {
		if item == nil {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: "'messages' must contain objects with string roles"}
		}
		m, rerr := decodeChatMessage(i, item, lose, lim, dest)
		if rerr != nil {
			return nil, rerr
		}
		out = append(out, *m)
	}
	return out, nil
}

func chatStringField(item map[string]json.RawMessage, key string) (string, bool, *RouterError) {
	raw, ok := item[key]
	if !ok || isNull(raw) {
		return "", false, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'messages.%s' must be a string", key)}
	}
	return s, true, nil
}

func decodeChatMessage(i int, item map[string]json.RawMessage, lose func(string, string, bool), lim Limits, dest chatDest) (*Message, *RouterError) {
	role, ok, rerr := chatStringField(item, "role")
	if rerr != nil {
		return nil, rerr
	}
	if !ok || strings.TrimSpace(role) == "" {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: "'messages' must contain objects with string roles"}
	}
	for key := range item {
		switch key {
		case "role", "content", "tool_calls", "tool_call_id", "name":
		default:
			return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: messages[%d].%s", i, key)}
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
	case "tool":
		m.Role = RoleTool
	case "developer":
		if dest == chatDestResponses {
			m.Role = RoleDeveloper
		} else {
			m.Role = RoleSystem
			lose(fmt.Sprintf("messages[%d].role", i), "target system instructions cannot preserve developer role precedence", true)
		}
	case "function":
		return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
			Msg: fmt.Sprintf("messages[%d].role %q requires a same-protocol route", i, role)}
	default:
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: "'messages' must contain objects with string roles"}
	}
	// name: an unmapped identity hint. Null means absent; a string becomes a
	// loss; any other type is malformed.
	if rawName, ok := item["name"]; ok && !isNull(rawName) {
		var s string
		if err := json.Unmarshal(rawName, &s); err != nil {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'messages[%d].name' must be a string", i)}
		}
		lose(fmt.Sprintf("messages[%d].name", i), "no reviewed bridge mapping", false)
	}

	// content: string | array of blocks | null.
	if rawC, ok := item["content"]; ok && !isNull(rawC) {
		var s string
		if err := json.Unmarshal(rawC, &s); err == nil {
			if s != "" || m.Role == RoleUser || m.Role == RoleSystem || m.Role == RoleDeveloper {
				m.Texts = append(m.Texts, TextContent{Text: s})
			} else if m.Role == RoleAssistant && s != "" {
				m.Texts = append(m.Texts, TextContent{Text: s})
			}
		} else {
			var blocks []map[string]json.RawMessage
			if err := json.Unmarshal(rawC, &blocks); err != nil {
				return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: "'messages.content' must be a string, array or null"}
			}
			var ordered []ContentBlock
			hasImage := false
			for bi, b := range blocks {
				if b == nil {
					return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
						Msg: "'messages.content' must contain content block objects"}
				}
				t, img, rerr := decodeChatContentBlock(i, bi, b, m.Role, func(field, reason string) { lose(field, reason, false) }, lim, dest)
				if rerr != nil {
					return nil, rerr
				}
				if t != nil {
					m.Texts = append(m.Texts, *t)
					if t.Text != "" {
						ordered = append(ordered, ContentBlock{Kind: ContentKindText, Text: t.Text})
					}
				}
				if img != nil {
					hasImage = true
					ordered = append(ordered, *img)
				}
			}
			if hasImage && m.Role == RoleUser {
				m.Blocks = ordered
			}
		}
	}

	if m.Role == RoleAssistant {
		if rawTC, ok := item["tool_calls"]; ok && !isNull(rawTC) {
			var calls []map[string]json.RawMessage
			if err := json.Unmarshal(rawTC, &calls); err != nil {
				return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: "'messages.tool_calls' must be an array"}
			}
			for ci, c := range calls {
				if c == nil {
					return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
						Msg: fmt.Sprintf("'messages[%d].tool_calls[%d]' must be an object", i, ci)}
				}
				tc, rerr := decodeChatToolCallForDest(i, ci, c, lim, dest)
				if rerr != nil {
					return nil, rerr
				}
				m.ToolCalls = append(m.ToolCalls, *tc)
			}
		} else if _, ok := item["tool_calls"]; ok {
			// Explicit null: absent.
		}
	} else if rawTC, ok := item["tool_calls"]; ok && !isNull(rawTC) {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: "'messages.tool_calls' is only valid on assistant messages"}
	}

	if m.Role == RoleTool {
		id, ok, rerr := chatStringField(item, "tool_call_id")
		if rerr != nil {
			return nil, rerr
		}
		if !ok || id == "" {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: "'messages.tool_call_id' is required on tool messages"}
		}
		// Wrapped identities restore the provider identity so the
		// result pairs with the unwrapped assistant call; any other
		// value passes through untouched.
		if _, inner, isWrapped := unwrapGeminiSignatureID(id); isWrapped {
			id = inner
		}
		// Tool results carry plain text content only.
		if rawC, ok := item["content"]; ok && !isNull(rawC) {
			var s string
			if err := json.Unmarshal(rawC, &s); err != nil {
				return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: "'messages.content' must be a string, array or null"}
			}
		}
		var content string
		for _, t := range m.Texts {
			content += t.Text
		}
		m.Texts = nil
		m.Results = append(m.Results, ToolResult{CallID: id, Content: content})
	}
	return m, nil
}

func decodeChatContentBlock(mi, bi int, b map[string]json.RawMessage, role Role, lose func(string, string), lim Limits, dest chatDest) (*TextContent, *ContentBlock, *RouterError) {
	prefix := fmt.Sprintf("messages[%d].content[%d]", mi, bi)
	var typ string
	if raw, ok := b["type"]; ok && !isNull(raw) {
		if err := json.Unmarshal(raw, &typ); err != nil {
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", prefix+".type")}
		}
	}
	for k := range b {
		switch k {
		case "type", "text", "image_url", "input_audio", "file", "refusal":
		default:
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", prefix+"."+k)}
		}
	}
	invalid := func(msg string) *RouterError {
		return &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: msg}
	}
	switch typ {
	case "text":
		raw, ok := b["text"]
		if !ok || isNull(raw) {
			return nil, nil, invalid("'messages.content' text blocks require a string 'text'")
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			// Structured text parts have no reviewed mapping: refuse.
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", prefix+".text")}
		}
		if s == "" && role != RoleUser {
			return nil, nil, nil
		}
		return &TextContent{Text: s}, nil, nil
	case "refusal":
		// Refusal text is assistant text; it has an exact representation.
		raw, ok := b["refusal"]
		if !ok || isNull(raw) {
			return nil, nil, invalid("'messages.content' refusal blocks require a string 'refusal'")
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, nil, invalid("'messages.content' refusal blocks require a string 'refusal'")
		}
		if s == "" {
			return nil, nil, nil
		}
		return &TextContent{Text: s}, nil, nil
	case "image_url":
		if rerr := validateMediaBlock(prefix, b, "image_url", map[string]bool{"url": true, "detail": true}); rerr != nil {
			return nil, nil, rerr
		}
		if dest == chatDestGemini && role == RoleUser {
			return decodeChatImageURLForGemini(prefix, b, lose, lim)
		}
		lose(prefix+"."+typ, "no reviewed bridge mapping")
		return nil, nil, nil
	case "input_audio":
		if rerr := validateMediaBlock(prefix, b, "input_audio", map[string]bool{"data": true, "format": true}); rerr != nil {
			return nil, nil, rerr
		}
		lose(prefix+"."+typ, "no reviewed bridge mapping")
		return nil, nil, nil
	case "file":
		// The file-block schema varies by harness and is not reviewed:
		// refuse rather than silently drop it.
		return nil, nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
			Msg: fmt.Sprintf("messages[%d].content[%d].file requires a same-protocol route", mi, bi)}
	default:
		return nil, nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
			Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", prefix+".type")}
	}
}

// decodeChatImageURLForGemini bridges one user-role Chat image_url block to
// a Gemini inlineData image, preserving order via ContentBlock. It reuses
// the shared neutral MIME allowlist and base64 canonicalization (media_image.go),
// so the finite PNG/JPEG/WebP/GIF policy matches the Gemini->Chat direction
// exactly. No URI is fetched and no pixels are decoded.
//
//   - data: URLs with a listed MIME bridge with exact bytes (lowercased MIME,
//     standard padded base64).
//   - Remote http(s) URLs keep the existing reviewed loss
//     (messages[i].content[j].image_url, "no reviewed bridge mapping"), never
//     fetched.
//   - Well-formed but unlisted data-URL MIMEs are a diagnosed media loss,
//     never a silent drop.
//   - Malformed shape (bad MIME syntax, missing ;base64, empty/invalid or
//     unbounded base64) is 400 pre-I/O in both modes.
//   - detail "auto"/absent is exact; any other explicit detail has no Gemini
//     equivalent and is diagnosed as a dropped loss while the image still
//     bridges (strict refuses pre-I/O, lossy bridges with a header).
func decodeChatImageURLForGemini(prefix string, b map[string]json.RawMessage, lose func(string, string), lim Limits) (*TextContent, *ContentBlock, *RouterError) {
	raw := b["image_url"]
	var urlStr, detail string
	hasDetail := false
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		urlStr = s
	} else {
		var obj map[string]json.RawMessage
		_ = json.Unmarshal(raw, &obj)
		_ = json.Unmarshal(obj["url"], &urlStr)
		if dRaw, ok := obj["detail"]; ok && !isNull(dRaw) {
			_ = json.Unmarshal(dRaw, &detail)
			hasDetail = true
		}
	}
	mime, data, isData, malformed := splitImageDataURL(urlStr)
	if !isData {
		lose(prefix+".image_url", "no reviewed bridge mapping")
		return nil, nil, nil
	}
	if malformed {
		return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s.image_url.url' must be a base64 data URL", prefix)}
	}
	path := prefix + ".image_url.url"
	img, rerr := validateImagePayload(path, path, mime, data, lim)
	if rerr != nil {
		return nil, nil, rerr
	}
	if img == nil {
		lose(prefix+".image_url", "media type has no established equivalent on this destination")
		return nil, nil, nil
	}
	if hasDetail && detail != "auto" {
		lose(prefix+".image_url.detail", "image detail has no Gemini equivalent")
	}
	return nil, &ContentBlock{Kind: ContentKindImage, Image: *img}, nil
}

// validateMediaBlock checks a reviewed media wrapper: the member must be a
// string or an object of strings limited to the allowed keys.
func validateMediaBlock(prefix string, b map[string]json.RawMessage, member string, allowed map[string]bool) *RouterError {
	raw, ok := b[member]
	if !ok || isNull(raw) {
		return &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s' blocks require '%s'", prefix, member)}
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'%s' blocks require '%s'", prefix, member)}
		}
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s' blocks require '%s'", prefix, member)}
	}
	for k, v := range obj {
		if !allowed[k] {
			return &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", prefix+"."+member+"."+k)}
		}
		var vs string
		if err := json.Unmarshal(v, &vs); err != nil || vs == "" {
			return &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'%s' blocks require '%s'", prefix, member)}
		}
	}
	if _, ok := obj["url"]; allowed["url"] && !ok {
		if _, ok := obj["data"]; !ok {
			return &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'%s' blocks require '%s'", prefix, member)}
		}
	}
	return nil
}

func decodeChatToolCall(mi, ci int, c map[string]json.RawMessage, lim Limits) (*ToolCall, *RouterError) {
	prefix := fmt.Sprintf("messages[%d].tool_calls[%d]", mi, ci)
	for k := range c {
		switch k {
		case "id", "type", "function":
		default:
			return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", prefix+"."+k)}
		}
	}
	var typ string
	if raw, ok := c["type"]; ok && !isNull(raw) {
		if err := json.Unmarshal(raw, &typ); err != nil {
			return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", prefix+".type")}
		}
	}
	if typ != "" && typ != "function" {
		return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
			Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", prefix+".type")}
	}
	rawFn, ok := c["function"]
	if !ok || isNull(rawFn) {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s.function' is required", prefix)}
	}
	var fn map[string]json.RawMessage
	if err := json.Unmarshal(rawFn, &fn); err != nil {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s.function' must be an object", prefix)}
	}
	for k := range fn {
		switch k {
		case "name", "arguments":
		default:
			return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", prefix+".function."+k)}
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
	// Router-minted wrapped identities (Gemini thought-signature
	// passback) restore the provider identity and the opaque signature;
	// any other value passes through untouched.
	sig := ""
	if s, inner, ok := unwrapGeminiSignatureID(id); ok {
		if err := validateCallID(inner, lim); err != nil {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'%s.id' %s", prefix, err.Error())}
		}
		id, sig = inner, s
	}
	var name string
	if rawN, ok := fn["name"]; ok && !isNull(rawN) {
		_ = json.Unmarshal(rawN, &name)
	}
	if err := validateToolName(name); err != nil {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s.function.name' %s", prefix, err.Error())}
	}
	args, rerr := decodeToolArguments(prefix, fn["arguments"], lim)
	if rerr != nil {
		return nil, rerr
	}
	return &ToolCall{ID: id, Name: name, Arguments: args, Signature: sig}, nil
}

// decodeToolArguments validates a JSON-stringified tool argument value: it
// must decode to a JSON object (never a scalar or array) and is preserved
// as compact raw JSON without a float64 round-trip, so large integers and
// number literals survive exactly. Empty input means "no arguments".
func decodeToolArguments(prefix string, raw json.RawMessage, lim Limits) (json.RawMessage, *RouterError) {
	bad := &RouterError{Status: 400, Code: CodeInvalidRequest,
		Msg: fmt.Sprintf("'%s.function.arguments' must be a JSON string holding an object", prefix)}
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
			Msg: fmt.Sprintf("'%s.function.arguments' exceeds the size bound", prefix)}
	}
	trimmed := bytes.TrimSpace([]byte(s))
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, bad
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(s)); err != nil {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: fmt.Sprintf("'%s.function.arguments' must be valid JSON", prefix)}
	}
	return json.RawMessage(compact.Bytes()), nil
}

// validateToolName enforces the cross-dialect tool identity contract:
// Anthropic requires ^[A-Za-z0-9_-]{1,64}$, and the router must not invent
// identities that fail downstream.
func validateToolName(name string) error {
	if len(name) == 0 || len(name) > 64 {
		return fmt.Errorf("must be 1-64 characters")
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return fmt.Errorf("must match [A-Za-z0-9_-]{1,64}")
		}
	}
	return nil
}

// validateCallID validates an opaque tool-call identity: any bounded
// nonempty string. Call IDs are client- or provider-minted opaque handles
// (the Chat completions contract requires a string id on every tool call
// and tool result; Gemini FunctionCall id is an optional string), so the
// tool-name grammar in validateToolName must never be imposed on them.
// The bound reuses the existing per-argument cap so identities stay
// inside framed budgets.
func validateCallID(id string, lim Limits) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("must be a nonempty string")
	}
	if len(id) > lim.MaxArgsChars {
		return fmt.Errorf("exceeds the size bound")
	}
	return nil
}

func decodeChatTool(i int, item map[string]json.RawMessage, lim Limits) (*ToolDef, *RouterError) {
	for k := range item {
		switch k {
		case "type", "function":
		default:
			return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: tools[%d].%s", i, k)}
		}
	}
	var typ string
	if raw, ok := item["type"]; ok && !isNull(raw) {
		if err := json.Unmarshal(raw, &typ); err != nil {
			return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: tools[%d].type", i)}
		}
	}
	// Milestone 2 supports ordinary JSON-schema function tools only.
	// Anything else — custom tools, computer use, unknown types — has no
	// representation and is rejected specifically, in both modes.
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
	if rawS, ok := fn["strict"]; ok && !isNull(rawS) {
		var b bool
		if err := json.Unmarshal(rawS, &b); err != nil {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'tools[%d].function.strict' must be bool", i)}
		}
	}
	return &ToolDef{Name: name, Description: desc, Schema: schema}, nil
}

func decodeChatToolChoice(raw json.RawMessage) (*ToolChoice, *RouterError) {
	return decodeChatToolChoiceForDest(raw, chatDestAnthropic)
}

// decodeChatToolChoiceForDest validates the tool_choice union with
// destination-aware custom support: every destination admits
// {"type":"custom","custom":{"name"}} exactly (Responses natively,
// Anthropic/Gemini as emulated functions); any other destination
// keeps the function-only contract for unknown shapes.
func decodeChatToolChoiceForDest(raw json.RawMessage, dest chatDest) (*ToolChoice, *RouterError) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "none":
			return &ToolChoice{Mode: "none"}, nil
		case "auto":
			return &ToolChoice{Mode: "auto"}, nil
		case "required":
			return &ToolChoice{Mode: "any"}, nil
		default:
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: "'tool_choice' must be 'none', 'auto', 'required' or a function reference"}
		}
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: "'tool_choice' must be 'none', 'auto', 'required' or a function reference"}
	}
	for k := range obj {
		switch k {
		case "type", "function", "custom":
		default:
			return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", "tool_choice."+k)}
		}
	}
	var typ string
	if rawT, ok := obj["type"]; ok && !isNull(rawT) {
		_ = json.Unmarshal(rawT, &typ)
	}
	switch typ {
	case "function":
		if _, ok := obj["custom"]; ok && !isNull(obj["custom"]) {
			return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: "Unsupported or invalid request field for translation: \"tool_choice.type\""}
		}
		rawFn, ok := obj["function"]
		if !ok || isNull(rawFn) {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: "'tool_choice.function' must be an object"}
		}
		var fn map[string]json.RawMessage
		if err := json.Unmarshal(rawFn, &fn); err != nil {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: "'tool_choice.function' must be an object"}
		}
		for k := range fn {
			if k != "name" {
				return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
					Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", "tool_choice.function."+k)}
			}
		}
		var name string
		if rawN, ok := fn["name"]; ok && !isNull(rawN) {
			_ = json.Unmarshal(rawN, &name)
		}
		if err := validateToolName(name); err != nil {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'tool_choice.function.name' %s", err.Error())}
		}
		return &ToolChoice{Mode: "specific", Name: name, Kind: ToolKindFunction}, nil
	case "custom":
		if _, ok := obj["function"]; ok && !isNull(obj["function"]) {
			return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: "Unsupported or invalid request field for translation: \"tool_choice.type\""}
		}
		name, rerr := decodeChatCustomChoice(obj)
		if rerr != nil {
			return nil, rerr
		}
		return &ToolChoice{Mode: "specific", Name: name, Kind: ToolKindCustom}, nil
	default:
		return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
			Msg: "Unsupported or invalid request field for translation: \"tool_choice.type\""}
	}
}

// decodeChatToolCallForDest validates one history tool call. Custom
// history is admitted on every destination (Responses natively,
// Anthropic/Gemini as emulated functions); the caller builds the
// emulated plan and records orphan losses.
func decodeChatToolCallForDest(mi, ci int, c map[string]json.RawMessage, lim Limits, dest chatDest) (*ToolCall, *RouterError) {
	var typ string
	if raw, ok := c["type"]; ok && !isNull(raw) {
		_ = json.Unmarshal(raw, &typ)
	}
	if typ == "custom" {
		return decodeChatCustomToolCall(mi, ci, c, lim)
	}
	return decodeChatToolCall(mi, ci, c, lim)
}
