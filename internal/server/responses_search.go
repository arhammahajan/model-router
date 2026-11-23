package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/arhammahajan/router/internal/capability"
	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/protocol"
	"github.com/arhammahajan/router/internal/router"
	"github.com/arhammahajan/router/internal/searxng"
	"github.com/arhammahajan/router/internal/transport"
)

// maybeServeResponsesSearch runs the opt-in search substitution vertical when
// the request carries the exact web_search_preview tool on a bound lossy
// Chat route. It returns handled=false when the request is not a search
// request, letting the caller fall through to the ordinary bridge.
func (s *Server) maybeServeResponsesSearch(w http.ResponseWriter, r *http.Request, p *config.Provider, model *config.Model, body []byte, sess router.Session, snap *router.Snapshot) (bool, int) {
	if !protocol.ContainsWebSearchPreview(body) {
		return false, 0
	}
	lim := protocol.DefaultLimits()
	// Wrong-route and unbound routes refuse before I/O with no backend or
	// upstream calls. Bindings name only enabled canonical lossy Chat
	// routes; aliases resolve through the canonical model.
	binding, backend, ok := s.cfg.SearchBindingFor(model.Name)
	if s.cfg.Search == nil || !ok || binding == nil || backend == nil {
		s.writeIngressError(w, config.DialectOpenAIResponses, protocol.RouterError{Status: 400,
			Code: protocol.CodeInvalidGeneration,
			Msg:  "web_search_preview requires an opt-in binding for this route"})
		return true, 400
	}
	plan, gen, losses, rerr := protocol.DecodeSearchAdmission(body, lim)
	if rerr != nil {
		s.writeIngressError(w, config.DialectOpenAIResponses, *rerr)
		return true, rerr.Status
	}
	if gen.Reasoning.IsPresent() && snap.HasLlamaProvider(p.Name) &&
		snap.LlamaRuntime(p.Name) == capability.RuntimeUnobserved {
		s.writeIngressError(w, config.DialectOpenAIResponses, protocol.RouterError{Status: 400,
			Code: protocol.CodeInvalidGeneration,
			Msg:  "Responses reasoning requires a same-protocol route"})
		return true, 400
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
		return true, rerr.Status
	}
	lossHeader := protocol.EncodeLosses(kept)
	dest := model.EffectiveDialect(p)
	url, rerr := transport.UpstreamURL(p.BaseURL, dest)
	if rerr != nil {
		s.writeIngressError(w, config.DialectOpenAIResponses, *rerr)
		return true, rerr.Status
	}
	headers := transport.OutboundHeaders(r.Header, p, config.DialectOpenAIResponses, dest, sess.ID, sess.Has)

	first := *gen
	first.Stream = false
	upstreamBody1, rerr := protocol.EncodeChatRequest(&first, planned, model.Upstream)
	if rerr != nil {
		s.writeIngressError(w, config.DialectOpenAIResponses, *rerr)
		return true, rerr.Status
	}
	res1, rerr := s.up.Do(r.Context(), p, s.cfg.Retry, url, headers, upstreamBody1, false, s.cfg.Server.MaxBodyBytes)
	if rerr != nil {
		if res1 != nil && res1.Failed {
			forwardRetryHints(w, res1.Header)
			rerr = upstreamDetail(res1.Status, res1.Body)
		}
		s.writeIngressError(w, config.DialectOpenAIResponses, *rerr)
		return true, rerr.Status
	}
	if res1.Stream != nil {
		res1.Stream.Close()
	}
	resp1, rerr := protocol.DecodeChatResponse(res1.Body, lim)
	if rerr != nil {
		s.writeIngressError(w, config.DialectOpenAIResponses, *rerr)
		return true, rerr.Status
	}
	query, call, qerr := protocol.ExtractSearchQuery(resp1, plan.FuncName, lim)
	if qerr != nil {
		// Ordinary model text returns without a search: one model round,
		// source echo, public model, loss header, no web_search_call.
		if isSearchNoCall(resp1) {
			if rerr := protocol.CheckSearchOutputBudget(resp1.Usage, gen.MaxTokens); rerr != nil {
				s.writeIngressError(w, config.DialectOpenAIResponses, *rerr)
				return true, rerr.Status
			}
			if gen.Stream {
				// Streaming no-call renders the already-decoded complete
				// typed first answer via typed events through the
				// existing lifecycle/Responses encoder. No fabricated
				// upstream SSE and no second model call.
				return true, s.streamSearchNoCall(w, r, resp1, model.Name, lossHeader, res1.Header)
			}
			translated, rerr := protocol.EncodeResponsesResponse(resp1, model.Name)
			if rerr != nil {
				s.writeIngressError(w, config.DialectOpenAIResponses, *rerr)
				return true, rerr.Status
			}
			enriched, rerr := enrichResponsesEnvelope(translated, searchSourceToolsRaw(), searchSourceChoiceRaw(), s.cfg.Server.MaxBodyBytes)
			if rerr != nil {
				s.writeIngressError(w, config.DialectOpenAIResponses, *rerr)
				return true, rerr.Status
			}
			forwardRetryHints(w, res1.Header)
			w.Header().Set("Content-Type", "application/json")
			if lossHeader != "" {
				w.Header().Set("X-Model-Router-Loss", lossHeader)
			}
			s.writeJSON(w, 200, enriched)
			return true, 200
		}
		s.writeIngressError(w, config.DialectOpenAIResponses, *qerr)
		return true, qerr.Status
	}

	// Budget: subtract authoritative first-round output usage.
	var contMax *int
	if gen.MaxTokens != nil {
		if resp1.Usage.Completion == nil {
			s.writeIngressError(w, config.DialectOpenAIResponses, protocol.RouterError{Status: 502, Code: protocol.CodeUpstreamBad, Msg: "Upstream returned invalid usage accounting"})
			return true, 502
		}
		remaining, rerr := protocol.SubtractBudget(*gen.MaxTokens, *resp1.Usage.Completion)
		if rerr != nil {
			s.writeIngressError(w, config.DialectOpenAIResponses, *rerr)
			return true, rerr.Status
		}
		contMax = &remaining
	}

	// One bound backend call under the same request context. The original
	// call ID is preserved. No query/snippets travel to logs.
	sc, err := searxng.NewClient(backend.Endpoint, backend.Headers)
	if err != nil {
		s.writeIngressError(w, config.DialectOpenAIResponses, protocol.RouterError{Status: 500, Code: "encoding_error", Msg: "failed to encode downstream request"})
		return true, 500
	}
	defer sc.CloseIdleConnections()
	sraw, serr := sc.Search(r.Context(), searxng.Request{Query: query, Count: backend.Count})
	if serr != nil {
		if ctxErr := r.Context().Err(); ctxErr == context.DeadlineExceeded {
			s.writeIngressError(w, config.DialectOpenAIResponses, protocol.RouterError{Status: 504,
				Code: "request_deadline_exceeded",
				Msg:  "Overall upstream request deadline exceeded; generation may have been accepted and billed."})
			return true, 504
		}
		s.writeIngressError(w, config.DialectOpenAIResponses, protocol.RouterError{Status: 502,
			Code: protocol.CodeUpstreamBad, Msg: "Search backend failed"})
		return true, 502
	}
	views := make([]protocol.SearchResultView, 0, len(sraw))
	for i, sr := range sraw {
		views = append(views, protocol.SearchResultView{
			Label:   searchLabel(i),
			Title:   sr.Title,
			URL:     sr.URL,
			Snippet: sr.Snippet,
		})
	}
	toolContent, views := protocol.BuildSearchToolContent(views, lim)

	trace := protocol.ResponsesSearchTrace{Query: query, Results: views, IncludeSources: plan.IncludeSources}
	// Continuation: original assistant call plus its bounded tool result.
	// Same provider credential/session/endpoint; further calls disabled.
	gen2 := &protocol.Generation{
		Model:             gen.Model,
		Stream:            gen.Stream,
		Messages:          appendSearchContinuationMessages(gen.Messages, call, toolContent),
		Tools:             []protocol.ToolDef{protocol.SearchContinuationTool()},
		HasTools:          true,
		Choice:            &protocol.ToolChoice{Mode: "none"},
		MaxTokens:         contMax,
		Temperature:       gen.Temperature,
		TopP:              gen.TopP,
		ParallelToolCalls: gen.ParallelToolCalls,
		Reasoning:         gen.Reasoning,
	}
	upstreamBody2, rerr := protocol.EncodeChatRequest(gen2, planned, model.Upstream)
	if rerr != nil {
		s.writeIngressError(w, config.DialectOpenAIResponses, *rerr)
		return true, rerr.Status
	}
	// The result bytes are retained in upstreamBody2 for transport retries,
	// which cannot rerun the search.
	if gen.Stream {
		// Streaming continuation: one generation slot and the same request
		// context cover search and both model rounds; same credential,
		// session, endpoint and upstream model. Retries resend retained
		// continuation bytes only.
		res2, rerr := s.up.Do(r.Context(), p, s.cfg.Retry, url, headers, upstreamBody2, true, s.cfg.Server.MaxBodyBytes)
		if rerr != nil {
			if res2 != nil && res2.Failed {
				forwardRetryHints(w, res2.Header)
				rerr = upstreamDetail(res2.Status, res2.Body)
			}
			s.writeIngressError(w, config.DialectOpenAIResponses, *rerr)
			return true, rerr.Status
		}
		defer res2.Stream.Close()
		enc := protocol.NewResponsesEventEncoder(model.Name, lim)
		enc.SetSearchPrelude(trace, resp1.Usage, gen.MaxTokens)
		return true, s.copyResponsesStream(w, r, res2, model.Name, lossHeader, searchSourceToolsRaw(), searchSourceChoiceRaw(),
			func(src io.Reader, dst io.Writer, flush func()) *protocol.RouterError {
				frames := protocol.NewSSEFrameReader(src, lim)
				dec := protocol.NewChatStreamDecoder(lim)
				return protocol.TranslateStream(frames, dec, enc, dst, flush, lim, protocol.DialectChat)
			})
	}
	res2, rerr := s.up.Do(r.Context(), p, s.cfg.Retry, url, headers, upstreamBody2, false, s.cfg.Server.MaxBodyBytes)
	if rerr != nil {
		if res2 != nil && res2.Failed {
			forwardRetryHints(w, res2.Header)
			rerr = upstreamDetail(res2.Status, res2.Body)
		}
		s.writeIngressError(w, config.DialectOpenAIResponses, *rerr)
		return true, rerr.Status
	}
	if res2.Stream != nil {
		res2.Stream.Close()
	}
	resp2, rerr := protocol.DecodeChatResponse(res2.Body, lim)
	if rerr != nil {
		s.writeIngressError(w, config.DialectOpenAIResponses, *rerr)
		return true, rerr.Status
	}
	if resp2.MalformedTools > 0 || hasSearchFinalCalls(resp2) || resp2.Finish.Kind == protocol.FinishToolCalls {
		s.writeIngressError(w, config.DialectOpenAIResponses, protocol.RouterError{Status: 502, Code: protocol.CodeUpstreamBad, Msg: "Upstream returned an invalid response"})
		return true, 502
	}
	agg, rerr := protocol.AddSearchUsage(resp1.Usage, resp2.Usage)
	if rerr != nil {
		s.writeIngressError(w, config.DialectOpenAIResponses, *rerr)
		return true, rerr.Status
	}
	if rerr := protocol.CheckSearchOutputBudget(agg, gen.MaxTokens); rerr != nil {
		s.writeIngressError(w, config.DialectOpenAIResponses, *rerr)
		return true, rerr.Status
	}
	respFinal := resp2
	respFinal.Usage = agg
	translated, rerr := protocol.EncodeResponsesSearchResponse(respFinal, model.Name, trace)
	if rerr != nil {
		s.writeIngressError(w, config.DialectOpenAIResponses, *rerr)
		return true, rerr.Status
	}
	enriched, rerr := enrichResponsesEnvelope(translated, searchSourceToolsRaw(), searchSourceChoiceRaw(), s.cfg.Server.MaxBodyBytes)
	if rerr != nil {
		s.writeIngressError(w, config.DialectOpenAIResponses, *rerr)
		return true, rerr.Status
	}
	forwardRetryHints(w, res2.Header)
	w.Header().Set("Content-Type", "application/json")
	if lossHeader != "" {
		w.Header().Set("X-Model-Router-Loss", lossHeader)
	}
	s.writeJSON(w, 200, enriched)
	return true, 200
}

// streamSearchNoCall renders the already-decoded complete typed first
// answer via typed lifecycle/encoder events. No upstream SSE is
// fabricated and no second model call runs. Commitment mirrors the
// ordinary stream: pre-commit failures are HTTP errors, post-commit
// failures emit one response.failed with no success.
func (s *Server) streamSearchNoCall(w http.ResponseWriter, r *http.Request, resp protocol.Response, publicModel, lossHeader string, retryHeader http.Header) int {
	timeout := time.Duration(s.cfg.Server.WriteTimeout * float64(time.Second))
	rc := http.NewResponseController(w)
	if timeout > 0 {
		defer rc.SetWriteDeadline(time.Time{})
	}
	committer := &responsesStreamCommitter{
		w: w, rc: rc, timeout: timeout,
		lossHeader: lossHeader, retryHeader: retryHeader,
	}
	lim := protocol.DefaultLimits()
	toolsRaw, choiceRaw := searchSourceToolsRaw(), searchSourceChoiceRaw()
	echo := &responsesEchoWriter{dst: committer, tools: toolsRaw, choice: choiceRaw, maxFrame: lim.MaxFrameBytes}
	flushFailed := false
	flush := func() {
		if timeout > 0 {
			rc.SetWriteDeadline(time.Now().Add(timeout))
		}
		if ferr := rc.Flush(); ferr != nil {
			flushFailed = true
			committer.failed = true
		} else if timeout > 0 {
			rc.SetWriteDeadline(time.Now().Add(timeout))
		}
	}
	enc := protocol.NewResponsesEventEncoder(publicModel, lim)
	lifecycle := protocol.NewResponseLifecycle(lim, protocol.DialectChat)
	aborted := false
	fail := func(rerr *protocol.RouterError) *protocol.RouterError {
		if rerr != nil && enc.Committed() && !aborted {
			aborted = true
			enc.Abort(echo, flush)
		}
		return rerr
	}
	feed := func(ev protocol.ResponseEvent) *protocol.RouterError {
		if rerr := lifecycle.Observe(ev); rerr != nil {
			return fail(rerr)
		}
		if lifecycle.Failed() {
			return fail(lifecycle.Failure())
		}
		if eerr := enc.Encode(echo, flush, ev); eerr != nil {
			if eerr.WriteFailed {
				return eerr.Err
			}
			return fail(eerr.Err)
		}
		return nil
	}
	events := make([]protocol.ResponseEvent, 0, 2*len(resp.Items)+3)
	events = append(events, protocol.ResponseStart{
		ID: resp.ID, Model: resp.Model, Source: protocol.DialectChat,
		IDOrigin: resp.IDOrigin, ModelOrigin: resp.ModelOrigin,
	})
	for _, it := range resp.Items {
		switch v := it.(type) {
		case protocol.TextItem:
			events = append(events, protocol.BlockOpen{Item: protocol.ItemID(v.ID), Kind: protocol.BlockText})
			events = append(events, protocol.TextFragment{Item: protocol.ItemID(v.ID), Text: v.Text})
			events = append(events, protocol.BlockClose{Item: protocol.ItemID(v.ID)})
		case protocol.RefusalItem:
			events = append(events, protocol.BlockOpen{Item: protocol.ItemID(v.ID), Kind: protocol.BlockRefusal})
			events = append(events, protocol.TextFragment{Item: protocol.ItemID(v.ID), Text: v.Text})
			events = append(events, protocol.BlockClose{Item: protocol.ItemID(v.ID)})
		default:
			rerr := &protocol.RouterError{Status: 502, Code: protocol.CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
			if !committer.committed {
				s.writeIngressError(w, config.DialectOpenAIResponses, *rerr)
				return rerr.Status
			}
			if !committer.failedSent {
				emitResponsesStreamError(committer, rc, timeout, publicModel, toolsRaw, choiceRaw)
			}
			return 200
		}
	}
	events = append(events, protocol.UsageEvent{Usage: resp.Usage})
	events = append(events, protocol.CompletionEvent{Finish: resp.Finish})
	var feedErr *protocol.RouterError
	for _, ev := range events {
		if rerr := feed(ev); rerr != nil {
			feedErr = rerr
			break
		}
		if flushFailed || committer.failed {
			break
		}
	}
	if feedErr != nil || flushFailed || committer.failed {
		if !committer.committed {
			s.writeIngressError(w, config.DialectOpenAIResponses, *rerrOrUpstream(feedErr))
			if feedErr != nil {
				return feedErr.Status
			}
			return 502
		}
		if committer.failed || flushFailed {
			return 200
		}
		if !committer.failedSent {
			emitResponsesStreamError(committer, rc, timeout, publicModel, toolsRaw, choiceRaw)
		}
		return 200
	}
	return 200
}

func isSearchNoCall(resp protocol.Response) bool {
	if resp.MalformedTools > 0 {
		return false
	}
	for _, it := range resp.Items {
		switch it.(type) {
		case protocol.ToolCallItem, protocol.CustomCallItem:
			return false
		}
	}
	switch resp.Finish.Kind {
	case protocol.FinishStop, protocol.FinishLength, protocol.FinishContentFilter:
		return true
	default:
		return false
	}
}

func hasSearchFinalCalls(resp protocol.Response) bool {
	for _, it := range resp.Items {
		switch it.(type) {
		case protocol.ToolCallItem, protocol.CustomCallItem:
			return true
		}
	}
	return false
}

func searchLabel(i int) string {
	return "[S" + strconv.Itoa(i+1) + "]"
}

func searchSourceToolsRaw() json.RawMessage {
	return json.RawMessage(`[{"type":"web_search_preview"}]`)
}

func searchSourceChoiceRaw() json.RawMessage {
	return json.RawMessage(`"auto"`)
}

func appendSearchContinuationMessages(base []protocol.Message, call protocol.ToolCallItem, toolContent string) []protocol.Message {
	out := append([]protocol.Message(nil), base...)
	out = append(out, protocol.Message{
		Role: protocol.RoleAssistant,
		ToolCalls: []protocol.ToolCall{{
			ID:        string(call.CallID),
			Name:      call.Name,
			Arguments: call.Arguments,
		}},
	})
	out = append(out, protocol.Message{
		Role:    protocol.RoleTool,
		Results: []protocol.ToolResult{{CallID: string(call.CallID), Content: toolContent}},
	})
	return out
}
