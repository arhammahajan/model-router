package protocol

// Anthropic complete-response row cutover: all three upstream JSON paths
// select (DecodeAnthropicResponse, target encoder) with no per-pair
// reparsing. These tests call the actual thin wrappers with shared
// fixtures and prove the IR source once; they do not re-encode by hand
// and do not consult the retired pair parsers.

import (
	"encoding/json"
	"strings"
	"testing"
)

func anthCutRaw(stop, content, usage string) string {
	return `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x",` +
		`"content":` + content + `,"stop_reason":"` + stop + `","usage":` + usage + `}`
}

func anthCutUsage(input, output int) string {
	return `{"input_tokens":` + itoa(input) + `,"output_tokens":` + itoa(output) + `}`
}

func mustAnthCut(t *testing.T, fn func([]byte, string, Limits) ([]byte, *RouterError), raw, model string) []byte {
	t.Helper()
	out, rerr := fn([]byte(raw), model, DefaultLimits())
	if rerr != nil {
		t.Fatalf("wrapper failed: %v", rerr)
	}
	return out
}

func mustAnthCut502(t *testing.T, fn func([]byte, string, Limits) ([]byte, *RouterError), raw string) *RouterError {
	t.Helper()
	_, rerr := fn([]byte(raw), "public", DefaultLimits())
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("expected 502, got %v", rerr)
	}
	return rerr
}

// Shared decode is invoked once: the same fixture decodes to one IR and
// all three wrappers render from it.
func TestAnthropicCutoverSharedDecode(t *testing.T) {
	content := `[{"type":"text","text":"lead"},` +
		`{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"n":9007199254740993}}]`
	usage := `{"input_tokens":10,"cache_creation_input_tokens":3,"cache_read_input_tokens":4,"output_tokens":7}`
	raw := anthCutRaw("tool_use", content, usage)
	resp, rerr := DecodeAnthropicResponse([]byte(raw), DefaultLimits())
	if rerr != nil {
		t.Fatalf("shared decode: %v", rerr)
	}
	if resp.Source != DialectAnthropic || string(resp.ID) != "msg_1" {
		t.Fatalf("provenance lost: %+v", resp)
	}
	if resp.Usage.Prompt == nil || *resp.Usage.Prompt != 17 || *resp.Usage.Total != 24 {
		t.Fatalf("canonical math wrong: %+v", resp.Usage)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("items wrong: %#v", resp.Items)
	}
	if _, ok := resp.Items[0].(TextItem); !ok {
		t.Fatalf("text-first order lost: %#v", resp.Items[0])
	}
	if string(resp.Items[1].(ToolCallItem).Arguments) != `{"n":9007199254740993}` {
		t.Fatalf("bigint not exact: %s", resp.Items[1].(ToolCallItem).Arguments)
	}
	// All three actual wrappers accept the same bytes.
	chatOut := mustAnthCut(t, TranslateAnthropicJSON, raw, "public-chat")
	gemOut := mustAnthCut(t, TranslateAnthropicJSONToGemini, raw, "public-gemini")
	respOut := mustAnthCut(t, TranslateAnthropicJSONToResponses, raw, "public-resp")
	if !strings.Contains(string(chatOut), `"finish_reason":"tool_calls"`) {
		t.Fatalf("chat finish: %s", chatOut)
	}
	if !strings.Contains(string(gemOut), `"finishReason":"STOP"`) {
		t.Fatalf("gemini finish: %s", gemOut)
	}
	if !strings.Contains(string(respOut), `"status":"completed"`) {
		t.Fatalf("responses status: %s", respOut)
	}
	// Public model stamped everywhere; upstream id preserved only in Gemini.
	if !strings.Contains(string(chatOut), `"model":"public-chat"`) {
		t.Fatalf("chat public model: %s", chatOut)
	}
	if !strings.Contains(string(gemOut), `"modelVersion":"public-gemini"`) || !strings.Contains(string(gemOut), `"responseId":"msg_1"`) {
		t.Fatalf("gemini identity: %s", gemOut)
	}
	if !strings.Contains(string(respOut), `"model":"public-resp"`) {
		t.Fatalf("responses public model: %s", respOut)
	}
	if strings.Contains(string(chatOut), "msg_1") && strings.Contains(string(chatOut), `"id":"msg_1"`) {
		t.Fatalf("chat must mint its id: %s", chatOut)
	}
	// Call identity survives verbatim in all three.
	for _, out := range []string{string(chatOut), string(gemOut), string(respOut)} {
		if !strings.Contains(out, "toolu_1") || !strings.Contains(out, "9007199254740993") {
			t.Fatalf("identity/bigint lost: %s", out)
		}
	}
}

// Ordered blocks: tool-first preserves order in Gemini/Responses;
// Chat refuses text-after-tool explicitly (no silent reorder).
func TestAnthropicCutoverOrder(t *testing.T) {
	usage := anthCutUsage(4, 3)
	// Text-then-tool: all three capable.
	raw := anthCutRaw("tool_use",
		`[{"type":"text","text":"hi"},{"type":"tool_use","id":"c1","name":"f","input":{}}]`, usage)
	mustAnthCut(t, TranslateAnthropicJSON, raw, "public")
	mustAnthCut(t, TranslateAnthropicJSONToGemini, raw, "public")
	mustAnthCut(t, TranslateAnthropicJSONToResponses, raw, "public")
	// Tool-then-text: Gemini/Responses preserve, Chat refuses interleaving.
	raw = anthCutRaw("tool_use",
		`[{"type":"tool_use","id":"c1","name":"f","input":{}},{"type":"text","text":"after"}]`, usage)
	mustAnthCut(t, TranslateAnthropicJSONToGemini, raw, "public")
	out := mustAnthCut(t, TranslateAnthropicJSONToResponses, raw, "public")
	if strings.Index(string(out), "function_call") > strings.Index(string(out), "after") {
		t.Fatalf("tool-first order lost: %s", out)
	}
	if rerr := mustAnthCut502(t, TranslateAnthropicJSON, raw); !strings.Contains(rerr.Msg, "response.items order") {
		t.Fatalf("chat must refuse text-after-tool explicitly: %v", rerr)
	}
}

// Cache totals: inclusive prompt counted once; per-target details.
func TestAnthropicCutoverCache(t *testing.T) {
	content := `[{"type":"text","text":"hi"}]`
	raw := anthCutRaw("end_turn", content,
		`{"input_tokens":10,"cache_creation_input_tokens":3,"cache_read_input_tokens":4,"output_tokens":7}`)
	chatOut := mustAnthCut(t, TranslateAnthropicJSON, raw, "public")
	if !strings.Contains(string(chatOut), `"prompt_tokens":17`) || !strings.Contains(string(chatOut), `"cached_tokens":4`) {
		t.Fatalf("chat cache wrong: %s", chatOut)
	}
	gemOut := mustAnthCut(t, TranslateAnthropicJSONToGemini, raw, "public")
	if !strings.Contains(string(gemOut), `"promptTokenCount":17`) ||
		!strings.Contains(string(gemOut), `"cachedContentTokenCount":4`) ||
		!strings.Contains(string(gemOut), `"totalTokenCount":24`) {
		t.Fatalf("gemini cache wrong: %s", gemOut)
	}
	respOut := mustAnthCut(t, TranslateAnthropicJSONToResponses, raw, "public")
	if !strings.Contains(string(respOut), `"input_tokens":17`) ||
		!strings.Contains(string(respOut), `"cached_tokens":4`) ||
		!strings.Contains(string(respOut), `"cache_write_tokens":3`) {
		t.Fatalf("responses cache wrong: %s", respOut)
	}
	// Combined overflow fails once at shared decode for all three.
	bad := anthCutRaw("end_turn", content, anthCutUsage(9223372036854775807, 1))
	for _, fn := range []func([]byte, string, Limits) ([]byte, *RouterError){
		TranslateAnthropicJSON, TranslateAnthropicJSONToGemini, TranslateAnthropicJSONToResponses,
	} {
		mustAnthCut502(t, fn, bad)
	}
}

// Finish gates: stop_sequence refuses only in Responses (target gate,
// not source decode); refusal maps per target; call-less tool_use
// refuses only in Responses.
func TestAnthropicCutoverFinishGates(t *testing.T) {
	usage := anthCutUsage(1, 1)
	content := `[{"type":"text","text":"hi"}]`
	// stop_sequence: Chat/Gemini stop, Responses 502 (root 618f95f).
	ss := anthCutRaw("stop_sequence", content, usage)
	mustAnthCut(t, TranslateAnthropicJSON, ss, "public")
	mustAnthCut(t, TranslateAnthropicJSONToGemini, ss, "public")
	if rerr := mustAnthCut502(t, TranslateAnthropicJSONToResponses, ss); !strings.Contains(rerr.Msg, "stop_reason") {
		t.Fatalf("responses stop_sequence gate: %v", rerr)
	}
	// Decode itself carries the raw stop (proves target-side gate).
	resp, rerr := DecodeAnthropicResponse([]byte(ss), DefaultLimits())
	if rerr != nil || resp.Finish.Kind != FinishStop || resp.Finish.Raw != "stop_sequence" {
		t.Fatalf("decode must carry stop_sequence provenance: %+v %v", resp.Finish, rerr)
	}
	// Refusal: Chat content_filter, Responses incomplete/content_filter, Gemini 502.
	ref := anthCutRaw("refusal", content, usage)
	chatOut := mustAnthCut(t, TranslateAnthropicJSON, ref, "public")
	if !strings.Contains(string(chatOut), `"finish_reason":"content_filter"`) {
		t.Fatalf("chat refusal: %s", chatOut)
	}
	respOut := mustAnthCut(t, TranslateAnthropicJSONToResponses, ref, "public")
	if !strings.Contains(string(respOut), `"status":"incomplete"`) || !strings.Contains(string(respOut), "content_filter") {
		t.Fatalf("responses refusal: %s", respOut)
	}
	mustAnthCut502(t, TranslateAnthropicJSONToGemini, ref)
	// Call-less tool_use: decode-valid, Responses refuses, others render.
	empty := anthCutRaw("tool_use", `[]`, usage)
	rd, rerr := DecodeAnthropicResponse([]byte(empty), DefaultLimits())
	if rerr != nil || rd.Finish.Kind != FinishToolCalls || len(rd.Items) != 0 {
		t.Fatalf("call-less decode: %+v %v", rd, rerr)
	}
	mustAnthCut(t, TranslateAnthropicJSON, empty, "public")
	mustAnthCut(t, TranslateAnthropicJSONToGemini, empty, "public")
	mustAnthCut502(t, TranslateAnthropicJSONToResponses, empty)
	// Unknown stops fail at shared decode for all three.
	for _, stop := range []string{"pause_turn", "model_context_window_exceeded", "bogus"} {
		raw := anthCutRaw(stop, content, usage)
		for _, fn := range []func([]byte, string, Limits) ([]byte, *RouterError){
			TranslateAnthropicJSON, TranslateAnthropicJSONToGemini, TranslateAnthropicJSONToResponses,
		} {
			if rerr := mustAnthCut502(t, fn, raw); strings.Contains(rerr.Msg, "msg_1") || strings.Contains(rerr.Msg, stop) && stop != "bogus" && len(stop) < 20 {
				// Diagnostics stay value-free; only short enum names may appear in the quoted path.
				t.Fatalf("value-free violation: %v", rerr)
			}
		}
	}
}

// Opaque IDs and exact numerics: bounded, never name-grammar.
func TestAnthropicCutoverIdentities(t *testing.T) {
	odd := "has:colon and space"
	usage := anthCutUsage(4, 3)
	raw := anthCutRaw("tool_use",
		`[{"type":"tool_use","id":"`+odd+`","name":"get_time","input":{"n":9007199254740993}}]`, usage)
	for _, fn := range []func([]byte, string, Limits) ([]byte, *RouterError){
		TranslateAnthropicJSON, TranslateAnthropicJSONToGemini, TranslateAnthropicJSONToResponses,
	} {
		out := mustAnthCut(t, fn, raw, "public")
		if !strings.Contains(string(out), odd) || !strings.Contains(string(out), "9007199254740993") {
			t.Fatalf("opaque/bigint lost: %s", out)
		}
	}
	// Oversized response/call IDs fail at shared decode for all three.
	lim := DefaultLimits()
	lim.MaxArgsChars = 8
	bigID := strings.Replace(anthCutRaw("end_turn", `[{"type":"text","text":"hi"}]`, usage), `"msg_1"`, `"123456789"`, 1)
	for _, fn := range []func([]byte, string, Limits) ([]byte, *RouterError){
		TranslateAnthropicJSON, TranslateAnthropicJSONToGemini, TranslateAnthropicJSONToResponses,
	} {
		if _, rerr := fn([]byte(bigID), "public", lim); rerr == nil || rerr.Status != 502 {
			t.Fatalf("oversized id must fail: %v", rerr)
		}
	}
	// Empty/whitespace IDs fail everywhere.
	for _, raw := range []string{
		strings.Replace(anthCutRaw("end_turn", `[{"type":"text","text":"hi"}]`, usage), `"msg_1"`, `"  "`, 1),
		anthCutRaw("tool_use", `[{"type":"tool_use","id":"","name":"f","input":{}}]`, usage),
	} {
		for _, fn := range []func([]byte, string, Limits) ([]byte, *RouterError){
			TranslateAnthropicJSON, TranslateAnthropicJSONToGemini, TranslateAnthropicJSONToResponses,
		} {
			mustAnthCut502(t, fn, raw)
		}
	}
}

// Complete tool_use input is required: missing/null/malformed fail closed.
func TestAnthropicCutoverInputRequired(t *testing.T) {
	usage := anthCutUsage(2, 1)
	mk := func(frag string) string {
		return anthCutRaw("tool_use", `[{"type":"tool_use","id":"c1","name":"f"`+frag+`}]`, usage)
	}
	for _, raw := range []string{
		mk(``), mk(`,"input":null`), mk(`,"input":[1]`),
		mk(`,"input":"{}"`), mk(`,"input":7`), mk(`,"input":{"a":}`),
	} {
		for _, fn := range []func([]byte, string, Limits) ([]byte, *RouterError){
			TranslateAnthropicJSON, TranslateAnthropicJSONToGemini, TranslateAnthropicJSONToResponses,
		} {
			mustAnthCut502(t, fn, raw)
		}
	}
}

// Unknown members, billing, citations, and bounds fail closed in all three.
func TestAnthropicCutoverClosedWorld(t *testing.T) {
	usage := anthCutUsage(4, 3)
	badContents := []string{
		`[{"type":"text","text":"x","id":"c1"}]`,
		`[{"type":"tool_use","id":"c1","name":"f","input":{},"text":"x"}]`,
		`[{"type":"text","text":"x","cache_control":{}}]`,
		`[{"type":"thinking","thinking":"h"}]`,
		`[{"type":"redacted_thinking","data":"x"}]`,
		`[{"type":"server_tool_use","id":"s","name":"search","input":{}}]`,
		`[{"type":"image","source":{}}]`,
		`[{"type":"mystery","x":1}]`,
		`[{"text":"x"}]`,
		`[{"type":"text"}]`,
		`[{"type":"text","text":null}]`,
		`[{"type":"tool_use","id":"c1","name":"f","input":{}}]`, // placeholder, replaced below for dup test
	}
	for _, c := range badContents[:len(badContents)-1] {
		raw := anthCutRaw("end_turn", c, usage)
		for _, fn := range []func([]byte, string, Limits) ([]byte, *RouterError){
			TranslateAnthropicJSON, TranslateAnthropicJSONToGemini, TranslateAnthropicJSONToResponses,
		} {
			mustAnthCut502(t, fn, raw)
		}
	}
	// Duplicate IDs fail everywhere.
	dup := anthCutRaw("tool_use",
		`[{"type":"tool_use","id":"d","name":"a","input":{}},{"type":"tool_use","id":"d","name":"b","input":{}}]`, usage)
	for _, fn := range []func([]byte, string, Limits) ([]byte, *RouterError){
		TranslateAnthropicJSON, TranslateAnthropicJSONToGemini, TranslateAnthropicJSONToResponses,
	} {
		mustAnthCut502(t, fn, dup)
	}
	// Unknown envelope member fails everywhere.
	raw := `{"id":"m","type":"message","role":"assistant","model":"c","content":[],"stop_reason":"end_turn","usage":` + usage + `,"future":1}`
	for _, fn := range []func([]byte, string, Limits) ([]byte, *RouterError){
		TranslateAnthropicJSON, TranslateAnthropicJSONToGemini, TranslateAnthropicJSONToResponses,
	} {
		mustAnthCut502(t, fn, raw)
	}
	// Non-empty citations fail; empty/null/absent pass (checked via decode).
	for _, frag := range []string{``, `,"citations":null`, `,"citations":[]`} {
		raw := anthCutRaw("end_turn", `[{"type":"text","text":"hi"`+frag+`}]`, usage)
		mustAnthCut(t, TranslateAnthropicJSON, raw, "public")
		mustAnthCut(t, TranslateAnthropicJSONToGemini, raw, "public")
		mustAnthCut(t, TranslateAnthropicJSONToResponses, raw, "public")
	}
	raw = anthCutRaw("end_turn", `[{"type":"text","text":"hi","citations":[{"type":"x"}]}]`, usage)
	for _, fn := range []func([]byte, string, Limits) ([]byte, *RouterError){
		TranslateAnthropicJSON, TranslateAnthropicJSONToGemini, TranslateAnthropicJSONToResponses,
	} {
		mustAnthCut502(t, fn, raw)
	}
	// Non-null billing members fail; nulls pass.
	for _, k := range []string{"server_tool_use", "service_tier", "cache_creation", "output_tokens_details", "speed", "inference_geo"} {
		raw := anthCutRaw("end_turn", `[{"type":"text","text":"hi"}]`,
			`{"input_tokens":2,"output_tokens":1,"`+k+`":{"x":1}}`)
		for _, fn := range []func([]byte, string, Limits) ([]byte, *RouterError){
			TranslateAnthropicJSON, TranslateAnthropicJSONToGemini, TranslateAnthropicJSONToResponses,
		} {
			mustAnthCut502(t, fn, raw)
		}
		raw = anthCutRaw("end_turn", `[{"type":"text","text":"hi"}]`,
			`{"input_tokens":2,"output_tokens":1,"`+k+`":null}`)
		mustAnthCut(t, TranslateAnthropicJSON, raw, "public")
		mustAnthCut(t, TranslateAnthropicJSONToGemini, raw, "public")
		mustAnthCut(t, TranslateAnthropicJSONToResponses, raw, "public")
	}
	// Oversize args / count fail everywhere (shared decode bounds).
	big := `{"a":"` + strings.Repeat("x", DefaultLimits().MaxArgsChars) + `"}`
	raw = anthCutRaw("tool_use", `[{"type":"tool_use","id":"c1","name":"f","input":`+big+`}]`, usage)
	for _, fn := range []func([]byte, string, Limits) ([]byte, *RouterError){
		TranslateAnthropicJSON, TranslateAnthropicJSONToGemini, TranslateAnthropicJSONToResponses,
	} {
		mustAnthCut502(t, fn, raw)
	}
	small := DefaultLimits()
	small.MaxToolCalls = 1
	raw = anthCutRaw("tool_use",
		`[{"type":"tool_use","id":"c1","name":"f","input":{}},{"type":"tool_use","id":"c2","name":"g","input":{}}]`, usage)
	for _, fn := range []func([]byte, string, Limits) ([]byte, *RouterError){
		TranslateAnthropicJSON, TranslateAnthropicJSONToGemini, TranslateAnthropicJSONToResponses,
	} {
		if _, rerr := fn([]byte(raw), "public", small); rerr == nil || rerr.Status != 502 {
			t.Fatalf("tool count must fail: %v", rerr)
		}
	}
}

// Envelope shape: empty text retained, empty content renders a message item.
func TestAnthropicCutoverEmptySpans(t *testing.T) {
	usage := anthCutUsage(2, 1)
	raw := anthCutRaw("end_turn", `[{"type":"text","text":""},{"type":"text","text":"hi"}]`, usage)
	resp, rerr := DecodeAnthropicResponse([]byte(raw), DefaultLimits())
	if rerr != nil || len(resp.Items) != 2 {
		t.Fatalf("empty span dropped: %+v %v", resp.Items, rerr)
	}
	for _, fn := range []func([]byte, string, Limits) ([]byte, *RouterError){
		TranslateAnthropicJSON, TranslateAnthropicJSONToGemini, TranslateAnthropicJSONToResponses,
	} {
		mustAnthCut(t, fn, raw, "public")
	}
	raw = anthCutRaw("end_turn", `[]`, usage)
	mustAnthCut(t, TranslateAnthropicJSON, raw, "public")
	mustAnthCut(t, TranslateAnthropicJSONToGemini, raw, "public")
	out := mustAnthCut(t, TranslateAnthropicJSONToResponses, raw, "public")
	var body struct {
		Output []any `json:"output"`
	}
	if err := json.Unmarshal(out, &body); err != nil || len(body.Output) != 1 {
		t.Fatalf("empty content must render one item: %s %v", out, err)
	}
}
