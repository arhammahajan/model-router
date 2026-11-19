#!/bin/sh
# End-to-end smoke gate for the harness CLI wiring (cmd/harness).
# Builds the binary, runs it against a sandboxed HOME/XDG environment with a
# stub router HTTP server, and checks the command surface end to end.
# Never touches the real home; never launches real agent binaries.
set -eu

repo=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)
export PATH="$HOME/sdk/go/bin:$PATH"

tmp=$(mktemp -d /tmp/opencode/harness-smoke.XXXXXX)
trap 'rm -rf "$tmp"' EXIT
bin="$tmp/harness"

cd "$repo"
go build -o "$bin" ./cmd/harness

fail() { printf 'smoke: FAIL: %s\n' "$1" >&2; exit 1; }

# --- sandbox environment ----------------------------------------------------
home="$tmp/home"
mkdir -p "$home" "$tmp/xdg/config" "$tmp/xdg/data" "$tmp/xdg/state" "$tmp/dots"
export HOME="$home"
export XDG_CONFIG_HOME="$tmp/xdg/config"
export XDG_DATA_HOME="$tmp/xdg/data"
export XDG_STATE_HOME="$tmp/xdg/state"
unset HARNESS_AGENT HARNESS_DOTFILES HARNESS_ROUTER_BIN HARNESS_ROUTER_CONFIG || true

# Fake dotfiles tree for profile seeding.
mkdir -p "$tmp/dots/claude" "$tmp/dots/antigravity"
printf 'model = "gpt-6-luna"\n' > "$tmp/dots/claude/settings.toml"
export HARNESS_DOTFILES="$tmp/dots"

# --- stub router --------------------------------------------------------------
cat > "$tmp/stub.py" <<'PY'
import json
from http.server import BaseHTTPRequestHandler, HTTPServer

class Handler(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass
    def do_GET(self):
        routes = {
            "/": {"name": "model-router-go", "version": "0.1.0-smoke"},
            "/livez": {"status": "ok"},
            "/readyz": {"status": "ready"},
            "/v1/models": {"data": [{"id": "glm-5.3", "owned_by": "test",
                                      "metadata": {"router": {}}}]},
        }
        body = json.dumps(routes.get(self.path, {})).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

HTTPServer(("127.0.0.1", 0), Handler) if False else None
import socketserver, threading
server = socketserver.TCPServer(("127.0.0.1", 0), Handler)
print(server.server_address[1], flush=True)
server.serve_forever()
PY
python3 "$tmp/stub.py" > "$tmp/port" &
stub_pid=$!
trap 'kill $stub_pid 2>/dev/null; rm -rf "$tmp"' EXIT
for _ in 1 2 3 4 5 6 7 8 9 10; do
    [ -s "$tmp/port" ] && break
    sleep 0.2
done
[ -s "$tmp/port" ] || fail "stub router did not start"
port=$(cat "$tmp/port")
export MODEL_ROUTER_URL="http://127.0.0.1:$port"
export MODEL_ROUTER_API_KEY="smoke-key"

# --- command surface ---------------------------------------------------------
"$bin" --version | grep -q "harness" || fail "--version"
"$bin" help >/dev/null || fail "help"
"$bin" agents | grep -q "claude" || fail "agents lists claude"
"$bin" router status --json | grep -q '"Livez": *true' || fail "router status json livez"
"$bin" router status | grep -q "live:     true" || fail "router status text"
"$bin" router models | grep -q "glm-5.3" || fail "router models"

# Profile lifecycle with shadow home + seeding.
"$bin" profile create claude_alt --agent claude --display-name "Alt Claude" \
    --seed-from claude || fail "profile create"
grep -q '"agent": *"claude"' "$tmp/home/.model-router/profiles/claude/claude_alt/profile.json" || fail "profile json saved"
[ -f "$tmp/home/.model-router/profiles/claude/claude_alt/home/settings.toml" ] \
    || fail "shadow home seeded from dots"
"$bin" profile list | grep -q "^claude_alt	claude	Alt Claude" || fail "profile list"
"$bin" profile show claude_alt | grep -q "source:       store" || fail "profile show"
"$bin" profile describe claude_alt | grep -q "claude" || fail "profile describe"

# Launch path resolves and execs the real binary from PATH; stub it. A
# piped stdin keeps the launch non-interactive (the access picker only
# engages on a terminal). The fake exits 42 for the --in-zj probe so the
# attention child-run path can assert exit-code propagation.
printf '#!/bin/sh\nfor arg in "$@"; do [ "$arg" = "--in-zj" ] && exit 42; done\necho "launched:$*"\n' > "$tmp/claude"
chmod +x "$tmp/claude"
out=$(printf '' | PATH="$tmp:$PATH" "$bin" profile claude_alt --smoke-arg 2>&1) \
    || fail "profile launch failed: $out"
printf '%s\n' "$out" | grep -q -- "--smoke-arg" || fail "profile launch arg passthrough"

# Bare agent launch with HARNESS_AGENT override (T3 drop-in contract).
out=$(HARNESS_AGENT=claude PATH="$tmp:$PATH" "$bin" --from-t3 2>&1) \
    || fail "HARNESS_AGENT launch failed: $out"
printf '%s\n' "$out" | grep -q -- "--from-t3" || fail "HARNESS_AGENT arg passthrough"

# Access levels: preselect, T3 bypass, and bad-level usage errors. The
# smoke has no tty, so the interactive picker is naturally out of scope.
# With a second profile added, `harness claude` asks for explicit selection,
# and `harness claude profile default` launches the unmodified default.
if PATH="$tmp:$PATH" "$bin" claude --smoke-access >/dev/null 2>&1; then
    fail "multi-profile harness claude must require explicit profile selection"
fi
out=$(HARNESS_ACCESS=full PATH="$tmp:$PATH" "$bin" claude profile default --smoke-access 2>&1) \
    || fail "HARNESS_ACCESS launch failed: $out"
printf '%s\n' "$out" | grep -q -- "--dangerously-skip-permissions --smoke-access" \
    || fail "HARNESS_ACCESS=full must prepend claude full-access flags"
out=$(HARNESS_AGENT=claude HARNESS_ACCESS=full PATH="$tmp:$PATH" "$bin" --t3-access 2>&1) \
    || fail "T3 launch failed: $out"
printf '%s\n' "$out" | grep -q -- "--dangerously-skip-permissions" \
    && fail "T3 invocations must never receive access flags"
printf '%s\n' "$out" | grep -q -- "--t3-access" || fail "T3 arg passthrough"
if HARNESS_ACCESS=bogus PATH="$tmp:$PATH" "$bin" claude profile default >/dev/null 2>&1; then
    fail "unknown access level must be a usage error"
fi

# The interactive picker through a real pty: pick full access for claude.
if command -v script >/dev/null 2>&1; then
    out=$(printf '4\n' | PATH="$tmp:$PATH" script -qec "$bin profile claude_alt --smoke-pty" /dev/null 2>&1) \
        || fail "pty picker launch failed: $out"
    printf '%s\n' "$out" | grep -q "Access level for claude" \
        || fail "pty picker did not render the option list"
    printf '%s\n' "$out" | grep -q -- "--dangerously-skip-permissions --smoke-pty" \
        || fail "pty picker must layer the selected access level"
else
    printf 'skipping pty picker check (no script(1))\n'
fi

# Aliases and usage errors.
"$bin" agents | grep -q "antigravity" || fail "agents lists antigravity"
if "$bin" no-such-agent >/dev/null 2>&1; then fail "unknown agent must exit 2"; fi
if "$bin" profile no-such-profile >/dev/null 2>&1; then fail "unknown profile must exit 2"; fi

# Remove with purge.
"$bin" profile remove claude_alt --purge || fail "profile remove"
[ ! -e "$tmp/home/.model-router/profiles/claude/claude_alt" ] || fail "purge removed shadow home"
[ ! -f "$tmp/home/.model-router/profiles/claude/claude_alt/profile.json" ] || fail "profile json still exists"

# The router subcommands: models table (headless) and the accounting usage
# report against a fixture log; doctor renders its stack report.
printf '' | "$bin" models | grep -q "glm-5.3" || fail "harness models table"
mkdir -p "$tmp/state/model-router"
printf '%s\n' '{"ts":1795000000.0,"model":"glm-5.3","provider":"opencode-chat","ingress":"openai-chat","upstream":"openai-chat","status":200,"input_tokens":120,"output_tokens":7,"cached_tokens":30,"duration_ms":42,"stream":false}' \
    > "$tmp/state/model-router/accounting.jsonl"
out=$(XDG_STATE_HOME="$tmp/state" "$bin" usage --by model) || fail "harness usage exit: $out"
printf '%s\n' "$out" | grep -q "glm-5.3" || fail "usage table lists the model"
out=$(XDG_STATE_HOME="$tmp/state" "$bin" usage --by model --json) || fail "harness usage --json exit"
printf '%s\n' "$out" | grep -q "glm-5.3" || fail "usage json lists the model"
"$bin" doctor | grep -q "ok" || fail "harness doctor renders"

# Attention-aware launches: outside Zellij nothing changes (exec path); the
# child-run supervision reports and propagates exit codes inside a pane.
out=$(printf '' | PATH="$tmp:$PATH" "$bin" claude --no-zellij) || fail "exec path launch failed"
code=0
printf '' | ZELLIJ_SESSION_NAME=smoke-main PATH="$tmp:$PATH" "$bin" claude --in-zj >/dev/null 2>&1 || code=$?
[ "$code" = 42 ] || fail "attn wrap must propagate the child exit code (got $code)"

printf 'smoke: OK\n'
