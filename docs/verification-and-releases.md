# Verification and release archives

This document covers local verification commands, conformance suite coverage, recorded divergences from the Python baseline, and development release archive builds.

## Local checks

Run these commands from the repository root to build the Go binary, run static checks, execute Go tests, and run the conformance suite:

```bash
go build -o /tmp/routerd ./cmd/routerd
go vet ./... && gofmt -l internal cmd
go test -count=1 ./...
go test -race -count=1 ./...
python3 conformance/run.py --backend go --go-bin /tmp/routerd
python3 conformance/test_run.py
```

Each command verifies a specific stage:

| Command | Verification target |
| --- | --- |
| `go build -o /tmp/routerd ./cmd/routerd` | Builds the Go binary at `/tmp/routerd`. |
| `go vet ./... && gofmt -l internal cmd` | Runs static analysis and checks formatting. The `gofmt -l` output must be empty. |
| `go test -count=1 ./...` | Runs unit tests and fake-upstream tests without test caching. |
| `go test -race -count=1 ./...` | Runs concurrency checks under the Go race detector. |
| `python3 conformance/run.py --backend go --go-bin /tmp/routerd` | Runs the external conformance suite against the built binary and reports case counts. |
| `python3 conformance/test_run.py` | Runs the 78 conformance runner unit tests. |

Conformance cases live in `conformance/cases/*.json` and assert both upstream wire requests and client-visible output. Run `python3 conformance/run.py --backend go --list` to list all cases. To compare the Go router against the Python baseline, place the legacy checkout next to this repository or set `MODEL_ROUTER_LEGACY_REPO` to its path.

## Conformance coverage

The conformance runner reports 616 passing external cases and remains the source of the case count. Those 616 cases include 41 keyless bearer-compatibility contracts. The repository also passes 78 runner unit tests, fresh Go race tests, build checks, `go vet`, and `gofmt` formatting checks.

Regression tests in the root package cover marked Chat request shapes and null boundaries, surrogate-preserving system-message merges, and streamed canonical session hashing. An independent compatibility runtime review found no reachable issues, and its regression tests pass under race detection.

The suite covers native and translated protocol contracts, the static registry, session affinity, response and event IR, and legacy provider headers and aliases. It also verifies catalog routing, llama.cpp discovery, diagnostics, explicit route dialect overrides, and canonical capability listings and overrides. An independent route override review found no implementation issues, and its adversarial regression tests pass with race detection. Fixes and regression tests address all operational read-path review findings.

Commits `223d3a9` and `ff183b2` integrate and route the final 30 Anthropic and Gemini contracts. Commit `5a5c975` corrects the codecs, and commits `2e8f5e3`, `968143b`, and `cdbfdd3` add the contracts. Reviewed coverage includes pre-commit HTTP 502 and 504 errors, flush-failure cancellation, immediate stream termination with slot recovery, post-commit failure framing, model-text framing guards, and validated Responses `tools` and `tool_choice` echoes.

The suite also verifies these translation and state rules:

- The router supports empty annotation arrays.
- Scalar reasoning-token accounting maps Responses `reasoning_tokens` to Gemini `thoughtsTokenCount`. The reverse direction from Gemini to Responses rejects nonzero thought counts and tool use on strict routes.
- Translation from Responses to Chat carries the reasoning-token scalar. Translation from Responses to Anthropic rejects nonzero reasoning usage.
- Cache accounting for the Anthropic and Gemini pair runs once without inventing a Gemini write bucket, matching the reviewed bridge scope.
- Fixture corrections preserve text-before-tool order and ingress-dialect response shapes. Omitting a new optional zero-usage field is not a bug.
- Translated stateful requests retain `unsupported_responses_state`.
- The suite checks exact tool-result content preservation across the reviewed bridges.

## Recorded Python baseline divergences

The conformance suite records divergences against the Python baseline where Go matches items 7 and 8 of [behavior changes compared to Python](transport-and-security.md#behavior-changes-compared-to-python):

- In `chat_to_responses_text`, Python hoists the system message into top-level `instructions` and leaves only the user message in `input`. Go sends the system message as the first input item. Both are documented valid Responses input wire forms, and the easy input-message `type` field is optional. The conformance case asserts Go's ordered `system0` and `user1` form. The Python mismatch is an equivalent wire-form difference and not a product bug.
- In both `*_sse_truncated` cases, Python fabricates `completed` or `[DONE]` after stream truncation. Go ends truncated streams without synthetic success markers.
- In the three tightened pre-commit HTTP 502 cases, Python returns raw-exception HTTP 500 responses, and one case echoes a traceback with an upstream secret. Go returns sanitized HTTP 502 responses.

Tool-result regression tests pin exact content preservation, which covers empty strings and Unicode strings. The tests reject duplicated tool-result content.

Python remains the production baseline. The Go conformance set documents review fixes for large-integer preservation, header isolation, gzip handling, sanitized errors, retry exhaustion, and unaffordable `Retry-After` values. Baseline verification checked Python validation, translation, retry, and `count_tokens` behavior with `env -u MODEL_ROUTER_API_KEY -u MODEL_ROUTER_API_KEYS`. This documentation makes no claims about live-router credentials or logs.

## Development release archives

Build development archives separately from deployment:

```bash
python3 scripts/build_release.py --go go --version 0.1.0-dev --out dist/0.1.0-dev
python3 scripts/check_release.py dist/0.1.0-dev/routerd-0.1.0-dev-linux-amd64.tar.gz --version 0.1.0-dev
```

The build script uses cached dependencies only. It emits deterministic tarballs, `SHA256SUMS`, and a source and toolchain manifest for Linux and macOS on amd64 and arm64. Pass `--target OS/ARCH` to build one target. Use a separate output directory for each version and target set. The build script rejects mixed outputs instead of leaving stale archives behind.

Run `scripts/check_release.py` with the archive that matches your host platform. The smoke checker extracts and executes the packaged binary with an isolated `HOME` and `XDG` environment and a dummy configuration. The Linux amd64 archive passed this check locally, and repeated builds produced identical archive bytes. Other targets cross-compile. CI defines runtime smoke checks for those targets, and those checks have not run locally. Windows is outside the current Unix-cache platform contract.

See [installation and rollback](../RELEASE.md) for deployment and rollback instructions.
