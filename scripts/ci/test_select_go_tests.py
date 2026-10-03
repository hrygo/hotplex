import importlib.util
import io
import json
import sys
import tempfile
import unittest
from pathlib import Path


MODULE_PATH = Path(__file__).with_name("select_go_tests.py")


def load_selector():
    spec = importlib.util.spec_from_file_location("select_go_tests", MODULE_PATH)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"cannot load selector module from {MODULE_PATH}")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


class PackageSelectionTest(unittest.TestCase):
    def setUp(self):
        self.selector = load_selector()
        self.packages = [
            {
                "ImportPath": "example.com/hotplex/internal/events",
                "Dir": "/repo/internal/events",
                "Imports": [],
                "TestImports": [],
                "XTestImports": [],
            },
            {
                "ImportPath": "example.com/hotplex/internal/gateway",
                "Dir": "/repo/internal/gateway",
                "Imports": ["example.com/hotplex/internal/events"],
                "TestImports": ["example.com/hotplex/internal/testsupport"],
                "XTestImports": ["example.com/hotplex/internal/xtestsupport"],
            },
            {
                "ImportPath": "example.com/hotplex/internal/testsupport",
                "Dir": "/repo/internal/testsupport",
                "Imports": ["example.com/hotplex/internal/events"],
                "TestImports": [],
                "XTestImports": [],
            },
            {
                "ImportPath": "example.com/hotplex/internal/xtestsupport",
                "Dir": "/repo/internal/xtestsupport",
                "Imports": [],
                "TestImports": [],
                "XTestImports": [],
            },
            {
                "ImportPath": "example.com/hotplex/cmd/hotplex",
                "Dir": "/repo/cmd/hotplex",
                "Imports": ["example.com/hotplex/internal/gateway"],
                "TestImports": [],
                "XTestImports": [],
            },
            {
                "ImportPath": "example.com/hotplex/cmd/hotplex-standalone",
                "Dir": "/repo/cmd/hotplex-standalone",
                "Imports": [],
                "TestImports": [],
                "XTestImports": [],
            },
            {
                "ImportPath": "example.com/hotplex/internal/e2econtract",
                "Dir": "/repo/internal/e2econtract",
                "Imports": ["example.com/hotplex/internal/events"],
                "TestImports": [],
                "XTestImports": [],
            },
        ]

    def select(self, changed):
        return self.selector.select_packages(
            changed_paths=changed,
            packages=self.packages,
            repo_root=Path("/repo"),
            excluded_patterns=self.selector.DEFAULT_EXCLUDED_PATTERNS,
        )

    def test_changed_package_selects_itself_and_reverse_dependency_closure(self):
        result = self.select(["internal/events/events.go"])

        self.assertEqual(result.mode, "scoped")
        self.assertEqual(
            result.test_packages,
            [
                "example.com/hotplex/internal/events",
                "example.com/hotplex/internal/gateway",
                "example.com/hotplex/internal/testsupport",
            ],
        )
        self.assertEqual(
            result.excluded_test_packages,
            [
                "example.com/hotplex/cmd/hotplex",
                "example.com/hotplex/internal/e2econtract",
            ],
        )

    def test_internal_test_dependency_is_part_of_the_runtime_graph(self):
        result = self.select(["internal/gateway/gateway.go"])

        self.assertEqual(
            result.test_packages,
            ["example.com/hotplex/internal/gateway"],
        )
        self.assertEqual(result.excluded_test_packages, ["example.com/hotplex/cmd/hotplex"])

    def test_only_excluded_packages_produce_an_empty_coverage_scope(self):
        result = self.select(["cmd/hotplex-standalone/routes.go"])

        self.assertEqual(result.mode, "empty")
        self.assertEqual(result.test_packages, [])
        self.assertEqual(result.excluded_test_packages, ["example.com/hotplex/cmd/hotplex-standalone"])

    def test_xtest_dependency_expands_to_the_external_test_package_and_its_upstream(self):
        result = self.select(["internal/xtestsupport/helper.go"])

        self.assertEqual(result.mode, "scoped")
        self.assertEqual(
            result.test_packages,
            [
                "example.com/hotplex/internal/gateway",
                "example.com/hotplex/internal/xtestsupport",
            ],
        )
        self.assertEqual(result.excluded_test_packages, ["example.com/hotplex/cmd/hotplex"])

    def test_root_directory_maps_to_module_path(self):
        packages = [
            {
                "ImportPath": "example.com/hotplex",
                "Dir": "/repo",
                "Imports": [],
                "TestImports": [],
                "XTestImports": [],
            }
        ]

        result = self.selector.select_packages(
            changed_paths=["main.go"],
            packages=packages,
            repo_root=Path("/repo"),
            excluded_patterns=self.selector.DEFAULT_EXCLUDED_PATTERNS,
        )

        self.assertEqual(result.test_packages, ["example.com/hotplex"])
        self.assertEqual(result.mode, "scoped")

    def test_deleted_or_unmapped_go_path_falls_back_to_full_selection(self):
        result = self.select(["internal/removed/removed.go"])

        self.assertEqual(result.mode, "full")
        self.assertEqual(result.fallback_reason, "changed Go path maps to no listed package")
        self.assertEqual(
            result.test_packages,
            [
                "example.com/hotplex/internal/events",
                "example.com/hotplex/internal/gateway",
                "example.com/hotplex/internal/testsupport",
                "example.com/hotplex/internal/xtestsupport",
            ],
        )
        self.assertEqual(
            result.excluded_test_packages,
            [
                "example.com/hotplex/cmd/hotplex",
                "example.com/hotplex/cmd/hotplex-standalone",
                "example.com/hotplex/internal/e2econtract",
            ],
        )

    def test_dependency_manifest_change_falls_back_to_full_selection(self):
        result = self.select(["go.mod"])

        self.assertEqual(result.mode, "full")
        self.assertEqual(result.fallback_reason, "dependency manifest changed")

    def test_selector_or_protocol_contract_change_falls_back_to_full_selection(self):
        for path in (
            ".github/workflows/ci.yml",
            "scripts/ci/select_go_tests.py",
            "pkg/aep/schema/aep-v1.json",
            "scripts/test-contract-matrix.sh",
        ):
            with self.subTest(path=path):
                result = self.select([path])
                self.assertEqual(result.mode, "full")
                self.assertTrue(result.fallback_reason)

    def test_unrelated_non_go_change_does_not_expand_go_scope(self):
        result = self.select(["webchat/app/admin/page.tsx", "docs/index.md"])

        self.assertEqual(result.mode, "empty")
        self.assertEqual(result.test_packages, [])
        self.assertEqual(result.excluded_test_packages, [])

    def test_excluded_packages_are_validated_in_full_mode(self):
        result = self.select(["go.sum"])

        self.assertEqual(result.mode, "full")
        self.assertEqual(
            result.excluded_test_packages,
            [
                "example.com/hotplex/cmd/hotplex",
                "example.com/hotplex/cmd/hotplex-standalone",
                "example.com/hotplex/internal/e2econtract",
            ],
        )

    def test_nul_stdin_parser_preserves_spaces_in_paths(self):
        raw = b"docs/a file.md\0internal/events/events.go\0"

        self.assertEqual(
            self.selector.read_nul_paths(io.BytesIO(raw)),
            ["docs/a file.md", "internal/events/events.go"],
        )

    def test_json_output_is_machine_readable_and_sorted(self):
        result = self.select(["internal/events/events.go"])

        encoded = json.loads(self.selector.encode_result(result))

        self.assertEqual(encoded["mode"], "scoped")
        self.assertEqual(encoded["test_packages"], result.test_packages)
        self.assertEqual(
            encoded["excluded_test_packages"],
            result.excluded_test_packages,
        )

    def test_github_output_writes_one_value_per_required_key(self):
        result = self.select(["internal/events/events.go"])

        with tempfile.TemporaryDirectory() as tmp:
            output_path = Path(tmp) / "github-output"
            self.selector.write_github_output(result, output_path)
            content = output_path.read_text()

        self.assertIn("mode=scoped\n", content)
        self.assertIn("fallback_reason=\n", content)
        self.assertIn(
            "test_packages<<SELECT_GO_TESTS_EOF\n"
            "example.com/hotplex/internal/events\n"
            "example.com/hotplex/internal/gateway\n"
            "example.com/hotplex/internal/testsupport\n"
            "SELECT_GO_TESTS_EOF\n",
            content,
        )
        self.assertIn(
            "excluded_test_packages<<SELECT_GO_TESTS_EOF\n"
            "example.com/hotplex/cmd/hotplex\n"
            "example.com/hotplex/internal/e2econtract\n"
            "SELECT_GO_TESTS_EOF\n",
            content,
        )

    def test_full_flag_is_available_for_push_to_main(self):
        args = self.selector.parse_args(["--full"])

        self.assertTrue(args.full)


if __name__ == "__main__":
    unittest.main()
