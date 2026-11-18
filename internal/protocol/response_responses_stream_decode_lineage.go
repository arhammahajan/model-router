// Responses source stream codec: terminal raw lineage.
//
// This file owns the pre-canonicalization raw wire lineage checks for
// ResponsesStreamDecoder terminals (array positions against source
// output_index, provider item/call identities, kinds, roles, names,
// arguments and content hashes). See response_responses_stream_decode.go
// for the wire contract and divergence log.
package protocol

import (
	"crypto/sha256"
	"encoding/json"
)

// checkTerminalLineage proves the raw terminal output array agrees with
// the streamed source lineage before canonicalization: array position p
// must hold the item added with output_index p, with matching provider
// item/call identities, kinds, roles, statuses, names, arguments and
// content (hash-compared, never retained). Empty message spans and
// malformed tools are present in the raw wire and validated here; the
// canonical validator later omits them by policy. Malformed argument
// payloads and grammar-invalid names are lineaged by identity only, so
// the typed malformed classification survives canonicalization.
func (d *ResponsesStreamDecoder) checkTerminalLineage(rawResp json.RawMessage) *RouterError {
	contradictory := &RouterError{Status: 502, Code: CodeUpstreamBad,
		Msg: "Upstream stream sent contradictory terminal output"}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(rawResp, &top); err != nil || top == nil {
		return rsBad()
	}
	rawOut, ok := top["output"]
	if !ok || isNull(rawOut) {
		return rsBad()
	}
	var items []json.RawMessage
	if err := json.Unmarshal(rawOut, &items); err != nil || items == nil {
		return rsBad()
	}
	if len(items) != len(d.slots) {
		return contradictory
	}
	for p, raw := range items {
		s, ok := d.slots[p]
		if !ok {
			return contradictory
		}
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil || probe.Type == "" {
			return rsBad()
		}
		switch s.kind {
		case "message":
			if probe.Type != "message" {
				return contradictory
			}
			if rerr := checkRawMessageLineage(raw, s, d.allowPrivateDrop); rerr != nil {
				return rerr
			}
		case "tool":
			if probe.Type != "function_call" {
				return contradictory
			}
			if rerr := checkRawToolLineage(raw, s); rerr != nil {
				return rerr
			}
		case "custom":
			if probe.Type != "custom_tool_call" {
				return contradictory
			}
			if rerr := checkRawCustomLineage(raw, s); rerr != nil {
				return rerr
			}
		case "reasoning":
			if probe.Type != "reasoning" {
				return contradictory
			}
			if rerr := checkRawReasoningLineage(raw, s); rerr != nil {
				return rerr
			}
		default:
			return contradictory
		}
	}
	return nil
}

// checkRawMessageLineage validates one raw terminal message item against
// its streamed slot: provider id, assistant role, completed/incomplete
// status, and content text hash agreement. Empty spans must carry empty
// content; populated annotations/logprobs fail as unrepresentable.
func checkRawMessageLineage(raw json.RawMessage, s *responsesStreamSlot, allowLossy bool) *RouterError {
	contradictory := &RouterError{Status: 502, Code: CodeUpstreamBad,
		Msg: "Upstream stream sent contradictory terminal output"}
	item, rerr := responsesStreamObject(raw, map[string]bool{
		"type": true, "id": true, "status": true, "role": true, "content": true, "phase": true,
	}, "response.output")
	if rerr != nil {
		return rerr
	}
	if rerr := validateResponsesMessagePhase(item["phase"], "response.output.phase", allowLossy, &s.messagePhase); rerr != nil {
		return rerr
	}
	var id string
	if rawID, ok := item["id"]; !ok || json.Unmarshal(rawID, &id) != nil || id != s.itemID {
		return contradictory
	}
	if rawRole, ok := item["role"]; !ok || isNull(rawRole) {
		return rsBad()
	} else {
		var role string
		if err := json.Unmarshal(rawRole, &role); err != nil || role != "assistant" {
			return rsBad()
		}
	}
	var status string
	if rawSt, ok := item["status"]; !ok || json.Unmarshal(rawSt, &status) != nil || status != s.doneStatus {
		return contradictory
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
		// Empty spans are present in the raw wire but contribute
		// nothing downstream: allow an empty array or one empty part
		// of the opened kind (both shapes are dropped by the
		// canonical complete row).
		if len(parts) == 0 {
			return nil
		}
		if len(parts) != 1 {
			return contradictory
		}
		part, rerr := responsesStreamObject(parts[0], map[string]bool{
			"type": true, "text": true, "annotations": true, "logprobs": true,
			"refusal": true,
		}, "response.output.content[0]")
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
				return contradictory
			}
			if rawT, ok := part["text"]; !ok || isNull(rawT) {
				return rsBad()
			} else {
				var t string
				if json.Unmarshal(rawT, &t) != nil || t != "" {
					return contradictory
				}
			}
		case "refusal":
			if s.partKind != "refusal" {
				return contradictory
			}
			if rawR, ok := part["refusal"]; !ok || isNull(rawR) {
				return rsBad()
			} else {
				var t string
				if json.Unmarshal(rawR, &t) != nil || t != "" {
					return contradictory
				}
			}
		default:
			return rsUnrep("response.output.content[0].type")
		}
		return nil
	}
	if len(parts) != 1 {
		return contradictory
	}
	part, rerr := responsesStreamObject(parts[0], map[string]bool{
		"type": true, "text": true, "annotations": true, "logprobs": true,
		"refusal": true,
	}, "response.output.content[0]")
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
			return contradictory
		}
		rawT, ok := part["text"]
		if !ok || isNull(rawT) || json.Unmarshal(rawT, &text) != nil {
			return rsBad()
		}
	case "refusal":
		if s.partKind != "refusal" {
			return contradictory
		}
		rawR, ok := part["refusal"]
		if !ok || isNull(rawR) || json.Unmarshal(rawR, &text) != nil {
			return rsBad()
		}
	default:
		return rsUnrep("response.output.content[0].type")
	}
	for _, k := range []string{"annotations", "logprobs"} {
		if rawV, ok := part[k]; ok && !isNull(rawV) {
			var arr []json.RawMessage
			if err := json.Unmarshal(rawV, &arr); err != nil || len(arr) > 0 {
				return rsUnrep("response.output.content[0]." + k)
			}
		}
	}
	h := sha256.Sum256([]byte(text))
	if h != s.doneHash || len(text) != s.doneLen {
		return contradictory
	}
	return nil
}

// checkRawToolLineage validates one raw terminal function_call item
// against its streamed slot: provider item/call identities, in-progress
// vs completed status, name exactness, and argument exactness (with the
// source-absent "" canonicalizing to "{}", exactly like the complete
// row). Malformed tools (grammar-invalid names, non-object arguments)
// are lineaged by identity only: name equality still holds (the
// malformed name is the source fact), while argument bytes are compared
// exactly so a contradictory terminal cannot hide behind the
// classification.
func checkRawToolLineage(raw json.RawMessage, s *responsesStreamSlot) *RouterError {
	contradictory := &RouterError{Status: 502, Code: CodeUpstreamBad,
		Msg: "Upstream stream sent contradictory terminal output"}
	item, rerr := responsesStreamObject(raw, map[string]bool{
		"type": true, "id": true, "call_id": true, "name": true,
		"arguments": true, "status": true, "namespace": true,
		"caller": true, "async": true,
	}, "response.output")
	if rerr != nil {
		return rerr
	}
	for _, k := range []string{"namespace", "caller"} {
		if rawV, ok := item[k]; ok && !isNull(rawV) {
			return rsUnrep("response.output." + k)
		}
	}
	if rawA, ok := item["async"]; ok && !isNull(rawA) {
		var bAsync bool
		if err := json.Unmarshal(rawA, &bAsync); err != nil {
			return rsBad()
		}
		if bAsync {
			return rsUnrep("response.output.async")
		}
	}
	var id string
	if rawID, ok := item["id"]; !ok || json.Unmarshal(rawID, &id) != nil || id != s.itemID {
		return contradictory
	}
	var callID string
	if rawC, ok := item["call_id"]; !ok || json.Unmarshal(rawC, &callID) != nil || callID != s.callID {
		return contradictory
	}
	var status string
	if rawSt, ok := item["status"]; !ok || json.Unmarshal(rawSt, &status) != nil || status != s.doneStatus {
		return contradictory
	}
	var name string
	if rawN, ok := item["name"]; !ok || json.Unmarshal(rawN, &name) != nil || name != s.name {
		return contradictory
	}
	rawArgs, ok := item["arguments"]
	if !ok || isNull(rawArgs) {
		if s.args.String() != "" {
			return contradictory
		}
		return nil
	}
	var argsStr string
	if err := json.Unmarshal(rawArgs, &argsStr); err != nil {
		return rsBad()
	}
	// Source-absent arguments canonicalize to "{}": an empty
	// accumulation agrees with either "" or "{}" on the wire. Valid
	// objects compare canonically (whitespace-insensitive,
	// numeric-preserving); malformed payloads compare raw so the typed
	// classification survives.
	if !responsesCanonicalArgsEqual(argsStr, s.args.String()) {
		return contradictory
	}
	return nil
}

// checkRawCustomLineage validates one raw terminal custom_tool_call item
// against its streamed slot: provider item/call identities, status,
// name exactness and input exactness.
func checkRawCustomLineage(raw json.RawMessage, s *responsesStreamSlot) *RouterError {
	contradictory := &RouterError{Status: 502, Code: CodeUpstreamBad,
		Msg: "Upstream stream sent contradictory terminal output"}
	item, rerr := responsesStreamObject(raw, map[string]bool{
		"type": true, "id": true, "call_id": true, "name": true,
		"input": true, "status": true, "namespace": true,
		"caller": true,
	}, "response.output")
	if rerr != nil {
		return rerr
	}
	for _, k := range []string{"namespace", "caller"} {
		if rawV, ok := item[k]; ok && !isNull(rawV) {
			return rsUnrep("response.output." + k)
		}
	}
	var id string
	if rawID, ok := item["id"]; !ok || json.Unmarshal(rawID, &id) != nil || id != s.itemID {
		return contradictory
	}
	var callID string
	if rawC, ok := item["call_id"]; !ok || json.Unmarshal(rawC, &callID) != nil || callID != s.callID {
		return contradictory
	}
	var status string
	if rawSt, ok := item["status"]; !ok || json.Unmarshal(rawSt, &status) != nil || status != s.doneStatus {
		return contradictory
	}
	var name string
	if rawN, ok := item["name"]; !ok || json.Unmarshal(rawN, &name) != nil || name != s.name {
		return contradictory
	}
	rawIn, ok := item["input"]
	if !ok || isNull(rawIn) {
		if s.args.String() != "" {
			return contradictory
		}
		return nil
	}
	input, valid := decodeUnicodeString(rawIn)
	if !valid {
		return rsBad()
	}
	if input != s.args.String() {
		return contradictory
	}
	return nil
}
