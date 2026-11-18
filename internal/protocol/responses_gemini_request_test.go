// Responses -> Gemini request codec tests: decode policy plus wire encoding.
package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func r2gDecode(t *testing.T, body string) (*Generation, []Loss) {
	t.Helper()
	g, losses, rerr := DecodeResponsesForGemini([]byte(body), DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode failed: %v", rerr)
	}
	return g, losses
}

func r2gDecodeErr(t *testing.T, body string) *RouterError {
	t.Helper()
	_, _, rerr := DecodeResponsesForGemini([]byte(body), DefaultLimits())
	if rerr == nil {
		t.Fatalf("expected error for %s", body)
	}
	return rerr
}

func r2gLossFields(losses []Loss) []string {
	fields := make([]string, 0, len(losses))
	for _, l := range losses {
		fields = append(fields, l.Field)
	}
	return fields
}

func TestResponsesGeminiTextRequestExact(t *testing.T) {
	g, losses := r2gDecode(t, `{"model":"m","instructions":"Be brief.","input":"Hi",
		"temperature":1.5,"top_p":0.9,"max_output_tokens":32}`)
	if len(losses) != 0 {
		t.Fatalf("losses = %v", losses)
	}
	if g.Model != "m" {
		t.Fatalf("model = %q", g.Model)
	}
	if g.Temperature == nil || *g.Temperature != 1.5 {
		t.Fatalf("temperature not preserved: %+v", g.Temperature)
	}
	out, rerr := EncodeGeminiRequest(g)
	if rerr != nil {
		t.Fatalf("encode: %v", rerr)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatalf("wire not JSON: %v", err)
	}
	for _, k := range []string{"model", "stream"} {
		if _, ok := body[k]; ok {
			t.Fatalf("wire must not carry %q: %s", k, out)
		}
	}
	var sys struct {
		Parts []struct {
			Text string `json:"text"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(body["systemInstruction"], &sys); err != nil {
		t.Fatalf("systemInstruction: %v", err)
	}
	if len(sys.Parts) != 1 || sys.Parts[0].Text != "Be brief." {
		t.Fatalf("systemInstruction = %s", body["systemInstruction"])
	}
	var gen map[string]json.RawMessage
	if err := json.Unmarshal(body["generationConfig"], &gen); err != nil {
		t.Fatalf("generationConfig: %v", err)
	}
	for _, k := range []string{"temperature", "topP", "maxOutputTokens"} {
		if _, ok := gen[k]; !ok {
			t.Fatalf("generationConfig missing %q: %s", k, body["generationConfig"])
		}
	}
}

func TestResponsesGeminiToolStrictFalseExact(t *testing.T) {
	g, losses := r2gDecode(t, `{"model":"m","input":"Hi","tools":[
		{"type":"function","name":"lookup","description":"d",
		 "parameters":{"type":"object"},"strict":false}]}`)
	if len(losses) != 0 {
		t.Fatalf("losses = %v", losses)
	}
	out, rerr := EncodeGeminiRequest(g)
	if rerr != nil {
		t.Fatalf("encode: %v", rerr)
	}
	if strings.Contains(string(out), "strict") {
		t.Fatalf("wire must not carry a strict guarantee: %s", out)
	}
	if !strings.Contains(string(out), "parametersJsonSchema") {
		t.Fatalf("wire must carry parametersJsonSchema: %s", out)
	}
}

func TestResponsesGeminiToolStrictOmittedLoss(t *testing.T) {
	_, losses := r2gDecode(t, `{"model":"m","input":"Hi","tools":[
		{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`)
	fields := r2gLossFields(losses)
	found := false
	for _, f := range fields {
		if f == "tools[0].strict" {
			found = true
		}
	}
	if !found {
		t.Fatalf("omitted strict must be a diagnosed loss, got %v", fields)
	}
	if _, rerr := EnforcePolicy(losses, false); rerr == nil {
		t.Fatalf("strict policy must refuse omitted strict")
	} else if rerr.Code != CodeTranslationLoss {
		t.Fatalf("strict refusal code = %q", rerr.Code)
	}
}

func TestResponsesGeminiToolStrictTrueLoss(t *testing.T) {
	_, losses := r2gDecode(t, `{"model":"m","input":"Hi","tools":[
		{"type":"function","name":"lookup","parameters":{"type":"object"},"strict":true}]}`)
	if len(losses) == 0 {
		t.Fatalf("explicit strict:true must be a diagnosed loss for Gemini")
	}
	if _, rerr := EnforcePolicy(losses, false); rerr == nil {
		t.Fatalf("strict policy must refuse explicit strict:true")
	}
	g, losses, rerr := DecodeResponsesForGemini([]byte(`{"model":"m","input":"Hi","tools":[
		{"type":"function","name":"lookup","parameters":{"type":"object"},"strict":true}]}`), DefaultLimits())
	if rerr != nil {
		t.Fatal(rerr)
	}
	if _, rerr := EnforcePolicy(losses, true); rerr != nil {
		t.Fatalf("lossy must keep the plan: %v", rerr)
	}
	out, rerr2 := EncodeGeminiRequest(g)
	if rerr2 != nil {
		t.Fatal(rerr2)
	}
	if strings.Contains(string(out), "strict") {
		t.Fatalf("lossy wire must not invent a strict guarantee: %s", out)
	}
}

func TestResponsesGeminiParallelToolCalls(t *testing.T) {
	_, losses := r2gDecode(t, `{"model":"m","input":"Hi","parallel_tool_calls":true}`)
	if len(losses) != 0 {
		t.Fatalf("parallel_tool_calls:true is exact, got %v", losses)
	}
	_, losses = r2gDecode(t, `{"model":"m","input":"Hi"}`)
	if len(losses) != 0 {
		t.Fatalf("omitted parallel_tool_calls is exact, got %v", losses)
	}
	_, losses = r2gDecode(t, `{"model":"m","input":"Hi","parallel_tool_calls":false}`)
	found := false
	for _, l := range losses {
		if l.Field == "parallel_tool_calls" {
			found = true
		}
	}
	if !found {
		t.Fatalf("parallel_tool_calls:false must be a diagnosed loss, got %v", losses)
	}
}

func TestResponsesGeminiStatefulRefused(t *testing.T) {
	bodies := []string{
		`{"model":"m","input":"Hi","previous_response_id":"resp_1"}`,
		`{"model":"m","input":"Hi","conversation":"conv_1"}`,
		`{"model":"m","input":"Hi","background":true}`,
		`{"model":"m","input":"Hi","prompt":{"id":"pmpt_1"}}`,
	}
	for _, b := range bodies {
		rerr := r2gDecodeErr(t, b)
		if rerr.Status != 400 {
			t.Fatalf("stateful %s: status = %d", b, rerr.Status)
		}
	}
}

func TestResponsesGeminiUnknownAndMalformed400(t *testing.T) {
	bodies := []string{
		`{"model":"m","input":"Hi","bogus_field":1}`,
		`{"model":"m","input":"Hi","tools":[{"type":"function","name":"f","parameters":{"type":"object"},"bogus":1}]}`,
		`{"model":"m","input":[{"type":"message","role":"user","content":"Hi","bogus":1}]}`,
		`{"model":"m","input":[{"type":"function_call","call_id":"c1","name":"f","arguments":"[1,2]"}]}`,
		`{"model":"m","input":[{"type":"function_call","call_id":"","name":"f","arguments":"{}"}]}`,
		`{"model":"m","input":[{"type":"message","role":"user"}]}`,
		`{"model":"m","input":[{"type":"function_call_output","call_id":"c9","output":"x"}]}`,
		`{"model":"m","temperature":"high","input":"Hi"}`,
	}
	for _, b := range bodies {
		rerr := r2gDecodeErr(t, b)
		if rerr.Status != 400 {
			t.Fatalf("body %s: status = %d, want 400", b, rerr.Status)
		}
	}
}

func TestResponsesGeminiToolChoice(t *testing.T) {
	g, losses := r2gDecode(t, `{"model":"m","input":"Hi",
		"tools":[{"type":"function","name":"f","parameters":{"type":"object"},"strict":false}],
		"tool_choice":"required"}`)
	if len(losses) != 0 {
		t.Fatalf("losses = %v", losses)
	}
	if g.Choice == nil || g.Choice.Mode != "any" {
		t.Fatalf("choice = %+v", g.Choice)
	}
	out, rerr := EncodeGeminiRequest(g)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !strings.Contains(string(out), `"mode":"ANY"`) {
		t.Fatalf("toolConfig mode ANY missing: %s", out)
	}
	g, _ = r2gDecode(t, `{"model":"m","input":"Hi",
		"tools":[{"type":"function","name":"f","parameters":{"type":"object"},"strict":false}],
		"tool_choice":{"type":"function","name":"f"}}`)
	if g.Choice == nil || g.Choice.Mode != "specific" || g.Choice.Name != "f" {
		t.Fatalf("choice = %+v", g.Choice)
	}
	_, losses = r2gDecode(t, `{"model":"m","input":"Hi",
		"tools":[{"type":"function","name":"f","parameters":{"type":"object"},"strict":false}],
		"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"f"}]}}`)
	if len(losses) == 0 {
		t.Fatalf("allowed tool sets must be a diagnosed loss")
	}
}

func TestResponsesGeminiHistoryRoundTrip(t *testing.T) {
	for _, content := range []string{"noon", "", "α\n🙂"} {
		quoted, _ := json.Marshal(content)
		raw := `{"model":"m","input":[
			{"type":"message","role":"user","content":"Hi"},
			{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":` + string(quoted) + `}]}`
		g, losses := r2gDecode(t, raw)
		if len(losses) != 0 {
			t.Fatalf("losses = %v", losses)
		}
		wire, rerr := EncodeGeminiRequest(g)
		if rerr != nil {
			t.Fatal(rerr)
		}
		var body struct {
			Contents []struct {
				Role  string `json:"role"`
				Parts []struct {
					Call struct {
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"functionCall"`
					Response struct {
						ID   string `json:"id"`
						Name string `json:"name"`
						Resp struct {
							Output string `json:"output"`
						} `json:"response"`
					} `json:"functionResponse"`
				} `json:"parts"`
			} `json:"contents"`
		}
		if err := json.Unmarshal(wire, &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Contents) != 3 {
			t.Fatalf("contents = %s", wire)
		}
		if body.Contents[1].Parts[0].Call.ID != "call_1" {
			t.Fatalf("call id not preserved: %s", wire)
		}
		got := body.Contents[2].Parts[0].Response.Resp.Output
		if got != content {
			t.Fatalf("tool result = %q, want %q", got, content)
		}
	}
}

func TestResponsesGeminiLargeIntegersExact(t *testing.T) {
	g, _ := r2gDecode(t, `{"model":"m","input":"Hi","tools":[
		{"type":"function","name":"f","parameters":{"type":"object",
		 "properties":{"n":{"type":"integer"}}},"strict":false}],
		"tool_choice":"none"}`)
	_ = g
	raw := `{"model":"m","input":[
		{"type":"message","role":"user","content":"Hi"},
		{"type":"function_call","call_id":"c1","name":"f","arguments":"{\"n\":9007199254740993}"}]}`
	g2, losses := r2gDecode(t, raw)
	if len(losses) != 0 {
		t.Fatalf("losses = %v", losses)
	}
	wire, rerr := EncodeGeminiRequest(g2)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !strings.Contains(string(wire), "9007199254740993") {
		t.Fatalf("large integer not preserved exactly: %s", wire)
	}
}
