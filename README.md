# router

`router` is a Go gateway for OpenAI Chat Completions, OpenAI Responses, Anthropic Messages, Gemini, and local llama.cpp models. It forwards same-dialect requests natively and translates across all twelve cross-dialect pairs.

The repository builds with Go 1.24 at the root module. It uses `github.com/pelletier/go-toml/v2 v2.2.4` for TOML configuration parsing and the Go standard library for everything else.

This Go router daemon supersedes the previous Python implementation and is the production service.

## Quick start

Build and run the router binary from the repository root:

```bash
go build -o routerd ./cmd/routerd
./routerd --config router-dev.toml
./routerd --validate-config --config router-dev.toml
./routerd --check-readiness --config router-dev.toml
./routerd --version
```

The router reads TOML and JSON configuration files. For local tests, copy [config.example.toml](config.example.toml) and keep dummy keys rather than pointing at a file with real credentials.

Flags `--host` and `--port` override `server.host` and `server.port`. On `SIGINT` or `SIGTERM`, the server stops admitting new work, drains active requests for up to 10 seconds, and closes remaining connections. See [Configuration and CLI](docs/configuration.md) for config discovery, environment loading, exit codes, and all supported sections.

## Companion tools

The `router` CLI in `cmd/router` manages and inspects the router daemon (systemd-first with direct-spawn fallback), lists/picks models, validates configs offline, streams logs, and summarizes accounting usage:

```bash
go build -o router ./cmd/router
./router start [--config PATH] [--foreground] [--wait DURATION]
./router stop
./router restart [--wait DURATION]
./router status [--json]
./router models [--json] [--pick]
./router validate [--config PATH]
./router logs [--follow] [-n LINES]
./router usage [--days N] [--by day|model] [--json] [PATH]
```

Run `router help` to print the full command list.

## Endpoints

| Endpoint | Behavior |
| --- | --- |
| `POST /v1/chat/completions`, `/chat/completions` | Chat Completions native passthrough or translation to Anthropic, Responses, or Gemini |
| `POST /v1/responses`, `/responses` | Responses native passthrough or translation to Chat, Anthropic, or Gemini |
| `POST /v1/messages`, `/messages` | Anthropic Messages native passthrough or translation to Chat, Responses, or Gemini |
| `POST /v1/messages/count_tokens`, `/messages/count_tokens` | Anthropic token-count passthrough when declared or first-party |
| `POST /v1beta/models/{name}:generateContent`, `:streamGenerateContent` | Gemini native passthrough or translation to Chat, Responses, or Anthropic |
| `GET /v1/models`, `/models` | Header-authenticated canonical model list with `metadata.router` |
| `GET /muse-code/models`, `/v1/muse-code/models` | Header-authenticated Muse model projection from cached snapshots |
| `GET /v1beta/models`, `/v1beta/models/{id}` | Gemini model discovery with header or `?key=` authentication |
| `GET /`, `/livez`, `/readyz`, `/diagnostics` | Build info, liveness check, readiness check, and authenticated diagnostics |

See [Endpoints and protocol translation](docs/endpoints-and-translation.md) for versioned path aliases, Gemini query rules, strict and lossy translation policies, reasoning controls, and streaming guarantees.

## Documentation

- [Configuration and CLI](docs/configuration.md) documents CLI flags, exit codes, `[server]`, `[retry]`, `[providers.*]`, `[[models]]`, capability overrides, `[catalog]`, `[llama_cpp]`, `[discovery_cache]`, `[accounting]`, `[search]`, and diagnostics.
- [Endpoints and protocol translation](docs/endpoints-and-translation.md) documents the shared IR architecture, HTTP endpoints, Gemini 4 extensions, reasoning translation, and all six bidirectional dialect pairs.
- [Transport, security, and scope](docs/transport-and-security.md) documents client and upstream authentication, header isolation, retries, admission bounds, behavior changes compared to Python, known limits, and deferred milestones.
- [Verification and release archives](docs/verification-and-releases.md) documents Go test commands and development release builds.
- [Feature tracker](docs/features.md) tracks shipped and planned work for orchestration and the router.
- [Install and roll back](RELEASE.md) documents archive verification, staged cutover from Python, and rollback.

## Verification

```bash
go build -o /tmp/routerd ./cmd/routerd
go vet ./... && gofmt -l internal cmd
go test -count=1 ./...
go test -race -count=1 ./...
```

See [Verification and release archives](docs/verification-and-releases.md) for local checks and release archive builds.
