package protocol

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

func mustA2GDecode(t *testing.T, raw string) (*Generation, []Loss) {
	t.Helper()
	g, losses, rerr := DecodeAnthropicForGemini([]byte(raw), DefaultLimits())
	if rerr != nil {
		t.Fatalf("decode failed: %v", rerr)
	}
	return g, losses
}

func a2gExpect400(t *testing.T, raw string) {
	t.Helper()
	if _, _, rerr := DecodeAnthropicForGemini([]byte(raw), DefaultLimits()); rerr == nil || rerr.Status != 400 {
		t.Fatalf("expected 400 for %s, got %v", raw, rerr)
	}
}

func a2gLossFields(losses []Loss) map[string]bool {
	m := map[string]bool{}
	for _, l := range losses {
		m[l.Field] = true
	}
	return m
}

func TestA2GExactMinimal(t *testing.T) {
	// A portable Anthropic request maps exactly: no losses, and the IR
	// renders once via EncodeGeminiRequest with the Gemini wire names.
	g, losses := mustA2GDecode(t, `{"model":"m","max_tokens":16,
		"temperature":0.5,"top_p":0.9,"stop_sequences":["a","b"],
		"system":"sys","messages":[{"role":"user","content":"hi"}]}`)
	if len(losses) != 0 {
		t.Fatalf("expected no losses, got %+v", losses)
	}
	if g.MaxTokens == nil || *g.MaxTokens != 16 {
		t.Fatalf("max tokens lost: %+v", g.MaxTokens)
	}
	out, rerr := EncodeGeminiRequest(g)
	if rerr != nil {
		t.Fatalf("encode failed: %v", rerr)
	}
	var body struct {
		Contents []struct {
			Role  string `json:"role"`
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"contents"`
		SystemInstruction struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"systemInstruction"`
		GenerationConfig map[string]any `json:"generationConfig"`
	}
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	if body.GenerationConfig["temperature"] != 0.5 || body.GenerationConfig["topP"] != 0.9 {
		t.Fatalf("controls not exact: %s", out)
	}
	if body.GenerationConfig["maxOutputTokens"] != float64(16) {
		t.Fatalf("max tokens not exact: %s", out)
	}
	stops, _ := body.GenerationConfig["stopSequences"].([]any)
	if len(stops) != 2 {
		t.Fatalf("stop sequences not exact: %s", out)
	}
}

func TestA2GStopFiveExactSixthDropped(t *testing.T) {
	// Five stop sequences are exact; six diagnose a loss and drop the
	// field entirely, never a silent truncation to five.
	raw5 := `{"model":"m","max_tokens":8,"stop_sequences":["1","2","3","4","5"],
		"messages":[{"role":"user","content":"hi"}]}`
	g, losses := mustA2GDecode(t, raw5)
	if len(losses) != 0 {
		t.Fatalf("five stops must be exact, got %+v", losses)
	}
	if len(g.Stop) != 5 {
		t.Fatalf("five stops lost: %+v", g.Stop)
	}
	raw6 := `{"model":"m","max_tokens":8,"stop_sequences":["1","2","3","4","5","6"],
		"messages":[{"role":"user","content":"hi"}]}`
	g6, losses6, rerr := DecodeAnthropicForGemini([]byte(raw6), DefaultLimits())
	if rerr != nil {
		t.Fatalf("six stops must decode with a loss, got %v", rerr)
	}
	if !a2gLossFields(losses6)["stop_sequences"] {
		t.Fatalf("six stops need a diagnosed loss, got %+v", losses6)
	}
	if len(g6.Stop) != 0 {
		t.Fatalf("six stops must be dropped, never truncated: %+v", g6.Stop)
	}
	if _, rerr := EnforcePolicy(losses6, false); rerr == nil {
		t.Fatal("strict must refuse the stop loss")
	}
}

func TestA2GToolStrictAndParallelPolicy(t *testing.T) {
	tools := `"tools":[{"name":"calc","input_schema":{"type":"object"}}]`
	base := func(extra string) string {
		return `{"model":"m","max_tokens":8,` + tools + `,` + extra + `,
			"messages":[{"role":"user","content":"hi"}]}`
	}
	// Omitted strict is exact and non-strict.
	g, losses := mustA2GDecode(t, base(`"tool_choice":{"type":"auto"}`))
	if len(losses) != 0 {
		t.Fatalf("omitted strict must be exact, got %+v", losses)
	}
	if len(g.Tools) != 1 || g.Tools[0].Name != "calc" {
		t.Fatalf("tool lost: %+v", g.Tools)
	}
	// Explicit false is exact.
	rawFalse := `{"model":"m","max_tokens":8,
		"tools":[{"name":"calc","strict":false,"input_schema":{"type":"object"}}],
		"messages":[{"role":"user","content":"hi"}]}`
	if _, losses := mustA2GDecode(t, rawFalse); len(losses) != 0 {
		t.Fatalf("explicit false strict must be exact, got %+v", losses)
	}
	// Explicit true is a diagnosed loss (never silently weakened).
	rawTrue := `{"model":"m","max_tokens":8,
		"tools":[{"name":"calc","strict":true,"input_schema":{"type":"object"}}],
		"messages":[{"role":"user","content":"hi"}]}`
	_, lossesT, rerr := DecodeAnthropicForGemini([]byte(rawTrue), DefaultLimits())
	if rerr != nil {
		t.Fatalf("explicit true must decode with a loss, got %v", rerr)
	}
	if !a2gLossFields(lossesT)["tools[0].strict"] {
		t.Fatalf("explicit true needs a diagnosed loss, got %+v", lossesT)
	}
	if _, rerr := EnforcePolicy(lossesT, false); rerr == nil {
		t.Fatal("strict must refuse the explicit-true loss")
	}
	// Parallel disable has no Gemini mapping: diagnosed and dropped.
	rawPar := base(`"tool_choice":{"type":"auto","disable_parallel_tool_use":true}`)
	gP, lossesP, rerr := DecodeAnthropicForGemini([]byte(rawPar), DefaultLimits())
	if rerr != nil {
		t.Fatalf("parallel disable must decode with a loss, got %v", rerr)
	}
	if !a2gLossFields(lossesP)["tool_choice.disable_parallel_tool_use"] {
		t.Fatalf("parallel disable needs a diagnosed loss, got %+v", lossesP)
	}
	if gP.ParallelToolCalls != nil {
		t.Fatalf("parallel disable must not reach the encoder: %+v", gP.ParallelToolCalls)
	}
}

func TestA2GTopKThinkingMaxTokens(t *testing.T) {
	// top_k is a diagnosed loss: the IR ExtraTopK channel exists for the
	// Chat bridge, but EncodeGeminiRequest honors no topK, so forwarding
	// it would be silent capability inference.
	raw := `{"model":"m","max_tokens":8,"top_k":5,
		"messages":[{"role":"user","content":"hi"}]}`
	_, losses, rerr := DecodeAnthropicForGemini([]byte(raw), DefaultLimits())
	if rerr != nil {
		t.Fatalf("top_k must decode with a loss, got %v", rerr)
	}
	if !a2gLossFields(losses)["top_k"] {
		t.Fatalf("top_k needs a diagnosed loss, got %+v", losses)
	}
	if _, rerr := EnforcePolicy(losses, false); rerr == nil {
		t.Fatal("strict must refuse the top_k loss")
	}
	// Malformed top_k is a 400 in both modes, before any drop.
	a2gExpect400(t, `{"model":"m","max_tokens":8,"top_k":-1,
		"messages":[{"role":"user","content":"hi"}]}`)
	a2gExpect400(t, `{"model":"m","max_tokens":8,"top_k":"5",
		"messages":[{"role":"user","content":"hi"}]}`)
	// Thinking: explicit disabled is a diagnosed loss; enabled admits the
	// well-formed Budget intent (planner gates the exact wire on the pinned
	// canonical native fact); adaptive requires a same-protocol route.
	rawDis := `{"model":"m","max_tokens":8,"thinking":{"type":"disabled"},
		"messages":[{"role":"user","content":"hi"}]}`
	if _, losses := mustA2GDecode(t, rawDis); !a2gLossFields(losses)["thinking"] {
		t.Fatalf("disabled thinking needs a diagnosed loss, got %+v", losses)
	}
	rawEn := `{"model":"m","max_tokens":8,"thinking":{"type":"enabled","budget_tokens":1024},
		"messages":[{"role":"user","content":"hi"}]}`
	gEn, lossesEn := mustA2GDecode(t, rawEn)
	if !gEn.Reasoning.IsBudget() || gEn.Reasoning.Origin() != "thinking" {
		t.Fatalf("enabled thinking must be Budget intent: %+v", gEn.Reasoning)
	}
	if b, _ := gEn.Reasoning.Budget(); b != 1024 {
		t.Fatalf("enabled budget must be exact: %d", b)
	}
	if len(lossesEn) != 0 {
		t.Fatalf("no-display enabled must be lossless at decode: %v", lossesEn)
	}
	a2gExpect400(t, `{"model":"m","max_tokens":8,"thinking":{"type":"adaptive"},
		"messages":[{"role":"user","content":"hi"}]}`)
	a2gExpect400(t, `{"model":"m","max_tokens":8,"thinking":{"type":"disabled","budget_tokens":1},
		"messages":[{"role":"user","content":"hi"}]}`)
	// max_tokens is required and positive in both modes.
	a2gExpect400(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	a2gExpect400(t, `{"model":"m","max_tokens":0,"messages":[{"role":"user","content":"hi"}]}`)
	a2gExpect400(t, `{"model":"m","max_tokens":"8","messages":[{"role":"user","content":"hi"}]}`)
}

func TestA2GMalformedAndUnknownRejected(t *testing.T) {
	// Malformed known fields are 400, never diagnosed losses.
	a2gExpect400(t, `{"model":"m","max_tokens":8,"temperature":2,
		"messages":[{"role":"user","content":"hi"}]}`)
	a2gExpect400(t, `{"model":"m","max_tokens":8,"stop_sequences":"x",
		"messages":[{"role":"user","content":"hi"}]}`)
	a2gExpect400(t, `{"model":"m","max_tokens":8,"stop_sequences":[1],
		"messages":[{"role":"user","content":"hi"}]}`)
	a2gExpect400(t, `{"model":"m","max_tokens":8,"stream":null,
		"messages":[{"role":"user","content":"hi"}]}`)
	// Unknown top-level and unknown nested members are 400 before drop.
	a2gExpect400(t, `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"nope":1}`)
	a2gExpect400(t, `{"model":"m","max_tokens":8,
		"messages":[{"role":"user","content":[{"type":"text","text":"hi","bogus":1}]}]}`)
	a2gExpect400(t, `{"model":"m","max_tokens":8,
		"tools":[{"name":"calc","input_schema":{"type":"object"},"bogus":1}],
		"messages":[{"role":"user","content":"hi"}]}`)
}

func TestA2GSiblingBehaviorUnchanged(t *testing.T) {
	// Guards the shared branches this bridge extends: Chat still truncates
	// five stops to four with a loss, Chat still diagnoses an explicit
	// strict true, and Responses still maps an omitted strict exactly to
	// an explicit false with no loss.
	raw5 := `{"model":"m","max_tokens":8,"stop_sequences":["1","2","3","4","5"],
		"messages":[{"role":"user","content":"hi"}]}`
	g, losses, rerr := DecodeAnthropicRequest([]byte(raw5), DefaultLimits())
	if rerr != nil {
		t.Fatalf("chat decode failed: %v", rerr)
	}
	if len(g.Stop) != 4 || !a2gLossFields(losses)["stop_sequences"] {
		t.Fatalf("chat five-stop truncation changed: %+v %+v", g.Stop, losses)
	}
	rawStrict := `{"model":"m","max_tokens":8,
		"tools":[{"name":"calc","strict":true,"input_schema":{"type":"object"}}],
		"messages":[{"role":"user","content":"hi"}]}`
	_, lossesC, rerr := DecodeAnthropicRequest([]byte(rawStrict), DefaultLimits())
	if rerr != nil {
		t.Fatalf("chat decode failed: %v", rerr)
	}
	if !a2gLossFields(lossesC)["tools[0].strict"] {
		t.Fatalf("chat explicit-true strict loss changed: %+v", lossesC)
	}
	gr, lossesR, rerr := DecodeAnthropicForResponses([]byte(rawStrict), DefaultLimits())
	if rerr != nil {
		t.Fatalf("responses decode failed: %v", rerr)
	}
	if len(lossesR) != 0 || gr.Tools[0].Strict == nil || *gr.Tools[0].Strict != true {
		t.Fatalf("responses strict plan changed: %+v %+v", gr.Tools[0].Strict, lossesR)
	}
}

func TestA2GToolHistoryRoundTrip(t *testing.T) {
	// Tool definitions, choices, history call/result identities and exact
	// raw schemas/args survive into the Gemini wire request.
	raw := `{"model":"m","max_tokens":32,
		"tools":[{"name":"calc","description":"d",
			"input_schema":{"type":"object","properties":{"n":{"type":"integer"}}}}],
		"tool_choice":{"type":"tool","name":"calc"},
		"messages":[
			{"role":"assistant","content":[
				{"type":"text","text":"looking"},
				{"type":"tool_use","id":"call_1","name":"calc","input":{"n":9007199254740993}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"ok"}]},
			{"role":"user","content":"thanks"}]}`
	g, losses := mustA2GDecode(t, raw)
	if len(losses) != 0 {
		t.Fatalf("history round trip must be exact, got %+v", losses)
	}
	out, rerr := EncodeGeminiRequest(g)
	if rerr != nil {
		t.Fatalf("encode failed: %v", rerr)
	}
	s := string(out)
	for _, want := range []string{`"name":"calc"`, `"call_1"`, "9007199254740993",
		`"allowedFunctionNames":["calc"]`, `"parametersJsonSchema"`, `"output":"ok"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("wire request missing %s: %s", want, s)
		}
	}
	// Conflicting and dangling identities are 400, not guessed.
	a2gExpect400(t, `{"model":"m","max_tokens":8,
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"c","name":"calc","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"nope","content":"x"}]}]}`)
}

func a2gOpaqueHistoryRaw(id string) string {
	return `{"model":"m","max_tokens":16,
		"messages":[
			{"role":"user","content":"hi"},
			{"role":"assistant","content":[{"type":"tool_use","id":` + strconv.Quote(id) + `,"name":"f","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":` + strconv.Quote(id) + `,"content":"done"}]}],
		"tools":[{"name":"f","input_schema":{"type":"object"}}]}`
}

func TestA2GOpaqueHistoryIDsAllDestinations(t *testing.T) {
	// History identities are opaque handles, not tool names: an id like
	// "call:opaque" (colon outside the [A-Za-z0-9_-] name grammar) must
	// decode exactly on every Anthropic-source destination, preserving the
	// raw identity verbatim. Primary contracts: Anthropic Messages
	// tool_use/tool_result ids are strings echoed back by the client
	// (https://docs.anthropic.com/en/api/messages); Gemini FunctionCall
	// id is an optional free string echoed in functionResponse id
	// (https://ai.google.dev/api/generate-content).
	raw := a2gOpaqueHistoryRaw("call:opaque")
	for _, tc := range []struct {
		name string
		dec  func([]byte, Limits) (*Generation, []Loss, *RouterError)
	}{
		{"gemini", DecodeAnthropicForGemini},
		{"chat", DecodeAnthropicRequest},
		{"responses", DecodeAnthropicForResponses},
	} {
		g, losses, rerr := tc.dec([]byte(raw), DefaultLimits())
		if rerr != nil {
			t.Fatalf("%s: opaque history id rejected: %v", tc.name, rerr)
		}
		if len(losses) != 0 {
			t.Fatalf("%s: opaque history must be exact, got %+v", tc.name, losses)
		}
		foundCall, foundResult := false, false
		for _, m := range g.Messages {
			for _, c := range m.ToolCalls {
				if c.ID == "call:opaque" && c.Name == "f" {
					foundCall = true
				}
			}
			for _, r := range m.Results {
				if r.CallID == "call:opaque" && r.Content == "done" {
					foundResult = true
				}
			}
		}
		if !foundCall || !foundResult {
			t.Fatalf("%s: opaque identity not preserved exactly: %+v", tc.name, g.Messages)
		}
	}
}

func TestA2GOpaqueHistoryIDsMalformedAllDestinations(t *testing.T) {
	// Empty and oversize identities stay 400 on every destination; the
	// opener/closer pairing rules are unchanged (dangling still 400).
	decoders := []func([]byte, Limits) (*Generation, []Loss, *RouterError){
		DecodeAnthropicForGemini, DecodeAnthropicRequest, DecodeAnthropicForResponses,
	}
	for _, dec := range decoders {
		for _, raw := range []string{a2gOpaqueHistoryRaw(""), a2gOpaqueHistoryRaw(" ")} {
			if _, _, rerr := dec([]byte(raw), DefaultLimits()); rerr == nil || rerr.Status != 400 {
				t.Fatalf("empty opaque id must be 400, got %v for %q", rerr, raw)
			}
		}
		big := strings.Repeat("x", DefaultLimits().MaxArgsChars+1)
		if _, _, rerr := dec([]byte(a2gOpaqueHistoryRaw(big)), DefaultLimits()); rerr == nil || rerr.Status != 400 {
			t.Fatalf("oversize opaque id must be 400, got %v", rerr)
		}
		dangling := `{"model":"m","max_tokens":8,
			"messages":[
				{"role":"assistant","content":[{"type":"tool_use","id":"call:opaque","name":"f","input":{}}]},
				{"role":"user","content":[{"type":"tool_result","tool_use_id":"call:other","content":"x"}]}]}`
		if _, _, rerr := dec([]byte(dangling), DefaultLimits()); rerr == nil || rerr.Status != 400 {
			t.Fatalf("dangling result identity must be 400, got %v", rerr)
		}
	}
}

func TestA2GToolNameGrammarUnweakened(t *testing.T) {
	// The opaque-handle rule applies to history identities only. Tool
	// definition names, tool_use names and tool_choice names keep the
	// cross-dialect [A-Za-z0-9_-]{1,64} grammar on every destination.
	badTool := `{"model":"m","max_tokens":8,
		"tools":[{"name":"bad name!","input_schema":{"type":"object"}}],
		"messages":[{"role":"user","content":"hi"}]}`
	badUse := `{"model":"m","max_tokens":8,
		"tools":[{"name":"f","input_schema":{"type":"object"}}],
		"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call:ok","name":"bad name!","input":{}}]}]}`
	badChoice := `{"model":"m","max_tokens":8,
		"tools":[{"name":"f","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"tool","name":"bad name!"},
		"messages":[{"role":"user","content":"hi"}]}`
	for _, raw := range []string{badTool, badUse, badChoice} {
		for _, dec := range []func([]byte, Limits) (*Generation, []Loss, *RouterError){
			DecodeAnthropicForGemini, DecodeAnthropicRequest, DecodeAnthropicForResponses,
		} {
			if _, _, rerr := dec([]byte(raw), DefaultLimits()); rerr == nil || rerr.Status != 400 {
				t.Fatalf("tool-name grammar must hold, got %v for %q", rerr, raw)
			}
		}
	}
}

func TestA2GExplicitParallelFalseExact(t *testing.T) {
	// An explicit disable_parallel_tool_use:false is the default and stays
	// exact on the Gemini destination: no diagnosed loss.
	raw := `{"model":"m","max_tokens":16,
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"f","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"auto","disable_parallel_tool_use":false}}`
	_, losses, rerr := DecodeAnthropicForGemini([]byte(raw), DefaultLimits())
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(losses) != 0 {
		t.Fatalf("explicit parallel allowance is exact, got losses: %v", losses)
	}
}
