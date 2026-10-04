import importlib.util
import io
import json
import shutil
import sys
import tarfile
import tempfile
import unittest
import zipfile
from pathlib import Path
from unittest import mock


REPO_ROOT = Path(__file__).resolve().parents[2]
MODULE_PATH = Path(__file__).with_name("generate_sbom.py")


def load_generator():
    spec = importlib.util.spec_from_file_location("generate_sbom", MODULE_PATH)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"cannot load generator module from {MODULE_PATH}")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


generator = load_generator()


def component(name: str, purl: str) -> dict:
    return {"name": name, "purl": purl}


def syft_stub(payloads: dict[str, list[dict]]):
    """A stand-in for syft: returns the components registered per filename."""

    def fake_run(argv, **kwargs):
        target = argv[2]
        name = Path(target).name
        document = {
            "bomFormat": "CycloneDX",
            "components": payloads.get(name, []),
        }
        return mock.Mock(returncode=0, stdout=json.dumps(document), stderr="")

    return fake_run


def make_tarball(path: Path, members: dict[str, bytes]) -> None:
    with tarfile.open(path, "w:gz") as handle:
        for name, body in members.items():
            info = tarfile.TarInfo(name)
            info.size = len(body)
            handle.addfile(info, io.BytesIO(body))


class GenerateSbomTest(unittest.TestCase):
    """The point of these cases is the failure this script exists to prevent:
    a syntactically valid CycloneDX document that lists no components, which
    is what a plain `syft scan <offline bundle>` produces."""

    def setUp(self):
        self.dir = Path(tempfile.mkdtemp())

    def tearDown(self):
        shutil.rmtree(self.dir, ignore_errors=True)

    def generate(self, archive: Path, payloads: dict[str, list[dict]]) -> dict:
        output = self.dir / (archive.name + ".sbom.json")
        argv = [
            "generate_sbom.py",
            "--archive",
            str(archive),
            "--output",
            str(output),
            "--syft",
            "syft",
        ]
        with mock.patch.object(sys, "argv", argv), mock.patch.object(
            generator.shutil, "which", returnvalue="/usr/bin/syft"
        ), mock.patch.object(generator.subprocess, "run", side_effect=syft_stub(payloads)):
            code = generator.main()
        self.assertEqual(0, code)
        return json.loads(output.read_text(encoding="utf-8"))

    def test_nested_archive_members_are_described(self):
        bundle = self.dir / "hotplex-offline-bundle-darwin-arm64.tar.gz"
        make_tarball(
            bundle,
            {"opencode.zip": b"PK\x03\x04", "oh-my-opencode-5.1.13.tgz": b"\x1f\x8b"},
        )
        document = self.generate(
            bundle,
            {"oh-my-opencode-5.1.13.tgz": [component("oh-my-opencode", "pkg:npm/x@5.1.13")]},
        )
        self.assertEqual("CycloneDX", document["bomFormat"])
        names = [c["name"] for c in document["components"]]
        self.assertIn("oh-my-opencode", names)

    def test_document_names_the_artifact_it_describes(self):
        bundle = self.dir / "hotplex-offline-bundle-linux-x64.tar.gz"
        make_tarball(bundle, {"opencode.tar.gz": b"\x1f\x8b"})
        document = self.generate(
            bundle, {"opencode.tar.gz": [component("opencode", "pkg:generic/opencode@1")]}
        )
        self.assertEqual(bundle.name, document["metadata"]["component"]["name"])

    def test_zip_bundles_are_supported(self):
        bundle = self.dir / "hotplex-offline-bundle-windows-x64.zip"
        with zipfile.ZipFile(bundle, "w") as handle:
            handle.writestr("opencode.zip", b"PK\x03\x04")
        document = self.generate(
            bundle, {"opencode.zip": [component("opencode", "pkg:generic/opencode@1")]}
        )
        self.assertEqual(1, len(document["components"]))

    def test_binary_archives_are_described(self):
        archive = self.dir / "hotplex-linux-amd64.tar.gz"
        make_tarball(archive, {"hotplex": b"\x7fELF", "hotplex.provenance": b"source_sha=x\n"})
        document = self.generate(
            archive,
            {"hotplex": [component("github.com/spf13/cobra", "pkg:golang/spf13/cobra@v1.9.1")]},
        )
        self.assertEqual(1, len(document["components"]))

    def test_duplicate_components_are_collapsed(self):
        archive = self.dir / "hotplex-linux-amd64.tar.gz"
        make_tarball(archive, {"a.bin": b"1", "b.bin": b"2"})
        shared = component("shared", "pkg:golang/shared@v1")
        document = self.generate(archive, {"a.bin": [shared], "b.bin": [shared]})
        self.assertEqual(1, len(document["components"]))

    def test_macos_appledouble_entries_are_ignored(self):
        archive = self.dir / "hotplex-darwin-arm64.tar.gz"
        make_tarball(archive, {"hotplex": b"\x7fELF", "._hotplex": b"\x00\x05\x16\x07"})
        # The AppleDouble entry is given its own component so that dropping
        # the filter shows up as an extra entry rather than as no change.
        document = self.generate(
            archive,
            {
                "hotplex": [component("cobra", "pkg:golang/spf13/cobra@v1.9.1")],
                "._hotplex": [component("appledouble", "pkg:generic/appledouble@1")],
            },
        )
        self.assertEqual(1, len(document["components"]))

    def test_an_unreadable_member_does_not_sink_the_release(self):
        # A compiled runtime with no embedded dependency metadata contributes
        # nothing; it must not become a publish failure.
        archive = self.dir / "hotplex-offline-bundle-linux-x64.tar.gz"
        make_tarball(archive, {"opencode.zip": b"PK\x03\x04"})

        def fake_run(argv, **kwargs):
            return mock.Mock(returncode=1, stdout="", stderr="unsupported")

        output = self.dir / "out.json"
        argv = [
            "generate_sbom.py",
            "--archive",
            str(archive),
            "--output",
            str(output),
            "--syft",
            "syft",
        ]
        with mock.patch.object(sys, "argv", argv), mock.patch.object(
            generator.shutil, "which", returnvalue="/usr/bin/syft"
        ), mock.patch.object(generator.subprocess, "run", side_effect=fake_run):
            self.assertEqual(0, generator.main())
        document = json.loads(output.read_text(encoding="utf-8"))
        self.assertEqual([], document["components"])

    def test_a_path_traversing_member_is_refused(self):
        archive = self.dir / "evil.tar.gz"
        with tarfile.open(archive, "w:gz") as handle:
            info = tarfile.TarInfo("../escaped")
            info.size = 0
            handle.addfile(info, io.BytesIO(b""))
        argv = [
            "generate_sbom.py",
            "--archive",
            str(archive),
            "--output",
            str(self.dir / "out.json"),
            "--syft",
            "syft",
        ]
        with mock.patch.object(sys, "argv", argv), mock.patch.object(
            generator.shutil, "which", returnvalue="/usr/bin/syft"
        ):
            with self.assertRaises(SystemExit):
                generator.main()
        self.assertFalse((self.dir.parent / "escaped").exists())

    def test_a_missing_archive_fails_loudly(self):
        argv = [
            "generate_sbom.py",
            "--archive",
            str(self.dir / "absent.tar.gz"),
            "--output",
            str(self.dir / "out.json"),
            "--syft",
            "syft",
        ]
        with mock.patch.object(sys, "argv", argv), mock.patch.object(
            generator.shutil, "which", returnvalue="/usr/bin/syft"
        ):
            self.assertEqual(1, generator.main())


if __name__ == "__main__":
    unittest.main()
