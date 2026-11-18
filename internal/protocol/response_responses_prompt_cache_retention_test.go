package protocol

import (
	"strings"
	"testing"
)

// Live GPT-5.6 Luna Responses upstream echoes `prompt_cache_retention:"24h"`
// on ordinary generations without a cache policy (completed JSON and created
// SSE). Reviewed values ("24h", "in_memory") are exact absent controls with
// nothing to carry: validated and discarded. Malformed values are invalid;
// future string values have no representation. Billing usage `cost`/`opencode`
// is intentionally untouched here (root shared cross-source policy owns it).

func promptCacheRetentionFullRaw(extra string) string {
	okOut := `[{"id":"m","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi","annotations":[]}]}]`
	return `{"id":"resp_1","object":"response","created_at":1746989954.0,"model":"up",` +
		`"status":"completed","error":null,"incomplete_details":null,` +
		`"output":` + okOut + `,"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}` + extra + `}`
}

func TestResponsesPromptCacheRetentionFullAbsentControls(t *testing.T) {
	for _, extra := range []string{"", `,"prompt_cache_retention":null`, `,"prompt_cache_retention":"24h"`, `,"prompt_cache_retention":"in_memory"`} {
		raw := promptCacheRetentionFullRaw(extra)
		if _, rerr := DecodeResponsesResponse([]byte(raw), DefaultLimits()); rerr != nil {
			t.Fatalf("full extra %q must pass, got %v", extra, rerr)
		}
		// All three target encoders accept the same source (no cache policy to carry).
		for _, fn := range []func([]byte, string, Limits) ([]byte, *RouterError){
			TranslateResponsesJSONToChat, TranslateResponsesJSONToAnthropic, TranslateResponsesJSONToGemini,
		} {
			if _, rerr := fn([]byte(raw), "public", DefaultLimits()); rerr != nil {
				t.Fatalf("full extra %q must pass all targets, got %v", extra, rerr)
			}
		}
	}
}

func TestResponsesPromptCacheRetentionFullFutureUnrep(t *testing.T) {
	for _, extra := range []string{`,"prompt_cache_retention":"48h"`, `,"prompt_cache_retention":"30m"`, `,"prompt_cache_retention":""`, `,"prompt_cache_retention":"IN_MEMORY"`} {
		_, rerr := DecodeResponsesResponse([]byte(promptCacheRetentionFullRaw(extra)), DefaultLimits())
		if rerr == nil || rerr.Status != 502 {
			t.Fatalf("full extra %q must fail 502, got %v", extra, rerr)
		}
		if !strings.Contains(rerr.Msg, "response.prompt_cache_retention") || !strings.Contains(rerr.Msg, "has no representation") {
			t.Fatalf("future %q must name response.prompt_cache_retention as unrepresentable, got %q", extra, rerr.Msg)
		}
	}
}

func TestResponsesPromptCacheRetentionFullMalformedInvalid(t *testing.T) {
	for _, extra := range []string{
		`,"prompt_cache_retention":24`, `,"prompt_cache_retention":true`, `,"prompt_cache_retention":[]`, `,"prompt_cache_retention":{}`,
		`,"prompt_cache_retention":24.5`,
	} {
		_, rerr := DecodeResponsesResponse([]byte(promptCacheRetentionFullRaw(extra)), DefaultLimits())
		if rerr == nil || rerr.Status != 502 {
			t.Fatalf("full extra %q must fail 502, got %v", extra, rerr)
		}
		if rerr.Msg != "Upstream returned an invalid response" {
			t.Fatalf("malformed %q must be invalid, got %q", extra, rerr.Msg)
		}
	}
}

func promptCacheRetentionSSEUpstream(createdExtra, terminalExtra string) string {
	createdResp := `{"id":"resp_1","status":"in_progress"` + createdExtra + `}`
	terminalResp := `{"id":"resp_1","model":"gpt-4.1","status":"completed",` +
		`"output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi","annotations":[]}]}],` +
		`"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}` + terminalExtra + `}`
	return "event: response.created\ndata: {\"type\":\"response.created\",\"response\":" + createdResp + "}\n\n" +
		r2cItemAddedMsg() + r2cPartAdded() + r2cTextDelta("hi") +
		"event: response.output_text.done\ndata: {\"type\":\"response.output_text.done\",\"output_index\":0,\"content_index\":0,\"item_id\":\"msg_1\",\"text\":\"hi\"}\n\n" +
		r2cPartDone("hi") + r2cItemDoneMsg("hi") +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + terminalResp + "}\n\n"
}

func TestResponsesPromptCacheRetentionSSEEnvelopes(t *testing.T) {
	// Reviewed echoes on created + terminal pass end-to-end.
	out, rerr := runR2CStream(t, promptCacheRetentionSSEUpstream(`,"prompt_cache_retention":"24h"`, `,"prompt_cache_retention":"24h"`), true, DefaultLimits())
	if rerr != nil {
		t.Fatalf("SSE reviewed echoes must pass, got %v", rerr)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Fatalf("SSE reviewed echoes missing terminator: %s", out)
	}
	out, rerr = runR2CStream(t, promptCacheRetentionSSEUpstream(`,"prompt_cache_retention":"in_memory"`, `,"prompt_cache_retention":"in_memory"`), true, DefaultLimits())
	if rerr != nil {
		t.Fatalf("SSE in_memory echoes must pass, got %v", rerr)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Fatalf("SSE in_memory missing terminator: %s", out)
	}
	// Absent/null on created + terminal passes.
	out, rerr = runR2CStream(t, promptCacheRetentionSSEUpstream("", ""), true, DefaultLimits())
	if rerr != nil {
		t.Fatalf("SSE absent echoes must pass, got %v", rerr)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Fatalf("SSE absent missing terminator: %s", out)
	}
	// Future value on created fails as unrepresentable before success.
	_, rerr = runR2CStream(t, promptCacheRetentionSSEUpstream(`,"prompt_cache_retention":"48h"`, `,"prompt_cache_retention":"24h"`), true, DefaultLimits())
	if rerr == nil || rerr.Status != 502 || !strings.Contains(rerr.Msg, "response.prompt_cache_retention") {
		t.Fatalf("SSE future created must fail naming response.prompt_cache_retention, got %v", rerr)
	}
	// Future value on terminal fails as unrepresentable.
	_, rerr = runR2CStream(t, promptCacheRetentionSSEUpstream(`,"prompt_cache_retention":"24h"`, `,"prompt_cache_retention":"48h"`), true, DefaultLimits())
	if rerr == nil || rerr.Status != 502 || !strings.Contains(rerr.Msg, "response.prompt_cache_retention") {
		t.Fatalf("SSE future terminal must fail naming response.prompt_cache_retention, got %v", rerr)
	}
	// Malformed on created fails invalid.
	_, rerr = runR2CStream(t, promptCacheRetentionSSEUpstream(`,"prompt_cache_retention":24`, `,"prompt_cache_retention":"24h"`), true, DefaultLimits())
	if rerr == nil || rerr.Status != 502 || rerr.Msg != "Upstream returned an invalid response" {
		t.Fatalf("SSE malformed created must be invalid, got %v", rerr)
	}
	// Identity/terminal strictness preserved even with the reviewed echo present:
	// response identity changing at the terminal still fails.
	badIdentity := strings.Replace(
		promptCacheRetentionSSEUpstream(`,"prompt_cache_retention":"24h"`, `,"prompt_cache_retention":"24h"`),
		`"id":"resp_1","model":"gpt-4.1"`, `"id":"resp_2","model":"gpt-4.1"`, 1)
	_, rerr = runR2CStream(t, badIdentity, true, DefaultLimits())
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("identity change with reviewed echo must still fail, got %v", rerr)
	}
	// Terminal status contradicting the event kind still fails.
	badTerminal := strings.Replace(
		promptCacheRetentionSSEUpstream(`,"prompt_cache_retention":"24h"`, `,"prompt_cache_retention":"24h"`),
		`"status":"completed","output"`, `"status":"incomplete","output"`, 1)
	_, rerr = runR2CStream(t, badTerminal, true, DefaultLimits())
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("status contradiction with reviewed echo must still fail, got %v", rerr)
	}
}
