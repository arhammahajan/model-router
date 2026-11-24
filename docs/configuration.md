# Configuration and CLI

This reference documents the CLI flags, exit codes, configuration sections, discovery behavior, usage accounting, opt-in search replacement, and operational checks for the Go model router.

## CLI flags and exit codes

Build and run the router binary from the repository root:

```bash
go build -o routerd ./cmd/routerd
./routerd --config router-dev.toml
./routerd --validate-config --config router-dev.toml
./routerd --check-readiness --config router-dev.toml
./routerd --version
```

Running the binary without a check flag starts the HTTP server. Do not point a quick test at a configuration file that holds real credentials. Use fixtures shaped like `config.example.toml` with dummy keys.

The loader accepts TOML and JSON configuration files and preserves integer tokens exactly. When neither `--config` nor `-c` is passed, the router checks filenames in this order:

1. `router.toml`
2. `router.json`
3. `config.toml`
4. `config.json`
5. YAML filenames

YAML configuration files trigger an explicit migration error. Convert YAML files to TOML or JSON before running the router. For unknown filename suffixes, the loader tries JSON first and then TOML. Passing `--config` and `-c` together uses last-value precedence across both flags.

| Flag | Behavior |
| --- | --- |
| `--config`, `-c` | Sets the configuration file path. Both flags share last-value behavior. An explicit empty value exits with code 2. |
| `--host` | Overrides `server.host`. An explicit empty value exits with code 2. |
| `--port` | Overrides `server.port`. Zero or out-of-range ports exit with code 2. |
| `--validate-config` | Validates configuration offline without discovery cache or network I/O, prints JSON on the final stdout line, and exits. |
| `--check-readiness`, `--check` | Makes one bounded attempt per configured discovery source, prints JSON on the final stdout line, and exits with readiness status. |
| `--offline` | Skips cache and discovery during `--check-readiness` or `--check`, reports indeterminate readiness, and exits with code 4. `--offline --validate-config` is allowed. Combining `--offline` with serve mode is a usage error and exits with code 2. |
| `--version` | Prints version output and exits without loading configuration. Help flags also exit without loading configuration. |
| `--env-file` | Loads environment variables from a file before optional default credential files. This flag is repeatable. Existing process environment values win, including present-empty values. Across multiple files, the first file wins. |
| `--no-default-env` | Disables automatic loading of default credential files. |
| `--api-key` | Overrides configured ingress keys after trimming whitespace and removing duplicates. This flag cannot be combined with `server.bearer_session_compat = true`. |
| `--verbose` | Logs bounded router configuration counts. It does not enable provider, dependency, or request-body logging. |

Unexpected positional arguments exit with code 2.

When the server receives `SIGINT` or `SIGTERM`, it stops admitting new work, cancels and joins discovery workers, and drains active requests for up to 10 seconds. During that drain window, the discovery cache runs a best-effort flush. After the 10-second bound, the server force-closes remaining connections and closes idle upstream pools before exiting.

| Exit code | Meaning |
| --- | --- |
| `0` | Success. Returned by a clean serve exit, a valid `--validate-config`, or a ready `--check-readiness`. |
| `1` | Runtime or bind failure during serve mode. |
| `2` | Configuration error or flag misuse, including explicit empty host or config values, zero or out-of-range ports, unexpected positional arguments, and offline serve mode. |
| `3` | Not ready during online `--check-readiness` or `--check`. |
| `4` | Indeterminate readiness during `--offline --check-readiness` or `--offline --check`. |

## Server settings

Configure the HTTP listener, ingress authentication, limits, and timeouts in `[server]`.

| Key | Description |
| --- | --- |
| `host` | Bind address for the HTTP listener. |
| `port` | Bind port for the HTTP listener. |
| `auth_mode` | Ingress authentication mode. Accepted values are `none` and `required`. |
| `api_keys` | List of ingress API keys accepted by the router. |
| `allow_dynamic_routes` | Enables or disables dynamic routing. |
| `max_body_bytes` | Maximum request body size in bytes. |
| `max_connections` | Maximum concurrent ingress connections. |
| `max_generations` | Maximum concurrent generation requests. |
| `header_read_timeout` | Timeout for reading request headers. |
| `body_read_timeout` | Timeout for reading request bodies. |
| `write_timeout` | Timeout for writing responses. |
| `bearer_session_compat` | Enables keyless bearer session compatibility on Chat and Gemini generation ingress. Defaults to `false`. Setting this key to `true` requires zero effective ingress API keys. |

`ROUTER_API_KEY` and `ROUTER_API_KEYS` supply environment variable fallback for ingress API keys. The former `MODEL_ROUTER_API_KEY` and `MODEL_ROUTER_API_KEYS` names remain supported as legacy aliases. When `auth_mode = "none"`, the router ignores those environment variables, while configured `api_keys` still conflict with `auth_mode = "none"`. When `auth_mode = "required"`, the router requires at least one effective ingress key.

## Retry policy

Configure upstream retries and absolute request deadlines in `[retry]`.

| Key | Description |
| --- | --- |
| `max_attempts` | Maximum upstream attempts, from 1 to 20. |
| `statuses` | List of retryable HTTP status codes. Only `429` and `5xx` codes are allowed. |
| `connect_errors` | Controls retries on upstream connection errors. |
| `backoff_base` | Base delay between retry attempts. |
| `backoff_max` | Maximum backoff delay between retry attempts. |
| `deadline` | Absolute deadline covering request preparation, attempts, backoff, and full stream consumption. The clock starts at handler entry. |

## CLIProxyAPI upstream (default)

The default setup fronts CLIProxyAPI under `[providers.cliproxyapi]` instead of
configuring upstream providers directly. Upstream AI/OAuth credentials live
only in the CLIProxyAPI YAML file and environment, never in the router TOML.

```toml
[providers.cliproxyapi]
base_url = "http://127.0.0.1:8317/v1"
api_key = "replace-this-proxy-key"
# api_key_env = "CLIPROXYAPI_API_KEY"  # alternative: read the key from env
# config_path = "/path/to/cliproxyapi-config.yaml"  # optional embedded mode
# passthrough = true  # forward Chat, Responses, and Anthropic natively
```

`[providers.cliproxyapi]` accepts all shared `[providers.<name>]` common fields
(`base_url`, `api_key`, `api_key_env`, `api_key_file`, `auth_header`, `headers`,
`compatibility`, `timeout`) plus these CLIProxyAPI-specific keys:

| Key | Description |
| --- | --- |
| `base_url` | Proxy base URL. Defaults to `http://127.0.0.1:8317/v1`. |
| `api_key`, `api_key_env`, `api_key_file` | Proxy access key supplied inline, from an environment variable, or from a file. This is the client key in the proxy's `access.api-keys` list, not an upstream provider credential. |
| `auth_header` | Optional custom upstream authentication header name. When omitted, follows the effective egress dialect (`Authorization: Bearer` for OpenAI, `x-api-key` for Anthropic, `x-goog-api-key` for Gemini). |
| `headers` | Trusted static headers sent on proxy discovery probes and generation requests. |
| `compatibility` | Translation compatibility mode, either `strict` (default) or `lossy`. |
| `timeout` | Generation request timeout in seconds. Defaults to `600` (`600s`). Separate from `probe_timeout`. |
| `config_path` | Optional path to a CLIProxyAPI YAML file. When set, model-router runs the SDK service embedded in-process; when omitted, it dials an external proxy listener. |
| `passthrough` | When `true`, natively forwards OpenAI Chat, OpenAI Responses, and Anthropic Messages requests to CLIProxyAPI (while Gemini ingress and explicit `[[models]]` overrides still translate). Defaults to `false` (all non-Chat dialects translate through OpenAI Chat). |
| `refresh_seconds` | Periodic model discovery interval in seconds. Defaults to `30.0`. Setting `0` runs only the initial startup fetch. |
| `probe_timeout` | Discovery probe timeout in seconds for fetching `{base_url}/models`. Defaults to `30.0`. Separate from the generation `timeout`. |

A default external-wrapper config needs no custom `[providers.<name>]`,
`[[models]]`, or `[catalog]` sections. Model names are discovered from the
proxy as opaque `opencode-go/...` IDs. Discovery refreshes every 30 seconds by
default (configurable via `refresh_seconds`) so newly loaded plugins and OAuth
accounts become available promptly. By default the generated `cliproxyapi`
provider speaks OpenAI Chat on the wire while model-router retains
cross-dialect translation for all ingress dialects; setting `passthrough = true`
adds native OpenAI Responses and Anthropic Messages passthrough.

External mode: start CLIProxyAPI separately (see
[cliproxyapi.example.yaml](../cliproxyapi.example.yaml) for the proxy-side
access key and `plugins.configs.opencode-go-cliproxyapi` setup with secrets
from env), then start model-router with the wrapper config. Embedded mode:
set `config_path` and model-router starts the SDK service itself. The proxy
`base_url` must match the YAML listener and include `/v1`. Startup waits up
to 30 seconds for the authenticated model endpoint before discovery.
`--check-readiness` queries the configured proxy endpoint without starting
an embedded listener. Run the service before using an online readiness check.

### Proxy inspection CLI

The `router` companion CLI (`cmd/router`) adds a `router proxy` family for
the CLIProxyAPI integration. It reads `[providers.cliproxyapi]` from the detected
router config (override with `--config PATH` on each subcommand), reports
the configured mode (`disabled`, `external`, or `embedded`), the sanitized
proxy endpoint, live reachability, and the model count, and exits non-zero
when the proxy is unreachable:

```bash
go build -o router ./cmd/router
./router proxy status [--json]
./router proxy models [--json]
./router proxy plugins [--json]
./router proxy plugin enable|disable --id ID [--json]
./router proxy accounts [--json]
```

- `status` probes the authenticated `GET {base_url}/models` endpoint with
  the proxy inference key and, when the management key is set, the
  `GET /v0/management/plugins` endpoint for management availability
  (`ok`, `missing-key`, `unauthorized`, `unreachable`, `unavailable`, or
  `disabled`).
- `models` lists proxy models by their opaque IDs with the reported
  `owned_by` provider metadata.
- `plugins` lists proxy plugins with their `enabled`, `effective_enabled`,
  `registered`, OAuth, and quota flags.
- `plugin enable|disable --id ID` calls
  `PATCH /v0/management/plugins/:id/enabled` with `{"enabled": ...}`. The
  change persists proxy-side and applies after the proxy reloads. It is an
  explicit operator action with no extra confirmation.
- `accounts` summarizes `GET /v0/management/auth-files` as name, type,
  status, and availability flags. Tokens, secrets, file paths, and raw
  auth-file content are never printed.

Configure `management.secret-key` in the CLIProxyAPI YAML to enable its
management API. Keep `management.allow-remote` false for local access.
Management commands (`plugins`, `plugin`, `accounts`) read the same key from
`CLIPROXYAPI_MANAGEMENT_KEY`. The management key is separate from the proxy
inference key (`[providers.cliproxyapi] api_key`): no fallback is attempted, and
neither key is ever written to terminal output, JSON output, or errors.
HTTP clients use bounded timeouts and never follow redirects while holding
credentials.

The `harness` OpenCode agent launcher is unrelated: it starts the local
OpenCode coding tool and is unaffected by this upstream change. The
`metadata_source = "opencode-go-v1"` catalog option below selects frozen
historical capability provenance only; it carries no credentials and never
contacts `opencode.ai`.

## Providers

All upstream integrations use the unified `[providers.<name>]` table schema:

- `[providers.cliproxyapi]` — built-in CLIProxyAPI proxy provider (see [CLIProxyAPI upstream](#cliproxyapi-upstream-default)).
- `[providers.llama_cpp]` and `[providers.strata]` — built-in local discovery fleets (see [Local llama.cpp and Strata discovery](#local-llamacpp-and-strata-discovery)).
- `[providers.<name>]` — custom direct providers for non-proxy upstreams and local fixtures.

Top-level `[cliproxyapi]` and `[llama_cpp]` tables are rejected at startup with
a migration hint to move them under `[providers.*]`.

### Shared common fields

Every `[providers.<name>]` table (built-in or custom) accepts these common fields:

| Key | Description |
| --- | --- |
| `base_url` | Upstream base URL. Required for custom providers; optional for `[providers.cliproxyapi]` (defaults to `http://127.0.0.1:8317/v1`), `[providers.llama_cpp]`, and `[providers.strata]` (cannot be combined with `servers` or `ports`). |
| `api_key`, `api_key_env`, `api_key_file` | Upstream credential supplied inline, from an environment variable, or from a file. At most one form may be set per provider. |
| `auth_header` | Optional override for the upstream authentication header name (for example `bearer`, `x-api-key`, `api-key`, `x-goog-api-key`, or a custom header token). When omitted, the default auth header follows the effective egress dialect (`x-api-key` for `anthropic`, `x-goog-api-key` for `gemini`, and `Authorization: Bearer` for OpenAI dialects). |
| `headers` | Trusted static headers sent to the upstream provider. |
| `compatibility` | Translation compatibility mode, either `strict` (default) or `lossy`. Custom providers also accept the legacy `drop_params = true` alias for `lossy`. |
| `timeout` | Upstream generation request timeout in seconds. Defaults to `600` (`600s`). For discovery-backed providers (`cliproxyapi`, `llama_cpp`, `strata`), `timeout` bounds generation requests while `probe_timeout` separately bounds discovery probes. |

### Custom provider fields

Custom `[providers.<name>]` entries also accept:

| Key | Description |
| --- | --- |
| `dialect` | Upstream protocol dialect. Supported values are `openai-chat`, `openai-responses`, `anthropic`, and `gemini`. All four dialects execute natively. |
| `count_tokens` | Enables token counting. Supported on Anthropic providers only. |

### Session affinity

Configure provider-scoped session affinity on generation requests using `session_header`, `session_source_headers`, `session_prompt_cache_key`, and `session_missing`.

The router selects a session identity in this precedence order:

1. The configured destination header wins first.
2. Ordered reviewed native source headers come next.
3. The optional body fallback through `prompt_cache_key` applies if enabled.
4. When `server.bearer_session_compat = true`, keyless bearer compatibility provides the final fallback on Gemini generation requests.

Session affinity obeys these rules:

- Setting `session_source_headers` to an explicit empty list disables native source header selection.
- The router validates all reviewed native source values, including losing values. Duplicate or invalid identities return HTTP 400 before upstream I/O.
- When no identity is found, the router either omits the header or rejects the request according to `session_missing`.
- When the router selects an identity, it emits only that identity upstream.
- Without a configured affinity policy, the Go router strips native session headers. The Python router forwards them on native routes.
- Native request bodies retain `prompt_cache_key`. Translated request bodies consume `prompt_cache_key` only when the body fallback is enabled.
- Token counting does not perform affinity selection.
- Session affinity maintains no session store.

### Keyless bearer session compatibility

Setting `server.bearer_session_compat = true` enables keyless bearer compatibility on Chat and Gemini generation ingress. This mode requires no effective ingress API keys and cannot be combined with `--api-key`. Provider credentials remain the only upstream credential.

On Chat generation ingress, the router applies these rules:

- The router consumes an exact marked `Authorization: Bearer router-session-*` header and uses it as an `x-session-id` candidate only when no reviewed native session header is present.
- On marked bodies, the router merges leading plain system messages with a blank line between them. Null, structured, or named content stops the merge.
- Marked bodies validate minimal message shapes. Unmarked bodies keep standard native behavior.

On Gemini generation ingress, the router applies these rules:

- The router consumes `sessionId` and collects exact lowercase UUID4 `router-session-*` tokens from reviewed key headers or the `?key=` query parameter.
- A nonempty `sessionId` becomes an `agy-acp-*` identity using SHA256 over Python-compatible `ensure_ascii` JSON string bytes, including lone surrogates.
- That derived identity or collected token is the final affinity fallback, after the configured destination header, native source headers, and body fallback.
- Conflicting distinct tokens return HTTP 400 even when another identity would win.
- The router emits only the selected identity upstream, under the provider's configured session header.

On Gemini generation ingress only, `server.bearer_session_compat = true` also enables the `x-model-router-reasoning-effort` compatibility header:

- The literal value must appear in the canonical model's normalized, trusted `effort.values`. Raw `metadata.router` cannot admit it.
- Native Gemini maps `none` to thinking budget zero and maps `minimal`, `low`, `medium`, and `high` to uppercase levels.
- Other literals or requests with existing body reasoning controls return HTTP 400.
- Cross-dialect bridges use the shared typed planner and the provider's `strict` or `lossy` compatibility policy.
- Anthropic effort has no reviewed Go wire mapping. In `strict` mode, the router rejects the request. In `lossy` mode, the router drops the effort control and emits the loss header.

Keyless bearer compatibility does not run on read endpoints, token counting, Responses ingress, or Anthropic ingress. The router always strips `x-model-router-reasoning-effort`, including when compatibility is disabled.

### Legacy LiteLLM compatibility

Providers accept a restricted subset of legacy LiteLLM keys:

- `litellm_kwargs.headers` and `litellm_kwargs.extra_headers` apply to translated traffic only. Native forwarding and token counting ignore both legacy header maps.
- Header precedence is case-insensitive. From lowest priority to highest priority, the order is `litellm_kwargs.headers`, `litellm_kwargs.extra_headers`, provider `headers`, and finally the selected credential and session header.
- The router validates all configured header values even when overridden. Static session identities remain forbidden.
- When `litellm_provider` is present, it must match the dialect default. Valid values are `openai` for both `openai-chat` and `openai-responses`, `anthropic` for `anthropic`, and `gemini` for `gemini`. This field does not load an adapter.
- The router accepts a redundant `litellm_kwargs.api_base` only when its suffix-expanded generation URL matches the provider's generation URL for the default dialect and for every configured route destination, including disabled routes.
- `litellm_kwargs.timeout` must equal the effective provider timeout, including the default timeout.
- Neither `litellm_kwargs.api_base` nor `litellm_kwargs.timeout` creates a separate runtime endpoint or timeout. Divergent values fail startup. The declared provider `base_url` remains unchanged for token-count capability checks.
- Other legacy kwargs, including credentials, retries, and SDK controls, fail startup with migration hints.
- Unsupported tables and unknown keys fail startup. The router never ignores them silently.

## Models and route overrides

Configure explicit model routes with `[[models]]` tables.

| Key | Description |
| --- | --- |
| `name` | Canonical public model name. |
| `provider` | Configured provider name. |
| `model` | Upstream model identifier. |
| `aliases` | Optional list of alias names sharing the route destination. |
| `metadata` | Retained metadata table served on `GET /v1/models`. |
| `enabled` | Boolean controlling route availability. Defaults to `true`. |
| `dialect` | Optional upstream dialect override. Supported values are `openai-chat`, `openai-responses`, `anthropic`, and `gemini`. |

Setting `enabled = false` reserves the model `name` and all `aliases` and returns HTTP 404 for requests to them, even when dynamic routing is enabled. The router omits disabled entries from the model list and from the readiness count.

Route `dialect` selects the actual upstream protocol for the route. Omitting `dialect` or setting it equal to the provider dialect inherits the provider default. Aliases share the route destination. Catalog, llama.cpp, and dynamic routes inherit their provider's default dialect.

The effective route destination controls dispatch, URL construction, codecs, native forwarding, translated headers, and session-body consumption. Credentials remain provider-owned. When `auth_header` is omitted, the auth header follows the destination dialect when the provider natively supports it (for example `x-api-key` for Anthropic on a multi-dialect provider) and otherwise follows the provider's default dialect (for example a single-dialect Chat provider still sends its Bearer credential on an Anthropic route override); setting `auth_header` always pins the header explicitly. A route dialect override does not grant token-count support. Both the route destination and the provider's `count_tokens` policy must allow token counting. Diagnostics continue to report the configured provider default dialect.

## Capability overrides

Configure sparse validated capability assertions under `[capabilities.overrides.<canonical-public-name>]`.

- `GET /v1/models` renders a canonical `metadata.router` contract from configured routing facts and these overrides, with source freshness taken from the pinned snapshot.
- Unknown upstream support remains `null`. Explicit `false` is distinct from `null`.
- The router overwrites raw display `metadata.router` while preserving exact numeric values in other metadata.
- Alias keys do not override their canonical route.
- Overrides for absent model names still validate at startup so they can apply to later discovered routes.

## Catalog discovery

Configure remote catalog discovery under `[catalog]` and `[[catalog.rules]]`.

| Key | Description |
| --- | --- |
| `url` | Remote catalog URL. |
| `headers` | Trusted fetch-only HTTP headers. Catalog credentials never enter generation headers. |
| `timeout` | Fetch timeout. Defaults to 30 seconds. |
| `refresh_seconds` | Periodic refresh interval in seconds. Defaults to 300 seconds. Setting `refresh_seconds = 0` runs only the initial startup fetch. |
| `model_providers` | Map from exact model ID to a configured provider name. |
| `[[catalog.rules]]` | Ordered rules with `match` and `provider` fields. |
| `metadata_source` | Optional bundled metadata selector. `"opencode-go-v1"` explicitly selects the frozen bundled review. Other non-null values fail startup. |

### Matching and precedence

- An exact `model_providers` mapping wins over the first matching rule in `[[catalog.rules]]`.
- Rule patterns use the reviewed portable Python and Go regex subset. Unsupported regex constructs fail startup rather than changing routing meaning.
- Explicit `[[models]]` names and aliases, including disabled ones, take precedence over catalog routes. Local llama.cpp routes also win over catalog routes.
- Catalog rows supply model IDs and metadata only. They never supply endpoints or credentials.

### Bundled review metadata

When `metadata_source = "opencode-go-v1"` is configured, the router applies reviewed facts under these rules:

- Both the exact model ID and the dialect must match before reviewed facts apply.
- Unknown IDs stay unknown. Dialect mismatches retain provenance with `applicable: false`.
- Operator capability overrides apply afterward.
- Review age and TTL use the fixed bundled observation time, independently of catalog refreshes.
- Availability carries its own source and original catalog success timestamp, including after cache restore.
- Review timestamps render as epoch seconds in Go.
- Catalog metadata acquisition does not enable effort controls.

### Snapshots, bounds, and lifecycle

- One bounded catalog `GET` runs before the server starts listening.
- Periodic refresh publishes immutable complete snapshots. A failed refresh retains the previous routes, while a valid empty catalog replaces them.
- Fetches disable HTTP redirects, accept `identity` and `gzip` encodings only, and cap both encoded and decoded bodies at 4 MiB.
- Readiness and model listing read cached snapshots without network I/O. Catalog staleness does not remove usable routes.
- `--validate-config` remains offline. `--check-readiness` makes one bounded attempt per configured discovery source.
- On shutdown, the server closes admission, cancels and joins the catalog worker, drains active requests within the 10-second bound, and closes idle upstream pools.

## Local llama.cpp and Strata discovery

Configure local llama.cpp discovery in `[providers.llama_cpp]` and Strata
discovery in `[providers.strata]`. Both tables share the same schema and can
coexist in one config. Local-only configurations need no custom
`[providers.<name>]` entries.

`[providers.llama_cpp]` defaults `prefix` to `"llama/"` and falls back to the
`LLAMA_SERVER_URL` environment variable (and then `http://127.0.0.1:9931`) when
`base_url`, `servers`, and `ports` are all omitted. `[providers.strata]`
defaults `prefix` to `"strata/"` and falls back directly to
`http://127.0.0.1:9931` when no server address is given.

| Key | Description |
| --- | --- |
| `base_url` | Single local server URL. Cannot be combined with `servers` or `ports`. |
| `servers`, `ports` | Configured local servers and ports to probe. Each server entry accepts `url`, `name`, `headers`, `auth_header`, `timeout`, `api_key`, `api_key_env`, and `api_key_file`. |
| `headers`, `auth_header`, `api_key`, `api_key_env`, `api_key_file` | Trusted fleet-wide headers, auth header override, and credentials applied to all probes and generation requests unless overridden per server. |
| `prefix` | Optional prefix added to discovered model names. Defaults to `"llama/"` for `llama_cpp` and `"strata/"` for `strata`. |
| `aliases` | Optional boolean exposing unprefixed aliases for discovered models. Defaults to `false`. |
| `compatibility` | Translation compatibility mode, either `strict` (default) or `lossy`. |
| `timeout` | Generation request timeout in seconds for generated providers (also overridable per server). Defaults to `600` (`600s`). Separate from `probe_timeout`. |
| `probe_timeout` | Discovery probe timeout in seconds (whole-probe bound per server). Defaults to `2.0` (`2.0s`). |
| `refresh_seconds` | Periodic refresh interval in seconds. Defaults to `30.0`. Setting `0` runs only the initial startup probe. |
| `context_fallback` | Context window published when discovery has no value. If omitted, an unobserved context window stays unknown. |
| `[providers.llama_cpp.limit_overrides.<upstream-model-id>]`, `[providers.strata.limit_overrides.<upstream-model-id>]` | Overrides reported limits for an exact upstream model ID before the router applies a prefix or alias. |

### Probes and generated providers

- The router probes each server at `/health`, `/v1/models`, and optional `/props?autoload=false`.
- Every valid model ID is eligible regardless of load status.
- Canonical rows precede aliases within each server. Aliases resolve without adding listing rows.
- Route precedence puts explicit `[[models]]` reservations first, llama.cpp/Strata routes second, and catalog routes third.
- Generated providers use trusted configuration, support OpenAI Chat (default) and Anthropic Messages natively, and default to a 600-second generation `timeout`. The discovery `probe_timeout` (default `2.0s`) is separate from the generation `timeout`.
- Trusted server headers override fleet-wide headers case-insensitively and apply to generation requests as well as probes.
- A first failed probe does not activate dynamic routing. A successful probe returning an empty model list does activate it.
- Later probe failures retain existing routes while excluding the unreachable provider from readiness.

### Metadata observations and Strata detection

- Listings preserve row display metadata while replacing `metadata.router` with trusted reported or inferred observations and operator overrides.
- Valid architecture lists, partial input modality flags, template tool and effort reports, sampling defaults, and reported limits retain their provenance.
- Empty modality lists report `false`. Missing or invalid reports stay unknown.
- Router and swap modes ignore global model properties. The router performs no per-model `/props` fan-out.
- Output defaults retain zero or the `unlimited` and `until_context` markers. Positive `fit-ctx` arguments retain constraint evidence.
- Failed refreshes retain the last observations and their original timestamp. Restored route-only cache entries remain unobserved until a successful probe completes.
- Listings record whether each limit value came from observation, `context_fallback`, or `limit_overrides`. Both `context_fallback` and `limit_overrides` affect published capability facts.
- Server engine identity is distinct from per-model observations. A live probe identifies Strata from the first build-info token and otherwise defaults to `llama.cpp`. Router and swap modes can use that engine identity while continuing to ignore global model-property evidence.

### Reasoning effort mapper

- Confirmed `llama.cpp` native Chat requests consume `reasoning_effort`.
- Recognized levels supply a default thinking budget. Setting `reasoning_effort` to `none` defaults `enable_thinking = false` inside `chat_template_kwargs`.
- Explicit native values win, including `null` budgets. The router treats a `null` `chat_template_kwargs` value as absent and leaves other non-object `chat_template_kwargs` values untouched.
- The router consumes unknown, `max`, `null`, and non-string effort values without adding validation or loss headers.
- Strata and ordinary native providers retain raw passthrough.
- Responses named effort routed to known local runtimes uses the same literal mapper and existing capability policy before I/O. `llama.cpp` emits budgets or disables thinking, while Strata emits the literal effort.
- Published mapper facts record the configured or inferred enum values or native shape with distinct provenance. They do not claim that the model supports reasoning, and they do not invent budget support or defaults.
- Cache-only runtimes stay unknown until a live probe succeeds. Native requests pass through, translated effort stays refused, and mapper facts remain absent.

### Concurrency pool and refresh ordering

- Startup and CLI readiness refresh catalog discovery first and llama.cpp discovery second.
- A fixed pool probes at most 8 llama.cpp servers concurrently under whole-probe deadlines. The fleet bound covers all waves plus scheduling overhead.
- Catalog and llama.cpp discovery publish complete snapshots without overwriting each other's layer.
- Shutdown cancels and joins both discovery workers.

## Discovery cache

Configure optional route caching in `[discovery_cache]`.

| Key | Description |
| --- | --- |
| `path` | Filesystem path for the cache file. |
| `max_age_seconds` | Positive integer maximum cache age in seconds. Defaults to 86400. |

The discovery cache stores route IDs, aliases, and original source success timestamps in a Go-owned v2 format. The router ignores Python v1 files as incompatible and excludes endpoints, credentials, headers, metadata, and capabilities. It omits unsafe IDs on save, rejects files containing unsafe IDs on load, and never rewrites destinations.

Cache files must be regular files, owned by the current user, private, and at most 8 MiB. Writes use a private temporary file and atomic replacement.

Cache restore runs as a single transaction before live probes begin, with a 2-second handoff deadline on startup reads. Restored catalog routes count toward readiness, while restored llama.cpp routes remain stale and unready until verified by a live probe. Failed probes preserve original timestamps, and an empty successful probe replaces the prior inventory. Restored listing metadata stays empty until a live refresh completes.

`--validate-config` touches neither the cache nor the network. One background writer coalesces saves. Shutdown runs a best-effort flush within the existing 10-second drain budget. Already-issued filesystem calls may finish during drain because kernel file I/O cannot be canceled. A hard exit can leave behind a private temporary file.

## Usage accounting

Configure optional JSONL usage metering in `[accounting]`. When this section is omitted, the router creates no recorder and leaves the request path untouched.

| Key | Description |
| --- | --- |
| `path` | Required non-empty output file path. Supports `${ENV}` expansion following the loader's rules. |
| `max_file_bytes` | Maximum file size in bytes before rotation. Defaults to 104857600, which is 100 MiB. The minimum value is 1 MiB, and smaller values fail startup. |

When `[accounting]` is configured, the router appends one metadata-only JSONL record per completed Chat generation after the response finishes. Both native and translated paths emit records, and streamed responses write after the terminal chunk.

Each record carries only the fields `ts`, `model`, `provider`, `ingress`, `upstream`, `status`, `input_tokens`, `output_tokens`, `cached_tokens`, `duration_ms`, and `stream`. Records never contain bodies, prompts, headers, session IDs, or identities.

Metering is fail-open. The router drops write and rotation errors and never fails a request because of them. Once the file exceeds `max_file_bytes`, the router rotates it to `path.1` and keeps one prior generation.

`router usage` aggregates the JSONL file into per-day and per-model rollups reporting request counts, error counts for status codes of 400 and above, and token sums.

## Opt-in search replacement

An explicit search binding allows OpenAI Responses requests to use SearXNG on a configured `lossy` Chat route. The router waits for the upstream model to request a search, sends one search request to the backend, and resumes the same model with the result. The response reports the substitution as a translation loss and accounts for both model rounds when search runs.

Configure a backend under `[search.backends.<name>]` and bind it to the canonical public model name under `[search.bindings."<canonical-public-model>"]`. The optional backend `headers` table is separate from the model provider's headers and credentials.

```toml
[search.backends.internal]
endpoint = "https://search.example.invalid/searxng"
count = 5

[search.bindings."public-search-model"]
tool = "web_search_preview"
backend = "internal"
```

The translated path behaves according to these rules:

- It accepts one `web_search_preview` tool without options.
- Set `include: ["web_search_call.action.sources"]` to return the consulted result URLs. Inline annotations refer to actual result labels in the answer.
- For streamed requests, the first model round decides whether to search. When search runs, the router sends a search output item before streaming the final answer deltas.
- The terminal streaming event carries usage for both model rounds and annotations for verified sources.
- The router refuses additional tools, search filters, and stateful requests.
- Native Responses routes forward hosted search directly to the provider.
- The `internal/server` regressions pin the accepted behavior.

## Diagnostics and readiness

### Diagnostics endpoint

`GET /diagnostics` reports readiness, providers, catalog and llama.cpp refresh state, and cache outcomes from one pinned snapshot and one clock sample. It performs no network or filesystem I/O.

- Without configured router keys, `GET /diagnostics` returns HTTP 403 with `diagnostics_disabled`.
- When router keys are configured, the endpoint requires header authentication. Bearer scheme matching is case-insensitive, and any matching presented header credential authenticates the request.
- `GET /diagnostics` and the Chat model list do not accept query keys.
- Reports redact configured secrets and credential-shaped text. They contain no model metadata, request bodies, headers, or session IDs.
- Diagnostic `models` counts public canonical models.

### Readiness endpoint and refresh deadlines

`GET /readyz` returns `ready` with HTTP 200 or `not_ready` with HTTP 503. It reads cached snapshots without network I/O. Readiness counts effective named routes including aliases, whereas diagnostic `models` counts public canonical models.

Each refresh attempt publishes its deadline once, and the scheduler waits for that same deadline. A busy manual attempt wakes its waiting scheduler on completion.

### CLI JSON output contract

CLI check commands print JSON on their final stdout line:

- `--validate-config` exits with code 0 without cache or network I/O. Configuration or usage errors exit with code 2.
- Online `--check-readiness` and its alias `--check` exit with code 0 when ready or code 3 when unready.
- `--offline --check` skips cache and discovery, reports indeterminate readiness, and exits with code 4.
- `--offline --validate-config` is allowed. Offline serve is a usage error and exits with code 2.
- Serve and runtime failures exit with code 1.
