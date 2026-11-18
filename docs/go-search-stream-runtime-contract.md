# Streaming counterpart to opt-in Responses→Chat search

Scope: source-facing SSE for the already accepted complete
`docs/go-search-runtime-contract.md` vertical. Same
binding, one search, same loss. No new tools, filters, location,
context-size, forced choice, state, or previous search items.

## Admission (stream:true only)

- `DecodeSearchAdmission` accepts `"stream": true` for this path. An
  absent or false flag selects the complete reply. All other refusals
  (strict, unbound, unknown keys, extra tools, filters, `store`,
  `max_tool_calls`, non-text history, tool history, `include` beyond
  `web_search_call.action.sources`) stay pre-I/O with zero calls.
- Pin model/provider once, plan effort, enforce policy, mint loss
  header before any I/O, as in the complete path.

## Orchestration (handler-owned, not codec-owned)

- Stay in `maybeServeResponsesSearch` beside the existing pin, policy,
  and transport. A standalone codec must not own the outer deadline
  or the generation slot.
- Hold one generation slot and the same request context across all
  three I/Os: first Chat round, SearXNG call, final Chat stream.
  Same provider credential, session identity, endpoint, and upstream
  model for both model rounds. Search uses only the bound backend
  endpoint/headers; model and search credentials never cross.
- At most one backend call and one continuation per request. Retries
  resend retained continuation bytes only; they never rerun the
  search and never rebuild the query.

## Round 1: bounded non-stream decision

- Send the existing synthetic Chat request with `stream=false`
  (`router_web_search`, auto choice). Decode with the existing
  `DecodeChatResponse` into the typed `Response` IR.
- `ExtractSearchQuery` decides: exactly one planned call, valid opaque
  call ID, closed `{query}` object, nonblank UTF-8, ≤500 runes, no
  duplicates/extras, `tool_calls` finish, no text/refusal/mixed output.
  Small bounded buffering here is internal and never downstream.
- No-call path (stop/length/content-filter, no calls): stream the
  already-decoded response as typed events through the Responses encoder,
  committer, and tools/choice echo path.
  One model round, no `web_search_call`, public model preserved.
- Query failure, malformed wrapper, second-call shape, or budget
  failure is pre-commit: HTTP error, no 200, no events, close
  upstream, release slot.
- Budget: if `max_output_tokens` is set, require authoritative
  round-1 usage and `SubtractBudget`; exhausted or unrepresentable
  accounting fails pre-commit before search I/O.

## Search interlude (still pre-commit)

- One `searxng` call under the same context. Preserve the opaque call
  ID. Filter at the adapter; `BuildSearchToolContent` bounds and drops
  trailing results. Query/snippets never log.
- Backend failure or deadline is pre-commit: sanitized 502 (504 on
  request-deadline expiry), no continuation, no downstream bytes.
- Commit no downstream headers or events before search succeeds and
  the final upstream stream yields its first valid translatable frame.

## Round 2: stream downstream, no buffer, no replay

- Continuation = original messages + assistant call (same ID) + tool
  result bytes, `tool_choice: none`, trusted citation description.
  Encode once; retain bytes for transport retries only.
- Open the final Chat request with `stream=true` under the same
  headers/session/endpoint. Pipe the upstream SSE through the existing
  decoder → lifecycle → `ResponsesEventEncoder` → echo writer →
  committer. Incremental deltas flush; never buffer final text outside
  the encoder-owned bounded `TerminalBuffer`; never build a Chat JSON
  intermediate; never reparse source bytes.
- Refuse post-decision tool calls, `MalformedToolEvent`, and
  `tool_calls` finish post-commit per the complete gate. Usage from
  round 1 plus terminal round-2 usage aggregate once with
  `AddSearchUsage` (checked arithmetic, search adds no tokens) and is
  emitted exactly once in the terminal event.

## Downstream SSE order, IDs, lineage

- Mint one downstream `resp_*` ID and `created_at` at
  `response.created`; reuse for every later event including fallback
  `response.failed`. Sequence numbers monotonic from zero.
- Order: `response.created`, `response.in_progress` (both with usage
  null, public model, tools=`[web_search_preview]`,
  `tool_choice="auto"` echo), then one synthetic search item at
  `output_index 0`: `response.output_item.added` with the
  router-issued `ws_*` `web_search_call` (`action: {type: search,
  query}` plus `sources` URLs only when requested), then its
  `response.output_item.done` (`status: completed`). Final answer
  text follows at subsequent `output_index` values through the
  standard message/part/delta/done events.
- Opaque upstream call ID preserved verbatim inside the continuation;
  downstream `ws_*`/`msg_*` IDs are router-minted and never collide
  with it. Emit the search item exactly once; never re-emit on retry
  or after `response.completed`.
- Terminal `response.completed` (or `response.incomplete` on
  length/content-filter mapping) carries the full output with the
  search item first, summed authoritative usage, and citation
  annotations. Emit usage and search lineage exactly once.

## Sources, citations, Unicode spans

- Annotations only for visible `[S1]`..`[S10]` markers that name a
  result actually transferred to the model. Unknown markers and
  model-written URLs never acquire provenance. Bound at
  `maxSearchAnnotations`; excess fails closed.
- Compute spans at terminal assembly from the encoder `TerminalBuffer`
  text plus the kept `SearchResultView` list, reusing the complete-path
  rune-offset rule (`start_index`/`end_index` in characters). The existing
  terminal text buffer joins split markers so the encoder can validate citations
  without reparsing downstream frames.
- `include` controls `action.sources` only, as in the complete path.

## Provider identity and slot/deadline

- Both model rounds use `OutboundHeaders` for the pinned provider and
  the same session/endpoint/credential. Upstream model reports update
  terminal metadata only; downstream model stays the public alias.
- The outer context and held slot cover search and both rounds. First
  failure mode (transport 429/5xx per retry policy) retries the same
  bytes; search is outside retry. Client cancel closes upstream
  promptly and releases the slot.

## Pre-commit vs post-commit failures

- Pre-commit (before first downstream write): admission, round-1
  transport/decode/query/budget, search, continuation encode, or final
  stream failure before commitment → ordinary Responses HTTP error
  (502; 504 on deadline), never an empty 200. Includes truncated
  upstream, invalid frames, missing authoritative usage, and
  unrepresentable events.
- Post-commit: keep 200, emit exactly one `response.failed` reusing the
  committed ID and next sequence number (encoder `Abort` or committer
  fallback when the translator did not emit), then terminate with no
  `response.completed` and no duplicate codec error. Downstream write
  failure fails fast, closes upstream synchronously, releases the slot.

## External SSE contract

- Reuse the proven Responses event set already emitted by
  `ResponsesEventEncoder`: `response.created`, `response.in_progress`,
  `response.output_item.added`, `response.content_part.added`,
  `response.output_text.delta`, `response.refusal.delta`,
  `response.output_text.done`, `response.content_part.done`,
  `response.output_item.done`, `response.completed`,
  `response.incomplete`, `response.failed`.
- Represent the completed search as `output_item.added/done` carrying
  the `web_search_call` item (mirrors the complete `output[0]`
  shape). Do not invent new `response.web_search_call.*` names in this
  slice; any official search-event adoption is additive and needs a
  separate contract review against the Responses streaming docs.

## Offline acceptance

Fake Chat + SearXNG behind the router. Prove: same credential,
endpoint, session, and call ID across rounds; one search under two
model rounds; continuation retry resends bytes without a second
search; no-call streams with no search item; strict/unbound and
unsupported controls refuse pre-I/O; malformed/duplicate args, mixed
output, and second-call finals refuse (second-call only after one
search); search failure sanitized pre-commit; deadline/cancel mapping;
usage overflow/budget exhaustion; sources gated by `include`; exact
rune citation spans with split-marker and forged-marker/URL cases;
loss header, public model, and source echo on streamed events.
