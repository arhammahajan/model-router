// Anthropic source stream codec: content-block and terminal handlers.
//
// This file owns the block lifecycle of the Anthropic source decoder
// (content_block_start/delta/stop, message_delta/message_stop, ping):
// ordered unique blocks, immediate fragment forwarding, seeded content,
// the canonical "{}" fallback, MalformedToolEvent classification, and
// the single authoritative terminal. Start validation, usage parsing,
// and the DecodeFrame/Close seam live in
// response_anthropic_stream_decode.go. No downstream writes, no reader
// ownership, no goroutines, no destination policy.
package protocol

import (
	"encoding/json"
	"strings"
)

// handleBlockStart validates one content_block_start and emits its BlockOpen
// plus any seeded content. Seeded text and non-"{}" tool input emit
// immediately; a "{}" tool input is the placeholder for deltas to come and
// seeds nothing.
func (d *AnthropicStreamDecoder) handleBlockStart(data string) ([]ResponseEvent, *RouterError) {
	if rerr := d.requireStarted(); rerr != nil {
		return nil, rerr
	}
	if strings.TrimSpace(data) == "" {
		return nil, anthropicStreamBad()
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &body); err != nil || body == nil {
		return nil, anthropicStreamBad()
	}
	if _, rerr := chatResponseObject(mustRaw(body), map[string]bool{"type": true, "index": true, "content_block": true}, "content_block_start"); rerr != nil {
		return nil, rerr
	}
	if rerr := anthropicStreamCheckType(body, "content_block_start"); rerr != nil {
		return nil, rerr
	}
	rawIdx, ok := body["index"]
	if !ok {
		return nil, anthropicStreamBad()
	}
	idx, rerr := streamIndex(rawIdx)
	if rerr != nil {
		return nil, anthropicStreamBad()
	}
	if d.seenIdx[idx] {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream reused a content block index"}
	}
	if len(d.seenIdx)+1 > d.lim.MaxStreamBlocks {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream exceeds the content block bound"}
	}
	rawCB, ok := body["content_block"]
	if !ok || isNull(rawCB) {
		return nil, anthropicStreamBad()
	}
	cb, rerr := chatResponseObject(rawCB, map[string]bool{
		"type": true, "id": true, "name": true, "input": true, "text": true, "citations": true,
	}, "content_block_start.content_block")
	if rerr != nil {
		return nil, rerr
	}
	rawType, ok := cb["type"]
	if !ok || isNull(rawType) {
		return nil, anthropicStreamBad()
	}
	var cbType string
	if err := json.Unmarshal(rawType, &cbType); err != nil || cbType == "" {
		return nil, anthropicStreamBad()
	}
	switch cbType {
	case "text":
		return d.openTextBlock(cb, idx)
	case "tool_use":
		return d.openToolBlock(cb, idx)
	default:
		// Thinking, signatures, media, server/hosted tools and unknown
		// kinds have no IR form: fail closed, never dropped. The kind
		// value itself is upstream content and stays out of diagnostics.
		return nil, anthropicStreamUnrep("content_block_start.content_block.type")
	}
}

// openTextBlock opens one text block, emitting seeded text immediately.
// Cross-kind members (id/name/input) fail closed per the closed per-kind
// contract; a non-empty citations array has no representation.
func (d *AnthropicStreamDecoder) openTextBlock(cb map[string]json.RawMessage, idx int) ([]ResponseEvent, *RouterError) {
	for _, k := range []string{"id", "name", "input"} {
		if rawV, ok := cb[k]; ok && !isNull(rawV) {
			return nil, anthropicStreamBad()
		}
	}
	initial := ""
	if rawText, ok := cb["text"]; ok && !isNull(rawText) {
		var text string
		if err := json.Unmarshal(rawText, &text); err != nil {
			return nil, anthropicStreamBad()
		}
		initial = text
	}
	if rawCit, ok := cb["citations"]; ok && !isNull(rawCit) {
		var cits []json.RawMessage
		if err := json.Unmarshal(rawCit, &cits); err != nil || len(cits) > 0 {
			return nil, anthropicStreamUnrep("content_block_start.content_block.citations")
		}
	}
	item := d.nextItemID()
	open, rerr := NewIndexedBlockOpen(item, BlockText, "", "", idx)
	if rerr != nil {
		return nil, rerr
	}
	d.seenIdx[idx] = true
	d.blocks[idx] = &anthropicStreamBlock{kind: BlockText, item: item}
	evs := []ResponseEvent{open}
	if initial != "" {
		frag, rerr := NewTextFragment(item, initial)
		if rerr != nil {
			return nil, rerr
		}
		evs = append(evs, frag)
	}
	return evs, nil
}

// openToolBlock opens one tool block once its complete identity is known.
// Call IDs are opaque handles (never the tool-name grammar); names stay
// nonempty and bounded here while their grammar remains a target gate.
func (d *AnthropicStreamDecoder) openToolBlock(cb map[string]json.RawMessage, idx int) ([]ResponseEvent, *RouterError) {
	for _, k := range []string{"text", "citations"} {
		if rawV, ok := cb[k]; ok && !isNull(rawV) {
			return nil, anthropicStreamBad()
		}
	}
	rawID, okID := cb["id"]
	rawName, okName := cb["name"]
	if !okID || !okName || isNull(rawID) || isNull(rawName) {
		return nil, anthropicStreamBad()
	}
	var toolID, toolName string
	if err := json.Unmarshal(rawID, &toolID); err != nil || !validOpaqueID(toolID) {
		return nil, anthropicStreamBad()
	}
	if len(toolID) > d.lim.MaxArgsChars {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call exceeds the size bound"}
	}
	if err := json.Unmarshal(rawName, &toolName); err != nil || strings.TrimSpace(toolName) == "" {
		return nil, anthropicStreamBad()
	}
	if len(toolName) > d.lim.MaxArgsChars {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call exceeds the size bound"}
	}
	if d.seenCalls[toolID] {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned conflicting tool call identities"}
	}
	if d.toolCount+1 > d.lim.MaxToolCalls {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call count exceeds the size bound"}
	}
	// Seed the argument buffer with a non-empty start input. An
	// empty-object start is the streaming placeholder for deltas to
	// come, so it seeds nothing: retaining "{}" verbatim would corrupt
	// the later fragment concatenation. A malformed seed (non-object
	// or invalid JSON) is a syntax failure here, not a classification:
	// classification applies only to accumulated fragment bytes at stop.
	var seed []byte
	if rawInput, ok := cb["input"]; ok && !isNull(rawInput) {
		compacted, rerr := CompactArgs(rawInput)
		if rerr != nil {
			return nil, anthropicStreamBad()
		}
		if string(compacted) != "{}" {
			if len(compacted) > d.lim.MaxArgsChars || d.totalArgs+len(compacted) > d.lim.MaxStreamArgsChars {
				return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call exceeds the size bound"}
			}
			seed = []byte(compacted)
		}
	}
	item := d.nextItemID()
	open, rerr := NewToolBlockOpen(item, CallID(toolID), toolName, idx)
	if rerr != nil {
		return nil, rerr
	}
	blk := &anthropicStreamBlock{kind: BlockTool, item: item, call: CallID(toolID), name: toolName}
	if len(seed) > 0 {
		blk.args.Write(seed)
		blk.argsLen = len(seed)
		d.totalArgs += len(seed)
	}
	d.seenIdx[idx] = true
	d.seenCalls[toolID] = true
	d.toolCount++
	d.blocks[idx] = blk
	evs := []ResponseEvent{open}
	if len(seed) > 0 {
		frag, rerr := NewArgsFragment(item, string(seed))
		if rerr != nil {
			return nil, rerr
		}
		evs = append(evs, frag)
	}
	return evs, nil
}

// handleBlockDelta validates one content_block_delta and forwards its
// fragment immediately. Thinking/signature/citation and other
// unrepresentable delta kinds fail closed; kind-crossed deltas fail as
// malformed.
func (d *AnthropicStreamDecoder) handleBlockDelta(data string) ([]ResponseEvent, *RouterError) {
	if rerr := d.requireStarted(); rerr != nil {
		return nil, rerr
	}
	if strings.TrimSpace(data) == "" {
		return nil, anthropicStreamBad()
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &body); err != nil || body == nil {
		return nil, anthropicStreamBad()
	}
	if _, rerr := chatResponseObject(mustRaw(body), map[string]bool{"type": true, "index": true, "delta": true}, "content_block_delta"); rerr != nil {
		return nil, rerr
	}
	if rerr := anthropicStreamCheckType(body, "content_block_delta"); rerr != nil {
		return nil, rerr
	}
	rawIdx, ok := body["index"]
	if !ok {
		return nil, anthropicStreamBad()
	}
	idx, rerr := streamIndex(rawIdx)
	if rerr != nil {
		return nil, anthropicStreamBad()
	}
	blk, ok := d.blocks[idx]
	if !ok {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream sent a delta for an unknown block"}
	}
	rawDelta, ok := body["delta"]
	if !ok || isNull(rawDelta) {
		return nil, anthropicStreamBad()
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(rawDelta, &probe); err != nil || probe == nil {
		return nil, anthropicStreamBad()
	}
	rawDType, ok := probe["type"]
	if !ok || isNull(rawDType) {
		return nil, anthropicStreamBad()
	}
	var dtype string
	if err := json.Unmarshal(rawDType, &dtype); err != nil || dtype == "" {
		return nil, anthropicStreamBad()
	}
	switch dtype {
	case "text_delta":
		delta, rerr := chatResponseObject(rawDelta, map[string]bool{"type": true, "text": true}, "content_block_delta.delta")
		if rerr != nil {
			return nil, rerr
		}
		if blk.kind != BlockText {
			return nil, anthropicStreamBad()
		}
		rawText, ok := delta["text"]
		if !ok || isNull(rawText) {
			return nil, anthropicStreamBad()
		}
		var text string
		if err := json.Unmarshal(rawText, &text); err != nil {
			return nil, anthropicStreamBad()
		}
		if text == "" {
			return nil, nil
		}
		frag, rerr := NewTextFragment(blk.item, text)
		if rerr != nil {
			return nil, rerr
		}
		return []ResponseEvent{frag}, nil
	case "input_json_delta":
		delta, rerr := chatResponseObject(rawDelta, map[string]bool{"type": true, "partial_json": true}, "content_block_delta.delta")
		if rerr != nil {
			return nil, rerr
		}
		if blk.kind != BlockTool {
			return nil, anthropicStreamBad()
		}
		rawFrag, ok := delta["partial_json"]
		if !ok || isNull(rawFrag) {
			return nil, anthropicStreamBad()
		}
		var frag string
		if err := json.Unmarshal(rawFrag, &frag); err != nil {
			return nil, anthropicStreamBad()
		}
		if frag == "" {
			return nil, nil
		}
		if blk.argsLen+len(frag) > d.lim.MaxArgsChars || d.totalArgs+len(frag) > d.lim.MaxStreamArgsChars {
			return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call exceeds the size bound"}
		}
		blk.args.WriteString(frag)
		blk.argsLen += len(frag)
		d.totalArgs += len(frag)
		out, rerr := NewArgsFragment(blk.item, frag)
		if rerr != nil {
			return nil, rerr
		}
		return []ResponseEvent{out}, nil
	default:
		// Reasoning, signature, citation and hosted-tool deltas have no
		// semantic form here. The kind value itself may carry upstream
		// state and stays out of diagnostics.
		return nil, anthropicStreamUnrep("content_block_delta.delta.type")
	}
}

// handleBlockStop closes one open block. A tool block with no argument
// bytes emits the canonical "{}" exactly once (counted in both args
// bounds); accumulated bytes that never form one JSON object classify the
// block with MalformedToolEvent for the target gate instead of failing.
// Either marker precedes the BlockClose the lifecycle requires.
func (d *AnthropicStreamDecoder) handleBlockStop(data string) ([]ResponseEvent, *RouterError) {
	if rerr := d.requireStarted(); rerr != nil {
		return nil, rerr
	}
	if strings.TrimSpace(data) == "" {
		return nil, anthropicStreamBad()
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &body); err != nil || body == nil {
		return nil, anthropicStreamBad()
	}
	if _, rerr := chatResponseObject(mustRaw(body), map[string]bool{"type": true, "index": true}, "content_block_stop"); rerr != nil {
		return nil, rerr
	}
	if rerr := anthropicStreamCheckType(body, "content_block_stop"); rerr != nil {
		return nil, rerr
	}
	rawIdx, ok := body["index"]
	if !ok {
		return nil, anthropicStreamBad()
	}
	idx, rerr := streamIndex(rawIdx)
	if rerr != nil {
		return nil, anthropicStreamBad()
	}
	blk, ok := d.blocks[idx]
	if !ok {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream stopped an unknown block"}
	}
	var evs []ResponseEvent
	if blk.kind == BlockTool {
		raw := blk.args.String()
		if raw == "" {
			if blk.argsLen+2 > d.lim.MaxArgsChars || d.totalArgs+2 > d.lim.MaxStreamArgsChars {
				return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call exceeds the size bound"}
			}
			blk.args.WriteString("{}")
			blk.argsLen += 2
			d.totalArgs += 2
			frag, rerr := NewArgsFragment(blk.item, "{}")
			if rerr != nil {
				return nil, rerr
			}
			evs = append(evs, frag)
		} else if _, rerr := CompactArgs(json.RawMessage(raw)); rerr != nil {
			evs = append(evs, MalformedToolEvent{Item: blk.item})
		}
	}
	evs = append(evs, BlockClose{Item: blk.item})
	delete(d.blocks, idx)
	return evs, nil
}

// handleMessageDelta records one cumulative accounting update. Delta counts
// are authoritative totals, never increments: output overwrites, and any
// input/cache members present refresh the start values rather than adding
// to them. No events are emitted here; the terminal UsageEvent waits for a
// valid message_stop.
func (d *AnthropicStreamDecoder) handleMessageDelta(data string) ([]ResponseEvent, *RouterError) {
	if rerr := d.requireStarted(); rerr != nil {
		return nil, rerr
	}
	if len(d.blocks) != 0 {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent message_delta with unfinished blocks"}
	}
	if strings.TrimSpace(data) == "" {
		return nil, anthropicStreamBad()
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &body); err != nil || body == nil {
		return nil, anthropicStreamBad()
	}
	if _, rerr := chatResponseObject(mustRaw(body), map[string]bool{"type": true, "delta": true, "usage": true}, "message_delta"); rerr != nil {
		return nil, rerr
	}
	if rerr := anthropicStreamCheckType(body, "message_delta"); rerr != nil {
		return nil, rerr
	}
	rawDelta, ok := body["delta"]
	if !ok || isNull(rawDelta) {
		return nil, anthropicStreamBad()
	}
	delta, rerr := chatResponseObject(rawDelta, map[string]bool{
		"stop_reason": true, "stop_sequence": true, "stop_details": true, "container": true,
	}, "message_delta.delta")
	if rerr != nil {
		return nil, rerr
	}
	if rawContainer, ok := delta["container"]; ok && !isNull(rawContainer) {
		return nil, anthropicStreamUnrep("message_delta.delta.container")
	}
	if rawStop, ok := delta["stop_reason"]; ok && !isNull(rawStop) {
		var stop string
		if err := json.Unmarshal(rawStop, &stop); err != nil {
			return nil, anthropicStreamBad()
		}
		d.stop = stop
		d.sawDelta = true
	}
	if rawSS, ok := delta["stop_sequence"]; ok && !isNull(rawSS) {
		var ss string
		if err := json.Unmarshal(rawSS, &ss); err != nil {
			return nil, anthropicStreamBad()
		}
	}
	if rawDetails, ok := delta["stop_details"]; ok && !isNull(rawDetails) {
		if rerr := anthropicStreamStopDetails(rawDetails); rerr != nil {
			return nil, rerr
		}
		d.sawStopDetails = true
		// Same-delta mismatch fails immediately: non-null refusal details
		// cannot accompany a non-refusal stop in one update. Split-delta
		// mismatches wait for message_stop (see handleMessageStop).
		if d.stop != "" && d.stop != "refusal" {
			return nil, anthropicStreamUnrep("message_delta.delta.stop_details")
		}
	}
	rawUsage, ok := body["usage"]
	if !ok || isNull(rawUsage) {
		return nil, anthropicStreamBad()
	}
	u, rerr := parseAnthropicStreamUsageWithPolicy(rawUsage, "message_delta.usage", false, true, d.gatewayDrop)
	if rerr != nil {
		return nil, rerr
	}
	if u.hasInput {
		d.inputBase = u.input
	}
	if u.hasCreation {
		d.cacheCreate, d.hasCreation = u.creation, true
	}
	if u.hasRead {
		d.cacheRead, d.hasRead = u.read, true
	}
	if _, rerr := anthropicStreamPrompt(anthropicStreamUsage{
		input: d.inputBase, creation: d.cacheCreate, read: d.cacheRead,
		hasCreation: d.hasCreation, hasRead: d.hasRead,
	}); rerr != nil {
		return nil, rerr
	}
	d.output, d.hasOutput = u.output, true
	d.bodyEnded = true
	return nil, nil
}

// handleMessageStop validates the source terminal and emits the single
// authoritative UsageEvent plus CompletionEvent. It requires a recorded
// stop reason, terminal usage, and fully closed blocks, and maps the stop
// through the shared source finish (unknown stops fail closed with a
// static diagnostic). Unknown stops and truncation never become a Finish.
func (d *AnthropicStreamDecoder) handleMessageStop(data string) ([]ResponseEvent, *RouterError) {
	if rerr := d.requireStarted(); rerr != nil {
		return nil, rerr
	}
	if strings.TrimSpace(data) == "" {
		return nil, anthropicStreamBad()
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &body); err != nil || body == nil {
		return nil, anthropicStreamBad()
	}
	if _, rerr := chatResponseObject(mustRaw(body), map[string]bool{"type": true}, "message_stop"); rerr != nil {
		return nil, rerr
	}
	if rerr := anthropicStreamCheckType(body, "message_stop"); rerr != nil {
		return nil, rerr
	}
	if !d.sawDelta || d.stop == "" || !d.hasOutput {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream ended without completion"}
	}
	if len(d.blocks) > 0 {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream ended with unfinished blocks"}
	}
	// Split-delta safety gate: non-null refusal details seen in any
	// message_delta require a refusal terminal. Otherwise a safety signal
	// would be silently downgraded to a normal stop.
	if d.sawStopDetails && d.stop != "refusal" {
		return nil, anthropicStreamUnrep("message_delta.delta.stop_details")
	}
	finish, rerr := decodeAnthropicFinish(d.stop)
	if rerr != nil {
		return nil, rerr
	}
	prompt, rerr := anthropicStreamPrompt(anthropicStreamUsage{
		input: d.inputBase, creation: d.cacheCreate, read: d.cacheRead,
		hasCreation: d.hasCreation, hasRead: d.hasRead,
	})
	if rerr != nil {
		return nil, rerr
	}
	total := prompt + d.output
	if total < prompt || total < d.output {
		return nil, anthropicStreamBad()
	}
	usage, rerr := NewTerminalUsage(DialectAnthropic, prompt, d.output, total)
	if rerr != nil {
		return nil, rerr
	}
	var creation, read *int
	if d.hasCreation {
		c := d.cacheCreate
		creation = &c
	}
	if d.hasRead {
		r := d.cacheRead
		read = &r
	}
	if usage, rerr = usage.WithCache(creation, read); rerr != nil {
		return nil, rerr
	}
	complete, rerr := NewCompletionEvent(finish)
	if rerr != nil {
		return nil, rerr
	}
	d.done = true
	return []ResponseEvent{UsageEvent{Usage: usage}, complete}, nil
}

// anthropicStreamStopDetails validates one non-null message_delta.delta
// stop_details object for shape only (finish consistency waits for
// message_stop so split deltas stay safe). Accepted shape mirrors the
// complete decoder: {type:"refusal", category in the reviewed set or null,
// explanation string or null}, closed members, value-free diagnostics, never
// carried into the IR.
func anthropicStreamStopDetails(raw json.RawMessage) *RouterError {
	obj, rerr := chatResponseObject(raw, map[string]bool{
		"type": true, "category": true, "explanation": true,
	}, "message_delta.delta.stop_details")
	if rerr != nil {
		return rerr
	}
	rawType, ok := obj["type"]
	if !ok || isNull(rawType) {
		return anthropicStreamBad()
	}
	var typ string
	if err := json.Unmarshal(rawType, &typ); err != nil || typ != "refusal" {
		return anthropicStreamUnrep("message_delta.delta.stop_details.type")
	}
	if rawCat, ok := obj["category"]; ok && !isNull(rawCat) {
		var cat string
		if err := json.Unmarshal(rawCat, &cat); err != nil {
			return anthropicStreamBad()
		}
		switch cat {
		case "cyber", "bio", "frontier_llm", "reasoning_extraction", "general_harms":
		default:
			return anthropicStreamUnrep("message_delta.delta.stop_details.category")
		}
	}
	if rawExp, ok := obj["explanation"]; ok && !isNull(rawExp) {
		var exp string
		if err := json.Unmarshal(rawExp, &exp); err != nil {
			return anthropicStreamBad()
		}
		_ = exp
	}
	return nil
}

// handlePing accepts the documented keepalive. An empty payload is the
// common keepalive form; a non-empty payload must be the bare ping
// envelope. Ping is valid at any point, including before start.
func (d *AnthropicStreamDecoder) handlePing(data string) ([]ResponseEvent, *RouterError) {
	if strings.TrimSpace(data) == "" {
		return nil, nil
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &body); err != nil || body == nil {
		return nil, anthropicStreamBad()
	}
	if _, rerr := chatResponseObject(mustRaw(body), map[string]bool{"type": true}, "ping"); rerr != nil {
		return nil, rerr
	}
	if rerr := anthropicStreamCheckType(body, "ping"); rerr != nil {
		return nil, rerr
	}
	return nil, nil
}
