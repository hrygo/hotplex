import copy
import importlib.util
import json
import subprocess
import sys
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[2]
MODULE_PATH = Path(__file__).with_name("check_runtime_lock.py")
LOCK_PATH = REPO_ROOT / "configs" / "worker-runtime-lock.json"


def load_checker():
    spec = importlib.util.spec_from_file_location("check_runtime_lock", MODULE_PATH)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"cannot load checker module from {MODULE_PATH}")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


checker = load_checker()


def real_lock() -> dict:
    return json.loads(LOCK_PATH.read_text(encoding="utf-8"))


class RuntimeLockTest(unittest.TestCase):
    def manifest_problems(self, lock: dict) -> list[str]:
        platforms = lock.get("platforms") or []
        return checker.validate_manifest(lock, platforms)

    # -- Baseline ---------------------------------------------------------

    def test_committed_lock_is_valid(self):
        self.assertEqual([], self.manifest_problems(real_lock()))

    def test_repository_passes_end_to_end(self):
        self.assertEqual([], checker.check(REPO_ROOT))

    # -- A version that is not a pin --------------------------------------

    def test_latest_version_is_rejected(self):
        lock = copy.deepcopy(real_lock())
        lock["runtimes"]["opencode"]["version"] = "latest"
        problems = self.manifest_problems(lock)
        self.assertTrue(any("is not pinned" in p for p in problems), problems)

    def test_empty_version_is_rejected(self):
        lock = copy.deepcopy(real_lock())
        lock["runtimes"]["oh-my-opencode"]["version"] = ""
        problems = self.manifest_problems(lock)
        self.assertTrue(any("is not pinned" in p for p in problems), problems)

    def test_non_semver_version_is_rejected(self):
        lock = copy.deepcopy(real_lock())
        lock["runtimes"]["opencode"]["version"] = "v1.18.34-rc1"
        problems = self.manifest_problems(lock)
        self.assertTrue(any("not a plain semver" in p for p in problems), problems)

    # -- A digest that is not a digest ------------------------------------

    def test_missing_platform_digest_is_rejected(self):
        lock = copy.deepcopy(real_lock())
        del lock["runtimes"]["opencode"]["assets"]["linux-arm64"]["sha256"]
        problems = self.manifest_problems(lock)
        self.assertTrue(
            any("linux-arm64" in p and "sha256" in p for p in problems),
            problems,
        )

    def test_non_hex_digest_is_rejected(self):
        lock = copy.deepcopy(real_lock())
        lock["runtimes"]["opencode"]["assets"]["darwin-arm64"]["sha256"] = "not-a-digest"
        problems = self.manifest_problems(lock)
        self.assertTrue(
            any("not a 64-character hex digest" in p for p in problems),
            problems,
        )

    def test_truncated_digest_is_rejected(self):
        lock = copy.deepcopy(real_lock())
        lock["runtimes"]["opencode"]["assets"]["windows-x64"]["sha256"] = "abc123"
        problems = self.manifest_problems(lock)
        self.assertTrue(
            any("not a 64-character hex digest" in p for p in problems),
            problems,
        )

    def test_unpinned_platform_is_rejected(self):
        lock = copy.deepcopy(real_lock())
        lock["platforms"].append("linux-riscv64")
        problems = self.manifest_problems(lock)
        self.assertTrue(
            any("no asset pinned for platform 'linux-riscv64'" in p for p in problems),
            problems,
        )

    def test_platform_without_url_is_rejected(self):
        lock = copy.deepcopy(real_lock())
        del lock["runtimes"]["opencode"]["assets"]["darwin-x64"]["url"]
        problems = self.manifest_problems(lock)
        self.assertTrue(any("no download url" in p for p in problems), problems)

    # -- npm integrity ----------------------------------------------------

    def test_bad_integrity_is_rejected(self):
        lock = copy.deepcopy(real_lock())
        lock["runtimes"]["oh-my-opencode"]["packages"]["oh-my-opencode"]["integrity"] = "sha1-abc"
        problems = self.manifest_problems(lock)
        self.assertTrue(any("not an npm sha512 integrity" in p for p in problems), problems)

    def test_mixed_omo_versions_are_rejected(self):
        lock = copy.deepcopy(real_lock())
        lock["runtimes"]["oh-my-opencode"]["packages"]["oh-my-opencode-darwin-arm64"]["version"] = "5.1.12"
        problems = self.manifest_problems(lock)
        self.assertTrue(any("a lock must pin one version" in p for p in problems), problems)

    def test_missing_packages_are_rejected(self):
        lock = copy.deepcopy(real_lock())
        lock["runtimes"]["oh-my-opencode"]["packages"] = {}
        problems = self.manifest_problems(lock)
        self.assertTrue(any("no packages pinned" in p for p in problems), problems)

    # -- The packer must actually obey the lock ---------------------------

    def test_packer_is_checked_against_the_committed_lock(self):
        problems = checker.check_packer(REPO_ROOT, LOCK_PATH)
        self.assertEqual([], problems)

    def test_packer_fails_closed_on_an_unpinned_platform(self):
        """The behavioral half: the script must reject a platform the lock does
        not describe rather than downloading whatever exists for it."""
        result = subprocess.run(
            [
                "bash",
                str(REPO_ROOT / "scripts" / "pack-offline-bundle.sh"),
                "--dry-run",
                "--lock",
                str(LOCK_PATH),
                "--platform",
                "linux-riscv64",
            ],
            capture_output=True,
            text=True,
            timeout=120,
        )
        self.assertNotEqual(0, result.returncode)
        # die() writes to stdout, so the operator sees the reason either way.
        self.assertIn("not in the runtime lock", result.stdout + result.stderr)

    def test_packer_fails_closed_on_a_missing_lock(self):
        result = subprocess.run(
            [
                "bash",
                str(REPO_ROOT / "scripts" / "pack-offline-bundle.sh"),
                "--dry-run",
                "--lock",
                str(REPO_ROOT / "configs" / "no-such-lock.json"),
                "--platform",
                "linux-x64",
            ],
            capture_output=True,
            text=True,
            timeout=120,
        )
        self.assertNotEqual(0, result.returncode)


if __name__ == "__main__":
    unittest.main()
