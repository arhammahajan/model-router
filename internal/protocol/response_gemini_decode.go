// Gemini source codec: one complete-response parser for the Gemini row.
//
// DecodeGeminiResponse validates a Gemini generateContent response ONCE,
// independently of destination, and returns the canonical Response. It
// replaces the source-validation halves of TranslateGeminiJSONToChat
// (g2cValidateResponseEnvelope, g2cPromptBlocked, g2cDecodeContent,
// g2cFinishReason, g2cParseUsage), TranslateGeminiJSONToAnthropic
// (g2a* counterparts) and TranslateGeminiJSONToResponses (g2r*
// counterparts). Destination differences (Anthropic input = prompt -
// cache, Responses details, Chat cached_tokens breakout) stay in the
// future target encoders via CheckRepresentable plus target-owned
// framing, never in a second parse of the source bytes.
//
// Four disagreements between the old paths are resolved here and pinned
// by response_gemini_ir_test.go (with executable probes of the old
// translators run before this file landed):
//  1. Role "assistant": accepted only by the Chat path; rejected by
//     Anthropic/Responses as a Chat alias (g2rCheckRole). Rejected here:
//     the reviewed contract names "model" as the only valid role.
//  2. Filter finish with calls: rendered only by the Chat path;
//     refused as contradictory by Anthropic/Responses. Refused here.
//  3. Whitespace-only modelVersion/responseId/createTime: accepted only
//     by the Chat path; rejected by Anthropic. Rejected here (opaque
//     identities are whitespace-invalid per validOpaqueID).
//  4. Malformed/absent candidates wording: "multiple choices" (Chat,
//     Responses) vs "invalid response" (Anthropic), all 502. Malformed
//     or absent reports "invalid response" here; a well-formed array
//     whose length is not 1 keeps "multiple choices".
//
// Identity policy (matches all three old paths, unified here):
// Gemini functionCall.id (optional) is preserved exactly when supplied;
// every id is a bounded nonempty opaque string (validateCallID),
// unique within the response, and missing/duplicate/conflicting
// identities fail. Tool-name grammar (validateToolName) applies to
// function NAMES only, never to call ids: names and ids stay separate.
// When the provider omits an id, the router mints a response-scoped
// compatibility id "gemini-compat-call-<n>" (first free suffix after
// reserving all supplied ids, so minting never collides). Minted ids
// are recorded in Response.RouterMintedCalls and never claimed to be
// provider-issued.
//
// Response identity: provider responseId verbatim when present
// (IDOrigin=provider), else the router-scoped constant
// geminiCompatResponseID (IDOrigin=router). Origin is explicit, never
// inferred from the prefix: a provider-supplied "gemini-compat-..."
// string keeps a provider origin and still echoes downstream, while a
// minted one is omitted by EncodeGeminiResponse.
//
// Model: modelVersion verbatim when present (ModelOrigin=provider),
// else "" with ModelOrigin=router (absence, never a fake provider
// string). Target encoders stamp the public model, so canonical Model
// never renders downstream; NewResponse allows empty only for Gemini
// to express this absence.
//
// Usage: promptTokenCount is already cache-inclusive per the REST
// contract, so canonical Prompt carries it verbatim and CacheRead
// carries the cached share (cache <= prompt). No target bucket math
// here. Total must equal prompt + candidates exactly with overflow
// checks. Nonzero thoughts/toolUse and nonempty *TokensDetails fail
// closed; explicit zeros are carried as present. trafficType must be a
// nonempty string when present.
//
// Ordering preserves upstream part order one-to-one (tool-first stays
// tool-first, text-call-text stays three items). Consecutive text runs
// coalesce; empty text parts contribute nothing. Missing/null
// functionCall.args defaults to {}.
//
// Streams are NOT migrated by this unit: the Gemini stream translators
// keep their own validation until the streaming row migrates.
package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Router-owned identity namespace for Gemini-source compatibility ids.
// The "gemini-compat-" prefix marks ids the router minted because the
// provider omitted them; it never collides with the old per-target
// prefixes (g2c-call-, g2a-call-, g2r-call-) and is never claimed to
// be provider-issued. Provenance is carried in Response.IDOrigin /
// ModelOrigin / RouterMintedCalls, never inferred from this prefix:
// a provider may legitimately supply the same string.
const geminiCompatCallPrefix = "gemini-compat-call-"

// geminiCompatResponseID is the canonical internal response id when the
// provider omits responseId. Constant and deterministic: one complete
// decode produces one generation, so no per-decode uniqueness is needed,
// and determinism keeps the decoder testable without normalization.
// It is router-scoped (Response.IDOrigin == router) and never echoed as
// a provider responseId (see EncodeGeminiResponse guard).
const geminiCompatResponseID = "gemini-compat-response"

// DecodeGeminiResponse parses one Gemini generateContent complete
// response into the canonical IR under the strict policy: unknown
// members, malformed envelopes, bad totals and universally unmapped
// nonzero accounting fail closed here for every target. Known
// target-divergent renderings (usage bucket math, finish wording,
// downstream id minting) stay in the target encoders behind
// CheckRepresentable.
func DecodeGeminiResponse(raw []byte, lim Limits) (Response, *RouterError) {
	return DecodeGeminiResponseWithPolicy(raw, lim, false)
}

// DecodeGeminiResponseWithPolicy parses one Gemini generateContent
// complete response with narrow lossy-extension policy. allowDrop keeps
// every strict failure identical; when true the decoder additionally
// accepts, after bounded validation, the routine provider/gateway
// response extensions that every target dialect drops:
//
//   - parts[*].thoughtSignature and functionCall.thoughtSignature:
//     opaque provider-private reasoning state (Gemini 4 attaches one to
//     ordinary text parts on every response), dropped exactly as the
//     request-side row drops signatures in history.
//   - usageMetadata.promptTokensDetails/candidatesTokensDetails/
//     cacheTokensDetails/toolUsePromptTokensDetails TEXT-modality
//     entries: pure breakdowns of totals already carried by the
//     authoritative scalars, so dropping them changes no accounting.
//   - usageMetadata.cost and usageMetadata.opencode gateway billing
//     members (validateGatewayBillingMetadata, shared with the
//     Chat/Anthropic/Responses rows).
//
// Malformed extensions and non-TEXT modality shares still fail closed
// in both modes: media-bearing accounting has no reviewed mapping, and
// validation never invents values. thought=true parts, media parts,
// server tools and nonzero thoughts/toolUse shares keep their strict
// failures unchanged.
func DecodeGeminiResponseWithPolicy(raw []byte, lim Limits, allowDrop bool) (Response, *RouterError) {
	bad := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	contradiction := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned a contradictory blocked response"}
	multi := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned multiple choices with no representation"}

	obj := map[string]json.RawMessage{}
	{
		var top map[string]json.RawMessage
		if err := json.Unmarshal(raw, &top); err != nil || top == nil {
			return Response{}, bad
		}
		unrepTop := func(what string) *RouterError {
			return &RouterError{Status: 502, Code: CodeUpstreamBad,
				Msg: fmt.Sprintf("Upstream response feature %q has no representation", what)}
		}
		for k, v := range top {
			switch k {
			case "candidates", "promptFeedback", "usageMetadata",
				"modelVersion", "responseId", "createTime":
				obj[k] = v
			default:
				// Unknown null members are accepted as newer
				// defaults; unknown non-null members fail closed.
				// (Null tolerance applies only at the envelope
				// level; nested objects reject unknown members
				// outright, exactly as in the old paths.)
				if isNull(v) {
					continue
				}
				return Response{}, unrepTop("response." + k)
			}
		}
	}

	model := ""
	modelOrigin := IdentityRouterMinted
	if rawMV, ok := obj["modelVersion"]; ok && !isNull(rawMV) {
		var s string
		if err := json.Unmarshal(rawMV, &s); err != nil || strings.TrimSpace(s) == "" {
			return Response{}, bad
		}
		model = s
		modelOrigin = IdentityProvider
	}
	respID := ResponseID(geminiCompatResponseID)
	idOrigin := IdentityRouterMinted
	if rawRID, ok := obj["responseId"]; ok && !isNull(rawRID) {
		var s string
		if err := json.Unmarshal(rawRID, &s); err != nil || !validOpaqueID(s) {
			return Response{}, bad
		}
		if len(s) > lim.MaxArgsChars {
			return Response{}, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream response exceeds the size bound"}
		}
		respID = ResponseID(s)
		idOrigin = IdentityProvider
	}
	if rawCT, ok := obj["createTime"]; ok && !isNull(rawCT) {
		var s string
		if err := json.Unmarshal(rawCT, &s); err != nil || strings.TrimSpace(s) == "" {
			return Response{}, bad
		}
	}

	// Prompt blocks never become an ordinary generation. Missing, null
	// and empty-array candidates are all coherent blocked shapes; any
	// populated candidates contradict the block.
	if rawPF, ok := obj["promptFeedback"]; ok && !isNull(rawPF) {
		blocked, reason, rerr := geminiDecodePromptBlocked(rawPF)
		if rerr != nil {
			return Response{}, rerr
		}
		if blocked {
			rawCands, present := obj["candidates"]
			_, absent, malformed := geminiNormalizeCandidates(rawCands, present)
			if malformed {
				return Response{}, bad
			}
			if !absent {
				return Response{}, contradiction
			}
			rawUsage, ok := obj["usageMetadata"]
			if !ok || isNull(rawUsage) {
				return Response{}, bad
			}
			usage, rerr := geminiParseUsageWithPolicy(rawUsage, allowDrop)
			if rerr != nil {
				return Response{}, rerr
			}
			resp, rerr := NewResponse(respID, model, DialectGemini, nil, usage, Finish{Kind: FinishContentFilter, Raw: reason})
			if rerr != nil {
				return Response{}, rerr
			}
			resp.IDOrigin = idOrigin
			resp.ModelOrigin = modelOrigin
			return resp, nil
		}
	}

	rawCands, present := obj["candidates"]
	cands, absent, malformed := geminiNormalizeCandidates(rawCands, present)
	if malformed || absent {
		return Response{}, bad
	}
	if len(cands) != 1 {
		return Response{}, multi
	}
	cand, rerr := geminiResponseObject(cands[0], map[string]bool{
		"content": true, "finishReason": true, "finishMessage": true,
		"safetyRatings": true, "citationMetadata": true, "groundingMetadata": true,
		"avgLogprobs": true, "logprobsResult": true, "index": true,
		"tokenCountMetadata": true, "continuationToken": true,
	}, "response.candidates[0]")
	if rerr != nil {
		return Response{}, rerr
	}
	if rawIdx, ok := cand["index"]; ok && !isNull(rawIdx) {
		idx, rerr := responseInt(rawIdx)
		if rerr != nil || idx != 0 {
			return Response{}, bad
		}
	}
	// Safety/citation/grounding/logprobs payloads have no mapping.
	// Documented empty/absent members are harmless defaults; any
	// populated semantic content fails closed rather than stripped.
	if rawSR, ok := cand["safetyRatings"]; ok && !isNull(rawSR) {
		if rerr := geminiAcceptEmptyRatings(rawSR); rerr != nil {
			return Response{}, rerr
		}
	}
	unrep := func(what string) *RouterError {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: fmt.Sprintf("Upstream response feature %q has no representation", what)}
	}
	for _, k := range []string{"citationMetadata", "groundingMetadata", "logprobsResult", "tokenCountMetadata"} {
		if rawV, ok := cand[k]; ok && !isNull(rawV) {
			return Response{}, unrep("response.candidates[0]." + k)
		}
	}
	if rawLP, ok := cand["avgLogprobs"]; ok && !isNull(rawLP) {
		var f float64
		if err := json.Unmarshal(rawLP, &f); err != nil {
			return Response{}, bad
		}
		if f != 0 {
			return Response{}, unrep("response.candidates[0].avgLogprobs")
		}
	}
	if rawCT, ok := cand["continuationToken"]; ok && !isNull(rawCT) {
		return Response{}, unrep("response.candidates[0].continuationToken")
	}
	if rawFM, ok := cand["finishMessage"]; ok && !isNull(rawFM) {
		var s string
		if err := json.Unmarshal(rawFM, &s); err != nil || len(s) > lim.MaxArgsChars {
			return Response{}, bad
		}
		// Gemini 4 attaches an informational finishMessage to tool-call
		// finishes ("Model generated function call(s)."). No target
		// dialect renders it; strict fails closed, lossy validates the
		// bounded string and drops it.
		if s != "" && !allowDrop {
			return Response{}, unrep("response.candidates[0].finishMessage")
		}
	}
	rawFinish, ok := cand["finishReason"]
	if !ok || isNull(rawFinish) {
		// An empty finish reason means the model has not stopped; a
		// complete response without one is invalid.
		return Response{}, bad
	}
	var finish string
	if err := json.Unmarshal(rawFinish, &finish); err != nil {
		return Response{}, bad
	}
	rawContent, ok := cand["content"]
	if !ok || isNull(rawContent) {
		// Safety-filtered candidates carry empty content with a filter
		// finish reason; otherwise content is required.
		if g2cIsFilterFinish(finish) {
			rawContent = json.RawMessage(`{"parts":[]}`)
		} else {
			return Response{}, bad
		}
	}
	items, mintedCalls, rerr := geminiDecodeContent(rawContent, lim, allowDrop)
	if rerr != nil {
		return Response{}, rerr
	}
	hasCalls := false
	for _, it := range items {
		if _, ok := it.(ToolCallItem); ok {
			hasCalls = true
			break
		}
	}
	irFinish, rerr := geminiDecodeFinish(finish, hasCalls)
	if rerr != nil {
		return Response{}, rerr
	}
	rawUsage, ok := obj["usageMetadata"]
	if !ok || isNull(rawUsage) {
		return Response{}, bad
	}
	usage, rerr := geminiParseUsageWithPolicy(rawUsage, allowDrop)
	if rerr != nil {
		return Response{}, rerr
	}
	resp, rerr := NewResponse(respID, model, DialectGemini, items, usage, irFinish)
	if rerr != nil {
		return Response{}, rerr
	}
	resp.IDOrigin = idOrigin
	resp.ModelOrigin = modelOrigin
	resp.RouterMintedCalls = mintedCalls
	return resp, nil
}

// geminiResponseObject decodes a JSON object, rejecting non-objects and
// any member outside the allowed set. `what` is a static value-free
// field path. This is the Gemini-source counterpart of chatResponseObject
// and strictObject: one shared neutral validator with
// destination-independent wording, so the three former per-target
// copies (whose diagnostics each named their own destination) cannot
// drift. Unknown null members are accepted as newer defaults; unknown
// non-null members fail closed.
func geminiResponseObject(raw json.RawMessage, allowed map[string]bool, what string) (map[string]json.RawMessage, *RouterError) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, k := range keys {
		if !allowed[k] {
			return nil, &RouterError{Status: 502, Code: CodeUpstreamBad,
				Msg: fmt.Sprintf("Upstream response feature %q has no representation", what+"."+k)}
		}
	}
	return obj, nil
}

// geminiDecodeFinish maps a Gemini finish reason onto the closed IR
// Finish. STOP with calls is a tool-calls finish (the Chat target
// renders tool_calls, the Anthropic target tool_use, the Responses
// target completed-with-calls); a safety-family reason with calls is
// a contradiction (see the package doc). Anything else without a
// reviewed mapping fails closed and never becomes a Finish.
func geminiDecodeFinish(finish string, hasCalls bool) (Finish, *RouterError) {
	switch {
	case finish == "STOP" && hasCalls:
		return Finish{Kind: FinishToolCalls, Raw: finish}, nil
	case finish == "STOP":
		return Finish{Kind: FinishStop, Raw: finish}, nil
	case finish == "MAX_TOKENS":
		return Finish{Kind: FinishLength, Raw: finish}, nil
	case g2cIsFilterFinish(finish):
		if hasCalls {
			return Finish{}, &RouterError{Status: 502, Code: CodeUpstreamBad,
				Msg: "Upstream returned a contradictory blocked response"}
		}
		return Finish{Kind: FinishContentFilter, Raw: finish}, nil
	default:
		return Finish{}, &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream stop reason has no representation"}
	}
}

// geminiDecodePromptBlocked reports whether promptFeedback carries a
// blocking reason, returning the reason for Raw-finish provenance.
// UNSPECIFIED/absent/null means not blocked. The blocked set matches
// all three old paths (g2cPromptBlocked, g2aPromptBlocked, and the
// Responses path which reuses g2cPromptBlocked); unknown reasons fail
// closed on every path. safetyRatings here needs only array shape, as
// in all three old paths.
func geminiDecodePromptBlocked(raw json.RawMessage) (bool, string, *RouterError) {
	bad := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	pf, rerr := geminiResponseObject(raw, map[string]bool{
		"blockReason": true, "safetyRatings": true,
	}, "response.promptFeedback")
	if rerr != nil {
		return false, "", rerr
	}
	if rawSR, ok := pf["safetyRatings"]; ok && !isNull(rawSR) {
		var arr []json.RawMessage
		if err := json.Unmarshal(rawSR, &arr); err != nil {
			return false, "", bad
		}
	}
	rawBR, ok := pf["blockReason"]
	if !ok || isNull(rawBR) {
		return false, "", nil
	}
	var reason string
	if err := json.Unmarshal(rawBR, &reason); err != nil {
		return false, "", bad
	}
	switch reason {
	case "", "BLOCK_REASON_UNSPECIFIED", "BLOCK_REASON_UNKNOWN":
		return false, "", nil
	case "SAFETY", "OTHER", "BLOCKLIST", "PROHIBITED_CONTENT", "IMAGE_SAFETY":
		return true, reason, nil
	default:
		return false, "", &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: fmt.Sprintf("Upstream stop reason %q has no representation", "response.promptFeedback.blockReason")}
	}
}

// geminiDecodeContent validates one candidate content into ordered
// items preserving upstream part order: tool-first stays tool-first
// and text-call-text stays three items. Only text and complete
// functionCall parts are supported; thought parts, media, executable
// code, server tools, function responses and partial-arg streaming
// all fail closed in both modes, exactly as in all three old paths.
// allowDrop (lossy) additionally accepts parts[*].thoughtSignature —
// opaque provider-private reasoning state that no target dialect can
// carry — after bounded string validation, dropping it exactly as the
// request-side row drops signatures in history. Consecutive text
// parts coalesce into one TextItem; empty text parts contribute
// nothing and create no item. Item ids are router-minted position
// identities ("t<n>" for text runs, "c<n>" for calls, matching the
// Chat decoder convention); call ids follow the package identity
// policy. The returned minted set records call IDs the router minted
// (provider omitted); supplied IDs — even ones colliding with the
// compat prefix — are never added.
func geminiDecodeContent(raw json.RawMessage, lim Limits, allowDrop bool) ([]ResponseItem, map[CallID]struct{}, *RouterError) {
	bad := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	unrep := func(what string) *RouterError {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: fmt.Sprintf("Upstream response feature %q has no representation", what)}
	}
	const prefix = "response.candidates[0].content"
	content, rerr := geminiResponseObject(raw, map[string]bool{
		"role": true, "parts": true,
	}, prefix)
	if rerr != nil {
		return nil, nil, rerr
	}
	if rawRole, ok := content["role"]; ok && !isNull(rawRole) {
		var role string
		if err := json.Unmarshal(rawRole, &role); err != nil || role != "model" {
			// The Chat alias "assistant" is rejected here even though
			// the old Chat path accepted it (see the package doc).
			return nil, nil, bad
		}
	}
	rawParts, ok := content["parts"]
	if !ok || isNull(rawParts) {
		return nil, nil, bad
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(rawParts, &parts); err != nil || parts == nil {
		return nil, nil, bad
	}
	// Reserve every provider-supplied id before minting compatibility
	// ids, so a minted id never collides with a supplied one regardless
	// of part order. Duplicates are still diagnosed in the main loop
	// below; reservation only steers minting. (All three old paths.)
	reserved := map[string]bool{}
	for _, praw := range parts {
		var peek map[string]json.RawMessage
		if err := json.Unmarshal(praw, &peek); err != nil {
			continue // The main loop reports malformed parts.
		}
		rawFC, ok := peek["functionCall"]
		if !ok || isNull(rawFC) {
			continue
		}
		var fcPeek map[string]json.RawMessage
		if err := json.Unmarshal(rawFC, &fcPeek); err != nil {
			continue
		}
		rawID, ok := fcPeek["id"]
		if !ok || isNull(rawID) {
			continue
		}
		var id string
		if err := json.Unmarshal(rawID, &id); err != nil {
			continue
		}
		reserved[id] = true
	}
	seenIDs := map[string]bool{}
	var mintedCalls map[CallID]struct{}
	nextCompat := 0
	mintID := func() string {
		for {
			id := fmt.Sprintf("%s%d", geminiCompatCallPrefix, nextCompat)
			nextCompat++
			if !seenIDs[id] && !reserved[id] {
				return id
			}
		}
	}
	var items []ResponseItem
	textCount, callCount, totalArgs := 0, 0, 0
	appendText := func(s string) {
		if n := len(items); n > 0 {
			if tx, ok := items[n-1].(TextItem); ok {
				tx.Text += s
				items[n-1] = tx
				return
			}
		}
		items = append(items, TextItem{ID: ItemID(fmt.Sprintf("t%d", textCount)), Text: s})
		textCount++
	}
	for pi, praw := range parts {
		part, rerr := geminiResponseObject(praw, map[string]bool{
			"text": true, "functionCall": true, "functionResponse": true,
			"thought": true, "thoughtSignature": true, "inlineData": true,
			"fileData": true, "executableCode": true, "codeExecutionResult": true,
			"toolCall": true, "toolResponse": true, "mediaResolution": true,
			"videoMetadata": true, "partMetadata": true, "mediaProcessing": true,
		}, fmt.Sprintf("%s.parts[%d]", prefix, pi))
		if rerr != nil {
			return nil, nil, rerr
		}
		// Part.data is a oneof union: at most one content payload may
		// be set per part. Multiple payloads are malformed upstream
		// data. (All three old paths.)
		payloads := 0
		for _, k := range []string{"text", "inlineData", "fileData", "functionCall",
			"functionResponse", "executableCode", "codeExecutionResult"} {
			if rawV, ok := part[k]; ok && !isNull(rawV) {
				payloads++
			}
		}
		if payloads > 1 {
			return nil, nil, bad
		}
		// Thought parts require a reviewed mapping; never stripped,
		// never merged into ordinary text.
		if rawT, ok := part["thought"]; ok && !isNull(rawT) {
			var b bool
			if err := json.Unmarshal(rawT, &b); err != nil {
				return nil, nil, bad
			}
			if b {
				return nil, nil, unrep(fmt.Sprintf("%s.parts[%d].thought", prefix, pi))
			}
		}
		// thoughtSignature is opaque provider-private reasoning state:
		// Gemini 4 attaches one to ordinary text parts on every
		// response. Strict fails closed; lossy validates a bounded
		// string. On functionCall parts the signature is captured and
		// wrapped into the public call identity (see the functionCall
		// branch); on text parts it is dropped, exactly as the
		// request-side row drops signatures in conversation history.
		partSig := ""
		if rawTS, ok := part["thoughtSignature"]; ok && !isNull(rawTS) {
			var sig string
			if err := json.Unmarshal(rawTS, &sig); err != nil || len(sig) > lim.MaxArgsChars {
				return nil, nil, bad
			}
			if !allowDrop {
				return nil, nil, unrep(fmt.Sprintf("%s.parts[%d].thoughtSignature", prefix, pi))
			}
			partSig = sig
		}
		for _, k := range []string{"inlineData", "fileData", "executableCode",
			"codeExecutionResult", "toolCall", "toolResponse", "mediaResolution", "videoMetadata",
			"partMetadata", "mediaProcessing", "functionResponse"} {
			if rawV, ok := part[k]; ok && !isNull(rawV) {
				return nil, nil, unrep(fmt.Sprintf("%s.parts[%d].%s", prefix, pi, k))
			}
		}
		if rawText, ok := part["text"]; ok && !isNull(rawText) {
			var s string
			if err := json.Unmarshal(rawText, &s); err != nil {
				return nil, nil, bad
			}
			// Empty text parts carry no characters and create no item;
			// nonempty runs coalesce so text-call-text keeps three
			// items instead of collapsing to text-before-call.
			if s != "" {
				appendText(s)
			}
		}
		if rawFC, ok := part["functionCall"]; ok && !isNull(rawFC) {
			name, id, hasID, args, fcSig, rerr := geminiParseFunctionCall(rawFC, pi, lim, prefix, allowDrop)
			if rerr != nil {
				return nil, nil, rerr
			}
			minted := false
			if !hasID {
				id = mintID()
				minted = true
			}
			if seenIDs[id] {
				return nil, nil, &RouterError{Status: 502, Code: CodeUpstreamBad,
					Msg: "Upstream returned conflicting tool call identities"}
			}
			seenIDs[id] = true
			if minted {
				if mintedCalls == nil {
					mintedCalls = map[CallID]struct{}{}
				}
				mintedCalls[CallID(id)] = struct{}{}
			}
			// Lossy signature passback: mint the public identity that
			// carries the opaque signature (part-level wins over a
			// functionCall-level value when both appear), so harnesses
			// echo it back and the Gemini encoder restores the signed
			// part. The provider identity stays intact inside the
			// wrapped form.
			if allowDrop {
				sig := partSig
				if sig == "" {
					sig = fcSig
				}
				id = wrapGeminiSignatureID(sig, id)
			}
			totalArgs += len(args)
			if totalArgs > lim.MaxStreamArgsChars {
				return nil, nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call exceeds the size bound"}
			}
			item, rerr := NewToolCallItem(ItemID(fmt.Sprintf("c%d", callCount)), CallID(id), name, args)
			if rerr != nil {
				return nil, nil, rerr
			}
			callCount++
			items = append(items, item)
		}
	}
	if callCount > lim.MaxToolCalls {
		return nil, nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call count exceeds the size bound"}
	}
	return items, mintedCalls, nil
}

// geminiParseFunctionCall validates one complete functionCall part.
// Partial-arg streaming and willContinue fail closed on every old path.
// thoughtSignature — opaque provider reasoning state attached to tool
// calls by Gemini 4 — fails closed when allowDrop is false; when true
// it is validated and returned so the caller mints a wrapped public
// identity (gemini_signature_id.go) that carries it back on the next
// turn. Missing args default to the empty object. The returned id is ""
// when the provider omitted it; callers mint a response-scoped
// compatibility id. Names carry the tool-name grammar; ids are opaque
// and bounded (validateCallID), never grammar-checked.
func geminiParseFunctionCall(raw json.RawMessage, pi int, lim Limits, prefix string, allowDrop bool) (name, id string, hasID bool, args json.RawMessage, sig string, rerr *RouterError) {
	bad := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	unrep := func(what string) *RouterError {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: fmt.Sprintf("Upstream response feature %q has no representation", what)}
	}
	what := fmt.Sprintf("%s.parts[%d].functionCall", prefix, pi)
	fc, rerr := geminiResponseObject(raw, map[string]bool{
		"name": true, "args": true, "id": true,
		"partialArgs": true, "willContinue": true, "thoughtSignature": true,
	}, what)
	if rerr != nil {
		return "", "", false, nil, "", rerr
	}
	if rawPA, ok := fc["partialArgs"]; ok && !isNull(rawPA) {
		var arr []json.RawMessage
		if err := json.Unmarshal(rawPA, &arr); err != nil {
			return "", "", false, nil, "", bad
		}
		if len(arr) > 0 {
			return "", "", false, nil, "", unrep(what + ".partialArgs")
		}
	}
	if rawWC, ok := fc["willContinue"]; ok && !isNull(rawWC) {
		var b bool
		if err := json.Unmarshal(rawWC, &b); err != nil {
			return "", "", false, nil, "", bad
		}
		if b {
			return "", "", false, nil, "", unrep(what + ".willContinue")
		}
	}
	var fcSig string
	if rawTS, ok := fc["thoughtSignature"]; ok && !isNull(rawTS) {
		var sig string
		if err := json.Unmarshal(rawTS, &sig); err != nil || len(sig) > lim.MaxArgsChars {
			return "", "", false, nil, "", bad
		}
		if !allowDrop {
			return "", "", false, nil, "", unrep(what + ".thoughtSignature")
		}
		// Lossy: validated and returned so the caller can wrap it into
		// the public identity; wrapGeminiSignatureID refuses oversized
		// values and the caller keeps the plain identity then.
		fcSig = sig
	}
	var n string
	if rawN, ok := fc["name"]; !ok || json.Unmarshal(rawN, &n) != nil || strings.TrimSpace(n) == "" {
		return "", "", false, nil, "", bad
	}
	if err := validateToolName(n); err != nil {
		return "", "", false, nil, "", bad
	}
	var callID string
	var present bool
	if rawID, ok := fc["id"]; ok && !isNull(rawID) {
		var s string
		if err := json.Unmarshal(rawID, &s); err != nil || strings.TrimSpace(s) == "" {
			return "", "", false, nil, "", bad
		}
		if err := validateCallID(s, lim); err != nil {
			return "", "", false, nil, "", bad
		}
		callID, present = s, true
	}
	out := json.RawMessage("{}")
	if rawArgs, ok := fc["args"]; ok && !isNull(rawArgs) {
		compacted, rerr := g2cCompactArgsObject(what+".args", rawArgs, lim)
		if rerr != nil {
			return "", "", false, nil, "", rerr
		}
		out = compacted
	}
	return n, callID, present, out, fcSig, nil
}
