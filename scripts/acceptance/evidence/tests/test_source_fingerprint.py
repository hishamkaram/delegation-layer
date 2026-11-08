"""Proof fingerprints follow static imports and do not execute inspected code."""

from pathlib import Path
import tempfile
import unittest

from scripts.acceptance.evidence import source as source_evidence
from scripts.acceptance.evidence import source as source
from scripts.acceptance.testing.record_fixtures import array_at
from scripts.acceptance.testing.record_fixtures import object_at



class SourceFingerprintTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="proof-source-")
        self.addCleanup(self.temporary.cleanup)
        temporary = Path(self.temporary.name)
        physical = temporary / "source"
        physical.mkdir()
        alias = temporary / "source-alias"
        alias.symlink_to(physical, target_is_directory=True)
        self.root = alias.resolve(strict=True)
        self.write(source_evidence.PARSER_SOURCE, "raise RuntimeError('never execute the inspected parser')\n")

    def write(self, name: str, value: str):
        path = self.root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(value, encoding="utf-8")

    def test_parser_seed_names_the_defining_pure_module(self):
        self.assertEqual(source.parse_source_facts.__module__, "scripts.maintainability.python_ast")
        self.assertEqual(source_evidence.PARSER_SOURCE,
                         source.parse_source_facts.__module__.replace(".", "/") + ".py")

    def test_parser_retains_static_imports_without_evaluating_source(self):
        code = ("from . import values\n"
                "import scripts.fixture.values\n"
                "from scripts.fixture import values as renamed\n"
                "from datetime import datetime\n"
                "raise RuntimeError('never evaluate')\n")
        facts = source.parse_source_facts("scripts/fixture/__init__.py", code,
                                          {"fixture", "fixture.values"})
        self.assertEqual(facts, {
            "path": "scripts/fixture/__init__.py", "module": "py:fixture", "imports": [
                {"target": "py:fixture.values", "line": 1},
                {"target": "py:fixture.values", "line": 2},
                {"target": "py:fixture.values", "line": 3},
                {"target": "py:datetime", "line": 4},
            ],
        })

    def test_parser_definition_and_transitive_dependency_changes_are_bound(self):
        parser = "scripts/maintainability/python_ast.py"
        helper = "scripts/maintainability/parser_helpers.py"
        values = "scripts/maintainability/parser_values.py"
        parser_source = "from .parser_helpers import fields\nraise RuntimeError('never execute')\n"
        self.write("scripts/entry.py", "import json\n")
        self.write(parser, parser_source)
        self.write(helper, "from .parser_values import fields\n")
        self.write(values, "fields = ('imports',)\n")
        original = source_evidence.fingerprint_sources(self.root, ("scripts/entry.py",))
        self.assertEqual({object_at(row)["path"] for row in array_at(original, "files")}, {
            "scripts/entry.py", parser, helper, values,
        })
        self.write(parser, parser_source + "# changed parser bytes\n")
        parser_changed = source_evidence.fingerprint_sources(self.root, ("scripts/entry.py",))
        self.assertNotEqual(original["sha256"], parser_changed["sha256"])
        self.write(values, "fields = ('imports', 'functions')\n")
        dependency_changed = source_evidence.fingerprint_sources(self.root, ("scripts/entry.py",))
        self.assertNotEqual(parser_changed["sha256"], dependency_changed["sha256"])
        self.write(values, "def incomplete(\n")
        with self.assertRaisesRegex(source.VerificationError, "source_file_invalid"):
            source_evidence.fingerprint_sources(self.root, ("scripts/entry.py",))

    def test_transitive_imports_parent_initializers_and_exact_bytes_are_bound(self):
        self.write("scripts/entry.py", "from scripts.fixture import middle\n")
        self.write("scripts/fixture/__init__.py", "raise RuntimeError('never execute')\n")
        self.write("scripts/fixture/middle.py", "from .values import answer\n")
        self.write("scripts/fixture/values.py", "answer = 42\n")
        first = source_evidence.fingerprint_sources(self.root, ("scripts/entry.py",))
        self.assertEqual({object_at(row)["path"] for row in array_at(first, "files")}, {
            "scripts/entry.py", "scripts/fixture/__init__.py", "scripts/fixture/middle.py",
            "scripts/fixture/values.py", source_evidence.PARSER_SOURCE,
        })
        self.write("scripts/fixture/values.py", "answer = 43\n")
        second = source_evidence.fingerprint_sources(self.root, ("scripts/entry.py",))
        self.assertNotEqual(first["sha256"], second["sha256"])

    def test_unreferenced_python_source_does_not_enter_the_claim(self):
        self.write("scripts/entry.py", "import json\n")
        first = source_evidence.fingerprint_sources(self.root, ("scripts/entry.py",))
        self.write("scripts/unrelated.py", "raise RuntimeError('not part of this gate')\n")
        second = source_evidence.fingerprint_sources(self.root, ("scripts/entry.py",))
        self.assertEqual(first, second)

    def test_static_import_cycle_is_finite(self):
        self.write("scripts/left.py", "import right\n")
        self.write("scripts/right.py", "import left\n")
        result = source_evidence.fingerprint_sources(self.root, ("scripts/left.py",))
        self.assertEqual(len(array_at(result, "files")), 3)

    def test_module_and_package_candidates_are_both_bound_without_execution(self):
        self.write("scripts/entry.py", "import candidate\n")
        self.write("scripts/candidate.py", "raise RuntimeError('never execute module')\n")
        self.write("scripts/candidate/__init__.py", "raise RuntimeError('never execute package')\n")
        result = source_evidence.fingerprint_sources(self.root, ("scripts/entry.py",))
        self.assertEqual(len(array_at(result, "files")), 4)

    def test_malformed_transitive_source_is_not_accepted(self):
        self.write("scripts/entry.py", "import broken\n")
        self.write("scripts/broken.py", "def incomplete(\n")
        with self.assertRaisesRegex(source.VerificationError, "source_file_invalid"):
            source_evidence.fingerprint_sources(self.root, ("scripts/entry.py",))


if __name__ == "__main__":
    unittest.main()
