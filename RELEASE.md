# Install and roll back model-router-go

These development archives contain a standalone Go executable, a dummy-key
configuration example, this guide, and bundled upstream notices. Linux and macOS builds target amd64 and
arm64 with CGO disabled. Windows is not declared supported: the discovery cache
requires Unix file ownership and no-follow semantics.

Cross-compilation proves a target builds; only an archive tested on its target
platform has runtime smoke evidence. These artifacts do not establish complete
rewrite parity or live harness acceptance.

## Check an archive

Verify its SHA256 against `SHA256SUMS` from the same trusted release source.
Extract the archive into a fresh directory. Then run:

```sh
./model-router-go --version
./model-router-go --config config.example.toml --validate-config --offline
```

The example has dummy credentials and local placeholder upstreams. Replace the
keys, URLs, and model IDs in a separate private configuration before serving.
For environment credentials, replace `api_key` with `api_key_env`; never publish
your private configuration or credential files in an archive.

The binary needs no Python interpreter or LiteLLM. System CA certificates and
normal operating-system networking remain runtime dependencies. The optional
discovery cache is route-only schema v2; it does not import Python v1 files.

## Stage the switch

Keep the Python installation, launcher and private configuration intact. Copy
the configuration to a separate private TOML or JSON file. Convert YAML and
replace SDK-only provider options with the documented portable settings. The
[configuration reference](README.md) lists accepted options and route behavior.
Use `--no-default-env` with an explicit `--env-file` when staging requires an
isolated credential set. Keep ingress keys separate from provider credentials.
Keyless bearer session compatibility requires a configuration with zero
effective ingress keys.

If you enable `[discovery_cache]`, give Go its own cache path before starting
either the readiness check or the listener. Keep the Python cache path in the
original config so rollback can reuse it.

```sh
./model-router-go --config ./router-go.toml --validate-config --offline
./model-router-go --config ./router-go.toml --check-readiness
./model-router-go --config ./router-go.toml --host 127.0.0.1 --port 18080
```

Offline validation checks configuration without contacting upstreams. The
online readiness check makes bounded discovery probes. Exercise each selected
harness workflow against the staging listener, including streaming, tools,
session affinity and failure behavior. Check translated requests for diagnosed
losses before migrating a harness that relies on exact wire semantics.
Offline fake-upstream conformance does not establish real-provider acceptance.
Enable a search backend only after its binding and limits are configured and
its deployment smoke checks pass.

### Harness checks

Exercise the relevant wire contracts against fake upstreams before running the
installed launchers against the staging listener.

| Harness flow | Wire contract to verify |
| --- | --- |
| pi adapter | Send `x-session-id` with generation requests and verify the configured upstream session header. |
| Muse | Read `/v1/muse-code/models`, then generate through `/v1/responses` using a listed model. |
| Cline keyless Chat | Send the launch affinity token as `Authorization: Bearer …`. Verify the selected session and the provider credential on Chat generation. |
| Antigravity Gemini API-key or ACP gateway | For API-key mode, send the launch affinity token on Gemini generation. For ACP, verify top-level `sessionId` is consumed and its stable identity takes precedence over the launch token. |

The fake-upstream suite covers these request shapes. Run it from the repository root with
`python3 conformance/run.py --backend go --go-bin /path/to/model-router-go`.
For live staging, run `python3 scripts/check_live_routes.py --origin
http://127.0.0.1:18080 --route responses=YOUR_CHAT_MODEL --route
chat=YOUR_RESPONSES_MODEL`. This sends one small JSON request and one stream
per selected route and checks for visible output and successful completion.
Then verify your installed launcher, selected provider and streaming workflow
on the staging listener. OAuth, Vertex, and Antigravity account-based routing
are outside this release profile.

Once those checks pass, stop the old listener and change the harness base URL
or launcher to the Go listener. Never run duplicate paid generations to compare
implementations. SIGTERM gives Go a bounded ten-second drain.

## Roll back

Stop the Go listener and restore the previous Python launcher and harness URL.
Use the original Python configuration and cache path; do not rewrite either to
match Go's cache. There is no conversation database to migrate. Upstream-owned
state and credentials remain with the selected upstream.

Do not delete the previous binary or environment until the selected deployment's
complete workflows have passed. A release artifact is not permission to change
an existing deployment automatically.
