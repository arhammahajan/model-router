// Anthropic gateway billing metadata: validated lossy exclusion.
//
// An external gateway may attach usage.cost (numeric, observed 0) and
// usage.opencode {billing_source, list_cost, allowance, tokens{...}} at the
// terminal JSON and SSE delta. These carry no representation downstream:
// strict fails closed, lossy validates the shape and drops them, never
// forwarding cost or allowance state. Authoritative counters and terminal
// verification are unchanged; native passthrough is unaffected.
package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func gatewayBillingUsage(costFrag, opencodeFrag string) string {
	base := `{"input_tokens":14,"output_tokens":4}`
	if costFrag != "" {
		base = strings.Replace(base, `"output_tokens":4`, `"output_tokens":4,"cost":`+costFrag, 1)
	}
	if opencodeFrag != "" {
		base = strings.Replace(base, `"output_tokens":4`, `"output_tokens":4,"opencode":`+opencodeFrag, 1)
	}
	return base
}

func gatewayValidOpencode() string {
	return `{"billing_source":"gateway","list_cost":0.5,"allowance":100,` +
		`"tokens":{"input":14,"output":4,"reasoning":0,"cache_read":0,"cache_write":0,"cache_write_5m":0,"cache_write_1h":0}}`
}

func TestGatewayBillingCompleteStrictFails(t *testing.T) {
	content := `[{"type":"text","text":"OK"}]`
	for _, usage := range []string{
		gatewayBillingUsage(`0`, ""),
		gatewayBillingUsage(`0`, gatewayValidOpencode()),
		gatewayBillingUsage(``, gatewayValidOpencode()),
	} {
		raw := anthIRRaw("end_turn", content, usage)
		if _, rerr := DecodeAnthropicResponse([]byte(raw), DefaultLimits()); rerr == nil || rerr.Status != 502 {
			t.Fatalf("strict must fail for %s, got %v", usage, rerr)
		}
	}
}

func TestGatewayBillingCompleteLossyDrops(t *testing.T) {
	content := `[{"type":"text","text":"OK"}]`
	usage := gatewayBillingUsage(`0`, gatewayValidOpencode())
	raw := anthIRRaw("end_turn", content, usage)
	resp, rerr := DecodeAnthropicResponseWithPolicy([]byte(raw), DefaultLimits(), true)
	if rerr != nil {
		t.Fatalf("lossy must drop validated billing: %v", rerr)
	}
	if resp.Usage.Prompt == nil || *resp.Usage.Prompt != 14 || resp.Usage.Completion == nil || *resp.Usage.Completion != 4 {
		t.Fatalf("authoritative counters lost: %+v", resp.Usage)
	}
	// Translated Chat envelope carries no gateway fields.
	out, rerr := TranslateAnthropicJSONWithPolicy([]byte(raw), "public", DefaultLimits(), true)
	if rerr != nil {
		t.Fatalf("translate lossy: %v", rerr)
	}
	s := string(out)
	if strings.Contains(s, "cost") || strings.Contains(s, "opencode") || strings.Contains(s, "billing_source") || strings.Contains(s, "list_cost") {
		t.Fatalf("gateway metadata leaked downstream: %s", s)
	}
	// Responses and Gemini translators drop identically.
	if _, rerr := TranslateAnthropicJSONToResponsesWithPolicy([]byte(raw), "public", DefaultLimits(), false); rerr == nil {
		t.Fatalf("responses strict must fail")
	}
	out, rerr = TranslateAnthropicJSONToResponsesWithPlanAndPolicy([]byte(raw), "public", DefaultLimits(), nil, true)
	if rerr != nil {
		t.Fatalf("responses lossy: %v", rerr)
	}
	if strings.Contains(string(out), "opencode") {
		t.Fatalf("responses leaked gateway metadata: %s", out)
	}
	out, rerr = TranslateAnthropicJSONToGeminiWithPolicy([]byte(raw), "public", DefaultLimits(), true)
	if rerr != nil {
		t.Fatalf("gemini lossy: %v", rerr)
	}
	if strings.Contains(string(out), "opencode") {
		t.Fatalf("gemini leaked gateway metadata: %s", out)
	}
}

func TestGatewayBillingNullAbsent(t *testing.T) {
	content := `[{"type":"text","text":"OK"}]`
	usage := gatewayBillingUsage(`null`, `null`)
	raw := anthIRRaw("end_turn", content, usage)
	if _, rerr := DecodeAnthropicResponse([]byte(raw), DefaultLimits()); rerr != nil {
		t.Fatalf("null billing must be absent in strict: %v", rerr)
	}
	if _, rerr := DecodeAnthropicResponseWithPolicy([]byte(raw), DefaultLimits(), true); rerr != nil {
		t.Fatalf("null billing must be absent in lossy: %v", rerr)
	}
}

func TestGatewayBillingMalformedFailsBoth(t *testing.T) {
	content := `[{"type":"text","text":"OK"}]`
	badUsages := []string{
		gatewayBillingUsage(`"0"`, ""),
		gatewayBillingUsage(`-1`, ""),
		gatewayBillingUsage(`{"x":1}`, ""),
		gatewayBillingUsage(``, `{"billing_source":"g"}`),
		gatewayBillingUsage(``, `{"billing_source":"g","list_cost":0,"allowance":1,"tokens":{"input":1}}`),
		gatewayBillingUsage(``, `{"billing_source":"g","list_cost":0,"allowance":1,"tokens":{"input":1,"output":1,"reasoning":0,"cache_read":0,"cache_write":0,"cache_write_5m":0,"cache_write_1h":0,"future":0}}`),
		gatewayBillingUsage(``, `{"billing_source":"g","list_cost":0,"allowance":1,"tokens":{"input":1,"output":1,"reasoning":0,"cache_read":0,"cache_write":0,"cache_write_5m":0,"cache_write_1h":0},"future":1}`),
		gatewayBillingUsage(``, `{"billing_source":7,"list_cost":0,"allowance":1,"tokens":{"input":1,"output":1,"reasoning":0,"cache_read":0,"cache_write":0,"cache_write_5m":0,"cache_write_1h":0}}`),
		gatewayBillingUsage(``, `{"billing_source":"g","list_cost":"x","allowance":1,"tokens":{"input":1,"output":1,"reasoning":0,"cache_read":0,"cache_write":0,"cache_write_5m":0,"cache_write_1h":0}}`),
		gatewayBillingUsage(``, `{"billing_source":"g","list_cost":0,"allowance":1,"tokens":{"input":"1","output":1,"reasoning":0,"cache_read":0,"cache_write":0,"cache_write_5m":0,"cache_write_1h":0}}`),
	}
	for _, usage := range badUsages {
		raw := anthIRRaw("end_turn", content, usage)
		if _, rerr := DecodeAnthropicResponse([]byte(raw), DefaultLimits()); rerr == nil {
			t.Fatalf("strict must fail for %s", usage)
		}
		if _, rerr := DecodeAnthropicResponseWithPolicy([]byte(raw), DefaultLimits(), true); rerr == nil {
			t.Fatalf("lossy must fail for malformed %s", usage)
		}
	}
}

func TestGatewayBillingValueFree(t *testing.T) {
	secret := "super-secret-billing-token-xyz"
	content := `[{"type":"text","text":"OK"}]`
	opencode := `{"billing_source":"` + secret + `","list_cost":0,"allowance":1,` +
		`"tokens":{"input":1,"output":1,"reasoning":0,"cache_read":0,"cache_write":0,"cache_write_5m":0,"cache_write_1h":0}}`
	raw := anthIRRaw("end_turn", content, gatewayBillingUsage(`0`, opencode))
	_, rerr := DecodeAnthropicResponse([]byte(raw), DefaultLimits())
	if rerr == nil || strings.Contains(rerr.Msg, secret) {
		t.Fatalf("secret leaked in strict diagnostic: %v", rerr)
	}
	// Unknown nested key value must not echo either.
	raw2 := anthIRRaw("end_turn", content, gatewayBillingUsage(``, `{"billing_source":"g","list_cost":0,"allowance":1,"tokens":{"input":1,"output":1,"reasoning":0,"cache_read":0,"cache_write":0,"cache_write_5m":0,"cache_write_1h":0},"`+secret+`":1}`))
	_, rerr = DecodeAnthropicResponseWithPolicy([]byte(raw2), DefaultLimits(), true)
	if rerr == nil || strings.Contains(rerr.Msg, secret) {
		t.Fatalf("secret key leaked: %v", rerr)
	}
}

func TestGatewayBillingStreamDelta(t *testing.T) {
	full := func(usage string) []Frame {
		return []Frame{
			asFr("message_start", asStartData(asMinMsg(`"m1"`, `{"input_tokens":14}`))),
			asFr("content_block_start", asBlockStart(0, `{"type":"text","text":""}`)),
			asFr("content_block_delta", asDelta(0, `{"type":"text_delta","text":"OK"}`)),
			asFr("content_block_stop", asBlockStop(0)),
			asFr("message_delta", asMsgDelta(`{"stop_reason":"end_turn"}`, usage)),
			asFr("message_stop", `{"type":"message_stop"}`),
		}
	}
	usage := `{"output_tokens":4,"cost":0,"opencode":` + gatewayValidOpencode() + `}`
	// Strict fails at delta.
	dec := NewAnthropicStreamDecoder(DefaultLimits())
	failed := false
	for _, f := range full(usage) {
		if _, rerr := dec.DecodeFrame(f); rerr != nil {
			failed = true
			if strings.Contains(rerr.Msg, "billing_source") || strings.Contains(rerr.Msg, "0.5") {
				t.Fatalf("stream diagnostic leaked values: %v", rerr)
			}
			break
		}
	}
	if !failed {
		if _, rerr := dec.Close(); rerr == nil {
			t.Fatalf("strict stream must fail on gateway billing")
		}
	}
	// Lossy drops and completes with authoritative usage.
	decLossy := NewAnthropicStreamDecoderWithPolicy(DefaultLimits(), true)
	var all []ResponseEvent
	for _, f := range full(usage) {
		evs, rerr := decLossy.DecodeFrame(f)
		if rerr != nil {
			t.Fatalf("lossy stream must drop validated billing: %v", rerr)
		}
		all = append(all, evs...)
	}
	if _, rerr := decLossy.Close(); rerr != nil {
		t.Fatalf("lossy Close: %v", rerr)
	}
	found := false
	for _, ev := range all {
		if ue, ok := ev.(UsageEvent); ok && ue.Terminal() {
			found = true
			if ue.Usage.Completion == nil || *ue.Usage.Completion != 4 {
				t.Fatalf("terminal completion lost: %+v", ue.Usage)
			}
			if ue.Usage.Prompt == nil || *ue.Usage.Prompt != 14 {
				t.Fatalf("terminal prompt lost: %+v", ue.Usage)
			}
		}
	}
	if !found {
		t.Fatalf("no terminal usage emitted")
	}
	// Malformed delta billing fails even lossy; missing output still fails.
	badDelta := `{"output_tokens":4,"cost":"0"}`
	decBad := NewAnthropicStreamDecoderWithPolicy(DefaultLimits(), true)
	failed = false
	for _, f := range full(badDelta) {
		if _, rerr := decBad.DecodeFrame(f); rerr != nil {
			failed = true
			break
		}
	}
	if !failed {
		t.Fatalf("malformed stream billing must fail lossy")
	}
	truncated := []Frame{
		asFr("message_start", asStartData(asMinMsg(`"m1"`, `{"input_tokens":14}`))),
		asFr("content_block_start", asBlockStart(0, `{"type":"text","text":""}`)),
		asFr("content_block_stop", asBlockStop(0)),
		asFr("message_delta", asMsgDelta(`{"stop_reason":"end_turn"}`, `{"cost":0}`)),
		asFr("message_stop", `{"type":"message_stop"}`),
	}
	decTrunc := NewAnthropicStreamDecoderWithPolicy(DefaultLimits(), true)
	for _, f := range truncated {
		if _, rerr := decTrunc.DecodeFrame(f); rerr != nil {
			return // missing output fails as before; gateway drop does not invent it
		}
	}
	if _, rerr := decTrunc.Close(); rerr == nil {
		t.Fatalf("terminal verification must still require output")
	}
}

func TestGatewayBillingValidatorShared(t *testing.T) {
	// Direct validator: reusable for Chat/Responses rows; mutates only to drop.
	obj := map[string]json.RawMessage{
		"cost":     json.RawMessage(`0`),
		"opencode": json.RawMessage(gatewayValidOpencode()),
	}
	if rerr := validateGatewayBillingMetadata(obj, "response.usage", false); rerr == nil {
		t.Fatalf("strict validator must fail")
	}
	if _, ok := obj["cost"]; !ok {
		t.Fatalf("strict validator must not drop")
	}
	if rerr := validateGatewayBillingMetadata(obj, "response.usage", true); rerr != nil {
		t.Fatalf("lossy validator must drop: %v", rerr)
	}
	if _, ok := obj["cost"]; ok {
		t.Fatalf("lossy validator must delete cost")
	}
	if _, ok := obj["opencode"]; ok {
		t.Fatalf("lossy validator must delete opencode")
	}
}
