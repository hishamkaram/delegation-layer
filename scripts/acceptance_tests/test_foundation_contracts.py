"""Compatibility and import contracts for the shared foundation extraction."""

import ast
import hashlib
import importlib
import json
from pathlib import Path
import subprocess
import sys
import unittest

from scripts import acceptance_provider_common as provider
from scripts import acceptance_supervisor_common as supervisor
from scripts.delegate_acceptance.directories.operations import DirectoryOwner
from scripts.delegate_acceptance.processes.operations import Process, Processes
from scripts.delegate_acceptance.tasks.operations import NativeTaskOps


class FoundationContractTests(unittest.TestCase):
    def setUp(self):
        fixture_path = Path(__file__).with_name("shared_foundation_contract.json")
        self.contract = json.loads(fixture_path.read_text(encoding="utf-8"))

    def defining_node(self, value):
        module = importlib.import_module(value.__module__)
        tree = ast.parse(Path(module.__file__).read_text(encoding="utf-8"))
        return next(node for node in tree.body
                    if isinstance(node, (ast.FunctionDef, ast.ClassDef))
                    and node.name == value.__name__)

    def test_every_baseline_export_and_callable_signature_is_preserved(self):
        for module_name, contract in self.contract["modules"].items():
            module = importlib.import_module("scripts." + module_name)
            for name in contract["public_exports"]:
                with self.subTest(module=module_name, export=name):
                    self.assertTrue(hasattr(module, name))
            for name, expected in contract["functions"].items():
                with self.subTest(module=module_name, function=name):
                    node = self.defining_node(getattr(module, name))
                    self.assertEqual(ast.dump(node.args), expected)
            for name, methods in contract["classes"].items():
                node = self.defining_node(getattr(module, name))
                actual = {method.name: ast.dump(method.args) for method in node.body
                          if isinstance(method, ast.FunctionDef)}
                with self.subTest(module=module_name, owner=name):
                    self.assertEqual(actual, methods)

    def test_frozen_registry_methods_keep_their_baseline_statements(self):
        for module_name, contract in self.contract["modules"].items():
            module = importlib.import_module("scripts." + module_name)
            for name, methods in contract["frozen_methods"].items():
                node = self.defining_node(getattr(module, name))
                actual = {method.name: method for method in node.body
                          if isinstance(method, ast.FunctionDef)}
                for method, expected in methods.items():
                    with self.subTest(owner=name, method=method):
                        digest = hashlib.sha256(ast.dump(actual[method]).encode()).hexdigest()
                        self.assertEqual(digest, expected)

    def test_facades_alias_the_same_canonical_state_owners(self):
        self.assertIs(supervisor.Process, Process)
        self.assertIs(supervisor.Processes, Processes)
        self.assertIs(supervisor.DirectoryOwner, DirectoryOwner)
        self.assertIs(provider.NativeTaskOps, NativeTaskOps)

    def test_importing_every_foundation_module_starts_no_fixture(self):
        program = """
import importlib
import os
import pkgutil
from pathlib import Path
import sys

def forbid_effects(event, arguments):
    forbidden = {
        'subprocess.Popen', 'os.system', 'os.fork', 'os.posix_spawn',
        'os.mkdir', 'os.remove', 'os.rmdir', 'os.rename', 'socket.connect',
    }
    if event in forbidden:
        raise RuntimeError('import-time effect: ' + event)
    if event == 'open' and arguments[2] & (os.O_WRONLY | os.O_RDWR | os.O_CREAT):
        raise RuntimeError('import-time write')

sys.addaudithook(forbid_effects)
sys.path.insert(0, str(Path(sys.argv[1]).parent))
import scripts.delegate_acceptance as foundation
for module in pkgutil.walk_packages(foundation.__path__, 'scripts.delegate_acceptance.'):
    importlib.import_module(module.name)
import scripts.acceptance_supervisor_common
import scripts.acceptance_provider_common
"""
        scripts = str(Path(__file__).resolve().parents[1])
        result = subprocess.run(
            [sys.executable, "-I", "-B", "-c", program, scripts],
            capture_output=True, text=True, timeout=20, check=False)
        self.assertEqual(result.returncode, 0, result.stderr)


if __name__ == "__main__":
    unittest.main()
