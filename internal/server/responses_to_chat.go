package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/arhammahajan/model-router/internal/capability"
	"github.com/arhammahajan/model-router/internal/config"
	"github.com/arhammahajan/model-router/internal/protocol"
	"github.com/arhammahajan/model-router/internal/router"
	"github.com/arhammahajan/model-router/internal/transport"
)

// serveResponsesTranslated bridges POST /v1/responses to an openai-chat
// upstream. The request path is decode -> plan -> enforce policy -> encode
// once: the Responses body is decoded into the typed IR, losses are enforced
// against the provider compatibility only (the client cannot select lossy),
// and the plan is encoded once as a Chat request for the upstream model. No
// upstream I/O happens before the policy decision.
//
// The response path reuses the shared transport with the upstream
// /chat/completions endpoint. JSON responses translate to the Responses
// envelope with the public model name; SSE streams translate incrementally
// with commitment awareness: before the first encoder write no downstream
// headers are committed, so early upstream truncation/invalid/error surfaces
// as an ordinary Responses HTTP 502. After commitment the status is kept, a
// single response.failed event is emitted (the translator emits it on the
// reviewed paths; the boundary emits it once when missing, reusing the
// committed response ID and the next sequence number), and the stream
// terminates without a fabricated response.completed or a duplicate codec
// error.
func (s *Server) serveResponsesTranslated(w http.ResponseWriter, r *http.Request, p *config.Provider, model *config.Model, body []byte, sess router.Session, snap *router.Snapshot) int {
	lim := protocol.DefaultLimits()
	if handled, status := s.maybeServeResponsesSearch(w, r, p, model, body, sess, snap); handled {
		return status
	}
	gen, losses, rerr := protocol.DecodeResponsesRequest(body, lim)
	if rerr != nil {
		s.writeIngressError(w, config.DialectOpenAIResponses, *rerr)
		return rerr.Status
	}
	// Unobserved generated providers retain the pre-I/O hard refusal in
	// both modes: no live probe has established an engine, so supplied
	// effort is never silently treated as ordinary Chat. Membership comes
	// from the same pinned snapshot, never a name heuristic. Known
	// runtimes plan through the shared mapper below.
	if gen.Reasoning.IsPresent() && snap.HasLlamaProvider(p.Name) &&
		snap.LlamaRuntime(p.Name) == capability.RuntimeUnobserved {
		s.writeIngressError(w, config.DialectOpenAIResponses, protocol.RouterError{Status: 400,
			Code: protocol.CodeInvalidGeneration,
			Msg:  "Responses reasoning requires a same-protocol route"})
		return 400
	}
	contract := snap.RequestCapability(model, p)
	caps := capability.ReasoningFor(contract)
	scope := protocol.OrdinaryRoute()
	if snap.HasLlamaProvider(p.Name) {
		scope = protocol.LocalRoute(snap.LlamaRuntime(p.Name))
	}
	planned, planLosses := protocol.PlanEffort(gen.Reasoning, caps, scope)
	losses = append(losses, planLosses...)
	kept, rerr := protocol.EnforcePolicy(losses, p.IsLossy())
	if rerr != nil {
		s.writeIngressError(w, config.DialectOpenAIResponses, *rerr)
		return rerr.Status
	}
	upstreamBody, rerr := protocol.EncodeChatRequest(gen, planned, model.Upstream)
	if rerr != nil {
		s.writeIngressError(w, config.DialectOpenAIResponses, *rerr)
		return rerr.Status
	}
	dest := model.EffectiveDialect(p)
	url, rerr := transport.UpstreamURL(p.BaseURL, dest)
	if rerr != nil {
		s.writeIngressError(w, config.DialectOpenAIResponses, *rerr)
		return rerr.Status
	}
	headers := transport.OutboundHeaders(r.Header, p, config.DialectOpenAIResponses, dest, sess.ID, sess.Has)
	res, rerr := s.up.Do(r.Context(), p, s.cfg.Retry, url, headers, upstreamBody, gen.Stream, s.cfg.Server.MaxBodyBytes)
	if rerr != nil {
		if res != nil && res.Failed {
			forwardRetryHints(w, res.Header)
			rerr = upstreamDetail(res.Status, res.Body)
		}
		s.writeIngressError(w, config.DialectOpenAIResponses, *rerr)
		return rerr.Status
	}
	// Every exit closes the upstream body. For streams ownership moves to
	// the copy loop below, which closes on return; the deferred close is a
	// safety net for the JSON path and pre-commit stream failures.
	defer func() {
		if res.Stream != nil {
			res.Stream.Close()
		}
	}()
	lossHeader := protocol.EncodeLosses(kept)
	if !gen.Stream {
		translated, rerr := protocol.TranslateChatJSONToResponsesWithPlanAndPolicy(res.Body, model.Name, lim, gen.ToolPlan, p.IsLossy())
		if rerr != nil {
			s.writeIngressError(w, config.DialectOpenAIResponses, *rerr)
			return rerr.Status
		}
		toolsRaw, choiceRaw := responsesRequestEcho(gen)
		enriched, rerr := enrichResponsesEnvelope(translated, toolsRaw, choiceRaw, s.cfg.Server.MaxBodyBytes)
		if rerr != nil {
			s.writeIngressError(w, config.DialectOpenAIResponses, *rerr)
			return rerr.Status
		}
		forwardRetryHints(w, res.Header)
		w.Header().Set("Content-Type", "application/json")
		if lossHeader != "" {
			w.Header().Set("X-Model-Router-Loss", lossHeader)
		}
		s.writeJSON(w, 200, enriched)
		return 200
	}
	return s.copyResponsesTranslatedStream(w, r, res, model.Name, lossHeader, gen, p.IsLossy())
}

// responsesStreamCommitter is a minimal explicit lazy writer/commit wrapper
// for the translated Responses SSE path. No headers are committed before the
// first valid encoder write: WriteHeader(200) plus SSE framing, loss header
// and safe retry hints are set on the first Write. Before commitment a
// translator failure maps to an ordinary Responses HTTP 502; after commitment
// the status is kept and the boundary emits a single response.failed event
// when the translator did not already emit one.
//
// The committer also sniffs the committed response.created envelope for the
// upstream response ID and counts forwarded events, so the fallback
// response.failed carries the same response ID and the next unique
// sequence_number. Every translator write is one complete framed event, so
// per-Write scanning is exact; no unbounded retention.
type responsesStreamCommitter struct {
	w           http.ResponseWriter
	rc          *http.ResponseController
	timeout     time.Duration
	lossHeader  string
	retryHeader http.Header
	committed   bool
	failed      bool
	events      int
	failedSent  bool
	respID      string
}

func (c *responsesStreamCommitter) Write(p []byte) (int, error) {
	if c.failed {
		return 0, fmt.Errorf("downstream write failed")
	}
	if !c.committed {
		c.w.Header().Set("Content-Type", "text/event-stream")
		c.w.Header().Set("Cache-Control", "no-cache")
		if c.lossHeader != "" {
			c.w.Header().Set("X-Model-Router-Loss", c.lossHeader)
		}
		forwardRetryHints(c.w, c.retryHeader)
		c.w.WriteHeader(200)
		c.committed = true
	}
	if c.timeout > 0 {
		c.rc.SetWriteDeadline(time.Now().Add(c.timeout))
	}
	n, err := c.w.Write(p)
	if err != nil {
		c.failed = true
		return n, err
	}
	// Only explicit SSE framing counts: a line whose field name is "event"
	// carries the event name. Model text containing "event: error" inside a
	// data JSON payload never marks framing.
	c.events++
	for _, line := range strings.Split(string(p), "\n") {
		line = strings.TrimSuffix(line, "\r")
		name, value, found := strings.Cut(line, ":")
		if !found || strings.TrimSpace(name) != "event" {
			continue
		}
		ev := strings.TrimSpace(value)
		if ev == "response.failed" {
			c.failedSent = true
		}
		if ev == "response.created" && c.respID == "" {
			c.respID = sniffCreatedResponseID(p)
		}
	}
	return n, nil
}

// sniffCreatedResponseID extracts response.id from a committed
// response.created event buffer. It returns "" when the buffer does not
// carry a parseable created envelope; callers fall back to emitting the
// failure without an ID rather than fabricating one.
func sniffCreatedResponseID(p []byte) string {
	for _, line := range strings.Split(string(p), "\n") {
		line = strings.TrimSuffix(line, "\r")
		name, value, found := strings.Cut(line, ":")
		if !found || strings.TrimSpace(name) != "data" {
			continue
		}
		var payload struct {
			Response struct {
				ID string `json:"id"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(value)), &payload); err != nil {
			continue
		}
		if payload.Response.ID != "" {
			return payload.Response.ID
		}
	}
	return ""
}

// responsesRequestEcho builds the validated Responses tools/tool_choice
// echo from the typed plan. The source of truth is the decoded Generation
// (ToolDef with explicit Strict pointer, ToolChoice, canonical schema
// RawMessage) — never the raw request body, so unvalidated members cannot
// leak into the response. Ordinary function tools echo with their effective
// strictness: an explicit true/false is retained exactly; an omitted strict
// (implicit diagnosed loss, only reachable in lossy mode since strict policy
// refuses it) reflects the effective Chat upstream guarantee, which is
// non-strict, so it echoes as false rather than implying Responses strict
// normalization. A dropped lossy tool choice (allowed_tools sets) leaves
// Choice nil and is never echoed as if enforced. With no tools the echo is
// the documented schema default: tools [] with tool_choice "auto".
func responsesRequestEcho(gen *protocol.Generation) (toolsRaw, choiceRaw json.RawMessage) {
	functionEntry := func(t protocol.ToolDef, name string) map[string]any {
		strict := false
		if t.Strict != nil {
			strict = *t.Strict
		}
		entry := map[string]any{"type": "function", "name": name, "strict": strict,
			"parameters": json.RawMessage(append(json.RawMessage(nil), t.Schema...))}
		if t.Description != "" {
			entry["description"] = t.Description
		}
		return entry
	}
	customEntry := func(t protocol.ToolDef, name string) map[string]any {
		entry := map[string]any{"type": "custom", "name": name}
		if t.Description != "" {
			entry["description"] = t.Description
		}
		if t.Format != nil {
			if t.Format.Type == "grammar" {
				entry["format"] = map[string]any{"type": "grammar", "syntax": t.Format.Syntax, "definition": t.Format.Definition}
			} else {
				entry["format"] = map[string]any{"type": "text"}
			}
		}
		return entry
	}
	tools := make([]any, 0, len(gen.Tools))
	if declarations := gen.ToolPlan.Declarations(); len(declarations) > 0 {
		for _, declaration := range declarations {
			if declaration.Namespace == "" {
				t := gen.Tools[declaration.Start]
				if t.IsCustom() {
					tools = append(tools, customEntry(t, t.Name))
				} else {
					tools = append(tools, functionEntry(t, t.Name))
				}
				continue
			}
			children := make([]any, 0, declaration.Count)
			for _, t := range gen.Tools[declaration.Start : declaration.Start+declaration.Count] {
				source, _ := gen.ToolPlan.SourceFor(t.Name)
				if t.IsCustom() {
					children = append(children, customEntry(t, source.Name))
				} else {
					children = append(children, functionEntry(t, source.Name))
				}
			}
			group := map[string]any{"type": "namespace", "name": declaration.Namespace, "tools": children}
			if declaration.HasDescription {
				group["description"] = declaration.Description
			}
			tools = append(tools, group)
		}
	} else {
		for _, t := range gen.Tools {
			if t.IsCustom() {
				tools = append(tools, customEntry(t, t.Name))
			} else {
				tools = append(tools, functionEntry(t, t.Name))
			}
		}
	}
	toolsRaw, _ = json.Marshal(tools)
	var choice any = "auto"
	if gen.Choice != nil {
		switch gen.Choice.Mode {
		case "none":
			choice = "none"
		case "auto":
			choice = "auto"
		case "any":
			choice = "required"
		case "specific":
			kind := "function"
			if gen.Choice.IsCustomChoice() {
				kind = "custom"
			}
			selected := map[string]any{"type": kind, "name": gen.Choice.Name}
			if source, ok := gen.ToolPlan.SourceFor(gen.Choice.Name); ok {
				selected["name"] = source.Name
				selected["namespace"] = source.Namespace
			} else if emu, ok := gen.ToolPlan.EmulatedSource(gen.Choice.Name); ok && emu.Namespace != "" {
				// Plain emu map covers namespaced customs when declarations
				// are absent; SourceFor already covers the declared case.
				selected["name"] = emu.Name
				selected["namespace"] = emu.Namespace
			}
			choice = selected
		}
	}
	choiceRaw, _ = json.Marshal(choice)
	return toolsRaw, choiceRaw
}

// enrichResponsesEnvelope injects the validated tools/tool_choice echo into
// a translated Responses envelope. The codec owns item IDs, sequencing and
// output; only the two echo members are added. The enriched body stays
// within maxBody; exceeding it is a 502, never a truncated envelope.
func enrichResponsesEnvelope(translated []byte, toolsRaw, choiceRaw json.RawMessage, maxBody int64) ([]byte, *protocol.RouterError) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(translated, &top); err != nil || top == nil {
		return nil, &protocol.RouterError{Status: 502, Code: protocol.CodeUpstreamBad,
			Msg: "Upstream returned an invalid response"}
	}
	top["tools"] = toolsRaw
	top["tool_choice"] = choiceRaw
	out, err := json.Marshal(top)
	if err != nil {
		return nil, &protocol.RouterError{Status: 500, Code: "encoding_error",
			Msg: "failed to encode downstream response"}
	}
	if int64(len(out)) > maxBody {
		return nil, &protocol.RouterError{Status: 502, Code: protocol.CodeUpstreamBad,
			Msg: "Upstream response exceeds the size bound"}
	}
	return out, nil
}

// responsesEchoWriter enriches one bounded Responses SSE frame at a time
// with the validated tools/tool_choice echo. It wraps the translator output
// so the codec signature stays unchanged: the translator writes one complete
// framed event per Write, and only that frame is parsed — the generation is
// never buffered. Frames without a "response" object (deltas, item adds)
// pass through untouched. The enriched frame stays within maxFrame;
// exceeding it fails the write so the boundary terminates without a success
// marker instead of emitting a truncated frame.
type responsesEchoWriter struct {
	dst      io.Writer
	tools    json.RawMessage
	choice   json.RawMessage
	maxFrame int
}

func (e *responsesEchoWriter) Write(p []byte) (int, error) {
	enriched, err := enrichResponsesFrame(p, e.tools, e.choice, e.maxFrame)
	if err != nil {
		return 0, err
	}
	if _, werr := e.dst.Write(enriched); werr != nil {
		return 0, werr
	}
	return len(p), nil
}

func enrichResponsesFrame(frame []byte, toolsRaw, choiceRaw json.RawMessage, maxFrame int) ([]byte, error) {
	if len(frame) > maxFrame {
		return nil, fmt.Errorf("Downstream response exceeds the size bound")
	}
	lines := strings.Split(string(frame), "\n")
	changed := false
	for i, ln := range lines {
		rest, ok := strings.CutPrefix(ln, "data:")
		if !ok {
			continue
		}
		rest = strings.TrimPrefix(rest, " ")
		var payload map[string]json.RawMessage
		if err := json.Unmarshal([]byte(rest), &payload); err != nil {
			continue
		}
		rawResp, ok := payload["response"]
		if !ok {
			continue
		}
		var respObj map[string]json.RawMessage
		if err := json.Unmarshal(rawResp, &respObj); err != nil || respObj == nil {
			continue
		}
		respObj["tools"] = toolsRaw
		respObj["tool_choice"] = choiceRaw
		newResp, err := json.Marshal(respObj)
		if err != nil {
			return nil, fmt.Errorf("failed to encode downstream event")
		}
		payload["response"] = json.RawMessage(newResp)
		newPayload, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("failed to encode downstream event")
		}
		lines[i] = "data: " + string(newPayload)
		changed = true
	}
	if !changed {
		return frame, nil
	}
	enriched := []byte(strings.Join(lines, "\n"))
	if len(enriched) > maxFrame {
		return nil, fmt.Errorf("Downstream response exceeds the size bound")
	}
	return enriched, nil
}

// copyResponsesTranslatedStream translates an upstream Chat SSE stream into
// Responses SSE with commitment awareness and bounded downstream writes. It
// never buffers the whole generation and never repairs/retries.
func (s *Server) copyResponsesTranslatedStream(w http.ResponseWriter, r *http.Request, res *transport.Result, publicModel, lossHeader string, gen *protocol.Generation, allowPrivateDrop bool) int {
	tr := &protocol.ChatToResponsesSSETranslator{PublicModel: publicModel, Limits: protocol.DefaultLimits(), Plan: gen.ToolPlan, AllowPrivateReasoningDrop: allowPrivateDrop}
	toolsRaw, choiceRaw := responsesRequestEcho(gen)
	return s.copyResponsesStream(w, r, res, publicModel, lossHeader, toolsRaw, choiceRaw, tr.Translate)
}

func (s *Server) copyResponsesStream(w http.ResponseWriter, r *http.Request, res *transport.Result, publicModel, lossHeader string, toolsRaw, choiceRaw json.RawMessage, translate func(io.Reader, io.Writer, func()) *protocol.RouterError) int {
	timeout := time.Duration(s.cfg.Server.WriteTimeout * float64(time.Second))
	rc := http.NewResponseController(w)
	// The write deadline always resets for keep-alive reuse, including the
	// best-effort post-commit error path below.
	if timeout > 0 {
		defer rc.SetWriteDeadline(time.Time{})
	}
	committer := &responsesStreamCommitter{
		w: w, rc: rc, timeout: timeout,
		lossHeader: lossHeader, retryHeader: res.Header,
	}
	tracker := &anthropicStreamReadTracker{src: res.Stream}
	flushFailed := false
	lim := protocol.DefaultLimits()
	echo := &responsesEchoWriter{dst: committer, tools: toolsRaw, choice: choiceRaw, maxFrame: lim.MaxFrameBytes}
	rerr := translate(tracker, echo, func() {
		if timeout > 0 {
			rc.SetWriteDeadline(time.Now().Add(timeout))
		}
		if ferr := rc.Flush(); ferr != nil {
			// Downstream gone: fail fast, close upstream synchronously so
			// a blocked upstream Read unblocks, and release the slot via
			// the deferred preamble cancel. No goroutine.
			flushFailed = true
			committer.failed = true
			tracker.Close()
		} else if timeout > 0 {
			rc.SetWriteDeadline(time.Now().Add(timeout))
		}
	})
	if rerr != nil || flushFailed || committer.failed {
		if !committer.committed {
			// Pre-commit: ordinary Responses HTTP error, never an empty
			// 200. A stalled body past the absolute or provider deadline
			// is a 504; truncation, invalid frames and client cancellation
			// keep the existing 502 semantics.
			tracker.Close()
			if isPrecommitStreamTimeout(tracker, r.Context()) {
				s.writeIngressError(w, config.DialectOpenAIResponses, protocol.RouterError{Status: 504,
					Code: "request_deadline_exceeded",
					Msg:  "Overall upstream request deadline exceeded; generation may have been accepted and billed."})
				return 504
			}
			s.writeIngressError(w, config.DialectOpenAIResponses, *rerrOrUpstream(rerr))
			if rerr != nil {
				return rerr.Status
			}
			return 502
		}
		// Post-commit: keep 200, emit one response.failed event when the
		// translator did not already emit one, and terminate without a
		// fabricated response.completed or a duplicate codec error. When
		// the downstream already failed, never attempt another raw write
		// that could stall; close upstream promptly instead.
		if committer.failed || flushFailed {
			tracker.Close()
			return 200
		}
		if !committer.failedSent {
			emitResponsesStreamError(committer, rc, timeout, publicModel, toolsRaw, choiceRaw)
		}
		tracker.Close()
		return 200
	}
	return 200
}

// emitResponsesStreamError best-effort writes one response.failed event
// after commitment, reusing the committed response ID when available and the
// next unique sequence_number. The public model identity is preserved from
// the committed generation (the translator emits PublicModel on every event,
// so the fallback must match rather than invent an empty or upstream
// identity), and the validated tools/tool_choice echo is carried so the
// failure frame stays consistent with created/in_progress/completed. A write
// failure here is terminal.
func emitResponsesStreamError(c *responsesStreamCommitter, rc *http.ResponseController, timeout time.Duration, publicModel string, toolsRaw, choiceRaw json.RawMessage) {
	resp := map[string]any{
		"id": c.respID, "object": "response",
		"model": publicModel, "status": "failed",
		"error": map[string]any{
			"code": "server_error", "message": "Upstream stream reported an error",
		},
		"incomplete_details": nil,
		"output":             []any{},
		"usage":              nil,
		"tools":              toolsRaw,
		"tool_choice":        choiceRaw,
	}
	payload := map[string]any{"type": "response.failed", "response": resp,
		"sequence_number": c.events}
	var buf strings.Builder
	buf.WriteString("event: response.failed\ndata: ")
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	buf.Write(data)
	buf.WriteString("\n\n")
	if timeout > 0 {
		rc.SetWriteDeadline(time.Now().Add(timeout))
	}
	_, _ = io.WriteString(c.w, buf.String())
	c.failedSent = true
	_ = rc.Flush()
}
