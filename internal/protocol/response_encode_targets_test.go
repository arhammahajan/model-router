// Shared target-encoder cross-product: direct IR fixtures, no source reparse.
//
// These tests prove the four Encode* targets are source-neutral: the same
// typed Response encodes per-target, with ordered blocks preserved where
// the wire allows it and explicit refusal where it does not. Fixtures are
// hand-built IR (NewTerminalUsage/WithCache/NewResponse), grounded in the
// legacy pair translators cited per case; no Decode* or pair translator is
// called here.
package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustUsage(t *testing.T, source Dialect, prompt, completion int) ResponseUsage {
	t.Helper()
	u, rerr := NewTerminalUsage(source, prompt, completion, prompt+completion)
	if rerr != nil {
		t.Fatalf("NewTerminalUsage: %v", rerr)
	}
	return u
}

func mustResponse(t *testing.T, source Dialect, items []ResponseItem, usage ResponseUsage, finish FinishKind) Response {
	t.Helper()
	id, _ := NewResponseID("r1")
	resp, rerr := NewResponse(id, "m", source, items, usage, Finish{Kind: finish, Raw: string(finish)})
	if rerr != nil {
		t.Fatalf("NewResponse(%s): %v", source, rerr)
	}
	return resp
}

func decodeJSON(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	return v
}

func toolItem(id, call, name string) ToolCallItem {
	args, _ := CompactArgs([]byte(`{"n":1}`))
	return ToolCallItem{ID: ItemID(id), CallID: CallID(call), Name: name, Arguments: args}
}

// --- Ordered tool-first: preserved in block wires, refused in Chat ---------

func TestEncodeOrderedToolFirst(t *testing.T) {
	items := []ResponseItem{toolItem("i1", "call-1", "f"), TextItem{ID: "t0", Text: "hello"}}
	for _, source := range []Dialect{DialectAnthropic, DialectResponses, DialectGemini, DialectChat} {
		// Chat source with tool-first cannot arise from DecodeChatResponse
		// (it always emits text first), but the gate is source-neutral, so
		// the same order policy applies: Chat target refuses it.
		resp := mustResponse(t, source, items, mustUsage(t, source, 5, 2), FinishStop)
		a, rerr := EncodeAnthropicResponse(resp, "p")
		if rerr != nil {
			t.Fatalf("%s -> anthropic: %v", source, rerr)
		}
		content := decodeJSON(t, a)["content"].([]any)
		if source == DialectChat {
			// Chat source keeps the proven refusal-wins fold (text
			// first, then tools); tool-first IR cannot arise from
			// DecodeChatResponse, which always emits text first.
			if len(content) != 2 || content[0].(map[string]any)["type"] != "text" {
				t.Fatalf("%s -> anthropic fold lost: %s", source, a)
			}
		} else if len(content) != 2 || content[0].(map[string]any)["type"] != "tool_use" {
			t.Fatalf("%s -> anthropic order lost: %s", source, a)
		}
		r, rerr := EncodeResponsesResponse(resp, "p")
		if rerr != nil {
			t.Fatalf("%s -> responses: %v", source, rerr)
		}
		output := decodeJSON(t, r)["output"].([]any)
		if source == DialectChat {
			// Chat collapse: single message then function_call.
			if len(output) != 2 || output[0].(map[string]any)["type"] != "message" {
				t.Fatalf("%s -> responses collapse lost: %s", source, r)
			}
		} else if len(output) != 2 || output[0].(map[string]any)["type"] != "function_call" {
			t.Fatalf("%s -> responses order lost: %s", source, r)
		}
		g, rerr := EncodeGeminiResponse(resp, "p")
		if rerr != nil {
			t.Fatalf("%s -> gemini: %v", source, rerr)
		}
		parts := decodeJSON(t, g)["candidates"].([]any)[0].(map[string]any)["content"].(map[string]any)["parts"].([]any)
		if len(parts) != 2 || parts[0].(map[string]any)["functionCall"] == nil {
			t.Fatalf("%s -> gemini order lost: %s", source, g)
		}
		// Chat wire (one content string + tool_calls array) cannot
		// distinguish [tool,text] from [text,tool]: explicit refusal,
		// never a silent reorder (tightening vs legacy concatenation).
		if _, rerr := EncodeChatResponse(resp, "p"); rerr == nil {
			t.Fatalf("%s tool-first accepted by chat; must refuse order", source)
		}
	}
}

// --- Text-call-text interleaving --------------------------------------------

func TestEncodeTextCallText(t *testing.T) {
	items := []ResponseItem{
		TextItem{ID: "t0", Text: "a"},
		toolItem("i1", "c1", "f"),
		TextItem{ID: "t1", Text: "b"},
	}
	resp := mustResponse(t, DialectAnthropic, items, mustUsage(t, DialectAnthropic, 5, 2), FinishStop)
	a, rerr := EncodeAnthropicResponse(resp, "p")
	if rerr != nil {
		t.Fatalf("anthropic: %v", rerr)
	}
	content := decodeJSON(t, a)["content"].([]any)
	if len(content) != 3 || content[1].(map[string]any)["type"] != "tool_use" {
		t.Fatalf("anthropic interleave lost: %s", a)
	}
	r, rerr := EncodeResponsesResponse(resp, "p")
	if rerr != nil {
		t.Fatalf("responses: %v", rerr)
	}
	output := decodeJSON(t, r)["output"].([]any)
	if len(output) != 3 || output[1].(map[string]any)["type"] != "function_call" {
		t.Fatalf("responses interleave lost: %s", r)
	}
	g, rerr := EncodeGeminiResponse(resp, "p")
	if rerr != nil {
		t.Fatalf("gemini: %v", rerr)
	}
	parts := decodeJSON(t, g)["candidates"].([]any)[0].(map[string]any)["content"].(map[string]any)["parts"].([]any)
	if len(parts) != 3 || parts[1].(map[string]any)["functionCall"] == nil {
		t.Fatalf("gemini interleave lost: %s", g)
	}
	if _, rerr := EncodeChatResponse(resp, "p"); rerr == nil {
		t.Fatal("text-call-text accepted by chat; must refuse order")
	}
}

// --- Usage: Anthropic disjoint shares subtract both --------------------------

func TestEncodeAnthropicUsageSubtractsBoth(t *testing.T) {
	// Legacy wire: input=10, creation=3, read=4 (prompt 17), output 7.
	// Proven by anthropic_to_chat.go prompt() and a2gemUsageFromScalars.
	u := mustUsage(t, DialectAnthropic, 17, 7)
	var rerr *RouterError
	u, rerr = u.WithCache(intPtr(3), intPtr(4))
	if rerr != nil {
		t.Fatalf("WithCache: %v", rerr)
	}
	resp := mustResponse(t, DialectAnthropic, []ResponseItem{TextItem{ID: "t", Text: "x"}}, u, FinishStop)
	a, rerr := EncodeAnthropicResponse(resp, "p")
	if rerr != nil {
		t.Fatalf("anthropic: %v", rerr)
	}
	au := decodeJSON(t, a)["usage"].(map[string]any)
	// input_tokens excludes BOTH shares (r2aParseUsage proves the same
	// subtraction for Responses input).
	if au["input_tokens"] != float64(10) || au["cache_creation_input_tokens"] != float64(3) || au["cache_read_input_tokens"] != float64(4) {
		t.Fatalf("anthropic usage: %v", au)
	}
	g, rerr := EncodeGeminiResponse(resp, "p")
	if rerr != nil {
		t.Fatalf("gemini: %v", rerr)
	}
	gu := decodeJSON(t, g)["usageMetadata"].(map[string]any)
	if gu["promptTokenCount"] != float64(17) || gu["cachedContentTokenCount"] != float64(4) {
		t.Fatalf("gemini usage: %v", gu)
	}
	r, rerr := EncodeResponsesResponse(resp, "p")
	if rerr != nil {
		t.Fatalf("responses: %v", rerr)
	}
	ru := decodeJSON(t, r)["usage"].(map[string]any)
	det := ru["input_tokens_details"].(map[string]any)
	if ru["input_tokens"] != float64(17) || det["cached_tokens"] != float64(4) || det["cache_write_tokens"] != float64(3) {
		t.Fatalf("responses usage: %v", ru)
	}
	c, rerr := EncodeChatResponse(resp, "p")
	if rerr != nil {
		t.Fatalf("chat: %v", rerr)
	}
	cu := decodeJSON(t, c)["usage"].(map[string]any)
	if cu["prompt_tokens"] != float64(17) {
		t.Fatalf("chat usage: %v", cu)
	}
	if det, ok := cu["prompt_tokens_details"]; ok && det.(map[string]any)["cached_tokens"] != float64(4) {
		t.Fatalf("chat cached detail: %v", cu)
	}
}

// --- Usage: Gemini thoughts render, never silently dropped -------------------

func TestEncodeGeminiThoughts(t *testing.T) {
	// Chat scalar reasoning -> Gemini thoughts (c2gemUsage proves it).
	u := mustUsage(t, DialectChat, 5, 8)
	three := 3
	var rerr *RouterError
	u, rerr = u.WithReasoning(&three)
	if rerr != nil {
		t.Fatalf("WithReasoning: %v", rerr)
	}
	resp := mustResponse(t, DialectChat, []ResponseItem{TextItem{ID: "t", Text: "x"}}, u, FinishStop)
	g, rerr := EncodeGeminiResponse(resp, "p")
	if rerr != nil {
		t.Fatalf("gemini: %v", rerr)
	}
	gu := decodeJSON(t, g)["usageMetadata"].(map[string]any)
	if gu["thoughtsTokenCount"] != float64(3) || gu["candidatesTokenCount"] != float64(5) {
		t.Fatalf("reasoning->thoughts: %v", gu)
	}
	// Gemini-native Thoughts bucket also renders (the gate allows it only
	// in Gemini; the encoder must not drop it).
	u2 := mustUsage(t, DialectGemini, 5, 8)
	two := 2
	u2, rerr = u2.WithThoughts(&two)
	if rerr != nil {
		t.Fatalf("WithThoughts: %v", rerr)
	}
	resp2 := mustResponse(t, DialectGemini, []ResponseItem{TextItem{ID: "t", Text: "x"}}, u2, FinishStop)
	g2, rerr := EncodeGeminiResponse(resp2, "p")
	if rerr != nil {
		t.Fatalf("gemini thoughts: %v", rerr)
	}
	gu2 := decodeJSON(t, g2)["usageMetadata"].(map[string]any)
	if gu2["thoughtsTokenCount"] != float64(2) {
		t.Fatalf("thoughts dropped: %v", gu2)
	}
	// Thoughts refuse outside Gemini (closed default, never silent zero).
	for _, dest := range []func(Response, string) ([]byte, *RouterError){EncodeChatResponse, EncodeAnthropicResponse, EncodeResponsesResponse} {
		if _, rerr := dest(resp2, "p"); rerr == nil {
			t.Fatal("thoughts accepted outside gemini")
		}
	}
}

// --- Chat target exact wire ---------------------------------------------------

func TestEncodeChatExactWire(t *testing.T) {
	u := mustUsage(t, DialectAnthropic, 12, 5)
	u, _ = u.WithCache(nil, intPtr(4))
	three := 2
	u, _ = u.WithReasoning(&three)
	items := []ResponseItem{TextItem{ID: "t0", Text: "hi"}, toolItem("i1", "has:colon and space", "f")}
	resp := mustResponse(t, DialectAnthropic, items, u, FinishStop)
	// Bare stop with calls coerces to tool_calls (all three legacy Chat
	// translators prove it).
	c, rerr := EncodeChatResponse(resp, "public")
	if rerr != nil {
		t.Fatalf("chat: %v", rerr)
	}
	out := decodeJSON(t, c)
	if out["model"] != "public" || out["object"] != "chat.completion" {
		t.Fatalf("envelope: %v", out)
	}
	choice := out["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish coercion: %v", choice)
	}
	msg := choice["message"].(map[string]any)
	if msg["content"] != "hi" {
		t.Fatalf("content: %v", msg)
	}
	tc := msg["tool_calls"].([]any)[0].(map[string]any)
	if tc["id"] != "has:colon and space" {
		t.Fatalf("call id not preserved: %v", tc)
	}
	fn := tc["function"].(map[string]any)
	if fn["name"] != "f" || !strings.Contains(fn["arguments"].(string), `"n":1`) {
		t.Fatalf("function wire: %v", fn)
	}
	cu := out["usage"].(map[string]any)
	if cu["prompt_tokens"] != float64(12) || cu["completion_tokens"] != float64(5) || cu["total_tokens"] != float64(17) {
		t.Fatalf("usage totals: %v", cu)
	}
	if cu["prompt_tokens_details"].(map[string]any)["cached_tokens"] != float64(4) {
		t.Fatalf("cached detail: %v", cu)
	}
	if cu["completion_tokens_details"].(map[string]any)["reasoning_tokens"] != float64(2) {
		t.Fatalf("reasoning detail: %v", cu)
	}
	// Refusal + tools has no Chat form (r2c proves it).
	refused := mustResponse(t, DialectResponses,
		[]ResponseItem{RefusalItem{ID: "r0", Text: "no"}, toolItem("i1", "c1", "f")},
		mustUsage(t, DialectResponses, 5, 2), FinishStop)
	if _, rerr := EncodeChatResponse(refused, "p"); rerr == nil {
		t.Fatal("refusal+tools accepted by chat")
	}
	// Refusal finish maps to content_filter (Anthropic/Gemini safety map
	// there; Responses incomplete/content_filter is the same terminal).
	fref := mustResponse(t, DialectAnthropic,
		[]ResponseItem{TextItem{ID: "t", Text: "x"}},
		mustUsage(t, DialectAnthropic, 5, 2), FinishRefusal)
	c2, rerr := EncodeChatResponse(fref, "p")
	if rerr != nil {
		t.Fatalf("refusal finish: %v", rerr)
	}
	if decodeJSON(t, c2)["choices"].([]any)[0].(map[string]any)["finish_reason"] != "content_filter" {
		t.Fatalf("refusal finish mapping: %s", c2)
	}
}
