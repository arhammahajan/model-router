// Opt-in search substitution for Responses -> Chat (complete JSON only).
//
// This vertical never derives a query from the prompt and never searches
// ahead of generation: the model emits the validated synthetic function
// first; the substitution is explicitly approximated and opt-in per
// canonical route with a lossy provider. At most one backend call and one
// continuation run under the same pinned provider/model/session/context/
// slot, with no replay across retry and no stateful/stream/mixed or
// unsupported filters.
package protocol

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"
)

// SearchSyntheticName is the reserved synthetic function identity carried
// on the Chat generation request. The admitted slice carries no other
// tools or history calls, so it cannot collide with a client definition.
const SearchSyntheticName = "router_web_search"

// SearchToolType is the single admitted Responses tool type.
const SearchToolType = "web_search_preview"

// SearchQueryDescription guides the first model round to emit a query.
const SearchQueryDescription = "Search the web for fresh information. Call this function with a single query string."

// SearchCitationDescription guides the continuation round to cite labels.
const SearchCitationDescription = "Web results are provided as tool data. You may cite sources with labels like [S1] in your answer."

// SearchPlan is the semantic plan for one search substitution.
type SearchPlan struct {
	FuncName       string
	IncludeSources bool
}

// SearchSyntheticSchema is the single required query member with no
// additional properties.
func SearchSyntheticSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`)
}

// SearchSyntheticTool builds the first-round synthetic function definition.
func SearchSyntheticTool() ToolDef {
	return ToolDef{
		Kind:        ToolKindFunction,
		Name:        SearchSyntheticName,
		Description: SearchQueryDescription,
		Schema:      SearchSyntheticSchema(),
	}
}

// SearchContinuationTool builds the continuation-round definition carrying
// the trusted citation instruction. Choice is none downstream so no
// further calls can be forced.
func SearchContinuationTool() ToolDef {
	return ToolDef{
		Kind:        ToolKindFunction,
		Name:        SearchSyntheticName,
		Description: SearchCitationDescription,
		Schema:      SearchSyntheticSchema(),
	}
}

// ContainsWebSearchPreview sniffs the request boundary for the single
// admitted search tool type. It performs no validation.
func ContainsWebSearchPreview(raw []byte) bool {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil || top == nil {
		return false
	}
	rawTools, ok := top["tools"]
	if !ok || isNull(rawTools) {
		return false
	}
	var arr []map[string]json.RawMessage
	if err := json.Unmarshal(rawTools, &arr); err != nil {
		return false
	}
	for _, item := range arr {
		if item == nil {
			continue
		}
		var typ string
		_ = json.Unmarshal(item["type"], &typ)
		if typ == SearchToolType {
			return true
		}
	}
	return false
}

// DecodeSearchAdmission validates a bound search request and builds its plan.
// The generation retains the requested downstream framing.
func DecodeSearchAdmission(raw []byte, lim Limits) (*SearchPlan, *Generation, []Loss, *RouterError) {
	bad := func(msg string) (*SearchPlan, *Generation, []Loss, *RouterError) {
		return nil, nil, nil, &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: msg}
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil || top == nil {
		return bad("'body' must be a JSON object")
	}
	var tools []map[string]json.RawMessage
	if json.Unmarshal(top["tools"], &tools) != nil || len(tools) != 1 || len(tools[0]) != 1 {
		return bad("'tools' must contain only one web_search_preview without options")
	}
	kind, _ := decodeUnicodeString(tools[0]["type"])
	if kind != SearchToolType {
		return bad("'tools' must contain only one web_search_preview without options")
	}
	if raw, present := top["tool_choice"]; present && !isNull(raw) {
		if choice, _ := decodeUnicodeString(raw); choice != "auto" {
			return bad("'tool_choice' must be absent or auto for search")
		}
	}

	plan := &SearchPlan{FuncName: SearchSyntheticName}
	if raw, present := top["include"]; present && !isNull(raw) {
		var includes []string
		if json.Unmarshal(raw, &includes) != nil {
			return bad("'include' must be an array of strings")
		}
		for _, include := range includes {
			if include != "web_search_call.action.sources" {
				return bad("'include' supports only search action sources")
			}
			plan.IncludeSources = true
		}
	}
	if raw, present := top["store"]; present && !isNull(raw) {
		var store bool
		if json.Unmarshal(raw, &store) != nil || store {
			return bad("'store' must be false for search")
		}
	}
	if raw, present := top["max_tool_calls"]; present && !isNull(raw) {
		return bad("'max_tool_calls' is unsupported for search replacement")
	}
	// Search is stateless and admits only message history. The shared
	// decoder still owns the detailed validation of messages and controls.
	if raw := top["input"]; len(bytes.TrimSpace(raw)) > 0 && bytes.TrimSpace(raw)[0] == '[' {
		var items []map[string]json.RawMessage
		if json.Unmarshal(raw, &items) != nil {
			return bad("'input' must contain only text messages for search")
		}
		for _, item := range items {
			kind, _ := decodeUnicodeString(item["type"])
			if item == nil || kind != "" && kind != "message" {
				return bad("'input' must contain only text messages for search")
			}
			if content := bytes.TrimSpace(item["content"]); len(content) > 0 && content[0] == '[' {
				var parts []map[string]json.RawMessage
				if json.Unmarshal(content, &parts) != nil {
					return bad("'input' must contain only text messages for search")
				}
				for _, part := range parts {
					kind, _ := decodeUnicodeString(part["type"])
					if kind != "input_text" && kind != "output_text" && kind != "text" {
						return bad("'input' must contain only text messages for search")
					}
				}
			}
		}
	}
	delete(top, "tools")
	delete(top, "tool_choice")
	delete(top, "include")
	gen, losses, rerr := decodeResponsesObject(top, lim, responsesDestChat)
	if rerr != nil {
		return nil, nil, nil, rerr
	}
	gen.Tools = []ToolDef{SearchSyntheticTool()}
	gen.HasTools = true
	gen.Choice = &ToolChoice{Mode: "auto"}
	losses = append(losses, Loss{Field: "tools[0]", Reason: "web search preview is approximated as a synthetic function", Action: "approximated"})
	return plan, gen, boundResponsesLosses(losses), nil
}

// ExtractSearchQuery validates the first Chat round: exactly one function
// call with the planned name, a valid opaque call ID, and a closed JSON
// object with one nonblank UTF-8 query of at most 500 runes. It refuses
// duplicate keys, unexpected calls, malformed wrappers, mixed answer/tool
// output and incompatible finish reasons before search I/O.
func ExtractSearchQuery(resp Response, funcName string, lim Limits) (string, ToolCallItem, *RouterError) {
	bad := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	if resp.MalformedTools > 0 {
		return "", ToolCallItem{}, bad
	}
	if resp.Finish.Kind != FinishToolCalls {
		return "", ToolCallItem{}, bad
	}
	var calls []ToolCallItem
	var texts, refusals int
	for _, it := range resp.Items {
		switch v := it.(type) {
		case TextItem:
			_ = v
			texts++
		case RefusalItem:
			refusals++
		case ToolCallItem:
			calls = append(calls, v)
		case CustomCallItem:
			return "", ToolCallItem{}, bad
		default:
			return "", ToolCallItem{}, bad
		}
	}
	if texts > 0 || refusals > 0 {
		return "", ToolCallItem{}, bad
	}
	if len(calls) != 1 {
		return "", ToolCallItem{}, bad
	}
	call := calls[0]
	if call.Name != funcName {
		return "", ToolCallItem{}, bad
	}
	if err := validateCallID(string(call.CallID), lim); err != nil {
		return "", ToolCallItem{}, bad
	}
	q, rerr := decodeSearchArgs(call.Arguments)
	if rerr != nil {
		return "", ToolCallItem{}, rerr
	}
	return q, call, nil
}

// decodeSearchArgs enforces the closed single-query object: exactly one
// "query" member, no duplicates, no additional properties, nonblank UTF-8
// of at most 500 runes.
func decodeSearchArgs(args json.RawMessage) (string, *RouterError) {
	bad := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	trimmed := bytes.TrimSpace(args)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return "", bad
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	tok, err := dec.Token()
	if err != nil {
		return "", bad
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return "", bad
	}
	seen := map[string]bool{}
	var query *string
	for dec.More() {
		ktok, err := dec.Token()
		if err != nil {
			return "", bad
		}
		ks, ok := ktok.(string)
		if !ok {
			return "", bad
		}
		if seen[ks] {
			return "", bad
		}
		seen[ks] = true
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return "", bad
		}
		if ks != "query" {
			return "", bad
		}
		s, valid := decodeUnicodeString(v)
		if !valid {
			return "", bad
		}
		if strings.TrimSpace(s) == "" {
			return "", bad
		}
		if utf8.RuneCountInString(s) > 500 {
			return "", bad
		}
		c := s
		query = &c
	}
	tok, err = dec.Token()
	if err != nil {
		return "", bad
	}
	if d, ok := tok.(json.Delim); !ok || d != '}' {
		return "", bad
	}
	if _, err := dec.Token(); err != io.EOF {
		return "", bad
	}
	if query == nil || len(seen) != 1 {
		return "", bad
	}
	return *query, nil
}

// SearchResultView is the bounded adapter view transferred to the model.
type SearchResultView struct {
	Label   string
	Title   string
	URL     string
	Snippet string
}

// BuildSearchToolContent serializes accepted results as bounded structured
// tool data with S-labels the model may cite. It never fails open: when
// the payload would exceed the bound, trailing results are dropped until
// it fits; an empty result set is exact.
func BuildSearchToolContent(results []SearchResultView, lim Limits) (string, []SearchResultView) {
	bound := lim.MaxArgsChars
	if bound <= 0 {
		bound = DefaultLimits().MaxArgsChars
	}
	kept := results
	for {
		payload := map[string]any{
			"results": searchToolResults(kept),
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return `{"results":[]}`, nil
		}
		if len(data) <= bound || len(kept) == 0 {
			return string(data), kept
		}
		kept = kept[:len(kept)-1]
	}
}

func searchToolResults(results []SearchResultView) []any {
	out := make([]any, 0, len(results))
	for _, r := range results {
		out = append(out, map[string]any{
			"label": r.Label, "title": r.Title, "url": r.URL, "snippet": r.Snippet,
		})
	}
	return out
}

// AddSearchUsage aggregates authoritative usage from each actual model
// round with checked arithmetic. Search itself contributes no model
// tokens. Overflow or contradictory accounting fails closed.
func AddSearchUsage(a, b ResponseUsage) (ResponseUsage, *RouterError) {
	bad := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned invalid usage accounting"}
	add := func(x, y int) (int, bool) {
		s := x + y
		if s < x || s < y {
			return 0, false
		}
		return s, true
	}
	if a.Prompt == nil || a.Completion == nil || a.Total == nil ||
		b.Prompt == nil || b.Completion == nil || b.Total == nil {
		return ResponseUsage{}, bad
	}
	prompt, ok := add(*a.Prompt, *b.Prompt)
	if !ok {
		return ResponseUsage{}, bad
	}
	completion, ok := add(*a.Completion, *b.Completion)
	if !ok {
		return ResponseUsage{}, bad
	}
	total, ok := add(*a.Total, *b.Total)
	if !ok {
		return ResponseUsage{}, bad
	}
	if prompt+completion != total {
		// Each round proved total == prompt+completion; the sum must
		// preserve it. Overflow-checked addition above guarantees the
		// comparison is exact.
		sum, ok := add(prompt, completion)
		if !ok || sum != total {
			return ResponseUsage{}, bad
		}
	}
	out, rerr := NewTerminalUsage(DialectChat, prompt, completion, total)
	if rerr != nil {
		return ResponseUsage{}, rerr
	}
	// Optional detail counters sum with subset enforcement.
	sumOpt := func(x, y *int) (*int, *RouterError) {
		if x == nil && y == nil {
			return nil, nil
		}
		xv, yv := 0, 0
		if x != nil {
			xv = *x
		}
		if y != nil {
			yv = *y
		}
		s, ok := add(xv, yv)
		if !ok {
			return nil, bad
		}
		return &s, nil
	}
	cached, rerr := sumOpt(a.CacheRead, b.CacheRead)
	if rerr != nil {
		return ResponseUsage{}, rerr
	}
	creation, rerr := sumOpt(a.CacheCreation, b.CacheCreation)
	if rerr != nil {
		return ResponseUsage{}, rerr
	}
	reasoning, rerr := sumOpt(a.Reasoning, b.Reasoning)
	if rerr != nil {
		return ResponseUsage{}, rerr
	}
	text, rerr := sumOpt(a.TextTokens, b.TextTokens)
	if rerr != nil {
		return ResponseUsage{}, rerr
	}
	// Thoughts/tool-use must stay absent: Chat rounds never populate them.
	if a.Thoughts != nil || b.Thoughts != nil || a.ToolUsePrompt != nil || b.ToolUsePrompt != nil {
		return ResponseUsage{}, bad
	}
	out.CacheRead, out.CacheCreation, out.Reasoning, out.TextTokens = cached, creation, reasoning, text
	if rerr := out.ValidateAuthoritative(); rerr != nil {
		return ResponseUsage{}, rerr
	}
	return out, nil
}

// CheckSearchOutputBudget applies the source's output limit to final usage.
func CheckSearchOutputBudget(usage ResponseUsage, budget *int) *RouterError {
	if budget == nil || usage.Completion != nil && *usage.Completion <= *budget {
		return nil
	}
	return &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream exceeded the search generation budget"}
}

// SubtractBudget subtracts authoritative first-round output usage from the
// request output budget. Exhausted or unrepresentable accounting fails.
func SubtractBudget(budget, used int) (int, *RouterError) {
	if budget <= 0 || used < 0 {
		return 0, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned invalid usage accounting"}
	}
	if used >= budget {
		return 0, &RouterError{Status: 400, Code: CodeInvalidRequest, Msg: "'max_output_tokens' budget is exhausted by the first model round"}
	}
	return budget - used, nil
}

var searchCitationMarker = regexp.MustCompile(`\[S(?:[1-9]|10)\]`)

const maxSearchAnnotations = 128

// buildSearchAnnotations uses rune offsets in the exact rendered text.
// Only markers for results transferred to the model acquire provenance.
func buildSearchAnnotations(text string, results []SearchResultView) ([]map[string]any, *RouterError) {
	if len(results) == 0 {
		return nil, nil
	}
	byLabel := make(map[string]SearchResultView, len(results))
	for _, result := range results {
		byLabel[result.Label] = result
	}
	var out []map[string]any
	offset := 0
	for text != "" {
		span := searchCitationMarker.FindStringIndex(text)
		if span == nil {
			break
		}
		offset += utf8.RuneCountInString(text[:span[0]])
		marker := text[span[0]:span[1]]
		end := offset + len(marker) // markers are ASCII
		if result, known := byLabel[marker]; known {
			if len(out) == maxSearchAnnotations {
				return nil, &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream exceeded the search citation bound"}
			}
			out = append(out, map[string]any{
				"type": "url_citation", "start_index": offset, "end_index": end,
				"url": result.URL, "title": result.Title,
			})
		}
		text, offset = text[span[1]:], end
	}
	return out, nil
}

// renderWebSearchCall builds a source-facing item from recorded search results.
func renderWebSearchCall(id, status string, trace ResponsesSearchTrace) map[string]any {
	action := map[string]any{"type": "search", "query": trace.Query}
	if trace.IncludeSources {
		srcs := make([]any, 0, len(trace.Results))
		for _, r := range trace.Results {
			srcs = append(srcs, map[string]any{"type": "url", "url": r.URL})
		}
		action["sources"] = srcs
	}
	return map[string]any{
		"type": "web_search_call", "id": id,
		"status": status, "action": action,
	}
}
