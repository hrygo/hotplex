#!/usr/bin/env python3
"""Select the Go packages whose tests can be affected by a change set.

The selector consumes NUL-separated repository-relative paths and computes the
reverse dependency closure over runtime, internal-test and external-test
imports. It emits either JSON or GitHub Actions output values; it never shells
out through a command interpreter.
"""

from __future__ import annotations

import argparse
import fnmatch
import io
import json
import subprocess
import sys
from collections import defaultdict, deque
from dataclasses import asdict, dataclass
from pathlib import Path
from typing import BinaryIO, Iterable, Sequence


# These are the packages the current Test job excludes from its main coverage
# and shard set. They are still selected for a dedicated, unsharded validation
# step so a scoped Test job cannot silently skip them.
DEFAULT_EXCLUDED_PATTERNS = (
    "internal/worker/proc",
    "internal/worker/pi",
    "cmd/hotplex",
    "e2e",
)

# A change to any of these files can alter package discovery, compilation,
# protocol fixtures, or the selection algorithm itself. A filtered run would
# risk proving less than the pre-change workflow, so fall back to every
# non-excluded package plus the dedicated excluded-package step.
FULL_FALLBACK_PATTERNS = (
    "go.mod",
    "go.sum",
    "Makefile",
    ".github/workflows/ci.yml",
    ".github/actions/setup-go-webchat/action.yml",
    "scripts/ci/select_go_tests.py",
    "scripts/ci/test_select_go_tests.py",
    "scripts/test-contract-matrix.sh",
    "pkg/aep/schema/*",
    "pkg/aep/schema/**",
)

GITHUB_OUTPUT_DELIMITER = "SELECT_GO_TESTS_EOF"


@dataclass(frozen=True)
class SelectionResult:
    mode: str
    fallback_reason: str
    test_packages: list[str]
    excluded_test_packages: list[str]


def read_nul_paths(stream: BinaryIO) -> list[str]:
    """Read NUL-separated UTF-8 paths, rejecting malformed unterminated input."""

    raw = stream.read()
    if not raw:
        return []
    if not raw.endswith(b"\0"):
        raise ValueError("changed-path stream is not NUL terminated")

    paths: list[str] = []
    for chunk in raw.split(b"\0"):
        if not chunk:
            continue
        paths.append(chunk.decode("utf-8", errors="surrogateescape"))
    return paths


def matches_any(path: str, patterns: Iterable[str]) -> bool:
    normalized = path.replace("\\", "/")
    return any(fnmatch.fnmatch(normalized, pattern) for pattern in patterns)


def is_excluded(import_path: str, patterns: Sequence[str]) -> bool:
    return any(pattern in import_path for pattern in patterns)


def full_selection(packages: Sequence[dict], reason: str, excluded_patterns: Sequence[str]) -> SelectionResult:
    all_import_paths = sorted(
        package["ImportPath"] for package in packages if package.get("ImportPath") and package.get("Dir")
    )
    excluded = sorted(path for path in all_import_paths if is_excluded(path, excluded_patterns))
    excluded_set = set(excluded)
    return SelectionResult(
        mode="full",
        fallback_reason=reason,
        test_packages=[path for path in all_import_paths if path not in excluded_set],
        excluded_test_packages=excluded,
    )


def package_dir(package: dict) -> Path:
    return Path(package["Dir"]).resolve()


def select_packages(
    *,
    changed_paths: Sequence[str],
    packages: Sequence[dict],
    repo_root: Path,
    excluded_patterns: Sequence[str] = DEFAULT_EXCLUDED_PATTERNS,
) -> SelectionResult:
    """Return a full or scoped selection for the supplied repository changes."""

    repo_root = repo_root.resolve()
    listed = [package for package in packages if package.get("ImportPath") and package.get("Dir")]
    full_result = full_selection(listed, "", excluded_patterns)

    normalized_changes = [path.replace("\\", "/") for path in changed_paths]
    for path in normalized_changes:
        if matches_any(path, FULL_FALLBACK_PATTERNS):
            reason = {
                "go.mod": "dependency manifest changed",
                "go.sum": "dependency checksum manifest changed",
            }.get(path, f"test selection control file changed: {path}")
            return SelectionResult(
                mode="full",
                fallback_reason=reason,
                test_packages=full_result.test_packages,
                excluded_test_packages=full_result.excluded_test_packages,
            )

    changed_dirs = {
        (repo_root / path).parent.resolve()
        for path in normalized_changes
        if path.endswith(".go")
    }
    if not changed_dirs:
        return SelectionResult(
            mode="empty",
            fallback_reason="",
            test_packages=[],
            excluded_test_packages=[],
        )

    packages_by_dir: dict[Path, list[str]] = defaultdict(list)
    for package in listed:
        packages_by_dir[package_dir(package)].append(package["ImportPath"])

    changed_packages: set[str] = set()
    for directory in changed_dirs:
        matches = packages_by_dir.get(directory)
        if not matches:
            return SelectionResult(
                mode="full",
                fallback_reason="changed Go path maps to no listed package",
                test_packages=full_result.test_packages,
                excluded_test_packages=full_result.excluded_test_packages,
            )
        changed_packages.update(matches)

    known = {package["ImportPath"] for package in listed}
    reverse_graph: dict[str, set[str]] = defaultdict(set)
    for package in listed:
        importer = package["ImportPath"]
        dependencies = {
            dependency
            for field in ("Imports", "TestImports", "XTestImports")
            for dependency in package.get(field, [])
            if dependency in known
        }
        for dependency in dependencies:
            reverse_graph[dependency].add(importer)

    affected: set[str] = set(changed_packages)
    queue = deque(sorted(changed_packages))
    while queue:
        dependency = queue.popleft()
        for importer in sorted(reverse_graph.get(dependency, ())):
            if importer in affected:
                continue
            affected.add(importer)
            queue.append(importer)

    excluded = sorted(path for path in affected if is_excluded(path, excluded_patterns))
    selected = sorted(path for path in affected if path not in set(excluded))
    mode = "scoped" if selected else "empty"
    return SelectionResult(
        mode=mode,
        fallback_reason="" if selected else "only excluded packages affected",
        test_packages=selected,
        excluded_test_packages=excluded,
    )


def load_go_list_packages(repo_root: Path) -> list[dict]:
    """Run go list once and decode its concatenated JSON objects."""

    completed = subprocess.run(
        ["go", "list", "-json", "./..."],
        cwd=repo_root,
        check=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    decoder = json.JSONDecoder()
    payload = completed.stdout
    packages: list[dict] = []
    index = 0
    length = len(payload)
    while index < length:
        while index < length and payload[index].isspace():
            index += 1
        if index >= length:
            break
        package, index = decoder.raw_decode(payload, index)
        packages.append(package)
    return packages


def encode_result(result: SelectionResult) -> str:
    return json.dumps(asdict(result), ensure_ascii=False, sort_keys=True, separators=(",", ":"))


def write_github_output(result: SelectionResult, output_path: Path) -> None:
    lines = [
        f"mode={result.mode}",
        f"fallback_reason={result.fallback_reason}",
        f"test_packages<<{GITHUB_OUTPUT_DELIMITER}",
        *result.test_packages,
        GITHUB_OUTPUT_DELIMITER,
        f"excluded_test_packages<<{GITHUB_OUTPUT_DELIMITER}",
        *result.excluded_test_packages,
        GITHUB_OUTPUT_DELIMITER,
    ]
    output_path.write_text("\n".join(lines) + "\n", encoding="utf-8")


def parse_args(argv: Sequence[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--repo-root",
        type=Path,
        default=Path.cwd(),
        help="repository root used for go list and path mapping (default: cwd)",
    )
    parser.add_argument(
        "--github-output",
        type=Path,
        help="append mode, reason, and package lists to a GitHub Actions output file",
    )
    parser.add_argument(
        "--full",
        action="store_true",
        help="select every package, including the dedicated excluded-package list",
    )
    return parser.parse_args(argv)


def main(argv: Sequence[str] | None = None) -> int:
    args = parse_args(argv)
    repo_root = args.repo_root.resolve()

    try:
        packages = load_go_list_packages(repo_root)
        if args.full:
            result = full_selection(
                packages,
                "full selection requested",
                DEFAULT_EXCLUDED_PATTERNS,
            )
        else:
            changed_paths = read_nul_paths(io.BytesIO(sys.stdin.buffer.read()))
            result = select_packages(
                changed_paths=changed_paths,
                packages=packages,
                repo_root=repo_root,
            )
    except (ValueError, subprocess.CalledProcessError, json.JSONDecodeError) as exc:
        if args.github_output:
            write_github_output(
                SelectionResult(
                    mode="full",
                    fallback_reason=f"test selection failed: {exc}",
                    test_packages=[],
                    excluded_test_packages=[],
                ),
                args.github_output,
            )
        print(f"select_go_tests: {exc}", file=sys.stderr)
        return 1

    if args.github_output:
        write_github_output(result, args.github_output)
    else:
        print(encode_result(result))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
