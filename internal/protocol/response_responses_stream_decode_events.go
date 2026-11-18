// Responses source stream codec: delta/done handlers.
//
// This file owns the content-part, text/args delta, and done-marker
// validation for ResponsesStreamDecoder (see
// response_responses_stream_decode.go for the wire contract, provenance,
// and divergence log). Item-done and terminal validation live in
// response_responses_stream_decode_terminal.go. Splitting keeps each
// module under the migration module-map bound; no behavior lives here
// that the header file does not describe.
package protocol

import (
	"crypto/sha256"
	"encoding/json"
)

func (d *ResponsesStreamDecoder) onPartAdded(body map[string]json.RawMessage) ([]ResponseEvent, *RouterError) {
	if _, rerr := responsesStreamObject(mustRaw(body), map[string]bool{
		"type": true, "sequence_number": true, "output_index": true,
		"item_id": true, "content_index": true, "part": true,
	}, "response.content_part.added"); rerr != nil {
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
	if !ok || s.kind != "message" {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream stream sent a delta for an unknown block"}
	}
	rawIID, ok := body["item_id"]
	if !ok || isNull(rawIID) {
		return nil, rsBad()
	}
	var iid string
	if err := json.Unmarshal(rawIID, &iid); err != nil || iid != s.itemID {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream returned conflicting response item identities"}
	}
	// Exactly one part per message item: content_index must be zero and
	// a second part for the same item is a duplicate (merging would
	// silently alter block structure; r2g refused nonzero indices).
	if rawCI, ok := body["content_index"]; !ok || isNull(rawCI) {
		return nil, rsBad()
	} else if ci, rerr := responseInt(rawCI); rerr != nil || ci != 0 {
		return nil, rsBad()
	}
	if s.partKind != "" {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream stream sent duplicate done markers"}
	}
	if s.itemDone {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream sent content after the finish marker"}
	}
	rawPart, ok := body["part"]
	if !ok || isNull(rawPart) {
		return nil, rsBad()
	}
	part, rerr := responsesStreamObject(rawPart, map[string]bool{
		"type": true, "text": true, "annotations": true, "logprobs": true, "refusal": true,
	}, "response.content_part.added.part")
	if rerr != nil {
		return nil, rerr
	}
	var ptype string
	if rawT, ok := part["type"]; !ok || json.Unmarshal(rawT, &ptype) != nil {
		return nil, rsBad()
	}
	var kind BlockKind
	switch ptype {
	case "output_text":
		kind = BlockText
		if rawT, ok := part["text"]; ok && !isNull(rawT) {
			var t string
			if err := json.Unmarshal(rawT, &t); err != nil || t != "" {
				return nil, rsBad()
			}
		}
	case "refusal":
		kind = BlockRefusal
		if rawR, ok := part["refusal"]; ok && !isNull(rawR) {
			var t string
			if err := json.Unmarshal(rawR, &t); err != nil || t != "" {
				return nil, rsBad()
			}
		}
	default:
		return nil, rsUnrep("response.content_part.added.part.type")
	}
	for _, k := range []string{"annotations", "logprobs"} {
		if rawV, ok := part[k]; ok && !isNull(rawV) {
			var arr []json.RawMessage
			if err := json.Unmarshal(rawV, &arr); err != nil || len(arr) > 0 {
				return nil, rsUnrep("response.content_part.added.part." + k)
			}
		}
	}
	if kind == BlockText {
		s.partKind = "text"
	} else {
		s.partKind = "refusal"
	}
	open, rerr := NewIndexedBlockOpen(ItemID(s.itemID), kind, "", "", outIdx)
	if rerr != nil {
		return nil, rerr
	}
	// The withheld identity now flows into the lifecycle's post-open
	// budget: release the decoder's pending retention.
	d.pendingIdentityBytes -= int64(s.pendingIdentityBytes)
	s.pendingIdentityBytes = 0
	return []ResponseEvent{open}, nil
}

// onTextDelta validates one output_text/refusal delta and returns its
// immediate TextFragment event. Text is never retained: bytes fold into
// the per-block hash and length only. output_text.delta carries an
// optional protocol `obfuscation` string padding (absent/null is exact):
// validated as a bounded string and discarded after validation, never
// folded into text, events, or terminal identity. Malformed or oversized
// padding fails closed. refusal.delta owns no obfuscation member.
func (d *ResponsesStreamDecoder) onTextDelta(body map[string]json.RawMessage, isRefusal bool) ([]ResponseEvent, *RouterError) {
	ev := "response.output_text.delta"
	wantKind := "text"
	if isRefusal {
		ev = "response.refusal.delta"
		wantKind = "refusal"
	}
	allowed := map[string]bool{
		"type": true, "sequence_number": true, "output_index": true,
		"content_index": true, "item_id": true, "delta": true,
		"logprobs": true, "annotations": true,
	}
	if !isRefusal {
		allowed["obfuscation"] = true
	}
	if _, rerr := responsesStreamObject(mustRaw(body), allowed, ev); rerr != nil {
		return nil, rerr
	}
	if rerr := d.checkSequence(body); rerr != nil {
		return nil, rerr
	}
	for _, k := range []string{"logprobs", "annotations"} {
		if rawV, ok := body[k]; ok && !isNull(rawV) {
			var arr []json.RawMessage
			if err := json.Unmarshal(rawV, &arr); err != nil || len(arr) > 0 {
				return nil, rsUnrep(ev + "." + k)
			}
		}
	}
	if !isRefusal {
		if rawObf, ok := body["obfuscation"]; ok && !isNull(rawObf) {
			var pad string
			if err := json.Unmarshal(rawObf, &pad); err != nil {
				return nil, rsBad()
			}
			if len(pad) > d.lim.MaxArgsChars {
				return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream response exceeds the size bound"}
			}
		}
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
	if s.kind != "message" || s.partKind != wantKind {
		return nil, rsBad()
	}
	if s.textDone || s.partDone || s.itemDone {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream sent content after the finish marker"}
	}
	if rawCI, ok := body["content_index"]; ok && !isNull(rawCI) {
		if ci, rerr := responseInt(rawCI); rerr != nil || ci != 0 {
			return nil, rsBad()
		}
	}
	rawD, ok := body["delta"]
	if !ok || isNull(rawD) {
		return nil, rsBad()
	}
	var delta string
	if err := json.Unmarshal(rawD, &delta); err != nil {
		return nil, rsBad()
	}
	if delta == "" {
		return nil, nil // legacy skip policy: empty deltas emit nothing
	}
	s.textHash.Write([]byte(delta))
	s.textLen += len(delta)
	frag, rerr := NewTextFragment(ItemID(s.itemID), delta)
	if rerr != nil {
		return nil, rerr
	}
	return []ResponseEvent{frag}, nil
}

func (d *ResponsesStreamDecoder) onTextDone(body map[string]json.RawMessage, isRefusal bool) *RouterError {
	ev := "response.output_text.done"
	wantKind := "text"
	key := "text"
	if isRefusal {
		ev = "response.refusal.done"
		wantKind = "refusal"
		key = "refusal"
	}
	if _, rerr := responsesStreamObject(mustRaw(body), map[string]bool{
		"type": true, "sequence_number": true, "output_index": true,
		"content_index": true, "item_id": true, "text": true,
		"refusal": true, "logprobs": true, "annotations": true,
	}, ev); rerr != nil {
		return rerr
	}
	if rerr := d.checkSequence(body); rerr != nil {
		return rerr
	}
	for _, k := range []string{"logprobs", "annotations"} {
		if rawV, ok := body[k]; ok && !isNull(rawV) {
			var arr []json.RawMessage
			if err := json.Unmarshal(rawV, &arr); err != nil || len(arr) > 0 {
				return rsUnrep(ev + "." + k)
			}
		}
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
	if s.kind != "message" || s.partKind != wantKind {
		return rsBad()
	}
	if s.itemDone {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream sent content after the finish marker"}
	}
	if s.textDone {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream stream sent duplicate done markers"}
	}
	if rawCI, ok := body["content_index"]; ok && !isNull(rawCI) {
		if ci, rerr := responseInt(rawCI); rerr != nil || ci != 0 {
			return rsBad()
		}
	}
	rawDone, ok := body[key]
	if !ok || isNull(rawDone) {
		return rsBad()
	}
	var doneText string
	if err := json.Unmarshal(rawDone, &doneText); err != nil {
		return rsBad()
	}
	// Terminal consistency without retention: the done payload must equal
	// the accumulated deltas (bounded rolling SHA256 plus length).
	h := sha256.Sum256([]byte(doneText))
	var cur [32]byte
	copy(cur[:], s.textHash.Sum(nil))
	if h != cur || len(doneText) != s.textLen {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream terminal output contradicts streamed deltas"}
	}
	s.textDone = true
	s.doneHash, s.doneLen = h, len(doneText)
	return nil
}

func (d *ResponsesStreamDecoder) onArgsDelta(body map[string]json.RawMessage) ([]ResponseEvent, *RouterError) {
	ev := "response.function_call_arguments.delta"
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
	if s.kind != "tool" {
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
	var frag string
	if err := json.Unmarshal(rawD, &frag); err != nil {
		return nil, rsBad()
	}
	if frag == "" {
		return nil, nil // legacy skip policy
	}
	if s.argsLen+len(frag) > d.lim.MaxArgsChars ||
		d.totalArgs+len(frag) > d.lim.MaxStreamArgsChars {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call exceeds the size bound"}
	}
	s.args.WriteString(frag)
	s.argsLen += len(frag)
	d.totalArgs += len(frag)
	out, rerr := NewArgsFragment(ItemID(s.itemID), frag)
	if rerr != nil {
		return nil, rerr
	}
	return []ResponseEvent{out}, nil
}

func (d *ResponsesStreamDecoder) onArgsDone(body map[string]json.RawMessage) *RouterError {
	ev := "response.function_call_arguments.done"
	if _, rerr := responsesStreamObject(mustRaw(body), map[string]bool{
		"type": true, "sequence_number": true, "output_index": true,
		"item_id": true, "arguments": true,
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
	if s.kind != "tool" {
		return rsBad()
	}
	if s.argsDone || s.itemDone {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream stream sent duplicate done markers"}
	}
	rawA, ok := body["arguments"]
	if !ok || isNull(rawA) {
		return rsBad()
	}
	var finalArgs string
	if err := json.Unmarshal(rawA, &finalArgs); err != nil {
		return rsBad()
	}
	// The done arguments must agree with the accumulated deltas
	// canonically (whitespace-insensitive, numeric-preserving); otherwise
	// the stream contradicts itself. Malformed payloads fall back to raw
	// exactness and classify later at output_item.done.
	if !responsesCanonicalArgsEqual(finalArgs, s.args.String()) {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream stream sent contradictory tool arguments"}
	}
	s.argsDone = true
	return nil
}

func (d *ResponsesStreamDecoder) onPartDone(body map[string]json.RawMessage) *RouterError {
	ev := "response.content_part.done"
	if _, rerr := responsesStreamObject(mustRaw(body), map[string]bool{
		"type": true, "sequence_number": true, "output_index": true,
		"content_index": true, "item_id": true, "part": true,
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
	if s.kind != "message" {
		return rsBad()
	}
	if s.itemDone {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream sent content after the finish marker"}
	}
	if s.partDone {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream stream sent duplicate done markers"}
	}
	if rawCI, ok := body["content_index"]; ok && !isNull(rawCI) {
		if ci, rerr := responseInt(rawCI); rerr != nil || ci != 0 {
			return rsBad()
		}
	}
	// The full part payload must cohere with the opened kind and the
	// accumulated text: part.done carries the complete part text (not
	// an empty echo), validated by hash/length against the text.done
	// marker. Populated annotations/logprobs fail as unrepresentable.
	// The text.done marker must precede part.done in stream order.
	if !s.textDone {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream stream ended with unfinished content"}
	}
	rawPart, ok := body["part"]
	if !ok || isNull(rawPart) {
		return rsBad()
	}
	partObj, rerr := responsesStreamObject(rawPart, map[string]bool{
		"type": true, "text": true, "refusal": true,
		"annotations": true, "logprobs": true,
	}, ev+".part")
	if rerr != nil {
		return rerr
	}
	var ptype string
	if rawT, ok := partObj["type"]; !ok || json.Unmarshal(rawT, &ptype) != nil {
		return rsBad()
	}
	var full string
	switch ptype {
	case "output_text":
		if s.partKind != "text" {
			return rsBad()
		}
		rawT, ok := partObj["text"]
		if !ok || isNull(rawT) {
			return rsBad()
		}
		if err := json.Unmarshal(rawT, &full); err != nil {
			return rsBad()
		}
		if rawR, ok := partObj["refusal"]; ok && !isNull(rawR) {
			return rsBad()
		}
	case "refusal":
		if s.partKind != "refusal" {
			return rsBad()
		}
		rawR, ok := partObj["refusal"]
		if !ok || isNull(rawR) {
			return rsBad()
		}
		if err := json.Unmarshal(rawR, &full); err != nil {
			return rsBad()
		}
		if rawT, ok := partObj["text"]; ok && !isNull(rawT) {
			return rsBad()
		}
	default:
		return rsUnrep(ev + ".part.type")
	}
	for _, k := range []string{"annotations", "logprobs"} {
		if rawV, ok := partObj[k]; ok && !isNull(rawV) {
			var arr []json.RawMessage
			if err := json.Unmarshal(rawV, &arr); err != nil || len(arr) > 0 {
				return rsUnrep(ev + ".part." + k)
			}
		}
	}
	h := sha256.Sum256([]byte(full))
	if h != s.doneHash || len(full) != s.doneLen {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream terminal output contradicts streamed deltas"}
	}
	s.partDone = true
	return nil
}
