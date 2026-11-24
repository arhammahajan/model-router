# Transport, security, and scope

## Authentication and headers

Client authentication accepts `Authorization: Bearer`, `x-api-key`, `api-key`, and `x-goog-api-key` headers, or `auth_mode = "none"`. The router compares client credentials in constant time. On Gemini endpoints only, the router also accepts `?key=` query authentication.

The router never forwards client credentials to upstream providers. On Gemini endpoints, the router uses `?key=` parameters for authentication only and strips them before dispatch. The router forwards only `?alt=sse` to upstream Gemini endpoints.

Upstream requests always use the credential configured for the provider, with surrounding whitespace stripped. When a provider key is empty, the router sends the `router-no-auth` placeholder. When `auth_header` is omitted, the default upstream credential header follows the effective egress dialect (the destination dialect when natively supported by the provider, otherwise the provider's default dialect): `x-api-key` for Anthropic, `x-goog-api-key` for Gemini, and `Authorization: Bearer` for OpenAI. Setting `auth_header` on any `[providers.<name>]` table overrides the default header name explicitly. Before applying the selected credential, the router scrubs all other credential header names so static provider headers cannot leave a second credential on the outbound request.

Header forwarding depends on whether the route is native or bridged. Native requests forward all reviewed inbound headers, including on Gemini native routes. Bridged requests forward only destination-dialect opt-in headers. The router forwards `anthropic-version` and `anthropic-beta` toward Anthropic upstreams, `openai-beta` toward Chat or Responses upstreams, and no opt-in headers toward Gemini upstreams.

In all paths, the router drops the following inbound headers:

- Headers nominated by the `Connection` header
- Session identities other than the configured and validated selection
- Idempotency keys
- Authentication headers
- Cookies
- Quota and billing attribution headers
- Representation headers

Static provider headers override inbound opt-in headers, and the selected upstream credential takes final precedence. In the default CLIProxyAPI setup, the only credential in the router TOML is the proxy access key (`[providers.cliproxyapi] api_key`); upstream AI/OAuth credentials live only in the CLIProxyAPI YAML file and environment. The router enforces `Content-Type: application/json` and `Accept-Encoding: identity` on outbound requests. The router also clears `GetBody` on outbound requests so the HTTP transport layer cannot silently replay a generation outside the explicit retry policy.

## Timeouts, retries, and encoding

The router bounds per-attempt upstream generation work by `providers.*.timeout` (default `600s`), clamped to the remaining absolute request deadline. Discovery probes for `[providers.llama_cpp]`, `[providers.strata]`, and `[providers.cliproxyapi]` use a separate `probe_timeout` (default `2.0s` for `llama_cpp`/`strata`, `30.0s` for `cliproxyapi`). If context cancellation or deadline expiry occurs before response commit, the router returns HTTP 504 and does not retry. Streaming request contexts remain active until the response body closes. The router isolates stream connection pool cancellation on a dedicated transport that preserves environment proxy selection.

The router refuses HTTP redirects and never follows them. By default, the router sends one upstream attempt. Configured retryable HTTP statuses and pre-send connection failures retry with jittered backoff within the remaining absolute deadline. When an upstream response includes a `Retry-After` header, the router treats that value as the minimum wait before retrying.

The router never replays ambiguous failures. Ambiguous failures include timeouts, truncated responses, truncated error bodies, and any failure after output has started. When retries are exhausted, the router preserves the upstream status code, the `Retry-After` and `RateLimit-*` hint headers, and on native paths the upstream response body. If a `Retry-After` wait cannot fit within the remaining absolute deadline, the router returns HTTP 504 instead of retrying.

Before committing a response, the router decodes upstream `Content-Encoding: gzip` within bounded limits and refuses `br` and other encodings. The router never forwards compressed bytes with the `Content-Encoding` header stripped.

## Admission control and server bounds

The router enforces `max_connections` at two distinct layers with separate semaphores:

- Raw-socket `LimitListener` admission runs at accept time at HTTP/1 socket scope. Each accepted TCP socket holds one slot until `Close`, including idle keep-alive sockets and connections with partial headers. Overload never queues. When the limit is full, the accept loop writes a best-effort synchronous HTTP 503 response with `Retry-After: 1` and closes the socket inline without spawning a goroutine. It bounds the rejection write to 500ms so a slow peer cannot stall subsequent accepts.
- The handler-level `conns` bound runs after the server reads request headers. When requests exceed this bound, the router returns an ingress-aware HTTP 503 error envelope.

The router also enforces the `max_generations` concurrency bound, tracked internally as `gens`. Generation admission does not queue, and overload immediately returns HTTP 503 with `overloaded`.

The HTTP server sets `ReadHeaderTimeout`, sets `IdleTimeout` to 60s, and sets `MaxHeaderBytes` to 1 MiB. The server sets request body read deadlines and downstream write deadlines per request and resets them for keep-alive connection reuse. When a downstream flush fails, the router cancels upstream work and terminates the stream without emitting a success marker.

## Errors and logging

The router preserves upstream HTTP error status codes downstream. On native paths, the router preserves both the upstream status code and the upstream response body. On translated paths, the router preserves the upstream status code and returns a stable sanitized message. The router never copies raw upstream error bodies into translated errors.

Retry and rate-limit hint headers are the only upstream headers forwarded on error responses.

Structured logs record only the route, dialect, status, and duration. Logs never contain message content, session values, or secrets.

## Behavior changes compared to Python

The Go router introduces nine intentional behavior changes compared to the Python baseline:

1. Truncated translated streams terminate without `finish_reason` or `[DONE]`. The Python router fabricates `finish_reason: stop` and `[DONE]`. Tests verified this behavior against Python 0.3.0. The Go router design forbids synthetic success markers.
2. Translated responses set the downstream `model` or `modelVersion` field to the public model name. The Python router leaks the upstream model ID.
3. The router forwards native same-protocol request bodies byte-identical when no alias rewrite or confirmed llama.cpp effort mutation is needed. The Python router re-serializes JSON.
4. Native upstream errors preserve their HTTP status and raw response bytes. In some native paths, such as Gemini errors, the Python router returns sanitized error envelopes while the Go router preserves the raw upstream bytes. Both implementations sanitize `count_tokens` errors and forward only retry hint headers.
5. On Gemini endpoints, `?key=` authenticates the request but is never forwarded upstream. Any client query parameter other than `?key=`, `alt=sse`, or `alt=json` returns HTTP 400 before upstream I/O. The Python router ignores unknown Gemini query parameters.
6. The router never retries transport errors after dispatch. It replays only pre-send refused connections, pre-send DNS failures, and configured HTTP statuses.
7. Midstream translated-SSE failures keep HTTP 200 and terminate without a synthetic success marker. The Python router fabricates `response.completed` for Responses downstream or `data: [DONE]` for Chat downstream after truncation. Tests verified this behavior against the Python baseline, and the Go router design forbids synthetic success markers. Instead, the Go router emits one `response.failed` event for Responses downstream and remains silent for Chat downstream.
8. Translated-stream failures return a sanitized HTTP 502, or HTTP 504 on typed deadline expiry. The Python router returns raw LiteLLM exceptions with HTTP 500, including tracebacks that echo upstream secrets. Tests verified this leak with audio payload bytes. The Go router never copies raw upstream bodies into translated errors.
9. Gemini tool results use the exact string `response.output` form, and genuinely model-malformed tool arguments return HTTP 200 with `MALFORMED_FUNCTION_CALL` rather than HTTP 502, which applies to envelope, bound, or truncation failures.

## Known limits

Several limits remain, and full milestone 4 parity is not complete.

Raw-listener admission operates at HTTP/1 socket scope only and does not cap per-stream multiplexing. Its HTTP 503 response is an empty minimal reply sent before the server knows the requested endpoint, so it carries no ingress error envelope. By contrast, the handler-level HTTP 503 selects the Anthropic, Gemini, or OpenAI error envelope by request path. The synchronous 500ms bounded write on raw-listener rejection is a documented limitation that offers best-effort delivery rather than a full non-blocking contract.

The router enforces strict validation on usage accounting. Ordinary tool JSON numbers stay exact without `float64` round-trip conversion. The router validates standard Anthropic `usage` input, output, and cache scalar fields with cumulative accounting, where the terminal `message_delta` output count is authoritative, and refuses non-null unmapped usage members.

Streaming usage has a specific limitation because Chat and Responses report prompt tokens only in their terminal usage event. OpenAI Chat sends `"usage": null` on every earlier chunk, and the terminal event may never arrive if the stream is interrupted. Anthropic-ingress streams backed by Chat or Responses upstreams emit `message_start.usage: {input_tokens: 0, output_tokens: 0}` as an explicit placeholder because input accounting is unknown until the terminal upstream usage event arrives. These paths stream incrementally without buffering the whole generation, and the corrected terminal `message_delta` carries the authoritative cumulative totals. Only accumulators that apply the terminal delta totals are covered.

The forward path, which translates an Anthropic upstream to a Chat downstream, emits a Chat role chunk at the start and carries the provider's actual `message_start` input accounting forward. In both directions, the router fails a stream that ends without terminal usage instead of emitting an underreported total.

On the reverse stream, receiving a valid finish reason, terminal usage, and `[DONE]` ends the stream without waiting for upstream EOF. The router ignores trailing bytes in the same response body and closes the upstream body for resource cleanup.

This release covers only the ordinary text and function-tools scope and does not claim full feature-matrix parity, milestones 4 through 6, or production validation.

## Scope and deferred work

The router implements four native paths and the twelve-bridge matrix between Chat and Anthropic, Chat and Responses, Anthropic and Responses, Chat and Gemini, Responses and Gemini, and Anthropic and Gemini. These routes cover the claimed ordinary text and function-tools scope. The router also implements `count_tokens` passthrough and raw-listener admission.

The Go daemon is the production router service. Remaining deferred work is limited to further discovery enrichment, reasoning variants, media and tool variants, broader search replacement beyond the opt-in Responses-to-Chat SearXNG binding, and the native coding-agent tool matrix.

Bridges cover ordinary JSON-schema function tools. Chat custom tools use native freeform fields on Responses upstreams. On other supported upstreams, Chat and Responses custom tools use a single string-valued function. Strict mode rejects that single-string approximation before dispatch. Lossy mode reports the approximation and carries any grammar as instructions. The router restores Responses namespace names on returned tool calls.

Private reasoning output, unsupported media variants, and provider-opaque signatures outside the lossy Gemini 4 `gts1_` tool-call wrapper remain outside the translated response scope. The router preserves supplied call IDs, and translated responses carry the public model name as the downstream model identifier.

The router integrates several milestone 4 foundations:

- Reviewed request effort controls and keyless bearer session compatibility.
- The M4.1 static snapshot, disabled-reserved routes, model-list authentication, and the usable readiness rule. Snapshots copy their inputs and returned route data, and all ingress paths use the same resolver. Fifteen independent external M4.1 contracts pass.
- Authenticated diagnostics preserving the Python schema for the admitted subset.
- Catalog refresh, immutable publication, and reviewed llama.cpp discovery.
- Provider session affinity, verified by 22 independent affinity contracts.
- CLI validation, readiness, and offline checks using the reviewed exit code contract of 0, 2, 3, and 4. See [Configuration and CLI](configuration.md) for all supported CLI and environment-file flags.

Further discovery enrichment, reasoning variants, media and tool variants, broader search replacement beyond the opt-in Responses-to-Chat SearXNG binding, the native coding-agent tool matrix, and full CLI parity remain deferred.

Token counting follows strict startup and request rules:

- The router rejects `count_tokens = true` on non-Anthropic dialects at startup.
- Leaving `count_tokens` undeclared on an Anthropic-compatible provider whose base URL is not exactly `https://api.anthropic.com` returns HTTP 400 with no local estimate.
- The router supports explicit `count_tokens = true` on any Anthropic base URL and automatic detection on `https://api.anthropic.com`.
- Explicit `count_tokens = false` disables `count_tokens`.

Requests that require unsupported or disabled features receive explicit HTTP 400, 404, or 501 errors, never silent approximation.
