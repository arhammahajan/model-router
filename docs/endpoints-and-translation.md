# Endpoints and protocol translation

This document describes the HTTP endpoints, ingress dispatch rules, intermediate representation, reasoning translation, and dialect translation scopes in `model-router`.

## Architecture

Translated requests use the shared `Generation` intermediate representation. Complete translated responses from all four upstream dialects decode once into the shared `Response` representation and encode through shared typed target encoders. Compatibility wrappers select only the source decoder and target encoder.

Native passthrough bypasses translation. All translated streams from all four upstream dialects use shared framing, shared source events, a shared pipeline, and reusable target encoders. Compatibility wrappers select only the source framing, source decoder, and destination encoder.

Gemini JSON-array stream completion waits for the closing bracket. SSE streams return at the source terminal. Reported initial usage stays distinct from authoritative terminal accounting. These architecture guarantees do not establish full feature parity or production readiness.

## HTTP endpoints

| Endpoint | Behavior |
| --- | --- |
| `POST /v1/chat/completions`, `POST /chat/completions` | Accepts OpenAI Chat ingress. Routes natively when the upstream dialect matches, or translates to Anthropic, Responses, or Gemini according to ingress dispatch. |
| `POST /v1/responses`, `POST /responses` | Accepts OpenAI Responses ingress. Routes as native passthrough with stateful-field passthrough and no conversation store, or translates to Chat, Anthropic, or Gemini. Other paths under `/v1/responses/` return HTTP 404 in the Responses error shape. |
| `POST /v1/messages`, `POST /messages` | Accepts Anthropic ingress. Routes as native passthrough, or translates to Chat, Responses, or Gemini. Other paths under `/v1/messages/` return HTTP 404 in the Anthropic error shape. |
| `POST /v1/messages/count_tokens`, `POST /messages/count_tokens` | Proxies Anthropic token-count requests natively. Does not acquire a generation slot. Validates the `{input_tokens}` response body up to 64 KiB. Returns HTTP 400 with `count_tokens_unsupported` unless the route is declared or first-party. |
| `POST /v1beta/models/{name}:generateContent`, `POST /v1beta/models/{name}:streamGenerateContent`, and the `/v1/`, versionless `/models/`, and `tunedModels` forms | Accepts Gemini ingress. Routes natively or translates to Chat, Responses, or Anthropic according to ingress dispatch. Reads the model and method from the URL, accepts only `?alt=sse`, `?alt=json`, and auth-only `?key=` query parameters, and rejects `model` and `stream` in the request body. Unknown methods return HTTP 404, and known unsupported methods return HTTP 501. Downstream framing follows the requested `alt` parameter. It sends `text/event-stream` for `alt=sse` and an incremental JSON array otherwise. Native passthrough preserves the upstream content type for both JSON-array and SSE streams. |
| `GET /v1/models`, `GET /models` | Returns the header-authenticated canonical model list. Includes trusted `metadata.router` fields and preserves unrelated metadata. Omits aliases and disabled routes. Query keys do not authenticate this endpoint. |
| `GET /muse-code/models`, `GET /v1/muse-code/models` | Returns the header-authenticated Muse projection with typed reasoning variants and display-limit defaults. Serves cached reads only. |
| `GET /v1beta/models`, `GET /v1beta/models/{id}` | Serves Gemini model discovery with canonical resources, pagination, and known limits. Omits unknown limits. Authenticates with headers or `?key=`. |
| `GET /` | Returns the public Go executable name, build version, and endpoint index. |
| `GET /livez`, `GET /health`, `GET /healthz` | Returns `{"status":"ok"}` with no network calls. |
| `GET /readyz` | Returns HTTP 200 when at least one enabled named route exists, and HTTP 503 otherwise. Dynamic-only routes do not satisfy readiness. |
| `GET /diagnostics` | Reports readiness, providers, discovery refresh state, and cache outcomes from a pinned snapshot. Requires header authentication when router keys are configured. See [Configuration and CLI](configuration.md#diagnostics-endpoint). |
| Any other path | Returns HTTP 404 in the closest ingress error shape. Gemini-like paths use the `google.rpc.Status` shape. |

## Ingress dispatch

After resolving the route, the router selects native passthrough, cross-dialect translation, or rejection.

When the ingress dialect matches the upstream route dialect, the router uses native passthrough. Native passthrough is byte-identical unless the public model alias differs from the upstream model ID or a confirmed generated llama.cpp Chat request carries `reasoning_effort`. In that llama.cpp case, the narrow mapper consumes `reasoning_effort`. Body mutations preserve `RawMessage` subvalues and exact large integers.

For Gemini URLs, the router decodes `EscapedPath` once, the same way Python decodes `urlsplit().path` with a single `unquote`. Raw `/` and `%2F` resolve to the same `/` model, while `%252F` decodes to a literal `%2F` and routes to the percent alias. Traversal and query encodings cannot escape the route. The router maps the public name to the upstream model path segment with traversal and query escaping.

The router supports 12 translated dialect pairs through the typed intermediate representation with strict or lossy policy enforcement.

| Client ingress | Upstream route | Dispatch behavior |
| --- | --- | --- |
| Chat | `anthropic` | Translates through the typed IR with strict or lossy policy and commitment-aware SSE. |
| Anthropic | `openai-chat` | Translates through the typed IR with strict or lossy policy and commitment-aware SSE. |
| Chat | `openai-responses` | Translates with the Responses-destination loss plan, commitment-aware SSE into Chat chunks, and `store: false` forced on the wire. |
| Responses | `openai-chat` | Translates with the Chat-destination loss plan, commitment-aware SSE into Responses events, and pre-I/O refusal of stateful references. |
| Responses | `anthropic` | Translates with the Anthropic-destination loss plan, commitment-aware SSE into Responses events, and pre-I/O refusal of stateful references. |
| Anthropic | `openai-responses` | Translates with the Responses-destination loss plan and commitment-aware SSE into Anthropic events. |
| Chat | `gemini` | Translates with the Gemini-destination loss plan, commitment-aware SSE into Chat chunks, and SSE or JSON-array decoder selection from the reviewed upstream `Content-Type`. |
| Gemini | `openai-chat` | Translates with the Chat-destination loss plan and commitment-aware SSE or incremental JSON-array downstream framing following the requested `alt` parameter. |
| Responses | `gemini` | Translates with the Gemini-destination loss plan, URL-owned `generateContent` or `streamGenerateContent?alt=sse`, decoder selection from upstream `Content-Type`, diagnosis of source `store: true` as a loss, and pre-I/O refusal of stateful references. |
| Gemini | `openai-responses` | Translates with the Responses-destination loss plan, URL-owned model and method, `store: false` forced on the wire, and downstream framing following `alt`, where `alt=sse` sends SSE and other values send an incremental JSON array. |
| Anthropic | `gemini` | Translates with the Gemini-destination loss plan and URL-owned `generateContent` or `streamGenerateContent?alt=sse`. |
| Gemini | `anthropic` | Translates with the Anthropic-destination loss plan and downstream framing following the requested `alt` parameter. |

Any other dialect pair, including an unknown dialect, returns HTTP 400 with `unsupported_route` in the ingress error envelope before any upstream I/O.

## Gemini 4 response extensions

Gemini 4 upstreams attach three routine response extensions that no translated client dialect defines:

- A `thoughtSignature` field on ordinary parts.
- A `finishMessage` field on tool-call finishes.
- TEXT-modality `usageMetadata` breakdowns along with the gateway `cost` and `opencode` billing envelope.

Strict Gemini routes refuse all three extensions with HTTP 502 and value-free error paths. Lossy routes configured with `compatibility = "lossy"` validate each extension after bounded checks and drop it. Dropped extensions never override authoritative provider counters.

Gemini 4 default thinking reports `thoughtsTokenCount`. Strict Gemini routes refuse nonzero `thoughtsTokenCount` with HTTP 502. On lossy Gemini routes, the router validates the wire total equation and folds `thoughtsTokenCount` into the canonical completion total. Chat and Responses downstream render the folded share through `reasoning_tokens`, while Anthropic downstream keeps the aggregate `output_tokens` and omits the reasoning breakdown.

Because the Gemini upstream rejects `functionCall` history parts that lack their original signature, lossy bridges mint a wrapped public tool-call identity in the format `gts1_<base64url({"s","i"})>`. This identity carries the opaque signature across the stateless client round trip. Every ingress decoder restores the provider identity and signature before identity matching, and the Gemini encoder emits the signed part again on the next turn. Non-canonical identities, including provider-supplied lookalikes, pass through untouched.

Media modality shares, thought parts, server tools, and malformed extensions fail closed in both strict and lossy modes.

## Shared translation rules

All six bidirectional dialect pairs share the core translation, compatibility, streaming, bound, and header isolation contracts described below.

### Typed IR and strict or lossy policy

Every translator uses the typed intermediate representation in `internal/protocol` and never uses a `map[string]any` semantic model. The router evaluates destination-specific loss plans against the provider compatibility setting only, and clients cannot select lossy mode.

Strict mode is the default. In strict mode, the router refuses any planned loss before upstream I/O with HTTP 400 and `translation_loss`. In lossy mode, the router drops or approximates reviewed mappings and emits the `x-model-router-loss` header on JSON and SSE responses. Each loss entry reports `field`, `reason`, and `action`, and never includes request values or prompt text.

Unknown request fields fail validation with HTTP 400 and are never silently accepted. Unrepresentable upstream response elements fail closed with HTTP 502 in both strict and lossy modes. A truncated stream never produces a synthetic success marker.

### Streaming commitment and failure handling

Translated streams use lazy commitment before the first valid downstream write. If an upstream stream fails or truncates before commit, the router returns HTTP 502 for invalid or truncated responses, or HTTP 504 for a typed deadline expiry. It never returns an empty HTTP 200.

After commit, the router keeps HTTP 200 and terminates without a synthetic success marker using dialect-native failure behavior:

- Chat downstream terminates silently with no `[DONE]` marker, no error frame, and no fabricated finish chunk.
- Responses downstream emits one framed `response.failed` event. Reviewed paths emit this event from the translator. When it is missing, the boundary emits it once and reuses the committed response ID and the next unique sequence number. Responses downstream never fabricates `response.completed` or a duplicate codec error.
- Anthropic downstream emits a single framed error with no `message_stop`.
- Gemini SSE downstream emits one sanitized `data: {error: google.rpc.Status}` frame with no `[DONE]` marker and no fabricated `STOP` or usage. Gemini JSON-array downstream stops without writing the closing `]` and does not repair or retry.

Only a framed `event:` line counts as an SSE event. Model text containing `event: error` inside a data payload stays payload data and never acts as framing.

A downstream write or flush failure is sticky. The router closes the upstream body immediately, cancels upstream work, and releases the generation slot. The router attempts no raw writes after a downstream failure, and the write deadline resets on every exit path, including the fallback error path. Translators spawn no goroutines, do not buffer whole generations, and do not run dry-run encodes.

### Bounded retained output and size limits

Stream deltas flush downstream immediately. Only terminal reconstruction retains output, subject to fixed limits:

- `max_body_bytes` caps request and response bodies. Gemini bridges check translated JSON bodies against `max_body_bytes` again after translation because escaping can expand a fitting upstream body past the limit. Oversize translations fail closed pre-commit with a sanitized ingress-native HTTP 502. JSON downstream bodies also stay within the per-frame, per-argument, aggregate, and tool-call bounds below.
- `MaxFrameBytes` caps a single SSE frame or JSON-array element at 8 MiB.
- Each accumulated tool-argument field has a 1 MiB limit.
- `MaxStreamArgsChars` caps total accumulated tool arguments, or combined retained text and arguments on Responses and Anthropic paths, at 8 MiB per stream. This cap limits retained arguments rather than total stream bytes. `max_body_bytes` caps non-stream bodies, and the absolute deadline bounds total streams.
- Each stream allows at most 1024 tool calls or content blocks.

### Header, authentication, and query isolation

Bridged requests never forward client credentials. The router uses Gemini `?key=` for authentication only, strips it, and forwards only `?alt=sse` upstream. Bridged requests forward only destination-dialect opt-ins. The router drops Connection-nominated, session, idempotency, authentication, cookie, and quota or billing headers. Provider static headers override inbound opt-ins, and provider authentication wins last.

## Reasoning and thinking translation

Reasoning effort and thinking controls pass through a shared typed planner that checks the route's capability contract in the pinned snapshot.

### Capability contract gating

Unknown capability facts are neutral. Explicit unsupported facts or known enum mismatches become translation losses, which strict mode rejects before upstream I/O and lossy mode drops and reports. When `reasoning.supported = false`, requesting `none` removes the control cleanly with no loss record. Responses summaries and unknown-runtime generated providers keep their pre-I/O reasoning refusal in both modes. Other reasoning controls, destinations, and private response-reasoning refusals keep their existing policies.

### Chat and Responses effort translation

Chat ingress targeting a Responses destination carries `reasoning_effort` as `reasoning.effort`. Responses ingress targeting an ordinary Chat destination carries `reasoning.effort` as `reasoning_effort` through the same typed request IR and effort planner. Exact mappings preserve the literal value and add no loss header.

### Confirmed llama.cpp and Strata targets

A live probe identifies Strata from the first build-info token and otherwise defaults to llama.cpp. On confirmed llama.cpp native Chat requests, the router consumes `reasoning_effort`. Recognized levels set a default thinking budget, and `none` sets `enable_thinking = false` inside `chat_template_kwargs`. Explicit native values win, including null budgets. The router treats a null `chat_template_kwargs` value as absent and leaves other non-object `chat_template_kwargs` values untouched. The router consumes unknown, `max`, null, and non-string efforts without adding validation or loss headers.

Strata and ordinary native providers retain raw passthrough. Translated Responses named effort targeting known local runtimes uses the same literal mapper and capability policy before I/O. For those routes, llama.cpp emits budgets or off, and Strata emits the literal effort. Mapper capability facts report the configured or inferred enum values, native wire shape, and source provenance. They do not assert that the model supports reasoning or invent budget defaults. Cache-only runtimes stay unknown until a live probe completes, so native requests pass through, translated effort stays refused, and mapper facts remain absent.

### Anthropic thinking translation

Anthropic `thinking: {type: "disabled"}` carries the typed disable intent to Chat and Responses. Unknown capabilities emit literal `none`. An explicit unsupported reasoning capability strips the control cleanly. An unsupported toggle or effort enum produces a strict or lossy loss with origin `thinking`. Confirmed llama.cpp Chat routes use their disable toggle. Gemini-bound disable requests produce a translation loss.

Anthropic `thinking: {type: "enabled", budget_tokens: N}` carries an explicit budget to confirmed llama.cpp Chat routes. Gemini accepts it when the pinned capability declares `reasoning.budget.supported = true` with native parameter `{protocol = "gemini", path = "generationConfig.thinkingConfig.thinkingBudget"}`. Both targets enforce known budget bounds. Ordinary Chat routes and Gemini routes without that capability diagnose a dropped budget. An explicit valid `display` field on enabled thinking produces a separate loss. Responses refuses enabled thinking before I/O. Adaptive thinking remains unavailable on translated routes, and native Anthropic passthrough continues to forward the original body.

### Gemini reasoning and thinking translation

Chat `reasoning_effort` and Responses `reasoning.effort` translate to Gemini `thinkingLevel` when the pinned capability declares support, the matching Gemini native parameter, and an enum that includes the requested level. Unreviewed or excluded levels produce a translation loss.

Gemini generation ingress carries reviewed thinking levels, disable requests, and explicit budgets through the same typed planner for Chat and Responses destinations. Budget `-1` and `THINKING_LEVEL_UNSPECIFIED` mean no explicit control, and budget `0` disables thinking. Explicit positive budgets map cleanly only to confirmed llama.cpp Chat targets and otherwise become diagnosed losses. Named levels retain effort gates and the existing llama literal budget map, and derived budgets do not inherit explicit-budget bounds. `includeThoughts` keeps its independent loss policy. Malformed controls fail before policy in both modes, including `XHIGH`, which is outside the translated source grammar. Native Gemini remains raw passthrough.

## Chat and Anthropic

The Chat and Anthropic bridge covers ordered text messages, ordinary JSON-schema function tools, `temperature`, `top_p`, `max_tokens`, `max_completion_tokens`, `stop`, `stream`, `stream_options.include_usage`, and `extra_body.top_k`. It hoists system messages and merges adjacent same-role messages. Function tool support covers definitions, `none`, `auto`, `any`, and named tool choice, `parallel_tool_calls`, and call and result identity in history and streamed output.

Because Anthropic requires `max_tokens`, the bridge sends an absent cap as `max_tokens: 4096`. This default is documented and does not produce a loss record. The bridge accepts `n: 1` as an identity without a loss record.

Unknown request fields fail validation with HTTP 400 and are never silently accepted. OpenAI `metadata` always requires a same-protocol route.

Upstream response elements with no Chat representation fail translation with HTTP 502 in both strict and lossy modes. These include thinking blocks, server tools, and unknown stop reasons or deltas.

## Chat and Responses

The Chat and Responses bridge covers ordered text messages, ordinary function tools, `temperature`, `top_p`, `max_tokens`, `max_completion_tokens`, `stream`, and `stream_options.include_usage`. Function tool support covers definitions, `none`, `auto`, `required`, and named tool choice, `parallel_tool_calls`, and call and result identity in history and streamed output.

Stateless translation forces `store: false` on the Responses wire. A source `store: true` is a diagnosed loss and is never silently changed. The bridge encodes Responses tools with explicit `strict: false` to preserve Chat non-strict semantics.

The bridge refuses stateful request references before upstream I/O with HTTP 400 because it keeps no conversation store. Refused fields are `previous_response_id`, `conversation`, `prompt`, and `background`.

Incomplete Responses with `incomplete_details.reason` set to `max_output_tokens` or `content_filter` map to Chat `finish_reason` values `length` and `content_filter`. Cached usage details pass through under the same scalar validation as the Chat and Anthropic path.

During streaming, the Responses terminal event reconstructs the final payload from earlier deltas within the shared output bounds without duplicating emitted chunks, and never buffers an unbounded generation. Post-commit Responses downstream emits a single `response.failed` event, while Chat downstream terminates silently with no `[DONE]` marker and no fabricated finish chunk.

## Anthropic and Responses

The Anthropic and Responses bridge covers ordered text messages, ordinary function tools, `temperature`, `top_p`, `max_tokens`, `max_output_tokens`, and `stream`. Function tool support covers definitions, tool choice, and call identity in history and streamed output.

Destination loss plans differ by direction and apply against provider compatibility only:

- From Responses to Anthropic, the bridge records a loss and drops a source `temperature` above 1 rather than clamping it, so strict policy refuses it. Responses implicit-strict normalization has no Anthropic equivalent. The bridge records omitted or null `strict` as a loss, maps explicit `false` cleanly, and refuses explicit `true` because Anthropic strict guarantees lack review. Because Anthropic requires `max_tokens`, the bridge sends an absent Responses cap as `max_tokens: 4096` without a loss record. The bridge refuses stateful references `previous_response_id`, `conversation`, `prompt`, and `background: true` before upstream I/O with HTTP 400 `unsupported_responses_state`.
- From Anthropic to Responses, `top_k` and `stop_sequences` have no reviewed Responses mapping, so the bridge records well-formed values as losses and never forwards them. Explicit tool `strict` values of `true` or `false` carry over cleanly. Omitted or null Anthropic tool `strict` maps cleanly to explicit Responses `strict: false`, which preserves the source non-strict default without a loss record.

Response translation fails closed with HTTP 502 in both strict and lossy modes for unrepresentable upstream elements. These include thinking blocks, server tools, citations and annotations, refusal content, logprobs, reasoning accounting, unknown stop reasons or members, and conflicting call or response identities. They are never silently dropped and never produce a success marker.

When streaming from Anthropic upstream toward Responses, deltas flush downstream immediately and terminal reconstruction stays within the shared bounds, including 8 MiB combined retained text and arguments and 1024 content blocks or tool calls. Post-commit failure keeps HTTP 200 and emits one `response.failed` event carrying the validated `tools` and `tool_choice` echo, with no fabricated `response.completed` or duplicate error frame.

When streaming from Responses upstream toward Anthropic, prompt accounting arrives only at the terminal event. `message_start` therefore carries zeros as an explicit placeholder, and the terminal `message_delta` carries the authoritative totals. Clients must apply the final totals and never sum the placeholder.

## Chat and Gemini

The Chat and Gemini bridge covers ordered text messages, ordinary JSON-schema function tools, `temperature`, `top_p`, `max_tokens`, `max_completion_tokens`, `stop`, `stream`, and `stream_options.include_usage`. It hoists system messages and merges adjacent same-role messages. Function tool support covers definitions, `none`, `auto`, `any`, and named tool choice, `parallel_tool_calls`, and call and result identity in history and streamed output.

### Tool schemas, request controls, and URL rules

The Gemini wire uses `parametersJsonSchema` with full JSON Schema and no OpenAPI-subset rewriting. Supplying both `parameters` and `parametersJsonSchema` returns HTTP 400.

`stop` accepts at most five entries. When `stop` has more than five entries, the bridge records a loss instead of truncating the list. Omitted or explicit `false` Chat tool `strict` maps cleanly because the Gemini wire carries no strict member, while explicit `true` is a diagnosed loss. `extra_body.top_k` remains a translation loss, and structured-output controls lack a reviewed mapping. The bridge accepts `n: 1` as an identity without a loss record. Stateless translation carries no `store` on the Gemini wire, so explicit `false` matches cleanly and explicit `true` is a diagnosed loss.

The router sends the model and streaming mode in the upstream URL only, via `generateContent` or `streamGenerateContent?alt=sse`. The wire body never carries `model` or `stream`.

### Call identity

The bridge preserves a supplied Gemini `functionCall` or `functionResponse` ID verbatim when it is a non-empty string within the size bound. Empty or oversize supplied IDs return HTTP 400 in both modes. No character-grammar rewrite applies to opaque IDs. The `[A-Za-z0-9_-]{1,64}` grammar constrains function names only.

The router mints a local compatibility ID only when one is missing. It uses `gdec-N` on ingress decode and `g2c-call-n` on the complete response path. The complete path reserves every supplied ID in the request before minting so a minted name never collides. The stream path skips already-seen IDs and fails closed if a later supplied ID collides. The router never invents provider-private signatures or opaque resource IDs.

### Request decoding, media, and part validation

Gemini ingress decodes closed camelCase and snake_case spellings and refuses collisions. Unknown nested members and router-held state fields `cachedContent` and `continuationToken` are refused before upstream I/O.

Gemini user `inlineData` preserves part order and original bytes in standard padded base64. PNG, JPEG, WebP, and GIF become image inputs on Chat, Responses, and Anthropic. WAV and MP3 become `input_audio` on Chat and produce a loss on the other translated routes. Image and audio display names produce a loss.

Inline PDFs become file inputs on Chat and Responses and document inputs on Anthropic. A safe PDF display name becomes the filename or document title, and an unsafe name produces a loss. Without a safe name, OpenAI file inputs use `document.pdf` and Anthropic omits the title. See [OpenAI file inputs](https://developers.openai.com/api/docs/guides/file-inputs) and [Anthropic PDF documents](https://platform.claude.com/docs/en/build-with-claude/citations#pdf-documents).

Chat user `image_url` data URLs use the same four image formats when translating to Gemini. Chat `detail` values other than `auto` produce a separate loss. Remote image URLs produce a loss without fetching the URL.

`FunctionResponse.response` is a required object. Chat tool results map to it as the exact string form `response.output` and are never parsed into structured fields. `FunctionResponsePart` accepts only `inlineData` using `FunctionResponseBlob` or `fileData` using `FunctionResponseFileData` with one payload, plus the closed optional enum and string members. Anything else returns HTTP 400 in both modes.

The same malformed-versus-loss rule holds for part metadata. The router returns HTTP 400 in both modes, without loss records, for out-of-contract enum members such as `scheduling` and media enums, malformed MIME types, and raw strings supplied where a Part object is required.

### Response validation, terminal usage, and streaming

Gemini response thought parts, opaque signatures outside the lossy `gts1_` tool-call wrapper, media, executable code, server tools, grounding and citation payloads, logprobs, and nonzero `toolUsePromptTokenCount` fail closed with HTTP 502 in both modes. Nonzero upstream `thoughtsTokenCount` fails closed with HTTP 502 on strict routes and folds into completion and `reasoning_tokens` on lossy routes as described in [Gemini 4 response extensions](#gemini-4-response-extensions). In the forward direction, Chat `reasoning_tokens` scalars map into `thoughtsTokenCount` as a scalar mapping only, with no thought controls claimed. Gemini `usageMetadata.promptTokenCount` already includes the cached share and is never double-counted.

Every terminal across JSON, SSE, and JSON-array responses must carry its own usage. A finish without terminal usage returns HTTP 502 and never produces an underreported total. SSE returns immediately at the finish chunk without waiting for EOF. A JSON array requires its closing `]` with no trailing comma and returns without waiting for EOF. Chat downstream emits usage as a separate empty-choices chunk when `include_usage` is true, with `"usage": null` on ordinary chunks, and fails if a terminal lacks usage.

Genuinely model-malformed Chat tool arguments map to a Gemini HTTP 200 with `MALFORMED_FUNCTION_CALL` as the native finish marker, never an ordinary `STOP` and never an invalid function rendering. Parsing, envelope, and bound failures such as oversize frames, oversize arguments, oversize aggregates, or truncated and malformed envelopes remain HTTP 502 or post-commit aborts.

Toward Chat from Gemini upstream, the router selects the framing decoder from the reviewed upstream `Content-Type` and accepts `text/event-stream` or `application/json` array without body sniffing. Any other content type returns a pre-commit HTTP 502. Downstream writes Chat SSE through the shared lazy Chat committer with the exact public model.

Toward Gemini from Chat upstream, downstream framing follows the requested `alt` parameter. `?alt=sse` sends `text/event-stream` data-only frames, and omitted `alt` or `alt=json` sends an incremental `application/json` array. Pre-commit errors return Gemini `google.rpc.Status` HTTP 502 or HTTP 504.

## Responses and Gemini

Response translators run from the upstream dialect back to the client.

| Client ingress | Upstream | JSON response translator |
| --- | --- | --- |
| Responses | Gemini | `TranslateGeminiJSONToResponses` |
| Gemini | Responses | `TranslateResponsesJSONToGemini` |

The Responses and Gemini bridge covers ordered text messages, ordinary function tools, `temperature`, `top_p`, `max_output_tokens`, `maxOutputTokens`, and `stream`. Function tool support covers definitions, `none`, `auto`, `required`, and named tool choice, and parallel-call identity in history and streamed output.

### Request rules

Gemini ingress forces `store: false` on the Responses upstream wire. Responses ingress sends no `store` field to Gemini, and an explicit source `store: true` is a diagnosed loss. The bridge refuses Responses stateful references `previous_response_id`, `conversation`, `prompt`, and `background: true` before upstream I/O because it keeps no conversation store.

Gemini source `stopSequences` accepts at most five non-empty strings. Malformed arrays or lists with more than five entries return HTTP 400 in both modes before destination loss planning. Valid lists become a dropped loss for the Responses destination, which has no stop-sequence contract.

The router sends the Gemini model and method in the upstream URL only, via `generateContent` or `streamGenerateContent?alt=sse`. The wire body never carries `model` or `stream`. Upstream Gemini `Content-Type` selects the framed decoder between `text/event-stream` and `application/json` array without body sniffing, and any other content type returns a pre-commit HTTP 502. Downstream Gemini framing follows `alt`, where `?alt=sse` sends SSE data-only frames and omitted `alt` or `alt=json` sends an incremental JSON array.

### History, call IDs, and ordered parts

A Gemini `FunctionResponse.response` object maps to a Responses `function_call_output` whose `output` keeps the entire object serialized as a compact JSON string. The bridge never unwraps an `output` member and never parses a JSON-looking inner string. In the reverse direction, a Responses output string stays verbatim in `response.output`.

Call IDs are opaque bounded strings preserved verbatim. Missing IDs mint router-local compatibility IDs in the `g2r-call-N` scope. The stream path skips already-seen IDs and fails closed on a late supplied-ID collision. The bridge accepts a legacy minimal `response.created` envelope containing only an ID and validates any additional supplied members.

From Gemini to Responses, the bridge emits parts in response-part order, such as text, call, and text, in both JSON and streams. From Responses to Gemini, the bridge holds pending complete calls until all lower output items close and emits them in logical `output_index` order. The bridge refuses higher text that crosses an unfinished lower item before the order changes so it never buffers a whole generation. The terminal event never duplicates streamed text. Stream terminals check consistency across `id`, `status`, `kind`, `args`, and hash.

### Reasoning, cache, and post-commit framing

Nonzero Gemini `toolUsePromptTokenCount` fails closed with HTTP 502 in both modes. Nonzero Gemini `thoughtsTokenCount` fails closed with HTTP 502 on strict routes and folds into `output_tokens` and `reasoning_tokens` on lossy routes as described in [Gemini 4 response extensions](#gemini-4-response-extensions). The Responses `reasoning_tokens` scalar maps to Gemini `thoughtsTokenCount`. Cached `cached_tokens` maps to `cachedContentTokenCount` inside the prompt total and is never double-counted. Totals are exact, with total equal to input plus output.

Responses output `reasoning` items remain unmapped and fail closed. The shared helper validates the top-level Responses `reasoning` request echo for Chat, Anthropic, and Gemini destinations. It checks enum fields `summary`, `generate_summary`, `context`, and `effort`, plus string `mode` per the Responses create reference. This echo never executes reasoning or claims chain-of-thought controls.

Responses downstream reuses the validated typed `tools` and `tool_choice` echo after enrichment within bounds. Post-commit failure follows the shared dialect-native framing rules. Responses downstream emits one framed `response.failed`, Gemini SSE emits one sanitized `data: {error}` frame, and Gemini JSON-array downstream stops without the closing `]`.

## Anthropic and Gemini

Response translators run from the upstream dialect back to the client, so their names follow the upstream source rather than the ingress.

| Client ingress | Upstream | JSON response translator |
| --- | --- | --- |
| Anthropic | Gemini | `TranslateGeminiJSONToAnthropic` |
| Gemini | Anthropic | `TranslateAnthropicJSONToGemini` |

The base scope for Anthropic and Gemini is ordinary text and ordinary JSON-schema function tools, plus the supported Gemini user `inlineData` images and PDFs described under [Chat and Gemini](#request-decoding-media-and-part-validation) and the capability-gated Anthropic `thinking` budget translation described under [Reasoning and thinking translation](#anthropic-thinking-translation). Unknown source blocks return HTTP 400 in both modes.

Covered fields are ordered text messages, ordinary function tools with definitions, `none`, `auto`, `any`, and named tool choice, call and result identity in history and streamed output, `temperature`, `top_p`, `max_tokens`, `maxOutputTokens`, and `stream`. There are no provider version floors.

### Stop sequences, IDs, URLs, and framing

The bridge validates Gemini-source `stopSequences` first as an array of at most five non-empty strings. Malformed arrays or lists with more than five entries return HTTP 400 in both modes before loss planning. Valid lists map cleanly to Anthropic `stop_sequences`. Anthropic-source stop sequences map cleanly to Gemini within its five-entry cap, and larger source lists produce a loss record.

Call IDs are shared opaque bounded strings preserved verbatim with no character-grammar rewrite. Function names keep the `[A-Za-z0-9_-]{1,64}` grammar.

The router sends the Gemini model and streaming method in the upstream URL only, via `generateContent` or `streamGenerateContent?alt=sse`. The wire body never carries `model` or `stream`. Upstream Gemini streams always request `alt=sse`. The router selects the framed decoder from the actual upstream `Content-Type` and accepts `text/event-stream` or `application/json` array without body sniffing.

Downstream Gemini framing follows the client `alt` parameter through the shared `copyGeminiDownstream` flow. `?alt=sse` sends `text/event-stream` data-only frames, and omitted `alt` or `alt=json` sends an incremental JSON array. The bridge checks translated JSON in both directions against `max_body_bytes` after translation because escaping can expand a fitting upstream body past the limit. The downstream public model comes from the request `call.name` and route alias. The bridge emits it as Gemini `modelVersion` and never exposes the upstream ID.

### Ordering guards and cache accounting

Logical ordering guards refuse a crossed unfinished block before the order changes instead of buffering a generation. The bridge tracks and resets empty-argument counters, ignores keepalive pings, and rejects updates to closed blocks.

Cache accounting counts cached tokens once and invents no Gemini cache-write bucket. For example, a Gemini prompt count of 12 with a cached share of 4 maps to Anthropic `input_tokens: 8` and `cache_read_input_tokens: 4`, because the cached share is subtracted once. In the reverse direction, the bridge sums Anthropic `input_tokens`, `cache_creation_input_tokens`, and `cache_read_input_tokens` into Gemini `promptTokenCount` and reports only the read share separately. `cache_creation` output aggregates into the prompt total and is never placed in an invented Gemini bucket.

Unknown, billing, thought, and server-tool usage members fail closed with HTTP 502 in both modes, except for the validated lossy Gemini 4 billing and `thoughtsTokenCount` handling in [Gemini 4 response extensions](#gemini-4-response-extensions). Gemini thought parts, opaque signatures outside the lossy `gts1_` wrapper, media, executable code, server tools, grounding and citation payloads, logprobs, and nonzero `toolUsePromptTokenCount` fail closed in both modes.

Header, authentication, and query isolation follow the shared rules in [Header, authentication, and query isolation](#header-authentication-and-query-isolation).
