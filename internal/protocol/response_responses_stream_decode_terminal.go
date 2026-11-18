// Responses source stream codec: item-done and terminal validation.
//
// This file owns output_item.done strict validation, the canonical empty
// args fallback, and terminal raw-lineage checks for ResponsesStreamDecoder
// (see response_responses_stream_decode.go for the wire contract and
// divergence log). Splitting keeps each module under the migration
// module-map bound; no behavior lives here that the header file does not
// describe.
package protocol

import (
	"crypto/sha256"
	"encoding/json"
)

func (d *ResponsesStreamDecoder) onItemDone(body map[string]json.RawMessage) ([]ResponseEvent, *RouterError) {
	if _, rerr := responsesStreamObject(mustRaw(body), map[string]bool{
		"type": true, "sequence_number": true, "output_index": true, "item": true,
	}, "response.output_item.done"); rerr != nil {
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
	s, ok := d.slots[outIdx]
	if !ok {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	}
	if s.itemDone {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream stream sent duplicate done markers"}
	}
	rawItem, ok := body["item"]
	if !ok || isNull(rawItem) {
		return nil, rsBad()
	}
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(rawItem, &probe); err != nil || probe.Type == "" {
		return nil, rsBad()
	}
	switch probe.Type {
	case "message":
		if s.kind != "message" {
			return nil, rsBad()
		}
		if rerr := d.validateMessageDoneItem(rawItem, s); rerr != nil {
			return nil, rerr
		}
	case "function_call":
		if s.kind != "tool" {
			return nil, rsBad()
		}
		if rerr := d.validateToolDoneItem(rawItem, s); rerr != nil {
			return nil, rerr
		}
	case "custom_tool_call":
		if s.kind != "custom" {
			return nil, rsBad()
		}
		if rerr := d.validateCustomDoneItem(rawItem, s); rerr != nil {
			return nil, rerr
		}
	case "reasoning":
		if !d.allowPrivateDrop {
			return nil, rsUnrep("response.output_item.done.item.type")
		}
		if s.kind != "reasoning" {
			return nil, rsBad()
		}
		if rerr := d.validateReasoningDoneItem(rawItem, s); rerr != nil {
			return nil, rerr
		}
	default:
		return nil, rsUnrep("response.output_item.done.item.type")
	}
	// Every opened block must be fully closed before the terminal: an
	// unfinished block contradicts the stream. A message item that never
	// received a content part contributes nothing (the complete row drops
	// empty spans); it closes without events since no block ever opened.
	// Its withheld pending identity is released: nothing flows into the
	// lifecycle, so nothing stays retained. Reasoning items never open a
	// block either: they close without events after releasing their
	// withheld identity.
	if s.kind == "reasoning" {
		d.pendingIdentityBytes -= int64(s.pendingIdentityBytes)
		s.pendingIdentityBytes = 0
		s.itemDone = true
		return nil, nil
	}
	if s.kind == "message" {
		if s.partKind == "" {
			d.pendingIdentityBytes -= int64(s.pendingIdentityBytes)
			s.pendingIdentityBytes = 0
			s.itemDone = true
			return nil, nil
		}
		if !s.textDone || !s.partDone {
			return nil, &RouterError{Status: 502, Code: CodeUpstreamBad,
				Msg: "Upstream stream ended with unfinished content"}
		}
	} else {
		if !s.argsDone {
			return nil, &RouterError{Status: 502, Code: CodeUpstreamBad,
				Msg: "Upstream stream ended with unfinished content"}
		}
	}
	s.itemDone = true
	var evs []ResponseEvent
	// Ordinary malformed tools emit only their classification, never
	// fabricated argument bytes. Valid tools with source-absent
	// arguments canonicalize to one explicit "{}" fragment here
	// (never duplicated: explicit "{}" bytes already arrived as
	// fragments), counted in both the per-call and aggregate bounds
	// with lifecycle hash/buffer semantics, exactly like the accepted
	// Chat source.
	if s.malformedTool() {
		d.malformed++
		evs = append(evs, MalformedToolEvent{Item: ItemID(s.itemID)})
	} else if s.kind == "tool" && s.args.String() == "" {
		if s.argsLen+2 > d.lim.MaxArgsChars ||
			d.totalArgs+2 > d.lim.MaxStreamArgsChars {
			return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call exceeds the size bound"}
		}
		d.totalArgs += 2
		frag, rerr := NewArgsFragment(ItemID(s.itemID), "{}")
		if rerr != nil {
			return nil, rerr
		}
		evs = append(evs, frag)
	}
	evs = append(evs, BlockClose{Item: ItemID(s.itemID)})
	return evs, nil
}

// validateMessageDoneItem strictly validates a message output_item.done
// payload against the added identity and streamed text: type coherence,
// id/role/status/content shapes, and text hash agreement. Unknown
// non-null members (including private_* semantics) fail as
// unrepresentable; they are never folded into the block.
func (d *ResponsesStreamDecoder) validateMessageDoneItem(rawItem json.RawMessage, s *responsesStreamSlot) *RouterError {
	item, rerr := responsesStreamObject(rawItem, map[string]bool{
		"type": true, "id": true, "status": true, "role": true, "content": true, "phase": true,
	}, "response.output_item.done.item")
	if rerr != nil {
		return rerr
	}
	if rerr := validateResponsesMessagePhase(item["phase"], "response.output_item.done.item.phase", d.allowPrivateDrop, &s.messagePhase); rerr != nil {
		return rerr
	}
	var typ string
	if err := json.Unmarshal(item["type"], &typ); err != nil || typ != "message" {
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
	var role string
	if rawRole, ok := item["role"]; !ok || json.Unmarshal(rawRole, &role) != nil || role != "assistant" {
		return rsBad()
	}
	rawContent, ok := item["content"]
	if !ok || isNull(rawContent) {
		return rsBad()
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(rawContent, &parts); err != nil || parts == nil {
		return rsBad()
	}
	if s.textLen == 0 {
		// Empty spans contribute nothing downstream: the done payload
		// must carry no content (empty array) or one empty part of the
		// opened kind, matching the added-time contract.
		if len(parts) == 0 {
			return nil
		}
		if len(parts) == 1 {
			part, rerr := responsesStreamObject(parts[0], map[string]bool{
				"type": true, "text": true, "annotations": true, "logprobs": true,
				"refusal": true,
			}, "response.output_item.done.item.content[0]")
			if rerr != nil {
				return rerr
			}
			var ptype string
			if err := json.Unmarshal(part["type"], &ptype); err != nil {
				return rsBad()
			}
			switch ptype {
			case "output_text":
				if s.partKind != "text" {
					return rsBad()
				}
				if rawT, ok := part["text"]; !ok || isNull(rawT) {
					return rsBad()
				} else {
					var t string
					if json.Unmarshal(rawT, &t) != nil || t != "" {
						return &RouterError{Status: 502, Code: CodeUpstreamBad,
							Msg: "Upstream terminal output contradicts streamed deltas"}
					}
				}
			case "refusal":
				if s.partKind != "refusal" {
					return rsBad()
				}
				if rawR, ok := part["refusal"]; !ok || isNull(rawR) {
					return rsBad()
				} else {
					var t string
					if json.Unmarshal(rawR, &t) != nil || t != "" {
						return &RouterError{Status: 502, Code: CodeUpstreamBad,
							Msg: "Upstream terminal output contradicts streamed deltas"}
					}
				}
			default:
				return rsUnrep("response.output_item.done.item.content[0].type")
			}
			return nil
		}
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream terminal output contradicts streamed deltas"}
	}
	if len(parts) != 1 {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream terminal output contradicts streamed deltas"}
	}
	part, rerr := responsesStreamObject(parts[0], map[string]bool{
		"type": true, "text": true, "annotations": true, "logprobs": true,
		"refusal": true,
	}, "response.output_item.done.item.content[0]")
	if rerr != nil {
		return rerr
	}
	var ptype string
	if err := json.Unmarshal(part["type"], &ptype); err != nil {
		return rsBad()
	}
	var text string
	switch ptype {
	case "output_text":
		if s.partKind != "text" {
			return rsBad()
		}
		rawT, ok := part["text"]
		if !ok || isNull(rawT) || json.Unmarshal(rawT, &text) != nil {
			return rsBad()
		}
	case "refusal":
		if s.partKind != "refusal" {
			return rsBad()
		}
		rawR, ok := part["refusal"]
		if !ok || isNull(rawR) || json.Unmarshal(rawR, &text) != nil {
			return rsBad()
		}
	default:
		return rsUnrep("response.output_item.done.item.content[0].type")
	}
	for _, k := range []string{"annotations", "logprobs"} {
		if rawV, ok := part[k]; ok && !isNull(rawV) {
			var arr []json.RawMessage
			if err := json.Unmarshal(rawV, &arr); err != nil || len(arr) > 0 {
				return rsUnrep("response.output_item.done.item.content[0]." + k)
			}
		}
	}
	h := sha256.Sum256([]byte(text))
	if h != s.doneHash || len(text) != s.doneLen {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream terminal output contradicts streamed deltas"}
	}
	return nil
}

// validateToolDoneItem strictly validates a function_call
// output_item.done payload against the added identity and accumulated
// arguments: type coherence, id/call_id/name/arguments/status shapes,
// and exact argument agreement with the args.done marker. Unknown
// non-null members (including private_* semantics) fail as
// unrepresentable. Grammar-invalid names and non-object arguments are
// NOT failures here: they are the typed malformed provenance the
// terminal counts and omits.
func (d *ResponsesStreamDecoder) validateToolDoneItem(rawItem json.RawMessage, s *responsesStreamSlot) *RouterError {
	_ = d
	item, rerr := responsesStreamObject(rawItem, map[string]bool{
		"type": true, "id": true, "call_id": true, "name": true,
		"arguments": true, "status": true, "namespace": true,
		"caller": true, "async": true,
	}, "response.output_item.done.item")
	if rerr != nil {
		return rerr
	}
	for _, k := range []string{"namespace", "caller"} {
		if rawV, ok := item[k]; ok && !isNull(rawV) {
			return rsUnrep("response.output_item.done.item." + k)
		}
	}
	if rawA, ok := item["async"]; ok && !isNull(rawA) {
		var bAsync bool
		if err := json.Unmarshal(rawA, &bAsync); err != nil {
			return rsBad()
		}
		if bAsync {
			return rsUnrep("response.output_item.done.item.async")
		}
	}
	var typ string
	if err := json.Unmarshal(item["type"], &typ); err != nil || typ != "function_call" {
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
	rawArgs, ok := item["arguments"]
	if !ok || isNull(rawArgs) {
		return rsBad()
	}
	var argsStr string
	if err := json.Unmarshal(rawArgs, &argsStr); err != nil {
		return rsBad()
	}
	if !responsesCanonicalArgsEqual(argsStr, s.args.String()) {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream terminal output contradicts streamed deltas"}
	}
	return nil
}

func (d *ResponsesStreamDecoder) onTerminal(body map[string]json.RawMessage, kind string) ([]ResponseEvent, *RouterError) {
	ev := "response." + kind
	if _, rerr := responsesStreamObject(mustRaw(body), map[string]bool{
		"type": true, "sequence_number": true, "response": true,
	}, ev); rerr != nil {
		return nil, rerr
	}
	if rerr := d.checkSequence(body); rerr != nil {
		return nil, rerr
	}
	rawResp, ok := body["response"]
	if !ok || isNull(rawResp) {
		return nil, rsBad()
	}
	// The terminal envelope IS the actual source wire: its status must
	// match the event kind before the shared complete-row validator runs.
	var statusProbe struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rawResp, &statusProbe); err != nil || statusProbe.Status != kind {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream stream sent contradictory terminal state"}
	}
	// Raw wire lineage is proven BEFORE canonicalization: the terminal
	// output array positions, provider item/call identities, kinds,
	// roles, names, arguments and content must agree with the added,
	// delta and done events. The shared validator synthesizes canonical
	// item IDs, so it cannot prove this lineage itself.
	if rerr := d.checkTerminalLineage(rawResp); rerr != nil {
		return nil, rerr
	}
	// Reuse the shared complete-response validator: envelope, output,
	// usage, and finish validation stay single-sourced (this is the
	// terminal Responses envelope, not an intermediate). The decoder
	// policy threads through so a lossy reasoning lineage validates
	// with the same drop semantics as the stream.
	resp, rerr := DecodeResponsesResponseWithPolicy([]byte(rawResp), d.lim, d.allowPrivateDrop)
	if rerr != nil {
		return nil, rerr
	}
	// Source identity is immutable; the provider model may be reported
	// for the first time here when created (and all progress envelopes)
	// omitted it. That absent-to-actual promotion emits a typed
	// ResponseModelEvent BEFORE UsageEvent plus CompletionEvent so the
	// lifecycle retains the actual terminal model and encoders see it
	// before their completion validation. Drift after the model is
	// known stays a value-free 502, identical to the progress path.
	if string(resp.ID) != d.respID {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream stream changed its response identity"}
	}
	var evs []ResponseEvent
	if d.model == "" {
		// The terminal validator requires an actual model, so resp.Model
		// is nonempty here by construction.
		d.model = resp.Model
		evs = append(evs, ResponseModelEvent{Model: resp.Model})
	} else if resp.Model != d.model {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream stream changed its response identity"}
	}
	for _, idx := range d.order {
		if !d.slots[idx].itemDone {
			return nil, &RouterError{Status: 502, Code: CodeUpstreamBad,
				Msg: "Upstream stream ended with unfinished content"}
		}
	}
	if rerr := d.checkTerminalOutput(resp); rerr != nil {
		return nil, rerr
	}
	evs = append(evs, UsageEvent{Usage: resp.Usage})
	complete, rerr := NewCompletionEvent(resp.Finish)
	if rerr != nil {
		return nil, rerr
	}
	evs = append(evs, complete)
	d.done = true
	return evs, nil
}

// checkTerminalOutput proves the shared-validator terminal output agrees
// with the streamed blocks: same valid blocks in arrival order (malformed
// tools omitted, empty text spans dropped per the complete-row policy),
// hash/identity/argument agreement, and matching malformed counts. Raw
// provider lineage (positions, item IDs) was already proven by
// checkTerminalLineage before canonicalization; this compares the
// canonical items the shared validator synthesized.
func (d *ResponsesStreamDecoder) checkTerminalOutput(resp Response) *RouterError {
	contradictory := &RouterError{Status: 502, Code: CodeUpstreamBad,
		Msg: "Upstream stream sent contradictory terminal output"}
	if resp.MalformedTools != d.malformed {
		return contradictory
	}
	pos := 0
	for _, idx := range d.order {
		s := d.slots[idx]
		if (s.kind == "tool" || s.kind == "custom") && s.malformedTool() {
			continue // omitted from terminal output, counted above
		}
		if s.kind == "message" && s.textLen == 0 {
			continue // complete row drops empty spans
		}
		if s.kind == "reasoning" {
			continue // lossy private reasoning is dropped from the IR
		}
		if pos >= len(resp.Items) {
			return contradictory
		}
		switch s.kind {
		case "message":
			var want string
			if s.partKind == "refusal" {
				ri, ok := resp.Items[pos].(RefusalItem)
				if !ok {
					return contradictory
				}
				want = ri.Text
			} else {
				ti, ok := resp.Items[pos].(TextItem)
				if !ok {
					return contradictory
				}
				want = ti.Text
			}
			h := sha256.Sum256([]byte(want))
			if h != s.doneHash || len(want) != s.doneLen {
				return contradictory
			}
		case "tool":
			tc, ok := resp.Items[pos].(ToolCallItem)
			if !ok || string(tc.CallID) != s.callID || tc.Name != s.name {
				return contradictory
			}
			raw := s.args.String()
			if raw == "" {
				raw = "{}"
			}
			compacted, rerr := CompactArgs(json.RawMessage(raw))
			if rerr != nil || string(compacted) != string(tc.Arguments) {
				return contradictory
			}
		case "custom":
			cc, ok := resp.Items[pos].(CustomCallItem)
			if !ok || string(cc.CallID) != s.callID || cc.Name != s.name {
				return contradictory
			}
			if cc.Input != s.args.String() {
				return contradictory
			}
		default:
			return contradictory
		}
		pos++
	}
	if pos != len(resp.Items) {
		return contradictory
	}
	return nil
}
