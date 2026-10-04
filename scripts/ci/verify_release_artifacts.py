#!/usr/bin/env python3
"""Verify a set of release artifacts the way a downloader would have to.

F1 made every artifact declare the source SHA it was built from. That claim is
worthless unless something checks it, and "the release job wrote the file" is
not a check: the same process that produced the artifacts also produced the
manifest that describes them.

This script verifies a directory as an outside party would:
  - every checksum in checksums.txt matches the bytes on disk (tamper fails)
  - source-sha.txt and every .provenance agree with the expected SHA, and with
    each other (a wrong SHA fails)
  - each shipped artifact has a CycloneDX SBOM (a missing one fails)
  - signature and attestation records are present (a missing one fails)

It reads only files. It never contacts a registry, a keyserver or GitHub, so
the same command works offline against a directory someone just downloaded.

Usage:
    python3 scripts/ci/verify_release_artifacts.py --dir dist --expect-sha <sha>
"""
from __future__ import annotations

import argparse
import hashlib
import json
import sys
from pathlib import Path

SBOM_SUFFIXES = (".sbom.json", ".cdx.json")
# What actually ships. Provenance sidecars and the checksum file are metadata
# about these, not products in their own right.
ARTIFACT_PATTERNS = ("hotplex-*.tar.gz", "hotplex-*.zip")


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def shipped_artifacts(directory: Path) -> list[Path]:
    found: set[Path] = set()
    for pattern in ARTIFACT_PATTERNS:
        found.update(directory.glob(pattern))
    return sorted(found)


def verify(directory: Path, expected_sha: str | None) -> list[str]:
    problems: list[str] = []

    if not directory.is_dir():
        return [f"{directory} is not a directory"]

    artifacts = shipped_artifacts(directory)
    if not artifacts:
        return [f"{directory} contains no hotplex-* release artifacts"]

    # ── Checksums ──────────────────────────────────────────────────────
    checksums = directory / "checksums.txt"
    if not checksums.exists():
        problems.append("checksums.txt is missing; a downloader cannot verify anything")
    else:
        listed: dict[str, str] = {}
        for lineno, line in enumerate(checksums.read_text(encoding="utf-8").splitlines(), 1):
            line = line.strip()
            if not line:
                continue
            parts = line.split(None, 1)
            if len(parts) != 2:
                problems.append(f"checksums.txt:{lineno} is not '<sha256>  <name>'")
                continue
            listed[parts[1].strip().lstrip("*")] = parts[0]

        for artifact in artifacts:
            name = artifact.name
            if name not in listed:
                problems.append(f"{name} is shipped but absent from checksums.txt")
                continue
            actual = sha256_file(artifact)
            if actual != listed[name]:
                problems.append(
                    f"{name} does not match checksums.txt: expected "
                    f"{listed[name]}, got {actual}"
                )

        for name in listed:
            if not (directory / name).exists():
                problems.append(f"checksums.txt lists {name}, which is not present")

    # ── Source SHA ─────────────────────────────────────────────────────
    source_sha_file = directory / "source-sha.txt"
    declared: str | None = None
    if not source_sha_file.exists():
        problems.append("source-sha.txt is missing")
    else:
        fields = dict(
            line.split("=", 1)
            for line in source_sha_file.read_text(encoding="utf-8").splitlines()
            if "=" in line
        )
        declared = fields.get("source_sha")
        if not declared:
            problems.append("source-sha.txt declares no source_sha")

    if expected_sha and declared and declared != expected_sha:
        problems.append(
            f"source-sha.txt declares {declared}, but the release was validated "
            f"at {expected_sha}"
        )

    # ── Per-artifact provenance ────────────────────────────────────────
    provenance_files = sorted(directory.glob("hotplex-*.provenance"))
    if not provenance_files:
        problems.append(
            "no .provenance files found; artifacts cannot be tied to a source SHA"
        )
    for prov in provenance_files:
        fields = dict(
            line.split("=", 1)
            for line in prov.read_text(encoding="utf-8").splitlines()
            if "=" in line
        )
        sha = fields.get("source_sha")
        if not sha:
            problems.append(f"{prov.name} declares no source_sha")
            continue
        if expected_sha and sha != expected_sha:
            problems.append(
                f"{prov.name} declares {sha}, but the release was validated at "
                f"{expected_sha}"
            )
        if declared and sha != declared:
            problems.append(
                f"{prov.name} declares {sha} while source-sha.txt declares "
                f"{declared}; they must agree"
            )

    # ── SBOM ───────────────────────────────────────────────────────────
    for artifact in artifacts:
        candidates = [
            artifact.with_name(artifact.name + suffix) for suffix in SBOM_SUFFIXES
        ]
        found = [c for c in candidates if c.exists()]
        if not found:
            problems.append(f"{artifact.name} has no CycloneDX SBOM")
            continue
        sbom = found[0]
        try:
            document = json.loads(sbom.read_text(encoding="utf-8"))
        except json.JSONDecodeError as exc:
            problems.append(f"{sbom.name} is not valid JSON: {exc}")
            continue
        if document.get("bomFormat") != "CycloneDX":
            problems.append(
                f"{sbom.name} has bomFormat {document.get('bomFormat')!r}, "
                "expected 'CycloneDX'"
            )
        if not document.get("components"):
            problems.append(f"{sbom.name} lists no components")

    # ── Signature and attestation ───────────────────────────────────────
    signatures = sorted(directory.glob("*.sig")) + sorted(directory.glob("*.sigstore"))
    if not signatures:
        problems.append(
            "no signature files found; a checksum alone does not prove who "
            "produced these artifacts"
        )
    if not list(directory.glob("*.intoto.jsonl")) and not list(directory.glob("*.jsonl")):
        problems.append("no build-provenance attestation found")

    return problems


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--dir", required=True, type=Path)
    parser.add_argument("--expect-sha", default=None)
    args = parser.parse_args()

    problems = verify(args.dir.resolve(), args.expect_sha)
    for problem in problems:
        print(f"::error::{problem}")
    if problems:
        print(f"{len(problems)} release artifact problem(s) found.")
        return 1
    print("Release artifacts verify.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
