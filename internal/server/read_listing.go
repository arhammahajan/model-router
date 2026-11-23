// Read-path vertical (M4 G1+G2): shared listing renderer for OpenAI,
// Muse and Gemini discovery views.
//
// One pinned Snapshot plus one Clock.Sample per report derives every age
// and staleness; no network, filesystem, refresh or locks run here. Bases
// are CanonicalBase(effectiveDialect) plus trusted overrides plus
// Normalize; raw metadata.router is overwritten, never merged; unrelated
// metadata stays byte-exact (RawMessage, no float round-trip).
package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/arhammahajan/router/internal/capability"
	"github.com/arhammahajan/router/internal/protocol"
)

const goIndexName = "routerd"

// BuildVersion is shared by the endpoint index and CLI. Release builds set
// it once with -ldflags; it is immutable during server operation.
var BuildVersion = "0.1.0"

var goIndexEndpoints = []string{
	"/chat/completions",
	"/messages",
	"/responses",
	"/v1/chat/completions",
	"/v1/messages",
	"/v1/responses",
}

// listingRow is one canonical row with its rendered contract and merged
// metadata. The contract is the typed Render output (limits/reasoning read
// from here, never from serialized metadata/router). Merged metadata is the
// raw map with "router" overwritten by the rendered bytes.
type listingRow struct {
	name         string
	eff          string
	providerName string
	display      string
	contract     capability.Contract
	meta         map[string]json.RawMessage
}

// buildListing pins one snapshot plus one clock sample and renders every
// canonical row. Pure: no network, filesystem, refresh or locks.
func (s *Server) buildListing() ([]listingRow, float64) {
	snap := s.reg.Current()
	sample := s.reg.Clock().Sample()
	wall := float64(sample.Wall)
	catInterval := catalogInterval(s.cfg)
	llamaInterval := llamaInterval(s.cfg)
	listed := snap.ListedModels()
	rows := make([]listingRow, 0, len(listed))
	for _, lm := range listed {
		eff := lm.Model.EffectiveDialect(lm.Provider)
		base := snap.CanonicalBase(lm.Model.Name, eff)
		overrides := snap.CapabilityOverrides(lm.Model.Name)
		var discovery capability.Discovery
		switch lm.Source {
		case "llama":
			discovery = snap.LlamaDiscovery(lm.Provider.Name, llamaInterval, sample.Mono)
		case "catalog":
			discovery = snap.CatalogDiscovery(catInterval, sample.Mono)
		default:
			discovery = capability.StaticDiscovery()
		}
		rendered := capability.Render(base, overrides, discovery, wall)
		renderedBytes, _ := json.Marshal(rendered)
		var meta map[string]json.RawMessage
		if len(lm.Model.Metadata) == 0 {
			meta = map[string]json.RawMessage{}
		} else if err := json.Unmarshal(lm.Model.Metadata, &meta); err != nil {
			meta = map[string]json.RawMessage{}
		}
		if meta == nil {
			meta = map[string]json.RawMessage{}
		}
		meta["router"] = renderedBytes
		rows = append(rows, listingRow{
			name:         lm.Model.Name,
			eff:          eff,
			providerName: lm.Provider.Name,
			display:      listingDisplay(lm.Model.Name, meta),
			contract:     rendered,
			meta:         meta,
		})
	}
	return rows, wall
}

// listingDisplay resolves the human label: raw metadata display_name when a
// non-empty string, else the canonical id. No table, no network.
func listingDisplay(canonical string, meta map[string]json.RawMessage) string {
	if raw, ok := meta["display_name"]; ok {
		var v string
		if err := json.Unmarshal(raw, &v); err == nil && v != "" {
			return v
		}
	}
	return canonical
}

// --- OpenAI view ---

func (s *Server) writeOpenAIModels(w http.ResponseWriter, rows []listingRow) {
	type modelEntry struct {
		ID       string          `json:"id"`
		Object   string          `json:"object"`
		OwnedBy  string          `json:"owned_by"`
		Metadata json.RawMessage `json:"metadata"`
	}
	data := make([]modelEntry, 0, len(rows))
	for _, row := range rows {
		merged, _ := json.Marshal(row.meta)
		data = append(data, modelEntry{ID: row.name, Object: "model", OwnedBy: "router", Metadata: merged})
	}
	if data == nil {
		data = []modelEntry{}
	}
	body, _ := json.Marshal(map[string]any{"object": "list", "data": data})
	s.writeJSON(w, 200, body)
}

// handleModelsAlias serves GET /models (OpenAI shape, headers-only auth).
// Non-GET preserves the previous catch-all: Gemini 404 model_not_found.
func (s *Server) handleModelsAlias(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeGeminiError(w, protocol.RouterError{Status: 404, Code: "model_not_found",
			Msg: "Unknown route or unsupported Gemini method; generateContent and streamGenerateContent are supported"})
		return
	}
	if !s.authed(r) {
		s.writeChatError(w, protocol.RouterError{Status: 401, Code: protocol.CodeUnauthorized, Msg: "Invalid API key"})
		return
	}
	rows, _ := s.buildListing()
	s.writeOpenAIModels(w, rows)
}

// --- Muse view ---

func museCodeFor(row listingRow) map[string]any {
	caps := capability.ReasoningFor(row.contract)
	var ctx int64 = 128000
	if v, ok := row.contract.Get("limits.context_window"); ok {
		if n, ok := v.Int(); ok && n > 0 {
			ctx = n
		}
	}
	var out int64 = 16384
	if v, ok := row.contract.Get("limits.max_output_tokens"); ok {
		if n, ok := v.Int(); ok && n > 0 {
			out = n
		}
	}
	m := map[string]any{
		"name":          row.display,
		"display_label": row.display,
		"is_hidden":     false,
		"limit":         map[string]any{"context": ctx, "output": out},
	}
	m["reasoning"] = caps.Supported != nil && *caps.Supported
	if caps.EffortValues.Set {
		variants := map[string]any{}
		for _, e := range caps.EffortValues.List {
			variants[e] = map[string]any{"reasoningEffort": e, "selector": map[string]any{"description": e}}
		}
		m["variants"] = variants
		if len(caps.EffortValues.List) > 0 {
			def := ""
			if caps.EffortDefault != nil {
				def = *caps.EffortDefault
			}
			found := false
			for _, e := range caps.EffortValues.List {
				if e == def && def != "" {
					found = true
					break
				}
			}
			if !found {
				hasHigh := false
				for _, e := range caps.EffortValues.List {
					if e == "high" {
						hasHigh = true
						break
					}
				}
				if hasHigh {
					def = "high"
				} else {
					def = caps.EffortValues.List[0]
				}
			}
			m["options"] = map[string]any{"reasoningEffort": def}
		}
	}
	return m
}

// handleMuseModels serves GET /muse-code/models and /v1/muse-code/models.
// Headers-only auth; ?key never applies here. Non-GET stays OpenAI 404.
func (s *Server) handleMuseModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeChatError(w, protocol.RouterError{Status: 404, Code: "not_found",
			Msg: "Not implemented in this build (milestone 2: Chat ingress only)"})
		return
	}
	if !s.authed(r) {
		s.writeChatError(w, protocol.RouterError{Status: 401, Code: protocol.CodeUnauthorized, Msg: "Invalid API key"})
		return
	}
	rows, _ := s.buildListing()
	type museEntry struct {
		ID       string          `json:"id"`
		Object   string          `json:"object"`
		OwnedBy  string          `json:"owned_by"`
		Metadata json.RawMessage `json:"metadata"`
	}
	data := make([]museEntry, 0, len(rows))
	for _, row := range rows {
		cp := make(map[string]json.RawMessage, len(row.meta)+1)
		for k, v := range row.meta {
			cp[k] = v
		}
		mc, _ := json.Marshal(museCodeFor(row))
		cp["muse-code"] = mc
		merged, _ := json.Marshal(cp)
		data = append(data, museEntry{ID: row.name, Object: "model", OwnedBy: "router", Metadata: merged})
	}
	if data == nil {
		data = []museEntry{}
	}
	var firstID, lastID any
	if len(rows) > 0 {
		firstID = rows[0].name
		lastID = rows[len(rows)-1].name
	}
	body, _ := json.Marshal(map[string]any{
		"object": "list", "data": data, "first_id": firstID, "last_id": lastID,
	})
	s.writeJSON(w, 200, body)
}

// --- endpoint index ---

func (s *Server) writeIndex(w http.ResponseWriter) {
	body, _ := json.Marshal(map[string]any{
		"name": goIndexName, "version": BuildVersion, "endpoints": goIndexEndpoints,
	})
	s.writeJSON(w, 200, body)
}

// --- Gemini views ---

func geminiResourceFor(row listingRow) map[string]any {
	m := map[string]any{
		"name":                       "models/" + row.name,
		"baseModelId":                row.name,
		"displayName":                row.display,
		"description":                fmt.Sprintf("Routed to provider '%s' (%s) by router.", row.providerName, row.eff),
		"supportedGenerationMethods": []string{"generateContent", "streamGenerateContent"},
	}
	if v, ok := row.contract.Get("limits.max_input_tokens"); ok {
		if n, ok := v.Int(); ok && n != 0 {
			m["inputTokenLimit"] = n
		}
	}
	if _, hasInput := m["inputTokenLimit"]; !hasInput {
		if v, ok := row.contract.Get("limits.context_window"); ok {
			if n, ok := v.Int(); ok {
				m["inputTokenLimit"] = n
			}
		}
	}
	if v, ok := row.contract.Get("limits.max_output_tokens"); ok {
		if n, ok := v.Int(); ok {
			m["outputTokenLimit"] = n
		}
	}
	caps := capability.ReasoningFor(row.contract)
	if caps.Supported != nil {
		m["thinking"] = *caps.Supported
	}
	return m
}

func parseGeminiPageSize(vals []string) (int, *protocol.RouterError) {
	if len(vals) == 0 {
		return 50, nil
	}
	s := vals[len(vals)-1]
	if s == "" {
		return 50, nil
	}
	if len(s) < 1 || len(s) > 6 {
		return 0, &protocol.RouterError{Status: 400, Code: protocol.CodeInvalidRequest,
			Msg: "'pageSize' must be an integer between 0 and 1000"}
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, &protocol.RouterError{Status: 400, Code: protocol.CodeInvalidRequest,
				Msg: "'pageSize' must be an integer between 0 and 1000"}
		}
	}
	n := 0
	for i := 0; i < len(s); i++ {
		n = n*10 + int(s[i]-'0')
	}
	if n < 0 || n > 1000 {
		return 0, &protocol.RouterError{Status: 400, Code: protocol.CodeInvalidRequest,
			Msg: "'pageSize' must be an integer between 0 and 1000"}
	}
	if n == 0 {
		return 50, nil
	}
	return n, nil
}

func parseGeminiPageToken(vals []string) (int, *protocol.RouterError) {
	if len(vals) == 0 {
		return 0, nil
	}
	t := vals[len(vals)-1]
	if t == "" {
		return 0, nil
	}
	for i := 0; i < len(t); i++ {
		if t[i] > 127 {
			return 0, &protocol.RouterError{Status: 400, Code: protocol.CodeInvalidRequest, Msg: "'pageToken' is invalid"}
		}
	}
	padded := t + strings.Repeat("=", (4-len(t)%4)%4)
	raw, err := base64.URLEncoding.DecodeString(padded)
	if err != nil {
		return 0, &protocol.RouterError{Status: 400, Code: protocol.CodeInvalidRequest, Msg: "'pageToken' is invalid"}
	}
	s := string(raw)
	if !strings.HasPrefix(s, "offset:") {
		return 0, &protocol.RouterError{Status: 400, Code: protocol.CodeInvalidRequest, Msg: "'pageToken' is invalid"}
	}
	digits := s[len("offset:"):]
	if len(digits) < 1 || len(digits) > 9 {
		return 0, &protocol.RouterError{Status: 400, Code: protocol.CodeInvalidRequest, Msg: "'pageToken' is invalid"}
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, &protocol.RouterError{Status: 400, Code: protocol.CodeInvalidRequest, Msg: "'pageToken' is invalid"}
		}
	}
	n := 0
	for i := 0; i < len(digits); i++ {
		n = n*10 + int(digits[i]-'0')
	}
	return n, nil
}

func geminiListPayload(rows []listingRow, size, offset int) []byte {
	if offset < 0 {
		offset = 0
	}
	if offset > len(rows) {
		offset = len(rows)
	}
	end := offset + size
	if end > len(rows) {
		end = len(rows)
	}
	models := make([]map[string]any, 0, end-offset)
	for _, row := range rows[offset:end] {
		models = append(models, geminiResourceFor(row))
	}
	if models == nil {
		models = []map[string]any{}
	}
	out := map[string]any{"models": models}
	if end < len(rows) {
		tok := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("offset:%d", end)))
		out["nextPageToken"] = tok
	}
	body, _ := json.Marshal(out)
	return body
}

// serveGeminiList serves the paginated models.list view. Auth first, then
// pagination validation (value-free 400s), then slicing.
func (s *Server) serveGeminiList(w http.ResponseWriter, r *http.Request) int {
	if !s.authedQuery(r, true) {
		s.writeGeminiError(w, protocol.RouterError{Status: 401, Code: protocol.CodeUnauthorized, Msg: "Invalid API key"})
		return 401
	}
	q := r.URL.Query()
	size, rerr := parseGeminiPageSize(q["pageSize"])
	if rerr != nil {
		s.writeGeminiError(w, *rerr)
		return rerr.Status
	}
	offset, rerr := parseGeminiPageToken(q["pageToken"])
	if rerr != nil {
		s.writeGeminiError(w, *rerr)
		return rerr.Status
	}
	rows, _ := s.buildListing()
	s.writeJSON(w, 200, geminiListPayload(rows, size, offset))
	return 200
}

// serveGeminiGet serves models.get for a canonical listed name. Aliases,
// disabled and dynamic names are not listed and return 404. tunedModels
// always 404s.
func (s *Server) serveGeminiGet(w http.ResponseWriter, r *http.Request, kind, name string) int {
	if !s.authedQuery(r, true) {
		s.writeGeminiError(w, protocol.RouterError{Status: 401, Code: protocol.CodeUnauthorized, Msg: "Invalid API key"})
		return 401
	}
	if kind != "models" {
		s.writeGeminiError(w, protocol.RouterError{Status: 404, Code: "model_not_found",
			Msg: fmt.Sprintf("%s/%s is not found for API version v1beta. Call ListModels to see the list of available models.", kind, name)})
		return 404
	}
	rows, _ := s.buildListing()
	for _, row := range rows {
		if row.name == name {
			body, _ := json.Marshal(geminiResourceFor(row))
			s.writeJSON(w, 200, body)
			return 200
		}
	}
	s.writeGeminiError(w, protocol.RouterError{Status: 404, Code: "model_not_found",
		Msg: fmt.Sprintf("models/%s is not found for API version v1beta. Call ListModels to see the list of available models.", name)})
	return 404
}

// handleGeminiModelsList serves exact GET /v1beta/models. Non-GET preserves
// the previous catch-all Gemini 404.
func (s *Server) handleGeminiModelsList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeGeminiError(w, protocol.RouterError{Status: 404, Code: "model_not_found",
			Msg: "Unknown route or unsupported Gemini method; generateContent and streamGenerateContent are supported"})
		return
	}
	s.serveGeminiList(w, r)
}

// parseGeminiGetPath parses an escaped GET resource path after trailing "/"
// normalization. Only /v1beta/models... is a discovery path; other versions
// and prefixes return ok=false. The name is percent-decoded exactly once;
// control characters reject the path.
func parseGeminiGetPath(escaped string) (kind, name string, isList bool, ok bool) {
	trimmed := strings.TrimRight(escaped, "/")
	if trimmed == "" {
		trimmed = "/"
	}
	if trimmed == "/v1beta/models" {
		return "", "", true, true
	}
	const prefix = "/v1beta/"
	if !strings.HasPrefix(trimmed, prefix) {
		return "", "", false, false
	}
	rest := trimmed[len(prefix):]
	var k string
	for _, cand := range []string{"models", "tunedModels"} {
		head := cand + "/"
		if strings.HasPrefix(rest, head) && len(rest) > len(head) {
			k = cand
			rest = rest[len(head):]
			break
		}
	}
	if k == "" {
		return "", "", false, false
	}
	decoded, err := url.PathUnescape(rest)
	if err != nil || decoded == "" {
		return "", "", false, false
	}
	for i := 0; i < len(decoded); i++ {
		if decoded[i] < 0x20 || decoded[i] == 0x7f {
			return "", "", false, false
		}
	}
	return k, decoded, false, true
}

// serveGeminiGetDispatch routes GETs that reached a Gemini prefix handler:
// the v1beta list, a v1beta resource get, or a Gemini 404 otherwise. It
// returns handled and the HTTP status it wrote.
func (s *Server) serveGeminiGetDispatch(w http.ResponseWriter, r *http.Request) (bool, int) {
	if r.Method != http.MethodGet {
		return false, 0
	}
	escaped := r.URL.EscapedPath()
	kind, name, isList, ok := parseGeminiGetPath(escaped)
	if !ok {
		s.writeGeminiError(w, protocol.RouterError{Status: 404, Code: "unknown_route",
			Msg: "Unknown route or unsupported Gemini method; generateContent and streamGenerateContent are supported"})
		return true, 404
	}
	if isList {
		return true, s.serveGeminiList(w, r)
	}
	return true, s.serveGeminiGet(w, r, kind, name)
}
