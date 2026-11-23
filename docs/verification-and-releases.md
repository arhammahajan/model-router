# Verification and release archives

This document covers local verification commands and development release archive builds.

## Local checks

Run these commands from the repository root to build the Go binary, run static checks, and execute Go tests:

```bash
go build -o /tmp/routerd ./cmd/routerd
go vet ./... && gofmt -l internal cmd
go test -count=1 ./...
go test -race -count=1 ./...
```

Each command verifies a specific stage:

| Command | Verification target |
| --- | --- |
| `go build -o /tmp/routerd ./cmd/routerd` | Builds the Go binary at `/tmp/routerd`. |
| `go vet ./... && gofmt -l internal cmd` | Runs static analysis and checks formatting. The `gofmt -l` output must be empty. |
| `go test -count=1 ./...` | Runs unit tests and fake-upstream tests without test caching. |
| `go test -race -count=1 ./...` | Runs concurrency checks under the Go race detector. |
## Go verification

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

Tool-result regression tests pin exact content preservation, which covers empty strings and Unicode strings. The tests reject duplicated tool-result content.

## Toolchain notes

The router root module and CLIProxyAPI v8 SDK dependency require Go 1.26.10 or later. Building the `opencode-go-cliproxyapi` shared library requires CGO (`-buildmode=c-shared`). Release archives use CGO-disabled builds and can connect to an external CLIProxyAPI service. To load the OpenCode plugin in embedded mode, build with `CGO_ENABLED=1` and install the plugin binary in CLIProxyAPI's configured plugin directory.
## Development release archives

Build development archives separately from deployment:

```bash
python3 scripts/build_release.py --go go --version 0.1.0-dev --out dist/0.1.0-dev
python3 scripts/check_release.py dist/0.1.0-dev/routerd-0.1.0-dev-linux-amd64.tar.gz --version 0.1.0-dev
```

The build script uses cached dependencies only. It emits deterministic tarballs, `SHA256SUMS`, and a source and toolchain manifest for Linux and macOS on amd64 and arm64. Pass `--target OS/ARCH` to build one target. Use a separate output directory for each version and target set. The build script rejects mixed outputs instead of leaving stale archives behind.

Run `scripts/check_release.py` with the archive that matches your host platform. The smoke checker extracts and executes the packaged binary with an isolated `HOME` and `XDG` environment and a dummy configuration. The Linux amd64 archive passed this check locally, and repeated builds produced identical archive bytes. Other targets cross-compile. CI defines runtime smoke checks for those targets, and those checks have not run locally. Windows is outside the current Unix-cache platform contract.

See [installation and rollback](../RELEASE.md) for deployment and rollback instructions.
