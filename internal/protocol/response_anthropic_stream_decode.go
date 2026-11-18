// Anthropic source stream codec: one SSE decoder for the Anthropic row.
//
// AnthropicStreamDecoder validates the Anthropic Messages SSE source ONCE,
// independently of destination, and emits canonical semantic events for the
// shared lifecycle (ResponseLifecycle) and the single pipeline
// (TranslateStream). It performs no downstream writes, mints no downstream
// IDs, owns no reader, spawns no goroutines, and enforces no destination
// policy: target differences (placeholders, refusal rendering, name grammar,
// usage detailing) live in target encoders via CheckRepresentable, never here.
//
// Wire contract (proven against the Anthropic stream halves of
// anthropic_to_chat.go, anthropic_to_responses_stream.go and the Gemini
// stream translators, which this decoder does NOT call):
//
//   - Framing is owned by FrameSource (NewSSEFrameReader). The SSE event
//     name is semantic: every payload carries a "type" that must equal the
//     frame's event name, and a frame without an event name is malformed.
//     Event-only frames (HasEvent, empty Data) are keepalives only for
//     ping; any other event-only frame is malformed.
//   - message_start is first and once. Its message carries the full Message
//     envelope shape, but minimal senders omit the descriptive members, so
//     type/role/model/content/stop fields are validated only when present
//     while id and usage are always required. The reviewed legacy
//     compatibility (go/conformance/cases/chat_sse_text.json) sends
//     message {id, usage:{input_tokens:5}} with NO model/type/role: the
//     absence is preserved truthfully as ResponseStart.Model "" (never a
//     fake model or public alias). A supplied-empty model string is
//     rejected at this boundary; absence is representable only when the
//     member is omitted or null.
//   - InitialUsage carries start-time accounting with ScopeInitial: input
//     plus the cache shares when present, output only when the start
//     usage reports it. An input-only legacy start leaves Completion
//     absent, never a fabricated zero. Multiple cumulative message_delta
//     updates are valid: output overwrites, explicit input/cache members
//     refresh the start values, and the inclusive prompt (input+creation+
//     read, overflow-checked via the shared anthropicIRPrompt) is
//     recomputed. The single authoritative UsageEvent (ScopeTerminal) is
//     emitted only at a valid message_stop.
//   - Blocks open in arrival order under unique nonnegative source indices;
//     text and tool blocks share one ordinal ItemID series ("b0", "b1",
//     ...) while each block additionally carries its source-dialect
//     logical position in SourceIndex so targets order by typed provenance
//     instead of parsing the opaque ItemID. Call IDs are opaque
//     (validOpaqueID + MaxArgsChars, never the tool-name grammar) and
//     unique across the whole generation.
//   - Seeded block content is content: initial text and non-"{}" tool
//     input emit immediately as fragments. A "{}" block-start input is the
//     streaming placeholder for deltas to come and seeds nothing, so the
//     later partial_json bytes are never prefixed with a duplicated "{}".
//     A tool block with no argument bytes at stop emits the canonical "{}"
//     exactly once (counted in both args bounds). Accumulated bytes that
//     never form one JSON object classify the block with MalformedToolEvent
//     for the target gate (Gemini suppress-and-remap vs Anthropic/Responses
//     refuse); identity, envelope, and size errors stay 502 *RouterError.
//   - Thinking, signatures, citations, media, server/hosted tools and any
//     other unrepresentable block or delta kind fail closed with a
//     value-free source path; they are never silently dropped. Unknown
//     stops fail at message_stop via the shared decodeAnthropicFinish, so
//     no synthetic terminal is invented (truncation stays truncation).
//   - message_stop requires a recorded stop reason, terminal usage, and
//     fully closed blocks; it emits BlockClose-free UsageEvent then
//     CompletionEvent and returns promptly (no EOF read past the
//     terminal). Close validates state, reads nothing, and returns no
//     events. FailureEvent is never emitted here: every failure is a
//     *RouterError for the pipeline to finalize.
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
	"strconv"
	"strings"
)

// AnthropicStreamDecoder decodes Anthropic SSE frames into canonical semantic
// events. It owns only upstream source state; it performs no downstream
// writes, holds no reader, spawns no goroutines, and enforces no destination
// policy. Use it with NewSSEFrameReader and TranslateStream.
type AnthropicStreamDecoder struct {
	bodyEnded bool // first message_delta closes the content phase
	lim       Limits

	started  bool
	respID   string
	model    string
	done     bool
	sawDelta bool
	stop     string
	// sawStopDetails records whether any message_delta carried a non-null
	// stop_details object. The terminal consistency check (details require a
	// refusal stop) waits for message_stop so split deltas cannot downgrade
	// a safety signal.
	sawStopDetails bool

	inputBase   int
	cacheCreate int
	cacheRead   int
	hasCreation bool
	hasRead     bool
	output      int
	hasOutput   bool

	blocks    map[int]*anthropicStreamBlock
	seenIdx   map[int]bool
	seenCalls map[string]bool
	totalArgs int
	toolCount int
	nextItem  int

	// gatewayDrop drops validated gateway billing metadata
	// (usage.cost, usage.opencode) when true (lossy compatibility).
	// False keeps the strict 502. Set via NewAnthropicStreamDecoderWithPolicy.
	gatewayDrop bool
}

// anthropicStreamBlock accumulates one Anthropic content block keyed by its
// source index. Argument bytes buffer under the Limits bounds (the
// decoder's only retention); text emits immediately and is never kept.
type anthropicStreamBlock struct {
	kind    BlockKind
	item    ItemID
	call    CallID
	name    string
	args    strings.Builder
	argsLen int
}

// NewAnthropicStreamDecoder creates the Anthropic source stream decoder for
// one generation (strict: gateway billing metadata fails closed).
func NewAnthropicStreamDecoder(lim Limits) *AnthropicStreamDecoder {
	return NewAnthropicStreamDecoderWithPolicy(lim, false)
}

// NewAnthropicStreamDecoderWithPolicy creates the Anthropic source stream
// decoder with narrow gateway-billing policy: gatewayDrop drops validated
// usage.cost/usage.opencode, false keeps the strict 502. Absent/null is
// exact in both modes; malformed or unknown nested members are 502 in both
// modes. Authoritative counters and terminal verification are unchanged.
func NewAnthropicStreamDecoderWithPolicy(lim Limits, gatewayDrop bool) *AnthropicStreamDecoder {
	return &AnthropicStreamDecoder{
		lim:         lim,
		blocks:      map[int]*anthropicStreamBlock{},
		seenIdx:     map[int]bool{},
		seenCalls:   map[string]bool{},
		gatewayDrop: gatewayDrop,
	}
}

func anthropicStreamBad() *RouterError {
	return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
}

func anthropicStreamUnrep(what string) *RouterError {
	return &RouterError{Status: 502, Code: CodeUpstreamBad,
		Msg: fmt.Sprintf("Upstream response feature %q has no representation", what)}
}

// DecodeFrame decodes one typed SSE frame into semantic events in wire order.
// It returns promptly on message_stop and never emits FailureEvent (all
// failures are *RouterError for the pipeline to finalize).
func (d *AnthropicStreamDecoder) DecodeFrame(frame Frame) ([]ResponseEvent, *RouterError) {
	if d.done {
		// The pipeline never reads past the source terminal, but a
		// keepalive ping raced with completion stays harmless.
		if frame.HasEvent && frame.Event == "ping" {
			return d.handlePing(frame.Data)
		}
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent events after completion"}
	}
	if !frame.HasEvent {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream event is missing its event name"}
	}
	if d.bodyEnded && strings.HasPrefix(frame.Event, "content_block_") {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent content after message_delta"}
	}
	switch frame.Event {
	case "message_start":
		return d.handleStart(frame.Data)
	case "content_block_start":
		return d.handleBlockStart(frame.Data)
	case "content_block_delta":
		return d.handleBlockDelta(frame.Data)
	case "content_block_stop":
		return d.handleBlockStop(frame.Data)
	case "message_delta":
		return d.handleMessageDelta(frame.Data)
	case "message_stop":
		return d.handleMessageStop(frame.Data)
	case "ping":
		return d.handlePing(frame.Data)
	case "error":
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream reported an error"}
	default:
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream event has no representation"}
	}
}

// Close validates the trailing state after the source terminal. It performs
// no reads and returns no events: success was already emitted at
// message_stop.
func (d *AnthropicStreamDecoder) Close() ([]ResponseEvent, *RouterError) {
	if !d.done {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream ended without completion"}
	}
	return nil, nil
}

// anthropicStreamCheckType enforces the streaming contract that every event
// body is a JSON object whose nonempty "type" equals the SSE event name. A
// missing, mismatched, or empty type fails closed with a value-free 502;
// model, text, and error bytes are never echoed.
func anthropicStreamCheckType(body map[string]json.RawMessage, want string) *RouterError {
	rawT, ok := body["type"]
	if !ok || isNull(rawT) {
		return anthropicStreamBad()
	}
	var typ string
	if err := json.Unmarshal(rawT, &typ); err != nil || typ == "" || typ != want {
		return anthropicStreamBad()
	}
	return nil
}

// anthropicStreamUsage is the parsed scalar accounting of one streaming
// usage object with presence: input is required at start, output is
// required at delta, and the cache shares are optional everywhere (nil
// when the wire omitted them or sent an explicit null). Scalar reasoning
// has no Anthropic usage field: it would have to be invented, so there is
// no bucket here.
type anthropicStreamUsage struct {
	input, creation, read, output          int
	hasInput, hasCreation, hasRead, hasOut bool
}

// parseAnthropicStreamUsage validates one Anthropic streaming usage object
// with explicit presence. requireInput/requireOutput select which of
// input_tokens/output_tokens must be present as non-null integers (a start
// needs input, a delta needs output). Malformed counters (negative,
// fractional, quoted, overflowing), unknown members and non-null unmapped
// billing members are 502s with neutral, value-free diagnostics: usage is
// never underreported or silently trimmed. Gateway billing metadata is
// strict by default; see parseAnthropicStreamUsageWithPolicy.
func parseAnthropicStreamUsage(raw json.RawMessage, what string, requireInput, requireOutput bool) (anthropicStreamUsage, *RouterError) {
	return parseAnthropicStreamUsageWithPolicy(raw, what, requireInput, requireOutput, false)
}

// parseAnthropicStreamUsageWithPolicy validates one Anthropic streaming
// usage object with narrow gateway-billing policy: allowDrop drops
// validated cost/opencode, false keeps the strict 502.
func parseAnthropicStreamUsageWithPolicy(raw json.RawMessage, what string, requireInput, requireOutput bool, allowDrop bool) (anthropicStreamUsage, *RouterError) {
	var u anthropicStreamUsage
	obj, rerr := chatResponseObject(raw, map[string]bool{
		"input_tokens": true, "output_tokens": true,
		"cache_creation_input_tokens": true, "cache_read_input_tokens": true,
		"cache_creation": true, "output_tokens_details": true,
		"server_tool_use": true, "service_tier": true,
		"speed": true, "inference_geo": true,
		"cost": true, "opencode": true,
	}, what)
	if rerr != nil {
		return u, rerr
	}
	if rerr := validateGatewayBillingMetadata(obj, what, allowDrop); rerr != nil {
		return u, rerr
	}
	for _, k := range []string{"input_tokens", "output_tokens", "cache_creation_input_tokens", "cache_read_input_tokens"} {
		rawN, ok := obj[k]
		if !ok || isNull(rawN) {
			continue
		}
		n, rerr := responseInt(rawN)
		if rerr != nil {
			return u, rerr
		}
		switch k {
		case "input_tokens":
			u.input, u.hasInput = n, true
		case "output_tokens":
			u.output, u.hasOut = n, true
		case "cache_creation_input_tokens":
			u.creation, u.hasCreation = n, true
		case "cache_read_input_tokens":
			u.read, u.hasRead = n, true
		}
	}
	for k := range obj {
		if anthropicNullAbsent[k] {
			if v := obj[k]; !isNull(v) && !anthropicEmptyUsageEcho(k, v, u.creation) {
				return u, anthropicStreamUnrep(what + "." + k)
			}
		}
	}
	if requireInput && !u.hasInput {
		return u, anthropicStreamBad()
	}
	if requireOutput && !u.hasOut {
		return u, anthropicStreamBad()
	}
	return u, nil
}

func (d *AnthropicStreamDecoder) requireStarted() *RouterError {
	if !d.started {
		return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream sent content before the response start"}
	}
	return nil
}

func (d *AnthropicStreamDecoder) nextItemID() ItemID {
	id := ItemID("b" + strconv.Itoa(d.nextItem))
	d.nextItem++
	return id
}

// handleStart validates the single message_start. The descriptive message
// members (type/role/model/content/stop fields) are validated when present;
// id and usage are always required. An omitted or null model preserves the
// reviewed absence as ""; a supplied-empty model string fails closed.
func (d *AnthropicStreamDecoder) handleStart(data string) ([]ResponseEvent, *RouterError) {
	if d.started {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream stream sent a duplicate message_start"}
	}
	if strings.TrimSpace(data) == "" {
		return nil, anthropicStreamBad()
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &body); err != nil || body == nil {
		return nil, anthropicStreamBad()
	}
	if _, rerr := chatResponseObject(mustRaw(body), map[string]bool{"type": true, "message": true}, "message_start"); rerr != nil {
		return nil, rerr
	}
	if rerr := anthropicStreamCheckType(body, "message_start"); rerr != nil {
		return nil, rerr
	}
	rawMsg, ok := body["message"]
	if !ok || isNull(rawMsg) {
		return nil, anthropicStreamBad()
	}
	msg, rerr := chatResponseObject(rawMsg, map[string]bool{
		"id": true, "type": true, "role": true, "model": true,
		"content": true, "stop_reason": true, "stop_sequence": true, "stop_details": true, "usage": true,
	}, "message_start.message")
	if rerr != nil {
		return nil, rerr
	}
	rawID, ok := msg["id"]
	if !ok || isNull(rawID) {
		return nil, anthropicStreamBad()
	}
	var id string
	if err := json.Unmarshal(rawID, &id); err != nil || !validOpaqueID(id) {
		return nil, anthropicStreamBad()
	}
	if len(id) > d.lim.MaxArgsChars {
		return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream response exceeds the size bound"}
	}
	if rawT, ok := msg["type"]; ok && !isNull(rawT) {
		var typ string
		if err := json.Unmarshal(rawT, &typ); err != nil || typ != "message" {
			return nil, anthropicStreamBad()
		}
	}
	if rawRole, ok := msg["role"]; ok && !isNull(rawRole) {
		var role string
		if err := json.Unmarshal(rawRole, &role); err != nil || role != "assistant" {
			return nil, anthropicStreamBad()
		}
	}
	var model string
	if rawModel, ok := msg["model"]; ok && !isNull(rawModel) {
		var m string
		if err := json.Unmarshal(rawModel, &m); err != nil || m == "" || strings.TrimSpace(m) == "" {
			return nil, anthropicStreamBad()
		}
		model = m
	}
	if rawContent, ok := msg["content"]; ok && !isNull(rawContent) {
		var blocks []json.RawMessage
		if err := json.Unmarshal(rawContent, &blocks); err != nil || len(blocks) != 0 {
			return nil, anthropicStreamBad()
		}
	}
	for _, k := range []string{"stop_reason", "stop_sequence", "stop_details"} {
		if rawS, ok := msg[k]; ok && !isNull(rawS) {
			return nil, anthropicStreamBad()
		}
	}
	rawUsage, ok := msg["usage"]
	if !ok || isNull(rawUsage) {
		return nil, anthropicStreamBad()
	}
	u, rerr := parseAnthropicStreamUsageWithPolicy(rawUsage, "message_start.message.usage", true, false, d.gatewayDrop)
	if rerr != nil {
		return nil, rerr
	}
	prompt, rerr := anthropicStreamPrompt(u)
	if rerr != nil {
		return nil, rerr
	}
	d.respID, d.model = id, model
	d.inputBase = u.input
	if u.hasCreation {
		d.cacheCreate, d.hasCreation = u.creation, true
	}
	if u.hasRead {
		d.cacheRead, d.hasRead = u.read, true
	}
	initial := ResponseUsage{
		Prompt:     intPtr(prompt),
		Provenance: UsageProvenance{Source: DialectAnthropic, Scope: UsageInitial},
	}
	if u.hasCreation {
		c := u.creation
		initial.CacheCreation = &c
	}
	if u.hasRead {
		r := u.read
		initial.CacheRead = &r
	}
	// An input-only legacy start leaves output absent: a zero here would
	// be a fabricated source claim, not a reported counter.
	if u.hasOut {
		o := u.output
		total := prompt + o
		if total < prompt || total < o {
			return nil, anthropicStreamBad()
		}
		initial.Completion = &o
		initial.Total = &total
	}
	d.started = true
	return []ResponseEvent{ResponseStart{
		ID: ResponseID(id), Model: model, Source: DialectAnthropic, InitialUsage: &initial,
	}}, nil
}

// anthropicStreamPrompt returns the canonical inclusive prompt total for
// streaming counters via the shared complete-row helper: input+creation+
// read counted exactly once, overflow-checked.
func anthropicStreamPrompt(u anthropicStreamUsage) (int, *RouterError) {
	iru := anthropicIRUsage{input: u.input}
	if u.hasCreation {
		c := u.creation
		iru.creation = &c
	}
	if u.hasRead {
		r := u.read
		iru.read = &r
	}
	return anthropicIRPrompt(iru)
}
