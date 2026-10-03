import importlib.util
import sys
import tempfile
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[2]
MODULE_PATH = Path(__file__).with_name("check_release_gating.py")


def load_checker():
    spec = importlib.util.spec_from_file_location("check_release_gating", MODULE_PATH)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"cannot load checker module from {MODULE_PATH}")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


checker = load_checker()


VALIDATE_HEADER = """\
name: Validate
on:
  workflow_call:
    inputs:
      ref:
        required: true
        type: string
jobs:
  lint:
    name: Lint
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
        with:
          ref: ${{ inputs.ref }}
  test:
    name: Test
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
        with:
          ref: ${{ inputs.ref }}
  gate:
    name: Validation Gate
    needs: [lint, test]
    if: always()
    steps:
      - run: echo ok
"""

RELEASE_HEADER = """\
name: Release
on:
  push:
    tags: ['v*']
jobs:
  resolve:
    name: Resolve
    runs-on: ubuntu-latest
    steps:
      - run: echo resolve
  validate:
    name: Validate
    needs: [resolve]
    uses: ./.github/workflows/validate.yml
    with:
      ref: ${{ needs.resolve.outputs.sha }}
  build:
    name: Build
    needs: [resolve, validate]
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
        with:
          ref: ${{ needs.resolve.outputs.sha }}
  offline-bundle:
    name: Pack offline bundle
    needs: [resolve, validate, build]
    runs-on: ubuntu-latest
    steps:
      - run: echo pack
  smoke:
    name: Smoke
    needs: [resolve, validate, build]
    runs-on: ubuntu-latest
    steps:
      - run: echo smoke
  publish:
    name: Publish
    needs: [resolve, validate, build, offline-bundle, smoke]
    runs-on: ubuntu-latest
    permissions:
      contents: write
    steps:
      - name: Verify
        run: python3 scripts/ci/verify_release_artifacts.py --dir dist
      - uses: softprops/action-gh-release@v3.0.0
"""


class ReleaseGatingTest(unittest.TestCase):
    def write_workflows(self, release: str, validate: str) -> Path:
        root = Path(tempfile.mkdtemp())
        workflows = root / ".github" / "workflows"
        workflows.mkdir(parents=True)
        (workflows / "release.yml").write_text(release, encoding="utf-8")
        (workflows / "validate.yml").write_text(validate, encoding="utf-8")
        return root

    def check(self, release: str, validate: str) -> list[str]:
        return checker.check(self.write_workflows(release, validate))

    # ── Baseline ───────────────────────────────────────────────────────
    def test_intact_graph_passes(self):
        self.assertEqual([], self.check(RELEASE_HEADER, VALIDATE_HEADER))

    def test_repository_workflows_pass(self):
        """The real workflows must satisfy the invariant this script enforces."""
        self.assertEqual([], checker.check(REPO_ROOT))

    # ── The parser's own blind spot ────────────────────────────────────
    def test_column_0_line_inside_a_run_block_is_reported_as_truncated(self):
        """A file the parser cannot finish reading must say so.

        A PowerShell here-string written from column 0 ends the YAML block
        scalar. The parser used to end its jobs block on that line and go on
        reporting verdicts about a workflow whose publish job it had never
        seen — which is how a release.yml GitHub refuses to parse reached
        main with every local check green.
        """
        broken = RELEASE_HEADER.replace(
            "      - run: echo smoke",
            '      - run: |\n          @"\ngateway:\n  addr: "127.0.0.1"\n"@ | Out-File x',
        )
        problems = self.check(broken, VALIDATE_HEADER)
        self.assertTrue(
            any("not a workflow-level key" in p for p in problems),
            f"expected a truncation diagnostic, got {problems}",
        )

    def test_repository_workflows_are_not_truncated(self):
        """The real workflows must be read to their end, not just their head."""
        for name in ("release.yml", "validate.yml"):
            path = REPO_ROOT / ".github" / "workflows" / name
            workflow = checker.Workflow(path)
            self.assertIsNone(
                workflow.truncated_at,
                f"{name} stops being parseable at {workflow.truncated_at!r}",
            )

    # ── The edges that matter ──────────────────────────────────────────
    def test_publish_without_validate_is_rejected(self):
        broken = RELEASE_HEADER.replace(
            "  publish:\n"
            "    name: Publish\n"
            "    needs: [resolve, validate, build, offline-bundle, smoke]\n",
            "  publish:\n"
            "    name: Publish\n"
            "    needs: [resolve, build, offline-bundle]\n",
        )
        self.assertNotEqual(RELEASE_HEADER, broken, "fixture did not apply")
        problems = self.check(broken, VALIDATE_HEADER)
        self.assertTrue(
            any("does not need 'validate'" in p for p in problems),
            problems,
        )

    def test_publish_without_offline_bundle_is_rejected(self):
        broken = RELEASE_HEADER.replace(
            "    needs: [resolve, validate, build, offline-bundle, smoke]\n"
            "    runs-on: ubuntu-latest\n"
            "    permissions:\n",
            "    needs: [resolve, validate, build]\n"
            "    runs-on: ubuntu-latest\n"
            "    permissions:\n",
        )
        self.assertNotEqual(RELEASE_HEADER, broken, "fixture did not apply")
        problems = self.check(broken, VALIDATE_HEADER)
        self.assertTrue(
            any("does not need 'offline-bundle'" in p for p in problems),
            problems,
        )

    def test_a_second_publisher_is_rejected(self):
        """The offline bundle used to publish in parallel with the release."""
        two_publishers = RELEASE_HEADER + """\
  publish-early:
    name: Publish early
    needs: [resolve, validate, build]
    runs-on: ubuntu-latest
    permissions:
      contents: write
    steps:
      - uses: softprops/action-gh-release@v3.0.0
"""
        problems = self.check(two_publishers, VALIDATE_HEADER)
        self.assertTrue(
            any("exactly one job that publishes" in p for p in problems),
            problems,
        )

    def test_build_ahead_of_the_gate_is_rejected(self):
        broken = RELEASE_HEADER.replace(
            "  build:\n"
            "    name: Build\n"
            "    needs: [resolve, validate]\n",
            "  build:\n"
            "    name: Build\n"
            "    needs: [resolve]\n",
        )
        self.assertNotEqual(RELEASE_HEADER, broken, "fixture did not apply")
        problems = self.check(broken, VALIDATE_HEADER)
        self.assertTrue(
            any("'build' runs without needing 'validate'" in p for p in problems),
            problems,
        )

    def test_conditional_gate_job_is_rejected(self):
        broken = RELEASE_HEADER.replace(
            "  validate:\n"
            "    name: Validate\n"
            "    needs: [resolve]\n",
            "  validate:\n"
            "    name: Validate\n"
            "    needs: [resolve]\n"
            "    if: github.event_name == 'push'\n",
        )
        self.assertNotEqual(RELEASE_HEADER, broken, "fixture did not apply")
        problems = self.check(broken, VALIDATE_HEADER)
        self.assertTrue(
            any("has an 'if' condition" in p for p in problems),
            problems,
        )

    def test_continue_on_error_publish_is_rejected(self):
        broken = RELEASE_HEADER.replace(
            "    needs: [resolve, validate, build, offline-bundle, smoke]\n"
            "    runs-on: ubuntu-latest\n",
            "    needs: [resolve, validate, build, offline-bundle, smoke]\n"
            "    runs-on: ubuntu-latest\n"
            "    continue-on-error: true\n",
        )
        self.assertNotEqual(RELEASE_HEADER, broken, "fixture did not apply")
        problems = self.check(broken, VALIDATE_HEADER)
        self.assertTrue(
            any("continue-on-error" in p for p in problems),
            problems,
        )

    # ── The gate must not be satisfiable by absence ────────────────────
    def test_ungated_check_is_rejected(self):
        broken = VALIDATE_HEADER.replace(
            "  gate:\n    name: Validation Gate\n    needs: [lint, test]\n",
            "  gate:\n    name: Validation Gate\n    needs: [lint]\n",
        )
        self.assertNotEqual(VALIDATE_HEADER, broken, "fixture did not apply")
        problems = self.check(RELEASE_HEADER, broken)
        self.assertTrue(
            any("gate does not depend on test" in p for p in problems),
            problems,
        )

    def test_gate_without_always_is_rejected(self):
        broken = VALIDATE_HEADER.replace(
            "    needs: [lint, test]\n    if: always()\n",
            "    needs: [lint, test]\n",
        )
        self.assertNotEqual(VALIDATE_HEADER, broken, "fixture did not apply")
        problems = self.check(RELEASE_HEADER, broken)
        self.assertTrue(
            any("if: always()" in p for p in problems),
            problems,
        )

    def test_continue_on_error_check_is_rejected(self):
        broken = VALIDATE_HEADER.replace(
            "  test:\n    name: Test\n    runs-on: ubuntu-latest\n",
            "  test:\n    name: Test\n    runs-on: ubuntu-latest\n    continue-on-error: true\n",
        )
        self.assertNotEqual(VALIDATE_HEADER, broken, "fixture did not apply")
        problems = self.check(RELEASE_HEADER, broken)
        self.assertTrue(
            any("continue-on-error" in p for p in problems),
            problems,
        )

    # ── Immutability ───────────────────────────────────────────────────
    def test_floating_checkout_is_rejected(self):
        broken = VALIDATE_HEADER.replace(
          "          ref: ${{ inputs.ref }}\n",
          "          ref: main\n",
        )
        self.assertNotEqual(VALIDATE_HEADER, broken, "fixture did not apply")
        problems = self.check(RELEASE_HEADER, broken)
        self.assertTrue(
            any("is not an expression" in p for p in problems),
            problems,
        )

    def test_validate_must_use_the_resolved_sha(self):
        broken = RELEASE_HEADER.replace(
            "      ref: ${{ needs.resolve.outputs.sha }}\n"
            "  build:",
            "      ref: main\n"
            "  build:",
        )
        self.assertNotEqual(RELEASE_HEADER, broken, "fixture did not apply")
        problems = self.check(broken, VALIDATE_HEADER)
        self.assertTrue(
            any("must check out the resolved SHA" in p for p in problems),
            problems,
        )

    # ── The check itself must degrade into a diagnostic ───────────────
    def test_a_release_without_a_gate_reports_instead_of_crashing(self):
        """This is the pre-F1 shape: nothing validates, and nothing publishes
        except jobs that were never told to wait. The checker has to describe
        that, not raise — a traceback in CI is not a gate failure anyone
        reads."""
        ungated = """\
name: Release
on:
  push:
    tags: ['v*']
jobs:
  build:
    name: Build
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
        with:
          ref: ${{ github.sha }}
  release:
    name: Release
    needs: [build]
    runs-on: ubuntu-latest
    steps:
      - uses: softprops/action-gh-release@v3.0.0
"""
        problems = self.check(ungated, VALIDATE_HEADER)
        self.assertTrue(
            any("no 'validate' job" in p for p in problems),
            problems,
        )
        self.assertTrue(
            any("does not need 'validate'" in p for p in problems),
            problems,
        )


    # -- A release must not resolve versions at build time ---------------
    def _dynamic_release(self, needle: str) -> str:
        return RELEASE_HEADER.replace(
            "      - run: echo pack\n",
            f"      - run: echo pack\n        {needle}\n",
        )

    def test_opencode_version_override_is_rejected(self):
        broken = self._dynamic_release("bash scripts/pack-offline-bundle.sh --opencode-version 1.2.3")
        self.assertNotEqual(RELEASE_HEADER, broken, "fixture did not apply")
        problems = self.check(broken, VALIDATE_HEADER)
        self.assertTrue(
            any("--opencode-version" in p for p in problems),
            problems,
        )

    def test_omo_version_override_is_rejected(self):
        broken = self._dynamic_release("bash scripts/pack-offline-bundle.sh --omo-version 9.9.9")
        self.assertNotEqual(RELEASE_HEADER, broken, "fixture did not apply")
        problems = self.check(broken, VALIDATE_HEADER)
        self.assertTrue(any("--omo-version" in p for p in problems), problems)

    def test_allow_dynamic_is_rejected(self):
        broken = self._dynamic_release("bash scripts/pack-offline-bundle.sh --allow-dynamic")
        self.assertNotEqual(RELEASE_HEADER, broken, "fixture did not apply")
        problems = self.check(broken, VALIDATE_HEADER)
        self.assertTrue(any("--allow-dynamic" in p for p in problems), problems)

    def test_latest_release_lookup_is_rejected(self):
        broken = self._dynamic_release(
            "curl -sf https://api.github.com/repos/anomalyco/opencode/releases/latest"
        )
        self.assertNotEqual(RELEASE_HEADER, broken, "fixture did not apply")
        problems = self.check(broken, VALIDATE_HEADER)
        self.assertTrue(any("releases/latest" in p for p in problems), problems)

    def test_npm_view_is_rejected(self):
        broken = self._dynamic_release("npm view oh-my-opencode version")
        self.assertNotEqual(RELEASE_HEADER, broken, "fixture did not apply")
        problems = self.check(broken, VALIDATE_HEADER)
        self.assertTrue(any("npm view" in p for p in problems), problems)

    # -- Verification must come before publication -----------------------
    def test_publish_without_verification_is_rejected(self):
        broken = RELEASE_HEADER.replace(
            "      - name: Verify\n"
            "        run: python3 scripts/ci/verify_release_artifacts.py --dir dist\n",
            "",
        )
        self.assertNotEqual(RELEASE_HEADER, broken, "fixture did not apply")
        problems = self.check(broken, VALIDATE_HEADER)
        self.assertTrue(
            any("without running verify_release_artifacts.py" in p for p in problems),
            problems,
        )

    def test_verification_after_publication_is_rejected(self):
        """Checking the artifacts once the release page exists can only ever
        annotate a page that is already public."""
        broken = RELEASE_HEADER.replace(
            "      - name: Verify\n"
            "        run: python3 scripts/ci/verify_release_artifacts.py --dir dist\n"
            "      - uses: softprops/action-gh-release@v3.0.0\n",
            "      - uses: softprops/action-gh-release@v3.0.0\n"
            "      - name: Verify\n"
            "        run: python3 scripts/ci/verify_release_artifacts.py --dir dist\n",
        )
        self.assertNotEqual(RELEASE_HEADER, broken, "fixture did not apply")
        problems = self.check(broken, VALIDATE_HEADER)
        self.assertTrue(
            any("creates the release before verifying" in p for p in problems),
            problems,
        )

if __name__ == "__main__":
    unittest.main()
