// Responses source stream codec: item-added validation.
//
// This file owns output_item.added handling for ResponsesStreamDecoder
// (message and tool item validation, pending-identity budgeting and
// arrival-order enforcement). See response_responses_stream_decode.go
// for the wire contract and divergence log.
package protocol

import (
	"crypto/sha256"
	"encoding/json"
	"strings"
)

func (d *ResponsesStreamDecoder) onMessageAdded(outIdx int, rawItem json.RawMessage) *RouterError {
	item, rerr := responsesStreamObject(rawItem, map[string]bool{
		"type": true, "id": true, "status": true, "role": true, "content": true, "phase": true,
	}, "response.output_item.added.item")
	if rerr != nil {
		return rerr
	}
	var phase string
	if rerr := validateResponsesMessagePhase(item["phase"], "response.output_item.added.item.phase", d.allowPrivateDrop, &phase); rerr != nil {
		return rerr
	}
	var itemID string
	if rawID, ok := item["id"]; !ok || json.Unmarshal(rawID, &itemID) != nil || !opaqueBoundedStream(itemID, d.lim) {
		return rsBad()
	}
	if d.seenItem[itemID] {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream returned conflicting response item identities"}
	}
	if rawSt, ok := item["status"]; !ok || isNull(rawSt) {
		return rsBad()
	} else {
		var st string
		if err := json.Unmarshal(rawSt, &st); err != nil || st != "in_progress" {
			return rsBad()
		}
	}
	// Absent role is tolerated (legacy r2cs policy); a present role must
	// be assistant, matching the complete row.
	if rawRole, ok := item["role"]; ok && !isNull(rawRole) {
		var role string
		if err := json.Unmarshal(rawRole, &role); err != nil || role != "assistant" {
			return rsBad()
		}
	}
	// Absent content is tolerated (legacy r2cs policy); present content
	// must be the empty array: parts arrive separately.
	if rawContent, ok := item["content"]; ok && !isNull(rawContent) {
		var arr []json.RawMessage
		if err := json.Unmarshal(rawContent, &arr); err != nil || arr == nil || len(arr) != 0 {
			return rsBad()
		}
	}
	if len(d.slots)+1 > d.lim.MaxStreamBlocks {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream stream exceeds the content block bound"}
	}
	// Bound pending source-owned identities before the semantic block
	// opens: BlockOpen is withheld until content_part.added, so the
	// lifecycle's post-open identity budget never sees these bytes.
	// Release happens at part.added (identity flows into the
	// lifecycle) or at output_item.done for empty items (retention
	// discarded). Value-free: the bound error never echoes the ID.
	next, rerr := addBytes(d.pendingIdentityBytes, len(itemID))
	if rerr != nil || next > int64(d.lim.MaxStreamArgsChars) {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream response exceeds the size bound"}
	}
	d.seenItem[itemID] = true
	slot := &responsesStreamSlot{outIdx: outIdx, itemID: itemID, kind: "message", textHash: sha256.New(), messagePhase: phase}
	slot.pendingIdentityBytes = len(itemID)
	d.pendingIdentityBytes = next
	d.slots[outIdx] = slot
	d.order = append(d.order, outIdx)
	return nil
}

func (d *ResponsesStreamDecoder) onToolAdded(outIdx int, rawItem json.RawMessage) ([]ResponseEvent, *RouterError) {
	item, rerr := responsesStreamObject(rawItem, map[string]bool{
		"type": true, "id": true, "call_id": true, "name": true,
		"arguments": true, "status": true, "namespace": true,
		"caller": true, "async": true,
	}, "response.output_item.added.item")
	if rerr != nil {
		return nil, rerr
	}
	for _, k := range []string{"namespace", "caller"} {
		if rawV, ok := item[k]; ok && !isNull(rawV) {
			return nil, rsUnrep("response.output_item.added.item." + k)
		}
	}
	if rawA, ok := item["async"]; ok && !isNull(rawA) {
		var bAsync bool
		if err := json.Unmarshal(rawA, &bAsync); err != nil {
			return nil, rsBad()
		}
		if bAsync {
			return nil, rsUnrep("response.output_item.added.item.async")
		}
	}
	var itemID string
	if rawID, ok := item["id"]; !ok || json.Unmarshal(rawID, &itemID) != nil || !opaqueBoundedStream(itemID, d.lim) {
		return nil, rsBad()
	}
	if d.seenItem[itemID] {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream returned conflicting response item identities"}
	}
	var callID string
	if rawC, ok := item["call_id"]; !ok || json.Unmarshal(rawC, &callID) != nil || strings.TrimSpace(callID) == "" {
		return nil, rsBad()
	}
	if len(callID) > d.lim.MaxArgsChars {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call exceeds the size bound"}
	}
	if d.seenCall[callID] {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream returned conflicting tool call identities"}
	}
	// The name must be present and nonempty to open a block (identity);
	// its 1..64 grammar is NOT enforced here (recorded for the malformed
	// classification instead: the complete row proves grammar-invalid
	// names are source facts, not destination-specific failures).
	var name string
	if rawN, ok := item["name"]; !ok || json.Unmarshal(rawN, &name) != nil || strings.TrimSpace(name) == "" {
		return nil, rsBad()
	}
	if len(name) > d.lim.MaxArgsChars {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call exceeds the size bound"}
	}
	if rawSt, ok := item["status"]; !ok || isNull(rawSt) {
		return nil, rsBad()
	} else {
		var st string
		if err := json.Unmarshal(rawSt, &st); err != nil || st != "in_progress" {
			return nil, rsBad()
		}
	}
	// Arguments at added time are source-absent (absent/null/""); deltas
	// carry the payload. A non-string or nonempty seed contradicts the
	// delta contract.
	if rawArgs, ok := item["arguments"]; ok && !isNull(rawArgs) {
		var seed string
		if err := json.Unmarshal(rawArgs, &seed); err != nil || seed != "" {
			return nil, rsBad()
		}
	}
	if len(d.slots)+1 > d.lim.MaxStreamBlocks {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream stream exceeds the content block bound"}
	}
	if d.tools+1 > d.lim.MaxToolCalls {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call count exceeds the size bound"}
	}
	d.seenItem[itemID] = true
	d.seenCall[callID] = true
	d.tools++
	slot := &responsesStreamSlot{outIdx: outIdx, itemID: itemID, kind: "tool",
		callID: callID, name: name, badName: validateToolName(name) != nil, textHash: sha256.New()}
	d.slots[outIdx] = slot
	d.order = append(d.order, outIdx)
	open, rerr := NewToolBlockOpen(ItemID(itemID), CallID(callID), name, outIdx)
	if rerr != nil {
		return nil, rerr
	}
	return []ResponseEvent{open}, nil
}
