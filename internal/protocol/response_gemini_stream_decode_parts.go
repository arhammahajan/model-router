// Gemini source stream codec: content parts (split for the 700-line module bound).
//
// Companion to response_gemini_stream_decode.go, which owns framing,
// envelope, identity, start, and terminal arming. This file owns
// candidate/content/part validation plus text-run and tool-call emission
// (BlockOpen/ArgsFragment, minting, bounds). No behavior lives here that
// the header file does not describe.
package protocol

import (
	"encoding/json"
	"fmt"
)

func (d *GeminiStreamDecoder) handleCandidate(raw json.RawMessage) ([]ResponseEvent, string, bool, *RouterError) {
	cand, rerr := geminiResponseObject(raw, map[string]bool{
		"content": true, "finishReason": true, "finishMessage": true,
		"safetyRatings": true, "citationMetadata": true, "groundingMetadata": true,
		"avgLogprobs": true, "logprobsResult": true, "index": true,
		"tokenCountMetadata": true, "continuationToken": true,
	}, "response.candidates[0]")
	if rerr != nil {
		return nil, "", false, rerr
	}
	if rawIdx, ok := cand["index"]; ok && !isNull(rawIdx) {
		idx, rerr := responseInt(rawIdx)
		if rerr != nil || idx != 0 {
			return nil, "", false, geminiStreamBad()
		}
	}
	if rawSR, ok := cand["safetyRatings"]; ok && !isNull(rawSR) {
		if rerr := geminiAcceptEmptyRatings(rawSR); rerr != nil {
			return nil, "", false, rerr
		}
	}
	for _, k := range []string{"citationMetadata", "groundingMetadata", "logprobsResult", "tokenCountMetadata"} {
		if rawV, ok := cand[k]; ok && !isNull(rawV) {
			return nil, "", false, geminiStreamUnrep("response.candidates[0]." + k)
		}
	}
	if rawLP, ok := cand["avgLogprobs"]; ok && !isNull(rawLP) {
		var f float64
		if err := json.Unmarshal(rawLP, &f); err != nil {
			return nil, "", false, geminiStreamBad()
		}
		if f != 0 {
			return nil, "", false, geminiStreamUnrep("response.candidates[0].avgLogprobs")
		}
	}
	if rawCT, ok := cand["continuationToken"]; ok && !isNull(rawCT) {
		return nil, "", false, geminiStreamUnrep("response.candidates[0].continuationToken")
	}
	if rawFM, ok := cand["finishMessage"]; ok && !isNull(rawFM) {
		var s string
		if err := json.Unmarshal(rawFM, &s); err != nil {
			return nil, "", false, geminiStreamBad()
		}
		if s != "" {
			return nil, "", false, geminiStreamUnrep("response.candidates[0].finishMessage")
		}
	}
	var finish string
	hasFinish := false
	if rawF, ok := cand["finishReason"]; ok && !isNull(rawF) {
		var f string
		if err := json.Unmarshal(rawF, &f); err != nil {
			return nil, "", false, geminiStreamBad()
		}
		finish, hasFinish = f, true
	}
	rawContent, ok := cand["content"]
	if !ok || isNull(rawContent) {
		if hasFinish && g2cIsFilterFinish(finish) {
			rawContent = json.RawMessage(`{"parts":[]}`)
		} else {
			return nil, "", false, geminiStreamBad()
		}
	}
	evs, rerr := d.handleContent(rawContent)
	if rerr != nil {
		return nil, "", false, rerr
	}
	return evs, finish, hasFinish, nil
}

func (d *GeminiStreamDecoder) handleContent(raw json.RawMessage) ([]ResponseEvent, *RouterError) {
	content, rerr := geminiResponseObject(raw, map[string]bool{
		"role": true, "parts": true,
	}, "response.candidates[0].content")
	if rerr != nil {
		return nil, rerr
	}
	if rawRole, ok := content["role"]; ok && !isNull(rawRole) {
		var role string
		if err := json.Unmarshal(rawRole, &role); err != nil || role != "model" {
			return nil, geminiStreamBad()
		}
	}
	rawParts, ok := content["parts"]
	if !ok || isNull(rawParts) {
		return nil, geminiStreamBad()
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(rawParts, &parts); err != nil || parts == nil {
		return nil, geminiStreamBad()
	}
	var evs []ResponseEvent
	for pi, praw := range parts {
		partEvs, rerr := d.handlePart(praw, pi)
		if rerr != nil {
			return nil, rerr
		}
		evs = append(evs, partEvs...)
	}
	return evs, nil
}

func (d *GeminiStreamDecoder) handlePart(praw json.RawMessage, pi int) ([]ResponseEvent, *RouterError) {
	prefix := fmt.Sprintf("response.candidates[0].content.parts[%d]", pi)
	part, rerr := geminiResponseObject(praw, map[string]bool{
		"text": true, "functionCall": true, "functionResponse": true,
		"thought": true, "thoughtSignature": true, "inlineData": true,
		"fileData": true, "executableCode": true, "codeExecutionResult": true,
		"toolCall": true, "toolResponse": true, "mediaResolution": true,
		"videoMetadata": true, "partMetadata": true, "mediaProcessing": true,
	}, prefix)
	if rerr != nil {
		return nil, rerr
	}
	payloads := 0
	for _, k := range []string{"text", "inlineData", "fileData", "functionCall", "functionResponse", "executableCode", "codeExecutionResult"} {
		if rawV, ok := part[k]; ok && !isNull(rawV) {
			payloads++
		}
	}
	if payloads > 1 {
		return nil, geminiStreamBad()
	}
	if rawT, ok := part["thought"]; ok && !isNull(rawT) {
		var b bool
		if err := json.Unmarshal(rawT, &b); err != nil {
			return nil, geminiStreamBad()
		}
		if b {
			return nil, geminiStreamUnrep(prefix + ".thought")
		}
	}
	for _, k := range []string{"thoughtSignature", "inlineData", "fileData", "executableCode", "codeExecutionResult", "toolCall", "toolResponse", "mediaResolution", "videoMetadata", "partMetadata", "mediaProcessing", "functionResponse"} {
		if rawV, ok := part[k]; ok && !isNull(rawV) {
			return nil, geminiStreamUnrep(prefix + "." + k)
		}
	}
	var evs []ResponseEvent
	if rawText, ok := part["text"]; ok && !isNull(rawText) {
		var s string
		if err := json.Unmarshal(rawText, &s); err != nil {
			return nil, geminiStreamBad()
		}
		if s != "" {
			fevs, rerr := d.emitText(s)
			if rerr != nil {
				return nil, rerr
			}
			evs = append(evs, fevs...)
		}
	}
	if rawFC, ok := part["functionCall"]; ok && !isNull(rawFC) {
		fevs, rerr := d.emitTool(rawFC, pi, prefix)
		if rerr != nil {
			return nil, rerr
		}
		evs = append(evs, fevs...)
	}
	return evs, nil
}

func (d *GeminiStreamDecoder) emitText(s string) ([]ResponseEvent, *RouterError) {
	var evs []ResponseEvent
	if !d.hasText {
		if d.blocks+1 > d.lim.MaxStreamBlocks {
			return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream exceeds the content block bound"}
		}
		item := ItemID(fmt.Sprintf("t%d", d.textCount))
		open, rerr := NewBlockOpen(item, BlockText, "", "")
		if rerr != nil {
			return nil, rerr
		}
		evs = append(evs, open)
		d.order = append(d.order, item)
		d.textCount++
		d.blocks++
		d.curText = item
		d.hasText = true
	}
	frag, rerr := NewTextFragment(d.curText, s)
	if rerr != nil {
		return nil, rerr
	}
	return append(evs, frag), nil
}

func (d *GeminiStreamDecoder) emitTool(rawFC json.RawMessage, pi int, prefix string) ([]ResponseEvent, *RouterError) {
	name, id, hasID, args, rerr := geminiParseFunctionCall(rawFC, pi, d.lim, "response.candidates[0].content")
	if rerr != nil {
		return nil, rerr
	}
	_ = prefix
	callID := id
	callOrigin := IdentityProvider
	if !hasID {
		callID = d.mintCallID()
		callOrigin = IdentityRouterMinted
	}
	if d.seenCalls[callID] {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned conflicting tool call identities"}
	}
	d.seenCalls[callID] = true
	if callOrigin == IdentityRouterMinted {
		d.minted[CallID(callID)] = struct{}{}
	}
	if len(args) > d.lim.MaxArgsChars || d.totalArgs+len(args) > d.lim.MaxStreamArgsChars {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call exceeds the size bound"}
	}
	if d.toolCount+1 > d.lim.MaxToolCalls {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call count exceeds the size bound"}
	}
	if d.blocks+1 > d.lim.MaxStreamBlocks {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream exceeds the content block bound"}
	}
	item := ItemID(fmt.Sprintf("c%d", d.callCount))
	open, rerr := NewBlockOpen(item, BlockTool, CallID(callID), name)
	if rerr != nil {
		return nil, rerr
	}
	open.CallOrigin = callOrigin
	d.order = append(d.order, item)
	d.callCount++
	d.toolCount++
	d.blocks++
	d.totalArgs += len(args)
	d.hasText = false
	d.curText = ""
	frag, rerr := NewArgsFragment(item, string(args))
	if rerr != nil {
		return nil, rerr
	}
	return []ResponseEvent{open, frag}, nil
}

func (d *GeminiStreamDecoder) mintCallID() string {
	for {
		id := fmt.Sprintf("%s%d", geminiCompatCallPrefix, d.nextMint)
		d.nextMint++
		if !d.seenCalls[id] {
			return id
		}
	}
}
