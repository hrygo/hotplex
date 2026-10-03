#!/usr/bin/env python3
"""Static check that the release gate is actually a gate.

A directed workflow graph is the only thing standing between an untested
commit and a download page, and every one of its edges is a line someone can
delete in a hurry. `needs:` does not defend itself: drop `validate` from
publish's `needs:` and nothing fails, the release just ships.

This script reads the workflow files as text and asserts the properties that
make the gate meaningful. It deliberately uses no YAML dependency — CI does
not install PyYAML, and a gate that cannot run is not a gate.

Usage:
    python3 scripts/ci/check_release_gating.py [--repo-root .]
"""

from __future__ import annotations

import argparse
import re
import sys
from pathlib import Path


JOB_RE = re.compile(r"^  ([A-Za-z0-9_-]+):\s*(?:#.*)?$")
PROP_RE = re.compile(r"^    ([A-Za-z0-9_-]+):\s*(.*)$")
NESTED_RE = re.compile(r"^      ([A-Za-z0-9_-]+):\s*(.*)$")
LIST_ITEM_RE = re.compile(r"^      -\s+(.*)$")
STEP_USE_RE = re.compile(r"^\s*-?\s*uses:\s*(\S+)")
CHECKOUT_REF_RE = re.compile(r"^\s*ref:\s*(.+?)\s*$")


class Workflow:
    """The subset of a workflow file this check cares about."""

    def __init__(self, path: Path) -> None:
        self.path = path
        self.jobs: dict[str, dict] = {}
        self._parse(path.read_text(encoding="utf-8"))

    def _parse(self, text: str) -> None:
        in_jobs = False
        current: str | None = None
        in_steps = False
        in_needs = False
        nested: dict | None = None

        for raw in text.splitlines():
            if raw.rstrip() == "jobs:":
                in_jobs = True
                continue
            if not in_jobs or not raw.strip() or raw.lstrip().startswith("#"):
                continue
            # A top-level key ends the jobs block.
            if raw[:1] not in (" ", "\t"):
                in_jobs = False
                current = None
                nested = None
                continue

            job_match = JOB_RE.match(raw)
            if job_match:
                current = job_match.group(1)
                self.jobs[current] = {"needs": [], "steps": []}
                in_steps = False
                in_needs = False
                nested = None
                continue
            if current is None:
                continue

            prop = PROP_RE.match(raw)
            if prop:
                key, value = prop.group(1), prop.group(2).strip()
                in_steps = key == "steps"
                in_needs = key == "needs"
                nested = None
                if in_needs:
                    self.jobs[current]["needs"] = _parse_needs(value)
                elif not in_steps and value == "":
                    # A nested mapping (`with:`, `outputs:`, `permissions:`).
                    nested = {}
                    self.jobs[current][key] = nested
                elif not in_steps:
                    self.jobs[current][key] = value
                continue

            if nested is not None:
                inner = NESTED_RE.match(raw)
                if inner:
                    nested[inner.group(1)] = inner.group(2).strip()
                    continue
                nested = None

            if in_needs:
                item = LIST_ITEM_RE.match(raw)
                if item:
                    self.jobs[current]["needs"].append(_unquote(item.group(1)))
                    continue
                in_needs = False

            if in_steps:
                use = STEP_USE_RE.match(raw)
                if use:
                    self.jobs[current]["steps"].append(use.group(1))

    def job(self, name: str) -> dict:
        return self.jobs.get(name, {})

    def uses(self, action: str) -> list[str]:
        """Job ids whose steps use the given action."""
        return [
            name
            for name, job in self.jobs.items()
            if any(step.startswith(action) for step in job["steps"])
        ]


def _unquote(token: str) -> str:
    token = token.split("#", 1)[0].strip()
    return token.strip("'\"")


def _parse_needs(value: str) -> list[str]:
    value = value.split("#", 1)[0].strip()
    if not value:
        return []
    if value.startswith("["):
        inner = value.strip("[]")
        return [_unquote(part) for part in inner.split(",") if part.strip()]
    return [_unquote(value)]


def checkout_refs(text: str) -> list[tuple[int, str]]:
    """(line number, ref expression) for every checkout step that pins a ref."""
    refs: list[tuple[int, str]] = []
    in_steps = False
    pending = False
    for number, raw in enumerate(text.splitlines(), start=1):
        if re.match(r"^    steps:\s*$", raw):
            in_steps = True
            continue
        if in_steps and re.match(r"^    [A-Za-z0-9_-]+:", raw):
            in_steps = False
        if not in_steps:
            continue
        if re.match(r"^\s*-?\s*uses:\s*actions/checkout@", raw):
            pending = True
            continue
        if pending:
            match = CHECKOUT_REF_RE.match(raw)
            if match:
                refs.append((number, match.group(1)))
                pending = False
    return refs


def check(repo_root: Path) -> list[str]:
    release_path = repo_root / ".github" / "workflows" / "release.yml"
    validate_path = repo_root / ".github" / "workflows" / "validate.yml"
    release = Workflow(release_path)
    validate = Workflow(validate_path)
    problems: list[str] = []

    def require(condition: bool, message: str) -> None:
        if not condition:
            problems.append(message)

    def require_job(workflow: Workflow, name: str) -> bool:
        """A missing job is a gating failure, not a crash."""
        if name not in workflow.jobs:
            problems.append(f"{workflow.path.name}: no '{name}' job")
            return False
        return True

    # ── The publish edge ───────────────────────────────────────────────
    # Everything that can create or attach a release must sit behind the gate.
    for producer in release.uses("softprops/action-gh-release"):
        job = release.job(producer)
        for required in ("validate", "build", "offline-bundle"):
            require(
                required in job["needs"],
                f"release.yml: '{producer}' publishes but does not need '{required}'",
            )
        require(
            not job.get("continue-on-error"),
            f"release.yml: '{producer}' has continue-on-error, so a blocked "
            "release would still be reported as successful",
        )

    require(
        len(release.uses("softprops/action-gh-release")) == 1,
        "release.yml: expected exactly one job that publishes; a second "
        "publisher can ship while the gate is failing",
    )

    # ── Nothing expensive runs ahead of the gate ───────────────────────
    for name, job in release.jobs.items():
        # `resolve` produces the SHA and `validate` is the gate, so neither can
        # depend on itself.
        if name not in ("resolve", "validate"):
            require(
                "validate" in job["needs"],
                f"release.yml: '{name}' runs without needing 'validate'",
            )
        # A conditional gate job is the worst case: it reads as protection while
        # being the one thing that can quietly switch itself off.
        require(
            "if" not in job,
            f"release.yml: '{name}' has an 'if' condition; a conditional gate "
            "job can be skipped and the run still looks green",
        )

    # ── The gate must be the shared, immutable-SHA one ─────────────────
    if require_job(release, "validate"):
        validate_job = release.job("validate")
        require(
            validate_job.get("uses", "").endswith("validate.yml"),
            "release.yml: 'validate' must call the reusable validate workflow",
        )
        require(
            "needs.resolve.outputs.sha" in validate_job.get("with", {}).get("ref", ""),
            "release.yml: 'validate' must check out the resolved SHA, not a "
            "branch or tag name that can move after validation",
        )

    # ── The reusable gate itself ───────────────────────────────────────
    if require_job(validate, "gate"):
        gate = validate.job("gate")
        check_jobs = {name for name in validate.jobs if name != "gate"}
        missing = sorted(check_jobs - set(gate["needs"]))
        require(
            not missing,
            f"validate.yml: gate does not depend on {', '.join(missing)}; an "
            "ungated check cannot block a release",
        )
        require(
            gate.get("if") == "always()",
            "validate.yml: the gate must run with if: always() so a skipped or "
            "cancelled check becomes an explicit failure instead of a skipped gate",
        )

    for name, job in validate.jobs.items():
        require(
            not job.get("continue-on-error"),
            f"validate.yml: '{name}' has continue-on-error; a failing check "
            "that continues is not a check",
        )

    # ── No floating checkout ───────────────────────────────────────────
    for path in (release_path, validate_path):
        text = path.read_text(encoding="utf-8")
        refs = checkout_refs(text)
        require(
            bool(refs),
            f"{path.name}: expected checkout steps that pin an explicit ref",
        )
        for number, ref in refs:
            require(
                "${{" in ref,
                f"{path.name}:{number}: checkout ref '{ref}' is not an "
                "expression; validating or building a moving branch proves "
                "nothing about the released artifact",
            )

    # ── Permissions ────────────────────────────────────────────────────
    writable = [
        name
        for name, job in release.jobs.items()
        if "write" in job.get("permissions", "") and "contents: write" in job.get("permissions", "")
    ]
    require(
        len(writable) <= 1,
        "release.yml: more than one job holds contents: write",
    )

    # ── A release must not resolve runtime versions at build time ──────
    # Passing --opencode-version / --omo-version switches the packer into
    # dynamic mode, and a workflow that queries upstream for "latest" ships
    # whatever is published that day rather than what the lock pins. Only
    # command lines are scanned: a comment explaining WHY the flag is absent
    # must not be mistaken for its use.
    release_text = "\n".join(
        line
        for line in release_path.read_text(encoding="utf-8").splitlines()
        if not line.lstrip().startswith("#")
    )
    for forbidden in (
        "--opencode-version",
        "--omo-version",
        "--allow-dynamic",
        "releases/latest",
        "npm view",
    ):
        require(
            forbidden not in release_text,
            f"release.yml: references {forbidden!r}; a release must build from "
            "configs/worker-runtime-lock.json, not from whatever upstream "
            "publishes that day",
        )

    return problems


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo-root", default=".", type=Path)
    args = parser.parse_args()

    problems = check(args.repo_root.resolve())
    for problem in problems:
        print(f"::error::{problem}")
    if problems:
        print(f"{len(problems)} release-gating problem(s) found.")
        return 1
    print("Release gating graph is intact.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
