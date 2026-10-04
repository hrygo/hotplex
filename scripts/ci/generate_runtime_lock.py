#!/usr/bin/env python3
"""Regenerate configs/worker-runtime-lock.json from upstream metadata.

This runs deliberately, by hand, when someone decides to move the pinned
worker runtime versions. It is never invoked by a release: a release reads the
committed lock, so the version a bundle ships cannot drift between the moment
the lock was reviewed and the moment the artifact was built.

Every digest recorded here came from the upstream publisher:
  - OpenCode: the `digest` field GitHub publishes for each release asset
  - Oh My OpenAgent: the `dist.integrity` field from the npm registry

Usage:
    python3 scripts/ci/generate_runtime_lock.py [--out configs/worker-runtime-lock.json]
"""

from __future__ import annotations

import argparse
import datetime as dt
import json
import sys
import urllib.request
from pathlib import Path

OPENCODE_REPO = "anomalyco/opencode"
OPENCODE_RELEASES = f"https://api.github.com/repos/{OPENCODE_REPO}/releases"
NPM_VERSION_URL = "https://registry.npmjs.org/{package}/{version}"
USER_AGENT = "hotplex-runtime-lock-generator"

# The platforms scripts/pack-offline-bundle.sh knows how to name. Keeping this
# list explicit is the point: a platform that is not pinned cannot ship.
PLATFORMS = {
    "linux-x64": "opencode-linux-x64.tar.gz",
    "linux-x64-musl": "opencode-linux-x64-musl.tar.gz",
    "linux-arm64": "opencode-linux-arm64.tar.gz",
    "linux-arm64-musl": "opencode-linux-arm64-musl.tar.gz",
    "darwin-arm64": "opencode-darwin-arm64.zip",
    "darwin-x64": "opencode-darwin-x64.zip",
    "windows-x64": "opencode-windows-x64.zip",
    "windows-arm64": "opencode-windows-arm64.zip",
}

OMO_PACKAGES = ["oh-my-opencode"] + [
    f"oh-my-opencode-{name}"
    for name in (
        "linux-x64",
        "linux-x64-musl",
        "linux-x64-baseline",
        "linux-arm64",
        "linux-arm64-musl",
        "darwin-x64",
        "darwin-x64-baseline",
        "darwin-arm64",
        "windows-x64",
        "windows-arm64",
    )
]


def _fetch_json(url: str) -> dict:
    request = urllib.request.Request(url, headers={"User-Agent": USER_AGENT})
    with urllib.request.urlopen(request, timeout=30) as response:
        return json.load(response)


def opencode_lock() -> dict:
    release = _fetch_json(f"{OPENCODE_RELEASES}/latest")
    tag = release["tag_name"]
    version = tag[1:] if tag.startswith("v") else tag
    assets = {asset["name"]: asset for asset in release["assets"]}

    locked: dict[str, dict] = {}
    for platform, filename in sorted(PLATFORMS.items()):
        asset = assets.get(filename)
        if asset is None:
            raise SystemExit(
                f"opencode {version} has no asset {filename}; refusing to write a "
                "partial lock that would let an unpinned platform through"
            )
        digest = asset.get("digest") or ""
        if not digest.startswith("sha256:"):
            raise SystemExit(
                f"{filename} has no sha256 digest upstream "
                f"(got {digest!r}); refusing to lock an unverifiable artifact"
            )
        locked[platform] = {
            "asset": filename,
            "sha256": digest.removeprefix("sha256:"),
            "url": (
                f"https://github.com/{OPENCODE_REPO}/releases/download/{tag}/{filename}"
            ),
        }

    return {
        "version": version,
        "tag": tag,
        "source": f"https://github.com/{OPENCODE_REPO}/releases",
        "assets": locked,
    }


def latest_npm_version(package: str) -> str:
    meta = _fetch_json(f"https://registry.npmjs.org/{package}")
    latest = meta.get("dist-tags", {}).get("latest")
    if not latest:
        raise SystemExit(f"npm registry reports no dist-tags.latest for {package}")
    return latest


def oh_my_opencode_lock() -> dict:
    version = latest_npm_version("oh-my-opencode")
    packages: dict[str, dict] = {}
    for package in OMO_PACKAGES:
        meta = _fetch_json(NPM_VERSION_URL.format(package=package, version=version))
        integrity = meta.get("dist", {}).get("integrity")
        if not integrity:
            raise SystemExit(
                f"{package}@{version} publishes no dist.integrity; refusing to "
                "lock a package whose bytes cannot be verified"
            )
        packages[package] = {
            "version": meta.get("version", version),
            "integrity": integrity,
            "tarball": meta.get("dist", {}).get("tarball", ""),
        }
    return {
        "version": version,
        "registry": "https://registry.npmjs.org",
        "packages": packages,
    }


def build() -> dict:
    return {
        "schema_version": 1,
        "description": (
            "Pinned worker runtime versions for offline bundles. Regenerate with "
            "scripts/ci/generate_runtime_lock.py; a release reads this file and "
            "never resolves a version at build time."
        ),
        "platforms": sorted(PLATFORMS),
        "runtimes": {
            "opencode": opencode_lock(),
            "oh-my-opencode": oh_my_opencode_lock(),
        },
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--out",
        type=Path,
        default=Path("configs/worker-runtime-lock.json"),
        help="Where to write the lock (default: configs/worker-runtime-lock.json)",
    )
    parser.add_argument(
        "--stdout",
        action="store_true",
        help="Print the lock instead of writing it",
    )
    args = parser.parse_args()

    lock = build()
    lock["generated_at"] = (
        dt.datetime.now(dt.timezone.utc).replace(microsecond=0).isoformat()
    )
    text = json.dumps(lock, indent=2, sort_keys=False) + "\n"

    if args.stdout:
        sys.stdout.write(text)
        return 0

    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(text, encoding="utf-8")
    print(f"wrote {args.out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
