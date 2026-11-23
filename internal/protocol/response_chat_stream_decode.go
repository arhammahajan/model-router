// Chat source stream codec: one SSE decoder for the Chat row.
//
// ChatStreamDecoder validates the Chat Completions SSE source ONCE,
// independently of destination, and emits canonical semantic events for the
// shared lifecycle (ResponseLifecycle) and the single pipeline
// (TranslateStream). It performs no downstream writes, mints no downstream
// IDs, and enforces no destination policy: target differences (Anthropic
// start-zero placeholders, refusal rendering, name grammar, usage detail
// breakout) live in target encoders via CheckRepresentable, never here.
//
// Wire contract (proven against the three replaced pair translators in
// chat_to_anthropic_response.go, chat_to_responses_response.go and
// chat_to_gemini_stream.go, which this decoder does NOT call):
//
//   - Framing is owned by FrameSource (NewSSEFrameReader). Frame.Event is
//     preserved but ignored: Chat streams carry no event names and all
//     three legacy translators ignore it. Frame.Data carries one chunk;
//     "[DONE]" is the Chat-source terminal ONLY.
//   - Event-only frames (HasEvent, empty Data; e.g. ping) are keepalives.
//     Empty data-only frames are malformed (legacy JSON-parse failure).
//   - A chunk is a chat.completion.chunk object under the complete-row
//     envelope allowlist; unknown members fail closed with a value-free
//     source path. object must read "chat.completion.chunk" when present
//     (Gemini policy; the other streams skipped the check: tightened here).
//   - Response id/model are tracked for stability: first values define the
//     generation, later differing values conflict. The first chunk must
//     carry both (the other streams ignored upstream id/model entirely:
//     requiring them restores provenance).
//   - choices carries zero or one entries. More than one is a
//     multiple-choices failure (no target renders parallel choices). Zero
//     entries means a usage-only chunk, which is valid only as the terminal
//     accounting chunk (non-null usage after the finish marker).
//   - Text deltas emit immediately as TextFragment events under a single
//     stable "t0" block; text is never retained. Empty content deltas emit
//     nothing (legacy skip policy).
//   - Tool fragments key on tool_calls[].index (sparse/parallel-safe).
//     ItemIDs encode the source index ("c<index>") so sparse order stays
//     explicit; arrival order is preserved, never sorted (the Gemini
//     translator sorted ascending at the terminal; this decoder keeps wire
//     order and leaves any reordering to target encoders).
//   - Call IDs are opaque (validOpaqueID + MaxArgsChars, as the complete
//     row); the legacy streams imposed the 1..64 name grammar on IDs too.
//     Names stay nonempty/bounded; their grammar is a target gate, never a
//     source refusal. Fragmented names are NOT concatenated: a differing
//     non-empty name for a known index conflicts (legacy policy).
//   - BlockOpen emits only once identity (id+name) is complete. Argument
//     fragments arriving before identity are buffered under the per-call
//     (MaxArgsChars) and aggregate (MaxStreamArgsChars) bounds and flushed
//     as ArgsFragment events once the block opens; this is the only
//     retention in the decoder (argument bytes, never text).
//   - Argument JSON classifies at the terminal: source-absent args emit
//     the canonical "{}" fallback fragment exactly once (counted in both
//     args bounds); accumulated bytes that never form one JSON object via
//     CompactArgs (bigints exact) classify the block with MalformedToolEvent
//     for the target gate (Gemini suppress-and-remap vs Anthropic/Responses
//     refuse). Syntax, identity, and size errors stay 502 *RouterError.
//   - Tool blocks carry their logical source index (tool_calls[].index) in
//     BlockOpen for reviewed target ordering; ItemIDs stay opaque.
//   - Usage is terminal-only: one object after finish, via shared
//     parseChatUsageBuckets + NewTerminalUsage/chatTextTotal. Scalar
//     reasoning, cache_write and text shares are CARRIED with Chat
//     provenance for the target gate; no target-specific refusal here.
//   - Completion needs finish + authoritative usage + [DONE] in order.
//     [DONE] emits BlockClose events, then UsageEvent, then CompletionEvent,
//     and returns promptly (no EOF read after). Close validates state, reads
//     nothing, returns no events (called BEFORE success encoding). Every
//     other failure returns *RouterError for the pipeline; FailureEvent is
//     never emitted here.
//
// Bounds (inherited, none new): per-call args MaxArgsChars, retained args
// aggregate MaxStreamArgsChars, distinct blocks MaxStreamBlocks, tool calls
// MaxToolCalls. Text is unretained and unbounded here by design (the
// lifecycle counts but never caps text bytes; incremental encoders forward
// without buffering).
package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ChatStreamDecoder decodes Chat SSE frames into canonical semantic events.
// It owns only upstream source state; it performs no downstream writes, holds
// no reader, spawns no goroutines, and enforces no destination policy. Use it
// with NewSSEFrameReader and TranslateStream.
type ChatStreamDecoder struct {
	lim Limits
	// allowPrivateDrop drops substantive provider-private
	// delta.reasoning_content and delta.reasoning (lossy compatibility).
	// False keeps the strict 502. Absent/null/empty is exact in both modes.
	allowPrivateDrop bool

	started bool
	respID  string
	model   string

	textOpen bool

	sawFinish bool
	finish    Finish

	hasUsage      bool
	usageOnFinish bool
	usage         ResponseUsage

	done bool

	slots map[int]*chatStreamSlot
	// order records first-seen source indices so emission never depends on
	// Go map iteration order.
	order []int
	// totalArgs counts retained argument bytes across all slots.
	totalArgs int
	// Pending identities have not reached lifecycle validation yet.
	pendingIdentityBytes int64
	// blocks counts emitted BlockOpen events (text block + tool blocks).
	blocks int
}

// NewChatStreamDecoder creates the Chat source stream decoder for one
// generation (strict: substantive delta.reasoning_content/delta.reasoning
// is 502).
func NewChatStreamDecoder(lim Limits) *ChatStreamDecoder {
	return &ChatStreamDecoder{lim: lim, slots: map[int]*chatStreamSlot{}}
}

// NewChatStreamDecoderWithPolicy creates the Chat source stream decoder
// with narrow lossy compatibility policy: allowPrivateDrop drops
// substantive delta.reasoning_content/delta.reasoning and, when true,
// also drops the known
// gateway usage extensions (usage.cost, usage.opencode) after bounded
// validation. False keeps the strict 502 for both.
func NewChatStreamDecoderWithPolicy(lim Limits, allowPrivateDrop bool) *ChatStreamDecoder {
	return &ChatStreamDecoder{lim: lim, allowPrivateDrop: allowPrivateDrop, slots: map[int]*chatStreamSlot{}}
}

// DecodeFrame decodes one typed SSE frame into semantic events in wire
// order. It returns promptly on [DONE] and never emits FailureEvent (all
// failures are *RouterError for the pipeline to finalize).
func (d *ChatStreamDecoder) DecodeFrame(frame Frame) ([]ResponseEvent, *RouterError) {
	if d.done {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent events after completion"}
	}
	data := frame.Data
	if data == "[DONE]" {
		return d.handleDone()
	}
	if data == "" {
		if frame.HasEvent {
			return nil, nil // event-only keepalive (e.g. ping)
		}
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	}
	return d.handleChunk(data)
}

// Close validates trailing state after the source terminal. It performs
// no reads and returns no events: success was already emitted at [DONE].
func (d *ChatStreamDecoder) Close() ([]ResponseEvent, *RouterError) {
	if !d.done {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream ended without completion"}
	}
	return nil, nil
}

func chatStreamBad() *RouterError {
	return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
}

func chatStreamUnrep(what string) *RouterError {
	return &RouterError{Status: 502, Code: CodeUpstreamBad,
		Msg: fmt.Sprintf("Upstream response feature %q has no representation", what)}
}

// handleDone validates the Chat terminal: finish and authoritative usage must
// both have arrived, every tool identity must be complete, and then the
// closing events emit in first-seen order. Argument bytes that never form
// a JSON object are classified per block with MalformedToolEvent (typed
// source provenance for the target gate), never a 502: only the later
// target decides suppress-and-remap (Gemini) vs refuse (Anthropic,
// Responses). Incomplete or conflicting identities stay 502.
func (d *ChatStreamDecoder) handleDone() ([]ResponseEvent, *RouterError) {
	if !d.sawFinish {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream ended without completion"}
	}
	if !d.hasUsage {
		return nil, chatStreamBad()
	}
	seenCalls := map[string]bool{}
	malformed := map[int]bool{}
	for _, idx := range d.order {
		slot := d.slots[idx]
		if !slot.emitted {
			return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call is not valid JSON"}
		}
		if seenCalls[slot.id] {
			return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned conflicting tool call identities"}
		}
		seenCalls[slot.id] = true
		raw := slot.args.String()
		if raw == "" {
			raw = "{}"
		}
		if _, rerr := CompactArgs(json.RawMessage(raw)); rerr != nil {
			malformed[idx] = true
		}
	}
	var evs []ResponseEvent
	if d.textOpen {
		evs = append(evs, BlockClose{Item: "t0"})
		d.textOpen = false
	}
	for _, idx := range d.order {
		slot := d.slots[idx]
		if malformed[idx] {
			evs = append(evs, MalformedToolEvent{Item: chatStreamToolItem(idx)})
		} else if slot.emitted && slot.args.String() == "" {
			// Canonical empty-object fallback: source-absent arguments
			// mean "{}", emitted exactly once before close and counted
			// in both the per-call and aggregate args bounds. Explicit
			// "{}" bytes arrived as fragments already, so this branch
			// never duplicates them.
			if slot.argsLen+2 > d.lim.MaxArgsChars ||
				d.totalArgs+2 > d.lim.MaxStreamArgsChars {
				return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call exceeds the size bound"}
			}
			slot.args.WriteString("{}")
			slot.argsLen += 2
			d.totalArgs += 2
			frag, rerr := NewArgsFragment(chatStreamToolItem(idx), "{}")
			if rerr != nil {
				return nil, rerr
			}
			evs = append(evs, frag)
		}
		evs = append(evs, BlockClose{Item: chatStreamToolItem(idx)})
	}
	evs = append(evs, UsageEvent{Usage: d.usage})
	complete, rerr := NewCompletionEvent(d.finish)
	if rerr != nil {
		return nil, rerr
	}
	evs = append(evs, complete)
	d.done = true
	return evs, nil
}

// handleChunk validates one non-[DONE] SSE data payload and returns its
// semantic events in wire order.
func (d *ChatStreamDecoder) handleChunk(data string) ([]ResponseEvent, *RouterError) {
	var chunk map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &chunk); err != nil || chunk == nil {
		return nil, chatStreamBad()
	}
	if rawErr, ok := chunk["error"]; ok && !isNull(rawErr) {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream reported an error"}
	}
	for k := range chunk {
		switch k {
		case "id", "object", "created", "model", "choices", "usage", "system_fingerprint", "service_tier":
		default:
			return nil, chatStreamUnrep("chunk." + k)
		}
	}
	var evs []ResponseEvent
	if rerr := d.observeEnvelope(chunk); rerr != nil {
		return nil, rerr
	}
	if !d.started {
		if d.respID == "" || d.model == "" {
			return nil, chatStreamBad()
		}
		evs = append(evs, ResponseStart{ID: ResponseID(d.respID), Model: d.model, Source: DialectChat})
		d.started = true
	}
	rawChoices, hasChoices := chunk["choices"]
	var choices []json.RawMessage
	if hasChoices && !isNull(rawChoices) {
		if err := json.Unmarshal(rawChoices, &choices); err != nil {
			return nil, chatStreamBad()
		}
	}
	rawUsage, hasUsage := chunk["usage"]
	if len(choices) > 1 {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned multiple choices with no representation"}
	}
	if len(choices) == 0 {
		if !hasUsage || isNull(rawUsage) {
			return nil, chatStreamBad()
		}
		if rerr := d.handleUsage(rawUsage, false, true); rerr != nil {
			return nil, rerr
		}
		return evs, nil
	}
	hadFinish := d.sawFinish
	choiceEvs, rerr := d.handleChoice(choices[0])
	if rerr != nil {
		return nil, rerr
	}
	evs = append(evs, choiceEvs...)
	if hasUsage && !isNull(rawUsage) {
		if rerr := d.handleUsage(rawUsage, !hadFinish && d.sawFinish, false); rerr != nil {
			return nil, rerr
		}
	}
	return evs, nil
}

// observeEnvelope type-checks known envelope members and tracks id/model
// stability. Empty id/model fragments are ignored (legacy skip policy);
// differing non-empty values conflict.
func (d *ChatStreamDecoder) observeEnvelope(top map[string]json.RawMessage) *RouterError {
	if rawID, ok := top["id"]; ok && !isNull(rawID) {
		var id string
		if err := json.Unmarshal(rawID, &id); err != nil {
			return chatStreamBad()
		}
		if id != "" {
			if !validOpaqueID(id) {
				return chatStreamBad()
			}
			if len(id) > d.lim.MaxArgsChars {
				return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call exceeds the size bound"}
			}
			if d.respID == "" {
				d.respID = id
			} else if d.respID != id {
				return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned conflicting response identities"}
			}
		}
	}
	if rawModel, ok := top["model"]; ok && !isNull(rawModel) {
		var m string
		if err := json.Unmarshal(rawModel, &m); err != nil {
			return chatStreamBad()
		}
		if m != "" {
			if strings.TrimSpace(m) == "" {
				return chatStreamBad()
			}
			if d.model == "" {
				d.model = m
			} else if d.model != m {
				return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned conflicting response identities"}
			}
		}
	}
	if rawObj, ok := top["object"]; ok && !isNull(rawObj) {
		var o string
		if err := json.Unmarshal(rawObj, &o); err != nil || o != "chat.completion.chunk" {
			return chatStreamBad()
		}
	}
	if rawCreated, ok := top["created"]; ok && !isNull(rawCreated) {
		if _, rerr := responseInt(rawCreated); rerr != nil {
			return chatStreamBad()
		}
	}
	for _, k := range []string{"system_fingerprint", "service_tier"} {
		if rawV, ok := top[k]; ok && !isNull(rawV) {
			var s string
			if err := json.Unmarshal(rawV, &s); err != nil {
				return chatStreamBad()
			}
		}
	}
	return nil
}

// handleUsage validates terminal accounting after the finish marker. Some
// gateways repeat counters from the finishing choice in one separate usage
// frame. Allow that echo only when its validated counters agree.
func (d *ChatStreamDecoder) handleUsage(raw json.RawMessage, finishingChoice, usageOnly bool) *RouterError {
	if !d.sawFinish {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream sent usage before completion"}
	}
	if d.hasUsage && !(d.usageOnFinish && usageOnly) {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream sent duplicate usage"}
	}
	b, rerr := parseChatUsageBucketsWithPolicy(raw, d.allowPrivateDrop)
	if rerr != nil {
		return rerr
	}
	usage, rerr := NewTerminalUsage(DialectChat, b.prompt, b.completion, b.total)
	if rerr != nil {
		return rerr
	}
	usage.CacheCreation, usage.CacheRead = b.cacheWrite, b.cached
	usage.Reasoning = b.reasoning
	text, rerr := chatTextTotal(b)
	if rerr != nil {
		return rerr
	}
	usage.TextTokens = text
	if rerr := usage.ValidateAuthoritative(); rerr != nil {
		return rerr
	}
	if d.hasUsage {
		for _, pair := range [][2]*int{
			{d.usage.Prompt, usage.Prompt}, {d.usage.Completion, usage.Completion}, {d.usage.Total, usage.Total},
			{d.usage.CacheCreation, usage.CacheCreation}, {d.usage.CacheRead, usage.CacheRead},
			{d.usage.Reasoning, usage.Reasoning}, {d.usage.TextTokens, usage.TextTokens},
		} {
			a, b := pair[0], pair[1]
			if (a == nil) != (b == nil) || (a != nil && *a != *b) {
				return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream sent conflicting usage"}
			}
		}
		d.usageOnFinish = false
		return nil
	}
	d.usage = usage
	d.hasUsage = true
	d.usageOnFinish = finishingChoice
	return nil
}

// handleChoice validates one choices[0] entry and returns its content events.
// The finish marker, when present, is recorded but its CompletionEvent waits
// for authoritative usage and [DONE].
func (d *ChatStreamDecoder) handleChoice(raw json.RawMessage) ([]ResponseEvent, *RouterError) {
	var choice map[string]json.RawMessage
	if err := json.Unmarshal(raw, &choice); err != nil || choice == nil {
		return nil, chatStreamBad()
	}
	for k := range choice {
		switch k {
		case "index", "delta", "finish_reason", "logprobs", "native_finish_reason":
		default:
			return nil, chatStreamUnrep("chunk.choices[0]." + k)
		}
	}
	if rawIdx, ok := choice["index"]; ok && !isNull(rawIdx) {
		idx, rerr := responseInt(rawIdx)
		if rerr != nil || idx != 0 {
			return nil, chatStreamBad()
		}
	}
	if rawLP, ok := choice["logprobs"]; ok && !isNull(rawLP) {
		return nil, chatStreamUnrep("chunk.choices[0].logprobs")
	}
	var finish *string
	if rawF, ok := choice["finish_reason"]; ok && !isNull(rawF) {
		var f string
		if err := json.Unmarshal(rawF, &f); err != nil {
			return nil, chatStreamBad()
		}
		finish = &f
	}
	rawDelta, ok := choice["delta"]
	if !ok || isNull(rawDelta) {
		if finish == nil {
			return nil, chatStreamBad()
		}
		if rerr := d.handleFinish(*finish); rerr != nil {
			return nil, rerr
		}
		return nil, nil
	}
	delta, rerr := chatStreamObject(rawDelta, "chunk.choices[0].delta", map[string]bool{
		"role": true, "content": true, "tool_calls": true, "refusal": true,
		"function_call": true, "audio": true, "reasoning_content": true,
		"reasoning": true,
	})
	if rerr != nil {
		return nil, rerr
	}
	if rawRole, ok := delta["role"]; ok && !isNull(rawRole) {
		var role string
		if err := json.Unmarshal(rawRole, &role); err != nil || role != "assistant" {
			return nil, chatStreamBad()
		}
	}
	if rawRef, ok := delta["refusal"]; ok && !isNull(rawRef) {
		var refusal string
		if err := json.Unmarshal(rawRef, &refusal); err != nil {
			return nil, chatStreamBad()
		}
		if refusal != "" {
			return nil, chatStreamUnrep("chunk.choices[0].delta.refusal")
		}
	}
	if rawFn, ok := delta["function_call"]; ok && !isNull(rawFn) {
		return nil, chatStreamUnrep("chunk.choices[0].delta.function_call")
	}
	if rawAudio, ok := delta["audio"]; ok && !isNull(rawAudio) {
		return nil, chatStreamUnrep("chunk.choices[0].delta.audio")
	}
	// Provider-private reasoning: absent/null/empty is exact and emits
	// nothing; substantive is strict-502 unless the policy drop applies.
	// Covers both delta.reasoning_content and the native kimi-class
	// delta.reasoning variant. Dropped bytes never become TextFragment
	// and never affect finish.
	reasoningNonempty := false
	for _, key := range []string{"reasoning_content", "reasoning"} {
		raw, ok := delta[key]
		if !ok || isNull(raw) {
			continue
		}
		path := "chunk.choices[0].delta." + key
		_, nonempty, rerr := validateChatReasoningContent(raw, path)
		if rerr != nil {
			return nil, rerr
		}
		if nonempty && !d.allowPrivateDrop {
			return nil, chatStreamUnrep(path)
		}
		reasoningNonempty = reasoningNonempty || nonempty
	}
	var evs []ResponseEvent
	if d.sawFinish {
		// Terminal content is closed: only empty deltas may follow; usage
		// arrives in its own chunk.
		if reasoningNonempty {
			return nil, chatStreamBad()
		}
		for _, k := range []string{"content", "tool_calls"} {
			rawV, ok := delta[k]
			if !ok || isNull(rawV) {
				continue
			}
			if k == "content" {
				var s string
				if err := json.Unmarshal(rawV, &s); err != nil || s != "" {
					return nil, chatStreamBad()
				}
				continue
			}
			var arr []json.RawMessage
			if err := json.Unmarshal(rawV, &arr); err != nil || len(arr) > 0 {
				return nil, chatStreamBad()
			}
		}
	} else {
		if rawC, ok := delta["content"]; ok && !isNull(rawC) {
			var s string
			if err := json.Unmarshal(rawC, &s); err != nil {
				return nil, chatStreamBad()
			}
			if s != "" {
				ev, rerr := d.emitText(s)
				if rerr != nil {
					return nil, rerr
				}
				evs = append(evs, ev...)
			}
		}
		if rawTC, ok := delta["tool_calls"]; ok && !isNull(rawTC) {
			var arr []json.RawMessage
			if err := json.Unmarshal(rawTC, &arr); err != nil {
				return nil, chatStreamBad()
			}
			for _, traw := range arr {
				fevs, rerr := d.handleToolFragment(traw)
				if rerr != nil {
					return nil, rerr
				}
				evs = append(evs, fevs...)
			}
		}
	}
	if finish != nil {
		if rerr := d.handleFinish(*finish); rerr != nil {
			return nil, rerr
		}
	}
	return evs, nil
}

// chatStreamObject decodes a JSON object under an allowlist. Unknown members
// are unrepresentable (value-free source path); non-objects are malformed.
func chatStreamObject(raw json.RawMessage, what string, allowed map[string]bool) (map[string]json.RawMessage, *RouterError) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, chatStreamBad()
	}
	for k := range obj {
		if !allowed[k] {
			return nil, chatStreamUnrep(what + "." + k)
		}
	}
	return obj, nil
}

// handleFinish records the terminal finish marker exactly once. Tool blocks
// with incomplete identity are refused here (no guessed identities); argument
// JSON validates at [DONE].
func (d *ChatStreamDecoder) handleFinish(finish string) *RouterError {
	if d.sawFinish {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream sent a duplicate finish marker"}
	}
	var kind FinishKind
	switch finish {
	case "stop":
		kind = FinishStop
	case "length":
		kind = FinishLength
	case "content_filter":
		kind = FinishContentFilter
	case "tool_calls":
		kind = FinishToolCalls
	default:
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream stop reason has no representation"}
	}
	d.sawFinish = true
	d.finish = Finish{Kind: kind, Raw: finish}
	for _, idx := range d.order {
		if !d.slots[idx].emitted {
			return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream tool call is not valid JSON"}
		}
	}
	return nil
}
