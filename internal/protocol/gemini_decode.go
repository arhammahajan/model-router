// Gemini ingress decoding: closed, typed validation of a
// GenerateContentRequest body into the semantic IR for the Chat upstream.
// The public model and the streaming mode come from the parsed URL and
// method (never from the body): a body carrying "model" or "stream" is
// rejected. Unknown fields are validation errors, never silent accepts.
// Known-but-unbridged fields become Loss records for the strict/lossy
// policy — but only when well-formed: malformed types/values and unknown
// nested members are validation errors in both modes, before upstream I/O.
//
// Null policy mirrors the Chat decoder: explicit null on an optional field
// means "absent" and is skipped, except for nested boolean controls, where
// null is a type error. Omitted, null, zero and false are distinguished
// explicitly at every boundary.
//
// Alias policy: Gemini REST uses camelCase, but clients send snake_case
// spellings as well (system_instruction, generation_config, ...). Both are
// accepted at every object boundary; a request that sets the same member in
// both spellings is rejected as a collision, and unknown nested members are
// rejected in both modes. Only these documented camel/snake pairs exist:
// no other renaming is performed.
//
// Ordering rule: the IR carries assistant text as one ordered string plus
// an ordered tool-call list, and tool results as an ordered list. A model
// content that interleaves text with functionCall, or a user content that
// mixes text with functionResponse, has no exact destination
// representation: reordering silently would change the conversation, so
// mixed-order contents are rejected in both modes rather than guessed.
// The bounded media verticals preserve user text/media interleave in
// order: user Blocks hold text, image_url, input_audio, and file blocks
// exactly as sent for the Chat destination; mixing that content with
// functionResponse in one Content is still rejected in both modes.
//
// Media rule (bounded image/audio/PDF verticals): only user-role inlineData
// media in the finite sets (image/png, image/jpeg, image/webp, image/gif to
// image_url; audio/wav, audio/x-wav, audio/wave to input_audio wav and
// audio/mpeg, audio/mp3 to mp3; application/pdf to file) bridges to Chat
// with exact MIME/data preservation and safe filename handling. All other
// well-formed media (other MIME types, model-produced media, fileData URI
// references, FunctionResponse parts) keeps the existing diagnosed
// loss/refusal. Malformed inline shape, MIME syntax, base64, or displayName
// is 400 pre-I/O in both modes; base64 is bounded by the existing
// per-field limit and never decoded to pixels/audio or fetched.
//
// Identity policy (documented, scoped to one request): supplied
// functionCall/functionResponse ids are opaque provider handles and are
// preserved exactly when non-empty and bounded; the tool-name grammar
// ([A-Za-z0-9_-]{1,64}) applies to function *names* only, never to call
// ids. A missing id on a functionCall mints a router-local compatibility
// identity "gdec-N"; a missing id on a functionResponse correlates by
// name to the single pending call with that name. Zero matches, or more
// than one pending call with the same name (parallel same-name
// ambiguity, including repeats across turns), is a 400: the router never
// invents provider-private opaque ids or signatures.
//
// Source contracts (verified November 2026):
//
//	GenerateContentRequest/Response, Content/Part, FunctionDeclaration,
//	FunctionCall/FunctionResponse id/args/response shapes, usageMetadata
//	(promptTokenCount still includes cached content; totalTokenCount covers
//	prompt + tool-use + thoughts + candidates):
//	https://ai.google.dev/api/generate-content
//	streamGenerateContent (alt=sse SSE frames; otherwise the stream is one
//	JSON array of GenerateContentResponse objects):
//	https://ai.google.dev/api/rest/v1beta/models/streamGenerateContent
//	Function calling (declaration parameters vs parametersJsonSchema,
//	optional call/response ids):
//	https://ai.google.dev/gemini-api/docs/function-calling
//	Chat completions request/response and streaming (finish reasons,
//	delta tool_calls, terminal usage chunk, [DONE]):
//	https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create
package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// DecodeGeminiRequest decodes raw JSON Gemini GenerateContentRequest bytes
// into the IR plus the planned losses. publicModel is the model from the
// parsed request URL and stream is the method-derived mode
// (streamGenerateContent); both are set on the Generation for the
// destination encoder, which always adds stream_options.include_usage on
// streams for the Chat destination. It performs no I/O.
func DecodeGeminiRequest(raw []byte, publicModel string, stream bool, lim Limits) (*Generation, []Loss, *RouterError) {
	return decodeGeminiRequest(raw, publicModel, stream, lim, geminiDestChat)
}

// decodeGeminiRequest validates the shared Gemini source contract once
// and applies only the small destination policy (stopSequences handling
// and loss wording). Source validation, errors, and losses are otherwise
// identical for all destinations.
func decodeGeminiRequest(raw []byte, publicModel string, stream bool, lim Limits, dest geminiDestination) (*Generation, []Loss, *RouterError) {
	var top map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&top); err != nil {
		return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: "'body' must be a JSON object"}
	}
	if top == nil {
		return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: "'body' must be a JSON object"}
	}
	// The model and the streaming mode live in the URL and method, never in
	// the Gemini body.
	for _, k := range []string{"model", "stream"} {
		if _, ok := top[k]; ok {
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'%s' must come from the request URL and method, not the body", k)}
		}
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

	g := &Generation{Model: publicModel, Stream: stream}

	obj, rerr := gdecObject(top, map[string]bool{
		"contents": true, "systemInstruction": true, "tools": true,
		"toolConfig": true, "safetySettings": true, "generationConfig": true,
		"cachedContent": true, "labels": true, "continuationToken": true,
		"serviceTier": true, "store": true,
	}, "request")
	if rerr != nil {
		return nil, nil, rerr
	}
	// Router-held state references have no translated representation: they
	// require a native Gemini route.
	for _, k := range []string{"cachedContent", "continuationToken"} {
		if v, ok := obj[k]; ok && !isNull(v) {
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'%s' refers to state held by Gemini and requires a native Gemini route", k)}
		}
	}

	d := &gdecDecoder{lim: lim, loseDrop: loseDrop, lose: lose, dest: dest}
	if rawContents, ok := obj["contents"]; ok && !isNull(rawContents) {
		d.reservedIDs = gdecCollectSuppliedIDs(rawContents)
	}
	if rawSys, ok := obj["systemInstruction"]; ok && !isNull(rawSys) {
		text, rerr := d.systemText(rawSys)
		if rerr != nil {
			return nil, nil, rerr
		}
		if text != "" {
			g.Messages = append(g.Messages, Message{Role: RoleSystem, Texts: []TextContent{{Text: text}}})
		}
	}
	if rawContents, ok := obj["contents"]; !ok || isNull(rawContents) {
		return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: "'contents' must be a non-empty array"}
	} else {
		msgs, rerr := d.messages(rawContents)
		if rerr != nil {
			return nil, nil, rerr
		}
		g.Messages = append(g.Messages, msgs...)
	}
	hasNonSystem := false
	for _, m := range g.Messages {
		if m.Role != RoleSystem {
			hasNonSystem = true
		}
	}
	if !hasNonSystem {
		return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
			Msg: "'contents' has nothing that can be translated to this destination"}
	}

	if rawGen, ok := obj["generationConfig"]; ok && !isNull(rawGen) {
		if rerr := d.generation(rawGen, g); rerr != nil {
			return nil, nil, rerr
		}
	}
	if rawTools, ok := obj["tools"]; ok && !isNull(rawTools) {
		if rerr := d.tools(rawTools, g); rerr != nil {
			return nil, nil, rerr
		}
	}
	if rawTC, ok := obj["toolConfig"]; ok && !isNull(rawTC) {
		if rerr := d.toolConfig(rawTC, g); rerr != nil {
			return nil, nil, rerr
		}
	}
	if rawSafety, ok := obj["safetySettings"]; ok && !isNull(rawSafety) {
		var arr []map[string]json.RawMessage
		if err := json.Unmarshal(rawSafety, &arr); err != nil {
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: "'safetySettings' must be an array"}
		}
		for i, item := range arr {
			if item == nil {
				return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: "'safetySettings' must contain objects"}
			}
			safety, rerr := gdecObject(item, map[string]bool{
				"category": true, "threshold": true, "method": true,
			}, fmt.Sprintf("safetySettings[%d]", i))
			if rerr != nil {
				return nil, nil, rerr
			}
			for _, k := range []string{"category", "threshold", "method"} {
				if v, ok := safety[k]; ok && !isNull(v) {
					var s string
					if err := json.Unmarshal(v, &s); err != nil || s == "" {
						return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
							Msg: fmt.Sprintf("'safetySettings[%d].%s' must be a non-empty string", i, k)}
					}
				}
			}
		}
		loseDrop("safetySettings", "safety thresholds are Gemini-specific; the destination applies its own moderation")
	}
	if rawLabels, ok := obj["labels"]; ok && !isNull(rawLabels) {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(rawLabels, &m); err != nil || m == nil {
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: "'labels' must be an object"}
		}
		for k, v := range m {
			if isNull(v) {
				continue
			}
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
					Msg: fmt.Sprintf("'labels.%s' must be a string", k)}
			}
		}
		loseDrop("labels", "request labels are Gemini billing metadata with no equivalent")
	}
	if rawTier, ok := obj["serviceTier"]; ok && !isNull(rawTier) {
		var s string
		if err := json.Unmarshal(rawTier, &s); err != nil || s == "" {
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: "'serviceTier' must be a string"}
		}
		loseDrop("serviceTier", "service tiers are provider-specific")
	}
	if rawStore, ok := obj["store"]; ok && !isNull(rawStore) {
		var b bool
		if err := json.Unmarshal(rawStore, &b); err != nil {
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: "'store' must be bool"}
		}
		loseDrop("store", "the logging preference is provider-specific")
	}

	if g.Choice != nil && (g.Choice.Mode == "specific" || g.Choice.Mode == "any") && !g.HasTools {
		return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: "'toolConfig' requires 'tools'"}
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
			return nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: "'toolConfig' names an unknown tool"}
		}
	}
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

// gdecDecoder carries the per-request decode state: the destination
// policy, the reserved supplied ids (so minted compat ids never collide),
// the used id set, the pending model calls awaiting functionResponses,
// and the loss channels.
type gdecDecoder struct {
	lim         Limits
	dest        geminiDestination
	lose        func(string, string, bool)
	loseDrop    func(string, string)
	nextID      int
	reservedIDs map[string]bool
	usedIDs     map[string]bool
	pending     []gdecPending
}

type gdecPending struct {
	id   string
	name string
}

// gdecSnake converts one snake_case spelling to camelCase, mirroring the
// documented Gemini alias rule.
var gdecSnakeRe = regexp.MustCompile(`_([a-z0-9])`)

func gdecSnake(name string) string {
	return gdecSnakeRe.ReplaceAllStringFunc(name, func(m string) string {
		return strings.ToUpper(m[1:])
	})
}

// gdecObject validates a JSON object with camelCase-or-snake_case members.
// Members mapping to the same camelCase name from both spellings are a
// collision error; unknown members are translation errors. Null members
// are kept (callers treat null as absent); the returned map is keyed by
// camelCase.
func gdecObject(item map[string]json.RawMessage, allowed map[string]bool, what string) (map[string]json.RawMessage, *RouterError) {
	out := make(map[string]json.RawMessage, len(item))
	keys := make([]string, 0, len(item))
	for k := range item {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		camel := k
		if strings.Contains(k, "_") {
			camel = gdecSnake(k)
		}
		if !allowed[camel] {
			return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", what+"."+k)}
		}
		if _, dup := out[camel]; dup {
			return nil, &RouterError{Status: 400, Code: CodeInvalidRequest,
				Msg: fmt.Sprintf("'%s' sets %s more than once (camelCase and snake_case)", what, camel)}
		}
		out[camel] = item[k]
	}
	return out, nil
}

func (d *gdecDecoder) bad(msg string) *RouterError {
	return &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: msg}
}

func (d *gdecDecoder) systemText(raw json.RawMessage) (string, *RouterError) {
	var content map[string]json.RawMessage
	if err := json.Unmarshal(raw, &content); err != nil || content == nil {
		return "", d.bad("'systemInstruction' must be an object")
	}
	obj, rerr := gdecObject(content, map[string]bool{"role": true, "parts": true}, "systemInstruction")
	if rerr != nil {
		return "", rerr
	}
	rawParts, ok := obj["parts"]
	if !ok || isNull(rawParts) {
		return "", d.bad("'systemInstruction.parts' must be an array")
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(rawParts, &parts); err != nil {
		return "", d.bad("'systemInstruction.parts' must be an array")
	}
	var texts []string
	for i, p := range parts {
		if p == nil {
			return "", d.bad("'systemInstruction.parts' must contain objects")
		}
		part, rerr := gdecObject(p, gdecPartKeys(), fmt.Sprintf("systemInstruction.parts[%d]", i))
		if rerr != nil {
			return "", rerr
		}
		if rerr := gdecValidatePartShapes(fmt.Sprintf("systemInstruction.parts[%d]", i), part); rerr != nil {
			return "", rerr
		}
		if thoughtOnly(part) {
			d.lose(fmt.Sprintf("systemInstruction.parts[%d].thought", i), "thought summaries are provider-private reasoning state", false)
			continue
		}
		kind := gdecDataKind(part)
		if kind != "text" {
			if kind == "" {
				return "", d.bad(fmt.Sprintf("'systemInstruction.parts[%d]' must contain text", i))
			}
			return "", &RouterError{Status: 400, Code: CodeInvalidGeneration,
				Msg: fmt.Sprintf("'systemInstruction.parts[%d]' accepts text parts only", i)}
		}
		var s string
		if err := json.Unmarshal(part["text"], &s); err != nil {
			return "", d.bad(fmt.Sprintf("'systemInstruction.parts[%d].text' must be a string", i))
		}
		texts = append(texts, s)
	}
	return strings.Join(texts, "\n"), nil
}

func gdecPartKeys() map[string]bool {
	return map[string]bool{
		"text": true, "inlineData": true, "fileData": true,
		"functionCall": true, "functionResponse": true,
		"executableCode": true, "codeExecutionResult": true,
		"toolCall": true, "toolResponse": true,
		"thought": true, "thoughtSignature": true,
		"videoMetadata": true, "mediaResolution": true, "partMetadata": true,
		"mediaProcessing": true, "audioTranscription": true, "speechMetadata": true,
	}
}

// gdecDataKind names the single set data member of a normalized part, or
// "" when none is set. More than one is a malformed request.
func gdecDataKind(part map[string]json.RawMessage) string {
	found := ""
	for _, k := range []string{"text", "inlineData", "fileData", "functionCall", "functionResponse", "executableCode", "codeExecutionResult", "toolCall", "toolResponse"} {
		if v, ok := part[k]; ok && !isNull(v) {
			if found != "" {
				return "!"
			}
			found = k
		}
	}
	return found
}

func thoughtOnly(part map[string]json.RawMessage) bool {
	v, ok := part["thought"]
	return ok && !isNull(v)
}

func (d *gdecDecoder) messages(raw json.RawMessage) ([]Message, *RouterError) {
	var arr []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil || arr == nil {
		return nil, d.bad("'contents' must be a non-empty array")
	}
	if len(arr) == 0 {
		return nil, d.bad("'contents' must be a non-empty array")
	}
	var out []Message
	for ci, item := range arr {
		if item == nil {
			return nil, d.bad("'contents' must contain objects")
		}
		obj, rerr := gdecObject(item, map[string]bool{"role": true, "parts": true}, fmt.Sprintf("contents[%d]", ci))
		if rerr != nil {
			return nil, rerr
		}
		role := "user"
		if rawRole, ok := obj["role"]; ok && !isNull(rawRole) {
			if err := json.Unmarshal(rawRole, &role); err != nil {
				return nil, d.bad(fmt.Sprintf("'contents[%d].role' must be a string", ci))
			}
		}
		if role != "user" && role != "model" {
			return nil, d.bad(fmt.Sprintf("'contents[%d].role' must be 'user' or 'model'", ci))
		}
		rawParts, ok := obj["parts"]
		if !ok || isNull(rawParts) {
			return nil, d.bad(fmt.Sprintf("'contents[%d].parts' must be an array", ci))
		}
		var parts []map[string]json.RawMessage
		if err := json.Unmarshal(rawParts, &parts); err != nil {
			return nil, d.bad(fmt.Sprintf("'contents[%d].parts' must be an array", ci))
		}
		if role == "model" {
			m, rerr := d.modelTurn(ci, parts)
			if rerr != nil {
				return nil, rerr
			}
			if m != nil {
				out = append(out, *m)
			}
			continue
		}
		ms, rerr := d.userTurn(ci, parts)
		if rerr != nil {
			return nil, rerr
		}
		out = append(out, ms...)
	}
	return out, nil
}

// modelTurn decodes one model content. All text parts must precede all
// functionCall parts; interleaved text after a call is rejected rather than
// reordered into the IR split.
func (d *gdecDecoder) modelTurn(ci int, parts []map[string]json.RawMessage) (*Message, *RouterError) {
	m := &Message{Role: RoleAssistant}
	seenCall := false
	for pi, p := range parts {
		if p == nil {
			return nil, d.bad("'contents' parts must be objects")
		}
		path := fmt.Sprintf("contents[%d].parts[%d]", ci, pi)
		part, rerr := gdecObject(p, gdecPartKeys(), path)
		if rerr != nil {
			return nil, rerr
		}
		if rerr := gdecValidatePartShapes(path, part); rerr != nil {
			return nil, rerr
		}
		if v, ok := part["thought"]; ok && !isNull(v) {
			var b bool
			if err := json.Unmarshal(v, &b); err != nil {
				return nil, d.bad(fmt.Sprintf("'%s.thought' must be bool", path))
			}
			if b {
				d.lose(path+".thought", "thought summaries are provider-private reasoning state", false)
				continue
			}
		}
		if v, ok := part["thoughtSignature"]; ok && !isNull(v) {
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				return nil, d.bad(fmt.Sprintf("'%s.thoughtSignature' must be a string", path))
			}
			d.lose(path+".thoughtSignature", "thought signatures are provider-private reasoning state", false)
		}
		kind := gdecDataKind(part)
		if kind == "!" {
			return nil, d.bad(fmt.Sprintf("'%s' sets more than one part payload", path))
		}
		switch kind {
		case "text":
			var s string
			if err := json.Unmarshal(part["text"], &s); err != nil {
				return nil, d.bad(fmt.Sprintf("'%s.text' must be a string", path))
			}
			if seenCall && s != "" {
				return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
					Msg: fmt.Sprintf("'%s' interleaves text with function calls; mixed order requires a same-protocol route", path)}
			}
			if s != "" {
				m.Texts = append(m.Texts, TextContent{Text: s})
			}
		case "functionCall":
			seenCall = true
			tc, rerr := d.functionCall(path, part["functionCall"])
			if rerr != nil {
				return nil, rerr
			}
			for _, e := range m.ToolCalls {
				if e.ID == tc.ID {
					return nil, d.bad(fmt.Sprintf("'%s.functionCall.id' conflicts with another call identity", path))
				}
			}
			m.ToolCalls = append(m.ToolCalls, *tc)
			d.pending = append(d.pending, gdecPending{id: tc.ID, name: tc.Name})
		case "":
			// Metadata-only part (e.g. partMetadata) carries no translatable
			// payload.
			d.lose(path, "part metadata has no equivalent on this destination", false)
		case "inlineData":
			d.lose(path+".inlineData", "model-produced media has no reviewed bridge mapping", false)
		default:
			if rerr := d.loseDataPart(path, kind, part[kind]); rerr != nil {
				return nil, rerr
			}
		}
	}
	if len(m.Texts) == 0 && len(m.ToolCalls) == 0 {
		return nil, nil
	}
	return m, nil
}

// userTurn decodes one user content into tool messages plus at most one
// user message. Text/media and functionResponse in the same content are
// rejected rather than split in a guessed order. The typed blocks retain
// the input order for every supported media destination.
func (d *gdecDecoder) userTurn(ci int, parts []map[string]json.RawMessage) ([]Message, *RouterError) {
	var out []Message
	var sb strings.Builder
	var ordered []ContentBlock
	hasText := false
	hasMedia := false
	hasResponse := false
	for pi, p := range parts {
		if p == nil {
			return nil, d.bad("'contents' parts must be objects")
		}
		path := fmt.Sprintf("contents[%d].parts[%d]", ci, pi)
		part, rerr := gdecObject(p, gdecPartKeys(), path)
		if rerr != nil {
			return nil, rerr
		}
		if rerr := gdecValidatePartShapes(path, part); rerr != nil {
			return nil, rerr
		}
		if v, ok := part["thought"]; ok && !isNull(v) {
			var b bool
			if err := json.Unmarshal(v, &b); err != nil {
				return nil, d.bad(fmt.Sprintf("'%s.thought' must be bool", path))
			}
			if b {
				d.lose(path+".thought", "thought summaries are provider-private reasoning state", false)
				continue
			}
		}
		if v, ok := part["thoughtSignature"]; ok && !isNull(v) {
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				return nil, d.bad(fmt.Sprintf("'%s.thoughtSignature' must be a string", path))
			}
			d.lose(path+".thoughtSignature", "thought signatures are provider-private reasoning state", false)
		}
		kind := gdecDataKind(part)
		if kind == "!" {
			return nil, d.bad(fmt.Sprintf("'%s' sets more than one part payload", path))
		}
		switch kind {
		case "text":
			var s string
			if err := json.Unmarshal(part["text"], &s); err != nil {
				return nil, d.bad(fmt.Sprintf("'%s.text' must be a string", path))
			}
			if s != "" {
				hasText = true
				sb.WriteString(s)
				ordered = append(ordered, ContentBlock{Kind: ContentKindText, Text: s})
			}
		case "inlineData":
			block, named, filenameFallback, rerr := gdecValidateInlineMedia(path, part["inlineData"], d.lim)
			if rerr != nil {
				return nil, rerr
			}
			if block != nil && (block.Kind != ContentKindAudio || d.dest == geminiDestChat) {
				switch block.Kind {
				case ContentKindImage:
					if named {
						d.lose(path+".inlineData.displayName", "image display names have no destination equivalent", false)
					}
				case ContentKindAudio:
					if named {
						d.lose(path+".inlineData.displayName", "audio display names have no Chat equivalent", false)
					}
				case ContentKindFile:
					if filenameFallback {
						d.lose(path+".inlineData.displayName", "PDF display name cannot be preserved safely", false)
					}
					if d.dest == geminiDestAnthropic && (!named || filenameFallback) {
						block.File.Filename = ""
					}
				}
				hasMedia = true
				ordered = append(ordered, *block)
				continue
			}
			d.lose(path+".inlineData", "media input has no reviewed bridge mapping", false)
		case "functionResponse":
			hasResponse = true
			tool, rerr := d.functionResponse(path, part["functionResponse"])
			if rerr != nil {
				return nil, rerr
			}
			out = append(out, Message{Role: RoleTool, Results: []ToolResult{*tool}})
		case "":
			d.lose(path, "part metadata has no equivalent on this destination", false)
		default:
			if rerr := d.loseDataPart(path, kind, part[kind]); rerr != nil {
				return nil, rerr
			}
		}
	}
	if (hasText || hasMedia) && hasResponse {
		return nil, &RouterError{Status: 400, Code: CodeInvalidGeneration,
			Msg: fmt.Sprintf("'contents[%d]' mixes function responses with text or media; mixed order requires a same-protocol route", ci)}
	}
	if hasMedia {
		var texts []TextContent
		if sb.Len() > 0 {
			texts = []TextContent{{Text: sb.String()}}
		}
		out = append(out, Message{Role: RoleUser, Texts: texts, Blocks: ordered})
	} else if sb.Len() > 0 {
		out = append(out, Message{Role: RoleUser, Texts: []TextContent{{Text: sb.String()}}})
	}
	return out, nil
}

// loseDataPart validates a well-formed out-of-scope data part and records
// the diagnosed loss. User inline media is handled separately in userTurn.
func (d *gdecDecoder) loseDataPart(path, kind string, raw json.RawMessage) *RouterError {
	switch kind {
	case "fileData":
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
			return d.bad(fmt.Sprintf("'%s.%s' must be an object", path, kind))
		}
		d.lose(path+"."+kind, "media input has no reviewed bridge mapping", false)
		return nil
	case "executableCode", "codeExecutionResult", "toolCall", "toolResponse":
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
			return d.bad(fmt.Sprintf("'%s.%s' must be an object", path, kind))
		}
		return &RouterError{Status: 400, Code: CodeInvalidGeneration,
			Msg: fmt.Sprintf("'%s.%s' requires a same-protocol route", path, kind)}
	default:
		return &RouterError{Status: 400, Code: CodeInvalidGeneration,
			Msg: fmt.Sprintf("Unsupported or invalid request field for translation: %q", path+"."+kind)}
	}
}
