"""Build deterministic, self-contained Go archives without downloading modules."""
from __future__ import annotations

import argparse
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import re
import subprocess
import tarfile
import tempfile


ROOT = Path(__file__).resolve().parents[1]
TARGETS = ("linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64")


def run(command: list[str], env: dict[str, str]) -> str:
    return subprocess.check_output(command, cwd=ROOT, env=env, text=True).strip()


def archive(path: Path, prefix: str, files: dict[str, tuple[bytes, int]]) -> None:
    # Fixed owner, time, modes, ordering and gzip header make repackaging stable.
    with path.open("wb") as raw:
        with gzip.GzipFile(fileobj=raw, mode="wb", filename="", mtime=0) as zipped:
            with tarfile.open(fileobj=zipped, mode="w", format=tarfile.USTAR_FORMAT) as tar:
                for name, (data, mode) in sorted(files.items()):
                    info = tarfile.TarInfo(f"{prefix}/{name}")
                    info.size, info.mode, info.mtime = len(data), mode, 0
                    info.uid = info.gid = 0
                    info.uname = info.gname = ""
                    tar.addfile(info, io.BytesIO(data))


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--go", default="go", help="Go 1.24+ executable")
    parser.add_argument("--version", required=True)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--target", choices=TARGETS, action="append")
    args = parser.parse_args()
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9.+_-]{0,95}", args.version):
        parser.error("version must be a short filename-safe label")
    out = args.out.resolve()
    out.mkdir(parents=True, exist_ok=True)
    targets = sorted(set(args.target or TARGETS))
    expected_files = {"manifest.json", "SHA256SUMS"} | {
        f"routerd-{args.version}-{target.replace('/', '-')}.tar.gz"
        for target in targets
    }
    if any(path.name not in expected_files for path in out.iterdir()):
        parser.error("output contains another release or target set; use an empty directory")
    env = dict(os.environ, CGO_ENABLED="0", GOENV="off", GOFLAGS="",
               GOTOOLCHAIN="local", GOWORK="off", GOPROXY="off", GOSUMDB="off")
    # Never make a foreign-architecture go executable try to run on this host.
    env.pop("GOOS", None)
    env.pop("GOARCH", None)
    env.pop("GOARM", None)
    env.pop("GOAMD64", None)
    goroot = Path(run([args.go, "env", "GOROOT"], env))
    toml = json.loads(run([args.go, "list", "-m", "-json", "github.com/pelletier/go-toml/v2"], env))
    notices = (
        "Go standard library and runtime\n\n" + (goroot / "LICENSE").read_text()
        + f"\n\n{toml['Path']} {toml['Version']}\n\n"
        + (Path(toml["Dir"]) / "LICENSE").read_text()
    ).encode()
    manifest = {
        "version": args.version,
        "source_revision": run(["git", "rev-parse", "HEAD"], env),
        "source_dirty": bool(run(["git", "status", "--porcelain", "--", "."], env)),
        "go_version": run([args.go, "version"], env),
        "dependencies": {toml["Path"]: toml["Version"]},
        "artifacts": [],
    }
    with tempfile.TemporaryDirectory(prefix=".release-", dir=out) as directory:
        staging = Path(directory)
        for target in targets:
            goos, goarch = target.split("/")
            build_env = dict(env, GOOS=goos, GOARCH=goarch, GOAMD64="v1")
            binary = staging / "routerd"
            subprocess.run([
                args.go, "build", "-trimpath", "-buildvcs=false",
                "-ldflags", f"-s -w -buildid= -X github.com/arhammahajan/router/internal/server.BuildVersion={args.version}",
                "-o", str(binary), "./cmd/routerd",
            ], cwd=ROOT, env=build_env, check=True)
            prefix = f"routerd-{args.version}-{goos}-{goarch}"
            packed = staging / f"{prefix}.tar.gz"
            archive(packed, prefix, {
                "routerd": (binary.read_bytes(), 0o755),
                "config.example.toml": ((ROOT / "config.example.toml").read_bytes(), 0o600),
                "README.md": ((ROOT / "RELEASE.md").read_bytes(), 0o644),
                "THIRD_PARTY_NOTICES": (notices, 0o644),
            })
            manifest["artifacts"].append({
                "file": packed.name, "target": target,
                "sha256": hashlib.sha256(packed.read_bytes()).hexdigest(),
            })
            print(f"built {packed.name}", flush=True)
        checksums = "".join(f"{item['sha256']}  {item['file']}\n"
                            for item in manifest["artifacts"])
        (staging / "SHA256SUMS").write_text(checksums)
        (staging / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
        for item in manifest["artifacts"]:
            os.replace(staging / item["file"], out / item["file"])
        os.replace(staging / "SHA256SUMS", out / "SHA256SUMS")
        os.replace(staging / "manifest.json", out / "manifest.json")


if __name__ == "__main__":
    main()
