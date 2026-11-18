# Go bearer_session_compat Contract (M4 design freeze, rev 2)

Status: F1-F8 runtime and 41 independent external contracts are integrated.
Root regression fixes preserve null message boundaries and validate marked
Chat content blocks; canonical hashing streams into SHA256 with bounded storage.
Full 518-case external suite, 78 runner units, eleven-package race suite, vet,
and formatting pass. Independent runtime review found no reachable F1-F8 bugs;
its five-package regression suite is integrated and race-clean.
Source: stable Python HEAD via `git show HEAD:model_router/*.py` +
HEAD:router_metadata/__init__.py only (HEAD:gemini.py stable git-show only,
never working-tree gemini). No live env/creds/cache/log reads.
Base: local main `183ec16` (root-accepted 477 external + 78 runner + full 11
race + independent Gemini-source / canonical-capability / catalog / llama /
local-runtime-mapper / ordinary-chat-responses-named / Gemini-source-off-budget
reviews). This records the reviewed design and its implementation boundaries.
No full-M4/parity/production claim.

## 1. Scope

Freeze `server.bearer_session_compat` as the smallest USEFUL complete F1-F8
flag-admission unit. The blanket `=true` startup rejection is replaced by the
effective-key admission rule below. All enabled behavior ships together;
there is no unused runtime behind a rejected flag.

## 2. Source facts (Python HEAD, stable only)

- F1 admission (`config.py:392,406-409` + `863-877`): `boolean`; `false`
  inert. Effective keys built BEFORE compat check: `server.api_keys`
  (stripped) + `MODEL_ROUTER_API_KEYS` (comma-split, stripped) +
  `MODEL_ROUTER_API_KEY` (stripped), order-preserved dedup — unless
  `auth_mode == "none"` which ignores both env vars entirely.
  `none` + configured keys = startup conflict; `required` + zero effective
  keys = startup failure. Compat `true` + any effective key = startup
  `SystemExit` ("requires unauthenticated client access"), value-free.
- Auth grammar is SEPARATE from compat grammar (`discovery.py:789-815`):
  `api_keys` empty means open; candidates `Authorization: Bearer <k>`
  (case-INSENSITIVE scheme, strip), `x-api-key`/`api-key`/`x-goog-api-key`
  headers, plus `?key=` values only when caller passes them —
  `http.py:292,383` passes `query.get("key",[])` only on Gemini
  discovery/generation paths; non-Gemini ingress (`http.py:339,383`) uses
  headers only. `hmac.compare_digest`. Do not broaden compat grammar with
  this auth case-fold.
- F2 Gemini opt-in (`app.py:195-202`, `client_compat.prepare_gemini`): flag
  + `ingress == gemini`, inside `_dispatch` (generation POST only), before
  `_validate_ingress`. Token grammar: `Authorization` requires exact
  case-sensitive `"Bearer "` prefix (`startswith`, slice `[7:]`, no
  strip/fold); header names match case-insensitively
  (`x-goog-api-key`/`x-api-key` via `lower()`); every candidate must
  `_SESSION_TOKEN.fullmatch` = `router-session-` + lowercase UUID4
  (`[0-9a-f]{8}-...-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}`).
  Uppercase hex never matches. `query_keys` entries same fullmatch.
  `>1` DISTINCT tokens = 400 `invalid_session_id`. Non-matching values
  ignored (ordinary keys pass to normal auth).
- F2 `sessionId` body (`prepare_gemini`): non-string/non-null = 400;
  field ALWAYS stripped on this path. Truthy string wins over launch token:
  `agy-acp-` + `sha256(json.dumps(session, sort_keys=True))` with default
  `ensure_ascii=True` (NOT `Go json.Marshal(string)`). `None`/`""`/absent
  falls back to launch token (maybe none). Native-header priority resolves
  later in `_session_id`, never here. Headers NOT mutated
  ("stay intact so duplicate identity headers still reach validation").
  Credential headers/query keys excluded upstream via
  `_DROP_REQUEST_HEADERS`/URL building. Frozen goldens (Python stdlib):
  Full SHA-256 vectors over canonical JSON string bytes:
  - `"abc"`: `6cc43f858fbb763301637b5af970e2a46b46f461f27e5a0f41e009c59b827b25`
  - `"\u00e9"`: `abc3932c9ec58f042750738c49034e7ee67cf556d6be86832ac3dda38decb703`
  - `"\ud83d\ude00"`: `e30439cb87e140c0b998ad3095285abfec0294e2a8916f9a8f30d85734e6f82b`
  - `"<>&"`: `de0b609d2f9f5af97658414422d275ed44984bc2eaa807e7ba5f308b4e436365`
  - `"\ud800"`: `8c0c59dd0d275aadcd462a5fe12eb352cbdfeaf961eae4f85a4660521df7d2f5`
  Parse the raw JSON string for hashing: normalize escapes to Python JSON
  ensure_ascii output, including lowercase surrogate-pair and lone-surrogate
  escapes. Never decode lone surrogates through Go string replacement. Stream
  canonical bytes into SHA-256 under the existing body bound; do not buffer
  a whole expanded body or impose a new session-ID length limit. JSON null
  and the decoded empty string select fallback; escaped NUL is a nonempty ID.
- F3 Chat opt-in (`app.py:204-205`, `prepare_chat`): flag +
  `ingress == openai-chat`, AFTER `_validate_ingress`. Exact
  `_SESSION.fullmatch(authorization)` (full `Bearer router-session-<uuid4>`,
  case-sensitive) else inputs unchanged. On match: strip `Authorization`
  (all case variants), inject `x-session-id: <token>` ONLY if no native
  header (`x-opencode-session` + `_NATIVE_SESSION_HEADERS`, which already
  contains `x-session-id`) present. Injected header is a NATIVE-source
  candidate under `_session_id` ordering (selected only if provider
  configured that name), NOT the generic `fallback` (only `prepare_gemini`
  produces fallback). Merge leading plain `{"role","content"}` string
  system messages with `"\n\n"`; named/structured stops merge.
- F4 precedence (`headers.py:_session_id`): no-op (`None`) when
  `provider.session_header` empty. Else validate EVERY reviewed native
  value even when it loses/disabled; duplicates incl. case variants = 400;
  grammar `[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}` else 400. `Connection`-
  nominated names skipped entirely (neither validated nor selected).
  Order, first enabled wins: (a) explicit configured
  `provider.session_header`; (b) configured `session_source_headers` in
  order; (c) `prompt_cache_key` body ONLY when
  `provider.session_prompt_cache_key` set; (d) `fallback` (Gemini compat
  token) last, validated only if no earlier candidate won. Custom header means
  natives are validated-but-losing unless also configured. Explicit `[]` disables
  native header selection, not the explicit header, configured body fallback or
  Gemini fallback. Translated routes with `prompt_cache_key` affinity consume field
  before validation (`app.py:221-224`); native passthrough preserves body.
- F5 no-compat paths: `_dispatch` serves generation only. Compat runs on
  POST Gemini `generateContent`/`streamGenerateContent`
  (`gemini.parse_method_path`) and POST `/v1/chat/completions`
  (+ `/chat/completions` alias). NEVER on GET list/get
  (`gemini.is_list_path`/`parse_model_path`, auth-only, no token/`sessionId`
  /effort), Responses, Anthropic, count-tokens (forwards `session_id=None`,
  affinity never counted), `/livez`/`readyz`/`diagnostics`/`/`, unknown
  labels. `?key=` accepted only on Gemini paths.
- F6 effort (`app.py:197-202`, `prepare_gemini_effort`): Gemini ingress +
  flag + `x-model-router-reasoning-effort` header present ONLY. `contract`
  = RENDERED canonical router contract from `_route_entry` (trusted
  `_capabilities` + `capability_overrides` + freshness via
  `router_metadata.render`) — explicitly NEVER raw `route.metadata.router`.
  Value must be literal member of rendered
  `reasoning.effort.values.value` list else 400. Reject non-object
  `generationConfig`/`thinkingConfig`. Reject `thinkingLevel`/`thinkingBudget`
  PRESENCE via `in` (including explicit null) = 400 conflict. `none` ->
  `thinkingBudget:0`; `minimal/low/medium/high` -> `thinkingLevel` UPPER;
  other published (e.g. `xhigh/max`) = 400 "Native Gemini cannot express".
  Non-Gemini target returns `(payload, effort)`; caller sets
  `translation.params["reasoning_effort"]` + origin header
  (`app.py:236-238`), flowing through `_prepare_gemini_translation` /
  `_prepare_translation` + reviewed llama controls. Header never forwarded
  (`_DROP_REQUEST_HEADERS` always covers it, even when flag off).
  Errors value-free, no echo; `request_outcome` omits queries/IDs.
- F7/F8 emission (`headers.py:_session_request_headers`,
  `constants._DROP_REQUEST_HEADERS`): enabled (`session_header` set):
  consume all native identity headers, emit only
  `provider.session_header: <selected>` or nothing. Python without affinity forwards ordinary identity headers. Go preserves its
  stricter existing isolation: reviewed identity headers are always scrubbed
  and only a configured selected identity is emitted. Do not reverse this
  baseline delta when adding compat.
  Credentials = `Provider.auth_headers()` (provider config only); effective
  destination is `EffectiveDialect`, auth stays provider-owned.
  Disabled effort header is still transport-stripped (DROP is
  unconditional), never validated/mutated, body byte-identical.

## 3. Go baseline (same base)

Exists: `router/session.go:PrepareSession` + `transport.OutboundHeaders`
(typed `Session{ID,Has}`, validate-all/losing/disabled/`Connection`/
`prompt_cache_key`-on-translated-only/body-raw-passthrough, single-identity
emission under `provider.SessionHeader`, `EffectiveDialect` dest,
`AuthHeaders(p)` provider-owned, query-key Gemini-only
(`native.go:authedQuery`, `native_gemini.go` allowlist `alt+key` only),
count path session-none, 16-path dispatch, `protocol.PlanEffort` for
Chat/Responses/Llama/Strata + `DecodeGemini*` thinking intents +
`EncodeChat/Responses` + llama native applier, pinned-snapshot capability
facts (`snap.RequestCapability` + `capability.ReasoningFor`).
Missing: compat pre-step (strict token regex, `sessionId` ascii-hash, chat
`Authorization->x-session-id` + system merge, effort carrier). No
`router-session-*` collection, no `ensure_ascii` hash, no system merge, no
`x-model-router-reasoning-effort` handling on Gemini ingress.

## 4. Frozen enabled semantics (including declared Go deltas)

| # | Rule |
|---|------|
| F1 | `boolean`; `true` requires zero EFFECTIVE keys (config + env order as §2, `none` ignores env; `none`+keys conflict; `required`+zero fail stay). Violation = startup-fail value-free. `false` disables compatibility interpretation/mutations only; existing affinity, native llama mapping, alias rewrite and transport policy stay active. The effort header remains stripped and unvalidated. |
| F2 | Gemini generation POST: distinct valid carrier tokens from reviewed header/query sources; conflicting tokens reject. Consume sessionId with string/null validation, ACP canonical hash wins, otherwise token fallback. GET paths never run compat. |
| F3 | Chat generation POST: exact marked Bearer only; strip it, inject x-session-id only if no native-name header is present, merge leading plain systems. Unmarked inputs stay untouched. |
| F4 | Existing affinity validation and no-policy behavior stay. Connection nominations skip identity-header selection/validation; they do NOT hide credential/effort compat carriers from collection. Validate all reviewed losing headers and configured body value. |
| F5 | Responses/Anthropic/count/GET/read: no compat step. Count passes Session{}. Query-key collection is Gemini-only. |
| F6 | Gemini effort carrier: published literal membership; configs object/null, canonical control-key presence conflicts including null; native map or value-free400; bridged typed carrier intent through policy. Anthropic is the explicit unsupported-loss delta in section6. |
| F7 | Explicit destination header > configured header sources > configured prompt_cache_key > Gemini fallback. Empty sources disable only native selection; body/fallback remain. Losing fallback is not validated. Chat injection is a native-source candidate, not fallback. |
| F8 | Sole transport emission stays: one selected identity or none, provider-owned credential, consumed carriers, no request-scoped shared state. Disabled effort header remains stripped; reviewed identity headers retain existing unconditional Go scrubbing even without affinity. |

## 5. Ingress x dest matrix (compat-relevant only)

- Gemini POST -> native Gemini: F2+F6-native (direct body mutation) + F7/F8.
- Gemini POST -> Chat / Responses: F2 + F6-bridged (`PlanEffort` ordinary/local scope) + F7/F8.
- Gemini POST -> Anthropic: F2 + F6-validated-then-DROPPED (deliberate Go unsupported-loss policy, §6) + F7/F8.
- Chat POST (`/v1/chat/completions`+alias) -> any dest: F3 + F7/F8 only (no effort carrier here).
- All other ingress/dest (Responses/Anthropic/count/GET/list/get): F5, no compat step; existing decode/plan/transport unchanged.

## 6. Wire effort table + loss actions (exact, no invented budgets)

Native Gemini target (direct mutation, bypasses `PlanEffort`/outbound-Gemini planner — no global migration prerequisite): `none`->`generationConfig.thinkingConfig.thinkingBudget:0`; `minimal/low/medium/high`->`thinkingLevel` UPPER (`MINIMAL/LOW/MEDIUM/HIGH`); `xhigh/max`/other published->400 "Native Gemini cannot express"; body `thinkingBudget`/`thinkingLevel` key presence (incl. null) or non-object configs->400. Preserve unrelated `RawMessage` members + exact bigints (no float64); output after mutation carries no whole-body byte-identity claim.
Bridged Chat target: membership-checked level -> `PlanEffort(intent, caps, OrdinaryRoute|LocalRoute(llama/strata))`; off/budget/named per existing planner (unknown facts neutral, known-empty/false dropped; llama budget table only for mapped levels; `minimal` never renamed to `low`). Responses target: same with `OrdinaryResponsesRoute|LocalResponsesRoute` (named literal incl. `none`; explicit budget always dropped). Loss field is the closed static origin `x-model-router-reasoning-effort`,
not the Chat-body origin. Add a dedicated carrier constructor/origin, never
misuse the Gemini-level constructor for published xhigh/max values. Strict refuses pre-I/O; lossy carries `x-model-router-loss` and continues without control.
Anthropic destination: NO adapter-free exact mapping exists. Python delegates to LiteLLM model-dependent budget/adaptive choice; Go forbids model-name heuristics + invented numeric tables. Frozen policy: validated header is DIAGNOSED DROPPED (`dropped`, "no reviewed Anthropic wire mapping; adapter approximation not ported"), strict 400 / lossy header+continue. This is deliberate unsupported-loss, not source parity and not silent ignore; no prior Anthropic-destination unit is required for F1-F8 completeness. Outbound-Gemini generic reasoning (non-Gemini ingress) is out of scope for this freeze.

## 7. Smallest typed boundary (proposal, no behavior change beyond F1-F8)

Owner: `router` owns compat collection/mutation; `protocol` owns closed
`ReasoningIntent` constructors + `PlanEffort`; `transport` SOLELY emits
`OutboundHeaders(..., sess)`; `server` owns seam ordering + pinned snap.
No ambient stores/globals/per-bridge copies, no model rewrites, no
`map[string]any` semantic params, no `metadata.router` reads.
```go
// router/compat.go
type CompatFallback struct{ ID string; Has bool }
func PrepareGeminiCompat(hdr http.Header, queryKeys []string, body []byte,
  published capability.OptionalStringList, target string) (out []byte, fb CompatFallback,
  effort protocol.ReasoningIntent, rerr *protocol.RouterError)
func PrepareChatCompat(hdr http.Header, body []byte) (out []byte,
  hdrOut http.Header, rerr *protocol.RouterError)
```
`published` = `ReasoningFor(contract).EffortValues` from the normalized
SAME pinned `snap.RequestCapability(model,p)` used for planning. Its semantic
values equal Render output; do not serialize metadata or sample a clock just
for membership. Never use raw metadata or model-name gates. Raw `[]byte` + `json.RawMessage`
envelopes preserve unrelated subvalues/exact bigints; untouched bodies stay
byte-identical. `PrepareGeminiCompat` validates header membership +
shape/presence conflicts BEFORE Gemini decode; if compat effort present AND
decoded `gen.Reasoning` present -> 400 pre-I/O (mirrors Python double-control
refusal). Go `http.Header` arrays harden Python `Mapping` single-value
assumption: collect across ALL casings + repeated lines; compat tokens dedup
by SET (same token repeated is one; `>1` distinct = 400), while session
headers keep existing duplicate-reject (any `>1` value = 400). `?key=`
uses `r.URL.Query()["key"]` on Gemini ingress only.
Effort headers are single-valued: reject repeated/case-variant effort fields
rather than choose one. Compat session tokens dedup identical matches; multiple
distinct valid matches reject, including marked Chat Authorization fields.
Nonmatching credentials remain ordinary values. Chat native-presence checks
include Connection-nominated and empty headers before affinity later skips or
validates them; do not opportunistically inject a replacement. This set includes
x-opencode-session plus the seven reviewed natives, without broadening the
existing affinity validator's names.

Effort preparation recognizes only the existing camel/snake config aliases;
collisions reject, and thinking-control presence under either spelling conflicts
with a carrier, even null. Preserve the selected container spelling on mutation.
This is explicit Go alias hardening; disabled/no-carrier native bodies stay raw.
Chat marked-body preparation validates minimal message object/role/content shape
before merging. It never runs native through a full bridge decoder; unmarked
native requests retain existing validation/fidelity. Translated source validation
still runs before policy and I/O.

The Anthropic unsupported destination is an explicit closed planner target,
not a duplicate bridge-owned policy. Never create ambiguous simultaneous target
booleans. Keep native Gemini direct mutation outside the bridge planner. This
unit handles every enabled carrier with mutation, a typed plan, or an explicit
policy/error; it makes no claim of Anthropic adapter-conversion parity.

Seam callers: `handleGemini` (after auth/query-allowlist/body/model/snap-pin,
before `prepareSession`): if compat-enabled call `PrepareGeminiCompat`,
thread `fb` as `PrepareSession` final fallback (migrate all callers, delete the old signature), thread `effort` as below; Chat
POST handlers: if compat-enabled call `PrepareChatCompat` after the existing body/model
checks, before `prepareSession`. Its marked-message shape guard precedes merge;
full translated validation remains in the existing decoder. Native-Gemini: mutated body forwards
via `serveNativeForward`. Bridged: decode -> `PlanEffort(effort OR body
intent, caps, scope)` -> `EnforcePolicy` -> encode once. Transport already
drops effort header unconditionally (§2-F6); no new preserve/refusal in
native-unmarked. Pre-existing native alias/llama and affinity behavior still applies.
Admission deletion: DELETE `config.go:789-799` blanket `true`-reject; keep
`type bool` check; effective-keys logic (§3, already order/dedup/`none`
parity) stays; add `if BearerSessionCompat && len(APIKeys)>0 -> startup
fail`. Minimal bool constructor APIs only; no global cache.

## 8. Source vs hardening vs genuinely unresolved

Source: §2 mechanics with file:line refs. Deliberate hardenings (keep):
value-free errors/no echo, `?key=` Gemini-only, identity-only `Connection` skip,
validate-losers, unknown-query reject, count never translated, framing
last, Header-array duplicate hardening, unconditional effort-header strip and
existing unconditional reviewed-session-header scrub (including no-policy).
Policy choice, not a missing-data assertion: this unit does not implement
Anthropic adapter approximation, even when a native descriptor is known.
Section6 dropped-loss is the terminal Go policy; no general conversion or
model-name fallback is inferred from partial native facts.
No other prior unit (canonical-capability render + `PlanEffort` + decode
scopes already reviewed: 477/78/11 + independent reviews) blocks F1-F8.

## 9. Proof inventory (hermetic, no live/browsing)

Config: effective-keys order/env/`none`/`required`/compat-true+keys fail +
bool-type unit (no env leakage in test). Router: strict regex/case
(uppercase-hex + `bearer `-lowercase reject), distinct-vs-duplicate tokens,
`?key=` non-Gemini ignored, `Connection` skip, hash goldens (unicode,
surrogate-pair/lone-surrogate, `<>&` literal), chat strip/inject/native-wins + merge
edges, `[]`/disabled precedence. Protocol: membership + null-presence +
non-object conflicts, native map/`xhigh/max` 400, bridged Chat/Responses/
llama/strata plan + strict/lossy, Anthropic dropped-loss (strict 400, lossy
header+no emit, no invented budget). Transport: sole emission, carriers
consumed, disabled/unmarked byte-identity, auth provider-owned,
`EffectiveDialect` dest. Independent external: 4 Gemini carriers x 4 dest
affinity matrix + ACP-win + consumed-body + conflict-400 + effort exact/map/
reject/ignored-when-off + count-no-session. Mutation: drop `fallback` arg,
skip duplicate check, forward carrier, widen `?key=`, echo token, trust raw
metadata for effort — all must fail. Race `-race`: concurrent distinct
sessions keep affinity, no shared state.

## 10. Report

- Artifact: this file only. Base `183ec16`.
- Decisions: F1-F8 per §2/§4 with the explicit Go alias and unsupported-destination deltas; chat token travels as header
  candidate; hash `ensure_ascii` goldens; effort needs rendered contract +
  existing `PlanEffort` scopes; Anthropic-dest effort = diagnosed dropped
  (terminal Go policy, §6); disabled effort header still stripped;
  byte-identity only for untouched bodies; STALE claims (no capabilities /
  no generation / no Go map) removed by this revision.
- Open choices: none. Independent runtime review is accepted.
