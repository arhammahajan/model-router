// Responses row source tests: bytes -> canonical IR, independent of target.
//
// These tests pin DecodeResponsesResponse only. No TranslateResponsesJSONTo*
// pair translator is called here: destination differences live in
// CheckRepresentable plus target-owned framing (generic worker), never in a
// second parse of the source. A per-target reparse or Chat-JSON intermediate
// would turn the shared-validation tests below red.
package protocol

import (
	"strings"
	"testing"
)

func respIRRaw(status, output, usage, extra string) string {
	return `{"id":"resp_9","object":"response","created_at":1746989954.0,"model":"up",` +
		`"status":"` + status + `","error":null,"incomplete_details":null,` +
		`"output":` + output + `,"usage":` + usage + extra + `}`
}

func respIRDecode(t *testing.T, raw string) Response {
	t.Helper()
	resp, rerr := DecodeResponsesResponse([]byte(raw), DefaultLimits())
	if rerr != nil {
		t.Fatalf("DecodeResponsesResponse: %v", rerr)
	}
	return resp
}

func respIRMustFail(t *testing.T, raw string) *RouterError {
	t.Helper()
	_, rerr := DecodeResponsesResponse([]byte(raw), DefaultLimits())
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("expected 502 for %.120s, got %v", raw, rerr)
	}
	return rerr
}

func respIRUsage(prompt, completion int) string {
	return `{"input_tokens":` + respItoa(prompt) + `,"output_tokens":` + respItoa(completion) +
		`,"total_tokens":` + respItoa(prompt+completion) + `}`
}

func respItoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var b [32]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// --- Decoder pins source facts once ------------------------------------------

func TestResponsesIRDecoderFacts(t *testing.T) {
	raw := `{"id":"resp_9","object":"response","created_at":1746989954.0,"model":"up",` +
		`"status":"completed","error":null,"incomplete_details":null,` +
		`"background":false,"instructions":null,"max_output_tokens":null,` +
		`"parallel_tool_calls":true,"previous_response_id":null,` +
		`"reasoning":{"effort":null,"summary":null},"store":false,` +
		`"temperature":1.0,"text":{"format":{"type":"text"}},"tool_choice":"auto","tools":[],` +
		`"top_p":1.0,"truncation":"disabled","user":null,"metadata":{},"service_tier":"default",` +
		`"output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed",` +
		`"content":[{"type":"output_text","text":"hi ","annotations":[],"logprobs":[]},` +
		`{"type":"output_text","text":"there","annotations":[],"logprobs":[]}]},` +
		`{"id":"fc_1","type":"function_call","status":"completed","call_id":"call 1:x","name":"get_time","arguments":"{\"n\":9007199254740993}"},` +
		`{"id":"fc_2","type":"function_call","status":"completed","call_id":"c2","name":"ping"}]` +
		`,"usage":{"input_tokens":12,"input_tokens_details":{"cached_tokens":4,"cache_write_tokens":1},` +
		`"output_tokens":7,"output_tokens_details":{"reasoning_tokens":3},"total_tokens":19}}`
	resp := respIRDecode(t, raw)
	if resp.Source != DialectResponses || string(resp.ID) != "resp_9" || resp.Model != "up" {
		t.Fatalf("provenance lost: %+v", resp)
	}
	// Order preserved: text parts in wire order, then calls in wire order.
	// Two output_text parts concatenate as two TextItems (b0,b1) to preserve
	// part order one-to-one; calls follow (b2,b3).
	if len(resp.Items) != 4 {
		t.Fatalf("items wrong count %d: %#v", len(resp.Items), resp.Items)
	}
	if tx, ok := resp.Items[0].(TextItem); !ok || tx.Text != "hi " {
		t.Fatalf("text part 0 wrong: %#v", resp.Items[0])
	}
	if tx, ok := resp.Items[1].(TextItem); !ok || tx.Text != "there" {
		t.Fatalf("text part 1 wrong: %#v", resp.Items[1])
	}
	tc, ok := resp.Items[2].(ToolCallItem)
	if !ok || string(tc.CallID) != "call 1:x" || tc.Name != "get_time" {
		t.Fatalf("call 0 wrong: %#v", resp.Items[2])
	}
	if string(tc.Arguments) != `{"n":9007199254740993}` {
		t.Fatalf("bigint args not exact: %s", tc.Arguments)
	}
	tc2, ok := resp.Items[3].(ToolCallItem)
	if !ok || string(tc2.CallID) != "c2" {
		t.Fatalf("call 1 wrong: %#v", resp.Items[3])
	}
	if string(tc2.Arguments) != `{}` {
		t.Fatalf("missing args must default to explicit {}: %s", tc2.Arguments)
	}
	// Item IDs are synthetic distinct handles; call_ids preserve upstream.
	seen := map[string]bool{}
	for _, it := range resp.Items {
		var iid string
		switch v := it.(type) {
		case TextItem:
			iid = string(v.ID)
		case RefusalItem:
			iid = string(v.ID)
		case ToolCallItem:
			iid = string(v.ID)
			if string(v.CallID) == iid {
				t.Fatalf("item id must be distinct from call id: %#v", v)
			}
		}
		if seen[iid] {
			t.Fatalf("duplicate item id %q", iid)
		}
		seen[iid] = true
	}
	u := resp.Usage
	if *u.Prompt != 12 || *u.Completion != 7 || *u.Total != 19 {
		t.Fatalf("totals wrong: %+v", u)
	}
	if u.CacheRead == nil || *u.CacheRead != 4 {
		t.Fatalf("cache read wrong: %+v", u)
	}
	if u.CacheCreation == nil || *u.CacheCreation != 1 {
		t.Fatalf("cache creation wrong: %+v", u)
	}
	if u.Reasoning == nil || *u.Reasoning != 3 {
		t.Fatalf("reasoning wrong: %+v", u)
	}
	if rerr := u.ValidateAuthoritative(); rerr != nil {
		t.Fatalf("decoded usage not authoritative: %v", rerr)
	}
	// Finish derives from status only (completed -> Stop); content
	// distinctions travel as items, never as a coupled finish rewrite.
	if resp.Finish.Kind != FinishStop || resp.Finish.Raw != "completed" {
		t.Fatalf("finish wrong: %+v", resp.Finish)
	}
	if resp.MalformedTools != 0 {
		t.Fatalf("malformed count fabricated: %d", resp.MalformedTools)
	}
}

func TestResponsesIRToolFirstOrder(t *testing.T) {
	raw := respIRRaw("completed",
		`[{"id":"fc_9","type":"function_call","status":"completed","call_id":"toolu_1","name":"calc","arguments":"{\"n\":1}"},`+
			`{"id":"msg_9","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"working","annotations":[]}]}]`,
		respIRUsage(12, 7), "")
	resp := respIRDecode(t, raw)
	if len(resp.Items) != 2 {
		t.Fatalf("items wrong: %#v", resp.Items)
	}
	if _, ok := resp.Items[0].(ToolCallItem); !ok {
		t.Fatalf("tool-first order lost: %#v", resp.Items[0])
	}
	if tx, ok := resp.Items[1].(TextItem); !ok || tx.Text != "working" {
		t.Fatalf("text after call lost: %#v", resp.Items[1])
	}
	if string(resp.Items[0].(ToolCallItem).CallID) != "toolu_1" {
		t.Fatalf("call identity lost: %#v", resp.Items[0])
	}
}

// --- Top-level reasoning echo vs output reasoning ----------------------------

func TestResponsesIRReasoningEcho(t *testing.T) {
	base := func(reasoningFrag, output string) string {
		raw := respIRRaw("completed", output, respIRUsage(5, 2), "")
		if reasoningFrag == "" {
			return raw
		}
		return strings.Replace(raw, `"output":`, `"reasoning":`+reasoningFrag+`,"output":`, 1)
	}
	okOut := `[{"id":"m","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi","annotations":[]}]}]`
	// Top-level summary enum strings are request-echo configuration.
	for _, frag := range []string{`{"summary":"auto"}`, `{"summary":"concise"}`, `{"summary":"detailed"}`, `{}`, `null`} {
		respIRDecode(t, base(frag, okOut))
	}
	// Top-level summary array is the OUTPUT shape, never the echo shape.
	for _, frag := range []string{`{"summary":[]}`, `{"summary":{}}`, `{"summary":"verbose"}`, `{"effort":"turbo"}`, `"auto"`, `[]`} {
		rerr := respIRMustFail(t, base(frag, okOut))
		if !strings.Contains(rerr.Msg, "response.reasoning") {
			t.Fatalf("reasoning echo must name field path, got %q", rerr.Msg)
		}
		if strings.Contains(rerr.Msg, "verbose") || strings.Contains(rerr.Msg, "turbo") {
			t.Fatalf("reasoning error must be value-free, got %q", rerr.Msg)
		}
	}
	// OUTPUT reasoning items have no IR variant and fail at source decode.
	badOut := `[{"id":"rs_1","type":"reasoning","status":"completed"}]`
	rerr := respIRMustFail(t, base(`{"summary":"auto"}`, badOut))
	if !strings.Contains(rerr.Msg, "response.output[0].type") {
		t.Fatalf("output reasoning must name item path, got %q", rerr.Msg)
	}
	// Hosted/media/private tool types fail explicitly (all targets refuse).
	for _, typ := range []string{
		`{"id":"w1","type":"web_search_call","status":"completed"}`,
		`{"id":"f1","type":"file_search_call","status":"completed"}`,
		`{"id":"c1","type":"computer_call","status":"completed"}`,
	} {
		respIRMustFail(t, base("", `[`+typ+`]`))
	}
}

// --- Status / incomplete / error --------------------------------------------

func TestResponsesIRStatusMapping(t *testing.T) {
	okOut := `[{"id":"m","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi","annotations":[]}]}]`
	// completed -> Stop.
	resp := respIRDecode(t, respIRRaw("completed", okOut, respIRUsage(5, 2), ""))
	if resp.Finish.Kind != FinishStop || resp.Finish.Raw != "completed" {
		t.Fatalf("completed finish wrong: %+v", resp.Finish)
	}
	// incomplete reasons -> canonical finish, Raw preserves the reason.
	raw := strings.Replace(respIRRaw("incomplete", okOut, respIRUsage(5, 2), ""),
		`"incomplete_details":null`, `"incomplete_details":{"reason":"max_output_tokens"}`, 1)
	resp = respIRDecode(t, raw)
	if resp.Finish.Kind != FinishLength || resp.Finish.Raw != "max_output_tokens" {
		t.Fatalf("length finish wrong: %+v", resp.Finish)
	}
	raw = strings.Replace(respIRRaw("incomplete", okOut, respIRUsage(5, 2), ""),
		`"incomplete_details":null`, `"incomplete_details":{"reason":"content_filter"}`, 1)
	resp = respIRDecode(t, raw)
	if resp.Finish.Kind != FinishContentFilter || resp.Finish.Raw != "content_filter" {
		t.Fatalf("filter finish wrong: %+v", resp.Finish)
	}
	// failed never becomes a fake stop.
	rerr := respIRMustFail(t, respIRRaw("failed", okOut, respIRUsage(5, 2), ""))
	if !strings.Contains(rerr.Msg, "reported an error") {
		t.Fatalf("failed must report error, got %q", rerr.Msg)
	}
	// error object with members never becomes content.
	rawErr := strings.Replace(respIRRaw("completed", okOut, respIRUsage(5, 2), ""),
		`"error":null`, `"error":{"code":"server_error","message":"boom"}`, 1)
	rerr = respIRMustFail(t, rawErr)
	if !strings.Contains(rerr.Msg, "reported an error") {
		t.Fatalf("error object must fail, got %q", rerr.Msg)
	}
	// Unknown statuses refuse value-free, never fabricate stop.
	for _, st := range []string{"cancelled", "queued", "in_progress", "bogus"} {
		rerr := respIRMustFail(t, respIRRaw(st, okOut, respIRUsage(5, 2), ""))
		if !strings.Contains(rerr.Msg, "response.status") {
			t.Fatalf("status %s must name response.status, got %q", st, rerr.Msg)
		}
		if strings.Contains(rerr.Msg, st) {
			t.Fatalf("status error must be value-free, got %q", rerr.Msg)
		}
	}
	// Unknown incomplete reasons refuse value-free.
	raw = strings.Replace(respIRRaw("incomplete", okOut, respIRUsage(5, 2), ""),
		`"incomplete_details":null`, `"incomplete_details":{"reason":"bogus"}`, 1)
	rerr = respIRMustFail(t, raw)
	if !strings.Contains(rerr.Msg, "response.incomplete_details.reason") {
		t.Fatalf("bad reason must name path, got %q", rerr.Msg)
	}
	if strings.Contains(rerr.Msg, "bogus") {
		t.Fatalf("reason error must be value-free, got %q", rerr.Msg)
	}
	// Missing incomplete_details on incomplete fails.
	respIRMustFail(t, respIRRaw("incomplete", okOut, respIRUsage(5, 2), ""))
}

// --- Refusal carried as provenance, not coupled finish ------------------------

func TestResponsesIRRefusalCarried(t *testing.T) {
	// Old r2c mapped completed+refusal to content_filter and failed
	// refusal+calls for Chat; r2a/r2g failed any refusal. The canonical
	// decoder carries ordered RefusalItems with Finish from status only;
	// encoders map per destination. This documents the deliberate
	// decoupling, not a weakened test.
	raw := respIRRaw("completed",
		`[{"id":"m","type":"message","role":"assistant","status":"completed",`+
			`"content":[{"type":"refusal","refusal":"I cannot help."}]}]`,
		respIRUsage(10, 5), "")
	resp := respIRDecode(t, raw)
	if resp.Finish.Kind != FinishStop {
		t.Fatalf("refusal must not rewrite Finish at source: %+v", resp.Finish)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("refusal items wrong: %#v", resp.Items)
	}
	if rf, ok := resp.Items[0].(RefusalItem); !ok || rf.Text != "I cannot help." {
		t.Fatalf("refusal item wrong: %#v", resp.Items[0])
	}
	// Refusal + tools both carried; the combination is encoder policy.
	raw = respIRRaw("completed",
		`[{"id":"m","type":"message","role":"assistant","status":"completed",`+
			`"content":[{"type":"refusal","refusal":"no"}]},`+
			`{"id":"fc_1","type":"function_call","status":"completed","call_id":"c1","name":"a","arguments":"{}"}]`,
		respIRUsage(10, 5), "")
	resp = respIRDecode(t, raw)
	if len(resp.Items) != 2 {
		t.Fatalf("refusal+calls must both be carried: %#v", resp.Items)
	}
	if _, ok := resp.Items[0].(RefusalItem); !ok {
		t.Fatalf("refusal must stay ordered first: %#v", resp.Items[0])
	}
	if _, ok := resp.Items[1].(ToolCallItem); !ok {
		t.Fatalf("call must stay ordered second: %#v", resp.Items[1])
	}
}

// --- Malformed carried as provenance (Gemini renders, others refuse) --------

func TestResponsesIRMalformedCarried(t *testing.T) {
	// Old r2c/r2a fail closed on bad names/args while r2g reports
	// malformed -> MALFORMED_FUNCTION_CALL. The canonical decoder counts
	// and skips (Chat-row shape); encoders gate via MalformedTools.
	// Pure probes: name "has space" passes nothing as valid; it is
	// malformed in r2g and invalid in r2c/r2a.
	mk := func(fc string) string {
		return respIRRaw("completed", `[`+fc+`]`, respIRUsage(4, 3), "")
	}
	// Grammar-invalid name -> malformed, not a valid item, not a hard fail.
	resp := respIRDecode(t, mk(`{"id":"fc_1","type":"function_call","status":"completed","call_id":"c1","name":"has space","arguments":"{}"}`))
	if resp.MalformedTools != 1 || len(resp.Items) != 0 {
		t.Fatalf("grammar-invalid name must be malformed: %+v %d", resp.Items, resp.MalformedTools)
	}
	// Non-object args string -> malformed.
	resp = respIRDecode(t, mk(`{"id":"fc_1","type":"function_call","status":"completed","call_id":"c1","name":"a","arguments":"[1]"}`))
	if resp.MalformedTools != 1 {
		t.Fatalf("array args must be malformed: %+v", resp.Items)
	}
	// Non-string args (object directly) -> malformed, never unwrapped.
	resp = respIRDecode(t, mk(`{"id":"fc_1","type":"function_call","status":"completed","call_id":"c1","name":"a","arguments":{}}`))
	if resp.MalformedTools != 1 {
		t.Fatalf("object args must be malformed: %+v", resp.Items)
	}
	// Missing name -> malformed.
	resp = respIRDecode(t, mk(`{"id":"fc_1","type":"function_call","status":"completed","call_id":"c1","arguments":"{}"}`))
	if resp.MalformedTools != 1 {
		t.Fatalf("missing name must be malformed: %+v", resp.Items)
	}
	// One valid + two malformed: only the valid call survives.
	resp = respIRDecode(t, respIRRaw("completed",
		`[{"id":"f0","type":"function_call","status":"completed","call_id":"ok","name":"f","arguments":"{}"},`+
			`{"id":"f1","type":"function_call","status":"completed","call_id":"b1","name":"","arguments":"{}"},`+
			`{"id":"f2","type":"function_call","status":"completed","call_id":"b2","name":"g","arguments":"[1]"}]`,
		respIRUsage(4, 3), ""))
	if resp.MalformedTools != 2 {
		t.Fatalf("malformed count = %d, want 2", resp.MalformedTools)
	}
	var calls []ToolCallItem
	for _, it := range resp.Items {
		if c, ok := it.(ToolCallItem); ok {
			calls = append(calls, c)
		}
	}
	if len(calls) != 1 || string(calls[0].CallID) != "ok" {
		t.Fatalf("valid call lost: %#v", resp.Items)
	}
}

// --- Opaque IDs, not the 64-char name grammar ---------------------------------

func TestResponsesIROpaqueIDs(t *testing.T) {
	// Bug record: old r2c/r2a applied validateToolName (1..64 name grammar)
	// to call_id; r2g correctly used validateCallID (opaque bounded).
	// Canonical uses validateCallID only. Pure probe: "has:colon and space"
	// passes r2g, fails r2c/r2a; here it must pass as a valid call.
	odd := "has:colon and space"
	long := strings.Repeat("z", 100)
	raw := `{"id":"` + long + `","object":"response","created_at":1,"model":"up",` +
		`"status":"completed","error":null,"incomplete_details":null,` +
		`"output":[{"id":"fc_1","type":"function_call","status":"completed","call_id":"` + odd + `","name":"f","arguments":"{\"n\":9007199254740993}"},` +
		`{"id":"fc_2","type":"function_call","status":"completed","call_id":"` + long + `","name":"g","arguments":""}],` +
		`"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}`
	resp := respIRDecode(t, raw)
	if len(resp.Items) != 2 {
		t.Fatalf("opaque ids must validate: %#v", resp.Items)
	}
	if string(resp.Items[0].(ToolCallItem).CallID) != odd {
		t.Fatalf("odd call id lost: %#v", resp.Items[0])
	}
	if !strings.Contains(string(resp.Items[0].(ToolCallItem).Arguments), "9007199254740993") {
		t.Fatalf("bigint lost: %s", resp.Items[0].(ToolCallItem).Arguments)
	}
	if string(resp.Items[1].(ToolCallItem).Arguments) != `{}` {
		t.Fatalf("empty args must be {}: %s", resp.Items[1].(ToolCallItem).Arguments)
	}
	// Whitespace-only and oversize IDs fail (deliberate tightening, same as
	// the Chat row precedent; no old case covers them).
	lim := DefaultLimits()
	bigID := strings.Repeat("x", lim.MaxArgsChars+1)
	for _, badID := range []string{"   ", bigID} {
		raw := `{"id":"` + badID + `","object":"response","created_at":1,"model":"up",` +
			`"status":"completed","error":null,"incomplete_details":null,` +
			`"output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
		// Empty output with bad response id must fail on the id, not pass.
		// Use non-empty output to isolate the id check from emptiness.
		_ = raw
		raw2 := strings.Replace(raw, `"output":[]`, `"output":[{"id":"m","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi","annotations":[]}]}]`, 1)
		respIRMustFail(t, raw2)
	}
}

// --- Usage: canonical prompt, cache buckets, reasoning ------------------------

func TestResponsesIRUsageFacts(t *testing.T) {
	mk := func(usage string) Response {
		return respIRDecode(t, respIRRaw("completed",
			`[{"id":"m","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi","annotations":[]}]}]`,
			usage, ""))
	}
	// Canonical prompt is verbatim input_tokens (no Anthropic subtraction);
	// cached/creation/reasoning carried as buckets.
	resp := mk(`{"input_tokens":12,"input_tokens_details":{"cached_tokens":4,"cache_write_tokens":1},` +
		`"output_tokens":7,"output_tokens_details":{"reasoning_tokens":3},"total_tokens":19}`)
	u := resp.Usage
	if *u.Prompt != 12 || *u.Completion != 7 || *u.Total != 19 {
		t.Fatalf("totals wrong: %+v", u)
	}
	if *u.CacheRead != 4 || *u.CacheCreation != 1 || *u.Reasoning != 3 {
		t.Fatalf("buckets wrong: %+v", u)
	}
	// Explicit zero vs missing: zero is carried, missing is nil.
	resp = mk(`{"input_tokens":5,"input_tokens_details":{"cached_tokens":0},"output_tokens":2,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":7}`)
	if resp.Usage.CacheRead == nil || *resp.Usage.CacheRead != 0 {
		t.Fatalf("explicit zero cache must be carried: %+v", resp.Usage)
	}
	if resp.Usage.Reasoning == nil || *resp.Usage.Reasoning != 0 {
		t.Fatalf("explicit zero reasoning must be carried: %+v", resp.Usage)
	}
	resp = mk(`{"input_tokens":5,"output_tokens":2,"total_tokens":7}`)
	if resp.Usage.CacheRead != nil || resp.Usage.CacheCreation != nil || resp.Usage.Reasoning != nil {
		t.Fatalf("absent buckets must stay nil: %+v", resp.Usage)
	}
	// Missing terminal counters fail.
	respIRMustFail(t, respIRRaw("completed",
		`[{"id":"m","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi","annotations":[]}]}]`,
		`{"input_tokens":5,"output_tokens":2}`, ""))
	// Mismatched total, subset overflow, fraction/quoted/negative fail.
	for _, usage := range []string{
		`{"input_tokens":5,"output_tokens":2,"total_tokens":99}`,
		`{"input_tokens":4,"input_tokens_details":{"cached_tokens":5},"output_tokens":3,"total_tokens":7}`,
		`{"input_tokens":4,"output_tokens":3,"output_tokens_details":{"reasoning_tokens":4},"total_tokens":7}`,
		`{"input_tokens":4,"input_tokens_details":{"cache_write_tokens":9},"output_tokens":3,"total_tokens":7}`,
		`{"input_tokens":1.5,"output_tokens":1,"total_tokens":2}`,
		`{"input_tokens":"5","output_tokens":2,"total_tokens":7}`,
		`{"input_tokens":-1,"output_tokens":2,"total_tokens":1}`,
		`{"input_tokens":5,"input_tokens_details":{"cached_tokens":-1},"output_tokens":2,"total_tokens":7}`,
	} {
		respIRMustFail(t, respIRRaw("completed",
			`[{"id":"m","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi","annotations":[]}]}]`,
			usage, ""))
	}
	// Unknown null accepted as newer default; unknown non-null fails.
	respIRDecode(t, respIRRaw("completed",
		`[{"id":"m","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi","annotations":[]}]}]`,
		`{"input_tokens":5,"output_tokens":2,"total_tokens":7}`, `,"future_null":null`))
	rerr := respIRMustFail(t, respIRRaw("completed",
		`[{"id":"m","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi","annotations":[]}]}]`,
		`{"input_tokens":5,"output_tokens":2,"total_tokens":7}`, `,"future":1`))
	if !strings.Contains(rerr.Msg, "response.future") {
		t.Fatalf("unknown field must name path, got %q", rerr.Msg)
	}
	// Zero ordinary details validate; nonzero unmapped fails.
	respIRDecode(t, respIRRaw("completed",
		`[{"id":"m","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi","annotations":[]}]}]`,
		`{"input_tokens":5,"input_tokens_details":{"cached_tokens":0,"text_tokens":0,"audio_tokens":0},"output_tokens":2,"output_tokens_details":{"audio_tokens":0,"text_tokens":0},"total_tokens":7}`, ""))
	respIRMustFail(t, respIRRaw("completed",
		`[{"id":"m","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi","annotations":[]}]}]`,
		`{"input_tokens":5,"input_tokens_details":{"text_tokens":1},"output_tokens":2,"total_tokens":7}`, ""))
	// Unknown nulls inside usage sub-objects are newer defaults (matches
	// the pre-cutover r2c/r2a contract); unknown non-nulls fail closed.
	respIRDecode(t, respIRRaw("completed",
		`[{"id":"m","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi","annotations":[]}]}]`,
		`{"input_tokens":5,"input_tokens_details":{"cached_tokens":null,"future_detail":null},"output_tokens":2,"output_tokens_details":null,"total_tokens":7,"future_usage":null}`, ""))
	rerr = respIRMustFail(t, respIRRaw("completed",
		`[{"id":"m","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi","annotations":[]}]}]`,
		`{"input_tokens":5,"input_tokens_details":{"future_detail":1},"output_tokens":2,"total_tokens":7}`, ""))
	if !strings.Contains(rerr.Msg, "response.usage.input_tokens_details.future_detail") {
		t.Fatalf("usage detail refusal must name path, got %q", rerr.Msg)
	}
}

// --- Shared validation fails identically --------------------------------------

func TestResponsesIRSharedValidation(t *testing.T) {
	okOut := `[{"id":"m","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi","annotations":[]}]}]`
	okUsage := respIRUsage(5, 2)
	bad := []string{
		// Unknown top-level non-null fails; null passes (covered above).
		strings.Replace(respIRRaw("completed", okOut, okUsage, ""), `"output":`, `"mystery":1,"output":`, 1),
		// Bad totals / envelopes.
		respIRRaw("completed", okOut, `{"input_tokens":5,"output_tokens":2,"total_tokens":8}`, ""),
		`{"id":"","object":"response","created_at":1,"model":"up","status":"completed","error":null,"incomplete_details":null,"output":` + okOut + `,"usage":` + okUsage + `}`,
		`{"id":"r","object":"bogus","created_at":1,"model":"up","status":"completed","error":null,"incomplete_details":null,"output":` + okOut + `,"usage":` + okUsage + `}`,
		`{"id":"r","object":"response","created_at":"yesterday","model":"up","status":"completed","error":null,"incomplete_details":null,"output":` + okOut + `,"usage":` + okUsage + `}`,
		`{"id":"r","object":"response","created_at":1,"model":"up","status":"completed","error":null,"incomplete_details":null,"output":null,"usage":` + okUsage + `}`,
		respIRRaw("completed", `"not-an-array"`, okUsage, ""),
		// Message contract.
		respIRRaw("completed", `[{"id":"m","type":"message","role":"user","status":"completed","content":[{"type":"output_text","text":"hi","annotations":[]}]}]`, okUsage, ""),
		respIRRaw("completed", `[{"id":"m","type":"message","role":"assistant","status":"weird","content":[{"type":"output_text","text":"hi","annotations":[]}]}]`, okUsage, ""),
		respIRRaw("completed", `[{"id":"m","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi","annotations":[{}]}]}]`, okUsage, ""),
		respIRRaw("completed", `[{"id":"m","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi","logprobs":[{}]}]}]`, okUsage, ""),
		respIRRaw("completed", `[{"id":"m","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","annotations":[]}]}]`, okUsage, ""),
		respIRRaw("completed", `[{"id":"m","type":"message","role":"assistant","status":"completed","content":[{"type":"weird","text":"hi"}]}]`, okUsage, ""),
		// Function-call contract: namespace/caller/async-true fail.
		respIRRaw("completed", `[{"type":"function_call","id":"f1","call_id":"c1","name":"a","arguments":"{}","namespace":"x"}]`, okUsage, ""),
		respIRRaw("completed", `[{"type":"function_call","id":"f1","call_id":"c1","name":"a","arguments":"{}","caller":"y"}]`, okUsage, ""),
		respIRRaw("completed", `[{"type":"function_call","id":"f1","call_id":"c1","name":"a","arguments":"{}","async":true}]`, okUsage, ""),
		// Duplicate identities.
		respIRRaw("completed", `[{"type":"function_call","id":"f1","call_id":"dup","name":"a","arguments":"{}"},{"type":"function_call","id":"f2","call_id":"dup","name":"b","arguments":"{}"}]`, okUsage, ""),
		respIRRaw("completed", `[{"id":"dup","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"a","annotations":[]}]},{"id":"dup","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"b","annotations":[]}]}]`, okUsage, ""),
		// background:true, non-text format, bad truncation.
		strings.Replace(respIRRaw("completed", okOut, okUsage, ""), `"status":"completed"`, `"status":"completed","background":true`, 1),
		strings.Replace(respIRRaw("completed", okOut, okUsage, ""), `"status":"completed"`, `"status":"completed","text":{"format":{"type":"json"}}`, 1),
		strings.Replace(respIRRaw("completed", okOut, okUsage, ""), `"status":"completed"`, `"status":"completed","truncation":"sometimes"`, 1),
	}
	for _, raw := range bad {
		rerr := respIRMustFail(t, raw)
		if rerr.Code != CodeUpstreamBad {
			t.Fatalf("code must be upstream_response_invalid for %.80s, got %v", raw, rerr)
		}
	}
	// async:false is the default and passes.
	respIRDecode(t, respIRRaw("completed",
		`[{"type":"function_call","id":"f1","call_id":"c1","name":"a","arguments":"{}","async":false}]`, okUsage, ""))
}

// --- Envelope echoes, created_at, metadata value-free ------------------------

func TestResponsesIREnvelopeEcho(t *testing.T) {
	okOut := `[{"id":"m","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi","annotations":[]}]}]`
	mk := func(extra string) string {
		return `{"id":"resp_1","object":"response","created_at":1746989954.0,"model":"up",` +
			`"status":"completed","error":null,"incomplete_details":null,` +
			`"output":` + okOut + `,"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}` + extra + `}`
	}
	// Realistic full echo succeeds.
	respIRDecode(t, mk(`, "background":false,"instructions":null,"max_output_tokens":null,`+
		`"parallel_tool_calls":true,"previous_response_id":null,`+
		`"reasoning":{"effort":null,"summary":null},"store":false,`+
		`"temperature":1.0,"text":{"format":{"type":"text"}},"tool_choice":"auto","tools":[],`+
		`"top_p":1.0,"truncation":"disabled","user":null,"metadata":{},"service_tier":"default",`+
		`"completed_at":1746989955.0`))
	// created_at accepts integer and float; quoted/bool fail.
	respIRDecode(t, strings.Replace(mk(""), "1746989954.0", "1746989954", 1))
	for _, bad := range []string{`"yesterday"`, `true`} {
		respIRMustFail(t, strings.Replace(mk(""), "1746989954.0", bad, 1))
	}
	// Metadata echoes are validated, never leaked into errors.
	rerr := respIRMustFail(t, mk(`, "mystery":"secret-value"`))
	if strings.Contains(rerr.Msg, "secret-value") {
		t.Fatalf("envelope error must be value-free, got %q", rerr.Msg)
	}
	if !strings.Contains(rerr.Msg, "response.mystery") {
		t.Fatalf("unknown echo must name path, got %q", rerr.Msg)
	}
}

// --- Bounds -------------------------------------------------------------------

func TestResponsesIRBounds(t *testing.T) {
	// Tiny limits force the bounds without huge fixtures.
	small := DefaultLimits()
	small.MaxArgsChars = 8
	_, rerr := DecodeResponsesResponse([]byte(respIRRaw("completed",
		`[{"id":"f1","type":"function_call","status":"completed","call_id":"c1","name":"a","arguments":"{\"k\":\"0123456789\"}"}]`,
		respIRUsage(5, 2), "")), small)
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("oversized args must fail, got %v", rerr)
	}
	// Aggregate text+args bound.
	small = DefaultLimits()
	small.MaxStreamArgsChars = 8
	_, rerr = DecodeResponsesResponse([]byte(respIRRaw("completed",
		`[{"id":"f1","type":"function_call","status":"completed","call_id":"c1","name":"a","arguments":"{\"k\":\"0123456789\"}"}]`,
		respIRUsage(5, 2), "")), small)
	if rerr == nil {
		t.Fatalf("aggregate bound must fail")
	}
	// Too many calls.
	small = DefaultLimits()
	small.MaxToolCalls = 1
	_, rerr = DecodeResponsesResponse([]byte(respIRRaw("completed",
		`[{"id":"f1","type":"function_call","status":"completed","call_id":"c1","name":"a","arguments":"{}"},`+
			`{"id":"f2","type":"function_call","status":"completed","call_id":"c2","name":"b","arguments":"{}"}]`,
		respIRUsage(5, 2), "")), small)
	if rerr == nil {
		t.Fatalf("too many calls must fail")
	}
}
