// Responses source private-reasoning compatibility: provider-private
// reasoning OUTPUT items for the Responses complete and SSE decoders.
//
// Wire contract (measured live Muse OpenCode Responses shape):
//   - added:   {"id":<opaque>,"type":"reasoning","status":"in_progress","summary":[]}
//   - done:    {"id":<same>,"type":"reasoning","status":"completed",
//     "encrypted_content":<nonempty opaque>,"summary":[]}
//   - terminal output repeats the item identity with an independently emitted
//     encrypted value, then the visible message.
//
// Policy (narrow, destination-independent, lossy-only):
//   - Strict (default) refuses every reasoning OUTPUT item with a
//     value-free `has no representation` path, preserving the existing
//     complete and stream contracts.
//   - Lossy (allowPrivateDrop) admits ONLY the exact measured shape:
//     closed safe metadata, empty summary, no new event families. It
//     retains bounded identity and ordering facts to validate
//     source lineage at the terminal, emits no public text/metadata from
//     reasoning or encrypted blobs, and never returns reasoning as
//     continuation input. Visible messages and authoritative
//     reasoning/token usage are preserved.
//   - Nonempty summaries, new reasoning event families, malformed types,
//     missing/empty identities, and contradictory terminal lineage fail
//     closed in both modes. Truncation, pending, finish, and size checks
//     are unchanged.
//
// Dropped bytes are never buffered as text, never emitted as fragments,
// never added to usage, and never used to invent tool or continuation
// state. Diagnostics name field paths only, never content.
package protocol

import (
	"encoding/json"
	"fmt"
)

// validateResponsesReasoningSummary validates one reasoning `summary`
// value: absent/null/[] is exact (no reasoning text to carry);
// malformed JSON types are invalid; a nonempty array has no
// representation (even in lossy mode). `what` is the value-free field
// path naming the summary itself.
func validateResponsesReasoningSummary(raw json.RawMessage, what string) *RouterError {
	if isNull(raw) {
		return nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	}
	if len(arr) > 0 {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: fmt.Sprintf("Upstream response feature %q has no representation", what)}
	}
	return nil
}

// validateResponsesReasoningEncrypted validates one required
// `encrypted_content` blob: a nonempty opaque bounded string. Missing,
// null, empty, malformed, or oversized values are invalid (never
// unrepresentable: the shape itself is the measured contract, not a
// target-divergent feature). The value is discarded after validation.
func validateResponsesReasoningEncrypted(raw json.RawMessage, lim Limits) *RouterError {
	bad := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	if len(raw) == 0 || isNull(raw) {
		return bad
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return bad
	}
	if !validOpaqueID(s) {
		return bad
	}
	if len(s) > lim.MaxArgsChars {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream response exceeds the size bound"}
	}
	return nil
}

// decodeResponsesReasoningCompleteItem validates one complete-response
// reasoning OUTPUT item for the lossy policy and drops it (no IR item,
// no malformed count). Strict callers never reach here: they fail the
// type in decodeResponsesOutput before this helper.
func decodeResponsesReasoningCompleteItem(raw json.RawMessage, oi int, lim Limits, seenItem map[string]bool) *RouterError {
	bad := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	what := fmt.Sprintf("response.output[%d]", oi)
	item, rerr := chatResponseObject(raw, map[string]bool{
		"type": true, "id": true, "status": true, "summary": true,
		"encrypted_content": true,
	}, what)
	if rerr != nil {
		return rerr
	}
	var typ string
	if err := json.Unmarshal(item["type"], &typ); err != nil || typ != "reasoning" {
		return bad
	}
	var id string
	if rawID, ok := item["id"]; !ok || json.Unmarshal(rawID, &id) != nil || !validOpaqueID(id) {
		return bad
	}
	if len(id) > lim.MaxArgsChars {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream response exceeds the size bound"}
	}
	if seenItem[id] {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream returned conflicting response item identities"}
	}
	seenItem[id] = true
	if rawSt, ok := item["status"]; ok && !isNull(rawSt) {
		var st string
		if err := json.Unmarshal(rawSt, &st); err != nil || (st != "completed" && st != "incomplete") {
			return bad
		}
	}
	if rawSum, ok := item["summary"]; ok && !isNull(rawSum) {
		if rerr := validateResponsesReasoningSummary(rawSum, what+".summary"); rerr != nil {
			return rerr
		}
	}
	return validateResponsesReasoningEncrypted(item["encrypted_content"], lim)
}

// onReasoningAdded validates one lossy reasoning output_item.added item:
// closed safe metadata only (id/type/status/summary), status exactly
// in_progress, summary empty. It retains bounded identity/ordering facts
// and emits no events. Strict callers never reach here.
func (d *ResponsesStreamDecoder) onReasoningAdded(outIdx int, rawItem json.RawMessage) *RouterError {
	item, rerr := responsesStreamObject(rawItem, map[string]bool{
		"type": true, "id": true, "status": true, "summary": true,
	}, "response.output_item.added.item")
	if rerr != nil {
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
	if rawSum, ok := item["summary"]; ok && !isNull(rawSum) {
		if rerr := validateResponsesReasoningSummary(rawSum, "response.output_item.added.item.summary"); rerr != nil {
			// Preserve the stream invalid vs unrepresentable split:
			// malformed summaries are invalid, nonempty are
			// unrepresentable. The helper already distinguishes them,
			// but its invalid message is the complete-row text; map it
			// to the stream invalid marker.
			if rerr.Msg == "Upstream returned an invalid response" {
				return rsBad()
			}
			return rerr
		}
	}
	if len(d.slots)+1 > d.lim.MaxStreamBlocks {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream stream exceeds the content block bound"}
	}
	next, rerr := addBytes(d.pendingIdentityBytes, len(itemID))
	if rerr != nil || next > int64(d.lim.MaxStreamArgsChars) {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream response exceeds the size bound"}
	}
	d.seenItem[itemID] = true
	slot := &responsesStreamSlot{outIdx: outIdx, itemID: itemID, kind: "reasoning"}
	slot.pendingIdentityBytes = len(itemID)
	d.pendingIdentityBytes = next
	d.slots[outIdx] = slot
	d.order = append(d.order, outIdx)
	return nil
}

// validateReasoningDoneItem validates one lossy reasoning
// output_item.done payload: identity coherence, completed/incomplete
// status, empty summary, and a required nonempty encrypted blob. It
// discards the opaque value and emits no events.
func (d *ResponsesStreamDecoder) validateReasoningDoneItem(rawItem json.RawMessage, s *responsesStreamSlot) *RouterError {
	item, rerr := responsesStreamObject(rawItem, map[string]bool{
		"type": true, "id": true, "status": true, "summary": true,
		"encrypted_content": true,
	}, "response.output_item.done.item")
	if rerr != nil {
		return rerr
	}
	var typ string
	if err := json.Unmarshal(item["type"], &typ); err != nil || typ != "reasoning" {
		return rsBad()
	}
	var id string
	if rawID, ok := item["id"]; !ok || json.Unmarshal(rawID, &id) != nil || id != s.itemID {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned conflicting response item identities"}
	}
	var st string
	if rawSt, ok := item["status"]; !ok || json.Unmarshal(rawSt, &st) != nil || (st != "completed" && st != "incomplete") {
		return rsBad()
	}
	s.doneStatus = st
	if rawSum, ok := item["summary"]; ok && !isNull(rawSum) {
		if rerr := validateResponsesReasoningSummary(rawSum, "response.output_item.done.item.summary"); rerr != nil {
			if rerr.Msg == "Upstream returned an invalid response" {
				return rsBad()
			}
			return rerr
		}
	}
	rawEnc, ok := item["encrypted_content"]
	if !ok {
		return rsBad()
	}
	rerr = validateResponsesReasoningEncrypted(rawEnc, d.lim)
	if rerr != nil {
		if rerr.Msg == "Upstream response exceeds the size bound" {
			return rerr
		}
		return rsBad()
	}
	return nil
}

// checkRawReasoningLineage validates one raw terminal reasoning item
// against its streamed slot: provider id, done status, and empty summary.
// The gateway can emit different ciphertext for the same private item in
// the terminal envelope. The shared complete decoder validates that value.
func checkRawReasoningLineage(raw json.RawMessage, s *responsesStreamSlot) *RouterError {
	contradictory := &RouterError{Status: 502, Code: CodeUpstreamBad,
		Msg: "Upstream stream sent contradictory terminal output"}
	item, rerr := responsesStreamObject(raw, map[string]bool{
		"type": true, "id": true, "status": true, "summary": true,
		"encrypted_content": true,
	}, "response.output")
	if rerr != nil {
		return rerr
	}
	var typ string
	if err := json.Unmarshal(item["type"], &typ); err != nil || typ != "reasoning" {
		return rsBad()
	}
	var id string
	if rawID, ok := item["id"]; !ok || json.Unmarshal(rawID, &id) != nil || id != s.itemID {
		return contradictory
	}
	var status string
	if rawSt, ok := item["status"]; ok && !isNull(rawSt) {
		if err := json.Unmarshal(rawSt, &status); err != nil {
			return rsBad()
		}
		if status != s.doneStatus {
			return contradictory
		}
	} else if s.doneStatus != "" {
		// Terminal omits status while the stream proved one: the raw
		// wire disagrees with the streamed lineage.
		return contradictory
	}
	if rawSum, ok := item["summary"]; ok && !isNull(rawSum) {
		if rerr := validateResponsesReasoningSummary(rawSum, "response.output.summary"); rerr != nil {
			if rerr.Msg == "Upstream returned an invalid response" {
				return rsBad()
			}
			return rerr
		}
	}
	return nil
}
