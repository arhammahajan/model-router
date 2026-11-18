// Responses namespace ToolPlan: first M5 runtime unit tests (ordinary
// functions only). Real wire across all three upstream dialects (Chat,
// Anthropic, Gemini) for JSON and SSE plus the Gemini JSON-array stream:
// namespace definitions, forced choices, history tool results, alias
// collisions, plain ns_-prefix non-ownership, exact bigints, failed names,
// refusals, unknown and double identities, id bounds, same-request
// pin/copy/concurrency, truncation, single errors, write failures and
// immediacy. No custom/freeform/hosted/versioned execution in this unit.
package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const nsAliasLookup = "ns_f1831ae13a0fefbb018daff435dd2acb7fbf5de0f69418141c6cac84"

func nsRequest(toolChoice string) string {
	choice := `"auto"`
	if toolChoice != "" {
		choice = toolChoice
	}
	return `{"model":"m-resp","input":"Look up hello","tools":[
		{"type":"namespace","name":"muse","tools":[
			{"type":"function","name":"lookup","description":"Look up a term","strict":false,
			 "parameters":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"],"additionalProperties":false}}]},
		{"type":"function","name":"ping","description":"Ping","strict":false,
		 "parameters":{"type":"object"}}],
		"tool_choice":` + choice + `}`
}

func nsDecode(t *testing.T, raw string, dest responsesDestination) *Generation {
	t.Helper()
	gen, losses, rerr := decodeResponsesSource([]byte(raw), DefaultLimits(), dest)
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	if len(losses) != 0 {
		t.Fatalf("namespace functions must be exact, got losses %v", losses)
	}
	if gen.ToolPlan == nil || gen.ToolPlan.Empty() {
		t.Fatalf("plan missing for namespaced request")
	}
	return gen
}

func TestToolPlanAliasMatchesPython(t *testing.T) {
	if got := NamespaceAlias("muse", "lookup"); got != nsAliasLookup {
		t.Fatalf("alias mismatch:\n got %s\nwant %s", got, nsAliasLookup)
	}
	if NamespaceAlias("other", "lookup") == nsAliasLookup {
		t.Fatal("dup child across namespaces must mint a distinct alias")
	}
	if NamespaceAlias("muse", "lookup") == NamespaceAlias("muse", "other") {
		t.Fatal("distinct names must not share an alias")
	}
}

func TestToolPlanDecodeFlattensAllDialects(t *testing.T) {
	choice := `{"type":"function","namespace":"muse","name":"lookup"}`
	for _, dest := range []responsesDestination{responsesDestChat, responsesDestAnthropic, responsesDestGemini} {
		gen := nsDecode(t, nsRequest(choice), dest)
		var names []string
		for _, tl := range gen.Tools {
			names = append(names, tl.Name)
		}
		joined := strings.Join(names, ",")
		if !strings.Contains(joined, nsAliasLookup) || !strings.Contains(joined, "ping") {
			t.Fatalf("dest %d tools not flattened: %v", dest, names)
		}
		if gen.Choice == nil || gen.Choice.Mode != "specific" || gen.Choice.Name != nsAliasLookup {
			t.Fatalf("dest %d choice not flattened: %+v", dest, gen.Choice)
		}
		id, ok := gen.ToolPlan.SourceFor(nsAliasLookup)
		if !ok || id.Namespace != "muse" || id.Name != "lookup" {
			t.Fatalf("binding provenance wrong: %+v %v", id, ok)
		}
		if origin, ok := gen.ToolPlan.OriginFor("tools[0]"); !ok || origin != "tools[0].tools[0]" {
			t.Fatalf("origin must retain tools[i].tools[j], got %q %v", origin, ok)
		}
		if origin, ok := gen.ToolPlan.OriginFor("tools[1]"); !ok || origin != "tools[1]" {
			t.Fatalf("plain origin must stay tools[i], got %q %v", origin, ok)
		}
		// Plain ns_-shaped names never claim ownership without a binding.
		if _, ok := gen.ToolPlan.SourceFor("ns_" + strings.Repeat("a", 56)); ok {
			t.Fatal("unbound alias must not resolve")
		}
	}
}

func TestToolPlanHistoryAndResultsFlatten(t *testing.T) {
	raw := `{"model":"m-resp","input":[
		{"type":"function_call","namespace":"muse","name":"lookup","arguments":"{\"q\":\"hello\"}","call_id":"call_1"},
		{"type":"function_call_output","call_id":"call_1","output":"found"}],
		"tools":[{"type":"namespace","name":"muse","tools":[
			{"type":"function","name":"lookup","strict":false,"parameters":{"type":"object"}}]}]}`
	gen, losses, rerr := decodeResponsesSource([]byte(raw), DefaultLimits(), responsesDestChat)
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	if len(losses) != 0 {
		t.Fatalf("losses: %v", losses)
	}
	if len(gen.Messages) != 2 || len(gen.Messages[0].ToolCalls) != 1 {
		t.Fatalf("history order lost: %+v", gen.Messages)
	}
	tc := gen.Messages[0].ToolCalls[0]
	if tc.Name != nsAliasLookup || tc.ID != "call_1" {
		t.Fatalf("history call not flattened/opaque: %+v", tc)
	}
	if string(tc.Arguments) != `{"q":"hello"}` {
		t.Fatalf("history args not exact: %s", tc.Arguments)
	}
	if len(gen.Messages[1].Results) != 1 || gen.Messages[1].Results[0].CallID != "call_1" {
		t.Fatalf("result call id must stay opaque: %+v", gen.Messages)
	}
}

func TestToolPlanUpstreamBodiesCarryAlias(t *testing.T) {
	gen := nsDecode(t, nsRequest(`{"type":"function","namespace":"muse","name":"lookup"}`), responsesDestChat)
	chatBody, rerr := EncodeChatRequest(gen, PlannedEffort{}, "upstream-chat")
	if rerr != nil {
		t.Fatalf("chat encode: %v", rerr)
	}
	if !strings.Contains(string(chatBody), nsAliasLookup) || strings.Contains(string(chatBody), `"namespace"`) {
		t.Fatalf("chat upstream must carry flat alias only: %s", chatBody)
	}
	genA := nsDecode(t, nsRequest(`{"type":"function","namespace":"muse","name":"lookup"}`), responsesDestAnthropic)
	anthBody, rerr := EncodeAnthropicRequest(genA, "upstream-anth")
	if rerr != nil {
		t.Fatalf("anthropic encode: %v", rerr)
	}
	if !strings.Contains(string(anthBody), nsAliasLookup) {
		t.Fatalf("anthropic upstream must carry alias: %s", anthBody)
	}
	genG := nsDecode(t, nsRequest(`{"type":"function","namespace":"muse","name":"lookup"}`), responsesDestGemini)
	gemBody, rerr := EncodeGeminiRequest(genG)
	if rerr != nil {
		t.Fatalf("gemini encode: %v", rerr)
	}
	if !strings.Contains(string(gemBody), nsAliasLookup) {
		t.Fatalf("gemini upstream must carry alias: %s", gemBody)
	}
}

func nsChatCompletion(alias string) string {
	return `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"up",` +
		`"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[` +
		`{"id":"call_7","type":"function","function":{"name":"` + alias + `","arguments":"{\"n\":9007199254740993}"}}]},` +
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`
}

func nsAnthropicMessage(alias string) string {
	return `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x",` +
		`"content":[{"type":"tool_use","id":"call_7","name":"` + alias + `","input":{"n":9007199254740993}}],` +
		`"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":3}}`
}

func nsGeminiResponse(alias string) string {
	return `{"candidates":[{"content":{"role":"model","parts":[` +
		`{"functionCall":{"name":"` + alias + `","args":{"n":9007199254740993},"id":"call_7"}}]},` +
		`"finishReason":"STOP","index":0}],"modelVersion":"gemini-x",` +
		`"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3,"totalTokenCount":8}}`
}

func nsCheckRestored(t *testing.T, out []byte) {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	output := body["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("want one restored call, got %v", output)
	}
	call := output[0].(map[string]any)
	if call["type"] != "function_call" || call["namespace"] != "muse" || call["name"] != "lookup" {
		t.Fatalf("identity not restored: %v", call)
	}
	if call["call_id"] != "call_7" {
		t.Fatalf("call id must stay opaque: %v", call)
	}
	if call["arguments"] != `{"n":9007199254740993}` {
		t.Fatalf("bigint args not exact: %v", call)
	}
}

func TestToolPlanJSONRestoredAllBackends(t *testing.T) {
	gen := nsDecode(t, nsRequest(`"auto"`), responsesDestChat)
	plan := gen.ToolPlan
	out, rerr := TranslateChatJSONToResponsesWithPlan([]byte(nsChatCompletion(nsAliasLookup)), "public", DefaultLimits(), plan)
	if rerr != nil {
		t.Fatalf("chat json: %v", rerr)
	}
	nsCheckRestored(t, out)

	genA := nsDecode(t, nsRequest(`"auto"`), responsesDestAnthropic)
	out, rerr = TranslateAnthropicJSONToResponsesWithPlan([]byte(nsAnthropicMessage(nsAliasLookup)), "public", DefaultLimits(), genA.ToolPlan)
	if rerr != nil {
		t.Fatalf("anthropic json: %v", rerr)
	}
	nsCheckRestored(t, out)

	genG := nsDecode(t, nsRequest(`"auto"`), responsesDestGemini)
	out, rerr = TranslateGeminiJSONToResponsesWithPlan([]byte(nsGeminiResponse(nsAliasLookup)), "public", DefaultLimits(), genG.ToolPlan)
	if rerr != nil {
		t.Fatalf("gemini json: %v", rerr)
	}
	nsCheckRestored(t, out)

	// Nil/empty plan stays raw: alias passes through untouched.
	rawOut, rerr := TranslateChatJSONToResponsesWithPlan([]byte(nsChatCompletion(nsAliasLookup)), "public", DefaultLimits(), nil)
	if rerr != nil {
		t.Fatalf("nil plan: %v", rerr)
	}
	var rawBody map[string]any
	if err := json.Unmarshal(rawOut, &rawBody); err != nil {
		t.Fatal(err)
	}
	call := rawBody["output"].([]any)[0].(map[string]any)
	if call["name"] != nsAliasLookup {
		t.Fatalf("nil plan must stay raw: %v", call)
	}
	if _, hasNS := call["namespace"]; hasNS {
		t.Fatalf("nil plan must not mint namespace: %v", call)
	}
}

func nsChatChunk(delta, finish string) string {
	fr := "null"
	if finish != "" {
		fr = `"` + finish + `"`
	}
	return `{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"u","choices":[{"index":0,"delta":` + delta + `,"finish_reason":` + fr + `}]}`
}

func nsChatUsage() string {
	return `{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"u","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`
}

func nsRunChatStream(t *testing.T, chunks []string, plan *ToolPlan) []map[string]any {
	t.Helper()
	var in bytes.Buffer
	for _, c := range chunks {
		in.WriteString("data: " + c + "\n\n")
	}
	var out bytes.Buffer
	tr := &ChatToResponsesSSETranslator{PublicModel: "public", Limits: DefaultLimits(), Plan: plan}
	if rerr := tr.Translate(&in, &out, nil); rerr != nil {
		t.Fatalf("chat stream: %v", rerr)
	}
	return parseResponsesEvents(t, out.String())
}

func nsStreamChunks(alias string) []string {
	return []string{
		nsChatChunk(`{"tool_calls":[{"index":0,"id":"call_7","type":"function","function":{"name":"`+alias+`","arguments":"{\"n\":"}}]}`, ""),
		nsChatChunk(`{"tool_calls":[{"index":0,"function":{"arguments":"9007199254740993}"}}]}`, ""),
		nsChatChunk(`{}`, "tool_calls"),
		nsChatUsage(),
		"[DONE]",
	}
}

func nsAssertStreamRestored(t *testing.T, events []map[string]any) {
	t.Helper()
	var added, done map[string]any
	var completed map[string]any
	for _, ev := range events {
		switch ev["type"] {
		case "response.output_item.added":
			if item, ok := ev["item"].(map[string]any); ok && item["type"] == "function_call" {
				added = item
			}
		case "response.output_item.done":
			if item, ok := ev["item"].(map[string]any); ok && item["type"] == "function_call" {
				done = item
			}
		case "response.completed":
			completed = ev["response"].(map[string]any)
		}
	}
	if added == nil || added["namespace"] != "muse" || added["name"] != "lookup" || added["call_id"] != "call_7" {
		t.Fatalf("added not restored: %v", added)
	}
	if done == nil || done["namespace"] != "muse" || done["name"] != "lookup" {
		t.Fatalf("done not restored: %v", done)
	}
	if done["arguments"] != `{"n":9007199254740993}` {
		t.Fatalf("stream bigint not exact: %v", done)
	}
	found := false
	for _, item := range completed["output"].([]any) {
		m := item.(map[string]any)
		if m["type"] == "function_call" && m["namespace"] == "muse" && m["name"] == "lookup" {
			found = true
		}
	}
	if !found {
		t.Fatalf("terminal not restored: %v", completed["output"])
	}
}

func TestToolPlanStreamRestoredChat(t *testing.T) {
	gen := nsDecode(t, nsRequest(`"auto"`), responsesDestChat)
	nsAssertStreamRestored(t, nsRunChatStream(t, nsStreamChunks(nsAliasLookup), gen.ToolPlan))
}

func nsAFrame(event, data string) string { return "event: " + event + "\ndata: " + data + "\n\n" }

func nsAPartial(frag string) string {
	q, _ := json.Marshal(frag)
	return string(q)
}

func TestToolPlanStreamRestoredAnthropic(t *testing.T) {
	gen := nsDecode(t, nsRequest(`"auto"`), responsesDestAnthropic)
	upstream :=
		nsAFrame("message_start", `{"type":"message_start","message":{"id":"msg_9","type":"message","role":"assistant","model":"claude-x","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":1}}}`) +
			nsAFrame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_7","name":"`+nsAliasLookup+`","input":{}}}`) +
			nsAFrame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":`+nsAPartial(`{"n":`)+`}}`) +
			nsAFrame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":`+nsAPartial(`9007199254740993}`)+`}}`) +
			nsAFrame("content_block_stop", `{"type":"content_block_stop","index":0}`) +
			nsAFrame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":3}}`) +
			nsAFrame("message_stop", `{"type":"message_stop"}`)
	var out bytes.Buffer
	tr := &AnthropicToResponsesSSETranslator{PublicModel: "public", Limits: DefaultLimits(), Plan: gen.ToolPlan}
	if rerr := tr.Translate(strings.NewReader(upstream), &out, nil); rerr != nil {
		t.Fatalf("anthropic stream: %v", rerr)
	}
	nsAssertStreamRestored(t, parseResponsesEvents(t, out.String()))
}

func TestToolPlanStreamRestoredGeminiSSEAndArray(t *testing.T) {
	gen := nsDecode(t, nsRequest(`"auto"`), responsesDestGemini)
	sseBody := `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"` + nsAliasLookup + `","args":{"n":9007199254740993},"id":"call_7"}}]},"finishReason":"STOP","index":0}],` +
		`"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3,"totalTokenCount":8},"modelVersion":"gemini-x"}`
	var out bytes.Buffer
	tr := &GeminiToResponsesSSETranslator{PublicModel: "public", Limits: DefaultLimits(), Plan: gen.ToolPlan}
	if rerr := tr.Translate(strings.NewReader("data: "+sseBody+"\n\n"), &out, nil); rerr != nil {
		t.Fatalf("gemini sse: %v", rerr)
	}
	nsAssertStreamRestored(t, parseResponsesEvents(t, out.String()))

	var arrOut bytes.Buffer
	arrTr := &GeminiToResponsesJSONArrayTranslator{PublicModel: "public", Limits: DefaultLimits(), Plan: gen.ToolPlan}
	if rerr := arrTr.Translate(strings.NewReader("["+sseBody+"]"), &arrOut, nil); rerr != nil {
		t.Fatalf("gemini array: %v", rerr)
	}
	nsAssertStreamRestored(t, parseResponsesEvents(t, arrOut.String()))
}

func TestToolPlanStreamImmediacyAndTruncation(t *testing.T) {
	gen := nsDecode(t, nsRequest(`"auto"`), responsesDestChat)
	// Immediacy: added event flushes before the terminal is read.
	var in bytes.Buffer
	for _, c := range nsStreamChunks(nsAliasLookup) {
		in.WriteString("data: " + c + "\n\n")
	}
	var flushed []string
	var out bytes.Buffer
	tr := &ChatToResponsesSSETranslator{PublicModel: "public", Limits: DefaultLimits(), Plan: gen.ToolPlan}
	if rerr := tr.Translate(&in, &out, func() { flushed = append(flushed, "flush") }); rerr != nil {
		t.Fatalf("stream: %v", rerr)
	}
	if len(flushed) == 0 {
		t.Fatal("deltas must flush incrementally, never only at the terminal")
	}
	// Truncation: dropped terminal fails closed with no fabricated completion.
	var trunc bytes.Buffer
	trunc.WriteString("data: " + nsChatChunk(`{"tool_calls":[{"index":0,"id":"call_7","type":"function","function":{"name":"`+nsAliasLookup+`","arguments":"{}"}}]}`, "") + "\n\n")
	var truncOut bytes.Buffer
	tr2 := &ChatToResponsesSSETranslator{PublicModel: "public", Limits: DefaultLimits(), Plan: gen.ToolPlan}
	rerr := tr2.Translate(&trunc, &truncOut, nil)
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("truncation must fail 502, got %v", rerr)
	}
	if strings.Contains(truncOut.String(), "response.completed") {
		t.Fatalf("truncated stream must not fabricate completion: %s", truncOut.String())
	}
	// Single-error and write-failure bounds: one terminal error, no success.
	if rerr := tr2.Translate(strings.NewReader("data: [DONE]\n\n"), io.Discard, nil); rerr == nil {
		t.Fatal("empty stream must fail, not succeed")
	}
	failOut := &failAfterWriter{failAfter: 10}
	var failIn bytes.Buffer
	for _, c := range nsStreamChunks(nsAliasLookup) {
		failIn.WriteString("data: " + c + "\n\n")
	}
	tr3 := &ChatToResponsesSSETranslator{PublicModel: "public", Limits: DefaultLimits(), Plan: gen.ToolPlan}
	if rerr := tr3.Translate(&failIn, failOut, nil); rerr == nil {
		t.Fatal("write failure must surface an error")
	}
}

type failAfterWriter struct {
	failAfter int
	written   int
}

func (w *failAfterWriter) Write(p []byte) (int, error) {
	if w.written+len(p) > w.failAfter {
		return 0, errors.New("downstream gone")
	}
	w.written += len(p)
	return len(p), nil
}

func TestToolPlanCollisionAndUnknown(t *testing.T) {
	// Alias colliding with a plain function is a strict 400.
	colliding := `{"model":"m","input":"hi","tools":[
		{"type":"namespace","name":"muse","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]},
		{"type":"function","name":"` + nsAliasLookup + `","parameters":{"type":"object"}}]}`
	if _, _, rerr := decodeResponsesSource([]byte(colliding), DefaultLimits(), responsesDestChat); rerr == nil || rerr.Status != 400 {
		t.Fatalf("alias/plain collision must be 400, got %v", rerr)
	}
	// Duplicate (namespace,name) twice is a conflicting tool definition.
	dup := `{"model":"m","input":"hi","tools":[
		{"type":"namespace","name":"muse","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]},
		{"type":"namespace","name":"muse","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}]}`
	if _, _, rerr := decodeResponsesSource([]byte(dup), DefaultLimits(), responsesDestChat); rerr == nil || rerr.Status != 400 {
		t.Fatalf("duplicate namespaced tool must be 400, got %v", rerr)
	}
	// Same child name in different namespaces is admitted (distinct aliases).
	crossDup := `{"model":"m","input":"hi","tools":[
		{"type":"namespace","name":"muse","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]},
		{"type":"namespace","name":"other","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}]}`
	gen, _, rerr := decodeResponsesSource([]byte(crossDup), DefaultLimits(), responsesDestChat)
	if rerr != nil {
		t.Fatalf("cross-namespace dup children must be admitted: %v", rerr)
	}
	if len(gen.Tools) != 2 || gen.Tools[0].Name == gen.Tools[1].Name {
		t.Fatalf("cross-namespace children need distinct aliases: %+v", gen.Tools)
	}
	// Unknown names, hosted/versioned children, and bad schemas fail strict.
	for name, raw := range map[string]string{
		"invalid namespace history": `{"model":"m","input":[{"type":"function_call","namespace":17,"name":"ghost","arguments":"{}","call_id":"c1"}],"tools":[
			{"type":"namespace","name":"muse","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}]}`,
		"unknown choice": `{"model":"m","input":"hi","tools":[
			{"type":"namespace","name":"muse","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}],
			"tool_choice":{"type":"function","namespace":"nope","name":"ghost"}}`,
		"hosted child": `{"model":"m","input":"hi","tools":[
			{"type":"namespace","name":"muse","tools":[{"type":"web_search"}]}]}`,
		"bad schema": `{"model":"m","input":"hi","tools":[
			{"type":"namespace","name":"muse","tools":[{"type":"function","name":"lookup","parameters":[]}]}]}`,
	} {
		if _, _, rerr := decodeResponsesSource([]byte(raw), DefaultLimits(), responsesDestChat); rerr == nil || rerr.Status != 400 {
			t.Fatalf("%s must be strict 400, got %v", name, rerr)
		}
	}
	// Double call/result identities stay bounded with a plan present.
	doubleID := `{"model":"m","input":[
		{"type":"function_call","namespace":"muse","name":"lookup","arguments":"{}","call_id":"c1"},
		{"type":"function_call","namespace":"muse","name":"lookup","arguments":"{}","call_id":"c1"}],
		"tools":[{"type":"namespace","name":"muse","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}]}`
	if _, _, rerr := decodeResponsesSource([]byte(doubleID), DefaultLimits(), responsesDestChat); rerr == nil || rerr.Status != 400 {
		t.Fatalf("double call ids must be 400, got %v", rerr)
	}
}

func TestToolPlanOtherTargetsRefuseNamespace(t *testing.T) {
	resp, rerr := DecodeChatResponse([]byte(nsChatCompletion(nsAliasLookup)), DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	gen := nsDecode(t, nsRequest(`"auto"`), responsesDestChat)
	restored, rerr := gen.ToolPlan.RestoreResponse(resp, DefaultLimits())
	if rerr != nil {
		t.Fatalf("restore: %v", rerr)
	}
	if rerr := CheckRepresentable(restored, DialectResponses); rerr != nil {
		t.Fatalf("responses must render namespace: %v", rerr)
	}
	for _, dest := range []Dialect{DialectChat, DialectAnthropic, DialectGemini} {
		if rerr := CheckRepresentable(restored, dest); rerr == nil {
			t.Fatalf("%s must refuse namespaced identity, not drop it", dest)
		}
	}
}

func TestToolPlanPinCopyConcurrency(t *testing.T) {
	gen := nsDecode(t, nsRequest(`"auto"`), responsesDestChat)
	plan := gen.ToolPlan
	resp, rerr := DecodeChatResponse([]byte(nsChatCompletion(nsAliasLookup)), DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode: %v", rerr)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			restored, rerr := plan.RestoreResponse(resp, DefaultLimits())
			if rerr != nil {
				t.Errorf("concurrent restore: %v", rerr)
				return
			}
			call, ok := restored.Items[0].(ToolCallItem)
			if !ok || call.Namespace != "muse" || call.Name != "lookup" {
				t.Errorf("concurrent restore wrong: %+v", restored.Items)
			}
			if n := strconv.Itoa(len(plan.Bindings())); n == "" {
				t.Errorf("bindings unreadable")
			}
		}()
	}
	wg.Wait()
	// Same request re-decoded pins the identical alias (deterministic).
	gen2 := nsDecode(t, nsRequest(`"auto"`), responsesDestChat)
	if gen2.Tools[0].Name != nsAliasLookup {
		t.Fatalf("same request must pin the same alias: %+v", gen2.Tools)
	}
}
