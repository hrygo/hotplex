#!/usr/bin/env python3
"""Generate one CycloneDX SBOM per shipped release archive.

The offline bundle is an archive whose payload is itself archives: the
 OpenCode runtime ships as .zip and the Oh My OpenAgent packages as .tgz.
syft does not recurse into them — scanning the bundle reports zero
 components, and scanning the extracted tree reports zero more — so a
 plain `syft scan <archive>` produces a technically valid document that
 describes nothing.

This walks one level of nesting explicitly: extract the shipped archive,
scan each file inside it, and merge the components into a single document
named after the artifact. Syft is invoked once per member so the merge is
syft's own view of each file rather than this script's guess at it.

Usage:
    python3 scripts/ci/generate_sbom.py --archive dist/foo.tar.gz \
        --output dist/foo.tar.gz.sbom.json --syft syft
"""
from __future__ import annotations

import argparse
import json
import shutil
import subprocess
import sys
import tarfile
import tempfile
import zipfile
from pathlib import Path

MAX_MEMBERS = 64


def extract(archive: Path, destination: Path) -> None:
    """Unpack a .tar.gz or .zip into destination."""
    if zipfile.is_zipfile(archive):
        with zipfile.ZipFile(archive) as handle:
            handle.extractall(destination)
        return
    with tarfile.open(archive) as handle:
        # Bundles are built from this repository's own pack script, but a
        # release pipeline should not be the thing that turns a crafted
        # archive into files written outside the temp directory.
        for member in handle.getmembers():
            target = (destination / member.name).resolve()
            if not str(target).startswith(str(destination.resolve()) + "/"):
                raise SystemExit(f"refusing to extract {member.name!r}: escapes target")
            if member.issym() or member.islnk():
                raise SystemExit(f"refusing to extract link {member.name!r}")
        handle.extractall(destination)


def scan(syft: str, source: Path) -> list[dict]:
    """Return the CycloneDX components syft finds in one file or directory."""
    # syft only accepts the explicit "dir:" scheme for directories; a file
    # has to be named directly or the scan fails.
    target = f"dir:{source}" if source.is_dir() else str(source)
    result = subprocess.run(
        # "cyclonedx-json" without "=<path>" writes the document to stdout.
        # The "=-" spelling exits 0 and prints nothing at all.
        [syft, "scan", target, "-o", "cyclonedx-json", "-q"],
        capture_output=True,
        text=True,
        check=False,
    )
    if result.returncode != 0:
        # A member syft cannot read (a compiled runtime with no embedded
        # dependency metadata) is not a reason to fail the release; it just
        # contributes nothing.
        print(f"note: syft could not scan {source.name}", file=sys.stderr)
        return []
    try:
        document = json.loads(result.stdout)
    except json.JSONDecodeError:
        print(f"note: syft emitted no JSON for {source.name}", file=sys.stderr)
        return []
    return [
        component
        for component in document.get("components", [])
        if isinstance(component, dict)
    ]


def deduplicate(components: list[dict]) -> list[dict]:
    """Drop repeats, keyed by purl when present and by name/purl otherwise."""
    seen: set[str] = set()
    unique: list[dict] = []
    for component in components:
        key = component.get("purl") or json.dumps(component, sort_keys=True)
        if key in seen:
            continue
        seen.add(key)
        unique.append(component)
    return sorted(unique, key=lambda c: (str(c.get("name", "")), str(c.get("purl", ""))))


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--archive", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--syft", default="syft")
    args = parser.parse_args()

    archive = args.archive.resolve()
    if not archive.is_file():
        print(f"::error::{archive} does not exist")
        return 1
    if shutil.which(args.syft) is None:
        print(f"::error::{args.syft} is not on PATH")
        return 1

    workdir = Path(tempfile.mkdtemp(prefix="hotplex-sbom-"))
    try:
        extracted = workdir / "extracted"
        extracted.mkdir()
        try:
            extract(archive, extracted)
        except (tarfile.TarError, zipfile.BadZipFile, OSError) as exc:
            print(f"::error::cannot read {archive.name}: {exc}")
            return 1

        # The two archive shapes do not agree on layout. A binary archive
        # puts the executable at the root, but pack-offline-bundle.sh tars
        # from the bundle's parent directory, so every payload sits inside one
        # top-level directory. Walking only the top level finds the binaries
        # and nothing at all in a bundle.
        members = sorted(
            path
            for path in extracted.rglob("*")
            if path.is_file()
            # "._name" AppleDouble entries and __MACOSX are filesystem
            # metadata, not payload.
            and not path.name.startswith("._")
            and "__MACOSX" not in path.parts
        )
        if not members:
            print(f"::error::{archive.name} contains no files to describe")
            return 1
        if len(members) > MAX_MEMBERS:
            print(
                f"::error::{archive.name} has {len(members)} top-level entries, "
                f"more than the {MAX_MEMBERS} this script will scan"
            )
            return 1

        components: list[dict] = []
        for member in members:
            components.extend(scan(args.syft, member))
    finally:
        shutil.rmtree(workdir, ignore_errors=True)

    document = {
        "bomFormat": "CycloneDX",
        "specVersion": "1.6",
        "version": 1,
        "metadata": {
            "component": {
                "type": "file",
                "name": archive.name,
                "bom-ref": archive.name,
            }
        },
        "components": deduplicate(components),
    }
    args.output.write_text(
        json.dumps(document, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    print(f"{archive.name}: {len(document['components'])} component(s)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
