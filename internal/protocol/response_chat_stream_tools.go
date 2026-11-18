// Chat source stream codec: tool-fragment accumulation.
//
// This file owns the Chat tool_calls[].index slot table: sparse/parallel
// arrival buffering under the args bounds, BlockOpen emission once
// identity (id+name) completes, and immediate ArgsFragment forwarding.
// It performs no downstream writes and enforces no destination policy;
// terminal assembly (empty-object fallback, malformed classification,
// [DONE] closing) lives in response_chat_stream_decode.go.
package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
)

// chatStreamSlot accumulates one Chat tool call keyed by source index.
// Argument bytes buffer under the Limits bounds (the decoder's only
// retention); pre-identity bytes flush once the block opens.
type chatStreamSlot struct {
	idx                  int
	id                   string
	name                 string
	args                 strings.Builder
	argsLen              int
	pendingIdentityBytes int
	emitted              bool
}

// chatStreamToolItem maps a source tool index to a stable ItemID. The index
// stays visible so sparse/parallel arrivals keep explicit order; targets own
// any reordering.
func chatStreamToolItem(idx int) ItemID {
	return ItemID(fmt.Sprintf("c%d", idx))
}

// emitText opens the single text block on first content and appends the
// fragment. Text is never retained.
func (d *ChatStreamDecoder) emitText(s string) ([]ResponseEvent, *RouterError) {
	var evs []ResponseEvent
	if !d.textOpen {
		if d.blocks+1 > d.lim.MaxStreamBlocks {
			return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream exceeds the content block bound"}
		}
		open, rerr := NewBlockOpen("t0", BlockText, "", "")
		if rerr != nil {
			return nil, rerr
		}
		evs = append(evs, open)
		d.textOpen = true
		d.blocks++
	}
	frag, rerr := NewTextFragment("t0", s)
	if rerr != nil {
		return nil, rerr
	}
	return append(evs, frag), nil
}

// handleToolFragment accumulates one tool_calls delta. BlockOpen emits only
// once id+name are complete; earlier argument bytes stay buffered under the
// args bounds and flush once identity resolves.
func (d *ChatStreamDecoder) handleToolFragment(raw json.RawMessage) ([]ResponseEvent, *RouterError) {
	entry, rerr := chatStreamObject(raw, "chunk.choices[0].delta.tool_calls", map[string]bool{
		"index": true, "id": true, "type": true, "function": true,
	})
	if rerr != nil {
		return nil, rerr
	}
	rawIdx, ok := entry["index"]
	if !ok || isNull(rawIdx) {
		return nil, chatStreamBad()
	}
	idx, rerr := streamIndex(rawIdx)
	if rerr != nil {
		return nil, chatStreamBad()
	}
	slot, ok := d.slots[idx]
	if !ok {
		if len(d.slots)+1 > d.lim.MaxToolCalls {
			return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call count exceeds the size bound"}
		}
		slot = &chatStreamSlot{idx: idx}
		d.slots[idx] = slot
		d.order = append(d.order, idx)
	}
	if rawID, ok := entry["id"]; ok && !isNull(rawID) {
		var id string
		if err := json.Unmarshal(rawID, &id); err != nil {
			return nil, chatStreamBad()
		}
		if id != "" {
			if !validOpaqueID(id) {
				return nil, chatStreamBad()
			}
			if len(id) > d.lim.MaxArgsChars {
				return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call exceeds the size bound"}
			}
			if slot.id == "" {
				slot.id = id
			} else if slot.id != id {
				return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned conflicting tool call identities"}
			}
		}
	}
	var fragName, fragArgs string
	if rawFn, ok := entry["function"]; ok && !isNull(rawFn) {
		fn, rerr := chatStreamObject(rawFn, "chunk.choices[0].delta.tool_calls.function", map[string]bool{
			"name": true, "arguments": true,
		})
		if rerr != nil {
			return nil, rerr
		}
		if rawN, ok := fn["name"]; ok && !isNull(rawN) {
			if err := json.Unmarshal(rawN, &fragName); err != nil {
				return nil, chatStreamBad()
			}
		}
		if rawA, ok := fn["arguments"]; ok && !isNull(rawA) {
			if err := json.Unmarshal(rawA, &fragArgs); err != nil {
				return nil, chatStreamBad()
			}
		}
	}
	if rawT, ok := entry["type"]; ok && !isNull(rawT) {
		var typ string
		if err := json.Unmarshal(rawT, &typ); err != nil || (typ != "" && typ != "function") {
			return nil, chatStreamUnrep("chunk.choices[0].delta.tool_calls.type")
		}
	}
	if fragName != "" {
		// Fragmented names have no mapping: a differing name conflicts.
		if len(fragName) > d.lim.MaxArgsChars {
			return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call exceeds the size bound"}
		}
		if strings.TrimSpace(fragName) == "" {
			return nil, chatStreamBad()
		}
		if slot.name == "" {
			slot.name = fragName
		} else if slot.name != fragName {
			return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned conflicting tool call identities"}
		}
	}
	// Completed identities flow immediately into the lifecycle's aggregate
	// identity budget. Bound unfinished identities separately while their
	// BlockOpen is withheld, so missing names/IDs cannot bypass retention.
	pending := 0
	if slot.id == "" || slot.name == "" {
		pending = len(slot.id) + len(slot.name)
	}
	next, rerr := addBytes(d.pendingIdentityBytes-int64(slot.pendingIdentityBytes), pending)
	if rerr != nil || next > int64(d.lim.MaxStreamArgsChars) {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream response exceeds the size bound"}
	}
	d.pendingIdentityBytes, slot.pendingIdentityBytes = next, pending
	var evs []ResponseEvent
	if fragArgs != "" {
		if slot.argsLen+len(fragArgs) > d.lim.MaxArgsChars ||
			d.totalArgs+len(fragArgs) > d.lim.MaxStreamArgsChars {
			return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call exceeds the size bound"}
		}
		slot.args.WriteString(fragArgs)
		slot.argsLen += len(fragArgs)
		d.totalArgs += len(fragArgs)
		if slot.emitted {
			frag, rerr := NewArgsFragment(chatStreamToolItem(idx), fragArgs)
			if rerr != nil {
				return nil, rerr
			}
			evs = append(evs, frag)
		}
	}
	if slot.id != "" && slot.name != "" && !slot.emitted {
		for _, otherIdx := range d.order {
			other := d.slots[otherIdx]
			if other != slot && other.emitted && other.id == slot.id {
				return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned conflicting tool call identities"}
			}
		}
		if d.blocks+1 > d.lim.MaxStreamBlocks {
			return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream exceeds the content block bound"}
		}
		open, rerr := NewToolBlockOpen(chatStreamToolItem(idx), CallID(slot.id), slot.name, idx)
		if rerr != nil {
			return nil, rerr
		}
		evs = append(evs, open)
		slot.emitted = true
		d.blocks++
		if buffered := slot.args.String(); buffered != "" {
			frag, rerr := NewArgsFragment(chatStreamToolItem(idx), buffered)
			if rerr != nil {
				return nil, rerr
			}
			evs = append(evs, frag)
		}
	}
	return evs, nil
}
