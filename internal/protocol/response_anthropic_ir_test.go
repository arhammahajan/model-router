// Anthropic row source tests: wire bytes -> canonical IR, no targets.
//
// These tests pin DecodeAnthropicResponse as a destination-independent
// source validator: ordered blocks, exact arguments, opaque identities,
// closed per-kind fields, stop provenance, and canonical usage math. No
// target encoder and no old pair translator is consulted here;
// representability for any destination is the generic worker's gate
// (CheckRepresentable) over the provenance carried below.
package protocol

import (
	"strings"
	"testing"
)

func anthIRRaw(stop, content, usage string) string {
	return `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x",` +
		`"content":` + content + `,"stop_reason":"` + stop + `","usage":` + usage + `}`
}

func anthIRUsage(input, output int) string {
	return `{"input_tokens":` + itoa(input) + `,"output_tokens":` + itoa(output) + `}`
}

func anthIRDecode(t *testing.T, raw string) Response {
	t.Helper()
	resp, rerr := DecodeAnthropicResponse([]byte(raw), DefaultLimits())
	if rerr != nil {
		t.Fatalf("DecodeAnthropicResponse: %v", rerr)
	}
	return resp
}

func anthIRFail(t *testing.T, raw string) *RouterError {
	t.Helper()
	_, rerr := DecodeAnthropicResponse([]byte(raw), DefaultLimits())
	if rerr == nil {
		t.Fatalf("expected 502 for %.120s, got success", raw)
	}
	if rerr.Status != 502 {
		t.Fatalf("expected 502 for %.120s, got %v", raw, rerr)
	}
	return rerr
}

// --- Decoder pins source facts once ------------------------------------------

func TestAnthropicIRDecoderFacts(t *testing.T) {
	odd := "has:colon and space"
	long := strings.Repeat("z", 100)
	content := `[{"type":"tool_use","id":"` + odd + `","name":"get_time","input":{"n":9007199254740993}},` +
		`{"type":"text","text":"mid"},` +
		`{"type":"tool_use","id":"` + long + `","name":"ping","input":{}}]`
	usage := `{"input_tokens":10,"cache_creation_input_tokens":3,` +
		`"cache_read_input_tokens":4,"output_tokens":7}`
	resp := anthIRDecode(t, anthIRRaw("tool_use", content, usage))
	if resp.Source != DialectAnthropic || string(resp.ID) != "msg_1" || resp.Model != "claude-x" {
		t.Fatalf("provenance lost: %+v", resp)
	}
	if len(resp.Items) != 3 {
		t.Fatalf("items wrong: %#v", resp.Items)
	}
	// Order preserved one-to-one: tool, text, tool.
	first, ok := resp.Items[0].(ToolCallItem)
	if !ok || string(first.CallID) != odd || first.Name != "get_time" {
		t.Fatalf("tool-first order lost: %#v", resp.Items[0])
	}
	mid, ok := resp.Items[1].(TextItem)
	if !ok || mid.Text != "mid" {
		t.Fatalf("text order lost: %#v", resp.Items[1])
	}
	last, ok := resp.Items[2].(ToolCallItem)
	if !ok || string(last.CallID) != long {
		t.Fatalf("second tool lost: %#v", resp.Items[2])
	}
	// Item IDs are minted ordinals, distinct from the opaque call IDs.
	for i, it := range resp.Items {
		if want := "b" + itoa(i); string(it.responseItemID()) != want {
			t.Fatalf("item %d id = %q, want %q", i, it.responseItemID(), want)
		}
	}
	if string(first.Arguments) != `{"n":9007199254740993}` {
		t.Fatalf("bigint args not exact: %s", first.Arguments)
	}
	if string(last.Arguments) != `{}` {
		t.Fatalf("empty input must stay {}: %s", last.Arguments)
	}
	// Canonical prompt counts each share exactly once: 10+3+4=17.
	u := resp.Usage
	if u.Prompt == nil || *u.Prompt != 17 {
		t.Fatalf("prompt double-counted or lost: %+v", u)
	}
	if u.Completion == nil || *u.Completion != 7 || u.Total == nil || *u.Total != 24 {
		t.Fatalf("output/total wrong: %+v", u)
	}
	if u.CacheCreation == nil || *u.CacheCreation != 3 || u.CacheRead == nil || *u.CacheRead != 4 {
		t.Fatalf("cache shares not separate: %+v", u)
	}
	if rerr := u.ValidateAuthoritative(); rerr != nil {
		t.Fatalf("decoded usage not authoritative: %v", rerr)
	}
	if resp.Finish.Kind != FinishToolCalls || resp.Finish.Raw != "tool_use" {
		t.Fatalf("finish wrong: %+v", resp.Finish)
	}
}

func TestAnthropicIRDecoderEmptySpans(t *testing.T) {
	// An explicitly empty text span is an item, never dropped.
	resp := anthIRDecode(t, anthIRRaw("end_turn",
		`[{"type":"text","text":""},{"type":"text","text":"hi"}]`, anthIRUsage(2, 1)))
	if len(resp.Items) != 2 {
		t.Fatalf("empty text dropped: %#v", resp.Items)
	}
	if tx, ok := resp.Items[0].(TextItem); !ok || tx.Text != "" {
		t.Fatalf("empty text item wrong: %#v", resp.Items[0])
	}
	// An empty content array is a valid empty generation (all old rows
	// rendered it); a call-less tool_use stop stays decode-valid with its
	// provenance for the target gate.
	resp = anthIRDecode(t, anthIRRaw("tool_use", `[]`, anthIRUsage(2, 1)))
	if len(resp.Items) != 0 || resp.Finish.Kind != FinishToolCalls {
		t.Fatalf("call-less tool_use wrong: %#v %+v", resp.Items, resp.Finish)
	}
}

// --- Complete tool_use requires its input object ------------------------------

func TestAnthropicIRInputRequired(t *testing.T) {
	mk := func(inputFrag string) string {
		return anthIRRaw("tool_use",
			`[{"type":"tool_use","id":"c1","name":"f"`+inputFrag+`}]`, anthIRUsage(2, 1))
	}
	for _, raw := range []string{
		mk(``),                // missing
		mk(`,"input":null`),   // null
		mk(`,"input":[1]`),    // array
		mk(`,"input":"{}"`),   // scalar string
		mk(`,"input":7`),      // scalar number
		mk(`,"input":{"a":}`), // invalid JSON
	} {
		anthIRFail(t, raw)
	}
	// Present objects pass, including nested bigints, compacted exactly.
	resp := anthIRDecode(t, mk(`,"input":{"a":{"n":9007199254740993}}`))
	if got := string(resp.Items[0].(ToolCallItem).Arguments); got != `{"a":{"n":9007199254740993}}` {
		t.Fatalf("nested args not exact: %s", got)
	}
}

func TestAnthropicIRDuplicateIDs(t *testing.T) {
	raw := anthIRRaw("tool_use",
		`[{"type":"tool_use","id":"d","name":"a","input":{}},`+
			`{"type":"text","text":"x"},`+
			`{"type":"tool_use","id":"d","name":"b","input":{}}]`, anthIRUsage(4, 3))
	rerr := anthIRFail(t, raw)
	if !strings.Contains(rerr.Msg, "conflicting tool call identities") {
		t.Fatalf("wrong diagnostic: %v", rerr)
	}
}

// --- Closed per-kind fields ----------------------------------------------------

func TestAnthropicIRClosedBlocks(t *testing.T) {
	usage := anthIRUsage(4, 3)
	bad := []string{
		// Unknown envelope member.
		`{"id":"m","type":"message","role":"assistant","model":"c","content":[],"stop_reason":"end_turn","usage":` + usage + `,"future":1}`,
		// Cross-kind members.
		anthIRRaw("end_turn", `[{"type":"text","text":"x","id":"c1"}]`, usage),
		anthIRRaw("end_turn", `[{"type":"text","text":"x","input":{}}]`, usage),
		anthIRRaw("tool_use", `[{"type":"tool_use","id":"c1","name":"f","input":{},"text":"x"}]`, usage),
		anthIRRaw("tool_use", `[{"type":"tool_use","id":"c1","name":"f","input":{},"citations":[]}]`, usage),
		// Unknown block member.
		anthIRRaw("end_turn", `[{"type":"text","text":"x","cache_control":{}}]`, usage),
		// Private and unmapped block kinds fail closed, never dropped.
		anthIRRaw("end_turn", `[{"type":"thinking","thinking":"hmm"}]`, usage),
		anthIRRaw("end_turn", `[{"type":"redacted_thinking","data":"x"}]`, usage),
		anthIRRaw("end_turn", `[{"type":"server_tool_use","id":"s","name":"search","input":{}}]`, usage),
		anthIRRaw("end_turn", `[{"type":"web_search_tool_result","tool_use_id":"s","content":[]}]`, usage),
		anthIRRaw("end_turn", `[{"type":"image","source":{}}]`, usage),
		anthIRRaw("end_turn", `[{"type":"document","source":{}}]`, usage),
		anthIRRaw("end_turn", `[{"type":"tool_result","tool_use_id":"c1","content":"x"}]`, usage),
		anthIRRaw("end_turn", `[{"type":"mystery","x":1}]`, usage),
		// Missing/incoherent members.
		anthIRRaw("end_turn", `[{"text":"x"}]`, usage),
		anthIRRaw("end_turn", `[{"type":"text"}]`, usage),
		anthIRRaw("end_turn", `[{"type":"text","text":null}]`, usage),
		anthIRRaw("end_turn", `[{"type":"text","text":7}]`, usage),
		anthIRRaw("tool_use", `[{"type":"tool_use","name":"f","input":{}}]`, usage),
		anthIRRaw("tool_use", `[{"type":"tool_use","id":"c1","input":{}}]`, usage),
		anthIRRaw("tool_use", `[{"type":"tool_use","id":"","name":"f","input":{}}]`, usage),
		anthIRRaw("tool_use", `[{"type":"tool_use","id":"c1","name":"","input":{}}]`, usage),
		anthIRRaw("tool_use", `[{"type":"tool_use","id":"c1","name":"  ","input":{}}]`, usage),
		// Malformed envelope/type/role/model/id.
		`{"type":"message","role":"assistant","model":"c","content":[],"stop_reason":"end_turn","usage":` + usage + `}`,
		anthIRRaw("end_turn", `null`, usage),
		`{"id":"","type":"message","role":"assistant","model":"c","content":[],"stop_reason":"end_turn","usage":` + usage + `}`,
		`{"id":"   ","type":"message","role":"assistant","model":"c","content":[],"stop_reason":"end_turn","usage":` + usage + `}`,
		`{"id":"m","type":"text","role":"assistant","model":"c","content":[],"stop_reason":"end_turn","usage":` + usage + `}`,
		`{"id":"m","type":"message","role":"user","model":"c","content":[],"stop_reason":"end_turn","usage":` + usage + `}`,
		`{"id":"m","type":"message","role":"assistant","model":"","content":[],"stop_reason":"end_turn","usage":` + usage + `}`,
		anthIRRaw("end_turn", `{"type":"text"}`, usage),
	}
	for _, raw := range bad {
		anthIRFail(t, raw)
	}
}

// --- Citations: closed block union ----------------------------------------------

func TestAnthropicIRCitations(t *testing.T) {
	usage := anthIRUsage(4, 3)
	// Absent, null, and empty citations are native; only carried text.
	for _, frag := range []string{``, `,"citations":null`, `,"citations":[]`} {
		resp := anthIRDecode(t, anthIRRaw("end_turn",
			`[{"type":"text","text":"hi"`+frag+`}]`, usage))
		if tx, ok := resp.Items[0].(TextItem); !ok || tx.Text != "hi" {
			t.Fatalf("citations frag %q broke text: %#v", frag, resp.Items[0])
		}
	}
	// Non-empty citations have no representation: fail closed.
	anthIRFail(t, anthIRRaw("end_turn",
		`[{"type":"text","text":"hi","citations":[{"type":"x"}]}]`, usage))
}

// --- Stop provenance ------------------------------------------------------------

func TestAnthropicIRStops(t *testing.T) {
	usage := anthIRUsage(4, 3)
	content := `[{"type":"text","text":"hi"}]`
	for _, tc := range []struct {
		stop string
		kind FinishKind
	}{
		{"end_turn", FinishStop},
		{"stop_sequence", FinishStop},
		{"max_tokens", FinishLength},
		{"tool_use", FinishToolCalls},
		{"refusal", FinishRefusal},
	} {
		resp := anthIRDecode(t, anthIRRaw(tc.stop, content, usage))
		if resp.Finish.Kind != tc.kind || resp.Finish.Raw != tc.stop {
			t.Fatalf("stop %q -> %+v, want kind %q", tc.stop, resp.Finish, tc.kind)
		}
	}
	// Refusal keeps its raw stop: it is never downgraded to stop.
	resp := anthIRDecode(t, anthIRRaw("refusal", content, usage))
	if _, ok := resp.Items[0].(TextItem); !ok {
		t.Fatalf("refusal must not invent items: %#v", resp.Items)
	}
	// Unknown stops fail closed; nothing invents a synthetic stop.
	for _, stop := range []string{"pause_turn", "model_context_window_exceeded", "stop", "STOP", "foobar", ""} {
		rerr := anthIRFail(t, anthIRRaw(stop, content, usage))
		if !strings.Contains(rerr.Msg, "stop reason") {
			t.Fatalf("stop %q wrong diagnostic: %v", stop, rerr)
		}
	}
	for _, raw := range []string{
		anthIRRaw("end_turn", content, `null`),
		strings.Replace(anthIRRaw("end_turn", content, usage), `"stop_reason":"end_turn"`, `"stop_reason":null`, 1),
		strings.Replace(anthIRRaw("end_turn", content, usage), `"stop_reason":"end_turn"`, `"stop_reason":7`, 1),
		strings.Replace(anthIRRaw("end_turn", content, usage), `"stop_reason":"end_turn",`, ``, 1),
	} {
		anthIRFail(t, raw)
	}
}

func TestAnthropicIRStopSequenceEcho(t *testing.T) {
	usage := anthIRUsage(4, 3)
	content := `[{"type":"text","text":"hi"}]`
	// A well-formed stop-sequence echo is harmless metadata and ignored.
	base := anthIRRaw("stop_sequence", content, usage)
	raw := strings.Replace(base, `"usage":`, `"stop_sequence":"stop","usage":`, 1)
	resp := anthIRDecode(t, raw)
	if resp.Finish.Kind != FinishStop || resp.Finish.Raw != "stop_sequence" {
		t.Fatalf("stop_sequence echo broke finish: %+v", resp.Finish)
	}
	// A mistyped echo fails closed.
	raw = strings.Replace(base, `"usage":`, `"stop_sequence":7,"usage":`, 1)
	anthIRFail(t, raw)
}

// --- Canonical usage: prompt counted exactly once ---------------------------------

func TestAnthropicIRCacheMath(t *testing.T) {
	usage := `{"input_tokens":10,"cache_creation_input_tokens":3,` +
		`"cache_read_input_tokens":4,"output_tokens":7}`
	resp := anthIRDecode(t, anthIRRaw("end_turn", `[{"type":"text","text":"hi"}]`, usage))
	u := resp.Usage
	if *u.Prompt != 17 || *u.Completion != 7 || *u.Total != 24 {
		t.Fatalf("canonical math wrong (want 17/7/24): %+v", u)
	}
	if *u.CacheCreation != 3 || *u.CacheRead != 4 {
		t.Fatalf("shares lost: %+v", u)
	}
	// Missing shares are absent (nil), not zero-filled.
	resp = anthIRDecode(t, anthIRRaw("end_turn", `[{"type":"text","text":"hi"}]`, anthIRUsage(5, 2)))
	if resp.Usage.CacheCreation != nil || resp.Usage.CacheRead != nil {
		t.Fatalf("absent shares fabricated: %+v", resp.Usage)
	}
	// Explicit null shares are absent too.
	usage = `{"input_tokens":5,"cache_creation_input_tokens":null,` +
		`"cache_read_input_tokens":null,"output_tokens":2}`
	resp = anthIRDecode(t, anthIRRaw("end_turn", `[{"type":"text","text":"hi"}]`, usage))
	if resp.Usage.CacheCreation != nil || resp.Usage.CacheRead != nil || *resp.Usage.Prompt != 5 {
		t.Fatalf("null shares wrong: %+v", resp.Usage)
	}
	// Explicit zero shares stay present (zero is representable everywhere).
	usage = `{"input_tokens":5,"cache_creation_input_tokens":0,` +
		`"cache_read_input_tokens":0,"output_tokens":2}`
	resp = anthIRDecode(t, anthIRRaw("end_turn", `[{"type":"text","text":"hi"}]`, usage))
	if resp.Usage.CacheCreation == nil || *resp.Usage.CacheCreation != 0 ||
		resp.Usage.CacheRead == nil || *resp.Usage.CacheRead != 0 {
		t.Fatalf("zero shares lost: %+v", resp.Usage)
	}
}

// --- Required counters, exact totals, overflow -------------------------------------

func TestAnthropicIRUsageContract(t *testing.T) {
	content := `[{"type":"text","text":"hi"}]`
	bad := []string{
		anthIRRaw("end_turn", content, `null`),
		anthIRRaw("end_turn", content, `{"output_tokens":1}`),
		anthIRRaw("end_turn", content, `{"input_tokens":2}`),
		anthIRRaw("end_turn", content, `{"input_tokens":null,"output_tokens":1}`),
		anthIRRaw("end_turn", content, `{"input_tokens":2,"output_tokens":null}`),
		// Total must equal prompt+completion exactly.
		anthIRRaw("end_turn", content, `{"input_tokens":2,"output_tokens":1,"total_tokens":9}`),
		anthIRRaw("end_turn", content, `{"input_tokens":4,"output_tokens":3,"total_tokens":8}`),
		// Malformed counters fail: fractions, exponents, quotes, negatives.
		anthIRRaw("end_turn", content, `{"input_tokens":2.0,"output_tokens":1}`),
		anthIRRaw("end_turn", content, `{"input_tokens":"2","output_tokens":1}`),
		anthIRRaw("end_turn", content, `{"input_tokens":-1,"output_tokens":1}`),
		anthIRRaw("end_turn", content, `{"input_tokens":2,"output_tokens":1e2}`),
		// Overflow fails closed, never wraps.
		anthIRRaw("end_turn", content, `{"input_tokens":9223372036854775807,"output_tokens":1}`),
		anthIRRaw("end_turn", content, `{"input_tokens":9223372036854775807,"cache_creation_input_tokens":1,"cache_read_input_tokens":0,"output_tokens":0}`),
	}
	for _, raw := range bad {
		anthIRFail(t, raw)
	}
	// Unknown usage members fail; documented null-absent members pass as null.
	for _, k := range []string{"server_tool_use", "service_tier", "cache_creation",
		"output_tokens_details", "speed", "inference_geo"} {
		anthIRFail(t, anthIRRaw("end_turn", content,
			`{"input_tokens":2,"output_tokens":1,"`+k+`":{"x":1}}`))
		anthIRFail(t, anthIRRaw("end_turn", content,
			`{"input_tokens":2,"output_tokens":1,"`+k+`":0}`))
		resp := anthIRDecode(t, anthIRRaw("end_turn", content,
			`{"input_tokens":2,"output_tokens":1,"`+k+`":null}`))
		if *resp.Usage.Prompt != 2 || *resp.Usage.Total != 3 {
			t.Fatalf("null %s broke totals: %+v", k, resp.Usage)
		}
	}
	anthIRFail(t, anthIRRaw("end_turn", content, `{"input_tokens":2,"output_tokens":1,"future":1}`))
}

// --- Bounds -------------------------------------------------------------------------

func TestAnthropicIRLimits(t *testing.T) {
	content := `[{"type":"text","text":"hi"}]`
	usage := anthIRUsage(4, 3)
	lim := DefaultLimits()
	// One oversized argument field fails.
	big := `{"a":"` + strings.Repeat("x", lim.MaxArgsChars) + `"}`
	_, rerr := DecodeAnthropicResponse([]byte(anthIRRaw("tool_use",
		`[{"type":"tool_use","id":"c1","name":"f","input":`+big+`}]`, usage)), DefaultLimits())
	if rerr == nil {
		t.Fatal("oversized args must fail")
	}
	// Aggregate text+args bound enforced at decode.
	small := DefaultLimits()
	small.MaxStreamArgsChars = 20
	_, rerr = DecodeAnthropicResponse([]byte(anthIRRaw("tool_use",
		`[{"type":"tool_use","id":"c1","name":"f","input":{"a":"123456789012345"}},`+
			`{"type":"tool_use","id":"c2","name":"g","input":{"b":"123456789012345"}}]`, usage)), small)
	if rerr == nil {
		t.Fatal("aggregate args must fail")
	}
	// Tool-call count bound enforced at decode.
	small = DefaultLimits()
	small.MaxToolCalls = 1
	_, rerr = DecodeAnthropicResponse([]byte(anthIRRaw("tool_use",
		`[{"type":"tool_use","id":"c1","name":"f","input":{}},`+
			`{"type":"tool_use","id":"c2","name":"g","input":{}}]`, usage)), small)
	if rerr == nil {
		t.Fatal("tool count must fail")
	}
	// String IDs bounded, never by the 64-char name grammar.
	small = DefaultLimits()
	small.MaxArgsChars = 8
	_, rerr = DecodeAnthropicResponse([]byte(strings.Replace(
		anthIRRaw("end_turn", content, usage), `"msg_1"`, `"123456789"`, 1)), small)
	if rerr == nil {
		t.Fatal("oversized response id must fail")
	}
	_, rerr = DecodeAnthropicResponse([]byte(anthIRRaw("tool_use",
		`[{"type":"tool_use","id":"123456789","name":"f","input":{}}]`, usage)), small)
	if rerr == nil {
		t.Fatal("oversized call id must fail")
	}
}

// --- Source accepts what destinations constrain ----------------------------------------

func TestAnthropicIRSourceVsDestConstraints(t *testing.T) {
	// Grammar-invalid tool names are source-valid: the 1..64 name grammar
	// is a destination constraint the target gate enforces, never the
	// source decoder. Opaque call IDs (colons, spaces, 100 chars) pass.
	resp := anthIRDecode(t, anthIRRaw("tool_use",
		`[{"type":"tool_use","id":"has:colon and space","name":"has space","input":{}},`+
			`{"type":"tool_use","id":"`+strings.Repeat("z", 100)+`","name":"`+strings.Repeat("n", 100)+`","input":{}}]`,
		anthIRUsage(4, 3)))
	calls := 0
	for _, it := range resp.Items {
		if c, ok := it.(ToolCallItem); ok {
			calls++
			if c.Name == "" {
				t.Fatalf("name lost: %#v", c)
			}
		}
	}
	if calls != 2 {
		t.Fatalf("source-valid calls lost: %#v", resp.Items)
	}
}

func TestAnthropicIRUnknownStopValueFree(t *testing.T) {
	secret := "private-upstream-stop-value"
	err := anthIRFail(t, anthIRRaw(secret, `[]`, anthIRUsage(1, 1)))
	if strings.Contains(err.Msg, secret) {
		t.Fatal("unknown upstream enum leaked into diagnostic")
	}
}
