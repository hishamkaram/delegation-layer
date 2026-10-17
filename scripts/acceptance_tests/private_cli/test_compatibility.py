"""Public private-CLI contracts and single-collection test relocation facts."""

import ast
from collections import Counter
import importlib
import json
from pathlib import Path
import unittest

from scripts import acceptance_private_cli as private_cli


HERE = Path(__file__).resolve().parent
SCRIPTS = HERE.parents[1]


def defining_node(value):
    module = importlib.import_module(value.__module__)
    tree = ast.parse(Path(module.__file__).read_text(encoding="utf-8"))
    return next(node for node in tree.body
                if isinstance(node, (ast.FunctionDef, ast.ClassDef))
                and node.name == value.__name__)


class PrivateCompatibilityTests(unittest.TestCase):
    def test_original_function_and_class_signatures_are_preserved(self):
        contract = json.loads((HERE / "baseline_contract.json").read_text())
        for name, expected in contract["functions"].items():
            with self.subTest(function=name):
                self.assertEqual(ast.dump(defining_node(getattr(private_cli, name)).args), expected)
        for name, expected in contract["classes"].items():
            node = defining_node(getattr(private_cli, name))
            expected = dict(expected)
            if name == "LiveFIFOLease":
                # Only this private helper changed; public lease APIs keep the baseline.
                signature = ast.parse(
                    "def _acquire_with_parent(cls, *, task_id, pid, path, nonce, "
                    "parent_descriptor, parent_identity, timeout, owns_parent_descriptor): pass")
                expected["_acquire_with_parent"] = ast.dump(signature.body[0].args)
            actual = {method.name: ast.dump(method.args) for method in node.body
                      if isinstance(method, ast.FunctionDef)}
            with self.subTest(owner=name):
                self.assertEqual(actual, expected)

    def test_baseline_private_ids_have_an_explicit_one_to_one_mapping(self):
        baseline = json.loads((HERE / "baseline_contract.json").read_text())["test_ids"]
        rows = [json.loads(line) for line in (HERE / "test_id_mapping.jsonl").read_text().splitlines()]
        self.assertEqual(Counter(row["baseline"] for row in rows), Counter(baseline))
        self.assertEqual(len(rows), len(set(row["mandatory_id"] for row in rows)))
        self.assertEqual(len(rows), len(set(row["all_scripts_id"] for row in rows)))

    def test_recorded_ids_resolve_in_make_discovery_namespace(self):
        rows = [json.loads(line) for line in (HERE / "test_id_mapping.jsonl").read_text().splitlines()]
        for row in rows:
            for target in {row["mandatory_id"], row["all_scripts_id"]}:
                with self.subTest(test_id=target):
                    loader = unittest.TestLoader()
                    suite = loader.loadTestsFromName(target)
                    self.assertEqual(loader.errors, [])
                    self.assertEqual(suite.countTestCases(), 1)

    def test_old_dispatch_facade_does_not_expose_moved_test_cases(self):
        facade = ast.parse((SCRIPTS / "test_acceptance_private_dispatch.py").read_text())
        self.assertTrue(all(isinstance(node, ast.Expr) and isinstance(node.value, ast.Constant)
                            and isinstance(node.value.value, str) for node in facade.body))
        rows = [json.loads(line) for line in (HERE / "test_id_mapping.jsonl").read_text().splitlines()]
        moved = [row for row in rows if row["baseline"].startswith("test_acceptance_private_dispatch.")]
        self.assertEqual(len(moved), 86)
        for row in moved:
            module, class_name, method = row["mandatory_id"].rsplit(".", 2)
            source = SCRIPTS / (module.replace(".", "/") + ".py")
            tree = ast.parse(source.read_text())
            definitions = [node for node in tree.body if isinstance(node, ast.ClassDef)
                           and node.name == class_name]
            with self.subTest(test_id=row["baseline"]):
                self.assertEqual(len(definitions), 1)
                self.assertEqual(sum(isinstance(node, ast.FunctionDef) and node.name == method
                                     for node in definitions[0].body), 1)
