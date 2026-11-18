package protocol

import "encoding/json"

// Emulated custom stream restoration: buffers upstream function argument
// bytes for Responses custom tools and re-emits them as typed custom
// events. Text stays incremental; only emulated tool bytes are buffered,
// under the existing per-field and aggregate caps, until a valid closed
// call decodes. Malformed wrappers, mismatched native customs, order
// violations, oversize chunks and unfinished streams fail closed as
// terminal protocol errors, never fabricated completions.

type emuStreamBuf struct {
	source     ToolIdentity
	call       CallID
	sourceIdx  *int
	callOrigin IdentityOrigin
	data       []byte
}

type emulatedStreamDecoder struct {
	inner StreamDecoder
	plan  *ToolPlan
	lim   Limits
	bufs  map[ItemID]*emuStreamBuf
	total int
}

func (d *emulatedStreamDecoder) DecodeFrame(frame Frame) ([]ResponseEvent, *RouterError) {
	events, rerr := d.inner.DecodeFrame(frame)
	if rerr != nil {
		return nil, rerr
	}
	return d.restore(events)
}

func (d *emulatedStreamDecoder) Close() ([]ResponseEvent, *RouterError) {
	events, rerr := d.inner.Close()
	if rerr != nil {
		return nil, rerr
	}
	if len(d.bufs) > 0 {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream ended without completion"}
	}
	if len(events) == 0 {
		return events, nil
	}
	return d.restore(events)
}

func (d *emulatedStreamDecoder) restore(events []ResponseEvent) ([]ResponseEvent, *RouterError) {
	out := make([]ResponseEvent, 0, len(events))
	for _, ev := range events {
		switch v := ev.(type) {
		case BlockOpen:
			switch v.Kind {
			case BlockTool:
				if src, ok := d.plan.emuCustom[v.Name]; ok {
					if _, dup := d.bufs[v.Item]; dup {
						return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream opened a duplicate response block"}
					}
					open, rerr := NewBlockOpenWithNamespace(v.Item, BlockCustom, v.Call, src.Namespace, src.Name)
					if rerr != nil {
						return nil, rerr
					}
					open.SourceIndex = v.SourceIndex
					open.CallOrigin = v.CallOrigin
					d.bufs[v.Item] = &emuStreamBuf{source: src, call: v.Call, sourceIdx: v.SourceIndex, callOrigin: v.CallOrigin}
					out = append(out, open)
					continue
				}
				// Namespace function restoration (existing behavior).
				if src, bound := d.plan.byAlias[v.Name]; bound && src.Namespace != "" {
					restored, rerr := NewBlockOpenWithNamespace(v.Item, v.Kind, v.Call, src.Namespace, src.Name)
					if rerr != nil {
						return nil, rerr
					}
					restored.SourceIndex = v.SourceIndex
					restored.CallOrigin = v.CallOrigin
					out = append(out, restored)
					continue
				}
				out = append(out, ev)
			case BlockCustom:
				// Upstream dialects with emulation never emit native customs.
				return nil, &RouterError{Status: 502, Code: CodeUpstreamBad,
					Msg: "Upstream response feature \"response.output.custom_tool_call\" has no representation"}
			default:
				out = append(out, ev)
			}
		case ArgsFragment:
			if buf, ok := d.bufs[v.Item]; ok {
				_ = buf
				if len(v.Fragment) > d.lim.MaxArgsChars-len(buf.data) || d.total+len(v.Fragment) > d.lim.MaxStreamArgsChars {
					return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call exceeds the size bound"}
				}
				buf.data = append(buf.data, v.Fragment...)
				d.total += len(v.Fragment)
				continue
			}
			out = append(out, ev)
		case CustomDelta:
			// Upstream never emits custom deltas on emulated paths.
			return nil, &RouterError{Status: 502, Code: CodeUpstreamBad,
				Msg: "Upstream response feature \"response.output.custom_tool_call\" has no representation"}
		case MalformedToolEvent:
			// Pass through: the target encoder refuses malformed tools.
			// Buffered emulated items stay open until BlockClose; do not emit extra.
			out = append(out, ev)
		case BlockClose:
			if buf, ok := d.bufs[v.Item]; ok {
				input, rerr := decodeCustomArgs(jsonRaw(buf.data), d.lim)
				if rerr != nil {
					return nil, rerr
				}
				delta, rerr := NewCustomDelta(v.Item, input)
				if rerr != nil {
					return nil, rerr
				}
				out = append(out, delta)
				out = append(out, ev)
				delete(d.bufs, v.Item)
				continue
			}
			out = append(out, ev)
		default:
			out = append(out, ev)
		}
	}
	return out, nil
}

func jsonRaw(b []byte) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage("{}")
	}
	return json.RawMessage(append(json.RawMessage(nil), b...))
}
