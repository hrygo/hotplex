import hashlib
import importlib.util
import json
import shutil
import sys
import tempfile
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[2]
MODULE_PATH = Path(__file__).with_name("verify_release_artifacts.py")

SHA = "a" * 40


def load_verifier():
    spec = importlib.util.spec_from_file_location("verify_release_artifacts", MODULE_PATH)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"cannot load verifier module from {MODULE_PATH}")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


verifier = load_verifier()


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


class ReleaseArtifactsTest(unittest.TestCase):
    """A release directory is built once, then broken one way at a time. The
    point of every negative case is that the same verifier a real downloader
    would run refuses the artifact."""

    def setUp(self):
        self.dir = Path(tempfile.mkdtemp())
        self.payload = b"pretend this is a hotplex archive"
        self.artifact = self.dir / "hotplex-linux-x64.tar.gz"
        self.artifact.write_bytes(self.payload)
        self.digest = sha256_bytes(self.payload)

        (self.dir / "checksums.txt").write_text(
            f"{self.digest}  hotplex-linux-x64.tar.gz\n", encoding="utf-8"
        )
        (self.dir / "source-sha.txt").write_text(
            f"version=v1.0.0\nsource_sha={SHA}\n", encoding="utf-8"
        )
        (self.dir / "hotplex-linux-x64.provenance").write_text(
            f"version=v1.0.0\nsource_sha={SHA}\n", encoding="utf-8"
        )
        (self.dir / "hotplex-linux-x64.tar.gz.sbom.json").write_text(
            json.dumps(
                {
                    "bomFormat": "CycloneDX",
                    "specVersion": "1.5",
                    "components": [{"name": "hotplex", "version": "v1.0.0"}],
                }
            ),
            encoding="utf-8",
        )
        (self.dir / "checksums.txt.sig").write_bytes(b"signature bytes")
        (self.dir / "provenance.intoto.jsonl").write_text("{}\n", encoding="utf-8")

    def tearDown(self):
        shutil.rmtree(self.dir, ignore_errors=True)

    def verify(self, expect_sha=SHA):
        return verifier.verify(self.dir, expect_sha)

    # -- Baseline ---------------------------------------------------------

    def test_a_complete_release_verifies(self):
        self.assertEqual([], self.verify())

    def test_repository_has_a_verifier(self):
        self.assertTrue(MODULE_PATH.exists())

    # -- Tampered bytes ---------------------------------------------------

    def test_tampered_artifact_is_rejected(self):
        self.artifact.write_bytes(self.payload + b" and an extra byte")
        problems = self.verify()
        self.assertTrue(
            any("does not match checksums.txt" in p for p in problems),
            problems,
        )

    def test_artifact_missing_from_checksums_is_rejected(self):
        (self.dir / "checksums.txt").write_text("", encoding="utf-8")
        problems = self.verify()
        self.assertTrue(
            any("absent from checksums.txt" in p for p in problems),
            problems,
        )

    def test_missing_checksums_file_is_rejected(self):
        (self.dir / "checksums.txt").unlink()
        problems = self.verify()
        self.assertTrue(any("checksums.txt is missing" in p for p in problems), problems)

    # -- Wrong source SHA -------------------------------------------------

    def test_wrong_source_sha_is_rejected(self):
        (self.dir / "source-sha.txt").write_text(
            f"source_sha={'b' * 40}\n", encoding="utf-8"
        )
        problems = self.verify()
        self.assertTrue(any("but the release was validated at" in p for p in problems), problems)

    def test_provenance_disagreeing_with_source_sha_is_rejected(self):
        (self.dir / "hotplex-linux-x64.provenance").write_text(
            f"source_sha={'c' * 40}\n", encoding="utf-8"
        )
        problems = self.verify(expect_sha=None)
        self.assertTrue(any("they must agree" in p for p in problems), problems)

    def test_missing_provenance_is_rejected(self):
        (self.dir / "hotplex-linux-x64.provenance").unlink()
        problems = self.verify()
        self.assertTrue(
            any("no .provenance files found" in p for p in problems),
            problems,
        )

    def test_missing_source_sha_file_is_rejected(self):
        (self.dir / "source-sha.txt").unlink()
        problems = self.verify()
        self.assertTrue(any("source-sha.txt is missing" in p for p in problems), problems)

    # -- Missing supply-chain evidence ------------------------------------

    def test_missing_sbom_is_rejected(self):
        (self.dir / "hotplex-linux-x64.tar.gz.sbom.json").unlink()
        problems = self.verify()
        self.assertTrue(any("has no CycloneDX SBOM" in p for p in problems), problems)

    def test_unparseable_sbom_is_rejected(self):
        (self.dir / "hotplex-linux-x64.tar.gz.sbom.json").write_text("{not json", encoding="utf-8")
        problems = self.verify()
        self.assertTrue(any("not valid JSON" in p for p in problems), problems)

    def test_sbom_with_wrong_format_is_rejected(self):
        (self.dir / "hotplex-linux-x64.tar.gz.sbom.json").write_text(
            json.dumps({"bomFormat": "SPDX", "components": [{"name": "x"}]}),
            encoding="utf-8",
        )
        problems = self.verify()
        self.assertTrue(any("expected 'CycloneDX'" in p for p in problems), problems)

    def test_sbom_without_components_is_rejected(self):
        (self.dir / "hotplex-linux-x64.tar.gz.sbom.json").write_text(
            json.dumps({"bomFormat": "CycloneDX", "components": []}), encoding="utf-8"
        )
        problems = self.verify()
        self.assertTrue(any("lists no components" in p for p in problems), problems)

    def test_missing_signature_is_rejected(self):
        (self.dir / "checksums.txt.sig").unlink()
        problems = self.verify()
        self.assertTrue(any("no signature files found" in p for p in problems), problems)

    def test_missing_attestation_is_rejected(self):
        (self.dir / "provenance.intoto.jsonl").unlink()
        problems = self.verify()
        self.assertTrue(
            any("no build-provenance attestation found" in p for p in problems),
            problems,
        )

    # -- Nothing to verify ------------------------------------------------

    def test_empty_directory_is_rejected(self):
        empty = Path(tempfile.mkdtemp())
        try:
            problems = verifier.verify(empty, SHA)
            self.assertTrue(any("no hotplex-* release artifacts" in p for p in problems), problems)
        finally:
            shutil.rmtree(empty, ignore_errors=True)


if __name__ == "__main__":
    unittest.main()
