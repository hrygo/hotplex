#!/usr/bin/env python3
"""Check that the worker runtime lock is a lock, and that the packer obeys it.

The offline bundle used to resolve OpenCode and Oh My OpenAgent from whatever
upstream published at build time, and silently substituted a different npm
version when a pinned one was missing. A bundle built that way is not
reproducible and nothing downstream can say which runtime it contains.

This check has two halves:
  1. the manifest itself pins concrete versions and real digests
  2. scripts/pack-offline-bundle.sh actually resolves through the lock — proven
     by running its --dry-run, not by reading it and hoping

Usage:
    python3 scripts/ci/check_runtime_lock.py [--repo-root .]
"""

from __future__ import annotations

import argparse
import json
import re
import subprocess
import sys
from pathlib import Path

SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
# npm publishes base64 sha512; only the prefix and a non-empty payload matter
# for a structural check.
SHA512_RE = re.compile(r"^sha512-[A-Za-z0-9+/]+={0,2}$")
SEMVER_RE = re.compile(r"^\d+\.\d+\.\d+$")

# A version that is not a concrete release is not a pin.
VAGUE_VERSIONS = {"", "latest", "unknown", "*", "x", "main", "HEAD"}


def validate_manifest(lock: dict, platforms: list[str]) -> list[str]:
    problems: list[str] = []

    if lock.get("schema_version") != 1:
        problems.append(
            f"lock: unsupported schema_version {lock.get('schema_version')!r}"
        )

    runtimes = lock.get("runtimes")
    if not isinstance(runtimes, dict):
        return problems + ["lock: missing 'runtimes' object"]

    opencode = runtimes.get("opencode") or {}
    omo = runtimes.get("oh-my-opencode") or {}

    for label, runtime in (("opencode", opencode), ("oh-my-opencode", omo)):
        version = runtime.get("version")
        if version in VAGUE_VERSIONS:
            problems.append(
                f"{label}: version {version!r} is not pinned; the lock must name "
                "a concrete release"
            )
        elif not SEMVER_RE.match(str(version)):
            problems.append(f"{label}: version {version!r} is not a plain semver")

    assets = opencode.get("assets") or {}
    for platform in platforms:
        asset = assets.get(platform)
        if not isinstance(asset, dict):
            problems.append(f"opencode: no asset pinned for platform {platform!r}")
            continue
        digest = asset.get("sha256", "")
        if not SHA256_RE.match(str(digest)):
            problems.append(
                f"opencode: platform {platform!r} has sha256 {digest!r}, which is "
                "not a 64-character hex digest"
            )
        if not asset.get("url"):
            problems.append(f"opencode: platform {platform!r} has no download url")

    packages = omo.get("packages") or {}
    if not packages:
        problems.append("oh-my-opencode: no packages pinned")
    for name, package in sorted(packages.items()):
        integrity = str((package or {}).get("integrity", ""))
        if not SHA512_RE.match(integrity):
            problems.append(
                f"oh-my-opencode: {name} has integrity {integrity!r}, which is not "
                "an npm sha512 integrity"
            )
        pinned = (package or {}).get("version")
        if pinned != omo.get("version"):
            problems.append(
                f"oh-my-opencode: {name} is pinned at {pinned!r} but the runtime "
                f"version is {omo.get('version')!r}; a lock must pin one version"
            )

    return problems


def check_packer(repo_root: Path, lock_path: Path) -> list[str]:
    """Prove the packer resolves through the lock, by running it."""
    problems: list[str] = []
    script = repo_root / "scripts" / "pack-offline-bundle.sh"
    if not script.exists():
        return [f"{script} is missing"]

    lock = json.loads(lock_path.read_text(encoding="utf-8"))
    platforms = lock.get("platforms") or sorted((lock["runtimes"]["opencode"]["assets"]))

    def run(*args: str) -> subprocess.CompletedProcess:
        return subprocess.run(
            ["bash", str(script), "--dry-run", "--lock", str(lock_path), *args],
            capture_output=True,
            text=True,
            timeout=120,
        )

    for platform in platforms:
        result = run("--platform", platform)
        if result.returncode != 0:
            problems.append(
                f"packer: --dry-run failed for locked platform {platform!r}: "
                f"{result.stderr.strip()[:200]}"
            )
            continue
        if "LOCKED" not in result.stdout:
            problems.append(
                f"packer: {platform!r} did not report LOCKED mode; the default "
                "must be the lock, not an upstream lookup"
            )
        digest = lock["runtimes"]["opencode"]["assets"][platform]["sha256"]
        if digest not in result.stdout:
            problems.append(
                f"packer: --dry-run for {platform!r} did not surface the locked "
                f"digest {digest[:16]}…"
            )

    # A platform nobody pinned must fail closed rather than download whatever
    # happens to exist for it.
    unpinned = run("--platform", "hotplex-nonexistent-platform")
    if unpinned.returncode == 0:
        problems.append(
            "packer: an unpinned platform was accepted; the lock must fail closed"
        )

    missing_lock = subprocess.run(
        [
            "bash",
            str(script),
            "--dry-run",
            "--lock",
            str(repo_root / "configs" / "no-such-lock.json"),
            "--platform",
            platforms[0],
        ],
        capture_output=True,
        text=True,
        timeout=120,
    )
    if missing_lock.returncode == 0:
        problems.append(
            "packer: a missing lock file was accepted; locked mode must not "
            "silently degrade to resolving from upstream"
        )

    return problems


def check(repo_root: Path) -> list[str]:
    lock_path = repo_root / "configs" / "worker-runtime-lock.json"
    if not lock_path.exists():
        return [
            "configs/worker-runtime-lock.json is missing; offline bundles would "
            "resolve runtime versions from upstream at build time"
        ]

    lock = json.loads(lock_path.read_text(encoding="utf-8"))
    platforms = lock.get("platforms") or sorted(lock.get("runtimes", {}).get("opencode", {}).get("assets", {}))
    if not platforms:
        return ["lock: no platforms declared"]

    return validate_manifest(lock, platforms) + check_packer(repo_root, lock_path)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo-root", default=".", type=Path)
    args = parser.parse_args()

    try:
        problems = check(args.repo_root.resolve())
    except (json.JSONDecodeError, KeyError, OSError) as exc:
        print(f"::error::runtime lock could not be read: {exc}")
        return 1

    for problem in problems:
        print(f"::error::{problem}")
    if problems:
        print(f"{len(problems)} runtime-lock problem(s) found.")
        return 1
    print("Worker runtime lock is pinned and the packer obeys it.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
