package server

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// Chat -> Responses native custom at actual HTTP runtime: the request
// ToolPlan threads into the JSON and stream response paths. Native
// passthrough succeeds; family mismatches refuse and never reinterpret
// compacted JSON args as custom input.
func TestChatToResponsesCustomPlanJSONBranches(t *testing.T) {
	reqBody := `{"model":"bridge-chat","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"custom","custom":{"name":"patch"}}]}`

	// Native custom with declared name passes through.
	h := newResponsesBridgeHarness(t, responsesBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"gpt-4.1","output":[{"type":"custom_tool_call","id":"ctc_1","call_id":"c1","name":"patch","input":"hello","status":"completed"}],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}`)
	}
	status, _, data := postResponsesBridge(t, h.router.URL+"/v1/chat/completions", reqBody, nil)
	if status != 200 {
		t.Fatalf("native custom must pass, status=%d body=%s", status, data)
	}
	if !strings.Contains(string(data), `"type":"custom"`) || !strings.Contains(string(data), "hello") {
		t.Fatalf("native custom not preserved: %s", data)
	}

	// Declared custom returned as function refuses (no facade repair).
	h2 := newResponsesBridgeHarness(t, responsesBridgeCfg(t))
	h2.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"gpt-4.1","output":[{"type":"function_call","id":"fc_1","call_id":"c1","name":"patch","arguments":"{}","status":"completed"}],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}`)
	}
	status, _, data = postResponsesBridge(t, h2.router.URL+"/v1/chat/completions", reqBody, nil)
	if status != 502 {
		t.Fatalf("function with declared custom name must 502, got status=%d body=%s", status, data)
	}

	// Hallucinated custom (name never declared) refuses; ordinary unknown
	// functions still pass.
	h3 := newResponsesBridgeHarness(t, responsesBridgeCfg(t))
	h3.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"gpt-4.1","output":[{"type":"custom_tool_call","id":"ctc_1","call_id":"c1","name":"ghost","input":"x","status":"completed"}],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}`)
	}
	status, _, data = postResponsesBridge(t, h3.router.URL+"/v1/chat/completions", reqBody, nil)
	if status != 502 {
		t.Fatalf("undeclared custom must 502, got status=%d body=%s", status, data)
	}

	h4 := newResponsesBridgeHarness(t, responsesBridgeCfg(t))
	h4.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"gpt-4.1","output":[{"type":"function_call","id":"fc_1","call_id":"c1","name":"lookup","arguments":"{}","status":"completed"}],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}`)
	}
	status, _, data = postResponsesBridge(t, h4.router.URL+"/v1/chat/completions", reqBody, nil)
	if status != 200 {
		t.Fatalf("ordinary unknown function must pass, status=%d body=%s", status, data)
	}
}

// Stream path threads the same plan: a declared custom returned as a
// function fails the stream without a synthetic success marker.
func TestChatToResponsesCustomPlanStreamBranch(t *testing.T) {
	reqBody := `{"model":"bridge-chat","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"custom","custom":{"name":"patch"}}],"stream":true}`
	h := newResponsesBridgeHarness(t, responsesBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		frames := []string{
			"event: response.created\ndata: {\"type\": \"response.created\", \"response\": {\"id\": \"resp_1\", \"object\": \"response\", \"created_at\": 1, \"status\": \"in_progress\", \"model\": \"gpt-4.1\", \"output\": []}, \"sequence_number\": 0}\n\n",
			"event: response.output_item.added\ndata: {\"type\": \"response.output_item.added\", \"output_index\": 0, \"item\": {\"id\": \"fc_1\", \"type\": \"function_call\", \"call_id\": \"c1\", \"name\": \"patch\", \"arguments\": \"\", \"status\": \"in_progress\"}, \"sequence_number\": 1}\n\n",
			"event: response.function_call_arguments.delta\ndata: {\"type\": \"response.function_call_arguments.delta\", \"output_index\": 0, \"item_id\": \"fc_1\", \"delta\": \"{}\", \"sequence_number\": 2}\n\n",
			"event: response.function_call_arguments.done\ndata: {\"type\": \"response.function_call_arguments.done\", \"output_index\": 0, \"item_id\": \"fc_1\", \"arguments\": \"{}\", \"sequence_number\": 3}\n\n",
			"event: response.output_item.done\ndata: {\"type\": \"response.output_item.done\", \"output_index\": 0, \"item\": {\"id\": \"fc_1\", \"type\": \"function_call\", \"call_id\": \"c1\", \"name\": \"patch\", \"arguments\": \"{}\", \"status\": \"completed\"}, \"sequence_number\": 4}\n\n",
			"event: response.completed\ndata: {\"type\": \"response.completed\", \"response\": {\"id\": \"resp_1\", \"object\": \"response\", \"created_at\": 1, \"status\": \"completed\", \"model\": \"gpt-4.1\", \"output\": [{\"id\": \"fc_1\", \"type\": \"function_call\", \"call_id\": \"c1\", \"name\": \"patch\", \"arguments\": \"{}\", \"status\": \"completed\"}], \"usage\": {\"input_tokens\": 7, \"output_tokens\": 3, \"total_tokens\": 10}}, \"sequence_number\": 5}\n\n",
		}
		for _, f := range frames {
			_, _ = io.WriteString(w, f)
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	status, _, data := postResponsesBridge(t, h.router.URL+"/v1/chat/completions", reqBody, nil)
	text := string(data)
	// The role chunk commits at start, so a mid-stream family mismatch
	// keeps 200 post-commit but terminates without success markers.
	if status != 200 && status != 502 {
		t.Fatalf("stream family mismatch must be 200 post-commit or 502 pre-commit, got %d body=%s", status, text)
	}
	if strings.Contains(text, "data: [DONE]") {
		t.Fatalf("refused stream must not emit [DONE]: %s", text)
	}
	if strings.Contains(text, `"finish_reason":"tool_calls"`) {
		t.Fatalf("refused stream must not fabricate finish: %s", text)
	}
}

func TestChatToResponsesRejectsCustomWithoutToolPlan(t *testing.T) {
	h := newResponsesBridgeHarness(t, responsesBridgeCfg(t))
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"gpt-4.1","output":[{"type":"custom_tool_call","id":"ctc_1","call_id":"c1","name":"ghost","input":"x","status":"completed"}],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}`)
	}
	status, _, data := postResponsesBridge(t, h.router.URL+"/v1/chat/completions",
		`{"model":"bridge-chat","messages":[{"role":"user","content":"hi"}]}`, nil)
	if status != 502 {
		t.Fatalf("an unsolicited custom call must be rejected even when no tools were requested: %d %s", status, data)
	}
}
