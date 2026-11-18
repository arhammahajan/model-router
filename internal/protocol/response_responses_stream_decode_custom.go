package protocol

import (
	"encoding/json"
	"strings"
)

// Responses custom stream codec: custom_tool_call item validation and
// freeform input delta/done handling.
//
// Wire contract (OpenAI Responses custom-tool streaming events):
//   - response.output_item.added with item type custom_tool_call carries
//     the complete identity (id, call_id, name) with empty input (""
//     or absent) and status in_progress. A non-string or nonempty seed
//     contradicts the delta contract.
//   - response.custom_tool_call_input.delta carries {item_id,
//     output_index, delta:string}; empty deltas emit nothing (legacy skip).
//   - response.custom_tool_call_input.done carries {item_id,
//     output_index, input:string} and must equal the accumulation exactly.
//   - response.output_item.done carries the full custom_tool_call item
//     which must agree with the accumulation; grammar-invalid names are
//     typed malformed provenance (target gate), never a 502.
// Bounds mirror function tools: per-field MaxArgsChars, aggregate
// MaxStreamArgsChars, distinct blocks MaxStreamBlocks, calls MaxToolCalls.

func (d *ResponsesStreamDecoder) onCustomAdded(outIdx int, rawItem json.RawMessage) ([]ResponseEvent, *RouterError) {
	item, rerr := responsesStreamObject(rawItem, map[string]bool{
		"type": true, "id": true, "call_id": true, "name": true,
		"input": true, "status": true, "namespace": true,
		"caller": true,
	}, "response.output_item.added.item")
	if rerr != nil {
		return nil, rerr
	}
	for _, k := range []string{"namespace", "caller"} {
		if rawV, ok := item[k]; ok && !isNull(rawV) {
			return nil, rsUnrep("response.output_item.added.item." + k)
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
	if rawIn, ok := item["input"]; ok && !isNull(rawIn) {
		var seed string
		if err := json.Unmarshal(rawIn, &seed); err != nil || seed != "" {
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
	slot := &responsesStreamSlot{outIdx: outIdx, itemID: itemID, kind: "custom",
		callID: callID, name: name, badName: validateToolName(name) != nil}
	// bad UTF-8 in names is caught by validOpaqueID? names may contain
	// spaces; only grammar decides malformed, never a 502 here beyond
	// the empty check above. Non-UTF8 names fail at BlockOpen validation.
	d.slots[outIdx] = slot
	d.order = append(d.order, outIdx)
	open, rerr := NewBlockOpen(ItemID(itemID), BlockCustom, CallID(callID), name)
	if rerr != nil {
		return nil, rerr
	}
	open.SourceIndex = intPtr(outIdx)
	return []ResponseEvent{open}, nil
}

func (d *ResponsesStreamDecoder) onCustomDelta(body map[string]json.RawMessage) ([]ResponseEvent, *RouterError) {
	ev := "response.custom_tool_call_input.delta"
	if _, rerr := responsesStreamObject(mustRaw(body), map[string]bool{
		"type": true, "sequence_number": true, "output_index": true,
		"item_id": true, "delta": true,
	}, ev); rerr != nil {
		return nil, rerr
	}
	if rerr := d.checkSequence(body); rerr != nil {
		return nil, rerr
	}
	rawIdx, ok := body["output_index"]
	if !ok || isNull(rawIdx) {
		return nil, rsBad()
	}
	outIdx, rerr := responseInt(rawIdx)
	if rerr != nil {
		return nil, rsBad()
	}
	rawIID, ok := body["item_id"]
	if !ok || isNull(rawIID) {
		return nil, rsBad()
	}
	var iid string
	if err := json.Unmarshal(rawIID, &iid); err != nil {
		return nil, rsBad()
	}
	s, rerr := d.slotFor(outIdx, iid)
	if rerr != nil {
		return nil, rerr
	}
	if s.kind != "custom" {
		return nil, rsBad()
	}
	if s.argsDone || s.itemDone {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream stream sent arguments after completion"}
	}
	rawD, ok := body["delta"]
	if !ok || isNull(rawD) {
		return nil, rsBad()
	}
	frag, valid := decodeUnicodeString(rawD)
	if !valid {
		return nil, rsBad()
	}
	if frag == "" {
		return nil, nil
	}
	if s.argsLen+len(frag) > d.lim.MaxArgsChars ||
		d.totalArgs+len(frag) > d.lim.MaxStreamArgsChars {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call exceeds the size bound"}
	}
	s.args.WriteString(frag)
	s.argsLen += len(frag)
	d.totalArgs += len(frag)
	out, rerr := NewCustomDelta(ItemID(s.itemID), frag)
	if rerr != nil {
		return nil, rerr
	}
	return []ResponseEvent{out}, nil
}

func (d *ResponsesStreamDecoder) onCustomDone(body map[string]json.RawMessage) *RouterError {
	ev := "response.custom_tool_call_input.done"
	if _, rerr := responsesStreamObject(mustRaw(body), map[string]bool{
		"type": true, "sequence_number": true, "output_index": true,
		"item_id": true, "input": true,
	}, ev); rerr != nil {
		return rerr
	}
	if rerr := d.checkSequence(body); rerr != nil {
		return rerr
	}
	rawIdx, ok := body["output_index"]
	if !ok || isNull(rawIdx) {
		return rsBad()
	}
	outIdx, rerr := responseInt(rawIdx)
	if rerr != nil {
		return rsBad()
	}
	rawIID, ok := body["item_id"]
	if !ok || isNull(rawIID) {
		return rsBad()
	}
	var iid string
	if err := json.Unmarshal(rawIID, &iid); err != nil {
		return rsBad()
	}
	s, rerr := d.slotFor(outIdx, iid)
	if rerr != nil {
		return rerr
	}
	if s.kind != "custom" {
		return rsBad()
	}
	if s.argsDone || s.itemDone {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream stream sent duplicate done markers"}
	}
	rawIn, ok := body["input"]
	if !ok || isNull(rawIn) {
		return rsBad()
	}
	final, valid := decodeUnicodeString(rawIn)
	if !valid {
		return rsBad()
	}
	if final != s.args.String() {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream stream sent contradictory tool arguments"}
	}
	s.argsDone = true
	return nil
}

// validateCustomDoneItem strictly validates a custom_tool_call
// output_item.done payload against the added identity and accumulated
// input: type coherence, id/call_id/name/input/status shapes and input
// exactness. Grammar-invalid names are typed malformed provenance, not
// failures here.
func (d *ResponsesStreamDecoder) validateCustomDoneItem(rawItem json.RawMessage, s *responsesStreamSlot) *RouterError {
	_ = d
	item, rerr := responsesStreamObject(rawItem, map[string]bool{
		"type": true, "id": true, "call_id": true, "name": true,
		"input": true, "status": true, "namespace": true,
		"caller": true,
	}, "response.output_item.done.item")
	if rerr != nil {
		return rerr
	}
	for _, k := range []string{"namespace", "caller"} {
		if rawV, ok := item[k]; ok && !isNull(rawV) {
			return rsUnrep("response.output_item.done.item." + k)
		}
	}
	var typ string
	if err := json.Unmarshal(item["type"], &typ); err != nil || typ != "custom_tool_call" {
		return rsBad()
	}
	var id string
	if rawID, ok := item["id"]; !ok || json.Unmarshal(rawID, &id) != nil || id != s.itemID {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned conflicting response item identities"}
	}
	var callID string
	if rawC, ok := item["call_id"]; !ok || json.Unmarshal(rawC, &callID) != nil || callID != s.callID {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned conflicting tool call identities"}
	}
	var st string
	if rawSt, ok := item["status"]; !ok || json.Unmarshal(rawSt, &st) != nil || (st != "completed" && st != "incomplete") {
		return rsBad()
	}
	s.doneStatus = st
	var name string
	if rawN, ok := item["name"]; !ok || json.Unmarshal(rawN, &name) != nil || name != s.name {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream terminal output contradicts streamed deltas"}
	}
	rawIn, ok := item["input"]
	if !ok || isNull(rawIn) {
		if s.args.String() != "" {
			return &RouterError{Status: 502, Code: CodeUpstreamBad,
				Msg: "Upstream terminal output contradicts streamed deltas"}
		}
		return nil
	}
	input, valid := decodeUnicodeString(rawIn)
	if !valid {
		return rsBad()
	}
	if input != s.args.String() {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream terminal output contradicts streamed deltas"}
	}
	return nil
}
