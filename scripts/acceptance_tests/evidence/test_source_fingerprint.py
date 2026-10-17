"""Proof fingerprints follow static imports and do not execute inspected code."""

from pathlib import Path
import tempfile
import unittest

from scripts.delegate_acceptance.release_verification import source


class SourceFingerprintTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="proof-source-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.write(source.PARSER_SOURCE, "raise RuntimeError('never execute the inspected parser')\n")

    def write(self, name: str, value: str):
        path = self.root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(value, encoding="utf-8")

    def test_parser_seed_names_the_defining_pure_module(self):
        self.assertEqual(source.parse_source_facts.__module__, "scripts.maintainability.python_ast")
        self.assertEqual(source.PARSER_SOURCE,
                         source.parse_source_facts.__module__.replace(".", "/") + ".py")

    def test_parser_definition_and_transitive_dependency_changes_are_bound(self):
        parser = "scripts/maintainability/python_ast.py"
        helper = "scripts/maintainability/parser_helpers.py"
        values = "scripts/maintainability/parser_values.py"
        parser_source = "from .parser_helpers import fields\nraise RuntimeError('never execute')\n"
        self.write("scripts/entry.py", "import json\n")
        self.write(parser, parser_source)
        self.write(helper, "from .parser_values import fields\n")
        self.write(values, "fields = ('imports',)\n")
        original = source.fingerprint_sources(self.root, ("scripts/entry.py",))
        self.assertEqual({row["path"] for row in original["files"]}, {
            "scripts/entry.py", parser, helper, values,
        })
        self.write(parser, parser_source + "# changed parser bytes\n")
        parser_changed = source.fingerprint_sources(self.root, ("scripts/entry.py",))
        self.assertNotEqual(original["sha256"], parser_changed["sha256"])
        self.write(values, "fields = ('imports', 'functions')\n")
        dependency_changed = source.fingerprint_sources(self.root, ("scripts/entry.py",))
        self.assertNotEqual(parser_changed["sha256"], dependency_changed["sha256"])
        self.write(values, "def incomplete(\n")
        with self.assertRaisesRegex(source.VerificationError, "source_file_invalid"):
            source.fingerprint_sources(self.root, ("scripts/entry.py",))

    def test_transitive_imports_parent_initializers_and_exact_bytes_are_bound(self):
        self.write("scripts/entry.py", "from scripts.fixture import middle\n")
        self.write("scripts/fixture/__init__.py", "raise RuntimeError('never execute')\n")
        self.write("scripts/fixture/middle.py", "from .values import answer\n")
        self.write("scripts/fixture/values.py", "answer = 42\n")
        first = source.fingerprint_sources(self.root, ("scripts/entry.py",))
        self.assertEqual({row["path"] for row in first["files"]}, {
            "scripts/entry.py", "scripts/fixture/__init__.py", "scripts/fixture/middle.py",
            "scripts/fixture/values.py", source.PARSER_SOURCE,
        })
        self.write("scripts/fixture/values.py", "answer = 43\n")
        second = source.fingerprint_sources(self.root, ("scripts/entry.py",))
        self.assertNotEqual(first["sha256"], second["sha256"])

    def test_unreferenced_python_source_does_not_enter_the_claim(self):
        self.write("scripts/entry.py", "import json\n")
        first = source.fingerprint_sources(self.root, ("scripts/entry.py",))
        self.write("scripts/unrelated.py", "raise RuntimeError('not part of this gate')\n")
        second = source.fingerprint_sources(self.root, ("scripts/entry.py",))
        self.assertEqual(first, second)

    def test_static_import_cycle_is_finite(self):
        self.write("scripts/left.py", "import right\n")
        self.write("scripts/right.py", "import left\n")
        result = source.fingerprint_sources(self.root, ("scripts/left.py",))
        self.assertEqual(len(result["files"]), 3)

    def test_module_and_package_candidates_are_both_bound_without_execution(self):
        self.write("scripts/entry.py", "import candidate\n")
        self.write("scripts/candidate.py", "raise RuntimeError('never execute module')\n")
        self.write("scripts/candidate/__init__.py", "raise RuntimeError('never execute package')\n")
        result = source.fingerprint_sources(self.root, ("scripts/entry.py",))
        self.assertEqual(len(result["files"]), 4)

    def test_malformed_transitive_source_is_not_accepted(self):
        self.write("scripts/entry.py", "import broken\n")
        self.write("scripts/broken.py", "def incomplete(\n")
        with self.assertRaisesRegex(source.VerificationError, "source_file_invalid"):
            source.fingerprint_sources(self.root, ("scripts/entry.py",))


if __name__ == "__main__":
    unittest.main()
