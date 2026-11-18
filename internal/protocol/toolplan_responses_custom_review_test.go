package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestReviewCustomMissingInputMust400(t *testing.T) {
	for _, raw := range []string{
		`{"model":"m","input":[{"type":"custom_tool_call","call_id":"c1","name":"patch"}],"tools":[{"type":"custom","name":"patch"}]}`,
		`{"model":"m","input":[{"type":"custom_tool_call","call_id":"c1","name":"patch","input":null}],"tools":[{"type":"custom","name":"patch"}]}`,
	} {
		if _, _, rerr := decodeResponsesSource([]byte(raw), DefaultLimits(), responsesDestChat); rerr == nil || rerr.Status != 400 {
			t.Fatalf("missing/null custom input must 400, got %v for %s", rerr, raw)
		}
	}
	// Empty string is valid in both (exact empty freeform).
	rawEmpty := `{"model":"m","input":[{"type":"custom_tool_call","call_id":"c1","name":"patch","input":""}],"tools":[{"type":"custom","name":"patch"}]}`
	if _, _, rerr := decodeResponsesSource([]byte(rawEmpty), DefaultLimits(), responsesDestChat); rerr != nil {
		t.Fatalf("empty string custom input must be accepted, got %v", rerr)
	}
}

func TestReviewCustomRestoreRespectsLimits(t *testing.T) {
	rawReq := `{"model":"m-resp","input":"hi","tools":[{"type":"custom","name":"patch"}]}`
	gen, _, rerr := decodeResponsesSource([]byte(rawReq), DefaultLimits(), responsesDestChat)
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	bigInput := strings.Repeat("x", 100)
	args, _ := json.Marshal(map[string]string{"content": bigInput})
	id, rerr2 := NewItemID("item_1")
	if rerr2 != nil {
		t.Fatalf("id: %v", rerr2)
	}
	call, rerr2 := NewCallID("call_1")
	if rerr2 != nil {
		t.Fatalf("call: %v", rerr2)
	}
	item, rerr2 := NewToolCallItem(id, call, "patch", json.RawMessage(args))
	if rerr2 != nil {
		t.Fatalf("item: %v", rerr2)
	}
	resp := Response{Source: DialectChat, Items: []ResponseItem{item}}
	smallLim := DefaultLimits()
	smallLim.MaxArgsChars = 5
	if _, rerr := gen.ToolPlan.RestoreResponse(resp, smallLim); rerr == nil || rerr.Status != 502 {
		t.Fatalf("the caller's 5-byte limit must reject the emulated wrapper: %v", rerr)
	}
	if _, rerr := gen.ToolPlan.RestoreResponse(resp, DefaultLimits()); rerr != nil {
		t.Fatalf("the same valid wrapper must fit the default limit: %v", rerr)
	}
}

func TestReviewCustomCallIDConsistentOpaque(t *testing.T) {
	const opaque = "has:colon and space"
	for _, pair := range []struct{ call, result string }{
		{
			`{"type":"function_call","call_id":"has:colon and space","name":"lookup","arguments":"{}"}`,
			`{"type":"function_call_output","call_id":"has:colon and space","output":"done"}`,
		},
		{
			`{"type":"custom_tool_call","call_id":"has:colon and space","name":"patch","input":"hi"}`,
			`{"type":"custom_tool_call_output","call_id":"has:colon and space","output":"done"}`,
		},
	} {
		raw := `{"model":"m","input":[` + pair.call + "," + pair.result + `]}`
		for _, dest := range []responsesDestination{responsesDestChat, responsesDestAnthropic, responsesDestGemini} {
			gen, _, err := decodeResponsesSource([]byte(raw), DefaultLimits(), dest)
			if err != nil {
				t.Fatalf("destination %d must accept opaque call and result IDs: %v", dest, err)
			}
			if gen.Messages[0].ToolCalls[0].ID != opaque || gen.Messages[1].Results[0].CallID != opaque {
				t.Fatalf("destination %d must preserve both opaque IDs", dest)
			}
		}
	}
}
