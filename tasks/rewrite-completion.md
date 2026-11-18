# Finish the Go rewrite

The runtime at `30ef437` passes all 610 HTTP conformance cases, 78 runner
self-tests, build, vet, and the twelve-package race suite. Two local builds
produced matching checksums for all four release targets. The extracted Linux
amd64 executable passed its CLI and HTTP smoke tests.
The user requested completion on 2026-11-18. Historical slice evidence remains
in `todo.md`; the current work is tracked here and in
[`go-rewrite-completion-gates.md`](../docs/go-rewrite-completion-gates.md).

## Operational and request coverage

- [x] All four complete-response and streaming source rows use shared typed IR.
- [x] Keyless bearer compatibility F1–F8 is independently accepted.
- [x] Inventory remaining operational, tool and release requirements from source.
- [x] Finish cached Gemini model resources, Muse catalogs and listing aliases.
  Twelve independent HTTP cases plus root context/null and trailing-path fixes
  pass; independent actual-HTTP review is integrated and race-clean.
- [x] Carry Anthropic disabled thinking through the shared reasoning planner.
  Fifteen independent HTTP cases and four-package review regressions pass;
  malformed-input guards and other-destination policy remain explicit.
- [x] Verify the reviewed request reasoning and media conversions.
  Gemini user inline images and PDFs translate to Chat, Anthropic, and
  Responses. Chat inline images translate to Gemini. Gemini WAV and MP3
  translate to Chat. Anthropic enabled-thinking budgets translate to confirmed
  llama.cpp Chat routes and to Gemini routes with a trusted budget capability.
  Chat and Responses named effort translate to Gemini when the route declares
  the matching level capability. Unsupported wire semantics retain their
  documented refusals or losses. [Go route contracts](../README.md)
- [x] Complete TOML/JSON and explicit CLI environment loading, config aliases,
  supplied-value validation and key overrides. Independent config/CLI review
  found no reachable issues; regressions pass under race detection.
- [x] Publish explicit llama fallback and per-model limit overrides. The HTTP
  listing case verifies precedence and provenance, and independent review
  verified snapshot and cache behavior.
- [x] Verify exact token counting and diagnostics admission. Existing transport
  and HTTP tests cover explicit disable, undeclared providers, invalid upstream
  counts, and diagnostics authorization and redaction.

## Tool coverage

- [x] Review a shared reversible tool-plan design with closed semantic types.
- [x] Restore namespace identities across definitions, history, choices,
  returned calls and streams. Fifteen HTTP cases cover the three foreign
  backends, including Gemini SSE and array framing, and native passthrough.
- [x] Implement custom/freeform tool codecs with explicit grammar losses.
  Chat to Responses now carries native custom definitions, grammars, calls and
  results. Complete and streamed HTTP cases pass, including truncation before
  a call is complete. The reviewed path preserves mixed call order and rejects
  mismatched or unsolicited custom calls using the request's tool bindings.
  Responses ingress emulation covers Chat, Anthropic, and Gemini, including
  namespaced custom calls. It reports approximation and validates exact string
  restoration. Chat ingress emulation to Anthropic and Gemini passes HTTP
  tests. The review regression covers name collisions in both history orders.
- [x] Verify the finite versioned and hosted tool boundary matrix.
  Twenty-six HTTP cases cover native forwarding and strict and lossy pre-I/O
  refusal for eight tool contracts, plus translated refusal for an unknown
  editor version. [Tool matrix](../docs/g9-g10-tool-boundary-matrix.md)
- [x] Implement one explicitly bound search backend, bounded continuation,
  usage aggregation, real-result provenance and source-facing streaming.
  Both reply modes pass fake-service tests for binding, continuation, usage,
  and citations. The streamed path also proves early delivery and upstream
  closure after the terminal marker. Independent review covers deadline expiry,
  invalid first events, and a valid empty final answer.

## Release and acceptance

- [x] Commit reproducible Linux/macOS archives, checksums, upstream notices,
  a dummy config, extracted-binary CLI/HTTP/auth/shutdown smoke checks and CI.
  All four targets cross-compile. Linux amd64 smoke and repeated-build byte
  equality pass; other target runtime checks await CI. No live cutover claim.
- [x] Run the full offline protocol and harness wire-contract suite after
  the last runtime changes. Coverage includes the stream failure matrix,
  independent streamed-search review, and fake upstreams for media and tools.
- [x] Document configuration migration, isolated staging, and rollback.
- [x] Verify live launcher requests on staging and cut over the local user
  service. Pi, Muse, Cline, and Antigravity completed live requests. Pi's
  launcher also passed on port 4000 after the switch. All 27 model IDs
  carried over. Antigravity passed with `gemini-4-argon`. Its saved default,
  `gemini-3.8-flash`, was missing from both catalogs.

- [ ] Reverify live model coverage before another Go cutover. The user service
  still runs Python on port 4000. Go staging on port 18080 passed native JSON
  and streaming checks for all 27 advertised models, plus 22 checks across
  11 selected route/model pairs using both modes. The Codex launcher returned
  the expected answer with `deepseek-v4.1-flash` and `gpt-6-luna`. Cline and Muse passed with
  `muse-spark-1.3-contributor`, Pi with `claude-haiku-5-5`, and Antigravity with
  `gemini-4-argon`. A separate Muse bridge check preserved an ordinary
  function call and accepted its result in the next turn. The gateway
  returned 400 for a forced function choice on a direct native Muse request,
  so this check used automatic choice.

  The recovery builds handle observed gateway reply fields without exporting
  private reasoning or changing authoritative token totals. The live checks
  require visible text and a normal terminal event. Launcher acceptance is
  still pending across every translated route and saved launcher profile.
