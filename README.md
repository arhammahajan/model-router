# model-router

A Go gateway for OpenAI Chat Completions, OpenAI Responses, Anthropic
Messages, Gemini, and local llama.cpp models. It serves each of the four
API dialects natively and translates between them where the request and
response have a supported mapping.

The previous Python implementation lives in
[model-router-legacy](https://github.com/arhammahajan/model-router-legacy).

Architecture status: requests use the shared `Generation` IR. Complete
translated responses from all four upstream dialects decode once into `Response`
and use shared typed target encoders. Compatibility wrappers only select the
source decoder and target encoder; replaced pair-owned JSON parsing is removed.
Native passthrough still bypasses translation. All translated streams from all
four upstream dialects use shared framing, source events, pipeline, and reusable
target encoders. Replaced pair-owned source parsing is deleted; compatibility
wrappers select the source framing/decoder and destination encoder only. Gemini
array completion waits for its closing bracket; SSE returns at its source terminal.
Reported initial usage stays distinct from authoritative terminal accounting.
Independent runtime and architecture review is accepted. This does not establish
full feature parity or production readiness.

Toolchain baseline: **Go 1.24** (`go build ./...`). One TOML dependency:
`github.com/pelletier/go-toml/v2 v2.2.4` for config parsing; everything else
is stdlib (`net/http`, `encoding/json`). The Go module lives at the repository root.

## Run

```bash
go build -o model-router-go ./cmd/model-router
./model-router-go --config router-dev.toml   # serve (default command)
./model-router-go --validate-config --config router-dev.toml
./model-router-go --check-readiness --config router-dev.toml
./model-router-go --version
```

Flags `--host`/`--port` override `server.host`/`server.port` (used by the
conformance runner for ephemeral ports). Shutdown: SIGINT/SIGTERM stops new
work, allows a bounded drain (10s), then force-closes remaining connections
and idle upstream pools before exit. Exit codes: 0 success, 1 runtime/bind
failure, 2 config or flag misuse, 3 not ready, 4 offline readiness indeterminate.

Never point this at a config holding real credentials for a quick test: use
`config.example.toml`-shaped fixtures with dummy keys (see conformance).

Configuration accepts TOML and JSON, with integer tokens preserved exactly.
With no `--config`/`-c`, selection checks `router.toml`, `router.json`,
`config.toml`, `config.json`, then YAML filenames. YAML is an explicit migration
error; convert it to TOML/JSON. Unknown filename suffixes try JSON then TOML.
The two config flags share last-value behavior. Explicit empty host/config,
zero or out-of-range ports, and unexpected positional arguments exit 2.

`--env-file` is repeatable and loads before optional default credential files;
existing process values, including present-empty values, win, then the first
file wins. `--no-default-env` disables automatic credential-file loading.
`--api-key` overrides configured ingress keys after trim/dedup and still cannot
be combined with keyless bearer compatibility. Help and version exit without
loading configuration. `--verbose` adds bounded router configuration counts;
it does not enable provider/dependency or request-body logging.

## Endpoints (this build)

| Endpoint | Behavior |
| --- | --- |
| `POST /v1/chat/completions` (+ `/chat/completions`) | Chat ingress: native, or →Anthropic / →Responses / →Gemini (see dispatch) |
| `POST /v1/responses` (+ `/responses`) | Responses ingress: native passthrough with stateful-field passthrough and no conversation store, or →Chat / →Anthropic / →Gemini (`/v1/responses/*` → 404 Responses shape) |
| `POST /v1/messages` (+ `/messages`) | Anthropic ingress: native passthrough, or →Chat / →Responses / →Gemini; other `/v1/messages/*` → 404 Anthropic shape |
| `POST /v1/messages/count_tokens` (+ `/messages/count_tokens`) | Anthropic token-count passthrough (no generation slot; 64 KiB `{input_tokens}` validation; 400 `count_tokens_unsupported` unless declared/first-party) |
| `POST /v1beta/models/{name}:generateContent` / `:streamGenerateContent` (+ `/v1/`, version-less `/models/`, `tunedModels` forms) | Gemini passthrough or →Chat / →Responses / →Anthropic (see dispatch); model+method in the URL, `?alt=sse` only (+ auth-only `?key=`); body `model`/`stream` rejected; unknown methods 404, known-unsupported 501; downstream framing follows the requested `alt` (`text/event-stream` for `alt=sse`, incremental JSON array otherwise); JSON-array and SSE streams preserve upstream content type natively |
| `GET /v1/models` (+ `/models`) | Header-authenticated canonical model list with trusted `metadata.router` and preserved unrelated metadata; aliases/disabled routes omitted; query keys do not authenticate |
| `GET /muse-code/models` (+ `/v1/muse-code/models`) | Header-authenticated Muse projection with typed reasoning variants and display-limit defaults; cached reads only |
| `GET /v1beta/models`, `GET /v1beta/models/{id}` | Gemini discovery: canonical resources, pagination and known limits; unknown limits omitted; headers or `?key=` authenticate |
| `GET /` | Public Go executable name, build version and endpoint index |
| `GET /livez` (+ `/health`, `/healthz`) | `{"status":"ok"}`, no networking |
| `GET /readyz` | 200 when ≥1 enabled named route exists, else 503; dynamic-only routes do not make it ready |
| anything else | 404 in the closest ingress shape (Gemini-like paths use the `google.rpc.Status` shape) |

Ingress dispatch on the resolved route:

- same dialect → **native passthrough**, byte-identical unless the public
  model alias differs from the upstream ID or a confirmed generated llama.cpp
  Chat request carries `reasoning_effort`. The narrow mapper exception below
  consumes that control; mutations preserve `RawMessage` subvalues and exact
  large integers. Gemini decodes `EscapedPath` exactly once (matching
  Python `urlsplit().path` + single `unquote`); raw `/` and `%2F` resolve to
  the same `/` model, literal `%2F` sequences route to the percent alias, and
  traversal/query encodings cannot escape the route. The public name maps to
  the upstream model path segment with traversal/query escaping.
- Chat ingress + `anthropic` route → **translated** (typed IR, strict/lossy
  policy; commitment-aware SSE).
- Anthropic ingress + `openai-chat` route → **translated** (typed IR,
  strict/lossy policy; commitment-aware SSE).
- Chat ingress + `openai-responses` route → **translated** (typed IR,
  Responses-destination loss plan, strict/lossy policy; commitment-aware SSE
  into Chat chunks; `store:false` forced on the wire).
- Responses ingress + `openai-chat` route → **translated** (typed IR,
  Chat-destination loss plan, strict/lossy policy; commitment-aware SSE into
  Responses events; stateful references refused pre-I/O).
- Responses ingress + `anthropic` route → **translated** (typed IR,
  Anthropic-destination loss plan, strict/lossy policy; commitment-aware SSE
  into Responses events; stateful references refused pre-I/O).
- Anthropic ingress + `openai-responses` route → **translated** (typed IR,
  Responses-destination loss plan, strict/lossy policy; commitment-aware SSE
  into Anthropic events).
- Chat ingress + `gemini` route → **translated** (typed IR,
  Gemini-destination loss plan, strict/lossy policy; commitment-aware SSE
  into Chat chunks; stream selection of the SSE vs JSON-array decoder from
  the reviewed upstream `Content-Type`).
- Gemini ingress + `openai-chat` route → **translated** (typed IR,
  Chat-destination loss plan, strict/lossy policy; commitment-aware SSE or
  incremental JSON-array downstream following the requested `alt`).
- Responses ingress + `gemini` route → **translated** (typed IR,
  Gemini-destination loss plan, strict/lossy policy; URL-owned
  `generateContent` / `streamGenerateContent?alt=sse`; upstream Content-Type
  selects the SSE vs JSON-array decoder; Responses `store:false` enforced,
  stateful references refused pre-I/O).
- Gemini ingress + `openai-responses` route → **translated** (typed IR,
  Responses-destination loss plan, strict/lossy policy; model/method from the
  URL, downstream framing follows the requested `alt`: `alt=sse` → SSE,
  otherwise incremental JSON array).
- Anthropic ingress + `gemini` route → **translated** (typed IR,
  Gemini-destination loss plan, strict/lossy policy; URL-owned
  `generateContent` / `streamGenerateContent?alt=sse`; see the
  Anthropic↔Gemini scope below).
- Gemini ingress + `anthropic` route → **translated** (typed IR,
  Anthropic-destination loss plan, strict/lossy policy; downstream framing
  follows the requested `alt`; see the Anthropic↔Gemini scope below).
- any other dialect pair (unknown dialect) → HTTP 400 `unsupported_route`
  in the ingress envelope **before any upstream I/O**.

## Supported configuration subset

`[server]`: `host`, `port`, `auth_mode` (`none`|`required`), `api_keys`,
`allow_dynamic_routes`, `max_body_bytes`, `max_connections`,
`max_generations`, `header_read_timeout`, `body_read_timeout`,
`write_timeout`, `bearer_session_compat` (default `false`; `true` requires no
effective ingress API keys). `MODEL_ROUTER_API_KEY(S)` env fallback mirrors Python.
`auth_mode = "none"` ignores those environment keys; configured keys still
conflict with that mode. `auth_mode = "required"` still requires a key.

`[retry]`: `max_attempts` (1–20), `statuses` (only 429/5xx), `connect_errors`,
`backoff_base`, `backoff_max`, `deadline` (absolute, covering preparation,
attempts, backoff and full stream consumption; the clock starts at handler
entry).

`[providers.*]`: `dialect` (`openai-chat`, `openai-responses`, `anthropic`,
`gemini` — all four execute natively), `base_url`,
`api_key`/`api_key_env`/`api_key_file`, `headers` (trusted static headers),
`timeout`, `compatibility` (`strict`|`lossy`; legacy `drop_params=true`
selects lossy), `auth_header`, `count_tokens` (anthropic only).
`session_header`, `session_source_headers`, `session_prompt_cache_key` and
`session_missing` configure provider-scoped affinity on generation requests.
The configured destination header wins, followed by ordered reviewed native
sources, then the optional body fallback. An explicit empty source list disables
native selection. All reviewed native values still validate, including losing
ones; duplicate or invalid identities return 400 before upstream I/O.
Missing identity is omitted or rejected per policy. Exactly one selected identity
is emitted upstream. With no policy, Go continues to strip native session
headers (Python currently forwards them on native routes).
Native bodies retain `prompt_cache_key`; translated bodies consume it only
when fallback is enabled. Token counting does not perform affinity selection.
Affinity has no session store. With `server.bearer_session_compat = true`, Gemini
generation requests consume `sessionId` and collect exact lowercase UUID4
`router-session-*` tokens from the reviewed key headers or `?key=`. A nonempty
`sessionId` becomes an `agy-acp-*` identity using SHA256 over Python-compatible
`ensure_ascii` JSON string bytes, including lone surrogates. That identity or
token is the final affinity fallback, after the configured header, native
sources, and body fallback. Conflicting distinct tokens return 400 even when
another identity would win. Only the selected identity is emitted, under the
provider's configured session header.

On Chat generation ingress, an exact marked `Authorization: Bearer router-session-*`
is consumed and supplies an `x-session-id` candidate only when no reviewed native
session header is present. Leading plain system messages merge with a blank line;
null, structured, or named content stops merging. Marked bodies validate minimal
message shapes. Unmarked bodies retain the existing native behavior.

On Gemini generation ingress only, the compatibility flag also enables
`x-model-router-reasoning-effort`. The literal must appear in the canonical
model's normalized, trusted `effort.values`; raw `metadata.router` cannot admit it.
Native Gemini maps `none` to budget zero and `minimal|low|medium|high` to uppercase
levels. Other literals or existing body controls return 400. Bridges use the
shared typed planner and provider strict/lossy policy. Anthropic effort has no
reviewed Go wire mapping: strict rejects, lossy drops with the loss header.
Compatibility does not run on reads, token counting, or Responses/Anthropic
ingress. The effort header is always stripped, including when compatibility is
disabled. Provider credentials remain the only upstream credential. See the
[bearer compatibility contract](docs/go-bearer-session-compat-contract.md).

`[[models]]`: `name`, `provider`, `model`, `aliases`, `metadata` (retained
table, served on `GET /v1/models`), `enabled` (boolean, defaults true), and
optional `dialect` (`openai-chat`, `openai-responses`, `anthropic`, or `gemini`).
`enabled = false` reserves the name and aliases while refusing routing with
404, including dynamic-route fallback; disabled entries are omitted from
the model list and readiness count. Route `dialect` selects the actual upstream
protocol; omitted or provider-equal values inherit the provider default. Aliases
share the route destination. Catalog, llama, and dynamic routes inherit their
provider default.

The effective route destination controls dispatch, URL construction, codecs,
native forwarding, translated headers, and session-body consumption. Credentials
remain provider-owned: a Chat-default provider still sends its configured Bearer
credential on an Anthropic route. An override does not confer token-count support;
both the route destination and the provider's counting policy must allow it.
Diagnostics continue to report the configured provider dialect.

Providers also accept `litellm_kwargs.headers` and `extra_headers` for translated
traffic only. Header precedence is case-insensitive: legacy `headers`, then
`extra_headers`, then provider `headers`, then the selected credential/session.
Native forwarding and token counting ignore the legacy maps. All configured
values validate even when overridden; static session identities remain forbidden.
`litellm_provider`, when present, must match the dialect default (`openai` for
both OpenAI dialects, `anthropic`, or `gemini`); it does not load an adapter.

Redundant `litellm_kwargs.api_base` is accepted only when its suffix-expanded
generation URL equals the provider's for its default and every configured route
destination, including disabled routes; `litellm_kwargs.timeout` must equal the
effective provider timeout, including the default. They create no separate
runtime endpoint or timeout. Divergent values fail startup; the declared provider
base remains unchanged for token-count capability checks.

Other legacy kwargs, including credentials, retries, and SDK controls, remain
startup errors with migration hints. Unsupported tables and unknown keys also
fail startup rather than being silently ignored.

`[capabilities.overrides.<canonical-public-name>]` admits sparse validated
capability assertions. `GET /v1/models` renders a canonical `metadata.router`
contract from configured routing facts and these overrides, with source
freshness from the pinned snapshot. Unknown upstream support remains null;
explicit false is distinct. Raw display `metadata.router` is overwritten,
and other metadata retains exact numeric values. Alias keys do not override
their canonical route; absent-name overrides validate for later discovery.
Canonical listing, reviewed catalog acquisition, and observed llama acquisition
are reviewed and verified. Request-effort controls remain separate followups.

`[catalog]` supports `url`, trusted fetch-only `headers`, `timeout` (default 30s),
`refresh_seconds` (default 300s), `model_providers` (exact ID → configured provider),
and ordered `[[catalog.rules]]` (`match`, `provider`). Exact mapping wins over
the first matching rule. Patterns use the reviewed portable Python/Go subset;
unsupported regex constructs fail startup rather than changing routing meaning.
`metadata_source = "opencode-go-v1"` explicitly selects the frozen bundled
review. Exact model ID and dialect must match before reviewed facts apply;
unknown IDs stay unknown and dialect mismatches retain provenance with
`applicable: false`. Operator overrides apply afterward. Review age and TTL
use the fixed bundled observation time, independently of catalog refreshes.
Availability carries its own source and original catalog success timestamp,
including after cache restore. Other non-null source values reject at startup.
Review timestamps render as epoch seconds in Go. This acquisition slice is
reviewed and verified; it does not enable effort controls.

One bounded catalog GET runs before listening; `refresh_seconds = 0` performs
that attempt only. Periodic refresh publishes immutable complete snapshots;
failure retains the previous routes, and a valid empty catalog replaces them.
Explicit names and aliases, including disabled ones, reserve precedence over
catalog routes. Catalog rows supply IDs/metadata, never endpoints or credentials.
Fetches disable redirects, accept identity/gzip only, and cap both encoded and
decoded bodies at 4 MiB. Catalog credentials never enter generation headers.

Readiness and model listing read cached snapshots without network I/O. Catalog
staleness does not remove usable routes. `--validate-config` remains offline;
`--check-readiness` makes one bounded attempt per configured discovery source.
Shutdown closes admission, cancels/joins the
worker, drains active requests within the existing bound, and closes idle pools.
The catalog-only slice is integrated and independently reviewed.

`[llama_cpp]` now supports configured `servers`/`ports`, trusted global and
server headers and credentials, `prefix`, `aliases`, `compatibility` (`strict`
or `lossy`), probe `timeout` (default 2s), and `refresh_seconds` (default 30s).
Llama-only configurations need no static providers. Each server is probed at
`/health`, `/v1/models`, and optional `/props?autoload=false`; every valid model
ID is eligible regardless of load status. Canonical rows precede aliases within
each server; aliases resolve without adding listing rows. Explicit reservations
win over llama routes, which win over catalog routes.

Generated providers use trusted configuration, Chat endpoints, and the existing
600s generation timeout; the probe timeout is separate. Trusted server headers
override global headers case-insensitively and apply to generation too. A first
failed probe does not activate dynamic routing; a successful empty list does.
Later failures retain routes but exclude the unreachable provider from readiness.
Listing preserves row display metadata while replacing `metadata.router` with
trusted reported/inferred observations and operator overrides. Valid architecture
lists, partial input modality flags, template tool/effort reports, sampling
defaults, and reported limits retain their provenance. Empty modality lists
report false; missing or invalid reports stay unknown. Router/swap modes ignore
global model properties; no per-model `/props` fan-out is performed.
Output defaults retain zero or the `unlimited`/`until_context` markers, and
positive `fit-ctx` arguments retain constraint evidence. Failed refreshes retain
the last observations and their original time; restored route-only cache entries
remain unobserved until a successful probe. Server engine identity is distinct
from per-model observations: a live probe identifies Strata from the first
build-info token, otherwise defaults to llama.cpp. Router/swap mode may use that
identity while continuing to ignore global model-property evidence.

Confirmed llama.cpp native Chat requests consume `reasoning_effort`. Recognized
levels default a thinking budget; `none` defaults `enable_thinking=false` inside
`chat_template_kwargs`. Explicit native values win, including null budgets;
null kwargs act as absent, and other non-object kwargs stay untouched. Unknown,
max, null and non-string efforts are consumed without new validation or loss
headers. Strata and ordinary native providers retain raw passthrough.
Responses named effort to known local runtimes uses the same literal mapper
and existing capability policy before I/O: llama.cpp emits budgets/off, Strata
emits the literal effort. Working-mapper facts now advertise their configured
or inferred enum/native shape with distinct provenance, without asserting that
the model reasons or inventing budget support/defaults. Cache-only runtimes
stay unknown: native requests pass through, translated effort stays refused,
and mapper facts are absent until a live probe.
Set `context_fallback` in `[llama_cpp]` to publish a context window when discovery
has no value. If omitted, an unobserved context window stays unknown. Set
`[llama_cpp.limit_overrides.<upstream-model-id>]` to override reported limits for
an exact upstream ID before the router applies a prefix or alias. Listings record
whether each value came from observation, fallback, or override. These settings
affect published capability facts.

Startup and CLI readiness refresh catalog then llama. The fixed pool probes at
most eight llama servers concurrently under whole-probe deadlines; the fleet
bound covers all waves plus scheduling overhead. Both sources publish complete
snapshots without overwriting each other's layer. Shutdown cancels and joins
both workers. The llama slice is integrated and independently reviewed.

Optional `[discovery_cache]` accepts `path` and positive `max_age_seconds`
(default 86400). It stores route IDs, aliases, and original source success times
in a Go-owned v2 format; Python v1 files are ignored as incompatible. Endpoints,
credentials, headers, metadata, and capabilities are excluded. Unsafe IDs are
omitted on save and reject a file on load; destinations are never rewritten.
Files must be regular, owned by the current user, private, and at most 8 MiB.
Writes use a private temporary file and atomic replacement.

Cache restore is one transaction before live probes. Restored catalog routes
count toward readiness; restored llama routes remain stale and unready until
verified. Failed probes preserve original timestamps; empty success replaces
the prior inventory. Restored listing metadata is empty until a live refresh.
`--validate-config` touches neither cache nor network. One background writer
coalesces saves, and startup reads have a 2s handoff deadline. Shutdown performs
a best-effort flush within the existing drain budget; issued filesystem calls
may finish during drain because kernel file I/O cannot be canceled. Hard exit
can leave a private temporary file. Persistence is integrated and independently
reviewed, including concurrent starts, flush ordering, cancellation, and drain.

`GET /diagnostics` reports readiness, providers, catalog/llama refresh state, and
cache outcomes from one pinned snapshot and one clock sample. It performs no
network or filesystem I/O. Without configured router keys it returns
`403 diagnostics_disabled`; otherwise header authentication is required. Bearer
scheme matching is case-insensitive, and any matching presented header credential
authenticates. Query keys are not accepted on diagnostics or the Chat model list.
Reports redact configured secrets and credential-shaped text; they contain no
model metadata, request bodies, headers, or session IDs.

`/readyz` now returns `ready` (200) or `not_ready` (503). Readiness counts effective
named routes including aliases; diagnostic `models` counts public canonicals.
Refresh deadlines are published once per attempt and the scheduler waits that
same deadline; a busy manual attempt wakes its waiting scheduler on completion.

CLI checks print JSON on their final stdout line. `--validate-config` exits 0
without cache or network I/O; config/usage errors exit 2. Online
`--check-readiness` (alias `--check`) exits 0 when ready or 3 when unready.
`--offline --check` skips cache/discovery, reports indeterminate, and exits 4;
`--offline --validate-config` is allowed, while offline serve is a usage error.
Serve/runtime failures retain exit 1. Gemini/Muse model views and the supported
TOML/JSON/env-file CLI paths are integrated; remaining protocol and enrichment
work is tracked in the completion checklist.

## Opt-in search replacement

An explicit search binding allows Responses requests to use SearXNG
on a configured lossy Chat route. The router waits for the upstream model to
request the search, sends one search request, and resumes the same model with
the result. The response reports the substitution as a translation loss and
accounts for both model rounds when search runs.

Configure a backend and bind it to the canonical public model name. The
optional backend `headers` table is separate from the model provider's headers
and credentials.

```toml
[search.backends.internal]
endpoint = "https://search.example.invalid/searxng"
count = 5

[search.bindings."public-search-model"]
tool = "web_search_preview"
backend = "internal"
```

The translated path accepts one `web_search_preview` tool without options.
Use `include: ["web_search_call.action.sources"]` to return the consulted
result URLs. Inline annotations refer to actual result labels in the answer.
For streamed requests, the first model round decides whether to search. When
search runs, the router sends a search output item before streaming the final
answer deltas. The terminal event carries usage for both model rounds and
annotations for verified sources. Additional tools, search filters, and
stateful requests are refused. Native Responses routes forward hosted search
to the provider. [Search contract](docs/go-search-runtime-contract.md)

## Translation scope (Chat ↔ Anthropic)

Typed IR only (`internal/protocol`); no `map[string]any` semantic model.
Covered: ordered text messages (system hoisted, same-role merged), ordinary
JSON-schema function tools (definitions, `none`/`auto`/`any`/named choice,
`parallel_tool_calls`, call/result identity in history and streamed output),
`temperature`, `top_p`, `max_tokens`/`max_completion_tokens`,
`stop`, `stream`, `stream_options.include_usage`, `extra_body.top_k`.

Bridge defaults: Anthropic requires `max_tokens`; an absent cap is sent as
`max_tokens: 4096` (documented, not a loss record). `n: 1` is accepted as an
identity without a record.

Strict (default) refuses any planned loss before upstream I/O with HTTP 400
`translation_loss`. Lossy drops/approximates reviewed mappings and reports
`x-model-router-loss` (`field`/`reason`/`action`, never values or prompt
text) on JSON and SSE responses. Unknown request fields are validation errors
(HTTP 400), never silently accepted; OpenAI `metadata` always requires a
same-protocol route. Response features without a Chat representation
(thinking blocks, server tools, unknown stop reasons/deltas) fail the
translation with HTTP 502 — including in lossy mode. A truncated stream never
produces a success marker.

## Translation scope (Chat ↔ Responses)

Typed IR only (`internal/protocol`); same no-`map[string]any` rule. Covered:
ordered text messages, ordinary function tools (definitions,
`none`/`auto`/`required`/named choice, `parallel_tool_calls`, call/result
identity in history and streamed output), `temperature`, `top_p`,
`max_tokens`/`max_completion_tokens`, `stream`, `stream_options.include_usage`.

Bridge defaults: stateless translation forces `store:false` on the Responses
wire (a source `store:true` is a diagnosed loss, never silently changed);
Responses tools are encoded with explicit `strict:false` to preserve Chat
non-strict semantics. Stateful request references (`previous_response_id`,
`conversation`, `prompt`, `background`) are refused with HTTP 400 before
upstream I/O — the bridge keeps no conversation store. Incomplete Responses
(`incomplete_details.reason: max_output_tokens|content_filter`) map to Chat
`finish_reason` `length`/`content_filter`; cached usage details pass through
under the same scalar validation as the Chat↔Anthropic path.

Streaming commitment mirrors the Anthropic bridges with dialect-native
failure events: lazy commit before the first valid downstream write —
pre-commit invalid/truncated streams are ordinary HTTP 502 (typed deadline
expiry 504), never an empty 200; post-commit keeps HTTP 200 and terminates
without a synthetic success marker. Responses downstream emits a single
`response.failed` event (translator-owned on reviewed paths; the boundary
emits it once when missing, reusing the committed response ID and the next
unique sequence number) and never fabricates `response.completed` or a
duplicate codec error. Chat downstream terminates silently: no `[DONE]`, no
fabricated finish chunk. Only a framed `event:` line counts as an event —
model text containing `event: error` inside a data payload is never treated
as framing. Downstream write/flush failure closes the upstream body
immediately and releases the generation slot; the write deadline resets on
every exit path. No goroutines, no whole-generation buffering, no dry-run
encode.

Bounded retained output (design checkpoint): deltas flush downstream
immediately; only the terminal reconstruction retains output, capped by the
existing bounds (one SSE frame 8 MiB, one tool-argument field 1 MiB, 8 MiB
total accumulated arguments, 1024 tool calls). The Responses terminal repeats
— not duplicates — earlier deltas under these caps; it never buffers an
unbounded generation.

Bounds: request/response bodies at `max_body_bytes`; one SSE frame 8 MiB; one
accumulated tool-argument field 1 MiB; 8 MiB total accumulated tool arguments
per stream (`MaxStreamArgsChars` — not total stream bytes, which are bounded
by `max_body_bytes` for non-streams and the absolute deadline for streams);
1024 tool calls.

## Translation scope (Anthropic ↔ Responses)

Typed IR only (`internal/protocol`); same no-`map[string]any` rule. Covered:
ordered text messages, ordinary function tools (definitions, choice, call
identity in history and streamed output), `temperature`, `top_p`,
`max_tokens`/`max_output_tokens`, `stream`.

Destination-specific loss plans, enforced against the provider compatibility
only (the client cannot select lossy):

- Responses → Anthropic: source `temperature` above 1 is diagnosed and
  dropped (never clamped), so strict policy refuses it; the Responses
  implicit-strict normalization has no Anthropic equivalent (omitted/null is
  diagnosed), explicit `false` is exact, and explicit `true` is conservatively
  refused because Anthropic strict guarantees are not reviewed. Anthropic
  requires `max_tokens`; an absent Responses cap is sent as `max_tokens: 4096`
  (documented, not a loss record). Stateful references
  (`previous_response_id`, `conversation`, `prompt`, `background:true`) are refused
  with HTTP 400 `unsupported_responses_state` before upstream I/O.
- Anthropic → Responses: `top_k` and `stop_sequences` have no reviewed
  Responses mapping (well-formed values are diagnosed losses, never
  forwarded); explicit tool `strict` true/false is carried exactly. Omitted
  or null Anthropic tool strict maps exactly to explicit Responses
  `strict:false`, preserving the source non-strict default without a loss.

Response translation fails closed with HTTP 502 in both strict and lossy
modes for unrepresentable upstream features (thinking blocks, server tools,
citations/annotations, refusal content, logprobs, reasoning accounting,
unknown stop reasons/members, conflicting call or response identities) —
never a silent drop and never a success marker.

Streaming commitment mirrors the Chat bridges with dialect-native failure
events. Toward Responses (Anthropic upstream): deltas flush downstream
immediately and the terminal reconstruction is bounded by the existing caps
(one SSE frame 8 MiB, one tool-argument field 1 MiB, 8 MiB combined retained
text and arguments, 1024 content blocks/tool calls); pre-commit invalid/truncated streams are
ordinary HTTP 502 (typed deadline expiry 504), post-commit keeps HTTP 200
with one `response.failed` carrying the validated tools/tool_choice echo, and
no fabricated `completed` or duplicate error frame. Toward Anthropic
(Responses upstream): prompt accounting arrives only at the terminal, so
`message_start` carries zeros as an explicit placeholder and the terminal
`message_delta` carries the authoritative totals — clients must apply the
final totals, never sum the placeholder. Only a framed `event:` line counts
as an event: model text containing `event: error` inside a data payload is
payload, never framing. Downstream write/flush failure closes the upstream
body immediately and releases the generation slot; the write deadline resets
on every exit path. Deltas are delivered immediately; retained terminal
reconstruction stays within the bounds above. No goroutines or dry-run encode.

## Translation scope (Chat ↔ Gemini)

Typed IR only (`internal/protocol`); same no-`map[string]any` rule. Covered:
ordered text messages (system hoisted, same-role merged), ordinary
JSON-schema function tools (definitions, `none`/`auto`/`any`/named choice,
`parallel_tool_calls`, call/result identity in history and streamed output),
`temperature`, `top_p`, `max_tokens`/`max_completion_tokens`,
`stop` (at most 5 entries — more is a diagnosed loss, never truncated),
`stream`, `stream_options.include_usage`. The Gemini wire uses
`parametersJsonSchema` (full JSON Schema, no OpenAPI-subset rewriting);
`parameters` vs `parametersJsonSchema` together is a 400. Omitted or
explicit-`false` Chat tool `strict` is exact (the Gemini wire carries no
strict member); explicit `true` is a diagnosed loss. `extra_body.top_k` remains
a translation loss, and structured-output controls lack a reviewed mapping.
The reasoning section describes capability-gated effort translation.
`n: 1` is accepted as an
identity without a record. Stateless translation carries no `store` on the
Gemini wire (explicit `false` matches exactly; explicit `true` is a diagnosed
loss). The model and streaming mode ride the upstream URL only
(`generateContent`, or `streamGenerateContent` with `?alt=sse` for streams);
the wire body never carries `model`/`stream`.

Call identity (documented, per request): a supplied Gemini
`functionCall`/`functionResponse` id is preserved exactly when it is a
non-empty string within the size bound; empty or oversize supplied ids are
400 in both modes (no character-grammar rewrite is applied to opaque ids —
the `[A-Za-z0-9_-]{1,64}` grammar constrains function *names* only). Only a
missing id mints a router-local compatibility identity (`gdec-N` on ingress
decode, `g2c-call-n` on the complete response path). The complete path
reserves every supplied id in the request before minting so a minted name
never collides; the stream path skips already-seen ids and fails closed if
a later supplied id collides. The router never invents provider-private signatures or
opaque resource ids.

Request direction notes: Gemini ingress decodes closed camelCase/snake_case
spellings with collision refusal; unknown nested members and router-held
state (`cachedContent`, `continuationToken`) are refused pre-I/O.
Gemini user `inlineData` preserves part order and the original bytes in standard
padded base64. PNG, JPEG, WebP, and GIF become image inputs on Chat, Responses,
and Anthropic. WAV and MP3 become `input_audio` on Chat and produce a loss on
the other translated routes. Image and audio display names produce a loss.

Inline PDFs become file inputs on Chat and Responses and document inputs on
Anthropic. A safe PDF display name becomes the filename or document title.
An unsafe name produces a loss. OpenAI file inputs use `document.pdf` without a
safe name, and Anthropic omits the title.
[OpenAI file inputs](https://developers.openai.com/api/docs/guides/file-inputs), [Anthropic PDF documents](https://platform.claude.com/docs/en/build-with-claude/citations#pdf-documents)

Chat user `image_url` data URLs use the same four image formats when translating
to Gemini. Chat `detail` values other than `auto` produce a separate loss.
Remote image URLs produce a loss without a fetch.
`FunctionResponse.response` is a required object; Chat tool results map to it
as the exact string form `response.output` (never parsed into structured
fields). `FunctionResponsePart` accepts only `inlineData`
(`FunctionResponseBlob`) or `fileData` (`FunctionResponseFileData`) with
exactly one payload, plus the closed optional enum/string members;
anything else is malformed 400 in both modes. The same malformed-vs-loss
rule holds for part metadata: enum-typed members outside their contract
(`scheduling` and media enums), malformed MIME-type fields and string-vs-part confusions
(a string where a Part object is required) are 400 in both modes, never
loss records. Gemini response thought parts/signatures, opaque signatures,
media, executable code, server tools, grounding/citation payloads, logprobs
and nonzero upstream `thoughtsTokenCount`/`toolUsePromptTokenCount` fail
closed with 502 in both modes. The forward direction does carry Chat
`reasoning_tokens` scalars into `thoughtsTokenCount` (scalar mapping only;
no thought controls are claimed). Gemini `usageMetadata`
`promptTokenCount` already includes the cached share (never double-counted).

Response terminal rules: every terminal — JSON, SSE, and JSON-array — must
carry its own usage; a finish without terminal usage is 502, never an
underreported total. SSE returns promptly at the finish chunk without waiting
for EOF; the JSON array requires its closing `]` with no trailing comma and
returns without waiting for EOF. Chat downstream emits usage as a separate empty-choices chunk
when `include_usage` is true (`"usage": null` on ordinary chunks); a terminal
without usage fails instead of underreporting. Genuinely model-malformed
Chat tool arguments map to a Gemini 200 with `MALFORMED_FUNCTION_CALL` as
the native finish marker (per the documented finish contract) — never an
ordinary `STOP`, never an invalid function rendering. Parsing, envelope, and
bound failures (oversize frame/argument/aggregate, truncated or malformed
envelopes) remain 502 or post-commit aborts.

Streaming commitment mirrors the other bridges. Toward Chat (Gemini
upstream): the framing decoder is selected from the reviewed upstream
`Content-Type` (`text/event-stream` vs `application/json` array, never body
sniffing); anything else is a pre-commit 502. Downstream is Chat SSE through
the shared lazy Chat committer: silent post-commit abort, no `[DONE]`, no
error frame, exact public model, typed pre-commit 504 vs invalid 502. Toward
Gemini (Chat upstream): downstream framing follows the requested `alt`
(`?alt=sse` → `text/event-stream` data-only frames; missing `alt` or
`alt=json` → incremental JSON array, `application/json`). Pre-commit errors
are ordinary Gemini `google.rpc.Status` HTTP 502/504; post-commit SSE emits
one sanitized `data: {error: google.rpc.Status}` frame (no `[DONE]`, no
invented STOP/usage), while an array failure ends without the closing `]`
(no repair/retry). Only a framed `event:` line counts as an event: model
text containing `event: error` inside a data payload is payload, never
framing. Terminal rules are framing-distinct (SSE ends at the finish chunk;
the array ends at the finish element plus `]`); a valid terminal returns
promptly without requiring EOF, the upstream body is closed on every exit
path, the write deadline resets (including the fallback error path), the
slot is reclaimed, and no raw write is attempted after the downstream
failed. Translated JSON bodies in both directions are additionally bounded by
`max_body_bytes` after translation (escaping can expand a fitting upstream
body past the bound; oversize translations fail closed pre-commit with a
sanitized ingress-native 502). The JSON downstream body stays within the
existing bounds too (MaxFrameBytes per frame/element, 1 MiB per argument
field, 8 MiB aggregate, 1024 calls). No goroutines, no dry-run encode.

## Translation scope (Responses ↔ Gemini)

Typed IR only (`internal/protocol`); same no-`map[string]any` rule. Response
translators run from the upstream dialect back to the client:

| Client ingress | Upstream | JSON response translator |
| --- | --- | --- |
| Responses | Gemini | `TranslateGeminiJSONToResponses` |
| Gemini | Responses | `TranslateResponsesJSONToGemini` |

Covered: ordered text messages, ordinary function
tools (definitions, `none`/`auto`/`required`/named choice, parallel-call
identity in history and streamed output), `temperature`, `top_p`,
`max_output_tokens`/`maxOutputTokens`, `stream`. Destination loss plans are
enforced against the provider compatibility only (the client cannot select
lossy); strict refuses any planned loss pre-I/O with 400, before any
upstream request.

Request rules: Gemini ingress forces `store:false` on the Responses upstream
wire. Responses ingress sends no `store` field to Gemini (explicit source
`store:true` is a diagnosed loss); Responses stateful references
(`previous_response_id`, `conversation`, `prompt`, `background:true`) are
refused pre-I/O — no conversation store. Gemini source `stopSequences`
shape is validated (array of non-empty strings, malformed 400 in both
modes), then diagnosed as a dropped loss for the Responses destination,
which has no stop-sequence contract. Gemini model/method ride the upstream
URL only (`generateContent`, or `streamGenerateContent` with `?alt=sse`);
the wire body never carries `model`/`stream`. Gemini upstream Content-Type
selects the framed decoder (`text/event-stream` vs `application/json`
array, never body sniffing); anything else is a pre-commit 502. Gemini
downstream framing follows the requested `alt` (`?alt=sse` → SSE data-only
frames; missing/`alt=json` → incremental JSON array).

Chat ingress to a Responses destination carries `reasoning_effort` as
`reasoning.effort`; Responses ingress to an ordinary Chat destination carries
the reverse through the same typed request IR and effort planner. Exact mappings
preserve the literal and add no loss header. Planning uses the resolved route's private
canonical capability contract from the same pinned snapshot: unknown facts
are neutral, while explicit unsupported facts or known enum mismatches become
translation losses (strict rejects before I/O; lossy drops and reports).
When `reasoning.supported=false`, requesting `none` removes the control exactly.
Responses summaries and unknown-runtime generated providers retain their pre-I/O
reasoning refusal in both modes. Other reasoning controls, destinations and
private response-reasoning refusals retain their existing policies.

Anthropic `thinking: {type: "disabled"}` now carries the same typed disable
intent to Chat and Responses. Unknown capabilities emit literal `none`; an
explicit unsupported reasoning capability strips it exactly, while an
unsupported toggle or effort enum becomes a strict/lossy loss with origin
`thinking`. Confirmed llama.cpp Chat routes use their disable toggle.
Gemini-bound disable requests still produce a translation loss.

Anthropic `thinking: {type: "enabled", budget_tokens: N}` carries an explicit
budget to confirmed llama.cpp Chat routes. Gemini accepts it when the pinned
capability declares `reasoning.budget.supported = true` with native parameter
`{protocol = "gemini", path = "generationConfig.thinkingConfig.thinkingBudget"}`.
Both enforce known budget bounds. Ordinary Chat and Gemini routes without that
capability diagnose a dropped budget. An explicit valid `display` on enabled
thinking is a separate loss. Responses refuses enabled thinking before I/O.
Adaptive thinking remains unavailable on translated routes, and native
Anthropic continues to forward the original body.

Chat `reasoning_effort` and Responses `reasoning.effort` translate to
Gemini `thinkingLevel` when the pinned capability declares support, the
matching Gemini native parameter, and an enum that includes the requested
level. Unreviewed or excluded levels produce a translation loss.

Gemini generation ingress now carries its reviewed thinking levels, disable
request, and explicit budget through the same typed planner for Chat/Responses
destinations. Budget `-1` and `THINKING_LEVEL_UNSPECIFIED` mean no explicit
control; budget `0` disables. Explicit positive budgets map exactly only to
confirmed llama.cpp Chat targets and otherwise become diagnosed losses.
Named levels retain effort gates and the existing llama literal budget map;
derived budgets do not inherit explicit-budget bounds. `includeThoughts`
keeps its independent loss policy. Malformed controls fail before policy in
both modes, including `XHIGH`, which is outside the translated source grammar.
Native Gemini remains raw passthrough.

History: a Gemini `FunctionResponse.response` object maps to a Responses
`function_call_output` whose `output` keeps the entire object serialized
as a compact JSON string (never unwrapping an `output` member, never
parsing a JSON-looking inner string). The reverse keeps a Responses output
string verbatim in `response.output`. IDs are opaque bounded strings,
preserved exactly; missing ids mint router-local compat ids
(`g2r-call-N` scope; stream path skips seen ids and fails closed on a late
supplied-id collision). A legacy minimal `response.created` envelope with
only an id is accepted; any additional supplied members are validated.
Ordered parts: Gemini→Responses
emits `text → call → text` in response-part order in JSON and streams;
Responses→Gemini holds pending complete calls until all lower output items
close and emits in logical `output_index` order — higher text crossing an
unfinished lower item is refused before the order changes (no whole-
generation buffer); duplicate terminal text is fixed. Stream terminals
check `id`/`status`/`kind`/`args`/hash consistency.

Reasoning/cache: nonzero Gemini `thoughtsTokenCount`/`toolUsePromptTokenCount`
has no Responses representation and fails closed (502) in both modes.
Responses `reasoning_tokens` scalar maps to Gemini `thoughtsTokenCount`;
cached `cached_tokens` maps to `cachedContentTokenCount` inside the prompt
total (never double-counted); totals are exact (`total == input+output`).
Responses OUTPUT `reasoning` items remain unmapped and are refused. The
top-level Responses `reasoning` request echo is validated by the shared
helper for Chat/Anthropic/Gemini destinations (enum `summary` /
`generate_summary` / `context` / `effort`, plus string `mode` per the
Responses create reference); it never executes reasoning or claims
chain-of-thought controls.

Streaming commitment mirrors the other bridges with dialect-native
failures: lazy commit, sticky flush failure (close upstream, cancel,
reclaim slot, no goroutines), deadline 504 vs invalid/truncated 502
pre-commit. Post-commit Responses downstream emits exactly one framed
`response.failed` (translator-owned where reviewed; boundary emits once
when missing, reusing the committed id and next sequence; never a
fabricated `completed` or duplicate codec error). Post-commit Gemini SSE
emits one sanitized `data: {error}` frame (no `[DONE]`, no invented STOP or
usage); the Gemini JSON array ends without the closing `]` (no repair).
A valid terminal returns promptly without EOF; a Gemini array requires its
closing `]` (trailing comma rejected). Only a framed `event:` line counts
as an event. Translated JSON in both directions is bounded by
`max_body_bytes` after translation; Responses downstream reuses the
validated typed tools/tool_choice echo after enrichment within bounds.

Gemini-source `stopSequences` accepts at most five entries. Larger lists
are malformed requests (400 in both modes), before destination loss planning.

## Translation scope (Anthropic ↔ Gemini)

Typed IR only (`internal/protocol`); same no-`map[string]any` rule. Claimed
scope is ordinary text plus ordinary JSON-schema function tools only: no
media/reasoning variants, no tool/reasoning expansion, unknown source blocks are
400 in both modes. Response translators run from the upstream dialect back
to the client; the names follow the upstream source, not the ingress:

| Client ingress | Upstream | JSON response translator |
| --- | --- | --- |
| Anthropic | Gemini | `TranslateGeminiJSONToAnthropic` |
| Gemini | Anthropic | `TranslateAnthropicJSONToGemini` |

Covered: ordered text messages, ordinary function tools (definitions,
`none`/`auto`/`any`/named choice, call/result identity in history and
streamed output), `temperature`, `top_p`, `max_tokens`/`maxOutputTokens`,
`stream`. Destination loss plans are enforced against the provider
compatibility only (the client cannot select lossy). Gemini-source
`stopSequences` shape is validated first (array of non-empty strings,
malformed 400 in both modes; more than five entries is 400 in both modes
before loss planning), then maps exactly to Anthropic `stop_sequences`.
Anthropic-source stop sequences map exactly to Gemini within its five-entry
cap; larger source lists are diagnosed as a loss. There are no provider
version floors. IDs are shared opaque bounded strings preserved exactly;
function *names* keep the `[A-Za-z0-9_-]{1,64}` grammar while opaque call
ids carry no character-grammar rewrite.

Request/response rules: the Gemini model and streaming method ride the
upstream URL only (`generateContent`, or `streamGenerateContent` with
`?alt=sse` for upstream streams); the wire body never carries
`model`/`stream`. Upstream Gemini streams always request `alt=sse`, and the
framed decoder is selected from the actual upstream `Content-Type`
(`text/event-stream` SSE vs `application/json` array, never body
sniffing). Downstream Gemini framing follows the client `alt` through the
shared `copyGeminiDownstream` flow (`?alt=sse` → `text/event-stream`
data-only frames; missing/`alt=json` → incremental JSON array).
Translated JSON in both directions is bounded by `max_body_bytes` after
translation (escaping can expand a fitting upstream body past the bound).
The downstream public model comes from the request `call.name`/route alias
(Gemini `modelVersion`), never the upstream id.

Streaming commitment mirrors the other bridges: typed early 504 (deadline)
vs 502 (invalid), lazy commit, sticky flush failure (close upstream,
cancel, reclaim slot, no goroutines). Post-commit Anthropic downstream
emits a single framed error with no `message_stop`; post-commit Gemini SSE
emits a single `data: {error: google.rpc.Status}` frame with no synthetic
finish or usage, and the Gemini JSON array ends without the closing `]`
(no repair). A valid terminal returns promptly without requiring EOF,
except a JSON array still requires its closing `]` (trailing comma
rejected). Logical ordering guards refuse a crossed unfinished block
before the order changes rather than buffering a generation; empty-args
counting and reset/ping/closed-block handling are complete.

Cache accounting is exactly once with no invented Gemini write bucket:
Gemini prompt 12 with cached share 4 maps to Anthropic `input_tokens:8`
and `cache_read_input_tokens:4`; the cached share is subtracted once.
The reverse maps Anthropic `input_tokens` plus
`cache_creation_input_tokens` plus `cache_read_input_tokens` into Gemini
`promptTokenCount`, reporting only the read share separately.
`cache_creation` output aggregates into the prompt and is never placed in an
invented Gemini bucket. Unknown, billing, thought, and server-tool usage
members fail closed (502) in both modes. Gemini thought parts/signatures,
opaque signatures, media, executable code, server tools, grounding/citation
payloads, logprobs, and nonzero `thoughtsTokenCount`/`toolUsePromptTokenCount`
fail closed in both modes.

Header/auth/query isolation matches the other bridges: client credentials
are never forwarded (Gemini `?key=` is auth-only and stripped; only
`?alt=sse` is ever forwarded upstream); bridged requests forward only
destination-dialect opt-ins; Connection-nominated, session, idempotency,
auth, cookie, and quota/billing headers are dropped; provider statics
override inbound opt-ins and auth wins last.

## Transport and security posture

- Client auth: `Authorization: Bearer`, `x-api-key`, `api-key`,
  `x-goog-api-key` (constant-time compare) or `auth_mode = "none"`, plus
  Gemini-only `?key=` query auth. Client credentials are never forwarded
  (query keys are auth-only and stripped; only `?alt=sse` is ever forwarded).
- Upstream credential is always the provider config (stripped); empty keys
  send the `model-router-no-auth` placeholder. Anthropic default
  `x-api-key`, Gemini default `x-goog-api-key`, OpenAI default Bearer;
  `auth_header` overrides. All other
  credential header names are scrubbed before the selected credential is
  applied, so provider statics cannot leave a second credential.
- Native requests forward reviewed inbound headers (Gemini: none are
  destination opt-ins, so all reviewed inbound headers pass); bridged
  requests forward only destination-dialect opt-ins (`anthropic-version` /
  `anthropic-beta` toward Anthropic; `openai-beta` toward Chat or Responses).
  In all paths Connection-nominated headers, session
  identities other than the configured validated selection, idempotency keys, auth,
  cookies, quota/billing attribution and representation headers are dropped.
  Provider statics override inbound opt-ins; auth wins last; framing
  (`Content-Type: application/json`, `Accept-Encoding: identity`) is enforced.
  Request `GetBody` is cleared so the HTTP layer cannot hidden-replay a
  generation outside the explicit retry policy.
- Upstream timeouts: per-attempt work is bounded by `providers.*.timeout`
  clamped to the remaining absolute deadline; context/deadline expiry surfaces
  as HTTP 504 precommit and is never retried. Stream request contexts live
  until the body closes; pool cancellation is isolated on a dedicated
  transport with environment proxy selection preserved.
- Redirects are refused (never followed). One attempt by default; configured
  statuses and pre-send connection failures retry with jittered backoff and
  `Retry-After` (minimum) inside the absolute deadline. Anything ambiguous —
  timeouts, truncated responses (including truncated error bodies), started
  output — is never replayed. Exhausted retries preserve status/body and
  `Retry-After`/`RateLimit-*` hints; unaffordable `Retry-After` is a 504.
  Upstream `Content-Encoding` is decoded bounded (`gzip`) or refused
  (`br` et al.) before commit; encoded bytes are never forwarded with the
  encoding stripped.
- Admission: `max_connections` is enforced at two distinct layers with
  separate semaphores: raw-socket `LimitListener` admission at accept time
  (HTTP/1 scope: each accepted TCP socket holds one slot until
  Close, including idle keep-alive and partial-header connections; overload
  never queues — the extra socket gets a best-effort synchronous minimal
  503 with `Retry-After: 1` and is closed in the accept loop, write bounded by
  500ms so a slow peer cannot stall accepts; no per-rejection goroutine) plus
  the handler-level `conns` bound after headers (ingress-aware 503 envelope)
  and the `gens` generation bound (no queue; overload is an immediate HTTP
  503 `overloaded`). HTTP server sets `ReadHeaderTimeout`, `IdleTimeout`
  (60s) and `MaxHeaderBytes` (1MiB); body and downstream write deadlines are
  per-request and reset for keep-alive reuse. Downstream flush failures
  cancel upstream work and terminate the stream without a success marker.
- Upstream error statuses are preserved downstream (native: status+body;
  translated: status with a stable sanitized message — raw upstream bodies
  are never copied into translated errors). Retry/rate-limit hints are the
  only upstream headers forwarded on errors. Structured logs carry
  route/dialect/status/duration only — no content, sessions or secrets.

## Intentional behavior changes vs the Python baseline

1. Truncated translated streams terminate **without** `finish_reason` or
   `[DONE]`; the Python router fabricates `finish_reason: stop` + `[DONE]`
   (verified against Python 0.3.0; the design forbids success markers).
2. Translated responses carry the **public model name** as the downstream
   `model`/`modelVersion`; Python leaks the upstream model ID.
3. Native same-protocol bodies are forwarded **byte-identical** when no alias
   rewrite or confirmed llama.cpp effort mutation is needed; Python re-serializes JSON.
4. Native upstream errors keep their **status and bytes** (Python: sanitized
   envelopes in some paths, e.g. Gemini/count errors are redacted there vs
   Go's opaque native preservation; count_tokens errors stay sanitized in
   both, with only retry hints forwarded).
5. Gemini `?key=` authenticates but is never forwarded; any other client
   query besides `alt=sse|json` is a 400 before upstream I/O (Python ignores
   unknown Gemini query parameters).
5. Transport errors after dispatch are never retried; only refused
   connections/DNS failures (pre-send) and configured statuses replay.
6. Midstream translated-SSE failures keep HTTP 200 and terminate without a
   synthetic success marker: the Python router fabricates
   `response.completed` (Responses downstream) or `data: [DONE]` (Chat
   downstream) after truncation (verified against the Python baseline; the
   design forbids success markers). Go emits one `response.failed` for the
   Responses downstream instead, and stays silent for the Chat downstream.
7. Translated-stream failures are sanitized HTTP 502 (typed deadline expiry
   504): the Python router surfaces raw LiteLLM exceptions with HTTP 500,
   including tracebacks that echo upstream secrets (verified: audio payload
   bytes). Go never copies raw upstream bodies into translated errors.
8. Case fixtures that changed during review pin the wire-commitment and
   malformed-script behavior, not new product features: Gemini-tool-result
   cases pin the exact string `response.output` form, and malformed-argument
   cases pin the 200 + `MALFORMED_FUNCTION_CALL` finish for genuinely
   model-malformed args vs 502 for envelope/bound/truncation failures.

## Limits remaining (not full milestones 1–4)

- Raw listener admission is implemented (HTTP/1 socket scope only; it does
  not cap per-stream multiplexing). Its 503 is an empty minimal reply sent
  before the endpoint is known, so it carries no ingress envelope; the
  handler-level 503 selects the Anthropic / Gemini / OpenAI envelope by path.
  The synchronous 500ms bounded write is the documented limitation (best
  effort, not a full non-blocking contract).
- Usage accounting is implemented: ordinary tool JSON numbers stay exact (no
  float64 round-trip); standard Anthropic `usage` input/output/cache scalars
  are validated with cumulative accounting (terminal `message_delta` output
  count authoritative); non-null unmapped usage members are refused. The
  server boundary already cancels upstream work and terminates without a
  success marker on downstream flush failure.
- Streaming usage limitation: Chat reports prompt tokens only in the terminal
  usage chunk (OpenAI sends `"usage": null` on every other chunk, and the
  chunk may never arrive on interruption). Only the Anthropic-ingress reverse
  path (Chat upstream → Anthropic downstream) emits
  `message_start.usage: {input_tokens: 0, output_tokens: 0}` as an explicit
  placeholder, because input accounting is unknown until the terminal Chat
  usage chunk; it streams incrementally without buffering the whole
  generation, and the corrected terminal `message_delta` carries the
  authoritative cumulative totals (only accumulators that apply the delta
  totals are covered). The forward path (Chat ingress ← Anthropic upstream)
  instead emits a Chat role chunk at start and carries the provider's actual
  `message_start` input accounting forward. Either direction fails a stream
  ending without terminal usage instead of emitting an underreported total.
  Valid finish + terminal usage + `[DONE]` ends the reverse stream without
  waiting for EOF; trailing bytes in the same response body are ignored and
  the upstream body is closed for resource cleanup. Only the claimed
  ordinary text/function-tools scope is covered; no full-feature-matrix,
  M4/M5/M6, or production validation is claimed.

## Not implemented (later milestones)

Native four paths plus the twelve-bridge Chat↔Anthropic / Chat↔Responses /
Anthropic↔Responses / Chat↔Gemini / Responses↔Gemini / Anthropic↔Gemini
matrix are routed for the claimed ordinary text/function-tools scope;
count_tokens passthrough and raw-listener admission are implemented.
Remaining: milestones 4–6 (operational parity, tool expansion, cutover).
Python remains the production baseline. Bridges cover ordinary JSON-schema
function tools. Chat custom tools use native freeform fields on Responses
upstreams. Chat and Responses custom tools use a single string-valued function
on other supported upstreams, and strict mode rejects that approximation
before dispatch. Lossy mode reports the approximation and carries any grammar
as instructions. Responses namespace names are restored on returned calls.
Private reasoning output, media variants, and provider-opaque signatures
remain outside the translated response scope. Supplied call IDs
are preserved, and translated responses carry the public model name as the
downstream model identifier. Only the reviewed paths are covered; no full
feature-matrix or production validation is claimed. Reviewed request effort controls and keyless bearer
session compatibility are integrated. Further discovery enrichment, reasoning
and media/tool variants, search replacement, the harness-native tool matrix,
and full CLI parity remain deferred. The M4.1 static snapshot, disabled-reserved routes, model-list auth,
and usable readiness rule are integrated.
Snapshots copy their inputs and returned route data; all ingress uses the
same resolver. Fifteen independent external M4.1 contracts pass.
Authenticated diagnostics preserve the Python schema for the admitted subset.
Catalog refresh, immutable publication, and reviewed llama discovery are integrated.
Provider session affinity is integrated;
All 22 independent affinity contracts pass. CLI validation/readiness/offline
checks use the reviewed 0/2/3/4 exit contract; supported CLI/env-file flags are documented above.
`count_tokens = true` on non-Anthropic dialects stays a
startup rejection; undeclared `count_tokens` on an Anthropic-compatible
provider whose base is not exactly `https://api.anthropic.com` stays 400
(no local estimate). Explicit `count_tokens = true` (any Anthropic base) or
exact first-party automatic detection is supported; explicit `false`
disables. Requests needing these get explicit
400/404/501-class errors, never silent approximation.

## Verification

```bash
go build -o /tmp/model-router-go ./cmd/model-router
go vet ./... && gofmt -l internal cmd                    # must be empty
go test -count=1 ./...                                   # unit + fake-upstream
go test -race -count=1 ./...                             # concurrency checks
python3 conformance/run.py --backend go --go-bin /tmp/model-router-go  # runner reports counts
python3 conformance/test_run.py                          # 78 runner tests
```

Development archives are built separately from deployment:

```bash
python3 scripts/build_release.py --go go --version 0.1.0-dev --out dist/0.1.0-dev
python3 scripts/check_release.py dist/0.1.0-dev/model-router-go-0.1.0-dev-linux-amd64.tar.gz --version 0.1.0-dev
```

The builder uses cached dependencies only and emits deterministic tarballs,
`SHA256SUMS`, and a source/toolchain manifest for Linux/macOS amd64/arm64.
Select `--target OS/ARCH` to build one target. Use a separate output directory
for each version and target set; mixed outputs are rejected rather than silently
leaving stale archives. Run the smoke checker with the
archive matching your host; it extracts and executes the packaged binary with
an isolated HOME/XDG and dummy config. The Linux amd64 archive passed this check
locally, and repeated builds produced identical archive bytes. Other targets
cross-compile; their runtime smoke checks are defined in CI but have not run
locally. Windows is outside the current Unix-cache platform contract.
See [installation and rollback](RELEASE.md) and the current
[completion checklist](tasks/rewrite-completion.md).

The runner reports 616 passing external cases and remains the source of the
count. This includes 41 keyless bearer-compatibility contracts. Root regressions
cover marked-Chat shape/null boundaries, exact surrogate-preserving system
merges, and streamed canonical session hashing. Independent compatibility
runtime review found no reachable issues; its regression tests pass under race
detection. The suite includes native
and translated protocol contracts, static registry, affinity, response/event IR,
legacy provider headers/aliases, catalog routing, llama discovery, diagnostics,
explicit route dialect overrides, and canonical capability listing/overrides.
Independent route override review found no
implementation issues; its adversarial regression tests pass with race detection.
Operational read-path review findings are corrected and regression-tested.
It also passes 78 runner
unit tests, fresh Go race tests, build, vet and formatting checks. The final
30 Anthropic↔Gemini contracts are integrated and routed (commits 223d3a9+ff183b2,
corrected codecs 5a5c975, contracts 2e8f5e3+968143b+cdbfdd3). Reviewed coverage
includes pre-commit 502/504, flush-failure cancellation, prompt terminal
return with slot recovery, post-commit failure framing, model-text framing
guards, and validated Responses `tools`/`tool_choice` echoes. Empty annotation
arrays are supported; scalar reasoning-token accounting maps Responses
`reasoning_tokens` to Gemini `thoughtsTokenCount` (reverse nonzero thought /
tool-use refuses), Responses→Chat carries the scalar, while
Responses→Anthropic refuses nonzero reasoning usage. Final-pair cache
accounting is exactly once with no invented Gemini write bucket (see the
Anthropic↔Gemini scope). Fixture corrections uphold text-before-tool order
and ingress-dialect response shapes; a new optional zero-usage omission is
not a bug.
Translated stateful requests retain `unsupported_responses_state`.
Tool-result content is checked for exact preservation across the reviewed bridges.

Recorded Python baseline divergences (Go is correct per the design, see
“Intentional behavior changes” 6–7): `chat_to_responses_text` — Python
hoists the system message into top-level `instructions` with a user-only
`input`, while Go sends the system message as the first input item; both
are documented valid Responses input wire forms (easy input-message `type`
optional), so the case asserts Go's ordered system0/user1 form and the
Python mismatch is an equivalent wire-form difference, not a product bug.
Also recorded: both `*_sse_truncated` cases (Python fabricates `completed` /
`[DONE]` after truncation) and the three tightened pre-commit 502 cases
(Python returns raw-exception 500s, one echoing a traceback + upstream
secret). Tool-result regression tests pin exact content preservation,
including empty and Unicode strings; no duplication is accepted as correct.

Python remains the production baseline; the Go conformance set documents
review fixes (large-integer preservation, header isolation, gzip handling,
sanitized errors, retry exhaustion, unaffordable Retry-After). Python
validation/translation/retry/count_tokens were parent-verified with
`env -u MODEL_ROUTER_API_KEY -u MODEL_ROUTER_API_KEYS`; no live-router
credential or log claims are made here.

Conformance cases (`conformance/cases/*.json`) assert upstream wire requests
and client-visible output. To compare against Python, place the legacy checkout
next to this repository or set `MODEL_ROUTER_LEGACY_REPO` to its path.
Use `python3 conformance/run.py --backend go --list` to list the cases.
