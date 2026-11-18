# Go rewrite completion gates (M4/M5/M6)

Baseline: local main `a83929e`; accepted keyless-compat slice passes
518 external cases + 78 runner units + full eleven-package race suite.
`tasks/plan.md` / `tasks/todo.md` still cite older 360–477 totals; the
conformance runner (`go/conformance/run.py`) and `go/conformance/cases/`
(518 files) are the source of truth, not those frozen numbers.
No blanket full-parity claim follows: each gate below names required
behavior vs valid unsupported-migration errors vs accepted Go divergences.

Accepted deliberate divergences (do NOT re-litigate as gaps):
canonical capability listing renders review/observation instants as epoch
seconds, not Python's bundled ISO text; translated-only legacy header
precedence and redundant endpoint/timeout aliases are startup-rejected
unless exactly equivalent; cache v2 ignores Python v1 files; unknown
upstream facts stay null rather than defaulted.

## M4 — config / CLI / read-paths / resources

### G1 Gemini discovery read-paths absent
Trigger: `GET /v1beta/models?pageSize=N`, `GET /v1beta/models/{id}`.
Go today: `GET /v1beta/models` falls to catch-all
`go/internal/server/server.go:61` (`handleNotFound`) and
`go/internal/server/native.go:100-125` (`nativeNotFound`) returns Gemini
404 model_not_found; only `POST` generation routes exist
(`native.go:89-93`). Stable Python: `model_router/http.py` `_gemini_get`
serves `models.list`/`models.get` with pageSize/pageToken auth and 404
`model_not_found`; `model_router/gemini.py:parse_model_path,is_list_path`.
Deps: registry snapshot read only, no network. Unit: fake-config Go server,
`curl` list + get + unknown-id 404, header auth required.
Check: `go test ./internal/server -run TestGeminiModels -count=1`.

### G2 Muse catalog + aliases absent
Trigger: `GET /muse-code/models`, `GET /v1/muse-code/models`,
`GET /models`, `GET /`.
Go today: `server.go:55` registers only `/v1/models`;
`handleModels` at `server.go:424-483` serves OpenAI shape only; other
paths 404 via `handleNotFound` (`server.go:484-494`). Stable Python:
`http.py:do_GET` serves muse payload vs models payload, `/models` alias,
and `/` name/version/endpoints index. Deps: same snapshot/listing seam as
G1. Unit: table test over five paths, auth behavior preserved.
Check: named conformance cases `m4_models_alias_*`, `m4_muse_*`.

### G3 Model-list enrichment + limits truthfulness
Trigger: routes with llama/catalog limits, overrides, unknown facts.
Go today: listing renders `metadata.router` from snapshot
(`server.go:424-483` + `internal/capability/render.go`); `limit_overrides`
and `context_fallback` are startup errors
(`go/internal/config/llama.go:198-207`). Stable Python:
`README.md` limits section, `[llama_cpp.limit_overrides]`,
`context_fallback`, per-field source/kind reporting; `config.example.toml`
documents them. Valid unsupported migration: keep rejecting those knobs
with hints (do NOT silently implement). Required: null stays unknown,
empty list renders `[]` not null, overrides never leak into routing.
Unit: snapshot listing fixture with text/image modalities + unknown limits.
Check: `go test ./internal/capability ./internal/server -count=1`.

### G4 CLI / env-file / YAML surface
Trigger: `--env-file`, `router.yaml`/`config.yaml` lookup,
`--models-preset`, `--verbose` parity.
Go today: flags are `--config/--validate-config/--check-readiness/--check/
--offline/--host/--port/--version` (`go/cmd/model-router/main.go:43-46`);
env-file auto-load exists (`go/internal/config/config.go:318-331`) but no
YAML loader or extra flags. Stable Python: `model_router/cli.py:24-145`
default-file search, `--env-file/--no-default-env`, `--offline` semantics,
`--verbose`. Valid divergence: YAML stays unsupported (config-compat doc
§11 defers it); must fail startup value-free, never half-parse.
Required: `--offline serve` stays usage error; offline check exit 4;
validate exit 0/2; readiness 0/3. Unit: CLI arg matrix test, no network.
Check: `go test ./...` + manual `--help` diff vs Python.

### G5 count_tokens + diagnostics deltas
Trigger: non-Anthropic route count, malformed upstream success shape,
diagnostics without keys.
Go today: implemented (`native_count.go:17-140`), Anthropic-only guard,
64 KiB bound, no slot, sanitized errors. Confirm remaining: explicit
`count_tokens=false` disable path, undeclared non-first-party 400 without
I/O, 2xx non-`{input_tokens:int}` 502. Stable Python:
`model_router/count_tokens.py:30-52`, `config.py:198-214`,
`constants.py:COUNT_TOKENS_ENDPOINTS`. Diagnostics: Go
`server.go:306-320` is pinned-snapshot, redacted; Python
`http.py:do_GET /diagnostics` + discovery summary. Required: no estimate,
no generation endpoint call, no secret/log leak. Unit: fake-upstream count
matrix + diagnostics 403/401/200. Check: existing `native_count_test.go`
plus one disabled-provider case.

### G6 Harness-facing M4 wiring (pi/Muse/Cline/Antigravity docs)
Trigger: harness uses native ingress + session compat + lossy defaults.
Go today: bearer compat F1-F8 done; session/affinity done; bundled
`router.toml` lossy defaults are Python-side. Required: document which
harness flows are offline-verified vs explicitly live-only; no code change
unless a wire gap is found. Unit: harness smoke scripts against fake
upstreams (see M6). User/live decision: real provider keys and launcher
binaries cannot be invented here.

## M5 — tools: namespaces / custom / versioned / hosted / search

Python reference: `model_router/tools.py` (`decide_tool`, `ToolPlan`,
`_NATIVE_RESPONSES_HISTORY`), `model_router/tool_namespaces.py`,
`docs/tool-translation.md`, `docs/native-tools-triage.md`.
Go today: ordinary JSON-schema functions only; everything else is a
specific pre-I/O 400 in both modes:
`protocol/chat_decode.go:1276-1283` (custom/computer/unknown),
`protocol/responses_decode.go:288-290` (hosted/versioned/namespaced/custom
no bridge), `protocol/gemini_decode_tools.go:200-213` (only
`functionDeclarations` portable; `googleSearch`/`codeExecution`/etc.
require same-protocol route), response IR has no media/search variants by
design (`protocol/response_ir_types.go:36`).

### G7 Namespaced functions (reversible identities)
Trigger: `{"type":"namespace",...}` with duplicate child names, history
and stream continuation. Go refusal above; Python:
`tool_namespaces.py:flatten_namespaces/restore_namespaces`. Required:
collision-safe alias minting, stable call IDs, loss paths retain
`tools[i].tools[j]` location. Unit: smallest is Responses ingress with
two same-named children in distinct namespaces, JSON + SSE, strict 400
today → mapped tomorrow. Check: new `m5_namespace_*` conformance pair.

### G8 Freeform custom emulation (Chat ↔ Responses)
Trigger: Chat `type:custom` ↔ Responses custom, grammar-carrying tools.
Go refusal above; Python: `tools.py:78-92,122-160,242-330` (single
`content:string` function emulation, grammar reported as approximation,
`response.custom_tool_call_input.delta/done` normalization, bounded
buffers 1 MiB/field, 8 MiB total, 1024 calls). Required: emulation always
`approximated`, never silent; history/result restoration shares one plan;
malformed/oversize/unfinished streams fail without fabricated success.
Unit: one fake-backend JSON case + one SSE case per direction, then
fragmented-JSON/Unicode/concurrency variants. Deps: G7 identity seam.

### G9 Versioned typed client tools (bash/editor/computer)
Trigger: Anthropic `bash_20250124`, `text_editor_*`, `computer_*`.
Go refusal e.g. `protocol/anthropic_ingress_test.go:225` documents 400;
Python `tools.py:80` classifies as typed-client, currently
rejected/dropped per `native-tools-triage.md`. Valid outcome is either a
reviewed versioned mapping or continued explicit 400/drop — not silent
pass-through. Each accepted version needs its own schema/prompt contract.
Unit: per-version fixture proving exact vs approximated vs unsupported.
Each version needs a reviewed executable contract; unknown versions stay refused.
This is an engineering review gate, not an extra user permission request.

### G10 Hosted tools stay native-only (search/MCP/code-exec/grounding)
Trigger: `web_search`, `file_search`, `mcp`, `code_interpreter`,
`googleSearch`, `urlContext`, citations/annotations, `thoughtSignature`.
Go: request refused pre-I/O; response 502 on server/citation/refusal/
logprob/nonzero thought shares (Gemini/Anthropic/Responses response
tests). Python: same refusal/drop policy; `tool-translation.md:22-28`
requires native routes, never adds hosted execution; `_skip_mcp_handler`
stays set (`config.py:269-270`, Go `config.go:1040-1041` rejects the key
as unsupported because the router never executes tools). Required: keep
fail-closed; allowed mapping only per verified destination service with
response metadata/events/usage/continuation reviewed. No guessing
endpoints or switching models to satisfy a tool.

### G11 Media translation matrix
Trigger: images/audio/PDF/inlineData/fileData/video, Gemini
`executableCode`, `safetySettings`, `candidateCount>1`, non-text
modalities. Go: closed enum/part validation, malformed 400 both modes,
untranslatable fails 502 both modes (see `go/README.md` Chat↔Gemini and
Responses↔Gemini sections). Python: `gemini.py:485 media`,
`gemini.Translation.tools/generation/thinking`, README Gemini table.
Required: exact carry only where destination defines it; elsewhere
diagnosed loss (strict 400 / lossy drop + `x-model-router-loss`).
Unit: per-modality fake-backend pair; audio only Chat-bound.

### G12 Opt-in search replacement (new reviewed behavior)
Stable Python has no search backend; `docs/go-config-compatibility.md`
§11 records that search-replacement bindings have no existing parity
contract. The user-owned rewrite design explicitly requires one optional
backend. This needs a reviewed new contract rather than guessed Python parity.
Offline implementation is authorized; enabling a real backend is separate.
Required gates:
opt-in bound backend only; collision-safe synthetic calls; bounded
same-route continuation (no silent model/provider switch); real citations
and aggregated usage; no harness-action execution; no silent
replacement or mixed calls (at most one ownership per call); private
state fail-closed (no `cachedContent`/`continuationToken` reuse).
Smallest unit: one fake search backend + one continuation case, then full
stream. Use explicit fake-service endpoints and primary backend documentation.
Real credentials and deployment enablement remain operator-owned; their
absence does not block offline implementation.

## M6 — release / smoke / rollback

### G13 Offline harness smoke (no live calls)
Trigger: pi / Muse / Cline / Antigravity flows against Go with fake
upstreams. Required: scripted matrix covering Chat/Responses/Anthropic/
Gemini ingress × native + translated destinations, streaming +
truncation + error sanitization, session-affinity headers, custom-config
strict refusal. Checks stay hermetic: `go build ./...`, `go vet ./...`,
`gofmt -l internal cmd` empty, `go test -count=1 ./...`,
`go test -race -count=1 ./...`, `python3 conformance/run.py --backend go`.
No live inference. Artifact: smoke checklist + release notes stating
Python remains production until a selected deployment's full workflows
pass.

### G14 Artifacts + rollback doc
Trigger: cutover request. Required: installable binaries for declared
platforms, config migration notes (equivalent-alias rule, header
precedence, rejected kwargs with hints), documented rollback to Python
(single config flip + process swap, no schema migration). Valid
divergence: Go cache v2 does not read Python v1 — document, do not bridge
silently. Check: clean install from artifact + rollback drill on staging.

### G15 Explicitly live acceptance (separate, never implied)
Requires actual user credentials, provider endpoints, launcher binaries,
or paid inference. Includes: any real-upstream generation probe, quota/
billing behavior, latency/throughput claims, Antigravity account-based
auth beyond API-key mode, OAuth/Vertex paths. This worktree must not
invent keys, endpoints, or results. Report as blocked until the operator
supplies them; keep offline gates green independently.

## Stale task/doc claims to correct (report only, not edited here)
- `tasks/plan.md:309-310`, `tasks/todo.md` totals (360/399/432/452/477)
  predate the accepted 518-case baseline; runner remains source of truth.
- `tasks/plan.md:196-200` M4 scope paragraph implies discovery/affinity
  outstanding while later slices record them integrated; reconcile counts
  and readiness/drain status in one place.
- `go/README.md:341-343` trailing CLI/enrichment note overlaps G4; keep
  until G4 lands.

## Prioritized sequence and blockers
Order: G1 → G2 → remaining reasoning → G3/G5 → G7 → G8 → G9/G10/G11
(per-tool review), with G4/G12/G13/G14 developed alongside independent units.
The new search contract and version-specific mappings require source review
and fake-service tests; they are authorized engineering work. G15 requires
real deployment credentials and workflows, which cannot be invented. YAML
remains an explicit supported-config decision; any rejected format needs a
clear migration path. Offline completion and live cutover are separate gates.
