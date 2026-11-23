// Package router compat implements the opt-in bearer_session_compat
// pre-step for Gemini and Chat generation ingress. It owns compat
// collection and mutation only: strict token grammar, sessionId
// ensure_ascii hashing, Chat marked-token handling and the Gemini effort
// carrier. Protocol owns closed intent constructors and planning;
// transport solely emits; the server seam owns ordering and the pinned
// snapshot. No globals, no session store, no whole-body buffering beyond
// the existing bound.
package router

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/arhammahajan/router/internal/capability"
	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/protocol"
)

// CompatFallback is the final typed Gemini fallback identity. Has=false
// means no fallback token was collected. It travels explicitly from the
// compat pre-step into PrepareSession; there is no ambient store.
type CompatFallback struct {
	ID  string
	Has bool
}

// NoCompatFallback is the explicit empty fallback for non-compat paths.
func NoCompatFallback() CompatFallback { return CompatFallback{} }

var (
	compatToken  = regexp.MustCompile(`^router-session-[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	compatMarked = regexp.MustCompile(`^Bearer (router-session-[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12})$`)
)

const compatEffortHeader = "x-model-router-reasoning-effort"

// nativeCompatHeaders are the native-session names (plus the router-owned
// token) consulted for Chat injection presence. Presence includes empty
// and Connection-nominated values here; affinity later skips or validates
// them. This set never broadens the affinity validator's names.
var nativeCompatHeaders = []string{
	"x-opencode-session",
	"x-claude-code-session-id", "session-id", "session_id", "thread-id",
	"conversation_id", "x-session-id", "x-mavis-session-id",
}

// PrepareGeminiCompat is the Gemini generation POST pre-step, run only when
// the compat flag is enabled on Gemini ingress. It collects valid
// router-session tokens from Authorization (exact case-sensitive "Bearer "
// prefix, no trim) plus x-goog-api-key/x-api-key headers and Gemini ?key=
// values, consumes the sessionId body member (string|null validated, truthy
// wins as agy-acp-sha256 over the canonical ensure_ascii bytes, otherwise
// the launch token), and handles the single effort carrier against the
// published canonical values with native alias hardening. Headers are never
// mutated. Bodies without sessionId stay byte-identical; untouched bodies
// with no sessionId and no native effort mutation return the original
// bytes. Distinct tokens >1 reject, even against a native winner.
func PrepareGeminiCompat(hdr http.Header, queryKeys []string, body []byte,
	published capability.OptionalStringList, target string) (out []byte, fb CompatFallback, effort protocol.ReasoningIntent, rerr *protocol.RouterError) {
	tokens := map[string]bool{}
	for name, values := range hdr {
		if strings.EqualFold(name, "authorization") {
			for _, v := range values {
				if !strings.HasPrefix(v, "Bearer ") {
					continue
				}
				cand := v[len("Bearer "):]
				if compatToken.MatchString(cand) {
					tokens[cand] = true
				}
			}
			continue
		}
		lower := strings.ToLower(name)
		if lower != "x-goog-api-key" && lower != "x-api-key" {
			continue
		}
		for _, v := range values {
			if compatToken.MatchString(v) {
				tokens[v] = true
			}
		}
	}
	for _, k := range queryKeys {
		if compatToken.MatchString(k) {
			tokens[k] = true
		}
	}
	if len(tokens) > 1 {
		return nil, CompatFallback{}, protocol.AbsentReasoning(),
			&protocol.RouterError{Status: 400, Code: "invalid_session_id", Msg: "Conflicting public session tokens"}
	}
	var launch string
	for t := range tokens {
		launch = t
	}
	if !utf8.Valid(body) {
		return nil, CompatFallback{}, protocol.AbsentReasoning(),
			&protocol.RouterError{Status: 400, Code: protocol.CodeInvalidRequest, Msg: "'body' must be a JSON object"}
	}
	var top map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&top); err != nil || top == nil {
		return nil, CompatFallback{}, protocol.AbsentReasoning(),
			&protocol.RouterError{Status: 400, Code: protocol.CodeInvalidRequest, Msg: "'body' must be a JSON object"}
	}
	effBody := body
	if raw, ok := top["sessionId"]; ok {
		trimmed := bytes.TrimSpace(raw)
		if string(trimmed) == "null" {
			delete(top, "sessionId")
			reb, err := json.Marshal(top)
			if err != nil {
				return nil, CompatFallback{}, protocol.AbsentReasoning(),
					&protocol.RouterError{Status: 400, Code: protocol.CodeInvalidRequest, Msg: "'body' must be a JSON object"}
			}
			effBody = reb
		} else if len(trimmed) > 0 && trimmed[0] == '"' {
			id, empty, herr := canonicalSessionHash(trimmed)
			if herr != nil {
				return nil, CompatFallback{}, protocol.AbsentReasoning(), herr
			}
			delete(top, "sessionId")
			reb, err := json.Marshal(top)
			if err != nil {
				return nil, CompatFallback{}, protocol.AbsentReasoning(),
					&protocol.RouterError{Status: 400, Code: protocol.CodeInvalidRequest, Msg: "'body' must be a JSON object"}
			}
			effBody = reb
			if !empty {
				return finishGeminiEffort(hdr, effBody, top, published, target, CompatFallback{ID: id, Has: true})
			}
		} else {
			return nil, CompatFallback{}, protocol.AbsentReasoning(),
				&protocol.RouterError{Status: 400, Code: "invalid_session_id", Msg: "'sessionId' must be a string or null"}
		}
	}
	var fbOut CompatFallback
	if launch != "" {
		if !validSessionValue(launch) {
			return nil, CompatFallback{}, protocol.AbsentReasoning(),
				&protocol.RouterError{Status: 400, Code: "invalid_session_id", Msg: "Conflicting public session tokens"}
		}
		fbOut = CompatFallback{ID: launch, Has: true}
	}
	return finishGeminiEffort(hdr, effBody, top, published, target, fbOut)
}

// finishGeminiEffort validates and applies the single effort carrier on the
// session-stripped envelope. No carrier leaves the body as passed in.
func finishGeminiEffort(hdr http.Header, effBody []byte, top map[string]json.RawMessage,
	published capability.OptionalStringList, target string, fb CompatFallback) ([]byte, CompatFallback, protocol.ReasoningIntent, *protocol.RouterError) {
	vals := headerValues(hdr, compatEffortHeader)
	if len(vals) == 0 {
		return effBody, fb, protocol.AbsentReasoning(), nil
	}
	if len(vals) > 1 {
		return nil, CompatFallback{}, protocol.AbsentReasoning(),
			&protocol.RouterError{Status: 400, Code: protocol.CodeInvalidGeneration, Msg: "Unsupported or invalid reasoning effort for translation"}
	}
	level := vals[0]
	if !published.Set || !containsPublished(published.List, level) {
		return nil, CompatFallback{}, protocol.AbsentReasoning(),
			&protocol.RouterError{Status: 400, Code: protocol.CodeInvalidGeneration, Msg: "Requested effort is not published for this model"}
	}
	genRaw, genKey, genPresent, genCollision := findAlias(top, "generationConfig", "generation_config")
	if genCollision {
		return nil, CompatFallback{}, protocol.AbsentReasoning(),
			&protocol.RouterError{Status: 400, Code: protocol.CodeInvalidRequest, Msg: "'generationConfig' sets generationConfig more than once (camelCase and snake_case)"}
	}
	var genObj map[string]json.RawMessage
	if genPresent {
		if string(bytes.TrimSpace(genRaw)) == "null" {
			genObj = map[string]json.RawMessage{}
		} else {
			if err := json.Unmarshal(genRaw, &genObj); err != nil || genObj == nil {
				return nil, CompatFallback{}, protocol.AbsentReasoning(),
					&protocol.RouterError{Status: 400, Code: protocol.CodeInvalidGeneration, Msg: "'generationConfig' must be an object"}
			}
		}
		thinkRaw, _, thinkPresent, thinkCollision := findAlias(genObj, "thinkingConfig", "thinking_config")
		if thinkCollision {
			return nil, CompatFallback{}, protocol.AbsentReasoning(),
				&protocol.RouterError{Status: 400, Code: protocol.CodeInvalidRequest, Msg: "'thinkingConfig' sets thinkingConfig more than once (camelCase and snake_case)"}
		}
		var thinkObj map[string]json.RawMessage
		if thinkPresent {
			if string(bytes.TrimSpace(thinkRaw)) == "null" {
				thinkObj = map[string]json.RawMessage{}
			} else {
				if err := json.Unmarshal(thinkRaw, &thinkObj); err != nil || thinkObj == nil {
					return nil, CompatFallback{}, protocol.AbsentReasoning(),
						&protocol.RouterError{Status: 400, Code: protocol.CodeInvalidGeneration, Msg: "'thinkingConfig' must be an object"}
				}
			}
		} else {
			thinkObj = map[string]json.RawMessage{}
		}
		if hasThinkingControl(thinkObj) {
			return nil, CompatFallback{}, protocol.AbsentReasoning(),
				&protocol.RouterError{Status: 400, Code: protocol.CodeInvalidGeneration, Msg: "Router effort cannot be combined with Gemini thinking controls"}
		}
	}
	if target == config.DialectGemini {
		mutated, rerr := mutateNativeThinking(top, genKey, genPresent, level)
		if rerr != nil {
			return nil, CompatFallback{}, protocol.AbsentReasoning(), rerr
		}
		return mutated, fb, protocol.AbsentReasoning(), nil
	}
	intent, ok := protocol.NewCompatEffort(level)
	if !ok {
		return nil, CompatFallback{}, protocol.AbsentReasoning(),
			&protocol.RouterError{Status: 400, Code: protocol.CodeInvalidGeneration, Msg: "Requested effort is not published for this model"}
	}
	return effBody, fb, intent, nil
}

// mutateNativeThinking applies the validated carrier directly to the native
// Gemini body, preserving the selected generationConfig container spelling.
// none -> thinkingBudget 0; minimal/low/medium/high -> thinkingLevel UPPER;
// any other published literal -> 400 with no invented budget.
func mutateNativeThinking(top map[string]json.RawMessage, genKey string, genPresent bool, level string) ([]byte, *protocol.RouterError) {
	container := "generationConfig"
	if genPresent {
		container = genKey
	}
	var genObj map[string]json.RawMessage
	if genPresent {
		raw := top[genKey]
		if string(bytes.TrimSpace(raw)) == "null" {
			genObj = map[string]json.RawMessage{}
		} else if err := json.Unmarshal(raw, &genObj); err != nil || genObj == nil {
			return nil, &protocol.RouterError{Status: 400, Code: protocol.CodeInvalidGeneration, Msg: "'generationConfig' must be an object"}
		}
	} else {
		genObj = map[string]json.RawMessage{}
	}
	thinkKey := "thinkingConfig"
	if _, ok := genObj["thinking_config"]; ok {
		if _, dup := genObj["thinkingConfig"]; dup {
			return nil, &protocol.RouterError{Status: 400, Code: protocol.CodeInvalidRequest, Msg: "'thinkingConfig' sets thinkingConfig more than once (camelCase and snake_case)"}
		}
		thinkKey = "thinking_config"
	}
	var thinkObj map[string]json.RawMessage
	if raw, ok := genObj[thinkKey]; ok {
		if string(bytes.TrimSpace(raw)) == "null" {
			thinkObj = map[string]json.RawMessage{}
		} else if err := json.Unmarshal(raw, &thinkObj); err != nil || thinkObj == nil {
			return nil, &protocol.RouterError{Status: 400, Code: protocol.CodeInvalidGeneration, Msg: "'thinkingConfig' must be an object"}
		}
	} else {
		thinkObj = map[string]json.RawMessage{}
	}
	switch level {
	case "none":
		zero, _ := json.Marshal(0)
		thinkObj["thinkingBudget"] = zero
	case "minimal", "low", "medium", "high":
		up, _ := json.Marshal(strings.ToUpper(level))
		thinkObj["thinkingLevel"] = up
	default:
		return nil, &protocol.RouterError{Status: 400, Code: protocol.CodeInvalidGeneration, Msg: "Native Gemini cannot express this effort"}
	}
	tb, err := json.Marshal(thinkObj)
	if err != nil {
		return nil, &protocol.RouterError{Status: 500, Code: "encoding_error", Msg: "failed to encode upstream request"}
	}
	genObj[thinkKey] = tb
	gb, err := json.Marshal(genObj)
	if err != nil {
		return nil, &protocol.RouterError{Status: 500, Code: "encoding_error", Msg: "failed to encode upstream request"}
	}
	top[container] = gb
	out, err := json.Marshal(top)
	if err != nil {
		return nil, &protocol.RouterError{Status: 500, Code: "encoding_error", Msg: "failed to encode upstream request"}
	}
	return out, nil
}

// PrepareChatCompat is the Chat generation POST pre-step, run only when the
// compat flag is enabled on Chat ingress after the existing body/model
// checks. An exact marked Bearer token is stripped (all Authorization
// casings) and re-injected as x-session-id only when no native header name
// is present (presence includes empty and Connection-nominated values).
// Leading plain system messages merge with "\n\n". Unmarked inputs return
// the original body and header untouched. Marked bodies validate minimal
// message object/role/content shapes before merging.
func PrepareChatCompat(hdr http.Header, body []byte) (out []byte, hdrOut http.Header, rerr *protocol.RouterError) {
	matched := map[string]bool{}
	for name, values := range hdr {
		if !strings.EqualFold(name, "authorization") {
			continue
		}
		for _, v := range values {
			if m := compatMarked.FindStringSubmatch(v); m != nil {
				matched[m[1]] = true
			}
		}
	}
	if len(matched) == 0 {
		return body, hdr, nil
	}
	if len(matched) > 1 {
		return nil, nil, &protocol.RouterError{Status: 400, Code: "invalid_session_id", Msg: "Conflicting public session tokens"}
	}
	var token string
	for t := range matched {
		token = t
	}
	cloned := http.Header{}
	for k, vs := range hdr {
		if strings.EqualFold(k, "authorization") {
			continue
		}
		cp := append([]string(nil), vs...)
		cloned[k] = cp
	}
	present := false
	for _, n := range nativeCompatHeaders {
		for k := range cloned {
			if strings.EqualFold(k, n) {
				present = true
				break
			}
		}
		if present {
			break
		}
	}
	if !present {
		cloned.Set("x-session-id", token)
	}
	if !utf8.Valid(body) {
		return nil, nil, &protocol.RouterError{Status: 400, Code: protocol.CodeInvalidRequest, Msg: "'body' must be a JSON object"}
	}
	var top map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&top); err != nil || top == nil {
		return nil, nil, &protocol.RouterError{Status: 400, Code: protocol.CodeInvalidRequest, Msg: "'body' must be a JSON object"}
	}
	rawMsgs, ok := top["messages"]
	if !ok || string(bytes.TrimSpace(rawMsgs)) == "null" {
		return nil, nil, &protocol.RouterError{Status: 400, Code: protocol.CodeInvalidRequest, Msg: "'messages' must be an array of objects"}
	}
	var msgs []map[string]json.RawMessage
	dec2 := json.NewDecoder(bytes.NewReader(rawMsgs))
	dec2.UseNumber()
	if err := dec2.Decode(&msgs); err != nil || msgs == nil {
		return nil, nil, &protocol.RouterError{Status: 400, Code: protocol.CodeInvalidRequest, Msg: "'messages' must be an array of objects"}
	}
	for _, m := range msgs {
		if m == nil {
			return nil, nil, &protocol.RouterError{Status: 400, Code: protocol.CodeInvalidRequest, Msg: "'messages' must be an array of objects"}
		}
		var role string
		rawRole, hasRole := m["role"]
		if !hasRole || string(bytes.TrimSpace(rawRole)) == "null" {
			return nil, nil, &protocol.RouterError{Status: 400, Code: protocol.CodeInvalidRequest, Msg: "'messages' must be an array of objects"}
		}
		if err := json.Unmarshal(rawRole, &role); err != nil {
			return nil, nil, &protocol.RouterError{Status: 400, Code: protocol.CodeInvalidRequest, Msg: "'messages' must be an array of objects"}
		}
		if rawContent, hasContent := m["content"]; hasContent && string(bytes.TrimSpace(rawContent)) != "null" {
			var s string
			if err := json.Unmarshal(rawContent, &s); err != nil {
				// Structured content stops the merge but stays valid.
				var arr []map[string]json.RawMessage
				if json.Unmarshal(rawContent, &arr) != nil {
					return nil, nil, &protocol.RouterError{Status: 400, Code: protocol.CodeInvalidRequest, Msg: "'messages' content blocks must be objects"}
				}
				for _, block := range arr {
					if block == nil {
						return nil, nil, &protocol.RouterError{Status: 400, Code: protocol.CodeInvalidRequest, Msg: "'messages' content blocks must be objects"}
					}
				}
			}
		}
	}
	count := 0
	for _, m := range msgs {
		if len(m) != 2 {
			break
		}
		var role string
		if err := json.Unmarshal(m["role"], &role); err != nil || role != "system" {
			break
		}
		rawContent, ok := m["content"]
		if !ok || string(bytes.TrimSpace(rawContent)) == "null" {
			break
		}
		var s string
		if err := json.Unmarshal(rawContent, &s); err != nil {
			break
		}
		count++
	}
	if count < 2 {
		return body, cloned, nil
	}
	// Join validated JSON string interiors without decoding through Go
	// strings, which would replace lone surrogate code units with U+FFFD.
	mergedContent := json.RawMessage{'"'}
	for i, m := range msgs[:count] {
		if i > 0 {
			mergedContent = append(mergedContent, `\n\n`...)
		}
		raw := bytes.TrimSpace(m["content"])
		mergedContent = append(mergedContent, raw[1:len(raw)-1]...)
	}
	mergedContent = append(mergedContent, '"')
	mergedRole, _ := json.Marshal("system")
	merged := map[string]json.RawMessage{"role": mergedRole, "content": mergedContent}
	rest := make([]map[string]json.RawMessage, 0, len(msgs)-count+1)
	rest = append(rest, merged)
	rest = append(rest, msgs[count:]...)
	mb, err := json.Marshal(rest)
	if err != nil {
		return nil, nil, &protocol.RouterError{Status: 500, Code: "encoding_error", Msg: "failed to encode upstream request"}
	}
	top["messages"] = mb
	outBody, err := json.Marshal(top)
	if err != nil {
		return nil, nil, &protocol.RouterError{Status: 500, Code: "encoding_error", Msg: "failed to encode upstream request"}
	}
	return outBody, cloned, nil
}

func findAlias(obj map[string]json.RawMessage, camel, snake string) (raw json.RawMessage, key string, present bool, collision bool) {
	cRaw, cOk := obj[camel]
	sRaw, sOk := obj[snake]
	if cOk && sOk {
		return nil, "", true, true
	}
	if cOk {
		return cRaw, camel, true, false
	}
	if sOk {
		return sRaw, snake, true, false
	}
	return nil, "", false, false
}

func hasThinkingControl(thinkObj map[string]json.RawMessage) bool {
	for _, k := range []string{"thinkingLevel", "thinking_level", "thinkingBudget", "thinking_budget"} {
		if _, ok := thinkObj[k]; ok {
			return true
		}
	}
	return false
}

func containsPublished(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
