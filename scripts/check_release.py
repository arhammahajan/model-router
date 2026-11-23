"""Extract one host archive and verify its actual CLI using only dummy config."""
from __future__ import annotations

import argparse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path, PurePosixPath
import socket
import subprocess
import tarfile
import tempfile
import threading
import time
import urllib.error
import urllib.request


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("archive", type=Path, help="archive for this host OS/architecture")
    parser.add_argument("--version", required=True)
    args = parser.parse_args()
    with tempfile.TemporaryDirectory(prefix="router-release-smoke-") as directory:
        home = Path(directory)
        with tarfile.open(args.archive, "r:gz") as packed:
            members = packed.getmembers()
            prefixes = {PurePosixPath(item.name).parts[0] for item in members}
            if len(prefixes) != 1 or len(members) != 5:
                raise ValueError("expected exactly five files under one archive directory")
            prefix = next(iter(prefixes))
            allowed = {"routerd", "config.example.toml", "cliproxyapi.example.yaml", "README.md", "THIRD_PARTY_NOTICES"}
            found = set()
            for item in members:
                path = PurePosixPath(item.name)
                if (not item.isfile() or path.is_absolute() or len(path.parts) != 2
                        or path.parts[0] != prefix or path.parts[1] not in allowed
                        or path.parts[1] in found or item.size > 128 * 1024 * 1024):
                    raise ValueError("unexpected archive entry")
                found.add(path.parts[1])
                stream = packed.extractfile(item)
                if stream is None:
                    raise ValueError("missing archive file")
                dest = home / path.parts[1]
                dest.write_bytes(stream.read())
                dest.chmod(0o700 if dest.name == "routerd" else 0o600)
        binary = str(home / "routerd")
        config = str(home / "config.example.toml")
        # No inherited API keys, dotenv, provider proxies, credential paths or cache.
        env = {"HOME": str(home), "XDG_CONFIG_HOME": str(home),
               "XDG_CACHE_HOME": str(home), "PATH": os.defpath}

        def invoke(flags: list[str], code: int) -> str:
            result = subprocess.run([binary, *flags], cwd=home, env=env,
                                    text=True, capture_output=True, timeout=10)
            if result.returncode != code:
                raise AssertionError(f"CLI {flags}: exit {result.returncode}, expected {code}")
            return result.stdout.strip()

        if invoke(["--version"], 0) != f"routerd {args.version}":
            raise AssertionError("archive executable has the wrong version")
        valid = invoke(["--config", config, "--validate-config", "--offline"], 0)
        if json.loads(valid) != {"status": "valid", "network_checked": False}:
            raise AssertionError("offline validation contract changed")
        offline = invoke(["--config", config, "--check", "--offline"], 4)
        if json.loads(offline) != {"status": "indeterminate", "network_checked": False,
                                  "reason": "offline_readiness_requires_discovery"}:
            raise AssertionError("offline readiness contract changed")
        class ProxyHandler(BaseHTTPRequestHandler):
            def do_GET(self) -> None:
                if (self.path != "/v1/models"
                        or self.headers.get("Authorization") != "Bearer proxy-smoke-key"):
                    self.send_error(403)
                    return
                body = json.dumps({"data": [{"id": "opencode-go/smoke-model"}]}).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *_args: object) -> None:
                pass

        # Exercise proxy discovery against a local fixture with dummy credentials.
        proxy = ThreadingHTTPServer(("127.0.0.1", 0), ProxyHandler)
        proxy_thread = threading.Thread(target=proxy.serve_forever, daemon=True)
        proxy_thread.start()
        smoke_config = home / "smoke.toml"
        smoke_config.write_text(
            '[server]\napi_keys = ["replace-this-local-key"]\n'
            '[cliproxyapi]\n'
            f'base_url = "http://127.0.0.1:{proxy.server_port}/v1"\n'
            'api_key = "proxy-smoke-key"\n'
        )
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            port = listener.getsockname()[1]
        process = subprocess.Popen(
            [binary, "--config", str(smoke_config), "--host", "127.0.0.1", "--port", str(port)],
            cwd=home, env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

        def get(path: str, authenticated: bool = False) -> tuple[int, dict]:
            headers = {"Authorization": "Bearer replace-this-local-key"} if authenticated else {}
            request = urllib.request.Request(f"http://127.0.0.1:{port}{path}", headers=headers)
            try:
                response = opener.open(request, timeout=1)
            except urllib.error.HTTPError as error:
                response = error
            with response:
                return response.status, json.loads(response.read(1024 * 1024))

        try:
            deadline = time.monotonic() + 10
            while True:
                if process.poll() is not None:
                    raise AssertionError("archive server exited before readiness")
                try:
                    status, ready = get("/readyz")
                    if status == 200 and ready == {"status": "ready"}:
                        break
                except (OSError, urllib.error.URLError):
                    pass
                if time.monotonic() >= deadline:
                    raise AssertionError("archive server did not become ready")
                time.sleep(0.02)
            status, index = get("/")
            if status != 200 or index.get("version") != args.version:
                raise AssertionError("HTTP index and CLI release versions disagree")
            status, _ = get("/v1/models")
            if status != 401:
                raise AssertionError("archive model listing bypassed authentication")
            status, models = get("/v1/models", authenticated=True)
            if status != 200 or [row["id"] for row in models.get("data", [])] != ["opencode-go/smoke-model"]:
                raise AssertionError("archive model listing lost the discovered proxy route")
        finally:
            proxy.shutdown()
            proxy.server_close()
            proxy_thread.join(timeout=5)
            if process.poll() is None:
                process.terminate()
            try:
                result = process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
                raise AssertionError("archive server did not stop promptly") from None
        if result != 0:
            raise AssertionError("archive server did not shut down cleanly")
        print(f"archive CLI, version, cached HTTP reads, auth and shutdown: OK ({args.version})")


if __name__ == "__main__":
    main()
